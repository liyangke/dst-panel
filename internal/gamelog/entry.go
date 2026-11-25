// Package gamelog 读引擎写在分片目录里的 server_log.txt 与 server_chat_log.txt，
// 把它们变成界面能直接用的结构化条目。
//
// 为什么不把文件原文直接推给前端，有三条各自独立的原因：
//
//  1. 噪声比例。实测（2026-10-31 云机，760944）Master 那份 1146 行里，
//     [Http]/[Error] 的 Klei 列表广播重试占 260 行、vault_key_trial 的探测占 242 行，
//     而"谁进来了、存过档、世界生成炸了"加起来不到二十行。管理员打开日志页要看的是后者。
//  2. 机密。引擎自己只打 `Token retrieved from: <路径>`（只给路径），
//     但同一份文件里出现过完整令牌的可能性不能赌——面板是往外发 JSON 的，
//     脱敏必须在出进程之前做完，而不是指望前端记得遮。
//  3. 时钟。引擎的 [00:06:01] 是"距本轮启动多久"，不是墙上时间；直接显示没法归档，
//     也没法和平台操作记录对上。好在启动流水里有 `Current time: Sat Oct 31 02:17:35 2026`
//     这一行（服务器本地时区），可以把它换算回来。
//
// 分类规则的取值来源都是真机日志或二进制 strings，不是社区帖：
// 匹配不上的一律降级成"普通 + 无事件"，原文照给。认不出来不等于没有价值。
package gamelog

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Kind 是读哪一份日志。取值和查询参数一致，都由引擎的两个文件名决定。
type Kind string

const (
	KindServer Kind = "server"
	KindChat   Kind = "chat"
)

func (k Kind) Label() string {
	if k == KindChat {
		return "聊天与公告"
	}
	return "服务端日志"
}

// ParseKind 解析查询参数。这里不给默认值：默认读哪一份是接口层的事，
// 两处各定一套默认值的话，前端和后端会对"不传 kind 是什么行为"产生不同预期。
func ParseKind(raw string) (Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "server":
		return KindServer, true
	case "chat":
		return KindChat, true
	}
	return "", false
}

// 级别只有三档，够用：引擎自己就只分普通/警告/错误（[WARNING]、[Error]）。
// 再多一档（比如 fatal）需要"面板比引擎更懂严重程度"的判断，那种表迟早和实现脱节。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

func levelLabel(level string) string {
	switch level {
	case LevelError:
		return "报错"
	case LevelWarn:
		return "警告"
	default:
		return "普通"
	}
}

// LevelRank 给"至少到这个严重程度"的比较用。不认识的值当普通，
// 因为筛选条件写错不该把本来就该看到的行挡掉。
func LevelRank(level string) int {
	switch level {
	case LevelError:
		return 2
	case LevelWarn:
		return 1
	default:
		return 0
	}
}

// LevelCatalog 是界面的级别筛选项，顺序按严重程度从轻到重。
func LevelCatalog() []EventInfo {
	return []EventInfo{
		{Code: LevelInfo, Label: levelLabel(LevelInfo)},
		{Code: LevelWarn, Label: levelLabel(LevelWarn)},
		{Code: LevelError, Label: levelLabel(LevelError)},
	}
}

// KindCatalog 是界面可读的日志种类。
func KindCatalog() []EventInfo {
	return []EventInfo{
		{Code: string(KindServer), Label: KindServer.Label()},
		{Code: string(KindChat), Label: KindChat.Label()},
	}
}

// 事件码。界面拿 eventLabel，程序拿 event；两个都给是为了前端不再自己摆一张翻译表。
const (
	EventBoot           = "boot"
	EventVersion        = "version"
	EventAccountOK      = "account_ok"
	EventAccountFail    = "account_fail"
	EventPlayerJoin     = "player_join"
	EventPlayerLeave    = "player_leave"
	EventAuthOK         = "player_auth"
	EventPlayerDeath    = "player_death"
	EventGhostGone      = "ghost_gone"
	EventResurrect      = "player_resurrect"
	EventAFKStart       = "afk_start"
	EventAFKEnd         = "afk_end"
	EventKick           = "player_kick"
	EventBan            = "player_ban"
	EventChat           = "chat"
	EventAnnounce       = "announce"
	EventAdminCommand   = "admin_command"
	EventSave           = "save"
	EventSnapshotTrim   = "snapshot_trim"
	EventDiskSpace      = "disk_space"
	EventShardUp        = "shard_up"
	EventShardLink      = "shard_link"
	EventShardReady     = "shard_ready"
	EventSettingsSync   = "settings_sync"
	EventSessionID      = "session_id"
	EventPause          = "pause"
	EventUnpause        = "unpause"
	EventWorldgenFail   = "worldgen_fail"
	EventPanic          = "panic"
	EventLuaError       = "lua_error"
	EventHttpFail       = "http_fail"
	EventBroadcastFail  = "broadcast_fail"
	EventModLoad        = "mod_load"
	EventModEnable      = "mod_enable"
	EventModError       = "mod_error"
	EventModInfoWarn    = "mod_info_warn"
	EventModAPIVersion  = "mod_api_version"
	EventFileReadFail   = "file_read_fail"
	EventFirstRun       = "first_run"
	EventPortalCheck    = "portal_check"
	EventPortalSkip     = "portal_skip"
	EventOverrideApply  = "override_apply"
	EventPermissionLoad = "permission_load"
	EventPlain          = "plain"
)

