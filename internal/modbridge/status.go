package modbridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"dstpanel/internal/dst"
)

// World 对应 panel_bridge.lua 里 collect_status 的 world 字段。
// 取值来自服务端 scripts/components/worldstate.lua：季节是 autumn/winter/spring/summer，
// 时段是 day/dusk/night/full/new（后两个是满月与新月）。
//
// 这里没有 day：引擎的世界状态上不存在 day 这个字段（实测：把这版服务端的 4095 个 lua
// 全文搜 state.day，一处都没有），Klei 自己也是从 cycles 现算的，见 DayNumber。
// 曾经在这里放过一个 Day float64 去读那个不存在的键，于是界面上每个房间都报「第 0 天」。
type World struct {
	Cycles      float64 `json:"cycles"`
	Season      string  `json:"season"`
	Phase       string  `json:"phase"`
	Temperature float64 `json:"temperature"`
	Wetness     float64 `json:"wetness"`
	// 这一季已经过完几天、还剩几天。引擎里是 components/seasons.lua 维护的两个网络读数，
	// 换季时过完的天数归 0，再由 worldstate.lua:122-123 复制进世界状态。
	//
	// 用指针：过完 0 天是"这一季第 1 天"这个真事的合法读数，跟"引擎没给这个键"
	// 不能都显示成第 1 天——后者该什么都不说。
	ElapsedDaysInSeason   *float64 `json:"elapsed_days_in_season"`
	RemainingDaysInSeason *float64 `json:"remaining_days_in_season"`
}

// 无尽模式里引擎把"还剩几天"报成 seasons.lua 的 ENDLESS_DAYS（10000）这个哨兵值，
// 固定季节的世界则让"这一季第几天"一路涨过任何真实季节长度。这两种情况下
// "第 3 季 / 还剩 5 天"这句话都没有意义，界面上就不说，而不是编一个数。
// 90 这个上界是照 Klei 的季节长度配置算的：默认一档一个季节 20 天，最长一档翻倍到 40，
// 而春、秋那两季的 totaldaysinseason 又被人为乘了 2，所以留到 90 才判死。
const (
	endlessRemainingSentinel = 1000
	maxPlausibleSeasonDays   = 90
)

// DayInSeason 是界面上的"这一季第几天"：过完的天数是 0 起算，所以 +1，
// 与 DayNumber 对 cycles 的处理同一个道理。第二个返回值表示这个数有没有依据。
func (w World) DayInSeason() (float64, bool) {
	if !w.seasonRotates() {
		return 0, false
	}
	day := math.Floor(*w.ElapsedDaysInSeason) + 1
	if day > maxPlausibleSeasonDays {
		return 0, false
	}
	return day, true
}

// DaysLeftInSeason 是"这一季还剩几天"。没有依据时（季节不轮换、或这一份引擎没这个键）
// 第二个返回值为 false，界面不说"还剩 0 天"这种会造成恐慌的假消息。
func (w World) DaysLeftInSeason() (float64, bool) {
	if !w.seasonRotates() {
		return 0, false
	}
	left := math.Floor(*w.RemainingDaysInSeason)
	if left < 0 {
		return 0, false
	}
	return left, true
}

// seasonRotates 判断这个世界是不是真的在按季轮换。两个读数缺一个就没法说，
// "还剩"是哨兵值时是无尽模式，而"过完的天数"涨过任何真实季节长度时是固定季节：
// 这两种情况下"第几天 / 还剩几天"这句话本身都不成立，不能只挡住其中一句。
func (w World) seasonRotates() bool {
	if w.ElapsedDaysInSeason == nil || w.RemainingDaysInSeason == nil {
		return false
	}
	return *w.RemainingDaysInSeason < endlessRemainingSentinel &&
		*w.ElapsedDaysInSeason < maxPlausibleSeasonDays
}

// DayNumber 是界面上的"第几天"，换算照 Klei 自己的写法：
//   - scripts/components/worldstate.lua:277 GetWorldAge() 返回 1 + cycles + time；
//   - scripts/prefabs/world.lua:321 直接写 day = inst.state.cycles + 1；
//   - 游戏里那个钟 scripts/widgets/uiclock.lua:153 用的也是 cycles + 1。
//
// cycles 是"已经过完几个整天"，开局那天还没过完所以是 0，界面就该说第 1 天。
// time（当天走了几分之几）助手没有上报，所以这里给的是整数天，不编一个小数出来。
func (w World) DayNumber() float64 {
	return math.Floor(w.Cycles) + 1
}

func (w World) SeasonLabel() string { return SeasonLabel(w.Season) }
func (w World) PhaseLabel() string  { return Label(phaseLabels, w.Phase) }

