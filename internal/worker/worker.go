// Package worker 执行异步任务队列。
//
// 为什么要单独一个包而不是塞进 api：这些活要跑十几分钟（SteamCMD 拉服务端本体），
// HTTP 请求挂在那里等等于把连接和超时都赌在一个不可控的外部网络上。
// 约定是"提交即返回 id，界面轮询 /api/v1/tasks 看进度"。
//
// 一次只跑一条任务，这是刻意的：这台机器只有 4G 内存，两个分片已经在吃满，
// 下载与解压再并发就是把"服务器突然卡死"变成日常。
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dstpanel/internal/backup"
	"dstpanel/internal/dst"
	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
)

const (
	// KindServerUpdate 用 steamcmd 更新服务端本体（app 343050）。
	// 工坊模组不走这条路：引擎启动时自己按 dedicated_server_mods_setup.lua 下载。
	KindServerUpdate = "server.update"
)

// Kind 是一种可提交的任务类型。界面按这张表渲染按钮，
// 用户提交不了表外的 kind，也就不可能把面板变成任意命令执行器。
type Kind struct {
	Name  string
	Label string
	Desc  string
	// Danger 为真时前端要二次确认，服务端同样强制。
	Danger bool
	// SubmitVia 非空表示这类任务的参数只能由面板内部构造，通用提交口不收。
	// 存档备份和恢复都要带集群名/快照编号，那两个值必须在入队前对着磁盘核过白名单，
	// 而通用提交口的请求体里只有一个 kind——所以给它一个专门的入口，
	// 而不是在 /api/v1/tasks 上开一个"谁都能塞 payload"的口子。
	SubmitVia string
}

var kinds = map[string]Kind{
	KindServerUpdate: {
		Name:  KindServerUpdate,
		Label: "下载/更新服务端本体",
		Desc: "用 steamcmd 拉 app_update 343050 validate 到配置里的 paths.dstInstall，" +
			"并强制按 linux 取 depot（不指定就会拉成 Windows 那份，界面报成功但起不了服）。" +
			"目录是空的也一样跑：那时装的是全新本体。整包多大、还要多久都由 steamcmd " +
			"实时报出来，进度条跟着字节数走。所有世界必须先关掉。",
		// 会覆盖正在被分片使用的文件，而且更新完必然要重启，属于危险动作。
		Danger: true,
	},
	KindBackupCreate: {
		Name:  KindBackupCreate,
		Label: "备份存档",
		Desc: "给一个房间打一份存档备份：先让在跑的世界立即存盘，再把整个房间目录压成一个带哈希清单的包。" +
			"Klei 令牌和面板命令通道文件不会进包（备份是可以下载的）。",
		SubmitVia: "存档页的房间备份按钮（POST /api/v1/clusters/{name}/backups）",
	},
	KindBackupRestore: {
		Name:  KindBackupRestore,
		Label: "恢复存档",
		Desc: "用一份备份换掉房间现在的存档目录（整棵替换，因为引擎读的是编号最大的存档档位）。" +
			"动手前会自动打一份恢复前的安全备份，校验不过就回滚。目标房间的世界必须全部停掉。",
		Danger:    true,
		SubmitVia: "存档页的恢复按钮（POST /api/v1/backups/{id}/restore）",
	},
	KindBackupImport: {
		Name:  KindBackupImport,
		Label: "导入外部存档",
		Desc: "把上传的存档包（zip、tar.gz、tar 三种）变成一个带哈希清单的快照。" +
			"面板先只读包的结构：认出 Master / Caves 的世界索引和档位文件才放行，" +
			"读不出世界编号的包当场拦下——放行那种包的话引擎进世界会静默生成一个新世界，一句错都不报。" +
			"包里的 Klei 令牌、面板命令通道文件和引擎日志都不进快照。" +
			"选了「导入后就换进这个房间」，后半段走的就是恢复存档那套流程：先自动打安全备份，" +
			"目标房间的世界必须全部停掉。",
		Danger:    true,
		SubmitVia: "存档页的导入面板（POST /api/v1/backups/import）",
	},
}

