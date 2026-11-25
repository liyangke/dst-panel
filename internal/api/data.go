// 面板自身数据的读接口：在线趋势、操作记录、运行参数。
//
// 这些数据全在 SQLite 里，和 DST 的文件层没关系，所以这里的错误都是"面板自己的事"，
// 不像 status/helper 那样要把"分片没进世界"当正常状态返回。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"dstpanel/internal/dst"
	"dstpanel/internal/gamelog"
	"dstpanel/internal/modbridge"
	"dstpanel/internal/store"
)

// statsPoint 是趋势图上的一个格子：这段时间里这个房间总共几人。
//
// 只有时间和人数两个数：day/season/players 这些是"某一次采样"的细节，
// 一个格子里装着几十次采样，硬挑一条出来报成"这段时间的季节"就是在编。
type statsPoint struct {
	At     string `json:"at"`
	Online int    `json:"online"`
}

type statsPage struct {
	Points        []statsPoint `json:"points"`
	Hours         int          `json:"hours"`
	BucketSeconds int          `json:"bucketSeconds"`
	BucketLabel   string       `json:"bucketLabel"`
	Samples       int          `json:"samples"`
	DataFrom      string       `json:"dataFrom,omitempty"`
	DataTo        string       `json:"dataTo,omitempty"`
	// LatestAgoMinutes 是"最近一条采样距今几分钟"。趋势图最容易骗人的地方就是
	// 右端其实停在几小时前，而图上什么都没说，所以这个数由后端算好给出去。
	// 一条采样都没有时它是 nil 而不是 0：报 0 等于说"最新一条就在刚才"，
	// 而屏幕上是一张空图，界面那枚新鲜度把手也会跟着变成假话。
	LatestAgoMinutes *int   `json:"latestAgoMinutes,omitempty"`
	Note             string `json:"note"`
}

// trendBucket 按时间窗挑一个格子大小，让点数落在几百这个量级：
// 太密了图上糊成一片、接口也白给几 MB JSON，太疏了短时窗里的细节全没了。
func trendBucket(hours int) time.Duration {
	steps := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute,
		15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour, 24 * time.Hour}
	span := time.Duration(hours) * time.Hour
	for _, st := range steps {
		if span/st <= 600 {
			return st
		}
	}
	return steps[len(steps)-1]
}

// bucketLabel 把格子大小翻成界面能直接用的中文（"每 5 分钟一个点"）。
func bucketLabel(d time.Duration) string {
	switch {
	case d < time.Hour:
		m := int(d / time.Minute)
		if m == 1 {
			return "每分钟一个点"
		}
		return fmt.Sprintf("每 %d 分钟一个点", m)
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		if h == 1 {
			return "每小时一个点"
		}
		return fmt.Sprintf("每 %d 小时一个点", h)
	default:
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "每天一个点"
		}
		return fmt.Sprintf("每 %d 天一个点", days)
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	cluster := r.URL.Query().Get("cluster")
	if cluster == "" {
		writeErr(w, http.StatusBadRequest, "必须指定 cluster")
		return
	}
	// 趋势是"这个房间过去几天的在线人数"，属于房间数据：
	// 房间列表对成员已经裁过，但这个接口认参数不认路径，得单独判一次。
	if !s.denyInvisibleRoom(w, r, cluster) {
		return
	}
	shard := r.URL.Query().Get("shard")
	hours := 24
	if raw := r.URL.Query().Get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		// 上限 24*90：超过 90 天的趋势早被保留期清掉了，留着大数只会让人以为数据在、只是没显示。
		if err != nil || n < 1 || n > 24*90 {
			writeErr(w, http.StatusBadRequest, "hours 得是 1~2160 之间的整数")
			return
		}
		hours = n
	}
	bucket := trendBucket(hours)
	tn, err := s.Store.SnapshotTrend(cluster, shard, time.Now().UTC().Add(-time.Duration(hours)*time.Hour), bucket)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	out := statsPage{
		Points:        make([]statsPoint, 0, len(tn.Points)),
		Hours:         hours,
		BucketSeconds: int(bucket / time.Second),
		BucketLabel:   bucketLabel(bucket),
		Samples:       tn.Samples,
	}
	for _, p := range tn.Points {
		out.Points = append(out.Points, statsPoint{At: p.At.Format(time.RFC3339), Online: p.Online})
	}
	scope := "这个房间"
	if shard != "" {
		scope = "这个世界的"
	}
	switch {
	case tn.Samples == 0:
		out.Note = fmt.Sprintf("%s在过去 %d 小时里一条在线人数采样都没有，所以图上没有点。", scope, hours)
	default:
		out.DataFrom = tn.DataFrom.Format(time.RFC3339)
		out.DataTo = tn.DataTo.Format(time.RFC3339)
		ago := int(time.Now().UTC().Sub(tn.DataTo).Minutes())
		if ago < 0 {
			ago = 0
		}
		out.LatestAgoMinutes = &ago
		// 这句里不写钟点：库里的时刻是 UTC，服务端一拼"最早 11-05 11:32"，
		// 浏览器那一头按本地钟画的图下面那行就差 8 小时，两处说的不是同一刻。
		// 绝对时刻照旧从 dataFrom/dataTo 走，由界面换算成本地钟；这里只留相对时长。
		out.Note = fmt.Sprintf("%s%s里有 %d 条原始采样，归成 %d 个点（%s，各世界人数相加）；"+
			"这条线最早的那个点距今 %s，最近一条距今 %d 分钟。"+
			"要是这几个小时里有一段时间没开服，那一段就没有点，线会自己断在那儿。",
			scope, hourSpanLabel(hours), tn.Samples, len(tn.Points), out.BucketLabel,
			humanAgo(time.Now().UTC().Sub(tn.DataFrom)), ago)
	}
	writeOK(w, out)
}

