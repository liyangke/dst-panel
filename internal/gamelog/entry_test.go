package gamelog

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 这里的每一条样本都是 2026-10-31 从 dst-cloud 的 Cluster_1 真实日志里取的原文
// （Master/server_log.txt 1146 行、Caves 923 行、server_chat_log.txt 1 行），
// 只有 KU_ 后面的账户 id 换成了本机那份。测试的价值在于"引擎真的这么打"，
// 造一条社区帖里的假格式只会把分类器钉在错误的前提上。
func TestParseLineRealShapes(t *testing.T) {
	cases := []struct {
		name     string
		kind     Kind
		line     string
		clock    string
		module   string
		level    string
		event    string
		userid   string
		wantTail string
	}{
		{
			name: "启动第一行给出真实存档根目录", kind: KindServer,
			line:  "[00:00:00]: PersistRootStorage is now /home/ubuntu/.klei//DoNotStarveTogether/Cluster_1/Master/ ",
			clock: "00:00:00", level: LevelInfo, event: EventBoot,
		},
		{
			name: "Klei 列表广播的重试是报错但不是本地故障", kind: KindServer,
			line:  `[00:00:35]: [Http] Curl failed[1] with HTTP_500, retrying (2 times). Response: _{"Error":{"Code":"E_ROWID_EXIST"}}_`,
			clock: "00:00:35", module: "Http", level: LevelError, event: EventHttpFail,
		},
		{
			name: "广播错误标签下有更具体的判断", kind: KindServer,
			line:  "[00:00:36]: [Error] Master Server Broadcast Error: E_ROWID_EXIST",
			clock: "00:00:36", module: "Error", level: LevelError, event: EventBroadcastFail,
		},
		{
			name: "账户接口通了的标签是数字", kind: KindServer,
			line:  "[00:00:09]: [200] Account Communication Success (6)",
			clock: "00:00:09", module: "200", level: LevelInfo, event: EventAccountOK,
		},
		{
			name: "令牌确认行能提出账户 id", kind: KindServer,
			line:  "[00:00:09]: Received (KU_tvX4ySmb) from TokenPurpose",
			clock: "00:00:09", level: LevelInfo, event: EventAccountOK, userid: "KU_tvX4ySmb",
		},
		{
			name: "世界同步行", kind: KindServer,
			line:  "[00:06:01]: [SyncWorldSettings] Resyncing master world option day_length = 20 to secondary shards.",
			clock: "00:06:01", module: "SyncWorldSettings", level: LevelInfo, event: EventSettingsSync,
		},
		{
			name: "落盘存档", kind: KindServer,
			line:  "[05:43:57]: Serializing world: session/206B72E995BA2B91/0000000059",
			clock: "05:43:57", level: LevelInfo, event: EventSave,
		},
		{
			name: "新会话 id", kind: KindServer,
			line:  "[00:00:34]: Telling Client our new session identifier: DCF8E1BCE43CE58A",
			clock: "00:00:34", level: LevelInfo, event: EventSessionID,
		},
		{
			name: "面板注入的命令会留痕", kind: KindServer,
			line:  `[00:03:12]: RemoteCommandInput: "c_save()"`,
			clock: "00:03:12", level: LevelInfo, event: EventAdminCommand,
		},
		{
			name: "无人暂停", kind: KindServer,
			line:  "[00:10:00]: Sim paused",
			clock: "00:10:00", level: LevelInfo, event: EventPause,
		},
		{
			name: "次分片就绪是两个分片真的连上的证据", kind: KindServer,
			line:  "[00:00:40]: [Shard] secondary shard LUA is now ready!",
			clock: "00:00:40", module: "Shard", level: LevelInfo, event: EventShardReady,
		},
		{
			// 这条钉的是"标签会被拆走"这件事：正文匹配之前 [Shard] 已经进了 module，
			// 规则里照抄带方括号的原文就永远匹配不上（本机界面夹具实测到它掉进 plain）。
			name: "主分片起服务器那行按正文定性", kind: KindServer,
			line:  "[00:00:05]: [Shard] Starting master server and waiting for connections",
			clock: "00:00:05", module: "Shard", level: LevelInfo, event: EventShardUp,
		},
		{
			name: "控制台参数废弃只是警告", kind: KindServer,
			line:  "[00:00:00]: [WARNING] -console has been deprecated: Use the [MISC] / console_enabled setting instead.",
			clock: "00:00:00", module: "WARNING", level: LevelWarn, event: EventPlain,
		},
		{
			name: "首启缺存档文件不算故障", kind: KindServer,
			line:  "[00:00:08]: PlayerDeaths could not load morgue",
			clock: "00:00:08", level: LevelWarn, event: EventFirstRun,
		},
		{
			name: "modinfo 缺字段的警告原文里没有方括号标签", kind: KindServer,
			line:  "WARNING loading modinfo.lua: DSTPanelHelper does not specify a configuration_options table",
			level: LevelWarn, event: EventModInfoWarn,
		},
		{
			// 这条是排序的验证：它既符合 Lua 报错形状，又是引擎的 assert 消息。
			// 归成 lua_error 的话，界面就丢了"世界生成失败"这个能决定下一步的结论。
			name: "世界生成的 Lua 报错优先按世界生成失败定性", kind: KindServer,
			line:  `[00:00:45]: [string "../scripts/map/storygen.lua"]:865: Must specify a layout mode for your level.`,
			clock: "00:00:45", module: `string "../scripts/map/storygen.lua"`,
			level: LevelError, event: EventWorldgenFail,
		},
		{
			name: "认不出的 Lua 报错按形状兜住", kind: KindServer,
			line:  `[00:01:02]: [string "../scripts/update.lua"]:229: Update() called on paused server!`,
			clock: "00:01:02", module: `string "../scripts/update.lua"`,
			level: LevelError, event: EventLuaError,
		},
		{
			name: "引擎拒绝启动", kind: KindServer,
			line:  "[00:00:01]: !!!! Your Server Will Not Start !!!!",
			clock: "00:00:01", level: LevelError, event: EventAccountFail,
		},
		{
			// 通过验证和进服是同一次连接的两行，各自写法不同。合成 player_join 的话，
			// 时间线里一个人进一次服会算出"进服 2 次"，所以给它自己的事件码。
			name: "玩家通过验证", kind: KindServer,
			line:  "[00:01:23]: Client authenticated: (KU_9aBcDeF01) 14672355439136578013",
			clock: "00:01:23", level: LevelInfo, event: EventAuthOK, userid: "KU_9aBcDeF01",
		},
		{
			name: "跑过一百小时后的小时数不是两位数", kind: KindServer,
			line:  "[100:00:00]: Available disk space for save files: 41209 MB",
			clock: "100:00:00", level: LevelInfo, event: EventDiskSpace,
		},
		{
			name: "公告有专门的事件码", kind: KindChat,
			line:  "[00:01:44]: [Announcement] 面板首启验证：这条中文公告来自管理面板",
			clock: "00:01:44", module: "Announcement", level: LevelInfo, event: EventAnnounce,
			wantTail: "面板首启验证：这条中文公告来自管理面板",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := ParseLine(tc.kind, 0, tc.line)
			if !ok {
				t.Fatalf("这一行必须解析成记录：%q", tc.line)
			}
			if e.Clock != tc.clock {
				t.Errorf("clock = %q，期望 %q", e.Clock, tc.clock)
			}
			if e.Module != tc.module {
				t.Errorf("module = %q，期望 %q", e.Module, tc.module)
			}
			if e.Level != tc.level {
				t.Errorf("level = %q，期望 %q（原文 %q）", e.Level, tc.level, tc.line)
			}
			if e.Event != tc.event {
				t.Errorf("event = %q，期望 %q（原文 %q）", e.Event, tc.event, tc.line)
			}
			if e.UserID != tc.userid {
				t.Errorf("userid = %q，期望 %q", e.UserID, tc.userid)
			}
			if e.LevelLabel == "" {
				t.Errorf("每条记录都要带中文级别，否则界面只能显示 info 这种码值")
			}
			if tc.wantTail != "" && !strings.HasSuffix(e.Raw, tc.wantTail) {
				t.Errorf("raw = %q，期望以 %q 结尾", e.Raw, tc.wantTail)
			}
		})
	}
}

