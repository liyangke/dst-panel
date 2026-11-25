//go:build !unix

package api

import "fmt"

// dirFreeBytes 在非类 Unix 系统上读不到分区剩余空间（开发机上用）。
// 返回错误而不是硬凑一个数字：存档页拿到错误就只显示"已用多少"，
// 那句"磁盘还剩"宁可写"未知"，也不能让面板在 4G 小机上报一个乐观值。
func dirFreeBytes(dir string) (int64, error) {
	return 0, fmt.Errorf("%s 所在分区的剩余空间在这个系统上读不到（只在 Linux/macOS 实现了 statfs）", dir)
}
