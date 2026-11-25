package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/dst"
	"dstpanel/internal/store"
)

// playersView 的三份名单顺序、生效说明和花名册合并，是这个页面全部的信息来源。
// 这里一次把"只在名单里/只在线/只在库里"三种人都摆出来，防止合并时漏掉某一路。
func TestPlayersViewMergesListsRosterAndOnline(t *testing.T) {
	f := newFixture(t, "")
	seenAt := time.Date(2026, 3, 5, 8, 0, 0, 0, time.UTC)
	if err := f.srv.Store.UpsertSeenPlayers(f.cluster, "Master", []store.SeenPlayer{
		{UserID: "KU_seen", Name: "老玩家", Prefab: "wendy"},
	}, seenAt); err != nil {
		t.Fatal(err)
	}
	if err := dst.WritePlayerList(f.paths.AdminListFile(f.cluster), []string{"KU_listonly"}); err != nil {
		t.Fatal(err)
	}
	if err := dst.WritePlayerList(f.paths.BlocklistFile(f.cluster), []string{"KU_seen"}); err != nil {
		t.Fatal(err)
	}
	f.writeStatus(t, "Master", `{"shard":"Master","players":[
		{"name":"在线甲","prefab":"wilson","userid":"KU_online","health":88}]}`, time.Now())
	// 过期的分片不算在线：拿旧快照去做踢人，目标可能早就下线了。
	f.writeStatus(t, "Caves", `{"shard":"Caves","players":[
		{"name":"旧快照","prefab":"willow","userid":"KU_stale"}]}`, time.Now().Add(-time.Hour))

	rec := do(t, f.srv.Handler(), http.MethodGet,
		"/api/v1/clusters/Cluster_1/players", "admin", "secret")
	var v playersView
	decodeData(t, rec, &v)

	if len(v.Lists) != 3 {
		t.Fatalf("应固定给出三份名单，实际 %d", len(v.Lists))
	}
	wantOrder := []string{dst.PlayerListAdmin, dst.PlayerListWhitelist, dst.PlayerListBlocklist}
	for i, key := range wantOrder {
		g := v.Lists[i]
		if g.Key != key {
			t.Errorf("第 %d 份名单 = %q，期望 %q", i, g.Key, key)
		}
		if g.Label == "" || g.Effect == "" || g.File == "" {
			t.Errorf("名单 %q 缺标签/说明/文件路径: %+v", key, g)
		}
	}
	if len(v.Online) != 1 || v.Online[0].UserID != "KU_online" {
		t.Errorf("只该有新鲜分片里的在线玩家，实际 %+v", v.Online)
	}
	if len(v.RunningShards) != 1 || v.RunningShards[0] != "Master" {
		t.Errorf("正在回话的分片只该有 Master，实际 %+v", v.RunningShards)
	}
	if !strings.Contains(v.Note, "在线操作") {
		t.Errorf("有分片在跑时要说清在线操作走助手，实际 %q", v.Note)
	}

	byID := map[string]rosterEntryView{}
	for _, e := range v.Roster {
		byID[e.UserID] = e
	}
	if len(byID) != 3 {
		t.Fatalf("花名册应是三路并集（3 人），实际 %d: %+v", len(byID), v.Roster)
	}
	if e := byID["KU_listonly"]; e.Name != "" || e.Online {
		t.Errorf("只在名单里的人没有历史资料: %+v", e)
	}
	if e := byID["KU_seen"]; !e.Blocked || e.Name != "老玩家" {
		t.Errorf("黑名单标记或历史名字丢了: %+v", e)
	}
	if e := byID["KU_online"]; !e.Online || e.Name != "在线甲" || e.Prefab != "wilson" {
		t.Errorf("在线玩家应直接进花名册并带角色: %+v", e)
	}
	if v.Roster[0].UserID != "KU_online" {
		t.Errorf("在线玩家应排在最前，实际 %q", v.Roster[0].UserID)
	}
}

