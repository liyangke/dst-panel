package dst

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLuaRoundTripAndBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "modoverrides.lua")
	tbl := NewTable()
	tbl.Set("workshop", String("workshop-1158324974"))
	if err := SaveLua(path, tbl); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Error("新建文件不该产生 .bak")
	}

	got, _, err := LoadLua(path)
	if err != nil {
		t.Fatalf("回读: %v", err)
	}
	if got.Get("workshop").AsString("") != "workshop-1158324974" {
		t.Errorf("回读内容不对: %+v", got)
	}

	tbl.Set("workshop", String("workshop-378160973"))
	if err := SaveLua(path, tbl); err != nil {
		t.Fatalf("二次写入: %v", err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("读 .bak: %v", err)
	}
	if !strings.Contains(string(bak), "workshop-1158324974") {
		t.Errorf(".bak 里不是上一版: %s", bak)
	}
	// 不能有临时文件残留
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}
}

func TestSaveLuaPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leveldataoverride.lua")
	// 先造一个 CRLF 的原始文件（Klei 在 Windows 上生成的文件就是这种）。
	crlf := "return {\r\n\ta = \"b\",\r\n}\r\n"
	if err := os.WriteFile(path, []byte(crlf), 0644); err != nil {
		t.Fatal(err)
	}
	tbl, _, err := LoadLua(path)
	if err != nil {
		t.Fatalf("解析 CRLF: %v", err)
	}
	if err := SaveLua(path, tbl); err != nil {
		t.Fatalf("写回: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\r\n") {
		t.Errorf("行尾风格被改成了 LF:\n%s", raw)
	}
}

func TestWriteFileAtomicCreatesDirsAndKeepsOldOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "cluster.ini")
	if err := WriteFileAtomic(path, []byte("[MISC]\na = 1\n"), 0644); err != nil {
		t.Fatalf("写入: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[MISC]\na = 1\n" {
		t.Errorf("内容不对: %q", raw)
	}

	// 目标是"非空目录"时 rename 必然失败，此时旧内容必须一个字都没动。
	blocker := filepath.Join(dir, "blocker")
	if err := os.Mkdir(blocker, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "x"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(blocker, "x")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(blocker, []byte("boom"), 0644); err == nil {
		t.Fatal("覆盖目录应该失败")
	}
	if _, err := os.Stat(filepath.Join(blocker, "x")); err != nil {
		t.Errorf("失败的路径把原有目录删掉了: %v", err)
	}
}

// 凭据文件不留上一版：轮换令牌的动机通常就是"旧的泄露了"，
// 把旧明文留在 .bak 里等于只换了一半。
func TestWriteFileAtomicNoBackupLeavesNoCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster_token.txt")
	if err := WriteFileAtomicNoBackup(path, []byte("first\n"), 0600); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	if err := WriteFileAtomicNoBackup(path, []byte("second\n"), 0600); err != nil {
		t.Fatalf("二次写入: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "second\n" {
		t.Errorf("内容没换掉: %q", raw)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("留下了一份带旧值的 .bak: %v", err)
	}
}

func TestLoadLuaMissingAndBroken(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadLua(filepath.Join(dir, "nope.lua")); err == nil ||
		!strings.Contains(err.Error(), "nope.lua") {
		t.Errorf("缺文件应报可定位的错: %v", err)
	}

	bad := filepath.Join(dir, "bad.lua")
	if err := os.WriteFile(bad, []byte("return { a = GlobalVar }"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadLua(bad); err == nil || !strings.Contains(err.Error(), "bad.lua") {
		t.Errorf("坏文件应在错误里带文件名: %v", err)
	}
}

func TestReadTextFileFriendlyError(t *testing.T) {
	msg, err := ReadTextFile(filepath.Join(t.TempDir(), "cluster.ini"))
	if err == nil || msg != "" {
		t.Fatalf("应返回错误: %v", err)
	}
	if !strings.Contains(err.Error(), "cluster.ini") || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误文案不友好: %v", err)
	}
}
