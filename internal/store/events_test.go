package store

import (
	"testing"
	"time"

	"dstpanel/internal/gamelog"
)

// 时间线的断言全部盯「几何」：一段在服区间从哪儿到哪儿、怎么收尾、时长相加对不对。
// 绝对时刻由归档那一层负责（那里要把引擎时钟换算成时刻），这层只管把行归成段。

// tlAt 造一个 UTC 时刻。固定 2026-11-03 是为了让窗口边界写得出来也读得出来。
func tlAt(h, m, s int) time.Time {
	return time.Date(2026, 11, 3, h, m, s, 0, time.UTC)
}

type tlRow struct {
	at    time.Time
	round time.Time
	shard string
	user  string
	name  string
	event string
}

func mustRecord(t *testing.T, s *Store, cluster string, rows ...tlRow) int {
	t.Helper()
	evs := make([]PlayerEventRow, 0, len(rows))
	for _, r := range rows {
		evs = append(evs, PlayerEventRow{
			At: r.at, Round: r.round.UnixMilli(), Cluster: cluster, Shard: r.shard,
			UserID: r.user, Name: r.name, Event: r.event, Source: "server",
		})
	}
	n, err := s.RecordPlayerEvents(evs)
	if err != nil {
		t.Fatalf("RecordPlayerEvents: %v", err)
	}
	return n
}

