//go:build unix

package api

import (
	"fmt"
	"syscall"
)

// dirFreeBytes 返回目录所在分区的可用字节数。
//
// 用 Bavail 而不是 Bfree：Bfree 把文件系统的预留块也算进去了，
// 普通用户实际写不进去的那部分会被报成"还剩这些"，于是"够不够放一份快照"的结论偏乐观。
func dirFreeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("读 %s 所在分区的剩余空间: %w", dir, err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
