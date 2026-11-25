package lobby

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dstpanel/internal/modbridge"
)

// bucketJSON 造一份和真桶同形的正文：顶层只有 GET 一个键，值是房间数组。
func bucketJSON(t *testing.T, rooms ...map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"GET": rooms})
	if err != nil {
		t.Fatalf("造夹具失败: %v", err)
	}
	return raw
}

func room(name, addr string, port, connected, max int) map[string]any {
	return map[string]any{
		"name": name, "__addr": addr, "port": port,
		"connected": connected, "maxconnections": max,
		"intent": "survival", "season": "autumn", "tags": "caves,show",
		"password": false, "allownewplayers": true,
		// 这一对实测在真记录里同时为 true，面板不解释它们（见 Room 的注释），
		// 但解 JSON 时它们必须在，否则等于没测到"字段比我们知道的多"这一种。
		"dedicated": true, "clienthosted": true, "fo": false, "v": 740477,
	}
}

func newTestFetcher(url string) *Fetcher {
	f := NewFetcher("", 5*time.Second)
	f.BaseURL = url
	return f
}

func TestFetchAcceptsGzipWithoutContentEncoding(t *testing.T) {
	// 真桶的形状：正文是 gzip，响应头却写 Content-Type: application/json、
	// 没有 Content-Encoding。这是 2026-11-03 实测到的，也是这个包必须自己认魔数的原因。
	// Go 的 Transport 只在响应头写了 Content-Encoding 时才帮忙解，这里它帮不上。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		zw := gzip.NewWriter(w)
		zw.Write(bucketJSON(t, room("压缩的", "8.8.8.8", 10999, 3, 6)))
		zw.Close()
	}))
	defer srv.Close()

	snap, err := newTestFetcher(srv.URL).Fetch(context.Background(), "ap-southeast-1")
	if err != nil {
		t.Fatalf("gzip 正文该自己解开: %v", err)
	}
	if len(snap.Rooms) != 1 || snap.Rooms[0].Players != 3 || snap.Rooms[0].Free != 3 {
		t.Fatalf("内容不对：%+v", snap.Rooms)
	}
	if snap.Total != 1 || snap.Decoded <= 0 {
		t.Errorf("原始条数与解压后大小要在（界面要说\"从 N 间里筛出 M 间\"）：%+v", snap)
	}
	if snap.Region != "ap-southeast-1" || snap.FetchedAt.IsZero() {
		t.Errorf("快照该带着它是哪个大区、几点取到的：%+v", snap)
	}
}

