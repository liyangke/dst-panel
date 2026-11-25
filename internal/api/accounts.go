package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"dstpanel/internal/auth"
	"dstpanel/internal/config"
	"dstpanel/internal/store"
)

// sessionView 是登录后界面用来决定"显示哪些按钮"的唯一依据。
// role 给机器看，roleLabel 给人看；界面不根据码值自己翻译。
type sessionView struct {
	Username  string `json:"username"`
	Role      string `json:"role"`
	RoleLabel string `json:"roleLabel"`
	// RoleHint 是"这个角色能做什么"那一句。界面在右上角角色徽标的提示里用它，
	// 而不是自己编一句：权限的说法改了，前端那句迟早和后端对不上。
	RoleHint string `json:"roleHint"`
	IsAdmin  bool   `json:"isAdmin"`
	// MyClusters 只对房间成员有意义：他名下的房间名单。
	// 界面拿它决定"这行是我能改的"，而不是把 /clusters 返回的数量当权限——
	// 那个列表里还有"未指派"的房间，看得见不等于改得动。
	MyClusters []string `json:"myClusters"`
	// CanAssignOwner 说清"这个账号能指派归属吗"。它等于 IsAdmin 但不是别名：
	// 界面上的「指派」按钮要的是这一句，将来若放开"房主可转派"，只改这里。
	CanAssignOwner bool `json:"canAssignOwner"`
	// UsingDefaultPassword 让外壳在任意页面都能挂出"还在用出厂默认密码"的提醒。
	// 面板默认直接开在公网端口上，而 admin/admin 是公开知识：光在 README 里写"请改密码"
	// 等于没写，必须在界面上一直看得见。
	UsingDefaultPassword bool `json:"usingDefaultPassword"`
	// Roles 是三档角色的完整说法（值 + 中文 + 一句"这个角色能做什么"）。
	// 账号页新建下拉框和改角色都读它，而不是在前端另抄一份：接口拒人那句
	// "角色只能是管理员、房间成员、只读"用的就是同一份顺序和说法（见 roleChoices），
	// 两边各抄一遍迟早会把权限目录说出两种版本。
	Roles []roleChoice `json:"roles"`
	// Engine 是这台机器上的服务端本体状态。
	//
	// 机器状态跟着账号状态一起从这个低频接口出去，是因为外壳每一页都要靠它挂提醒条，
	// 而本接口只在登录、点"立即同步"、下载任务结束时才取——它是唯一一个允许做磁盘 stat
	// 的入口：五秒轮询的那几组接口里既不该 stat 文件，更不该算 PBKDF2。
	Engine engineView `json:"engine"`
}

