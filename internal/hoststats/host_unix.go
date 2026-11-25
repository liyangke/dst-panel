//go:build unix

package hoststats

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// clockTicks 是 Linux 的 USER_HZ（进程节拍的每秒个数）。内核导出的是这个固定值，
// 不是 sysconf 可变的 HZ：/proc 里所有时间字段都按 100 计。
const clockTicks = 100

func readRaw(dataDir string) Raw {
	now := time.Now()
	out := Raw{At: now, DiskPath: dataDir}

	if ticks, idle, err := procCPU(); err != nil {
		out.ErrCPU = err
	} else {
		out.CPUTicks, out.CPUIdle = ticks, idle
	}

	if total, avail, swapTotal, swapUse, err := procMem(); err != nil {
		out.ErrMem = err
	} else {
		out.MemTotal, out.MemAvail = total, avail
		out.SwapTotal, out.SwapUse = swapTotal, swapUse
	}

	if l, err := procLoad(); err == nil {
		out.Load, out.HasLoad = l, true
	}
	if up, err := procUptime(); err != nil {
		out.ErrUptime = err
	} else {
		out.Uptime = up
	}
	if rx, tx, err := procNet(); err != nil {
		out.ErrNet = err
	} else {
		out.RxBytes, out.TxBytes = rx, tx
	}
	if m, err := procCPUModel(); err != nil {
		out.ErrCPUModel = err
	} else {
		out.CPUModel = m
	}
	if total, free, err := diskSpace(dataDir); err != nil {
		out.ErrDisk = err
	} else {
		out.DiskTotal, out.DiskFree = total, free
	}
	return out
}

// procSelf 读单个进程的累计节拍与常驻内存（树的那一层，见 sumProcTree）。
func procSelf(pid int) Proc {
	out := Proc{At: time.Now()}
	ticks, err := procPIDTicks(pid)
	if err != nil {
		out.Err = err
		return out
	}
	rss, err := procPIDRSS(pid)
	if err != nil {
		out.Err = err
		return out
	}
	out.Ticks, out.RssBytes = ticks, rss
	return out
}

// procChildren 列一个进程的直接子进程，读不到就当它是叶子。
//
// 这个文件（/proc/<pid>/task/<pid>/children）要内核开 CONFIG_PROC_CHILDREN，
// 2026-11-04 在云机上验过是有的（会话进程那一项直接列出了引擎的进程号）。
// 万一开在一台没有它的机器上，退化出来的就是这函数修之前的行为——
// 内存那一格只报 screen 自己那 2 MB，而不是一条把整页打挂的错误。
func procChildren(pid int) []int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if v, err := strconv.Atoi(f); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

// procCPUModel 取 /proc/cpuinfo 里第一条 model name。
// 只取一条就够：同一台机器上所有核是同一颗处理器，"model name" 逐核重复一遍而已。
func procCPUModel() (string, error) {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "", errors.New("这台机器上读不到 /proc/cpuinfo，处理器型号采不了")
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "model name" {
			continue
		}
		if m := cleanCPUModel(strings.TrimSpace(rest)); m != "" {
			return m, nil
		}
	}
	return "", errors.New("/proc/cpuinfo 里没有 model name 这一行")
}

func procCPU() (total, idle uint64, err error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, errors.New("这台机器上读不到 /proc/stat，CPU 占用采不了")
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		var sum, idleAll uint64
		for i, f := range strings.Fields(line)[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				continue
			}
			// guest/guest_nice 已经含在 user/nice 里，再算一遍会重复计数。
			if i >= 8 {
				break
			}
			sum += v
			if i == 3 || i == 4 { // idle + iowait
				idleAll += v
			}
		}
		if sum == 0 {
			return 0, 0, errors.New("/proc/stat 的 cpu 行读不出可用的节拍")
		}
		return sum, idleAll, nil
	}
	return 0, 0, errors.New("/proc/stat 里没有 cpu 汇总行")
}

