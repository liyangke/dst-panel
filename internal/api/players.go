// 玩家管理：在线玩家的即时操作 + 三份游戏身份名单。
//
// 这里的"玩家"有两条完全不同的通道，界面上必须分开说：
//   - 在线操作（禁言/踢出/拉黑）走助手 MOD，只对已经在线的角色有效，立即生效；
//   - 名单文件（管理员/白名单/黑名单）是 Klei 引擎在启动时读的，写进去不等于马上生效。
//
// 面板账号是第三套东西（见 accounts.go），三者互不自动绑定：
// 在面板里登录成功不代表游戏里有管理员，反过来也一样。
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

	"dstpanel/internal/dst"
	"dstpanel/internal/modbridge"
	"dstpanel/internal/store"
)

type listEntryView struct {
	UserID string `json:"userid"`
	Name   string `json:"name,omitempty"`
	Online bool   `json:"online"`
	// LastSeenAt 给"只出现在名单里、从没上线"的人留空，界面据此显示"没见过上线"。
	LastSeenAt string `json:"lastSeenAt,omitempty"`
}

type playerListGroup struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	File  string `json:"file"`
	// Effect 是这份名单什么时候生效。三份文件的行为不一样，不能共用一句话。
	Effect  string          `json:"effect"`
	Entries []listEntryView `json:"entries"`
}

type onlinePlayerView struct {
	UserID    string   `json:"userid"`
	Name      string   `json:"name"`
	Prefab    string   `json:"prefab"`
	Shard     string   `json:"shard"`
	Health    *float64 `json:"health,omitempty"`
	MaxHealth *float64 `json:"maxHealth,omitempty"`
	Hunger    *float64 `json:"hunger,omitempty"`
	MaxHunger *float64 `json:"maxHunger,omitempty"`
	Sanity    *float64 `json:"sanity,omitempty"`
	MaxSanity *float64 `json:"maxSanity,omitempty"`
	X         *float64 `json:"x,omitempty"`
	Z         *float64 `json:"z,omitempty"`
	Dead      bool     `json:"dead"`
}

type rosterEntryView struct {
	UserID      string `json:"userid"`
	Name        string `json:"name,omitempty"`
	Prefab      string `json:"prefab,omitempty"`
	LastShard   string `json:"lastShard,omitempty"`
	Online      bool   `json:"online"`
	Admin       bool   `json:"admin"`
	Whitelisted bool   `json:"whitelisted"`
	Blocked     bool   `json:"blocked"`
	FirstSeenAt string `json:"firstSeenAt,omitempty"`
	LastSeenAt  string `json:"lastSeenAt,omitempty"`
}

type playersView struct {
	Cluster       string             `json:"cluster"`
	Lists         []playerListGroup  `json:"lists"`
	Online        []onlinePlayerView `json:"online"`
	Roster        []rosterEntryView  `json:"roster"`
	RunningShards []string           `json:"runningShards"`
	// PausedShards 是"进程还在跑、日志最后一句是暂停标记"的世界。
	// 单独给一列而不是只写进 note：界面上"此刻在线 0"这一格必须能区分
	// "真的没人"和"暂停时读不到"，否则用户看到的就是一个假空列表。
	PausedShards []string `json:"pausedShards"`
	Note         string   `json:"note"`
}

var playerListEffect = map[string]string{
	dst.PlayerListAdmin:     "写进 adminlist.txt。引擎在玩家加入时读这份名单，改完让玩家重新连接；还不生效就重启世界。",
	dst.PlayerListWhitelist: "写进 whitelist.txt。引擎启动时读这份名单，改完必须重启世界；cluster.ini 里的白名单保留位也要够。",
	dst.PlayerListBlocklist: "写进 blocklist.txt。从面板在线拉黑会立刻生效；手工改这份文件要重启世界。",
}

