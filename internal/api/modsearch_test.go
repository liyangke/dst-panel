package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/steam"
	"dstpanel/internal/store"
)

// 检索接口最难写的不是成功路径，而是"接口 unavailable 时界面该说什么"。
// 下面每个用例都同时盯两件事：条目本身对不对，以及那句中文有没有把降级讲明白。

const searchTestKey = "ABCDEF0123456789ABCDEF0123456789"

// attachWorkshop 把假 Web API 接到 fixture 上，同时记录请求路径和 query。
func attachWorkshop(t *testing.T, f *apiFixture, handler http.HandlerFunc) *string {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := steam.New(searchTestKey, "")
	c.BaseURL = srv.URL
	f.srv.Steam = c
	return &gotPath
}

func seedCache(t *testing.T, f *apiFixture, mods ...store.SeenMod) {
	t.Helper()
	if err := f.srv.Store.RecordSeen(mods...); err != nil {
		t.Fatalf("seed RecordSeen: %v", err)
	}
}

func search(t *testing.T, f *apiFixture, query string) (modSearchView, string) {
	t.Helper()
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods/search"+query, "admin", "secret")
	var v modSearchView
	decodeData(t, rec, &v)
	return v, rec.Body.String()
}

func itemByID(v modSearchView, id string) (modSearchItem, bool) {
	for _, it := range v.Items {
		if it.WorkshopID == id {
			return it, true
		}
	}
	return modSearchItem{}, false
}

// TestModSearchWithoutKeyServesCacheWithHonestNote 是整个设计的落点：
// 没 key 时不能报错，也不能显示空列表装作工坊上没有东西。
func TestModSearchWithoutKeyServesCacheWithHonestNote(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	seedCache(t, f,
		store.SeenMod{WorkshopID: "350811795", Name: "几何防御 Geometric Defense",
			Version: "1.4", Subscriptions: 5000, ClientRequired: true, Origin: store.SeenFromWorkshop},
		store.SeenMod{WorkshopID: "111", Name: "无关模组", Subscriptions: 9000, Origin: store.SeenFromWorkshop},
	)

	v, body := search(t, f, "?q=几何")
	if v.OnlineEnable || v.Live {
		t.Errorf("没配 key 却自称在线检索过：%+v", v)
	}
	if len(v.Items) != 1 {
		t.Fatalf("结果 %d 条：%+v", len(v.Items), v.Items)
	}
	it := v.Items[0]
	if it.WorkshopID != "350811795" || it.Name != "几何防御 Geometric Defense" {
		t.Errorf("条目 = %+v", it)
	}
	if it.SourceLabel == "" || it.Source != "cache" {
		t.Errorf("要标出结果来自本地缓存：%+v", it)
	}
	if !strings.Contains(v.Note, "steamApi.webApiKey") {
		t.Errorf("Note 要指出去配哪一项：%q", v.Note)
	}
	if !strings.Contains(v.Note, "订阅工坊 ID 不需要 key") {
		t.Errorf("Note 要说清订阅不需要 key：%q", v.Note)
	}
	// 客户端要订阅是"装了没生效"的头号原因，藏在标签里等于没说。
	if !strings.Contains(v.Note, "客户端") {
		t.Errorf("带 all_clients_require_mod 时必须提醒：%q", v.Note)
	}
	if !strings.Contains(body, "几何防御") {
		t.Errorf("响应体里没有中文名：%s", body)
	}
	// key 值不能出现在任何响应里，包括"没配 key"这种文本。
	if strings.Contains(body, searchTestKey) {
		t.Error("响应里漏了 key")
	}

	// 搜索框留空时同样不能 500，也不能把全库最热当结果端出来。
	v, body = search(t, f, "")
	if len(v.Items) != 2 {
		t.Fatalf("空搜索词应该按热度列出缓存：%+v", v.Items)
	}
	if v.Items[0].Name != "无关模组" {
		t.Errorf("空搜索词的排序 = %v，期望订阅数降序", []string{v.Items[0].Name})
	}
	if !strings.Contains(body, "webApiKey") {
		t.Errorf("Note = %q", v.Note)
	}
}

// TestModSearchEmptyCacheTellsYouWhatToDoNext：一条都没有时必须给下一步，
// 只说"没有结果"的界面等于让用户自己猜是没网还是真没有这个模组。
// TestModSearchCachePagesAreReal 是"点下一页没反应"的回归测试。
// 缓存这一路以前既不吃 page（SQL 没有 OFFSET），也从不设 hasMore/total，
// 于是界面上那个按钮永远处于 disabled——用户看到的是"界面坏了"，
// 而实际上后端根本没被问过第二页。
func TestModSearchCachePagesAreReal(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	// 名字都带同一个前缀：这样磁盘夹具里的模组不会混进这一批，总数才是可预期的。
	seedCache(t, f,
		store.SeenMod{WorkshopID: "1", Name: "分页甲", Subscriptions: 900, Origin: store.SeenFromWorkshop},
		store.SeenMod{WorkshopID: "2", Name: "分页乙", Subscriptions: 800, Origin: store.SeenFromWorkshop},
		store.SeenMod{WorkshopID: "3", Name: "分页丙", Subscriptions: 700, Origin: store.SeenFromWorkshop},
		store.SeenMod{WorkshopID: "4", Name: "分页丁", Subscriptions: 600, Origin: store.SeenFromWorkshop},
		store.SeenMod{WorkshopID: "5", Name: "分页戊", Subscriptions: 500, Origin: store.SeenFromWorkshop},
	)

	ids := map[string]bool{}
	for page, want := range map[int][]string{1: {"1", "2"}, 2: {"3", "4"}, 3: {"5"}} {
		v, _ := search(t, f, fmt.Sprintf("?q=分页&page=%d&size=2", page))
		if v.Total != 5 {
			t.Errorf("第 %d 页 total = %d，期望 5（缓存这一路也要给总数，页码才画得出来）", page, v.Total)
		}
		if len(v.Items) != len(want) {
			t.Fatalf("第 %d 页 = %+v，期望 %v", page, v.Items, want)
		}
		for i, it := range v.Items {
			if it.WorkshopID != want[i] {
				t.Errorf("第 %d 页第 %d 条 = %s，期望 %s", page, i+1, it.WorkshopID, want[i])
			}
			if ids[it.WorkshopID] {
				t.Errorf("%s 在两页都出现了，翻页会看到重复行", it.WorkshopID)
			}
			ids[it.WorkshopID] = true
		}
		// 最后一页必须说"没有下一页了"，否则界面会挂一个点了没反应的按钮。
		if wantMore := page < 3; v.HasMore != wantMore {
			t.Errorf("第 %d 页 hasMore = %v，期望 %v", page, v.HasMore, wantMore)
		}
	}
	if len(ids) != 5 {
		t.Errorf("三页并起来 %d 条，期望 5 条：有行被翻页跳过了", len(ids))
	}
}

