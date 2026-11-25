// 游戏端口接口的用例。这里盯的是三件在真机上会要命的事：
// 端口改完三份文件必须还是一个自洽的组合（洞穴靠 cluster.ini 那个号找地面）、
// 世界还在跑的时候不能写、被别的房间占着的号不能写。
//
// 和集群设置那组同样的原则：回复说得再好听，也要把 server.ini 打开看一眼。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"dstpanel/internal/dst"
)

// seedPortsRoom 建一个真带 server.ini 的房间。
// 夹具里那个 Cluster_1 只有 cluster.ini（它只用来满足"列表能看见这个房间"），
// 端口这件事必须有 server.ini 才谈得上。
func seedPortsRoom(t *testing.T, f *apiFixture, dir string, numShards, wantMaster int) {
	t.Helper()
	set := baseSettings()
	set.ClusterName = dir + "房"
	if _, err := f.paths.CreateClusterAt(dir, set, numShards, 10998, 11900, wantMaster); err != nil {
		t.Fatalf("建房间 %s: %v", dir, err)
	}
}

func TestPortsShowListsEveryNumberAndWhoOwnsIt(t *testing.T) {
	f := newFixture(t, "")
	seedPortsRoom(t, f, "Cluster_9", 2, 0)

	var v portsView
	decodeData(t, do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters/Cluster_9/ports", "admin", "secret"), &v)

	if v.Cluster != "Cluster_9" || v.MasterPort != 10998 || v.IntentPort != 10999 || v.CavesPort != 11000 {
		t.Errorf("三份文件里的号没读全: %+v", v)
	}
	if !v.HasCaves {
		t.Error("两世界房间要标出有洞穴，界面才知道该不该给洞穴那一栏")
	}
	// 自动号既不能等于他现在用的这个（等于建议他改回原样），也不能落在任何一个已占用的号上。
	if v.AutoPort <= 0 || v.AutoPort == v.MasterPort {
		t.Errorf("autoPort 该给一个真正空闲的号，实际 %d（现在用 %d）", v.AutoPort, v.MasterPort)
	}
	if v.Range != "10998-11900" || v.Path == "" {
		t.Errorf("没把默认区间和文件位置带回去: %+v", v)
	}
	if len(v.RunningWorlds) != 0 {
		t.Errorf("夹具里没有任何世界在跑，别报出在跑的: %+v", v.RunningWorlds)
	}

	byPort := map[int]portOwnerView{}
	for _, o := range v.Taken {
		byPort[o.Port] = o
	}
	// 每个世界占两个号：自己那个 + 引擎同时绑的配套号。只列前一个的话，
	// 用户填 10999 时面板会说"空闲"，而它正是地表 10998 的配套端口。
	for _, want := range []int{10998, 10999, 11000, 11001} {
		o, ok := byPort[want]
		if !ok {
			t.Fatalf("占用表里没有 %d: %+v", want, v.Taken)
		}
		if o.Room != "Cluster_9房" {
			t.Errorf("%d 的归属房间应是显示名，实际 %q", want, o.Room)
		}
		if o.World != "地表" && o.World != "洞穴" {
			t.Errorf("%d 的世界名露出码值了: %q", want, o.World)
		}
		if o.KindZh == "" {
			t.Errorf("%d 没说清是被当什么占的，用户看不懂为什么这个号不能用", want)
		}
	}
	if got := byPort[10999].KindZh; !strings.Contains(got, "配套") {
		t.Errorf("10999 应说明是配套端口，实际 %q", got)
	}
	if got := byPort[11001].KindZh; !strings.Contains(got, "配套") {
		t.Errorf("洞穴那个配套号（任何文件里都查不到）也要登记，实际 %q", got)
	}
	if !byPort[10998].Self {
		t.Error("本房间的号要标 self，界面才能把它和别人的号分开列")
	}
	// 建议号必须真空闲：UsedPorts 以前漏登记洞穴那个 +1，
	// 自动挑出来的号撞上它时，两个房间都起得来但玩家连不上。
	if _, busy := byPort[v.AutoPort]; busy {
		t.Errorf("autoPort %d 其实已经被占用: %+v", v.AutoPort, v.Taken)
	}
	// 目录名不能出现在界面文案里，但排查时要能对上文件：走 json:"-"。
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("编码视图: %v", err)
	}
	if strings.Contains(string(raw), `"dir"`) || strings.Contains(string(raw), "Cluster_9/") {
		t.Errorf("占用表把集群目录名递给了前端，界面上迟早会露出来: %s", raw)
	}
}

