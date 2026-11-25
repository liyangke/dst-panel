// Package modbridge 实现面板侧的"文件桥"，与 mods/DSTPanelHelper 里的 Lua 一侧一一对应
// （modmain.lua 挂周期任务，scripts/panel_bridge.lua 是桥本体；两边改协议要一起改）。
//
// 为什么用文件而不是 HTTP：DST 服务端是个没有对外接口的 C++ 进程，唯一能从内部
// 主动做事的途径是跑一个 MOD，而 MOD 的沙箱里只能用 TheSim:SetPersistentString/GetPersistentString
// 读写存档目录。文件因此是唯一的通道，协议必须假设对面随时可能崩在半路。
//
// 三条从那次"半写覆盖"顾虑里推出来的规则：
//  1. 每个文件只有一个写者：queue.txt 只有面板写，status/result/processed 只有 MOD 写。
//  2. 面板写 queue.txt 永远整份原子重写（临时文件 + rename），MOD 只会看到旧内容或新内容。
//  3. 已经出现在 processed.txt 里的 id 才会被裁剪掉；反过来 MOD 用 id 去重，
//     所以裁剪慢一步只是浪费几行文本，快一步也不会丢命令。
package modbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dstpanel/internal/config"
	"dstpanel/internal/dst"
)

// 命令名必须与 panel_bridge.lua 的 handlers 表完全一致，
// 面板侧提前挡住未知命令，用户看到的就是中文提示而不是游戏里的一句 "未知命令"。
const (
	CmdPing     = "ping"
	CmdStatus   = "status"
	CmdMap      = "map"
	CmdAnnounce = "announce"
	CmdKick     = "kick"
	CmdMute     = "mute"
	CmdUnmute   = "unmute"
	CmdBan      = "ban"
	CmdUnban    = "unban"
)

var commandLabels = map[string]string{
	CmdPing:     "连通性探测",
	CmdStatus:   "索取一次完整数据",
	CmdMap:      "导出世界地图",
	CmdAnnounce: "全服公告",
	CmdKick:     "踢出玩家",
	CmdMute:     "禁言玩家",
	CmdUnmute:   "解除禁言",
	CmdBan:      "拉黑玩家（封禁）",
	CmdUnban:    "解除拉黑",
}

// ErrHelperOffline 表示桥那头没人应答：分片在跑但 helper MOD 没加载，或者已经卡死。
var ErrHelperOffline = errors.New("助手 MOD 没有响应")

// ErrCommandPending 表示助手是活的、只是这条命令还没轮到它执行（前面的命令在跑，
// 或者 MOD 的轮询间隔比等待时长长）。和"没人应答"必须分开：用户该等，不该重复提交。
var ErrCommandPending = errors.New("命令还在队列里等待执行")

type Bridge struct {
	Paths dst.Paths
	// StaleAfter 超过这个时长没有新的 status.json，就判定助手 MOD 掉线。
	StaleAfter time.Duration
	// QueueLimit 是待执行命令条数上限。MOD 掉线时命令会一直堆着，
	// 等它回来时执行的可能已经是几小时前的操作了，那种"迟到生效"比直接拒绝危险得多。
	QueueLimit int
	// Helper 是往 modoverrides.lua 写助手条目时用的默认设置。
	Helper HelperSettings
	// Now 供测试替换。
	Now func() time.Time

	mu  sync.Mutex
	seq atomic.Uint64
}

func New(paths dst.Paths, helper config.Helper) *Bridge {
	return &Bridge{
		Paths:      paths,
		StaleAfter: time.Duration(helper.StaleSeconds) * time.Second,
		QueueLimit: 200,
		Helper:     defaultHelperSettings(helper),
	}
}

func (b *Bridge) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Bridge) staleAfter() time.Duration {
	if b.StaleAfter > 0 {
		return b.StaleAfter
	}
	return 15 * time.Second
}

func (b *Bridge) queueLimit() int {
	if b.QueueLimit > 0 {
		return b.QueueLimit
	}
	return 200
}

