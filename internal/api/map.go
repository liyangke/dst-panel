// 世界地图接口：地形底图 + 建筑/资源分布 + 玩家位置。
//
// 数据分两条路进来，界面上必须说清楚：
//   - 地形与建筑/资源是助手 MOD 按需导出的（一次 20k 个格子的采样，不可能每秒做）；
//   - 玩家位置直接从每秒刷新的状态快照里取，所以人在动，地图不用跟着重导。
//
// 导出是异步的：第一次打开地图时接口先回 202 + "正在导出"，前端隔两秒再问一次。
// 同步等的话，一个刚进世界的分片要卡住十几秒，用户只会看到转圈。
package api

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"time"

	"dstpanel/internal/modbridge"
)

// 地形和建筑不会一秒一变，重导一次却要在地图线程里采样两万格。
// 页面打开时只有底图超过这个时长才自动重导；想立刻要最新的，界面上有「重新导出」。
const mapAutoRefresh = 10 * time.Minute

// 一次导出正常在几秒内完成；界面上的轮询最多问 30 秒。超过这个时长还等不到新文件，
// 面板就不该继续对外声称"正在导出"，否则重新打开页面会一直挂着假的进行中。
const mapPendingTimeout = 3 * time.Minute

type mapTerrainView struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Cells int    `json:"cells"`
}

func sortedIntKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

type mapCodeView struct {
	Code  int    `json:"code"`
	Key   string `json:"key"`
	Label string `json:"label"`
}

type mapPointView struct {
	X    int    `json:"x"`
	Y    int    `json:"y"`
	Kind string `json:"kind"`
	// KindLabel 是给界面直接显示的"建筑/资源/危险"，不让前端根据 kind 再翻一张表。
	KindLabel string `json:"kindLabel"`
	Count     int    `json:"count"`
	// Sub / SubLabel 只有资源点才有：同一格里的树和浆果丛要能分开说。
	// 旧版助手不带子类时这里留空，界面退回只显示"资源"。
	Sub      string `json:"sub,omitempty"`
	SubLabel string `json:"subLabel,omitempty"`
}

type mapPlayerView struct {
	UserID string `json:"userid"`
	Name   string `json:"name"`
	Prefab string `json:"prefab"`
	Shard  string `json:"shard"`
	// X/Y 是网格坐标（0~grid），没拿到位置时为空，界面把它放进列表但画不到图上。
	X      *float64 `json:"x,omitempty"`
	Y      *float64 `json:"y,omitempty"`
	WorldX *float64 `json:"worldX,omitempty"`
	WorldZ *float64 `json:"worldZ,omitempty"`
	Dead   bool     `json:"dead"`
}

type mapTotalsView struct {
	Build     int `json:"build"`
	Res       int `json:"res"`
	Danger    int `json:"danger"`
	Companion int `json:"companion"`
}

type mapView struct {
	Cluster string `json:"cluster"`
	Shard   string `json:"shard"`
	Ready   bool   `json:"ready"`
	Pending bool   `json:"pending"`
	Note    string `json:"note"`

	UpdatedAt  string  `json:"updatedAt,omitempty"`
	AgeSeconds float64 `json:"ageSeconds,omitempty"`
	Stale      bool    `json:"stale"`

	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	Grid   int `json:"grid,omitempty"`
	// Tiles 是每个格子的地形分组序号，对应 Terrain 数组；界面拿它上色，不接触 Klei 的原始 tile id。
	Tiles   []int            `json:"tiles,omitempty"`
	Terrain []mapTerrainView `json:"terrain,omitempty"`
	Points  []mapPointView   `json:"points,omitempty"`
	// Kinds / Subs 是协议编号到键和中文的字典，界面靠它翻译：
	// 个体点位用紧凑数组传（一万条 × 五个字符串键会把体积翻三倍），
	// 但编号本身不许直接显示给人看，所以字典跟着一起下发。
	Kinds   []mapCodeView   `json:"kinds,omitempty"`
	Subs    []mapCodeView   `json:"subs,omitempty"`
	Ents    [][4]int        `json:"ents,omitempty"`
	Totals  mapTotalsView   `json:"totals"`
	Players []mapPlayerView `json:"players"`
	// Truncated 表示助手那边点位太多，只导出了前一部分。
	Truncated bool `json:"truncated,omitempty"`
}

