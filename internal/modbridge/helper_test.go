package modbridge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/config"
	"dstpanel/internal/dst"
)

const wantOverride = `return {
	DSTPanelHelper = {
		priority = 0,
		enabled = true,
		configuration_options = {
			poll_interval = 1,
			player_detail = true,
		},
	},
}
`

func TestSetHelperOverrideWritesExpectedLua(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", dst.ShardMaster)

	if err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, true, HelperSettings{PollSeconds: 1, PlayerDetail: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("modoverrides.lua 没写出来: %v", err)
	}
	if string(raw) != wantOverride {
		t.Fatalf("内容不符。\n想要:\n%s\n实际:\n%s", wantOverride, string(raw))
	}
	// 这份文件会被 RunInEnvironment(fn, {}) 在空 env 里执行，括号一律是语法错误。
	if strings.ContainsAny(string(raw), "()") {
		t.Errorf("生成的 modoverrides.lua 里出现了括号，空 env 下会执行失败:\n%s", raw)
	}
}

func TestSetHelperOverrideKeepsOtherModsAndOrder(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", dst.ShardMaster)
	seed := `return {
	["workshop-378160973"] = {
		enabled = true,
		priority = -100,
		configuration_options = {
			enable_player_respawn = true,
		},
	},
}
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, true, HelperSettings{PollSeconds: 5, PlayerDetail: false}); err != nil {
		t.Fatal(err)
	}

	tbl, _, err := dst.LoadLua(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := tbl.Keys()
	if len(keys) != 2 || keys[0] != "workshop-378160973" {
		t.Fatalf("已有条目顺序被动了: %+v", keys)
	}
	workshop := tbl.Get("workshop-378160973")
	if workshop.Kind != dst.KindTable || workshop.T.Get("priority").AsNumber(0) != -100 {
		t.Fatalf("workshop 条目被改写: %+v", workshop)
	}
	if !workshop.T.Get("configuration_options").T.Has("enable_player_respawn") {
		t.Errorf("workshop 的配置项丢了")
	}

	// 文本层面也要确认原文没被吃掉。
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "enable_player_respawn = true") {
		t.Errorf("原有条目内容不在文件里:\n%s", raw)
	}

	got, err := b.GetHelperOverride("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present || !got.Enabled {
		t.Fatalf("读回的状态不对: %+v", got)
	}
	if got.Settings.PollSeconds != 5 || got.Settings.PlayerDetail {
		t.Errorf("设置项没按写入值读回: %+v", got.Settings)
	}
}

func TestSetHelperOverrideDisableKeepsSettings(t *testing.T) {
	b, _ := newFixture(t)
	settings := HelperSettings{PollSeconds: 2, PlayerDetail: true}
	if err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, true, settings); err != nil {
		t.Fatal(err)
	}
	if err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, false, settings); err != nil {
		t.Fatal(err)
	}
	got, err := b.GetHelperOverride("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present || got.Enabled {
		t.Fatalf("关闭后条目要还在、只把 enabled 置否: %+v", got)
	}
	// 文本层面确认配置项还在。
	raw, _ := os.ReadFile(b.Paths.ModOverrides("Cluster_1", dst.ShardMaster))
	text := string(raw)
	if !strings.Contains(text, "enabled = false") || !strings.Contains(text, "poll_interval = 2") {
		t.Errorf("关闭时把配置项一起抹掉了:\n%s", text)
	}
}

// 面板的状态接口会反复调这条，内容没变就不该再动文件——每次动都会多一份 .bak。
func TestSetHelperOverrideIsIdempotent(t *testing.T) {
	b, _ := newFixture(t)
	settings := HelperSettings{PollSeconds: 1, PlayerDetail: true}
	if err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, true, settings); err != nil {
		t.Fatal(err)
	}
	path := b.Paths.ModOverrides("Cluster_1", dst.ShardMaster)
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, true, settings); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Errorf("内容没变却还是重写了文件")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Errorf("重复写同一份配置还留下了 .bak")
	}
}

func TestSetHelperOverrideValidates(t *testing.T) {
	b, _ := newFixture(t)
	for _, bad := range []int{0, -3, 61, 3600} {
		err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, true, HelperSettings{PollSeconds: bad, PlayerDetail: true})
		if err == nil {
			t.Fatalf("间隔 %d 秒应该被拒绝", bad)
		}
		if !strings.Contains(err.Error(), "秒之间") {
			t.Errorf("错误信息要能让人看懂怎么改，实际: %v", err)
		}
	}
}

func TestSetHelperOverrideRefusesBrokenFile(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", dst.ShardMaster)
	if err := os.WriteFile(path, []byte("return { [ = ] }"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, true, HelperSettings{PollSeconds: 1, PlayerDetail: true})
	if err == nil {
		t.Fatal("坏文件应该拒绝写入，而不是覆盖掉用户的排查线索")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("报错的同时还把文件改了:\n%s", after)
	}
}

func TestGetHelperOverrideWithoutFileOrEntry(t *testing.T) {
	b, _ := newFixture(t)
	got, err := b.GetHelperOverride("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatalf("文件不存在时不该报错，这是新房间的常态: %v", err)
	}
	if got.Present || got.Enabled {
		t.Errorf("不该读到条目: %+v", got)
	}

	path := b.Paths.ModOverrides("Cluster_1", dst.ShardMaster)
	if err := os.WriteFile(path, []byte("return {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = b.GetHelperOverride("Cluster_1", dst.ShardMaster); err != nil || got.Present {
		t.Errorf("空表里不该有助手条目: %+v %v", got, err)
	}
}

// 第三方模组偶尔把数字写成带引号的形式，引擎照样能用，面板也不能因此显示成 0。
func TestGetHelperOverrideAcceptsQuotedNumber(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", dst.ShardMaster)
	seed := `return {
	DSTPanelHelper = {
		enabled = true,
		configuration_options = {
			poll_interval = "4",
			player_detail = false,
		},
	},
}
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := b.GetHelperOverride("Cluster_1", dst.ShardMaster)
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings.PollSeconds != 4 || got.Settings.PlayerDetail {
		t.Errorf("读回的设置不对: %+v", got.Settings)
	}
}

