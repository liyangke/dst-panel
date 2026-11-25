package modbridge

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/dst"
)

func embeddedRels(t *testing.T) []string {
	t.Helper()
	files, err := HelperEmbeddedFiles()
	if err != nil {
		t.Fatalf("HelperEmbeddedFiles: %v", err)
	}
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	return rels
}

// TestEmbeddedHelperShape 钉的是"打进二进制的那份是不是可用的那份"。
// 这三条都是拿服务端包里读出来的事实反推的，任何一条断了，分片就不会回话，
// 而且断在编译期看不出来的地方。
func TestEmbeddedHelperShape(t *testing.T) {
	rels := embeddedRels(t)
	if len(rels) != 4 {
		t.Fatalf("内嵌文件 = %v，期望 4 个（modinfo/modmain + scripts 下两个）", rels)
	}
	for _, want := range []string{"modinfo.lua", "modmain.lua", "scripts/panel_bridge.lua", "scripts/panel_json.lua"} {
		if !contains(rels, want) {
			t.Errorf("内嵌清单里没有 %s，有 %v", want, rels)
		}
	}

	modmain := mustEmbed(t, "modmain.lua")
	// 引擎只把 MODS_ROOT..modname.."\\scripts\\?.lua" 拼进 package.path（mods.lua:528），
	// 所以桥本体必须在 scripts/ 下，modmain 用 require 找它而不是自己拼路径。
	if !bytes.Contains(modmain, []byte(`pcall(require, "panel_bridge")`)) {
		t.Error("modmain.lua 不是通过 require 载入 panel_bridge 的，加载路径可能不对")
	}
	if bytes.Contains(modmain, []byte("package.path =")) {
		t.Error("modmain.lua 真的给 package.path 赋值了：沙箱 env 里没有 package 这个全局，会直接报错")
	}
	// 沙箱里没有裸 pcall（util.lua:788 的 RunInEnvironment 不带 __index 回退），
	// modmain 必须自己从 GLOBAL 取。
	if !bytes.Contains(modmain, []byte("pcall")) || !bytes.Contains(modmain, []byte("_G.pcall")) {
		t.Error("modmain.lua 没有从 GLOBAL 补 pcall，首次加载就会触发 DisableBecauseBad")
	}

	info := mustEmbed(t, "modinfo.lua")
	// 配置项名字是 Go 侧和 Lua 侧唯一的契约：modindex.lua 的 ApplyConfigOptionOverrides
	// 按 modinfo 里 settings[].name 去匹配 modoverrides.lua 的 configuration_options 键，
	// 两边写串了不会报错，只会让"面板上改了间隔、游戏里还是 1 秒"。
	for _, key := range []string{optPollInterval, optPlayerDetail} {
		if !bytes.Contains(info, []byte(`name = "`+key+`"`)) {
			t.Errorf("modinfo.lua 的 settings 里没有 %q 这一项", key)
		}
		if !bytes.Contains(modmain, []byte(`setting("`+key+`"`)) {
			t.Errorf("modmain.lua 没读配置项 %q，面板改它不会有效果", key)
		}
	}
	if !bytes.Contains(info, []byte("dedicated_server_only = true")) ||
		!bytes.Contains(info, []byte("all_clients_require_mod = false")) {
		t.Error("modinfo.lua 不再是'只服务端、客户端不需要同步'，房间里的玩家会被强制下载这个 MOD")
	}
}