// 地形分组。Klei 的 tile id 是引擎内部编号，界面上一个都不能出现；
// 这里的键只用于前端配色，Label 才是给人看的。
//
// 编号取自引擎自己的 scripts/constants.lua:686 的 GROUND 表（云机上解开
// data/databundles/scripts.zip 实测），不是凭印象列的：早前这张表按另一版编号写，
// 于是 201~208 那一整片海洋（一张 425×425 的森林图里有 1.05 万个格子、占半数）
// 全部落进"其它地形"，被涂成暗紫红——用户那句"正常情况下海会是红色的吗"就是它。
var mapTerrainDefs = []struct {
	Key   string
	Label string
}{
	{"void", "不可通行"},
	{"road", "道路"},
	{"rocky", "岩石地"},
	{"dirt", "泥地"},
	{"savanna", "稀树草原"},
	{"grass", "草地"},
	{"forest", "森林"},
	{"marsh", "沼泽"},
	{"sand", "沙地"},
	{"cave", "洞穴"},
	{"fungus", "蘑菇地"},
	{"floor", "人造地面"},
	{"web", "蛛网"},
	{"coast", "近岸浅水"},
	{"ocean", "海水"},
	{"farm", "耕地"},
	{"wall", "洞穴壁"},
	{"other", "其它地形"},
}

// terrainGroupOf 把 Klei 的 GROUND 编号归到上面那组。
// 海洋整段（OCEAN_START 201 到 OCEAN_END 247）单独成组，但近岸和shore留一组：
// 玩家看海主要看"哪里能踩、哪里要划船"，把八种海拆成八种颜色只会让图例占满一屏。
// 认不出来的编号（模组地形、引擎新加的噪声格）一律落"其它地形"，不能猜成海。
func terrainGroupOf(tile int) int {
	switch {
	case tile <= 0 || tile == 65535: // 读不到 / INVALID
		return 0
	case tile == 1: // IMPASSABLE
		return 0
	case tile == 2: // ROAD
		return 1
	case tile == 3: // ROCKY
		return 2
	case tile == 4 || tile == 17: // DIRT / MUD
		return 3
	case tile == 5: // SAVANNA
		return 4
	case tile == 6: // GRASS
		return 5
	case tile == 7 || tile == 30: // FOREST / DECIDUOUS
		return 6
	case tile == 8: // MARSH
		return 7
	case tile == 9: // WEB
		return 12
	case tile == 31 || tile == 44: // DESERT_DIRT / SHELLBEACH
		return 8
	case tile == 42: // PEBBLEBEACH：砾石滩，读图时更接近岩石地而不是沙
		return 2
	case tile == 13 || tile == 15 || tile == 16 || tile == 126 || tile == 128: // CAVE / SINKHOLE / UNDERROCK / 洞穴噪声 / UNDERGROUND
		return 9
	case tile == 14 || tile == 24 || tile == 25 || tile == 46 || tile == 120 || tile == 127: // FUNGUS 系
		return 10
	case tile >= 10 && tile <= 12 || tile >= 18 && tile <= 23 || tile == 33 || tile == 34 || tile == 38 || tile == 41 || tile == 45: //
		// 木地板/地毯/棋盘/砖/瓦/饰边（含发光变体）、竞技场与沼泽活动的铺装、遗迹与城市石地
		return 11
	case tile == 201 || tile == 202 || tile == 206: // 海岸与其滩涂、盐池滩
		return 13
	case tile >= 201 && tile <= 247: // OCEAN_START..OCEAN_END
		return 14
	case tile == 47: // FARMING_SOIL：翻过的地，和泥地不是一回事
		return 15
	case tile >= 151 && tile <= 163: // WALL_*
		return 16
	default:
		return 17
	}
}

var mapKinds = map[int]struct {
	Key   string
	Label string
}{
	modbridge.CatBuild:     {"build", "建筑"},
	modbridge.CatRes:       {"res", "资源"},
	modbridge.CatDanger:    {"danger", "危险"},
	modbridge.CatCompanion: {"companion", "伙伴随从"},
}

