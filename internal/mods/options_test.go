// 配置项字典的用例：同一个"可选值列表"在真实 modinfo.lua 里有好几种拼法，
// 每种都要落到一个界面上能直接画的控件；拼不出来的必须报出来，不能猜一个值。
package mods

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchemaReadsDataListShapes(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "workshop-100", `name = "背包大小"
api_version = 10
configuration_options =
{
    {
        name = "size",
        label = "背包格数",
        hover = "越大越吃内存",
        data =
        {
            {description = "普通", data = 8},
            {description = "加倍", data = 15},
        },
        default = 8,
    },
    {
        name = "mode",
        label = "模式",
        data = { normal = 1, chaos = 2 },
        default = 2,
    },
    {
        name = "names",
        label = "文字项",
        data = { "a", "b" },
        default = "a",
    },
    {
        name = "not_set",
        label = "还没实现",
        hover = "作者留的空表",
        data = {},
        default = 3,
    },
    {
        name = "broken_one",
        label = "读不出值",
        data = { { description = "只有描述没有值" } },
        default = 0,
    },
}
`)
	sc, err := m.SchemaFor("workshop-100")
	if err != nil {
		t.Fatal(err)
	}
	if sc.From != "configuration_options" {
		t.Errorf("字典来源不对: %s", sc.From)
	}
	if len(sc.Options) != 4 {
		t.Fatalf("应该认出 4 项，实际 %d 项: %+v", len(sc.Options), sc.Options)
	}
	if !strings.Contains(strings.Join(sc.Skipped, "|"), "broken_one") {
		t.Errorf("读不出值的项没报进 Skipped: %+v", sc.Skipped)
	}

	byName := map[string]Option{}
	for _, o := range sc.Options {
		byName[o.Name] = o
	}
	size := byName["size"]
	if size.Kind != "select" || len(size.Choices) != 2 {
		t.Fatalf("size 应该是两个候选的下拉: %+v", size)
	}
	if size.Choices[0].Label != "普通" || size.Choices[0].Value != float64(8) {
		t.Errorf("候选项的标签/值不对: %+v", size.Choices)
	}
	if size.Hover != "越大越吃内存" {
		t.Errorf("hover 没读到: %+v", size)
	}
	// 默认值要显示成候选项的中文标签，不是 8 这个码值。
	if size.DefaultLabel != "普通" {
		t.Errorf("默认值标签应该是「普通」，实际 %q", size.DefaultLabel)
	}
	if byName["mode"].Kind != "select" || len(byName["mode"].Choices) != 2 ||
		byName["mode"].Choices[1].Label != "chaos" {
		t.Errorf("键表写法没接住: %+v", byName["mode"])
	}
	if byName["mode"].DefaultLabel != "chaos" {
		t.Errorf("键表默认值标签不对: %+v", byName["mode"])
	}
	if byName["names"].Kind != "select" || byName["names"].Choices[0].Value != "a" {
		t.Errorf("标量数组写法没接住: %+v", byName["names"])
	}
	// data = {} 是官方模板里"这一项还没实现"的写法：没有候选值，按默认值的类型给控件。
	if byName["not_set"].Kind != "number" || len(byName["not_set"].Choices) != 0 {
		t.Errorf("空值列表应该退成数字项: %+v", byName["not_set"])
	}
}

func TestSchemaFallsBackToSettingsTable(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "OnlySettings", `name = "只有 settings"
settings =
{
    { name = "count", label = "数量", typing = "number", default = 5 },
    { name = "on", label = "启用", typing = "bool", default = true, description = "关掉就不干活" },
    { name = "level", label = "档位", values = { {description = "低", data = 1}, {description = "高", data = 9} }, default = 9 },
}
`)
	sc, err := m.SchemaFor("OnlySettings")
	if err != nil {
		t.Fatal(err)
	}
	if sc.From != "settings" {
		t.Fatalf("没有 configuration_options 时要退到 settings，实际 %s", sc.From)
	}
	if len(sc.Options) != 3 {
		t.Fatalf("项数不对: %+v", sc.Options)
	}
	var kinds []string
	for _, o := range sc.Options {
		kinds = append(kinds, o.Name+"="+o.Kind)
	}
	got := strings.Join(kinds, ",")
	if got != "count=number,on=toggle,level=select" {
		t.Errorf("控件类型不对: %s", got)
	}
	if sc.Options[1].Hover != "关掉就不干活" {
		t.Errorf("settings 那族的说明写在 description 里，没读出来: %+v", sc.Options[1])
	}
}

