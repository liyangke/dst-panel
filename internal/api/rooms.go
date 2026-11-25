// 房间总览：这台机器上所有饥荒服务器的信息，一次问完，一张表摆平。
//
// 为什么要有这个接口，而不是让界面自己去拼 /clusters + /status + /settings：
// 那要前端发 1 + 2N 个请求（N 是分片数），每页都要自己处理"某个分片读不到"，
// 结果是同一套降级判断在 JS 里写一遍、在 Go 里再写一遍。这里把一次刷新需要的
// 事实收拢成一条响应，界面只做排序、筛选和分页。
//
// 一行 = 一个分片，不是一个集群。理由是分片才是那个"有进程、有端口、有天数"的东西：
// 地表和洞穴可以一个在跑一个没在跑，合成一行就没法说清哪个停了。
//
// 一条约定贯穿到底：读不到的东西留空并把原因写进 note，绝不编一个看起来正常的值。
// 模组件数读不出时显示"—"是对的，显示 0 会让用户以为模组被清空了然后去做多余的操作。
package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"dstpanel/internal/dst"
	"dstpanel/internal/modbridge"
)

// 这几张表放在面板这一侧而不是前端：接口的约定是"码值必须配一句中文"，
// 前端翻译表的另一份副本迟早和这里对不上（世界设定页就踩过这个坑）。
var gameModeLabels = map[string]string{
	"survival":   "生存",
	"endless":    "无尽",
	"wilderness": "荒野",
}

var intentionLabels = map[string]string{
	"cooperative": "合作",
	"competitive": "竞争",
	"social":      "社交",
	"madness":     "疯狂",
}

// accessShort 是那一列只放得下三四个字时用的短名。
// 完整那句仍然由 accessLabel 给，界面收进按需展开的提示里——四个分片各写一遍
// "已注册到公网服务器列表"会把表格压成一面文字墙。
func accessShort(code string) string {
	switch code {
	case "online":
		return "公网"
	case "lan":
		return "局域网"
	case "offline":
		return "离线"
	case "no-token":
		return "缺令牌"
	default:
		return "未知"
	}
}

// accessLabel 把"玩家怎么进得来"这一件事说成一句话。
//
// 三个开关（lan_only_cluster、offline_cluster、有没有登录令牌）在 cluster.ini 里是分开的，
// 但它们的组合才是用户关心的结论。新手最容易踩的是"开了机、日志没报错、公网就是搜不到"，
// 原因往往是 offline_cluster 或者令牌是空的——所以这句话必须出现在总览里。
func accessLabel(set dst.GameSettings, hasToken bool) (string, string) {
	switch {
	case set.OfflineCluster:
		return "offline", "离线房间：只有本机局域网能进"
	case set.LanOnly:
		return "lan", "仅局域网：不向 Klei 服务器注册"
	case !hasToken:
		return "no-token", "缺登录令牌：公网玩家搜不到这个房间"
	default:
		return "online", "已注册到公网服务器列表"
	}
}

// modeLabel 给游戏模式与偏好配中文。取不到值时留空而不是"未知"：
// 这一句会和季节、上限拼在同一行里，"未知 / 未知 · 上限 6"读起来像这个房间坏了，
// 而真实情况只是 cluster.ini 没写这一项（引擎按默认跑）。前端按空值跳过。
func modeLabel(table map[string]string, code string) string {
	if code == "" {
		return ""
	}
	return modbridge.Label(table, code)
}

