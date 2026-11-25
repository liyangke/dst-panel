// 助手 MOD 与文件桥的接口层。
//
// 这一组的意义不只是"能点"：助手文件是 //go:embed 打进二进制的，只有真被代码引用到，
// 打包时才会留在成品里。之前这些方法只有测试在调，linker 把整块内嵌数据当死代码回收了，
// 于是"二进制里其实没有助手 MOD"。接线本身就是产物完整性的验证点。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"dstpanel/internal/modbridge"
	"dstpanel/internal/store"
)

// shardScope 校验 URL 里的集群名和分片名确实是磁盘上存在的那一个。
//
// 为什么必须在写之前查：{name}/{shard} 是用户可控的两段路径，后面所有操作都是在
// <klei>/<cluster>/<shard>/save/panel 下读写文件。段匹配虽然挡住了 "/"，
// 但 ".." 本身就是一个合法的路径段，不校验就等于让请求方能指定目录。
func (s *Server) shardScope(w http.ResponseWriter, r *http.Request) (cluster, shard string, ok bool) {
	cluster, shard = r.PathValue("name"), r.PathValue("shard")
	names, err := s.Paths.ClusterNames()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return "", "", false
	}
	if !contains(names, cluster) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("集群 %s 不存在，现有：%s", cluster, strings.Join(names, "、")))
		return "", "", false
	}
	if !contains(s.Paths.ShardNames(cluster), shard) {
		writeErr(w, http.StatusNotFound,
			fmt.Sprintf("%s 下没有世界 %s，现有：%s", cluster, shard, strings.Join(s.Paths.ShardNames(cluster), "、")))
		return "", "", false
	}
	return cluster, shard, true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---- 助手状态 ----

type helperFileView struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type helperView struct {
	Cluster string `json:"cluster"`
	Shard   string `json:"shard"`

	// 文件层：内嵌版本和磁盘版本的对账结果。四个桶各自独立，含义见 modbridge.HelperSync。
	Installed bool             `json:"installed"`
	Missing   []string         `json:"missing"`
	Differs   []string         `json:"differs"`
	Extra     []string         `json:"extra"`
	Files     []helperFileView `json:"files"`

	// 配置层：modoverrides.lua 里助手这一条的现状。
	Configured   bool `json:"configured"`
	Enabled      bool `json:"enabled"`
	PollSeconds  int  `json:"pollSeconds"`
	PlayerDetail bool `json:"playerDetail"`
	PollMin      int  `json:"pollMinSeconds"`
	PollMax      int  `json:"pollMaxSeconds"`

	// 运行层：MOD 有没有在回话。
	Online  bool    `json:"online"`
	AgeSecs float64 `json:"ageSeconds,omitempty"`
	Note    string  `json:"note,omitempty"`
	Hint    string  `json:"hint,omitempty"`
}

// buildHelperView 把三个来源拼成一份界面可直接渲染的结论。
// 任何一步失败都不影响其余部分——文件对账读不到时，启用状态和心跳仍然要说清楚，
// 否则一个目录权限问题会把整页变成空白，用户什么线索都拿不到。
func (s *Server) buildHelperView(cluster, shard string) (helperView, error) {
	v := helperView{
		Cluster: cluster, Shard: shard,
		Missing: []string{}, Differs: []string{}, Extra: []string{},
		PollMin: modbridge.MinHelperPollSeconds, PollMax: modbridge.MaxHelperPollSeconds,
	}
	var firstErr error

	sync, err := s.Bridge.SyncHelper(true)
	if err != nil {
		firstErr = err
		v.Note = publicError(err)
	} else {
		// modbridge 里这三个桶是 append 出来的，全空时是 nil；直接赋过去会让界面拿到 null。
		v.Missing = emptyIfNil(sync.Missing)
		v.Differs = emptyIfNil(sync.Differs)
		v.Extra = emptyIfNil(sync.Extra)
		v.Installed = len(sync.Missing) == 0
	}
	files, err := modbridge.HelperEmbeddedFiles()
	if err != nil {
		if firstErr == nil {
			firstErr = err
		}
	} else {
		rels := make([]string, 0, len(files))
		for rel := range files {
			rels = append(rels, rel)
		}
		sort.Strings(rels)
		for _, rel := range rels {
			v.Files = append(v.Files, helperFileView{Path: rel, SHA256: files[rel]})
		}
	}

	ov, err := s.Bridge.GetHelperOverride(cluster, shard)
	switch {
	case err != nil:
		if firstErr == nil {
			firstErr = err
		}
	case ov.Present:
		v.Configured, v.Enabled = ov.Present, ov.Enabled
		v.PollSeconds, v.PlayerDetail = ov.Settings.PollSeconds, ov.Settings.PlayerDetail
	default:
		// 没配过就回显面板将要用的默认值，界面不用自己猜一个数填进输入框。
		v.PollSeconds, v.PlayerDetail = s.Bridge.Helper.PollSeconds, s.Bridge.Helper.PlayerDetail
	}

	st, err := s.Bridge.Status(cluster, shard)
	online, note := helperState(st, err)
	v.Online = online
	if note != "" {
		v.Note = note
	}
	if err == nil {
		v.AgeSecs = st.Age.Seconds()
	}

	v.Hint = helperHint(v)
	return v, firstErr
}

