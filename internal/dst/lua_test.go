package dst

import (
	"strings"
	"testing"
)

// 覆盖 DST 配置文件里会出现的全部语法形状。
const realWorldSample = `-- 这是一个块注释的第一行
--[[ 这是
     跨行的块注释 ]]
return {
	autumn = "long",
	desc = "永不结束的饥荒沙盒模式。永远可以在绚丽之门复活。",
	has_ocean = true,
	keep_disconnected_tiles = true,
	layout_mode = "LinkNodesByKeys",
	max_playlist_position = 999,
	min_playlist_position = 0,
	numrandom_set_pieces = 4,
	overrides = {
		season_start = "default",
		world_size = "small",
		ocean_shoal = "default",
		start_location = "default",
		darkness = "default",
	},
	playstyle = "endless",
	random_starting_pieces = {},
	required_drops_prefabs = {
		"axe",
	},
	settings_id = "ENDLESS",
	substitutes = {},
	version = 4,
	wormhole_location_id = "RANDOM",
	zero = 0,
	ratio = 0.5,
	neg = -1,
}
`

const modOverridesSample = `return {
	["workshop-1158324974"] = {
		configuration_options = {
			ShowDurabilityNumbers = true,
			ShowRawState = false,
			StackSize = 20,
			LabelStyle = "MODERN",
		},
		enabled = true,
		mod_name = "Show Me",
		server_only = false,
	},
	["workshop-378160973"] = {
		configuration_options = {
			PANDORAENABLED = true,
		},
		enabled = true,
	},
}
`

func deepEqualTable(a, b *LuaTable) bool {
	if a.Len() != b.Len() {
		return false
	}
	for i := range a.entries {
		ae, be := a.entries[i], b.entries[i]
		if (ae.key == nil) != (be.key == nil) {
			return false
		}
		if ae.key != nil && *ae.key != *be.key {
			return false
		}
		if !deepEqualValue(ae.val, be.val) {
			return false
		}
	}
	return true
}

func deepEqualValue(a, b LuaValue) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KindString:
		return a.S == b.S
	case KindNumber:
		return a.N == b.N
	case KindBool:
		return a.B == b.B
	case KindTable:
		return deepEqualTable(a.T, b.T)
	}
	return false
}

func TestParseRealWorldSample(t *testing.T) {
	tbl, err := ParseLuaTable(realWorldSample)
	if err != nil {
		t.Fatalf("解析真实世界配置失败: %v", err)
	}

	cases := []struct {
		key  string
		want LuaValue
	}{
		{"desc", String("永不结束的饥荒沙盒模式。永远可以在绚丽之门复活。")},
		{"has_ocean", Bool(true)},
		{"min_playlist_position", Number(0)},
		{"numrandom_set_pieces", Number(4)},
		{"version", Number(4)},
		{"zero", Number(0)},
		{"ratio", Number(0.5)},
		{"neg", Number(-1)},
		{"layout_mode", String("LinkNodesByKeys")},
	}
	for _, c := range cases {
		if !tbl.Has(c.key) {
			t.Errorf("键 %s 不存在", c.key)
			continue
		}
		got := tbl.Get(c.key)
		if !deepEqualValue(got, c.want) {
			t.Errorf("键 %s = %+v, 期望 %+v", c.key, got, c.want)
		}
	}

	ov, ok := tbl.Nested("overrides")
	if !ok {
		t.Fatal("overrides 子表缺失")
	}
	if got := ov.Get("world_size").AsString(""); got != "small" {
		t.Errorf("overrides.world_size = %q", got)
	}
	if ov.Len() != 5 {
		t.Errorf("overrides 应有 5 项, 实际 %d", ov.Len())
	}

	rsp := tbl.Get("random_starting_pieces")
	if rsp.Kind != KindTable || rsp.T.Len() != 0 {
		t.Errorf("random_starting_pieces 应为空表, 实际 %+v", rsp)
	}
	if rsp.Kind == KindTable && rsp.T.IsArray() {
		t.Error("空表不应判定为数组")
	}

	rdp := tbl.Get("required_drops_prefabs")
	if rdp.Kind != KindTable || rdp.T == nil {
		t.Fatal("required_drops_prefabs 缺失")
	}
	if !rdp.T.IsArray() {
		t.Error("required_drops_prefabs 应判定为数组")
	}
	if rdp.T.entries[0].val.AsString("") != "axe" {
		t.Errorf("数组首元素 = %v", rdp.T.entries[0].val)
	}
	if rdp.T.entries[0].key != nil {
		t.Error("数组元素不应带键")
	}
}

