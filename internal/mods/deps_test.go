package mods

import (
	"strings"
	"testing"
)

// 引擎的 mod_dependencies 实际是"数组的数组"：每项是一张表，先看 workshop 字段，
// 再看数组部分的候选名（modindex.lua:484 的 FindEnabledMod）。
// 但第三方模组的写法不统一，两种拼法都要能读出来，否则会误报缺依赖。
func TestListParsesBothDependencySpellings(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "workshop-100", `name = "需要库的模组"
api_version = 10
mod_dependencies = {
	{ workshop = "workshop-111", "my_local_library" },
	"workshop-222",
	{},
}
`)
	writeModinfo(t, m, "workshop-999", `name = "纯客户端模组"
client_only_mod = true
all_clients_require_mod = false
`)
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Mod{}
	for _, x := range list {
		by[x.Dir] = x
	}
	got := by["workshop-100"].Dependencies
	if len(got) != 2 {
		t.Fatalf("空的那一组不该产出一条要求: %+v", got)
	}
	if strings.Join(got[0], ",") != "workshop-111,my_local_library" {
		t.Errorf("表写法的 workshop 字段和候选名都要收进来: %+v", got[0])
	}
	if strings.Join(got[1], ",") != "workshop-222" {
		t.Errorf("字符串写法没接住: %+v", got[1])
	}
	if len(by["workshop-999"].Dependencies) != 0 {
		t.Errorf("没声明依赖的模组不该有依赖: %+v", by["workshop-999"].Dependencies)
	}
	if !by["workshop-999"].ClientOnly || by["workshop-999"].AllClientsRequire {
		t.Errorf("两个标记读反了: %+v", by["workshop-999"])
	}
}

func TestCheckDependenciesReportsAbsentAsSilentDisable(t *testing.T) {
	m, _ := newManager(t)
	// 三种情形放在一起，才能验面板把它们的后果说成两件不同的事：
	//   workshop-100 的依赖在磁盘上但本分片没开——引擎不禁用，只是库没加载；
	//   workshop-300 的依赖整台机器都没有——引擎 DisableBecauseBad 静默禁用。
	writeModinfo(t, m, "workshop-100", `name = "A"
mod_dependencies = { { workshop = "workshop-111" } }
`)
	writeModinfo(t, m, "workshop-111", `name = "库"
`)
	writeModinfo(t, m, "workshop-300", `name = "C"
mod_dependencies = { "workshop-301" }
`)
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}

	ps := CheckDependencies(list, []string{"workshop-100", "workshop-300"})
	if len(ps) != 2 {
		t.Fatalf("两条都该报出来: %+v", ps)
	}
	by := map[string]DependencyProblem{}
	for _, p := range ps {
		by[p.Dir] = p
	}
	onDisk := by["workshop-100"]
	if !onDisk.OnDisk || !strings.Contains(onDisk.Status, "没开") {
		t.Errorf("磁盘上有、分片没开要说清楚: %+v", onDisk)
	}
	if !strings.Contains(onDisk.Hint, "不会") {
		t.Errorf("这种情形和静默禁用的区别要说明白，别让用户以为模组被禁了: %+v", onDisk.Hint)
	}
	absent := by["workshop-300"]
	if absent.OnDisk {
		t.Errorf("workshop-301 根本不在磁盘上: %+v", absent)
	}
	if !strings.Contains(absent.Status, "找不到") || !strings.Contains(absent.Hint, "静默禁用") {
		t.Errorf("要说清后果: %+v", absent)
	}
}

// 一组里的候选是"任一命中即满足"，不是全部要求。
func TestCheckDependenciesAnyCandidateSatisfiesGroup(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "workshop-100", `name = "A"
mod_dependencies = { { workshop = "workshop-111", "my_lib" } }
`)
	writeModinfo(t, m, "my_lib", `name = "本地库"
`)
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if ps := CheckDependencies(list, []string{"workshop-100", "my_lib"}); len(ps) != 0 {
		t.Errorf("第二个候选命中就该算满足: %+v", ps)
	}
	// 只开 A、不开库：候选都找不到启用的，但 my_lib 在磁盘上。
	ps := CheckDependencies(list, []string{"workshop-100"})
	if len(ps) != 1 || !ps[0].OnDisk {
		t.Fatalf("要报成\"有但没开\": %+v", ps)
	}
}

// 作者常把工坊 id 写成不带前缀的数字，引擎那边用的是目录名，前缀要补。
func TestCheckDependenciesAcceptsBareWorkshopID(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "workshop-100", `name = "A"
mod_dependencies = { { workshop = "111" } }
`)
	writeModinfo(t, m, "workshop-111", `name = "库"
`)
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if ps := CheckDependencies(list, []string{"workshop-100", "workshop-111"}); len(ps) != 0 {
		t.Errorf("裸数字 id 也该认: %+v", ps)
	}
}

// 没开的模组不查依赖：用户可能故意留着它，报出来只是噪音。
func TestCheckDependenciesIgnoresDisabledMods(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "workshop-100", `name = "A"
mod_dependencies = { "workshop-777" }
`)
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if ps := CheckDependencies(list, []string{}); len(ps) != 0 {
		t.Errorf("模组没开就不该报: %+v", ps)
	}
	if ps := CheckDependencies(list, []string{"workshop-100"}); len(ps) != 1 {
		t.Fatalf("开了才该报: %+v", ps)
	}
}

// 磁盘级检查不看分片启用状态：它的用途是"装了个依赖都还没下来的模组"的预警。
func TestDependenciesAbsentFromDiskIgnoresShards(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "workshop-100", `name = "A"
mod_dependencies = { "workshop-777" }
`)
	writeModinfo(t, m, "workshop-200", `name = "B"
mod_dependencies = { "workshop-201" }
`)
	writeModinfo(t, m, "workshop-201", `name = "B 的库"
`)
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	ps := DependenciesAbsentFromDisk(list)
	if len(ps) != 1 || ps[0].Dir != "workshop-100" {
		t.Fatalf("只报磁盘上没有的那组: %+v", ps)
	}
	if !strings.Contains(ps[0].Status, "还没在这台机器上") {
		t.Errorf("磁盘级结论不能写成\"这个分片没开\": %q", ps[0].Status)
	}
	if strings.Contains(ps[0].Hint, "静默禁用") {
		t.Errorf("还没下载不等于会被禁用，别说成同一个后果: %q", ps[0].Hint)
	}
}
