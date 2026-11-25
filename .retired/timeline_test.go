package gamelog

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/dst"
)

// 一轮真实会话的最小样本：服务端日志给引擎公告，聊天日志给发言和公告抄本。
// 句子部分全是从引擎自带文案表里抄的形状（strings.lua 第 10753~10765 行，
// 2026-11-03 在云机逐条核对），不是编出来的。
const timelineServerLog = "[00:00:00]: Version: 760944\n" +
	"[00:01:00]: Client authenticated: (KU_1a2b3c4d) 14672355439136578013\n" +
	"[00:01:01]: 小满 has joined the game\n" +
	"[00:02:00]: 小满 was killed by SpiderdenGuardian. They became a spooky ghost!\n" +
	"[00:03:00]: 阿黎 lost all of their humanity and is gone forever.\n" +
	"[00:04:00]: 小满 went away for a bit.\n" +
	"[00:05:00]: 小满 has returned from their relaxing time away.\n" +
	"[00:06:00]: [Kick] (KU_9z8y7x6w) 房主 kicked (KU_1a2b3c4d) 小满\n" +
	"[00:07:00]: 小满 has left the game\n"

const timelineChatLog = "[00:01:01]: [Announcement] 小满 has joined the game\n" +
	"[00:02:30]: (KU_1a2b3c4d) 小满: 谁来帮忙打龙\n" +
	"[00:02:31]: (KU_5e6f7g8h) 阿黎: 马上到\n" +
	"[00:07:00]: [Announcement] 小满 has left the game\n"

func timelineFixture(t *testing.T) *Reader {
	t.Helper()
	p := fixturePaths(t)
	writeLog(t, p, KindServer, timelineServerLog)
	writeLog(t, p, KindChat, timelineChatLog)
	return NewReader(p)
}

func playerByName(tl *Timeline, name string) (*TimelinePlayer, bool) {
	for i := range tl.Players {
		if tl.Players[i].Key == name {
			return &tl.Players[i], true
		}
	}
	return nil, false
}

// rawsOf 把一截明细的原文拼成一行，方便按"哪几句还在列表里"来判。
func rawsOf(events []TimelineEvent) string {
	var sb strings.Builder
	for _, e := range events {
		sb.WriteString(e.Raw)
		sb.WriteString("\n")
	}
	return sb.String()
}

func countOf(p *TimelinePlayer, code string) int {
	for _, c := range p.Counts {
		if c.Code == code {
			return c.N
		}
	}
	return -1
}