// eventNames 是界面用的短名，事件目录里的 label 才是整句解释。
// 分开的原因是密度：日志行前面每行都挂一个说明，手机上整句会把原文挤出去；
// 短名负责"一眼看出是哪类事"，整句留给筛选选中时再展开。
var eventNames = map[string]string{
	EventBoot:           "启动信息",
	EventVersion:        "版本号",
	EventAccountOK:      "账户连通",
	EventAccountFail:    "无法启动",
	EventPlayerJoin:     "玩家进服",
	EventPlayerLeave:    "玩家离开",
	EventAuthOK:         "玩家通过验证",
	EventPlayerDeath:    "玩家死亡",
	EventGhostGone:      "幽灵消散",
	EventResurrect:      "玩家被复活",
	EventAFKStart:       "玩家暂时离开",
	EventAFKEnd:         "玩家回来继续",
	EventKick:           "有人被请下线",
	EventBan:            "有人被封禁",
	EventChat:           "玩家聊天",
	EventAnnounce:       "服务端公告",
	EventAdminCommand:   "控制台命令",
	EventSave:           "存档落盘",
	EventSnapshotTrim:   "存档轮换",
	EventDiskSpace:      "磁盘空间",
	EventShardUp:        "端口已监听",
	EventShardLink:      "世界互联",
	EventShardReady:     "世界就绪",
	EventSettingsSync:   "规则同步",
	EventSessionID:      "世界实例",
	EventPause:          "世界暂停",
	EventUnpause:        "世界恢复",
	EventWorldgenFail:   "世界生成失败",
	EventPanic:          "引擎崩溃",
	EventLuaError:       "Lua 报错",
	EventHttpFail:       "网络请求失败",
	EventBroadcastFail:  "列表广播失败",
	EventModLoad:        "模组加载",
	EventModEnable:      "模组启用",
	EventModError:       "模组配置错",
	EventModInfoWarn:    "模组信息缺字段",
	EventModAPIVersion:  "模组版本不符",
	EventFileReadFail:   "文件读取失败",
	EventFirstRun:       "首次启动",
	EventPortalCheck:    "传送门校验",
	EventPortalSkip:     "传送门跳过",
	EventOverrideApply:  "世界覆盖生效",
	EventPermissionLoad: "权限表读取",
}

type rule struct {
	// 匹配方式只有子串一种：这些行是引擎的固定文案，
	// 而每条记录都要过完这张表，正则的成本是实打实的。
	// match 与 module 互斥：前者看消息体，后者只看引擎的方括号标签。
	match  string
	module string

	event string
	level string
	// name 只给"同一个事件码要分两种短名"的规则用（比如 [Error] 与 [WARNING]）；
	// 留空就落到 eventNames 里的那一份。
	name  string
	label string
}

// luaErrRe 匹配引擎打出的 Lua 报错头，形如
// [string "../scripts/map/storygen.lua"]:865: Must specify a layout mode for your level.
// 冒号后面的具体消息每个版本、每种错都不一样，所以只认这个形状，不认消息。
var luaErrRe = regexp.MustCompile(`\[string "[^"]+"\]:\d+:`)

// clockRe 是引擎的行首时钟。小时数不锁两位：长期运行的分片会打出 [100:00:00]，
// 锁死两位的话第 100 小时之后的日志会全部退化成"没有时间"。
var clockRe = regexp.MustCompile(`^\[(\d{1,4}:\d{2}:\d{2})\]:\s*`)

// moduleRe 吃消息体开头的 [标签]。引擎的标签里有数字（[200]）、文件名
// （[vault_key_trial.lua]）、带引号的路径（[string "..."]），所以字符集放开，
// 只要求不含右方括号且不超过 60 字符——超长的方括号内容基本是正文自己带的括号。
var moduleRe = regexp.MustCompile(`^\[([^]\r\n]{1,60})\]:?\s*`)

