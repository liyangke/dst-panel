// 存档快照的接口层：列表、详情、下载、删除、校验，以及备份/恢复两个入口。
//
// 这一层的分工要说清楚，因为它是"错了就丢世界"那条链子上唯一面对用户的一段：
// 读接口（列表、详情、校验、下载）直接调 backup 包，只碰快照目录；
// 写接口（备份、恢复）只往任务队列里放一条带着已校验参数的任务就返回 202，
// 真正动存档目录的是 worker。所以在这些 handler 里没有任何一句"已经恢复好了"——
// 那必须是任务中心里的结论，界面拿到的是任务 id。
//
// 还有一条：payload 一律在这里构造。请求体里没有 payload 字段，
// 用户能给的只有房间名和快照编号，而这两个都先对着磁盘上的名单核过。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"dstpanel/internal/backup"
	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
	"dstpanel/internal/worker"
)

// backupsUnavailable 是接线漏了时的统一回话。
// 回 503 而不是 500：快照目录没配上不是面板崩溃，用户下一步该去改配置而不是重试。
func (s *Server) backupsUnavailable(w http.ResponseWriter) bool {
	if s.Backups != nil {
		return true
	}
	writeErr(w, http.StatusServiceUnavailable, "面板没接上存档备份模块：存档目录可能不可写，确认后重启面板")
	return false
}

// shardRuns 是一次 `screen -ls` 的结果。
//
// 为什么单独包一层：列表页要给每份快照标注"现在能不能恢复"，逐份问就是
// 逐次创建进程；而"问不清"（没装 screen）对备份和恢复的严重程度完全不同，
// 所以这里把会话名单和错误一起带住，由调用方按动作决定拦不拦。
type shardRuns struct {
	procs    *procrun.Driver
	sessions map[string]int
	err      error
}

func (s *Server) shardRuns(ctx context.Context) shardRuns {
	if s.Procs == nil {
		return shardRuns{err: errors.New("面板没接上进程驱动")}
	}
	sessions, err := s.Procs.Sessions(ctx)
	return shardRuns{procs: s.Procs, sessions: sessions, err: err}
}

// running 挑出这些分片里正在跑的。问不清时返回空——调用方必须先看过 unknown。
func (r shardRuns) running(cluster string, shards []string) []string {
	if r.err != nil || r.procs == nil {
		return nil
	}
	var out []string
	for _, shard := range shards {
		if _, ok := r.sessions[r.procs.SessionName(cluster, shard)]; ok {
			out = append(out, shard)
		}
	}
	return out
}

// unknown 非空表示这次没问到进程状态。
// 备份不拦（在跑也能打，任务里还会先催一次存盘），恢复必须拦：
// 它要整棵换掉存档目录，"应该在关着"和"确认关着"是两件事。
func (r shardRuns) unknown() string {
	if r.err == nil {
		return ""
	}
	return "问不到世界进程状态：面板需要在服务端同机运行，且 PATH 里要有 screen 命令"
}

// backupSnapshotView 是列表与详情共用的一份快照数据。
// 快照自带的 Restorable/RestoreNote 已经回答了"这份档本身能不能用"，
// 这里再补运行中的分片：那才是"能点但点了会失败"最常见的原因。
type backupSnapshotView struct {
	backup.Snapshot
	RunningShards []string `json:"runningShards,omitempty"`
	RunningNote   string   `json:"runningNote,omitempty"`
	DownloadPath  string   `json:"downloadPath"`
	DetailPath    string   `json:"detailPath"`
	RestorePath   string   `json:"restorePath"`
}

type backupPolicy struct {
	Dir string `json:"dir"`
	// Keep 是当前生效的保留上限：可能来自面板设置，也可能来自配置文件，
	// KeepSource 就是那句"以这里为准"的说明。
	Keep       int    `json:"keep"`
	KeepSource string `json:"keepSource"`
	UsedBytes  int64  `json:"usedBytes"`
	UsedLabel  string `json:"usedLabel"`
}

