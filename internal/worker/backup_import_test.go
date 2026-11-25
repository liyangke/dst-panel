// 导入外部存档走任务队列这一层的用例。
//
// 这一层要钉住的是"什么时候不动房间目录"和"进度怎么说人话"：
//   - 只导入不恢复时，房间里的存档必须一点没动（用户可能想先做成快照再说）；
//   - 选了"导入后就换进房间"，那一步的拦截必须和恢复任务完全一致（在跑就不动），
//     因为它复用的是同一个 runRestore；
//   - 上传包是一次几十 MB 的东西：成功要删掉，失败要留着并说清为什么留着。
package worker

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/backup"
	"dstpanel/internal/dst"
	"dstpanel/internal/store"
)

const importGameID = "DCF8E1BCE43CE58A"

// externalSaveTree 是别人传过来的那份存档：房间目录打头，
// 令牌、面板通道文件、引擎日志都在里面（真实包就是都有），面板必须只取该取的那部分。
func externalSaveTree(root string) map[string]string {
	pre := root + "/"
	idx := func(name string) string {
		return "KLEI     1 \nreturn {\n    session_id = \"" + importGameID + "\",\n    name = \"" + name +
			"\",\n    root = \"" + name + "\",\n    enabled = true,\n}\n"
	}
	sess := pre + "Master/save/session/" + importGameID + "/"
	cave := pre + "Caves/save/session/" + importGameID + "/"
	return map[string]string{
		pre + "cluster.ini":                    "cluster\n",
		pre + "cluster_token.txt":              "pds-g^KU_fakeid001^THIS_TEXT_MUST_NEVER_BE_BACKED_UP\n",
		pre + "Master/save/shardindex":         idx("Master"),
		sess + "0000000085":                    "imported 地表世界\n",
		sess + "0000000085.meta":               "return {clock={cycles=74}}\n",
		pre + "Master/save/dstpanel_queue.txt": "cmd-1|kick|阿黎|恶意\n",
		pre + "Master/server_log.txt":          "[00:00:01]: old boot log line\n",
		pre + "Master/server.ini":              "[SHARD]\nrole=MASTER\n",
		pre + "Master/modoverrides.lua":        "return {}\n",
		pre + "Caves/save/shardindex":          idx("Caves"),
		cave + "0000000002":                    "imported 洞穴世界\n",
		pre + "Caves/server.ini":               "[SHARD]\nrole=CAVES\n",
	}
}

// zipBytes 把名字→内容压成 zip，不写目录条目：那是最常见也最容易漏的形状
// （zip -r 会写目录项，某些工具压出来的就没有，面板两种都得认）。
func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// stageUpload 走接口层真正会调的那一步（WriteStaged），而不是往临时目录手写一个文件：
// 上传包的编号是面板定的，测试绕过它就等于没测编号校验那一段。
func stageUpload(t *testing.T, mgr *backup.Manager, name string, files map[string]string) string {
	t.Helper()
	ref, _, err := mgr.WriteStaged(name, bytes.NewReader(zipBytes(t, files)))
	if err != nil {
		t.Fatalf("落地上传包: %v", err)
	}
	return ref
}

