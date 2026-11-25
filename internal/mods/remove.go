package mods

// 删除已下载/已上传的模组目录。
//
// 为什么这条通道必须存在：登记（往清单里写一行）和装（文件落到磁盘上）是两件事，
// 面板两样都会做，却一直没有反向操作——装错的模组删不掉，磁盘只进不出。
// 4h4g 这台机器的内存和磁盘都是硬约束，几个装错的工坊模组就是几百 MB 永久占着，
// 而"手工去 mods/ 下删目录"要求用户知道哪个目录对应哪串 ID（他正是因为对不上才来问的）。
//
// 这一步不可逆，所以判定一次做严，不给"看起来像模组就删"留空间：
// 目录名走和上传同一份白名单（顺手挡掉了面板助手模组的目录名），
// 路径必须仍在 mods/ 里面，而且必须是个目录。
//
// 工坊模组要删两处：mods/ 下面那一份（手工放置的旧形态）和引擎自己那份下载区里的副本
// （ugc_mods/<房间>/<世界>/content/322330/<id>，一个世界一份）。
// 删下载区那一份是安全的，2026-11-03 在云机实测过：把内容目录挪走后重启那个世界，
// 工坊日志打 "Loaded workshop items in … (1 installed, 1 needed)" 并重新下载
// （Download item … result : OK），模组照常 Loading mod。也就是说那份台账不会挡住重拉，
// "删文件重下"这句话对下载区里的副本同样成立。

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Removal 是一次删除的结果，界面上那句"删掉了什么、腾出多少"按它写。
type Removal struct {
	// Freed 是删掉的所有文件加起来的大小（不含目录本身）。
	Freed int64
	// ModsDir 为真表示 mods/ 下面那一份被删掉了。
	ModsDir bool
	// UGCShards 是引擎下载区里被清掉的那几份，写的是"房间 / 世界"。
	// 一个模组在每个世界各有一份，所以这里是一串而不是一个。
	UGCShards []string
}

// RemoveFiles 删掉这个模组目录在这台机器上的全部副本。
// 一处都没有时返回错误而不是静默成功：界面要把"没删掉任何东西"如实说出来，
// 否则用户以为清理过了，磁盘其实一点没动。
func (m *Manager) RemoveFiles(dir string) (Removal, error) {
	var out Removal
	if err := ValidModDirName(dir); err != nil {
		return out, err
	}
	root := filepath.Clean(m.Paths.ModsRoot())
	target := filepath.Join(root, dir)
	// 白名单已经排掉了斜杠和点开头，这一层是防以后有人放宽白名单时顺手打开逃逸口。
	if filepath.Dir(target) != root {
		return out, fmt.Errorf("要删的 mods/%s 不在 %s 里面，面板不做这个删除", dir, root)
	}
	st, err := os.Stat(target)
	switch {
	case err == nil && !st.IsDir():
		return out, fmt.Errorf("mods/%s 不是目录，面板只删模组目录", dir)
	case err == nil:
		size, err := dirSize(target)
		if err != nil {
			// 统计失败不拦删除（那多半是一个读不动的子项），但要说清释放量是不知道的。
			size = 0
		}
		if err := os.RemoveAll(target); err != nil {
			return out, fmt.Errorf("删除 mods/%s: %w", dir, err)
		}
		out.Freed += size
		out.ModsDir = true
	case !os.IsNotExist(err):
		return out, fmt.Errorf("检查 mods/%s: %w", dir, err)
	}

	// 工坊模组另有一份在引擎自己的下载区里，每个世界一份，一起清掉才算把话说完。
	if id := WorkshopIDFromDir(dir); id != "" {
		for _, cp := range m.ugcCopies() {
			if cp.ID != id {
				continue
			}
			size, _ := dirSize(cp.Path)
			if err := os.RemoveAll(cp.Path); err != nil {
				return out, fmt.Errorf("删除引擎下载区里的那一份（%s）: %w", cp.Label(), err)
			}
			out.Freed += size
			out.UGCShards = append(out.UGCShards, cp.Label())
		}
	}

	if !out.ModsDir && len(out.UGCShards) == 0 {
		return out, fmt.Errorf("mods/%s 本来就不存在，引擎的下载区里也没有这一条，没有删掉任何东西", dir)
	}
	return out, nil
}

// dirSize 累加目录里所有文件的大小，给"这次腾出了多少空间"那句话用。
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
