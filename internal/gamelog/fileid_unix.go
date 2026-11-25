//go:build unix

package gamelog

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity 用 inode 认"这是不是同一份文件"。
//
// 引擎轮转日志有两种写法，光看 size 只能认出后一种：
//   - 把旧日志改名（归档进 backup/server_log/），再新建一个同名文件：
//     路径没变，size 可能更大也可能更小，但 inode 换了；
//   - 直接把同名文件截断重写：inode 没变，size 变短。
//
// Unix 上 Stat_t.Ino 就是那个身份。同一台机器盯着同一个路径，inode 够用；
// 拿到 Sys() 不是 Stat_t 时返回空串，由 ReadSince 退化为 size 判断，不谎报轮转。
// 用 fmt 打而不是 strconv：各 Unix 的 Ino 是 uint32/uint64 不一，%d 对两者都成立。
func fileIdentity(st os.FileInfo) string {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys == nil {
		return ""
	}
	return fmt.Sprintf("%d", sys.Ino)
}
