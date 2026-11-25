// 开服引导这一层的用例：新建集群、令牌、世界设定。
//
// 这组接口是"面板能开服"的全部前提，所以每个用例都同时看两头：
// HTTP 回复说了什么，以及磁盘上真的留下了什么。只断言回复的测试挡不住
// "回了 200 但文件写歪了"——那种失败要等引擎启动时才暴露，代价最大。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"dstpanel/internal/dst"
)

func sptr(s string) *string { return &s }
func iptr(i int) *int       { return &i }
func bptr(b bool) *bool     { return &b }

func readOrDie(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(raw)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s 不该存在（err=%v）", path, err)
	}
}

// schemaPickKey 从字典里现取一个"只在 onlyIn 地形有、notIn 地形没有"的键及其合法取值。
// 不写死键名也不写死第一个：world_setting.json 是 Klei 的资产，条目会随版本增减，
// 钉死具体键的用例会在某次更新资产后变成假失败。
func schemaPickKey(t *testing.T, onlyIn, notIn string, except ...string) (string, string) {
	t.Helper()
	schema, err := dst.LoadWorldSchema()
	if err != nil {
		t.Fatalf("LoadWorldSchema: %v", err)
	}
	a, ok := schema.Location(onlyIn, "zh")
	if !ok {
		t.Fatalf("字典里没有地形 %q", onlyIn)
	}
	b, ok := schema.Location(notIn, "zh")
	if !ok {
		t.Fatalf("字典里没有地形 %q", notIn)
	}
	skip := strings.Join(except, ",")
	for _, g := range a.Groups {
		for _, it := range g.Items {
			if len(it.Options) == 0 || strings.Contains(skip, it.Key) {
				continue
			}
			if _, exists := b.Item(it.Key); exists {
				continue
			}
			// 要挑一个和默认值不同的取值：改了等于没改的话，
			// "changedFromDefault"那类断言就是在比空气。
			for _, o := range it.Options {
				if o.Value != it.Default {
					return it.Key, o.Value
				}
			}
		}
	}
	t.Fatalf("%s 地形里找不到 %q 地形没有的键（已排除 %s）", onlyIn, notIn, skip)
	return "", ""
}