type backupsPage struct {
	Clusters      []string             `json:"clusters"`
	Snapshots     []backupSnapshotView `json:"snapshots"`
	Policy        backupPolicy         `json:"policy"`
	Import        importPolicy         `json:"import"`
	FreeBytes     int64                `json:"freeBytes"`
	FreeLabel     string               `json:"freeLabel"`
	ScheduleHours int                  `json:"scheduleHours"`
	// ScheduleRunning 回答"定时那一路到底有没有在跑"：开了间隔但一个存档都没有，
	// 界面上要能说清为什么还没多出快照来。
	ScheduleRunning bool             `json:"scheduleRunning"`
	Note            string           `json:"note"`
	SomeBackups     bool             `json:"someBackups"`
	RestoreBlocked  []blockedCluster `json:"restoreBlocked,omitempty"`
}

// blockedCluster 是"这个房间现在不能恢复"的清单条目。
// 单独列一份而不是只在快照行里写：用户想在恢复前一眼看到"哪些房间要先去关服"，
// 而不是把十几行快照逐个展开找那句提示。
type blockedCluster struct {
	Cluster string   `json:"cluster"`
	Running []string `json:"running"`
	Note    string   `json:"note"`
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	if !s.backupsUnavailable(w) {
		return
	}
	cluster := strings.TrimSpace(r.URL.Query().Get("cluster"))
	if cluster != "" {
		if _, ok := s.knownCluster(w, r, cluster); !ok {
			return
		}
		// ?cluster= 是浏览器直接给的房间名，光靠列表页过滤挡不住：
		// 成员手敲一个别人房间的名字，这里也得拒。
		if !s.denyInvisibleRoom(w, r, cluster) {
			return
		}
	}
	list, err := s.Backups.List()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	runs := s.shardRuns(r.Context())
	visible := s.visibleClusters(r, s.clusterNames())
	seen := make(map[string]bool, len(visible))
	for _, n := range visible {
		seen[normalizeClusterName(n)] = true
	}
	out := backupsPage{
		Clusters:  visible,
		Snapshots: make([]backupSnapshotView, 0, len(list)),
	}
	blocked := map[string][]string{}
	for _, snap := range list {
		if cluster != "" && snap.Cluster != cluster {
			continue
		}
		// 别人房间里的快照不列出来：那一行上有"恢复"这个按钮，
		// 列出来再让接口拒它，不如一开始就不给。
		if !seen[normalizeClusterName(snap.Cluster)] {
			continue
		}
		view := backupSnapshotView{
			Snapshot:     snap,
			DownloadPath: fmt.Sprintf("/api/v1/backups/%s/download", snap.ID),
			DetailPath:   fmt.Sprintf("/api/v1/backups/%s", snap.ID),
			RestorePath:  fmt.Sprintf("/api/v1/backups/%s/restore", snap.ID),
		}
		// "能不能点恢复"要顺着现在的进程状态说：快照本身没变，
		// 但分片起来了就是不能恢复。界面上不能让用户点下去才知道这件事。
		if snap.Restorable {
			if note := runs.unknown(); note != "" {
				view.RunningNote = note
			} else {
				running := runs.running(snap.Cluster, snap.SaveShards)
				view.RunningShards = running
				if len(running) > 0 {
					view.RunningNote = fmt.Sprintf("这个房间还有世界在跑（%s），要恢复得先把它们全停掉",
						strings.Join(running, "、"))
					blocked[snap.Cluster] = mergeUniqueStrings(blocked[snap.Cluster], running)
				}
			}
		}
		out.Snapshots = append(out.Snapshots, view)
	}
	// 上面的循环只说得出"有快照的房间"在不在跑，而导入那一步的目标房间名单是全部房间。
	// 一个刚开服还没打过备份的房间正在跑，就得靠这一趟把它报出来：
	// 否则界面上的「导入并换进房间」是亮的，用户点下去才知道要关服。
	if runs.unknown() == "" {
		for _, name := range out.Clusters {
			if running := runs.running(name, s.Paths.ShardNames(name)); len(running) > 0 {
				blocked[name] = mergeUniqueStrings(blocked[name], running)
			}
		}
	}
	for name, running := range blocked {
		out.RestoreBlocked = append(out.RestoreBlocked, blockedCluster{
			Cluster: name, Running: running,
			Note: fmt.Sprintf("%s 在跑，恢复前要先关掉：%s", name, strings.Join(running, "、")),
		})
	}

	sched, schedErr := s.settingInt(store.KeyBackupScheduleHours)
	out.ScheduleHours = sched
	out.SomeBackups = len(out.Snapshots) > 0
	out.Policy = backupPolicy{
		Dir: s.Backups.Dir, Keep: s.Backups.KeepNow(), UsedBytes: s.Backups.TotalBytes(),
	}
	out.Policy.UsedLabel = humanBytes(out.Policy.UsedBytes)
	if out.Policy.UsedBytes == 0 {
		// 空目录时 humanBytes 给的是空串（它是给"可省略字段"用的），
		// 而这里那一行是"已用 、磁盘还剩 …"，留空就成了半句话。
		out.Policy.UsedLabel = "0 B"
	}
	out.Import = s.importPolicyOf()
	if _, ok, _ := s.Store.Setting(store.KeyBackupKeep); ok {
		out.Policy.KeepSource = "以面板设置里的「存档备份最多保留几份」为准（配置文件里的 backup.keep 是它没被改过时的默认值）"
	} else {
		out.Policy.KeepSource = "来自配置文件 backup.keep，想改就在面板设置里改，改完立刻生效不用重启"
	}
	if free, err := dirFreeBytes(s.Backups.Dir); err == nil {
		out.FreeBytes = free
		out.FreeLabel = humanBytes(free)
	} else {
		s.log().Warn("读备份目录剩余空间失败", "dir", s.Backups.Dir, "err", err.Error())
	}

	var notes []string
	// "一份快照多大"必须是打出来的实测数而不是猜的：云机上 137.9MB 的存档目录
	// 打成 19.0MB 的包（存档本体本来就是压过的），照着 140MB 报会把磁盘预算夸到七倍，
	// 用户据此得出的结论是"keep=20 要 2.8G"，而实际只要几百 MB。
	// 取的是最近一份**归档还在**的快照：最新那份可能正被人删了一半，
	// 拿它的 0 字节说话就等于指着三份好好的快照说"还没有快照可比"。
	sizeNote := "还没有备份可比出实际大小：先打一份，这里就跟着它报归档和解压前各多少"
	for _, s := range list {
		if s.ArchiveBytes > 0 {
			sizeNote = fmt.Sprintf("按最近一份备份看，一个归档 %s（解压前 %s），恢复时还会自动多打一份恢复前的安全备份",
				humanBytes(s.ArchiveBytes), humanBytes(s.UncompressedBytes))
			break
		}
	}
	notes = append(notes, fmt.Sprintf("备份目录 %s，已用 %s，磁盘还剩 %s；%s",
		s.Backups.Dir, out.Policy.UsedLabel, orDash(out.FreeLabel), sizeNote))
	switch {
	case schedErr != nil:
		notes = append(notes, fmt.Sprintf("定时备份间隔这项现在读不出可信值：%s", publicError(schedErr)))
	case sched > 0:
		out.ScheduleRunning = true
		notes = append(notes, fmt.Sprintf("定时备份每 %d 小时给每个有存档的房间打一份；面板刚重启时要先等满一个间隔才会打第一份", sched))
	default:
		notes = append(notes, "定时备份没开，只在存档页手工打；要开就在面板设置里把间隔小时数改成大于 0")
	}
	if len(out.Snapshots) == 0 {
		notes = append(notes, "还没有任何备份：先点「备份存档」打一份，恢复动作要用的一定是已经存在的备份；"+
			"别人给的存档包就走导入，它做完的同样是一份能下载、能校验、能恢复的快照")
	}
	if runs.unknown() != "" {
		notes = append(notes, runs.unknown()+"，所以这一页没法告诉你哪些房间正在跑")
	}
	out.Note = strings.Join(notes, "；")
	writeOK(w, out)
}