// onlineByUserID 汇总所有正在回话的分片里的玩家。
//
// 只认"新鲜且采集没报错"的快照：助手卡死时最后一份快照里的玩家可能早就下线了，
// 拿那份数据去做禁言/踢人，用户看到的是"操作成功"但目标根本不存在。
func (s *Server) onlineByUserID(cluster string) (map[string]onlinePlayerView, []string) {
	out := make(map[string]onlinePlayerView)
	// 空的时候必须是空切片：nil 切片进 JSON 是 null，而这一页的模板直接读
	// view.runningShards.length —— 云机上两个世界都暂停时整页渲染崩在 Vue 里。
	running := []string{}
	for _, shard := range s.Paths.ShardNames(cluster) {
		st, err := s.Bridge.Status(cluster, shard)
		if err != nil || st.Stale || st.Error != "" {
			continue
		}
		running = append(running, shard)
		for _, p := range st.Players {
			if p.UserID == "" {
				continue
			}
			out[p.UserID] = onlinePlayerView{
				UserID: p.UserID, Name: p.Name, Prefab: p.Prefab, Shard: shard,
				Health: p.Health, MaxHealth: p.MaxHealth,
				Hunger: p.Hunger, MaxHunger: p.MaxHunger,
				Sanity: p.Sanity, MaxSanity: p.MaxSanity,
				X: p.X, Z: p.Z, Dead: p.IsDead,
			}
		}
	}
	return out, running
}

func (s *Server) buildPlayersView(ctx context.Context, cluster string) (playersView, error) {
	v := playersView{Cluster: cluster, Lists: []playerListGroup{}, Online: []onlinePlayerView{},
		Roster: []rosterEntryView{}, PausedShards: []string{}}
	seen := map[string]store.SeenPlayer{}
	if s.Store != nil {
		rows, err := s.Store.ListSeenPlayers(cluster, 1000)
		if err != nil {
			return v, err
		}
		for _, p := range rows {
			seen[p.UserID] = p
		}
	}
	online, running := s.onlineByUserID(cluster)
	v.RunningShards = running
	v.PausedShards = s.pausedShards(ctx, cluster)

	order := []string{dst.PlayerListAdmin, dst.PlayerListWhitelist, dst.PlayerListBlocklist}
	flags := map[string]map[string]bool{}
	for _, kind := range order {
		path, _ := s.Paths.PlayerListFile(cluster, kind)
		ids, err := dst.ReadPlayerList(path)
		if err != nil {
			return v, err
		}
		set := make(map[string]bool, len(ids))
		group := playerListGroup{
			Key: kind, Label: dst.LabelPlayerList(kind), File: path,
			Effect: playerListEffect[kind], Entries: make([]listEntryView, 0, len(ids)),
		}
		for _, id := range ids {
			set[id] = true
			e := listEntryView{UserID: id}
			if p, ok := seen[id]; ok {
				e.Name = p.Name
				if !p.LastSeen.IsZero() {
					e.LastSeenAt = p.LastSeen.UTC().Format(time.RFC3339)
				}
			}
			if o, ok := online[id]; ok {
				e.Online = true
				if e.Name == "" {
					e.Name = o.Name
				}
			}
			group.Entries = append(group.Entries, e)
		}
		flags[kind] = set
		v.Lists = append(v.Lists, group)
	}

	for _, o := range online {
		v.Online = append(v.Online, o)
	}
	sort.Slice(v.Online, func(i, j int) bool {
		if v.Online[i].Shard != v.Online[j].Shard {
			return v.Online[i].Shard < v.Online[j].Shard
		}
		return v.Online[i].Name < v.Online[j].Name
	})

	// 花名册 = 见过的 ∪ 名单里出现过的 ∪ 现在在线的。
	// 三处都可能有人只出现在其中一处：刚被手工加进名单的 ID 还没上线过；
	// 在线玩家的第一份快照还没被采样进库。
	ids := map[string]bool{}
	for id := range seen {
		ids[id] = true
	}
	for id := range online {
		ids[id] = true
	}
	for _, set := range flags {
		for id := range set {
			ids[id] = true
		}
	}
	for id := range ids {
		p := seen[id]
		e := rosterEntryView{
			UserID: id, Name: p.Name, Prefab: p.Prefab, LastShard: p.LastShard,
			Admin: flags[dst.PlayerListAdmin][id], Whitelisted: flags[dst.PlayerListWhitelist][id],
			Blocked: flags[dst.PlayerListBlocklist][id],
		}
		if !p.FirstSeen.IsZero() {
			e.FirstSeenAt = p.FirstSeen.UTC().Format(time.RFC3339)
		}
		if !p.LastSeen.IsZero() {
			e.LastSeenAt = p.LastSeen.UTC().Format(time.RFC3339)
		}
		if o, ok := online[id]; ok {
			e.Online = true
			e.LastShard = o.Shard
			if e.Name == "" {
				e.Name = o.Name
			}
			if e.Prefab == "" {
				e.Prefab = o.Prefab
			}
			// 在线的人一定"刚刚见过"，哪怕采样还没写库。
			e.LastSeenAt = time.Now().UTC().Format(time.RFC3339)
		}
		v.Roster = append(v.Roster, e)
	}
	sort.Slice(v.Roster, func(i, j int) bool {
		a, b := v.Roster[i], v.Roster[j]
		if a.Online != b.Online {
			return a.Online
		}
		if a.LastSeenAt != b.LastSeenAt {
			return a.LastSeenAt > b.LastSeenAt
		}
		return a.UserID < b.UserID
	})

	switch {
	case len(running) == 0 && len(v.PausedShards) > 0:
		v.Note = "这些世界正被引擎暂停着（没人时会自动暂停），暂停期间引擎连助手的调度一起冻住，所以读不到在线名单——" +
			"上面的空列表不代表没人在玩。有人进来自会自动恢复；在线操作要等恢复之后再点。"
	case len(running) == 0:
		v.Note = "没有世界在回话：在线操作不可用；名单文件仍然可以改，改完在世界启动时生效。"
	default:
		v.Note = "在线操作（禁言、踢出、拉黑）走助手 MOD，只对正在回话的世界有效；名单文件是另外一套，生效时间看每份名单下面的说明。"
	}
	return v, nil
}

