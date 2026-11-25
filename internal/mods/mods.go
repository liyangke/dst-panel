// Package mods 管服务端模组：磁盘上有什么、下载清单登记了什么、清单写得对不对。
//
// 为什么工坊模组一定要走 dedicated_server_mods_setup.lua，而不是面板自己拿 steamcmd 拉文件：
//   - scripts/mods.lua 的 LoadServerModsFile 会在启动时执行这个文件，把每条
//     ServerModSetup("<id>") 交给 TheNet:ServerModSetup()，再由 TheNet:DownloadServerMods() 下载；
//   - 下载与解包全是引擎做的，落点是 <dstInstall>/ugc_mods/<集群>/<世界>/content/322330/<id>
//     （2026-11-03 云机实测：工坊日志 "[AppID 322330] Loaded workshop items in …/ugc_mods/Cluster_1/Master"，
//     紧接着 DownloadPublishedFile → OnDownloadPublishedFile [1] → Loading mod: workshop-<id>）。
//     换一条路就得自己复刻这套目录约定和更新时机，而 Klei 已经替我们对好了；
//   - 模组的识别点是 MODS_ROOT..目录名.."/modinfo.lua"（modindex.lua），而引擎把下载区里那份
//     也叫 workshop-<id>，所以面板两边都要扫：只扫 mods/ 会把刚刚下载好、也加载成功的模组
//     说成"文件还没下载"。
//
// 所以 steamcmd 在这个面板里只剩一件事：app_update 343050 更新服务端本体。
// 工坊内容整体不可用时（服务端有 "Steam Workshop functionality will be disabled." 这条分支）
// 靠手工上传把文件放进 mods/，见 upload.go。
package mods

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"dstpanel/internal/dst"
)

// Manager 只做文件层的事：读清单、列目录，不碰进程也不写 modoverrides。
type Manager struct {
	Paths dst.Paths
}

// Mod 是一个已落在 mods/ 目录里的模组。
type Mod struct {
	// Dir 是目录名，同时也是 modoverrides.lua 里的键——这两者必须一致，
	// 引擎判的就是 MODS_ROOT..modname.."/modinfo.lua"。
	Dir string
	// WorkshopID 对工坊模组是那串数字；本地模组为空。
	WorkshopID string
	Workshop   bool
	Name       string
	Version    string
	// Description / Tags 也来自 modinfo.lua。没配 Steam key 时检索页能列出的只有磁盘上
	// 这一批，少了这两个字段每行就只剩一个名字，和"没信息"没区别。
	Description string
	Tags        []string
	// ModinfoOK 为 false 表示 modinfo.lua 读不到或解析不了。
	// 这里不报错而是留个标记：一个坏掉的第三方模组不该让整个模组列表打不开，
	// 用户需要看到的是"这一个有问题，其他照常被管理"。
	ModinfoOK bool
	Modinfo   string
	// InSetupList 表示它出现在下载清单里，也就是服务端启动时会拉它。
	InSetupList bool
	// Dependencies 是 modinfo.lua 里 mod_dependencies 声明的每一组依赖，
	// 每组是该作者写的所有候选标识（按书写顺序）。引擎读处 modindex.lua:577 起。
	// 为什么要按"组"存：引擎的 FindEnabledMod（modindex.lua:484）对一组里的候选是
	// **任一命中就算满足**，把每个标识都当成独立要求会误报缺依赖。
	// 依赖在整个 mods/ 里都对不上时，引擎的做法是打一句 "no valid dependent mod found
	// for mod X" 再 DisableBecauseBad(X)——**静默禁用**，不是报错退出。
	// 所以界面不自己挑出来，用户看到的就是"装了个开了却没效果的模组"。
	Dependencies [][]string
	// ClientOnly / AllClientsRequire 来自 modinfo.lua 的两个同名标记，引擎认为它们互斥
	// （modindex.lua:684 同时为真会打 WARNING）。
	// AllClientsRequire 决定玩家没订阅就进不了服（networking.lua:351/411/442 的断线分支），
	// 服务端开了它不等于大家都能玩。
	ClientOnly        bool
	AllClientsRequire bool
	// FromUGC 说这一份文件在引擎自己的下载区里（ugc_mods/<集群>/<世界>/content/322330/<id>），
	// 不在 mods/ 下面。它决定面板能动这一份的哪只手：读与开关照旧，
	// 但那份目录归引擎的工坊客户端记账，删不删得看引擎下一次启动会不会重新拉
	// （见 remove.go 顶上那段实测结论）。
	FromUGC bool
}

