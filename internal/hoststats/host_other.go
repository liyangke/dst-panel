//go:build !unix && !windows

package hoststats

import (
	"errors"
	"time"
)

// 面板实际只跑在 Linux（线上）和 Windows（开发机）上。这一份存在的唯一理由是
// `GOOS=plan9 go build ./...` 这类交叉编译不该在采集层就断掉：所有项都明确报"读不到"，
// 界面就会老实地显示说明文字而不是 0。
const clockTicks = 0

func readRaw(dataDir string) Raw {
	err := errors.New("这个系统上没有任何运行状态可读")
	now := time.Now()
	return Raw{At: now, DiskPath: dataDir, ErrCPU: err, ErrMem: err, ErrUptime: err,
		ErrNet: err, ErrDisk: err, ErrCPUModel: err}
}

func procSelf(pid int) Proc {
	return Proc{At: time.Now(), Err: errors.New("这个系统上采不到单个进程的占用")}
}

func procChildren(pid int) []int { return nil }
