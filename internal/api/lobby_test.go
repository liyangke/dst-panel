package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dstpanel/internal/lobby"
	"dstpanel/internal/store"
)

// lobbyFixture 是和真桶同形的一份正文：顶层只有 GET 一个键（真桶就是这样，见 lobby 包的注释）。
// 里面故意放一条没地址、一条没端口的，用来验"源记录数"和"能用房间数"这两个数是分开说的——
// 只报后者的话，哪天 Klei 改了字段形状，界面会安静地少一半房间而没人知道。
const lobbyFixture = `{"GET":[
 {"name":"生存小屋","__addr":"1.2.3.4","port":10999,"connected":3,"maxconnections":6,
  "intent":"survival","season":"autumn","mode":"survival","tags":"caves,show",
  "password":false,"clanonly":false,"lanonly":false,"allownewplayers":true,"serverpaused":false,
  "dedicated":true,"clienthosted":false,"fo":false,"v":740477},
 {"name":"满员的房间","__addr":"1.2.3.5","port":10999,"connected":6,"maxconnections":6,
  "intent":"survival","season":"winter","tags":"caves",
  "password":false,"allownewplayers":true,"dedicated":true,"clienthosted":true},
 {"name":"要密码的房间","__addr":"1.2.3.6","port":10999,"connected":0,"maxconnections":6,
  "intent":"endless","season":"spring",
  "password":true,"allownewplayers":true,"dedicated":true,"clienthosted":false},
 {"name":"暂停中的房间","__addr":"1.2.3.7","port":11001,"connected":1,"maxconnections":8,
  "intent":"survival","season":"summer","tags":"show",
  "password":false,"allownewplayers":true,"serverpaused":true,"kleiofficial":true},
 {"name":"不收新人的房间","__addr":"1.2.3.8","port":10999,"connected":0,"maxconnections":10,
  "intent":"wilderness","season":"autumn",
  "password":false,"allownewplayers":false,"dedicated":true},
 {"name":"没地址的一条","__addr":"","port":10999,"connected":0,"maxconnections":6,"intent":"survival"},
 {"name":"没端口的一条","__addr":"1.2.3.9","port":0,"connected":0,"maxconnections":6,"intent":"survival"}
]}`

// fakeLobby 是那个 CDN 桶的替身。形状必须和实测一致：响应头写 application/json、
// 不写 Content-Encoding，正文却是 gzip—— lobby 包自己认魔数，这里不能替它解好。
type fakeLobby struct {
	body   string
	hits   atomic.Int64
	broken atomic.Bool
	srv    *httptest.Server
}

func newFakeLobby(t *testing.T, body string) *fakeLobby {
	t.Helper()
	fl := &fakeLobby{body: body}
	fl.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl.hits.Add(1)
		if fl.broken.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "boom")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(fl.body))
		_ = zw.Close()
	}))
	t.Cleanup(fl.srv.Close)
	return fl
}

func (fl *fakeLobby) cache(ttl time.Duration) *lobby.Cache {
	f := lobby.NewFetcher("", 5*time.Second)
	f.BaseURL = fl.srv.URL
	return lobby.NewCache(f, ttl, 0, true)
}

// newFixtureLobby 给默认夹具接上假桶。
// 为什么要在夹具里就接好：那是一条 GET 接口的 null 不变式会扫所有读接口，
// 没接上的话这一页永远拿到 503、被那句"只守答得出来的响应"跳过，等于没测。
func newFixtureLobby(t *testing.T) *lobby.Cache {
	t.Helper()
	return newFakeLobby(t, lobbyFixture).cache(time.Hour)
}

func lobbyGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(t, h, http.MethodGet, path, "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d，body %s", path, rec.Code, rec.Body.String())
	}
	return rec
}

