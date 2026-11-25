// 导入外部存档的接口用例。
//
// 这一层要钉住三件事：
//   - 上传只是落到备份目录的临时区：房间目录、快照列表、任务队列都不该因为"传了个包"而变化；
//   - "包不对"分两种回法：读不出这个包是错误（400/422），读得出但不是饥荒存档是预览结论
//     （200 + 能不能导入=否），因为界面要把整串原因摊开给用户看，而不是只给一句话；
//   - 提交口只入队，payload 只有面板拼的那三个字段，请求体里多出什么都进不去；
//     而"导入后就换进房间"那一步的前置拦截必须和恢复任务说同一句话。
package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"dstpanel/internal/backup"
	"dstpanel/internal/dst"
	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
	"dstpanel/internal/worker"
)

const importGameID = "8C1A2B3D4E5F6071"

// externalSave 是别人传过来的那份存档的真实形状：房间目录打头，
// 令牌、面板通道文件、引擎日志都混在里面（手工打包就是这样），
// 而档位是专用服务端那种纯数字编号（云机实测 10 位）。
func externalSave(root string) map[string]string {
	pre := root + "/"
	idx := func(name string) string {
		return "KLEI     1 \nreturn {\n    session_id = \"" + importGameID + "\",\n    name = \"" + name +
			"\",\n    root = \"" + name + "\",\n    enabled = true,\n}\n"
	}
	master := pre + "Master/save/session/" + importGameID + "/"
	caves := pre + "Caves/save/session/" + importGameID + "/"
	return map[string]string{
		pre + "cluster.ini":                    "cluster\n",
		pre + "cluster_token.txt":              fakeTokenSecret + "\n",
		pre + "Master/save/shardindex":         idx("Master"),
		master + "0000000085":                  "imported 地表世界\n",
		master + "0000000085.meta":             "return {clock={cycles=74}}\n",
		pre + "Master/save/dstpanel_queue.txt": "cmd-1|kick|阿黎|恶意\n",
		pre + "Master/server_log.txt":          "[00:00:01]: old boot log line\n",
		pre + "Master/server.ini":              "[SHARD]\nrole=MASTER\n",
		pre + "Master/modoverrides.lua":        "return {}\n",
		pre + "Caves/save/shardindex":          idx("Caves"),
		caves + "0000000002":                   "imported 洞穴世界\n",
		pre + "Caves/server.ini":               "[SHARD]\nrole=CAVES\n",
	}
}