// enqueueImport 走完整队列：入队、领任务、执行，返回那条任务行。
// 界面上看到的 Status/Message/Progress 就是这一份。
func enqueueImport(t *testing.T, bf *backupFixture, p BackupImportPayload) store.Task {
	t.Helper()
	id, err := bf.f.st.CreateTask(KindBackupImport, "导入外部存档", p)
	if err != nil {
		t.Fatal(err)
	}
	if !bf.f.w.runOnce(context.Background()) {
		t.Fatal("队列里有任务却没领到")
	}
	task, err := bf.f.st.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// reportImport 直接调执行器，把每次 report 的百分比按顺序记下来：
// 任务行只留最后一次的百分比，中间会不会倒退只能这样验。
//
// 它和 enqueueImport 各跑一遍真实导入，同一个测试里别两个都用——
// 导入的产物是磁盘上的快照，跑两遍就多一份，后面的数量断言全在测空气。
func reportImport(t *testing.T, bf *backupFixture, p BackupImportPayload) (error, []int) {
	t.Helper()
	var percents []int
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	err = bf.f.w.backupImport(context.Background(), store.Task{Kind: KindBackupImport, Payload: string(raw)},
		func(pct int, _ string, _ *store.TaskDetail) { percents = append(percents, pct) })
	return err, percents
}

func mustPercentMonotonic(t *testing.T, percents []int) {
	t.Helper()
	if len(percents) == 0 {
		t.Fatal("一次进度都没报")
	}
	for i, p := range percents {
		if p < 1 || p > 100 {
			t.Fatalf("第 %d 次报的百分比是 %d，界面画不出来", i, p)
		}
		if i > 0 && p < percents[i-1] {
			t.Fatalf("进度从 %d%% 倒退回 %d%%（第 %d 次），用户会以为又从头做了一遍", percents[i-1], p, i)
		}
	}
}

func TestBackupImportMakesSnapshotAndLeavesRoomAlone(t *testing.T) {
	bf := newBackupFixture(t, "")
	ref := stageUpload(t, bf.mgr, "我的存档.zip", externalSaveTree("Cluster_7"))

	task := enqueueImport(t, bf, BackupImportPayload{Package: ref, Cluster: "Cluster_1"})
	if task.Status != store.TaskSucceeded {
		t.Fatalf("状态 = %s，消息 %q", task.Status, task.Message)
	}
	if task.Progress != 100 {
		t.Errorf("收尾进度是 %d，想要 100", task.Progress)
	}

	list, err := bf.mgr.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("快照目录里应该有 1 份，实际 %+v err=%v", list, err)
	}
	snap := list[0]
	if snap.Label != backup.LabelImported || snap.LabelLabel != "外部导入的存档" {
		t.Errorf("来源记成了 %q / %q", snap.Label, snap.LabelLabel)
	}
	if snap.Cluster != "Cluster_1" {
		t.Errorf("快照登记给 %s，想要导入参数里的 Cluster_1", snap.Cluster)
	}
	if !snap.Restorable {
		t.Errorf("导入出来的快照却不能恢复：%s", snap.RestoreNote)
	}
	if strings.Join(snap.SaveShards, ",") != "Caves,Master" {
		t.Errorf("两个世界都该带存档，实际 %v", snap.SaveShards)
	}

	// 没勾恢复就不能碰房间：本机那两份档还在原位，导入的世界也没被写进去。
	master := bf.paths.SessionRoot("Cluster_1", dst.ShardMaster)
	for _, slot := range []string{"slot-0001", "slot-0002"} {
		if _, err := os.Stat(filepath.Join(master, slot)); err != nil {
			t.Errorf("本机存档的 %s 被动过了：%v", slot, err)
		}
	}
	if _, err := os.Stat(filepath.Join(master, importGameID)); !os.IsNotExist(err) {
		t.Error("没勾恢复却把导入的世界写进了房间")
	}
	// 上传包用完就该消失：一份最多 512MB，留在 4G 小机的盘上是看不见的占用。
	if _, err := bf.mgr.StagedPath(ref); err == nil {
		t.Error("导入做完了，临时上传包还留在盘上")
	}
	// 令牌和通道文件不能因为"来自外部包"就绕开黑名单（快照是可以下载的）。
	blob := archiveText(t, filepath.Join(bf.mgr.Dir, snap.ID+backup.ArchiveSuffix))
	for _, banned := range []string{"THIS_TEXT_MUST_NEVER_BE_BACKED_UP", "cmd-1|kick", "old boot log line"} {
		if strings.Contains(blob, banned) {
			t.Errorf("导入的快照里带着 %q", banned)
		}
	}
	// 没勾恢复时那句结论要给出下一步，而不是只说"做完了"。
	if !strings.Contains(task.Message, "恢复") {
		t.Errorf("收尾消息没交代下一步：%q", task.Message)
	}
}

// 勾了"导入后就换进房间"，后半段必须走和恢复任务同一条路：
// 整棵换掉存档目录，并先打一份恢复前的安全备份。
func TestBackupImportWithRestoreReplacesWholeSaveTree(t *testing.T) {
	bf := newBackupFixture(t, "")
	ref := stageUpload(t, bf.mgr, "朋友给的档.zip", externalSaveTree("Cluster_7"))

	task := enqueueImport(t, bf, BackupImportPayload{Package: ref, Cluster: "Cluster_1", Restore: true})
	if task.Status != store.TaskSucceeded {
		t.Fatalf("状态 = %s，消息 %q", task.Status, task.Message)
	}

	master := bf.paths.SessionRoot("Cluster_1", dst.ShardMaster)
	body, err := os.ReadFile(filepath.Join(master, importGameID, "0000000085"))
	if err != nil || !strings.Contains(string(body), "imported") {
		t.Errorf("导入的世界没落到房间里：%v %q", err, body)
	}
	// 引擎读的是编号最大的档位，所以本机那些档位必须整批消失——
	// 只覆盖文件等于没恢复，这是整个功能最贵的一条。
	for _, slot := range []string{"slot-0001", "slot-0002"} {
		if _, err := os.Stat(filepath.Join(master, slot)); err == nil {
			t.Errorf("%s 还留在原位，引擎进世界读的还是本机那份档", slot)
		}
	}
	if _, err := os.Stat(bf.paths.SaveDir("Cluster_1", dst.ShardCaves)); err != nil {
		t.Errorf("包里带来的洞穴世界没被建出来：%v", err)
	}
	// 房间配置保持本机这份：包里的 server.ini 写的是别人的端口，照抄就是两个房间抢端口。
	got, _ := os.ReadFile(bf.paths.ShardIni("Cluster_1", dst.ShardMaster))
	if !strings.Contains(string(got), "id=Master") {
		t.Errorf("房间配置被包里的值覆盖了：%q", got)
	}
	if !strings.Contains(task.Message, "恢复前的安全备份") {
		t.Errorf("收尾要说清退路：%q", task.Message)
	}
	var safety bool
	list, _ := bf.mgr.List()
	for _, s := range list {
		if s.Label == backup.LabelBeforeRestore {
			safety = true
		}
	}
	if !safety {
		t.Error("没有留下恢复前的安全备份，出问题就没退路了")
	}
}