func TestLobbyRegionsAnswersInChinese(t *testing.T) {
	f := newFixture(t, "")
	rec := lobbyGet(t, f.srv.Handler(), "/api/v1/lobby/regions")
	var v lobbyRegionsView
	decodeInto(t, rec, &v)

	if !v.Enabled || len(v.Regions) != 3 || v.Problem != "" {
		t.Fatalf("开关与地区表不对：%+v", v)
	}
	if v.DefaultRegion != "ap-southeast-1" {
		t.Errorf("默认大区该是亚太（用面板的人在国内），实际 %s", v.DefaultRegion)
	}
	for _, r := range v.Regions {
		// 界面上摆的是 Label，绝不可能出现 ap-southeast-1 这种码值当标题。
		if r.Label == "" || r.Note == "" || r.ID == "" {
			t.Errorf("一个大区三样都得有：%+v", r)
		}
	}
	if v.Regions[0].Label != "亚太" {
		t.Errorf("第一个该是亚太，实际 %q", v.Regions[0].Label)
	}
	if !strings.Contains(v.Note, "快照") {
		t.Errorf("顶部那句来历说明没了：%q", v.Note)
	}
}

func TestLobbyRoomsBasicShape(t *testing.T) {
	f := newFixture(t, "")
	rec := lobbyGet(t, f.srv.Handler(), "/api/v1/lobby/rooms")
	var v lobbyRoomsView
	decodeInto(t, rec, &v)

	if v.SourceRecords != 7 || v.UsableRooms != 5 {
		t.Errorf("两个数要分开说：源记录 7 条、能用的 5 间，实际 %d / %d", v.SourceRecords, v.UsableRooms)
	}
	if v.WithPlayers != 3 {
		t.Errorf("这一刻有人的该是 3 间，实际 %d", v.WithPlayers)
	}
	if v.RegionLabel != "亚太" || v.RegionNote == "" {
		t.Errorf("大区名与说明由后端拼：%+v", v)
	}
	if !v.Live || v.Stale || v.StaleWhy != "" {
		t.Errorf("第一次取到就该是新的：%+v", v)
	}
	if v.FetchedAt == "" || v.FetchedAgo == "" || v.DecodedSize == "" {
		t.Errorf("几点取的、多大，三个都得有：%q %q %q", v.FetchedAt, v.FetchedAgo, v.DecodedSize)
	}
	if !strings.Contains(v.JoinNote, "控制台") {
		t.Errorf("连接说明要告诉人去控制台（游戏没有按 IP 加入的输入框）：%q", v.JoinNote)
	}
	// 这句里不能出现"上面那句/下面这句"这类指代：命令默认只挂在按钮的悬停标题上，
	// 页面上没有那样一行字，用户照着提示会去选中一段不存在的东西（实测的 bug）。
	if strings.Contains(v.JoinNote, "上面那句") || strings.Contains(v.JoinNote, "下面这句") {
		t.Errorf("连接说明指了一行页面上没有的字：%q", v.JoinNote)
	}
	// 默认按在线人多在前，人数相同时先看空位。
	want := []string{"满员的房间", "生存小屋", "暂停中的房间", "不收新人的房间", "要密码的房间"}
	if len(v.Rooms) != len(want) {
		t.Fatalf("列表条数不对：%d", len(v.Rooms))
	}
	for i, name := range want {
		if v.Rooms[i].Name != name {
			t.Errorf("第 %d 行该是 %s，实际 %s", i+1, name, v.Rooms[i].Name)
		}
	}
	if v.Rooms[0].AccessLabel != "没有门槛" {
		t.Errorf("门槛那句该是中文结论，实际 %q", v.Rooms[0].AccessLabel)
	}
	if v.Rooms[1].IntentLabel != "生存" || v.Rooms[1].SeasonLabel != "秋天" {
		t.Errorf("玩法与季节要翻成中文：%+v", v.Rooms[1])
	}
	// 满员那间不该给一个"点了也连不上"的可复制状态。
	if v.Rooms[0].Joinable {
		t.Errorf("满员的房间不能算可进：%+v", v.Rooms[0])
	}
	for _, r := range v.Rooms {
		if r.Name == "没地址的一条" || r.Name == "没端口的一条" {
			t.Errorf("连不上手的条目不该进列表：%+v", r)
		}
	}
	if len(v.Intents) != 3 || v.Intents[0].Value != "endless" || v.Intents[0].Label != "无尽" {
		t.Errorf("玩法下拉只列这份列表里真有的：%+v", v.Intents)
	}
	if len(v.Sorts) != 3 {
		t.Errorf("排序选项该由后端给中文标签：%+v", v.Sorts)
	}
}