func (s *Server) handleBackupDetail(w http.ResponseWriter, r *http.Request) {
	if !s.backupsUnavailable(w) {
		return
	}
	id := r.PathValue("id")
	if err := backup.CheckID(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	snap, err := s.Backups.Get(id)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 详情就是清单本身：界面要能回答"这份档里到底有哪些文件、每个多大"，
	// 而用户判断"这份是不是我要的那一天"靠的正是这些名字和大小。
	writeOK(w, map[string]any{
		"snapshot": snap,
		"labels": map[string]string{
			"source":       backup.LabelFor(snap.Label),
			"restorable":   yesNo(snap.Restorable, "可以用来恢复", "不能用来恢复"),
			"hasSave":      shardsLabel(snap.SaveShards),
			"archiveBytes": humanBytes(snap.ArchiveBytes),
			"uncompressed": humanBytes(snap.UncompressedBytes),
		},
		"note": backupDetailNote(snap),
	})
}

// backupDetailNote 把一份清单变成能照着做的下一步。
// 光有字段表是不够的：跳过名单里那些名字要配上原因，用户才会明白"没有令牌"是面板做对了而不是丢了。
func backupDetailNote(snap backup.Snapshot) string {
	var parts []string
	if snap.Restorable {
		parts = append(parts, fmt.Sprintf("这份备份登记了 %d 个文件、解压前 %s，带存档的世界是 %s，可以用来恢复",
			snap.FileCount, humanBytes(snap.UncompressedBytes), shardsLabel(snap.SaveShards)))
	} else if snap.RestoreNote != "" {
		parts = append(parts, "这份备份现在不能用来恢复："+snap.RestoreNote)
	}
	if len(snap.Skipped) > 0 {
		parts = append(parts, fmt.Sprintf("打这份备份时刻意跳过了 %d 项（Klei 令牌、面板命令通道文件、引擎日志），每一项后面都写了原因",
			len(snap.Skipped)))
	}
	if snap.Note != "" {
		parts = append(parts, snap.Note)
	}
	parts = append(parts, "恢复时房间配置（cluster.ini 和世界的 server.ini）不会被覆盖，端口和服务器名保持现状")
	return strings.Join(parts, "；")
}

func shardsLabel(shards []string) string {
	if len(shards) == 0 {
		return "没有带存档的世界"
	}
	return strings.Join(shards, "、")
}

func yesNo(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	if !s.backupsUnavailable(w) {
		return
	}
	id := r.PathValue("id")
	path, err := s.Backups.ArchivePath(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, publicError(err))
		return
	}
	// 下载走 ServeFile 而不是拼 JSON：几十 MB 的归档读进内存再写出去，
	// 在 4G 的小机上等于把内存吃给自己看，而且断点续传和 Content-Length 都得自己重写。
	http.ServeFile(w, r, path)
	s.audit(r, "backup.download", id, filepath.Base(path), nil)
}

