package procrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/dst"
)

type call struct {
	name string
	args []string
}

// fakeRunner 模拟 screen 的行为：维护会话表，按 argv 语义应答。
// 目的不是测 screen，而是测"面板到底让 screen 干了什么"。
type fakeRunner struct {
	calls    []call
	sessions map[string]int
	// failStuffFor 让指定会话的 stuff 返回 "No such session"，模拟竞态。
	failStuffFor string
	// ignoreStart 接受 -dmS 但不登记会话，模拟进程起来就退出。
	ignoreStart bool
	// afterStart 在 -dmS 之后调用，用来模拟"服务端把日志写出来了"：
	// 启动判定的用例必须让日志的时间戳晚于发起启动的时刻，否则测的是上一轮的旧日志。
	afterStart func()
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{sessions: map[string]int{}}
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "-ls":
		var b strings.Builder
		for sess, pid := range f.sessions {
			fmt.Fprintf(&b, "\t%d.%s\t(Detached)\n", pid, sess)
		}
		if len(f.sessions) == 0 {
			return "No Sockets found.\n", nil
		}
		return b.String(), nil
	case "-dmS":
		if !f.ignoreStart {
			f.sessions[args[1]] = 40000 + len(f.sessions)
		}
		if f.afterStart != nil {
			f.afterStart()
		}
		return "", nil
	case "-S":
		sess := args[1]
		rest := strings.Join(args[2:], " ")
		switch {
		case strings.HasPrefix(rest, "-X quit"):
			if _, ok := f.sessions[sess]; !ok {
				return fmt.Sprintf("No screen session found.\tNo such session %s\n", sess), nil
			}
			delete(f.sessions, sess)
			return "", nil
		case strings.Contains(rest, "stuff"):
			if _, ok := f.sessions[sess]; !ok || sess == f.failStuffFor {
				return fmt.Sprintf("No such session %s\n", sess), nil
			}
			return "", nil
		}
	}
	return "", nil
}

// stuffLines 按顺序返回注入到会话控制台的内容，含结尾换行。
// 面板会先查 -ls 再发命令，所以不能按固定下标取调用。
func (f *fakeRunner) stuffLines() []string {
	var out []string
	for _, c := range f.calls {
		if len(c.args) == 5 && c.args[0] == "-S" && c.args[3] == "stuff" {
			out = append(out, c.args[4])
		}
	}
	return out
}

func (f *fakeRunner) callByFirstArg(t *testing.T, arg0 string) call {
	t.Helper()
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == arg0 {
			return c
		}
	}
	t.Fatalf("没有以 %q 开头的调用: %+v", arg0, f.calls)
	return call{}
}

