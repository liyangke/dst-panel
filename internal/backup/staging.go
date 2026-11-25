package backup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// staging.go 管"用户上传的存档包在落地之后、变成快照之前待在哪"。
//
// 为什么不直接从请求体流进 Import：上传几十 MB 在这条链上要几十秒，
// 而导入的判断（读包目录、认形状、算大小）必须在用户点"确认导入"之前就把结论摆出来。
// 所以分成两步——先落地成一个临时包，预览它，用户点头了才解开。
// 临时包必须在面板重启后还能找到（用户传完就去点了别的页是常态），
// 但也必须会自己消失，否则每次试探性上传都在小机的盘上留一份 512MB。

const (
	// incomingDirName 是上传包的落脚目录，就在备份目录里面：同一个文件系统，
	// 改名才能是原子的，也省掉配置里再开一个"临时目录"给人配错。
	incomingDirName = "incoming"
	// unpackDirName 是解开包用的临时树，用完即删。和上传目录分开，
	// 因为两者的清理规则不同：临时目录要按时间收尾，解压树失败也要当场删干净。
	unpackDirName = "import-tmp"
	// stagedPrefix 是上传包编号的固定开头。编号会进任务参数、进界面文案，
	// 所以它必须一看就是面板生成的，而不是谁手工放进目录里的一个文件。
	stagedPrefix = "import"
	// writingSuffix 是"还在写"的后缀：写完了才改名成正式编号。
	// 没有这一步，一个传了一半被掐断的包会当成"用户上传好的存档"出现在预览里。
	writingSuffix = ".writing"
)

// StagedUploadTTL 是一份上传包在临时目录里能待多久，启动时和每次导入前各收一次。
//
// 之所以不需要人来清、也不做成配置项：用户传完没收尾（改主意、页面刷掉、面板重启）是常态，
// 而一份临时上传最多 512MB，在 4G 小机上是能把盘塞满的量。
// 6 小时给的是"传完先去别的页转一圈，回来还能直接点导入"的余地——再长就不是余地而是堆积。
const StagedUploadTTL = 6 * time.Hour

// stagedRefRegexp 是上传包编号的白名单形状。这个字符串来自请求参数，会参与拼路径，
// 所以按形状卡死而不是"看着没有 ../ 就放行"。
var stagedRefRegexp = regexp.MustCompile(
	`^import-[0-9]{8}T[0-9]{6}Z(-[0-9]{1,3})?-[A-Za-z0-9_-]{1,40}\.(zip|tar\.gz|tar)$`)

// checkStagedRef 校验一个上传包编号。
func checkStagedRef(ref string) error {
	if !stagedRefRegexp.MatchString(ref) {
		return fmt.Errorf("上传包的编号 %q 不是面板生成的格式（形如 import-20261102T071500Z-存档.zip），不按它去找文件", ref)
	}
	return nil
}

func (m *Manager) incomingDir() string { return filepath.Join(m.Dir, incomingDirName) }

// ErrStagedGone 是"这个上传包已经不在盘上了"这个结论的错误值。
//
// 接口层要按它挑状态码（包没了是 404 加一句"重新上传"，读不动是另一回事），
// 所以这个判断必须靠错误值而不是靠中文句子里的字样——
// 那句话以后改个标点就够让状态码悄悄跑偏，而没人会去测一个状态码。
var ErrStagedGone = errors.New("已经不在了")

// StagedPath 把一个上传包编号换成磁盘上的绝对路径，并确认文件真的在。
func (m *Manager) StagedPath(ref string) (string, error) {
	if err := checkStagedRef(ref); err != nil {
		return "", err
	}
	path := filepath.Join(m.incomingDir(), ref)
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("上传的存档包 %s %w：隔得久的临时上传会被面板自己清掉，重新上传一次", ref, ErrStagedGone)
		}
		return "", fmt.Errorf("读上传的存档包 %s: %w", ref, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s 是个目录而不是存档包，重新上传那个 .zip 或 .tar.gz 文件", ref)
	}
	if st.Size() == 0 {
		return "", fmt.Errorf("%s 是 0 字节：上传没走完就被中断了，重新上传一次", ref)
	}
	return path, nil
}

// WriteStaged 把请求体写成一个上传包，返回它的编号和落地的字节数。
//
// 名字是面板自己定的（上传时刻 + 原文件名去掉不安全的那部分 + 按包的实际字节认出的后缀），
// 三处都不能由客户端决定：客户端给的名字会带 ../、带盘符，或者干脆是个 .txt 改名的 zip。
// 后缀尤其要按 magic 决定，否则 listPackage 那句"认不出格式"要等到预览时才报，
// 而那时候一个根本没被认过的文件已经在盘上了。
func (m *Manager) WriteStaged(originalName string, r io.Reader) (string, int64, error) {
	if r == nil {
		return "", 0, fmt.Errorf("没有收到上传的内容")
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	head = head[:n]
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", 0, fmt.Errorf("读上传内容的开头: %w", err)
	}
	kind, err := sniffPkgKind(head, originalName)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(m.incomingDir(), 0o755); err != nil {
		return "", 0, fmt.Errorf("创建上传临时目录 %s: %w", m.incomingDir(), err)
	}
	ref, err := m.freeStagedRef(originalName, kind)
	if err != nil {
		return "", 0, err
	}
	tmpPath := filepath.Join(m.incomingDir(), ref+writingSuffix)
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", 0, fmt.Errorf("创建上传临时文件: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}
	written, err := io.Copy(f, io.MultiReader(bytes.NewReader(head), io.LimitReader(r, MaxImportArchiveBytes-int64(len(head))+1)))
	if err != nil {
		cleanup()
		return "", 0, fmt.Errorf("收下上传的存档包: %w", err)
	}
	if written > MaxImportArchiveBytes {
		cleanup()
		return "", 0, fmt.Errorf("上传的包超过 %s 的上限（收到 %s 就停了）：先确认这是不是一个真实存档，或者只压一个世界而不是整个 Klei 目录",
			humanBytes(MaxImportArchiveBytes), humanBytes(written))
	}
	if written == 0 {
		cleanup()
		return "", 0, fmt.Errorf("上传内容是空的")
	}
	if err := f.Chmod(0o644); err != nil {
		cleanup()
		return "", 0, fmt.Errorf("设置上传文件的权限: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, fmt.Errorf("写完上传文件: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(m.incomingDir(), ref)); err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, fmt.Errorf("把上传的临时文件换成正式编号: %w", err)
	}
	return ref, written, nil
}

