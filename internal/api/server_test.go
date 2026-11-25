package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/config"
	"dstpanel/internal/dst"
	"dstpanel/internal/hoststats"
	"dstpanel/internal/modbridge"
	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
)

// screenRunner 用固定的 `screen -ls` 输出代替真进程，
// 这样"哪些分片在跑"完全由测试掌握，不依赖本机有没有装 screen。
type screenRunner struct {
	out string
	err error
}

func (r screenRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return r.out, nil
}

type apiFixture struct {
	srv     *Server
	paths   dst.Paths
	cluster string
}

func newFixture(t *testing.T, screenOut string) *apiFixture {
	t.Helper()
	root := t.TempDir()
	paths := dst.Paths{KleiRoot: filepath.Join(root, "klei"), DstInstall: filepath.Join(root, "dst")}
	st, err := store.Open(filepath.Join(root, "panel", "dstpanel.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cluster := "Cluster_1"
	for _, shard := range []string{"Master", "Caves"} {
		if err := os.MkdirAll(filepath.Join(paths.KleiRoot, cluster, shard), 0o755); err != nil {
			t.Fatalf("建分片目录: %v", err)
		}
	}
	// ClusterNames 只认有 cluster.ini 的目录，测试要造出这个前提。
	if err := os.WriteFile(paths.ClusterIni(cluster), []byte("[GAME]\n"), 0o644); err != nil {
		t.Fatalf("写 cluster.ini: %v", err)
	}

	bridge := modbridge.New(paths, config.Helper{PollMs: 1000, StaleSeconds: 15})
	world, err := dst.LoadWorldSchema()
	if err != nil {
		t.Fatalf("LoadWorldSchema: %v", err)
	}
	f := &apiFixture{paths: paths, cluster: cluster}
	procs := procrun.New(paths, screenRunner{out: screenOut})
	// 这里的 screen 是假的、server_log.txt 也永远不存在，按真实的 2 秒等待只是白等；
	// "会话在跑但服务端拒绝启动"那条判定本身在 procrun 包里钉死了。
	procs.BootLogWait = 50 * time.Millisecond
	f.srv = &Server{
		Paths:   paths,
		Procs:   procs,
		Bridge:  bridge,
		Store:   st,
		Auth:    config.Server{AdminUser: "admin", AdminPassword: "secret"},
		Network: config.Network{ShardPortBase: 10998, ShardPortEnd: 11900, MasterIntentPortBase: 11000},
		World:   world,
		// 运行状态采集器接上夹具的 klei 根目录：/api/v1/host 在测试里也要真读一次，
		// 否则"接口 200 但字段全是零"这种问题只有浏览器能发现。
		Host:   hoststats.NewCollector(paths.KleiRoot),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// 公网房间列表接一个假桶（见 lobby_test.go）：默认夹具就要让这一页答得出 200，
	// 否则那条"200 的响应里不许有 null"的全量扫描会因为 503 把它整条跳过。
	f.srv.Lobby = newFixtureLobby(t)
	return f
}

// writeBootLog 造出引擎这一轮启动写下的 server_log.txt。
// 真实引擎每次启动都会把它截断重写，所以测试里直接整份写就行。
func (f *apiFixture) writeBootLog(t *testing.T, shard, body string) {
	t.Helper()
	path := f.paths.ServerLog(f.cluster, shard)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建分片目录: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写 server_log.txt: %v", err)
	}
}

// writeStatus 往助手 MOD 该写的路径落一份快照。
func (f *apiFixture) writeStatus(t *testing.T, shard, body string, mt time.Time) {
	t.Helper()
	path := f.paths.HelperStatusFile(f.cluster, shard)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建 save/panel 目录: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写 status.json: %v", err)
	}
	if !mt.IsZero() {
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("改 mtime: %v", err)
		}
	}
}

func do(t *testing.T, h http.Handler, method, path, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeReply 先把响应壳子解开，失败信息直接断言出来。
// 这里刻意检查 data 非空：之前吃过一次"字段名写错，answer 一直是空串，
// 负向断言反而通过"的亏，所有检查都必须先确认输入真的到了。
func decodeReply(t *testing.T, rec *httptest.ResponseRecorder) reply {
	t.Helper()
	if rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q，期望带 utf-8 的 json", rec.Header().Get("Content-Type"))
	}
	var r reply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，原文 %q", err, rec.Body.String())
	}
	return r
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d，body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("healthz body = %s", rec.Body.String())
	}
}

