// 开服引导：新建集群、写 Klei 令牌、写与改世界设定。
//
// 这一片补的是"面板只能开关已经存在的集群"这个洞。dst.Paths.CreateCluster 早就写好了
// （挑空闲端口、生成 cluster_key、建 Master/Caves 目录），但没有任何路由引用它，
// 链接器把整块当不可达代码从成品里回收掉了——单测全绿也照样开不了服。
// 云机实测印证了后果：那台机器上 ~/.klei 根本不存在，面板能列出的集群数是 0，
// "开服"这条最基础的闭环在界面上走不通。
//
// 三条写盘路径都遵守同一条顺序：先把所有参数校验完，再动磁盘。
// 建到一半失败会留下一个"有 cluster.ini 没有 server.ini"的集群，
// 那种半成品比失败本身难清理得多。
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

// ---- 世界设定字典 ----

type worldOptionView struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type worldItemView struct {
	Key     string            `json:"key"`
	Label   string            `json:"label"`
	Default string            `json:"default"`
	Options []worldOptionView `json:"options"`
}

type worldGroupView struct {
	Key   string          `json:"key"`
	Label string          `json:"label"`
	Items []worldItemView `json:"items"`
}

type worldLocationView struct {
	Location string           `json:"location"`
	Label    string           `json:"label"`
	Shard    string           `json:"shard"`
	Groups   []worldGroupView `json:"groups"`
	// Presets 是这个地形的可选世界预设。世界文件走 preset 通道之后，
	// "选哪个预设"是比任何单项都高一级的事实：完整世界定义由预设带进来，
	// 界面不给出这张表，用户就没法知道自己是"在标准森林上改了几项"。
	Presets       []worldPresetView `json:"presets"`
	DefaultPreset worldPresetView   `json:"defaultPreset"`
	// ItemCount 让界面能先报"这张表有 120 项"再决定怎么分组渲染，
	// 不至于点开才知道是一屏塞不下的长列表。
	ItemCount int `json:"itemCount"`
}

// worldPresetView 是一个世界预设。Value 是写进文件的那个字符串，
// Label 才是给使用者看的；三个字段各管一件事，缺一个都会在界面上变成猜。
type worldPresetView struct {
	Value   string `json:"value"`
	Label   string `json:"label"`
	English string `json:"english"`
	Note    string `json:"note,omitempty"`
}

func presetView(p dst.WorldPreset) worldPresetView {
	return worldPresetView{Value: p.ID, Label: p.Text, English: p.English, Note: p.Note}
}

// locationLabel 只有两个取值，直接写死比再造一张表清楚。
// 界面拿到的是这句中文，不是 forest/cave 这两个码值。
func locationLabel(location string) string {
	switch location {
	case dst.LocationCave:
		return "洞穴"
	case dst.LocationForest:
		return "地表（森林）"
	}
	return location
}

func schemaView(loc dst.Location) []worldGroupView {
	out := make([]worldGroupView, 0, len(loc.Groups))
	for _, g := range loc.Groups {
		gv := worldGroupView{Key: g.Key, Label: g.Text, Items: make([]worldItemView, 0, len(g.Items))}
		for _, it := range g.Items {
			iv := worldItemView{Key: it.Key, Label: it.Text, Default: it.Default,
				Options: make([]worldOptionView, 0, len(it.Options))}
			for _, o := range it.Options {
				iv.Options = append(iv.Options, worldOptionView{Value: o.Value, Label: o.Text})
			}
			gv.Items = append(gv.Items, iv)
		}
		out = append(out, gv)
	}
	return out
}

// handleWorldSchema 把字典原样交给界面。界面据此渲染下拉框，
// 提交回来的码值再由 handleWorldPut 用同一份字典校验——两头共用一张表，
// 不会出现"界面选得出来、后端说非法"。
func (s *Server) handleWorldSchema(w http.ResponseWriter, r *http.Request) {
	lang := r.URL.Query().Get("lang")
	if lang == "" {
		lang = "zh"
	}
	out := make([]worldLocationView, 0, 2)
	for _, location := range []string{dst.LocationForest, dst.LocationCave} {
		loc, ok := s.World.Location(location, lang)
		if !ok {
			s.fail(w, r, http.StatusInternalServerError,
				fmt.Errorf("世界规则字典里没有地形 %q，成品内的字典文件可能不完整", location))
			return
		}
		v := worldLocationView{Location: location, Label: locationLabel(location),
			Shard: dst.ShardForLocation(location), Groups: schemaView(loc)}
		for _, g := range loc.Groups {
			v.ItemCount += len(g.Items)
		}
		for _, pr := range dst.PresetsForLocation(location) {
			v.Presets = append(v.Presets, presetView(pr))
		}
		// 默认预设单独给一份：新建集群时用户什么都不选，落进文件的就是它。
		if def, ok := dst.PresetByID(dst.DefaultPreset(location)); ok {
			v.DefaultPreset = presetView(def)
		}
		out = append(out, v)
	}
	writeOK(w, out)
}