// useridRe 认 Klei 账户 id。它出现在玩家进服、封禁名单、令牌确认这些行里，
// 是公开信息（服务器列表里就能查到），脱敏时不动它。
var useridRe = regexp.MustCompile(`KU_[A-Za-z0-9]{4,}`)

// labelChat / labelAnnounce 是这两个事件给筛选下拉看的整句说明；
// 逐行前缀只用短名（见 eventNames），否则每行原文都要被一句解释推到屏幕外。
const (
	labelChat     = "玩家在聊天频道里的发言"
	labelAnnounce = "管理员或引擎发到聊天频道的公告"
)

// 聊天行的两种写法。引擎的 server_chat_log.txt 由 C++ 侧写，实测这台机器上只有
// 公告那一行（`[Announcement] ...`）拿得到样本，玩家发言的形状是按
// "(KU_xxx) 名字：内容" 与 "名字(KU_xxx)：内容" 两种通用形态推的。
// 所以这两条只用来"把名字拆出来方便筛选"，拆不出来整行照进 raw，不做任何断言。
var chatNamePrefix = regexp.MustCompile(`^\s*(.+?)\s*\((KU_[A-Za-z0-9]{4,})\)\s*[:：]?\s*`)
var chatNameSuffix = regexp.MustCompile(`^\s*\((KU_[A-Za-z0-9]{4,})\)\s*(.+?)\s*[:：]\s*`)

