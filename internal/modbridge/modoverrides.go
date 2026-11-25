package modbridge

// 分片级 modoverrides.lua 的通用读写。
//
// 助手模组有专门的 SetHelperOverride（它管的是固定三项），这里是给第三方模组用的：
// 面板不能假设别人的 configuration_options 长什么样，只能"保留没动的键、覆盖用户改的键"。
// 专用服务端启动时会 DisableAllMods，模组能不能生效全看这个文件里的条目，
// 所以"文件装好了"和"这个分片开了"是两件事，界面必须分开表达。

import (
	"fmt"
	"math"

	"dstpanel/internal/dst"
)

// OverrideSummary 是某个模组在某个分片上的覆盖状态。
type OverrideSummary struct {
	Dir     string
	Present bool
	Enabled bool
	// Priority 为空表示条目里没写，引擎按默认处理，面板不要显示成 0。
	Priority *float64
	Options  map[string]any
	// NestedOptions 记录没展开显示的嵌套键个数，避免用户以为面板能编辑它们。
	NestedOptions []string
}

// ListOverrides 读分片的 modoverrides.lua，返回所有条目。
// 文件不存在返回空列表——没覆盖过任何模组是合法状态，不是错误。
func (b *Bridge) ListOverrides(cluster, shard string) ([]OverrideSummary, error) {
	tbl, err := b.loadOverrides(b.Paths.ModOverrides(cluster, shard))
	if err != nil {
		return nil, err
	}
	var out []OverrideSummary
	for _, key := range tbl.SortedKeys() {
		v := tbl.Get(key)
		if v.Kind != dst.KindTable || v.T == nil {
			// 不是表就不是合法条目。这里不删也不报错：这文件可能是用户手写的，
			// 面板只做能看懂的部分，看不懂的留着。
			continue
		}
		s := OverrideSummary{Dir: key, Present: true, Options: map[string]any{}}
		if e := v.T.Get("enabled"); e.Kind == dst.KindBool {
			s.Enabled = e.B
		}
		if p := v.T.Get("priority"); p.Kind == dst.KindNumber {
			val := p.N
			s.Priority = &val
		}
		if opts := v.T.Get("configuration_options"); opts.Kind == dst.KindTable && opts.T != nil {
			for _, ok := range opts.T.Keys() {
				ov := opts.T.Get(ok)
				switch ov.Kind {
				case dst.KindString:
					s.Options[ok] = ov.S
				case dst.KindNumber:
					s.Options[ok] = ov.N
				case dst.KindBool:
					s.Options[ok] = ov.B
				default:
					s.NestedOptions = append(s.NestedOptions, ok)
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// OverridePatch 是一次局部修改：nil 的字段一律不碰。
type OverridePatch struct {
	Enabled  *bool
	Priority *float64
	// Options 按键覆盖；键值为 nil 表示删掉这个键，交回模组默认值。
	Options map[string]any
}

// Validate 把"用户给的东西不合法"和"文件读写失败"分开：前者接口要回 400，
// 后者才是面板的故障。规则放在这里而不是接口层，是为了只有一个出处。
func (p OverridePatch) Validate() error {
	// priority 在引擎侧只是个排序键（scripts/mods.lua:491 sanitizepriority + :518 降序排），
	// 没有范围限制，负数合法；真正会坏事的是 NaN/Inf——它们渲染出来的字面量 Lua 读不回来。
	if p.Priority != nil && (math.IsNaN(*p.Priority) || math.IsInf(*p.Priority, 0)) {
		return fmt.Errorf("priority 得是有限数字，收到的是 %v", *p.Priority)
	}
	for k, v := range p.Options {
		if v == nil {
			continue
		}
		if _, err := luaFromJSON(v); err != nil {
			return fmt.Errorf("配置项 %s：%w", k, err)
		}
	}
	return nil
}

// PatchOverride 改某个模组条目。模组条目不存在时按 patch 里给出的字段新建，
// 没给的字段留空——引擎会读 modinfo.lua 里的 default，面板不替它编一个。
func (b *Bridge) PatchOverride(cluster, shard, dir string, p OverridePatch) error {
	if err := p.Validate(); err != nil {
		return err
	}
	path := b.Paths.ModOverrides(cluster, shard)
	tbl, err := b.loadOverrides(path)
	if err != nil {
		return err
	}

	entry := dst.NewTable()
	if old := tbl.Get(dir); old.Kind == dst.KindTable && old.T != nil {
		for _, k := range old.T.Keys() {
			entry.Set(k, old.T.Get(k))
		}
	}
	if p.Enabled != nil {
		entry.Set("enabled", dst.Bool(*p.Enabled))
	}
	if p.Priority != nil {
		entry.Set("priority", dst.Number(*p.Priority))
	}
	if p.Options != nil {
		opts := dst.NewTable()
		if old := entry.Get("configuration_options"); old.Kind == dst.KindTable && old.T != nil {
			for _, k := range old.T.Keys() {
				opts.Set(k, old.T.Get(k))
			}
		}
		for k, v := range p.Options {
			if v == nil {
				opts.Delete(k)
				continue
			}
			lv, err := luaFromJSON(v)
			if err != nil {
				return fmt.Errorf("模组 %s 的配置项 %s：%w", dir, k, err)
			}
			opts.Set(k, lv)
		}
		entry.Set("configuration_options", dst.Table(opts))
	}
	if entry.Len() == 0 {
		// patch 全空且原本也没有这个条目：不写文件，避免生成一个空壳条目。
		return nil
	}
	tbl.Set(dir, dst.Table(entry))

	if rendered := dst.Render(tbl); rendered == normalizeEOL(existingOrEmpty(path)) {
		return nil
	}
	return dst.SaveLua(path, tbl)
}

// luaFromJSON 把 JSON 解码出来的值转成 Lua 值。只收标量：
// 第三方模组的设置项在 modinfo.lua 里就只有 string / number / boolean 三种。
// int 也收：调用方不只有 HTTP 层（JSON 给的是 float64），Go 侧直接传字面量也要能用。
func luaFromJSON(v any) (dst.LuaValue, error) {
	switch x := v.(type) {
	case string:
		return dst.String(x), nil
	case bool:
		return dst.Bool(x), nil
	case int:
		return dst.Number(float64(x)), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return dst.LuaValue{}, fmt.Errorf("数值不能是 NaN 或无穷大")
		}
		return dst.Number(x), nil
	case nil:
		return dst.LuaValue{}, fmt.Errorf("配置项不能是 null")
	default:
		// 这里不能写 %T：那会打出 map[string]interface {}，界面上一出现 Go 的类型名，
		// 用户就只能去猜是面板坏了还是自己填错了。
		return dst.LuaValue{}, fmt.Errorf("配置项只能是文本、数字或是否，不能再套一层或写成数组")
	}
}
