// Package monitor 是面板的后台采样与保留期清理。
//
// 为什么必须有个后台进程而不是"界面刷新时才读"：趋势图要的是等间隔采样，
// 按需读会得到一串以用户点击密度为横轴的点；而且面板重启期间没人点，
// 那段时间的数据就永久丢了。
//
// 另一件事是保留期：在线快照和操作记录都是只增的表，不清理迟早把 4G 小机的
// 磁盘和查询一起拖死。清理周期由配置给，具体保留天数在设置里可改。
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"dstpanel/internal/dst"
	"dstpanel/internal/hoststats"
	"dstpanel/internal/modbridge"
	"dstpanel/internal/store"
)

type Sampler struct {
	Paths  dst.Paths
	Bridge *modbridge.Bridge
	Store  *store.Store
	Logger *slog.Logger

	// Events 是玩家行为归档器。留 nil 就不跑（测试夹具、以及不想动日志文件的场合）。
	Events *Archiver

	// Host 是整机运行状态采集器（CPU/内存/磁盘）。和 Supervisor 一起接上时，
	// 每轮采样额外持久化一条主机指标并判阈值；留 nil 就只跑原来的在线人数快照。
	Host *hoststats.Collector
	// Supervisor 是采样落盘、阈值告警、Webhook 与崩溃自愈共用的共享状态。
	// 留 nil 时这一整块都不跑（测试夹具就靠这个开关保持原行为）。
	Supervisor *Supervisor

	// Interval 是采样间隔；<=0 时用 60 秒。
	Interval time.Duration
	// CleanupEvery 是保留期清理的间隔；<=0 时用 6 小时。
	CleanupEvery time.Duration
	// Now 供测试替换。
	Now func() time.Time
}

func (s *Sampler) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Sampler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sampler) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return time.Minute
}

func (s *Sampler) cleanupEvery() time.Duration {
	if s.CleanupEvery > 0 {
		return s.CleanupEvery
	}
	return 6 * time.Hour
}

// Run 一直采样到 ctx 结束。首次立即采一轮，免得面板起来后要空等一个间隔。
//
// 归档和采样同一个节拍：进出事件是分钟级的东西，跟着采样循环跑就够，
// 另开一个 ticker 只会多一处"面板起来后要等满一个间隔"。
func (s *Sampler) Run(ctx context.Context) {
	s.SampleOnce()
	s.archive()
	s.CleanupOnce()
	// 第一轮心跳只把「现在哪些分片在跑」记下来，不拿它当上一轮做崩溃判定，
	// 否则面板刚起来的这一刻会把所有没在跑的分片报成崩溃。
	s.supervise(ctx)

	ticker := time.NewTicker(s.interval())
	defer ticker.Stop()
	cleanupAt := s.now().Add(s.cleanupEvery())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SampleOnce()
			s.archive()
			s.supervise(ctx)
			if !s.now().Before(cleanupAt) {
				s.CleanupOnce()
				cleanupAt = s.now().Add(s.cleanupEvery())
			}
		}
	}
}

func (s *Sampler) archive() {
	if s.Events != nil {
		s.Events.ArchiveOnce()
	}
}

// supervise 是崩溃检测心跳。没接 Supervisor 时（测试夹具、裁剪版）整块跳过。
func (s *Sampler) supervise(ctx context.Context) {
	if s.Supervisor != nil {
		s.Supervisor.CheckShards(ctx)
	}
}