func TestLobbyRoomsFilters(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	cases := []struct {
		path string
		want int
		why  string
	}{
		{"/api/v1/lobby/rooms?q=小屋", 1, "按名字"},
		{"/api/v1/lobby/rooms?access=open", 3, "只看没门槛"},
		{"/api/v1/lobby/rooms?access=locked", 2, "只看有门槛"},
		{"/api/v1/lobby/rooms?slots=free", 4, "只看有空位"},
		{"/api/v1/lobby/rooms?slots=full", 1, "只看满员"},
		{"/api/v1/lobby/rooms?players=has", 3, "只看有人"},
		{"/api/v1/lobby/rooms?players=none", 2, "只看没人"},
		{"/api/v1/lobby/rooms?intent=survival", 3, "只看生存"},
		{"/api/v1/lobby/rooms?q=1.2.3.4", 1, "按地址筛"},
	}
	for _, c := range cases {
		var v lobbyRoomsView
		decodeInto(t, lobbyGet(t, h, c.path), &v)
		if v.Total != c.want || len(v.Rooms) != c.want {
			t.Errorf("%s（%s）：total=%d 行数=%d，期望 %d", c.path, c.why, v.Total, len(v.Rooms), c.want)
		}
		if v.Matched != 5 {
			t.Errorf("%s：matched 要说的是这份列表里可用的总数，实际 %d", c.path, v.Matched)
		}
	}

	// 筛完的条件要回给界面：切大区、翻页、后退之后下拉不该留着上一次的选项。
	var echo lobbyRoomsView
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?q=小屋&slots=free&sort=free"), &echo)
	if echo.Query.Text != "小屋" || echo.Query.Slots != "free" || echo.Query.Sort != "free" {
		t.Errorf("筛选条件没回：%+v", echo.Query)
	}

	// 暂停那一间的门槛说法必须和"要密码"区分开：一个是进不去，一个是进去也不动。
	var locked lobbyRoomsView
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?access=locked"), &locked)
	var paused lobbyRoomsView
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?q=暂停"), &paused)
	if len(paused.Rooms) != 1 || !strings.Contains(paused.Rooms[0].AccessLabel, "暂停") {
		t.Errorf("暂停中的房间要标出暂停：%+v", paused.Rooms)
	}
	if paused.Rooms[0].Joinable {
		t.Errorf("房主暂停了就不算可进：%+v", paused.Rooms[0])
	}
}

func TestLobbyRoomsPagingIsReal(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	var p1, p2, p3 lobbyRoomsView
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?size=2&page=1"), &p1)
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?size=2&page=2"), &p2)
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?size=2&page=3"), &p3)
	if p1.Pages != 3 || p1.Total != 5 {
		t.Fatalf("分页算错：pages=%d total=%d", p1.Pages, p1.Total)
	}
	if len(p1.Rooms) != 2 || len(p2.Rooms) != 2 || len(p3.Rooms) != 1 {
		t.Errorf("三页该是 2/2/1 行：%d/%d/%d", len(p1.Rooms), len(p2.Rooms), len(p3.Rooms))
	}
	if p1.Rooms[0].Name == p2.Rooms[0].Name {
		t.Errorf("第二页必须是同一份排序的下一段，不是重新切一刀：%s vs %s",
			p1.Rooms[0].Name, p2.Rooms[0].Name)
	}

	// 页码超界钳到最后一页，而不是给一张空表。
	var over lobbyRoomsView
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?size=2&page=99"), &over)
	if over.Page != 3 || len(over.Rooms) != 1 {
		t.Errorf("超界页码该钳回最后一页，实际 page=%d 行数=%d", over.Page, len(over.Rooms))
	}

	// 换个排序，第一行就该跟着变——否则"排序"只是界面上的一个摆设。
	var byFree lobbyRoomsView
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?sort=free"), &byFree)
	if byFree.Rooms[0].Name != "不收新人的房间" {
		t.Errorf("按空位排该把 10 个空位那间摆最前，实际 %+v", byFree.Rooms[0])
	}

	// 一条都没命中也要给出得了解释的形状：空数组 + 0 条，而不是 null。
	var none lobbyRoomsView
	body := lobbyBody(t, h, "/api/v1/lobby/rooms?q=不可能存在的名字")
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?q=不可能存在的名字"), &none)
	if none.Rooms == nil || none.Total != 0 || none.Pages != 1 {
		t.Errorf("筛空了的形状不对：%+v", none)
	}
	if !strings.Contains(body, `"rooms":[]`) {
		t.Errorf("rooms 进 JSON 必须是空数组，实际 %s", body)
	}
}

