package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dstpanel/internal/mods"
	"dstpanel/internal/steam"
	"dstpanel/internal/store"
)

// 模组检索。这一页要回答的是用户最想问、却最难自己答的一句：
// "我想加个让背包大一点的模组，它的工坊 ID 是多少？"
//
// 三条数据来源按信息量排：Steam 在线接口 > 本地缓存（mods_seen，见过即入库）> 磁盘上的文件。
// 在线接口要 key、还要网络出得去 api.steampowered.com，国内机房经常两个都不满足，
// 所以降级是常态不是异常。每条响应都必须带一句中文说明这批结果是从哪来的——
// 否则用户会把"搜不到"读成"这个模组不存在"，而真相往往只是面板没配 key。
//
// 反方向也一样重要：检索结果只改缓存，绝不动 dedicated_server_mods_setup.lua。
// 看到名字就顺手登记是越权，登记必须是用户点出来的那一下。

type modSearchItem struct {
	WorkshopID string   `json:"workshopId"`
	Name       string   `json:"name"`
	Desc       string   `json:"desc,omitempty"`
	Version    string   `json:"version,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	// Subscriptions 是工坊订阅数，也是默认排序依据（前排就是大家都在用的）。
	// 只有在线和本地缓存里存过的才有；为 0 不代表没人订阅，只代表不知道。
	Subscriptions int64  `json:"subscriptions,omitempty"`
	ModUpdatedAt  int64  `json:"modUpdatedAt,omitempty"`
	UpdatedLabel  string `json:"updatedLabel,omitempty"`
	FileSize      int64  `json:"fileSize,omitempty"`
	SizeLabel     string `json:"sizeLabel,omitempty"`
	PreviewURL    string `json:"previewUrl,omitempty"`
	// Favorited 是收藏数：工坊卡片上那颗星星对应的就是它，和订阅数是两个量级不同的数
	// （实测 Epic Healthbar 订阅 372 万、收藏 11.8 万）。
	Favorited int64 `json:"favorited,omitempty"`
	// VoteScore 是好评率（0~1），VotesTotal 是投过票的人数。
	// 界面只在 VotesTotal>0 时画星星：没有投票的 0 分要画成"没评分"而不是一星。
	VoteScore  float64 `json:"voteScore,omitempty"`
	VotesTotal int64   `json:"votesTotal,omitempty"`

	// ClientRequired 来自 all_clients_require_mod 标签：为真时光在服务端装没用，
	// 玩家自己也得到工坊订阅。这是"装了没生效"的头号原因，必须在列表里就能看见。
	ClientRequired bool `json:"clientRequired"`
	ServerOnly     bool `json:"serverOnly"`
	// HasChildren 表示它带子模组（依赖）。登记时要连依赖一起写进清单，
	// 否则服务端启动会因为缺文件而报错——所以这里只是个提醒位。
	HasChildren bool `json:"hasChildren"`

	// 文件在不在磁盘上、清单里有没有，是两件不同的事，混起来用户就只能问
	// "我明明装了怎么没生效"。
	Downloaded  bool   `json:"downloaded"`
	Registered  bool   `json:"registered"`
	StatusLabel string `json:"statusLabel"`

	Source      string `json:"source"`
	SourceLabel string `json:"sourceLabel"`
}

type modSearchView struct {
	Query string `json:"query"`
	Tag   string `json:"tag"`
	// TagLabel 是那一类的中文名。界面用它回显，不让前端再去字典里翻
	// （前端翻得到才出现"码值和中文名对不上"这种事）。
	TagLabel string `json:"tagLabel,omitempty"`
	// Categories 是分类标签那一排：14 枚一直在（名目来自字典，界面上不必再放一份），
	// 但只有 CategoryCountsKnown 为真时那后面的条数才是这一屏的口径，见下。
	Categories []modCategory `json:"categories"`
	// CategoryCountsKnown 说"这一排分类后面的条数可不可信"。
	// 能数出来的只有两种屏：在线带分类（数的是这次取回的那一批，由 liveBrowseTag 填）、
	// 纯缓存（数的是缓存里对得上的那些，由 cacheFacets 填）。
	// 另两种（在线不带分类、按 ID 精确查）没有同一个口径的样本，硬画一排 0 会让人
	// 以为"宠物类一条都没有"，而真相只是"这一屏没数过"。
	CategoryCountsKnown bool `json:"categoryCountsKnown"`
	// Batch 是"在线 + 分类"那一屏一次从工坊取回多少条用来筛；别的屏为 0。
	Batch int             `json:"batch"`
	Page  int             `json:"page"`
	Size  int             `json:"size"`
	Items []modSearchItem `json:"items"`
	// Total 是这一屏"能翻的总条数"，口径随来源变：在线不带分类时是工坊那边的总数，
	// 在线带分类时是这次取回那一批里命中的条数，纯缓存时是缓存里对得上的条数。
	// 三者都必须在 Note 里说清是哪一个，否则"共 N 条"就是个假数。
	Total   int  `json:"total"`
	HasMore bool `json:"hasMore"`
	// Live 表示这次真的打到了 Steam 接口；LiveProblem 是没打成功的中文结论。
	Live         bool   `json:"live"`
	LiveProblem  string `json:"liveProblem,omitempty"`
	OnlineEnable bool   `json:"onlineEnabled"`
	CacheCount   int    `json:"cacheCount"`
	// Note 是显示在结果上方的一句中文，负责把"这批结果可信到什么程度"说清。
	Note string `json:"note"`

	// tagDropped 记一次"用户选了分类，但这一屏走的是按 ID 精确查，分类没参与"。
	// 它不外发（小写字段不进 JSON），只用来把那句话补进 Note：
	// 光把界面上的选回落到"不限分类"而不说原因，用户会以为自己手滑点错了。
	tagDropped bool
}

// modCategory 是分类那一排上的一枚。Count 为 0 且整排都是 0 时界面不画数字
// （在线不带分类那一屏就是这种：那一屏的总数来自 Steam 的分页，
// 而分类计数只能在一次取回的整批里数，两个口径混在一排上就是假数字）。
type modCategory struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// modCategories 是这一排分类的字典，顺序就是界面上的顺序。
//
// 键是工坊作者自己写的标签（实测 1200 条样本：tweak 369、item 337、character 337、
// interface 317、utility 303、other 294、art 272、environment 158、creature 128、
// pet 79、worldgen 63、scenario 49、tutorial 40、language 38），
// 值是给中文用户看的名目。样本里还有作者自填的杂词（dargonfly、menu、UI 之类），
// 它们进不了字典——字典外没有中文说法，画出来就是一串码值。
// 大小写不用管：工坊那边同一个分类有人写 other 有人写 Other（294/52 次），
// SQLite 的 LIKE 和这里的比较都按不区分大小写处理。
var modCategories = []modCategory{
	{Key: "character", Label: "人物"},
	{Key: "creature", Label: "生物"},
	{Key: "pet", Label: "宠物"},
	{Key: "item", Label: "物品"},
	{Key: "environment", Label: "环境"},
	{Key: "worldgen", Label: "世界生成"},
	{Key: "scenario", Label: "剧本"},
	{Key: "art", Label: "美术"},
	{Key: "interface", Label: "界面"},
	{Key: "utility", Label: "实用工具"},
	{Key: "tweak", Label: "游戏微调"},
	{Key: "language", Label: "语言翻译"},
	{Key: "tutorial", Label: "教程"},
	{Key: "other", Label: "其它"},
}

func modCategoryLabel(key string) string {
	for _, c := range modCategories {
		if c.Key == key {
			return c.Label
		}
	}
	return ""
}

func modCategoryKeys() []string {
	out := make([]string, 0, len(modCategories))
	for _, c := range modCategories {
		out = append(out, c.Key)
	}
	return out
}

// modTagChoice 认一枚分类参数。
// 报错里列的是中文名而不是键：用户是在"人物/环境"这一排里挑的，
// 他没见过 character 这个拼法，把码值写进报错等于让他去猜一个他没看到的词。
func modTagChoice(v string) (string, string, error) {
	t := strings.ToLower(strings.TrimSpace(v))
	if t == "" {
		return "", "", nil
	}
	for _, c := range modCategories {
		if c.Key == t {
			return c.Key, c.Label, nil
		}
	}
	labels := make([]string, 0, len(modCategories))
	for _, c := range modCategories {
		labels = append(labels, c.Label)
	}
	return "", "", fmt.Errorf("tag 只能是这些分类之一：%s（留空表示不限分类），收到的是 %q",
		strings.Join(labels, "、"), v)
}

// hasModTag 在一条模组的标签里找某个分类。
// 比较时两边都小写：作者写的 Other 和字典里的 other 是同一类（实测两种写法都有）。
func hasModTag(tags []string, key string) bool {
	for _, t := range tags {
		if strings.ToLower(strings.TrimSpace(t)) == key {
			return true
		}
	}
	return false
}

// modBatchLimit 是一次从工坊取回多少条用来筛分类。
// 100 是实测上限：numperpage=100 真回 100 条，再多就不给了。
const modBatchLimit = 100

// diskIndex 是"这台机器上现在有什么"的一次快照。
type diskIndex struct {
	downloaded map[string]bool
	registered map[string]bool
	// readErr 非空表示 mods/ 压根没读到（dstInstall 配错）。
	// 这时候不 500：用户正需要靠检索找 ID 去修配置，但必须在 Note 里说明
	// 上面每行的"安装状态"都不可信，否则"没显示已装"会被读成"没装"。
	readErr string
}

func (s *Server) handleModSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	tag, tagLabel, err := modTagChoice(r.URL.Query().Get("tag"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	page, size, err := searchPaging(r.URL.Query())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v := modSearchView{Query: q, Tag: tag, TagLabel: tagLabel, Page: page, Size: size,
		Items: []modSearchItem{}, Categories: facetView(nil),
		OnlineEnable: s.Steam.Enabled()}
	if s.Store != nil {
		v.CacheCount, _ = s.Store.SeenCount()
	}

	m := s.modsManager()
	disk := diskIndex{downloaded: map[string]bool{}, registered: map[string]bool{}}
	// 清单要单独读：已登记但还没下载的 ID 根本不在 mods/ 里，
	// 只按目录列就永远标不出"已登记，等重启时下载"这一格。
	setup, setupErr := m.SetupEntries()
	list, listErr := m.List()
	switch {
	case listErr != nil:
		disk.readErr = publicError(listErr)
	case setupErr != nil:
		disk.readErr = publicError(setupErr)
	default:
		for _, x := range list {
			if x.WorkshopID != "" {
				disk.downloaded[x.WorkshopID] = true
			}
		}
		// 扫过就算见过：把磁盘上的 modinfo 名字写进缓存，
		// 用户下次按名字搜已装的模组时不该因为"没联网"而搜不到手边的东西。
		s.rememberDiskMods(list)
		for _, id := range setup {
			disk.registered[id] = true
		}
	}

	// 一串纯数字时走 GetDetails：那是精确查。QueryFiles 会把数字当文字去匹配标题，
	// 用户抄来的 ID 很可能一条都对不上，然后以为模组不存在。
	isID := q != "" && mods.ValidWorkshopID(q) == nil
	// 精确查这一条不带分类：按 ID 只会回一条，回的那条是不是"人物"类都由不得筛选，
	// 这时候把 tag 继续留在界面上选着，就是在承诺一件这一屏没有做过的事。
	// 所以这里把它抹掉并把 v.Tag 回成空，前端跟着接口回的那一份画（和房间列表那枚下拉同一条规矩）。
	if isID && tag != "" {
		v.Tag, v.TagLabel, v.tagDropped = "", "", true
	}
	switch {
	case isID && v.OnlineEnable:
		s.liveDetails(r, q, &disk, &v)
	case v.OnlineEnable && v.Tag != "":
		s.liveBrowseTag(r, q, v.Tag, page, size, &disk, &v)
	case v.OnlineEnable:
		// 关键词为空也打接口：这就是 #76 那条需求——"我不知道要什么模组，
		// 只想一页一页翻，一个个看，直到找到我想要的"。以前这一路只在给了词时才走，
		// 于是"翻一翻"只能翻面板见过的缓存，而缓存里有的恰恰是他已经知道的东西。
		// 实测（.tools/workshop_browse_probe.py）不带 search_text 时工坊回的是全量榜，
		// page 真能翻页，所以这一条不是把空查询硬塞进搜索，而是本来就存在的那张榜单。
		s.liveSearch(r, q, page, size, &disk, &v)
	}
	// 在线结果优先占位，剩下的名额给缓存补足：这样接口只回 3 条时界面不会大片空白，
	// 而缓存里那 3 条也从"重复"变成了"确认"。
	//
	// 两种屏不补：
	//   - "在线 + 分类"：这一屏的每一句数字（共几条、有没有下一页）都只描述这次从工坊取回的那一批，
	//     掺进缓存条目之后"共 N 条"就不再说它字面的意思了。
	//   - "在线 + 空关键词"（#76 那一整张榜）：这一屏讲的是"工坊自己的榜，共 27313 条、现在第几页"，
	//     补进面板见过的东西，上面那句就成了假话。
	// 分类没选、又给了关键词时才混合两路来源，那时候的总数口径本来就是工坊那边给的。
	if room := size - len(v.Items); room > 0 && !(v.Live && (v.Tag != "" || v.Query == "")) {
		s.fillFromCache(q, v.Tag, room, size, page, &disk, &v)
	}
	// 分类那一排上的数字只能来自和这一屏同一个的那一批：在线带分类时 liveBrowseTag
	// 已经从它取回的那一批里现算好了；纯缓存这一路由 cacheFacets 数缓存。
	// 剩下的两种屏（在线不带分类、按 ID 精确查）不数，界面就只联名目不联数。
	if !v.Live {
		s.cacheFacets(q, &v)
	}
	if s.Store != nil {
		// 刚才那两批"见过即入库"可能已经让缓存变长了，这里的数字要和结果一致。
		v.CacheCount, _ = s.Store.SeenCount()
	}
	v.explain(disk)
	writeOK(w, v)
}

func (s *Server) liveSearch(r *http.Request, q string, page, size int, disk *diskIndex, v *modSearchView) {
	out, err := s.Steam.Search(r.Context(), q, page, size)
	if err != nil {
		// 不返回错误：接口挂了 + 缓存里的老结果对用户是有用的，一个 500 不是。
		// 界面只拿得到那句中文，技术原因留在日志里（Go 的字段名对用户没有意义，
		// 但少了它我们自己也分不清是出口改写还是 Steam 改了契约）。
		s.log().Warn("工坊在线检索失败，退回本地缓存", "q", q, "page", page, "err", err)
		v.LiveProblem = publicError(err)
		return
	}
	v.Live = true
	v.Total = out.Total
	v.Items = append(v.Items, toSearchItems(out.Items, disk)...)
	v.HasMore = page*size < out.Total
	s.rememberWorkshopMods(out.Items)
}

// liveBrowseTag 是"在线 + 分类"这一屏：一次取回一批（最多 modBatchLimit 条），
// 在面板这一头筛完、再切出这一页。
//
// 为什么不问 Steam 要"这一类的全部"：实测 QueryFiles 的 matched_tag / exclude_tag /
// required_tags 三个参数全被忽略——同 query_type 下带与不带都回同一串 ID、同一个 total
// （证据见 .tools/tmp/tag_filter_probe4.py）。那就只有两条路：把这一屏 12 条在前端藏掉几条
// （"共 N 条"立刻变成假的），或者多取一些在这头筛。这里选后者，并且把"筛的是哪一批"
// （Batch，这一趟真取回几条）原样回给界面写进那句话，含糊成"工坊里共 N 条"就是骗人。
func (s *Server) liveBrowseTag(r *http.Request, q, tag string, page, size int, disk *diskIndex, v *modSearchView) {
	items, err := s.Steam.Browse(r.Context(), q, modBatchLimit)
	if err != nil {
		s.log().Warn("工坊分类浏览失败，退回本地缓存", "q", q, "tag", tag, "err", err)
		v.LiveProblem = publicError(err)
		return
	}
	v.Live = true
	v.Batch = len(items)
	counts := map[string]int{}
	var hit []steam.Item
	for _, it := range items {
		// 一条模组可以同时属于几类（实测很常见：既 item 又 utility），
		// 所以每一类都各自加一次，而不是只算它的第一枚标签。
		for _, c := range modCategories {
			if hasModTag(it.Tags, c.Key) {
				counts[c.Key]++
			}
		}
		if hasModTag(it.Tags, tag) {
			hit = append(hit, it)
		}
	}
	v.Categories = facetView(counts)
	v.CategoryCountsKnown = true
	// 保持 Steam 回的顺序（它按自己的相关性/热度排），我们没有比它更有底气的序。
	v.Total = len(hit)
	off := (page - 1) * size
	if off < 0 || off > len(hit) {
		// 越界的一页回空的而不是报错：分页是从这里那个 total 推出来的，
		// 用户手动改 page 时踩到边界，界面顶多显示"这一页没有"。
		off = len(hit)
	}
	end := off + size
	if end > len(hit) {
		end = len(hit)
	}
	v.Items = append(v.Items, toSearchItems(hit[off:end], disk)...)
	v.HasMore = off+size < len(hit)
	// 整批都存进缓存，不只命中的那些：分类只筛这一批，剩下的条目下次按名字搜照样用得上。
	s.rememberWorkshopMods(items)
}

// cacheFacets 给"没配 key 或在线没成"那一屏数出各分类有几条。
// 口径是本地缓存，和这一屏的列表、总数同一个来源；在线那一屏的分类数由
// liveBrowseTag 从它取回的那一批里现算，两边不会互相冒充。
func (s *Server) cacheFacets(q string, v *modSearchView) {
	if s.Store == nil {
		return
	}
	counts, err := s.Store.SeenFacets(q, modCategoryKeys())
	if err != nil {
		// 数不出来就不画数字，而不是画一排 0：一排"0"会让人以为这一类真的没有模组，
		// 而真相是这一步失败了。列表本身照旧出得来。
		s.log().Warn("本地缓存分类计数失败", "q", q, "err", err.Error())
		return
	}
	v.Categories = facetView(counts)
	v.CategoryCountsKnown = true
}

// facetView 按字典顺序摊成一排，没命中的分类也带上（Count=0）。
// 少一枚会让那一排在"有没有结果"之间跳来跳去，用户每次都要重新找那一枚在哪。
func facetView(counts map[string]int) []modCategory {
	out := make([]modCategory, 0, len(modCategories))
	for _, c := range modCategories {
		out = append(out, modCategory{Key: c.Key, Label: c.Label, Count: counts[c.Key]})
	}
	return out
}

func (s *Server) liveDetails(r *http.Request, id string, disk *diskIndex, v *modSearchView) {
	items, err := s.Steam.Details(r.Context(), id)
	if err != nil {
		v.LiveProblem = publicError(err)
		return
	}
	v.Live = true
	if len(items) == 0 {
		// 接口活着但这串 ID 查不到东西：通常是抄错一位、已下架或被封。
		// 这必须和"没配 key"分开说，因为前者的下一步是去核 ID，后者是去配 key。
		v.LiveProblem = "Steam 上说没有这串 ID 对应的模组（可能已下架、被删除，或者 ID 抄错了一位）"
		return
	}
	v.Items = append(v.Items, toSearchItems(items, disk)...)
	s.rememberWorkshopMods(items)
}

// fillFromCache 用本地缓存补足名额；一条在线结果都没有时，它就是这一页的全部来源，
// 所以页码、总数、有没有下一页都得由它自己说清楚。
func (s *Server) fillFromCache(q, tag string, room, size, page int, disk *diskIndex, v *modSearchView) {
	if s.Store == nil || room <= 0 {
		return
	}
	pure := len(v.Items) == 0
	// 两种模式都按页取偏移：补足那批若永远从缓存第 1 条开始，翻到第二页时
	// 那几条补位的老面孔会原样再来一遍，用户看到的是"翻页翻不动"而不是"翻到头了"。
	offset := (page - 1) * size
	if offset < 0 {
		offset = 0
	}
	// 多要一些再去重：在线那批和缓存那批经常是同一批 ID。
	found, err := s.Store.SearchSeenPage(q, tag, room+size, offset)
	if err != nil {
		v.LiveProblem = joinNote(v.LiveProblem, "读本地缓存失败："+publicError(err))
		return
	}
	known := map[string]bool{}
	for _, it := range v.Items {
		known[it.WorkshopID] = true
	}
	var fresh []store.SeenMod
	for _, x := range found {
		if !known[x.WorkshopID] {
			known[x.WorkshopID] = true
			fresh = append(fresh, x)
		}
	}
	if len(fresh) > room {
		fresh = fresh[:room]
	}
	v.Items = append(v.Items, cacheToSearchItems(fresh, disk)...)
	if !pure {
		return
	}
	// 纯缓存这一页没有 Steam 给的 total，只能自己数同一个搜索词加同一个分类下有几条。
	// 不数就出不来页码：界面会永远停在"第 1 页"，下一页也永远点不动。
	total, err := s.Store.CountSeen(q, tag)
	if err != nil {
		return
	}
	v.Total = total
	v.HasMore = offset+size < total
}

// explain 拼出那句必须有的中文说明。顺序按"用户下一步要做什么"排：
// 先说结果从哪来，再说这东西现在装没装上，最后说界面上一眼看不出的坑。
func (v *modSearchView) explain(disk diskIndex) {
	var parts []string
	switch {
	case !v.OnlineEnable:
		parts = append(parts, fmt.Sprintf(
			"没配 steamApi.webApiKey，这里只翻了面板见过的 %d 条（这台机器上装着的都在里面，按订阅数排，同数按名字）；订阅工坊 ID 不需要 key，把 ID 填进去就能装",
			v.CacheCount))
	case v.Live && v.Query == "" && v.Tag == "":
		// 这一屏讲的是"工坊自己的榜"，不是"面板见过的东西"：那句必须把两者分开，
		// 否则用户还是会以为列表只有他装过的那几个（#76 的原始抱怨）。
		parts = append(parts, fmt.Sprintf(
			"这一屏是 Steam 工坊自己的榜，共 %d 条模组、现在第 %d 页，不用先想好关键词也能一页一页翻下去；"+
				"面板同时把见过的条目存进本地缓存（累计 %d 条），下次断网也搜得到",
			v.Total, v.Page, v.CacheCount))
	case v.Live:
		parts = append(parts, "结果来自 Steam 在线检索，面板已把见过的条目存进本地缓存（累计 "+
			strconv.Itoa(v.CacheCount)+" 条），下次断网也搜得到")
	case v.LiveProblem != "":
		// 上游那句中文结论自己带句号（`internal/steam` 的 Conclusion），而整段 Note 是用
		// "。" 拼起来的，中间再点一次就成了"能直连。。下面是"——公网那台机器断 Steam 时
		// 屏顶就是这么写的，云机截图上看得见。
		parts = append(parts, "在线检索没成功："+trimPeriod(v.LiveProblem)+
			"。下面是本地缓存里对得上的老结果，名字可能不是最新的")
	default:
		parts = append(parts, "没给搜索词，这里按热度列出面板见过的模组；换关键词就能搜工坊上的新模组")
	}
	switch {
	case v.tagDropped:
		parts = append(parts, "按 ID 精确查时分类筛选不参与，所以上面那一排回到了「不限分类」")
	case v.Live && v.Tag != "":
		parts = append(parts, fmt.Sprintf(
			"「%s」这一类是面板在这一次取回的 %d 条里筛出来的（Steam 的在线检索接口不认标签参数，实测），"+
				"所以 %d 条只说这一批里有几条，不是工坊上这一类的总数；换个关键词就是换一批",
			v.TagLabel, v.Batch, v.Total))
		if len(v.Items) == 0 {
			parts = append(parts, "这一批里一条都没有：换个关键词再筛，或者回「不限分类」")
		}
	case v.Tag != "":
		parts = append(parts, fmt.Sprintf("「%s」是在本地缓存里按标签筛的，对得上一共 %d 条",
			v.TagLabel, v.Total))
	}
	if len(v.Items) == 0 && v.CacheCount > 0 && !v.Live {
		parts = append(parts, "缓存里也没有"+seenScope(v.Tag)+"，可以直接填工坊 ID 订阅")
	}
	if len(v.Items) == 0 && v.CacheCount == 0 && !v.Live {
		// 全新面板第一次打开这一页："为什么是空的"界面那栏空状态已经按 onlineEnable
		// 分两种说清了（配了 key 但没给关键词时不能说"还没配 key"，那种自相矛盾的话
		// 云机上出现过）。这里只补它没说的半步：不靠检索也能装上模组。
		parts = append(parts, "一条都没有也能装上模组：把工坊页地址里那串 ID 填到上面订阅，或者去第 2 步「装到这台机器」上传一个")
	}
	if v.HasMore {
		switch {
		case v.Live && v.Query == "" && v.Tag == "":
			// 空关键词这一屏不再补"还有更多"：上面第一句已经把"这是工坊的榜、共几条、
			// 现在第几页"说完了，下面那条分页条也一直摆在那儿，同一屏写两遍就是废话。
		case v.Live && v.Tag != "":
			// 这里绝不写"工坊那边还有更多（约 N 条）"：带分类那一屏的 N 是面板自己
			// 在这一批里筛出来的命中数，上一句刚刚把它说清楚是"这一批里有几条"，
			// 这里再把它当成工坊的总数说一遍就自相矛盾了，而且那是骗人。
			parts = append(parts, "下一页还是这一次取回的那一批，面板不会为翻页再去工坊取新货")
		case v.Live:
			parts = append(parts, fmt.Sprintf("工坊那边还有更多（约 %d 条），翻下一页", v.Total))
		default:
			parts = append(parts, fmt.Sprintf("本地缓存对得上一共 %d 条，还有下一页", v.Total))
		}
	}
	for _, it := range v.Items {
		if it.ClientRequired {
			parts = append(parts, "带「客户端也要订阅」标记的模组，玩家自己得去工坊订阅，光服务端装上没效果")
			break
		}
	}
	for _, it := range v.Items {
		if it.HasChildren {
			parts = append(parts, "带依赖的模组要把子模组一起订阅，否则服务端启动会缺文件报错")
			break
		}
	}
	if disk.readErr != "" && len(v.Items) > 0 {
		// 只在真的列了东西时才补这一句：一条都没有的时候"上面每行的装没装不可信"
		// 说的是空气，而那句读不到的原因界面已经用红条整句摆在上面了（冷启动实测里
		// 同一段路径在一屏里出现了三次）。
		parts = append(parts, "另外模组目录现在读不到（"+disk.readErr+
			"），上面每行的「装没装」都不可信")
	}
	v.Note = strings.Join(parts, "。")
}

// rememberDiskMods / rememberWorkshopMods 只写缓存，写失败不影响这次请求：
// 缓存是加速器不是权威来源，因为它而报错把检索结果一起吞掉是不划算的。
func (s *Server) rememberDiskMods(list []mods.Mod) {
	if s.Store == nil {
		return
	}
	var toSave []store.SeenMod
	for _, x := range list {
		if x.WorkshopID == "" || !x.ModinfoOK {
			continue
		}
		toSave = append(toSave, store.SeenMod{
			WorkshopID: x.WorkshopID, Name: x.Name, Version: x.Version,
			Description: x.Description, Tags: x.Tags,
			// 这两个标记 modinfo.lua 里就有，比工坊那边按标签猜更权威。
			// serverOnly 故意不填：工坊那条是从 server_only_mod 标签来的，modinfo.lua
			// 里没有对应的已核实字段，硬套 client_only_mod 会把意思说反。
			ClientRequired: x.AllClientsRequire,
			HasChildren:    len(x.Dependencies) > 0,
			Origin:         store.SeenFromDisk,
		})
	}
	s.saveSeen(toSave)
}

func (s *Server) rememberWorkshopMods(items []steam.Item) {
	if s.Store == nil {
		return
	}
	toSave := make([]store.SeenMod, 0, len(items))
	for _, it := range items {
		toSave = append(toSave, store.SeenMod{
			WorkshopID:     it.ID,
			Name:           it.Name,
			Description:    it.Desc,
			Version:        it.Version,
			Tags:           it.Tags,
			Subscriptions:  it.Subscriptions,
			Favorited:      it.Favorited,
			VoteScore:      it.VoteScore,
			VotesTotal:     it.VotesTotal,
			ModUpdatedAt:   it.ModUpdatedAt,
			FileSize:       it.FileSize,
			PreviewURL:     it.PreviewURL,
			HasChildren:    it.HasChildren,
			ClientRequired: it.ClientRequired,
			ServerOnly:     it.ServerOnly,
			Origin:         store.SeenFromWorkshop,
		})
	}
	s.saveSeen(toSave)
}

func (s *Server) saveSeen(toSave []store.SeenMod) {
	if len(toSave) == 0 {
		return
	}
	if err := s.Store.RecordSeen(toSave...); err != nil {
		s.log().Warn("工坊模组缓存没写进去", "err", err.Error())
	}
}

func toSearchItems(items []steam.Item, disk *diskIndex) []modSearchItem {
	out := make([]modSearchItem, 0, len(items))
	for _, it := range items {
		out = append(out, modSearchItem{
			WorkshopID: it.ID, Name: it.Name, Desc: clipDesc(it.Desc), Version: it.Version,
			Tags: it.Tags, Subscriptions: it.Subscriptions,
			Favorited: it.Favorited, VoteScore: it.VoteScore, VotesTotal: it.VotesTotal,
			ModUpdatedAt: it.ModUpdatedAt, UpdatedLabel: dateLabel(it.ModUpdatedAt),
			FileSize: it.FileSize, SizeLabel: humanBytes(it.FileSize), PreviewURL: it.PreviewURL,
			ClientRequired: it.ClientRequired, ServerOnly: it.ServerOnly, HasChildren: it.HasChildren,
			Downloaded: disk.downloaded[it.ID], Registered: disk.registered[it.ID],
			StatusLabel: modStatusLabel(disk.downloaded[it.ID], disk.registered[it.ID]),
			Source:      "live", SourceLabel: "Steam 在线检索",
		})
	}
	return out
}

func cacheToSearchItems(list []store.SeenMod, disk *diskIndex) []modSearchItem {
	out := make([]modSearchItem, 0, len(list))
	for _, m := range list {
		// source 只有 live/cache 两种：这一行确实是从缓存读出来的。
		// 见过的来源用 label 说清就够——把 origin 当 source 会出现
		// "来源=这台机器上的文件"而 downloaded=false 的自相矛盾行（目录被手工删掉就是这样）。
		label := "本地缓存（以前在线检索见过）"
		if m.Origin == store.SeenFromDisk {
			label = "本地缓存（扫这台机器的模组目录时见过）"
		}
		out = append(out, modSearchItem{
			WorkshopID: m.WorkshopID, Name: m.Name, Desc: clipDesc(m.Description), Version: m.Version,
			Tags: m.Tags, Subscriptions: m.Subscriptions,
			Favorited: m.Favorited, VoteScore: m.VoteScore, VotesTotal: m.VotesTotal,
			ModUpdatedAt: m.ModUpdatedAt, UpdatedLabel: dateLabel(m.ModUpdatedAt),
			FileSize: m.FileSize, SizeLabel: humanBytes(m.FileSize), PreviewURL: m.PreviewURL,
			ClientRequired: m.ClientRequired, ServerOnly: m.ServerOnly, HasChildren: m.HasChildren,
			Downloaded: disk.downloaded[m.WorkshopID], Registered: disk.registered[m.WorkshopID],
			StatusLabel: modStatusLabel(disk.downloaded[m.WorkshopID], disk.registered[m.WorkshopID]),
			Source:      "cache", SourceLabel: label,
		})
	}
	return out
}

// modStatusLabel 把两个布尔说成一句能照着做的中文。
func modStatusLabel(downloaded, registered bool) string {
	switch {
	case downloaded && registered:
		return "已下载，也在订阅清单里"
	case downloaded:
		return "文件在磁盘上，但订阅清单里没有，重启不会被更新"
	case registered:
		return "已订阅，等重启时下载"
	default:
		return "没装"
	}
}

// searchPaging 只在校验参数，不 clamp：page=abc 和 page=0 是用户传错了，
// 悄悄改成 1 会让"翻下一页"看起来永远停在第一页。
func searchPaging(q url.Values) (page, size int, err error) {
	page, size = 1, 10
	if raw := strings.TrimSpace(q.Get("page")); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 1 {
			return 0, 0, fmt.Errorf("page 要是从 1 开始的整数，收到的是 %q", raw)
		}
		page = n
	}
	if raw := strings.TrimSpace(q.Get("size")); raw != "" {
		n, cerr := strconv.Atoi(raw)
		if cerr != nil || n < 1 || n > 20 {
			return 0, 0, fmt.Errorf("size 要是 1~20 的整数，收到的是 %q（一屏超过 20 条也看不出东西）", raw)
		}
		size = n
	}
	return page, size, nil
}

// seenScope 是那句"没有对得上的东西"里的限定语。
// 选了分类之后还说"名字对得上的模组"，用户会以为缓存里真没有这个名字——
// 实际上名字对得上的有，只是不带那一类。
func seenScope(tag string) string {
	if tag == "" {
		return "名字对得上的模组"
	}
	return "名字和分类都对得上的模组"
}

func clipDesc(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 160
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func dateLabel(unixSec int64) string {
	if unixSec <= 0 {
		return ""
	}
	return time.Unix(unixSec, 0).UTC().Format("2006-01-02")
}

// humanBytes 的单位用中文习惯的 MB/KB，不让前端自己除 1024。
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/float64(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/float64(1<<10), 'f', 1, 64) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return trimPeriod(a) + "；" + trimPeriod(b)
}

// trimPeriod 收掉一句中文尾巴上的标点。这些句子来自两处不同的作者：面板自己写的说明
// 不带标点（靠 strings.Join(parts, "。") 统一加），而 internal/steam 的 Conclusion 自带
// 句号，两边一拼就会出现"。。"和"。；"。
func trimPeriod(s string) string {
	return strings.TrimRight(s, "。；")
}
