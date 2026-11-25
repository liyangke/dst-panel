package modbridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"dstpanel/internal/dst"
)

// MapData 是助手 MOD 的 dstpanel_map.json 在面板侧的形状。
//
// tiles 和 points 用紧凑数组而不是对象：一张 144×144 的地形网格有两万多个格子，
// 每个格子写成 {"x":..,"y":..,"tile":..} 光键名就会多出几百 KB。
// points 的每一项是 [格x, 格y, 类别, 数量] 或 [格x, 格y, 类别, 数量, 子类]。
// 第 5 位是后加的：JSON 数组解到定长 Go 数组时，缺的尾部补零值，
// 所以旧版助手写的四元组照样读（子类落到 ResSubAny）。
type MapData struct {
	Shard  string   `json:"shard"`
	At     int64    `json:"at"`
	GameID string   `json:"game_id"`
	Width  int      `json:"width"`
	Height int      `json:"height"`
	Grid   int      `json:"grid"`
	Tiles  []int    `json:"tiles"`
	Points [][5]int `json:"points"`
	// Ents 是逐个实体的 [tileX, tileY, 类别, 子类]，tile 坐标是引擎原始格（0~width-1），
	// 没有被 144 网格量化过——"每丛浆果各画一个图标"要靠它。
	// 新版助手只发这个；Points 留给云机上那份还没重启加载的旧版助手，两条路都得能走。
	Ents      [][4]int  `json:"ents"`
	Totals    MapTotals `json:"totals"`
	Truncated bool      `json:"truncated"`

	UpdatedAt time.Time     `json:"-"`
	Age       time.Duration `json:"-"`
	Stale     bool          `json:"-"`
}

// 点位类别。编号是助手 MOD 和面板之间的协议，中文标签在 api 层，别在两边写死数字。
const (
	CatBuild     = 1
	CatRes       = 2
	CatDanger    = 3
	CatCompanion = 4 // 跟着人走的：切斯特、格罗姆、伯尼、阿比盖尔（引擎的 companion tag）

	catMin = CatBuild
	catMax = CatCompanion
)

// 资源子类，只在类别 2 上有意义。拆到"树/浆果/草"这一层，
// 玩家放大后才看得懂那一格凭什么值得去——只知道"这里有资源"没有决策价值。
const (
	ResSubAny      = 0  // 旧版助手没细分，或不归进下面任何一类
	ResSubTree     = 1  // 树（常青/落叶/丛林树）
	ResSubSapling  = 2  // 树苗
	ResSubGrass    = 3  // 草
	ResSubReeds    = 4  // 芦苇
	ResSubBerry    = 5  // 浆果丛
	ResSubMushroom = 6  // 蘑菇
	ResSubFlower   = 7  // 花
	ResSubCactus   = 8  // 仙人掌
	ResSubStone    = 9  // 石头/岩石/大理石
	ResSubOther    = 10 // 认得出是资源但没对上子类

	resSubMax = ResSubOther
)

type MapTotals struct {
	Build     int `json:"build"`
	Res       int `json:"res"`
	Danger    int `json:"danger"`
	Companion int `json:"companion"`
}

const (
	// mapGridMax 和 mapGridMin 是面板愿意接受的网格边长。
	// 助手现在固定发 144，但协议上留一点余量；超过 256 的网格画到手机上每个格子不足一个像素，
	// 传输和渲染都不划算，也意味着对面写了脏数据。
	mapGridMax = 256
	mapGridMin = 8
	// mapPointsMax 挡住异常放大的点集：正常一个十人基地也就一两千个格子。
	mapPointsMax = 60000
	// mapStaleAfter 是"这份地图已经不能代表现在"的门槛。地形变化慢，
	// 但玩家和新建的建筑会变；超过这个时长的地图只当底图用，界面要标出来。
	mapStaleAfter = 10 * time.Minute
)

// Map 读取并校验一份地图文件。文件缺失返回 os.ErrNotExist，调用方据此决定要不要让助手重新导出。
func (b *Bridge) Map(cluster, shard string) (*MapData, error) {
	path := b.Paths.HelperMapFile(cluster, shard)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w：%s 还没生成，助手 MOD 未加载或还没导出过地图", os.ErrNotExist, path)
		}
		return nil, fmt.Errorf("读取地图文件 %s: %w", path, err)
	}
	raw = dst.StripKleiHeader(raw)
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("地图文件 %s 是空的，助手可能写到一半就退出了", path)
	}
	var m MapData
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("地图文件不是合法 JSON: %w（开头 %q）", err, headOf(raw, 200))
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("地图数据不可用: %w", err)
	}
	if mt, err := os.Stat(path); err == nil {
		m.UpdatedAt = mt.ModTime()
		m.Age = b.now().Sub(m.UpdatedAt)
		if m.Age < 0 {
			m.Age = 0
		}
		m.Stale = m.Age > mapStaleAfter
	}
	return &m, nil
}