func KindNames() []string {
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func KindSpec(name string) (Kind, bool) {
	k, ok := kinds[name]
	return k, ok
}

// ErrNoShutDown 表示还有分片在跑，本体更新被拦住。
var ErrShardsRunning = errors.New("还有世界在运行")

// ProgressFn 是执行器向 worker 回报进度的唯一通道。
//
// detail 为 nil 表示这一步没有"字节数"可言：备份按条目数走、恢复按阶段走，
// 界面这时候只把 message 那句话显示出来，比摆一组 0 诚实。
type ProgressFn func(percent int, message string, detail *store.TaskDetail)

type Worker struct {
	Store *store.Store
	Paths dst.Paths
	// Lines 是执行外部命令的通道，生产用 procrun.ExecRunner。
	Lines procrun.LineRunner
	// Procs 只用来问"现在有没有分片在跑"；为 nil 时跳过这项检查（测试里用）。
	Procs *procrun.Driver
	// Backups 是存档快照层，备份与恢复两类任务用它。为 nil 时这两条任务直接失败
	// 并说明是没接上，而不是半路 panic——接线漏了是最常见的"测试全绿但线上没有"。
	Backups *backup.Manager
	// SteamCmd 来自配置 paths.steamCmd。
	SteamCmd string
	// Timeout 是普通任务（备份、恢复）的硬上限，默认 60 分钟。
	Timeout time.Duration
	// LongTimeout 是"下载/更新本体"这种整包任务的硬上限，默认 12 小时。
	// 这一档为什么宽到不像话：整包 4.28 GB，带宽却没人能替你猜——同一台云机在一次任务里
	// 本体文件下来那一段实测 9.9~29.6 MB/s（4 分半下完），而 steamcmd 给自己更新那一段
	// 一共约 115 MB 跑了 34 分钟（不足 60 KB/s，中途 30 秒窗口量到 33 KB/s）。
	// 两段差五百倍，按"典型带宽"写死的上限一定会在某一台机器上把正常下载判成失败。
	// 所以这一项不只是防僵尸：判停只能认出"彻底不动"，认不出"失败后从头重下、行一直在变"，
	// 那一种（云机实测过：Download failed: http error 0，然后换一轮包重头来）就归这一档兜。
	LongTimeout time.Duration
	// Stall 是"连续多久读不到新内容就算卡死"，默认 5 分钟。
	// 只对逐行读外部命令的任务生效（现在就是本体下载）；备份/恢复不走输出流，由 Timeout 管。
	Stall time.Duration
	// Poll 是没活时的轮询间隔，默认 2 秒。
	Poll time.Duration
	// Logger 为空用 slog.Default()。
	Logger *slog.Logger
}

func (w *Worker) log() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// timeoutFor 按任务类型挑硬上限。
func (w *Worker) timeoutFor(kind string) time.Duration {
	if kind == KindServerUpdate {
		if w.LongTimeout > 0 {
			return w.LongTimeout
		}
		return 12 * time.Hour
	}
	if w.Timeout > 0 {
		return w.Timeout
	}
	return 60 * time.Minute
}

// stallAfter 是"多久没新内容算卡死"。填 0 或负数用默认 5 分钟，没有关掉这一档的口子：
// 关掉之后只剩 12 小时那道闸，真卡死的下载要白等十二个小时才在界面上说一句话。
func (w *Worker) stallAfter() time.Duration {
	if w.Stall > 0 {
		return w.Stall
	}
	return 5 * time.Minute
}

// moveClock 记"最近一次读到新内容"的时刻，判停就靠它。
//
// 为什么不用墙钟上限了事：整包大小和带宽都是机器的属性，写死的数字换一台就错
// （同一台云机在一次任务里实测过 33 KB/s 和 29 MB/s），而"输出还在不在往前刷"
// 是它自己给的答案。也不用"本体字节在不在涨"：steamcmd 先给自己做自更新，
// 那三十几分钟一个本体字节都没有（云机实测），只看字节会把一条正常的线判死。
//
// 写它的是读输出的 goroutine，读它的是看门狗，两条 goroutine 碰同一个时刻，所以带锁。
type moveClock struct {
	mu sync.Mutex
	at time.Time
	// prev 是上一行原文：只有行内容变了才算进展。这一条是"新内容"的最低门槛，防的是同一行反复重绘
	// （steamcmd 的进度行以 \r 结尾，同一个数字可以连着刷好几行）。
	// 更正一次：这里原先写的"云机实测过卡死的形态就是同一行十几行不变"没有依据——机器上
	// $HOME/Steam/logs/bootstrap_log.txt 里 3359 行 `Downloading update`，去掉时间戳后每一行都不同，
	// KB 计数一直在往前。真正看到的坏形态是另一回事：4 轮 `Startup - updater`、一次
	// `Download failed: http error 0`、整包大小从 35,399 KB 换成 41,370 又换成 41,429 KB，
	// 也就是失败之后从头重下。那种情况下行一直在变，判停不会动手，兜住它的是 LongTimeout 那道闸。
	prev string
}

// start 定下计时起点：进程还没吐第一行之前，"空闲"也从这一刻算起。
func (c *moveClock) start(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = now
}

// see 收到一行输出：和上一行不同才把时刻推到 now。
func (c *moveClock) see(line string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if line != "" && line != c.prev {
		c.prev, c.at = line, now
	}
}

// idle 是"距离上一次新内容过了多久"。一行都没读到过时返回 0，
// 由调用方在起进程前先用 start 定好起点，否则 steamcmd 还没吐字就被判死。
func (c *moveClock) idle(now time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() {
		return 0
	}
	return now.Sub(c.at)
}

// watchStall 起一条看门狗：连续 stall 那么久没有新内容，就取消返回的 ctx，
// 于是外部命令被 kill、RunLines 带错误回来。
// 返回的 stop 负责收掉这条 goroutine，返回的 fired 用来分清"是被判停杀的还是自己死的"——
// 界面上那两句话说法完全不同，不能都推给"steamcmd 更新失败"。
func (w *Worker) watchStall(ctx context.Context, clock *moveClock, stall time.Duration) (context.Context, func(), func() bool) {
	runCtx, cancel := context.WithCancel(ctx)
	var fired atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 检查间隔取窗口的十分之一，最短 250 毫秒：
		// 太疏会让真卡死的那条线在界面上多转好几分钟，太密是白烧 CPU。
		interval := stall / 10
		if interval < 250*time.Millisecond {
			interval = 250 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-runCtx.Done():
				return
			case now := <-ticker.C:
				if idle := clock.idle(now); idle >= stall {
					w.log().Warn("外部命令连续没有新输出，判为卡住并中断",
						"idle", idle.String(), "stall", stall.String())
					fired.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return runCtx, func() {
		close(stop)
		<-done
		cancel()
	}, func() bool { return fired.Load() }
}

// humanTimeout 把一个时长写成界面上能读的那句话。
// 不用 time.Duration.String()：它给的是 1h0m0s 这种 Go 字面量，
// 出现在任务中心就像程序内部错误串味到了界面上。
func humanTimeout(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d 毫秒", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		if s := int(d.Seconds()) - m*60; s > 0 {
			return fmt.Sprintf("%d 分 %d 秒", m, s)
		}
		return fmt.Sprintf("%d 分钟", m)
	default:
		h := int(d.Hours())
		if m := int(d.Minutes()) - h*60; m > 0 {
			return fmt.Sprintf("%d 小时 %d 分钟", h, m)
		}
		return fmt.Sprintf("%d 小时", h)
	}
}

// Run 一直领任务直到 ctx 结束。
//
// 返回时如果还有任务在执行，那条会停在"执行中"——面板启动时会调
// store.RecoverInterruptedTasks 把它标成失败并提示可以重提，不会留下永久转圈的行。
func (w *Worker) Run(ctx context.Context) {
	if w.Store == nil || w.Lines == nil {
		return
	}
	poll := w.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if done := w.runOnce(ctx); !done {
			select {
			case <-ctx.Done():
				return
			case <-time.After(poll):
			}
		}
	}
}

