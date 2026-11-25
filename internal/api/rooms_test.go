package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/backup"
	"dstpanel/internal/dst"
	"dstpanel/internal/procrun"
)

// 这一页的契约和 /clusters 是同一条：一行一个分片、码值必须配中文、读不到的留空。
// 下面的断言就是盯着这三条写的——尤其是"留空"那条，因为把未知摆成 0 或第 0 天，
// 用户在界面上看不出任何异常，只会照着假数据做决定。

func (f *apiFixture) setSettings(t *testing.T, s dst.GameSettings) {
	t.Helper()
	c, err := dst.OpenCluster(f.paths, f.cluster)
	if err != nil {
		t.Fatalf("打开集群: %v", err)
	}
	c.SetSettings(s)
	if err := c.Save(); err != nil {
		t.Fatalf("写 cluster.ini: %v", err)
	}
}

func (f *apiFixture) setShard(t *testing.T, shard string, s dst.ShardSettings) {
	t.Helper()
	c, err := dst.OpenCluster(f.paths, f.cluster)
	if err != nil {
		t.Fatalf("打开集群: %v", err)
	}
	if err := c.SetShard(shard, s); err != nil {
		t.Fatalf("写 %s/server.ini: %v", shard, err)
	}
}

// writeToken 只造"有没有令牌"这个前提。
// 内容必须是假的：测试数据会进仓库，真的 Klei 令牌绝不能落到磁盘上。
func (f *apiFixture) writeToken(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.paths.TokenFile(f.cluster), []byte("KLEI-测试用假令牌\n"), 0o600); err != nil {
		t.Fatalf("写 cluster_token.txt: %v", err)
	}
}

func (f *apiFixture) dropToken(t *testing.T) {
	t.Helper()
	if err := os.Remove(f.paths.TokenFile(f.cluster)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("删 cluster_token.txt: %v", err)
	}
}

func (f *apiFixture) writeModOverrides(t *testing.T, shard, body string) {
	t.Helper()
	path := f.paths.ModOverrides(f.cluster, shard)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建分片目录: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写 modoverrides.lua: %v", err)
	}
}

func getRooms(t *testing.T, f *apiFixture) (roomsReply, string) {
	t.Helper()
	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/rooms", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("rooms = %d，body %s", rec.Code, rec.Body.String())
	}
	text := rec.Body.String()
	raw, err := json.Marshal(decodeReply(t, rec).Data)
	if err != nil {
		t.Fatalf("重编码 data: %v", err)
	}
	var out roomsReply
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解码 rooms: %v", err)
	}
	// 先确认行数：items 空了的话，后面每一条"字段等于什么"都会假通过。
	if len(out.Items) == 0 {
		t.Fatalf("items 为空，响应 %s", text)
	}
	return out, text
}

func roomRow(t *testing.T, out roomsReply, shard string) roomView {
	t.Helper()
	for _, r := range out.Items {
		if r.Shard == shard {
			return r
		}
	}
	t.Fatalf("没有 %s 这一行，实际 %+v", shard, out.Items)
	return roomView{}
}

