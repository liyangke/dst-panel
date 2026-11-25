package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/dst"
	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
)

// fakeLines 记录"到底执行了什么命令"，并把预设的输出逐行喂回回调。
type fakeLines struct {
	name    string
	args    []string
	emitted []string
	err     error
	// onLine 之后、返回之前执行，用来模拟"跑到一半外部命令被杀"。
	onFinish func()
	// 调用次数：前置检查没过时必须是 0。
	calls int
}

func (f *fakeLines) RunLines(ctx context.Context, name string, args []string, onLine func(string)) error {
	f.calls++
	f.name = name
	f.args = args
	for _, line := range f.emitted {
		onLine(line)
	}
	if f.onFinish != nil {
		f.onFinish()
	}
	return f.err
}

type workerFixture struct {
	w     *Worker
	lines *fakeLines
	st    *store.Store
}

func newWorkerFixture(t *testing.T, emitted []string, runErr error) *workerFixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "panel.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	steam := filepath.Join(root, "steamcmd.sh")
	if err := os.WriteFile(steam, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写 steamcmd: %v", err)
	}
	paths := dst.Paths{KleiRoot: filepath.Join(root, "klei"), DstInstall: filepath.Join(root, "dst", "dedicated_server")}
	lines := &fakeLines{emitted: emitted, err: runErr}
	w := &Worker{
		Store:    st,
		Paths:    paths,
		Lines:    lines,
		SteamCmd: steam,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Poll:     10 * time.Millisecond,
	}
	return &workerFixture{w: w, lines: lines, st: st}
}

// collectReportsFull 直接调 serverUpdate，把每次 report 的（百分比, 消息, 字节明细）按顺序记下来。
// 任务行的 progress 会被 ClaimTask 清零、被收尾覆盖，中间序列只能这样验。
func collectReportsFull(t *testing.T, w *Worker) (error, []int, []string, []*store.TaskDetail) {
	t.Helper()
	var percents []int
	var messages []string
	var details []*store.TaskDetail
	err := w.serverUpdate(context.Background(), func(p int, m string, d *store.TaskDetail) {
		percents = append(percents, p)
		messages = append(messages, m)
		details = append(details, d)
	})
	return err, percents, messages, details
}

// collectReports 是不关心字节明细的用例用的那半边。
func collectReports(t *testing.T, w *Worker) (error, []int, []string) {
	t.Helper()
	err, percents, messages, _ := collectReportsFull(t, w)
	return err, percents, messages
}

func TestServerUpdateRunsPinnedSteamcmdArguments(t *testing.T) {
	f := newWorkerFixture(t, []string{
		// 这一段是 2026-11-01 云机真跑 app_update 343050 抄下来的形状，
		// 不是按"steamcmd 大概长这样"编的：上一版夹具用的是假形状，
		// 于是解析器收下了 steamcmd 自更新的百分比，测试全绿而界面在说谎。
		"Redirecting stderr to '/data/dst/Steam/logs/stderr.txt'",
		"[  0%] Checking for available updates...",
		"[----] Verifying installation...",
		"[ 37%] Downloading update...",
		"[ 55%] Downloading package steamcmd_bins_linux...",
		" Update state (0x3) reconfiguring, progress: 0.00 (0 / 0)",
		" Update state (0x61) downloading, progress: 1.07 (49005024 / 4594664662)",
		" Update state (0x61) downloading, progress: 45.00 (2067599098 / 4594664662)",
		"Success! App '343050' fully installed.",
	}, nil)

	err, percents, _ := collectReports(t, f.w)
	if err != nil {
		t.Fatalf("serverUpdate: %v", err)
	}
	if f.lines.calls != 1 {
		t.Fatalf("调用次数 = %d，期望 1", f.lines.calls)
	}
	if f.lines.name != f.w.SteamCmd {
		t.Errorf("执行的文件 = %q，期望配置里的 %q", f.lines.name, f.w.SteamCmd)
	}
	want := []string{"+@sSteamCmdForcePlatformType", "linux",
		"+force_install_dir", f.w.Paths.DstInstall, "+login", "anonymous",
		"+app_update", dst.ServerAppID, "validate", "+quit"}
	if strings.Join(f.lines.args, " ") != strings.Join(want, " ") {
		t.Errorf("steamcmd 参数不对\n  实到 %v\n  期望 %v", f.lines.args, want)
	}
	// 绝不经过 shell：参数里没有 -c/bash，也没有把整串命令拼成一个参数。
	for _, a := range f.lines.args {
		if a == "-c" || a == "bash" || a == "sh" || strings.Contains(a, " && ") {
			t.Errorf("参数里出现了 shell 包装的形状： %v", f.lines.args)
		}
	}
	if len(percents) == 0 || percents[0] != 1 {
		t.Errorf("开局就该报 1%% 让界面知道任务在动：%v", percents)
	}
	if percents[len(percents)-1] != 100 {
		t.Errorf("成功收尾要报满 100%%：%v", percents)
	}
	// 45% 那行是 2067599098/4594664662 ≈ 44.9 的整包字节比，取整后是 44。
	if !containsPercent(percents, 44) {
		t.Errorf("进度序列没按整包字节走：%v", percents)
	}
	// 37% 和 55% 是 steamcmd 在量它自己那两百来兆，跟本体无关：
	// 收进序列就会画出一条"本体已经下了一小半、实际一个字节都没动"的假条子（云机实测踩过）。
	for _, fake := range []int{37, 55} {
		if containsPercent(percents, fake) {
			t.Errorf("把 steamcmd 自更新的 %d%% 当成了本体进度：%v", fake, percents)
		}
	}
}

