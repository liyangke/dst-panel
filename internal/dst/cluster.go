package dst

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// 集群/分片配置层。
//
// DST 的多世界（地面+洞穴）靠两份 ini 联动，规则很容易被写错且游戏只会静默失败：
//   - cluster.ini 的 [SHARD] master_port 必须等于 Master 分片的 server_port + 1，
//     洞穴分片靠它找地面，错了就是"洞穴连不上地面"。
//   - 每个分片的 [SHARD] id 必须互不相同，重复时游戏能起，但玩家会被送到错误的世界。
//   - cluster_token.txt 缺失或带空白字符，服务器会拒绝上线而不是报好读的错。
// 这一层把这三件事变成"面板保证正确"，而不是"用户抄文档抄对"。

const (
	FieldGameMode      = "game_mode"
	FieldMaxPlayers    = "max_players"
	FieldPVP           = "pvp"
	FieldPauseEmpty    = "pause_when_empty"
	FieldClusterName   = "cluster_name"
	FieldDescription   = "cluster_description"
	FieldPassword      = "cluster_password"
	FieldIntention     = "cluster_intention"
	FieldLanguage      = "cluster_language"
	FieldTickRate      = "tick_rate"
	FieldWhitelistSlot = "whitelist_slots"
)

// GameSettings 是 cluster.ini 里面板会改的那些项。
type GameSettings struct {
	ClusterName     string
	Description     string
	Password        string
	GameMode        string // survival / endless / wilderness
	Intention       string // cooperative / competitive / social / madness
	MaxPlayers      int
	PVP             bool
	PauseWhenEmpty  bool
	VoteEnabled     bool
	VoteKickEnabled bool
	LanOnly         bool
	OfflineCluster  bool
	Language        string
	TickRate        int
	WhitelistSlots  int
	ConsoleEnabled  bool
	MaxSnapshots    int
}

// ShardSettings 是某个分片 server.ini 的镜像。
type ShardSettings struct {
	ServerPort int
	IsMaster   bool
	Name       string
	ID         int
	// MasterIP/MasterPort 只有非 Master 分片需要；Master 分片留空。
	MasterIP   string
	MasterPort int
	ClusterKey string
}

// Cluster 持有一个集群的 cluster.ini 解析结果。
type Cluster struct {
	Name  string
	Paths Paths
	Ini   *IniFile
	raw   string
}

func OpenCluster(p Paths, name string) (*Cluster, error) {
	ini, raw, err := LoadIni(p.ClusterIni(name))
	if err != nil {
		return nil, err
	}
	return &Cluster{Name: name, Paths: p, Ini: ini, raw: raw}, nil
}

func (c *Cluster) Raw() string { return c.raw }

func (c *Cluster) Settings() GameSettings {
	i := c.Ini
	return GameSettings{
		ClusterName:     i.Unquote("[NETWORK]", FieldClusterName),
		Description:     i.Unquote("[NETWORK]", FieldDescription),
		Password:        i.Unquote("[NETWORK]", FieldPassword),
		GameMode:        i.Unquote("[GAMEPLAY]", FieldGameMode),
		Intention:       i.Unquote("[NETWORK]", FieldIntention),
		MaxPlayers:      i.GetInt("[GAMEPLAY]", FieldMaxPlayers, 6),
		PVP:             i.GetBool("[GAMEPLAY]", FieldPVP, false),
		PauseWhenEmpty:  i.GetBool("[GAMEPLAY]", FieldPauseEmpty, true),
		VoteEnabled:     i.GetBool("[GAMEPLAY]", "vote_enabled", false),
		VoteKickEnabled: i.GetBool("[GAMEPLAY]", "vote_kick_enabled", false),
		LanOnly:         i.GetBool("[NETWORK]", "lan_only_cluster", false),
		OfflineCluster:  i.GetBool("[NETWORK]", "offline_cluster", false),
		Language:        i.Unquote("[NETWORK]", FieldLanguage),
		TickRate:        i.GetInt("[NETWORK]", FieldTickRate, 15),
		WhitelistSlots:  i.GetInt("[NETWORK]", FieldWhitelistSlot, 0),
		ConsoleEnabled:  i.GetBool("[MISC]", "console_enabled", true),
		MaxSnapshots:    i.GetInt("[MISC]", "max_snapshots", 20),
	}
}

