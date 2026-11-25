package dst

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 文件层：读写都走原子替换并留下多版本 .bak。
// DST 的配置就是"改错一次世界就废了"的东西，面板必须保证：
// 要么完整写出新内容，要么保持原文件不动，且用户随时能拿回上一版。
//
// 备份不再是"一份 .bak 每次覆盖"：每次写之前都留一份带时间戳的独立副本，
// 最多保留 10 份，超出的按修改时间从旧到新清掉。
// 只有一份时，连点两次保存就会把"真正想回滚的那一版"冲成"和当前一样"的副本，
// 那正是回滚最没用的时刻。固定名 <file>.bak（旧版本面板留下的）照样认、照样列，
// 但只要写一次新格式备份，它就一起纳入 10 份的清理口径。

// LoadLua 解析 Lua 数据文件，同时返回原文，便于调用方判断"内容没变就别重写"。
func LoadLua(path string) (*LuaTable, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("读取 %s: %w", path, err)
	}
	text := string(raw)
	tbl, err := ParseLuaTable(text)
	if err != nil {
		return nil, text, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return tbl, text, nil
}

// SaveLua 按 Klei 风格渲染并原子写入，行尾沿用原文件的风格。
func SaveLua(path string, tbl *LuaTable) error {
	text := Render(tbl)
	if raw, err := os.ReadFile(path); err == nil && dominantEOL(string(raw)) == "\r\n" {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	return WriteFileAtomic(path, []byte(text), 0644)
}

func dominantEOL(text string) string {
	if strings.Contains(text, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// WriteFileAtomic 先写临时文件再 rename，中途崩溃不会留下半截配置。
// 覆盖前把旧内容存成 .bak（Windows 上 rename 不能覆盖已存在文件，退化成先删再改）。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm, true)
}

// WriteFileAtomicNoBackup 和 WriteFileAtomic 一样原子，但不留 .bak。
// 只给凭据文件用：轮换令牌的动机通常就是"旧的泄露了"，把上一串明文留在 .bak 里
// 等于只换了一半。
func WriteFileAtomicNoBackup(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm, false)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode, backup bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建目录 %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 成功 rename 后这句是空操作

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入 %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("设置权限 %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("落盘 %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭 %s: %w", tmpName, err)
	}

	if backup {
		if _, err := backupCurrent(path, perm); err != nil {
			return fmt.Errorf("备份 %s: %w", path, err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		// 只有"目标还在"才值得重试一次（Windows 上替换被占用文件会失败）；
		// 目标已消失说明是别的故障，别把配置删了还假装成功。
		if _, statErr := os.Stat(path); statErr != nil {
			return fmt.Errorf("替换 %s: %w", path, err)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("移除旧文件 %s: %w", path, err)
		}
		if err := os.Rename(tmpName, path); err != nil {
			return fmt.Errorf("二次替换 %s: %w", path, err)
		}
	}
	return nil
}

// ReadTextFile 统一读取入口，缺文件时给出可操作的中文错误。
func ReadTextFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s 不存在，可能需要先创建集群或重新生成世界", filepath.Base(path))
		}
		return "", fmt.Errorf("读取 %s: %w", path, err)
	}
	return string(raw), nil
}

// ---- 多版本备份 ----

const (
	// backupVersionSep 把"原文件名"和"版本时间戳"隔开。备份名形如
	// cluster.ini.bak-20261108T101530.123456789Z。
	backupVersionSep = ".bak-"
	// MaxBackupVersions 是一个文件最多留几份历史版本。10 份够覆盖"连着改了一下午"，
	// 又不会把小机的盘写满（配置文件都很小）。导出给接口层显示，免得两边各写一个数。
	MaxBackupVersions = 10
)

// BackupInfo 是一份历史备份的界面视图。
type BackupInfo struct {
	Name    string `json:"name"` // 文件名，不含目录
	Path    string `json:"path"` // 绝对路径
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"` // UTC RFC3339
}

// isBackupName 判断一个目录项是不是 path 的历史备份，同时兼做"名字合不合法"的闸：
// 回滚接口会把这个名字拼进路径，所以它只能是 base+".bak" 或 base+".bak-<安全后缀>"，
// 不许出现路径分隔符、.. 或别的文件的备份名。
func isBackupName(base, name string) bool {
	if name == base+".bak" {
		return true // 旧版本面板留下的单份备份，照样认
	}
	if !strings.HasPrefix(name, base+backupVersionSep) {
		return false
	}
	rest := strings.TrimPrefix(name, base+backupVersionSep)
	if rest == "" {
		return false
	}
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9', r == '-', r == 'T', r == 'Z', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
}

