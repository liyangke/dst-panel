// 房间归属这一层的用例：谁能看见哪间房、哪间房改得动、指派本身归谁。
//
// 这组测试全部从 http 入口走，不直接调 guardRooms：漏鉴权最难发现的形式恰好是
// "处理器单独测是通的，但那条路由没挂上判断"。只有发请求才量得出真实覆盖面。
// 每一条正向断言都顺手看盘上的文件（改 settings 那条），免得权限放对了、写盘写歪了。
package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dstpanel/internal/store"
)

// addRoom 在夹具的 klei 根目录下多造一间房。
// 和 newFixture 里那间一样：只放 cluster.ini，因为 ClusterNames 就认这个文件。
func (f *apiFixture) addRoom(t *testing.T, name string) {
	t.Helper()
	for _, shard := range []string{"Master", "Caves"} {
		if err := os.MkdirAll(filepath.Join(f.paths.KleiRoot, name, shard), 0o755); err != nil {
			t.Fatalf("建 %s/%s 目录: %v", name, shard, err)
		}
	}
	if err := os.WriteFile(f.paths.ClusterIni(name), []byte("[GAME]\n"), 0o644); err != nil {
		t.Fatalf("写 %s 的 cluster.ini: %v", name, err)
	}
}

// newAccount 建一个指定角色的账号并把 id 返回出来，指派要用。
func newAccount(t *testing.T, h http.Handler, username, password, role string) accountView {
	t.Helper()
	rec := doJSONAuth(t, h, http.MethodPost, "/api/v1/accounts", "admin", "secret",
		`{"username":"`+username+`","password":"`+password+`","role":"`+role+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("建账号 %s = %d，body %s", username, rec.Code, rec.Body.String())
	}
	var v accountView
	decodeInto(t, rec, &v)
	if v.Username != username || v.Role != role {
		t.Fatalf("建回来的账号不对：%+v", v)
	}
	return v
}

// assign 以管理员身份做一次指派/取消，把响应原样带出去给调用方断言。
func assign(t *testing.T, h http.Handler, cluster string, accountID int64) *httptest.ResponseRecorder {
	t.Helper()
	return doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/"+cluster+"/owner", "admin", "secret",
		`{"accountId":`+itoa(accountID)+`}`)
}

