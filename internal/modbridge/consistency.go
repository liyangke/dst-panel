package modbridge

// 跨分片的 modoverrides.lua 一致性校验。
//
// 为什么面板必须自己做这件事：把整个 scripts 包 grep 一遍，引擎**没有**任何
// "比较两个分片加载了哪些模组"的代码（分片之间只同步世界设置，见日志里的
// [SyncWorldSettings]）。它只在玩家进服时校验客户端与服务端的模组清单
// （networking.lua:351/411/442 的缺模组与版本不匹配断线分支）。
// 所以"地表开了、洞穴没开"不会被引擎拒绝，只会安静地开出两个世界规则不同的服：
// 玩家穿过传送门到洞穴那边，模组加的物品与配置都不存在。这种错最难查，
// 因为两边都"正常运行"。

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"dstpanel/internal/dst"
)

// ShardMods 是某个分片加载模组这件事的一张体检表。
type ShardMods struct {
	Shard string `json:"shard"`
	// Enabled 是这个分片上真正会加载的模组目录名（有条目且 enabled = true）。
	Enabled []string `json:"enabled"`
	// ClientModsDisabled 对应 modoverrides.lua 里的顶层键 client_mods_disabled。
	// 引擎在 ModIndex:ApplyEnabledOverrides（modindex.lua:433）里读它并 DisableClientMods，
	// 它不是一个模组条目，所以 ListOverrides 看不到它，这里单独读。
	// nil 表示两份文件里都没写这个键。
	ClientModsDisabled *bool `json:"clientModsDisabled,omitempty"`
	// Present 表示这个分片到底有没有 modoverrides.lua。没有也合法（一个模组都不开）。
	Present bool `json:"present"`
}

// ModIssue 是某个模组在各分片之间的不一致。
type ModIssue struct {
	Dir string `json:"dir"`
	// Kind 与 Detail 都是给人看的中文，界面直接显示，不经过任何映射表。
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	// ByShard 逐个分片说明它那边的状态，用户不用自己开文件比对。
	ByShard map[string]string `json:"byShard"`
	// Fix 是面板能替用户做的那一步；为空表示只能手工核对。
	Fix string `json:"fix,omitempty"`
}

// ConsistencyReport 是一个集群的跨分片校验结果。
type ConsistencyReport struct {
	Cluster string      `json:"cluster"`
	Shards  []ShardMods `json:"shards"`
	Issues  []ModIssue  `json:"issues"`
	// Note 在"没有任何不一致"时也要给一句人话，否则界面只能显示空白。
	Note string `json:"note"`
}