// 资源子类的中文。编号在 modbridge 里定，这里只负责翻译——
// 界面上永远不出现 1/2/3 这种码值，也不出现 Klei 的 prefab 名。
var mapResSubs = map[int]string{
	modbridge.ResSubAny:      "采集点",
	modbridge.ResSubTree:     "树",
	modbridge.ResSubSapling:  "树苗",
	modbridge.ResSubGrass:    "草",
	modbridge.ResSubReeds:    "芦苇",
	modbridge.ResSubBerry:    "浆果丛",
	modbridge.ResSubMushroom: "蘑菇",
	modbridge.ResSubFlower:   "花",
	modbridge.ResSubCactus:   "仙人掌",
	modbridge.ResSubStone:    "石料",
	modbridge.ResSubOther:    "其它资源",
}

func (s *Server) handleShardMap(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	st, stErr := s.Bridge.Status(cluster, shard)
	md, mdErr := s.Bridge.Map(cluster, shard)
	if mdErr != nil && !errors.Is(mdErr, os.ErrNotExist) {
		// 文件坏了也要走"让助手重导"这条路，不能把 500 扔给用户。
		s.log().Warn("地图文件读取失败", "cluster", cluster, "shard", shard, "err", mdErr.Error())
	}

	force := r.URL.Query().Get("refresh") == "1"
	fresh := mdErr == nil && !md.Stale && md.Age <= mapAutoRefresh && mapMatchesWorld(md, st)
	pending, note := false, ""
	if force || !fresh {
		pending, note = s.requestMapExport(cluster, shard, st, stErr)
	}
	// 排队和落地之间有一段说不清的时间：只看"这份文件够不够新"会提前收工。
	// 用户点重新导出就是要看刚盖下去的基地，队列没跑完之前不能说"已经好了"；
	// 反过来，新文件都已经写回来了，也不该让界面上一直挂着"正在导出"。
	if mdErr == nil {
		outstanding := s.mapOutstanding(cluster, shard, md)
		if pending && !outstanding {
			pending, note = false, ""
		} else if !pending && outstanding {
			pending, note = true, "地图已经在导出队列里，稍等一下。"
		}
	}

	view := mapView{Cluster: cluster, Shard: shard, Players: []mapPlayerView{}, Pending: pending, Note: note}
	if mdErr == nil {
		view = s.buildMapView(cluster, shard, md, st)
		view.Pending = pending
		if note != "" {
			view.Note = note
		}
		if md.Truncated {
			view.Note = joinNote(view.Note, "建筑和资源点太多，图上只保留了前一部分。")
		}
		if stErr != nil || st.Stale {
			view.Note = joinNote(view.Note, "助手 MOD 现在没有回话：底图是上一次导出的，玩家位置可能已经过时。")
		}
		writeOK(w, view)
		return
	}

	view.Players = mapPlayers(st, nil)
	if pending {
		view.Note = joinNote(note, "地形第一次导出通常需要几秒，页面会自动再问一次。")
		writeJSON(w, http.StatusAccepted, reply{OK: true, Data: view})
		return
	}
	if errors.Is(mdErr, os.ErrNotExist) {
		msg := "这个世界还没有导出过地图：助手 MOD 要先跑进存档，并且正在回话。"
		if stErr != nil || st.Stale {
			msg = "地图要由游戏里的助手 MOD 导出，这个世界现在没有回话。先把这个世界开起来，等它进入存档后再回这一页。"
		}
		if note != "" {
			msg = note
		}
		writeErr(w, http.StatusServiceUnavailable, msg)
		return
	}
	writeErr(w, http.StatusServiceUnavailable, "地图文件暂时读不出来，已经让助手重新导出，请稍后再看。")
}

