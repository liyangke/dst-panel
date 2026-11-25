// 存档快照接口的用例。
//
// 这一层的价值全在"点下去之前就知道行不行"：
// 列表要说清哪份能恢复、为什么不能；两个写入口只入队不动存档；
// 参数只能由面板对着磁盘名单拼出来，请求体改不动它。
// 所以每个用例都两头看：HTTP 回了什么，以及任务队列里真的落了什么。
package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/backup"
	"dstpanel/internal/dst"
	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
	"dstpanel/internal/worker"
)

const fakeTokenSecret = "pds-g^KU_fakeid001^THIS_TEXT_MUST_NEVER_BE_BACKED_UP"

type backupAPIFixture struct {
	f     *apiFixture
	mgr   *backup.Manager
	snap  backup.Snapshot
	paths dst.Paths
}

// newBackupAPIFixture 造一个"有存档、已经打过一份快照"的房间。
// screenOut 决定哪些分片正在跑，恢复该不该被拦就由它控制。
func newBackupAPIFixture(t *testing.T, screenOut string) *backupAPIFixture {
	t.Helper()
	f := newFixture(t, screenOut)
	for shard, slots := range map[string][]string{
		dst.ShardMaster: {"slot-0001", "slot-0002"},
		dst.ShardCaves:  {"slot-0001"},
	} {
		writeSaveFile(t, f.paths.ShardIni(f.cluster, shard), "[shard]\nid=x\n")
		writeSaveFile(t, filepath.Join(f.paths.SaveDir(f.cluster, shard), "world"), "worldsave\n")
		writeSaveFile(t, filepath.Join(f.paths.SaveDir(f.cluster, shard), "dstpanel_status.json"), `{"shard":"Master"}`)
		for _, slot := range slots {
			writeSaveFile(t, filepath.Join(f.paths.SessionRoot(f.cluster, shard), slot, "server.dat"),
				shard+"/"+slot+" 世界内容\n")
		}
	}
	writeSaveFile(t, f.paths.TokenFile(f.cluster), fakeTokenSecret+"\n")

	bm := &backup.Manager{
		Paths: f.paths, Dir: filepath.Join(t.TempDir(), "backups"), Keep: 7, Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	f.srv.Backups = bm
	// 配置文件那份是 7：Manager 现读设置，和 main.go 里的接法一致。
	f.srv.SettingDefaults = map[string]int{store.KeyBackupKeep: 7, store.KeyBackupScheduleHours: 0}
	bm.KeepFunc = func() int {
		n, err := f.srv.settingInt(store.KeyBackupKeep)
		if err != nil {
			return bm.Keep
		}
		return n
	}

	snap, err := bm.Create(context.Background(), f.cluster, backup.LabelManual)
	if err != nil {
		t.Fatalf("造快照失败: %v", err)
	}
	return &backupAPIFixture{f: f, mgr: bm, snap: snap, paths: f.paths}
}

func writeSaveFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// archiveText 把下载到的 tar.gz 解成文本，令牌这类断言只能对着解压内容查。
func archiveText(t *testing.T, raw []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("解 gzip: %v", err)
	}
	defer gz.Close()
	var sb strings.Builder
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
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
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("读 %s: %v", hdr.Name, err)
		}
		sb.Write(body)
	}
	return sb.String()
}

// queuedBackupTasks 把队列里的备份类任务读出来，看 payload 到底是什么。
func queuedBackupTasks(t *testing.T, st *store.Store) []store.Task {
	t.Helper()
	list, err := st.ListTasks(50)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	var out []store.Task
	for _, task := range list {
		if strings.HasPrefix(task.Kind, "backup.") {
			out = append(out, task)
		}
	}
	return out
}

// 备份入口只入队：接口不能替用户等几十秒，更不能自己碰存档目录。
// payload 里的房间名来自 URL，来源（手工）由面板写死——浏览器说什么都没用。
func TestBackupCreateEnqueuesValidatedTask(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	h := bf.f.srv.Handler()

	rec := do(t, h, http.MethodPost, "/api/v1/clusters/Cluster_1/backups", "admin", "secret")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("应 202，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var accepted struct {
		TaskID int64  `json:"taskId"`
		Note   string `json:"note"`
	}
	decodeData(t, rec, &accepted)
	if accepted.TaskID == 0 {
		t.Fatalf("没回任务 id：%s", rec.Body.String())
	}
	if !strings.Contains(accepted.Note, "备份任务已入队") {
		t.Errorf("note 要说清接下来会发生什么：%q", accepted.Note)
	}

	tasks := queuedBackupTasks(t, bf.f.srv.Store)
	if len(tasks) != 1 || tasks[0].Kind != worker.KindBackupCreate {
		t.Fatalf("队列里应该正好一条备份任务：%+v", tasks)
	}
	var p worker.BackupCreatePayload
	if err := tasks[0].PayloadInto(&p); err != nil {
		t.Fatalf("payload 读不出: %v", err)
	}
	if p.Cluster != "Cluster_1" || p.Label != backup.LabelManual {
		t.Errorf("payload 被请求影响到了：%+v", p)
	}
	// HTTP 这一段不该真的打出快照：那份是 worker 的活。
	// 数量不变就证明接口没有自己去动目录。
	list, err := bf.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("只是入队，却多出快照：%+v", list)
	}
	// 任务详情要一并回来，界面不用再发一次请求就能显示进度壳子。
	var withTask struct {
		Task taskView `json:"task"`
	}
	decodeData(t, rec, &withTask)
	if withTask.Task.Kind != worker.KindBackupCreate || withTask.Task.StatusLab == "" {
		t.Errorf("202 里没带任务视图：%+v", withTask.Task)
	}
}

