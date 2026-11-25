// 分片生命周期与控制台白名单命令。
//
// 这一片是"面板能开服"的核心，之前只有包和单测、没接 HTTP：链接器把
// procrun 的 Start/Stop/Kill 当成不可达代码直接从成品里删掉了，
// 单测全绿也照样开不了服。所以这里加接口，同时用成品符号检查把住这道门。
//
// 启停都只走 screen 会话，面板不 fork 服务端子进程：面板自己也要升级重启，
// 子进程会跟着面板一起死，服务器不该因为面板发版而掉线。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dstpanel/internal/dst"
	"dstpanel/internal/procrun"
	"dstpanel/internal/store"
)

type shardStateView struct {
	Cluster string `json:"cluster"`
	Shard   string `json:"shard"`
	Session string `json:"session"`
	Running bool   `json:"running"`
	Pid     int    `json:"pid,omitempty"`
	Hint    string `json:"hint,omitempty"`
	// Phase / PhaseReason 是启动阶段（starting/loading/ready/failed）。
	// 全部是新增字段，既有字段名、类型、含义一个没动：前端依赖它们。
	Phase       string `json:"phase,omitempty"`
	PhaseReason string `json:"phaseReason,omitempty"`
	// Warning 只在需要用户当心时出现（强停丢档、停服没确认退出）。
	Warning string `json:"warning,omitempty"`
}

// 状态查询不绑定请求生命周期：关闭后的等待轮询要跨几秒，
// 客户端超时或断开不该让面板误判"会话还在"。
var lifecycleCtx = context.Background()

func (s *Server) shardState(cluster, shard, hint string) shardStateView {
	return s.shardStateWith(cluster, shard, hint, "")
}

func (s *Server) shardStateWith(cluster, shard, hint, warning string) shardStateView {
	v := shardStateView{Cluster: cluster, Shard: shard,
		Session: s.Procs.SessionName(cluster, shard), Hint: hint, Warning: warning}
	v.Phase, v.PhaseReason = s.Procs.Phase(cluster, shard)
	running, err := s.Procs.IsRunning(lifecycleCtx, cluster, shard)
	if err == nil && running {
		v.Running = true
		v.Pid, _ = s.Procs.Pid(lifecycleCtx, cluster, shard)
	}
	return v
}

// shardShutdownWait 从监控配置实时读停服等待上限；没接监控时走 30 秒默认值。
// 走实时读而不是启动时抄一份，是因为这一项在面板里能改、改完要立刻生效。
func (s *Server) shardShutdownWait() time.Duration {
	if s.Monitor != nil {
		return s.Monitor.Options().ShutdownWait()
	}
	return 30 * time.Second
}

// markExpected 告诉监控「这个分片是不是面板自己起过、期望在跑」。
// 没接监控时（测试夹具）整块跳过。
func (s *Server) markExpected(cluster, shard string, on bool) {
	if s.Monitor != nil {
		s.Monitor.MarkExpected(cluster, shard, on)
	}
}

// emitSystemEvent 复用现成的操作记录表记一条系统侧告警。
// 用 Store.AppendAudit 直接写表，不走审计 helper：动作码的中文目录在 data.go，
// 凡是走 helper 的新码值都必须先在目录里登记（审计页不该露出裸码）。
func (s *Server) emitSystemEvent(action, target, detail string) {
	if s.Store == nil {
		return
	}
	if err := s.Store.AppendAudit(store.AuditEntry{
		Actor: "system", Action: action, Target: target, Detail: detail, OK: false,
	}); err != nil {
		s.log().Error("系统事件写入失败", "action", action, "err", err.Error())
	}
}

func (s *Server) handleShardStart(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	// 启动命令发出后等会话出现、盯日志判就绪都是面板自己的事，
	// 用 lifecycleCtx 免得客户端断开把判定掐断、误判成启动失败。
	err := s.Procs.Start(lifecycleCtx, cluster, shard)
	s.audit(r, "shard.start", cluster+"/"+shard, "", err)
	if err != nil {
		s.failShard(w, r, cluster, shard, err)
		return
	}
	s.markExpected(cluster, shard, true)
	phase, _ := s.Procs.Phase(cluster, shard)
	hint := "启动命令已发出（阶段：" + phase + "）；助手 MOD 通常要几十秒才回传第一份数据，确认可进入前状态会停在加载中"
	writeOK(w, s.shardState(cluster, shard, hint))
}

