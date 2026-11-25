-- 面板自己的状态库（dstpanel.db）。
--
-- 只存"文件里放不下的东西"：任务队列、审计日志、在线人数趋势、可在后台改的设置。
-- 集群配置、世界设置、模组覆盖这些都仍然直接写 Klei 的原文件——
-- 数据库里存一份副本只会带来"两边不一致时该信谁"的问题。
--
-- 时间统一用 UTC 毫秒整数（created_at 等），不存字符串：
-- SQLite 的日期函数在不同编译选项下行为不一致，而面板只做范围查询和排序，整数最省心。

CREATE TABLE IF NOT EXISTS tasks (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	-- kind 是执行器标识，例如 mod_install / server_update / backup。
	kind        TEXT    NOT NULL,
	-- title 直接给前端显示的中文句子，不让前端去拼码值。
	title       TEXT    NOT NULL,
	payload     TEXT    NOT NULL DEFAULT '{}',
	status      TEXT    NOT NULL DEFAULT 'queued',
	-- progress 是 0~100 的粗粒度进度；拉不动精度的任务（下载）就按字节数换算。
	progress    INTEGER NOT NULL DEFAULT 0,
	-- message 放最近一次失败的中文原因，成功时放结果摘要。
	message     TEXT    NOT NULL DEFAULT '',
	-- detail 是进度条背后那组可核对的数（已下字节、整包字节、此刻速度、预计剩余、阶段名），
	-- JSON 文本。为什么单开一列而不是把那串数字写在 message 里：界面要的是能分列对齐的数，
	-- 整句中文只能整句刷，用户扫一眼读不出"下了多少、还剩多久"。没有字节进度的任务留空。
	detail      TEXT    NOT NULL DEFAULT '',
	attempts    INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	started_at  INTEGER,
	finished_at INTEGER
);

-- 队列表的查询形态只有两种：按状态取最早的一条（worker 领取）、按状态倒序列举（前端任务中心）。
-- 有索引和没索引在几千条以后差别就很明显了。
CREATE INDEX IF NOT EXISTS idx_tasks_status_id ON tasks (status, id);