func TestParseAndFilter(t *testing.T) {
	// room() 给的是"什么都没挡"的一条，这里单独把那间有密码的改出来：
	// 门槛那一列的判断全靠 password 这一位，夹具里全 false 等于没测。
	locked := room("有门槛的房间", "1.2.3.7", 10999, 0, 6)
	locked["password"] = true
	raw := bucketJSON(t,
		room("好的房间", "1.2.3.4", 10999, 2, 6),
		room("满员的房间", "1.2.3.5", 10999, 6, 6),
		room("没地址的房间", "", 10999, 0, 6),
		room("没端口的房间", "1.2.3.6", 0, 0, 6),
		map[string]any{"name": "缺字段的一整条"},
		locked,
	)
	var doc bucket
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解不开夹具: %v", err)
	}
	snap := &Snapshot{Region: "ap-southeast-1", FetchedAt: time.Now(), Total: len(doc.GET)}
	for _, rr := range doc.GET {
		if r, ok := rr.toRoom(); ok {
			snap.Rooms = append(snap.Rooms, r)
		}
	}
	if len(snap.Rooms) != 3 {
		t.Fatalf("可用的房间该是 3 间（滤掉没地址/没端口/整条缺字段的），实际 %d：%+v", len(snap.Rooms), snap.Rooms)
	}
	if snap.Rooms[0].Free != 4 {
		t.Errorf("空位该由人数与上限算出来，实际 %+v", snap.Rooms[0])
	}
	if snap.Rooms[0].IntentLabel != "生存" || snap.Rooms[0].SeasonLabel != "秋天" {
		t.Errorf("中文标签没跟上：%+v", snap.Rooms[0])
	}
	if !snap.Rooms[0].FullAccess || snap.Rooms[2].FullAccess {
		t.Errorf("门槛该由密码/战队/局域网/收不收新人合算：%+v %+v", snap.Rooms[0], snap.Rooms[2])
	}
	if strings.Join(snap.Rooms[0].Tags, ",") != "caves,show" {
		t.Errorf("标签该原样传下去：%v", snap.Rooms[0].Tags)
	}
	if snap.WithPlayers() != 2 {
		t.Errorf("有人的房间该是 2 间，实际 %d", snap.WithPlayers())
	}

	page := snap.Apply(Query{Sort: "free", Size: 2})
	if page.Total != 3 || page.Pages != 2 || len(page.Rooms) != 2 {
		t.Fatalf("分页算错：total=%d pages=%d 这一页 %d 条", page.Total, page.Pages, len(page.Rooms))
	}
	if page.Rooms[0].Name != "有门槛的房间" || page.Rooms[1].Name != "好的房间" {
		t.Errorf("按空位排该把 6 个空位那间摆最前，实际 %+v", page.Rooms)
	}
	// 第二页必须是同一份排序的下一段，不是重新切一刀。
	page2 := snap.Apply(Query{Sort: "free", Size: 2, Page: 2})
	if len(page2.Rooms) != 1 || page2.Rooms[0].Name != "满员的房间" {
		t.Errorf("第二页该剩那间满员的，实际 %+v", page2.Rooms)
	}

	if got := snap.Apply(Query{Text: "好的", Size: 10}).Total; got != 1 {
		t.Errorf("按名字筛「好的」该命中 1 间，实际 %d", got)
	}
	if got := snap.Apply(Query{Slots: "free", Size: 10}).Total; got != 2 {
		t.Errorf("只看有空位该 2 间（满员那间排除），实际 %d", got)
	}
	if got := snap.Apply(Query{Players: "has", Size: 10}).Total; got != 2 {
		t.Errorf("只看有人的该 2 间，实际 %d", got)
	}
	if got := snap.Apply(Query{Access: "locked", Size: 10}).Total; got != 1 {
		t.Errorf("只看有门槛的该 1 间（那间有密码），实际 %d", got)
	}
	// 页码超界钳到最后一页，而不是给一张空表：见 Apply 里那段理由。
	over := snap.Apply(Query{Size: 10, Page: 99})
	if over.Page != 1 || len(over.Rooms) != 3 {
		t.Errorf("超界页码该钳回最后一页，实际 page=%d 条数=%d", over.Page, len(over.Rooms))
	}
	// 一条都没命中时 Rooms 必须是空数组：nil 进 JSON 是 null，前端读 .length 就崩。
	none := snap.Apply(Query{Text: "不可能存在的名字", Size: 10})
	if none.Rooms == nil || none.Total != 0 || none.Pages != 1 {
		t.Errorf("筛空了也要给出得了解释的形状：%+v", none)
	}
}

func TestIntentLabel(t *testing.T) {
	if got := IntentLabel("oceanfishing"); got != "海洋垂钓" {
		t.Errorf("intent 没翻：%s", got)
	}
	// 没收录的取值原样返回：那是房主上报的一个词，不是面板自己的码值。
	if got := IntentLabel("super_afk"); got != "super_afk" {
		t.Errorf("陌生取值该原样传下去，实际 %q", got)
	}
	if got := IntentLabel("  "); got != "" {
		t.Errorf("空值该留空，实际 %q", got)
	}
	if got := modbridge.SeasonLabel("winter"); got != "冬天" {
		t.Errorf("季节中文没跟上（用的是 modbridge 那一张表）：%s", got)
	}
}

func TestRegionWhitelist(t *testing.T) {
	// 打的是自己：URL 由 BaseURL 拼，但路径那一段永远来自白名单。
	var hit atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(r.URL.Path)
		w.Write(bucketJSON(t))
	}))
	defer srv.Close()
	f := newTestFetcher(srv.URL)

	if _, err := f.Fetch(context.Background(), "../../etc/passwd"); err == nil {
		t.Fatal("路径穿越样的地区名该被白名单挡住")
	}
	if p, _ := hit.Load().(string); p != "" {
		t.Errorf("非法地区名不该发出任何请求，实际打到了 %s", p)
	}
	if _, err := f.Fetch(context.Background(), "ap-southeast-1"); err != nil {
		t.Fatalf("合法地区该取到: %v", err)
	}
	if p, _ := hit.Load().(string); p != "/ap-southeast-1-Steam.json.gz" {
		t.Errorf("请求路径形状不对：%s", p)
	}
	if len(Regions()) != 3 {
		t.Errorf("地区表该是实测到的那三个：%+v", Regions())
	}
}

