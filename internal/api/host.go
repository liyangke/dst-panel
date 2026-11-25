// 这台机器的运行状态接口：CPU、内存、磁盘、网络、负载、运行时长。
//
// 一条设计取舍：进程级的占用不在这里自己找。面板已经每几秒问一次 `screen -ls`
// 并把每个分片的 pid 发给了界面（/api/v1/rooms），所以这里让界面把已经知道的 pid
// 回传过来（?pid=123&pid=456），我们只负责按 pid 采样。
// 另起一次 screen 调用会让这个接口跟着进程数变慢，而它恰恰是要在被嫌弃"卡"的时候常刷的。
//
// 一个后来补上的口径：界面传回来的是 screen 会话进程的 pid，饥荒服务端是它的子进程，
// 所以采样那一层（hoststats.readProc）顺着进程树往下加，而不是只读这一个 pid。
// 只读会话那一层的话，界面上那格会常年停在「内存 2.0 MB · CPU 0%」——那是那个 2 MB 空壳的数，
// 引擎实际吃掉的 0.9~1.1 GB 一次都没进来（2026-11-04 云机实测：会话 2024 kB，引擎 882900 kB）。
package api

import (
	"net/http"
	"strconv"

	"dstpanel/internal/hoststats"
)

// 指标口径说明：这一页的数字来自好几个不同层面，很容易被当成同一套读数。
// metricsNote 是给界面直接显示的一整段话，metricsScope 是拆开的分项版本，前端想分开摆也够用。
//
// 重点纠正一处历史命名：hoststats.Stats.panelRssBytes 取的其实是 runtime.MemStats.Sys
// （Go 运行时向系统申请的内存总量，含堆、栈与运行时内部结构），并不是操作系统意义上的
// 常驻集 RSS。底层 hoststats 包不在这次改动范围内，不能直接改它的字段名，所以这里在
// 响应层补一个语义准确的 panelHeapSysBytes，并把"两者当前是同一个值"讲清楚。
const metricsNoteText = "口径说明：panelRssBytes 是历史字段，实际取的是 Go 运行时的 runtime.MemStats.Sys" +
	"（运行时向系统申请的内存总量，含堆、栈与运行时内部结构），不是操作系统意义上的常驻集 RSS；" +
	"语义准确的同一份数据见 panelHeapSysBytes，当前两者是同一个值。" +
	"磁盘（disk*）与网络（net*）是整台机器的读数，不是某个分片或面板进程；" +
	"CPU 整机百分比来自 /proc/stat 两次采样的差值，进程级 CPU/内存（processes）在 Linux 上按 /proc 顺着进程树累加；" +
	"Windows 开发机上不采集进程级指标，对应 pid 的 note 会写明读不到。"

// hostMetricsScope 每次返回新 map，避免多个响应共享同一份可变 map。
func hostMetricsScope() map[string]string {
	return map[string]string{
		"panelMemory": "panelHeapSysBytes / panelRssBytes 取 runtime.MemStats.Sys，是 Go 运行时申请的内存，不是 OS RSS",
		"disk":        "整机所在分区的容量与剩余，不是某个分片",
		"network":     "整台机器的收发速率，不是某个进程",
		"cpu":         "整机 cpuPercent 来自 /proc/stat 两次采样的差值",
		"process":     "processes 按 pid 顺着 /proc 进程树累加（Linux）；Windows 不采集进程级指标",
	}
}

type hostProcessView struct {
	PID        int     `json:"pid"`
	RssBytes   int64   `json:"rssBytes"`
	CPUPercent float64 `json:"cpuPercent"`
	CPUReady   bool    `json:"cpuReady"`
	Note       string  `json:"note"`
}

type hostView struct {
	hoststats.Stats
	Processes []hostProcessView `json:"processes"`

	// 下面三项是口径纠正与说明，不改动 hoststats.Stats 里的任何既有字段。
	// panelRssBytes 及其余老字段照旧输出，前端旧代码不受影响。
	PanelHeapSysBytes int64             `json:"panelHeapSysBytes"`
	MetricsNote       string            `json:"metricsNote"`
	MetricsScope      map[string]string `json:"metricsScope"`
}

func (s *Server) handleHost(w http.ResponseWriter, r *http.Request) {
	if s.Host == nil {
		writeErr(w, http.StatusServiceUnavailable, "面板没有接上运行状态采集器")
		return
	}
	// Processes 给空数组而不是 null：没起世界时一个 pid 参数都不会有。
	out := hostView{
		Stats:        s.Host.Stats(),
		Processes:    []hostProcessView{},
		MetricsNote:  metricsNoteText,
		MetricsScope: hostMetricsScope(),
	}
	// 底层 panelRssBytes 本来就是 runtime.MemStats.Sys，这里直接复用同一个值：
	// 不重复 ReadMemStats，跨平台（含 Windows）都拿得到——这本身就是"取不到真实 RSS 时"的兜底。
	out.PanelHeapSysBytes = out.Stats.PanelRSSBytes
	for _, raw := range r.URL.Query()["pid"] {
		pid, err := strconv.Atoi(raw)
		if err != nil || pid <= 0 {
			// 界面传了个不像进程号的东西：跳过它，但不要把整页问句变成 400——
			// 这一页其余的机器数字和某个进程号对不对没关系。
			continue
		}
		np := s.Host.Process(pid)
		out.Processes = append(out.Processes, hostProcessView{
			PID: pid, RssBytes: np.RssBytes, CPUPercent: np.CPUPercent,
			CPUReady: np.CPUReady, Note: np.Note,
		})
	}
	// 走 writeOK 而不是 writeJSON：界面的接口层统一从 {ok, data} 里取 data，
	// 这里发裸对象的话界面拿到的是 null，那一栏就永远停在"还没采到"。
	writeOK(w, out)
}