// pausedShards 从每个世界的服务端日志里读出"此刻是暂停还是恢复"。
//
// 问日志是因为"进程活着 + 助手没心跳"有两种完全不同的含义（真卡死 / 只是没人玩），
// 只有日志里那句暂停标记分得开（见 procrun.SimPausedNote）。
// 但先要问进程：停掉的世界最后一句照样是 Server Autopaused，
// 那时候说"有人进来自会自动恢复"就是在骗用户——它已经不在跑了。
func (s *Server) pausedShards(ctx context.Context, cluster string) []string {
	out := []string{}
	if s.Procs == nil {
		return out
	}
	for _, shard := range s.Paths.ShardNames(cluster) {
		if running, err := s.Procs.IsRunning(ctx, cluster, shard); err != nil || !running {
			continue
		}
		if _, paused := s.Procs.SimPausedNote(cluster, shard); paused {
			out = append(out, shard)
		}
	}
	return out
}

func (s *Server) handlePlayers(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	v, err := s.buildPlayersView(r.Context(), cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeOK(w, v)
}

type playerListRequest struct {
	Action string `json:"action"`
	UserID string `json:"userid"`
}

func (s *Server) handlePlayerList(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if !dst.ValidPlayerList(kind) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"名单种类只有 admin（管理员）、whitelist（白名单）、blocklist（黑名单），收到的是 %q", kind))
		return
	}
	var req playerListRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.Action != "add" && req.Action != "remove" {
		writeErr(w, http.StatusBadRequest, "action 只能是 add 或 remove")
		return
	}
	userID, err := dst.NormalizePlayerID(req.UserID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	path, _ := s.Paths.PlayerListFile(cluster, kind)
	ids, err := dst.ReadPlayerList(path)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	next, changed := toggleID(ids, userID, req.Action == "add")
	if changed {
		err = dst.WritePlayerList(path, next)
		s.audit(r, "player.list."+req.Action, cluster+"/"+kind, userID, err)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
	} else {
		// 已经是目标状态就不重写文件：重写会多一份 .bak，
		// 也让"这个 ID 是不是本来就写着"变得看不出来。
		s.audit(r, "player.list."+req.Action, cluster+"/"+kind, userID+"（原本就是这样）", nil)
	}
	v, verr := s.buildPlayersView(r.Context(), cluster)
	if verr != nil {
		s.fail(w, r, http.StatusInternalServerError, verr)
		return
	}
	writeOK(w, v)
}

// toggleID 在保持原顺序的前提下增删一个 ID。
func toggleID(ids []string, id string, add bool) ([]string, bool) {
	if add {
		for _, x := range ids {
			if x == id {
				return ids, false
			}
		}
		return append(ids, id), true
	}
	out := make([]string, 0, len(ids))
	removed := false
	for _, x := range ids {
		if x == id {
			removed = true
			continue
		}
		out = append(out, x)
	}
	return out, removed
}

