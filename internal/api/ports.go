// 游戏端口：看一个房间现在用哪几个号、改这些号。
//
// 为什么单独一个接口而不是塞进 cluster settings：改端口和改人数不是一类动作。
// 人数改完重启就完事，端口改错了是"玩家连不上、面板还显示着旧地址"，
// 而且它牵动三个文件（cluster.ini 的意图端口 + 两个分片的 server.ini），必须一次写全。
// 分开之后这一份能把"改之前先停服""安全组要放行新号"讲清楚。
//
// 为什么端口要让用户自己填：饥荒加入房间靠的是 IP:端口，一台机器上放好几个房间是常态，
// 面板自动挑的号他不知道要在安全组放行哪个；自己填一个熟号（比如 10999）省掉一整轮沟通。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"dstpanel/internal/dst"
)

// 端口下界取 1024：以下要 root 权限，云服务器上面板通常不是 root 跑的。
// 上界取 65534：每个世界还要占一个 +1 的配套端口（引擎实测会同时绑这两个 UDP 号）。
const (
	minGamePort = 1024
	maxGamePort = 65534
)

type portOwnerView struct {
	Port int    `json:"port"`
	Dir  string `json:"-"`
	Room string `json:"room"`
	// World 已经是中文（地表 / 洞穴）：这一份要直接进界面文案，
	// 把 Master/Caves 码值递出去就得在 JS 里再翻一次，两边迟早对不上。
	World string `json:"world"`
	Kind  string `json:"kind"`
	// KindZh 说清"这个号是被当成什么占掉的"。意图端口不出现在游戏里，
	// 用户看不到它就理解不了"为什么 11000 也不能填"。
	KindZh string `json:"kindLabel"`
	Self   bool   `json:"self"`
}

type portsView struct {
	Cluster    string `json:"cluster"`
	MasterPort int    `json:"masterPort"`
	IntentPort int    `json:"intentPort"`
	CavesPort  int    `json:"cavesPort"`
	HasCaves   bool   `json:"hasCaves"`
	// CavesMasterIP 是洞穴 server.ini 里"去哪找地表"那一行。空着的话光补端口没用，
	// 洞穴照样连不上地面，而引擎一句错都不报，所以这一格得让界面看得见。
	CavesMasterIP string `json:"cavesMasterIp"`
	// AutoPort 是"这一栏交给面板时会挑中的号"：界面拿它当占位提示，
	// 用户就能在"自己填"和"交给面板"之间做决定，而不是对着空框猜。
	AutoPort int             `json:"autoPort"`
	Range    string          `json:"autoRange"`
	Taken    []portOwnerView `json:"taken"`
	// RunningWorlds 非空时改端口会被拒：端口只在世界启动时读一次，
	// 正在跑的进程还守着旧号，这时候写新号只会让面板显示的加入地址变成假的。
	RunningWorlds []string `json:"runningWorlds"`
	Note          string   `json:"note,omitempty"`
	Path          string   `json:"path"`
}

// portOwners 扫全部集群，把每个被占用的号落到"哪个房间的哪个世界、当什么用"。
//
// 没有直接用 dst.Paths.UsedPorts：它给的是文件视角的 "Cluster_1/Master server_port"，
// 那是我们排查时想要的字符串，不是给客户看的；而且它漏掉了洞穴那个 +1 端口
// （引擎每个世界都会同时绑 server_port 和 server_port+1，那个号同样是占掉的）。
func (s *Server) portOwners() ([]portOwnerView, error) {
	names, err := s.Paths.ClusterNames()
	if err != nil {
		return nil, err
	}
	var out []portOwnerView
	add := func(port int, dir, room, world, kind string) {
		if port <= 0 {
			return
		}
		out = append(out, portOwnerView{Port: port, Dir: dir, Room: room, World: world, Kind: kind})
	}
	for _, n := range names {
		c, err := dst.OpenCluster(s.Paths, n)
		if err != nil {
			continue
		}
		room := firstNonEmpty(c.Settings().ClusterName, n)
		for _, sh := range s.Paths.ShardNames(n) {
			world := worldNameOf(sh)
			sp := c.Shard(sh).ServerPort
			// 没写到端口的世界（server.ini 缺失或坏掉）不登记：
			// 把 0 记成占用会把 "0 和 1" 递进界面，而它其实谁也没占。
			if sp <= 0 {
				continue
			}
			add(sp, n, room, world, "game")
			add(sp+1, n, room, world, "intent")
		}
		if mp := c.MasterPort(); mp > 0 {
			add(mp, n, room, worldNameOf(dst.ShardMaster), "intent")
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Kind < out[j].Kind
	})
	// 同一个号会被登记两次（洞穴的 +1 和 cluster.ini 的意图端口都指向它），
	// 界面上只要一句"这个号被谁占着"，重复的丢掉。
	dedup := out[:0]
	seen := map[int]bool{}
	for _, t := range out {
		if seen[t.Port] {
			continue
		}
		seen[t.Port] = true
		dedup = append(dedup, t)
	}
	for i := range dedup {
		if dedup[i].Kind == "intent" {
			dedup[i].KindZh = "配套端口（游戏里看不到这个号，但它同样被占着）"
		} else {
			dedup[i].KindZh = "游戏端口"
		}
	}
	return emptyIfNil(dedup), nil
}

