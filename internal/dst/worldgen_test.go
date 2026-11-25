package dst

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func worldGenPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	p := Paths{KleiRoot: filepath.Join(root, "klei"), DstInstall: filepath.Join(root, "dst")}
	for _, shard := range []string{ShardMaster, ShardCaves} {
		if err := os.MkdirAll(p.ShardDir("Cluster_1", shard), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// readWorldGenFile 读回字节并顺手做一次"行锚定"检查用的取行函数。
func readWorldGenFile(t *testing.T, p Paths, shard string) string {
	t.Helper()
	raw, err := os.ReadFile(p.WorldGenOverride("Cluster_1", shard))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// 引擎的 USER USE 通道只要三样东西：override_enabled 开关、preset 名、overrides 表。
// 少开关 = 整份文件当没有；少 preset = 只剩一张没有地形归属的 overrides。
func TestWriteWorldGenCarriesSwitchPresetAndTable(t *testing.T) {
	p := worldGenPaths(t)
	if err := p.WriteWorldGen("Cluster_1", ShardMaster, WorldSpec{
		Preset:    "SURVIVAL_TOGETHER",
		Overrides: map[string]string{"world_size": "small"},
	}); err != nil {
		t.Fatalf("WriteWorldGen: %v", err)
	}
	text := readWorldGenFile(t, p, ShardMaster)
	// 行锚定而不是 substring：这一层写坏过一整类问题就是"我们自己的读法
	// 把错误的字节读成对的"（cluster.ini 的引号），所以断言字节形态。
	// (?m) 必须有：Go 的 ^ 默认只锚整个字符串的开头，不加就是每条都失败。
	// 逗号是 Render 给每一项加的（Lua 表构造允许尾逗号），一并锚掉。
	for _, want := range []string{
		`(?m)^\toverride_enabled = true,\s*$`,
		`(?m)^\tpreset = "SURVIVAL_TOGETHER",\s*$`,
		`(?m)^\toverrides = \{$`,
		`(?m)^\t\tworld_size = "small",$`,
	} {
		if !regexp.MustCompile(want).MatchString(text) {
			t.Errorf("文件里没有匹配 %s 的行:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(text, "return {") {
		t.Errorf("文件不是 return { ... } 的形状：%q", text[:min(60, len(text))])
	}
	// 空 overrides 也必须是表而不是 nil。
	empty := worldGenPaths(t)
	if err := empty.WriteWorldGen("Cluster_1", ShardMaster, WorldSpec{Preset: "SURVIVAL_TOGETHER", Overrides: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s*overrides = \{`).MatchString(readWorldGenFile(t, empty, ShardMaster)) {
		t.Errorf("空 overrides 没写成表：%s", readWorldGenFile(t, empty, ShardMaster))
	}
	if _, err := ParseLuaTable(text); err != nil {
		t.Errorf("写出去的文件自己读不回来: %v", err)
	}
}

// 面板不再往世界文件里补 layout_mode/has_ocean 这批引擎内生项：
// 它们是 locations.lua 给地形的，走 preset 通道由引擎自己带上；
// 写进 worldgenoverride.lua 只会让 shardindex.lua 的 SanityCheckWorldGenOverride
// 在日志里打 "WARNING! Found override 'layout_mode', but this doesn't match any known option"
// ——2026-10-31 云机实测过这两条 WARNING。
//
// 断言打在"顶层键集合"上而不是 substring：SanityCheckWorldGenOverride 只认
// 那五个顶层键，而 overrides 里面出现 start_location 这类以 location 结尾的
// 字典键是合法的，拿子串去查会把合法文件判成违规（bootstrap_test 里就误报过一次）。
func TestWriteWorldGenDoesNotCarryEngineInternals(t *testing.T) {
	p := worldGenPaths(t)
	if err := p.WriteWorldGen("Cluster_1", ShardMaster, WorldSpec{
		Preset: "SURVIVAL_TOGETHER",
		// 地表字典里有 start_location：它是 overrides 里的一项，不是顶层键。
		Overrides: map[string]string{"world_size": "small", "start_location": "forest"},
	}); err != nil {
		t.Fatal(err)
	}
	text := readWorldGenFile(t, p, ShardMaster)
	tbl, err := ParseLuaTable(text)
	if err != nil {
		t.Fatalf("文件读不回来: %v\n%s", err, text)
	}
	allowed := map[string]bool{"override_enabled": true, "preset": true,
		"settings_preset": true, "worldgen_preset": true, "overrides": true}
	for _, k := range tbl.Keys() {
		if !allowed[k] {
			t.Errorf("写出了引擎不认的顶层键 %q（只会换一条 WARNING）:\n%s", k, text)
		}
	}
	for _, bad := range []string{"name", "desc", "location", "id"} {
		if tbl.Has(bad) {
			t.Errorf("preset 通道不接受顶层键 %q：%q 是 leveldataoverride.lua 的字段:\n%s", bad, bad, text)
		}
	}
	ov := tbl.Get("overrides")
	if ov.Kind != KindTable || ov.T == nil {
		t.Fatalf("overrides 不是表:\n%s", text)
	}
	for _, bad := range []string{"layout_mode", "has_ocean", "keep_disconnected_tiles",
		"no_wormholes_to_disconnected_tiles", "no_joining_islands", "wormhole_prefab"} {
		if ov.T.Has(bad) {
			t.Errorf("面板自己把引擎内生项 %s 写进了 overrides:\n%s", bad, text)
		}
	}
	// 用户从字典里点的东西要原样进 overrides。
	if got := ov.T.Get("world_size").AsString(""); got != "small" {
		t.Errorf("world_size 没进 overrides: %+v", ov.T.Keys())
	}
}

// preset 是白名单而不是自由文本：引擎查不到预设名时不报错，
// 只打一句 nonexistent preset 然后按别的生成，用户看到的是"我选了它但没生效"。
func TestWriteWorldGenRejectsWrongPresetBeforeWriting(t *testing.T) {
	cases := []struct {
		name    string
		shard   string
		preset  string
		wantSub string
	}{
		{"地表分片给洞穴预设", ShardMaster, "DST_CAVE", "不是"},
		{"洞穴分片给地表预设", ShardCaves, "SURVIVAL_TOGETHER", "不是"},
		{"根本不存在的名", ShardMaster, "MY_OWN_WORLD", "SURVIVAL_TOGETHER"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := worldGenPaths(t)
			err := p.WriteWorldGen("Cluster_1", c.shard, WorldSpec{
				Preset: c.preset, Overrides: map[string]string{},
			})
			if err == nil {
				t.Fatalf("该拦住了却写成功：%+v", c)
			}
			if !errors.Is(err, ErrInvalidArg) {
				t.Errorf("错误应是 ErrInvalidArg（接口要回 400），得到 %v", err)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("错误 %q 里没提到 %q", err.Error(), c.wantSub)
			}
			if !strings.Contains(err.Error(), "（") {
				t.Errorf("报错应给出可选预设的中文说法，而不只一串码值：%q", err.Error())
			}
			if _, statErr := os.Stat(p.WorldGenOverride("Cluster_1", c.shard)); !os.IsNotExist(statErr) {
				t.Errorf("校验没过却还是把文件写下去了：%v", statErr)
			}
		})
	}
}

// 用户不选预设时不能把空串写进去：每个地形的默认预设就是引擎自己那份。
func TestWriteWorldGenFillsDefaultPresetPerShard(t *testing.T) {
	for _, shard := range []string{ShardMaster, ShardCaves} {
		p := worldGenPaths(t)
		if err := p.WriteWorldGen("Cluster_1", shard, WorldSpec{Overrides: map[string]string{}}); err != nil {
			t.Fatalf("%s: %v", shard, err)
		}
		want := DefaultPreset(ShardLocation(shard))
		if !regexp.MustCompile(`(?m)^\tpreset = "` + regexp.QuoteMeta(want) + `",$`).MatchString(readWorldGenFile(t, p, shard)) {
			t.Errorf("%s 没补上默认预设 %s:\n%s", shard, want, readWorldGenFile(t, p, shard))
		}
	}
}

func TestWriteWorldGenRejectsKeysThatAreNotLuaIdentifiers(t *testing.T) {
	for _, k := range []string{"world size", "1st", "world-size"} {
		p := worldGenPaths(t)
		err := p.WriteWorldGen("Cluster_1", ShardMaster, WorldSpec{
			Preset: "SURVIVAL_TOGETHER", Overrides: map[string]string{k: "small"},
		})
		if err == nil {
			t.Fatalf("键名 %q 该被拒掉", k)
		}
		if !strings.Contains(err.Error(), "Lua") {
			t.Errorf("错误 %q 没说清为什么不能写", err.Error())
		}
	}
}

// 两次写同一份设定必须逐字节一致，否则每次保存都留下一份"看起来改过什么"的 .bak。
func TestWriteWorldGenIsStable(t *testing.T) {
	p := worldGenPaths(t)
	if err := p.WriteWorldGen("Cluster_1", ShardMaster, WorldSpec{
		Preset:    "SURVIVAL_TOGETHER",
		Overrides: map[string]string{"world_size": "small", "autumn": "short", "autumn_enabled": "default"},
	}); err != nil {
		t.Fatal(err)
	}
	first := readWorldGenFile(t, p, ShardMaster)
	if err := p.WriteWorldGen("Cluster_1", ShardMaster, WorldSpec{
		Preset:    "SURVIVAL_TOGETHER",
		Overrides: map[string]string{"autumn_enabled": "default", "autumn": "short", "world_size": "small"},
	}); err != nil {
		t.Fatal(err)
	}
	second := readWorldGenFile(t, p, ShardMaster)
	if first != second {
		t.Errorf("同样一份设定写出两个文件：\n%s\n----\n%s", first, second)
	}
	if _, err := os.Stat(p.WorldGenOverride("Cluster_1", ShardMaster) + ".bak"); err != nil {
		t.Errorf("第二次覆盖没留下 .bak：%v", err)
	}
}

func TestReadWorldGenRoundTripKeepsUserValues(t *testing.T) {
	p := worldGenPaths(t)
	spec := WorldSpec{
		Preset:    "DST_CAVE_PLUS",
		Overrides: map[string]string{"world_size": "medium", "regrowth": "fast", "spiders": "dangerous"},
	}
	if err := p.WriteWorldGen("Cluster_1", ShardCaves, spec); err != nil {
		t.Fatalf("WriteWorldGen: %v", err)
	}
	got, present, err := p.ReadWorldGen("Cluster_1", ShardCaves)
	if err != nil {
		t.Fatalf("ReadWorldGen: %v", err)
	}
	if !present {
		t.Fatal("文件明明写了却读不到")
	}
	if got.Preset != spec.Preset {
		t.Errorf("preset 回来是 %q，写进去的是 %q", got.Preset, spec.Preset)
	}
	for k, want := range spec.Overrides {
		if got.Overrides[k] != want {
			t.Errorf("%s 回来是 %q，写进去的是 %q（全套：%+v）", k, got.Overrides[k], want, got.Overrides)
		}
	}
}

func TestReadWorldGenMissingIsNotAnError(t *testing.T) {
	p := worldGenPaths(t)
	spec, present, err := p.ReadWorldGen("Cluster_1", ShardMaster)
	if err != nil {
		t.Fatalf("文件不存在不该报错：%v", err)
	}
	if present {
		t.Error("没写过却报告存在")
	}
	if spec.Overrides == nil {
		t.Error("Overrides 是 nil：调用方要能直接 range，不用先判空")
	}
}

// 预设表是界面下拉框的数据源，也是写文件的白名单，两处共用一份不能自相矛盾。
func TestPresetTableIsSelfConsistent(t *testing.T) {
	if got := DefaultPreset(LocationForest); got != "SURVIVAL_TOGETHER" {
		t.Errorf("地表的默认预设 = %q，期望 SURVIVAL_TOGETHER（云机实测过的那一个）", got)
	}
	if got := DefaultPreset(LocationCave); got != "DST_CAVE" {
		t.Errorf("洞穴的默认预设 = %q，期望 DST_CAVE", got)
	}
	if len(PresetsForLocation(LocationForest)) == 0 || len(PresetsForLocation(LocationCave)) == 0 {
		t.Error("有一个地形没有可选预设，界面上会是空下拉框")
	}
	if len(PresetsForLocation("nowhere")) != 0 {
		t.Error("不存在的地形不该有预设")
	}
	if DefaultPreset("nowhere") != "" {
		t.Error("不存在的地形不该有默认预设")
	}
	for _, p := range PresetsForLocation(LocationForest) {
		if p.Text == "" || p.Text == p.ID {
			t.Errorf("预设 %s 没有中文说法，界面会直接露码值", p.ID)
		}
		if p.English == "" {
			t.Errorf("预设 %s 没带引擎原文，用户没法拿去搜帖子对照", p.ID)
		}
		if !isKnownPreset(LocationForest, p.ID) {
			t.Errorf("PresetsForLocation 与 isKnownPreset 不一致：%s", p.ID)
		}
		if PresetLabel(p.ID) != p.Text {
			t.Errorf("PresetLabel(%s) = %q", p.ID, PresetLabel(p.ID))
		}
	}
	// 字典里给中文说法的键，界面上永远不该出现同一个键的码值形态。
	if PresetLabel("SOMETHING_ELSE") != "SOMETHING_ELSE" {
		t.Error("认不出的预设应原样返回，让界面看得见而不是编一个名字")
	}
}

func TestShardLocationPairing(t *testing.T) {
	if got := ShardLocation(ShardCaves); got != LocationCave {
		t.Errorf("Caves 的地形 = %q，期望 %q", got, LocationCave)
	}
	if got := ShardLocation(ShardMaster); got != LocationForest {
		t.Errorf("Master 的地形 = %q，期望 %q", got, LocationForest)
	}
	if got := ShardForLocation(LocationCave); got != ShardCaves {
		t.Errorf("cave 该属于 %q，得到 %q", ShardCaves, got)
	}
	// 洞穴不能没有 Master：这是地形配对的物理前提，写反了是分片连不上。
	if ShardForLocation(ShardLocation(ShardCaves)) != ShardCaves {
		t.Error("ShardLocation 与 ShardForLocation 不互为反函数")
	}
}

// 引擎读完这份文件会把它重写一遍，2026-10-31 云机 Cluster_1/Master 实测：
// 面板写出去的是 preset + 189 项 overrides（189 字节量级），分片进世界后磁盘上剩的是
// 下面这一份 —— 186 字节，开头多了 SetPersistentString 的 "KLEI     1 " 头，
// preset 展开成 worldgen_preset + settings_preset 两个键（shardindex.lua:434/437
// 就是这两个名字，或上 preset 都认），overrides 只留下它认下的 task_set。
// 日志里对应的两句是 "Loaded and applied world gen overrides from ../worldgenoverride.lua"
// 和 "Overwriting savedata with override file."
//
// 这一份才是第一次成功启动之后世界页读到的文件。按面板自己写出去的形状读，
// 结果是把"引擎只留了一项"读成"没有 preset"：世界页要么 500，要么 PUT 永远 409，
// 用户看着一个跑得好好的服务器改不动任何设定。
func TestReadWorldGenToleratesEngineRewrittenFile(t *testing.T) {
	p := worldGenPaths(t)
	raw := "KLEI     1 return {\n\toverride_enabled = true,\n\tworldgen_preset = \"SURVIVAL_TOGETHER_CLASSIC\",\n\tsettings_preset = \"SURVIVAL_TOGETHER_CLASSIC\",\n\toverrides = {\n\t\ttask_set = \"default\",\n\t},\n}"
	if err := os.WriteFile(p.WorldGenOverride("Cluster_1", ShardMaster), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got, present, err := p.ReadWorldGen("Cluster_1", ShardMaster)
	if err != nil {
		t.Fatalf("引擎留下的这一份读不回来（世界页会 500）：%v", err)
	}
	if !present {
		t.Fatal("文件在位却报不存在")
	}
	if got.Preset != "SURVIVAL_TOGETHER_CLASSIC" {
		t.Errorf("预设应从 worldgen_preset 读出来，得到 %q", got.Preset)
	}
	if !IsKnownPreset(LocationForest, got.Preset) {
		t.Errorf("读回来的预设 %q 不在面板可选表里，下一次 PUT 会被自己拒掉", got.Preset)
	}
	if got.Overrides["task_set"] != "default" {
		t.Errorf("overrides 没读回来：%+v", got.Overrides)
	}
	if !got.EngineRewritten {
		t.Error("没认出这是引擎改写过的文件：界面就不会解释为什么设定项从 189 变成 1")
	}
}

// 只认引擎那两种拼法里的一种也算读得出（settings_preset 单独出现时同样合法）。
func TestReadWorldGenAcceptsEitherEnginePresetKey(t *testing.T) {
	for _, key := range []string{"worldgen_preset", "settings_preset"} {
		p := worldGenPaths(t)
		raw := fmt.Sprintf("KLEI     1 return {\n\toverride_enabled = true,\n\t%s = \"DST_CAVE\",\n\toverrides = {\n\t},\n}", key)
		if err := os.WriteFile(p.WorldGenOverride("Cluster_1", ShardCaves), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		got, _, err := p.ReadWorldGen("Cluster_1", ShardCaves)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got.Preset != "DST_CAVE" {
			t.Errorf("%s 没读成预设：%q", key, got.Preset)
		}
	}
}

// 2026-10-31 云机实测：洞穴分片存过一次世界之后，磁盘上只剩这一份 128 字节的文件
// （面板当初写的是 2223 字节、81 项）。它和地表那份的区别是 overrides 是空表——
// 洞穴世界的定制项全在预设里，引擎认为没什么要留的。
// 空表这一支要单独钉住：读成 nil 还是读成空 map，决定了世界页会不会显示成"没读出来"。
func TestReadWorldGenCavesRewrittenFileHasEmptyOverrides(t *testing.T) {
	p := worldGenPaths(t)
	raw := "KLEI     1 return {\n\toverride_enabled = true,\n\tworldgen_preset = \"DST_CAVE\",\n\tsettings_preset = \"DST_CAVE\",\n\toverrides = {\n\t},\n}"
	if len(raw) > 200 {
		t.Fatalf("这份夹具不是云机那一份 128 字节的形状：%d 字节", len(raw))
	}
	if err := os.WriteFile(p.WorldGenOverride("Cluster_1", ShardCaves), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got, present, err := p.ReadWorldGen("Cluster_1", ShardCaves)
	if err != nil {
		t.Fatalf("洞穴这份读不回来：%v", err)
	}
	if !present || got.Preset != "DST_CAVE" || !IsKnownPreset(LocationCave, got.Preset) {
		t.Fatalf("洞穴预设不对或读丢了：%+v present=%v", got, present)
	}
	if len(got.Overrides) != 0 {
		t.Errorf("引擎留下的空 overrides 应读成 0 项，得到 %d 项：%+v", len(got.Overrides), got.Overrides)
	}
	if !got.EngineRewritten {
		t.Error("洞穴这份也要认出是引擎重写过的，界面才会解释那 81 项去哪了")
	}
}

// 面板自己写的文件不该被当成引擎改写过的：EngineRewritten 只在引擎那两种拼法出现时为真，
// 界面上那句"文件被改写过"不能随便说。
func TestReadWorldGenPanelWrittenFileIsNotMarkedRewritten(t *testing.T) {
	p := worldGenPaths(t)
	if err := p.WriteWorldGen("Cluster_1", ShardMaster, WorldSpec{
		Preset: "SURVIVAL_TOGETHER", Overrides: map[string]string{"world_size": "small"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := p.ReadWorldGen("Cluster_1", ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if got.EngineRewritten {
		t.Errorf("面板刚写的文件被误判成引擎改写过的：%+v", got)
	}
}

// 手工放过 leveldataoverride.lua 的现场要能在界面上被看见：
// 那份文件里的世界名在 preset 通道下不会生效，不说清楚就有人对着它调参。
func TestLevelDataNoteOnlyWhenThatFileExists(t *testing.T) {
	p := worldGenPaths(t)
	if got := p.LevelDataNote("Cluster_1", ShardMaster); got != "" {
		t.Errorf("文件不存在却给了提示：%q", got)
	}
	if err := os.WriteFile(p.LevelData("Cluster_1", ShardMaster), []byte("return {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := p.LevelDataNote("Cluster_1", ShardMaster)
	if !strings.Contains(got, "leveldataoverride.lua") || !strings.Contains(got, "不会生效") {
		t.Errorf("提示没说清是哪份文件、为什么不起作用：%q", got)
	}
}