// 在跑的世界绝对不行：引擎会把内存里的世界按当前状态再存回去，
// 表现为"恢复没生效"。这一条拦截必须和恢复任务说的是同一句话。
func TestBackupImportWithRestoreRefusesRunningShard(t *testing.T) {
	bf := newBackupFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")
	ref := stageUpload(t, bf.mgr, "存档.zip", externalSaveTree("Cluster_7"))

	task := enqueueImport(t, bf, BackupImportPayload{Package: ref, Cluster: "Cluster_1", Restore: true})
	if task.Status != store.TaskFailed {
		t.Fatalf("世界在跑却报成功：%+v", task)
	}
	if !strings.Contains(task.Message, "还有世界在运行") {
		t.Errorf("要说清是卡在停世界上：%q", task.Message)
	}
	master := bf.paths.SessionRoot("Cluster_1", dst.ShardMaster)
	if _, err := os.Stat(filepath.Join(master, "slot-0001")); err != nil {
		t.Errorf("拦住了却把本机存档弄没了：%v", err)
	}
	// 快照已经做成了，这一点要在消息里认出来——不然用户以为整件事没发生，
	// 于是重新上传一次几十 MB。
	if !strings.Contains(task.Message, "快照") {
		t.Errorf("消息没交代快照已经做成：%q", task.Message)
	}
	list, _ := bf.mgr.List()
	if len(list) != 1 {
		t.Errorf("快照要留在盘上（点它的恢复就行），实际 %+v", list)
	}
}

// 包结构不对：拦在动手之前，房间一点没动，临时包留着（改好包再传一次是几十 MB 的事）。
func TestBackupImportRefusesUnusablePackage(t *testing.T) {
	bf := newBackupFixture(t, "")
	files := externalSaveTree("Cluster_7")
	delete(files, "Cluster_7/Master/save/shardindex")
	delete(files, "Cluster_7/Caves/save/shardindex")
	ref := stageUpload(t, bf.mgr, "少了索引的.zip", files)

	task := enqueueImport(t, bf, BackupImportPayload{Package: ref, Cluster: "Cluster_1", Restore: true})
	if task.Status != store.TaskFailed {
		t.Fatalf("没有世界索引的包却做成了快照：%+v", task)
	}
	if !strings.Contains(task.Message, "不能导入") || !strings.Contains(task.Message, "一点都没动") {
		t.Errorf("消息要让一眼看出既没做成也没动盘：%q", task.Message)
	}
	if _, err := bf.mgr.StagedPath(ref); err != nil {
		t.Errorf("失败后临时包被删了，用户只能重传一次几十 MB：%v", err)
	}
	list, _ := bf.mgr.List()
	if len(list) != 0 {
		t.Errorf("拦住了还是留下了快照：%+v", list)
	}
}

// 上传包不在了（被 TTL 收了，或者面板重启前没落地）：
// 这句必须说"重新上传一次"，而不是留一句 Go 的 file not found 在界面上。
func TestBackupImportNamesMissingUpload(t *testing.T) {
	bf := newBackupFixture(t, "")
	ref := stageUpload(t, bf.mgr, "要过期的.zip", externalSaveTree("Cluster_7"))
	if err := bf.mgr.DropStaged(ref); err != nil {
		t.Fatal(err)
	}
	task := enqueueImport(t, bf, BackupImportPayload{Package: ref, Cluster: "Cluster_1"})
	if task.Status != store.TaskFailed {
		t.Fatalf("包已经不在了却报成功：%+v", task)
	}
	if !strings.Contains(task.Message, "重新上传") {
		t.Errorf("没给出下一步：%q", task.Message)
	}
}

