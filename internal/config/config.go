package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   Server   `yaml:"server"`
	Paths    Paths    `yaml:"paths"`
	Network  Network  `yaml:"network"`
	Backup   Backup   `yaml:"backup"`
	SteamAPI SteamAPI `yaml:"steamApi"`
	Helper   Helper   `yaml:"helper"`
	Monitor  Monitor  `yaml:"monitor"`
	Tasks    Tasks    `yaml:"tasks"`
	Lobby    Lobby    `yaml:"lobby"`
}

// DefaultAdminUser / DefaultAdminPassword 是首次启动时的管理员账号。
// 放在代码里而不是"留空让你自己填"：留空的面板在公网端口上等于没有门，
// 而默认值至少让第一次打开就能登录。
// 取 admin/admin 而不是一个看起来像口令的长串：这个凭据会印在 README、教程和
// 各种部署笔记里，用真口令当默认值等于把它公开。代价是它必然被爆破，所以
// 账号管理页会一直显示"还在用默认密码"，把改密的提醒留在界面上而不是靠文档。
const (
	DefaultAdminUser     = "admin"
	DefaultAdminPassword = "admin"
)

type Server struct {
	Bind          string `yaml:"bind"`
	Port          int    `yaml:"port"`
	AdminUser     string `yaml:"adminUser"`
	AdminPassword string `yaml:"adminPassword"`
}

type Paths struct {
	// KleiRoot 是 ~/.klei/DoNotStarveTogether，房间(cluster)配置与存档都在这下面。
	KleiRoot string `yaml:"kleiRoot"`
	// DstInstall 是 SteamCMD 装 app 343050 的目录，内含 bin64/ 与 mods/。
	DstInstall string `yaml:"dstInstall"`
	// SteamCmd 是 steamcmd.sh 可执行文件路径。
	SteamCmd string `yaml:"steamCmd"`
	// WorkDir 存放面板自身状态：数据库、备份、MOD 下载暂存。
	WorkDir string `yaml:"workDir"`
}

type Network struct {
	// ShardPortBase 起为每个分片分配 server_port，步长 2（分片端口 + 主意图端口）。
	ShardPortBase int `yaml:"shardPortBase"`
	ShardPortEnd  int `yaml:"shardPortEnd"`
	// MasterIntentPortBase 用于 [SHARD] master_port，分片间内部通信。
	MasterIntentPortBase int `yaml:"masterIntentPortBase"`
}

type Backup struct {
	// Keep 是最多留几份快照（含恢复前自动打的安全备份），超出删最旧的。
	// 0 或负数表示不自动清理。
	Keep int `yaml:"keep"`
	// ScheduleHours 是定时备份的间隔小时数，0 表示不开。
	// 默认不开是有意的：一份快照要在盘上落几十 MB，在 4G 小机上没问过用户就定期写盘
	// 既占空间也占 IO，让想开的人在面板设置里自己开。
	ScheduleHours int `yaml:"scheduleHours"`
}

type SteamAPI struct {
	// WebAPIKey 用来在线检索创意工坊（IPublishedFileService.QueryFiles）。
	// 不填不影响装模组：工坊 ID 本身就是凭据，照样能登记；少的是"按名字找 ID"，
	// 这时候检索只会翻面板本地缓存（mods_seen，见过即入库）。
	// 申请页：https://steamcommunity.com/dev/apikey
	WebAPIKey string `yaml:"webApiKey"`
	// Proxy 形如 http://127.0.0.1:7890；国内机房直连 api.steampowered.com 经常不通时用。
	// 空表示跟随 HTTPS_PROXY 环境变量。只影响在线检索，不影响服务端自己的工坊下载。
	Proxy string `yaml:"proxy"`
}

type Helper struct {
	// PollMs 是 helper MOD 拉取命令队列的游戏内 tick 间隔。
	PollMs int `yaml:"pollMs"`
	// StaleSeconds 超过这个时长没有心跳就判定 MOD 不在线。
	StaleSeconds int `yaml:"staleSeconds"`
}

type Monitor struct {
	// SampleSeconds 是在线/世界状态入趋势库的间隔。
	// 按秒存会把 4G 小机的库撑爆（跑一个月就是两百多万行），默认一分钟。
	SampleSeconds int `yaml:"sampleSeconds"`
	// CleanupHours 是保留期清理跑多久一次；具体保留天数在面板设置里改。
	CleanupHours int `yaml:"cleanupHours"`
}

type Tasks struct {
	// TimeoutMinutes 是单条任务的硬上限，只用来兜底防僵尸进程。
	// 0 或负数 = 按任务类型取默认：本体下载 720 分钟（12 小时），备份与恢复 60 分钟。
	// 本体的上限为什么这么宽：整包实测 4.28 GB，而带宽没人能替你猜——
	// 同一台云机在一次任务里实测过 33 KB/s（steamcmd 给自己更新那一段）
	// 和 29 MB/s（本体文件下来那一段），差三个数量级，
	// 按"典型带宽"定的上限一定会在某一台机器上把正常下载判成失败。
	// 真正的判停靠 StallMinutes，这一项只是最后那道闸。
	TimeoutMinutes int `yaml:"timeoutMinutes"`
	// StallMinutes 是"连续多久读不到新进展就算卡死"。
	// 只要 steamcmd 还在往外吐新内容就让它继续跑，连续这么多分钟一行新的都吐不出来才中断。
	// 云机实测过这个死相：自更新卡在同一个数字上反复打印，十几分钟不带变一个数的。
	// 0 或负数 = 用默认的 5 分钟。这一档没有"关掉"的口子：关掉之后只剩上面那道 12 小时的闸，
	// 真卡死的下载要白等十二个小时才在界面上说一句话。
	StallMinutes int `yaml:"stallMinutes"`
}