func TestParseLineDropsBlankLines(t *testing.T) {
	// 实测 Master 与 Caves 各有 5 行空行/纯空白行，它们会把窗口占掉却什么都没说。
	for _, line := range []string{"", " ", "\t", "\r\n", "\t\t"} {
		if _, ok := ParseLine(KindServer, 0, line); ok {
			t.Errorf("空行 %q 不该进列表", line)
		}
	}
}

// 令牌脱敏是这个包存在的理由之一：日志行会经接口、SSE、平台运行日志好几条路离开进程，
// 而 Klei 的服务令牌一旦出现在任何一个响应里就等于泄露。
func TestParseLineRedactsKleiToken(t *testing.T) {
	const token = "pds-g^KU_tvX4ySmb^um8I49hTGVMuXVeupDF7DlTL/MeTdZFwo5qisVGaHPA=^u0qG/7isngoYLJKi"
	cases := []string{
		"[00:00:09]: Auth token is " + token,
		"[00:00:09]: [Http] header " + token + " sent",
		"[00:00:09]: token=" + token,
	}
	for _, line := range cases {
		e, ok := ParseLine(KindServer, 0, line)
		if !ok {
			t.Fatalf("这一行该有记录：%q", line)
		}
		if strings.Contains(e.Raw, "pds-g") || strings.Contains(e.Raw, "um8I49hTGVMu") {
			t.Errorf("令牌漏出：%q", e.Raw)
		}
		if !strings.Contains(e.Raw, "[已隐去Klei令牌]") {
			t.Errorf("要留下明确的隐去标记，否则用户以为日志本来就没这一项：%q", e.Raw)
		}
	}
}

