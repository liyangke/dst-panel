package api

// 房间归属的权限层。
//
// 这一层只回答一个问题：这个账号对这间房能看 / 能改吗。
//
// 为什么做成一个中间件，而不是在每个处理器里各判一次：
// /api/v1/clusters/{name}/... 已经有三十多条，逐条加判断迟早会漏一条，
// 而"漏鉴权"恰好是最难发现的那类问题——接口自己跑得通、界面也点得动，
// 只有第二个账号登进来才暴露，而那时往往已经写坏了别人的房间。
//
// 为什么两条判断线分开放：
//   - 只读账号（viewer）的限制和"哪间房"无关，留在 authenticate 里一句话判完；
//   - 成员（member）的限制必须先知道是哪间房，所以统一在这里判。
//     authenticate 放行它的写请求到这一层，两层各管一件事，
//     不会出现"两边都以为对方管了"，也不会出现"两边都管了但说法不一样"。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"dstpanel/internal/store"
)

// roomPathPrefix 是"这条请求的主语是一间房"的判断依据。
// 结尾那道斜杠不能省：POST /api/v1/clusters（建房间）不属于任何一间房，
// 它是"这台机器上多一个目录"，归管理员。
const roomPathPrefix = "/api/v1/clusters/"

// memberGlobalWriteNote 是成员碰机器级写操作时的那句话。
// 写一次、两处复用（中间件和房间外的接口），免得同一个拒因说出两种理由。
const memberGlobalWriteNote = "成员只能改指派给自己名下的房间；面板设置、账号、模组库和机器级任务都归管理员"

