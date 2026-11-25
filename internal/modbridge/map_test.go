package modbridge

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"dstpanel/internal/dst"
)

func writeMapFile(t *testing.T, b *Bridge, cluster, shard string, m MapData, mt time.Time) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := b.Paths.HelperMapFile(cluster, shard)
	if err := os.WriteFile(path, append([]byte("KLEI     1 "), raw...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func validMap(grid int) MapData {
	tiles := make([]int, grid*grid)
	for i := range tiles {
		tiles[i] = 6
	}
	return MapData{
		Shard: dst.ShardMaster, GameID: "G1", Width: 32, Height: 32, Grid: grid,
		Tiles:  tiles,
		Points: [][5]int{{1, 1, 1, 2, 0}, {2, 2, 2, 5, ResSubStone}, {3, 3, 3, 1, 0}},
		Totals: MapTotals{Build: 2, Res: 5, Danger: 1},
	}
}

func TestMapReadsKleiHeaderAndFreshness(t *testing.T) {
	b, _ := newFixture(t)
	writeMapFile(t, b, "Cluster_1", dst.ShardMaster, validMap(8), b.now().Add(-2*time.Minute))

	m, err := b.Map("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if m.Grid != 8 || len(m.Tiles) != 64 || len(m.Points) != 3 {
		t.Errorf("解码结果不对: grid=%d tiles=%d points=%d", m.Grid, len(m.Tiles), len(m.Points))
	}
	if m.Age != 2*time.Minute || m.Stale {
		t.Errorf("新鲜度 = %s / stale=%v，期望 2m 且不算过期", m.Age, m.Stale)
	}
	if m.Totals.Build != 2 || m.Totals.Res != 5 {
		t.Errorf("汇总丢了: %+v", m.Totals)
	}

	writeMapFile(t, b, "Cluster_1", dst.ShardMaster, validMap(8), b.now().Add(-11*time.Minute))
	m, err = b.Map("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Stale {
		t.Error("11 分钟前的地图应该标成过期")
	}
}

func TestMapMissingIsNotExist(t *testing.T) {
	b, _ := newFixture(t)
	_, err := b.Map("Cluster_1", dst.ShardMaster)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("缺文件要能被 errors.Is 认出来，实际 %v", err)
	}
}

func TestMapRejectsBadShapes(t *testing.T) {
	cases := []struct {
		name string
		edit func(*MapData)
	}{
		{"网格太细", func(m *MapData) { m.Grid = 4; m.Tiles = make([]int, 16) }},
		{"格子数不对", func(m *MapData) { m.Tiles = m.Tiles[:10] }},
		{"世界尺寸为 0", func(m *MapData) { m.Width = 0 }},
		{"点位越界", func(m *MapData) { m.Points[0][0] = 99 }},
		{"点位类别不认识", func(m *MapData) { m.Points[0][2] = 9 }},
		{"点位数量为 0", func(m *MapData) { m.Points[0][3] = 0 }},
		{"资源子类不认识", func(m *MapData) { m.Points[1][4] = resSubMax + 1 }},
		{"非资源点带了子类", func(m *MapData) { m.Points[0][4] = ResSubTree }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newFixture(t)
			m := validMap(8)
			tc.edit(&m)
			writeMapFile(t, b, "Cluster_1", dst.ShardMaster, m, b.now())
			if _, err := b.Map("Cluster_1", dst.ShardMaster); err == nil {
				t.Fatal("脏数据也应该报错，不能拿去画图")
			}
		})
	}
}