// Enqueue 追加一条命令，返回命令 id。id 形如 <毫秒时间戳>-<自增号>，
// 既能排序又能保证同一毫秒内不撞车，MOD 侧靠它去重。
func (b *Bridge) Enqueue(cluster, shard, command string, args ...string) (string, error) {
	if _, ok := commandLabels[command]; !ok {
		return "", fmt.Errorf("不支持的桥命令 %q，可用：%s", command, strings.Join(CommandNames(), "、"))
	}
	for i, a := range args {
		if err := checkArg(a); err != nil {
			return "", fmt.Errorf("第 %d 个参数不合法: %w", i+1, err)
		}
	}
	id := b.newID()
	line := strings.Join(append([]string{id, command}, args...), "|")

	b.mu.Lock()
	defer b.mu.Unlock()

	queue, err := b.readLines(b.Paths.HelperCommandQueue(cluster, shard))
	if err != nil {
		return "", err
	}
	if len(queue) >= b.queueLimit() {
		return "", fmt.Errorf("待执行命令已经积压 %d 条，助手 MOD 没有消费；先确认世界在跑且 MOD 已加载", len(queue))
	}
	queue = append(queue, line)
	if err := dst.WriteFileAtomic(b.Paths.HelperCommandQueue(cluster, shard),
		[]byte(strings.Join(queue, "\n")+"\n"), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

func checkArg(a string) error {
	if strings.ContainsAny(a, "\r\n") {
		return fmt.Errorf("不能包含换行")
	}
	if len(a) > 500 {
		return fmt.Errorf("长度不能超过 500 字节")
	}
	return nil
}

func (b *Bridge) newID() string {
	return fmt.Sprintf("%d-%d", b.now().UnixMilli(), b.seq.Add(1))
}

// CommandNames 返回界面上能点的桥命令，顺序固定，避免每次刷新按钮换位。
func CommandNames() []string {
	names := make([]string, 0, len(commandLabels))
	for k := range commandLabels {
		names = append(names, k)
	}
	// 探测类在前，破坏性在后，和 dst 的命令面板同一套排序思路。
	order := map[string]int{
		CmdPing: 0, CmdStatus: 1, CmdMap: 2, CmdAnnounce: 3, CmdKick: 4,
		CmdMute: 5, CmdUnmute: 6, CmdBan: 7, CmdUnban: 8,
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && order[names[j]] < order[names[j-1]]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

func CommandLabel(command string) string {
	if l, ok := commandLabels[command]; ok {
		return l
	}
	return command
}

// Pending 返回还在队列里、MOD 尚未标记执行的命令。
func (b *Bridge) Pending(cluster, shard string) ([]string, error) {
	queue, err := b.readLines(b.Paths.HelperCommandQueue(cluster, shard))
	if err != nil {
		return nil, err
	}
	done, err := b.processedIDs(cluster, shard)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range queue {
		id := line[:firstByte(line, '|')]
		if !done[id] {
			out = append(out, line)
		}
	}
	return out, nil
}

// Trim 把已被 MOD 执行的命令从队列里删掉，返回删除条数。
// 队列文件只会被写小，不会丢未执行命令，所以随时调用都安全。
func (b *Bridge) Trim(cluster, shard string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	queue, err := b.readLines(b.Paths.HelperCommandQueue(cluster, shard))
	if err != nil {
		return 0, err
	}
	if len(queue) == 0 {
		return 0, nil
	}
	done, err := b.processedIDs(cluster, shard)
	if err != nil {
		return 0, err
	}
	var keep []string
	for _, line := range queue {
		if !done[line[:firstByte(line, '|')]] {
			keep = append(keep, line)
		}
	}
	removed := len(queue) - len(keep)
	if removed == 0 {
		return 0, nil
	}
	var body string
	if len(keep) > 0 {
		body = strings.Join(keep, "\n") + "\n"
	}
	if err := dst.WriteFileAtomic(b.Paths.HelperCommandQueue(cluster, shard), []byte(body), 0o644); err != nil {
		return 0, err
	}
	return removed, nil
}

func (b *Bridge) processedIDs(cluster, shard string) (map[string]bool, error) {
	lines, err := b.readLines(b.Paths.HelperProcessedFile(cluster, shard))
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(lines))
	for _, l := range lines {
		set[l] = true
	}
	return set, nil
}

// Result 是 MOD 对一条命令的回执。value 保留原始 JSON，由调用方按命令类型解释，
// 这样新增命令时不用改这里的结构。
type Result struct {
	ID    string          `json:"-"`
	OK    bool            `json:"ok"`
	Error string          `json:"error"`
	Value json.RawMessage `json:"value"`
}

// ValueInto 把回执里的 value 解进目标类型。
func (r Result) ValueInto(out any) error {
	if len(r.Value) == 0 {
		return fmt.Errorf("回执里没有数据")
	}
	return json.Unmarshal(r.Value, out)
}

func (b *Bridge) Results(cluster, shard string) (map[string]Result, error) {
	raw, err := b.readJSON(b.Paths.HelperResultFile(cluster, shard))
	if err != nil {
		return nil, err
	}
	var m map[string]Result
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("助手 MOD 的回执文件不是合法 JSON: %w", err)
	}
	for id, r := range m {
		r.ID = id
		m[id] = r
	}
	return m, nil
}

// Result 查单条命令的回执，found=false 表示 MOD 还没回。
func (b *Bridge) Result(cluster, shard, id string) (Result, bool, error) {
	all, err := b.Results(cluster, shard)
	if err != nil {
		return Result{}, false, err
	}
	r, ok := all[id]
	return r, ok, nil
}

// Wait 轮询到某条命令有回执为止。超时不是失败：MOD 可能只是慢，
// 所以超时错误里带上队列与心跳状态，让用户知道该看哪里。
func (b *Bridge) Wait(ctx context.Context, cluster, shard, id string, timeout time.Duration) (Result, error) {
	deadline := b.now().Add(timeout)
	interval := 250 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return Result{}, b.timeoutError(ctx, cluster, shard, id)
		default:
		}
		r, ok, err := b.Result(cluster, shard, id)
		if err != nil {
			return Result{}, err
		}
		if ok {
			return r, nil
		}
		if !b.now().Before(deadline) {
			return Result{}, b.timeoutError(ctx, cluster, shard, id)
		}
		select {
		case <-ctx.Done():
			return Result{}, b.timeoutError(ctx, cluster, shard, id)
		case <-time.After(interval):
		}
	}
}

