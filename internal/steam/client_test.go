package steam

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 这个测试文件里的样本是 2026-10-30 用真 key 打真接口抓下来的形状
// （见 .tools/steam_probe.py），刻意保留了几个"看着像 bug 其实是接口现实"的点：
// response 里没有 success 字段、publishedfileid 有时是字符串有时是数字、
// 403 的正文是一段 HTML。哪天 Steam 改了这些，下面的断言会先响。

const testKey = "ABCDEF0123456789ABCDEF0123456789"

// 一条真实的 QueryFiles 条目：中文名靠 language=6 直接拿到，标签里带版本和"客户端也要订阅"。
const sampleQueryFiles = `{
 "response": {
  "publishedfiledetails": [
   {
    "publishedfileid": "351210508",
    "result": 1,
    "title": "更多物品堆叠 More Items Less Screens",
    "file_description": "Stack more items, fewer screens.\nAuthor note here.",
    "subscriptions": 20134,
    "favorited": 118340,
    "vote_data": {"score": 0.94, "votes_up": 3011, "votes_down": 180},
    "time_updated": 1730000000,
    "preview_url": "https://images.steamusercontent.com/ugc/123/hash/A.jpg",
    "file_size": 81234,
    "num_children": 0,
    "tags": [
      {"tag": "version:1.6", "description": ""},
      {"tag": "all_clients_require_mod", "description": ""},
      {"tag": ""}
    ]
   },
   {
    "publishedfileid": 890433445,
    "title": "Geometric Defense",
    "subscriptions": 5123,
    "favorited": "1202",
    "num_children": 2,
    "tags": [{"tag": "server_only_mod"}]
   },
   {
    "publishedfileid": "999000111",
    "title": "已经被封的模组",
    "banned": true
   }
  ],
  "total": 137
 }
}`

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(testKey, "")
	c.BaseURL = srv.URL
	return c
}

func TestSearchSendsThePinnedQueryAndParsesSample(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		fmt.Fprint(w, sampleQueryFiles)
	})

	page, err := c.Search(context.Background(), "堆叠", 2, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/IPublishedFileService/QueryFiles/v1/" {
		t.Errorf("请求路径 = %q", gotPath)
	}
	// appid 必须是本体 322330：填成专用服务端 343050 会一条都搜不到，
	// 而那看起来像"工坊上没有这个模组"。
	for key, want := range map[string]string{
		"appid":       WorkshopAppID,
		"search_text": "堆叠",
		"page":        "2",
		"numperpage":  "10",
		"language":    "6",
		"key":         testKey,
	} {
		if got := gotQuery.Get(key); got != want {
			t.Errorf("参数 %s = %q，期望 %q", key, got, want)
		}
	}
	if got := gotQuery.Get("return_tags"); got != "true" {
		t.Errorf("没要标签，客户端订阅提醒和版本就都无从判断：%q", got)
	}
	if page.Total != 137 {
		t.Errorf("total = %d，期望 137", page.Total)
	}
	// 被 ban 的那条必须被滤掉：让用户登记一个下架模组，等于替他制造一次启动失败。
	if len(page.Items) != 2 {
		t.Fatalf("返回 %d 条，期望滤掉 banned 后剩 2 条：%+v", len(page.Items), page.Items)
	}
	first := page.Items[0]
	if first.ID != "351210508" || first.Name == "" {
		t.Fatalf("第一条 = %+v", first)
	}
	if first.Version != "1.6" {
		t.Errorf("version 没从 version:1.6 标签里取出来 = %q", first.Version)
	}
	if !first.ClientRequired || first.ServerOnly {
		t.Errorf("标签判位错：client_required=%v server_only=%v", first.ClientRequired, first.ServerOnly)
	}
	if first.Subscriptions != 20134 || first.ModUpdatedAt != 1730000000 || first.FileSize != 81234 {
		t.Errorf("数字字段 = %d/%d/%d", first.Subscriptions, first.ModUpdatedAt, first.FileSize)
	}
	if len(first.Tags) != 2 {
		t.Errorf("空标签没滤掉：%v", first.Tags)
	}
	if !strings.Contains(first.Desc, "fewer screens") {
		t.Errorf("desc = %q", first.Desc)
	}
	// publishedfileid 是数字时也要变成字符串 ID，否则后面 every 一步（写 Lua、查缓存）都对不上。
	second := page.Items[1]
	if second.ID != "890433445" {
		t.Errorf("数字型 ID 解析成 %q，期望 890433445", second.ID)
	}
	if !second.ServerOnly || !second.HasChildren {
		t.Errorf("第二条标签 = %+v", second)
	}
	// 星星那几个数：favorited 在这个样本里两种写法都有（数字和字符串），
	// 而票数是 up+down 两枚字段，接口不给合计——不在这里合好，界面就得自己猜。
	if first.Favorited != 118340 {
		t.Errorf("favorited = %d，期望 118340", first.Favorited)
	}
	if first.VoteScore != 0.94 || first.VotesTotal != 3191 {
		t.Errorf("好评率/投票数 = %v / %d，期望 0.94 / 3191（3011 上 + 180 下）", first.VoteScore, first.VotesTotal)
	}
	if second.Favorited != 1202 {
		t.Errorf("字符串写法的 favorited 解析成 %d，期望 1202", second.Favorited)
	}
	// 这一条没有 vote_data：分数必须是 0（界面据此不画星星），不能编出一个"好评率 0%"。
	if second.VoteScore != 0 || second.VotesTotal != 0 {
		t.Errorf("没有投票数据的条目被编出了分数：%v / %d", second.VoteScore, second.VotesTotal)
	}
}