// 这是这张表最核心的一条：一次进服在日志里是两行（验证行 + 公告行），
// 把它们算成同一种行为的话，时间线上就是"进服 2 次"，而界面上的数字必须是真数过的。
func TestTimelineMergesBothLogsPerPlayer(t *testing.T) {
	tl, err := timelineFixture(t).Timeline("Cluster_1", dst.ShardMaster, 0)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.Window != TimelineDefaultLines || tl.Cluster != "Cluster_1" || tl.Shard != dst.ShardMaster {
		t.Errorf("回看窗口/归属 = %d %q/%q", tl.Window, tl.Cluster, tl.Shard)
	}
	if tl.Note != "" {
		t.Errorf("两份文件都读到了还给了提示: %q", tl.Note)
	}
	if len(tl.Players) != 3 {
		var keys []string
		for _, p := range tl.Players {
			keys = append(keys, p.Key)
		}
		t.Fatalf("归出 %d 个人（%v），期望 3 个：小满、房主、阿黎", len(tl.Players), keys)
	}

	xm, ok := playerByName(tl, "小满")
	if !ok {
		t.Fatal("时间线里找不到小满")
	}
	// 验证行只有账户编号、公告行只有名字，靠聊天行把两个身份接起来。
	// 接不上就会多出一个标题是 KU_1a2b3c4d 的"人"。
	if _, dup := playerByName(tl, "KU_1a2b3c4d"); dup {
		t.Error("同一个人因为一条只有编号、一条只有名字而裂成了两条")
	}
	if xm.UserID != "KU_1a2b3c4d" {
		t.Errorf("小满的 userid = %q，期望从聊天行里带出来", xm.UserID)
	}
	for _, tc := range []struct {
		code string
		n    int
	}{
		{EventAuthOK, 1}, {EventPlayerJoin, 1}, {EventPlayerDeath, 1},
		{EventAFKStart, 1}, {EventAFKEnd, 1},
		{EventPlayerLeave, 1}, {EventChat, 1},
	} {
		if got := countOf(xm, tc.code); got != tc.n {
			t.Errorf("小满的 %s 计 %d 次，期望 %d（全部计数 %+v）", tc.code, got, tc.n, xm.Counts)
		}
	}
	// 幽灵消散那行说的是阿黎，不该出现在小满的时间线里。
	if countOf(xm, EventGhostGone) != -1 {
		t.Errorf("小满的时间线里出现了幽灵消散: %+v", xm.Counts)
	}
	// 踢人这一记在动手的人名下（Player 和 UserID 是一对，被踢的在 Target 里）。
	// 记在被踢的人名下会得出"小满踢了小满"，而 UserID 还是管理员的。
	if countOf(xm, EventKick) != -1 {
		t.Errorf("小满的时间线里出现了踢人计数: %+v", xm.Counts)
	}

	// 聊天文件里的公告抄本不能当第二下：引擎给每条公告都挂 [Announcement]，
	// 定性时它先认成"公告"，所以根本不进行为表；计数全部来自服务端日志那一份。
	lm, ok := playerByName(tl, "房主")
	if !ok {
		t.Fatal("动手踢人的那位也得单独成一条")
	}
	if countOf(lm, EventKick) != 1 {
		t.Errorf("房主的踢人计数 = %+v", lm.Counts)
	}
	kick, ok := findEvent(lm, EventKick)
	if !ok {
		t.Fatal("房主名下没有踢人记录")
	}
	// 被踢的人单独放在 target：Player 和 UserID 是一对（这笔账算在谁头上），
	// 把被踢的写进 Player 会让编号是管理员的、名字是被踢的。
	if kick.Target != "小满" {
		t.Errorf("kick 的 target = %q，期望 小满（原文 %q）", kick.Target, kick.Raw)
	}
	if kick.UserID != "KU_9z8y7x6w" {
		t.Errorf("kick 的 userid = %q，期望动手那位的编号", kick.UserID)
	}

	ali, ok := playerByName(tl, "阿黎")
	if !ok {
		t.Fatal("阿黎该有两个来源：聊天发言 + 服务端日志的幽灵公告")
	}
	if countOf(ali, EventGhostGone) != 1 || countOf(ali, EventChat) != 1 {
		t.Errorf("阿黎的计数 = %+v", ali.Counts)
	}
}

func findEvent(p *TimelinePlayer, code string) (TimelineEvent, bool) {
	for _, e := range p.Events {
		if e.Event == code {
			return e, true
		}
	}
	return TimelineEvent{}, false
}

// 组内按引擎时钟升序，人与人之间按最后一次行为倒序。
// 顺序错了的话，界面读出来就是"他先离开、后来才死"这种荒唐话。
func TestTimelineOrdersEventsAndPeople(t *testing.T) {
	tl, err := timelineFixture(t).Timeline("Cluster_1", dst.ShardMaster, 0)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	xm, ok := playerByName(tl, "小满")
	if !ok {
		t.Fatal("找不到小满")
	}
	var clocks []string
	for _, e := range xm.Events {
		clocks = append(clocks, e.Clock)
	}
	want := []string{"00:01:00", "00:01:01", "00:02:00", "00:02:30", "00:04:00", "00:05:00", "00:07:00"}
	if strings.Join(clocks, " ") != strings.Join(want, " ") {
		t.Errorf("小满的时钟顺序 = %v，期望 %v", clocks, want)
	}
	if xm.LastClock != "00:07:00" {
		t.Errorf("LastClock = %q，期望最后一次行为的时钟", xm.LastClock)
	}

	var order []string
	for _, p := range tl.Players {
		order = append(order, p.Key)
	}
	// 小满最后动到 00:07，房主 00:06，阿黎 00:03。
	if strings.Join(order, ",") != "小满,房主,阿黎" {
		t.Errorf("人员顺序 = %v，期望最近活跃的在前", order)
	}
}