// ---- 新建集群 ----

type clusterGameRequest struct {
	DisplayName    *string `json:"displayName"`
	Description    *string `json:"description"`
	Password       *string `json:"password"`
	GameMode       *string `json:"gameMode"`
	Intention      *string `json:"intention"`
	MaxPlayers     *int    `json:"maxPlayers"`
	Pvp            *bool   `json:"pvp"`
	PauseWhenEmpty *bool   `json:"pauseWhenEmpty"`
	LanOnly        *bool   `json:"lanOnly"`
	OfflineCluster *bool   `json:"offlineCluster"`
	Language       *string `json:"language"`
	TickRate       *int    `json:"tickRate"`
	ConsoleEnabled *bool   `json:"consoleEnabled"`
}

type clusterWorldRequest struct {
	// Presets 按分片给世界预设：地表和洞穴各自一张表，混着填会被当场拒掉
	// （引擎查不到预设名时不报错，只是静默换一张图生成）。留空用该地形的默认预设。
	Presets map[string]string `json:"presets"`
	// Overrides 按分片给：地表和洞穴是两张不同的表（洞穴没有季节、 day_length 那些项）。
	Overrides map[string]map[string]string `json:"overrides"`
	// FlatOverrides 允许只写一份，自动落到两个分片里各自合法的那些项。
	// 界面上"新建集群"通常是一屏表单，分片切换反而容易填错地方。
	FlatOverrides map[string]string `json:"flatOverrides"`
	// 这里不再有 name/desc：世界名与世界说明只有 leveldataoverride.lua（GAME USE，
	// 要一整份完整世界定义）能带，面板走的 preset 通道不认这两个顶层键。
	// 玩家在服务器列表上看到的是集群名与分片名，那两个由 game.displayName 和
	// server.ini 管，界面里不重复给一个不起作用的名字。
}

type clusterCreateRequest struct {
	// Name 是目录名（Cluster_N）。留空由面板取下一个空号。
	Name      *string `json:"name"`
	NumShards *int    `json:"numShards"`
	Token     *string `json:"token"`
	// InstallHelper 默认 true：没有助手 MOD 的集群在面板里是一台"起得来但没有状态"的服务器。
	InstallHelper *bool `json:"installHelper"`
	// GamePort 是 Master 那个世界的游戏端口。留空由面板在配置区间里挑空闲号；
	// 填了就必须连它的配套端口（+1）一起空着，否则洞穴连不上地面。
	// 之所以让用户填：一台机器放好几个房间时，他要照着这个号去开云安全组。
	GamePort int                  `json:"gamePort"`
	Game     clusterGameRequest   `json:"game"`
	World    *clusterWorldRequest `json:"world"`
}

type createdShardView struct {
	Shard      string `json:"shard"`
	Port       int    `json:"port"`
	Location   string `json:"location"`
	LocationZh string `json:"locationLabel"`
	// Preset 是这次真正写进 worldgenoverride.lua 的预设：码值留着便于对着文件排查，
	// 界面上用的是 PresetZh。
	Preset    string `json:"preset,omitempty"`
	PresetZh  string `json:"presetLabel,omitempty"`
	WorldFile bool   `json:"worldFileWritten"`
	Helper    bool   `json:"helperEnabled"`
}

type clusterCreateReply struct {
	Name   string             `json:"name"`
	Dir    string             `json:"dir"`
	Shards []createdShardView `json:"shards"`
	// Notes 是"接下来会发生什么"，界面上直接列出来。开服这一步最怕的是
	// 用户以为配错了，其实是第一次生成世界要等两三分钟。
	Notes []string `json:"notes"`
}

// 白名单：这几项的值会被写进 cluster.ini，而 Klei 对认不出的取值是"静默按默认走"，
// 所以宁可当场 400，也不要生成一个"看着没生效"的集群。
// 允许值直接取自服务端自带 cluster.ini 模板里那几行注释。
var (
	gameModes  = map[string]bool{"survival": true, "endless": true, "wilderness": true}
	intentions = map[string]bool{"cooperative": true, "competitive": true, "social": true, "madness": true}
	languages  = map[string]bool{"zh": true, "en": true}
	tickRates  = map[string]bool{"15": true, "20": true, "30": true}
)

// validClusterDirName 限制集群目录名。这个字符串会变成 kleiRoot 下的一个目录名，
// 所以只允许字母数字下划线短横线，且不许以点开头——
// 光挡 "/" 和 "\" 不够，".." 本身就是个合法目录名。
func validClusterDirName(name string) error {
	if name == "" || len(name) > 32 {
		return fmt.Errorf("%w: 集群目录名要 1~32 个字符，收到的是 %q", dst.ErrInvalidArg, name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: 集群目录名不能以点开头: %q", dst.ErrInvalidArg, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return fmt.Errorf("%w: 集群目录名只能用字母、数字、下划线、短横线，%q 里有 %q",
				dst.ErrInvalidArg, name, string(r))
		}
	}
	return nil
}

