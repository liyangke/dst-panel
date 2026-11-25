package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/dst"
	"dstpanel/internal/gamelog"
	"dstpanel/internal/modbridge"
	"dstpanel/internal/store"
)

// 这份测试盯的是"码值不外泄"这条硬要求：操作记录页显示的必须是中文说法，
// 而说法来自 auditLabels / 两族白名单。调用点新增一个没登记的码值时，
// 界面会安静地显示 shard.suspend 这种裸码，没人会去查库——所以在这里编译期拦住。
func TestAuditCallSitesAreCatalogued(t *testing.T) {
	exactRe := regexp.MustCompile(`s\.audit\(r, "([^"]+)",`)
	familyRe := regexp.MustCompile(`s\.audit\(r, "([^"]+)"\+`)

	var exact, family []string
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("找源文件: %v", err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue // 本文件的测试数据里就有这些字面量，扫进来会自己骗自己
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		checked++
		for _, m := range exactRe.FindAllStringSubmatch(string(raw), -1) {
			exact = append(exact, m[1])
		}
		for _, m := range familyRe.FindAllStringSubmatch(string(raw), -1) {
			family = append(family, m[1])
		}
	}
	// 先确认扫到了东西：正则写错时这里会炸，而不是给出一个"全部已登记"的假绿。
	if len(exact) < 12 || checked < 5 {
		t.Fatalf("扫到 %d 处字面量动作、%d 个文件，和调用点数量对不上，正则八成写错了", len(exact), checked)
	}
	for _, code := range exact {
		if _, ok := auditLabels[code]; !ok {
			t.Errorf("动作码 %q 没登记中文说明，界面上会露出裸码值", code)
		}
	}
	for _, prefix := range family {
		found := false
		for _, fam := range auditFamilies {
			if fam.Code == prefix {
				found = true
			}
		}
		if !found {
			t.Errorf("前缀 %q 拼出来的动作没有所属族，筛选下拉里选不到它", prefix)
		}
	}
	// helper 的动作为"开/关"共用一个调用点，靠函数返回值区分，所以单独验这两个取值。
	for _, code := range []string{actionHelperSwitch(true), actionHelperSwitch(false)} {
		if _, ok := auditLabels[code]; !ok {
			t.Errorf("助手开关的动作码 %q 没登记中文说明", code)
		}
	}
}

// 目录里每一条都必须有中文说法，而且不能说成"没登记说明"。
func TestAuditActionLabelCoversCatalog(t *testing.T) {
	list := auditActionCatalog()
	if len(list) < len(auditLabels)+20 {
		t.Fatalf("目录只有 %d 条，静态 %d 条加两族白名单不该这么少", len(list), len(auditLabels))
	}
	seen := map[string]bool{}
	for _, it := range list {
		if it.Label == "" {
			t.Errorf("%q 没有中文标签", it.Code)
		}
		if it.Label == it.Code {
			t.Errorf("%q 的标签就是码值本身", it.Code)
		}
		if strings.Contains(auditActionLabel(it.Code), "没登记") {
			t.Errorf("目录里的 %q 反而说没登记说明：目录和取说明的路径不一致", it.Code)
		}
		seen[it.Code] = true
	}
	// 两族的具体动作必须由各自白名单展开，不能在面板里再摆一张翻译表。
	for _, key := range dst.ConsoleCommandKeys() {
		if !seen["admin."+key] {
			t.Errorf("控制台命令 %s 没进筛选目录", key)
		}
	}
	for _, name := range modbridge.CommandNames() {
		if !seen["bridge."+name] {
			t.Errorf("助手命令 %s 没进筛选目录", name)
		}
	}
	// 库里可能留着旧版面板写的行：认不出也要给一句中文，不能回空串。
	if got := auditActionLabel("shard.suspend"); got == "" || got == "shard.suspend" {
		t.Errorf("历史动作码的兜底 = %q", got)
	}
	if got := auditActionLabel("完全没见过的东西"); got == "" || got == "完全没见过的东西" {
		t.Errorf("无族可归的动作兜底 = %q", got)
	}
}

