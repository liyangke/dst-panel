// 玩家行为落库与时间线归并。
//
// 为什么归并写在 store 而不是交给界面：一段"在服区间"是两条日志行相减得来的，
// 前端再算一遍就有两处对"他到底在线多久"的说法——这个仓库里已经栽过一次
// （趋势图的归并口径写了两处，界面上写着"过去 24 小时"，图上最右端是 6 小时前）。
// 时长、开口段、被窗口切掉的头，全部在这里定完，接口只负责翻成 JSON。
//
// 行为码值一律用 gamelog 那一份常量，不在这里另摆一张表：
// 认引擎英文原句的规则只有一处，解释它的表也只能有一处。
package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"dstpanel/internal/gamelog"
)

// PlayerEventRow 是 player_events 的一行。
type PlayerEventRow struct {
	At      time.Time
	Round   int64
	Cluster string
	Shard   string
	UserID  string
	Name    string
	Event   string
	Source  string
	Clock   string
}

// RecordPlayerEvents 一批写进去，撞上去重键的那几条直接忽略（键的由来见 schema.sql）。
// 返回真正落库的条数，归档器拿它判断"这一轮有没有新东西"。
func (s *Store) RecordPlayerEvents(evs []PlayerEventRow) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	written := 0
	err := s.inTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO player_events
			(at, round, cluster, shard, userid, name, event, source, clock)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(cluster, shard, round, at, event, userid, name) DO NOTHING`)
		if err != nil {
			return fmt.Errorf("准备写入玩家行为: %w", err)
		}
		defer func() { _ = stmt.Close() }()
		for _, e := range evs {
			res, err := stmt.Exec(e.At.UnixMilli(), e.Round, e.Cluster, e.Shard,
				e.UserID, e.Name, e.Event, e.Source, e.Clock)
			if err != nil {
				return fmt.Errorf("写玩家行为 %s/%s %s: %w", e.Cluster, e.Shard, e.Event, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("写玩家行为后取行数: %w", err)
			}
			written += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// TimelineSpan 是一段"这个人在服里"的区间。
type TimelineSpan struct {
	From, To time.Time
	// EndedBy 说清这一段是怎么收尾的：leave 他自己走了、restart 世界起了新一轮、
	// stop 世界停了（停在最后一次可信采样）、open 到查询这一刻为止日志里没有他离开的记录。
	// 取值见 SpanEnd* 常量，中文说法由接口层给——界面不出现码值。
	EndedBy string
	// ClippedStart 表示这一段没有起点行：窗口开头他就已经在里面了，从窗口边界画起。
	ClippedStart bool

	// 下面三个只在归并过程中用：开口段要靠"哪个分片的哪一轮"去找下一轮启动时刻。
	Shard string
	Round int64
	Open  bool
}

// TimelineMark 是区间上的一个记号（死亡、幽灵消散、被复活）。
type TimelineMark struct {
	At    time.Time
	Event string
}

// EventCount 是某一种行为在这一窗里发生的次数（去重之后）。
type EventCount struct {
	Event string
	Count int
}

// TimelinePerson 是一个人在这个窗口里的全部段落与记号。
type TimelinePerson struct {
	UserID string
	Name   string
	Spans  []TimelineSpan
	Marks  []TimelineMark
	// Seconds 是各段时长相加，界面那句"这段时间里一共在线 1 小时 12 分"就是它。
	Seconds int64
	// OnlineNow 只在有一段的收尾方式是 open 时为真。
	OnlineNow bool
}

// Timeline 是一张时间线的全部答案。
type Timeline struct {
	People []TimelinePerson
	// EventCounts 是这一窗里每种行为各发生几次，顺序按次数从多到少。
	// 口径和 Rows 是同一套：同一条公告被两个世界各写一遍的只算一次，
	// 所以这几条加起来正好等于 Rows——总览那张「行为构成」靠这一点才敢
	// 把分母写成上面那句「N 次行为」，两处不一致迟早飘成两个数。
	EventCounts []EventCount
	// RecordFrom 是这个房间在库里最早的一条行为，不受窗口限制。
	// 界面那句"面板从什么时候开始攒这份记录"取它——归档是新加的能力，
	// 早于它的历史真的不存在，图上不能假装一直有。
	RecordFrom time.Time
	// LastSampleAt 是这个房间最后一次可信在线采样。世界停摆时采样就停了，
	// 所以它是"还没离开的那个人该在哪儿收口"的唯一凭据。零值表示一条采样都没有。
	LastSampleAt time.Time
	// Rows 是这次归并真正用到的次数：一次发生算一次。
	// 同一条公告被两个世界各写一遍时库里有两行，这里只算一次——界面上那个
	// "N 次行为"是这张图的分母，它得跟图上的段与记号对得上，不能是读上来的行数。
	Rows int
	// Truncated 为真表示窗口里的行数撞了上限，后面的人没算进来。
	Truncated bool
}

// 区间收尾方式。
const (
	SpanEndLeave   = "leave"
	SpanEndRestart = "restart"
	SpanEndStop    = "stop"
	SpanEndOpen    = "open"
)

// TimelineRowLimit 是一次归并最多读多少行。按 5 种行为算，一台 16 人服跑满保留期
// 也就几千行，取 5000 是"正常情况下永远撞不到，真有人把房间开成公网服也拖不垮接口"。
// 撞到了不静默：Truncated 会带着它上屏，接口那句说明里也报同一个数。
const TimelineRowLimit = 5000

// migrateGap 是"离开后又进来"仍算同一次在服的间隔。
//
// 为什么要有这条：在洞穴和地表之间来回时，引擎打的是"从这个世界离开"+"从那个世界进来"
// 两行，中间只隔几秒。照字面连起来画，一个人换个世界就会被画成"出去了又回来"，
// 而用户看到的是一段他根本没离开过的时间。一分钟是迁移实测的尺度，
// 真出去吃个饭再回来不会卡在一分钟以内。这条规则必须写进接口那句 note，让图上说得出来。
const migrateGap = time.Minute

// timelineEvents 是进这张表的几种行为。
//
// 只收"能决定一个人在不在世界里、以及他在里面出了什么事"的五行：
// 进出定区间，死亡/幽灵/复活是区间上的记号。挂机（afk）不算离开——连接还在，
// 引擎自己也是这么说的；踢人和封禁是管理员动作，事件页那份表格里看得更全，
// 画在时间线上只会挤掉真正的时间信息。
var timelineEvents = map[string]bool{
	gamelog.EventPlayerJoin:  true,
	gamelog.EventPlayerLeave: true,
	gamelog.EventPlayerDeath: true,
	gamelog.EventGhostGone:   true,
	gamelog.EventResurrect:   true,
}

// IsTimelineEvent 给归档器筛行用：判定表留在这一层，调用方不再抄一份取值。
func IsTimelineEvent(code string) bool { return timelineEvents[code] }

// sortedTally 把计数表排成"该先看见哪一类"：次数多的在前，一样多的按行为目录走。
// 目录那一串本来就是事情发生的顺序（进服 → 离开 → 死亡 → 幽灵 → 复活），
// 而人少的房间五类常常各算出一样的次数，这时候按码值排会排出一脸莫名其妙的次序。
func sortedTally(tally map[string]int) []EventCount {
	rank := map[string]int{}
	for i, it := range gamelog.BehaviourCatalog() {
		rank[it.Code] = i
	}
	out := make([]EventCount, 0, len(tally))
	for code, n := range tally {
		if n > 0 {
			out = append(out, EventCount{Event: code, Count: n})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return rank[out[i].Event] < rank[out[j].Event]
	})
	return out
}

// personAcc 是归并过程中"一个人"的累加器。
type personAcc struct {
	userid string
	name   string
	spans  []TimelineSpan
	marks  []TimelineMark
	// lastOf 是这个人物每种行为最近见过的那一行，只用来挡跨分片重复公告（见 shardRepeat）。
	lastOf map[string]PlayerEventRow
}

// shardRepeat 认"同一次引擎公告在另一个世界的那一份"。
//
// 为什么库里会有两份：地表和洞穴各自写自己的日志，而进出、死亡这类公告是整个集群一起打的，
// 于是同一次发生落下两行。入库那枚去重键带着 shard 和 round（分片重启的判据要用它们），
// 两份都进得来——它们是两次写，却是一次发生。
// 归并里有两处是不去重的：没有起点行的那段区间每见一条离开就新起一段，
// 记号每见一条死亡就多一个点，于是换个世界的人在线时长直接翻倍、图上多出成对的点。
//
// 判据是"同一个人、同一种行为、来自另一个世界、时刻相差不到一分钟"。
// 时刻为什么能直接比：at 是各分片用"自己的本轮启动时刻 + 自己日志里的相对时钟"换算的，
// 两次换算都朝着同一个真实时刻收敛，两个世界开机早晚的差自己抵消了，剩下的只是取整的秒级差。
// 一分钟沿用 migrateGap 那个尺度：真在两个世界里各出一次事（比如洞穴死后回地表再死一次）
// 中间一定隔着迁移，不会挤在一分钟内。
func shardRepeat(prev, e PlayerEventRow) bool {
	if prev.Event == "" || prev.Shard == e.Shard {
		return false
	}
	d := e.At.Sub(prev.At)
	if d < 0 {
		d = -d
	}
	return d <= migrateGap
}

// PlayerTimeline 把一段时间里的行为行归成"每个人一段段在服区间"。
//
// until 传查询时刻：开口段画到哪儿由它决定，不由"最后一条行"决定，
// 否则一个人只是还没打过死亡公告，图就会把他的那段缩短到最后一次死。
func (s *Store) PlayerTimeline(cluster string, since, until time.Time) (Timeline, error) {
	var out Timeline
	if cluster == "" {
		return out, fmt.Errorf("查时间线必须有房间名")
	}
	if until.Before(since) {
		return out, fmt.Errorf("时间线的结束时刻要在开始之后")
	}

	var minMs, lastMs int64
	if err := s.db.QueryRow(`SELECT IFNULL(MIN(at), 0) FROM player_events WHERE cluster = ?`,
		cluster).Scan(&minMs); err != nil {
		return out, fmt.Errorf("查这个房间最早一条玩家行为: %w", err)
	}
	if minMs > 0 {
		out.RecordFrom = time.UnixMilli(minMs).UTC()
	}
	// 这一列来自另一张表：采样只在进程活着且数据不陈旧时写，所以它同时是
	// "这个世界最后一次在动"。停着的世界里那条还开着的区间必须靠它收口，
	// 否则三天前进来、没打离开行的人，会被画成这三天一直在服里。
	if err := s.db.QueryRow(`SELECT IFNULL(MAX(at), 0) FROM player_snapshots WHERE cluster = ?`,
		cluster).Scan(&lastMs); err != nil {
		return out, fmt.Errorf("查这个房间最后一次在线采样: %w", err)
	}
	if lastMs > 0 {
		out.LastSampleAt = time.UnixMilli(lastMs).UTC()
	}

	rows, err := s.db.Query(`SELECT at, round, shard, userid, name, event
		FROM player_events WHERE cluster = ? AND at >= ? AND at <= ?
		ORDER BY at ASC, id ASC LIMIT ?`,
		cluster, since.UnixMilli(), until.UnixMilli(), TimelineRowLimit+1)
	if err != nil {
		return out, fmt.Errorf("查询玩家行为: %w", err)
	}
	defer rows.Close()

	var raw []PlayerEventRow
	for rows.Next() {
		var (
			e    PlayerEventRow
			at   int64
			roun int64
		)
		if err := rows.Scan(&at, &roun, &e.Shard, &e.UserID, &e.Name, &e.Event); err != nil {
			return out, fmt.Errorf("解码玩家行为: %w", err)
		}
		e.At = time.UnixMilli(at).UTC()
		e.Round = roun
		raw = append(raw, e)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("遍历玩家行为: %w", err)
	}
	if len(raw) > TimelineRowLimit {
		out.Truncated = true
		raw = raw[:TimelineRowLimit]
	}

	// 第一遍只干一件事：把"只有名字的线索"接到账户编号上。
	// 进服公告在聊天文件里只带名字，而同一轮里服务端日志的验证行两个都带；
	// 不合这一手，同一个人会在时间线上出现两行——一行有编号一行没有。
	nameUser := map[string]string{}
	for _, e := range raw {
		if e.UserID != "" && e.Name != "" {
			if _, ok := nameUser[e.Name]; !ok {
				nameUser[e.Name] = e.UserID
			}
		}
	}

	byKey := map[string]*personAcc{}
	var order []string
	skipped := 0
	tally := map[string]int{}
	for _, e := range raw {
		key := personKey(e, nameUser)
		p := byKey[key]
		if p == nil {
			p = &personAcc{lastOf: map[string]PlayerEventRow{}}
			byKey[key] = p
			order = append(order, key)
			if !strings.HasPrefix(key, "n:") {
				p.userid = key
			}
		}
		if p.name == "" {
			p.name = e.Name
		}
		if prev := p.lastOf[e.Event]; shardRepeat(prev, e) {
			skipped++
			continue
		}
		tally[e.Event]++
		p.lastOf[e.Event] = e
		p.absorb(e, since, out.RecordFrom)
	}
	out.Rows = len(raw) - skipped
	out.EventCounts = sortedTally(tally)

	for _, key := range order {
		p := byKey[key]
		s.closeOpenSpans(p.spans, until, out.LastSampleAt, cluster)
		p.spans = mergeOverlapSpans(p.spans)
		tp := TimelinePerson{UserID: p.userid, Name: p.name, Spans: p.spans, Marks: p.marks}
		for _, sp := range p.spans {
			if sp.EndedBy == SpanEndOpen {
				tp.OnlineNow = true
			}
			if d := sp.To.Sub(sp.From); d > 0 {
				tp.Seconds += int64(d / time.Second)
			}
		}
		sort.SliceStable(tp.Marks, func(i, j int) bool { return tp.Marks[i].At.Before(tp.Marks[j].At) })
		out.People = append(out.People, tp)
	}
	// 排序就是"该先看见谁"：现在还在里面的排前面，其余按这段时间里在线多久降序。
	sort.SliceStable(out.People, func(i, j int) bool {
		a, b := out.People[i], out.People[j]
		if a.OnlineNow != b.OnlineNow {
			return a.OnlineNow
		}
		if a.Seconds != b.Seconds {
			return a.Seconds > b.Seconds
		}
		return a.Name < b.Name
	})
	return out, nil
}

// personKey 是"这行算在谁头上"。账户编号优先，其次用这一轮里从编号那里借来的编号，
// 两个都凑不到才按名字分——名字是玩家自己改的，能不用就不用。
func personKey(e PlayerEventRow, nameUser map[string]string) string {
	if e.UserID != "" {
		return e.UserID
	}
	if u := nameUser[e.Name]; u != "" {
		return u
	}
	return "n:" + e.Name
}

// absorb 把一行并进这个人的区间里。行是按时间升序进来的，所以只看最后一段。
func (p *personAcc) absorb(e PlayerEventRow, since, recordFrom time.Time) {
	last := len(p.spans) - 1
	switch e.Event {
	case gamelog.EventPlayerJoin:
		if last >= 0 {
			sp := &p.spans[last]
			// 还在里面又打了一条进服：重连、或者两份文件各抄一份。
			// 不另起一段——凭空造一段"他出去了"比少画一段更糟。
			if sp.EndedBy == "" {
				return
			}
			// 刚走不久又进来：按世界间迁移合成一段（见 migrateGap）。
			if sp.EndedBy == SpanEndLeave && e.At.Sub(sp.To) <= migrateGap {
				sp.EndedBy = ""
				sp.Open = true
				sp.To = e.At
				sp.Shard, sp.Round = e.Shard, e.Round
				return
			}
		}
		p.spans = append(p.spans, TimelineSpan{
			From: e.At, To: e.At, Open: true, Shard: e.Shard, Round: e.Round,
		})
	case gamelog.EventPlayerLeave:
		if last >= 0 && p.spans[last].EndedBy == "" {
			sp := &p.spans[last]
			if e.At.After(sp.From) {
				sp.To = e.At
			}
			sp.Open = false
			sp.EndedBy = SpanEndLeave
			return
		}
		// 窗口开头他已经在里面：这一段的起点日志里没有（进服行滚出窗口了，
		// 或者记录起点就在那之后），只能从几个下界里最晚的那个画起，
		// 并且必须带着 ClippedStart 上屏——那是"这里是被切开的"，不是"他就是这时候进来的"。
		from := since
		if !recordFrom.IsZero() && recordFrom.After(from) {
			from = recordFrom
		}
		// 本轮的启动时刻也是下界：这一轮里根本没有他的进服行，而世界是这一轮才起来的，
		// 那一段最多只能从这一轮算起。少了这一手，上一轮的记录起点会把这一段往前拉长，
		// 图上就成了两段重叠、时长把上一轮那段又算了一遍（真机上一天重启几次是常态）。
		if e.Round > 0 {
			if rs := time.UnixMilli(e.Round).UTC(); rs.After(from) {
				from = rs
			}
		}
		if from.Before(e.At) {
			p.spans = append(p.spans, TimelineSpan{
				From: from, To: e.At, ClippedStart: true, EndedBy: SpanEndLeave,
			})
		}
	case gamelog.EventPlayerDeath, gamelog.EventGhostGone, gamelog.EventResurrect:
		p.marks = append(p.marks, TimelineMark{At: e.At, Event: e.Event})
	}
}

// closeOpenSpans 给还没收口的段落定下"画到哪儿"。三种收口，按证据强弱排：
//
//   - restart：这个分片之后又起过一轮。引擎每轮启动会截断重写日志，上一轮里
//     "还没离开"的人最晚只能算到新轮启动那一刻——那是硬证据，round 本身就是新轮的启动时刻。
//   - stop：没有再起新一轮，但在线采样停了。采样只在进程活着且数据不陈旧时写，
//     所以最后一次采样就是"这个世界停止活动"的时刻。
//   - open：两样都没有，那就是真的还在里面，画到查询这一刻。
func (s *Store) closeOpenSpans(spans []TimelineSpan, until, lastSample time.Time, cluster string) {
	for i := range spans {
		sp := &spans[i]
		if sp.EndedBy != "" {
			continue
		}
		if next, ok := s.nextRoundStart(cluster, sp.Shard, sp.Round); ok && next.Before(until) {
			sp.To, sp.Open, sp.EndedBy = next, false, SpanEndRestart
			continue
		}
		if !lastSample.IsZero() && lastSample.Before(until) && lastSample.After(sp.From) {
			sp.To, sp.Open, sp.EndedBy = lastSample, false, SpanEndStop
			continue
		}
		sp.To, sp.Open, sp.EndedBy = until, true, SpanEndOpen
	}
}

// mergeOverlapSpans 把同一个人在时间上互相盖住的段落并成一整段。
//
// 为什么会有两段盖在一起：一个人同一刻只可能"在服里"或"不在"，两段在场说的是同一件事，
// 只会是同一份在场被记了两遍——最常见的是上一轮的收口时刻晚于下一轮的启动时刻
// （离服行落在两轮之间，两轮各记一次），偶发的是两个世界各写一份而抄本没合掉。
// 不并的话有两处一起错：那句"一共在线多久"把重叠的那截加了两遍，
// 图上同一个人的轨道里出现两根互相压住的条（看着像两个人）。
// 并段只动重叠的部分，隔开的两次到场照旧是两根条。
// 收尾说法取结束更晚的那一段——那才是这段在场真正的结束方式；
// 只要有一段是被窗口切开的，并完必须还带着那个标记，否则图上就把"起点未知"画成了"确有其事"。
func mergeOverlapSpans(spans []TimelineSpan) []TimelineSpan {
	if len(spans) < 2 {
		return spans
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].From.Before(spans[j].From) })
	out := []TimelineSpan{spans[0]}
	for _, next := range spans[1:] {
		cur := &out[len(out)-1]
		if !next.From.Before(cur.To) {
			out = append(out, next)
			continue
		}
		cur.ClippedStart = cur.ClippedStart || next.ClippedStart
		if next.To.After(cur.To) {
			cur.To, cur.EndedBy, cur.Open = next.To, next.EndedBy, next.Open
			cur.Shard, cur.Round = next.Shard, next.Round
		}
	}
	return out
}

// nextRoundStart 找这个分片在 round 之后起的那一轮的启动时刻。
// round 存的就是那一轮的启动 UTC 毫秒，所以查到一个更大的 round 就等于查到了时刻。
func (s *Store) nextRoundStart(cluster, shard string, round int64) (time.Time, bool) {
	if shard == "" || round <= 0 {
		return time.Time{}, false
	}
	var next sql.NullInt64
	err := s.db.QueryRow(`SELECT MIN(round) FROM player_events
		WHERE cluster = ? AND shard = ? AND round > ?`, cluster, shard, round).Scan(&next)
	if err != nil || !next.Valid || next.Int64 <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(next.Int64).UTC(), true
}

// PurgePlayerEvents 按保留期裁掉过期的行为行。
func (s *Store) PurgePlayerEvents(before time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM player_events WHERE at < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("清理玩家行为记录: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("清理玩家行为记录后取行数: %w", err)
	}
	return int(n), nil
}

// PlayerEventSpan 是这个房间攒了多少条行为、最早最晚各是什么时刻。
// 接口的诚实说明（"从什么时候开始攒、这段时间一条都没有"）取它。
type PlayerEventSpan struct {
	Rows int
	From time.Time
	To   time.Time
}

func (s *Store) PlayerEventSpan(cluster string) (PlayerEventSpan, error) {
	var out PlayerEventSpan
	var (
		rows   int
		fromMs int64
		toMs   int64
	)
	err := s.db.QueryRow(`SELECT COUNT(*), IFNULL(MIN(at), 0), IFNULL(MAX(at), 0)
		FROM player_events WHERE cluster = ?`, cluster).Scan(&rows, &fromMs, &toMs)
	if err != nil {
		return out, fmt.Errorf("统计玩家行为条数: %w", err)
	}
	out.Rows = rows
	if fromMs > 0 {
		out.From = time.UnixMilli(fromMs).UTC()
	}
	if toMs > 0 {
		out.To = time.UnixMilli(toMs).UTC()
	}
	return out, nil
}
