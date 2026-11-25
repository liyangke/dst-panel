package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "dstpanel.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestOpenCreatesWALAndIsIdempotent 盯的是 DSN 真的生效：
// 如果 journal_mode 没起来，面板写日志的同时读接口会整段被锁住，
// 而那只有在真并发下才复现，所以在这里直接把 pragma 读出来断言。
func TestOpenCreatesWALAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dstpanel.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("第一次 Open: %v", err)
	}
	if _, err := s.CreateTask("mod_install", "安装模组：显示模式", nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 第二次 Open 必须复用同一个库，migrate 里的 IF NOT EXISTS 不能把数据抹了。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("第二次 Open: %v", err)
	}
	defer s2.Close()

	var mode string
	if err := s2.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("读 journal_mode: %v", err)
	}
	if strings.ToLower(mode) != "wal" {
		t.Errorf("journal_mode = %q，期望 wal", mode)
	}
	tasks, err := s2.ListTasks(10)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("重开后任务数 = %d，期望 1（建表把数据带走了）", len(tasks))
	}
}

func TestTaskLifecycleFIFOAndAttempts(t *testing.T) {
	s := newStore(t)

	first, err := s.CreateTask("mod_install", "第一个", map[string]any{"id": "workshop-1"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	second, err := s.CreateTask("backup", "第二个", nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// 领取必须按 id 升序，也就是先提交先执行。
	got, err := s.ClaimTask()
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if got.ID != first {
		t.Fatalf("领到 %d，期望先提交的 %d", got.ID, first)
	}
	if got.Status != TaskRunning || got.Attempts != 1 {
		t.Errorf("领取后 status=%s attempts=%d，期望 running/1", got.Status, got.Attempts)
	}
	if got.StartedAt == nil || got.FinishedAt != nil {
		t.Errorf("时间戳不对：started=%v finished=%v", got.StartedAt, got.FinishedAt)
	}

	// payload 回读：创建时传结构体，领取时得能原样解出来。
	var payload struct{ ID string }
	if err := got.PayloadInto(&payload); err != nil {
		t.Fatalf("PayloadInto: %v", err)
	}
	if payload.ID != "workshop-1" {
		t.Errorf("payload = %+v，期望 workshop-1", payload)
	}

	// 队列里还剩第二条，这时候不该报空。
	got2, err := s.ClaimTask()
	if err != nil {
		t.Fatalf("ClaimTask 第二条: %v", err)
	}
	if got2.ID != second {
		t.Fatalf("领到 %d，期望 %d", got2.ID, second)
	}
	if got2.Payload != "{}" {
		t.Errorf("payload = %q，期望默认 %q", got2.Payload, "{}")
	}
	if _, err := s.ClaimTask(); !errors.Is(err, ErrNoTask) {
		t.Errorf("队列排空后错误 = %v，期望 ErrNoTask", err)
	}
}

func TestClaimResetsPreviousRunState(t *testing.T) {
	s := newStore(t)
	id, err := s.CreateTask("mod_install", "失败过一次", nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := s.ClaimTask(); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.SetTaskProgress(id, 60, "已经下到一半", &TaskDetail{
		DoneBytes: 428750751, TotalBytes: 4594664662, BytesPerSec: 12345678, EtaSeconds: 337, Phase: "下载服务端本体文件",
	}); err != nil {
		t.Fatalf("SetTaskProgress: %v", err)
	}
	// 明细要能原样读回来：界面那几个数字块就是从这一列取的。
	withDetail, err := s.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !strings.Contains(withDetail.Detail, `"totalBytes":4594664662`) {
		t.Errorf("明细没落库：%q", withDetail.Detail)
	}
	if err := s.FinishTask(id, TaskFailed, "网络断了"); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}

	// 重提一条同类的，领取后不能看到上一轮的进度和失败原因。
	again, err := s.CreateTask("mod_install", "失败过一次", nil)
	if err != nil {
		t.Fatalf("CreateTask 重提: %v", err)
	}
	if err := s.SetTaskProgress(id, 60, "脏数据", &TaskDetail{DoneBytes: 1, TotalBytes: 2, Phase: "脏数据"}); err != nil {
		t.Fatalf("SetTaskProgress: %v", err)
	}
	// 新那条也要先写上脏值：ClaimTask 的 UPDATE 里带着 progress/message/detail 清零，
	// 只测"新行本来是空的"等于没测那次清零。
	if err := s.SetTaskProgress(again, 88, "残留", &TaskDetail{DoneBytes: 9, TotalBytes: 10, Phase: "残留"}); err != nil {
		t.Fatalf("SetTaskProgress: %v", err)
	}
	got, err := s.ClaimTask()
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if got.ID != again {
		t.Fatalf("领到 %d，期望重提的 %d", got.ID, again)
	}
	if got.Progress != 0 || got.Message != "" || got.Detail != "" {
		t.Errorf("领取后 progress=%d message=%q detail=%q，期望被清空", got.Progress, got.Message, got.Detail)
	}
}

func TestProgressAndFinishGuards(t *testing.T) {
	s := newStore(t)
	id, err := s.CreateTask("server_update", "更新服务端", nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if err := s.SetTaskProgress(id, 101, "", nil); err == nil {
		t.Error("进度 101 没被拒绝")
	}
	if err := s.SetTaskProgress(id, -1, "", nil); err == nil {
		t.Error("进度 -1 没被拒绝")
	}
	// 没领取就想收尾必须失败——否则一个排队任务会凭空变成"已完成"。
	if err := s.FinishTask(id, TaskSucceeded, ""); err == nil {
		t.Error("排队中的任务被收尾成功，期望拒绝")
	}
	if _, err := s.ClaimTask(); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// 终态之外的状态不能当收尾结果。
	if err := s.FinishTask(id, TaskRunning, ""); err == nil {
		t.Error("收尾成 running 没被拒绝")
	}
	if err := s.FinishTask(id, TaskSucceeded, "完成"); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}
	if err := s.FinishTask(id, TaskFailed, "再来一次"); err == nil {
		t.Error("已结束的任务被二次收尾，期望拒绝")
	}
	// 不存在的 id 也要给可操作的中文错误，不能静默成功。
	if err := s.FinishTask(9999, TaskFailed, ""); err == nil {
		t.Error("不存在的任务被收尾成功，期望报错")
	} else if !strings.Contains(err.Error(), "9999") {
		t.Errorf("错误里没有 id：%v", err)
	}
	if _, err := s.GetTask(9998); err == nil {
		t.Error("读不存在的任务返回了 nil error")
	}
}

func TestCancelOnlyQueued(t *testing.T) {
	s := newStore(t)
	running, err := s.CreateTask("backup", "备份存档", nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	queued, err := s.CreateTask("backup", "排队的那个", nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := s.ClaimTask(); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	// 在跑的任务不能靠改库来"取消"：worker 还在动文件，这边先显示已取消就是骗人。
	err = s.CancelTask(running, "用户取消")
	if err == nil {
		t.Fatal("执行中的任务被取消成功，期望拒绝")
	}
	if !strings.Contains(err.Error(), "执行中") {
		t.Errorf("取消失败的提示是 %v，期望带上当前状态的中文标签", err)
	}
	if err := s.CancelTask(queued, "用户取消"); err != nil {
		t.Fatalf("取消排队任务: %v", err)
	}
	tk, err := s.GetTask(queued)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.Status != TaskCanceled || tk.Message != "用户取消" || tk.FinishedAt == nil {
		t.Errorf("取消后的任务 = %+v", tk)
	}
}

func TestRecoverInterruptedTasks(t *testing.T) {
	s := newStore(t)
	claimed, err := s.CreateTask("mod_install", "面板挂了时的任务", nil)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := s.ClaimTask(); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if _, err := s.CreateTask("mod_install", "还在排队的", nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	n, err := s.RecoverInterruptedTasks()
	if err != nil {
		t.Fatalf("RecoverInterruptedTasks: %v", err)
	}
	if n != 1 {
		t.Errorf("回收了 %d 条，期望只回收那 1 条 running", n)
	}
	tk, err := s.GetTask(claimed)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.Status != TaskFailed {
		t.Errorf("中断任务状态 = %s，期望 failed", tk.Status)
	}
	if !strings.Contains(tk.Message, "重启") {
		t.Errorf("中断任务的说明是 %q，用户看不出为什么失败", tk.Message)
	}
	// 回收不能顺手把排队的也一起改了。
	all, err := s.ListTasks(10, TaskQueued)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("回收后排队任务剩 %d 条，期望 1 条", len(all))
	}
	// 再跑一次必须是空操作，面板重启两次不该把已完成的任务改坏。
	if n, err := s.RecoverInterruptedTasks(); err != nil || n != 0 {
		t.Errorf("第二次回收 n=%d err=%v，期望 0 且无错", n, err)
	}
}

func TestListAndCountTasks(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.CreateTask("mod_install", "任务", nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	if _, err := s.CreateTask("backup", "备份", nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := s.ClaimTask(); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	counts, err := s.CountTasksByStatus()
	if err != nil {
		t.Fatalf("CountTasksByStatus: %v", err)
	}
	if counts[TaskQueued] != 3 || counts[TaskRunning] != 1 {
		t.Errorf("状态统计 = %v，期望 queued=3 running=1", counts)
	}

	// 倒序：最新创建的在最前。
	all, err := s.ListTasks(0)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("全量列举 = %d 条，期望 4", len(all))
	}
	if all[0].ID < all[1].ID {
		t.Errorf("列举顺序不是按 id 倒序：%d 在 %d 前面", all[0].ID, all[1].ID)
	}
	// 按状态筛。
	only, err := s.ListTasks(10, TaskQueued)
	if err != nil {
		t.Fatalf("ListTasks 筛状态: %v", err)
	}
	if len(only) != 3 {
		t.Errorf("筛排队任务得到 %d 条，期望 3", len(only))
	}
	for _, tk := range only {
		if tk.Status != TaskQueued {
			t.Errorf("筛出来的任务状态是 %s", tk.Status)
		}
	}
	// limit 传 0 不能变成"把全表拖出来"，这里 4 条测不出上限，直接断言它给了默认值上限内的结果。
	if _, err := s.ListTasks(-5); err != nil {
		t.Errorf("负数 limit 报错：%v", err)
	}
}

func TestAuditAppendFilterAndPurge(t *testing.T) {
	s := newStore(t)
	if err := s.AppendAudit(AuditEntry{Action: "mod.enable"}); err == nil {
		t.Error("没有操作者的审计记录被接受")
	}
	if err := s.AppendAudit(AuditEntry{Actor: "admin"}); err == nil {
		t.Error("没有动作名的审计记录被接受")
	}

	base := time.Now().UTC()
	entries := []AuditEntry{
		{Actor: "admin", Action: "mod.enable", Target: "workshop-1", OK: true, At: base.Add(-3 * 24 * time.Hour)},
		{Actor: "admin", Action: "mod.disable", Target: "workshop-1", OK: false, Err: "模组没装", At: base.Add(-2 * 24 * time.Hour)},
		{Actor: "system", Action: "shard.stop", Target: "Cluster_1/Master", OK: true, At: base.Add(-1 * time.Hour)},
	}
	for _, e := range entries {
		if err := s.AppendAudit(e); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}

	got, err := s.ListAudit(10, 0, "")
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("列出 %d 条，期望 3", len(got))
	}
	if got[0].Action != "shard.stop" {
		t.Errorf("第一条是 %s，期望最新的 shard.stop", got[0].Action)
	}
	if !got[0].OK || got[1].OK {
		t.Errorf("ok 字段存取反了：%v/%v", got[0].OK, got[1].OK)
	}
	if got[1].Err != "模组没装" {
		t.Errorf("失败原因丢了：%q", got[1].Err)
	}
	if !got[2].At.UTC().Equal(entries[0].At.Truncate(time.Millisecond)) {
		t.Errorf("时间戳来回不等值：%v vs %v", got[2].At, entries[0].At)
	}

	// 前缀筛 + 翻页。
	modOnly, err := s.ListAudit(10, 0, "mod.")
	if err != nil {
		t.Fatalf("ListAudit 前缀筛: %v", err)
	}
	if len(modOnly) != 2 {
		t.Errorf("mod. 前缀筛出 %d 条，期望 2", len(modOnly))
	}
	// 翻页：列表是倒序，所以第 2 页是较早那条 mod.enable。
	page, err := s.ListAudit(1, 1, "mod.")
	if err != nil {
		t.Fatalf("翻页: %v", err)
	}
	if len(page) != 1 || page[0].Action != "mod.enable" {
		t.Errorf("第 2 页 = %+v，期望只有 mod.enable", page)
	}
	// 前缀不能当通配用：把整条 action 当prefix传进来只应命中自己。
	if all, err := s.ListAudit(10, 0, "mod.enable"); err != nil || len(all) != 1 {
		t.Errorf("精确前缀 = %d 条 / %v，期望 1 条", len(all), err)
	}

	n, err := s.PurgeAudit(base.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("PurgeAudit: %v", err)
	}
	if n != 2 {
		t.Errorf("按保留期删了 %d 条，期望 2（只剩 1 小时前的那条）", n)
	}
	left, err := s.ListAudit(10, 0, "")
	if err != nil {
		t.Fatalf("清理后再列: %v", err)
	}
	if len(left) != 1 || left[0].Action != "shard.stop" {
		t.Errorf("清理后剩 %+v，期望只有 shard.stop", left)
	}
}

func TestSnapshotsRoundTripAndFiltering(t *testing.T) {
	s := newStore(t)
	if err := s.RecordSnapshot(Snapshot{Shard: "Master"}); err == nil {
		t.Error("缺集群名的快照被接受")
	}
	base := time.Now().UTC()
	// 同一时刻写多条时，测试不能依赖 id 顺序去推断时间顺序，所以显式错开秒。
	rows := []Snapshot{
		{Cluster: "Cluster_1", Shard: "Master", Day: 12, Season: "autumn", Online: 3, Detail: `["Wilson","Wes"]`, At: base.Add(-3 * time.Hour)},
		{Cluster: "Cluster_1", Shard: "Caves", Day: 12, Season: "autumn", Online: 1, At: base.Add(-2 * time.Hour)},
		{Cluster: "Cluster_1", Shard: "Master", Day: 13, Season: "winter", Online: 5, At: base.Add(-1 * time.Hour)},
		{Cluster: "Cluster_2", Shard: "Master", Day: 1, Season: "summer", Online: 0, At: base.Add(-30 * time.Minute)},
	}
	for _, sn := range rows {
		if err := s.RecordSnapshot(sn); err != nil {
			t.Fatalf("RecordSnapshot: %v", err)
		}
	}

	all, err := s.Snapshots("Cluster_1", "", base.Add(-time.Hour*24), 100)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("Cluster_1 全分片 = %d 条，期望 3", len(all))
	}
	if all[0].At.After(all[1].At) || all[1].At.After(all[2].At) {
		t.Errorf("趋势数据不是升序：%v %v %v", all[0].At, all[1].At, all[2].At)
	}
	if all[0].Detail != `["Wilson","Wes"]` || all[0].Online != 3 || all[0].Season != "autumn" {
		t.Errorf("快照字段存取不符：%+v", all[0])
	}

	only, err := s.Snapshots("Cluster_1", "Master", base.Add(-time.Hour*24), 100)
	if err != nil {
		t.Fatalf("按分片筛: %v", err)
	}
	if len(only) != 2 {
		t.Errorf("Master 分片 = %d 条，期望 2", len(only))
	}
	// since 生效：只取最近 90 分钟。
	recent, err := s.Snapshots("Cluster_1", "", base.Add(-90*time.Minute), 100)
	if err != nil {
		t.Fatalf("按时间筛: %v", err)
	}
	if len(recent) != 1 || recent[0].Shard != "Master" {
		t.Errorf("最近 90 分钟 = %+v，期望只有第 13 天那条", recent)
	}
	// 别的集群不能串味。
	other, err := s.Snapshots("Cluster_2", "", base.Add(-time.Hour*24), 100)
	if err != nil {
		t.Fatalf("查 Cluster_2: %v", err)
	}
	if len(other) != 1 || other[0].Cluster != "Cluster_2" {
		t.Errorf("Cluster_2 = %+v", other)
	}

	// 边界是严格小于：正好卡在 cutoff 那条（2 小时前）必须留下，
	// 否则"保留 30 天"会变成"第 30 天当天的数据被删光"。
	n, err := s.PurgeSnapshots(base.Add(-2 * time.Hour))
	if err != nil {
		t.Fatalf("PurgeSnapshots: %v", err)
	}
	if n != 1 {
		t.Errorf("删了 %d 条，期望 1（只有 3 小时前那条）", n)
	}
}

// TestSnapshotTrendBuckets 盯的是趋势图那笔账：一个格子必须把同一格里各世界的人数相加，
// 而且不能因为"取前 N 行"把最近的一段丢掉——云端实测过一次，界面写着过去 24 小时，
// 图最右端其实是 6 小时前的采样。
func TestSnapshotTrendBuckets(t *testing.T) {
	s := newStore(t)
	// 格子按 epoch 对齐，所以基准时刻必须正好落在 10 分钟整点上，期望值才是可手算的。
	base := time.Now().UTC().Truncate(10 * time.Minute)
	rows := []Snapshot{
		// 这五条都落在同一个格子 [base-10m, base) 里。
		{Cluster: "Cluster_1", Shard: "Master", Online: 2, At: base.Add(-9 * time.Minute)},
		{Cluster: "Cluster_1", Shard: "Caves", Online: 1, At: base.Add(-8 * time.Minute)},
		// 同一格里的第二条只该影响平均值，不该把人数翻倍。
		{Cluster: "Cluster_1", Shard: "Master", Online: 4, At: base.Add(-5 * time.Minute)},
		{Cluster: "Cluster_1", Shard: "Caves", Online: 3, At: base.Add(-4 * time.Minute)},
		{Cluster: "Cluster_1", Shard: "Caves", Online: 1, At: base.Add(-1 * time.Minute)},
		// 前面那一格只有一条：最新这条必须在结果里，图才有右端。
		{Cluster: "Cluster_1", Shard: "Master", Online: 7, At: base.Add(-70 * time.Minute)},
		{Cluster: "Cluster_2", Shard: "Master", Online: 5, At: base.Add(-2 * time.Minute)},
	}
	for _, sn := range rows {
		if err := s.RecordSnapshot(sn); err != nil {
			t.Fatalf("RecordSnapshot: %v", err)
		}
	}

	tn, err := s.SnapshotTrend("Cluster_1", "", base.Add(-24*time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatalf("SnapshotTrend: %v", err)
	}
	if tn.Samples != 6 {
		t.Errorf("原始采样条数 = %d，期望 6（Cluster_2 那条不算）", tn.Samples)
	}
	if got, want := tn.DataTo, rows[4].At; !got.Equal(want) {
		t.Errorf("最晚采样 = %v，期望 %v", got, want)
	}
	// 升序、且每个格子最多一条。
	for i := 1; i < len(tn.Points); i++ {
		if !tn.Points[i-1].At.Before(tn.Points[i].At) {
			t.Fatalf("格子不是升序：%v %v", tn.Points[i-1].At, tn.Points[i].At)
		}
	}
	// 逐格手算：
	//   [base-70m) 只有地表 7 人 → 7；
	//   [base-10m) 地表平均 (2+4)/2=3，洞穴平均 (1+3+1)/3≈1.67，相加 4.67 → 四舍五入 5。
	want := []TrendPoint{{At: base.Add(-70 * time.Minute), Online: 7},
		{At: base.Add(-10 * time.Minute), Online: 5}}
	if len(tn.Points) != len(want) {
		t.Fatalf("格子数 = %d，期望 %d：%+v", len(tn.Points), len(want), tn.Points)
	}
	for i, w := range want {
		if !tn.Points[i].At.Equal(w.At) || tn.Points[i].Online != w.Online {
			t.Errorf("第 %d 格 = %+v，期望 %+v", i, tn.Points[i], w)
		}
	}

	// 只看一个世界时不许把另一个世界的数字加进来。
	only, err := s.SnapshotTrend("Cluster_1", "Master", base.Add(-24*time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatalf("按世界筛: %v", err)
	}
	if only.Samples != 3 {
		t.Errorf("Master 的原始条数 = %d，期望 3", only.Samples)
	}
	for _, p := range only.Points {
		if p.Online > 7 {
			t.Errorf("单世界出现了 %d 人，期望不超过这个世界的最大人数 7", p.Online)
		}
	}

	// 没采样的房间：空点、0 条、时间零值，而不是报错或返回上一房间的数据。
	none, err := s.SnapshotTrend("NoSuchCluster", "", base.Add(-time.Hour), time.Minute)
	if err != nil {
		t.Fatalf("空房间: %v", err)
	}
	if len(none.Points) != 0 || none.Samples != 0 || !none.DataTo.IsZero() {
		t.Errorf("空房间给回了 %+v", none)
	}
}

func TestSettingsGuardsAndDefaults(t *testing.T) {
	s := newStore(t)

	// 拼错的键必须被拒绝：设置表是写进去就没人复查的地方。
	if err := s.SetSetting("auditr_retention_dayz", "30"); err == nil {
		t.Fatal("未知设置项被写入")
	} else if !errors.Is(err, ErrUnknownSetting) {
		t.Errorf("错误没带上 ErrUnknownSetting：%v", err)
	}
	if _, err := s.Int("nope"); !errors.Is(err, ErrUnknownSetting) {
		t.Errorf("读未知项 = %v，期望 ErrUnknownSetting", err)
	}

	// 没写过就用登记值。
	days, err := s.Int(KeyAuditRetentionDays)
	if err != nil {
		t.Fatalf("默认值: %v", err)
	}
	if days != defaults[KeyAuditRetentionDays] {
		t.Errorf("默认 %d，期望 %d", days, defaults[KeyAuditRetentionDays])
	}
	if _, found, err := s.Setting(KeyAuditRetentionDays); err != nil || found {
		t.Errorf("默认项不该被算成已设置：found=%v err=%v", found, err)
	}

	if err := s.SetSetting(KeyAuditRetentionDays, "7"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := s.SetSetting(KeyAuditRetentionDays, "365"); err != nil {
		t.Fatalf("SetSetting 覆盖: %v", err)
	}
	days, err = s.Int(KeyAuditRetentionDays)
	if err != nil || days != 365 {
		t.Errorf("覆盖后 = %d / %v，期望 365", days, err)
	}
	if err := s.SetSetting(KeySnapshotRetentionDays, ""); err == nil {
		t.Error("空值被写入")
	}

	// 脏数据（有人手改过库）要回落默认值并把话说清楚。
	if _, err := s.db.Exec(`UPDATE settings SET value = '十天' WHERE key = ?`, KeyAuditRetentionDays); err != nil {
		t.Fatalf("伪造脏数据: %v", err)
	}
	got, err := s.Int(KeyAuditRetentionDays)
	if err == nil {
		t.Error("非整数值没报错")
	} else if !strings.Contains(err.Error(), "十天") {
		t.Errorf("报错里没提原值：%v", err)
	}
	if got != defaults[KeyAuditRetentionDays] {
		t.Errorf("脏数据下没回落默认值：%d", got)
	}

	listed, err := s.Settings()
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if len(listed) != 1 || listed[KeyAuditRetentionDays] != "十天" {
		t.Errorf("列举设置 = %v，期望只有被改过的那项", listed)
	}
	if d, ok := DefaultInt(KeySnapshotRetentionDays); !ok || d != 30 {
		t.Errorf("DefaultInt = %d/%v", d, ok)
	}
	if _, ok := DefaultInt("bogus"); ok {
		t.Error("DefaultInt 承认了未登记的键")
	}
}

func TestRetentionCutoff(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	// 默认 90 天。
	cut, err := s.RetentionCutoff(KeyAuditRetentionDays, now)
	if err != nil {
		t.Fatalf("RetentionCutoff: %v", err)
	}
	if !cut.Equal(now.AddDate(0, 0, -90)) {
		t.Errorf("截止 = %v，期望 %v", cut, now.AddDate(0, 0, -90))
	}
	// 0 或负数表示"都留着"，必须回零值时间让调用方跳过清理，
	// 绝不能算成"今天之前的全删"。
	for _, v := range []string{"0", "-1"} {
		if err := s.SetSetting(KeyAuditRetentionDays, v); err != nil {
			t.Fatalf("SetSetting %s: %v", v, err)
		}
		cut, err := s.RetentionCutoff(KeyAuditRetentionDays, now)
		if err != nil {
			t.Fatalf("RetentionCutoff %s: %v", v, err)
		}
		if !cut.IsZero() {
			t.Errorf("%s 天算出截止 %v，期望零值（表示不清理）", v, cut)
		}
	}
	if _, err := s.RetentionCutoff("bogus_key", now); err == nil {
		t.Error("未登记的键算出了截止时间")
	}
}
