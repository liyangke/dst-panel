// Package hoststats 读这台机器自己的运行状况：CPU、内存、磁盘、网络、负载。
//
// 为什么要单独一个包而不是在接口里现读：百分比要的是"两次采样之间"的差值，
// 单看一次 /proc/stat 只能得到开机以来的累计值，画在界面上是一条永远接近满的线。
// 所以这里必须存住上一次采样；而"存住"这件事一旦被两个并发请求交错读写就会串台，
// 干脆把状态收在一个带锁的 Collector 里。
//
// 读不到的项一律留一句说明，不给 0：4G 小机上"内存 0%"和"这台机器读不到内存"
// 是两件完全不同的事，前者会让人以为一切正常。
package hoststats

import (
	"runtime"
	"strings"
	"sync"
	"time"
)

// Raw 是平台层一次读到的累计量。百分比一律由 Collector 用两次采样的差值算。
type Raw struct {
	At time.Time

	// CPUTicks 是开机以来所有核的累计节拍，CPUIdle 是其中"空闲"的那部分。
	// 各平台节拍单位不同，但比值无量纲，所以两边都往这两个字段里塞。
	CPUTicks, CPUIdle uint64
	ErrCPU            error

	MemTotal, MemAvail int64
	SwapTotal, SwapUse int64
	ErrMem             error

	Load      [3]float64
	HasLoad   bool
	Uptime    time.Duration
	ErrUptime error

	RxBytes, TxBytes uint64
	ErrNet           error

	// CPUModel 是这台机器自己上报的处理器型号。单独一个错误而不并进 ErrCPU：
	// 百分比读的是 /proc/stat，型号读的是 /proc/cpuinfo，一个读不到不代表另一个也是。
	CPUModel    string
	ErrCPUModel error

	DiskPath  string
	DiskTotal int64
	DiskFree  int64
	ErrDisk   error
}

// Proc 是一棵进程树的累计量：给的那个进程号，加上它下面所有子进程。
// Ticks 用和整机同一个"节拍"口径（见 clockTicks）。
type Proc struct {
	At       time.Time
	Ticks    uint64
	RssBytes int64
	Err      error
}

// procTreeDepth 是往下数的层数上限。
//
// 为什么要有这一棵"树"：界面那一格原来只采 screen 会话进程自己，2026-11-04 在云机上
// 数过——会话进程 VmRSS 2024 kB，而真正的饥荒服务端是它的子进程，882900 kB 和
// 1121888 kB。所以屏上那句"内存 2.0 MB · CPU 0%"报的是那个 2 MB 的空壳，
// 引擎吃掉的 1 GB 一次都没进过这一格。
//
// 为什么要有上限：实测引擎是会话的直接子进程（启动命令里 bash 被 exec 换掉了，只有一层），
// 但层数不是契约，少写一层就多算一层都不该把整格读成 0；而顺着 /proc 往下走时，
// 遇上一个疯狂 fork 的进程得走得出来。
const procTreeDepth = 6

// sumProcTree 把一棵进程树的节拍与常驻内存加起来。两个平台读法当参数传进来，
// 这样"到底把哪些进程算进这一格"这段逻辑在任何一台开发机上都测得到。
func sumProcTree(root int, self func(int) Proc, children func(int) []int) Proc {
	out := self(root)
	if out.Err != nil {
		return out
	}
	seen := map[int]bool{root: true}
	frontier := children(root)
	for level := 0; level < procTreeDepth && len(frontier) > 0; level++ {
		var next []int
		for _, pid := range frontier {
			if pid <= 0 || seen[pid] {
				continue
			}
			seen[pid] = true
			// 树里某一个进程读不到（刚退出、或这一层没权限）就跳过它：
			// 少一个子进程不该让整格变成"读不到"。
			p := self(pid)
			if p.Err != nil {
				continue
			}
			out.Ticks += p.Ticks
			out.RssBytes += p.RssBytes
			next = append(next, children(pid)...)
		}
		frontier = next
	}
	return out
}

// readProc 采平台层给的那个进程号所在的整棵树。两个平台读法由 host_*.go 提供。
func readProc(root int) Proc { return sumProcTree(root, procSelf, procChildren) }

// cleanCPUModel 去掉厂商拼在型号里的注册标记并收掉多余空格。
// "Intel(R) Core(TM) i7-9700 CPU @ 3.00GHz" 里那几处 (R) 是广告不是信息，
// 而这一行要站在窄屏的一格标签里。
func cleanCPUModel(s string) string {
	for _, mark := range []string{"(R)", "(TM)", "(C)", "(r)", "(m)"} {
		s = strings.ReplaceAll(s, mark, "")
	}
	return strings.Join(strings.Fields(s), " ")
}

