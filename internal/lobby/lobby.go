// Package lobby 读饥荒联机版的公网房间列表（游戏里那个"加入房间"界面背后的同一份数据）。
//
// 为什么读 CDN 上的快照，而不是大厅接口：lobby-v2.klei.com 的 /lobby/* 每一个都要
// 玩家账号的 Bearer 会话（2026-11-02 在云机上实测：只带房间令牌一律 403
// E_AUTH_FAILURE）。要走到那一步就得存下用户的 Klei 登录态，而面板是一个服务端管理工具，
// 不该拿玩家的账号。CDN 上这份是同一个数据源的公开快照，不要凭据、不带 cookie。
//
// 实测口径（2026-11-03，脚本 .tools/tmp/lobby_probe.py，三个地区都跑过）：
//   - 地址形如 <base>/ap-southeast-1-Steam.json.gz；响应头写的是
//     Content-Type: application/json，**没有 Content-Encoding**，但正文前两字节是
//     1f 8b（gzip）。所以只能自己认魔数解压——Go 的 Transport 只在响应头写了
//     Content-Encoding 时才帮忙解，这里它帮不上。
//   - 顶层只有一个 "GET" 键，值是房间数组。三个地区都在：
//     ap-southeast-1 16932 间（解压后 15.3 MB）、us-east-1 1523 间、eu-central-1 2721 间。
//     亚太那份一天内从 6958 涨到 16932（2026-11-03 两次实测），条数是会变的量，
//     所以这一页所有"共几间"都从接口现算，没有一个数字写死在代码里。
//   - 字段是否出现因地区而异（亚太这份 tags 有 16930/16932；上一轮实测 kleiofficial 只有 10 间带），
//     所以每个字段都按"可能没有"来解；数值字段实测全为 int（不像 Steam 那样会变字符串）。
//
// 一条贯穿到底的边界：region 参数只能拿去查下面的白名单，绝不能拼进 URL。
// 面板跑在用户的机器上，能任意拼地址就等于交出去一个内网探测器。
package lobby

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"dstpanel/internal/modbridge"
)

// BucketBase 是 Klei 放这份快照的 CDN 前缀。它是代码里的常量，不是配置项：
// 配置里能改一个 outbound 地址，前面那句白名单就白立了。
const BucketBase = "https://lobby-v2-cdn.klei.com"

// MaxDecodedBytes 是"解压后最多收多少字节"。实测亚太那份解压后 15.3 MB（2026-11-03 早上），
// 同一天早些时候还是 6.8 MB，另两份 1.2 / 2.1 MB。
// 上限放到 48 MB：留出约三倍的余量——这一份的条数是会涨的，撞线不该是正常增长的结果。
// 它拦的是 Klei 改格式或 CDN 把别的东西当这份文件发回来那种情况，
// 让面板不至于在 4G 的小机上把内存吃穿，而撞线时界面会明说"超过上限"而不是猜。
const MaxDecodedBytes = 48 << 20

// Region 是一个大区对应的那一份列表。
type Region struct {
	// ID 既是接口参数，也是拼 URL 用的那一段——只有出现在这张表里的 ID 才拼得出去。
	ID    string `json:"id"`
	Label string `json:"label"`
	// Note 是"这一份里装的是谁的房间"，界面摆在地区下拉下面。
	Note string `json:"note"`
}

var regions = []Region{
	{ID: "ap-southeast-1", Label: "亚太", Note: "亚洲这一片的房间都登记在这里，国内的服务器多数出现在这份列表里"},
	{ID: "us-east-1", Label: "美东", Note: "北美东部的大区，房间数比亚太少一个量级"},
	{ID: "eu-central-1", Label: "欧洲中部", Note: "欧洲房间的大区"},
}

// Regions 返回白名单的副本。给副本是因为界面会把它当列表渲染，
// 谁也不该拿到能改动这张表的切片。
func Regions() []Region {
	out := make([]Region, len(regions))
	copy(out, regions)
	return out
}

func LookupRegion(id string) (Region, bool) {
	id = strings.TrimSpace(id)
	for _, r := range regions {
		if r.ID == id {
			return r, true
		}
	}
	return Region{}, false
}

func regionIDs() string {
	ids := make([]string, 0, len(regions))
	for _, r := range regions {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, "、")
}

// IntentLabel 把大厅里的 intent 翻成中文。
//
// 这张表覆盖的是实测三个地区里出现过的全部取值。查不到时原样返回：
// 那是房主那一侧上报的一个词，不是面板内部的码值，藏起来等于把这一间房的信息删掉一栏。
func IntentLabel(intent string) string {
	if strings.TrimSpace(intent) == "" {
		return ""
	}
	if l, ok := intentLabels[intent]; ok {
		return l
	}
	return intent
}

