package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/modbridge"
)

func (f *apiFixture) writeMap(t *testing.T, shard string, m modbridge.MapData, mt time.Time) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := f.paths.HelperMapFile(f.cluster, shard)
	if err := os.MkdirAll(strings.TrimSuffix(path, "dstpanel_map.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("KLEI     1 "), raw...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func testMapData() modbridge.MapData {
	tiles := make([]int, 64)
	for i := range tiles {
		switch {
		case i < 48:
			tiles[i] = 7 // FOREST 森林
		case i < 63:
			tiles[i] = 6 // GRASS 草地
		default:
			tiles[i] = 204 // OCEAN_ROUGH 海水
		}
	}
	return modbridge.MapData{
		Shard: "Master", GameID: "G1", Width: 32, Height: 32, Grid: 8,
		Tiles: tiles,
		Points: [][5]int{
			{1, 1, 1, 2, 0},  // 建筑
			{2, 2, 2, 5, 3},  // 资源·草
			{2, 2, 2, 12, 5}, // 同一格的另一种资源：一格可以出多条
			{8, 8, 3, 1, 0},  // 危险
			{3, 3, 4, 1, 0},  // 伙伴随从（切斯特那一类）
		},
		Totals: modbridge.MapTotals{Build: 2, Res: 17, Danger: 1, Companion: 1},
	}
}

func TestMapViewUsesGroupNamesAndLivePlayers(t *testing.T) {
	f := newFixture(t, "")
	f.writeStatus(t, "Master", `{"shard":"Master","game_id":"G1","players":[
		{"name":"阿伟","prefab":"wilson","userid":"KU_1","x":0,"z":0,"is_dead":false},
		{"name":"没坐标","prefab":"willow","userid":"KU_2"}]}`, time.Now())
	f.writeMap(t, "Master", testMapData(), time.Now())

	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/map", "admin", "secret")
	var v mapView
	decodeData(t, rec, &v)

	if !v.Ready || v.Pending {
		t.Fatalf("ready/pending = %v/%v，期望 200 且不在导出", v.Ready, v.Pending)
	}
	if v.Width != 32 || v.Grid != 8 || len(v.Tiles) != 64 {
		t.Errorf("地形尺寸不对: %dx%d grid=%d tiles=%d", v.Width, v.Height, v.Grid, len(v.Tiles))
	}
	// 图例按格子数从多到少，且只给中文名，没有 Klei 的 tile id。
	if len(v.Terrain) != 3 || v.Terrain[0].Label != "森林" || v.Terrain[0].Cells != 48 ||
		v.Terrain[1].Label != "草地" || v.Terrain[2].Label != "海水" {
		t.Errorf("地形图例 = %+v", v.Terrain)
	}
	if v.Tiles[0] != 0 || v.Tiles[63] != 2 {
		t.Errorf("格子序号没跟着图例重排: 首=%d 尾=%d", v.Tiles[0], v.Tiles[63])
	}
	if len(v.Points) != 5 {
		t.Fatalf("点位 = %+v", v.Points)
	}
	if p := v.Points[0]; p.Kind != "build" || p.KindLabel != "建筑" || p.X != 0 || p.Y != 0 || p.Count != 2 {
		t.Errorf("建筑点 = %+v", p)
	}
	// 同一格的两种资源各一条，且各自带中文子类名——放大后"浆果丛×12"那句就靠它。
	if p := v.Points[2]; p.Kind != "res" || p.Count != 12 || p.SubLabel != "浆果丛" || p.Sub != "sub5" {
		t.Errorf("资源子类 = %+v，期望 浆果丛/12", p)
	}
	if p := v.Points[1]; p.SubLabel != "草" {
		t.Errorf("第二个资源点的子类 = %+v", p)
	}
	if p := v.Points[4]; p.Kind != "companion" || p.KindLabel != "伙伴随从" || p.SubLabel != "" {
		t.Errorf("伙伴随从点 = %+v", p)
	}
	if v.Totals.Build != 2 || v.Totals.Res != 17 || v.Totals.Danger != 1 || v.Totals.Companion != 1 {
		t.Errorf("汇总 = %+v", v.Totals)
	}
	if len(v.Players) != 2 {
		t.Fatalf("在线玩家 = %+v", v.Players)
	}
	// 世界原点在地图中心：0/4 + 32/2 = 16，再除以 32 乘 8 得 4。
	p := v.Players[0]
	if p.X == nil || p.Y == nil || *p.X != 4 || *p.Y != 4 {
		t.Errorf("原点应落在网格中心 (4,4)，实际 %+v", p)
	}
	if v.Players[1].X != nil {
		t.Errorf("没坐标的玩家不该被画到 (0,0): %+v", v.Players[1])
	}
	// 数据还新鲜时不该往队列里塞命令。
	if q, err := os.ReadFile(f.paths.HelperCommandQueue(f.cluster, "Master")); err == nil && strings.Contains(string(q), "|map") {
		t.Errorf("新鲜地图仍然触发了导出: %s", string(q))
	}
}

// TestMapViewCarriesIndividualPoints 钉高倍率贴图的数据前提：
// 个体点位要原样到界面（每个实体一条，带引擎 tile 坐标），
// 而协议编号必须有随响应下发的字典可翻——界面上不许出现 1/2/3 这种码值。
func TestMapViewCarriesIndividualPoints(t *testing.T) {
	f := newFixture(t, "")
	f.writeStatus(t, "Master", `{"shard":"Master","game_id":"G1","players":[]}`, time.Now())
	md := modbridge.MapData{
		Shard: "Master", GameID: "G1", Width: 32, Height: 32, Grid: 8,
		Tiles:  make([]int, 64),
		Ents:   [][4]int{{4, 4, 2, 5}, {5, 5, 2, 5}, {0, 0, 4, 0}, {31, 31, 3, 0}},
		Totals: modbridge.MapTotals{Res: 2, Danger: 1, Companion: 1},
	}
	f.writeMap(t, "Master", md, time.Now())

	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/map", "admin", "secret")
	var v mapView
	decodeData(t, rec, &v)

	if len(v.Ents) != 4 {
		t.Fatalf("个体点位没传到界面：%+v", v.Ents)
	}
	// 聚合视图照旧给出：低倍率那一层还是按格计数，不能因为换了数据源就空掉。
	if len(v.Points) != 3 {
		t.Fatalf("由个体推导出的聚合点位 = %+v", v.Points)
	}
	berry := v.Points[0]
	for _, p := range v.Points {
		if p.Kind == "res" {
			berry = p
		}
	}
	if berry.Count != 2 || berry.SubLabel != "浆果丛" || berry.X != 1 || berry.Y != 1 {
		t.Errorf("两丛浆果该并成一格两条计数 = %+v", berry)
	}
	var hasSub, hasKind bool
	for _, s := range v.Subs {
		if s.Code == 5 && s.Key == "sub5" && s.Label == "浆果丛" {
			hasSub = true
		}
	}
	for _, k := range v.Kinds {
		if k.Code == 4 && k.Key == "companion" && k.Label == "伙伴随从" {
			hasKind = true
		}
	}
	if !hasSub {
		t.Errorf("子类字典没带全：%+v", v.Subs)
	}
	if !hasKind {
		t.Errorf("类别字典没带全：%+v", v.Kinds)
	}
}

func TestMapFirstRequestQueuesExport(t *testing.T) {
	f := newFixture(t, "")
	f.writeStatus(t, "Master", `{"shard":"Master","game_id":"G1","players":[]}`, time.Now())

	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/map", "admin", "secret")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("第一次请求 = %d，期望 202 让前端稍后再问，body %s", rec.Code, rec.Body.String())
	}
	var v mapView
	decodeData(t, rec, &v)
	if v.Ready || !v.Pending || v.Note == "" {
		t.Errorf("首次导出的状态 = ready:%v pending:%v note:%q", v.Ready, v.Pending, v.Note)
	}
	if n := mapCommandCount(t, f, "Master"); n != 1 {
		t.Fatalf("队列里应有 1 条 map，实际 %d", n)
	}
	// 同一个导出在途时，再问一次不能又塞一条：助手要为此多跑一遍两万格采样。
	rec = do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/map", "admin", "secret")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("第二次请求 = %d，期望仍是 202", rec.Code)
	}
	if n := mapCommandCount(t, f, "Master"); n != 1 {
		t.Errorf("重复请求把 map 命令塞了 %d 条", n)
	}

	// 导出完成后文件出现，接口直接给数据，不再入队。
	f.writeMap(t, "Master", testMapData(), time.Now())
	rec = do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/map", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("导出完成后的请求 = %d，body %s", rec.Code, rec.Body.String())
	}
}