func TestMemberSeesOnlyOwnAndUnownedRooms(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	f.addRoom(t, "Cluster_2")
	f.addRoom(t, "Cluster_3")

	alice := newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	bob := newAccount(t, h, "bob", "bob-pass-1", store.RoleMember)
	// 默认只有一名管理员时不能把管理员降级，所以这里不派给管理员：
	// Cluster_1 归 alice、Cluster_2 归 bob、Cluster_3 留未指派。
	if rec := assign(t, h, "Cluster_1", alice.ID); rec.Code != http.StatusOK {
		t.Fatalf("指派 Cluster_1 = %d，body %s", rec.Code, rec.Body.String())
	}
	if rec := assign(t, h, "Cluster_2", bob.ID); rec.Code != http.StatusOK {
		t.Fatalf("指派 Cluster_2 = %d，body %s", rec.Code, rec.Body.String())
	}

	var rooms []clusterView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/clusters", "alice", "alice-pass-1"), &rooms)
	got := map[string]clusterView{}
	for _, r := range rooms {
		got[r.Name] = r
	}
	if _, ok := got["Cluster_1"]; !ok {
		t.Error("alice 看不到自己名下的 Cluster_1")
	}
	if _, ok := got["Cluster_3"]; !ok {
		t.Error("alice 看不到未指派的 Cluster_3：房间是磁盘事实，藏起来会让他以为这台机器上只有两间")
	}
	if _, ok := got["Cluster_2"]; ok {
		t.Errorf("alice 竟然看得到 bob 的 Cluster_2：%+v", rooms)
	}
	if !got["Cluster_1"].OwnedByMe {
		t.Error("Cluster_1 没标成我自己的")
	}
	if got["Cluster_1"].OwnerLabel != "归 alice 管" {
		t.Errorf("Cluster_1 的归属说法 = %q，期望带人名", got["Cluster_1"].OwnerLabel)
	}
	if got["Cluster_3"].OwnerLabel != "未指派" {
		t.Errorf("Cluster_3 的归属说法 = %q", got["Cluster_3"].OwnerLabel)
	}
	if got["Cluster_3"].OwnedByMe {
		t.Error("未指派不该算成 alice 名下的房间")
	}

	// 房间总览页走的是另一条路由（/rooms），可见性必须一致：
	// 只在 /clusters 过滤的话，成员换个页面还是把别人的房间列出来了。
	var page roomsReply
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/rooms", "alice", "alice-pass-1"), &page)
	if len(page.Items) == 0 {
		t.Fatal("/rooms 一行都没有")
	}
	// 先自证这一页本来是"三间房"的形状：Cluster_2 在这页上必须真的出现过，
	// 否则 alice 看不到它这条断言就是假的（管理员视角读一次）。
	var adminPage roomsReply
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/rooms", "admin", "secret"), &adminPage)
	var sawOthers bool
	for _, r := range adminPage.Items {
		if r.Cluster == "Cluster_2" {
			sawOthers = true
		}
	}
	if !sawOthers {
		t.Fatalf("管理员视角的 /rooms 里没有 Cluster_2，后面的断言不成立：%+v", adminPage.Items)
	}
	for _, r := range page.Items {
		if r.Cluster == "Cluster_2" {
			t.Errorf("/rooms 里出现了 bob 的房间：%+v", page.Items)
		}
	}
	labels := map[string]string{}
	for _, r := range page.Items {
		labels[r.Cluster] = r.OwnerLabel
	}
	if labels["Cluster_1"] != "归 alice 管" || labels["Cluster_3"] != "未指派" {
		t.Errorf("/rooms 的归属列 = %+v", labels)
	}

	// 「指派归属」弹窗要三样东西：当前派给谁（人名摆出来）、下拉框选中哪一项（id）、
	// 以及那句"由谁、什么时候派的"。这些只能由 /clusters 带出来——弹窗是管理员打开的，
	// 而管理员拿不到"某一间的归属"单独的读接口。
	var adminRooms []clusterView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/clusters", "admin", "secret"), &adminRooms)
	byName := map[string]clusterView{}
	for _, r := range adminRooms {
		byName[r.Name] = r
	}
	c2 := byName["Cluster_2"]
	if c2.OwnerName != "bob" || c2.OwnerAccountID != bob.ID {
		t.Errorf("Cluster_2 的归属明细 = %+v %d，弹窗选不中当前项", c2.OwnerName, c2.OwnerAccountID)
	}
	if !strings.Contains(c2.AssignedNote, "由 admin 指派") {
		t.Errorf("Cluster_2 的指派说明 = %q", c2.AssignedNote)
	}
	if c3 := byName["Cluster_3"]; c3.OwnerAccountID != 0 || c3.AssignedNote != "" {
		t.Errorf("未指派那间应该什么都没有，实际 = %+v", c3)
	}

	// 按名字直接问也要答不出来：只在列表里过滤等于没过滤。
	rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_2/settings", "alice", "alice-pass-1")
	if rec.Code != http.StatusForbidden {
		t.Errorf("读别人的房间 = %d，期望 403，body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "bob") || strings.Contains(body, "200") {
		t.Errorf("拒语没带上归谁管：%s", body)
	}

	// ?cluster= 这种把房间写在参数里的接口同样要挡（趋势图与存档页都这样传）。
	if rec := do(t, h, http.MethodGet, "/api/v1/stats?cluster=Cluster_2", "alice", "alice-pass-1"); rec.Code != http.StatusForbidden {
		t.Errorf("带参数的接口 = %d，期望 403，body %s", rec.Code, rec.Body.String())
	}

	// 事件表这一支最容易被漏：它不经数据库，直接读磁盘上那两份日志，
	// 而且比日志页多给了 IP、SteamID 和账户编号。
	// 路径解析一旦退化，别人房间里玩家的这些身份线索就整个给出去了。
	if rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_2/shards/Master/events", "alice", "alice-pass-1"); rec.Code != http.StatusForbidden {
		t.Errorf("读别人房间的玩家事件表 = %d，期望 403，body %s", rec.Code, rec.Body.String())
	}
	// 反过来：自己名下那间不能顺手也被挡。夹具里没有日志文件，所以这里只能断言
	// "不是权限层拦的"（真回话是 503 那句"还没有日志文件"）。
	if rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/shards/Master/events", "alice", "alice-pass-1"); rec.Code == http.StatusForbidden {
		t.Errorf("成员读自己房间的事件表被权限层挡了：%s", rec.Body.String())
	}
}

