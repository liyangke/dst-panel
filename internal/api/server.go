// Package api 是面板的 HTTP 层。
//
// 为什么只用标准库 ServeMux：这个面板的路由形态是"十来个 GET、十来个 POST，路径带两段变量"，
// 框架能给的增益主要是省几行写法，代价是多一个依赖和一层看不到源码的行为
// （中间件顺序、panic 处理、JSON 绑定）。Go 1.22 起 ServeMux 支持方法和通配段，
// 剩下的差距就不值得付了。
//
// 一条贯穿全部接口的约定：响应里不出现裸码值。
// status 是 "running" 这种给机器看的，同时必须给 statusLabel 中文，前端直接渲染，
// 免得翻译表在两边各写一份、改一边忘一边。
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"dstpanel/internal/auth"
	"dstpanel/internal/backup"
	"dstpanel/internal/config"
	"dstpanel/internal/dst"
	"dstpanel/internal/hoststats"
	"dstpanel/internal/lobby"
	"dstpanel/internal/modbridge"
	"dstpanel/internal/monitor"
	"dstpanel/internal/procrun"
	"dstpanel/internal/steam"
	"dstpanel/internal/store"
	"dstpanel/internal/webui"
)

type Server struct {
	Paths  dst.Paths
	Procs  *procrun.Driver
	Bridge *modbridge.Bridge
	Store  *store.Store
	Auth   config.Server
	// SteamCmd 是配置里的 steamcmd.sh 路径。异步任务的提交口要在建任务之前
	// 先看它配了没有，否则用户会拿到一条 id，然后在一堆任务里看到失败。
	SteamCmd string
	// Network 是配置里的端口区间。新建集群要在这个区间里挑空闲端口，
	// 所以建集群的接口必须拿到它，而不是自己另写一套默认值。
	Network config.Network
	// World 是世界设定字典（内嵌在成品里的 Klei 官方表）。
	// 界面渲染下拉框、写文件前拦非法取值都走这一份；为 nil 时相关接口直接报错，
	// 因为那意味着成品里的字典文件没打进来，属于构建问题而不是运行时问题。
	World *dst.WorldSchema
	// Steam 是工坊在线检索客户端。为 nil 或没 key 时接口自己降级到本地缓存，
	// 所以所有调用点都必须先 Enabled()，不能让一个空指针把检索页搞挂。
	Steam *steam.Client
	// Backups 是存档快照层。为 nil 时存档页的接口直接回一句"没接上"，
	// 而不是 panic 成一个 500——接线漏了这种事要让界面说得出原因。
	Backups *backup.Manager
	// Host 是这台机器的运行状态采集器（CPU/内存/磁盘/网络）。它必须是有状态的单例：
	// 百分比要拿两次采样的差值，每次请求新建一个就永远只能报"刚开始采样"。
	Host *hoststats.Collector
	// Lobby 是公网房间列表的缓存（Klei 的公开 CDN 快照，不填任何凭据）。
	// 同样必须是有状态的单例：缓存的正是"这个大区几分钟内别再去拉十几 MB"。
	// 为 nil 或配置里没打开时接口回一句中文说明，界面不会因此白屏（见 api/lobby.go）。
	Lobby *lobby.Cache
	// SettingDefaults 是配置文件里那几个运行参数的当前值（保留几份、几小时一次）。
	// 设置页用它当"库里没改过时的值"，否则界面显示的默认数和实际生效的数会各一套。
	SettingDefaults map[string]int
	// Logger 为空时用 slog.Default()，测试里传 discard 的 logger。
	Logger *slog.Logger

	// Monitor 是监控采样/告警/自愈的共享状态，由 main.go 装配。
	// 为 nil 时（测试夹具、裁剪版）相关接口回 503，其余接口完全按原行为跑。
	Monitor *monitor.Supervisor

	authOnce  sync.Once
	authCache *auth.Cache

	// authLimitMu 保护按来源 IP 的登录失败计数与临时锁定（问题 6）。
	authLimitMu  sync.Mutex
	authFailures map[string]*authAttempt

	// mapMu 把"检查有没有在导出"和"入队"合成一步。
	// 不加锁时两个同时打开的地图页会各看到一次空队列，然后各塞一条 map 命令，
	// 助手要为此跑两遍两万格采样。
	mapMu       sync.Mutex
	mapRequests map[string]time.Time
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Handler 组装路由。认证只盖住 /api/，/healthz 留给探活脚本——
// 反代和 systemd 的健康检查不该拿着管理员密码。
func (s *Server) Handler() http.Handler {
	// 首次启动时把配置文件里的管理员写成第一个库内账号；之后以库为准，
	// 在页面上改密码就不再需要动 yaml、也不再需要重启面板。
	s.ensureBootstrapAccount()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/session", s.handleSession)
	mux.HandleFunc("PUT /api/v1/session/password", s.handleSelfPassword)
	mux.HandleFunc("GET /api/v1/accounts", s.handleAccountsList)
	mux.HandleFunc("POST /api/v1/accounts", s.handleAccountCreate)
	mux.HandleFunc("PUT /api/v1/accounts/{id}/password", s.handleAccountPassword)
	mux.HandleFunc("PUT /api/v1/accounts/{id}/role", s.handleAccountRole)
	mux.HandleFunc("DELETE /api/v1/accounts/{id}", s.handleAccountDelete)
	mux.HandleFunc("GET /api/v1/clusters", s.handleClusters)
	// 房间归属的指派口。它是管理员的决定，所以即便路径挂在房间下面，
	// guardRooms 也单独把它留给管理员（见 scope.go 里 /owner 那段）。
	mux.HandleFunc("PUT /api/v1/clusters/{name}/owner", s.handleClusterOwnerPut)
	// 房间总览：所有集群的所有分片一次返回，筛选和分页在界面做（数据量是"几个进程"，
	// 不是"几万条记录"，服务端分页只会让刷新变慢和让搜索变成跨页搜索这种伪需求）。
	mux.HandleFunc("GET /api/v1/rooms", s.handleRooms)
	// 公网上的房间：读 Klei 放在 CDN 上的公开快照，不填任何凭据（见 lobby 包的注释）。
	// 这一组和上面那条不是一回事——这里只给看得到的行和一句连接说明，
	// 不给任何启停/改配置的入口，因为那些房间不是这台机器上的。
	mux.HandleFunc("GET /api/v1/lobby/regions", s.handleLobbyRegions)
	mux.HandleFunc("GET /api/v1/lobby/rooms", s.handleLobbyRooms)
	// 加入房间那句命令按需现拼：它要读 server.ini 和这次的请求 Host，
	// 塞进 /rooms 的话每次轮询都多一遍磁盘读，而绝大多数刷新根本不点复制。
	mux.HandleFunc("GET /api/v1/clusters/{name}/connect", s.handleConnect)
	mux.HandleFunc("POST /api/v1/clusters", s.handleClusterCreate)
	mux.HandleFunc("GET /api/v1/world-schema", s.handleWorldSchema)
	mux.HandleFunc("GET /api/v1/clusters/{name}/settings", s.handleClusterSettingsShow)
	mux.HandleFunc("PUT /api/v1/clusters/{name}/settings", s.handleClusterSettingsPut)
	// 配置改动可见性：保存前先看 diff，历史备份可列举与回滚（见 cluster_settings.go）。
	mux.HandleFunc("GET /api/v1/clusters/{name}/settings/diff", s.handleClusterSettingsDiff)
	mux.HandleFunc("GET /api/v1/clusters/{name}/settings/backups", s.handleClusterSettingsBackups)
	mux.HandleFunc("POST /api/v1/clusters/{name}/settings/backups/rollback", s.handleClusterSettingsRollback)
	// 端口单独一对接口：它牵动三个文件、改前必须停服，和改人数不是一类动作（见 ports.go）。
	mux.HandleFunc("GET /api/v1/ports/plan", s.handlePortPlan)
	mux.HandleFunc("GET /api/v1/clusters/{name}/ports", s.handlePortsShow)
	mux.HandleFunc("PUT /api/v1/clusters/{name}/ports", s.handlePortsPut)
	mux.HandleFunc("GET /api/v1/clusters/{name}/token", s.handleTokenShow)
	mux.HandleFunc("PUT /api/v1/clusters/{name}/token", s.handleTokenPut)
	mux.HandleFunc("DELETE /api/v1/clusters/{name}/token", s.handleTokenDelete)
	mux.HandleFunc("GET /api/v1/clusters/{name}/players", s.handlePlayers)
	mux.HandleFunc("POST /api/v1/clusters/{name}/players/lists/{kind}", s.handlePlayerList)
	mux.HandleFunc("POST /api/v1/clusters/{name}/players/{userid}/{action}", s.handlePlayerAction)
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/world", s.handleWorldShow)
	mux.HandleFunc("PUT /api/v1/clusters/{name}/shards/{shard}/world", s.handleWorldPut)
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/status", s.handleShardStatus)
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/map", s.handleShardMap)
	mux.HandleFunc("POST /api/v1/clusters/{name}/shards/{shard}/start", s.handleShardStart)
	mux.HandleFunc("POST /api/v1/clusters/{name}/shards/{shard}/stop", s.handleShardStop)
	mux.HandleFunc("POST /api/v1/clusters/{name}/shards/{shard}/kill", s.handleShardKill)
	mux.HandleFunc("POST /api/v1/clusters/{name}/shards/{shard}/restart", s.handleShardRestart)
	mux.HandleFunc("GET /api/v1/admin-commands", s.handleAdminCommands)
	mux.HandleFunc("POST /api/v1/clusters/{name}/shards/{shard}/admin-commands", s.handleAdminCommandPost)
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/helper", s.handleHelperShow)
	mux.HandleFunc("PUT /api/v1/clusters/{name}/shards/{shard}/helper", s.handleHelperSet)
	mux.HandleFunc("GET /api/v1/log-events", s.handleLogEvents)
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/log", s.handleLogShow)
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/log/stream", s.handleLogStream)
	// 历史日志分页与导出：窗口之外的行不再只能"去服务端 grep"（见 gamelog.go）。
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/log/history", s.handleLogHistory)
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/log/export", s.handleLogExport)
	// 玩家事件表：同一个世界、两份文件合起来的行为，筛选和分页都在后端（见 events.go）。
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/commands", s.handleCommands)
	mux.HandleFunc("POST /api/v1/clusters/{name}/shards/{shard}/commands", s.handleCommandPost)
	mux.HandleFunc("GET /api/v1/tasks", s.handleTasks)
	mux.HandleFunc("GET /api/v1/task-kinds", s.handleTaskKinds)
	mux.HandleFunc("POST /api/v1/tasks", s.handleTaskSubmit)
	mux.HandleFunc("DELETE /api/v1/tasks/{id}", s.handleTaskCancel)
	mux.HandleFunc("GET /api/v1/mods", s.handleModsShow)
	mux.HandleFunc("GET /api/v1/mods/search", s.handleModSearch)
	mux.HandleFunc("GET /api/v1/mods/{dir}/schema", s.handleModSchema)
	mux.HandleFunc("POST /api/v1/mods/setup", s.handleSetupAdd)
	mux.HandleFunc("POST /api/v1/mods/subscribe-and-open", s.handleSubscribeOpen)
	mux.HandleFunc("DELETE /api/v1/mods/setup/{id}", s.handleSetupRemove)
	mux.HandleFunc("POST /api/v1/mods/upload", s.handleModUpload)
	mux.HandleFunc("DELETE /api/v1/mods/files/{dir}", s.handleModFileDelete)
	mux.HandleFunc("GET /api/v1/clusters/{name}/shards/{shard}/mods", s.handleShardModsShow)
	mux.HandleFunc("PUT /api/v1/clusters/{name}/shards/{shard}/mods/{dir}", s.handleShardModPatch)
	mux.HandleFunc("GET /api/v1/clusters/{name}/mods/consistency", s.handleModConsistency)
	mux.HandleFunc("GET /api/v1/stats", s.handleStats)
	mux.HandleFunc("GET /api/v1/player-timeline", s.handlePlayerTimeline)
	mux.HandleFunc("GET /api/v1/host", s.handleHost)
	mux.HandleFunc("GET /api/v1/audit", s.handleAudit)
	mux.HandleFunc("GET /api/v1/audit-actions", s.handleAuditActions)
	// 存档快照。列表是整页数据（含保留策略和磁盘余量），房间过滤用 ?cluster=；
	// 备份挂在房间上、恢复挂在快照上，因为这两个动作各自只有一个可信的主语。
	mux.HandleFunc("GET /api/v1/backups", s.handleBackups)
	mux.HandleFunc("POST /api/v1/clusters/{name}/backups", s.handleBackupCreate)
	mux.HandleFunc("GET /api/v1/backups/{id}", s.handleBackupDetail)
	mux.HandleFunc("DELETE /api/v1/backups/{id}", s.handleBackupDelete)
	mux.HandleFunc("GET /api/v1/backups/{id}/download", s.handleBackupDownload)
	mux.HandleFunc("POST /api/v1/backups/{id}/verify", s.handleBackupVerify)
	mux.HandleFunc("POST /api/v1/backups/{id}/restore", s.handleBackupRestore)
	// 导入外部存档。预览是"上传 + 只读包目录"，提交只往队列里放一条任务；
	// 两条都挂在 /backups 下面而不是挂在房间下面，因为上传包在落地那一刻还不属于任何房间。
	mux.HandleFunc("POST /api/v1/backups/import/preview", s.handleBackupImportPreview)
	mux.HandleFunc("POST /api/v1/backups/import", s.handleBackupImport)
	mux.HandleFunc("GET /api/v1/settings", s.handleSettings)
	mux.HandleFunc("PUT /api/v1/settings", s.handleSettingsPut)
	// 监控告警视图与自愈/阈值配置（新增接口，既有 /host、/stats 的响应字段不变）。
	mux.HandleFunc("GET /api/v1/monitor", s.handleMonitor)
	mux.HandleFunc("PUT /api/v1/monitor/config", s.handleMonitorConfigPut)
	// 界面：内嵌在成品里的单页外壳。GET / 这条通配兜底只会接住没注册过的路径，
	// 上面那些精确路由优先级更高，所以接口不受影响。
	// /api/ 开头却没匹配到的请求必须回 JSON 404：拿 index.html 兜底会让探针
	// 看到"200 且是 HTML"，然后按"接口通了、数据为空"下结论——那是最难查的假成功。
	mux.Handle("GET /", webui.Handler(s.handleNoSuchAPI))
	// 顺序：recover 在最外（一个 panic 不能带走整个面板），然后记日志，
	// 然后核对凭据，最后按"这条请求的主语是哪间房"过归属这道闸。
	return s.recover(s.drainBody(s.logRequests(s.authenticate(s.guardRooms(mux)))))
}

// ---- 中间件 ----

// drainBody 在处理器返回之后，把请求体里剩下没读掉的字节抽干。
//
// 为什么要有这一层：写接口里绝大多数（启停、指派、改设置）的主语在路径上，
// 处理器要么根本不读体、要么只读一半，响应写完了体还留在内核的接收队列里。
// 这时关连接，Windows 用 RST 而不是 FIN 收尾，客户端把"已经写好的响应字节"
// 一起收不到——界面上就是点了「启动」而面板报网络错误，审计里其实已经记下了一条 400。
// 本机 127.0.0.1 实测（30 次一组，两种连接复用方式各一组）：
// 不读体的 POST 重置 14/30 与 10/30，处理器自己读干净或由这一层抽干净的都是 0/30，
// 不带体的 POST 和 GET 也是 0/30。所以这不是客户端的毛病，也不能靠重试糊过去。
//
// 上限 4 MB 是留给"大上传读到一半被拒"那种的：正常 JSON 请求远在它以下，一次就抽得干净；
// 真超过的说明体量大到不值得为它把这次请求挂住。
func (s *Server) drainBody(next http.Handler) http.Handler {
	const drainLimit = 4 << 20
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if r.Body == nil {
				return
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, drainLimit))
			_ = r.Body.Close()
		}()
		next.ServeHTTP(w, r)
	})
}