func TestAuthGate(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	if rec := do(t, h, http.MethodGet, "/api/v1/clusters", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("无凭据 = %d，期望 401", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/clusters", "admin", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("错密码 = %d，期望 401", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/clusters", "other", "secret"); rec.Code != http.StatusUnauthorized {
		t.Errorf("错用户名 = %d，期望 401", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/clusters", "admin", "secret"); rec.Code != http.StatusOK {
		t.Errorf("正确凭据 = %d，body %s", rec.Code, rec.Body.String())
	}

	// 没设密码时绝不能"匿名放行"，也不能用默认密码。
	// 账号是库里的：先把首个管理员删掉，才回到"一个账号都没有"的 503 状态。
	if _, err := f.srv.Store.DB().Exec(`DELETE FROM accounts`); err != nil {
		t.Fatalf("清空账号: %v", err)
	}
	f.srv.Auth.AdminPassword = ""
	rec := do(t, h, http.MethodGet, "/api/v1/clusters", "admin", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("空密码 = %d，期望 503", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "adminPassword") {
		t.Errorf("空密码的提示没告诉用户改哪里：%s", body)
	}
}

func TestClustersOverviewSeparatesProcessAndHelper(t *testing.T) {
	fresh := time.Now().Add(-2 * time.Second)
	old := time.Now().Add(-5 * time.Minute)
	f := newFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")

	f.writeStatus(t, "Master", `{"shard":"Master","player_count":3}`, fresh)
	f.writeStatus(t, "Caves", `{"shard":"Caves","player_count":1}`, old)

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("clusters = %d，body %s", rec.Code, rec.Body.String())
	}
	var out []clusterView
	data := decodeReply(t, rec).Data
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("重编码 data: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解码 data: %v", err)
	}
	if len(out) != 1 || out[0].Name != f.cluster {
		t.Fatalf("集群列表 = %+v，期望只有 %s", out, f.cluster)
	}
	if len(out[0].Shards) != 2 {
		t.Fatalf("分片数 = %d，期望 2", len(out[0].Shards))
	}
	master, caves := out[0].Shards[0], out[0].Shards[1]
	if !master.Running || master.Pid != 20314 {
		t.Errorf("Master 进程态 = %+v，期望 running + pid 20314", master)
	}
	if !master.HelperOnline {
		t.Errorf("Master 助手态 = %+v，期望在线", master)
	}
	// Caves 没进程、快照过期：两个"不在线"要能分开说清。
	if caves.Running || caves.Pid != 0 {
		t.Errorf("Caves 进程态 = %+v，期望未运行", caves)
	}
	if caves.HelperOnline {
		t.Errorf("Caves 助手态 = %+v，期望离线", caves)
	}
	if !strings.Contains(caves.HelperNote, "过期") {
		t.Errorf("Caves 的离线原因是 %q，期望写明快照过期", caves.HelperNote)
	}
	if caves.HelperNoteUrgent {
		t.Errorf("进程根本没起来时\"没数据\"天经地义，不该染成报警色：%+v", caves)
	}

	// 状态文件缺失要写成"还没写入过"，不能把整条路径甩给用户。
	if err := os.Remove(f.paths.HelperStatusFile(f.cluster, "Master")); err != nil {
		t.Fatalf("删 status.json: %v", err)
	}
	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters", "admin", "secret")
	data = decodeReply(t, rec).Data
	raw, _ = json.Marshal(data)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解码 data: %v", err)
	}
	if out[0].Shards[0].HelperOnline || !strings.Contains(out[0].Shards[0].HelperNote, "还没收到游戏数据") {
		t.Errorf("缺文件时的行 = %+v", out[0].Shards[0])
	}
	if !out[0].Shards[0].HelperNoteUrgent {
		t.Errorf("进程在跑、助手却一次都没回过话是真问题（没装/没启用），该报警：%+v", out[0].Shards[0])
	}

	// 云机 2026-10-31 的真实现场：世界生成卡在重试循环里，进程和会话都还在，
	// 助手永远没机会回话。这种"进程活着但没人进得来"的状态必须在总览页上说清，
	// 否则用户看到的只是"助手还没写入过状态"，下一步只能瞎点。
	f.writeBootLog(t, "Master", "[00:00:15]: PANIC: missing required prefab [sculpture_bishop]! Expected 1, got 0\n")
	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters", "admin", "secret")
	raw, _ = json.Marshal(decodeReply(t, rec).Data)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解码 data: %v", err)
	}
	note := out[0].Shards[0].HelperNote
	if !strings.Contains(note, "还没收到游戏数据") || !strings.Contains(note, "PANIC: missing required prefab") {
		t.Errorf("进程在跑但没进世界时，离线原因要把引擎自己那句带上，实际 %q", note)
	}
	if !out[0].Shards[0].HelperNoteUrgent {
		t.Errorf("这种是真进不去世界的故障，必须染成报警色，实际 %q", note)
	}

	// 助手已经回话了就不该再提引擎的失败：那句是几小时前那一轮生成留下的，
	// 现在服务器是好着的，把它继续贴上去就是谎报。
	f.writeStatus(t, "Master", `{"shard":"Master","player_count":0}`, time.Now())
	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters", "admin", "secret")
	raw, _ = json.Marshal(decodeReply(t, rec).Data)
	// 必须换个新的切片：helperNote 带 omitempty，在线时这个键根本不在 JSON 里，
	// 复用旧切片会把上一轮的说明留在 struct 上，测出来的"没清掉"是解码器的假象。
	out = nil
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解码 data: %v", err)
	}
	if !out[0].Shards[0].HelperOnline || out[0].Shards[0].HelperNote != "" {
		t.Errorf("助手在线时不该带任何离线说明，实际 %+v", out[0].Shards[0])
	}

	// 另一种"看着像掉线其实没掉"：没人玩时引擎把世界暂停了，助手随之停摆
	//（引擎 update.lua:229 那句 assert 让暂停期间整个 Lua 调度器不跑）。
	// 只写"助手还没写入过状态"的话，用户会去重启一个本来好好的服务器。
	if err := os.Remove(f.paths.HelperStatusFile(f.cluster, "Master")); err != nil {
		t.Fatalf("删状态快照: %v", err)
	}
	f.writeBootLog(t, "Master", "[00:51:36]: Server Unpaused\t\n[00:51:46]: Server Autopaused\t\n")
	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters", "admin", "secret")
	raw, _ = json.Marshal(decodeReply(t, rec).Data)
	out = nil
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解码 data: %v", err)
	}
	pauseNote := out[0].Shards[0].HelperNote
	if !strings.Contains(pauseNote, "Server Autopaused") || !strings.Contains(pauseNote, "无人时暂停") {
		t.Errorf("暂停中的分片要把引擎那句带上并给出开关名，实际 %q", pauseNote)
	}
	if out[0].Shards[0].HelperOnline {
		t.Errorf("暂停时助手确实没心跳，不该报在线")
	}
	if out[0].Shards[0].HelperNoteUrgent {
		t.Errorf("没人时引擎自己冻住世界是设计如此，不该按故障上色：%q", pauseNote)
	}
	if strings.Contains(pauseNote, "没有新数据") && strings.ContainsAny(pauseNote, "ms") {
		// 过期时长那半句必须是中文（"3 分钟"），不是 Go 的 3m12s。
		t.Errorf("说明里混进了 Go 的时长字面量：%q", pauseNote)
	}
}