func (req clusterGameRequest) toSettings(displayName string) (dst.GameSettings, error) {
	set := dst.GameSettings{
		ClusterName: displayName, PauseWhenEmpty: true, ConsoleEnabled: true,
		GameMode: "endless", Intention: "cooperative", MaxPlayers: 6,
		Language: "zh", TickRate: 15, MaxSnapshots: 20,
	}
	if req.DisplayName != nil {
		set.ClusterName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Description != nil {
		set.Description = *req.Description
	}
	if req.Password != nil {
		set.Password = strings.TrimSpace(*req.Password)
	}
	if req.GameMode != nil {
		set.GameMode = *req.GameMode
	}
	if req.Intention != nil {
		set.Intention = *req.Intention
	}
	if req.MaxPlayers != nil {
		set.MaxPlayers = *req.MaxPlayers
	}
	if req.Pvp != nil {
		set.PVP = *req.Pvp
	}
	if req.PauseWhenEmpty != nil {
		set.PauseWhenEmpty = *req.PauseWhenEmpty
	}
	if req.LanOnly != nil {
		set.LanOnly = *req.LanOnly
	}
	if req.OfflineCluster != nil {
		set.OfflineCluster = *req.OfflineCluster
	}
	if req.Language != nil {
		set.Language = *req.Language
	}
	if req.TickRate != nil {
		set.TickRate = *req.TickRate
	}
	if req.ConsoleEnabled != nil {
		set.ConsoleEnabled = *req.ConsoleEnabled
	}
	return set, validateGameSettings(set)
}

// validateGameSettings 是 cluster.ini 那批"认不出就静默按默认走"的项目白名单。
//
// 新建集群和建好之后修改共用这一份：两边写的是同一个文件，校验各写一套迟早会
// 一边松一边紧——建的时候拦得住、改的时候放过去，用户就多了一个"看着没生效"的现场。
func validateGameSettings(set dst.GameSettings) error {
	return validateGameSettingsFields(set, nil)
}

// validateGameSettingsFields 只校验 changed 里点到名的项；changed 为 nil 表示全查。
//
// 建好之后再改必须走这一条。文件里可能本来就有一项是面板不认识的取值
// （手工改过、或早先版本写的），而界面明说了"不动它就保持原样"；拿整份配置去跑
// 全量校验的话，用户改个名字都会被那一项挡住，这一页等于废了。
func validateGameSettingsFields(set dst.GameSettings, changed map[string]bool) error {
	want := func(name string) bool { return changed == nil || changed[name] }
	if want("ClusterName") {
		if set.ClusterName == "" {
			return fmt.Errorf("%w: 服务器名称（界面上的 displayName）不能为空，玩家列表里显示的就是它", dst.ErrInvalidArg)
		}
		if len([]rune(set.ClusterName)) > 64 {
			return fmt.Errorf("%w: 服务器名称最多 64 个字，当前 %d 个", dst.ErrInvalidArg, len([]rune(set.ClusterName)))
		}
	}
	// 这三项是自由文本，会一行一个键写进 cluster.ini。值里带换行等于往配置文件里
	// 插行——下一个键可以是 console_enabled，也可以是别人的 [SECTION]，
	// 所以非要在名称里换行的话只能拒绝，不能"顺手过滤掉"。
	for _, kv := range []struct {
		key   string
		label string
		value string
	}{
		{"ClusterName", "服务器名称", set.ClusterName},
		{"Description", "服务器说明", set.Description},
		{"Password", "连接密码", set.Password},
	} {
		if !want(kv.key) {
			continue
		}
		if strings.ContainsAny(kv.value, "\r\n") {
			return fmt.Errorf("%w: %s里不能有换行，它会占住 cluster.ini 的一行，换行后面的内容会被服务端当成新的配置项",
				dst.ErrInvalidArg, kv.label)
		}
	}
	if want("GameMode") && !gameModes[set.GameMode] {
		return fmt.Errorf("%w: 游戏模式只能是 survival、endless、wilderness，收到的是 %q", dst.ErrInvalidArg, set.GameMode)
	}
	if want("Intention") && !intentions[set.Intention] {
		return fmt.Errorf("%w: 游戏偏好只能是 cooperative、competitive、social、madness，收到的是 %q",
			dst.ErrInvalidArg, set.Intention)
	}
	if want("Language") && !languages[set.Language] {
		return fmt.Errorf("%w: 服务器语言面板只认 zh 和 en，收到的是 %q", dst.ErrInvalidArg, set.Language)
	}
	if want("MaxPlayers") {
		if set.MaxPlayers < 1 {
			return fmt.Errorf("%w: 最大玩家数至少得是 1，收到 %d——填 0 不会变成\"只有房主\"，服务端会直接按默认值走",
				dst.ErrInvalidArg, set.MaxPlayers)
		}
		if set.MaxPlayers > 16 {
			return fmt.Errorf("%w: 最大玩家数的上限是 16（服务端模板注释里写明的），收到 %d",
				dst.ErrInvalidArg, set.MaxPlayers)
		}
	}
	if want("TickRate") && !tickRates[fmt.Sprint(set.TickRate)] {
		return fmt.Errorf("%w: 每秒通信次数只能填 15、20、30，收到 %d；越高越顺，但 4G 小机建议留在 15",
			dst.ErrInvalidArg, set.TickRate)
	}
	if want("WhitelistSlots") && set.WhitelistSlots < 0 {
		return fmt.Errorf("%w: 白名单保留位不能是负数，收到 %d", dst.ErrInvalidArg, set.WhitelistSlots)
	}
	// 这是一条跨字段约束：只改其中一边也可能把这一对弄矛盾，所以任一边变了都要查。
	if (want("WhitelistSlots") || want("MaxPlayers")) && set.WhitelistSlots > set.MaxPlayers {
		return fmt.Errorf("%w: 白名单保留位（%d）不能超过最大玩家数（%d），否则永远有一批位置空着谁都不能进",
			dst.ErrInvalidArg, set.WhitelistSlots, set.MaxPlayers)
	}
	return nil
}

// resolveWorldSpecs 把请求里的世界设定摊到每个分片上，并按字典校验。
// 返回的 map 键是分片名；没有 world 请求时为 nil（那就完全不写世界设定文件）。
func (s *Server) resolveWorldSpecs(req *clusterWorldRequest, shards []string) (map[string]dst.WorldSpec, error) {
	if req == nil {
		return nil, nil
	}
	out := make(map[string]dst.WorldSpec, len(shards))
	for _, shard := range shards {
		location := dst.ShardLocation(shard)
		preset, ok := req.Presets[shard]
		if !ok || strings.TrimSpace(preset) == "" {
			preset = dst.DefaultPreset(location)
		} else {
			preset = strings.TrimSpace(preset)
		}
		// 预设当场拒：引擎查不到预设名时只打一句 "nonexistent preset" 就按别的生成，
		// 等开服以后才发现"我选的预设没生效"是最难查的那种现场。
		if !dst.IsKnownPreset(location, preset) {
			return nil, fmt.Errorf("%w: %s 世界（%s）可用的世界预设是 %s，收到的是 %q",
				dst.ErrInvalidArg, shard, locationLabel(location),
				dst.PresetChoicesText(location), preset)
		}
		// 和 handleWorldPut 一样写全整套默认值，理由见那里的注释。
		merged := s.World.DefaultOverrides(location, "zh")
		// 平铺的那份先套：只保留这个地形认识的键，其余留给分片级覆盖，
		// 免得"洞穴也有 day_length"这种填错在两个分片里各生成一份怪文件。
		loc, _ := s.World.Location(location, "zh")
		for k, v := range req.FlatOverrides {
			if _, ok := loc.Item(k); ok {
				merged[k] = v
			}
		}
		for k, v := range req.Overrides[shard] {
			merged[k] = v
		}
		if problems := s.World.CheckOverrides(location, "zh", merged); len(problems) > 0 {
			var errs []string
			for _, p := range problems {
				if p.Level == "error" {
					errs = append(errs, p.Key+": "+p.Reason)
				}
			}
			if len(errs) > 0 {
				return nil, fmt.Errorf("%w: %s 世界的世界规则有问题：%s",
					dst.ErrInvalidArg, shard, strings.Join(errs, "；"))
			}
		}
		out[shard] = dst.WorldSpec{Preset: preset, Overrides: merged}
	}
	return out, nil
}

func (s *Server) handleClusterCreate(w http.ResponseWriter, r *http.Request) {
	var req clusterCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}

	numShards := 2
	if req.NumShards != nil {
		numShards = *req.NumShards
	}
	if numShards != 1 && numShards != 2 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"世界数量只支持 1（只有地表）或 2（地表 + 洞穴），收到 %d；洞穴要单独开服是做不到的，它必须挂在 Master 上", numShards))
		return
	}
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		next, err := s.Paths.NextClusterName()
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		name = next
	}
	if err := validClusterDirName(name); err != nil {
		writeErr(w, http.StatusBadRequest, publicError(err))
		return
	}
	settings, err := req.Game.toSettings(name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, publicError(err))
		return
	}
	shards := []string{dst.ShardMaster}
	if numShards == 2 {
		shards = append(shards, dst.ShardCaves)
	}
	worlds, err := s.resolveWorldSpecs(req.World, shards)
	if err != nil {
		writeErr(w, http.StatusBadRequest, publicError(err))
		return
	}
	// 令牌也在这里验：宁可一个字节都不写，也不要建好目录之后才发现令牌是粘贴错的。
	token := ""
	if req.Token != nil {
		token = strings.TrimSpace(*req.Token)
	}
	if token != "" {
		if err := dst.ValidateToken(token); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	wantPort := req.GamePort
	if wantPort != 0 && (wantPort < minGamePort || wantPort > maxGamePort) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"游戏端口要填 %d 到 %d 之间的数字，收到 %d；1024 以下需要 root 权限，最后一个号要留给配套端口。留空则由面板自动挑一个空闲号",
			minGamePort, maxGamePort, wantPort))
		return
	}
	// 撞号在这儿先说一遍：底层那条错误说的是"[SHARD]master_port 被 Cluster_1/Master 占用"，
	// 那是我们排查时想要的字符串，不是客户看得懂的话；而向导里同一件事已经有一句人话了，
	// 两边说法必须一样，否则用户会以为是两个不同的检查。
	if wantPort > 0 {
		if owners, oerr := s.portOwners(); oerr == nil {
			for _, o := range owners {
				if o.Port != wantPort && o.Port != wantPort+1 {
					continue
				}
				which := "游戏端口"
				if o.Port != wantPort {
					which = "配套端口"
				}
				writeErr(w, http.StatusBadRequest, fmt.Sprintf(
					"%d 这组号用不了：%d 已被 %s 的%s世界占用（它是那边的%s）。"+
						"换一组后面空着的号，或者这一栏留空让面板挑",
					wantPort, o.Port, o.Room, o.World, which))
				return
			}
		}
	}
	cluster, err := s.Paths.CreateClusterAt(name, settings, numShards,
		s.Network.ShardPortBase, s.Network.ShardPortEnd, wantPort)
	if err != nil {
		if errors.Is(err, dst.ErrInvalidArg) || strings.Contains(err.Error(), "已存在") {
			writeErr(w, http.StatusBadRequest, publicError(err))
			return
		}
		s.audit(r, "cluster.create", name, "", err)
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	var notes []string
	failStep := func(step string, err error) {
		s.audit(r, "cluster.create", name, step, err)
		s.log().Error("集群已创建但后续步骤失败", "cluster", name, "step", step, "err", err.Error())
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf(
			"集群 %s 的目录与 cluster.ini 已经建好，但「%s」这一步失败了：%s。目录没有回滚（怕删掉不该删的东西），修好这一项再重试时它会报\"已存在\"",
			name, step, publicError(err)))
	}

	if token != "" {
		if err := cluster.SetToken(token); err != nil {
			failStep("写入 Klei 令牌", err)
			return
		}
	}
	installHelper := true
	if req.InstallHelper != nil {
		installHelper = *req.InstallHelper
	}

	created := make([]createdShardView, 0, len(shards))
	for _, shard := range shards {
		cv := createdShardView{Shard: shard, Port: cluster.Shard(shard).ServerPort,
			Location: dst.ShardLocation(shard)}
		cv.LocationZh = locationLabel(cv.Location)
		if ws, ok := worlds[shard]; ok {
			if err := s.Paths.WriteWorldGen(name, shard, ws); err != nil {
				failStep("写入世界规则文件", err)
				return
			}
			cv.WorldFile = true
			cv.Preset = ws.Preset
			cv.PresetZh = dst.PresetLabel(ws.Preset)
		}
		if installHelper {
			if _, err := s.Bridge.EnableHelper(name, shard, true, s.Bridge.Helper); err != nil {
				failStep("安装助手 MOD", err)
				return
			}
			cv.Helper = true
		}
		created = append(created, cv)
	}

	if token == "" {
		notes = append(notes, "还没有写 Klei 令牌：这个集群现在启起来会被服务端拒绝，"+
			"日志原话是「!!!! Your Server Will Not Start !!!!」。令牌要在 Klei 个人中心的服务器页面上生成，填在这里或之后的令牌接口")
	}
	if req.World == nil {
		notes = append(notes, "没有写世界规则文件，引擎会用它自己的默认世界"+
			"（日志里是 Not applying level data overrides 加 Not applying world gen overrides）；"+
			"要改世界大小、季节这些，用世界规则接口")
	} else {
		notes = append(notes, "世界名字用的是所选预设自带的那个（例如「标准森林」）：服务器列表上玩家看到的房间名是集群名与世界名，"+
			"这两个已经按你填的走了。世界定义里的 name 只有引擎客户端下发的整份完整世界定义能带，"+
			"面板走的是预设通道，不重复给一个不起作用的名字")
	}
	if installHelper {
		notes = append(notes, "助手 MOD 的配置已写入两个世界，世界第一次启动后才会开始回传状态")
	}
	notes = append(notes, "第一次启动要生成世界，地表通常一到两分钟、洞穴再久一点；这期间状态页显示\"助手还没回话\"是正常的，不是启动失败")
	// 端口在这里说一次最省事：用户接下来要拿去开云安全组，而这两个号他自己在文件里找不到。
	if ports := portNotes(created); ports != "" {
		note := "防火墙与云安全组要放行的 UDP 号：" + ports + "（每个世界各占两个号，后面那个是引擎自己绑的配套端口）"
		// 区间外的号是用户自己挑的，不是面板挑错的：不说一句，以后自动挑号
		// 永远挑不到它，他会以为面板坏了。
		if wantPort > 0 && (wantPort < s.Network.ShardPortBase || wantPort > s.Network.ShardPortEnd) {
			note += fmt.Sprintf("。地表的 %d 在面板默认区间 %d-%d 之外，用着没问题，但面板以后自动挑号不会再挑到它",
				wantPort, s.Network.ShardPortBase, s.Network.ShardPortEnd)
		}
		notes = append(notes, note)
	}
	s.audit(r, "cluster.create", name, fmt.Sprintf("世界 %d 个，令牌 %s，世界规则文件 %s",
		numShards, tokenPresence(token), mapKeysPresent(worlds)), nil)
	writeOK(w, clusterCreateReply{Name: name, Dir: s.Paths.ClusterDir(name), Shards: created, Notes: notes})
}

