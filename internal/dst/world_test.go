package dst

import (
	"strings"
	"testing"
)

func mustSchema(t *testing.T) *WorldSchema {
	t.Helper()
	s, err := LoadWorldSchema()
	if err != nil {
		t.Fatalf("LoadWorldSchema: %v", err)
	}
	return s
}

func TestSchemaHasChineseLabelsForKnownSettings(t *testing.T) {
	s := mustSchema(t)
	loc, ok := s.Location("forest", "zh")
	if !ok {
		t.Fatal("字典里没有 forest 地形")
	}
	for _, key := range []string{"world_size", "season_start", "autumn", "start_location", "ocean_shoal"} {
		it, ok := loc.Item(key)
		if !ok {
			t.Errorf("条目 %s 缺失", key)
			continue
		}
		if it.Text == key || it.Text == "" {
			t.Errorf("条目 %s 没有中文名: %q", key, it.Text)
		}
		if len(it.Options) == 0 {
			t.Errorf("条目 %s 没有可选值", key)
			continue
		}
		for _, o := range it.Options {
			if o.Value == "" || o.Text == "" {
				t.Errorf("条目 %s 有空的选项文案: %+v", key, o)
			}
		}
	}
	if _, ok := s.Location("forest", "en"); !ok {
		t.Error("缺英文回退表")
	}
	// cave 的条目集与 forest 不同，必须各自成表。
	cave, ok := s.Location("cave", "zh")
	if !ok {
		t.Fatal("字典里没有 cave 地形")
	}
	if len(cave.Groups) == 0 || len(cave.Groups[0].Items) == 0 {
		t.Error("cave 地形没有条目")
	}
}

func TestOverridesReadFromRealShape(t *testing.T) {
	tbl, err := ParseLuaTable(realWorldSample)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	got := Overrides(tbl)
	if got["world_size"] != "small" || got["season_start"] != "default" {
		t.Fatalf("overrides 读取错误: %+v", got)
	}
	// 顶层字段不能被当成世界设定。
	if _, ok := got["desc"]; ok {
		t.Error("Overrides 误读了顶层字段")
	}
	if got["darkness"] != "default" {
		t.Errorf("漏读 darkness: %+v", got)
	}
}

func TestCheckOverrides(t *testing.T) {
	s := mustSchema(t)
	season, ok := s.Location("forest", "zh")
	if !ok {
		t.Fatal("no forest")
	}
	ws, _ := season.Item("world_size")
	if len(ws.Options) == 0 {
		t.Fatal("world_size 无选项")
	}
	valid := ws.Options[0].Value

	problems := s.CheckOverrides("forest", "zh", map[string]string{
		"world_size":         valid,
		"autumn":             "nonsense_value",
		"mod_custom_setting": "",
	})
	byKey := map[string]WorldProblem{}
	for _, p := range problems {
		byKey[p.Key] = p
	}
	if _, bad := byKey["world_size"]; bad {
		t.Errorf("合法取值被判错: %+v", byKey["world_size"])
	}
	if p, okk := byKey["autumn"]; !okk || p.Level != "error" {
		t.Errorf("表外取值应报 error: %+v", byKey["autumn"])
	}
	// 空值同样要拦住：写进文件会变成 overrides = { world_size = "" } 这种坑。
	if p, okk := byKey["mod_custom_setting"]; !okk {
		t.Errorf("未知键应至少给 warn: %+v", problems)
	} else if p.Level != "warn" && p.Level != "error" {
		t.Errorf("未知键级别异常: %+v", p)
	}

	if got := s.CheckOverrides("nowhere", "zh", map[string]string{"world_size": valid}); len(got) == 0 || got[0].Level != "warn" {
		t.Errorf("未知地形应只给 warn: %+v", got)
	}
	if got := s.CheckOverrides("forest", "zh", nil); len(got) != 0 {
		t.Errorf("空提交不该有问题: %+v", got)
	}
}

func TestApplyOverridesRejectsBadValueAtomically(t *testing.T) {
	s := mustSchema(t)
	tbl, err := ParseLuaTable(realWorldSample)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	before := Render(tbl)
	if err := s.ApplyOverrides(tbl, "forest", "zh", map[string]string{
		"world_size": "tiny", // 表里没有
		"autumn":     "long", // 合法
	}); err == nil {
		t.Fatal("非法取值却没报错")
	} else if !strings.Contains(err.Error(), "world_size") {
		t.Errorf("错误信息没点出键名: %v", err)
	}
	if Render(tbl) != before {
		t.Error("校验失败却改动了表内容")
	}
}

func TestApplyOverridesKeepsOrderAndUnknownKeys(t *testing.T) {
	s := mustSchema(t)
	tbl, err := ParseLuaTable(realWorldSample)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	patch := map[string]string{"world_size": "huge", "ocean_shoal": "default", "mod_added_setting": "whatever"}
	if err := s.ApplyOverrides(tbl, "forest", "zh", patch); err != nil {
		t.Fatalf("应用: %v", err)
	}
	ov := Overrides(tbl)
	if ov["world_size"] != "huge" {
		t.Errorf("未写入新值: %+v", ov)
	}
	if ov["start_location"] != "default" || ov["season_start"] != "default" {
		t.Errorf("未提交的条目被改动: %+v", ov)
	}
	// 数组顺序必须还在原位，否则 diff 会整片抖动。
	ovTbl, _ := tbl.Nested("overrides")
	keys := ovTbl.Keys()
	if keys[0] != "season_start" {
		t.Errorf("overrides 首键被挪动: %+v", keys)
	}
	// 顶层其它字段（如 required_drops_prefabs 数组）不受影响。
	if _, ok := tbl.Nested("overrides"); !ok {
		t.Fatal("overrides 丢了")
	}
	rendered := Render(tbl)
	again, err := ParseLuaTable(rendered)
	if err != nil {
		t.Fatalf("改后重解析失败: %v", err)
	}
	if !deepEqualTable(tbl, again) {
		t.Error("改后渲染不无损")
	}
}

func TestDefaultOverridesAreAllValid(t *testing.T) {
	s := mustSchema(t)
	for _, lang := range []string{"zh", "en"} {
		for _, loc := range []string{"forest", "cave"} {
			def := s.DefaultOverrides(loc, lang)
			if len(def) < 50 {
				t.Fatalf("%s/%s 默认值太少: %d", lang, loc, len(def))
			}
			for _, p := range s.CheckOverrides(loc, lang, def) {
				if p.Level == "error" {
					t.Errorf("%s/%s 默认值不合法: %+v", lang, loc, p)
				}
			}
		}
	}
}
