// 文件层：有界地读日志尾巴，以及从上次的位置续读。
//
// 这两件事都不能"整份读进来再切"。dst-cloud 上的 Master 日志跑了 6 小时就到 73KB，
// 按这个速度开一周就是几百 KB 起步、挂上模组和 Lua 报错能到几十 MB，
// 而日志页是会被反复刷、SSE 会每秒轮询的接口——整份读等于把内存和磁盘都交出去。
// 所以 tail 只从文件末尾按块往前翻到够数为止，follow 只读新增的那一段。
package gamelog

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dstpanel/internal/dst"
)

const (
	// DefaultLines 是不传 lines 时的窗口大小。200 行的依据是实测噪声构成：
	// 引擎每 30 秒一次账户心跳、每 8 分钟一次存档，200 行大约回看 20~30 分钟，
	// 够看清"刚才那一下发生了什么"，又不至于把界面撑满。
	DefaultLines = 200
	// MaxLines 是窗口上限。再大就该去 shell 里 grep 了，不是日志页的事。
	MaxLines = 2000

	// chunkBytes 是往回翻时每块读的字节数。
	chunkBytes = 64 * 1024
	// maxTailBytes 是 tail 的总上限：窗口内的行平均几百字节，2000 行也才 1MB 多点，
	// 撞到上限说明有人在日志里刷超长行，这时宁可少给几行也不能无限吃内存。
	maxTailBytes = 8 * 1024 * 1024
	// maxFollowBytes 是单次续读的上限。超出的部分留到下一轮，不丢——
	// 一次世界生成失败的堆栈能连打几百行，一次性推给 SSE 只会把连接压垮。
	maxFollowBytes = 512 * 1024
	// headBytes 是文件头窗口：`Current time:` 与 `Version:` 都在前 20 行内。
	headBytes = 4096
)

// currentTimePrefix 与 versionPrefix 是引擎启动流水里的两行。
// 前者给出本轮启动的绝对时间（本地时区），后者给版本号，界面头部要用。
const (
	currentTimePrefix = "Current time: "
	versionPrefix     = "Version: "
)

// Reader 是有界读日志的执行者。它不记状态：续读位置由调用方拿着（见 Since），
// 这样一次 HTTP 请求里读到的偏移不会残留在面板进程里。
type Reader struct {
	Paths dst.Paths
}

func NewReader(p dst.Paths) *Reader { return &Reader{Paths: p} }

// Since 是续读位置：读到哪了，以及本轮启动的墙上时间。
// 墙上时间必须跟着一起传——引擎的时钟是相对值，换了起始时间之后所有行都要重算，
// 只传偏移量的话第二批记录的 At 会带着上一轮的偏移。
type Since struct {
	Offset int64
	Start  time.Time
	// FileID 是上次读的那份文件的身份（Unix 上是 inode；见 fileid_*.go）。
	// 只有同一份文件才谈得上"从 offset 续读"：轮转时引擎会把旧日志改名再新建同名
	// 文件，路径没变、size 也可能更大，光比 size 认不出来，得靠这个身份。
	// 空串表示调用方没给身份（例如 SSE 断线重连只带 Last-Event-ID 一个 offset），
	// 那种情况退化为原来的"文件变短"判断，不把"没给身份"误报成轮转。
	FileID string
	// File 是进程内的真实文件身份。跨请求（SSE 断线重连只带一个 offset）时拿不到，
	// 但同一条流里每一轮都能拿到，用 os.SameFile 比较：Windows 上它走文件的
	// volume+index，Unix 上走 dev+ino，两者都不会因为"日志又追加了一行"而改变。
	// 这是轮转判定的主力；FileID 只是给跨请求重连的一个补充线索。
	File os.FileInfo
}

// Snapshot 是一次读取的结果。
type Snapshot struct {
	Path    string
	Kind    Kind
	Size    int64
	ModTime time.Time
	// FileID 是这一次读到的文件身份，随 Since() 一起传给下一轮 ReadSince，
	// 用来认"改名后新建同名文件"那种轮转（见 ReadSince）。
	FileID string
	// File 是这一轮读到的文件身份，随 Since() 传给下一轮做 os.SameFile 比对。
	File os.FileInfo

	// Start 是本轮启动的墙上时间，零值表示这份日志里没有 Current time 行
	// （被截断过、或者是一份手工拷来的文件）。这时每条记录都不带 At。
	Start time.Time
	// Version 是引擎版本，读不到为空。
	Version string

	// Next 是下一次续读该传的位置（始终落在行首）。
	Next int64
	// Restarted 只在续读时可能为 true：文件比上次的位置短，说明引擎截断重写了
	// （专用服务端每轮启动都会重写这两份日志）。
	Restarted bool
	// Older 为 true 表示窗口之前还有更早的行没给出去。
	Older bool
	// Blanks 是窗口里被丢掉的空行数（引擎会打不少空白行）。
	Blanks int

	Entries []Entry
}