// recover 把 panic 关在单个请求里。
// 面板进程一旦挂了，正在跑的 DST 分片虽然还活着（它们在 screen 里），
// 但用户看到的是"整个后台打不开"，这个代价不能由一个空指针换来。
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log().Error("请求处理 panic",
					"method", r.Method, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				writeErr(w, http.StatusInternalServerError, "面板内部错误，详情看平台运行日志")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap 是给 http.ResponseController 用的：它靠这个方法一层层找到最底下的真实 ResponseWriter。
// 少了它，SSE 的 Flush() 会以为这个包装层不支持流式输出（statusWriter 只转发了三个方法），
// 于是日志流在每个响应上都退化成"憋满才发"。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		// 探活请求每秒都可能来一次，全记下来会把真正有用的行冲走。
		if r.URL.Path == "/healthz" {
			return
		}
		s.log().Info("请求",
			"method", r.Method, "path", r.URL.Path, "status", sw.code,
			"cost_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

// authenticate 是 HTTP Basic。
//
// 为什么不是 session：面板默认只绑 127.0.0.1，对外访问由反代或 SSH 隧道兜，
// Basic 不需要服务端存会话状态，也就没有"重启后 everyone 都掉线"和会话表膨胀。
// 代价是密码在请求头里反复出现，所以强烈建议对外时套 HTTPS（见部署文档）。
//
// 账号在库里（内部 auth 包做 PBKDF2 哈希），每个请求只多一次按用户名的索引查询；
// PBKDF2 的结果有短时缓存，前端每五秒一轮的轮询不会把小机 CPU 吃满。
//
// 另外按来源 IP 做失败限速：连续 5 次凭据不对锁 60 秒，锁定期给 429 中文说明。
// 成功登录清零计数；不带凭据的请求和系统错误都不算失败，免得把正常浏览挡掉。
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		now := time.Now()
		if locked, secs := s.authLocked(ip, now); locked {
			writeErr(w, http.StatusTooManyRequests,
				fmt.Sprintf("登录失败次数过多，该来源 IP 已被临时锁定，请 %d 秒后再试", secs))
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || strings.TrimSpace(user) == "" {
			// 没带凭据不算「认证失败尝试」：探活、静态资源、未登录访问都走这条路，
			// 把它们计进去会把正常浏览的用户也锁掉。
			s.denyUnauthorized(w)
			return
		}
		sess, err := s.checkLogin(user, pass)
		if err != nil {
			if errors.Is(err, errNoAccounts) {
				// 没账号就绝不放行，也不能"默认 admin/admin"那种更糟的做法。
				writeErr(w, http.StatusServiceUnavailable,
					"面板还没有任何登录账号：在配置文件 server.adminUser / server.adminPassword 里填一组再重启，"+
						"首次启动会自动把它建成管理员")
				return
			}
			s.log().Error("核对登录凭据失败", "user", user, "err", err.Error())
			writeErr(w, http.StatusInternalServerError, "面板内部错误，详情看平台运行日志")
			return
		}
		if sess == nil {
			s.authRecordFailure(ip, now)
			s.denyUnauthorized(w)
			return
		}
		// 成功登录清零这个 IP 的失败计数：合法登录绝不能被算进失败，
		// 也不能让之前几次手抖把后面的正常请求挡掉。
		s.authRecordSuccess(ip)
		// 只读账号只能看。改自己的密码是唯一例外——否则只读账号只能找管理员重置。
		//
		// 成员（member）不在这里判：它的限制天生是"哪间房"，在这一层还看不到房间主语，
		// 判了也只能一刀切全拒（那等于把它降级成只读）。放行到 guardRooms 那一层，
		// 那里按路径把房间名认出来再判，既不用每个处理器各写一遍，
		// 也说得出"这间没指派 / 那间归别人"这两种完全不同的下一步。
		if sess.Role == store.RoleViewer && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.URL.Path != "/api/v1/session/password" {
			writeErr(w, http.StatusForbidden, "当前账号是只读，修改操作需要管理员")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, *sess)))
	})
}

