package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
)

// fakeScreen 是一个会改自己会话表的 screen。
//
// 生命周期接口的关键在于"调用顺序"和"调用次数"：重启必须是先关再起、
// 关要等会话真消失、启动不能双开。固定输出的 screenRunner 测不出这些，
// 所以这里按真 screen 的退出码语义建模（找不到会话时输出 No such session）。
type fakeScreen struct {
	sessions map[string]int
	calls    []string
	// injected 记录每一次 -X stuff 送进去的行，用于断言"到底注入了什么"。
	injected   []string
	nextPID    int
	keepsAlive bool
}

func newFakeScreen() *fakeScreen {
	return &fakeScreen{sessions: map[string]int{}, nextPID: 20314}
}

func (f *fakeScreen) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) == 0 {
		return "", fmt.Errorf("screen 缺参数")
	}
	switch args[0] {
	case "-ls":
		if len(f.sessions) == 0 {
			// 真 screen 这时退出码 1 且只打一行提示，isNotFoundExit 会把它归一化成空输出。
			return "No Sockets found in /tmp.\n", nil
		}
		names := make([]string, 0, len(f.sessions))
		for k := range f.sessions {
			names = append(names, k)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, n := range names {
			fmt.Fprintf(&b, "\t%d.%s\t(Detached)\n", f.sessions[n], n)
		}
		return b.String(), nil
	case "-dmS":
		f.nextPID++
		f.sessions[args[1]] = f.nextPID
		return "", nil
	case "-S":
		session := args[1]
		if _, ok := f.sessions[session]; !ok {
			return "No such session " + session + "\n", nil
		}
		if len(args) >= 4 && args[2] == "-X" {
			switch args[3] {
			case "quit":
				delete(f.sessions, session)
			case "stuff":
				line := args[4]
				f.injected = append(f.injected, strings.TrimSuffix(line, "\n"))
				// c_shutdown 的语义是"落盘后进程退出"，会话随之消失。
				if strings.Contains(line, "c_shutdown") && !f.keepsAlive {
					delete(f.sessions, session)
				}
			}
		}
		return "", nil
	}
	return "", nil
}

func (f *fakeScreen) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeScreen) running(cluster, shard string) bool {
	_, ok := f.sessions[fmt.Sprintf("dst-%s-%s", cluster, shard)]
	return ok
}

// makeStartable 补齐 CheckPrereqs 要的四样东西，让"能启动"这个前提是真的成立，
// 而不是靠把前置检查改成永远通过。
func (f *apiFixture) makeStartable(t *testing.T) {
	t.Helper()
	bin := f.paths.ServerBinary()
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("建 bin64: %v", err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写服务端二进制: %v", err)
	}
	if err := os.WriteFile(f.paths.TokenFile(f.cluster), []byte("DU_fake_token_for_tests\n"), 0o600); err != nil {
		t.Fatalf("写 cluster_token.txt: %v", err)
	}
	for _, shard := range []string{"Master", "Caves"} {
		if err := os.WriteFile(f.paths.ShardIni(f.cluster, shard), []byte("[MISC]\n"), 0o644); err != nil {
			t.Fatalf("写 %s/server.ini: %v", shard, err)
		}
	}
}

func (f *apiFixture) useFakeScreen(t *testing.T) *fakeScreen {
	t.Helper()
	r := newFakeScreen()
	d := procrun.New(f.paths, r)
	// 假 screen 不会写出日志，2 秒的启动日志确认在这里纯粹是白等；
	// 那条判定本身在 procrun 包里有专门用例钉住。
	d.BootLogWait = 50 * time.Millisecond
	f.srv.Procs = d
	return r
}

const startPath = "/api/v1/clusters/Cluster_1/shards/Master"