func seedAudit(t *testing.T, f *apiFixture, action, target string) {
	t.Helper()
	if err := f.srv.Store.AppendAudit(store.AuditEntry{
		Actor: "admin", Action: action, Target: target, Detail: "测试写入", OK: true,
	}); err != nil {
		t.Fatalf("写审计 %s: %v", action, err)
	}
}

func getAudit(t *testing.T, f *apiFixture, query string) ([]auditView, *httptest.ResponseRecorder) {
	t.Helper()
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/audit"+query, "admin", "secret")
	r := decodeReply(t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET audit%s = %d，body %s", query, rec.Code, rec.Body.String())
	}
	raw, err := json.Marshal(r.Data)
	if err != nil {
		t.Fatalf("data 重新编码: %v", err)
	}
	var out []auditView
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解 auditView: %v，原文 %s", err, raw)
	}
	return out, rec
}

func TestAuditListLabelsAndFilter(t *testing.T) {
	f := newFixture(t, "")
	for _, code := range []string{"shard.start", "shard.kill", "mod.setup.add", "admin.announce", "bridge.kick"} {
		seedAudit(t, f, code, "Cluster_1/Master")
	}
	rows, _ := getAudit(t, f, "?limit=100")
	if len(rows) != 5 {
		t.Fatalf("库里 5 条，接口给了 %d 条", len(rows))
	}
	// 倒序：最新的排在最前，界面不用自己排。
	if rows[0].Action != "bridge.kick" {
		t.Errorf("首条 = %q，期望最新写入的那条", rows[0].Action)
	}
	for _, row := range rows {
		if row.ActionLabel == "" || row.ActionLabel == row.Action {
			t.Errorf("%q 的中文标签 = %q", row.Action, row.ActionLabel)
		}
		if row.At == "" || !strings.HasSuffix(row.At, "Z") {
			t.Errorf("at = %q，必须给 UTC 的 RFC3339", row.At)
		}
	}
	// 动态两族走各自白名单取名字，不是笼统一句"命令"。
	var announce, kick string
	for _, row := range rows {
		switch row.Action {
		case "admin.announce":
			announce = row.ActionLabel
		case "bridge.kick":
			kick = row.ActionLabel
		}
	}
	if !strings.Contains(announce, "公告") {
		t.Errorf("admin.announce 的标签 = %q，没带命令自己的中文名", announce)
	}
	if !strings.Contains(kick, "踢") {
		t.Errorf("bridge.kick 的标签 = %q，没带命令自己的中文名", kick)
	}

	// 族前缀把整族捞出来。
	fam, _ := getAudit(t, f, "?action=shard.")
	if len(fam) != 2 {
		t.Errorf("action=shard. 给了 %d 条，期望 2 条", len(fam))
	}
	exact, _ := getAudit(t, f, "?action=shard.kill")
	if len(exact) != 1 || exact[0].Action != "shard.kill" {
		t.Errorf("action=shard.kill 给了 %+v", exact)
	}
	// 分页参数照旧生效。
	page, _ := getAudit(t, f, "?limit=2&offset=3")
	if len(page) != 2 {
		t.Errorf("limit=2&offset=3 给了 %d 条", len(page))
	}

	for _, bad := range []string{"?action=shard", "?action=nope.", "?action=shard.start.", "?action=admin.nosuch"} {
		rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/audit"+bad, "admin", "secret")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d，期望 400，body %s", bad, rec.Code, rec.Body.String())
			continue
		}
		if msg := decodeReply(t, rec).Err; !strings.Contains(msg, "audit-actions") {
			t.Errorf("%s 的 400 没指路到目录接口：%q", bad, msg)
		}
	}
	// 空 action 是"不筛"，不能被当成写错的取值拒掉。
	all, rec := getAudit(t, f, "")
	if rec.Code != http.StatusOK || len(all) != 5 {
		t.Errorf("不带 action 时 %d 条 / %d 条", rec.Code, len(all))
	}
}