func preparedPaths(t *testing.T, shards ...string) dst.Paths {
	t.Helper()
	p := dst.Paths{
		KleiRoot:   filepath.Join(t.TempDir(), "klei"),
		DstInstall: filepath.Join(t.TempDir(), "dst"),
	}
	if err := os.MkdirAll(filepath.Dir(p.ServerBinary()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ServerBinary(), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cluster := range []string{"Cluster_1"} {
		if err := os.MkdirAll(p.ClusterDir(cluster), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.ClusterIni(cluster), []byte("[SHARD]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.TokenFile(cluster), []byte("abcdefghijklmnopqrstuvwx\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, sh := range shards {
			if err := os.MkdirAll(p.ShardDir(cluster, sh), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ShardIni(cluster, sh), []byte("[NETWORK]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return p
}

func newDriver(p dst.Paths, f *fakeRunner) *Driver {
	d := New(p, f)
	d.StartWait = 200 * time.Millisecond
	d.BootLogWait = 300 * time.Millisecond
	return d
}

// writeBootLog 造一份"这次启动"的日志：mtime 必须由 afterStart 在那之后写，
// 否则驱动会当成上一轮留下的旧文件而不作数。
func writeBootLog(t *testing.T, p dst.Paths, cluster, shard, text string) func() {
	t.Helper()
	return func() {
		path := p.ServerLog(cluster, shard)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 实测过的现场：无效令牌时 screen 会话一直存在，服务端却在日志里说不会启动。
// 只查会话的启动判定会把这种情况报成"启动成功"，界面上就是一个假活的绿色。
func TestStartFailsWhenServerRefusesToBoot(t *testing.T) {
	p := preparedPaths(t, dst.ShardMaster)
	f := newFakeRunner()
	f.afterStart = writeBootLog(t, p, "Cluster_1", dst.ShardMaster,
		"[00:00:00]: PersistRootStorage is now [[DoNotStarveTogether/Cluster_1/Master/]]\n"+
			"[00:00:01]: [200] Account Failed (6): \"E_INVALID_TOKEN\"\n"+
			"[00:00:01]: !!!! Your Server Will Not Start !!!!\n"+
			"[00:00:01]: No auth token could be found.\n")
	d := newDriver(p, f)

	err := d.Start(context.Background(), "Cluster_1", dst.ShardMaster)
	if !errors.Is(err, ErrStartRefused) {
		t.Fatalf("会话在跑但服务端拒绝启动，必须报错，实际 %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"Your Server Will Not Start", "令牌"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误里要给出指纹和可动手的结论，缺 %q: %s", want, msg)
		}
	}
	// 世界设定那类 assert 也算"这一轮启动废了"，不能只认令牌一种。
	f2 := newFakeRunner()
	f2.afterStart = writeBootLog(t, p, "Cluster_1", dst.ShardMaster,
		"[00:00:00]: PersistRootStorage is now [[x]]\n"+
			"[00:00:04]: STACKTRACE - Lua Error: Level data override is invalid!\n")
	d2 := newDriver(p, f2)
	err = d2.Start(context.Background(), "Cluster_1", dst.ShardMaster)
	if !errors.Is(err, ErrStartRefused) || !strings.Contains(err.Error(), "leveldataoverride") {
		t.Errorf("世界文件不合法也要报出来并指向文件名，实际 %v", err)
	}

	// 云机首启的现场第三种：世界生成失败，进程和会话都还在（一挂就是几十分钟），
	// 只有日志说明"没人进得来"。这一条不认，状态页就是假活的绿色。
	f3 := newFakeRunner()
	f3.afterStart = writeBootLog(t, p, "Cluster_1", dst.ShardMaster,
		"[00:00:07]: DoLuaFile Error: #[string \"scripts/map/storygen.lua\"]:865: Must specify a layout mode for your level.\n"+
			"[00:00:07]: Error loading worldgen_main.lua\n"+
			"[00:00:07]: WorldSim::SimThread::Main() ERROR\n"+
			"[00:00:07]: Worldgen had an error, displaying...\t\n")
	d3 := newDriver(p, f3)
	err = d3.Start(context.Background(), "Cluster_1", dst.ShardMaster)
	if !errors.Is(err, ErrStartRefused) {
		t.Fatalf("世界生成失败也要判成本轮启动废了，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "没能进入存档") {
		t.Errorf("结论要说清\"进程在但没进世界\"，实际 %s", err)
	}
}

// 日志是上一次启动留下的（时间戳早于这次发起）时不能拿它判这次的死：
// 引擎虽然会截断重写，但"截断"发生在进程真正跑起来之后。
func TestStartIgnoresStaleBootLog(t *testing.T) {
	p := preparedPaths(t, dst.ShardMaster)
	path := p.ServerLog("Cluster_1", dst.ShardMaster)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[00:00:01]: !!!! Your Server Will Not Start !!!!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	f := newFakeRunner()
	d := newDriver(p, f)
	if err := d.Start(context.Background(), "Cluster_1", dst.ShardMaster); err != nil {
		t.Errorf("旧日志里的失败行不该判死这次启动: %v", err)
	}
}

// 云机 2026-10-31 的现场：世界生成每 4~5 秒撞一次必需物件缺失、推倒重来，
// 进程和 screen 会话一直活着，助手 MOD 永远没机会回话。这时候总览页只写
// "助手 MOD 还没写入过状态"是不够的——引擎早就把原因打在日志里了，
// 不把它带出来，用户只能自己去翻 server_log.txt。
func TestBootFailureNoteQuotesEngineDuringRetryLoop(t *testing.T) {
	p := preparedPaths(t, dst.ShardMaster)
	path := p.ServerLog("Cluster_1", dst.ShardMaster)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var looping strings.Builder
	looping.WriteString("[00:00:00]: PersistRootStorage is now [[x]]\n")
	for i := 0; i < 3; i++ {
		looping.WriteString("[00:00:15]: Checking Required Prefab sculpture_bishop has at least 1 instances (0 found).\t\n")
		looping.WriteString("[00:00:15]: PANIC: missing required prefab [sculpture_bishop]! Expected 1, got 0\n")
		looping.WriteString("[00:00:15]: An error occured during world gen we will retry!\t\n")
	}
	if err := os.WriteFile(path, []byte(looping.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newDriver(p, newFakeRunner())
	note, ok := d.BootFailureNote("Cluster_1", dst.ShardMaster)
	if !ok {
		t.Fatal("生成还在重试循环里时必须给得出结论")
	}
	for _, want := range []string{"PANIC: missing required prefab", "必需物件"} {
		if !strings.Contains(note, want) {
			t.Errorf("结论要引用引擎原句并说清后果，缺 %q: %s", want, note)
		}
	}

	// 几小时前那次失败、后来又跑了几百行正常日志的场合不能算"现在坏了"：
	// 这条用例钉的就是"只回看尾巴若干行"这个设计。
	var recovered strings.Builder
	recovered.WriteString("[00:00:15]: PANIC: missing required prefab [sculpture_bishop]! Expected 1, got 0\n")
	for i := 0; i < bootFailureScanLines; i++ {
		recovered.WriteString("[00:20:00]: Network tick rate: U=15(2), D=0\n")
	}
	if err := os.WriteFile(path, []byte(recovered.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if note, ok := d.BootFailureNote("Cluster_1", dst.ShardMaster); ok {
		t.Errorf("指纹被后来的正常日志挤出回看窗口后不该再报，实际 %s", note)
	}

	// 还没有日志文件（刚建好集群）不能崩，也不能编一句结论。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.BootFailureNote("Cluster_1", dst.ShardMaster); ok {
		t.Error("读不到日志就不该给出结论")
	}
}

// 无人暂停时引擎冻住整个 Lua 调度器，助手必然没心跳。这时候报"助手离线"是误导：
// 用户会去重启一个本来好好的服务器。所以要从日志里把"此刻是暂停还是恢复"读出来。
func TestSimPausedNoteReadsLastTransition(t *testing.T) {
	p := preparedPaths(t, dst.ShardMaster)
	path := p.ServerLog("Cluster_1", dst.ShardMaster)
	if err := os.MkdirAll(filepath.Dir(path), 0o754); err != nil {
		t.Fatal(err)
	}
	d := newDriver(p, newFakeRunner())

	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 云机真实形态（2026-11-03 从 Master/server_log.txt 里取的）：词是 Server Autopaused，
	// 行尾还带一个制表符。这一种必须说成"没人在玩、引擎自己冻的"。
	write("[00:51:36]: Server Unpaused\t\n[00:51:46]: Server Autopaused\t\n")
	note, ok := d.SimPausedNote("Cluster_1", dst.ShardMaster)
	if !ok {
		t.Fatal("日志最后一句是 Server Autopaused，必须报出暂停状态而不是让界面写助手离线")
	}
	for _, want := range []string{"Server Autopaused", "没有人在玩", "冻结", "无人时暂停"} {
		if !strings.Contains(note, want) {
			t.Errorf("自动暂停的结论里缺 %q，原文: %s", want, note)
		}
	}

	// 手动暂停是另一种语义：这时候说"没人"就与界面上摆着的在线数当面矛盾。
	write("[00:00:44]: Server registered via geo DNS in ap-southeast-1\n" +
		"[00:00:44]: Sim paused\n")
	note, ok = d.SimPausedNote("Cluster_1", dst.ShardMaster)
	if !ok {
		t.Fatal("日志最后一句是 Sim paused，必须报出暂停状态")
	}
	for _, want := range []string{"Sim paused", "冻结", "不是掉线"} {
		if !strings.Contains(note, want) {
			t.Errorf("手动暂停的结论里缺 %q，原文: %s", want, note)
		}
	}
	if strings.Contains(note, "没有人在玩") {
		t.Errorf("Sim paused 不是引擎的自动暂停，结论里不该断言没人在玩：%s", note)
	}

	// 有人进来就恢复了：取最后一次方向，不能因为历史里有 Sim paused 就一直报暂停。
	write("[00:00:44]: Sim paused\n" +
		"[00:12:01]: Sim unpaused\n" +
		"[00:12:02]: Network tick rate: U=15(2), D=0\n")
	if _, ok := d.SimPausedNote("Cluster_1", dst.ShardMaster); ok {
		t.Error("恢复之后不该再报暂停")
	}

	// 自动暂停后又有人进来：引擎打的是 Server Unpaused，方向要按最后一次算。
	write("[00:51:46]: Server Autopaused\t\n[00:52:30]: Server Unpaused\t\n")
	if _, ok := d.SimPausedNote("Cluster_1", dst.ShardMaster); ok {
		t.Error("Autopaused 之后又有 Server Unpaused，不该再报暂停")
	}

	// 从没暂停过的干净日志。
	write("[00:00:44]: Sim unpaused\n[00:00:45]: Network tick rate: U=15(2), D=0\n")
	if _, ok := d.SimPausedNote("Cluster_1", dst.ShardMaster); ok {
		t.Error("没有暂停记录时不该编出暂停状态")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.SimPausedNote("Cluster_1", dst.ShardMaster); ok {
		t.Error("读不到日志就不该给出结论")
	}
}

// 一份干净的日志（没有指纹）不该被误判。
// 另外几条老用例走的是"日志文件根本不存在"这条路（preparedPaths 从不造日志），
// Start 依旧返回 nil：读不到证据时宁可漏判，也不能把每次正常启动都判成失败，
// 漏判的那一面由状态页的"助手没回话"补上。
func TestStartWithHealthyBootLog(t *testing.T) {
	p := preparedPaths(t, dst.ShardMaster)
	f := newFakeRunner()
	f.afterStart = writeBootLog(t, p, "Cluster_1", dst.ShardMaster,
		"[00:00:00]: PersistRootStorage is now [[DoNotStarveTogether/Cluster_1/Master/]]\n"+
			"[00:00:02]: About to start a server with the following options:\n"+
			"[00:00:03]: Starting server on 127.0.0.1:10999 (Cluster_1 Master 0 1 0 0)\n")
	d := newDriver(p, f)
	if err := d.Start(context.Background(), "Cluster_1", dst.ShardMaster); err != nil {
		t.Errorf("健康的日志被当成启动失败: %v", err)
	}
}

func TestStartBuildsExpectedScreenCommand(t *testing.T) {
	p := preparedPaths(t, dst.ShardMaster, dst.ShardCaves)
	f := newFakeRunner()
	d := newDriver(p, f)

	if err := d.Start(context.Background(), "Cluster_1", dst.ShardMaster); err != nil {
		t.Fatalf("Start: %v", err)
	}
	c := f.callByFirstArg(t, "-dmS")
	if c.name != "screen" {
		t.Errorf("应该调用 screen，实际 %q", c.name)
	}
	want := []string{"-dmS", "dst-Cluster_1-Master", "bash", "-c"}
	if strings.Join(c.args[:len(want)], " ") != strings.Join(want, " ") {
		t.Errorf("screen 前缀参数不对: %v", c.args)
	}
	inner := c.args[len(want)]
	if !strings.Contains(inner, "-console -cluster 'Cluster_1' -shard 'Master'") {
		t.Errorf("缺少 Klei 启动参数: %s", inner)
	}
	if !strings.Contains(inner, "cd '"+filepath.Dir(p.ServerBinary())+"'") {
		t.Errorf("没有先 cd 到 bin64: %s", inner)
	}
	if !strings.Contains(inner, "exec '"+p.ServerBinary()+"'") {
		t.Errorf("没用 exec 接管进程（会多一层 shell 进程，kill 不干净）: %s", inner)
	}
	// Klei 自己的启动脚本就导这两个变量，少了它们工坊与 Steam 相关功能可能整体关掉。
	if !strings.Contains(inner, "export SteamAppId="+dst.GameAppID+" SteamGameId="+dst.GameAppID+" &&") {
		t.Errorf("没有导 SteamAppId/SteamGameId: %s", inner)
	}

	running, err := d.IsRunning(context.Background(), "Cluster_1", dst.ShardMaster)
	if err != nil || !running {
		t.Errorf("启动后应判定在跑: %v %v", running, err)
	}
	if r, _ := d.IsRunning(context.Background(), "Cluster_1", dst.ShardCaves); r {
		t.Error("洞穴没启动却判定在跑")
	}
	pid, _ := d.Pid(context.Background(), "Cluster_1", dst.ShardMaster)
	if pid <= 0 {
		t.Error("拿不到 PID")
	}

	// 重复启动必须被拦住：同端口双开会让第二个进程静默退出。
	if err := d.Start(context.Background(), "Cluster_1", dst.ShardMaster); err == nil ||
		!strings.Contains(err.Error(), "已在运行") {
		t.Errorf("重复启动应报错: %v", err)
	}
}

func TestStartFailsWithActionablePrereqMessages(t *testing.T) {
	ctx := context.Background()

	t.Run("缺令牌", func(t *testing.T) {
		p := preparedPaths(t, dst.ShardMaster)
		if err := os.Remove(p.TokenFile("Cluster_1")); err != nil {
			t.Fatal(err)
		}
		f := newFakeRunner()
		d := newDriver(p, f)
		err := d.Start(ctx, "Cluster_1", dst.ShardMaster)
		if err == nil || !strings.Contains(err.Error(), "cluster_token.txt") {
			t.Errorf("应提示缺令牌: %v", err)
		}
		if len(f.calls) != 0 {
			t.Error("前置检查没过就不该碰 screen")
		}
	})

	t.Run("令牌为空", func(t *testing.T) {
		p := preparedPaths(t, dst.ShardMaster)
		if err := os.WriteFile(p.TokenFile("Cluster_1"), []byte("   \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		d := newDriver(p, newFakeRunner())
		if err := d.Start(ctx, "Cluster_1", dst.ShardMaster); err == nil ||
			!strings.Contains(err.Error(), "空的") {
			t.Errorf("应提示令牌为空: %v", err)
		}
	})

	t.Run("没有二进制", func(t *testing.T) {
		p := preparedPaths(t, dst.ShardMaster)
		if err := os.Remove(p.ServerBinary()); err != nil {
			t.Fatal(err)
		}
		d := newDriver(p, newFakeRunner())
		if err := d.Start(ctx, "Cluster_1", dst.ShardMaster); err == nil ||
			!strings.Contains(err.Error(), "服务端二进制") {
			t.Errorf("应提示缺二进制: %v", err)
		}
	})

	t.Run("会话留不住时带回日志", func(t *testing.T) {
		p := preparedPaths(t, dst.ShardMaster)
		if err := os.WriteFile(p.ServerLog("Cluster_1", dst.ShardMaster),
			[]byte("[00:00:01] failed to load some mod\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// 假 runner 接受 -dmS 但不登记会话，模拟进程立刻退出。
		f := &fakeRunner{sessions: map[string]int{}, ignoreStart: true}
		d := newDriver(p, f)
		err := d.Start(ctx, "Cluster_1", dst.ShardMaster)
		if err == nil || !strings.Contains(err.Error(), "failed to load some mod") {
			t.Errorf("应把 server_log.txt 末尾带回错误里: %v", err)
		}
	})
}

func TestStopUsesShutdownSaveArgument(t *testing.T) {
	ctx := context.Background()
	p := preparedPaths(t, dst.ShardMaster)
	f := newFakeRunner()
	d := newDriver(p, f)
	if err := d.Start(ctx, "Cluster_1", dst.ShardMaster); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(ctx, "Cluster_1", dst.ShardMaster, true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	lines := f.stuffLines()
	// c_shutdown 的布尔参数就是"要不要先落盘"，Klei 自己实现了顺序，
	// 面板不再多发一条保存命令、也不再靠 sleep 猜时间。
	if len(lines) != 1 || lines[0] != "c_shutdown()\n" {
		t.Errorf("正常停服只该发一条 c_shutdown(): %+v", lines)
	}

	// 不保存直接关
	f2 := newFakeRunner()
	d2 := newDriver(p, f2)
	if err := d2.Start(ctx, "Cluster_1", dst.ShardMaster); err != nil {
		t.Fatal(err)
	}
	if err := d2.Stop(ctx, "Cluster_1", dst.ShardMaster, false); err != nil {
		t.Fatal(err)
	}
	got := f2.stuffLines()
	if len(got) != 1 || got[0] != "c_shutdown(false)\n" {
		t.Errorf("saveFirst=false 必须显式传 false，缺省会变成保存后关闭: %+v", got)
	}

	// 未运行的分片
	if err := d.Stop(ctx, "Cluster_1", dst.ShardCaves, true); !errors.Is(err, ErrNotRunning) {
		t.Errorf("停一个没跑的分片应报 ErrNotRunning: %v", err)
	}
}

func TestConsoleInjectsExactlyOneLine(t *testing.T) {
	ctx := context.Background()
	p := preparedPaths(t, dst.ShardMaster)
	f := newFakeRunner()
	d := newDriver(p, f)
	if err := d.Start(ctx, "Cluster_1", dst.ShardMaster); err != nil {
		t.Fatal(err)
	}

	if err := d.Console(ctx, "Cluster_1", dst.ShardMaster, `c_say("你好")`); err != nil {
		t.Fatalf("Console: %v", err)
	}
	c := f.callByFirstArg(t, "-S")
	if c.args[1] != "dst-Cluster_1-Master" ||
		strings.Join(c.args[2:4], " ") != "-X stuff" {
		t.Errorf("screen 参数不对: %v", c.args)
	}
	lines := f.stuffLines()
	if len(lines) != 1 || lines[0] != `c_say("你好")`+"\n" {
		t.Errorf("注入内容被改动: %+v", lines)
	}

	// 多行、空行、超长都要挡住。
	for _, bad := range []string{"", "   ", "b_save()\nc_shutdown()", "c_say('a')\r\n", strings.Repeat("x", 2001)} {
		if err := d.Console(ctx, "Cluster_1", dst.ShardMaster, bad); err == nil {
			t.Errorf("本该拒绝: %q", bad)
		}
	}
	// 挡住之后不该再打到 screen。
	if len(f.stuffLines()) != 1 {
		t.Errorf("非法命令也送到 screen 了: %+v", f.stuffLines())
	}
}

func TestConsoleReportsDeadSession(t *testing.T) {
	ctx := context.Background()
	p := preparedPaths(t, dst.ShardMaster)
	f := newFakeRunner()
	d := newDriver(p, f)
	// 手工登记一个"存在但 stuff 失败"的会话，模拟进程卡死。
	f.sessions["dst-Cluster_1-Master"] = 1
	f.failStuffFor = "dst-Cluster_1-Master"
	if err := d.Console(ctx, "Cluster_1", dst.ShardMaster, "b_save()"); err == nil ||
		!errors.Is(err, ErrNotRunning) {
		t.Errorf("stuff 打到死会话应翻成 ErrNotRunning: %v", err)
	}
}

func TestKillRemovesSession(t *testing.T) {
	ctx := context.Background()
	p := preparedPaths(t, dst.ShardMaster)
	f := newFakeRunner()
	d := newDriver(p, f)
	if err := d.Start(ctx, "Cluster_1", dst.ShardMaster); err != nil {
		t.Fatal(err)
	}
	if err := d.Kill(ctx, "Cluster_1", dst.ShardMaster); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if r, _ := d.IsRunning(ctx, "Cluster_1", dst.ShardMaster); r {
		t.Error("Kill 后会话还在")
	}
	if err := d.Kill(ctx, "Cluster_1", dst.ShardMaster); err == nil {
		t.Error("重复 Kill 应报错")
	}
}

func TestRunAdminCommandUsesWhitelist(t *testing.T) {
	ctx := context.Background()
	p := preparedPaths(t, dst.ShardMaster)
	f := newFakeRunner()
	d := newDriver(p, f)
	if err := d.Start(ctx, "Cluster_1", dst.ShardMaster); err != nil {
		t.Fatal(err)
	}
	line, err := d.RunAdminCommand(ctx, "Cluster_1", dst.ShardMaster, "announce",
		map[string]string{"message": `存档 5 分钟后回档"); c_shutdown(); ("`})
	if err != nil {
		t.Fatalf("RunAdminCommand: %v", err)
	}
	if !strings.HasPrefix(line, "c_announce(\"") {
		t.Errorf("返回的命令不像公告: %s", line)
	}
	got := f.stuffLines()
	last := got[len(got)-1]
	if strings.Count(last, "c_shutdown") != 1 || !strings.Contains(last, `\"`) {
		t.Errorf("注入内容没被转义: %s", last)
	}
	if _, err := d.RunAdminCommand(ctx, "Cluster_1", dst.ShardMaster, "forcecrash", nil); err == nil {
		t.Error("白名单外的 key 应该拒绝")
	}
	if _, err := d.RunAdminCommand(ctx, "Cluster_1", dst.ShardMaster, "rollback",
		map[string]string{"count": "-1"}); err == nil {
		t.Error("非法参数应该拒绝")
	}
	// 被拒绝的命令不该碰到 screen
	if n := len(f.stuffLines()); n != len(got) {
		t.Errorf("拒绝的命令仍然注入了 %+v", f.stuffLines()[len(got):])
	}
}

func TestParseScreenList(t *testing.T) {
	out := `There are screens on:
	20314.dst-Cluster_1-Master	(Detached)
	24007.dst-Cluster_1-Caves	(Attached)
	99.other-thing	(Detached)
	 3000 .weird
	abc.dst-Cluster_2-Master	(Detached)
	31337.dst-Cluster_9-Master	(Detached)
No Sockets found? whatever
`
	got := parseScreenList(out)
	if len(got) != 3 {
		t.Fatalf("应该只收下面板自己的 3 个会话: %+v", got)
	}
	if got["dst-Cluster_1-Master"] != 20314 || got["dst-Cluster_9-Master"] != 31337 {
		t.Errorf("PID 解析不对: %+v", got)
	}
	if _, ok := got["other-thing"]; ok {
		t.Error("别人的会话被收进来了")
	}
	if _, ok := got["abc.dst-Cluster_2-Master"]; ok {
		t.Error("PID 不是数字的行没被跳过")
	}
	if parseScreenList("No Sockets found.\n") != nil && len(parseScreenList("No Sockets found.\n")) != 0 {
		t.Error("无会话应该得到空表")
	}
}

func TestQuoteShell(t *testing.T) {
	if got := quoteShell("/tmp/a b"); got != "'/tmp/a b'" {
		t.Errorf("空格没保护: %s", got)
	}
	if got := quoteShell("it's"); got != `'it'\''s'` {
		t.Errorf("单引号转义不对: %s", got)
	}
}

func TestReadLogTail(t *testing.T) {
	p := preparedPaths(t, dst.ShardMaster)
	d := newDriver(p, newFakeRunner())
	txt, err := d.ReadLogTail("Cluster_1", dst.ShardMaster, 5)
	if err != nil || txt != "" {
		t.Errorf("没有日志文件时不该报错: %q %v", txt, err)
	}
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf("[00:00:%02d] line %d", i, i))
	}
	if err := os.WriteFile(p.ServerLog("Cluster_1", dst.ShardMaster),
		[]byte(strings.Join(lines, "\r\n")+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadLogTail("Cluster_1", dst.ShardMaster, 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "line") != 3 || !strings.Contains(got, "line 39") {
		t.Errorf("末尾取错: %q", got)
	}
}