func TestMapForceRefreshServesOldDataWhileExporting(t *testing.T) {
	f := newFixture(t, "")
	f.writeStatus(t, "Master", `{"shard":"Master","game_id":"G1","players":[]}`, time.Now())
	f.writeMap(t, "Master", testMapData(), time.Now())

	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/map?refresh=1", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("强制刷新 = %d，期望先把旧图给出来，body %s", rec.Code, rec.Body.String())
	}
	var v mapView
	decodeData(t, rec, &v)
	if !v.Ready || !v.Pending || !strings.Contains(v.Note, "导出") {
		t.Errorf("强制刷新的状态 = ready:%v pending:%v note:%q", v.Ready, v.Pending, v.Note)
	}
	if n := mapCommandCount(t, f, "Master"); n != 1 {
		t.Errorf("强制刷新应只入队 1 条 map，实际 %d", n)
	}
}

// 点「重新导出」之后，界面上的轮询走的是不带 refresh 的普通请求：底图才导出两分钟
// （没到十分钟的自动重导门槛），只看文件新鲜度的话这一问就得到 pending:false，
// 前端提前收工，等助手真把新图写回来时已经没人再问了。
func TestMapRefreshStaysPendingUntilNewerFileArrives(t *testing.T) {
	f := newFixture(t, "")
	f.writeStatus(t, "Master", `{"shard":"Master","game_id":"G1","players":[]}`, time.Now())
	f.writeMap(t, "Master", testMapData(), time.Now())

	get := func(q string) mapView {
		rec := do(t, f.srv.Handler(), http.MethodGet,
			"/api/v1/clusters/Cluster_1/shards/Master/map"+q, "admin", "secret")
		var v mapView
		decodeData(t, rec, &v)
		return v
	}
	if v := get("?refresh=1"); !v.Pending {
		t.Fatalf("刚排完队就说没在导出，前端不会再问：pending:%v note:%q", v.Pending, v.Note)
	}
	if v := get(""); !v.Pending {
		t.Errorf("不带 refresh 的轮询也该报 pending，实际 note:%q", v.Note)
	}

	// 助手把新底图写回来了：mtime 晚于排队时刻。
	f.writeMap(t, "Master", testMapData(), time.Now().Add(2*time.Second))
	if v := get(""); v.Pending {
		t.Errorf("新图已经落地，还在报 pending：%q", v.Note)
	}
}

