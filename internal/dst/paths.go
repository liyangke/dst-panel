package dst

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 分片名固定为这两个字面值，Klei 二进制按 -shard 参数找目录，改名会导致起服失败。
const (
	ShardMaster = "Master"
	ShardCaves  = "Caves"
)

// HelperModName 是面板自带助手 MOD 的目录名，同时也是它在 modoverrides.lua 里的键。
// 专用服务端启动时会先 DisableAllMods，只靠 modoverrides.lua 重新打开模组
// （modindex.lua:102 的注释就是这么写的），所以这个名字必须两边一致。
const HelperModName = "DSTPanelHelper"

// 两个 app id 不要混：GameAppID 是游戏本体，工坊内容与启动时的 SteamAppId/SteamGameId
// 环境变量用它（Klei 自带的 bin64/scripts/launch_dedicated_server.sh 导的就是 322330）；
// ServerAppID 是那个免费的服务端工具，SteamCMD 更新和本地 appmanifest 用它。
const (
	GameAppID   = "322330"
	ServerAppID = "343050"
)

// Paths 解析 DST 专用服务端与集群目录下的全部文件位置。
//
// 约定（已按参考实现核实）：
//
//	<kleiRoot>/Cluster_1/cluster.ini              集群公共配置
//	<kleiRoot>/Cluster_1/cluster_token.txt        Klei 账户令牌
//	<kleiRoot>/Cluster_1/Master/modoverrides.lua  分片模组覆盖
//	<kleiRoot>/Cluster_1/Master/save/             分片存档
//	<dstInstall>/mods/dedicated_server_mods_setup.lua  服务端模组下载清单
//	<dstInstall>/ugc_mods/<集群>/<世界>/content/322330/<工坊id>/  引擎自己拉的工坊内容
//	<dstInstall>/bin64/dontstarve_dedicated_server_nullrenderer_x64
type Paths struct {
	KleiRoot   string
	DstInstall string
}

func (p Paths) ClusterDir(cluster string) string {
	return filepath.Join(p.KleiRoot, cluster)
}

func (p Paths) ShardDir(cluster, shard string) string {
	return filepath.Join(p.KleiRoot, cluster, shard)
}

func (p Paths) ClusterIni(cluster string) string {
	return filepath.Join(p.ClusterDir(cluster), "cluster.ini")
}

func (p Paths) ShardIni(cluster, shard string) string {
	return filepath.Join(p.ShardDir(cluster, shard), "server.ini")
}

func (p Paths) TokenFile(cluster string) string {
	return filepath.Join(p.ClusterDir(cluster), "cluster_token.txt")
}

// 玩家名单都放在集群根目录（和 cluster.ini 同级），不是分片目录：
// 引擎在启动时按集群读这三份文件，地表和洞穴共用同一套身份。
// 文件名是 Klei 自己用的，大小写和拼写都不能改，否则引擎会当成不存在的文件。
func (p Paths) AdminListFile(cluster string) string {
	return filepath.Join(p.ClusterDir(cluster), "adminlist.txt")
}

func (p Paths) WhitelistFile(cluster string) string {
	return filepath.Join(p.ClusterDir(cluster), "whitelist.txt")
}

func (p Paths) BlocklistFile(cluster string) string {
	return filepath.Join(p.ClusterDir(cluster), "blocklist.txt")
}

// LevelData 是引擎的 GAME USE 世界文件：必须由客户端下发的一份"完整世界定义"。
// 面板不写它（见 worldgen.go 顶部那段实测结论），只读它，用于在界面上提示
// "有人手工放过这份文件"。
func (p Paths) LevelData(cluster, shard string) string {
	return filepath.Join(p.ShardDir(cluster, shard), "leveldataoverride.lua")
}

// WorldGenOverride 是引擎的 USER USE 世界文件：只给 preset 名和要改的那几项，
// 完整世界定义由引擎自己从 scripts/map/levels 里取。
func (p Paths) WorldGenOverride(cluster, shard string) string {
	return filepath.Join(p.ShardDir(cluster, shard), "worldgenoverride.lua")
}

func (p Paths) ModOverrides(cluster, shard string) string {
	return filepath.Join(p.ShardDir(cluster, shard), "modoverrides.lua")
}

func (p Paths) SaveDir(cluster, shard string) string {
	return filepath.Join(p.ShardDir(cluster, shard), "save")
}

func (p Paths) WorldFile(cluster, shard string) string {
	return filepath.Join(p.SaveDir(cluster, shard), "world")
}

