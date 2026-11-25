package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"dstpanel/internal/store"
)

// 房间归属的接口层：指派、取消、以及把归属说法发到界面。
//
// 为什么用账号 id 而不是用户名当参数：界面上那个下拉框的每一项本来就带着 id
// （账号列表给的），而用户名是可以改的、也允许中英文带点，拿它当主键拼路径
// 会多出"改名之后归属跟着丢"这种说不清的状态。id 只出现在请求体里，
// 用户看到的始终是名字。

type clusterOwnerRequest struct {
	// AccountID 为 0 表示取消指派，房间回到"未指派"（谁都能看、只有管理员能改）。
	AccountID int64 `json:"accountId"`
}

type clusterOwnerView struct {
	Cluster    string `json:"cluster"`
	OwnerLabel string `json:"ownerLabel"`
	// AccountID 回给界面是为了让下拉框把当前项选中；0 就是"未指派"。
	AccountID   int64  `json:"accountId"`
	AccountName string `json:"ownerName"`
	AssignedBy  string `json:"assignedBy"`
	// AssignedNote 是"由谁指派"那一句人话，界面上的悬浮说明用它。
	AssignedNote string `json:"assignedNote"`
	// AssignedAt 单独给一个 RFC3339 的时刻，不拼进上面那句话：面板上别的时间都是
	// 前端按浏览器所在时区格式化过的（运维要知道的是自己表上的时间，见 fmt.js 的 clock）。
	// 这一句如果带着后端算好的 UTC 串，同一屏里就会出现两个相差几个小时的钟点，
	// 而用户没有任何线索能发现哪个才对。
	AssignedAt string `json:"assignedAt"`
}

func (s *Server) handleClusterOwnerPut(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("name")
	// 房间必须真在磁盘上。指派对一个不存在的目录，界面上会留下一行
	// "某间房归谁管"，而那间房根本打不开——归属表就成了假数据的来源。
	if _, ok := s.knownCluster(w, r, cluster); !ok {
		return
	}
	var req clusterOwnerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.AccountID < 0 {
		writeErr(w, http.StatusBadRequest, "账号 id 不可能是负数")
		return
	}
	view := clusterOwnerView{Cluster: cluster}
	if req.AccountID == 0 {
		if err := s.Store.SetClusterOwner(cluster, 0, sessionOf(r).Username); err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		view.OwnerLabel = ownerLabel(nil)
		s.audit(r, "cluster.owner", cluster, "取消指派（房间回到未指派：成员只能看不能改）", nil)
		writeOK(w, view)
		return
	}
	acct, err := s.Store.AccountByID(req.AccountID)
	if errors.Is(err, store.ErrAccountNotFound) {
		writeErr(w, http.StatusNotFound, "这个账号已经不存在了，刷新一下账号名单再指派")
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 只读账号管不了房间（写请求在 authenticate 就被拒），把房间派给它只会让
	// 那个账号看见一间改不动的房子。这里拦下来并说清该怎么改，
	// 比让管理员指派完再去账号页找原因省一轮。
	if acct.Role == store.RoleViewer {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"%s 现在是只读账号，派房间给它没有意义：先在账号页把它改成「%s」，再回来指派",
			acct.Username, store.LabelRole(store.RoleMember)))
		return
	}
	// 管理员不当房间负责人。「未指派」在这套规则里说的就是"管理员自己管"，
	// 派给管理员没有任何新增权限，反而有副作用：成员的可见名单是按"我自己名下的 + 未指派的"筛的，
	// 一旦某间挂到管理员名下，它就从所有成员的列表里消失了，
	// 而管理员在界面上看不出自己按下了什么。
	if acct.Role == store.RoleAdmin {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"%s 是管理员，管理员负责所有房间，不需要单独指派。%s 保持在「%s」就是你在管它",
			acct.Username, cluster, ownerLabel(nil)))
		return
	}
	if err := s.Store.SetClusterOwner(cluster, acct.ID, sessionOf(r).Username); err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view.AccountID = acct.ID
	view.AccountName = acct.Username
	view.AssignedBy = sessionOf(r).Username
	view.OwnerLabel = ownerLabel(&store.ClusterOwner{AccountName: acct.Username})
	view.AssignedNote = fmt.Sprintf("由 %s 指派给 %s", view.AssignedBy, acct.Username)
	view.AssignedAt = time.Now().UTC().Format(time.RFC3339)
	s.audit(r, "cluster.owner", cluster,
		fmt.Sprintf("指派给 %s（%s）", acct.Username, store.LabelRole(acct.Role)), nil)
	writeOK(w, view)
}

// ownerViews 给房间类视图批量附上归属说法。
//
// 为什么在接口层拼而不是前端拿 accountId 去账号列表里找名字：
// 成员账号打不开账号页（那是管理员的），前端找不到名字就会显示成
// "归属："后面什么都没有——用户以为没数据，实际是权限边界。
func (s *Server) ownerViews(names []string) (map[string]clusterOwnerView, error) {
	owners, err := s.roomOwners()
	if err != nil {
		return nil, err
	}
	out := make(map[string]clusterOwnerView, len(names))
	for _, n := range names {
		view := clusterOwnerView{Cluster: n}
		if o, ok := owners[normalizeClusterName(n)]; ok {
			view = clusterOwnerView{
				Cluster:     n,
				OwnerLabel:  ownerLabel(&o),
				AccountID:   o.AccountID,
				AccountName: o.AccountName,
				AssignedBy:  o.AssignedBy,
			}
			// 指派人是空的情况只有一种：这一行不是面板写进去的（有人手工动过库）。
			// 那就老实说没有操作者，而不是编一个"系统"出来。
			who := "这条指派记录里没有操作者"
			if strings.TrimSpace(o.AssignedBy) != "" {
				who = "由 " + o.AssignedBy + " 指派"
			}
			view.AssignedNote = who
			// 年份为 0 的时刻（那一行没有毫秒值）不发给前端：前端 dateTime 会老实把
			// 它换算成"0001-01-01 …"，看着像数据坏了。留空，界面就只说那一句人话。
			if !o.AssignedAt.IsZero() {
				view.AssignedAt = o.AssignedAt.UTC().Format(time.RFC3339)
			}
		} else {
			view.OwnerLabel = ownerLabel(nil)
		}
		out[normalizeClusterName(n)] = view
	}
	return out, nil
}

// ownerFromViews 在已经取好的归属视图里认一间房，省掉第二次查库。
func ownerFromViews(views map[string]clusterOwnerView, cluster string) (clusterOwnerView, bool) {
	v, ok := views[normalizeClusterName(cluster)]
	return v, ok
}