func (s *Server) buildPortsView(cluster string) (portsView, error) {
	c, err := dst.OpenCluster(s.Paths, cluster)
	if err != nil {
		return portsView{}, err
	}
	plan := c.PortPlan()
	numShards := 1
	if plan.HasCaves {
		numShards = 2
	}
	v := portsView{
		Cluster: cluster, MasterPort: plan.Master, IntentPort: plan.Intent,
		CavesPort: plan.Caves, HasCaves: plan.HasCaves,
		CavesMasterIP: c.Shard(dst.ShardCaves).MasterIP,
		Taken:         []portOwnerView{},
		Range:         fmt.Sprintf("%d-%d", s.Network.ShardPortBase, s.Network.ShardPortEnd),
		Path:          s.Paths.ClusterIni(cluster),
	}
	// 自动端口取"假如现在重新挑一个"的结果。本房间自己的号在 UsedPorts 里也是占用，
	// 所以这个建议一定落得了地，不会建议用户改回他现在这个号。
	if ports, _, err := s.Paths.PickPorts(s.Network.ShardPortBase, s.Network.ShardPortEnd, numShards); err == nil {
		v.AutoPort = ports[0]
	}
	taken, err := s.portOwners()
	if err != nil {
		return v, err
	}
	for _, t := range taken {
		t.Self = t.Dir == cluster
		v.Taken = append(v.Taken, t)
	}
	return v, nil
}

// portPlanView 是开服向导要的那份"还没建房时能知道的事"。
// 房间的端口接口按目录读，新建时目录还不存在，所以这一份只报区间、
// 面板会挑中的号，以及这台机器上已经被别的房间占走的号。
type portPlanView struct {
	Shards     int             `json:"shards"`
	AutoPort   int             `json:"autoPort"`
	IntentPort int             `json:"intentPort"`
	Range      string          `json:"autoRange"`
	Taken      []portOwnerView `json:"taken"`
	Note       string          `json:"note,omitempty"`
}

func (s *Server) handlePortPlan(w http.ResponseWriter, r *http.Request) {
	numShards := 1
	raw := r.URL.Query().Get("shards")
	if raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 2 {
			writeErr(w, http.StatusBadRequest, "世界数量只支持 1 或 2，收到「"+raw+"」")
			return
		}
		numShards = n
	}
	v := portPlanView{Shards: numShards, Range: fmt.Sprintf("%d-%d", s.Network.ShardPortBase, s.Network.ShardPortEnd)}
	// 挑不出号时不报错：向导还能用"用户自己填号"这条路，
	// 把原因说清楚比给一个 500 更有用。
	ports, intent, err := s.Paths.PickPorts(s.Network.ShardPortBase, s.Network.ShardPortEnd, numShards)
	if err != nil {
		v.Note = publicError(err)
	} else {
		v.AutoPort, v.IntentPort = ports[0], intent
	}
	taken, err := s.portOwners()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	v.Taken = taken
	writeOK(w, v)
}

func (s *Server) handlePortsShow(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	v, err := s.buildPortsViewFor(r, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, v)
}

// buildPortsViewFor 是带请求上下文的版本：正在跑的世界要问 screen，而 screen 要 ctx。
func (s *Server) buildPortsViewFor(r *http.Request, cluster string) (portsView, error) {
	v, err := s.buildPortsView(cluster)
	if err != nil {
		return v, err
	}
	running, err := s.runningShards(r.Context(), cluster)
	if err != nil {
		v.RunningWorlds = nil
		return v, nil
	}
	v.RunningWorlds = running
	return v, nil
}

type portsPatchRequest struct {
	MasterPort *int `json:"masterPort"`
	CavesPort  *int `json:"cavesPort"`
}