// 时间区间是操作记录的第二条筛选轴。这里盯三件事：
// 区间和动作筛选是"并且"而不是"或者"；until 那一头不含（界面把"今天"算到明天零点，
// 如果后端含边界，跨零点的那一条会在两个相邻区间里各出现一次）；
// 写错格式必须当场 400，因为区间写错返回的空表和"这段时间确实没操作过"长得一样。
func TestAuditTimeRange(t *testing.T) {
	f := newFixture(t, "")
	now := time.Now().UTC().Truncate(time.Second)
	seed := []struct {
		action string
		at     time.Time
	}{
		{"shard.start", now.Add(-10 * time.Hour)},
		{"mod.setup.add", now.Add(-3 * time.Hour)},
		{"task.submit", now.Add(-30 * time.Minute)},
	}
	for _, s := range seed {
		if err := f.srv.Store.AppendAudit(store.AuditEntry{
			At: s.at, Actor: "admin", Action: s.action, Target: "Cluster_1/Master", OK: true,
		}); err != nil {
			t.Fatalf("写审计 %s: %v", s.action, err)
		}
	}
	fmtText := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	// 只看最近 5 小时：最早那条要落在外面。
	rows, _ := getAudit(t, f, "?since="+url.QueryEscape(fmtText(-5*time.Hour)))
	if len(rows) != 2 || rows[0].Action == "shard.start" {
		t.Errorf("since 给了 %d 条 %+v，期望只剩最近两条", len(rows), actionsOf(rows))
	}
	// until 不含：切在最早那条之前一条都不留。
	rows, _ = getAudit(t, f, "?until="+url.QueryEscape(fmtText(-10*time.Hour)))
	if len(rows) != 0 {
		t.Errorf("until 正好压在那条记录的时刻上，还给了 %d 条 %+v", len(rows), actionsOf(rows))
	}
	rows, _ = getAudit(t, f, "?until="+url.QueryEscape(fmtText(-9*time.Hour)))
	if len(rows) != 1 || rows[0].Action != "shard.start" {
		t.Errorf("until 之后 %d 条 %+v，期望只有最早那条", len(rows), actionsOf(rows))
	}
	// 两头一起给。
	rows, _ = getAudit(t, f, "?since="+url.QueryEscape(fmtText(-9*time.Hour))+"&until="+url.QueryEscape(fmtText(-1*time.Hour)))
	if len(rows) != 1 || rows[0].Action != "mod.setup.add" {
		t.Errorf("区间中间 %d 条 %+v", len(rows), actionsOf(rows))
	}
	// 和动作筛选同时生效：族前缀 + 区间，两个条件都得满足。
	rows, _ = getAudit(t, f, "?action=shard.&since="+url.QueryEscape(fmtText(-1*time.Hour)))
	if len(rows) != 0 {
		t.Errorf("shard. 族在最近一小时内不该有记录，给了 %+v", actionsOf(rows))
	}
	rows, _ = getAudit(t, f, "?action=shard.&since="+url.QueryEscape(fmtText(-11*time.Hour)))
	if len(rows) != 1 {
		t.Errorf("shard. 族 + 宽区间给了 %d 条", len(rows))
	}

	for _, bad := range []string{
		"?since=2026-11-02", "?since=昨天", "?until=2026-11-02T00:00:00",
		"?since=" + url.QueryEscape(fmtText(0)) + "&until=" + url.QueryEscape(fmtText(-time.Hour)),
		"?since=" + url.QueryEscape(fmtText(0)) + "&until=" + url.QueryEscape(fmtText(0)),
	} {
		rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/audit"+bad, "admin", "secret")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d，期望 400，body %s", bad, rec.Code, rec.Body.String())
			continue
		}
		if msg := decodeReply(t, rec).Err; msg == "" || strings.Contains(msg, "null") {
			t.Errorf("%s 的 400 没说清楚：%q", bad, msg)
		}
	}
	// 两头都不带 = 不筛时间，必须还是全量（老调用方不受影响）。
	rows, _ = getAudit(t, f, "")
	if len(rows) != 3 {
		t.Errorf("不带时间参数给了 %d 条，期望 3 条", len(rows))
	}
}

