// 世界生成文件的写入。这一层存在的理由全是 2026-10-30/31 在云机（v760944）上
// 拿真启动日志撞出来的，依据都在引擎自带脚本里（data/databundles/scripts.zip）：
//
//  1. leveldataoverride.lua 是 GAME USE 文件，shardindex.lua:509 的注释原话是
//     "It contains a _complete level definition_"。它由 RunInSandboxSafe 加载
//     （util.lua:810，setfenv 成空表），文件里连 require、table 都用不了，
//     只能是一份纯字面量。map/level.lua:32-34 直接从这张表取 required_setpieces /
//     numrandom_set_pieces / random_set_pieces：手写不全时不会得到格式错误，
//     而是世界生成死在必需物件上——实测两种死法，
//     "PANIC: missing required prefab [sculpture_bishop]"（缺 required_setpieces）和
//     level.lua:172 "attempt to get length of field 'random_set_pieces'"（只补齐前两项）。
//     两种都是世界没生成、进程和 screen 会话照样活着。
//  2. worldgenoverride.lua 是 USER USE 文件（shardindex.lua:411 GetWorldgenOverride）：
//     顶层只认 overrides / preset / settings_preset / worldgen_preset / override_enabled。
//     给了 preset，引擎自己 require("map/levels") 取 GetDataForLevelID，
//     把 locations.lua 的地形内生项和 levels/forest.lua 的预设合并成完整世界定义，
//     再把这份文件里的 overrides 深合并进去（presetdata = MergeMapsDeep(presetdata, savedata)）。
//     面板走的就是这条路：格式面小、跟装机版本走、不把 Klei 的数据抄进仓库。
//
// 实测对照（.tmp_boot/wgab/）：同样 11 项定制，只写 worldgenoverride.lua
// （preset=SURVIVAL_TOGETHER）→ 50 秒生完世界、助手 MOD 回传快照；
// 写成 leveldataoverride.lua → 生成失败，日志里只有上面那两句话。
//
// 用这条路的代价要说清楚：世界名与世界说明只有 leveldataoverride.lua 能带，
// 走 preset 就用预设自带的名字。服务端列表上玩家看到的是集群名与分片名
// （cluster.ini / server.ini，面板写、助手 MOD 也把它们回传），不是世界定义里的
// name，所以面板的可辨识度没有损失。
package dst

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// WorldSpec 是一份要写进 worldgenoverride.lua 的世界生成请求。
//
// Overrides 允许只放用户改过的那几项：customize.lua 里
// option.default = locationdata.overrides[option.name]，表里没有的键退回预设值。
// 面板为了便于手工排查会写全整套默认值，但这一层不强迫调用方写全。
type WorldSpec struct {
	// Preset 是引擎预设的 id（见 worldPresets）。留空由 DefaultPreset 补该地形的默认预设。
	Preset    string
	Overrides map[string]string
	// EngineRewritten 表示这份文件是引擎重写后留下的那一份，不是面板写出去的：
	// 每次存世界（自动存档、c_save()、停服）引擎都会按当前存档把它重写一遍——
	// 加 "KLEI     1 " 头、把 preset 展开成 worldgen_preset + settings_preset、
	// overrides 收缩成存档里带着的那些项。读回来时要能区分，
	// 否则界面会以为"设定被人删光了"，而 PUT 会因为认不出预设而永久 409。
	EngineRewritten bool
}

// WorldPreset 是引擎自带的一个世界预设。
//
// 这张表首先是白名单：preset 会原样写进 Lua 交给 GetDataForLevelID 查，
// 查不到时引擎的动作是"忽略它、照样套用 overrides"（日志里一句 nonexistent preset），
// 于是用户以为自己选了某个预设、生出来的却是另一个——所以宁可当场拒掉。
type WorldPreset struct {
	ID       string
	Location string
	// Text 是面板给的中文说法；English 是引擎字符串表（strings.lua:12078 PRESETLEVELS）
	// 里的原文，界面上作为对照，免得用户拿着中文去搜社区帖子对不上。
	Text    string
	English string
	Note    string
}