func TestRoomsOneRowPerShardWithChineseLabels(t *testing.T) {
	f := newFixture(t, "  20314.dst-Cluster_1-Master\t(Detached)\n")

	f.setSettings(t, dst.GameSettings{
		ClusterName: "本地实测房",
		Description: "周末联机",
		GameMode:    "endless",
		Intention:   "social",
		MaxPlayers:  4,
		PVP:         true,
		TickRate:    20,
	})
	f.setShard(t, "Master", dst.ShardSettings{ServerPort: 10999, IsMaster: true, Name: "地表", ID: 1})
	f.setShard(t, "Caves", dst.ShardSettings{ServerPort: 10998, IsMaster: false, Name: "洞穴", ID: 2,
		MasterIP: "127.0.0.1", MasterPort: 10999})
	f.writeToken(t)
	if err := f.paths.WriteWorldGen(f.cluster, "Master",
		dst.WorldSpec{Preset: "SURVIVAL_TOGETHER", Overrides: map[string]string{}}); err != nil {
		t.Fatalf("写世界设定: %v", err)
	}
	f.writeModOverrides(t, "Master", `return {
	["workshop-111"] = { enabled = true, priority = 0 },
	["workshop-222"] = { enabled = false, priority = 0 },
}
`)
	f.writeStatus(t, "Master",
		`{"shard":"Master","player_count":2,"world":{"cycles":30,"season":"autumn","phase":"dusk"}}`,
		time.Now().Add(-time.Second))

	out, text := getRooms(t, f)
	if out.Clusters != 1 || out.Rooms != 2 {
		t.Errorf("房间/分片计数 = %d/%d，期望 1 个房间摊成 2 行", out.Clusters, out.Rooms)
	}
	if out.Running != 1 {
		t.Errorf("在跑的分片 = %d，期望 1", out.Running)
	}
	if out.Online != 2 {
		t.Errorf("在线合计 = %d，期望 2（只算助手在报数的分片）", out.Online)
	}
	if out.GeneratedAt == "" {
		t.Error("响应没带生成时刻，界面的数据时刻没得显示")
	}

	master := roomRow(t, out, "Master")
	// 房间名和目录名是两件事：界面上必须都拿得到，否则用户照着屏幕上的房间名去改配置会找不到目录。
	if master.RoomName != "本地实测房" || master.Cluster != f.cluster {
		t.Errorf("房间名/目录名 = %q/%q，期望 本地实测房/Cluster_1", master.RoomName, master.Cluster)
	}
	if master.ShardName != "地表" || master.Shard != "Master" {
		t.Errorf("分片名 = %q/%q，期望界面名取 server.ini 的 name", master.ShardName, master.Shard)
	}
	if master.Terrain != dst.LocationForest || master.TerrainLabel != "地表" {
		t.Errorf("地形 = %q/%q", master.Terrain, master.TerrainLabel)
	}
	if !master.Running || master.Pid != 20314 || master.RunningLabel != "运行中" {
		t.Errorf("进程 = %+v，期望运行中 + pid 20314", master)
	}
	if !master.IsMaster || master.Port != 10999 || master.MasterAddr != "" {
		t.Errorf("主机行 = %+v，期望端口 10999 且不带 masterAddr", master)
	}
	// 码值和中文标签必须同时在场：前端排序看码值，展示只读标签。
	if master.GameMode != "endless" || master.GameModeLabel != "无尽" {
		t.Errorf("模式 = %q/%q", master.GameMode, master.GameModeLabel)
	}
	if master.Intention != "social" || master.IntentionLabel != "社交" {
		t.Errorf("偏好 = %q/%q", master.Intention, master.IntentionLabel)
	}
	if master.MaxPlayers != 4 || !master.Pvp || master.TickRate != 20 {
		t.Errorf("规则 = %+v，期望上限 4、PVP 开、帧率 20", master)
	}
	if master.PresetLabel != "标准森林" {
		t.Errorf("预设 = %q，期望中文", master.PresetLabel)
	}
	if master.Access != "online" || master.AccessShort != "公网" {
		t.Errorf("连接 = %q/%q，期望已注册到公网", master.Access, master.AccessShort)
	}
	if !master.HasToken || !strings.Contains(master.AccessLabel, "公网服务器列表") {
		t.Errorf("令牌与连接说明 = %+v", master)
	}
	if !master.WorldReady || !master.HelperOnline || master.HelperNote != "" {
		t.Errorf("助手/世界 = %+v，期望快照新鲜时都不带说明", master)
	}
	// cycles=30（过完了 30 个整天）换算成界面上的"第 31 天"，不是第 30 天也不是第 0 天。
	if master.Online != 2 || master.Day != 31 || master.SeasonLabel != "秋天" || master.PhaseLabel != "黄昏" {
		t.Errorf("世界字段 = %+v", master)
	}
	if master.ModsEnabled == nil || *master.ModsEnabled != 1 {
		t.Errorf("启用模组数 = %v，期望 1", master.ModsEnabled)
	}
	if master.ModsTotal == nil || *master.ModsTotal != 2 {
		t.Errorf("模组总数 = %v，期望 2", master.ModsTotal)
	}
	if master.Description != "周末联机" {
		t.Errorf("房间说明 = %q", master.Description)
	}

	caves := roomRow(t, out, "Caves")
	if caves.Terrain != dst.LocationCave || caves.TerrainLabel != "洞穴" {
		t.Errorf("洞穴地形 = %q/%q", caves.Terrain, caves.TerrainLabel)
	}
	// 洞穴连的是哪个端口只写在洞穴自己的 server.ini 里，
	// 这一条是穿传送门失败最常见的成因，必须原样带到界面上。
	if caves.IsMaster || caves.MasterAddr != "127.0.0.1:10999" {
		t.Errorf("洞穴连主机 = %q，期望 127.0.0.1:10999，实际 %+v", caves.MasterAddr, caves)
	}
	if caves.Running || caves.Pid != 0 || caves.RunningLabel != "未运行" {
		t.Errorf("未启动的分片 = %+v", caves)
	}
	if caves.WorldReady || caves.Online != 0 {
		t.Errorf("没进世界的分片不该报在线人数：%+v", caves)
	}
	if caves.Alert != "" {
		t.Errorf("停着的分片不该被算进要先处理：%q", caves.Alert)
	}
	if !strings.Contains(caves.HelperNote, "还没收到游戏数据") {
		t.Errorf("助手从没回过话的说明 = %q", caves.HelperNote)
	}
	// 没写过 modoverrides.lua 就是"一个模组都没配"，报 0 是对的；
	// 读不出已存在的文件才是"未知"，两者不能混。
	if caves.ModsEnabled == nil || *caves.ModsEnabled != 0 || caves.ModsTotal == nil || *caves.ModsTotal != 0 {
		t.Errorf("没这个文件时分片模组 = %v/%v，期望 0/0", caves.ModsEnabled, caves.ModsTotal)
	}
	if caves.PresetLabel != "" {
		t.Errorf("没写世界设定却报预设：%q", caves.PresetLabel)
	}
	if !strings.Contains(text, `"roomName":"本地实测房"`) {
		t.Errorf("响应里找不到房间名原文：%s", text)
	}
}