func lobbyBody(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	return lobbyGet(t, h, path).Body.String()
}

func TestLobbyBadParamsSaidInChinese(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	cases := []struct{ path, want string }{
		{"/api/v1/lobby/rooms?region=ap-southeast-9", "没有这个大区"},
		{"/api/v1/lobby/rooms?access=bogus", "access 只能填"},
		{"/api/v1/lobby/rooms?slots=bogus", "slots 只能填"},
		{"/api/v1/lobby/rooms?players=bogus", "players 只能填"},
		{"/api/v1/lobby/rooms?sort=bogus", "sort 只能填"},
		{"/api/v1/lobby/rooms?page=0", "page 要是从 1 开始"},
		{"/api/v1/lobby/rooms?size=200", "size 要是 1~100"},
		{"/api/v1/lobby/rooms?intent=" + strings.Repeat("长", 41), "intent 太长"},
	}
	for _, c := range cases {
		rec := do(t, h, http.MethodGet, c.path, "admin", "secret")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d，期望 400，body %s", c.path, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s 的错误没说出是哪一项：%s", c.path, rec.Body.String())
		}
	}

	// sort 留空不是"不筛"，是"按默认那一枚排"。这句要是跟着另外三枚一起写"留空表示不筛"，
	// 用户照着理解就会以为这一屏没排过序——而它排了（在线人多在前）。
	rec := do(t, h, http.MethodGet, "/api/v1/lobby/rooms?sort=bogus", "admin", "secret")
	msg := rec.Body.String()
	if strings.Contains(msg, "留空表示不筛") || !strings.Contains(msg, "留空按在线人多在前排") {
		t.Errorf("sort 那句留空的说明说错了口径：%s", msg)
	}
}

// 出不去公网是这一页最常见的失败（云机实测过）。这时候不能把已经看得见的房间换成一条错误，
// 也不能假装"这个大区没有房间"——必须摆上一版能看的列表 + 这一次为什么没刷动。
func TestLobbyStaleServesLastGoodList(t *testing.T) {
	fl := newFakeLobby(t, lobbyFixture)
	f := newFixture(t, "")
	f.srv.Lobby = fl.cache(time.Hour)
	h := f.srv.Handler()

	var first lobbyRoomsView
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms"), &first)
	if !first.Live || first.Stale {
		t.Fatalf("第一次该是真取到的：%+v", first)
	}

	// TTL 内不带 refresh 的轮询不再打网络：这一页几秒一轮，每轮十几 MB 会把带宽喂进去。
	lobbyGet(t, h, "/api/v1/lobby/rooms")
	if fl.hits.Load() != 1 {
		t.Fatalf("TTL 内的轮询不该再打桶，hits=%d", fl.hits.Load())
	}

	fl.broken.Store(true)
	var second lobbyRoomsView
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms?refresh=1"), &second)
	if !second.Stale || second.Live {
		t.Errorf("刷不动时该标成旧数据：%+v", second)
	}
	if !strings.Contains(second.StaleWhy, "HTTP 500") {
		t.Errorf("同时要说明为什么没更新，实际 %q", second.StaleWhy)
	}
	if len(second.Rooms) != 5 || second.UsableRooms != 5 {
		t.Errorf("列表本身不能因为刷新失败就变空：%d 行", len(second.Rooms))
	}
	if fl.hits.Load() != 2 {
		t.Errorf("按了刷新就该真打一次桶，hits=%d", fl.hits.Load())
	}

	// 失败之后有一档冷却：不带 refresh 的轮询不该每次都重撞网络。
	before := fl.hits.Load()
	lobbyGet(t, h, "/api/v1/lobby/rooms")
	if fl.hits.Load() != before {
		t.Errorf("冷却期内不该再去撞网络")
	}
}

