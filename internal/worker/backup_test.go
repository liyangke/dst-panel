// 备份/恢复走任务队列这一段的用例。
//
// 这一层最值得钉住的是"什么时候不动存档目录"：
// 分片还在跑、进程状态问不清、快照校验不过、房间已经不在了——
// 这四种情况都必须停在动手之前，因为它们任何一种做下去都是丢世界。
// 恢复成功那条只验"换掉了"，还要验"换的方式对"（整棵替换而不是覆盖文件）。
package worker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/backup"
	"dstpanel/internal/dst"
	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
)

// recordRunner 既回答 screen -ls，又记下每一次被执行的命令。
// 记命令是为了能验"面板确实往那个会话里塞了 c_save()"，而不是只验它返回成功。
type recordRunner struct {
	lsOut string
	calls [][]string
}

func (r *recordRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if strings.Contains(strings.Join(args, " "), "-ls") {
		return r.lsOut, nil
	}
	return "", nil
}

func (r *recordRunner) consoleLines() string {
	var out []string
	for _, c := range r.calls {
		out = append(out, strings.Join(c, " "))
	}
	return strings.Join(out, "\n")
}

type failRunner struct{ err error }

func (r failRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return "", r.err
}

type backupFixture struct {
	f     *workerFixture
	mgr   *backup.Manager
	paths dst.Paths
	procs *recordRunner
	dir   string
}

