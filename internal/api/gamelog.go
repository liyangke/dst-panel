// 游戏日志接口：一次读一屏（JSON）和一直跟着读（SSE）。
//
// 两个接口共用同一套解析与筛选，区别只在"读完就回"还是"读完继续盯"。
// 之所以不合成一个"传 follow=true"的接口：JSON 响应和 event-stream 的
// 错误语义完全不同（前者一个 404 就完事，后者要先把头写完才可能出错），
// 混在一个 handler 里必然写成两套分支还看不出。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"dstpanel/internal/gamelog"
)

// SSE 轮询间隔的边界。下限防有人拿 10ms 把 4h4g 的小机敲盘敲满，
// 上限防"看着像卡住了"——超过 10 秒的间隔已经不如刷新一次页面。
const (
	logPollDefaultMs = 1000
	logPollMinMs     = 200
	logPollMaxMs     = 10000
	// logKeepaliveEvery 是每隔多少次轮询发一条 SSE 注释帧。
	// 反代（nginx 默认 60 秒）会把长时间静默的连接当死连接掐掉，注释帧不占前端带宽。
	logKeepaliveEvery = 20
	// logRetryMs 写进 SSE 的 retry 帧：告诉浏览器断线后隔多久自己重连。
	// 重连时浏览器会自动带上最后收到的 id（Last-Event-ID），handler 拿它当 offset 续读。
	logRetryMs = 3000
)

type logQuery struct {
	kind   gamelog.Kind
	lines  int
	level  string
	event  string
	text   string
	pollMs int
	once   bool
}

// parseLogQuery 解析日志类接口的公共参数。
// 任何一项写错都直接写回 400 并返回 false：宁可让用户改一次，
// 也不要"参数打错了但页面看起来正常"，那种情况下用户会以为筛选生效了。
func (s *Server) parseLogQuery(w http.ResponseWriter, r *http.Request) (logQuery, bool) {
	q := logQuery{kind: gamelog.KindServer, lines: gamelog.DefaultLines, pollMs: logPollDefaultMs}
	raw := r.URL.Query()
	if v := strings.TrimSpace(raw.Get("kind")); v != "" {
		k, ok := gamelog.ParseKind(v)
		if !ok {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("kind 只能是 %s", strings.Join(kindCodes(), " 或 ")))
			return q, false
		}
		q.kind = k
	}
	if v := strings.TrimSpace(raw.Get("lines")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > gamelog.MaxLines {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("lines 得是 1~%d 之间的整数", gamelog.MaxLines))
			return q, false
		}
		q.lines = n
	}
	if v := strings.TrimSpace(raw.Get("level")); v != "" {
		if !contains(levelCodes(), v) {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("level 只能是 %s", strings.Join(levelCodes(), " 或 ")))
			return q, false
		}
		q.level = v
	}
	q.event = strings.TrimSpace(raw.Get("event"))
	// 事件码也校验：认不得的事件筛出来是空列表，而空列表在界面上和
	// "这台服务器确实没发生过这件事"长得一模一样，必须当场拒绝。
	if q.event != "" && !contains(eventCodes(), q.event) {
		writeErr(w, http.StatusBadRequest, "没有这个事件码，可调 /api/v1/log-events 看可选值")
		return q, false
	}
	q.text = strings.ToLower(strings.TrimSpace(raw.Get("q")))
	if v := strings.TrimSpace(raw.Get("pollMs")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < logPollMinMs || n > logPollMaxMs {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("pollMs 得是 %d~%d 之间的整数", logPollMinMs, logPollMaxMs))
			return q, false
		}
		q.pollMs = n
	}
	q.once = truthy(raw.Get("once"))
	return q, true
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

func kindCodes() []string {
	out := make([]string, 0, 2)
	for _, k := range gamelog.KindCatalog() {
		out = append(out, k.Code)
	}
	return out
}

func levelCodes() []string {
	out := make([]string, 0, 3)
	for _, l := range gamelog.LevelCatalog() {
		out = append(out, l.Code)
	}
	return out
}

// eventCodes 里补上 plain（"没被任何规则命中"）：它是合法筛选项，
// 但规则表里没有对应的说明文字，所以不会自动出现在 catalog 里。
func eventCodes() []string {
	out := make([]string, 0, 40)
	for _, ev := range gamelog.EventCatalog() {
		out = append(out, ev.Code)
	}
	return append(out, gamelog.EventPlain)
}