// 一次都没成功过、这次又刷不动：必须 503 并把那句中文原样给界面。
// 降级成"空列表 + ok"的话，界面说的是"这个大区一间房都没有"，而真相是出不去公网。
func TestLobbyNeverReachedIs503InChinese(t *testing.T) {
	fl := newFakeLobby(t, lobbyFixture)
	fl.broken.Store(true)
	f := newFixture(t, "")
	f.srv.Lobby = fl.cache(time.Hour)

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/lobby/rooms", "admin", "secret")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "HTTP 500") {
		t.Errorf("要把 Klei 给的状态码说出来：%s", body)
	}
}

func TestLobbyUnreachableNetworkSaysSoInChinese(t *testing.T) {
	fl := newFakeLobby(t, lobbyFixture)
	url := fl.srv.URL
	fl.srv.Close() // 端口立刻没人听了：这就是"这台机器出不去"的样子

	f := newFixture(t, "")
	f.srv.Lobby = lobby.NewCache(withBaseURL(lobby.NewFetcher("", time.Second), url), time.Hour, 0, true)
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/lobby/rooms", "admin", "secret")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "连不上 Klei 的公网房间列表") {
		t.Errorf("这句必须是中文结论，实际 %s", body)
	}
	// 同时不能把 Go 的原始错误格式（Get "URL": …）原样甩给界面。
	if strings.Contains(body, `Get "`) {
		t.Errorf("响应里漏了 Go 的内部措辞：%s", body)
	}
}

func withBaseURL(fetch *lobby.Fetcher, url string) *lobby.Fetcher {
	fetch.BaseURL = url
	return fetch
}

// 配置里关掉这一页：界面要指得出是哪一项，而不是显示一个空表格让人以为 Klei 上没房间。
func TestLobbyDisabledInConfigIsExplicit(t *testing.T) {
	f := newFixture(t, "")
	f.srv.Lobby = lobby.NewCache(lobby.NewFetcher("", time.Second), 0, 0, false)

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/lobby/rooms", "admin", "secret")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "lobby.enabled") {
		t.Errorf("要指得出配置里哪一项关着：%s", rec.Body.String())
	}

	// regions 这条不联网，所以照常 200，只是把 problem 说出来。
	rec2 := lobbyGet(t, f.srv.Handler(), "/api/v1/lobby/regions")
	var v lobbyRegionsView
	decodeInto(t, rec2, &v)
	if v.Enabled || !strings.Contains(v.Problem, "lobby.enabled") {
		t.Errorf("开关状态要说清：%+v", v)
	}
	if len(v.Regions) != 3 {
		t.Errorf("关掉的时候大区表还是要给（界面得知道开着时会有哪几项）：%+v", v.Regions)
	}
}

