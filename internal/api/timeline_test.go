package api

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/gamelog"
	"dstpanel/internal/store"
)

type tlReply struct {
	Cluster string `json:"cluster"`
	Hours   int    `json:"hours"`
	From    string `json:"from"`
	To      string `json:"to"`
	Rows    int    `json:"rows"`
	Events  []struct {
		Code  string `json:"code"`
		Name  string `json:"name"`
		Label string `json:"label"`
		Count int    `json:"count"`
	} `json:"events"`
	// RecordFrom 是"这份记录从什么时候开始攒"，界面上那句诚实说明取它。
	RecordFrom string `json:"recordFrom"`
	Truncated  bool   `json:"truncated"`
	Note       string `json:"note"`
	People     []struct {
		UserID    string `json:"userid"`
		Name      string `json:"name"`
		Seconds   int64  `json:"seconds"`
		OnlineNow bool   `json:"onlineNow"`
		LastAt    string `json:"lastAt"`
		Spans     []struct {
			From         string `json:"from"`
			To           string `json:"to"`
			Reason       string `json:"reason"`
			ReasonLabel  string `json:"reasonLabel"`
			ClippedStart bool   `json:"clippedStart"`
		} `json:"spans"`
		Marks []struct {
			At    string `json:"at"`
			Event string `json:"event"`
			Label string `json:"label"`
		} `json:"marks"`
	} `json:"people"`
}

func getTimeline(t *testing.T, h http.Handler, query string) (int, tlReply, string) {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/api/v1/player-timeline?"+query, "admin", "secret")
	var r tlReply
	if rec.Code == http.StatusOK {
		decodeData(t, rec, &r)
	}
	return rec.Code, r, rec.Body.String()
}

func seedEvents(t *testing.T, s *store.Store, cluster string, rows ...store.PlayerEventRow) {
	t.Helper()
	for i := range rows {
		rows[i].Cluster = cluster
		if rows[i].Shard == "" {
			rows[i].Shard = "Master"
		}
	}
	if _, err := s.RecordPlayerEvents(rows); err != nil {
		t.Fatalf("铺玩家行为: %v", err)
	}
}

