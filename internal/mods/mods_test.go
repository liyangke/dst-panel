package mods

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/dst"
)

func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	m := &Manager{Paths: dst.Paths{KleiRoot: filepath.Join(root, "klei"), DstInstall: filepath.Join(root, "dst")}}
	if err := os.MkdirAll(m.Paths.ModsRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	return m, root
}

// writeModinfo 造一个模组目录。
func writeModinfo(t *testing.T, m *Manager, dir, body string) {
	t.Helper()
	full := filepath.Join(m.Paths.ModsRoot(), dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "modinfo.lua"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const kleiSetupFile = `--There are two functions that will install mods, ServerModSetup and ServerModCollectionSetup. Put the calls to the functions in this file and they will be executed on boot.

--ServerModSetup takes a string of a specific mod's Workshop id. It will download and install the mod to your mod directory on boot.
	--Example: http://steamcommunity.com/sharedfiles/filedetails/?id=350811795
	--ServerModSetup("350811795")

`

func TestListSeparatesWorkshopLocalAndBroken(t *testing.T) {
	m, _ := newManager(t)
	// modinfo.lua 的真实形状是一串裸赋值（引擎 kleiloadlua 后从 env 里取字段），
	// 不是 return 一张表。 fixture 按引擎认的那种写法来。
	writeModinfo(t, m, "workshop-350811795", `name = "几何防御"
version = "1.4"
api_version = 10
forum_id = "123"
`)
	writeModinfo(t, m, "MyLocalMod", `name = "自制模组"
version = 20
api_version = 3.0
`)
	// 名字是代码算出来的（多语言模组都这么写）：文件能读，但面板不执行 Lua，
	// 这种情况要留一句解释，不能只给一个空白名字。
	writeModinfo(t, m, "Computed", `local function hello() return 1 end
name = hello()
version = "9"
`)
	// 真的读坏了（花括号没闭合）：标记为解析失败，但其他模组照常被列出。
	writeModinfo(t, m, "Broken", `name = "坏文件"
settings = { { name = "x" }
`)
	// 缺 modinfo.lua 的目录：引擎根本不会认它，但目录确实在那儿。
	if err := os.MkdirAll(filepath.Join(m.Paths.ModsRoot(), "EmptyDir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 散文件不是模组，不该出现在列表里。
	if err := os.WriteFile(filepath.Join(m.Paths.ModsRoot(), "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("应该列出 5 个目录模组，实际 %+v", list)
	}
	byDir := map[string]Mod{}
	for _, x := range list {
		byDir[x.Dir] = x
	}
	w := byDir["workshop-350811795"]
	if !w.Workshop || w.WorkshopID != "350811795" || w.Name != "几何防御" || w.Version != "1.4" || !w.ModinfoOK {
		t.Errorf("工坊模组信息不对: %+v", w)
	}
	// version 写成数字时也要能显示，第三方模组里 version = 2 这种很常见。
	if byDir["MyLocalMod"].Version != "20" {
		t.Errorf("数字 version 没转好: %+v", byDir["MyLocalMod"])
	}
	c := byDir["Computed"]
	if !c.ModinfoOK || c.Version != "9" || c.Name != "" || !strings.Contains(c.Modinfo, "不执行 Lua") {
		t.Errorf("名字算出来的模组应该照常可读并留一句说明: %+v", c)
	}
	if b := byDir["Broken"]; b.ModinfoOK || b.Modinfo == "" {
		t.Errorf("结构读坏了要留原因，实际 %+v", b)
	}
	if byDir["EmptyDir"].ModinfoOK {
		t.Error("没有 modinfo.lua 却报解析成功")
	}
	for _, x := range list {
		if x.Dir == "stray.txt" {
			t.Error("散文件被当成模组列出来了")
		}
	}
}

// TestListReadsOurOwnShippedHelper 用仓库里那份真文件（面板助手 MOD 的 modinfo.lua）
// 回归：它在云机上被引擎正常加载，面板却一度读不出它的中文名字。
func TestListReadsOurOwnShippedHelper(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "modbridge", "mods", "DSTPanelHelper", "modinfo.lua"))
	if err != nil {
		t.Fatalf("助手 modinfo.lua 读不到（这个文件是面板的一部分，不该缺）: %v", err)
	}
	m, _ := newManager(t)
	writeModinfo(t, m, "DSTPanelHelper", string(raw))
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应该只有助手一个模组，实际 %+v", list)
	}
	x := list[0]
	if !x.ModinfoOK || x.Name != "DST 管理面板助手" || x.Version != "1.0.0" {
		t.Errorf("自家助手的 modinfo 必须完整读出来: %+v", x)
	}
	// 助手的 description 是 [[ ]] 长字符串，跨三行。检索页默认列表在没配 Steam key 时
	// 只能靠 modinfo.lua 给出信息，这份真文件读不出说明就等于界面只剩名字。
	if !strings.Contains(x.Description, "结构化运行数据") || !strings.Contains(x.Description, "卸载后存档不受影响") {
		t.Errorf("助手的长字符串说明没读全: %q", x.Description)
	}
	if x.ClientOnly || x.AllClientsRequire {
		t.Errorf("助手是服务端专用，两个客户端标记都该是 false: %+v", x)
	}
	// settings 里 values = POLL_CHOICES 是同一文件里前面定义过的那张候选表：
	// 名字到表的对应关系由上往下已经读到了，接住它是把已有的数据用起来（不是求值），
	// 抽屉的下拉框就靠这份字典渲染。
	tbl, err := dst.ParseLuaAssignments(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	item, ok := tbl.Nested("settings")
	if !ok || item.Len() != 2 {
		t.Fatalf("settings 应该读到 2 项，实际 %+v", item)
	}
	first := item.Values()[0].T
	if first == nil || first.Get("default").AsNumber(0) != 1 {
		t.Errorf("settings 第一项读得不对: %+v", first)
	}
	ref := first.Get("values").T
	if ref == nil || len(ref.Values()) != 4 {
		t.Fatalf("settings 第一项的 values 应该接住 POLL_CHOICES 那 4 档，实际 %+v", ref)
	}
	c0 := ref.Values()[0].T
	if c0 == nil || c0.Get("description").AsString("") != "每 1 秒" || c0.Get("data").AsNumber(0) != 1 {
		t.Errorf("候选档位内容不对: %+v", c0)
	}
}

// TestListReadsDescriptionAndTags 盯的是检索页的信息量：没配 Steam key 时这一页能拿到的
// 只有磁盘上的 modinfo.lua，说明和标签读不出来，每一行就只剩一个名字。
// tags 在第三方模组里有两种写法，都得接住。
func TestListReadsDescriptionAndTags(t *testing.T) {
	m, _ := newManager(t)
	// 数组写法，并且故意重复：标签是直接给用户看的词，重复的显示两遍很吵。
	writeModinfo(t, m, "workshop-101", `name = "数组标签"
description = "一行说明"
tags = { "QoL", "Gameplay", "QoL" }
`)
	// 键表写法：tags = { MapInfo = true }。这种时候键名自己就是要展示的词。
	// 同一条里再放一个跨行的 [[ ]] 说明。
	writeModinfo(t, m, "workshop-102", `name = "键表标签"
description = [[
第一段说明
第二段说明
]]
tags = { UtilityMod = true, MapInfo = true }
`)
	writeModinfo(t, m, "workshop-103", `name = "什么都没写"
`)
	writeModinfo(t, m, "workshop-104", `name = "空标签表"
tags = { }
`)

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	byDir := map[string]Mod{}
	for _, x := range list {
		byDir[x.Dir] = x
	}

	if x := byDir["workshop-101"]; x.Description != "一行说明" || strings.Join(x.Tags, ",") != "QoL,Gameplay" {
		t.Errorf("数组写法读歪了: %+v", x)
	}
	x2 := byDir["workshop-102"]
	if !strings.Contains(x2.Description, "第一段说明") || !strings.Contains(x2.Description, "第二段说明") {
		t.Errorf("长字符串说明没读全: %q", x2.Description)
	}
	if strings.Join(x2.Tags, ",") != "MapInfo,UtilityMod" {
		t.Errorf("键表标签应该按键名收（并排序），实际 %v", x2.Tags)
	}
	// 缺字段不是错误：说明留空、标签为空列表，界面自己决定隐藏哪一块。
	if x3 := byDir["workshop-103"]; !x3.ModinfoOK || x3.Description != "" || len(x3.Tags) != 0 {
		t.Errorf("什么都没写的模组不该被误判: %+v", x3)
	}
	if x4 := byDir["workshop-104"]; len(x4.Tags) != 0 || x4.Description != "" {
		t.Errorf("空标签表读成了: %+v", x4)
	}
}

// TestLuaTextNumbers 钉住数字格式化：整数不能带小数点，也不能被裁掉尾零。
func TestLuaTextNumbers(t *testing.T) {
	tbl, err := dst.ParseLuaTable(`return { a = 20, b = 2, c = 1.5, d = 3.0, e = "1.4", f = -7 }`)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"a": "20", "b": "2", "c": "1.5", "d": "3", "e": "1.4", "f": "-7"}
	for k, want := range cases {
		if got := luaText(tbl, k); got != want {
			t.Errorf("luaText(%s) = %q，期望 %q", k, got, want)
		}
	}
	if got := luaText(tbl, "missing"); got != "" {
		t.Errorf("缺键应该返回空串，实际 %q", got)
	}
}

func TestSetupEntriesOnKleiDefaultFile(t *testing.T) {
	m, _ := newManager(t)
	// 用 Klei 原版那份注释满布的文件测：里面的示例行是 "--ServerModSetup(...)"，必须不算登记。
	if err := os.WriteFile(m.Paths.ServerModSetup(), []byte(kleiSetupFile), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := m.SetupEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("注释里的示例不能算登记，实际 %+v", ids)
	}
}

func TestAddSetupIsIdempotentAndKeepsComments(t *testing.T) {
	m, _ := newManager(t)
	if err := os.WriteFile(m.Paths.ServerModSetup(), []byte(kleiSetupFile), 0o644); err != nil {
		t.Fatal(err)
	}
	added, err := m.AddSetup("666111")
	if err != nil || !added {
		t.Fatalf("第一次登记应该成功: %v %v", added, err)
	}
	added, err = m.AddSetup("666111")
	if err != nil || added {
		t.Errorf("重复登记不该再写文件: %v %v", added, err)
	}
	text, _ := os.ReadFile(m.Paths.ServerModSetup())
	if !strings.Contains(string(text), `ServerModSetup("666111")`) {
		t.Fatalf("没写进清单:\n%s", text)
	}
	if !strings.Contains(string(text), "There are two functions") {
		t.Error("Klei 的说明注释被弄丢了")
	}
	if strings.Count(string(text), "666111") != 1 {
		t.Errorf("重复登记产生了多余行:\n%s", text)
	}
	// 备份文件是原子写的产物，说明没有直接原地覆盖。
	if _, err := os.Stat(m.Paths.ServerModSetup() + ".bak"); err != nil {
		t.Errorf("覆盖前没留 .bak: %v", err)
	}
}

func TestAddSetupCreatesMissingFileAndRejectsInjection(t *testing.T) {
	m, _ := newManager(t)
	// 新装的服务端可能压根没有这个文件。
	added, err := m.AddSetup("123456789")
	if err != nil || !added {
		t.Fatalf("文件不存在时也要能登记: %v %v", added, err)
	}
	text, _ := os.ReadFile(m.Paths.ServerModSetup())
	if strings.TrimSpace(string(text)) != `ServerModSetup("123456789")` {
		t.Errorf("新建内容不该带别的东西: %q", text)
	}

	// 这个文件是引擎执行的，所以 id 只收数字。
	for _, bad := range []string{"", "  ", "1); os.execute('rm -rf /') //", "abc", "123456789012345678901234", "-1", "1e9"} {
		if _, err := m.AddSetup(bad); err == nil {
			t.Errorf("id %q 竟然被接受了", bad)
		}
	}
	after, _ := os.ReadFile(m.Paths.ServerModSetup())
	if string(after) != string(text) {
		t.Errorf("被拒的 id 改了文件:\n%s", after)
	}
}

func TestRemoveSetupOnlyTouchesThatID(t *testing.T) {
	m, _ := newManager(t)
	body := kleiSetupFile + "\nServerModSetup(\"111\")\n\tServerModCollectionSetup(\"222\")\nServerModSetup ( '333' );\n"
	if err := os.WriteFile(m.Paths.ServerModSetup(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := m.SetupEntries()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "111,222,333" {
		t.Fatalf("三种写法都该认出来，实际 %+v", ids)
	}
	n, err := m.RemoveSetup("333")
	if err != nil || n != 1 {
		t.Fatalf("应该删掉 1 行: %v %v", n, err)
	}
	text, _ := os.ReadFile(m.Paths.ServerModSetup())
	if strings.Contains(string(text), "333") {
		t.Errorf("333 没删干净:\n%s", text)
	}
	if !strings.Contains(string(text), `ServerModSetup("111")`) ||
		!strings.Contains(string(text), `ServerModCollectionSetup("222")`) {
		t.Errorf("别人的行被误删:\n%s", text)
	}
	if !strings.Contains(string(text), "There are two functions") {
		t.Error("注释被误删")
	}
	if n, err := m.RemoveSetup("999"); err != nil || n != 0 {
		t.Errorf("没登记过的 id 该返回 0: %v %v", n, err)
	}
}

func TestSetupKeepsOriginalEOL(t *testing.T) {
	m, _ := newManager(t)
	crlf := strings.ReplaceAll(kleiSetupFile, "\n", "\r\n")
	if err := os.WriteFile(m.Paths.ServerModSetup(), []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddSetup("444"); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(m.Paths.ServerModSetup())
	if strings.Count(string(text), "\r\n") != strings.Count(string(text), "\n") {
		t.Errorf("CRLF 文件里混进了 LF，用户的 diff 会整片变红:\n%q", text)
	}
}

func TestDownloaded(t *testing.T) {
	m, _ := newManager(t)
	if ok, _ := m.Downloaded("555"); ok {
		t.Error("目录还不存在却报已下载")
	}
	writeModinfo(t, m, "workshop-555", `return { name = "x" }`)
	ok, dir := m.Downloaded("555")
	if !ok || !strings.HasSuffix(dir, filepath.Join("mods", "workshop-555")) {
		t.Errorf("已下载的模组要给出路径: %v %s", ok, dir)
	}
	// 非法 id 不能顺着路径拼接跑到别处去。
	if ok, _ := m.Downloaded("../klei"); ok {
		t.Error("越界 id 竟然判成已下载")
	}
	// 引擎自己拉的那一份不在 mods/ 下面，只认 mods/ 就会把已经加载好的模组说成"还没下载"。
	writeUgcModinfo(t, m, "Cluster_1", "Master", "378160973", `name = "Global Positions"
version = "1.7.6"
`)
	ok, dir = m.Downloaded("378160973")
	if !ok {
		t.Fatal("下载区里有这一条，必须算已下载")
	}
	if !strings.Contains(dir, filepath.Join("ugc_mods", "Cluster_1", "Master")) {
		t.Errorf("给出的路径要落在引擎下载区里: %s", dir)
	}
	// 整台机器有 ≠ 这个世界有：洞穴那一份还没下来，分片那一屏要说没。
	if !m.DownloadedIn("Cluster_1", "Master", "378160973") {
		t.Error("地表下载区里明明有")
	}
	if m.DownloadedIn("Cluster_1", "Caves", "378160973") {
		t.Error("洞穴还没有这一份，不能算有")
	}
	if m.DownloadedIn("Cluster_1", "Master", "../378160973") {
		t.Error("非法 id 不该判成已下载")
	}
}

// 云机实测（2026-11-03）：引擎按订阅清单把工坊内容拉到 ugc_mods/<房间>/<世界>/content/322330/<id>，
// 日志里紧跟着 Loading mod: workshop-<id>。面板如果只扫 mods/，这个模组在界面上就永远是
// "已订阅但还没下载"，而它其实正在跑。
func TestListSeesTheEnginesOwnDownloadArea(t *testing.T) {
	m, _ := newManager(t)
	if _, err := m.AddSetup("378160973"); err != nil {
		t.Fatal(err)
	}
	writeModinfo(t, m, dst.HelperModName, `name = "面板助手"`)
	// 同一个模组在两个世界各一份（引擎就是这么存的），列表里要归成一条。
	writeUgcModinfo(t, m, "Cluster_1", "Master", "378160973", `name = "Global Positions"
version = "1.7.6"
`)
	writeUgcModinfo(t, m, "Cluster_1", "Caves", "378160973", `name = "Global Positions"
version = "1.7.6"
`)
	// 没登记的下载区条目也要列出来：它确确实实占着这台机器的磁盘。
	writeUgcModinfo(t, m, "Cluster_2", "Master", "999", `name = "别处下的"`)
	// 台账式临时目录与手工备份不是就位的内容，不该变成模组条目。
	for _, junk := range []string{"378160973.download", "abc"} {
		if err := os.MkdirAll(filepath.Join(m.Paths.UgcContentDir("Cluster_1", "Master"), junk), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	byDir := map[string]Mod{}
	for _, x := range list {
		byDir[x.Dir] = x
	}
	if len(list) != 3 {
		t.Fatalf("应该列出助手 + 两个工坊条目（同一个模组在两个世界各一份，要归成一条），实际 %+v", list)
	}
	g := byDir["workshop-378160973"]
	if !g.Workshop || g.WorkshopID != "378160973" || g.Name != "Global Positions" ||
		g.Version != "1.7.6" || !g.ModinfoOK || !g.InSetupList || !g.FromUGC {
		t.Errorf("下载区里那份要读得懂并说清来路: %+v", g)
	}
	if !byDir["workshop-999"].FromUGC || byDir["workshop-999"].InSetupList {
		t.Errorf("没登记的下载区条目照样要列出来，但不能算已订阅: %+v", byDir["workshop-999"])
	}
	if byDir[dst.HelperModName].FromUGC {
		t.Errorf("mods/ 下面那一份不该被标成下载区来的: %+v", byDir[dst.HelperModName])
	}

	// 两处都有同一串 ID 时以 mods/ 里那份为准：那是 MODS_ROOT 直接指向的位置。
	writeModinfo(t, m, "workshop-900", `name = "手工放的那份"
version = "0.1"
`)
	writeUgcModinfo(t, m, "Cluster_1", "Master", "900", `name = "引擎拉的那份"
version = "0.9"
`)
	list, err = m.List()
	if err != nil {
		t.Fatal(err)
	}
	byDir = map[string]Mod{}
	for _, x := range list {
		byDir[x.Dir] = x
	}
	if w := byDir["workshop-900"]; w.Name != "手工放的那份" || w.FromUGC {
		t.Errorf("mods/ 那一份要优先，两处只能出一条: %+v", w)
	}
}

// writeUgcModinfo 在引擎下载区里造一份就位的工坊内容。
func writeUgcModinfo(t *testing.T, m *Manager, cluster, shard, id, body string) {
	t.Helper()
	dir := m.Paths.UgcModDir(cluster, shard, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "modinfo.lua"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