// TestRoomsAccessColumnCoversTheThreeSwitches 钉的是"玩家怎么进得来"这一列。
// 三个开关在 cluster.ini 里是分开的，界面只给一个结论，所以四个组合都要有出处。
func TestRoomsAccessColumnCoversTheThreeSwitches(t *testing.T) {
	cases := []struct {
		name       string
		set        dst.GameSettings
		token      bool
		wantAccess string
		wantShort  string
		wantLabel  string
		wantAlert  string
	}{
		{"公网", dst.GameSettings{}, true, "online", "公网", "已注册到公网服务器列表", ""},
		{"缺令牌", dst.GameSettings{}, false, "no-token", "缺令牌", "公网玩家搜不到", "没有登录令牌，公网玩家搜不到"},
		{"仅局域网", dst.GameSettings{LanOnly: true}, true, "lan", "局域网", "仅局域网", ""},
		{"离线房间", dst.GameSettings{OfflineCluster: true}, true, "offline", "离线", "离线房间", ""},
		// 局域网/离线本来就不需要令牌，这时不能再吓人一次。
		{"离线且没令牌也不报缺令牌", dst.GameSettings{OfflineCluster: true}, false, "offline", "离线", "离线房间", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 两个分片都不在跑：进程在跑时"助手没回话"会抢走 alert，
			// 这一条测的是连接判断本身。
			f := newFixture(t, "")
			set := c.set
			set.ClusterName, set.GameMode, set.Intention, set.MaxPlayers = "本地实测房", "survival", "cooperative", 6
			f.setSettings(t, set)
			if c.token {
				f.writeToken(t)
			} else {
				f.dropToken(t)
			}

			out, _ := getRooms(t, f)
			for _, r := range out.Items {
				if r.Access != c.wantAccess || r.AccessShort != c.wantShort {
					t.Errorf("%s 行连接 = %q/%q，期望 %q/%q", r.Shard, r.Access, r.AccessShort, c.wantAccess, c.wantShort)
				}
				if !strings.Contains(r.AccessLabel, c.wantLabel) {
					t.Errorf("%s 行说明 = %q，期望含 %q", r.Shard, r.AccessLabel, c.wantLabel)
				}
				if r.Alert != c.wantAlert {
					t.Errorf("%s 行 alert = %q，期望 %q", r.Shard, r.Alert, c.wantAlert)
				}
			}
			// access 是房间级事实（同一份 cluster.ini、同一个令牌），
			// 两个分片都进 attention 是按行数计，界面文案得说"行"而不是"个房间"。
			want := 0
			if c.wantAlert != "" {
				want = len(out.Items)
			}
			if out.Attention != want {
				t.Errorf("要注意 = %d，期望 %d", out.Attention, want)
			}
		})
	}
}