// workshopDirRe 是引擎给工坊模组的目录名格式。
var workshopDirRe = regexp.MustCompile(`^workshop-(\d{1,20})$`)

// setupLineRe 抓 ServerModSetup("123") / ServerModCollectionSetup("123") 两种调用。
// 允许前后有空格与单双引号，因为用户可能手工编辑过这个文件。
var setupLineRe = regexp.MustCompile(
	`(?i)^\s*(ServerMod(?:Setup|CollectionSetup))\s*\(\s*["'](\d{1,20})["']\s*\)\s*;?\s*$`)

// workshopIDRe 是工坊 id 的白名单：只允许纯数字。
var workshopIDRe = regexp.MustCompile(`^\d{1,20}$`)

// ValidWorkshopID 校验工坊 id。这是白名单：id 会被写进 Lua 文件，
// 放任意字符串进去就等于让用户提供的内容变成可执行代码。
func ValidWorkshopID(id string) error {
	if !workshopIDRe.MatchString(id) {
		return fmt.Errorf("工坊模组 id 必须是 1~20 位数字，收到的是 %q", id)
	}
	return nil
}

// WorkshopIDFromDir 从模组目录名里取出工坊 ID；不是 workshop-<数字> 这种写法返回空串。
//
// 删除那一步要先在文件还在的时候认出 ID：目录一旦没了，就再也没人知道它对应哪串数字，
// 下载清单里那行 ServerModSetup 也就再也配不上对（它会让人以为模组还等着下载）。
func WorkshopIDFromDir(dir string) string {
	m := workshopDirRe.FindStringSubmatch(strings.TrimSpace(dir))
	if m == nil {
		return ""
	}
	return m[1]
}