func TestMemberWritesWithinAndOutsideOwnRoom(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	f.addRoom(t, "Cluster_2")

	alice := newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	rec := assign(t, h, "Cluster_1", alice.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("指派 = %d，body %s", rec.Code, rec.Body.String())
	}

	// 自己名下的房间：和管理员一样能改，并且真的落到 cluster.ini 上。
	r2 := doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/settings", "alice", "alice-pass-1",
		`{"displayName":"alice 的房子","maxPlayers":6}`)
	if r2.Code != http.StatusOK {
		t.Fatalf("成员改自己房间 = %d，body %s", r2.Code, r2.Body.String())
	}
	ini, err := os.ReadFile(f.paths.ClusterIni("Cluster_1"))
	if err != nil {
		t.Fatalf("读 cluster.ini: %v", err)
	}
	if !strings.Contains(string(ini), "alice 的房子") || !strings.Contains(string(ini), "max_players = 6") {
		t.Errorf("权限放对了却没写真值:\n%s", ini)
	}

	// 别人的房间：读得到（未指派之外的情形是"归 bob"），这里 Cluster_2 还没指派，
	// 所以成员能看见但改不动——"看得见但只读"是这套规则里唯一合法的中间态。
	r3 := doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_2/settings", "alice", "alice-pass-1",
		`{"maxPlayers":3}`)
	if r3.Code != http.StatusForbidden {
		t.Errorf("成员改未指派房间 = %d，期望 403，body %s", r3.Code, r3.Body.String())
	}
	if body := r3.Body.String(); !strings.Contains(body, "还没指派") || !strings.Contains(body, "Cluster_2") {
		t.Errorf("未指派的拒语没告诉用户下一步做什么：%s", body)
	}
	if ini, err := os.ReadFile(f.paths.ClusterIni("Cluster_2")); err != nil {
		t.Fatalf("读 Cluster_2: %v", err)
	} else if strings.Contains(string(ini), "max_players") {
		t.Errorf("被拒的写竟然落盘了:\n%s", ini)
	}

	// 建房间、改面板设置这类机器级动作对成员一律关着。
	if r := doJSONAuth(t, h, http.MethodPost, "/api/v1/clusters", "alice", "alice-pass-1", `{}`); r.Code != http.StatusForbidden {
		t.Errorf("成员建房间 = %d，期望 403", r.Code)
	}
	if r := doJSONAuth(t, h, http.MethodPut, "/api/v1/settings", "alice", "alice-pass-1", `{}`); r.Code != http.StatusForbidden {
		t.Errorf("成员改面板设置 = %d，期望 403", r.Code)
	}
	// 唯一的例外：改自己的密码。拦掉它的话成员只能找人重置。
	if r := doJSONAuth(t, h, http.MethodPut, "/api/v1/session/password", "alice", "alice-pass-1",
		`{"oldPassword":"alice-pass-1","newPassword":"alice-pass-2"}`); r.Code != http.StatusOK {
		t.Errorf("成员改自己密码 = %d，期望 200，body %s", r.Code, r.Body.String())
	}
	// 指派本身也不在成员手里：把自己名下的房子转给别人，等于绕开管理员。
	if r := doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/owner", "alice", "alice-pass-2",
		`{"accountId":0}`); r.Code != http.StatusForbidden {
		t.Errorf("成员取消指派 = %d，期望 403，body %s", r.Code, r.Body.String())
	}
}