func TestModSearchEmptyCacheTellsYouWhatToDoNext(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	v, _ := search(t, f, "?q=背包")
	if len(v.Items) != 0 {
		t.Fatalf("缓存是空的却有结果：%+v", v.Items)
	}
	if !strings.Contains(v.Note, "工坊 ID") {
		t.Errorf("Note 要给替代路径：%q", v.Note)
	}
	// 一条缓存都没有（全新面板第一次打开这一页）时，"没有结果"还不够：
	// 得说清为什么是空的、下一步做什么，否则用户以为接口坏了。
	v, _ = search(t, f, "")
	if v.CacheCount != 0 || len(v.Items) != 0 {
		t.Fatalf("空缓存的空查询也该是空的：%+v", v)
	}
	if !strings.Contains(v.Note, "上传") {
		t.Errorf("Note 该指出另一条出路（先上传一个模组）：%q", v.Note)
	}
}

// TestEmptyCacheNoteDoesNotLieWhenKeyIsConfigured 钉的是云机上真出现过的一句自相矛盾：
// key 配好了、缓存是空的，Note 却说"在线检索还没配 key"。
// 降级说明比没有说明更糟的地方就在于它会把人往错的那条路上支。
func TestEmptyCacheNoteDoesNotLieWhenKeyIsConfigured(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	// key 装上，但工坊这一路答不上来：空关键词现在也要打接口（#76 那一整张榜），
	// 所以这里必须让假接口真的被问到一次，而不是靠"压根不发请求"造出降级。
	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "Too many requests")
	})

	v, _ := search(t, f, "")
	if !v.OnlineEnable {
		t.Fatal("前提没造出来：这一条要的是配了 key")
	}
	if len(v.Items) != 0 || v.CacheCount != 0 || v.Live {
		t.Fatalf("应该是空缓存、在线没成：%+v", v)
	}
	if strings.Contains(v.Note, "没配 key") {
		t.Errorf("配了 key 就不能说没配：%q", v.Note)
	}
	// 原因必须认成"接口没答上来"。说成"没给关键词"就是把人往错路上支——
	// 不给关键词现在是一条正经的浏览路径。
	if strings.Contains(v.Note, "没给搜索词") {
		t.Errorf("空关键词不是这一屏空着的原因：%q", v.Note)
	}
	if !strings.Contains(v.Note, "在线检索没成功") {
		t.Errorf("要把降级原因摆在最前面：%q", v.Note)
	}
}

// TestModSearchEmptyQueryBrowsesTheWholeWorkshopRanking 是 #76 那条需求的落点：
// "假如我不知道我要什么模组，我只是想一页一页的翻，一个个看，直到找到我想要的呢？
// 缓存没法满足我。"
//
// 钉住三件事，少一件都是假分页：
//   - 空关键词真的打到了工坊，而且发出去的是 page=N、不带 search_text
//     （云机实测只有 page 这一种翻页口径：offset / start / sort_by 全被忽略，
//     见 .tools/workshop_browse_probe.py）；
//   - 总数用的是工坊给的那一个，界面那句"共几条"和分页条是同一个数；
//   - 这一屏不掺缓存条目——上面刚说完"这是工坊自己的榜"，下面混进面板见过的东西就是假话。
func TestModSearchEmptyQueryBrowsesTheWholeWorkshopRanking(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	seedCache(t, f, store.SeenMod{WorkshopID: "999999999", Name: "面板自己见过的老面孔"})

	var got url.Values
	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[
			{"publishedfileid":"111111111","title":"榜上第一条","subscriptions":5000},
			{"publishedfileid":"222222222","title":"榜上第二条","subscriptions":4000}
		],"total":27313}}`)
	})

	v, body := search(t, f, "?page=2&size=12")
	if !v.Live || !v.OnlineEnable {
		t.Fatalf("空关键词这一屏要真走在线：%+v", v)
	}
	if _, ok := got["search_text"]; ok {
		t.Errorf("空关键词不该把 search_text 发出去：%v", got)
	}
	if got.Get("page") != "2" || got.Get("numperpage") != "12" {
		t.Errorf("翻页只能靠 page/numperpage，实际发出去的是 %v", got)
	}
	if v.Total != 27313 || !v.HasMore {
		t.Errorf("total/hasMore = %d/%v，工坊给的总数要原样传下去", v.Total, v.HasMore)
	}
	if len(v.Items) != 2 {
		t.Fatalf("这一屏只能有榜上那两条：%+v", v.Items)
	}
	for _, it := range v.Items {
		if it.Source != "live" {
			t.Errorf("缓存条目混进了工坊的榜：%+v", it)
		}
	}
	if !strings.Contains(v.Note, "工坊自己的榜") || !strings.Contains(v.Note, "27313") ||
		!strings.Contains(v.Note, "第 2 页") {
		t.Errorf("那句中文要说清这是工坊的榜、共几条、第几页：%q", v.Note)
	}
	// 同一屏不许写两遍"还有更多"：分页条自己就在下面，重复那句是用户点出来过的毛病。
	if strings.Contains(v.Note, "翻下一页") {
		t.Errorf("榜这一屏不需要再教一遍怎么翻页：%q", v.Note)
	}
	if strings.Contains(body, searchTestKey) {
		t.Error("响应里漏了 key")
	}
}

// writeWorkshopMod 造一个 workshop-<id> 目录和它的 modinfo.lua。
func writeWorkshopMod(t *testing.T, modsDir, id, body string) {
	t.Helper()
	full := filepath.Join(modsDir, "workshop-"+id)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "modinfo.lua"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDefaultListCarriesModinfoRichness 是"检索页一打开就该有内容"这条要求的落点：
// 没配 Steam key 时这一页只有磁盘上的 modinfo.lua 可用，说明/标签/客户端标记/依赖
// 任何一项在半路丢了，界面就退化成一行行只有模组名字的空壳。
func TestDefaultListCarriesModinfoRichness(t *testing.T) {
	f := newFixture(t, "")
	modsDir := ensureModsDir(t, f)
	// 裸赋值写法（引擎实际认的那种），一个带"客户端也要订阅"，一个带子模组依赖。
	writeWorkshopMod(t, modsDir, "900001", `name = "小地图"
