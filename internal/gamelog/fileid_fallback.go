//go:build !unix

package gamelog

import "os"

// fileIdentity 是没有 Unix inode 可用时（含 windows/amd64）的兜底。
//
// 这里刻意返回空串，而不是拼 size+mtime：日志每追加一行，size 和 mtime 都会变，
// 把它们当"文件身份"会让 ReadSince 把每一次正常追加都误判成轮转，SSE 就会不停
// 发 reset、界面上刚出来的行反复被清空。这比"少认出一种轮转"严重得多。
//
// 真正的身份判定走 Reader.ReadSince 里的 os.SameFile(since.File, st)：
// Windows 上 os.SameFile 用 volume+file index，Unix 上用 dev+ino，两者都是
// 文件创建时就固定、追加内容不会变的属性。跨进程/断线重连拿不到 os.FileInfo
// 时，退化为"offset 比文件还靠后"这条老判据，并在 SSE 的 meta 里说明。
func fileIdentity(st os.FileInfo) string {
	return ""
}
