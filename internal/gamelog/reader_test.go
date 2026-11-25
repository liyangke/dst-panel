package gamelog

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/dst"
)

func fixturePaths(t *testing.T) dst.Paths {
	t.Helper()
	root := t.TempDir()
	p := dst.Paths{KleiRoot: filepath.Join(root, "klei"), DstInstall: filepath.Join(root, "dst")}
	for _, shard := range []string{dst.ShardMaster, dst.ShardCaves} {
		if err := os.MkdirAll(p.ShardDir("Cluster_1", shard), 0o755); err != nil {
			t.Fatalf("建分片目录: %v", err)
		}
	}
	return p
}

func writeLog(t *testing.T, p dst.Paths, kind Kind, body string) string {
	t.Helper()
	path := p.ServerLog("Cluster_1", dst.ShardMaster)
	if kind == KindChat {
		path = p.ChatLog("Cluster_1", dst.ShardMaster)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写日志: %v", err)
	}
	return path
}

func appendLog(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开待追加的日志: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("追加日志: %v", err)
	}
}

func TestTailWindowAndOffsets(t *testing.T) {
	p := fixturePaths(t)
	var b strings.Builder
	const total = 500
	for i := 1; i <= total; i++ {
		fmt.Fprintf(&b, "[00:00:01]: Serializing world: session/AAA/BBB 第 %d 行\n", i)
	}
	writeLog(t, p, KindServer, b.String())
	raw := []byte(b.String())

	snap, err := NewReader(p).Tail("Cluster_1", dst.ShardMaster, KindServer, 50)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(snap.Entries) != 50 {
		t.Fatalf("窗口给了 %d 条，期望 50", len(snap.Entries))
	}
	if got := snap.Entries[0].Raw; !strings.HasSuffix(got, "第 451 行") {
		t.Errorf("窗口第一条 = %q，期望第 451 行（尾巴 50 行）", got)
	}
	if got := snap.Entries[49].Raw; !strings.HasSuffix(got, "第 500 行") {
		t.Errorf("窗口最后一条 = %q，期望第 500 行", got)
	}
	if !snap.Older {
		t.Error("前面还有 450 行没读，Older 必须为 true，界面才能说明这是尾巴而不是全部")
	}
	if snap.Next != int64(len(raw)) {
		t.Errorf("Next = %d，期望 %d（读到了文件末尾）", snap.Next, len(raw))
	}
	// offset 这个字段是"面板这一条 == 文件里那一行"的唯一凭据，必须是真字节位置。
	for _, e := range snap.Entries {
		if e.Offset < 0 || int(e.Offset) >= len(raw) {
			t.Fatalf("offset %d 越界", e.Offset)
		}
		end := bytes.IndexByte(raw[e.Offset:], '\n')
		got := string(raw[e.Offset : int(e.Offset)+end])
		want := "[00:00:01]: " + e.Raw
		if got != want {
			t.Fatalf("offset %d 处的原文是 %q，和这条记录的 %q 对不上", e.Offset, got, want)
		}
	}
	for i := 1; i < len(snap.Entries); i++ {
		if snap.Entries[i].Offset <= snap.Entries[i-1].Offset {
			t.Fatal("offset 必须严格递增，续读位置才有意义")
		}
	}
}

func TestTailNoOlderWhenWholeFileFits(t *testing.T) {
	p := fixturePaths(t)
	writeLog(t, p, KindServer, "[00:00:00]: Starting Up\n[00:00:01]: Sim unpaused\n\n")
	snap, err := NewReader(p).Tail("Cluster_1", dst.ShardMaster, KindServer, DefaultLines)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if snap.Older {
		t.Error("整份文件都读进来了还报 Older，界面会多出一句假提示")
	}
	if len(snap.Entries) != 2 {
		t.Errorf("给了 %d 条，期望 2 条（空行不该占窗口）", len(snap.Entries))
	}
	if snap.Blanks != 1 {
		t.Errorf("Blanks = %d，期望 1", snap.Blanks)
	}
	// 窗口不传时按 DefaultLines 走，而不是 0 行。
	if snap.Path == "" || !strings.HasSuffix(snap.Path, "server_log.txt") {
		t.Errorf("path = %q", snap.Path)
	}
}

