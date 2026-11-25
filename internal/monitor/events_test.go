package monitor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"dstpanel/internal/gamelog"
	"dstpanel/internal/store"
)

// 归档这一层的断言只有一件事值得钉：日志文件里那些行能不能变成带绝对时刻的库行，
// 以及同一行会不会被写第二遍。归并几何在 store/events_test.go 里钉过了。

// 行形状逐条对着 2026-11-03 云机 Cluster_1/Master 的两份文件写，只把标识换成夹具值。
// 换掉的是「谁的标识」，不是「引擎怎么写这些行」——形状一改，测出来的就是夹具而不是真机。
const archiverServerBody = "Current time: Tue Nov 03 09:00:00 2026\n" +
	"[00:00:00]: Version: 760944\n" +
	"[00:01:06]: Client authenticated: (KU_aaaa1111) 风萧萧\n" +
	"[00:04:00]: 风萧萧 was killed by SpiderdenGuardian. They became a spooky ghost!\n"

// 进服/离开这两记在真机上只写在聊天文件里（服务端日志那份 grep 零命中），
// 而那份没有 Current time 行——时刻必须从服务端那份借。
const archiverChatBody = "[00:01:24]: [Join Announcement] 风萧萧\n" +
	"[00:06:43]: [Leave Announcement] 风萧萧\n"