func TestLuaRoundTripIsLossless(t *testing.T) {
	for name, src := range map[string]string{"world": realWorldSample, "mods": modOverridesSample} {
		first, err := ParseLuaTable(src)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", name, err)
		}
		rendered := Render(first)
		second, err := ParseLuaTable(rendered)
		if err != nil {
			t.Fatalf("%s 重解析失败: %v\n%s", name, err, rendered)
		}
		if !deepEqualTable(first, second) {
			t.Errorf("%s 语义在往返中改变", name)
		}
		// 渲染必须确定，否则每次保存都产生无意义 diff。
		if Render(second) != rendered {
			t.Errorf("%s 渲染不确定:\n--- A ---\n%s\n--- B ---\n%s", name, rendered, Render(second))
		}
		// 中文必须原样回来，不能被转义成 \xxx 序列。
		if name == "world" && !strings.Contains(rendered, "永不结束的饥荒沙盒模式") {
			t.Errorf("中文串被破坏:\n%s", rendered)
		}
	}
}

func TestParseWorkshopBracketKeys(t *testing.T) {
	tbl, err := ParseLuaTable(modOverridesSample)
	if err != nil {
		t.Fatalf("解析 modoverrides 失败: %v", err)
	}
	keys := tbl.Keys()
	if len(keys) != 2 {
		t.Fatalf("应有 2 个模组条目, 实际 %d: %v", len(keys), keys)
	}
	if keys[0] != "workshop-1158324974" {
		t.Errorf("首个键 = %q", keys[0])
	}
	mod := tbl.Get("workshop-1158324974")
	if mod.Kind != KindTable || mod.T == nil {
		t.Fatal("workshop-1158324974 取不到")
	}
	if !mod.T.Get("enabled").AsBool(false) {
		t.Error("enabled 应为 true")
	}
	opts, ok := mod.T.Nested("configuration_options")
	if !ok {
		t.Fatal("configuration_options 缺失")
	}
	if v := opts.Get("StackSize"); v.Kind != KindNumber || v.N != 20 {
		t.Errorf("StackSize = %+v, 期望数字 20", v)
	}
	if v := opts.Get("LabelStyle"); v.AsString("") != "MODERN" {
		t.Errorf("LabelStyle = %+v", v)
	}
	// 回写必须保持 ["workshop-xxx"] 方括号形式，否则 DST 语法错误。
	out := Render(tbl)
	if !strings.Contains(out, `["workshop-1158324974"]`) {
		t.Errorf("workshop 键未用方括号形式:\n%s", out)
	}
	// 普通标识符键不应被加方括号。
	if strings.Contains(out, `["enabled"]`) {
		t.Error("普通键被无谓地加了方括号")
	}
}

func TestSetMutatesInPlaceKeepsOrder(t *testing.T) {
	tbl, err := ParseLuaTable(realWorldSample)
	if err != nil {
		t.Fatal(err)
	}
	before := tbl.Keys()
	tbl.Get("overrides").T.Set("world_size", String("huge"))
	after := tbl.Keys()
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("改子表值导致顶层顺序变化: %v -> %v", before, after)
	}
	// 新增键追加在末尾。
	ov, ok := tbl.Nested("overrides")
	if !ok {
		t.Fatal("改值后 overrides 子表反而取不到了")
	}
	if got := ov.Get("world_size").AsString(""); got != "huge" {
		t.Errorf("world_size 未改成 huge, 实际 %q", got)
	}
	n := tbl.Len()
	tbl.Set("brand_new_key", Number(7))
	if tbl.Len() != n+1 {
		t.Errorf("追加后长度 %d, 期望 %d", tbl.Len(), n+1)
	}
	if tbl.Keys()[tbl.Len()-1] != "brand_new_key" {
		t.Error("新键应追加在末尾")
	}
}