// CompareShardOverrides 比对一个集群里各分片的 modoverrides.lua。
// 分片少于两个时不算错误：单分片集群本来就没有一致性可言，返回空问题表并说明原因。
func (b *Bridge) CompareShardOverrides(cluster string, shards []string) (*ConsistencyReport, error) {
	rpt := &ConsistencyReport{Cluster: cluster, Shards: []ShardMods{}, Issues: []ModIssue{}}
	if len(shards) < 2 {
		rpt.Note = "这个集群只有一个世界，不存在跨世界的模组一致性问题"
		return rpt, nil
	}
	byShard := make(map[string]map[string]OverrideSummary, len(shards))
	for _, sh := range shards {
		list, err := b.ListOverrides(cluster, sh)
		if err != nil {
			return nil, err
		}
		m := make(map[string]OverrideSummary, len(list))
		for _, s := range list {
			m[s.Dir] = s
		}
		byShard[sh] = m
		view := ShardMods{Shard: sh, Enabled: []string{}, Present: true}
		if _, err := os.Stat(b.Paths.ModOverrides(cluster, sh)); err != nil {
			view.Present = false
		}
		for _, s := range list {
			if s.Enabled {
				view.Enabled = append(view.Enabled, s.Dir)
			}
		}
		sort.Strings(view.Enabled)
		if v := readClientModsDisabled(b.Paths.ModOverrides(cluster, sh)); v != nil {
			view.ClientModsDisabled = v
		}
		rpt.Shards = append(rpt.Shards, view)
	}

	// 取所有分片出现过的模组目录名的并集，才能保证每个模组只被检查一次。
	dirs := map[string]bool{}
	for _, m := range byShard {
		for d := range m {
			dirs[d] = true
		}
	}
	sorted := make([]string, 0, len(dirs))
	for d := range dirs {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted)

	for _, dir := range sorted {
		states := make(map[string]string, len(shards))
		for _, sh := range shards {
			s, ok := byShard[sh][dir]
			switch {
			case !ok:
				states[sh] = "没有条目（等于不开）"
			case s.Enabled:
				states[sh] = "开着"
			default:
				states[sh] = "有条目但 enabled = false（明确不开）"
			}
		}
		// 引擎在专用服务端启动时先 DisableAllMods，再只按 enabled 开模组，
		// 所以"没有条目"和"enabled = false"在生效结果上是同一件事，不能算不一致。
		anyOn, anyOff := false, false
		for _, sh := range shards {
			if states[sh] == "开着" {
				anyOn = true
			} else {
				anyOff = true
			}
		}
		if anyOn && anyOff {
			var on, off []string
			for _, sh := range shards {
				if states[sh] == "开着" {
					on = append(on, sh)
				} else {
					off = append(off, sh)
				}
			}
			rpt.Issues = append(rpt.Issues, ModIssue{
				Dir: dir, Kind: "启用状态不一致",
				Detail:  fmt.Sprintf("%s 开了这个模组，%s 没开", strings.Join(on, "、"), strings.Join(off, "、")),
				ByShard: states,
				Fix:     "到没开的那个世界的模组页把它打开（或反过来关掉），两边一致即可",
			})
			continue
		}
		if issue := compareEntryOptions(shards, byShard, dir, states); issue != nil {
			rpt.Issues = append(rpt.Issues, *issue)
		}
	}

	// client_mods_disabled 是全局开关：两边不一样时，一个分片放行客户端模组、另一个不放行。
	if issue := compareClientModsDisabled(shards, rpt.Shards); issue != nil {
		rpt.Issues = append(rpt.Issues, *issue)
	}
	if len(rpt.Issues) == 0 {
		rpt.Note = "各世界加载的模组与配置项一致，玩家在地表与洞穴之间往返不会碰到规则不同的世界"
	}
	return rpt, nil
}

// compareEntryOptions 比对都开了（或都不开）时，配置项与 priority 是否一致。
func compareEntryOptions(shards []string, byShard map[string]map[string]OverrideSummary,
	dir string, states map[string]string) *ModIssue {
	options := map[string]map[string]string{}
	nested := map[string][]string{}
	priorities := map[string]string{}
	for _, sh := range shards {
		s, ok := byShard[sh][dir]
		if !ok {
			continue
		}
		if len(s.Options) > 0 {
			flat := make(map[string]string, len(s.Options))
			for k, v := range s.Options {
				flat[k] = luaishValue(v)
			}
			options[sh] = flat
		}
		// 每片都要占一个键，空列表表示"这边一个嵌套表都没有"。
		nested[sh] = append([]string{}, s.NestedOptions...)
		sort.Strings(nested[sh])
		if s.Priority != nil {
			priorities[sh] = fmt.Sprintf("%v", *s.Priority)
		}
	}
	if diff := firstOptionDiff(shards, options); diff != "" {
		return &ModIssue{
			Dir: dir, Kind: "配置项不一致", Detail: diff, ByShard: states,
			Fix: "两边都用同一个配置值；面板的世界模组页可以逐项写回去",
		}
	}
	// 嵌套表面板不展开，只能比"两边有没有同名的嵌套键"；键名对得上时值对不对仍要靠人。
	if diff := firstNestedDiff(shards, nested); diff != "" {
		return &ModIssue{
			Dir: dir, Kind: "有面板读不懂的配置项",
			Detail:  diff,
			ByShard: states,
		}
	}
	if len(priorities) > 1 && !allSame(priorities) {
		return &ModIssue{
			Dir: dir, Kind: "priority 写法不同",
			Detail: "两边写的 priority 不一样。这个版本的引擎排序只看模组自己 modinfo.lua 里的 priority，" +
				"modoverrides.lua 里这个键不参与排序，所以不一致不会改变加载顺序，可以不管",
			ByShard: states,
		}
	}
	return nil
}

