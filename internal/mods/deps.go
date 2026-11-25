package mods

import (
	"sort"
	"strings"
)

// 依赖缺失的检查。
//
// 这一层为什么必须存在，而且是面板而不是引擎来做：
// modindex.lua:577 起，模组声明的依赖在对不上时引擎只打一句
// "no valid dependent mod found for mod X"，然后 DisableBecauseBad(X)
// ——把 X 静默禁用，服务端照常启动、日志翻过去就没了。
// 用户看到的现象是"这个模组明明开着却没效果"，最难查的一类故障。
//
// 这里只用磁盘上的模组目录名比对，不联网、不读工坊：面板判的是
// "这台机器上有没有那个东西"，和引擎的 DoesModExistAnyVersion 同一条轴。

// DependencyProblem 是一条"开了的模组，依赖对不上"的记录。
type DependencyProblem struct {
	// Dir / Name 是那个开了却依赖有问题的模组。
	Dir  string `json:"dir"`
	Name string `json:"name"`
	// Candidates 是这一组依赖声明的所有候选标识，一个都没对上才会走到这里。
	Candidates []string `json:"candidates"`
	// OnDisk 表示依赖的目录在这台机器上找得到，只是这个分片没开它。
	OnDisk bool `json:"onDisk"`
	// Status 是给人看的中文结论，界面直接显示，不做任何码值映射。
	Status string `json:"status"`
	Hint   string `json:"hint"`
}

// CheckDependencies 检查这个分片启用的模组声明的依赖。
//
//   - all 是 mods/ 磁盘上的全部模组（mods/ 是全集群共享的，分片只决定开不开）；
//   - enabledDirs 是这个分片 modoverrides.lua 里 enabled = true 的目录名。
//
// 返回按模组目录名排序的问题列表。
func CheckDependencies(all []Mod, enabledDirs []string) []DependencyProblem {
	enabled := make(map[string]bool, len(enabledDirs))
	for _, d := range enabledDirs {
		enabled[d] = true
	}
	out := unmetGroups(all, enabled, func(x Mod) bool { return enabled[x.Dir] })
	for i := range out {
		if out[i].OnDisk {
			out[i].Status = "依赖的模组装在这台机器上，但这个分片没开它"
			out[i].Hint = "引擎不会因为这种写法禁用它（modindex.lua 只看模组存不存在），" +
				"但它要用的那个库没加载进来，实际效果大概率是不生效或报错；" +
				"到分片模组列表把依赖一起打开"
			continue
		}
		out[i].Status = "依赖的模组在这台机器上找不到"
		out[i].Hint = "引擎会把这个模组静默禁用（DisableBecauseBad），服务端照常启动，" +
			"日志里只有一行提示；先把依赖模组下载或上传到 mods/ 再开它"
	}
	return out
}

// DependenciesAbsentFromDisk 只看磁盘：哪些模组声明的依赖在整个 mods/ 里一个候选都对不上。
// 不分分片，也不看启用状态——工坊内容要等服务端启动时才拉，所以在"登记了但还没下载"
// 的阶段这条只是提醒，不能当错误报。
func DependenciesAbsentFromDisk(all []Mod) []DependencyProblem {
	enabled := map[string]bool{}
	for _, x := range all {
		enabled[x.Dir] = true
	}
	out := unmetGroups(all, enabled, func(Mod) bool { return true })
	for i := range out {
		out[i].Status = "依赖的模组还没在这台机器上"
		out[i].Hint = "登记进下载清单后，服务端下次启动才会从工坊拉内容；" +
			"手工上传的本地模组要先把它依赖的那个也放进 mods/"
	}
	return out
}

// unmetGroups 是两种检查共用的部分：逐模组逐依赖组找候选，
// isModInScope 决定"这个模组的依赖要不要查"，enabled 决定"候选算不算已满足"。
// 只有候选既不在启用集合、也不在磁盘上时（OnDisk 为 false）才产出问题；
// OnDisk 为 true 表示磁盘上有但没开，那是另一种可修复状态。
func unmetGroups(all []Mod, enabled map[string]bool, isModInScope func(Mod) bool) []DependencyProblem {
	byDir := make(map[string]Mod, len(all))
	for _, x := range all {
		byDir[x.Dir] = x
	}
	var out []DependencyProblem
	for _, x := range all {
		if !isModInScope(x) || len(x.Dependencies) == 0 {
			continue
		}
		for _, group := range x.Dependencies {
			hit, onDisk := resolveGroup(group, enabled, byDir)
			if hit {
				continue
			}
			out = append(out, DependencyProblem{
				Dir: x.Dir, Name: x.Name, Candidates: group, OnDisk: onDisk,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir < out[j].Dir
		}
		return strings.Join(out[i].Candidates, ",") < strings.Join(out[j].Candidates, ",")
	})
	return out
}

// resolveGroup 判断一组候选标识：命中启用集合返回 hit，只命中磁盘返回 onDisk。
// 候选可能是工坊 id（"123456"）、工坊目录名（"workshop-123456"）或本地目录名，
// 每种都补上 workshop- 前缀再比，避免作者少写前缀就判成缺依赖。
func resolveGroup(group []string, enabled map[string]bool, byDir map[string]Mod) (hit, onDisk bool) {
	for _, raw := range group {
		for _, cand := range dirCandidates(raw) {
			if enabled[cand] {
				return true, false
			}
			if _, ok := byDir[cand]; ok {
				onDisk = true
			}
		}
	}
	return false, onDisk
}

func dirCandidates(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := []string{raw}
	if !strings.HasPrefix(raw, "workshop-") {
		out = append(out, "workshop-"+raw)
	}
	return out
}