// helperHint 只说下一步动作，不复述状态：文件没装 / 装了但没启用 / 启用了但没回话，
// 这三种要用户做的事完全不同，而前端很容易把 note 和 hint 显示成同一句话。
func helperHint(v helperView) string {
	switch {
	case !v.Installed:
		return "点启用就会把面板自带的助手文件写到服务端 MOD 目录"
	case len(v.Differs) > 0:
		return "磁盘上的助手文件和面板内置版本不一致，启用会覆盖成面板版本（原文件留 .bak）"
	case !v.Configured:
		return "启用后面板会往这个世界的 modoverrides.lua 加助手条目，需要重启世界才加载"
	case !v.Enabled:
		return "助手目前是关闭状态，开启后需重启世界生效"
	case !v.Online:
		return "配置都齐了，但游戏那边没传回数据：多半是还没进存档，或者助手没被加载。去「游戏日志」搜 DSTPanelHelper 能看到报错"
	}
	return "助手正常回话中"
}

func (s *Server) handleHelperShow(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	v, err := s.buildHelperView(cluster, shard)
	if err != nil {
		// 这里不整体失败：view 已经把拿到的部分填好了，只把底层错误记进运行日志。
		s.log().Warn("助手状态有一部分没读到", "cluster", cluster, "shard", shard, "err", err.Error())
	}
	writeOK(w, v)
}

type helperSetRequest struct {
	Enabled      *bool `json:"enabled"`
	PollSeconds  *int  `json:"pollSeconds"`
	PlayerDetail *bool `json:"playerDetail"`
}

func (s *Server) handleHelperSet(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	var req helperSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "必须给出 enabled（true 开启 / false 关闭）")
		return
	}
	// 没给字段就沿用现状，给了才改：界面只做"改哪填哪"，不用每次把三个值都提交回来。
	settings := s.Bridge.Helper
	if ov, err := s.Bridge.GetHelperOverride(cluster, shard); err == nil && ov.Present {
		settings = ov.Settings
	}
	if req.PollSeconds != nil {
		settings.PollSeconds = *req.PollSeconds
	}
	if req.PlayerDetail != nil {
		settings.PlayerDetail = *req.PlayerDetail
	}
	if err := settings.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	sync, err := s.Bridge.EnableHelper(cluster, shard, *req.Enabled, settings)
	s.audit(r, actionHelperSwitch(*req.Enabled), cluster+"/"+shard,
		fmt.Sprintf("写入 %d 个文件，轮询 %d 秒，玩家明细 %v", len(sync.Written), settings.PollSeconds, settings.PlayerDetail),
		err)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	v, _ := s.buildHelperView(cluster, shard)
	v.Hint = "配置已写入，重启这个世界后助手才会加载"
	writeOK(w, v)
}

func actionHelperSwitch(enabled bool) string {
	if enabled {
		return "helper.enable"
	}
	return "helper.disable"
}

// ---- 桥命令 ----

type commandView struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// ArgsHint 是界面上的占位提示。命令名与参数顺序由 panel_bridge.lua 的 handlers 决定，
	// 两边不一致时用户会看到 MOD 回的一句"参数不对"，所以这里把要求摊开写。
	ArgsHint string `json:"argsHint"`
}

var commandArgsHint = map[string]string{
	modbridge.CmdPing:     "无参数",
	modbridge.CmdStatus:   "无参数",
	modbridge.CmdMap:      "无参数；结果写到存档目录的 dstpanel_map.json，面板地图页会自动读取",
	modbridge.CmdAnnounce: "第 1 个参数：公告文字",
	modbridge.CmdKick:     "第 1 个参数：玩家名或 user id",
	modbridge.CmdMute:     "第 1 个参数：在线玩家的 user id 或玩家名",
	modbridge.CmdUnmute:   "第 1 个参数：在线玩家的 user id 或玩家名",
	modbridge.CmdBan:      "第 1 个参数：user id（不必在线）",
	modbridge.CmdUnban:    "第 1 个参数：user id",
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	out := make([]commandView, 0)
	for _, name := range modbridge.CommandNames() {
		out = append(out, commandView{
			Name: name, Label: modbridge.CommandLabel(name), ArgsHint: commandArgsHint[name],
		})
	}
	writeOK(w, out)
}

type commandRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	// WaitSeconds > 0 时等到回执再返回；0 表示只入队（后端命令排队久时用这个，不占着连接）。
	WaitSeconds int `json:"waitSeconds"`
}

type commandReply struct {
	ID     string          `json:"id"`
	Done   bool            `json:"done"`
	Result json.RawMessage `json:"result,omitempty"`
	// Pending 是入队后还留在这条队列里的命令数，包含刚提交的这条。
	Pending int    `json:"pending,omitempty"`
	Note    string `json:"note,omitempty"`
}

func (s *Server) handleCommandPost(w http.ResponseWriter, r *http.Request) {
	cluster, shard, ok := s.shardScope(w, r)
	if !ok {
		return
	}
	var req commandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeErr(w, http.StatusBadRequest, "要指定命令名")
		return
	}
	if req.WaitSeconds < 0 || req.WaitSeconds > 120 {
		writeErr(w, http.StatusBadRequest, "等待回执的秒数要在 0~120 之间")
		return
	}

	// 命令白名单在 Enqueue 里挡（modbridge.CommandNames），这里不再自己列一份，
	// 免得两边各有一套名单、加命令时漏改一边。
	id, err := s.Bridge.Enqueue(cluster, shard, req.Command, req.Args...)
	if err != nil {
		writeErr(w, http.StatusBadRequest, publicError(err))
		return
	}
	s.audit(r, "bridge."+req.Command, cluster+"/"+shard, strings.Join(req.Args, " | "), err)

	rep := commandReply{ID: id}
	if req.WaitSeconds == 0 {
		if pending, perr := s.Bridge.Pending(cluster, shard); perr == nil {
			rep.Pending = len(pending)
		}
		rep.Note = "命令已入队，未等待回执"
		writeOK(w, rep)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.WaitSeconds)*time.Second)
	defer cancel()
	res, err := s.Bridge.Wait(ctx, cluster, shard, id, time.Duration(req.WaitSeconds)*time.Second)
	if err != nil {
		// 超时和"助手没回话"都还是 200 之前的状态问题：命令可能已经在队列里了，
		// 说成 5xx 会让用户去重试同一个操作，反而堆两份命令。
		switch {
		case errors.Is(err, modbridge.ErrHelperOffline):
			s.log().Warn("等回执时助手无响应", "cluster", cluster, "shard", shard, "id", id, "err", err.Error())
			writeErr(w, http.StatusServiceUnavailable, publicError(err))
		case errors.Is(err, modbridge.ErrCommandPending),
			errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			// 202 而不是错误：命令确实已经交上去了，只是还没回执。
			// 回 5xx 会让用户以为失败而重复点，队列里就多一条同样的公告/踢人。
			writeJSON(w, http.StatusAccepted, reply{OK: true, Data: commandReply{
				ID:   id,
				Note: fmt.Sprintf("等了 %d 秒还没有回执，命令仍在队列里，助手恢复后可能仍会执行", req.WaitSeconds),
			}})
		default:
			s.fail(w, r, http.StatusInternalServerError, err)
		}
		return
	}

	rep.Done = true
	rep.Result = res.Value
	if !res.OK {
		// MOD 回了话但执行失败（比如要踢的玩家不在线）：这是业务结论，不是接口错误。
		rep.Note = "助手执行失败：" + res.Error
	}
	// 回执已到手就把这条从队列里裁掉，否则队列只会长。Trim 只删已执行的行，安全。
	if _, err := s.Bridge.Trim(cluster, shard); err != nil {
		s.log().Warn("命令队列裁剪失败", "cluster", cluster, "shard", shard, "err", err.Error())
	}
	writeOK(w, rep)
}

// audit 记一条平台操作记录。
//
// 记不上日志时只报警不断言失败：这里的调用点全都在副作用已经发生之后
// （文件已写、命令已入队），把"已经做成"回成 500 会让用户以为没生效而重复操作。
func (s *Server) audit(r *http.Request, action, target, detail string, cause error) {
	if s.Store == nil {
		return
	}
	actor, _, ok := r.BasicAuth()
	if !ok || actor == "" {
		actor = "system"
	}
	auditErr := ""
	if cause != nil {
		auditErr = cause.Error()
	}
	err := s.Store.AppendAudit(store.AuditEntry{
		Actor:  actor,
		Action: action,
		Target: target,
		Detail: detail,
		OK:     cause == nil,
		Err:    auditErr,
	})
	if err != nil {
		s.log().Error("操作记录写入失败", "action", action, "target", target, "err", err.Error())
	}
}