func (s *Snapshot) Since() Since {
	return Since{Offset: s.Next, Start: s.Start, FileID: s.FileID, File: s.File}
}

// Tail 读该分片日志的末尾 lines 行。
func (r *Reader) Tail(cluster, shard string, kind Kind, lines int) (*Snapshot, error) {
	if lines <= 0 {
		lines = DefaultLines
	}
	if lines > MaxLines {
		lines = MaxLines
	}
	path, err := r.path(cluster, shard, kind)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取日志 %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("读取日志 %s: %w", path, err)
	}
	snap := &Snapshot{Path: path, Kind: kind, Size: st.Size(), ModTime: st.ModTime(),
		FileID: fileIdentity(st), File: st}
	snap.fillHeader(f)

	data, base := readTailBytes(f, st.Size(), lines)
	// base > 0 说明这块数据是从任意字节位置开始的，第一行大概率是半行；
	// 从文件头开始读时第一行是完整的，丢了它就等于每份日志的第一行永远看不到
	// （引擎那句 PersistRootStorage 恰好是最该看到的一行）。
	entries, blanks, next := consumeLines(kind, data, base, base > 0)
	// 窗口内的条数可能比 lines 多一两行（往回翻是按 64KB 整块取的），裁掉多出来的。
	trimmed := false
	if len(entries) > lines {
		entries = entries[len(entries)-lines:]
		trimmed = true
	}
	snap.Next = next
	snap.Blanks = blanks
	// base > 0 就已经说明更早的内容没读进来，与裁不裁条数无关。
	snap.Older = trimmed || base > 0
	snap.Entries = entries
	stampAt(entries, snap.Start)
	return snap, nil
}

// ReadSince 从上次的位置往后读。没有新内容时返回空的 Snapshot（不是错误）——
// SSE 轮询九成九走的是这条路，把它做成错误会让调用方每秒钟记一条无意义的告警。
func (r *Reader) ReadSince(cluster, shard string, kind Kind, since Since) (*Snapshot, error) {
	path, err := r.path(cluster, shard, kind)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取日志 %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("读取日志 %s: %w", path, err)
	}
	curID := fileIdentity(st)
	snap := &Snapshot{Path: path, Kind: kind, Size: st.Size(), ModTime: st.ModTime(),
		Start: since.Start, FileID: curID, File: st}
	if since.Offset < 0 {
		since.Offset = 0
	}

	// 轮转的两种形态都要认出来，缺一不可：
	//  1) 文件身份变了：旧日志被改名归档、再新建一个同名文件。路径没变，size
	//     可能更大也可能更小，"文件变短"这一条认不出它，得靠 inode/FileID。
	//  2) 同一份文件被截断重写：身份没变，但 size 比上次位置短（引擎每轮启动
	//     都会重写这两份日志），这一条是老逻辑，留着兼容。
	// 身份为空（调用方没给，例如 SSE 只带 Last-Event-ID 重连；或这版系统拿不到
	// inode）时跳过第 1 条，只按第 2 条判：宁可少报一次轮转，也不能把"没给身份"
	// 当成"文件换了"去清掉时间基准。
	// 注意 FileID 只作补充：拿不到真实身份的系统上它是空串，此时一律跳过，
	// 绝不能用 size+mtime 之类的"会随追加变化"的值冒充身份——那会让每一条新日志
	// 都被判成轮转，SSE 不停发 reset、界面上的行反复被清掉。
	replacedByID := since.FileID != "" && curID != "" && since.FileID != curID
	replacedByFile := since.File != nil && !os.SameFile(since.File, st)
	if replacedByID || replacedByFile || since.Offset > st.Size() {
		snap.Restarted = true
		since.Offset = 0
		// 连本轮起始时间一起丢掉：上一轮的 Current time 对新内容没有任何意义，
		// 留着它会把新第一轮的时间算成旧时间。
		snap.Start = time.Time{}
	}
	// 手里没有时间就去文件头再认一次。新一轮的 Current time 行通常还没打出来，
	// 这时维持零值，宁可界面不显示时间也不能显示错的时间。
	if snap.Start.IsZero() {
		snap.fillHeader(f)
	}
	if since.Offset == st.Size() {
		snap.Next = since.Offset
		return snap, nil
	}

	want := st.Size() - since.Offset
	if want > maxFollowBytes {
		want = maxFollowBytes
	}
	data, err := readAt(f, since.Offset, int(want))
	if err != nil {
		return nil, fmt.Errorf("读取日志 %s: %w", path, err)
	}
	entries, blanks, next := consumeLines(kind, data, since.Offset, false)
	snap.Entries = entries
	snap.Blanks = blanks
	snap.Next = next
	stampAt(entries, snap.Start)
	return snap, nil
}

