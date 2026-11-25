package dst

import (
	"strings"
	"testing"
)

// 真实 modinfo.lua 的形状：顶层一串裸赋值，值可以是长字符串、local 变量引用、函数。
// 这一整组测试钉的就是"面板能读这种文件，并且不去执行它"。
const helperStyleModinfo = `-- DST 面板助手：把服务端内部状态导出成文件，供管理面板读取。
version = "1.0.0"
api_version = 10
name = "DST 管理面板助手"
author = "dstpanel"
all_clients_require_mod = false
client_only_mod = false

description = [[
为自建服务器管理面板提供结构化运行数据：
世界天数/季节、玩家生命饥饿理智。
]]

local POLL_CHOICES =
{
    {description = "每 1 秒", data = 1},
    {description = "每 5 秒", data = 5},
}

settings =
{
    {
        name = "poll_interval",
        label = "数据回写间隔",
        values = POLL_CHOICES,
        default = 1,
    },
    {
        name = "player_detail",
        typing = "bool",
        default = true,
    },
}
`

func TestParseLuaAssignmentsReadsBareForm(t *testing.T) {
	tbl, err := ParseLuaAssignments(helperStyleModinfo)
	if err != nil {
		t.Fatalf("自家助手的 modinfo 必须读得下来: %v", err)
	}
	if got := tbl.Get("name").AsString(""); got != "DST 管理面板助手" {
		t.Errorf("name 读错了: %q", got)
	}
	if got := tbl.Get("version").AsString(""); got != "1.0.0" {
		t.Errorf("version 读错了: %q", got)
	}
	if api := tbl.Get("api_version").AsNumber(0); api != 10 {
		t.Errorf("api_version 应该是 10，实际 %v", api)
	}
	if tbl.Get("all_clients_require_mod").AsBool(true) {
		t.Error("all_clients_require_mod = false 被读成 true")
	}
	// 长字符串：紧跟开括号的换行不计入内容，正文里的中文和斜杠原样留着。
	desc := tbl.Get("description").AsString("")
	if !strings.HasPrefix(desc, "为自建服务器") || !strings.Contains(desc, "世界天数/季节") {
		t.Errorf("description 长字符串没读对: %q", desc)
	}
	// settings 是一张数组表，里面 values = POLL_CHOICES 是变量引用：
	// 这个名字到表的对应关系就在同一个文件里、由上往下已经读到了，所以接住它
	// 是把已有的数据用起来，不是求值（引擎那边同样是把那张表的引用放进 env）。
	item, ok := tbl.Nested("settings")
	if !ok || item.Len() != 2 {
		t.Fatalf("settings 应该读到 2 项，实际 %+v", item)
	}
	first := item.Values()[0]
	if first.Kind != KindTable || first.T == nil {
		t.Fatalf("settings 第一项不是表: %+v", first)
	}
	if got := first.T.Get("label").AsString(""); got != "数据回写间隔" {
		t.Errorf("settings 第一项的 label 不对: %q", got)
	}
	ref := first.T.Get("values")
	if ref.Kind != KindTable || ref.T == nil || len(ref.T.Values()) != 2 {
		t.Fatalf("values = POLL_CHOICES 没接住前面那张两行的候选表: %+v", ref)
	}
	if c := ref.T.Values()[0]; c.Kind != KindTable || c.T == nil ||
		c.T.Get("description").AsString("") != "每 1 秒" {
		t.Errorf("候选表第一项的内容不对: %+v", c)
	}
	if d := first.T.Get("default").AsNumber(0); d != 1 {
		t.Errorf("接住变量引用之后，同一个表条目后面的 default 也要还在，实际 %v", d)
	}
	// local 赋的值引擎也留在 env 里，所以面板同样收。
	if !tbl.Has("POLL_CHOICES") {
		t.Error("local POLL_CHOICES 没被收下来")
	}
}

