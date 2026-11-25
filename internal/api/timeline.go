// 玩家在线时间线接口：把库里攒下来的进出与死亡行归成"每个人一段段在服区间"。
//
// 归并算法在 store 里（那里是唯一一处说"他在线多久"的地方），这一层只做三件事：
// 校验参数、把码值翻成中文、把"这份数据新不新、从什么时候开始攒"说清楚。
//
// 为什么界面上必须有那句"从什么时候开始攒"：归档是这一版才加的能力，
// 早于它的历史真的不存在。不说这一句，一张只有今天下午的图会被读成
// "这房间一直就这么点人"，那是图上最容易被当成事实的假话。
package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"dstpanel/internal/gamelog"
	"dstpanel/internal/store"
)

// spanView 是一段在服区间。
type spanView struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	// ReasonLabel 说清这一段为什么在这儿收尾。四种说法都来自 store 的判定，
	// 前端不参与推断——它一猜就会出现"界面说他在里面、库里说他早走了"。
	ReasonLabel  string `json:"reasonLabel"`
	ClippedStart bool   `json:"clippedStart,omitempty"`
}

type markView struct {
	At    string `json:"at"`
	Event string `json:"event"`
	// Label 是中文短名，取自 gamelog 那份行为目录（和事件页同一个来源）。
	Label string `json:"label"`
}