// TestRoomsRunningWithoutHelper 钉的是这一页最该醒目的那一格：
// 进程在跑、助手不回话，界面上其它字段全是空的或默认值，
// 用户看到的就是"服务器起来了但什么都干不了"，必须当场说破。
func TestRoomsRunningWithoutHelper(t *testing.T) {
	f := newFixture(t, "  5.dst-Cluster_1-Master\t(Detached)\n")
	f.setSettings(t, dst.GameSettings{ClusterName: "本地实测房"})
	f.writeToken(t)

	out, _ := getRooms(t, f)
	master := roomRow(t, out, "Master")
	if master.Alert != "进程在跑，但助手 MOD 没有回话" {
		t.Errorf("alert = %q", master.Alert)
	}
	// 没在跑的 Caves 不该被同一句话点名：它只是停着，不是坏了。
	if out.Attention != 1 {
		t.Errorf("要注意 = %d，期望只有 Master 这一行", out.Attention)
	}
	if roomRow(t, out, "Caves").Alert != "" {
		t.Errorf("停着的分片被算进了要注意：%+v", roomRow(t, out, "Caves"))
	}
	if master.WorldReady || master.HelperOnline {
		t.Errorf("没有快照时不该报世界就绪：%+v", master)
	}
	// 引擎自己那句失败原因要跟着进来（云机 2026-10-31 的真实形状）。
	f.writeBootLog(t, "Master", "[00:00:15]: PANIC: missing required prefab [sculpture_bishop]! Expected 1, got 0\n")
	out, _ = getRooms(t, f)
	if note := roomRow(t, out, "Master").HelperNote; !strings.Contains(note, "PANIC: missing required prefab") {
		t.Errorf("助手离线说明没带上引擎那句：%q", note)
	}
}

// TestRoomsStaleSnapshotIsNotNow 盯的是最隐蔽的假象：
// 快照还在，但已经停更了。这时天数/在线是历史值，摆成"现在"就等于谎报。
func TestRoomsStaleSnapshotIsNotNow(t *testing.T) {
	f := newFixture(t, "  5.dst-Cluster_1-Master\t(Detached)\n")
	f.setSettings(t, dst.GameSettings{ClusterName: "本地实测房"})
	f.writeToken(t)
	f.writeStatus(t, "Master",
		`{"shard":"Master","player_count":3,"world":{"cycles":39,"season":"winter","phase":"night"}}`,
		time.Now().Add(-5*time.Minute))

	out, _ := getRooms(t, f)
	master := roomRow(t, out, "Master")
	if master.WorldReady {
		t.Error("过期快照被当成实时世界报出去了")
	}
	if master.HelperOnline {
		t.Error("快照停更时助手不该算在线")
	}
	if !strings.Contains(master.HelperNote, "过期") {
		t.Errorf("离线原因 = %q，期望写明快照过期", master.HelperNote)
	}
	if master.Alert == "" {
		t.Error("进程在跑但助手停更是这一页最该提示的状态")
	}
	if !master.HelperNoteUrgent {
		t.Errorf("日志里没有暂停标记时，助手停更就是真故障，该报警：%q", master.HelperNote)
	}
	// 顶部"在线玩家"要和每一行的"未知"讲同一个故事，
	// 否则用户信的是那个大数字，而它是五分钟前的人数。
	if out.Online != 0 {
		t.Errorf("在线合计 = %d，期望 0：worldReady 为假的行不计入", out.Online)
	}
}