// rules 按顺序匹配，第一条命中即定性，所以具体的在前、泛化的在后。
// 表里每条都是真机日志或引擎二进制 strings 里核对过的原文。
var rules = []rule{
	// —— 起不来 / 进不了世界 ——
	{match: "!!!! Your Server Will Not Start !!!!", event: EventAccountFail, level: LevelError,
		label: "服务端明确表示不会启动，上下文基本都是账户验证失败"},
	{match: "No auth token could be found.", event: EventAccountFail, level: LevelError,
		label: "没读到有效的 Klei 令牌"},
	{match: "Level data override is invalid!", event: EventWorldgenFail, level: LevelError,
		label: "手工放置的 leveldataoverride.lua 缺引擎要求的五键，这份文件被整份丢弃"},
	{match: "Must provide complete level data to worldgen.", event: EventWorldgenFail, level: LevelError,
		label: "世界规则不完整，引擎拒绝开始生成"},
	{match: "Must specify a layout mode for your level.", event: EventWorldgenFail, level: LevelError,
		label: "世界规则缺必填项 layout_mode"},
	{match: "Must specify the task set for a level!", event: EventWorldgenFail, level: LevelError,
		label: "世界规则缺 task_set，这一项没有地形默认值兜底"},
	{match: "attempt to get length of field 'random_set_pieces'", event: EventWorldgenFail, level: LevelError,
		label: "手工放置的世界规则缺预设自带的字段，设-piece 那一步过不去"},
	{match: "Worldgen had an error", event: EventWorldgenFail, level: LevelError,
		label: "世界生成失败，服务端没能进入存档（进程不一定退出）"},
	{match: "PANIC:", event: EventPanic, level: LevelError,
		label: "引擎内部 panic，常见表现是不退出并反复重打世界生成"},

	// —— 运行期错误 ——
	{match: "Master Server Broadcast Error", event: EventBroadcastFail, level: LevelError,
		label: "向 Klei 官方服务器列表广播失败，不影响玩家直连"},
	{match: "Curl failed", event: EventHttpFail, level: LevelError,
		label: "引擎发起的 HTTP 请求失败，重试次数已打满"},
	{match: "MOD ERROR:", event: EventModError, level: LevelError,
		label: "模组配置里有引擎不认的项，该项不会生效"},
	{match: "Failed to load", event: EventFileReadFail, level: LevelError,
		label: "有文件没能读进来"},

	// —— 警告 ——
	{match: "api_version for", event: EventModAPIVersion, level: LevelWarn,
		label: "模组声明的 api_version 和引擎不一致，这个模组不会加载"},
	{match: "WARNING loading modinfo.lua", event: EventModInfoWarn, level: LevelWarn,
		label: "模组 modinfo.lua 缺字段，引擎按默认值处理"},
	{match: "Skipping portal", event: EventPortalSkip, level: LevelWarn,
		label: "世界生成阶段的传送门跳过提示，世界互通之后还会有验证行"},
	{match: "could not load", event: EventFirstRun, level: LevelWarn,
		label: "还没有对应的存档文件，引擎随后会自己建（首启常见）"},

	// —— 有明确语义的正常事件 ——
	{match: "PersistRootStorage is now", event: EventBoot, level: LevelInfo,
		label: "本轮启动的第一行，给出引擎实际使用的存档根目录"},
	{match: "Current time:", event: EventBoot, level: LevelInfo,
		label: "本轮启动的绝对时间（服务器本地时区）"},
	{match: "Version: ", event: EventVersion, level: LevelInfo, label: "服务端版本号"},
	{match: "Account Communication Success", event: EventAccountOK, level: LevelInfo,
		label: "Klei 账户接口连通"},
	{match: "from TokenPurpose", event: EventAccountOK, level: LevelInfo,
		label: "令牌对应的账户已确认（KU_ 开头是账户 id，不是密钥）"},
	{match: "Client authenticated:", event: EventAuthOK, level: LevelInfo,
		label: "玩家的 Klei 账户通过验证，正在进入游戏；这行给的是账户编号和账户上的显示名"},
	{match: "has joined the game", event: EventPlayerJoin, level: LevelInfo, label: "玩家进入了世界"},
	{match: "has left the game", event: EventPlayerLeave, level: LevelInfo, label: "玩家离开了世界"},
	// —— 玩家行为的那几句 ——
	// 这些短语不是社区帖转述：它们是引擎自带文案表里的原文，2026-11-03 在云机的
	// scripts/strings.lua 第 10753~10765 行逐条核对过（DEATH_ANNOUNCEMENT_1、
	// REZ_ANNOUNCEMENT、START_AFK、STOP_AFK、GHOST_DEATH_ANNOUNCEMENT_*）。
	// 服务端换语言时这几条会匹配不上，下面按方括号标签的那几条是兜底。
	{match: "was killed by", event: EventPlayerDeath, level: LevelInfo,
		label: "引擎广播的死亡公告：这句说的是谁、死于什么"},
	{match: "is gone forever", event: EventGhostGone, level: LevelInfo,
		label: "幽灵彻底消散，这个玩家在这一轮里不会再回到世界"},
	{match: "was resurrected by", event: EventResurrect, level: LevelInfo,
		label: "有人把这名玩家的尸体复活了"},
	{match: "went away for a bit", event: EventAFKStart, level: LevelInfo,
		label: "引擎判定这名玩家挂机：连接还在，角色不再操作"},
	{match: "has returned from their relaxing", event: EventAFKEnd, level: LevelInfo,
		label: "刚才挂机的这名玩家回来继续操作"},
	{match: "RemoteCommandInput:", event: EventAdminCommand, level: LevelInfo,
		label: "有一条命令进入服务端控制台执行（可能来自面板，也可能来自 screen 会话）"},
	{match: "Serializing world:", event: EventSave, level: LevelInfo, label: "引擎把当前世界状态写进存档文件"},
	{match: "Truncating to snapshot #", event: EventSnapshotTrim, level: LevelInfo,
		label: "删掉超出保留数量的旧存档档位"},
	{match: "Available disk space for save files:", event: EventDiskSpace, level: LevelInfo,
		label: "引擎报告的存档所在分区可用空间"},
	{match: "Starting master server", event: EventShardUp, level: LevelInfo,
		label: "地表开始起服务器"},
	{match: "Shard server started on port", event: EventShardUp, level: LevelInfo, label: "端口已监听"},
	{match: "secondary shard LUA is now ready!", event: EventShardReady, level: LevelInfo,
		label: "洞穴已进入世界，两个世界开始互通"},
	{match: "Secondary shard is waiting for LUA", event: EventShardLink, level: LevelInfo,
		label: "洞穴还在等自己的 Lua 起来"},
	{match: "Connecting to master...", event: EventShardLink, level: LevelInfo, label: "洞穴正在连地表"},
	{match: "Resyncing master world option", event: EventSettingsSync, level: LevelInfo,
		label: "地表把世界选项同步给洞穴"},
	{match: "new session identifier:", event: EventSessionID, level: LevelInfo,
		label: "本轮世界实例 id，对应 save/session 下的目录名"},
	{match: "Sim paused", event: EventPause, level: LevelInfo,
		label: "世界已暂停（无人暂停会把助手 MOD 的调度一起冻住）"},
	{match: "Server Paused", event: EventPause, level: LevelInfo,
		label: "世界已暂停（无人暂停会把助手 MOD 的调度一起冻住）"},
	{match: "Server Autopaused", event: EventPause, level: LevelInfo, label: "引擎自动暂停了世界（没人在线）"},
	{match: "Sim unpaused", event: EventUnpause, level: LevelInfo, label: "世界恢复运行"},
	{match: "Server Unpaused", event: EventUnpause, level: LevelInfo, label: "世界恢复运行"},
	{match: "Overwriting savedata with override file.", event: EventOverrideApply, level: LevelWarn,
		label: "引擎用 worldgenoverride.lua 覆盖了存档里的世界规则，说明这份文件生效了"},
	{match: "SUCCESS: Loaded modoverrides.lua", event: EventModEnable, level: LevelInfo,
		label: "模组开关配置已被引擎接受"},
	{match: "modoverrides.lua enabling", event: EventModEnable, level: LevelInfo, label: "引擎按覆盖配置打开了某个模组"},
	{match: "applying configuration_options from modoverrides.lua", event: EventModEnable, level: LevelInfo,
		label: "引擎把 modoverrides.lua 里的模组配置项应用了上去"},
	{match: "Registering Mods:", event: EventModLoad, level: LevelInfo, label: "开始登记模组"},
	{match: "Mods registered.", event: EventModLoad, level: LevelInfo, label: "模组登记完成"},
	{match: "ModIndex: Load sequence finished successfully", event: EventModLoad, level: LevelInfo,
		label: "模组加载流程走完"},
	{match: "OnLoadPermissionList:", event: EventPermissionLoad, level: LevelInfo, label: "读取世界权限表"},
	{match: "Validating portal", event: EventPortalCheck, level: LevelInfo, label: "校验世界传送门"},
}

