// 公网房间列表的接口：把 internal/lobby 读来的那一份 Klei 公开快照，
// 变成「公网房间」那一页上能和自家房间并列浏览的那批行。
//
// 这一组和 /api/v1/clusters 那组的关键区别，界面必须替用户说清：
// 前者是这台机器上的房间（面板能启停、能改配置），后者是别人开的房间（只能看、只能复制连接命令）。
// 所以这里的每一个响应都带一句 note，而不是让界面自己去猜该不该给「启动」按钮。
package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dstpanel/internal/lobby"
)

// defaultLobbyRegion 是不带 region 参数时给的那一个大区。
// 取白名单第一个（亚太）：用面板的人在国内，这份列表里命中他们附近房间的概率最高。
const defaultLobbyRegion = "ap-southeast-1"

// lobbySourceNote 解释这一屏数据的来历。摆在列表上方，而不是藏在角标里——
// "这份列表是几点抓的、为什么上面那一间游戏里连不上"是这一页最容易误读的两件事。
const lobbySourceNote = "这里读的是 Klei 放在 CDN 上的公开房间快照，和游戏里「加入房间」那个列表同源：不要账号、不填任何凭据。" +
	"快照本身有延迟，而且房主随时可能已经关了房，所以「能不能进」要以游戏里真连一次为准。"

// lobbyConnectNote 是复制连接命令那一句的用法说明。
// 单独一条是因为游戏客户端根本没有 IP 输入框（实测：手动添加只能进控制台敲 c_connect），
// 不写出来的话用户会拿着一个 IP 在界面里找一个不存在输入框。
// 句子里指的是那一行右边的按钮，不是"下面那句"：命令默认只挂在按钮的悬停标题上，
// 说"下面这句"会让人在页面上找一行并不存在的字（复制失败那一支才会把它摊出来）。
const lobbyConnectNote = "游戏里没有\"按 IP 加入\"的输入框：按一行右边的「复制连接」拿到那句 c_connect，" +
	"进游戏后按 ` 打开控制台，粘贴进去再回车。"

type lobbyRegionView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Note  string `json:"note"`
}

type lobbyRegionsView struct {
	Enabled bool              `json:"enabled"`
	Regions []lobbyRegionView `json:"regions"`
	// DefaultRegion 是界面进来时该选中的那一个大区，省得前端自己复制这个常量。
	DefaultRegion string `json:"defaultRegion"`
	Note          string `json:"note"`
	Problem       string `json:"problem,omitempty"`
}

type lobbyIntentView struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type lobbyRoomView struct {
	lobby.Room
	// AccessLabel 把 password / clanonly / lanonly / allownewplayers / serverpaused 合成
	// 一句中文。界面上不摆这五个布尔的码值，也不该让前端各判一次——那五栏并排放出来
	// 只是一排勾叉，没人读得懂"fo: true"是什么意思。
	AccessLabel string `json:"accessLabel"`
	// Joinable 是"这一间值不值得点复制"：有空位、不收新人没关、也没被暂停。
	// 由后端算，界面用它把明显进不去的行摆到后面并标灰，而不是给每一行一个按下去连不上的按钮。
	Joinable bool `json:"joinable"`
}

type lobbyRoomsView struct {
	Enabled bool `json:"enabled"`
	// RegionLabel / RegionNote 由后端拼：前端拿不到那张中文表。
	Region      string `json:"region"`
	RegionLabel string `json:"regionLabel"`
	RegionNote  string `json:"regionNote"`
	// Live 表示这一次真的去打到了 Klei；命中缓存时为 false（界面不显示"正在取"那种转圈）。
	Live bool `json:"live"`
	// Stale 是"这一屏是上次成功留下的"，StaleWhy 是这一次为什么没刷动。
	Stale    bool   `json:"stale"`
	StaleWhy string `json:"staleWhy,omitempty"`

	FetchedAt   string `json:"fetchedAt"`
	FetchedAgo  string `json:"fetchedAgo"`
	DecodedSize string `json:"decodedSize"`
	// SourceRecords 是 Klei 这一份里一共多少条，UsableRooms 是面板收下多少条。
	// 两个数分开摆：正常情况下差 0，一旦哪天字段变了形导致大量条目被滤掉，
	// 界面能说出"16932 条里只认出 3 间"而不是默默给出 3 行。
	SourceRecords int `json:"sourceRecords"`
	UsableRooms   int `json:"usableRooms"`
	WithPlayers   int `json:"withPlayers"`

	// Intents 只列这份列表里真出现过的玩法。下拉里摆一个这份列表一间都没有的选项，
	// 点下去必然是空表格。
	Intents []lobbyIntentView `json:"intents"`
	Sorts   []lobbyIntentView `json:"sorts"`

	Rooms   []lobbyRoomView `json:"rooms"`
	Total   int             `json:"total"`
	Page    int             `json:"page"`
	Size    int             `json:"size"`
	Pages   int             `json:"pages"`
	Matched int             `json:"matched"`

	Query    lobbyQueryView `json:"query"`
	Note     string         `json:"note"`
	JoinNote string         `json:"joinNote"`
}