func zipOf(t *testing.T, files map[string]string) []byte {
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

// uploadSave 按接口注册的那个字段名上传。字段名直接取常量而不在这儿抄一份字符串：
// 抄的那份在常量改名后会安静地一直撞"必须带文件字段"那条报错，看着像拦住了，其实一条正路都没走。
func uploadSave(t *testing.T, bf *backupAPIFixture, fileName string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return uploadSaveWithField(t, bf.f.srv.Handler(), importUploadField, fileName, body)
}

func uploadSaveWithField(t *testing.T, h http.Handler, field, fileName string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if fileName != "" {
		part, err := mw.CreateFormFile(field, fileName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backups/import/preview", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// previewAndRef 走完上传这一步并交出编号：提交导入的用例都要先有个合法编号，
// 而自己拼一个编号就等于没测"编号必须是面板生成的"那一条。
func previewAndRef(t *testing.T, bf *backupAPIFixture, files map[string]string) importPreviewView {
	t.Helper()
	rec := uploadSave(t, bf, "我的存档.zip", zipOf(t, files))
	if rec.Code != http.StatusOK {
		t.Fatalf("上传预览应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var v importPreviewView
	decodeData(t, rec, &v)
	return v
}

func importTasks(t *testing.T, st *store.Store) []store.Task {
	t.Helper()
	var out []store.Task
	for _, task := range queuedBackupTasks(t, st) {
		if task.Kind == worker.KindBackupImport {
			out = append(out, task)
		}
	}
	return out
}

func saveSlots(t *testing.T, paths dst.Paths, cluster, shard string) []string {
	t.Helper()
	entries, err := os.ReadDir(paths.SessionRoot(cluster, shard))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// 预览这一步的全部产出都只来自"读包的目录"：它必须既不动房间、也不动快照列表，
// 还要把界面上要展示的每一件事实一次给全（编号、提交地址、字段名、上限）。
func TestBackupImportPreviewReadsPackageWithoutTouchingAnything(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	before := saveSlots(t, bf.paths, bf.f.cluster, dst.ShardMaster)

	v := previewAndRef(t, bf, externalSave("Cluster_7"))

	if !v.CanImport {
		t.Fatalf("这是一份合法存档，却被判不能导入：%+v", v.Problems)
	}
	if v.PackageRef == "" || v.Package != v.PackageRef {
		t.Errorf("界面提交时要用的编号没和包名对上：ref=%q package=%q", v.PackageRef, v.Package)
	}
	if v.ImportPath != "/api/v1/backups/import" || v.UploadField != importUploadField {
		t.Errorf("前端要照抄的两个字符串没给对：%q / %q", v.ImportPath, v.UploadField)
	}
	if v.MaxUpload == "" {
		t.Error("上传上限没给中文标签，界面只能自己造单位")
	}
	// 编号是纯 ASCII 的机器 id（中文文件名在里面缩成下划线），界面那行靠这个字段认档。
	if v.SourceName != "我的存档.zip" {
		t.Errorf("原名没带回来，界面只剩一个看不懂的编号：%q", v.SourceName)
	}
	if v.Root != "Cluster_7" || v.Shape != "cluster" {
		t.Errorf("包里那个房间没被认出来：root=%q shape=%q", v.Root, v.Shape)
	}
	if len(v.Shards) != 2 {
		t.Fatalf("两个世界都要登记，实际 %+v", v.Shards)
	}
	for _, sh := range v.Shards {
		if sh.SessionID != importGameID {
			t.Errorf("%s 的世界编号读成 %q", sh.Shard, sh.SessionID)
		}
	}
	if !strings.Contains(v.Note, "认出来了") {
		t.Errorf("结论要一句人话先说能不能用：%q", v.Note)
	}
	// 令牌、通道文件、引擎日志必须出现在"被丢掉"的名单里并带原因：
	// 用户看到导入后没有令牌会以为面板丢了东西，这句话是他唯一的解释来源。
	dropped := strings.Join(v.Dropped, "；")
	for _, want := range []string{"cluster_token.txt", "dstpanel_queue.txt", "server_log.txt"} {
		if !strings.Contains(dropped, want) {
			t.Errorf("丢弃名单里没交代 %s：%q", want, dropped)
		}
	}
	if strings.Contains(dropped, "THIS_TEXT_MUST_NEVER_BE_BACKED_UP") || strings.Contains(v.Note, "THIS_TEXT_MUST_NEVER_BE_BACKED_UP") {
		t.Error("预览把令牌内容说出去了")
	}

	// 上传包落在临时区，此刻是能找回来的（下一步提交不用重传）。
	if _, err := bf.mgr.StagedPath(v.PackageRef); err != nil {
		t.Errorf("预览说能导入，上传包却不在盘上：%v", err)
	}
	// 但房间和快照列表一点没变：这一步只该多一个上传包。
	list, err := bf.mgr.List()
	if err != nil || len(list) != 1 {
		t.Errorf("只是预览，快照列表却变了：%+v err=%v", list, err)
	}
	if got := saveSlots(t, bf.paths, bf.f.cluster, dst.ShardMaster); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("只是预览，房间存档目录却动了：%v → %v", before, got)
	}
	if n := importTasks(t, bf.f.srv.Store); len(n) != 0 {
		t.Errorf("只是预览，队列里却多了任务：%+v", n)
	}
}

// 传上来的东西根本不是包：这一步就该挡住，而不是让它变成盘上一个"待导入的存档"。
// WriteStaged 是先认格式再建目录，所以三条拒收路径都must不留任何文件。
func TestBackupImportPreviewRejectsFileThatIsNotAPackage(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	h := bf.f.srv.Handler()

	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"说明.txt", []byte("这是我的存档，解压后放到 Klei 目录里\n"), "认不出"},
		{"空包.zip", []byte("PK\x05\x06"), "一个文件都没有"},
	}
	for _, c := range cases {
		rec := uploadSaveWithField(t, h, importUploadField, c.name, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应 400，实际 %d: %s", c.name, rec.Code, rec.Body.String())
			continue
		}
		if msg := readErr(t, rec); !strings.Contains(msg, c.want) {
			t.Errorf("%s 的报错没说到点上：%q", c.name, msg)
		}
	}

	// 没带文件字段：报错要念出字段名，不然照着别的接口抄的人会一直撞这条。
	rec := uploadSaveWithField(t, h, importUploadField, "", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(readErr(t, rec), importUploadField) {
		t.Errorf("缺字段应 400 并点名 %q，实际 %d: %s", importUploadField, rec.Code, rec.Body.String())
	}

	if count, _ := bf.mgr.StagedUsage(); count != 0 {
		t.Errorf("三条都拒收了，上传目录里却留下 %d 份", count)
	}
}

// 是合法的 zip，但里面不是饥荒存档：这不是 HTTP 错误，是预览要说清的结构结论。
// 回 200 才能让用户看到"面板认出了哪些、缺了什么、该怎么重压"；
// 回 400 的话界面只剩一句报错，而那一句恰恰给不出能照着做的下一步。
func TestBackupImportPreviewSaysPackageIsNotADSTSave(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	rec := uploadSave(t, bf, "游戏截图.zip", zipOf(t, map[string]string{
		"screenshot/01.png": "not a save",
		"README.txt":        "自己看着办",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("结构不对该回 200 并把问题摊开，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var v importPreviewView
	decodeData(t, rec, &v)
	if v.CanImport {
		t.Error("一份截图被判成可以导入")
	}
	if len(v.Problems) == 0 || !strings.Contains(v.Note, "不能导入") {
		t.Errorf("要给出成串的原因和一句总结：problems=%+v note=%q", v.Problems, v.Note)
	}
	// 这里必须给出"怎么压才对"，因为最常见的错法是只压了 save 目录或者只压了世界目录。
	if !strings.Contains(strings.Join(v.Problems, "；"), "压") {
		t.Errorf("结论里没有那句能照着做的：%+v", v.Problems)
	}
}

// 界面在读这三份名单之前都要先量 .length，而 Go 的 nil 切片进 JSON 是 null、
// 带 omitempty 干脆没这个键——两种都会让渲染那一下抛错，用户看到的是"点了读包没反应"，
// 而不是"这个包没有问题可报"。所以四份数组在响应里必须永远存在。
//
// 两个用例各测一半：结构不对的包 problems 有值、其余为空；合法的包 problems 为空。
// 只测其中一种的话，以后有人给某一个字段加回 omitempty，另一种情况照样漏得过去。
func TestBackupImportPreviewAlwaysSendsArrays(t *testing.T) {
	keys := []string{"shards", "dropped", "warnings", "problems"}

	checkArrays := func(name, file string, files map[string]string) {
		t.Helper()
		bf := newBackupAPIFixture(t, "")
		rec := uploadSave(t, bf, file, zipOf(t, files))
		var reply struct {
			OK   bool           `json:"ok"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatalf("%s: 响应不是合法 JSON: %v %q", name, err, rec.Body.String())
		}
		for _, key := range keys {
			v, ok := reply.Data[key]
			if !ok {
				t.Errorf("%s: %s 这个键在响应里根本没有，界面读它会拿到 undefined", name, key)
				continue
			}
			if _, isArray := v.([]any); !isArray {
				t.Errorf("%s: %s 不是数组（%#v），前端 .length 会在渲染时抛错", name, key, v)
			}
		}
	}

	checkArrays("结构不对的包", "notes.zip", map[string]string{"notes/plan.txt": "下次想建个森林房"})
	checkArrays("合法的包", "save.zip", externalSave("Cluster_7"))
}

// 提交导入只入队：几十 MB 的解包、打包、换目录都在 worker 里，
// 而 payload 只有面板拼出来的那三个字段。
func TestBackupImportEnqueuesWithoutMakingTheSnapshot(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	v := previewAndRef(t, bf, externalSave("Cluster_7"))

	rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/backups/import",
		backupImportRequest{Package: v.PackageRef, Cluster: bf.f.cluster})
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
	// 没勾"导入后就换进房间"时，note 不能承诺换目录——那是那条勾选才发生的事。
	if !strings.Contains(accepted.Note, "登记给房间 "+bf.f.cluster) || strings.Contains(accepted.Note, "整棵换掉") {
		t.Errorf("未勾恢复时的收尾话不对：%q", accepted.Note)
	}

	tasks := importTasks(t, bf.f.srv.Store)
	if len(tasks) != 1 {
		t.Fatalf("队列里应该正好一条导入任务：%+v", tasks)
	}
	var p worker.BackupImportPayload
	if err := tasks[0].PayloadInto(&p); err != nil {
		t.Fatalf("payload 读不出: %v", err)
	}
	if p.Package != v.PackageRef || p.Cluster != bf.f.cluster || p.Restore {
		t.Errorf("payload 被请求影响到了：%+v", p)
	}
	list, err := bf.mgr.List()
	if err != nil || len(list) != 1 {
		t.Errorf("只是入队，快照却多打了：%+v err=%v", list, err)
	}
	// 房间里的存档目录必须还是原来那两份档位（导入没做成之前谁都不该动它）。
	if got := saveSlots(t, bf.paths, bf.f.cluster, dst.ShardMaster); strings.Join(got, ",") != "slot-0001,slot-0002" {
		t.Errorf("只是入队，房间存档目录却动了：%v", got)
	}
}

// 二次确认落在服务端：前端的复选框藏得住按钮，藏不住一个直接发请求的人。
func TestBackupImportRestoreWithoutConfirmIsRefused(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	v := previewAndRef(t, bf, externalSave("Cluster_7"))

	rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/backups/import",
		backupImportRequest{Package: v.PackageRef, Cluster: bf.f.cluster, Restore: true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没带 confirm 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	msg := readErr(t, rec)
	if !strings.Contains(msg, "confirm") || !strings.Contains(msg, "整棵换掉") {
		t.Errorf("要说清为什么拦、拦的是哪一步：%q", msg)
	}
	if n := importTasks(t, bf.f.srv.Store); len(n) != 0 {
		t.Errorf("被拒的请求还是入队了：%+v", n)
	}
	// 挡下来也要留痕：这条失败记录是唯一能区分"没点"和"被挡"的依据。
	// 筛"backup.import"这个前缀会一并带出上传和预览那两条（族筛选就是这么设计的），
	// 所以要挑出动作完全相等的那一条来看。
	entries, err := bf.f.srv.Store.ListAudit(20, 0, "backup.import")
	if err != nil {
		t.Fatal(err)
	}
	var refused []store.AuditEntry
	for _, e := range entries {
		if e.Action == "backup.import" {
			refused = append(refused, e)
		}
	}
	if len(refused) != 1 {
		t.Fatalf("被拒的导入该留一条记录：%+v", refused)
	}
	if refused[0].OK || !strings.Contains(refused[0].Err, "confirm") {
		t.Errorf("这条要记成失败并且带上被拒的理由：%+v", refused[0])
	}
	if refused[0].Target != bf.f.cluster {
		t.Errorf("被拒的记录要点名是哪个房间：%+v", refused[0])
	}
}

// 在跑的世界绝不能换档：引擎会把内存里那份再存回去，表现为"导入没生效"。
// 这句话必须和恢复任务说的完全一致，因为界面靠同一个判断决定按钮灰不灰。
func TestBackupImportRestoreRefusesRunningWorld(t *testing.T) {
	bf := newBackupAPIFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")
	v := previewAndRef(t, bf, externalSave("Cluster_7"))

	rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/backups/import",
		backupImportRequest{Package: v.PackageRef, Cluster: bf.f.cluster, Restore: true, Confirm: true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("世界在跑应 409，实际 %d: %s", rec.Code, rec.Body.String())
	}
	msg := readErr(t, rec)
	if !strings.Contains(msg, "还有世界在跑") || !strings.Contains(msg, dst.ShardMaster) {
		t.Errorf("要点名是哪个世界在跑：%q", msg)
	}
	if n := importTasks(t, bf.f.srv.Store); len(n) != 0 {
		t.Errorf("被拦的恢复还是入队了：%+v", n)
	}
}

// 问不到进程状态时不能放行"换进房间"：这里要拦的是"我以为它关着"。
// 不勾恢复的导入不受影响（它不动房间），所以这条只测勾了的那一半。
func TestBackupImportRestoreRefusesWhenProbeFails(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	bf.f.srv.Procs = procrun.New(bf.paths, screenRunner{err: errors.New("screen: executable file not found in $PATH")})
	v := previewAndRef(t, bf, externalSave("Cluster_7"))

	rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/backups/import",
		backupImportRequest{Package: v.PackageRef, Cluster: bf.f.cluster, Restore: true, Confirm: true})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("问不清应 503，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if msg := readErr(t, rec); !strings.Contains(msg, "问不到世界进程状态") {
		t.Errorf("要如实说问不到：%q", msg)
	}
	if n := importTasks(t, bf.f.srv.Store); len(n) != 0 {
		t.Errorf("问不清却还是入队了：%+v", n)
	}
}

// 全停 + 带 confirm：这一步才允许承诺"换掉存档目录"。
func TestBackupImportRestoreFlagReachesTheTask(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	v := previewAndRef(t, bf, externalSave("Cluster_7"))

	rec := doJSON(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/backups/import",
		backupImportRequest{Package: v.PackageRef, Cluster: bf.f.cluster, Restore: true, Confirm: true})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("应 202，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var accepted struct {
		Note string `json:"note"`
	}
	decodeData(t, rec, &accepted)
	if !strings.Contains(accepted.Note, "整棵换掉") || !strings.Contains(accepted.Note, "安全备份") {
		t.Errorf("勾选恢复时的收尾要说清退路和动作：%q", accepted.Note)
	}
	tasks := importTasks(t, bf.f.srv.Store)
	if len(tasks) != 1 {
		t.Fatalf("应正好一条：%+v", tasks)
	}
	var p worker.BackupImportPayload
	if err := tasks[0].PayloadInto(&p); err != nil {
		t.Fatal(err)
	}
	if !p.Restore {
		t.Errorf("勾了恢复却没写进 payload：%+v", p)
	}
}

// 编号是请求参数、又参与拼路径，所以形状不合法的一律不去找文件；
// 形状对但已经不在了要说"重新上传"，因为那是这份任务最常见的真实失败原因（TTL 收走了）。
func TestBackupImportRejectsForeignOrMissingPackageRef(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	h := bf.f.srv.Handler()

	for _, bad := range []string{"", "my-handmade.zip", "../../klei", "import-../x.zip"} {
		rec := doJSON(t, h, http.MethodPost, "/api/v1/backups/import",
			backupImportRequest{Package: bad, Cluster: bf.f.cluster})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("编号 %q 应被形状检查挡住，实际 %d: %s", bad, rec.Code, rec.Body.String())
			continue
		}
		if msg := readErr(t, rec); !strings.Contains(msg, "不是面板生成的格式") {
			t.Errorf("编号 %q 的报错没说什么被挡：%q", bad, msg)
		}
	}

	rec := doJSON(t, h, http.MethodPost, "/api/v1/backups/import",
		backupImportRequest{Package: "import-20261102T071500Z-gone.zip", Cluster: bf.f.cluster})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("形状对却不该回 %d: %s", rec.Code, rec.Body.String())
	}
	if msg := readErr(t, rec); !strings.Contains(msg, "重新上传") {
		t.Errorf("包被收走时要给出下一步：%q", msg)
	}
	if n := importTasks(t, bf.f.srv.Store); len(n) != 0 {
		t.Errorf("这些请求不该入队：%+v", n)
	}
}

// 提交时必须重新读一遍包：不能信客户端"我已经预览过了"。
// 这一条防的是把预览过的那个包换成另一个内容同名文件，或者干脆没预览就发编号。
func TestBackupImportRefusesUnusablePackageAtSubmit(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	rec := uploadSave(t, bf, "不是存档.zip", zipOf(t, map[string]string{"readme.txt": "自己看着办\n"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("预览应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var v importPreviewView
	decodeData(t, rec, &v)
	if v.CanImport {
		t.Fatal("夹具造的包被认成可导入，后面两条断言都在测空气")
	}

	submit := doJSON(t, bf.f.srv.Handler(), http.MethodPost, "/api/v1/backups/import",
		backupImportRequest{Package: v.PackageRef, Cluster: bf.f.cluster})
	if submit.Code != http.StatusUnprocessableEntity {
		t.Fatalf("不能导入的包应 422，实际 %d: %s", submit.Code, submit.Body.String())
	}
	msg := readErr(t, submit)
	if !strings.Contains(msg, "这个包不能导入") || !strings.Contains(msg, "一点都没动") {
		t.Errorf("要说清既没放行也没动盘：%q", msg)
	}
	if n := importTasks(t, bf.f.srv.Store); len(n) != 0 {
		t.Errorf("却还是入队了：%+v", n)
	}
	// 拦下来不删上传包：这一份还占着多少盘由存档页那句临时上传说明交代，
	// 删不删不该由一条报错决定。
	if count, _ := bf.mgr.StagedUsage(); count != 1 {
		t.Errorf("被拒的上传包不该被顺手删掉或复制，实际留了 %d 份", count)
	}
}

// 房间名要对着磁盘名单核：这个字符串决定快照登记给谁、以及勾了恢复时换掉哪个目录。
func TestBackupImportRejectsUnknownCluster(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	v := previewAndRef(t, bf, externalSave("Cluster_7"))
	h := bf.f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, "/api/v1/backups/import",
		backupImportRequest{Package: v.PackageRef, Cluster: "Cluster_9"})
	if rec.Code != http.StatusNotFound || !strings.Contains(readErr(t, rec), "Cluster_9") {
		t.Errorf("不存在的房间要 404 并点名：%d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/backups/import", backupImportRequest{Package: v.PackageRef})
	if rec.Code != http.StatusBadRequest || !strings.Contains(readErr(t, rec), "房间名") {
		t.Errorf("没给房间名应 400 并说要什么：%d %s", rec.Code, rec.Body.String())
	}
	if n := importTasks(t, bf.f.srv.Store); len(n) != 0 {
		t.Errorf("两次都该停在入队之前：%+v", n)
	}
}

// 存档页那份导入现状：上限、字段名、两条 URL 必须由后端给，前端一处都不该抄。
// 临时上传的份数和占用是 4G 小机上唯一"看不见的盘"，界面要能说出来。
func TestBackupsPageCarriesImportPolicy(t *testing.T) {
	bf := newBackupAPIFixture(t, "")
	h := bf.f.srv.Handler()

	rec := do(t, h, http.MethodGet, "/api/v1/backups", "admin", "secret")
	var page backupsPage
	decodeData(t, rec, &page)
	if page.Import.MaxArchiveBytes != backup.MaxImportArchiveBytes {
		t.Errorf("上限和 backup 包里的不是同一个数：页面 %+v", page.Import.MaxArchiveBytes)
	}
	if page.Import.MaxArchiveLabel == "" || page.Import.UploadField != importUploadField {
		t.Errorf("界面要抄的字符串没给全：%+v", page.Import)
	}
	if page.Import.PreviewPath != "/api/v1/backups/import/preview" || page.Import.ImportPath != "/api/v1/backups/import" {
		t.Errorf("两条 URL 不对：%+v", page.Import)
	}
	if page.Import.ExpireHours <= 0 {
		t.Errorf("过期小时数没跟上 backup.StagedUploadTTL：%+v", page.Import)
	}
	if !strings.Contains(page.Import.Note, "不会直接改房间") {
		t.Errorf("这句话是导入唯一的动线说明：%q", page.Import.Note)
	}
	if page.Import.StagedCount != 0 {
		t.Errorf("新目录该是空的：%+v", page.Import)
	}

	previewAndRef(t, bf, externalSave("Cluster_7"))
	rec = do(t, h, http.MethodGet, "/api/v1/backups", "admin", "secret")
	decodeData(t, rec, &page)
	if page.Import.StagedCount != 1 || page.Import.StagedBytes <= 0 || page.Import.StagedLabel == "" {
		t.Errorf("上传过一份却没统计出来：%+v", page.Import)
	}
	if !strings.Contains(page.Import.Note, "还留着 1 份") {
		t.Errorf("临时上传那句话要跟着数字走：%q", page.Import.Note)
	}
}

// 快照列表怎么展示导入来的那一份，是"数据 → 界面"那一段的事，已由
// internal/worker/backup_import_test.go 的 MakesSnapshotAndLeavesRoomAlone 钉住
// （来源标签、能不能恢复、令牌不在包里）。接口层这边没有 worker 可跑任务，
// 所以这里只钉"任务有没有被正确入队"，不重复验产物。
