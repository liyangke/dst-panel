package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

// 这一组测试守的是"成品里到底有没有把界面打进去"。
// 内嵌资源是唯一一类"编译全绿但成品是空的"失败（embed 指令写错目录名照样能编过），
// 而它的表现是浏览器打开面板只有一片白，且控制台里没有一条后端日志。
func TestEmbeddedAssetsArePresentAndServed(t *testing.T) {
	h := Handler(nil)
	for _, c := range []struct {
		path      string
		ctype     string
		mustHave  string
		minLength int
	}{
		{"/", "text/html", `id="app"`, 200},
		{"/index.html", "text/html", `<script type="module"`, 200},
		{"/app.css", "text/css", "--bg", 500},
		{"/js/api.js", "javascript", "export async function request", 300},
		{"/vendor/vue.global.prod.js", "javascript", "Vue", 50000},
	} {
		rec := get(h, c.path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d，期望 200，body %s", c.path, rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
			continue
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, c.ctype) {
			t.Errorf("%s 的 Content-Type 是 %q，期望含 %q（浏览器靠这个决定要不要当模块脚本执行）", c.path, ct, c.ctype)
		}
		body := rec.Body.String()
		if len(body) < c.minLength {
			t.Errorf("%s 只有 %d 字节，少于 %d：内嵌的那份文件可能是空壳", c.path, len(body), c.minLength)
		}
		if !strings.Contains(body, c.mustHave) {
			t.Errorf("%s 里找不到 %q", c.path, c.mustHave)
		}
	}
}

// 缓存策略要分两档：vendor 那份是固定文件，长缓存能省下每次开页面的 150KB；
// 自家那几份每次发版都变，缓存住了就是"新接口 + 旧界面"那种字段读成 undefined 的现场。
func TestCachePolicy(t *testing.T) {
	h := Handler(nil)
	for _, c := range []struct{ path, want string }{
		{"/vendor/vue.global.prod.js", "immutable"},
		{"/app.css", "no-cache"},
		{"/js/api.js", "no-cache"},
		{"/", "no-cache"},
		{"/backup", "no-cache"}, // SPA 兜底的那份也必须是 no-cache，否则发版后打不开新界面
	} {
		got := get(h, c.path).Header().Get("Cache-Control")
		if !strings.Contains(got, c.want) {
			t.Errorf("%s 的 Cache-Control 是 %q，期望含 %q", c.path, got, c.want)
		}
	}
}

// 前端是 hash 路由，但用户会手敲、也会从收藏夹进没有 # 的地址；
// 那种路径磁盘上不存在，必须回外壳而不是 404。
func TestUnknownNonAPIPathFallsBackToShell(t *testing.T) {
	h := Handler(nil)
	for _, path := range []string{"/backup", "/Cluster_1/Master/status", "/nope/nada", "/app.css.map"} {
		rec := get(h, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d，期望 200 的外壳兜底", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), `id="app"`) {
			t.Errorf("%s 兜底回来的不是外壳：%s", path, rec.Body.String()[:min(120, rec.Body.Len())])
		}
	}
}

// 路径穿越：静态树只有 assets 那一层，仓库里的 go.mod、配置文件、
// 乃至任何绝对路径都不该因为 URL 里多点几个点就吐出去。
func TestPathTraversalNeverLeavesAssetTree(t *testing.T) {
	h := Handler(nil)
	// r.URL.Path 是直接赋值的：正常客户端和 ServeMux 都会先清洗一遍，
	// 这里要测的是"清洗被绕过/上游代理原样转发"时这一层自己站不站得住。
	for _, raw := range []string{
		"/../go.mod", "/../../go.mod", "/js/../../go.mod", "/..%2fgo.mod",
		"//go.mod", "/./../go.mod", "/..//go.mod", "/%2e%2e/%2e%2e/etc/passwd",
		"/..", "/../", "/assets/app.css", "/assets/../go.mod",
	} {
		rec := rawPath(h, http.MethodGet, raw)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d，期望 200 外壳", raw, rec.Code)
			continue
		}
		body := rec.Body.String()
		if strings.Contains(body, "module dstpanel") || strings.Contains(body, "root =") {
			t.Errorf("%s 把仓库文件发出去了：%s", raw, body[:min(200, len(body))])
		}
		if !strings.Contains(body, `id="app"`) {
			t.Errorf("%s 回的不是外壳，也不像仓库文件，先记下形状：%s", raw, body[:min(160, len(body))])
		}
	}
	// /assets/... 这一类必须回外壳而不是文件树：内嵌树被 fs.Sub 剥掉了 assets/ 前缀，
	// 所以带前缀的地址本来就不该命中，命中了说明前缀处理写反了。
	if rec := rawPath(h, http.MethodGet, "/assets/go.mod"); strings.Contains(rec.Body.String(), "module dstpanel") {
		t.Error("/assets/go.mod 泄漏了 go.mod")
	}
}