func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	if !s.backupsUnavailable(w) {
		return
	}
	id := r.PathValue("id")
	// 删之前先看一眼：Delete 对"两个文件都不在"会报错，但那句"没删任何东西"
	// 在界面上要连着说清这份是谁的档，否则用户不知道自己是不是删错了房间。
	snap, err := s.Backups.Get(id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, publicError(err))
		return
	}
	if err := s.Backups.Delete(id); err != nil {
		s.audit(r, "backup.delete", id, "", err)
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "backup.delete", id,
		fmt.Sprintf("删除了 %s（房间 %s，%s，%d 个文件）", id, snap.Cluster, backup.LabelFor(snap.Label), snap.FileCount), nil)
	writeOK(w, map[string]any{
		"id": id,
		"note": fmt.Sprintf("已删除 %s 的归档和清单。磁盘上少 %s；要是这是最近一份，恢复就只能退回更早的备份了",
			id, humanBytes(snap.ArchiveBytes)),
	})
}

type backupVerifyView struct {
	backup.VerifyResult
	At     string `json:"at"`
	Detail string `json:"detail"`
}

// handleBackupVerify 把一个归档整个读一遍、逐文件比哈希。只读，不动存档。
//
// 为什么这个接口是同步的：校验就是"点一下想知道结果"，几十秒的读盘和哈希
// 对几十 MB 的归档来说够用了，而且它没有副作用，失败了重跑一次没有任何代价——
// 和恢复那种"必须走队列，因为做一半会留下中间态"是两类事。
func (s *Server) handleBackupVerify(w http.ResponseWriter, r *http.Request) {
	if !s.backupsUnavailable(w) {
		return
	}
	id := r.PathValue("id")
	res, err := s.Backups.Verify(r.Context(), id)
	s.audit(r, "backup.verify", id, fmt.Sprintf("checked=%d missing=%d unexpected=%d mismatched=%d",
		res.Checked, len(res.Missing), len(res.Unexpected), len(res.Mismatched)), err)
	if err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, err)
		return
	}
	writeOK(w, backupVerifyView{
		VerifyResult: res,
		At:           time.Now().UTC().Format(time.RFC3339),
		Detail:       verifyDetail(res),
	})
}