description = [[
在右上角加一块可缩放的小地图。
]]
tags = { "Map Info", "Utility Mod" }
all_clients_require_mod = true
api_version = 10
`)
	writeWorkshopMod(t, modsDir, "900002", `name = "打包工具箱"
description = "一批 QoL 小功能的合集"
tags = { QoL = true }
mod_dependencies = { { workshop = "workshop-900001" } }
`)

	v, _ := search(t, f, "")
	if len(v.Items) != 2 {
		t.Fatalf("没给搜索词时应该把磁盘上这批都列出来：%+v", v.Items)
	}
	a, _ := itemByID(v, "900001")
	if !strings.Contains(a.Desc, "小地图") {
		t.Errorf("说明没带到界面：%+v", a)
	}
	if strings.Join(a.Tags, ",") != "Map Info,Utility Mod" {
		t.Errorf("标签 = %v", a.Tags)
	}
	if !a.ClientRequired {
		t.Errorf("all_clients_require_mod 没带到：%+v", a)
	}
	if a.ServerOnly {
		t.Error("serverOnly 是猜出来的：modinfo.lua 里没有已核实对应的字段，磁盘这条必须是 false")
	}
	if !a.Downloaded {
		t.Errorf("文件确实在模组目录里：%+v", a)
	}
	b, _ := itemByID(v, "900002")
	if !b.HasChildren {
		t.Errorf("带 mod_dependencies 的模组要标出子模组：%+v", b)
	}
	if strings.Join(b.Tags, ",") != "QoL" {
		t.Errorf("键表写法的标签 = %v", b.Tags)
	}

	// 缓存里也得是这份完整记录：下一次打开这一页靠它，靠残缺的那份等于重新来过。
	got, ok, err := f.srv.Store.GetSeen("900002")
	if err != nil {
		t.Fatalf("GetSeen: %v", err)
	}
	if !ok || got.Description == "" || len(got.Tags) != 1 || !got.HasChildren {
		t.Errorf("入库的记录不完整：%+v", got)
	}
}

func TestModSearchRejectsSillyPaging(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"?page=abc", "page"},
		{"?page=0", "page"},
		{"?size=0", "size"},
		{"?size=50", "size"},
		{"?size=big", "size"},
	} {
		rec := do(t, h, http.MethodGet, "/api/v1/mods/search"+tc.query, "admin", "secret")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应该 400，实际 %d：%s", tc.query, rec.Code, rec.Body.String())
			continue
		}
		if msg := readErr(t, rec); !strings.Contains(msg, tc.want) {
			t.Errorf("%s 的错误要提到 %s，实际 %q", tc.query, tc.want, msg)
		}
	}
	// 未登录不能借降级之名绕过认证。
	rec := do(t, h, http.MethodGet, "/api/v1/mods/search?q=x", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("检索也要认证，实际 %d", rec.Code)
	}
}

// TestModSearchLiveSuccessAnnotatesDiskState 盯的是三个状态位：
// 文件在不在磁盘上、清单里有没有——这两件事用户在同一个界面里就要看完。
func TestModSearchLiveSuccessAnnotatesDiskState(t *testing.T) {
	f := newFixture(t, "")
	modsDir := ensureModsDir(t, f)
	// 造出"已下载且在清单里"。
	if err := os.MkdirAll(filepath.Join(modsDir, "workshop-351210508"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modsDir, "workshop-351210508", "modinfo.lua"),
		[]byte(`return { name = "更多物品堆叠", version = "1.6" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.modsManager().AddSetup("351210508"); err != nil {
		t.Fatalf("AddSetup: %v", err)
	}
	if _, err := f.srv.modsManager().AddSetup("999888777"); err != nil {
		t.Fatalf("AddSetup: %v", err)
	}

	path := attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[
		 {"publishedfileid":"351210508","title":"更多物品堆叠","subscriptions":20134,
			  "tags":[{"tag":"server_only_mod"},{"tag":"version:1.6"}],"file_size":81234,"num_children":0},
		 {"publishedfileid":"999888777","title":"待下载的模组","subscriptions":12,
			  "tags":[{"tag":"all_clients_require_mod"}],"num_children":3}
		],"total":37}}`)
	})

	v, body := search(t, f, "?q=堆叠")
	if !strings.HasSuffix(*path, "/IPublishedFileService/QueryFiles/v1/") {
		t.Errorf("请求路径 = %q", *path)
	}
	if !v.Live || !v.OnlineEnable {
		t.Errorf("这条应该算在线成功：%+v", v)
	}
	if v.Total != 37 || !v.HasMore {
		t.Errorf("total/hasMore = %d/%v，第一页 10 条对 37 条总数应该还有下一页", v.Total, v.HasMore)
	}
	onDisk, ok := itemByID(v, "351210508")
	if !ok {
		t.Fatalf("结果里没有 351210508：%+v", v.Items)
	}
	if !onDisk.Downloaded || !onDisk.Registered {
		t.Errorf("磁盘状态没标出来：%+v", onDisk)
	}
	if !strings.Contains(onDisk.StatusLabel, "已下载") {
		t.Errorf("statusLabel = %q", onDisk.StatusLabel)
	}
	if onDisk.ServerOnly != true || onDisk.Version != "1.6" || onDisk.SizeLabel != "79.3 KB" {
		t.Errorf("接口字段没落到条目上：%+v", onDisk)
	}
	// 这条样本里没有 time_updated，标签必须留空而不是显示 1970-01-01。
	if onDisk.UpdatedLabel != "" {
		t.Errorf("没有更新时间却编出了一个日期：%q", onDisk.UpdatedLabel)
	}
	pending, _ := itemByID(v, "999888777")
	if pending.Downloaded || !pending.Registered {
		t.Errorf("订阅了但没下载的要分开说：%+v", pending)
	}
	if !strings.Contains(pending.StatusLabel, "重启") {
		t.Errorf("statusLabel 要说清什么时候才有文件：%q", pending.StatusLabel)
	}
	if !pending.HasChildren {
		t.Error("子模组标记丢了，用户会漏登记依赖")
	}
	if !strings.Contains(v.Note, "在线检索") {
		t.Errorf("Note = %q", v.Note)
	}
	if strings.Contains(body, searchTestKey) {
		t.Error("响应里漏了 key")
	}

	// 见过即入库：这次在线结果必须已经落到缓存里，断网后还能搜到。
	n, err := f.srv.Store.SeenCount()
	if err != nil || n < 2 {
		t.Errorf("在线结果没进缓存：SeenCount=%d err=%v", n, err)
	}
	cached, ok, err := f.srv.Store.GetSeen("999888777")
	if err != nil || !ok || cached.ClientRequired != true {
		t.Errorf("缓存里缺这条或提醒位丢了：%+v ok=%v err=%v", cached, ok, err)
	}
	if cached.Origin != store.SeenFromWorkshop {
		t.Errorf("origin = %q，接口来源不该被降级", cached.Origin)
	}
	// 磁盘上那个模组的中文名也进了缓存——这是"没 key 也能搜到已装模组"的前提。
	diskCached, ok, _ := f.srv.Store.GetSeen("351210508")
	if !ok || diskCached.Name != "更多物品堆叠" {
		t.Errorf("磁盘扫描没入库：%+v ok=%v", diskCached, ok)
	}
}

// TestModSearchLiveFailureFallsBackToCache 是用户最常遇到的那条路：
// 机房出不去。这时候给老结果 + 一句真话，比给一个 500 有用得多。
func TestModSearchLiveFailureFallsBackToCache(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	seedCache(t, f, store.SeenMod{WorkshopID: "350811795", Name: "几何防御",
		Subscriptions: 5000, Origin: store.SeenFromWorkshop})

	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<html>Access is denied. Please verify your key= parameter.</html>`)
	})

	v, body := search(t, f, "?q=几何")
	if v.Live {
		t.Error("403 却标记成在线成功")
	}
	if !strings.Contains(v.LiveProblem, "key") {
		t.Errorf("LiveProblem 要说清是 key 的事：%q", v.LiveProblem)
	}
	if !strings.Contains(v.Note, "在线检索没成功") {
		t.Errorf("Note 要承认降级：%q", v.Note)
	}
	if len(v.Items) != 1 || v.Items[0].Source != "cache" {
		t.Fatalf("降级后没有回到缓存：%+v", v.Items)
	}
	if strings.Contains(body, searchTestKey) {
		t.Errorf("整段响应里出现了 key 值：%s", body)
	}
}