func TestShardStartReportsPrereqsAsActionable400(t *testing.T) {
	f := newFixture(t, "")
	fs := f.useFakeScreen(t)

	rec := doJSON(t, f.srv.Handler(), http.MethodPost, startPath+"/start", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺前置条件时 = %d，期望 400，body %s", rec.Code, rec.Body.String())
	}
	msg := decodeReply(t, rec).Err
	for _, want := range []string{"cluster_token.txt", "服务端二进制"} {
		if !strings.Contains(msg, want) {
			t.Errorf("前置检查提示没提到 %q：%q", want, msg)
		}
	}
	if strings.Contains(msg, "面板内部错误") {
		t.Errorf("把可修复的环境问题报成了面板故障：%q", msg)
	}
	// 前置没过就绝不能碰 screen，否则用户以为在启动。
	if len(fs.calls) != 0 {
		t.Errorf("前置检查失败还调了 screen：%v", fs.calls)
	}
}

func TestShardStartStopKillLifecycle(t *testing.T) {
	f := newFixture(t, "")
	f.makeStartable(t)
	fs := f.useFakeScreen(t)
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, startPath+"/start", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("启动 = %d，body %s", rec.Code, rec.Body.String())
	}
	var v shardStateView
	decodeData(t, rec, &v)
	if !v.Running || v.Session != "dst-Cluster_1-Master" || v.Pid <= 0 {
		t.Errorf("启动后的状态 = %+v，期望会话在跑且带 PID", v)
	}
	if !strings.Contains(v.Hint, "助手") {
		t.Errorf("启动提示没告诉用户接下来等什么：%q", v.Hint)
	}
	if fs.count("-dmS") != 1 {
		t.Errorf("screen 创建调用数 = %d，期望 1：%v", fs.count("-dmS"), fs.calls)
	}

	// 重复启动必须是 409，而不是又建一个会话：同端口双开会静默退出。
	rec = doJSON(t, h, http.MethodPost, startPath+"/start", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("重复启动 = %d，期望 409，body %s", rec.Code, rec.Body.String())
	}
	if fs.count("-dmS") != 1 {
		t.Errorf("重复启动竟然又建了一次会话：%v", fs.calls)
	}

	rec = doJSON(t, h, http.MethodPost, startPath+"/stop", map[string]any{"saveFirst": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭 = %d，body %s", rec.Code, rec.Body.String())
	}
	if len(fs.injected) != 1 || fs.injected[0] != "c_shutdown(false)" {
		t.Errorf("不落盘关闭注入的命令 = %v，期望只有一条 c_shutdown(false)", fs.injected)
	}
	if fs.running("Cluster_1", "Master") {
		t.Error("发完关闭命令还会话在")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "不落盘") {
		t.Errorf("关闭响应没写清存档停在哪儿：%s", body)
	}

	// 已经不在了再关 / 再强关，都应该是 409 而不是 500。
	for _, path := range []string{"/stop", "/kill"} {
		rec = doJSON(t, h, http.MethodPost, startPath+path, nil)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s 对未运行的分片 = %d，期望 409，body %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(decodeReply(t, rec).Err, "file does not exist") {
			t.Errorf("%s 把内部错误甩给了界面：%s", path, rec.Body.String())
		}
	}

	// kill 在会话还在时直接掐，不等 c_shutdown。
	doJSON(t, h, http.MethodPost, startPath+"/start", nil)
	before := len(fs.injected)
	rec = doJSON(t, h, http.MethodPost, startPath+"/kill", nil)
	if rec.Code != http.StatusOK || fs.running("Cluster_1", "Master") {
		t.Errorf("强关 = %d，还在跑=%v", rec.Code, fs.running("Cluster_1", "Master"))
	}
	if len(fs.injected) != before {
		t.Errorf("强关不该注入控制台命令，却发了 %v", fs.injected[before:])
	}
}

