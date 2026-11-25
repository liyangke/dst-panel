package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 缓存条目的来源。界面上要能说清"这条是从哪知道的"，因为两者的信息量差很多。
const (
	// SeenFromWorkshop 是刚从 Steam 接口拿回来的，字段最全。
	SeenFromWorkshop = "workshop"
	// SeenFromDisk 是扫本地模组目录得到的，只有名字和版本，没有订阅数。
	SeenFromDisk = "disk"
)

// SeenMod 是一条工坊模组元数据缓存。
//
// 注意它和 steam.Item 是两个东西：Item 是"接口这一次返回了什么"，
// SeenMod 是"面板目前确定知道的一些事实"，后者可能来自三次不同来源的合并。
type SeenMod struct {
	WorkshopID  string
	Name        string
	Description string
	Version     string
	// WorkshopVersion 是"工坊那边写的版本"，只有在线来源会填。
	// 它和 Version 必须分开：Version 会被磁盘扫描刷成这台机器上那份的版本，
	// 两个事实挤一列的话"工坊更新了没"这个问题在任何一次扫盘之后都问不出来。
	// 这一列只读不写——写的时候填 Version 就够，recordSeen 按 origin 决定它落哪儿。
	WorkshopVersion string
	Tags            []string
	Subscriptions   int64
	// ModUpdatedAt 是模组作者在工坊的最后更新时间（Unix 秒），0 表示不知道。
	ModUpdatedAt int64
	FileSize     int64
	PreviewURL   string
	HasChildren  bool
	// Favorited 是收藏数。它和订阅数是两个数（实测 Epic Healthbar：订阅 372 万、
	// 收藏 11.8 万），工坊卡片上那颗星星对应的就是它，所以两个都存。
	Favorited int64
	// VoteScore 是工坊好评率（0~1），VotesTotal 是投过票的人数。
	// 三个都到位才算"这次真读到了评分"，所以界面按 votesTotal>0 决定显不显示星星。
	VoteScore  float64
	VotesTotal int64
	// ClientRequired 来自 all_clients_require_mod 标签：为真时玩家也必须订阅，
	// 光在服务端装上没用，这是"装了没效果"最常见的解释。
	ClientRequired bool
	ServerOnly     bool
	Origin         string
	FirstSeenAt    time.Time
	LastSeenAt     time.Time
}