// 操作人和对象是这张表上另外两条独立的轴。这里最要紧的一条是"对象按子串找"
// 不许把用户输入当成通配符：% 和 _ 在 LIKE 里各自有特殊含义，而备份编号、
// 进度值里真会出现它们——不转义的话搜 "50%" 会把整张表都捞出来，
// 看着像筛中了，其实一条都没筛。
func TestAuditActorAndTarget(t *testing.T) {
	f := newFixture(t, "")
	seed := []struct {
		actor, action, target, detail string
	}{
		{"admin", "shard.start", "Cluster_1/Master", ""},
		{"admin", "backup.create", "Cluster_1/Caves", ""},
		{"ops_bot", "mod.upload", "workshop-727536464", ""},
		{"ops_bot", "settings.update", "50%_keep", ""},
		// 详情里带着房间名、对象为空的一条：按对象筛不许把它捞出来。
		{"admin", "task.submit", "", "Cluster_1 的备份任务"},
	}
	now := time.Now().UTC().Truncate(time.Second)
	for i, s := range seed {
		if err := f.srv.Store.AppendAudit(store.AuditEntry{
			At:    now.Add(time.Duration(i) * time.Minute),
			Actor: s.actor, Action: s.action, Target: s.target, Detail: s.detail, OK: true,
		}); err != nil {
			t.Fatalf("写审计 %s: %v", s.action, err)
		}
	}

	rows, _ := getAudit(t, f, "?actor=ops_bot")
	if len(rows) != 2 {
		t.Errorf("actor=ops_bot 给了 %d 条 %+v", len(rows), actionsOf(rows))
	}
	for _, r := range rows {
		if r.Actor != "ops_bot" {
			t.Errorf("按人筛却混进了别人：%+v", r)
		}
	}
	// 等值匹配：大小写不一样就是没这个人，界面会拿到 400 而不是"看着筛了其实没筛"。
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/audit?actor=OPS_BOT", "admin", "secret")
	if rec.Code != http.StatusBadRequest || !strings.Contains(decodeReply(t, rec).Err, "audit-actions") {
		t.Errorf("不存在的操作人 = %d %s，期望 400 并指路目录", rec.Code, rec.Body.String())
	}

	byTarget := func(q string) []auditView {
		rows, _ := getAudit(t, f, "?target="+url.QueryEscape(q))
		return rows
	}
	if got := byTarget("Cluster_1"); len(got) != 2 {
		t.Errorf("target=Cluster_1 给了 %d 条 %+v，期望两个世界各一条", len(got), actionsOf(got))
	}
	// 英文大小写不敏感是 SQLite LIKE 的既有行为，界面那句话照它写。
	if got := byTarget("cluster_1"); len(got) != 2 {
		t.Errorf("target 小写给了 %d 条，和上面那句说明不一致", len(got))
	}
	if got := byTarget("50%"); len(got) != 1 || got[0].Action != "settings.update" {
		t.Errorf("target=50%% 给了 %+v：%% 被当成通配符了，整张表都会被捞出来", actionsOf(got))
	}
	if got := byTarget("_"); len(got) != 3 {
		t.Errorf("target=_ 给了 %d 条 %+v，期望含下划线的那三条（_ 没转义就会全都算上）", len(got), actionsOf(got))
	}
	// 只认对象这一列，不许顺带搜详情。
	for _, r := range byTarget("Cluster_1") {
		if r.Action == "task.submit" {
			t.Errorf("按对象筛把详情里带房间名的那条捞进来了：%+v", r)
		}
	}

	// 四条轴同时生效：取交集，不是并集。
	if got := byTarget("workshop"); len(got) != 1 || got[0].Actor != "ops_bot" {
		t.Errorf("target=workshop 给了 %+v", got)
	}
	rows, _ = getAudit(t, f, "?action=shard.&actor=admin&target=Caves")
	if len(rows) != 0 {
		t.Errorf("shard. + admin + Caves 三条同时筛给了 %+v，期望空", actionsOf(rows))
	}
	rows, _ = getAudit(t, f, "?action=backup.&actor=admin&target=Caves")
	if len(rows) != 1 || rows[0].Action != "backup.create" {
		t.Errorf("三条都对得上的那一条没筛出来：%+v", actionsOf(rows))
	}
	// 空参数 = 不筛，老调用方不受影响。
	rows, _ = getAudit(t, f, "?actor=&target=")
	if len(rows) != len(seed) {
		t.Errorf("空参数给了 %d 条，期望 %d 条", len(rows), len(seed))
	}
}