// freeStagedRef 挑一个没被占用的上传编号。
// 同一秒传两次同名包是真会发生的（连点、或者传错了想马上重来），
// 而编号就是文件名——撞了就是把前一份覆盖掉，用户在预览里看到的是后一份。
func (m *Manager) freeStagedRef(originalName, kind string) (string, error) {
	stem := sanitizeForName(stemOf(originalName))
	if stem == "" || stem == "x" {
		stem = "package"
	}
	suffix := map[string]string{"zip": ".zip", "tar.gz": ".tar.gz", "tar": ".tar"}[kind]
	for i := 1; i <= 999; i++ {
		ref := fmt.Sprintf("%s-%s-%s%s", stagedPrefix, time.Now().UTC().Format("20060102T150405Z"), stem, suffix)
		if i > 1 {
			ref = fmt.Sprintf("%s-%s-%d-%s%s", stagedPrefix, time.Now().UTC().Format("20060102T150405Z"), i, stem, suffix)
		}
		if _, err := os.Stat(filepath.Join(m.incomingDir(), ref)); os.IsNotExist(err) {
			return ref, nil
		} else if err != nil {
			return "", fmt.Errorf("看上传目录: %w", err)
		}
	}
	return "", fmt.Errorf("上传目录里这一秒已经堆了 999 个同名包，先清一下 %s", incomingDirName)
}

// stemOf 取原文件名的主干（去掉后缀）。客户端给的后缀本来就不作数，
// 留主干只为了编号在界面和任务里读得出"这是传的那个存档"。
func stemOf(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	return name
}

// DropStaged 删掉一个上传包：导入成功后它的使命就结束了，留着占小机的盘。
// 文件不在时不算错误——重复调用（比如任务重试的收尾）不该报一次失败。
func (m *Manager) DropStaged(ref string) error {
	if err := checkStagedRef(ref); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(m.incomingDir(), ref)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除上传的存档包 %s: %w", ref, err)
	}
	return nil
}

// SweepStaged 清掉超过 maxAge 还没被用上的上传包（以及写了一半的 .writing）。
//
// 用户传完包却没收尾（改了主意、页面刷掉了、面板正好重启）是常态，
// 而一份临时上传最多 512MB，在 4G 小机上是能塞满盘的量。
// 这一步由调用方在启动时和每次导入前顺手做一次，不需要配置项：
// 临时上传不是成果，没有什么"要留久一点"的理由。
func (m *Manager) SweepStaged(maxAge time.Duration) []string {
	if maxAge <= 0 {
		return nil
	}
	entries, err := os.ReadDir(m.incomingDir())
	if err != nil {
		if !os.IsNotExist(err) {
			m.log().Warn("读上传临时目录失败，这次的清理没做成", "dir", m.incomingDir(), "err", err.Error())
		}
		return nil
	}
	var gone []string
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		// .writing 是没写完的半成品；认不出编号的是别人临时丢进这个目录的文件。
		// 后者也照时间收：这个目录是面板的临时目录，不是收件箱。
		if err := os.Remove(filepath.Join(m.incomingDir(), e.Name())); err != nil {
			m.log().Warn("清理过期的上传临时包失败", "name", e.Name(), "err", err.Error())
			continue
		}
		gone = append(gone, e.Name())
	}
	if len(gone) > 0 {
		m.log().Info("清理过期的上传临时包", "count", len(gone), "olderThan", maxAge.String())
	}
	return gone
}

// StagedUsage 是上传目录此刻占的名次与字节，给存档页那句"临时上传占了多少"用。
func (m *Manager) StagedUsage() (int, int64) {
	entries, err := os.ReadDir(m.incomingDir())
	if err != nil {
		return 0, 0
	}
	var count int
	var bytes int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		count++
		bytes += info.Size()
	}
	return count, bytes
}

// PruneUnpacked 收尾解压树。正常路径上 Import 用 defer 删自己的那次，
// 这里收的是"面板被 kill 在解压中间"留下的残骸（启动时调一次）。
func (m *Manager) PruneUnpacked() int {
	dir := filepath.Join(m.Dir, unpackDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var removed int
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			m.log().Warn("清理上次没删掉的解压目录失败", "name", e.Name(), "err", err.Error())
			continue
		}
		removed++
	}
	if removed > 0 {
		m.log().Info("清掉了上次没删完的解压目录", "count", removed, "dir", dir)
	}
	return removed
}
