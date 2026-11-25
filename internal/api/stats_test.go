package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/store"
)

// statsResp 只列这个测试要钉住的字段。
type statsResp struct {
	Points []struct {
		At     string `json:"at"`
		Online int    `json:"online"`
	} `json:"points"`
	Hours            int    `json:"hours"`
	BucketSeconds    int    `json:"bucketSeconds"`
	BucketLabel      string `json:"bucketLabel"`
	Samples          int    `json:"samples"`
	DataFrom         string `json:"dataFrom"`
	DataTo           string `json:"dataTo"`
	LatestAgoMinutes *int   `json:"latestAgoMinutes"`
	Note             string `json:"note"`
}

func getStats(t *testing.T, f *apiFixture, query string) (int, statsResp, string) {
	t.Helper()
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/stats?"+query, "admin", "secret")
	var r statsResp
	if rec.Code == http.StatusOK {
		decodeData(t, rec, &r)
	}
	return rec.Code, r, rec.Body.String()
}

// TestStatsCoversWholeWindow 钉的是趋势图最容易骗人的一处：窗口写 24 小时，
// 数据就必须覆盖到 24 小时前的那一段，而且右端必须是最新一条采样。
// 老实现是"取前 2000 行"，双世界跑一天就超了，于是图停在 6 小时前、界面还在说 24 小时。
func TestStatsCoversWholeWindow(t *testing.T) {
	f := newFixture(t, "")
	base := time.Now().UTC().Truncate(time.Minute)
	// 铺一条横跨 23 小时 40 分的采样带：每 20 分钟三个时刻、每个时刻两个世界各一条，
	// 最新那条就在刚才。留 20 分钟余量是因为接口的窗口是"从现在往回 24 小时"，
	// 而 base 已经向下取整到分钟——正好卡在 24 小时整的那条会被 since 切掉。
	for i := 0; i < 72; i++ {
		at := base.Add(-time.Duration(71-i) * 20 * time.Minute)
		for _, pair := range [][2]int{{2, 1}, {3, 0}, {1, 1}} {
			if err := f.srv.Store.RecordSnapshot(store.Snapshot{
				Cluster: f.cluster, Shard: "Master", Online: pair[0], At: at,
			}); err != nil {
				t.Fatalf("铺采样: %v", err)
			}
			if err := f.srv.Store.RecordSnapshot(store.Snapshot{
				Cluster: f.cluster, Shard: "Caves", Online: pair[1], At: at,
			}); err != nil {
				t.Fatalf("铺采样: %v", err)
			}
		}
	}
	earliest := base.Add(-71 * 20 * time.Minute)

	code, r, _ := getStats(t, f, "cluster="+f.cluster+"&hours=24")
	if code != http.StatusOK {
		t.Fatalf("状态 = %d", code)
	}
	if len(r.Points) < 2 {
		t.Fatalf("点太少：%d", len(r.Points))
	}
	if r.Samples != 72*3*2 {
		t.Errorf("原始条数 = %d，期望 %d", r.Samples, 72*3*2)
	}
	first, err := time.Parse(time.RFC3339, r.Points[0].At)
	if err != nil {
		t.Fatalf("第一个点的时间解不开: %v", err)
	}
	// 左端那一格必须装着最早那条采样（格子向下取整，所以点会比采样早最多一个格子）：
	// 老实现从这里就开始丢数据，丢的正是最靠窗口起点的那一段。
	if gap := earliest.Sub(first); gap < 0 || gap >= 5*time.Minute {
		t.Errorf("图左端 = %v，最早采样 = %v，不在同一格里（格子 5 分钟）", first, earliest)
	}
	if r.DataTo == "" {
		t.Fatal("没给数据实际覆盖到哪")
	}
	last, err := time.Parse(time.RFC3339, r.DataTo)
	if err != nil {
		t.Fatalf("最晚采样解不开: %v", err)
	}
	if lag := time.Now().UTC().Sub(last); lag > 2*time.Minute {
		t.Errorf("右端停在 %v 前，最新那条采样明明就在刚才", lag)
	}
	// 有数据才许报"距今几分钟"：这个数由后端算，界面那枚新鲜度把手直接引用它。
	if r.LatestAgoMinutes == nil {
		t.Fatal("有采样的房间没给最近一条距今几分钟")
	}
	if *r.LatestAgoMinutes > 2 {
		t.Errorf("距今 %d 分钟，这份夹具最新一条就在刚才", *r.LatestAgoMinutes)
	}
	// 各世界要相加：这一份夹具每格是 Master 2/3/1 之一 + Caves 1/0/1 之一。
	for _, p := range r.Points {
		if p.Online < 1 || p.Online > 4 {
			t.Errorf("格子里 %d 人，超出这份夹具可能拼出的范围 1~4（相加写错就会翻几倍）", p.Online)
		}
	}
	if !strings.Contains(r.Note, "点") || !strings.Contains(r.Note, "采样") {
		t.Errorf("说明没讲清点是怎么来的：%q", r.Note)
	}
	// 这句里不许出现服务端拼好的钟点：库里的时刻是 UTC，浏览器按本地钟画图下面那行，
	// 两处一拼就差一个时区（云机 UTC、人在这边 +8，界面上就是"距今 8 小时"那种对不上）。
	// 判据抄的是浏览器探针 .tools/overview105_check.py 里同一条。
	if clock := regexp.MustCompile(`\d{2}-\d{2} \d{2}:\d{2}`).FindString(r.Note); clock != "" {
		t.Errorf("说明里出现了服务端拼的钟点 %q：%q", clock, r.Note)
	}
	if !strings.Contains(r.Note, "距今") {
		t.Errorf("说明里没给出这份数据实际覆盖到什么时候（相对说法）：%q", r.Note)
	}

	// 窗口越长格子越大，但点数不许爆：一次给几千点前端画不动也没人看得清。
	for _, h := range []string{"1", "24", "168", "2160"} {
		_, long, _ := getStats(t, f, "cluster="+f.cluster+"&hours="+h)
		if len(long.Points) > 600 {
			t.Errorf("hours=%s 给了 %d 个点，超过 600", h, len(long.Points))
		}
		if long.BucketSeconds <= 0 {
			t.Errorf("hours=%s 的格子大小是 %d", h, long.BucketSeconds)
		}
	}

	// 没采样的房间：空点 + 一句人话，而不是 null、报错，或者上一房间的点。
	_, none, raw := getStats(t, f, "cluster=NoSuchCluster&hours=24")
	if len(none.Points) != 0 || none.Samples != 0 {
		t.Errorf("空房间给回了 %d 个点 / %d 条", len(none.Points), none.Samples)
	}
	if none.Note == "" {
		t.Error("空房间没有说明")
	}
	// 覆盖范围那三个数一个都不许出现：没有采样却说"距今 0 分钟"，
	// 界面就会在一张空图旁边挂一枚"最近一条采样就在刚才"。
	if none.DataFrom != "" || none.DataTo != "" {
		t.Errorf("空房间报出了覆盖范围：%v ~ %v", none.DataFrom, none.DataTo)
	}
	if none.LatestAgoMinutes != nil {
		t.Errorf("空房间报了最近一条距今 %d 分钟；raw=%s", *none.LatestAgoMinutes, raw)
	}

	// hours 越界仍然要说人话。
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/stats?cluster=x&hours=0", "admin", "secret")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "1~2160") {
		t.Errorf("hours=0 → %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/stats?hours=24", "admin", "secret")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("不带 cluster → %d，期望 400", rec.Code)
	}
	// 只问一个世界时不许把另一个世界的数字加进来。
	_, one, _ := getStats(t, f, "cluster="+f.cluster+"&hours=24&shard=Master")
	if one.Samples != 72*3 {
		t.Errorf("单世界原始条数 = %d，期望 %d", one.Samples, 72*3)
	}
	for _, p := range one.Points {
		if p.Online > 3 {
			t.Errorf("单世界一个格子 %d 人，期望不超过 Master 的最大人数 3", p.Online)
		}
	}
}

// TestTrendBucketLadder 钉窗口与格子大小的对应：界面写着"每分钟一个点"就必须真是每分钟。
func TestTrendBucketLadder(t *testing.T) {
	cases := map[int]time.Duration{
		1:    time.Minute,
		6:    time.Minute,
		10:   time.Minute,
		12:   2 * time.Minute,
		24:   5 * time.Minute,
		72:   10 * time.Minute,
		168:  30 * time.Minute,
		720:  2 * time.Hour,
		2160: 6 * time.Hour,
	}
	for h, want := range cases {
		if got := trendBucket(h); got != want {
			t.Errorf("hours=%d 的格子 = %v，期望 %v", h, got, want)
		}
	}
	if label := bucketLabel(5 * time.Minute); label != "每 5 分钟一个点" {
		t.Errorf("格子说明 = %q", label)
	}
	if label := bucketLabel(time.Hour); label != "每小时一个点" {
		t.Errorf("格子说明 = %q", label)
	}
	if label := bucketLabel(24 * time.Hour); label != "每天一个点" {
		t.Errorf("格子说明 = %q", label)
	}
}