func (s *Server) handlePortsPut(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	var req portsPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.MasterPort == nil && req.CavesPort == nil {
		writeErr(w, http.StatusBadRequest, "没有收到要改的端口；要改哪个就带哪个字段")
		return
	}
	c, err := dst.OpenCluster(s.Paths, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	before := c.PortPlan()
	master, caves := before.Master, before.Caves
	if req.MasterPort != nil {
		master = *req.MasterPort
	}
	if req.CavesPort != nil {
		caves = *req.CavesPort
	}

	// 先挡住"正在跑还改端口"：端口只在世界启动时读一次，改完进程仍守着旧号，
	// 而面板这时候给出的加入地址、复制的连接命令都会变成新号——那是连不上的。
	running, err := s.runningShards(r.Context(), cluster)
	if err != nil {
		// 问不到进程状态时不放行：这条判断是端口改动的唯一护栏，
		// 拿"看不见"当"没有"会把玩家的房间改到连不上。
		writeErr(w, http.StatusInternalServerError,
			"问不到世界进程状态（面板需要和服务端同机、PATH 里有 screen），为了不改坏可能正在跑的房间，这次没有写入端口")
		return
	}
	if len(running) > 0 {
		named := make([]string, 0, len(running))
		for _, sh := range running {
			named = append(named, worldNameOf(sh))
		}
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"%s 正在运行，端口要先把这个世界停下来才能改。"+
				"正在跑的世界还守着旧号，这时候改完面板显示的加入地址就是假的",
			strings.Join(named, "、")))
		return
	}

	taken, err := s.portOwners()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 本房间自己占的那几个号要排除：把 10999 改成 10999 不该报"被自己占用"。
	occupied := map[int]string{}
	for _, t := range taken {
		if t.Dir == cluster {
			continue
		}
		occupied[t.Port] = t.Room + " 的" + t.World + "世界"
	}
	// 两个世界各绑两个号：{master, master+1} 与 {caves, caves+1}，两组不能重叠。
	// 把对方那一组当 peer 一起查，是因为"谁改成谁的号"是同一个错误：
	// 只查洞穴那一次会漏掉"地表改成洞穴的配套号"这种反过来的填法，
	// 而那种组合引擎两边都起得来，玩家连进去的是谁全看运气。
	// 没有洞穴的房间 caves 读回来就是 0，checkPort 见到 0 自然跳过这一段。
	if err := checkPort("地表世界的游戏端口", master, occupied, caves); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if before.HasCaves {
		if err := checkPort("洞穴世界的游戏端口", caves, occupied, master); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	what := make([]string, 0, 2)
	if master != before.Master {
		what = append(what, fmt.Sprintf("地表 %d→%d（配套端口跟着变成 %d）", before.Master, master, master+1))
	}
	if before.HasCaves && caves != before.Caves {
		what = append(what, fmt.Sprintf("洞穴 %d→%d", before.Caves, caves))
	}
	// 配套端口那一行自己就是坏的，也算一次改动：手工造出来的房间、或者早期版本漏写这一格，
	// 症状是"洞穴起得来但穿不过去"，而这一页显示给用户的只有"配套端口 0"。
	// 不把它记进 what，下面的"没有变化就短路"会把这次写入整个跳过，
	// 于是这一页列出来的毛病它自己修不了。
	// 只在号没变的时候单独报：改了地表端口，上面那句已经说了配套端口跟到哪个号，
	// 再列一遍就是同一件事说两次。
	if master == before.Master {
		cavesShard := c.Shard(dst.ShardCaves)
		if fix := intentFixNote(master, before, cavesShard.MasterPort, cavesShard.MasterIP); fix != "" {
			what = append(what, fix)
		}
	}
	v, err := s.buildPortsViewFor(r, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 一个号都没变就到此为止：SetPorts 就算值相同也会把 cluster.ini 和洞穴的
	// server.ini 各写一遍（还要留 .bak）。点了"保存"什么都没改是常见动作，
	// 白重写文件并把"文件未重写"说成假话都不该发生。
	if len(what) == 0 {
		v.Note = "端口没有变化，文件未重写"
		writeOK(w, v)
		return
	}

	if err := c.SetPorts(master, caves); err != nil {
		if errors.Is(err, dst.ErrInvalidArg) {
			writeErr(w, http.StatusBadRequest, publicError(err))
			return
		}
		s.audit(r, "cluster.ports", cluster, "写入失败", err)
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 回读要在写盘之后：直接把改前那份视图发回去，界面会显示旧号，
	// 用户以为没保存成功，接着又点一次。
	v, err = s.buildPortsViewFor(r, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "cluster.ports", cluster, "改了："+strings.Join(what, "、"), nil)

	// 只补了文件里缺的那一行时别说"按新号监听"：一个号都没动，
	// 那句会让人以为要去安全组再加一个新端口。
	tail := "下次启动世界就按新号监听，加入命令里的端口会自动跟着变"
	if master == before.Master && (!before.HasCaves || caves == before.Caves) {
		tail = "号都没变，补的是文件里缺的那一行；把世界重新启动一次就生效"
	}
	v.Note = fmt.Sprintf("已写入：%s。防火墙和云安全组要放行的 UDP 号是 %s；%s",
		strings.Join(what, "、"), openPortsNote(master, caves, before.HasCaves), tail)
	if master < s.Network.ShardPortBase || master > s.Network.ShardPortEnd {
		v.Note += fmt.Sprintf("。这个号在面板默认区间 %s 之外，面板自动挑端口时不会挑到它，用着没问题但别忘了它归你管", v.Range)
	}
	writeOK(w, v)
}

// intentFixNote 判断"配套端口"那一格要不要补，并给出一句人话。
//
// 两处文件都要等于地表端口 + 1：cluster.ini 的 [SHARD] master_port，和洞穴 server.ini
// 里同名的那一行。后者更隐蔽——cluster.ini 是对的、洞穴照样连不上地面，而引擎不报错。
// 洞穴那一份还牵一个端口之外的东西：master_ip 空着时补端口没用，所以一并说出来，
// 写盘那一步（SetPorts）会把它补成 127.0.0.1。
// 返回空串表示这一格本来就是好的。
func intentFixNote(master int, plan dst.PortPlan, cavesIntent int, cavesIP string) string {
	if master <= 0 {
		return ""
	}
	want := master + 1
	clauses := make([]string, 0, 2)
	switch {
	case plan.Intent <= 0 && (!plan.HasCaves || cavesIntent == want):
		clauses = append(clauses, fmt.Sprintf("补上配套端口 %d（cluster.ini 里缺这一行，洞穴会找不到地表）", want))
	case plan.Intent != want:
		clauses = append(clauses, fmt.Sprintf("配套端口 %d→%d（这一行必须等于地表端口加一，洞穴靠它找地表）", plan.Intent, want))
	case plan.HasCaves && cavesIntent != want:
		clauses = append(clauses, fmt.Sprintf("把洞穴 server.ini 里的配套端口补成 %d（它要和 cluster.ini 那一行对得上）", want))
	}
	if plan.HasCaves && strings.TrimSpace(cavesIP) == "" {
		clauses = append(clauses, "给洞穴补上主机地址 127.0.0.1（它的 server.ini 里没写去哪找地表，光有端口照样连不上）")
	}
	return strings.Join(clauses, "、")
}

// worldNameOf 把分片目录名说成客户的话。只有两种，不值得再查一次 worldgen 的中文名。
func worldNameOf(shard string) string {
	if shard == dst.ShardCaves {
		return "洞穴"
	}
	return "地表"
}

// openPortsNote 列出要在防火墙/云安全组放行的 UDP 号。
// 没洞穴的房间不能带出洞穴那两个号，"0 和 1"这种话一旦进文案就没人能看懂。
func openPortsNote(master, caves int, hasCaves bool) string {
	if hasCaves {
		return fmt.Sprintf("%d 和 %d（地表一组）、%d 和 %d（洞穴一组）", master, master+1, caves, caves+1)
	}
	return fmt.Sprintf("%d 和 %d", master, master+1)
}

// checkPort 管单号合法性、被别的房间占用、以及撞上同一个房间另一个世界。
// 除了游戏端口本身，还要查它 +1 那个配套号：引擎每个世界会同时绑这两个，
// 只查前一个会放行"游戏端口空着、配套号撞车"这种起得来但连不上的组合。
// peer>0 表示另一个世界的游戏端口，它连带自己的配套号也不能碰。
func checkPort(label string, port int, occupied map[int]string, peer int) error {
	if port < minGamePort || port > maxGamePort {
		return fmt.Errorf("%s要填 %d 到 %d 之间的数字，收到 %d；1024 以下需要 root 权限，最后一个号要留给配套端口",
			label, minGamePort, maxGamePort, port)
	}
	checks := []int{port, port + 1}
	for _, p := range checks {
		if who, busy := occupied[p]; busy {
			which := "游戏端口"
			if p != port {
				which = "配套端口"
			}
			return fmt.Errorf("%s %d 的%s %d 已被 %s 占用，换一组号", label, port, which, p, who)
		}
		if peer > 0 && (p == peer || p == peer+1) {
			which := "游戏端口"
			if p != peer {
				which = "配套端口"
			}
			return fmt.Errorf("%s %d 会压到同一个房间另一个世界的%s %d，两个世界要各占一组不重叠的号",
				label, port, which, p)
		}
	}
	return nil
}
