// Package backup 是存档快照层：打包、清单、校验、恢复与保留。
//
// 为什么要单独一个包，而不是把 tar 塞进 api：这三类失败都会丢世界，
// 而它们各自要防的东西完全不同——
//   - 打包要防把 Klei 令牌一起打进快照（快照是可以被下载的）；
//   - 恢复要防"回滚静默不生效"：引擎加载的是 save/session/<GAMEID>/ 下编号最大的
//     slot 目录（mainfunctions.lua 的 GetNextSaveSlot 只做递增），所以只覆盖
//     server.dat 而留着更新的 slot，进游戏看到的还是恢复前的世界；
//   - 保留要防定时备份把小机的盘写满（一份归档实测是存档目录的零头，见 api 层那句说明）。
//
// 这一层只管文件，不碰进程：c_save() 由调用方（worker）在打包前触发，
// "分片必须停"这条检查也在 worker 那边做——它才有 screen 会话的可见性。
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"dstpanel/internal/dst"
)

// 快照的四种来源。这四个字符串会进文件名、进清单、进界面，改动要考虑旧快照。
// imported 不是"这台机器打出来的档"，而是外部上传的包变成的快照（见 import.go）。
const (
	LabelManual        = "manual"
	LabelScheduled     = "scheduled"
	LabelBeforeRestore = "before-restore"
	LabelImported      = "imported"
)

// ArchiveSuffix / ManifestSuffix 是快照在磁盘上的两个文件。
// 清单单独放（而不是打进归档里）是有意的：界面列快照时只需要读几百字节的 JSON，
// 不必为了一次列表把 140MB 的包解开。
const (
	ArchiveSuffix  = ".tar.gz"
	ManifestSuffix = ".manifest.json"
)

// labelText 是快照来源的中文说法。界面只显示这个，不显示裸码值。
var labelText = map[string]string{
	LabelManual:        "手工备份",
	LabelScheduled:     "定时备份",
	LabelBeforeRestore: "恢复前的安全备份",
	LabelImported:      "外部导入的存档",
}

// LabelFor 把来源码翻成中文；认不出来的来源也必须有中文，
// 因为库里可能留着别的版本面板写的标签，而界面拿到空标签会退回显示码值。
func LabelFor(label string) string {
	if t, ok := labelText[label]; ok {
		return t
	}
	return "未知来源（" + label + "）"
}

// FileEntry 是归档里一个文件的登记项。
// SHA256 在打包时算一次，之后"校验快照"和"恢复后确认写对了"都复用它，
// 不再相信"我能把这个包解开"就等于"这个包里的东西是对的"。
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest 是快照的清单，也就是磁盘上那份 JSON 的结构。
type Manifest struct {
	ID      string `json:"id"`
	Cluster string `json:"cluster"`
	Label   string `json:"label"`
	// CreatedAt 用 RFC3339 的 UTC 文本；文件名里已经带了同一时刻，
	// 这里留一份是为了不靠解析文件名来显示时间。
	CreatedAt         string      `json:"createdAt"`
	Shards            []string    `json:"shards"`
	PanelVersion      string      `json:"panelVersion"`
	FileCount         int         `json:"fileCount"`
	UncompressedBytes int64       `json:"uncompressedBytes"`
	ArchiveBytes      int64       `json:"archiveBytes"`
	Files             []FileEntry `json:"files"`
	Skipped           []string    `json:"skipped,omitempty"`
	Note              string      `json:"note,omitempty"`
}

// Snapshot 是给界面看的快照视图：清单字段 + 中文标签 + 两个"能不能用"的结论。
type Snapshot struct {
	Manifest
	LabelLabel string `json:"labelLabel"`
	// Restorable 回答"这份快照现在真能拿去做恢复吗"：清单读得出、归档在、
	// 里面至少有一个分片的 save 目录。少了任何一样，界面上的恢复按钮就不该点。
	Restorable bool `json:"restorable"`
	// RestoreNote 是 Restorable 为假时的那句原因，为真时为空。
	RestoreNote string `json:"restoreNote,omitempty"`
	// SaveShards 是这份快照里带 save 目录的分片（恢复要整棵替换的那几个）。
	SaveShards []string `json:"saveShards"`
	// HasSave 回答某个分片在不在这份快照里，界面用它解释"为什么 Caves 没动"。
	ShardSaveBytes map[string]int64 `json:"shardSaveBytes,omitempty"`
}