// 名单内容原样写回引擎读的文件，并留一条能看懂的操作记录。
func TestPlayerListAddAndRemoveWritesFileAndAudit(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost,
		"/api/v1/clusters/Cluster_1/players/lists/admin",
		playerListRequest{Action: "add", UserID: " KU_abc "})
	var v playersView
	decodeData(t, rec, &v)
	if len(v.Lists[0].Entries) != 1 || v.Lists[0].Entries[0].UserID != "KU_abc" {
		t.Fatalf("加完应立刻在管理员名单里看到，实际 %+v", v.Lists[0])
	}
	ids, err := dst.ReadPlayerList(f.paths.AdminListFile(f.cluster))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "KU_abc" {
		t.Errorf("adminlist.txt 内容不对: %#v", ids)
	}
	entries, err := f.srv.Store.ListAudit(10, 0, "player.list.")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != "player.list.add" ||
		entries[0].Actor != "admin" || !strings.Contains(entries[0].Detail, "KU_abc") {
		t.Errorf("加名单要有操作记录: %+v", entries)
	}

	// 再点一次"添加"不该重写文件，也不该多一份 .bak。
	if err := os.Remove(f.paths.AdminListFile(f.cluster) + ".bak"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	rec = doJSON(t, h, http.MethodPost,
		"/api/v1/clusters/Cluster_1/players/lists/admin",
		playerListRequest{Action: "add", UserID: "KU_abc"})
	if rec.Code != http.StatusOK {
		t.Fatalf("重复添加应照常返回当前视图: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(f.paths.AdminListFile(f.cluster) + ".bak"); err == nil {
		t.Error("没有实际改动却重写了文件（多出 .bak）")
	}

	rec = doJSON(t, h, http.MethodPost,
		"/api/v1/clusters/Cluster_1/players/lists/admin",
		playerListRequest{Action: "remove", UserID: "KU_abc"})
	decodeData(t, rec, &v)
	if len(v.Lists[0].Entries) != 0 {
		t.Errorf("删完名单应清空，实际 %+v", v.Lists[0].Entries)
	}
	ids, _ = dst.ReadPlayerList(f.paths.AdminListFile(f.cluster))
	if len(ids) != 0 {
		t.Errorf("adminlist.txt 应清空，实际 %#v", ids)
	}
}

func TestPlayerListRejectsBadInput(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	cases := []struct {
		name, path string
		body       playerListRequest
	}{
		{"未知名单", "/api/v1/clusters/Cluster_1/players/lists/mods", playerListRequest{Action: "add", UserID: "KU_a"}},
		{"未知动作", "/api/v1/clusters/Cluster_1/players/lists/admin", playerListRequest{Action: "toggle", UserID: "KU_a"}},
		{"换行注入", "/api/v1/clusters/Cluster_1/players/lists/admin", playerListRequest{Action: "add", UserID: "KU_a\nKU_b"}},
		{"空 ID", "/api/v1/clusters/Cluster_1/players/lists/admin", playerListRequest{Action: "add", UserID: "  "}},
	}
	for _, c := range cases {
		rec := doJSON(t, h, http.MethodPost, c.path, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应 400，实际 %d: %s", c.name, rec.Code, rec.Body.String())
			continue
		}
		// 错误里要给出可选的取值，不能只说 "invalid"。
		if c.name == "未知名单" && !strings.Contains(rec.Body.String(), "whitelist") {
			t.Errorf("未知名单的报错没列出可选值: %s", rec.Body.String())
		}
	}
	for _, path := range []string{f.paths.AdminListFile(f.cluster), f.paths.BlocklistFile(f.cluster)} {
		if ids, _ := dst.ReadPlayerList(path); len(ids) != 0 {
			t.Errorf("被拒的请求写出了内容 %s: %#v", path, ids)
		}
	}
}

// 踢人和拉黑必须在服务端再要一次确认：只靠前端弹窗等于把风险交给调用方自觉。
func TestPlayerActionRequiresConfirmForKickAndBan(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	f.writeStatus(t, "Master", `{"shard":"Master","players":[
		{"name":"在线甲","userid":"KU_online"}]}`, time.Now())

	for _, action := range []string{"kick", "ban"} {
		rec := doJSON(t, h, http.MethodPost,
			"/api/v1/clusters/Cluster_1/players/KU_online/"+action, playerActionRequest{})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "confirm") {
			t.Errorf("%s 不带确认应 400 并说明缺 confirm，实际 %d: %s", action, rec.Code, rec.Body.String())
		}
	}
	if _, err := os.Stat(f.paths.HelperCommandQueue(f.cluster, "Master")); err == nil {
		t.Error("没确认的动作仍然进了队列")
	}
}