func (s *Server) guardRooms(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := sessionOf(r)
		// 管理员不用判；没有账号库的接线（测试或裁剪版）里根本没有归属这回事，
		// 而那种接线下能登录的只有配置文件里那一个管理员。
		if s.Store == nil || sess.Role == store.RoleAdmin {
			next.ServeHTTP(w, r)
			return
		}
		// 指派归属虽然挂在房间路径下，但它回答的是"这台机器怎么分工"，
		// 不是房间里的日常操作：成员把自己名下的房子转给别人，等于绕开了管理员。
		if strings.HasSuffix(r.URL.Path, "/owner") {
			if isWrite(r) {
				writeErr(w, http.StatusForbidden, "指派房间归属是管理员的决定，成员改不了")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		cluster := s.requestCluster(r)
		if cluster == "" {
			// 到这里的都是机器级的口子。改自己的密码是唯一例外——
			// 把它也拦住的话，成员只能找管理员重置密码。
			if r.URL.Path == "/api/v1/session/password" {
				next.ServeHTTP(w, r)
				return
			}
			if isWrite(r) {
				writeErr(w, http.StatusForbidden, memberGlobalWriteNote)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		owner, err := s.ownerOf(cluster)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		switch {
		case owner == nil:
			// 没指派不等于谁都能改。宁可让用户先去找管理员，也不能让两个成员
			// 在同一间房上互相覆盖——那正是"归属"要解决的问题。
			if isWrite(r) {
				writeErr(w, http.StatusForbidden, unownedWriteNote(cluster))
				return
			}
		case owner.AccountID == sess.ID:
			// 自己名下的房间：成员能做的和管理员一样（除开归属本身）。
		case sess.Role == store.RoleMember:
			// 归别人的房间，对成员连读也不给：只在列表里过滤掉、按名字直接问还答得出来，
			// 那层"看不到"就是假的。
			writeErr(w, http.StatusForbidden, roomNotYoursNote(cluster, ownerTitle(*owner)))
			return
		default:
			// 只读账号的写请求正常在 authenticate 就被挡了，这里再兜一层：
			// 将来若有人放宽那条判断，这一层仍然说得出"不能改别人的房间"。
			if isWrite(r) {
				writeErr(w, http.StatusForbidden,
					roomOwnedByOtherWriteNote(cluster, ownerTitle(*owner)))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func unownedWriteNote(cluster string) string {
	return cluster + " 还没指派给谁，成员改不了它。请管理员在房间列表那一行点「指派」，把它划到你名下"
}

func roomNotYoursNote(cluster, owner string) string {
	return cluster + " 不在你的房间名单里（归 " + owner + " 管）"
}

func roomOwnedByOtherWriteNote(cluster, owner string) string {
	return cluster + " 归 " + owner + " 管，只读账号不做修改"
}

func isWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// ownerTitle 是给用户看的归属说法。
//
// AccountName 为空只有一种来路：归属行指向一个已经不在库里的账号（手工动过数据库）。
// 那时说"已不在"比编一个名字诚实，而且这句恰好就是用户该去做的事。
func ownerTitle(o store.ClusterOwner) string {
	if strings.TrimSpace(o.AccountName) == "" {
		return "一个已经不存在的账号（请管理员重新指派）"
	}
	return o.AccountName
}

// requestCluster 找出这条请求的主语房间。
//
// 三种形态：路径里就写着房间名；路径写的是快照编号（存档那组接口）；
// 以及导入——它连快照都还没有，目标房间只在请求体里。
// 房间之外的请求返回空串，由调用方按"机器级"处理。
func (s *Server) requestCluster(r *http.Request) string {
	if n := clusterFromPath(r.URL.Path); n != "" {
		return n
	}
	// 取消任务这条：主语是"某间房的一次操作"，房间写在任务的 payload 里。
	// 不解决的话成员连自己房间里排队的备份都撤不回（那条口子会被当成机器级拒掉），
	// 而界面上「取消」按钮对管理员是亮的、对成员永远灰着，说不通。
	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/tasks/"); ok {
		id, err := strconv.ParseInt(strings.TrimSuffix(rest, "/"), 10, 64)
		if err != nil || s.Store == nil {
			return ""
		}
		task, err := s.Store.GetTask(id)
		if err != nil {
			// 查不到就按机器级处理：让处理器自己回那句"任务不存在"，
			// 权限层不替它编一个"归谁管"。
			return ""
		}
		return payloadCluster(task.Payload)
	}
	if s.Backups == nil {
		return ""
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/backups/")
	if !ok {
		return ""
	}
	seg := rest
	if i := strings.Index(seg, "/"); i >= 0 {
		seg = seg[:i]
	}
	// 导入这两个口子没有快照可查：预览还没有目标房间（它只是把包收下），
	// 提交的目标房间在请求体里。
	if seg == "import" || seg == "" {
		return peekRequestCluster(r)
	}
	// 跨房间迁移时"要改的是目标房间"，所以请求体里的 targetCluster
	// 优先于快照自己的房间。
	if target := peekRequestCluster(r); target != "" {
		return target
	}
	snap, err := s.Backups.Get(seg)
	if err != nil {
		// 查不到就当没有房间主语：让处理器自己回那句"这份备份不存在"，
		// 权限层不替它编一个"归谁管"的结论。
		return ""
	}
	return snap.Cluster
}

func clusterFromPath(path string) string {
	rest, ok := strings.CutPrefix(path, roomPathPrefix)
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return ""
	}
	// 房间名从 URL 上来，百分号编码要先解出来才和磁盘上的目录名对得上：
	// 不解的话 "%43luster_1" 会绕过归属判断、又在处理器那边报"房间不存在"。
	name, err := url.PathUnescape(rest)
	if err != nil {
		return ""
	}
	return name
}

// peekRequestCluster 只读一眼 JSON 请求体里的房间字段，读完立刻把同一份字节塞回 r.Body，
// 处理器再解一遍拿到的还是原文。
//
// 为什么不在处理器里判：那样就回到"每条接口各判一次"的老路。
// 为什么带体积和类型两道闸：超过一米或不是 JSON 的请求（存档预览是 multipart 上传）
// 不是这一层该读的东西；判不出房间主语就按机器级处理，成员被拦在外面——
// 宁可多拦一次，也不放过一次越权。
func peekRequestCluster(r *http.Request) string {
	const peekLimit = 1 << 20
	if r.Body == nil || r.ContentLength <= 0 || r.ContentLength > peekLimit {
		return ""
	}
	if !strings.Contains(r.Header.Get("Content-Type"), "json") {
		return ""
	}
	data, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	var in struct {
		Cluster       string `json:"cluster"`
		TargetCluster string `json:"targetCluster"`
	}
	if json.Unmarshal(data, &in) != nil {
		return ""
	}
	if strings.TrimSpace(in.TargetCluster) != "" {
		return strings.TrimSpace(in.TargetCluster)
	}
	return strings.TrimSpace(in.Cluster)
}

// ownerOf 取一间的归属；库里没有这行回 nil, nil。
func (s *Server) ownerOf(cluster string) (*store.ClusterOwner, error) {
	owners, err := s.roomOwners()
	if err != nil {
		return nil, err
	}
	o, ok := owners[normalizeClusterName(cluster)]
	if !ok {
		return nil, nil
	}
	return &o, nil
}

// roomOwners 一次取全量归属表。
//
// 每个请求一次查询，换掉"每个房间一次查询"：这几条接口是五秒一轮的轮询目标，
// 而房间数在这台机器上会上到两位数。
// 键统一折成小写：cluster_owners 的主键带 COLLATE NOCASE，Go 的 map 却分大小写，
// 不折一下就会出现"库里明明有这行、界面说未指派"。
func (s *Server) roomOwners() (map[string]store.ClusterOwner, error) {
	if s.Store == nil {
		return map[string]store.ClusterOwner{}, nil
	}
	all, err := s.Store.ClusterOwners()
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.ClusterOwner, len(all))
	for name, o := range all {
		out[normalizeClusterName(name)] = o
	}
	return out, nil
}

func normalizeClusterName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// visibleClusters 按角色筛房间名单：成员只看得到"自己名下的 + 还没指派的"。
//
// 为什么"未指派"也给成员看：房间是磁盘事实，装面板之前就有几个目录，
// 把它们对成员藏起来等于让他以为这台机器上只有两间房——他第一步就会问"另外几间呢"。
// 看得见但改不动（guardRooms 那条判断）才是说得通的状态。
func (s *Server) visibleClusters(r *http.Request, names []string) []string {
	sess := sessionOf(r)
	if s.Store == nil || sess.Role != store.RoleMember {
		return names
	}
	owners, err := s.roomOwners()
	if err != nil {
		// 读不到归属表时不能"当作全都可见"放过去：那等于在最坏的情况下给出最大权限。
		// 这里筛成只剩没归属的那些，并且把失败记进运行日志。
		s.log().Error("筛可见房间时读归属表失败，按最小可见集继续", "err", err.Error())
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		o, ok := owners[normalizeClusterName(n)]
		if err != nil && ok {
			continue
		}
		if !ok || o.AccountID == sess.ID {
			out = append(out, n)
		}
	}
	return out
}

// roomVisible 是"参数里写着房间名"那批接口的单间版本（?cluster=xxx）。
func (s *Server) roomVisible(r *http.Request, cluster string) (bool, error) {
	sess := sessionOf(r)
	if s.Store == nil || sess.Role != store.RoleMember {
		return true, nil
	}
	o, err := s.ownerOf(cluster)
	if err != nil {
		return false, err
	}
	return o == nil || o.AccountID == sess.ID, nil
}

// denyInvisibleRoom 把"你问的那间房不在你名单里"说成一句能动手的话。
// 返回 false 表示已经写好响应，调用方直接 return。
func (s *Server) denyInvisibleRoom(w http.ResponseWriter, r *http.Request, cluster string) bool {
	ok, err := s.roomVisible(r, cluster)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return false
	}
	if ok {
		return true
	}
	o, oerr := s.ownerOf(cluster)
	if oerr != nil {
		s.fail(w, r, http.StatusInternalServerError, oerr)
		return false
	}
	if o == nil {
		writeErr(w, http.StatusForbidden, cluster+" 还没指派给谁，成员只能看不能改它")
		return false
	}
	writeErr(w, http.StatusForbidden, roomNotYoursNote(cluster, ownerTitle(*o)))
	return false
}

// taskVisible 判一条任务该不该出现在这个账号的列表里。
//
// 任务的房间参数在 payload 里（那串 JSON 由后端自己拼，字段名是有限集合：
// cluster / targetCluster），判不出房间的任务就是机器级任务
// （更新服务端本体、清理备份），对成员一律不显示：
// 那种动作轮不到他做，列出来只会变成"这条是什么、我能不能点"。
//
// 成员这里按"我自己名下的房间"筛，比列表页那条更严一档，理由是形状不同：
// 未指派的房间对成员是"看得见但改不动"，那没问题——它是磁盘事实，藏起来会让他
// 以为这台机器上少了几间房。任务是一件正在进行的操作，列表上每一行都跟着一个「取消」，
// 把别人（或没人）负责的房间的操作摆在他面前，就是给他一个按下去会被拒的按钮。
func (s *Server) taskVisible(r *http.Request, payload string) bool {
	sess := sessionOf(r)
	if s.Store == nil || sess.Role != store.RoleMember {
		return true
	}
	cluster := payloadCluster(payload)
	if cluster == "" {
		return false
	}
	owner, err := s.ownerOf(cluster)
	if err != nil {
		// 读不出归属时按最小可见集：宁可让他看不到自己的任务，也不能看到别人的。
		s.log().Error("筛任务时读归属表失败，按最小可见集继续", "err", err.Error())
		return false
	}
	return owner != nil && owner.AccountID == sess.ID
}

func payloadCluster(payload string) string {
	if strings.TrimSpace(payload) == "" {
		return ""
	}
	var in struct {
		Cluster       string `json:"cluster"`
		TargetCluster string `json:"targetCluster"`
	}
	if err := json.Unmarshal([]byte(payload), &in); err != nil {
		return ""
	}
	// 导入与恢复任务同时带着"快照来自哪间"和"要落到哪间"，要筛的是后者：
	// 动作改的是目标房间。
	if strings.TrimSpace(in.TargetCluster) != "" {
		return strings.TrimSpace(in.TargetCluster)
	}
	return strings.TrimSpace(in.Cluster)
}

// ownerLabel 是房间列表那一行显示的归属说法。
//
// 由后端拼而不是前端按 account_id 去账号列表里找名字：成员拿不到账号列表，
// 前端找就必然找出空字符串，界面上会显示成"归属："后面什么都没有。
func ownerLabel(o *store.ClusterOwner) string {
	if o == nil {
		return "未指派"
	}
	if strings.TrimSpace(o.AccountName) == "" {
		return ownerTitle(*o)
	}
	return "归 " + o.AccountName + " 管"
}

// ownersByAccount 给账号列表那一页用：一个账号名下有哪几间房。
// 数据来自同一份归属表，不为每个账号再查一次（那是 N+1）。
func (s *Server) ownersByAccount() (map[int64][]string, error) {
	if s.Store == nil {
		return map[int64][]string{}, nil
	}
	all, err := s.Store.ClusterOwners()
	if err != nil {
		return nil, err
	}
	out := map[int64][]string{}
	for name, o := range all {
		out[o.AccountID] = append(out[o.AccountID], name)
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out, nil
}