// mapOutstanding 回答"上次排的那次导出落地了没有"。
//
// 判断依据是文件时间戳晚于我们提交导出的时刻：助手写完 dstpanel_map.json，
// mtime 一定晚于排队时间；还是同一份旧文件，就说明这轮导出没跑完。
// 超过 mapPendingTimeout 还没结果的请求直接忘掉——助手可能中途关了，
// 不能让界面永远挂着"正在导出"。
func (s *Server) mapOutstanding(cluster, shard string, md *modbridge.MapData) bool {
	key := cluster + "/" + shard
	s.mapMu.Lock()
	at, ok := s.mapRequests[key]
	if ok && time.Since(at) > mapPendingTimeout {
		delete(s.mapRequests, key)
		ok = false
	}
	s.mapMu.Unlock()
	if !ok {
		return false
	}
	return md == nil || md.UpdatedAt.IsZero() || !md.UpdatedAt.After(at)
}

// requestMapExport 决定"要不要让助手重导一次"，并把重复请求挡在面板侧。
//
// 浏览器里两三个标签页同时开着地图是常态；不做去重的话，一次页面切换会往队列里
// 塞三条 map，助手得连着跑三遍 20k 次地形采样。
func (s *Server) requestMapExport(cluster, shard string, st *modbridge.Status, stErr error) (bool, string) {
	if stErr != nil || st.Stale {
		return false, "助手 MOD 现在没有回话，地图要等这个世界进入存档后才能导出。"
	}

	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	key := cluster + "/" + shard
	if s.mapRequests == nil {
		s.mapRequests = map[string]time.Time{}
	}
	if last, ok := s.mapRequests[key]; ok && time.Since(last) < 8*time.Second {
		return true, "地图已经在导出队列里，稍等一下。"
	}
	if pending, err := s.Bridge.Pending(cluster, shard); err == nil {
		for _, line := range pending {
			if hasCommand(line, modbridge.CmdMap) {
				s.mapRequests[key] = time.Now()
				return true, "地图已经在导出队列里，稍等一下。"
			}
		}
	}
	if _, err := s.Bridge.Enqueue(cluster, shard, modbridge.CmdMap); err != nil {
		return false, publicError(err)
	}
	s.mapRequests[key] = time.Now()
	return true, "正在让助手导出地图，通常几秒内完成。"
}

func hasCommand(line, command string) bool {
	// 队列行是 id|命令|参数…；这里只看第二段，避免把参数里恰好出现 map 当成命令。
	for i := 0; i < len(line); i++ {
		if line[i] != '|' {
			continue
		}
		rest := line[i+1:]
		for j := 0; j < len(rest); j++ {
			if rest[j] == '|' {
				return rest[:j] == command
			}
		}
		return rest == command
	}
	return false
}