var intentLabels = map[string]string{
	"survival":     "生存",
	"endless":      "无尽",
	"wilderness":   "荒野",
	"relaxed":      "轻松",
	"lightsout":    "关灯",
	"oceanfishing": "海洋垂钓",
}

// Room 是别人开的一个房间，只保留"要不要点进去看"用得到的那些列。
//
// 有意丢掉的四类字段：
//   - 标识符（guid / session / steamid / ownernetid / __rowId / host）：这一页是挑房间，
//     不是查户；把房主的 KU_ 与 SteamID 摆出来只是凭白暴露。
//   - dedicated 与 clienthosted：实测同一条记录里两个都是 true，这对不上"专用/房主开"
//     这种非此即彼的说法，解释不了就不上图。
//   - v（如 761239）：形状像构建号，但面板拿不到引擎的对照表，认不出来就别写成"版本"。
//   - mods（布尔）与 tags 里的模组名有重叠，而 tags 本身要原样给界面，够了。
type Room struct {
	Name string `json:"name"`
	// Addr 与 Port 是玩家在游戏控制台里 c_connect 的那一对，来自 __addr / port。
	Addr string `json:"addr"`
	Port int    `json:"port"`

	// Players 是这一刻在里面的人数（connected），Capacity 是上限（maxconnections）。
	Players  int `json:"players"`
	Capacity int `json:"capacity"`
	// Free 由面板算出来，不是 Klei 的字段：负数一律按 0 摆，
	// 因为"空位 -1"看着像数据而实际上是人数超了上限，那该由人数那一列自己说。
	Free int `json:"free"`

	// Intent / Season 是原始取值，配 IntentLabel / SeasonLabel 中文。
	Intent      string `json:"intent"`
	IntentLabel string `json:"intentLabel"`
	Season      string `json:"season,omitempty"`
	SeasonLabel string `json:"seasonLabel,omitempty"`

	Password bool `json:"password"`
	ClanOnly bool `json:"clanOnly"`
	LanOnly  bool `json:"lanOnly"`
	Pvp      bool `json:"pvp"`
	AllowNew bool `json:"allowNew"`
	// FullAccess 不是大厅的字段，是面板算出来的"门槛"汇总：没密码、不限战队、
	// 不限局域网、还收新人。界面用它把"点了也进不来"的房间标出来，见 api/lobby.go。
	FullAccess   bool `json:"fullAccess"`
	Paused       bool `json:"paused"`
	KleiOfficial bool `json:"kleiOfficial"`
	// Tags 是房主自己填的标签，Klei 用逗号拼成一个字符串。原样给界面，不翻译不归类。
	Tags []string `json:"tags"`
}

// Snapshot 是一个大区的一次成功拉取。
type Snapshot struct {
	Region    string    `json:"region"`
	FetchedAt time.Time `json:"fetchedAt"`
	Rooms     []Room    `json:"rooms"`
	// Total 是 Klei 这一份里有多少条记录，Decoded 是解压后的字节数。
	// 两个数分开是因为面板会滤掉没有地址或没有端口的条目，界面得说清少了多少、为什么少。
	Total   int `json:"total"`
	Decoded int `json:"decodedBytes"`
}

// WithPlayers 是"这一刻有人在里面的房间数"。界面按它解释"在线优先"这一排序有没有东西可排。
func (s *Snapshot) WithPlayers() int {
	n := 0
	for i := range s.Rooms {
		if s.Rooms[i].Players > 0 {
			n++
		}
	}
	return n
}

// PauseNote 说这一份列表有多旧。CDN 上的快照本身就有延迟（Klei 那边多久刷一次没公开），
// 所以界面只报"这是几点取到的"，不承诺它是此刻的状态。
func (s *Snapshot) Age() time.Duration {
	if s == nil {
		return 0
	}
	d := time.Since(s.FetchedAt)
	if d < 0 {
		return 0
	}
	return d
}

// ---- 拉取 ----

// Fetcher 是这份快照的只读客户端。nil 也可以安全调用 Enabled。
type Fetcher struct {
	BaseURL string
	HTTP    *http.Client
	// Timeout 是单次拉取的上限。列表有十几 MB，走代理的机器上十几秒是正常的，
	// 所以这一档不能按 API 请求的 3 秒来定。
	Timeout time.Duration
}