// URL 和请求体各写一个房间名时不能"两个都信"：备份的是哪一个必须只有一个答案。
func TestBackupCreateRejectsConflictingCluster(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/clusters/Cluster_1/backups",
		backupCreateRequest{Cluster: "Cluster_2"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("两处房间名不一致应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Cluster_2") || !strings.Contains(body, "一致") {
		t.Errorf("要点名是哪两处不一致，实际 %s", body)
	}
	if len(queuedBackupTasks(t, bf.f.srv.Store)) != 0 {
		t.Error("被拒的请求还是入了队")
	}
}

func TestBackupCreateRejectsUnknownCluster(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	rec := do(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/clusters/Cluster_9/backups", "admin", "secret")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的房间应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if len(queuedBackupTasks(t, bf.f.srv.Store)) != 0 {
		t.Error("房间不存在也入了队，任务只会在队列里失败一次")
	}
}

// 带参数的任务不能从通用提交口进：那里的请求体只有一个 kind，
// 一旦允许 payload，浏览器就能拼出"恢复任何一个房间"的任务。
func TestGenericSubmitRefusesBackupKinds(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	h := bf.f.srv.Handler()

	for _, kind := range []string{worker.KindBackupCreate, worker.KindBackupRestore} {
		rec := doJSON(t, h, http.MethodPost, "/api/v1/tasks",
			taskSubmitRequest{Kind: kind, Confirm: true})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 从通用口提交竟然 %d: %s", kind, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "通用提交口不接受") {
			t.Errorf("%s 的拒绝理由不合格：%s", kind, body)
		}
		// 拒绝里要给出该走哪个入口，否则界面只能自己猜路由。
		if !strings.Contains(body, "/api/v1/") {
			t.Errorf("%s 的回复没指出正确入口：%s", kind, body)
		}
	}
	if len(queuedBackupTasks(t, bf.f.srv.Store)) != 0 {
		t.Error("通用提交口竟然造出了备份任务")
	}

	// 任务类型表也要把这个限制说在前面，界面才有能力不渲染那个按钮。
	rec := do(t, h, http.MethodGet, "/api/v1/task-kinds", "admin", "secret")
	var kinds []taskKindView
	decodeData(t, rec, &kinds)
	saw := map[string]bool{}
	for _, k := range kinds {
		saw[k.Kind] = true
		if !strings.HasPrefix(k.Kind, "backup.") {
			continue
		}
		if !k.NeedsPayload || k.SubmitVia == "" {
			t.Errorf("%s 没标记该从哪提交：%+v", k.Kind, k)
		}
		if !k.Runnable {
			t.Errorf("接好了快照模块却报不可运行：%+v", k)
		}
	}
	if !saw[worker.KindBackupCreate] || !saw[worker.KindBackupRestore] {
		t.Errorf("任务类型表里少了备份或恢复：%+v", kinds)
	}
}

