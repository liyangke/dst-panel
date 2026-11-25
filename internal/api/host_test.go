package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

// hostResp 只列这个测试要钉住的字段；缺字段不影响 json 解码，所以它同时也是
// "界面拿得到什么"的一份可读清单。
type hostResp struct {
	CPUCores    int     `json:"cpuCores"`
	CPUPercent  float64 `json:"cpuPercent"`
	CPUReady    bool    `json:"cpuReady"`
	CPUNote     string  `json:"cpuNote"`
	MemTotal    int64   `json:"memTotalBytes"`
	MemUsed     int64   `json:"memUsedBytes"`
	MemPercent  float64 `json:"memPercent"`
	MemNote     string  `json:"memNote"`
	SwapTotal   int64   `json:"swapTotalBytes"`
	SwapUsed    int64   `json:"swapUsedBytes"`
	LoadNote    string  `json:"loadNote"`
	UptimeSec   int64   `json:"uptimeSeconds"`
	UptimeNote  string  `json:"uptimeNote"`
	NetNote     string  `json:"netNote"`
	DiskPath    string  `json:"diskPath"`
	DiskTotal   int64   `json:"diskTotalBytes"`
	DiskFree    int64   `json:"diskFreeBytes"`
	DiskPercent float64 `json:"diskPercent"`
	DiskNote    string  `json:"diskNote"`
	PanelRSS    int64   `json:"panelRssBytes"`
	CollectedAt string  `json:"collectedAt"`
	Processes   []struct {
		PID      int     `json:"pid"`
		RSS      int64   `json:"rssBytes"`
		CPUPct   float64 `json:"cpuPercent"`
		CPUReady bool    `json:"cpuReady"`
		Note     string  `json:"note"`
	} `json:"processes"`
}