// NewFetcher 造客户端。proxy 为空时跟随 HTTPS_PROXY 环境变量，
// 和 steam 包同一套写法——国内机房出不去的时候，两个功能要的是同一个出路。
func NewFetcher(proxy string, timeout time.Duration) *Fetcher {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        4,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if u, err := url.Parse(p); err == nil && u.Scheme != "" {
			tr.Proxy = func(*http.Request) (*url.URL, error) { return u, nil }
		}
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Fetcher{
		BaseURL: BucketBase,
		HTTP:    &http.Client{Timeout: timeout, Transport: tr},
	}
}

func (f *Fetcher) base() string {
	if f == nil || f.BaseURL == "" {
		return BucketBase
	}
	return strings.TrimRight(f.BaseURL, "/")
}

func (f *Fetcher) httpClient() *http.Client {
	if f != nil && f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Fetch 取一个大区的列表。regionID 必须在白名单里，不在就直接失败——
// 这是"参数会变成 URL"的唯一入口，也是这个包唯一的安全边界。
func (f *Fetcher) Fetch(ctx context.Context, regionID string) (*Snapshot, error) {
	if f == nil {
		return nil, errors.New("面板没有接上公网房间列表")
	}
	reg, ok := LookupRegion(regionID)
	if !ok {
		return nil, fmt.Errorf("没有这个大区：%s。面板只读这几个：%s", strings.TrimSpace(regionID), regionIDs())
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base()+"/"+reg.ID+"-Steam.json.gz", nil)
	if err != nil {
		return nil, fmt.Errorf("构造房间列表请求失败：%w", err)
	}
	// 不带任何凭据：这个桶是公开的，面板自己的地址、账号、令牌一个都不出现在请求里。
	req.Header.Set("User-Agent", "dstpanel/0.1")
	resp, err := f.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("取公网房间列表超时（%d 秒没下来）。%s",
				int(f.timeout().Seconds()), netHint(err))
		}
		if errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("这次取列表已经取消，不需要再报")
		}
		return nil, fmt.Errorf("连不上 Klei 的公网房间列表（lobby-v2-cdn.klei.com）。%s", netHint(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Klei 的房间列表返回了 HTTP %d，面板没法读它；稍后再试", resp.StatusCode)
	}
	raw, err := readMaybeGzip(resp.Body)
	if err != nil {
		return nil, err
	}
	doc, err := parse(raw)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	snap := &Snapshot{Region: reg.ID, FetchedAt: now, Decoded: len(raw), Total: len(doc.GET)}
	for _, rr := range doc.GET {
		if room, ok := rr.toRoom(); ok {
			snap.Rooms = append(snap.Rooms, room)
		}
	}
	if snap.Rooms == nil {
		snap.Rooms = []Room{}
	}
	return snap, nil
}

func (f *Fetcher) timeout() time.Duration {
	if f != nil && f.Timeout > 0 {
		return f.Timeout
	}
	return 15 * time.Second
}

// netHint 把"出不去"说成一句可操作的中文。带上原始原因，因为代理没配和 DNS 挂了
// 在界面上必须长得一样吗？不一样——但用户能照着原因去查，运维也能照着原因去查日志。
func netHint(err error) string {
	return "国内机房通常要走代理：在配置里填 lobby.proxy，或确认这台机器出得去。（原始原因：" + reasonOf(err) + "）"
}

// reasonOf 只取 Go 网络错误里引号后面那段原因。
// 前半段是 "Get \"https://...\""，把完整 URL 复读一遍既没用又占地方。
func reasonOf(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, `": `); i >= 0 {
		return msg[i+3:]
	}
	return msg
}