// TestPlayerTimelineReplyShape 钉的是界面直接渲染的那份形状：时长、收尾说法、
// 记号名都得后端算完给全，前端一参与推断就会出现"图上说他在、库里说他早走了"。
func TestPlayerTimelineReplyShape(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	base := time.Now().UTC().Truncate(time.Minute)
	round := base.Add(-3 * time.Hour).UnixMilli()
	seedEvents(t, f.srv.Store, f.cluster,
		store.PlayerEventRow{At: base.Add(-2 * time.Hour), Round: round, Name: "小满", UserID: "KU_a", Event: gamelog.EventPlayerJoin},
		store.PlayerEventRow{At: base.Add(-100 * time.Minute), Round: round, Name: "小满", Event: gamelog.EventPlayerDeath},
		store.PlayerEventRow{At: base.Add(-1 * time.Hour), Round: round, Name: "小满", UserID: "KU_a", Event: gamelog.EventPlayerLeave},
		store.PlayerEventRow{At: base.Add(-30 * time.Minute), Round: round, Name: "阿栗", UserID: "KU_b", Event: gamelog.EventPlayerJoin},
	)

	code, r, body := getTimeline(t, h, "cluster="+f.cluster+"&hours=24")
	if code != http.StatusOK {
		t.Fatalf("状态 = %d，body %s", code, body)
	}
	if r.Cluster != f.cluster || r.Hours != 24 {
		t.Errorf("回声字段 = %q / %d", r.Cluster, r.Hours)
	}
	if r.Rows != 4 {
		t.Errorf("rows = %d，期望 4", r.Rows)
	}
	if r.RecordFrom == "" || r.From == "" || r.To == "" {
		t.Errorf("窗口与记录起点没给全：%q ~ %q，起点 %q", r.From, r.To, r.RecordFrom)
	}
	if len(r.People) != 2 {
		t.Fatalf("认出 %d 个人：%s", len(r.People), body)
	}
	online, gone := r.People[0], r.People[1]
	if !online.OnlineNow || online.Name != "阿栗" {
		t.Errorf("第一个泳道 = %+v，期望此刻在线的阿栗排在最前", online)
	}
	if len(online.Spans) != 1 {
		t.Fatalf("阿栗的区间 = %v", online.Spans)
	}
	if online.Spans[0].Reason != store.SpanEndOpen {
		t.Errorf("开口段的收尾 = %q，期望 open", online.Spans[0].Reason)
	}
	// 收尾那一方必须是一句人话：界面直接摆这个字段，露出 open 就坏了面板的规矩。
	if !strings.Contains(online.Spans[0].ReasonLabel, "还在里面") {
		t.Errorf("开口段的中文说明 = %q", online.Spans[0].ReasonLabel)
	}
	// 没有记号的人也要给空数组：前端读 marks.length，null 会整页崩（这亏吃过）。
	if !strings.Contains(body, `"marks":[]`) {
		t.Errorf("阿栗没有死亡记录，响应里却没有空数组：%s", body)
	}
	if _, err := time.Parse(time.RFC3339, online.LastAt); err != nil {
		t.Errorf("阿栗的 lastAt = %q 解不开", online.LastAt)
	}

	if gone.Name != "小满" || gone.OnlineNow {
		t.Fatalf("第二个泳道 = %+v，期望已经离开的小满", gone)
	}
	if gone.Seconds != 3600 {
		t.Errorf("小满在线 %d 秒，期望 3600（两小时进、一小时离）", gone.Seconds)
	}
	if len(gone.Spans) != 1 || gone.Spans[0].Reason != store.SpanEndLeave {
		t.Errorf("小满的区间 = %v，期望一段、停在 leave", gone.Spans)
	}
	if len(gone.Marks) != 1 || gone.Marks[0].Label == "" || gone.Marks[0].Label == gone.Marks[0].Event {
		t.Errorf("死亡记号 = %v，期望带中文名（不许露码值）", gone.Marks)
	}

	// 「记录从」是界面上那枚起点标签的说法，说明里指向它而不是自己报时刻。
	for _, want := range []string{"过去 24 小时里读到 4 次玩家行为", "记录从", "迁移"} {
		if !strings.Contains(r.Note, want) {
			t.Errorf("说明里缺 %q：%q", want, r.Note)
		}
	}
	// 这一句是整句上屏的，前端换不掉里面的时刻，所以它一个钟点都不许带：
	// Go 按 UTC 拼出来的 "11-03 06:41" 会和界面上其余按浏览器时区显示的时间差一截，
	// 而用户没有线索发现哪个才对。记录起点由界面那枚「记录从」本地化显示。
	if hasClock := regexp.MustCompile(`\d{2}-\d{2} \d{2}:\d{2}`); hasClock.MatchString(r.Note) {
		t.Errorf("说明里出现了服务端 UTC 钟点，界面会和其他时间打架：%q", r.Note)
	}
}

