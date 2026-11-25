package mods

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstpanel/internal/dst"
)

// 删除这一步是不可逆的，所以测试盯的不是"删得掉"，而是"什么情况下必须删不掉"。

func TestRemoveFilesFreesTheDirAndReportsSize(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, "workshop-351210508", `name = "更多物品堆叠"`)
	full := filepath.Join(m.Paths.ModsRoot(), "workshop-351210508")
	// 模组目录里再套一层，验证是整棵子树都没了、大小也算了子文件。
	if err := os.MkdirAll(filepath.Join(full, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "images", "icon.tex"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	writeModinfo(t, m, "keepme", `name = "留着"`)

	size, err := m.RemoveFiles("workshop-351210508")
	if err != nil {
		t.Fatalf("RemoveFiles: %v", err)
	}
	if !size.ModsDir {
		t.Errorf("mods/ 下面那一份要报出来，界面那句删掉了哪里全靠它：%+v", size)
	}
	if size.Freed < 4096 {
		t.Errorf("释放字节数 = %d，子目录里的 4096 字节没算进来", size.Freed)
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Errorf("目录还在：%v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Paths.ModsRoot(), "keepme")); err != nil {
		t.Errorf("旁边的模组被误删了：%v", err)
	}
}

// 引擎自己拉的工坊内容不在 mods/ 下，而是在按房间按世界各一份的下载区里。
// 删除这一步必须把它一起清掉，否则"腾出多少磁盘"那句话是少说了的，
// 而那句正是 4h4g 这台机器上用户最关心的数。
func TestRemoveFilesAlsoClearsTheEnginesDownloadArea(t *testing.T) {
	m, _ := newManager(t)
	// 同一个模组在地表和洞穴各有一份，外加一个别的模组和另一个房间的一份。
	a := writeUgcCopy(t, m, "Cluster_1", "Master", "351210508", 2048)
	b := writeUgcCopy(t, m, "Cluster_1", "Caves", "351210508", 1024)
	other := writeUgcCopy(t, m, "Cluster_1", "Master", "999", 512)
	otherCluster := writeUgcCopy(t, m, "Cluster_2", "Master", "351210508", 256)

	got, err := m.RemoveFiles("workshop-351210508")
	if err != nil {
		t.Fatalf("RemoveFiles: %v", err)
	}
	if got.ModsDir {
		t.Errorf("mods/ 下面本来没有这一份，不该说删了它：%+v", got)
	}
	if len(got.UGCShards) != 3 {
		t.Errorf("三份副本都要删（两个世界 + 另一个房间那一份），实际 %+v", got.UGCShards)
	}
	// 每一份目录里还有一 modinfo.lua（十个字节），那也算释放量，所以这里只卡
	// "三份内容加起来"并且"别的模组那 512 字节没混进来"。
	if got.Freed < 3328 || got.Freed > 3328+64 {
		t.Errorf("释放字节 = %d，要 3328 上下（别的模组那 512 不算）", got.Freed)
	}
	for _, p := range []string{a, b, otherCluster} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("下载区里的那份还在：%s", p)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("旁边的别的模组被误删了：%v", err)
	}
	// mods/ 与下载区两处都有的那种（手工放过一份、引擎又拉了一份）要一起没。
	writeModinfo(t, m, "workshop-123", `name = "两处都有"`)
	both := writeUgcCopy(t, m, "Cluster_1", "Master", "123", 64)
	res, err := m.RemoveFiles("workshop-123")
	if err != nil {
		t.Fatalf("RemoveFiles 两处都有: %v", err)
	}
	if !res.ModsDir || len(res.UGCShards) != 1 {
		t.Errorf("两处都要报出来：%+v", res)
	}
	if _, err := os.Stat(both); !os.IsNotExist(err) {
		t.Errorf("下载区那一份没跟着删：%v", err)
	}
}

// writeUgcCopy 在引擎下载区里造一份模组内容（一个 modinfo.lua 加一个指定大小的文件），
// 返回那个目录的路径。
func writeUgcCopy(t *testing.T, m *Manager, cluster, shard, id string, size int) string {
	t.Helper()
	dir := m.Paths.UgcModDir(cluster, shard, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "modinfo.lua"),
		[]byte(`name = "x"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if err := os.WriteFile(filepath.Join(dir, "modmain.lua"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRemoveFilesRefusesWhatItMustNotTouch(t *testing.T) {
	m, _ := newManager(t)
	writeModinfo(t, m, dst.HelperModName, `name = "面板助手"`)
	writeModinfo(t, m, "uploaded_ok", `name = "上传的模组"`)
	if err := os.WriteFile(filepath.Join(m.Paths.ModsRoot(), "loose.lua"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, dir, want string }{
		// 助手模组删掉等于把回传通道拆了：面板自己会读不到世界状态，而且用户无从恢复。
		{"面板助手目录", dst.HelperModName, "助手"},
		{"穿越路径", "../dst", "目录名"},
		{"绝对路径", "/etc", "目录名"},
		{"空名", "", "目录名"},
		{"点开头", ".upload-123", "目录名"},
		{"没装过的模组", "workshop-999999999", "不存在"},
		{"mods 下的散文件", "loose.lua", "不是目录"},
	} {
		if _, err := m.RemoveFiles(tc.dir); err == nil {
			t.Errorf("%s 应该被拒绝，实际删掉了", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s 的错误要提到 %s，实际 %q", tc.name, tc.want, err.Error())
		}
	}
	// 拒绝的那几个必须一个都没少。
	for _, dir := range []string{dst.HelperModName, "uploaded_ok"} {
		if _, err := os.Stat(filepath.Join(m.Paths.ModsRoot(), dir)); err != nil {
			t.Errorf("%s 不该被动过：%v", dir, err)
		}
	}
}
