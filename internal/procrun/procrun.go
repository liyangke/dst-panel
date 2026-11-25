package procrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"dstpanel/internal/dst"
)

// Runner 抽象一次命令执行，让"到底往 screen 传了什么参数"可以被单测钉死。
// 真实实现直接 exec 二进制，不经过 shell：控制台命令本身就是 Lua，
// 再套一层 shell 解析等于把用户输入二次解释，注入面会变大。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner 是本机执行器。面板与服务端同机部署，screen/游戏进程都归它管。
type ExecRunner struct {
	// Timeout 是单条命令的兜底超时，screen/pgrep 都是毫秒级操作。
	Timeout time.Duration
}

func (e ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}
	bin, err := lookPath(name)
	if err != nil {
		// 带上 ErrCommandMissing 让 HTTP 层能给出"装 screen"这种可操作的结论，
		// 而不是把 exec 的英文原文（含 %PATH%）甩到界面上。
		return "", fmt.Errorf("%w %s: %w", ErrCommandMissing, name, err)
	}
	cmd := commandContext(ctx, bin, args...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	text := out.String()
	if err != nil {
		// screen/pgrep 用退出码表达"没找到"，属于正常语义，不算故障。
		if isNotFoundExit(err) {
			return text, nil
		}
		return text, fmt.Errorf("执行 %s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(text))
	}
	return text, nil
}

func isNotFoundExit(err error) bool {
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 1, 3: // pgrep 无匹配=1；screen 无会话=3
			return true
		}
	}
	return false
}

// Driver 用 screen 会话驱动 DST 专用服务端。
//
// 为什么用 screen 而不是 exec.Cmd 直接起：面板自己也要升级重启，
// 子进程会跟着面板一起死，服务器不该因为面板发版而掉线。
type Driver struct {
	Paths  dst.Paths
	Runner Runner
	// ScreenBin 默认 "screen"，测试里可换成假路径。
	ScreenBin string
	// StartWait 是启动后等待会话出现的上限。
	StartWait time.Duration
	// BootLogWait 是"会话已经出现"之后再盯 server_log.txt 的时长：
	// 服务端拒绝启动的那几行就出现在这一两秒里。
	// 留 0 表示不看日志（单测用），真实启动不要关。
	BootLogWait time.Duration
	// ShutdownWait 是停服/重启时确认 screen 会话真的消失的上限；<=0 时用 30 秒。
	ShutdownWait time.Duration

	// opLocks 是 per-shard 操作锁。所有生命周期操作先抢同一把锁，
	// 抢不到就明确报「已有操作进行中」，而不是让两次启动撞上同端口双开。
	opLocks sync.Map // key -> *sync.Mutex
	// phases 存每个分片最近一次启动所处的阶段。
	phases sync.Map // key -> phaseInfo
}

// defaultBootLogWait 是云机实测出来的量级（4h4g，2026-10-30，版本 760944）：
// 假令牌那一次，进程起来后约 4 秒才把 server_log.txt 截断重写，约 5~7 秒才写出
// "!!!! Your Server Will Not Start !!!!"。原来盯 2 秒正好错过，接口把"会话在跑"
// 当成"服务器起来了"报成功——这就是要修的谎。10 秒是留了余量的实测值。
const defaultBootLogWait = 10 * time.Second

func New(paths dst.Paths, runner Runner) *Driver {
	return &Driver{Paths: paths, Runner: runner, ScreenBin: "screen",
		StartWait: 3 * time.Second, BootLogWait: defaultBootLogWait}
}

// SessionName 形如 dst-Cluster_1-Master，一台机器上多集群共存全靠它区分。
func (d *Driver) SessionName(cluster, shard string) string {
	return fmt.Sprintf("dst-%s-%s", cluster, shard)
}

// ErrNotRunning 表示会话不存在，调用方据此决定是提示用户还是直接启动。
var ErrNotRunning = errors.New("世界未在运行")

// ErrAlreadyRunning 挡住重复启动：同端口双开会让第二个进程静默退出，
// 而界面上只看得到"第一次启动成功了"。
var ErrAlreadyRunning = errors.New("世界已在运行")

// ErrPrereq 表示本机环境还不满足开服条件（缺令牌、缺二进制、存档没生成）。
// 用 sentinel 而不是让 HTTP 层去匹配中文文案：文案改一个字，状态码分类就错了。
var ErrPrereq = errors.New("启动前置检查未通过")

// ErrCommandMissing 表示 screen 这类外部命令不在 PATH 里。
// 面板与服务端不同机时必然撞上这条，界面上要能说清"装 screen 或在同机跑面板"。
var ErrCommandMissing = errors.New("本机命令缺失")

