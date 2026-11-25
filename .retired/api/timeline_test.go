package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"dstpanel/internal/gamelog"
)

// timelineBootLog 是时间线接口的样本服务端日志：一次进服（验证行 + 公告行各一）、
// 一次死亡、一次踢人（动手的人和被踢的人都在同一行）。
const timelineBootLog = "[00:00:00]: Version: 760944\n" +
	"[00:01:00]: Client authenticated: (KU_1a2b3c4d) 14672355439136578013\n" +
	"[00:01:01]: 小满 has joined the game\n" +
	"[00:02:00]: 小满 was killed by SpiderdenGuardian. They became a spooky ghost!\n" +
	"[00:06:00]: [Kick] (KU_9z8y7x6w) 房主 kicked (KU_1a2b3c4d) 小满\n"

const timelineChatBody = "[00:01:01]: [Announcement] 小满 has joined the game\n" +
	"[00:02:30]: (KU_1a2b3c4d) 小满: 谁来帮忙打龙\n"

func getTimeline(t *testing.T, f *apiFixture, query string) (gamelog.Timeline, *httptest.ResponseRecorder) {
	t.Helper()
	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/"+f.cluster+"/shards/Master/timeline"+query, "admin", "secret")
	r := decodeReply(t, rec)
	var tl gamelog.Timeline
	if rec.Code == http.StatusOK {
		raw, err := json.Marshal(r.Data)
		if err != nil {
			t.Fatalf("data 段重新编码: %v", err)
		}
		if err := json.Unmarshal(raw, &tl); err != nil {
			t.Fatalf("解 Timeline: %v，原文 %s", err, raw)
		}
		// 字段名对不上时会一路零值、断言全绿，这里先钉住"真的归出人了"。
		if tl.Players == nil {
			t.Fatalf("players 是 null 而不是空数组，前端要判空: %+v", tl)
		}
	}
	return tl, rec
}

func TestTimelineMergesBothLogFiles(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", timelineBootLog)
	f.writeChatLog(t, "Master", timelineChatBody)

	tl, rec := getTimeline(t, f, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("timeline = %d，body %s", rec.Code, rec.Body.String())
	}
	if tl.Cluster != "Cluster_1" || tl.Shard != "Master" {
		t.Errorf("作用域 = %s/%s", tl.Cluster, tl.Shard)
	}
	if tl.Window != gamelog.TimelineDefaultLines {
		t.Errorf("不传 lines 时 window = %d，期望默认 %d", tl.Window, gamelog.TimelineDefaultLines)
	}
	if len(tl.Players) != 2 {
		t.Fatalf("归出 %d 个人，期望 2 个（小满、房主）: %+v", len(tl.Players), tl.Players)
	}
	// 房主那一下踢人发生在 00:06，比小满最后一次（00:02:30 发言）更晚，
	// 所以按"最近活跃的在前"排在第一个。顺序在 gamelog 包里另有细证，
	// 这里只钉接口把排好序的结果原样送了出去。
	if tl.Players[0].Key != "房主" {
		t.Errorf("第一个人 = %q，期望最后一次行为更晚的房主在前", tl.Players[0].Key)
	}

	// 两份文件都得真的被读进来：聊天行只有发言那条能提供。
	var sawChat bool
	for _, p := range tl.Players {
		for _, e := range p.Events {
			if e.Kind == "chat" {
				sawChat = true
			}
		}
	}
	if !sawChat {
		t.Error("时间线里没有任何来自聊天日志的行，两份文件其实只读了一份")
	}
	if tl.Dupled != 0 {
		t.Errorf("dupled = %d：聊天里的公告抄本不该走到去重那一步（它先被定性成公告）", tl.Dupled)
	}

	// 响应里一个码值都不该露出来：整份 JSON 里搜不到 player_death 之外的英文事件名，
	// 界面拿到的 name/kindLabel 必须全是中文。
	raw := rec.Body.String()
	for _, want := range []string{"玩家死亡", "玩家进服", "服务端日志", "聊天与公告", "有人被请下线"} {
		if !strings.Contains(raw, want) {
			t.Errorf("响应里找不到中文名 %q，界面会露出码值\n%s", want, raw)
		}
	}
}

