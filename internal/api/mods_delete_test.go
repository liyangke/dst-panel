package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/dst"
)

// 删除已下载的模组。这一条要验的不是"能不能删掉一个目录"，
// 而是删完之后那三处痕迹是不是一起干净了——只删文件的话，清单会把它下回来、
// 房间会带一条指向不存在目录的启用项，用户看到的都是"我明明删了"。

func shardModEnabled(t *testing.T, h http.Handler, cluster, shard, dir string) bool {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/api/v1/clusters/"+cluster+"/shards/"+shard+"/mods", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("读 %s/%s 的模组列表 = %d：%s", cluster, shard, rec.Code, rec.Body.String())
	}
	var v shardModsView
	decodeData(t, rec, &v)
	for _, o := range v.Mods {
		if o.Dir == dir {
			return o.Enabled
		}
	}
	return false
}

func TestModFileDeleteTakesFilesSetupAndSwitchesTogether(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	const id = "378545146"
	dir := "workshop-" + id
	// 造出用户点删除时的真实前提：清单里有、文件在磁盘上、地表还开着它。
	if _, err := f.srv.modsManager().AddSetup(id); err != nil {
		t.Fatal(err)
	}
	putModDir(t, f, dir, `return { name = "木牌传送" }`)
	enableMod(t, h, "Cluster_1", "Master", dir, nil)
	if !shardModEnabled(t, h, "Cluster_1", "Master", dir) {
		t.Fatal("前提没造出来：地表这个模组没开着")
	}

	rec := do(t, h, http.MethodDelete, "/api/v1/mods/files/"+dir, "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应该成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var v modsView
	decodeData(t, rec, &v)
	if _, err := os.Stat(filepath.Join(f.paths.ModsRoot(), dir)); !os.IsNotExist(err) {
		t.Errorf("目录还在：%v", err)
	}
	text, err := os.ReadFile(f.paths.ServerModSetup())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), id) {
		t.Errorf("下载清单里还留着那一行，下次重启会把它重新下载回来：\n%s", text)
	}
	if shardModEnabled(t, h, "Cluster_1", "Master", dir) {
		t.Error("地表那个世界的启用开关没关掉：文件没了还留着一条开了的条目")
	}
	// 洞穴本来就没开它，不该被面板写出一条新条目来。
	if shardModEnabled(t, h, "Cluster_1", "Caves", dir) {
		t.Error("洞穴原本没开这个模组，删除这一步不该把它写成开着")
	}
	for _, want := range []string{"删掉", "腾出", "清单", "Cluster_1 / Master"} {
		if !strings.Contains(v.Hint, want) {
			t.Errorf("Hint 要说到 %s（这次到底动了哪几处），实际 %q", want, v.Hint)
		}
	}
	entries, err := f.srv.Store.ListAudit(50, 0, "mod.files.")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Error("删除模组目录是不可逆操作，必须进平台审计")
	}
}

func TestModFileDeleteRefusesWhileTheWorldIsRunning(t *testing.T) {
	// 假 screen 输出里让地表在跑：正在被引擎用着的文件不该被中途抽走。
	f := newFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	dir := "workshop-378545146"
	putModDir(t, f, dir, `return { name = "木牌传送" }`)
	enableMod(t, h, "Cluster_1", "Master", dir, nil)

	rec := do(t, h, http.MethodDelete, "/api/v1/mods/files/"+dir, "admin", "secret")
	if rec.Code != http.StatusConflict {
		t.Fatalf("世界在跑时应该 409，实际 %d：%s", rec.Code, rec.Body.String())
	}
	msg := readErr(t, rec)
	if !strings.Contains(msg, "停掉") || !strings.Contains(msg, "Cluster_1 / Master") {
		t.Errorf("要说出该停掉哪一间，实际 %q", msg)
	}
	if !strings.Contains(msg, "玩家") {
		t.Errorf("要说清为什么不替他停服（会踢掉正在玩的玩家），实际 %q", msg)
	}
	if _, err := os.Stat(filepath.Join(f.paths.ModsRoot(), dir)); err != nil {
		t.Errorf("被拒的删除必须一点都没动手：%v", err)
	}
}

