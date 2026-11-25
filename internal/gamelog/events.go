// 玩家事件表：把"谁在什么时候做了什么"排成一页一页的表，每行尽量把这个人说全
// —— 显示名、角色、账户编号、SteamID、来源 IP、动作、动作原文。
//
// 为什么从"按人汇总的时间线"换成表格（2026-11-03 用户明确要求，参考别家面板）：
// 汇总回答"这一轮谁干了什么"，表格回答"刚才那一下是谁干的、从哪连进来的"。
// 管理员排查捣乱玩家时要的是后者：他要顺着 IP 和 SteamID 去找人，
// 而不是先数出"他死了 3 次"再逐行去日志里翻原文。
//
// 为什么分页和筛选放在后端而不是前端：
//   - 窗口上限是每份文件 2000 行，前端拿到全部再切页等于把两整份日志的解析结果
//     都塞进浏览器，而手机上只需要 20 行；
//   - 界面底部那句"第 1~20 条 / 共 312 条"必须是对真的发出去的东西说的。
//     前端自己数就只能说"这一页有 20 条"，总数得另外编一个。
//
// 每一列的证据来源都写在这里，因为不是每列都能从同一行里拿到（原文取自
// 2026-11-03 云机 Cluster_1/Master/server_log.txt，逐行核对过）：
//
//	[00:46:05]: Client connected from 117.172.19.249|16537 <5242766981745090006>
//	[00:46:06]: Client authenticated: (KU_tvX4ySmb) 风萧萧
//	[00:46:08]: [ClientObject] Initialized (authenticated) on server: guid=5242766981745090006 userid=KU_tvX4ySmb netid=76561198928942009 admin=1
//	[00:46:34]: Spawn request: wilson from 风萧萧
//	[01:16:44]: [Steam] SendUserDisconnect for '76561198928942009'
//
// 聊天文件那边则是另一个形状（实测只有标签，没有句子）：
//
//	[00:46:24]: [Join Announcement] 风萧萧
//
// 所以 IP、SteamID、角色只存在于服务端日志那一份里，而"进服/离开"只存在于聊天文件里：
// 一次扫描先按会话把两边的人对上（guid → 账户编号 → 显示名），
// 之后每条行为行才有资格说"这就是那个从 117.172.19.249 连进来的人"。
// 对不上就留空而不是猜——把 IP 安错人头上比空着严重得多。
//
// 聊天行（玩家发言）默认不进这张表：一个 16 人服一轮能打出上千句，
// 表格第一页会被聊天挤满，而"他说过什么"在日志页那份筛选里看更合适。
// 界面上给的是真开关（chat=1），不是永远为空的选项。
package gamelog