// 脱敏不能误伤：这两行都是引擎真的会打的、对排查有用的话。
// 把 `Token retrieved from: <路径>` 遮成 `[已隐去]` 等于把最关键的线索擦掉。
func TestParseLineKeepsTokenDiagnosticsIntact(t *testing.T) {
	for line, want := range map[string]string{
		"[00:00:00]: Token retrieved from: /home/ubuntu/.klei//DoNotStarveTogether/Cluster_1/cluster_token.txt": "cluster_token.txt",
		"[00:00:09]: Received (KU_tvX4ySmb) from TokenPurpose":                                                  "from TokenPurpose",
	} {
		e, _ := ParseLine(KindServer, 0, line)
		if !strings.Contains(e.Raw, want) {
			t.Errorf("%q 里的 %q 被脱敏误伤了：%q", line, want, e.Raw)
		}
	}
}

func TestParseLineTruncatesOverlongBody(t *testing.T) {
	// 用中文构造：按字节截会把多字节字符切成半个，前端拿到非法 UTF-8 整个响应都废了。
	body := strings.Repeat("模组配置项", 400) // 2000 个汉字
	e, ok := ParseLine(KindServer, 0, "[00:00:01]: "+body)
	if !ok {
		t.Fatal("超长行也该有记录")
	}
	if !e.Cut {
		t.Error("截断必须打标记，不能静默")
	}
	if n := len([]rune(e.Raw)); n != MaxRawRunes+1 { // 正文 + 省略号
		t.Errorf("截到了 %d 个字符，期望 %d", n, MaxRawRunes+1)
	}
	if !strings.HasSuffix(e.Raw, "…") {
		t.Errorf("截断处要有省略号: %q", e.Raw[len(e.Raw)-8:])
	}
	short, _ := ParseLine(KindServer, 0, "[00:00:01]: 短行")
	if short.Cut {
		t.Error("没截断的行不该带 cut 标记")
	}
}