func TestTailUsesChatPath(t *testing.T) {
	p := fixturePaths(t)
	writeLog(t, p, KindServer, "[00:00:01]: Serializing world: 这一行属于服务端日志\n")
	writeLog(t, p, KindChat, "[00:00:02]: [Announcement] 这一行属于聊天日志\n")

	snap, err := NewReader(p).Tail("Cluster_1", dst.ShardMaster, KindChat, 10)
	if err != nil {
		t.Fatalf("Tail chat: %v", err)
	}
	if len(snap.Entries) != 1 || !strings.Contains(snap.Entries[0].Raw, "聊天") {
		t.Fatalf("chat 读到的不是聊天日志: %+v", snap.Entries)
	}
	if snap.Entries[0].Event != EventAnnounce {
		t.Errorf("event = %s，公告要被单独认出来", snap.Entries[0].Event)
	}
}

// 半行不能显示、也不能推进位置：引擎是一行一行往文件里追加的，
// 没有换行的尾巴意味着"还在写"。把它当一条记录送出去，SSE 会先给半行、
// 再给同一行的后半截，一行变成两条，而 next 也已经越过了那半行的开头。
func TestFollowWaitsForLineTerminator(t *testing.T) {
	p := fixturePaths(t)
	path := writeLog(t, p, KindServer, "[00:00:01]: a\n[00:00:02]: b\n[00:00:03]: c\n")

	r := NewReader(p)
	snap, err := r.Tail("Cluster_1", dst.ShardMaster, KindServer, 10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(snap.Entries) != 3 {
		t.Fatalf("backlog %d 条", len(snap.Entries))
	}

	appendLog(t, path, "[00:00:04]: 前半")
	delta, err := r.ReadSince("Cluster_1", dst.ShardMaster, KindServer, snap.Since())
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(delta.Entries) != 0 {
		t.Fatalf("半行不该出记录，实际 %+v", delta.Entries)
	}
	if delta.Next != snap.Next {
		t.Errorf("半行不该推进位置：%d → %d", snap.Next, delta.Next)
	}

	appendLog(t, path, "后半\n")
	delta2, err := r.ReadSince("Cluster_1", dst.ShardMaster, KindServer, delta.Since())
	if err != nil {
		t.Fatalf("ReadSince 第二轮: %v", err)
	}
	if len(delta2.Entries) != 1 {
		t.Fatalf("补全后应得 1 条，实际 %+v", delta2.Entries)
	}
	if got := delta2.Entries[0].Raw; got != "前半后半" {
		t.Errorf("拼回来的正文 = %q", got)
	}
	if delta2.Entries[0].Clock != "00:00:04" {
		t.Errorf("时钟 = %q，半行合成后照样要解出来", delta2.Entries[0].Clock)
	}
}

// 引擎每轮启动都会把这两份日志截断重写（实测第一行固定是 PersistRootStorage）。
// 不报 Restarted 的话，前端会把新一轮的第一行接在上一轮后面，时间线直接接错。
func TestFollowDetectsTruncatedLog(t *testing.T) {
	p := fixturePaths(t)
	start := time.Date(2026, 10, 31, 2, 17, 35, 0, time.Local)
	old := fmt.Sprintf("[00:00:00]: Current time: %s\n", start.Format(time.ANSIC)) +
		"[00:00:01]: Serializing world: 上一轮的尾巴\n[00:00:02]: 再一行\n"
	writeLog(t, p, KindServer, old)

	r := NewReader(p)
	snap, err := r.Tail("Cluster_1", dst.ShardMaster, KindServer, 10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if snap.Version != "" || snap.Start.IsZero() {
		t.Fatalf("backlog 该带出起始时间，实际 %+v", snap)
	}

	writeLog(t, p, KindServer, "[00:00:00]: PersistRootStorage is now x\n")
	delta, err := r.ReadSince("Cluster_1", dst.ShardMaster, KindServer, snap.Since())
	if err != nil {
		t.Fatalf("截断后 ReadSince: %v", err)
	}
	if !delta.Restarted {
		t.Error("文件变短了必须报 Restarted")
	}
	if len(delta.Entries) != 1 || delta.Entries[0].Event != EventBoot {
		t.Fatalf("重启后该从头给出记录，实际 %+v", delta.Entries)
	}
	if !delta.Start.IsZero() {
		t.Errorf("新一轮没有 Current time 行，Start 必须回到零值而不是沿用上一轮: %v", delta.Start)
	}
	if delta.Entries[0].At != "" {
		t.Errorf("起始时间未知时不该硬给 At: %q", delta.Entries[0].At)
	}
}

func TestTailReadsStartAndVersion(t *testing.T) {
	p := fixturePaths(t)
	start := time.Date(2026, 10, 31, 2, 17, 35, 0, time.Local)
	body := fmt.Sprintf("[00:00:00]: Current time: %s\n", start.Format(time.ANSIC)) +
		"[00:00:00]: Version: 760944\n" +
		"[00:01:00]: Serializing world: session/206B72E995BA2B91/0000000059\n"
	writeLog(t, p, KindServer, body)

	snap, err := NewReader(p).Tail("Cluster_1", dst.ShardMaster, KindServer, 10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if snap.Version != "760944" {
		t.Errorf("version = %q", snap.Version)
	}
	if !snap.Start.Equal(start) {
		t.Errorf("start = %v，期望 %v", snap.Start, start)
	}
	last := snap.Entries[len(snap.Entries)-1]
	want := start.Add(time.Minute).UTC().Format(time.RFC3339)
	if last.At != want {
		t.Errorf("存档那行的墙上时间 = %q，期望 %q", last.At, want)
	}
}

// 引擎的日志尾部没有新一轮的行也可能超长：一次续读只取上限那么多，
// 剩下的留给下一轮——必须不重不漏。
func TestFollowDrainsWithoutGaps(t *testing.T) {
	p := fixturePaths(t)
	path := writeLog(t, p, KindServer, "[00:00:00]: 起点\n")

	var b strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "[00:00:01]: 世界生成日志 %04d %s\n", i, strings.Repeat("x", 200))
	}
	appendLog(t, path, b.String())

	r := NewReader(p)
	// 从 0 开始订：Tail 会把位置推到文件末尾，那样一进来就没得分轮了。
	since := Since{Offset: 0}

	seen := 0
	rounds := 0
	gaps := 0
	for round := 0; round < 20; round++ {
		rounds++
		delta, err := r.ReadSince("Cluster_1", dst.ShardMaster, KindServer, since)
		if err != nil {
			t.Fatalf("第 %d 轮 ReadSince: %v", round, err)
		}
		if delta.Restarted {
			t.Fatalf("第 %d 轮误判成重启了", round)
		}
		if len(delta.Entries) > 0 {
			first := delta.Entries[0]
			if first.Offset != since.Offset {
				gaps++
				t.Errorf("第 %d 轮起点错位：传 %d，得到 %d", round, since.Offset, first.Offset)
			}
		}
		seen += len(delta.Entries)
		since = delta.Since()
		if since.Offset == delta.Size {
			break
		}
	}
	wantBytes := int64(len(b.String()) + len("[00:00:00]: 起点\n"))
	if since.Offset != wantBytes {
		t.Errorf("最后停在 %d，文件长度 %d", since.Offset, wantBytes)
	}
	if gaps != 0 {
		t.Errorf("有 %d 处续读对不齐", gaps)
	}
	if seen != 4001 {
		t.Errorf("一共收到 %d 条，期望 4001 条（分轮必须不重不漏）", seen)
	}
	// 一轮最多读 maxFollowBytes：不封顶的话一次世界生成失败就能把整条 SSE 挤爆。
	// 这组数据有 900KB，必须真的被拆成多轮，否则这个上限根本没被验证到。
	if rounds < 2 {
		t.Errorf("只用了 %d 轮就读完，续读上限没生效", rounds)
	}
}

func TestMissingLogFileIsNotExist(t *testing.T) {
	p := fixturePaths(t)
	_, err := NewReader(p).Tail("Cluster_1", dst.ShardMaster, KindServer, 10)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("错误必须保住 os.ErrNotExist 这条链，接口层靠它选 503：%v", err)
	}
	_, err = NewReader(p).ReadSince("Cluster_1", dst.ShardCaves, KindChat, Since{})
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("续读也一样要能认出文件不存在：%v", err)
	}
}

