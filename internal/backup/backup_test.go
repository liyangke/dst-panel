package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/dst"
)

// 这个测试文件里最要紧的是 TestRestoreReplacesWholeSaveTree：
// 它盯的是 DST 特有的那个坑——引擎读的是编号最大的 slot，
// 所以"只覆盖文件"的恢复会静默失效。其余用例盯的是令牌泄漏、校验和保留策略。

type fixture struct {
	root    string
	klei    string
	backups string
	paths   dst.Paths
	mgr     *Manager
	written int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		root:    root,
		klei:    filepath.Join(root, "klei", "DoNotStarveTogether"),
		backups: filepath.Join(root, "panel", "backups"),
	}
	if err := os.MkdirAll(f.klei, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.backups, 0o755); err != nil {
		t.Fatal(err)
	}
	f.paths = dst.Paths{KleiRoot: f.klei, DstInstall: filepath.Join(root, "install")}
	f.mgr = &Manager{Paths: f.paths, Dir: f.backups, Keep: 20, Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return f
}

// writeCluster 造一个有 Master + Caves 的房间：
// 每个分片有 save/session/<GAMEID>/slot-N/、cluster_token.txt、面板通道文件和日志。
func (f *fixture) writeCluster(t *testing.T, cluster string, masterSlots, cavesSlots []string) {
	t.Helper()
	f.writeFile(t, f.paths.ClusterIni(cluster), "cluster\n")
	f.writeFile(t, f.paths.TokenFile(cluster), "pds-g^KU_fakeid001^THIS_TEXT_MUST_NEVER_BE_BACKED_UP\n")
	for shard, slots := range map[string][]string{dst.ShardMaster: masterSlots, dst.ShardCaves: cavesSlots} {
		f.writeFile(t, f.paths.ShardIni(cluster, shard), fmt.Sprintf("[shard]\nid=%s\n", shard))
		f.writeFile(t, f.paths.WorldGenOverride(cluster, shard),
			fmt.Sprintf("return {preset=\"%s\", enabled=true}\n", "DST_"+"forest"))
		f.writeFile(t, f.paths.ServerLog(cluster, shard), "[00:00:01]: old boot log line\n")
		f.writeFile(t, f.paths.ChatLog(cluster, shard), "[00:00:02]: [Announcement] 旧公告\n")
		f.writeFile(t, filepath.Join(f.paths.SaveDir(cluster, shard), "dstpanel_queue.txt"),
			"cmd-1|kick|阿黎|恶意\n")
		f.writeFile(t, filepath.Join(f.paths.SaveDir(cluster, shard), "dstpanel_status.json"), `{"shard":"Master"}`)
		f.writeFile(t, filepath.Join(f.paths.SaveDir(cluster, shard), "dstpanel_queue.txt.bak"), "cmd-0|old\n")
		f.writeFile(t, filepath.Join(f.paths.SaveDir(cluster, shard), "world"), "worldsave-header\n")
		for _, slot := range slots {
			f.writeFile(t, filepath.Join(f.paths.SessionRoot(cluster, shard), slot, "server.dat"),
				fmt.Sprintf("%s/%s 世界内容\n", shard, slot))
			f.writeFile(t, filepath.Join(f.paths.SessionRoot(cluster, shard), slot, "worldsnippet.bin"),
				fmt.Sprintf("%s/%s 片段\n", shard, slot))
		}
	}
	// 空的 mod_config_data 目录：只以目录形式存在，恢复后必须还是目录。
	if err := os.MkdirAll(filepath.Join(f.paths.SaveDir(cluster, dst.ShardMaster), "mod_config_data"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f.written++
}

func (f *fixture) read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(raw)
}

// snapshotIDs 把列表里的编号取出来，顺序就是列表给的顺序。
func snapshotIDs(list []Snapshot) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}

func mustCreate(t *testing.T, f *fixture, cluster, label string) Snapshot {
	t.Helper()
	snap, err := f.mgr.Create(context.Background(), cluster, label)
	if err != nil {
		t.Fatalf("备份 %s 失败: %v", cluster, err)
	}
	return snap
}

// ---- 打包 ----