func TestParseLuaAssignmentsSkipsCodeWithoutFailing(t *testing.T) {
	src := `local function hello() return 1 end
name = ChooseTranslationTable({ en = "English", zh = "中文" })
if locale == "zh" then
    name = "凭运气选出来的中文"
else
    name = "凭运气选出来的英文"
end
print("这一行里有 , 逗号 和 } 花括号")
for i = 1, 3 do
    table.insert(choices, {description = tostring(i), data = i})
end
version = "2.0"
description = "带函数值的写法: " .. (function() return "x" end)()
settings = { { name = "x", default = function() return 1 end, description = "说明" } }
configuration_options = settings
name_from_forward_ref = LATER_TABLE
LATER_TABLE = { { description = "晚了一步", data = 1 } }
`
	tbl, err := ParseLuaAssignments(src)
	if err != nil {
		t.Fatalf("函数和控制流是 modinfo.lua 的正常写法，不该报错: %v", err)
	}
	// 表达式求不出来的（函数调用、翻译表选择）不收，宁可空着也不猜一个。
	if v := tbl.Get("name"); v.Kind != KindInvalid {
		t.Errorf("name = ChooseTranslationTable(...) 不该收值，实际 %+v", v)
	}
	// 两个分支都写 name：不跳过整块的话这里会收到一个凭运气选出来的名字。
	if v := tbl.Get("version"); v.AsString("") != "2.0" {
		t.Errorf("跳过代码块之后要能继续读后面的赋值，实际 %+v", v)
	}
	if tbl.Get("description").Kind != KindInvalid {
		t.Errorf("description 是字符串拼接表达式，不该收到半截值: %+v", tbl.Get("description"))
	}
	item, ok := tbl.Nested("settings")
	if !ok || item.Len() != 1 {
		t.Fatalf("settings 应该读到 1 项，实际 %+v", item)
	}
	first := item.Values()[0]
	if first.T == nil {
		t.Fatal("settings 第一项不是表")
	}
	if got := first.T.Get("name").AsString(""); got != "x" {
		t.Errorf("函数值前后的字段要能读全，name 实际 %q", got)
	}
	if first.T.Has("default") {
		t.Error("default = function() ... end 不该收值")
	}
	if got := first.T.Get("description").AsString(""); got != "说明" {
		t.Errorf("函数体跳完后同一项里的 description 要还在，实际 %q", got)
	}
	// configuration_options = settings 复用同一张表：名字在上面已经读到，所以接得住。
	// 反过来，名字没在前面定义过的引用一律不收（见下面 forwardRef），面板不凭空造表。
	co, ok := tbl.Nested("configuration_options")
	if !ok || co.Len() != 1 {
		t.Fatalf("configuration_options 应该接到 settings 那张表: %+v", tbl.Get("configuration_options"))
	}
	if v := tbl.Get("name_from_forward_ref"); v.Kind != KindInvalid {
		t.Errorf("引用还没定义的名字不该收到值: %+v", v)
	}
}

func TestParseLuaAssignmentsStillAcceptsReturnTable(t *testing.T) {
	// 少数模组和面板早期的样例写成 return { ... }。引擎碰这种其实读不到 env，
	// 但面板不必比引擎更挑：能读就读，别把一份能用信息判成坏文件。
	tbl, err := ParseLuaAssignments(`return { name = "旧写法", version = "3" }`)
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Get("name").AsString("") != "旧写法" {
		t.Errorf("return 形状没读对: %+v", tbl)
	}
}

func TestParseLuaAssignmentsReportsStructuralDamage(t *testing.T) {
	bad := map[string]string{
		"引号没闭合":   "name = \"没闭合\nversion = \"1\"\n",
		"表没闭合":    "settings = { { name = \"x\" }\n",
		"长字符串没闭合": "description = [[\n正文\n",
	}
	for name, src := range bad {
		if _, err := ParseLuaAssignments(src); err == nil {
			t.Errorf("%s：结构损坏必须报错，否则一个被截断的文件会伪装成\"读成功但字段全空\"", name)
		}
	}
}