// keep 判断一条记录要不要出现在结果里。三个条件是"与"：
// 级别是下限（选警告就同时给报错），事件和关键词是精确/包含。
func (q logQuery) keep(e gamelog.Entry) bool {
	if q.level != "" && gamelog.LevelRank(e.Level) < gamelog.LevelRank(q.level) {
		return false
	}
	if q.event != "" && e.Event != q.event {
		return false
	}
	if q.text != "" && !strings.Contains(strings.ToLower(e.Raw+" "+e.Player+" "+e.UserID), q.text) {
		return false
	}
	return true
}

type logView struct {
	Cluster   string `json:"cluster"`
	Shard     string `json:"shard"`
	Kind      string `json:"kind"`
	KindLabel string `json:"kindLabel"`
	Path      string `json:"path"`

	SizeBytes int64  `json:"sizeBytes"`
	ModTime   string `json:"modTime"`
	// StartAt 是本轮启动的墙上时间；解不出来时为空，并配一句 StartNote 说明为什么。
	StartAt   string `json:"startAt,omitempty"`
	StartNote string `json:"startNote,omitempty"`
	// GameVersion 是这份日志所属的服务端版本：出问题时"哪一版写的日志"必须能当场看到。
	GameVersion string `json:"gameVersion,omitempty"`

	Window   int `json:"window"`
	Scanned  int `json:"scanned"`
	Returned int `json:"returned"`
	Blanks   int `json:"blanks"`
	// Older 为 true 表示窗口之前还有更早的内容没给出去（界面该提示"只回看了最近 N 行"）。
	Older bool  `json:"older"`
	Next  int64 `json:"next"`
	// StartOffset 是这一屏（或这条流）开始续读的字节位置。
	// 普通 /log 接口里它等于 Next（下一次续读的位置）；SSE 的 meta 帧会把它
	// 覆盖成"本次流实际从哪开始"，前端断线重连就知道该把哪个 offset 带回来。
	StartOffset int64 `json:"startOffset"`

	// Entries 按文件里的顺序（旧→新），前端追加显示不用自己反转。
	Entries []gamelog.Entry `json:"entries"`
	Note    string          `json:"note,omitempty"`
}

// logViewOf 把一次读取的结果变成响应。筛选是在解析完之后做的，
// 所以 scanned 和 returned 会不同——界面上必须把这两个数都给出，
// 否则"筛完只剩 2 条"会被读成"这服务器只发生过 2 件事"。
func logViewOf(cluster, shard string, snap *gamelog.Snapshot, q logQuery) logView {
	v := logView{
		Cluster: cluster, Shard: shard,
		Kind: string(q.kind), KindLabel: q.kind.Label(), Path: snap.Path,
		SizeBytes: snap.Size, ModTime: snap.ModTime.UTC().Format(time.RFC3339),
		GameVersion: snap.Version,
		Window:      q.lines, Scanned: len(snap.Entries), Blanks: snap.Blanks,
		Older: snap.Older, Next: snap.Next, StartOffset: snap.Next,
		Entries: make([]gamelog.Entry, 0, len(snap.Entries)),
	}
	if !snap.Start.IsZero() {
		v.StartAt = snap.Start.UTC().Format(time.RFC3339)
	} else {
		v.StartNote = "这份日志开头没有 Current time 行，只有引擎的相对时钟，所以换算不出墙上时间"
	}
	for _, e := range snap.Entries {
		if q.keep(e) {
			v.Entries = append(v.Entries, e)
		}
	}
	v.Returned = len(v.Entries)
	if v.Older {
		v.Note = fmt.Sprintf("只回看了最近 %d 行，更早的内容没读；要查历史请直接在服务端上 grep 这份文件", q.lines)
	}
	return v
}

func (s *Server) handleLogShow(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	q, ok := s.parseLogQuery(w, r)
	if !ok {
		return
	}
	snap, err := gamelog.NewReader(s.Paths).Tail(cluster, shard, q.kind, q.lines)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 503 而不是 404：文件还没写出来是分片没启动过的常态，
			// 报 404 会让前端把它当"地址写错了"，也不去重试。
			writeErr(w, http.StatusServiceUnavailable,
				"这个世界还没有"+q.kind.Label()+"文件：世界可能从没启动过，或者面板指错了存档目录")
			return
		}
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, logViewOf(cluster, shard, snap, q))
}