// TestBrowseTakesOneBatchAndNeedsNoKeyword 钉的是分类那条路的地基。
// 两件事都是实测出来的（.tools/tmp/tag_vocab_probe.py）：numperpage=100 真回 100 条，
// 以及不带给 search_text 时接口照样回一批——没有这两条，"只选分类、不填关键词"
// 这一档就只能做成一个点了没反应的按钮。
func TestBrowseTakesOneBatchAndNeedsNoKeyword(t *testing.T) {
	var gotQuery url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		fmt.Fprint(w, sampleQueryFiles)
	})
	items, err := c.Browse(context.Background(), "", 500)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if gotQuery.Get("numperpage") != "100" {
		t.Errorf("numperpage = %q，接口上限是 100，要多就得多打几次（我们把筛的范围如实回给界面）",
			gotQuery.Get("numperpage"))
	}
	if gotQuery.Get("search_text") != "" {
		t.Errorf("没关键词时不该塞一个搜索词进去：%q", gotQuery.Get("search_text"))
	}
	if gotQuery.Get("page") != "1" {
		t.Errorf("page = %q，一批只取第一页", gotQuery.Get("page"))
	}
	if gotQuery.Get("return_tags") != "true" || gotQuery.Get("return_vote_data") != "true" {
		t.Error("分类靠标签、星星靠投票，这两个 return_ 参数少一个那一栏就是空的")
	}
	// 我们不向接口要标签筛选：实测 matched_tag / exclude_tag / required_tags 全被忽略，
	// 带上它只会让人以为筛过，而 Steam 那一头压根没看。
	for _, k := range []string{"matched_tag", "exclude_tag", "required_tags"} {
		if gotQuery.Get(k) != "" {
			t.Errorf("发了一个接口不认的参数 %s，等于在假装按分类筛过", k)
		}
	}
	if len(items) != 2 {
		t.Fatalf("返回 %d 条：%+v", len(items), items)
	}
	if items[0].Favorited != 118340 {
		t.Errorf("Browse 这条路没带上收藏数：%+v", items[0])
	}
}

func TestBrowseRejectsDisabledClientInChinese(t *testing.T) {
	c := New("", "")
	if _, err := c.Browse(context.Background(), "x", 100); err == nil {
		t.Fatal("没配 key 时 Browse 不该真发请求")
	} else if !strings.Contains(err.Error(), "steamApi.webApiKey") {
		t.Errorf("要说清去配哪一项：%q", err.Error())
	}
}

func TestSearchClampsSillyPaging(t *testing.T) {
	var gotNum, gotPage string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotNum = r.URL.Query().Get("numperpage")
		gotPage = r.URL.Query().Get("page")
		fmt.Fprint(w, sampleQueryFiles)
	})
	if _, err := c.Search(context.Background(), "x", 0, 500); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotNum != "10" {
		t.Errorf("numperpage = %q，越界应该退回默认的 10（一屏超过 20 条界面也放不下）", gotNum)
	}
	if gotPage != "1" {
		t.Errorf("page = %q，page<1 应该退回 1", gotPage)
	}
}