// 界面不能出现 Go 的时长字面量（25h1m11s 那种）：任务中心那句超时文案踩过这个坑，
// 总览页"数据已过期"是同一个位置、同一个毛病。
func TestHumanAgoReadsAsChinese(t *testing.T) {
	for d, want := range map[time.Duration]string{
		42 * time.Second:               "42 秒",
		3*time.Minute + 12*time.Second: "3 分钟",
		75 * time.Minute:               "1 小时 15 分钟",
		25*time.Hour + time.Minute:     "1 天 1 小时",
		-3 * time.Second:               "0 秒",
	} {
		if got := humanAgo(d); got != want {
			t.Errorf("humanAgo(%s) = %q，期望 %q", d, got, want)
		}
	}
}

func TestShardStatusRoute(t *testing.T) {
	f := newFixture(t, "")
	body := `{"shard":"Master","server_name":"测试服","game_id":"KU2abc","max_players":6,
		"world":{"cycles":11,"season":"winter","phase":"dusk","temperature":-3.2,"wetness":0},
		"players":[{"name":"阿伟","prefab":"wilson","userid":"KU-1","health":45,"max_health":150,"is_dead":false},
			{"name":"小王","prefab":"wx78","userid":"KU-2","is_dead":true}],
		"player_count":2}`
	f.writeStatus(t, "Master", body, time.Now().Add(-time.Second))

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/status", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，body %s", rec.Code, rec.Body.String())
	}
	text := rec.Body.String()
	// 断言的是"界面会拿到的中文标签"，不是内部码值。
	for _, want := range []string{"冬天", "黄昏", "阿伟", `"online":2`, `"stale":false`} {
		if !strings.Contains(text, want) {
			t.Errorf("响应缺少 %q：%s", want, text)
		}
	}
	if !strings.Contains(text, `"season":"winter"`) {
		t.Error("响应丢了原始 season 值，前端判断逻辑没得用")
	}
	var v struct {
		Players []playerView `json:"players"`
	}
	raw, _ := json.Marshal(decodeReply(t, rec).Data)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("解码 players: %v", err)
	}
	if len(v.Players) != 2 {
		t.Fatalf("玩家数 = %d，期望 2", len(v.Players))
	}
	// 有值就报数，MOD 没写的组件必须是 null/缺字段，不能被编成 0。
	if v.Players[0].Health == nil || *v.Players[0].Health != 45 {
		t.Errorf("第一个玩家血量 = %v，期望 45", v.Players[0].Health)
	}
	if v.Players[1].Hunger != nil {
		t.Errorf("第二个玩家饥饿值 = %v，期望没有该字段（未知不是 0）", *v.Players[1].Hunger)
	}
	if !v.Players[1].Dead || v.Players[0].Dead {
		t.Errorf("存活状态串了：%+v", v.Players)
	}

	// 分片没数据 -> 503，且错误可操作、不带 Go 的英文原文。
	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Caves/status", "admin", "secret")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("没数据的分片 = %d，期望 503", rec.Code)
	}
	if r := decodeReply(t, rec); r.Err == "" || strings.Contains(r.Err, "file does not exist") {
		t.Errorf("503 的文案不合格：%q", r.Err)
	} else if !strings.Contains(r.Err, "还没有状态数据") {
		t.Errorf("503 没说清是什么状态：%q", r.Err)
	}

	// 坏 JSON 是 MOD 侧故障，要报 500 让人去查，不能装作"还没数据"。
	f.writeStatus(t, "Master", `{"shard":`, time.Time{})
	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/status", "admin", "secret")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("坏快照 = %d，期望 500，body %s", rec.Code, rec.Body.String())
	}
}