// SessionDir 是 save/session/<GAMEID>，GAMEID 为 16 位十六进制，随世界重生变化。
func (p Paths) SessionRoot(cluster, shard string) string {
	return filepath.Join(p.SaveDir(cluster, shard), "session")
}

func (p Paths) ServerLog(cluster, shard string) string {
	return filepath.Join(p.ShardDir(cluster, shard), "server_log.txt")
}

func (p Paths) ChatLog(cluster, shard string) string {
	return filepath.Join(p.ShardDir(cluster, shard), "server_chat_log.txt")
}

// HelperDir 是 helper MOD 与面板之间的文件通道目录。
//
// 位置不是随手选的：MOD 侧用 TheSim:Get/SetPersistentString 读写，相对路径的基准就是
// 分片的 save 目录（游戏自己的 saveindex 就是用 "../leveldataoverride.lua" 回指分片目录的）。
//
// 2026-10-31 云机实测改成了平铺：原先放在 save/panel 子目录里，MOD 每次写都打
// "[CRITICAL] Failed to save file"——SetPersistentString 不创建多级目录，而
// TheSim.MakeDirectory 在这个构建里是 nil（控制台 print 实测），MOD 自己建不出目录。
// 平铺在 save/ 根下还省掉另一件事：面板不用在引擎建 save/ 之前抢先造出空存档目录，
// 那会让"这局是不是新档"的判断多一个我们自己造的变量。
func (p Paths) HelperDir(cluster, shard string) string {
	return p.SaveDir(cluster, shard)
}

func (p Paths) HelperCommandQueue(cluster, shard string) string {
	return filepath.Join(p.HelperDir(cluster, shard), "dstpanel_queue.txt")
}

// HelperProcessedFile 由 helper MOD 写：已执行过的命令 id，用于防重复执行。
func (p Paths) HelperProcessedFile(cluster, shard string) string {
	return filepath.Join(p.HelperDir(cluster, shard), "dstpanel_processed.txt")
}

func (p Paths) HelperResultFile(cluster, shard string) string {
	return filepath.Join(p.HelperDir(cluster, shard), "dstpanel_result.json")
}

func (p Paths) HelperStatusFile(cluster, shard string) string {
	return filepath.Join(p.HelperDir(cluster, shard), "dstpanel_status.json")
}

// HelperMapFile 是助手 MOD 按需导出的世界地图（地形 + 建筑/资源分布）。
// 单独一个文件而不是塞进结果回执：地形网格有十几 KB，回执文件是给短命令用的，
// 混在一起会先撞上助手侧的体积上限，还会让每条命令的回执都变慢。
func (p Paths) HelperMapFile(cluster, shard string) string {
	return filepath.Join(p.HelperDir(cluster, shard), "dstpanel_map.json")
}

// ServerModSetup 是服务端模组下载清单，必须落在安装目录而不是集群目录。
func (p Paths) ServerModSetup() string {
	return filepath.Join(p.DstInstall, "mods", "dedicated_server_mods_setup.lua")
}

// ModsRoot 就是引擎里的 MODS_ROOT：专用服务端用 TheSim:GetModDirectoryNames()
// 扫这个目录来发现模组，所以本地模组（含面板助手）必须落在这里，
// 而不是集群目录。
func (p Paths) ModsRoot() string { return filepath.Join(p.DstInstall, "mods") }

// HelperModDir 是面板自带助手 MOD 的安装位置。目录名即 modoverrides.lua 里的键，
// 两者必须一致：ModIndex:TryLoadMod 判的就是 MODS_ROOT..modname.."/modinfo.lua"。
func (p Paths) HelperModDir() string { return filepath.Join(p.ModsRoot(), HelperModName) }

// WorkshopModDir 是工坊模组在旧形态下的目录名：手工把下载包放进 mods/ 时就是这个写法。
//
// 这个引擎真正落工坊内容的地方不是这里，见下面 UgcModsRoot 那一组。服务端二进制里
// 同时留着 "workshop-%lu"（模组在引擎内部的标识名，modoverrides.lua 的键也就是它）
// 和 "../ugc_mods/"（文件实际存放的根），所以两种都要认。
func (p Paths) WorkshopModDir(modID string) string {
	return filepath.Join(p.ModsRoot(), "workshop-"+modID)
}

// UgcModsRoot 是引擎自己那套工坊下载区的根。
//
// 云机实测（2026-11-03，构建 761239 一线）：世界启动时按订阅清单拉工坊文件，
// Steam 工坊日志打的是
// "[AppID 322330] Loaded workshop items in <dstInstall>/ugc_mods/Cluster_1/Master"，
// 解开的内容在 content/322330/<工坊id>/，同级还有一份台账 appworkshop_322330.acf
// （里面 WorkshopItemsInstalled 记着每一条的 size 与 manifest）。
// 注意这套目录是按集群、按世界各一份——不是整台机器共用一份。
func (p Paths) UgcModsRoot() string { return filepath.Join(p.DstInstall, "ugc_mods") }