func containsPercent(xs []int, n int) bool {
	for _, x := range xs {
		if x == n {
			return true
		}
	}
	return false
}

// TestServerUpdateOnFreshMachineIsADownload 钉的是"从零下载"这条路。
// 全新机器上 paths.dstInstall 还不存在，命令和参数一个都不用换（app_update 会自建目录），
// 但报告里必须说"下载"：沿用"更新"会让第一次部署的人以为这一步只对已有安装可用，
// 于是绕回去用命令行自己拉——面板里那条进度条就白做了。
func TestServerUpdateOnFreshMachineIsADownload(t *testing.T) {
	f := newWorkerFixture(t, []string{"Success! App '343050' fully installed."}, nil)
	err, _, messages := collectReports(t, f.w)
	if err != nil {
		t.Fatalf("安装目录不存在时也要能跑：%v", err)
	}
	if !strings.Contains(messages[0], "开始下载") {
		t.Errorf("开局报告没说明这是下载：%q", messages[0])
	}
	if !strings.Contains(messages[len(messages)-1], "已下载") {
		t.Errorf("收尾报告没说明这是下载：%q", messages[len(messages)-1])
	}
}

// TestServerUpdateReportsBuildNumber 钉的是那句"服务端版本太旧"的对端：
// 用户要拿去做比较的是构建号，报告里只写"成功"等于让他自己去翻 version.txt。
func TestServerUpdateReportsBuildNumber(t *testing.T) {
	f := newWorkerFixture(t, []string{"Success! App '343050' fully installed."}, nil)
	if err := os.MkdirAll(filepath.Dir(f.w.Paths.ServerBinary()), 0o755); err != nil {
		t.Fatalf("造安装目录: %v", err)
	}
	if err := os.WriteFile(f.w.Paths.ServerBinary(), []byte("elf"), 0o755); err != nil {
		t.Fatalf("造服务端二进制: %v", err)
	}
	if err := os.WriteFile(f.w.Paths.VersionFile(), []byte("760944\n"), 0o644); err != nil {
		t.Fatalf("写 version.txt: %v", err)
	}
	err, _, messages := collectReports(t, f.w)
	if err != nil {
		t.Fatalf("serverUpdate: %v", err)
	}
	if !strings.Contains(messages[0], "开始更新") {
		t.Errorf("已有本体时该报更新而不是下载：%q", messages[0])
	}
	last := messages[len(messages)-1]
	if !strings.Contains(last, "760944") {
		t.Errorf("收尾报告里要带 version.txt 的构建号：%q", last)
	}
}

func TestServerUpdateProgressNeverGoesBackwards(t *testing.T) {
	// 换 depot 与"下载转校验"时 steamcmd 会把数字打回低位数，倒着跳会让界面看起来像重跑。
	f := newWorkerFixture(t, []string{
		" Update state (0x61) downloading, progress: 60.00 (2756798797 / 4594664662)",
		" Update state (0x61) downloading, progress: 0.10 (4718592 / 471859200)",
		" Update state (0x61) validating, progress: 3.00 (138000000 / 4594664662)",
	}, nil)
	err, percents, _ := collectReports(t, f.w)
	if err != nil {
		t.Fatalf("serverUpdate: %v", err)
	}
	for i := 1; i < len(percents); i++ {
		if percents[i] < percents[i-1] {
			t.Fatalf("进度倒退了：%v", percents)
		}
	}
	// 最后一条是成功收尾的 100%，它前面那条才是"保持在 60 不回退"的证据。
	if len(percents) < 2 {
		t.Fatalf("进度序列太短，看不出回退：%v", percents)
	}
	// 60.00 是 steamcmd 报的整包比，面板把途中上限压在 99%，2756798797*99/4594664662 取整就是 59。
	if got := percents[len(percents)-2]; got != 59 {
		t.Errorf("60%% 之后的低百分比没被压住，倒数第二条 = %d，全程 %v", got, percents)
	}
}