func newBackupFixture(t *testing.T, screenOut string) *backupFixture {
	t.Helper()
	bf := &backupFixture{f: newWorkerFixture(t, nil, nil)}
	bf.paths = bf.f.w.Paths
	bf.procs = &recordRunner{lsOut: screenOut}
	bf.f.w.Procs = procrun.New(bf.paths, bf.procs)

	dir := filepath.Join(t.TempDir(), "panel", "backups")
	bf.dir = dir
	bf.mgr = &backup.Manager{
		Paths: bf.paths, Dir: dir, Keep: 20, Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	bf.f.w.Backups = bf.mgr
	writeRoom(t, bf.paths, "Cluster_1",
		map[string][]string{dst.ShardMaster: {"slot-0001", "slot-0002"}, dst.ShardCaves: {"slot-0001"}})
	return bf
}

// writeRoom 造一个有存档的房间：slot 目录、令牌、通道文件、引擎日志都齐，
// 这样"跳过了哪些"和"整棵替换"才有东西可验。
func writeRoom(t *testing.T, paths dst.Paths, cluster string, slotsByShard map[string][]string) {
	t.Helper()
	mustWrite(t, paths.ClusterIni(cluster), "cluster\n")
	mustWrite(t, paths.TokenFile(cluster), "pds-g^KU_fakeid001^THIS_TEXT_MUST_NEVER_BE_BACKED_UP\n")
	for shard, slots := range slotsByShard {
		mustWrite(t, paths.ShardIni(cluster, shard), fmt.Sprintf("[shard]\nid=%s\n", shard))
		mustWrite(t, filepath.Join(paths.SaveDir(cluster, shard), "world"), "worldsave\n")
		for _, slot := range slots {
			mustWrite(t, filepath.Join(paths.SessionRoot(cluster, shard), slot, "server.dat"),
				fmt.Sprintf("%s/%s 世界内容\n", shard, slot))
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runTaskToCompletion(t *testing.T, f *workerFixture, kind, title string, payload any) store.Task {
	t.Helper()
	id, err := f.st.CreateTask(kind, title, payload)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !f.w.runOnce(context.Background()) {
		t.Fatal("队列里有任务却没领到")
	}
	task, err := f.st.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

func TestBackupCreateProducesUsableSnapshot(t *testing.T) {
	bf := newBackupFixture(t, "")
	task := runTaskToCompletion(t, bf.f, KindBackupCreate, "备份存档 · Cluster_1",
		BackupCreatePayload{Cluster: "Cluster_1"})

	if task.Status != store.TaskSucceeded {
		t.Fatalf("状态 = %s，消息 %q", task.Status, task.Message)
	}
	if !strings.Contains(task.Message, "已生成") || !strings.Contains(task.Message, "个文件") {
		t.Errorf("收尾消息要说清打了几个文件，实际 %q", task.Message)
	}
	// 没跑分片时也要说清打的是磁盘上现有的档，否则用户以为面板催过引擎存盘了。
	if !strings.Contains(task.Message, "没有在跑的世界") {
		t.Errorf("收尾消息丢了「这份档从哪来」那句：%q", task.Message)
	}

	list, err := bf.mgr.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("快照目录里应该有 1 份，实际 %+v err=%v", list, err)
	}
	snap := list[0]
	if snap.Label != backup.LabelManual {
		t.Errorf("payload 没给来源时应落到手工，实际 %q", snap.Label)
	}
	if !snap.Restorable {
		t.Errorf("刚打的快照却不能用：%s", snap.RestoreNote)
	}
	if len(snap.SaveShards) != 2 {
		t.Errorf("两个分片的存档都要登记，实际 %+v", snap.SaveShards)
	}
	// 令牌绝不能进包：这份快照是能下载的。
	blob := archiveText(t, filepath.Join(bf.mgr.Dir, snap.ID+backup.ArchiveSuffix))
	if strings.Contains(blob, "THIS_TEXT_MUST_NEVER_BE_BACKED_UP") {
		t.Error("归档里有 Klei 令牌明文")
	}
}

// 在跑的分片要先收一次 c_save()，否则快照是"上一次自动存盘"那个时刻的世界。
// 命令走的是白名单里的 save 那一条，不经过 shell。
func TestBackupCreateFlushesRunningShards(t *testing.T) {
	bf := newBackupFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")
	// 这条会真的等 saveFlushWait（引擎收命令后要跑几 tick 才落盘），是测试里唯一慢的一处。
	task := runTaskToCompletion(t, bf.f, KindBackupCreate, "备份存档 · Cluster_1",
		BackupCreatePayload{Cluster: "Cluster_1"})
	if task.Status != store.TaskSucceeded {
		t.Fatalf("状态 = %s，消息 %q", task.Status, task.Message)
	}
	if !strings.Contains(task.Message, "已让 Master 立即存盘") {
		t.Errorf("要说明确实催过存盘，实际 %q", task.Message)
	}
	lines := bf.procs.consoleLines()
	if !strings.Contains(lines, "c_save()") {
		t.Errorf("没往会话里塞存盘命令：\n%s", lines)
	}
	if !strings.Contains(lines, "dst-Cluster_1-Master") {
		t.Errorf("命令发给了别的会话：\n%s", lines)
	}
	// Caves 没在跑，就不该对它下命令。
	if strings.Contains(lines, "dst-Cluster_1-Caves") {
		t.Errorf("给没在跑的分片也发了命令：\n%s", lines)
	}
}

// 问不到进程状态时备份照样打（备份是可重复的只读动作），但结论不能说"这个房间没有在跑的分片"：
// 那句话是在替面板撒谎，而用户正是靠它判断这份档有没有带上刚才那几分钟。
func TestBackupCreateDoesNotClaimNoShardsRunningWhenProbeFails(t *testing.T) {
	bf := newBackupFixture(t, "")
	bf.f.w.Procs = procrun.New(bf.paths, failRunner{err: fmt.Errorf("screen: executable file not found in $PATH")})

	task := runTaskToCompletion(t, bf.f, KindBackupCreate, "备份存档 · Cluster_1",
		BackupCreatePayload{Cluster: "Cluster_1"})
	if task.Status != store.TaskSucceeded {
		t.Fatalf("问不清不该让备份失败，实际 %s：%q", task.Status, task.Message)
	}
	if !strings.Contains(task.Message, "问不到") {
		t.Errorf("要如实说问不到进程状态，实际 %q", task.Message)
	}
	if strings.Contains(task.Message, "没有在跑的分片") {
		t.Errorf("把问不到说成没在跑：%q", task.Message)
	}
}

// 任务行是持久化的：重启面板后队列里那条 payload 可能已经和磁盘隔了一段时间，
// 房间被删掉是真实会发生的，这时必须失败而不是在别处打出一份"看起来成功"的快照。
func TestBackupCreateRefusesVanishedCluster(t *testing.T) {
	bf := newBackupFixture(t, "")
	task := runTaskToCompletion(t, bf.f, KindBackupCreate, "备份存档 · Cluster_9",
		BackupCreatePayload{Cluster: "Cluster_9"})
	if task.Status != store.TaskFailed {
		t.Fatalf("房间不存在却要报成功：%+v", task)
	}
	if !strings.Contains(task.Message, "Cluster_9") || !strings.Contains(task.Message, "不存在") {
		t.Errorf("要点名是哪个房间不存在，实际 %q", task.Message)
	}
	list, _ := bf.mgr.List()
	if len(list) != 0 {
		t.Errorf("拦住了还是打出了快照：%+v", list)
	}
}

func TestBackupCreateSaysWhenModuleMissing(t *testing.T) {
	bf := newBackupFixture(t, "")
	bf.f.w.Backups = nil
	task := runTaskToCompletion(t, bf.f, KindBackupCreate, "备份存档 · Cluster_1",
		BackupCreatePayload{Cluster: "Cluster_1"})
	if task.Status != store.TaskFailed || !strings.Contains(task.Message, "没接上存档备份模块") {
		t.Errorf("接线漏了要能一眼看出来：%+v", task)
	}
}

// 恢复必须是"整棵换掉 save 目录"：引擎读的是编号最大的档位，
// 只覆盖文件的话 slot-0002 还留在原位，下次存盘又写回那个世界。
func TestBackupRestoreReplacesWholeSaveTree(t *testing.T) {
	bf := newBackupFixture(t, "")
	snap, err := bf.mgr.Create(context.Background(), "Cluster_1", backup.LabelManual)
	if err != nil {
		t.Fatalf("打快照: %v", err)
	}
	// 快照之后又存了一次盘，多出 slot-0003：恢复要把它抹掉。
	mustWrite(t, filepath.Join(bf.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-0003", "server.dat"), "更新的档\n")

	task := runTaskToCompletion(t, bf.f, KindBackupRestore, "恢复存档",
		BackupRestorePayload{SnapshotID: snap.ID})
	if task.Status != store.TaskSucceeded {
		t.Fatalf("状态 = %s，消息 %q", task.Status, task.Message)
	}
	if _, err := os.Stat(filepath.Join(bf.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-0003")); !os.IsNotExist(err) {
		t.Error("恢复后快照里没有的档位还在，等于没换干净")
	}
	body, err := os.ReadFile(filepath.Join(bf.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-0002", "server.dat"))
	if err != nil || !strings.Contains(string(body), "slot-0002") {
		t.Errorf("快照里的档位没恢复回来：%v %q", err, body)
	}
	if !strings.Contains(task.Message, "恢复前的安全备份") {
		t.Errorf("要说清退路在哪，实际 %q", task.Message)
	}
	// 安全备份自己也是一份能用的快照，界面上要看得见。
	list, _ := bf.mgr.List()
	var safety bool
	for _, s := range list {
		if s.Label == backup.LabelBeforeRestore {
			safety = true
		}
	}
	if !safety {
		t.Errorf("列表里没有那份恢复前的安全备份：%+v", snapshotBrief(list))
	}
}

func snapshotBrief(list []backup.Snapshot) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID+"/"+s.Label)
	}
	return out
}

// 包被改过的快照在动手之前就被拦下，磁盘还是原样：
// 这时消息里绝不能出现"没能自动换回原存档"——那等于告诉用户现场已经坏了，
// 而他真正需要知道的是"这个房间可以直接启动"。（实测踩过：任务中心就是这么说的。）
func TestBackupRestoreOfBrokenSnapshotSaysNothingWasTouched(t *testing.T) {
	bf := newBackupFixture(t, "")
	snap, err := bf.mgr.Create(context.Background(), "Cluster_1", backup.LabelManual)
	if err != nil {
		t.Fatalf("打快照: %v", err)
	}
	// 改一个字节、长度不变：改大小的话 Get 就直接判不可恢复了，
	// 这里要钉的是"包能打开但内容对不上"这一种——它才会走进恢复流程再被整包校验拦下。
	archivePath := filepath.Join(bf.dir, snap.ID+backup.ArchiveSuffix)
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(archivePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	datPath := filepath.Join(bf.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-0002", "server.dat")
	before := readFile(t, datPath)

	task := runTaskToCompletion(t, bf.f, KindBackupRestore, "恢复存档",
		BackupRestorePayload{SnapshotID: snap.ID})
	if task.Status != store.TaskFailed {
		t.Fatalf("坏包却恢复了：%+v", task)
	}
	if !strings.Contains(task.Message, "一点都没动") {
		t.Errorf("要说清现有存档没被动过、可以直接启动，实际 %q", task.Message)
	}
	if strings.Contains(task.Message, "没能自动换回") {
		t.Errorf("磁盘根本没被碰，却在说换不回去：%q", task.Message)
	}
	if got := readFile(t, datPath); got != before {
		t.Error("被拒绝的恢复动了存档目录")
	}
	// 拦在动手之前，那份"恢复前的安全备份"也不该打——列表里会多出一份什么都没换掉的档。
	list, _ := bf.mgr.List()
	if len(list) != 1 {
		t.Errorf("恢复被拦却多打了快照：%+v", snapshotBrief(list))
	}
}

// 归档被截短（大小和清单对不上）同样拦在动手之前，而且是 Get 那一步就判不可恢复：
// 这条分支在 Restore 之前，也要说清"现场没被动过"，否则用户只能猜。
func TestBackupRestoreOfShortArchiveSaysNothingWasTouched(t *testing.T) {
	bf := newBackupFixture(t, "")
	snap, err := bf.mgr.Create(context.Background(), "Cluster_1", backup.LabelManual)
	if err != nil {
		t.Fatalf("打快照: %v", err)
	}
	p := filepath.Join(bf.dir, snap.ID+backup.ArchiveSuffix)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	task := runTaskToCompletion(t, bf.f, KindBackupRestore, "恢复存档",
		BackupRestorePayload{SnapshotID: snap.ID})
	if task.Status != store.TaskFailed {
		t.Fatalf("截短的归档却恢复了：%+v", task)
	}
	if !strings.Contains(task.Message, "不能用来恢复") || !strings.Contains(task.Message, "一点都没动") {
		t.Errorf("要说清这份不能用、而且现有存档没被动过，实际 %q", task.Message)
	}
	if strings.Contains(task.Message, "没能自动换回") {
		t.Errorf("根本没动手，却在说换不回去：%q", task.Message)
	}
}

// 对着在跑的分片换存档目录 = 引擎把内存里的世界再存回去，恢复白做还留下混合状态。
func TestBackupRestoreRefusesRunningShards(t *testing.T) {
	bf := newBackupFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")
	snap, err := bf.mgr.Create(context.Background(), "Cluster_1", backup.LabelManual)
	if err != nil {
		t.Fatalf("打快照: %v", err)
	}
	before := readFile(t, filepath.Join(bf.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-0002", "server.dat"))

	task := runTaskToCompletion(t, bf.f, KindBackupRestore, "恢复存档",
		BackupRestorePayload{SnapshotID: snap.ID})
	if task.Status != store.TaskFailed {
		t.Fatalf("分片在跑却恢复了：%+v", task)
	}
	if !strings.Contains(task.Message, "还有世界在运行") || !strings.Contains(task.Message, "Cluster_1/Master") {
		t.Errorf("要说清是哪个分片在跑，实际 %q", task.Message)
	}
	if got := readFile(t, filepath.Join(bf.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-0002", "server.dat")); got != before {
		t.Error("拦住之前已经动过存档目录了")
	}
	// 被拦住时连安全备份都不该打：那会往列表里塞一份"什么都没换掉"的档。
	list, _ := bf.mgr.List()
	if len(list) != 1 {
		t.Errorf("恢复被拦却多打了快照：%+v", snapshotBrief(list))
	}
}

// 问不到进程状态时选择拦住（和"更新本体"那一项相反）：
// 覆盖文件可以重来，换存档目录撞上引擎正在写档就是半新半旧的世界。
func TestBackupRestoreFailsClosedWhenProcessStateUnknown(t *testing.T) {
	bf := newBackupFixture(t, "")
	snap, err := bf.mgr.Create(context.Background(), "Cluster_1", backup.LabelManual)
	if err != nil {
		t.Fatalf("打快照: %v", err)
	}
	bf.f.w.Procs = procrun.New(bf.paths, failRunner{err: fmt.Errorf("%w screen: exec not found", procrun.ErrCommandMissing)})

	task := runTaskToCompletion(t, bf.f, KindBackupRestore, "恢复存档",
		BackupRestorePayload{SnapshotID: snap.ID})
	if task.Status != store.TaskFailed {
		t.Fatalf("问不清却恢复了：%+v", task)
	}
	if !strings.Contains(task.Message, "问不到") || !strings.Contains(task.Message, "问不清就不动") {
		t.Errorf("要把「问不清所以不动」这句说到位，实际 %q", task.Message)
	}
}

// 跨房间迁移：目标房间还没有 Caves 分片，但快照里有，恢复要把它建出来。
func TestBackupRestoreMigratesToOtherCluster(t *testing.T) {
	bf := newBackupFixture(t, "")
	snap, err := bf.mgr.Create(context.Background(), "Cluster_1", backup.LabelManual)
	if err != nil {
		t.Fatalf("打快照: %v", err)
	}
	writeRoom(t, bf.paths, "Cluster_2", map[string][]string{dst.ShardMaster: {"slot-0007"}})

	task := runTaskToCompletion(t, bf.f, KindBackupRestore, "迁移存档",
		BackupRestorePayload{SnapshotID: snap.ID, TargetCluster: "Cluster_2"})
	if task.Status != store.TaskSucceeded {
		t.Fatalf("状态 = %s，消息 %q", task.Status, task.Message)
	}
	if _, err := os.Stat(filepath.Join(bf.paths.SaveDir("Cluster_2", dst.ShardCaves))); err != nil {
		t.Errorf("快照里的洞穴没建出来：%v", err)
	}
	// 目标房间自己的世界（slot-0007）不该继续留着被引擎选中。
	if _, err := os.Stat(filepath.Join(bf.paths.SessionRoot("Cluster_2", dst.ShardMaster), "slot-0007")); !os.IsNotExist(err) {
		t.Error("迁移后目标房间原来的档位还在，引擎会读错世界")
	}
}

// ---- 定时备份 ----

// fakeClock 让"隔了几小时"这件事可测。
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

type schedFixture struct {
	sc    *BackupScheduler
	st    *store.Store
	clock *fakeClock
	paths dst.Paths
	hours int
}

func newSchedFixture(t *testing.T, hours int) *schedFixture {
	t.Helper()
	f := newWorkerFixture(t, nil, nil)
	clock := &fakeClock{now: time.Now()}
	sc := &BackupScheduler{
		Store:  f.st,
		Paths:  f.w.Paths,
		Hours:  func() int { return hours },
		Now:    clock.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	writeRoom(t, f.w.Paths, "Cluster_1", map[string][]string{dst.ShardMaster: {"slot-0001"}})
	// Cluster_2 有配置目录但从没起过服：没有 save，定时备份要跳过它。
	writeRoomNoSave(t, f.w.Paths, "Cluster_2")
	return &schedFixture{sc: sc, st: f.st, clock: clock, paths: f.w.Paths, hours: hours}
}

func writeRoomNoSave(t *testing.T, paths dst.Paths, cluster string) {
	t.Helper()
	mustWrite(t, paths.ClusterIni(cluster), "cluster\n")
	mustWrite(t, paths.ShardIni(cluster, dst.ShardMaster), "[shard]\nid=Master\n")
}

func pendingTasks(t *testing.T, st *store.Store, kind string) []store.Task {
	t.Helper()
	list, err := st.ListTasks(50)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	var out []store.Task
	for _, task := range list {
		if task.Kind == kind {
			out = append(out, task)
		}
	}
	return out
}

func TestBackupSchedulerTickRespectsIntervalAndSavePresence(t *testing.T) {
	f := newSchedFixture(t, 6)
	// 面板刚起来就等一个基周期：这时距 lastRun 不满间隔，一份都不该打。
	f.sc.lastRun = f.clock.Now()
	f.clock.advance(time.Hour)
	if got := f.sc.Tick(); got != 0 {
		t.Fatalf("间隔没到就打了 %d 份，期望 0", got)
	}

	f.clock.advance(6 * time.Hour)
	if got := f.sc.Tick(); got != 1 {
		t.Errorf("到点了该给有存档的那个房间打一份，实际 %d", got)
	}
	tasks := pendingTasks(t, f.st, KindBackupCreate)
	if len(tasks) != 1 {
		t.Fatalf("队列里应该只有 1 条定时备份：%+v", tasks)
	}
	if !strings.Contains(tasks[0].Title, "Cluster_1") || !strings.Contains(tasks[0].Title, "定时") {
		t.Errorf("任务标题要看出是定时备份的哪个房间：%q", tasks[0].Title)
	}
	var p BackupCreatePayload
	if err := tasks[0].PayloadInto(&p); err != nil {
		t.Fatalf("payload 解不开: %v", err)
	}
	if p.Cluster != "Cluster_1" || p.Label != backup.LabelScheduled {
		t.Errorf("payload = %+v，期望 Cluster_1 + %q", p, backup.LabelScheduled)
	}
	// 没存档的房间被跳过了：那份快照只会以一条失败任务收尾，白占列表。
	if strings.Contains(fmt.Sprint(p.Cluster), "Cluster_2") {
		t.Error("给没有存档的房间也入了队")
	}

	// 打完这一轮又要等满一个间隔，否则每 10 分钟的基周期都会重复入队。
	if got := f.sc.Tick(); got != 0 {
		t.Errorf("刚打过一轮还接着打，实际 %d", got)
	}
}

func TestBackupSchedulerOffByDefault(t *testing.T) {
	f := newSchedFixture(t, 0)
	f.sc.lastRun = f.clock.Now().Add(-30 * 24 * time.Hour)
	f.clock.advance(30 * 24 * time.Hour)
	if got := f.sc.Tick(); got != 0 {
		t.Errorf("间隔设成 0 就是不开定时备份，实际入了 %d 条队", got)
	}
	if len(pendingTasks(t, f.st, KindBackupCreate)) != 0 {
		t.Error("没开却在队列里留了任务")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(raw)
}

// archiveText 把归档解成纯文本供 substring 检查（令牌泄漏这类断言只能这么验）。
func archiveText(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("开归档: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()
	var sb strings.Builder
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读 tar: %v", err)
		}
		sb.WriteString(hdr.Name)
		sb.WriteString("\n")
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("读 %s: %v", hdr.Name, err)
		}
		sb.Write(raw)
	}
	return sb.String()
}