// compareClientModsDisabled 比对各分片的 client_mods_disabled 顶层键。
func compareClientModsDisabled(shards []string, views []ShardMods) *ModIssue {
	vals := map[string]string{}
	states := map[string]string{}
	for _, v := range views {
		switch {
		case v.ClientModsDisabled == nil:
			vals[v.Shard] = "没写"
			states[v.Shard] = "没写（客户端模组照常允许）"
		case *v.ClientModsDisabled:
			vals[v.Shard] = "true"
			states[v.Shard] = "客户端模组被整体关掉"
		default:
			vals[v.Shard] = "false"
			states[v.Shard] = "明确允许客户端模组"
		}
	}
	if !allSame(vals) {
		return &ModIssue{
			Dir: "client_mods_disabled", Kind: "客户端模组总开关不一致",
			Detail: "这个顶层键不是模组条目，它决定整个世界允不允许加载客户端模组；" +
				"两边不同会让同一个玩家在地表和洞穴看到的能力不一样",
			ByShard: states,
		}
	}
	return nil
}

// readClientModsDisabled 读 modoverrides.lua 的顶层键 client_mods_disabled。
// 这个键不是模组条目，ListOverrides 会跳过它（值不是表），所以这里单独取。
// 返回 nil 表示"文件里没有这个键"——引擎效果和不写一样，但两个分片一个写 false
// 一个不写时，说明有人在其中一边动过总开关，值得提示。
func readClientModsDisabled(path string) *bool {
	tbl, err := dst.ParseLuaTable(existingOrEmpty(path))
	if err != nil {
		return nil
	}
	if v := tbl.Get("client_mods_disabled"); v.Kind == dst.KindBool {
		out := v.B
		return &out
	}
	return nil
}

// firstNestedDiff 找出第一个"只在部分分片出现"的嵌套配置表键名。
// 面板读得了键名读不了值，所以这种差异只能报"读不懂"，不能假装比过。
func firstNestedDiff(shards []string, nested map[string][]string) string {
	names := map[string]bool{}
	for _, sh := range shards {
		for _, k := range nested[sh] {
			names[k] = true
		}
	}
	list := make([]string, 0, len(names))
	for k := range names {
		list = append(list, k)
	}
	sort.Strings(list)
	for _, k := range list {
		var has, missing []string
		for _, sh := range shards {
			if hasNestedKey(nested[sh], k) {
				has = append(has, sh)
			} else {
				missing = append(missing, sh)
			}
		}
		if len(missing) > 0 {
			return fmt.Sprintf("配置项 %s 是面板读不懂的嵌套表：%s 那边有、%s 那边没有，"+
				"面板比不了它里面的值，只能手工核对",
				k, strings.Join(has, "、"), strings.Join(missing, "、"))
		}
	}
	return ""
}

func hasNestedKey(list []string, key string) bool {
	for _, k := range list {
		if k == key {
			return true
		}
	}
	return false
}

func allSame(m map[string]string) bool {
	first, seen := "", false
	for _, v := range m {
		if seen && v != first {
			return false
		}
		first, seen = v, true
	}
	return true
}

// firstOptionDiff 找出第一个各分片取值不同的配置项名字，返回那句人话。
func firstOptionDiff(shards []string, options map[string]map[string]string) string {
	names := map[string]bool{}
	for _, m := range options {
		for k := range m {
			names[k] = true
		}
	}
	if len(names) == 0 {
		return ""
	}
	list := make([]string, 0, len(names))
	for k := range names {
		list = append(list, k)
	}
	sort.Strings(list)
	for _, k := range list {
		vals := map[string]string{}
		for _, sh := range shards {
			if m, ok := options[sh]; ok {
				if v, ok := m[k]; ok {
					vals[sh] = v
					continue
				}
			}
			vals[sh] = "没写"
		}
		if !allSame(vals) {
			parts := make([]string, 0, len(shards))
			for _, sh := range shards {
				parts = append(parts, sh+"="+vals[sh])
			}
			return "配置项 " + k + " 各世界取值不同：" + strings.Join(parts, "，")
		}
	}
	return ""
}

// luaishValue 把配置项的值渲染成文件里那种写法，方便用户拿去和 modinfo.lua 对照。
func luaishValue(v any) string {
	switch x := v.(type) {
	case string:
		return `"` + x + `"`
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
	default:
		return fmt.Sprintf("%v", v)
	}
}
