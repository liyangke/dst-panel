// 存档快照走任务队列，这是量出来的结论而不是猜的：
// 云机上 Master/save 73MB + Caves/save 66MB（解压前 137.9MB），打成包是 19.0MB，
// 打包要几十秒，恢复还要多一遍解压加一遍逐文件哈希。把这些挂在 HTTP 请求上等着做完，
// 等于把"备份到底成功没有"押在浏览器和反向代理的超时上——而超时切断之后，
// 磁盘上的那份快照该写完还是写完了，用户却只看到一个转圈的按钮。
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"dstpanel/internal/backup"
	"dstpanel/internal/dst"
	"dstpanel/internal/store"
)

const (
	// KindBackupCreate 给一个房间打一份存档快照。
	KindBackupCreate = "backup.create"
	// KindBackupRestore 用一份快照换掉房间现在的存档目录。
	KindBackupRestore = "backup.restore"
	// KindBackupImport 把用户上传的外部存档包变成一份快照，可选顺手换掉房间存档。
	KindBackupImport = "backup.import"
)

// consoleKeySave 是引擎的"立即存盘"白名单命令（dst.ConsoleCommandSpec 里的 save → c_save()）。
const consoleKeySave = "save"

// saveFlushWait 是下发 c_save() 之后留给引擎落盘的时间。
// 命令进 screen 之后还要跑几 tick 才真正写完档，不等就等于备份了存盘前的世界。
const saveFlushWait = 2 * time.Second

// BackupCreatePayload 是备份任务的参数。
//
// 结构体在这个包里定义、由接口层构造：请求体里没有 payload 这个字段，
// 用户能影响的只有房间名，而它在入队前已经按磁盘上的集群名单校验过。
type BackupCreatePayload struct {
	Cluster string `json:"cluster"`
	// Label 是快照来源（手工 / 定时），来自面板而不是浏览器。
	Label string `json:"label"`
}

// BackupRestorePayload 是恢复任务的参数。
type BackupRestorePayload struct {
	SnapshotID string `json:"snapshotId"`
	// TargetCluster 为空表示恢复回快照自己的房间；非空就是跨房间迁移。
	// 两个值都在提交时按磁盘上的集群名单校验过，任务里这份只是把它们带给 worker。
	TargetCluster string `json:"targetCluster,omitempty"`
}

// BackupImportPayload 是导入任务的参数。
type BackupImportPayload struct {
	// Package 是上传包的编号，由面板在落地那一刻生成（见 backup/staging.go）。
	// 这里绝不放客户端给的文件名，更不放路径：编号要参与拼出读哪个文件，
	// 而临时目录里可能有别人手工拷进去的东西。
	Package string `json:"package"`
	// Cluster 是这份外部存档登记给哪个房间，提交时按磁盘上的集群名单核过。
	Cluster string `json:"cluster"`
	// Restore 决定导入完要不要当场换掉这个房间的存档目录。
	// 它只影响要不要接着走 runRestore，不影响导入本身——导入只写备份目录。
	Restore bool `json:"restore,omitempty"`
}

// backupCreate 打一份快照。
func (w *Worker) backupCreate(ctx context.Context, t store.Task, report ProgressFn) error {
	if w.Backups == nil {
		return fmt.Errorf("面板没接上存档备份模块：存档目录可能不可写，确认后重启面板")
	}
	var p BackupCreatePayload
	if err := t.PayloadInto(&p); err != nil {
		return fmt.Errorf("备份任务的参数读不出来，可能是任务表被改过: %w", err)
	}
	if err := w.checkBackupCluster(p.Cluster); err != nil {
		return err
	}
	label := strings.TrimSpace(p.Label)
	if label == "" {
		label = backup.LabelManual
	}

	// 这句要一直带到收尾消息里：它说的是"这份快照是哪个时刻的世界"，
	// 只报在进度中间就会被完成那条覆盖掉，而用户回看任务中心看的是最后一条。
	flushNote := w.flushSaves(ctx, p.Cluster, report)
	if flushNote != "" {
		report(5, flushNote, nil)
	}
	total := 0
	progress := func(done, all int) {
		total = all
		// 每个条目都写一次库会把单连接的写锁占满，而 137 个条目报 137 次也没人看。
		if all == 0 || (done != all && done%10 != 0) {
			return
		}
		report(percentOf(done, all), fmt.Sprintf("已打包 %d/%d 个条目", done, all), nil)
	}
	snap, err := w.Backups.CreateWithProgress(ctx, p.Cluster, label, progress)
	if err != nil {
		return err
	}
	done := fmt.Sprintf("备份 %s 已生成：%d 个文件、包 %s、解压前 %s",
		snap.ID, snap.FileCount, humanBytes(snap.ArchiveBytes), humanBytes(snap.UncompressedBytes))
	if total > 0 && snap.FileCount > 0 {
		done = fmt.Sprintf("备份 %s 已生成：%d 个文件（连目录共 %d 个条目）、包 %s、解压前 %s",
			snap.ID, snap.FileCount, total, humanBytes(snap.ArchiveBytes), humanBytes(snap.UncompressedBytes))
	}
	if snap.RestoreNote != "" {
		// 打完却不能用来恢复，必须在这里喊出来：那多半是这个房间还没生成过存档。
		done = done + "；但这份备份不能用来恢复：" + snap.RestoreNote
	}
	if flushNote != "" {
		done = addSentence(done, flushNote)
	}
	if snap.Note != "" {
		done = addSentence(done, snap.Note)
	}
	report(100, done, nil)
	return nil
}