// Lobby 是"公网上的房间"那一页的来源（Klei 放在 CDN 上的公开快照）。
type Lobby struct {
	// Enabled 关掉时接口直接回一句"没在配置里打开"，面板一次也不会去连 Klei。
	// 默认开：这一页不要凭据、不写盘、不影响任何本地房间，关掉它省的是出网流量。
	Enabled bool `yaml:"enabled"`
	// RefreshSeconds 是同一个大区两次真实拉取的最小间隔。
	// 一份列表解压后有十几 MB，界面几秒一轮地刷会把带宽喂进去，所以必须有这一档。
	RefreshSeconds int `yaml:"refreshSeconds"`
	// TimeoutSeconds 是单次拉取的上限。列表大，走代理的机器上十几秒是正常的，
	// 不能按普通接口的三五秒来定，否则每次都判它超时。
	TimeoutSeconds int `yaml:"timeoutSeconds"`
	// Proxy 形如 http://127.0.0.1:7890；空表示跟随 HTTPS_PROXY 环境变量。
	// 和 steamApi.proxy 分开是因为两者的出口不一定同一个：
	// 有些人只把 Steam 社区挂了代理，Klei 这个 CDN 直连反而通。
	Proxy string `yaml:"proxy"`
}

func Default() *Config {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/root"
	}
	return &Config{
		Server: Server{
			Bind: "127.0.0.1", Port: 8921,
			AdminUser: DefaultAdminUser, AdminPassword: DefaultAdminPassword,
		},
		Paths: Paths{
			// 不变量：**面板运行用户的 HOME 就是这套系统的业务根目录**，四个默认值全在它下面。
			// 这么定不是审美：服务端二进制忽略 -savedir、存档只能落在 $HOME/.klei，
			// steamcmd 的缓存同样按 $HOME 落盘——只有让 HOME 本身当根目录，
			// "所有数据在一个目录"和"删掉一个用户就卸干净"才同时成立。
			// 部署脚本因此建专用用户并把它的 home 设成业务目录（deploy/start.sh），
			// 而不是往家目录里塞软链。
			KleiRoot:   filepath.Join(home, ".klei", "DoNotStarveTogether"),
			DstInstall: filepath.Join(home, "dedicated_server"),
			SteamCmd:   filepath.Join(home, "tools", "steamcmd", "steamcmd.sh"),
			WorkDir:    filepath.Join(home, "panel"),
		},
		Network: Network{ShardPortBase: 10998, ShardPortEnd: 11900, MasterIntentPortBase: 11000},
		Backup:  Backup{Keep: 20},
		Helper:  Helper{PollMs: 1000, StaleSeconds: 15},
		Monitor: Monitor{SampleSeconds: 60, CleanupHours: 6},
		// TimeoutMinutes 留 0 是"按类型给默认"的记号，不是"没有上限"；
		// 上限具体多少由 worker 那侧解释，配置这里只负责"要不要覆盖"。
		Tasks: Tasks{StallMinutes: 5},
		Lobby: Lobby{Enabled: true, RefreshSeconds: 60, TimeoutSeconds: 20},
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return nil, fmt.Errorf("server.port 非法: %d", cfg.Server.Port)
	}
	if cfg.Network.ShardPortBase <= 0 {
		cfg.Network.ShardPortBase = 10998
	}
	// 这两项写成 0 会让后台要么空转（间隔 0 秒，把 CPU 吃满）要么永远不触发，
	// 所以兜回默认值而不是报错——配置手抖不该挡住面板启动。
	if cfg.Monitor.SampleSeconds < 5 {
		cfg.Monitor.SampleSeconds = 60
	}
	if cfg.Monitor.CleanupHours < 1 {
		cfg.Monitor.CleanupHours = 6
	}
	// 负数写成 0（不开），不兜默认值：定时备份的"不开"是一个正经选项。
	if cfg.Backup.ScheduleHours < 0 {
		cfg.Backup.ScheduleHours = 0
	}
	// 这两项填负数是手抖，兜回"用面板默认"（0 就是默认的记号），不报错：
	// 判停参数写歪了不该挡住面板启动，而那两条默认值本来就是保守的。
	if cfg.Tasks.TimeoutMinutes < 0 {
		cfg.Tasks.TimeoutMinutes = 0
	}
	if cfg.Tasks.StallMinutes < 0 {
		cfg.Tasks.StallMinutes = 5
	}
	// 这两项写成 0 或负数，一个会让公网列表每次界面轮询都重拉十几 MB，
	// 一个会让它永远拉不动，都不如兜回默认值。真想关掉这一页请改 lobby.enabled。
	if cfg.Lobby.RefreshSeconds < 10 {
		cfg.Lobby.RefreshSeconds = 60
	}
	// 上限 120 秒是因为一次拉取会占住一个 HTTP 请求：填成十分钟，
	// 一个卡住的出口就能让浏览器那一端转十分钟的圈。
	if cfg.Lobby.TimeoutSeconds <= 0 {
		cfg.Lobby.TimeoutSeconds = 20
	}
	if cfg.Lobby.TimeoutSeconds > 120 {
		cfg.Lobby.TimeoutSeconds = 120
	}
	return cfg, nil
}

func (p Paths) DBFile() string    { return filepath.Join(p.WorkDir, "dstpanel.db") }
func (p Paths) BackupDir() string { return filepath.Join(p.WorkDir, "backups") }
func (p Paths) ModStaging() string {
	return filepath.Join(p.WorkDir, "mod_staging")
}
