package modbridge

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/dst"
)

const masterShard = dst.ShardMaster

func pBool(v bool) *bool      { return &v }
func pNum(v float64) *float64 { return &v }

func TestPatchOverrideCreatesWorkshopEntry(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", masterShard)

	err := b.PatchOverride("Cluster_1", masterShard, "workshop-378160973", OverridePatch{
		Enabled:  pBool(true),
		Priority: pNum(-100),
		Options:  map[string]any{"enable_player_respawn": true, "radius": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("文件没写出来: %v", err)
	}
	// 带连字符的键必须走方括号形式，否则是 Lua 语法错误，服务端整个文件读不进来。
	if !strings.Contains(string(raw), `["workshop-378160973"]`) {
		t.Fatalf("工坊键没按方括号写:\n%s", raw)
	}
	if strings.ContainsAny(string(raw), "()") {
		t.Errorf("生成的文件里不该有括号（modoverrides 是在空 env 里执行的）:\n%s", raw)
	}

	list, err := b.ListOverrides("Cluster_1", masterShard)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应该只有一条: %+v", list)
	}
	got := list[0]
	if got.Dir != "workshop-378160973" || !got.Enabled || got.Priority == nil || *got.Priority != -100 {
		t.Errorf("读回来的状态不对: %+v", got)
	}
	if got.Options["enable_player_respawn"] != true {
		t.Errorf("布尔配置项没读回来: %+v", got.Options)
	}
	if n, ok := got.Options["radius"].(float64); !ok || n != 3 {
		t.Errorf("数字配置项没读回来: %+v", got.Options)
	}
}

func TestPatchOverrideKeepsUntouchedOptions(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", masterShard)
	seed := `return {
	["workshop-111"] = {
		enabled = false,
		priority = 5,
		configuration_options = {
			a = 1,
			name = "旧值",
		},
	},
}
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.PatchOverride("Cluster_1", masterShard, "workshop-111",
		OverridePatch{Options: map[string]any{"a": 2}}); err != nil {
		t.Fatal(err)
	}
	list, err := b.ListOverrides("Cluster_1", masterShard)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Options["name"] != "旧值" {
		t.Errorf("没动的配置项被抹掉了: %+v", list[0].Options)
	}
	if n, ok := list[0].Options["a"].(float64); !ok || n != 2 {
		t.Errorf("给了新值却没写: %+v", list[0].Options)
	}
	// enabled 与 priority 不在 patch 里，必须原样。
	if list[0].Enabled || list[0].Priority == nil || *list[0].Priority != 5 {
		t.Errorf("patch 没提的字段被改了: %+v", list[0])
	}
}

func TestPatchOverrideDeletesOptionWhenValueIsNil(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", masterShard)
	seed := `return {
	["workshop-222"] = {
		enabled = true,
		configuration_options = {
			dropped = 1,
			kept = "y",
		},
	},
}
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.PatchOverride("Cluster_1", masterShard, "workshop-222",
		OverridePatch{Options: map[string]any{"dropped": nil}}); err != nil {
		t.Fatal(err)
	}
	list, _ := b.ListOverrides("Cluster_1", masterShard)
	if _, ok := list[0].Options["dropped"]; ok {
		t.Errorf("删除没生效，引擎就不可能回落到 modinfo 默认值: %+v", list[0].Options)
	}
	if list[0].Options["kept"] != "y" {
		t.Errorf("别的配置项被牵连: %+v", list[0].Options)
	}
}

func TestListOverridesSkipsWhatItCannotRead(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", masterShard)
	// 用户手写的文件：一条标量、一条带嵌套配置项。面板只做能看懂的部分，不删也不报错。
	seed := `return {
	["junk"] = true,
	["workshop-333"] = {
		enabled = true,
		configuration_options = {
			list = { 1, 2, 3 },
			scalar = "ok",
		},
	},
}
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := b.ListOverrides("Cluster_1", masterShard)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Dir != "workshop-333" {
		t.Fatalf("非表条目应该跳过: %+v", list)
	}
	if list[0].Options["scalar"] != "ok" {
		t.Errorf("标量配置项要读出来: %+v", list[0].Options)
	}
	if strings.Join(list[0].NestedOptions, ",") != "list" {
		t.Errorf("嵌套配置项要单独报出来，别让用户以为面板能编辑: %+v", list[0])
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `["junk"]`) {
		t.Error("读一遍就把看不懂的条目删了，这是不可逆的")
	}
}

func TestPatchOverrideValidatesBeforeTouchingFile(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", masterShard)

	cases := []struct {
		name    string
		patch   OverridePatch
		wantMsg string
	}{
		{"嵌套配置项", OverridePatch{Options: map[string]any{"x": map[string]any{"a": 1}}}, "文本、数字或是否"},
		{"数组配置项", OverridePatch{Options: map[string]any{"x": []any{1}}}, "文本、数字或是否"},
		{"priority 是 NaN", OverridePatch{Priority: pNum(math.NaN())}, "有限数字"},
		{"priority 是无穷", OverridePatch{Priority: pNum(math.Inf(1))}, "有限数字"},
	}
	for _, c := range cases {
		err := b.PatchOverride("Cluster_1", masterShard, "workshop-444", c.patch)
		if err == nil {
			t.Errorf("%s 应该被拒", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantMsg) {
			t.Errorf("%s 的错误要说清规则，实际 %q", c.name, err.Error())
		}
		// 错误会原样进界面，Go 的类型名一出现就等于让用户自己去猜。
		if strings.Contains(err.Error(), "map[") || strings.Contains(err.Error(), "interface") {
			t.Errorf("%s 的错误里漏了 Go 类型名：%q", c.name, err.Error())
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("全部被拒的 patch 还是写出了文件")
	}

	// NaN 会渲染成 Lua 读不懂的东西，必须在写入前就拦掉。
	if err := b.PatchOverride("Cluster_1", masterShard, "workshop-444",
		OverridePatch{Options: map[string]any{"x": math.NaN()}}); err == nil {
		t.Error("NaN 应该被拒")
	}
}

func TestPatchOverrideIsIdempotent(t *testing.T) {
	b, _ := newFixture(t)
	patch := OverridePatch{Enabled: pBool(true), Options: map[string]any{"a": 1}}
	if err := b.PatchOverride("Cluster_1", masterShard, "workshop-555", patch); err != nil {
		t.Fatal(err)
	}
	path := b.Paths.ModOverrides("Cluster_1", masterShard)
	first, _ := os.ReadFile(path)
	bakCount := countBaks(t, filepath.Dir(path))
	if err := b.PatchOverride("Cluster_1", masterShard, "workshop-555", patch); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Errorf("同样的 patch 写出不同内容:\n%s\n---\n%s", first, second)
	}
	// 内容没变就不该再留一份 .bak——前端定时刷新会把备份刷成一堆同样内容的快照。
	if got := countBaks(t, filepath.Dir(path)); got != bakCount {
		t.Errorf(".bak 从 %d 变成 %d", bakCount, got)
	}
}

func countBaks(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bak") {
			n++
		}
	}
	return n
}