// TestModSearchNotePunctuation：降级那半句的原文自己带句号（`internal/steam` 的 Conclusion），
// 而整段 Note 是把各段用"。"拼起来的，不收尾巴就会写出"能直连。。下面是"——
// 2026-11-03 公网那台机器连不上 Steam 时，屏顶就是这两个句号，真机截图上看得见。
func TestModSearchNotePunctuation(t *testing.T) {
	v := &modSearchView{OnlineEnable: true, CacheCount: 3,
		LiveProblem: "连不上 Steam 的 Web API（api.steampowered.com）。" +
			"国内机房通常要走代理：在配置里填 steamApi.proxy，或者确认这台机器能直连。"}
	v.explain(diskIndex{})
	if strings.Contains(v.Note, "。。") || strings.Contains(v.Note, "。；") {
		t.Errorf("说明里有重复标点：%q", v.Note)
	}
	if !strings.Contains(v.Note, "能直连。下面是本地缓存") {
		t.Errorf("降级说明没接上后半句：%q", v.Note)
	}
	if got := joinNote("甲。", "乙。"); got != "甲；乙" {
		t.Errorf("joinNote 该把两段的句号尾巴都收掉：%q", got)
	}
}

// TestModSearchNumericIDUsesDetails：用户手里常常只有一串抄来的数字。
// 这时候走关键词搜索会返回一堆无关结果，看起来像"工坊上没有这个模组"。
func TestModSearchNumericIDUsesDetails(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	path := attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[
		 {"publishedfileid":"350811795","result":1,"title":"几何防御","subscriptions":5000}
		]}}`)
	})
	v, _ := search(t, f, "?q=350811795")
	if !strings.HasSuffix(*path, "/IPublishedFileService/GetDetails/v1/") {
		t.Errorf("纯数字 ID 应该走精确查询，实际打了 %q", *path)
	}
	if len(v.Items) != 1 || v.Items[0].Name != "几何防御" {
		t.Fatalf("条目 = %+v", v.Items)
	}
}

func TestModSearchUnknownIDSaysWhy(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		// 真实接口对查不到的 ID 也给一条 result!=1 的记录，没有 title。
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[{"publishedfileid":"400000001","result":2}]}}`)
	})
	v, _ := search(t, f, "?q=400000001")
	if !v.Live {
		t.Error("接口活着，这条要算在线成功")
	}
	if !strings.Contains(v.LiveProblem, "抄错") {
		t.Errorf("要说清 ID 查不到是抄错/下架，而不是没网：%q", v.LiveProblem)
	}
}