// portNotes 把"要放行哪些号"拼成一句人话，例如 "10999、11000、11001、11002"。
func portNotes(shards []createdShardView) string {
	seen := map[int]bool{}
	var parts []string
	for _, sh := range shards {
		for _, p := range []int{sh.Port, sh.Port + 1} {
			if sh.Port <= 0 || seen[p] {
				continue
			}
			seen[p] = true
			parts = append(parts, strconv.Itoa(p))
		}
	}
	return strings.Join(parts, "、")
}

// tokenPresence 只描述"有没有给"，永远不把令牌本身或其前缀写进审计。
func tokenPresence(token string) string {
	if token == "" {
		return "未提供"
	}
	return fmt.Sprintf("已写入（%d 位）", len(token))
}

func mapKeysPresent(m map[string]dst.WorldSpec) string {
	if len(m) == 0 {
		return "未写"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "已写 " + strings.Join(keys, "、")
}

// ---- Klei 令牌 ----

type tokenRequest struct {
	Token string `json:"token"`
}

type tokenView struct {
	Configured bool   `json:"configured"`
	Length     int    `json:"length"`
	Path       string `json:"path"`
	Hint       string `json:"hint,omitempty"`
}

// clusterScope 和 shardScope 是同一条思路，只是这几个接口的 URL 里没有分片段：
// {name} 来自请求，先跟磁盘上的集群名单对一遍，再决定动哪个目录。
// 靠 OpenCluster 的报错文本去猜"不存在"是不行的——那句中文是 ReadTextFile 拼的，
// 换个文件、换条错误就漏判，等于让请求决定读哪棵树。
func (s *Server) clusterScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	names, err := s.Paths.ClusterNames()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return "", false
	}
	if !contains(names, name) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("集群 %s 不存在，现有：%s", name, strings.Join(names, "、")))
		return "", false
	}
	return name, true
}