// personOf 按显示名取归并结果里的那个人。找不到就直接失败——
// 后面的断言全都建立在「这个人存在」上。
func personOf(t *testing.T, tl Timeline, name string) TimelinePerson {
	t.Helper()
	for _, p := range tl.People {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("时间线里没有 %q，共 %d 个人", name, len(tl.People))
	return TimelinePerson{}
}

func TestRecordPlayerEventsDedupsOnKey(t *testing.T) {
	s := newStore(t)
	round := tlAt(9, 0, 0)
	rows := []tlRow{
		{at: tlAt(10, 0, 0), round: round, shard: "Master", name: "小满", event: gamelog.EventPlayerJoin},
		{at: tlAt(10, 0, 0), round: round, shard: "Master", user: "KU_a", event: gamelog.EventPlayerJoin},
	}
	if n := mustRecord(t, s, "Cluster_1", rows...); n != 2 {
		t.Fatalf("第一次写入 %d 条，期望 2", n)
	}
	// 同一批原样再写一遍必须一条都不进：面板重启后归档器会回看同一个窗口，
	// 撞不开的话同一次进服在库里就是两行，时间线上多出的一段根本不存在。
	if n := mustRecord(t, s, "Cluster_1", rows...); n != 0 {
		t.Errorf("重复写入 %d 条，期望 0（去重键没起作用）", n)
	}
	// 换一个世界就是另一行：洞穴和地表的进出是两个事件，合不掉。
	other := tlRow{at: tlAt(10, 0, 0), round: round, shard: "Caves", name: "小满", event: gamelog.EventPlayerJoin}
	if n := mustRecord(t, s, "Cluster_1", other); n != 1 {
		t.Errorf("换世界的同刻同名同事件写入 %d 条，期望 1", n)
	}
	span, err := s.PlayerEventSpan("Cluster_1")
	if err != nil {
		t.Fatalf("PlayerEventSpan: %v", err)
	}
	if span.Rows != 3 || !span.From.Equal(tlAt(10, 0, 0)) || !span.To.Equal(tlAt(10, 0, 0)) {
		t.Errorf("库里的账 = %+v，期望 3 条、首尾都在 10:00", span)
	}
}

// TestPlayerTimelineJoinsSpanAndCountsOnlineSeconds 是最基础的那一条：
// 进服 + 离开 = 一段，死亡是段上的记号而不是段的结尾（死了的人还在服里当幽灵）。
func TestPlayerTimelineJoinsSpanAndCountsOnlineSeconds(t *testing.T) {
	s := newStore(t)
	round := tlAt(9, 0, 0)
	const cluster = "Cluster_1"
	n := mustRecord(t, s, cluster,
		tlRow{at: tlAt(10, 0, 0), round: round, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin},
		tlRow{at: tlAt(10, 20, 0), round: round, shard: "Master", name: "小满", event: gamelog.EventPlayerDeath},
		tlRow{at: tlAt(10, 50, 0), round: round, shard: "Master", name: "小满", event: gamelog.EventPlayerLeave},
		tlRow{at: tlAt(10, 10, 0), round: round, shard: "Master", user: "KU_b", name: "阿栗", event: gamelog.EventPlayerJoin},
	)
	if n != 4 {
		t.Fatalf("写入 %d 条，期望 4", n)
	}

	tl, err := s.PlayerTimeline(cluster, tlAt(9, 30, 0), tlAt(12, 0, 0))
	if err != nil {
		t.Fatalf("PlayerTimeline: %v", err)
	}
	if tl.Rows != 4 {
		t.Errorf("Rows = %d，期望 4", tl.Rows)
	}
	if !tl.RecordFrom.Equal(tlAt(10, 0, 0)) {
		t.Errorf("RecordFrom = %v，期望最早那条行为 10:00", tl.RecordFrom)
	}
	// 现在还在里面的那个排在前面：图上一眼要先看见「此刻谁在」。
	if len(tl.People) != 2 || tl.People[0].Name != "阿栗" || tl.People[1].Name != "小满" {
		t.Fatalf("人的次序 = %v，期望在线的阿栗在前", tl.People)
	}

	a := personOf(t, tl, "小满")
	if len(a.Spans) != 1 {
		t.Fatalf("小满有 %d 段，期望 1 段：%v", len(a.Spans), a.Spans)
	}
	sp := a.Spans[0]
	if !sp.From.Equal(tlAt(10, 0, 0)) || !sp.To.Equal(tlAt(10, 50, 0)) {
		t.Errorf("区间 = %v ~ %v，期望 10:00 ~ 10:50", sp.From, sp.To)
	}
	if sp.EndedBy != SpanEndLeave {
		t.Errorf("收尾方式 = %q，期望 %q", sp.EndedBy, SpanEndLeave)
	}
	if a.Seconds != 3000 {
		t.Errorf("在线时长 = %d 秒，期望 3000（50 分钟）", a.Seconds)
	}
	if len(a.Marks) != 1 || a.Marks[0].Event != gamelog.EventPlayerDeath {
		t.Errorf("记号 = %v，期望一条死亡", a.Marks)
	}
	if a.OnlineNow {
		t.Error("小满已经打过离开行了，不许报现在在线")
	}

	b := personOf(t, tl, "阿栗")
	if len(b.Spans) != 1 {
		t.Fatalf("阿栗有 %d 段，期望 1 段", len(b.Spans))
	}
	if b.Spans[0].EndedBy != SpanEndOpen || !b.OnlineNow {
		t.Errorf("阿栗那段 = %q / onlineNow=%v，日志里没有离开行就该是开口段",
			b.Spans[0].EndedBy, b.OnlineNow)
	}
	// 开口段画到查询这一刻，而不是「他最后一条行」那一刻：
	// 只有死亡行的人被缩短到死亡时刻，图上就成了「他一死就出去了」，可他其实还在当幽灵。
	if !b.Spans[0].To.Equal(tlAt(12, 0, 0)) {
		t.Errorf("开口段右端 = %v，期望停在查询时刻 12:00", b.Spans[0].To)
	}
}

// TestPlayerTimelineMergesShardMigration 钉住那条会造假的规则：
// 洞穴和地表之间来回，引擎打的是「从这个世界离开」+「从那个世界进来」两行，
// 照字面画就成了「他出去了又回来」，而用户知道自己根本没离开过房间。
func TestPlayerTimelineMergesShardMigration(t *testing.T) {
	s := newStore(t)
	r1, r2 := tlAt(9, 0, 0), tlAt(9, 50, 0)
	const cluster = "Cluster_1"
	mustRecord(t, s, cluster,
		tlRow{at: tlAt(10, 0, 0), round: r1, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin},
		tlRow{at: tlAt(10, 30, 0), round: r1, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerLeave},
		tlRow{at: tlAt(10, 30, 30), round: r2, shard: "Caves", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin},
		tlRow{at: tlAt(11, 0, 0), round: r2, shard: "Caves", user: "KU_a", name: "小满", event: gamelog.EventPlayerLeave},
	)
	tl, err := s.PlayerTimeline(cluster, tlAt(9, 30, 0), tlAt(12, 0, 0))
	if err != nil {
		t.Fatalf("PlayerTimeline: %v", err)
	}
	if len(tl.People) != 1 {
		t.Fatalf("认出 %d 个人，期望 1 个（同一条账户编号不许裂成两行）", len(tl.People))
	}
	p := tl.People[0]
	if len(p.Spans) != 1 {
		t.Fatalf("%d 段，期望合成 1 段：%v", len(p.Spans), p.Spans)
	}
	if !p.Spans[0].From.Equal(tlAt(10, 0, 0)) || !p.Spans[0].To.Equal(tlAt(11, 0, 0)) {
		t.Errorf("区间 = %v ~ %v，期望 10:00 ~ 11:00", p.Spans[0].From, p.Spans[0].To)
	}
	if p.Seconds != 3600 {
		t.Errorf("时长 = %d 秒，期望 3600（迁移那 30 秒不该算成离开）", p.Seconds)
	}

	// 反过来：真出去吃个饭再回来（超过一分钟）必须还是两段，不能什么间隔都合。
	s2 := newStore(t)
	mustRecord(t, s2, cluster,
		tlRow{at: tlAt(10, 0, 0), round: r1, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin},
		tlRow{at: tlAt(10, 30, 0), round: r1, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerLeave},
		tlRow{at: tlAt(10, 32, 0), round: r1, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin},
	)
	tl2, err := s2.PlayerTimeline(cluster, tlAt(9, 30, 0), tlAt(12, 0, 0))
	if err != nil {
		t.Fatalf("PlayerTimeline(第二段): %v", err)
	}
	if got := len(tl2.People[0].Spans); got != 2 {
		t.Errorf("隔 2 分钟回来只剩 %d 段，期望 2 段（这次是真的出去了）", got)
	}
}

// TestPlayerTimelineCollapsesCrossShardRepeats 钉的是「一次公告、两个世界各写一遍」。
// 库里那两行都得留着（去重键带分片和轮次，判「这个世界后来起过新一轮」要用它们），
// 但一次发生只能画一段、点一个记号。不挡这一手的话最容易被撞见的就是换世界的人：
// 在线时长直接翻倍，图上多出一对成对的死亡点。
func TestPlayerTimelineCollapsesCrossShardRepeats(t *testing.T) {
	s := newStore(t)
	const cluster = "Cluster_1"
	r1, r2 := tlAt(9, 0, 0), tlAt(9, 0, 4)
	n := mustRecord(t, s, cluster,
		tlRow{at: tlAt(10, 0, 0), round: r1, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin},
		// 洞穴那一份晚两秒。两边的时刻是各分片拿「自己的本轮启动 + 自己日志里的相对时钟」
		// 换算的，开机早晚的差自己抵消，剩下的就是这种秒级取整误差。
		tlRow{at: tlAt(10, 0, 2), round: r2, shard: "Caves", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin},
		tlRow{at: tlAt(10, 20, 0), round: r1, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerDeath},
		tlRow{at: tlAt(10, 20, 1), round: r2, shard: "Caves", user: "KU_a", name: "小满", event: gamelog.EventPlayerDeath},
		tlRow{at: tlAt(10, 50, 0), round: r1, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerLeave},
		tlRow{at: tlAt(10, 50, 3), round: r2, shard: "Caves", user: "KU_a", name: "小满", event: gamelog.EventPlayerLeave},
	)
	if n != 6 {
		t.Fatalf("写入 %d 条，期望 6（两个世界各一份本来就该都进库）", n)
	}
	tl, err := s.PlayerTimeline(cluster, tlAt(9, 30, 0), tlAt(12, 0, 0))
	if err != nil {
		t.Fatalf("PlayerTimeline: %v", err)
	}
	// 上屏那个分母跟着图走：六行里有三次发生。
	if tl.Rows != 3 {
		t.Errorf("Rows = %d，期望 3（三次发生，另三条是副本）", tl.Rows)
	}
	p := personOf(t, tl, "小满")
	if len(p.Spans) != 1 {
		t.Fatalf("%d 段，期望 1 段：%v", len(p.Spans), p.Spans)
	}
	sp := p.Spans[0]
	if !sp.From.Equal(tlAt(10, 0, 0)) || !sp.To.Equal(tlAt(10, 50, 0)) {
		t.Errorf("区间 = %v ~ %v，期望停在先到那一份的 10:00 ~ 10:50", sp.From, sp.To)
	}
	if p.Seconds != 3000 {
		t.Errorf("时长 = %d 秒，期望 3000（副本不该再算一遍）", p.Seconds)
	}
	if len(p.Marks) != 1 || !p.Marks[0].At.Equal(tlAt(10, 20, 0)) {
		t.Errorf("记号 = %v，期望只有一个 10:20 的死亡", p.Marks)
	}

	// 只剩离开行的那一位更要挡：两个世界各一条离开，落到归并里是各起一段被切开的区间。
	s2 := newStore(t)
	mustRecord(t, s2, cluster,
		// 这一条在窗口之前，只为把「记录从」推到 10:00，好让切开的那段真有得画。
		tlRow{at: tlAt(10, 0, 0), round: r1, shard: "Master", user: "KU_x", name: "路人甲", event: gamelog.EventPlayerJoin},
		tlRow{at: tlAt(12, 0, 0), round: r1, shard: "Master", name: "阿黎", event: gamelog.EventPlayerLeave},
		tlRow{at: tlAt(12, 0, 2), round: r2, shard: "Caves", name: "阿黎", event: gamelog.EventPlayerLeave},
	)
	tl2, err := s2.PlayerTimeline(cluster, tlAt(11, 0, 0), tlAt(13, 0, 0))
	if err != nil {
		t.Fatalf("PlayerTimeline(只剩离开行): %v", err)
	}
	q := personOf(t, tl2, "阿黎")
	if len(q.Spans) != 1 {
		t.Fatalf("%d 段，期望 1 段：%v", len(q.Spans), q.Spans)
	}
	if sp := q.Spans[0]; !sp.ClippedStart || !sp.From.Equal(tlAt(11, 0, 0)) || !sp.To.Equal(tlAt(12, 0, 0)) {
		t.Errorf("切开的那段 = %+v，期望从窗口边界 11:00 画到 12:00", sp)
	}
	if q.Seconds != 3600 {
		t.Errorf("时长 = %d 秒，期望 3600（两遍区间会算成 7200）", q.Seconds)
	}
}

// TestPlayerTimelineClosesOpenSpanByEvidence 是开口段的三种收口。
// 每种都得有说得出名字的凭据，画错就是把「他还在里面」说成一件不成立的事。
func TestPlayerTimelineClosesOpenSpanByEvidence(t *testing.T) {
	const cluster = "Cluster_1"
	round := tlAt(9, 0, 0)
	join := tlRow{at: tlAt(10, 0, 0), round: round, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin}
	until := tlAt(12, 0, 0)

	t.Run("同分片起过新一轮就停在新一轮的启动时刻", func(t *testing.T) {
		s := newStore(t)
		newRound := tlAt(11, 0, 0)
		mustRecord(t, s, cluster, join,
			tlRow{at: newRound, round: newRound, shard: "Master", user: "KU_b", name: "阿栗", event: gamelog.EventPlayerJoin})
		tl, err := s.PlayerTimeline(cluster, tlAt(9, 30, 0), until)
		if err != nil {
			t.Fatalf("PlayerTimeline: %v", err)
		}
		sp := personOf(t, tl, "小满").Spans[0]
		if sp.EndedBy != SpanEndRestart {
			t.Errorf("收尾 = %q，期望 %q", sp.EndedBy, SpanEndRestart)
		}
		// 引擎每轮启动会截断重写日志，上一轮里「没打离开行」最多只能算到新一轮起来那一刻。
		if !sp.To.Equal(newRound) {
			t.Errorf("右端 = %v，期望停在新一轮启动时刻 %v", sp.To, newRound)
		}
		if sp.Open {
			t.Error("已经收口了却还标着开口，界面会把他算成此刻在线")
		}
	})

	t.Run("没起新一轮但采样停了就停在最后一次采样", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s, cluster, join)
		if err := s.RecordSnapshot(Snapshot{
			Cluster: cluster, Shard: "Master", Online: 1, At: tlAt(11, 30, 0),
		}); err != nil {
			t.Fatalf("铺采样: %v", err)
		}
		tl, err := s.PlayerTimeline(cluster, tlAt(9, 30, 0), until)
		if err != nil {
			t.Fatalf("PlayerTimeline: %v", err)
		}
		sp := personOf(t, tl, "小满").Spans[0]
		if sp.EndedBy != SpanEndStop {
			t.Errorf("收尾 = %q，期望 %q（三天前进来、没打离开行的人不能画成这三天一直在服里）",
				sp.EndedBy, SpanEndStop)
		}
		if !sp.To.Equal(tlAt(11, 30, 0)) {
			t.Errorf("右端 = %v，期望最后一次采样 11:30", sp.To)
		}
		if !tl.LastSampleAt.Equal(tlAt(11, 30, 0)) {
			t.Errorf("LastSampleAt = %v，期望 11:30", tl.LastSampleAt)
		}
	})

	t.Run("采样比查询时刻还新就仍然算在线", func(t *testing.T) {
		s := newStore(t)
		mustRecord(t, s, cluster, join)
		if err := s.RecordSnapshot(Snapshot{
			Cluster: cluster, Shard: "Master", Online: 1, At: tlAt(12, 30, 0),
		}); err != nil {
			t.Fatalf("铺采样: %v", err)
		}
		tl, err := s.PlayerTimeline(cluster, tlAt(9, 30, 0), until)
		if err != nil {
			t.Fatalf("PlayerTimeline: %v", err)
		}
		sp := personOf(t, tl, "小满").Spans[0]
		// 采样点在将来只会是「这条查询的时刻落在两轮采样之间」，不能拿它把一段收掉，
		// 更不能把右端推到查询时刻之外——图上画出一个未来的区间是纯粹的假话。
		if sp.EndedBy != SpanEndOpen || !sp.To.Equal(until) {
			t.Errorf("收尾 = %q 右端 %v，期望开口且停在查询时刻", sp.EndedBy, sp.To)
		}
	})
}