// ---- 有限次自动重试 ----

// transientError 标记"这次失败是网络/瞬时故障，值得原样重跑一次"。
//
// 为什么用私有类型而不是按错误文本匹配：只有本包里明确认定的那几处（steamcmd
// 下载链路）会包它，参数错误、配置缺失、分片还在跑、用户取消一概不会。
// 于是"哪些错误能重试"是一份编译期看得清的白名单，不会哪天有人改了半句中文
// 提示就误把"参数填错"重跑两遍。现有测试构造的都是普通 error，asTransient
// 对它们一律为假，重试循环完全不生效——状态机与既有测试语义不变。
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

func asTransient(err error) bool {
	var t *transientError
	return errors.As(err, &t)
}

// maxAutoRetries 是自动重试的次数上限（不含第一次执行）：失败最多再跑 2 次。
const maxAutoRetries = 2

// retryBackoff 是指数退避：第 1 次 2 秒、第 2 次 4 秒。只对被标记的瞬时故障用，
// 不会把"参数填错"这类立刻能判死的失败拖上几秒。
func retryBackoff(retry int) time.Duration {
	d := 2 * time.Second
	for i := 1; i < retry; i++ {
		d *= 2
	}
	return d
}

// runOnce 领取并执行一条任务；返回有没有干过活。
func (w *Worker) runOnce(ctx context.Context) bool {
	// 面板正在退出时不接新活：接了也只是立刻变成"被中断"，白写一行任务。
	if ctx.Err() != nil {
		return false
	}
	t, err := w.Store.ClaimTask()
	if err != nil {
		if !errors.Is(err, store.ErrNoTask) {
			w.log().Error("领取任务失败", "err", err.Error())
		}
		return false
	}
	taskCtx, cancel := context.WithTimeout(ctx, w.timeoutFor(t.Kind))
	defer cancel()

	w.log().Info("任务开始", "id", t.ID, "kind", t.Kind, "title", t.Title)
	var lastReport string
	var lastPercent int
	report := func(percent int, message string, detail *store.TaskDetail) {
		lastReport = message
		lastPercent = percent
		if err := w.Store.SetTaskProgress(t.ID, percent, clipMessage(message), detail); err != nil {
			w.log().Error("写任务进度失败", "id", t.ID, "err", err.Error())
		}
	}

	// 有限次自动重试：只对被 transientError 明确标记的瞬时/网络故障生效，
	// 参数错误、配置缺失、用户取消、面板退出、任务硬超时一概不重试。
	// 重试期间任务保持 Running（状态机不变）；retryCount 与下次重试时间写进任务的
	// message 和运行日志，界面上看得到"第 N 次重试、X 秒后再试"。重试耗尽才落到
	// 下面的 FinishTask 置终态。
	var runErr error
	retryCount := 0
	for {
		runErr = w.dispatch(taskCtx, t, report)
		if !asTransient(runErr) || ctx.Err() != nil || taskCtx.Err() != nil ||
			errors.Is(runErr, context.Canceled) || retryCount >= maxAutoRetries {
			break
		}
		retryCount++
		delay := retryBackoff(retryCount)
		nextAt := time.Now().Add(delay)
		retryMsg := fmt.Sprintf("第 %d/%d 次自动重试：上次是瞬时故障（%s），%s后重试；下次重试时间 %s",
			retryCount, maxAutoRetries, clip(runErr.Error()), humanTimeout(delay),
			nextAt.UTC().Format(time.RFC3339))
		w.log().Warn("任务遇到瞬时故障，准备自动重试",
			"id", t.ID, "kind", t.Kind, "retryCount", retryCount,
			"nextRetryAt", nextAt.UTC().Format(time.RFC3339), "err", runErr.Error())
		// 直接写进度而不经 report 闭包：重试说明不是"执行进度"，别让它覆盖
		// lastReport（收尾那句"完成：…"要带的是最后一步在做什么），进度也别倒退回 0。
		if err := w.Store.SetTaskProgress(t.ID, lastPercent, clipMessage(retryMsg), nil); err != nil {
			w.log().Error("写自动重试说明失败", "id", t.ID, "err", err.Error())
		}
		select {
		case <-ctx.Done():
			runErr = ctx.Err()
		case <-taskCtx.Done():
			runErr = taskCtx.Err()
		case <-time.After(delay):
		}
		if ctx.Err() != nil || taskCtx.Err() != nil {
			break
		}
	}

	status, message := store.TaskSucceeded, "完成"
	if lastReport != "" {
		// 收尾消息带上最后一步在做什么，用户回看任务中心时才知道"完成"完成到哪。
		message = clipMessage("完成：" + lastReport)
	}
	if runErr != nil {
		// 这些错误都由本包生成，本来就是中文；外部命令的原文进运行日志，界面只留结论。
		status = store.TaskFailed
		message = clipMessage(runErr.Error())
		w.log().Error("任务失败", "id", t.ID, "kind", t.Kind, "err", runErr.Error())
	}
	if ctx.Err() != nil {
		// 面板正在退出：外部命令已经被 kill，这条任务不能再算在跑。
		status, message = store.TaskFailed, "面板退出时任务被打断，确认没有残留文件后可以直接重提"
	}
	if err := w.Store.FinishTask(t.ID, status, message); err != nil {
		w.log().Error("结束任务失败", "id", t.ID, "status", status, "err", err.Error())
	}
	w.log().Info("任务结束", "id", t.ID, "kind", t.Kind, "status", status, "message", message)
	return true
}

