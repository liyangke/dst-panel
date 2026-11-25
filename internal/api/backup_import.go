// 导入外部存档的接口层：上传→看结构→提交导入。
//
// 分工和备份/恢复那两条一样：这一层最多在备份目录里多放一个上传包，
// 房间目录一下都不碰。真正换掉存档的是 worker 里那条导入任务，
// 而它走的还是恢复那同一条路（runRestore）——整个面板只有两处会换存档目录。
//
// 为什么预览和提交要各读一遍包：几十 MB 的档传完，得先让人看清
// "里面有哪几个世界、世界编号对不对得上"，他才知道要不要换进哪个房间；
// 而提交那一次必须重新检查，因为请求里的上传包编号是客户端给的，
// 不能假设它这次传的和上次预览的是同一个文件，也不能假设它预览过。
//
// 上传包落在备份目录的 incoming 子目录里，编号是面板按时刻+包内容认出的后缀定的
// （见 backup.WriteStaged）；它既不进快照列表也不参与保留数清理，
// 只由 TTL 收尾，所以界面上必须单独说一句"还留着多少临时上传"。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"dstpanel/internal/backup"
	"dstpanel/internal/worker"
)

// importUploadField 是 multipart 里那个存档文件字段名。
// 它出现在界面提示和报错里，所以和 maxUploadBytes 那种常量一样不能两边各写一份。
const importUploadField = "save"

// importBodyHeadroom 给 multipart 的边界和字段名留的余量。
// 不留的话一个正好卡在 512MB 的合法包会因为这点封装开销被整个拒掉，
// 而拒的那句还是"请求体太大"，用户根本不知道少了哪几 KB。
const importBodyHeadroom = 1 << 20

// importPreviewView 是预览的返回：包的结构结论，加上界面提交时要用的那几个字符串。
// 把路径和上限一起给出去，前端才不会自己拼出一个面板没注册的 URL，
// 或者把上限抄成一个数字——抄错的那份是界面上唯一一句"这个包能不能传"的依据。
type importPreviewView struct {
	backup.ImportPreview
	PackageRef  string `json:"packageRef"`
	ImportPath  string `json:"importPath"`
	UploadField string `json:"uploadField"`
	MaxUpload   string `json:"maxUploadLabel"`
	ExpireNote  string `json:"expireNote"`
	// SourceName 是用户当初那个文件名。上传包编号里只留 ASCII（它要参与拼路径，
	// 白名单形状卡得很死），所以中文文件名在编号里会缩成一个下划线——
	// 界面那行要让人认出"这是我传的哪一种档"，靠的就是这个字段而不是编号。
	SourceName string `json:"sourceName"`
}

// importPolicy 是存档页那份"导入这件事的现状"。
type importPolicy struct {
	MaxArchiveBytes int64  `json:"maxArchiveBytes"`
	MaxArchiveLabel string `json:"maxArchiveLabel"`
	UploadField     string `json:"uploadField"`
	PreviewPath     string `json:"previewPath"`
	ImportPath      string `json:"importPath"`
	StagedCount     int    `json:"stagedCount"`
	StagedBytes     int64  `json:"stagedBytes"`
	StagedLabel     string `json:"stagedLabel"`
	ExpireHours     int    `json:"expireHours"`
	Note            string `json:"note"`
}