// labelRules 只按引擎的方括号标签定性，排在 rules 之后：
// [Error] Master Server Broadcast Error 要先被上面那条更具体的消息命中，
// 落到这里的只是"引擎确实记成错误、但面板说不出是什么错"的行。
//
// 后面那五条公告标签是本地化的兜底：句子部分的原文来自引擎自带的英文文案表，
// 服务端换了语言之后那些 match 就命中不了了，而方括号标签是 C++ 侧写死的
// （2026-11-03 在云机服务端二进制里核对过 [Death Announcement] %s 这类模板），
// 只靠标签定性虽然读不出"死于什么"，但至少这一行不会被归进"没归类"。
var labelRules = []rule{
	{module: "Error", event: EventPlain, level: LevelError, name: "引擎错误", label: "引擎把它记成了错误"},
	{module: "WARNING", event: EventPlain, level: LevelWarn, name: "引擎警告", label: "引擎把它记成了警告"},
	{module: "Death Announcement", event: EventPlayerDeath, level: LevelInfo,
		label: "引擎的死亡公告标签，句子部分因为服务端语言不同没能拆开"},
	{module: "Resurrect Announcement", event: EventResurrect, level: LevelInfo,
		label: "引擎的复活公告标签，句子部分因为服务端语言不同没能拆开"},
	{module: "Join Announcement", event: EventPlayerJoin, level: LevelInfo,
		label: "引擎的进服公告标签，句子部分因为服务端语言不同没能拆开"},
	// 离开公告在真机上就是这个形状（2026-11-03 云机 Master/server_chat_log.txt 第 2 行：
	// `[Leave Announcement] 风萧萧`）。没这条的话它会掉到"聊天文件里没命中规则就是发言"
	// 那一句，于是"谁离开了世界"在表里显示成"谁说了一句话"，名字还是整串。
	{module: "Leave Announcement", event: EventPlayerLeave, level: LevelInfo,
		label: "引擎的离服公告标签：这名玩家离开了世界"},
	{module: "Kick", event: EventKick, level: LevelInfo,
		label: "管理员（或面板）把某个玩家请下了线，原文里同时有双方的账户编号"},
	{module: "Ban", event: EventBan, level: LevelInfo,
		label: "管理员把某个玩家加进封禁名单，原文里同时有双方的账户编号"},
}

// EventInfo 是界面筛选下拉里的一项。
type EventInfo struct {
	Code string `json:"code"`
	// Name 是短名，下拉和行前缀用它；Label 是整句解释，选中后展开给人看。
	Name  string `json:"name"`
	Label string `json:"label"`
}

var (
	catalogOnce sync.Once
	catalog     []EventInfo
)