// Manager 在一个目录里读写快照。
type Manager struct {
	Paths dst.Paths
	// Dir 是快照目录（配置里的 workDir/backups）。
	Dir string
	// Keep 是最多保留几份；小于等于 0 表示不自动清理。
	// 需要在运行中跟着面板设置走时填 KeepFunc，它优先于这个字段。
	Keep int
	// KeepFunc 让保留上限能在面板设置里改，不用重启。
	// 为什么不直接改 Keep：Manager 是多个接口和 worker 共用的一个值，
	// 写它会和正在跑的 Prune 抢同一字段。这里每次现读库里的设置。
	KeepFunc func() int
	// Version 只写进清单，供以后判断"这份快照是哪版面板打的、字段读不读得懂"。
	Version string
	// Logger 为空时用 slog.Default()。
	Logger *slog.Logger
	// openFileFn 只给测试用：注入一个会报"文件已经没了"的打开函数，
	// 才能稳定复现引擎在打包途中淘汰最旧自动存档的那一瞬间。
	openFileFn func(path string) (*os.File, error)
}

// openFile 打开一个待打包的条目。为什么不直接 os.Open：见 packOne 那段——
// 生产不设 openFileFn，行为和 os.Open 完全一致。
func (m *Manager) openFile(path string) (*os.File, error) {
	if m.openFileFn != nil {
		return m.openFileFn(path)
	}
	return os.Open(path)
}

func (m *Manager) log() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// keep 返回当前生效的保留上限：设置里改过就以设置为准，否则用配置文件的值。
// 两者并存是刻意的——配置给一个不用登录也能定的默认值，设置让人能在界面里调，
// 而改配置要重启，重启对正在跑的分片是一次打扰。
func (m *Manager) keep() int {
	if m.KeepFunc != nil {
		return m.KeepFunc()
	}
	return m.Keep
}

// KeepNow 把"当前生效的保留上限"给接口层显示。
// 不给字段而给方法，是因为真正在用的那个值可能来自面板设置而不是 Manager 里的数字，
// 界面上显示一份、清理时按另一份，比显示一个粗略数字更糟。
func (m *Manager) KeepNow() int { return m.keep() }

// checkClusterName 挡住"把 ../ 或绝对路径当集群名传进来"。
// 调用方（api 层）本来就按 ClusterNames() 白名单校验过，这里是第二道闸：
// 这一层的任何方法都不该因为上游多信了一次就写到 kleiRoot 外面去。
func checkClusterName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name ||
		strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("非法的集群名 %q：只能是 kleiRoot 下的目录名本身，不能带路径", name)
	}
	return nil
}

// freeID 给这次备份挑一个没被占用的编号。
//
// 同一秒里做两次备份是真会发生的（连点、或者定时任务正好撞上手工那次），
// 而编号就是文件名。直接覆盖的后果是"用户以为有两次备份，其实只剩一次"，
// 所以撞名时把来源后缀加个号，而不是把编号做成随机串——编号要一眼读得懂。
func (m *Manager) freeID(cluster, label string, at time.Time) (string, error) {
	id := NewID(cluster, label, at)
	for i := 2; i <= 999; i++ {
		_, errA := os.Stat(m.archivePath(id))
		_, errM := os.Stat(m.manifestPath(id))
		missingBoth := os.IsNotExist(errA) && os.IsNotExist(errM)
		if missingBoth {
			return id, nil
		}
		if errA != nil && !os.IsNotExist(errA) {
			return "", fmt.Errorf("读备份 %s: %w", id, errA)
		}
		if errM != nil && !os.IsNotExist(errM) {
			return "", fmt.Errorf("读备份清单 %s: %w", id, errM)
		}
		id = NewID(cluster, fmt.Sprintf("%s-%d", label, i), at)
	}
	return "", fmt.Errorf("%s 这一秒里已经攒了 999 份同名备份，先检查是不是有程序在循环备份", at.Format(time.RFC3339))
}

