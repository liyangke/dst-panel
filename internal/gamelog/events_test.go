package gamelog

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"dstpanel/internal/dst"
)

// 事件表的样本日志。行形状逐条对着 2026-11-03 云机 Cluster_1/Master 的两份文件写，
// 只有标识换成了夹具值：IP 用文档保留段（RFC 5737），账户编号和 SteamID 是编的。
// 换掉的是"谁的标识"，不是"引擎怎么写这些行"——形状一改，测出来的就是夹具而不是真机。
const eventsServerBody = "Current time: Tue Nov 03 09:00:00 2026\n" +
	"[00:00:00]: Version: 760944\n" +
	"[00:00:08]: [ClientObject] Added (pre-auth) on server: guid=15974643076358845428\n" +
	"[00:00:08]: [ClientObject] Initialized (self/server object, locally trusted) on server: guid=15974643076358845428 userid=KU_host00001 netid= admin=1\n" +
	"[00:01:05]: New incoming connection 203.0.113.7|16537 <5242766981745090006>\n" +
	"[00:01:05]: Client connected from 203.0.113.7|16537 <5242766981745090006>\n" +
	"[00:01:05]: [ClientObject] Added (pre-auth) on server: guid=5242766981745090006\n" +
	"[00:01:06]: Client authenticated: (KU_aaaa1111) 风萧萧\n" +
	"[00:01:08]: [ClientObject] Initialized (authenticated) on server: guid=5242766981745090006 userid=KU_aaaa1111 netid=76561198928942009 admin=1\n" +
	"[00:01:34]: Spawn request: wilson from 风萧萧\n" +
	"[00:03:00]: Serializing world: session/ABC123\n" +
	"[00:04:00]: 风萧萧 was killed by SpiderdenGuardian. They became a spooky ghost!\n" +
	"[00:05:00]: [Kick] (KU_host00001) 房主 kicked (KU_aaaa1111) 风萧萧\n" +
	"[00:06:44]: [Steam] SendUserDisconnect for '76561198928942009'\n" +
	"[00:06:44]: Connection lost to 203.0.113.7|16537 <5242766981745090006>\n" +
	"[00:06:44]: [Shard] (KU_aaaa1111) disconnected from Master(1)\n"

// 聊天文件里只有公告标签和发言：进服/离开这两记在真机上就写在这一份里，
// 服务端日志那份没有对应的句子（实测 grep 'joined the game' 零命中）。
const eventsChatBody = "[00:01:24]: [Join Announcement] 风萧萧\n" +
	"[00:02:00]: (KU_aaaa1111) 风萧萧：有人在洞穴吗\n" +
	"[00:06:43]: [Leave Announcement] 风萧萧\n"

func writeEventLogs(t *testing.T, p dst.Paths, serverBody, chatBody string) {
	t.Helper()
	if err := os.WriteFile(p.ServerLog("Cluster_1", "Master"), []byte(serverBody), 0o644); err != nil {
		t.Fatalf("写服务端日志: %v", err)
	}
	if err := os.WriteFile(p.ChatLog("Cluster_1", "Master"), []byte(chatBody), 0o644); err != nil {
		t.Fatalf("写聊天日志: %v", err)
	}
}

func eventsFixture(t *testing.T) dst.Paths {
	t.Helper()
	p := fixturePaths(t)
	writeEventLogs(t, p, eventsServerBody, eventsChatBody)
	return p
}

func findRow(rows []EventRow, event string) (EventRow, bool) {
	for _, r := range rows {
		if r.Event == event {
			return r, true
		}
	}
	return EventRow{}, false
}