// TestBootstrapPercentLinesDoNotMoveBar 是 2026-11-01 那个假进度条的回归测试。
// 当时的现象：界面上条子已经爬到 37%、写着"下载服务端本体文件"，
// 而 dedicated_server 目录还没被创建、一个字节都没落盘——
// 那些百分比全部来自 steamcmd 给自己做自更新的那两百来兆。
func TestBootstrapPercentLinesDoNotMoveBar(t *testing.T) {
	f := newWorkerFixture(t, []string{
		"[  0%] Checking for available updates...",
		"[----] Verifying installation...",
		"[ 37%] Downloading update...",
		"[  0%] Downloading package steamcmd_bins_linux...",
		"[ 92%] Downloading package steamcmd_bins_linux...",
		"[----] Unpacking package steamcmd_bins_linux...",
	}, nil)
	err, percents, messages, details := collectReportsFull(t, f.w)
	if err != nil {
		t.Fatalf("serverUpdate: %v", err)
	}
	// 最后一条是收尾的 100%，不在这条断言的范围里。
	for i, p := range percents[:len(percents)-1] {
		// 首条是开局的 1%（面板自己报的"任务在动"），其余都必须还停在 1。
		if p != 1 {
			t.Fatalf("第 %d 次报告把 steamcmd 自更新的百分比当成了本体进度：%v", i, percents)
		}
	}
	for _, m := range messages {
		// 认的是 progressLine 那几句的形状，不是"下载""已下"这种词：
		// 开局和收尾两条本来就要说"下载服务端本体"，按词查会误报。
		for _, bad := range []string{"实测", "已下 ", "约还需", "就要下完了"} {
			if strings.Contains(m, bad) {
				t.Errorf("本体还没开始下载就报了字节进度：%q", m)
			}
		}
	}
	for i, d := range details {
		if d != nil {
			t.Errorf("第 %d 次报告给出了字节明细，可这一段确实一个字节都没下：%+v", i, *d)
		}
	}
	if !strings.Contains(messages[len(messages)-2], "steamcmd 正在更新自己") {
		t.Errorf("最后一段要说清卡在谁身上：%q", messages[len(messages)-2])
	}
}

// TestDetailCarriesMeasuredBytes 钉的是"数字块"那半边：界面显示的已下/总量/速度/剩余
// 直接取自这一组数，不再从中文句子里解析。
func TestDetailCarriesMeasuredBytes(t *testing.T) {
	f := newWorkerFixture(t, []string{
		" Update state (0x61) downloading, progress: 1.07 (49005024 / 4594664662)",
		" Update state (0x61) downloading, progress: 9.33 (428750751 / 4594664662)",
	}, nil)
	err, _, _, details := collectReportsFull(t, f.w)
	if err != nil {
		t.Fatalf("serverUpdate: %v", err)
	}
	// details 的形状是：开局那条（面板自己报的，没有字节）→ 每行 depot 一条 → 收尾 100% 一条。
	// 收尾那条按设计不带字节数，所以这里看的是倒数第二条。
	if len(details) != 4 {
		t.Fatalf("报告次数 = %d，期望 1 条开局 + 2 条 depot + 1 条收尾：%+v", len(details), details)
	}
	last := details[len(details)-2]
	if last == nil {
		t.Fatal("已经有整包字节量却没给出明细")
	}
	if last.TotalBytes != 4594664662 || last.DoneBytes != 428750751 {
		t.Errorf("字节数不是从输出里读的：done=%d total=%d", last.DoneBytes, last.TotalBytes)
	}
	if last.Phase != "下载服务端本体文件" {
		t.Errorf("阶段名 = %q", last.Phase)
	}
	if !last.Downloading {
		t.Error("0x61 downloading 那一档必须标成正在下载，界面据此决定说已下还是已处理")
	}
	// 两行之间没有 2 秒真实间隔（fake 是瞬间喂完的），第一窗测不出速度，只能为 0。
	if last.BytesPerSec != 0 || last.EtaSeconds != 0 {
		t.Errorf("还没采满一窗就报了速度和剩余时间：%.1f B/s, %.0f s", last.BytesPerSec, last.EtaSeconds)
	}
	if details[1] == nil {
		t.Error("第一条 depot 行就该带明细，界面得先看到已下多少")
	}
	if details[0] != nil || details[len(details)-1] != nil {
		t.Errorf("开局和收尾两条不该带字节明细：开局 %+v / 收尾 %+v", details[0], details[len(details)-1])
	}
}

// 下面这几条用的是云机真跑 app_update 343050 抓下来的原文。
// 之所以盯住"括号里那对字节"而不是行首的百分号：那个百分号是单个 depot 内部的，
// steamcmd 在多 depot 之间会归零重打，界面上的条子就会来回蹿。
var realDepotLines = []struct {
	line  string
	after time.Duration
}{
	{"[  0%] Checking for available updates...", 0},
	{" Update state (0x61) downloading, progress: 0.00 (0 / 4594664662)", 0},
	{" Update state (0x61) downloading, progress: 1.07 (49005024 / 4594664662)", 2 * time.Second},
	{" Update state (0x61) downloading, progress: 9.33 (428750751 / 4594664662)", 4 * time.Second},
}

func TestProgressStateReportsWholePackageBytes(t *testing.T) {
	p := &progressState{}
	now := time.Now()
	var percents []int
	var last string
	for _, item := range realDepotLines {
		percent, message := p.observe(item.line, now.Add(item.after))
		percents = append(percents, percent)
		last = message
	}
	// 49005024/4594664662≈1.07%，428750751/4594664662≈9.33%：条子按整包字节走。
	if percents[1] != 0 || percents[2] != 1 || percents[3] != 9 {
		t.Errorf("整包百分比 = %v，期望 0 → 1 → 9", percents)
	}
	for _, want := range []string{"408.9 MB", "4.3 GB", "实测", "MB/s", "就要下完了"} {
		if !strings.Contains(last, want) {
			t.Errorf("收尾那句缺了 %q：%q", want, last)
		}
	}
	// 英文原文不能出现在界面上那一句里（Checking/downloading 都不许漏）。
	for _, bad := range []string{"Checking", "downloading", "Update state"} {
		if strings.Contains(last, bad) {
			t.Errorf("消息里漏出了 steamcmd 原文 %q：%q", bad, last)
		}
	}
}