// 这一页读的是 Klei 的公开数据，和"哪间房归谁"无关，所以成员和只读账号都该看得到。
// 反过来说：这里一个能改的按钮都没有。
func TestLobbyOpenToMemberAndViewer(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	alice := newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	if r := assign(t, h, "Cluster_1", alice.ID); r.Code != http.StatusOK {
		t.Fatalf("指派 = %d", r.Code)
	}
	newAccount(t, h, "guest", "guest-pass-1", store.RoleViewer)

	for _, who := range [][2]string{{"alice", "alice-pass-1"}, {"guest", "guest-pass-1"}} {
		for _, path := range []string{"/api/v1/lobby/regions", "/api/v1/lobby/rooms"} {
			rec := do(t, h, http.MethodGet, path, who[0], who[1])
			if rec.Code != http.StatusOK {
				t.Errorf("%s 走 %s = %d，body %s", who[0], path, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestLobbyRoomWithoutAnyRegionParam(t *testing.T) {
	f := newFixture(t, "")
	var v lobbyRoomsView
	decodeInto(t, lobbyGet(t, f.srv.Handler(), "/api/v1/lobby/rooms"), &v)
	if v.Region != "ap-southeast-1" {
		t.Errorf("不带 region 参数时该落到亚太，实际 %q", v.Region)
	}

	// 换大区是真的换了另一份列表：假桶对每个路径回同一份内容，所以这里比的是参数回显。
	var us lobbyRoomsView
	decodeInto(t, lobbyGet(t, f.srv.Handler(), "/api/v1/lobby/rooms?region=us-east-1"), &us)
	if us.Region != "us-east-1" || us.RegionLabel != "美东" {
		t.Errorf("切大区没生效：%+v", us)
	}
}

// 不带 sort 时接口必须自己说清楚按哪一枚排序，而且那一枚要在这页下拉的选项里。
// 排序本身从来没错（lobby.sortRooms 的空值走 default 分支，就是"在线人多在前"），
// 错的是回显：query.sort 空着的时候，界面那枚没有"空值"选项的下拉只能画成一个空白框
// （实测截图如此），用户既看不出这一屏按什么排，也没法确认自己选的生效了没有。
// 这条测试就是钉住"默认值只有一个来源（后端），界面跟着它填"。
func TestLobbySortEchoesOneOfTheOptions(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	for _, path := range []string{
		"/api/v1/lobby/rooms",
		"/api/v1/lobby/rooms?sort=",
		"/api/v1/lobby/rooms?sort=free",
		"/api/v1/lobby/rooms?sort=capacity",
	} {
		var v lobbyRoomsView
		decodeInto(t, lobbyGet(t, h, path), &v)
		if len(v.Sorts) != len(lobbySorts) {
			t.Fatalf("%s：排序选项没回全：%+v", path, v.Sorts)
		}
		var label string
		for _, s := range v.Sorts {
			if s.Value == v.Query.Sort {
				label = s.Label
			}
			if s.Value == "" || s.Label == "" {
				t.Errorf("%s：排序选项里有空值（%+v），界面就会画出一个空白项", path, s)
			}
		}
		if label == "" {
			t.Errorf("%s：query.sort=%q 不在选项里，界面那枚下拉会是个空框", path, v.Query.Sort)
		}
		// 回显的那一枚必须真的作用在这一屏上，否则"排序"只是界面摆设。
		if !sortedBy(v.Rooms, v.Query.Sort) {
			t.Errorf("%s：回的是 %s（%s），但这一屏不是那个序：%s",
				path, v.Query.Sort, label, roomKeys(v.Rooms, v.Query.Sort))
		}
	}

	var d lobbyRoomsView
	decodeInto(t, lobbyGet(t, h, "/api/v1/lobby/rooms"), &d)
	if d.Query.Sort != "players" {
		t.Errorf("默认该落在 players（在线人多在前），实际 %q", d.Query.Sort)
	}
}

func roomKeys(rooms []lobbyRoomView, field string) string {
	vals := make([]string, 0, len(rooms))
	for _, r := range rooms {
		n := r.Players
		switch field {
		case "free":
			n = r.Free
		case "capacity":
			n = r.Capacity
		}
		vals = append(vals, r.Name+"="+strconv.Itoa(n))
	}
	return strings.Join(vals, " → ")
}

// sortedBy 按接口承诺的那一枚序逐行核对。空值算不过：那种情况界面填不出下拉。
func sortedBy(rooms []lobbyRoomView, by string) bool {
	var prev *int
	for _, r := range rooms {
		n := r.Players
		switch by {
		case "free":
			n = r.Free
		case "capacity":
			n = r.Capacity
		case "players":
		default:
			return false
		}
		if prev != nil && *prev < n {
			return false
		}
		v := n
		prev = &v
	}
	return true
}