func TestMapStaleFileTriggersReexport(t *testing.T) {
	f := newFixture(t, "")
	f.writeStatus(t, "Master", `{"shard":"Master","game_id":"G1","players":[]}`, time.Now())
	// 超过十分钟的底图会被自动重导（同一份地形十年前没变）。
	f.writeMap(t, "Master", testMapData(), time.Now().Add(-11*time.Minute))

	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/map", "admin", "secret")
	var v mapView
	decodeData(t, rec, &v)
	if !v.Ready || !v.Pending {
		t.Errorf("旧底图应该先给出来再后台重导，实际 ready:%v pending:%v", v.Ready, v.Pending)
	}
	if n := mapCommandCount(t, f, "Master"); n != 1 {
		t.Errorf("旧底图没有触发重导，队列 %d 条", n)
	}
}

func TestMapWithoutHelperIsServiceUnavailable(t *testing.T) {
	f := newFixture(t, "") // 没有 status，也就是没有分片在回话
	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/map", "admin", "secret")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("没有助手时 = %d，期望 503，body %s", rec.Code, rec.Body.String())
	}
	msg := decodeReply(t, rec).Err
	if !strings.Contains(msg, "没有回话") {
		t.Errorf("503 要说清是助手没回话，实际 %q", msg)
	}
	if n := mapCommandCount(t, f, "Master"); n != 0 {
		t.Errorf("助手不在线还把命令排进队列，会变成迟到生效：%d 条", n)
	}
}