// handleTokenShow/handleTokenPut 是令牌唯一的出入口。
// 响应里只有"配没配、多长"，明文既不进 JSON 也不进审计与运行日志。
func (s *Server) handleTokenShow(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	c, err := dst.OpenCluster(s.Paths, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	token, err := c.Token()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, tokenView{Configured: token != "", Length: len(token), Path: s.Paths.TokenFile(cluster)})
}

func (s *Server) handleTokenPut(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	var req tokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	c, err := dst.OpenCluster(s.Paths, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	token := strings.TrimSpace(req.Token)
	if err := c.SetToken(token); err != nil {
		// ValidateToken 的每条结论都是中文且指出了改法，直接给界面；
		// 不是参数问题就是写盘问题，那种要进日志而不是当用户填错。
		if errors.Is(err, dst.ErrInvalidArg) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.audit(r, "cluster.token", cluster, "写入失败", err)
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "cluster.token", cluster, fmt.Sprintf("写入令牌 %d 位（明文不留存）", len(token)), nil)
	writeOK(w, tokenView{Configured: true, Length: len(token), Path: s.Paths.TokenFile(cluster),
		Hint: "文件权限 0600；改完不需要重启面板，下次启动世界时生效"})
}

// handleTokenDelete 是"摘下令牌"的唯一入口。
//
// 为什么不用 PUT 一个空串来表达：界面上"我没粘上"和"我要清空"在请求体里长得一模一样，
// 而后者会让整个房间上不了列表。清空必须换一个动作，误触不了。
func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	c, err := dst.OpenCluster(s.Paths, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := c.ClearToken(); err != nil {
		s.audit(r, "cluster.token", cluster, "清空失败", err)
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "cluster.token", cluster, "清空令牌（连 cluster_token.txt.bak 一起删）", nil)
	writeOK(w, tokenView{Path: s.Paths.TokenFile(cluster),
		Hint: "令牌已摘下：这个房间再启动会被引擎拒绝上列表（日志里 !!!! Your Server Will Not Start !!!!），" +
			"正在跑的世界不会因此掉线。已经在 Klei 那侧注册过的服务器条目要自己去个人中心下架。"})
}

// ---- 世界设定 ----

type worldView struct {
	Cluster  string `json:"cluster"`
	Shard    string `json:"shard"`
	Location string `json:"location"`
	// Present 为假表示没有 worldgenoverride.lua，引擎用的是预设自带的默认世界。
	Present bool `json:"present"`
	// Preset 是文件里写的预设码值（排查时要照着它对文件），PresetZh 是给界面显示的。
	// 读回来的码值可能根本不在面板的可选表里（手工改过文件），那种原样给出、由 Note 说明，
	// 而不是悄悄换成默认预设——那会让人以为自己的手工改动被引擎接受了。
	Preset        string            `json:"preset,omitempty"`
	PresetZh      string            `json:"presetLabel,omitempty"`
	Overrides     map[string]string `json:"overrides"`
	Changed       []string          `json:"changedFromDefault"`
	Note          string            `json:"note,omitempty"`
	Path          string            `json:"path"`
	LevelConflict string            `json:"levelDataConflict,omitempty"`

	// 下面三个只在"刚写过 worldgenoverride.lua"的那次响应里给（omitempty）。
	// 改世界规则永远不会立刻重造现有地图，这个口径以前只写在 Note 那段中文里，
	// 前端要拿它做判断得解析句子；这里补成和 cluster_settings.go 同一套结构化字段。
	Effective string `json:"effective,omitempty"`
	// RegenerateRequested 回执请求里那个提醒开关，RegenerateApplied 恒为 false：
	// 面板不替用户按重生世界的命令。以前这个开关只在 Note 末尾加一句括号说明，
	// 前端读不到"后端到底收没收到"，等于被静默吞掉。
	RegenerateRequested bool `json:"regenerateRequested,omitempty"`
	RegenerateApplied   bool `json:"regenerateApplied,omitempty"`
}

func (s *Server) buildWorldView(cluster, shard string) (worldView, error) {
	spec, present, err := s.Paths.ReadWorldGen(cluster, shard)
	if err != nil {
		return worldView{}, err
	}
	location := dst.ShardLocation(shard)
	v := worldView{Cluster: cluster, Shard: shard, Location: location, Present: present,
		Preset: spec.Preset, Overrides: spec.Overrides,
		Path: s.Paths.WorldGenOverride(cluster, shard)}
	if present && spec.Preset != "" {
		v.PresetZh = dst.PresetLabel(spec.Preset)
	}
	def := s.World.DefaultOverrides(location, "zh")
	changed := make([]string, 0, len(v.Overrides))
	for k, val := range v.Overrides {
		if d, ok := def[k]; ok && d != val {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	v.Changed = changed
	v.LevelConflict = s.Paths.LevelDataNote(cluster, shard)
	if !present {
		v.Note = "还没有世界规则文件，这个世界用的是引擎默认世界"
		return v, nil
	}
	v.Note = fmt.Sprintf("文件里写了 %d 项，其中 %d 项和字典默认值不一样", len(v.Overrides), len(changed))
	if spec.EngineRewritten {
		// 不说这一句，用户看到的就是"我存了 189 项，回来只剩 16 项"，
		// 第一反应会是面板把设定弄丢了。事实是引擎每次存世界都会重写这份文件
		//（mainfunctions.lua 的世界保存回调里那句 "copy overrides from the world back to
		// the shard index and worldgenoverrides" + UpdateWorldGenOverride），
		// 它只把当前存档里带的那些项写回来，没写出来的走预设默认值。
		v.Note += "；这份文件是世界存档时（自动存档、立即保存或停服）引擎按当前存档重写过的版本：" +
			"它把 preset 换成了 worldgen_preset 与 settings_preset，overrides 只留下存档里带的那些项，" +
			"没写出来的走预设默认值，不是面板把你的设定删了"
	}
	if spec.Preset == "" {
		v.Note += "；这份文件里没有 preset，引擎只把 overrides 并进当前存档，不会重造世界"
	} else if !dst.IsKnownPreset(location, spec.Preset) {
		v.Note += "；preset 不是这个世界地形可用的预设，引擎查不到它会按当前存档生成（日志里一句 nonexistent preset）"
	}
	return v, nil
}

func (s *Server) handleWorldShow(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	v, err := s.buildWorldView(cluster, shard)
	if err != nil {
		s.failShard(w, r, cluster, shard, err)
		return
	}
	writeOK(w, v)
}

type worldPutRequest struct {
	Preset    *string           `json:"preset"`
	Overrides map[string]string `json:"overrides"`
	// Regenerate 只是界面上的提醒开关：面板不会替用户重生世界，
	// 那是一条会清掉当前存档的危险命令，必须走白名单命令接口单独确认。
	Regenerate bool `json:"regenerate"`
}

func (s *Server) handleWorldPut(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	var req worldPutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	location := dst.ShardLocation(shard)
	old, present, err := s.Paths.ReadWorldGen(cluster, shard)
	if err != nil {
		s.failShard(w, r, cluster, shard, err)
		return
	}
	preset := old.Preset
	if req.Preset != nil && strings.TrimSpace(*req.Preset) != "" {
		preset = strings.TrimSpace(*req.Preset)
		if !dst.IsKnownPreset(location, preset) {
			// 用户明确点了某个预设却拼错、或把洞穴的预设点到地表：当场拒，
			// 不替他换成默认值。
			writeErr(w, http.StatusBadRequest, fmt.Sprintf(
				"%s 世界（%s）可用的世界预设是 %s，收到的是 %q",
				shard, locationLabel(location), dst.PresetChoicesText(location), preset))
			return
		}
	} else if present && (preset == "" || !dst.IsKnownPreset(location, preset)) {
		// 文件是手工做的：要么没有 preset（引擎只把 overrides 并进当前存档），
		// 要么这个分片用不了里面那个 preset（拷来的）。这次提交又没点预设，
		// 面板补一个默认值上去等于把"部分合并"改成"整张覆盖"，是替用户换了世界图。
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"%s 世界（%s）的 worldgenoverride.lua 里 preset 是 %q，不是这个世界直接可用的预设；"+
				"可用的是 %s。确认要换成哪一个再提交，面板不替你猜",
			shard, locationLabel(location), preset, dst.PresetChoicesText(location)))
		return
	}
	// 写全，不写增量：默认值原样赋给自己在引擎那边是 no-op
	// （scripts/map/customize.lua 里是 option.default = locationdata.overrides[option.name]），
	// 但换来的好处实在——用户打开 worldgenoverride.lua 看到的就是当前生效的完整状态，
	// 不用一边看文件一边猜"没写出来的那一项到底是默认还是被谁动过"。
	merged := s.World.DefaultOverrides(location, "zh")
	for k, v := range old.Overrides {
		// 只接字典认识的那些项：手工文件里残留的 layout_mode / has_ocean 这批
		// 是 leveldataoverride 时代的写法，搬进这份文件只会在引擎日志里换一条
		// "WARNING! Found override ... doesn't match any known option"，
		// 而且它们已经由预设带进来了。
		if _, known := s.World.Item(location, "zh", k); !known {
			continue
		}
		merged[k] = v
	}
	for k, v := range req.Overrides {
		merged[k] = v
	}
	// 校验整份要落盘的表，而不是只校验这次提交的那几项：
	// 默认值那张表本身就是合法的（world_test 里逐项验过），
	// 老文件里模组自己加的键只会得到 warn，不会被拦下。
	if problems := s.World.CheckOverrides(location, "zh", merged); len(problems) > 0 {
		var errs, warns []string
		for _, p := range problems {
			switch p.Level {
			case "error":
				errs = append(errs, p.Key+": "+p.Reason)
			default:
				warns = append(warns, p.Key+": "+p.Reason)
			}
		}
		if len(errs) > 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("世界规则校验未通过：%s", strings.Join(errs, "；")))
			return
		}
		if len(warns) > 0 {
			// 不拦，但也不能一句话不说：这些键会原样写进 Lua，多半是手工改过文件或模组加的。
			s.log().Warn("世界规则里有字典不认识的键", "cluster", cluster, "shard", shard,
				"keys", strings.Join(warns, "；"))
		}
	}
	spec := dst.WorldSpec{Preset: preset, Overrides: merged}
	if err := s.Paths.WriteWorldGen(cluster, shard, spec); err != nil {
		s.failShard(w, r, cluster, shard, err)
		return
	}
	s.audit(r, "world.update", cluster+"/"+shard,
		fmt.Sprintf("写入 %d 项设定，预设 %s", len(merged), preset), nil)
	v, err := s.buildWorldView(cluster, shard)
	if err != nil {
		s.failShard(w, r, cluster, shard, err)
		return
	}
	// 口径用结构化字段直说，不再只塞在一段中文里：改世界规则只影响下一次生成，
	// 现有地图一点没动。regenerate 这个开关也明确回执"收到了、但没执行"，
	// 不让它像以前那样只在 Note 末尾留一句括号、前端无从判断。
	v.Effective = effectiveNextWorldgen
	v.RegenerateRequested = req.Regenerate
	v.RegenerateApplied = false
	v.Note = "本次修改只影响下一次世界生成，现有地图不变：改世界规则不会重造已经跑着的存档。" +
		"要重做世界得用控制台里那条重生世界的命令（会清当前存档，得单独确认），面板不会替你按下去；" +
		"世界名字用的是预设自带的那个，要改玩家看到的房间名，改集群设置里的显示名"
	if req.Regenerate {
		v.Note += "；请求里的 regenerate=true 已收到，但它只是提醒开关，面板依旧没有重生世界（regenerateApplied=false）"
	}
	writeOK(w, v)
}