func TestSyncHelperDryRunThenInstall(t *testing.T) {
	b, _ := newFixture(t)
	rels := embeddedRels(t)

	// 只查不写。
	sync, err := b.SyncHelper(true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(sync.Missing) != len(rels) || len(sync.Written) != 0 {
		t.Errorf("dry run 结果 = %+v，期望只报缺失不动文件", sync)
	}
	if _, err := os.Stat(b.Paths.HelperModDir()); !os.IsNotExist(err) {
		t.Error("dry run 竟然把模组目录建出来了")
	}

	// 真装。
	sync, err = b.SyncHelper(false)
	if err != nil {
		t.Fatalf("安装: %v", err)
	}
	if len(sync.Written) != len(rels) {
		t.Fatalf("写了 %d 个文件，期望 %d：%+v", len(sync.Written), len(rels), sync)
	}
	for _, rel := range rels {
		onDisk, err := os.ReadFile(filepath.Join(b.Paths.HelperModDir(), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读回 %s: %v", rel, err)
		}
		if !bytes.Equal(onDisk, mustEmbed(t, rel)) {
			t.Errorf("%s 落盘内容和内嵌的不一致（差 %d 字节）", rel, len(onDisk)-len(mustEmbed(t, rel)))
		}
		// 全新安装不该留下 .bak：那时根本没有旧文件可备份。
		if _, err := os.Stat(filepath.Join(b.Paths.HelperModDir(), filepath.FromSlash(rel)) + ".bak"); err == nil {
			t.Errorf("%s 首次安装就生成了 .bak", rel)
		}
	}

	// 再跑一次必须全是"一致"，而且一个字节都不写——
	// 每次都重写会在模组目录堆 .bak，还会让人怀疑面板偷偷改了 MOD。
	before := modTime(t, filepath.Join(b.Paths.HelperModDir(), "modmain.lua"))
	sync, err = b.SyncHelper(false)
	if err != nil {
		t.Fatalf("第二次安装: %v", err)
	}
	if len(sync.Written) != 0 || len(sync.Matched) != len(rels) || !sync.OK() {
		t.Errorf("第二次结果 = %+v，期望全部 Matched 且不重写", sync)
	}
	if after := modTime(t, filepath.Join(b.Paths.HelperModDir(), "modmain.lua")); !after.Equal(before) {
		t.Error("modmain.lua 被无意义地重写了")
	}
}

func TestSyncHelperDetectsEditedFile(t *testing.T) {
	b, _ := newFixture(t)
	if _, err := b.SyncHelper(false); err != nil {
		t.Fatalf("先装一遍: %v", err)
	}
	target := filepath.Join(b.Paths.HelperModDir(), "modmain.lua")
	if err := os.WriteFile(target, []byte("-- 用户手改的一行\n"), 0o644); err != nil {
		t.Fatalf("手改: %v", err)
	}

	sync, err := b.SyncHelper(true)
	if err != nil {
		t.Fatalf("对账: %v", err)
	}
	if len(sync.Differs) != 1 || sync.Differs[0] != "modmain.lua" {
		t.Fatalf("改动没被发现：+%v", sync)
	}
	if !sync.OK() && len(sync.Missing) != 0 {
		t.Errorf("把改动报成了缺失：%+v", sync)
	}

	// 真写时把它覆盖回内嵌版本，并留下 .bak（用户的改动至少还能从备份里找回）。
	sync, err = b.SyncHelper(false)
	if err != nil {
		t.Fatalf("覆盖: %v", err)
	}
	if len(sync.Written) != 1 || sync.Written[0] != "modmain.lua" {
		t.Errorf("重写明细 = %+v", sync.Written)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !bytes.Equal(raw, mustEmbed(t, "modmain.lua")) {
		t.Error("被改过的 modmain.lua 没有还原成内嵌版本")
	}
	bak, err := os.ReadFile(target + ".bak")
	if err != nil {
		t.Fatalf("没留 .bak: %v", err)
	}
	if !bytes.Contains(bak, []byte("用户手改的一行")) {
		t.Errorf(".bak 里不是用户原来那份：%s", bak)
	}
}

func TestSyncHelperExtraAndLeftovers(t *testing.T) {
	b, _ := newFixture(t)
	if _, err := b.SyncHelper(false); err != nil {
		t.Fatalf("先装一遍: %v", err)
	}
	dir := b.Paths.HelperModDir()

	// 上一次布局留下的旧文件（放错目录的桥本体）必须被认成多余。
	if err := os.WriteFile(filepath.Join(dir, "panel_bridge.lua"), []byte("-- 旧位置\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 面板自己产生的备份不能污染"多余文件"的判定，否则一致性校验永远不干净。
	if err := os.WriteFile(filepath.Join(dir, "modmain.lua.bak"), []byte("-- 备份\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "panel_json.lua.tmp-12345"), []byte("半截"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 子目录里的文件也要能报出来，带上相对路径。
	if err := os.MkdirAll(filepath.Join(dir, "scripts", "levels"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "levels", "extra.lua"), []byte("-- x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sync, err := b.SyncHelper(true)
	if err != nil {
		t.Fatalf("对账: %v", err)
	}
	if !contains(sync.Extra, "panel_bridge.lua") {
		t.Errorf("没把放错位置的旧文件报成多余：%+v", sync.Extra)
	}
	if !contains(sync.Extra, "scripts/levels/extra.lua") {
		t.Errorf("没报出子目录里的多余文件：%+v", sync.Extra)
	}
	for _, e := range sync.Extra {
		if strings.HasSuffix(e, ".bak") || strings.Contains(e, ".tmp-") {
			t.Errorf("把面板自己的备份/临时文件当成多余文件了：%s", e)
		}
	}
	if sync.OK() {
		t.Error("明明有 2 个多余文件却报 OK")
	}
	if len(sync.Matched) != 4 {
		t.Errorf("正常四个文件应全部一致，实际 %+v", sync.Matched)
	}
}

func TestEnableHelperInstallsBeforeEnabling(t *testing.T) {
	b, _ := newFixture(t)
	sync, err := b.EnableHelper("Cluster_1", dst.ShardMaster, true, HelperSettings{PollSeconds: 2, PlayerDetail: true})
	if err != nil {
		t.Fatalf("EnableHelper: %v", err)
	}
	if len(sync.Written) != 4 {
		t.Errorf("助手文件没装齐：%+v", sync)
	}
	raw, err := os.ReadFile(b.Paths.ModOverrides("Cluster_1", dst.ShardMaster))
	if err != nil {
		t.Fatalf("modoverrides.lua 没写：文件装好不等于分片启用了助手: %v", err)
	}
	if !strings.Contains(string(raw), "enabled = true") || !strings.Contains(string(raw), "poll_interval = 2") {
		t.Errorf("条目内容不对：\n%s", raw)
	}
	if missing, err := b.HelperInstallState(); err != nil || len(missing) != 0 {
		t.Errorf("启用后仍缺文件：%+v %v", missing, err)
	}
	// 关掉助手时文件要留着：下次再开就是秒开，而且 Extra 判定不该把助手自己的文件算进去。
	if _, err := b.EnableHelper("Cluster_1", dst.ShardMaster, false, HelperSettings{PollSeconds: 2, PlayerDetail: true}); err != nil {
		t.Fatalf("关闭助手: %v", err)
	}
	off, err := os.ReadFile(b.Paths.ModOverrides("Cluster_1", dst.ShardMaster))
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !strings.Contains(string(off), "enabled = false") || !strings.Contains(string(off), "poll_interval = 2") {
		t.Errorf("关闭时把配置项也抹掉了：\n%s", off)
	}
	if files, _ := os.ReadDir(filepath.Join(b.Paths.HelperModDir(), "scripts")); len(files) != 2 {
		t.Errorf("关闭助手把模组文件一起删了：%v", files)
	}
}

func mustEmbed(t *testing.T, rel string) []byte {
	t.Helper()
	raw, err := helperFS.ReadFile(helperEmbedRoot + "/" + filepath.ToSlash(rel))
	if err != nil {
		t.Fatalf("读内嵌文件 %s: %v", rel, err)
	}
	return raw
}

func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.ModTime()
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