func getHost(t *testing.T, h http.Handler, query string) hostResp {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/api/v1/host"+query, "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/host%s = %d，期望 200：%s", query, rec.Code, rec.Body.String())
	}
	var out hostResp
	// 信封也是这一条要钉住的：界面那一层只从 {ok, data} 的 data 里取，
	// 发裸对象时页面不报错、只是那一栏永远停在"还没采到"（实测踩过）。
	var body struct {
		OK   bool     `json:"ok"`
		Data hostResp `json:"data"`
		Err  string   `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解不开 /api/v1/host：%v\n%s", err, rec.Body.String())
	}
	if !body.OK {
		t.Fatalf("/api/v1/host 回了 ok=false：%q", body.Err)
	}
	out = body.Data
	return out
}

// TestHostNeedsAuth 钉住"这台机器的硬件信息不是公开信息"：
// CPU 型号、内存大小、磁盘余量合起来足够让外人判断这台机器值不值得打。
func TestHostNeedsAuth(t *testing.T) {
	f := newFixture(t, "")
	if rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/host", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("不带凭据访问 /api/v1/host = %d，期望 401", rec.Code)
	}
}

func TestHostReportsMachineStats(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	first := getHost(t, h, "")
	// 第一次采样没有区间。这时候报 0% 会被读成"这台机器很闲"，
	// 所以必须给出说明、并把 ready 标成 false。
	if first.CPUReady || first.CPUNote == "" {
		t.Errorf("第一次采样的 CPU 应该是「还没区间 + 一句说明」，实际 ready=%v note=%q", first.CPUReady, first.CPUNote)
	}
	if first.NetNote == "" {
		t.Errorf("第一次采样的网速也该带一句说明，不能显示 0 B/s")
	}
	if first.CPUCores <= 0 {
		t.Errorf("cpuCores = %d，至少要有一个核", first.CPUCores)
	}
	if _, err := time.Parse(time.RFC3339, first.CollectedAt); err != nil {
		t.Errorf("collectedAt %q 不是 RFC3339：%v", first.CollectedAt, err)
	}

	time.Sleep(150 * time.Millisecond)
	second := getHost(t, h, "")
	if !second.CPUReady {
		t.Errorf("第二次采样该有区间了，实际 note=%q", second.CPUNote)
	}
	if second.CPUPercent < 0 || second.CPUPercent > 100 {
		t.Errorf("cpuPercent = %v，超出 0~100", second.CPUPercent)
	}
	if second.MemTotal <= 0 || second.MemNote != "" {
		t.Errorf("内存读不到：total=%d note=%q", second.MemTotal, second.MemNote)
	}
	if second.MemUsed < 0 || second.MemUsed > second.MemTotal {
		t.Errorf("内存已用 %d 超出总量 %d", second.MemUsed, second.MemTotal)
	}
	// Windows 上这一条是真会破的：GlobalMemoryStatusEx 给的是"提交额度"，
	// 里面含了物理内存那一份，不减掉就会报出"页文件用了 28 GB / 上限 20 GB"。
	if second.SwapUsed < 0 || second.SwapUsed > second.SwapTotal {
		t.Errorf("交换区/页文件已用 %d 不在 0~上限 %d 之间", second.SwapUsed, second.SwapTotal)
	}
	if second.DiskPath != f.paths.KleiRoot {
		t.Errorf("diskPath = %q，期望存档根目录 %q", second.DiskPath, f.paths.KleiRoot)
	}
	if second.DiskTotal <= 0 || second.DiskNote != "" {
		t.Errorf("磁盘读不到：total=%d note=%q", second.DiskTotal, second.DiskNote)
	}
	if second.DiskFree < 0 || second.DiskFree > second.DiskTotal {
		t.Errorf("磁盘剩余 %d 不在 0~总量 %d 之间", second.DiskFree, second.DiskTotal)
	}
	if second.PanelRSS <= 0 {
		t.Errorf("面板自己那份内存 = %d，runtime 拿不到这个数说明接线错了", second.PanelRSS)
	}
	// 运行时长：Linux 读 /proc/uptime，Windows 走 GetTickCount64，两边都该有数；
	// 真的读不到时也要留下那句说明，而不是显示"已运行 0 秒"。
	if second.UptimeSec <= 0 && second.UptimeNote == "" {
		t.Errorf("uptimeSeconds = 0 且没有说明")
	}
}

// TestHostProcessSampling 界面把已经知道的 pid 回传过来采进程占用。
// 读不到时必须留下说明：一个安静的 0 会让"这个分片没吃内存"看起来像结论。
func TestHostProcessSampling(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	_ = getHost(t, h, "") // 先建立区间，省掉第一轮"刚开始采样"

	self := strconv.Itoa(os.Getpid())
	got := getHost(t, h, "?pid="+self+"&pid=0&pid=abc&pid=99999999")
	if len(got.Processes) != 2 {
		t.Fatalf("只该收下两个像样的进程号，实际 %+v", got.Processes)
	}
	if got.Processes[0].PID != os.Getpid() {
		t.Fatalf("第一个进程号应是本测试自己（%d），实际 %+v", os.Getpid(), got.Processes[0])
	}
	p := got.Processes[0]
	if p.RSS <= 0 && p.Note == "" {
		t.Errorf("自家进程既没读到内存也没给说明")
	}
	if p.RSS > 0 && p.RSS < 1024*1024 {
		t.Errorf("自家进程的常驻内存 = %d 字节，小得不像话", p.RSS)
	}
	// 已经不存在的进程号：要有说明，不能报 0 字节 0%。
	ghost := got.Processes[1]
	if ghost.Note == "" {
		t.Errorf("不存在的进程 %d 该有说明，实际 %+v", ghost.PID, ghost)
	}
}

func TestHostWithoutCollector(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleHost(rec, httptest.NewRequest(http.MethodGet, "/api/v1/host", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("没接采集器时 = %d，期望 503：%s", rec.Code, rec.Body.String())
	}
}