// TestPlayerTimelineEventCounts 钉的是总览右列那张「行为构成」。三件事必须成立：
//  1. 各类加起来正好是 rows——那是同一个去重口径算出来的分母，两张卡上的数不一致
//     迟早被读成"下面那张多算了"；
//  2. 同一条公告在两个世界各写一遍的只算一次（库里两行、算一次发生）；
//  3. 上屏的是中文短名，不是 player_join 这种码值。
func TestPlayerTimelineEventCounts(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	base := time.Now().UTC().Truncate(time.Minute)
	round := base.Add(-3 * time.Hour).UnixMilli()
	seedEvents(t, f.srv.Store, f.cluster,
		store.PlayerEventRow{At: base.Add(-2 * time.Hour), Round: round, Shard: "Master", UserID: "KU_a", Name: "小满", Event: gamelog.EventPlayerJoin},
		// 同一次进服，洞穴那一份只晚 20 秒：算一次发生。
		store.PlayerEventRow{At: base.Add(-2*time.Hour + 20*time.Second), Round: round, Shard: "Caves", UserID: "KU_a", Name: "小满", Event: gamelog.EventPlayerJoin},
		store.PlayerEventRow{At: base.Add(-100 * time.Minute), Round: round, Shard: "Master", UserID: "KU_a", Name: "小满", Event: gamelog.EventPlayerDeath},
		store.PlayerEventRow{At: base.Add(-100*time.Minute + 10*time.Second), Round: round, Shard: "Caves", UserID: "KU_a", Name: "小满", Event: gamelog.EventPlayerDeath},
		store.PlayerEventRow{At: base.Add(-1 * time.Hour), Round: round, Shard: "Master", UserID: "KU_a", Name: "小满", Event: gamelog.EventPlayerLeave},
		store.PlayerEventRow{At: base.Add(-30 * time.Minute), Round: round, Shard: "Master", UserID: "KU_b", Name: "阿栗", Event: gamelog.EventPlayerJoin},
		store.PlayerEventRow{At: base.Add(-20 * time.Minute), Round: round, Shard: "Master", UserID: "KU_b", Name: "阿栗", Event: gamelog.EventPlayerDeath},
		store.PlayerEventRow{At: base.Add(-10 * time.Minute), Round: round, Shard: "Master", UserID: "KU_b", Name: "阿栗", Event: gamelog.EventResurrect},
	)

	_, r, body := getTimeline(t, h, "cluster="+f.cluster+"&hours=24")
	if r.Rows != 6 {
		t.Fatalf("rows = %d，期望 6（库里 8 行、两份跨世界重复各算一次）：%s", r.Rows, body)
	}
	if len(r.Events) != 4 {
		t.Fatalf("行为构成 = %v，期望四类：%s", r.Events, body)
	}
	sum := 0
	for _, e := range r.Events {
		sum += e.Count
		if e.Name == e.Code || e.Name == "" {
			t.Errorf("%q 没有中文短名：%q", e.Code, e.Name)
		}
		if e.Label == "" || e.Label == e.Code {
			t.Errorf("%q 的整句解释没给：%q", e.Code, e.Label)
		}
	}
	if sum != r.Rows {
		t.Errorf("各类加起来 %d，和分母 %d 不一致", sum, r.Rows)
	}
	want := []struct {
		code  string
		count int
	}{
		{gamelog.EventPlayerJoin, 2}, {gamelog.EventPlayerDeath, 2},
		{gamelog.EventPlayerLeave, 1}, {gamelog.EventResurrect, 1},
	}
	for i, w := range want {
		if r.Events[i].Code != w.code || r.Events[i].Count != w.count {
			t.Errorf("第 %d 行 = %s %d，期望 %s %d（次数多的在前，一样多按行为目录）",
				i+1, r.Events[i].Code, r.Events[i].Count, w.code, w.count)
		}
	}
}

// TestPlayerTimelineEmptyRoom 是升级后的第一屏：这个房间一行都还没攒。
// 这时候图是空的，但那句话必须说清"更早的历史真的不存在"，
// 否则一张只有今天下午的图会被读成"这房间一直就这么点人"。
func TestPlayerTimelineEmptyRoom(t *testing.T) {
	f := newFixture(t, "")
	code, r, body := getTimeline(t, f.srv.Handler(), "cluster="+f.cluster+"&hours=24")
	if code != http.StatusOK {
		t.Fatalf("状态 = %d，body %s", code, body)
	}
	if r.Rows != 0 || len(r.People) != 0 || r.RecordFrom != "" {
		t.Errorf("空房间给回了 rows=%d people=%d 起点=%q", r.Rows, len(r.People), r.RecordFrom)
	}
	if !strings.Contains(body, `"people":[]`) {
		t.Errorf("空房间要回空数组而不是 null：%s", body)
	}
	// 行为构成那一行同理：前端读 events.length，null 会让整页崩（这亏吃过）。
	if !strings.Contains(body, `"events":[]`) {
		t.Errorf("空房间的行为构成要回空数组而不是 null：%s", body)
	}
	if !strings.Contains(r.Note, "一条玩家行为都还没攒下") || !strings.Contains(r.Note, "不是面板漏读") {
		t.Errorf("空房间那句说明没交代清楚：%q", r.Note)
	}
}