type shardStopRequest struct {
	// SaveFirst 为假时跳过落盘直接关（Klei 默认先落盘）。
	SaveFirst *bool `json:"saveFirst"`
}

func (s *Server) handleShardStop(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	saveFirst := true
	if r.Body != nil && r.ContentLength != 0 {
		var req shardStopRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
			return
		}
		if req.SaveFirst != nil {
			saveFirst = *req.SaveFirst
		}
	}
	// 注入 c_shutdown 之后不再立刻回「命令已发出」：等进程真的退出（带上限）。
	// 超时不算执行失败——命令确实发出去了，只是没确认退出，因此给一句明确结论 + warning。
	// 等待用 lifecycleCtx 而不是 r.Context()：命令已注入、存档正在落盘，
	// 客户端这时断开不该把轮询掐断、让面板误判状态。
	wait := s.shardShutdownWait()
	confirmed, err := s.Procs.StopAndWait(lifecycleCtx, cluster, shard, saveFirst, wait)
	s.audit(r, "shard.stop", cluster+"/"+shard,
		fmt.Sprintf("先落盘=%v；等待进程退出上限=%d 秒", saveFirst, int(wait.Seconds())), err)
	if err != nil {
		s.failShard(w, r, cluster, shard, err)
		return
	}
	s.markExpected(cluster, shard, false)
	note := "关闭命令已注入，已确认进程退出（存档已落盘）"
	warning := ""
	if !saveFirst {
		note = "已按不落盘方式关闭，已确认进程退出，存档停在最后一次自动保存"
	}
	if !confirmed {
		note = fmt.Sprintf("已发出关闭命令，但 %d 秒内未确认进程退出：存档可能仍在落盘，请稍后再看这个世界的状态",
			int(wait.Seconds()))
		warning = "尚未确认进程退出；此时不要重复点启动或强停，强停会丢档"
	}
	writeOK(w, s.shardStateWith(cluster, shard, note, warning))
}

// handleShardKill 直接掐会话，不等存档。
// 只给"分片卡死、关闭命令也没反应"这种场合，所以审计里明确标出没保存。
func (s *Server) handleShardKill(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	// 强停始终保持手动：面板不自动执行 screen -X quit。
	// 但响应里要明确说会丢档，并另写一条系统侧事件记录，事后能复盘是谁、什么时候按的。
	const killWarning = "强制关闭不经过存档落盘，会丢失最后一次自动保存之后的世界进度；仅在分片卡死、正常关闭无响应时使用"
	err := s.Procs.Kill(r.Context(), cluster, shard)
	s.audit(r, "shard.kill", cluster+"/"+shard, "强制关闭（screen -X quit），未落盘，会丢档", err)
	if err != nil {
		s.failShard(w, r, cluster, shard, err)
		return
	}
	s.markExpected(cluster, shard, false)
	s.emitSystemEvent("shard.kill.warning", cluster+"/"+shard,
		"管理员手动强停：screen -X quit 不经存档落盘，会丢失最后一次自动保存之后的进度")
	writeOK(w, s.shardStateWith(cluster, shard, "会话已强关，存档停在最后一次自动保存", killWarning))
}