func procMem() (total, avail, swapTotal, swapUsed int64, err error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, 0, errors.New("这台机器上读不到 /proc/meminfo，内存采不了")
	}
	var free, swapFree int64
	want := map[string]*int64{"MemTotal:": &total, "MemAvailable:": &avail, "MemFree:": &free,
		"SwapTotal:": &swapTotal, "SwapFree:": &swapFree}
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		dst, hit := want[key+":"]
		if !hit {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		*dst = v * 1024 // meminfo 的单位是 kB
	}
	if total <= 0 {
		return 0, 0, 0, 0, errors.New("/proc/meminfo 里没有 MemTotal")
	}
	if avail <= 0 {
		// 老内核（<3.14）没有 MemAvailable。退回 MemFree：把 cache 当成"可用"会让
		// 面板在内存其实吃紧的机器上一直显示绿灯，宁可报得偏紧一点。
		avail = free
	}
	swapUsed = swapTotal - swapFree
	if swapUsed < 0 {
		swapUsed = 0
	}
	return total, avail, swapTotal, swapUsed, nil
}

func procLoad() ([3]float64, error) {
	var out [3]float64
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return out, err
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return out, errors.New("/proc/loadavg 字段不够")
	}
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return out, err
		}
		out[i] = v
	}
	return out, nil
}

func procUptime() (time.Duration, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, errors.New("这台机器上读不到 /proc/uptime")
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("/proc/uptime 是空的")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/uptime 解不出秒数: %w", err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// procNet 把所有网卡（去掉回环）的收发字节加起来。
func procNet() (rx, tx uint64, err error) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, errors.New("这台机器上读不到 /proc/net/dev，网速采不了")
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 3 {
		return 0, 0, errors.New("/proc/net/dev 里没有网卡条目")
	}
	seen := false
	for _, line := range lines[2:] {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		// 字段顺序（/proc/net/dev）：rx bytes packets errs drop fifo frame compressed
		// 然后 tx bytes packets ...，所以收在 0、发在 8。
		if len(f) < 9 {
			continue
		}
		r, e1 := strconv.ParseUint(f[0], 10, 64)
		t, e2 := strconv.ParseUint(f[8], 10, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		rx += r
		tx += t
		seen = true
	}
	if !seen {
		return 0, 0, errors.New("/proc/net/dev 里一个能读的网卡都没有")
	}
	return rx, tx, nil
}

func diskSpace(dir string) (total, free int64, err error) {
	if dir == "" {
		return 0, 0, errors.New("没有告诉面板要看哪个目录所在分区")
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, fmt.Errorf("读 %s 所在分区: %w", dir, err)
	}
	bsize := int64(st.Bsize)
	return int64(st.Blocks) * bsize, int64(st.Bavail) * bsize, nil
}

// procPIDTicks 读一个进程的 utime+stime。
// /proc/<pid>/stat 的第 2 个字段是括号包住的进程名，里面可能有空格，所以从最后一个 ')' 之后切。
func procPIDTicks(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("读不到进程 %d 的统计信息（可能已经退了）", pid)
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return 0, fmt.Errorf("进程 %d 的统计信息格式不认识", pid)
	}
	f := strings.Fields(s[i+2:])
	// 去掉 state 之后，utime 是第 12 个（原表第 14 列）、stime 第 13 个（第 15 列），
	// 再往后 cutime/cstime 是第 14、15 个，所以要读到下标 14。
	if len(f) < 15 {
		return 0, fmt.Errorf("进程 %d 的统计信息字段不够", pid)
	}
	user, e1 := strconv.ParseUint(f[11], 10, 64)
	kill, e2 := strconv.ParseUint(f[12], 10, 64)
	cuser, e3 := strconv.ParseUint(f[13], 10, 64)
	ckill, e4 := strconv.ParseUint(f[14], 10, 64)
	if e1 != nil || e2 != nil {
		return 0, fmt.Errorf("进程 %d 的 CPU 节拍解不出来", pid)
	}
	if e3 == nil && e4 == nil {
		user += cuser
		kill += ckill
	}
	return user + kill, nil
}

// procPIDRSS 读进程的常驻内存（VmRSS，kB）。
func procPIDRSS(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, fmt.Errorf("读不到进程 %d 的内存信息（可能已经退了）", pid)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(line, "VmRSS:"))
		if len(f) == 0 {
			break
		}
		v, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			break
		}
		return v * 1024, nil
	}
	return 0, fmt.Errorf("进程 %d 没有 VmRSS 这一行", pid)
}
