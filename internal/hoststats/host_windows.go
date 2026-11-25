//go:build windows

package hoststats

import (
	"errors"
	"syscall"
	"time"
	"unsafe"
)

// 这个文件只为"在 Windows 开发机上把界面跑起来看"服务：线上是 Linux，走 host_unix.go。
//
// 为什么不引 golang.org/x/sys：它虽然已经在依赖图里（sqlite 带的），但把它提成面板
// 的直接依赖，换来的是只有开发机才跑的十几行读数——用 stdlib 的 syscall.NewLazyDLL
// 自己挂 kernel32 就够，依赖表和"只用标准库"这条项目约定都不动。

// clockTicks 在 Windows 上没有"进程节拍"这个口径（readProc 直接报读不到），
// 这个常数只为满足共用代码里的分支。
const clockTicks = 0

var (
	modKernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = modKernel32.NewProc("GetSystemTimes")
	procGetTickCount64       = modKernel32.NewProc("GetTickCount64")
	procGetDiskFreeSpaceExW  = modKernel32.NewProc("GetDiskFreeSpaceExW")

	modAdvapi32          = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyExW    = modAdvapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW = modAdvapi32.NewProc("RegQueryValueExW")
	procRegCloseKey      = modAdvapi32.NewProc("RegCloseKey")
)