// handleShardRestart 停干净再起。模组与配置改动都要重启分片才生效，
// 这条是模组流程的最后一步，没有它"登记完模组"就只能让用户自己去点。
func (s *Server) handleShardRestart(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	// 停-等-起整体在 procrun 的一把 per-shard 锁里完成，接口层不再分三次调用，
	// 免得停完到再起之间被另一个启停请求插进来。
	// 同样用 lifecycleCtx：重启里含「等旧进程退出」，不该被客户端断开打断。
	running, err := s.Procs.Restart(lifecycleCtx, cluster, shard)
	s.audit(r, "shard.restart", cluster+"/"+shard, fmt.Sprintf("原本在跑=%v", running), err)
	if err != nil {
		if errors.Is(err, procrun.ErrStopTimeout) {
			writeErr(w, http.StatusGatewayTimeout, err.Error())
			return
		}
		s.failShard(w, r, cluster, shard, err)
		return
	}
	s.markExpected(cluster, shard, true)
	hint := "已重启"
	if !running {
		hint = "原本没在跑，直接启动了"
	}
	writeOK(w, s.shardState(cluster, shard, hint+"，模组与配置改动这次会一起生效"))
}

// failShard 按错误语义分层给状态码：前置条件没满足是 400（用户能自己修），
// 会话没开着或重复启动是 409（该先看状态再决定），剩下才是 500。
//
// 启动失败时 server_log.txt 的尾巴已经由 procrun.Start 拼在错误里带回来了，
// 会话起来了不等于服务端起来了，崩溃原因只在那份日志里；这里不再重复读。
func (s *Server) failShard(w http.ResponseWriter, r *http.Request, cluster, shard string, err error) {
	msg := publicError(err)
	switch {
	case errors.Is(err, dst.ErrInvalidArg):
		// 参数没过白名单校验：原值就在消息里，用户改一处就能重试。
		writeErr(w, http.StatusBadRequest, msg)
		return
	case errors.Is(err, procrun.ErrCommandMissing):
		// 与总览页同一句话：进程状态问不到，说"没在跑"比不说更糟。
		s.log().Error("screen 命令缺失", "path", r.URL.Path, "err", err.Error())
		writeErr(w, http.StatusServiceUnavailable,
			"面板找不到 screen 命令：需要和服务端在同一台机器上运行，且 PATH 里装有 screen")
		return
	case errors.Is(err, procrun.ErrPrereq):
		writeErr(w, http.StatusBadRequest, msg+"；这些条件补齐后就能启动，不用等面板重启")
		return
	case errors.Is(err, procrun.ErrStartRefused):
		// 实测到的形态：screen 会话一直留着，进程也活着，服务端却在日志里说"不会启动"。
		// 所以这句话必须同时交代"会话还挂着"——否则状态页显示运行中，
		// 用户改完令牌再点启动会撞上"已在运行"，两头对不上。
		s.log().Error("服务端拒绝启动", "path", r.URL.Path, "cluster", cluster, "shard", shard, "err", err.Error())
		writeErr(w, http.StatusConflict, msg+
			"；注意这个世界的 screen 会话还挂着（进程没退出），改好之后先在这里点\"强制停止\"再启动")
		return
	case errors.Is(err, procrun.ErrNotRunning):
		writeErr(w, http.StatusConflict, msg+"；先看这个世界的状态，确认后再启动")
		return
	case errors.Is(err, procrun.ErrAlreadyRunning):
		writeErr(w, http.StatusConflict, msg)
		return
	case errors.Is(err, procrun.ErrOperationInProgress):
		// 同一分片已有启停/重启在跑：409 而不是 500，也不是静默排队后再双开。
		writeErr(w, http.StatusConflict, msg+"；等上一次操作结束再试")
		return
	}
	s.log().Error("世界操作失败", "path", r.URL.Path, "cluster", cluster, "shard", shard, "err", err.Error())
	writeErr(w, http.StatusInternalServerError, msg)
}

// ---- 白名单控制台命令 ----

type consoleChoiceView struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type consoleArgView struct {
	Name    string              `json:"name"`
	Label   string              `json:"label"`
	Kind    string              `json:"kind"`
	Default string              `json:"default,omitempty"`
	Min     int64               `json:"min,omitempty"`
	Max     int64               `json:"max,omitempty"`
	MaxLen  int                 `json:"maxLen,omitempty"`
	Choices []consoleChoiceView `json:"choices,omitempty"`
}

type consoleCommandView struct {
	Key    string           `json:"key"`
	Label  string           `json:"label"`
	Desc   string           `json:"desc"`
	Danger bool             `json:"danger"`
	Args   []consoleArgView `json:"args"`
}

