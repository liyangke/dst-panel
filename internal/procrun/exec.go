package procrun

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// 单独放一层是为了让 Runner 的测试完全不碰真实进程。
func lookPath(name string) (string, error) { return exec.LookPath(name) }

func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// LineRunner 逐行拿子进程输出。
//
// 为什么要第三种执行方式：SteamCMD 更新服务端本体要十几分钟，一次性攒完输出
// 就没法给进度条；而 screen 那些命令都是毫秒级的，用 Runner 更简单。
// 两者都走 exec，不经过 shell——参数里带空格或引号都不可能被重新解释。
type LineRunner interface {
	RunLines(ctx context.Context, name string, args []string, onLine func(string)) error
}

// RunLines 实现 LineRunner。
//
// 和 Run 不同的是这里不做 isNotFoundExit 归一化：screen 用退出码表达"没会话"，
// SteamCMD 的非零退出就是失败，调用方要看清是哪一种。
func (e ExecRunner) RunLines(ctx context.Context, name string, args []string, onLine func(string)) error {
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}
	bin, err := lookPath(name)
	if err != nil {
		return fmt.Errorf("%w %s: %w", ErrCommandMissing, name, err)
	}
	cmd := commandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("接管 %s 的输出: %w", name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("接管 %s 的错误输出: %w", name, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s: %w", name, err)
	}

	// 两条流合进一个无缓冲 chan：谁先出词谁先被消费，天然交错，也就不用自己管顺序。
	// 计数到 2 再关闭，是因为两个 goroutine 共用这个 chan，任何一方 close 都会丢对方的数据。
	lines := make(chan string)
	var streams sync.WaitGroup
	streams.Add(2)
	go func() { defer streams.Done(); scanLines(stdout, lines) }()
	go func() { defer streams.Done(); scanLines(stderr, lines) }()
	go func() { streams.Wait(); close(lines) }()

	// 先把每一行喂给回调再 Wait：SteamCMD 的进度就是这样一行行刷出来的。
	for line := range lines {
		if onLine != nil {
			onLine(line)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("执行 %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// scanLines 把一条流的每一行送进 out，读到结束就返回（不 close，见调用处）。
type lineReader interface {
	Read([]byte) (int, error)
}

func scanLines(r lineReader, out chan<- string) {
	sc := bufio.NewScanner(r)
	// SteamCMD 的进度条会把很多内容刷在同一行，默认 64KB 上限会被撑爆并直接结束扫描。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		out <- sc.Text()
	}
}