// hourSpanLabel 把"多少小时"说成中文：超过一天的人读"168 小时"没有概念。
func hourSpanLabel(hours int) string {
	if hours >= 24 && hours%24 == 0 {
		return fmt.Sprintf("过去 %d 小时（%d 天）", hours, hours/24)
	}
	return fmt.Sprintf("过去 %d 小时", hours)
}

type auditView struct {
	ID     int64  `json:"id"`
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	// ActionLabel 是这条动作的中文说法。界面只显示这个字段，不显示裸码值。
	ActionLabel string `json:"actionLabel"`
	Target      string `json:"target"`
	Detail      string `json:"detail,omitempty"`
	OK          bool   `json:"ok"`
	Err         string `json:"error,omitempty"`
}

// 面板写进审计表的动作码。这些字符串的调用点散在 lifecycle/mods/tasks/bootstrap 里，
// 这份表是它们唯一的解释来源；_test.go 里有一条测试会扫源码，
// 保证任何调用点写了这张表没有的码就编译不过测试，而不是等到界面上冒出裸码值。
var auditLabels = map[string]string{
	"cluster.create":            "创建集群（含世界目录与配置）",
	"cluster.settings":          "修改集群设置",
	"cluster.ports":             "修改游戏端口",
	"cluster.token":             "写入 Klei 令牌",
	"cluster.owner":             "指派房间归属",
	"world.update":              "修改世界规则",
	"shard.start":               "启动世界",
	"shard.stop":                "停止世界",
	"shard.kill":                "强制关闭世界（未落盘）",
	"shard.restart":             "重启世界",
	"shard.kill.warning":        "强停前的丢档风险提示",
	"cluster.settings.diff":     "预览配置改动",
	"cluster.settings.rollback": "回滚集群配置",
	"monitor.alert":             "监控阈值告警",
	"monitor.config":            "修改监控与自愈配置",
	"log.export":                "导出游戏日志",
	"helper.enable":             "启用助手 MOD",
	"helper.disable":            "关闭助手 MOD",
	"mod.setup.add":             "订阅模组（写进下载清单）",
	"mod.subscribe_open":        "订阅模组并在选中的世界打开它",
	"mod.setup.remove":          "取消订阅模组",
	"mod.files.remove":          "删除已下载的模组文件",
	"mod.upload":                "上传本地模组",
	"mod.override":              "修改世界模组配置",
	"task.submit":               "提交后台任务",
	"task.cancel":               "取消后台任务",
	"settings.update":           "修改运行参数",
	"backup.create":             "备份存档",
	"backup.import":             "导入外部存档（提交导入任务）",
	"backup.import.upload":      "上传外部存档包",
	"backup.import.preview":     "预览外部存档包的结构",
	"backup.restore":            "恢复存档（用备份换掉现有存档）",
	"backup.delete":             "删除存档备份",
	"backup.verify":             "校验存档备份",
	"backup.download":           "下载存档备份",
	"account.create":            "新建面板账号",
	"account.password":          "修改面板账号密码",
	"account.role":              "调整面板账号权限",
	"account.delete":            "删除面板账号",
	"player.list.add":           "加入玩家名单",
	"player.list.remove":        "从玩家名单移除",
}

