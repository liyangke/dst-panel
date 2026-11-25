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

// eventsServerBody 是事件表接口的样本服务端日志：一条完整的身份链
// （连接 → 验证 → 会话初始化 → 角色生成）加一次死亡和一记踢人。
// 行形状对着 2026-11-03 云机 Master/server_log.txt，标识换成夹具值。
const eventsServerBody = "Current time: Tue Nov 03 09:00:00 2026\n" +
	"[00:01:05]: Client connected from 203.0.113.7|16537 <5242766981745090006>\n" +
	"[00:01:06]: Client authenticated: (KU_aaaa1111) 风萧萧\n" +
	"[00:01:08]: [ClientObject] Initialized (authenticated) on server: guid=5242766981745090006 userid=KU_aaaa1111 netid=76561198928942009 admin=1\n" +
	"[00:01:34]: Spawn request: wilson from 风萧萧\n" +
	"[00:04:00]: 风萧萧 was killed by SpiderdenGuardian. They became a spooky ghost!\n" +
	"[00:05:00]: [Kick] (KU_host00001) 房主 kicked (KU_aaaa1111) 风萧萧\n"

const eventsChatBody = "[00:01:24]: [Join Announcement] 风萧萧\n" +
	"[00:02:00]: (KU_aaaa1111) 风萧萧：谁来帮忙打龙\n" +
	"[00:06:43]: [Leave Announcement] 风萧萧\n"

func getEvents(t *testing.T, f *apiFixture, query string) (gamelog.EventPage, *httptest.ResponseRecorder) {
	t.Helper()
	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/"+f.cluster+"/shards/Master/events"+query, "admin", "secret")
	r := decodeReply(t, rec)
	var page gamelog.EventPage
	if rec.Code == http.StatusOK {
		raw, err := json.Marshal(r.Data)
		if err != nil {
			t.Fatalf("data 段重新编码: %v", err)
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatalf("解 EventPage: %v，原文 %s", err, raw)
		}
		// 字段名对不上时会一路零值、断言全绿，所以先钉住"真的给出行来了"，
		// 以及 rows 是空数组而不是 null（前端 v-for 拿到 null 就整页崩）。
		if page.Rows == nil {
			t.Fatalf("rows 是 null 而不是空数组：%s", raw)
		}
	}
	return page, rec
}

func TestEventsAttachesIdentityAndPages(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", eventsServerBody)
	f.writeChatLog(t, "Master", eventsChatBody)

	page, rec := getEvents(t, f, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("events = %d，body %s", rec.Code, rec.Body.String())
	}
	if page.Cluster != "Cluster_1" || page.Shard != "Master" {
		t.Errorf("作用域 = %s/%s", page.Cluster, page.Shard)
	}
	if page.Window != gamelog.EventDefaultLines {
		t.Errorf("不传 lines 时 window = %d，期望默认 %d", page.Window, gamelog.EventDefaultLines)
	}
	if page.Size != gamelog.EventDefaultSize || page.Page != 1 {
		t.Errorf("size/page = %d/%d，期望 %d/1", page.Size, page.Page, gamelog.EventDefaultSize)
	}
	if page.Matched == 0 || len(page.Rows) != page.Matched {
		t.Fatalf("matched=%d、rows=%d，条数不多时两数必须一致", page.Matched, len(page.Rows))
	}
	if page.RoundStart == "" {
		t.Error("roundStart 没给：这一轮从几点开始界面说不出来")
	}
	join := gamelog.EventRow{}
	for _, r := range page.Rows {
		if r.Event == gamelog.EventPlayerJoin {
			join = r
			break
		}
	}
	if join.Player != "风萧萧" || join.IP != "203.0.113.7" || join.SteamID != "76561198928942009" ||
		join.UserID != "KU_aaaa1111" || join.Prefab != "wilson" {
		t.Errorf("进服那一记的身份没接上：%+v", join)
	}
	if join.Kind != string(gamelog.KindChat) || join.KindLabel == "" {
		t.Errorf("行要说得出自己出自哪份文件（IP 只有服务端日志那份有）：%+v", join)
	}
	for _, r := range page.Rows {
		if r.Name == "" {
			t.Errorf("行没有短名，界面会露出裸码值：%+v", r)
		}
		if r.Raw == "" {
			t.Errorf("行没有原文：%+v", r)
		}
	}
	// 默认档不含发言，且收掉的条数要说出来。
	for _, r := range page.Rows {
		if r.Event == gamelog.EventChat {
			t.Errorf("默认档里出现了发言行：%+v", r)
		}
	}
	if page.ChatHidden == 0 || page.ChatIn {
		t.Errorf("chatHidden=%d chatIn=%v，默认档必须数出被收掉的发言", page.ChatHidden, page.ChatIn)
	}
}