// 名字来自 URL 路径段。这一层是第二道闸：接口层的 shardScope 万一被绕过，
// 这里也不能让 "../../x" 变成一次跨目录读取。
func TestReaderRejectsPathTraversal(t *testing.T) {
	p := fixturePaths(t)
	writeLog(t, p, KindServer, "[00:00:00]: x\n")
	for _, name := range []string{"", ".", "..", "../..", "a/b", `a\b`} {
		if _, err := NewReader(p).Tail(name, dst.ShardMaster, KindServer, 10); err == nil {
			t.Errorf("集群名 %q 必须被拒绝", name)
		}
		if _, err := NewReader(p).Tail("Cluster_1", name, KindServer, 10); err == nil {
			t.Errorf("分片名 %q 必须被拒绝", name)
		}
	}
	if _, err := NewReader(p).Tail("Cluster_1", dst.ShardMaster, Kind("chatlog"), 10); err == nil {
		t.Error("不认识的日志种类必须报错，而不是默认去读服务端日志")
	}
}

func TestTailTrimsToWindow(t *testing.T) {
	p := fixturePaths(t)
	var b strings.Builder
	const total = MaxLines + 300
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, "[00:00:01]: Serializing world: 第 %d 行\n", i)
	}
	writeLog(t, p, KindServer, b.String())

	// lines 超上限时被夹到 MaxLines：不报错，也不照单全收。
	snap, err := NewReader(p).Tail("Cluster_1", dst.ShardMaster, KindServer, 100000)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(snap.Entries) != MaxLines {
		t.Errorf("给了 %d 条，期望被夹到 %d", len(snap.Entries), MaxLines)
	}
	if !snap.Older {
		t.Error("被窗口裁掉时 Older 必须为 true")
	}
	// 裁掉的必须是窗口外（更早）的那些，尾巴一定留住——留错方向的日志页等于没有日志。
	if got := snap.Entries[len(snap.Entries)-1].Raw; !strings.HasSuffix(got, "第 2299 行") {
		t.Errorf("最后一条 = %q，期望文件真正的那一行", got)
	}
	if got := snap.Entries[0].Raw; !strings.HasSuffix(got, "第 300 行") {
		t.Errorf("窗口起点 = %q，期望第 300 行", got)
	}
}

func TestTailDefaultWindowOnSmallFile(t *testing.T) {
	p := fixturePaths(t)
	writeLog(t, p, KindServer, "[00:00:01]: 只有一行\n")

	// 不传 lines 时按 DefaultLines 走（而不是 0 行），整份文件都在窗口内时不能谎报 Older。
	snap, err := NewReader(p).Tail("Cluster_1", dst.ShardMaster, KindServer, 0)
	if err != nil {
		t.Fatalf("Tail 默认窗口: %v", err)
	}
	if len(snap.Entries) != 1 || snap.Older {
		t.Errorf("整份文件都读到了却报 Older 或条数不对: %+v", snap.Entries)
	}
}