// UgcShardRoot 是一个世界的下载区根（引擎那一侧叫法仍是 cluster + shard 两层目录）。
func (p Paths) UgcShardRoot(cluster, shard string) string {
	return filepath.Join(p.UgcModsRoot(), cluster, shard)
}

// UgcContentDir 是一个世界里工坊内容的解包目录，条目名就是那串数字 ID。
func (p Paths) UgcContentDir(cluster, shard string) string {
	return filepath.Join(p.UgcShardRoot(cluster, shard), "content", GameAppID)
}

// UgcModDir 是引擎下载区里某个工坊条目的目录。
func (p Paths) UgcModDir(cluster, shard, modID string) string {
	return filepath.Join(p.UgcContentDir(cluster, shard), modID)
}

func (p Paths) BinDir() string {
	return filepath.Join(p.DstInstall, "bin64")
}

func (p Paths) ServerBinary() string {
	return filepath.Join(p.BinDir(), "dontstarve_dedicated_server_nullrenderer_x64")
}

func (p Paths) AppManifest() string {
	return filepath.Join(p.DstInstall, "steamapps", "appmanifest_"+ServerAppID+".acf")
}

// VersionFile 是安装目录根上那行构建号（实测内容就是 "760944\n"）。
// 面板读它只为了回答"这台机器上装的是哪一版"，因为客户端连不上时的第一句报错
// 是"服务端版本太旧"，而那个数字只有这里对得上。
func (p Paths) VersionFile() string { return filepath.Join(p.DstInstall, "version.txt") }

// EngineVersion 返回已安装服务端的构建号；没装过（或文件读不出来）返回空串。
//
// 判"装没装"用的是二进制在不在，不是这个号：steamcmd 半途断掉时 version.txt
// 可能已经落下来而 bin64 还没写完，那种状态必须报成"缺本体"而不是"本体是某版本"。
func (p Paths) EngineVersion() string {
	raw, err := os.ReadFile(p.VersionFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// EnginePresent 回答"服务端二进制在不在"。
// 引擎缺位时面板所有接口都能正常返回——只是开不出服，所以这一句要在动手之前露出来。
func (p Paths) EnginePresent() bool {
	st, err := os.Stat(p.ServerBinary())
	return err == nil && !st.IsDir()
}

// ClusterNames 列出所有集群目录，按 Cluster_N 的数字序排，避免 Cluster_10 排在 Cluster_2 前。
func (p Paths) ClusterNames() ([]string, error) {
	entries, err := os.ReadDir(p.KleiRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// 只有含 cluster.ini 的目录才是真集群，否则可能是残留空目录。
		if ok, _ := fileExists(p.ClusterIni(e.Name())); ok {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		mi, mj := clusterOrdinal(names[i]), clusterOrdinal(names[j])
		if mi != mj {
			return mi < mj
		}
		return names[i] < names[j]
	})
	return names, nil
}

func clusterOrdinal(name string) int {
	idx := strings.LastIndexAny(name, "0123456789")
	if idx < 0 {
		return -1
	}
	start := idx
	for start > 0 && name[start-1] >= '0' && name[start-1] <= '9' {
		start--
	}
	n := 0
	for i := start; i <= idx; i++ {
		n = n*10 + int(name[i]-'0')
	}
	return n
}

// ShardNames 返回该集群实际存在的分片目录，顺序固定 Master 在前。
func (p Paths) ShardNames(cluster string) []string {
	var out []string
	for _, s := range []string{ShardMaster, ShardCaves} {
		if ok, _ := dirExists(p.ShardDir(cluster, s)); ok {
			out = append(out, s)
		}
	}
	return out
}

func (p Paths) NextClusterName() (string, error) {
	names, err := p.ClusterNames()
	if err != nil {
		return "", err
	}
	used := make(map[int]bool, len(names))
	for _, n := range names {
		used[clusterOrdinal(n)] = true
	}
	for i := 1; i <= 999; i++ {
		if !used[i] {
			return "Cluster_" + itoa(i), nil
		}
	}
	return "", errTooManyClusters{}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

type errTooManyClusters struct{}

func (errTooManyClusters) Error() string { return "集群编号已用尽（1-999）" }

func fileExists(path string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !st.IsDir(), nil
}

func dirExists(path string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return st.IsDir(), nil
}