// 零次的不出现，出现的按固定顺序排：一排"死亡 0 · 复活 0"会把真发生过的事挤没。
func TestTimelineCountsSkipZerosAndFollowOrder(t *testing.T) {
	tl, err := timelineFixture(t).Timeline("Cluster_1", dst.ShardMaster, 0)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	ali, ok := playerByName(tl, "阿黎")
	if !ok {
		t.Fatal("找不到阿黎")
	}
	var codes []string
	for _, c := range ali.Counts {
		if c.N == 0 {
			t.Errorf("%s 零次还进了计数", c.Code)
		}
		if c.Name == "" || c.Name == c.Code {
			t.Errorf("%s 的中文名没给（%q），界面会露出码值", c.Code, c.Name)
		}
		switch c.Tone {
		case "", "warn", "bad", "ok":
		default:
			t.Errorf("%s 的色档 = %q，前端只认这四档", c.Code, c.Tone)
		}
		codes = append(codes, c.Code)
	}
	if strings.Join(codes, ",") != EventGhostGone+","+EventChat {
		t.Errorf("阿黎的计数 = %v，期望只有幽灵消散和发言两项、且按 behaviourOrder 排", codes)
	}
	for _, e := range ali.Events {
		// 每条记录自带短名：前端再摆一张码值表迟早和规则表漂移成两种说法。
		if e.Name == "" || e.Name == e.Event {
			t.Errorf("%s 的短名 = %q", e.Event, e.Name)
		}
		if e.KindLabel == "" {
			t.Errorf("%s 的日志种类中文名没给", e.Event)
		}
	}
}

// 两份文件里同一时钟、同一正文的那一条不会被数两次。
// 主防线其实是聊天公告先定性成"公告"；这条钉的是兜底那一路：
// 真的出现两份同钟同文的抄本时，多吃掉一下并把 Dupled 记出来，而不是静默。
func TestTimelineDropsDuplicateCopy(t *testing.T) {
	// 聊天文件里那条抄本有两种可能的形状：不带标签的原句，以及引擎二进制里的模板
	// 形状 `[Join Announcement] %s`（2026-11-03 在服务端 strings 里核对到模板，真机上
	// 聊天文件到底打哪一种还没抓到样本）。两种都得合成一记，标签拆走之后正文一模一样。
	for _, chatLine := range []string{
		"[00:01:01]: 小满 has joined the game\n",
		"[00:01:01]: [Join Announcement] 小满 has joined the game\n",
	} {
		p := fixturePaths(t)
		writeLog(t, p, KindServer, "[00:01:01]: 小满 has joined the game\n")
		writeLog(t, p, KindChat, chatLine)

		tl, err := NewReader(p).Timeline("Cluster_1", dst.ShardMaster, 0)
		if err != nil {
			t.Fatalf("Timeline: %v", err)
		}
		xm, ok := playerByName(tl, "小满")
		if !ok {
			t.Fatalf("没归出人: %+v", tl.Players)
		}
		if got := countOf(xm, EventPlayerJoin); got != 1 {
			t.Errorf("抄本 %q 计成了 %d 次，期望 1", chatLine, got)
		}
		if tl.Dupled != 1 {
			t.Errorf("抄本 %q：Dupled = %d，期望 1——吃掉的那一下要能被界面说出来", chatLine, tl.Dupled)
		}
	}
}