func TestOwnerAssignmentValidations(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	alice := newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	guest := newAccount(t, h, "guest", "guest-pass-1", store.RoleViewer)
	admin := listAccounts(t, h)[0]

	// 派给管理员：管理员负责所有房间，这一按反而会让房间从成员的名单里消失。
	r := doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/owner", "admin", "secret",
		`{"accountId":`+itoa(admin.ID)+`}`)
	if r.Code != http.StatusConflict {
		t.Errorf("派给管理员 = %d，期望 409，body %s", r.Code, r.Body.String())
	}
	if body := r.Body.String(); !strings.Contains(body, "未指派") {
		t.Errorf("这句该说明管理员不需要指派，实际 %s", body)
	}

	// 派给只读账号：它会看见一间自己改不动的房子。
	r = doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/owner", "admin", "secret",
		`{"accountId":`+itoa(guest.ID)+`}`)
	if r.Code != http.StatusConflict {
		t.Errorf("派给只读账号 = %d，期望 409，body %s", r.Code, r.Body.String())
	}
	if body := r.Body.String(); !strings.Contains(body, "房间成员") {
		t.Errorf("拒语没给出可操作的改法：%s", body)
	}

	// 账号不存在（比如另一个浏览器标签页里已经删掉了）。
	r = doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/owner", "admin", "secret", `{"accountId":99999}`)
	if r.Code != http.StatusNotFound {
		t.Errorf("指派给不存在的账号 = %d，期望 404", r.Code)
	}
	// 房间不存在：不能往归属表里写一行指向空目录的数据。
	r = doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Nope_9/owner", "admin", "secret",
		`{"accountId":`+itoa(alice.ID)+`}`)
	if r.Code != http.StatusNotFound {
		t.Errorf("指派不存在的房间 = %d，期望 404，body %s", r.Code, r.Body.String())
	}
	if _, err := f.srv.Store.ClusterOwners(); err != nil {
		t.Fatalf("读归属表: %v", err)
	}
	if owners, _ := f.srv.Store.ClusterOwners(); len(owners) != 0 {
		t.Errorf("被拒的指派留下了行：%+v", owners)
	}

	// 指派 → 回读 → 取消，界面上那一行要跟着换。
	r = doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/owner", "admin", "secret",
		`{"accountId":`+itoa(alice.ID)+`}`)
	if r.Code != http.StatusOK {
		t.Fatalf("指派给成员 = %d，body %s", r.Code, r.Body.String())
	}
	var v clusterOwnerView
	decodeInto(t, r, &v)
	if v.AccountID != alice.ID || v.AccountName != "alice" || v.OwnerLabel != "归 alice 管" {
		t.Errorf("指派回来的视图 = %+v", v)
	}
	if !strings.Contains(v.AssignedNote, "由 admin 指派") {
		t.Errorf("指派说明缺操作人：%q", v.AssignedNote)
	}
	// 时刻单独一个字段，那句人话里不许夹钟点：面板其余时间都由前端按浏览器时区格式化
	// （fmt.js），后端拼好的 UTC 钟点会在同一屏里摆出两个相差几小时的表。
	if strings.ContainsAny(v.AssignedNote, "0123456789") {
		t.Errorf("指派说明里夹了数字：%q", v.AssignedNote)
	}
	at, err := time.Parse(time.RFC3339, v.AssignedAt)
	if err != nil {
		t.Errorf("assignedAt 解不开（前端只吃 RFC3339）：%q", v.AssignedAt)
	} else if d := time.Since(at); d < 0 || d > time.Minute {
		t.Errorf("assignedAt 离现在太远：%q（差 %v）", v.AssignedAt, d)
	}

	r = doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/owner", "admin", "secret", `{"accountId":0}`)
	decodeInto(t, r, &v)
	if v.OwnerLabel != "未指派" || v.AccountID != 0 {
		t.Errorf("取消指派后的视图 = %+v", v)
	}
	// 取消之后那句和时刻都得清空：留着上一轮的 "由 admin 指派给 alice，2026-…"，
	// 界面就会指着"未指派"的房间说它有人指派过。
	if v.AssignedNote != "" || v.AssignedAt != "" {
		t.Errorf("取消指派后仍留着说明：%q / %q", v.AssignedNote, v.AssignedAt)
	}
	owners, err := f.srv.Store.ClusterOwners()
	if err != nil {
		t.Fatalf("读归属表: %v", err)
	}
	if len(owners) != 0 {
		t.Errorf("取消后归属表里还剩：%+v", owners)
	}

	entries, err := f.srv.Store.ListAudit(50, 0, "cluster.owner")
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if len(entries) < 2 {
		t.Errorf("指派与取消没都记进操作记录：%+v", entries)
	}
}