func writeArchiverLogs(t *testing.T, f *fixture, serverBody, chatBody string) {
	t.Helper()
	for _, pair := range []struct {
		path, body string
	}{
		{f.paths.ServerLog(f.cluster, "Master"), serverBody},
		{f.paths.ChatLog(f.cluster, "Master"), chatBody},
	} {
		if err := os.MkdirAll(filepath.Dir(pair.path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pair.path, []byte(pair.body), 0o644); err != nil {
			t.Fatalf("写日志: %v", err)
		}
	}
}

func newArchiver(f *fixture) *Archiver {
	return &Archiver{Paths: f.paths, Store: f.s.Store, Logger: f.s.Logger}
}

// timelineFromDB 用库里的实际跨度当窗口，避免测试依赖本机时区：
// Current time 那行是服务器本地时间，换算出的绝对时刻会跟着时区走，
// 而行与行之间的差是定的，所以断言只比差值。
func timelineFromDB(t *testing.T, s *store.Store, cluster string) store.Timeline {
	t.Helper()
	span, err := s.PlayerEventSpan(cluster)
	if err != nil {
		t.Fatalf("PlayerEventSpan: %v", err)
	}
	if span.Rows == 0 {
		t.Fatal("库里一条玩家行为都没有")
	}
	tl, err := s.PlayerTimeline(cluster, span.From.Add(-time.Minute), span.To.Add(time.Hour))
	if err != nil {
		t.Fatalf("PlayerTimeline: %v", err)
	}
	return tl
}

func TestArchiverWritesEventsWithAbsoluteTimes(t *testing.T) {
	f := newFixture(t)
	writeArchiverLogs(t, f, archiverServerBody, archiverChatBody)
	// 花名册是助手采样写的、两个标识都有；归档行要靠它把公告里的名字接上账户编号。
	if err := f.s.Store.UpsertSeenPlayers(f.cluster, "Master", []store.SeenPlayer{
		{UserID: "KU_aaaa1111", Name: "风萧萧", Prefab: "wilson"},
	}, time.Now().UTC()); err != nil {
		t.Fatalf("铺花名册: %v", err)
	}

	res := newArchiver(f).Archive()
	// 三行：聊天里的进服与离开 + 服务端里的死亡。Current time、Version、
	// 验证那三行不是玩家行为，一行都不许进表。
	if res.Written != 3 || res.Read != 3 {
		t.Errorf("写入 %d 条 / 读到 %d 条，期望都是 3（raw=%+v）", res.Written, res.Read, res)
	}
	if res.SkippedNoAt != 0 || res.SkippedNoWho != 0 {
		t.Errorf("本该认得出人和时刻，却跳过了 noAt=%d noWho=%d", res.SkippedNoAt, res.SkippedNoWho)
	}

	tl := timelineFromDB(t, f.s.Store, f.cluster)
	if len(tl.People) != 1 {
		t.Fatalf("认出 %d 个人：%v", len(tl.People), tl.People)
	}
	p := tl.People[0]
	if p.UserID != "KU_aaaa1111" {
		t.Errorf("泳道的账户编号 = %q，期望顺着花名册接上（接不上这人改一次名就裂成两条）", p.UserID)
	}
	if len(p.Spans) != 1 {
		t.Fatalf("%d 段，期望 1 段：%v", len(p.Spans), p.Spans)
	}
	// 进服 00:01:24、离开 00:06:43 都在聊天文件里，死亡 00:04:00 在服务端文件里。
	// 时长对得上就说明聊天那两行借到了服务端那份的启动时刻，而不是各算各的。
	if got := p.Seconds; got != 319 {
		t.Errorf("在线 %d 秒，期望 319（00:01:24 ~ 00:06:43）", got)
	}
	if len(p.Marks) != 1 || p.Marks[0].Event != gamelog.EventPlayerDeath {
		t.Fatalf("记号 = %v，期望一条死亡", p.Marks)
	}
	if gap := p.Marks[0].At.Sub(p.Spans[0].From); gap != 156*time.Second {
		t.Errorf("死亡落在进服后 %v，期望 2 分 36 秒（两份文件的行必须换算到同一条时间轴）", gap)
	}
	if p.Spans[0].EndedBy != store.SpanEndLeave {
		t.Errorf("收尾 = %q，期望 leave", p.Spans[0].EndedBy)
	}
}

// TestArchiverReadsOnlyNewLines 是续读那一手：第二遍跑一条都不该写，
// 追加一行只该写那一行。每轮重读整个窗口的话，去重全靠唯一键，
// 重复解析的量会一直摊在 CPU 上，而"写了 0 条"也再也解释不了到底读没读到。
func TestArchiverReadsOnlyNewLines(t *testing.T) {
	f := newFixture(t)
	writeArchiverLogs(t, f, archiverServerBody, archiverChatBody)
	a := newArchiver(f)
	if n := a.ArchiveOnce(); n != 3 {
		t.Fatalf("第一遍写了 %d 条，期望 3", n)
	}
	if n := a.ArchiveOnce(); n != 0 {
		t.Errorf("第二遍写了 %d 条，期望 0（位置记不住就会反复重读同一截日志）", n)
	}

	path := f.paths.ChatLog(f.cluster, "Master")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body,
		[]byte("[00:08:00]: [Join Announcement] 阿栗\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	res := a.Archive()
	if res.Written != 1 {
		t.Errorf("追加一行动了 %d 条，期望 1", res.Written)
	}
	if res.Read != 1 {
		t.Errorf("这一轮读到 %d 条行为行，期望只数到新加的那 1 条", res.Read)
	}
	tl := timelineFromDB(t, f.s.Store, f.cluster)
	if len(tl.People) != 2 {
		t.Fatalf("认出 %d 个人，期望 2 个：%v", len(tl.People), tl.People)
	}
	// 阿栗只在聊天文件里露过一次名字，花名册没有他：这时候只能按显示名立一条泳道，
	// 绝不能给他安一个别人的账户编号。
	for _, p := range tl.People {
		if p.Name == "阿栗" && p.UserID != "" {
			t.Errorf("给对不上号的人编了账户编号 %q", p.UserID)
		}
	}
}

// TestArchiverDropsRowsItCannotPlace 钉住两种"不敢入库"的行：
// 换算不出绝对时刻的（这份日志没有 Current time 行）、认不出是谁的。
// 放它们进库，时间线上就会出现一段不知道属于谁、也不知道是几点的区间，那是编。
func TestArchiverDropsRowsItCannotPlace(t *testing.T) {
	f := newFixture(t)
	noStart := "[00:01:06]: Client authenticated: (KU_aaaa1111) 风萧萧\n" +
		"[00:04:00]: 风萧萧 was killed by SpiderdenGuardian. They became a spooky ghost!\n"
	writeArchiverLogs(t, f, noStart, archiverChatBody)
	res := newArchiver(f).Archive()
	if res.Written != 0 {
		t.Errorf("没有 Current time 行却写了 %d 条，期望 0", res.Written)
	}
	if res.SkippedNoAt != 3 {
		t.Errorf("跳过的没有时刻的行 = %d，期望 3（三行都要单独报出来）", res.SkippedNoAt)
	}

	// 只有 [Leave Announcement] 那种整串正文就是名字的公告才可能拆不出名字，
	// 这里给一条空正文的造出现场。服务端那份只留启动行，免得死亡行把结果盖过去。
	f2 := newFixture(t)
	writeArchiverLogs(t, f2, "Current time: Tue Nov 03 09:00:00 2026\n[00:00:00]: Version: 760944\n",
		"[00:02:00]: [Leave Announcement]\n")
	res2 := newArchiver(f2).Archive()
	if res2.Written != 0 {
		t.Errorf("认不出是谁的行被写了 %d 条，期望 0", res2.Written)
	}
	if res2.SkippedNoWho == 0 {
		t.Error("认不出是谁的行没被单独计数，出了现场分不出是世界没开还是面板读不懂日志")
	}
}

// TestArchiverSurvivesMissingLogs 是每台新机器的一开机现场：
// 房间目录建好了、世界从没跑过，日志文件不存在。这一轮不该写错误日志，
// 更不该让整个采样循环跟着失败。
func TestArchiverSurvivesMissingLogs(t *testing.T) {
	f := newFixture(t)
	res := newArchiver(f).Archive()
	if res.Written != 0 || res.Read != 0 {
		t.Errorf("没有日志却写了 %+v", res)
	}
}