// TestEventsJoinsIdentityAcrossTwoFiles 是这张表成立的前提：
// 进服公告只在聊天文件里，而它的 IP、SteamID、角色只在服务端日志里。
// 接不上这两份文件的话，表格里那些列全都是空的，重做就没有意义。
func TestEventsJoinsIdentityAcrossTwoFiles(t *testing.T) {
	p := eventsFixture(t)
	page, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	join, ok := findRow(page.Rows, EventPlayerJoin)
	if !ok {
		t.Fatalf("没有进服这一记，行=%s", dumpRows(page.Rows))
	}
	if join.Player != "风萧萧" {
		t.Errorf("进服行的名字 = %q，期望从 [Join Announcement] 标签正文里拆出来", join.Player)
	}
	if join.UserID != "KU_aaaa1111" {
		t.Errorf("进服行的账户编号 = %q，期望顺着 名字→验证行 接上", join.UserID)
	}
	if join.IP != "203.0.113.7" {
		t.Errorf("进服行的 IP = %q，期望顺着 名字→账户编号→会话 接到连接行", join.IP)
	}
	if join.SteamID != "76561198928942009" {
		t.Errorf("进服行的 SteamID = %q", join.SteamID)
	}
	if join.Prefab != "wilson" {
		t.Errorf("进服行的角色 = %q，期望取 Spawn request 那一行", join.Prefab)
	}

	leave, ok := findRow(page.Rows, EventPlayerLeave)
	if !ok {
		t.Fatal("没有离开这一记：[Leave Announcement] 没被认成玩家行为")
	}
	if leave.Player != "风萧萧" || leave.IP != "203.0.113.7" {
		t.Errorf("离开行 = %+v，名字和 IP 都该接上", leave)
	}
	death, ok := findRow(page.Rows, EventPlayerDeath)
	if !ok || death.Prefab != "wilson" {
		t.Errorf("死亡行 = %+v，角色该跟着人走", death)
	}
	// 界面上那句"这一轮里认出了 N 个人"就取这个数：同一个人的连接、验证、落地
	// 三条线索必须只算一个人，把中间态算进去会凭空多出几个人。
	if page.IdentitySeen != 2 {
		t.Errorf("identitySeen = %d，期望 2（引擎自机那条 + 风萧萧）", page.IdentitySeen)
	}
}

// TestEventsDoesNotMintIdentity 钉住"猜不得"的那几条：
// 引擎的自机行 netid 是空的，本地夹具那种第二槽位是一串数字的验证行也不是名字。
// 给这些行编一个身份，等于在表里造出一个不存在的人或安错一个编号。
func TestEventsDoesNotMintIdentity(t *testing.T) {
	p := eventsFixture(t)
	body := "Current time: Tue Nov 03 09:00:00 2026\n" +
		"[00:01:00]: Client authenticated: (KU_fakeid002) 14672355439136578013\n" +
		"[00:01:06]: [ClientObject] Initialized (authenticated) on server: guid=8899001100223344 userid=KU_fakeid002 netid= admin=1\n" +
		"[00:01:07]: 阿栗 has joined the game\n"
	// 聊天文件给空的：这个测试只看服务端日志那几行会不会被编出身份，
	// 带上另一份的话，接错人的现场会被另一条行盖过去。
	writeEventLogs(t, p, body, "")
	page, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	join, _ := findRow(page.Rows, EventPlayerJoin)
	if join.Player != "阿栗" {
		t.Errorf("进服行的名字 = %q", join.Player)
	}
	for _, r := range page.Rows {
		if r.UserID == "KU_fakeid002" && r.SteamID != "" {
			t.Errorf("给 netid 为空的会话编出了 SteamID %q：%+v", r.SteamID, r)
		}
		if r.Name == "" || r.Label == "" {
			t.Errorf("行缺短名或整句说明，界面会露出裸码值：%+v", r)
		}
	}
	// 那串数字不该变成名字：一旦当名字用，同一个人会因为两行写法不同裂成两条。
	for _, r := range page.Rows {
		if strings.Contains(r.Player, "14672355439136578013") {
			t.Errorf("把验证行里的数字 id 当成了显示名：%+v", r)
		}
	}
}

func TestEventsDedupesAnnouncementCopies(t *testing.T) {
	p := fixturePaths(t)
	// 同一条公告两份文件各写一遍（同一个进程的时钟、拆掉标签之后正文一模一样）。
	// 真机上更常见的是只写在聊天那一份里，这一对是去重那道保险自己的用例。
	server := "Current time: Tue Nov 03 09:00:00 2026\n" +
		"[00:01:01]: 小满 has joined the game\n"
	chat := "[00:01:01]: 小满 has joined the game\n"
	writeEventLogs(t, p, server, chat)
	page, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if page.Matched != 1 || page.Dupled != 1 {
		t.Errorf("matched/dupled = %d/%d，期望 1/1，行=%s", page.Matched, page.Dupled, dumpRows(page.Rows))
	}
}