type roomView struct {
	Cluster string `json:"cluster"`
	// OwnerLabel 是这行房归属的说法（"未指派" / "归 小王 管"）。
	// 房间总览是逐分片一行，同一个房间的两行写同一个归属，
	// 所以界面只在每间房的第一行显示它（见 rooms.js 的分组）。
	OwnerLabel string `json:"ownerLabel"`
	OwnedByMe  bool   `json:"ownedByMe"`
	// OwnerAccountID 和 /clusters 上那一份同义：0 就是未指派。
	// 界面靠它挑颜色，不去比 OwnerLabel 的字符串——把"未指派"写进前端，
	// 后端改一个字说法就会让那一列的配色失灵。
	OwnerAccountID int64  `json:"ownerAccountId"`
	Shard          string `json:"shard"`
	// RoomName 是 cluster.ini 里的 cluster_name，玩家在饥荒服务器列表上看到的那个名字；
	// Cluster 是磁盘目录名，面板所有接口路径都用它。两者可以不一样，界面上必须都看得见，
	// 否则用户照着屏幕上的房间名去改配置会找不到目录。
	RoomName  string `json:"roomName"`
	ShardName string `json:"shardName"`
	// Terrain 是引擎地形码值（forest/cave），配 TerrainLabel 中文。
	Terrain      string `json:"terrain"`
	TerrainLabel string `json:"terrainLabel"`
	Description  string `json:"description,omitempty"`

	Running      bool   `json:"running"`
	RunningLabel string `json:"runningLabel"`
	Pid          int    `json:"pid,omitempty"`
	IsMaster     bool   `json:"isMaster"`
	Port         int    `json:"port,omitempty"`
	// MasterAddr 只有洞穴分片有：它写在洞穴的 server.ini 里，地表重启换了端口时，
	// 这里能当场看出"洞穴连的还是旧端口"，那是穿传送门失败的常见原因。
	MasterAddr string `json:"masterAddr,omitempty"`

	GameMode       string `json:"gameMode,omitempty"`
	GameModeLabel  string `json:"gameModeLabel,omitempty"`
	Intention      string `json:"intention,omitempty"`
	IntentionLabel string `json:"intentionLabel,omitempty"`
	MaxPlayers     int    `json:"maxPlayers"`
	Pvp            bool   `json:"pvp"`
	Access         string `json:"access"`
	AccessLabel    string `json:"accessLabel"`
	AccessShort    string `json:"accessShort"`
	TickRate       int    `json:"tickRate"`
	HasToken       bool   `json:"hasToken"`

	// PresetLabel 是世界生成预设的中文说法。文件不存在时留空——"还没写过世界设定"
	// 和"用了某个预设"是两件事，前者由引擎自己决定。
	PresetLabel string `json:"presetLabel,omitempty"`

	// 模组件数用指针：读不到 modoverrides.lua 和"确实一个都没开"必须分得开。
	ModsEnabled *int `json:"modsEnabled,omitempty"`
	ModsTotal   *int `json:"modsTotal,omitempty"`

	// 下面四个数来自助手 MOD 的状态快照，所以进程刚起来还没进世界时是空的。
	Online      int     `json:"online"`
	Day         float64 `json:"day"`
	Season      string  `json:"season,omitempty"`
	SeasonLabel string  `json:"seasonLabel,omitempty"`
	// 这一季第几天、还剩几天。指针 + omitempty：季节不轮换（无尽模式、固定季节）
	// 时这两个键不出现，界面就不说这句话，而不是摆一个"第 1 天"。
	DayInSeason      *float64 `json:"dayInSeason,omitempty"`
	DaysLeftInSeason *float64 `json:"daysLeftInSeason,omitempty"`
	Phase            string   `json:"phase,omitempty"`
	PhaseLabel       string   `json:"phaseLabel,omitempty"`
	// WorldReady 表示这一行的天数/季节/时段是真数据。没有它时前端只能猜，
	// 而"把没有数据摆成第 0 天、秋天、白天"正是最难查的假象。
	WorldReady bool `json:"worldReady"`
	// WorldNote 是 WorldReady 为假时那一列该说的话，来自助手状态那份分类结论。
	// 放在后端是因为前端只会照着 running 猜，于是暂停中的世界被说成"还没进世界"，
	// 和同一行助手那栏的"引擎把世界冻住了"当场打架。
	WorldNote string `json:"worldNote,omitempty"`

	HelperOnline bool   `json:"helperOnline"`
	HelperNote   string `json:"helperNote,omitempty"`
	// HelperNoteUrgent 见 shardView 上同名字段的说明：暂停这类"设计如此"的读不到不该染成红。
	HelperNoteUrgent bool   `json:"helperNoteUrgent"`
	LastBackupAt     string `json:"lastBackupAt,omitempty"`
	// Note 是这一行的降级原因（配置文件读不出来之类），面向用户的一句话。
	Note string `json:"note,omitempty"`
	// Alert 是这一行最该先处理的一件事，界面用它排序和上色。为空表示没发现问题。
	Alert string `json:"alert,omitempty"`
}

type roomsReply struct {
	GeneratedAt string `json:"generatedAt"`
	// Clusters/Rooms 是"几个房间、几个进程"，两者不等（一个房间通常有地表+洞穴两个分片）。
	Clusters int `json:"clusters"`
	Rooms    int `json:"rooms"`
	Running  int `json:"running"`
	Online   int `json:"online"`
	// Attention 汇总"有话要说的行数"。给新手的第一屏只放这一个数，
	// 而不是让他自己逐行判断哪一行不对劲。
	Attention int        `json:"attention"`
	Items     []roomView `json:"items"`
}

