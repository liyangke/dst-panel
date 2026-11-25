package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/modbridge"
)

// doJSON 带请求体发一次请求。GET 用例继续用 do()，这里只为 PUT/POST 存在。
func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("编码请求体: %v", err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeData 把响应壳里的 data 解进目标类型。
// 一定要先确认 data 非空：字段名写错时 data 会解成零值，负向断言反而一片绿。
func decodeData(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 %d，body %s", rec.Code, rec.Body.String())
	}
	var r reply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，原文 %q", err, rec.Body.String())
	}
	if !r.OK || r.Data == nil {
		t.Fatalf("响应里没有数据: %s", rec.Body.String())
	}
	body, err := json.Marshal(r.Data)
	if err != nil {
		t.Fatalf("重新编码 data: %v", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("解 data 失败: %v，原文 %s", err, body)
	}
}

func TestHelperShowReportsMissingFilesBeforeInstall(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/shards/Master/helper", "admin", "secret")
	var v helperView
	decodeData(t, rec, &v)

	if v.Installed {
		t.Errorf("什么都没装却报已安装: %+v", v)
	}
	// 缺失项必须等于内嵌清单：这里不写死 4，以后加文件时用例不该先过期。
	embedded, err := modbridge.HelperEmbeddedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Missing) != len(embedded) || len(v.Files) != len(embedded) {
		t.Errorf("缺失 %d 项、清单 %d 项、内嵌 %d 项，应一致", len(v.Missing), len(v.Files), len(embedded))
	}
	if v.Configured {
		t.Error("还没启用过却报成已配置")
	}
	if v.PollMin != modbridge.MinHelperPollSeconds || v.PollMax != modbridge.MaxHelperPollSeconds {
		t.Errorf("范围应回给界面，实际 %d~%d", v.PollMin, v.PollMax)
	}
	if !strings.Contains(v.Hint, "启用") {
		t.Errorf("提示要说下一步做什么，实际 %q", v.Hint)
	}
	// 每个内嵌文件都要带哈希，界面靠它回答"我装的到底是哪一版"。
	for _, file := range v.Files {
		if len(file.SHA256) != 8 || file.Path == "" {
			t.Errorf("文件条目不完整: %+v", file)
		}
	}
}

func TestHelperSetInstallsFilesThenWritesOverride(t *testing.T) {
	f := newFixture(t, "")
	trueVal, five, detail := true, 5, true
	rec := doJSON(t, f.srv.Handler(), http.MethodPut,
		"/api/v1/clusters/Cluster_1/shards/Master/helper",
		helperSetRequest{Enabled: &trueVal, PollSeconds: &five, PlayerDetail: &detail})
	var v helperView
	decodeData(t, rec, &v)

	if !v.Installed || !v.Enabled || v.PollSeconds != 5 {
		t.Errorf("启用后应装好文件并写进 override，实际 %+v", v)
	}
	dir := f.paths.HelperModDir()
	for _, rel := range []string{"modinfo.lua", "modmain.lua",
		filepath.Join("scripts", "panel_bridge.lua"), filepath.Join("scripts", "panel_json.lua")} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("助手文件没落地: %s (%v)", rel, err)
		}
	}
	ovText, err := os.ReadFile(f.paths.ModOverrides(f.cluster, "Master"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ovText), "DSTPanelHelper") || !strings.Contains(string(ovText), "enabled = true") {
		t.Errorf("modoverrides.lua 里没看到启用的助手条目:\n%s", ovText)
	}
	// configuration_options 的键必须和 modinfo.lua 里 settings[].name 一致，
	// 引擎是按这个名字匹配的，写成 ["name"] = "poll_interval" 那种形式反而不生效。
	if !strings.Contains(string(ovText), "poll_interval = 5") {
		t.Errorf("轮询间隔没按引擎认的形式写进 override:\n%s", ovText)
	}
	// 启停是要改服务端配置的動作，必须留操作记录。
	entries, err := f.srv.Store.ListAudit(10, 0, "helper.")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Actor != "admin" || !entries[0].OK {
		t.Errorf("helper.enable 审计记录不对: %+v", entries)
	}
	if !strings.Contains(v.Hint, "重启") {
		t.Errorf("启用完必须提醒重启分片才加载，实际 %q", v.Hint)
	}
}