func (w *Worker) dispatch(ctx context.Context, t store.Task, report ProgressFn) error {
	switch t.Kind {
	case KindServerUpdate:
		return w.serverUpdate(ctx, report)
	case KindBackupCreate:
		return w.backupCreate(ctx, t, report)
	case KindBackupRestore:
		return w.backupRestore(ctx, t, report)
	case KindBackupImport:
		return w.backupImport(ctx, t, report)
	default:
		return fmt.Errorf("面板不认识任务类型 %q，可能是任务表被改过", t.Kind)
	}
}

// waitSaveStable 是"c_save() 之后等落盘稳定"在 worker 侧的接线。
//
// 它替换 worker/backup.go 的 flushSaves 里那段固定的 time.After(saveFlushWait)：
// 最短仍等 2 秒（保留原行为），最多 30 秒，期间反复采样存档目录的 size+mtime
// 指纹，连续两次不变才认为引擎写完了。返回的这句话要拼进任务收尾消息——
// 超时/采样失败的 Warning 尤其不能吞，那样等于假装存盘成功。
// 用 backup 包自己的默认常量而不引 saveFlushWait，免得两处常量哪天被改成不一样。
func (w *Worker) waitSaveStable(ctx context.Context, cluster string) string {
	res := backup.WaitSaveStable(ctx, w.Paths, cluster,
		backup.DefaultSaveMinWait, backup.DefaultSaveMaxWait)
	if res.Warning != "" {
		return "存盘落盘没等到稳定：" + res.Warning
	}
	if res.Note != "" {
		return res.Note
	}
	return fmt.Sprintf("已等存档落盘稳定后打包（等了 %s，采样 %d 次，%d 个文件）",
		res.Waited, res.Sampled, res.Files)
}