func TestParseLineChatPlayerShapes(t *testing.T) {
	// 两种发言形态都试；拆不出名字时必须是整行照给，不能把内容截断。
	cases := map[string]struct{ name, uid string }{
		"[00:12:34]: 阿黎(KU_9aBcDeF01): 有人在吗":  {name: "阿黎", uid: "KU_9aBcDeF01"},
		"[00:12:34]: (KU_9aBcDeF01) 阿黎: 有人在吗": {name: "阿黎", uid: "KU_9aBcDeF01"},
		"[00:12:35]: 这句没有账户 id 也没有冒号":         {name: "", uid: ""},
	}
	for line, want := range cases {
		e, _ := ParseLine(KindChat, 0, line)
		if e.Player != want.name {
			t.Errorf("%q 拆出的名字 = %q，期望 %q", line, e.Player, want.name)
		}
		if e.UserID != want.uid {
			t.Errorf("%q 拆出的 userid = %q，期望 %q", line, e.UserID, want.uid)
		}
		if e.Event != EventChat {
			t.Errorf("%q 的 event = %q，聊天日志里没命中规则的行要算聊天", line, e.Event)
		}
		if e.EventName != eventNames[EventChat] {
			t.Errorf("%q 的 eventName = %q，期望 %q", line, e.EventName, eventNames[EventChat])
		}
		// 中文说明必须一起给：留空会让日志页的事件汇总把玩家发言算进"没归类的那些行"，
		// 而筛选下拉里明明写着"玩家聊天"。
		if e.EventLabel != labelChat {
			t.Errorf("%q 的 eventLabel = %q，期望 %q", line, e.EventLabel, labelChat)
		}
	}
}

func TestClockSeconds(t *testing.T) {
	for _, tc := range []struct {
		clock string
		secs  int64
		ok    bool
	}{
		{"00:06:01", 361, true},
		{"05:43:57", 20637, true},
		{"100:00:00", 360000, true},
		{"00:60:00", 0, false}, // 分钟不可能到 60，出现说明这行不是时钟
		{"00:00", 0, false},
		{"", 0, false},
		{"ab:cd:ef", 0, false},
	} {
		got, ok := ClockSeconds(tc.clock)
		if ok != tc.ok || (ok && got != tc.secs) {
			t.Errorf("ClockSeconds(%q) = %d,%v，期望 %d,%v", tc.clock, got, ok, tc.secs, tc.ok)
		}
	}
}

// stampAt 是"能不能换算出墙上时间"的唯一出口：换算不了就不给时间，
// 给一个猜出来的时间比不给更糟（用户会拿它去对操作记录）。
func TestStampAtUsesFileStart(t *testing.T) {
	start := time.Date(2026, 10, 31, 2, 17, 35, 0, time.Local)
	entries := []Entry{{Clock: "00:06:01"}, {Clock: ""}, {Clock: "99:99:99"}}
	stampAt(entries, start)

	want := start.Add(361 * time.Second).UTC().Format(time.RFC3339)
	if entries[0].At != want {
		t.Errorf("At = %q，期望 %q", entries[0].At, want)
	}
	if entries[1].At != "" || entries[2].At != "" {
		t.Errorf("换不出时间的行必须留空: %q %q", entries[1].At, entries[2].At)
	}

	none := []Entry{{Clock: "00:06:01"}}
	stampAt(none, time.Time{})
	if none[0].At != "" {
		t.Errorf("起始时间未知时不能补 At，实际 %q", none[0].At)
	}
}

func TestEventCatalogIsUsableAsFilterOptions(t *testing.T) {
	list := EventCatalog()
	if len(list) < 20 {
		t.Fatalf("目录只有 %d 项，规则表明显没被带进来", len(list))
	}
	seen := map[string]bool{}
	for _, ev := range list {
		// Name 是下拉里的那行字。空着等于界面露出码值，
		// 和 Label 一样等于下拉里每条都是整句、把选项框撑成一段话。
		if ev.Code == "" || ev.Name == "" || ev.Label == "" {
			t.Fatalf("目录项不完整: %+v", ev)
		}
		if ev.Name == ev.Code {
			t.Errorf("事件 %s 的短名就是码值，用户会在下拉里看到英文", ev.Code)
		}
		if seen[ev.Code] {
			t.Errorf("事件码 %s 出现两次，界面会有两个一样的选项", ev.Code)
		}
		seen[ev.Code] = true
	}
	// plain 是"没命中任何规则"的意思，它必须由接口单独列出、不能混在目录里，
	// 否则用户会以为存在一种叫 plain 的引擎事件。
	if seen[EventPlain] {
		t.Error("plain 不该出现在事件目录里")
	}
	for _, code := range []string{EventSave, EventPlayerJoin, EventWorldgenFail, EventAccountFail, EventChat, EventAnnounce} {
		if !seen[code] {
			t.Errorf("目录里缺 %s", code)
		}
	}
}

