package backup

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/dst"
)

// import_test.go 盯的是"导入会不会把一个不能用的包说成能用"。
//
// 最要紧的两条：
//   - 没有 shardindex（或索引里的世界编号和包里的 session 目录对不上）必须拦住。
//     放行的后果不是报错，而是引擎静默生成一个新世界——用户以为档没了。
//   - 导入产出的就是普通快照，所以它必须能直接走已经测过的恢复那条路。

const gameID = "DCF8E1BCE43CE58A"

// shardIndex 是引擎写出的那份索引（带 SetPersistentString 的头，面板必须读得动）。
func shardIndex(id string) string {
	return "KLEI     1 \nreturn {\n    session_id = \"" + id + "\",\n    name = \"Master\",\n" +
		"    root = \"Master\",\n    enabled = true,\n    world_id = \"default\",\n}\n"
}

// saveTree 是一份最小可用的真实存档：地表世界带索引和两份滚动档位，
// 外加房间配置、令牌、引擎日志和面板通道文件——后三类必须被挡在快照外。
func saveTree(root string) map[string]string {
	pre := ""
	if root != "" {
		pre = root + "/"
	}
	return map[string]string{
		pre + "cluster.ini":                                        "cluster\n",
		pre + "clusteroptions.ini":                                 "options\n",
		pre + "cluster_token.txt":                                  "pds-g^KU_fakeid001^THIS_TEXT_MUST_NEVER_BE_BACKED_UP\n",
		pre + "Master/save/shardindex":                             shardIndex(gameID),
		pre + "Master/save/session/" + gameID + "/0000000084":      "local savedata = {} -- 0000000084\n",
		pre + "Master/save/session/" + gameID + "/0000000084.meta": "return {clock={cycles=70}}\n",
		pre + "Master/save/session/" + gameID + "/0000000085":      "local savedata = {} -- 0000000085\n",
		pre + "Master/save/session/" + gameID + "/0000000085.meta": "return {clock={cycles=74}}\n",
		pre + "Master/server.ini":                                  "[shard]\nid=Master\n",
		pre + "Master/leveldataoverride.lua":                       "return {override=true}\n",
		pre + "Master/modoverrides.lua":                            "return {[\"workshop-1\"]={enabled=true}}\n",
		pre + "Master/save/dstpanel_queue.txt":                     "cmd-1|kick|阿黎|恶意\n",
		pre + "Master/server_log.txt":                              "[00:00:01]: old boot log line\n",
		pre + "Caves/save/shardindex":                              strings.Replace(shardIndex(gameID), "Master", "Caves", -1),
		pre + "Caves/save/session/" + gameID + "/0000000001":       "cave world\n",
		pre + "Caves/server.ini":                                   "[shard]\nid=Caves\n",
	}
}

