// Package webui 把面板界面内嵌进成品二进制。
//
// 为什么不做成"web 目录放在可执行文件旁边"：这台面板是 scp 一个文件上去就跑起来的，
// 只要有一个目录需要人手工搬，就一定会出现"二进制是新的、界面是旧的"那种现场
// ——而界面和接口是一起改的，旧界面会把新字段读成 undefined，报错还发生在浏览器里。
//
// 前端是免构建的 Vue3（vendor 里那份 global 构建，带模板编译器）+ 手写 CSS：
// 没有 npm、没有打包步骤，改完文件重新 go build 就完了。代价是没有 tree-shaking
// 和类型检查，换来的是这台 4h4g 小机上不需要装 node 工具链。
package webui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:assets
var assets embed.FS

// Root 是去掉 assets/ 前缀之后的界面文件树。
var Root = mustSub(assets, "assets")

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		// embed 指令在编译期就会校验目录，走到这里只可能是有人改了包结构。
		panic("webui: 内嵌资源目录 " + dir + " 不在成品里: " + err.Error())
	}
	return sub
}

// indexName 是单页外壳那份 HTML。界面上除了静态文件全是前端路由，
// 所以任何"文件不存在"的路径都要回它，刷新深链接才不会 404。
const indexName = "index.html"

// Handler 给出界面侧的处理器。
//
// unknownAPI 是"/api/ 开头但没注册路由"时该怎么回，由调用方（api 层）传进来：
// 那种请求必须回 JSON 错误。如果这里也拿 index.html 兜底，探针和脚本会看到
// 一个 200 的 HTML，然后按"接口通了、数据是空"去判断——那是最难查的一类假成功。
func Handler(unknownAPI http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			serve(w, r)
			return
		}
		// /api/ 下的请求只有没被 mux 选中的才会落到这里。
		if unknownAPI != nil {
			unknownAPI(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

func serve(w http.ResponseWriter, r *http.Request) {
	name := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if name == "." || name == "/" || name == "" {
		name = indexName
	}
	// 只允许 GET/HEAD：内嵌的树里没有可写东西，但把方法限死能让"用 POST 打静态路径"
	// 得到一个干脆的 405，而不是被 index.html 兜底成 200。
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "界面只支持 GET 与 HEAD", http.StatusMethodNotAllowed)
		return
	}
	if st, err := fs.Stat(Root, name); err == nil && !st.IsDir() {
		// 每次发版都会改的那几份不许被缓存住：浏览器留着一份旧 app.js，
		// 拿到的是新接口 + 旧渲染，字段改名时界面会显示成空白而不是报错。
		// vendor/ 那份是第三方固定文件（Vue 构建，文件名带版本），可以长缓存。
		setCache(w, strings.HasPrefix(name, "vendor/"))
		serveFile(w, r, name)
		return
	}
	// 认不出来的路径回外壳：前端按 hash 路由（/#/backup），所以浏览器直接访问
	// /backup 这种地址时磁盘上并没有这个文件，兜底给 index.html 才能自己跳到那一页。
	// 这里给的是"外壳这份文件的内容"而不是"再重定向到 /"：URL 长什么样都不影响返回，
	// 所以 ../ 之类的形状既拿不到树外的东西，也不会像 ServeFile 那样反过来报 400。
	setCache(w, false)
	serveFile(w, r, indexName)
}

// serveFile 发一份内嵌文件。
//
// 为什么不用 http.ServeFileFS：它拿 r.URL.Path 参与重定向与合法性判断，
// 于是 /index.html 会被 301 到 /、带 ".." 的路径会被 400 掉。
// 内嵌树本身已经由 fs.FS 挡在 assets 一层里（".." 这种路径在 fs.Stat 就直接失败，
// 走的是上面的外壳兜底），所以这里只需要按文件名把内容发出去。
func serveFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := Root.Open(name)
	if err != nil {
		http.Error(w, "界面文件 "+name+" 不在这个成品里：打包时资源没进去，需要重新构建", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "读不到界面文件 "+name+" 的信息", http.StatusInternalServerError)
		return
	}
	// 内嵌的普通文件都实现 io.ReadSeeker，ServeContent 靠它支持 Range 与 304。
	http.ServeContent(w, r, name, st.ModTime(), f.(io.ReadSeeker))
}

func setCache(w http.ResponseWriter, long bool) {
	if long {
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}
