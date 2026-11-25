// fake_steamcmd 是本地实测用的 steamcmd 替身，两处用它：
//   - .tools/api_probe.py：验"提交任务 → worker 逐行读输出 → 进度入库 → 界面读到 100%"，
//     这里要的是真子进程（单测的 fakeLines 验不了 main 有没有把 worker 起起来）。
//   - .tools/ui_dev.py：界面上那条进度条只有在任务"正在跑"的时候才看得见。
//
// 输出的每一行都是云机真跑 app_update 343050 抓下来的形状，连 total 也是真的：
// 面板要是认错了行（比如把单个文件的百分比当整包），这里一样会露馅。
// 什么都不下载、什么都不写盘，退出码 0。
//
// 节奏用环境变量调，不改命令行参数（面板拼给 steamcmd 的参数是写死的）：
//
//	DST_FAKE_GAP   每行之间隔多久，默认 350ms（探针要快）；界面实测给 2200ms 才看得出条子在动
//	DST_FAKE_STEPS 下载行数，默认 8
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	fmt.Println("Redirecting stderr to 'fakesteam/logs/stderr.txt'")
	args := strings.Join(os.Args[1:], " ")
	// 参数是面板写死的：形状不对就说明 worker 拼错了，直接失败让它现形。
	if !strings.Contains(args, "+app_update 343050 validate") ||
		!strings.Contains(args, "+force_install_dir") ||
		!strings.Contains(args, "+@sSteamCmdForcePlatformType linux") {
		fmt.Fprintln(os.Stderr, "fake_steamcmd 收到的参数不是预期形状:", args)
		os.Exit(1)
	}
	gap := 350 * time.Millisecond
	if v := os.Getenv("DST_FAKE_GAP"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			gap = d
		}
	}
	steps := 8
	if n, err := strconv.Atoi(os.Getenv("DST_FAKE_STEPS")); err == nil && n > 0 {
		steps = n
	}

	fmt.Println("\"@sSteamCmdForcePlatformType\" = \"linux\"")
	fmt.Println("Loading Steam API...OK")
	fmt.Println("Connecting anonymously to Steam Public...OK")
	fmt.Println("[  0%] Checking for available updates...")
	fmt.Println("[----] Verifying installation...")
	// 这一段是 steamcmd 给自己做自更新（云机实测要拉两百多兆，慢机器上要几分钟）。
	// 它的百分比和本体一个字节的关系都没有，正是 2026-11-01 那条假进度条的来源，
	// 所以替身必须把它演出来：不演，界面实测就看不见这个分支。
	for _, n := range []int{12, 37, 68, 92} {
		fmt.Printf("[%3d%%] Downloading update...\n", n)
		fmt.Printf("[%3d%%] Downloading package steamcmd_bins_linux...\n", n)
		time.Sleep(gap)
	}
	fmt.Println("[----] Unpacking package steamcmd_bins_linux...")
	fmt.Println("Update state (0x3) reconfiguring, progress: 0.00 (0 / 0)")
	fmt.Println("Downloading update for app 343050 (0 MB)")

	// 这个总量是 2026-11-01 云机实测 app 343050 的 linux depot 整包字节数。
	const total = 4594664662
	per := total / int64(steps)
	for i := 1; i <= steps; i++ {
		done := per * int64(i)
		fmt.Printf(" Update state (0x61) downloading, progress: %.2f (%d / %d)\n",
			float64(done)/float64(total)*100, done, total)
		if i < steps {
			time.Sleep(gap)
		}
	}
	// 下完之后 steamcmd 还要把整包校验一遍，用的是同一套字节量、同样的行形状，
	// 只有状态词不同（云机原文：Update state (0x5) verifying install）。
	// 这一段必须演：面板要是把它当成"又在下"，界面就会报出几百 MB/s 的假下载速度。
	for i := 1; i <= 4; i++ {
		done := int64(float64(total) * (0.1 + 0.2*float64(i)))
		fmt.Printf(" Update state (0x5) verifying install, progress: %.2f (%d / %d)\n",
			float64(done)/float64(total)*100, done, total)
		time.Sleep(gap)
	}
	fmt.Println("Success! App '343050' fully installed.")
}