// eventCountView 是「行为构成」那一行。短名和整句都从行为目录取：
// 行标签用短名，悬停展开整句——事件页的筛选项用的是同一份名字，两处不许各写一套。
type eventCountView struct {
	Code  string `json:"code"`
	Name  string `json:"name"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type personView struct {
	UserID string     `json:"userid,omitempty"`
	Name   string     `json:"name"`
	Spans  []spanView `json:"spans"`
	Marks  []markView `json:"marks"`
	// Seconds 是各段时长相加，界面上"这段时间一共在线 1 小时 12 分"就是它。
	Seconds   int64  `json:"seconds"`
	OnlineNow bool   `json:"onlineNow"`
	LastAt    string `json:"lastAt"`

	// last 只是算 LastAt 时用的中间量：这一段一段取最大值，不用前端再扫一遍。
	last time.Time
}

type timelinePage struct {
	Cluster string       `json:"cluster"`
	Hours   int          `json:"hours"`
	From    string       `json:"from"`
	To      string       `json:"to"`
	People  []personView `json:"people"`
	Rows    int          `json:"rows"`
	// Events 是这一窗里各类行为各发生几次（次数多的排前面），分母就是 Rows。
	Events []eventCountView `json:"events"`
	// RecordFrom 是这个房间在库里最早的一条行为（不受窗口限制）。
	RecordFrom string `json:"recordFrom,omitempty"`
	Truncated  bool   `json:"truncated"`
	Note       string `json:"note"`
}

// spanReasonLabels 是四种收尾说法。取值和 store 的 SpanEnd* 常量一一对应，
// 少一条就退回码值本身——那会让界面上冒出 restart 这种英文，所以宁可写长也不能漏。
var spanReasonLabels = map[string]string{
	store.SpanEndLeave:   "他自己离开了世界",
	store.SpanEndRestart: "世界在这之后起过新一轮，上一轮的日志到此为止",
	store.SpanEndStop:    "世界在这之前停了，最后停在最后一次可信采样",
	store.SpanEndOpen:    "日志里还没有他离开的记录，他这会儿应该还在里面",
}

// behaviourNames 从行为目录里取「码值 → 短名」和「码值 → 整句」两张表。
// 只在这一处取：记号上的中文、行为构成那一行的标签和悬停，用的都是它。
func behaviourNames() (short, full map[string]string) {
	short, full = map[string]string{}, map[string]string{}
	for _, it := range gamelog.BehaviourCatalog() {
		short[it.Code] = it.Name
		full[it.Code] = it.Label
	}
	return short, full
}

func (s *Server) handlePlayerTimeline(w http.ResponseWriter, r *http.Request) {
	cluster := r.URL.Query().Get("cluster")
	if cluster == "" {
		writeErr(w, http.StatusBadRequest, "必须指定 cluster")
		return
	}
	// 和趋势图同一个理由：这个接口认参数不认路径，得单独判一次可见范围。
	if !s.denyInvisibleRoom(w, r, cluster) {
		return
	}
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 24*90 {
			writeErr(w, http.StatusBadRequest, "hours 得是 1~2160 之间的整数")
			return
		}
		hours = n
	}
	until := time.Now().UTC()
	since := until.Add(-time.Duration(hours) * time.Hour)

	tl, err := s.Store.PlayerTimeline(cluster, since, until)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	names, fulls := behaviourNames()
	out := timelinePage{
		Cluster:   cluster,
		Hours:     hours,
		From:      since.Format(time.RFC3339),
		To:        until.Format(time.RFC3339),
		People:    make([]personView, 0, len(tl.People)),
		Rows:      tl.Rows,
		Events:    make([]eventCountView, 0, len(tl.EventCounts)),
		Truncated: tl.Truncated,
	}
	for _, ec := range tl.EventCounts {
		name, ok := names[ec.Event]
		if !ok {
			name = ec.Event
		}
		label, ok := fulls[ec.Event]
		if !ok {
			label = name
		}
		out.Events = append(out.Events, eventCountView{Code: ec.Event, Name: name, Label: label, Count: ec.Count})
	}
	if !tl.RecordFrom.IsZero() {
		out.RecordFrom = tl.RecordFrom.Format(time.RFC3339)
	}
	anon, people := 0, len(tl.People)
	for _, p := range tl.People {
		if p.UserID == "" {
			anon++
		}
		v := personView{UserID: p.UserID, Name: p.Name, Seconds: p.Seconds, OnlineNow: p.OnlineNow}
		v.Spans = make([]spanView, 0, len(p.Spans))
		for _, sp := range p.Spans {
			label, ok := spanReasonLabels[sp.EndedBy]
			if !ok {
				label = sp.EndedBy
			}
			v.Spans = append(v.Spans, spanView{
				From: sp.From.Format(time.RFC3339), To: sp.To.Format(time.RFC3339),
				Reason: sp.EndedBy, ReasonLabel: label, ClippedStart: sp.ClippedStart,
			})
			if sp.To.After(v.last) {
				v.last = sp.To
			}
		}
		v.Marks = make([]markView, 0, len(p.Marks))
		for _, m := range p.Marks {
			label, ok := names[m.Event]
			if !ok {
				label = m.Event
			}
			v.Marks = append(v.Marks, markView{At: m.At.Format(time.RFC3339), Event: m.Event, Label: label})
			if m.At.After(v.last) {
				v.last = m.At
			}
		}
		if v.last.IsZero() {
			v.last = since
		}
		v.LastAt = v.last.Format(time.RFC3339)
		out.People = append(out.People, v)
	}
	out.Note = timelineNote(hours, tl, anon, people)
	writeOK(w, out)
}

// timelineNote 是这张图唯一一句"怎么读"。四件事必须说全：
// 窗口里读到多少、记录从什么时候开始攒、世界间迁移怎么算、只有显示名时改名怎么算。
//
// 这一句里不带钟点：它是整句拼好上屏的，前端没法再把里面那个时刻换成浏览器所在时区，
// 而面板上别的时间（趋势轴、段落起止、记录起点）都是本地钟。同一屏里差八小时的话，
// 用户没有任何线索能发现哪个才对——和 assignedText 当初那个坑同源。
// "记录从什么时候攒起"由界面上那枚「记录从…」单独说，它走 fmt 的本地化。
func timelineNote(hours int, tl store.Timeline, anon, people int) string {
	base := "一段横条是「这个人在世界里」，条上的记号是死亡与复活；" +
		"离开后一分钟内又进来的按世界间迁移处理，合成同一段（洞穴和地表之间来回就是这样，他其实没出去过）；" +
		"同一条公告被两个世界各写一遍的，按一次算。"
	if tl.Rows == 0 {
		if tl.RecordFrom.IsZero() {
			return fmt.Sprintf("这个房间一条玩家行为都还没攒下。面板是从这一版起才开始把进出记进库的"+
				"（在线的世界下一轮采样就会写入），所以更早的历史真的不存在，不是面板漏读。%s", base)
		}
		return fmt.Sprintf("过去 %d 小时里没有玩家行为；这个房间最早的记录比这一窗更早，"+
			"时刻写在上面那枚「记录从」里。%s", hours, base)
	}
	// 这里的单位是「次」不是「行」：归并那层把两个世界的重复公告合成一次，
	// 界面上这个数要和图上的段与记号数得上来。
	head := fmt.Sprintf("过去 %d 小时里读到 %d 次玩家行为，认出 %d 个人；库里最早那条的时刻写在上面那枚「记录从」里。",
		hours, tl.Rows, len(tl.People))
	if tl.Truncated {
		head += fmt.Sprintf("这一窗太挤，读到 %d 行就停了，后面的人没算进来，换个短一点的窗口。", store.TimelineRowLimit)
	}
	// "按名字认人"这件事要说得准：面板会拿花名册把公告里的显示名接上账户编号，
	// 接得上的人改名不会裂成两条。所以只有真接不上的那几个才需要提醒，
	// 全接不上和只有一部分接不上也不能说成同一句话——前者是这一轮没读到账号，
	// 后者才是那几个人的公告本身不带号。
	switch {
	case anon == people && people > 0:
		head += "这些人目前都只按显示名认（引擎的进出公告只带名字，花名册里也没对上账户编号），同一个人改了名会被当成两个人。"
	case anon > 0:
		head += fmt.Sprintf("其中有 %d 个人只带显示名（账户编号没在花名册里对上），他们改了名会被当成两个人。", anon)
	}
	return head + base
}