// fillHeader 从文件头取本轮启动时间与版本号。读不到不报错：
// 这两样只是界面头部的便利信息，缺了不影响逐行解析。
func (s *Snapshot) fillHeader(f *os.File) {
	if s.Size == 0 {
		return
	}
	n := s.Size
	if n > headBytes {
		n = headBytes
	}
	head, err := readAt(f, 0, int(n))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(head), "\n") {
		line = strings.TrimRight(line, "\r ")
		if s.Start.IsZero() {
			if i := strings.Index(line, currentTimePrefix); i >= 0 {
				// 引擎打的是本机本地时间（实测 ANSIC 形态：Sat Oct 31 02:17:35 2026），
				// 按本地时区解才能得到正确的绝对时刻；界面一律收 UTC 的 RFC3339。
				if t, err := time.ParseInLocation(time.ANSIC, line[i+len(currentTimePrefix):], time.Local); err == nil {
					s.Start = t
				}
			}
		}
		if s.Version == "" {
			if i := strings.Index(line, versionPrefix); i >= 0 {
				if v := strings.TrimSpace(line[i+len(versionPrefix):]); v != "" {
					s.Version = v
				}
			}
		}
		if !s.Start.IsZero() && s.Version != "" {
			return
		}
	}
}

// path 拼出日志路径。这里再校验一次名字，不依赖调用方做过 shardScope：
// 这两个值来自 URL 路径段，拼错一次就是读别人目录下的文件。
func (r *Reader) path(cluster, shard string, kind Kind) (string, error) {
	if err := checkSegment(cluster); err != nil {
		return "", err
	}
	if err := checkSegment(shard); err != nil {
		return "", err
	}
	switch kind {
	case KindChat:
		return r.Paths.ChatLog(cluster, shard), nil
	case KindServer, "":
		return r.Paths.ServerLog(cluster, shard), nil
	default:
		return "", fmt.Errorf("不认识的日志种类 %q，只有 server 和 chat", string(kind))
	}
}

func checkSegment(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("非法的名字片段 %q：只能是目录名本身，不能带路径", name)
	}
	return nil
}

// readTailBytes 从文件末尾往回翻，直到拿够 lines 行或者翻到文件头。
// 返回的 base 是这块数据的起始字节偏移（可能是半行的中间，由 consumeLines 负责丢）。
func readTailBytes(f *os.File, size int64, lines int) ([]byte, int64) {
	if size == 0 {
		return nil, 0
	}
	var chunks [][]byte
	var off, total int64 = size, 0
	newlines := 0
	for off > 0 && total < maxTailBytes {
		n := chunkBytes
		if int64(n) > off {
			n = int(off)
		}
		if int64(n) > maxTailBytes-total {
			n = int(maxTailBytes - total)
		}
		data, err := readAt(f, off-int64(n), n)
		if err != nil {
			break
		}
		off -= int64(n)
		newlines += bytes.Count(data, []byte("\n"))
		chunks = append([][]byte{data}, chunks...)
		total += int64(n)
		// 多要一行：第一行大概率是被切断的半行，得留给 consumeLines 丢掉。
		if newlines >= lines+1 {
			break
		}
	}
	return bytes.Join(chunks, nil), off
}

// consumeLines 把从 base 开始的一块字节切成整行并逐行解析。
//
// dropFirst 只在该块数据不是从行首开始时传 true（tail 的窗口起点是任意字节位置）。
// 续读永远传 false：next 保证落在行首。
//
// 另一处刻意的取舍：结尾没有换行的那段不消费，返回的 next 停在它开头。
// 引擎是一行一行写的，半行意味着还在写；等下一轮它自然完整。这样 next 始终落在行首，
// 续读不会把一行拆成两条记录。
func consumeLines(kind Kind, data []byte, base int64, dropFirst bool) ([]Entry, int, int64) {
	if len(data) == 0 {
		return nil, 0, base
	}
	if dropFirst {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil, 0, base
		}
		base += int64(i + 1)
		data = data[i+1:]
	}

	var entries []Entry
	blanks := 0
	pos := base
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		var line []byte
		if i < 0 {
			// 尾下的半行：留给下一轮，位置不前进。
			break
		}
		line, data = data[:i], data[i+1:]
		if e, ok := ParseLine(kind, pos, string(line)); ok {
			entries = append(entries, e)
		} else {
			blanks++
		}
		pos += int64(len(line)) + 1
	}
	return entries, blanks, pos
}

func readAt(f *os.File, off int64, n int) ([]byte, error) {
	buf := make([]byte, n)
	read, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:read], nil
}
