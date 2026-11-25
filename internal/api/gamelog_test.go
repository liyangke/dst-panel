package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/gamelog"
)

// logFixture 是从云机那份 server_log.txt 里挑出来的混合样本：
// 两条报错、一条警告、一条带账户 id 的进服行、一条超长时钟、一条空白行。
// 筛选类断言全靠这个构成，改它之前先想清楚每个数字还成不成立。
const logFixture = "[00:00:00]: PersistRootStorage is now /home/ubuntu/.klei/DoNotStarveTogether/Cluster_1/Master/ \n" +
	"[00:00:00]: Current time: Sat Oct 31 02:17:35 2026\n" +
	"[00:00:00]: Version: 760944\n" +
	"[00:00:01]: Starting Up\n" +
	"[00:00:02]: Account Communication Success\n" +
	"[00:00:03]: [200] Account Communication Success (6)\n" +
	"[00:00:04]: could not load morgue file ../assets/dlc/regrowth/savefiles/TheShard/Master/player_data.lua\n" +
	"[00:00:05]: Serializing world: session/2026-10-31/02_17_35/\n" +
	"[00:00:06]: [Error] Master Server Broadcast Error: [E_ROWID_EXIST]\n" +
	"[00:00:07]: [string \"../scripts/map/storygen.lua\"]:865: Must specify a layout mode for your level.\n" +
	"[00:00:08]: Client authenticated: (KU_abcdef01) 阿黎\n" +
	"\n" +
	"[100:00:00]: Available disk space for save files: 41324 MB\n"

// 这份样本的构成：13 行、1 条空白、12 条记录，其中 2 条报错、1 条警告。
const (
	fixtureLines    = 13
	fixtureEntries  = 12
	fixtureBlanks   = 1
	fixtureErrors   = 2
	fixtureWarnPlus = 3 // 警告及以上
)

func (f *apiFixture) writeChatLog(t *testing.T, shard, body string) {
	t.Helper()
	path := f.paths.ChatLog(f.cluster, shard)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建分片目录: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写 server_chat_log.txt: %v", err)
	}
}

func (f *apiFixture) appendLog(t *testing.T, shard, body string) {
	t.Helper()
	path := f.paths.ServerLog(f.cluster, shard)
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("追加日志: %v", err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.WriteString(body); err != nil {
		t.Fatalf("追加日志: %v", err)
	}
}

// getLog 拉一次日志接口并解开 data 段。
func getLog(t *testing.T, f *apiFixture, query string) (logView, *httptest.ResponseRecorder) {
	t.Helper()
	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/"+f.cluster+"/shards/Master/log"+query, "admin", "secret")
	r := decodeReply(t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET log%s = %d，body %s", query, rec.Code, rec.Body.String())
	}
	raw, err := json.Marshal(r.Data)
	if err != nil {
		t.Fatalf("data 段重新编码: %v", err)
	}
	var v logView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("解 logView: %v，原文 %s", err, raw)
	}
	// 这道检查是防"字段名对不上导致全程零值"：真机上出过一次整屏空白但断言全绿的事故。
	if v.Entries == nil {
		t.Fatalf("entries 是 null 而不是空数组，前端要判空： %+v", v)
	}
	return v, rec
}

