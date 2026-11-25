package modbridge

// 助手 MOD 的"安装/启用"侧。
//
// 结论来自服务端自带的 scripts/modindex.lua，不是从别的面板抄的：
//   * 专用服务端启动时先 DisableAllMods()（modindex.lua:102 的注释原话是
//     "We assume the mods will be re-enabled by the modoverrides.lua file"），
//     所以在云上想让助手加载，唯一的路就是把条目写进分片的 modoverrides.lua；
//     dedicated_server_mods_setup.lua 只管创意工坊下载，本地模组不走它。
//   * ApplyEnabledOverrides 拿键当目录名去 TryLoadMod，判的是
//     MODS_ROOT..modname.."/modinfo.lua"，所以"文件夹名 == modoverrides 的键"是硬要求。
//   * LoadModOverides 用 RunInEnvironment(fn, {}) 跑这个文件，env 是空的，
//     连 print 都没有——这份文件必须是纯数据表字面量，不能有任何函数调用。
//     面板的 dst.Render 输出的就是 return { ... }，天然满足。

import (
	"fmt"
	"os"
	"path/filepath"

	"dstpanel/internal/config"
	"dstpanel/internal/dst"
)

// 这两个键名必须与 modinfo.lua 里 settings 的 name 一致；
// ApplyConfigOptionOverrides 是按 option 名字逐个匹配 config_option.name 的，
// 写错名字不会报错，只会静默用默认值——那种"设置没生效"最难查。
const (
	optPollInterval   = "poll_interval"
	optPlayerDetail   = "player_detail"
	helperKeyEnabled  = "enabled"
	helperKeyPriority = "priority"
	helperKeyConfig   = "configuration_options"
)

// HelperSettings 是助手 MOD 的可调项。
type HelperSettings struct {
	// PollSeconds 是 MOD 写快照与拉队列的间隔，对应 modmain 的 DoPeriodicTask。
	PollSeconds int
	// PlayerDetail 关掉后快照只带人数，不带每个玩家的血量坐标。
	PlayerDetail bool
}

// HelperOverride 是分片 modoverrides.lua 里助手那一条的现状。
type HelperOverride struct {
	// Present 为 false 表示这个分片从来没配过助手。
	Present  bool
	Enabled  bool
	Settings HelperSettings
}

func defaultHelperSettings(cfg config.Helper) HelperSettings {
	seconds := cfg.PollMs / 1000
	if seconds < MinHelperPollSeconds {
		seconds = MinHelperPollSeconds
	}
	if seconds > MaxHelperPollSeconds {
		seconds = MaxHelperPollSeconds
	}
	return HelperSettings{PollSeconds: seconds, PlayerDetail: true}
}

// 轮询间隔的上下界。导出是为了让接口能把范围回给界面，
// 免得前端自己写一套 1~60，改一处忘一处。
const (
	MinHelperPollSeconds = 1
	MaxHelperPollSeconds = 60
)

// Validate 也供 HTTP 层提前调用，好把越界写成 400 而不是 500。
func (s HelperSettings) Validate() error {
	if s.PollSeconds < MinHelperPollSeconds || s.PollSeconds > MaxHelperPollSeconds {
		return fmt.Errorf("数据回写间隔要在 %d~%d 秒之间，当前是 %d",
			MinHelperPollSeconds, MaxHelperPollSeconds, s.PollSeconds)
	}
	return nil
}

// HelperInstallState 返回助手 MOD 缺失的文件（相对路径，正斜杠）；空切片表示文件齐。
//
// 判定依据从"手写的三个文件名"改成"二进制里的内嵌清单"（见 embed.go）：
// 以后往模组里加第四个文件，只要它被嵌进来了就会进检查范围，
// 不再需要记得同步改这里——漏改的后果是分片永远不回话，而且看不出原因。
func (b *Bridge) HelperInstallState() ([]string, error) {
	sync, err := b.SyncHelper(true)
	if err != nil {
		return nil, err
	}
	return sync.Missing, nil
}

// SetHelperOverride 在分片的 modoverrides.lua 里写入/更新助手条目。
// enabled=false 时只改 enabled，配置项原样留着，面板下次打开不用重填。
// 其它模组已有的条目与顺序都不动。
func (b *Bridge) SetHelperOverride(cluster, shard string, enabled bool, s HelperSettings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	path := b.Paths.ModOverrides(cluster, shard)
	tbl, err := b.loadOverrides(path)
	if err != nil {
		return err
	}

	entry := dst.NewTable()
	if old := tbl.Get(dst.HelperModName); old.Kind == dst.KindTable && old.T != nil {
		// 沿用已有条目的字段（priority 之类可能被人手工调过），只覆盖我们管的三项。
		for _, k := range old.T.Keys() {
			entry.Set(k, old.T.Get(k))
		}
	}
	if !entry.Has(helperKeyPriority) {
		entry.Set(helperKeyPriority, dst.Number(0))
	}
	entry.Set(helperKeyEnabled, dst.Bool(enabled))
	opts := dst.NewTable()
	opts.Set(optPollInterval, dst.Number(float64(s.PollSeconds)))
	opts.Set(optPlayerDetail, dst.Bool(s.PlayerDetail))
	entry.Set(helperKeyConfig, dst.Table(opts))
	tbl.Set(dst.HelperModName, dst.Table(entry))

	// 内容没变就别碰文件：这文件每次重写都会留一份 .bak，
	// 前端每几秒刷新一次状态时不该把 .bak 刷成一堆同样内容的快照。
	if rendered := dst.Render(tbl); rendered == normalizeEOL(existingOrEmpty(path)) {
		return nil
	}
	return dst.SaveLua(path, tbl)
}

// HelperOverride 读回分片里助手的现状。文件不存在或没有助手条目都算 Present=false。
func (b *Bridge) GetHelperOverride(cluster, shard string) (HelperOverride, error) {
	path := b.Paths.ModOverrides(cluster, shard)
	tbl, err := b.loadOverrides(path)
	if err != nil {
		return HelperOverride{}, err
	}
	old := tbl.Get(dst.HelperModName)
	if old.Kind != dst.KindTable || old.T == nil {
		return HelperOverride{}, nil
	}
	out := HelperOverride{
		Present: true,
		Enabled: old.T.Get(helperKeyEnabled).AsBool(false),
		// 条目里没有 configuration_options 时，引擎用的是 MOD 自己的默认值，
		// 面板这边就按当前配置显示，别显示成 0。
		Settings: b.Helper,
	}
	if opts := old.T.Get(helperKeyConfig); opts.Kind == dst.KindTable && opts.T != nil {
		out.Settings.PollSeconds = int(opts.T.Get(optPollInterval).AsNumber(float64(out.Settings.PollSeconds)))
		out.Settings.PlayerDetail = opts.T.Get(optPlayerDetail).AsBool(out.Settings.PlayerDetail)
	}
	return out, nil
}

func (b *Bridge) loadOverrides(path string) (*dst.LuaTable, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 新建的房间没有这个文件，Klei 侧读不到就当成没有覆盖，我们直接从空表开始。
			return dst.NewTable(), nil
		}
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	tbl, err := dst.ParseLuaTable(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s 解析失败，面板没有改动它：%w", filepath.Base(path), err)
	}
	return tbl, nil
}

func existingOrEmpty(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

func normalizeEOL(text string) string {
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
			continue
		}
		out = append(out, text[i])
	}
	return string(out)
}