func TestTasksRouteLabelsAndLimit(t *testing.T) {
	f := newFixture(t, "")
	id, err := f.srv.Store.CreateTask("mod_install", "安装模组：显示血量", map[string]any{"mod": "workshop-1"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := f.srv.Store.ClaimTask(); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := f.srv.Store.SetTaskProgress(id, 40, "下载服务端本体文件，已下 408.9 MB / 4.3 GB",
		&store.TaskDetail{DoneBytes: 428750751, TotalBytes: 4594664662, BytesPerSec: 13000000, EtaSeconds: 320, Phase: "下载服务端本体文件"}); err != nil {
		t.Fatalf("SetTaskProgress: %v", err)
	}

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/tasks", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks = %d，body %s", rec.Code, rec.Body.String())
	}
	text := rec.Body.String()
	if !strings.Contains(text, "执行中") || !strings.Contains(text, `"status":"running"`) {
		t.Errorf("任务响应没同时给出码值和中文标签：%s", text)
	}
	if !strings.Contains(text, "下载服务端本体文件") {
		t.Errorf("任务响应丢了进度说明：%s", text)
	}
	// 数字块那组数必须原样给到界面（解好的一方对象，不是库里那串 JSON 文本）。
	// 这里解结构体而不是比字符串：JSON 把 13000000 写成 1.3e+07，
	// 拿字符串去对就等于把编码细节钉进断言，Go 改一下浮点格式就红给我看。
	var envelope struct {
		OK   bool       `json:"ok"`
		Data []taskView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解不开任务响应: %v", err)
	}
	if len(envelope.Data) != 1 || envelope.Data[0].Detail == nil {
		t.Fatalf("响应里没带进度明细：%s", text)
	}
	d := *envelope.Data[0].Detail
	if d.DoneBytes != 428750751 || d.TotalBytes != 4594664662 || d.Phase != "下载服务端本体文件" {
		t.Errorf("明细三个数不对：%+v", d)
	}
	if d.BytesPerSec != 13000000 || d.EtaSeconds != 320 {
		t.Errorf("速度和剩余时间没原样传出来：%+v", d)
	}

	rec = do(t, f.srv.Handler(), http.MethodGet, "/api/v1/tasks?limit=abc", "admin", "secret")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 limit = %d，期望 400", rec.Code)
	}
	if r := decodeReply(t, rec); !strings.Contains(r.Err, "abc") {
		t.Errorf("非法参数的提示没带上原值：%q", r.Err)
	}
}