func TestShardRestartStopsBeforeStarts(t *testing.T) {
	f := newFixture(t, "")
	f.makeStartable(t)
	fs := f.useFakeScreen(t)
	h := f.srv.Handler()

	doJSON(t, h, http.MethodPost, startPath+"/start", nil)
	pidBefore := fs.sessions["dst-Cluster_1-Master"]
	rec := doJSON(t, h, http.MethodPost, startPath+"/restart", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("重启 = %d，body %s", rec.Code, rec.Body.String())
	}
	// 顺序是唯一有意义的断言：先建会话再关，等于把两个服务端挤在同一端口上。
	var sawShutdown, sawQuit bool
	for _, c := range fs.calls {
		switch {
		case strings.Contains(c, "stuff") && strings.Contains(c, "c_shutdown()"):
			sawShutdown = true
		case strings.HasPrefix(c, "-dmS") && sawShutdown:
			sawQuit = true
		}
	}
	if !sawShutdown || !sawQuit {
		t.Errorf("重启没走成\"先关后起\"：%v", fs.calls)
	}
	if fs.count("-dmS") != 2 {
		t.Errorf("创建会话次数 = %d，期望 2（初次启动 + 重启）：%v", fs.count("-dmS"), fs.calls)
	}
	if fs.sessions["dst-Cluster_1-Master"] == pidBefore {
		t.Error("重启后 PID 没变，说明没真的重建会话")
	}
	var v shardStateView
	decodeData(t, rec, &v)
	if !v.Running || !strings.Contains(v.Hint, "模组") {
		t.Errorf("重启响应 = %+v，期望带\"模组改动会生效\"的提示", v)
	}

	// 没在跑时分片时，restart 退化成启动，不能报"未在运行"。
	fs2 := f.useFakeScreen(t)
	rec = doJSON(t, h, http.MethodPost, "/api/v1/clusters/Cluster_1/shards/Caves/restart", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("对未运行分片 restart = %d，期望 200，body %s", rec.Code, rec.Body.String())
	}
	if fs2.count("-dmS") != 1 || !fs2.running("Cluster_1", "Caves") {
		t.Errorf("没在跑时应该直接起，calls=%v", fs2.calls)
	}
}

func TestShardRoutesRejectUnknownTargets(t *testing.T) {
	f := newFixture(t, "")
	fs := f.useFakeScreen(t)
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, "/api/v1/clusters/Cluster_9/shards/Master/start", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的集群 = %d，期望 404", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/clusters/Cluster_1/shards/Cave/start", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的分片 = %d，期望 404，body %s", rec.Code, rec.Body.String())
	}
	if len(fs.calls) != 0 {
		t.Errorf("作用域校验失败后还是碰了 screen：%v", fs.calls)
	}
}

func TestAdminCommandsAreListedInChinese(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/admin-commands", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("命令列表 = %d，body %s", rec.Code, rec.Body.String())
	}
	var out []consoleCommandView
	decodeData(t, rec, &out)
	if len(out) < 6 {
		t.Fatalf("白名单命令只剩 %d 条", len(out))
	}
	var danger int
	for _, c := range out {
		if c.Label == "" || c.Label == c.Key || c.Desc == "" {
			t.Errorf("命令 %q 没配上中文标签或说明：%+v", c.Key, c)
		}
		if c.Danger {
			danger++
		}
		for _, a := range c.Args {
			if a.Label == "" || a.Label == a.Name {
				t.Errorf("%s 的参数 %q 没中文标签", c.Key, a.Name)
			}
			// 界面拿到的是取值 + 中文，两个都得给，否则要么显示码值要么没法提交。
			for _, ch := range a.Choices {
				if ch.Value == "" || ch.Label == "" {
					t.Errorf("%s 的选项不完整：%+v", c.Key, ch)
				}
			}
		}
	}
	if danger == 0 {
		t.Error("一条危险命令都没标出来，前端没法做二次确认")
	}
	// 危险命令必须排在后面：界面按这个顺序渲染，误触成本低的先给。
	var sawSafe bool
	for _, c := range out {
		if !c.Danger {
			sawSafe = true
		} else if !sawSafe {
			t.Errorf("危险命令 %s 排在了安全命令前面", c.Key)
		}
	}
}