// 墙上时间只有服务端日志里那行 Current time 给得出来，聊天文件根本没这一行。
// 不借过来补上的话，同一个人的时间线里就是两种写法挨着（22:59:10 旁边一个 [00:01:00]），
// 而"谁先谁后"正是这张表要回答的第一件事。
func TestTimelineBorrowsRoundStartForChatRows(t *testing.T) {
	p := fixturePaths(t)
	start := time.Date(2026, 11, 3, 2, 17, 35, 0, time.Local)
	writeLog(t, p, KindServer,
		fmt.Sprintf("[00:00:00]: Current time: %s\n", start.Format(time.ANSIC))+timelineServerLog)
	writeLog(t, p, KindChat, timelineChatLog)

	tl, err := NewReader(p).Timeline("Cluster_1", dst.ShardMaster, 0)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	xm, ok := playerByName(tl, "小满")
	if !ok {
		t.Fatalf("没归出人: %+v", tl.Players)
	}
	want := map[string]string{
		// 服务端日志里的验证行：本轮开始后 1 分钟。
		"00:01:00": start.Add(time.Minute).UTC().Format(time.RFC3339),
		// 聊天日志里的那句发言：开始后 2 分 30 秒，这份文件自己没有时间锚点。
		"00:02:30": start.Add(150 * time.Second).UTC().Format(time.RFC3339),
	}
	var seen map[string]string
	for _, ev := range xm.Events {
		if _, ok := want[ev.Clock]; ok {
			if seen == nil {
				seen = map[string]string{}
			}
			seen[ev.Clock] = ev.At
		}
	}
	for clock, at := range want {
		if got, ok := seen[clock]; !ok {
			t.Errorf("小满的时间线里没有 %s 这一行", clock)
		} else if got != at {
			t.Errorf("%s 那行的墙上时间 = %q，期望 %q", clock, got, at)
		}
	}
	for _, ev := range xm.Events {
		if ev.At == "" {
			t.Errorf("%s 那行没有墙上时间，界面上就会跟其它行用两种写法", ev.Clock)
		}
	}
}

// 认不出是谁的行为行不进任何人的时间线，但必须记进 NoOwner：
// 静默丢掉的话，界面上"共 N 下"就和原文对不上了。
func TestTimelineKeepsScoreOfUnattributedRows(t *testing.T) {
	p := fixturePaths(t)
	writeLog(t, p, KindServer, "[00:01:01]: 小满 has joined the game\n")
	// 聊天文件里一句既没括号编号也没冒号的话：拆不出名字，也不该硬造一个。
	writeLog(t, p, KindChat, "[00:02:00]: 这句话谁都没署名\n")

	tl, err := NewReader(p).Timeline("Cluster_1", dst.ShardMaster, 0)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.NoOwner != 1 {
		t.Errorf("NoOwner = %d，期望 1", tl.NoOwner)
	}
	if tl.Kept != 1 {
		t.Errorf("Kept = %d，期望只有小满那一下", tl.Kept)
	}
	if tl.Scanned < tl.Kept+tl.NoOwner {
		t.Errorf("账对不上：扫了 %d 行，却归出 %d + 无主 %d", tl.Scanned, tl.Kept, tl.NoOwner)
	}
	for _, pl := range tl.Players {
		if pl.Key == "" {
			t.Error("不能有一个没有名字的人挂在列表里")
		}
	}
}

// 只回看了窗口以内的行，这件事必须说出来：
// 界面报的数字是"最近 N 行里"的，不是这一轮的累计。
func TestTimelineNotesWindowAndMissingFile(t *testing.T) {
	tl, err := timelineFixture(t).Timeline("Cluster_1", dst.ShardMaster, 3)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if !tl.Older {
		t.Fatal("三份行的窗口该报 Older")
	}
	if !strings.Contains(tl.Note, "最近 3 行") {
		t.Errorf("Note = %q，该说清只回看了多少行", tl.Note)
	}

	// 只有一份文件的世界（引擎刚启动、聊天文件还没写出来）：照给结果，但要说实话。
	p := fixturePaths(t)
	writeLog(t, p, KindServer, "[00:01:01]: 小满 has joined the game\n")
	one, err := NewReader(p).Timeline("Cluster_1", dst.ShardMaster, 0)
	if err != nil {
		t.Fatalf("只有一份日志时不该报错: %v", err)
	}
	if !strings.Contains(one.Note, "有一份日志") {
		t.Errorf("Note = %q，缺文件时该说明", one.Note)
	}
	if len(one.Players) != 1 {
		t.Errorf("一份日志也归得出人: %+v", one.Players)
	}

	// 两份都没有 = 这个世界还没跑过，界面该说"还没有日志文件"，
	// 而不是给一张谁都没来过的空表（空表会让人以为房间坏了）。
	none := fixturePaths(t)
	if _, err := NewReader(none).Timeline("Cluster_1", dst.ShardMaster, 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("两份都缺必须保住 os.ErrNotExist 这条链，接口层靠它选 503：%v", err)
	}
}

