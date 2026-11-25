package modbridge

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"dstpanel/internal/dst"
)

// 助手 MOD 的文件直接打进二进制。
//
// 为什么不用"配置一个源目录，启动时拷过去"：那样发版要同时拷二进制和 mods 目录，
// 漏一个就是"面板升级了、助手还是旧的"，而 Lua 侧和 Go 侧的协议是一起改的，
// 版本错开的表现是分片静默不回话，排查方向很容易被带偏到网络上去。
// 打进二进制以后，二进制版本 == 助手版本，这一类问题不存在。
//
// 源文件在 internal/modbridge/mods/DSTPanelHelper：go:embed 不能跨出包目录（模式里不许有 ..），
// 所以模组本体跟着用它的那个包走。
//
//go:embed all:mods/DSTPanelHelper
var helperFS embed.FS

// helperEmbedRoot 是 embed.FS 里的路径前缀。
const helperEmbedRoot = "mods/DSTPanelHelper"

// HelperEmbeddedFiles 返回内嵌的全部文件（相对模组目录、正斜杠）及其 sha256 前 8 位。
// 给一致性校验的界面用："助手由面板自带，当前内嵌版本 abc12345"。
func HelperEmbeddedFiles() (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(helperFS, helperEmbedRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, err := helperFS.ReadFile(p)
		if err != nil {
			return fmt.Errorf("读内嵌文件 %s: %w", p, err)
		}
		rel := strings.TrimPrefix(p, helperEmbedRoot+"/")
		out[rel] = fmt.Sprintf("%x", sha256.Sum256(raw))[:8]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HelperSync 是内嵌版本与磁盘版本的对账结果。
//
// 四个桶必须分开：Missing 是"没装"，Differs 是"装过但旧了/被人改过"，
// Extra 是"目录里有不属于面板的文件"，Matched 才是"一致"。
// 只在 Missing 上判失败的话，用户改过一行 Lua 就会看到"已安装"，
// 然后困惑于为什么游戏里的行为和面板说得不一样。
type HelperSync struct {
	Missing []string // 内嵌有、磁盘没有
	Differs []string // 两边都有但内容不同
	Matched []string // 内容一致
	Extra   []string // 磁盘有、内嵌没有（不含 .bak / 临时文件）
	Written []string // 只有真正执行了写入时才有内容
}

// OK 表示磁盘与内嵌完全一致，且没有多余文件。
func (s HelperSync) OK() bool {
	return len(s.Missing) == 0 && len(s.Differs) == 0 && len(s.Extra) == 0
}

// IsLeftover 认面板自己留下的备份与临时文件——这些不算"用户多出来的文件"。
// 原子写每次覆盖都会产生 .bak，如果把它当 Extra，一致性校验永远不可能"干净"。
func IsLeftover(rel string) bool {
	base := path.Base(rel)
	return strings.HasSuffix(base, ".bak") ||
		strings.Contains(base, ".tmp-") ||
		strings.HasPrefix(base, ".")
}

// SyncHelper 对账助手 MOD 的文件；dryRun 为真时只查不写。
//
// 写入走 dst.WriteFileAtomic：内容一致的文件一律不动，
// 一是免得每次都留一堆 .bak，二是模组文件被游戏进程读着，
// 无意义的重写会白白给用户制造"这文件是不是被改过"的疑问。
func (b *Bridge) SyncHelper(dryRun bool) (HelperSync, error) {
	var sync HelperSync
	dir := b.Paths.HelperModDir()

	embedded := map[string][]byte{}
	err := fs.WalkDir(helperFS, helperEmbedRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, err := helperFS.ReadFile(p)
		if err != nil {
			return fmt.Errorf("读内嵌文件 %s: %w", p, err)
		}
		embedded[strings.TrimPrefix(p, helperEmbedRoot+"/")] = raw
		return nil
	})
	if err != nil {
		return sync, err
	}
	if len(embedded) == 0 {
		return sync, fmt.Errorf("二进制里没有内嵌的助手 MOD 文件，这个包不完整")
	}

	rels := make([]string, 0, len(embedded))
	for rel := range embedded {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		onDisk, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		switch {
		case os.IsNotExist(err):
			sync.Missing = append(sync.Missing, rel)
		case err != nil:
			return sync, fmt.Errorf("读磁盘上的助手文件 %s: %w", filepath.Join(dir, rel), err)
		case bytes.Equal(onDisk, embedded[rel]):
			sync.Matched = append(sync.Matched, rel)
		default:
			sync.Differs = append(sync.Differs, rel)
		}
	}

	// 反方向再走一遍磁盘：只在内嵌一侧比对的话，
	// 上一次布局改动留下的旧文件（比如曾经放在模组根目录的 panel_bridge.lua）永远看不见。
	if _, err := os.Stat(dir); err == nil {
		extra, err := collectExtra(dir, "", embedded)
		if err != nil {
			return sync, err
		}
		sync.Extra = append(sync.Extra, extra...)
	} else if !os.IsNotExist(err) {
		return sync, fmt.Errorf("读助手模组目录 %s: %w", dir, err)
	}
	sort.Strings(sync.Extra)

	if dryRun {
		return sync, nil
	}
	for _, rel := range append(append([]string{}, sync.Missing...), sync.Differs...) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := dst.WriteFileAtomic(full, embedded[rel], 0o644); err != nil {
			return sync, err
		}
		sync.Written = append(sync.Written, rel)
	}
	return sync, nil
}

// EnableHelper 一步做完"装文件"和"在这个分片开/关注手"。界面上的开关调的就是它。
//
// 顺序是有讲究的：先落文件，再写 modoverrides.lua。反过来的话，服务端可能在必经文件
// 还没到位时启动——引擎读到 enabled=true 就尝试加载，加载失败会走 DisableBecauseBad
// 把这个模组永久禁用（mods.lua:585），而 modoverrides.lua 里的条目还留着，
// 下次启动照样失败，用户看到的只是"我明明启用了却没有数据"。
func (b *Bridge) EnableHelper(cluster, shard string, enabled bool, s HelperSettings) (HelperSync, error) {
	sync, err := b.SyncHelper(false)
	if err != nil {
		return sync, err
	}
	if err := b.SetHelperOverride(cluster, shard, enabled, s); err != nil {
		return sync, err
	}
	return sync, nil
}

// collectExtra 递归找出磁盘上有、embedded 里没有的文件，返回相对 dir 的正斜杠路径。
func collectExtra(dir, prefix string, embedded map[string][]byte) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读目录 %s: %w", dir, err)
	}
	var extra []string
	for _, e := range entries {
		rel := path.Join(prefix, e.Name())
		if e.IsDir() {
			sub, err := collectExtra(filepath.Join(dir, e.Name()), rel, embedded)
			if err != nil {
				return nil, err
			}
			extra = append(extra, sub...)
			continue
		}
		if IsLeftover(rel) {
			continue
		}
		// 目录项也可能因为符号链接之类的原因不是普通文件，这里只按"有内容"处理。
		if _, ok := embedded[rel]; !ok {
			extra = append(extra, rel)
		}
	}
	return extra, nil
}