// lines 是"数的是哪一段"的唯一依据，越界必须拒而不是裁：
// 悄悄裁到上限会让界面写着 5000 行、实际数了 2000 行。
func TestTimelineRejectsBadLines(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", timelineBootLog)
	for _, q := range []string{"?lines=0", "?lines=-3", "?lines=abc",
		"?lines=" + strconv.Itoa(gamelog.MaxLines+1), "?lines=" + strconv.Itoa(gamelog.MaxLines*5)} {
		_, rec := getTimeline(t, f, q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET timeline%s = %d，期望 400，body %s", q, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "1~") {
			t.Errorf("400 的说明没给出范围: %s", rec.Body.String())
		}
	}

	// 上限本身要放过：MaxLines 是日志页共用的那个天花板。
	_, rec := getTimeline(t, f, "?lines="+strconv.Itoa(gamelog.MaxLines))
	if rec.Code != http.StatusOK {
		t.Errorf("lines 取上限 = %d，期望 200，body %s", rec.Code, rec.Body.String())
	}
}

// 世界从没启动过 = 两份文件都没有。这时要说"还没有日志文件"，
// 而不是给一张谁都没来过的空表——空表看着像房间坏了。
func TestTimelineNeedsLogFiles(t *testing.T) {
	f := newFixture(t, "")
	_, rec := getTimeline(t, f, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("没有日志文件时 = %d，期望 503，body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "还没有日志文件") {
		t.Errorf("503 的文案没说明下一步: %s", rec.Body.String())
	}

	// 只有服务端日志的世界照样给结果，但要说明另一份还没写出来。
	f.writeBootLog(t, "Master", timelineBootLog)
	tl, rec := getTimeline(t, f, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("只有一份日志时 = %d，body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(tl.Note, "有一份日志") {
		t.Errorf("note = %q，缺文件时该说明", tl.Note)
	}
}

// 房间归属这层对新接口同样生效：它挂在 /clusters/{name}/shards/{shard}/ 下面，
// 由 guardRooms 统一判，这里钉住"漏鉴权"这一类问题。
func TestTimelineScopeAndAuth(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", timelineBootLog)
	h := f.srv.Handler()
	for _, path := range []string{
		"/api/v1/clusters/Cluster_9/shards/Master/timeline",
		"/api/v1/clusters/Cluster_1/shards/Waterworld/timeline",
	} {
		rec := do(t, h, http.MethodGet, path, "admin", "secret")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d，期望 404，body %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := do(t, h, http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/timeline", "admin", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("错凭据 timeline = %d，期望 401", rec.Code)
	}
	// 穿越路径同样不能读出别人的日志。
	rec = do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/../timeline", "admin", "secret")
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "小满") {
		t.Errorf("穿越路径拿到了时间线：%d %s", rec.Code, rec.Body.String())
	}
}

// 时间线数出来的每个事件码，日志页都必须能按同一个码筛出来：
// 界面从时间线跳到日志页时带的就是这个参数，筛不到就是假链接。
func TestTimelineEmitsOnlyFilterableEvents(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", timelineBootLog)
	f.writeChatLog(t, "Master", timelineChatBody)
	tl, _ := getTimeline(t, f, "")
	h := f.srv.Handler()

	checked := 0
	// 逐条行为回筛，而不是只筛计数里的码：/log 一次只看一份文件，
	// 时间线给的两份文件里的行各要带自己的 kind 才筛得中（不带 kind 默认服务端日志，
	// 那句聊天就会筛成空，而空列表在界面上和"确实没说过话"长得一模一样）。
	done := map[string]bool{}
	for _, p := range tl.Players {
		for _, e := range p.Events {
			key := e.Kind + "/" + e.Event
			if done[key] {
				continue
			}
			done[key] = true
			rec := do(t, h, http.MethodGet,
				"/api/v1/clusters/"+f.cluster+"/shards/Master/log?kind="+e.Kind+"&event="+e.Event,
				"admin", "secret")
			if rec.Code != http.StatusOK {
				t.Errorf("时间线给出的 %q 被日志页的筛选参数拒绝：%d %s", key, rec.Code, rec.Body.String())
				continue
			}
			v := decodeReply(t, rec)
			raw, err := json.Marshal(v.Data)
			if err != nil {
				t.Fatalf("log data 重新编码: %v", err)
			}
			// 筛出来的行数必须真包含这一记：否则"点过去看到 0 条"就是假链接。
			if !strings.Contains(string(raw), "\"event\":\""+e.Event+"\"") {
				t.Errorf("按 %q 筛日志筛不到它自己的行: %s", key, raw)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("一条计数都没归出来，样本或规则表坏了")
	}
}