// readMaybeGzip 收正文，并在正文真是 gzip 时解压。
//
// 为什么自己判断：服务端给的 Content-Type 是 application/json、Content-Encoding 是空的，
// 但头两字节是 1f 8b。Go 的 Transport 只认 Content-Encoding，指望它解就会拿到一堆二进制
// 然后报"不是预期的 JSON"。将来 Klei 要是改成规范的 gzip 头，Transport 会先解掉，
// 那时这里认不出魔数，走原样解析这条路——两种都得通，所以这里只认数据本身。
func readMaybeGzip(body io.Reader) ([]byte, error) {
	head := make([]byte, 2)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && err != io.EOF {
		return nil, fmt.Errorf("读公网房间列表读到一半断了：%w", err)
	}
	rest := io.MultiReader(bytes.NewReader(head[:n]), body)
	var raw []byte
	if n == 2 && head[0] == 0x1f && head[1] == 0x8b {
		zr, gzErr := gzip.NewReader(rest)
		if gzErr != nil {
			return nil, fmt.Errorf("公网房间列表说是 gzip 但解不开：%w", gzErr)
		}
		defer zr.Close()
		raw, err = io.ReadAll(io.LimitReader(zr, MaxDecodedBytes+1))
	} else {
		raw, err = io.ReadAll(io.LimitReader(rest, MaxDecodedBytes+1))
	}
	if err != nil {
		return nil, fmt.Errorf("读公网房间列表读到一半断了：%w", err)
	}
	if len(raw) > MaxDecodedBytes {
		return nil, fmt.Errorf("公网房间列表解压后超过 %d MB 的上限，面板不接（2026-11-03 实测亚太那份解压后 15.3 MB）",
			MaxDecodedBytes>>20)
	}
	return raw, nil
}

type bucket struct {
	GET []rawRoom `json:"GET"`
}

func parse(raw []byte) (*bucket, error) {
	var doc bucket
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("Klei 返回的内容不是预期的 JSON（可能被网络出口改写了），这一份列表没有采信：%w", err)
	}
	return &doc, nil
}

type rawRoom struct {
	Addr   string `json:"__addr"`
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Host   string `json:"host"`
	GUID   string `json:"guid"`
	V      int    `json:"v"`
	Mods   bool   `json:"mods"`
	Pvp    bool   `json:"pvp"`
	Fo     bool   `json:"fo"`
	Intent string `json:"intent"`
	Mode   string `json:"mode"`
	Season string `json:"season"`
	Tags   string `json:"tags"`

	Connected      int  `json:"connected"`
	MaxConnections int  `json:"maxconnections"`
	Password       bool `json:"password"`
	ClanOnly       bool `json:"clanonly"`
	LanOnly        bool `json:"lanonly"`
	Dedicated      bool `json:"dedicated"`
	ClientHosted   bool `json:"clienthosted"`
	AllowNew       bool `json:"allownewplayers"`
	ServerPaused   bool `json:"serverpaused"`
	KleiOfficial   bool `json:"kleiofficial"`
}

// toRoom 把一条记录变成界面上的那一行，并回答"这条能不能进列表"。
//
// 滤掉的是"列出来也连不上"的：没有名字、没有地址、没有端口。
// 2026-11-03 在云机上对同一份亚太快照逐条数过（脚本 .tools/tmp/lobby_drop_count.sh）：
// 16468 条里只有 1 条被滤掉，原因是 port=0（房主那边没报上号）。
// 所以这一条过滤器正常情况下滤掉一两间——它的价值在将来某个字段变了形的时候，
// 让界面少一列空白而不是多一列空白。
func (r rawRoom) toRoom() (Room, bool) {
	name := strings.TrimSpace(r.Name)
	addr := strings.TrimSpace(r.Addr)
	if name == "" || addr == "" || r.Port <= 0 || r.Port > 65535 {
		return Room{}, false
	}
	free := r.MaxConnections - r.Connected
	if free < 0 {
		free = 0
	}
	room := Room{
		Name:        name,
		Addr:        addr,
		Port:        r.Port,
		Players:     r.Connected,
		Capacity:    r.MaxConnections,
		Free:        free,
		Intent:      r.Intent,
		IntentLabel: IntentLabel(r.Intent),
		Season:      r.Season,
		SeasonLabel: modbridge.SeasonLabel(r.Season),
		Password:    r.Password,
		ClanOnly:    r.ClanOnly,
		LanOnly:     r.LanOnly,
		Pvp:         r.Pvp,
		AllowNew:    r.AllowNew,
		Paused:      r.ServerPaused,
		Tags:        splitTags(r.Tags),
	}
	room.FullAccess = !room.Password && !room.ClanOnly && !room.LanOnly && room.AllowNew
	return room, true
}

func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		// 只裁空白，不清洗内容：标签是房主写的自由文本，面板改写了它就不再是那条数据。
		// 前端渲染它走 Vue 插值，永远不是 HTML，所以这里不需要防注入。
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ---- 筛选与分页 ----

// Query 是界面那一排筛选条件。空值一律表示"这一条不筛"。
type Query struct {
	Text   string
	Intent string
	// Access："" 不筛，"open" 只要有密码/战队/局域网/新人门槛都放开的，"locked" 反之。
	Access string
	// Slots："" 不筛，"free" 只还有空位的，"full" 只满员的。
	Slots string
	// Players："" 不筛，"has" 只看有人的，"none" 只看没人的。
	Players string
	// Sort："" 或 "players" 在线多的在前，"free" 空位多的在前，"capacity" 上限大的在前。
	Sort string
	Page int
	Size int
}