// sse 把响应写成 event-stream。
//
// 前端要用 fetch + ReadableStream 读它而不是 EventSource：面板的认证是 HTTP Basic，
// 而 EventSource 不能设请求头（它只会带上浏览器缓存的那份凭据），
// fetch 可以，而且 SSE 的帧格式照样好解。
type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// frame 写一帧，带 id。id 取"这一行在文件里的字节位置(offset)"：SSE 规范里 id 就是
// 断线续读的锚，浏览器重连会自动把它放进 Last-Event-ID，服务端拿它当 offset
// 就能从断掉的那一行接着发——不必在面板里存一张"谁读到哪了"的连接表，
// 前端也不用自己猜重连时该从哪继续。轮转时 offset 归零由 reset 帧通知，
// 那是新的一轮，id 从 0 重新单调往上走是预期的。
func (e *sse) frame(id int64, event string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(e.w, "id: %d\nevent: %s\ndata: %s\n\n", id, event, raw); err != nil {
		return err
	}
	// 每条都 flush：SSE 的意义就是"这会儿就送到"，攒在缓冲区里等于没有。
	return e.rc.Flush()
}

// firstFrame 把 retry 字段并进第一帧，而不是单独发一帧 "retry: N"。
// SSE 允许同一帧同时带 retry / id / event / data，浏览器照样把 retry 应用到整条
// 连接；单独发一帧时它没有 event/data，按帧解析的客户端会把它当成噪声或坏帧，
// 也拿不到"从哪个 offset 开始续"的上下文。
func (e *sse) firstFrame(id int64, event string, v any, retryMs int) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(e.w, "retry: %d\nid: %d\nevent: %s\ndata: %s\n\n",
		retryMs, id, event, raw); err != nil {
		return err
	}
	return e.rc.Flush()
}

func (e *sse) comment(text string) error {
	if _, err := fmt.Fprintf(e.w, ": %s\n\n", text); err != nil {
		return err
	}
	return e.rc.Flush()
}

func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	q, ok := s.parseLogQuery(w, r)
	if !ok {
		return
	}
	reader := gamelog.NewReader(s.Paths)

	// Last-Event-ID 是浏览器（或前端 fetch 重连）带回来的"上次收到哪一行"。
	// 有它就从那个 offset 接着发、不重发 backlog；没有就照老样子先发一屏。
	// 非法值忽略而不是 400：这个头是浏览器自动加的，为它把整条流拒掉不值当。
	lastID, hasLast := parseLastEventID(r.Header.Get("Last-Event-ID"))

	// 先把 backlog 读出来再写流式头：这一步失败还是普通的 JSON 错误响应，
	// 反过来（头先写、读盘再失败）就只能靠 SSE 的 error 帧通知前端，而那时
	// 前端已经分不清"接口错了"和"日志出了新情况"。
	snap, err := reader.Tail(cluster, shard, q.kind, q.lines)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusServiceUnavailable,
				"这个世界还没有"+q.kind.Label()+"文件，世界启动并写出第一行之后再订阅")
			return
		}
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // 反代（nginx）默认会缓冲整块响应，这一句关掉本条的缓冲
	w.WriteHeader(http.StatusOK)

	e := &sse{w: w, rc: rc}
	// retry 随第一帧一起给：断线后浏览器按这个节奏重连，并把最后收到的 id 带回来。
	// 起始 offset：带 Last-Event-ID 就从那里续，否则从 backlog 末尾开始。
	startOffset := snap.Next
	if hasLast {
		startOffset = lastID
	}
	view := logViewOf(cluster, shard, snap, q)
	// meta 帧带的是"这一屏的上下文"：文件位置、本轮启动时间、窗口边界，
	// 再加起始 offset——前端断线重连才知道该把哪个位置放进 Last-Event-ID。
	view.Entries = nil
	view.StartOffset = startOffset
	if hasLast {
		// 带 Last-Event-ID 时不重发 backlog，meta 里的"返回条数"别留一个假数。
		view.Returned, view.Scanned = 0, 0
	}
	if err := e.firstFrame(startOffset, "meta", view, logRetryMs); err != nil {
		return
	}

	since := snap.Since()
	if hasLast {
		// 只带 offset 续读：Start 和文件身份都交给 ReadSince 自己从文件头认，
		// 不把上一轮的时间硬塞给可能已经被截断重写过的那一轮。
		since = gamelog.Since{Offset: startOffset}
	} else {
		for _, en := range snap.Entries {
			if q.keep(en) {
				if err := e.frame(en.Offset, "line", en); err != nil {
					return
				}
			}
		}
		if q.once {
			// once 是探针和"只看一眼"的用法：把 backlog 发完就收尾，
			// 不留下一个永远挂着的连接。
			_ = e.frame(snap.Next, "done", map[string]any{"returned": view.Returned, "next": snap.Next})
			return
		}
	}

	// once + Last-Event-ID：从指定位置读一次增量就收尾，同样不留挂着的连接。
	if hasLast && q.once {
		next, err := reader.ReadSince(cluster, shard, q.kind, since)
		if err != nil {
			_ = e.frame(startOffset, "error", map[string]any{"error": publicError(err)})
			return
		}
		if next.Restarted {
			_ = e.frame(0, "reset", resetFrame(next))
		}
		for _, en := range next.Entries {
			if q.keep(en) {
				if err := e.frame(en.Offset, "line", en); err != nil {
					return
				}
			}
		}
		_ = e.frame(next.Next, "done", map[string]any{"returned": len(next.Entries), "next": next.Next})
		return
	}

	ticker := time.NewTicker(time.Duration(q.pollMs) * time.Millisecond)
	defer ticker.Stop()
	idle := 0
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// 浏览器断开是这里唯一的退出信号：面板不留活跃连接的表，
			// 也不该留——一个 SSE 挂几小时是正常用法，记下来只会变成又一张要清的表。
			return
		case <-ticker.C:
			next, err := reader.ReadSince(cluster, shard, q.kind, since)
			if err != nil {
				// 文件被删/权限变了：把原因说清楚再断，前端才能决定是重连还是报错。
				_ = e.frame(since.Offset, "error", map[string]any{"error": publicError(err)})
				return
			}
			since = next.Since()
			if next.Restarted {
				// 引擎每轮启动会截断重写这两份日志，也可能把旧日志改名后新建同名文件
				//（见 gamelog/reader.go 的 FileID 判据）。不通知前端的话，它会把新一轮的
				// 第一行接在上一轮最后一行后面，时间线直接接错。reset 帧给出新的
				// 起始 offset=0 与文件身份，前端据此清空重来。
				_ = e.frame(0, "reset", resetFrame(next))
			}
			for _, en := range next.Entries {
				if q.keep(en) {
					if err := e.frame(en.Offset, "line", en); err != nil {
						return
					}
				}
			}
			if len(next.Entries) == 0 {
				idle++
				if idle >= logKeepaliveEvery {
					idle = 0
					if err := e.comment("keepalive"); err != nil {
						return
					}
				}
			} else {
				idle = 0
			}
		}
	}
}