func TestClusterCreateWritesEverythingAndSaysWhatsNext(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	sizeKey, sizeVal := schemaPickKey(t, "forest", "cave")
	worldKey, worldVal := schemaPickKey(t, "cave", "forest")

	rec := doJSON(t, h, http.MethodPost, "/api/v1/clusters", clusterCreateRequest{
		Name:      sptr("Cluster_7"),
		NumShards: iptr(2),
		Game:      clusterGameRequest{DisplayName: sptr("测试服"), MaxPlayers: iptr(8)},
		World: &clusterWorldRequest{
			// 地表点一个非默认预设，洞穴什么都不点（该落到洞穴自己的默认预设上）。
			Presets:       map[string]string{"Master": "SURVIVAL_TOGETHER_CLASSIC"},
			FlatOverrides: map[string]string{sizeKey: sizeVal},
			Overrides:     map[string]map[string]string{"Caves": {worldKey: worldVal}},
		},
	})
	var reply clusterCreateReply
	decodeData(t, rec, &reply)

	if reply.Name != "Cluster_7" {
		t.Errorf("集群名回来是 %q", reply.Name)
	}
	if want := filepath.Join(f.paths.KleiRoot, "Cluster_7"); reply.Dir != want {
		t.Errorf("目录 %q 不是 kleiRoot 下的那个集群目录 %q", reply.Dir, want)
	}
	if len(reply.Shards) != 2 {
		t.Fatalf("两个分片都要报出来: %+v", reply.Shards)
	}
	for _, sh := range reply.Shards {
		if !sh.WorldFile || !sh.Helper {
			t.Errorf("%s 分片的世界文件或助手没写成: %+v", sh.Shard, sh)
		}
		// 界面拿的是中文地形名，forest/cave 这两个码值不该出现在文案位上。
		if sh.LocationZh == "" || sh.LocationZh == sh.Location {
			t.Errorf("%s 分片的地形文案是码值: %+v", sh.Shard, sh)
		}
		// 预设同理：码值留给排查，文案位必须是中文，而且两者不能一样。
		if sh.PresetZh == "" || sh.PresetZh == sh.Preset {
			t.Errorf("%s 分片的预设文案是码值: %+v", sh.Shard, sh)
		}
		if sh.Port == 0 {
			t.Errorf("%s 分片没分到端口", sh.Shard)
		}
	}

	ini := readOrDie(t, f.paths.ClusterIni("Cluster_7"))
	if !strings.Contains(ini, "cluster_name = 测试服") || !strings.Contains(ini, "max_players = 8") {
		t.Errorf("cluster.ini 没写进这次给的设置:\n%s", ini)
	}
	// 引擎不剥引号：写成 game_mode = "survival" 它会读成 '"survival"'，
	// 然后报 "Game mode '\"survival\"' not found in GAME_MODES"（云机首启实测）。
	if strings.Contains(ini, `= "`) {
		t.Errorf("cluster.ini 里有带引号的字符串值，引擎会连引号一起当值读:\n%s", ini)
	}
	if !strings.Contains(ini, "shard_enabled = true") {
		t.Errorf("两个分片却没写 shard_enabled，洞穴会连不上地面:\n%s", ini)
	}

	// 落盘的世界文件按引擎的读法解回来，比对内容而不是比对文本形状
	// （开关/preset/overrides 三件套齐备这条由 dst 层的用例盯着，
	// 这里管"给进去的设定生效了没有"）。
	master, present := mustReadWorldGen(t, f, "Cluster_7", "Master")
	if !present {
		t.Fatal("Master 的世界文件没落地")
	}
	if master.Preset != "SURVIVAL_TOGETHER_CLASSIC" {
		t.Errorf("Master 的预设 = %q，请求点的是 SURVIVAL_TOGETHER_CLASSIC", master.Preset)
	}
	if master.Overrides[sizeKey] != sizeVal {
		t.Errorf("平铺的 %s=%s 没进 Master: %+v", sizeKey, sizeVal, master.Overrides)
	}
	if _, leaked := master.Overrides[worldKey]; leaked {
		t.Errorf("洞穴专属的 %s 漏进了地表表: %+v", worldKey, master.Overrides)
	}

	caves, present := mustReadWorldGen(t, f, "Cluster_7", "Caves")
	if !present {
		t.Fatal("Caves 的世界文件没落地")
	}
	// 洞穴没点预设，必须落到洞穴地形自己的默认预设上，而不是跟着地表走。
	if caves.Preset != dst.DefaultPreset(dst.LocationCave) {
		t.Errorf("Caves 的预设 = %q，期望该地形的默认预设 %q",
			caves.Preset, dst.DefaultPreset(dst.LocationCave))
	}
	if caves.Overrides[worldKey] != worldVal {
		t.Errorf("分片专属的 %s 没进 Caves: %+v", worldKey, caves.Overrides)
	}
	if _, leaked := caves.Overrides[sizeKey]; leaked {
		t.Errorf("地表专属的 %s 漏进了洞穴表: %+v", sizeKey, caves.Overrides)
	}

	// 助手 MOD：建集群默认就装，否则这个集群在面板里是"起得来但没有状态"。
	if _, err := os.Stat(f.paths.ModOverrides("Cluster_7", "Master")); err != nil {
		t.Errorf("没写 modoverrides.lua: %v", err)
	}

	notes := strings.Join(reply.Notes, "\n")
	if !strings.Contains(notes, "令牌") {
		t.Errorf("没给令牌时必须说明启动会被服务端拒绝，实际 notes:\n%s", notes)
	}
	if !strings.Contains(notes, "Your Server Will Not Start") {
		t.Errorf("notes 要引用引擎原话，用户搜日志时能对上:\n%s", notes)
	}
	if !strings.Contains(notes, "生成世界") {
		t.Errorf("第一次启动要等世界生成，不说明就会被当成卡死:\n%s", notes)
	}
	// 世界请求给过之后要说明"世界名"这件事：接口不再有 name 字段，
	// 不解释的话用户会以为面板把名字弄丢了（名字在预设那一层，面板不写它）。
	if !strings.Contains(notes, "世界名字") {
		t.Errorf("给了世界请求却没说明世界名的归属，用户会来找这个字段:\n%s", notes)
	}

	entries, err := f.srv.Store.ListAudit(10, 0, "cluster.")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != "cluster.create" || !entries[0].OK {
		t.Fatalf("开服要留一条操作记录，实际 %+v", entries)
	}
	// 详情里只有"给了没给、多长"，永远不能有令牌明文。
	if !strings.Contains(entries[0].Detail, "未提供") || !strings.Contains(entries[0].Detail, "已写") {
		t.Errorf("审计详情要说清令牌和世界设定的状态，实际 %q", entries[0].Detail)
	}
}

// 参数没验完之前一个字节都不能写：建到一半失败会留下"有 cluster.ini 没 server.ini"
// 的半成品，比直接失败难清理得多。
func TestClusterCreateValidatesBeforeTouchingDisk(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	target := filepath.Join(f.paths.KleiRoot, "Cluster_8")

	cases := []struct {
		name string
		req  clusterCreateRequest
		want string
	}{
		{"非法游戏模式", clusterCreateRequest{Name: sptr("Cluster_8"),
			Game: clusterGameRequest{GameMode: sptr("nonsense")}}, "survival"},
		{"玩家数为 0", clusterCreateRequest{Name: sptr("Cluster_8"),
			Game: clusterGameRequest{MaxPlayers: iptr(0)}}, "至少"},
		{"玩家数超上限", clusterCreateRequest{Name: sptr("Cluster_8"),
			Game: clusterGameRequest{MaxPlayers: iptr(20)}}, "16"},
		{"非法 tick 速率", clusterCreateRequest{Name: sptr("Cluster_8"),
			Game: clusterGameRequest{TickRate: iptr(22)}}, "15"},
		{"分片数只支持 1 或 2", clusterCreateRequest{Name: sptr("Cluster_8"), NumShards: iptr(3)}, "洞穴"},
		{"目录名以点开头", clusterCreateRequest{Name: sptr("..evil"), NumShards: iptr(1)}, "点开头"},
		{"目录名带分隔符", clusterCreateRequest{Name: sptr("a/b"), NumShards: iptr(1)}, "只能用"},
		{"令牌太短", clusterCreateRequest{Name: sptr("Cluster_8"), NumShards: iptr(1),
			Token: sptr("short")}, "过短"},
		{"令牌混进空格", clusterCreateRequest{Name: sptr("Cluster_8"), NumShards: iptr(1),
			Token: sptr("abcd efgh ijkl mnop qrst")}, "空白"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPost, "/api/v1/clusters", c.req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d: %s", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, c.want) {
				t.Errorf("报错没说到点上（要含 %q）: %s", c.want, body)
			}
			if _, err := os.Stat(target); err == nil {
				t.Fatalf("%s 被拒了却留下了集群目录", c.name)
			}
		})
	}
	mustNotExist(t, filepath.Join(filepath.Dir(f.paths.KleiRoot), "evil"))
	mustNotExist(t, filepath.Join(f.paths.KleiRoot, "evil"))
}