func TestModFileDeleteRefusesWhatMustSurvive(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	putModDir(t, f, dst.HelperModName, `return { name = "面板助手" }`)
	putModDir(t, f, "MyLocalMod", `return { name = "本地模组" }`)

	for _, tc := range []struct{ path, dir, want string }{
		// 助手模组是面板读世界状态的那双眼睛，删掉它等于让用户自己把面板弄瞎。
		{dst.HelperModName, dst.HelperModName, "助手"},
		{"workshop-999999999", "workshop-999999999", "不存在"},
		{".hidden", ".hidden", "目录名"},
	} {
		rec := do(t, h, http.MethodDelete, "/api/v1/mods/files/"+tc.path, "admin", "secret")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应该 400，实际 %d：%s", tc.path, rec.Code, rec.Body.String())
			continue
		}
		if msg := readErr(t, rec); !strings.Contains(msg, tc.want) {
			t.Errorf("%s 的错误要提到 %s，实际 %q", tc.path, tc.want, msg)
		}
	}
	// 没被删的那个本地模组必须还在，别因为一次失败的请求牵连到旁边。
	if _, err := os.Stat(filepath.Join(f.paths.ModsRoot(), "MyLocalMod")); err != nil {
		t.Errorf("旁边的模组被牵连了：%v", err)
	}
	// 未登录不能删。
	if rec := do(t, h, http.MethodDelete, "/api/v1/mods/files/MyLocalMod", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("删除要认证，实际 %d", rec.Code)
	}
}

// 本地上传的模组没有工坊 ID，删它就只有两处痕迹：文件 + 各世界的开关。
func TestModFileDeleteLocalModKeepsSetupFileUntouched(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	if _, err := f.srv.modsManager().AddSetup("111222333"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.paths.ServerModSetup())
	if err != nil {
		t.Fatal(err)
	}
	putModDir(t, f, "MyLocalMod", `return { name = "本地模组" }`)

	rec := do(t, h, http.MethodDelete, "/api/v1/mods/files/MyLocalMod", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除本地模组应该成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(f.paths.ServerModSetup())
	if string(after) != string(before) {
		t.Errorf("删本地模组不该动下载清单：\n%s", after)
	}
	var v modsView
	decodeData(t, rec, &v)
	if strings.Contains(v.Hint, "清单") {
		t.Errorf("这一趟没碰清单，Hint 里就不该提它：%q", v.Hint)
	}
}

// keepSetup=1 是同一段代码的第二种语义："只要文件没了，订阅和开关都别动"。
// 这一条对应的是"工坊更新了、这台机器上还是旧的那份"：删掉旧文件逼它下次启动重下，
// 但绝不撤订阅——撤了就没有东西会被下载回来。
func TestModFileDeleteKeepSetupLeavesSubscriptionAndSwitch(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	const id = "378545146"
	dir := "workshop-" + id
	if _, err := f.srv.modsManager().AddSetup(id); err != nil {
		t.Fatal(err)
	}
	putModDir(t, f, dir, `return { name = "木牌传送" }`)
	enableMod(t, h, "Cluster_1", "Master", dir, nil)

	rec := do(t, h, http.MethodDelete, "/api/v1/mods/files/"+dir+"?keepSetup=1", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("保留订阅的删除应该成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(f.paths.ModsRoot(), dir)); !os.IsNotExist(err) {
		t.Errorf("文件没删掉：%v", err)
	}
	text, err := os.ReadFile(f.paths.ServerModSetup())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), `ServerModSetup("`+id+`")`) {
		t.Errorf("清单那一行必须留着，否则下次重启没有东西会被重新下载：\n%s", text)
	}
	if !shardModEnabled(t, h, "Cluster_1", "Master", dir) {
		t.Error("这是重下不是卸载，启用开关不该被关掉")
	}
	var v modsView
	decodeData(t, rec, &v)
	for _, want := range []string{"重新下载", "启用没动"} {
		if !strings.Contains(v.Hint, want) {
			t.Errorf("Hint 要说到 %s（下一次启动会发生什么），实际 %q", want, v.Hint)
		}
	}
}

// 删除按钮的确认框要当场说出"点下去会关掉哪几间的开关"，那句只能接口带回来。
// 前端不许自己猜，所以这一条钉的是 modView.usedBy 的口径：只算 enabled 的那些条目。
func TestModsViewSaysWhichRoomsUseAMod(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	putModDir(t, f, "ModUsed", `return { name = "有人在用" }`)
	putModDir(t, f, "ModIdle", `return { name = "没人开" }`)
	enableMod(t, h, "Cluster_1", "Master", "ModUsed", nil)
	// 开着但关掉的条目同样不算"在用"。
	caves := "/api/v1/clusters/Cluster_1/shards/Caves/mods/ModUsed"
	on := true
	if rec := doJSON(t, h, http.MethodPut, caves, overridePatchRequest{Enabled: &on}); rec.Code != http.StatusOK {
		t.Fatalf("先在洞穴开起来：%d %s", rec.Code, rec.Body.String())
	}
	off := false
	if rec := doJSON(t, h, http.MethodPut, caves, overridePatchRequest{Enabled: &off}); rec.Code != http.StatusOK {
		t.Fatalf("再在洞穴关掉：%d %s", rec.Code, rec.Body.String())
	}

	rec := do(t, h, http.MethodGet, "/api/v1/mods", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", rec.Code, rec.Body.String())
	}
	var v modsView
	decodeData(t, rec, &v)
	got := map[string][]string{}
	for _, m := range v.Mods {
		got[m.Dir] = m.UsedBy
	}
	if len(got["ModUsed"]) != 1 || got["ModUsed"][0] != "Cluster_1 / Master" {
		t.Errorf("ModUsed 的在用世界应该是地表一间，实际 %+v", got["ModUsed"])
	}
	if len(got["ModIdle"]) != 0 {
		t.Errorf("没人开的模组不该报出在用世界：%+v", got["ModIdle"])
	}
	// 助手模组在这一屏是"面板自带"，界面靠这个字段决定不给删除按钮。
	var helper *modView
	for i := range v.Mods {
		if v.Mods[i].Dir == dst.HelperModName {
			helper = &v.Mods[i]
		}
	}
	if helper != nil && !helper.Builtin {
		t.Error("自家助手模组要标成面板自带")
	}
}