func (s *Server) buildMapView(cluster, shard string, md *modbridge.MapData, st *modbridge.Status) mapView {
	v := mapView{
		Cluster: cluster, Shard: shard, Ready: true,
		Width: md.Width, Height: md.Height, Grid: md.Grid,
		Terrain: []mapTerrainView{}, Points: []mapPointView{}, Players: []mapPlayerView{},
		Totals: mapTotalsView{Build: md.Totals.Build, Res: md.Totals.Res,
			Danger: md.Totals.Danger, Companion: md.Totals.Companion},
		Stale: md.Stale, Truncated: md.Truncated,
	}
	if !md.UpdatedAt.IsZero() {
		v.UpdatedAt = md.UpdatedAt.UTC().Format(time.RFC3339)
		v.AgeSeconds = md.Age.Seconds()
	}

	// 先把每格归到分组并统计出现次数，图例只列这张图上真实存在的组。
	groups := make([]int, len(md.Tiles))
	present := make([]int, len(mapTerrainDefs))
	for i, tile := range md.Tiles {
		g := terrainGroupOf(tile)
		groups[i] = g
		present[g]++
	}
	type count struct{ group, cells int }
	var counts []count
	for g, n := range present {
		if n > 0 {
			counts = append(counts, count{g, n})
		}
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].cells != counts[j].cells {
			return counts[i].cells > counts[j].cells
		}
		return counts[i].group < counts[j].group
	})
	viewOf := make([]int, len(mapTerrainDefs))
	for i := range viewOf {
		viewOf[i] = -1
	}
	for i, c := range counts {
		def := mapTerrainDefs[c.group]
		viewOf[c.group] = i
		v.Terrain = append(v.Terrain, mapTerrainView{Key: def.Key, Label: def.Label, Cells: c.cells})
	}
	v.Tiles = make([]int, len(groups))
	for i, g := range groups {
		v.Tiles[i] = viewOf[g]
	}

	for _, p := range md.Aggregated() {
		kind, ok := mapKinds[p[2]]
		if !ok {
			continue
		}
		view := mapPointView{X: p[0] - 1, Y: p[1] - 1, Kind: kind.Key, KindLabel: kind.Label, Count: p[3]}
		if p[2] == modbridge.CatRes {
			if label, ok := mapResSubs[p[4]]; ok && p[4] != modbridge.ResSubAny {
				view.Sub = fmt.Sprintf("sub%d", p[4])
				view.SubLabel = label
			}
		}
		v.Points = append(v.Points, view)
	}
	// 个体点位原样转出去（tile 坐标 + 两个编号），并补上把编号翻译成键和中文的字典。
	if len(md.Ents) > 0 {
		v.Ents = md.Ents
		kindSeen := map[int]bool{}
		subSeen := map[int]bool{}
		for _, e := range md.Ents {
			kindSeen[e[2]] = true
			if e[2] == modbridge.CatRes {
				subSeen[e[3]] = true
			}
		}
		for _, code := range sortedIntKeys(kindSeen) {
			kind, ok := mapKinds[code]
			if !ok {
				continue
			}
			v.Kinds = append(v.Kinds, mapCodeView{Code: code, Key: kind.Key, Label: kind.Label})
		}
		for _, code := range sortedIntKeys(subSeen) {
			label, ok := mapResSubs[code]
			if !ok {
				continue
			}
			v.Subs = append(v.Subs, mapCodeView{Code: code, Key: fmt.Sprintf("sub%d", code), Label: label})
		}
	}
	// 助手没带汇总时用点位自己加一遍；有汇总就尊重它的全量数字（截断时两者会不同）。
	if v.Totals.Build == 0 && v.Totals.Res == 0 && v.Totals.Danger == 0 && v.Totals.Companion == 0 {
		for _, p := range v.Points {
			switch p.Kind {
			case "build":
				v.Totals.Build += p.Count
			case "res":
				v.Totals.Res += p.Count
			case "danger":
				v.Totals.Danger += p.Count
			case "companion":
				v.Totals.Companion += p.Count
			}
		}
	}
	v.Players = mapPlayers(st, md)

	switch {
	case md.Stale:
		v.Note = fmt.Sprintf("这份底图是 %s 前导出的；地形一般没变，但新建的建筑和资源要等下一次刷新。", md.Age.Round(time.Second))
	}
	return v
}

func mapMatchesWorld(md *modbridge.MapData, st *modbridge.Status) bool {
	if md == nil || st == nil || md.GameID == "" || st.GameID == "" {
		return true
	}
	return md.GameID == st.GameID
}

// mapPlayers 把状态快照里的世界坐标换成网格坐标。
//
// 世界坐标以地图中心为原点，一格 4 个世界单位：格 x = 世界x/4 + 宽/2。
// 这个换算和助手导出地形时采样用的公式是同一个，所以玩家标记会落在正确的格子上。
func mapPlayers(st *modbridge.Status, md *modbridge.MapData) []mapPlayerView {
	out := []mapPlayerView{}
	if st == nil || st.Stale || st.Error != "" {
		return out
	}
	shard := st.Shard
	for _, p := range st.Players {
		v := mapPlayerView{UserID: p.UserID, Name: p.Name, Prefab: p.Prefab, Shard: shard, Dead: p.IsDead}
		if md != nil && p.X != nil && p.Z != nil && md.Width > 0 && md.Height > 0 {
			gx := (*p.X/4 + float64(md.Width)/2) / float64(md.Width) * float64(md.Grid)
			gy := (*p.Z/4 + float64(md.Height)/2) / float64(md.Height) * float64(md.Grid)
			gx = math.Max(0, math.Min(gx, float64(md.Grid)))
			gy = math.Max(0, math.Min(gy, float64(md.Grid)))
			v.X, v.Y = &gx, &gy
			v.WorldX, v.WorldZ = p.X, p.Z
		}
		out = append(out, v)
	}
	return out
}