func TestClusterCreateRejectsDuplicateName(t *testing.T) {
	f := newFixture(t, "")
	one := 1
	rec := doJSON(t, f.srv.Handler(), http.MethodPost, "/api/v1/clusters",
		clusterCreateRequest{Name: sptr("Cluster_1"), NumShards: &one})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("重名应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Cluster_1") || !strings.Contains(body, "已存在") {
		t.Errorf("要说清是哪个集群已存在，实际 %s", body)
	}
}

// 不指定名字时用下一个空号，且不能撞上已经在跑的 Cluster_1。
func TestClusterCreatePicksNextFreeName(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPost, "/api/v1/clusters",
		clusterCreateRequest{NumShards: iptr(1)})
	var reply clusterCreateReply
	decodeData(t, rec, &reply)
	if reply.Name == "" || reply.Name == "Cluster_1" {
		t.Errorf("自动取名不对: %q", reply.Name)
	}
	if _, err := os.Stat(f.paths.ClusterIni(reply.Name)); err != nil {
		t.Errorf("回复说建好了，目录里却没有 cluster.ini: %v", err)
	}
}

// 单分片集群：洞穴目录和分片共享都不该出现；installHelper=false 要真的不装。
func TestClusterCreateSingleShard(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPost, "/api/v1/clusters",
		clusterCreateRequest{Name: sptr("Cluster_5"), NumShards: iptr(1), InstallHelper: bptr(false)})
	var reply clusterCreateReply
	decodeData(t, rec, &reply)
	if len(reply.Shards) != 1 || reply.Shards[0].Shard != "Master" {
		t.Fatalf("只要一个分片却回了 %+v", reply.Shards)
	}
	if reply.Shards[0].Helper {
		t.Error("installHelper=false 却把助手装上了")
	}
	if reply.Shards[0].WorldFile {
		t.Error("没带 world 请求却写了世界文件——引擎用自己的默认世界更好")
	}
	mustNotExist(t, f.paths.ShardDir("Cluster_5", "Caves"))
	ini := readOrDie(t, f.paths.ClusterIni("Cluster_5"))
	if strings.Contains(ini, "shard_enabled = true") {
		t.Errorf("单分片集群不该开分片共享:\n%s", ini)
	}
	notes := strings.Join(reply.Notes, "\n")
	if !strings.Contains(notes, "默认世界") {
		t.Errorf("没写世界设定时要说明用的是引擎默认世界，实际:\n%s", notes)
	}
}

// 令牌是这一层唯一的秘密：明文不进响应，也不进审计。
// TestClusterCreateHonoursWantedGamePort 盯"用户在向导里自己填端口"这条路。
// 填进去的号必须真的落在地表的 server.ini 上（而不是被面板"优化"掉），
// 回复里还要给出要放行的 UDP 号：他照着这些号去开云安全组，
// 少一个号就是房间起得来、玩家进不来，而面板上一切正常。
func TestClusterCreateHonoursWantedGamePort(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPost, "/api/v1/clusters",
		clusterCreateRequest{Name: sptr("Cluster_5"), NumShards: iptr(2), InstallHelper: bptr(false), GamePort: 11500})
	var reply clusterCreateReply
	decodeData(t, rec, &reply)

	shard := readOrDie(t, f.paths.ShardIni("Cluster_5", dst.ShardMaster))
	if !strings.Contains(shard, "server_port = 11500") {
		t.Errorf("填的号没落到地表的 server.ini 上:\n%s", shard)
	}
	ini := readOrDie(t, f.paths.ClusterIni("Cluster_5"))
	if !strings.Contains(ini, "master_port = 11501") {
		t.Errorf("意图端口没跟着变成 +1，洞穴会连不上地面:\n%s", ini)
	}
	if len(reply.Shards) != 2 || reply.Shards[0].Port != 11500 {
		t.Errorf("回复里的分片端口不对: %+v", reply.Shards)
	}
	notes := strings.Join(reply.Notes, "\n")
	// 洞穴那个 +1 只存在于引擎的行为里，不写出来没人知道要放行。
	for _, want := range []string{"云安全组", "11500", "11501"} {
		if !strings.Contains(notes, want) {
			t.Errorf("提示缺 %q，实际:\n%s", want, notes)
		}
	}
}