func TestCreateRegistersSaveTreeAndShards(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1", "slot-2"}, []string{"slot-1"})

	snap := mustCreate(t, f, "Cluster_1", LabelManual)

	if snap.Cluster != "Cluster_1" || snap.Label != LabelManual {
		t.Fatalf("清单记的集群/来源不对：%+v", snap)
	}
	if snap.LabelLabel != "手工备份" {
		t.Errorf("来源的中文标签是 %q，想要 手工备份", snap.LabelLabel)
	}
	if !snap.Restorable {
		t.Fatalf("刚打完的快照应该可以恢复，实际原因：%s", snap.RestoreNote)
	}
	if strings.Join(snap.SaveShards, ",") != "Caves,Master" {
		t.Errorf("带存档的分片是 %v，想要 Caves,Master", snap.SaveShards)
	}
	if strings.Join(snap.Shards, ",") != "Caves,Master" {
		t.Errorf("清单里的分片是 %v", snap.Shards)
	}
	// 两个分片各 2 个 slot 文件 + 1 个 Caves slot = 5，加上 world、mod_config_data 目录、
	// cluster_token 被跳过，通道文件被跳过，日志被跳过。这里按前缀数普通文件。
	var files, saveFiles int
	for _, e := range snap.Files {
		files++
		if strings.Contains(e.Path, "/save/") {
			saveFiles++
		}
	}
	if saveFiles != 8 {
		t.Errorf("save 目录里登记了 %d 个文件，想要 8（Master 4 个 slot 文件 + 2 个 world + Caves 2 个）", saveFiles)
	}
	if files != snap.FileCount {
		t.Errorf("FileCount=%d 和实际列出来的 %d 个不一致", snap.FileCount, files)
	}
	if snap.UncompressedBytes <= 0 || snap.ArchiveBytes <= 0 {
		t.Errorf("大小没记上：解压前 %d，包 %d", snap.UncompressedBytes, snap.ArchiveBytes)
	}
	// 归档内路径必须以斜杠分隔并且带分片前缀——恢复时靠它认目的地。
	for _, e := range snap.Files {
		if strings.Contains(e.Path, `\`) {
			t.Fatalf("归档路径里有反斜杠：%s", e.Path)
		}
		if !strings.HasPrefix(e.Path, "Master/") && !strings.HasPrefix(e.Path, "Caves/") && e.Path != "cluster.ini" {
			t.Errorf("归档路径 %q 不在预期前缀下", e.Path)
		}
	}
	// 清单落盘了，且 JSON 里能读回来。
	mfPath := filepath.Join(f.backups, snap.ID+ManifestSuffix)
	raw, err := os.ReadFile(mfPath)
	if err != nil {
		t.Fatalf("清单没写到磁盘：%v", err)
	}
	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("清单不是合法 JSON: %v", err)
	}
	if back.ID != snap.ID || back.FileCount != snap.FileCount {
		t.Errorf("清单读回来对不上：%+v", back)
	}
}

func TestCreateSkipsTokenChannelFilesAndLogs(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	// 面板没预料到的房间文件也必须进包：封禁名单是玩家数据，白名单式打包会安静地丢掉它。
	f.writeFile(t, filepath.Join(f.paths.ShardDir("Cluster_1", dst.ShardMaster), "blocklist.txt"), "KLEI-xxx\n")
	snap := mustCreate(t, f, "Cluster_1", LabelManual)

	var packed []string
	for _, e := range snap.Files {
		packed = append(packed, e.Path)
		switch filepath.Base(filepath.FromSlash(e.Path)) {
		case "cluster_token.txt", "server_log.txt", "server_chat_log.txt":
			t.Fatalf("令牌或日志被打进了快照：%s", e.Path)
		}
		if strings.HasPrefix(filepath.Base(filepath.FromSlash(e.Path)), "dstpanel_") {
			t.Fatalf("面板命令通道文件被打进了快照：%s", e.Path)
		}
	}
	if !contains(packed, "Master/blocklist.txt") {
		t.Errorf("房间里的封禁名单没被打进快照，恢复出来它会变空：%v", packed)
	}
	joined := strings.Join(snap.Skipped, "\n")
	for _, want := range []string{"cluster_token.txt", "dstpanel_queue.txt", "dstpanel_queue.txt.bak",
		"dstpanel_status.json", "server_log.txt", "server_chat_log.txt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("跳过的名单里少了 %s，实际：%s", want, joined)
		}
	}
	// 最硬的一条：整个包（解开之后）里不能出现令牌的那段明文。
	blob := archiveText(t, filepath.Join(f.backups, snap.ID+ArchiveSuffix))
	if strings.Contains(blob, "THIS_TEXT_MUST_NEVER_BE_BACKED_UP") {
		t.Fatal("归档内容里有令牌明文")
	}
	if strings.Contains(blob, "pds-g") {
		t.Fatal("归档内容里有令牌前缀")
	}
	// 队列文件也不能在包里：它是"恢复出去的命令"。
	if strings.Contains(blob, "cmd-1|kick") {
		t.Fatal("归档里有助手命令队列的内容")
	}
}

// archiveText 把 tar.gz 里所有普通文件的内容拼成一个字符串（只给测试做包含判断用）。
func archiveText(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
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
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		buf.WriteString("=== " + hdr.Name + " ===\n")
		if _, err := io.Copy(&buf, tr); err != nil {
			t.Fatal(err)
		}
	}
	return buf.String()
}

func TestCreateRejectsUnknownClusterAndPathishName(t *testing.T) {
	f := newFixture(t)
	if _, err := f.mgr.Create(context.Background(), "Cluster_9", LabelManual); err == nil {
		t.Error("集群不存在时 Create 应该报错")
	} else if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("报错没说不存在：%v", err)
	}
	for _, bad := range []string{"..", "../Cluster_1", "a/b", "."} {
		if _, err := f.mgr.Create(context.Background(), bad, LabelManual); err == nil {
			t.Errorf("集群名 %q 被当成了合法名字", bad)
		}
	}
}

func TestCreateTwiceInSameSecondDoesNotOverwrite(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, nil)
	first := mustCreate(t, f, "Cluster_1", LabelManual)
	second := mustCreate(t, f, "Cluster_1", LabelManual)
	if first.ID == second.ID {
		t.Fatalf("同一秒里两次备份用了同一个编号 %q，前一份会被覆盖", first.ID)
	}
	list, err := f.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("列表里应该有 2 份，实际 %d：%v", len(list), snapshotIDs(list))
	}
	// 序号只体现在编号（文件名）里，清单里的来源仍是 manual：
	// 两份都该老实显示"手工备份"，谁也不该退化成码值或"未知来源"。
	for _, want := range []Snapshot{findSnapshot(list, first.ID), findSnapshot(list, second.ID)} {
		if want.LabelLabel != "手工备份" {
			t.Errorf("同一秒里的两份来源说法不对：label=%q labelLabel=%q", want.Label, want.LabelLabel)
		}
	}
	if !strings.HasSuffix(second.ID, "-manual-2") {
		t.Errorf("撞名时编号该带上来源后缀来区分两份，实际第二份 %q", second.ID)
	}
}

func findSnapshot(list []Snapshot, id string) Snapshot {
	for _, s := range list {
		if s.ID == id {
			return s
		}
	}
	panic("列表里没有 " + id)
}

func TestCreateFailsWhenCancelled(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.mgr.Create(ctx, "Cluster_1", LabelManual); err == nil {
		t.Fatal("ctx 已结束时备份不该报成功")
	} else if !strings.Contains(err.Error(), "中断") {
		t.Errorf("报错没说是被中断：%v", err)
	}
	// 失败的现场必须干净：快照目录里不能留下半截包或孤儿清单。
	entries, err := os.ReadDir(f.backups)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.HasSuffix(e.Name(), ArchiveSuffix) ||
			strings.HasSuffix(e.Name(), ManifestSuffix) {
			t.Fatalf("备份失败后留下了残留文件：%s", e.Name())
		}
	}
}

// ---- 校验 ----

func TestVerifyDetectsTamperedArchive(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	snap := mustCreate(t, f, "Cluster_1", LabelManual)

	res, err := f.mgr.Verify(context.Background(), snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ArchiveOK || res.Checked == 0 {
		t.Fatalf("原样的快照校验不过：%+v", res)
	}
	if !strings.Contains(res.Note, "全部和清单对得上") {
		t.Errorf("校验通过的话没说清结论：%s", res.Note)
	}

	// 手工把包里的一个文件换成别的字节（长度也一样），只有哈希能发现它。
	archive := filepath.Join(f.backups, snap.ID+ArchiveSuffix)
	target := snap.Files[0].Path
	if hits := rewriteArchiveEntry(t, archive, target, []byte("tampered-by-hand---")); hits != 1 {
		t.Fatalf("改写命中 %d 个条目，测试本身没做对（想要 1）", hits)
	}
	res, err = f.mgr.Verify(context.Background(), snap.ID)
	if err != nil {
		t.Fatalf("校验被改过的包也要能跑完：%v", err)
	}
	if res.ArchiveOK {
		t.Fatal("包被人改过而校验说没问题")
	}
	if len(res.Mismatched) != 1 || !strings.Contains(res.Mismatched[0], target) {
		t.Errorf("没指认出被改的那个文件：%v", res.Mismatched)
	}
	if !strings.Contains(res.Note, "别拿它做恢复") {
		t.Errorf("校验失败的话没给下一步：%s", res.Note)
	}
}

// rewriteArchiveEntry 把归档里某个文件的内容换掉，其他条目原样重打包。
// 返回命中改写的条目数：调用方要确认"改的就是那一个文件"，否则后面的断言全在测空气。
func rewriteArchiveEntry(t *testing.T, archive, name string, content []byte) int {
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
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		clone := *hdr
		if hdr.Name == name && hdr.Typeflag == tar.TypeReg {
			clone.Size = int64(len(content))
			hits++
			_ = tw.WriteHeader(&clone)
			_, _ = tw.Write(content)
			continue
		}
		_ = tw.WriteHeader(&clone)
		if hdr.Typeflag == tar.TypeReg {
			_, _ = io.Copy(tw, tr)
		}
	}
	_ = tw.Close()
	_ = gw.Close()
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// 大小变了，清单里那个归档大小也要跟着改，否则测试会先撞上"大小对不上"那条检查，
	// 而不是我们想验的逐文件哈希。
	mfPath := filepath.Join(filepath.Dir(archive), strings.TrimSuffix(filepath.Base(archive), ArchiveSuffix)+ManifestSuffix)
	var mf Manifest
	raw, err := os.ReadFile(mfPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatal(err)
	}
	mf.ArchiveBytes = 0 // 0 表示"不和磁盘大小比"，让逐文件哈希那条来判
	plain, _ := json.Marshal(mf)
	if err := os.WriteFile(mfPath, plain, 0o644); err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestVerifyRejectsMissingAndTruncatedArchive(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	snap := mustCreate(t, f, "Cluster_1", LabelManual)
	archive := filepath.Join(f.backups, snap.ID+ArchiveSuffix)

	// 截断：留一半。
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Verify(context.Background(), snap.ID); err == nil {
		t.Fatal("半截包应该报解压失败")
	}
	view, err := f.mgr.Get(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Restorable {
		t.Errorf("半截包在列表里还标成可恢复")
	}
}

// ---- 列表 / 保留 ----

func TestListOrdersNewestFirstAndShowsOrphans(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	first := mustCreate(t, f, "Cluster_1", LabelManual)
	time.Sleep(1100 * time.Millisecond) // 编号精度到秒，隔一秒才能保证顺序可判
	f.writeFile(t, filepath.Join(f.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-2", "server.dat"), "new\n")
	second := mustCreate(t, f, "Cluster_1", LabelScheduled)

	// 用户自己拷进来一个包：没有清单，名字也不合格式。
	orphan := filepath.Join(f.backups, "my-handmade-backup.tar.gz")
	if err := os.WriteFile(orphan, []byte("not a panel archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := f.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("列表里有 %d 条，想要 3：%v", len(list), snapshotIDs(list))
	}
	if list[0].ID != second.ID || list[1].ID != first.ID {
		t.Errorf("顺序不是新→旧：%v", snapshotIDs(list))
	}
	if list[0].LabelLabel != "定时备份" {
		t.Errorf("定时备份的中文标签是 %q", list[0].LabelLabel)
	}
	// 列表项要给出文件数（界面靠它说"这份档有多少个文件"），但不展开逐个文件：
	// 20 份 × 上百个文件、每个还带 sha256，会把列表响应撑大好几倍。明细在详情接口里。
	if list[0].FileCount == 0 {
		t.Errorf("列表项连文件数都没算出来：%+v", list[0])
	}
	if len(list[0].Files) != 0 {
		t.Errorf("列表项把逐个文件的清单也带出来了（%d 条），应该只给计数", len(list[0].Files))
	}
	// 认不出的文件名必须被说出来，不能只显示"有 2 份快照"。
	tail := list[len(list)-1]
	if tail.Restorable {
		t.Errorf("手工包被当成可恢复的快照了")
	}
	if !strings.Contains(tail.Manifest.Note, "my-handmade-backup.tar.gz") {
		t.Errorf("没把认不出的文件名列出来：%+v", tail)
	}
	if !strings.Contains(tail.RestoreNote, "不会被列出、下载或清理") {
		t.Errorf("没解释清楚这些文件的处置：%s", tail.RestoreNote)
	}
}

func TestListReportsManifestWithoutArchive(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	snap := mustCreate(t, f, "Cluster_1", LabelManual)
	if err := os.Remove(filepath.Join(f.backups, snap.ID+ArchiveSuffix)); err != nil {
		t.Fatal(err)
	}
	list, err := f.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("只剩清单时列表有 %d 条", len(list))
	}
	if list[0].Restorable {
		t.Error("归档不见了还标可恢复")
	}
	if !strings.Contains(list[0].RestoreNote, "归档文件不见了") {
		t.Errorf("没说原因：%s", list[0].RestoreNote)
	}
	// 归档缺失只该让"能不能恢复"这一项变坏，不该把清单里读得出的东西一起抹掉：
	// 界面要靠时间、来源、文件数告诉用户"丢的是哪一天那一份"。
	// 实测踩过先 Stat 后读清单的顺序，那一行退化成一串 0 和空串。
	if list[0].CreatedAt == "" {
		t.Error("归档不在就连打包时间都不报了，列表里这一行没法认")
	}
	if list[0].FileCount == 0 || list[0].UncompressedBytes == 0 {
		t.Errorf("清单里的字段没读出来：%+v", list[0])
	}
	if len(list[0].SaveShards) == 0 {
		t.Error("归档不在就把带存档的分片报成没有，清单里明明列着")
	}
	if list[0].ArchiveBytes != 0 {
		t.Errorf("归档文件都不在了还报大小 %d，界面会自相矛盾", list[0].ArchiveBytes)
	}
	if list[0].LabelLabel == "" {
		t.Error("来源的中文标签没给")
	}
	if strings.Contains(list[0].RestoreNote, "The system cannot find") ||
		strings.Contains(list[0].RestoreNote, "no such file") {
		t.Errorf("界面文案里带了操作系统的原始报错：%s", list[0].RestoreNote)
	}
}

func TestPruneDeletesOldestUpToKeep(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	f.mgr.Keep = 2
	var ids []string
	for i := 0; i < 4; i++ {
		snap := mustCreate(t, f, "Cluster_1", LabelManual)
		ids = append(ids, snap.ID)
		time.Sleep(1100 * time.Millisecond)
	}
	list, err := f.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	var real []Snapshot
	for _, s := range list {
		if !strings.HasPrefix(s.ID, "(") {
			real = append(real, s)
		}
	}
	if len(real) != 2 {
		t.Fatalf("Keep=2 却剩了 %d 份：%v", len(real), snapshotIDs(real))
	}
	// 留下的必须是最新那两份。
	if real[0].ID != ids[3] || real[1].ID != ids[2] {
		t.Errorf("留下的是 %v，想要 %v、%v", snapshotIDs(real), ids[3], ids[2])
	}
	for _, gone := range ids[:2] {
		if _, err := os.Stat(filepath.Join(f.backups, gone+ArchiveSuffix)); err == nil {
			t.Errorf("最旧的 %s 没被清掉", gone)
		}
		if _, err := os.Stat(filepath.Join(f.backups, gone+ManifestSuffix)); err == nil {
			t.Errorf("%s 的清单没跟着删，列表里会留一条点不动的", gone)
		}
	}
	// 最新那份要能说清"顺手清理了几份"，否则用户不知道为什么少了两档。
	// 每打一份就按 Keep 收一次尾，所以每次只删最旧那一份，而不是最后一次删两份。
	if !strings.Contains(real[0].Note, "按保留上限 2 份清理了 1 份最旧的备份") {
		t.Errorf("最新那份的说明没提清理：%s", real[0].Note)
	}
}

// 保住"此刻正被用来恢复的那一份"不能把上限撑破：界面上写着最多留 3 份，
// 目录里却躺着 4 份，用户信不过的就是这条清理。名额让给受保护那份，
// 该删的从次旧的开始删。
func TestPruneStaysInsideLimitWhenOldestIsProtected(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	f.mgr.Keep = 3
	var ids []string
	for i := 0; i < 3; i++ {
		snap := mustCreate(t, f, "Cluster_1", LabelManual)
		ids = append(ids, snap.ID)
		time.Sleep(1100 * time.Millisecond)
	}
	// 第 4 份是"恢复到最旧那份时自动打的安全备份"，同一次流程里它和最旧那份都要保住。
	if _, err := f.mgr.Create(context.Background(), "Cluster_1", LabelBeforeRestore, ids[0]); err != nil {
		t.Fatal(err)
	}
	list, err := f.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	var real []Snapshot
	for _, s := range list {
		if !strings.HasPrefix(s.ID, "(") {
			real = append(real, s)
		}
	}
	if len(real) != 3 {
		t.Fatalf("上限 3 份却剩 %d 份：%v", len(real), snapshotIDs(real))
	}
	if !hasID(real, ids[0]) {
		t.Errorf("正在被用来恢复的那份 %s 被清掉了：%v", ids[0], snapshotIDs(real))
	}
	if hasID(real, ids[1]) {
		t.Errorf("次旧的 %s 该让位给受保护那份：%v", ids[1], snapshotIDs(real))
	}
	if _, err := os.Stat(filepath.Join(f.backups, ids[0]+ArchiveSuffix)); err != nil {
		t.Errorf("受保护那份的归档不在了：%v", err)
	}
}

func hasID(list []Snapshot, id string) bool {
	for _, s := range list {
		if s.ID == id {
			return true
		}
	}
	return false
}

func TestKeepZeroMeansNoAutoDelete(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	f.mgr.Keep = 0
	for i := 0; i < 3; i++ {
		mustCreate(t, f, "Cluster_1", LabelManual)
		time.Sleep(1100 * time.Millisecond)
	}
	list, err := f.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("keep=0 本意是都别删，结果剩 %d 份", len(list))
	}
}

// ---- 恢复 ----

// TestRestoreReplacesWholeSaveTree 是整个功能的核心断言：
// 备份之后有人又存了两次档（多出 slot-2、slot-3），恢复必须把整棵 save 换掉，
// 让那两个更新的 slot 消失。只覆盖文件的话引擎照样读编号最大的那个，
// 世界回到不了备份时刻，而且一句错都不报。
func TestRestoreReplacesWholeSaveTree(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	snap := mustCreate(t, f, "Cluster_1", LabelManual)

	masterSession := f.paths.SessionRoot("Cluster_1", dst.ShardMaster)
	// 备份之后的世界：多了一个 slot-3，还把 slot-1 的内容改了。
	f.writeFile(t, filepath.Join(masterSession, "slot-3", "server.dat"), "Master/slot-3 新的世界\n")
	f.writeFile(t, filepath.Join(masterSession, "slot-1", "server.dat"), "slot-1 被后来的存档改过\n")
	// 备份期间面板又发过一条命令，队列里留着痕迹：恢复后不该带回来。
	f.writeFile(t, filepath.Join(f.paths.SaveDir("Cluster_1", dst.ShardMaster), "dstpanel_queue.txt"), "cmd-9|kick|x|y\n")

	res, err := f.mgr.Restore(context.Background(), snap.ID, Options{})
	if err != nil {
		t.Fatalf("恢复失败: %v（结果 %+v）", err, res)
	}
	if !res.Verified {
		t.Errorf("恢复完说没做逐个校验：%+v", res)
	}
	if res.Aftermath != "" {
		t.Errorf("恢复成功了却还留着失败现场标记 %q：%+v", res.Aftermath, res)
	}
	if strings.Join(res.ReplacedShards, ",") != "Caves,Master" {
		t.Errorf("被替换的分片是 %v", res.ReplacedShards)
	}
	if res.SafetyBackupID == "" || res.SafetyBackupID == snap.ID {
		t.Errorf("恢复前没另打一份安全备份：%q", res.SafetyBackupID)
	}
	// 1) 比快照新的 slot-3 必须彻底消失。
	if _, err := os.Stat(filepath.Join(masterSession, "slot-3")); err == nil {
		t.Fatal("slot-3 还在：引擎会读编号最大的 slot，这样恢复等于没做")
	}
	// 2) 备份时刻的内容原样回来（连被改过的 slot-1 一起）。
	got := f.read(t, filepath.Join(masterSession, "slot-1", "server.dat"))
	if got != "Master/slot-1 世界内容\n" {
		t.Errorf("slot-1 恢复出来的内容是 %q，想要备份时那一份", got)
	}
	// 3) 安全备份里必须留着恢复前那个世界（含 slot-3），这是唯一的退路。
	safetyText := archiveText(t, filepath.Join(f.backups, res.SafetyBackupID+ArchiveSuffix))
	if !strings.Contains(safetyText, "Master/slot-3 新的世界") {
		t.Error("恢复前的安全备份里没有 slot-3，退路是假的")
	}
	safety, err := f.mgr.Get(res.SafetyBackupID)
	if err != nil {
		t.Fatal(err)
	}
	if safety.LabelLabel != "恢复前的安全备份" {
		t.Errorf("安全备份的来源标签是 %q", safety.LabelLabel)
	}
	// 4) 空目录也要回来：引擎写模组档时要有目录。
	if d := filepath.Join(f.paths.SaveDir("Cluster_1", dst.ShardMaster), "mod_config_data"); !isDir(d) {
		t.Errorf("恢复后 %s 不是目录", d)
	}
	// 5) 不能留下改名移开的中间态。
	leftovers := findNames(t, f.klei, ".rollback-")
	if len(leftovers) > 0 {
		t.Errorf("恢复成功后还留着移开用的中间目录：%v", leftovers)
	}
	// 6) 世界设定文件跟着回来了。
	if got := f.read(t, f.paths.WorldGenOverride("Cluster_1", dst.ShardMaster)); !strings.Contains(got, "preset") {
		t.Errorf("worldgenoverride.lua 恢复后不对：%q", got)
	}
	// 7) 令牌没被恢复动作写回去过（它本来就不在包里）。
	if !strings.Contains(f.read(t, f.paths.TokenFile("Cluster_1")), "pds-g") {
		t.Error("恢复把令牌文件弄丢了——它本来就不该参与")
	}
}

// TestRestoreKeepsTheSnapshotItIsRestoringFrom 钉的是"保留上限"和"恢复"这两件事的相互关系。
//
// 恢复会先打一份恢复前的安全备份，那一份把总数顶过上限，而满额目录里最旧的那一份
// 经常正是这次要恢复的档。实测踩过：归档在解压到一半时被自己的清理删掉了，
// 整次恢复只能回滚，用户看到的是一次"归档文件不见了"这种说不清原因的失败。
// 这里同时要求清理仍然生效（第二旧的那份照删），别让保护变成一个后门。
func TestRestoreKeepsTheSnapshotItIsRestoringFrom(t *testing.T) {
	f := newFixture(t)
	f.mgr.Keep = 3
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})

	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, mustCreate(t, f, "Cluster_1", LabelManual).ID)
		time.Sleep(1100 * time.Millisecond)
	}
	oldest, mid := ids[0], ids[1]

	masterSession := f.paths.SessionRoot("Cluster_1", dst.ShardMaster)
	f.writeFile(t, filepath.Join(masterSession, "slot-1", "server.dat"), "备份之后又玩了几个档\n")

	res, err := f.mgr.Restore(context.Background(), oldest, Options{})
	if err != nil {
		t.Fatalf("恢复到最旧那一份失败了: %v（结果 %+v）", err, res)
	}
	if _, err := os.Stat(filepath.Join(f.backups, oldest+ArchiveSuffix)); err != nil {
		t.Errorf("正在恢复的那一份被自己的清理删掉了：%v", err)
	}
	// 名额是让给受保护那份，而不是把上限抬高：次旧的那份这一轮就得让位。
	if _, err := os.Stat(filepath.Join(f.backups, mid+ArchiveSuffix)); err == nil {
		t.Errorf("上限 %d 份，恢复完却还留着 %s：清理该往里挪一格而不是破例", f.mgr.Keep, mid)
	}
	if _, err := os.Stat(filepath.Join(f.backups, ids[2]+ArchiveSuffix)); err != nil {
		t.Errorf("最新那份也被牵连了：%v", err)
	}
	if got := f.read(t, filepath.Join(masterSession, "slot-1", "server.dat")); got != "Master/slot-1 世界内容\n" {
		t.Errorf("恢复出来的世界是 %q，想要最旧那份的时刻", got)
	}

	// 保护只对"正在用的那一次"生效，不是给恢复开一个永久名额：
	// 再打一份普通备份，最旧那份这时就该按上限收掉。
	newest := mustCreate(t, f, "Cluster_1", LabelManual).ID
	if _, err := os.Stat(filepath.Join(f.backups, newest+ArchiveSuffix)); err != nil {
		t.Fatalf("新那份没在：%v", err)
	}
	if _, err := os.Stat(filepath.Join(f.backups, oldest+ArchiveSuffix)); err == nil {
		t.Errorf("%s 一直被供着：清理名额不能因为恢复过就永久让位", oldest)
	}
	list, err := f.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	var real int
	for _, s := range list {
		if CheckID(s.ID) == nil {
			real++
		}
	}
	if real > f.mgr.Keep {
		t.Errorf("清理之后还有 %d 份，上限是 %d 份", real, f.mgr.Keep)
	}
}

// TestRestoreIntoOtherClusterMigratesWorld 是"跨房间迁移"：
// 世界和缺的配置进 B 房，B 房已有的端口与服务器名一个字都不动。
func TestRestoreIntoOtherClusterMigratesWorld(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	snap := mustCreate(t, f, "Cluster_1", LabelManual)

	// 目标房间：只有 Master，端口和 A 房不一样。
	f.writeCluster(t, "Cluster_2", []string{"slot-7"}, nil)
	// writeCluster 两个分片都会写；这里把 Caves 整个拆掉，才有"目标缺分片"这回事。
	if err := os.RemoveAll(filepath.Join(f.paths.ClusterDir("Cluster_2"), dst.ShardCaves)); err != nil {
		t.Fatal(err)
	}
	masterPortBefore := f.read(t, f.paths.ShardIni("Cluster_2", dst.ShardMaster))

	res, err := f.mgr.Restore(context.Background(), snap.ID, Options{TargetCluster: "Cluster_2"})
	if err != nil {
		t.Fatalf("迁移失败: %v（%+v）", err, res)
	}
	if !res.Verified {
		t.Errorf("迁移后没做逐个校验：%+v", res)
	}
	// B 房原来的 Master 世界被换成了 A 房的世界。
	got := f.read(t, filepath.Join(f.paths.SessionRoot("Cluster_2", dst.ShardMaster), "slot-1", "server.dat"))
	if got != "Master/slot-1 世界内容\n" {
		t.Errorf("迁移后 Master 的档不对：%q", got)
	}
	if _, err := os.Stat(filepath.Join(f.paths.SessionRoot("Cluster_2", dst.ShardMaster), "slot-7")); err == nil {
		t.Error("B 房自己的 slot-7 还在，整棵替换没做到")
	}
	// B 房已有的 server.ini 必须保持原样。
	if f.read(t, f.paths.ShardIni("Cluster_2", dst.ShardMaster)) != masterPortBefore {
		t.Error("目标房间已有的 server.ini 被覆盖了，端口会被换成备份当时的值")
	}
	if !contains(res.KeptConfig, "Master/server.ini") {
		t.Errorf("结果里没说清哪个配置被保留：%v", res.KeptConfig)
	}
	if !strings.Contains(res.Note, "房间配置保持现状没有覆盖") {
		t.Errorf("说明里没这句：%s", res.Note)
	}
	// B 房没有 Caves，这次新建出来，连缺的配置一起补齐。
	if !contains(res.CreatedShards, "Caves") {
		t.Errorf("没把新建的 Caves 分片说出来：%+v", res)
	}
	if !isDir(f.paths.SaveDir("Cluster_2", dst.ShardCaves)) {
		t.Error("Caves 的存档目录没建出来")
	}
	if _, err := os.Stat(f.paths.ShardIni("Cluster_2", dst.ShardCaves)); err != nil {
		t.Errorf("迁移到缺分片的房间时没补 server.ini：%v", err)
	}
	if !strings.Contains(res.Note, "目标房间原本没有这些世界，已新建：Caves") {
		t.Errorf("说明里没提新建的分片：%s", res.Note)
	}
	// 目标房间的令牌文件必须还是它自己那份：恢复动作碰不到它。
	if got := f.read(t, f.paths.TokenFile("Cluster_2")); !strings.Contains(got, "THIS_TEXT_MUST_NEVER_BE_BACKED_UP") {
		t.Error("目标房间的令牌文件被动过")
	}
}

func TestRestoreRefusesUnrestorableAndBadTarget(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	snap := mustCreate(t, f, "Cluster_1", LabelManual)

	if _, err := f.mgr.Restore(context.Background(), "20261031T021500Z-Cluster_1-manual", Options{}); err == nil {
		t.Error("不存在的快照编号也能恢复")
	}
	if _, err := f.mgr.Restore(context.Background(), snap.ID, Options{TargetCluster: "Cluster_8"}); err == nil {
		t.Error("恢复到不存在的集群也能过")
	} else if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("报错没说不存在：%v", err)
	}
	if _, err := f.mgr.Restore(context.Background(), snap.ID, Options{TargetCluster: "../../etc"}); err == nil {
		t.Error("带路径的集群名被接受了")
	}

	// 房间建好了但引擎从没生成过存档：save 目录是空的。
	// 这种快照里没有世界，界面上不能给一个能点的"恢复"按钮。
	f.writeFile(t, f.mgr.Paths.ClusterIni("Cluster_3"), "cluster\n")
	f.writeFile(t, f.mgr.Paths.ShardIni("Cluster_3", dst.ShardMaster), "[shard]\nid=Master\n")
	if err := os.MkdirAll(f.mgr.Paths.SaveDir("Cluster_3", dst.ShardMaster), 0o755); err != nil {
		t.Fatal(err)
	}
	emptySnap, err := f.mgr.Create(context.Background(), "Cluster_3", LabelManual)
	if err != nil {
		t.Fatalf("空存档目录也该打得开（配置文件还在）: %v", err)
	}
	if len(emptySnap.SaveShards) != 0 {
		t.Errorf("空 save 目录被算成了有世界的分片：%v", emptySnap.SaveShards)
	}
	if emptySnap.Restorable {
		t.Error("没有世界的快照被标成可恢复")
	}
	if !strings.Contains(emptySnap.RestoreNote, "没有任何世界的 save 目录") {
		t.Errorf("没说清为什么不能恢复：%q", emptySnap.RestoreNote)
	}
	if _, err := f.mgr.Restore(context.Background(), emptySnap.ID, Options{}); err == nil {
		t.Error("这种快照也能发起恢复")
	} else if !strings.Contains(err.Error(), "不能用来恢复") {
		t.Errorf("报错没指向真正的原因：%v", err)
	}
}

func TestRestoreRefusesTamperedArchiveWithoutTouchingAnything(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	snap := mustCreate(t, f, "Cluster_1", LabelManual)

	archive := filepath.Join(f.backups, snap.ID+ArchiveSuffix)
	rewriteArchiveEntry(t, archive, snap.Files[0].Path, []byte("tampered-by-hand---"))

	before := snapshotTree(t, f.klei)
	res, err := f.mgr.Restore(context.Background(), snap.ID, Options{})
	if err == nil {
		t.Fatal("被改过的包也能开始恢复")
	}
	if !strings.Contains(err.Error(), "没通过校验") {
		t.Errorf("报错没说清是恢复前就拦下的：%v", err)
	}
	if res.SafetyBackupID != "" {
		t.Errorf("校验没过却已经打了安全备份，说明拦的位置太晚：%+v", res)
	}
	if res.Aftermath != AftermathUntouched {
		t.Errorf("失败发生在动手之前，Aftermath 却是 %q，界面会跟着说错话：%+v", res.Aftermath, res)
	}
	if snapshotTree(t, f.klei) != before {
		t.Error("被拒绝的恢复动了磁盘上的存档")
	}
}

// TestStageRollbackPutsEverythingBack 盯的是那段"旧存档先移开、校验通过才删"的中间态：
// 解压失败时必须原样换回去，包括比快照新的那些 slot。
func TestStageRollbackPutsEverythingBack(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, []string{"slot-1"})
	snap := mustCreate(t, f, "Cluster_1", LabelManual)
	masterSession := f.paths.SessionRoot("Cluster_1", dst.ShardMaster)
	f.writeFile(t, filepath.Join(masterSession, "slot-3", "server.dat"), "slot-3 是备份之后的世界\n")

	before := snapshotTree(t, f.klei)
	p, staged, err := f.mgr.planAndStage(f.paths.ClusterDir("Cluster_1"), snap, "111")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.paths.SaveDir("Cluster_1", dst.ShardMaster))); err == nil {
		t.Error("移开之后 Master/save 还在原地")
	}
	if len(staged) == 0 {
		t.Fatal("没有移开任何东西，这个测试没测到中间态")
	}
	if err := p.rollback(staged); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	after := snapshotTree(t, f.klei)
	if after != before {
		t.Errorf("回滚后磁盘和回滚前不一样：\n--- 想要 ---\n%s\n--- 实际 ---\n%s", before, after)
	}
	// 回滚必须把比快照新的 slot 也保住（那是用户现在的.world）。
	if got := f.read(t, filepath.Join(masterSession, "slot-3", "server.dat")); !strings.Contains(got, "备份之后的世界") {
		t.Errorf("回滚把现在的世界弄丢了：%q", got)
	}
	if leftovers := findNames(t, f.klei, ".rollback-"); len(leftovers) > 0 {
		t.Errorf("回滚后还留着中间目录：%v", leftovers)
	}
}

func TestCheckIDRejectsPathishIDs(t *testing.T) {
	for _, bad := range []string{"", "..", "../Cluster_1", "20261031T021500Z-Cluster_1",
		"20261031T021500Z-Cluster_1-manual/../Master", "id with space",
		"20261031T02150Z-Cluster_1-manual", "20261031T021500Z-Cluster_1-manual/../x"} {
		if CheckID(bad) == nil {
			t.Errorf("编号 %q 被当成合法的", bad)
		}
	}
	for _, ok := range []string{"20261031T021500Z-Cluster_1-manual",
		"20261031T021500Z-Cluster_12-before-restore", "20261031T021500Z-Cluster_1-scheduled-2"} {
		if err := CheckID(ok); err != nil {
			t.Errorf("合法编号 %q 被拒了：%v", ok, err)
		}
	}
}

func TestSkipRelRules(t *testing.T) {
	for rel, want := range map[string]string{
		"cluster_token.txt":                  "Klei 令牌不打进备份",
		"Master/cluster_token.txt":           "Klei 令牌不打进备份",
		"Master/save/dstpanel_queue.txt":     "面板与助手 MOD 的命令通道文件不打包",
		"Master/save/dstpanel_queue.txt.bak": "面板与助手 MOD 的命令通道文件不打包",
		"Caves/save/dstpanel_status.json":    "面板与助手 MOD 的命令通道文件不打包",
		"Master/server_log.txt":              "引擎日志不打包",
		"Caves/server_chat_log.txt":          "引擎日志不打包",
	} {
		if got := skipRel(rel); !strings.Contains(got, want) {
			t.Errorf("%q 的跳过原因是 %q，想要含 %q", rel, got, want)
		}
	}
	for _, rel := range []string{"Master/save/session/ABC/slot-1/server.dat",
		"Master/worldgenoverride.lua", "cluster.ini", "Master/save/mod_config_data"} {
		if got := skipRel(rel); got != "" {
			t.Errorf("%q 不该被跳过：%s", rel, got)
		}
	}
}

func TestCollectReportsShardWithoutSave(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, nil)
	// Caves 目录在（writeCluster 写过 server.ini），但没有 save 目录。
	cavesSave := f.paths.SaveDir("Cluster_1", dst.ShardCaves)
	if err := os.RemoveAll(cavesSave); err != nil {
		t.Fatal(err)
	}
	items, skipped, err := f.mgr.Collect("Cluster_1")
	if err != nil {
		t.Fatal(err)
	}
	var hasMasterSave, hasCavesSave bool
	for _, it := range items {
		if strings.HasPrefix(it.rel, "Master/save/") || it.rel == "Master/save/" {
			hasMasterSave = true
		}
		if strings.HasPrefix(it.rel, "Caves/save/") {
			hasCavesSave = true
		}
	}
	if !hasMasterSave || hasCavesSave {
		t.Errorf("没存档的分片被当成有存档了：Master=%v Caves=%v", hasMasterSave, hasCavesSave)
	}
	if len(skipped) == 0 {
		t.Error("令牌和通道文件一个都没跳，跳过名单是空的")
	}
}

// ---- 辅助 ----

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// findNames 列出目录树里名字含片段的文件/目录名（用来抓 .rollback- 残留）。
func findNames(t *testing.T, root, piece string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(d.Name(), piece) {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// snapshotTree 把目录树变成可比较的字符串：路径 + 内容。
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var buf strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			fmt.Fprintf(&buf, "D %s\n", rel)
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&buf, "F %s %s\n", rel, strings.TrimRight(string(raw), "\n"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// 云机实测出来的存档形状和界面样本不一样：这台引擎把自动存档写成
// save/session/<GAMEID>/<8 位编号> 再加一份 .meta，没有 slot-N 目录；
// 而分片会把上一轮的日志挪进 <分片>/backup/server_log/ 存着——那 22 个文件
// 正是被黑名单漏掉、打进快照的那一批。用真形状再跑一遍，才能证明打包认的是
// "save 目录里的都是世界状态"这条规则，而不是认我自己编的目录名。
func TestCreatePacksRealCloudSaveShapeAndSkipsRotatedLogs(t *testing.T) {
	f := newFixture(t)
	cluster := "Cluster_1"
	f.writeFile(t, f.paths.ClusterIni(cluster), "cluster\n")
	f.writeFile(t, f.paths.TokenFile(cluster), "pds-g^KU_fakeid001^THIS_TEXT_MUST_NEVER_BE_BACKED_UP\n")
	f.writeFile(t, f.paths.ShardIni(cluster, dst.ShardMaster), "[shard]\nid=Master\n")
	sess := filepath.Join(f.paths.SessionRoot(cluster, dst.ShardMaster), "DCF8E1BCE43CE58A")
	newest := "local savedata = {} -- 0000000085\n"
	for _, n := range []string{"0000000084", "0000000085"} {
		body := "local savedata = {} -- " + n + "\n"
		if n == "0000000085" {
			body = newest
		}
		f.writeFile(t, filepath.Join(sess, n), body)
		f.writeFile(t, filepath.Join(sess, n+".meta"), "return {clock={cycles=74}}\n")
	}
	rotLog := filepath.Join(f.paths.ShardDir(cluster, dst.ShardMaster),
		"backup", "server_log", "server_log_2026-10-31-02-18-18.txt")
	rotChat := filepath.Join(f.paths.ShardDir(cluster, dst.ShardMaster),
		"backup", "server_chat_log", "server_chat_log_2026-10-31-02-18-19.txt")
	f.writeFile(t, rotLog, "[00:00:00]: old boot log\n")
	f.writeFile(t, rotChat, "[00:00:00]: [Announcement] 旧公告\n")

	snap := mustCreate(t, f, cluster, LabelManual)

	var paths []string
	for _, e := range snap.Files {
		paths = append(paths, e.Path)
		if strings.Contains(e.Path, "/server_log/") || strings.Contains(e.Path, "/server_chat_log/") {
			t.Fatalf("引擎攒的旧日志副本被打进了快照：%s", e.Path)
		}
	}
	for _, want := range []string{
		"Master/save/session/DCF8E1BCE43CE58A/0000000085",
		"Master/save/session/DCF8E1BCE43CE58A/0000000085.meta",
	} {
		if !contains(paths, want) {
			t.Errorf("快照里没有 %s，实际登记：%v", want, paths)
		}
	}
	if strings.Join(snap.SaveShards, ",") != "Master" {
		t.Errorf("带存档的分片是 %v，想要 Master（编号文件那种形状也要认得出是存档）", snap.SaveShards)
	}
	if !snap.Restorable {
		t.Errorf("这份应当可以恢复，实际原因：%s", snap.RestoreNote)
	}
	// 滚日志那一层要连目录一起挡在包外：目录进包的话恢复出来会多出四个空的日志目录，
	// 而清单上还写着"跳过了引擎日志"，两边对不上。子树整棵跳过，所以名单里记的是目录。
	joined := strings.Join(snap.Skipped, "\n")
	for _, want := range []string{"Master/backup/server_log", "Master/backup/server_chat_log"} {
		if !strings.Contains(joined, want) {
			t.Errorf("跳过名单里没有 %s 这一层，实际：%s", want, joined)
		}
	}

	// 恢复回去：编号文件必须原样落回磁盘，而 restore 不该顺手清理 save 之外的东西。
	if err := os.RemoveAll(f.paths.SaveDir(cluster, dst.ShardMaster)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Restore(context.Background(), snap.ID, Options{}); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if got := f.read(t, filepath.Join(sess, "0000000085")); got != newest {
		t.Errorf("恢复回来的档位内容不对：%q", got)
	}
	if _, err := os.Stat(rotLog); err != nil {
		t.Errorf("恢复把 save 之外的引擎日志副本也清掉了：%v", err)
	}
}

// TestCreateToleratesOldestSaveRotatingOutMidPack 钉住云机上真发生过的一次备份失败：
// 打包打到一半，引擎把编号最旧的自动存档（save/session/DCF8E1BCE43CE58A/0000000068）
// 从磁盘上删掉了。那时打包是"先 os.Stat 再 os.Open"两步问，Stat 那一下文件还在、
// Open 时已经没了，于是整个备份任务失败，而报错是 Go 的英文原文
// （stat ...: no such file or directory）——那句话会原样出现在用户看的任务消息里。
// 注入照抄现场时序：轮到那个文件时先真的把它从磁盘删掉，再报 ErrNotExist。
func TestCreateToleratesOldestSaveRotatingOutMidPack(t *testing.T) {
	f := newFixture(t)
	cluster := "Cluster_1"
	f.writeFile(t, f.paths.ClusterIni(cluster), "cluster\n")
	f.writeFile(t, f.paths.ShardIni(cluster, dst.ShardMaster), "[shard]\nid=Master\n")
	f.writeFile(t, f.paths.TokenFile(cluster), "pds-g^KU_fakeid001^THIS_TEXT_MUST_NEVER_BE_BACKED_UP\n")
	sess := filepath.Join(f.paths.SessionRoot(cluster, dst.ShardMaster), "DCF8E1BCE43CE58A")
	oldest := filepath.Join(sess, "0000000084")
	newest := filepath.Join(sess, "0000000085")
	newestBody := "local savedata = {} -- 0000000085\n"
	for _, n := range []string{"0000000084", "0000000085"} {
		f.writeFile(t, filepath.Join(sess, n), "local savedata = {} -- "+n+"\n")
		f.writeFile(t, filepath.Join(sess, n+".meta"), "return {clock={cycles=74}}\n")
	}
	f.mgr.openFileFn = func(path string) (*os.File, error) {
		if path != oldest {
			return os.Open(path)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("模拟淘汰 %s 时出错: %v", path, err)
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}

	snap := mustCreate(t, f, cluster, LabelManual)

	var paths []string
	for _, e := range snap.Files {
		paths = append(paths, e.Path)
	}
	if contains(paths, "Master/save/session/DCF8E1BCE43CE58A/0000000084") {
		t.Errorf("已经没了的档位还登记在清单里，校验时必然报缺文件：%v", paths)
	}
	if !contains(paths, "Master/save/session/DCF8E1BCE43CE58A/0000000085") {
		t.Fatalf("最近那份存档没进快照，这份备份没有意义：%v", paths)
	}
	if snap.FileCount != len(snap.Files) {
		t.Errorf("清单登记的份数和列出来的对不上：%d vs %d", snap.FileCount, len(snap.Files))
	}
	// 少了一个正在滚动的旧档不算失败，但必须交代：用户对比"房间里有两份档、快照里只有一份"
	// 时，答案要写在快照自己身上，而不是让人以为面板漏打包了。
	if !strings.Contains(snap.Note, "淘汰") {
		t.Errorf("清单说明里没交代有条目被引擎淘汰：%q", snap.Note)
	}
	if !snap.Restorable {
		t.Errorf("滚掉一个旧档之后快照仍然应该可恢复，实际原因：%s", snap.RestoreNote)
	}

	text := archiveText(t, filepath.Join(f.backups, snap.ID+ArchiveSuffix))
	if strings.Contains(text, "=== Master/save/session/DCF8E1BCE43CE58A/0000000084 ===") {
		t.Error("包里有那个条目而清单里却没有，校验会报出包里有清单没登记")
	}
	if !strings.Contains(text, "=== Master/save/session/DCF8E1BCE43CE58A/0000000085 ===") {
		t.Error("包里少了最近那份存档")
	}

	vr, err := f.mgr.Verify(context.Background(), snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !vr.ArchiveOK || vr.Checked != snap.FileCount {
		t.Errorf("滚掉一个旧档的快照校验不过：%+v", vr)
	}

	// 恢复：整棵存档删掉再回来。最近那份要原样落回，被滚掉的那份不能凭空造出来。
	if err := os.RemoveAll(f.paths.SaveDir(cluster, dst.ShardMaster)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.Restore(context.Background(), snap.ID, Options{}); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if got := f.read(t, newest); got != newestBody {
		t.Errorf("恢复回来的最近档位不对：%q", got)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("被引擎淘汰掉的那一档被恢复成了凭空多出来的文件（err=%v）", err)
	}
}