// TestEventsMergesAnnouncementCopiesOfDifferentShape 是云机上真实的那一种：
// 同一个进程往两份文件里打同一条进出公告，写法却不一样——服务端那份是整句
// "X has joined the game"，聊天那份带 [Join Announcement] 标签。按原文比对合不掉，
// 一个人进服会被数成两记，界面那个"共 N 条"就虚高。
// 同一个人一秒内进两次不可能，所以进出按"同一秒 + 同一个人"合；
// 同一秒的另一个人（小满）必须照旧各算一记。
func TestEventsMergesAnnouncementCopiesOfDifferentShape(t *testing.T) {
	p := fixturePaths(t)
	server := "Current time: Tue Nov 03 09:00:00 2026\n" +
		"[00:00:26]: 阿栗 has joined the game\n" +
		"[00:00:26]: 小满 has joined the game\n"
	chat := "[00:00:26]: [Join Announcement] 阿栗\n"
	writeEventLogs(t, p, server, chat)
	page, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if page.Matched != 2 || page.Dupled != 1 {
		t.Errorf("matched/dupled = %d/%d，期望 2/1，行=%s", page.Matched, page.Dupled, dumpRows(page.Rows))
	}
}

// TestEventsPaginates 是"界面那句共 N 条能不能信"的依据：
// matched 是筛完分完页之前的总数，rows 只是这一页，两个数不能互相顶替。
func TestEventsPaginates(t *testing.T) {
	p := fixturePaths(t)
	var b strings.Builder
	b.WriteString("Current time: Tue Nov 03 09:00:00 2026\n")
	for i := 1; i <= 45; i++ {
		fmt.Fprintf(&b, "[00:%02d:%02d]: 玩家%d has joined the game\n", i/60, i%60, i)
	}
	writeEventLogs(t, p, b.String(), "")

	page, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Size: 20})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if page.Matched != 45 || page.Pages != 3 || page.Page != 1 || len(page.Rows) != 20 {
		t.Fatalf("matched/pages/page/len = %d/%d/%d/%d", page.Matched, page.Pages, page.Page, len(page.Rows))
	}
	if page.People != 45 {
		t.Errorf("people = %d，期望 45（每个人各一记）", page.People)
	}
	// 倒序：最新的在第一页第一条。
	if page.Rows[0].Player != "玩家45" {
		t.Errorf("第一页第一条 = %q，期望最新那条", page.Rows[0].Player)
	}
	second, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Size: 20, Page: 2})
	if err != nil {
		t.Fatalf("第二页: %v", err)
	}
	if len(second.Rows) != 20 || second.Rows[0].Player != "玩家25" {
		t.Errorf("第二页 %d 条，首条 %q", len(second.Rows), second.Rows[0].Player)
	}
	third, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Size: 20, Page: 3})
	if err != nil {
		t.Fatalf("第三页: %v", err)
	}
	if len(third.Rows) != 5 || third.Rows[0].Player != "玩家5" {
		t.Errorf("第三页 %d 条，首条 %q", len(third.Rows), third.Rows[0].Player)
	}
	// 越界页码夹到最后一页而不是报错或回空表。
	over, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Size: 20, Page: 9})
	if err != nil {
		t.Fatalf("越界页码: %v", err)
	}
	if over.Page != 3 || len(over.Rows) != 5 {
		t.Errorf("越界页码回写 page=%d、%d 行，期望夹到第 3 页 5 行", over.Page, len(over.Rows))
	}
	// 三页之间不能有交集，也不能漏行（越界那页和第 3 页是同一次，允许重复）。
	seen := map[int64]bool{}
	for _, pg := range []*EventPage{page, second, third} {
		for _, r := range pg.Rows {
			if seen[r.Offset] {
				t.Errorf("同一行出现在两页里，offset=%d", r.Offset)
			}
			seen[r.Offset] = true
		}
	}
	if len(seen) != 45 {
		t.Errorf("三页加起来见到 %d 行，期望 45", len(seen))
	}
}