// TestPlayerTimelineClipsSpanStartAtWindowEdge 钉的是「没有起点行」那一类：
// 进服行早就滚出窗口了，这一段只能从窗口边界画起，并且必须带着被切开的标记上屏。
func TestPlayerTimelineClipsSpanStartAtWindowEdge(t *testing.T) {
	s := newStore(t)
	const cluster = "Cluster_1"
	round := tlAt(9, 0, 0)
	// 死亡行在窗口之前（10:00 是这个房间的起点），窗口从 11:00 起，只读到他 12:00 离开。
	mustRecord(t, s, cluster,
		tlRow{at: tlAt(10, 0, 0), round: round, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerDeath},
		tlRow{at: tlAt(12, 0, 0), round: round, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerLeave},
	)
	tl, err := s.PlayerTimeline(cluster, tlAt(11, 0, 0), tlAt(13, 0, 0))
	if err != nil {
		t.Fatalf("PlayerTimeline: %v", err)
	}
	// 起点不受窗口限制：界面那句「面板从什么时候开始攒」必须说得出真正的起点。
	if !tl.RecordFrom.Equal(tlAt(10, 0, 0)) {
		t.Errorf("RecordFrom = %v，期望库里最早那条 10:00", tl.RecordFrom)
	}
	if len(tl.People[0].Marks) != 0 {
		t.Errorf("窗口外那条死亡也被算成记号：%v", tl.People[0].Marks)
	}
	p := personOf(t, tl, "小满")
	if len(p.Spans) != 1 {
		t.Fatalf("%d 段：%v", len(p.Spans), p.Spans)
	}
	sp := p.Spans[0]
	if !sp.ClippedStart {
		t.Error("这段没有起点行却没标被切开的起点，界面会把它说成他 11 点进来的")
	}
	if !sp.From.Equal(tlAt(11, 0, 0)) || !sp.To.Equal(tlAt(12, 0, 0)) {
		t.Errorf("区间 = %v ~ %v，期望从窗口边界 11:00 画到 12:00", sp.From, sp.To)
	}
}