// ErrStartRefused 表示会话留住了、进程也活着，但服务端自己在日志里宣布不会启动。
// 和"会话没保持"是两类故障：前者几乎总是令牌问题，后者是缺库/权限/存档。
// 用 sentinel 是因为 HTTP 层要按它选状态码，而不是去匹配中文文案。
var ErrStartRefused = errors.New("服务端拒绝启动")

// ErrOperationInProgress 表示这个分片正有另一个生命周期操作在跑。
// 用 TryLock 而不是阻塞排队：重复点启动的人要的是一句明确结论，
// 而不是等十秒后又一个进程挤上同一个端口。
var ErrOperationInProgress = errors.New("该分片已有操作进行中")

// ErrStopTimeout 表示关闭命令已发出，但在等待上限内没确认会话消失。
var ErrStopTimeout = errors.New("关闭后等待进程退出超时")

// 启动阶段。"命令已发出"和"世界可进入"是两件事，不能混成一个布尔值。
const (
	PhaseStarting = "starting" // 命令已发出，会话还没确认出现
	PhaseLoading  = "loading"  // 进程在、世界还没确认就绪
	PhaseReady    = "ready"    // 确认可进入
	PhaseFailed   = "failed"   // 日志里明确宣布起不来，或会话没保持
)

type phaseInfo struct {
	Phase    string
	Reason   string
	Launched time.Time
}

// readyMarkers 是"世界已经加载/注册完成"的日志原文，取自 internal/gamelog 的规则表
// 与真机日志（Server registered via 在 2026-11-03 云机 Master/server_log.txt 里见过），
// 不在这里凭空造措辞。只认注册/端口监听这类强标志：模组加载完成不等于能进世界。
var readyMarkers = []string{
	"Shard server started on port",      // 端口已监听（gamelog EventShardUp）
	"secondary shard LUA is now ready!", // 洞穴已进入世界（gamelog EventShardReady）
	"Server registered via",             // 已在 Klei 列表注册成功
}

func shardKey(cluster, shard string) string { return cluster + "\x00" + shard }

// acquire 抢这个分片的操作锁。抢不到立即返回 ErrOperationInProgress，不阻塞。
func (d *Driver) acquire(cluster, shard string) (func(), error) {
	v, _ := d.opLocks.LoadOrStore(shardKey(cluster, shard), &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, ErrOperationInProgress
	}
	return mu.Unlock, nil
}

func (d *Driver) setPhase(cluster, shard string, p phaseInfo) {
	d.phases.Store(shardKey(cluster, shard), p)
}

func (d *Driver) clearPhase(cluster, shard string) {
	d.phases.Delete(shardKey(cluster, shard))
}

// Phase 给状态/生命周期接口用，返回 (阶段, 说明)。
// 停在 starting/loading 时顺手再问一次就绪证据：世界加载常常比启动日志窗口慢得多，
// 惰性刷新让几十秒后回来刷状态的人也能看到 ready，而不是永远卡在 loading。
func (d *Driver) Phase(cluster, shard string) (string, string) {
	v, ok := d.phases.Load(shardKey(cluster, shard))
	if !ok {
		return "", ""
	}
	p := v.(phaseInfo)
	if p.Phase == PhaseStarting || p.Phase == PhaseLoading {
		if ok, reason := d.readyEvidence(cluster, shard, p.Launched); ok {
			p.Phase, p.Reason = PhaseReady, reason
			d.setPhase(cluster, shard, p)
		}
	}
	return p.Phase, p.Reason
}

// readyEvidence 回答"这次启动有没有确认可进入的证据"。
// 优先助手 MOD 首份快照（status.json 是本轮启动后写的），其次日志里的注册/加载完成标志。
// 两者都要求文件时间戳不早于发起启动的时刻，免得拿上一轮留下的旧文件谎报就绪。
func (d *Driver) readyEvidence(cluster, shard string, launched time.Time) (bool, string) {
	cutoff := launched.Add(-time.Second)
	sp := d.Paths.HelperStatusFile(cluster, shard)
	if st, err := os.Stat(sp); err == nil && st.Size() > 0 && !st.ModTime().Before(cutoff) {
		return true, "助手 MOD 已写出本轮启动后的首份状态快照，世界确认可进入"
	}
	lp := d.Paths.ServerLog(cluster, shard)
	if st, err := os.Stat(lp); err == nil && !st.ModTime().Before(cutoff) {
		tail, err := d.ReadLogTail(cluster, shard, 400)
		if err == nil {
			for _, m := range readyMarkers {
				if strings.Contains(tail, m) {
					return true, fmt.Sprintf("服务端日志出现 %q，世界已确认加载完成或注册成功", m)
				}
			}
		}
	}
	return false, ""
}