// TestPlayerTimelineWindowOlderThanRecords 是这一窗没数据、库里有更早的那一屏：
// 近 6 小时没人进出，三天前有一晚玩得很久。这时候不能说一条都还没攒下（那会把
// 已经攒下的说没了），也不能报一个服务端 UTC 钟点（界面其余时间都是浏览器所在时区）。
func TestPlayerTimelineWindowOlderThanRecords(t *testing.T) {
	f := newFixture(t, "")
	base := time.Now().UTC().Truncate(time.Minute)
	round := base.Add(-72 * time.Hour).UnixMilli()
	seedEvents(t, f.srv.Store, f.cluster,
		store.PlayerEventRow{At: base.Add(-72 * time.Hour), Round: round, UserID: "KU_a", Name: "小满", Event: gamelog.EventPlayerJoin},
		store.PlayerEventRow{At: base.Add(-71 * time.Hour), Round: round, UserID: "KU_a", Name: "小满", Event: gamelog.EventPlayerLeave},
	)
	_, r, body := getTimeline(t, f.srv.Handler(), "cluster="+f.cluster+"&hours=6")
	if r.Rows != 0 || len(r.People) != 0 {
		t.Fatalf("近 6 小时该是空的：%s", body)
	}
	if r.RecordFrom == "" {
		t.Errorf("库里有更早的记录，起点不能空着：%s", body)
	}
	if !strings.Contains(r.Note, "比这一窗更早") || !strings.Contains(r.Note, "记录从") {
		t.Errorf("这句话要说清窗口内没有、但库里有更早的：%q", r.Note)
	}
	if hasClock := regexp.MustCompile(`\d{2}-\d{2} \d{2}:\d{2}`); hasClock.MatchString(r.Note) {
		t.Errorf("说明里带了服务端 UTC 钟点：%q", r.Note)
	}
}

func TestPlayerTimelineRejectsBadParams(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	cases := []struct {
		query       string
		wantCode    int
		wantContain string
		why         string
	}{
		{"hours=24", http.StatusBadRequest, "必须指定 cluster", "不带 cluster"},
		{"cluster=" + f.cluster + "&hours=0", http.StatusBadRequest, "1~2160", "hours=0"},
		{"cluster=" + f.cluster + "&hours=2161", http.StatusBadRequest, "1~2160", "hours 超上限"},
		{"cluster=" + f.cluster + "&hours=abc", http.StatusBadRequest, "1~2160", "hours 不是数字"},
		{"cluster=" + f.cluster, http.StatusOK, "", "不带 hours 时用默认窗口"},
	}
	for _, c := range cases {
		rec := do(t, h, http.MethodGet, "/api/v1/player-timeline?"+c.query, "admin", "secret")
		if rec.Code != c.wantCode {
			t.Errorf("%s = %d，期望 %d：%s", c.why, rec.Code, c.wantCode, rec.Body.String())
			continue
		}
		if c.wantContain != "" && !strings.Contains(rec.Body.String(), c.wantContain) {
			t.Errorf("%s 的拒语没报出该说的内容：%s", c.why, rec.Body.String())
		}
		if c.wantCode == http.StatusOK {
			var r tlReply
			decodeData(t, rec, &r)
			if r.Hours != 24 {
				t.Errorf("默认窗口 = %d 小时，期望 24", r.Hours)
			}
		}
	}
}