func TestPortPlanShowsSuggestedFreePortBeforeTheRoomExists(t *testing.T) {
	f := newFixture(t, "")
	seedPortsRoom(t, f, "Cluster_9", 2, 0) // 10998 / 10999 / 11000 / 11001

	var v portPlanView
	decodeData(t, do(t, f.srv.Handler(), http.MethodGet, "/api/v1/ports/plan?shards=2", "admin", "secret"), &v)
	if v.Shards != 2 || v.Range != "10998-11900" {
		t.Errorf("区间与世界数没带回去: %+v", v)
	}
	if v.AutoPort <= 0 || v.IntentPort != v.AutoPort+1 {
		t.Errorf("建议号要满足意图端口 = 游戏端口 +1，实际 %+v", v)
	}
	// 建议号不能落在任何一个已被占用的号上（含洞穴那个只在运行时才绑的 +1）。
	for _, o := range v.Taken {
		if o.Port == v.AutoPort || o.Port == v.IntentPort {
			t.Errorf("建议的 %d/%d 其实被 %s 的%s世界占着", v.AutoPort, v.IntentPort, o.Room, o.World)
		}
	}
	if len(v.Taken) != 4 {
		t.Errorf("这台机器上现在只有 4 个号被占着，实际 %+v", v.Taken)
	}
	for _, o := range v.Taken {
		if o.Self {
			t.Errorf("房间还没建，不该有任何号算它自己的: %+v", o)
		}
	}

	// 区间排不出号时也要给向导一条能走的路：不报 500，把原因写进 note。
	f.srv.Network.ShardPortEnd = 10999
	var tight portPlanView
	decodeData(t, do(t, f.srv.Handler(), http.MethodGet, "/api/v1/ports/plan?shards=2", "admin", "secret"), &tight)
	if tight.AutoPort != 0 || !strings.Contains(tight.Note, "端口") {
		t.Errorf("区间不够时要把原因说清楚，实际 %+v", tight)
	}

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/ports/plan?shards=3", "admin", "secret")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "世界数量") {
		t.Errorf("非法世界数要回 400 并说清，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

func TestPortsPutRefusedWhileWorldIsRunning(t *testing.T) {
	f := newFixture(t, "20314.dst-Cluster_9-Master\t(Detached)\n")
	seedPortsRoom(t, f, "Cluster_9", 2, 0)
	ini := readOrDie(t, f.paths.ClusterIni("Cluster_9"))

	rec := doJSON(t, f.srv.Handler(), http.MethodPut, "/api/v1/clusters/Cluster_9/ports",
		portsPatchRequest{MasterPort: iptr(12345)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("世界在跑时改端口要回 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
	// 说的是"地表"，不是 Master 这个码值。
	if !strings.Contains(rec.Body.String(), "地表") || !strings.Contains(rec.Body.String(), "停下来") {
		t.Errorf("拒绝理由要告诉用户先停哪个世界：%s", rec.Body.String())
	}
	if got := readOrDie(t, f.paths.ClusterIni("Cluster_9")); got != ini {
		t.Errorf("被拒的请求不该动 cluster.ini:\n%s", got)
	}
}

func TestPortsPutRefusesNumbersTakenByAnotherRoom(t *testing.T) {
	f := newFixture(t, "")
	seedPortsRoom(t, f, "Cluster_9", 2, 0)     // 10998 / 10999 / 11000 / 11001
	seedPortsRoom(t, f, "Cluster_8", 1, 11500) // 11500 / 11501
	ini := readOrDie(t, f.paths.ClusterIni("Cluster_8"))
	h := f.srv.Handler()

	cases := []struct {
		name string
		port int
		want string
	}{
		{"游戏端口撞了", 10998, "已被"},
		// 10997 本身空着，但它是 10998 的配套号：引擎两个号都会绑，只查前一个会放行一个连不上的组合。
		{"配套端口撞了", 10997, "配套端口"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_8/ports",
				portsPatchRequest{MasterPort: iptr(tc.port)})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应回 400，实际 %d：%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("拒绝理由里要带上 %q，实际 %s", tc.want, body)
			}
			if !strings.Contains(body, "Cluster_9房") {
				t.Errorf("要指名是哪个房间占着这个号，用户才知道去关哪个: %s", body)
			}
			if got := readOrDie(t, f.paths.ClusterIni("Cluster_8")); got != ini {
				t.Errorf("被拒的请求不该动 cluster.ini:\n%s", got)
			}
		})
	}
}

func TestPortsPutRewritesThreeFilesAndTellsFirewallNumbers(t *testing.T) {
	f := newFixture(t, "")
	seedPortsRoom(t, f, "Cluster_9", 2, 0)

	var v portsView
	decodeData(t, doJSON(t, f.srv.Handler(), http.MethodPut, "/api/v1/clusters/Cluster_9/ports",
		portsPatchRequest{MasterPort: iptr(12345), CavesPort: iptr(12400)}), &v)

	if v.MasterPort != 12345 || v.IntentPort != 12346 || v.CavesPort != 12400 {
		t.Errorf("保存后的回读没跟上: %+v", v)
	}
	// 三份文件一起才算改对：只写 server.ini 会留下一个静默连不上地面的洞穴。
	masterIni := readOrDie(t, f.paths.ClusterIni("Cluster_9"))
	if !strings.Contains(masterIni, "master_port = 12346") {
		t.Errorf("cluster.ini 的意图端口没跟着变:\n%s", masterIni)
	}
	shardM := readOrDie(t, f.paths.ShardIni("Cluster_9", dst.ShardMaster))
	if !strings.Contains(shardM, "server_port = 12345") {
		t.Errorf("地表的 server.ini 没写到新号:\n%s", shardM)
	}
	shardC := readOrDie(t, f.paths.ShardIni("Cluster_9", dst.ShardCaves))
	for _, want := range []string{"server_port = 12400", "master_port = 12346"} {
		if !strings.Contains(shardC, want) {
			t.Errorf("洞穴的 server.ini 缺 %q:\n%s", want, shardC)
		}
	}
	// 键名之外那两个值不能掉：cluster_key / is_master 是洞穴的身份。
	if !strings.Contains(shardC, "is_master = false") {
		t.Errorf("洞穴的 is_master 被改掉了:\n%s", shardC)
	}

	// 这一句是用户唯一能拿到"该在云上放行哪些号"的地方，四个号都得在里面。
	for _, want := range []string{"云安全组", "12345 和 12346", "12400 和 12401"} {
		if !strings.Contains(v.Note, want) {
			t.Errorf("提示缺 %q，实际 %q", want, v.Note)
		}
	}
	// 12345 在面板默认区间外：不说一句，以后自动挑号永远挑不到它，用户会以为坏了。
	if !strings.Contains(v.Note, "区间") {
		t.Errorf("区间外的号要提醒一句，实际 %q", v.Note)
	}
	// 这一次是真的换了号（和"只补文件里缺的那一行"相对），要说按新号监听。
	if !strings.Contains(v.Note, "新号") {
		t.Errorf("换了号就要说下次按新号监听，实际 %q", v.Note)
	}

	entries, err := f.srv.Store.ListAudit(10, 0, "cluster.ports")
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("改端口该留一条操作记录，实际 %d 条", len(entries))
	}
	if !strings.Contains(entries[0].Detail, "12345") || !strings.Contains(entries[0].Detail, "12400") {
		t.Errorf("审计要记下新旧号，实际 %q", entries[0].Detail)
	}
	if _, err := os.Stat(f.paths.ClusterIni("Cluster_9") + ".bak"); err != nil {
		t.Errorf("写 cluster.ini 应该留 .bak: %v", err)
	}
}

func TestPortsPutRejectsNumbersKleiWouldNotBind(t *testing.T) {
	f := newFixture(t, "")
	seedPortsRoom(t, f, "Cluster_9", 2, 0)
	ini := readOrDie(t, f.paths.ClusterIni("Cluster_9"))
	shard := readOrDie(t, f.paths.ShardIni("Cluster_9", dst.ShardMaster))
	h := f.srv.Handler()

	cases := []struct {
		name string
		req  portsPatchRequest
		want string
	}{
		{"太小", portsPatchRequest{MasterPort: iptr(80)}, "1024"},
		{"零也挡掉", portsPatchRequest{MasterPort: iptr(0)}, "1024"},
		{"顶到上界外", portsPatchRequest{MasterPort: iptr(65535)}, "65534"},
		{"洞穴和地表同号", portsPatchRequest{CavesPort: iptr(10998)}, "会压到同一个房间另一个世界的"},
		{"洞穴占了地表的配套号", portsPatchRequest{CavesPort: iptr(10999)}, "会压到同一个房间另一个世界的"},
		// 反方向那一种：洞穴 10997 的配套号是 10998，正好压在地表的游戏端口上。
		{"洞穴的配套号占了地表", portsPatchRequest{CavesPort: iptr(10997)}, "会压到同一个房间另一个世界的"},
		// 反过来填也一样要拦：地表改成 11001，它的配套号 11002 没事，但 11001 是洞穴 11000 的配套号。
		{"地表占了洞穴的配套号", portsPatchRequest{MasterPort: iptr(11001)}, "会压到同一个房间另一个世界的"},
		{"洞穴太小", portsPatchRequest{CavesPort: iptr(99)}, "1024"},
		{"什么都没填", portsPatchRequest{}, "没有收到要改的端口"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_9/ports", tc.req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应回 400，实际 %d：%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("拒绝理由里要带上 %q，实际 %s", tc.want, rec.Body.String())
			}
			if got := readOrDie(t, f.paths.ClusterIni("Cluster_9")); got != ini {
				t.Errorf("被拒的请求不该动 cluster.ini:\n%s", got)
			}
			if got := readOrDie(t, f.paths.ShardIni("Cluster_9", dst.ShardMaster)); got != shard {
				t.Errorf("被拒的请求不该动 server.ini:\n%s", got)
			}
		})
	}
}