import (
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EventDefaultLines 是事件表默认回看的行数（每份文件）。
// 和日志页的 200 不同：表格要"数得出总共多少条"，窗口越大总数越接近真相。
const EventDefaultLines = 500

// 每页行数。上限 100 是为了守住一次响应的大小：每行带 raw（最长 1000 字）
// 和六七个身份字段，100 行已经几百 KB，再大手机上要转半天。
const (
	EventDefaultSize = 20
	EventMaxSize     = 100
)

// identityLines 是身份索引单独用的窗口，比行为窗口大一档（直接取读取上限）。
// 理由是这两件事的时间尺度不一样：连接行只在玩家进来的那一刻打一次，
// 而他在世界里活动几小时之后行为行还在往文件尾部追加。
// 行为窗口只要 500 行，身份索引却得能一直回看到这一轮开头，
// 否则"下午进来的人傍晚干的事"就没有 IP 了。
const identityLines = MaxLines

// behaviourOrder 决定"哪些行算玩家行为"，同时也是表格里动作筛选项的顺序。
// 一张表两用：筛谁和数谁共用它，免得出现"字典里有它、表格里永远没有它"。
var behaviourOrder = []string{
	EventAuthOK, EventPlayerJoin, EventPlayerLeave,
	EventPlayerDeath, EventGhostGone, EventResurrect,
	EventAFKStart, EventAFKEnd,
	EventKick, EventBan,
	EventChat,
}

// behaviourTone 决定动作芯片的颜色档位。"死亡 2"和"进服 2"长一个样的话，
// 一屏里看不出该处理什么；中性那几档留白，才让带色的真的显眼。
var behaviourTone = map[string]string{
	EventPlayerDeath: "warn", EventGhostGone: "warn", EventAFKStart: "warn",
	EventKick: "bad", EventBan: "bad",
	EventResurrect: "ok", EventAFKEnd: "ok",
}

// EventRow 是表格的一行：一次行为，加上这个人这一轮的已知身份。
type EventRow struct {
	// Offset 让界面能一句话指回日志页那一行（同一份文件里它唯一）。
	Offset int64  `json:"offset"`
	Clock  string `json:"clock,omitempty"`
	At     string `json:"at,omitempty"`
	Kind   string `json:"kind"`
	// KindLabel 是"这行出自哪份文件"。IP 和 SteamID 只有服务端日志那份有，
	// 界面把它写出来，用户才知道聊天行那些空列不是面板漏读了。
	KindLabel string `json:"kindLabel"`
	Shard     string `json:"shard"`

	Event  string `json:"event"`
	Name   string `json:"name"`
	Label  string `json:"label,omitempty"`
	Tone   string `json:"tone,omitempty"`
	Player string `json:"player,omitempty"`
	Target string `json:"target,omitempty"`

	// UserID / SteamID / IP / Prefab / Admin 都不来自这一行本身，
	// 而是这一轮里关于这个人的会话索引。凑不到就是空字符串。
	UserID  string `json:"userid,omitempty"`
	SteamID string `json:"steamid,omitempty"`
	IP      string `json:"ip,omitempty"`
	Prefab  string `json:"prefab,omitempty"`
	Admin   bool   `json:"admin,omitempty"`

	Raw string `json:"raw"`
	// Cut 沿用条目层的截断标记：原文超长时这里给的是前 1000 字。
	Cut bool `json:"cut,omitempty"`
}

// EventPage 是一次筛选 + 分页的结果。
type EventPage struct {
	Cluster string `json:"cluster"`
	Shard   string `json:"shard"`
	// Window 是每份文件回看的行数；Size/TotalPage 是这一页的口径。
	Window int `json:"window"`
	Size   int `json:"size"`
	Page   int `json:"page"`
	// Matched 是筛选之后、分页之前的总条数。界面那句"共 N 条"就是它。
	Matched int `json:"matched"`
	// Pages 是总页数（Matched 除以 Size 向上取整，至少 1）。
	Pages int `json:"pages"`
	// Scanned 是两份文件窗口里解析出的行数（含没定性成行为的），
	// 和 Matched 一起给，用户才分得清"筛掉了"和"根本没读到"。
	Scanned int `json:"scanned"`
	// People 是筛出这些行里涉及的人数（认不出是谁的行合计算一个）。
	People int `json:"people"`

	// RoundStart 是这一轮的启动时刻（服务端日志里那行 Current time）。
	// 没有它整张表的时间都只敢说"距启动多久"。
	RoundStart string `json:"roundStart,omitempty"`
	// Dupled 是两份文件里同一下的抄本数，NoOwner 是认不出是谁的行数，
	// ChatHidden 是因为"不把发言算进来"这个开关收掉的行数。
	// 三个数分开给：合成一个"合掉了 N 条"就说不清到底是哪一种，
	// 而用户看到总数比日志页少时问的正是"少的那些去哪了"。
	Dupled     int  `json:"dupled"`
	NoOwner    int  `json:"noOwner"`
	ChatHidden int  `json:"chatHidden"`
	Older      bool `json:"older"`
	// ChatIn 说明这一页里有没有把玩家发言算进来：它决定"共 N 条"是大是小，
	// 必须跟着结果一起外送，不能只写在界面上的开关里。
	ChatIn bool `json:"chatIn"`
	// IdentitySeen 是这一轮里认出的人数（一个人的连接、验证、落地三条线索算一个，
	// 只连上没走完验证的那条不算）。界面用它说明"为什么有些行没有 IP"：
	// 认得出的人只有这些，超出这一轮的连接行早就滚出窗口了。
	IdentitySeen int        `json:"identitySeen"`
	Rows         []EventRow `json:"rows"`
	Note         string     `json:"note,omitempty"`
}

// EventQuery 是表格的筛选与分页条件。零值走默认，越界一律由接口层先拦掉。
type EventQuery struct {
	Lines  int
	Page   int
	Size   int
	Player string
	Event  string
	Text   string
	Chat   bool
}

// identity 是一个人这一轮的身份线索。指针共享，所以连接行后到、
// 验证行先到时两边都能看见对方的字段。
type identity struct {
	GUID    string
	UserID  string
	Name    string
	SteamID string
	IP      string
	Prefab  string
	Admin   bool
}

// identities 是三张查人用的索引。同一个 identity 对象可能同时挂在三张表里。
type identities struct {
	byGUID map[string]*identity
	byUser map[string]*identity
	byName map[string]*identity
	// nameUser 只用来把"公告里只有名字"的行接到"账户编号"那条链上。
	nameUser map[string]string
	all      []*identity
}

func newIdentities() *identities {
	return &identities{
		byGUID:   map[string]*identity{},
		byUser:   map[string]*identity{},
		byName:   map[string]*identity{},
		nameUser: map[string]string{},
	}
}

func (id *identities) guid(g string) *identity {
	if g == "" {
		return nil
	}
	if v := id.byGUID[g]; v != nil {
		return v
	}
	v := &identity{GUID: g}
	id.byGUID[g] = v
	id.all = append(id.all, v)
	return v
}

func (id *identities) user(u string) *identity {
	if u == "" {
		return nil
	}
	if v := id.byUser[u]; v != nil {
		return v
	}
	v := &identity{UserID: u}
	id.byUser[u] = v
	id.all = append(id.all, v)
	return v
}

// merge 把两条线索并成一条。同一个人在不同行里被不同线索认出来
// （连接行只有 guid，公告行只有名字，聊天行两个都有），
// 不合并的话同一个人会在表里显示成"有的行有 IP、有的行没有"。
func (id *identities) merge(a, b *identity) *identity {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if a == b {
		return a
	}
	// 留下 a（先出现的那条），把 b 的字段补进来，再把所有索引改指 a。
	for _, f := range []struct{ dst, src *string }{
		{&a.GUID, &b.GUID}, {&a.UserID, &b.UserID}, {&a.Name, &b.Name},
		{&a.SteamID, &b.SteamID}, {&a.IP, &b.IP}, {&a.Prefab, &b.Prefab},
	} {
		if *f.dst == "" {
			*f.dst = *f.src
		}
	}
	if b.Admin {
		a.Admin = true
	}
	if b.GUID != "" {
		id.byGUID[b.GUID] = a
	}
	if b.UserID != "" {
		id.byUser[b.UserID] = a
	}
	for n, v := range id.byName {
		if v == b {
			id.byName[n] = a
		}
	}
	return a
}

func (id *identities) name(n string) *identity {
	if n == "" {
		return nil
	}
	return id.byName[n]
}

func (id *identities) setName(n string, v *identity) {
	if n == "" || v == nil {
		return
	}
	id.byName[n] = v
	if v.Name == "" {
		v.Name = n
	}
	if v.UserID != "" {
		id.nameUser[n] = v.UserID
	}
}

// 身份链的正则。只认引擎自己拼的固定形状，解不出来就当这条线索不存在。
var (
	// 连接行有两种前缀（实测同一刻各打一条，内容一样），任意一条都够填 IP。
	connectedRe = regexp.MustCompile(`(?:Client connected from|New incoming connection)\s+(\S+?)\|(\d+)\s+<(\d+)>`)
	// 自机那一行也走同一个形状，但 netid 是空的：这时候不能给人编一个 SteamID。
	clientObjRe     = regexp.MustCompile(`\[ClientObject\] Initialized \(([^)]*)\) on server: guid=(\d+) userid=(\S*) netid=(\S*) admin=(\d)`)
	authenticatedRe = regexp.MustCompile(`Client authenticated:\s*\((KU_[A-Za-z0-9]{4,})\)\s*(\S.*?)\s*$`)
	spawnRe         = regexp.MustCompile(`^Spawn request:\s*([A-Za-z0-9_]+)\s+from\s+(.+?)\s*$`)
	// 纯数字的第二槽位不是显示名。实测两份日志里都见过：
	// 有的账户这一槽打的是 Klei 数字 id（本地夹具那条 14672355439136578013），
	// 认成名字的话，同一个人会因为两行写法不同而裂成两条，
	// 而"名字"列里出现一串数字比空着更容易被当成面板读错了。
	digitsOnlyRe = regexp.MustCompile(`^\d{6,}$`)
)

// scanIdentity 扫服务端日志里的连接/验证/生成行，建这一轮的身份索引。
//
// 用的是完整行（把引擎的方括号标签拼回去）：`[ClientObject] Initialized …` 这条
// 在条目层已经标签被拆走、Raw 只剩 `Initialized …`，正则找不到 `[ClientObject]`。
func (id *identities) scanIdentity(entries []Entry) {
	for _, e := range entries {
		full := e.Raw
		if e.Module != "" && !strings.HasPrefix(full, "["+e.Module+"]") {
			full = "[" + e.Module + "] " + full
		}
		if m := connectedRe.FindStringSubmatch(full); m != nil {
			v := id.guid(m[3])
			v.IP = m[1]
			continue
		}
		if m := clientObjRe.FindStringSubmatch(full); m != nil {
			v := id.guid(m[2])
			u := strings.TrimSpace(m[3])
			// netid 为空是引擎的自机行（本机玩家），不是"读不到"：
			// 这时把 userid 认下来，但 SteamID 保持空。
			if s := strings.TrimSpace(m[4]); s != "" {
				v.SteamID = s
			}
			v.Admin = m[5] == "1"
			if u != "" {
				v.UserID = u
				id.byUser[u] = id.merge(id.byUser[u], v)
			}
			continue
		}
		if m := authenticatedRe.FindStringSubmatch(full); m != nil {
			v := id.user(m[1])
			n := strings.TrimSpace(m[2])
			if n != "" && !digitsOnlyRe.MatchString(n) {
				id.setName(n, v)
			}
			continue
		}
		if m := spawnRe.FindStringSubmatch(full); m != nil {
			n := strings.TrimSpace(m[2])
			v := id.name(n)
			if v == nil {
				v = &identity{Name: n}
				id.all = append(id.all, v)
				if u := id.nameUser[n]; u != "" {
					v = id.merge(v, id.user(u))
				}
				id.setName(n, v)
			}
			v.Prefab = m[1]
			continue
		}
	}
	// 最后把 byUser 里的重复对象收一遍：同一个人可能先按 guid 建、又按 userid 建。
	for _, v := range id.all {
		if v.UserID != "" {
			id.byUser[v.UserID] = id.merge(id.byUser[v.UserID], v)
		}
		if v.Name != "" {
			id.byName[v.Name] = id.merge(id.byName[v.Name], v)
		}
	}
}

// people 是"这一轮认出了几个人"。按三张索引去重，而不是直接数 all：
// merge 会把后出现的那条线索并到先出现的那条上，被并掉的对象还挂在 all 里，
// 数 all 会把同一个人的"连接线索"和"验证线索"数成两个人，
// 界面那句"认出了 N 个人的连接线索"就虚高了。
// 只剩 IP 的那条（连上了但没走完验证）不算一个人：它接不到任何名字，也解释不了任何行。
func (id *identities) people() int {
	set := map[*identity]struct{}{}
	for _, m := range []map[string]*identity{id.byGUID, id.byUser, id.byName} {
		for _, v := range m {
			if v != nil {
				set[v] = struct{}{}
			}
		}
	}
	n := 0
	for v := range set {
		if v.UserID != "" || v.Name != "" || v.SteamID != "" {
			n++
		}
	}
	return n
}

// resolve 查这一行的身份。三个入口按可靠程度排队：
// 账户编号（引擎和名单文件共用的稳定标识）→ 显示名 → 这一行自己带的名字。
// 查不到就返回空身份：宁可几列是空的，也不能把别人的 IP 写到这行上。
func (id *identities) resolve(row EventRow) *identity {
	if v := id.byUser[row.UserID]; v != nil {
		return v
	}
	if row.Player != "" {
		if v := id.byName[row.Player]; v != nil {
			return v
		}
		if u := id.nameUser[row.Player]; u != "" {
			if v := id.byUser[u]; v != nil {
				return v
			}
		}
	}
	return nil
}

// link 把聊天行自己带的"名字 + 账户编号"补进索引。
//
// 这一步为什么必要：连接/验证行只出现在服务端日志里，而一个玩家进来之后
// 说的第一句话往往在聊天文件里。聊天行两个标识都带着，是最便宜的接线机会——
// 错过它，之后所有只有名字的公告行都接不到 IP 那条链上。
func (id *identities) link(e Entry) {
	if e.UserID == "" {
		return
	}
	v := id.user(e.UserID)
	n := strings.TrimSpace(e.Player)
	if n == "" || digitsOnlyRe.MatchString(n) {
		return
	}
	if cur := id.byName[n]; cur != nil && cur != v {
		id.merge(cur, v)
	}
	id.setName(n, v)
}

// Events 读一个分片这一轮的两份日志，筛出行为行并按页给出。
//
// 两份都读不到才当错误（os.ErrNotExist）往上抛，界面该说"还没有日志文件"；
// 只缺一份照常出结果，并在 Note 里说明。
func (r *Reader) Events(cluster, shard string, q EventQuery) (*EventPage, error) {
	if q.Lines <= 0 {
		q.Lines = EventDefaultLines
	}
	if q.Lines > MaxLines {
		q.Lines = MaxLines
	}
	if q.Size <= 0 {
		q.Size = EventDefaultSize
	}
	if q.Size > EventMaxSize {
		q.Size = EventMaxSize
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	// 「动作」下拉里有玩家发言这一档（它是行为目录里的一项），而默认那一道开关把
	// 发言整档收掉了。用户点名要发言还按关着算，就会得到一张空表——
	// 那张空表说的是假话：这一轮明明有人说过话。所以动作里点名了发言就当它被显式打开。
	if q.Event == EventChat {
		q.Chat = true
	}

	p := &EventPage{
		Cluster: cluster, Shard: shard, Window: q.Lines, Size: q.Size,
		ChatIn: q.Chat, Rows: []EventRow{},
	}
	ident := newIdentities()
	var rows []EventRow
	var roundStart time.Time
	var got, miss int
	seen := map[string]struct{}{}

	// 服务端日志先读：身份链全在这份里，行为行要用它。
	for _, kind := range []Kind{KindServer, KindChat} {
		lines := q.Lines
		if kind == KindServer && identityLines > lines {
			lines = identityLines
		}
		snap, err := r.Tail(cluster, shard, kind, lines)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				miss++
				continue
			}
			return nil, err
		}
		if snap.Start.IsZero() {
			stampAt(snap.Entries, roundStart)
		} else {
			roundStart = snap.Start
		}
		got++
		p.Older = p.Older || snap.Older
		if kind == KindServer {
			ident.scanIdentity(snap.Entries)
		}
		// 窗口里比行为窗口多出来的那一段只用来建身份，不进表：
		// 否则"回看 500 行"这句会变成一个假话。
		from := 0
		if kind == KindServer && len(snap.Entries) > q.Lines {
			from = len(snap.Entries) - q.Lines
			// 身份窗口比行为窗口深，所以 Tail 报的 older 可能是 false 而这里确实裁掉了行：
			// older 说的必须是"行为行有没有被窗口切掉"，不是"文件有没有被读完"。
			p.Older = true
		}
		// scanned 说的是"筛了这么多行"，不是"从盘上读了这么多行"。
		// 服务端那份为了接身份会多读一大截（见 identityLines），把它算进来就会出现
		// 头部写"每份文件回看 200 行"、底部写"从 313 行里定性出来的"这种自相矛盾。
		p.Scanned += len(snap.Entries) - from
		for _, e := range snap.Entries[from:] {
			if kind == KindChat {
				ident.link(e)
			}
			if !isBehaviour(e.Event) {
				continue
			}
			if e.Event == EventChat && !q.Chat {
				p.ChatHidden++
				continue
			}
			who := strings.TrimSpace(e.Player)
			// 进出的公告在两份文件里写法不同（服务端是整句 "X has joined the game"，
			// 聊天那份带 [Join Announcement] 标签），同一个进程打的两条抄本原文不一样。
			// 只按原文比对合不掉它们，一个人进服就会被数成两记，
			// 所以进出这两类按"同一秒 + 同一个人 + 同一种进出"合：一秒内进两次不可能。
			key := e.Clock + "\x00" + e.Event + "\x00" + e.Raw
			if who != "" && (e.Event == EventPlayerJoin || e.Event == EventPlayerLeave) {
				key = e.Clock + "\x00" + e.Event + "\x00" + who
			}
			if _, ok := seen[key]; ok {
				p.Dupled++
				continue
			}
			seen[key] = struct{}{}
			rows = append(rows, EventRow{
				Offset: e.Offset, Clock: e.Clock, At: e.At,
				Kind: string(kind), KindLabel: kind.Label(), Shard: shard,
				Event: e.Event, Name: eventShortName(e.Event), Label: e.EventLabel,
				Tone:   behaviourTone[e.Event],
				Player: who, Target: e.Target,
				UserID: e.UserID, Raw: e.Raw, Cut: e.Cut,
			})
		}
	}
	if got == 0 {
		return nil, os.ErrNotExist
	}
	if !roundStart.IsZero() {
		p.RoundStart = roundStart.UTC().Format(time.RFC3339)
	}
	p.IdentitySeen = ident.people()

	// 身份补齐要放在两份文件都扫完之后：聊天行里的"名字↔账户编号"
	// 可能比它对应的公告行更晚出现，先补会把该接上的线接丢。
	for i := range rows {
		v := ident.resolve(rows[i])
		if v == nil {
			continue
		}
		if rows[i].UserID == "" {
			rows[i].UserID = v.UserID
		}
		if rows[i].Player == "" {
			rows[i].Player = v.Name
		}
		rows[i].SteamID, rows[i].IP, rows[i].Prefab = v.SteamID, v.IP, v.Prefab
		rows[i].Admin = v.Admin
	}

	sort.SliceStable(rows, func(i, j int) bool { return clockGreater(rows[i], rows[j]) })
	p.NoOwner = 0
	kept := rows[:0]
	for _, row := range rows {
		if row.Player == "" && row.UserID == "" {
			p.NoOwner++
		}
		if q.match(row) {
			kept = append(kept, row)
		}
	}
	p.Matched = len(kept)
	p.People = countPeople(kept)
	p.Pages = (p.Matched + q.Size - 1) / q.Size
	if p.Pages < 1 {
		p.Pages = 1
	}
	if q.Page > p.Pages {
		// 页码越界不回空页也不报错：夹到最后一页并把实际用的页号回写出去。
		// 前端拿着回写的 page 修自己的状态就行，把"改了筛选还剩 3 页、你却停在第 7 页"
		// 报成 400 只会让用户以为筛选出错了。
		q.Page = p.Pages
	}
	p.Page = q.Page
	start := (p.Page - 1) * q.Size
	end := start + q.Size
	if end > p.Matched {
		end = p.Matched
	}
	p.Rows = append(p.Rows, kept[start:end]...)

	if miss > 0 {
		p.Note = "有一份日志还没写出来，上面只数了读到的那一份；世界跑过一轮之后两份都会齐"
	}
	if p.Older {
		p.Note = moreNote(p.Note, "只回看了每份文件最近 "+strconv.Itoa(q.Lines)+" 行，更早的行为没算进来；共 N 条是这一段里的总数")
	}
	if p.Matched == 0 {
		p.Note = moreNote(p.Note, "这些条件在这个窗口里一条都没命中；窗口内的行为行总数可以在日志页那份逐行列表里翻到")
	}
	if !q.Chat {
		p.Note = moreNote(p.Note, "玩家发言默认不进这张表（连同发言一起看就打开聊天那一档）")
	}
	if p.Dupled > 0 {
		p.Note = moreNote(p.Note, "两份文件里同一下的抄本合掉了 "+strconv.Itoa(p.Dupled)+" 条")
	}
	return p, nil
}

