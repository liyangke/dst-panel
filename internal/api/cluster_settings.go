// 建好之后修改集群设置（cluster.ini）。
//
// 新建集群时那批玩法参数只写一次，之后想改名字、密码、人数就得手工开文件——
// 这是 README 里明写的缺口。这一片把它补上，并且刻意只碰"改了不影响目录结构"的项：
// 端口不在这里，它牵动 cluster.ini 与两个 server.ini、改前必须停服，单独走 /ports（见 ports.go）。
//
// 校验与新建共用一份（见 bootstrap.go 的 validateGameSettingsFields），两边写的是
// 同一个文件，各写一套迟早会一边松一边紧；区别只在改的时候只查这次点过名的项，
// 免得文件里本来就认不出的取值把改名也挡住。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"dstpanel/internal/dst"
)

type clusterSettingsView struct {
	Cluster     string `json:"cluster"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	// HasPassword 是"有没有设密码"，原值永远不出接口：
	// 密码框回填不了内容不是缺陷，是这一项本来就不该能被读回来。
	HasPassword     bool   `json:"hasPassword"`
	GameMode        string `json:"gameMode"`
	Intention       string `json:"intention"`
	MaxPlayers      int    `json:"maxPlayers"`
	Pvp             bool   `json:"pvp"`
	PauseWhenEmpty  bool   `json:"pauseWhenEmpty"`
	VoteEnabled     bool   `json:"voteEnabled"`
	VoteKickEnabled bool   `json:"voteKickEnabled"`
	LanOnly         bool   `json:"lanOnly"`
	OfflineCluster  bool   `json:"offlineCluster"`
	Language        string `json:"language"`
	TickRate        int    `json:"tickRate"`
	WhitelistSlots  int    `json:"whitelistSlots"`
	ConsoleEnabled  bool   `json:"consoleEnabled"`
	// RunningShards 是当前还在跑的分片。cluster.ini 只在分片启动时读一次，
	// 所以在跑的分片必须重启才吃得到新值，界面要能把这件事说清楚。
	RunningShards []string `json:"runningShards"`
	Path          string   `json:"path"`
	Note          string   `json:"note,omitempty"`

	// 下面三个是"这次保存到底改了什么、什么时候生效"的结构化回执。
	// 之所以另开字段而不是只往 Note 里塞一句话：Note 是给人读的一整段，
	// 前端要拿它做判断（要不要弹"需重启"的提示、逐项高亮）就得再解析一遍中文，
	// 两边各写一套解析迟早对不上。omitempty 保证 show 接口和"无改动"分支的响应
	// 和以前一模一样——只有真正写过盘的保存才带上它们。
	Diff            *settingsDiffView `json:"diff,omitempty"`
	Effective       string            `json:"effective,omitempty"`
	RequiresRestart bool              `json:"requiresRestart,omitempty"`
}

// runningShards 只报本集群里会话还在的那些分片。
// screen 问不到时返回错误而不是"全都未运行"：状态说错比不说更糟，
// 用户会以为改完就生效了。
func (s *Server) runningShards(ctx context.Context, cluster string) ([]string, error) {
	sessions, err := s.Procs.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, 2)
	for _, shard := range s.Paths.ShardNames(cluster) {
		if sessions[s.Procs.SessionName(cluster, shard)] > 0 {
			out = append(out, shard)
		}
	}
	return out, nil
}

func (s *Server) buildClusterSettingsView(ctx context.Context, cluster string) (clusterSettingsView, error) {
	c, err := dst.OpenCluster(s.Paths, cluster)
	if err != nil {
		return clusterSettingsView{}, err
	}
	set := c.Settings()
	v := clusterSettingsView{
		Cluster:     cluster,
		DisplayName: set.ClusterName, Description: set.Description,
		RunningShards: []string{},
		HasPassword:   set.Password != "",
		GameMode:      set.GameMode, Intention: set.Intention, MaxPlayers: set.MaxPlayers,
		Pvp: set.PVP, PauseWhenEmpty: set.PauseWhenEmpty,
		VoteEnabled: set.VoteEnabled, VoteKickEnabled: set.VoteKickEnabled,
		LanOnly: set.LanOnly, OfflineCluster: set.OfflineCluster,
		Language: set.Language, TickRate: set.TickRate,
		WhitelistSlots: set.WhitelistSlots, ConsoleEnabled: set.ConsoleEnabled,
		Path: s.Paths.ClusterIni(cluster),
	}
	running, err := s.runningShards(ctx, cluster)
	if err != nil {
		v.Note = "问不到世界进程状态（需要和服务端同机、且 PATH 里有 screen）；下面这些值直接读自 cluster.ini"
		return v, nil
	}
	v.RunningShards = running
	return v, nil
}

func (s *Server) handleClusterSettingsShow(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	v, err := s.buildClusterSettingsView(r.Context(), cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, v)
}

// clusterSettingsPatch 是"只改给了的字段"的补丁。
// 指针为 nil 就是没提到这一项，保留文件里的现值——界面上本来就只有改过的那几栏
// 会带值回来，这里再要求整份表单提交只会让漏传变成清空。
type clusterSettingsPatch struct {
	DisplayName     *string `json:"displayName"`
	Description     *string `json:"description"`
	Password        *string `json:"password"`
	ClearPassword   *bool   `json:"clearPassword"`
	GameMode        *string `json:"gameMode"`
	Intention       *string `json:"intention"`
	MaxPlayers      *int    `json:"maxPlayers"`
	Pvp             *bool   `json:"pvp"`
	PauseWhenEmpty  *bool   `json:"pauseWhenEmpty"`
	VoteEnabled     *bool   `json:"voteEnabled"`
	VoteKickEnabled *bool   `json:"voteKickEnabled"`
	LanOnly         *bool   `json:"lanOnly"`
	OfflineCluster  *bool   `json:"offlineCluster"`
	Language        *string `json:"language"`
	TickRate        *int    `json:"tickRate"`
	WhitelistSlots  *int    `json:"whitelistSlots"`
	ConsoleEnabled  *bool   `json:"consoleEnabled"`
}

func (s *Server) handleClusterSettingsPut(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	var req clusterSettingsPatch
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	// 同一次请求里既给新密码又要清空，两句话互相打架，只能拒。
	if req.ClearPassword != nil && *req.ClearPassword && req.Password != nil && strings.TrimSpace(*req.Password) != "" {
		writeErr(w, http.StatusBadRequest,
			"同一份请求里既有新密码又要求清空密码；要改就填新密码，要清空就别带密码字段")
		return
	}
	c, err := dst.OpenCluster(s.Paths, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	old := c.Settings()
	// 构造 next / changed / passwordSet / passwordCleared 这一段和只读预览接口
	// 共用同一份（applySettingsPatch）：预览给用户看的旧值/新值，必须和真正写下去的
	// 那份完全一样，否则"给你看的"和"落盘的"会变成两套。
	next, changed, passwordSet, passwordCleared := applySettingsPatch(old, req)
	if err := validateGameSettingsFields(next, changed); err != nil {
		writeErr(w, http.StatusBadRequest, publicError(err))
		return
	}

	if next == old {
		// 一个字都没变就别动磁盘：每次点保存都写一遍会刷掉 .bak，
		// 真正需要回滚时只剩一份"和当前一样"的备份。
		v, err := s.buildClusterSettingsView(r.Context(), cluster)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		v.Note = "没有改动，cluster.ini 未重写"
		writeOK(w, v)
		return
	}

	c.SetSettings(next)
	if err := c.Save(); err != nil {
		s.audit(r, "cluster.settings", cluster, "写入失败", err)
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "cluster.settings", cluster,
		describeSettingsChange(old, next, passwordSet, passwordCleared), nil)

	v, err := s.buildClusterSettingsView(r.Context(), cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 结构化回执：逐项 diff + 整体生效口径。这里的 runningShards 结果直接复用
	// buildClusterSettingsView 已经问过的那一次（v.Note 非空表示当时问不到进程状态），
	// 不再另起一次 screen -ls，也不会出现 Note 说"要重启"、diff 却说"立即生效"。
	statusKnown := v.Note == ""
	eff, restart := settingsEffective(v.RunningShards, statusKnown)
	v.Effective, v.RequiresRestart = eff, restart
	diff := computeSettingsDiff(cluster, v.Path, old, next, passwordSet, passwordCleared, eff, statusKnown)
	v.Diff = &diff
	switch {
	case len(v.RunningShards) > 0:
		v.Note = fmt.Sprintf("%s 正在运行：cluster.ini 只在世界启动时读一次，改动要重启这些世界才生效"+
			"（重启会先落盘，正在玩的玩家会掉线重连）；已经连上的玩家不会因为改设置被踢",
			strings.Join(v.RunningShards, "、"))
	case v.Note != "":
		v.Note = "已写入 cluster.ini，但问不到世界进程状态；如果有世界正在跑，要重启它才读得到新值"
	default:
		v.Note = "已写入 cluster.ini，世界下次启动时读取新值"
	}
	writeOK(w, v)
}

// describeSettingsChange 给审计写"改了哪几项"，只写项名不写值：
// 名称和说明可能是玩家隐私，密码更不用说；值本身在 cluster.ini 里随时能看。
func describeSettingsChange(old, next dst.GameSettings, passwordSet, passwordCleared bool) string {
	var parts []string
	add := func(label string, changed bool) {
		if changed {
			parts = append(parts, label)
		}
	}
	add("服务器名称", old.ClusterName != next.ClusterName)
	add("服务器说明", old.Description != next.Description)
	add("游戏模式", old.GameMode != next.GameMode)
	add("游戏偏好", old.Intention != next.Intention)
	add("最大玩家数", old.MaxPlayers != next.MaxPlayers)
	add("PVP", old.PVP != next.PVP)
	add("空服暂停", old.PauseWhenEmpty != next.PauseWhenEmpty)
	add("投票开启", old.VoteEnabled != next.VoteEnabled)
	add("投票踢人", old.VoteKickEnabled != next.VoteKickEnabled)
	add("仅局域网", old.LanOnly != next.LanOnly)
	add("离线模式", old.OfflineCluster != next.OfflineCluster)
	add("服务器语言", old.Language != next.Language)
	add("通信频率", old.TickRate != next.TickRate)
	add("白名单保留位", old.WhitelistSlots != next.WhitelistSlots)
	add("控制台", old.ConsoleEnabled != next.ConsoleEnabled)
	if passwordSet {
		parts = append(parts, "重设连接密码")
	}
	if passwordCleared {
		parts = append(parts, "清空连接密码")
	}
	if len(parts) == 0 {
		return "无改动"
	}
	return "改了：" + strings.Join(parts, "、")
}

// ---- 配置 diff：只读预览与保存回执共用同一份计算 ----

// 生效口径的三个取值。cluster.ini 只在分片启动时读一次：
//   - 有分片在跑（或问不到进程状态，按保守的那一侧说）→ 需重启；
//   - 没有分片在跑 → 改动写进文件就已经定稿，下次启动自然读取，界面按"立即生效"说；
//   - "下次生成世界生效"是 worldgenoverride.lua 那一类接口的口径（见 bootstrap.go），
//     cluster.ini 的 diff 不会取它。放在同一组常量里，前端只需要认这一套字符串。
const (
	effectiveRestart      = "需重启"
	effectiveImmediate    = "立即生效"
	effectiveNextWorldgen = "下次生成世界生效"
)

// settingsDiffItem 是一项配置的旧值/新值对比。
// 密码一项永远脱敏：接口里只说"已设置/未设置"，原值不出进程（同 describeSettingsChange）。
type settingsDiffItem struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Old       string `json:"old"`
	New       string `json:"new"`
	Changed   bool   `json:"changed"`
	Effective string `json:"effective"`
}

// settingsDiffView 是只读预览接口与保存响应共用的 diff 摘要。
type settingsDiffView struct {
	Cluster string             `json:"cluster"`
	Path    string             `json:"path"`
	Items   []settingsDiffItem `json:"items"`
	// ChangedCount 是这次真正有变化的项数；预览时若一个都没改，界面不必再逐项比。
	ChangedCount int `json:"changedCount"`
	// Effective 是这一整份改动的生效口径，RequiresRestart 是它的布尔化，
	// 前端不用自己比对中文字符串就能决定要不要弹"需重启"的提示。
	Effective       string `json:"effective"`
	RequiresRestart bool   `json:"requiresRestart"`
	Note            string `json:"note,omitempty"`
}

// applySettingsPatch 把"只改给了的字段"的补丁套在旧值上。
// 保存（PUT）和只读预览（GET diff）都调它，两处算出的 next / changed 必须完全一致，
// 否则界面上"给你看的"和"真正写下去的"会是两套。
func applySettingsPatch(old dst.GameSettings, req clusterSettingsPatch) (dst.GameSettings, map[string]bool, bool, bool) {
	next := old
	if req.DisplayName != nil {
		next.ClusterName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Description != nil {
		next.Description = *req.Description
	}
	if req.GameMode != nil {
		next.GameMode = *req.GameMode
	}
	if req.Intention != nil {
		next.Intention = *req.Intention
	}
	if req.MaxPlayers != nil {
		next.MaxPlayers = *req.MaxPlayers
	}
	if req.Pvp != nil {
		next.PVP = *req.Pvp
	}
	if req.PauseWhenEmpty != nil {
		next.PauseWhenEmpty = *req.PauseWhenEmpty
	}
	if req.VoteEnabled != nil {
		next.VoteEnabled = *req.VoteEnabled
	}
	if req.VoteKickEnabled != nil {
		next.VoteKickEnabled = *req.VoteKickEnabled
	}
	if req.LanOnly != nil {
		next.LanOnly = *req.LanOnly
	}
	if req.OfflineCluster != nil {
		next.OfflineCluster = *req.OfflineCluster
	}
	if req.Language != nil {
		next.Language = *req.Language
	}
	if req.TickRate != nil {
		next.TickRate = *req.TickRate
	}
	if req.WhitelistSlots != nil {
		next.WhitelistSlots = *req.WhitelistSlots
	}
	if req.ConsoleEnabled != nil {
		next.ConsoleEnabled = *req.ConsoleEnabled
	}

	// 空密码等于"没改"：界面回填不了原值，用户不填就是不想动它。
	// 清空是单独的动作（clearPassword），免得"没粘上"把整个房间的密码摘掉。
	passwordSet, passwordCleared := false, false
	switch {
	case req.ClearPassword != nil && *req.ClearPassword:
		passwordCleared = old.Password != ""
		next.Password = ""
	case req.Password != nil && strings.TrimSpace(*req.Password) != "":
		pw := strings.TrimSpace(*req.Password)
		passwordSet = pw != old.Password
		next.Password = pw
	}

	// 只校验这次点过名的项：文件里本来就认不出的取值（手工改过）不该挡住改名。
	changed := map[string]bool{}
	for key, touched := range map[string]bool{
		"ClusterName": req.DisplayName != nil, "Description": req.Description != nil,
		"GameMode": req.GameMode != nil, "Intention": req.Intention != nil,
		"MaxPlayers": req.MaxPlayers != nil, "Language": req.Language != nil,
		"TickRate": req.TickRate != nil, "WhitelistSlots": req.WhitelistSlots != nil,
		"Password": passwordSet || passwordCleared,
	} {
		if touched {
			changed[key] = true
		}
	}
	return next, changed, passwordSet, passwordCleared
}

// settingsEffective 定这一整份改动的生效口径。问不到进程状态时按"需重启"保守说：
// 那句"立即生效"会被读成"改完就吃到了新值"，状态不明时不能替面板撒这个谎。
func settingsEffective(running []string, statusKnown bool) (string, bool) {
	if len(running) > 0 || !statusKnown {
		return effectiveRestart, true
	}
	return effectiveImmediate, false
}

// computeSettingsDiff 逐项算出 old/new/changed，并给每一项标上生效口径。
// 纯计算，不碰磁盘也不问进程：运行分片与"状态是否已知"由调用方从
// buildClusterSettingsView 的结果里拿来，避免另起一次 screen -ls。
func computeSettingsDiff(cluster, path string, old, next dst.GameSettings,
	passwordSet, passwordCleared bool, effective string, statusKnown bool) settingsDiffView {

	specs := []struct {
		key, label string
		value      func(dst.GameSettings) string
	}{
		{"cluster_name", "服务器名称", func(s dst.GameSettings) string { return s.ClusterName }},
		{"cluster_description", "服务器说明", func(s dst.GameSettings) string { return s.Description }},
		{"game_mode", "游戏模式", func(s dst.GameSettings) string { return s.GameMode }},
		{"cluster_intention", "游戏偏好", func(s dst.GameSettings) string { return s.Intention }},
		{"max_players", "最大玩家数", func(s dst.GameSettings) string { return strconv.Itoa(s.MaxPlayers) }},
		{"pvp", "PVP", func(s dst.GameSettings) string { return strconv.FormatBool(s.PVP) }},
		{"pause_when_empty", "空服暂停", func(s dst.GameSettings) string { return strconv.FormatBool(s.PauseWhenEmpty) }},
		{"vote_enabled", "投票开启", func(s dst.GameSettings) string { return strconv.FormatBool(s.VoteEnabled) }},
		{"vote_kick_enabled", "投票踢人", func(s dst.GameSettings) string { return strconv.FormatBool(s.VoteKickEnabled) }},
		{"lan_only_cluster", "仅局域网", func(s dst.GameSettings) string { return strconv.FormatBool(s.LanOnly) }},
		{"offline_cluster", "离线模式", func(s dst.GameSettings) string { return strconv.FormatBool(s.OfflineCluster) }},
		{"cluster_language", "服务器语言", func(s dst.GameSettings) string { return s.Language }},
		{"tick_rate", "通信频率", func(s dst.GameSettings) string { return strconv.Itoa(s.TickRate) }},
		{"whitelist_slots", "白名单保留位", func(s dst.GameSettings) string { return strconv.Itoa(s.WhitelistSlots) }},
		{"console_enabled", "控制台", func(s dst.GameSettings) string { return strconv.FormatBool(s.ConsoleEnabled) }},
	}

	items := make([]settingsDiffItem, 0, len(specs)+1)
	changedCount := 0
	for _, sp := range specs {
		o, n := sp.value(old), sp.value(next)
		ch := o != n
		if ch {
			changedCount++
		}
		items = append(items, settingsDiffItem{
			Key: sp.key, Label: sp.label, Old: o, New: n,
			Changed: ch, Effective: effective,
		})
	}
	// 密码单列：原值一律不出接口，只报"已设置/未设置"。
	oldPW, newPW := "未设置", "未设置"
	if old.Password != "" {
		oldPW = "已设置"
	}
	if next.Password != "" {
		newPW = "已设置"
	}
	pwChanged := passwordSet || passwordCleared
	if pwChanged {
		changedCount++
	}
	items = append(items, settingsDiffItem{
		Key: "cluster_password", Label: "连接密码", Old: oldPW, New: newPW,
		Changed: pwChanged, Effective: effective,
	})

	view := settingsDiffView{
		Cluster: cluster, Path: path, Items: items, ChangedCount: changedCount,
		Effective: effective, RequiresRestart: effective == effectiveRestart,
	}
	switch {
	case !statusKnown:
		view.Note = "问不到世界进程状态，按「需重启」保守处理：如果有世界正在跑，cluster.ini 要重启它才读得到新值"
	case effective == effectiveRestart:
		view.Note = "cluster.ini 只在世界启动时读一次，运行中的分片要重启才吃得到新值（重启会先落盘，正在玩的玩家会掉线重连）"
	default:
		view.Note = "没有在跑的分片，改动写进文件就已定稿，下次启动世界时读取新值"
	}
	return view
}

// settingsPatchFromRequest 解析只读预览接口的补丁。
// 优先认 JSON 请求体（前端可以把保存请求体原样发来做预览），否则从 query 参数读。
// 密码不鼓励走 query（会进访问日志和浏览器历史）：预览接口另给 setPassword=1 这个
// 不带明文的标记——diff 里密码那一项本来就只显示"已设置/未设置"，不需要真实值。
func (s *Server) settingsPatchFromRequest(w http.ResponseWriter, r *http.Request) (clusterSettingsPatch, bool) {
	var req clusterSettingsPatch
	if r.ContentLength > 0 && strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
			return req, false
		}
		return req, true
	}
	raw := r.URL.Query()
	text := func(k string) *string {
		if !raw.Has(k) {
			return nil
		}
		v := raw.Get(k)
		return &v
	}
	req.DisplayName = text("displayName")
	req.Description = text("description")
	req.GameMode = text("gameMode")
	req.Intention = text("intention")
	req.Language = text("language")

	num := func(k string) (*int, error) {
		if !raw.Has(k) {
			return nil, nil
		}
		n, err := strconv.Atoi(raw.Get(k))
		if err != nil {
			return nil, fmt.Errorf("%s 得是整数，收到 %q", k, raw.Get(k))
		}
		return &n, nil
	}
	flag := func(k string) (*bool, error) {
		if !raw.Has(k) {
			return nil, nil
		}
		b, err := strconv.ParseBool(raw.Get(k))
		if err != nil {
			return nil, fmt.Errorf("%s 得是 true/false，收到 %q", k, raw.Get(k))
		}
		return &b, nil
	}
	for _, f := range []struct {
		key string
		dst **int
	}{
		{"maxPlayers", &req.MaxPlayers}, {"tickRate", &req.TickRate},
		{"whitelistSlots", &req.WhitelistSlots},
	} {
		n, err := num(f.key)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return req, false
		}
		if n != nil {
			*f.dst = n
		}
	}
	for _, f := range []struct {
		key string
		dst **bool
	}{
		{"pvp", &req.Pvp}, {"pauseWhenEmpty", &req.PauseWhenEmpty},
		{"voteEnabled", &req.VoteEnabled}, {"voteKickEnabled", &req.VoteKickEnabled},
		{"lanOnly", &req.LanOnly}, {"offlineCluster", &req.OfflineCluster},
		{"consoleEnabled", &req.ConsoleEnabled},
	} {
		b, err := flag(f.key)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return req, false
		}
		if b != nil {
			*f.dst = b
		}
	}
	if truthy(raw.Get("clearPassword")) {
		b := true
		req.ClearPassword = &b
	}
	if raw.Has("password") {
		req.Password = text("password")
	} else if truthy(raw.Get("setPassword")) {
		ph := "（预览占位，不显示明文）"
		req.Password = &ph
	}
	return req, true
}

// handleClusterSettingsDiff 是只读预览：把"如果这样保存会改什么"算给用户看，
// 一个字节都不写盘。它和 PUT 共用 applySettingsPatch 与 computeSettingsDiff，
// 所以预览里的 old/new 和保存成功后回执里的 diff 是同一份。
func (s *Server) handleClusterSettingsDiff(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	req, ok := s.settingsPatchFromRequest(w, r)
	if !ok {
		return
	}
	if req.ClearPassword != nil && *req.ClearPassword && req.Password != nil && strings.TrimSpace(*req.Password) != "" {
		writeErr(w, http.StatusBadRequest,
			"同一份请求里既有新密码又要求清空密码；要改就填新密码，要清空就别带密码字段")
		return
	}
	c, err := dst.OpenCluster(s.Paths, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	old := c.Settings()
	next, changed, passwordSet, passwordCleared := applySettingsPatch(old, req)
	// 预览同样过一遍校验：让用户当场知道"这样改会被拒"，而不是先给一份好看的 diff、
	// 再在保存时挨一个 400。校验是纯计算，不写盘，符合"只读"。
	if err := validateGameSettingsFields(next, changed); err != nil {
		writeErr(w, http.StatusBadRequest, publicError(err))
		return
	}
	v, err := s.buildClusterSettingsView(r.Context(), cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	statusKnown := v.Note == ""
	eff, _ := settingsEffective(v.RunningShards, statusKnown)
	writeOK(w, computeSettingsDiff(cluster, s.Paths.ClusterIni(cluster), old, next,
		passwordSet, passwordCleared, eff, statusKnown))
}

// ---- cluster.ini 多版本备份：列出与回滚 ----

// clusterBackupsView 是"这个文件有哪些历史版本"的响应。
type clusterBackupsView struct {
	Cluster string           `json:"cluster"`
	Path    string           `json:"path"`
	Keep    int              `json:"keep"`
	Backups []dst.BackupInfo `json:"backups"`
	Note    string           `json:"note,omitempty"`
}

// handleClusterSettingsBackups 列出 cluster.ini 的历史备份（新→旧）。
func (s *Server) handleClusterSettingsBackups(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	path := s.Paths.ClusterIni(cluster)
	list, err := dst.ListBackups(path)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, clusterBackupsView{
		Cluster: cluster, Path: path, Keep: dst.MaxBackupVersions, Backups: list,
		Note: "回滚会把选中的备份写回 cluster.ini；回滚前会先把当前文件再备份一次，所以回滚本身也可逆。" +
			"cluster.ini 只在世界启动时读一次，回滚后要重启在跑的世界才生效",
	})
}

type clusterRollbackRequest struct {
	Name string `json:"name"`
}

// handleClusterSettingsRollback 把某份备份写回 cluster.ini。
// 名字先对着 ListBackups 的结果核一遍（白名单），再交给 dst.RollbackBackup：
// 用户选错/名字不合法在这里给 400，真正的写盘失败才落到 500，不混成一句。
func (s *Server) handleClusterSettingsRollback(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	var req clusterRollbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "要回滚到哪一份？给备份列表里那一行的 name")
		return
	}
	path := s.Paths.ClusterIni(cluster)
	list, err := dst.ListBackups(path)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	found := false
	for _, b := range list {
		if b.Name == name {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("备份 %q 不在 %s 的备份列表里；先调列出接口拿准确的 name", name, cluster))
		return
	}
	before, err := dst.RollbackBackup(path, name)
	if err != nil {
		s.audit(r, "cluster.settings.rollback", cluster, "回滚到 "+name+" 失败", err)
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	v, err := s.buildClusterSettingsView(r.Context(), cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	statusKnown := v.Note == ""
	eff, restart := settingsEffective(v.RunningShards, statusKnown)
	v.Effective, v.RequiresRestart = eff, restart
	note := fmt.Sprintf("已把 cluster.ini 回滚到备份 %s；回滚前的当前版本另存为 %s", name, before)
	if before == "" {
		note = fmt.Sprintf("已把 cluster.ini 回滚到备份 %s（原文件当时不存在，没有可留的回滚前副本）", name)
	}
	if eff == effectiveRestart {
		note += "；cluster.ini 只在世界启动时读一次，回滚要重启在跑的世界才生效"
	}
	v.Note = note
	s.audit(r, "cluster.settings.rollback", cluster, "回滚到 "+name, nil)
	writeOK(w, v)
}