// 一个人几百行时，折的是明细、不是数字：芯片上那个数照样按全量算，
// 列表只留最近 RowsPerPlayer 行，并把折掉多少行说清楚（eventsHidden）。
// 不折的话，一个话痨能占掉三屏，第二个人就看不见；折了不说的话，
// 这份列表会被读成"他就做了这么点事"。
func TestTimelineFoldsOldRowsButKeepsCounts(t *testing.T) {
	p := fixturePaths(t)
	start := time.Date(2026, 11, 3, 2, 17, 35, 0, time.Local)
	writeLog(t, p, KindServer,
		fmt.Sprintf("[00:00:00]: Current time: %s\n", start.Format(time.ANSIC))+
			"[00:01:00]: Client authenticated: (KU_1a2b3c4d) 14672355439136578013\n")
	const total = RowsPerPlayer + 25
	var sb strings.Builder
	for i := 0; i < total; i++ {
		// 时钟逐行递增：去重键里有时钟，几十句一模一样的话会互相吃掉，那就测不到折叠了。
		fmt.Fprintf(&sb, "[00:%02d:%02d]: (KU_1a2b3c4d) 小满: 第 %d 句\n", i/60, i%60, i)
	}
	writeLog(t, p, KindChat, sb.String())

	tl, err := NewReader(p).Timeline("Cluster_1", dst.ShardMaster, 0)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	xm, ok := playerByName(tl, "小满")
	if !ok {
		t.Fatalf("没归出人: %+v", tl.Players)
	}
	if len(xm.Events) != RowsPerPlayer {
		t.Errorf("列出了 %d 行，期望折到 %d 行", len(xm.Events), RowsPerPlayer)
	}
	// 小满名下不止那几十句发言，服务端那条验证行也归他，所以折掉的是 发言+验证 的总数减一档。
	if hidden := total + 1 - RowsPerPlayer; xm.EventsHidden != hidden {
		t.Errorf("eventsHidden = %d，期望 %d", xm.EventsHidden, hidden)
	}
	if got := countOf(xm, EventChat); got != total {
		t.Errorf("发言计数 = %d，期望 %d：折明细不该动数字", got, total)
	}
	// 折掉的是更早的那些，留下的是最近这一段：第一句已经不在列表里，最后一句在。
	if strings.Contains(rawsOf(xm.Events), "第 0 句") {
		t.Errorf("最早那句还在列表里，折的是后头: %s", xm.Events[0].Raw)
	}
	if !strings.Contains(rawsOf(xm.Events), fmt.Sprintf("第 %d 句", total-1)) {
		t.Errorf("最近那句不在列表里: %s", xm.Events[len(xm.Events)-1].Raw)
	}
	if want := fmt.Sprintf("00:%02d:%02d", (total-1)/60, (total-1)%60); xm.LastClock != want {
		t.Errorf("LastClock = %q，期望 %q", xm.LastClock, want)
	}
	// 账仍然对得上：Kept 数的是全量，不是折完之后的列表长度。
	if tl.Kept != total+1 {
		t.Errorf("Kept = %d，期望 %d（发言 + 服务端那条验证行）", tl.Kept, total+1)
	}
	// 界面那句"本轮从 … 开始"吃的就是这个锚点，缺了它这句话就没依据。
	if want := start.UTC().Format(time.RFC3339); tl.RoundStart != want {
		t.Errorf("roundStart = %q，期望 %q", tl.RoundStart, want)
	}
}