func TestProgressStateSpeedComesOnlyFromByteDeltas(t *testing.T) {
	// 第一窗（相邻采样点不足 2 秒）不给速度：这时说"约还需 X"是猜的。
	p := &progressState{}
	_, msg := p.observe(" Update state (0x61) downloading, progress: 1.00 (45946646 / 4594664662)", time.Now())
	if strings.Contains(msg, "MB/s") || strings.Contains(msg, "约还需") {
		t.Errorf("只有一个采样点就报了速度和剩余时间：%q", msg)
	}
	_, msg = p.observe(" Update state (0x61) downloading, progress: 2.00 (91893293 / 4594664662)", time.Now().Add(3*time.Second))
	if !strings.Contains(msg, "14.6 MB/s") {
		t.Errorf("速度不是这两行字节差算出来的：%q", msg)
	}
	// 剩余时间同样是从这两行推的：4594664662-91893293 字节 / 14.6 MB/s ≈ 4 分钟。
	if !strings.Contains(msg, "约还需 4 分钟") {
		t.Errorf("剩余时间没跟着字节差走：%q", msg)
	}
}

func TestProgressStateRestartsSpeedSampleWhenBytesReset(t *testing.T) {
	// 换一个 depot 时已下字节会归零：拿旧采样点一减就是负速度，
	// 界面要么显示乱数要么干脆不动，所以这里丢掉旧窗口重采。
	p := &progressState{}
	now := time.Now()
	before, _ := p.observe(" Update state (0x61) downloading, progress: 60.00 (2756798797 / 4594664662)", now)
	percent, msg := p.observe(" Update state (0x61) downloading, progress: 0.10 (4718592 / 471859200)", now.Add(5*time.Second))
	if p.bytesPerS != 0 {
		t.Errorf("换 depot 后速度该重采，实际 %f", p.bytesPerS)
	}
	if strings.Contains(msg, "MB/s") || strings.Contains(msg, "约还需") {
		t.Errorf("重采的第一行就报出了速度：%q", msg)
	}
	// 整包地板不能被第二个 depot 的小数字压回去。
	if percent != before {
		t.Errorf("百分比 = %d，期望保持在 %d", percent, before)
	}
}

func TestProgressStateCapsAt99UntilTaskFinishes(t *testing.T) {
	p := &progressState{}
	percent, _ := p.observe(" Update state (0x61) downloading, progress: 100.00 (4594664662 / 4594664662)", time.Now())
	if percent != 99 {
		t.Errorf("字节下完了就报 100%%，可 steamcmd 还要校验、还要落 version.txt；实际 %d", percent)
	}
}

func TestProgressStateFallsBackToSteamcmdDecimalWithoutTotal(t *testing.T) {
	// 括号里有字节、却没有总量（progress: 12.34 (45946646 / 0)）：
	// 这时候只能退回 steamcmd 自己算的那个小数，否则条子一动不动。
	// 但剩余时间没有依据，不能编。
	p := &progressState{}
	percent, msg := p.observe(" Update state (0x61) downloading, progress: 12.34 (45946646 / 0)", time.Now())
	if percent != 12 {
		t.Errorf("百分比 = %d，期望按 steamcmd 自己给的小数取整", percent)
	}
	if strings.Contains(msg, "约还需") || strings.Contains(msg, "MB/s") {
		t.Errorf("没有总量却报了进度派生的数字：%q", msg)
	}
	if !strings.Contains(msg, "整包大小") {
		t.Errorf("要说清现在还不知道整包多大：%q", msg)
	}
}

// 云机实测（2026-11-01 21:15）：开局连着一行 `progress: 18.xx (0 / 0)` 和
// `progress: 18.xx (0 / 4594664662)`，随后才是 `progress: 4.60 (211481985 / 4594664662)`。
// 跟着 steamcmd 自己的那列小数走，条子会一上来停在 18%，而括号里的字节只到 201 MB，
// 界面就成了"已下 201 MB / 4.3 GB（18%）"这种前半句和后半句打架的话。
// 规矩收到一条：知道总量以后，百分比只从字节算。
func TestSteamcmdOwnPercentCannotStampedeTheBar(t *testing.T) {
	now := time.Now()
	p := &progressState{}
	if percent, _ := p.observe(" Update state (0x61) downloading, progress: 18.24 (0 / 0)", now); percent != 0 {
		t.Errorf("空读数把条子推到了 %d%%，期望一动不动", percent)
	}
	if percent, _ := p.observe(" Update state (0x61) downloading, progress: 18.24 (0 / 4594664662)", now.Add(time.Second)); percent != 0 {
		t.Errorf("总量已知、字节还是 0 时条子该停在原地，实际推到了 %d%%", percent)
	}
	percent, msg := p.observe(" Update state (0x61) downloading, progress: 25.00 (211481985 / 4594664662)", now.Add(2*time.Second))
	if percent != 4 {
		t.Errorf("201 MB / 4.3 GB 该是 4%%，实际 %d%%", percent)
	}
	if !strings.Contains(msg, "（4%）") || strings.Contains(msg, "18%") {
		t.Errorf("那句话里的百分比要和字节量对得上：%q", msg)
	}
}

