package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"dstpanel/internal/config"
)

func doJSONAuth(t *testing.T, h http.Handler, method, path, user, pass, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeInto 解 data 字段；201 也是成功，decodeData 只认 200/202。
func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	r := decodeReply(t, rec)
	raw, err := json.Marshal(r.Data)
	if err != nil {
		t.Fatalf("重编码响应数据: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("解码响应数据: %v，原文 %s", err, string(raw))
	}
}

func TestSessionReportsRole(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	rec := do(t, h, http.MethodGet, "/api/v1/session", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("session = %d，body %s", rec.Code, rec.Body.String())
	}
	var v sessionView
	decodeInto(t, rec, &v)
	if v.Username != "admin" || v.Role != "admin" || v.RoleLabel != "管理员" || !v.IsAdmin {
		t.Fatalf("session = %+v", v)
	}
	if v.UsingDefaultPassword {
		t.Error("这个面板的管理员密码是 secret，不该被说成在用出厂默认密码")
	}
}

// TestDefaultPasswordIsSurfaced 钉住"还在用出厂默认密码"这件事在三个入口都看得见：
// 新建返回、账号列表、/session。默认凭据是公开信息，提醒只要断供就等于没有，
// 而这只能靠接口回话——明文没入库，界面自己算不出来。
func TestDefaultPasswordIsSurfaced(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	rec := doJSONAuth(t, h, http.MethodPost, "/api/v1/accounts", "admin", "secret",
		`{"username":"stilldefault","password":"`+config.DefaultAdminPassword+`","role":"viewer"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("用出厂默认密码建账号 = %d，body %s", rec.Code, rec.Body.String())
	}
	var created accountView
	decodeInto(t, rec, &created)
	if !created.UsingDefaultPassword {
		t.Error("新建时就在用默认密码，返回的视图里必须标出来")
	}

	var seenDefault bool
	for _, a := range listAccounts(t, h) {
		switch a.Username {
		case "stilldefault":
			seenDefault = true
			if !a.UsingDefaultPassword {
				t.Error("账号列表没标出这个账号还在用默认密码")
			}
		case "admin":
			if a.UsingDefaultPassword {
				t.Error("admin 用的是 secret，不该被标成默认密码")
			}
		}
	}
	if !seenDefault {
		t.Fatal("列表里找不到 stilldefault")
	}

	rec = do(t, h, http.MethodGet, "/api/v1/session", "stilldefault", config.DefaultAdminPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("默认密码账号的 session = %d，body %s", rec.Code, rec.Body.String())
	}
	var sess sessionView
	decodeInto(t, rec, &sess)
	if !sess.UsingDefaultPassword {
		t.Error("session 没报告在用默认密码，外壳就挂不出提醒")
	}
}

func TestAccountCreateViewerReadOnlyAndSelfPassword(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	rec := doJSONAuth(t, h, http.MethodPost, "/api/v1/accounts", "admin", "secret",
		`{"username":"guest","password":"guest-pass-1","role":"viewer"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("新建账号 = %d，body %s", rec.Code, rec.Body.String())
	}
	var created accountView
	decodeInto(t, rec, &created)
	if created.Username != "guest" || created.Role != "viewer" || created.RoleLabel != "只读" {
		t.Fatalf("新账号 = %+v", created)
	}

	// 只读账号能看，不能改，也不能进账号管理。
	if rec := do(t, h, http.MethodGet, "/api/v1/clusters", "guest", "guest-pass-1"); rec.Code != http.StatusOK {
		t.Errorf("只读账号读接口 = %d，期望 200", rec.Code)
	}
	if rec := doJSONAuth(t, h, http.MethodPost, "/api/v1/clusters", "guest", "guest-pass-1", `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("只读账号写接口 = %d，期望 403", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/accounts", "guest", "guest-pass-1"); rec.Code != http.StatusForbidden {
		t.Errorf("只读账号看账号列表 = %d，期望 403", rec.Code)
	}

	// 只读账号可以改自己的密码（唯一允许的写操作）。
	rec = doJSONAuth(t, h, http.MethodPut, "/api/v1/session/password", "guest", "guest-pass-1",
		`{"oldPassword":"guest-pass-1","newPassword":"guest-pass-2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("改自己密码 = %d，body %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/session", "guest", "guest-pass-1"); rec.Code != http.StatusUnauthorized {
		t.Errorf("旧密码还能登录 = %d，期望 401", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/session", "guest", "guest-pass-2"); rec.Code != http.StatusOK {
		t.Errorf("新密码登录 = %d，期望 200", rec.Code)
	}

	// 旧密码填错不能改；新密码太短也不行。
	if rec := doJSONAuth(t, h, http.MethodPut, "/api/v1/session/password", "guest", "guest-pass-2",
		`{"oldPassword":"wrong","newPassword":"whatever-1"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("旧密码错 = %d，期望 400", rec.Code)
	}
	if rec := doJSONAuth(t, h, http.MethodPut, "/api/v1/session/password", "guest", "guest-pass-2",
		`{"oldPassword":"guest-pass-2","newPassword":"abcd"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("新密码太短 = %d，期望 400", rec.Code)
	}
	// 5 位要能过：内置默认凭据本身就是 5 位，规则比默认值严的话，
	// 照着文档来的人反而会被自己的接口拦住。
	if rec := doJSONAuth(t, h, http.MethodPut, "/api/v1/session/password", "guest", "guest-pass-2",
		`{"oldPassword":"guest-pass-2","newPassword":"abcde"}`); rec.Code != http.StatusOK {
		t.Errorf("5 位新密码 = %d，期望 200，body %s", rec.Code, rec.Body.String())
	}
}

func TestAdminResetsPasswordAndRole(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	rec := doJSONAuth(t, h, http.MethodPost, "/api/v1/accounts", "admin", "secret",
		`{"username":"helper","password":"helper-pass-1","role":"viewer"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("新建账号 = %d，body %s", rec.Code, rec.Body.String())
	}
	var created accountView
	decodeInto(t, rec, &created)

	// 管理员重置密码后，旧密码立刻失效。
	rec = doJSONAuth(t, h, http.MethodPut, "/api/v1/accounts/"+itoa(created.ID)+"/password", "admin", "secret",
		`{"password":"helper-pass-2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("重置密码 = %d，body %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/session", "helper", "helper-pass-1"); rec.Code != http.StatusUnauthorized {
		t.Errorf("被重置的旧密码还能用 = %d，期望 401", rec.Code)
	}

	// 升成管理员后可以进账号列表。
	rec = doJSONAuth(t, h, http.MethodPut, "/api/v1/accounts/"+itoa(created.ID)+"/role", "admin", "secret",
		`{"role":"admin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("改角色 = %d，body %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/accounts", "helper", "helper-pass-2"); rec.Code != http.StatusOK {
		t.Errorf("新管理员看账号列表 = %d，期望 200", rec.Code)
	}

	list := listAccounts(t, h)
	if len(list) != 2 {
		t.Fatalf("账号数 = %d，期望 2：%+v", len(list), list)
	}
	selfCount := 0
	for _, a := range list {
		if a.Self {
			selfCount++
		}
	}
	if selfCount != 1 {
		t.Errorf("列表里标记为自己的账号数 = %d，期望 1", selfCount)
	}

	entries, err := f.srv.Store.ListAudit(50, 0, "account.")
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	want := map[string]bool{"account.create": false, "account.password": false, "account.role": false}
	for _, e := range entries {
		if _, ok := want[e.Action]; ok {
			want[e.Action] = true
		}
	}
	for action, seen := range want {
		if !seen {
			t.Errorf("审计里没有 %s：%+v", action, entries)
		}
	}
}

func TestLastAdminAndSelfDeletionAreProtected(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	list := listAccounts(t, h)
	if len(list) != 1 {
		t.Fatalf("初始账号 = %+v", list)
	}
	adminID := list[0].ID

	// 最后一名管理员不能降成只读。
	rec := doJSONAuth(t, h, http.MethodPut, "/api/v1/accounts/"+itoa(adminID)+"/role", "admin", "secret",
		`{"role":"viewer"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("降级最后一名管理员 = %d，期望 409：%s", rec.Code, rec.Body.String())
	}

	// 当前登录账号不能删自己。
	if rec := doJSONAuth(t, h, http.MethodDelete, "/api/v1/accounts/"+itoa(adminID), "admin", "secret", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("删自己 = %d，期望 400", rec.Code)
	}

	// 最后一名管理员也不能被管理员重置自己的密码（避免绕过旧密码校验）。
	if rec := doJSONAuth(t, h, http.MethodPut, "/api/v1/accounts/"+itoa(adminID)+"/password", "admin", "secret",
		`{"password":"new-admin-pass"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("管理员重置自己 = %d，期望 400", rec.Code)
	}
}

func TestAccountValidation(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	cases := []struct {
		name string
		body string
		want int
	}{
		{"用户名太短", `{"username":"a","password":"12345678","role":"viewer"}`, http.StatusBadRequest},
		{"用户名带空格", `{"username":"a b","password":"12345678","role":"viewer"}`, http.StatusBadRequest},
		{"密码太短", `{"username":"okname","password":"1234","role":"viewer"}`, http.StatusBadRequest},
		{"角色不认识", `{"username":"okname","password":"12345678","role":"root"}`, http.StatusBadRequest},
		{"重复用户名", `{"username":"admin","password":"12345678","role":"viewer"}`, http.StatusConflict},
		{"5 位密码放行", `{"username":"shortpw","password":"12345","role":"viewer"}`, http.StatusCreated},
	}
	for _, c := range cases {
		rec := doJSONAuth(t, h, http.MethodPost, "/api/v1/accounts", "admin", "secret", c.body)
		if rec.Code != c.want {
			t.Errorf("%s = %d，期望 %d，body %s", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}

func listAccounts(t *testing.T, h http.Handler) []accountView {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/api/v1/accounts", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("账号列表 = %d，body %s", rec.Code, rec.Body.String())
	}
	var list []accountView
	decodeInto(t, rec, &list)
	return list
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