// TestModSearchDedupesLiveAndCache：在线那批刚写进缓存，
// 补足名额时又会被 SearchSeen 捞回来，不去重就是同一条模组显示两遍。
func TestModSearchDedupesLiveAndCache(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	seedCache(t, f, store.SeenMod{WorkshopID: "351210508", Name: "更多物品堆叠",
		Subscriptions: 20134, Origin: store.SeenFromWorkshop})
	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[
		 {"publishedfileid":"351210508","title":"更多物品堆叠","subscriptions":20134}
		],"total":1}}`)
	})
	v, _ := search(t, f, "?q=堆叠")
	if len(v.Items) != 1 {
		t.Fatalf("同一模组出现 %d 次：%+v", len(v.Items), v.Items)
	}
	if v.Items[0].Source != "live" {
		t.Errorf("去重要保住信息更全的那条：%+v", v.Items[0])
	}
}

// TestModSearchSurvivesUnreadableModsDir：dstInstall 指错时 /mods 是 500，
// 但检索必须照常——用户正是靠它找到 ID 去修配置。代价是必须明说状态不可信。
func TestModSearchSurvivesUnreadableModsDir(t *testing.T) {
	f := newFixture(t, "") // 故意不建 mods/
	fakeEngine(t, f)       // 本体在、目录不在，才是"配置指错"这一支（另一支见 mods_test）
	seedCache(t, f, store.SeenMod{WorkshopID: "1", Name: "以前搜到的模组",
		Subscriptions: 3, Origin: store.SeenFromWorkshop})
	v, _ := search(t, f, "?q=搜到")
	if len(v.Items) != 1 {
		t.Fatalf("检索不该被模组目录挡住：%+v", v.Items)
	}
	if !strings.Contains(v.Note, "模组目录") {
		t.Errorf("Note 要说清安装状态不可信：%q", v.Note)
	}
	if !strings.Contains(v.Note, "dstInstall") {
		t.Errorf("要指到配置项：%q", v.Note)
	}
}

// TestModsShowAlsoFeedsCache：打开模组列表这条路径也得入库，
// 否则用户装完模组、没搜过、换了台没网的机器就什么都搜不到。
func TestModsShowAlsoFeedsCache(t *testing.T) {
	f := newFixture(t, "")
	modsDir := ensureModsDir(t, f)
	if err := os.MkdirAll(filepath.Join(modsDir, "workshop-4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modsDir, "workshop-4242", "modinfo.lua"),
		[]byte(`return { name = "只在看列表时见过", version = "3.2" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	decodeData(t, rec, &modsView{})

	got, ok, err := f.srv.Store.GetSeen("4242")
	if err != nil {
		t.Fatalf("GetSeen: %v", err)
	}
	if !ok || got.Name != "只在看列表时见过" || got.Version != "3.2" {
		t.Errorf("浏览列表没入库：ok=%v %+v", ok, got)
	}
	if got.Origin != store.SeenFromDisk {
		t.Errorf("origin = %q，这条只可能来自磁盘", got.Origin)
	}
	// 没有 modinfo.lua 的目录不该编出一个空名条目占缓存。
	if err := os.MkdirAll(filepath.Join(modsDir, "workshop-4243"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	decodeData(t, rec, &modsView{})
	if _, ok, _ := f.srv.Store.GetSeen("4243"); ok {
		t.Error("解析不出 modinfo 的目录也被当成认识的模组存进了缓存")
	}
}

// 一批带分类的假工坊结果：两条人物类（其中一条故意写成大写字母开头的 Other），
// 一条只带 utility，一条被 ban（不该出现在任何计数里），一条 result!=1。
const sampleBatch = `{"response":{"publishedfiledetails":[
 {"publishedfileid":"101","result":1,"title":"新人物 A","subscriptions":9000,
  "favorited":400,"vote_data":{"score":0.97,"votes_up":300,"votes_down":9},
  "tags":[{"tag":"character"},{"tag":"Other"}]},
 {"publishedfileid":"102","result":1,"title":"新人物 B","subscriptions":800,
  "favorited":20,"vote_data":{"score":0.5,"votes_up":4,"votes_down":4},
  "tags":[{"tag":"Character"},{"tag":"utility"}]},
 {"publishedfileid":"103","result":1,"title":"只有实用工具","subscriptions":70000,
  "tags":[{"tag":"utility"},{"tag":"all_clients_require_mod"}]},
 {"publishedfileid":"104","result":1,"title":"下架的人物模组","banned":true,
  "tags":[{"tag":"character"}]},
 {"publishedfileid":"105","result":2,"title":"查不到详情的那条","tags":[{"tag":"character"}]}
],"total":3592}}`

// TestModSearchLiveTagFiltersInThePanelAndSaysItsScope 是"分类"这一整条路的落点：
// Steam 不认标签参数（实测），所以筛发生在面板这一头，而那句"共 N 条"只能描述
// 这次取回的那一批。这里盯的是它有没有把范围说出口，以及有没有拿缓存条目把这一屏填满
// ——填满之后那个 N 就不再是它字面的意思了。
func TestModSearchLiveTagFiltersInThePanelAndSaysItsScope(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	// 缓存里放一条也带 character 的：在线 + 分类这一屏不该把它掺进来。
	seedCache(t, f, store.SeenMod{WorkshopID: "199", Name: "缓存里的人物模组",
		Tags: []string{"character"}, Subscriptions: 500000, Origin: store.SeenFromWorkshop})

	var gotQuery url.Values
	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		fmt.Fprint(w, sampleBatch)
	})

	v, _ := search(t, f, "?q=新人物&tag=character&size=10")
	if !v.Live {
		t.Fatalf("这一条该是在线成功：%+v", v)
	}
	if gotQuery.Get("numperpage") != "100" {
		t.Errorf("筛之前要多取一批，numperpage = %q，期望 100", gotQuery.Get("numperpage"))
	}
	if gotQuery.Get("matched_tag") != "" {
		t.Error("向接口发了 matched_tag：那个参数实测被 Steam 忽略，发出去等于假装筛过")
	}
	if v.Tag != "character" || v.TagLabel != "人物" {
		t.Errorf("分类回显 = %q/%q，界面那一排要跟着这一份画", v.Tag, v.TagLabel)
	}
	if len(v.Items) != 2 {
		t.Fatalf("结果 %d 条：%+v（期望只回两条人物类，且滤掉 banned 和 result!=1）", len(v.Items), v.Items)
	}
	for _, it := range v.Items {
		if it.Source != "live" {
			t.Errorf("在线+分类这一屏掺进了别处的条目：%+v", it)
		}
	}
	// 大写的 Character 也算人物类：工坊作者两种写法都有（实测 1200 条里 other 294 / Other 52）。
	if v.Items[0].WorkshopID != "101" || v.Items[1].WorkshopID != "102" {
		t.Errorf("命中 = %s / %s，期望 101、102（Character 大写也要算）",
			v.Items[0].WorkshopID, v.Items[1].WorkshopID)
	}
	// total 说"这一批里两条"，不是 Steam 那个 3592。
	if v.Total != 2 {
		t.Errorf("total = %d，期望 2：把接口那个 3592 端出来就是告诉用户这一类有 3592 个模组", v.Total)
	}
	if v.Batch != 3 {
		t.Errorf("batch = %d，期望 3（滤掉两条不可用的那批里，真拿来筛的是 3 条）", v.Batch)
	}
	if v.HasMore {
		t.Error("两条都在第一页里还报有下一页，那个按钮点下去是空的")
	}
	if !v.CategoryCountsKnown {
		t.Error("在线+分类这一屏的条数是数得出来的，却不肯说")
	}
	// 每一类都必须在字典顺序上，且计数是"这一批里有几条"。
	want := map[string]int{"character": 2, "other": 1, "utility": 2, "pet": 0}
	for i, c := range v.Categories {
		if w, ok := want[c.Key]; ok && c.Count != w {
			t.Errorf("分类 %s 计数 = %d，期望 %d", c.Key, c.Count, w)
		}
		if i > 0 && c.Key == v.Categories[i-1].Key {
			t.Errorf("分类重复：%+v", v.Categories)
		}
	}
	if len(v.Categories) != len(modCategories) {
		t.Errorf("分类只回 %d 枚，期望字典里全部 %d 枚（缺一枚那一排就会跳）", len(v.Categories), len(modCategories))
	}
	for _, want := range []string{"人物", "3 条", "不认标签参数"} {
		if !strings.Contains(v.Note, want) {
			t.Errorf("Note 里少了 %q：%q", want, v.Note)
		}
	}
	if strings.Contains(v.Note, "3592") {
		t.Errorf("Note 把接口那个总数当成了这一类的总数：%q", v.Note)
	}
	// 星星：这两条都带 vote_data，收藏数也得落到条目上。
	if v.Items[0].Favorited != 400 || v.Items[0].VoteScore != 0.97 || v.Items[0].VotesTotal != 309 {
		t.Errorf("第一条星星字段 = %d/%v/%d：%+v",
			v.Items[0].Favorited, v.Items[0].VoteScore, v.Items[0].VotesTotal, v.Items[0])
	}
	// 这一条没有 vote_data：0 分 0 票，界面据此不画星星而不是画一星。
	if v.Items[1].VoteScore != 0.5 || v.Items[1].VotesTotal != 8 {
		t.Errorf("第二条星星字段 = %v/%d", v.Items[1].VoteScore, v.Items[1].VotesTotal)
	}
	// 整批入库，不只命中的：下次按名字搜"实用工具"那条也该搜得到。
	if _, ok, _ := f.srv.Store.GetSeen("103"); !ok {
		t.Error("这一批里没命中分类的条目没进缓存，白筛一次就丢一次信息")
	}
	if _, ok, _ := f.srv.Store.GetSeen("104"); ok {
		t.Error("被 ban 的条目进了缓存，下次离线还会把它当可选模组列出来")
	}
}

