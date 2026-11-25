// 玩家行为时间线：把"谁进来了、谁死了、谁说了话"从两份日志里挑出来，按人归拢。
//
// 为什么在 gamelog 里做而不是在前端做：分组要同时看 server 和 chat 两份文件，
// 而同一条公告在两份里各写了一遍（引擎把公告既打进服务端日志，也打进聊天日志）。
// 这件事只有一个地方能判准——就是知道自己在打标签的那一层。
// 放前端的话，"死亡 3 次"里有一半其实是同一次的两份抄本。
//
// 为什么单独一个接口而不是日志页的筛选：日志页一次只看一个世界的一份文件，
// 而"这个人今天来过几次、死了几回"问的是这一轮的整段时间，
// 得两份文件一起数才成立。
//
// 时间线只回看窗口以内的事实，不做累计：面板没有把每一下都落库（那要一张新表、
// 一套清理策略，还要处理引擎每轮启动截断重写这两份文件），而"最近 N 行里发生过什么"
// 这句话在界面上说得出依据，累计数字说不出口。
package gamelog

import (
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TimelineDefaultLines 是时间线每个文件默认回看的行数。
// 比日志页的 200 大一档是有原因的：日志页要"看得过来"，时间线要"数得准"，
// 后者每多回看一行就多一分把握，而它只给汇总不给整屏原文。
const TimelineDefaultLines = 500

// RowsPerPlayer 是一个人最多列出多少行行为。芯片的数字始终按全部行算，
// 只有下面那份明细折到最近这么多行，并把自己折了多少写在 eventsHidden 里。
//
// 为什么要有这一档：窗口能开到每份文件 2000 行，一个话痨在 16 人服里跑一轮
// 能占掉几百行，而这一页是给人扫的——一个人占三屏的话，第二个人就看不见了。
// 取 40 行：一屏大约放得下十几行，40 行是"往上翻两屏就到这一轮的头"，
// 再多就不如直接点芯片跳去逐行日志看原文（那边有筛选和跟随）。
const RowsPerPlayer = 40

// behaviourOrder 决定"这个人做过哪些事"在界面上的顺序，
// 同时也是"哪些行算玩家行为"的唯一依据——筛谁和数谁共用一张表，
// 免得出现"事件字典里有它、时间线里永远没有它"。
var behaviourOrder = []string{
	EventAuthOK, EventPlayerJoin, EventPlayerLeave,
	EventPlayerDeath, EventGhostGone, EventResurrect,
	EventAFKStart, EventAFKEnd,
	EventKick, EventBan,
	EventChat,
}

// TimelineEvent 是时间线上的一格。
type TimelineEvent struct {
	// Offset 让界面能一句话指回日志页那一行（同一份文件里它是唯一的）。
	Offset    int64  `json:"offset"`
	Clock     string `json:"clock,omitempty"`
	At        string `json:"at,omitempty"`
	Kind      string `json:"kind"`
	KindLabel string `json:"kindLabel"`
	Shard     string `json:"shard"`
	Event     string `json:"event"`
	// Name 是短名（行前缀用），Label 是整句（选中才展开）。
	Name   string `json:"name"`
	Label  string `json:"label,omitempty"`
	Player string `json:"player,omitempty"`
	// Target 只在"一个人对另一个人做了点什么"的行里有值（被踢的、被封的那位）。
	Target string `json:"target,omitempty"`
	UserID string `json:"userid,omitempty"`
	Raw    string `json:"raw"`
}

// TimelineCount 是某个人对某一件行为的计数。中文名跟着一起给：
// 前端自己摆一张翻译表的话，它迟早和规则表漂移成两种说法。
type TimelineCount struct {
	Code  string `json:"code"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// Tone 是界面上的颜色档位（warn=情况变坏、bad=被人处置、ok=好转、空=中性）。
	// 词汇表在这一层，不在前端：事件码是这里定义的。
	Tone string `json:"tone,omitempty"`
	N    int    `json:"n"`
}

// behaviourTone 决定哪一记 chip 该带颜色。"死亡 2"和"进服 2"长一个样的话，
// 一排芯片里看不出该处理什么；中性那几档留白，是为了让带色的真的显眼。
var behaviourTone = map[string]string{
	EventPlayerDeath: "warn", EventGhostGone: "warn", EventAFKStart: "warn",
	EventKick: "bad", EventBan: "bad",
	EventResurrect: "ok", EventAFKEnd: "ok",
}

// TimelinePlayer 是"这一轮里这个人做过什么"。
type TimelinePlayer struct {
	// Key 是分组用的显示名。引擎的死亡/进出公告里只有名字没有账户编号，
	// 所以名字是唯一的公共键；账户编号是从聊天行里凑到的，凑不到就留空。
	Key       string          `json:"key"`
	UserID    string          `json:"userid,omitempty"`
	Counts    []TimelineCount `json:"counts"`
	LastAt    string          `json:"lastAt,omitempty"`
	LastClock string          `json:"lastClock,omitempty"`
	Events    []TimelineEvent `json:"events"`
	// EventsHidden 是这个人被折掉的更早行数（只折明细，计数照旧算它）。
	// 界面必须把它说出来：一份只列了最近若干行的列表看着像"他就做了这些"。
	EventsHidden int `json:"eventsHidden,omitempty"`
}

// Timeline 是一次汇总的结果。
type Timeline struct {
	Cluster string `json:"cluster"`
	Shard   string `json:"shard"`
	Window  int    `json:"window"`
	// RoundStart 是这一轮的启动时刻（服务端日志里那行 Current time）。
	// 界面那句"这一轮谁做了什么"原本只说得出"这一轮"，有了它才说得出从几点开始；
	// 日志被手工拷来、没有那行时留空，界面也就不写这一档。
	RoundStart string `json:"roundStart,omitempty"`
	Scanned    int    `json:"scanned"`
	// Kept 是归到人头上的条数，Dupled 是两份文件里同一下的抄本，
	// NoOwner 是认不出是谁的行为行。三者加起来等于窗口内的行为行总数，
	// 界面那句"共 N 下"就是从这里来的，缺一项都会让它和原文对不上。
	Kept    int              `json:"kept"`
	Dupled  int              `json:"dupled"`
	NoOwner int              `json:"noOwner"`
	Older   bool             `json:"older"`
	Players []TimelinePlayer `json:"players"`
	Note    string           `json:"note,omitempty"`

	// 这两份是装配过程的临时状态，不进响应：seen 用来去掉两份文件里的同一条公告，
	// rows 是所有行为行的扁平表，finish 之后再按人分。
	seen map[string]struct{}
	rows []TimelineEvent
}

// Timeline 读一个分片这一轮的两份日志，按人归拢行为。
//
// 两份文件都可以缺（世界从没启动过就两份都缺；只跑了一轮时聊天文件可能还没写出内容），
// 但两份都读不到才当错误往上抛——那时界面该说的是"还没有日志文件"，
// 而不是给出一张谁都没来过的空表。
func (r *Reader) Timeline(cluster, shard string, lines int) (*Timeline, error) {
	if lines <= 0 {
		lines = TimelineDefaultLines
	}
	if lines > MaxLines {
		lines = MaxLines
	}
	t := &Timeline{
		Cluster: cluster, Shard: shard, Window: lines,
		Players: []TimelinePlayer{}, seen: map[string]struct{}{},
	}
	// 本轮启动时刻只有服务端日志里那一行 Current time 给得出来，聊天文件从头到尾
	// 没有这一行，所以它读出来的每行都没有墙上时间。引擎每次启动都会把这两份重写，
	// 两份就是同一轮，借过来用是成立的；不借的话同一个人的行里就是两种时间写法
	// （22:59:10 挨着 [00:01:00]），一眼看不出谁先谁后。
	var roundStart time.Time
	var got, miss int
	for _, kind := range []Kind{KindServer, KindChat} {
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
		t.Scanned += len(snap.Entries)
		t.Older = t.Older || snap.Older
		t.collect(kind, shard, snap)
	}
	if got == 0 {
		return nil, os.ErrNotExist
	}
	if !roundStart.IsZero() {
		t.RoundStart = roundStart.UTC().Format(time.RFC3339)
	}
	if miss > 0 {
		t.Note = "有一份日志还没写出来，上面只数了读到的那一份；世界跑过一轮之后两份都会齐"
	}
	if t.Older {
		t.Note = moreNote(t.Note, "只回看了每份文件最近 "+strconv.Itoa(lines)+" 行，更早的行为没算进来")
	}
	t.finish()
	return t, nil
}

// collect 收下这份文件里的行为行。去重键是 (时钟, 事件, 原文)：
// 同一条公告在服务端日志里是 `小满 has left the game`，在聊天日志里是
// `[Announcement] 小满 has left the game`，方括号标签被拆走之后两处正文一模一样，
// 而时钟出自同一个进程，所以这个键能准确认出"这就是同一下"。
// 反过来，两次一模一样的死亡因为时钟不同，不会互相吃掉。
func (t *Timeline) collect(kind Kind, shard string, snap *Snapshot) {
	for _, e := range snap.Entries {
		if !isBehaviour(e.Event) {
			continue
		}
		key := e.Clock + "\x00" + e.Event + "\x00" + e.Raw
		if _, ok := t.seen[key]; ok {
			t.Dupled++
			continue
		}
		t.seen[key] = struct{}{}
		t.rows = append(t.rows, TimelineEvent{
			Offset: e.Offset, Clock: e.Clock, At: e.At,
			Kind: string(kind), KindLabel: kind.Label(), Shard: shard,
			Event: e.Event, Name: eventShortName(e.Event), Label: e.EventLabel,
			Player: strings.TrimSpace(e.Player), Target: e.Target, UserID: e.UserID, Raw: e.Raw,
		})
	}
}

// finish 把散行按人归拢、各自按时间排好。
//
// 归人用两趟：第一趟先找出"账户编号↔显示名"的对应关系，第二趟才分组。
// 必须分两趟是因为这两种身份不是每行都同时出现——进服的验证行只带编号，
// 死亡公告只带名字，聊天行两个都带。先分组再合并的话，同一个人会因为
// "哪一行先到"而裂成两条，而时间线上最不该出现的就是把一个人数成两个。
//
// 组内排序用引擎时钟换算的秒数（同一份文件里它单调递增），换算不出来的排到最后：
// 那些行没有可比较的时间，插进中间会把整个人的顺序带歪。
// 人与人之间的顺序按"各自最后一次行为"倒序，最近活跃的排最前。
func (t *Timeline) finish() {
	// 第一趟：只信"同一行里同时给了名字和编号"的那几条对应关系。
	linked := map[string]string{}
	for _, ev := range t.rows {
		if ev.UserID == "" || ev.Player == "" {
			continue
		}
		if _, ok := linked[ev.UserID]; !ok {
			linked[ev.UserID] = ev.Player
		}
	}

	keyOf := func(ev TimelineEvent) string {
		switch {
		case ev.Player != "":
			return ev.Player
		case ev.UserID == "":
			return ""
		case linked[ev.UserID] != "":
			return linked[ev.UserID]
		default:
			// 只认识编号、这一轮里这人一次没说过话也没进过公告行——编号本身是公开信息
			// （服务器列表里就查得到），拿它当标题比编一个"陌生玩家"更诚实。
			return ev.UserID
		}
	}

	by := map[string]*TimelinePlayer{}
	var order []string
	for _, ev := range t.rows {
		key := keyOf(ev)
		if key == "" {
			// 认不出是谁的行不进任何人的时间线，但要记进 NoOwner：
			// 静默丢掉的话，界面上的"共 N 下"就和原文对不上了。
			t.NoOwner++
			continue
		}
		t.Kept++
		p := by[key]
		if p == nil {
			p = &TimelinePlayer{Key: key, Counts: []TimelineCount{}, Events: []TimelineEvent{}}
			by[key] = p
			order = append(order, key)
		}
		if p.UserID == "" && ev.UserID != "" {
			p.UserID = ev.UserID
		}
		p.Events = append(p.Events, ev)
	}

	out := make([]TimelinePlayer, 0, len(order))
	for _, name := range order {
		p := by[name]
		sort.SliceStable(p.Events, func(i, j int) bool { return clockLess(p.Events[i], p.Events[j]) })
		// 先按全量算计数，再折明细：芯片上那个数字是"这一轮他做了几次"，
		// 不能因为界面只列最近几行就跟着变小。
		p.Counts = countEvents(p.Events)
		if n := len(p.Events); n > RowsPerPlayer {
			p.EventsHidden = n - RowsPerPlayer
			p.Events = p.Events[n-RowsPerPlayer:]
		}
		if n := len(p.Events); n > 0 {
			p.LastAt = p.Events[n-1].At
			p.LastClock = p.Events[n-1].Clock
		}
		out = append(out, *p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, okA := lastSecs(out[i])
		b, okB := lastSecs(out[j])
		if okA != okB {
			return okA
		}
		if okA && a != b {
			return a > b
		}
		return out[i].Key < out[j].Key
	})
	t.Players = out
}

// countEvents 按 behaviourOrder 的固定顺序给出计数，零次的项不出现：
// 一排"死亡 0 · 复活 0"会把真正发生过的事挤到看不见。
func countEvents(events []TimelineEvent) []TimelineCount {
	n := map[string]int{}
	for _, e := range events {
		n[e.Event]++
	}
	out := make([]TimelineCount, 0, len(behaviourOrder))
	for _, code := range behaviourOrder {
		if n[code] == 0 {
			continue
		}
		out = append(out, TimelineCount{Code: code, Name: eventShortName(code), Label: eventFullLabel(code), Tone: behaviourTone[code], N: n[code]})
	}
	return out
}

func isBehaviour(code string) bool {
	for _, c := range behaviourOrder {
		if c == code {
			return true
		}
	}
	return false
}

// eventShortName 取界面上的短名。规则表里没这个名字时退回整句，再退回码值：
// 时间线宁可写长一点，也不能露出 player_death 这种码值（这是面板的硬规矩）。
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
// 这里取第一条说得出话的，字典和逐行前缀才不会是两种说法。
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

func clockLess(a, b TimelineEvent) bool {
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

func lastSecs(p TimelinePlayer) (int64, bool) {
	for i := len(p.Events) - 1; i >= 0; i-- {
		if s, ok := ClockSeconds(p.Events[i].Clock); ok {
			return s, true
		}
	}
	return 0, false
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
