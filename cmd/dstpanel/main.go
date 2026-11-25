// cmd/dstpanel 是面板的入口：读配置、起库、接线、监听、优雅退出。
//
// 这里刻意只做"装配"，不含任何业务：所有能测的东西都在 internal 里，
// main 里出问题的话，症状一定是"起不来"而不是"跑歪了"，这样排查路径最短。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"dstpanel/internal/api"
	"dstpanel/internal/backup"
	"dstpanel/internal/config"
	"dstpanel/internal/dst"
	"dstpanel/internal/hoststats"
	"dstpanel/internal/lobby"
	"dstpanel/internal/logx"
	"dstpanel/internal/modbridge"
	"dstpanel/internal/monitor"
	"dstpanel/internal/procrun"
	"dstpanel/internal/steam"
	"dstpanel/internal/store"
	"dstpanel/internal/worker"
)

const version = "0.1.0"

// drainBackground 给后台协程（采样 + 任务 worker）最多 15 秒收尾。
// 采样读的是本地文件，worker 的子进程在 ctx 取消时会被 exec 直接收掉，
// 所以正常情况下两秒内就该回来；等这么久是防磁盘挂住，那种时候用户要的是
// 面板立刻退出好重启，而不是停在这里。
func drainBackground(wg *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		slog.Warn("后台协程 15 秒没收尾，直接退出")
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		// 启动失败必须留下能照着做的中文结论：这套东西通常是在 ssh 里第一次跑的。
		// 走 slog 是因为失败也可能发生在 logx.Setup 之后，那样文件里也得有一条。
		slog.Error("面板启动失败", "err", err.Error())
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("dstpanel", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", "", "配置文件路径（YAML）；不传则用内置默认值")
	genConfig := fs.String("gen-config", "", "把带注释的示例配置写到该路径后退出")
	showVersion := fs.Bool("version", false, "打印版本号后退出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintf(out, "dstpanel %s\n", version)
		return nil
	}
	if *genConfig != "" {
		return writeSampleConfig(*genConfig, out)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// 日志目录要先建：logx 在 store 之前，因为建库失败本身也要有地方记。
	if err := os.MkdirAll(cfg.Paths.WorkDir, 0o755); err != nil {
		return fmt.Errorf("创建工作目录 %s: %w", cfg.Paths.WorkDir, err)
	}
	closeLog, err := logx.Setup(cfg.Paths.WorkDir)
	if err != nil {
		return err
	}
	defer closeLog()

	st, err := store.Open(cfg.Paths.DBFile())
	if err != nil {
		return err
	}
	defer st.Close()

	paths := dst.Paths{KleiRoot: cfg.Paths.KleiRoot, DstInstall: cfg.Paths.DstInstall}
	// 世界设定字典是内嵌进成品的数据：读不出来就是构建出了问题，
	// 没有"运行时能绕过它"的选项，所以这里直接失败退出，而不是让开服接口在半路 500。
	worldSchema, err := dst.LoadWorldSchema()
	if err != nil {
		return err
	}
	// 存档快照层：读 kleiRoot 下的房间目录，写 workDir/backups。
	// 目录在这里就建好——留到第一次备份才建的话，"目录建不出来"会变成一个 500
	// 而不是启动时一句中文结论。
	if err := os.MkdirAll(cfg.Paths.BackupDir(), 0o755); err != nil {
		return fmt.Errorf("创建备份目录 %s: %w", cfg.Paths.BackupDir(), err)
	}
	// 保留上限走回调而不是把数字抄一份：这一项在面板设置里能改且立刻生效，
	// 而 Manager 是接口层和 worker 共用的同一个对象，直接写字段会和正在跑的清理抢同一个值。
	backupMgr := &backup.Manager{
		Paths:   paths,
		Dir:     cfg.Paths.BackupDir(),
		Keep:    cfg.Backup.Keep,
		Version: version,
		Logger:  slog.Default(),
		KeepFunc: func() int {
			n, err := st.IntWithDefault(store.KeyBackupKeep, cfg.Backup.Keep)
			if err != nil {
				// 读不出可信值时既不变成"不清理"也不变成"只留一份"：按配置文件那份执行并喊出来。
				slog.Warn("存档保留上限读不出可信值，按配置文件里的数字执行",
					"err", err.Error(), "keep", cfg.Backup.Keep)
				return cfg.Backup.Keep
			}
			return n
		},
	}
	// 上一次运行留下的导入残骸在这里收掉，不留到第一次导入时顺手收：
	// 解压树被 kill 在中间就是几十 MB 到几 GB 的目录，而它既不算快照也不进列表，
	// 用户从界面上永远看不到它——只在盘满的那天才知道它存在。
	if n := backupMgr.PruneUnpacked(); n > 0 {
		slog.Info("清掉了上次没删完的解压目录", "count", n)
	}
	if gone := backupMgr.SweepStaged(backup.StagedUploadTTL); len(gone) > 0 {
		slog.Info("清掉了过期的上传临时包", "count", len(gone))
	}
	// 公网房间列表是面板唯一一个主动出去读别人数据的功能，所以拉取间隔和超时都从配置里来。
	// 缓存必须是有状态的单例：它存在的意义就是"同一个大区在 refreshSeconds 内只问一次 Klei"。
	// 第三个参数（失败后再撞网络的间隔）传 0 走 lobby 包里的默认 15 秒——
	// 那一档是"出不去时别让每次轮询都先卡满一次超时"，和带宽无关，不值得占一个配置项。
	lobbyCache := lobby.NewCache(
		lobby.NewFetcher(cfg.Lobby.Proxy, time.Duration(cfg.Lobby.TimeoutSeconds)*time.Second),
		time.Duration(cfg.Lobby.RefreshSeconds)*time.Second, 0, cfg.Lobby.Enabled)
	// 进程驱动单独留个变量：监控自愈和 HTTP 层要共用同一个实例
	// （per-shard 操作锁与启动阶段状态都挂在它上面，各建一份就各说各话）。
	procs := procrun.New(paths, procrun.ExecRunner{Timeout: 10 * time.Second})
	// 监控/自愈参数落在 workDir，运行时可由 /api/v1/monitor/config 改且立刻生效。
	monitorOpts := monitor.LoadOptions(cfg.Paths.WorkDir)
	supervisor := monitor.NewSupervisor(cfg.Paths.WorkDir, paths, st, procs, monitorOpts, slog.Default())
	procs.ShutdownWait = monitorOpts.ShutdownWait()
	srv := &api.Server{
		Paths:    paths,
		Procs:    procs,
		Bridge:   modbridge.New(paths, cfg.Helper),
		Store:    st,
		Auth:     cfg.Server,
		SteamCmd: cfg.Paths.SteamCmd,
		Steam:    steam.New(cfg.SteamAPI.WebAPIKey, cfg.SteamAPI.Proxy),
		Network:  cfg.Network,
		World:    worldSchema,
		Backups:  backupMgr,
		Lobby:    lobbyCache,
		Monitor:  supervisor,
		// 运行状态采集器盯着存档所在的那个分区：这台机器的内存和磁盘本来就是
		// "能不能再起一个世界"的判断依据，看别的分区没有意义。
		Host: hoststats.NewCollector(paths.KleiRoot),
		// 面板设置里这几项的"没改过时用什么"全部来自这份配置：
		// 登记在 store 里的常数只是文档值，用户写在 yaml 里的数字才是缺省值。
		SettingDefaults: map[string]int{
			store.KeyBackupKeep:          cfg.Backup.Keep,
			store.KeyBackupScheduleHours: cfg.Backup.ScheduleHours,
		},
	}

	// 上一轮进程留下的"执行中"任务在这里回收，不回收它们会永远显示执行中。
	if n, err := st.RecoverInterruptedTasks(); err != nil {
		// 回收失败不该拦住启动：库是好的，只是有几行状态可疑，记下来让人去任务中心看。
		slog.Warn("回收中断任务失败", "err", err.Error())
	} else if n > 0 {
		slog.Warn("上次运行有任务被中断，已标记为失败", "count", n)
	}

	if err := checkBind(cfg); err != nil {
		return err
	}
	clusters, _ := paths.ClusterNames()
	slog.Info("面板启动",
		"version", version,
		"config", orDefault(*cfgPath, "<内置默认值>"),
		"listen", fmt.Sprintf("%s:%d", cfg.Server.Bind, cfg.Server.Port),
		"kleiRoot", paths.KleiRoot,
		"dstInstall", paths.DstInstall,
		"clusters", len(clusters))
	if w := kleiRootWarning(paths.KleiRoot); w != "" {
		slog.Warn(w, "kleiRoot", paths.KleiRoot)
	}

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Server.Bind, cfg.Server.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 故意不设 WriteTimeout：日志实时推送走 SSE，是一条可以挂几小时的响应。
		// 真要限流应该在具体的长连接接口里自己带 context 超时，而不是在服务器层一刀切。
	}

	// 先单独 Listen：Serve 的报错混着"起起来又死了"，单独这一步能把"端口被占"讲清楚。
	ln, err := net.Listen("tcp", httpSrv.Addr)
	if err != nil {
		return bindError(err, httpSrv.Addr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sampler := &monitor.Sampler{
		Paths:  paths,
		Bridge: srv.Bridge,
		Store:  st,
		// 玩家行为归档：总览那张「玩家在线时间图」的历史从这里来。
		// 挂在采样循环里而不是另开一个 goroutine，理由见 monitor/events.go 开头。
		Events: &monitor.Archiver{Paths: paths, Store: st},
		// 整机指标采样与告警/自愈：和 HTTP 层共用同一个 Supervisor，
		// 采样落盘、阈值判断、崩溃心跳都挂在原来的采样循环上，不另起一套节拍。
		Host:       srv.Host,
		Supervisor: supervisor,
		// 采样间隔和 MOD 的心跳间隔是两回事：MOD 每秒写一次 status.json，
		// 这里一分钟读一次入库，否则一个月就是两百多万行趋势数据。
		Interval:     time.Duration(cfg.Monitor.SampleSeconds) * time.Second,
		CleanupEvery: time.Duration(cfg.Monitor.CleanupHours) * time.Hour,
	}
	var wg sync.WaitGroup
	// 随面板恢复：开关打开时把上次退出前仍在跑的分片拉起来。放后台跑，
	// 不挡住下面监听端口；关掉开关时 Recover 立刻返回，不会重启任何分片。
	wg.Add(1)
	go func() {
		defer wg.Done()
		supervisor.Recover(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		sampler.Run(ctx)
	}()
	// 异步任务 worker：SteamCMD 更新这类十几分钟的活走它，HTTP 请求只负责入队。
	// 收到退出信号时 ctx 会传到 exec.CommandContext，子进程被一起收掉，
	// 那条任务由下次启动的 RecoverInterruptedTasks 标成失败并提示可以重提。
	wk := &worker.Worker{
		Store:    st,
		Paths:    paths,
		Lines:    procrun.ExecRunner{},
		Procs:    srv.Procs,
		Backups:  backupMgr,
		SteamCmd: cfg.Paths.SteamCmd,
		// 两条判停线都来自配置，填 0 走 worker 里的默认：
		// 硬上限防僵尸（本体那一段按类型放宽到 12 小时），空闲判停靠"输出还在不在动"。
		// 为什么不写死：整包大小和带宽都是机器的属性，同一台机器上实测过 33 KB/s 和 29 MB/s。
		LongTimeout: time.Duration(cfg.Tasks.TimeoutMinutes) * time.Minute,
		Timeout:     time.Duration(cfg.Tasks.TimeoutMinutes) * time.Minute,
		Stall:       time.Duration(cfg.Tasks.StallMinutes) * time.Minute,
		Logger:      slog.Default(),
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		wk.Run(ctx)
	}()
	// 定时备份只负责入队，跑的是上面那个 worker，所以定时和手工天然串行。
	scheduler := &worker.BackupScheduler{
		Store:  st,
		Paths:  paths,
		Logger: slog.Default(),
		Hours: func() int {
			n, err := st.IntWithDefault(store.KeyBackupScheduleHours, cfg.Backup.ScheduleHours)
			if err != nil {
				slog.Warn("定时备份间隔读不出可信值，按配置文件里的数字执行",
					"err", err.Error(), "hours", cfg.Backup.ScheduleHours)
				return cfg.Backup.ScheduleHours
			}
			return n
		},
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		scheduler.Run(ctx)
	}()
	defer drainBackground(&wg)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("监听 %s 时出错: %w", httpSrv.Addr, err)
		}
		return nil
	case <-ctx.Done():
		slog.Info("收到退出信号，正在关闭（不再接新请求，进行中的请求最多等 10 秒）")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("优雅关闭失败: %w", err)
		}
		slog.Info("面板已退出")
		return nil
	}
}

// kleiRootWarning 在存档根目录不像引擎会读的那个时给一句话提示。
//
// 为什么要单喊：实测服务端二进制（760944）忽略 -savedir，永远读写运行用户自己的
// ~/.klei/DoNotStarveTogether。指错时面板这边一切"成功"——它改的是另一个目录里的配置，
// 开服时引擎用的是它自己那个世界，错误完全不外露。
func kleiRootWarning(kleiRoot string) string {
	if kleiRoot == "" {
		return ""
	}
	p := filepath.Clean(filepath.ToSlash(kleiRoot))
	// 分隔符手动归一：filepath.ToSlash 在非 Windows 上不认反斜杠，
	// 而这段判断两种系统都要能用（测试也要能在 Linux 上验 Windows 形态的路径）。
	p = strings.ReplaceAll(p, `\`, "/")
	if strings.HasSuffix(p, "/.klei/DoNotStarveTogether") || strings.HasSuffix(p, "/Klei/DoNotStarveTogether") {
		return ""
	}
	return "kleiRoot 不像服务端会读的存档根目录：引擎忽略 -savedir，只认运行用户的 ~/.klei/DoNotStarveTogether" +
		"（Windows 是 文档/Klei/DoNotStarveTogether）；现在这个目录里的集群配置不会影响开服"
}

// checkBind 挡住"监听在所有网卡上却没设密码"这一种组合。
// 只绑回环时可以留空密码（自己用 SSH 隧道或本机浏览器看），那时接口层会返回 503 提示去设。
func checkBind(cfg *config.Config) error {
	if cfg.Server.AdminPassword != "" {
		return nil
	}
	loopback := cfg.Server.Bind == "" || cfg.Server.Bind == "127.0.0.1" || cfg.Server.Bind == "localhost" || cfg.Server.Bind == "::1"
	if loopback {
		return nil
	}
	return fmt.Errorf("server.bind 是 %s（不是回环地址）却没设 server.adminPassword，面板拒绝启动；先填密码，或把 bind 改回 127.0.0.1", cfg.Server.Bind)
}

// bindError 把内核错误码翻译成人能执行的下一步。
func bindError(err error, addr string) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("端口 %s 已被占用：先确认是不是已经有一个面板在跑（systemctl status dstpanel 或 ss -lntp | grep %s）", addr, portOf(addr))
	}
	if errors.Is(err, syscall.EACCES) {
		return fmt.Errorf("没有权限监听 %s：1024 以下的端口需要 root 或给二进制 CAP_NET_BIND_SERVICE", addr)
	}
	return fmt.Errorf("监听 %s 失败: %w", addr, err)
}

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return port
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// sampleConfig 是带注释的样例配置模板。
// 目标不是"能跑就行"：每个键都在解释它管的是哪份 Klei 文件，
// 因为面板改错一个路径的后果是"世界没了"，必须让填的人看懂在填什么。
//
// paths 段用占位符而不是写死路径：这三个值同时存在于 config.Default()、部署脚本和
// 服务器上已经跑着的实盘目录里，抄一份到模板里就等于第四个版本，迟早对不上。
// 实际文本由 sampleConfigText() 从 Default() 渲染。
const sampleConfigTemplate = `# DST 面板配置。路径都写绝对路径，~ 不会被展开。
server:
  # 默认只听本机。要局域网或公网访问就改成 0.0.0.0。
  # 下面是首次启动的默认账号（admin/admin，刻意用最广为人知的一组，
  # 因为它会出现在文档和笔记里；面板会一直提示"还在用默认密码"，登录后请尽快改）。
  bind: 127.0.0.1
  port: 8921
  adminUser: admin
  adminPassword: "admin"

paths:
  # 这一段的默认值全部展开在"面板运行用户的家目录"下面，这是刻意的布局：
  # deploy/start.sh 会建一个专用用户并把它的 home 设成业务根目录，
  # 于是引擎存档、steamcmd 缓存、面板数据库和备份都长在同一棵树上，删掉这个用户就删干净。
  # 集群配置与存档的根目录。服务端二进制不认 -savedir（实测 760944 版：给了照样忽略），
  # 只读运行用户自己的 ~/.klei/DoNotStarveTogether（Windows 是 文档/Klei/DoNotStarveTogether），
  # 所以这一行必须指到那个位置。指到别处的后果是静默的：面板把配置写好了，
  # 开服时引擎却用的是另一个目录里它自己生成的世界。
  kleiRoot: {{kleiRoot}}
  # SteamCMD 装 app 343050 的目录，内含 bin64/ 与 mods/。
  dstInstall: {{dstInstall}}
  steamCmd: {{steamCmd}}
  # 面板自己的东西：数据库、备份、MOD 暂存。
  workDir: {{workDir}}

network:
  # 分片端口分配区间，面板会跳过已被别的集群占用的端口。
  shardPortBase: 10998
  shardPortEnd: 11900
  masterIntentPortBase: 11000

backup:
  # 保留几份存档备份（含恢复前自动打的那份安全备份），超出就删最旧的。
  # 占磁盘的是归档文件，实测这台 137.9MB 的存档目录打成 19.0MB 的包；往上调之前先看一眼 df -h。
  # 这里填 0 或负数表示不自动清理。面板设置里改这一项会立刻生效，不用重启。
  keep: 20
  # 定时备份的间隔小时数，0 表示不开（默认不开：没问过用户就定期往盘上写几十 MB
  # 不该是开箱行为）。开面板后要先等满一个间隔才打第一份，避免每次发版重启都立刻多一份。
  scheduleHours: 0

steamApi:
  # 在线检索创意工坊用（https://steamcommunity.com/dev/apikey 申请，只要只读权限）。
  # 留空照样能装模组：把工坊 ID 填进登记框就行，少的是"按名字找 ID"。
  webApiKey: ""
  # 只在在线检索时生效，形如 http://127.0.0.1:7890；留空跟随 HTTPS_PROXY。
  # 服务端自己下载模组走的是另一条路，不归这个代理管。
  proxy: ""

helper:
  # 助手 MOD 拉命令队列的间隔（毫秒），以及多久没心跳算掉线。
  pollMs: 1000
  staleSeconds: 15

monitor:
  # 在线趋势入库的间隔（秒），和保留期清理跑多久一次（小时）。
  # 采样间隔别学 MOD 心跳设成 1 秒：跑一个月就是两百多万行，图也画不动。
  sampleSeconds: 60
  cleanupHours: 6
  # 监控告警、自愈开关、停服等待上限这些项不写在 yaml 里：它们要能在面板运行时改、
  # 改完立刻生效，统一走 GET/PUT /api/v1/monitor/config（GET /api/v1/monitor 看采样与告警）。
  # 随面板恢复/崩溃自动拉起默认关闭；CPU/内存/磁盘阈值默认 90%；
  # 助手 MOD 超过 300 秒无快照报警；同类告警冷却 10 分钟；停服等进程退出默认 30 秒；
  # Webhook 留空则不发。配置持久化在 workDir/monitor_options.json，重启后读回。

tasks:
  # 单条任务的硬上限（分钟）。除了防僵尸，它还兜住"失败之后从头重下、输出一直在变"那一种，
  # 下面那项判停认不出它。0 = 按类型给默认：本体下载 720（12 小时），备份与恢复 60。
  # 本体这一档为什么这么宽：整包实测 4.28 GB，带宽每台机器都不一样——
  # 同一台云机在一次任务里量过 33 KB/s（steamcmd 给自己更新那一段）和 29 MB/s（本体下来那一段），
  # 按"典型带宽"写死的数字一定会在某一台机器上把正常下载判成失败。
  timeoutMinutes: 0
  # 判停靠这一项：连续这么多分钟读不到任何新内容（同一行重绘不算新内容）就中断，并说清是卡死。
  # 0 = 默认 5 分钟。没有"关掉"这一档，关掉只剩上面那道 12 小时的闸。
  stallMinutes: 5

lobby:
  # "公网上的房间"这一页读的是 Klei 放在 CDN 上的公开快照：不要账号、不带凭据、不写盘。
  # 关掉它只是不读这一页，本地房间的一切功能不受影响。
  enabled: true
  # 同一个大区多久允许真实拉取一次（秒）。一份列表解压后有十几 MB，
  # 浏览器开着这一页时会几秒一轮地问，没有这一档就是拿带宽换"看起来更实时"。
  refreshSeconds: 60
  # 单次拉取的超时（秒），上限 120。列表大，走代理的机器上十几秒是正常的。
  timeoutSeconds: 20
  # 只在读公网列表时生效，形如 http://127.0.0.1:7890；留空跟随 HTTPS_PROXY。
  # 和上面 steamApi.proxy 分开填：两者的出口不一定同一个，Klei 这个 CDN 常常直连就通。
  proxy: ""
`

func sampleConfigText() string {
	paths := config.Default().Paths
	r := strings.NewReplacer(
		"{{kleiRoot}}", paths.KleiRoot,
		"{{dstInstall}}", paths.DstInstall,
		"{{steamCmd}}", paths.SteamCmd,
		"{{workDir}}", paths.WorkDir,
	)
	return r.Replace(sampleConfigTemplate)
}

func writeSampleConfig(path string, out io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s 已经存在，面板没有覆盖它（配置写坏比没配置更麻烦）", path)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(sampleConfigText()), 0o600); err != nil {
		return fmt.Errorf("写示例配置 %s: %w", path, err)
	}
	fmt.Fprintf(out, "示例配置已写到 %s（权限 0600，里面有 adminPassword 的位置），改完后用 -config 指向它启动\n", path)
	return nil
}