// serverUpdate 跑 steamcmd 更新服务端本体。
//
// 参数是写死的：kind、appid、安装目录都来自配置和白名单，任务 payload 里没有任何
// 能变成命令的东西。steamcmd 的 "+cmd" 参数本身就是指令，绝不能拼用户输入。
func (w *Worker) serverUpdate(ctx context.Context, report ProgressFn) error {
	if err := w.checkNoShardRunning(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(w.SteamCmd) == "" {
		return fmt.Errorf("配置里 paths.steamCmd 是空的，没法下载或更新服务端本体；填上 steamcmd.sh 的绝对路径后重启面板")
	}
	if _, err := os.Stat(w.SteamCmd); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("找不到 steamcmd %s，先跑部署包里的 start.sh 装它，或在配置里指到已经装好的位置", w.SteamCmd)
		}
		return fmt.Errorf("读不到 steamcmd %s: %w", w.SteamCmd, err)
	}
	args := []string{
		// 这一行不能省：steamcmd 按"当前跑在什么系统上"选 depot，同一台机器上
		// 不指定就会拉到 Windows 那份（bin64 里全是 .exe/.dll），
		// 症状是面板显示"本体已更新"，启动分片时却说找不到服务端二进制。
		"+@sSteamCmdForcePlatformType", "linux",
		"+force_install_dir", w.Paths.DstInstall,
		"+login", "anonymous",
		"+app_update", dst.ServerAppID, "validate",
		"+quit",
	}
	verb := "更新"
	if !w.Paths.EnginePresent() {
		// 目录不存在时 app_update 会自己建并整份下载，命令与参数一个都不用换。
		// 报告里把这一区分出来，否则全新机器上的人会以为"更新"是个只对已有安装可用的动作。
		verb = "下载"
	}
	report(1, fmt.Sprintf("开始%s服务端本体到 %s：正在连接 steam，整包大小要等 steamcmd 报出来才知道", verb, w.Paths.DstInstall), nil)

	// 进度只从输出里认"数字%"，取到的值单调不减：
	// steamcmd 在切换阶段时会重新从 0% 开始打印，倒着跳会让界面看起来像卡死或重跑。
	// 地板从 1 起，因为上面那条开局报告已经把界面推到 1%：
	// 若从 0 起，紧接着的 steamcmd 自更新那一段会把条子从 1% 拉回 0%，
	// 而那点位移正是要避免的"看起来像重跑"。
	clock := &moveClock{}
	// 起点定在拉起进程这一刻：steamcmd 从被拉到吐第一行之间也要时间，
	// 不定起点的话看门狗会把这段安静当成"卡死"。
	clock.start(time.Now())
	stall := w.stallAfter()
	state := &progressState{report: report, max: 1, clock: clock}
	runCtx, stopWatch, stalled := w.watchStall(ctx, clock, stall)
	defer stopWatch()
	err := w.Lines.RunLines(runCtx, w.SteamCmd, args, state.feed)
	if err != nil {
		// 三种"没跑完"在界面上是三句话，混成一句就成了"不知道发生了什么"：
		// 被看门狗判死、跑满硬上限、被自己的退出带走。前两种的说法要避免两个雷——
		// Go 的时长字面量（1h0m0s）和 context deadline exceeded 这种裸英文，
		// 这句是会原样出现在任务中心的。
		switch {
		case stalled():
			// 链路不通/长时间没输出是典型的瞬时网络故障，交给 runOnce 的有限次重试。
			return &transientError{fmt.Errorf("%s服务端本体被中断：连续 %s 没有读到任何新内容，判定这条线已经卡死。多半是本机到 steam 的链路不通，可以在服务器上手动跑一次 steamcmd 验证连通，再回来重提这条任务",
				verb, humanTimeout(stall))}
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			// 跑满硬上限不是瞬时故障：重试只是重复等一整轮，照样下不完，不重试。
			return fmt.Errorf("%s服务端本体被中断：跑满了单条任务 %s 的硬上限。整包 %s，这一段时间下不完；要一次下完可以在配置 tasks.timeoutMinutes 里放宽上限，或者换带宽更大的机器",
				verb, humanTimeout(w.timeoutFor(KindServerUpdate)), humanBytes(state.total))
		}
		if errors.Is(err, procrun.ErrCommandMissing) {
			return fmt.Errorf("面板执行不了 steamcmd：%w；确认 %s 有可执行权限且在 PATH 可达的位置", err, w.SteamCmd)
		}
		// steamcmd 自己报错、半路断链、下载失败都落在这里，是值得重跑的网络类故障。
		// 包成 transientError 让 runOnce 有限次重试；参数/配置错误走不到这一行。
		return &transientError{fmt.Errorf("steamcmd 更新失败，最后一行输出：%s；原因：%w", clip(state.last), err)}
	}
	// 报版本号而不是只报"成功"：客户端连不上时的报错是"服务端版本太旧"，
	// 这里给出的构建号正是用户要去和客户端对的那个数字。
	// detail 传 nil 把那组字节数清掉：跑完之后"实测速度"已经没有意义，
	// 留在界面上反而像还在下载。中途失败时不清，最后停在哪个量上一目了然。
	if v := w.Paths.EngineVersion(); v != "" {
		report(100, fmt.Sprintf("服务端本体已%s，构建号 %s，可以启动世界了", verb, v), nil)
	} else {
		report(100, fmt.Sprintf("服务端本体已%s，可以启动世界了", verb), nil)
	}
	return nil
}

func (w *Worker) checkNoShardRunning(ctx context.Context) error {
	if w.Procs == nil {
		return nil
	}
	sessions, err := w.Procs.Sessions(ctx)
	if err != nil {
		// 问不到进程状态时选择放行而不是拦住：screen 不在的场景下，
		// 拦死会让"更新本体"这个动作永远做不了，而覆盖文件本身是可重复的。
		w.log().Warn("更新前问不到世界进程状态，跳过检查", "err", err.Error())
		return nil
	}
	if len(sessions) == 0 {
		return nil
	}
	names := make([]string, 0, len(sessions))
	for name := range sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("%w：%s，本体更新要先把它们全关掉", ErrShardsRunning, strings.Join(names, "、"))
}