// 一条记录必须同时带短名和整句：短名是行前缀，整句是选中事件时的说明。
// 只给其中一个，界面就只能要么每行一句长篇、要么说清规则在讲什么。
func TestParseLineCarriesShortNameAndFullLabel(t *testing.T) {
	e, _ := ParseLine(KindServer, 0, "[00:06:01]: Serializing world:")
	if e.Event != EventSave {
		t.Fatalf("event = %q，期望 %q", e.Event, EventSave)
	}
	if e.EventName != "存档落盘" {
		t.Errorf("eventName = %q，期望短名 存档落盘", e.EventName)
	}
	if e.EventLabel != "引擎把当前世界状态写进存档文件" {
		t.Errorf("eventLabel = %q，期望整句说明", e.EventLabel)
	}

	// 按方括号标签定性的行共用 EventPlain，短名要能分清是错误还是警告。
	errRow, _ := ParseLine(KindServer, 0, "[00:00:02]: [Error] something odd")
	if errRow.EventName != "引擎错误" || errRow.EventLabel != "引擎把它记成了错误" {
		t.Errorf("[Error] 行得到 %q / %q", errRow.EventName, errRow.EventLabel)
	}
	warnRow, _ := ParseLine(KindServer, 0, "[00:00:02]: [WARNING] something odd")
	if warnRow.EventName != "引擎警告" || warnRow.EventLabel != "引擎把它记成了警告" {
		t.Errorf("[WARNING] 行得到 %q / %q", warnRow.EventName, warnRow.EventLabel)
	}

	// 完全没归类的行不能凭空造一个名字：界面靠 eventName 是否为空决定要不要挂前缀。
	plain, _ := ParseLine(KindServer, 0, "[00:00:02]: 一段谁也不知道是什么的原文")
	if plain.Event != EventPlain || plain.EventName != "" || plain.EventLabel != "" {
		t.Errorf("未归类行得到 %q / %q / %q", plain.Event, plain.EventName, plain.EventLabel)
	}
}

func TestParseKind(t *testing.T) {
	for _, raw := range []string{"server", "Server", " chat "} {
		if _, ok := ParseKind(raw); !ok {
			t.Errorf("%q 该被认出来", raw)
		}
	}
	for _, raw := range []string{"", "chatlog", "SERVERS"} {
		if _, ok := ParseKind(raw); ok {
			t.Errorf("%q 不该被认出来", raw)
		}
	}
}

// 规则表里的每个 match 都必须真的能命中一行。
// 这条是自省：靠读代码看不出"某条规则的原文其实写错了"，
// 而写错的后果是那一类事件永远不出现在筛选结果里，没人会发现。
func TestEveryRuleMatchIsReachable(t *testing.T) {
	for _, rl := range rules {
		if rl.match == "" {
			t.Errorf("规则 %+v 没有匹配文本，等于死代码", rl)
			continue
		}
		line := fmt.Sprintf("[00:00:01]: %s", rl.match)
		e, ok := ParseLine(KindServer, 0, line)
		if !ok {
			t.Errorf("规则 %q 构造出的行居然解析不了", rl.match)
			continue
		}
		if e.Event != rl.event && e.Event != EventPlain {
			t.Errorf("规则 %q 被前面的规则抢了：得到 %s，期望 %s", rl.match, e.Event, rl.event)
		}
	}
}