// match 是行级筛选：人、动作、关键词各管一栏，条件是"与"。
// 关键词要盖住身份列（IP、SteamID、账户编号、角色），
// 因为这张表存在的意义就是"顺着 IP 或编号去找人"。
func (q EventQuery) match(row EventRow) bool {
	if q.Event != "" && row.Event != q.Event {
		return false
	}
	if q.Player != "" {
		hay := strings.ToLower(row.Player + " " + row.Target + " " + row.UserID)
		if !strings.Contains(hay, q.Player) {
			return false
		}
	}
	if q.Text != "" {
		hay := strings.ToLower(row.Raw + " " + row.Player + " " + row.Target + " " +
			row.UserID + " " + row.SteamID + " " + row.IP + " " + row.Prefab)
		if !strings.Contains(hay, q.Text) {
			return false
		}
	}
	return true
}

func isBehaviour(code string) bool {
	for _, c := range behaviourOrder {
		if c == code {
			return true
		}
	}
	return false
}

// BehaviourCatalog 是界面"动作"下拉里的选项：短名、整句、颜色档一起给。
// 从 behaviourOrder 推而不是手写一份，界面就不会筛出一个表格里永远没有的行为。
func BehaviourCatalog() []EventInfo {
	out := make([]EventInfo, 0, len(behaviourOrder))
	for _, code := range behaviourOrder {
		out = append(out, EventInfo{Code: code, Name: eventShortName(code), Label: eventFullLabel(code)})
	}
	return out
}