func TestRoleDowngradeAndDeleteAccountRespectOwnership(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	f.addRoom(t, "Cluster_2")

	alice := newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	if r := assign(t, h, "Cluster_1", alice.ID); r.Code != http.StatusOK {
		t.Fatalf("指派 = %d", r.Code)
	}
	if r := assign(t, h, "Cluster_2", alice.ID); r.Code != http.StatusOK {
		t.Fatalf("指派 Cluster_2 = %d", r.Code)
	}

	// 名下还有房间时不能改成只读：只读改不了任何东西，那两间会卡在"有人负责却没人能动"。
	r := doJSONAuth(t, h, http.MethodPut, "/api/v1/accounts/"+itoa(alice.ID)+"/role", "admin", "secret",
		`{"role":"viewer"}`)
	if r.Code != http.StatusConflict {
		t.Errorf("带房间降成只读 = %d，期望 409，body %s", r.Code, r.Body.String())
	}
	if body := r.Body.String(); !strings.Contains(body, "Cluster_1") || !strings.Contains(body, "Cluster_2") {
		t.Errorf("拒语没列出是哪几间：%s", body)
	}

	// 账号列表那一行要数得出房间，前端不该自己去翻归属表。
	var list []accountView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/accounts", "admin", "secret"), &list)
	for _, a := range list {
		if a.Username != "alice" {
			continue
		}
		if a.OwnedLabel != "名下 2 间" {
			t.Errorf("alice 的归属列 = %q", a.OwnedLabel)
		}
		if strings.Join(a.OwnedClusters, ",") != "Cluster_1,Cluster_2" {
			t.Errorf("alice 名下房间 = %+v", a.OwnedClusters)
		}
	}

	// 删账号连带清掉归属，并且把"回到未指派"留在操作记录里。
	r = doJSONAuth(t, h, http.MethodDelete, "/api/v1/accounts/"+itoa(alice.ID), "admin", "secret", "")
	if r.Code != http.StatusOK {
		t.Fatalf("删账号 = %d，body %s", r.Code, r.Body.String())
	}
	owners, err := f.srv.Store.ClusterOwners()
	if err != nil {
		t.Fatalf("读归属表: %v", err)
	}
	if len(owners) != 0 {
		t.Errorf("删账号后归属表里还剩：%+v", owners)
	}
	var rooms []clusterView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/clusters", "admin", "secret"), &rooms)
	for _, rm := range rooms {
		if rm.OwnerLabel != "未指派" {
			t.Errorf("%s 在 alice 被删后还显示 %q", rm.Name, rm.OwnerLabel)
		}
	}
	entries, err := f.srv.Store.ListAudit(50, 0, "account.delete")
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	if len(entries) == 0 || !strings.Contains(entries[0].Detail, "未指派") {
		t.Errorf("删账号的记录没说明它名下房间的下落：%+v", entries)
	}

	// 清空归属之后，降成只读就该放行（同一个请求，只差那两间房）。
	bob := newAccount(t, h, "bob", "bob-pass-1", store.RoleMember)
	if r := doJSONAuth(t, h, http.MethodPut, "/api/v1/accounts/"+itoa(bob.ID)+"/role", "admin", "secret",
		`{"role":"viewer"}`); r.Code != http.StatusOK {
		t.Errorf("没房间的成员降只读 = %d，期望 200，body %s", r.Code, r.Body.String())
	}
}