type accountView struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	RoleLabel   string `json:"roleLabel"`
	RoleHint    string `json:"roleHint"`
	CreatedAt   string `json:"createdAt"`
	LastLoginAt string `json:"lastLoginAt,omitempty"`
	Self        bool   `json:"self"`

	// OwnedClusters 是归属表里派给这个账号的房间目录名。
	// 空数组而不是 null：账号页那一行要数房间、也要列名单，
	// 前端对 null 做 .length 会当场报错。
	OwnedClusters []string `json:"ownedClusters"`
	// OwnedLabel 是那一列直接显示的话："名下 2 间" / "没有房间"。
	// 为什么由后端给而不是前端拼："间"这个量词、以及 0 间该怎么说，
	// 两边各写一份迟早会出现"名下 0 间"这种没人会说出口的话。
	OwnedLabel string `json:"ownedLabel"`

	UsingDefaultPassword bool `json:"usingDefaultPassword"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r)
	writeOK(w, sessionView{
		Username:             sess.Username,
		Role:                 sess.Role,
		RoleLabel:            store.LabelRole(sess.Role),
		RoleHint:             store.HintRole(sess.Role),
		IsAdmin:              sess.Role == store.RoleAdmin,
		MyClusters:           s.myClustersOf(sess),
		CanAssignOwner:       sess.Role == store.RoleAdmin,
		Roles:                roleChoices(),
		UsingDefaultPassword: s.usesDefaultPassword(sess.ID),
		Engine:               s.engineStatus(),
	})
}

// myClustersOf 给成员那一份"我名下的房间"名单。
//
// 非成员返回空数组而不是 nil：前端那一行是 v-for + .length，
// JSON 里的 null 会让它当场报错（这是接口层的边界，不是前端的锅）。
// 读库失败也不让 /session 变成 500：这个接口还带着引擎状态，
// 归属读不出来时界面按"一间都不在我的名单里"处理，改不动东西但还能看。
func (s *Server) myClustersOf(sess session) []string {
	out := []string{}
	if s.Store == nil || sess.Role != store.RoleMember {
		return out
	}
	names, err := s.Store.ClustersOfAccount(sess.ID)
	if err != nil {
		s.log().Error("读我名下的房间失败", "user", sess.Username, "err", err.Error())
		return out
	}
	if names != nil {
		out = names
	}
	return out
}

// usesDefaultPassword 核对某个账号的密码是不是还等于出厂默认值。
//
// 只能靠哈希真算一遍 PBKDF2 来判断（明文没入库，盐还是随机的），所以只挂在 /session
// 和账号列表这两个低频入口上——界面每五秒那组轮询不会走到这里。
// 读不到哈希就回 false：这是提醒，不是权限判断，不该让一次查询失败把页面变成 500。
func (s *Server) usesDefaultPassword(id int64) bool {
	if s.Store == nil || config.DefaultAdminPassword == "" {
		return false
	}
	hash, err := s.Store.PasswordHashByID(id)
	if err != nil {
		return false
	}
	return auth.Verify(hash, config.DefaultAdminPassword)
}

// accountViewOf 拼账号那一行。owners 是整张归属表按账号聚合的结果，
// 由调用方取一次传进来：账号列表是十几行，逐行查归属就成了 N+1。
func (s *Server) accountViewOf(a store.Account, self string, owners map[int64][]string) accountView {
	list := owners[a.ID]
	if list == nil {
		list = []string{}
	}
	v := accountView{
		ID:                   a.ID,
		Username:             a.Username,
		Role:                 a.Role,
		RoleLabel:            store.LabelRole(a.Role),
		RoleHint:             store.HintRole(a.Role),
		CreatedAt:            a.CreatedAt.UTC().Format(time.RFC3339),
		Self:                 strings.EqualFold(a.Username, self),
		OwnedClusters:        list,
		OwnedLabel:           ownedLabel(len(list)),
		UsingDefaultPassword: s.usesDefaultPassword(a.ID),
	}
	if !a.LastLoginAt.IsZero() {
		v.LastLoginAt = a.LastLoginAt.UTC().Format(time.RFC3339)
	}
	return v
}

func ownedLabel(n int) string {
	if n == 0 {
		return "没有房间"
	}
	return fmt.Sprintf("名下 %d 间", n)
}

// roleChoiceNote 是"角色取值不对"那句里列举角色的部分。
// 名单来自 store 的同一张表，界面上的下拉和这里的报错不会说成两套。
func roleChoiceNote() string {
	return "角色只能是" + strings.Join(store.RoleLabels(), "、")
}

// roleChoice 是角色下拉里的一项：能提交的值、中文说法、以及"这个角色能做什么"。
type roleChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Hint  string `json:"hint"`
}

// roleChoices 按 store.RoleOrder 给出全部角色。顺序也只有一份：
// 界面上管理员排第一、只读排最后，报错那句列的也是同一个次序。
func roleChoices() []roleChoice {
	out := make([]roleChoice, 0, len(store.RoleOrder))
	for _, role := range store.RoleOrder {
		out = append(out, roleChoice{Value: role, Label: store.LabelRole(role), Hint: store.HintRole(role)})
	}
	return out
}

func (s *Server) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	if sessionOf(r).Role != store.RoleAdmin {
		writeErr(w, http.StatusForbidden, "只有管理员能查看账号列表")
		return
	}
	list, err := s.Store.ListAccounts()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	owners, err := s.ownersByAccount()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	self := sessionOf(r).Username
	out := make([]accountView, 0, len(list))
	for _, a := range list {
		out = append(out, s.accountViewOf(a, self, owners))
	}
	writeOK(w, out)
}

// validUsername 只允许中英文、数字和 _-.，2~32 个字符。
// 限制符号不是为了好看：用户名会进 HTTP Basic 头和审计记录，
// 空格、冒号、控制字符在里面会出现"到底是谁"说不清的行。
func validUsername(name string) error {
	n := len([]rune(name))
	if n < 2 || n > 32 {
		return errors.New("用户名要 2~32 个字符")
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			continue
		}
		return errors.New("用户名只能用中英文、数字，以及下划线、短横线、点")
	}
	return nil
}

func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Username)
	if err := validUsername(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	role := req.Role
	if role == "" {
		role = store.RoleViewer
	}
	if !store.ValidRole(role) {
		writeErr(w, http.StatusBadRequest, roleChoiceNote())
		return
	}
	hash, err := auth.Hash(req.Password)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	acct, err := s.Store.CreateAccount(name, hash, role)
	if errors.Is(err, store.ErrUsernameTaken) {
		writeErr(w, http.StatusConflict, "这个用户名已经被用了，换一个；大小写算同一个")
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "account.create", acct.Username, "角色："+store.LabelRole(acct.Role), nil)
	// 归属传 nil：这个账号刚建出来，不可能名下有房间，没必要为此再查一次表。
	// accountViewOf 对 nil map 取键返回空，那一行显示的就是"没有房间"。
	writeJSON(w, http.StatusCreated, reply{OK: true, Data: s.accountViewOf(acct, sessionOf(r).Username, nil)})
}

func (s *Server) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := s.accountID(w, r)
	if !ok {
		return
	}
	if id == sessionOf(r).ID {
		writeErr(w, http.StatusBadRequest, "改自己的密码请用页面上方的「修改我的密码」，要先验证旧密码")
		return
	}
	acct, err := s.Store.AccountByID(id)
	if errors.Is(err, store.ErrAccountNotFound) {
		writeErr(w, http.StatusNotFound, "这个账号已经不存在了，刷新一下列表")
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.Hash(req.Password)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := s.Store.UpdateAccountPassword(id, hash); err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "account.password", acct.Username, "管理员重置密码", nil)
	writeOK(w, map[string]any{"username": acct.Username})
}

// handleAccountRole 调权限。最后一名管理员既不能降级也不能删除，
// 否则库里会出现"没有任何人能改设置"的死局，只能去改数据库文件。
func (s *Server) handleAccountRole(w http.ResponseWriter, r *http.Request) {
	id, ok := s.accountID(w, r)
	if !ok {
		return
	}
	acct, err := s.Store.AccountByID(id)
	if errors.Is(err, store.ErrAccountNotFound) {
		writeErr(w, http.StatusNotFound, "这个账号已经不存在了，刷新一下列表")
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if !store.ValidRole(req.Role) {
		writeErr(w, http.StatusBadRequest, roleChoiceNote())
		return
	}
	if acct.Role == store.RoleAdmin && req.Role != store.RoleAdmin {
		n, err := s.Store.CountAdmins()
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		if n <= 1 {
			writeErr(w, http.StatusConflict, "至少要留一个管理员；先把别的账号升成管理员，再降这个")
			return
		}
	}
	// 改成「只读」之前先清空名下房间。只读账号改不了任何东西，而归属表里那一行还在：
	// 界面上这几间会显示"归他管"，实际上除了管理员谁也动不了，下一个接手的人看不出该找谁。
	// 与其留这种状态，不如当场说清楚要先把指派撤掉。
	if req.Role == store.RoleViewer && acct.Role != store.RoleViewer {
		names, err := s.Store.ClustersOfAccount(id)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		if len(names) > 0 {
			writeErr(w, http.StatusConflict, fmt.Sprintf(
				"%s 名下还有 %d 间房（%s），先把归属撤掉或派给别人，再改成只读",
				acct.Username, len(names), strings.Join(names, "、")))
			return
		}
	}
	if err := s.Store.UpdateAccountRole(id, req.Role); err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "account.role", acct.Username,
		"从"+store.LabelRole(acct.Role)+"改为"+store.LabelRole(req.Role), nil)
	writeOK(w, map[string]any{"username": acct.Username, "roleLabel": store.LabelRole(req.Role)})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.accountID(w, r)
	if !ok {
		return
	}
	if id == sessionOf(r).ID {
		writeErr(w, http.StatusBadRequest, "不能删掉当前正在登录的账号；要退出请用右上角的「退出」")
		return
	}
	acct, err := s.Store.AccountByID(id)
	if errors.Is(err, store.ErrAccountNotFound) {
		writeErr(w, http.StatusNotFound, "这个账号已经不存在了，刷新一下列表")
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if acct.Role == store.RoleAdmin {
		n, err := s.Store.CountAdmins()
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		if n <= 1 {
			writeErr(w, http.StatusConflict, "至少要留一个管理员，这个删了就没人能改设置了")
			return
		}
	}
	// 归属行会跟着账号一起在事务里删掉，那几间变成"未指派"。
	// 这件事必须留在操作记录里：删完之后归属表里查不到痕迹，
	// 只留下"某间房没有负责人"，而用户想知道的是它什么时候没有的。
	names, err := s.Store.ClustersOfAccount(id)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := s.Store.DeleteAccount(id); err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	detail := "角色：" + store.LabelRole(acct.Role)
	if len(names) > 0 {
		detail += fmt.Sprintf("；名下 %d 间房回到未指派（%s）", len(names), strings.Join(names, "、"))
	}
	s.audit(r, "account.delete", acct.Username, detail, nil)
	writeOK(w, map[string]any{"username": acct.Username})
}

// handleSelfPassword 让任何角色都能改自己的密码，但必须验证旧密码。
// 改完之后当前页面的 Basic 凭据就作废了，界面会主动退回登录层。
func (s *Server) handleSelfPassword(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r)
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "这个面板没有接账号库，改不了密码")
		return
	}
	acct, hash, err := s.Store.AccountByUsername(sess.Username)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if !auth.Verify(hash, req.OldPassword) {
		writeErr(w, http.StatusBadRequest, "旧密码不对，没有改")
		return
	}
	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.NewPassword == req.OldPassword {
		writeErr(w, http.StatusBadRequest, "新密码和旧密码一样，没有改")
		return
	}
	newHash, err := auth.Hash(req.NewPassword)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := s.Store.UpdateAccountPassword(acct.ID, newHash); err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.audit(r, "account.password", acct.Username, "本人修改密码", nil)
	writeOK(w, map[string]any{"username": acct.Username})
}

func (s *Server) accountID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("账号 id 得是正整数，收到的是 %q", raw))
		return 0, false
	}
	return id, true
}