// 离线的人不能踢/禁言；报错要说清是"不在线"，而不是让人去查配置。
func TestPlayerActionRejectsOfflineKickAndMute(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	rec := doJSON(t, h, http.MethodPost,
		"/api/v1/clusters/Cluster_1/players/KU_ghost/kick", playerActionRequest{Confirm: true})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不在线") {
		t.Errorf("离线踢人应 400 并点明不在线，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPost,
		"/api/v1/clusters/Cluster_1/players/KU_ghost/mute", playerActionRequest{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不在线") {
		t.Errorf("离线禁言应 400 并点明不在线，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 没有分片回话时，离线封禁无处执行：503 要指向"先把分片开起来"。
func TestPlayerBanWithoutRunningShardIs503(t *testing.T) {
	f := newFixture(t, "")
	rec := doJSON(t, f.srv.Handler(), http.MethodPost,
		"/api/v1/clusters/Cluster_1/players/KU_ghost/ban", playerActionRequest{Confirm: true})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "世界") {
		t.Fatalf("没有分片时应 503 并说明原因，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "先把世界开起来") {
		t.Errorf("进程真的没起来时该建议开起来，实际 %s", rec.Body.String())
	}
}

// "没人在玩"和"暂停时问不到"必须是两句话。
// 云机现场：他一个人进服又离开，引擎按「无人时暂停」冻住 Lua，界面上写着"此刻没有玩家在线"，
// 而同一行的总览还挂着"在线 1/6"——一句假结论加一句自相矛盾，比不写更糟。
func TestPlayersViewSeparatesPausedFromEmpty(t *testing.T) {
	const screenLine = "20314.dst-Cluster_1-Master\t(Detached)\n"

	t.Run("世界被引擎暂停着", func(t *testing.T) {
		f := newFixture(t, screenLine)
		// 云机 2026-11-03 的真实形态：词是 Server Autopaused，行尾带制表符。
		f.writeBootLog(t, "Master", "[00:51:36]: Server Unpaused\t\n[00:51:46]: Server Autopaused\t\n")
		rec := do(t, f.srv.Handler(), http.MethodGet,
			"/api/v1/clusters/Cluster_1/players", "admin", "secret")
		var v playersView
		decodeData(t, rec, &v)
		if len(v.PausedShards) != 1 || v.PausedShards[0] != "Master" {
			t.Errorf("暂停中的世界该单列出来，实际 %+v", v.PausedShards)
		}
		if len(v.RunningShards) != 0 {
			t.Errorf("暂停时助手没心跳，不该算在回话的世界里，实际 %+v", v.RunningShards)
		}
		for _, want := range []string{"暂停", "不代表没人在玩"} {
			if !strings.Contains(v.Note, want) {
				t.Errorf("说明里缺 %q，实际 %q", want, v.Note)
			}
		}
		if strings.Contains(v.Note, "先把世界开起来") {
			t.Errorf("进程明明在跑，不该建议去重启它：%q", v.Note)
		}

		// 同一种空档下的离线封禁：503 的出口要指向"等恢复"，不是"把世界开起来"。
		ban := doJSON(t, f.srv.Handler(), http.MethodPost,
			"/api/v1/clusters/Cluster_1/players/KU_ghost/ban", playerActionRequest{Confirm: true})
		if ban.Code != http.StatusServiceUnavailable || !strings.Contains(ban.Body.String(), "暂停") {
			t.Errorf("暂停中的封禁应说清是暂停，实际 %d: %s", ban.Code, ban.Body.String())
		}
		if strings.Contains(ban.Body.String(), "先把世界开起来") {
			t.Errorf("暂停不是没开起来，别让人去重启进程：%s", ban.Body.String())
		}
	})

	t.Run("世界在跑但助手没回话", func(t *testing.T) {
		f := newFixture(t, screenLine)
		f.writeBootLog(t, "Master", "[00:00:44]: Sim unpaused\n[00:01:02]: World gen complete\n")
		rec := do(t, f.srv.Handler(), http.MethodGet,
			"/api/v1/clusters/Cluster_1/players", "admin", "secret")
		var v playersView
		decodeData(t, rec, &v)
		if len(v.PausedShards) != 0 {
			t.Errorf("日志里没有暂停标记时不该编出暂停状态，实际 %+v", v.PausedShards)
		}
		if !strings.Contains(v.Note, "没有世界在回话") {
			t.Errorf("这一种才是真的没回话，说明要保留原话，实际 %q", v.Note)
		}
	})

	// 这一条是本地实测逼出来的：停掉的世界，日志最后一句照样是 Server Autopaused。
	// 不先问进程就照日志宣布"暂停中，有人进来自会自动恢复"，
	// 而界面上那一格明明写着没在跑——同一屏两个互相矛盾的口径。
	t.Run("已经关掉的世界不算暂停", func(t *testing.T) {
		f := newFixture(t, screenLine)
		// screenLine 里只有 Master 有会话；Caves 这轮已经关了，日志还停在最后一次自动暂停。
		f.writeBootLog(t, "Caves", "[00:51:46]: Server Autopaused\t\n")
		rec := do(t, f.srv.Handler(), http.MethodGet,
			"/api/v1/clusters/Cluster_1/players", "admin", "secret")
		var v playersView
		decodeData(t, rec, &v)
		if len(v.PausedShards) != 0 {
			t.Errorf("进程不在跑就不该报暂停，实际 %+v", v.PausedShards)
		}
		if !strings.Contains(v.Note, "没有世界在回话") {
			t.Errorf("关着的世界该说没在跑，实际 %q", v.Note)
		}

		ban := doJSON(t, f.srv.Handler(), http.MethodPost,
			"/api/v1/clusters/Cluster_1/players/KU_ghost/ban", playerActionRequest{Confirm: true})
		if ban.Code != http.StatusServiceUnavailable || !strings.Contains(ban.Body.String(), "先把世界开起来") {
			t.Errorf("世界真关着时该建议开起来，实际 %d: %s", ban.Code, ban.Body.String())
		}
	})
}

// 在线动作按 userid 入队，并且落到玩家所在的那个分片。
// 这里把请求上下文取消掉来跳过 10 秒回执等待：命令已经进队列，等待本身在
// TestCommandPostReturns202WhenHelperIsQuiet 那条路径里钉住了。
func TestPlayerActionEnqueuesByUserIDOnPlayersShard(t *testing.T) {
	f := newFixture(t, "")
	f.writeStatus(t, "Master", `{"shard":"Master","players":[]}`, time.Now())
	f.writeStatus(t, "Caves", `{"shard":"Caves","players":[
		{"name":"洞穴甲","prefab":"wilson","userid":"KU_cave"}]}`, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/clusters/Cluster_1/players/KU_cave/mute", strings.NewReader(`{}`)).WithContext(ctx)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("没等到回执应 202，实际 %d: %s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(f.paths.HelperCommandQueue(f.cluster, "Caves"))
	if err != nil {
		t.Fatalf("命令没进洞穴分片的队列: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	if !strings.Contains(line, "|mute|KU_cave") {
		t.Errorf("队列行应按 userid 下发 mute，实际 %q", line)
	}
	if _, err := os.Stat(f.paths.HelperCommandQueue(f.cluster, "Master")); err == nil {
		t.Error("玩家在洞穴，命令却进了地表分片的队列")
	}
	var r reply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r.Data)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["shard"] != "Caves" || got["id"] == "" {
		t.Errorf("202 里要带命令 id 与分片名，实际 %+v", got)
	}
}