func TestDeleteAndEnsureNested(t *testing.T) {
	tbl := NewTable()
	sub := tbl.EnsureNested("a", "b", "c")
	sub.Set("x", Bool(true))
	got, ok := tbl.Nested("a", "b", "c")
	if !ok || !got.Get("x").AsBool(false) {
		t.Fatalf("EnsureNested 未建出深层表:\n%s", Render(tbl))
	}
	tbl.Delete("a")
	if tbl.Has("a") {
		t.Error("Delete 未生效")
	}
	// 缺键的零值绝不能伪装成空表，否则 Nested 会解引用 nil。
	missing := tbl.Get("a")
	if missing.Kind != KindInvalid {
		t.Errorf("缺键应返回 KindInvalid, 实际 %v", missing.Kind)
	}
	if missing.T != nil {
		t.Error("缺键零值的 T 应为 nil")
	}
	if !missing.AsBool(true) || missing.AsString("默认") != "默认" {
		t.Error("缺键取值应回落到调用方给的默认值")
	}
	if _, ok := tbl.Nested("a"); ok {
		t.Error("对缺键走 Nested 不应成功")
	}
}

func TestLuaRejectsBareIdentifier(t *testing.T) {
	// Klei 文件里不会有变量引用；出现说明这不是数据文件，不能当配置解析。
	for _, bad := range []string{
		"return { a = SomeGlobal }",
		"return { a = ",
		"return { [1] = 'x' }",
		"return { a = 'unterminated }",
		"local x = 1",
	} {
		if _, err := ParseLuaTable(bad); err == nil {
			t.Errorf("应当解析失败但成功了: %q", bad)
		}
	}
}

func TestRenderEmptyTable(t *testing.T) {
	if got := Render(NewTable()); got != "return {  }\n" {
		t.Errorf("空表渲染 = %q, 期望 Klei 风格的 {  }", got)
	}
}

func TestStringQuotingRoundTrip(t *testing.T) {
	// 密码/集群名常带引号与反斜杠，必须活过往返。
	tbl := NewTable()
	tbl.Set("cluster_password", String(`a"b\c`+"换行\n尾巴"))
	out := Render(tbl)
	again, err := ParseLuaTable(out)
	if err != nil {
		t.Fatalf("含转义的串渲染后无法解析: %v\n%s", err, out)
	}
	if got := again.Get("cluster_password").AsString(""); got != `a"b\c`+"换行\n尾巴" {
		t.Errorf("转义往返损坏: %q", got)
	}
}

// 引擎的 SetPersistentString 会给它写出的每个文件加 11 字节文本头，
// 而它自己读的时候会剥掉——所以 MOD 与引擎留下的文件都是"带头"的，面板必须跟着剥。
// 版本号按数字通配（实测是 1），别哪天引擎写 v2 就变成全线解析失败。
func TestStripKleiHeader(t *testing.T) {
	body := `{"shard":"Master"}`
	for _, header := range []string{"KLEI     1 ", "KLEI     2 ", "KLEI\t\t11\t"} {
		if got := string(StripKleiHeader([]byte(header + body))); got != body {
			t.Errorf("%q 没剥干净，得到 %q", header, got)
		}
	}
	// 面板自己写的文件没有头，原样返回才是对的（不能拿 "KLEI" 开头的正文当头部误剥）。
	for _, raw := range []string{body, "return { }\n", ""} {
		if got := string(StripKleiHeader([]byte(raw))); got != raw {
			t.Errorf("没有头却动了字节: %q -> %q", raw, got)
		}
	}
	// 只有 KLEI 后跟空格+数字+空格才算头；"KLEITHIS" 这类正文不能被剥。
	if got := string(StripKleiHeader([]byte("KLEITHIS is a mod folder"))); got != "KLEITHIS is a mod folder" {
		t.Errorf("把正文当成了头: %q", got)
	}
	// Lua 侧同样要能读：引擎重写 worldgenoverride.lua 用的就是这条通道。
	tbl, err := ParseLuaTable("KLEI     1 return {\n\toverride_enabled = true,\n}")
	if err != nil {
		t.Fatalf("带头的 Lua 表读不回来: %v", err)
	}
	if !tbl.Get("override_enabled").AsBool(false) {
		t.Errorf("剥了头却没读到内容: %+v", tbl.Keys())
	}
}