// 恢复的三道闸都在接口层先替用户走一遍：没确认、有分片在跑、问不到进程状态。
// 三道都必须拦下来，而且拦下来之后队列里不能留任务——那是注定失败的行。
func TestBackupRestoreGates(t *testing.T) {
	t.Run("要二次确认", func(t *testing.T) {
		bf := newBackupAPIFixture(t, "")
		rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost,
			"/api/v1/backups/"+bf.snap.ID+"/restore", backupRestoreRequest{})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("没带 confirm 应 400，实际 %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "confirm") || !strings.Contains(body, "整棵替换") {
			t.Errorf("确认提示要说清要做什么：%s", body)
		}
		if len(queuedBackupTasks(t, bf.f.srv.Store)) != 0 {
			t.Error("没确认还是入了队")
		}
	})

	t.Run("有分片在跑", func(t *testing.T) {
		bf := newBackupAPIFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")
		rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost,
			"/api/v1/backups/"+bf.snap.ID+"/restore", backupRestoreRequest{Confirm: true})
		if rec.Code != http.StatusConflict {
			t.Fatalf("分片在跑应 409，实际 %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{"Master", "先把它们全停掉", "没恢复成功"} {
			if !strings.Contains(body, want) {
				t.Errorf("409 缺了 %q：%s", want, body)
			}
		}
		if len(queuedBackupTasks(t, bf.f.srv.Store)) != 0 {
			t.Error("拦住了还入队")
		}
		assertSaveIntact(t, bf)
	})

	t.Run("问不到进程状态", func(t *testing.T) {
		bf := newBackupAPIFixture(t, "")
		bf.f.srv.Procs = procrun.New(bf.paths, failingRunner{})
		rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost,
			"/api/v1/backups/"+bf.snap.ID+"/restore", backupRestoreRequest{Confirm: true})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("问不清应 503，实际 %d: %s", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "问不清就不动") {
			t.Errorf("要把拦住的理由说清：%s", body)
		}
		assertSaveIntact(t, bf)
	})

	t.Run("目标房间不存在", func(t *testing.T) {
		bf := newBackupAPIFixture(t, "")
		rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost,
			"/api/v1/backups/"+bf.snap.ID+"/restore",
			backupRestoreRequest{TargetCluster: "Cluster_9", Confirm: true})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("目标房间不存在应 404，实际 %d: %s", rec.Code, rec.Body.String())
		}
		if len(queuedBackupTasks(t, bf.f.srv.Store)) != 0 {
			t.Error("房间不存在也入了队")
		}
	})

	t.Run("快照本身不能用时不给提交", func(t *testing.T) {
		bf := newBackupAPIFixture(t, "")
		// 把归档删掉：清单还在，于是这份快照"看得见但不能用"。
		if err := os.Remove(filepath.Join(bf.mgr.Dir, bf.snap.ID+backup.ArchiveSuffix)); err != nil {
			t.Fatal(err)
		}
		rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost,
			"/api/v1/backups/"+bf.snap.ID+"/restore", backupRestoreRequest{Confirm: true})
		if rec.Code != http.StatusConflict {
			t.Fatalf("不能恢复的快照应 409，实际 %d: %s", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "归档") {
			t.Errorf("409 要说清是归档不见了：%s", body)
		}
	})

	t.Run("都停下来才入队", func(t *testing.T) {
		bf := newBackupAPIFixture(t, "")
		rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost,
			"/api/v1/backups/"+bf.snap.ID+"/restore", backupRestoreRequest{Confirm: true})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("应 202，实际 %d: %s", rec.Code, rec.Body.String())
		}
		tasks := queuedBackupTasks(t, bf.f.srv.Store)
		if len(tasks) != 1 || tasks[0].Kind != worker.KindBackupRestore {
			t.Fatalf("队列里应该只有一条恢复任务：%+v", tasks)
		}
		var p worker.BackupRestorePayload
		if err := tasks[0].PayloadInto(&p); err != nil {
			t.Fatalf("payload 读不出: %v", err)
		}
		if p.SnapshotID != bf.snap.ID || p.TargetCluster != "Cluster_1" {
			t.Errorf("payload = %+v，期望快照 %s 回到 Cluster_1", p, bf.snap.ID)
		}
		// 接口自己绝不动存档目录：换目录是 worker 的事。
		assertSaveIntact(t, bf)
	})
}

type failingRunner struct{}

func (failingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return "", errors.New(`找不到命令 screen: exec: "screen": executable file not found in %PATH%`)
}

// 接口返回 202 之后，磁盘上的存档目录必须还是原样：
// 这一条盯的是"HTTP 层偷跑恢复"——那种 bug 只在真机上炸，而且炸掉的是世界。
func assertSaveIntact(t *testing.T, bf *backupAPIFixture) {
	t.Helper()
	for shard, slots := range map[string][]string{
		dst.ShardMaster: {"slot-0001", "slot-0002"},
		dst.ShardCaves:  {"slot-0001"},
	} {
		for _, slot := range slots {
			path := filepath.Join(bf.paths.SessionRoot(bf.f.cluster, shard), slot, "server.dat")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s 不见了：%v", path, err)
				continue
			}
			if !strings.Contains(string(raw), slot) {
				t.Errorf("%s 内容被换过了：%q", path, raw)
			}
		}
	}
	// 中间态目录（save.rollback-*）一个都不该留下。
	entries, err := os.ReadDir(bf.paths.ShardDir(bf.f.cluster, dst.ShardMaster))
	if err != nil {
		t.Logf("读不到分片目录：%v", err)
		return
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".rollback-") {
			t.Errorf("留下了恢复中间态目录 %s", e.Name())
		}
	}
}