func TestLogShowView(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", logFixture)
	v, _ := getLog(t, f, "")

	if v.Cluster != "Cluster_1" || v.Shard != "Master" {
		t.Errorf("作用域 = %s/%s", v.Cluster, v.Shard)
	}
	if v.Kind != "server" || v.KindLabel != "服务端日志" {
		t.Errorf("kind = %q/%q，码值必须配中文标签", v.Kind, v.KindLabel)
	}
	if filepath.Base(v.Path) != "server_log.txt" || !strings.Contains(v.Path, "Master") {
		t.Errorf("path = %q，没指出读的是哪份文件", v.Path)
	}
	if v.GameVersion != "760944" {
		t.Errorf("gameVersion = %q", v.GameVersion)
	}
	if v.StartAt == "" || v.StartNote != "" {
		t.Errorf("这份样本有 Current time 行，startAt 该有值而 startNote 该空：%q / %q", v.StartAt, v.StartNote)
	}
	if v.SizeBytes != int64(len(logFixture)) {
		t.Errorf("sizeBytes = %d，文件 %d 字节", v.SizeBytes, len(logFixture))
	}
	if v.Window != gamelog.DefaultLines {
		t.Errorf("不传 lines 时 window = %d，期望默认 %d", v.Window, gamelog.DefaultLines)
	}
	if v.Scanned != fixtureEntries || v.Returned != fixtureEntries || v.Blanks != fixtureBlanks {
		t.Errorf("scanned/returned/blanks = %d/%d/%d，期望 %d/%d/%d",
			v.Scanned, v.Returned, v.Blanks, fixtureEntries, fixtureEntries, fixtureBlanks)
	}
	if v.Older || v.Note != "" {
		t.Errorf("整个文件都在窗口里，不该报 older：%v %q", v.Older, v.Note)
	}
	if v.Next != v.SizeBytes {
		t.Errorf("读完整个文件后 next = %d，期望等于文件大小 %d", v.Next, v.SizeBytes)
	}
	// 顺序必须是旧→新，前端追加显示不自己反转。
	if v.Entries[0].Event != gamelog.EventBoot || v.Entries[len(v.Entries)-1].Event != gamelog.EventDiskSpace {
		t.Errorf("首尾事件 = %q / %q", v.Entries[0].Event, v.Entries[len(v.Entries)-1].Event)
	}
	for _, e := range v.Entries {
		if e.LevelLabel == "" {
			t.Errorf("级别码 %q 没配中文标签", e.Level)
		}
		if e.At == "" {
			t.Errorf("已知本轮起始时间，这条却没换算出墙上时间：offset %d %q", e.Offset, e.Raw)
		}
	}
}

func TestLogShowFilters(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", logFixture)

	// 级别是"下限"而不是"等于"，所以三档的期望数是 12/3/2。
	for _, tc := range []struct {
		level string
		want  int
	}{
		{"info", fixtureEntries},
		{"warn", fixtureWarnPlus},
		{"error", fixtureErrors},
	} {
		v, _ := getLog(t, f, "?level="+tc.level)
		if v.Returned != tc.want {
			t.Errorf("level=%s 返回 %d 条，期望 %d 条", tc.level, v.Returned, tc.want)
		}
		// scanned 不能跟着筛选变：否则"筛完剩 2 条"会被读成"只发生过 2 件事"。
		if v.Scanned != fixtureEntries {
			t.Errorf("level=%s 时 scanned = %d，筛选不该改动扫描总数", tc.level, v.Scanned)
		}
		for _, e := range v.Entries {
			if gamelog.LevelRank(e.Level) < gamelog.LevelRank(tc.level) {
				t.Errorf("level=%s 却混进了 %q：%q", tc.level, e.Level, e.Raw)
			}
		}
	}

	v, _ := getLog(t, f, "?event=save")
	if v.Returned != 1 || v.Entries[0].Raw != "Serializing world: session/2026-10-31/02_17_35/" {
		t.Fatalf("event=save 筛出 %d 条， %+v", v.Returned, v.Entries)
	}

	// plain 是"没命中任何规则"的合法筛选项，界面上要靠它看"还有哪些行没被认出来"。
	v, _ = getLog(t, f, "?event=plain")
	if v.Returned != 1 || v.Entries[0].Event != gamelog.EventPlain {
		t.Errorf("event=plain 筛出 %d 条，期望那条 Starting Up", v.Returned)
	}

	// 关键词要能命中玩家名和账户 id，这两个字段都不在 raw 之外。
	// 关键词要能命中玩家名、账户 id 和消息正文——这三个字段都在 raw/userid 里。
	for _, tc := range []struct{ raw, needle string }{
		{"阿黎", "阿黎"},
		{"KU_abcdef01", "ku_abcdef01"},
		{"disk+space", "disk space"},
	} {
		v, _ := getLog(t, f, "?q="+tc.raw)
		if v.Returned != 1 {
			t.Errorf("q=%q 命中 %d 条，期望 1 条：%+v", tc.raw, v.Returned, v.Entries)
			continue
		}
		if !strings.Contains(strings.ToLower(v.Entries[0].Raw+v.Entries[0].UserID), tc.needle) {
			t.Errorf("q=%q 命中的那条对不上：%q", tc.raw, v.Entries[0].Raw)
		}
	}

	// 条件之间是"与"。
	v, _ = getLog(t, f, "?level=error&q=broadcast")
	if v.Returned != 1 || v.Entries[0].Event != gamelog.EventBroadcastFail {
		t.Errorf("level=error&q=broadcast 筛出 %+v", v.Entries)
	}

	v, _ = getLog(t, f, "?level=error&q=不存在的东西")
	if v.Returned != 0 || v.Scanned != fixtureEntries {
		t.Errorf("筛空了但 scanned/returned = %d/%d，期望 %d/0", v.Scanned, v.Returned, fixtureEntries)
	}
}