// handleRooms 汇总所有房间。
//
// 失败处理分三档，按"这一档说错了会误导多少人"排：
//   - 集群名单读不到 → 500，页面没有任何可信内容；
//   - screen 会话问不到 → 503，和 /clusters 一样：进程状态说错比不说更糟，
//     用户会以为服务器没跑着然后重复点启动；
//   - 单个房间/分片的文件读不到 → 那一行留原因，其余照常返回。
func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	names, err := s.Paths.ClusterNames()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 和 /clusters 同一套筛法：成员的房间名单在响应里就已经裁好，
	// 不靠前端"拿到全部再藏起来"。
	names = s.visibleClusters(r, names)
	owners, err := s.ownerViews(names)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	me := sessionOf(r)
	sessions, err := s.Procs.Sessions(r.Context())
	if err != nil {
		s.log().Error("读 screen 会话列表失败", "err", err.Error())
		writeErr(w, http.StatusServiceUnavailable,
			"问不到世界进程状态：面板需要在服务端同机运行，且 PATH 里要有 screen 命令")
		return
	}
	// 快照目录一次列完。逐行问的话每个房间都要重新 ReadDir 一遍，
	// 而且"最近备份"本来就是房间级的，同房间的两个分片报同一个时间。
	lastBackup := s.latestBackups()

	out := make([]roomView, 0, len(names)*2)
	for _, name := range names {
		cluster, err := dst.OpenCluster(s.Paths, name)
		var set dst.GameSettings
		token := ""
		// cluster.ini 读不出来时不能整页失败：其它房间是好的，而这个房间的分片
		// 还在跑、端口还在监听，那些事实照样有用。
		configNote := ""
		if err == nil {
			set = cluster.Settings()
			token, _ = cluster.Token()
		} else {
			configNote = "这个房间的 cluster.ini 读不出来，下面只显示进程与游戏状态"
			s.log().Error("房间总览读 cluster.ini 失败", "cluster", name, "err", err.Error())
		}
		shards := s.Paths.ShardNames(name)
		for _, shard := range shards {
			row := roomView{
				Cluster: name, Shard: shard,
				RoomName: name, ShardName: shard,
				Terrain: dst.ShardLocation(shard),
			}
			ov, _ := ownerFromViews(owners, name)
			row.OwnerLabel = ov.OwnerLabel
			row.OwnedByMe = ov.AccountID != 0 && ov.AccountID == me.ID
			row.OwnerAccountID = ov.AccountID
			row.TerrainLabel = "地表"
			if row.Terrain == dst.LocationCave {
				row.TerrainLabel = "洞穴"
			}
			if err == nil {
				row.RoomName = firstNonEmpty(set.ClusterName, name)
				row.Description = set.Description
				row.GameMode = set.GameMode
				row.GameModeLabel = modeLabel(gameModeLabels, set.GameMode)
				row.Intention = set.Intention
				row.IntentionLabel = modeLabel(intentionLabels, set.Intention)
				row.MaxPlayers = set.MaxPlayers
				row.Pvp = set.PVP
				row.TickRate = set.TickRate
				row.HasToken = token != ""
				row.Access, row.AccessLabel = accessLabel(set, row.HasToken)
				ss := cluster.Shard(shard)
				row.ShardName = firstNonEmpty(ss.Name, shard)
				row.IsMaster = ss.IsMaster
				row.Port = ss.ServerPort
				// 主机地址空着就不拼这一串：":10999" 看着像个地址却谁都连不上，
				// 而洞穴真正缺的就是那个地址——宁可留空，让"没写在配置里"说实话。
				if !ss.IsMaster && ss.MasterPort > 0 && strings.TrimSpace(ss.MasterIP) != "" {
					row.MasterAddr = ss.MasterIP + ":" + strconv.Itoa(ss.MasterPort)
				}
			} else {
				row.Note = configNote
				// 配置读不出来时"缺令牌"那句话说不得——我们并不知道它配没配。
				row.Access, row.AccessLabel = "unknown", "房间配置读不出来"
			}
			row.AccessShort = accessShort(row.Access)

			// 进程：pid 来自这一次共享的 `screen -ls`，逐分片各问一次会创建 N 个进程。
			if s.Procs != nil {
				row.Pid = sessions[s.Procs.SessionName(name, shard)]
			}
			row.Running = row.Pid > 0
			row.RunningLabel = "未运行"
			if row.Running {
				row.RunningLabel = "运行中"
			}

			// 助手状态与世界的天数/季节来自同一份快照，只读一次文件。
			st, stErr := s.Bridge.Status(name, shard)
			verdict := s.helperReadiness(name, shard, row.Running, st, stErr)
			row.HelperOnline, row.HelperNote, row.HelperNoteUrgent = verdict.Online, verdict.Note, verdict.Urgent
			if stErr == nil {
				row.WorldReady = true
				row.Online = st.PlayerCount
				row.Day = st.World.DayNumber()
				row.Season = st.World.Season
				row.SeasonLabel = st.World.SeasonLabel()
				if d, ok := st.World.DayInSeason(); ok {
					row.DayInSeason = &d
				}
				if left, ok := st.World.DaysLeftInSeason(); ok {
					row.DaysLeftInSeason = &left
				}
				row.Phase = st.World.Phase
				row.PhaseLabel = st.World.PhaseLabel()
				// 助手回了话但快照过期时，上面的天数是历史值，不能当"现在"报出去。
				if st.Stale {
					row.WorldReady = false
				}
			}
			row.WorldNote = verdict.worldNote(row.WorldReady)

			row.PresetLabel = s.presetLabel(name, shard)
			if en, total, ok := s.modCounts(name, shard); ok {
				row.ModsEnabled, row.ModsTotal = &en, &total
			}
			row.LastBackupAt = lastBackup[name]

			// 进程活着但助手不回话是唯一一个"看起来一切正常其实没有"的状态，
			// 值得单独占一个醒目位；其余不对劲由 helperNote/accessLabel 自己说。
			//
			// 但"没人时引擎把世界冻住"要排除在外：那一刻助手停摆是集群设置里
			// 「无人时暂停」的正常结果，把它算进"要先处理"，这一页就永远有两行
			// 没人管却报着警的房间，用户很快就不信这个筛选了。
			if row.Running && !row.HelperOnline && verdict.Urgent {
				row.Alert = "进程在跑，但助手 MOD 没有回话"
			} else if err == nil && row.Access == "no-token" {
				row.Alert = "没有登录令牌，公网玩家搜不到"
			}
			out = append(out, row)
		}
	}

	reply := roomsReply{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Clusters:    len(names),
		Rooms:       len(out),
	}
	for i := range out {
		if out[i].Running {
			reply.Running++
		}
		// 只累加"助手此刻真的在报数"的分片。界面上一行过期快照显示"未知"，
		// 合计里却带着它的人数，两个数互相打脸，而用户信的是顶部那个大数字。
		if out[i].WorldReady {
			reply.Online += out[i].Online
		}
		if out[i].Alert != "" {
			reply.Attention++
		}
	}
	reply.Items = out
	writeOK(w, reply)
}