func TestHelperSetKeepsOtherFieldsWhenOnlyToggling(t *testing.T) {
	f := newFixture(t, "")
	on, five := true, 5
	if rec := doJSON(t, f.srv.Handler(), http.MethodPut,
		"/api/v1/clusters/Cluster_1/shards/Master/helper",
		helperSetRequest{Enabled: &on, PollSeconds: &five}); rec.Code != http.StatusOK {
		t.Fatalf("首次启用: %s", rec.Body.String())
	}
	// 只关 enabled，不带 pollSeconds：沿用现状，不能把间隔悄悄打回默认值。
	off := false
	rec := doJSON(t, f.srv.Handler(), http.MethodPut,
		"/api/v1/clusters/Cluster_1/shards/Master/helper",
		helperSetRequest{Enabled: &off})
	var v helperView
	decodeData(t, rec, &v)
	if v.Enabled {
		t.Error("应该已经关闭")
	}
	if v.PollSeconds != 5 {
		t.Errorf("只改开关不该重置轮询间隔，实际 %d", v.PollSeconds)
	}
	entries, err := f.srv.Store.ListAudit(10, 0, "helper.disable")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("关闭也要单独记一条，实际 %+v", entries)
	}
}

func TestHelperSetRejectsOutOfRangePoll(t *testing.T) {
	f := newFixture(t, "")
	for _, bad := range []int{0, modbridge.MaxHelperPollSeconds + 1} {
		on := true
		rec := doJSON(t, f.srv.Handler(), http.MethodPut,
			"/api/v1/clusters/Cluster_1/shards/Master/helper",
			helperSetRequest{Enabled: &on, PollSeconds: &bad})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("pollSeconds=%d 应该 400，实际 %d: %s", bad, rec.Code, rec.Body.String())
			continue
		}
		if body := rec.Body.String(); !strings.Contains(body, "秒之间") {
			t.Errorf("要说清楚允许的范围，实际 %s", body)
		}
	}
	// 400 之后不能留下半个 override。
	if _, err := os.Stat(f.paths.ModOverrides(f.cluster, "Master")); err == nil {
		t.Error("参数被拒了却还是写了 modoverrides.lua")
	}
}