// TestModSearchLiveTagPagesInsideTheBatch 盯的是"分类那一屏的下一页"：
// 筛完剩两条时，size=1 就得真的能翻到第二条，而最后一页不能挂着按钮。
func TestModSearchLiveTagPagesInsideTheBatch(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sampleBatch) })

	first, _ := search(t, f, "?q=新人物&tag=character&size=1&page=1")
	if len(first.Items) != 1 || first.Items[0].WorkshopID != "101" {
		t.Fatalf("第 1 页 = %+v", first.Items)
	}
	if !first.HasMore {
		t.Error("只放了一条、批里有两条却说没有下一页，第二条永远看不到")
	}
	// 带分类那一屏的 total 是面板在这一批里筛出来的命中数，Note 绝不能把它说成
	// "工坊那边还有更多（约 N 条）"——同一句里前一段刚讲完"不是总数"，后一段又拿它当总数，
	// 界面等于自己拆自己的台。
	if strings.Contains(first.Note, "工坊那边还有更多") {
		t.Errorf("把批次里的命中数当成了工坊总数：%q", first.Note)
	}
	if !strings.Contains(first.Note, "下一页还是这一次取回的那一批") {
		t.Errorf("翻页口径没说清：%q", first.Note)
	}
	second, _ := search(t, f, "?q=新人物&tag=character&size=1&page=2")
	if len(second.Items) != 1 || second.Items[0].WorkshopID != "102" {
		t.Fatalf("第 2 页 = %+v，期望 102", second.Items)
	}
	if second.HasMore {
		t.Error("最后一页还报有下一页")
	}
	if second.Total != 2 {
		t.Errorf("第 2 页 total = %d，两页该是同一个数", second.Total)
	}
	// 越界的一页回空的而不是 500：手动改 page 时踩到边界不该把整屏打挂。
	beyond, _ := search(t, f, "?q=新人物&tag=character&size=1&page=9")
	if len(beyond.Items) != 0 || beyond.HasMore {
		t.Errorf("越界一页 = %+v hasMore=%v", beyond.Items, beyond.HasMore)
	}
	// 命中 0 条时那句"换关键词"必须出现，否则这一屏只剩一片空白。
	empty, _ := search(t, f, "?q=新人物&tag=pet")
	if len(empty.Items) != 0 || empty.Total != 0 {
		t.Errorf("pet 这一类在这批里该是 0 条：%+v", empty.Items)
	}
	if !strings.Contains(empty.Note, "换") || !strings.Contains(empty.Note, "不限分类") {
		t.Errorf("0 命中要给下一步：%q", empty.Note)
	}
}