func TestClusterCreateRejectsPortAlreadyInUse(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	decodeData(t, doJSON(t, h, http.MethodPost, "/api/v1/clusters",
		clusterCreateRequest{Name: sptr("Cluster_5"), NumShards: iptr(2), InstallHelper: bptr(false)}),
		&clusterCreateReply{})

	cases := []struct {
		name string
		port int
		want string
	}{
		{"撞上别的房间的游戏端口", 10998, "占用"},
		// 10997 空着，但它是 10998 的配套号；面板收下这个号就等于开了两个抢同一个端口的房间。
		{"撞上别的房间的配套端口", 10997, "配套"},
		{"太小", 80, "1024"},
		{"顶到上界外", 65535, "65534"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPost, "/api/v1/clusters",
				clusterCreateRequest{Name: sptr("Cluster_6"), NumShards: iptr(2), InstallHelper: bptr(false), GamePort: tc.port})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应回 400，实际 %d：%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("拒绝理由里要带上 %q，实际 %s", tc.want, rec.Body.String())
			}
			// 拒绝理由里不能出现 ini 的键名和分片目录名：那是我们排查时的说法。
			for _, leak := range []string{"[SHARD]", "server_port", "master_port"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("拒绝理由里漏出了文件细节 %q：%s", leak, rec.Body.String())
				}
			}
			// 被拒的这次不能留下半个房间：留了会在列表里变成一个起不来的条目。
			mustNotExist(t, f.paths.ClusterIni("Cluster_6"))
		})
	}
}

func TestClusterCreateAcceptsPortOutsideAutoRange(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPost, "/api/v1/clusters",
		clusterCreateRequest{Name: sptr("Cluster_5"), NumShards: iptr(2), InstallHelper: bptr(false), GamePort: 12345})
	if rec.Code != http.StatusOK {
		t.Fatalf("区间外的号该收得下（改端口那一页本来就允许），实际 %d：%s", rec.Code, rec.Body.String())
	}
	var reply clusterCreateReply
	decodeData(t, rec, &reply)
	if got := readOrDie(t, f.paths.ShardIni("Cluster_5", dst.ShardMaster)); !strings.Contains(got, "server_port = 12345") {
		t.Errorf("填的号没原样落盘:\n%s", got)
	}
	// 洞穴那一组仍从面板区间里挑，并且不能压到 12344（它的配套号正是 12345）。
	caves := readOrDie(t, f.paths.ShardIni("Cluster_5", dst.ShardCaves))
	if !strings.Contains(caves, "master_port = 12346") {
		t.Errorf("洞穴的配套端口没指向地表的 +1:\n%s", caves)
	}
	notes := strings.Join(reply.Notes, "\n")
	if !strings.Contains(notes, "12345") || !strings.Contains(notes, "12346") {
		t.Errorf("回复要列出要放行的号，实际:\n%s", notes)
	}
	if !strings.Contains(notes, "区间") {
		t.Errorf("区间外的号要说一句它归用户管，实际:\n%s", notes)
	}
}

func TestTokenNeverAppearsInResponsesOrAudit(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	const token = "AbCdEf1234567890_-xyzw"

	rec := doJSON(t, h, http.MethodPost, "/api/v1/clusters",
		clusterCreateRequest{Name: sptr("Cluster_6"), NumShards: iptr(1), Token: sptr(token)})
	if rec.Code != http.StatusOK {
		t.Fatalf("带令牌建集群失败: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("创建响应里带了令牌明文: %s", rec.Body.String())
	}
	if got := strings.TrimSpace(readOrDie(t, f.paths.TokenFile("Cluster_6"))); got != token {
		t.Errorf("cluster_token.txt 里是 %q，和给的不一致", got)
	}
	// 带令牌的创建不该再提醒"会被服务端拒绝"。
	var reply clusterCreateReply
	decodeData(t, rec, &reply)
	if strings.Contains(strings.Join(reply.Notes, "\n"), "Your Server Will Not Start") {
		t.Errorf("已经写了令牌还警告启动失败: %+v", reply.Notes)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_6/token", "admin", "secret")
	var view tokenView
	decodeData(t, rec, &view)
	if !view.Configured || view.Length != len(token) {
		t.Errorf("令牌状态回错了: %+v", view)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("GET token 响应泄了明文: %s", rec.Body.String())
	}

	// 换成另一枚：响应和审计依旧只能有长度。
	const second = "ZZZ111222333444555666777"
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_6/token", tokenRequest{Token: second})
	decodeData(t, rec, &view)
	if !view.Configured || view.Length != len(second) {
		t.Errorf("写入后回的状态不对: %+v", view)
	}
	if strings.Contains(rec.Body.String(), second) {
		t.Errorf("PUT token 泄了明文: %s", rec.Body.String())
	}
	if got := strings.TrimSpace(readOrDie(t, f.paths.TokenFile("Cluster_6"))); got != second {
		t.Errorf("令牌没换成新的，文件里是 %q", got)
	}

	entries, err := f.srv.Store.ListAudit(50, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token, second} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("审计记录里出现了令牌明文 %q", secret)
		}
	}

	// 纯空白是"清空"还是"没给"？这一层按非法处理，避免界面上误点把集群弄瘫。
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_6/token", tokenRequest{Token: "  "})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "为空") {
		t.Errorf("空白令牌要 400 并说明，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(readOrDie(t, f.paths.TokenFile("Cluster_6"))); got != second {
		t.Errorf("被拒的写入把已有令牌弄坏了: %q", got)
	}
}

