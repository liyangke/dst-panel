package dst

import (
	"strings"
	"testing"
)

// 模板里的 %s 个数必须和 Args 一致，否则加参数时改一处漏一处，
// 后果是把 %s 原样注入命令行——这是最不该由运行时才发现的错。
func TestConsolePatternsMatchArgs(t *testing.T) {
	for _, key := range ConsoleCommandKeys() {
		cmd, ok := ConsoleCommandSpec(key)
		if !ok {
			t.Fatalf("%s 在 key 列表里却查不到", key)
		}
		if cmd.Key != key {
			t.Errorf("命令 key 与 map 键不一致: %s / %s", cmd.Key, key)
		}
		if got := strings.Count(cmd.Pattern, "%s"); got != len(cmd.Args) {
			t.Errorf("%s: 模板有 %d 个占位符但声明了 %d 个参数: %s", key, got, len(cmd.Args), cmd.Pattern)
		}
		if strings.Count(cmd.Pattern, "%") != strings.Count(cmd.Pattern, "%s") {
			t.Errorf("%s: 模板里有 %%s 之外的百分号写法: %s", key, cmd.Pattern)
		}
		if !strings.HasPrefix(cmd.Pattern, "c_") {
			t.Errorf("%s: 模板不是 c_ 开头的控制台命令: %s", key, cmd.Pattern)
		}
		if cmd.Label == "" || cmd.Desc == "" {
			t.Errorf("%s: 界面文案没配齐", key)
		}
		for _, a := range cmd.Args {
			if a.Label == "" {
				t.Errorf("%s: 参数 %s 没有中文标签", key, a.Name)
			}
			switch a.Kind {
			case "int":
				if a.Min > a.Max {
					t.Errorf("%s: 取值区间反了", key)
				}
			case "enum":
				if len(a.Choices) == 0 {
					t.Errorf("%s: 枚举没有候选值", key)
				}
				for _, c := range a.Choices {
					if c.Label == "" || c.Value == "" {
						t.Errorf("%s: 枚举项缺 value 或中文标签", key)
					}
				}
			case "string":
				if a.MaxLen <= 0 {
					t.Errorf("%s: 字符串参数没限长", key)
				}
			default:
				t.Errorf("%s: 未知参数类型 %q", key, a.Kind)
			}
		}
	}
}

func TestBuildConsoleLineDefaults(t *testing.T) {
	cases := map[string]string{
		"save":             "c_save()",
		"rollback":         "c_rollback(1)",
		"reset_rollback":   "c_reset()",
		"regenerate_shard": "c_regenerateshard()",
		"auto_teleport":    "c_autoteleportplayers()",
		"force_phase":      `c_simphase("day")`,
	}
	for key, want := range cases {
		got, err := BuildConsoleLine(key, nil)
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if got != want {
			t.Errorf("%s: 期望 %q 实际 %q", key, want, got)
		}
	}
}

func TestBuildConsoleLineInjectsNoLua(t *testing.T) {
	// 公告内容是最容易被当成拼接漏洞的地方：这些串都想提前结束字面量再补一条命令。
	attacks := []string{
		`x"); c_shutdown(); ("`,
		`");c_regenerateshard()--`,
		`\\"`,
		"c_announce('y')",
		"\"..\"a\"",
	}
	for _, attack := range attacks {
		line, err := BuildConsoleLine("announce", map[string]string{"message": attack})
		if err != nil {
			t.Fatalf("announce(%q): %v", attack, err)
		}
		// 攻击文本本来就可能含 "c_shutdown" 这类字样，光数子串没意义；
		// 真正的判据是它只能待在字面量内部：整行只有首尾两个未转义引号。
		if n := unescapedQuoteCount(line); n != 2 {
			t.Errorf("字面量外的引号不该只有两个，实际 %d: %s", n, line)
		}
		if !strings.HasPrefix(line, "c_announce(\"") || !strings.HasSuffix(line, "\")") {
			t.Errorf("命令形态不对: %s", line)
		}
		// 拿面板自己的 Lua 数据解析器验一遍：能解析回同一个字符串才算合法字面量。
		parsed, err := ParseLuaTable("return { s = " + line[len("c_announce("):len(line)-1] + " }")
		if err != nil {
			t.Errorf("生成的字面量本地都解析不了: %v (%s)", err, line)
			continue
		}
		if got := parsed.Get("s").AsString(""); got != attack {
			t.Errorf("转义丢了原文: 期望 %q 实际 %q", attack, got)
		}
	}
}

// unescapedQuoteCount 数没被反斜杠转义的双引号。
func unescapedQuoteCount(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			n++
		}
	}
	return n
}

func TestBuildConsoleLineValidatesArgs(t *testing.T) {
	if _, err := BuildConsoleLine("rollback", map[string]string{"count": "0"}); err == nil {
		t.Error("回退 0 次应该拒绝")
	}
	if _, err := BuildConsoleLine("rollback", map[string]string{"count": "999"}); err == nil {
		t.Error("超出区间应该拒绝")
	}
	if _, err := BuildConsoleLine("rollback", map[string]string{"count": "1) c_shutdown()"}); err == nil {
		t.Error("非整数应该拒绝")
	}
	got, err := BuildConsoleLine("rollback", map[string]string{"count": " 3 "})
	if err != nil || got != "c_rollback(3)" {
		t.Errorf("两侧空格应被容忍: %q %v", got, err)
	}
	if _, err := BuildConsoleLine("announce", map[string]string{"message": ""}); err == nil {
		t.Error("空公告应该拒绝")
	}
	if _, err := BuildConsoleLine("announce", map[string]string{"message": strings.Repeat("饥", 201)}); err == nil {
		t.Error("超长公告应该拒绝")
	}
	if _, err := BuildConsoleLine("announce", map[string]string{"message": "第一行\n第二行"}); err == nil {
		t.Error("含换行应该拒绝")
	}
	// 枚举只认候选值，报错信息给用户看的是中文标签而不是码值。
	if _, err := BuildConsoleLine("force_phase", map[string]string{"phase": "midnight"}); err == nil {
		t.Error("非法时段应该拒绝")
	} else if !strings.Contains(err.Error(), "白天") {
		t.Errorf("报错应列出中文候选: %v", err)
	}
	// 换行绝不允许出现在生成结果里，否则一次注入变成多行脚本。
	if _, err := BuildConsoleLine("不存在", nil); err == nil {
		t.Error("未知命令应该拒绝")
	}
}

func TestConsoleCommandKeysAreStableAndSafeLast(t *testing.T) {
	keys := ConsoleCommandKeys()
	if len(keys) != len(consoleCommands) {
		t.Fatalf("key 数量对不上: %d vs %d", len(keys), len(consoleCommands))
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("重复的 key %s", k)
		}
		seen[k] = true
	}
	firstDanger := -1
	for i, k := range keys {
		if consoleCommands[k].Danger && firstDanger < 0 {
			firstDanger = i
		}
	}
	for i := firstDanger; i < len(keys); i++ {
		if !consoleCommands[keys[i]].Danger {
			t.Error("非危险命令排在了危险命令后面，误触概率会上升")
		}
	}
}