// TestRoomsPausedRoomIsNotAnIncident 把云机 2026-11-03 那一幕钉住：没人玩时引擎自己
// 把世界冻住，助手随之停摆，于是这一页给两个好好的世界各挂了一条"要先处理"。
// 停摆的原因写在日志里（Server Autopaused），分得清就不该报警——
// 一直报警的筛选条件，用户两三天之后就不看了。
func TestRoomsPausedRoomIsNotAnIncident(t *testing.T) {
	f := newFixture(t, "  5.dst-Cluster_1-Master\t(Detached)\n")
	f.setSettings(t, dst.GameSettings{ClusterName: "本地实测房"})
	f.writeToken(t)
	f.writeStatus(t, "Master",
		`{"shard":"Master","player_count":1,"world":{"cycles":39,"season":"winter","phase":"night"}}`,
		time.Now().Add(-5*time.Minute))
	f.writeBootLog(t, "Master", "[00:51:46]: Server Autopaused\t\n")

	out, _ := getRooms(t, f)
	master := roomRow(t, out, "Master")
	if master.Alert != "" {
		t.Errorf("暂停中的世界不该进「要先处理」，实际 %q", master.Alert)
	}
	if master.HelperNoteUrgent {
		t.Errorf("暂停的说明不该用报警色：%q", master.HelperNote)
	}
	if !strings.Contains(master.HelperNote, "Server Autopaused") {
		t.Errorf("不报警不等于不说明，原因还是要写清楚，实际 %q", master.HelperNote)
	}
	// 这一条是这次改动的正题：世界那一列以前照着 running 自己猜，
	// 于是暂停中的世界被写成"进程在跑，但还没进世界"，和同一行助手那栏直接矛盾。
	if master.WorldNote != "暂停中，读不到实时状态" {
		t.Errorf("暂停中的世界，「世界」那一列 = %q，期望「暂停中，读不到实时状态」", master.WorldNote)
	}
	if out.Attention != 0 {
		t.Errorf("顶部「要先处理」= %d，期望 0", out.Attention)
	}
	caves := roomRow(t, out, "Caves")
	if caves.WorldNote != "" {
		t.Errorf("没在跑的世界，「世界」那一列该留空，实际 %q", caves.WorldNote)
	}
}

// TestRoomsWorldNoteFollowsHelperReason 验剩下的两类：真故障要说"还没进世界"，
// 有快照但停更了只能说"读不到实时状态"——两者下一步动作完全不同，
// 前者去看启动日志，后者是助手 MOD 或引擎卡住了。
func TestRoomsWorldNoteFollowsHelperReason(t *testing.T) {
	f := newFixture(t, "  5.dst-Cluster_1-Master\t(Detached)\n")
	f.setSettings(t, dst.GameSettings{ClusterName: "本地实测房"})
	f.writeToken(t)
	f.writeBootLog(t, "Master", "[00:00:31]: Worldgen had an error and it's being retried!\t\n")

	out, _ := getRooms(t, f)
	master := roomRow(t, out, "Master")
	if master.WorldNote != "进程在跑，但还没进世界" {
		t.Errorf("启动就失败的世界 = %q，期望「进程在跑，但还没进世界」", master.WorldNote)
	}

	// 换一种原因：进程在跑、快照写过但停更了，且日志里没有失败也没有暂停标记。
	// 过期判的是文件 mtime，开发配置的阈值是一天，所以这里要摆到一天以前。
	f.writeBootLog(t, "Master", "[00:00:31]: About to serializ world, save version: 30\n")
	f.writeStatus(t, "Master",
		`{"shard":"Master","player_count":2,"world":{"cycles":8,"season":"autumn","phase":"day"}}`,
		time.Now().Add(-25*time.Hour))
	out2, _ := getRooms(t, f)
	stale := roomRow(t, out2, "Master")
	if stale.WorldNote != "读不到实时状态" {
		t.Errorf("快照停更的世界 = %q，期望「读不到实时状态」", stale.WorldNote)
	}
	if stale.WorldReady {
		t.Errorf("快照过期时天数是历史值，不该报成实时世界")
	}
	if !stale.HelperNoteUrgent {
		t.Errorf("没人暂停却停更，这一次是真故障，该报警：%q", stale.HelperNote)
	}
}

