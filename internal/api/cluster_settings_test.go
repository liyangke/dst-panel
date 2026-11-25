// 集群设置这一层的用例：读回来的是什么、改完盘上是什么、拦住了哪些值。
//
// 和开服引导那组同样的原则：回复说得再好听，也要打开 cluster.ini 看一眼。
// 另外两条专门盯着安全语义：密码原值不能出现在响应里，空密码不能意外把密码摘掉。
package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/dst"
)

func seedClusterSettings(t *testing.T, f *apiFixture, set dst.GameSettings) {
	t.Helper()
	c, err := dst.OpenCluster(f.paths, f.cluster)
	if err != nil {
		t.Fatalf("OpenCluster: %v", err)
	}
	c.SetSettings(set)
	if err := c.Save(); err != nil {
		t.Fatalf("写 cluster.ini: %v", err)
	}
}

func baseSettings() dst.GameSettings {
	return dst.GameSettings{
		ClusterName: "测试服", Description: "夜里开", Password: "s3cret",
		GameMode: "survival", Intention: "social", MaxPlayers: 8,
		PVP: true, PauseWhenEmpty: false, VoteEnabled: true, VoteKickEnabled: true,
		LanOnly: false, OfflineCluster: true, Language: "en", TickRate: 20,
		WhitelistSlots: 2, ConsoleEnabled: false, MaxSnapshots: 20,
	}
}

func TestClusterSettingsShowReadsIniWithoutLeakingPassword(t *testing.T) {
	f := newFixture(t, "20314.dst-Cluster_1-Master\t(Detached)\n")
	seedClusterSettings(t, f, baseSettings())

	rec := do(t, f.srv.Handler(), http.MethodGet, "/api/v1/clusters/Cluster_1/settings", "admin", "secret")
	var v clusterSettingsView
	decodeData(t, rec, &v)

	if v.DisplayName != "测试服" || v.GameMode != "survival" || v.MaxPlayers != 8 || v.TickRate != 20 {
		t.Errorf("读回来的值和文件不一致: %+v", v)
	}
	if !v.HasPassword {
		t.Error("文件里有密码却报没有")
	}
	if !v.Pvp || v.PauseWhenEmpty || !v.VoteEnabled || !v.VoteKickEnabled || v.LanOnly {
		t.Errorf("这几个开关没按文件读回: %+v", v)
	}
	if v.WhitelistSlots != 2 || v.ConsoleEnabled {
		t.Errorf("白名单位/控制台读错: %+v", v)
	}
	if len(v.RunningShards) != 1 || v.RunningShards[0] != "Master" {
		t.Errorf("在跑的分片应只有 Master: %+v", v.RunningShards)
	}
	// 密码明文既不能进字段也不能藏在别的名字下，所以直接搜整份响应。
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Errorf("响应里出现了密码明文: %s", rec.Body.String())
	}
	if v.Path == "" {
		t.Error("没给 cluster.ini 路径，排查时用户还得自己找")
	}
}

func TestClusterSettingsPutWritesFileAndAuditsFieldNamesOnly(t *testing.T) {
	f := newFixture(t, "20314.dst-Cluster_1-Master\t(Detached)\n")
	seedClusterSettings(t, f, baseSettings())

	rec := doJSON(t, f.srv.Handler(), http.MethodPut, "/api/v1/clusters/Cluster_1/settings",
		clusterSettingsPatch{
			DisplayName: sptr("新名字"), MaxPlayers: iptr(10), TickRate: iptr(30),
			PauseWhenEmpty: bptr(true), Password: sptr("n3wpass"), WhitelistSlots: iptr(3),
		})
	var v clusterSettingsView
	decodeData(t, rec, &v)
	if v.DisplayName != "新名字" || v.MaxPlayers != 10 || v.TickRate != 30 || !v.PauseWhenEmpty {
		t.Errorf("保存后的回读没跟上: %+v", v)
	}
	if !strings.Contains(v.Note, "Master") || !strings.Contains(v.Note, "重启") {
		t.Errorf("在跑的分片要提示重启才生效，实际 %q", v.Note)
	}

	ini := readOrDie(t, f.paths.ClusterIni(f.cluster))
	for _, want := range []string{"cluster_name = 新名字", "max_players = 10", "tick_rate = 30",
		"pause_when_empty = true", "cluster_password = n3wpass", "whitelist_slots = 3"} {
		if !strings.Contains(ini, want) {
			t.Errorf("cluster.ini 缺少 %q:\n%s", want, ini)
		}
	}
	// 没提到的项必须原样留着：补丁语义不是"整份覆盖成默认"。
	if !strings.Contains(ini, "game_mode = survival") || !strings.Contains(ini, "cluster_description = 夜里开") {
		t.Errorf("没提到的字段被改掉了:\n%s", ini)
	}
	if _, err := os.Stat(f.paths.ClusterIni(f.cluster) + ".bak"); err != nil {
		t.Errorf("写 cluster.ini 应该留 .bak: %v", err)
	}

	entries, err := f.srv.Store.ListAudit(10, 0, "cluster.settings")
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应该有 1 条集群设置审计，实际 %d", len(entries))
	}
	e := entries[0]
	if !e.OK || e.Target != "Cluster_1" {
		t.Errorf("审计内容不对: %+v", e)
	}
	for _, want := range []string{"服务器名称", "最大玩家数", "重设连接密码"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("审计值得点名改了哪项（%s），实际 %q", want, e.Detail)
		}
	}
	// 审计只写项名，不写值——名称、说明和密码都不该在操作记录里重复一份。
	for _, leak := range []string{"新名字", "n3wpass"} {
		if strings.Contains(e.Detail, leak) {
			t.Errorf("审计里出现了具体值 %q: %s", leak, e.Detail)
		}
	}
}

