package dst

import (
	"strings"
	"testing"
)

// 面板生成的 Lua 必须能被 Lua 5.1 解析。这里用我们自己的数据解析器当替身，
// 它能过的形状就是 Klei 那版 Lua 认得的形状（数字转义、\a\b\f\v 都实现了）。
func TestLuaStringLiteralRoundTrip(t *testing.T) {
	cases := []string{
		"Show Me",
		"永不结束的饥荒沙盒模式",
		`带"引号` + `和\反斜杠`,
		"换行\n制表\t回车\r",
		"响铃\a退格\b换页\f纵向\v",
		"控制字节\x01\x02\x1f",
		"删除键\x7f",
		`workshop-1158324974`,
		`"]; c_shutdown(); [//`,
		"尾随反斜杠\\",
		"",
	}
	for _, s := range cases {
		lit := LuaStringLiteral(s)
		tbl, err := ParseLuaTable("return { v = " + lit + " }")
		if err != nil {
			t.Errorf("解析失败 %q -> %s: %v", s, lit, err)
			continue
		}
		if got := tbl.Get("v").AsString("<nil>"); got != s {
			t.Errorf("往返不一致: 原 %q 字面量 %s 解回 %q", s, lit, got)
		}
	}
}

// 生成的字面量里不能出现 \xNN / \uXXXX：DST 内嵌的 Lua 是 5.1，不认这两种写法。
func TestLuaStringLiteralAvoidsLua53Escapes(t *testing.T) {
	for _, s := range []string{"\x01\x02\x1f", "\x7f", "tab\there"} {
		lit := LuaStringLiteral(s)
		if strings.Contains(lit, `\x`) {
			t.Errorf("产出了 Lua 5.1 不认的十六进制转义: %s", lit)
		}
		if strings.ContainsAny(lit, "\n\r\t") {
			t.Errorf("字面量里出现了裸换行，会把一行命令拆成多行: %q", lit)
		}
	}
	if got := LuaStringLiteral("\x01"); got != `"\001"` {
		t.Errorf("控制字符该写成三位十进制转义: %s", got)
	}
}

func TestFormatKeyEscapesBackslash(t *testing.T) {
	// 反斜杠不转义的话，键尾的 \ 会把收尾引号吃掉，整份文件语法错。
	for _, k := range []string{`a\b`, `workshop-1`, `name"q`, `a\`, `中 文`} {
		line := "return { " + formatKey(k) + " = true }"
		tbl, err := ParseLuaTable(line)
		if err != nil {
			t.Errorf("%q 渲染成 %s 解析不了: %v", k, line, err)
			continue
		}
		if !tbl.Has(k) {
			t.Errorf("键 %q 往返后变成别的键: %+v", k, tbl.Keys())
		}
	}
	if got := formatKey("plain_key"); got != "plain_key" {
		t.Errorf("合法标识符不该被加引号: %s", got)
	}
}