// TestModSearchTagWithoutKeyword：只选分类、不填关键词是合法的（实测接口不带
// search_text 也回一批），这一屏的口径同样得说清是"这一批"。
func TestModSearchTagWithoutKeyword(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	var sawSearchText bool
	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		sawSearchText = r.URL.Query().Get("search_text") != ""
		fmt.Fprint(w, sampleBatch)
	})
	v, _ := search(t, f, "?tag=utility")
	if sawSearchText {
		t.Error("没给关键词却硬塞了一个 search_text")
	}
	if !v.Live || len(v.Items) != 2 {
		t.Fatalf("在线按分类浏览没走通：%+v", v)
	}
	if v.TagLabel != "实用工具" {
		t.Errorf("分类中文名 = %q", v.TagLabel)
	}
	// 199 那条缓存里也是 utility 口径之外的：不能因为"这一屏还没满"就掺进来。
	for _, it := range v.Items {
		if it.Source != "live" {
			t.Errorf("掺进了 %s（%+v）", it.Source, it)
		}
	}
}

// TestModSearchCacheTagFilterAndFacets 是没配 key 那台机器上的一屏：
// 分类、总数、条数全都来自本地缓存这一个口径，三处必须互相咬合。
func TestModSearchCacheTagFilterAndFacets(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	seedCache(t, f,
		store.SeenMod{WorkshopID: "1", Name: "扩容堆叠", Tags: []string{"item", "utility"},
			Subscriptions: 900, Origin: store.SeenFromWorkshop},
		store.SeenMod{WorkshopID: "2", Name: "扩容新人物", Tags: []string{"character"},
			Subscriptions: 80, Origin: store.SeenFromWorkshop},
		store.SeenMod{WorkshopID: "3", Name: "另一个环境模组", Tags: []string{"environment"},
			Subscriptions: 70, Origin: store.SeenFromWorkshop},
	)

	v, _ := search(t, f, "?q=扩容&tag=character")
	if v.Live {
		t.Fatal("没配 key 却自称在线")
	}
	if len(v.Items) != 1 || v.Items[0].WorkshopID != "2" {
		t.Fatalf("缓存按分类筛 = %+v，期望只剩扩容新人物", v.Items)
	}
	if v.Total != 1 || v.HasMore {
		t.Errorf("total/hasMore = %d/%v，期望 1/false", v.Total, v.HasMore)
	}
	if !v.CategoryCountsKnown {
		t.Error("纯缓存这一屏的条数是数得出来的")
	}
	counts := map[string]int{}
	for _, c := range v.Categories {
		counts[c.Key] = c.Count
	}
	// 计数只跟着关键词走、不跟着已选分类走：否则"实用工具"那一枚会变成 0，
	// 用户没法从人物直接跳到它。
	if counts["item"] != 1 || counts["utility"] != 1 || counts["character"] != 1 || counts["environment"] != 0 {
		t.Errorf("分类计数 = %v，期望 item/utility/character 各 1、environment 0", counts)
	}
	if !strings.Contains(v.Note, "人物") || !strings.Contains(v.Note, "缓存") {
		t.Errorf("Note 要说清这一屏是按人物筛的缓存：%q", v.Note)
	}

	// 名字和分类都对不上时那句限定语要跟着变，不能还说"名字对得上的模组"。
	none, _ := search(t, f, "?q=扩容&tag=pet")
	if len(none.Items) != 0 {
		t.Fatalf("pet 这一类该是 0 条：%+v", none.Items)
	}
	if !strings.Contains(none.Note, "分类") {
		t.Errorf("选了分类之后那句\"没有\"要带上分类：%q", none.Note)
	}
}

// TestModSearchRejectsUnknownTagInChinese：报错里只点名目，不点名（码值）。
// 用户是在"人物/环境"这一排里挑的，他没见过 character 这个拼法。
func TestModSearchRejectsUnknownTagInChinese(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	rec := do(t, h, http.MethodGet, "/api/v1/mods/search?tag=bogus", "admin", "secret")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("不认识的分类该 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
	msg := readErr(t, rec)
	for _, want := range []string{"人物", "世界生成", "留空"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错要列名目并说清留空的意思，缺 %q：%q", want, msg)
		}
	}
	for _, leak := range []string{"character", "worldgen", "utility"} {
		if strings.Contains(msg, leak) {
			t.Errorf("报错里出现了码值 %q：%q", leak, msg)
		}
	}
	// 大小写和空格都认：这是同一排标签的另一种打字方式，不该拒。
	for _, ok := range []string{"?tag=Character", "?tag=%20item%20"} {
		rec := do(t, h, http.MethodGet, "/api/v1/mods/search"+ok, "admin", "secret")
		if rec.Code != http.StatusOK {
			t.Errorf("%s 该认得，实际 %d：%s", ok, rec.Code, rec.Body.String())
		}
	}
}