func TestClusterSettingsPutRejectsValuesKleiWouldSilentlyIgnore(t *testing.T) {
	f := newFixture(t, "")
	seedClusterSettings(t, f, baseSettings())
	before := readOrDie(t, f.paths.ClusterIni(f.cluster))

	cases := []struct {
		name  string
		patch clusterSettingsPatch
	}{
		{"空名称", clusterSettingsPatch{DisplayName: sptr("  ")}},
		{"名称换行", clusterSettingsPatch{DisplayName: sptr("第一行\nconsole_enabled = false")}},
		{"说明换行", clusterSettingsPatch{Description: sptr("a\r\nb")}},
		{"密码换行", clusterSettingsPatch{Password: sptr("a\nb")}},
		{"模式不认识", clusterSettingsPatch{GameMode: sptr("creative")}},
		{"偏好不认识", clusterSettingsPatch{Intention: sptr("solo")}},
		{"语言不认识", clusterSettingsPatch{Language: sptr("ja")}},
		{"人数为 0", clusterSettingsPatch{MaxPlayers: iptr(0)}},
		{"人数超上限", clusterSettingsPatch{MaxPlayers: iptr(17)}},
		{"通信频率不在白名单", clusterSettingsPatch{TickRate: iptr(25)}},
		{"白名单位超过人数", clusterSettingsPatch{WhitelistSlots: iptr(9)}},
		{"负数白名单位", clusterSettingsPatch{WhitelistSlots: iptr(-1)}},
		{"清空又给新密码", clusterSettingsPatch{ClearPassword: bptr(true), Password: sptr("x")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, f.srv.Handler(), http.MethodPut,
				"/api/v1/clusters/Cluster_1/settings", tc.patch)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应回 400，实际 %d：%s", rec.Code, rec.Body.String())
			}
			if after := readOrDie(t, f.paths.ClusterIni(f.cluster)); after != before {
				t.Errorf("被拒的请求不该动 cluster.ini:\n%s", after)
			}
		})
	}
}

func TestClusterSettingsPasswordRequiresExplicitClear(t *testing.T) {
	f := newFixture(t, "")
	seedClusterSettings(t, f, baseSettings())
	h := f.srv.Handler()
	iniPath := f.paths.ClusterIni(f.cluster)

	// 空字符串是"没填"，不是"清空"：密码框回填不了原值，误判一次就没有密码了。
	decodeData(t, doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/settings",
		clusterSettingsPatch{Password: sptr("")}), &clusterSettingsView{})
	if ini := readOrDie(t, iniPath); !strings.Contains(ini, "cluster_password = s3cret") {
		t.Fatalf("空密码把原密码弄丢了:\n%s", ini)
	}

	var v clusterSettingsView
	decodeData(t, doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/settings",
		clusterSettingsPatch{ClearPassword: bptr(true)}), &v)
	if v.HasPassword {
		t.Error("清空后还报有密码")
	}
	if ini := readOrDie(t, iniPath); strings.Contains(ini, "s3cret") {
		t.Fatalf("清空后文件里还留着旧密码:\n%s", ini)
	}
}