func (c *Cluster) SetSettings(s GameSettings) {
	i := c.Ini
	i.SetString("[NETWORK]", FieldClusterName, s.ClusterName)
	i.SetString("[NETWORK]", FieldDescription, s.Description)
	i.SetString("[NETWORK]", FieldPassword, s.Password)
	i.SetString("[GAMEPLAY]", FieldGameMode, s.GameMode)
	i.SetString("[NETWORK]", FieldIntention, s.Intention)
	i.SetInt("[GAMEPLAY]", FieldMaxPlayers, s.MaxPlayers)
	i.SetBool("[GAMEPLAY]", FieldPVP, s.PVP)
	i.SetBool("[GAMEPLAY]", FieldPauseEmpty, s.PauseWhenEmpty)
	i.SetBool("[GAMEPLAY]", "vote_enabled", s.VoteEnabled)
	i.SetBool("[GAMEPLAY]", "vote_kick_enabled", s.VoteKickEnabled)
	i.SetBool("[NETWORK]", "lan_only_cluster", s.LanOnly)
	i.SetBool("[NETWORK]", "offline_cluster", s.OfflineCluster)
	if s.Language != "" {
		i.SetString("[NETWORK]", FieldLanguage, s.Language)
	}
	if s.TickRate > 0 {
		i.SetInt("[NETWORK]", FieldTickRate, s.TickRate)
	}
	i.SetInt("[NETWORK]", FieldWhitelistSlot, s.WhitelistSlots)
	i.SetBool("[MISC]", "console_enabled", s.ConsoleEnabled)
	if s.MaxSnapshots > 0 {
		i.SetInt("[MISC]", "max_snapshots", s.MaxSnapshots)
	}
}

func (c *Cluster) Save() error {
	return c.Ini.Save(c.Paths.ClusterIni(c.Name))
}

// Shard 读取分片 server.ini 里面板关心的那几项。文件缺失时返回占位值，
// 由调用方决定是否补建（新建集群时本来就先有目录再写 ini）。
func (c *Cluster) Shard(shard string) ShardSettings {
	ini, _, err := LoadIni(c.Paths.ShardIni(c.Name, shard))
	if err != nil {
		return ShardSettings{Name: shard}
	}
	return ShardSettings{
		ServerPort: ini.GetInt("[NETWORK]", "server_port", 0),
		IsMaster:   ini.GetBool("[SHARD]", "is_master", shard == ShardMaster),
		Name:       ini.Unquote("[SHARD]", "name"),
		ID:         ini.GetInt("[SHARD]", "id", 0),
		MasterIP:   ini.Unquote("[SHARD]", "master_ip"),
		MasterPort: ini.GetInt("[SHARD]", "master_port", 0),
		ClusterKey: ini.Unquote("[SHARD]", "cluster_key"),
	}
}

func (c *Cluster) SetShard(shard string, s ShardSettings) error {
	ini, _, err := LoadIni(c.Paths.ShardIni(c.Name, shard))
	if err != nil {
		ini = ParseIni("")
	}
	ini.SetInt("[NETWORK]", "server_port", s.ServerPort)
	ini.SetBool("[SHARD]", "is_master", s.IsMaster)
	name := s.Name
	if name == "" {
		name = shard
	}
	ini.SetString("[SHARD]", "name", name)
	ini.SetInt("[SHARD]", "id", s.ID)
	if s.MasterIP != "" {
		ini.SetString("[SHARD]", "master_ip", s.MasterIP)
	}
	if s.MasterPort > 0 {
		ini.SetInt("[SHARD]", "master_port", s.MasterPort)
	}
	if s.ClusterKey != "" {
		ini.SetString("[SHARD]", "cluster_key", s.ClusterKey)
	}
	ini.SetBool("[ACCOUNT]", "encode_user_path", true)
	return ini.Save(c.Paths.ShardIni(c.Name, shard))
}