// backupCurrent 把 path 当前的内容另存成一份带时间戳的副本，返回新副本的文件名。
// 文件还不存在（第一次写）时不留备份，返回空串。写完顺手 pruneBackups 压回 10 份。
func backupCurrent(path string, perm os.FileMode) (string, error) {
	old, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("读取 %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	name := base + backupVersionSep + stamp
	target := filepath.Join(dir, name)
	// 同一纳秒里连写两次也要各自留一份：撞名就往后加序号，不覆盖上一份。
	for i := 2; ; i++ {
		_, statErr := os.Stat(target)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return "", fmt.Errorf("检查备份名 %s: %w", target, statErr)
		}
		name = fmt.Sprintf("%s%s%s-%d", base, backupVersionSep, stamp, i)
		target = filepath.Join(dir, name)
	}
	if err := os.WriteFile(target, old, perm); err != nil {
		return "", fmt.Errorf("写入备份 %s: %w", target, err)
	}
	// 固定名 <file>.bak 是"上一版"的镜像，每次同步刷新：部署脚本、旧版面板和
	// 用户习惯都在找这个名字，去掉它会让"改完配置想手动 cp 回来"的人扑空。
	// 它不占 10 份历史版本的名额（见 pruneBackups），避免被清理掉。
	fixed := filepath.Join(dir, base+".bak")
	if err := os.WriteFile(fixed, old, perm); err != nil {
		return "", fmt.Errorf("刷新上一版备份 %s: %w", fixed, err)
	}
	if err := pruneBackups(path); err != nil {
		return "", err
	}
	return name, nil
}

// pruneBackups 按修改时间保留最近 10 份，更旧的删掉。
// 用 ReadDir + 前缀判断而不是 filepath.Glob：文件名来自磁盘，逐项比对更直白，
// 也不会因为文件名里带 glob 元字符而误伤同目录里别的文件。
func pruneBackups(path string) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("读取备份目录 %s: %w", dir, err)
	}
	type version struct {
		name string
		at   time.Time
	}
	var versions []version
	for _, e := range entries {
		// 固定名是"上一版"的镜像，永远保留、不参与名额计数：清掉它等于
		// 让最近一次的改动没有 .bak，那正是最常被拿去回滚的一份。
		if e.IsDir() || e.Name() == base+".bak" || !isBackupName(base, e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // 刚被别人删掉的条目，跳过即可
		}
		versions = append(versions, version{e.Name(), info.ModTime()})
	}
	if len(versions) <= MaxBackupVersions {
		return nil
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].at.After(versions[j].at) })
	for _, v := range versions[MaxBackupVersions:] {
		if err := os.Remove(filepath.Join(dir, v.name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("清理旧备份 %s: %w", v.name, err)
		}
	}
	return nil
}

// ListBackups 列出 path 的全部历史备份，按时间倒序（新→旧）。
// 只包含本文件的备份，名字先过 isBackupName：目录里别人放的 .bak 不冒充它。
func ListBackups(path string) ([]BackupInfo, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil
		}
		return nil, fmt.Errorf("读取备份目录 %s: %w", dir, err)
	}
	out := make([]BackupInfo, 0, MaxBackupVersions)
	for _, e := range entries {
		// 固定名 .bak 和最新一份时间戳版内容相同，列表里只显示后者，
		// 免得用户对着两条一样的备份猜"该回滚哪一个"。
		if e.IsDir() || e.Name() == base+".bak" || !isBackupName(base, e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{
			Name: e.Name(), Path: filepath.Join(dir, e.Name()),
			Size: info.Size(), ModTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	return out, nil
}

// RollbackBackup 把某一份历史备份写回 path。
//
// 回滚前一定先给"当前文件"再做一次备份（backupCurrent），这样回滚本身也可逆：
// 用户发现选错了版本，还能从刚生成的那份拿回来。返回的第一个值就是这份
// 回滚前副本的名字，界面要把它说给用户听，别让它悄悄出现。
//
// name 是不含目录的备份文件名，来自 URL/请求体，必须先过 isBackupName 校验，
// 绝不拿它直接拼路径——否则 "../别的目录/文件" 就能读到面板根之外的内容。
func RollbackBackup(path, name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("备份名 %q 不合法：只能是文件所在目录里的一个名字，不能带路径", name)
	}
	base := filepath.Base(path)
	if !isBackupName(base, name) {
		return "", fmt.Errorf("%q 不是 %s 的历史备份", name, base)
	}
	src := filepath.Join(filepath.Dir(path), name)
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("备份 %s 不存在，先看一眼备份列表", name)
		}
		return "", fmt.Errorf("读取备份 %s: %w", name, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("备份 %s 是个目录，不是文件", name)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("读取备份 %s: %w", name, err)
	}
	perm := os.FileMode(0644)
	if cur, err := os.Stat(path); err == nil {
		perm = cur.Mode().Perm()
	}
	// 先备份当前版本，再原子写回。写回这一趟关掉 backup：当前版本已经另存过了，
	// 再触发一次备份只会多出一份和回滚结果无关的中间态。
	before, err := backupCurrent(path, perm)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, data, perm, false); err != nil {
		return "", fmt.Errorf("回滚写入 %s: %w", path, err)
	}
	return before, nil
}