// "删文件重下"对引擎下载区里的那份同样得成立，而且一个模组在每个世界各有一份：
// 只删 mods/ 的话，下次启动服务端读的还是下载区里那份旧的，用户等到的是一个没变的模组。
// 这句删除后的说明还要报出真的份数和真的位置——界面上的数字必须对得上实际删掉的东西。
func TestModFileDeleteClearsEnginesDownloadArea(t *testing.T) {
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
	putUgcCopy(t, f, "Cluster_1", "Caves", id, `return { name = "Global Positions", version = "1.7.6" }`)

	rec = do(t, h, http.MethodDelete, "/api/v1/mods/files/"+dir+"?keepSetup=1", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应该成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var v modsView
	decodeData(t, rec, &v)
	if !strings.Contains(v.Hint, "2 份") {
		t.Errorf("要点明删掉了两份（每个世界一份），实际 %q", v.Hint)
	}
	for _, want := range []string{"Cluster_1 / Master", "Cluster_1 / Caves"} {
		if !strings.Contains(v.Hint, want) {
			t.Errorf("说明里要列出删的是哪几个世界，缺 %q：%q", want, v.Hint)
		}
	}
	for _, shard := range []string{"Master", "Caves"} {
		if _, err := os.Stat(f.paths.UgcModDir("Cluster_1", shard, id)); !os.IsNotExist(err) {
			t.Errorf("%s 的下载区那一份没删掉：%v", shard, err)
		}
	}
	// 订阅清单留着，所以这一屏该回到"待下载"：下次启动会重新拉回来。
	if len(v.PendingDownload) != 1 || v.PendingDownload[0].WorkshopID != id {
		t.Errorf("删完该只剩待下载那一条: %+v", v.PendingDownload)
	}
	if len(v.PendingDownload) == 1 && len(v.PendingDownload[0].UsedBy) != 2 {
		t.Errorf("待下载那条还要说清两个世界的开关都开着: %+v", v.PendingDownload[0])
	}
}

// 手工在 mods/ 放过一份、引擎又自己下过一份的那种，说明不能只报一处——
// 少报的那一半还留在盘上，用户以为清干净了。
func TestModFileDeleteReportsBothPlaces(t *testing.T) {
	f := newFixture(t, "")
	ensureModsDir(t, f)
	h := f.srv.Handler()
	const id = "444555666"
	dir := "workshop-" + id
	if _, err := f.srv.modsManager().AddSetup(id); err != nil {
		t.Fatal(err)
	}
	putModDir(t, f, dir, `return { name = "两处都有" }`)
	putUgcCopy(t, f, "Cluster_1", "Master", id, `return { name = "两处都有" }`)

	rec := do(t, h, http.MethodDelete, "/api/v1/mods/files/"+dir, "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应该成功，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var v modsView
	decodeData(t, rec, &v)
	if !strings.Contains(v.Hint, "mods/"+dir) || !strings.Contains(v.Hint, "1 份") {
		t.Errorf("两处都删掉了，两处都要说出口：%q", v.Hint)
	}
	if _, err := os.Stat(filepath.Join(f.paths.ModsRoot(), dir)); !os.IsNotExist(err) {
		t.Errorf("mods/ 那一份没删掉：%v", err)
	}
	if _, err := os.Stat(f.paths.UgcModDir("Cluster_1", "Master", id)); !os.IsNotExist(err) {
		t.Errorf("下载区那一份没删掉：%v", err)
	}
	// 不带 keepSetup 的删除是卸载：清单那一行也要撤掉，否则下次启动它又会自己回来。
	text, err := os.ReadFile(f.paths.ServerModSetup())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), id) {
		t.Errorf("订阅清单里还留着那一行：\n%s", text)
	}
}