// 没有那行 Current time（日志被手工拷来、或者头几行已经被截掉）时，
// 界面就不该有"本轮从 … 开始"这一档——宁可不写，也不能编一个起点。
func TestTimelineOmitsRoundStartWithoutAnchor(t *testing.T) {
	tl, err := timelineFixture(t).Timeline("Cluster_1", dst.ShardMaster, 0)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.RoundStart != "" {
		t.Errorf("样本里没有 Current time 行，roundStart 却是 %q", tl.RoundStart)
	}
	if len(tl.Players) == 0 {
		t.Fatal("没有起点也该照常归出人")
	}
	for _, p := range tl.Players {
		for _, e := range p.Events {
			if e.At != "" {
				t.Errorf("%s 那行凭空有了墙上时间 %q", e.Clock, e.At)
			}
		}
	}
}

// 窗口上限和日志页一致：给一个比 MaxLines 大的数不该悄悄全收。
func TestTimelineClampsWindow(t *testing.T) {
	tl, err := timelineFixture(t).Timeline("Cluster_1", dst.ShardMaster, MaxLines+500)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.Window != MaxLines {
		t.Errorf("Window = %d，期望被夹到 %d", tl.Window, MaxLines)
	}
}

// 时间线给出的每个事件码都必须是筛选下拉里存在的项：
// 反过来说，界面按某个码筛日志时，那一行必须筛得出来。
// 这条钉住"事件字典"和"行为表"共用一张表，谁也别偷偷加一项。
func TestTimelineEventsAreInCatalog(t *testing.T) {
	tl, err := timelineFixture(t).Timeline("Cluster_1", dst.ShardMaster, 0)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	inCatalog := map[string]bool{}
	for _, ev := range EventCatalog() {
		inCatalog[ev.Code] = true
	}
	seen := 0
	for _, p := range tl.Players {
		for _, c := range p.Counts {
			if !inCatalog[c.Code] {
				t.Errorf("计数里的 %s 不在事件目录，筛选下拉永远选不到它", c.Code)
			}
			seen++
		}
		for _, e := range p.Events {
			if !inCatalog[e.Event] {
				t.Errorf("事件 %s 不在目录", e.Event)
			}
			// 每条都要能原样回到日志页那一行：偏移、种类、时钟三样缺一个就指不回去。
			if e.Offset == 0 {
				t.Errorf("%s 没带文件偏移，界面无法跳到日志页那一行", e.Event)
			}
			if e.Kind != string(KindServer) && e.Kind != string(KindChat) {
				t.Errorf("kind = %q，只该是 server/chat", e.Kind)
			}
			if e.Raw == "" {
				t.Errorf("%s 丢了原文", e.Event)
			}
		}
	}
	if seen == 0 {
		t.Fatal("一条计数都没归出来，样本或规则表坏了")
	}
}

// 行为表和"哪些行算行为"必须是同一张表：
// 目录里能筛出的行为码，时间线也要能数；否则界面能选出一个永远没有记录的分组。
func TestBehaviourOrderCoversEveryAnnouncement(t *testing.T) {
	// 色档管的是"这一下算不算坏消息"，判错方向比不给色更糟：
	// 被踢和被封锁是要处理的，死亡和消散是已经发生的。
	toneWant := map[string]string{
		EventPlayerDeath: "warn", EventGhostGone: "warn", EventAFKStart: "warn",
		EventResurrect: "ok", EventAFKEnd: "ok",
		EventKick: "bad", EventBan: "bad",
		EventPlayerJoin: "", EventPlayerLeave: "", EventAuthOK: "", EventChat: "",
	}
	for _, code := range behaviourOrder {
		if eventShortName(code) == code {
			t.Errorf("%s 没有中文名", code)
		}
		if !isBehaviour(code) {
			t.Errorf("%s 在行为表里却不被认成行为", code)
		}
		if want, ok := toneWant[code]; ok {
			if got := behaviourTone[code]; got != want {
				t.Errorf("%s 的色档 = %q，期望 %q", code, got, want)
			}
		} else {
			t.Errorf("行为表里多了 %s，色档表没跟着加", code)
		}
	}
	// 引擎公告里那些没进行为表的定性（存档、启动、端口）不该出现在个人时间线上。
	for _, code := range []string{EventSave, EventBoot, EventShardUp, EventAnnounce, EventPlain} {
		if isBehaviour(code) {
			t.Errorf("%s 不是玩家行为，不该进行为表", code)
		}
	}
}