func TestHelperOverrideKeepsManualPriority(t *testing.T) {
	b, _ := newFixture(t)
	path := b.Paths.ModOverrides("Cluster_1", dst.ShardMaster)
	seed := `return {
	DSTPanelHelper = {
		priority = -50,
		enabled = true,
	},
}
`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, true, HelperSettings{PollSeconds: 1, PlayerDetail: true}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "priority = -50") {
		t.Errorf("用户手改的 priority 被覆盖回 0:\n%s", raw)
	}
}

func TestHelperDefaultsComeFromConfig(t *testing.T) {
	b, _ := newFixture(t)
	if b.Helper.PollSeconds != 1 {
		t.Errorf("PollMs=1000 应该换成 1 秒，实际 %+v", b.Helper)
	}
	slow := New(dst.Paths{}, config.Helper{PollMs: 5000, StaleSeconds: 15})
	if slow.Helper.PollSeconds != 5 || !slow.Helper.PlayerDetail {
		t.Errorf("默认设置没从配置换算过来: %+v", slow.Helper)
	}
	// PollMs 配成小于 1000 时不能换算成 0 秒——MOD 那边会被引擎按最小间隔跑，
	// 面板显示 0 秒则是睁眼说瞎话。
	zero := New(dst.Paths{}, config.Helper{PollMs: 500, StaleSeconds: 15})
	if zero.Helper.PollSeconds != MinHelperPollSeconds {
		t.Errorf("过小的 PollMs 要夹到下限，实际 %+v", zero.Helper)
	}
}

