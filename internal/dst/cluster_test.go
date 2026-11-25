package dst

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTestPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		KleiRoot:   filepath.Join(dir, "klei", "DoNotStarveTogether"),
		DstInstall: filepath.Join(dir, "dst"),
	}
}

func baseSettings() GameSettings {
	return GameSettings{
		ClusterName:    "我的服务器",
		Description:    "欢迎 " + `"来客"`,
		Password:       `p\ass"w`,
		GameMode:       "survival",
		Intention:      "cooperative",
		MaxPlayers:     8,
		PauseWhenEmpty: true,
		Language:       "zh",
		TickRate:       15,
		ConsoleEnabled: true,
		MaxSnapshots:   20,
	}
}

func TestCreateClusterWritesConsistentShardConfig(t *testing.T) {
	p := newTestPaths(t)
	c, err := p.CreateCluster("Cluster_1", baseSettings(), 2, 10998, 11000)
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	for _, f := range []string{
		p.ClusterIni("Cluster_1"),
		p.ShardIni("Cluster_1", ShardMaster),
		p.ShardIni("Cluster_1", ShardCaves),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("缺文件 %s", f)
		}
	}
	if !c.ShardEnabled() {
		t.Error("两分片集群应打开 shard_enabled")
	}

	master := c.Shard(ShardMaster)
	caves := c.Shard(ShardCaves)
	if !master.IsMaster || caves.IsMaster {
		t.Errorf("is_master 分配错: %+v %+v", master, caves)
	}
	// Klei 的硬规则：cluster.ini 的 master_port 必须等于 Master 的 server_port + 1。
	if c.MasterPort() != master.ServerPort+1 {
		t.Errorf("master_port=%d 应该是 Master server_port=%d 加一", c.MasterPort(), master.ServerPort)
	}
	if caves.MasterPort != c.MasterPort() || caves.MasterIP != "127.0.0.1" {
		t.Errorf("洞穴分片没指回地面: %+v", caves)
	}
	if master.ID == caves.ID || master.ID == 0 || caves.ID == 0 {
		t.Errorf("分片 id 必须不同且非零: %d / %d", master.ID, caves.ID)
	}
	if master.ServerPort == caves.ServerPort {
		t.Errorf("两个分片端口撞了: %d", master.ServerPort)
	}
	// cluster_key 三处必须一致，否则洞穴连不上地面。
	if master.ClusterKey == "" || caves.ClusterKey != master.ClusterKey {
		t.Errorf("cluster_key 不一致: %q / %q", master.ClusterKey, caves.ClusterKey)
	}
	if c.Ini.Unquote("[SHARD]", "cluster_key") != caves.ClusterKey {
		t.Error("cluster.ini 与 server.ini 的 cluster_key 不一致")
	}
	// 存档目录在建集群时就备好，游戏首启才有地方落盘。
	for _, sh := range []string{ShardMaster, ShardCaves} {
		if ok, _ := dirExists(p.SaveDir("Cluster_1", sh)); !ok {
			t.Errorf("缺 save 目录: %s", sh)
		}
	}
}

func TestCreateClusterKeepsChineseComments(t *testing.T) {
	p := newTestPaths(t)
	if _, err := p.CreateCluster("Cluster_1", baseSettings(), 1, 10998, 11000); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	raw, err := os.ReadFile(p.ClusterIni("Cluster_1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#游戏模式", "#最大玩家数", "#每秒通信次数", "#分片间连接密码"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("注释骨架丢了 %q:\n%s", want, raw)
		}
	}
}