// 在线动作到桥命令的映射。键也是 URL 里 {action} 的取值；
// 没列在这里的动作一律拒绝，避免把 URL 段当成任意命令转发。
var playerActions = map[string]string{
	"kick":   modbridge.CmdKick,
	"mute":   modbridge.CmdMute,
	"unmute": modbridge.CmdUnmute,
	"ban":    modbridge.CmdBan,
	"unban":  modbridge.CmdUnban,
}

var playerActionLabels = map[string]string{
	"kick":   "踢出玩家",
	"mute":   "禁言玩家",
	"unmute": "解除禁言",
	"ban":    "拉黑玩家",
	"unban":  "解除拉黑",
}

type playerActionRequest struct {
	Confirm bool `json:"confirm"`
}

// handlePlayerAction 把"对某个 userid 做一件事"变成一条桥命令。
//
// 为什么要按 userid 而不是玩家名：名字可以改，改名后按名字操作会打到别人身上；
// userid 是引擎和名单文件共用的稳定标识。踢/禁言必须在线，封禁/解封可以离线。
func (s *Server) handlePlayerAction(w http.ResponseWriter, r *http.Request) {
	cluster, ok := s.clusterScope(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	command, exists := playerActions[action]
	if !exists {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"玩家动作只有 kick（踢出）、mute（禁言）、unmute（解除禁言）、ban（拉黑）、unban（解除拉黑），收到的是 %q", action))
		return
	}
	userID, err := dst.NormalizePlayerID(r.PathValue("userid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var req playerActionRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
			return
		}
	}
	// 踢人和拉黑会打断玩家，必须和别的危险动作一样在服务端再确认一次：
	// 光靠前端弹窗，等于把"有没有确认"交给调用方自觉。
	if (action == "kick" || action == "ban") && !req.Confirm {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("「%s」需要在请求体里带 confirm=true", playerActionLabels[action]))
		return
	}

	online, running := s.onlineByUserID(cluster)
	target, isOnline := online[userID]
	var shard string
	switch {
	case isOnline:
		shard = target.Shard
	case action == "kick" || action == "mute" || action == "unmute":
		writeErr(w, http.StatusBadRequest, "这个玩家现在不在线：踢出和禁言只对在线角色有效")
		return
	case len(running) > 0:
		// 离线封禁/解封不关心玩家在哪个分片，挑一个正在回话的就行。
		shard = running[0]
	default:
		// 进程活着但世界被暂停时，"先把世界开起来"是错的建议：用户会去重启一个本来好好的服务器。
		// 反过来说，这一句只在进程确实活着时才有资格讲——pausedShards 里就带了这道门。
		if paused := s.pausedShards(r.Context(), cluster); len(paused) > 0 {
			writeErr(w, http.StatusServiceUnavailable,
				"这些世界正被引擎暂停着（"+strings.Join(paused, "、")+"），暂停期间助手不执行任何命令；等有人进来恢复运行后再试")
		} else {
			writeErr(w, http.StatusServiceUnavailable,
				"没有世界在回话：离线封禁要由游戏进程写进黑名单，先把世界开起来")
		}
		return
	}

	id, err := s.Bridge.Enqueue(cluster, shard, command, userID)
	s.audit(r, "bridge."+command, cluster+"/"+shard, userID, err)
	if err != nil {
		writeErr(w, http.StatusBadRequest, publicError(err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := s.Bridge.Wait(ctx, cluster, shard, id, 10*time.Second)
	if err != nil {
		switch {
		case errors.Is(err, modbridge.ErrHelperOffline):
			writeErr(w, http.StatusServiceUnavailable, publicError(err))
		case errors.Is(err, modbridge.ErrCommandPending),
			errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			// 202：命令确实交上去了，只是没等到回执。回 5xx 会让人再点一次，
			// 队列里就多一条重复的踢人/拉黑。
			writeJSON(w, http.StatusAccepted, reply{OK: true, Data: map[string]any{
				"id": id, "shard": shard,
				"note": "命令已入队，助手还没回执；它恢复后可能仍会执行",
			}})
		default:
			s.fail(w, r, http.StatusInternalServerError, err)
		}
		return
	}
	_, _ = s.Bridge.Trim(cluster, shard)
	if !res.OK {
		writeErr(w, http.StatusBadRequest, "游戏侧没执行："+res.Error)
		return
	}
	writeOK(w, map[string]any{"id": id, "shard": shard, "result": res.Value})
}