// Stats 是给界面用的成品：算好的百分比 + 读不到的说明。
//
// 约定：凡是 Note 非空的项，对应的数字都不可信（要么没读到，要么区间还没成立）。
type Stats struct {
	CPUCores   int     `json:"cpuCores"`
	CPUPercent float64 `json:"cpuPercent"`
	CPUReady   bool    `json:"cpuReady"`
	CPUNote    string  `json:"cpuNote"`

	// CPUModel 是处理器型号原文（去掉厂商标记）；读不到时 CPUModelNote 说为什么，
	// 界面那一格不能空着——空着会被读成"这台机器没插 CPU"。
	CPUModel     string `json:"cpuModel"`
	CPUModelNote string `json:"cpuModelNote"`

	MemTotalBytes  int64   `json:"memTotalBytes"`
	MemUsedBytes   int64   `json:"memUsedBytes"`
	MemPercent     float64 `json:"memPercent"`
	MemNote        string  `json:"memNote"`
	SwapTotalBytes int64   `json:"swapTotalBytes"`
	SwapUsedBytes  int64   `json:"swapUsedBytes"`

	Load1    float64 `json:"load1"`
	Load5    float64 `json:"load5"`
	Load15   float64 `json:"load15"`
	LoadNote string  `json:"loadNote"`

	UptimeSeconds int64  `json:"uptimeSeconds"`
	UptimeNote    string `json:"uptimeNote"`

	NetRxPerSec float64 `json:"netRxBytesPerSecond"`
	NetTxPerSec float64 `json:"netTxBytesPerSecond"`
	NetNote     string  `json:"netNote"`

	DiskPath       string  `json:"diskPath"`
	DiskTotalBytes int64   `json:"diskTotalBytes"`
	DiskUsedBytes  int64   `json:"diskUsedBytes"`
	DiskFreeBytes  int64   `json:"diskFreeBytes"`
	DiskPercent    float64 `json:"diskPercent"`
	DiskNote       string  `json:"diskNote"`

	// PanelRSSBytes 是面板自己这一份进程的常驻内存：小机器上要分得清
	// "是引擎吃满了还是面板吃满了"，这一项靠 runtime 就能拿到，跨平台都有。
	PanelRSSBytes int64  `json:"panelRssBytes"`
	CollectedAt   string `json:"collectedAt"`
}

// ProcNote 是单个进程的采样成品。
type ProcNote struct {
	RssBytes   int64   `json:"rssBytes"`
	CPUPercent float64 `json:"cpuPercent"`
	CPUReady   bool    `json:"cpuReady"`
	Note       string  `json:"note"`
}

// Collector 存住上一次采样，把累计量换算成区间百分比。可以并发调用。
type Collector struct {
	mu       sync.Mutex
	prev     *Raw
	procPrev map[int]Proc
	dataDir  string
}

// NewCollector 的 dataDir 是"要看哪块盘"的那个目录（一般是存档根目录）。
func NewCollector(dataDir string) *Collector {
	return &Collector{dataDir: dataDir}
}