// PageResult 是一页内容加上"一共几页"。
//
// Rooms 一定不是 nil：这条不变式在云机上崩过一次整页（见 api/emptyIfNil 那处注释），
// 列表型字段从这里出来就得起得了 .length。
type PageResult struct {
	Rooms []Room `json:"rooms"`
	Total int    `json:"total"`
	Page  int    `json:"page"`
	Size  int    `json:"size"`
	Pages int    `json:"pages"`
	// Matched 是过滤前这一份列表里可用的房间数，界面用它说"从 N 间里筛出 M 间"。
	Matched int `json:"matched"`
}

// Apply 按条件过滤、排序并切一页出来。
// 排序在过滤之后、分页之前做，所以"下一页"真的是同一份排序结果里的下一段，
// 而不是把没排过的顺序再切一刀。
func (s *Snapshot) Apply(q Query) PageResult {
	if q.Size <= 0 {
		q.Size = 20
	}
	if q.Size > 200 {
		q.Size = 200
	}
	if q.Page < 1 {
		q.Page = 1
	}
	needle := strings.ToLower(strings.TrimSpace(q.Text))

	var kept []Room
	if s != nil {
		for _, r := range s.Rooms {
			if !keep(r, needle, q) {
				continue
			}
			kept = append(kept, r)
		}
	}
	sortRooms(kept, q.Sort)

	out := PageResult{Rooms: []Room{}, Total: len(kept), Matched: len(kept), Page: q.Page, Size: q.Size}
	if s != nil {
		out.Matched = len(s.Rooms)
	}
	if len(kept) == 0 {
		out.Pages = 1
		return out
	}
	out.Pages = (len(kept) + q.Size - 1) / q.Size
	// 页码超界时钳到最后一页，而不是回一个空页：界面上一条"下一页"点出去看到空表格，
	// 用户会以为数据被筛没了，其实是页码算错。
	if q.Page > out.Pages {
		out.Page = out.Pages
	}
	start := (out.Page - 1) * q.Size
	end := start + q.Size
	if end > len(kept) {
		end = len(kept)
	}
	out.Rooms = kept[start:end]
	return out
}

func keep(r Room, needle string, q Query) bool {
	if needle != "" && !strings.Contains(strings.ToLower(r.Name), needle) &&
		!strings.Contains(strings.ToLower(r.Addr), needle) {
		return false
	}
	if iq := strings.TrimSpace(q.Intent); iq != "" && r.Intent != iq {
		return false
	}
	switch q.Access {
	case "open":
		if !r.FullAccess {
			return false
		}
	case "locked":
		if r.FullAccess {
			return false
		}
	}
	switch q.Slots {
	case "free":
		if r.Free <= 0 {
			return false
		}
	case "full":
		if r.Free > 0 {
			return false
		}
	}
	switch q.Players {
	case "has":
		if r.Players <= 0 {
			return false
		}
	case "none":
		if r.Players > 0 {
			return false
		}
	}
	return true
}

func sortRooms(rooms []Room, by string) {
	sort.SliceStable(rooms, func(i, j int) bool {
		a, b := rooms[i], rooms[j]
		switch by {
		case "free":
			if a.Free != b.Free {
				return a.Free > b.Free
			}
		case "capacity":
			if a.Capacity != b.Capacity {
				return a.Capacity > b.Capacity
			}
		default: // players
			if a.Players != b.Players {
				return a.Players > b.Players
			}
			// 人数相同时先看有没有空位：两个都是 1 人的房间，能进的那个排在前面。
			if a.Free != b.Free {
				return a.Free > b.Free
			}
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Port < b.Port
	})
}

// Intents 给出这份列表里真实出现过的玩法，供筛选下拉用。
// 只列出现的，不列我们字典里的全部：下拉里摆一个这一份列表里一间都没有的选项，
// 点下去必然是空表格。
func (s *Snapshot) Intents() []string {
	seen := map[string]bool{}
	out := []string{}
	if s == nil {
		return out
	}
	for _, r := range s.Rooms {
		if r.Intent == "" || seen[r.Intent] {
			continue
		}
		seen[r.Intent] = true
		out = append(out, r.Intent)
	}
	sort.Slice(out, func(i, j int) bool {
		return IntentLabel(out[i]) < IntentLabel(out[j])
	})
	return out
}