// helperVerdict 是"这个世界的助手现在算不算在回话"的完整答案。
//
// 做成结构体而不是并排三个返回值：同一套判断还要给出「世界」那一栏的措辞
// （见 roomView.WorldNote），两处必须讲同一个故事——上一轮就是分开写才漏掉了暂停这一种。
type helperVerdict struct {
	Online bool
	Note   string
	// Urgent 表示这句是真故障：该用报警色，也该计入「要先处理」。
	Urgent bool
	// Reason 是分类结论，界面按它选措辞。取值：
	// stopped（进程没起来）/ boot-failure（引擎承认没进世界）/ paused（没人时引擎冻住）
	// / stale（有过快照、停更了）/ never（从没写过快照）/ error（读不出来）/ 空（在回话）。
	Reason string
}

// helperReadiness 回答"这个分片的助手 MOD 现在算不算在回话"，并把离线的原因补全。
//
// 进程活着、助手不回话，有两种最费解的情况：分片根本没进世界（世界生成失败或登录被拒时
// 引擎不会退出，进程和 screen 会话都还在，而原因早就写在 server_log.txt 里了），
// 以及没人玩时引擎暂停世界、助手随之停摆。前者只报"还没写入过状态"等于把结论藏起来，
// 后者不写清楚用户就会去重启一个好好的服务器。
//
// 单独抽出来是因为房间总览和集群列表两页都要这套判断：各写一遍迟早一套漏掉一种原因。
//
// Urgent 这一列是因为界面以前把整列说明一律染红，于是
// "没人时引擎自己冻住世界"和"令牌无效、服务端根本不启动"长得一模一样——
// 前者只需要等有人进来，后者要人去改文件，摆成同一种红等于把两种完全不同的下一步
// 压成一句"出错了"。
func (s *Server) helperReadiness(cluster, shard string, running bool, st *modbridge.Status, err error) helperVerdict {
	online, note := helperState(st, err)
	if online {
		return helperVerdict{Online: true}
	}
	if !running {
		// 进程都没起来，"没数据"天经地义，不该染成红。
		return helperVerdict{Note: note, Reason: "stopped"}
	}
	// 没有进程驱动时（面板只读文件运行）连"是不是暂停的"都问不到，只能按可疑处理。
	if s.Procs == nil {
		return helperVerdict{Note: note, Urgent: true, Reason: "stale"}
	}
	if engineNote, ok := s.Procs.BootFailureNote(cluster, shard); ok {
		return helperVerdict{Note: note + "；" + engineNote, Urgent: true, Reason: "boot-failure"}
	}
	if pauseNote, ok := s.Procs.SimPausedNote(cluster, shard); ok {
		// 暂停是集群设置里那个「无人时暂停」在起作用，不是坏了。
		return helperVerdict{Note: note + "；" + pauseNote, Reason: "paused"}
	}
	switch {
	case err == nil:
		return helperVerdict{Note: note, Urgent: true, Reason: "stale"}
	case errors.Is(err, os.ErrNotExist):
		return helperVerdict{Note: note, Urgent: true, Reason: "never"}
	default:
		return helperVerdict{Note: note, Urgent: true, Reason: "error"}
	}
}