// TestBadKeyIsDistinguishable 是这套降级逻辑里最值钱的一条：
// 403 的下一步是去换 key，超时和限流的下一步是等一会儿，
// 两者在界面上必须分开说，否则用户会一直点重试。
func TestBadKeyIsDistinguishable(t *testing.T) {
	cases := []struct {
		code      int
		body      string
		wantIs    error
		wantWords []string
	}{
		{http.StatusForbidden, `<html>Access is denied. Retrying will not help. Please verify your key= parameter.</html>`,
			ErrBadKey, []string{"key", "重新生成"}},
		{http.StatusUnauthorized, `{"response":{"error":"unauthorized"}}`, ErrBadKey, []string{"key"}},
		{http.StatusTooManyRequests, `{"response":{}}`, nil, []string{"限流"}},
		{http.StatusBadGateway, `{"response":{}}`, nil, []string{"HTTP 502"}},
	}
	for _, tc := range cases {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			fmt.Fprint(w, tc.body)
		})
		_, err := c.Search(context.Background(), "mod", 1, 10)
		if err == nil {
			t.Fatalf("HTTP %d 居然算成功：%v", tc.code, err)
		}
		if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
			t.Errorf("HTTP %d 没有认出 %v，err = %v", tc.code, tc.wantIs, err)
		}
		msg := err.Error()
		if isASCII(msg) {
			t.Errorf("HTTP %d 的结论是纯英文，不能上界面：%q", tc.code, msg)
		}
		if strings.Contains(msg, testKey) {
			t.Errorf("HTTP %d 的结论里带了 key：%q", tc.code, msg)
		}
		for _, word := range tc.wantWords {
			if !strings.Contains(msg, word) {
				t.Errorf("HTTP %d 的结论里缺 %q：%q", tc.code, word, msg)
			}
		}
	}
}