// 登录失败限速：5 次失败锁 60 秒。计数按来源 IP，成功即清零。
const (
	authMaxFailures = 5
	authLockFor     = 60 * time.Second
)

type authAttempt struct {
	failures    int
	lockedUntil time.Time
	last        time.Time
}

// clientIP 只认 RemoteAddr，不认 X-Forwarded-For：那个头可以随便伪造，
// 拿它做限速等于给爆破者一个随时换 IP 的开关。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// authLocked 回答这个来源 IP 现在是不是在锁定期，以及还剩几秒。
func (s *Server) authLocked(ip string, now time.Time) (bool, int) {
	s.authLimitMu.Lock()
	defer s.authLimitMu.Unlock()
	a := s.authFailures[ip]
	if a == nil || a.lockedUntil.IsZero() {
		return false, 0
	}
	if !now.Before(a.lockedUntil) {
		delete(s.authFailures, ip) // 锁到期，重新数
		return false, 0
	}
	return true, int(a.lockedUntil.Sub(now).Seconds()) + 1
}

func (s *Server) authRecordFailure(ip string, now time.Time) {
	s.authLimitMu.Lock()
	defer s.authLimitMu.Unlock()
	if s.authFailures == nil {
		s.authFailures = map[string]*authAttempt{}
	}
	a := s.authFailures[ip]
	if a == nil {
		a = &authAttempt{}
		s.authFailures[ip] = a
	}
	a.failures++
	a.last = now
	if a.failures >= authMaxFailures {
		a.lockedUntil = now.Add(authLockFor)
		a.failures = 0
	}
	// 顺手收掉早就过期的其它 IP，这张表不该跟着探测流量无限长。
	for k, v := range s.authFailures {
		if k != ip && now.Sub(v.last) > authLockFor {
			delete(s.authFailures, k)
		}
	}
}