func (m *Manager) archivePath(id string) string {
	return filepath.Join(m.Dir, id+ArchiveSuffix)
}

func (m *Manager) manifestPath(id string) string {
	return filepath.Join(m.Dir, id+ManifestSuffix)
}

// NewID 由时刻 + 集群名 + 来源拼出快照编号。
// 集群名进编号是为了让人在几十个文件里一眼看出这是哪个房间的档，
// 也顺带保证同一秒内的两次备份不会撞名（不同集群）。
func NewID(cluster, label string, at time.Time) string {
	return fmt.Sprintf("%s-%s-%s", at.UTC().Format("20060102T150405Z"),
		sanitizeForName(cluster), sanitizeForName(label))
}

var namePartRegexp = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// sanitizeForName 把任意标签变成能安全出现在文件名里的片段。
// 编号之后会参与拼路径，所以这里除了替换非法字符，也保证结果里不会有分隔符。
func sanitizeForName(s string) string {
	out := namePartRegexp.ReplaceAllString(strings.TrimSpace(s), "_")
	if out == "" {
		out = "x"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// 快照编号长这样：20261031T021500Z-Cluster_1-manual。
// 校验用白名单正则而不是"看着不像路径就放行"：这个字符串来自 URL 路径段，
// 会直接参与 filepath.Join。
var idRegexp = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[A-Za-z0-9_-]{1,64}-[A-Za-z0-9_-]{1,40}$`)

// CheckID 挡住"把 ../Master/save 当编号传进来"这一类输入。
func CheckID(id string) error {
	if !idRegexp.MatchString(id) {
		return fmt.Errorf("备份编号 %q 不是面板生成的格式（形如 20261031T021500Z-Cluster_1-manual），不按它去找文件", id)
	}
	return nil
}

// skipRel 返回这个归档路径不打包的原因；该打包时返回空串。
// 每条规则都对应一次真实代价：
//   - cluster_token.txt 是 Klei 账户凭据。快照能下载，进了快照就等于把账户交出去，
//     而恢复一个旧档本来也用不上它（令牌是房间配置，不是世界状态）。
//   - dstpanel_* 是面板和助手 MOD 之间的命令通道（队列/已执行/结果/状态，还有一个 .bak）。
//     把旧队列恢复回磁盘等于把用户早就撤掉的命令重新塞回游戏：MOD 每个 tick 照着队列执行，
//     而"已执行编号"也一起被回滚了，去重挡不住。
//   - 引擎日志每次启动都被重写，留一份只会让人对着旧日志判断现在发生了什么。
//     云机实测还要多挡一层：分片会把上一轮的日志挪进 <分片>/backup/server_log/ 存着，
//     那份滚动副本同样只是日志，却在黑名单只认文件名的时候被当成了"别的东西"打进去
//     （一份快照里混进 22 个日志副本，界面还喊"跳过了引擎日志"）。
//   - .tmp 与 .rollback- 是面板自己留下的中间态（写一半的临时文件、恢复时移开的旧存档）。
//     把它们打进快照，下一份快照里就有一份"看着像存档其实是被淘汰的旧档"。
func skipRel(rel string) string {
	base := filepath.Base(filepath.FromSlash(rel))
	switch {
	case base == "cluster_token.txt":
		return "Klei 令牌不打进备份（备份可以下载，令牌是账户凭据不是世界状态）"
	case strings.HasPrefix(base, "dstpanel_"):
		return "面板与助手 MOD 的命令通道文件不打包（恢复旧队列等于重新发出已撤销的命令）"
	case base == "server_log.txt" || base == "server_chat_log.txt":
		return "引擎日志不打包（每次启动都会被重写）"
	case strings.Contains(rel, "/server_log/") || strings.Contains(rel, "/server_chat_log/"):
		return "引擎攒的旧日志副本不打包（世界把上一轮的日志挪进 backup/server_log 存着，恢复它没有任何用处）"
	case strings.Contains(base, ".rollback-"):
		return "恢复存档时移开旧内容的中间目录不打包"
	case strings.HasSuffix(base, ".tmp") || strings.Contains(base, ".tmp-"):
		return "写一半的临时文件不打包"
	}
	return ""
}

// List 按时间倒序返回目录里的快照。
//
// 列表完全以磁盘为准，不建数据库表：手工拷进来或删掉一个文件时，
// 界面必须跟着变——库里留着一行"看起来能恢复"的快照，比少一条记录糟糕得多。
//
// 两份文件都要参与：只剩清单没有归档的快照（有人拷走了 .tar.gz）必须在列表里
// 显示成"不能恢复"，静默消失的话用户就永远不知道自己那份档去哪了。
func (m *Manager) List() ([]Snapshot, error) {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取备份目录 %s: %w", m.Dir, err)
	}
	var out []Snapshot
	var orphans []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var id string
		switch {
		case strings.HasSuffix(name, ArchiveSuffix):
			id = strings.TrimSuffix(name, ArchiveSuffix)
		case strings.HasSuffix(name, ManifestSuffix):
			id = strings.TrimSuffix(name, ManifestSuffix)
		default:
			// 快照目录里的别的文件（谁临时放的、半截的 .tmp）不冒充快照。
			continue
		}
		if CheckID(id) != nil {
			orphans = append(orphans, name)
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		snap := m.view(id)
		// 列表不展开文件清单：20 份 × 上百个文件（每个还带 sha256）会把响应撑到几百 KB，
		// 而列表只需要文件名、大小、能不能恢复。明细在详情接口里给。
		snap.Files = nil
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(orphans) > 0 {
		// 完全静默是最糟的结果：用户看到"只有 3 份快照"，而目录里明明有 5 个 .tar.gz。
		// 认不出的文件名照实说，人一眼就知道是自己拷进来的还是别的东西。
		out = append(out, Snapshot{
			Manifest: Manifest{
				ID:   "(无法识别的文件)",
				Note: fmt.Sprintf("备份目录里有 %d 个文件名不是面板生成的格式，面板不认它们：%s", len(orphans), strings.Join(orphans, "、")),
			},
			LabelLabel:     "不是面板生成的备份",
			SaveShards:     []string{},
			ShardSaveBytes: map[string]int64{},
			RestoreNote:    "面板只认自己生成的编号格式（形如 20261031T021500Z-Cluster_1-manual），这些文件不会被列出、下载或清理",
		})
	}
	return out, nil
}

// view 组装一个快照的界面视图。清单读不出来时仍然返回一条记录，
// 带着"为什么不能恢复"，而不是让这一份快照从列表里凭空消失。
func (m *Manager) view(id string) Snapshot {
	snap := Snapshot{
		Manifest:       Manifest{ID: id},
		SaveShards:     []string{},
		ShardSaveBytes: map[string]int64{},
	}
	st, statErr := os.Stat(m.archivePath(id))
	mf, err := m.readManifest(id)
	if err != nil {
		snap.RestoreNote = "清单读不出来，这份备份不能用来恢复：" + err.Error()
		return snap
	}
	snap.Manifest = mf
	snap.LabelLabel = LabelFor(mf.Label)
	snap.saveShards()
	// 归档不在时清单照旧读出来：这一行还有时间、来源、登记了多少个文件可看，
	// 用户要判断"是哪一天那份丢了"靠的就是这些，而不是一个空白壳。
	// 顺序很重要——先前这里先 Stat 再读清单，归档一缺就整行退化，
	// 界面只能显示"这份快照什么都没有"。saveShards 也得在归档那一步之前，
	// 它算的是清单里的文件列表，和归档在不在无关。
	if statErr != nil {
		// 清单里那个大小是打包当时记的；归档不在了就得报 0，
		// 否则界面会出现"归档 1.3 KB + 归档文件不见了"这种自相矛盾的一行。
		snap.ArchiveBytes = 0
		if os.IsNotExist(statErr) {
			snap.RestoreNote = "归档文件不见了，只剩清单：这一份的 .tar.gz 不在备份目录里（多半是被手工删过，或清理时删错了），不能用来恢复"
		} else {
			snap.RestoreNote = "归档文件读不到（权限或磁盘的问题），这份先不要拿来恢复"
		}
		return snap
	}
	// 大小一律以磁盘为准：清单里那个数字是打包当时记的，被人手工替换过文件时
	// 两者才会不一致，而那正是"别拿它恢复"的信号。
	snap.ArchiveBytes = st.Size()
	if len(snap.SaveShards) == 0 {
		snap.RestoreNote = "这份备份里没有任何世界的 save 目录，里面没有可恢复的世界"
		return snap
	}
	if mf.FileCount != len(mf.Files) {
		snap.RestoreNote = fmt.Sprintf("清单登记了 %d 个文件，实际列出来 %d 个，这份备份的清单本身就不自洽",
			mf.FileCount, len(mf.Files))
		return snap
	}
	if mf.ArchiveBytes > 0 && mf.ArchiveBytes != st.Size() {
		snap.RestoreNote = fmt.Sprintf("归档大小和清单记的对不上（清单 %d 字节，磁盘上 %d 字节），可能被别的程序改过；先做一次校验",
			mf.ArchiveBytes, st.Size())
		return snap
	}
	snap.Restorable = true
	return snap
}

// saveShards 从文件清单里算出"哪些分片带了 save 目录"以及各自多少字节。
// 不另存字段是因为清单本身就是事实来源，多存一份就会有两边不一致的一天。
func (s *Snapshot) saveShards() {
	seen := map[string]bool{}
	var order []string
	for _, f := range s.Files {
		parts := strings.Split(f.Path, "/")
		if len(parts) < 3 || parts[1] != "save" {
			continue
		}
		if s.ShardSaveBytes == nil {
			s.ShardSaveBytes = map[string]int64{}
		}
		s.ShardSaveBytes[parts[0]] += f.Size
		if !seen[parts[0]] {
			seen[parts[0]] = true
			order = append(order, parts[0])
		}
	}
	sort.Strings(order)
	s.SaveShards = order
}

// Get 返回单个快照（含完整文件清单）。
func (m *Manager) Get(id string) (Snapshot, error) {
	if err := CheckID(id); err != nil {
		return Snapshot{}, err
	}
	return m.view(id), nil
}

func (m *Manager) readManifest(id string) (Manifest, error) {
	var mf Manifest
	raw, err := os.ReadFile(m.manifestPath(id))
	if err != nil {
		return mf, fmt.Errorf("读清单 %s: %w", id+ManifestSuffix, err)
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		return mf, fmt.Errorf("清单 %s 不是合法 JSON: %w", id+ManifestSuffix, err)
	}
	if mf.ID != id {
		return mf, fmt.Errorf("清单里写的编号是 %q，文件名却是 %q，这两份对不上", mf.ID, id)
	}
	return mf, nil
}

// ArchivePath 给出下载用的归档绝对路径，并确认文件真的在。
func (m *Manager) ArchivePath(id string) (string, error) {
	if err := CheckID(id); err != nil {
		return "", err
	}
	p := m.archivePath(id)
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s 的归档文件不在 %s 里，可能已经被删掉了", id, m.Dir)
		}
		return "", fmt.Errorf("读 %s: %w", p, err)
	}
	if st.Size() == 0 {
		return "", fmt.Errorf("%s 的归档是 0 字节，这份备份没打完就被中断了", id)
	}
	return p, nil
}

// Delete 删掉一份快照的两个文件。
// 归档没了但清单还在时也要一起清：留着清单，列表里就会出现一条点不动的记录。
func (m *Manager) Delete(id string) error {
	if err := CheckID(id); err != nil {
		return err
	}
	errA := os.Remove(m.archivePath(id))
	errM := os.Remove(m.manifestPath(id))
	for _, err := range []error{errA, errM} {
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除备份 %s: %w", id, err)
		}
	}
	if os.IsNotExist(errA) && os.IsNotExist(errM) {
		return fmt.Errorf("备份 %s 不存在，没删任何东西；先看一眼备份列表", id)
	}
	return nil
}

// Prune 把快照压回 Keep 那一条总量上限，从最旧的开始删；protected 里的那些本轮不删。
// 返回被删掉的编号。
//
// 恢复前的安全备份也占名额，这是取舍而不是遗漏：如果给它开后门，
// 反复恢复会把快照目录变成"全是不值钱的中间态"，真正的旧档反而被挤掉。
// 所以 Keep 就是一条硬的总量上限，界面上把每份的来源写清楚，由人决定要不要先下载。
//
// protected 是给"此刻正被用着的那一份"留的活门：恢复会先打一份安全备份，
// 那一份把总数顶过上限时，最旧的那份往往正是这次要恢复的档。
// 实测踩过——目录满额时"恢复到最旧那份"必然失败：归档在解压中途被自己的清理删了，
// 只能靠回滚退回原世界，而用户看到的是一次莫名其妙的失败。
//
// 跳过它之后还要继续往里清：界面上写着"最多留 3 份"，目录里却躺着 4 份，
// 用户就会怀疑这条上限根本没生效。所以名额是让给受保护那一份，不是把上限抬高。
func (m *Manager) Prune(protected ...string) ([]string, error) {
	keep := m.keep()
	if keep <= 0 {
		return nil, nil
	}
	list, err := m.List()
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for _, p := range protected {
		skip[p] = true
	}
	// list 是倒序（新→旧），所以从尾部开始删才留得住最近的。
	// 数的是"认得出是快照的那些"：目录里那条认不出的说明行不算名额，
	// 否则它会占掉一格，让上限看起来比实际生效的宽。
	total := 0
	for _, snap := range list {
		if CheckID(snap.ID) == nil {
			total++
		}
	}
	if total <= keep {
		return nil, nil
	}
	var removed []string
	for i := len(list) - 1; i >= 0 && total > keep; i-- {
		snap := list[i]
		if CheckID(snap.ID) != nil || skip[snap.ID] {
			continue
		}
		if err := m.Delete(snap.ID); err != nil {
			return removed, fmt.Errorf("按保留上限清理备份时：%w", err)
		}
		removed = append(removed, snap.ID)
		total--
	}
	if total > keep {
		m.log().Info("受保护的备份让目录暂时超出保留上限", "keep", keep, "now", total)
	}
	return removed, nil
}

// TotalBytes 是快照目录当前占用（只算归档，不算清单）。
func (m *Manager) TotalBytes() int64 {
	list, err := m.List()
	if err != nil {
		return 0
	}
	var sum int64
	for _, s := range list {
		sum += s.ArchiveBytes
	}
	return sum
}

// ---- 等存档落盘稳定（崩溃一致性的前置） ----

const (
	// DefaultSaveMinWait 保留原来"发完 c_save() 固定等 2 秒"的行为：命令进 screen
	// 之后引擎还要跑几 tick 才真正开写，采样太早会把"还没动"误判成"已经稳了"。
	DefaultSaveMinWait = 2 * time.Second
	// DefaultSaveMaxWait 是最多等 30 秒。超过就明确给警告，不假装存盘完成。
	DefaultSaveMaxWait = 30 * time.Second
	// savePollInterval 是两次采样的间隔；saveStableSamples 是"连续几次采样指纹
	// 完全一样才算落盘完成"（连续两次）。
	savePollInterval  = 500 * time.Millisecond
	saveStableSamples = 2
)

// SaveStableResult 是一次"等存档落盘"的结论。
//
// 关键约定：Stable=false 时 Warning 必非空——超时或采样失败绝不静默当成成功。
// 备份照旧打（磁盘上随时有一份自洽的旧档，见 worker 侧 flushSaves 的注释），
// 但这句警告要由调用方带进任务收尾消息/快照清单，让人知道这份档的时间点。
type SaveStableResult struct {
	Stable   bool   `json:"stable"`
	Waited   string `json:"waited"`
	WaitedMs int64  `json:"waitedMs"`
	Sampled  int    `json:"sampled"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	Warning  string `json:"warning,omitempty"`
	Note     string `json:"note,omitempty"`
}

func (r *SaveStableResult) finish(start time.Time) {
	r.WaitedMs = time.Since(start).Milliseconds()
	r.Waited = humanSaveWait(time.Since(start))
}

// saveFingerprint 把各分片存档目录里所有普通文件的 (路径, 大小, mtime) 拼成指纹。
// 只看 save/ 目录：引擎存一次档会同时改好几个文件（world、session、快照档位），
// 盯单一文件会在"这一个刚稳、下一个还在写"时就误判完成。
// 遍历途中被引擎淘汰掉的文件（自动存档滚动）按"这一轮不算数"跳过，不报错。
func saveFingerprint(paths dst.Paths, cluster string) (string, int, int64, error) {
	var b strings.Builder
	files := 0
	var total int64
	for _, shard := range paths.ShardNames(cluster) {
		root := paths.SaveDir(cluster, shard)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s/%s:%d:%d|", shard, filepath.ToSlash(rel),
				info.Size(), info.ModTime().UnixNano())
			files++
			total += info.Size()
			return nil
		})
		if err != nil {
			if os.IsNotExist(err) {
				continue // 这个分片还没生成过存档
			}
			return "", 0, 0, err
		}
	}
	return b.String(), files, total, nil
}