// eventShortName 取界面上的短名。规则表里没这个名字时退回整句，再退回码值：
// 表格宁可写长一点，也不能露出 player_death 这种码值（这是面板的硬规矩）。
func eventShortName(code string) string {
	if n, ok := eventNames[code]; ok {
		return n
	}
	if l := eventFullLabel(code); l != "" {
		return l
	}
	return code
}

// eventFullLabel 取整句解释。同一个事件在规则表里可能有好几条（消息一条、标签一条），
// 这里取第一条说得出话的，字典和表格前缀才不会是两种说法。
func eventFullLabel(code string) string {
	for _, rl := range rules {
		if rl.event == code && rl.label != "" {
			return rl.label
		}
	}
	for _, rl := range labelRules {
		if rl.event == code && rl.label != "" {
			return rl.label
		}
	}
	switch code {
	case EventChat:
		return labelChat
	case EventAnnounce:
		return labelAnnounce
	}
	return ""
}

// clockLess 是同向（旧→新）的比较，留给需要正序的地方用；表格用的是 clockGreater。
func clockLess(a, b EventRow) bool {
	sa, oka := ClockSeconds(a.Clock)
	sb, okb := ClockSeconds(b.Clock)
	if oka != okb {
		return oka
	}
	if oka && sa != sb {
		return sa < sb
	}
	if a.Offset != b.Offset {
		return a.Offset < b.Offset
	}
	return a.Kind < b.Kind
}

func moreNote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "；" + b
}

// countPeople 数"这些行里涉及几个人"。认不出是谁的合计算一个，
// 而不是每个都不数：那样界面上"共 40 条 / 涉及 40 个人"会把没名字的行说成没名字的人。
func countPeople(rows []EventRow) int {
	set := map[string]struct{}{}
	unnamed := false
	for _, row := range rows {
		switch {
		case row.UserID != "":
			set[row.UserID] = struct{}{}
		case row.Player != "":
			set["n:"+row.Player] = struct{}{}
		default:
			unnamed = true
		}
	}
	if unnamed {
		set["?"] = struct{}{}
	}
	return len(set)
}

// clockGreater 判断 a 是不是比 b 新。和 clockLess 同一个比较次序（时钟、
// 再文件内位置、再种类），只是方向反过来，所以两处不会出现"一边升序一边升序"。
func clockGreater(a, b EventRow) bool {
	sa, oka := ClockSeconds(a.Clock)
	sb, okb := ClockSeconds(b.Clock)
	if oka != okb {
		return oka
	}
	if oka && sa != sb {
		return sa > sb
	}
	if a.Offset != b.Offset {
		return a.Offset > b.Offset
	}
	return a.Kind > b.Kind
}
