// 归档器：把引擎日志里的玩家进出与死亡攒进库，供总览那张时间线用。
//
// 为什么不复用事件页那条现读现解的路：事件页读的是每份文件最近 500 行（约半小时），
// 而引擎每轮启动会把日志截断重写——世界重启一次，上一轮"谁几点进来"就永久没了。
// 时间线要的是跨轮、跨天的历史，所以必须有一份落库的抄本。
//
// 为什么是"续读"而不是每轮重读尾巴：日志是一行一行追加的，按上次的位置读新增那一段
// 是常数代价；每轮重读 500 行看着不多，但去重全靠库里的唯一键，重复解析的量会一直摊在 CPU 上。
// 第一次见到某个分片时才回看一个窗口（不从文件头读起，那是一整份日志），
// 之后只读追加的部分；面板重启后位置丢了就再回看一个窗口，重复的行由唯一键忽略掉。
package monitor

import (
	"errors"
	"log/slog"
	"os"
	"time"

	"dstpanel/internal/dst"
	"dstpanel/internal/gamelog"
	"dstpanel/internal/store"
)

// 第一次见到一个分片时回看的行数。用事件页那个默认值而不是自己定一个数：
// 两处都是"刚接手时先补一段近史"，同一个尺度更容易解释。
const firstWindow = gamelog.EventDefaultLines

// seenPlayerScanLimit 是取花名册时的条数上限，直接用到 store 层允许的最大值：
// 这里要的是"这个房间见过的所有人都能接上编号"，少了谁那条泳道就退回按名字认人。
const seenPlayerScanLimit = 2000

type Archiver struct {
	Paths  dst.Paths
	Store  *store.Store
	Logger *slog.Logger

	logs  *gamelog.Reader
	since map[string]gamelog.Since
}

// ArchiveResult 是一轮归档的账：写了多少、为什么没写。
//
// 两个 skipped 都必须存在并上得了日志：没有绝对时刻的行（这份日志里没有 Current time 行）
// 和认不出是谁的行（进出公告只有一句没有名字），都是"时间线上少了一段"的原因，
// 出了现场只报"写了 0 条"就分不出是世界没开、还是面板读不懂这份日志。
type ArchiveResult struct {
	Written      int
	Read         int
	SkippedNoWho int
	SkippedNoAt  int
}

func (a *Archiver) log() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

func (a *Archiver) init() {
	if a.logs == nil {
		a.logs = gamelog.NewReader(a.Paths)
		a.since = map[string]gamelog.Since{}
	}
}

// ArchiveOnce 走一遍这台机器上的每个分片。返回写了多少条。
func (a *Archiver) ArchiveOnce() int {
	r := a.Archive()
	return r.Written
}

func (a *Archiver) Archive() ArchiveResult {
	a.init()
	names, err := a.Paths.ClusterNames()
	if err != nil {
		a.log().Error("归档玩家行为时枚举房间失败", "err", err.Error())
		return ArchiveResult{}
	}
	var total ArchiveResult
	for _, cluster := range names {
		links := a.nameLinks(cluster)
		for _, shard := range a.Paths.ShardNames(cluster) {
			got, err := a.archiveShard(cluster, shard, links)
			if err != nil {
				// 世界从没跑过就没有日志文件，这是常态不是故障：降到 Debug，
				// 否则每台新机器一开机就往日志里灌错误行。
				a.log().Debug("这一轮没归档到玩家行为",
					"cluster", cluster, "shard", shard, "err", err.Error())
				continue
			}
			total.Written += got.Written
			total.Read += got.Read
			total.SkippedNoWho += got.SkippedNoWho
			total.SkippedNoAt += got.SkippedNoAt
		}
	}
	if total.SkippedNoAt > 0 || total.SkippedNoWho > 0 {
		a.log().Debug("有些行为行没能定位到人或时刻",
			"没有绝对时刻", total.SkippedNoAt, "认不出是谁", total.SkippedNoWho)
	}
	return total
}

// nameLinks 取出这个房间花名册里的"显示名 → 账户编号"，给归档行补上账户编号。
//
// 为什么入库时就接上：引擎的进出公告只带显示名（实测聊天文件那行是
// `[Join Announcement] 风萧萧`，没有 KU_ 编号），而花名册是助手快照写的、两个标识都有。
// 不接这一步，时间线就只能按名字认人——玩家改一次名，同一个人会被画成两条不相干的泳道，
// 而界面上看不出来那是同一个人。查不到就留空：把别人的编号安过来比空着严重得多。
func (a *Archiver) nameLinks(cluster string) map[string]string {
	players, err := a.Store.ListSeenPlayers(cluster, seenPlayerScanLimit)
	if err != nil {
		a.log().Debug("这一轮没读到花名册，归档的行为行只按显示名认人",
			"cluster", cluster, "err", err.Error())
		return map[string]string{}
	}
	out := make(map[string]string, len(players))
	for _, p := range players {
		if p.UserID == "" || p.Name == "" {
			continue
		}
		if _, ok := out[p.Name]; !ok {
			out[p.Name] = p.UserID
		}
	}
	return out
}