func TestEventsChatSwitchIsReal(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", eventsServerBody)
	f.writeChatLog(t, "Master", eventsChatBody)

	off, _ := getEvents(t, f, "")
	on, rec := getEvents(t, f, "?chat=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat=1 = %d，body %s", rec.Code, rec.Body.String())
	}
	if !on.ChatIn || on.Matched != off.Matched+off.ChatHidden {
		t.Errorf("开关前后的数对不上：%d vs %d+%d", on.Matched, off.Matched, off.ChatHidden)
	}
	seen := false
	for _, r := range on.Rows {
		if r.Event == gamelog.EventChat {
			seen = true
		}
	}
	if !seen {
		t.Error("打开之后还是没有发言行，那这个开关是假的")
	}
}

func TestEventsRejectsBadParams(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", eventsServerBody)
	f.writeChatLog(t, "Master", eventsChatBody)

	for _, q := range []string{
		"?lines=0", "?lines=abc", "?lines=" + strings.Repeat("9", 7),
		"?size=0", "?size=101", "?size=x",
		"?page=0", "?page=-3", "?page=1.5",
		"?event=not_a_behaviour", "?event=save",
	} {
		_, rec := getEvents(t, f, q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET events%s = %d，期望 400，body %s", q, rec.Code, rec.Body.String())
		}
	}
	// 上限内都要放行，否则界面上的档位一选就红。
	if _, rec := getEvents(t, f, "?lines="+strconv.Itoa(gamelog.MaxLines)+"&size="+strconv.Itoa(gamelog.EventMaxSize)+"&page=1"); rec.Code != http.StatusOK {
		t.Errorf("边界内的参数被拒：%d %s", rec.Code, rec.Body.String())
	}
}

// TestEventsAcceptsEveryCatalogBehaviour 钉住"字典里有、表格里筛不出"这件事：
// 动作下拉里的每一项都必须能被接口接受，否则用户选到一个永远 400 的选项。
func TestEventsAcceptsEveryCatalogBehaviour(t *testing.T) {
	f := newFixture(t, "")
	f.writeBootLog(t, "Master", eventsServerBody)
	f.writeChatLog(t, "Master", eventsChatBody)

	for _, b := range gamelog.BehaviourCatalog() {
		_, rec := getEvents(t, f, "?event="+b.Code)
		if rec.Code != http.StatusOK {
			t.Errorf("动作 %q 被接口拒绝：%d %s", b.Code, rec.Code, rec.Body.String())
		}
	}
}

func TestEventsNeedsLogFiles(t *testing.T) {
	f := newFixture(t, "")
	_, rec := getEvents(t, f, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("两份日志都没有时 = %d，期望 503，body %s", rec.Code, rec.Body.String())
	}
	// 只有一份也照常出结果：世界刚起来时聊天文件常常还是空的。
	f.writeBootLog(t, "Master", eventsServerBody)
	page, rec := getEvents(t, f, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("只有服务端日志时 = %d，body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(page.Note, "有一份日志") {
		t.Errorf("note = %q，该说明缺了一份文件", page.Note)
	}
}

func TestEventsPageBeyondLastClamps(t *testing.T) {
	f := newFixture(t, "")
	var b strings.Builder
	b.WriteString("Current time: Tue Nov 03 09:00:00 2026\n")
	for i := 1; i <= 12; i++ {
		b.WriteString("[00:0" + strconv.Itoa(i%6) + ":0" + strconv.Itoa(i%6) + "]: 玩家" + strconv.Itoa(i) + " has joined the game\n")
	}
	f.writeBootLog(t, "Master", b.String())

	page, rec := getEvents(t, f, "?size=5&page=99")
	if rec.Code != http.StatusOK {
		t.Fatalf("越界页码 = %d，body %s", rec.Code, rec.Body.String())
	}
	if page.Pages != 3 || page.Page != 3 || len(page.Rows) != 2 {
		t.Errorf("pages/page/rows = %d/%d/%d，期望夹到最后一页 3/3/2", page.Pages, page.Page, len(page.Rows))
	}
}