// 注册表里那颗 CPU：HKEY_LOCAL_MACHINE + KEY_READ。这两个数是 Windows SDK 的常量，
// 和 Go 的 syscall 包没导出它们没关系——照着 advapi32 的签名原样写死。
const (
	regHKLMDir     = 0x80000002
	regKeyRead     = 0x20019
	regCPUPath     = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`
	regCPUValue    = "ProcessorNameString"
	regCPUMaxBytes = 4096
)

// memoryStatusEx 是 GlobalMemoryStatusEx 的那个带版本号的结构：第一字段必须先填大小，
// 否则 API 直接返回失败——这是它最常见的踩坑点。
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func readRaw(dataDir string) Raw {
	now := time.Now()
	out := Raw{At: now, DiskPath: dataDir}

	if err := procGetSystemTimes.Find(); err == nil {
		var idle, kernel, user syscall.Filetime
		ok, _, e := procGetSystemTimes.Call(
			uintptr(unsafe.Pointer(&idle)),
			uintptr(unsafe.Pointer(&kernel)),
			uintptr(unsafe.Pointer(&user)))
		if ok == 0 {
			out.ErrCPU = errors.New("问系统时间计数失败了：" + e.Error())
		} else {
			// kernel 里已经含了 idle，所以总量是 kernel+user，空闲就是 idle。
			out.CPUIdle = filetimeToUint64(idle)
			out.CPUTicks = filetimeToUint64(kernel) + filetimeToUint64(user)
		}
	} else {
		out.ErrCPU = errors.New("这台机器上没有 GetSystemTimes，CPU 占用采不了")
	}

	if err := procGlobalMemoryStatusEx.Find(); err == nil {
		st := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
		ok, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
		if ok == 0 {
			out.ErrMem = errors.New("问内存状态失败了：" + e.Error())
		} else {
			out.MemTotal = int64(st.TotalPhys)
			out.MemAvail = int64(st.AvailPhys)
			// 页文件在 Windows 上是"提交额度"，和 Linux 的 swap 不是一回事，
			// 但放在同一格里比空着有用：小机器上开太多东西先撞的就是它。
			// GlobalMemoryStatusEx 给的 TotalPageFile-AvailPageFile 是整个系统已提交的字节，
			// 里面含了物理内存里那一份，必须把已用的物理减掉才是落在页文件上的量；
			// 直接减 SwapTotal 会把物理那几十 GB 也算进页文件（实测报出"页文件用了 28 GB / 上限 20 GB"）。
			physUsed := int64(st.TotalPhys) - int64(st.AvailPhys)
			commit := int64(st.TotalPageFile) - int64(st.AvailPageFile)
			out.SwapTotal = int64(st.TotalPageFile) - int64(st.TotalPhys)
			if out.SwapTotal < 0 {
				out.SwapTotal = 0
			}
			out.SwapUse = commit - physUsed
			if out.SwapUse < 0 {
				out.SwapUse = 0
			}
			// 页文件会自动扩，提交量允许临时超过当前上限：那一格只能报满，不能报出个比上限大的数。
			if out.SwapUse > out.SwapTotal {
				out.SwapUse = out.SwapTotal
			}
		}
	} else {
		out.ErrMem = errors.New("这台机器上没有 GlobalMemoryStatusEx，内存采不了")
	}

	// GetTickCount64 是开机以来的毫秒数（不受系统时间被改动影响），够当运行时长。
	if err := procGetTickCount64.Find(); err == nil {
		ms, _, _ := procGetTickCount64.Call()
		out.Uptime = time.Duration(ms) * time.Millisecond
	} else {
		out.ErrUptime = errors.New("这台机器读不到开机时长")
	}

	out.ErrNet = errors.New("这台机器看不到网络收发（Windows 上没有这一项）")

	if m, err := winCPUModel(); err != nil {
		out.ErrCPUModel = err
	} else {
		out.CPUModel = m
	}

	if dataDir == "" {
		out.ErrDisk = errors.New("面板没设定要统计哪个目录所在分区")
	} else if err := procGetDiskFreeSpaceExW.Find(); err != nil {
		out.ErrDisk = errors.New("这台机器读不到磁盘剩余空间")
	} else {
		var free, total, totalFree uint64
		ok, _, e := procGetDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(dataDir))),
			uintptr(unsafe.Pointer(&free)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&totalFree)))
		if ok == 0 {
			out.ErrDisk = errors.New("读不到 " + dataDir + " 所在分区：" + e.Error())
		} else {
			out.DiskFree = int64(free)
			out.DiskTotal = int64(total)
		}
	}
	return out
}

func procSelf(pid int) Proc {
	return Proc{At: time.Now(), Err: errors.New("这个系统上采不到单个进程的占用（面板跑在 Linux 上时才有）")}
}

// procChildren 在 Windows 上恒为空：饥荒服务端只跑在 Linux 那一侧，
// 这台开发机上没有 screen 会话、也没有引擎进程可往下数。
func procChildren(pid int) []int { return nil }

func filetimeToUint64(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// winCPUModel 读注册表里那颗 CPU 的名字（每个核一项，取 0 号那颗）。
func winCPUModel() (string, error) {
	if err := procRegOpenKeyExW.Find(); err != nil {
		return "", errors.New("这台机器上没有注册表接口，处理器型号采不了")
	}
	var key uintptr
	path := syscall.StringToUTF16Ptr(regCPUPath)
	r, _, _ := procRegOpenKeyExW.Call(regHKLMDir, uintptr(unsafe.Pointer(path)), 0,
		regKeyRead, uintptr(unsafe.Pointer(&key)))
	if r != 0 {
		return "", errors.New("打不开注册表里描述处理器的那一项")
	}
	defer procRegCloseKey.Call(key)

	value := syscall.StringToUTF16Ptr(regCPUValue)
	// 第一次调用只问长度（传空缓冲区），第二次才把字符串取回来——注册表 API 的固定两步。
	var size uint32
	r, _, _ = procRegQueryValueExW.Call(key, uintptr(unsafe.Pointer(value)), 0, 0, 0,
		uintptr(unsafe.Pointer(&size)))
	if r != 0 || size == 0 || size > regCPUMaxBytes {
		return "", errors.New("注册表里那一项没有写出处理器名字")
	}
	buf := make([]uint16, size/2+1)
	r, _, _ = procRegQueryValueExW.Call(key, uintptr(unsafe.Pointer(value)), 0, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r != 0 {
		return "", errors.New("注册表里那一项没有写出处理器名字")
	}
	m := cleanCPUModel(syscall.UTF16ToString(buf))
	if m == "" {
		return "", errors.New("注册表里写的处理器名字是空的")
	}
	return m, nil
}