func TestNonJSONBodyIsReportedNotPaniced(t *testing.T) {
	// 网络出口被改写的样子：200 + 一段门户页面的 HTML。
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>上网认证页面</body></html>`)
	})
	_, err := c.Search(context.Background(), "mod", 1, 10)
	if err == nil {
		t.Fatal("HTML 当成 JSON 解析成功了？")
	}
	if !strings.Contains(err.Error(), "不是预期的 JSON") {
		t.Errorf("err = %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Cause == nil {
		t.Errorf("错误没带原始原因，日志里查不到：%-v", err)
	}
}

// TestKeyNeverLeavesTheClient 盯的是这个包里最容易出的安全事故：
// Go 的网络错误文本形如 Get "http://host/path?key=SECRET": dial tcp ...，
// 原样返回就等于把凭据发到界面上。
func TestKeyNeverLeavesTheClient(t *testing.T) {
	c := New(testKey, "")
	c.BaseURL = "http://127.0.0.1:1" // 立刻拒绝连接
	_, err := c.Search(context.Background(), "mod", 1, 10)
	if err == nil {
		t.Fatal("连不上却成功了")
	}
	for _, s := range []string{err.Error(), fmt.Sprintf("%+v", err)} {
		if strings.Contains(s, testKey) {
			t.Errorf("错误文本里出现了 key：%q", s)
		}
	}
	if !strings.Contains(err.Error(), "api.steampowered.com") {
		t.Errorf("结论应该提到域名和代理这条路：%q", err.Error())
	}
	// 原始原因留着写日志，但同样必须洗过。
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("不是 *Error：%T", err)
	}
	if strings.Contains(fmt.Sprint(e.Cause), testKey) {
		t.Errorf("Cause 里漏了 key：%q", e.Cause)
	}
}

func TestScrubKeyHandlesBothForms(t *testing.T) {
	in := `Get "https://api.steampowered.com/x?key=SECRET123&appid=1": dial tcp 1.2.3.4:443`
	got := scrubKey(in)
	if strings.Contains(got, "SECRET123") {
		t.Errorf("scrubKey 没洗掉： %q", got)
	}
	if !strings.Contains(got, "key=***") {
		t.Errorf("洗得太狠，看不出哪个参数了：%q", got)
	}
	if scrubKey("") != "" {
		t.Error("空串不该变出内容")
	}
}

func TestDisabledClientDegradesWithUsefulChinese(t *testing.T) {
	var nilClient *Client
	if nilClient.Enabled() {
		t.Error("nil 客户端不该算启用")
	}
	noKey := New("   ", "")
	if noKey.Enabled() {
		t.Error("只有空格也算有 key？")
	}
	_, err := noKey.Search(context.Background(), "mod", 1, 10)
	if err == nil || !strings.Contains(err.Error(), "steamApi.webApiKey") {
		t.Errorf("没 key 的错误要告诉用户去配哪一项：%v", err)
	}
	if _, err := nilClient.Details(context.Background(), "351210508"); err == nil {
		t.Error("nil 客户端调 Details 不该成功")
	} else if !strings.Contains(err.Error(), "登记本身不受影响") {
		t.Errorf("要说明登记不需要 key：%q", err)
	}
	// 空关键词不再被拒绝：它就是 #76 那条需求本身（"我不知道要什么模组，只想一页一页翻"）。
	// 云机真接口实测（.tools/workshop_browse_probe.py，2026-11-03）：不带 search_text 时
	// QueryFiles 回工坊全量榜（total 27313），page=1/2/3 三页 ID 零重叠；
	// 而 offset / start / sort_by 三个参数全被忽略，所以翻页只能用 page，别的路都是假的。
	var emptyQ url.Values
	fake := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		emptyQ = r.URL.Query()
		fmt.Fprint(w, sampleQueryFiles)
	})
	if _, err := fake.Search(context.Background(), "   ", 3, 12); err != nil {
		t.Fatalf("空关键词现在是要打接口的，不该报错：%v", err)
	}
	if _, ok := emptyQ["search_text"]; ok {
		t.Errorf("空关键词不该把 search_text 发出去（探针就是这么发的）：%v", emptyQ)
	}
	if emptyQ.Get("page") != "3" || emptyQ.Get("numperpage") != "12" {
		t.Errorf("翻页只能靠 page/numperpage，实际发出去的是 %v", emptyQ)
	}
}

func TestDetailsRejectsGarbageIDsButAcceptsBothWidths(t *testing.T) {
	var seen []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = []string{}
		for k, v := range r.URL.Query() {
			if strings.HasPrefix(k, "publishedfileids[") {
				seen = append(seen, v[0])
			}
		}
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[
		  {"publishedfileid":"350811795","result":1,"title":"几何防御","num_children":0},
		  {"publishedfileid":"400000001","result":2,"error":"Item not found"}
		]}}`)
	})
	items, err := c.Details(context.Background(), " 350811795 ", `1); os.execute("rm -rf /")`, "400000001", "", "abc")
	if err != nil {
		t.Fatalf("Details: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("返回 %d 条：%+v（result!=1 的条目必须滤掉）", len(items), items)
	}
	if items[0].Name != "几何防御" {
		t.Errorf("name = %q", items[0].Name)
	}
	joined := strings.Join(seen, ",")
	if !strings.Contains(joined, "3512105") && !strings.Contains(joined, "350811795") {
		t.Errorf("请求里没带上合法 ID：%q", joined)
	}
	if strings.Contains(joined, "os.execute") {
		t.Errorf("非法 ID 被拼进了请求：%q", joined)
	}
	if _, err := c.Details(context.Background(), "abc", ""); err == nil {
		t.Error("全是非法 ID 时不该发请求")
	}
}

func TestSearchHonoursContextCancellation(t *testing.T) {
	c := New(testKey, "")
	c.BaseURL = ""
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	// 用真实域名但极短的超时：这条断言关心的是"取消要认得出来，而且结论是中文"。
	out, err := c.Search(ctx, "mod", 1, 10)
	if err == nil {
		t.Fatalf("超时却成功了：%+v", out)
	}
	if isASCII(err.Error()) {
		t.Errorf("超时结论是英文的：%q", err)
	}
	// 上层靠这一条区分"用户自己关页面"和"机房出不去"，说错就是假诊断。
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("取消/超时没被认出来：%v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("错误里漏了 key：%q", err)
	}
}