// verifyDetail 把三个差异桶翻译成"然后做什么"。
// ArchiveOK 只是一个布尔值，界面旁边必须有一句人话，否则用户看到 4 个 0 长度的数组会以为坏了。
func verifyDetail(res backup.VerifyResult) string {
	if res.ArchiveOK {
		return fmt.Sprintf("包里 %d 个文件逐个比过哈希，和清单全部对得上；这份备份可以放心用来恢复", res.Checked)
	}
	var hints []string
	if len(res.Mismatched) > 0 {
		hints = append(hints, fmt.Sprintf("%d 个文件的内容和清单登记的哈希不一样（包被人改过，或者当初就没写完）", len(res.Mismatched)))
	}
	if len(res.Missing) > 0 {
		hints = append(hints, fmt.Sprintf("清单登记了但包里找不到 %s", strings.Join(firstOf(res.Missing, 3), "、")))
	}
	if len(res.Unexpected) > 0 {
		hints = append(hints, fmt.Sprintf("包里有清单没登记过的 %s", strings.Join(firstOf(res.Unexpected, 3), "、")))
	}
	return "这份备份不能用：" + strings.Join(hints, "；") + "。要恢复就换一份校验得过的备份"
}

func firstOf(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return list[:n]
}

// ---- 备份与恢复两个入口 ----

// backupCreateRequest 只有房间名一个字段，而且以 URL 上的 {name} 为准。
// 留这个字段是为了让"两处各写一个房间名"变成能被发现的错误，而不是各等一半。
type backupCreateRequest struct {
	Cluster string `json:"cluster"`
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	if !s.backupsUnavailable(w) {
		return
	}
	var req backupCreateRequest
	if err := decodeOptionalBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	cluster := r.PathValue("name")
	if body := strings.TrimSpace(req.Cluster); body != "" && body != cluster {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("URL 上的房间是 %s，请求体里写的是 %s，两者要一致（备份只能对 URL 指定的那个房间做）", cluster, body))
		return
	}
	if _, ok := s.knownCluster(w, r, cluster); !ok {
		return
	}
	// 在跑不是拦备份的理由：引擎在跑时打的快照靠的是任务开头那次 c_save()，
	// 打完了还是能在跑的这个进程里看到最新档位。这里只把结论说清楚。
	runs := s.shardRuns(r.Context())
	note := fmt.Sprintf("备份任务已入队：先让在跑的世界立即存盘，再打包 %s 的整个房间目录", cluster)
	if running := runs.running(cluster, s.Paths.ShardNames(cluster)); len(running) > 0 {
		note = fmt.Sprintf("%s（当前在跑：%s，会先催它们存盘再打包）", note, strings.Join(running, "、"))
	}

	id, err := s.Store.CreateTask(worker.KindBackupCreate, fmt.Sprintf("备份存档 · %s", cluster),
		worker.BackupCreatePayload{Cluster: cluster, Label: backup.LabelManual})
	s.audit(r, "backup.create", cluster, fmt.Sprintf("任务 id=%d", id), err)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeTaskAccepted(w, id, note)
}