// SeasonLabel 单独露出来是因为公网房间列表（internal/lobby）也报季节。
// 这张表要是两处各抄一份，改一边忘一边就是界面上的"码值泄漏"。
func SeasonLabel(season string) string { return Label(seasonLabels, season) }

// Player 是一个玩家的身体状况。字段与 panel_bridge.lua 的 collect_player 一一对应。
//
// 数值字段用指针：Lua 侧只在组件存在时才写这个键。用普通 float 的话，
// "没有饥饿组件"会显示成"饥饿值 0"，看的人以为玩家要饿死了，这属于把缺失数据编成坏消息。
type Player struct {
	Name      string   `json:"name"`
	Prefab    string   `json:"prefab"`
	UserID    string   `json:"userid"`
	Health    *float64 `json:"health"`
	MaxHealth *float64 `json:"max_health"`
	IsDead    bool     `json:"is_dead"`
	Hunger    *float64 `json:"hunger"`
	MaxHunger *float64 `json:"max_hunger"`
	Sanity    *float64 `json:"sanity"`
	MaxSanity *float64 `json:"max_sanity"`
	X         *float64 `json:"x"`
	Z         *float64 `json:"z"`
	Items     *int     `json:"items"`
}

// Status 是一次快照。JSON 之外的三个字段由面板补齐，用来告诉界面数据可不可信。
type Status struct {
	Shard       string   `json:"shard"`
	Seq         int64    `json:"seq"`
	GameID      string   `json:"game_id"`
	Heartbeat   float64  `json:"heartbeat"`
	ServerName  string   `json:"server_name"`
	MaxPlayers  int      `json:"max_players"`
	World       World    `json:"world"`
	Players     []Player `json:"players"`
	PlayerCount int      `json:"player_count"`
	// Error 非空表示 MOD 采集快照时抛了异常，异常文本写在这里。
	Error string `json:"error"`

	UpdatedAt time.Time     `json:"-"`
	Age       time.Duration `json:"-"`
	Stale     bool          `json:"-"`
}

var seasonLabels = map[string]string{
	"autumn": "秋天",
	"winter": "冬天",
	"spring": "春天",
	"summer": "夏天",
}

var phaseLabels = map[string]string{
	"day":   "白天",
	"dusk":  "黄昏",
	"night": "夜晚",
	"full":  "满月",
	"new":   "新月",
}

// Label 把游戏内部的英文取值换成界面用中文。查不到就退回原值，
// 宁可显示一个陌生的英文单词，也不能显示空白或码值占位符。
func Label(table map[string]string, v string) string {
	if v == "" {
		return "未知"
	}
	if l, ok := table[v]; ok {
		return l
	}
	return v
}

// Status 读取并判定快照新鲜度。
//
// 新鲜度看文件修改时间而不是 MOD 写的 heartbeat：Lua 那边只有 os.clock()，
// 它是进程 CPU 时间，分片空转时几乎不涨，拿来判断在线会误报。
func (b *Bridge) Status(cluster, shard string) (*Status, error) {
	path := b.Paths.HelperStatusFile(cluster, shard)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w：%s 还没生成，助手 MOD 未加载或世界从未存过档", os.ErrNotExist, path)
		}
		return nil, fmt.Errorf("读取状态数据 %s: %w", path, err)
	}
	// status.json 是 MOD 写的，开头带引擎加的 "KLEI     1 " 头，原理见 dst.StripKleiHeader。
	raw = dst.StripKleiHeader(raw)
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("状态数据 %s 是空的，助手 MOD 可能写到一半就退出了", path)
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("状态数据不是合法 JSON: %w（开头 %q）", err, headOf(raw, 200))
	}
	mt, err := os.Stat(path)
	if err == nil {
		st.UpdatedAt = mt.ModTime()
		st.Age = b.now().Sub(st.UpdatedAt)
		if st.Age < 0 {
			// 面板与游戏同机，正常不会出现；时钟被往回调过就会。
			st.Age = 0
		}
		st.Stale = st.Age > b.staleAfter()
	}
	return &st, nil
}

// StatusOrStale 给状态页用：文件缺失时也要返回一个可渲染的结构，
// "还没有数据"和"数据过期"在界面上是两件事，但都不该让请求失败。
func (b *Bridge) StatusOrStale(cluster, shard string) *Status {
	st, err := b.Status(cluster, shard)
	if err != nil {
		return &Status{Shard: shard, Stale: true}
	}
	return st
}

func headOf(raw []byte, n int) string {
	s := string(raw)
	if len(s) > n {
		s = s[:n]
	}
	return s
}
