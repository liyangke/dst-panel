package mods

// 手工上传通道：把本地模组包解压进 mods/。
//
// 为什么这条通道不是可选项：服务端二进制里有 "Steam Workshop functionality will be
// disabled." 这条分支——Steam 初始化拿不到时工坊下载整体不可用，4h4g 这种裸机上
// 匿名登录失败并不罕见。届时清单文件登记得再对也下不来东西，唯一的装模组方式
// 就是让文件出现在 mods/ 下（modindex.lua:417 判的就是 MODS_ROOT..modname.."/modinfo.lua"）。

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"dstpanel/internal/dst"
)

const (
	// maxUploadBytes 限制解压后的总字节数。DST 模组普遍是几 MB 量级，
	// 64MB 已经够放带大图集的模组；上限是为了防 zip 炸弹，不是防正常用户。
	maxUploadBytes = 64 << 20
	maxEntryBytes  = 32 << 20
	maxZipEntries  = 5000
)

// modDirNameRe 是本地模组目录名的白名单。
// 目录名会同时成为 modoverrides.lua 里的键，也会被拼进 Lua 路径字符串，
// 所以这里必须是纯 ASCII 安全字符，不能放行任何引号、斜杠或控制字符。
var modDirNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$`)

// ValidModDirName 校验本地模组目录名。
func ValidModDirName(name string) error {
	if !modDirNameRe.MatchString(name) {
		return fmt.Errorf("模组目录名只能是字母、数字、下划线、点或连字符，且不能以点开头（收到 %q）", name)
	}
	if strings.EqualFold(name, dst.HelperModName) {
		return fmt.Errorf("%s 是面板助手模组的目录名，不能覆盖", name)
	}
	return nil
}

// InstallFromZip 把 zip 内容解压成 mods/<name>。
//
// 三条安全线：
//  1. 先落到同目录下的临时目录，全部校验通过才改名到位，中途失败不会留下半个模组；
//  2. 每条 zip 条目都单独判相对路径，绝不用 zip 里的名字直接 join 目标目录；
//  3. 已存在同名目录时直接拒绝，不覆盖——覆盖用户模组是不可逆的。
func (m *Manager) InstallFromZip(zipPath, name string) (string, error) {
	if err := ValidModDirName(name); err != nil {
		return "", err
	}
	modsRoot := m.Paths.ModsRoot()
	if err := os.MkdirAll(modsRoot, 0o755); err != nil {
		return "", fmt.Errorf("创建模组目录 %s: %w", modsRoot, err)
	}
	target := filepath.Join(modsRoot, name)
	if _, err := os.Stat(target); err == nil {
		return "", fmt.Errorf("mods/%s 已经存在，要先删掉它再上传（面板不做覆盖，避免不可逆）", name)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("检查 mods/%s: %w", name, err)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("这个文件不是合法的 zip：%w", err)
	}
	defer zr.Close()
	if len(zr.File) > maxZipEntries {
		return "", fmt.Errorf("压缩包里 %d 个条目，超过 %d 的上限，面板不接这种东西", len(zr.File), maxZipEntries)
	}

	prefix, err := stripPrefix(&zr.Reader)
	if err != nil {
		return "", err
	}

	tmp, err := os.MkdirTemp(modsRoot, ".upload-")
	if err != nil {
		return "", fmt.Errorf("在 mods/ 下建临时目录: %w", err)
	}
	// 成功改名后 cleanup 就是空操作；失败时它负责把半成品扫掉。
	defer os.RemoveAll(tmp)

	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// 符号链接与特殊文件：DST 模组不需要，留着就是逃逸目录的现成通道。
		if mode := f.Mode(); !mode.IsRegular() {
			return "", fmt.Errorf("压缩包里 %s 不是普通文件（%s），拒绝解压", f.Name, mode)
		}
		rel, err := entryRelPath(f.Name, prefix)
		if err != nil {
			return "", err
		}
		if int64(f.UncompressedSize64) > maxEntryBytes {
			return "", fmt.Errorf("%s 解压后超过 %d MB，单个文件太大", rel, maxEntryBytes>>20)
		}
		if err := extractOne(f, filepath.Join(tmp, rel), &total); err != nil {
			return "", err
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "modinfo.lua")); err != nil {
		return "", fmt.Errorf("解压结果里没有 modinfo.lua，服务端不会认它是模组")
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", fmt.Errorf("把解压结果放到 mods/%s: %w", name, err)
	}
	return target, nil
}

// extractOne 解压单条文件，边写边计数，超上限就报错而不是写满磁盘。
func extractOne(f *zip.File, dstPath string, total *int64) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("打开压缩包条目 %s: %w", f.Name, err)
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("建子目录 %s: %w", filepath.Dir(dstPath), err)
	}
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("写 %s: %w", dstPath, err)
	}
	// 上限按剩余配额算，所以 *total 只会在成功写完时增长。
	n, err := io.Copy(out, io.LimitReader(rc, maxEntryBytes+1))
	closeErr := out.Close()
	if err != nil {
		return fmt.Errorf("解压 %s: %w", f.Name, err)
	}
	if closeErr != nil {
		return fmt.Errorf("关闭 %s: %w", dstPath, closeErr)
	}
	if n > maxEntryBytes {
		return fmt.Errorf("%s 解压后超过 %d MB", f.Name, maxEntryBytes>>20)
	}
	*total += n
	if *total > maxUploadBytes {
		return fmt.Errorf("模组解压后总大小超过 %d MB，拒绝", maxUploadBytes>>20)
	}
	return nil
}

// stripPrefix 找出应该剥掉的一层顶层目录。
//
// 两种合法打包方式：根下直接是 modinfo.lua（论坛包常见），或者整包套一层模组名目录
// （压缩右键"压缩到文件夹"的结果）。混着来或者套两层都判为非法，
// 因为剥错层会得到一个"目录里又套一层"的假模组，服务端只会当它不存在。
func stripPrefix(zr *zip.Reader) (string, error) {
	hasRootModinfo := false
	topDirs := map[string]bool{}
	nestedModinfo := ""
	for _, f := range zr.File {
		name := strings.TrimSuffix(normZipName(f.Name), "/")
		if name == "" {
			continue
		}
		first, rest := splitFirst(name)
		if rest == "" {
			if strings.EqualFold(first, "modinfo.lua") {
				hasRootModinfo = true
			}
			continue
		}
		topDirs[first] = true
		if strings.EqualFold(rest, "modinfo.lua") {
			nestedModinfo = first
		}
	}
	if hasRootModinfo {
		return "", nil
	}
	if len(topDirs) == 1 && nestedModinfo != "" {
		return nestedModinfo + "/", nil
	}
	return "", fmt.Errorf("认不出模组根目录：modinfo.lua 要么在压缩包根下，要么在唯一的一层顶层目录里")
}

func splitFirst(name string) (first, rest string) {
	i := strings.IndexByte(name, '/')
	if i < 0 {
		return name, ""
	}
	return name[:i], name[i+1:]
}

// normZipName 把 zip 条目名统一成斜杠分隔。
// zip 规范用斜杠，但 Windows 打包工具会写反斜杠，两种都当分隔符处理。
func normZipName(name string) string {
	return strings.ReplaceAll(name, "\\", "/")
}

// entryRelPath 把 zip 里的路径变成相对目标目录的安全路径。
func entryRelPath(name, prefix string) (string, error) {
	rel := strings.TrimPrefix(normZipName(name), prefix)
	if rel == "" {
		return "", fmt.Errorf("压缩包条目 %s 剥掉前缀后是空的", name)
	}
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("压缩包条目 %s 想跑到模组目录外面，已拒绝", name)
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("压缩包条目 %s 路径非法", name)
	}
	// 盘符开头的名字在 Linux 上不会被 IsAbs 拦住，但它同样不是合法相对路径。
	if len(clean) >= 2 && clean[1] == ':' && isLetter(clean[0]) {
		return "", fmt.Errorf("压缩包条目 %s 带了 Windows 盘符，拒绝", name)
	}
	return filepath.FromSlash(clean), nil
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