// depotProgressRe 认的是 steamcmd 的"整包"进度行。这一行是从云机上真跑一次
// app_update 343050 抓下来的，形状固定：
//
//	Update state (0x61) downloading, progress: 1.07 (49005024 / 4594664662)
//
// 括号里是「已下字节 / 整包字节」，两个都由 steamcmd 自己累计好了，
// 不是单个文件的百分比——所以整包大小只能从这一行读，绝不能写死：
// 换构建、换平台这个数就变（同一次实测总量 4594664662B）。速度也一样，
// 只能从相邻两行的字节差算，换一台带宽不同的机器数字自然跟着变。
var depotProgressRe = regexp.MustCompile(`progress:\s*([0-9]+(?:\.[0-9]+)?)\s*\(\s*([0-9]+)\s*/\s*([0-9]+)\s*\)`)

// depotLine 判定：只有 app_update 那台状态机自己报的行才算"本体在动"。
// 这条边界是 2026-11-01 云机实测画出来的：steamcmd 一启动先给自己做自更新，
// 那一段打的是 `[ 37%] Downloading update...`、`[ NN%] Downloading package steamcmd_bins_linux...`
// 之类的行，百分比是它自己那两百来兆的，和本体毫无关系——按"看见 % 就收"处理时，
// dedicated_server 里一个字节都还没有，界面上的条子已经爬到 37%，还顶着"下载服务端本体文件"的名字。
var depotStateRe = regexp.MustCompile(`update state \(0x[0-9a-f]+\)`)

func isDepotLine(line string) bool {
	return depotProgressRe.MatchString(line) || depotStateRe.MatchString(line)
}

// percentRe 是退路：某些 steamcmd 版本只报 `[ 12%] Update state (0x61) downloading`
// 这种带百分号、没有括号字节量的行。只允许用在已经确认是 depot 的行上。
var percentRe = regexp.MustCompile(`(\d{1,3})\s*%`)

// depotStages 是本体那台状态机里的阶段。认不出具体阶段时统一说"正在处理本体文件"，
// 不猜——猜错一个阶段名，界面就成了一本正经的假话。
//
// 这张表逐条对着 2026-11-01 云机 /data/dst/tools/steamcmd/linux32/logs/console_log.txt
// 里的原文写：0x61 downloading（真从网络下来）、0x5 verifying install 与
// 0x81 verifying update（读磁盘比对）、0x11 preallocating、0x101 committing、
// 0x3 reconfiguring、0x0 unknown。注意是 "verifying install" 不是 "verifying installation"——
// 差一个后缀就认不出来，那次 validate 全程被报成"正在处理本体文件"。
// 只有 downloading 那一档能叫"已下"，所以下面单独标一个 needle 给它。
var depotStages = []struct {
	needle, label string
	download      bool
}{
	{"downloading", "下载服务端本体文件", true},
	{"verifying install", "校验已下载的文件", false},
	{"verifying update", "校验已下载的文件", false},
	{"preallocating", "预留磁盘空间", false},
	{"committing", "写入安装目录", false},
	{"reconfiguring", "整理安装目录", false},
}

// prepStages 是 depot 之外那些行。这里的 downloading/installing 指的是 steamcmd 自己，
// 必须说清楚，否则用户以为本体在下载，实际面板还在等它更新完自己。
var prepStages = []struct {
	needle, label string
}{
	{"checking for available updates", "检查有没有新版本"},
	{"verifying installation", "校验安装状态"},
	{"downloading update", "steamcmd 正在更新自己"},
	{"downloading package", "steamcmd 正在更新自己"},
	{"unpacking", "steamcmd 正在更新自己"},
	{"loading steam api", "正在加载 steam 接口"},
	{"connecting", "正在连 steam"},
}

type progressState struct {
	report ProgressFn
	last   string
	// clock 是"输出里有没有新进展"的时钟，判停靠它而不是靠墙钟。为 nil 时不记
	// （单测里直接 observe 的那批用例不需要）。
	clock *moveClock
	// max 是已经报出去过的整包百分比（单调不减的地板）。
	max int
	// total 是最近一次读到的整包字节数；0 表示 steamcmd 还没给过总量。
	total int64
	// done/baseBytes/baseAt 是速度采样的当前值与上一个采样点。
	done      int64
	baseBytes int64
	baseAt    time.Time
	bytesPerS float64
	// stage 是最近一次认出来的阶段名，没有进度数字的行靠它维持消息。
	stage string
	// downloading 表示当前这一档字节量真的从网络下来（depot 的 0x61）。
	// 校验、提交走的是同一套字节量，那时候说"已下"就是把读磁盘说成下网络。
	downloading bool
	// detail 是进度条背后那组数的最新快照，nil 表示还没有字节可报（还在 steamcmd 自更新）。
	detail *store.TaskDetail
	// reported/message 是上一次真正写进库的值，next 是下一次允许报的时间。
	reported int
	message  string
	next     time.Time
}