func TestProgressStateNamesStagesInsteadOfRawEnglish(t *testing.T) {
	// depot 那几条的原文来自云机 /data/dst/tools/steamcmd/linux32/logs/console_log.txt，
	// 状态词必须逐字对上：表里曾写过 "verifying installation"，真机打的是
	// "verifying install"，差一个后缀就整段落到兜底名。
	cases := map[string]string{
		"[  0%] Checking for available updates...":                                         "检查有没有新版本",
		"[----] Verifying installation...":                                                 "校验安装状态",
		"[ 65%] Downloading package steamapi_bin...":                                       "steamcmd 正在更新自己",
		" Update state (0x3) reconfiguring, progress: 0.00 (0 / 0)":                        "整理安装目录",
		" Update state (0x11) preallocating, progress: 71.03 (3263664236 / 4594664662)":    "预留磁盘空间",
		" Update state (0x5) verifying install, progress: 91.80 (4217707544 / 4594664662)": "校验已下载的文件",
		" Update state (0x81) verifying update, progress: 12.00 (551359759 / 4594664662)":  "校验已下载的文件",
		" Update state (0x101) committing, progress: 50.00 (2297332331 / 4594664662)":      "写入安装目录",
	}
	for line, want := range cases {
		p := &progressState{}
		_, msg := p.observe(line, time.Now())
		if !strings.Contains(msg, want) {
			t.Errorf("%q 应该翻成 %q，实际 %q", line, want, msg)
		}
	}
}

// TestVerifyBytesAreNotCalledDownloaded 钉的是 2026-11-01 真机那次 validate：
// 本体早就装好了，再跑一次 app_update validate，steamcmd 打的是
// `Update state (0x5) verifying install` 那一档，字节量照样从 0 爬到 45 亿，
// 速度能报出 200 MB/s——那是在读磁盘，不是在下网络。
// 这时候界面写"已下 4.2 GB，速度 200 MB/s"就是一句一本正经的假话。
func TestVerifyBytesAreNotCalledDownloaded(t *testing.T) {
	p := &progressState{}
	now := time.Now()
	_, dl := p.observe(" Update state (0x61) downloading, progress: 60.00 (2756798797 / 4594664662)", now)
	if !strings.Contains(dl, "已下 ") {
		t.Errorf("真在下载时该说已下：%q", dl)
	}
	if d := p.buildDetail(); d == nil || !d.Downloading {
		t.Errorf("downloading 档位没标出来：%+v", d)
	}

	percent, msg := p.observe(" Update state (0x5) verifying install, progress: 91.80 (4217707544 / 4594664662)",
		now.Add(3*time.Second))
	// 认的是"已下 "那个带宾语的计数说法，不是"已下"两个字：阶段名"校验已下载的文件"
	// 里也有这两个字，按子串一刀切会把正确的一句话判成错。
	if strings.Contains(msg, "已下 ") {
		t.Errorf("校验阶段还在说已下：%q", msg)
	}
	for _, want := range []string{"校验已下载的文件", "已处理"} {
		if !strings.Contains(msg, want) {
			t.Errorf("校验那一句缺了 %q：%q", want, msg)
		}
	}
	if percent < 60 {
		t.Errorf("地板掉了：%d < 60（同一次任务里条子不许倒退）", percent)
	}
	d := p.buildDetail()
	if d == nil || d.Downloading {
		t.Errorf("校验阶段不该标 downloading：%+v", d)
	}
	if d.Phase != "校验已下载的文件" {
		t.Errorf("阶段名 = %q", d.Phase)
	}
}

// 云机实测：校验跑到 96% 时 steamcmd 夹了一行空读数的状态打印
// `Update state (0x0) unknown, progress: 0.00 (0 / 0)`，界面上的"已处理"跟着退回 0，
// 看着像整个任务重跑了一次。这一行两个数都是空的，它对进度一无所知，
// 所以不能拿它覆盖已经读到的字节量。
func TestEmptyUnknownLineKeepsLastByteReading(t *testing.T) {
	p := &progressState{}
	now := time.Now()
	percentBefore, _ := p.observe(" Update state (0x61) downloading, progress: 96.00 (4410878075 / 4594664662)", now)
	p.observe(" Update state (0x61) downloading, progress: 96.00 (4410878075 / 4594664662)", now.Add(3*time.Second))
	speed := p.bytesPerS
	percent, msg := p.observe(" Update state (0x0) unknown, progress: 0.00 (0 / 0)", now.Add(6*time.Second))
	if strings.Contains(msg, "0 B /") {
		t.Errorf("空读数把已下量清零了：%q", msg)
	}
	if p.done != 4410878075 {
		t.Errorf("已下字节 = %d，期望保持上一次的真读数", p.done)
	}
	if p.bytesPerS != speed {
		t.Errorf("速度窗口被一次空读数白白重置：%f != %f", p.bytesPerS, speed)
	}
	if percent != percentBefore {
		t.Errorf("百分比 = %d，期望保持在 %d", percent, percentBefore)
	}
	// 阶段认不出来就退回中性说法：继续挂着"下载服务端本体文件"是假装知道它在干什么。
	if d := p.buildDetail(); d == nil || d.Phase != "正在处理本体文件" || d.DoneBytes != 4410878075 {
		t.Errorf("明细 = %+v", d)
	}
	// 带真实总量的 (0 / 总量) 是换 depot 重新起表，总量本身换了，那一条不能被当成空读数吞掉。
	if _, msg := p.observe(" Update state (0x61) downloading, progress: 0.10 (4718592 / 471859200)", now.Add(9*time.Second)); !strings.Contains(msg, "450.0 MB") {
		t.Errorf("换 depot 时没有接受新的整包总量：%q", msg)
	}
}