// EventCatalog 列出面板能识别的全部事件码，顺序就是规则表的顺序。
// 从规则表里推而不是手写一份：手写那份迟早和规则漂移，
// 结果是界面能筛出一个永远选不出记录的事件码。
//
// 两张表都要过：只按消息匹配的那几张在 rules，被服务端语言拖住的几条只能靠
// 方括号标签（labelRules）。漏掉后一张的话，"有人被请下线"这类事件在界面筛不出来、
// 在 /api/v1/clusters/{name}/shards/{shard}/events 里又确实会给出，
// 就成了"字典里没有、数据里却有"。
func EventCatalog() []EventInfo {
	catalogOnce.Do(func() {
		seen := map[string]bool{}
		for _, group := range [][]rule{rules, labelRules} {
			for _, rl := range group {
				if rl.label == "" || rl.event == "" || rl.event == EventPlain || seen[rl.event] {
					continue
				}
				seen[rl.event] = true
				catalog = append(catalog, EventInfo{Code: rl.event, Name: eventNames[rl.event], Label: rl.label})
			}
		}
		for _, extra := range []EventInfo{
			{Code: EventLuaError, Name: eventNames[EventLuaError], Label: "引擎的 Lua 报错原文"},
			{Code: EventAnnounce, Name: eventNames[EventAnnounce], Label: labelAnnounce},
			{Code: EventChat, Name: eventNames[EventChat], Label: labelChat},
		} {
			// lua_error / announce / chat 三条不在消息表里（一个靠形状、两个在解析末尾补），
			// 所以规则表推不出来，只能自己列——但先问一句"上面是不是已经有了"，
			// 免得同一件事在字典里出现两次，界面里两个一样的选项。
			if !seen[extra.Code] {
				catalog = append(catalog, extra)
			}
		}
	})
	return catalog
}

// MaxRawRunes 是一条记录外送的正文字数上限。引擎的 Lua 堆栈和 Klei 接口回包
// 能到几千字符，一行就可能把整个响应撑爆；截断会打标记（cut=true），不静默。
const MaxRawRunes = 1000

// Entry 是一条解析好的日志记录。
type Entry struct {
	// Offset 是这一行在文件里的字节位置。带上它是为了让"面板上这条"和
	// "文件里那行"能互相对上——排查世界生成问题时经常要这么干。
	Offset int64  `json:"offset"`
	Clock  string `json:"clock,omitempty"`
	At     string `json:"at,omitempty"`
	Raw    string `json:"raw"`
	Module string `json:"module,omitempty"`

	Level      string `json:"level"`
	LevelLabel string `json:"levelLabel"`
	Event      string `json:"event,omitempty"`
	EventName  string `json:"eventName,omitempty"`
	EventLabel string `json:"eventLabel,omitempty"`

	UserID string `json:"userid,omitempty"`
	Player string `json:"player,omitempty"`
	// Target 只在"一个人对另一个人做了点什么"的行里有值（踢下线、封禁），
	// 是被操作的那一位。单开一个字段而不是塞进 Player：
	// Player 和 UserID 是配对的（这条记录算在谁头上），把被踢的人写进 Player
	// 会让账户编号变成管理员的、名字变成被踢的，时间线上就是张错的表。
	Target string `json:"target,omitempty"`
	Cut    bool   `json:"cut,omitempty"`
}

// ParseLine 解析一行。第二个返回值为 false 表示这行不该出现在列表里
// （引擎会打大量空行与纯空白行，实测 Master 与 Caves 各 5 行）。
func ParseLine(kind Kind, offset int64, line string) (Entry, bool) {
	text := strings.TrimRight(strings.ReplaceAll(line, "\r", ""), " \t")
	if strings.TrimSpace(text) == "" {
		return Entry{}, false
	}
	e := Entry{Offset: offset}
	body := text
	if m := clockRe.FindStringSubmatch(body); m != nil {
		e.Clock = m[1]
		body = strings.TrimLeft(body[len(m[0]):], " \t")
	}
	// 脱敏在拆标签之前：标签也是外送内容，不能假设它永远只是一段模块名。
	body = redact(body)
	msg := body
	if m := moduleRe.FindStringSubmatch(body); m != nil {
		e.Module = m[1]
		msg = strings.TrimLeft(body[len(m[0]):], " \t")
	}
	e.Raw, e.Cut = clampRunes(msg, MaxRawRunes)

	e.Level, e.Event, e.EventName, e.EventLabel = classify(kind, e.Module, body, msg)
	e.LevelLabel = levelLabel(e.Level)
	e.UserID = useridRe.FindString(body)
	if kind == KindChat {
		e.Player = chatPlayer(msg)
		if e.Module == "Announcement" {
			e.Event, e.EventName, e.EventLabel = EventAnnounce, eventNames[EventAnnounce], labelAnnounce
		}
	}
	// 排在聊天分支之后：聊天行已经先按"谁在说话"拆过一遍，这里只补那些
	// 定性成进出/死亡/挂机的公告行（它们在同一份文件里，名字也在主语位置）。
	applySubjects(&e, msg)
	return e, true
}