// TestPlayerTimelineClipsSpanStartAtRoundEdge 钉的是「这一轮里没有他的进服行，而世界重启过」。
// 少了本轮启动这个下界，上一轮攒下的记录起点会把这一段一路拉长进上一轮，
// 于是同一个人在图上出现两段互相重叠的横条，时长还把它加了两遍——真机上一天重启几次是常态。
func TestPlayerTimelineClipsSpanStartAtRoundEdge(t *testing.T) {
	s := newStore(t)
	const cluster = "Cluster_1"
	r1, r2 := tlAt(9, 0, 0), tlAt(11, 0, 0)
	mustRecord(t, s, cluster,
		tlRow{at: tlAt(10, 0, 0), round: r1, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin},
		tlRow{at: tlAt(10, 30, 0), round: r1, shard: "Master", name: "阿黎", event: gamelog.EventPlayerLeave},
		tlRow{at: tlAt(10, 30, 0), round: r1, shard: "Caves", name: "阿黎", event: gamelog.EventPlayerLeave},
		// 11:00 起了新一轮，引擎把日志截断重写了：这一轮里只有阿黎的离开行。
		tlRow{at: tlAt(11, 30, 0), round: r2, shard: "Master", name: "阿黎", event: gamelog.EventPlayerLeave},
		tlRow{at: tlAt(11, 30, 0), round: r2, shard: "Caves", name: "阿黎", event: gamelog.EventPlayerLeave},
	)
	tl, err := s.PlayerTimeline(cluster, tlAt(9, 30, 0), tlAt(12, 0, 0))
	if err != nil {
		t.Fatalf("PlayerTimeline: %v", err)
	}
	p := personOf(t, tl, "阿黎")
	if len(p.Spans) != 2 {
		t.Fatalf("%d 段，期望 2 段（两轮各一段）：%v", len(p.Spans), p.Spans)
	}
	if !p.Spans[0].From.Equal(tlAt(10, 0, 0)) || !p.Spans[0].To.Equal(tlAt(10, 30, 0)) {
		t.Errorf("上一轮那段 = %v ~ %v，期望从记录起点 10:00 画到 10:30", p.Spans[0].From, p.Spans[0].To)
	}
	if !p.Spans[1].From.Equal(tlAt(11, 0, 0)) || !p.Spans[1].To.Equal(tlAt(11, 30, 0)) {
		t.Errorf("本轮那段 = %v ~ %v，期望从本轮启动 11:00 画到 11:30，不许伸进上一轮",
			p.Spans[1].From, p.Spans[1].To)
	}
	if p.Spans[0].To.After(p.Spans[1].From) {
		t.Errorf("两段重叠：%v ~ %v", p.Spans[0], p.Spans[1])
	}
	if p.Seconds != 3600 {
		t.Errorf("时长 = %d 秒，期望 3600（两段各半小时）", p.Seconds)
	}
	// 上一轮里那个只有进服行的人，靠新一轮启动收口，不该被画成此刻还在里面。
	if m := personOf(t, tl, "小满"); m.Spans[0].EndedBy != SpanEndRestart || m.OnlineNow {
		t.Errorf("小满那段收尾 = %q / onlineNow=%v，期望停在新一轮启动的 restart",
			m.Spans[0].EndedBy, m.OnlineNow)
	}
}