// 这条是"不能依靠某一次的速度"的落点：整包字节数会随构建变，
// 所以下载相关的文案里不许出现写死的大小或耗时。
func TestDownloadCopyAssumesNoSizeOrSpeed(t *testing.T) {
	spec, _ := KindSpec(KindServerUpdate)
	f := newWorkerFixture(t, nil, nil)
	_, _, messages := collectReports(t, f.w)
	all := append([]string{spec.Desc, spec.Label}, messages...)
	for _, s := range all {
		for _, bad := range []string{"4.6G", "4594664662", "十几分钟", "248MB"} {
			if strings.Contains(s, bad) {
				t.Errorf("文案里写死了 %q：%q", bad, s)
			}
		}
	}
}

func TestServerUpdateRefusesWhileShardsRun(t *testing.T) {
	f := newWorkerFixture(t, nil, nil)
	// 复用 procrun 的假 Runner：会话表里有一个 Master 在跑。
	f.w.Procs = procrun.New(f.w.Paths, listingRunner{out: "  20314.dst-Cluster_1-Master\t(Detached)\n"})

	err, _, _ := collectReports(t, f.w)
	if err == nil {
		t.Fatal("有分片在跑还更新了本体，bin64 会被换掉")
	}
	if !strings.Contains(err.Error(), "还有世界在运行") || !strings.Contains(err.Error(), "dst-Cluster_1-Master") {
		t.Errorf("错误要指出是哪个会话：%v", err)
	}
	if f.lines.calls != 0 {
		t.Errorf("拦住之前就已经执行 steamcmd：%v", f.lines.args)
	}
}

type listingRunner struct{ out string }

func (r listingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return r.out, nil
}

func TestServerUpdateChecksSteamCmdBeforeEnqueuingWork(t *testing.T) {
	t.Run("没配", func(t *testing.T) {
		f := newWorkerFixture(t, nil, nil)
		f.w.SteamCmd = ""
		err, _, _ := collectReports(t, f.w)
		if err == nil || !strings.Contains(err.Error(), "paths.steamCmd") {
			t.Errorf("要告诉用户改配置的哪个键：%v", err)
		}
		if f.lines.calls != 0 {
			t.Error("没配路径也去执行了命令")
		}
	})

	t.Run("文件不在", func(t *testing.T) {
		f := newWorkerFixture(t, nil, nil)
		f.w.SteamCmd = filepath.Join(t.TempDir(), "nope", "steamcmd.sh")
		err, _, _ := collectReports(t, f.w)
		if err == nil || !strings.Contains(err.Error(), "找不到 steamcmd") {
			t.Errorf("要说清找不到什么：%v", err)
		}
		if strings.Contains(fmt.Sprint(err), "no such file") {
			t.Errorf("把系统调用英文原文留在了结论里：%v", err)
		}
	})
}