// classify 返回 (level, event, name, label)：消息类规则匹配 msg（不含方括号标签的正文），
// 形状类规则（Lua 报错）必须匹配 body（含标签），因为 `[string "../x.lua"]:229: `
// 这整个前缀就是它的形状——标签被拆走之后再去找 `[string "]` 永远找不到。
// 调用点用的是四个具名返回值，别按位置记。
func classify(kind Kind, module, body, msg string) (string, string, string, string) {
	for _, rl := range rules {
		if rl.module == "" && strings.Contains(msg, rl.match) {
			return rl.level, rl.event, ruleName(rl), rl.label
		}
	}
	// Lua 报错排在消息表之后：storygen 那句 assert 同时也是 Lua 报错形状，
	// 但"世界生成失败"才是管理员要看到的结论。
	if luaErrRe.MatchString(body) {
		return LevelError, EventLuaError, eventNames[EventLuaError], "引擎的 Lua 报错原文"
	}
	for _, rl := range labelRules {
		if rl.module == module {
			return rl.level, rl.event, ruleName(rl), rl.label
		}
	}
	if kind == KindChat {
		// 聊天文件里没命中规则的行就是玩家发言，这句必须给出来：
		// 留空的话日志页的事件汇总会把它归进"没归类的那些行"，
		// 而筛选下拉里明明写着"玩家聊天"，同一批行在两处显示成两件事。
		return LevelInfo, EventChat, eventNames[EventChat], labelChat
	}
	return LevelInfo, EventPlain, "", ""
}

func ruleName(rl rule) string {
	if rl.name != "" {
		return rl.name
	}
	return eventNames[rl.event]
}

// chatPlayer 从聊天行里拆出发言者名字。拆不出来就留空：
// 名字只是筛选用的便利字段，raw 一直在，拆错比不拆更糟。
func chatPlayer(raw string) string {
	if m := chatNamePrefix.FindStringSubmatch(raw); m != nil {
		return strings.TrimSpace(m[1])
	}
	if m := chatNameSuffix.FindStringSubmatch(raw); m != nil {
		return strings.TrimSpace(m[2])
	}
	return ""
}

// subjectRule 是"这一行在说谁"。行为公告的句子都是引擎自己拼的
// （见 rules 里那几句的出处），主语固定在谓语前面，所以一条正则拆一次就够。
// player/target 是要取的捕获组编号，0 表示这一类没有那一项：
// 踢人和封禁有两个玩家，被操作的那位单独放 Target，
// 免得 Player 是被踢的、UserID 却是动手的，时间线把账算错人头上。
//
// 同一个事件可以有好几种句式（按顺序试，命中即停），而且句式跟引擎打的方括号标签有关，
// 所以规则要么带 onlyModules，要么认整句谓语。
//
// 进服这件事两种都要：Lua 拼的公告是 `阿黎 has joined the game`，
// 而实测云机的聊天文件里打的是 C++ 侧的标签行 `[Join Announcement] 风萧萧`
// （2026-11-03 Master/server_chat_log.txt 第 1 行）——正文整串就是名字，没有谓语。
// 只认前一种的话，这个人在表里就没有"进服"这一记。
// 另一边，`Client authenticated: (KU_x) 名字` 的第二槽位实测两种都见过：
// 真机给的是显示名，本地夹具那份是一串 Klei 数字 id，
// 所以那一行的名字要经过"整串是数字就不当名字"这道判断（见 events.go 的 digitsOnlyRe）。
type subjectRule struct {
	re     *regexp.Regexp
	player int
	target int
	// onlyModules 限定这条句式只在引擎打了这些方括号标签时才成立。
	// 有了它才能收"整串正文就是名字"这种最宽松的写法：
	// `[Join Announcement] 风萧萧` 的正文确实只有一个名字，
	// 而 `小满 has joined the game` 的正文整串不是，两条不能共用一条正则。
	onlyModules []string
}