// auditFamilies 是筛选下拉里的"动作族"。ListAudit 的 action 参数是按前缀匹配的，
// 所以一族能同时捞出这一族的所有具体动作。
var auditFamilies = []gamelog.EventInfo{
	{Code: "cluster.", Label: "集群与令牌"},
	{Code: "world.", Label: "世界规则"},
	{Code: "shard.", Label: "世界启停"},
	{Code: "helper.", Label: "助手 MOD"},
	{Code: "admin.", Label: "控制台命令"},
	{Code: "bridge.", Label: "助手命令"},
	{Code: "player.list.", Label: "玩家名单"},
	{Code: "mod.", Label: "模组"},
	{Code: "backup.", Label: "存档备份"},
	{Code: "task.", Label: "后台任务"},
	{Code: "settings.", Label: "运行参数"},
	{Code: "account.", Label: "面板账号"},
}

// auditActionLabel 把一个动作码翻成人话。
//
// admin.<命令> 和 bridge.<命令> 这两族的具体名字来自各自的白名单，
// 所以这里去那两个目录现取，而不是在这里再摆一张表——两张表迟早一套改了另一套没改。
func auditActionLabel(action string) string {
	if l, ok := auditLabels[action]; ok {
		return l
	}
	if name, ok := strings.CutPrefix(action, "admin."); ok {
		if spec, exists := dst.ConsoleCommandSpec(name); exists {
			return "控制台命令 · " + spec.Label
		}
	}
	if name, ok := strings.CutPrefix(action, "bridge."); ok {
		if contains(modbridge.CommandNames(), name) {
			return "助手命令 · " + modbridge.CommandLabel(name)
		}
	}
	// 认不出来的码也必须有中文：库里可能留着旧版本面板写的行，
	// 而界面一旦拿到空标签就会退回去显示码值。
	for _, fam := range auditFamilies {
		if !strings.HasPrefix(action, fam.Code) {
			continue
		}
		if action == fam.Code {
			return fam.Label // 传进来的就是族前缀本身，不是某条具体动作
		}
		return fam.Label + "（这套面板没登记具体动作的说明）"
	}
	return "面板操作（没登记这条动作的说明）"
}