// worldNote 是「世界」那一列在拿不到实时天数时的说法。
//
// 这一列以前照着 running 自己猜（在跑就说"还没进世界"），于是暂停中的世界被写成
// "进程在跑，但还没进世界"，而同一条记录助手那栏明明写着"引擎把世界冻住了"——
// 同一屏两个互相矛盾的结论，正是用户在云上看到并报给我的那一种。
// 现在它和助手那栏用同一份分类结论，两处不可能再各说一套。
func (v helperVerdict) worldNote(worldReady bool) string {
	if worldReady {
		return ""
	}
	switch v.Reason {
	case "paused":
		return "暂停中，读不到实时状态"
	case "boot-failure", "never":
		return "进程在跑，但还没进世界"
	case "stale", "error":
		return "读不到实时状态"
	default:
		// 进程没起来（或正在回话却没有世界数据，那种情况下面板自己有问题）：
		// 这一列不该抢着下结论。
		return ""
	}
}

// presetLabel 读这个分片的世界生成预设。读不到或文件不存在都回空字符串——
// 世界设定是可选文件（引擎按默认生成），空不是错误。
func (s *Server) presetLabel(cluster, shard string) string {
	spec, exists, err := s.Paths.ReadWorldGen(cluster, shard)
	if err != nil || !exists || spec.Preset == "" {
		if err != nil {
			s.log().Error("房间总览读世界规则失败", "cluster", cluster, "shard", shard, "err", err.Error())
		}
		return ""
	}
	return dst.PresetLabel(spec.Preset)
}

// modCounts 数这个分片的模组条目数与启用数。只读 modoverrides.lua，
// 不去扫 MODS_ROOT——扫一遍磁盘是为"这些模组在不在"服务的，而这一列只回答"开了几个"。
// 第三个返回值是"读到了没有"，读不到时调用方留空而不是报 0。
func (s *Server) modCounts(cluster, shard string) (int, int, bool) {
	list, err := s.Bridge.ListOverrides(cluster, shard)
	if err != nil {
		s.log().Error("房间总览读模组覆盖失败", "cluster", cluster, "shard", shard, "err", err.Error())
		return 0, 0, false
	}
	enabled := 0
	for _, o := range list {
		if o.Enabled {
			enabled++
		}
	}
	return enabled, len(list), true
}