func TestHelperSetNeedsEnabled(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPut,
		"/api/v1/clusters/Cluster_1/shards/Master/helper", helperSetRequest{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "enabled") {
		t.Errorf("缺 enabled 要 400 并点名这个字段，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 集群/分片名来自 URL，后面所有读写都以此为目录前缀，所以这两层必须先挡住。
func TestShardScopeRejectsUnknownTargets(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_9/shards/Master/helper", "admin", "secret")
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的集群应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "Cluster_9") || !strings.Contains(body, "Cluster_1") {
		t.Errorf("404 里要给出集群叫什么和现有什么，实际 %s", body)
	}

	rec = do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Nope/helper", "admin", "secret")
	if rec.Code != http.StatusNotFound {
		t.Errorf("不存在的分片应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 带 .. 的路径不能被当成合法分片写进别处。
	on := true
	rec = doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/shards/..%2Fevil/helper",
		helperSetRequest{Enabled: &on})
	if rec.Code == http.StatusOK {
		t.Fatalf("跨目录的分片名竟然成功了: %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(f.paths.KleiRoot, "evil")); err == nil {
		t.Error("越界路径被写出来了")
	}
}

func TestCommandsListIsChineseLabelled(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/commands", "admin", "secret")
	var out []commandView
	decodeData(t, rec, &out)
	if len(out) != len(modbridge.CommandNames()) {
		t.Fatalf("列表数量要和桥一致: %d vs %d", len(out), len(modbridge.CommandNames()))
	}
	for _, c := range out {
		if c.Name == "" || c.Label == "" || c.Label == c.Name {
			t.Errorf("命令 %q 没有中文标签", c.Name)
		}
		if len(c.ArgsHint) == 0 {
			t.Errorf("命令 %q 没写参数说明", c.Name)
		}
	}
}

func TestCommandPostRejectsUnknownCommand(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPost,
		"/api/v1/clusters/Cluster_1/shards/Master/commands",
		commandRequest{Command: "c_shutdown"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未收录的命令应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "c_shutdown") || !strings.Contains(body, modbridge.CmdAnnounce) {
		t.Errorf("要把能用的命令列给用户，实际 %s", body)
	}
	// 被拒的命令一条都不该进队列。
	if _, err := os.Stat(f.paths.HelperCommandQueue(f.cluster, "Master")); err == nil {
		t.Error("非法命令仍然写进了 queue.txt")
	}
}

func TestCommandPostRejectsMultilineArg(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPost,
		"/api/v1/clusters/Cluster_1/shards/Master/commands",
		commandRequest{Command: modbridge.CmdAnnounce, Args: []string{"第一行\n第二行"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "换行") {
		t.Errorf("带换行的参数要挡住并说明原因，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCommandPostQueuesWithoutWaiting(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPost,
		"/api/v1/clusters/Cluster_1/shards/Master/commands",
		commandRequest{Command: modbridge.CmdAnnounce, Args: []string{"服务器 5 分钟后重启"}})
	var rep commandReply
	decodeData(t, rec, &rep)
	if rep.ID == "" || rep.Done {
		t.Errorf("没等待回执时不该说已完成: %+v", rep)
	}
	if rep.Pending != 1 {
		t.Errorf("队列里应该就这 1 条，实际 %d", rep.Pending)
	}
	queue, err := os.ReadFile(f.paths.HelperCommandQueue(f.cluster, "Master"))
	if err != nil {
		t.Fatal(err)
	}
	// 队列行的格式是 id|命令|参数，MOD 侧按这个拆。
	line := strings.TrimSpace(string(queue))
	if !strings.HasPrefix(line, rep.ID+"|"+modbridge.CmdAnnounce+"|") ||
		!strings.HasSuffix(line, "服务器 5 分钟后重启") {
		t.Errorf("队列内容不对: %q", line)
	}
	entries, err := f.srv.Store.ListAudit(10, 0, "bridge.")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != "bridge."+modbridge.CmdAnnounce || entries[0].Actor != "admin" {
		t.Errorf("公告要有操作记录: %+v", entries)
	}
}

// 助手活着但这条命令还没轮到：必须回 202 并说明"稍后仍可能执行"，
// 不能回 5xx 让用户以为失败了再点一次——那会在游戏里播两遍公告。
func TestCommandPostReturns202WhenHelperIsQuiet(t *testing.T) {
	f := newFixture(t, "")
	f.writeStatus(t, "Master", `{"shard":"Master","players":[]}`, time.Now())

	rec := doJSON(t, f.srv.Handler(), http.MethodPost,
		"/api/v1/clusters/Cluster_1/shards/Master/commands",
		commandRequest{Command: modbridge.CmdAnnounce, Args: []string{"现在上线发糖果"}, WaitSeconds: 1})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("应回 202，实际 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "仍在队列") {
		t.Errorf("202 里要说清命令还在排，实际 %s", body)
	}
	var r reply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r.Data)
	var rep commandReply
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.ID == "" || rep.Done {
		t.Errorf("202 的 data 要带着命令 id 且未完成: %+v", rep)
	}
}

func TestCommandPost503WhenHelperHasNoHeartbeat(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPost,
		"/api/v1/clusters/Cluster_1/shards/Master/commands",
		commandRequest{Command: modbridge.CmdPing, WaitSeconds: 1})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("助手从没写过快照时应 503，实际 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(strings.ToLower(body), "file does not exist") {
		t.Errorf("内部措辞漏到界面了: %s", body)
	}
	// 503 必须带上桥那边拼的诊断（缺文件 / 没启用 / 没进世界），
	// 光一句"助手 MOD 没有响应"等于把最有用的半句扔了。
	if !strings.Contains(body, "可能原因") {
		t.Errorf("503 要给出可动手的诊断，实际 %s", body)
	}
}

func TestAuditFallsBackToSystemActor(t *testing.T) {
	f := newFixture(t, "")
	// 直接在测试里调 audit：不带 Basic 头的内部调用也要留下操作者，不能空着。
	f.srv.audit(httptest.NewRequest(http.MethodPost, "/x", nil), "test.act", "Cluster_1/Master", "细节", nil)
	entries, err := f.srv.Store.ListAudit(10, 0, "test.")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Actor != "system" {
		t.Errorf("没认证时操作者要是 system: %+v", entries)
	}
}