// SampleOnce 把每个分片的当前状态各存一行，返回存了几行。
//
// 只存有把握的数据：读不到、已过期、MOD 采集本身报错的快照一律不入库。
// 把"助手卡死时的最后一帧"当成真实数据存进去，趋势图上就是一条假装平稳的横线，
// 那种图比缺一个点更误导人。
func (s *Sampler) SampleOnce() int {
	names, err := s.Paths.ClusterNames()
	if err != nil {
		s.log().Error("采样时枚举集群失败", "err", err.Error())
		return 0
	}
	written := 0
	for _, cluster := range names {
		for _, shard := range s.Paths.ShardNames(cluster) {
			st, err := s.Bridge.Status(cluster, shard)
			if err != nil {
				continue
			}
			if st.Stale || st.Error != "" {
				s.log().Debug("跳过不可信的数据",
					"cluster", cluster, "shard", shard, "stale", st.Stale, "collectError", st.Error)
				continue
			}
			sn := store.Snapshot{
				At:      s.now(),
				Cluster: cluster, Shard: shard,
				Day: int(st.World.DayNumber()), Season: st.World.Season, Online: st.PlayerCount,
				Detail: playerDetailJSON(st),
			}
			if err := s.Store.RecordSnapshot(sn); err != nil {
				s.log().Error("写入在线人数记录失败", "cluster", cluster, "shard", shard, "err", err.Error())
				continue
			}
			// 花名册和趋势图同源：都用这一份"可信且新鲜"的快照。
			// 采样间隔是一分钟，所以名字/角色最多晚一分钟更新，够用且不会把库写热。
			if err := s.Store.UpsertSeenPlayers(cluster, shard, seenPlayersOf(st), s.now()); err != nil {
				s.log().Error("写入玩家花名册失败", "cluster", cluster, "shard", shard, "err", err.Error())
			}
			written++
		}
	}
	// 在线人数快照之外，另外存一条整机指标并判阈值。这是新增的一条数据线，
	// 不影响上面 written 的语义（测试里 Supervisor 为 nil，这一步整个跳过）。
	if s.Supervisor != nil && s.Host != nil {
		s.Supervisor.Collect(s.now(), s.Host.Stats())
	}
	return written
}

// playerDetailJSON 存玩家名列表。留 JSON 而不是建表：
// 这列只在"看某一天谁在线"时整块取出来显示，没有按玩家查询的需求，
// 为它建表就要维护一份和快照同生命周期的一行行数据。
func playerDetailJSON(st *modbridge.Status) string {
	if len(st.Players) == 0 {
		return "[]"
	}
	names := make([]string, 0, len(st.Players))
	for _, p := range st.Players {
		if p.IsDead {
			continue
		}
		names = append(names, p.Name)
	}
	raw, err := json.Marshal(names)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// seenPlayersOf 把快照里的玩家转成花名册行。死掉的玩家也记：
// 花名册回答的是"这个人来过没有"，不是"现在活着没有"。
func seenPlayersOf(st *modbridge.Status) []store.SeenPlayer {
	out := make([]store.SeenPlayer, 0, len(st.Players))
	for _, p := range st.Players {
		if p.UserID == "" {
			continue
		}
		out = append(out, store.SeenPlayer{UserID: p.UserID, Name: p.Name, Prefab: p.Prefab})
	}
	return out
}

// CleanupReport 是一轮保留期清理各删了多少。
//
// 三样分开数而不是给一个总数：三张表的保留天数是三个独立设置项，
// "清了 4000 条"说不出清的是哪种，而界面和日志都要能说清。
type CleanupReport struct {
	Snapshots    int
	Audit        int
	PlayerEvents int
}

// CleanupOnce 按设置里的保留天数裁掉过期的快照、操作记录与玩家行为。
// 保留天数设成 0 或负数表示"永久保留"，这时一个字都不删。
func (s *Sampler) CleanupOnce() (CleanupReport, error) {
	var out CleanupReport
	var errs []error

	purge := func(key string, fn func(time.Time) (int, error), dst *int) {
		cutoff, err := s.Store.RetentionCutoff(key, s.now())
		if err != nil {
			errs = append(errs, err)
			return
		}
		if cutoff.IsZero() {
			return
		}
		n, err := fn(cutoff)
		if err != nil {
			errs = append(errs, err)
			return
		}
		*dst = n
	}

	purge(store.KeySnapshotRetentionDays, s.Store.PurgeSnapshots, &out.Snapshots)
	purge(store.KeyAuditRetentionDays, s.Store.PurgeAudit, &out.Audit)
	purge(store.KeyPlayerEventRetentionDays, s.Store.PurgePlayerEvents, &out.PlayerEvents)

	if len(errs) > 0 {
		return out, fmt.Errorf("保留期清理有 %d 处失败，首先：%w", len(errs), errs[0])
	}
	if out.Snapshots > 0 || out.Audit > 0 || out.PlayerEvents > 0 {
		s.log().Info("保留期清理完成", "snapshots", out.Snapshots,
			"audit", out.Audit, "playerEvents", out.PlayerEvents)
	}
	return out, nil
}