func TestPortsPutWithNoRealChangeLeavesFilesAlone(t *testing.T) {
	f := newFixture(t, "")
	seedPortsRoom(t, f, "Cluster_9", 2, 0)
	paths := []string{
		f.paths.ClusterIni("Cluster_9"),
		f.paths.ShardIni("Cluster_9", dst.ShardMaster),
		f.paths.ShardIni("Cluster_9", dst.ShardCaves),
	}
	before := make([]string, len(paths))
	for i, p := range paths {
		before[i] = readOrDie(t, p)
	}

	// 把当前这个号再填一遍：不能报"被自己占用"，也不该重写文件。
	var v portsView
	decodeData(t, doJSON(t, f.srv.Handler(), http.MethodPut, "/api/v1/clusters/Cluster_9/ports",
		portsPatchRequest{MasterPort: iptr(10998)}), &v)
	if !strings.Contains(v.Note, "没有变化") {
		t.Errorf("没改动要说清楚，实际 %q", v.Note)
	}
	for i, p := range paths {
		if got := readOrDie(t, p); got != before[i] {
			t.Errorf("%s 被白重写了一遍:\n%s", p, got)
		}
		if _, err := os.Stat(p + ".bak"); err == nil {
			t.Errorf("%s 留下了 .bak，说明这次真的写过文件", p)
		}
	}
}

