package modbridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/config"
	"dstpanel/internal/dst"
)

// fakeMod 是 Go 侧对 panel_bridge.lua 的模仿：把 MOD 该写的三个文件写出来。
// 用它测面板这一半，真正端到端要等云上起服（任务 #9）。
type fakeMod struct {
	t     *testing.T
	paths dst.Paths
	now   time.Time
}

func newFixture(t *testing.T) (*Bridge, *fakeMod) {
	t.Helper()
	clock := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	frozen := clock
	paths := dst.Paths{
		KleiRoot:   filepath.Join(t.TempDir(), "klei"),
		DstInstall: filepath.Join(t.TempDir(), "dst"),
	}
	for _, sh := range []string{dst.ShardMaster, dst.ShardCaves} {
		if err := os.MkdirAll(paths.HelperDir("Cluster_1", sh), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b := New(paths, config.Helper{PollMs: 1000, StaleSeconds: 15})
	b.Now = func() time.Time { return frozen }
	f := &fakeMod{t: t, paths: paths, now: clock}
	return b, f
}

// consume 把队列里的命令全部标记为已执行，并给 ping 写回执。
func (f *fakeMod) consume(cluster, shard string) []string {
	f.t.Helper()
	raw, err := os.ReadFile(f.paths.HelperCommandQueue(cluster, shard))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		f.t.Fatal(err)
	}
	var ids []string
	var processed []string
	var results []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		id := parts[0]
		ids = append(ids, id)
		processed = append(processed, id)
		cmd := ""
		if len(parts) > 1 {
			cmd = parts[1]
		}
		switch cmd {
		case CmdPing:
			results = append(results, fmt.Sprintf("%q:{\"ok\":true,\"shard\":%q,\"value\":%q}", id, shard, "pong"))
		case CmdAnnounce:
			text := strings.Join(parts[2:], "|")
			results = append(results, fmt.Sprintf("%q:{\"ok\":true,\"value\":%q}", id, text))
		case CmdKick:
			results = append(results, fmt.Sprintf("%q:{\"ok\":true,\"value\":{\"kicked\":%q,\"reason\":%q}}", id, parts[2], strings.Join(parts[3:], "|")))
		case CmdStatus:
			results = append(results, fmt.Sprintf("%q:{\"ok\":true,\"value\":{\"seq\":7,\"shard\":%q}}", id, shard))
		default:
			results = append(results, fmt.Sprintf("%q:{\"error\":%q,\"ok\":false}", id, "未知命令"))
		}
	}
	if len(processed) > 0 {
		f.writeFile(cluster, shard, f.paths.HelperProcessedFile(cluster, shard), strings.Join(processed, "\n"))
		f.writeFile(cluster, shard, f.paths.HelperResultFile(cluster, shard), "{"+strings.Join(results, ",")+"}")
	}
	return ids
}