// RecordSeen 写入或合并若干条缓存。
//
// 合并规则是这个函数唯一需要讲清楚的地方（写成 SQL 的 CASE 是为了让它只走一次写）：
//
//	文本字段：这次非空才覆盖。接口偶尔只回 ID 不回 title，那种时候不能把已有的名字抹掉。
//	几个计数：取更大的那个。磁盘扫描的订阅数是 0，它不该把工坊来的真实数字清零。
//	好评率：这次真读到了才覆盖（votesTotal>0 才算读到）。它不跟"只增不减"，
//	  因为评分是一次读数而不是累计量——模组后来掉分是常事，留历史最高分就是在说谎。
//	三个布尔：见过就置真。宁可多提示一次客户端要订阅。
//	origin：只有 workshop 会覆盖 disk，反过来不行。
//	workshop_version：跟着 origin 走——这一趟是工坊来的才用它的 Version 覆盖，
//	  磁盘那一趟不许碰它（这正是它单开一列的全部理由）。
//	last_seen_at 每次刷新，first_seen_at 永不动。
func (s *Store) RecordSeen(mods ...SeenMod) error {
	for _, m := range mods {
		if err := s.recordSeen(m); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recordSeen(m SeenMod) error {
	id := strings.TrimSpace(m.WorkshopID)
	if id == "" {
		return errors.New("写缓存必须有工坊 ID")
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	rawTags, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("编码模组标签: %w", err)
	}
	origin := m.Origin
	if origin == "" {
		origin = SeenFromWorkshop
	}
	// 新增那一行走这里，已存在那一行走上面 SQL 的同名 CASE。
	var workshopVersion string
	if origin == SeenFromWorkshop {
		workshopVersion = m.Version
	}
	now := time.Now()
	if m.LastSeenAt.IsZero() {
		m.LastSeenAt = now
	}
	if m.FirstSeenAt.IsZero() || m.FirstSeenAt.After(m.LastSeenAt) {
		m.FirstSeenAt = m.LastSeenAt
	}
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	_, err = s.db.Exec(`
		INSERT INTO mods_seen (
			workshop_id, name, description, version, workshop_version, tags, subscriptions,
			mod_updated_at, file_size, preview_url, has_children,
			client_required, server_only, origin, first_seen_at, last_seen_at,
			favorited, vote_score, votes_total)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(workshop_id) DO UPDATE SET
			name            = CASE WHEN excluded.name <> ''            THEN excluded.name            ELSE mods_seen.name END,
			description     = CASE WHEN excluded.description <> ''     THEN excluded.description     ELSE mods_seen.description END,
			version         = CASE WHEN excluded.version <> ''         THEN excluded.version         ELSE mods_seen.version END,
			-- 这一列只认在线来源：磁盘那一趟的 version 是"这台机器上那份"的版本，
			-- 让它进来的话每次刷新模组页都会把工坊的数刷成磁盘的数，"工坊更新了"就永远读不出来。
			workshop_version = CASE WHEN excluded.origin = 'workshop' AND excluded.version <> ''
			                        THEN excluded.version ELSE mods_seen.workshop_version END,
			tags            = CASE WHEN excluded.tags <> '[]'          THEN excluded.tags            ELSE mods_seen.tags END,
			preview_url     = CASE WHEN excluded.preview_url <> ''     THEN excluded.preview_url     ELSE mods_seen.preview_url END,
			subscriptions   = MAX(mods_seen.subscriptions, excluded.subscriptions),
			mod_updated_at  = MAX(mods_seen.mod_updated_at, excluded.mod_updated_at),
			file_size       = MAX(mods_seen.file_size, excluded.file_size),
			favorited       = MAX(mods_seen.favorited, excluded.favorited),
			votes_total     = MAX(mods_seen.votes_total, excluded.votes_total),
			-- 好评率只在这次真读到评分时换：votes_total 为 0 说明这一趟来源里没有投票数据，
			-- 那时候保留上一次的分数才是对的，写成 0 会让卡片上一颗星都不亮。
			vote_score      = CASE WHEN excluded.votes_total > 0       THEN excluded.vote_score      ELSE mods_seen.vote_score END,
			has_children    = MAX(mods_seen.has_children, excluded.has_children),
			client_required = MAX(mods_seen.client_required, excluded.client_required),
			server_only     = MAX(mods_seen.server_only, excluded.server_only),
			origin          = CASE WHEN excluded.origin = 'workshop'   THEN 'workshop'               ELSE mods_seen.origin END,
			last_seen_at    = excluded.last_seen_at`,
		id, m.Name, m.Description, m.Version, workshopVersion, string(rawTags), m.Subscriptions,
		m.ModUpdatedAt, m.FileSize, m.PreviewURL, b(m.HasChildren),
		b(m.ClientRequired), b(m.ServerOnly), origin,
		m.FirstSeenAt.UnixMilli(), m.LastSeenAt.UnixMilli(),
		m.Favorited, m.VoteScore, m.VotesTotal)
	if err != nil {
		return fmt.Errorf("写工坊模组缓存 %s: %w", id, err)
	}
	return nil
}

const seenSelect = `SELECT workshop_id, name, description, version, workshop_version, tags,
 subscriptions, mod_updated_at, file_size, preview_url, has_children, client_required,
 server_only, origin, first_seen_at, last_seen_at, favorited, vote_score, votes_total FROM mods_seen`

// seenKeyword 只翻"名字/ID 对得上"这一半条件。
// 单开一个函数是因为分类计数（SeenFacets）要的是"同一批关键词的结果里各分类有几条"，
// 它必须带着关键词、又不带着已选分类，否则选中之后的计数会变成 0 或者 1 列。
func seenKeyword(q string) (string, []any) {
	if like := likePattern(q); like != "" {
		return ` WHERE (name LIKE ? ESCAPE '\' OR workshop_id = ?)`, []any{like, strings.TrimSpace(q)}
	}
	return "", nil
}

// seenWhere 把搜索词和分类翻成 WHERE。SearchSeenPage 与 CountSeen 必须共用它：
// 分页的"总共几条"和"这一页几条"若来自两套条件，界面就会出现"共 12 条，
// 但第 2 页还有内容"这种自相矛盾。
func seenWhere(q, tag string) (string, []any) {
	where, args := seenKeyword(q)
	if tag == "" {
		return where, args
	}
	join := " WHERE"
	if where != "" {
		join = " AND"
	}
	// tags 列存的是 JSON 数组文本，所以按"带引号的整个标签"匹配：
	// 不带引号的话搜 item 会把 item_manager 之类一起捞进来。
	// SQLite 的 LIKE 对 ASCII 字母本来就大小写不敏感，作者写成 Other / Art 也能命中，
	// 这是实测出来的（同一批 1200 条里 other 294 次、Other 52 次）。
	return where + join + ` tags LIKE ? ESCAPE '\'`, append(args, tagLike(tag))
}

// tagLike 把标签变成匹配 JSON 数组里那一个带引号条目的模式。
// 转义是防"以后有人在字典里加一个带下划线的标签"——下划线在 LIKE 里是单字符通配符，
// 不转义的话它会命中别的标签，那一列的分类数就凭空多几条，而没人能看出来。
func tagLike(tag string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return `%"` + r.Replace(tag) + `"%`
}

// SearchSeenPage 按名字模糊找缓存并翻页，tag 非空时再叠一层分类。q 为空时按热度列全部——
// 分页不是在线检索的专利：没配 key 的那套界面同样要能往后翻。
//
// LIKE 的三个字符必须转义：百分号和下划线是通配符，反斜杠是转义符本身。
// 用户在搜索框里打一个 % 就应该真的匹配一个 %。
func (s *Store) SearchSeenPage(q, tag string, limit, offset int) ([]SeenMod, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	where, args := seenWhere(q, tag)
	// workshop_id 是并列时的兜底序：只按订阅数排，翻页时同一条可能在两页都出现
	// 或者一页都不出现。
	query := seenSelect + where + ` ORDER BY subscriptions DESC, name ASC, workshop_id ASC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	return s.querySeen(query, args...)
}

// SearchSeen 是从头取一批的简写。
func (s *Store) SearchSeen(q string, limit int) ([]SeenMod, error) {
	return s.SearchSeenPage(q, "", limit, 0)
}

// GetSeen 取单条。没有这条时返回 ok=false，不是错误——"没听过"是正常结果。
func (s *Store) GetSeen(id string) (SeenMod, bool, error) {
	list, err := s.querySeen(seenSelect+` WHERE workshop_id = ?`, strings.TrimSpace(id))
	if err != nil {
		return SeenMod{}, false, err
	}
	if len(list) == 0 {
		return SeenMod{}, false, nil
	}
	return list[0], true, nil
}

// SeenByIDs 一次按 ID 批量取缓存，回 map 时以 workshop_id 为键。
//
// 为什么要批量而不是循环调 GetSeen：调用方是"这台机器上装着的模组"那一屏，
// 一条模组一次查询，几十条就是几十次往返；而这一屏要画的封面/订阅数必须来自同一次读取，
// 分几十次读的话中间只要有一次写库，卡片之间就对不上了。
// 没听过的 ID 在 map 里就没有键，调用方按"没有这一条"处理，不补一个零值糊上去。
func (s *Store) SeenByIDs(ids []string) (map[string]SeenMod, error) {
	uniq := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	out := map[string]SeenMod{}
	if len(uniq) == 0 {
		return out, nil
	}
	var sb strings.Builder
	sb.WriteString(seenSelect + ` WHERE workshop_id IN (`)
	args := make([]any, 0, len(uniq))
	for i, id := range uniq {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("?")
		args = append(args, id)
	}
	sb.WriteString(")")
	list, err := s.querySeen(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	for _, m := range list {
		out[m.WorkshopID] = m
	}
	return out, nil
}

// SeenCount 是缓存条数，降级提示里要用它说一句有用的话
// （"本地有 37 条，够搜"和"本地一条都没有，得配 key"是完全不同的建议）。
func (s *Store) SeenCount() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mods_seen`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计工坊模组缓存: %w", err)
	}
	return n, nil
}

// CountSeen 是"按这个搜索词和分类能翻到几条"，分页要的那个数只能来自它。
// 和 SearchSeenPage 同一条 WHERE，所以两者永远对得上。
func (s *Store) CountSeen(q, tag string) (int, error) {
	where, args := seenWhere(q, tag)
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mods_seen`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计工坊模组缓存（按搜索词）: %w", err)
	}
	return n, nil
}

// SeenFacets 数"对得上这个关键词的缓存里，每个分类各有一条多少条"。
//
// 为什么一次查询数完而不是每个分类跑一次：这十几个数是同一屏上那一排标签，
// 跑十几次就有先后，中间的写库会让某一列比别的高——而界面只会显示一个数，
// 没人的看得出它是对哪一刻的库数的。
// 一次 SELECT 十几列 SUM(CASE) 就够，参数仍然是绑定的（模式不是拼出来的）。
//
// counts 按传入 tags 的顺序回填，调用方（internal/api/modsearch.go）拿字典顺序决定标签行。
func (s *Store) SeenFacets(q string, tags []string) (map[string]int, error) {
	out := make(map[string]int, len(tags))
	if len(tags) == 0 {
		return out, nil
	}
	where, whereArgs := seenKeyword(q)
	// 参数顺序必须跟着 SQL 文本里的占位符顺序：这一条是 SELECT 里那几枚在前、WHERE 在后，
	// 所以先把分类模式放进去，最后才接搜索词。写反了不报错，只是每一列都数成 0
	// （第一版就是这么红的：带关键词那一趟全零，不带关键词的那趟看着正常）。
	var sb strings.Builder
	sb.WriteString(`SELECT `)
	args := make([]any, 0, len(tags)+len(whereArgs))
	for _, t := range tags {
		// COALESCE 是必需的而不是防呆：一条都不匹配时 SUM 返回的是 NULL，
		// 直接扫进 int 会报错——搜一个缓存里没有的关键词时整屏就坏了。
		sb.WriteString("COALESCE(SUM(CASE WHEN tags LIKE ? ESCAPE '\\' THEN 1 ELSE 0 END), 0), ")
		args = append(args, tagLike(t))
	}
	query := strings.TrimSuffix(sb.String(), ", ") + ` FROM mods_seen` + where
	args = append(args, whereArgs...)
	ptrs := make([]any, len(tags))
	vals := make([]int, len(tags))
	for i := range ptrs {
		ptrs[i] = &vals[i]
	}
	// 用 QueryRow 而不是 Query：这一条 SQL 本来就只回一行，而 Query 要把指针
	// 停在 Next() 上才拿得到值——漏那一步不会报错，只会让每个分类都数是 0
	// （第一次跑就踩到了：测试红在"pet 应该 0"以外的列上）。
	if err := s.db.QueryRow(query, args...).Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("统计工坊模组缓存的分类计数: %w", err)
	}
	for i, t := range tags {
		out[t] = vals[i]
	}
	return out, nil
}

func (s *Store) querySeen(query string, args ...any) ([]SeenMod, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询工坊模组缓存: %w", err)
	}
	defer rows.Close()
	var out []SeenMod
	for rows.Next() {
		var (
			m           SeenMod
			tagsJSON    string
			children    int
			clientReq   int
			serverOnly  int
			first, last int64
		)
		if err := rows.Scan(&m.WorkshopID, &m.Name, &m.Description, &m.Version,
			&m.WorkshopVersion, &tagsJSON,
			&m.Subscriptions, &m.ModUpdatedAt, &m.FileSize, &m.PreviewURL,
			&children, &clientReq, &serverOnly, &m.Origin, &first, &last,
			&m.Favorited, &m.VoteScore, &m.VotesTotal); err != nil {
			return nil, fmt.Errorf("解码工坊模组缓存: %w", err)
		}
		m.HasChildren, m.ClientRequired, m.ServerOnly = children == 1, clientReq == 1, serverOnly == 1
		m.FirstSeenAt, m.LastSeenAt = time.UnixMilli(first).UTC(), time.UnixMilli(last).UTC()
		// 标签解不出来就把这一条当成没有标签：缓存的标签只用于展示和两个提醒位，
		// 为它让整个检索接口失败是不划算的。
		_ = json.Unmarshal([]byte(tagsJSON), &m.Tags)
		out = append(out, m)
	}
	return out, rows.Err()
}

// likePattern 把用户输入变成安全的 LIKE 模式；全空输入返回空串，表示不加 WHERE。
func likePattern(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}