func (d *Driver) shutdownWait() time.Duration {
	if d.ShutdownWait > 0 {
		return d.ShutdownWait
	}
	return 30 * time.Second
}

// waitGone 轮询到会话真的消失。第二个返回值是真故障（screen 读不动），
// 超时用 confirmed=false 表达：命令已经发出，不该把"没确认退出"报成执行失败。
func (d *Driver) waitGone(ctx context.Context, cluster, shard string, wait time.Duration) (bool, error) {
	deadline := time.Now().Add(wait)
	for {
		running, err := d.IsRunning(ctx, cluster, shard)
		if err != nil {
			return false, err
		}
		if !running {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// bootFailures 是实测到的指纹（2026-10-30，云机，服务端 760944）：
// 用一枚无效令牌启动时，screen 会话一直存在、面板当时报"启动成功"，
// 日志里却是下面这几行，而且 [00:00:00] 开头那句每秒重打一次，直到有人去杀进程。
// 所以"会话在跑"不能当启动成功的依据，必须再看一眼这次启动写出来的日志。
//
// 只列真的见过、且语义明确的行；解释里给的是下一步做什么。
type bootFailure struct {
	marker string
	why    string
}

var bootFailures = []bootFailure{
	{"!!!! Your Server Will Not Start !!!!",
		"服务端明确表示不会启动，日志上下文是账户验证失败：基本可以确定是 cluster_token.txt 里的令牌无效或过期，到 Klei 个人中心重新生成一枚"},
	{"No auth token could be found.",
		"服务端没读到有效令牌：确认 cluster_token.txt 在这个集群目录下、内容是一整行没有空格的令牌"},
	{"Level data override is invalid!",
		"手工放置的 leveldataoverride.lua 缺了引擎要求的字段（id/name/desc/location/overrides 五键），" +
			"这份文件会被整份丢弃、世界规则不生效。面板不写这份文件（它要一整份完整世界定义），" +
			"要么删掉它改用面板的世界规则接口，要么自己把五键补齐"},
	{"Must provide complete level data to worldgen.",
		"引擎在 scripts/worldgen_main.lua 里 assert 世界定义必须完整（leveldataoverride.lua 是 GAME USE 文件，" +
			"由客户端下发）。面板走的是 worldgenoverride.lua 的 preset 通道，不会触发这句；" +
			"出现它说明目录里有一份手工写的 leveldataoverride.lua，删掉它再生成一次"},
	// 2026-10-31 云机实测：手写 leveldataoverride.lua 时，即便 id/name/desc/location/overrides
	// 五键齐全、layout_mode 那批内生项也补齐了，仍会死在这一句上——引擎从这张表里直接取
	// required_setpieces / numrandom_set_pieces / random_set_pieces（scripts/map/level.lua:32-34），
	// 少哪一项都不报格式错，而是在设-piece 分配那里炸掉，世界生成失败而进程照活。
	{"attempt to get length of field 'random_set_pieces'",
		"手工写的 leveldataoverride.lua 不是一整份完整世界定义（缺 random_set_pieces 这类预设自带的字段）。" +
			"这份文件面板不写也补不全，删掉它，用面板的世界规则接口（worldgenoverride.lua + 预设）重新生成一次"},
	// 下面两条是云机首启（2026-10-30）真撞出来的：世界生成失败时进程并不退出，
	// screen 会话还在、IsRunning 还是 true，只有日志说明"没进世界"。
	// 不认这两条的话，面板会一直把一个玩家进不来的分片报成运行中。
	{"Worldgen had an error",
		"世界生成失败，服务端没能进入存档（进程还挂着但没人能进来看）。日志里这句上面就是引擎的 Lua 报错原文，按它改世界规则；改完先强制停止再启动"},
	{"Must specify a layout mode for your level.",
		"世界定义缺引擎内生项 layout_mode（来源是 scripts/map/locations.lua 每个地形的默认值，" +
			"不在定制界面那张表上，也不会出现在 worldgenoverride.lua 里）。" +
			"面板走 preset 通道时由引擎自带；出现这句说明目录里有一份手工写的 leveldataoverride.lua，删掉它再生成一次"},
	// 2026-10-31 云机实测：空 overrides 会让引擎在 scripts/map/level.lua:92 直接 assert 掉。
	// 这条和上一条不同——layout_mode 有地形默认值兜底，task_set 没有，面板必须自己写。
	{"Must specify the task set for a level!",
		"世界定义里 task_set 缺失或为空：引擎在 scripts/map/level.lua 里 assert 它，地形默认值不替它兜底；" +
			"面板写世界规则时会写全整套默认值（含这一项），出现这句说明文件被手工删过这一项，重写一次世界规则"},
	// 世界生成没造出必需物件时引擎不会退出，而是把整轮生成推倒重来，每轮都撞同一句，
	// 实测云机 30 分钟里重打了 401 次。进程一直活着、端口一直开着，但没人能进来看。
	{"PANIC: missing required prefab",
		"世界生成造不出引擎要求的必需物件，这一轮失败后引擎在反复推倒重来（这句每隔几秒重打一次），服务端进不了存档。这句上面的几行是引擎列的缺失清单；先强制停止，改一项世界规则再生成一次"},
	// 这句不是致命错误，进程会照常跑起来，但它意味着"用户选的预设没生效"，
	// 状态页不说界面就会一直显示一个引擎根本没接受的预设名。
	// 面板在写入前就按白名单拒掉了未知预设，会出现这句的只有手工改过的文件。
	{"Worldgenoverride specified a nonexistent preset",
		"worldgenoverride.lua 里的 preset 名字引擎查不到，它只是忽略这个预设、按当前存档的设定继续（不是崩溃）。" +
			"这句后面冒号那里就是引擎读到的名字；改回面板可选的预设，或删掉这份文件用默认世界"},
}

// Start 在 screen 会话里拉起分片。先抢 per-shard 操作锁：重复点启动不再
// check-then-act 各查一次、各起一个进程挤同一端口，而是拿到明确错误。
func (d *Driver) Start(ctx context.Context, cluster, shard string) error {
	release, err := d.acquire(cluster, shard)
	if err != nil {
		return err
	}
	defer release()
	return d.startLocked(ctx, cluster, shard)
}

// startLocked 是真正发命令的那一段，假定调用方已经持有这个分片的操作锁。
// Restart 直接调它，避免"Restart 里再调 Start"把自己锁死。
func (d *Driver) startLocked(ctx context.Context, cluster, shard string) error {
	if problems := d.CheckPrereqs(cluster, shard); len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrPrereq, strings.Join(problems, "; "))
	}
	if running, err := d.IsRunning(ctx, cluster, shard); err == nil && running {
		return fmt.Errorf("%w: 集群 %s 的世界 %s", ErrAlreadyRunning, cluster, shard)
	}
	name := d.SessionName(cluster, shard)
	bin := d.Paths.ServerBinary()
	// SteamAppId / SteamGameId 是 Klei 自带 launch_dedicated_server.sh 导的两个变量：
	// 服务端靠它们认定自己属于游戏 322330（工坊内容也挂在这个 app 下）。
	// bin64/steam_appid.txt 能兜底，但没理由只靠兜底跑。
	inner := fmt.Sprintf("cd %s && export SteamAppId=%s SteamGameId=%s && exec %s -console -cluster %s -shard %s",
		quoteShell(filepath.Dir(bin)), dst.GameAppID, dst.GameAppID,
		quoteShell(bin), quoteShell(cluster), quoteShell(shard))
	// 记住发起启动的时刻：日志文件里可能还是上一次启动留下的内容，
	// 拿旧日志里的失败行判这次启动失败会冤枉人；phase 的就绪判定也拿它挡旧文件。
	launched := time.Now()
	d.setPhase(cluster, shard, phaseInfo{Phase: PhaseStarting,
		Reason: "启动命令已发出，正在等待 screen 会话出现", Launched: launched})
	out, err := d.Runner.Run(ctx, d.screenBin(), "-dmS", name, "bash", "-c", inner)
	if err != nil {
		d.setPhase(cluster, shard, phaseInfo{Phase: PhaseFailed,
			Reason: "创建 screen 会话失败：" + err.Error(), Launched: launched})
		return fmt.Errorf("创建 screen 会话 %s: %w (%s)", name, err, strings.TrimSpace(out))
	}
	deadline := time.Now().Add(d.StartWait)
	for time.Now().Before(deadline) {
		if running, _ := d.IsRunning(ctx, cluster, shard); running {
			d.setPhase(cluster, shard, phaseInfo{Phase: PhaseLoading,
				Reason: "进程已在，等待世界就绪确认", Launched: launched})
			return d.awaitBootVerdict(cluster, shard, launched)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 会话没留住，通常是二进制起不来（缺 32 位库、权限、存档损坏）。
	// 把日志尾巴带回去，用户不用再自己去找 server_log.txt。
	tail, _ := d.ReadLogTail(cluster, shard, 20)
	d.setPhase(cluster, shard, phaseInfo{Phase: PhaseFailed,
		Reason: "会话没留住，服务端可能启动失败；server_log.txt 末尾：" + tail, Launched: launched})
	return fmt.Errorf("会话 %s 未能保持，服务端可能启动失败；server_log.txt 末尾:\n%s", name, tail)
}

// awaitBootVerdict 在会话出现之后再盯一小段日志，做两件事：
// 找"服务端自己承认没起来"的失败指纹，以及找"世界确认可进入"的就绪证据。
//
// 只在有证据时下结论：日志还没出现、或者文件还是上一次启动的时间戳，
// 一律不判失败也不判 ready。超时后停在 loading 并写明原因——
// "会话在跑"不等于"世界可进入"，宁可让状态页稍后刷新确认，也不谎报 ready。
func (d *Driver) awaitBootVerdict(cluster, shard string, launched time.Time) error {
	wait := d.BootLogWait
	if wait < 0 {
		wait = 0
	}
	deadline := time.Now().Add(wait)
	for {
		if fail, ok := d.bootFailure(cluster, shard, launched); ok {
			reason := fmt.Sprintf("日志里有 %q。%s", fail.marker, fail.why)
			d.setPhase(cluster, shard, phaseInfo{Phase: PhaseFailed, Reason: reason, Launched: launched})
			return fmt.Errorf("%w: %s", ErrStartRefused, reason)
		}
		if ok, reason := d.readyEvidence(cluster, shard, launched); ok {
			d.setPhase(cluster, shard, phaseInfo{Phase: PhaseReady, Reason: reason, Launched: launched})
			return nil
		}
		if !time.Now().Before(deadline) {
			d.setPhase(cluster, shard, phaseInfo{Phase: PhaseLoading,
				Reason:   fmt.Sprintf("启动后 %s 内未检测到世界就绪标志（助手 MOD 首份快照，或服务器注册/加载完成日志）；停在加载中，稍后刷新状态确认，不把「命令已发出」当成可进入", wait),
				Launched: launched})
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// bootFailure 扫本次启动写出的日志，返回第一条命中的失败指纹。
// 引擎每次启动都会把这份日志截断重写（实测第一行固定是 PersistRootStorage），
// 所以整份文件就是这一次启动的经过，不用做增量。
func (d *Driver) bootFailure(cluster, shard string, launched time.Time) (bootFailure, bool) {
	path := d.Paths.ServerLog(cluster, shard)
	st, err := os.Stat(path)
	if err != nil {
		return bootFailure{}, false
	}
	// 给一秒容差：mtime 在个别文件系统上只有秒级精度（FAT、某些网络挂载），
	// 不容差的话刚写出来的日志会被判成"上一轮的旧文件"，整条检测静默失效。
	// 真正的旧日志是几分钟以前的事，一秒容差不会放过它。
	if st.ModTime().Before(launched.Add(-time.Second)) {
		return bootFailure{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return bootFailure{}, false
	}
	text := string(raw)
	for _, f := range bootFailures {
		if strings.Contains(text, f.marker) {
			return f, true
		}
	}
	return bootFailure{}, false
}

// bootFailureScanLines 是状态页扫日志时看的行数。选这个量级的依据是实测：
// 世界生成每轮约 45 行、每 4~5 秒重打一轮，400 行等于回看了 8 轮——
// 真在死循环里一定命中，早已恢复的历史失败则早就被挤出窗口了。
const bootFailureScanLines = 400

// BootFailureNote 给总览页用：进程活着、助手却没回话时，引擎往往已经在日志里
// 承认了原因（世界生成卡在重试循环里、登录被拒）。不引用这句，界面只能显示
// "助手 MOD 还没写入过状态"，用户完全不知道下一步该做什么。
//
// 只扫日志尾巴：这些指纹在循环里每隔几秒重打一次，正在发生的事一定在尾巴上；
// 拿整份文件去匹配会把几小时前那次早已恢复的失败当成现在的结论。
func (d *Driver) BootFailureNote(cluster, shard string) (string, bool) {
	tail, err := d.ReadLogTail(cluster, shard, bootFailureScanLines)
	if err != nil {
		return "", false
	}
	for _, f := range bootFailures {
		if strings.Contains(tail, f.marker) {
			return fmt.Sprintf("服务端日志里有 %q：%s", f.marker, f.why), true
		}
	}
	return "", false
}

// SimPausedNote 判断这一轮启动此刻是不是正停在"无人暂停"上。
//
// 为什么非得单独问日志：暂停时引擎冻的是整个 Lua 调度器——update.lua:229 有一句
// assert(not TheNet:IsServerPaused())，而驱动所有 DoPeriodicTask 的 RunScheduler
// 全脚本只有 update.lua:240 一个调用点。助手 MOD 的心跳和命令执行都挂在调度器上，
// 于是"进程活着 + 助手没心跳"这一种组合有两种完全不同的含义：真卡死了，或者只是没人玩。
// 分不清就只能报"助手离线"，用户会去重启一个本来好好的服务器。
//
// 标记词是引擎自己的话：C++ 侧打 "Sim paused"/"Sim unpaused"，
// mainfunctions.lua:806-812 的 SetPause 打 "Server Unpaused"/"Server Paused"/"Server Autopaused"。
// 取最后一次出现的方向，因为一次启动里可能反复暂停恢复，最后一句才是现在的状态。
//
// 2026-11-03 在云机核对过真实形态：一个玩家进出之后，地表分片打的是
// "[00:51:46]: Server Autopaused"（带制表符结尾），而 "Sim paused" 一次都没出现过。
// 所以引用哪一句必须按日志里真的找到的那个词来写，不能把推断出来的措辞当成日志原文；
// 而 Autopaused 与其它两种的语义差别也值得分开说：前者是"没人了引擎自己冻的"，
// 后者是有人按下去的暂停——这时候说"无人暂停"就是错的，界面上一眼就能看出矛盾。
func (d *Driver) SimPausedNote(cluster, shard string) (string, bool) {
	tail, err := d.ReadLogTail(cluster, shard, bootFailureScanLines)
	if err != nil {
		return "", false
	}
	lastPaused, lastUnpaused := -1, -1
	pausedMarker := ""
	for _, m := range []string{"Sim paused", "Server Autopaused", "Server Paused"} {
		if i := strings.LastIndex(tail, m); i > lastPaused {
			lastPaused, pausedMarker = i, m
		}
	}
	for _, m := range []string{"Sim unpaused", "Server Unpaused"} {
		if i := strings.LastIndex(tail, m); i > lastUnpaused {
			lastUnpaused = i
		}
	}
	if lastPaused <= lastUnpaused {
		return "", false
	}
	// 暂停时读不到实时状态这一句是两种情形共用的后果。
	const frozen = "引擎在暂停时冻结全部 Lua 任务，助手上报和命令执行都会停，所以这一页读不到实时状态（不是掉线）。"
	if pausedMarker == "Server Autopaused" {
		return "服务端日志最后一条是 " + pausedMarker + "：这个世界此刻没有人在玩，引擎按集群设置自己冻住了。" + frozen +
			"有人进来自会自动恢复；要一直看到实时状态就把集群设置里的「无人时暂停」关掉", true
	}
	return "服务端日志最后一条是 " + pausedMarker + "：这个世界正被暂停（暂停是玩家或控制台按下去的，不是没人）。" + frozen +
		"恢复运行后面板会自动接上实时状态", true
}

// Stop 关闭分片。saveFirst=false 时不落盘直接关，用于"这局不要了"。
//
// 命令形态是从服务端自带的 scripts/consolecommands.lua 里核对过的：
// c_shutdown(save) 的参数本身就控制要不要保存——不传参时 Klei 会先 SaveCurrent 再退出，
// 只有显式传 false 才跳过落盘。所以这里是一条命令搞定，不需要额外的 c_save + 睡眠等待。
//
// 一个改不了的语义：洞穴分片上的 c_shutdown 会被转成 c_remote("c_shutdown()") 发给 Master，
// 参数在转发时丢掉，等于"整簇保存并关闭"。只想单独停洞穴时应该用 Kill。
func (d *Driver) Stop(ctx context.Context, cluster, shard string, saveFirst bool) error {
	release, err := d.acquire(cluster, shard)
	if err != nil {
		return err
	}
	defer release()
	return d.stopLocked(ctx, cluster, shard, saveFirst)
}

// stopLocked 假定操作锁已在手，只负责注入那一条关闭命令，立即返回。
// "停服等落盘"是 StopAndWait 的事：procrun 这层保持"发命令"的语义纯粹，
// 也让只调 Stop 的调用方（worker、测试）不必被等待拖住。
func (d *Driver) stopLocked(ctx context.Context, cluster, shard string, saveFirst bool) error {
	if running, err := d.IsRunning(ctx, cluster, shard); err != nil {
		return err
	} else if !running {
		return fmt.Errorf("%w: %s/%s", ErrNotRunning, cluster, shard)
	}
	line := "c_shutdown()"
	if !saveFirst {
		line = "c_shutdown(false)"
	}
	d.clearPhase(cluster, shard)
	return d.Console(ctx, cluster, shard, line)
}

// StopAndWait 注入关闭命令后，等进程真正退出再返回。
// confirmed=false 表示"命令已发出，但等待上限内没确认退出"——这是明确结论，不是执行失败，
// 生命周期层据此给用户一句能动手的话，而不是把已经发出的命令报成 500。
func (d *Driver) StopAndWait(ctx context.Context, cluster, shard string, saveFirst bool, wait time.Duration) (bool, error) {
	release, err := d.acquire(cluster, shard)
	if err != nil {
		return false, err
	}
	defer release()
	if err := d.stopLocked(ctx, cluster, shard, saveFirst); err != nil {
		return false, err
	}
	if wait <= 0 {
		wait = d.shutdownWait()
	}
	return d.waitGone(ctx, cluster, shard, wait)
}

// Kill 直接结束会话（screen -X quit），用于进程卡死不响应控制台命令。
func (d *Driver) Kill(ctx context.Context, cluster, shard string) error {
	release, err := d.acquire(cluster, shard)
	if err != nil {
		return err
	}
	defer release()
	return d.killLocked(ctx, cluster, shard)
}

func (d *Driver) killLocked(ctx context.Context, cluster, shard string) error {
	name := d.SessionName(cluster, shard)
	out, err := d.Runner.Run(ctx, d.screenBin(), "-S", name, "-X", "quit")
	if err != nil {
		return err
	}
	if strings.Contains(out, "No such session") {
		return fmt.Errorf("%w: %s/%s", ErrNotRunning, cluster, shard)
	}
	d.clearPhase(cluster, shard)
	return nil
}

// Restart 停干净再起，返回"原本是否在跑"。整个停-等-起都在同一把 per-shard 锁里，
// 内部直接调 stopLocked/startLocked（不再各自抢锁），所以不会自己把自己锁死。
func (d *Driver) Restart(ctx context.Context, cluster, shard string) (bool, error) {
	release, err := d.acquire(cluster, shard)
	if err != nil {
		return false, err
	}
	defer release()
	running, err := d.IsRunning(ctx, cluster, shard)
	if err != nil {
		return false, err
	}
	if running {
		if err := d.stopLocked(ctx, cluster, shard, true); err != nil {
			return running, err
		}
		confirmed, err := d.waitGone(ctx, cluster, shard, d.shutdownWait())
		if err != nil {
			return running, err
		}
		if !confirmed {
			return running, fmt.Errorf("%w: 等了 %s，会话 %s 仍在；存档可能正在落盘，确认后可强制停止再重试重启",
				ErrStopTimeout, d.shutdownWait(), d.SessionName(cluster, shard))
		}
	}
	if err := d.startLocked(ctx, cluster, shard); err != nil {
		return running, err
	}
	return running, nil
}

// Console 向分片的 stdin 注入一行 Lua 控制台命令。
func (d *Driver) Console(ctx context.Context, cluster, shard, line string) error {
	if err := ValidateConsoleLine(line); err != nil {
		return err
	}
	name := d.SessionName(cluster, shard)
	out, err := d.Runner.Run(ctx, d.screenBin(), "-S", name, "-X", "stuff", line+"\n")
	if err != nil {
		return err
	}
	if strings.Contains(out, "No such session") {
		return fmt.Errorf("%w: %s/%s（控制台命令需要一个活的会话）", ErrNotRunning, cluster, shard)
	}
	return nil
}

// RunAdminCommand 执行白名单里的一条管理命令，返回实际注入的那行 Lua 供审计日志记录。
// 参数校验和转义都在 dst.BuildConsoleLine 里，这里只负责"送进哪个会话"。
func (d *Driver) RunAdminCommand(ctx context.Context, cluster, shard, key string, args map[string]string) (string, error) {
	line, err := dst.BuildConsoleLine(key, args)
	if err != nil {
		return "", err
	}
	return line, d.Console(ctx, cluster, shard, line)
}

// ValidateConsoleLine 一次只允许一行：多行会让一次调用变成任意脚本执行，
// 也让审计日志与实际操作对不上。
func ValidateConsoleLine(line string) error {
	if strings.TrimSpace(line) == "" {
		return fmt.Errorf("控制台命令不能为空")
	}
	if strings.ContainsAny(line, "\r\n") {
		return fmt.Errorf("一次只能提交一行控制台命令")
	}
	if len(line) > 2000 {
		return fmt.Errorf("控制台命令过长（%d 字符）", len(line))
	}
	return nil
}

// IsRunning 判断会话是否存在。screen 的 -ls 会带窗口标题，这里只按会话名精确匹配。
func (d *Driver) IsRunning(ctx context.Context, cluster, shard string) (bool, error) {
	sessions, err := d.Sessions(ctx)
	if err != nil {
		return false, err
	}
	_, ok := sessions[d.SessionName(cluster, shard)]
	return ok, nil
}

// Sessions 列出本面板管理的 screen 会话（dst- 前缀）及其 PID。
func (d *Driver) Sessions(ctx context.Context) (map[string]int, error) {
	out, err := d.Runner.Run(ctx, d.screenBin(), "-ls")
	if err != nil {
		return nil, err
	}
	return parseScreenList(out), nil
}

// parseScreenList 解析 `screen -ls` 的行：
//
//	20314.dst-Cluster_1-Master	(Detached)
//	24007.dst-Cluster_1-Caves	(Attached)
func parseScreenList(out string) map[string]int {
	res := make(map[string]int)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "dst-") && !strings.Contains(line, ".dst-") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		dot := strings.IndexByte(fields[0], '.')
		if dot < 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0][:dot])
		if err != nil {
			continue
		}
		res[fields[0][dot+1:]] = pid
	}
	return res
}

// Pid 返回分片进程号，未运行返回 0。
func (d *Driver) Pid(ctx context.Context, cluster, shard string) (int, error) {
	sessions, err := d.Sessions(ctx)
	if err != nil {
		return 0, err
	}
	return sessions[d.SessionName(cluster, shard)], nil
}

// CheckPrereqs 汇总"现在起得来吗"的前置条件，返回人类可读的问题清单。
// 面板的价值就在这里：把 Klei 那种"起不来但只写一行 stack trace"变成明确提示。
func (d *Driver) CheckPrereqs(cluster, shard string) []string {
	var problems []string
	if _, err := os.Stat(d.Paths.ServerBinary()); err != nil {
		// 这一条是全新机器上最常见的一条，所以要把下一步写全：光说"找不到二进制"，
		// 人会去装游戏，而面板里就有下载它的任务。
		problems = append(problems, fmt.Sprintf(
			"找不到服务端二进制 %s：这台机器还没下载过饥荒服务端，去【任务中心】提交「下载/更新服务端本体」",
			d.Paths.ServerBinary()))
	}
	if _, err := os.Stat(d.Paths.ClusterIni(cluster)); err != nil {
		problems = append(problems, fmt.Sprintf("集群 %s 没有 cluster.ini", cluster))
	}
	if !shardDirPresent(d.Paths, cluster, shard) {
		problems = append(problems, fmt.Sprintf("集群 %s 没有 %s 世界目录", cluster, shard))
	}
	tok, err := os.ReadFile(d.Paths.TokenFile(cluster))
	switch {
	case os.IsNotExist(err):
		problems = append(problems, "缺少 cluster_token.txt，联机服务器无法登录 Klei 账户")
	case err != nil:
		problems = append(problems, fmt.Sprintf("读取 cluster_token.txt 失败: %v", err))
	case strings.TrimSpace(string(tok)) == "":
		problems = append(problems, "cluster_token.txt 是空的")
	}
	if _, err := os.Stat(d.Paths.ShardIni(cluster, shard)); err != nil {
		problems = append(problems, fmt.Sprintf("缺少 %s/server.ini", shard))
	}
	return problems
}

func shardDirPresent(p dst.Paths, cluster, shard string) bool {
	st, err := os.Stat(p.ShardDir(cluster, shard))
	return err == nil && st.IsDir()
}

// ReadLogTail 读 server_log.txt 末尾若干行，供启动失败与界面"最近日志"用。
func (d *Driver) ReadLogTail(cluster, shard string, lines int) (string, error) {
	raw, err := os.ReadFile(d.Paths.ServerLog(cluster, shard))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	all := strings.Split(strings.TrimRight(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n"), nil
}

func (d *Driver) screenBin() string {
	if d.ScreenBin == "" {
		return "screen"
	}
	return d.ScreenBin
}

// quoteShell 给 bash -c 内部的参数加引号。路径来自配置文件，加引号是纵深防御，
// 顺便让带空格的路径可用。
func quoteShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