// handleAdminCommands 把白名单原样交给界面。
// 界面只能选这里列出的 key：任何界面手输的命令都不会被执行。
func (s *Server) handleAdminCommands(w http.ResponseWriter, r *http.Request) {
	keys := dst.ConsoleCommandKeys()
	out := make([]consoleCommandView, 0, len(keys))
	for _, key := range keys {
		spec, ok := dst.ConsoleCommandSpec(key)
		if !ok {
			continue
		}
		cv := consoleCommandView{Key: spec.Key, Label: spec.Label, Desc: spec.Desc, Danger: spec.Danger,
			Args: []consoleArgView{}}
		for _, a := range spec.Args {
			av := consoleArgView{Name: a.Name, Label: a.Label, Kind: a.Kind, Default: a.Default,
				Min: a.Min, Max: a.Max, MaxLen: a.MaxLen}
			for _, ch := range a.Choices {
				av.Choices = append(av.Choices, consoleChoiceView{Value: ch.Value, Label: ch.Label})
			}
			cv.Args = append(cv.Args, av)
		}
		out = append(out, cv)
	}
	writeOK(w, out)
}

type adminCommandRequest struct {
	Command string            `json:"command"`
	Args    map[string]string `json:"args"`
	Confirm bool              `json:"confirm"`
}

type adminCommandReply struct {
	Cluster string `json:"cluster"`
	Shard   string `json:"shard"`
	// Line 是实际注入会话的那一行，审计与界面都要能看到，不能只说"已执行"。
	Line string `json:"line"`
	Hint string `json:"hint,omitempty"`
}

// handleAdminCommandPost 走白名单：面板永远不把用户提供的字符串直接当 Lua 注入。
// Danger 命令必须在服务端也要 confirm，光靠前端弹窗等于没有约束。
func (s *Server) handleAdminCommandPost(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	var req adminCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	spec, exists := dst.ConsoleCommandSpec(req.Command)
	if !exists {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"命令 %q 不在白名单里，可用的是：%s", req.Command, strings.Join(dst.ConsoleCommandKeys(), "、")))
		return
	}
	if spec.Danger && !req.Confirm {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"命令「%s」会改存档或打断在线玩家，需要带 confirm=true 才会执行：%s", spec.Label, spec.Desc))
		return
	}
	line, err := s.Procs.RunAdminCommand(r.Context(), cluster, shard, req.Command, req.Args)
	s.audit(r, "admin."+req.Command, cluster+"/"+shard, line, err)
	if err != nil {
		s.failShard(w, r, cluster, shard, err)
		return
	}
	reply := adminCommandReply{Cluster: cluster, Shard: shard, Line: line}
	if spec.Danger {
		reply.Hint = "已注入，结果要看服务端日志或助手回传的数据确认"
	} else {
		reply.Hint = "已注入这个世界的控制台"
	}
	writeOK(w, reply)
}

// ---- 监控配置与告警视图（问题 4/5 的轻量控制口）----

// handleMonitor 一次返回监控配置、最近采样、当前告警与「期望运行」名单。
// 这是新增接口，既有 /api/v1/host、/api/v1/stats 的响应字段一个没动。
func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	if s.Monitor == nil {
		writeErr(w, http.StatusServiceUnavailable, "面板没有接上监控状态（裁剪版或测试夹具）")
		return
	}
	writeOK(w, s.Monitor.View())
}

// handleMonitorConfigPut 按补丁方式改自愈开关、阈值、Webhook 与停服等待上限，
// 改完立刻写盘、立刻生效。审计动作复用已登记的 settings.update，不动静态动作目录。
func (s *Server) handleMonitorConfigPut(w http.ResponseWriter, r *http.Request) {
	if s.Monitor == nil {
		writeErr(w, http.StatusServiceUnavailable, "面板没有接上监控配置")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读请求体失败: "+err.Error())
		return
	}
	if err := s.Monitor.Options().Update(body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "settings.update", "monitor", "更新监控/自愈配置", nil)
	writeOK(w, s.Monitor.Options().Get())
}