// MasterPort 读 cluster.ini 的 [SHARD] master_port。
func (c *Cluster) MasterPort() int {
	return c.Ini.GetInt("[SHARD]", "master_port", 0)
}

func (c *Cluster) SetMasterPort(port int) {
	c.Ini.SetInt("[SHARD]", "master_port", port)
}

// PortPlan 是一个集群当前的端口组合。三个数字必须一起看：改 Master 的游戏端口
// 会连带改掉意图端口，而洞穴是靠意图端口找地面的。
type PortPlan struct {
	// Master 是玩家加入时用的那个端口（c_connect 里的号、云安全组要放行的号）。
	Master int
	// Intent 是 Klei 硬规定的意图端口，永远等于 Master + 1。
	Intent int
	// Caves 是洞穴世界的游戏端口；只有地表的集群为 0。
	Caves int
	// HasCaves 区分"没有洞穴"和"有洞穴但端口没读到"。
	HasCaves bool
}

// PortPlan 从磁盘上的三份文件里把端口读出来（cluster.ini + 每个分片的 server.ini）。
func (c *Cluster) PortPlan() PortPlan {
	plan := PortPlan{Intent: c.MasterPort()}
	plan.Master = c.Shard(ShardMaster).ServerPort
	for _, sh := range c.Paths.ShardNames(c.Name) {
		if sh == ShardCaves {
			plan.HasCaves = true
			plan.Caves = c.Shard(sh).ServerPort
		}
	}
	return plan
}

// SetPorts 改这个集群的游戏端口：master<=0 表示不动 Master，caves 同理。
//
// 这里刻意不"只改一个文件"。Klei 的规则是 cluster.ini 的 master_port 与洞穴
// server.ini 的 master_port 都必须等于 Master 的 server_port + 1，
// 只改 server.ini 会留下一个连不上地面的洞穴——而游戏不会报错，
// 只会静默地起一个进不去的世界。所以三处一起写。
//
// 洞穴的 master_ip 空着时一并补成 127.0.0.1：手工造的房间常常只有端口没有地址，
// 那种组合光修端口照样连不上地表，而洞穴和地表在同一台机器上，这个值没有第二种选法。
func (c *Cluster) SetPorts(master, caves int) error {
	if master <= 0 && caves <= 0 {
		return fmt.Errorf("%w: 没有要改的端口", ErrInvalidArg)
	}
	if master > 0 {
		c.SetMasterPort(master + 1)
		if err := c.Save(); err != nil {
			return fmt.Errorf("写入 cluster.ini 的意图端口: %w", err)
		}
	}
	for _, sh := range c.Paths.ShardNames(c.Name) {
		set := c.Shard(sh)
		if set.ServerPort == 0 && set.ID == 0 {
			// server.ini 读不回来（文件缺失或坏掉）：这里不猜值补建，
			// 补错了会把一个本来还能靠引擎自愈的世界彻底写坏。
			return fmt.Errorf("%s 的 server.ini 读不到端口，先修好这个文件再改端口", sh)
		}
		touched := false
		switch sh {
		case ShardMaster:
			if master > 0 && set.ServerPort != master {
				set.ServerPort = master
				touched = true
			}
		case ShardCaves:
			if caves > 0 && set.ServerPort != caves {
				set.ServerPort = caves
				touched = true
			}
			if master > 0 {
				set.MasterPort = master + 1
				if strings.TrimSpace(set.MasterIP) == "" {
					set.MasterIP = "127.0.0.1"
				}
				touched = true
			}
		}
		if !touched {
			continue
		}
		if err := c.SetShard(sh, set); err != nil {
			return fmt.Errorf("写入 %s 的端口: %w", sh, err)
		}
	}
	return nil
}