// TestModSearchIDQueryDropsTagAndSaysSo：抄来的 ID + 选着的分类同时在场时，
// 精确查赢，而分类必须从界面上退回"不限"并把原因说出口——
// 留着一枚选中的分类却不参与筛选，是在承诺一件这一屏没做过的事。
func TestModSearchIDQueryDropsTagAndSaysSo(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[
		 {"publishedfileid":"350811795","result":1,"title":"几何防御","subscriptions":5000,
		  "tags":[{"tag":"creature"}]}]}}`)
	})
	v, _ := search(t, f, "?q=350811795&tag=character")
	if v.Tag != "" {
		t.Errorf("按 ID 精确查时分类不该还挂着：%q", v.Tag)
	}
	if len(v.Items) != 1 || v.Items[0].WorkshopID != "350811795" {
		t.Fatalf("精确查没走到：%+v", v.Items)
	}
	if !strings.Contains(v.Note, "分类筛选不参与") {
		t.Errorf("要说明分类为什么被退回：%q", v.Note)
	}
	if v.CategoryCountsKnown {
		t.Error("这一屏没有可数的样本，那条数不该报可信")
	}
	// 这一条其实带的是 creature，选的是 character：如果分类真的参与了，这里会是空的。
	// 用这一点反证"精确查不受分类影响"（而不是靠读代码想当然）。
	if !strings.Contains(v.Note, "ID") && !strings.Contains(v.Note, "精确") {
		t.Errorf("Note 里要点出这是按 ID 查：%q", v.Note)
	}
}

// TestModSearchLiveWithoutTagKeepsPagedTotal：没选分类时这一屏还是走 Steam 的分页，
// total 是工坊那边的总数——这一条钉住"分类那套口径"没有污染原来那条路。
// 同时分类那一排不许画数：那批 10 条里的分类数不能代表 3592 条。
func TestModSearchLiveWithoutTagKeepsPagedTotal(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	var gotNum string
	attachWorkshop(t, f, func(w http.ResponseWriter, r *http.Request) {
		gotNum = r.URL.Query().Get("numperpage")
		fmt.Fprint(w, sampleBatch)
	})
	v, _ := search(t, f, "?q=新人物")
	if gotNum != "10" {
		t.Errorf("没选分类时该按这一屏的大小要数据，numperpage = %q", gotNum)
	}
	if v.Total != 3592 || !v.HasMore {
		t.Errorf("total/hasMore = %d/%v，期望 3592/true（这一屏还是工坊的分页）", v.Total, v.HasMore)
	}
	if v.CategoryCountsKnown {
		t.Error("这一屏的分类数没有同一个口径的样本，不该画数")
	}
	if len(v.Categories) != len(modCategories) {
		t.Errorf("分类那一排不该少：%d 枚", len(v.Categories))
	}
	for _, c := range v.Categories {
		if c.Count != 0 {
			t.Errorf("不数的时候分类 %s 却带了 %d", c.Key, c.Count)
			break
		}
	}
	// 命中的三条里（banned 和 result!=1 已被滤掉）只有一条是人物类：
	// 没选分类时不该把另外两条藏掉。
	if len(v.Items) != 3 {
		t.Errorf("结果 %d 条：%+v", len(v.Items), v.Items)
	}
	// 这一路没筛分类，total 就是接口给的那个总数，所以那句"还有更多"该留着、
	// 而且数字要是对的上：不让它出现在这里，用户就不知道下一页还有货。
	if !strings.Contains(v.Note, "工坊那边还有更多（约 3592 条）") {
		t.Errorf("在线无分类那一屏该报出接口总数：%q", v.Note)
	}
}

// TestCacheStarsSurviveOffline：星星要跟着缓存走。
// 在线那次存过的分数，断网这一屏还得画得出来——不然同一个模组昨天有星、今天没星，
// 用户只会以为模组掉了分。
func TestCacheStarsSurviveOffline(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	seedCache(t, f, store.SeenMod{WorkshopID: "700", Name: "记住分数的模组",
		Favorited: 118000, VoteScore: 0.93, VotesTotal: 4100, Origin: store.SeenFromWorkshop})
	v, _ := search(t, f, "?q=记住")
	if len(v.Items) != 1 {
		t.Fatalf("缓存里的人物没搜到：%+v", v.Items)
	}
	it := v.Items[0]
	if it.Favorited != 118000 || it.VoteScore != 0.93 || it.VotesTotal != 4100 {
		t.Errorf("星星字段没从缓存带出来：%d/%v/%d", it.Favorited, it.VoteScore, it.VotesTotal)
	}
}

// TestModSearchDiskOnlyResultIsLabelledAsSuch：只在这台机器上见过、
// 从没在线搜过的模组，来源标签必须是"这台机器上的文件"而不是"本地缓存"。
func TestModSearchDiskOnlyResultIsLabelledAsSuch(t *testing.T) {
	f := newFixture(t, "")
	modsDir := ensureModsDir(t, f)
	if err := os.MkdirAll(filepath.Join(modsDir, "workshop-777"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modsDir, "workshop-777", "modinfo.lua"),
		[]byte(`return { name = "只在磁盘上的模组", version = "1.0" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	v, _ := search(t, f, "?q=只在磁盘")
	if len(v.Items) != 1 {
		t.Fatalf("磁盘上的模组搜不到：%+v", v.Items)
	}
	it := v.Items[0]
	// 行是从缓存读出来的，所以 source=cache；label 说清它是扫目录时见过的。
	if it.Source != "cache" || !strings.Contains(it.SourceLabel, "这台机器") {
		t.Errorf("来源 = %q/%q", it.Source, it.SourceLabel)
	}
	if !it.Downloaded || !strings.Contains(it.StatusLabel, "清单里没有") {
		t.Errorf("磁盘状态 = %+v", it)
	}
}