// 配套端口那一格读不出号（手工造的房间、或者早期版本漏写这一行）时，
// "地表端口没变"不能成为不写的理由：这一页既然把 0 列出来了，就得能把它修好。
// 洞穴穿不过地面是静默失败——引擎起得来、面板上一切正常，只有玩家连不进去。
func TestPortsPutRepairsAMissingIntentPort(t *testing.T) {
	f := newFixture(t, "")
	seedPortsRoom(t, f, "Cluster_9", 2, 0)
	iniPath := f.paths.ClusterIni("Cluster_9")
	raw := readOrDie(t, iniPath)
	broken := strings.Replace(raw, "master_port = 10999\n", "", 1)
	if broken == raw {
		t.Fatalf("cluster.ini 里没找到那行配套端口，改不出坏样本:\n%s", raw)
	}
	if err := os.WriteFile(iniPath, []byte(broken), 0o600); err != nil {
		t.Fatalf("写坏 cluster.ini: %v", err)
	}

	var v portsView
	decodeData(t, doJSON(t, f.srv.Handler(), http.MethodPut, "/api/v1/clusters/Cluster_9/ports",
		portsPatchRequest{MasterPort: iptr(10998)}), &v) // 填的还是他现在这个号
	if !strings.Contains(v.Note, "补上配套端口 10999") {
		t.Errorf("这次写入要说成补配套端口，实际 %q", v.Note)
	}
	// 一个号都没动就不能说"按新号监听"：那句会把人送去安全组加一个根本不存在的端口。
	if strings.Contains(v.Note, "新号") {
		t.Errorf("补写没换号，别说按新号监听: %q", v.Note)
	}
	if !strings.Contains(v.Note, "重新启动") {
		t.Errorf("要说明这一行是启动时读的: %q", v.Note)
	}
	if v.IntentPort != 10999 {
		t.Errorf("回读没跟上: %+v", v)
	}
	if got := readOrDie(t, iniPath); !strings.Contains(got, "master_port = 10999") {
		t.Errorf("cluster.ini 里那行没补回来:\n%s", got)
	}
	if got := readOrDie(t, f.paths.ShardIni("Cluster_9", dst.ShardCaves)); !strings.Contains(got, "master_port = 10999") {
		t.Errorf("洞穴 server.ini 的配套端口没跟着补:\n%s", got)
	}
}