func TestCanceledRequestIsNotReportedAsNetworkFailure(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 拖到调用方取消
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Search(ctx, "mod", 1, 10)
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	err := <-done
	if err == nil {
		t.Fatal("取消了却返回成功")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("认不出 context.Canceled：%v", err)
	}
	if strings.Contains(err.Error(), "直连") || strings.Contains(err.Error(), "api.steampowered.com") {
		t.Errorf("把用户取消说成了机房网络故障：%q", err)
	}
}

func TestUsableAndEmptyItems(t *testing.T) {
	if !usable(rawItem{ID: "1"}) {
		t.Error("QueryFiles 不带 result 字段，result=0 必须算可用")
	}
	if usable(rawItem{ID: "1", Result: 2}) || usable(rawItem{ID: "1", Banned: true}) ||
		usable(rawItem{ID: "  "}) {
		t.Error("不可用的条目被判成可用")
	}
	// 空结果集要能出空列表而不是报错：搜冷门词是正常输入。
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"response":{"publishedfiledetails":[],"total":0}}`)
	})
	page, err := c.Search(context.Background(), "zzzzz不存在", 1, 10)
	if err != nil {
		t.Fatalf("空结果报错了：%v", err)
	}
	if len(page.Items) != 0 || page.Total != 0 {
		t.Errorf("page = %+v", page)
	}
}

func TestFlexIDAcceptsStringNumberAndNull(t *testing.T) {
	cases := map[string]string{`"123"`: "123", `123`: "123", `null`: "", `0`: "0"}
	for raw, want := range cases {
		var f flexID
		if err := f.UnmarshalJSON([]byte(raw)); err != nil {
			t.Errorf("%s 解析失败：%v", raw, err)
			continue
		}
		if string(f) != want {
			t.Errorf("%s → %q，期望 %q", raw, f, want)
		}
	}
	var f flexID
	if err := f.UnmarshalJSON([]byte(`"数字串"`)); err != nil {
		t.Logf("非数字字符串 ID 走的是字符串分支，不是错误：%v", err)
	}
	if err := f.UnmarshalJSON([]byte(`{}`)); err == nil {
		t.Error("对象形态的 ID 应该报错，不能悄悄变成空串")
	}
}

// TestFlexInt64AcceptsBothShapes 钉住 2026-11-01 真接口上撞到的事：
// file_size 回来的是字符串。整页按 int64 解会失败，而失败被当成"在线检索没成功"，
// 于是一次成功的检索在界面上显示成降级——最坏的那种坏法。
func TestFlexInt64AcceptsBothShapes(t *testing.T) {
	cases := map[string]int64{`123`: 123, `"456"`: 456, `null`: 0, `""`: 0, `" 789 "`: 789}
	for raw, want := range cases {
		var n flexInt64
		if err := n.UnmarshalJSON([]byte(raw)); err != nil {
			t.Errorf("%s 解析失败：%v", raw, err)
			continue
		}
		if int64(n) != want {
			t.Errorf("%s → %d，期望 %d", raw, n, want)
		}
	}
	for _, raw := range []string{`{}`, `"数字串"`} {
		var n flexInt64
		if err := n.UnmarshalJSON([]byte(raw)); err == nil {
			t.Errorf("%s 不该被当成数字静默收下", raw)
		}
	}

	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"response":{"total":7,"publishedfiledetails":[{
			"publishedfileid":"351210508","title":"字符串数字样本","result":1,
			"subscriptions":"20134","time_updated":"1730000000",
			"file_size":"81234","num_children":"2","preview_url":"https://img.example/a.jpg",
			"tags":[{"tag":"version:1.6"}]}]}}`)
	})
	page, err := c.Search(context.Background(), "样本", 1, 10)
	if err != nil {
		t.Fatalf("数字写成字符串就不该判失败：%v", err)
	}
	if page.Total != 7 || len(page.Items) != 1 {
		t.Fatalf("page = %+v", page)
	}
	it := page.Items[0]
	if it.Subscriptions != 20134 || it.ModUpdatedAt != 1730000000 || it.FileSize != 81234 {
		t.Errorf("数字字段没解出来：%+v", it)
	}
	if !it.HasChildren {
		t.Errorf("num_children 写成字符串 \"2\" 也要认出子模组：%+v", it)
	}
	if it.PreviewURL == "" {
		t.Error("预览图地址丢了")
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}