// TestMemberCannotReadOtherRoomTimeline：时间线给的是"谁几点进、几点死、几点走"，
// 属于房间里最隐私的那类数据。成员手敲别人房间的名字也必须拒。
func TestMemberCannotReadOtherRoomTimeline(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	f.addRoom(t, "Cluster_2")
	alice := newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	bob := newAccount(t, h, "bob", "bob-pass-1", store.RoleMember)
	if rec := assign(t, h, "Cluster_1", alice.ID); rec.Code != http.StatusOK {
		t.Fatalf("指派 = %d，body %s", rec.Code, rec.Body.String())
	}
	// 没指派的那一间成员本来就看得见（归属制的规则），所以"拒"的现场必须派给别人。
	if rec := assign(t, h, "Cluster_2", bob.ID); rec.Code != http.StatusOK {
		t.Fatalf("指派 Cluster_2 = %d，body %s", rec.Code, rec.Body.String())
	}

	rec := do(t, h, http.MethodGet, "/api/v1/player-timeline?cluster=Cluster_2", "alice", "alice-pass-1")
	if rec.Code != http.StatusForbidden {
		t.Errorf("成员读别人房间的时间线 = %d，期望 403，body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "bob") {
		t.Errorf("拒语没说出这一间归谁管，用户下一步不知道该找谁：%s", body)
	}
	own := do(t, h, http.MethodGet, "/api/v1/player-timeline?cluster=Cluster_1", "alice", "alice-pass-1")
	if own.Code == http.StatusForbidden {
		t.Errorf("成员读自己名下的房间被权限层挡了：%s", own.Body.String())
	}
}

// TestTimelineNoteSaysWhichPeopleAreAnonymous 钉那句诚实说明的分寸：
// 面板会拿花名册把公告里的名字接上账户编号，接得上的人改名不会裂成两条，
// 所以"只能按名字认"这句只在真接不上的时候才许说，而且要说清是几个人。
func TestTimelineNoteSaysWhichPeopleAreAnonymous(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	base := time.Now().UTC().Truncate(time.Minute)
	round := base.Add(-2 * time.Hour).UnixMilli()
	seedEvents(t, f.srv.Store, f.cluster,
		store.PlayerEventRow{At: base.Add(-90 * time.Minute), Round: round, Name: "小满", UserID: "KU_a", Event: gamelog.EventPlayerJoin},
		store.PlayerEventRow{At: base.Add(-60 * time.Minute), Round: round, Name: "路人甲", Event: gamelog.EventPlayerJoin},
		store.PlayerEventRow{At: base.Add(-30 * time.Minute), Round: round, Name: "路人乙", Event: gamelog.EventPlayerJoin},
	)
	_, r, body := getTimeline(t, h, "cluster="+f.cluster+"&hours=24")
	if len(r.People) != 3 {
		t.Fatalf("认出 %d 个人：%s", len(r.People), body)
	}
	if !strings.Contains(r.Note, "有 2 个人只带显示名") {
		t.Errorf("接不上号的是两个而不是三个，说明却可能写成全体：%q", r.Note)
	}
	if strings.Contains(r.Note, "都只按显示名认") {
		t.Errorf("小满明明有账户编号，说明不能说全体按名字认：%q", r.Note)
	}

	// 全体都接不上的那种（这一轮没读到账号，比如助手还没回过话）要换成另一句。
	f2 := newFixture(t, "")
	base2 := time.Now().UTC().Truncate(time.Minute)
	seedEvents(t, f2.srv.Store, f2.cluster,
		store.PlayerEventRow{At: base2.Add(-time.Hour), Round: base2.Add(-2 * time.Hour).UnixMilli(), Name: "路人甲", Event: gamelog.EventPlayerJoin},
	)
	_, r2, _ := getTimeline(t, f2.srv.Handler(), "cluster="+f2.cluster+"&hours=24")
	if !strings.Contains(r2.Note, "都只按显示名认") {
		t.Errorf("全体接不上号时没换成那一句：%q", r2.Note)
	}
}