// TestPlayerTimelineLinksNameOnlyRows 是「一份日志两种写法」的合流点：
// 公告行只带显示名，验证行两个标识都有，接不上的话同一个人会出现两条泳道。
func TestPlayerTimelineLinksNameOnlyRows(t *testing.T) {
	s := newStore(t)
	const cluster = "Cluster_1"
	round := tlAt(9, 0, 0)
	mustRecord(t, s, cluster,
		tlRow{at: tlAt(10, 0, 0), round: round, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerDeath},
		tlRow{at: tlAt(10, 5, 0), round: round, shard: "Master", name: "小满", event: gamelog.EventPlayerJoin},
		tlRow{at: tlAt(10, 40, 0), round: round, shard: "Master", name: "小满", event: gamelog.EventPlayerLeave},
		// 另一个人的公告行在花名册和这一轮里都对不上号，只能按名字立一条泳道。
		tlRow{at: tlAt(10, 6, 0), round: round, shard: "Master", name: "路人甲", event: gamelog.EventPlayerJoin},
	)
	tl, err := s.PlayerTimeline(cluster, tlAt(9, 30, 0), tlAt(12, 0, 0))
	if err != nil {
		t.Fatalf("PlayerTimeline: %v", err)
	}
	if len(tl.People) != 2 {
		t.Fatalf("认出 %d 个人，期望 2 个：%v", len(tl.People), tl.People)
	}
	a := personOf(t, tl, "小满")
	if a.UserID != "KU_a" {
		t.Errorf("小满的账户编号 = %q，期望从同名的验证行接过来", a.UserID)
	}
	if len(a.Spans) != 1 || !a.Spans[0].From.Equal(tlAt(10, 5, 0)) {
		t.Errorf("小满的区间 = %v，期望只有 10:05 那一段", a.Spans)
	}
	b := personOf(t, tl, "路人甲")
	if b.UserID != "" {
		t.Errorf("路人甲被安上了账户编号 %q，对不上号就该空着", b.UserID)
	}
}