// latestBackups 给出每个房间最近一次快照的时间。
//
// 拿不到（没接上快照模块、目录读不了）时返回空表：这一列整列留空比让接口失败更合适，
// "最近备份时间"是锦上添花，不是这一页的主语。
func (s *Server) latestBackups() map[string]string {
	out := map[string]string{}
	if s.Backups == nil {
		return out
	}
	list, err := s.Backups.List()
	if err != nil {
		s.log().Error("房间总览读备份列表失败", "err", err.Error())
		return out
	}
	// List 已按编号倒序，第一个碰到的就是这个房间最新的那份。
	for _, sn := range list {
		if _, seen := out[sn.Cluster]; !seen {
			out[sn.Cluster] = sn.CreatedAt
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- 加入房间的那一句命令 ----

type connectView struct {
	Cluster  string `json:"cluster"`
	RoomName string `json:"roomName"`
	// Shard 是这句话指向的那个世界（玩家永远从地表进，洞穴不对外收人）。
	Shard   string `json:"shard"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	// Command 是整句能直接粘进游戏控制台的东西，界面只负责复制它。
	// 三种拼不出来的情况（没端口 / 没地址 / 这个服压根没开）下它是空的，
	// 空的时候 Note 必须给话，否则界面只剩一个"复制"按钮按下去什么也不发生。
	Command string `json:"command"`
	// Source 说清这个地址是哪来的：面板在反向代理或内网穿透后面时它会不对，
	// 而用户唯一能核对的办法就是知道面板当时听到了什么。
	Source string `json:"source"`
	Note   string `json:"note,omitempty"`
}

// handleConnect 拼出玩家在游戏控制台里要敲的那一句。
//
// 饥荒联机版没有"输入 IP 加入"的输入框：房间在好友列表里搜不到时（没令牌、设了隐藏、
// 或者引擎还没把它注册上），唯一的入口是控制台 c_connect("地址", 游戏端口)。
// 游戏端口写在各自的 server.ini 里、每个房间都不一样，让人去翻文件太费事。
//
// 地址取这次请求的 Host：面板和服务端在同一台机器上，能把面板打开的那个地址
// 就是玩家该用的地址（端口是两回事，从配置文件现读）。反代/穿透时不一定对，
// 所以把来源一并返回，界面说清楚，别让人拿着一条连不上的命令猜原因。
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	out := connectView{
		Cluster: cluster, RoomName: cluster,
		Source: "地址取自你打开面板时用的那个（这台机器上面板和饥荒服务端是同一个地址）",
	}
	// r.Host 带着面板自己的端口（127.0.0.1:8963），要的是主机那一段。
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	out.Address = host

	clu, err := dst.OpenCluster(s.Paths, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	out.RoomName = firstNonEmpty(clu.Settings().ClusterName, cluster)

	// 玩家连的永远是地表那个进程：洞穴的分片只认地表，从外面连它的端口连不上人。
	// 没有 IsMaster 标记的旧房间就退回第一个分片名——那种情况下多半只有一个世界。
	master := ""
	for _, sh := range s.Paths.ShardNames(cluster) {
		if master == "" {
			master = sh
		}
		if clu.Shard(sh).IsMaster {
			master = sh
			break
		}
	}
	if master == "" {
		writeErr(w, http.StatusNotFound, "这个房间里还没有世界，先建一个再来拿加入命令")
		return
	}
	out.Shard = master
	out.Port = clu.Shard(master).ServerPort

	if out.Port > 0 && out.Address != "" {
		out.Command = `c_connect("` + out.Address + `", ` + strconv.Itoa(out.Port) + `)`
	}
	switch {
	case out.Port <= 0:
		out.Note = "这个世界的 server.ini 里没写游戏端口，命令拼不出来。去「开服与世界」把端口填上再来复制。"
	case out.Address == "":
		out.Note = "这次请求没带上访问地址，命令拼不出来。用这台机器的局域网或公网地址打开面板后再复制一次。"
	case !s.shardIsRunning(r.Context(), cluster, master):
		out.Note = "这个世界的进程现在没在跑，命令是拼好了，但得先开服才连得上。"
	case isLoopbackHost(out.Address):
		out.Note = "这是本机回环地址，只有这台机器上的游戏客户端连得上。要给别人玩，请用这台机器的局域网或公网地址打开面板后再复制。"
	}
	writeOK(w, out)
}

// isLoopbackHost 认 127.0.0.1 和 localhost 这类只有本机能通地址。
// 面板部署在云服务器上时用户十有八九是从别的机器打开的，走不到这个分支；
// 一旦真走到这里，说明他在这台机器的浏览器里复制，那条命令发给别人是连不上的。
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// shardIsRunning 只用来决定要不要多提醒那一句"得先开服"。
// 问不到进程状态时返回 true：这条信息只是锦上添花，把"screen 问不到"
// 说成"这个服没开"会误导人去做重启这种多余操作。
func (s *Server) shardIsRunning(ctx context.Context, cluster, shard string) bool {
	running, err := s.runningShards(ctx, cluster)
	if err != nil {
		return true
	}
	return contains(running, shard)
}
