// 异步任务队列的接口层：列出可提交的任务类型、提交、取消。
//
// 这一层只做"要不要收这条任务"的判断，执行全在 internal/worker。
// 分开是因为两边的失败方式完全不同：这里失败要立刻回用户一句话，
// 那边失败要留在任务行上，等用户回头翻任务中心。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"dstpanel/internal/store"
	"dstpanel/internal/worker"
)

type taskKindView struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
	// Danger 为真时界面要二次确认；服务端同样强制，见 handleTaskSubmit。
	Danger bool `json:"danger"`
	// Runnable 回答"这台机器上现在真能做这件事吗"，缺前置条件时界面就不用等提交才报错。
	Runnable bool   `json:"runnable"`
	Note     string `json:"note,omitempty"`
	// SubmitVia 非空表示这类任务不能从这里提交，参数要由面板对着磁盘名单校验后自己拼。
	// 界面看到它就该把按钮指向存档页，而不是在任务中心给一个点了必失败的入口。
	SubmitVia string `json:"submitVia,omitempty"`
	// NeedsPayload 是给界面的另一半答案：这条任务的参数不止 kind。
	NeedsPayload bool `json:"needsPayload"`
}

func (s *Server) handleTaskKinds(w http.ResponseWriter, r *http.Request) {
	names := worker.KindNames()
	out := make([]taskKindView, 0, len(names))
	for _, name := range names {
		spec, ok := worker.KindSpec(name)
		if !ok {
			continue
		}
		v := taskKindView{Kind: spec.Name, Label: spec.Label, Desc: spec.Desc, Danger: spec.Danger,
			Runnable: true, SubmitVia: spec.SubmitVia, NeedsPayload: spec.SubmitVia != ""}
		if name == worker.KindServerUpdate {
			v.Runnable, v.Note = s.steamCmdReady()
			// 本体还不在时这一条不是警告而是"这台机器的下一步"：
			// 全新装完的人打开任务中心看到的应该是"这里能下载游戏"，
			// 不是"这里能更新游戏（虽然没什么可更新的）"。
			if v.Runnable && !s.Paths.EnginePresent() {
				v.Note = fmt.Sprintf("这台机器还没有服务端本体（%s 下没有 bin64），"+
					"这一次是全新下载，整包多大由 steamcmd 实时报出来", s.Paths.DstInstall)
			}
		}
		if spec.SubmitVia != "" && s.Backups == nil {
			v.Runnable = false
			v.Note = "面板没接上存档备份模块：存档目录可能不可写，确认后重启面板"
		}
		out = append(out, v)
	}
	writeOK(w, out)
}

// steamCmdReady 只回答"配了且文件在不在"，不做可执行性测试：
// 真去跑一次 steamcmd --version 会让这个 GET 卡在网络与进程创建上。
func (s *Server) steamCmdReady() (bool, string) {
	path := strings.TrimSpace(s.SteamCmd)
	if path == "" {
		return false, "配置里 paths.steamCmd 还没填，填好后重启面板"
	}
	if _, err := os.Stat(path); err != nil {
		return false, fmt.Sprintf("找不到 steamcmd：%s", path)
	}
	return true, ""
}

// engineView 是"这台机器上的服务端本体"状态。它是机器事实，不是账号事实，
// 之所以随 /session 一起给，见 accounts.go 里 sessionView.Engine 的注释。
type engineView struct {
	Present    bool   `json:"present"`
	Version    string `json:"version,omitempty"`
	InstallDir string `json:"installDir"`
	// SteamCmdReady 决定横幅给哪一句：本体没装时，能下载就要指到任务中心，
	// 连下载工具都没有就得先说清缺的是哪一样，不能把用户指向一个点了必失败的按钮。
	SteamCmdReady bool   `json:"steamCmdReady"`
	SteamCmdNote  string `json:"steamCmdNote,omitempty"`
}

func (s *Server) engineStatus() engineView {
	v := engineView{Present: s.Paths.EnginePresent(), InstallDir: s.Paths.DstInstall}
	v.Version = s.Paths.EngineVersion()
	v.SteamCmdReady, v.SteamCmdNote = s.steamCmdReady()
	return v
}

type taskSubmitRequest struct {
	Kind    string `json:"kind"`
	Confirm bool   `json:"confirm"`
}

// handleTaskSubmit 只接受白名单里的 kind。
//
// payload 一律由服务端自己拼：任务的参数如果来自请求体，就等于把
// "steamcmd 该拉哪个 app" 这种决定交给了浏览器。
func (s *Server) handleTaskSubmit(w http.ResponseWriter, r *http.Request) {
	var req taskSubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	spec, ok := worker.KindSpec(req.Kind)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"任务类型 %q 不认识，可以提交的有：%s", req.Kind, strings.Join(worker.KindNames(), "、")))
		return
	}
	if spec.Danger && !req.Confirm {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"任务「%s」需要先确认：%s", spec.Label, spec.Desc))
		return
	}
	// 带参数的任务不从这里的口子进：payload 必须先是"对着磁盘核过的房间名/快照编号"
	// 再拼出来的，而这里的请求体只有一个 kind，接了就等于让浏览器给参数。
	if spec.SubmitVia != "" {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"「%s」的参数要由面板对着磁盘上的房间和备份名单校验后自己拼，通用提交口不接受它；请从 %s 提交",
			spec.Label, spec.SubmitVia))
		return
	}
	if req.Kind == worker.KindServerUpdate {
		if ready, note := s.steamCmdReady(); !ready {
			writeErr(w, http.StatusBadRequest, note)
			return
		}
	}
	id, err := s.Store.CreateTask(spec.Name, spec.Label, nil)
	s.audit(r, "task.submit", spec.Name, fmt.Sprintf("id=%d", id), err)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	task, err := s.Store.GetTask(id)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 202：这条任务只是进了队列，做完没有、什么时候做完都得再问。
	writeJSON(w, http.StatusAccepted, reply{OK: true, Data: taskViewOf(task, "")})
}

// handleTaskCancel 只取消还在排队的任务。
//
// 已经在跑的不支持取消：正在写的存档目录或正在解压的模组被半路掐掉，
// 留下的脏状态比多等几分钟贵得多。这种情况回 409 并说明现在的状态。
func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("任务 id 得是数字，收到的是 %q", r.PathValue("id")))
		return
	}
	task, err := s.Store.GetTask(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if store.IsTaskTerminal(task.Status) {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"任务 %d 已经是%s，不用再取消；要重做请重新提交", id, store.LabelTaskStatus(task.Status)))
		return
	}
	if task.Status == store.TaskRunning {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"任务 %d 正在执行，面板不做半路掐断（会留下没写完的文件）；等它跑完或失败后再重提", id))
		return
	}
	if err := s.Store.CancelTask(id, "在面板里取消"); err != nil {
		s.audit(r, "task.cancel", fmt.Sprintf("id=%d", id), "", err)
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "task.cancel", fmt.Sprintf("id=%d", id), task.Kind, nil)
	task, err = s.Store.GetTask(id)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, taskViewOf(task, "已取消排队中的任务"))
}