func TestViewerStillSeesEverythingButChangesNothing(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	f.addRoom(t, "Cluster_2")
	alice := newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	if r := assign(t, h, "Cluster_1", alice.ID); r.Code != http.StatusOK {
		t.Fatalf("指派 = %d", r.Code)
	}
	newAccount(t, h, "guest", "guest-pass-1", store.RoleViewer)

	// 只读账号是"这台机器的旁观者"，不是"某间房的旁观者"：所有房间都要能看，
	// 包括别人名下的那些，否则他连"出了什么事"都说不清。
	var rooms []clusterView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/clusters", "guest", "guest-pass-1"), &rooms)
	if len(rooms) != 2 {
		t.Errorf("只读账号看到的房间 = %+v，期望两间都在", rooms)
	}
	for _, rm := range rooms {
		if rm.OwnedByMe {
			t.Errorf("%s 不该算成只读账号名下的房间", rm.Name)
		}
	}
	if r := doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/settings", "guest", "guest-pass-1",
		`{"maxPlayers":4}`); r.Code != http.StatusForbidden {
		t.Errorf("只读改自己看不见的房间 = %d，期望 403", r.Code)
	}
	// /session 里那句角色说明必须由后端给：前端自己翻译码值迟早和这里对不上。
	var sess sessionView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/session", "guest", "guest-pass-1"), &sess)
	if sess.RoleHint == "" || sess.CanAssignOwner || len(sess.MyClusters) != 0 {
		t.Errorf("只读账号的 session = %+v", sess)
	}
}