// auditActionCatalog 是筛选可用的全部取值：族前缀 + 具体动作码。
// 两族动态动作从各自白名单展开，所以选了 admin.kick 就是精确筛那一条。
func auditActionCatalog() []gamelog.EventInfo {
	out := make([]gamelog.EventInfo, 0, len(auditFamilies)+len(auditLabels)+20)
	for _, fam := range auditFamilies {
		out = append(out, gamelog.EventInfo{Code: fam.Code, Label: fam.Label + "（全部）"})
	}
	for _, code := range sortedKeys(auditLabels) {
		out = append(out, gamelog.EventInfo{Code: code, Label: auditLabels[code]})
	}
	for _, key := range dst.ConsoleCommandKeys() {
		spec, ok := dst.ConsoleCommandSpec(key)
		if ok {
			out = append(out, gamelog.EventInfo{Code: "admin." + key, Label: "控制台命令 · " + spec.Label})
		}
	}
	for _, name := range modbridge.CommandNames() {
		out = append(out, gamelog.EventInfo{Code: "bridge." + name, Label: "助手命令 · " + modbridge.CommandLabel(name)})
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// handleAuditActions 给操作记录页的筛选下拉。
// 数据从面板自己的调用点推导，不在前端摆第二份表。
func (s *Server) handleAuditActions(w http.ResponseWriter, r *http.Request) {
	actors, err := s.Store.ListAuditActors()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if actors == nil {
		actors = []string{}
	}
	writeOK(w, map[string]any{
		"families": auditFamilies,
		"actions":  auditActionCatalog(),
		"actors":   actors,
		"note":     "按前缀匹配：选一个族会把这一族的所有动作都捞出来；选具体动作只筛那一条。actors 是库里出现过的操作人。",
	})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, offset := 50, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			writeErr(w, http.StatusBadRequest, "limit 得是 1~500 之间的整数")
			return
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "offset 得是不小于 0 的整数")
			return
		}
		offset = n
	}
	action := r.URL.Query().Get("action")
	// 筛选项写错时结果是空列表，而空列表在界面上和"确实没发生过这类操作"长得一样，
	// 所以取值必须当场拒绝，让人去调 /api/v1/audit-actions。
	if action != "" && !contains(auditActionCodes(), action) {
		writeErr(w, http.StatusBadRequest, "没有这个动作取值，可按族前缀（如 shard.）或具体动作筛，可选值调 /api/v1/audit-actions 获取")
		return
	}
	since, until, bad := parseAuditRange(r)
	if bad != "" {
		writeErr(w, http.StatusBadRequest, bad)
		return
	}
	// 操作人同样当场校验：下拉里的值是从这张表现查的，认不出来就是拼错的参数，
	// 而拼错的参数返回空表，和"这个人没操作过"在界面上长得一样。
	actor := strings.TrimSpace(r.URL.Query().Get("actor"))
	// 成员看操作记录只看到自己的操作，这是这一版划的线。
	//
	// 为什么不在这里把"别人对我房间的记录"筛出来：这一页是 limit/offset 翻页的，
	// 拿到一页之后再按房间名挑，页面上的"第 2 页 50 条"就和真的发出去的条数不一致，
	// 而且第 1 页被挑剩 3 条时用户只会以为"没发生过操作"。
	// 要在库里按可见房间集合筛，得让 audit_log 存一个明确的房间列（现在 target
	// 混着写房间名、房间/分片、快照编号、用户名），那是下一版的活。
	sess := sessionOf(r)
	if s.Store != nil && sess.Role == store.RoleMember {
		if actor != "" && !strings.EqualFold(actor, sess.Username) {
			writeErr(w, http.StatusForbidden,
				"成员看操作记录只能看自己的操作；要看别人对这间房做了什么，请管理员看完整记录")
			return
		}
		actor = sess.Username
	}
	if actor != "" {
		actors, err := s.Store.ListAuditActors()
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		if !contains(actors, actor) {
			writeErr(w, http.StatusBadRequest, "没有这个操作人，可选值调 /api/v1/audit-actions 的 actors 获取")
			return
		}
	}
	entries, err := s.Store.ListAuditFiltered(limit, offset, store.AuditFilter{
		ActionPrefix: action, Since: since, Until: until,
		Actor: actor, Target: r.URL.Query().Get("target"),
	})
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	out := make([]auditView, 0, len(entries))
	for _, e := range entries {
		out = append(out, auditView{
			ID: e.ID, At: e.At.UTC().Format(time.RFC3339), Actor: e.Actor,
			Action: e.Action, ActionLabel: auditActionLabel(e.Action),
			Target: e.Target, Detail: e.Detail, OK: e.OK, Err: e.Err,
		})
	}
	writeOK(w, out)
}

// parseAuditRange 解操作记录的时间区间。
//
// 界面传的是 UTC 的 RFC3339 时刻（"用户选的本地那一天"由浏览器换算掉了），所以这里
// 只判格式和"两头别写反"，不猜任何一方的日历。写错要当场拒绝：区间错了返回的是空表，
// 而空表在界面上和"这段时间确实没发生过操作"长得一模一样。
func parseAuditRange(r *http.Request) (since, until time.Time, bad string) {
	const eg = "2026-11-02T00:00:00Z"
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, "since 得是 " + eg + " 这样带时区的时刻，不是 " + raw
		}
		since = t
	}
	if raw := r.URL.Query().Get("until"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, "until 得是 " + eg + " 这样带时区的时刻，不是 " + raw
		}
		until = t
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		return time.Time{}, time.Time{}, "结束时间要在开始时间之后：两头写成同一个时刻，中间是空的"
	}
	return since, until, ""
}

// auditActionCodes 从目录里取码值列，校验和下拉共用同一份，不会两处各一套。
func auditActionCodes() []string {
	list := auditActionCatalog()
	out := make([]string, 0, len(list))
	for _, it := range list {
		out = append(out, it.Code)
	}
	return out
}

// settingView 是给界面的一条可改参数。
// 值一律回字符串：这两项本身就是整数天数，用字符串来回不会丢信息，
// 又让"没设过"和"设成 0"这两种情况能被区分开（0 表示永久保留）。
type settingView struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Default int    `json:"default"`
	Set     bool   `json:"set"`
	Unit    string `json:"unit"`
	Special string `json:"specialNote,omitempty"`
	// FromConfig 非空表示这项的默认值来自配置文件（界面要说清"改了会以这里为准"）：
	// 同一个数字在 yaml 和面板设置里各有一份时，不说清来源就是让用户去猜。
	FromConfig string `json:"fromConfig,omitempty"`
}