func TestClusterSettingsNoOpDoesNotRewriteFile(t *testing.T) {
	f := newFixture(t, "")
	seedClusterSettings(t, f, baseSettings())
	iniPath := f.paths.ClusterIni(f.cluster)
	// 先把 seed 时留下的 .bak 拿掉：接下来"有没有重写"就看它会不会再出现。
	if err := os.Remove(iniPath + ".bak"); err != nil {
		t.Fatalf("删 seed 的 .bak: %v", err)
	}

	var v clusterSettingsView
	decodeData(t, doJSON(t, f.srv.Handler(), http.MethodPut, "/api/v1/clusters/Cluster_1/settings",
		clusterSettingsPatch{}), &v)
	if !strings.Contains(v.Note, "没有改动") {
		t.Errorf("空补丁应明说没改，实际 %q", v.Note)
	}
	if _, err := os.Stat(iniPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("没改动却重写了文件（.bak 又出现了，err=%v）", err)
	}
}

// 文件里本来就有一项面板认不出的取值（手工改过，或者老版本写的）时，
// 界面明说了"不动它就保持原样"，后端就不能因为那一项把整次保存挡下来。
func TestClusterSettingsPutLeavesPreExistingUnknownValuesAlone(t *testing.T) {
	f := newFixture(t, "")
	broken := baseSettings()
	broken.ClusterName = ""
	broken.GameMode = "creative"
	broken.Intention = "solo"
	broken.Language = "ja"
	broken.TickRate = 25
	broken.WhitelistSlots = 99
	broken.MaxPlayers = 8
	seedClusterSettings(t, f, broken)
	h := f.srv.Handler()

	// 这些项都不该被"顺手修正"：界面没碰它们，保存就不该动它们。
	rec := doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/settings",
		clusterSettingsPatch{DisplayName: sptr("救回来")})
	if rec.Code != http.StatusOK {
		t.Fatalf("只改名字应放行，实际 %d：%s", rec.Code, rec.Body.String())
	}
	ini := readOrDie(t, f.paths.ClusterIni(f.cluster))
	for _, want := range []string{"cluster_name = 救回来", "game_mode = creative",
		"cluster_intention = solo", "cluster_language = ja", "tick_rate = 25", "whitelist_slots = 99"} {
		if !strings.Contains(ini, want) {
			t.Errorf("没碰的项被改掉了，缺少 %q:\n%s", want, ini)
		}
	}

	// 主动把这些项改成面板不认识的取值仍然要拒。
	before := readOrDie(t, f.paths.ClusterIni(f.cluster))
	for _, p := range []clusterSettingsPatch{
		{GameMode: sptr("creative")}, {Intention: sptr("solo")},
		{Language: sptr("ja")}, {TickRate: iptr(25)}, {WhitelistSlots: iptr(99)},
	} {
		if rec := doJSON(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/settings", p); rec.Code != http.StatusBadRequest {
			t.Errorf("主动改成不认识的取值应回 400，实际 %d：%s", rec.Code, rec.Body.String())
		}
	}
	if after := readOrDie(t, f.paths.ClusterIni(f.cluster)); after != before {
		t.Errorf("被拒的请求不该动 cluster.ini:\n%s", after)
	}
}

func TestClusterSettingsPatchRoundTripsEveryExposedField(t *testing.T) {
	f := newFixture(t, "")
	seedClusterSettings(t, f, baseSettings())

	var v clusterSettingsView
	decodeData(t, doJSON(t, f.srv.Handler(), http.MethodPut, "/api/v1/clusters/Cluster_1/settings",
		clusterSettingsPatch{
			Description: sptr("新的说明"), GameMode: sptr("endless"), Intention: sptr("madness"),
			Pvp: bptr(false), PauseWhenEmpty: bptr(true), VoteEnabled: bptr(false),
			VoteKickEnabled: bptr(false), LanOnly: bptr(true), OfflineCluster: bptr(false),
			Language: sptr("zh"), ConsoleEnabled: bptr(true), WhitelistSlots: iptr(8),
		}), &v)

	if v.Description != "新的说明" || v.GameMode != "endless" || v.Intention != "madness" {
		t.Errorf("文本项没落盘: %+v", v)
	}
	if v.Pvp || !v.PauseWhenEmpty || v.VoteEnabled || v.VoteKickEnabled || !v.LanOnly || v.OfflineCluster {
		t.Errorf("开关项没落盘: %+v", v)
	}
	if v.Language != "zh" || !v.ConsoleEnabled || v.WhitelistSlots != 8 {
		t.Errorf("语言/控制台/白名单没落盘: %+v", v)
	}
	// 路径也要是这台机器上的那一份，不能是拼出来的相对路径。
	if filepath.Base(v.Path) != "cluster.ini" {
		t.Errorf("路径不对: %q", v.Path)
	}
}