// 列表要说清"能不能点恢复"：分片在跑就点名，问不到进程也要点名。
// 光给一个 restorable:true 是不够的，那是"这份档本身完好"，不是"现在能换"。
func TestBackupsListExplainsWhyRestoreIsBlocked(t *testing.T) {
	bf := newBackupAPIFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")
	rec := do(t, bf.f.srv.Handler(), http.MethodGet, "/api/v1/backups", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表 = %d: %s", rec.Code, rec.Body.String())
	}
	var page backupsPage
	decodeData(t, rec, &page)
	if len(page.Snapshots) != 1 {
		t.Fatalf("应该只有 fixture 那份快照：%+v", page.Snapshots)
	}
	row := page.Snapshots[0]
	if !row.Restorable {
		t.Errorf("档本身是好的，不该报不可恢复：%s", row.RestoreNote)
	}
	if strings.Join(row.RunningShards, ",") != "Master" {
		t.Errorf("在跑的分片没标出来：%+v", row)
	}
	if !strings.Contains(row.RunningNote, "先把它们全停掉") {
		t.Errorf("runningNote 不合格：%q", row.RunningNote)
	}
	// 顶部那条"哪些房间要先关服"是给恢复按钮用的，不能只有逐行提示。
	if len(page.RestoreBlocked) != 1 || page.RestoreBlocked[0].Cluster != "Cluster_1" {
		t.Errorf("restoreBlocked 没汇总出来：%+v", page.RestoreBlocked)
	}

	// 策略那一段要能把"保留几份"的来源说清，配置和设置并存时最容易各说一套。
	if page.Policy.Keep != 7 {
		t.Errorf("生效上限 = %d，配置文件那份是 7", page.Policy.Keep)
	}
	if !strings.Contains(page.Policy.KeepSource, "配置文件") {
		t.Errorf("库里没改过时要说清来自配置：%q", page.Policy.KeepSource)
	}
	if page.ScheduleHours != 0 || strings.Contains(page.Note, "定时备份每") {
		t.Errorf("默认不开定时备份，note 却说要开：%q", page.Note)
	}
	if !page.SomeBackups {
		t.Error("已经有一份快照却报没有")
	}

	// 改成库里显式设置后，界面说的数要跟着变，来源那句也要跟着变。
	if err := bf.f.srv.Store.SetSetting(store.KeyBackupKeep, "3"); err != nil {
		t.Fatal(err)
	}
	rec = do(t, bf.f.srv.Handler(), http.MethodGet, "/api/v1/backups", "admin", "secret")
	decodeData(t, rec, &page)
	if page.Policy.Keep != 3 {
		t.Errorf("设置改过了却还显示 %d", page.Policy.Keep)
	}
	if !strings.Contains(page.Policy.KeepSource, "面板设置") {
		t.Errorf("来源那句没跟着变：%q", page.Policy.KeepSource)
	}
}