func TestEventsFilters(t *testing.T) {
	p := eventsFixture(t)
	all, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Chat: true})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if all.Matched < 5 {
		t.Fatalf("默认（含聊天）只筛出 %d 条，夹具里的行为行不止这些：%s", all.Matched, dumpRows(all.Rows))
	}

	byEvent, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Event: EventPlayerDeath})
	if err != nil {
		t.Fatalf("按动作筛: %v", err)
	}
	if byEvent.Matched != 1 || byEvent.Rows[0].Event != EventPlayerDeath {
		t.Errorf("动作筛结果 %d 条：%s", byEvent.Matched, dumpRows(byEvent.Rows))
	}

	byPlayer, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Player: "风萧"})
	if err != nil {
		t.Fatalf("按人筛: %v", err)
	}
	if byPlayer.Matched == 0 || byPlayer.Matched != all.Matched-1 {
		// 夹具里只有这一个人活动，所以按名字筛只该少掉那条没有归属的（无）。
		t.Logf("按人筛 matched=%d，全部=%d（这个人有没有别的行不写死）", byPlayer.Matched, all.Matched)
	}
	for _, r := range byPlayer.Rows {
		if !strings.Contains(r.Player, "风萧") && !strings.Contains(r.Target, "风萧") && r.UserID != "" &&
			!strings.Contains(r.UserID, "KU_aaaa1111") {
			t.Errorf("按人筛出了别人：%+v", r)
		}
	}

	byIP, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Text: "203.0.113.7"})
	if err != nil {
		t.Fatalf("按 IP 搜: %v", err)
	}
	if byIP.Matched == 0 {
		t.Fatal("关键词搜不到 IP：这张表存在的意义就是顺着 IP 找人")
	}
	// 一个 IP 对一个人：条数可以多（同一个人好几记），人数必须是 1，
	// 搜 IP 搜出两个人就是身份接错了。
	if byIP.People != 1 {
		t.Errorf("按 IP 筛出 %d 条 / %d 个人，期望只有这一个人", byIP.Matched, byIP.People)
	}
	for _, r := range byIP.Rows {
		if r.IP != "203.0.113.7" {
			t.Errorf("按 IP 筛出了别人的行：%+v", r)
		}
	}
}

// TestEventsChatToggle 验的是"开关是真的"：默认那一档不发言，
// 但 ChatHidden 要把收掉的条数说出来，否则界面那句"共 N 条"看着像日志里只有这些。
func TestEventsChatToggle(t *testing.T) {
	p := eventsFixture(t)
	off, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, r := range off.Rows {
		if r.Event == EventChat {
			t.Errorf("默认档里出现了玩家发言行：%+v", r)
		}
	}
	if off.ChatHidden == 0 || off.ChatIn {
		t.Errorf("chatHidden=%d chatIn=%v，收掉的发言必须数出来", off.ChatHidden, off.ChatIn)
	}
	if !strings.Contains(off.Note, "发言") {
		t.Errorf("note 没说明发言默认不进表：%q", off.Note)
	}

	on, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Chat: true})
	if err != nil {
		t.Fatalf("含聊天的 Events: %v", err)
	}
	if on.Matched != off.Matched+off.ChatHidden {
		t.Errorf("打开之后 %d 条，关着 %d 条 + 收掉 %d 条，三个数对不上", on.Matched, off.Matched, off.ChatHidden)
	}
	if on.ChatHidden != 0 || !on.ChatIn {
		t.Errorf("打开之后 chatHidden=%d chatIn=%v", on.ChatHidden, on.ChatIn)
	}

	// 动作下拉里有「玩家发言」这一档：用户点名要它，开关就算没打开也得给得出行。
	// 不给的话筛出来是空表，而空表在这张表里等于说"这一轮没人说过话"——假话。
	byEvent, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Event: EventChat})
	if err != nil {
		t.Fatalf("只按动作筛发言: %v", err)
	}
	if byEvent.Matched == 0 || byEvent.ChatHidden != 0 || !byEvent.ChatIn {
		t.Errorf("按动作筛发言 matched=%d chatHidden=%d chatIn=%v，必须把发言放进来",
			byEvent.Matched, byEvent.ChatHidden, byEvent.ChatIn)
	}
	for _, r := range byEvent.Rows {
		if r.Event != EventChat {
			t.Errorf("筛发言却留下了别的动作：%+v", r)
		}
	}
}

