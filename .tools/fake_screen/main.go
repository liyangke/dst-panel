// 只在开发机上用的 screen 替身，不进成品、不进部署包。
//
// 为什么需要它：面板的进程状态全部来自 `screen -ls`，而这台 Windows 上没有 screen，
// 于是本地跑起来时总览页只能显示"问不到进程状态"那一条分支，
// 界面里最要紧的形态（在跑 / 没在跑 / 助手没回话 / 启停按钮切换）一个都看不到。
// 这个替身只模仿面板会调的那几条命令，会话状态存在一个 JSON 文件里，
// 让浏览器实测能把整条启停回路点完。
//
// 它不是测试桩：真的分片行为（落盘顺序、会话留住与否）仍然只在云机上验。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type session struct {
	Name string `json:"name"`
	Pid  int    `json:"pid"`
}

func statePath() string {
	if p := os.Getenv("DST_FAKE_SCREEN"); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), "dstpanel_fake_screen.json")
}

func load() []session {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return nil
	}
	var s []session
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return s
}

func save(s []session) {
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(statePath(), b, 0o600)
}

func logLine(text string) {
	f, err := os.OpenFile(statePath()+".console", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n", text)
}

// find 返回 name 在 s 里的下标，没有则 -1。
func find(s []session, name string) int {
	for i, x := range s {
		if x.Name == name {
			return i
		}
	}
	return -1
}

func main() {
	args := os.Args[1:]
	// 面板只会用到这几条：-ls、-dmS <name> bash -c <cmd>、-S <name> -X quit|stuff。
	// 其余一律按"成功但什么都不做"处理，宁可让替身显得宽容，也不要让界面
	// 因为一条没模仿到的参数报错而去猜是不是面板写错了。
	target := ""
	i := 0
	for i < len(args) {
		a := args[i]
		switch a {
		case "-ls":
			list(load())
			return
		case "-dmS", "-S":
			if i+1 < len(args) {
				if a == "-dmS" {
					name := args[i+1]
					addSession(name)
					return
				}
				target = args[i+1]
			}
			i++
		case "-X":
			rest := strings.Join(args[min(i+1, len(args)):], " ")
			if target != "" {
				if strings.HasPrefix(rest, "quit") {
					drop(target)
					return
				}
				if strings.Contains(rest, "stuff") {
					payload := rest[strings.Index(rest, "stuff")+len("stuff"):]
					logLine(target + " <= " + strings.TrimSpace(payload))
					// 真的 screen 收到 c_shutdown 之后，引擎落完盘会自己退出、会话消失。
					// 替身不模拟"落盘要几秒"，直接让会话没掉：界面上停止按钮的效果才对得上。
					if strings.Contains(payload, "c_shutdown") {
						drop(target)
					}
					return
				}
			}
			return
		case "-p":
			i++ // 窗口号，忽略
		default:
			// bash -c 那一整串是启动命令，替身不执行任何东西。
			if strings.HasPrefix(a, "dst-") && target == "" && find(load(), a) < 0 {
				addSession(a)
				return
			}
		}
		i++
	}
}

func addSession(name string) {
	s := load()
	if find(s, name) >= 0 {
		return
	}
	next := 90000
	for _, x := range s {
		if x.Pid >= next {
			next = x.Pid + 1
		}
	}
	s = append(s, session{Name: name, Pid: next})
	save(s)
}

func drop(name string) {
	s := load()
	if i := find(s, name); i >= 0 {
		save(append(s[:i], s[i+1:]...))
	}
}

func list(s []session) {
	if len(s) == 0 {
		// 真 screen 在没有会话时退出码是 3，面板把这一种当成"正常地问过了、答案是空"
		// （见 internal/procrun/exec.go 的 isNotFoundExit）。替身必须照样退出 3，
		// 否则本地测的永远是"有会话"那一路。
		fmt.Println("No Sockets found.")
		os.Exit(3)
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Name < s[j].Name })
	fmt.Println("There are screens on:")
	for _, x := range s {
		fmt.Printf("\t%d.%s\t(Attached)\n", x.Pid, x.Name)
	}
	fmt.Printf("%d Sockets in /run/screen/S-developer.\n", len(s))
}