func TestDangerousAdminCommandNeedsServerSideConfirm(t *testing.T) {
	f := newFixture(t, "")
	f.makeStartable(t)
	fs := f.useFakeScreen(t)
	h := f.srv.Handler()
	doJSON(t, h, http.MethodPost, startPath+"/start", nil)

	rec := doJSON(t, h, http.MethodPost, startPath+"/admin-commands",
		map[string]any{"command": "rollback", "args": map[string]string{"count": "2"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没带 confirm 的危险命令 = %d，期望 400，body %s", rec.Code, rec.Body.String())
	}
	if msg := decodeReply(t, rec).Err; !strings.Contains(msg, "confirm") && !strings.Contains(msg, "回档") {
		t.Errorf("提示没说明为什么被拦：%q", msg)
	}
	// 拦住了就必须一条命令都没注入，这是这条接口唯一的安全价值。
	if len(fs.injected) != 0 {
		t.Errorf("被拒的危险命令还是注入了：%v", fs.injected)
	}

	rec = doJSON(t, h, http.MethodPost, startPath+"/admin-commands",
		map[string]any{"command": "rollback", "args": map[string]string{"count": "2"}, "confirm": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("确认后 = %d，body %s", rec.Code, rec.Body.String())
	}
	var reply adminCommandReply
	decodeData(t, rec, &reply)
	if reply.Line != "c_rollback(2)" || len(fs.injected) != 1 || fs.injected[0] != "c_rollback(2)" {
		t.Errorf("回档没按参数注入：line=%q injected=%v", reply.Line, fs.injected)
	}

	// 越界次数走参数校验，同样是 400，而且不能留下半条命令。
	rec = doJSON(t, h, http.MethodPost, startPath+"/admin-commands",
		map[string]any{"command": "rollback", "args": map[string]string{"count": "99"}, "confirm": true})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("越界次数 = %d，期望 400，body %s", rec.Code, rec.Body.String())
	}
	if len(fs.injected) != 1 {
		t.Errorf("非法参数之后又多注入了一条：%v", fs.injected)
	}
}

func TestAdminCommandRejectsUnknownName(t *testing.T) {
	f := newFixture(t, "")
	fs := f.useFakeScreen(t)
	rec := doJSON(t, f.srv.Handler(), http.MethodPost, startPath+"/admin-commands",
		map[string]any{"command": "c_regenerateshard(); print(1)"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("白名单外的命令 = %d，期望 400", rec.Code)
	}
	msg := decodeReply(t, rec).Err
	// 拒绝消息要给出可用的那几条（界面据此渲染按钮），同时原样回显请求串只是给用户看清自己填了什么。
	if !strings.Contains(msg, "announce") || !strings.Contains(msg, "save") {
		t.Errorf("拒绝消息没列出白名单命令：%q", msg)
	}
	if len(fs.calls) != 0 {
		t.Errorf("白名单外命令居然走到了 screen：%v", fs.calls)
	}
}

func TestAdminCommandEscapesStringArgIntoOneCall(t *testing.T) {
	f := newFixture(t, "")
	f.makeStartable(t)
	fs := f.useFakeScreen(t)
	h := f.srv.Handler()
	doJSON(t, h, http.MethodPost, startPath+"/start", nil)

	payload := `开服\"); c_rollback(9`
	rec := doJSON(t, h, http.MethodPost, startPath+"/admin-commands",
		map[string]any{"command": "announce", "args": map[string]string{"message": payload}})
	if rec.Code != http.StatusOK {
		t.Fatalf("公告 = %d，body %s", rec.Code, rec.Body.String())
	}
	if len(fs.injected) != 1 {
		t.Fatalf("注入条数 = %d，期望 1：%v", len(fs.injected), fs.injected)
	}
	got := fs.injected[0]
	// 反斜杠被抬成 `\\`、引号被抬成 `\"`，所以整行只有开头和结尾两个字符串定界符，
	// 后面那段 c_rollback(9 全在字面量里，不会被 Lua 当成第二条语句。
	want := `c_announce("开服\\\"); c_rollback(9")`
	if got != want {
		t.Errorf("转义结果不对\n  实到 %q\n  期望 %q", got, want)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("注入的命令里还有换行：%q", got)
	}

	// 带换行的参数在建命令阶段就该被拒，绝不能走到 screen。
	rec = doJSON(t, h, http.MethodPost, startPath+"/admin-commands",
		map[string]any{"command": "announce", "args": map[string]string{"message": "第一行\n第二行"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("多行参数 = %d，期望 400，body %s", rec.Code, rec.Body.String())
	}
	if len(fs.injected) != 1 {
		t.Errorf("多行参数被注入了：%q", fs.injected)
	}

	// 枚举参数只接受白名单取值，且错误消息给中文标签。
	rec = doJSON(t, h, http.MethodPost, startPath+"/admin-commands",
		map[string]any{"command": "force_phase", "args": map[string]string{"phase": "day(); --"}, "confirm": true})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法时段 = %d，期望 400", rec.Code)
	}
	if msg := decodeReply(t, rec).Err; !strings.Contains(msg, "白天") {
		t.Errorf("枚举报错没给中文选项：%q", msg)
	}
}

func TestAdminCommandNeedsLiveSession(t *testing.T) {
	f := newFixture(t, "")
	fs := f.useFakeScreen(t)
	rec := doJSON(t, f.srv.Handler(), http.MethodPost, startPath+"/admin-commands",
		map[string]any{"command": "save"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("分片没开着 = %d，期望 409，body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(decodeReply(t, rec).Err, "未在运行") {
		t.Errorf("提示没说明是分片没开：%q", decodeReply(t, rec).Err)
	}
	if fs.count("-S") != 1 {
		t.Errorf("只该试一次会话注入，calls=%v", fs.calls)
	}
}

func TestLifecycleWritesAuditWithInjectedLine(t *testing.T) {
	f := newFixture(t, "")
	f.makeStartable(t)
	f.useFakeScreen(t)
	h := f.srv.Handler()

	doJSON(t, h, http.MethodPost, startPath+"/start", nil)
	doJSON(t, h, http.MethodPost, startPath+"/admin-commands",
		map[string]any{"command": "announce", "args": map[string]string{"message": "十分钟后维护"}})
	doJSON(t, h, http.MethodPost, startPath+"/stop", nil)

	entries, err := f.srv.Store.ListAudit(50, 0, "")
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	byAction := map[string]store.AuditEntry{}
	for _, e := range entries {
		byAction[e.Action] = e
	}
	for _, action := range []string{"shard.start", "admin.announce", "shard.stop"} {
		e, ok := byAction[action]
		if !ok {
			t.Errorf("操作记录里没有 %s：%+v", action, entries)
			continue
		}
		if !e.OK || e.Actor != "admin" || e.Target != "Cluster_1/Master" {
			t.Errorf("%s 记成了 %+v", action, e)
		}
	}
	// 危险动作的审计必须能看到实际注入的那行 Lua，只记"执行过"没法复盘。
	if e := byAction["admin.announce"]; !strings.Contains(e.Detail, "c_announce(") {
		t.Errorf("公告的审计没留下注入原文：%+v", e)
	}
}

// TestShardStartWithRealRunnerAndNoScreen 用真的 ExecRunner 跑：
// 这台 Windows 上确实没有 screen，错误是 exec 自己产生的，比手写的假错误可靠。
func TestShardStartWithRealRunnerAndNoScreen(t *testing.T) {
	f := newFixture(t, "")
	f.makeStartable(t)
	f.srv.Procs = procrun.New(f.paths, procrun.ExecRunner{})
	rec := doJSON(t, f.srv.Handler(), http.MethodPost, startPath+"/start", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("screen 缺失时 = %d，期望 503，body %s", rec.Code, rec.Body.String())
	}
	msg := decodeReply(t, rec).Err
	if !strings.Contains(msg, "screen") || !strings.Contains(msg, "同一台机器") {
		t.Errorf("提示没告诉用户该做什么：%q", msg)
	}
	if strings.Contains(msg, "executable file not found") {
		t.Errorf("把 Go 的英文原文给了界面：%q", msg)
	}
}