func TestMemberSessionAndTaskAndAuditScopes(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	f.addRoom(t, "Cluster_2")
	alice := newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	if r := assign(t, h, "Cluster_1", alice.ID); r.Code != http.StatusOK {
		t.Fatalf("指派 = %d", r.Code)
	}

	var sess sessionView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/session", "alice", "alice-pass-1"), &sess)
	if strings.Join(sess.MyClusters, ",") != "Cluster_1" {
		t.Errorf("成员 session 的名单 = %+v，期望只有 Cluster_1", sess.MyClusters)
	}
	if sess.CanAssignOwner || sess.IsAdmin {
		t.Errorf("成员被判成能指派归属了：%+v", sess)
	}
	if sess.RoleHint == "" || !strings.Contains(sess.RoleHint, "指派") {
		t.Errorf("成员的角色说明没讲清边界：%q", sess.RoleHint)
	}

	// 任务列表：只带房间的任务按房间筛，机器级任务（更新本体、清理备份）不显示。
	if _, err := f.srv.Store.CreateTask("backup_run", "备份 Cluster_1", map[string]any{"cluster": "Cluster_1"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := f.srv.Store.CreateTask("backup_run", "备份 Cluster_2", map[string]any{"cluster": "Cluster_2"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := f.srv.Store.CreateTask("engine_update", "更新服务端本体", map[string]any{"platform": "linux32"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	var mine []taskView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/tasks", "alice", "alice-pass-1"), &mine)
	label := map[string]bool{}
	for _, tsk := range mine {
		label[tsk.Title] = true
	}
	if !label["备份 Cluster_1"] {
		t.Errorf("自己房间的任务没出现：%+v", mine)
	}
	if label["备份 Cluster_2"] || label["更新服务端本体"] {
		t.Errorf("别人的房间任务或机器级任务出现在了成员的任务中心：%+v", mine)
	}
	var all []taskView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/tasks", "admin", "secret"), &all)
	if len(all) != 3 {
		t.Errorf("管理员看到的任务数 = %d，期望 3：%+v", len(all), all)
	}

	// 操作记录：成员只能看自己的，把 actor 写成别人的名字要当场拒。
	// 这里先用 alice 做一次写（她名下的房间），制造一条她的记录。
	if r := doJSONAuth(t, h, http.MethodPut, "/api/v1/clusters/Cluster_1/settings", "alice", "alice-pass-1",
		`{"description":"alice 记一条"}`); r.Code != http.StatusOK {
		t.Fatalf("成员写自己房间 = %d，body %s", r.Code, r.Body.String())
	}
	var own []auditView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/audit", "alice", "alice-pass-1"), &own)
	if len(own) == 0 {
		t.Fatal("成员的操作记录是空的，那这一页对他就没有意义")
	}
	for _, e := range own {
		if !strings.EqualFold(e.Actor, "alice") {
			t.Errorf("成员的操作记录里出现了别人的操作：%+v", e)
		}
	}
	r := do(t, h, http.MethodGet, "/api/v1/audit?actor=admin", "alice", "alice-pass-1")
	if r.Code != http.StatusForbidden {
		t.Errorf("成员指定别人的操作人 = %d，期望 403，body %s", r.Code, r.Body.String())
	}
	var adminSeen []auditView
	decodeInto(t, do(t, h, http.MethodGet, "/api/v1/audit", "admin", "secret"), &adminSeen)
	if len(adminSeen) <= len(own) {
		t.Errorf("管理员看到的记录不该比成员还少：%d vs %d", len(adminSeen), len(own))
	}
}

func TestGuardIsOneMiddlewareNotPerHandler(t *testing.T) {
	// 这条专门盯"新加的接口忘了判"：拿一条成员一定无权的路径，逐条走一遍代表性前缀。
	// 只要中间件挂在 mux 上，任何 /api/v1/clusters/{name}/... 与机器级写都会被兜住。
	f := newFixture(t, "")
	h := f.srv.Handler()
	// alice 一间房都没派，Cluster_1 派给 bob：她对这间的每一次读写都该被兜住，
	// 而"被兜住"的样子必须是 403，不是处理器的 404 或 500。
	bob := newAccount(t, h, "bob", "bob-pass-1", store.RoleMember)
	newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	if r := assign(t, h, "Cluster_1", bob.ID); r.Code != http.StatusOK {
		t.Fatalf("指派 = %d，body %s", r.Code, r.Body.String())
	}

	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/clusters/Cluster_1/backups", `{}`},
		{http.MethodPost, "/api/v1/clusters/Cluster_1/shards/Master/start", `{}`},
		{http.MethodPut, "/api/v1/clusters/Cluster_1/token", `{"token":"KU-test"}`},
		{http.MethodPost, "/api/v1/clusters/Cluster_1/shards/Master/admin-commands", `{"command":"c_save()"}`},
		{http.MethodPut, "/api/v1/clusters/Cluster_1/ports", `{"masterPort":10998}`},
		{http.MethodDelete, "/api/v1/clusters/Cluster_1/token", ``},
		{http.MethodPost, "/api/v1/mods/upload", `{}`},
		{http.MethodPost, "/api/v1/tasks", `{"kind":"backup_run"}`},
		{http.MethodPut, "/api/v1/settings", `{}`},
	}
	for _, c := range cases {
		rec := doJSONAuth(t, h, c.method, c.path, "alice", "alice-pass-1", c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d，期望 403，body %s", c.method, c.path, rec.Code, rec.Body.String())
		}
		text := rec.Body.String()
		if strings.Contains(text, "permission denied") || strings.Contains(text, "not implemented") {
			t.Errorf("拒语里漏出英文原文：%s", text)
		}
		if strings.Contains(text, "Cluster_1") && !strings.Contains(text, "bob") {
			t.Errorf("拒语报了房间名却没报归谁管：%s", text)
		}
	}

	// 读别人的房间同样不行（这条不是写请求，authenticate 管不到它）。
	if rec := do(t, h, http.MethodGet, "/api/v1/clusters/Cluster_1/settings", "alice", "alice-pass-1"); rec.Code != http.StatusForbidden {
		t.Errorf("成员读别人的房间 = %d，期望 403，body %s", rec.Code, rec.Body.String())
	}

	// 路径里的百分号编码不能绕过判断：房间名是从 URL 上解出来的。
	// 不解的话 "%43luster_1" 在归属表里查不到、被当成"未指派"放行读，
	// 而处理器那边解出的是 Cluster_1 —— 归 bob 的房子就这样被读走了。
	for _, p := range []string{"/api/v1/clusters/%43luster_1/settings", "/api/v1/clusters/Cluster_1/%73ettings"} {
		rec := do(t, h, http.MethodGet, p, "alice", "alice-pass-1")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s 绕过了归属判断 = %d，body %s", p, rec.Code, rec.Body.String())
		}
	}

	// 取消任务这条： bob 名下的房间排着任务，alice 既看不到也不该能撤。
	if _, err := f.srv.Store.CreateTask("backup_run", "备份 Cluster_1", map[string]any{"cluster": "Cluster_1"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	rec := do(t, h, http.MethodDelete, "/api/v1/tasks/1", "alice", "alice-pass-1")
	if rec.Code != http.StatusForbidden {
		t.Errorf("alice 取消别人房间的任务 = %d，期望 403，body %s", rec.Code, rec.Body.String())
	}
	// bob 自己取消则是允许的（是否"还在排队"由处理器判，这里只验权限层放行）。
	if rec := do(t, h, http.MethodDelete, "/api/v1/tasks/1", "bob", "bob-pass-1"); rec.Code == http.StatusForbidden {
		t.Errorf("bob 取消自己房间的任务被权限层挡了：%s", rec.Body.String())
	}
}

func TestStoreOwnerRoundTripAndUnknownAccount(t *testing.T) {
	f := newFixture(t, "")
	// 首个管理员是登录时懒建出来的（TestAuthGate 那条讲了这套接线），
	// 直接按"admin 的 id 一定是 1"去指派会撞上"账号不存在"，所以先问一次 /session。
	h := f.srv.Handler()
	if rec := do(t, h, http.MethodGet, "/api/v1/session", "admin", "secret"); rec.Code != http.StatusOK {
		t.Fatalf("登录 = %d，body %s", rec.Code, rec.Body.String())
	}
	admin := listAccounts(t, h)[0]

	if err := f.srv.Store.SetClusterOwner("Cluster_1", 99999, "admin"); err != store.ErrAccountNotFound {
		t.Errorf("库里没这个账号时 SetClusterOwner = %v，期望 ErrAccountNotFound", err)
	}
	if err := f.srv.Store.SetClusterOwner("Cluster_1", admin.ID, "admin"); err != nil {
		t.Fatalf("指派给 admin: %v", err)
	}
	owners, err := f.srv.Store.ClusterOwners()
	if err != nil {
		t.Fatalf("读归属表: %v", err)
	}
	o, ok := owners["Cluster_1"]
	if !ok || o.AccountName != "admin" || o.AssignedBy != "admin" {
		t.Fatalf("归属表读出来 = %+v", owners)
	}
	// api 层那把键统一折成小写：库里 COLLATE NOCASE 而 Go 的 map 分大小写，
	// 不折会出现"库里有这行、界面说未指派"。这里钉的是折小写这一步读得到那行。
	if got, err := f.srv.ownerOf("CLUSTER_1"); err != nil || got == nil {
		t.Errorf("折成小写后读不到刚写进去的归属行：%v / %v", got, err)
	}
	names, err := f.srv.Store.ClustersOfAccount(admin.ID)
	if err != nil {
		t.Fatalf("ClustersOfAccount: %v", err)
	}
	if len(names) != 1 || names[0] != "Cluster_1" {
		t.Errorf("%d 名下 = %+v", admin.ID, names)
	}

	// 同一间改派：主键冲突那条 UPDATE 分支要走一遍，否则第二次指派会变成 500。
	alice := newAccount(t, h, "alice", "alice-pass-1", store.RoleMember)
	if err := f.srv.Store.SetClusterOwner("Cluster_1", alice.ID, "admin"); err != nil {
		t.Fatalf("改派: %v", err)
	}
	owners, _ = f.srv.Store.ClusterOwners()
	if len(owners) != 1 || owners["Cluster_1"].AccountName != "alice" {
		t.Errorf("改派后 = %+v", owners)
	}

	if err := f.srv.Store.SetClusterOwner("Cluster_1", 0, "admin"); err != nil {
		t.Fatalf("取消指派: %v", err)
	}
	owners, _ = f.srv.Store.ClusterOwners()
	if len(owners) != 0 {
		t.Errorf("取消后还剩 %+v", owners)
	}
}
