package dst

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed assets/world_setting.json
var worldSettingAssets embed.FS

// 世界生成设定字典：把 leveldataoverride.lua 里 overrides 的码值翻译成中文标签，
// 并提供每个条目的合法取值集合。面板靠它渲染下拉框并在写回前拦住非法取值——
// DST 对认不出的取值不报错，只会静默按默认生成，错一个字母就是一张"看着不对"的世界图。
type WorldSchema struct {
	locs map[string]map[string]Location // lang -> location -> Location
}

type Location struct {
	Groups []SettingGroup
}

type SettingGroup struct {
	Key   string        `json:"key"`
	Text  string        `json:"text"`
	Order int           `json:"-"`
	Items []SettingItem `json:"items"`
}

type SettingItem struct {
	Key     string          `json:"key"`
	Text    string          `json:"text"`
	Default string          `json:"default"`
	Options []SettingOption `json:"options"`
	Order   int             `json:"-"`
}

type SettingOption struct {
	Value string `json:"value"`
	Text  string `json:"text"`
}

type settingDoc struct {
	Zh jsonLang `json:"zh"`
	En jsonLang `json:"en"`
}

type jsonLang map[string]jsonLocation

type jsonLocation struct {
	Groups []jsonGroup `json:"groups"`
}

type jsonGroup struct {
	Key   string     `json:"key"`
	Text  string     `json:"text"`
	Order int        `json:"order"`
	Items []jsonItem `json:"items"`
}

type jsonItem struct {
	Key     string          `json:"key"`
	Text    string          `json:"text"`
	Default string          `json:"default"`
	Order   int             `json:"order"`
	Options []SettingOption `json:"options"`
}

const settingOrderUnset = 1000000

func LoadWorldSchema() (*WorldSchema, error) {
	raw, err := worldSettingAssets.ReadFile("assets/world_setting.json")
	if err != nil {
		return nil, fmt.Errorf("读取世界规则字典: %w", err)
	}
	var doc settingDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析世界规则字典: %w", err)
	}
	s := &WorldSchema{locs: map[string]map[string]Location{
		"zh": buildLang(doc.Zh),
		"en": buildLang(doc.En),
	}}
	if len(s.locs["zh"]) == 0 {
		return nil, fmt.Errorf("世界规则字典里没有中文数据")
	}
	return s, nil
}

func buildLang(in jsonLang) map[string]Location {
	out := make(map[string]Location, len(in))
	for name, loc := range in {
		groups := make([]SettingGroup, 0, len(loc.Groups))
		for _, g := range loc.Groups {
			sg := SettingGroup{Key: g.Key, Text: g.Text, Order: orderOrLast(g.Order)}
			for _, it := range g.Items {
				si := SettingItem{Key: it.Key, Text: it.Text, Default: it.Default,
					Order: orderOrLast(it.Order), Options: it.Options}
				sg.Items = append(sg.Items, si)
			}
			groups = append(groups, sg)
		}
		sort.Slice(groups, func(i, j int) bool {
			if groups[i].Order != groups[j].Order {
				return groups[i].Order < groups[j].Order
			}
			return groups[i].Key < groups[j].Key
		})
		out[name] = Location{Groups: groups}
	}
	return out
}

// orderOrLast：Klei 的表里缺省 order 表示"没排过序"，0 反而合法，所以用大数兜底。
func orderOrLast(o int) int {
	if o == 0 {
		return settingOrderUnset
	}
	return o
}

// Location 取某地形（forest/cave）的条目表；lang 没有时退回中文。
func (s *WorldSchema) Location(name, lang string) (Location, bool) {
	if l, ok := s.locs[lang]; ok {
		if loc, ok := l[name]; ok {
			return loc, true
		}
	}
	loc, ok := s.locs["zh"][name]
	return loc, ok
}

func (loc Location) Item(key string) (SettingItem, bool) {
	for _, g := range loc.Groups {
		for _, it := range g.Items {
			if it.Key == key {
				return it, true
			}
		}
	}
	return SettingItem{}, false
}

// Item 是按地形名直接查一个条目，省掉调用方先取 Location 再取 Item 的两步。
// 第二返回值是"这个地形有这一项吗"——面板据此决定要不要把它写进世界文件。
func (s *WorldSchema) Item(location, lang, key string) (SettingItem, bool) {
	loc, ok := s.Location(location, lang)
	if !ok {
		return SettingItem{}, false
	}
	return loc.Item(key)
}

// Overrides 读出 worldgenoverride.lua 中 overrides 表的标量取值。
// 嵌套表（map_options）与数组（required_setpieces）不在面板编辑范围，原样保留。
func Overrides(world *LuaTable) map[string]string {
	v := world.Get("overrides")
	if v.Kind != KindTable || v.T == nil {
		return map[string]string{}
	}
	out := make(map[string]string)
	for _, k := range v.T.Keys() {
		switch val := v.T.Get(k); val.Kind {
		case KindString:
			out[k] = val.S
		case KindNumber:
			out[k] = formatNumber(val.N)
		}
	}
	return out
}

// DefaultOverrides 给出该地形的整套默认取值，新建世界时用。
func (s *WorldSchema) DefaultOverrides(location, lang string) map[string]string {
	loc, ok := s.Location(location, lang)
	if !ok {
		return map[string]string{}
	}
	out := make(map[string]string)
	for _, g := range loc.Groups {
		for _, it := range g.Items {
			out[it.Key] = it.Default
		}
	}
	return out
}

// WorldProblem 是一条校验结论，交给界面标注而不是把整份提交丢掉。
type WorldProblem struct {
	Key    string `json:"key"`
	Level  string `json:"level"` // error / warn
	Reason string `json:"reason"`
}

// CheckOverrides：
// 字典里有的条目给了表外取值 => error（下拉框选不出来，多半是手工输入打错）；
// 字典里没有的键 => warn（模组会往 overrides 里加自己的条目，不能因此拒收）。
func (s *WorldSchema) CheckOverrides(location, lang string, patch map[string]string) []WorldProblem {
	var problems []WorldProblem
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	loc, ok := s.Location(location, lang)
	if !ok {
		if len(keys) > 0 {
			problems = append(problems, WorldProblem{Level: "warn",
				Reason: fmt.Sprintf("未知地形 %q，跳过取值校验", location)})
		}
		return problems
	}

	for _, k := range keys {
		it, known := loc.Item(k)
		if !known {
			problems = append(problems, WorldProblem{Key: k, Level: "warn", Reason: "不在官方世界规则表中"})
			continue
		}
		val := patch[k]
		if val == "" {
			problems = append(problems, WorldProblem{Key: k, Level: "error", Reason: "取值不能为空"})
			continue
		}
		found := false
		for _, o := range it.Options {
			if o.Value == val {
				found = true
				break
			}
		}
		if !found {
			problems = append(problems, WorldProblem{Key: k, Level: "error",
				Reason: fmt.Sprintf("取值 %q 不在「%s」的可选范围内", val, it.Text)})
		}
	}
	return problems
}

// ApplyOverrides 把 patch 写进 world 的 overrides 表；有 error 级问题时整体不写。
func (s *WorldSchema) ApplyOverrides(world *LuaTable, location, lang string, patch map[string]string) error {
	var errs []string
	for _, p := range s.CheckOverrides(location, lang, patch) {
		if p.Level == "error" {
			errs = append(errs, p.Key+": "+p.Reason)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("世界规则校验未通过: %s", strings.Join(errs, "; "))
	}
	ov := world.EnsureNested("overrides")
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ov.Set(k, String(patch[k]))
	}
	return nil
}