CREATE TABLE IF NOT EXISTS audit_log (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	at     INTEGER NOT NULL,
	-- actor 是登录用户名；系统自己触发的写 "system"。
	actor  TEXT    NOT NULL,
	-- action 用 "资源.动作" 形式，例如 mod.enable、shard.stop，方便按前缀筛。
	action TEXT    NOT NULL,
	target TEXT    NOT NULL DEFAULT '',
	detail TEXT    NOT NULL DEFAULT '',
	ok     INTEGER NOT NULL DEFAULT 1,
	err    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log (at);

-- 在线人数/世界天数趋势。助手 MOD 每秒都写快照，但库里按采样间隔存，
-- 否则一个长期运行的服务器光这张表就能吃掉几十 MB。
CREATE TABLE IF NOT EXISTS player_snapshots (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	at       INTEGER NOT NULL,
	cluster  TEXT    NOT NULL,
	shard    TEXT    NOT NULL,
	day      INTEGER NOT NULL DEFAULT 0,
	season   TEXT    NOT NULL DEFAULT '',
	online   INTEGER NOT NULL DEFAULT 0,
	-- detail 存玩家名列表的 JSON，趋势图不需要它，只在"看某一天谁在线"时用。
	detail   TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_snapshots_cluster_at ON player_snapshots (cluster, at);

-- 后台可改的运行参数（例如快照采样间隔、保留天数）。
-- yaml 里放的是安装期默认值，这里放的是"管理员在页面上改过的值"，改一次即生效不用重启。
CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);

-- 面板登录账号。和游戏内的管理员/白名单是两套东西：这里管"谁能打开面板"，
-- 游戏身份在 Klei 的 adminlist.txt / whitelist.txt 里，两者不自动绑定。
--
-- 为什么密码只存哈希：面板可能直接开在公网端口上，库文件被拖走时明文口令
-- 往往和用户别处的密码复用。哈希格式见 internal/auth。
-- 第一个管理员由启动时的 server.adminUser / server.adminPassword 自动建成。
CREATE TABLE IF NOT EXISTS accounts (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT    NOT NULL COLLATE NOCASE UNIQUE,
	password_hash TEXT    NOT NULL,
	-- role 三种：admin（管这台机器）、member（只管 cluster_owners 里派给它的房间）、
	-- viewer（只读）。中文说法与"这个角色能做什么"都在 store 的 roleLabels / roleHints 里。
	role          TEXT    NOT NULL DEFAULT 'viewer',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	-- 最后登录只在这个账号"冷启动"核对成功时更新，不是每个请求都写一次。
	last_login_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_accounts_role ON accounts (role);

-- 房间归属：哪个账号管磁盘上的哪个房间目录（见 internal/store/ownership.go）。
--
-- 键用房间名而不是账号外键加级联：房间是目录名，删掉目录不需要动这张表，
-- 顶多留下一行没人认领的归属，列房间时按"磁盘上没有这个房间"跳过就行。
-- 反过来删账号时要把指向它的行清掉，那一步写在 DeleteAccount 里，
-- 因为界面要在删之前先告诉用户"这个账号名下还有两个房间，删完它们会变回未指派"。
CREATE TABLE IF NOT EXISTS cluster_owners (
	cluster     TEXT    PRIMARY KEY COLLATE NOCASE,
	account_id  INTEGER NOT NULL,
	assigned_at INTEGER NOT NULL,
	-- 谁指派的只存用户名：审计里能查到细节，这里只给界面上那句"由 admin 指派"用。
	assigned_by TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_cluster_owners_account ON cluster_owners (account_id);

-- 见过的玩家：帮面把"只有一句 KU_xxx 的名单"变成"谁是谁"。
--
-- 为什么每个集群各存一份：同一个 Klei 账号能在不同集群里玩，但玩家管理页
-- 是按房间展示的；把两个房间的名单混成一份，用户会以为 A 房的黑名单管到了 B 房。
--
-- 名字取最后一次看到的：改名是玩家自己的事，但面板没有其它地方能拿到旧名字，
-- 只留最后一次比留一串"哪个才对"的历史更有用。last_seen_at 用最后一份可信快照，
-- 助手掉线时不会更新，所以它也是"这份名字有多可信"的旁证。
CREATE TABLE IF NOT EXISTS players_seen (
	cluster        TEXT    NOT NULL,
	userid         TEXT    NOT NULL,
	name           TEXT    NOT NULL DEFAULT '',
	prefab         TEXT    NOT NULL DEFAULT '',
	last_shard     TEXT    NOT NULL DEFAULT '',
	first_seen_at  INTEGER NOT NULL,
	last_seen_at   INTEGER NOT NULL,
	PRIMARY KEY (cluster, userid)
);

CREATE INDEX IF NOT EXISTS idx_players_seen_last ON players_seen (cluster, last_seen_at DESC);

-- 玩家行为落库：总览那张「玩家在线时间图」要的是"这个人几点进来、几点出去、中间死了几回"。
-- 引擎只在日志里打这几行，而日志每轮启动会被截断重写（见 gamelog/reader.go:79），
-- 不落库就永远只能回看每份文件最近 500 行，也就是半小时。
--
-- 为什么"绝对时刻"和"本轮启动号"两样都要存：引擎打的时钟是从本轮启动算起的相对值
-- （[00:46:05] 是"开服后 46 分 05 秒"），同一个 00:46:05 在上一轮和这一轮是两个不同时刻，
-- 只存时钟就会把两轮里的事并成一条。round 存本轮启动的 UTC 毫秒（读不到 Current time 行时为 0），
-- 它同时是去重键的一部分——换一轮之后同一个人同一种行为可以再来一次。
--
-- 去重键为什么是"同一轮 + 同一秒 + 同一种行为 + 同一个人"：面板重启时归档器会重读一段窗口，
-- 而进服公告在服务端日志和聊天文件里各有一份抄本。一秒内同一个人进两次不可能，
-- 所以这四样相同就当同一下，重复写入直接忽略（见 internal/monitor/events.go）。
--
-- 名字和账户编号可能只有其中一个：进服公告在聊天文件里只带名字，账户编号要从服务端日志
-- 那几条验证行才凑得到。两个都空的行为行不入库——它说不上是谁，画不出属于任何人的时间线。
CREATE TABLE IF NOT EXISTS player_events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	at      INTEGER NOT NULL,
	round   INTEGER NOT NULL DEFAULT 0,
	cluster TEXT    NOT NULL,
	shard   TEXT    NOT NULL,
	userid  TEXT    NOT NULL DEFAULT '',
	name    TEXT    NOT NULL DEFAULT '',
	-- event 是引擎行为的码值（player_join 这种），界面上只出现中文短名，
	-- 说法由 gamelog 那份行为目录给（见 store 之外的那一层），这里不另写一张表。
	event   TEXT    NOT NULL,
	-- source 是这行出自哪份文件（server / chat），留着便于对回日志原文。
	source  TEXT    NOT NULL DEFAULT '',
	clock   TEXT    NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_player_events_key
	ON player_events (cluster, shard, round, at, event, userid, name);
CREATE INDEX IF NOT EXISTS idx_player_events_cluster_at ON player_events (cluster, at);

-- 工坊模组元数据的本地缓存："见过即入库"。
--
-- 为什么要这张表：Web API 只有在装了 key、且网络出口能到 api.steampowered.com 的时候才可用，
-- 而"这个名字对应哪个工坊 ID"是用户唯一没法自己拼出来的信息。缓存过一条就永久能用，
-- 断网/没 key/被限流时检索依然有结果，只是要在界面上说清结果来自缓存。
--
-- 主键是工坊 ID 本身而不是自增 id：这一行的存在理由就是"ID → 名字"的映射，
-- 让 ID 当主键，重复检索天然变成更新，不会出现同名多条。
-- tags 存 JSON 数组：工坊标签是上传者随便写的字符串，逗号分隔会拆错。
CREATE TABLE IF NOT EXISTS mods_seen (
	workshop_id     TEXT    PRIMARY KEY,
	name            TEXT    NOT NULL DEFAULT '',
	description     TEXT    NOT NULL DEFAULT '',
	version         TEXT    NOT NULL DEFAULT '',
	-- workshop_version 单独一列而不是复用 version：扫磁盘时读到的版本来自这个目录里的
	-- modinfo.lua，也就是"这台机器上现在装着的那一份"；version 会被它覆盖掉，
	-- 于是"工坊那边是几点"和"我这边是几点"就只剩一个数，"工坊更新了没"永远问不出来。
	-- 这一列只由在线来源写（见 modsseen.go 的 recordSeen），磁盘扫描不许动它。
	workshop_version TEXT   NOT NULL DEFAULT '',
	tags            TEXT    NOT NULL DEFAULT '[]',
	-- 下面这几个"只增不减"的数（订阅、工坊最后更新、文件大小、收藏、投票人数）：
	-- 小值几乎总是意味着这次信息来源知道得更少（比如磁盘扫描根本没有订阅数）。
	subscriptions   INTEGER NOT NULL DEFAULT 0,
	mod_updated_at  INTEGER NOT NULL DEFAULT 0,
	file_size       INTEGER NOT NULL DEFAULT 0,
	preview_url     TEXT    NOT NULL DEFAULT '',
	-- favorited 是收藏（工坊卡片上那颗星对应的数），和订阅数是两个量级不同的数：
	-- 实测 Epic Healthbar 订阅 372 万、收藏 11.8 万。同向只增。
	favorited       INTEGER NOT NULL DEFAULT 0,
	-- vote_score 是工坊好评率（0~1），votes_total 是投过票的人数。
	-- score 不按"只增不减"合并：它是一次读数，模组后来掉分是常事，留历史最高分就是在说谎。
	-- 所以规则是"这次真读到了才覆盖"，见 modsseen.go 的 recordSeen。
	vote_score      REAL    NOT NULL DEFAULT 0,
	votes_total     INTEGER NOT NULL DEFAULT 0,
	-- 三个布尔：宁可多提醒一次"客户端也要订阅"，所以按"见过就置真"合并。
	has_children    INTEGER NOT NULL DEFAULT 0,
	client_required INTEGER NOT NULL DEFAULT 0,
	server_only     INTEGER NOT NULL DEFAULT 0,
	-- origin：workshop（在线检索/详情拿到的）或 disk（扫本地磁盘得到的）。
	-- 只有 workshop 会覆盖它，因为它知道的东西更多。
	origin          TEXT    NOT NULL DEFAULT 'disk',
	first_seen_at   INTEGER NOT NULL,
	last_seen_at    INTEGER NOT NULL
);

-- 检索结果按"热度"排，这是用户真正想要的顺序（前排就是大家都在用的）。
CREATE INDEX IF NOT EXISTS idx_mods_seen_subs ON mods_seen (subscriptions DESC);