func (f *fakeMod) writeFile(cluster, shard, path, body string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeMod) writeStatus(cluster, shard, json string) string {
	path := f.paths.HelperStatusFile(cluster, shard)
	f.writeFile(cluster, shard, path, json)
	// 新鲜度是按 mtime 判的，把 mtime 钉在测试时钟上，用例才不用依赖真实时间。
	if err := os.Chtimes(path, f.now, f.now); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func queueOf(t *testing.T, b *Bridge, cluster, shard string) []string {
	t.Helper()
	raw, err := os.ReadFile(b.Paths.HelperCommandQueue(cluster, shard))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// 云机 v760944 实测：TheSim:SetPersistentString 写出来的文件开头都带一句文本头
// "KLEI     1 "（xxd 对过，11 个字节），Lua 自己读的时候 GetPersistentString 会剥掉，
// 面板用普通 os.ReadFile 就必须在解析前剥掉，否则 status/result/processed 三条读路径
// 全线报"不是合法 JSON"——游戏跑得好好的，界面却全红。
const kleiFileHeader = "KLEI     1 "

func TestReadsStripKleiPersistentHeader(t *testing.T) {
	b, f := newFixture(t)
	status := `{"game_id":"4C88CF1E3E97EAD9","heartbeat":55.3065,"max_players":6,` +
		`"player_count":0,"players":[],"seq":172,"server_name":"首启验证服",` +
		`"shard":"Master","world":{"cycles":0,"phase":"day","season":"autumn","temperature":32.0126,"wetness":0}}`
	f.writeStatus("Cluster_1", dst.ShardMaster, kleiFileHeader+status)
	f.writeFile("Cluster_1", dst.ShardMaster, f.paths.HelperResultFile("Cluster_1", dst.ShardMaster),
		kleiFileHeader+`{"9001":{"ok":true,"shard":"Master","value":"pong"}}`)
	f.writeFile("Cluster_1", dst.ShardMaster, f.paths.HelperProcessedFile("Cluster_1", dst.ShardMaster),
		kleiFileHeader+"9001")
	f.writeFile("Cluster_1", dst.ShardMaster, f.paths.HelperCommandQueue("Cluster_1", dst.ShardMaster),
		"9001|ping\n")

	st, err := b.Status("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatalf("带引擎头的快照读失败: %v", err)
	}
	if st.Seq != 172 || st.MaxPlayers != 6 || st.ServerName != "首启验证服" || st.World.Phase != "day" {
		t.Fatalf("快照解错了: %+v", st)
	}
	if st.Stale {
		t.Errorf("快照被判成过期了: age=%s stale=%v", st.Age, st.Stale)
	}

	res, ok, err := b.Result("Cluster_1", dst.ShardMaster, "9001")
	if err != nil {
		t.Fatalf("带引擎头的回执读失败: %v", err)
	}
	if !ok || !res.OK {
		t.Fatalf("回执 = %+v (found=%v)，期望 ok=true", res, ok)
	}
	var pong string
	if err := res.ValueInto(&pong); err != nil || pong != "pong" {
		t.Fatalf("value = %q, err=%v，期望 pong", pong, err)
	}

	// processed 剥不掉头的话这条 id 就永远匹配不上，队列只涨不缩。
	removed, err := b.Trim("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatalf("裁剪队列: %v", err)
	}
	if removed != 1 {
		t.Fatalf("裁掉 %d 条，期望 1 条（队列里 9001 已被 processed 标记）", removed)
	}
}

// 头只在 MOD 写出的文件上；面板自己写的 queue 没有头，别把它首行的数字当成版本号吃掉。
func TestReadsLeavePlainPanelFilesAlone(t *testing.T) {
	b, f := newFixture(t)
	f.writeFile("Cluster_1", dst.ShardMaster, f.paths.HelperCommandQueue("Cluster_1", dst.ShardMaster),
		"12|ping\n13|announce|开会\n")
	pending, err := b.Pending("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0] != "12|ping" {
		t.Fatalf("队列 = %q，期望原样两条", pending)
	}
}

func TestEnqueueAppendsAndValidates(t *testing.T) {
	b, _ := newFixture(t)

	id1, err := b.Enqueue("Cluster_1", dst.ShardMaster, CmdAnnounce, "开服半小时，来的进")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := b.Enqueue("Cluster_1", dst.ShardMaster, CmdKick, "阿伟", "刷物品")
	if err != nil {
		t.Fatal(err)
	}
	q := queueOf(t, b, "Cluster_1", dst.ShardMaster)
	if len(q) != 2 {
		t.Fatalf("队列应有两条: %+v", q)
	}
	if !strings.HasPrefix(q[0], id1+"|announce|") || !strings.HasPrefix(q[1], id2+"|kick|阿伟|") {
		t.Errorf("行格式不对: %+v", q)
	}
	if id1 == id2 {
		t.Error("同一毫秒内的两条命令撞了 id")
	}

	// 参数校验
	if _, err := b.Enqueue("Cluster_1", dst.ShardMaster, CmdAnnounce, "换行\n破坏协议"); err == nil {
		t.Error("含换行的参数应该拒绝")
	}
	if _, err := b.Enqueue("Cluster_1", dst.ShardMaster, CmdAnnounce, strings.Repeat("字", 600)); err == nil {
		t.Error("超长参数应该拒绝")
	}
	if _, err := b.Enqueue("Cluster_1", dst.ShardMaster, "shutdown", ""); err == nil {
		t.Error("白名单外的桥命令应该拒绝")
	}
	if n := len(queueOf(t, b, "Cluster_1", dst.ShardMaster)); n != 2 {
		t.Errorf("被拒绝的命令写进队列了: %d 条", n)
	}
}

func TestEnqueueStopsWhenHelperIsDead(t *testing.T) {
	b, _ := newFixture(t)
	b.QueueLimit = 3
	for i := 0; i < 3; i++ {
		if _, err := b.Enqueue("Cluster_1", dst.ShardMaster, CmdPing); err != nil {
			t.Fatalf("第 %d 条不该被拒: %v", i, err)
		}
	}
	_, err := b.Enqueue("Cluster_1", dst.ShardMaster, CmdPing)
	if err == nil {
		t.Fatal("队列满了还接受命令，等于把过期操作攒着等有谁回来一次性执行")
	}
	if !strings.Contains(err.Error(), "助手 MOD 没有消费") {
		t.Errorf("报错没说清楚原因: %v", err)
	}
}

func TestPendingAndTrim(t *testing.T) {
	b, mod := newFixture(t)
	first, _ := b.Enqueue("Cluster_1", dst.ShardMaster, CmdPing)
	second, _ := b.Enqueue("Cluster_1", dst.ShardMaster, CmdAnnounce, "公告内容")

	// MOD 只标记了第一条，第二条要留着
	if err := os.WriteFile(b.Paths.HelperProcessedFile("Cluster_1", dst.ShardMaster), []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	pending, err := b.Pending("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !strings.HasPrefix(pending[0], second+"|") {
		t.Errorf("Pending 只该列出未执行的命令: %+v", pending)
	}
	n, err := b.Trim("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应该只裁掉已执行的 1 条，实际 %d", n)
	}
	q := queueOf(t, b, "Cluster_1", dst.ShardMaster)
	if len(q) != 1 || !strings.HasPrefix(q[0], second+"|") {
		t.Errorf("裁错了，把没执行的裁掉了: %+v", q)
	}
	if n, err := b.Trim("Cluster_1", dst.ShardMaster); err != nil || n != 0 {
		t.Errorf("重复裁剪不该有动作: %d %v", n, err)
	}

	// MOD 消费剩下的，再裁一次，队列变空文件而不是残留脏行
	mod.consume("Cluster_1", dst.ShardMaster)
	if n, err := b.Trim("Cluster_1", dst.ShardMaster); err != nil || n != 1 {
		t.Errorf("第二次裁剪应删掉 1 条: %d %v", n, err)
	}
	if q := queueOf(t, b, "Cluster_1", dst.ShardMaster); len(q) != 0 {
		t.Errorf("队列没清空: %+v", q)
	}
	// 文件不存在时分片也不能报错
	if _, err := b.Pending("Cluster_1", dst.ShardCaves); err != nil {
		t.Errorf("洞穴从没写过队列不该报错: %v", err)
	}
}

func TestWaitResultsAndPing(t *testing.T) {
	b, mod := newFixture(t)
	ctx := context.Background()
	// 这条用例要等真实超时，所以放掉测试时钟；其余用例用固定时钟保证不靠 sleep 判定。
	b.Now = nil

	id, err := b.Enqueue("Cluster_1", dst.ShardMaster, CmdAnnounce, "有玩家|在刷屏")
	if err != nil {
		t.Fatal(err)
	}
	// MOD 还没读队列时，Wait 要在超时后给出可行动的错误
	if _, err := b.Wait(ctx, "Cluster_1", dst.ShardMaster, id, 300*time.Millisecond); err == nil {
		t.Fatal("没有回执不该返回成功")
	} else if !errors.Is(err, ErrHelperOffline) {
		t.Errorf("超时应归因到助手 MOD 掉线: %v", err)
	}

	mod.consume("Cluster_1", dst.ShardMaster)
	res, err := b.Wait(ctx, "Cluster_1", dst.ShardMaster, id, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.ID != id {
		t.Errorf("回执不对: %+v", res)
	}
	var echoed string
	if err := res.ValueInto(&echoed); err != nil {
		t.Fatal(err)
	}
	// announce 参数被竖线拆开过，MOD 侧用 table.concat(args,"|") 拼回来，这里必须无损
	if echoed != "有玩家|在刷屏" {
		t.Errorf("公告文本被竖线截断: %q", echoed)
	}

	if _, err := b.Wait(ctx, "Cluster_1", dst.ShardMaster, "不存在的id", 100*time.Millisecond); err == nil {
		t.Error("查不到的 id 不该成功")
	}
}

func TestPingRoundTrip(t *testing.T) {
	b, mod := newFixture(t)
	// Ping 会一直等到回执出现；先在另一个 goroutine 里扮演 MOD。
	done := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		mod.consume("Cluster_1", dst.ShardMaster)
		close(done)
	}()
	pong, err := b.Ping(context.Background(), "Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if pong != "pong" {
		t.Errorf("pong 内容不对: %q", pong)
	}
}

// 这份 JSON 按 panel_json.lua 的实际输出形状手写：键按字典序、缺字段直接不出现、
// 空玩家列表是 []。用真实形状测，才不会测出一个只符合我想象的正确。
const realStatus = `{
  "game_id": "3AD15F911E034B3B",
  "heartbeat": 12.34,
  "max_players": 6,
  "player_count": 2,
  "players": [
    {"max_health": 150, "health": 118.5, "is_dead": false, "max_hunger": 150, "hunger": 92, "name": "阿伟", "prefab": "wilson", "userid": "KU_abcdef123456", "x": -120, "z": 88, "items": 12},
    {"health": 0, "max_health": 100, "is_dead": true, "name": "小美", "prefab": "wendy", "userid": "KU_zzz999"}
  ],
  "seq": 4212,
  "server_name": "周末联机房",
  "shard": "Master",
  "world": {"cycles": 5.5, "phase": "dusk", "season": "winter", "temperature": -8.2, "wetness": 0}
}`

func TestStatusParsing(t *testing.T) {
	b, mod := newFixture(t)
	path := mod.writeStatus("Cluster_1", dst.ShardMaster, realStatus)

	st, err := b.Status("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if st.ServerName != "周末联机房" || st.GameID != "3AD15F911E034B3B" || st.MaxPlayers != 6 {
		t.Errorf("基本字段不对: %+v", st)
	}
	if st.World.SeasonLabel() != "冬天" || st.World.PhaseLabel() != "黄昏" {
		t.Errorf("季节/时段没翻成中文: %s %s", st.World.SeasonLabel(), st.World.PhaseLabel())
	}
	// 引擎不上报 day，天数是这里从 cycles 换算的：过完了 5 个整天就是第 6 天。
	// 这一条钉住的是"界面上别再出现第 0 天"——那个 0 曾经是每个房间都中招的假数。
	if st.World.DayNumber() != 6 {
		t.Errorf("第几天 = %v，cycles=5.5 期望换算成 6", st.World.DayNumber())
	}
	if len(st.Players) != 2 || st.Players[0].Name != "阿伟" {
		t.Fatalf("玩家列表不对: %+v", st.Players)
	}
	p := st.Players[0]
	if p.Health == nil || *p.Health != 118.5 || p.X == nil || *p.X != -120 {
		t.Errorf("数值解析不对: %+v", p)
	}
	// 第二个玩家没有 hunger 键：必须保持 nil，不能显示成 0
	if st.Players[1].Hunger != nil {
		t.Errorf("缺失字段被编成了 0: %v", *st.Players[1].Hunger)
	}
	if !st.Players[1].IsDead {
		t.Error("死亡状态没解析出来")
	}
	if st.Stale {
		t.Error("刚写的文件不该判为过期")
	}

	// 把 mtime 推到测试时钟的 20 秒前（超过 StaleSeconds=15），就该报过期
	old := mod.now.Add(-20 * time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	st, err = b.Status("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Stale || st.Age < 19*time.Second {
		t.Errorf("过期没判定出来: stale=%v age=%v", st.Stale, st.Age)
	}
}

// 这一季第几天的判据都收在这里，界面上那句"秋天（第 6 天）"完全跟着第二个返回值走。
// 四种情况分别对着：常规轮换、老版助手没报这两个读数、无尽模式（还剩几天报的是哨兵值）、
// 固定季节（过完的天数一路涨过任何真实季节长度）。后三种都不是"第 1 天"，而是没得说。
func TestSeasonDayCounting(t *testing.T) {
	num := func(v float64) *float64 { return &v }

	cases := []struct {
		name                string
		world               World
		wantDay             float64
		wantDayOK, wantLeft bool
	}{
		{name: "常规：过完 5 天就是第 6 天，还剩 14 天",
			world:   World{ElapsedDaysInSeason: num(5), RemainingDaysInSeason: num(14)},
			wantDay: 6, wantDayOK: true, wantLeft: true},
		{name: "开局：过完 0 天是第 1 天，不是第 0 天",
			world:   World{ElapsedDaysInSeason: num(0), RemainingDaysInSeason: num(19)},
			wantDay: 1, wantDayOK: true, wantLeft: true},
		{name: "老版助手没报读数",
			world: World{}, wantDayOK: false, wantLeft: false},
		{name: "无尽模式的哨兵值",
			world:     World{ElapsedDaysInSeason: num(3), RemainingDaysInSeason: num(10000)},
			wantDayOK: false, wantLeft: false},
		{name: "季节不轮换，天数涨过了头",
			world:     World{ElapsedDaysInSeason: num(400), RemainingDaysInSeason: num(1)},
			wantDayOK: false, wantLeft: false},
	}
	for _, c := range cases {
		day, ok := c.world.DayInSeason()
		if ok != c.wantDayOK || (ok && day != c.wantDay) {
			t.Errorf("%s: DayInSeason = %v/%v，期望 %v/%v", c.name, day, ok, c.wantDay, c.wantDayOK)
		}
		left, lok := c.world.DaysLeftInSeason()
		if lok != c.wantLeft {
			t.Errorf("%s: DaysLeftInSeason 可得性 = %v，期望 %v（值 %v）", c.name, lok, c.wantLeft, left)
		}
	}
}

func TestStatusBrokenShapes(t *testing.T) {
	b, mod := newFixture(t)

	if _, err := b.Status("Cluster_1", dst.ShardCaves); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("没生成快照应报文件不存在: %v", err)
	}
	mod.writeStatus("Cluster_1", dst.ShardCaves, "   \n")
	if _, err := b.Status("Cluster_1", dst.ShardCaves); err == nil ||
		!strings.Contains(err.Error(), "空的") {
		t.Errorf("空文件要说清楚: %v", err)
	}
	mod.writeStatus("Cluster_1", dst.ShardCaves, `{"shard":"Caves", "seq":`)
	if _, err := b.Status("Cluster_1", dst.ShardCaves); err == nil ||
		!strings.Contains(err.Error(), "不是合法 JSON") {
		t.Errorf("半截 JSON 要说清楚: %v", err)
	}
	// MOD 采集失败时会写 error 字段，快照仍然可读
	mod.writeStatus("Cluster_1", dst.ShardCaves, `{"error":"boom","shard":"Caves"}`)
	st, err := b.Status("Cluster_1", dst.ShardCaves)
	if err != nil {
		t.Fatal(err)
	}
	if st.Error != "boom" {
		t.Errorf("采集异常没带出来: %+v", st)
	}
	// StatusOrStale 永远给得出结构
	if got := b.StatusOrStale("Cluster_9", dst.ShardMaster); got == nil || !got.Stale {
		t.Error("缺文件时 StatusOrStale 也该给出过期结构")
	}
}

func TestWaitContextCancel(t *testing.T) {
	b, _ := newFixture(t)
	id, _ := b.Enqueue("Cluster_1", dst.ShardMaster, CmdPing)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Wait(ctx, "Cluster_1", dst.ShardMaster, id, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("取消要立刻返回: %v", err)
	}
}

func TestCommandNamesOrderAndLabels(t *testing.T) {
	names := CommandNames()
	if len(names) != 9 || names[0] != CmdPing || names[1] != CmdStatus ||
		names[2] != CmdMap || names[3] != CmdAnnounce || names[4] != CmdKick || names[8] != CmdUnban {
		t.Errorf("桥命令顺序不对: %+v", names)
	}
	for _, n := range names {
		if CommandLabel(n) == n {
			t.Errorf("%s 没有中文标签", n)
		}
	}
	if CommandLabel("unknown_cmd") != "unknown_cmd" {
		t.Error("未知命令应退回原值")
	}
}

func TestLabelFallback(t *testing.T) {
	if got := Label(seasonLabels, ""); got != "未知" {
		t.Errorf("空值要给占位: %q", got)
	}
	if got := Label(phaseLabels, "unknown_phase"); got != "unknown_phase" {
		t.Errorf("查不到应退回原值: %q", got)
	}
}