func TestStaticPathsRejectOtherMethods(t *testing.T) {
	h := Handler(nil)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := rawPath(h, m, "/app.css")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /app.css = %d，期望 405", m, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
			t.Errorf("%s /app.css 没给出 Allow 头：%q", m, allow)
		}
	}
	// DELETE /api/... 之类的没注册接口路径不能掉到静态层的 405 上，
	// 必须由调用方给的处理器回 JSON——这里是"被调用到了"这个事实本身。
	var hit bool
	h2 := Handler(func(w http.ResponseWriter, r *http.Request) { hit = true; w.WriteHeader(http.StatusNotFound) })
	rec := rawPath(h2, http.MethodGet, "/api/v1/nope")
	if !hit || rec.Code != http.StatusNotFound {
		t.Errorf("/api/ 开头的路径没有交给 unknownAPI（hit=%v code=%d）", hit, rec.Code)
	}
	if strings.Contains(rec.Body.String(), `id="app"`) {
		t.Error("/api/ 未注册的路径被外壳兜底了，探针会把 HTML 当成空数据")
	}
}

// 内嵌树里绝对不许出现令牌、面板密钥或引擎存档。
// 这条是给"有人在项目里建了 webui/assets/Cluster_1/ 之类目录当临时文件"准备的：
// go:embed all: 会把它们一起打进成品，而成品是要 scp 到公网机器上的。
func TestEmbeddedTreeHasNoSecretsOrSaves(t *testing.T) {
	badNames := []string{"cluster_token", "server_log", "chat_log", "dstpanel_", "id_dsa", "id_rsa", ".env"}
	var files []string
	err := fs.WalkDir(Root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		lower := strings.ToLower(p)
		for _, b := range badNames {
			if strings.Contains(lower, b) {
				t.Errorf("内嵌树里出现了 %q（文件名含 %q），它会被打进成品", p, b)
			}
		}
		b, rerr := fs.ReadFile(Root, p)
		if rerr != nil {
			return rerr
		}
		if i := strings.Index(string(b), "pds-g^"); i >= 0 {
			t.Errorf("内嵌文件 %q 里有疑似 Klei 令牌的明文（第 %d 字节）", p, i)
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 4 {
		t.Errorf("内嵌树只有 %d 个文件：%v", len(files), files)
	}
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	return rawPath(h, http.MethodGet, path)
}

func rawPath(h http.Handler, method, raw string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/", nil)
	r.URL.Path = raw
	r.URL.RawPath = ""
	h.ServeHTTP(rec, r)
	return rec
}

// 免构建前端唯一一类"编译期抓不到、浏览器里才炸"的缺陷就是 import 路径写错：
// Go 那边全绿，浏览器里 app.js 拿到一个 404 的模块，整屏白掉且只有一句控制台报错。
// 这里把每条相对 import 都当成一次要落地的文件请求来验。
func TestRelativeImportsResolve(t *testing.T) {
	// 只认 from "…" 这种静态写法（本项目不用默认导入以外的形式）；
	// 需要动态加载时这里会漏判，那种改动应当同时更新本测试。
	importRE := regexp.MustCompile(`from\s+"([^"]+)"`)
	files := jsFiles(t)
	if len(files) < 6 {
		t.Fatalf("内嵌树里只有 %d 个 js 文件，界面代码可能没打进去", len(files))
	}
	for _, f := range files {
		src := readFile(t, f)
		for _, m := range importRE.FindAllStringSubmatch(src, -1) {
			target := m[1]
			if !strings.HasPrefix(target, "./") && !strings.HasPrefix(target, "../") {
				// "./js/store.js" 之外只剩 vendor 的 Vue（它是 window 全局，不许被 import）。
				if target == "Vue" || strings.Contains(target, "vue") {
					t.Errorf("%s 里 import 了 %q：Vue 是 vendor 那份挂在 window 上的全局对象，不是模块", f, target)
					continue
				}
				continue
			}
			clean := path.Join(path.Dir(f), target)
			if st, err := fs.Stat(Root, clean); err != nil || st.IsDir() {
				t.Errorf("%s 里写的 %q 在内嵌树里找不到（解析成 %q）——浏览器里会拿 404 并白屏", f, target, clean)
			}
		}
	}
}

// 入口链必须齐：app.js 是 HTML 里写死的那一个文件，它 import 不到的东西等于白屏。
func TestEntryChainExists(t *testing.T) {
	for _, p := range []string{
		"index.html", "app.css", "app.js",
		"js/api.js", "js/fmt.js", "js/store.js", "js/router.js", "js/nav.js",
		"js/ui.js", "js/shell.js", "js/text.js", "js/views/overview.js",
		"js/views/lobby.js",
		"vendor/vue.global.prod.js",
	} {
		if _, err := fs.Stat(Root, p); err != nil {
			t.Errorf("入口链上少了 %s：%v", p, err)
		}
	}
	// index.html 引的三个地址必须真的在树里：链接写错的名字不会有任何报错，
	// 只会表现为"样式没了"或"页面不动"。
	html := readFile(t, "index.html")
	for _, ref := range []string{"/app.css", "/vendor/vue.global.prod.js", "/app.js"} {
		if !strings.Contains(html, ref) {
			t.Errorf("index.html 里没有引用 %s", ref)
		}
		if _, err := fs.Stat(Root, strings.TrimPrefix(ref, "/")); err != nil {
			t.Errorf("index.html 引用的 %s 不在树里", ref)
		}
	}
}

// 导航表自己要对得上：新加一页时最容易漏的两件事——写了条目却忘 import（浏览器里
// ReferenceError，整页白掉）、import 了组件却没写条目（这一页根本进不去，左导航里没有它）。
// 这两种在 Go 编译期都不报错，路径也全对，所以只能在这里对表。
func TestNavPagesAreWiredUp(t *testing.T) {
	src := readFile(t, "js/nav.js")

	imported := map[string]string{}
	for _, m := range regexp.MustCompile(`import\s+(\w+)\s+from\s+"([^"]+)"`).FindAllStringSubmatch(src, -1) {
		clean := path.Join(path.Dir("js/nav.js"), m[2])
		st, err := fs.Stat(Root, clean)
		if err != nil || st.IsDir() {
			t.Errorf("nav.js import 的 %q 不在树里（解析成 %q）", m[2], clean)
			continue
		}
		body := readFile(t, clean)
		if !strings.Contains(body, "export default") {
			t.Errorf("%s 没有 export default：nav.js 拿到的是一个空的模块命名空间，那一页会白屏", clean)
		}
		imported[m[1]] = clean
	}

	names := regexp.MustCompile(`(?m)^\s+name:\s*"([^"]+)"`).FindAllStringSubmatch(src, -1)
	views := regexp.MustCompile(`(?m)^\s+view:\s*(\w+)`).FindAllStringSubmatch(src, -1)
	segments := regexp.MustCompile(`(?m)^\s+segments:\s*\[([^\]]*)\]`).FindAllStringSubmatch(src, -1)

	if len(names) < 10 || len(views) < 10 {
		t.Fatalf("只解析到 %d 个 name / %d 个 view，nav.js 的写法可能变了，这条测试得跟着改", len(names), len(views))
	}
	// 一屏条目和 view 必须一比一：漏一个的话那一行点进去就是 NotFound。
	if len(names) != len(views) {
		t.Errorf("导航条目 %d 个但 view 只有 %d 个：有条目没接组件，或者有组件没条目", len(names), len(views))
	}

	seen := map[string]bool{}
	for _, m := range names {
		if seen[m[1]] {
			t.Errorf("页面名 %q 出现了两次：路由只会认第一条，另一条永远点不到", m[1])
		}
		seen[m[1]] = true
	}
	used := map[string]bool{}
	for _, m := range views {
		if m[1] == "NotFound" {
			continue
		}
		used[m[1]] = true
		if _, ok := imported[m[1]]; !ok {
			t.Errorf("view: %s 没有对应的 import：浏览器里这里是 ReferenceError，整页白屏", m[1])
		}
	}
	for ident, file := range imported {
		if used[ident] {
			continue
		}
		// store.js 那类不是组件的 import 走的是另一条语法（import { state }），不在这里。
		// 按整词数：数子串的话 "Lobbyy" 里也算一次 Lobby，漏用的 import 就藏住了。
		n := len(regexp.MustCompile(`\b`+ident+`\b`).FindAllString(src, -1))
		if n == 1 {
			t.Errorf("import 了 %s（%s）却没有页面用它：这一页在导航里进不去", ident, file)
		}
	}

	head := map[string]string{}
	for _, m := range segments {
		inner := strings.TrimSpace(m[1])
		if inner == "" {
			continue
		}
		first := strings.Trim(strings.Fields(inner)[0], `",`)
		if by, dup := head[first]; dup {
			t.Errorf("段 %q 被 %q 和另一条条目同时占用", first, by)
		}
		head[first] = m[1]
	}
}

// 树里每个文件都要发得出去。这条是"embed 收到了但 serve 拿不到"的兜底
// （比如目录前缀处理写反、或者某份文件被缓存策略误伤成 404）。
func TestEveryEmbeddedFileIsFetchable(t *testing.T) {
	h := Handler(nil)
	var n int
	for _, f := range allFiles(t) {
		rec := rawPath(h, http.MethodGet, "/"+f)
		n++
		if rec.Code != http.StatusOK {
			t.Errorf("GET /%s = %d", f, rec.Code)
			continue
		}
		want := len(readFile(t, f))
		if rec.Body.Len() != want {
			t.Errorf("GET /%s 发出 %d 字节，文件是 %d 字节", f, rec.Body.Len(), want)
		}
	}
	if n < 12 {
		t.Errorf("只遍历到 %d 个文件，内嵌资源可能没打全", n)
	}
}

// 不许出现第二个 Vue 副本，也不许出现打包产物：那说明有人手工往 assets 里塞了 build 输出。
func TestNoStrayBundlesOrSecondVueCopy(t *testing.T) {
	for _, f := range allFiles(t) {
		base := strings.ToLower(path.Base(f))
		if strings.HasSuffix(base, ".min.js") && !strings.Contains(f, "vendor/") {
			t.Errorf("%s 看着像打包产物：这个界面刻意不做打包步骤", f)
		}
		if (base == "vue.js" || base == "vue.prod.js" || strings.HasPrefix(base, "vue.")) && !strings.HasPrefix(f, "vendor/") {
			t.Errorf("%s 是 vendor 之外又一份 Vue 副本", f)
		}
	}
}

// 公网房间页那两个"看不见就没人能发现"的口子钉在这儿。
// 两个都是实测修过的真 bug，共同点是后端数据完全正常、接口 200、控制台无错，
// 只有把浏览器打开对着截图看才看得出来，所以必须用源码断言守住。
//
// 一、复制失败时那句 c_connect 要真的渲染成一行的字。
//
//	原先它只挂在按钮的悬停标题上，而说明文字写的是"请把上面那句选中"——
//	页面上根本没有那一句，用户照着提示会对着空白处按 Ctrl+C。
//
// 二、那枚排序下拉的当前值要跟着接口回的那一份填。
//
//	三枚排序选项里没有"值为空"的那一项，接口回 query.sort="" 时浏览器只会画一个空框，
//	列表照样排得好好的（lobby.sortRooms 的空值走的就是 players 分支），
//	所以这个错在接口响应里查不出来，只能从"前端有没有同步这一项"上防。
func TestLobbyViewKeepsCopyFallbackAndSortSelectVisible(t *testing.T) {
	src := readFile(t, "js/views/lobby.js")

	if !strings.Contains(src, "copy-cmd") || !strings.Contains(src, "{{ failedCmd }}") {
		t.Errorf("复制失败时那一句命令没渲染成可选中的文本：说明里承诺了「可以选中再 Ctrl+C」，" +
			"页面上就得真有一行字（.copy-cmd + failedCmd）")
	}
	// 提示语和渲染必须一起改：只留悬停标题的话，说明里就不能出现"上面/下面那句"。
	// 先整行剔掉注释再找：注释里可以正当地引用那句被改掉的旧文案（它写清了为什么要改），
	// 连着源码一起匹配就会把"解释 bug 的那句话"当成"bug 本身"，红得莫名其妙。
	code := regexp.MustCompile(`(?m)^[ \t]*//.*$`).ReplaceAllString(src, "")
	if m := regexp.MustCompile(`[^\n]*(?:上面那句|下面那句)[^\n]*`).FindString(code); m != "" {
		t.Errorf("lobby.js 的代码或模板串里出现了「上面/下面那句」这种指代，但页面不一定真有这样一行字：%s", strings.TrimSpace(m))
	}
	if !strings.Contains(src, "this.sort = q.sort") {
		t.Errorf("排序下拉没跟着接口回的那一份同步（this.sort = q.sort）：后端给了默认值界面还是空框")
	}
}

func jsFiles(t *testing.T) []string {
	return pickFiles(t, ".js")
}

func allFiles(t *testing.T) []string {
	return pickFiles(t, "")
}

func pickFiles(t *testing.T, suffix string) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(Root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if suffix == "" || strings.HasSuffix(strings.ToLower(p), suffix) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := fs.ReadFile(Root, p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return string(b)
}
