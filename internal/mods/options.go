package mods

// 模组自带配置项的字典：从 modinfo.lua 里读出"这个模组有哪些设置、每项能填什么"。
//
// 为什么这件事必须做在 Go 层而不是让界面直接编辑 configuration_options 的键值：
// 用户面对的要是"背包大小：大 / 中 / 小"，而不是 `size = 2` 这种码值。把码值摊在
// 界面上等于让用户抄别人的作业还抄错——填了模组不认识的数字，引擎不会报错，
// 只是默默按默认值跑（见 modbridge/modoverrides.go：面板也不替它编一个默认值）。
//
// 第三方 modinfo.lua 的写法并不统一，实测存在两族：
//   - configuration_options：专用服务端 modoverrides.lua 认的那份，选项值列表写在 data 里；
//   - settings：客户端模组设置界面用的那份，选项值列表写在 values 里，常带 typing。
// 面板自家的助手 MOD 只有 settings（internal/modbridge/mods/DSTPanelHelper/modinfo.lua），
// 而 override 的键名两边是一致的，所以先读 configuration_options、读不到再退到 settings，
// 并在返回里说明这份字典是从哪来的。
//
// 值列表本身还有三种拼法（{description=,data=} 的数组、纯标量数组、{名字=值} 的键表），
// 这里一律接住；接不住的那些**不猜**，只把项名报进 Skipped，界面退回按原值编辑并说明原因。
// 依据是老规矩：面板不执行 Lua，读不懂的东西不能假装读懂。

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"dstpanel/internal/dst"
)

// Choice 是一个可选值：label 给人看，value 是写进 modoverrides.lua 的那份。
type Choice struct {
	Label string `json:"label"`
	Value any    `json:"value"`
}

// Option 是模组声明的一项设置。
type Option struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Hover string `json:"hover,omitempty"`
	// Kind 决定界面画什么控件：select / toggle / number / text / unknown。
	// unknown 不是错误，是"这个模组这项读不懂"，界面要退回原值编辑器。
	Kind    string   `json:"kind"`
	Choices []Choice `json:"choices,omitempty"`
	// Default 是 modinfo.lua 里写的默认值；不写就是 nil——引擎会用它自己的默认，
	// 面板不替模组编一个。
	Default      any    `json:"default,omitempty"`
	DefaultLabel string `json:"defaultLabel,omitempty"`
}