// 引擎是按文本文件读配置的，不是按面板内存里的那棵树。
// 所以这里把写出去的文件重新解析一遍再对值：段名被写成 [[NETWORK]] 这类
// 引擎不认的形式时，文件里会同时存在"模板默认值"和"面板给的值"两份，
// 内存对象看着全对、游戏实际用的是默认值——只有从磁盘重读才抓得住。
func TestCreateClusterIniIsReadableFromDisk(t *testing.T) {
	p := newTestPaths(t)
	set := baseSettings()
	set.ClusterName = "我的地面服"
	set.GameMode = "survival"
	set.MaxPlayers = 11
	set.TickRate = 20
	if _, err := p.CreateCluster("Cluster_1", set, 2, 10998, 11000); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	raw, err := ReadTextFile(p.ClusterIni("Cluster_1"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	if strings.Contains(text, "[[") {
		t.Errorf("写出的段名引擎不会读:\n%s", text)
	}
	for _, name := range []string{"GAMEPLAY", "NETWORK", "SHARD", "MISC"} {
		if n := strings.Count(text, "["+name+"]"); n != 1 {
			t.Errorf("段 [%s] 出现 %d 次，同一个段被写成两份", name, n)
		}
	}
	fresh := ParseIni(text)
	if got := fresh.Unquote("[NETWORK]", "cluster_name"); got != "我的地面服" {
		t.Errorf("从磁盘读回的服务器名是 %q", got)
	}
	if got := fresh.Unquote("[GAMEPLAY]", "game_mode"); got != "survival" {
		t.Errorf("从磁盘读回的游戏模式是 %q", got)
	}
	if got := fresh.GetInt("[GAMEPLAY]", "max_players", 0); got != 11 {
		t.Errorf("从磁盘读回的最大玩家数是 %d", got)
	}
	if got := fresh.GetInt("[NETWORK]", "tick_rate", 0); got != 20 {
		t.Errorf("从磁盘读回的 tick_rate 是 %d", got)
	}
	if !fresh.GetBool("[SHARD]", "shard_enabled", false) {
		t.Error("两分片集群从磁盘读回来 shard_enabled 是假的")
	}
}

func TestCreateClusterRefusesExisting(t *testing.T) {
	p := newTestPaths(t)
	if _, err := p.CreateCluster("Cluster_1", baseSettings(), 1, 10998, 10999); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateCluster("Cluster_1", baseSettings(), 1, 10998, 11000); err == nil ||
		!strings.Contains(err.Error(), "已存在") {
		t.Errorf("重复建同名集群应该被拦住: %v", err)
	}
	if _, err := p.CreateCluster("Cluster_2", baseSettings(), 3, 10998, 11000); err == nil {
		t.Error("分片数超过 2 应该报错")
	}
}

func TestPickPortsSkipsUsed(t *testing.T) {
	p := newTestPaths(t)
	// 第一个集群吃掉 10998/10999 以及洞穴端口。
	if _, err := p.CreateCluster("Cluster_1", baseSettings(), 2, 10998, 11000); err != nil {
		t.Fatal(err)
	}
	used, err := p.UsedPorts()
	if err != nil {
		t.Fatal(err)
	}
	// 一个两世界房间吃掉四个号：地表、地表的意图端口、洞穴、洞穴那个引擎自己绑的 +1。
	// 最后那个不写在任何文件里，漏登记的话下一个房间"自动挑号"会挑进它里面去，
	// 两个房间都能起来，但玩家连不上后起的那个。
	for _, want := range []int{10998, 10999, 11000, 11001} {
		if _, ok := used[want]; !ok {
			t.Errorf("UsedPorts 漏了 %d，实际 %+v", want, used)
		}
	}
	if len(used) != 4 {
		t.Errorf("应该只记下这 4 个被占用的端口，实际 %+v", used)
	}
	ports, masterIntent, err := p.PickPorts(10998, 11900, 2)
	if err != nil {
		t.Fatalf("PickPorts: %v", err)
	}
	for _, got := range append(ports, masterIntent) {
		if _, busy := used[got]; busy {
			t.Errorf("挑到了已占用的端口 %d", got)
		}
	}
	if ports[0]+1 != masterIntent {
		t.Errorf("Master 端口与 master_port 不满足 +1 关系: %d / %d", ports[0], masterIntent)
	}

	c2, err := p.CreateCluster("Cluster_2", baseSettings(), 2, 10998, 11900)
	if err != nil {
		t.Fatal(err)
	}
	used2, _ := p.UsedPorts()
	if len(used2) != 8 {
		t.Errorf("两个房间各占四个号，应共 8 个: %+v", used2)
	}
	if c2.Shard(ShardMaster).ServerPort == 0 {
		t.Error("第二个集群没写端口")
	}

	// 区间被占满时必须失败，而不是返回一个冲突端口。
	if _, _, err := p.PickPorts(10998, 11001, 2); err == nil {
		t.Error("端口区间耗尽应报错")
	}
}

// TestCreateClusterAtPinsWantedPort 盯的是"用户自己填端口"这条路：
// 填进去的号必须真的落在 Master 上，并且 cluster.ini 与洞穴的意图端口跟着走，
// 否则玩家按这个号开安全组却连不上。
func TestCreateClusterAtPinsWantedPort(t *testing.T) {
	p := newTestPaths(t)
	c, err := p.CreateClusterAt("Cluster_1", baseSettings(), 2, 10998, 11900, 11500)
	if err != nil {
		t.Fatalf("按指定端口建集群: %v", err)
	}
	if got := c.Shard(ShardMaster).ServerPort; got != 11500 {
		t.Errorf("Master 端口应是用户填的 11500，实际 %d", got)
	}
	if got := c.MasterPort(); got != 11501 {
		t.Errorf("cluster.ini 的意图端口应是 11501，实际 %d", got)
	}
	caves := c.Shard(ShardCaves)
	if caves.ServerPort == 11500 || caves.ServerPort == 11501 {
		t.Errorf("洞穴端口撞上地表的两个号: %d", caves.ServerPort)
	}
	if caves.MasterPort != 11501 {
		t.Errorf("洞穴的 master_port 应指向 11501，实际 %d", caves.MasterPort)
	}
	// 区间外的号要收得下：建完之后那一页允许改成区间外的号，建的时候却拒，
	// 用户就只能先建一个再改一次，而他要的本来就是那一个号。
	if _, err := p.CreateClusterAt("Cluster_2", baseSettings(), 2, 10998, 11900, 12345); err != nil {
		t.Errorf("区间外的号应被接受（洞穴仍从区间里挑）：%v", err)
	}
	c3, err := OpenCluster(p, "Cluster_2")
	if err != nil {
		t.Fatalf("回读 Cluster_2: %v", err)
	}
	if got := c3.Shard(ShardCaves).ServerPort; got < 10998 || got > 11900 || got == 12345 || got == 12346 {
		t.Errorf("洞穴端口应落在面板区间里且不撞上地表那一组，实际 %d", got)
	}
	// 洞穴挑到 wantMaster-1 就等于和地表的配套号撞车，这一格也要挡住。
	if got := c3.Shard(ShardCaves).ServerPort; got == 12344 {
		t.Errorf("洞穴端口挑了 12344：它配套的号正是地表的 12345")
	}
}

// TestPickPortsAtRefusesBusyPorts 两种占用都要拦：号本身被占，以及号空着但它的 +1 被占。
// 后者最容易漏——那个号在游戏的服务器列表里根本不出现，用户想不通为什么不行。
func TestPickPortsAtRefusesBusyPorts(t *testing.T) {
	p := newTestPaths(t)
	if _, err := p.CreateCluster("Cluster_1", baseSettings(), 2, 10998, 11900); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.PickPortsAt(10998, 11900, 1, 10998); err == nil ||
		!strings.Contains(err.Error(), "占用") {
		t.Errorf("游戏端口被占应报错，实际 %v", err)
	}
	_, _, err := p.PickPortsAt(10998, 11900, 1, 10997)
	if err == nil || !strings.Contains(err.Error(), "意图端口") {
		t.Errorf("配套端口被占也应报错，实际 %v", err)
	}
	if !errors.Is(err, ErrInvalidArg) {
		t.Errorf("这类占用是调用方能改的，应包 ErrInvalidArg 让接口回 400: %v", err)
	}
}

// TestSetPortsKeepsThreeFilesInSync 是"建完还能改端口"的地基。
// 端口牵动三个文件，漏掉任何一个的症状都是"世界起得来但玩家连不上/洞穴连不上地面"，
// 而引擎一句错都不报，所以只能由这一层保证。
func TestSetPortsKeepsThreeFilesInSync(t *testing.T) {
	p := newTestPaths(t)
	c, err := p.CreateCluster("Cluster_1", baseSettings(), 2, 10998, 11900)
	if err != nil {
		t.Fatal(err)
	}
	before := c.PortPlan()
	keyBefore := c.Shard(ShardCaves).ClusterKey

	if err := c.SetPorts(12345, 12347); err != nil {
		t.Fatalf("SetPorts: %v", err)
	}
	fresh, err := OpenCluster(p, "Cluster_1")
	if err != nil {
		t.Fatal(err)
	}
	plan := fresh.PortPlan()
	if plan.Master != 12345 || plan.Intent != 12346 || plan.Caves != 12347 {
		t.Errorf("三个号没一起改过来: %+v（改之前 %+v）", plan, before)
	}
	if got := fresh.Shard(ShardCaves).MasterPort; got != 12346 {
		t.Errorf("洞穴 server.ini 的 master_port 没跟着变，实际 %d", got)
	}
	// 端口之外的东西不能被顺手改掉：洞穴的 cluster_key 和地面不一致时它会连不上。
	if got := fresh.Shard(ShardCaves).ClusterKey; got != keyBefore {
		t.Errorf("改端口把洞穴的 cluster_key 也改了: %q -> %q", keyBefore, got)
	}
	if !fresh.Shard(ShardMaster).IsMaster || fresh.Shard(ShardCaves).IsMaster {
		t.Error("is_master 被改掉了")
	}
	if got := fresh.Shard(ShardCaves).ID; got != 2 {
		t.Errorf("分片 id 应稳定为 2，实际 %d", got)
	}

	// 只改洞穴：地表的两个号必须原地不动。
	if err := fresh.SetPorts(0, 12350); err != nil {
		t.Fatalf("只改洞穴: %v", err)
	}
	again, _ := OpenCluster(p, "Cluster_1")
	plan = again.PortPlan()
	if plan.Master != 12345 || plan.Intent != 12346 || plan.Caves != 12350 {
		t.Errorf("只改洞穴时把地表也动了: %+v", plan)
	}
	if err := again.SetPorts(0, 0); err == nil {
		t.Error("一个号都不改时应拒绝，而不是白写一遍文件")
	}
}

func TestSettingsRoundTripWithEscapes(t *testing.T) {
	p := newTestPaths(t)
	c, err := p.CreateCluster("Cluster_1", baseSettings(), 1, 10998, 11000)
	if err != nil {
		t.Fatal(err)
	}
	want := c.Settings()
	if want.ClusterName != "我的服务器" {
		t.Errorf("集群名读回不对: %q", want.ClusterName)
	}
	if want.Password != `p\ass"w` {
		t.Errorf("密码转义往返丢了: %q", want.Password)
	}
	if want.Description != `欢迎 "来客"` {
		t.Errorf("描述往返丢了: %q", want.Description)
	}
	if want.MaxPlayers != 8 || want.GameMode != "survival" || want.Intention != "cooperative" {
		t.Errorf("基础项读回不对: %+v", want)
	}

	// 改密码后重新打开，值必须是新的且文件仍是合法 ini。
	s := want
	s.Password = `new"pass\`
	s.MaxPlayers = 12
	s.PVP = true
	c.SetSettings(s)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, err := OpenCluster(p, "Cluster_1")
	if err != nil {
		t.Fatal(err)
	}
	got := c2.Settings()
	if got.Password != `new"pass\` || got.MaxPlayers != 12 || !got.PVP {
		t.Errorf("保存后读回不对: %+v", got)
	}
	if got.VoteEnabled {
		t.Error("未提交的 vote_enabled 被打开了")
	}
}

func TestTokenValidateAndStore(t *testing.T) {
	p := newTestPaths(t)
	if _, err := p.CreateCluster("Cluster_1", baseSettings(), 1, 10998, 11000); err != nil {
		t.Fatal(err)
	}
	c, _ := OpenCluster(p, "Cluster_1")
	if tok, err := c.Token(); err != nil || tok != "" {
		t.Errorf("新建集群不该有令牌: %q %v", tok, err)
	}

	for _, bad := range []string{
		"",
		"short",
		"abcdef0123456789abcdef 0123",
		"token\nwith newline",
		"pds-g^带中文的令牌^abcdef0123456789",
		"pds-g^\x01abcdef0123456789abcdef",
		strings.Repeat("a", 600),
	} {
		if err := c.SetToken(bad); err == nil {
			t.Errorf("令牌 %q 本该被拒", bad)
		}
	}
	// 云机首启真实撞过一次：Klei 的令牌形态里就有 ^ / = + 这些不属于 base64url 的字符，
	// 早先按猜的白名单把它们拒了。这里钉住"不透明凭据不猜字符集"。
	for _, ok := range []string{
		"abcdefghijklmnopqrstuvwx",
		"pds-g^KU_tvX4ySmb^ab/cdEF+GHIJKLMNOpqrs==^u0qG7isngo",
	} {
		if err := ValidateToken(ok); err != nil {
			t.Errorf("合法令牌被拒: %q: %v", ok, err)
		}
	}
	good := "abcdefghijklmnopqrstuvwx"
	if err := c.SetToken(good + "\n"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	got, err := c.Token()
	if err != nil || got != good {
		t.Errorf("令牌读回不对: %q %v", got, err)
	}
	// 旧版本轮换令牌时会留下一份 .bak，里面是上一串明文；再存一次要顺手清掉。
	if err := os.WriteFile(p.TokenFile("Cluster_1")+".bak", []byte("old-token-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.SetToken("zyxwvutsrqponmlkjihgfedc"); err != nil {
		t.Fatalf("轮换令牌: %v", err)
	}
	if _, err := os.Stat(p.TokenFile("Cluster_1") + ".bak"); !os.IsNotExist(err) {
		t.Errorf("轮换令牌后仍留着旧令牌备份: %v", err)
	}
	// Windows 上 Go 拿不到 POSIX 权限位，只在类 Unix 上断言。
	if runtime.GOOS != "windows" {
		st, err := os.Stat(p.TokenFile("Cluster_1"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("cluster_token.txt 权限过宽: %v", perm)
		}
	}
}

func TestNextClusterNameAndOrdering(t *testing.T) {
	p := newTestPaths(t)
	if _, err := p.CreateCluster("Cluster_1", baseSettings(), 1, 10998, 11000); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateCluster("Cluster_2", baseSettings(), 1, 11002, 11010); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateCluster("Cluster_10", baseSettings(), 1, 11012, 11020); err != nil {
		t.Fatal(err)
	}
	names, err := p.ClusterNames()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "Cluster_1,Cluster_2,Cluster_10" {
		t.Errorf("集群没按数字序: %+v", names)
	}
	next, err := p.NextClusterName()
	if err != nil {
		t.Fatal(err)
	}
	if next != "Cluster_3" {
		t.Errorf("NextClusterName 该补最小空缺: %s", next)
	}
	// 只有 cluster.ini 的目录才算集群，残留空目录要忽略。
	if err := os.MkdirAll(p.ClusterDir("Cluster_7"), 0755); err != nil {
		t.Fatal(err)
	}
	names, _ = p.ClusterNames()
	if len(names) != 3 {
		t.Errorf("空目录被当成集群: %+v", names)
	}
}

func TestClusterDirSize(t *testing.T) {
	p := newTestPaths(t)
	if _, err := p.CreateCluster("Cluster_1", baseSettings(), 1, 10998, 11000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.SaveDir("Cluster_1", ShardMaster), "world"),
		make([]byte, 4096), 0644); err != nil {
		t.Fatal(err)
	}
	n, err := p.ClusterDirSize("Cluster_1")
	if err != nil {
		t.Fatal(err)
	}
	if n < 4096 {
		t.Errorf("存档体积算少了: %d", n)
	}
	if FormatBytes(4096) != "4.0 KB" {
		t.Errorf("FormatBytes: %s", FormatBytes(4096))
	}
	if FormatBytes(0) != "0 B" {
		t.Errorf("FormatBytes(0): %s", FormatBytes(0))
	}
}