// 导入抽屉里的目标房间是"全部房间"，不是"有备份的房间"。
// 所以一个刚开服、一份备份都没打过、却正在跑的房间也必须出现在 restoreBlocked 里：
// 少报它，界面上「导入并换进房间」那颗危险按钮就是亮的，用户提交下去才吃一句 409。
func TestBackupsListBlocksRunningClusterWithoutSnapshots(t *testing.T) {
	bf := newBackupAPIFixture(t, "  20999.dst-Cluster_9-Caves\t(Detached)\n")
	// 造第二个房间：只建目录和 cluster.ini，一份备份都没有。
	for _, shard := range []string{"Master", "Caves"} {
		if err := os.MkdirAll(filepath.Join(bf.paths.KleiRoot, "Cluster_9", shard), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(bf.paths.ClusterIni("Cluster_9"), []byte("[GAME]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := do(t, bf.f.srv.Handler(), http.MethodGet, "/api/v1/backups", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表 = %d: %s", rec.Code, rec.Body.String())
	}
	var page backupsPage
	decodeData(t, rec, &page)
	if len(page.Snapshots) != 1 || page.Snapshots[0].Cluster != "Cluster_1" {
		t.Fatalf("fixture 只该有一份快照：%+v", page.Snapshots)
	}

	byName := map[string]blockedCluster{}
	for _, b := range page.RestoreBlocked {
		byName[b.Cluster] = b
	}
	hit, ok := byName["Cluster_9"]
	if !ok {
		t.Fatalf("在跑却没备份的房间没进 restoreBlocked：%+v", page.RestoreBlocked)
	}
	if strings.Join(hit.Running, ",") != "Caves" {
		t.Errorf("该点名在跑的那个世界：%+v", hit)
	}
	if !strings.Contains(hit.Note, "Caves") {
		t.Errorf("note 没把在跑的世界写进去：%q", hit.Note)
	}
	if _, ok := byName["Cluster_1"]; ok {
		t.Errorf("没在跑的房间被误拦了：%+v", page.RestoreBlocked)
	}
	// 快照行上那句"这个房间还有世界在跑"不该被顺手写出来：Cluster_1 是停着的。
	if page.Snapshots[0].RunningNote != "" {
		t.Errorf("停着的房间却在快照行上报在跑：%q", page.Snapshots[0].RunningNote)
	}
}

// 问不到进程状态时列表照常给：备份还能打，只是这一页说不出谁在跑。
// 这条和总览页那次的 503 是相反的选择，理由是这里读的东西本身仍然可信。
func TestBackupsListSurvivesMissingScreen(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	bf.f.srv.Procs = procrun.New(bf.paths, failingRunner{})

	rec := do(t, bf.f.srv.Handler(), http.MethodGet, "/api/v1/backups", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("缺 screen 不该把存档页整个打挂：%d %s", rec.Code, rec.Body.String())
	}
	var page backupsPage
	decodeData(t, rec, &page)
	row := page.Snapshots[0]
	if !strings.Contains(row.RunningNote, "问不到") || len(row.RunningShards) != 0 {
		t.Errorf("要把「问不到」如实标在行上：%+v", row)
	}
	if !strings.Contains(page.Note, "问不到世界进程状态") {
		t.Errorf("页顶说明也要带上这句：%q", page.Note)
	}
	if len(page.RestoreBlocked) != 0 {
		t.Errorf("问不清时不该编出被拦的房间：%+v", page.RestoreBlocked)
	}
	// 备份入口此时仍然可用：备份不要求停服，拦下来只会让人去 cp save 目录。
	rec = do(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/clusters/Cluster_1/backups", "admin", "secret")
	if rec.Code != http.StatusAccepted {
		t.Errorf("screen 缺失时备份应照常入队，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBackupDetailVerifyAndDownload(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	h := bf.f.srv.Handler()

	rec := do(t, h, http.MethodGet, "/api/v1/backups/"+bf.snap.ID, "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("详情 = %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Snapshot backup.Snapshot   `json:"snapshot"`
		Labels   map[string]string `json:"labels"`
		Note     string            `json:"note"`
	}
	decodeData(t, rec, &detail)
	if len(detail.Snapshot.Files) == 0 {
		t.Error("详情要带文件明细，列表才省略它们")
	}
	if !strings.Contains(detail.Labels["hasSave"], "Master") {
		t.Errorf("带存档的分片没说清：%+v", detail.Labels)
	}
	if !strings.Contains(detail.Note, "不会被覆盖") {
		t.Errorf("详情要说清配置的处理方式：%q", detail.Note)
	}
	// 快照来源必须是中文说法，界面直接渲染这个 map。
	if detail.Labels["source"] != "手工备份" {
		t.Errorf("来源文案 = %q", detail.Labels["source"])
	}

	rec = do(t, h, http.MethodPost, "/api/v1/backups/"+bf.snap.ID+"/verify", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("校验 = %d: %s", rec.Code, rec.Body.String())
	}
	var v backupVerifyView
	decodeData(t, rec, &v)
	if !v.ArchiveOK || v.Checked == 0 {
		t.Errorf("刚打的快照校验不过：%+v", v)
	}
	if !strings.Contains(v.Detail, "可以放心用来恢复") {
		t.Errorf("校验通过的结论要能照着做：%q", v.Detail)
	}

	// 下载：拿到的字节就是那个归档，而且里面不能有令牌。
	rec = do(t, h, http.MethodGet, "/api/v1/backups/"+bf.snap.ID+"/download", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("下载 = %d: %s", rec.Code, rec.Body.String())
	}
	blob := archiveText(t, rec.Body.Bytes())
	if !strings.Contains(blob, "slot-0002") {
		t.Error("下载到的归档里没有快照内容")
	}
	if strings.Contains(blob, "THIS_TEXT_MUST_NEVER_BE_BACKED_UP") {
		t.Error("下载到的归档里有 Klei 令牌明文")
	}
	// 通道文件（面板自己的命令队列/状态）也不跟着存档走：换机器会留下别人的队列。
	if strings.Contains(blob, "dstpanel_status.json") {
		t.Error("下载到的归档里有面板通道文件")
	}

	entries, err := bf.f.srv.Store.ListAudit(50, 0, "backup.")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("校验和下载都要留操作记录，实际 %+v", entries)
	}
}

// 校验过的包被人动过之后，接口要报"不能用"并点出是哪几个文件。
//
// 这里钉的是一个分工，而不是"闸门设在接口层"：整包哈希一份真快照要好几秒，
// 把它挂在 HTTP 请求上，就回到了"备份成没成押在反向代理超时上"那个老问题。
// 所以恢复入队前只挡确认、房间名单和在跑的分片，逐文件校验由 worker 在动手前跑；
// 那道闸本身在 internal/backup 的 TestRestoreRefusesTamperedArchiveWithoutTouchingAnything 里钉。
func TestBackupVerifyDetectsTamperedArchive(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	// 挑清单里第一个文件，换成同样长度的别的字节：包照样解得开、条目也没少，
	// 只有逐文件哈希能发现它被动过——这正是校验这一步存在的理由。
	target := bf.snap.Files[0].Path
	if n := tamperArchiveEntry(t, filepath.Join(bf.mgr.Dir, bf.snap.ID+backup.ArchiveSuffix), target); n != 1 {
		t.Fatalf("改写命中 %d 个条目，测试本身没做对（想要 1）", n)
	}

	rec := do(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/backups/"+bf.snap.ID+"/verify", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("读得开的包要让校验跑完并报结论，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var v backupVerifyView
	decodeData(t, rec, &v)
	if v.ArchiveOK {
		t.Errorf("包被人改过而校验说没问题：%+v", v)
	}
	if len(v.Mismatched) != 1 || !strings.Contains(v.Mismatched[0], target) {
		t.Errorf("没点出被改的那个文件：%v", v.Mismatched)
	}
	if !strings.Contains(v.Detail, "不能用") || !strings.Contains(v.Detail, "哈希") {
		t.Errorf("校验不过要给出人能照着做的结论：%q", v.Detail)
	}

	// 入队仍然允许——但回话必须写明"先整包校验"，界面才不至于把 202 说成"已经换好档"。
	rec = doJSON(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/backups/"+bf.snap.ID+"/restore",
		backupRestoreRequest{Confirm: true})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("恢复入队前不该做整包哈希，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var accepted struct {
		Task taskView `json:"task"`
	}
	decodeData(t, rec, &accepted)
	if !strings.Contains(accepted.Task.Note, "整包校验") {
		t.Errorf("入队回话没交代校验这一步：%q", accepted.Task.Note)
	}
	assertSaveIntact(t, bf)
}

// tamperArchiveEntry 把归档里某个文件的内容按字节取反后原样重打包，返回命中改写的条目数。
// 长度不变，所以清单里的归档大小会和磁盘上不一样（压缩后总长会变），
// 那里填 0 表示"不和磁盘大小比"，让逐文件哈希这一关来判——不然测试会先撞在大小检查上。
func tamperArchiveEntry(t *testing.T, archive, name string) int {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hits := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("读原包: %v", err)
		}
		clone := *hdr
		var body []byte
		if hdr.Typeflag == tar.TypeReg {
			body, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("读 %s: %v", hdr.Name, err)
			}
			if hdr.Name == name {
				for i := range body {
					body[i] ^= 0xff
				}
				hits++
			}
			clone.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&clone); err != nil {
			t.Fatal(err)
		}
		if body != nil {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	mfPath := filepath.Join(filepath.Dir(archive),
		strings.TrimSuffix(filepath.Base(archive), backup.ArchiveSuffix)+backup.ManifestSuffix)
	raw, err := os.ReadFile(mfPath)
	if err != nil {
		t.Fatal(err)
	}
	var mf backup.Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatal(err)
	}
	mf.ArchiveBytes = 0
	plain, err := json.Marshal(mf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mfPath, plain, 0o644); err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestBackupIDAndClusterAreValidated(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	h := bf.f.srv.Handler()

	for _, bad := range []string{"..", "..%2F..%2Fetc%2Fpasswd", "not-a-backup-id"} {
		rec := do(t, h, http.MethodGet, "/api/v1/backups/"+bad, "admin", "secret")
		if rec.Code == http.StatusOK {
			t.Errorf("编号 %q 竟然读出了东西：%s", bad, rec.Body.String())
		}
	}
	// 房间过滤参数也走同一张磁盘名单。
	rec := do(t, h, http.MethodGet, "/api/v1/backups?cluster=Cluster_9", "admin", "secret")
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的房间过滤应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/api/v1/backups?cluster=Cluster_1", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Errorf("存在的房间过滤应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var page backupsPage
	decodeData(t, rec, &page)
	if len(page.Snapshots) == 0 {
		t.Error("按存在的房间过滤却什么都没剩")
	}
}

func TestBackupDeleteRemovesBothFilesAndSaysWhatWasLost(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	second, err := bf.mgr.Create(context.Background(), bf.f.cluster, backup.LabelScheduled)
	if err != nil {
		t.Fatalf("再打一份: %v", err)
	}
	rec := do(t, bf.f.srv.Handler(), http.MethodDelete, "/api/v1/backups/"+second.ID, "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除 = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID   string `json:"id"`
		Note string `json:"note"`
	}
	decodeData(t, rec, &out)
	if out.ID != second.ID || !strings.Contains(out.Note, "已删除") {
		t.Errorf("删除结论没说是哪份：%+v", out)
	}
	if _, err := os.Stat(filepath.Join(bf.mgr.Dir, second.ID+backup.ArchiveSuffix)); !os.IsNotExist(err) {
		t.Error("归档文件还在")
	}
	if _, err := os.Stat(filepath.Join(bf.mgr.Dir, second.ID+backup.ManifestSuffix)); !os.IsNotExist(err) {
		t.Error("清单还在")
	}
	entries, err := bf.f.srv.Store.ListAudit(20, 0, "backup.delete")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].Detail, "定时备份") {
		t.Errorf("删除要记下这份是谁的：%+v", entries)
	}
}

// 接线漏了要能一句话看出来（503），而不是 panic 成 500。
// 这一条同时是"这些 handler 真的注册到路由上了"的守卫：没注册会是 mux 的 404。
func TestBackupRoutesAreWired(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	const id = "20261031T021500Z-Cluster_1-manual"
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/backups"},
		{http.MethodPost, "/api/v1/clusters/Cluster_1/backups"},
		{http.MethodGet, "/api/v1/backups/" + id},
		{http.MethodDelete, "/api/v1/backups/" + id},
		{http.MethodGet, "/api/v1/backups/" + id + "/download"},
		{http.MethodPost, "/api/v1/backups/" + id + "/verify"},
		{http.MethodPost, "/api/v1/backups/" + id + "/restore"},
		// 导入这两条也要在里面：它们在没接上备份模块时必须和别的一样先回 503，
		// 而"回得到 503"这件事本身就证明路径注册对了（没注册会是 mux 的 404）。
		{http.MethodPost, "/api/v1/backups/import/preview"},
		{http.MethodPost, "/api/v1/backups/import"},
	} {
		rec := do(t, h, c.method, c.path, "admin", "secret")
		if strings.Contains(rec.Body.String(), "404 page not found") {
			t.Errorf("%s %s 没注册到路由上", c.method, c.path)
			continue
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s 没接快照模块时应 503，实际 %d: %s", c.method, c.path, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "没接上存档备份模块") {
			t.Errorf("%s %s 的 503 文案不合格：%s", c.method, c.path, body)
		}
	}
}

// 下载接口不能把快照目录之外的东西给出去：编号是先按格式校验过的。
func TestBackupDownloadRejectsPathishID(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	rec := do(t, bf.f.srv.Handler(), http.MethodGet, "/api/v1/backups/..%2F..%2Fpanel.db/download", "admin", "secret")
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "SQLite format") {
		t.Fatal("越界编号竟然把数据库文件下载出去了")
	}
	if rec.Code == http.StatusOK {
		t.Errorf("越界编号不该返回内容，实际 %d，长度 %d", rec.Code, rec.Body.Len())
	}
	// 正常编号仍能下载（证明上一条拦的是格式，不是整个接口坏了）。
	ok := do(t, bf.f.srv.Handler(), http.MethodGet, "/api/v1/backups/"+bf.snap.ID+"/download", "admin", "secret")
	if ok.Code != http.StatusOK || ok.Body.Len() == 0 {
		t.Errorf("正常下载失败了：%d", ok.Code)
	}
}

