package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/mods"
	"dstpanel/internal/store"
)

// ensureModsDir 造出真实安装目录的前提：dstInstall/mods 一定存在。
// 面板对"目录都没有"的态度是报错而不是显示空列表，所以要用例自己补。
func ensureModsDir(t *testing.T, f *apiFixture) string {
	t.Helper()
	dir := f.paths.ModsRoot()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func zipBytes(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		fw, err := w.Create(e[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func doUpload(t *testing.T, h http.Handler, fields map[string]string, fileName string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if fileName != "" {
		part, err := mw.CreateFormFile("mod", fileName)
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
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mods/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func readErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var r reply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("响应不是合法 JSON: %v %q", err, rec.Body.String())
	}
	if r.OK {
		t.Fatalf("期望错误响应，实际 %s", rec.Body.String())
	}
	return r.Err
}

func TestModsShowEmptyInstall(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	var v modsView
	decodeData(t, rec, &v)
	if len(v.Mods) != 0 {
		t.Errorf("空安装目录该没有模组: %+v", v.Mods)
	}
	if v.SetupPath == "" {
		t.Error("要给界面报清单文件路径，用户才知道去核什么")
	}
}

// mkModDir 在 mods/ 下造一个模组目录。body 为空时只留目录名，modinfo.lua 都没有——
// 那就是"磁盘上有目录、面板读不出名字"的那种真实情况。
func mkModDir(t *testing.T, modsRoot, dir, body string) {
	t.Helper()
	full := filepath.Join(modsRoot, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if body == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(full, "modinfo.lua"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 下载页那一屏现在也画封面，所以"见过的有图、没见过的没图"必须有用例盯着：
// 封面、订阅数、大小和"更新于"都不在模组目录里，只能来自面板在线检索留下的缓存。
// 给一个没读到的模组编一张图，界面就会把不存在的信息说成事实；
// 反过来，一个真见过的模组没被补上，用户看到的是"这个模组坏了"。
func TestModsViewBackfillsCoverOnlyForSeenMods(t *testing.T) {
	f := newFixture(t, "")
	root := ensureModsDir(t, f)
	mkModDir(t, root, "workshop-111", "name = \"工坊见过的模组\"\nversion = \"1\"\n")
	mkModDir(t, root, "workshop-222", "name = \"没见过一次的模组\"\n")

	if err := f.srv.Store.RecordSeen(store.SeenMod{
		WorkshopID: "111", Name: "工坊见过的模组", Version: "2",
		PreviewURL:    "https://cdn.example.invalid/111.png",
		Subscriptions: 1234, Favorited: 56, VoteScore: 0.91, VotesTotal: 789,
		FileSize: 1048576, ModUpdatedAt: 1730000000,
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("磁盘那一屏该直接回数据: %s", rec.Body.String())
	}
	var v modsView
	decodeData(t, rec, &v)
	byID := map[string]modView{}
	for _, m := range v.Mods {
		byID[m.WorkshopID] = m
	}
	seen, ok := byID["111"]
	if !ok {
		t.Fatalf("111 没出现在这一屏: %+v", v.Mods)
	}
	if seen.PreviewURL != "https://cdn.example.invalid/111.png" {
		t.Errorf("见过的模组要补上缓存里的封面，实际 %q", seen.PreviewURL)
	}
	if seen.Subscriptions != 1234 || seen.Favorited != 56 || seen.VoteScore != 0.91 || seen.VotesTotal != 789 {
		t.Errorf("订阅/收藏/评分要一起补进来: %+v", seen)
	}
	if seen.SizeLabel == "" || seen.UpdatedLabel == "" {
		t.Errorf("大小和更新时间的中文文案要由后端出，界面不自己拼: %+v", seen)
	}
	unseen, ok := byID["222"]
	if !ok {
		t.Fatalf("222 没出现在这一屏: %+v", v.Mods)
	}
	if unseen.PreviewURL != "" || unseen.Subscriptions != 0 || unseen.Favorited != 0 {
		t.Errorf("一次都没见过的模组不许有封面和订阅数: %+v", unseen)
	}

	// 目录里读不出名字时，缓存里的工坊标题比 workshop-数字 好读得多；
	// 但磁盘上读得出的名字必须以磁盘为准（那是这个版本自己写的）。
	if byID["111"].Name != "工坊见过的模组" {
		t.Errorf("磁盘名字优先，实际 %q", byID["111"].Name)
	}
	if seen.Version != "1" {
		t.Errorf("modinfo 里有版本就该用它的，不许被缓存的 2 盖掉: %q", seen.Version)
	}

	mkModDir(t, root, "workshop-333", "")
	if err := f.srv.Store.RecordSeen(store.SeenMod{WorkshopID: "333", Name: "只剩缓存名字", Version: "7"}); err != nil {
		t.Fatal(err)
	}
	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	var v2 modsView
	decodeData(t, rec, &v2)
	var three *modView
	for i := range v2.Mods {
		if v2.Mods[i].WorkshopID == "333" {
			three = &v2.Mods[i]
		}
	}
	if three == nil {
		t.Fatalf("333 没出现在这一屏: %+v", v2.Mods)
	}
	if three.Name != "只剩缓存名字" || three.Version != "7" {
		t.Errorf("磁盘读不出名字和版本时才用缓存回填，实际 %+v", *three)
	}
	if three.ModinfoOK {
		t.Errorf("没 modinfo.lua 的目录要报读不懂：%+v", *three)
	}
	// 这一句会直接画在模组卡上：既不能是裸英文，也不能把安装目录的绝对路径端出去。
	if !strings.Contains(three.ModinfoErr, "modinfo.lua") ||
		strings.Contains(three.ModinfoErr, "The system cannot find") ||
		strings.Contains(three.ModinfoErr, "open ") {
		t.Errorf("界面那句要中文、不带 os 错误原文：%q", three.ModinfoErr)
	}
	if strings.Contains(three.ModinfoErr, root) {
		t.Errorf("不许把安装目录绝对路径写进界面文案：%q", three.ModinfoErr)
	}
}

// TestModVersionDiffSurvivesPageReload 盯的是"工坊更新了"这条提示的生死线。
//
// 打开一次模组列表就等于扫一次盘，而扫盘会把这台机器上的版本写进缓存的 version 列。
// 工坊那个版本以前存在同一列里，于是第一次 GET 之后两个数就变成同一个数，
// versionDiffers 永远是假、「删文件重下」按钮永远不出现——用户问的那个提示
// 就在自己的加载动作里被吃掉了。
func TestModVersionDiffSurvivesPageReload(t *testing.T) {
	f := newFixture(t, "")
	root := ensureModsDir(t, f)
	mkModDir(t, root, "workshop-111", "name = \"工坊见过的模组\"\nversion = \"1.7\"\n")
	if err := f.srv.Store.RecordSeen(store.SeenMod{
		WorkshopID: "111", Name: "工坊见过的模组", Version: "9.9",
		Origin: store.SeenFromWorkshop,
	}); err != nil {
		t.Fatal(err)
	}

	// 连读两次：第一次 GET 自带扫盘，第二次的结论必须和第一次一样。
	for i := 0; i < 2; i++ {
		rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次读取: %s", i+1, rec.Body.String())
		}
		var v modsView
		decodeData(t, rec, &v)
		var m *modView
		for j := range v.Mods {
			if v.Mods[j].WorkshopID == "111" {
				m = &v.Mods[j]
			}
		}
		if m == nil {
			t.Fatalf("第 %d 次读取里没有 111: %+v", i+1, v.Mods)
		}
		if m.Version != "1.7" {
			t.Errorf("第 %d 次：磁盘那一屏上的版本该是这台机器上的 1.7，实际 %q", i+1, m.Version)
		}
		if m.WorkshopVersion != "9.9" {
			t.Errorf("第 %d 次：工坊版本被扫盘刷掉了，实际 %q", i+1, m.WorkshopVersion)
		}
		if !m.VersionDiffers {
			t.Errorf("第 %d 次：磁盘 1.7 / 工坊 9.9 该报版本对不上（界面据此画徽标和「删文件重下」）", i+1)
		}
	}

	// 只有磁盘那份、从没在线见过工坊的模组不许报版本对不上：
	// 没有可比的那个数，报出来就是凭空气提示用户"工坊更新了"。
	mkModDir(t, root, "workshop-555", "name = \"只在磁盘上\"\nversion = \"1.0\"\n")
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	var v modsView
	decodeData(t, rec, &v)
	for _, m := range v.Mods {
		if m.WorkshopID == "555" && (m.VersionDiffers || m.WorkshopVersion != "") {
			t.Errorf("没在工坊见过的那一条不该报版本差异: %+v", m)
		}
	}
}

// 三种状态必须分开摆：文件读不到、文件读得通但名字是算出来的、名字从工坊缓存补上了。
// 把它们混成一句"modinfo 读不懂"是用户那句"这是 mod 的问题还是你的问题"的直接来源——
// 本机 62 份真实 modinfo 里，把 name 写成表达式/分支的多语言模组占大头，
// 那些文件本身好好的，面板不该说出"读不懂"这种自相矛盾的话。
func TestModsViewKeepsUnreadableFileApartFromUnreadableName(t *testing.T) {
	f := newFixture(t, "")
	root := ensureModsDir(t, f)
	mkModDir(t, root, "workshop-401",
		"local iszh = true\nif iszh then\n    name = \"分支里的中文名\"\nend\nversion = \"1\"\n")
	mkModDir(t, root, "workshop-402", "name = en_zh(\"English\", \"中文\")\nversion = \"2\"\n")
	if err := f.srv.Store.RecordSeen(store.SeenMod{WorkshopID: "402", Name: "工坊标题补上的名字"}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	var v modsView
	decodeData(t, rec, &v)
	byID := map[string]modView{}
	for _, m := range v.Mods {
		byID[m.WorkshopID] = m
	}

	n401 := byID["401"]
	if !n401.ModinfoOK {
		t.Errorf("文件读得通就不许报读不懂：%+v", n401)
	}
	if n401.Name != "" {
		t.Errorf("分支里的名字不能凭运气挑一支: %q", n401.Name)
	}
	if !strings.Contains(n401.ModinfoErr, "读得通") || strings.Contains(n401.ModinfoErr, "读不懂") {
		t.Errorf("这句要承认文件是读得通的：%q", n401.ModinfoErr)
	}
	if !strings.HasSuffix(n401.ModinfoErr, "只能按目录名显示") {
		t.Errorf("名字没有着落时，这句要说清屏幕上顶上来的是目录名：%q", n401.ModinfoErr)
	}

	n402 := byID["402"]
	if n402.Name != "工坊标题补上的名字" {
		t.Errorf("缓存里的工坊标题要补上，实际 %q", n402.Name)
	}
	// 屏幕上已经是一个正常名字，那句"名字读不出来"再挂着就是无谓的警报。
	if n402.ModinfoErr != "" {
		t.Errorf("名字有了着落就不许再留警示：%q", n402.ModinfoErr)
	}
}

// fakeEngine 造出"本体已经装好"的磁盘前提：bin64 下那个二进制在不在，
// 决定面板把读不到的模组目录判成"还没装本体"还是"dstInstall 指错了"。
func fakeEngine(t *testing.T, f *apiFixture) {
	t.Helper()
	bin := filepath.Join(f.paths.BinDir(), "dontstarve_dedicated_server_nullrenderer_x64")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestModsShowFailsLouderThanEmptyWhenInstallMissing(t *testing.T) {
	// 本体没装：模组目录当然也不存在，这时候喊"检查 dstInstall 配置"是把新手支去
	// 改一个本来没写错的设置，所以这一支要说"先去任务中心下载本体"。
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("模组目录读不到应该报错，实际 %d %s", rec.Code, rec.Body.String())
	}
	msg := readErr(t, rec)
	if !strings.Contains(msg, "本体还没装") || !strings.Contains(msg, "任务中心") {
		t.Errorf("本体没装时要说清原因和下一步，实际 %q", msg)
	}

	// 本体在、mods/ 不在：这才是配置指错了，必须点到 dstInstall 这一项。
	f2 := newFixture(t, "")
	fakeEngine(t, f2)
	rec2 := do(t, f2.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("配置指错应该报错，实际 %d %s", rec2.Code, rec2.Body.String())
	}
	if msg2 := readErr(t, rec2); !strings.Contains(msg2, "dstInstall") {
		t.Errorf("错误要说清指向哪个配置，实际 %q", msg2)
	}
}

// 界面要把 `size = 2` 这种码值翻成"加倍"，靠的就是这个字典接口。
func TestModSchemaGivesChoicesNotCodes(t *testing.T) {
	f := newFixture(t, "")
	dir := ensureModsDir(t, f)
	full := filepath.Join(dir, "workshop-378545146")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `name = "加倍背包"
api_version = 10
local SIZES =
{
    {description = "普通", data = 8},
    {description = "加倍", data = 15},
}
configuration_options =
{
    { name = "size", label = "背包格数", hover = "越大越吃内存", data = SIZES, default = 8 },
    { name = "note", label = "备注", typing = "string", default = "none" },
}
`
	if err := os.WriteFile(filepath.Join(full, "modinfo.lua"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods/workshop-378545146/schema", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("字典接口应该直接回数据: %s", rec.Body.String())
	}
	var sc mods.Schema
	decodeData(t, rec, &sc)
	if sc.From != "configuration_options" {
		t.Errorf("字典来源要报准: %s", sc.From)
	}
	if len(sc.Options) != 2 {
		t.Fatalf("两项设置，实际 %+v", sc.Options)
	}
	size := sc.Options[0]
	if size.Kind != "select" || len(size.Choices) != 2 {
		t.Fatalf("size 应该是两档下拉: %+v", size)
	}
	if size.Choices[1].Label != "加倍" || size.Choices[1].Value != float64(15) {
		t.Errorf("候选项的中文标签/值不对: %+v", size.Choices)
	}
	if size.DefaultLabel != "普通" {
		t.Errorf("默认值要显示成档位名，实际 %q", size.DefaultLabel)
	}
	if sc.Options[1].Kind != "text" {
		t.Errorf("typing=string 该画文本框: %+v", sc.Options[1])
	}

	// 目录不存在 / 目录名非法：都给 404 加一句能照着做的中文，而不是 500。
	for _, bad := range []string{"no-such-mod", "..hidden"} {
		rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods/"+bad+"/schema", "admin", "secret")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 应该 404，实际 %d %s", bad, rec.Code, rec.Body.String())
		}
		if msg := readErr(t, rec); !strings.Contains(msg, "modinfo.lua") && !strings.Contains(msg, "目录名") {
			t.Errorf("%s 的错误要说清下一步，实际 %q", bad, msg)
		}
	}
}

func TestSetupAddAndRemoveRoundTrip(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, "/api/v1/mods/setup", setupAddRequest{WorkshopID: "378545146"})
	var v modsView
	decodeData(t, rec, &v)
	if len(v.PendingDownload) != 1 || v.PendingDownload[0].WorkshopID != "378545146" {
		t.Errorf("登记后应显示待下载: %+v", v.PendingDownload)
	}
	// 这条是手工填 ID 登记的，缓存里从来没有过它：名字必须标成"不知道"，
	// 而不是把 workshop-378545146 当成模组名字显示（用户要核对的正是名字）。
	if v.PendingDownload[0].NameKnown {
		t.Errorf("没见过的 ID 不该有一个编出来的名字：%+v", v.PendingDownload[0])
	}
	if !strings.Contains(v.Hint, "重启") {
		t.Errorf("提示要说清什么时候才会下载，实际 %q", v.Hint)
	}
	text, err := os.ReadFile(f.paths.ServerModSetup())
	if err != nil || !strings.Contains(string(text), `ServerModSetup("378545146")`) {
		t.Fatalf("清单没写对: %q %v", text, err)
	}

	// 重复订阅不报错，但要说明没动文件。
	rec = doJSON(t, h, http.MethodPost, "/api/v1/mods/setup", setupAddRequest{WorkshopID: "378545146"})
	decodeData(t, rec, &v)
	if !strings.Contains(v.Hint, "早就在订阅清单里") {
		t.Errorf("重复订阅要说明幂等，实际 %q", v.Hint)
	}

	rec = do(t, h, http.MethodDelete, "/api/v1/mods/setup/378545146", "admin", "secret")
	decodeData(t, rec, &v)
	text, _ = os.ReadFile(f.paths.ServerModSetup())
	if strings.Contains(string(text), "378545146") {
		t.Errorf("移除后清单里还剩着:\n%s", text)
	}

	entries, err := f.srv.Store.ListAudit(50, 0, "mod.setup.")
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	if !strings.Contains(strings.Join(actions, ","), "mod.setup.add") ||
		!strings.Contains(strings.Join(actions, ","), "mod.setup.remove") {
		t.Errorf("登记与移除都要留审计，实际 %v", actions)
	}
}

func TestSetupAddRejectsBadIDWithoutTouchingFile(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, "/api/v1/mods/setup",
		setupAddRequest{WorkshopID: `1); os.execute("rm -rf /") //`})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("注入形态的 id 应该 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if msg := readErr(t, rec); !strings.Contains(msg, "数字") {
		t.Errorf("要说清合法形态，实际 %q", msg)
	}
	if _, err := os.Stat(f.paths.ServerModSetup()); err == nil {
		text, _ := os.ReadFile(f.paths.ServerModSetup())
		if strings.Contains(string(text), "os.execute") {
			t.Fatalf("被拒的内容进了清单:\n%s", text)
		}
	}

	// 路径参数同样要挡：DELETE /mods/setup/anything-non-numeric
	rec = do(t, h, http.MethodDelete, "/api/v1/mods/setup/not-a-number", "admin", "secret")
	if rec.Code == http.StatusOK {
		t.Error("非数字 id 的删除请求不该成功")
	}
}

func TestModsSetupProblemsSurfaceInGET(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	// 用户手工编辑过清单，写了引擎能跑但面板没法安全增删的写法。
	if err := os.WriteFile(f.paths.ServerModSetup(),
		[]byte("ServerModSetup(\"111\")\nlocal x = 1\nprint(x)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/mods", "admin", "secret")
	var v modsView
	decodeData(t, rec, &v)
	if len(v.SetupProblems) != 2 {
		t.Errorf("两行看不懂的写法都该报出来: %+v", v.SetupProblems)
	}
	if !strings.Contains(v.Hint, "退出") {
		t.Errorf("提示要说明后果是服务端起不来，实际 %q", v.Hint)
	}
}

func TestUploadCreatesModAndShowsLocalOnly(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()

	body := zipBytes(t,
		[2]string{"modinfo.lua", `return { name = "上传的模组", version = "1.0" }`},
		[2]string{"modmain.lua", "print('hi')"},
	)
	rec := doUpload(t, h, map[string]string{"name": "UploadedByAPI"}, "uploaded-by-api.zip", body)
	var v modsView
	decodeData(t, rec, &v)
	if _, err := os.Stat(filepath.Join(f.paths.ModsRoot(), "UploadedByAPI", "modinfo.lua")); err != nil {
		t.Fatalf("文件没落地: %v", err)
	}
	if len(v.LocalOnly) != 1 || v.LocalOnly[0] != "UploadedByAPI" {
		t.Errorf("本地模组要出现在 localOnly 里: %+v", v.LocalOnly)
	}
	if !strings.Contains(v.Hint, "打开") {
		t.Errorf("上传完必须提醒还要按分片启用，实际 %q", v.Hint)
	}
	if len(v.Mods) != 1 || v.Mods[0].Name != "上传的模组" {
		t.Errorf("列表里没认出这个模组: %+v", v.Mods)
	}

	// 同名再来一次：拒绝，而且原文件不动。
	rec = doUpload(t, h, map[string]string{"name": "UploadedByAPI"}, "other.zip", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("重复上传应该 400，实际 %d", rec.Code)
	}
	if msg := readErr(t, rec); !strings.Contains(msg, "已经存在") {
		t.Errorf("要说清是同名的锅，实际 %q", msg)
	}

	entries, err := f.srv.Store.ListAudit(50, 0, "mod.upload")
	if err != nil {
		t.Fatal(err)
	}
	// 成功的和被他拒的都要留痕，界面才能回答"我刚才那次到底传上去了没有"。
	var succeeded, refused bool
	for _, e := range entries {
		if e.Err == "" {
			succeeded = true
		} else {
			refused = true
		}
	}
	if !succeeded || !refused {
		t.Errorf("上传的成功与失败都该有审计，实际 %+v", entries)
	}
}

func TestUploadDerivesNameFromFilename(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	body := zipBytes(t, [2]string{"Geometric_Department/modinfo.lua", `return { name = "几何防御" }`})
	rec := doUpload(t, f.srv.Handler(), nil, "Geometric_Department.zip", body)
	var v modsView
	decodeData(t, rec, &v)
	if len(v.Mods) != 1 || v.Mods[0].Dir != "Geometric_Department" {
		t.Errorf("没给 name 时该从文件名取目录名: %+v", v.Mods)
	}
}

func TestUploadRejectsTraversalAndMissingFile(t *testing.T) {
	f := newFixture(t, "")
	modsDir := ensureModsDir(t, f)
	h := f.srv.Handler()

	bad := zipBytes(t,
		[2]string{"modinfo.lua", `return { name = "x" }`},
		[2]string{"../../outside.lua", "bad"},
	)
	rec := doUpload(t, h, map[string]string{"name": "BadZip"}, "bad.zip", bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("穿越包该 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(modsDir), "outside.lua")); err == nil {
		t.Error("穿越条目真的写到 mods/ 外面了")
	}
	// 没带文件字段也要是 400，不能 panic 也不能 500。
	rec = doUpload(t, h, map[string]string{"name": "NoFile"}, "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺文件字段应该 400，实际 %d", rec.Code)
	}
	// 非法目录名要拦住，且不能顺手把 mods/ 里的东西改掉。
	rec = doUpload(t, h, map[string]string{"name": "DSTPanelHelper"}, "x.zip",
		zipBytes(t, [2]string{"modinfo.lua", `return { name = "x" }`}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("覆盖助手目录应该 400，实际 %d", rec.Code)
	}
}

func TestShardModsEnableAndOptions(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	body := zipBytes(t, [2]string{"modinfo.lua", `return { name = "地图增强", version = "2.1" }`})
	if rec := doUpload(t, h, map[string]string{"name": "MapPlus"}, "mp.zip", body); rec.Code != http.StatusOK {
		t.Fatalf("前置上传失败: %d %s", rec.Code, rec.Body.String())
	}

	// 模组在磁盘上但分片还没开：列表要显示 present=false 的缺席状态而不是报错。
	rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/mods", "admin", "secret")
	var sv shardModsView
	decodeData(t, rec, &sv)
	if len(sv.Mods) != 0 {
		t.Errorf("还没启用过不该有条目: %+v", sv.Mods)
	}

	enabled := true
	priority := 0.0
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/MapPlus",
		overridePatchRequest{Enabled: &enabled, Priority: &priority,
			Options: map[string]any{"show_names": true, "radius": 3}})
	decodeData(t, rec, &sv)
	if len(sv.Mods) != 1 || !sv.Mods[0].Enabled || !sv.Mods[0].OnDisk {
		t.Errorf("启用后状态不对: %+v", sv.Mods)
	}
	if sv.Mods[0].Options["show_names"] != true {
		t.Errorf("布尔配置项没读回来: %+v", sv.Mods[0].Options)
	}
	text, err := os.ReadFile(f.paths.ModOverrides("Cluster_1", "Master"))
	if err != nil {
		t.Fatal(err)
	}
	// MapPlus 是合法 Lua 标识符，渲染器按标识符写（不套 ["..."]）；
	// workshop-123 那种带连字符的才需要方括号。断言跟着渲染器走，不是反过来。
	if !strings.Contains(string(text), "MapPlus = {") || !strings.Contains(string(text), "enabled = true") {
		t.Fatalf("modoverrides.lua 内容不对:\n%s", text)
	}

	// 只改一个配置项时，另一个必须原样留着——整片覆盖会把用户填过的设置抹掉。
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/MapPlus",
		overridePatchRequest{Options: map[string]any{"radius": 5}})
	decodeData(t, rec, &sv)
	if sv.Mods[0].Options["show_names"] != true {
		t.Errorf("改 radius 时把 show_names 弄丢了: %+v", sv.Mods[0].Options)
	}
	if number, ok := sv.Mods[0].Options["radius"].(float64); !ok || number != 5 {
		t.Errorf("radius 没更新: %+v", sv.Mods[0].Options)
	}

	// 洞穴分片是另一个文件，不该被 Master 的操作捎带改掉。
	rec = do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Caves/mods", "admin", "secret")
	var caves shardModsView
	decodeData(t, rec, &caves)
	if len(caves.Mods) != 0 {
		t.Errorf("Caves 不该被连带启用: %+v", caves.Mods)
	}
}

func TestShardModsRejectUnknownModAndBadOptions(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	enabled := true

	// mods/ 里没有的东西不能凭空写进覆盖文件。
	rec := doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/NoSuchMod",
		overridePatchRequest{Enabled: &enabled})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("磁盘上没有的模组该 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if msg := readErr(t, rec); !strings.Contains(msg, "上传") || !strings.Contains(msg, "重启") {
		t.Errorf("404 要说下一步怎么办，实际 %q", msg)
	}
	if _, err := os.Stat(f.paths.ModOverrides("Cluster_1", "Master")); err == nil {
		t.Error("被拒的请求还是写出了 modoverrides.lua")
	}

	// 路径里塞穿越也进不到 PatchOverride。
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/..%2Fevil",
		overridePatchRequest{Enabled: &enabled})
	if rec.Code == http.StatusOK {
		t.Error("非法目录名竟然被接受")
	}

	// 配置项只收标量：嵌套结构是面板读不懂的第三方格式，不能替用户猜。
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/Whatever",
		overridePatchRequest{Options: map[string]any{"nested": map[string]any{"a": 1}}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("模组不存在时该先 404，实际 %d", rec.Code)
	}
	// 先造一个存在的模组目录，再试嵌套配置项。
	dir := filepath.Join(f.paths.ModsRoot(), "RealMod")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "modinfo.lua"), []byte(`return { name = "真实模组" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/RealMod",
		overridePatchRequest{Options: map[string]any{"nested": map[string]any{"a": 1}}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("嵌套配置项该 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if msg := readErr(t, rec); !strings.Contains(msg, "文本、数字或是否") {
		t.Errorf("要说清允许的类型，实际 %q", msg)
	}

	// 空 patch 也要拒，否则界面点一下就写出一个空壳条目。
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/RealMod", overridePatchRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("全空 patch 应该 400，实际 %d", rec.Code)
	}
}

// TestSubscribeAndOpenOneClick 是"三步合成一步"这条动线的后端那一半：
// 一次请求要同时落下订阅清单和这个房间所有世界的开关。
func TestSubscribeAndOpenOneClick(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, "/api/v1/mods/subscribe-and-open",
		subscribeOpenRequest{WorkshopID: "777111", Cluster: "Cluster_1", Shards: []string{"Master", "Caves"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("订阅并开启该 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var v subscribeOpenView
	decodeData(t, rec, &v)
	if !v.Added || v.Downloaded {
		t.Errorf("这一次既没在清单里也没在磁盘上，Added/Downloaded 该是 true/false：%+v", v)
	}
	if len(v.Opened) != 2 || len(v.Failed) != 0 {
		t.Errorf("两个世界都要写成：%+v", v)
	}
	// 提示语要自己把"什么时候才生效"说清楚：用户点完最想知道的就是它现在算不算装好了。
	for _, want := range []string{"已订阅", "还没在这台机器上", "重启", "Cluster_1 / Master", "Cluster_1 / Caves"} {
		if !strings.Contains(v.Hint, want) {
			t.Errorf("提示里该有 %q，实际 %q", want, v.Hint)
		}
	}

	text, err := os.ReadFile(f.paths.ServerModSetup())
	if err != nil || !strings.Contains(string(text), `ServerModSetup("777111")`) {
		t.Fatalf("清单没写对: %q %v", text, err)
	}
	for _, shard := range []string{"Master", "Caves"} {
		ov, err := os.ReadFile(f.paths.ModOverrides("Cluster_1", shard))
		if err != nil {
			t.Fatalf("%s: %v", shard, err)
		}
		// 工坊目录名带连字符，渲染器要用 ["workshop-777111"] 这种方括号键。
		if !strings.Contains(string(ov), `["workshop-777111"]`) || !strings.Contains(string(ov), "enabled = true") {
			t.Errorf("%s 的覆盖文件没写对:\n%s", shard, ov)
		}
	}

	// 待下载那一条也要带"开着它的世界"：否则点完订阅并开启，界面上只剩一个"待下载"，
	// 用户没法确认开关到底写没写成。
	var mv modsView
	rec = do(t, h, http.MethodGet, "/api/v1/mods", "admin", "secret")
	decodeData(t, rec, &mv)
	if len(mv.PendingDownload) != 1 {
		t.Fatalf("该有一条待下载: %+v", mv.PendingDownload)
	}
	if len(mv.PendingDownload[0].UsedBy) != 2 {
		t.Errorf("待下载那条要说清开在哪几个世界，实际 %+v", mv.PendingDownload[0])
	}

	// 再点一次：清单不重复写行，但这一句要说明白，不能让人以为什么都没发生。
	rec = doJSON(t, h, http.MethodPost, "/api/v1/mods/subscribe-and-open",
		subscribeOpenRequest{WorkshopID: "777111", Cluster: "Cluster_1", Shards: []string{"Master", "Caves"}})
	decodeData(t, rec, &v)
	if v.Added {
		t.Errorf("重复订阅不该再添一行: %+v", v)
	}
	if !strings.Contains(v.Hint, "本来就有这一行") {
		t.Errorf("重复订阅要说明幂等，实际 %q", v.Hint)
	}
	text, _ = os.ReadFile(f.paths.ServerModSetup())
	if strings.Count(string(text), "777111") != 1 {
		t.Errorf("清单被写重复了:\n%s", text)
	}

	entries, err := f.srv.Store.ListAudit(50, 0, "mod.subscribe_open")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("两次点击都该留审计，实际 %d 条", len(entries))
	}
}

func TestSubscribeAndOpenRejectsBadScope(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()

	// 注入形态的 ID 走的是同一条校验：清单那一行只能有数字。
	rec := doJSON(t, h, http.MethodPost, "/api/v1/mods/subscribe-and-open",
		subscribeOpenRequest{WorkshopID: `1); os.execute("rm -rf /") //`, Cluster: "Cluster_1", Shards: []string{"Master"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 id 该 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(f.paths.ServerModSetup()); err == nil {
		text, _ := os.ReadFile(f.paths.ServerModSetup())
		if strings.Contains(string(text), "os.execute") {
			t.Fatalf("被拒的内容进了清单:\n%s", text)
		}
	}

	// 没有的世界：既不能凭空建目录，也不能写出一个没人读的覆盖条目。
	rec = doJSON(t, h, http.MethodPost, "/api/v1/mods/subscribe-and-open",
		subscribeOpenRequest{WorkshopID: "123456", Cluster: "Cluster_1", Shards: []string{"NoSuchWorld"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的世界该 404，实际 %d %s", rec.Code, rec.Body.String())
	}
	if msg := readErr(t, rec); !strings.Contains(msg, "现有") {
		t.Errorf("要说这一间到底有哪些世界，实际 %q", msg)
	}
	if _, err := os.Stat(f.paths.ModOverrides("Cluster_1", "NoSuchWorld")); err == nil {
		t.Error("被拒的请求写出了新世界的覆盖文件")
	}

	// 一个世界都不给：这一步的语义就不成立了，只能回订阅那一条接口。
	rec = doJSON(t, h, http.MethodPost, "/api/v1/mods/subscribe-and-open",
		subscribeOpenRequest{WorkshopID: "123456", Cluster: "Cluster_1"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("没有世界该 400，实际 %d", rec.Code)
	}
	// 房间不存在。
	rec = doJSON(t, h, http.MethodPost, "/api/v1/mods/subscribe-and-open",
		subscribeOpenRequest{WorkshopID: "123456", Cluster: "Cluster_9", Shards: []string{"Master"}})
	if rec.Code != http.StatusNotFound {
		t.Errorf("没有这个房间该 404，实际 %d", rec.Code)
	}
}

// TestEnablePendingMod 挡住一个具体的坑：订阅并开启之后文件还没下来，
// 这时候第 3 步那一行的开关必须还能按——不能因为 mods/ 里还没有目录就 404，
// 那等于给用户一个按下去没反应的按钮。
func TestEnablePendingMod(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	on, off := true, false

	rec := doJSON(t, h, http.MethodPost, "/api/v1/mods/setup", setupAddRequest{WorkshopID: "556677"})
	if rec.Code != http.StatusOK {
		t.Fatalf("前置订阅失败: %d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/workshop-556677",
		overridePatchRequest{Enabled: &on})
	if rec.Code != http.StatusOK {
		t.Fatalf("清单里有的模组该能开，实际 %d %s", rec.Code, rec.Body.String())
	}
	var sv shardModsView
	decodeData(t, rec, &sv)
	if len(sv.Mods) != 1 || !sv.Mods[0].Enabled || sv.Mods[0].OnDisk {
		t.Errorf("状态该是开着但文件不在：%+v", sv.Mods)
	}

	// 关掉同样要放行：文件不在的时候更需要能把那条目关掉。
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/workshop-556677",
		overridePatchRequest{Enabled: &off})
	if rec.Code != http.StatusOK {
		t.Errorf("关掉待下载的模组该 200，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 既不在磁盘上、又没订阅、覆盖文件里也没条目的，照旧要拒——那是凭空造键。
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/mods/workshop-998877",
		overridePatchRequest{Enabled: &on})
	if rec.Code != http.StatusNotFound {
		t.Errorf("没订阅过的工坊 ID 不该被凭空打开，实际 %d", rec.Code)
	}
	if msg := readErr(t, rec); !strings.Contains(msg, "订阅") {
		t.Errorf("404 要说下一步是订阅，实际 %q", msg)
	}
}

// 覆盖文件里有条目、磁盘上没目录、订阅清单里也没这一行的那种残留，界面上不能只剩 workshop-数字。
// 这一种通常是"当初从检索页订阅过、后来文件被删了"，面板缓存里就有它的标题。
// 摆一串编号让用户自己去核对，是他在 #78 里批过的事。
func TestShardModsNamesMissingDirFromCache(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost, "/api/v1/mods/subscribe-and-open",
		subscribeOpenRequest{WorkshopID: "556699", Cluster: "Cluster_1", Shards: []string{"Master"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("前置订阅并开启失败: %d %s", rec.Code, rec.Body.String())
	}
	// 取消订阅：现场变成"开关里有条目、磁盘没目录、清单里没这一行"。
	if rec = doJSON(t, h, http.MethodDelete, "/api/v1/mods/setup/556699", nil); rec.Code != http.StatusOK {
		t.Fatalf("前置取消订阅失败: %d %s", rec.Code, rec.Body.String())
	}
	seedCache(t, f, store.SeenMod{WorkshopID: "556699", Name: "缓存里认得出的名字",
		Origin: store.SeenFromWorkshop})

	rec = doJSON(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/mods", nil)
	var sv shardModsView
	decodeData(t, rec, &sv)
	var got *overrideView
	for i := range sv.Mods {
		if sv.Mods[i].Dir == "workshop-556699" {
			got = &sv.Mods[i]
		}
		if sv.Mods[i].OnDisk && sv.Mods[i].SeenName != "" {
			t.Errorf("%s 在磁盘上，名字要以 modinfo.lua 为准，不该带缓存标题：%q",
				sv.Mods[i].Dir, sv.Mods[i].SeenName)
		}
	}
	if got == nil {
		t.Fatalf("取消订阅以后这一条的开关条目应该还在（面板不替人删条目），实际 %+v", sv.Mods)
	}
	if got.OnDisk {
		t.Errorf("这条的现场该是磁盘上没目录，实际 %+v", *got)
	}
	if got.SeenName != "缓存里认得出的名字" {
		t.Errorf("缺目录那一条要把缓存里的标题带回去，实际 %q", got.SeenName)
	}
}

// putUgcCopy 造出"引擎自己把这个模组拉进了某个世界的下载区"的现场。
// 目录名就是那串工坊 ID（不带 workshop- 前缀），和 mods/ 下面的拼法不一样：
// 云机实测是 <dstInstall>/ugc_mods/<房间>/<世界>/content/322330/<id>。
func putUgcCopy(t *testing.T, f *apiFixture, cluster, shard, id, modinfo string) {
	t.Helper()
	full := f.paths.UgcModDir(cluster, shard, id)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "modinfo.lua"), []byte(modinfo), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 引擎把工坊内容下载到它自己的下载区，mods/ 下面不会多出目录（云机实测）。
// 面板以前只扫 mods/，结果"已经下载好、日志里明确 Loading mod 了"的模组在界面上报
// "文件还没下载"。这条测试钉两个口径，一高一低：
//   - 整台机器这一屏：下载区里那份也算有文件，名字版本要读得出来，不许再挂进待下载；
//   - 单个世界这一屏：只下给地表的模组，洞穴那一行必须说文件不在——
//     洞穴没重启过就没有这一份，开着也不会生效，说"在"就是假话。
func TestUGCModIsJudgedPerWorld(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	const id = "378160973"
	dir := "workshop-" + id

	rec := doJSON(t, h, http.MethodPost, "/api/v1/mods/subscribe-and-open",
		subscribeOpenRequest{WorkshopID: id, Cluster: "Cluster_1", Shards: []string{"Master", "Caves"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("前置订阅并开启失败: %d %s", rec.Code, rec.Body.String())
	}
	putUgcCopy(t, f, "Cluster_1", "Master", id, `return { name = "Global Positions", version = "1.7.6" }`)

	rec = do(t, h, http.MethodGet, "/api/v1/mods", "admin", "secret")
	var mv modsView
	decodeData(t, rec, &mv)
	var got *modView
	for i := range mv.Mods {
		if mv.Mods[i].Dir == dir {
			got = &mv.Mods[i]
		}
	}
	if got == nil {
		t.Fatalf("引擎下载区里的模组没被列出来: %+v", mv.Mods)
	}
	if got.Name != "Global Positions" || got.Version != "1.7.6" {
		t.Errorf("下载区那一份的 modinfo.lua 也要读出名字和版本: %+v", *got)
	}
	if !got.DownloadedByEngine {
		t.Errorf("这一行得说清文件是从引擎的下载区来的: %+v", *got)
	}
	if !got.InSetupList {
		t.Errorf("订阅清单里有它才对: %+v", *got)
	}
	if len(mv.PendingDownload) != 0 {
		t.Errorf("文件已经在这台机器上了，不该再报待下载: %+v", mv.PendingDownload)
	}

	for _, tc := range []struct {
		shard string
		want  bool
	}{
		{"Master", true}, // 重启过的那一间：下载区里有这一份
		{"Caves", false}, // 还没重启的那一间：开着也不会生效
	} {
		rec = do(t, h, http.MethodGet,
			"/api/v1/clusters/Cluster_1/shards/"+tc.shard+"/mods", "admin", "secret")
		var sv shardModsView
		decodeData(t, rec, &sv)
		var found *overrideView
		for i := range sv.Mods {
			if sv.Mods[i].Dir == dir {
				found = &sv.Mods[i]
			}
		}
		if found == nil {
			t.Fatalf("%s 的覆盖文件里应该有这一条: %+v", tc.shard, sv.Mods)
		}
		if found.Enabled != true {
			t.Errorf("%s 这一条该是开着的状态: %+v", tc.shard, *found)
		}
		if found.OnDisk != tc.want {
			t.Errorf("%s 的文件在不在要按这个世界判，期望 %v 实际 %v（%+v）",
				tc.shard, tc.want, found.OnDisk, *found)
		}
	}
}