// Schema 是一个模组的配置项字典。
type Schema struct {
	Dir        string `json:"dir"`
	Name       string `json:"name"`
	ModinfoOK  bool   `json:"modinfoOk"`
	ModinfoErr string `json:"modinfoError,omitempty"`
	// From 是这份字典读自哪张表：configuration_options / settings / none。
	// FromLabel 是同一件事的中文，界面只显示后者。
	From      string   `json:"from"`
	FromLabel string   `json:"fromLabel"`
	Options   []Option `json:"options"`
	// Skipped 是读出形状但认不出的项名，逐条带原因。
	Skipped []string `json:"skipped,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// validDirShape 只挡穿越与非法字符，不挡助手目录——读字典要能读助手 MOD 自己。
func validDirShape(name string) error {
	if !modDirNameRe.MatchString(name) {
		return fmt.Errorf("模组目录名只能是字母、数字、下划线、点或连字符，且不能以点开头（收到 %q）", name)
	}
	return nil
}

// SchemaFor 读某个模组的 modinfo.lua 并给出配置项字典。
//
// 文件读不到时返回错误（那是 404/500 级的事实），但 modinfo.lua 结构读坏只标记不报错：
// 用户这时候仍然需要知道"这个模组在磁盘上、能开关"，只是配置项要手工核对。
func (m *Manager) SchemaFor(dir string) (*Schema, error) {
	if err := validDirShape(dir); err != nil {
		return nil, err
	}
	infoPath := filepath.Join(m.Paths.ModsRoot(), dir, "modinfo.lua")
	raw, err := os.ReadFile(infoPath)
	if err != nil {
		// 引擎自己拉的工坊内容不在 mods/ 下面，同一串 ID 去它的下载区再找一次。
		// 少了这一步，模组卡上"有哪些配置项可改"那一栏会对一个已经加载成功的模组说"读不到"。
		if alt := m.ugcModinfoPath(dir); alt != "" {
			infoPath = alt
			raw, err = os.ReadFile(infoPath)
		}
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("这台机器上没有 %s 的 modinfo.lua（mods/ 和引擎的下载区里都没有），"+
				"面板读不到它的配置项字典（模组可能没下载完或者被手工删过）", dir)
		}
		return nil, fmt.Errorf("读 %s 的 modinfo.lua: %w", dir, err)
	}
	sc := &Schema{Dir: dir, Options: []Option{}, Skipped: []string{}}
	tbl, err := dst.ParseLuaAssignments(string(raw))
	if err != nil {
		sc.Name = dir
		sc.ModinfoErr = "modinfo.lua 结构读坏了（引号或花括号没闭合），面板只能按目录名识别它: " + err.Error()
		sc.From = "none"
		sc.FromLabel = "读不出来"
		sc.Note = "这个模组的 modinfo.lua 解析不了，配置项只能手工核对，界面不猜"
		return sc, nil
	}
	sc.ModinfoOK = true
	sc.Name = luaText(tbl, "name")
	if sc.Name == "" {
		sc.Name = dir
	}

	opts := tbl.Get("configuration_options")
	sc.From = "configuration_options"
	sc.FromLabel = "modinfo.lua 的 configuration_options（专用服务端读的那份）"
	if opts.Kind != dst.KindTable || opts.T == nil || len(opts.T.Values()) == 0 {
		st := tbl.Get("settings")
		if st.Kind == dst.KindTable && st.T != nil && len(st.T.Values()) > 0 {
			opts = st
			sc.From = "settings"
			sc.FromLabel = "modinfo.lua 的 settings（客户端设置界面用的那份，键名与覆盖文件一致）"
		} else if opts.Kind != dst.KindTable || opts.T == nil {
			sc.From = "none"
			sc.FromLabel = "没有配置项字典"
			sc.Note = "这个模组的 modinfo.lua 里没有 configuration_options，也没有 settings：" +
				"它要么没有可调参数，要么把参数写成了面板读不懂的表达式。" +
				"开关照常能用，配置项只能去问模组作者"
			return sc, nil
		}
	}
	sc.Options, sc.Skipped = readOptions(opts.T)
	if len(sc.Options) == 0 && sc.From != "none" {
		sc.Note = "那张表里的项面板一条都没认出来，配置项只能按原值手工编辑"
	}
	return sc, nil
}

// readOptions 逐项解析。返回认出的项和认不出的项名（带原因）。
func readOptions(t *dst.LuaTable) ([]Option, []string) {
	var out []Option
	var skipped []string
	for i, item := range t.Values() {
		if item.Kind != dst.KindTable || item.T == nil {
			skipped = append(skipped, fmt.Sprintf("第 %d 项不是表", i+1))
			continue
		}
		e := item.T
		name := luaText(e, "name")
		if name == "" {
			skipped = append(skipped, fmt.Sprintf("第 %d 项没有 name，写不进覆盖文件", i+1))
			continue
		}
		opt := Option{
			Name:  name,
			Label: firstNonEmpty(luaText(e, "label"), luaText(e, "description"), name),
			Hover: firstNonEmpty(luaText(e, "hover"), luaText(e, "description")),
		}
		choices, choiceErr := choicesOf(firstPresentVal(e, "data", "values"))
		if choiceErr != "" {
			skipped = append(skipped, name+"："+choiceErr)
			continue
		}
		opt.Choices = choices
		def := e.Get("default")
		opt.Default = jsonOf(def)
		typing := luaText(e, "typing")
		opt.Kind, opt.DefaultLabel = kindOf(typing, def, choices)
		if opt.DefaultLabel == "" {
			opt.DefaultLabel = scalarText(def)
		}
		out = append(out, opt)
	}
	return out, skipped
}

// kindOf 决定控件类型。判据顺序是"有没有得选 > 模组自己说了是什么类型 > 默认值像什么"，
// 因为 typing 这个键只有 settings 那族才有，而 data 列表两边都可能带。
func kindOf(typing string, def dst.LuaValue, choices []Choice) (string, string) {
	if len(choices) > 0 {
		label := ""
		for _, c := range choices {
			if sameScalar(c.Value, jsonOf(def)) {
				label = c.Label
				break
			}
		}
		return "select", label
	}
	switch typing {
	case "bool", "boolean":
		return "toggle", ""
	case "number":
		return "number", ""
	case "string", "text":
		return "text", ""
	}
	switch def.Kind {
	case dst.KindBool:
		return "toggle", ""
	case dst.KindNumber:
		return "number", ""
	case dst.KindString:
		return "text", ""
	}
	// 没有候选值、没有 typing、默认值也读不出来：这项只能是模组写成表达式了。
	// 报 unknown 让界面退回原值编辑，比硬挑一个控件然后存进去一个错的类型好。
	return "unknown", ""
}

// choicesOf 把值列表解析成候选项。三种拼法都接，接不住返回原因而不是猜一组值。
func choicesOf(v dst.LuaValue) ([]Choice, string) {
	if v.Kind != dst.KindTable || v.T == nil {
		// 没有值列表不算错误：这就是"自由填写项"（数字、文本、是否）。
		return nil, ""
	}
	t := v.T
	var out []Choice
	for _, item := range t.Values() {
		switch item.Kind {
		case dst.KindTable:
			if item.T == nil {
				continue
			}
			label := firstNonEmpty(luaText(item.T, "description"), luaText(item.T, "label"))
			val := firstPresentVal(item.T, "data", "value")
			if val.Kind == dst.KindInvalid {
				// 这一项没有可写的值位（只有描述），跳过而不是造一个。
				continue
			}
			jv := jsonOf(val)
			if jv == nil {
				continue
			}
			out = append(out, Choice{
				Label: firstNonEmpty(label, scalarText(val), strconv.FormatInt(int64(len(out)+1), 10)),
				Value: jv,
			})
		case dst.KindString, dst.KindNumber, dst.KindBool:
			out = append(out, Choice{Label: scalarText(item), Value: jsonOf(item)})
		}
	}
	if len(out) > 0 {
		return out, ""
	}
	// 数组部分一个都没凑上，再看 { 名字 = 值 } 这种键表写法。
	for _, k := range t.Keys() {
		kv := t.Get(k)
		switch kv.Kind {
		case dst.KindString, dst.KindNumber, dst.KindBool:
			out = append(out, Choice{Label: k, Value: jsonOf(kv)})
		}
	}
	if len(out) > 0 {
		return out, ""
	}
	// 空表是官方模板里 "data = {} 这一项还没实现" 的写法，不是解析失败。
	if t.Len() == 0 {
		return nil, ""
	}
	return nil, "值列表里的每一项都读不出可写进覆盖文件的值"
}

// firstPresentVal 取第一个写了的键（不分值类型）：同一个键名在选项层是表（值列表），
// 在候选项层是标量（要写进覆盖文件的那个值），两边都得用同一个取法。
func firstPresentVal(t *dst.LuaTable, keys ...string) dst.LuaValue {
	for _, k := range keys {
		if v := t.Get(k); v.Kind != dst.KindInvalid {
			return v
		}
	}
	return dst.LuaValue{}
}

// jsonOf 把 Lua 标量转成 JSON 能直接编码的 Go 类型；表和其他一律 nil。
// 界面拿这个值原样提交回来，modbridge.luaFromJSON 再转回 Lua，两边只有一种标量口径。
func jsonOf(v dst.LuaValue) any {
	switch v.Kind {
	case dst.KindString:
		return v.S
	case dst.KindNumber:
		return v.N
	case dst.KindBool:
		return v.B
	}
	return nil
}

func scalarText(v dst.LuaValue) string {
	switch v.Kind {
	case dst.KindString:
		return v.S
	case dst.KindNumber:
		return numText(v.N)
	case dst.KindBool:
		if v.B {
			return "是"
		}
		return "否"
	}
	return ""
}

// numText 与 luaText 的数字分支同一口径：整数不显示 ".0"，也不裁尾零
// （version = 20 裁成 "2" 那种错已经踩过一次）。
func numText(n float64) string {
	if n == math.Trunc(n) && math.Abs(n) < 1e15 {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

func sameScalar(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	if af, ok := a.(float64); ok {
		if bf, ok := b.(float64); ok {
			return af == bf
		}
	}
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return as == bs
		}
	}
	if ab, ok := a.(bool); ok {
		if bb, ok := b.(bool); ok {
			return ab == bb
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