// 反过来的坏法：cluster.ini 是对的，只有洞穴那一行被改歪。
// 只看 cluster.ini 的话这次会被判成"没有变化"，而洞穴照样连不上地面。
func TestPortsPutRepairsOnlyTheCavesIntentLine(t *testing.T) {
	f := newFixture(t, "")
	seedPortsRoom(t, f, "Cluster_9", 2, 0)
	cavesPath := f.paths.ShardIni("Cluster_9", dst.ShardCaves)
	raw := readOrDie(t, cavesPath)
	broken := strings.Replace(raw, "master_port = 10999", "master_port = 11555", 1)
	if broken == raw {
		t.Fatalf("洞穴 server.ini 里没找到那行配套端口:\n%s", raw)
	}
	if err := os.WriteFile(cavesPath, []byte(broken), 0o600); err != nil {
		t.Fatalf("写坏洞穴 server.ini: %v", err)
	}

	var v portsView
	decodeData(t, doJSON(t, f.srv.Handler(), http.MethodPut, "/api/v1/clusters/Cluster_9/ports",
		portsPatchRequest{MasterPort: iptr(10998)}), &v)
	if !strings.Contains(v.Note, "洞穴 server.ini") {
		t.Errorf("要指名补的是洞穴那一份文件，实际 %q", v.Note)
	}
	if got := readOrDie(t, cavesPath); !strings.Contains(got, "master_port = 10999") {
		t.Errorf("洞穴那行没修回来:\n%s", got)
	}
}

// 洞穴只写了端口、没写"去哪找地表"：症状和配套端口写错一模一样（洞穴起得来、进不去地面），
// 而引擎同样一句错都不报。这一格是端口页在"三个号对齐"之外顺手能修好的毛病，
// 所以它既要看得见（读接口报出来），也要修得掉（写入补 127.0.0.1）。
func TestPortsPutFillsCavesMasterAddress(t *testing.T) {
	f := newFixture(t, "")
	seedPortsRoom(t, f, "Cluster_9", 2, 0)
	cavesPath := f.paths.ShardIni("Cluster_9", dst.ShardCaves)
	raw := readOrDie(t, cavesPath)
	broken := strings.Replace(raw, "master_ip = 127.0.0.1\n", "", 1)
	if broken == raw {
		t.Fatalf("洞穴 server.ini 里没找到 master_ip 那行，改不出坏样本:\n%s", raw)
	}
	if err := os.WriteFile(cavesPath, []byte(broken), 0o600); err != nil {
		t.Fatalf("写坏洞穴 server.ini: %v", err)
	}

	h := f.srv.Handler()
	var shown portsView
	decodeData(t, doJSON(t, h, http.MethodGet, "/api/v1/clusters/Cluster_9/ports", nil), &shown)
	if shown.CavesMasterIP != "" {
		t.Errorf("读接口要如实报出\"主机地址空着\"，实际 %q", shown.CavesMasterIP)
	}

	var v portsView
	decodeData(t, doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_9/ports",
		portsPatchRequest{MasterPort: iptr(10998)}), &v) // 填的还是他现在这个号
	if !strings.Contains(v.Note, "主机地址") {
		t.Errorf("这次写入要说成补主机地址，实际 %q", v.Note)
	}
	if strings.Contains(v.Note, "新号") {
		t.Errorf("一个号都没换，别说按新号监听: %q", v.Note)
	}
	if got := readOrDie(t, cavesPath); !strings.Contains(got, "master_ip = 127.0.0.1") {
		t.Errorf("洞穴的主机地址没补上:\n%s", got)
	}
	if v.CavesMasterIP != "127.0.0.1" {
		t.Errorf("回读没跟上: %+v", v)
	}
}