var subjects = map[string][]subjectRule{
	EventPlayerJoin: {
		{re: regexp.MustCompile(`^(.+?)\s+has joined the game`), player: 1},
		{re: regexp.MustCompile(`^([^\r\n]{1,40})$`), player: 1, onlyModules: []string{"Join Announcement"}},
	},
	EventPlayerLeave: {
		{re: regexp.MustCompile(`^(.+?)\s+has left the game`), player: 1},
		{re: regexp.MustCompile(`^([^\r\n]{1,40})$`), player: 1, onlyModules: []string{"Leave Announcement"}},
	},
	EventPlayerDeath: {{re: regexp.MustCompile(`^(.+?)\s+was killed by\s+`), player: 1}},
	EventGhostGone:   {{re: regexp.MustCompile(`^(.+?)\s+lost all of (?:his|her|their) humanity`), player: 1}},
	EventResurrect:   {{re: regexp.MustCompile(`^(.+?)\s+was resurrected by\s+`), player: 1}},
	EventAFKStart:    {{re: regexp.MustCompile(`^(.+?)\s+went away for a bit`), player: 1}},
	EventAFKEnd:      {{re: regexp.MustCompile(`^(.+?)\s+has returned from their relaxing`), player: 1}},
	// 引擎的模板是 `(KU_a) 管理员 kicked (KU_b) 被踢的`，账户编号在名字前面。
	EventKick: {
		{re: regexp.MustCompile(`^(?:\(([^)]*)\)\s*)?(.+?)\s+kicked\s+(?:\(([^)]*)\)\s*)?(.+?)\s*$`), player: 2, target: 4},
	},
	EventBan: {
		{re: regexp.MustCompile(`^(?:\(([^)]*)\)\s*)?(.+?)\s+banned\s+(?:\(([^)]*)\)\s*)?(.+?)\s*$`), player: 2, target: 4},
	},
}

// containsModule 判断引擎的方括号标签在不在这几条允许名单里。
func containsModule(list []string, module string) bool {
	for _, m := range list {
		if m == module {
			return true
		}
	}
	return false
}

// applySubjects 补 Player/Target。两个方向都留空而不是猜：
// 服务端换了语言、或者引擎改了句式，这里就是拆不出来，
// 而时间线宁可少一行也不能凭空造一个玩家出来。
func applySubjects(e *Entry, msg string) {
	for _, sr := range subjects[e.Event] {
		if len(sr.onlyModules) > 0 && !containsModule(sr.onlyModules, e.Module) {
			continue
		}
		m := sr.re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		if sr.player > 0 && sr.player < len(m) {
			e.Player = strings.TrimSpace(m[sr.player])
		}
		if sr.target > 0 && sr.target < len(m) {
			e.Target = strings.TrimSpace(m[sr.target])
		}
		return
	}
}

// 脱敏规则按顺序执行。两条的替换产物都必须不会再被后面的规则命中
// （第一条的产物不含 pds-g；第二条的值字符集排除了左方括号，所以第一条的标记不会二次变形）。
var redactors = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Klei 的服务令牌形如 pds-g^....^....，字符集含 ^ / = +，整串没有空格。
	// 这条是主力：日志行会经由接口、SSE、探针输出好几条路离开进程。
	{regexp.MustCompile(`pds-g[\x21-\x7e]*`), "[已隐去Klei令牌]"},
	// 兜底：任何写成 token=xxx / token：xxx 的值都不外送，不管是不是 Klei 的。
	// ${1} 原样保留"token ="这一段，免得把 `Token: 有效` 这类正常文案吃掉半截；
	// 值里排除左方括号，是为了让上面那条的产物 `[已隐去Klei令牌]` 不会被这条再遮一次——
	// 两条规则叠出来的标记会丢信息（用户看不出遮掉的到底是不是 Klei 令牌）。
	{regexp.MustCompile(`(?i)(token\s*[=:]\s*)[^\s,;\[]+`), "${1}[已隐去]"},
}

func redact(s string) string {
	for _, rd := range redactors {
		s = rd.re.ReplaceAllString(s, rd.repl)
	}
	return s
}

func clampRunes(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	return string([]rune(s)[:max]) + "…", true
}

// ClockSeconds 把引擎的相对时钟换成秒数。解不出来返回 false——
// 一份没有可解时间戳的日志不该被硬安上时间。
func ClockSeconds(clock string) (int64, bool) {
	parts := strings.Split(clock, ":")
	if len(parts) != 3 {
		return 0, false
	}
	var total int64
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		// 分与秒必须小于 60；最左边那段（小时）不限制——跑过 100 小时的服务器打的是
		// [100:00:00]，拿 59 去卡它等于把长期运行的分片全部换成"没有时间"。
		if i > 0 && n > 59 {
			return 0, false
		}
		total = total*60 + int64(n)
	}
	return total, true
}

// stampAt 在起始时间已知时补上墙上时间（UTC，与面板其它接口的时间格式一致）。
// 起始时间是零值就不补——宁可不给时间，也不给一个猜出来的时间。
func stampAt(entries []Entry, start time.Time) {
	if start.IsZero() {
		return
	}
	for i := range entries {
		secs, ok := ClockSeconds(entries[i].Clock)
		if !ok {
			continue
		}
		entries[i].At = start.Add(time.Duration(secs) * time.Second).UTC().Format(time.RFC3339)
	}
}