// archiveShard 读一个分片的两份文件里新增的行，把玩家行为写进库。
//
// 服务端日志先读：本轮启动的绝对时刻（Current time 行）只在那一份里，
// 聊天文件里的行要靠它换算成时刻——这和事件页借的是同一条线索。
func (a *Archiver) archiveShard(cluster, shard string, links map[string]string) (ArchiveResult, error) {
	var res ArchiveResult
	server, err := a.read(cluster, shard, gamelog.KindServer, time.Time{})
	if err != nil {
		return res, err
	}
	roundStart := server.Start
	rows := a.rows(cluster, shard, gamelog.KindServer, server, roundStart, links, &res)

	chat, err := a.read(cluster, shard, gamelog.KindChat, roundStart)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// 聊天文件还没写出来是正常现象（世界刚起来那一瞬），
		// 但服务端那份已经读到了，这一轮就不算失败。
		a.log().Debug("这一轮没读到聊天文件", "cluster", cluster, "shard", shard, "err", err.Error())
	} else if err == nil {
		rows = append(rows, a.rows(cluster, shard, gamelog.KindChat, chat, roundStart, links, &res)...)
	}
	if len(rows) == 0 {
		return res, nil
	}
	n, err := a.Store.RecordPlayerEvents(rows)
	if err != nil {
		return res, err
	}
	res.Written = n
	return res, nil
}

// read 读一份文件里新增的行，并记住下一次该从哪儿接着读。
// hint 是本轮启动时刻：聊天文件自己没有那行 Current time，要从服务端那份借。
func (a *Archiver) read(cluster, shard string, kind gamelog.Kind, hint time.Time) (*gamelog.Snapshot, error) {
	key := cluster + "\x00" + shard + "\x00" + string(kind)
	prev, seen := a.since[key]
	if !seen {
		snap, err := a.logs.Tail(cluster, shard, kind, firstWindow)
		if err != nil {
			return nil, err
		}
		a.since[key] = snap.Since()
		return snap, nil
	}
	if prev.Start.IsZero() {
		prev.Start = hint
	}
	snap, err := a.logs.ReadSince(cluster, shard, kind, prev)
	if err != nil {
		return nil, err
	}
	a.since[key] = snap.Since()
	return snap, nil
}

// rows 把一次读取里的玩家行为转成入库行。
//
// 没有绝对时刻的行直接不要：引擎的时钟是从本轮启动算起的相对值，
// 换算不出时刻就只能落在"不知道是几点"的地方，那种行画进时间线里就是编。
// 名字和账户编号两个都凑不到的也不要——它说不上是谁。
func (a *Archiver) rows(cluster, shard string, kind gamelog.Kind, snap *gamelog.Snapshot,
	hint time.Time, links map[string]string, res *ArchiveResult) []store.PlayerEventRow {
	base := snap.Start
	if base.IsZero() {
		base = hint
	}
	var out []store.PlayerEventRow
	for _, e := range snap.Entries {
		if !store.IsTimelineEvent(e.Event) {
			continue
		}
		res.Read++
		// 行为行算在谁头上只看 Player：条目层的 Target 是被操作的那一位（踢人、封禁），
		// 那两个字段配不成"同一个人"，而这两类动作本来也不进这张表。
		userid := e.UserID
		if userid == "" {
			userid = links[e.Player]
		}
		if userid == "" && e.Player == "" {
			res.SkippedNoWho++
			continue
		}
		if base.IsZero() {
			res.SkippedNoAt++
			continue
		}
		secs, ok := gamelog.ClockSeconds(e.Clock)
		if !ok {
			res.SkippedNoAt++
			continue
		}
		out = append(out, store.PlayerEventRow{
			At:      base.Add(time.Duration(secs) * time.Second).UTC(),
			Round:   base.UnixMilli(),
			Cluster: cluster,
			Shard:   shard,
			UserID:  userid,
			Name:    e.Player,
			Event:   e.Event,
			Source:  string(kind),
			Clock:   e.Clock,
		})
	}
	return out
}