func (m *MapData) validate() error {
	switch {
	case m.Grid < mapGridMin || m.Grid > mapGridMax:
		return fmt.Errorf("网格边长 %d 超出 %d~%d", m.Grid, mapGridMin, mapGridMax)
	case m.Width <= 0 || m.Width > 8192 || m.Height <= 0 || m.Height > 8192:
		return fmt.Errorf("世界尺寸 %d×%d 不合理", m.Width, m.Height)
	case len(m.Tiles) != m.Grid*m.Grid:
		return fmt.Errorf("地形格子有 %d 个，按 %d×%d 应该是 %d 个", len(m.Tiles), m.Grid, m.Grid, m.Grid*m.Grid)
	case len(m.Points) > mapPointsMax || len(m.Ents) > mapPointsMax:
		return fmt.Errorf("点位有 %d 个（个体 %d 个），超过上限 %d", len(m.Points), len(m.Ents), mapPointsMax)
	case len(m.Points) == 0 && len(m.Ents) == 0:
		return fmt.Errorf("一个建筑/资源点都没有，这份导出不可信")
	}
	for _, p := range m.Points {
		if p[0] < 1 || p[0] > m.Grid || p[1] < 1 || p[1] > m.Grid {
			return fmt.Errorf("点位坐标 (%d,%d) 超出 %d×%d 网格", p[0], p[1], m.Grid, m.Grid)
		}
		if err := checkCatSub(p[2], p[4], p[0], p[1]); err != nil {
			return err
		}
		if p[3] < 1 {
			return fmt.Errorf("点位 (%d,%d) 的数量是 %d", p[0], p[1], p[3])
		}
	}
	// 个体点位用的是引擎 tile 坐标（不是 144 网格），越界判据也跟着换成世界尺寸。
	for _, e := range m.Ents {
		if e[0] < 0 || e[0] >= m.Width || e[1] < 0 || e[1] >= m.Height {
			return fmt.Errorf("个体点位 (%d,%d) 超出 %d×%d 世界", e[0], e[1], m.Width, m.Height)
		}
		if err := checkCatSub(e[2], e[3], e[0], e[1]); err != nil {
			return err
		}
	}
	return nil
}

// checkCatSub 是两种点位格式共用的类别/子类判据。
func checkCatSub(cat, sub, x, y int) error {
	if cat < catMin || cat > catMax {
		return fmt.Errorf("点位类别 %d 不认识（只有 %d~%d）", cat, catMin, catMax)
	}
	if sub < 0 || sub > resSubMax {
		return fmt.Errorf("点位 (%d,%d) 的资源子类 %d 不认识", x, y, sub)
	}
	// 子类只对资源有意义。别的类别带上子类，说明两边对协议的理解错位了，
	// 与其画出一张说不清什么的图，不如现在就报出来。
	if cat != CatRes && sub != ResSubAny {
		return fmt.Errorf("点位 (%d,%d) 类别 %d 不是资源，却带了子类 %d", x, y, cat, sub)
	}
	return nil
}

// Aggregated 把个体点位按 144 网格聚合成和旧版 points 一样的形状：
// [格x, 格y, 类别, 数量, 子类]，一格多种资源就出多条。
// 界面只认这一种形状，所以新旧助手在这一步汇合。
func (m *MapData) Aggregated() [][5]int {
	if len(m.Ents) == 0 {
		return m.Points
	}
	type cellKey struct{ x, y, cat, sub int }
	counts := map[cellKey]int{}
	order := make([]cellKey, 0, len(m.Ents))
	for _, e := range m.Ents {
		gx := clampInt(e[0]*m.Grid/m.Width+1, 1, m.Grid)
		gy := clampInt(e[1]*m.Grid/m.Height+1, 1, m.Grid)
		k := cellKey{gx, gy, e[2], e[3]}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.y != b.y {
			return a.y < b.y
		}
		if a.x != b.x {
			return a.x < b.x
		}
		if a.cat != b.cat {
			return a.cat < b.cat
		}
		return a.sub < b.sub
	})
	out := make([][5]int, 0, len(order))
	for _, k := range order {
		out = append(out, [5]int{k.x, k.y, k.cat, counts[k], k.sub})
	}
	return out
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
