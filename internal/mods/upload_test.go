package mods

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeZip 写一个临时 zip 并返回路径。entries 的键就是条目名，可以故意写非法名字。
func makeZip(t *testing.T, entries ...[2]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mod.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, e := range entries {
		hw := &zip.FileHeader{Name: e[0], Method: zip.Deflate}
		if strings.HasSuffix(e[0], "/") {
			hw.Method = zip.Store
		}
		fw, err := w.CreateHeader(hw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInstallFromZipFlatLayout(t *testing.T) {
	m, _ := newManager(t)
	zipPath := makeZip(t,
		[2]string{"modinfo.lua", `name = "上传模组"
version = "1.0"
`},
		[2]string{"modmain.lua", "local TUNING = require('tuning')\nprint(TUNING)"},
		[2]string{"images/README", "text"},
	)
	target, err := m.InstallFromZip(zipPath, "UploadedMod")
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(m.Paths.ModsRoot(), "UploadedMod") {
		t.Errorf("落地路径不对: %s", target)
	}
	body, err := os.ReadFile(filepath.Join(target, "modmain.lua"))
	if err != nil || string(body) != "local TUNING = require('tuning')\nprint(TUNING)" {
		t.Fatalf("modmain.lua 内容不对: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(target, "images", "README")); err != nil {
		t.Errorf("子目录没建出来: %v", err)
	}
	// 临时目录必须扫干净，否则 mods/ 下会多出服务端要扫的垃圾目录。
	leftovers := tempLeftovers(t, m)
	if len(leftovers) != 0 {
		t.Errorf("mods/ 下残留临时目录: %v", leftovers)
	}
	// 列表里要能认出它，名字来自 modinfo。
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	var found *Mod
	for i := range list {
		if list[i].Dir == "UploadedMod" {
			found = &list[i]
		}
	}
	if found == nil || !found.ModinfoOK || found.Name != "上传模组" || found.Version != "1.0" {
		t.Errorf("上传后的模组没被正确识别: %+v", found)
	}
}

func TestInstallFromZipStripsSingleTopDir(t *testing.T) {
	m, _ := newManager(t)
	zipPath := makeZip(t,
		[2]string{"Geometric_Department/", ""},
		[2]string{"Geometric_Department/modinfo.lua", `name = "几何防御"
`},
		[2]string{"Geometric_Department/scripts/modmain.lua", "print('hi')"},
	)
	target, err := m.InstallFromZip(zipPath, "Geometric_Department")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "modinfo.lua")); err != nil {
		t.Errorf("顶层目录没剥掉: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "Geometric_Department")); err == nil {
		t.Error("剥完还留了一层套壳")
	}
	if _, err := os.Stat(filepath.Join(target, "scripts", "modmain.lua")); err != nil {
		t.Errorf("子目录被剥错了: %v", err)
	}
}

func TestInstallFromZipRejectsTraversal(t *testing.T) {
	m, _ := newManager(t)
	// 目标目录名一律用 ASCII：非法名会被白名单先拦掉，那样测试就测不到路径这一层了。
	cases := []struct{ dir, entry string }{
		{"EvilUpper", "../../evil.lua"},
		{"EvilInner", "sub/../../../evil2.lua"},
		{"EvilAbs", "/tmp/evil3.lua"},
		{"EvilDrive", `C:\Windows\evil4.lua`},
	}
	for _, c := range cases {
		zipPath := makeZip(t,
			[2]string{"modinfo.lua", `name = "x"
`},
			[2]string{c.entry, "bad"},
		)
		if _, err := m.InstallFromZip(zipPath, c.dir); err == nil {
			t.Errorf("条目 %q 竟然被接受了", c.entry)
		}
		if _, err := os.Stat(filepath.Join(m.Paths.ModsRoot(), c.dir)); err == nil {
			t.Errorf("被拒的上传还是建了 mods/%s", c.dir)
		}
	}
	// 关键不是报了什么错，而是 mods/ 外面没被写过。
	for _, name := range []string{"evil.lua", "evil2.lua", "evil3.lua", "evil4.lua"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(m.Paths.ModsRoot()), name)); err == nil {
			t.Errorf("%s 真的被写到 mods/ 外面了", name)
		}
	}
	if left := tempLeftovers(t, m); len(left) != 0 {
		t.Errorf("被拒的上传在 mods/ 留下了东西: %v", left)
	}
}

func TestInstallFromZipRejectsSymlink(t *testing.T) {
	m, _ := newManager(t)
	zipPath := filepath.Join(t.TempDir(), "link.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	if _, err := w.Create("modinfo.lua"); err != nil {
		t.Fatal(err)
	}
	// 一个指向 /etc/passwd 的符号链接条目，内容写什么都无所谓，重点是模式位。
	// 注意这里必须传 fs.ModeSymlink：SetMode 吃的是 Go 的 FileMode，不是 unix 的 0o120000。
	hdr := &zip.FileHeader{Name: "sneaky"}
	hdr.SetMode(os.ModeSymlink | 0o777)
	fw, err := w.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("/etc/passwd")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.InstallFromZip(zipPath, "LinkMod"); err == nil {
		t.Fatal("符号链接条目应该被拒绝")
	}
	if left := tempLeftovers(t, m); len(left) != 0 {
		t.Errorf("拒绝后残留: %v", left)
	}
}

func TestInstallFromZipRefusesOverwriteAndBadNames(t *testing.T) {
	m, _ := newManager(t)
	good := makeZip(t, [2]string{"modinfo.lua", `name = "x"
`})
	if _, err := m.InstallFromZip(good, "KeepMe"); err != nil {
		t.Fatal(err)
	}
	// 原目录内容不能被第二次上传悄悄改掉。
	if _, err := m.InstallFromZip(good, "KeepMe"); err == nil {
		t.Error("同名目录已存在时应该拒绝")
	}
	body, err := os.ReadFile(filepath.Join(m.Paths.ModsRoot(), "KeepMe", "modinfo.lua"))
	if err != nil || !strings.Contains(string(body), `name = "x"`) {
		t.Errorf("已有模组内容被动了: %q %v", body, err)
	}
	// 助手模组目录是面板自己的通道，不能让上传覆盖。
	for _, bad := range []string{"", "..", ".", "DSTPanelHelper", "dstpanelhelper", ".hidden", "a/b", "with space", `back\slash`, strings.Repeat("x", 65), "-leading-dash"} {
		if _, err := m.InstallFromZip(good, bad); err == nil {
			t.Errorf("目录名 %q 应该被拒绝", bad)
		}
	}
}

func TestInstallFromZipRequiresModinfo(t *testing.T) {
	m, _ := newManager(t)
	// 没有 modinfo.lua 的包：服务端根本不会认，面板不能装完还说成功。
	zipPath := makeZip(t, [2]string{"modmain.lua", "print('hi')"})
	if _, err := m.InstallFromZip(zipPath, "NoModinfo"); err == nil {
		t.Error("缺 modinfo.lua 应该拒绝")
	}
	if _, err := os.Stat(filepath.Join(m.Paths.ModsRoot(), "NoModinfo")); err == nil {
		t.Error("被拒的模组目录留下来了")
	}
	// 只有一层顶层目录、modinfo 在里面：这是合法包，剥掉外壳后就是正常模组。
	nested := makeZip(t,
		[2]string{"outer/modinfo.lua", `name = "x"
`},
		[2]string{"outer/inner/modmain.lua", "x"},
	)
	target, err := m.InstallFromZip(nested, "OneLayer")
	if err != nil {
		t.Fatalf("套一层目录的合法包不该被拒: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "modinfo.lua")); err != nil {
		t.Errorf("顶层目录没剥掉: %v", err)
	}
	// 两个顶层目录就说不清哪个是根了，必须拒。
	ambig := makeZip(t,
		[2]string{"a/modinfo.lua", `name = "x"
`},
		[2]string{"b/modmain.lua", "x"},
	)
	if _, err := m.InstallFromZip(ambig, "Ambig"); err == nil {
		t.Error("两个顶层目录应该拒绝")
	}
	empty := makeZip(t)
	if _, err := m.InstallFromZip(empty, "EmptyZip"); err == nil {
		t.Error("空压缩包应该拒绝")
	}
}

func tempLeftovers(t *testing.T, m *Manager) []string {
	t.Helper()
	entries, err := os.ReadDir(m.Paths.ModsRoot())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			out = append(out, e.Name())
		}
	}
	return out
}