func actionsOf(rows []auditView) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Action)
	}
	return out
}

// 筛选目录的每个取值都必须能被列表接口接受——否则下拉里就有一个"选了永远空"的项。
// 操作人那一栏更直接：它整个来自这张表的 DISTINCT actor，列出来的人必须筛得出东西。
func TestAuditActionsRoundTrip(t *testing.T) {
	f := newFixture(t, "")
	for _, a := range []string{"admin", "ops_bot"} {
		if err := f.srv.Store.AppendAudit(store.AuditEntry{
			Actor: a, Action: "shard.start", Target: "Cluster_1/Master", OK: true,
		}); err != nil {
			t.Fatalf("写审计 %s: %v", a, err)
		}
	}
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/audit-actions", "admin", "secret")
	raw, err := json.Marshal(decodeReply(t, rec).Data)
	if err != nil {
		t.Fatalf("data 重新编码: %v", err)
	}
	var body struct {
		Families []gamelog.EventInfo `json:"families"`
		Actions  []gamelog.EventInfo `json:"actions"`
		Actors   []string            `json:"actors"`
		Note     string              `json:"note"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("audit-actions 形态不对: %v，原文 %s", err, raw)
	}
	if len(body.Families) != len(auditFamilies) || len(body.Actions) < 20 {
		t.Errorf("families/actions = %d/%d", len(body.Families), len(body.Actions))
	}
	if strings.Join(body.Actors, ",") != "admin,ops_bot" {
		t.Errorf("actors = %v，期望去重并排好序的两个人", body.Actors)
	}
	for _, a := range body.Actors {
		rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/audit?actor="+url.QueryEscape(a), "admin", "secret")
		if rec.Code != http.StatusOK {
			t.Errorf("目录里的操作人 %q 被列表接口拒绝：%d %s", a, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"action":"shard.start"`) {
			t.Errorf("按 %q 筛却没给出他做过的那条：%s", a, rec.Body.String())
		}
	}
	if !strings.Contains(body.Note, "前缀") {
		t.Errorf("没说明匹配方式：%q", body.Note)
	}
	for _, it := range body.Actions {
		rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/audit?action="+it.Code, "admin", "secret")
		if rec.Code != http.StatusOK {
			t.Errorf("目录里的 %q 被列表接口拒绝：%d %s", it.Code, rec.Code, rec.Body.String())
		}
	}
}

func TestAuditNeedsAuth(t *testing.T) {
	f := newFixture(t, "")
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit-actions"} {
		if rec := do(t, f.srv.Handler(), http.MethodGet, path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 无凭据 = %d，期望 401", path, rec.Code)
		}
	}
}

// auditView 的字段名是给前端的契约，写错一个字母界面就少一列，而且不会报错。
func TestAuditViewFieldNames(t *testing.T) {
	f := newFixture(t, "")
	seedAudit(t, f, "task.submit", "mod_install")
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/audit", "admin", "secret")
	body := rec.Body.String()
	for _, want := range []string{`"action":"task.submit"`, `"actionLabel":"提交后台任务"`, `"target":"mod_install"`, `"detail":"测试写入"`, `"ok":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("响应里没有 %s，实际 %s", want, body)
		}
	}
}