func TestHelperInstallState(t *testing.T) {
	b, _ := newFixture(t)
	dir := b.Paths.HelperModDir()

	missing, err := b.HelperInstallState()
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := HelperEmbeddedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != len(embedded) {
		t.Fatalf("什么都没装时应该报 %d 项缺失（内嵌清单的全集），实际 %+v", len(embedded), missing)
	}

	installHelper(t, b)
	if missing, err = b.HelperInstallState(); err != nil || len(missing) != 0 {
		t.Errorf("装齐了却还报缺失: %+v %v", missing, err)
	}

	// 这就是这次踩到的坑：文件在模组根目录而不是 scripts/，引擎照样"看得见模组"，
	// 但 modmain 里的 require 会失败，助手静默不干活。必须在这里报出来。
	if err := os.Remove(filepath.Join(dir, "scripts", "panel_bridge.lua")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "panel_bridge.lua"), []byte("--"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing, err = b.HelperInstallState()
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != "scripts/panel_bridge.lua" {
		t.Errorf("放错位置的桥没被识别出来: %+v", missing)
	}
}

// 把助手三个文件按线上布局摆好，供诊断相关的用例使用。
// installHelper 用真实安装路径装助手：直接跑 SyncHelper 把内嵌文件写下去。
// 手写几个占位文件的做法在 HelperInstallState 改成按内嵌清单比对之后就不成立了
// ——少一个文件就会让"文件不全"分支抢先命中，测不到本来想测的那条诊断。
func installHelper(t *testing.T, b *Bridge) {
	t.Helper()
	sync, err := b.SyncHelper(false)
	if err != nil {
		t.Fatalf("装助手 MOD: %v", err)
	}
	if len(sync.Missing) == 0 && len(sync.Written) == 0 && len(sync.Differs) == 0 {
		t.Fatal("SyncHelper 什么都没写，内嵌清单是空的？")
	}
}

func TestOfflineErrorNamesActionableCauses(t *testing.T) {
	b, _ := newFixture(t)

	// 什么都没做：既要报文件不全，也要报没启用，两个都是用户能自己修的。
	err := b.offlineError("Cluster_1", dst.ShardMaster, "1-1")
	msg := err.Error()
	if !strings.Contains(msg, "文件不全") || !strings.Contains(msg, "scripts/panel_bridge.lua") {
		t.Errorf("没提缺文件: %s", msg)
	}
	if !strings.Contains(msg, "从未启用") {
		t.Errorf("没提未启用: %s", msg)
	}
	if !strings.Contains(msg, b.Paths.HelperModDir()) {
		t.Errorf("没给出安装位置，用户不知道往哪儿放: %s", msg)
	}

	installHelper(t, b)
	err = b.offlineError("Cluster_1", dst.ShardMaster, "1-1")
	msg = err.Error()
	if strings.Contains(msg, "文件不全") {
		t.Errorf("装好了却还报缺文件: %s", msg)
	}
	if !strings.Contains(msg, "modoverrides.lua") {
		t.Errorf("仍然要指出启用问题: %s", msg)
	}

	if err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, false, HelperSettings{PollSeconds: 1, PlayerDetail: true}); err != nil {
		t.Fatal(err)
	}
	msg = b.offlineError("Cluster_1", dst.ShardMaster, "1-1").Error()
	if !strings.Contains(msg, "关闭状态") {
		t.Errorf("条目在但关着，应该说清: %s", msg)
	}

	// 启用 + 文件齐全，这时候才是真的"去看游戏日志"。
	if err := b.SetHelperOverride("Cluster_1", dst.ShardMaster, true, HelperSettings{PollSeconds: 1, PlayerDetail: true}); err != nil {
		t.Fatal(err)
	}
	err = b.offlineError("Cluster_1", dst.ShardMaster, "1-1")
	msg = err.Error()
	if !strings.Contains(msg, "游戏日志") {
		t.Errorf("排掉前两种后要把人指向日志: %s", msg)
	}
	// 文案再怎么变，哨兵错误不能丢——上层靠它区分"助手离线"和其它故障。
	if !errors.Is(err, ErrHelperOffline) {
		t.Errorf("丢了 ErrHelperOffline: %v", err)
	}
}