var worldPresets = []WorldPreset{
	{"SURVIVAL_TOGETHER", LocationForest, "标准森林", "Survival",
		"2026-10-31 云机实测：只写 worldgenoverride.lua 时 50 秒生成完成"},
	{"SURVIVAL_TOGETHER_CLASSIC", LocationForest, "经典森林（无巨人的国度）", "No Giants Here", ""},
	{"DST_CAVE", LocationCave, "标准洞穴", "The Caves", ""},
	{"DST_CAVE_PLUS", LocationCave, "加强洞穴", "Caves Plus", ""},
	{"TERRARIA_CAVE", LocationCave, "泰拉瑞亚洞穴", "Caves of Terraria", ""},
}

// PresetsForLocation 给出某个地形可用的预设，顺序即界面下拉框顺序（默认值在最前）。
func PresetsForLocation(location string) []WorldPreset {
	out := make([]WorldPreset, 0, 3)
	for _, p := range worldPresets {
		if p.Location == location {
			out = append(out, p)
		}
	}
	return out
}

// DefaultPreset 是这个地形的默认预设。
func DefaultPreset(location string) string {
	if ps := PresetsForLocation(location); len(ps) > 0 {
		return ps[0].ID
	}
	return ""
}

// PresetLabel 把码值翻成中文；字典里没有这个预设时原样返回，让界面看得见。
func PresetLabel(id string) string {
	for _, p := range worldPresets {
		if p.ID == id {
			return p.Text
		}
	}
	return id
}

// 两个地形名是引擎认的，不是面板发明的：Master 用 forest，Caves 用 cave。
// 分片和地形必须配对，配错的后果不是报错而是"洞穴连不上地表"这种更难查的现象。
const (
	LocationForest = "forest"
	LocationCave   = "cave"
)

// ShardLocation 给出这个分片应该用的地形。
func ShardLocation(shard string) string {
	if shard == ShardCaves {
		return LocationCave
	}
	return LocationForest
}

// ShardForLocation 是反方向的查表：界面拿的是地形，要显示"这一张表归哪个分片"。
func ShardForLocation(location string) string {
	if location == LocationCave {
		return ShardCaves
	}
	return ShardMaster
}

// shardZh 给错误文案用。引擎文件里只有 forest/cave 两个码值，
// 面向用户的这句话得说人话。
func shardZh(shard string) string {
	if shard == ShardCaves {
		return "洞穴"
	}
	return "地表"
}

// checkWorldGen 把"引擎会静默忽略的东西"提前变成一条用户能照做的中文。
func checkWorldGen(shard string, w WorldSpec) error {
	location := ShardLocation(shard)
	if !isKnownPreset(location, w.Preset) {
		return fmt.Errorf("%w: 世界预设 %q 不是 %s 世界（%s地形）可用的预设，可选 %s；"+
			"这个名字引擎不会报错，只会静默忽略掉再按另一个预设生成",
			ErrInvalidArg, w.Preset, shard, shardZh(shard), PresetChoicesText(location))
	}
	if w.Overrides == nil {
		return fmt.Errorf("%w: overrides 表不能是空的（哪怕是空表也要写出来，引擎见到 nil 就整个作废）", ErrInvalidArg)
	}
	for k := range w.Overrides {
		if !isLuaKey(k) {
			return fmt.Errorf("%w: 世界规则键名 %q 不是字母/数字/下划线，不能写进 Lua", ErrInvalidArg, k)
		}
	}
	return nil
}

// IsKnownPreset 给接口层用：读回来的文件里 preset 是手工改的时，
// 界面要能判断"这个预设归不属于这个分片的地形"。
func IsKnownPreset(location, id string) bool {
	return isKnownPreset(location, id)
}

// PresetByID 按 id 取整条预设记录（界面把默认预设单独渲染时要它的中文说法）。
func PresetByID(id string) (WorldPreset, bool) {
	for _, p := range worldPresets {
		if p.ID == id {
			return p, true
		}
	}
	return WorldPreset{}, false
}

func isKnownPreset(location, id string) bool {
	for _, p := range PresetsForLocation(location) {
		if p.ID == id {
			return true
		}
	}
	return false
}