// TestMapEntsAggregateIntoCells 钉的是"个体点位 → 界面用的格子计数"这条换算：
// 公式必须和旧版助手在 Lua 里做的那一遍一模一样（floor(tx*grid/width)+1，再夹进网格），
// 不然换了助手版本之后同一丛浆果会跳格子。
func TestMapEntsAggregateIntoCells(t *testing.T) {
	m := validMap(8)
	m.Points = nil
	// 世界 32×32 tile、网格 8 → 一格 4 tile。tile 4 落在格 2（floor(4*8/32)+1）。
	m.Ents = [][4]int{
		{4, 4, CatRes, ResSubBerry},
		{5, 5, CatRes, ResSubBerry},
		{4, 4, CatRes, ResSubTree},
		{31, 31, CatDanger, ResSubAny},
		{0, 0, CatCompanion, ResSubAny},
	}
	if err := m.validate(); err != nil {
		t.Fatalf("合法个体点位被拒: %v", err)
	}
	got := m.Aggregated()
	want := [][5]int{
		{1, 1, CatCompanion, 1, ResSubAny},
		// 同一格按子类编号排：树苗(1) 一条、浆果丛(5) 两条
		{2, 2, CatRes, 1, ResSubTree},
		{2, 2, CatRes, 2, ResSubBerry},
		{8, 8, CatDanger, 1, ResSubAny},
	}
	if len(got) != len(want) {
		t.Fatalf("聚合结果 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条 = %v，期望 %v", i, got[i], want[i])
		}
	}
	// 没有 ents 时原样返回 points（旧版助手那条路）。
	m2 := validMap(8)
	if got2 := m2.Aggregated(); len(got2) != len(m2.Points) {
		t.Errorf("只有 points 的旧文件应该原样返回：%v", got2)
	}
}

func TestMapEntsValidation(t *testing.T) {
	cases := []struct {
		name string
		ent  [4]int
	}{
		{"tile 越界右", [4]int{32, 3, CatRes, ResSubBerry}},
		{"tile 负数", [4]int{-1, 3, CatRes, ResSubBerry}},
		{"类别不认识", [4]int{3, 3, 9, ResSubBerry}},
		{"子类不认识", [4]int{3, 3, CatRes, resSubMax + 1}},
		{"非资源带子类", [4]int{3, 3, CatBuild, ResSubBerry}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validMap(8)
			m.Points = nil
			m.Ents = [][4]int{tc.ent}
			if err := m.validate(); err == nil {
				t.Fatalf("脏个体点位被放行了: %+v", tc.ent)
			}
		})
	}
}

func TestMapEmptyFile(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.HelperMapFile("Cluster_1", dst.ShardMaster)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Map("Cluster_1", dst.ShardMaster); err == nil {
		t.Fatal("空文件要报错")
	}
}

// TestMapAcceptsOldFourFieldPoints 钉协议向后兼容：云机上那份助手 MOD 要等分片下次
// 启动才会换成带子类的版本，在那之前它写的还是四元组。四元组必须照常读，子类算"没细分"，
// 否则面板一升级，线上地图的点位就整批判非法、直接画空。
func TestMapAcceptsOldFourFieldPoints(t *testing.T) {
	b, _ := newFixture(t)
	tiles := make([]int, 64)
	for i := range tiles {
		tiles[i] = 7
	}
	body := map[string]any{
		"shard": "Master", "at": b.now().Unix(), "game_id": "G1",
		"width": 32, "height": 32, "grid": 8, "tiles": tiles,
		"points": [][]int{
			{1, 1, CatBuild, 2},            // 旧版四元组
			{2, 2, CatRes, 5, ResSubGrass}, // 新版五元组
			{3, 3, CatCompanion, 1},        // 伙伴随从（切斯特那一类）
		},
		"totals": map[string]int{"build": 2, "res": 5, "companion": 1},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	path := b.Paths.HelperMapFile("Cluster_1", dst.ShardMaster)
	if err := os.WriteFile(path, append([]byte("KLEI     1 "), raw...), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := b.Map("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatalf("新旧混着写也要能读: %v", err)
	}
	if len(m.Points) != 3 {
		t.Fatalf("点位数不对: %+v", m.Points)
	}
	if m.Points[0][4] != ResSubAny {
		t.Errorf("四元组的子类应该补成 0，实际 %d", m.Points[0][4])
	}
	if m.Points[1][4] != ResSubGrass {
		t.Errorf("五元组的子类丢了，实际 %d", m.Points[1][4])
	}
	if m.Points[2][2] != CatCompanion {
		t.Errorf("伙伴类别丢了: %+v", m.Points[2])
	}
	if m.Totals.Companion != 1 {
		t.Errorf("汇总里的伙伴数丢了: %+v", m.Totals)
	}
}