// lobbyQueryView 把这次生效的筛选条件回给界面。
// 回一份是"筛选状态跟着接口走"：切大区、翻页、浏览器后退之后，界面上的下拉不该还留着上一次的选项。
type lobbyQueryView struct {
	Text    string `json:"text"`
	Intent  string `json:"intent"`
	Access  string `json:"access"`
	Slots   string `json:"slots"`
	Players string `json:"players"`
	Sort    string `json:"sort"`
}

// 排序选项的中文标签。Value 是接口参数，Label 是界面上那两个字，
// 两者一起回给前端，免得翻译表在前端再抄一份（抄了就会改一边忘一边）。
var lobbySorts = []lobbyIntentView{
	{Value: "players", Label: "在线人多在前"},
	{Value: "free", Label: "空位多在前"},
	{Value: "capacity", Label: "人数上限大在前"},
}

func (s *Server) handleLobbyRegions(w http.ResponseWriter, r *http.Request) {
	v := lobbyRegionsView{
		Enabled:       s.Lobby.Enabled(),
		Regions:       []lobbyRegionView{},
		DefaultRegion: defaultLobbyRegion,
		Note:          lobbySourceNote,
	}
	for _, reg := range lobby.Regions() {
		v.Regions = append(v.Regions, lobbyRegionView{ID: reg.ID, Label: reg.Label, Note: reg.Note})
	}
	if !v.Enabled {
		v.Problem = "配置里 lobby.enabled 是关的，这一页不会去连 Klei 的服务器"
	}
	writeOK(w, v)
}

func (s *Server) handleLobbyRooms(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query()
	regionID := strings.TrimSpace(raw.Get("region"))
	if regionID == "" {
		regionID = defaultLobbyRegion
	}
	reg, ok := lobby.LookupRegion(regionID)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"没有这个大区：%s。面板只读这几个：%s", regionID, strings.Join(lobbyRegionLabels(), "、")))
		return
	}
	page, size, err := lobbyPaging(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	q, err := lobbyQuery(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// refresh=1 是界面上那颗「刷新」：绕过 TTL 和失败冷却，真去打一次 Klei。
	// 为什么必须留这个口子：不带它的话，按钮在 TTL 内按下去只会拿到同一份数据，
	// 等于给用户一个按下去什么都不发生的按钮（自动轮询一律不带，见 lobby.Cache 的注释）。
	force := strings.TrimSpace(raw.Get("refresh")) == "1"

	var get = s.Lobby.Get
	if force {
		get = s.Lobby.GetForce
	}
	res, err := get(r.Context(), reg.ID)
	if err != nil {
		// 一次都没成功过、这次又刷不动：回 503 并把那句中文原样给界面。
		// 不降级成"空列表 + ok"，那样界面会显示"这个大区一间房都没有"，
		// 而真相是这台机器出不去公网——两句话的下一步操作完全相反。
		s.fail(w, r, http.StatusServiceUnavailable, err)
		return
	}

	cut := res.Snap.Apply(lobby.Query{
		Text: q.Text, Intent: q.Intent, Access: q.Access,
		Slots: q.Slots, Players: q.Players, Sort: q.Sort,
		Page: page, Size: size,
	})

	v := lobbyRoomsView{
		Enabled:       s.Lobby.Enabled(),
		Region:        reg.ID,
		RegionLabel:   reg.Label,
		RegionNote:    reg.Note,
		Live:          !res.Stale,
		Stale:         res.Stale,
		StaleWhy:      res.WhyStale,
		FetchedAt:     res.Snap.FetchedAt.UTC().Format(time.RFC3339),
		FetchedAgo:    humanAgo(res.Snap.Age()),
		DecodedSize:   humanBytes(int64(res.Snap.Decoded)),
		SourceRecords: res.Snap.Total,
		UsableRooms:   len(res.Snap.Rooms),
		WithPlayers:   res.Snap.WithPlayers(),
		Intents:       []lobbyIntentView{},
		Sorts:         lobbySorts,
		Rooms:         []lobbyRoomView{},
		Total:         cut.Total,
		Page:          cut.Page,
		Size:          cut.Size,
		Pages:         cut.Pages,
		Matched:       cut.Matched,
		Query:         q,
		Note:          lobbySourceNote,
		JoinNote:      lobbyConnectNote,
	}
	for _, it := range res.Snap.Intents() {
		v.Intents = append(v.Intents, lobbyIntentView{Value: it, Label: lobby.IntentLabel(it)})
	}
	for _, room := range cut.Rooms {
		v.Rooms = append(v.Rooms, lobbyRoomView{
			Room:        room,
			AccessLabel: roomAccessLabel(room),
			Joinable:    room.Free > 0 && room.AllowNew && !room.Paused,
		})
	}
	writeOK(w, v)
}

// roomAccessLabel 把那几个门槛布尔合成一句中文。
// 顺序是有意的：先说要不要密码（最常见的挡），再说战队/内网/收不收新人，
// 最后才是"房主按了暂停"——那一条不是门槛而是"进去也动不了"。
func roomAccessLabel(r lobby.Room) string {
	bits := []string{}
	if r.Password {
		bits = append(bits, "要密码")
	}
	if r.ClanOnly {
		bits = append(bits, "只收战队成员")
	}
	if r.LanOnly {
		bits = append(bits, "只有局域网能连")
	}
	if !r.AllowNew {
		bits = append(bits, "不收新玩家")
	}
	if r.Paused {
		bits = append(bits, "房主暂停了游戏")
	}
	if r.KleiOfficial {
		bits = append(bits, "Klei 官方服")
	}
	if len(bits) == 0 {
		return "没有门槛"
	}
	return strings.Join(bits, "、")
}

func lobbyRegionLabels() []string {
	out := make([]string, 0, 3)
	for _, reg := range lobby.Regions() {
		out = append(out, reg.Label+"（"+reg.ID+"）")
	}
	return out
}

func lobbyPaging(q url.Values) (page, size int, err error) {
	page, size = 1, 20
	if raw := strings.TrimSpace(q.Get("page")); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 1 {
			return 0, 0, fmt.Errorf("page 要是从 1 开始的整数，收到的是 %q", raw)
		}
		page = n
	}
	if raw := strings.TrimSpace(q.Get("size")); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 1 || n > 100 {
			return 0, 0, fmt.Errorf("size 要是 1~100 的整数，收到的是 %q（公网列表一行就七个字段，一屏超过一百条读不动）", raw)
		}
		size = n
	}
	return page, size, nil
}