type backupRestoreRequest struct {
	// TargetCluster 为空表示恢复回快照自己的房间；非空就是跨房间迁移。
	TargetCluster string `json:"targetCluster"`
	Confirm       bool   `json:"confirm"`
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if !s.backupsUnavailable(w) {
		return
	}
	id := r.PathValue("id")
	// 被挡下来的恢复尝试也要留痕：一次"这份是不是我要的那份"的试探失败了，
	// 事后要能查出是谁在什么时候点了哪一份，理由是什么。启动分片被前置检查拒掉是同一个道理。
	refuse := func(status int, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		s.audit(r, "backup.restore", id, "", errors.New(msg))
		writeErr(w, status, msg)
	}
	snap, err := s.Backups.Get(id)
	if err != nil {
		refuse(http.StatusNotFound, "%s", publicError(err))
		return
	}
	if !snap.Restorable {
		refuse(http.StatusConflict, "这份备份不能用来恢复：%s", snap.RestoreNote)
		return
	}
	var req backupRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "请求体不是合法 JSON: %s", err.Error())
		return
	}
	target := strings.TrimSpace(req.TargetCluster)
	if target == "" {
		target = snap.Cluster
	}
	// 迁移到别的房间是"照着快照重建一个房间的世界"，目标房间现在那个世界会被换掉，
	// 所以二次确认不能省，而且这句确认必须落在服务端而不是前端藏起来。
	spec, _ := worker.KindSpec(worker.KindBackupRestore)
	if spec.Danger && !req.Confirm {
		refuse(http.StatusBadRequest,
			"恢复存档要带 confirm:true 才能提交。目标房间：%s（备份来自 %s），它的存档目录会被整棵替换；%s",
			target, snap.Cluster, spec.Desc)
		return
	}
	if _, ok := s.knownCluster(w, r, target); !ok {
		return
	}
	// 要停的分片按"目标房间现有的 + 快照里带存档的"合集问：跨房间迁移时目标房间
	// 还没有 Caves 分片，只查现有名单会漏掉那个正要被新建的目录。
	shards := mergeUniqueStrings(s.Paths.ShardNames(target), snap.SaveShards)
	runs := s.shardRuns(r.Context())
	if note := runs.unknown(); note != "" {
		refuse(http.StatusServiceUnavailable, "%s。恢复要换掉整棵存档目录，问不清就不动", note)
		return
	}
	if running := runs.running(target, shards); len(running) > 0 {
		refuse(http.StatusConflict,
			"%s 还有世界在跑（%s）：先把它们全停掉再恢复。在跑的服务端会把刚恢复的内容按当前内存里的世界再存回去，"+
				"结果就是看起来没恢复成功", target, strings.Join(running, "、"))
		return
	}
	taskID, err := s.Store.CreateTask(worker.KindBackupRestore,
		fmt.Sprintf("恢复存档 · %s ← %s", target, id),
		worker.BackupRestorePayload{SnapshotID: id, TargetCluster: target})
	s.audit(r, "backup.restore", target, fmt.Sprintf("备份 %s，任务 id=%d", id, taskID), err)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeTaskAccepted(w, taskID, fmt.Sprintf(
		"恢复任务已入队：面板会先整包校验这份备份，再自动打一份恢复前的安全备份，然后把 %s 的存档目录整棵换掉", target))
}

func (s *Server) writeTaskAccepted(w http.ResponseWriter, id int64, note string) {
	task, err := s.Store.GetTask(id)
	if err != nil {
		s.log().Warn("任务已入队但读不回详情", "id", id, "err", err.Error())
		writeJSON(w, http.StatusAccepted, reply{OK: true, Data: map[string]any{
			"taskId": id, "note": note + "（任务详情读不回，去任务中心看进度）",
		}})
		return
	}
	writeJSON(w, http.StatusAccepted, reply{OK: true, Data: map[string]any{
		"taskId":    id,
		"task":      taskViewOf(task, note),
		"note":      note,
		"pollAfter": "1s",
	}})
}

// knownCluster 把请求里的房间名对着磁盘上的名单核一遍。
// 名单是唯一可信来源：拼接路径的每一个环节都必须先是磁盘上真存在的那个目录。
func (s *Server) knownCluster(w http.ResponseWriter, r *http.Request, cluster string) (string, bool) {
	cluster = strings.TrimSpace(cluster)
	if cluster == "" {
		writeErr(w, http.StatusBadRequest, "要指定房间名")
		return "", false
	}
	names, err := s.Paths.ClusterNames()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return "", false
	}
	if !contains(names, cluster) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("集群 %s 不存在，现有：%s", cluster, strings.Join(names, "、")))
		return "", false
	}
	return cluster, true
}

// decodeOptionalBody 让"没有请求体"和"请求体写坏了"区分开。
// 房间名在 URL 上，这个接口的体本来就可以不填；把空体的 EOF 报成 400
// 会让照着 curl 抄的用户以为自己写错了 JSON。
func decodeOptionalBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func mergeUniqueStrings(lists ...[]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, l := range lists {
		for _, v := range l {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func orDash(v string) string {
	if v == "" {
		return "未知"
	}
	return v
}