// feed 处理 steamcmd 的每一行输出。
//
// 进度条一秒能刷几十行，全写库的话 SQLite 的写锁会把接口拖住，所以同一百分比
// 只在文本变化超过半秒时才落一次；失败时最后一条完整输出另存在 last 里，不受节流影响。
func (p *progressState) feed(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	p.last = line
	now := time.Now()
	if p.clock != nil {
		p.clock.see(line, now)
	}
	percent, message := p.observe(line, now)
	if p.report == nil {
		return
	}
	if percent == p.reported && message == p.message && now.Before(p.next) {
		return
	}
	p.reported, p.message, p.next = percent, message, now.Add(500*time.Millisecond)
	p.report(percent, message, p.buildDetail())
}

// observe 把一行输出换算成（整包百分比，界面消息）。
// 百分比只增不减：steamcmd 在多 depot 之间、以及下载转成校验时会把数字打回
// 起点，倒着跳会让界面看起来像重跑或卡死。
//
// 只有 depot 行能推动百分比。bootstrap 阶段那些 `[ NN%]` 是 steamcmd 在量自己那两百兆，
// 收进来就会造出"本体还没开始下、条子已经三成"的假象（云机实测）。
func (p *progressState) observe(line string, now time.Time) (int, string) {
	lower := strings.ToLower(line)
	depot := isDepotLine(line)
	p.setStage(lower, depot)

	if m := depotProgressRe.FindStringSubmatch(line); m != nil {
		done, _ := strconv.ParseInt(m[2], 10, 64)
		total, _ := strconv.ParseInt(m[3], 10, 64)
		// 状态机换档时会夹一行 `Update state (0x0) unknown, progress: 0.00 (0 / 0)`：
		// 两个数都是空的是它一次空转的状态打印，对字节量一无所知（云机实测：校验跑到 96% 时打的）。
		// 要是跟着它把已处理量清成 0，界面就从"已处理 2.3 GB"倒回"已处理 0"，看着像重跑，
		// 速度那一段也会被这次倒退白白重置掉。带真实总量的 (0 / 总量) 不算在内——
		// 那是换 depot 重新起表，总量本身换了。
		if done == 0 && total == 0 {
			return p.max, p.progressLine()
		}
		p.done = done
		if total > 0 {
			p.total = total
		}
		p.sampleSpeed(now)
		switch {
		case p.total > 0:
			// 知道整包多大之后就只按字节算百分比。steamcmd 自己那列 progress: 是小数百分比，
			// 它量的是"这一步状态机走到哪"，不是"这 4.3 GB 下了多少"：
			// 云机实测开局有一行 `progress: 18.xx (0 / 4594664662)`，跟着它走的话
			// 条子一上来就停在 18%，而括号里的字节才 200 MB（真实是 4%），
			// 界面会出现"已下 201 MB / 4.3 GB（18%）"这种自相矛盾的一句话。
			p.advance(int(p.done * 99 / p.total))
		default:
			// 连总量都没见过：只能退回 steamcmd 自己算的那个小数百分比，至少条子会动。
			if pct, err := strconv.ParseFloat(m[1], 64); err == nil {
				p.advance(int(pct))
			}
		}
		return p.max, p.progressLine()
	}
	if depot {
		// 是 depot 的行，但这版 steamcmd 没在括号里给字节量：只能退回它自己的百分数。
		// 已经知道总量时不退回——那个数量的不是这一包，混进条子就会和文字那半句对不上。
		if p.total == 0 {
			if ms := percentRe.FindAllStringSubmatch(line, -1); len(ms) > 0 {
				if n, err := strconv.Atoi(ms[len(ms)-1][1]); err == nil && n <= 100 {
					p.advance(n)
				}
			}
			return p.max, p.stageLine()
		}
		return p.max, p.progressLine()
	}
	return p.max, p.stageLine()
}

// setStage 记住最近一次的阶段名。没有进度数字的行靠它维持界面那句话。
// 两张表分开走：depot 之外那些行说的都是 steamcmd 自己，永远不该被当成"本体在下来"。
func (p *progressState) setStage(lower string, depot bool) {
	if depot {
		for _, s := range depotStages {
			if strings.Contains(lower, s.needle) {
				p.stage, p.downloading = s.label, s.download
				return
			}
		}
		p.stage, p.downloading = "正在处理本体文件", false
		return
	}
	for _, s := range prepStages {
		if strings.Contains(lower, s.needle) {
			p.stage, p.downloading = s.label, false
			return
		}
	}
}

// doneWord 是那句话里"已下/已处理"的分寸。
func (p *progressState) doneWord() string {
	if p.downloading {
		return "已下"
	}
	return "已处理"
}

// advance 抬升地板，但下载途中最多到 99：剩下的 1% 由任务结束时面板自己补，
// 那样"到 100%"才等于"文件真的落完了、可以起服了"，而不是"字节下完了还要校验"。
func (p *progressState) advance(n int) {
	if n < 0 {
		n = 0
	}
	if n > 99 {
		n = 99
	}
	if n > p.max {
		p.max = n
	}
}