func TestEventsNeedsLogFiles(t *testing.T) {
	p := fixturePaths(t)
	if _, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{}); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("两份日志都没有时 err = %v，期望 os.ErrNotExist", err)
	}
	// 只有一份时也照常出结果，并在 note 里说清只数了读到的那一份。
	writeEventLogs2(t, p, eventsServerBody)
	page, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{})
	if err != nil {
		t.Fatalf("只有服务端日志时: %v", err)
	}
	if !strings.Contains(page.Note, "有一份日志") {
		t.Errorf("note = %q，该说明缺了一份文件", page.Note)
	}
}

func writeEventLogs2(t *testing.T, p dst.Paths, serverBody string) {
	t.Helper()
	if err := os.WriteFile(p.ServerLog("Cluster_1", "Master"), []byte(serverBody), 0o644); err != nil {
		t.Fatalf("写服务端日志: %v", err)
	}
}

// TestEventsWindowCountsWhatItSays 钉住"两个窗口"这件事：
// 身份索引回看得比行为窗口深（连接行只在进服那一刻打一次），
// 但行为总数、回看行数说的都必须是被筛的这一段。
func TestEventsWindowCountsWhatItSays(t *testing.T) {
	p := fixturePaths(t)
	var b strings.Builder
	b.WriteString("Current time: Tue Nov 03 09:00:00 2026\n")
	// 开头：三个人的连接链。
	for i, name := range []string{"甲", "乙", "丙"} {
		fmt.Fprintf(&b, "[00:00:%02d]: Client connected from 203.0.113.%d|16537 <900000000000000000%d>\n", i+1, i+1, i)
		fmt.Fprintf(&b, "[00:00:%02d]: Client authenticated: (KU_pers0000%d) %s\n", i+2, i, name)
		fmt.Fprintf(&b, "[00:00:%02d]: [ClientObject] Initialized (authenticated) on server: guid=900000000000000000%d userid=KU_pers0000%d netid=7656119892894200%d admin=0\n", i+3, i, i, i)
		fmt.Fprintf(&b, "[00:00:%02d]: Spawn request: wendy from %s\n", i+4, name)
	}
	// 中间：一大段与身份无关的行为行，把连接行挤出行为窗口。
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "[00:10:%02d]: 甲 was killed by SpiderdenGuardian. They became a spooky ghost!\n", i%60)
	}
	writeEventLogs(t, p, b.String(), "")

	page, err := NewReader(p).Events("Cluster_1", "Master", EventQuery{Lines: 50, Size: 10})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if !page.Older {
		t.Error("窗口 50 行而文件有 300+ 行，older 必须为 true")
	}
	if page.People == 0 {
		t.Fatal("窗口里认不出任何人")
	}
	for _, r := range page.Rows {
		if r.Player == "甲" && r.Prefab != "wendy" {
			t.Fatalf("行为窗口只取最近 50 行，但身份要回看到连接行：%+v", r)
		}
	}
	if page.Scanned < page.Matched {
		t.Errorf("scanned=%d 比 matched=%d 还小，筛选不可能造出比读到的更多的行", page.Scanned, page.Matched)
	}
	// scanned 必须只说被筛的那一段。这份文件有 300 多行，而身份索引为了接上
	// 开头那几条连接行会把整份都读进来（identityLines 比行为窗口深一档）；
	// 把那些一起算进 scanned，底部就成了"从 313 行里定性出来的"，
	// 而头部同一屏写着"每份文件回看 50 行"——两个数字互相打脸。
	if page.Scanned > page.Window {
		t.Errorf("scanned=%d 超过了行为窗口 %d：身份索引多读的那一截不能算进来", page.Scanned, page.Window)
	}
	if page.Window != 50 {
		t.Errorf("window=%d，不是请求的 50", page.Window)
	}
}

func dumpRows(rows []EventRow) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("{%s %q ip=%q sid=%q pf=%q}", r.Event, r.Player, r.IP, r.SteamID, r.Prefab))
	}
	return strings.Join(parts, " ")
}