// Stats 读一遍当前值并与上一次比。第一次只能给出绝对量（内存、磁盘、负载、开机时长），
// 需要区间的 CPU 与网速这一轮会带着"刚开始采样"的说明。
func (c *Collector) Stats() Stats {
	cur := readRaw(c.dataDir)

	c.mu.Lock()
	prev := c.prev
	c.prev = &cur
	c.mu.Unlock()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	out := Stats{
		CPUCores:      runtime.NumCPU(),
		CollectedAt:   cur.At.UTC().Format(time.RFC3339),
		PanelRSSBytes: int64(ms.Sys),
	}

	switch {
	case cur.ErrCPU != nil:
		out.CPUNote = cur.ErrCPU.Error()
	case prev == nil || prev.ErrCPU != nil || cur.CPUTicks <= prev.CPUTicks:
		// 区间不成立（第一次、或计数器被重置）时宁可说"刚开始采样"，
		// 也不要报 0%——0% 在这一台上会被当成结论。
		out.CPUNote = "刚开始采样，这一轮还没有可比的区间"
	default:
		span := float64(cur.CPUTicks - prev.CPUTicks)
		idle := float64(cur.CPUIdle - prev.CPUIdle)
		if idle < 0 {
			idle = 0
		}
		out.CPUPercent = clampPercent((span - idle) / span * 100)
		out.CPUReady = true
	}

	switch {
	case cur.ErrCPUModel != nil:
		out.CPUModelNote = cur.ErrCPUModel.Error()
	case cur.CPUModel == "":
		out.CPUModelNote = "这台机器没有把处理器型号写在他能读到的地方"
	default:
		out.CPUModel = cur.CPUModel
	}

	switch {
	case cur.ErrMem != nil:
		out.MemNote = cur.ErrMem.Error()
	case cur.MemTotal <= 0:
		out.MemNote = "读到了空的内存数值"
	default:
		out.MemTotalBytes = cur.MemTotal
		out.MemUsedBytes = cur.MemTotal - cur.MemAvail
		out.MemPercent = clampPercent(float64(out.MemUsedBytes) / float64(cur.MemTotal) * 100)
		out.SwapTotalBytes = cur.SwapTotal
		out.SwapUsedBytes = cur.SwapUse
	}

	if cur.HasLoad {
		out.Load1, out.Load5, out.Load15 = cur.Load[0], cur.Load[1], cur.Load[2]
	} else {
		out.LoadNote = "这个系统没有平均负载这一项"
	}

	if cur.ErrUptime != nil {
		out.UptimeNote = cur.ErrUptime.Error()
	} else {
		out.UptimeSeconds = int64(cur.Uptime / time.Second)
	}

	switch {
	case cur.ErrNet != nil:
		out.NetNote = cur.ErrNet.Error()
	case prev == nil || prev.ErrNet != nil || cur.RxBytes < prev.RxBytes || cur.TxBytes < prev.TxBytes:
		out.NetNote = "刚开始采样，这一轮还没有可比的区间"
	default:
		secs := cur.At.Sub(prev.At).Seconds()
		if secs <= 0 {
			out.NetNote = "两次采样的间隔太短"
		} else {
			out.NetRxPerSec = float64(cur.RxBytes-prev.RxBytes) / secs
			out.NetTxPerSec = float64(cur.TxBytes-prev.TxBytes) / secs
		}
	}

	out.DiskPath = cur.DiskPath
	switch {
	case cur.ErrDisk != nil:
		out.DiskNote = cur.ErrDisk.Error()
	case cur.DiskTotal <= 0:
		out.DiskNote = "读到了空的磁盘数值"
	default:
		out.DiskTotalBytes = cur.DiskTotal
		out.DiskFreeBytes = cur.DiskFree
		out.DiskUsedBytes = cur.DiskTotal - cur.DiskFree
		out.DiskPercent = clampPercent(float64(out.DiskUsedBytes) / float64(cur.DiskTotal) * 100)
	}
	return out
}

// Process 采一个进程组（按 screen 会话的进程号，连同它下面的引擎）。
// 和整机 CPU 同一个道理：CPU% 要两次采样才有区间。
func (c *Collector) Process(pid int) ProcNote {
	if pid <= 0 {
		return ProcNote{Note: "这个世界没有在跑，没有进程号可看"}
	}
	cur := readProc(pid)

	c.mu.Lock()
	if c.procPrev == nil {
		c.procPrev = map[int]Proc{}
	}
	prev, had := c.procPrev[pid]
	c.procPrev[pid] = cur
	c.mu.Unlock()

	if cur.Err != nil {
		return ProcNote{Note: cur.Err.Error()}
	}
	out := ProcNote{RssBytes: cur.RssBytes}
	switch {
	case !had || prev.Ticks == 0 || cur.Ticks < prev.Ticks:
		out.Note = "刚开始采样，这一轮还没有可比的区间"
		return out
	case clockTicks <= 0:
		out.Note = "这个系统上没有可读的进程节拍"
		return out
	}
	secs := cur.At.Sub(prev.At).Seconds()
	if secs <= 0 {
		out.Note = "两次采样的间隔太短"
		return out
	}
	// 口径和整机那一格对齐：都用"占全部核的百分比"，
	// 否则 4 核机器上引擎跑满一个核这里显示 100%、整机才 25%，两个数对不上会被当成 bug。
	usedTicks := float64(cur.Ticks-prev.Ticks) / clockTicks
	out.CPUPercent = clampPercent(usedTicks / (secs * float64(runtime.NumCPU())) * 100)
	out.CPUReady = true
	return out
}

// Reset 丢掉上一轮采样。测试和"换了台机器"这类场景用，界面上没有入口。
func (c *Collector) Reset() {
	c.mu.Lock()
	c.prev = nil
	c.procPrev = nil
	c.mu.Unlock()
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return float64(int64(v*10+0.5)) / 10
}