// parseLastEventID 把 SSE 重连头解析成 offset。取不到或不像 offset 时返回 false，
// 让调用方退回"先发一屏 backlog"的老路径，而不是把一条正常的流拒掉。
func parseLastEventID(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// resetFrame 是轮转时那一帧的内容：新的起始时间、文件大小，以及前端该从哪个
// offset 重新开始（0）。fileId 一并给出，便于排查"到底是截断还是换了文件"。
func resetFrame(next *gamelog.Snapshot) map[string]any {
	return map[string]any{
		"startAt":     utcText(next.Start),
		"sizeBytes":   next.Size,
		"startOffset": int64(0),
		"fileId":      next.FileID,
	}
}

func utcText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// handleLogEvents 给日志页的三个下拉框：种类、级别、事件。
// 数据全在 gamelog 的规则表里，这里不手写第二份（同 statusView 借 modbridge 季节表的理由）。
func (s *Server) handleLogEvents(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{
		"kinds":  gamelog.KindCatalog(),
		"levels": gamelog.LevelCatalog(),
		"events": gamelog.EventCatalog(),
		// behaviours 是事件表"动作"下拉的选项。它和 events（日志页逐行筛选的字典）
		// 不是同一份：字典里那些引擎运行事件（启动、模组加载、存档落盘）
		// 在表格里没有对应的行，把它们列进动作下拉就是筛不出的选项。
		"behaviours": gamelog.BehaviourCatalog(),
		"limits": map[string]any{
			"defaultLines": gamelog.DefaultLines,
			"maxLines":     gamelog.MaxLines,
			"minPollMs":    logPollMinMs,
			"maxPollMs":    logPollMaxMs,
			// 事件表的默认窗口和每页行数也从这儿给：前端自己记一个数，
			// 后端改的那天，界面上"最近 500 行 / 每页 20 条"就成了假话。
			"eventDefaultLines": gamelog.EventDefaultLines,
			"eventDefaultSize":  gamelog.EventDefaultSize,
			"eventMaxSize":      gamelog.EventMaxSize,
		},
	})
}

// ---- 日志历史分页与导出 ----