// TestPlayerTimelineStopsAtRowLimit 钉上限那一手：撞到了要认，不能静默少画人。
// 一次归并读的行数是有硬上限的（房间被开成公网服时这里是几万行），
// 截断本身不可怕，可怕的是图上少了几条泳道却一句都没说。
func TestPlayerTimelineStopsAtRowLimit(t *testing.T) {
	s := newStore(t)
	const cluster = "Cluster_1"
	round := tlAt(1, 0, 0)
	base := tlAt(2, 0, 0)
	var rows []tlRow
	for i := 0; i <= TimelineRowLimit/2; i++ {
		at := base.Add(time.Duration(i) * 20 * time.Second)
		rows = append(rows,
			tlRow{at: at, round: round, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerJoin},
			tlRow{at: at.Add(10 * time.Second), round: round, shard: "Master", user: "KU_a", name: "小满", event: gamelog.EventPlayerLeave},
		)
	}
	if n := mustRecord(t, s, cluster, rows...); n != len(rows) {
		t.Fatalf("写入 %d 条，期望 %d", n, len(rows))
	}
	tl, err := s.PlayerTimeline(cluster, tlAt(1, 30, 0), tlAt(23, 0, 0))
	if err != nil {
		t.Fatalf("PlayerTimeline: %v", err)
	}
	if !tl.Truncated {
		t.Error("行数超了上限却没报截断")
	}
	if tl.Rows != TimelineRowLimit {
		t.Errorf("Rows = %d，期望正好是上限 %d", tl.Rows, TimelineRowLimit)
	}
}