func (s *Server) handleBackupImportPreview(w http.ResponseWriter, r *http.Request) {
	if !s.backupsUnavailable(w) {
		return
	}
	// 上限交给 WriteStaged 去做精确判断（它按收到的字节数报"收到多少就停了"），
	// 这里这道 MaxBytesReader 只是不让一个无限大的请求体把连接挂着不放。
	r.Body = http.MaxBytesReader(w, r.Body, backup.MaxImportArchiveBytes+importBodyHeadroom)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest,
			"表单读不出来：要用 multipart/form-data，存档文件放在 "+importUploadField+" 这个字段里")
		return
	}
	defer r.MultipartForm.RemoveAll()

	f, header, err := r.FormFile(importUploadField)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("必须带一个名为 %s 的存档包文件字段（.zip 或 .tar.gz）", importUploadField))
		return
	}
	defer f.Close()

	ref, landed, err := s.Backups.WriteStaged(header.Filename, f)
	// 落地这一步单独留痕：一个外来的文件进了这台机器的盘，
	// 就算它后面被判定"不是存档"，这一刻也发生过，事后要查得出来。
	s.audit(r, "backup.import.upload", ref,
		fmt.Sprintf("来自 %s，%s", header.Filename, humanBytes(landed)), err)
	if err != nil {
		// 认不出格式、超上限、空内容都算上传内容不对，不算面板故障：400。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	path, err := s.Backups.StagedPath(ref)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	preview, err := s.Backups.InspectImport(path)
	if err == nil {
		s.audit(r, "backup.import.preview", ref, importPreviewSummary(preview), nil)
	} else {
		s.audit(r, "backup.import.preview", ref, "", err)
	}
	if err != nil {
		// 到这里文件已经在盘上了，只是这个包读不成目录（截断、假后缀）。
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// 结构不对不是错误：预览的产出就是那串问题，界面上要能整段展示，
	// 而 canImport 为假时"导入"按钮是灰的。回 200 才对得上这件事。
	writeOK(w, importPreviewView{
		ImportPreview: preview,
		PackageRef:    ref,
		ImportPath:    "/api/v1/backups/import",
		UploadField:   importUploadField,
		MaxUpload:     humanBytes(backup.MaxImportArchiveBytes),
		SourceName:    header.Filename,
		ExpireNote: fmt.Sprintf("这份上传包在盘上最多留 %d 小时，其间不用重传；没导入就会被面板自动清掉",
			int(backup.StagedUploadTTL.Hours())),
	})
}

// importPreviewSummary 是预览这一步留在审计里的一句话。
// 审计条目要能回答"当时看到的是不是现在这个包"，所以带上世界数、编号和大小。
func importPreviewSummary(p backup.ImportPreview) string {
	var worlds []string
	for _, sh := range p.Shards {
		worlds = append(worlds, fmt.Sprintf("%s=%s", sh.Shard, sh.SessionID))
	}
	return fmt.Sprintf("格式 %s，%d 个文件、解压前 %s，世界 %s，能导入=%v",
		p.KindLabel, p.FileCount, p.UncompressedLabel, strings.Join(worlds, "、"), p.CanImport)
}

type backupImportRequest struct {
	// Package 是预览那一步面板发出来的上传包编号，不是文件名，也不是路径。
	Package string `json:"package"`
	Cluster string `json:"cluster"`
	// Restore 为真表示"导入完就换进这个房间"，那一步要 confirm，
	// 而且要先把目标房间的世界停掉——和恢复任务的前置检查一模一样。
	Restore bool `json:"restore"`
	Confirm bool `json:"confirm"`
}

func (s *Server) handleBackupImport(w http.ResponseWriter, r *http.Request) {
	if !s.backupsUnavailable(w) {
		return
	}
	var req backupImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.refuseImport(w, r, "", req.Cluster, http.StatusBadRequest, "请求体不是合法 JSON: %s", err.Error())
		return
	}
	ref := strings.TrimSpace(req.Package)
	cluster := strings.TrimSpace(req.Cluster)

	// 先把编号当路径拼一遍会怎样，这里不说：StagedPath 里那道白名单形状检查
	// 就是为这个字符串准备的，它会点名说"不是面板生成的格式"。
	path, err := s.Backups.StagedPath(ref)
	if err != nil {
		if errors.Is(err, backup.ErrStagedGone) {
			s.refuseImport(w, r, ref, cluster, http.StatusNotFound, "%s", err.Error())
		} else {
			s.refuseImport(w, r, ref, cluster, http.StatusUnprocessableEntity, "%s", err.Error())
		}
		return
	}
	if _, ok := s.knownCluster(w, r, cluster); !ok {
		return
	}

	// 和 Import 同一份判断（都走 planPackage+judge），所以这里说能导，
	// 任务里就不会因为"结构不对"失败——反过来也成立：这里放行了，任务里也不会。
	preview, err := s.Backups.InspectImport(path)
	if err != nil {
		s.refuseImport(w, r, ref, cluster, http.StatusUnprocessableEntity, "%s", err.Error())
		return
	}
	if !preview.CanImport {
		// 拦下来却不删上传包：这份包可能只是压错了那一层，用户接着要重压重传，
		// 而"这一份还占着多少盘"由存档页顶部那句话说得清，删与不删不该由一条报错决定。
		s.refuseImport(w, r, ref, cluster, http.StatusUnprocessableEntity,
			"这个包不能导入：%s。房间 %s 的存档一点都没动；这份上传包还留在存档页说的那点空间里",
			strings.Join(preview.Problems, "；"), cluster)
		return
	}

	note := fmt.Sprintf("导入任务已入队：解开这个包、按面板的格式重打一份快照，登记给房间 %s", cluster)
	if req.Restore {
		// 二次确认落在服务端：前端藏得住按钮，藏不住一个直接发请求的人。
		spec, _ := worker.KindSpec(worker.KindBackupImport)
		if spec.Danger && !req.Confirm {
			s.refuseImport(w, r, ref, cluster, http.StatusBadRequest,
				"勾选了「导入后就换进这个房间」，要带 confirm:true 才能提交。目标房间 %s 的存档目录会被整棵换掉；%s",
				cluster, spec.Desc)
			return
		}
		// 要停的世界按"目标房间现有的 + 包里带来的"合集问：包里带洞穴而房间还没有洞穴目录时，
		// 只查现有名单就漏掉了那个正要被建出来的目录。
		shards := mergeUniqueStrings(s.Paths.ShardNames(cluster), previewShardNames(preview))
		runs := s.shardRuns(r.Context())
		if msg := runs.unknown(); msg != "" {
			s.refuseImport(w, r, ref, cluster, http.StatusServiceUnavailable,
				"%s。这一步要换掉整棵存档目录，问不清就不动", msg)
			return
		}
		if running := runs.running(cluster, shards); len(running) > 0 {
			s.refuseImport(w, r, ref, cluster, http.StatusConflict,
				"%s 还有世界在跑（%s）：先把它们全停掉再导入。在跑的服务端会把刚换进去的存档按内存里的世界再存回去，"+
					"结果就是看起来没换成功", cluster, strings.Join(running, "、"))
			return
		}
		note = fmt.Sprintf("%s；做完会先打一份恢复前的安全备份，再把 %s 的存档目录整棵换掉", note, cluster)
	}

	taskID, err := s.Store.CreateTask(worker.KindBackupImport,
		fmt.Sprintf("导入外部存档 · %s ← %s", cluster, ref),
		worker.BackupImportPayload{Package: ref, Cluster: cluster, Restore: req.Restore})
	s.audit(r, "backup.import", cluster,
		fmt.Sprintf("上传包 %s，房间 %s，导入后就换进房间=%v，任务 id=%d", ref, cluster, req.Restore, taskID), err)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeTaskAccepted(w, taskID, note)
}