// TestRecoverKeepsProcessAlive 直接测中间件：
// 真实路由里没有一个"必然 panic"的入口，构造不出来就别假装测过。
func TestRecoverKeepsProcessAlive(t *testing.T) {
	f := newFixture(t, "")
	var logged string
	f.srv.Logger = slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logged = string(p)
		return len(p), nil
	}), nil))

	h := f.srv.recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("空指针演示")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic 后状态 = %d，期望 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "面板内部错误") {
		t.Errorf("panic 响应没给中文结论：%s", rec.Body.String())
	}
	if !strings.Contains(logged, "空指针演示") || !strings.Contains(logged, "goroutine") {
		t.Errorf("panic 没把原因和栈记进运行日志：%s", logged)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestRequestLogSkipsHealthz 盯的是日志量：探活每秒都可能来一次，
// 全记就把真实操作的痕迹冲没了。
func TestRequestLogSkipsHealthz(t *testing.T) {
	f := newFixture(t, "")
	var logged strings.Builder
	f.srv.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	h := f.srv.Handler()
	logged.Reset() // 首次启动建管理员的日志不影响这条断言

	do(t, h, http.MethodGet, "/healthz", "", "")
	if logged.Len() != 0 {
		t.Errorf("healthz 被记进了请求日志：%s", logged.String())
	}
	do(t, h, http.MethodGet, "/api/v1/tasks", "admin", "secret")
	out := logged.String()
	if !strings.Contains(out, "/api/v1/tasks") || !strings.Contains(out, "status=200") {
		t.Errorf("普通请求没记全：\n%s", out)
	}
}

func TestMethodNotAllowedFromMux(t *testing.T) {
	f := newFixture(t, "")
	// 全部接口都是 GET/POST 分明的；这里确认路由确实带了方法约束。
	rec := do(t, f.srv.Handler(), http.MethodDelete, "/api/v1/clusters", "admin", "secret")
	if rec.Code == http.StatusOK {
		t.Error("DELETE /api/v1/clusters 竟然成功了")
	}
}

// TestClustersWhenScreenIsMissing 是在 Windows 上真跑出来的形状：
// 总览页宁可 503 说明"问不到进程"，也不能把"未知"渲染成"没在跑"。
func TestClustersWhenScreenIsMissing(t *testing.T) {
	f := newFixture(t, "")
	f.srv.Procs = procrun.New(f.paths, screenRunner{err: errors.New(`找不到命令 screen: exec: "screen": executable file not found in %PATH%`)})

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters", "admin", "secret")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("screen 缺失时 = %d，期望 503，body %s", rec.Code, rec.Body.String())
	}
	body := decodeReply(t, rec).Err
	if !strings.Contains(body, "screen") || !strings.Contains(body, "同机") {
		t.Errorf("提示没告诉用户该做什么：%q", body)
	}
	if strings.Contains(body, "executable file not found") {
		t.Errorf("把 Go 的英文原文甩给了界面：%q", body)
	}
}