// packDir 按 kinds 里的形状把一份名字→内容的表压成一个包。
// withDirs 为真时连目录条目一起写（大多数压缩工具会写）；
// 两种都要测，因为"没有目录条目"的包照样是合法包，面板不能只认一种。
func packDir(t *testing.T, path string, files map[string]string, kind string, withDirs bool, extra func(*tar.Writer, *zip.Writer)) {
	t.Helper()
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	switch kind {
	case "zip":
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		zw := zip.NewWriter(f)
		var dirs map[string]bool
		if withDirs {
			dirs = dirSet(names)
		}
		for dir := range dirs {
			w, err := zw.Create(dir + "/")
			if err != nil {
				t.Fatal(err)
			}
			_ = w
		}
		for _, name := range names {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(files[name])); err != nil {
				t.Fatal(err)
			}
		}
		if extra != nil {
			extra(nil, zw)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	case "tar.gz", "tar":
		var buf bytes.Buffer
		var gw *gzip.Writer
		wr := io.Writer(&buf)
		if kind == "tar.gz" {
			gw = gzip.NewWriter(&buf)
			wr = gw
		}
		tw := tar.NewWriter(wr)
		dirs := map[string]bool{}
		if withDirs {
			dirs = dirSet(names)
		}
		var order []string
		for d := range dirs {
			order = append(order, d)
		}
		sort.Strings(order)
		for _, d := range order {
			if err := tw.WriteHeader(&tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatGNU}); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range names {
			body := files[name]
			if err := tw.WriteHeader(&tar.Header{
				Name: name, Typeflag: tar.TypeReg, Mode: 0o644,
				Size: int64(len(body)), Format: tar.FormatGNU,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
		if extra != nil {
			extra(tw, nil)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if gw != nil {
			if err := gw.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("测试没准备 %s 这种包", kind)
	}
}

func dirSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			out[strings.Join(parts[:i], "/")] = true
		}
	}
	return out
}

func stagedZip(t *testing.T, f *fixture, files map[string]string, kind string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "import-fixture."+kind)
	packDir(t, path, files, kind, true, nil)
	return path
}

// ---- 预览 ----

func TestInspectImportRecognizesRealSaveTree(t *testing.T) {
	f := newFixture(t)
	for _, kind := range []string{"zip", "tar.gz", "tar"} {
		t.Run(kind, func(t *testing.T) {
			pkg := stagedZip(t, f, saveTree("Cluster_9"), kind)
			got, err := f.mgr.InspectImport(pkg)
			if err != nil {
				t.Fatalf("预览失败: %v", err)
			}
			if !got.CanImport {
				t.Fatalf("这份包该能导入，实际结论：%v（%s）", got.Problems, got.Note)
			}
			if got.Kind != kind {
				t.Errorf("认出的格式是 %q，想要 %q", got.Kind, kind)
			}
			if got.Shape != "cluster" || got.Root != "Cluster_9" {
				t.Errorf("形状判成了 %q / root %q，想要 cluster / Cluster_9", got.Shape, got.Root)
			}
			if len(got.Shards) != 2 {
				t.Fatalf("认出的世界是 %v，想要 Master 和 Caves 两个", got.Shards)
			}
			if got.Shards[0].Shard != "Master" || got.Shards[0].SessionID != gameID {
				t.Errorf("地表世界的编号没读出来：%+v", got.Shards[0])
			}
			if got.Shards[0].Slots != 0 {
				t.Errorf("这份是真形状（8 位编号平铺），不该认出 slot 目录：%+v", got.Shards[0])
			}
			if !strings.Contains(strings.Join(got.Shards[0].WorldFiles, ","), "0000000085") {
				t.Errorf("档位文件名没列出来：%v", got.Shards[0].WorldFiles)
			}
			if !got.Shards[0].HasModPrefs {
				t.Errorf("包里有 modoverrides.lua 却没说")
			}
			if got.ArchiveBytes <= 0 || got.UncompressedBytes <= 0 || got.FileCount == 0 {
				t.Errorf("大小和文件数没算出来：%+v", got)
			}
			// 令牌、通道文件、引擎日志必须出现在"刻意没取"的名单里，而不是静默消失。
			joined := strings.Join(got.Dropped, "\n")
			for _, want := range []string{"cluster_token.txt", "dstpanel_queue.txt", "server_log.txt"} {
				if !strings.Contains(joined, want) {
					t.Errorf("丢掉名单里少了 %s，实际：%s", want, joined)
				}
			}
			if !strings.Contains(joined, "Klei 令牌不打进备份") {
				t.Errorf("令牌被丢掉但没写原因：%s", joined)
			}
			if !strings.Contains(got.Note, "外部导入") && !strings.Contains(got.Note, "认出来了") {
				t.Errorf("预览的结论句没交代清楚：%s", got.Note)
			}
			// 模组配置和"配置不覆盖"这两件事必须在提交之前就看到。
			warn := strings.Join(got.Warnings, "\n")
			if !strings.Contains(warn, "modoverrides.lua") {
				t.Errorf("没提醒模组名单会跟着这份包走：%s", warn)
			}
			if !strings.Contains(warn, "不会被覆盖") {
				t.Errorf("没说清房间配置的处置：%s", warn)
			}
		})
	}
}

func TestInspectImportWithoutDirEntriesStillWorks(t *testing.T) {
	f := newFixture(t)
	pkg := filepath.Join(t.TempDir(), "no-dirs.zip")
	packDir(t, pkg, saveTree("Cluster_9"), "zip", false, nil)
	got, err := f.mgr.InspectImport(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CanImport {
		t.Errorf("没有目录条目的包被拒了：%v", got.Problems)
	}
	if got.Shards[0].SessionID != gameID {
		t.Errorf("世界编号读不出来：%+v", got.Shards[0])
	}
}

func TestInspectImportRejectsMissingShardIndex(t *testing.T) {
	f := newFixture(t)
	files := saveTree("Cluster_9")
	delete(files, "Cluster_9/Master/save/shardindex")
	delete(files, "Cluster_9/Caves/save/shardindex")
	pkg := stagedZip(t, f, files, "zip")

	got, err := f.mgr.InspectImport(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanImport {
		t.Fatal("没有 shardindex 的包说能导入——导进去引擎会生成一个新世界，一句错都不报")
	}
	joined := strings.Join(got.Problems, "\n") + "\n" + strings.Join(got.Warnings, "\n")
	if !strings.Contains(joined, "shardindex") {
		t.Errorf("没指出缺的是 shardindex：%s", joined)
	}
	// 拦住只是及格，那句话必须能照着做。
	if !strings.Contains(joined, "连 save 目录一起打包") {
		t.Errorf("没给出修这个包的做法：%s", joined)
	}
}

func TestInspectImportRejectsIndexPointingAtMissingSession(t *testing.T) {
	f := newFixture(t)
	files := saveTree("Cluster_9")
	files["Cluster_9/Master/save/shardindex"] = shardIndex("AABBCCDDEEFF0011")
	pkg := stagedZip(t, f, files, "zip")

	got, err := f.mgr.InspectImport(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanImport {
		t.Fatal("索引指向一个包里没有的世界编号，却说能导入")
	}
	if !strings.Contains(strings.Join(got.Problems, "；"), gameID) ||
		!strings.Contains(strings.Join(got.Problems, "；"), "AABBCCDDEEFF0011") {
		t.Errorf("报错里没同时给出索引写的编号和包里实际缺的那个：%v", got.Problems)
	}
}

func TestInspectImportRejectsLegacySaveIndexOnly(t *testing.T) {
	f := newFixture(t)
	files := saveTree("Cluster_9")
	delete(files, "Cluster_9/Master/save/shardindex")
	files["Cluster_9/Master/save/saveindex"] = "return {session_id=\"" + gameID + "\", shards={}}\n"
	pkg := stagedZip(t, f, files, "zip")

	got, err := f.mgr.InspectImport(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanImport {
		t.Fatal("只有客户端旧版 saveindex 的包被当成能导入")
	}
	if !strings.Contains(strings.Join(got.Problems, "；"), "saveindex") {
		t.Errorf("没指出那是旧版索引：%v", got.Problems)
	}
}

func TestInspectImportEmptySessionID(t *testing.T) {
	f := newFixture(t)
	files := saveTree("Cluster_9")
	files["Cluster_9/Master/save/shardindex"] = "return {\n    session_id = \"\",\n    name = \"Master\",\n}\n"
	pkg := stagedZip(t, f, files, "zip")
	got, err := f.mgr.InspectImport(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanImport {
		t.Fatal("索引里 session_id 是空的也能导（这份档还没真存过一次世界）")
	}
	if !strings.Contains(strings.Join(got.Problems, "；"), "session_id 是空的") {
		t.Errorf("报错没落在空编号上：%v", got.Problems)
	}
}

func TestInspectImportNormalizesLowercaseShardNames(t *testing.T) {
	f := newFixture(t)
	files := map[string]string{
		"cluster_1/master/save/shardindex":                        shardIndex(gameID),
		"cluster_1/master/save/session/" + gameID + "/0000000001": "地表\n",
		"cluster_1/caves/save/shardindex":                         strings.Replace(shardIndex(gameID), "Master", "Caves", -1),
		"cluster_1/caves/save/session/" + gameID + "/0000000002":  "洞穴\n",
		"cluster_1/CLUSTER.INI":                                   "cluster\n",
	}
	got, err := f.mgr.InspectImport(stagedZip(t, f, files, "zip"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.CanImport {
		t.Fatalf("Windows 拖出来的小写目录名被拒了：%v", got.Problems)
	}
	if got.Shards[0].Shard != "Master" {
		t.Errorf("世界名没归一成引擎认的写法：%+v", got.Shards)
	}
	warn := strings.Join(got.Warnings, "\n")
	if !strings.Contains(warn, "引擎只认 Master") {
		t.Errorf("归一过小写却没说：%s", warn)
	}
}

func TestInspectImportRefusesPathEscapeAndForeignEntries(t *testing.T) {
	f := newFixture(t)
	files := saveTree("Cluster_9")
	files["../../etc/passwd"] = "root:x:0:0\n"
	files["Cluster_9/Master/save/../../../Windows/system32/x.dll"] = "dll\n"
	files["/absolute/path.lua"] = "return {}\n"
	files["__MACOSX/._Cluster_9"] = "资源分叉\n"
	files["Cluster_9/README.txt"] = "我自己塞的说明\n"
	got, err := f.mgr.InspectImport(stagedZip(t, f, files, "zip"))
	if err != nil {
		t.Fatal(err)
	}
	dropped := strings.Join(got.Dropped, "\n")
	for _, want := range []string{"__MACOSX", "README.txt"} {
		if !strings.Contains(dropped, want) {
			t.Errorf("没交代 %s 为什么没取：%s", want, dropped)
		}
	}
	if !strings.Contains(strings.Join(got.Warnings, "\n"), "路径不正常") {
		t.Errorf("脏路径没被说出来：%v", got.Warnings)
	}
	// 最硬的一条：结论里任何一处都不许出现被写进快照的越界路径。
	for _, s := range got.Shards {
		for _, name := range s.WorldFiles {
			if strings.Contains(name, "..") {
				t.Errorf("档位名单里有越界路径：%s", name)
			}
		}
	}
	// 还能导：脏条目只丢掉，不牵连正常的那部分——但结论必须把丢的东西说全。
	if !got.CanImport {
		t.Errorf("脏路径不该让整个包不能导：%v", got.Problems)
	}
}

func TestInspectImportRejectsTwoClusterRoots(t *testing.T) {
	f := newFixture(t)
	files := map[string]string{}
	for name, body := range saveTree("Cluster_9") {
		files[name] = body
	}
	for name, body := range saveTree("Cluster_8") {
		files[name] = body
	}
	got, err := f.mgr.InspectImport(stagedZip(t, f, files, "zip"))
	if err != nil {
		t.Fatal(err)
	}
	if got.CanImport {
		t.Fatal("两个房间混在一个包里也说能导入")
	}
	joined := strings.Join(got.Problems, "；")
	if !strings.Contains(joined, "Cluster_8") || !strings.Contains(joined, "Cluster_9") {
		t.Errorf("没把两个房间名都点出来：%s", joined)
	}
}

func TestInspectImportDiagnosesWrongDepthPackage(t *testing.T) {
	f := newFixture(t)
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"只压了 save 里面": {
			files: map[string]string{
				"session/" + gameID + "/0000000085": "地表\n",
				"shardindex":                        shardIndex(gameID),
			},
			want: "请连 Master（或 Caves）目录本身一起压",
		},
		"压了世界目录但没有 save": {
			files: map[string]string{
				"Cluster_9/Master/leveldataoverride.lua": "return {override=true}\n",
				"Cluster_9/Master/modoverrides.lua":      "return {}\n",
			},
			want: "没有任何存档可恢复",
		},
		"根本不是饥荒的包": {
			files: map[string]string{
				"photos/2026/cat.jpg": "binary\n",
				"notes/todo.txt":      "买东西\n",
				"notes/sub/deep.txt":  "x\n",
			},
			want: "认不出这是饥荒的存档包",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := f.mgr.InspectImport(stagedZip(t, f, tc.files, "zip"))
			if err != nil {
				t.Fatal(err)
			}
			if got.CanImport {
				t.Fatalf("这种包不该能导：%+v", got)
			}
			joined := strings.Join(got.Problems, "；") + "；" + got.Note
			if !strings.Contains(joined, tc.want) {
				t.Errorf("结论里没有那句能照着做的：%s\n想要含 %q", joined, tc.want)
			}
		})
	}
}

func TestInspectImportRejectsUnknownFormat(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("这不是一个压缩包\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.InspectImport(path); err == nil {
		t.Fatal("一个文本文件也被当成存档包")
	} else if !strings.Contains(err.Error(), "认不出") {
		t.Errorf("报错没说清是格式问题：%v", err)
	}
}

func TestInspectImportRejectsSymlinkEntries(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, body := range saveTree("Cluster_9") {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatGNU,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	// 照着用户主目录写的软链：解开它等于往房间外面写。
	if err := tw.WriteHeader(&tar.Header{
		Name: "Cluster_9/Master/save/escape", Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd", Mode: 0o777, Format: tar.FormatGNU,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "with-link.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := f.mgr.InspectImport(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanImport {
		t.Fatal("带软链的包被放行")
	}
	if !strings.Contains(strings.Join(got.Problems, "；"), "不是普通文件或目录的条目") {
		t.Errorf("没指出是软链这类条目：%v", got.Problems)
	}
}

// ---- 导入产出快照，然后走恢复 ----

func TestImportProducesSnapshotAndRestoresLikeAPanelBackup(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-old"}, nil)
	pkg := stagedZip(t, f, saveTree("Cluster_9"), "zip")

	var unpacked, packed []string
	snap, err := f.mgr.Import(context.Background(), pkg, ImportOptions{
		Cluster:  "Cluster_1",
		OnUnpack: func(done, total int) { unpacked = append(unpacked, doneOf(done, total)) },
		OnPack:   func(done, total int) { packed = append(packed, doneOf(done, total)) },
	})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if snap.Label != LabelImported || snap.LabelLabel != "外部导入的存档" {
		t.Errorf("来源记成了 %q / %q", snap.Label, snap.LabelLabel)
	}
	if snap.Cluster != "Cluster_1" {
		t.Errorf("这份快照登记给 %s，想要 Cluster_1", snap.Cluster)
	}
	if !snap.Restorable {
		t.Fatalf("导入的快照不能恢复：%s", snap.RestoreNote)
	}
	if strings.Join(snap.SaveShards, ",") != "Caves,Master" {
		t.Errorf("带存档的世界是 %v", snap.SaveShards)
	}
	if len(unpacked) == 0 || len(packed) == 0 {
		t.Fatalf("两段进度都没报：解包 %d 步、打包 %d 步", len(unpacked), len(packed))
	}
	// 进度必须收在"做完 = 总数"上：界面拿最后一步算出来的百分比要正好是 100%，
	// 停在 86/87 的话任务显示"完成"而进度条差一格，用户会以为卡住了。
	for _, step := range [][]string{unpacked, packed} {
		last := step[len(step)-1]
		done, total, ok := strings.Cut(last, "/")
		if !ok {
			t.Fatalf("进度格式不对：%q", last)
		}
		if done != total {
			t.Errorf("最后一步进度是 %s，没收在总数上", last)
		}
		n, err := strconv.Atoi(total)
		if err != nil || n <= 0 {
			t.Errorf("进度里的总数没法用：%q", last)
		}
	}
	// 来历要写在清单里，日后才知道这份不是本机打的。
	if !strings.Contains(snap.Note, "外部导入的存档") || !strings.Contains(snap.Note, gameID) {
		t.Errorf("清单说明里没交代来历和世界编号：%q", snap.Note)
	}
	// 令牌和通道文件不能因为"来自外部包"就绕开黑名单。
	blob := archiveText(t, filepath.Join(f.backups, snap.ID+ArchiveSuffix))
	for _, banned := range []string{"THIS_TEXT_MUST_NEVER_BE_BACKED_UP", "pds-g", "cmd-1|kick", "old boot log line"} {
		if strings.Contains(blob, banned) {
			t.Fatalf("导入的快照里带着 %q", banned)
		}
	}
	// 临时解压树用完就没了，备份目录里只剩归档和清单。
	if leftovers := findNames(t, f.backups, unpackDirName); len(leftovers) > 0 {
		t.Errorf("导入完还留着解压目录：%v", leftovers)
	}

	// 恢复走的是已经测透的那条路：整棵换掉 save，且不动现有配置。
	if _, err := f.mgr.Restore(context.Background(), snap.ID, Options{}); err != nil {
		t.Fatalf("恢复到本机房间失败: %v", err)
	}
	got := f.read(t, filepath.Join(f.paths.SessionRoot("Cluster_1", dst.ShardMaster), gameID, "0000000085"))
	if got != "local savedata = {} -- 0000000085\n" {
		t.Errorf("恢复出来的档位内容不对：%q", got)
	}
	if _, err := os.Stat(filepath.Join(f.paths.SessionRoot("Cluster_1", dst.ShardMaster), "slot-old")); err == nil {
		t.Error("导入前的世界还在：save 没被整棵换掉，引擎会读回旧档")
	}
	if !isDir(f.paths.SaveDir("Cluster_1", dst.ShardCaves)) {
		t.Error("包里带来的 Caves 世界没被建出来")
	}
	if f.read(t, f.paths.TokenFile("Cluster_1")) == "" {
		t.Error("本机房间的令牌被动过")
	}
	// 本机已有的 server.ini 必须保持原样（writeCluster 写过 [shard] id=Master）。
	if got := f.read(t, f.paths.ShardIni("Cluster_1", dst.ShardMaster)); !strings.Contains(got, "id=Master") {
		t.Errorf("房间配置被包里的值覆盖了：%q", got)
	}
}

func TestImportRefusesBadPackageAndBadCluster(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, nil)

	files := saveTree("Cluster_9")
	delete(files, "Cluster_9/Master/save/shardindex")
	if _, err := f.mgr.Import(context.Background(), stagedZip(t, f, files, "zip"), ImportOptions{Cluster: "Cluster_1"}); err == nil {
		t.Fatal("不能导入的包也做成了快照")
	} else if !strings.Contains(err.Error(), "不能导入") {
		t.Errorf("报错没指回预览时说过的原因：%v", err)
	}

	for _, bad := range []string{"", "..", "../Cluster_1", "a/b"} {
		if _, err := f.mgr.Import(context.Background(), stagedZip(t, f, saveTree("Cluster_9"), "zip"),
			ImportOptions{Cluster: bad}); err == nil {
			t.Errorf("房间名 %q 被当成了合法名字", bad)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.mgr.Import(ctx, stagedZip(t, f, saveTree("Cluster_9"), "zip"), ImportOptions{Cluster: "Cluster_1"}); err == nil {
		t.Fatal("ctx 已经结束还能导入")
	}
	// 失败的现场必须干净：不许留下半截快照或解压目录。
	for _, e := range listAll(t, f.backups) {
		if strings.Contains(e, ".tmp") || strings.Contains(e, unpackDirName) || strings.Contains(e, ManifestSuffix) {
			t.Fatalf("导入失败后背目录留了残留：%s", e)
		}
	}
}

func listAll(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func doneOf(done, total int) string {
	return strconv.Itoa(done) + "/" + strconv.Itoa(total)
}

// ---- 上传临时包 ----

func TestWriteStagedNamesAndDetectsFormat(t *testing.T) {
	f := newFixture(t)
	pkg := stagedZip(t, f, saveTree("Cluster_9"), "zip")
	raw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}

	ref, n, err := f.mgr.WriteStaged("我的存档.zip", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("落地失败: %v", err)
	}
	if int(n) != len(raw) {
		t.Errorf("落地 %d 字节，上传了 %d 字节", n, len(raw))
	}
	if err := checkStagedRef(ref); err != nil {
		t.Fatalf("编号不合格式：%q（%v）", ref, err)
	}
	if !strings.HasPrefix(ref, "import-") || !strings.HasSuffix(ref, ".zip") {
		t.Errorf("编号 %q 认不出是哪个包", ref)
	}
	path, err := f.mgr.StagedPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(f.backups, incomingDirName) {
		t.Errorf("上传包落在了 %s，应该在备份目录里的 incoming 下", filepath.Dir(path))
	}
	// 预览接口拿这个编号就能读回同一份结论。
	got, err := f.mgr.InspectImport(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CanImport || got.Package != ref {
		t.Errorf("预览对不上：%+v", got)
	}

	// 后缀按包的实际字节决定，不信客户端写的名字：一个 .txt 的 tar.gz 照样认出 tar.gz。
	ref2, _, err := f.mgr.WriteStaged("../../../etc/passwd.txt", bytes.NewReader(gzBytes(t, pkg)))
	if err != nil {
		t.Fatalf("第二次落地失败: %v", err)
	}
	if !strings.HasSuffix(ref2, ".tar.gz") {
		t.Errorf("编号 %q 没按包的实际格式给后缀", ref2)
	}
	if strings.Contains(ref2, "..") {
		t.Errorf("客户端给的路径混进了编号：%q", ref2)
	}

	if _, _, err := f.mgr.WriteStaged("notes.txt", strings.NewReader("这不是压缩包\n")); err == nil {
		t.Error("一个文本文件也被收进了上传目录")
	}
	count, bytes := f.mgr.StagedUsage()
	if count != 2 || bytes == 0 {
		t.Errorf("上传目录的占用算错了：%d 个、%d 字节", count, bytes)
	}
	if err := f.mgr.DropStaged(ref); err != nil {
		t.Fatalf("删除上传包失败: %v", err)
	}
	if _, err := f.mgr.StagedPath(ref); err == nil {
		t.Error("删掉了还读得到")
	}
	if err := f.mgr.DropStaged(ref); err != nil {
		t.Errorf("重复删除不该报错（任务重试会再删一次）：%v", err)
	}
}

func TestStagedPathRejectsPathishRefs(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []string{"", "..", "../Cluster_1", "import-20261102T071500Z-x.zip/../../y",
		"my-handmade.zip", "import-2026110T071500Z-x.zip", "import-20261102T071500Z-x.rar",
		"import-20261102T071500Z-x.zip.tmp"} {
		if _, err := f.mgr.StagedPath(bad); err == nil {
			t.Errorf("编号 %q 被当成了合法的", bad)
		}
	}
}

func TestSweepStagedDropsOldUploads(t *testing.T) {
	f := newFixture(t)
	raw, err := os.ReadFile(stagedZip(t, f, saveTree("Cluster_9"), "zip"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, _, err := f.mgr.WriteStaged("fresh.zip", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	stale, _, err := f.mgr.WriteStaged("stale.zip", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(f.backups, incomingDirName, stale), old, old); err != nil {
		t.Fatal(err)
	}
	// 写一半被掐断的那种半成品也要被收掉——但"正在传"的那一份不能收：
	// 两条上传可以同时在跑，把活的那条吃了等于让另一个用户的上传凭空失败。
	dead := filepath.Join(f.backups, incomingDirName, fresh+writingSuffix)
	if err := os.WriteFile(dead, raw[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dead, old, old); err != nil {
		t.Fatal(err)
	}
	living := filepath.Join(f.backups, incomingDirName, stale+writingSuffix)
	if err := os.WriteFile(living, raw[:10], 0o644); err != nil {
		t.Fatal(err)
	}

	gone := f.mgr.SweepStaged(6 * time.Hour)
	if strings.Join(gone, ",") == "" {
		t.Fatal("过期的上传包一个都没清掉")
	}
	if !contains(gone, stale) {
		t.Errorf("过期的那份 %s 没被清掉：%v", stale, gone)
	}
	if _, err := f.mgr.StagedPath(fresh); err != nil {
		t.Errorf("没过期的那份被误删了：%v", err)
	}
	if _, err := os.Stat(dead); err == nil {
		t.Error("过期的 .writing 半成品留下来了")
	}
	if _, err := os.Stat(living); err != nil {
		t.Error("正在写的那份 .writing 被清了，同一次里另一条上传会被打断")
	}
}

// 上传目录里那两个临时目录不该出现在快照列表里，也不该占保留名额——
// 用户看到列表里多出一条"incoming"会以为是份坏掉的备份。
func TestIncomingDirectoryDoesNotPolluteSnapshotList(t *testing.T) {
	f := newFixture(t)
	f.writeCluster(t, "Cluster_1", []string{"slot-1"}, nil)
	pkg := stagedZip(t, f, saveTree("Cluster_9"), "zip")
	raw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.mgr.WriteStaged("keep.zip", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	f.mgr.Keep = 1
	mustCreate(t, f, "Cluster_1", LabelManual)

	list, err := f.mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if strings.HasPrefix(s.ID, incomingDirName) || strings.Contains(s.ID, "keep.zip") {
			t.Errorf("上传的临时包混进了快照列表：%+v", s)
		}
	}
	if len(list) != 1 {
		t.Errorf("列表里有 %d 条，只想要那 1 份备份：%v", len(list), snapshotIDs(list))
	}
}

func TestPruneUnpackedClearsCrashLeftovers(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.backups, unpackDirName, "1234567890")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "half.lua"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.mgr.PruneUnpacked(); got != 1 {
		t.Errorf("清掉了 %d 个残留，想要 1", got)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("上次没删完的解压目录还在")
	}
}

// 一个 helper：把已经压好的 zip 再压一层 gzip，用来验"后缀按字节认"那条。
func gzBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