// lobbyQuery 收下这一排筛选条件。四个单选参数只认列出来的那几个值：
// 填歪了必须当场说一句中文，而不是当成"这一条不筛"继续跑——
// 后者会让前端的一个拼写错误表现成"筛选没生效"，那是最难往回查的症状。
func lobbyQuery(q url.Values) (lobbyQueryView, error) {
	out := lobbyQueryView{Text: strings.TrimSpace(q.Get("q"))}
	var err error
	// intent 不查白名单：它的取值来自 Klei 那份数据本身（房主上报的一个词），
	// 面板不能假定自己认识全部取值。只拦长得离谱的。
	intent := strings.TrimSpace(q.Get("intent"))
	if len([]rune(intent)) > 40 {
		return out, fmt.Errorf("intent 太长了（最多 40 个字），收到的是 %d 个字", len([]rune(intent)))
	}
	out.Intent = intent
	if out.Access, err = lobbyChoice(q.Get("access"), "access", "留空表示两种都要", "open", "locked"); err != nil {
		return out, err
	}
	if out.Slots, err = lobbyChoice(q.Get("slots"), "slots", "留空表示两种都要", "free", "full"); err != nil {
		return out, err
	}
	if out.Players, err = lobbyChoice(q.Get("players"), "players", "留空表示两种都要", "has", "none"); err != nil {
		return out, err
	}
	if out.Sort, err = lobbyChoice(q.Get("sort"), "sort", "留空按在线人多在前排", "players", "free", "capacity"); err != nil {
		return out, err
	}
	// 没带 sort 时把这枚默认值说出口，而不是让它保持空字符串。
	// 排序本身没坏：lobby.sortRooms 的 default 分支就是"在线人多在前"，空值走进去照样排。
	// 坏的是回给界面的那一份 query.sort——它是空字符串，而界面上那枚排序下拉里没有值为空的选项，
	// 浏览器只能把一个空白框画在那里（实测截图就是这样）。用户看不出这一屏是按什么排的，
	// 也没法确认"我选的这项生效了没有"。默认值由后端定下来，前端跟着它填框，两边只有一个来源。
	if out.Sort == "" {
		out.Sort = "players"
	}
	return out, nil
}

// lobbyChoice 认一枚单选参数。留空时的那句说明由调用方给（emptyNote）：
// 四枚里三枚留空真是"不筛"，唯独 sort 留空是"按默认那一枚排"，
// 一句"留空表示不筛"套在 sort 上就成了假话——用户以为列表没排，实际排了。
func lobbyChoice(v, name, emptyNote string, allowed ...string) (string, error) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", nil
	}
	for _, a := range allowed {
		if a == t {
			return t, nil
		}
	}
	return "", fmt.Errorf("%s 只能填 %s（%s），收到的是 %q", name, strings.Join(allowed, "、"), emptyNote, v)
}