func TestPublicErrorTranslation(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want string
	}{
		{"空", nil, ""},
		{
			"英文前缀被丢掉",
			fmt.Errorf("file does not exist：C:/x/status.json 还没生成"),
			"C:/x/status.json 还没生成",
		},
		{
			"中文前缀保留（结论在后半句之前）",
			fmt.Errorf("助手 MOD 没有响应：查 server_log.txt"),
			"助手 MOD 没有响应：查 server_log.txt",
		},
		{"没有冒号原样返回", errors.New("快照不是合法 JSON"), "快照不是合法 JSON"},
		{"权限", &os.PathError{Op: "open", Path: "/x", Err: os.ErrPermission}, "面板没有权限读这个文件，检查运行用户和文件属主"},
		{"缺文件", fmt.Errorf("包一层: %w", os.ErrNotExist), "文件不存在，可能是这个世界还没生成过存档"},
		{
			"助手离线的诊断必须原样带着",
			fmt.Errorf("%w：%s/%s 还没有 status.json。可能原因：助手 MOD 文件不全",
				modbridge.ErrHelperOffline, "Cluster_1", "Master"),
			"助手 MOD 没有响应：Cluster_1/Master 还没有 status.json。可能原因：助手 MOD 文件不全",
		},
		{
			"命令还在排队时把原话给出",
			fmt.Errorf("%w：命令 1-1 在队列里超过 15s 仍未执行", modbridge.ErrCommandPending),
			"命令还在队列里等待执行：命令 1-1 在队列里超过 15s 仍未执行",
		},
	}
	for _, c := range cases {
		got := publicError(c.in)
		if got != c.want {
			t.Errorf("%s：publicError(%v) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// 处理器不读请求体时，中间件必须替它把剩下的字节抽干。
//
// 为什么这一条值得单独钉：写接口里绝大多数（启停、指派、改设置）的主语在路径上，
// 处理器压根不碰体。响应写完了体还留在接收队列里，这时关连接，Windows 会用 RST
// 而不是 FIN 收尾，客户端连"已经写好的响应字节"一起收不到——界面上就是点了「启动」
// 而面板报网络错误，审计里却已经记下了一条 400。本机 30 次一组实测：
// 不读体的 POST 重置 14/30（Connection: close）与 10/30（keep-alive），
// 由处理器或这一层抽干净的两组都是 0/30。
func TestDrainBodyConsumesWhatHandlerLeft(t *testing.T) {
	const left = 5000
	var s Server
	h := s.drainBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusBadRequest, "这一支接口不读请求体")
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/clusters/Cluster_1/shards/Master/start",
		strings.NewReader(strings.Repeat("x", left)))
	h.ServeHTTP(httptest.NewRecorder(), req)

	rest, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("抽干以后再读请求体回错：%v", err)
	}
	if len(rest) != 0 {
		t.Errorf("处理器没读的请求体还剩 %d 字节没抽干", len(rest))
	}
}

// 端到端那一遍：同一根连接上连着打三次"不读体"的写请求，每次都拿得到响应。
// 断言的是响应本身（状态码 + 那句中文），因为它才是用户看得见的东西；
// 至于连接有没有被 RST，Windows 上是真会偶发的，Linux 上这一遍只是自证接线没坏。
func TestUnreadBodyRequestsStillAnswer(t *testing.T) {
	f := newFixture(t, "")
	srv := httptest.NewServer(f.srv.Handler())
	defer srv.Close()

	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/clusters/Nope_9/shards/Master/start",
			strings.NewReader(`{"note":"客户端总会带一个体"}`))
		if err != nil {
			t.Fatalf("造请求: %v", err)
		}
		req.SetBasicAuth("admin", "secret")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("第 %d 次请求失败（连接被提前掐断就是这一条要抓的）: %v", i+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "Nope_9") {
			t.Errorf("第 %d 次 = %d %s", i+1, resp.StatusCode, string(body)[:min(120, len(body))])
		}
	}
}
