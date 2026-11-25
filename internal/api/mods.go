// 模组接口：清单登记、手工上传、以及分片级启用。
//
// 界面上必须区分三件事，混在一起用户就只会问"我明明装了怎么没生效"：
//
//	文件在不在磁盘上（下载/上传）
//	服务端启动时会不会去拉（清单登记）
//	这个分片有没有打开它（modoverrides.lua）
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"dstpanel/internal/dst"
	"dstpanel/internal/modbridge"
	"dstpanel/internal/mods"
	"dstpanel/internal/store"
)

type modView struct {
	Dir        string `json:"dir"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Workshop   bool   `json:"workshop"`
	WorkshopID string `json:"workshopId,omitempty"`
	ModinfoOK  bool   `json:"modinfoOk"`
	ModinfoErr string `json:"modinfoError,omitempty"`
	// InSetupList 表示服务端下次启动会去工坊拉它。
	InSetupList bool `json:"inSetupList"`
	// DownloadedByEngine 说这一份文件在引擎自己的下载区里（它按"每个房间的每个世界"各存一份），
	// 不是 mods/ 下面那个目录。它只改界面上这一行怎么说，不改能不能开：两种来路引擎都认。
	DownloadedByEngine bool `json:"downloadedByEngine"`
	// Dependencies 是模组自己声明的依赖，每一组只要有一个候选命中就算满足
	// （引擎 modindex.lua:484 的 FindEnabledMod 就是这个"任一"语义）。
	Dependencies [][]string `json:"dependencies,omitempty"`
	// ClientOnly / AllClientsRequire 决定这个模组在服务端上是什么身份：
	// 前者只影响客户端，后者要求每个玩家都得订阅上它，没订阅的会被断线
	// （networking.lua:351/411/442）。服务端开了 AllClientsRequire 不等于大家能进服。
	ClientOnly        bool `json:"clientOnly"`
	AllClientsRequire bool `json:"allClientsRequire"`
	// 下面这些全部来自面板的工坊缓存（mods_seen），不是磁盘上的事实：
	// 模组目录里根本没有预览图，也没有订阅数。所以"这一屏这张卡有图、那张没有"
	// 是正常状态（那张从来没被在线检索见过），界面按无封面兜底，绝不编一张。
	PreviewURL    string  `json:"previewUrl,omitempty"`
	Subscriptions int64   `json:"subscriptions,omitempty"`
	Favorited     int64   `json:"favorited,omitempty"`
	VoteScore     float64 `json:"voteScore,omitempty"`
	VotesTotal    int64   `json:"votesTotal,omitempty"`
	SizeLabel     string  `json:"sizeLabel,omitempty"`
	UpdatedLabel  string  `json:"updatedLabel,omitempty"`
	// Builtin 是面板自己带的助手模组（回传世界状态靠它）。它不是用户装的，
	// 删掉等于把面板的眼睛拆了，所以界面对它只给"看"、不给"删"。
	Builtin bool `json:"builtin"`
	// UsedBy 是这台机器上"modoverrides.lua 里把它打开了"的世界，写法同房间列表（Cluster_1 / Master）。
	// 删除按钮的确认框要能当场说出"点下去会关掉哪几间的开关"，那句只能在这里问出来；
	// 空数组是可信的"没人开着"——读不动的那一间会走 UnreadableRooms，不会静默算成没开着。
	UsedBy []string `json:"usedBy,omitempty"`
	// WorkshopVersion 是面板上次从工坊读到的那个版本（缓存里的，不是实时问的）。
	WorkshopVersion string `json:"workshopVersion,omitempty"`
	// VersionDiffers 说"工坊那边写的版本"和"这台机器上 modinfo.lua 写的"不是同一个字符串。
	// 面板不判谁新谁旧——版本号的排序不是数值比较（1.10 不比 1.9 大），
	// 判错了就是"提示有更新，其实没有"。所以这里只报"不一样"，两个值都摆出来让用户自己看。
	VersionDiffers bool `json:"versionDiffers"`
}

// pendingView 是"下载清单里有、磁盘上还没有文件"的那一条。
//
// 这里以前是一个裸的工坊 ID 字符串列表，界面就只能把 398558175 这样的数字摆给用户——
// 他问的正是这个："已登记还未下载的模组放个编号上去，难道还要用户自己去核对是哪个 mod 吗？"
// 名字和封面磁盘上都没有（文件还没下下来），只能来自面板见过的工坊条目（mods_seen 缓存）。
// 缓存里没有时 NameKnown 为假，界面要老实写"工坊标题还没读到"，
// 而不是把 workshop-数字 当成模组名字显示——那是把"我不知道"包装成"它就叫这个"。
type pendingView struct {
	WorkshopID    string  `json:"workshopId"`
	Name          string  `json:"name,omitempty"`
	NameKnown     bool    `json:"nameKnown"`
	Version       string  `json:"version,omitempty"`
	PreviewURL    string  `json:"previewUrl,omitempty"`
	SizeLabel     string  `json:"sizeLabel,omitempty"`
	UpdatedLabel  string  `json:"updatedLabel,omitempty"`
	Subscriptions int64   `json:"subscriptions,omitempty"`
	Favorited     int64   `json:"favorited,omitempty"`
	VoteScore     float64 `json:"voteScore,omitempty"`
	VotesTotal    int64   `json:"votesTotal,omitempty"`
	// ClientRequired / HasChildren 是订阅时就该看见的提醒：
	// 玩家自己没订阅的模组，服务端下得再全也不会生效；带子模组的要连依赖一起下载。
	ClientRequired bool `json:"clientRequired"`
	HasChildren    bool `json:"hasChildren"`
	// UsedBy 与 modView.UsedBy 同一口径：开着它的世界（"Cluster_1 / Master"）。
	// 待下载的那批也要带这个，否则"订阅并开启"之后界面上一片"待下载"，
	// 却看不出开关到底写没写成——而那正是用户点完第一眼要确认的事。
	UsedBy []string `json:"usedBy,omitempty"`
}

type modsView struct {
	Mods []modView `json:"mods"`
	// PendingDownload：清单登记了但磁盘还没有内容，只有重启（或工坊下载完成）才会出现。
	PendingDownload []pendingView `json:"pendingDownload"`
	// LocalOnly：磁盘上有但清单里没有，通常是手工上传的本地模组。
	LocalOnly []string `json:"localOnly"`
	// UnmetDependencies：磁盘上完全找不到候选的依赖。这一层不分分片，也不看启用状态，
	// 因为工坊内容要等启动才拉，这里只是"装了个用不了的模组"的预警。
	UnmetDependencies []mods.DependencyProblem `json:"unmetDependencies"`
	// SetupProblems 是会要命的东西：这个文件加载失败服务端直接 Shutdown()。
	SetupProblems []string `json:"setupProblems,omitempty"`
	// OverrideProblems 是 modoverrides.lua 读不动的那几间。有了它，"哪个世界开着这个模组"
	// 这一栏才敢在界面上写成事实；读不动的那几间只在页顶说一句，不在每张卡上重复一遍。
	OverrideProblems []string `json:"overrideReadProblems,omitempty"`
	SetupPath        string   `json:"setupPath"`
	Hint             string   `json:"hint,omitempty"`
}

func (s *Server) modsManager() *mods.Manager {
	return &mods.Manager{Paths: s.Paths}
}

// seenByWorkshopIDs 把这一屏的工坊 ID 一次性换成缓存条目。
// 读不到就回空 map：磁盘那一屏讲的是"这台机器上有什么"，缓存只是给卡片补张封面，
// 为它报错把整屏吞掉不划算（和 modsearch.go 里 saveSeen 的态度是同一条）。
func (s *Server) seenByWorkshopIDs(list []mods.Mod) map[string]store.SeenMod {
	ids := make([]string, 0, len(list))
	for _, x := range list {
		if x.WorkshopID != "" {
			ids = append(ids, x.WorkshopID)
		}
	}
	return s.seenByIDs(ids)
}

// seenByIDs 把一串工坊 ID 换成缓存条目。
func (s *Server) seenByIDs(ids []string) map[string]store.SeenMod {
	if s.Store == nil {
		return map[string]store.SeenMod{}
	}
	got, err := s.Store.SeenByIDs(ids)
	if err != nil {
		s.log().Warn("模组缓存批量读失败，这一屏的封面与订阅数留空", "err", err.Error())
		return map[string]store.SeenMod{}
	}
	return got
}

func (s *Server) buildModsView() (modsView, error) {
	m := s.modsManager()
	v := modsView{Mods: []modView{}, PendingDownload: []pendingView{}, LocalOnly: []string{},
		UnmetDependencies: []mods.DependencyProblem{}, SetupPath: m.Paths.ServerModSetup()}
	list, err := m.List()
	if err != nil {
		return v, err
	}
	setup, err := m.SetupEntries()
	if err != nil {
		return v, err
	}
	// 封面、订阅数这些磁盘上没有的信息只从面板的工坊缓存补：
	// 这一屏每卡的"这条信息从哪来"必须是真的读过缓存，而不是猜的。
	// 缓存读不到不影响这一屏——那一屏本来就是磁盘事实，界面只是每张卡都没图。
	seen := s.seenByWorkshopIDs(list)
	usedBy, overrideProblems := s.usedByIndex()
	for _, x := range list {
		mv := modView{
			Dir: x.Dir, Name: x.Name, Version: x.Version,
			Workshop: x.Workshop, WorkshopID: x.WorkshopID,
			ModinfoOK: x.ModinfoOK, ModinfoErr: x.Modinfo,
			InSetupList: x.InSetupList,
			// 文件在引擎下载区里而不是 mods/ 下面时，界面要在这一行说清楚它是哪一手来的。
			DownloadedByEngine: x.FromUGC,
			Dependencies:       x.Dependencies,
			ClientOnly:         x.ClientOnly, AllClientsRequire: x.AllClientsRequire,
			Builtin: strings.EqualFold(x.Dir, dst.HelperModName),
			UsedBy:  usedBy[x.Dir],
		}
		if c, ok := seen[x.WorkshopID]; ok {
			mv.PreviewURL = c.PreviewURL
			mv.Subscriptions = c.Subscriptions
			mv.Favorited = c.Favorited
			mv.VoteScore = c.VoteScore
			mv.VotesTotal = c.VotesTotal
			mv.SizeLabel = humanBytes(c.FileSize)
			mv.UpdatedLabel = dateLabel(c.ModUpdatedAt)
			// 工坊那边写的版本和磁盘 modinfo.lua 写的对不上时，这一卡要能把
			// "两个数都摆出来"这件事交给界面，而不是自己判谁新。
			// 这里必须读 c.WorkshopVersion 而不是 c.Version：后者会被扫盘刷成磁盘那份的值
			// （打开这一屏就是一次扫盘），用它的的话这两句永远相等。
			mv.WorkshopVersion = c.WorkshopVersion
			mv.VersionDiffers = c.WorkshopVersion != "" && x.Version != "" && c.WorkshopVersion != x.Version
			// 名字以磁盘上的 modinfo.lua 为准（那是这个版本自己写的），
			// 但目录里读不出名字时，缓存里的工坊标题比 workshop-123456 好读得多。
			if mv.Name == "" && c.Name != "" {
				mv.Name = c.Name
				// 名字有了着落，那句"模组自己没写出名字"就不要再挂在这张卡上吓人：
				// 屏幕上是一个正常名字，用户没有任何要处理的事。
				// 只在这一句讲的是"名字读不出来"时抹——文件真读不懂（ModinfoOK=false）
				// 的那两种必须留着，那是需要他去核对的。
				if x.ModinfoOK {
					mv.ModinfoErr = ""
				}
			}
			if mv.Version == "" {
				mv.Version = c.Version
			}
		}
		// 缓存里也没有名字（本地上传的模组本来就进不了缓存）时，屏幕上顶上来的是目录名，
		// 这句就得把它说清楚——否则用户对着 workshop-数字 分不清是面板没读出来还是模组没名字。
		// 措辞上刻意不写"这一行"：同一条原因会被配置页收成整屏一句（几行共用），
		// 带单量词的句子挪到那儿就变成"两行里说这一行"。
		if mv.ModinfoOK && mv.Name == "" && mv.ModinfoErr != "" {
			mv.ModinfoErr += "，只能按目录名显示"
		}
		v.Mods = append(v.Mods, mv)
		if !x.Workshop && !x.InSetupList {
			v.LocalOnly = append(v.LocalOnly, x.Dir)
		}
	}
	// 打开一次模组列表就等于"见过"这些名字：写进缓存后，断网时按名字搜也能搜到它们。
	s.rememberDiskMods(list)
	var pendingIDs []string
	for _, id := range setup {
		if ok, _ := m.Downloaded(id); !ok {
			pendingIDs = append(pendingIDs, id)
		}
	}
	// 待下载那几条的名字与封面同样只能来自缓存（文件还没在磁盘上）。
	// 这里单独读一次而不是复用上面的 seen：那一次的输入是磁盘上的模组，
	// 这些 ID 恰恰一个都不在磁盘上。
	pendingSeen := s.seenByIDs(pendingIDs)
	for _, id := range pendingIDs {
		p := pendingView{WorkshopID: id}
		p.UsedBy = usedBy["workshop-"+id]
		if c, ok := pendingSeen[id]; ok {
			p.Name = c.Name
			p.NameKnown = c.Name != ""
			// 待下载这一条没有磁盘文件可比，版本只可能是缓存来的；
			// 工坊那个数更新过就先用它，没有再用缓存里那份（可能来自扫盘 leftovers）。
			p.Version = c.WorkshopVersion
			if p.Version == "" {
				p.Version = c.Version
			}
			p.PreviewURL = c.PreviewURL
			p.SizeLabel = humanBytes(c.FileSize)
			p.UpdatedLabel = dateLabel(c.ModUpdatedAt)
			p.Subscriptions = c.Subscriptions
			p.Favorited = c.Favorited
			p.VoteScore = c.VoteScore
			p.VotesTotal = c.VotesTotal
			p.ClientRequired = c.ClientRequired
			p.HasChildren = c.HasChildren
		}
		v.PendingDownload = append(v.PendingDownload, p)
	}
	v.SetupProblems = m.SetupProblems()
	v.OverrideProblems = overrideProblems
	v.UnmetDependencies = mods.DependenciesAbsentFromDisk(list)
	switch {
	case len(v.SetupProblems) > 0:
		v.Hint = "下载清单里有面板看不懂的行，这种行加载失败会让服务端直接退出，建议手工核对"
	case len(v.UnmetDependencies) > 0:
		v.Hint = "有 " + strconv.Itoa(len(v.UnmetDependencies)) +
			" 个模组声明的依赖在这台机器上还找不到。服务端碰到这种模组会静默禁用它，" +
			"不会报错也不会拒绝启动"
	case len(v.PendingDownload) > 0:
		v.Hint = "有 " + strconv.Itoa(len(v.PendingDownload)) + " 个模组已订阅但还没下载，重启服务端时才会拉取"
	}
	return v, nil
}

func (s *Server) handleModsShow(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildModsView()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, v)
}

type setupAddRequest struct {
	WorkshopID string `json:"workshopId"`
}

// handleSetupAdd 登记一个工坊模组 id。这里只写清单，不下载、也不碰任何分片的覆盖文件。
func (s *Server) handleSetupAdd(w http.ResponseWriter, r *http.Request) {
	var req setupAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	m := s.modsManager()
	added, err := m.AddSetup(req.WorkshopID)
	s.audit(r, "mod.setup.add", req.WorkshopID, fmt.Sprintf("added=%v", added), err)
	if err != nil {
		// id 非法是用户输入问题，其它才是文件问题。
		if mods.ValidWorkshopID(req.WorkshopID) != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	v, err := s.buildModsView()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if !added {
		v.Hint = "这一串 ID 早就在订阅清单里了，没有重复写入"
	}
	writeOK(w, v)
}

func (s *Server) handleSetupRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m := s.modsManager()
	n, err := m.RemoveSetup(id)
	s.audit(r, "mod.setup.remove", id, fmt.Sprintf("removed=%d", n), err)
	if err != nil {
		if mods.ValidWorkshopID(id) != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	v, err := s.buildModsView()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if n == 0 {
		v.Hint = "订阅清单里本来就没有这一串 ID，两个文件都没动"
	} else {
		v.Hint = "已取消订阅；磁盘上已下载的目录还留着，要连文件一起清掉，用「已下载」那一栏里的删除"
	}
	writeOK(w, v)
}

// ---- 一次点完：订阅 + 在指定世界打开 ----

type subscribeOpenRequest struct {
	WorkshopID string   `json:"workshopId"`
	Cluster    string   `json:"cluster"`
	Shards     []string `json:"shards"`
}

type subscribeOpenView struct {
	WorkshopID string `json:"workshopId"`
	Dir        string `json:"dir"`
	// Added 说这一次有没有真的往清单里添新行；重复订阅时是 false，
	// 但开关照样要写，所以这一位只用来决定提示语怎么写。
	Added      bool     `json:"added"`
	Opened     []string `json:"opened"`
	Failed     []string `json:"failed"`
	Downloaded bool     `json:"downloaded"`
	Hint       string   `json:"hint"`
}

// handleSubscribeOpen 把"订阅这个模组"和"在用户指定的几个世界里打开它"合成一次写入。
//
// 为什么要有这一条接口：这三步以前散在三个页签上（登记 → 等重启下载 → 逐个世界开开关），
// 用户的第一句抱怨就是"这三步太繁琐"。而它们本来就是同一件事的三个副作用：
// 一个模组要在这个世界生效，缺哪一步都不会生效，分开点只会漏。
//
// 这里刻意一次写全部世界（界面默认把这个房间的地表和洞穴都勾上）：引擎只同步世界规则，
// 模组清单各世界读各自的 modoverrides.lua，只开一边会安静地开出两个规则不同的世界。
//
// 文件还没下载也要允许写开关：工坊目录名就是 workshop-<id>，重启时服务端先按清单下载、
// 再读覆盖文件，所以"开着但还没下来"是这条动线的正常中间态，不是错误状态。
func (s *Server) handleSubscribeOpen(w http.ResponseWriter, r *http.Request) {
	var req subscribeOpenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	id := strings.TrimSpace(req.WorkshopID)
	if err := mods.ValidWorkshopID(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Cluster == "" || len(req.Shards) == 0 {
		writeErr(w, http.StatusBadRequest, "要指明在哪个房间的哪几个世界打开它")
		return
	}
	names, err := s.Paths.ClusterNames()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if !contains(names, req.Cluster) {
		writeErr(w, http.StatusNotFound,
			fmt.Sprintf("没有房间 %s，现有：%s", req.Cluster, strings.Join(names, "、")))
		return
	}
	avail := s.Paths.ShardNames(req.Cluster)
	var bad []string
	for _, sh := range req.Shards {
		if !contains(avail, sh) {
			bad = append(bad, sh)
		}
	}
	if len(bad) > 0 {
		writeErr(w, http.StatusNotFound,
			fmt.Sprintf("%s 下没有世界 %s，现有：%s", req.Cluster, strings.Join(bad, "、"), strings.Join(avail, "、")))
		return
	}

	m := s.modsManager()
	added, err := m.AddSetup(id)
	s.audit(r, "mod.subscribe_open", id,
		fmt.Sprintf("added=%v rooms=%s", added, strings.Join(req.Shards, ",")), err)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	dir := "workshop-" + id
	v := subscribeOpenView{WorkshopID: id, Dir: dir, Added: added}
	on := true
	for _, sh := range req.Shards {
		if err := s.Bridge.PatchOverride(req.Cluster, sh, dir,
			modbridge.OverridePatch{Enabled: &on}); err != nil {
			v.Failed = append(v.Failed, sh+"（"+err.Error()+"）")
			continue
		}
		v.Opened = append(v.Opened, req.Cluster+" / "+sh)
	}
	v.Downloaded, _ = m.Downloaded(id)

	var parts []string
	if added {
		parts = append(parts, "已订阅")
	} else {
		parts = append(parts, "订阅清单里本来就有这一行")
	}
	switch {
	case len(v.Opened) > 0 && len(v.Failed) == 0:
		parts = append(parts, "并在 "+strings.Join(v.Opened, "、")+" 打开")
	case len(v.Opened) > 0:
		parts = append(parts, "已在 "+strings.Join(v.Opened, "、")+" 打开，但有 "+
			strconv.Itoa(len(v.Failed))+" 个世界没写成（"+strings.Join(v.Failed, "；")+"）")
	default:
		parts = append(parts, "开关一个世界都没写成（"+strings.Join(v.Failed, "；")+"）")
	}
	if v.Downloaded {
		parts = append(parts, "文件本来就在这台机器上，重启这些世界后就会加载")
	} else {
		parts = append(parts, "文件还没在这台机器上：这些世界下次启动时会先从工坊下载它，"+
			"所以下一次重启之前它是开着但没效果的，配置项也要等文件下来才改得动")
	}
	// 玩家侧订阅这件事必须在点完的这一刻说：它不是面板能补上的缺口，
	// 没在工坊订阅这个模组的玩家会被服务端直接断线（networking.lua 的判定）。
	if c, ok := s.seenByIDs([]string{id})[id]; ok && c.ClientRequired {
		parts = append(parts, "另外每个玩家还要自己在工坊订阅它，否则进不来")
	}
	v.Hint = strings.Join(parts, "。") + "。"
	writeOK(w, v)
}

// roomShard 是"某间房里的某个世界"。面板对用户说的是"房间/世界"，
// 目录名里的 cluster 与 shard 只在拼路径时用。
type roomShard struct {
	Cluster string
	Shard   string
}

func (r roomShard) String() string { return r.Cluster + " / " + r.Shard }

// usedByIndex 一次扫完这台机器上每个世界的 modoverrides.lua，反排成"模组目录 -> 开着它的世界"。
// 一次扫而不是每个目录各问一遍：这一屏十几个目录，各问一遍就是把同一批文件读十几遍。
// 第二个返回值是读不动的那几间——它们不能算"没开着"，只能如实报出来让用户自己核对。
func (s *Server) usedByIndex() (map[string][]string, []string) {
	index := map[string][]string{}
	var unreadable []string
	names, err := s.Paths.ClusterNames()
	if err != nil {
		return index, []string{"(房间名单没读到：" + err.Error() + ")"}
	}
	for _, name := range names {
		for _, shard := range s.Paths.ShardNames(name) {
			overrides, err := s.Bridge.ListOverrides(name, shard)
			if err != nil {
				unreadable = append(unreadable, roomShard{name, shard}.String())
				s.log().Warn("读模组启用列表失败，这一屏的\"哪个世界开着它\"要用户自己核对",
					"cluster", name, "shard", shard, "err", err.Error())
				continue
			}
			label := roomShard{name, shard}.String()
			for _, o := range overrides {
				if o.Enabled {
					index[o.Dir] = append(index[o.Dir], label)
				}
			}
		}
	}
	return index, unreadable
}

// roomsWithModEnabled 问"这台机器上有哪些世界正开着这个模组目录"，
// 顺带把读不动的房子里出来（那一间不能当"没开着"，但也拦不住别的房间删）。
func (s *Server) roomsWithModEnabled(dir string) (enabled []roomShard, unreadable []string) {
	names, err := s.Paths.ClusterNames()
	if err != nil {
		return nil, []string{"(整个房间的名单都没读到：" + err.Error() + ")"}
	}
	for _, name := range names {
		for _, shard := range s.Paths.ShardNames(name) {
			overrides, err := s.Bridge.ListOverrides(name, shard)
			if err != nil {
				unreadable = append(unreadable, roomShard{name, shard}.String())
				s.log().Warn("读模组启用列表失败，删除模组时这一间要用户自己核对",
					"cluster", name, "shard", shard, "err", err.Error())
				continue
			}
			for _, o := range overrides {
				if o.Dir == dir && o.Enabled {
					enabled = append(enabled, roomShard{name, shard})
				}
			}
		}
	}
	return enabled, unreadable
}

// handleModFileDelete 删掉 mods/<dir> 这个模组目录，并把和它绑定的两处配置一起收干净：
// 工坊模组的下载清单行、以及每个世界里对它的启用开关。
//
// 为什么三件事一起做，而不是让用户分三次点：
//   - 只删文件不撤清单，下次重启 steamcmd 会照着清单原样把它下回来，
//     用户看到的就是"我明明删了，怎么又回来了"；
//   - 只删文件不关开关，那一间房下次启动就带着一条指向不存在目录的启用项，
//     引擎的态度是静默禁用（不报错、不拒绝启动），于是变成"模组开了没效果"。
//
// 这一步不可逆，所以还有一条前置：正跑着的世界不许删。引擎已经把模组读进内存了，
// 中途抽走文件会让它后面按需加载时报缺文件——先停掉那间房再来删，
// 面板不替用户做"停服"这个决定（停服会踢掉正在玩的玩家）。
//
// ?keepSetup=1 是同一段代码的第二种语义："只要文件没了，订阅和开关都别动"。
// 它服务的正是"工坊更新了、这台机器上还是旧的那份"：清单留着，下次启动服务端会照清单
// 重新拉一份新的，开关也就不必重开——这条路要求"重下"，不要求"删干净"。
func (s *Server) handleModFileDelete(w http.ResponseWriter, r *http.Request) {
	dir := r.PathValue("dir")
	keepSetup := r.URL.Query().Get("keepSetup") == "1"
	m := s.modsManager()
	// 三件事必须在动手之前问：文件一旦没了，"它对应哪串 ID""哪些房间开着它"就都查不真了。
	id := mods.WorkshopIDFromDir(dir)
	enabled, unreadable := s.roomsWithModEnabled(dir)
	if len(enabled) > 0 {
		sessions, err := s.Procs.Sessions(r.Context())
		if err != nil {
			s.log().Error("读 screen 会话列表失败，删除模组时无法判断世界在不在跑", "err", err.Error())
			writeErr(w, http.StatusServiceUnavailable,
				"问不到世界进程状态，所以没法确认要删的这个模组正没正被用着：面板需要在服务端同机运行，且 PATH 里要有 screen 命令")
			return
		}
		var running []string
		for _, rs := range enabled {
			if sessions[s.Procs.SessionName(rs.Cluster, rs.Shard)] > 0 {
				running = append(running, rs.String())
			}
		}
		if len(running) > 0 {
			writeErr(w, http.StatusConflict, "这个模组正在被 "+strings.Join(running, "、")+
				" 用着（世界还在跑）。先在房间列表里停掉那几个世界再删，面板不替你做停服的决定——那会踢掉正在玩的玩家")
			return
		}
	}

	rem, err := m.RemoveFiles(dir)
	detail := fmt.Sprintf("freed=%d setup_id=%s rooms_off=%d unreadable=%d keep_setup=%v ugc_copies=%d",
		rem.Freed, id, len(enabled), len(unreadable), keepSetup, len(rem.UGCShards))
	s.audit(r, "mod.files.remove", dir, detail, err)
	if err != nil {
		// 目录名不合法、助手模组、根本不存在，都是用户点出来的输入问题，不是面板故障。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	var done []string
	// 工坊模组的文件可能有两处：mods/ 下面手工放的那一份，和引擎自己那份下载区里每个世界一份。
	// 这句话要按实际删掉了哪几处写，只删掉一处却说"都删了"就是把数字报错。
	switch {
	case rem.ModsDir && len(rem.UGCShards) > 0:
		done = append(done, fmt.Sprintf("mods/%s 和引擎下载区里的 %d 份（%s）都删掉了，腾出 %s",
			dir, len(rem.UGCShards), strings.Join(rem.UGCShards, "、"), humanBytes(rem.Freed)))
	case rem.ModsDir:
		done = append(done, fmt.Sprintf("mods/%s 已经删掉，腾出 %s", dir, humanBytes(rem.Freed)))
	default:
		done = append(done, fmt.Sprintf("引擎下载区里的 %d 份（%s）已经删掉，腾出 %s",
			len(rem.UGCShards), strings.Join(rem.UGCShards, "、"), humanBytes(rem.Freed)))
	}
	if keepSetup {
		if id != "" {
			done = append(done, "订阅清单里那一行留着，下次重启服务端会照它把这个模组重新下载回来")
		} else {
			// 本地上传的模组压根没有清单这一行，"重下"对它不成立——这句话必须说出口，
			// 否则用户按工坊那套理解去点，等到的是一个永远空着的目录。
			done = append(done, "这是本地上传的模组，不在工坊清单里，重启不会把它变回来；要再用只能重新上传")
		}
		if len(enabled) > 0 {
			names := make([]string, 0, len(enabled))
			for _, rs := range enabled {
				names = append(names, rs.String())
			}
			done = append(done, fmt.Sprintf("%s 的启用没动，文件回来它就照常生效", strings.Join(names, "、")))
		}
	} else {
		if id != "" {
			n, err := m.RemoveSetup(id)
			if err != nil {
				// 文件已经删了，这里失败必须说出口：清单里还留着那一行，下次重启会重新下载。
				s.log().Error("删掉模组文件后撤下载清单失败", "id", id, "err", err.Error())
				done = append(done, "但下载清单里那一行没撤掉（"+err.Error()+
					"）：下次重启服务端会把它重新下载回来，要彻底去掉请手工删掉那一行")
			} else if n > 0 {
				done = append(done, "下载清单里那一行也撤掉了，下次重启不会重新下载")
			}
		}
		offFailed := 0
		for _, rs := range enabled {
			off := false
			if err := s.Bridge.PatchOverride(rs.Cluster, rs.Shard, dir,
				modbridge.OverridePatch{Enabled: &off}); err != nil {
				offFailed++
				s.log().Error("删掉模组后关它的启用开关失败", "cluster", rs.Cluster, "shard", rs.Shard,
					"dir", dir, "err", err.Error())
			}
		}
		switch {
		case len(enabled) > 0 && offFailed == 0:
			names := make([]string, 0, len(enabled))
			for _, rs := range enabled {
				names = append(names, rs.String())
			}
			done = append(done, fmt.Sprintf("顺手关掉了 %d 个世界对它的启用（%s）", len(enabled), strings.Join(names, "、")))
		case offFailed > 0:
			done = append(done, fmt.Sprintf("有 %d 个世界的启用开关没关掉，要去那一间的模组列表里手工关", offFailed))
		}
		if len(unreadable) > 0 {
			done = append(done, "有 "+strings.Join(unreadable, "、")+" 的模组列表没读到，如果那里开着它，请自己核对")
		}
	}
	v, err := s.buildModsView()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	v.Hint = strings.Join(done, "；")
	writeOK(w, v)
}

const maxUploadBytes = 64 << 20

// handleModUpload 接收手工上传的模组包。
//
// 为什么这条通道要当正式功能做：服务端二进制里有一条 "Steam Workshop functionality
// will be disabled." 的分支，Steam 初始化拿不到时清单登记得再对也下不来东西。
// 上传走"先落临时文件再解压"，因为 zip.Reader 要随机读，流式读完就没法再读了。
func (s *Server) handleModUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	// multipart 必须用 ParseMultipartForm；ParseForm 不解析这个 content type，
	// 用它的话 FormFile 会拿到"没有文件字段"，用户看到的是莫名其妙的 400。
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "multipart 表单读不出来（要带 name 与 mod 两个字段）: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	name := strings.TrimSpace(r.FormValue("name"))

	f, header, err := r.FormFile("mod")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "必须带一个名为 mod 的 zip 文件字段")
		return
	}
	defer f.Close()
	if name == "" {
		name = nameFromUpload(header.Filename)
	}
	if err := mods.ValidModDirName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error()+"；也可以在表单里用 name 指定一个 ASCII 目录名")
		return
	}

	tmp, err := os.CreateTemp("", "dstpanel-mod-*.zip")
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, fmt.Errorf("建上传临时文件: %w", err))
		return
	}
	defer os.Remove(tmp.Name())
	// 上限多留 1 字节，好把"正好卡在上限"和"超了"分开报。
	n, err := io.Copy(tmp, io.LimitReader(f, maxUploadBytes+1))
	if err != nil {
		tmp.Close()
		s.fail(w, r, http.StatusInternalServerError, fmt.Errorf("接收上传内容: %w", err))
		return
	}
	if n > maxUploadBytes {
		tmp.Close()
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("压缩包超过 %d MB 上限", maxUploadBytes>>20))
		return
	}
	if err := tmp.Close(); err != nil {
		s.fail(w, r, http.StatusInternalServerError, fmt.Errorf("写入上传临时文件: %w", err))
		return
	}

	m := s.modsManager()
	target, err := m.InstallFromZip(tmp.Name(), name)
	s.audit(r, "mod.upload", name, fmt.Sprintf("来自 %s，落地 %s", header.Filename, target), err)
	if err != nil {
		// 上传内容不合法（穿越条目、缺 modinfo、同名已存在）都是 400，不是面板故障。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.buildModsView()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	v.Hint = "文件已就位，但要在这个世界的模组列表里打开它才会生效（专用服务端启动会先关掉所有模组）"
	writeOK(w, v)
}

// nameFromUpload 从上传文件名推目录名：去掉路径与 .zip 后缀。
// 这里只做取值，合法性交给 ValidModDirName，避免悄悄改名让用户对不上号。
func nameFromUpload(filename string) string {
	base := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	return strings.TrimSuffix(strings.TrimSuffix(base, ".zip"), ".ZIP")
}

// handleModSchema 返回这个模组自己声明的配置项字典（有哪些项、每项能选什么、默认是什么）。
//
// 为什么单独一个接口而不并进分片模组列表：字典是按目录（整台机器一份）而不是按分片的，
// 而且只有用户展开某一项配置时才需要。并进去等于每次刷新都把几十个 modinfo.lua 重读一遍。
// 这个接口只读文件，不改任何东西，所以不进审计。
func (s *Server) handleModSchema(w http.ResponseWriter, r *http.Request) {
	dir := r.PathValue("dir")
	sc, err := s.modsManager().SchemaFor(dir)
	if err != nil {
		// 目录名不合法或那个目录没有 modinfo.lua：都是用户点出来的输入问题，不是面板故障。
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeOK(w, sc)
}

// ---- 分片级启用 ----

type overrideView struct {
	Dir           string         `json:"dir"`
	Present       bool           `json:"present"`
	Enabled       bool           `json:"enabled"`
	Priority      *float64       `json:"priority,omitempty"`
	Options       map[string]any `json:"options"`
	NestedOptions []string       `json:"nestedOptions,omitempty"`
	// OnDisk 区分"覆盖文件里有这条但模组没下载/没上传"，那种情况开了也不会生效。
	OnDisk bool `json:"onDisk"`
	// SeenName 是面板在工坊缓存里见过的那个标题，只在这一目录不在磁盘上时才给
	// （在磁盘上时名字以 modinfo.lua 为准，那是这个版本自己写的）。
	// 为什么要有它：覆盖文件里有条目、磁盘上没目录、订阅清单里也没这一行的那种残留，
	// 界面上就只剩 workshop-数字——可这个 ID 往往正是当初从检索页订阅出去的，缓存里有名字。
	// 摆一串编号让用户自己去对，是他在 #78 里批过的事。
	SeenName string `json:"seenName,omitempty"`
}

type shardModsView struct {
	Cluster string         `json:"cluster"`
	Shard   string         `json:"shard"`
	Mods    []overrideView `json:"mods"`
	// DependencyProblems 是这个分片上"开了的模组依赖对不上"的清单。
	// 引擎遇到这种写法是静默禁用（见 mods/deps.go 的注释），不报出来就等于
	// 让用户去猜为什么模组没效果。
	DependencyProblems []mods.DependencyProblem `json:"dependencyProblems"`
	Hint               string                   `json:"hint,omitempty"`
}

func (s *Server) handleShardModsShow(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	v, err := s.buildShardModsView(cluster, shard)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, v)
}

// handleModConsistency 跨分片比对 modoverrides.lua。
// 这件事引擎自己不做：分片之间只同步世界设置（日志里的 [SyncWorldSettings]），
// 模组清单各读各的。所以"地表开了、洞穴没开"会安静地开出两个规则不同的世界，
// 玩家穿传送门过去才发现模组加的东西不存在。
func (s *Server) handleModConsistency(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	rpt, err := s.Bridge.CompareShardOverrides(cluster, s.Paths.ShardNames(cluster))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, rpt)
}

func (s *Server) buildShardModsView(cluster, shard string) (shardModsView, error) {
	v := shardModsView{Cluster: cluster, Shard: shard, Mods: []overrideView{},
		DependencyProblems: []mods.DependencyProblem{}}
	m := s.modsManager()
	list, err := m.List()
	if err != nil {
		return v, err
	}
	// onDisk 这一遍是"整台机器"的事实。引擎下载区里的副本是一个世界一份，
	// 所以那种条目还要按"这个世界有没有"再判一次：地表重启过、洞穴还没有，
	// 洞穴那一行说"文件在"就是假话——它开了也不会生效。
	onDisk := map[string]bool{}
	ugcOnly := map[string]bool{}
	for _, x := range list {
		onDisk[x.Dir] = true
		ugcOnly[x.Dir] = x.FromUGC
	}
	overrides, err := s.Bridge.ListOverrides(cluster, shard)
	if err != nil {
		return v, err
	}
	var enabled []string
	var missingIDs []string
	for _, o := range overrides {
		present := onDisk[o.Dir]
		if present && ugcOnly[o.Dir] {
			present = m.DownloadedIn(cluster, shard, mods.WorkshopIDFromDir(o.Dir))
		}
		v.Mods = append(v.Mods, overrideView{
			Dir: o.Dir, Present: o.Present, Enabled: o.Enabled, Priority: o.Priority,
			Options: o.Options, NestedOptions: o.NestedOptions, OnDisk: present,
		})
		if !present {
			if id := mods.WorkshopIDFromDir(o.Dir); id != "" {
				missingIDs = append(missingIDs, id)
			}
		}
		if o.Enabled {
			enabled = append(enabled, o.Dir)
		}
	}
	// 磁盘上没有目录的那几条，名字只能来自缓存：这一屏画的是"这个世界开着哪些模组"，
	// 顶着一排 workshop-数字就等于没回答。
	if len(missingIDs) > 0 {
		seen := s.seenByIDs(missingIDs)
		for i := range v.Mods {
			id := mods.WorkshopIDFromDir(v.Mods[i].Dir)
			if c, ok := seen[id]; ok && !v.Mods[i].OnDisk {
				v.Mods[i].SeenName = c.Name
			}
		}
	}
	v.DependencyProblems = mods.CheckDependencies(list, enabled)
	if v.DependencyProblems == nil {
		v.DependencyProblems = []mods.DependencyProblem{}
	}
	if len(v.DependencyProblems) > 0 {
		v.Hint = "有 " + strconv.Itoa(len(v.DependencyProblems)) +
			" 条依赖对不上。引擎碰到这种情况会静默禁用那个模组，服务端不会报错，" +
			"只是模组开了没效果"
	}
	return v, nil
}

type overridePatchRequest struct {
	Enabled  *bool          `json:"enabled"`
	Priority *float64       `json:"priority"`
	Options  map[string]any `json:"options"`
}

// handleShardModPatch 改这个分片里某个模组的启用状态与配置项。
// 这个模组必须先在这台机器上找得到目录（mods/ 下面，或引擎自己那份下载区里）：
// 这既挡住了凭空造键，也让"开了个不存在的模组"
// 在写入前就被拒绝，而不是等到启动时靠引擎的日志才发现。
func (s *Server) handleShardModPatch(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	dir := r.PathValue("dir")
	var req overridePatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.Enabled == nil && req.Priority == nil && req.Options == nil {
		writeErr(w, http.StatusBadRequest, "至少要给 enabled、priority 或 options 中的一个")
		return
	}
	found := false
	list, err := s.modsManager().List()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, x := range list {
		if x.Dir == dir {
			found = true
		}
	}
	// 目录还没在磁盘上，有两种正当情况，都得放行：
	//   - 订阅清单里有这一行（"订阅并开启"就是先写开关、重启时才下载文件）；
	//   - 这个世界的覆盖文件里已经有它的条目（文件被删掉了，得能把那条目关掉）。
	// 除此之外凭空写一个指向不存在目录的键才真要拦——引擎碰到那种是静默禁用，
	// 不报错也不拒绝启动，用户只会得到"模组开了没效果"。
	if !found {
		if id := mods.WorkshopIDFromDir(dir); id != "" {
			if setup, err := s.modsManager().SetupEntries(); err == nil && contains(setup, id) {
				found = true
			}
		}
		if !found {
			if overrides, err := s.Bridge.ListOverrides(cluster, shard); err == nil {
				for _, o := range overrides {
					if o.Dir == dir && o.Present {
						found = true
					}
				}
			}
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, fmt.Sprintf(
			"这台机器上没有 %s 这个模组（mods/ 和引擎的下载区里都找不到它），没法给 %s 开它；"+
				"工坊模组要先订阅（订阅后要等重启下载），本地模组要先上传", dir, shard))
		return
	}
	patch := bridgePatch(req)
	if err := patch.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Bridge.PatchOverride(cluster, shard, dir, patch); err != nil {
		s.audit(r, "mod.override", cluster+"/"+shard+"/"+dir, describePatch(req), err)
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "mod.override", cluster+"/"+shard+"/"+dir, describePatch(req), nil)
	v, err := s.buildShardModsView(cluster, shard)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 写成功那句话不能把依赖问题挤掉：用户刚点的就是"启用"，
	// 这时候最需要知道的是"开了也会被引擎静默禁用"。
	writeHint := "覆盖已写入，重启这个世界后生效"
	if v.Hint != "" {
		v.Hint = writeHint + "。" + v.Hint
	} else {
		v.Hint = writeHint
	}
	writeOK(w, v)
}

func bridgePatch(req overridePatchRequest) modbridge.OverridePatch {
	return modbridge.OverridePatch{Enabled: req.Enabled, Priority: req.Priority, Options: req.Options}
}

func describePatch(req overridePatchRequest) string {
	var parts []string
	if req.Enabled != nil {
		parts = append(parts, fmt.Sprintf("enabled=%v", *req.Enabled))
	}
	if req.Priority != nil {
		parts = append(parts, fmt.Sprintf("priority=%g", *req.Priority))
	}
	if req.Options != nil {
		keys := make([]string, 0, len(req.Options))
		for k := range req.Options {
			keys = append(keys, k)
		}
		parts = append(parts, "配置项 "+strconv.Itoa(len(keys))+" 个（"+strings.Join(keys, ",")+"）")
	}
	return strings.Join(parts, "，")
}