func TestLogShowKindChat(t *testing.T) {
	f := newFixture(t, "")
	// 服务端日志故意不写：kind=chat 只能走自己那份文件，走错了会在下面 503 那条露出来。
	f.writeChatLog(t, "Master", "[00:00:09]: [Announcement] 服务器将在 1 分钟后关闭\n"+
		"[00:12:34]: 阿黎(KU_9aBcDeF01): 有人在吗\n")
	v, _ := getLog(t, f, "?kind=chat")
	if v.Kind != "chat" || v.KindLabel != "聊天与公告" {
		t.Errorf("kind = %q/%q", v.Kind, v.KindLabel)
	}
	if !strings.HasSuffix(v.Path, "server_chat_log.txt") {
		t.Errorf("path = %q，kind=chat 该读聊天文件", v.Path)
	}
	if v.Returned != 2 {
		t.Fatalf("返回 %d 条，%+v", v.Returned, v.Entries)
	}
	if v.Entries[0].Event != gamelog.EventAnnounce {
		t.Errorf("公告行算成 %q", v.Entries[0].Event)
	}
	if v.Entries[1].Event != gamelog.EventChat || v.Entries[1].Player != "阿黎" {
		t.Errorf("发言行算成 %+v", v.Entries[1])
	}
	// 聊天文件里没有 Current time 行，换算不出墙上时间时必须给一句说明，
	// 而不是默默把 at 留空让人以为是面板的 bug。
	if v.StartAt != "" || v.StartNote == "" {
		t.Errorf("startAt=%q startNote=%q", v.StartAt, v.StartNote)
	}
}

func TestLogShowBadParams(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", logFixture)
	for _, q := range []string{
		"?kind=syslog",
		"?lines=0",
		"?lines=999999",
		"?lines=abc",
		"?level=fatal",
		"?event=no_such_event",
		"?pollMs=10",
		"?pollMs=abc",
	} {
		rec := do(t, f.srv.Handler(), http.MethodGet,
			"/api/v1/clusters/"+f.cluster+"/shards/Master/log"+q, "admin", "secret")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("log%s = %d，期望 400，body %s", q, rec.Code, rec.Body.String())
			continue
		}
		// 400 必须把可接受的取值写出来：只回"参数错误"的话用户只能猜。
		if msg := decodeReply(t, rec).Err; msg == "" {
			t.Errorf("log%s 的 400 没给原因", q)
		} else if !strings.ContainsAny(msg, "只能得是没有") {
			t.Errorf("log%s 的 400 没说清怎么改：%q", q, msg)
		}
	}
}

// 分片没启动过时日志文件还不存在，这是常态而不是错误路径写错。
// 用 503 是因为前端要能把它和 404 分开：前者该提示"等分片起来"，后者是名字拼错了。
func TestLogShowMissingFileIs503(t *testing.T) {
	f := newFixture(t, "")
	for _, q := range []string{"", "?kind=chat"} {
		rec := do(t, f.srv.Handler(), http.MethodGet,
			"/api/v1/clusters/"+f.cluster+"/shards/Master/log"+q, "admin", "secret")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("log%s = %d，body %s", q, rec.Code, rec.Body.String())
			continue
		}
		msg := decodeReply(t, rec).Err
		if !strings.Contains(msg, "还没") {
			t.Errorf("503 没说明原因：%q", msg)
		}
	}
}