func TestCacheTTLAndStaleFallback(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Write(bucketJSON(t, room("第一版", "1.2.3.4", 10999, 1, 6)))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewCache(newTestFetcher(srv.URL), time.Hour, time.Second, true)
	res, err := c.Get(context.Background(), "ap-southeast-1")
	if err != nil || res.Stale || len(res.Snap.Rooms) != 1 {
		t.Fatalf("第一次该拿到新列表: %+v err=%v", res, err)
	}
	// TTL 内不再打网络。
	if _, err := c.Get(context.Background(), "ap-southeast-1"); err != nil {
		t.Fatalf("缓存命中不该失败: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("TTL 内还打了网络，calls=%d", got)
	}

	// 过了 TTL：这次刷新失败，但旧数据必须照样给出来并明说是旧的。
	c.TTL = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	res, err = c.Get(context.Background(), "ap-southeast-1")
	if err != nil {
		t.Fatalf("有旧数据时不该整体失败: %v", err)
	}
	if !res.Stale || res.WhyStale == "" || len(res.Snap.Rooms) != 1 {
		t.Fatalf("失败时要留旧数据并给原因，实际 %+v", res)
	}
	if !strings.Contains(res.WhyStale, "HTTP 500") {
		t.Errorf("失败原因里要带上接口给的状态码，实际 %q", res.WhyStale)
	}
	// 冷却期内不再撞网络：直接复用上一条失败结论。
	before := atomic.LoadInt32(&calls)
	res, err = c.Get(context.Background(), "ap-southeast-1")
	if err != nil || !res.Stale {
		t.Fatalf("冷却期内该继续给旧数据: %+v err=%v", res, err)
	}
	if atomic.LoadInt32(&calls) != before {
		t.Errorf("冷却期内又打了一次网络")
	}
}

// 「刷新」按钮走 GetForce：它必须真的再打一次网络，哪怕 TTL 还没到、哪怕上一次刚失败。
// 没有这条路的话，界面上那颗按钮在 TTL 内按下去只会拿到同一份数据——等于给用户一个
// 按下去什么都不发生的按钮，而这一页用户最常问的就是"现在还有没有人在"。
func TestCacheForceSkipsTTLAndCooldown(t *testing.T) {
	var calls int32
	var broken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		switch {
		case broken.Load():
			http.Error(w, "boom", http.StatusInternalServerError)
		case n == 1:
			w.Write(bucketJSON(t, room("第一版", "1.2.3.4", 10999, 1, 6)))
		default:
			w.Write(bucketJSON(t, room("第二版", "1.2.3.4", 10999, 4, 6)))
		}
	}))
	defer srv.Close()

	// TTL 和冷却都给满小时：普通 Get 在这段里一次也不会自己打第二次网络。
	c := NewCache(newTestFetcher(srv.URL), time.Hour, time.Hour, true)
	res, err := c.Get(context.Background(), "ap-southeast-1")
	if err != nil || res.Snap.Rooms[0].Name != "第一版" {
		t.Fatalf("第一次 Get：%+v err=%v", res.Snap, err)
	}
	if _, err := c.Get(context.Background(), "ap-southeast-1"); err != nil {
		t.Fatalf("缓存命中：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("TTL 内 Get 打了网络，calls=%d", got)
	}

	res, err = c.GetForce(context.Background(), "ap-southeast-1")
	if err != nil || res.Stale {
		t.Fatalf("强制刷新该真取一次：%+v err=%v", res, err)
	}
	if res.Snap.Rooms[0].Name != "第二版" || res.Snap.Rooms[0].Players != 4 {
		t.Errorf("强制刷新该换到新数据：%+v", res.Snap.Rooms[0])
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("强制刷新该打一次网络，calls=%d", got)
	}

	// 冷却期内按「刷新」：照样撞一次网络，失败后把上一版带着 stale 给出去。
	broken.Store(true)
	res, err = c.GetForce(context.Background(), "ap-southeast-1")
	if err != nil {
		t.Fatalf("有旧数据时强制刷新失败也不该整体报错：%v", err)
	}
	if !res.Stale || !strings.Contains(res.WhyStale, "HTTP 500") {
		t.Errorf("该标成旧的并给出原因：%+v", res)
	}
	if res.Snap.Rooms[0].Name != "第二版" {
		t.Errorf("旧数据不能因为刷新失败就变空：%+v", res.Snap.Rooms[0])
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("强制刷新不该被冷却挡住，calls=%d", got)
	}
	// 而随后的自动轮询仍然吃这条冷却。
	before := atomic.LoadInt32(&calls)
	if _, err := c.Get(context.Background(), "ap-southeast-1"); err != nil {
		t.Fatalf("冷却期该继续给旧数据: %v", err)
	}
	if atomic.LoadInt32(&calls) != before {
		t.Errorf("冷却期内自动轮询又去撞网络了")
	}
}