func TestMapWrongShardIsNotFound(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Nope/map", "admin", "secret")
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的分片 = %d，期望 404，body %s", rec.Code, rec.Body.String())
	}
}

func mapCommandCount(t *testing.T, f *apiFixture, shard string) int {
	t.Helper()
	raw, err := os.ReadFile(f.paths.HelperCommandQueue(f.cluster, shard))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) > 1 && parts[1] == modbridge.CmdMap {
			n++
		}
	}
	return n
}

// TestMapTerrainGroupsCoverCommonTiles 的期望值全部抄自引擎自己的
// scripts/constants.lua:686 的 GROUND 表（云机上解 databundles/scripts.zip 读到的），
// 不是按面板的猜测写的——这张表早前用另一版编号，海洋 201~208 整段没人认领，
// 一张 425×425 的森林图有半数格子被涂成"其它地形"的暗紫红。
// 所以这里盯的是"编号对不对"，而不是"我们的函数返回了什么"。
func TestMapTerrainGroupsCoverCommonTiles(t *testing.T) {
	cases := map[int]string{
		1:     "void",    // IMPASSABLE
		2:     "road",    // ROAD
		3:     "rocky",   // ROCKY
		4:     "dirt",    // DIRT
		5:     "savanna", // SAVANNA
		6:     "grass",   // GRASS
		7:     "forest",  // FOREST
		8:     "marsh",   // MARSH
		9:     "web",     // WEB
		10:    "floor",   // WOODFLOOR
		13:    "cave",    // CAVE
		14:    "fungus",  // FUNGUS
		16:    "cave",    // UNDERROCK
		20:    "floor",   // TILES
		24:    "fungus",  // FUNGUSRED
		30:    "forest",  // DECIDUOUS
		31:    "sand",    // DESERT_DIRT
		42:    "rocky",   // PEBBLEBEACH
		44:    "sand",    // SHELLBEACH
		47:    "farm",    // FARMING_SOIL
		128:   "cave",    // UNDERGROUND
		151:   "wall",    // WALL_ROCKY
		201:   "coast",   // OCEAN_COASTAL
		202:   "coast",   // OCEAN_COASTAL_SHORE
		203:   "ocean",   // OCEAN_SWELL
		204:   "ocean",   // OCEAN_ROUGH
		205:   "ocean",   // OCEAN_BRINEPOOL
		208:   "ocean",   // OCEAN_WATERLOG
		247:   "ocean",   // OCEAN_END
		32:    "other",   // SCALE：认不出就不猜
		35:    "other",   // QUAGMIRE_PEATFOREST
		43:    "other",   // METEOR
		65535: "void",    // INVALID
		-1:    "void",    // 助手那边读不到
	}
	for tile, want := range cases {
		g := terrainGroupOf(tile)
		if g < 0 || g >= len(mapTerrainDefs) || mapTerrainDefs[g].Key != want {
			t.Errorf("tile %d 归到 %q，期望 %q", tile, mapTerrainDefs[g].Key, want)
		}
	}
}

// TestMapOceanNeverFallsIntoOther 是那句"正常情况下海会是红色的吗"的回归位：
// 整段海洋编号一个都不许掉进"其它地形"，否则一半地图会变成说不清颜色的暗紫红。
func TestMapOceanNeverFallsIntoOther(t *testing.T) {
	other := -1
	for i, d := range mapTerrainDefs {
		if d.Key == "other" {
			other = i
		}
	}
	if other < 0 {
		t.Fatal("分组表里没有 other 了？那这条测试要改写，不能直接删")
	}
	for tile := 201; tile <= 247; tile++ {
		if g := terrainGroupOf(tile); g == other {
			t.Errorf("海面编号 %d 落进了其它地形", tile)
		}
	}
}