func TestSchemaWithoutAnyTableIsNotAnError(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "NoOptions", `name = "没有可调项"
api_version = 10
`)
	sc, err := m.SchemaFor("NoOptions")
	if err != nil {
		t.Fatal(err)
	}
	if sc.From != "none" || len(sc.Options) != 0 || sc.Note == "" {
		t.Errorf("没有配置项要说清原因而不是回一个空列表: %+v", sc)
	}
}

func TestSchemaBrokenModinfoMarksNotOk(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "Broken", `name = "坏文件"
configuration_options = { { name = "x" }
`)
	sc, err := m.SchemaFor("Broken")
	if err != nil {
		t.Fatal(err)
	}
	if sc.ModinfoOK || sc.Name != "Broken" {
		t.Errorf("解析失败要留标记并用目录名兜住: %+v", sc)
	}
	if sc.From != "none" || !strings.Contains(sc.Note, "解析不了") {
		t.Errorf("坏文件要给界面一句解释: %+v", sc)
	}
}

func TestSchemaRejectsBadDirAndMissingFile(t *testing.T) {
	m, _ := newManager(t)
	if _, err := m.SchemaFor("..\\evil"); err == nil {
		t.Error("目录名要过白名单，不能让 ../ 拼进路径")
	}
	if _, err := m.SchemaFor("not-there"); err == nil || !strings.Contains(err.Error(), "modinfo.lua") {
		t.Errorf("目录不存在要给可操作的中文错误: %v", err)
	}
	// 目录在、modinfo.lua 不在：也是同一条结论。
	if err := os.MkdirAll(filepath.Join(m.Paths.ModsRoot(), "EmptyDir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SchemaFor("EmptyDir"); err == nil {
		t.Error("缺 modinfo.lua 要报错")
	}
}

// 面板自己的助手 MOD 必须能被这份字典读懂——它是这个页面上最常出现的一行，
// 而它的 modinfo.lua 用的是 settings 那一族。
func TestSchemaReadsOurOwnHelperMod(t *testing.T) {
	m, _ := newManager(t)
	raw, err := os.ReadFile(filepath.Join("..", "modbridge", "mods", "DSTPanelHelper", "modinfo.lua"))
	if err != nil {
		t.Fatal(err)
	}
	writeModinfo(t, m, "DSTPanelHelper", string(raw))
	sc, err := m.SchemaFor("DSTPanelHelper")
	if err != nil {
		t.Fatal(err)
	}
	if !sc.ModinfoOK || sc.Name != "DST 管理面板助手" {
		t.Fatalf("自家助手的 modinfo 读不对: %+v", sc)
	}
	if len(sc.Options) != 2 {
		t.Fatalf("助手有两项设置: %+v", sc.Options)
	}
	poll := sc.Options[0]
	if poll.Name != "poll_interval" || poll.Kind != "select" || len(poll.Choices) != 4 {
		t.Errorf("回写间隔应该是个四档下拉: %+v", poll)
	}
	if poll.DefaultLabel != "每 1 秒" {
		t.Errorf("默认值要显示成中文档位，实际 %q", poll.DefaultLabel)
	}
	if sc.Options[1].Name != "player_detail" || sc.Options[1].Kind != "toggle" {
		t.Errorf("玩家明细应该是开关: %+v", sc.Options[1])
	}
}