func TestServerUpdateKeepsLastOutputWhenFailing(t *testing.T) {
	f := newWorkerFixture(t, []string{
		"Logging session up...",
		"ERROR! Failed to install app '343050' (Disk budget is too low!)",
	}, fmt.Errorf("执行 steamcmd: exit status 7"))

	err, _, _ := collectReports(t, f.w)
	if err == nil {
		t.Fatal("非零退出必须算失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Disk budget is too low") {
		t.Errorf("失败结论里要带最后一行现场输出：%q", msg)
	}
	if !strings.Contains(msg, "exit status 7") {
		t.Errorf("丢了退出码：%q", msg)
	}
}

func TestServerUpdateExplainsMissingBinaryRatherThanCrashing(t *testing.T) {
	f := newWorkerFixture(t, nil, fmt.Errorf("%w steamcmd.sh: exec: not found", procrun.ErrCommandMissing))
	err, _, _ := collectReports(t, f.w)
	if err == nil || !strings.Contains(err.Error(), "执行不了 steamcmd") {
		t.Errorf("命令缺失要说成人话：%v", err)
	}
}

func TestRunOnceWritesTerminalStateBackToTask(t *testing.T) {
	f := newWorkerFixture(t, []string{"Downloading ( 50%) x / y KB"}, nil)
	id, err := f.st.CreateTask(KindServerUpdate, "更新服务端本体", nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if !f.w.runOnce(context.Background()) {
		t.Fatal("有任务却没干活")
	}
	task, err := f.st.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskSucceeded {
		t.Errorf("状态 = %s，期望已完成", task.Status)
	}
	if task.Progress != 100 || !strings.Contains(task.Message, "可以启动世界") {
		t.Errorf("收尾信息不对：progress=%d message=%q", task.Progress, task.Message)
	}
	if task.Attempts != 1 {
		t.Errorf("尝试次数 = %d，期望 1", task.Attempts)
	}

	// 队列空了要说"没活"，Run 的循环靠这个决定要不要睡。
	if f.w.runOnce(context.Background()) {
		t.Error("队列空了还报告干了活")
	}
	if f.lines.calls != 1 {
		t.Errorf("空队列也去执行命令：%d 次", f.lines.calls)
	}
}

func TestRunOnceFailsUnknownKindWithoutTouchingProcess(t *testing.T) {
	f := newWorkerFixture(t, nil, nil)
	id, err := f.st.CreateTask("cluster.delete", "删掉整个集群", nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !f.w.runOnce(context.Background()) {
		t.Fatal("该领到这条任务")
	}
	task, err := f.st.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskFailed {
		t.Errorf("状态 = %s，期望失败", task.Status)
	}
	if !strings.Contains(task.Message, "不认识任务类型") || !strings.Contains(task.Message, "cluster.delete") {
		t.Errorf("要说清是哪条类型不认识：%q", task.Message)
	}
	if f.lines.calls != 0 {
		t.Errorf("没注册的 kind 居然还是执行了命令：%v", f.lines.args)
	}
}

func TestRunOnceMarksFailedWhenPanelShutsDown(t *testing.T) {
	f := newWorkerFixture(t, []string{"Downloading ( 20%) x / y KB"}, nil)
	// 已经收到退出信号时不该再接新任务。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if f.w.runOnce(cancelled) {
		t.Fatal("面板在退出还是领了一条任务")
	}

	ctx, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	// 跑到一半才退出：子进程被 kill，这条任务必须留成"被中断"而不是"执行中"。
	f.lines.onFinish = func() {
		cancel2()
		f.lines.err = fmt.Errorf("执行 steamcmd: signal: killed")
	}
	id, _ := f.st.CreateTask(KindServerUpdate, "更新服务端本体", nil)
	if !f.w.runOnce(ctx) {
		t.Fatal("该领到这条任务")
	}
	task, _ := f.st.GetTask(id)
	if task.Status != store.TaskFailed {
		t.Errorf("状态 = %s，期望标成失败", task.Status)
	}
	if !strings.Contains(task.Message, "面板退出") {
		t.Errorf("消息要说明是被中断而不是普通失败：%q", task.Message)
	}
}

// 慢管道机器上"跑满上限"是常态而不是意外：云机实测到 steam 只有三十多 KB/s，
// 4.3 GB 的整包在一小时内根本下不完。这句话会原样出现在任务中心，
// 所以断言的是"读得懂"：既不能有 Go 的时长字面量，也不能有 context deadline exceeded 这种裸英文。
func TestTaskDeadlineExplainsItselfInChinese(t *testing.T) {
	f := newWorkerFixture(t, []string{"Downloading ( 20%) x / y KB"}, nil)
	f.w.LongTimeout = 30 * time.Millisecond
	f.lines.onFinish = func() {
		time.Sleep(60 * time.Millisecond)
		f.lines.err = fmt.Errorf("执行 steamcmd.sh +app_update 343050 validate: signal: killed")
	}
	id, _ := f.st.CreateTask(KindServerUpdate, "下载服务端本体", nil)
	if !f.w.runOnce(context.Background()) {
		t.Fatal("该领到这条任务")
	}
	task, err := f.st.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskFailed {
		t.Errorf("状态 = %s，期望标成失败", task.Status)
	}
	for _, bad := range []string{"context deadline exceeded", "1h0m0s", "signal: killed", "steamcmd.sh"} {
		if strings.Contains(task.Message, bad) {
			t.Errorf("消息里漏了实现细节 %q：%q", bad, task.Message)
		}
	}
	if !strings.Contains(task.Message, "硬上限") || !strings.Contains(task.Message, "30 毫秒") {
		t.Errorf("要说清是跑满硬上限、上限是多少：%q", task.Message)
	}
	if !strings.Contains(task.Message, "timeoutMinutes") {
		t.Errorf("要给下一步（配置里哪一项能放宽）：%q", task.Message)
	}
}

// quietLines 是"进程还挂着，但一个字都不再吐"的形态。
// 云机实测过：steamcmd 自更新卡在同一个数字上反复打印，那十几分钟界面上的条子一动不动。
type quietLines struct {
	emitted []string
}

func (q *quietLines) RunLines(ctx context.Context, _ string, _ []string, onLine func(string)) error {
	for _, line := range q.emitted {
		onLine(line)
	}
	<-ctx.Done()
	return fmt.Errorf("执行 steamcmd: %w", ctx.Err())
}

// tickingLines 每 step 打一行内容都不同的输出，打满 count 行后正常收工。
// 用来验"判停不会误杀真在动的下载"：总时长明明超过窗口，但每一窗都有新内容。
type tickingLines struct {
	step  time.Duration
	count int
}

func (t *tickingLines) RunLines(ctx context.Context, _ string, _ []string, onLine func(string)) error {
	for i := 1; i <= t.count; i++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("执行 steamcmd: %w", ctx.Err())
		case <-time.After(t.step):
		}
		onLine(fmt.Sprintf(" Update state (0x61) downloading, progress: %.2f (%d / 4594664662)",
			float64(i)*6, int64(i)*10000000))
	}
	return nil
}