// timeoutError 决定"等不到回执"这句话怎么说。
//
// 调用方传进来的 ctx 超时和我们自己的 deadline 通常一样长，谁先响是随机的；
// 早先这里直接把 ctx.Err() 抛出去，接口层就再也分不清"助手压根没回话"和
// "助手活着只是这条还没轮到"，面板最有用的那句诊断因此经常不出现。
// 到点一律走 offlineError；只有客户端真断开（Canceled）才原样返回，
// 那种情况继续诊断下去没有意义，也没人等结果。
func (b *Bridge) timeoutError(ctx context.Context, cluster, shard, id string) error {
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return b.offlineError(cluster, shard, id)
}

func (b *Bridge) offlineError(cluster, shard, id string) error {
	st, stErr := b.Status(cluster, shard)
	switch {
	case stErr != nil && errors.Is(stErr, os.ErrNotExist):
		return fmt.Errorf("%w：%s/%s 还没有 status.json。%s",
			ErrHelperOffline, cluster, shard, b.diagnoseOffline(cluster, shard))
	case stErr != nil:
		return fmt.Errorf("等待命令 %s 回执超时，读状态数据也失败: %w", id, stErr)
	case st.Stale:
		return fmt.Errorf("%w：数据已停更 %s，世界可能卡死或 MOD 被禁用", ErrHelperOffline, st.Age.Round(time.Second))
	}
	return fmt.Errorf("%w：命令 %s 在队列里超过 %s 仍未执行", ErrCommandPending, id, b.staleAfter())
}