// refuseImport 是导入这条路上的"挡下来并且留痕"。
// 试探失败也要可查：一次被拒的导入说明用户对某个包下了一个面板不许的判断，
// 事后排查"我的档怎么没换"时，这条记录是唯一能区分没点和被挡的依据。
func (s *Server) refuseImport(w http.ResponseWriter, r *http.Request, ref, cluster string, status int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	target := cluster
	if target == "" {
		target = ref
	}
	s.audit(r, "backup.import", target, "", errors.New(msg))
	writeErr(w, status, msg)
}

// previewShardNames 挑出包里带来哪几个世界。恢复要停的世界名单里必须有它们：
// 包里的洞穴目录是这次才建出来的，房间现有的名单里没有它。
func previewShardNames(p backup.ImportPreview) []string {
	var out []string
	for _, sh := range p.Shards {
		out = append(out, sh.Shard)
	}
	return out
}

// importPolicyOf 给存档页那份导入现状：上限、字段名、两条 URL，以及盘上留着的临时上传。
func (s *Server) importPolicyOf() importPolicy {
	count, bytes := s.Backups.StagedUsage()
	// 空目录时 humanBytes 给空串（它是给"可省略字段"用的），而这里那句是
	// "还留着 N 份，共 …"，留空就成了半句话——和备份目录那一行是同一个坑。
	label := humanBytes(bytes)
	if bytes <= 0 {
		label = "0 B"
	}
	note := fmt.Sprintf("导入不会直接改房间：上传的包先被解成一份面板快照，要用它换掉存档还得勾「导入后就换进这个房间」，"+
		"而那一步要求目标房间的世界全部停掉。单个包最大 %s。", humanBytes(backup.MaxImportArchiveBytes))
	if count > 0 {
		note = fmt.Sprintf("%s；上传目录里还留着 %d 份没派上用场的包，共 %s，超过 %d 小时会被自动清掉",
			note, count, label, int(backup.StagedUploadTTL.Hours()))
	}
	return importPolicy{
		MaxArchiveBytes: backup.MaxImportArchiveBytes,
		MaxArchiveLabel: humanBytes(backup.MaxImportArchiveBytes),
		UploadField:     importUploadField,
		PreviewPath:     "/api/v1/backups/import/preview",
		ImportPath:      "/api/v1/backups/import",
		StagedCount:     count,
		StagedBytes:     bytes,
		StagedLabel:     label,
		ExpireHours:     int(backup.StagedUploadTTL.Hours()),
		Note:            note,
	}
}