// parseHistoryPaging 单独解析 offset/limit，不塞进 logQuery：
// lines 是"末尾窗口"的大小，offset/limit 是"从第几条开始、给几条"，两个接口
// 各用一套，混在一个结构里迟早互相覆盖（界面上"每页 20 条"和"最近 200 行"
// 本来就是两个不同的数）。
func (s *Server) parseHistoryPaging(w http.ResponseWriter, r *http.Request) (offset, limit int, ok bool) {
	raw := r.URL.Query()
	limit = gamelog.DefaultLines
	if v := strings.TrimSpace(raw.Get("offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "offset 得是从 0 开始的整数")
			return 0, 0, false
		}
		offset = n
	}
	if v := strings.TrimSpace(raw.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > gamelog.MaxLines {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("limit 得是 1~%d 之间的整数", gamelog.MaxLines))
			return 0, 0, false
		}
		limit = n
	}
	return offset, limit, true
}

// historyFilterOf 把 /log 那套筛选条件原样搬到历史接口：level 下限、event 精确、q 包含。
func historyFilterOf(q logQuery) gamelog.HistoryFilter {
	return gamelog.HistoryFilter{Level: q.level, Event: q.event, Text: q.text}
}

// handleLogHistory 按 offset/limit 翻历史。total/hasMore 是"整份文件筛完"的口径，
// 不再像 /log 那样只给末尾 200/2000 行然后让用户上机器 grep。
func (s *Server) handleLogHistory(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	q, ok := s.parseLogQuery(w, r)
	if !ok {
		return
	}
	offset, limit, ok := s.parseHistoryPaging(w, r)
	if !ok {
		return
	}
	page, err := gamelog.NewReader(s.Paths).History(cluster, shard, q.kind, offset, limit, historyFilterOf(q))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusServiceUnavailable,
				"这个世界还没有"+q.kind.Label()+"文件：世界可能从没启动过，或者面板指错了存档目录")
			return
		}
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, page)
}

// logFilenameSafe 把任意一段收成能安全出现在 Content-Disposition 里的 ASCII 片段：
// 引号、换行、分号这些字符进了响应头就可能被当成新的头字段。
func logFilenameSafe(v string) string {
	var b strings.Builder
	for _, ch := range v {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9', ch == '-', ch == '_':
			b.WriteRune(ch)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "log"
	}
	return b.String()
}

// handleLogExport 把筛选后的日志导成 text 或 ndjson 下载。
//
// 路径完全由 shardScope 白名单里的 {name}/{shard} 和 kind 拼出来（gamelog.Reader.path
// 里还会再校验一次段名），请求里没有任何"路径"参数，也就没有 ../ 可读到别处。
// 导出走的是 ParseLine 之后的内容，Klei 令牌和 token= 那几处脱敏在写出之前就做完了。
func (s *Server) handleLogExport(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	q, ok := s.parseLogQuery(w, r)
	if !ok {
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "ndjson" {
		writeErr(w, http.StatusBadRequest, "format 只能是 text 或 ndjson")
		return
	}

	ext, ctype := "txt", "text/plain; charset=utf-8"
	if format == "ndjson" {
		ext, ctype = "ndjson", "application/x-ndjson; charset=utf-8"
	}
	filename := fmt.Sprintf("%s_%s_%s_log.%s",
		logFilenameSafe(cluster), logFilenameSafe(shard), logFilenameSafe(string(q.kind)), ext)
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	h.Set("X-Content-Type-Options", "nosniff")
	// 此刻只设了头、还没写任何字节：Export 若在 os.Open 就失败（文件不存在），
	// 下面还能把这些头改回 JSON 错误，不会发出一个"200 的附件、里面是错误信息"。
	_, err := gamelog.NewReader(s.Paths).Export(cluster, shard, q.kind, historyFilterOf(q), format, w)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			h.Del("Content-Disposition")
			writeErr(w, http.StatusServiceUnavailable,
				"这个世界还没有"+q.kind.Label()+"文件，没有可导出的内容")
			return
		}
		// 走到这里多半已经写出过几行、头也发出去了，改状态码只会让客户端收到
		// 半截文件加一段 JSON；把原文记进运行日志，让这条连接自然结束。
		s.log().Error("导出日志中途失败", "cluster", cluster, "shard", shard, "err", err.Error())
		return
	}
	s.audit(r, "log.export", cluster+"/"+shard,
		fmt.Sprintf("导出 %s 日志，格式 %s，文件 %s", q.kind.Label(), format, filename), nil)
}