// diagnoseOffline 把"助手没响应"缩到用户能动手的原因上。
//
// 这一步不是锦上添花：专用服务端启动时先 DisableAllMods，模组只可能由
// modoverrides.lua 重新打开，于是"没启用""文件装错位""分片没开世界"三种病
// 在面板上看症状完全一样（都是没有 status.json）。不区分就只能让用户去翻 server_log.txt。
func (b *Bridge) diagnoseOffline(cluster, shard string) string {
	var reasons []string

	if missing, err := b.HelperInstallState(); err == nil && len(missing) > 0 {
		reasons = append(reasons, fmt.Sprintf("助手 MOD 文件不全，缺 %s（安装位置应为 %s）",
			strings.Join(missing, "、"), b.Paths.HelperModDir()))
	}

	ov, err := b.GetHelperOverride(cluster, shard)
	switch {
	case err != nil:
		reasons = append(reasons, "读不到世界的 modoverrides.lua: "+err.Error())
	case !ov.Present:
		reasons = append(reasons, fmt.Sprintf("%s/%s 的 modoverrides.lua 里没有助手条目，即从未启用", cluster, shard))
	case !ov.Enabled:
		reasons = append(reasons, fmt.Sprintf("%s/%s 的 modoverrides.lua 里助手是关闭状态", cluster, shard))
	}

	if len(reasons) == 0 {
		return "启用状态和文件都在位，多半是服务端还没进入存档，或者助手没被加载。去「游戏日志」搜 DSTPanelHelper 能看到报错"
	}
	return "可能原因：" + strings.Join(reasons, "；")
}

// Ping 走桥问一次"你还在吗"。返回 MOD 报的分片名，用来验证命令确实被这个分片消费了
// ——多分片集群里，只有 Master 会处理 Master 目录下的队列，串了目录会立刻暴露。
func (b *Bridge) Ping(ctx context.Context, cluster, shard string) (string, error) {
	id, err := b.Enqueue(cluster, shard, CmdPing)
	if err != nil {
		return "", err
	}
	res, err := b.Wait(ctx, cluster, shard, id, b.staleAfter()+5*time.Second)
	if err != nil {
		return "", err
	}
	if !res.OK {
		return "", fmt.Errorf("助手 MOD 报错: %s", res.Error)
	}
	var pong string
	_ = res.ValueInto(&pong)
	return pong, nil
}

// Announce 通过 MOD 广播公告，回执确认才算发出去。
func (b *Bridge) Announce(ctx context.Context, cluster, shard, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("公告内容不能为空")
	}
	id, err := b.Enqueue(cluster, shard, CmdAnnounce, text)
	if err != nil {
		return err
	}
	res, err := b.Wait(ctx, cluster, shard, id, b.staleAfter()+5*time.Second)
	if err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("公告发送失败: %s", res.Error)
	}
	return nil
}

// Kick 按玩家名踢人。名字来自快照，不信任前端传来的自由文本。
func (b *Bridge) Kick(ctx context.Context, cluster, shard, playerName, reason string) error {
	if strings.TrimSpace(playerName) == "" {
		return fmt.Errorf("要踢的玩家名不能为空")
	}
	id, err := b.Enqueue(cluster, shard, CmdKick, playerName, reason)
	if err != nil {
		return err
	}
	res, err := b.Wait(ctx, cluster, shard, id, b.staleAfter()+5*time.Second)
	if err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("踢人失败: %s", res.Error)
	}
	return nil
}

func (b *Bridge) readLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	// 队列是面板自己写的（没有头），processed 是 MOD 写的（有头），两种都要能读。
	text := strings.ReplaceAll(string(dst.StripKleiHeader(raw)), "\r\n", "\n")
	var out []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

func (b *Bridge) readJSON(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []byte("{}"), nil
		}
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		// SetPersistentString 有可能先创建空文件再写内容。
		return []byte("{}"), nil
	}
	return dst.StripKleiHeader(raw), nil
}

func firstByte(s string, c byte) int {
	if i := strings.IndexByte(s, c); i >= 0 {
		return i
	}
	return len(s)
}