// flushSaves 让还在跑的分片先落一次盘，返回要告诉用户的一句话（无需告诉时为空）。
//
// 为什么不停服也敢备份：引擎只在存盘时写档，磁盘上随时都有一份自洽的旧档；
// 先 c_save() 是把"最近几分钟的动作"也带进快照。代价是备份期间引擎还在写，
// 那由快照自己的 Note 说清楚（哪些文件打包时还在变）。
// 存盘失败不阻断备份：磁盘上这份档仍然是能恢复的档，只是可能少几分钟进度，
// 为了这句话让整个备份失败反而会把人推向"直接 cp save 目录"那种更危险的做法。
// 但"问不到进程状态"要单独说：把它算成"没在跑"就等于替面板撒了一个谎，
// 而用户看到的那句"没有在跑的分片"决定了他要不要信这份档带上刚才那几分钟。
func (w *Worker) flushSaves(ctx context.Context, cluster string, report ProgressFn) string {
	if w.Procs == nil {
		return "面板没接上进程驱动，问不到世界是否在跑，跳过了存盘命令，打包的是磁盘上现有的档"
	}
	var flushed, failed, unknown []string
	for _, shard := range w.Paths.ShardNames(cluster) {
		running, err := w.Procs.IsRunning(ctx, cluster, shard)
		if err != nil {
			unknown = append(unknown, shard)
			continue
		}
		if !running {
			continue
		}
		if _, err := w.Procs.RunAdminCommand(ctx, cluster, shard, consoleKeySave, nil); err != nil {
			failed = append(failed, shard)
			continue
		}
		flushed = append(flushed, shard)
	}
	unknownNote := "问不到 %s 的进程状态（面板要在服务端同机运行，PATH 里还得有 screen）"
	note := "这个房间没有在跑的世界，打包的是磁盘上现有的档"
	switch {
	case len(flushed) > 0:
		// 命令进 screen 之后引擎还要跑几 tick 才真正写完，等落盘稳定再开始遍历目录。
		// 不再固定睡 saveFlushWait 两秒：那时存档可能还在写，打包的是半份档。
		// waitSaveStable 保留 2 秒最短等待，最多 30 秒，期间反复采样存档目录的
		// size+mtime 指纹，连续两次不变才认为写完了；超时的警告必须带进收尾消息。
		report(2, fmt.Sprintf("已让 %s 立即存盘，等引擎落盘", strings.Join(flushed, "、")), nil)
		stableNote := w.waitSaveStable(ctx, cluster)
		// 这句还要带回收尾消息：只报在进度里会被"完成"那条覆盖，
		// 而用户事后看任务中心时最想知道的就是这份档有没有带上最后几分钟。
		note = fmt.Sprintf("已让 %s 立即存盘后再打包", strings.Join(flushed, "、"))
		if stableNote != "" {
			note += "；" + stableNote
		}
		if len(failed) > 0 {
			note += fmt.Sprintf("；%s 存盘命令没发出去（多半是助手或 screen 不通），那部分打包的是磁盘上现有的档",
				strings.Join(failed, "、"))
		}
		if len(unknown) > 0 {
			note += "；" + fmt.Sprintf(unknownNote+"，那部分没下发存盘命令", strings.Join(unknown, "、"))
		}
	case len(unknown) > 0:
		note = fmt.Sprintf(unknownNote+"，没下发存盘命令，打包的是磁盘上现有的档", strings.Join(unknown, "、"))
	case len(failed) > 0:
		note = fmt.Sprintf("世界在跑但存盘命令没发出去（%s），打包的是磁盘上现有的档", strings.Join(failed, "、"))
	}
	return note
}