func TestStallInterruptsASilentSteamcmd(t *testing.T) {
	f := newWorkerFixture(t, nil, nil)
	f.w.Stall = 80 * time.Millisecond
	f.w.LongTimeout = time.Hour // 让硬上限不掺和，只看判停这一条
	f.w.Lines = &quietLines{emitted: []string{
		" Update state (0x61) downloading, progress: 1.00 (45946646 / 4594664662)",
	}}
	id, _ := f.st.CreateTask(KindServerUpdate, "下载服务端本体", nil)
	if !f.w.runOnce(context.Background()) {
		t.Fatal("该领到这条任务")
	}
	task, err := f.st.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskFailed {
		t.Fatalf("状态 = %s，期望判停后标成失败", task.Status)
	}
	for _, bad := range []string{"context canceled", "signal", "steamcmd.sh", "0x61"} {
		if strings.Contains(task.Message, bad) {
			t.Errorf("消息里漏了实现细节 %q：%q", bad, task.Message)
		}
	}
	if !strings.Contains(task.Message, "卡死") || !strings.Contains(task.Message, "80 毫秒") {
		t.Errorf("要说清是判停、窗口多长：%q", task.Message)
	}
}

func TestStallDoesNotKillAStreamThatKeepsMoving(t *testing.T) {
	// 判停的窗口比整条任务的时长还短，但输出一直在推进——这种必须成功。
	// 误杀一条正在慢速下载的线，比漏杀一条卡死的线更糟：用户看到的是"下不完"。
	f := newWorkerFixture(t, nil, nil)
	f.w.Stall = 100 * time.Millisecond
	f.w.LongTimeout = time.Hour
	f.w.Lines = &tickingLines{step: 20 * time.Millisecond, count: 15}
	id, _ := f.st.CreateTask(KindServerUpdate, "下载服务端本体", nil)
	if !f.w.runOnce(context.Background()) {
		t.Fatal("该领到这条任务")
	}
	task, err := f.st.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskSucceeded {
		t.Errorf("状态 = %s，消息 %q：一直在出内容的线不该被判停", task.Status, task.Message)
	}
}

// humanTimeout 只服务于一句界面文案，边界值一旦退回 Go 字面量就是往界面上漏内部格式。
func TestHumanTimeoutWords(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Millisecond, "30 毫秒"},
		{45 * time.Second, "45 秒"},
		{90 * time.Second, "1 分 30 秒"},
		{10 * time.Minute, "10 分钟"},
		{60 * time.Minute, "1 小时"},
		{90 * time.Minute, "1 小时 30 分钟"},
		{5 * time.Hour, "5 小时"},
	}
	for _, c := range cases {
		if got := humanTimeout(c.d); got != c.want {
			t.Errorf("%s 说成 %q，期望 %q", c.d, got, c.want)
		}
	}
}

func TestClipKeepsLongOutputInsideMessageColumn(t *testing.T) {
	long := strings.Repeat("下载中", 300)
	got := clip(long)
	if n := len([]rune(got)); n > 202 {
		t.Errorf("裁剪后还有 %d 个字符", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("被裁掉却没有省略号，用户会以为输出本来就这样")
	}
	if clip("  短句子  ") != "短句子" {
		t.Error("没顺手把首尾空白去掉")
	}
}

// 恢复结论那句的重点一直在后半段（哪些配置没被覆盖、要不要重启），
// 而它以前被按"外部命令一行输出"的 200 字尺子截断过——界面显示的就是半句话。
func TestClipMessageKeepsWholeTaskConclusion(t *testing.T) {
	long := "完成：已恢复 22 个文件并逐个比对哈希通过；存档目录被整棵换新的分片：Caves、Master；" +
		"恢复前的安全备份：20261031T072117Z-Cluster_1-before-restore；世界已回到快照时刻：22 个文件写出并逐个比对哈希通过；" +
		"房间配置保持现状没有覆盖（Caves/server.ini、Master/server.ini）——端口和服务器名不变，要照快照那套就手工改；" +
		"分片是停着的，启动后才有人能进"
	if n := len([]rune(long)); n <= 200 {
		t.Fatalf("这条夹具才 %d 字，测不到长结论", n)
	}
	if got := clipMessage(long); got != strings.TrimSpace(long) {
		t.Errorf("日常这么长的结论整条都要留住，实际 %q", got)
	}

	// 真装不下时退到最后一条分号，不留半个结论。
	huge := "已让 Master、Caves 立即存盘后再打包；" + strings.Repeat("拖", 700)
	got := clipMessage(huge)
	if !strings.HasSuffix(got, "…") {
		t.Error("被裁掉却没有省略号，用户会以为结论本来就这样")
	}
	if strings.Contains(got, "拖") {
		t.Errorf("要在分号处收，别把后面那串留成半句：%q", got[len(got)-24:])
	}
	if !strings.HasPrefix(got, "已让 Master、Caves 立即存盘后再打包") {
		t.Errorf("收得太狠，前面那句结论都没了：%q", got)
	}
}

func TestKindRegistryIsChineseLabelledAndSorted(t *testing.T) {
	names := KindNames()
	if len(names) == 0 {
		t.Fatal("没有可提交的任务类型")
	}
	for _, n := range names {
		spec, ok := KindSpec(n)
		if !ok {
			t.Fatalf("%s 在名单里却取不到定义", n)
		}
		if spec.Name != n || spec.Label == "" || spec.Label == n || spec.Desc == "" {
			t.Errorf("%s 没配上中文标签或说明：%+v", n, spec)
		}
	}
	if _, ok := KindSpec("rm -rf"); ok {
		t.Error("任意字符串成了可提交的任务类型")
	}
}