// 保留上限和定时间隔这两项，配置文件那份要当默认值而不是被登记常数盖掉。
// 这条测的是"yaml 写 5 结果跑的是 20"那种最容易被忽略的错。
func TestSettingsResolveConfigValueBeforeRegisteredDefault(t *testing.T) {
	f := newFixture(t, "")
	f.srv.SettingDefaults = map[string]int{store.KeyBackupKeep: 5}
	h := f.srv.Handler()

	rec := do(t, h, http.MethodGet, "/api/v1/settings", "admin", "secret")
	var list []settingView
	decodeData(t, rec, &list)
	var keep, sched settingView
	for _, v := range list {
		switch v.Key {
		case store.KeyBackupKeep:
			keep = v
		case store.KeyBackupScheduleHours:
			sched = v
		}
	}
	if keep.Default != 5 || keep.Value != "5" {
		t.Errorf("配置里写 5，界面却显示 %d/%q", keep.Default, keep.Value)
	}
	if keep.FromConfig == "" {
		t.Error("要标出这项的默认值来自配置文件哪一行")
	}
	if sched.Value != "0" || !strings.Contains(sched.Special, "不开定时备份") {
		t.Errorf("间隔 0 要说清是不开而不是永久：%+v", sched)
	}

	n, err := f.srv.settingInt(store.KeyBackupKeep)
	if err != nil || n != 5 {
		t.Errorf("库里没改过时该按配置的 5 执行，实际 %d/%v", n, err)
	}
	if err := f.srv.Store.SetSetting(store.KeyBackupKeep, "9"); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.srv.settingInt(store.KeyBackupKeep); n != 9 {
		t.Errorf("库里改过后该按 9 执行，实际 %d", n)
	}
	// 审计/趋势那两项没有配置来源，仍然走登记值。
	if n, err := f.srv.settingInt(store.KeyAuditRetentionDays); err != nil || n != 90 {
		t.Errorf("没配配置来源的项要回落登记值，实际 %d/%v", n, err)
	}

	// 界面改设置时把两项一起提交：非法值要整体拒绝，不能写进去一半。
	// 上面已经把保留上限改成过 9，所以这里查的是"有没有被这批里的 4 盖掉"，而不是"有没有值"。
	before, ok, err := f.srv.Store.Setting(store.KeyBackupKeep)
	if err != nil || !ok || before != "9" {
		t.Fatalf("前面的写入没生效，后面的断言就是在测空气：%q %v %v", before, ok, err)
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/settings", map[string]string{
		store.KeyBackupKeep: "4", store.KeyBackupScheduleHours: "abc"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法值应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	after, ok, err := f.srv.Store.Setting(store.KeyBackupKeep)
	if err != nil || !ok || after != "9" {
		t.Errorf("同批里有一项非法却还是写进了另一项：%q→%q (ok=%v err=%v)", before, after, ok, err)
	}
	if _, ok, _ := f.srv.Store.Setting(store.KeyBackupScheduleHours); ok {
		t.Error("非法的那一项被写进了库")
	}
}

// 列表页在快照目录还是空的时候就要给出"下一步做什么"，
// 否则新装的界面上一片空白，没人知道恢复动作要先有一份档。
func TestBackupsListEmptyPageGivesNextStep(t *testing.T) {
	f := newFixture(t, "")
	f.srv.Backups = &backup.Manager{
		Paths: f.paths, Dir: filepath.Join(t.TempDir(), "backups"), Keep: 20, Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/backups", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("空目录也要出得来：%d %s", rec.Code, rec.Body.String())
	}
	var page backupsPage
	decodeData(t, rec, &page)
	if page.Policy.UsedLabel != "0 B" {
		t.Errorf("空目录的已用量要写成 0 B，不然界面就是一句「已用 、磁盘还剩 …」：%+v", page.Policy)
	}
	if len(page.Snapshots) != 0 || page.SomeBackups {
		t.Errorf("还没有快照却报有：%+v", page)
	}
	if !strings.Contains(page.Note, "先点「备份存档」") {
		t.Errorf("空页要给出下一步：%q", page.Note)
	}
	if len(page.Clusters) == 0 {
		t.Error("房间名单没给，界面渲染不出备份按钮")
	}
}

func TestBackupsListLabelIsChineseNotCodeValue(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	rec := do(t, bf.f.srv.Handler(), http.MethodGet, "/api/v1/backups", "admin", "secret")
	var page backupsPage
	decodeData(t, rec, &page)
	row := page.Snapshots[0]
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "手工备份") {
		t.Errorf("列表行里要有来源的中文说法：%s", raw)
	}
	if row.LabelLabel == row.Label {
		t.Errorf("labelLabel 和码值一样，界面等于在显示码值：%q", row.LabelLabel)
	}
	// 三个路径字段是给界面当按钮用的，不能让它自己拼 URL。
	for _, p := range []struct{ name, got string }{
		{"downloadPath", row.DownloadPath}, {"detailPath", row.DetailPath}, {"restorePath", row.RestorePath},
	} {
		if !strings.Contains(p.got, row.ID) {
			t.Errorf("%s 没带上快照编号：%q", p.name, p.got)
		}
	}
}

