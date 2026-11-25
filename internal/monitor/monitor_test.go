package monitor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dstpanel/internal/config"
	"dstpanel/internal/dst"
	"dstpanel/internal/modbridge"
	"dstpanel/internal/store"
)

type fixture struct {
	s       *Sampler
	paths   dst.Paths
	cluster string
	tmp     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	paths := dst.Paths{KleiRoot: filepath.Join(root, "klei"), DstInstall: filepath.Join(root, "dst")}
	st, err := store.Open(filepath.Join(root, "panel", "dstpanel.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cluster := "Cluster_1"
	for _, shard := range []string{"Master", "Caves", "Broken"} {
		if err := os.MkdirAll(filepath.Join(paths.KleiRoot, cluster, shard), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths.ClusterIni(cluster), []byte("[GAME]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fixture{s: &Sampler{
		Paths:  paths,
		Bridge: modbridge.New(paths, config.Helper{PollMs: 1000, StaleSeconds: 15}),
		Store:  st,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, paths: paths, cluster: cluster, tmp: root}
	return f
}

// writeStatus 落一份助手快照。mtime 就是面板判断数据新不新的唯一依据
// （Lua 侧只有 os.clock，跨不了进程），所以过期用例靠改时间来做。
func (f *fixture) writeStatus(t *testing.T, shard, body string, mt time.Time) {
	t.Helper()
	path := f.paths.HelperStatusFile(f.cluster, shard)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mt.IsZero() {
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
}

// statusBody 拼一份助手快照。天数传的是 cycles（引擎上报的就是"过完了几个整天"），
// 库里那条 day 由面板换算出 cycles+1 —— 传 11 表示界面该说第 12 天。
func statusBody(shard string, cycles float64, season string, online int, players []map[string]any) string {
	raw, err := json.Marshal(map[string]any{
		"shard": shard, "player_count": online, "max_players": 10,
		"world":   map[string]any{"cycles": cycles, "season": season},
		"players": players,
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestSampleOnceSkipsUntrustedSnapshots(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	f.writeStatus(t, "Master", statusBody("Master", 11, "winter", 2,
		[]map[string]any{{"name": "阿强", "is_dead": false}, {"name": "小李", "is_dead": true}}), now)
	f.writeStatus(t, "Caves", statusBody("Caves", 11, "winter", 1, nil), now.Add(-5*time.Minute))
	f.writeStatus(t, "Broken", `{"shard":"Broken","player_count":3,"error":"采集炸了","world":{"cycles":0}}`, now)

	if n := f.s.SampleOnce(); n != 1 {
		t.Fatalf("只该存下 Master 这一条可信快照，实际存了 %d 条", n)
	}
	rows, err := f.s.Store.Snapshots(f.cluster, "", now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Shard != "Master" {
		t.Fatalf("库里内容不对: %+v", rows)
	}
	sn := rows[0]
	if sn.Day != 12 || sn.Online != 2 || sn.Season != "winter" {
		t.Errorf("快照字段没对上: %+v", sn)
	}
	// detail 只留活人：趋势图点开看"当时谁在线"，列表里躺个已重生的死者名字会误导。
	if sn.Detail != `["阿强"]` {
		t.Errorf("detail 应该只含在线未死的玩家，实际 %q", sn.Detail)
	}
}

func TestSampleOnceToleratesNoSnapshotAtAll(t *testing.T) {
	f := newFixture(t)
	// 一个新装好、还没进过世界的集群：一个文件都没有，采样必须安静地存 0 条而不是报错。
	if n := f.s.SampleOnce(); n != 0 {
		t.Fatalf("没快照时应该存 0 条，实际 %d", n)
	}
}

func TestCleanupOnceHonoursRetentionSettings(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	for _, sn := range []store.Snapshot{
		{At: old, Cluster: f.cluster, Shard: "Master"},
		{At: now, Cluster: f.cluster, Shard: "Master"},
	} {
		if err := f.s.Store.RecordSnapshot(sn); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.Store.AppendAudit(store.AuditEntry{At: old, Actor: "admin", Action: "mod.enable", Target: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Store.RecordPlayerEvents([]store.PlayerEventRow{
		{At: old, Round: old.UnixMilli(), Cluster: f.cluster, Shard: "Master",
			Name: "老玩家", Event: "player_join"},
		{At: now, Round: now.UnixMilli(), Cluster: f.cluster, Shard: "Master",
			Name: "新玩家", Event: "player_join"},
	}); err != nil {
		t.Fatal(err)
	}

	f.s.Now = func() time.Time { return now }
	if err := f.s.Store.SetSetting(store.KeySnapshotRetentionDays, "30"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Store.SetSetting(store.KeyAuditRetentionDays, "90"); err != nil {
		t.Fatal(err)
	}
	rep, err := f.s.CleanupOnce()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Snapshots != 1 || rep.Audit != 0 {
		t.Errorf("快照该清 1 条、90 天内的操作记录该留着，实际 %d / %d", rep.Snapshots, rep.Audit)
	}
	// 玩家行为默认留 30 天，和在线快照同一档：那条 40 天前的进服早该没了。
	if rep.PlayerEvents != 1 {
		t.Errorf("40 天前的玩家行为该被清掉，实际清了 %d 条", rep.PlayerEvents)
	}
	if span, err := f.s.Store.PlayerEventSpan(f.cluster); err != nil {
		t.Fatal(err)
	} else if span.Rows != 1 {
		t.Errorf("清完该只剩最近那一条，实际 %d 条", span.Rows)
	}

	// 保留天数设成 0 是"永久保留"的明确意思表示，不能理解成"全部删光"。
	if err := f.s.Store.SetSetting(store.KeySnapshotRetentionDays, "0"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Store.RecordSnapshot(store.Snapshot{At: now.Add(-500 * 24 * time.Hour), Cluster: f.cluster, Shard: "Master"}); err != nil {
		t.Fatal(err)
	}
	rep, err = f.s.CleanupOnce()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Snapshots != 0 {
		t.Errorf("设成 0 天还删了 %d 条", rep.Snapshots)
	}
	rows, _ := f.s.Store.Snapshots(f.cluster, "", now.Add(-900*24*time.Hour), 100)
	if len(rows) != 2 {
		t.Errorf("永久保留就该两条都在，实际 %d 条", len(rows))
	}
}

// Run 必须在 ctx 结束时返回：main 里等它收尾最多 5 秒，回不来就是退出时卡死。
func TestRunReturnsOnContextCancel(t *testing.T) {
	f := newFixture(t)
	f.s.Interval = 20 * time.Millisecond
	f.s.CleanupEvery = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.s.Run(ctx)
	}()
	time.Sleep(80 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 没有随 ctx 结束而返回")
	}
	rows, err := f.s.Store.Snapshots(f.cluster, "", time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Logf("这个用例本来不写快照，出现 %d 行说明有分片被当成可信数据存了", len(rows))
	}
}