// PresetChoicesText 给错误文案里的可选项。这里出现码值是必要的：
// 用户要往文件里填的就是这个字符串，只给中文名他照着填不出来。
func PresetChoicesText(location string) string {
	ps := PresetsForLocation(location)
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, fmt.Sprintf("%s（%s）", p.ID, p.Text))
	}
	return strings.Join(ids, "、")
}

// isLuaKey 限定键名形态：这些键要作为 Lua 的标识符写出去，
// 放任意字符串进去等于让用户提供的文本变成文件结构。
func isLuaKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return !(k[0] >= '0' && k[0] <= '9')
}

// WriteWorldGen 原子写出 worldgenoverride.lua（覆盖前留 .bak，和这一层其它文件一样）。
func (p Paths) WriteWorldGen(cluster, shard string, w WorldSpec) error {
	if w.Preset == "" {
		w.Preset = DefaultPreset(ShardLocation(shard))
	}
	if err := checkWorldGen(shard, w); err != nil {
		return err
	}
	tbl := NewTable()
	// 这一句是开关：引擎见到 override_enabled 不为真就直接 "Found world gen overrides
	// but not enabled"，整份文件当没有。
	tbl.Set("override_enabled", Bool(true))
	tbl.Set("preset", String(w.Preset))
	ov := NewTable()
	keys := make([]string, 0, len(w.Overrides))
	for k := range w.Overrides {
		keys = append(keys, k)
	}
	// 排序后再写：同一份设定两次生成的文件必须逐字节一致，
	// 否则每次保存都会产生一个"看起来改了东西"的 .bak。
	sort.Strings(keys)
	for _, k := range keys {
		ov.Set(k, String(w.Overrides[k]))
	}
	tbl.Set("overrides", Table(ov))
	return WriteFileAtomic(p.WorldGenOverride(cluster, shard), []byte(Render(tbl)), 0o644)
}

// ReadWorldGen 读回面板这份文件。第二个返回值是"文件存在吗"——
// 不存在不是错误，界面要据此说"当前用的是引擎默认世界"。
//
// 读的是磁盘上此刻的那一份，而它未必还是面板写出去的字节：分片进世界后引擎会把它
// 重写一遍（见 StripKleiHeader 与 WorldSpec.EngineRewritten）。这里按引擎的三种拼法
// 依次取预设，保证第一次成功启动之后世界页还读得出、也还改得动。
func (p Paths) ReadWorldGen(cluster, shard string) (WorldSpec, bool, error) {
	var w WorldSpec
	path := p.WorldGenOverride(cluster, shard)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		w.Overrides = map[string]string{}
		return w, false, nil
	}
	if err != nil {
		return w, false, fmt.Errorf("读 worldgenoverride.lua: %w", err)
	}
	tbl, err := ParseLuaTable(string(raw))
	if err != nil {
		return w, true, fmt.Errorf("解析 %s: %w", path, err)
	}
	// 面板写的是 preset；引擎改写后写的是 worldgen_preset 与 settings_preset
	// 两个键（shardindex.lua:434/437 就是按这三个名字取的，或出来的结果等价）。
	w.Preset = tbl.Get("preset").AsString("")
	if w.Preset == "" {
		for _, k := range []string{"worldgen_preset", "settings_preset"} {
			if v := tbl.Get(k).AsString(""); v != "" {
				w.Preset = v
				w.EngineRewritten = true
				break
			}
		}
	}
	w.Overrides = Overrides(tbl)
	if w.Overrides == nil {
		w.Overrides = map[string]string{}
	}
	return w, true, nil
}

// LevelDataNote 检查那份面板不写的 GAME USE 文件在不在。
// 它在的话，引擎的世界定义会被它先覆盖一遍（虽然带 preset 的 worldgenoverride
// 随后整体接管），界面得说清楚现在生效的是哪一份，别让人对着一个不起眼的文件调参。
func (p Paths) LevelDataNote(cluster, shard string) string {
	if _, err := os.Stat(p.LevelData(cluster, shard)); err != nil {
		return ""
	}
	return fmt.Sprintf("注意：%s 目录里还有一份手工放置的 leveldataoverride.lua。"+
		"面板只写 worldgenoverride.lua；两份都在时以 preset 为准，那份手工文件里的世界名与设定不会生效，建议删掉或移走",
		p.ShardDir(cluster, shard))
}