// sampleSpeed 用相邻采样点的字节差算速度，窗口至少 2 秒：
// steamcmd 一行刷几十次，两行之间的差值太小，除出来的数字只会来回跳。
func (p *progressState) sampleSpeed(now time.Time) {
	if p.baseAt.IsZero() {
		p.baseAt, p.baseBytes = now, p.done
		return
	}
	elapsed := now.Sub(p.baseAt).Seconds()
	if elapsed < 2 {
		return
	}
	delta := p.done - p.baseBytes
	if delta <= 0 {
		// 换 depot 时已下字节会归零：旧速度对新的字节流没有意义，重新采一窗。
		p.baseAt, p.baseBytes, p.bytesPerS = now, p.done, 0
		return
	}
	inst := float64(delta) / elapsed
	if p.bytesPerS > 0 {
		p.bytesPerS = p.bytesPerS*0.5 + inst*0.5
	} else {
		p.bytesPerS = inst
	}
	p.baseAt, p.baseBytes = now, p.done
}

// progressLine 拼界面上的那一句。全程只用输出里读到的数：
// 总量、速度、剩余时间没有一个来自"这台机器大概多快"的猜测。
func (p *progressState) progressLine() string {
	if p.total <= 0 {
		return fmt.Sprintf("%s，%s %s，整包大小 steamcmd 还没报出来", p.stageName(), p.doneWord(), humanBytes(p.done))
	}
	msg := fmt.Sprintf("%s，%s %s / %s（%d%%）", p.stageName(), p.doneWord(), humanBytes(p.done), humanBytes(p.total), p.max)
	if p.bytesPerS <= 0 {
		return msg + "，速度还在测第一窗"
	}
	msg += fmt.Sprintf("，实测 %s/s", humanBytes(int64(p.bytesPerS)))
	switch remain := float64(p.total-p.done) / p.bytesPerS; {
	case remain > 0 && remain < 60:
		// "约还需 不到 1 分钟"是两句废话叠在一起，直接说快好了。
		if p.downloading {
			msg += "，就要下完了"
		} else {
			msg += "，就要处理完了"
		}
	case remain >= 60 && remain < 24*3600:
		msg += "，约还需 " + roughDuration(remain)
	}
	return msg
}

func (p *progressState) stageName() string {
	if p.stage != "" {
		return p.stage
	}
	return "服务端本体正在处理"
}

// buildDetail 把进度条背后那组数交给库和界面。
// 还没有任何 depot 字节量时返回 nil：steamcmd 自更新那一段确实没在下本体，
// 界面上宁可只有那句话，也不要一排 0 看着像下载坏了。
func (p *progressState) buildDetail() *store.TaskDetail {
	if p.total <= 0 && p.done <= 0 {
		return nil
	}
	d := store.TaskDetail{
		DoneBytes:   p.done,
		TotalBytes:  p.total,
		BytesPerSec: p.bytesPerS,
		Phase:       p.stageName(),
		Downloading: p.downloading,
	}
	if p.bytesPerS > 0 && p.total > p.done {
		d.EtaSeconds = float64(p.total-p.done) / p.bytesPerS
	}
	return &d
}

// stageLine 是没有进度数字的行（连接 steam、检查更新这些）：
// 界面不跟着刷英文原文，只显示认得出的阶段名。
func (p *progressState) stageLine() string {
	if p.stage != "" {
		return p.stage
	}
	return fmt.Sprintf("%s，进度等 steamcmd 报数", p.stageName())
}

// roughDuration 是"约还需多久"的中文说法，只到分钟级的粗粒度：
// 速度本身是估的，给出秒只会让人觉得这数字更可信，不会更准。
func roughDuration(seconds float64) string {
	switch {
	case seconds < 3600:
		return fmt.Sprintf("%d 分钟", int(seconds/60))
	default:
		h := int(seconds / 3600)
		if m := int((seconds - float64(h)*3600) / 60); m > 0 {
			return fmt.Sprintf("%d 小时 %d 分钟", h, m)
		}
		return fmt.Sprintf("%d 小时", h)
	}
}

// clip 把外部命令的一行输出压进 message 字段。
func clip(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= 200 {
		return string(r)
	}
	return string(r[:200]) + "…"
}

// clipMessage 压的是面板自己拼的任务结论，和 clip 不是一回事：
// 这些句子（要不要重启、退路是哪一份快照、哪些配置没被覆盖）越往后越是重点，
// 拿外部命令那 200 字的尺子去量，截掉的正好是用户唯一需要读完的半句（实测踩过，
// 恢复成功的消息被切成 "…（Caves/server.ini、Master/server.i…"）。
// 上限给到 600 字，日常一条都装得下；真装不下就退到最后一条分号——
// 宁可少说一句也不留下半个结论。
func clipMessage(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= 600 {
		return string(r)
	}
	cut := string(r[:600])
	if i := strings.LastIndex(cut, "；"); i > 0 {
		return cut[:i] + "…"
	}
	return cut + "…"
}