func TestParseLuaLongStringLevels(t *testing.T) {
	tbl, err := ParseLuaAssignments(`a = [=[带 ]] 也不算结束]=]
b = [==[第二层 ]=] 还是正文]==]
c = ""
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := tbl.Get("a").AsString(""); got != "带 ]] 也不算结束" {
		t.Errorf("层级 1 的长字符串读错了: %q", got)
	}
	if got := tbl.Get("b").AsString(""); got != "第二层 ]=] 还是正文" {
		t.Errorf("层级 2 的长字符串读错了: %q", got)
	}
}

func TestParseLuaTableStaysStrictOnVariable(t *testing.T) {
	// modoverrides / worldgenoverride 是纯数据文件：看不懂的东西必须报错，
	// 宽松只给 modinfo.lua 用。
	if _, err := ParseLuaTable(`return { a = SOMEVAR }`); err == nil {
		t.Error("严格模式必须拒绝变量引用")
	}
	if _, err := ParseLuaTable(`return { a = function() return 1 end }`); err == nil {
		t.Error("严格模式必须拒绝函数值")
	}
}

func TestParseLuaAssignmentsSkipsLongBracketComment(t *testing.T) {
	tbl, err := ParseLuaAssignments("--[=[ 这段注释里有 ]= 和不成对的 ]] \n多行 ]=]\nname = \"过了\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := tbl.Get("name").AsString(""); got != "过了" {
		t.Errorf("--[=[ ]=] 长注释没跳对，name 实际 %q", got)
	}
}

// 下面这一组对着本机真实工坊内容里的写法（workshop/content/322330 实测）：
// 有作者的 modinfo.lua 整份一个换行都没有，Lua 本来就不需要语句分隔符。
// 早前面板把"值后面还有代码"一律当成半截表达式丢掉，于是一份好文件在界面上
// 只剩"名字读不出来"，还被标成 modinfo 读不懂。
func TestParseLuaAssignmentsReadsOnePhysicalLine(t *testing.T) {
	// 1207269058（Simple Health Bar DST）的缩略版：整行零换行。
	src := `name = " Simple Health Bar DST" version = "2.16" description = "Version:DST"..version.."\n正文" author = "DYC" api_version = 0x6 forumthread = ""`
	tbl, err := ParseLuaAssignments(src)
	if err != nil {
		t.Fatalf("一行写完的 modinfo 是合法 Lua，不该报错: %v", err)
	}
	if got := tbl.Get("name").AsString(""); got != " Simple Health Bar DST" {
		t.Errorf("同行的第一条赋值也该收下来，name 实际 %q", got)
	}
	if got := tbl.Get("version").AsString(""); got != "2.16" {
		t.Errorf("version 实际 %q", got)
	}
	if got := tbl.Get("author").AsString(""); got != "DYC" {
		t.Errorf("拼出来的 description 后面的 author 没读到: %q", got)
	}
	// description 是 "前缀" .. version .. "…" 拼出来的，左半边不等于整句，照旧不收。
	if v := tbl.Get("description"); v.Kind != KindInvalid {
		t.Errorf("拼接出来的 description 不该收到半截值: %+v", v)
	}
	// 0x6 是 Lua 5.1 合法的十六进制整数，读成 6；读成 0 比读不到更误导人。
	if got := tbl.Get("api_version").AsNumber(0); got != 6 {
		t.Errorf("api_version = 0x6 应该读成 6，实际 %v", got)
	}
	if got := tbl.Get("forumthread").AsString("缺"); got != "" {
		t.Errorf("行尾那条也要在，实际 %q", got)
	}
}

func TestParseLuaAssignmentsDoesNotSplitOneStatementInTwo(t *testing.T) {
	// `名字 =` 只在"和前面的代码隔了空白"时才算一条新语句：点号索引的赋值目标
	// （cfg.name = ...）里那个 name 是同一条语句的一部分，停过去会把这条切一半，
	// 还会凭空造出一个叫 cfg 的键。
	tbl, err := ParseLuaAssignments("local cfg = {} cfg.name = \"内部字段\" table.insert(cfg, 1) name = \"真名字\" api_version = 0x6 note = \"尾\"\n")
	if err != nil {
		t.Fatalf("一条语句一行写完也是合法 Lua: %v", err)
	}
	if got := tbl.Get("name").AsString(""); got != "真名字" {
		t.Errorf("同行的下一条赋值要读到，name 实际 %q", got)
	}
	if v := tbl.Get("cfg"); v.Kind != KindInvalid {
		t.Errorf("cfg.name = ... 是在往别的表里塞东西，不该收出一个 cfg 键: %+v", v)
	}
	if got := tbl.Get("note").AsString(""); got != "尾" {
		t.Errorf("十六进制数字后面的那条也要读到，note 实际 %q", got)
	}
	if got := tbl.Get("api_version").AsNumber(0); got != 6 {
		t.Errorf("api_version 实际 %v", got)
	}
}

// 1951468597（Sweet House）实测：语句之间一个空格都没有，
// `name="Sweet House"description=[[ … ]]author="羽中"`。字符串和长字符串自己就是
// 完整边界，所以紧跟其后的 `名字 =` 必须是下一条语句，而不是这一条的尾巴。
func TestParseLuaAssignmentsReadsStatementsWithoutGaps(t *testing.T) {
	src := "name=\"Sweet House\"description=[[\n\n正文\n]]author=\"羽中\" version = \"1.0\"\n"
	tbl, err := ParseLuaAssignments(src)
	if err != nil {
		t.Fatalf("贴在一起的写法是合法 Lua，不该报错: %v", err)
	}
	if got := tbl.Get("name").AsString(""); got != "Sweet House" {
		t.Errorf("紧贴长字符串的前一条 name 也要读到，实际 %q", got)
	}
	if got := tbl.Get("description").AsString(""); got != "\n正文\n" {
		t.Errorf("description 实际 %q", got)
	}
	if got := tbl.Get("author").AsString(""); got != "羽中" {
		t.Errorf("长字符串后面紧贴的 author 没读到: %q", got)
	}
	if got := tbl.Get("version").AsString(""); got != "1.0" {
		t.Errorf("version 实际 %q", got)
	}
}

func TestParseLuaAssignmentsIgnoresLocalMultiNameLine(t *testing.T) {
	// local a, b = 1, 2：名字和值不是一一对应，整条丢掉。
	// 同一条语句里的 `b = 1` 长得就像下一条赋值，停过去会凭空造出一个 b 键。
	tbl, err := ParseLuaAssignments("local a, b = 1, 2\nname = \"n\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if v := tbl.Get("b"); v.Kind != KindInvalid {
		t.Errorf("local a, b 的第二个名字不该被当成一条顶层赋值收下来: %+v", v)
	}
	if tbl.Get("name").AsString("") != "n" {
		t.Error("丢一条 local 不能把后面的字段带走")
	}
}