func (s *Server) authRecordSuccess(ip string) {
	s.authLimitMu.Lock()
	defer s.authLimitMu.Unlock()
	delete(s.authFailures, ip)
}

var errNoAccounts = errors.New("库里还没有任何账号")

type sessionKey struct{}

type session struct {
	ID       int64
	Username string
	Role     string
}

// sessionOf 只在 authenticate 放行之后调用；拿不到说明接线错了，返回空角色，
// 由各处理器自己的权限判断挡住。
func sessionOf(r *http.Request) session {
	s, _ := r.Context().Value(sessionKey{}).(session)
	return s
}

func (s *Server) denyUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="DST 面板", charset="UTF-8"`)
	writeErr(w, http.StatusUnauthorized, "管理员账号或密码不对")
}

func (s *Server) authCacheGet() *auth.Cache {
	s.authOnce.Do(func() { s.authCache = auth.NewCache(2 * time.Minute) })
	return s.authCache
}

// checkLogin 返回 nil, nil 表示凭据不对。只有"库里一个账号都没有"才回 errNoAccounts，
// 那个状态要给用户一句能动手的提示，而不是普通的 401。
func (s *Server) checkLogin(user, pass string) (*session, error) {
	if s.Store == nil {
		// 没有库的旧接线（测试或裁剪版）：退回配置文件单账号。
		if s.Auth.AdminPassword == "" {
			return nil, errNoAccounts
		}
		matched := subtle.ConstantTimeCompare([]byte(user), []byte(s.Auth.AdminUser)) == 1 &&
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.Auth.AdminPassword)) == 1
		if !matched {
			return nil, nil
		}
		return &session{Username: s.Auth.AdminUser, Role: store.RoleAdmin}, nil
	}

	acct, hash, err := s.Store.AccountByUsername(user)
	if errors.Is(err, store.ErrAccountNotFound) {
		// 用户不存在时也走一遍同等成本的核对，避免响应快慢泄露哪些用户名存在。
		auth.VerifyDummy(pass)
		n, cerr := s.Store.CountAccounts()
		if cerr != nil {
			return nil, cerr
		}
		if n == 0 {
			return nil, errNoAccounts
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	key := auth.Key(acct.Username, pass, hash)
	ok, hit := s.authCacheGet().Get(key)
	if !hit {
		ok = auth.Verify(hash, pass)
		s.authCacheGet().Put(key, ok)
		if ok {
			// 每个请求都写一次最后登录会把库写热；只在冷启动核对成功时记一笔。
			_ = s.Store.TouchAccountLogin(acct.ID, time.Now())
		}
	}
	if !ok {
		return nil, nil
	}
	return &session{ID: acct.ID, Username: acct.Username, Role: acct.Role}, nil
}

// ensureBootstrapAccount 只在库里一个账号都没有时，用配置文件那一组建首个管理员。
// 之后 yaml 里的账号密码就不再参与登录——否则页面上改了密码、重启后又被 yaml 盖回去，
// 用户永远说不清自己到底用的是哪一组。
func (s *Server) ensureBootstrapAccount() {
	if s.Store == nil || strings.TrimSpace(s.Auth.AdminUser) == "" || s.Auth.AdminPassword == "" {
		return
	}
	n, err := s.Store.CountAccounts()
	if err != nil {
		s.log().Error("读取账号数量失败", "err", err.Error())
		return
	}
	if n > 0 {
		return
	}
	hash, err := auth.Hash(s.Auth.AdminPassword)
	if err != nil {
		s.log().Error("生成初始管理员密码哈希失败", "err", err.Error())
		return
	}
	if _, err := s.Store.CreateAccount(strings.TrimSpace(s.Auth.AdminUser), hash, store.RoleAdmin); err != nil &&
		!errors.Is(err, store.ErrUsernameTaken) {
		s.log().Error("创建初始管理员失败", "err", err.Error())
		return
	}
	s.log().Info("已用配置文件里的凭据创建首个管理员账号", "user", s.Auth.AdminUser)
	if s.Auth.AdminPassword == config.DefaultAdminPassword {
		// 出厂默认凭据是公开信息，面板又可能直接开在公网端口上。界面上每页都在提醒，
		// 这一行是给只看得到 journalctl 的部署者的同一句话。
		s.log().Warn("管理员账号还在用出厂默认密码，登录后请在「账号管理」里改掉")
	}
}

// ---- 响应写法 ----

type reply struct {
	OK   bool   `json:"ok"`
	Data any    `json:"data,omitempty"`
	Err  string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, reply{OK: true, Data: data})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, reply{OK: false, Err: msg})
}

// fail 把内部错误写成响应，同时把原文记进运行日志。
// 用户看到的是中文结论，细节留在日志里，两边都不用猜。
func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, err error) {
	s.log().Error("接口失败", "path", r.URL.Path, "err", err.Error())
	writeErr(w, code, publicError(err))
}

// handleNoSuchAPI 接住"路径以 /api/ 开头但没有任何路由匹配"的请求。
//
// 为什么不能由界面那层的 index.html 兜底：浏览器里拼错的前端地址和脚本里拼错的
// 接口地址长得一样，返回一坨 200 的 HTML 会让调用方以为接口通了、只是数据为空。
func (s *Server) handleNoSuchAPI(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotFound, fmt.Sprintf(
		"没有这个接口：%s %s。接口的形状可以在 /api/v1/clusters 那批地址里对照，界面用的地址都由前端代码给出",
		r.Method, r.URL.Path))
}

// ---- 接口 ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"at":       time.Now().UTC().Format(time.RFC3339),
		"clusters": len(s.clusterNames()),
	})
}

func (s *Server) clusterNames() []string {
	names, err := s.Paths.ClusterNames()
	if err != nil {
		return nil
	}
	return names
}

type shardView struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Pid     int    `json:"pid"`
	// HelperOnline 回答"助手 MOD 有没有在回话"，和进程在不在是两件事：
	// 分片可能刚起来还没进世界，也可能进程活着但 MOD 被禁用了。
	HelperOnline bool   `json:"helperOnline"`
	HelperNote   string `json:"helperNote,omitempty"`
	// HelperNoteUrgent 区分这句说明是"真出事了"还是"这一刻只是读不到"。
	// 没人时引擎自己把世界冻住是设计如此，把它摆成红色，用户会去找一个不存在的故障；
	// 而登录被拒、世界生成卡在重试里才是必须处理的。界面按这一列上色。
	HelperNoteUrgent bool `json:"helperNoteUrgent"`
	// 启动阶段（starting/loading/ready/failed），新增字段：进程在跑不等于世界可进入。
	Phase       string `json:"phase,omitempty"`
	PhaseReason string `json:"phaseReason,omitempty"`
}

type clusterView struct {
	Name string `json:"name"`
	// RoomName 是 cluster.ini 里的 cluster_name（人在游戏和面板上认的那个名字），
	// 读不到时等于目录名 Name。界面上的标题用它，路径与接口参数仍用 Name。
	RoomName string `json:"roomName"`
	// 归属说法由后端拼（见 api/ownership.go 的 ownerViews）：成员拿不到账号列表，
	// 让前端按 id 找名字必然找出空字符串。
	OwnerLabel string `json:"ownerLabel"`
	// OwnedByMe 让界面上"这行归我管"的标记和"指派"按钮各自说得出理由，
	// 而不是让前端猜角色和房间名的组合。
	OwnedByMe bool `json:"ownedByMe"`
	// OwnerName 与 OwnerAccountID 是给「指派归属」弹窗用的：它要把当前选中那一项
	// 摆在下拉框里，也要在取消指派时确认派给了谁。
	// 成员的房间名单里根本不会出现别人的房间（visibleClusters 在响应里就裁掉了），
	// 所以这两个字段不构成额外的暴露。
	OwnerName      string `json:"ownerName"`
	OwnerAccountID int64  `json:"ownerAccountId"`
	// AssignedNote 是"由谁指派"那一句，弹窗里摆在当前归属下面；时刻单独走 assignedAt，
	// 由前端按浏览器时区格式化（理由见 api/ownership.go 那两个字段的注释）。
	AssignedNote string      `json:"assignedNote"`
	AssignedAt   string      `json:"assignedAt"`
	Shards       []shardView `json:"shards"`
}

// helperState 回答"助手 MOD 现在算不算在回话"，并给出界面能直接显示的一句话。
// 三种离线原因必须分开：文件在但过期（MOD 卡死或分片已退出）、从没写过（MOD 没加载），
// 以及读失败（权限/坏 JSON）——用户接下来要做的事完全不一样。
func helperState(st *modbridge.Status, err error) (bool, string) {
	switch {
	case err == nil && !st.Stale:
		return true, ""
	case err == nil:
		return false, fmt.Sprintf("数据已过期，%s没有新数据", humanAgo(st.Age))
	case errors.Is(err, os.ErrNotExist):
		return false, "还没收到游戏数据（助手没加载，或这个世界还没启动过）"
	default:
		return false, publicError(err)
	}
}

// humanAgo 把"距今多久"写成中文。
// 不用 time.Duration.String()：它给的是 25h1m11s 这种 Go 字面量，
// 摆在总览页上就像程序内部串味到了界面上（任务中心那句超时文案踩过同一个坑）。
func humanAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) - h*60; m > 0 {
			return fmt.Sprintf("%d 小时 %d 分钟", h, m)
		}
		return fmt.Sprintf("%d 小时", h)
	default:
		day := int(d.Hours()) / 24
		if h := int(d.Hours()) - day*24; h > 0 {
			return fmt.Sprintf("%d 天 %d 小时", day, h)
		}
		return fmt.Sprintf("%d 天", day)
	}
}

// publicError 把带 Go 内部措辞的错误换成能直接进界面的中文。
//
// 为什么需要这一层：os 和驱动的错误都是英文的（"file does not exist"、
// "executable file not found in %PATH%"），界面上出现半截英文等于让用户自己去翻源码。
// 原文一律进运行日志（s.fail 里记那条），界面只留结论。
func publicError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, os.ErrNotExist):
		return "文件不存在，可能是这个世界还没生成过存档"
	case errors.Is(err, os.ErrPermission):
		return "面板没有权限读这个文件，检查运行用户和文件属主"
	}
	// ErrHelperOffline / ErrCommandPending 不在这里短路：这两条错误的原文本来就是
	// 中文、而且后半句正是面板拼出来的诊断（缺哪个文件、有没有启用、快照停更多久）。
	// 之前在这里写死一句"助手 MOD 没有响应"，等于把最有用的那半句扔掉，
	// 用户只剩"然后呢？"，界面也就没办法回答。
	// 内部错误普遍是"英文原因：中文补充"的形式（status.go 就是这么包装 os.ErrNotExist 的），
	// 冒号前是纯 ASCII 才认为它是技术原因，整段丢掉；中文开头的要保留，否则
	// "助手 MOD 没有响应：……"会被砍成后半句，结论反倒没了。
	msg := err.Error()
	const sep = "："
	if i := strings.Index(msg, sep); i >= 0 {
		if prefix := msg[:i]; prefix != "" && isASCII(prefix) {
			return strings.TrimSpace(msg[i+len(sep):])
		}
	}
	return msg
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// emptyIfNil 给那些"由 append 攒出来"的列表字段收尾用。
//
// 为什么要在这一层管：Go 的 nil 切片进 JSON 是 null，而前端模板普遍直接读 xxx.length
// （免构建 Vue 没有类型挡这一刀）。云机上两个世界都暂停时，玩家管理页就是这么整页崩的。
// 一条不变式测试扫所有 GET 接口，见 TestNoNullInAnyGetReply。
func emptyIfNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}

func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	names, err := s.Paths.ClusterNames()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// 归属筛在这一层，而不是让前端拿到全部房间再藏：
	// 前端藏起来的名单在网络响应里仍然是全的，那不算"看不到"。
	names = s.visibleClusters(r, names)
	owners, err := s.ownerViews(names)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	me := sessionOf(r)
	// screen 会话一次问完：每个分片各起一次 `screen -ls` 的话，
	// 两个集群八个分片就是八次进程创建，这页是总览页，会被反复刷新。
	// 问不到时整页 503 而不是列出"全都未运行"：进程状态说错比不说更糟，
	// 用户会以为服务器没跑着，然后重复点启动。
	sessions, err := s.Procs.Sessions(r.Context())
	if err != nil {
		s.log().Error("读 screen 会话列表失败", "err", err.Error())
		writeErr(w, http.StatusServiceUnavailable,
			"问不到世界进程状态：面板需要在服务端同机运行，且 PATH 里要有 screen 命令")
		return
	}
	out := make([]clusterView, 0, len(names))
	for _, name := range names {
		shards := s.Paths.ShardNames(name)
		view := clusterView{Name: name, RoomName: name, Shards: make([]shardView, 0, len(shards))}
		ov, _ := ownerFromViews(owners, name)
		view.OwnerLabel = ov.OwnerLabel
		view.OwnedByMe = ov.AccountID != 0 && ov.AccountID == me.ID
		view.OwnerName = ov.AccountName
		view.OwnerAccountID = ov.AccountID
		view.AssignedNote = ov.AssignedNote
		view.AssignedAt = ov.AssignedAt
		// 房间名是 cluster.ini 里那个人填的，和目录名常常不一样。总览页要显示它，
		// 否则用户在游戏里看到的是"周末联机房"、在面板上看到的是"Cluster_2"，
		// 两边对不上号就只能挨个点进去认。读不到退回目录名（这一页不能因为
		// 一个文件读失败就让整个集群消失）。
		if c, err := dst.OpenCluster(s.Paths, name); err == nil {
			if dn := c.Settings().ClusterName; dn != "" {
				view.RoomName = dn
			}
		}
		for _, shard := range shards {
			sv := shardView{Name: shard, Pid: sessions[s.Procs.SessionName(name, shard)]}
			sv.Running = sv.Pid > 0
			// 助手在不在和进程在不在是两件事：分片可能刚起来还没进世界，
			// 也可能进程活着但 MOD 被引擎禁用了。总览页必须分开显示这两种。
			// 判断的整套分支在 helperReadiness 里，和房间总览页共用。
			st, stErr := s.Bridge.Status(name, shard)
			verdict := s.helperReadiness(name, shard, sv.Running, st, stErr)
			sv.HelperOnline, sv.HelperNote, sv.HelperNoteUrgent = verdict.Online, verdict.Note, verdict.Urgent
			sv.Phase, sv.PhaseReason = s.Procs.Phase(name, shard)
			view.Shards = append(view.Shards, sv)
		}
		out = append(out, view)
	}
	writeOK(w, out)
}

func (s *Server) handleShardStatus(w http.ResponseWriter, r *http.Request) {
	cluster, shard := r.PathValue("name"), r.PathValue("shard")
	st, err := s.Bridge.Status(cluster, shard)
	if err != nil {
		// 503 而不是 500：分片没进世界、MOD 没加载是运行中的常态，
		// 前端要能把它显示成"数据还没上来"而不是"接口坏了"。
		if errors.Is(err, os.ErrNotExist) {
			s.log().Warn("世界还没有状态数据", "cluster", cluster, "shard", shard, "err", err.Error())
			writeErr(w, http.StatusServiceUnavailable,
				"这个世界还没有状态数据：助手 MOD 未加载，或者它还没进过一次世界")
			return
		}
		if errors.Is(err, modbridge.ErrHelperOffline) {
			// 这条错误自带面板的诊断（文件齐不齐、有没有启用），原样给出比重新编一句有用。
			s.log().Warn("助手 MOD 离线", "cluster", cluster, "shard", shard, "err", err.Error())
			writeErr(w, http.StatusServiceUnavailable, publicError(err))
			return
		}
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	v := statusViewOf(st)
	v.BootPhase, v.BootPhaseReason = s.Procs.Phase(cluster, shard)
	writeOK(w, v)
}

type playerView struct {
	Name   string `json:"name"`
	Prefab string `json:"prefab"`
	UserID string `json:"userid"`
	// 数值字段保持指针：nil 是"MOD 没采到这个组件"，
	// 界面要显示"未知"，不能显示 0——0 会被读成"快饿死了"。
	Health    *float64 `json:"health,omitempty"`
	MaxHealth *float64 `json:"maxHealth,omitempty"`
	Hunger    *float64 `json:"hunger,omitempty"`
	MaxHunger *float64 `json:"maxHunger,omitempty"`
	Sanity    *float64 `json:"sanity,omitempty"`
	MaxSanity *float64 `json:"maxSanity,omitempty"`
	X         *float64 `json:"x,omitempty"`
	Z         *float64 `json:"z,omitempty"`
	Items     *int     `json:"items,omitempty"`
	Dead      bool     `json:"dead"`
}

type statusView struct {
	Shard      string  `json:"shard"`
	ServerName string  `json:"serverName"`
	GameID     string  `json:"gameId"`
	Day        float64 `json:"day"`
	Cycles     float64 `json:"cycles"`
	// 季/时段各给一份原值和一份中文，原值给判断逻辑，中文给渲染。
	Season      string `json:"season"`
	SeasonLabel string `json:"seasonLabel"`
	// 这一季第几天、还剩几天。指针 + omitempty：季节不轮换（无尽模式、固定季节）
	// 或这一版引擎没给读数时，这两个键干脆不出现在响应里，界面也就什么都不说，
	// 而不是摆一个"第 1 天"或"还剩 0 天"。
	DayInSeason      *float64 `json:"dayInSeason,omitempty"`
	DaysLeftInSeason *float64 `json:"daysLeftInSeason,omitempty"`
	Phase            string   `json:"phase"`
	PhaseLabel       string   `json:"phaseLabel"`
	Temperature      float64  `json:"temperature"`
	Wetness          float64  `json:"wetness"`

	Online     int          `json:"online"`
	MaxPlayers int          `json:"maxPlayers"`
	Players    []playerView `json:"players"`

	UpdatedAt  string  `json:"updatedAt"`
	AgeSeconds float64 `json:"ageSeconds"`
	Stale      bool    `json:"stale"`
	// HelperNote 非空表示这份数据的可信度有问题，界面要把它显示在数据旁边。
	HelperNote string `json:"helperNote,omitempty"`
	// CollectError 是 MOD 采集时抛的异常原文，非空时上面的数字都不完整。
	// 单独一条字段是为了让界面能把它显示成"数据异常"，同时把 Lua 原文留在旁边可查。
	CollectError string `json:"collectError,omitempty"`
	CollectNote  string `json:"collectNote,omitempty"`
	// BootPhase / BootPhaseReason 是分片启动阶段。名字带 Boot 是为了不和上面那个
	// 游戏内时段 Phase（day/dusk/night）撞名——既有字段一个都不动。
	BootPhase       string `json:"bootPhase,omitempty"`
	BootPhaseReason string `json:"bootPhaseReason,omitempty"`
}

func statusViewOf(st *modbridge.Status) statusView {
	v := statusView{
		Shard: st.Shard, ServerName: st.ServerName, GameID: st.GameID,
		Day: st.World.DayNumber(), Cycles: st.World.Cycles,
		Season: st.World.Season, SeasonLabel: st.World.SeasonLabel(),
		Phase: st.World.Phase, PhaseLabel: st.World.PhaseLabel(),
		Temperature: st.World.Temperature, Wetness: st.World.Wetness,
		Online: st.PlayerCount, MaxPlayers: st.MaxPlayers,
		Stale:        st.Stale,
		CollectError: st.Error,
	}
	if d, ok := st.World.DayInSeason(); ok {
		v.DayInSeason = &d
	}
	if left, ok := st.World.DaysLeftInSeason(); ok {
		v.DaysLeftInSeason = &left
	}
	if st.Error != "" {
		v.CollectNote = "助手在游戏里采集时出了错，下面的数字可能不完整"
	}
	if !st.UpdatedAt.IsZero() {
		v.UpdatedAt = st.UpdatedAt.UTC().Format(time.RFC3339)
		v.AgeSeconds = st.Age.Seconds()
	}
	v.Players = make([]playerView, 0, len(st.Players))
	for _, p := range st.Players {
		v.Players = append(v.Players, playerView{
			Name: p.Name, Prefab: p.Prefab, UserID: p.UserID,
			Health: p.Health, MaxHealth: p.MaxHealth,
			Hunger: p.Hunger, MaxHunger: p.MaxHunger,
			Sanity: p.Sanity, MaxSanity: p.MaxSanity,
			X: p.X, Z: p.Z, Items: p.Items, Dead: p.IsDead,
		})
	}
	return v
}

type taskView struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	StatusLab string `json:"statusLabel"`
	Progress  int    `json:"progress"`
	Message   string `json:"message"`
	// Detail 是进度条背后那组可对质的数（已下/总量/速度/预计剩余/阶段）。
	// 给的是解好的一方对象而不是库里那串 JSON 原文：前端拿到字符串就得自己解析一遍，
	// 而"已下 1.24 GB"这种句子就该由面板算好，两处各算一遍必然对不齐。
	// 没有字节可言的任务这一项整个缺席（omitempty），界面就不画那一块。
	Detail    *store.TaskDetail `json:"detail,omitempty"`
	Attempts  int               `json:"attempts"`
	CreatedAt string            `json:"createdAt"`
	StartedAt string            `json:"startedAt,omitempty"`
	FinishedA string            `json:"finishedAt,omitempty"`
	// Note 是面板对这次操作补的一句话（比如"已取消排队中的任务"），
	// 和 Message 区分开：Message 是 worker 写的执行进度，会随任务变化。
	Note string `json:"note,omitempty"`
}

// taskViewOf 把库里的任务变成界面要的形态：状态码 + 中文标签一起给，
// 时间统一成 RFC3339 的 UTC 文本，前端不需要再猜时区。
func taskViewOf(t store.Task, note string) taskView {
	v := taskView{
		ID: t.ID, Kind: t.Kind, Title: t.Title,
		Status: t.Status, StatusLab: store.LabelTaskStatus(t.Status),
		Progress: t.Progress, Message: t.Message, Attempts: t.Attempts,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		Note:      note,
	}
	if t.StartedAt != nil {
		v.StartedAt = t.StartedAt.UTC().Format(time.RFC3339)
	}
	if t.FinishedAt != nil {
		v.FinishedA = t.FinishedAt.UTC().Format(time.RFC3339)
	}
	if t.Detail != "" {
		// 解不动就只丢掉这一组数：进度百分比和那句话是界面本来就要显示的东西，
		// 不能因为明细坏了让整条任务读不出来。
		var d store.TaskDetail
		if err := json.Unmarshal([]byte(t.Detail), &d); err == nil {
			v.Detail = &d
		} else {
			slog.Warn("任务进度明细读不出来，界面这次只显示百分比", "id", t.ID, "err", err.Error())
		}
	}
	return v
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("limit 得是整数，收到的是 %q", raw))
			return
		}
		limit = n
	}
	tasks, err := s.Store.ListTasks(limit)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	out := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		// 成员只看自己名下房间的任务，加上他做得到的那几类。
		// 判据在 payload 的房间字段里（那串 JSON 由后端拼），
		// 认不出房间的任务就是机器级任务，那一类轮不到他做。
		if !s.taskVisible(r, t.Payload) {
			continue
		}
		out = append(out, taskViewOf(t, ""))
	}
	writeOK(w, out)
}