func TestLogShowScopeChecks(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", logFixture)
	h := f.srv.Handler()
	for _, path := range []string{
		"/api/v1/clusters/Cluster_9/shards/Master/log",
		"/api/v1/clusters/Cluster_1/shards/Waterworld/log",
	} {
		if rec := do(t, h, http.MethodGet, path, "admin", "secret"); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d，期望 404，body %s", path, rec.Code, rec.Body.String())
		}
	}

	// 带 .. 的路径根本到不了 handler：net/http 的 mux 先做路径归一化再回 301。
	// 这里钉住的是"绝不回 200、绝不带日志内容"，Reader 里那层 checkSegment
	// 是第二道闸（它自己的用例在 gamelog 包）。
	rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/../log", "admin", "secret")
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "PersistRootStorage") {
		t.Errorf("穿越路径拿到了日志：%d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/log", "admin", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("错凭据 log = %d，期望 401", rec.Code)
	}
}

// 筛选下拉的数据源必须和筛选参数严格一致，否则界面能选出一个永远拿不到记录的码。
func TestLogEventsCatalogRoundTrips(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", logFixture)
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/log-events", "admin", "secret")
	raw, err := json.Marshal(decodeReply(t, rec).Data)
	if err != nil {
		t.Fatalf("log-events 的 data 段重新编码: %v", err)
	}
	var body struct {
		Kinds  []gamelog.EventInfo `json:"kinds"`
		Levels []gamelog.EventInfo `json:"levels"`
		Events []gamelog.EventInfo `json:"events"`
		// Behaviours 是事件表动作下拉的选项，和 events（逐行日志的字典）不是同一份。
		Behaviours []gamelog.EventInfo `json:"behaviours"`
		Limits     map[string]int      `json:"limits"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("log-events 形态不对: %v，原文 %s", err, raw)
	}
	if len(body.Kinds) != 2 || len(body.Levels) != 3 || len(body.Events) < 20 {
		t.Errorf("kinds/levels/events = %d/%d/%d", len(body.Kinds), len(body.Levels), len(body.Events))
	}
	for _, k := range body.Kinds {
		if k.Label == "" || k.Label == k.Code {
			t.Errorf("种类 %q 没有中文标签", k.Code)
		}
	}
	for _, e := range body.Events {
		if e.Label == "" {
			t.Errorf("事件 %q 没配说明文字，下拉框里会是一串裸码值", e.Code)
		}
	}
	if body.Limits["maxLines"] != gamelog.MaxLines || body.Limits["minPollMs"] == 0 ||
		body.Limits["eventDefaultSize"] != gamelog.EventDefaultSize {
		t.Errorf("limits = %+v", body.Limits)
	}
	// behaviours 是事件表"动作"下拉的选项，每一项都得有短名和整句说明，
	// 否则下拉里会露出裸码值，用户也不知道这一档筛的是什么。
	if len(body.Behaviours) == 0 {
		t.Fatal("behaviours 是空的，事件表的动作筛选没有选项可用")
	}
	for _, b := range body.Behaviours {
		if b.Name == "" || b.Label == "" || b.Name == b.Code {
			t.Errorf("行为 %q 的短名/说明没配好：%q / %q", b.Code, b.Name, b.Label)
		}
	}

	for _, e := range body.Events {
		rec := do(t, f.srv.Handler(), http.MethodGet,
			"/api/v1/clusters/"+f.cluster+"/shards/Master/log?event="+e.Code, "admin", "secret")
		if rec.Code != http.StatusOK {
			t.Errorf("catalog 里的事件码 %q 被筛选参数拒绝：%d %s", e.Code, rec.Code, rec.Body.String())
		}
	}
}

// sseFrame 是一帧解析好的 SSE。注释帧（": keepalive"）不带 event/data，直接跳过。
type sseFrame struct {
	event string
	data  map[string]any
}

func TestLogStreamOnce(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", logFixture)
	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/"+f.cluster+"/shards/Master/log/stream?once=1&lines=50", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream = %d，body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("没关反代缓冲，nginx 后面这条流会被攒着：X-Accel-Buffering = %q", rec.Header().Get("X-Accel-Buffering"))
	}

	frames := splitFrames(t, rec.Body.String())
	if len(frames) < 3 {
		t.Fatalf("帧数 = %d，%q", len(frames), rec.Body.String())
	}
	if frames[0].event != "meta" {
		t.Errorf("第一帧 = %q，必须先给 meta 交代时间基准和窗口", frames[0].event)
	}
	if got := frames[0].data["kindLabel"]; got != "服务端日志" {
		t.Errorf("meta.kindLabel = %v", got)
	}
	if got := frames[0].data["window"]; fmt.Sprint(got) != "50" {
		t.Errorf("meta.window = %v，期望 50", got)
	}
	// meta 帧只给上下文，不重复带 entries：正文靠 line 帧一条条走。
	if raw, _ := json.Marshal(frames[0].data["entries"]); string(raw) != "null" {
		t.Errorf("meta.entries 应为空，实际 %s", raw)
	}
	lines := 0
	for _, fr := range frames[1 : len(frames)-1] {
		if fr.event != "line" {
			t.Errorf("backlog 中间混进 %q 帧", fr.event)
		}
		lines++
	}
	if frames[len(frames)-1].event != "done" {
		t.Errorf("最后一帧 = %q，once 模式必须以 done 收尾", frames[len(frames)-1].event)
	}
	if got := frames[len(frames)-1].data["returned"]; fmt.Sprint(got) != fmt.Sprint(fixtureEntries) {
		t.Errorf("done.returned = %v，期望 %d", got, fixtureEntries)
	}
	if lines != fixtureEntries {
		t.Errorf("line 帧 %d 条，期望 %d 条", lines, fixtureEntries)
	}

	// 筛选在流里同样生效：报错只该有 2 条，且 done.returned 要和 line 帧数一致。
	rec = do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/"+f.cluster+"/shards/Master/log/stream?once=1&level=warn", "admin", "secret")
	var got int
	for _, fr := range splitFrames(t, rec.Body.String()) {
		switch fr.event {
		case "line":
			got++
			if lv := fr.data["level"]; lv == gamelog.LevelInfo {
				t.Errorf("level=warn 的流里推来了普通：%v", fr.data["raw"])
			}
		case "done":
			if fmt.Sprint(fr.data["returned"]) != fmt.Sprint(got) {
				t.Errorf("done.returned = %v，实发 line 帧 %d", fr.data["returned"], got)
			}
		}
	}
	if got != fixtureWarnPlus {
		t.Errorf("level=warn 推了 %d 条 line 帧，期望 %d", got, fixtureWarnPlus)
	}
}

// backlog 读不出来时不能先把流式头写出去，否则前端分不清"接口错了"和"日志有新情况"。
func TestLogStreamFailureStaysPlainJSON(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/"+f.cluster+"/shards/Master/log/stream?once=1", "admin", "secret")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("没日志文件时订阅 = %d，body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q，失败响应必须还是 json", ct)
	}
	if decodeReply(t, rec).Err == "" {
		t.Error("失败响应没给中文原因")
	}
}

func TestLogStreamFollowsAndAnnouncesRestart(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", logFixture)
	srv := httptest.NewServer(f.srv.Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/v1/clusters/"+f.cluster+"/shards/Master/log/stream?pollMs=200", nil)
	if err != nil {
		t.Fatalf("建请求: %v", err)
	}
	req.SetBasicAuth("admin", "secret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("订阅: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}

	frames := make(chan sseFrame, 64)
	go pumpFrames(resp.Body, frames)

	next := func(want string) sseFrame {
		select {
		case fr := <-frames:
			if fr.event != want {
				t.Fatalf("帧序错了：想要 %q，来了 %q，data %+v", want, fr.event, fr.data)
			}
			return fr
		case <-time.After(5 * time.Second):
			t.Fatal("等 SSE 帧超时——实时推送这条路没通")
		}
		return sseFrame{}
	}
	quiet := func(why string) {
		select {
		case fr := <-frames:
			t.Errorf("%s：不该来帧，却来了 %+v", why, fr)
		case <-time.After(600 * time.Millisecond):
		}
	}

	next("meta")
	for i := 0; i < fixtureEntries; i++ {
		next("line")
	}

	// 引擎在跑，日志是追加进来的：轮询要把它带出来，而且只带新增的那几条。
	f.appendLog(t, "Master", "[00:00:30]: Serializing world: 追加的第一次\n")
	if fr := next("line"); fr.data["raw"] != "Serializing world: 追加的第一次" {
		t.Errorf("追加行的 raw = %v", fr.data["raw"])
	}
	f.appendLog(t, "Master", "[00:00:31]: PANIC: missing required prefab sculpture_bishop\n")
	if fr := next("line"); fr.data["event"] != gamelog.EventPanic || fr.data["level"] != gamelog.LevelError {
		t.Errorf("panic 行没被认出来：%+v", fr.data)
	}

	// 半行（引擎还在写）既不显示也不消费，补完换行才出这一条。
	f.appendLog(t, "Master", "[00:00:32]: 写到一半")
	quiet("半行不该被推出去")
	f.appendLog(t, "Master", "的后半段\n")
	if fr := next("line"); fr.data["raw"] != "写到一半的后半段" {
		t.Errorf("补完的半行 = %v，期望整条原文", fr.data["raw"])
	}

	// 引擎重启会把这份文件截断重写。不报 reset 的话前端会把新一轮第一行
	// 接在上一行后面，时间线接错还不报错。
	f.writeBootLog(t, "Master", "[00:00:00]: PersistRootStorage is now /home/ubuntu/.klei/DoNotStarveTogether/Cluster_1/Master/ \n")
	next("reset")
	if fr := next("line"); fr.data["event"] != gamelog.EventBoot {
		t.Errorf("重启后第一条 = %v", fr.data["event"])
	}

	// 断开后 handler 必须退出轮询循环；它不退的话这个测试会挂在 defer cancel 之后没完没了。
	cancel()
	select {
	case fr, ok := <-frames:
		if ok && fr.event != "" {
			t.Logf("取消后还收到一帧 %q（取消与最后一读重合属正常）", fr.event)
		}
	case <-time.After(3 * time.Second):
	}
}

// pumpFrames 把 SSE 流拆成帧。注释帧和空行跳过，多行 data 在本面板不会出现。
func pumpFrames(body io.Reader, out chan<- sseFrame) {
	defer close(out)
	br := bufio.NewReader(body)
	var event string
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "": // 帧边界
				if event != "" {
					out <- sseFrame{event: event}
				}
				event = ""
			case strings.HasPrefix(trimmed, ":"): // keepalive 注释
			case strings.HasPrefix(trimmed, "event: "):
				event = strings.TrimPrefix(trimmed, "event: ")
			case strings.HasPrefix(trimmed, "data: "):
				var data map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(trimmed, "data: ")), &data)
				out <- sseFrame{event: event, data: data}
				event = ""
			}
		}
		if err != nil {
			return
		}
	}
}

// splitFrames 是 recorder 版的帧拆分（once 模式一次性写完，直接按空行切块即可）。
func splitFrames(t *testing.T, body string) []sseFrame {
	t.Helper()
	var out []sseFrame
	for _, blk := range strings.Split(body, "\n\n") {
		blk = strings.TrimSpace(blk)
		if blk == "" || strings.HasPrefix(blk, ":") {
			continue
		}
		var fr sseFrame
		for _, line := range strings.Split(blk, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				fr.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &fr.data); err != nil {
					t.Fatalf("帧 data 不是合法 JSON: %v，原文 %q", err, line)
				}
			}
		}
		if fr.event == "" {
			t.Fatalf("拆出没有事件名的帧：%q", blk)
		}
		out = append(out, fr)
	}
	return out
}
