// Package logx 统一面板自己的运行日志。
//
// 和"平台日志"是两回事：这里记的是进程级的运行轨迹（启动、端口占用、worker 报错、
// 每个请求的耗时），出问题时 ssh 上去 tail 的就是它；
// 而"谁在什么时候踢了哪个玩家"那种是给管理员看的操作记录，走 store.audit_log。
//
// 同时写文件和标准输出：文件是给 systemd 之外手工启动时留的现场，
// 标准输出是给 journalctl 的。只选一个的话，另一半人就只能靠猜。
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

const logFileName = "panel.log"

// Setup 建好 logger 并把它设为进程默认（slog.Default），
// 返回的关闭函数要在退出时调用，否则最后几条日志可能还在缓冲区里。
//
// 日志文件不做轮转：面板的写入量是"每个请求一行"，一年也就几十 MB，
// 真要轮转应该交给 logrotate 而不是自己在进程里 rename——
// 进程内轮转会在 rename 的瞬间丢掉正在写的行。
func Setup(workDir string) (closeFn func() error, err error) {
	path := filepath.Join(workDir, logFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件 %s: %w", path, err)
	}
	logger := New(io.MultiWriter(f, os.Stdout))
	slog.SetDefault(logger)
	return func() error { return f.Close() }, nil
}

// New 用统一的格式造 logger，测试里传 io.Discard 让它闭嘴。
func New(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		// 毫秒时间戳：判断"是这一步慢还是下一步慢"时，秒级会并列成一片。
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
				a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
			}
			return a
		},
	}))
}