// 请求体形状非法时要 400 并说清是哪一段的问题，
// 而不是让界面把"不是合法 JSON"当成后端故障反复重试。
func TestBackupWriteEndpointsRejectBadBodies(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	h := bf.f.srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/backups/"+bf.snap.ID+"/restore",
		strings.NewReader(`{"confirm":`))
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不是合法 JSON") {
		t.Errorf("半截 JSON 应 400 说明白，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if len(queuedBackupTasks(t, bf.f.srv.Store)) != 0 {
		t.Error("请求体都没读出来却入了队")
	}
}

// 被挡下来的恢复尝试也要留痕：事后要查得出"谁在什么时候点了哪一份、为什么没让它恢复"。
// 只记成功那一条的话，一次失败的试探在操作记录里就等于没发生过。
func TestRefusedRestoreAttemptsAreAudited(t *testing.T) {
	bf := newBackupAPIFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")
	h := bf.f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, "/api/v1/backups/"+bf.snap.ID+"/restore", backupRestoreRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没确认应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/backups/"+bf.snap.ID+"/restore", backupRestoreRequest{Confirm: true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("有分片在跑应 409，实际 %d: %s", rec.Code, rec.Body.String())
	}

	entries, err := bf.f.srv.Store.ListAudit(50, 0, "backup.restore")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("两次被拒各留一条，实际 %d：%+v", len(entries), entries)
	}
	var reasons []string
	for _, e := range entries {
		if e.OK {
			t.Errorf("被拒的恢复记成了成功：%+v", e)
		}
		if e.Target != bf.snap.ID {
			t.Errorf("记录没点名是哪一份快照：%+v", e)
		}
		if e.Actor != "admin" {
			t.Errorf("记录没说是谁点的：%+v", e)
		}
		reasons = append(reasons, e.Err)
	}
	joined := strings.Join(reasons, "|")
	if !strings.Contains(joined, "confirm") || !strings.Contains(joined, "在跑") {
		t.Errorf("被拒的原因要原话留着，实际 %q", joined)
	}
	if len(queuedBackupTasks(t, bf.f.srv.Store)) != 0 {
		t.Error("被拒的恢复还是入了队")
	}
}