// ShardEnabled 表示这是不是多世界集群。
func (c *Cluster) ShardEnabled() bool {
	return c.Ini.GetBool("[SHARD]", "shard_enabled", false)
}

// Token 读取 Klei 账户令牌。面板把它当敏感值：接口只回传"是否已配置 + 长度"，绝不外文明文。
func (c *Cluster) Token() (string, error) {
	raw, err := os.ReadFile(c.Paths.TokenFile(c.Name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("读取 cluster_token.txt: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func (c *Cluster) SetToken(token string) error {
	token = strings.TrimSpace(token)
	if err := ValidateToken(token); err != nil {
		return err
	}
	tf := c.Paths.TokenFile(c.Name)
	// 令牌不留 .bak：轮换的场景就是"这串可能泄露了"，旧值留在盘上等于没换干净。
	if err := WriteFileAtomicNoBackup(tf, []byte(token+"\n"), 0600); err != nil {
		return err
	}
	// 旧版本写下的 .bak 里是上一串明文，这次轮换顺手清掉。
	if err := os.Remove(tf + ".bak"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除旧令牌备份 %s: %w", filepath.Base(tf+".bak"), err)
	}
	return nil
}

// ClearToken 摘下这个集群的 Klei 令牌：删掉 cluster_token.txt，连旁边那份
// cluster_token.txt.bak 一起删。那份备份里存的是上一个令牌，而"这串泄露了，先摘掉"
// 之后磁盘上还留着旧凭据，等于只摘了一半。
//
// 没有走 WriteFileAtomic 写空文件：那条路必然先把旧内容留成 .bak，而"没配令牌"的正常
// 形态本来就是文件不存在（新建集群时不填令牌就是这个样子，引擎两句日志是同一句）。
func (c *Cluster) ClearToken() error {
	tf := c.Paths.TokenFile(c.Name)
	for _, p := range []string{tf, tf + ".bak"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("删除 %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

// ValidateToken 只拦"这串一定不是 Klei 令牌"的情况：空、混进空白、含控制字符或
// 非 ASCII（富文本粘贴会把令牌改写成中文引号之类）、长度离谱。
//
// 刻意不用字符白名单。令牌是别人发的不透明凭据，云机首启实测到的真实形态里就有
// ^ / = + 这些不属于 base64url 的字符；我们猜的白名单把真令牌拒了，说明猜测没有依据。
// 放宽到这里是安全的：令牌只被写进 cluster_token.txt 让引擎自己读，
// 从不进 shell、不进 argv、不进 Lua 或 ini，没有需要靠字符集来防的注入面。
// 每条仍包 ErrInvalidArg：调用方要据此分 400 还是 500——写文件本身失败
// 和"这串字符不像令牌"是两件不同的事，都回 400 会掩盖磁盘问题。
func ValidateToken(token string) error {
	if token == "" {
		return fmt.Errorf("%w: Klei 令牌为空，请到个人中心生成 cluster_token 后填入", ErrInvalidArg)
	}
	if len(token) > 512 {
		return fmt.Errorf("%w: Klei 令牌长度 %d 过长，疑似整页粘贴，请只保留令牌本身", ErrInvalidArg, len(token))
	}
	for _, r := range token {
		if unicode.IsSpace(r) {
			return fmt.Errorf("%w: Klei 令牌里混进了空白字符，请只粘贴令牌本身", ErrInvalidArg)
		}
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return fmt.Errorf("%w: Klei 令牌含不可打印或非 ASCII 字符 %q，疑似复制时被富文本改写，请从令牌页面纯文本重抄", ErrInvalidArg, r)
		}
	}
	if len(token) < 20 {
		return fmt.Errorf("%w: Klei 令牌长度 %d 过短，疑似复制不完整", ErrInvalidArg, len(token))
	}
	return nil
}

// UsedPorts 扫全部集群，给出已被占用的端口，供新建集群时挑空闲端口。
func (p Paths) UsedPorts() (map[int]string, error) {
	names, err := p.ClusterNames()
	if err != nil {
		return nil, err
	}
	used := make(map[int]string)
	for _, n := range names {
		c, err := OpenCluster(p, n)
		if err != nil {
			continue
		}
		if mp := c.MasterPort(); mp > 0 {
			used[mp] = n + " [SHARD]master_port"
		}
		for _, sh := range p.ShardNames(n) {
			sp := c.Shard(sh).ServerPort
			if sp <= 0 {
				continue
			}
			used[sp] = n + "/" + sh + " server_port"
			// 每个世界还会绑 server_port+1。Master 的那个号写在 cluster.ini 里已经登记过了，
			// 洞穴的那个号不存在于任何文件，只有这条规则知道它被占着；漏掉它，
			// 下一个房间会自动挑进一个正在被别的房间用的端口，两边都起得来但连不上。
			if _, ok := used[sp+1]; !ok {
				used[sp+1] = n + "/" + sh + " server_port+1"
			}
		}
	}
	return used, nil
}

// PickPorts 为 newCluster 个分片挑一组空闲端口，返回 (各分片端口, master_port)。
func (p Paths) PickPorts(base, end, numShards int) ([]int, int, error) {
	return p.PickPortsAt(base, end, numShards, 0)
}

// PickPortsAt 同上，但可以把 Master 的游戏端口钉成 wantMaster（建房间时用户填的那个）。
// wantMaster<=0 就是"面板自己挑"。
//
// 钉端口比自动挑多两个坑：wantMaster+1 那个意图端口被别的房间占了（它不出现在游戏列表里，
// 但 Klei 要求它紧跟游戏端口，谁都想不到的时候它偏偏是硬要求），以及洞穴端口撞上。
// 所以钉了 Master 之后，洞穴仍然从空闲里挑，并且从意图端口之后开始找。
func (p Paths) PickPortsAt(base, end, numShards, wantMaster int) ([]int, int, error) {
	if numShards < 1 {
		numShards = 1
	}
	used, err := p.UsedPorts()
	if err != nil {
		return nil, 0, err
	}
	if end < base {
		end = base
	}
	// 钉住的号可以在面板区间之外：用户照着 12345 去开云安全组是常态，而"建完之后"
	// 那一页本来就允许改成区间外的号。区间只管面板自己挑号时往哪儿挑——
	// 所以洞穴那一组仍然从区间里挑，只是要绕开刚钉住的这一组。
	if wantMaster > 0 {
		intent := wantMaster + 1
		if who, busy := used[wantMaster]; busy {
			return nil, 0, fmt.Errorf("%w: 游戏端口 %d 已被 %s 占用，请换一个", ErrInvalidArg, wantMaster, who)
		}
		if who, busy := used[intent]; busy {
			return nil, 0, fmt.Errorf("%w: 游戏端口 %d 配套的意图端口 %d 已被 %s 占用；意图端口必须紧跟着游戏端口，换一个后面空着的号",
				ErrInvalidArg, wantMaster, intent, who)
		}
		// wantMaster-1 也一并挡掉：引擎会给它绑上 wantMaster 这个号，两个世界就撞在一起了。
		reserved := map[int]bool{wantMaster - 1: true, wantMaster: true, intent: true}
		rest, err := freePorts(used, base, end, numShards-1, reserved)
		if err != nil {
			return nil, 0, err
		}
		return append([]int{wantMaster}, rest...), intent, nil
	}
	for masterIntent := base + 1; masterIntent <= end; masterIntent++ {
		masterGame := masterIntent - 1
		if _, busy := used[masterIntent]; busy {
			continue
		}
		if _, busy := used[masterGame]; busy {
			continue
		}
		rest, err := freePorts(used, masterIntent+1, end, numShards-1, nil)
		if err != nil {
			return nil, 0, err
		}
		return append([]int{masterGame}, rest...), masterIntent, nil
	}
	// 区间满不是服务器的毛病，是这台机器的号被占完了：报 400 让调用方去放宽配置或腾号。
	return nil, 0, fmt.Errorf("%w: 端口区间 %d-%d 已被占满", ErrInvalidArg, base, end)
}

// freePorts 从 from 起挑 need 个空闲端口（挑不满就报错，不返回半个冲突方案）。
// reserved 是"这次刚被钉住、还没写进文件所以 used 里查不到"的那几个号，没有就传 nil。
func freePorts(used map[int]string, from, end, need int, reserved map[int]bool) ([]int, error) {
	out := make([]int, 0, need)
	for port := from; len(out) < need && port <= end; port++ {
		if _, busy := used[port]; busy {
			continue
		}
		if reserved[port] {
			continue
		}
		out = append(out, port)
	}
	if len(out) < need {
		return nil, fmt.Errorf("%w: 端口区间 %d-%d 内没有足够空闲端口，把面板配置里的端口区间放宽一点，或者先停掉一个房间",
			ErrInvalidArg, from, end)
	}
	return out, nil
}

// CreateCluster 按最小可用配置新建集群：cluster.ini + 每个分片的 server.ini，
// 世界文件（leveldataoverride.lua）由调用方用世界设定字典生成。
//
// numShards 传 2 时自动建 Master+Caves 并写好 shard_enabled。
func (p Paths) CreateCluster(name string, s GameSettings, numShards int, portBase, portEnd int) (*Cluster, error) {
	return p.CreateClusterAt(name, s, numShards, portBase, portEnd, 0)
}

// CreateClusterAt 多一个"Master 游戏端口由用户指定"的入口，0 表示面板自己挑。
func (p Paths) CreateClusterAt(name string, s GameSettings, numShards int, portBase, portEnd, wantMaster int) (*Cluster, error) {
	if numShards < 1 || numShards > 2 {
		return nil, fmt.Errorf("世界数量只支持 1 或 2，收到 %d", numShards)
	}
	if _, err := os.Stat(p.ClusterIni(name)); err == nil {
		return nil, fmt.Errorf("集群 %s 已存在", name)
	}
	ports, masterIntent, err := p.PickPortsAt(portBase, portEnd, numShards, wantMaster)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(p.ShardDir(name, ShardMaster), 0755); err != nil {
		return nil, fmt.Errorf("创建 Master 目录: %w", err)
	}
	shards := []string{ShardMaster}
	if numShards == 2 {
		if err := os.MkdirAll(p.ShardDir(name, ShardCaves), 0755); err != nil {
			return nil, fmt.Errorf("创建 Caves 目录: %w", err)
		}
		shards = append(shards, ShardCaves)
	}

	ini := ParseIni(defaultClusterIni)
	c := &Cluster{Name: name, Paths: p, Ini: ini}
	if s.ClusterName == "" {
		s.ClusterName = name
	}
	c.SetSettings(s)
	clusterKey, err := randomClusterKey()
	if err != nil {
		return nil, err
	}
	ini.SetBool("[SHARD]", "shard_enabled", numShards == 2)
	ini.Set("[SHARD]", "bind_ip", "127.0.0.1")
	ini.Set("[SHARD]", "master_ip", "127.0.0.1")
	ini.SetInt("[SHARD]", "master_port", masterIntent)
	ini.SetString("[SHARD]", "cluster_key", clusterKey)
	if err := ini.Save(p.ClusterIni(name)); err != nil {
		return nil, err
	}

	for i, sh := range shards {
		set := ShardSettings{
			ServerPort: ports[i],
			IsMaster:   i == 0,
			Name:       sh,
			ID:         shardID(sh),
			ClusterKey: ini.Unquote("[SHARD]", "cluster_key"),
		}
		if i != 0 {
			set.MasterIP = "127.0.0.1"
			set.MasterPort = masterIntent
		}
		if err := c.SetShard(sh, set); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(p.SaveDir(name, sh), 0755); err != nil {
			return nil, fmt.Errorf("创建存档目录: %w", err)
		}
	}
	return c, nil
}

// shardID 用固定映射而不是随机数：面板重启、备份恢复之后分片 id 必须稳定，
// 变了的话客户端本地记录的分片会错位。
func shardID(shard string) int {
	switch shard {
	case ShardMaster:
		return 1
	case ShardCaves:
		return 2
	}
	return 100
}

// randomClusterKey 是分片间连接密码。用 crypto/rand 而不是时间戳：
// 同一台机器上多个集群若 key 相同，洞穴分片会连到别的地面。
func randomClusterKey() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成世界连接密码: %w", err)
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var sb strings.Builder
	for _, v := range b {
		sb.WriteByte(digits[int(v)%len(digits)])
	}
	return sb.String(), nil
}

// defaultClusterIni 保留 Klei 文件里的中文注释骨架，用户改配置时看得懂每项干什么。
const defaultClusterIni = `[GAMEPLAY]
#游戏模式，可选 survival、endless、wilderness
game_mode = "endless"
#最大玩家数（上限 16）
max_players = 6
#是否开启玩家对战
pvp = false
#没人时服务器暂停。默认关掉，这条不是偏好而是功能前提：
#引擎 scripts/update.lua:229 有一句 assert(not TheNet:IsServerPaused())，
#而驱动所有 DoPeriodicTask 的 RunScheduler 全脚本只有 update.lua:240 一个调用点——
#暂停期间整个 Lua 调度器冻结，助手 MOD 既不写心跳也不执行命令。
#实测云机：开着它 150 秒收不到一份快照，关掉立刻每秒一份。
#要省资源可以打开，代价是面板在没有玩家时看不到实时状态、命令也不会被执行。
pause_when_empty = false
#投票重启世界
vote_enabled = false
#投票踢人
vote_kick_enabled = false

[NETWORK]
#局域网游戏
lan_only_cluster = false
#游戏偏好 cooperative、competitive、social、madness
cluster_intention = "cooperative"
#服务器密码
cluster_password = ""
#服务器描述
cluster_description = ""
#服务器名称
cluster_name = ""
#离线服务器，只有局域网用户能加入
offline_cluster = false
#服务器语言
cluster_language = "zh"
#预留位
whitelist_slots = 0
#每秒通信次数，越高体验越好，但会增大服务负担
tick_rate = 15

[MISC]
#是否开启控制台
console_enabled = true
#最大快照数量
max_snapshots = 20

[SHARD]
#分片共享，开启洞穴必须为 true
shard_enabled = true
#监听地址，同一台机器上跑多个实例可填 127.0.0.1
bind_ip = 127.0.0.1
#Master 服务器 IP，非 Master 分片用，同机可填 127.0.0.1
master_ip = 127.0.0.1
#Master 的意图端口，必须等于 Master 分片 server_port + 1
master_port = 10999
#分片间连接密码，各分片必须一致
cluster_key = ""

[STEAM]
#只允许某 Steam 组的成员加入
steam_group_only = false
#指定某个 Steam 组，填组 ID
steam_group_id = 0
#开启后 Steam 组管理员拥有服务器权限
steam_group_admins = false
`

// PortSummary 给界面一句话说明端口占用情况。
func PortSummary(used map[int]string) string {
	if len(used) == 0 {
		return "没有已占用的端口"
	}
	keys := make([]int, 0, len(used))
	for k := range used {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d(%s)", k, used[k])
	}
	return b.String()
}

// IsPortTakenErr 把系统 bind 失败翻译成面板能懂的话。
func IsPortTakenErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "address already in use") || strings.Contains(msg, "bind")
}

// ClusterDirSize 供界面展示存档体积。
func (p Paths) ClusterDirSize(cluster string) (int64, error) {
	var total int64
	for _, sh := range p.ShardNames(cluster) {
		err := filepath.Walk(p.SaveDir(cluster, sh), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // 没有 save 目录不算错误
			}
			if !info.IsDir() {
				total += info.Size()
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

// FormatBytes 给人看的体积文案。
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), []string{"KB", "MB", "GB", "TB"}[exp])
}