var settingMeta = []struct {
	Key, Label, Unit, ZeroNote, FromConfig string
}{
	{store.KeyAuditRetentionDays, "操作记录保留天数", "天", "0 或负数表示不清理、永久保留", ""},
	{store.KeySnapshotRetentionDays, "在线趋势保留天数", "天", "0 或负数表示不清理、永久保留", ""},
	{store.KeyPlayerEventRetentionDays, "玩家行为保留天数", "天",
		"0 或负数表示不清理、永久保留。总览那张「玩家在线时间图」的历史就是这些行，清了图就断在那儿", ""},
	{store.KeyBackupKeep, "存档备份最多保留几份", "份",
		"0 或负数表示不自动清理。留神磁盘：占空间的是归档文件，大小看存档页的「已用」和最近一份的实测数字", "backup.keep"},
	{store.KeyBackupScheduleHours, "定时备份间隔", "小时",
		"0 表示不开定时备份，只在存档页手工打；开面板后要先等满一个间隔才会打第一份", "backup.scheduleHours"},
}

// settingDefault 给出这一项在"库里没改过"时的当前值：配置文件里写过的优先。
// 把默认显示成登记表里的 20 而实际按 yaml 里的 5 跑，用户会以为设置没生效。
func (s *Server) settingDefault(key string, fallback int) int {
	if v, ok := s.SettingDefaults[key]; ok {
		return v
	}
	return fallback
}

// settingInt 取"现在真正生效的"整型设置项：库里改过就以库里的为准，没改过用配置文件那份。
// 界面显示和实际执行必须走同一条解析，否则存档页写的保留上限和清理时用的上限会差一个数。
func (s *Server) settingInt(key string) (int, error) {
	fallback, _ := store.DefaultInt(key)
	return s.Store.IntWithDefault(key, s.settingDefault(key, fallback))
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	current, err := s.Store.Settings()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	out := make([]settingView, 0, len(settingMeta))
	for _, meta := range settingMeta {
		def, _ := store.DefaultInt(meta.Key)
		def = s.settingDefault(meta.Key, def)
		v := settingView{Key: meta.Key, Label: meta.Label, Unit: meta.Unit, Default: def,
			FromConfig: meta.FromConfig,
			// 0 的含义是这项设置自己的属性，不是当前取值的结果：
			// 只在值 <=0 时下发，用户刚把 5 改成 0 的那一刻反而看不到解释。
			Special: meta.ZeroNote}
		if raw, ok := current[meta.Key]; ok {
			v.Set = true
			v.Value = raw
		} else {
			v.Value = strconv.Itoa(def)
		}
		out = append(out, v)
	}
	writeOK(w, out)
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	patch, err := decodeStringMap(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(patch) == 0 {
		writeErr(w, http.StatusBadRequest, "至少要给出一个设置项")
		return
	}
	// 先全部校验再落盘：否则第一项写成功、第二项才发现非法，界面拿到 400 却已经改了一半。
	for key, raw := range patch {
		if !isKnownSetting(key) {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("不认识的设置项 %q，可改：%s", key, strings.Join(settingKeys(), "、")))
			return
		}
		if _, err := strconv.Atoi(raw); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s 要填整数天数，收到的是 %q", key, raw))
			return
		}
	}
	for key, raw := range patch {
		if err := s.Store.SetSetting(key, raw); err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		s.audit(r, "settings.update", key, raw, nil)
	}
	// 改完立刻返回全量，界面不用自己拼两份数据。
	s.handleSettings(w, r)
}

func isKnownSetting(key string) bool {
	for _, k := range settingKeys() {
		if k == key {
			return true
		}
	}
	return false
}

func settingKeys() []string {
	out := make([]string, 0, len(settingMeta))
	for _, m := range settingMeta {
		out = append(out, m.Key)
	}
	return out
}

// decodeStringMap 收 {"key": "value" | 123} 这种设置补丁。
// 字符串和数字都接：界面上的输入框拿到的天然是字符串，而手工 curl 时常写数字，
// 只接一种会让另一半人以为是接口坏了。
func decodeStringMap(r *http.Request) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("请求体要是 {设置项: 值} 这种 JSON 对象: %w", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
			continue
		}
		out[k] = strings.TrimSpace(string(v))
	}
	return out, nil
}