// 编号形状不合法的一律不按它去找文件：这个字符串来自请求参数并参与拼路径。
func TestBackupImportRejectsPathishRefs(t *testing.T) {
	bf := newBackupFixture(t, "")
	for _, bad := range []string{"", "..", "../Cluster_1", "my-handmade.zip", "import-../x.zip"} {
		raw, err := json.Marshal(BackupImportPayload{Package: bad, Cluster: "Cluster_1"})
		if err != nil {
			t.Fatal(err)
		}
		err = bf.f.w.backupImport(context.Background(), store.Task{Kind: KindBackupImport, Payload: string(raw)},
			func(int, string, *store.TaskDetail) {})
		if err == nil || !strings.Contains(err.Error(), "不是面板生成的格式") {
			t.Errorf("编号 %q 被放过去了：%v", bad, err)
		}
	}
}

// 房间在任务排队期间被人删掉是真实会发生的（任务行是持久化的），
// 这时必须停在动手之前。
func TestBackupImportRefusesVanishedCluster(t *testing.T) {
	bf := newBackupFixture(t, "")
	ref := stageUpload(t, bf.mgr, "存档.zip", externalSaveTree("Cluster_7"))
	task := enqueueImport(t, bf, BackupImportPayload{Package: ref, Cluster: "Cluster_9"})
	if task.Status != store.TaskFailed {
		t.Fatalf("房间不存在却做成了快照：%+v", task)
	}
	if !strings.Contains(task.Message, "Cluster_9") || !strings.Contains(task.Message, "不存在") {
		t.Errorf("要点名是哪个房间不存在：%q", task.Message)
	}
	list, _ := bf.mgr.List()
	if len(list) != 0 {
		t.Errorf("拦住了还是打出了快照：%+v", list)
	}
}