// 摘令牌只有 DELETE 这一条路：PUT 空串要按"粘贴不完整"拒掉（上面那条用例守着这个），
// 所以界面点"清空"时换的是动作，不是换一个空值。这里盯四件：
// 轮换不留上一串明文的 .bak、清空时连旧版本留下的 .bak 一起删、状态回到"没配"、
// 审计里仍然只有动作没有明文。
func TestTokenDeleteRemovesTokenAndItsBak(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	const first = "AbCdEf1234567890_-xyzw"
	const second = "ZZZ111222333444555666777"

	rec := doJSON(t, h, http.MethodPost, "/api/v1/clusters",
		clusterCreateRequest{Name: sptr("Cluster_7"), NumShards: iptr(1), Token: sptr(first)})
	if rec.Code != http.StatusOK {
		t.Fatalf("带令牌建集群失败: %s", rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_7/token", tokenRequest{Token: second})
	if rec.Code != http.StatusOK {
		t.Fatalf("换令牌失败: %s", rec.Body.String())
	}
	bak := f.paths.TokenFile("Cluster_7") + ".bak"
	// 轮换令牌的动机常常是"旧值泄露了"，所以换完不能留下装着上一串明文的 .bak。
	mustNotExist(t, bak)
	// 旧版本会留下这样的 .bak：清空时必须一并删掉，不能只摘一半。
	if err := os.WriteFile(bak, []byte(first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec = do(t, h, http.MethodDelete, "/api/v1/clusters/Cluster_7/token", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("摘令牌应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var cleared tokenView
	decodeData(t, rec, &cleared)
	if cleared.Configured || cleared.Length != 0 {
		t.Errorf("摘完应回\"没配\"，实际 %+v", cleared)
	}
	if !strings.Contains(cleared.Hint, "Your Server Will Not Start") {
		t.Errorf("清空的结果要接着说清后果，实际 hint=%q", cleared.Hint)
	}
	mustNotExist(t, f.paths.TokenFile("Cluster_7"))
	mustNotExist(t, bak)

	rec = do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_7/token", "admin", "secret")
	var view tokenView
	decodeData(t, rec, &view)
	if view.Configured || view.Length != 0 {
		t.Errorf("摘下之后应回\"没配\"，实际 %+v", view)
	}
	if strings.Contains(rec.Body.String(), first) || strings.Contains(rec.Body.String(), second) {
		t.Errorf("GET token 响应泄了明文: %s", rec.Body.String())
	}

	entries, err := f.srv.Store.ListAudit(50, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{first, second} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("审计记录里出现了令牌明文 %q", secret)
		}
	}
}

func TestTokenRoutesRejectUnknownCluster(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_9/token", "admin", "secret")
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的集群应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_9/token",
		tokenRequest{Token: "AbCdEf1234567890_-xyzw"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("给不存在的集群写令牌应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	mustNotExist(t, f.paths.TokenFile("Cluster_9"))
}

// 世界设定字典是界面的数据来源：文案必须是中文，不能把 world_size 这种键名当标签。
func TestWorldSchemaSendsChineseLabels(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/world-schema", "admin", "secret")
	var out []worldLocationView
	decodeData(t, rec, &out)

	if len(out) != 2 {
		t.Fatalf("地表和洞穴两张表都要给，实际 %d 张", len(out))
	}
	counted := map[string]int{}
	for _, loc := range out {
		if loc.Label == "" || loc.Label == loc.Location {
			t.Errorf("地形 %q 的文案是码值: %q", loc.Location, loc.Label)
		}
		// shard 告诉界面"这张表该提交给哪个分片"，写反了就是把地表设定塞进洞穴。
		if want := dst.ShardForLocation(loc.Location); loc.Shard != want {
			t.Errorf("%s 表说属于 %q，应为 %q", loc.Location, loc.Shard, want)
		}
		n := 0
		for _, g := range loc.Groups {
			if g.Label == "" || g.Label == g.Key {
				t.Errorf("分组 %q 没有中文标题", g.Key)
			}
			for _, it := range g.Items {
				n++
				if it.Label == "" || it.Label == it.Key {
					t.Errorf("条目 %s 的标签是键名本身: %q", it.Key, it.Label)
				}
				if len(it.Options) == 0 {
					t.Errorf("条目 %s 没有可选项，界面渲染不出下拉框", it.Key)
				}
				for _, o := range it.Options {
					if o.Value == "" || o.Label == "" {
						t.Errorf("条目 %s 有空的选项: %+v", it.Key, o)
					}
				}
			}
		}
		if n != loc.ItemCount {
			t.Errorf("%s 表 itemCount=%d 但实际条目 %d", loc.Location, loc.ItemCount, n)
		}
		if n < 20 {
			t.Errorf("%s 表只有 %d 项，字典像是残缺的", loc.Location, n)
		}
		counted[loc.Location] = n
	}
	// 两张表必须各自成表：洞穴没有季节那一组，条目数明显更少。
	if counted[dst.LocationForest] <= counted[dst.LocationCave] {
		t.Errorf("地表条目数 (%d) 应多于洞穴 (%d)", counted[dst.LocationForest], counted[dst.LocationCave])
	}
}

func TestWorldShowSaysEngineDefaultWhenFileAbsent(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/world", "admin", "secret")
	var v worldView
	decodeData(t, rec, &v)
	if v.Present {
		t.Error("还没写过世界文件却报存在")
	}
	// 没有文件不是错误，界面要能一句话回答"那现在用的是什么"。
	if !strings.Contains(v.Note, "默认世界") {
		t.Errorf("note 要说明用的是引擎默认世界，实际 %q", v.Note)
	}
	if v.Overrides == nil {
		t.Error("overrides 要回空对象而不是 null，界面就不用两套判断")
	}
	if v.Location != dst.LocationForest {
		t.Errorf("Master 的地形应是 %q，实际 %q", dst.LocationForest, v.Location)
	}
}

func TestWorldPutWritesFullDefaultsAndRefusesToRegenerate(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	url := "/api/v1/clusters/Cluster_1/shards/Master/world"
	sizeKey, sizeVal := schemaPickKey(t, "forest", "cave")

	rec := doJSON(t, h, http.MethodPut, url,
		worldPutRequest{Preset: sptr("SURVIVAL_TOGETHER_CLASSIC"),
			Overrides: map[string]string{sizeKey: sizeVal}})
	var v worldView
	decodeData(t, rec, &v)
	if !v.Present {
		t.Error("写完却报不存在")
	}
	if v.Preset != "SURVIVAL_TOGETHER_CLASSIC" || v.PresetZh == "" || v.PresetZh == v.Preset {
		t.Errorf("预设没回显或文案位露了码值: %+v", v)
	}
	if v.Path != f.paths.WorldGenOverride("Cluster_1", "Master") {
		t.Errorf("界面要能跳到实际那份文件，path 回来是 %q", v.Path)
	}
	// 写全不写增量：用户打开文件看到的就是当前生效的完整状态。
	if len(v.Overrides) < 20 {
		t.Errorf("只提交了一项却写出 %d 项，默认值没补齐: %+v", len(v.Overrides), v.Overrides)
	}
	if v.Overrides[sizeKey] != sizeVal {
		t.Errorf("提交的那一项没生效: %+v", v.Overrides)
	}
	if len(v.Changed) != 1 || v.Changed[0] != sizeKey {
		t.Errorf("changedFromDefault 应只有刚改的那一项，实际 %+v", v.Changed)
	}
	// 改设定 ≠ 重生世界，这句话必须说，否则用户会以为世界立刻变小了。
	if !strings.Contains(v.Note, "下一次世界生成") {
		t.Errorf("note 没说明生效时机: %q", v.Note)
	}
	// 回复里的表和磁盘上的文件必须一致：界面显示的"当前状态"就是引擎下次要读的。
	onDisk, present := mustReadWorldGen(t, f, "Cluster_1", "Master")
	if !present {
		t.Fatal("文件没落地")
	}
	if onDisk.Preset != v.Preset || onDisk.Overrides[sizeKey] != sizeVal ||
		len(onDisk.Overrides) != len(v.Overrides) {
		t.Errorf("回复和文件不一致：回复 %d 项，文件 %+v", len(v.Overrides), onDisk.Overrides)
	}

	// 再改一次：没提交的那一项要留着，而不是被默认值打回去。
	otherKey, otherVal := schemaPickKey(t, "forest", "cave", sizeKey)
	if otherKey == "" {
		t.Skip("字典里只找到一个地表专属键")
	}
	rec = doJSON(t, h, http.MethodPut, url,
		worldPutRequest{Overrides: map[string]string{otherKey: otherVal}})
	decodeData(t, rec, &v)
	if v.Overrides[sizeKey] != sizeVal {
		t.Errorf("第二次提交把上一次的 %s 弄丢了: %+v", sizeKey, v.Overrides)
	}
	if len(v.Changed) != 2 {
		t.Errorf("两次改动都该在 changedFromDefault 里，实际 %+v", v.Changed)
	}
	// 不提交 preset 时沿用文件里已有的预设，不能悄悄退回默认那张图。
	if v.Preset != "SURVIVAL_TOGETHER_CLASSIC" {
		t.Errorf("预设被打回了: %q", v.Preset)
	}

	// 点了一个洞穴预设：换成默认值等于替用户重造世界，必须 400 说清楚。
	rec = doJSON(t, h, http.MethodPut, url, worldPutRequest{Preset: sptr("DST_CAVE")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("地表分片点洞穴预设应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "SURVIVAL_TOGETHER") {
		t.Errorf("400 要列出这个分片可用的预设，实际 %s", body)
	}

	// regenerate=true 也只是界面上的勾选，面板不许自己去清存档重造世界。
	rec = doJSON(t, h, http.MethodPut, url, worldPutRequest{Regenerate: true})
	decodeData(t, rec, &v)
	if !strings.Contains(v.Note, "没有重生世界") {
		t.Errorf("带 regenerate 时要明说没执行，实际 %q", v.Note)
	}

	entries, err := f.srv.Store.ListAudit(20, 0, "world.")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("三次写入各记一条，实际 %+v", entries)
	}
	// 审计里要能看出这次落的是哪个预设，出事时不用再去翻文件。
	if !strings.Contains(entries[2].Detail, "SURVIVAL_TOGETHER_CLASSIC") {
		t.Errorf("审计详情没记预设: %q", entries[2].Detail)
	}
}

func TestWorldPutRejectsValueOutsideTable(t *testing.T) {
	f := newFixture(t, "")
	path := f.paths.WorldGenOverride("Cluster_1", "Master")
	sizeKey, _ := schemaPickKey(t, "forest", "cave")

	rec := doJSON(t, f.srv.Handler(), http.MethodPut,
		"/api/v1/clusters/Cluster_1/shards/Master/world",
		worldPutRequest{Overrides: map[string]string{sizeKey: "nonsense_value"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("表外取值应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, sizeKey) {
		t.Errorf("报错要点名是哪一项，实际 %s", body)
	}
	// 校验没过就不能留文件：半套设定比没改更难查。
	mustNotExist(t, path)
}

// 文件是手工做的（没有 preset，或 preset 是这个分片用不了的那个）时，
// 只提交 overrides 不能悄悄补一个默认预设进去——那等于把"部分合并"改成"整张覆盖"，
// 替用户换了一张世界图。这里必须停下来让人去看。
func TestWorldPutRefusesToGuessAPresetForHandMadeFile(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	cases := []struct {
		name   string
		shard  string
		seeded string
	}{
		{"洞穴文件里是地表预设", "Caves",
			`return { override_enabled = true, preset = "SURVIVAL_TOGETHER", overrides = { } }`},
		{"文件里根本没有 preset", "Master",
			`return { override_enabled = true, overrides = { } }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := f.paths.WorldGenOverride("Cluster_1", c.shard)
			if err := os.WriteFile(path, []byte(c.seeded+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			rec := doJSON(t, h, http.MethodPut,
				"/api/v1/clusters/Cluster_1/shards/"+c.shard+"/world",
				worldPutRequest{Overrides: map[string]string{"start_location": "default"}})
			if rec.Code != http.StatusConflict {
				t.Fatalf("该 409 停下来，实际 %d: %s", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, "preset") {
				t.Errorf("409 要点名是 preset 的事，实际 %s", body)
			}
			if readOrDie(t, path) != c.seeded+"\n" {
				t.Error("409 之前已经把文件改掉了")
			}
			// 明确点了一个合法预设就该放行：这是用户自己选的，不是面板猜的。
			want := dst.DefaultPreset(dst.ShardLocation(c.shard))
			rec = doJSON(t, h, http.MethodPut,
				"/api/v1/clusters/Cluster_1/shards/"+c.shard+"/world",
				worldPutRequest{Preset: sptr(want)})
			if rec.Code != http.StatusOK {
				t.Fatalf("点了本分片可用的预设应放行，实际 %d: %s", rec.Code, rec.Body.String())
			}
			got, _ := mustReadWorldGen(t, f, "Cluster_1", c.shard)
			if got.Preset != want {
				t.Errorf("写回来的预设 = %q，点的是 %q", got.Preset, want)
			}
		})
	}
	// GET 不受这个限制：读接口要说清现状，不能也回 409 让用户什么都看不到。
	rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/world", "admin", "secret")
	var v worldView
	decodeData(t, rec, &v)
	if v.Preset == "" {
		t.Errorf("GET 要如实回显文件里的 preset: %+v", v)
	}
}

// 分片成功进过世界之后，磁盘上这份文件是引擎重写过的（TestReadWorldGenToleratesEngineRewrittenFile
// 里逐字节抄自云机的那一份）。世界页此时必须照常能用：读得回预设、改得动设定，
// 并且要说清"文件里只剩一项"是引擎干的，不是面板把用户的设定弄丢了。
func TestWorldRoutesWorkAfterEngineRewroteTheFile(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	path := f.paths.WorldGenOverride("Cluster_1", "Master")
	seeded := "KLEI     1 return {\n\toverride_enabled = true,\n\tworldgen_preset = \"SURVIVAL_TOGETHER_CLASSIC\",\n\tsettings_preset = \"SURVIVAL_TOGETHER_CLASSIC\",\n\toverrides = {\n\t\ttask_set = \"default\",\n\t},\n}"
	if err := os.WriteFile(path, []byte(seeded), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/world", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("引擎改写过的文件不能把世界页读挂: %d %s", rec.Code, rec.Body.String())
	}
	var got worldView
	decodeData(t, rec, &got)
	if got.Preset != "SURVIVAL_TOGETHER_CLASSIC" {
		t.Errorf("preset 应读成 %q，实际 %q", "SURVIVAL_TOGETHER_CLASSIC", got.Preset)
	}
	if got.PresetZh == "" || got.PresetZh == got.Preset {
		t.Errorf("界面拿到的是码值而不是中文说法: %+v", got)
	}
	if !strings.Contains(got.Note, "引擎") {
		t.Errorf("Note 要说明这份文件被引擎改写过，否则用户以为设定被面板删了: %q", got.Note)
	}

	// 这时只提交 overrides 不该 409：文件里的预设是合法可用的，只是拼法换了。
	sizeKey, sizeVal := schemaPickKey(t, "forest", "cave")
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/world",
		worldPutRequest{Overrides: map[string]string{sizeKey: sizeVal}})
	if rec.Code != http.StatusOK {
		t.Fatalf("已成功启动过的分片不该被锁住编辑: %d %s", rec.Code, rec.Body.String())
	}
	spec, _ := mustReadWorldGen(t, f, "Cluster_1", "Master")
	if spec.Preset != "SURVIVAL_TOGETHER_CLASSIC" {
		t.Errorf("没点预设时应沿用引擎留下的那一个，写回的是 %q", spec.Preset)
	}
	if spec.Overrides[sizeKey] != sizeVal {
		t.Errorf("提交的那一项没落盘: %+v", spec.Overrides)
	}
	// 面板接手后写回的是自己的拼法，界面与文件重新对上。
	if text := readOrDie(t, path); !strings.Contains(text, "\n\tpreset = \"SURVIVAL_TOGETHER_CLASSIC\",") {
		t.Errorf("面板该按自己的格式重写这份文件:\n%s", text)
	}
}

// 手工放过 leveldataoverride.lua 的现场必须在 GET 里看得见：
// 那份文件在 preset 通道下不会生效，不说清楚就有人对着它调半天的参。
func TestWorldShowPointsAtStaleLevelDataFile(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	if err := os.WriteFile(f.paths.LevelData("Cluster_1", "Master"),
		[]byte("return { id = \"default\", name = \"拷来的\", desc = \"\", location = \"forest\", overrides = { } }"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/world", "admin", "secret")
	var v worldView
	decodeData(t, rec, &v)
	if !strings.Contains(v.LevelConflict, "leveldataoverride.lua") {
		t.Errorf("界面要指出那份手工文件的存在与不生效，实际 %q", v.LevelConflict)
	}
	if !strings.Contains(v.LevelConflict, "不会生效") {
		t.Errorf("光说文件存在不够，要说清它不起作用:\n%s", v.LevelConflict)
	}
}

func TestWorldRoutesValidateShardName(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Nope/world", "admin", "secret")
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的分片应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/..%2Fevil/shards/Master/world",
		worldPutRequest{Overrides: map[string]string{"start_location": "default"}})
	if rec.Code == http.StatusOK {
		t.Fatalf("越界集群名竟然成功了: %s", rec.Body.String())
	}
	mustNotExist(t, filepath.Join(filepath.Dir(f.paths.KleiRoot), "evil"))
	mustNotExist(t, filepath.Join(f.paths.KleiRoot, "evil"))
}

// 世界文件里的中文、制表符要能原样读回来——这是玩家能看到的世界名。
// 面板写出去的那份文件、GET 报出来的那份状态、引擎下次启动要读的那张表，
// 三者必须逐项一致。云机首启教会我的就是"界面显示的和引擎读的不是一份"这种
// 偏差最不容易被发现：两边都各自看着正常。
func TestWorldPutGetAndDiskAgreeItemByItem(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	sizeKey, sizeVal := schemaPickKey(t, "forest", "cave")

	rec := doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/Master/world",
		worldPutRequest{Overrides: map[string]string{sizeKey: sizeVal}})
	if rec.Code != http.StatusOK {
		t.Fatalf("写入失败: %s", rec.Body.String())
	}
	var put worldView
	decodeData(t, rec, &put)

	spec, present := mustReadWorldGen(t, f, "Cluster_1", "Master")
	if !present {
		t.Fatal("没写成")
	}
	rec = do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/world", "admin", "secret")
	var got worldView
	decodeData(t, rec, &got)

	for _, v := range []worldView{put, got} {
		if v.Preset != spec.Preset {
			t.Errorf("接口回的预设 = %q，文件里是 %q", v.Preset, spec.Preset)
		}
		if v.Preset != dst.DefaultPreset(dst.LocationForest) {
			t.Errorf("没点预设时应落到该地形的默认预设，实际 %q", v.Preset)
		}
		if len(v.Overrides) != len(spec.Overrides) {
			t.Errorf("项数对不上：接口 %d 项，文件 %d 项", len(v.Overrides), len(spec.Overrides))
		}
		for k, want := range spec.Overrides {
			if v.Overrides[k] != want {
				t.Errorf("%s 接口回 %q，文件里是 %q", k, v.Overrides[k], want)
			}
		}
		if v.Overrides[sizeKey] != sizeVal {
			t.Errorf("提交的那一项在链路上丢了: %+v", v.Overrides)
		}
	}
	// 文件里不该再出现面板不写的东西：世界名与引擎内生项都是手工时代的残留。
	text := readOrDie(t, f.paths.WorldGenOverride("Cluster_1", "Master"))
	// 顶层键按缩进锚定：面板写出的文件是一个 tab 一层，overrides 里的字典项
	// 是三个 tab，所以 ^\t 恰好只命中顶层。拿 substring 查 "location =" 会把
	// 合法项 start_location = 误判成越界（dst 包里那次误报就是这么来的）。
	for _, bad := range []string{`(?m)^\tname = `, `(?m)^\tdesc = `, `(?m)^\tlocation = `, `(?m)^\tid = `} {
		if regexp.MustCompile(bad).MatchString(text) {
			t.Errorf("worldgenoverride.lua 的顶层出现了 %s（引擎只认五个顶层键，多写的只会换一条 WARNING）:\n%s", bad, text)
		}
	}
	for _, bad := range []string{"layout_mode", "has_ocean"} {
		if strings.Contains(text, bad) {
			t.Errorf("worldgenoverride.lua 里出现了引擎内生项 %q:\n%s", bad, text)
		}
	}
}

func mustReadWorldGen(t *testing.T, f *apiFixture, cluster, shard string) (dst.WorldSpec, bool) {
	t.Helper()
	spec, present, err := f.paths.ReadWorldGen(cluster, shard)
	if err != nil {
		t.Fatalf("读 %s/%s 的世界文件: %v", cluster, shard, err)
	}
	return spec, present
}
