package dst

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 引擎自己写过的文件可能是 BOM + CRLF，还可能留空行。
// 这些都不算内容，读进来只该剩 userid。
func TestReadPlayerListToleratesBOMAndCRLF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adminlist.txt")
	body := "\ufeffKU_aaa\r\n\r\n  KU_bbb  \r\nKU_aaa\r\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := ReadPlayerList(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "KU_aaa" || ids[1] != "KU_bbb" {
		t.Fatalf("去重/去空白后应只剩两个 ID，实际 %#v", ids)
	}
}

// 文件不存在不是错误：第一次启用面板时这三份名单本来就可能没有。
func TestReadPlayerListMissingFileIsEmpty(t *testing.T) {
	ids, err := ReadPlayerList(filepath.Join(t.TempDir(), "whitelist.txt"))
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("应返回空列表，实际 %#v", ids)
	}
}

func TestWritePlayerListRoundTripsAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocklist.txt")
	if err := WritePlayerList(path, []string{"KU_old"}); err != nil {
		t.Fatal(err)
	}
	if err := WritePlayerList(path, []string{"KU_old", "76561198000000000"}); err != nil {
		t.Fatal(err)
	}
	ids, err := ReadPlayerList(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[1] != "76561198000000000" {
		t.Fatalf("写回再读不一致: %#v", ids)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Errorf("名单末尾要留换行，实际 %q", raw)
	}
	// 改动前的那一版要留 .bak：名单写错人比改坏配置更难发现。
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("覆盖名单时没留 .bak: %v", err)
	}
}

func TestNormalizePlayerID(t *testing.T) {
	ok := map[string]string{
		"  KU_abc123  ":     "KU_abc123",
		"76561198000000000": "76561198000000000",
		"OU_9f3c":           "OU_9f3c",
	}
	for raw, want := range ok {
		got, err := NormalizePlayerID(raw)
		if err != nil || got != want {
			t.Errorf("NormalizePlayerID(%q) = %q, %v；期望 %q", raw, got, err, want)
		}
	}
	bad := []string{"", "   ", "KU_abc\nKU_evil", "KU_abc 123", "KU_abc\x00", strings.Repeat("a", 65)}
	for _, raw := range bad {
		if _, err := NormalizePlayerID(raw); err == nil {
			t.Errorf("%q 应该被拒绝", raw)
		}
	}
}

// kind 来自 URL，必须先挡在校验层；PlayerListFile 自己也再挡一次。
func TestPlayerListFileRejectsUnknownKind(t *testing.T) {
	p := Paths{KleiRoot: t.TempDir()}
	if _, ok := p.PlayerListFile("Cluster_1", "../../etc/passwd"); ok {
		t.Error("未知 kind 竟然拼出了路径")
	}
	if _, ok := p.PlayerListFile("Cluster_1", "adminlist.txt"); ok {
		t.Error("带扩展名的 kind 不该被当成合法种类")
	}
	path, ok := p.PlayerListFile("Cluster_1", PlayerListAdmin)
	if !ok || filepath.Base(path) != "adminlist.txt" {
		t.Errorf("管理员名单路径不对: %q, %v", path, ok)
	}
	if filepath.Dir(path) != p.ClusterDir("Cluster_1") {
		t.Errorf("名单要放在集群根目录，实际 %q", path)
	}
}