// 临时上传必须会自己消失：用户传完没收尾是常态，而一份最多 512MB。
// 清理挂在导入这条任务上（面板重启时另有启动那一次兜），所以要验它真的生效。
func TestBackupImportSweepsExpiredUploads(t *testing.T) {
	bf := newBackupFixture(t, "")
	expire := stageUpload(t, bf.mgr, "上次传一半的.zip", externalSaveTree("Cluster_3"))
	path := filepath.Join(bf.mgr.Dir, "incoming", expire)
	stamp := time.Now().Add(-backup.StagedUploadTTL - time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	fresh := stageUpload(t, bf.mgr, "这次的.zip", externalSaveTree("Cluster_7"))

	task := enqueueImport(t, bf, BackupImportPayload{Package: fresh, Cluster: "Cluster_1"})
	if task.Status != store.TaskSucceeded {
		t.Fatalf("状态 = %s，消息 %q", task.Status, task.Message)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("过期的临时上传还留在盘上")
	}
}

// 解包 → 打包 → 恢复三段拼在一条任务里，百分比必须一路向前：
// 倒退在界面上读起来就是"又从头做了一遍"，而它其实只是在换存档目录。
func TestBackupImportProgressNeverGoesBackwards(t *testing.T) {
	bf := newBackupFixture(t, "")
	ref := stageUpload(t, bf.mgr, "存档.zip", externalSaveTree("Cluster_7"))

	err, percents := reportImport(t, bf, BackupImportPayload{Package: ref, Cluster: "Cluster_1", Restore: true})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	mustPercentMonotonic(t, percents)
	if percents[0] > 20 {
		t.Errorf("第一段进度起报在 %d%%，解包该从个位数起", percents[0])
	}
	if last := percents[len(percents)-1]; last != 100 {
		t.Errorf("最后一步是 %d%%，想要 100%%（界面靠它把任务判成做完）", last)
	}
	// 三段都要有东西可报：解包和打包按条目走，恢复按阶段走。
	var unpacking, packing, restoring bool
	for _, p := range percents {
		switch {
		case p <= 45:
			unpacking = true
		case p <= 70:
			packing = true
		default:
			restoring = true
		}
	}
	if !unpacking || !packing || !restoring {
		t.Errorf("三段进度缺了段落：解包 %v 打包 %v 恢复 %v → %v", unpacking, packing, restoring, percents)
	}
}

// 接线漏了要一眼看出来，而不是半路 panic：备份模块没接上时两类任务都这样。
func TestBackupImportSaysWhenModuleMissing(t *testing.T) {
	bf := newBackupFixture(t, "")
	bf.f.w.Backups = nil
	raw, err := json.Marshal(BackupImportPayload{Package: "x", Cluster: "Cluster_1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := bf.f.w.backupImport(context.Background(), store.Task{Kind: KindBackupImport, Payload: string(raw)},
		func(int, string, *store.TaskDetail) {}); err == nil {
		t.Fatal("没接备份模块却报成功")
	} else if !strings.Contains(err.Error(), "没接上存档备份模块") {
		t.Errorf("接线漏了要说清：%v", err)
	}
	if err := bf.f.w.backupRestore(context.Background(),
		store.Task{Kind: KindBackupRestore, Payload: `{"snapshotId":"20261102T071500Z-Cluster_1-manual"}`},
		func(int, string, *store.TaskDetail) {}); err == nil ||
		!strings.Contains(err.Error(), "没接上存档备份模块") {
		t.Errorf("恢复任务没接上时也没说清：%v", err)
	}
}

// 导入这条 kind 必须在提交白名单里（否则界面点不动、通用提交口又拒收），
// 而它按危险动作登记：勾选"导入后就换进房间"时改的是世界本身。
func TestBackupImportKindRegistered(t *testing.T) {
	k, ok := KindSpec(KindBackupImport)
	if !ok {
		t.Fatal("kinds 里没有导入外部存档这一类任务")
	}
	if !k.Danger {
		t.Error("导入可以换掉存档目录，界面必须二次确认")
	}
	if k.SubmitVia == "" {
		t.Error("payload 里有房间名和上传包编号，通用提交口不该收，要给专门入口")
	}
	if strings.Contains(k.Desc, "backup.import") {
		t.Errorf("说明里出现了裸码值：%s", k.Desc)
	}
	if !strings.Contains(k.Desc, "拦下") {
		t.Errorf("说明里要讲清为什么要拦：放行了引擎会静默生成新世界：%s", k.Desc)
	}
}

// runRestore 是两条任务共用的那一段，"这份快照不能用来恢复"必须说不碰盘——
// 这句话在导入那条路上同样要说，因为界面共用一套结论。
func TestRunRestoreRefusesUnusableSnapshotWithoutTouchingDisk(t *testing.T) {
	bf := newBackupFixture(t, "")
	snap, err := bf.mgr.Create(context.Background(), "Cluster_1", backup.LabelManual)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bf.mgr.Dir, snap.ID+backup.ArchiveSuffix)); err != nil {
		t.Fatal(err)
	}
	var reports []int
	err = bf.f.w.runRestore(context.Background(), snap.ID, "", func(p int, _ string, _ *store.TaskDetail) {
		reports = append(reports, p)
	})
	if err == nil {
		t.Fatal("归档不见了还能恢复")
	}
	if !strings.Contains(err.Error(), "一点都没动") {
		t.Errorf("要说清没碰盘：%v", err)
	}
	if len(reports) != 0 {
		t.Errorf("还没开始动就报了进度：%v", reports)
	}
	// 本机存档必须还在原位。
	if _, err := os.Stat(filepath.Join(bf.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-0002")); err != nil {
		t.Errorf("拦住了却把本机存档弄没了：%v", err)
	}
}

// 目标房间留空 = 恢复到快照自己的房间。这句默认必须在 runRestore 里，
// 否则导入那条路（没有 targetCluster 这个参数）就得自己再判一次，两处迟早分叉。
func TestRunRestoreDefaultsToSnapshotCluster(t *testing.T) {
	bf := newBackupFixture(t, "")
	snap, err := bf.mgr.Create(context.Background(), "Cluster_1", backup.LabelManual)
	if err != nil {
		t.Fatal(err)
	}
	// 快照之后又存了一次盘，多出 slot-0003：恢复要把它抹掉。
	mustWrite(t, filepath.Join(bf.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-0003", "server.dat"), "更新的档\n")
	mustWrite(t, filepath.Join(bf.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-0002", "server.dat"), "改过的档\n")

	if err := bf.f.w.runRestore(context.Background(), snap.ID, "", func(int, string, *store.TaskDetail) {}); err != nil {
		t.Fatalf("恢复到自己的房间失败: %v", err)
	}
	master := bf.paths.SessionRoot("Cluster_1", dst.ShardMaster)
	if _, err := os.Stat(filepath.Join(master, "slot-0003")); !os.IsNotExist(err) {
		t.Error("没指定目标房间时没有换掉快照自己的房间")
	}
	body, _ := os.ReadFile(filepath.Join(master, "slot-0002", "server.dat"))
	if !strings.Contains(string(body), "slot-0002") {
		t.Errorf("恢复回来的内容不对：%q", body)
	}
}