func TestCacheDisabledAndNoData(t *testing.T) {
	c := NewCache(NewFetcher("", time.Second), time.Minute, time.Second, false)
	_, err := c.Get(context.Background(), "ap-southeast-1")
	if err == nil || !strings.Contains(err.Error(), "lobby.enabled") {
		t.Fatalf("关掉时该说出是哪个开关：%v", err)
	}
	// 开着但从没成功过：这时候没有旧数据可给，只能如实失败。
	var failed int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&failed, 1)
		w.Write([]byte("这不是 JSON"))
	}))
	defer srv.Close()
	c2 := NewCache(newTestFetcher(srv.URL), time.Minute, 30*time.Second, true)
	if _, err := c2.Get(context.Background(), "ap-southeast-1"); err == nil {
		t.Fatal("解不开正文时必须失败，不能给一个空列表假装正常")
	} else if !strings.Contains(err.Error(), "不是预期的 JSON") {
		t.Errorf("中文原因不对：%v", err)
	}
	// 冷却期内第二次不该再打网络。
	if _, err := c2.Get(context.Background(), "ap-southeast-1"); err == nil {
		t.Fatal("同上")
	}
	if got := atomic.LoadInt32(&failed); got != 1 {
		t.Errorf("失败后该停在冷却里，实际打了 %d 次", got)
	}
}

func TestFetchSizeCap(t *testing.T) {
	// 上限之内：造一份 4 万条的列表（解压后约 4 MB），正常解出来。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf strings.Builder
		buf.WriteString(`{"GET":[`)
		for i := 0; i < 40000; i++ {
			if i > 0 {
				buf.WriteString(",")
			}
			buf.WriteString(`{"name":"一遍一遍一遍","__addr":"1.2.3.4","port":10999}`)
		}
		buf.WriteString(`]}`)
		zw := gzip.NewWriter(w)
		zw.Write([]byte(buf.String()))
		zw.Close()
	}))
	defer srv.Close()
	if _, err := newTestFetcher(srv.URL).Fetch(context.Background(), "us-east-1"); err != nil {
		t.Fatalf("这一份还在上限内，该正常解出来：%v", err)
	}

	// 超过上限（gzip bomb 的最小形态：同一个字节重复到 56 MB，比 48 MB 的上限多一截）：
	// 必须拦住，而不是先把 4G 小机的内存吃穿再报错。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zw := gzip.NewWriter(w)
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < 56; i++ {
			if _, err := zw.Write([]byte(chunk)); err != nil {
				return
			}
		}
		zw.Close()
	}))
	defer srv2.Close()
	_, err := newTestFetcher(srv2.URL).Fetch(context.Background(), "us-east-1")
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超过上限要被拦住并说清，实际 %v", err)
	}
}

func TestFetchPlainJSONAlsoWorks(t *testing.T) {
	// 没有 gzip 魔数的正文（将来 Klei 改成不压、或中间某层代理替我们解了）也要照解。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"GET":[{"name":"直接的","__addr":"9.9.9.9","port":11000,"connected":1,"maxconnections":6}]}`)
	}))
	defer srv.Close()
	snap, err := newTestFetcher(srv.URL).Fetch(context.Background(), "eu-central-1")
	if err != nil {
		t.Fatalf("纯 JSON 该直接解: %v", err)
	}
	if len(snap.Rooms) != 1 || snap.Rooms[0].Addr != "9.9.9.9" {
		t.Fatalf("内容不对：%+v", snap.Rooms)
	}
}

func TestFetchNetworkErrorIsChinese(t *testing.T) {
	f := NewFetcher("", 200*time.Millisecond)
	f.BaseURL = "https://127.0.0.1:1" // 立刻拒绝连接
	c, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := f.Fetch(c, "ap-southeast-1")
	if err == nil {
		t.Fatal("连不上时该有错误")
	}
	if !strings.Contains(err.Error(), "连不上 Klei") || !strings.Contains(err.Error(), "lobby.proxy") {
		t.Errorf("要给一句能照着做的中文，并点出代理那一条：%v", err)
	}
	// 原始原因也要留着，但只留引号后面那一段——完整 URL 复读一遍既没用又可能带上代理地址。
	if strings.Contains(err.Error(), "Get \"") {
		t.Errorf("错误里不该再带完整的请求 URL：%v", err)
	}
}