// WaitSaveStable 在下发 c_save() 之后等存档真正落盘。
//
// 原来只固定等 2 秒就开打包：引擎慢一点、或存档正写到一半，快照就可能是
// "半份新档 + 半份旧档"。这里改成在保存窗口内反复采样存档目录的 size+mtime
// 指纹，连续 saveStableSamples 次一模一样才认为写完；到 maxWait 还没稳就返回
// Stable=false + 明确的 Warning，由调用方把这句话带进任务收尾/清单，而不是假装成功。
//
// 这一层只管文件、不碰进程（同本包分工）：c_save() 仍由 worker 触发，ctx 用任务的
// 那一个，面板退出时能立刻收手。调用点在 worker 的 flushSaves——本包导出这个函数，
// 由那边把"固定等 2 秒"换成"等稳定（最短仍 2 秒、最多 30 秒）"。
func WaitSaveStable(ctx context.Context, paths dst.Paths, cluster string,
	minWait, maxWait time.Duration) SaveStableResult {

	if minWait < 0 {
		minWait = 0
	}
	if maxWait <= 0 {
		maxWait = DefaultSaveMaxWait
	}
	if minWait > maxWait {
		minWait = maxWait
	}
	start := time.Now()
	res := SaveStableResult{}

	// 最短等待：保留原 2 秒行为，别在引擎还没开写时就采到一份"假稳定"。
	if minWait > 0 {
		select {
		case <-ctx.Done():
			res.Warning = "等存档落盘时任务被取消，按磁盘上现有的档打包（这次没有等到 c_save 落盘完成）"
			res.finish(start)
			return res
		case <-time.After(minWait):
		}
	}

	prev := ""
	stable := 0
	for {
		if time.Since(start) >= maxWait {
			res.Warning = fmt.Sprintf("等了 %s 存档文件仍在变化，这份备份可能不是刚存完的那一份："+
				"按磁盘上现有的档打包，建议停服后再备份一次", humanSaveWait(time.Since(start)))
			res.finish(start)
			return res
		}
		fp, files, bytes, err := saveFingerprint(paths, cluster)
		res.Sampled++
		res.Files, res.Bytes = files, bytes
		if err != nil {
			res.Warning = "采样存档目录失败：" + err.Error() + "；按磁盘上现有的档打包，这份可能不是刚存完的那一份"
			res.finish(start)
			return res
		}
		if stable > 0 && fp == prev {
			stable++
		} else {
			stable = 1
		}
		prev = fp
		if stable >= saveStableSamples {
			res.Stable = true
			res.finish(start)
			if files == 0 {
				res.Note = "存档目录里没有文件（这个房间可能还没生成过存档），没有可等的落盘动作"
			}
			return res
		}
		select {
		case <-ctx.Done():
			res.Warning = "等存档落盘时任务被取消，按磁盘上现有的档打包（这次没有等到 c_save 落盘完成）"
			res.finish(start)
			return res
		case <-time.After(savePollInterval):
		}
	}
}

func humanSaveWait(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d 毫秒", d.Milliseconds())
	}
	return fmt.Sprintf("%.0f 秒", d.Seconds())
}