// backupRestore 用快照换掉房间现在的存档目录。
func (w *Worker) backupRestore(ctx context.Context, t store.Task, report ProgressFn) error {
	var p BackupRestorePayload
	if err := t.PayloadInto(&p); err != nil {
		return fmt.Errorf("恢复任务的参数读不出来，可能是任务表被改过: %w", err)
	}
	// 空的目标房间表示"恢复回快照自己的房间"，这句留给 runRestore 判——
	// 它反正要读一次清单才能说清"这份备份能不能用"，这里再读一次纯属重复。
	return w.runRestore(ctx, p.SnapshotID, strings.TrimSpace(p.TargetCluster), report)
}

// runRestore 是"把一份快照变成房间现在的世界"的唯一入口：恢复任务和导入任务都走它。
//
// 之所以要共用：这两条路改的是同一块磁盘，而改错的代价都是丢世界。
// 导入那里如果自己写一遍"停世界检查 + 换目录 + 失败后怎么说"，
// 迟早会出现一份漏了停世界检查的版本。
//
// target 为空表示恢复到快照自己的房间。
// report 由调用方给：导入任务会把它包成一个只占后段百分比的包装，
// 所以这里照自己的节奏报 1 和 100。
func (w *Worker) runRestore(ctx context.Context, snapshotID, target string, report ProgressFn) error {
	if w.Backups == nil {
		return fmt.Errorf("面板没接上存档备份模块（备份目录没配好），先检查配置里 paths.workDir 是否可写")
	}
	if err := backup.CheckID(snapshotID); err != nil {
		return err
	}
	snap, err := w.Backups.Get(snapshotID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(target) == "" {
		target = snap.Cluster
	}
	if !snap.Restorable {
		// 这一句同样要说清"什么都没动"：这条分支在 Restore 之前，
		// 而用户看到"不能用来恢复"时最想知道的就是那间屋子现在还算不算数。
		return fmt.Errorf("这份备份不能用来恢复：%s；现有存档一点都没动，这个房间可以直接启动", snap.RestoreNote)
	}
	if err := w.checkBackupCluster(target); err != nil {
		return err
	}
	// 要停的世界 = 目标房间现有的 + 快照里带存档的。后者可能更多：
	// 跨房间迁移时目标房间还没有 Caves，而恢复要新建它，此时目标房间"现有"的世界名单里查不到。
	if err := w.checkShardsStopped(ctx, target, mergeShards(w.Paths.ShardNames(target), snap.SaveShards)); err != nil {
		return err
	}

	report(1, fmt.Sprintf("开始恢复 %s（%s 的房间 %s）：先整包校验，再自动打一份恢复前的安全备份",
		snapshotID, humanBytes(snap.ArchiveBytes), target), nil)
	res, err := w.Backups.Restore(ctx, snapshotID, backup.Options{TargetCluster: target})
	if err != nil {
		// 四种失败说的是四件不同的事，混成一句"恢复失败"就是让人拿猜测去决定要不要起服。
		switch res.Aftermath {
		case backup.AftermathUntouched:
			// 校验没过、安全备份没成、参数不合法都走这里：磁盘还是原样。
			note := "现有存档一点都没动，这个房间可以直接启动"
			if res.SafetyBackupID != "" {
				note = "现有存档一点都没动，可以直接启动；多打的那份恢复前安全备份是 " + res.SafetyBackupID
			}
			return fmt.Errorf("%w；%s", err, note)
		case backup.AftermathRolledBack:
			return fmt.Errorf("%w；原存档已经换回原位，世界还是恢复前的样子，可以重新启动世界", err)
		case backup.AftermathRestored:
			return fmt.Errorf("%w；世界里已经是这份备份的内容了（那部分逐个校验通过），世界可以启动，"+
				"只是移开旧存档用的中间目录没删干净，人工确认后再删", err)
		default:
			// 回滚没做成（或将来新增的失败位置没标状态）是这套流程里最糟的一种：
			// 现场既不是旧的也不是新的。这里必须把"别启动"喊清楚，并指出退路是那份安全备份。
			guard := "恢复前的安全备份也没打成"
			if res.SafetyBackupID != "" {
				guard = fmt.Sprintf("恢复前那份安全备份还在（%s），先别启动这个房间的世界，人工确认存档目录后再决定", res.SafetyBackupID)
			}
			return fmt.Errorf("%w；而且没能自动换回原存档：%s", err, guard)
		}
	}
	out := fmt.Sprintf("已恢复 %d 个文件并逐个比对哈希通过；存档目录被整棵换新的世界：%s；恢复前的安全备份：%s",
		res.WrittenFiles, strings.Join(res.ReplacedShards, "、"), res.SafetyBackupID)
	if res.Note != "" {
		out = addSentence(out, res.Note)
	}
	out = addSentence(out, "世界是停着的，启动后才有人能进；进去前想再确认一眼，就看备份列表里来源写着「恢复前的安全备份」那一份")
	report(100, out, nil)
	return nil
}

// backupImport 把用户上传的存档包变成一份快照，参数里要恢复的话就当场接着换进房间。
//
// 这一层做的事正好是 backup 包不做的那两件：问世界在不在跑，以及把两段进度说成人话。
func (w *Worker) backupImport(ctx context.Context, t store.Task, report ProgressFn) error {
	if w.Backups == nil {
		return fmt.Errorf("面板没接上存档备份模块（备份目录没配好），先检查配置里 paths.workDir 是否可写")
	}
	var p BackupImportPayload
	if err := t.PayloadInto(&p); err != nil {
		return fmt.Errorf("导入任务的参数读不出来，可能是任务表被改过: %w", err)
	}
	pkgPath, err := w.Backups.StagedPath(p.Package)
	if err != nil {
		return err
	}
	if err := w.checkBackupCluster(p.Cluster); err != nil {
		return err
	}
	// 顺手收一次过期的临时上传：指望人回这一页点"清理"是不现实的，
	// 而这条任务本身就是要往这个目录里东西的时刻，最该顺手收一次。
	if gone := w.Backups.SweepStaged(backup.StagedUploadTTL); len(gone) > 0 {
		w.log().Info("导入时顺手清理了过期的上传临时包", "count", len(gone))
	}

	// 解包和打包各占一段，是因为这两步的用户可见含义不同（见 backup.ImportOptions）。
	unpackCeil, packCeil := 45, 70
	var lastTotal int
	opts := backup.ImportOptions{
		Cluster: p.Cluster,
		OnUnpack: func(done, all int) {
			lastTotal = all
			report(rangedPercent(done, all, 1, unpackCeil), fmt.Sprintf("已解开 %d/%d 个条目", done, all), nil)
		},
		OnPack: func(done, all int) {
			lastTotal = all
			report(rangedPercent(done, all, unpackCeil, packCeil), fmt.Sprintf("已打包 %d/%d 个条目", done, all), nil)
		},
	}
	snap, err := w.Backups.Import(ctx, pkgPath, opts)
	if err != nil {
		// 临时包留着不删：传一次几十 MB 不该因为包的结构判不过就重来一遍，
		// 用户按预览里那几条原因把包修好再点一次就行。过期由上面的顺手清理收。
		return fmt.Errorf("%w；导入没做成，房间 %s 里的存档一点都没动", err, p.Cluster)
	}
	// 快照已经在磁盘上了，这时删临时包失败只算没收拾干净，不该把整条任务判成失败。
	if err := w.Backups.DropStaged(p.Package); err != nil {
		w.log().Warn("导入完成后没删掉上传的临时包", "ref", p.Package, "err", err.Error())
	}
	done := fmt.Sprintf("外部存档 %s 已变成快照 %s：%d 个文件、包 %s、解压前 %s（连目录共 %d 个条目）",
		p.Package, snap.ID, snap.FileCount, humanBytes(snap.ArchiveBytes), humanBytes(snap.UncompressedBytes), lastTotal)
	if snap.RestoreNote != "" {
		return fmt.Errorf("%s；但这份快照不能用来恢复：%s", done, snap.RestoreNote)
	}
	if !p.Restore {
		done = addSentence(done, "它已经和本机备份一样可以下载、校验和恢复；要现在换进房间，点这份快照的恢复")
		report(100, done, nil)
		return nil
	}
	// 换目录这一段接着导入的百分比往后报，否则进度条会从 70% 倒退回 1%——
	// 用户看到的"倒退"就是"是不是又从头来了一遍"。
	base := packCeil
	if err := w.runRestore(ctx, snap.ID, p.Cluster, shiftReport(report, base)); err != nil {
		return fmt.Errorf("%s；做成快照之后的恢复这一步失败了：%w", done, err)
	}
	return nil
}

// rangedPercent 把 done/all 映射进 [low, high]，并且永远不到 100。
// 上限留给真正做完那一步（写清单、换存档目录）报，理由同 percentOf。
func rangedPercent(done, all, low, high int) int {
	if all <= 0 {
		return low
	}
	span := high - low
	if span < 1 {
		return low
	}
	p := low + done*span/all
	if p > high-1 {
		return high - 1
	}
	if p < low {
		return low
	}
	return p
}

// shiftReport 把一个子阶段的百分比压进 [base, 100]：子阶段照自己的节奏报 1 和 100，
// 而它在整条任务里只占尾段。
func shiftReport(report ProgressFn, base int) ProgressFn {
	return func(percent int, message string, detail *store.TaskDetail) {
		if percent >= 100 {
			report(100, message, detail)
			return
		}
		p := base + (100-base)*percent/100
		if p > 99 {
			p = 99
		}
		report(p, message, detail)
	}
}

// checkBackupCluster 按磁盘上的集群名单校验。
// 这一层不是重复劳动：任务行是持久化的，面板重启后队列里那条 payload
// 已经和当时的磁盘状态隔了一段时间，房间可能早被删了。
func (w *Worker) checkBackupCluster(cluster string) error {
	if strings.TrimSpace(cluster) == "" {
		return fmt.Errorf("任务参数里没有集群名")
	}
	names, err := w.Paths.ClusterNames()
	if err != nil {
		return fmt.Errorf("读集群列表: %w", err)
	}
	if !containsStr(names, cluster) {
		return fmt.Errorf("集群 %s 现在不存在，现有：%s", cluster, strings.Join(names, "、"))
	}
	return nil
}

// checkShardsStopped 挡住"对着在跑的分片换存档目录"。
//
// 这里和更新服务端本体那一项相反，问不到进程状态时选择拦住而不是放行：
// 覆盖文件是可重复的动作，而换存档目录撞上引擎正在写档，
// 写回去的就是"半份旧档 + 半份新档"那种没人能解释的世界。
func (w *Worker) checkShardsStopped(ctx context.Context, cluster string, shards []string) error {
	if w.Procs == nil {
		// 没接进程驱动就等于"问不到"，而这里问不到必须当成"可能正在跑"：
		// 生产接线（main.go）一定会给它一份，走到这条分支说明面板本身没装好，
		// 那种时候最不该做的就是换存档目录。
		return fmt.Errorf("面板没接上进程驱动，问不到 %s 的世界是否在跑；恢复要换掉整棵存档目录，问不清就不动", cluster)
	}
	var running []string
	for _, shard := range shards {
		ok, err := w.Procs.IsRunning(ctx, cluster, shard)
		if err != nil {
			return fmt.Errorf("问不到 %s/%s 的进程状态：%v；恢复要换掉整棵存档目录，问不清就不动", cluster, shard, err)
		}
		if ok {
			running = append(running, cluster+"/"+shard)
		}
	}
	if len(running) > 0 {
		return fmt.Errorf("%w：%s。恢复存档要先把它们关掉——在跑的引擎会把刚恢复的内容按当前内存里的世界再存回去，"+
			"结果就是看起来没恢复成功", ErrShardsRunning, strings.Join(running, "、"))
	}
	return nil
}

func mergeShards(lists ...[]string) []string {
	set := map[string]bool{}
	for _, l := range lists {
		for _, v := range l {
			set[v] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func percentOf(done, total int) int {
	if total <= 0 {
		return 0
	}
	p := done * 100 / total
	if p > 99 {
		// 99 是上限：写完最后一个条目还要落盘、改名、写清单，那几步没算在这个数里，
		// 报成 100 会让用户以为已经可以点恢复了。
		return 99
	}
	if p < 1 {
		return 1
	}
	return p
}

// addSentence 往一句说明后面追加一条，避免出现"；；"或开头一个分号。
func addSentence(note, more string) string {
	if strings.TrimSpace(note) == "" {
		return more
	}
	if strings.TrimSpace(more) == "" {
		return note
	}
	return note + "；" + more
}

// ---- 定时备份 ----

// BackupScheduler 按间隔给每个有存档的房间入队一条定时备份。
//
// 它只入队，不自己打包：跑任务的始终是同一个 worker，
// 于是定时备份和手工备份天然串行，不会出现两份 gzip 同时写同一块盘。
type BackupScheduler struct {
	Store *store.Store
	Paths dst.Paths
	// BaseTick 是醒来的间隔，固定 10 分钟一次；真正做不做由设置里的间隔小时决定。
	// 这样做是因为间隔可以在面板设置里改，而 ticker 的周期在启动时就定死了。
	BaseTick time.Duration
	// Hours 取当前生效的间隔小时数：<=0 表示不开定时备份。
	Hours func() int
	// Now 只给测试用，用来判断"距上次跑够不够一个间隔"。
	Now func() time.Time
	// Logger 为空时用 slog.Default()。
	Logger *slog.Logger

	lastRun time.Time
}

func (sc *BackupScheduler) log() *slog.Logger {
	if sc.Logger != nil {
		return sc.Logger
	}
	return slog.Default()
}

func (sc *BackupScheduler) base() time.Duration {
	if sc.BaseTick > 0 {
		return sc.BaseTick
	}
	return 10 * time.Minute
}

func (sc *BackupScheduler) now() time.Time {
	if sc.Now != nil {
		return sc.Now()
	}
	return time.Now()
}

// Run 一直醒着到 ctx 结束。
//
// 面板启动后第一次真正备份要等满配置的间隔：如果一起来就打一份，
// 反复重启（升级、改配置、systemd 拉起）就变成了反复往盘上写几十 MB。
func (sc *BackupScheduler) Run(ctx context.Context) {
	if sc.Store == nil || sc.Hours == nil {
		return
	}
	sc.lastRun = sc.now()
	t := time.NewTicker(sc.base())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.Tick()
		}
	}
}

// Tick 判断这一轮该不该跑，该跑就每个房间入队一条。返回入队的房间数。
func (sc *BackupScheduler) Tick() int {
	hours := sc.Hours()
	if hours <= 0 {
		return 0
	}
	every := time.Duration(hours) * time.Hour
	if sc.now().Sub(sc.lastRun) < every {
		return 0
	}
	clusters, err := sc.Paths.ClusterNames()
	if err != nil {
		sc.log().Warn("定时备份读不到集群列表", "err", err.Error())
		return 0
	}
	var enqueued []string
	for _, cluster := range clusters {
		if !sc.hasSave(cluster) {
			// 房间建了但从没起过服：引擎还没有存档，打这份快照只会留下一条失败的定时任务。
			continue
		}
		title := fmt.Sprintf("定时备份存档 · %s", cluster)
		if _, err := sc.Store.CreateTask(KindBackupCreate, title,
			BackupCreatePayload{Cluster: cluster, Label: backup.LabelScheduled}); err != nil {
			sc.log().Warn("定时备份入队失败", "cluster", cluster, "err", err.Error())
			continue
		}
		enqueued = append(enqueued, cluster)
	}
	if len(enqueued) > 0 {
		sc.lastRun = sc.now()
		sc.log().Info("定时备份已入队", "clusters", strings.Join(enqueued, "、"), "every", every.String())
	}
	return len(enqueued)
}

// hasSave 看这个房间有没有任何一个分片生成过存档。
func (sc *BackupScheduler) hasSave(cluster string) bool {
	for _, shard := range sc.Paths.ShardNames(cluster) {
		st, err := os.Stat(sc.Paths.SaveDir(cluster, shard))
		if err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// humanBytes 把字节数说成人能读的 MB/GB。任务行的消息是给用户看的，
// "140751872 字节"这种写法等于让人自己做除法。
// GB 这一档目前只有"下载服务端本体"会走到（整包几 G），存档快照到不了这个量级。
// 单位和接口层那份一致（api 里的同名函数），两边不一致时界面和任务中心会各说一个数。
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "0 B"
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/float64(1<<30), 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/float64(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/float64(1<<10), 'f', 1, 64) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

// sortStrings 把分片名排好，报错里的名单顺序要稳定（同样两个分片在跑，
// 每次列出的顺序不一样会让人以为状态变了，也让界面没法逐字比较）。
func sortStrings(list []string) {
	sort.Strings(list)
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