// List 返回 mods/ 下的模组，按目录名排序。
func (m *Manager) List() ([]Mod, error) {
	entries, err := os.ReadDir(m.Paths.ModsRoot())
	if err != nil {
		if os.IsNotExist(err) {
			// 模组目录不存在有两种完全不同的原因，而指错方向的那一句会把新手支去
			// 改一个本来没写错的配置：本体都没装时 mods/ 当然也不存在（冷启动实测里
			// 就是这样，界面同时喊"检查 dstInstall 配置"和"去开服向导建房间"）。
			if !m.Paths.EnginePresent() {
				return nil, fmt.Errorf("服务端本体还没装，模组目录（%s）自然也还没有——先去任务中心下载本体",
					m.Paths.ModsRoot())
			}
			return nil, fmt.Errorf("找不到模组目录 %s，检查 dstInstall 配置是否指向服务端安装目录", m.Paths.ModsRoot())
		}
		return nil, fmt.Errorf("读模组目录 %s: %w", m.Paths.ModsRoot(), err)
	}
	setup, err := m.SetupEntries()
	if err != nil {
		return nil, err
	}
	inSetup := map[string]bool{}
	for _, id := range setup {
		inSetup["workshop-"+id] = true
	}

	var out []Mod
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mod := Mod{Dir: e.Name(), InSetupList: inSetup[e.Name()]}
		if sub := workshopDirRe.FindStringSubmatch(e.Name()); sub != nil {
			mod.Workshop = true
			mod.WorkshopID = sub[1]
		}
		// 这一句是直接画在模组卡上的，所以不能把 os 那句英文错误整句丢出去：
		// 那既是一串裸英文，又把安装目录的绝对路径端到了界面上。
		// 界面只需要知道"这一目录缺了什么"；是权限还是不存在，用户看得见的差别是一样的。
		m.readModinfo(&mod, filepath.Join(m.Paths.ModsRoot(), e.Name()), "mods/"+e.Name())
		out = append(out, mod)
		seen[mod.Dir] = true
	}
	// 引擎按订阅清单拉下来的那一批不在 mods/ 下，上面这一遍扫不到它们。
	// 两边都有同一串 ID 时以 mods/ 里那份为准：那是 MODS_ROOT 直接指向的位置，
	// 面板给的下载状态、开关、删除全都按它说。
	for _, u := range m.ugcMods(inSetup) {
		if seen[u.Dir] {
			continue
		}
		seen[u.Dir] = true
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// ugcMods 扫引擎自己的下载区，把里面的工坊条目报成和 List 同一形状的模组。
//
// 这一遍是"整台机器"的：同一个模组在某个集群的地表和洞穴下各有一份，这里按 ID 归成一条，
// 因为 modoverrides.lua 里的键与引擎日志里的模组名都是 workshop-<id>，界面也是按那一个名字列。
// 具体到"这个世界那一份在不在"要另外问 DownloadedIn。
func (m *Manager) ugcMods(inSetup map[string]bool) []Mod {
	var out []Mod
	seen := map[string]bool{}
	for _, cp := range m.ugcCopies() {
		dir := "workshop-" + cp.ID
		if seen[dir] {
			continue
		}
		seen[dir] = true
		mod := Mod{Dir: dir, Workshop: true, WorkshopID: cp.ID,
			InSetupList: inSetup[dir], FromUGC: true}
		// 这句上界面，所以不能写 mods/<id>（那会被读成"该去 mods/ 下找"），也不给绝对路径。
		m.readModinfo(&mod, cp.Path, "引擎下载区里的这一份")
		out = append(out, mod)
	}
	return out
}

// ugcCopy 是引擎下载区里的一份工坊内容：一个模组在每个世界的下载区里各有一份。
type ugcCopy struct {
	Cluster string
	Shard   string
	ID      string
	Path    string
}

// Label 是给用户看的"哪个房间的哪个世界"，和接口里"开着它的世界"同一套写法。
func (c ugcCopy) Label() string { return c.Cluster + " / " + c.Shard }

// ugcCopies 扫一遍引擎自己的下载区，列出已经就位的工坊内容。
//
// 三层目录（房间 / 世界 / content/322330/<id>）的名字全是从磁盘读回来的，
// 面板不拿配置去猜：删掉的世界留下的下载区也要看得见，那才是"这台机器上占了多少磁盘"的真相。
// 目录名里不可能带斜杠（ReadDir 只给基础名），所以拼回去出不了 ugc_mods 这一层。
func (m *Manager) ugcCopies() []ugcCopy {
	clusters, err := os.ReadDir(m.Paths.UgcModsRoot())
	if err != nil {
		// 没有这一层目录是常态：这台机器一次工坊模组都没拉过。不是错误，也不提醒。
		return nil
	}
	var out []ugcCopy
	for _, c := range clusters {
		if !c.IsDir() {
			continue
		}
		shards, err := os.ReadDir(filepath.Join(m.Paths.UgcModsRoot(), c.Name()))
		if err != nil {
			continue
		}
		for _, sh := range shards {
			if !sh.IsDir() {
				continue
			}
			content := m.Paths.UgcContentDir(c.Name(), sh.Name())
			items, err := os.ReadDir(content)
			if err != nil {
				continue
			}
			for _, it := range items {
				id := it.Name()
				// 只认纯数字那一种目录名：引擎与 Steam 的工坊客户端在解包前后会留下
				// 临时目录，手工挪开的备份也顶着一个相近的名字，那些都不是就位的内容。
				if !it.IsDir() || !workshopIDRe.MatchString(id) {
					continue
				}
				out = append(out, ugcCopy{
					Cluster: c.Name(), Shard: sh.Name(), ID: id,
					Path: filepath.Join(content, id),
				})
			}
		}
	}
	return out
}

// readModinfo 读一个模组目录里的 modinfo.lua 并填进 mod。
// where 是给界面那句说明用的定位说法（"mods/某目录" 或 "引擎下载区里的这一份"）：
// 两种来路的文件在盘上的位置不同，出问题时用户要去核对的地方也不同。
func (m *Manager) readModinfo(mod *Mod, dirPath, where string) {
	raw, err := os.ReadFile(filepath.Join(dirPath, "modinfo.lua"))
	if err != nil {
		mod.Modinfo = where + " 里没有可读的 modinfo.lua（模组可能没下载完，或者被手工删过）"
		return
	}
	// modinfo.lua 的形状是"一串全局赋值"，不是 return 一张表：引擎 kleiloadlua 之后
	// 从环境里取 env.name / env.api_version（modindex.lua 的 InitializeModInfo）。
	// 早前这里用 ParseLuaTable 读它，结果每一句都失败，连自家助手模组都在界面上
	// 变成"名字空白的本地模组"。
	tbl, err := dst.ParseLuaAssignments(string(raw))
	if err != nil {
		mod.Modinfo = "modinfo.lua 结构读坏了（引号或花括号没闭合），面板只能按目录名识别它: " + err.Error()
		return
	}
	mod.ModinfoOK = true
	mod.Name = luaText(tbl, "name")
	mod.Version = luaText(tbl, "version")
	mod.Description = luaText(tbl, "description")
	mod.Tags = luaStringList(tbl, "tags")
	mod.Dependencies = luaDependencyGroups(tbl)
	mod.ClientOnly = luaFlag(tbl, "client_only_mod")
	mod.AllClientsRequire = luaFlag(tbl, "all_clients_require_mod")
	// 文件读得通、名字却是空的，是因为这位作者把 name 写成了算出来的表达式
	// （name = ChooseTranslationTable({...})、if 中文 then name = "…" 这类多语言写法，
	// 本机 62 份真实 modinfo 里这一类占了大头）。面板不执行 Lua，挑哪一支都是编名字。
	//
	// 这句是整句直接上界面的，所以措辞必须和 ModinfoOK=false 那两种分开：
	// 文件读得通就不能说"读不懂"，界面上一句自相矛盾的话比留空更让人以为面板坏了。
	if mod.Name == "" {
		mod.Modinfo = "modinfo.lua 读得通，但作者把名称写成了算出来的表达式（多语言模组的常见写法），面板不执行 Lua，读不出模组自己写的名字"
	}
}

func luaText(tbl *dst.LuaTable, key string) string {
	v := tbl.Get(key)
	switch v.Kind {
	case dst.KindString:
		return v.S
	case dst.KindNumber:
		// version = 2 这类写法很常见，整数就按整数输出：
		// 用 %.0f 再裁尾零会把 "20" 裁成 "2"。
		if v.N == math.Trunc(v.N) && math.Abs(v.N) < 1e15 {
			return strconv.FormatInt(int64(v.N), 10)
		}
		return strconv.FormatFloat(v.N, 'f', -1, 64)
	}
	return ""
}

// luaStringList 读 tags 这种"一串字符串"的顶层字段。
// 也有人写成 { QoL = true } 这种键表，那种时候按键名收——标签本身就是要给人看的词。
func luaStringList(tbl *dst.LuaTable, key string) []string {
	v := tbl.Get(key)
	if v.Kind != dst.KindTable || v.T == nil {
		return nil
	}
	var out []string
	for _, item := range v.T.Values() {
		if item.Kind == dst.KindString && item.S != "" {
			out = append(out, item.S)
		}
	}
	if len(out) == 0 {
		out = append(out, v.T.SortedKeys()...)
	}
	return dedupe(out)
}

// luaDependencyGroups 解析 mod_dependencies。实测引擎把它当成"数组，每项又是一张表"：
// modindex.lua:580 逐组取 v.workshop 和数组部分的候选名，任一命中即满足。
// 但第三方模组的写法并不统一，两种形态都要接住：
//
//	mod_dependencies = { { workshop = "workshop-111", "某个本地库目录名" } }
//	mod_dependencies = { "workshop-111" }
//
// 组内一个标识都读不出来时丢掉这一组：宁可少提示，也不凭空造一个"缺依赖"。
func luaDependencyGroups(tbl *dst.LuaTable) [][]string {
	v := tbl.Get("mod_dependencies")
	if v.Kind != dst.KindTable || v.T == nil {
		return nil
	}
	var out [][]string
	add := func(group []string) {
		if len(group) > 0 {
			out = append(out, group)
		}
	}
	for _, item := range v.T.Values() {
		switch {
		case item.Kind == dst.KindString && item.S != "":
			add([]string{item.S})
		case item.Kind == dst.KindTable && item.T != nil:
			var group []string
			if w := item.T.Get("workshop"); w.Kind == dst.KindString && w.S != "" {
				group = append(group, w.S)
			}
			// 表写法里也可能把候选名放在键上（{ ["workshop-1"] = true }）。
			for _, k := range item.T.SortedKeys() {
				if k != "workshop" {
					group = append(group, k)
				}
			}
			for _, sub := range item.T.Values() {
				if sub.Kind == dst.KindString && sub.S != "" {
					group = append(group, sub.S)
				}
			}
			add(dedupe(group))
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// luaFlag 读一个布尔标记。缺键与写成 false 都算假：modinfo.lua 里不写就是没这个属性。
func luaFlag(tbl *dst.LuaTable, key string) bool {
	v := tbl.Get(key)
	if v.Kind == dst.KindBool {
		return v.B
	}
	return false
}

// SetupEntries 读下载清单里已登记的工坊 id，返回去重后的升序列表。
// 集合（ServerModCollectionSetup）也计入，但集合展开需要联网，这里只记它本身。
func (m *Manager) SetupEntries() ([]string, error) {
	text, err := m.readSetup()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, line := range strings.Split(text, "\n") {
		if sub := setupLineRe.FindStringSubmatch(line); sub != nil {
			id := sub[2]
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sortIDs(ids)
	return ids, nil
}

// sortIDs 按数值升序排。纯数字串用字典序会把 999 排在 1000 后面，
// 列表在界面上看着就像随机顺序。
func sortIDs(ids []string) {
	sort.Slice(ids, func(i, j int) bool {
		if len(ids[i]) != len(ids[j]) {
			return len(ids[i]) < len(ids[j])
		}
		return ids[i] < ids[j]
	})
}

// AddSetup 往下载清单追加一条 ServerModSetup。已存在时不动文件，返回 added=false。
//
// 写法刻意保持最简单的一行调用：这个文件是被引擎执行的，
// 我们只往里面放数字 id，别的一概不写。
func (m *Manager) AddSetup(id string) (bool, error) {
	if err := ValidWorkshopID(id); err != nil {
		return false, err
	}
	existing, err := m.SetupEntries()
	if err != nil {
		return false, err
	}
	for _, x := range existing {
		if x == id {
			return false, nil
		}
	}
	text, err := m.readSetup()
	if err != nil {
		return false, err
	}
	eol := detectEOL(text)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += eol
	}
	text += fmt.Sprintf(`ServerModSetup("%s")%s`, id, eol)
	if err := m.writeSetup(text); err != nil {
		return false, err
	}
	return true, nil
}

// RemoveSetup 删掉某个 id 的下载行。返回删了几行；0 表示本来就没登记。
// 只删调用行，Klei 原版注释与其它内容原样保留。
func (m *Manager) RemoveSetup(id string) (int, error) {
	if err := ValidWorkshopID(id); err != nil {
		return 0, err
	}
	text, err := m.readSetup()
	if err != nil {
		return 0, err
	}
	lines := strings.Split(text, "\n")
	var kept []string
	removed := 0
	for _, line := range lines {
		if sub := setupLineRe.FindStringSubmatch(line); sub != nil && sub[2] == id {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		return 0, nil
	}
	if err := m.writeSetup(strings.Join(kept, "\n")); err != nil {
		return 0, err
	}
	return removed, nil
}

// readSetup 读清单原文。文件不存在按"空清单"处理：
// 专用服务端刚装好时这个文件是可以不存在的，面板第一次登记模组就该能成功。
func (m *Manager) readSetup() (string, error) {
	raw, err := os.ReadFile(m.Paths.ServerModSetup())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("读模组下载清单 %s: %w", m.Paths.ServerModSetup(), err)
	}
	return string(raw), nil
}

func (m *Manager) writeSetup(text string) error {
	path := m.Paths.ServerModSetup()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("建模组目录 %s: %w", filepath.Dir(path), err)
	}
	return dst.WriteFileAtomic(path, []byte(text), 0o644)
}

// detectEOL 保住原文件的换行风格。Klei 发的是 LF，用户在 Windows 上编辑过就可能变 CRLF，
// 混着写不会让引擎读错，但会让用户的 diff 变成整片噪音。
func detectEOL(text string) string {
	if strings.Contains(text, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// Downloaded 回答"这个工坊模组的内容在不在磁盘上"（整台机器）。
// 在清单里但没下下来是常态（登记完还没重启过），界面要把这两种分开显示。
//
// 两种来路都要认：mods/workshop-<id> 是手工放置那一套，ugc_mods 下面那份是引擎自己拉的。
// 只认前者的话，刚刚被引擎下载好、日志里也写着 Loading mod 的模组会在界面上变成
// "文件还没下载"——那是把真话说成假话，比缺一个状态更糟。
func (m *Manager) Downloaded(id string) (bool, string) {
	if err := ValidWorkshopID(id); err != nil {
		return false, ""
	}
	dir := m.Paths.WorkshopModDir(id)
	if dirExists(dir) {
		return true, dir
	}
	for _, cp := range m.ugcCopies() {
		if cp.ID == id {
			return true, cp.Path
		}
	}
	return false, dir
}

// DownloadedIn 回答"这个房间的这个世界那一份下载区里有没有它"。
//
// 引擎的下载区是一个世界一份，所以"这台机器上有"和"这个世界有"不是一回事：
// 地表刚重启过、洞穴还没有，洞穴那一份就真的还没下来，开了也不会生效。
// 分片那一屏要的是后一句。
func (m *Manager) DownloadedIn(cluster, shard, id string) bool {
	if err := ValidWorkshopID(id); err != nil {
		return false
	}
	if cluster == "" || shard == "" {
		return false
	}
	return dirExists(m.Paths.UgcModDir(cluster, shard, id))
}

// dirExists 判一个路径是不是目录。
func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// ugcModinfoPath 按目录名（workshop-<id>）在引擎下载区里找那份 modinfo.lua，找不到返回空串。
// 多个世界各有一份时取第一份：同一串 ID 在同一台机器上就是同一个版本，
// 配置项字典两边一致，读哪一份都一样。
func (m *Manager) ugcModinfoPath(dir string) string {
	id := WorkshopIDFromDir(dir)
	if id == "" {
		return ""
	}
	for _, cp := range m.ugcCopies() {
		if cp.ID != id {
			continue
		}
		p := filepath.Join(cp.Path, "modinfo.lua")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// SetupProblems 指出清单里面板看不懂的行。
//
// 为什么要专门查这个：scripts/mods.lua 的 LoadServerModsFile 里，这个文件一旦加载报错
// 就直接 Shutdown()，整个服务端起不来。而文件是允许用户手工编辑的，
// 一行手抖（比如引号写成中文引号）就是全服掉线。面板不拦写，但必须把它显式说出来。
func (m *Manager) SetupProblems() []string {
	text, err := m.readSetup()
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if setupLineRe.MatchString(line) {
			continue
		}
		// 行尾注释是正常的（`ServerModSetup("123") -- 地图`），先去掉注释再判。
		// 这里不改进 setupLineRe：RemoveSetup 要精确到整行，不能被注释牵着删错。
		if idx := strings.Index(trimmed, "--"); idx >= 0 {
			if setupLineRe.MatchString(trimmed[:idx]) {
				continue
			}
		}
		problems = append(problems, fmt.Sprintf("第 %d 行不是面板认识的登记写法：%s", i+1, trimmed))
	}
	return problems
}
