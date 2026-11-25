package api

import (
	"net/http"
	"strings"
	"testing"
)

// 这一组测的是"界面挂上路由以后，接口那一边有没有被带坏"。
// GET / 是一条通配兜底，它和精确路由共存时的优先级、以及静态资源不该要密码，
// 都是只有真发一次请求才看得出来（embed 层单测全绿、路由漏注册照样白屏）。

// 静态资源不带凭据：assets 里没有任何密钥（webui 包有一条测试守着这点），
// 而登录外壳本身就是静态文件——它要是要认证才能拿到，没登录的人连密码框都看不见。
func TestStaticShellNeedsNoAuth(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()
	for _, path := range []string{"/", "/app.css", "/js/api.js", "/vendor/vue.global.prod.js"} {
		rec := do(t, h, http.MethodGet, path, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s 不带凭据 = %d，期望 200，body %s", path, rec.Code, rec.Body.String())
			continue
		}
		if rec.Body.Len() < 50 {
			t.Errorf("%s 只有 %d 字节", path, rec.Body.Len())
		}
	}
}

// 未注册的接口路径必须回 JSON 404 而不是外壳 HTML：
// 否则脚本会把"200 + 一屏 HTML"读成"接口通了、数据为空"。
func TestUnknownAPIPathIsJSON404(t *testing.T) {
	f := newFixture(t, "")
	h := f.srv.Handler()

	rec := do(t, h, http.MethodGet, "/api/v1/definitely-not-a-route", "admin", "secret")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未注册接口 = %d，期望 404，body %s", rec.Code, rec.Body.String())
	}
	r := decodeReply(t, rec)
	if r.OK {
		t.Error("未注册接口的 ok 字段是真的")
	}
	if !strings.Contains(r.Err, "/api/v1/definitely-not-a-route") {
		t.Errorf("404 里要带上是哪个地址，实际 %q", r.Err)
	}
	if strings.Contains(rec.Body.String(), `id="app"`) {
		t.Errorf("未注册接口被前端外壳兜底了：%s", rec.Body.String())
	}

	// 认证仍然盖住 /api/：没凭据的人不该从 404 的文案里确认"这个前缀存在"。
	rec = do(t, h, http.MethodGet, "/api/v1/definitely-not-a-route", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未注册接口不带凭据 = %d，期望 401", rec.Code)
	}

	// 已知接口不能被通配兜底抢走。
	rec = do(t, h, http.MethodGet, "/api/v1/clusters", "admin", "secret")
	if rec.Code != http.StatusOK {
		t.Errorf("注册过的接口被兜底抢了 = %d，body %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "text/html") {
		t.Errorf("healthz = %d，body %s", rec.Code, rec.Body.String())
	}
}

// 深链接（用户手敲或收藏夹里的 /backup）要拿到外壳，前端自己跳到那一页。
func TestDeepLinkServesShell(t *testing.T) {
	f := newFixture(t, "")
	rec := do(t, f.srv.Handler(), http.MethodGet, "/backup", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="app"`) {
		t.Errorf("深链接 = %d，body %s", rec.Code, rec.Body.String())
	}
}