// TestRoomsModCountsUnknownWhenUnreadable：文件存在但解析不了时报"未知"，
// 报 0 会让用户以为模组被清空了，然后去做多余的重装。
func TestRoomsModCountsUnknownWhenUnreadable(t *testing.T) {
	f := newFixture(t, "")
	f.setSettings(t, dst.GameSettings{ClusterName: "本地实测房"})
	f.writeToken(t)
	f.writeModOverrides(t, "Master", "return { [\"workshop-1\"")

	out, _ := getRooms(t, f)
	master := roomRow(t, out, "Master")
	if master.ModsEnabled != nil || master.ModsTotal != nil {
		t.Errorf("读不出来的模组数 = %v/%v，期望留空", master.ModsEnabled, master.ModsTotal)
	}
	// 这一列整列读不出时不影响其它字段：这一行还有进程、端口、世界要报。
	if master.RunningLabel != "未运行" || master.RoomName != "本地实测房" {
		t.Errorf("模组读不出把整行带坏了：%+v", master)
	}
}

// TestRoomsLatestBackupIsRoomLevel 验的是"同房间的两个分片报同一个时间，
// 而且报的是最新那份"：快照本来就是房间级的。
func TestRoomsLatestBackupIsRoomLevel(t *testing.T) {
	f := newFixture(t, "")
	f.setSettings(t, dst.GameSettings{ClusterName: "本地实测房"})
	f.writeToken(t)

	dir := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建快照目录: %v", err)
	}
	for id, at := range map[string]string{
		"20261030T020000Z-Cluster_1-manual":    "2026-10-30T02:00:00Z",
		"20261031T020000Z-Cluster_1-scheduled": "2026-10-31T02:00:00Z",
	} {
		// createdAt 用真格式（archive.go 写的就是 RFC3339 UTC），
		// 这样这一列在界面上拿到的是同一种文本，前端的时间解析才算被验过。
		mf := `{"id":"` + id + `","cluster":"Cluster_1","label":"scheduled","createdAt":"` +
			at + `","shards":["Master"]}`
		if err := os.WriteFile(filepath.Join(dir, id+".manifest.json"), []byte(mf), 0o644); err != nil {
			t.Fatalf("写清单: %v", err)
		}
	}
	f.srv.Backups = &backup.Manager{Paths: f.paths, Dir: dir}

	out, _ := getRooms(t, f)
	for _, r := range out.Items {
		if r.LastBackupAt != "2026-10-31T02:00:00Z" {
			t.Errorf("%s 的最近备份 = %q，期望两行都是最新那份", r.Shard, r.LastBackupAt)
		}
	}
}

// TestRoomsLeavesBackupColumnEmptyWithoutBackupLayer：这一列是锦上添花，
// 没接上快照层时留空，绝不能让整个接口失败。
func TestRoomsLeavesBackupColumnEmptyWithoutBackupLayer(t *testing.T) {
	f := newFixture(t, "")
	f.setSettings(t, dst.GameSettings{ClusterName: "本地实测房"})
	f.writeToken(t)

	out, _ := getRooms(t, f)
	for _, r := range out.Items {
		if r.LastBackupAt != "" {
			t.Errorf("没接快照层却报出备份时间：%q", r.LastBackupAt)
		}
	}
}

// TestRoomsWhenScreenIsMissing 与集群列表同一套优先级：
// 进程状态问不到就 503，把未知渲染成"没在跑"会诱导用户重复点启动。
func TestRoomsWhenScreenIsMissing(t *testing.T) {
	f := newFixture(t, "")
	f.srv.Procs = procrun.New(f.paths, screenRunner{
		err: errors.New(`找不到命令 screen: exec: "screen": executable file not found in %PATH%`),
	})

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/rooms", "admin", "secret")
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

func TestRoomsNeedsAuth(t *testing.T) {
	f := newFixture(t, "")
	if rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/rooms", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("无凭据 = %d，期望 401", rec.Code)
	}
	if rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/rooms", "admin", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("错密码 = %d，期望 401", rec.Code)
	}
}
