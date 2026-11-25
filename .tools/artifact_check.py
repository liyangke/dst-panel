import subprocess
import sys

# 目录名是 go1.23.1.windows-amd64，不是 go1.23.1/windows-amd64：写错时 Python 只会
# 抛 FileNotFoundError，看起来像"nm 用不了"，其实是路径不存在。
GO = r"C:/coding/go/sdk/go1.23.1.windows-amd64/go/bin/go.exe"
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# 这一轮新加的代码：链接器只保留"从 main 可达"的东西，
# 所以这些串和符号必须在成品里出现，否则就是接口没接上。
MARKERS = [
    "dedicated_server_mods_setup.lua",
    'ServerModSetup("',
    "认不出模组根目录",
    "不是普通文件",
    "已经存在，要先删掉它再上传",
    "configuration_options",
    "export SteamAppId=",
    # 生命周期接口：这几串只有 HTTP 层用到，缺了就说明"开服"还是没接上。
    "启动前置检查未通过",
    "不在白名单里",
    "会话已强关",
    # 异步任务通道：kind 表在 worker 包里，只有 main 真起 worker 才不会被裁掉。
    "下载/更新服务端本体",
    "服务端本体已",
    # 整包字节进度：正则、以及那句"已下 X / Y，实测 Z/s，约还需 T"的碎片。
    # 少一条就是 steamcmd 的输出解析没接进成品，界面上只剩一个不动的条子。
    "progress:\\s*([0-9]+",
    "下载服务端本体文件",
    "整包大小 steamcmd 还没报出来",
    "实测",
    "约还需",
    "还有世界在运行",
    "还没填，填好后重启面板",
    "面板不做半路掐断",
    # 工坊检索：接口路径、降级说明、以及建表语句里的缓存表名。
    # 少一条就说明 steam 客户端、缓存表或 HTTP 路由某一段根本没被接进成品。
    "/IPublishedFileService/QueryFiles/v1/",
    "all_clients_require_mod",
    "没配 steamApi.webApiKey",
    "在线检索没成功",
    "已订阅，等重启时下载",
    "这台机器的模组目录时见过",
    "CREATE TABLE IF NOT EXISTS mods_seen",
    # 开服引导（新建集群 / 令牌 / 世界设定）。这一整块的教训是"函数没被路由引用就会被
    # 链接器裁掉，单测全绿也开不了服"，所以路由串、文件名、以及文案各查一遍。
    "POST /api/v1/clusters",
    "GET /api/v1/world-schema",
    "PUT /api/v1/clusters/{name}/token",
    "/shards/{shard}/world",
    # 世界文件走 worldgenoverride.lua 的 preset 通道（2026-10-31 云机实测定的）：
    # 面板只写 override_enabled/preset/overrides 三样，leveldataoverride.lua 不再写、
    # 只在检测到残留时提示。所以两个文件名都要在成品里——一个代表写入，一个代表提示。
    "worldgenoverride.lua",
    "leveldataoverride.lua",
    "override_enabled",
    "SURVIVAL_TOGETHER_CLASSIC",
    "TERRARIA_CAVE",
    "world_setting.json",
    "还没有世界规则文件",
    "下一次世界生成",
    "没有重生世界",
    "手工放置的 leveldataoverride.lua",
    # 启动判定：会话在跑但服务端拒绝启动的那条路，指纹和结论都得在成品里。
    "!!!! Your Server Will Not Start !!!!",
    "Level data override is invalid!",
    "服务端拒绝启动",
    # 存档根目录提示：引擎忽略 -savedir 这件事只有这一处会外露，
    # 少掉它等于"kleiRoot 指错了也一路报成功"。
    "kleiRoot 不像服务端会读的存档根目录",
    # 云机首启（760944）换来的前提，各钉一个字节标记：
    #   助手 MOD 的 api_version 必须等于引擎的 MOD_API_VERSION，嵌错就是加载失败；
    #   世界生成失败时进程不退出，这条指纹要能报出"进程在但没进世界"。
    "api_version = 10",
    "Worldgen had an error",
    "没能进入存档",
    # 云机 2026-10-31 的两轮实测换来的四条，钉在启动判定上：
    #   手写 leveldataoverride.lua 要是一整份完整世界定义（GAME USE 文件，面板补不全）；
    #   设-piece 字段缺失炸在 random_set_pieces 的取长度上；
    #   task_set 没有地形默认值兜底，空 overrides 直接 assert；
    #   必需物件造不出来时引擎不退出而是无限推倒重来；
    #   预设名引擎查不到时不崩，只是不用它——这句要能报出"选的预设没生效"。
    "Must provide complete level data to worldgen.",
    "attempt to get length of field 'random_set_pieces'",
    "Must specify the task set for a level!",
    "PANIC: missing required prefab",
    "反复推倒重来",
    "Worldgenoverride specified a nonexistent preset",
    # 2026-10-31 云机实测的三条通道事实：MOD 写的文件带 "KLEI     1 " 头（读之前要剥）、
    # 引擎不给 SetPersistentString 建子目录（通道文件平铺）、无人暂停时 Lua 全冻结
    #（总览页要说清"这不是掉线"）。这三条各自代表一处改动，漏一条就是接口没接上。
    "dstpanel_status.json",
    "dstpanel_queue.txt",
    "Sim paused",
    "无人时暂停",
    # 总览页在"进程在跑、助手不回话"时要引用引擎日志里那句原因，
    # 否则界面只能显示"助手 MOD 还没写入过状态"，用户不知道下一步做什么。
    "服务端日志里有",
    # 令牌校验不再猜字符集：拒绝的话只可能是富文本改写过的字符。
    "疑似复制时被富文本改写",
    # 2026-10-31 云机实测：引擎每次存世界（自动存档、c_save()、停服）都按当前存档重写
    # worldgenoverride.lua——开头加 SetPersistentString 的头，preset 换成
    # worldgen_preset + settings_preset（mainfunctions.lua 的 UpdateWorldGenOverride；
    # 面板下发 c_save() 后 2 秒，Master 那份从 5321 字节变成 553 字节）。
    # 所以这三样必须都在成品里：剥头的正则、引擎那两种预设拼法、界面那句解释。
    # 剥头的正则、引擎那两种预设拼法、以及界面那句"只剩一项不是面板删的"。
    # 用字符串标记而不是符号名：StripKleiHeader 只有几行，链接器会把它内联进
    # readLines/readJSON/ParseLuaTable，查符号会得到一个假的"这段代码没进成品"
    #（和 store.recordSeen、DefaultPreset 同一课）。
    r"^KLEI[ \t]+\d+[ \t]+",
    "worldgen_preset",
    "settings_preset",
    "是世界存档时",
    # 2026-10-31 读引擎源码定下来的两条模组侧结论，代码必须真的在成品里：
    #   1) mod_dependencies 对不上时是 DisableBecauseBad（静默禁用，服务端不报错不拒绝启动），
    #      而引擎从不比较两个分片加载了哪些模组——跨分片一致性只能面板自己算；
    #   2) client_mods_disabled 是 modoverrides.lua 的顶层键，不是模组条目，单独读。
    # 这三段任何一个没接进路由，界面就只会让用户看到"模组开了却没效果"。
    "GET /api/v1/clusters/{name}/mods/consistency",
    "mod_dependencies",
    "client_mods_disabled",
    "静默禁用",
    "启用状态不一致",
    # modinfo.lua 是"一串裸赋值"，不是 return 一张表（引擎 kleiloadlua 后读 env.name）。
    # 这条没接进模组列表，界面上连自家助手模组都是空白名字，依赖/订阅标记也全是空的。
    # 2026-11-03 换文案：读得通但名字是算出来的那一种，现在说的是"面板不执行 Lua"，
    # 不再是"没有 name 字段"——那是 #80 之前的旧解释，界面上已经没人这么写了。
    "面板不执行 Lua，读不出模组自己写的名字",
    "没有配对的",
    # #85 的一次点击：订阅清单 + 这个房间每个世界的开关，后端一次写完。
    # 路由串、界面按钮文案、以及"清单里有条目但磁盘上没目录"那行靠缓存认名字的字段，
    # 少任一条界面就退回"三步各自点一遍"，或者残留行只剩一串裸编号。
    "POST /api/v1/mods/subscribe-and-open",
    "订阅并开启",
    "seenName",
    # #86：窄屏页签条会把选中那一枚滚进视野。这是 app.js 里的一个具名函数，
    # 没有新界面文案可当标记，函数名被 minify 的风险为零（本项目不压缩资源），
    # 所以用它证明"这一份前端确实带了那条滚动的观察器"。
    "revealActiveTab",
    # 日志这一路（游戏日志 + 平台审计）：路由串、SSE 头、脱敏标记、以及分类器的三条说明文字。
    # 少任一条就说明"读日志"只写在包里、没被路由引用——上一轮就是靠这类标记才发现的。
    "GET /api/v1/clusters/{name}/shards/{shard}/log/stream",
    # 玩家事件表（日志页的第一个看法）：两条路由、界面那句合抄本说明、
    # 引擎原文里的三种形状，以及三条新事件码。
    # 少任一条都是"聚合只写在包里、没被路由引用"或者"规则表被改掉了句子形状"，
    # 而界面上看着只是表里少几行，很难看出是没接线还是真没人来过。
    "GET /api/v1/clusters/{name}/shards/{shard}/events",
    "GET /api/v1/log-events",
    "玩家事件",
    "两份文件里同一下的抄本合掉了",
    "was killed by",
    "has returned from their relaxing",
    "lost all of",
    "player_resurrect",
    "afk_start",
    "player_kick",
    "GET /api/v1/audit-actions",
    "text/event-stream; charset=utf-8",
    "X-Accel-Buffering",
    "[已隐去Klei令牌]",
    "换算不出墙上时间",
    "Truncating to snapshot #",
    "Master Server Broadcast Error",
    "聊天与公告",
    "服务端公告",
    # 审计的中文动作名：这条没了就是"操作记录页又在露裸码值"。
    "强制关闭世界（未落盘）",
    "没有这个动作取值",
    # 2026-11-04 总览那一屏的六条（#95~#100）。这一轮有一半是纯前端 + 采集层，
    # 接口探针碰不到，所以钉的是"成品字节里得带着这轮的话"：
    #   处理器型号那一行（含读不到时那句原话）、平均负载读不到时的说明、
    #   在线趋势把目录名退到悬停（'存档目录名：'）、趋势标题用的那个 computed、
    #   房间名那颗复制图标自己的方块（.ic-box，是不靠加文字做出来的那一半）。
    "处理器型号",
    "这台机器没有把处理器型号写在他能读到的地方",
    "这个系统没有平均负载这一项",
    "存档目录名：",
    "trendRoom",
    ".ic-box",
    # 2026-11-04 #101「各板块没对齐」：四处全是纯 CSS，接口探针和单测都碰不到，
    # 截图又容易被看成"差不多"，所以钉这四条规则本身——
    #   分组标题横跨两列（否则左列首卡被推低 43px）、dl 默认上边距归零（表盘与明细表齐）、
    #   三枚环的说明行按两行留位（否则底部一高一低）、触屏那一档名字行按 40 留位
    #   （否则带说明气泡那枚低 16px）。
    ".grid-wide > .section-title",
    ".host-kv { align-content: start; margin: 0; }",
    "line-height: 1.45; min-height: 32px",
    ".ring-label { min-height: 40px; }",
    # 2026-11-04 #102/#103：详情里那一行换成中文角色名（键名用引擎自己的 prefab，
    # 麦斯威尔是 waxwell、女武神是 wathgrithr），三围回到一行一个。
    "wathgrithr",
    "charName(p.prefab)",
    ".player-bars { display: grid; gap: 3px; margin-top: 4px; grid-template-columns: minmax(0, 1fr); }",
    # 2026-11-04 #104：在跑且有人的世界，玩家实况不点详情也摊着；明细表仍收着。
    # 2026-11-05 更正（#119 这轮 markers_check 才报出来）：#111 整块拆掉「详情」那一档之后，
    # 这一格只剩玩家实况，条件从 "open || live" 缩成只剩 live；旁边那条
    # .shard-detail > .detail-players:first-child 的 margin 复位也跟着明细表删掉了
    # （没有第二个子元素，就没什么上边距要复位）。原来那两条标记因此一直对不上源码，
    # 而 "open || live" 含 || 时会被下面的正则兜底放行（见 markers_check 的说明），
    # 所以它藏了好几轮才露出来。标记改成钉这一行现在的写法。
    "class=\"shard-detail\" v-if=\"live\"",
]

SYMBOLS = [
    "dstpanel/internal/mods.(*Manager).List",
    "dstpanel/internal/mods.(*Manager).InstallFromZip",
    "dstpanel/internal/mods.(*Manager).SetupProblems",
    "dstpanel/internal/modbridge.(*Bridge).PatchOverride",
    "dstpanel/internal/modbridge.(*Bridge).ListOverrides",
    "dstpanel/internal/api.(*Server).handleModUpload",
    "dstpanel/internal/api.(*Server).handleShardModPatch",
    # 上一轮 nm 检查就是靠抓这组符号发现"启停分片根本没进成品"。
    "dstpanel/internal/procrun.(*Driver).Start",
    "dstpanel/internal/procrun.(*Driver).Stop",
    "dstpanel/internal/procrun.(*Driver).Kill",
    "dstpanel/internal/procrun.(*Driver).Console",
    "dstpanel/internal/procrun.(*Driver).RunAdminCommand",
    "dstpanel/internal/api.(*Server).handleShardStart",
    "dstpanel/internal/api.(*Server).handleShardRestart",
    "dstpanel/internal/api.(*Server).handleAdminCommandPost",
    # 任务队列：从 HTTP 提交一直到流式执行外部命令，断任何一环都会在这里露出来。
    "dstpanel/internal/api.(*Server).handleTaskKinds",
    "dstpanel/internal/api.(*Server).handleTaskSubmit",
    "dstpanel/internal/api.(*Server).handleTaskCancel",
    "dstpanel/internal/worker.(*Worker).Run",
    "dstpanel/internal/worker.(*Worker).serverUpdate",
    # RunLines 是值接收者，符号名不带 *；写成 *ExecRunner 会永远查不到。
    "dstpanel/internal/procrun.ExecRunner.RunLines",
    # 工坊检索这一路：HTTP → 客户端 → 缓存，三段都要在成品里。
    "dstpanel/internal/api.(*Server).handleModSearch",
    "dstpanel/internal/api.(*Server).rememberDiskMods",
    "dstpanel/internal/steam.New",
    "dstpanel/internal/steam.(*Client).Search",
    "dstpanel/internal/steam.(*Client).Details",
    # 可变参数包装 RecordSeen 会被整个内联进 rememberDiskMods，成品里没有它自己的符号；
    # 真正在跑的是循环体 recordSeen，查错名字会得到一个假的"这段代码没进成品"。
    "dstpanel/internal/store.(*Store).recordSeen",
    "dstpanel/internal/store.(*Store).SearchSeen",
    "dstpanel/internal/store.(*Store).SeenCount",
    # 开服引导：HTTP 层 → 世界文件读写 → 内嵌字典，断一环就在这里露出来。
    # 注意 WriteWorldGen/ReadWorldGen/LevelDataNote 是值接收者（Paths 而非 *Paths）。
    "dstpanel/internal/api.(*Server).handleClusterCreate",
    "dstpanel/internal/api.(*Server).resolveWorldSpecs",
    "dstpanel/internal/api.(*Server).clusterScope",
    "dstpanel/internal/api.(*Server).handleTokenShow",
    "dstpanel/internal/api.(*Server).handleTokenPut",
    "dstpanel/internal/api.(*Server).handleWorldSchema",
    "dstpanel/internal/api.(*Server).handleWorldShow",
    "dstpanel/internal/api.(*Server).handleWorldPut",
    "dstpanel/internal/api.(*Server).buildWorldView",
    "dstpanel/internal/dst.Paths.WriteWorldGen",
    "dstpanel/internal/dst.Paths.ReadWorldGen",
    "dstpanel/internal/dst.Paths.LevelDataNote",
    "dstpanel/internal/dst.Paths.WorldGenOverride",
    # DefaultPreset / IsKnownPreset / PresetsForLocation / PresetLabel 都是几行的查表函数，
    # 链接器把它们整个内联进调用方，成品里没有自己的符号——断言它们会得到一个假的
    # "这段代码没进成品"（和 store.recordSeen 那次同一课）。钉留下来的这几个：
    # 预设白名单校验、给错误文案的可选表、以及预设表本身。
    "dstpanel/internal/dst.checkWorldGen",
    "dstpanel/internal/dst.PresetChoicesText",
    "dstpanel/internal/dst.worldPresets",
    "dstpanel/internal/dst.Paths.LevelData",
    "dstpanel/internal/dst.LoadWorldSchema",
    "dstpanel/internal/dst.(*WorldSchema).Item",
    # 启动判定这一对只有在 Start 真的调用它们时才会留下来。
    "dstpanel/internal/procrun.(*Driver).awaitBootVerdict",
    "dstpanel/internal/procrun.(*Driver).bootFailure",
    # 总览页那句"引擎在日志里已经承认了原因"：只有 overview 真引用它才不会被裁掉。
    "dstpanel/internal/procrun.(*Driver).BootFailureNote",
    "dstpanel/internal/procrun.(*Driver).SimPausedNote",
    # 模组侧新增的三段：跨分片比对、依赖判定、HTTP 路由。
    # 断言的是大函数，不是几行的查表 helper——小函数会被内联，查它会得到假失败。
    "dstpanel/internal/modbridge.(*Bridge).CompareShardOverrides",
    "dstpanel/internal/mods.CheckDependencies",
    "dstpanel/internal/api.(*Server).handleModConsistency",
    "dstpanel/internal/dst.ParseLuaAssignments",
    # 日志读取与推流：文件层（有界尾部/续读）、解析层（逐行分类）、HTTP 层（三个路由）。
    # 查大函数不查 helper：kindCodes/keep/logViewOf 这类几行的会被内联，查它们只会得到假失败。
    "dstpanel/internal/gamelog.(*Reader).Tail",
    "dstpanel/internal/gamelog.(*Reader).ReadSince",
    "dstpanel/internal/gamelog.ParseLine",
    "dstpanel/internal/gamelog.EventCatalog",
    "dstpanel/internal/api.(*Server).handleLogShow",
    "dstpanel/internal/api.(*Server).handleLogStream",
    "dstpanel/internal/api.(*Server).handleLogEvents",
    "dstpanel/internal/api.(*Server).parseLogQuery",
    # 审计中文动作名这一对：读接口和目录接口都要在，
    # 缺 auditActionLabel 界面就露裸码值，缺 handleAuditActions 筛选下拉就是空的。
    "dstpanel/internal/api.auditActionLabel",
    "dstpanel/internal/api.(*Server).handleAuditActions",
    # 2026-11-04 #95：占用改成按进程树累加。sumProcTree 是把 procSelf/procChildren
    # 当函数值传进去的，所以这三个符号都不会被内联掉——缺任何一个等于采集层还是
    # "只算根进程"那一版，界面上就是那个 2 MB 的空壳。
    "dstpanel/internal/hoststats.sumProcTree",
    "dstpanel/internal/hoststats.procChildren",
    "dstpanel/internal/hoststats.(*Collector).Process",
]


def check(path):
    data = open(path, "rb").read()
    magic = "PE(windows)" if data[:2] == b"MZ" else ("ELF(linux)" if data[:4] == b"\x7fELF" else "unknown")
    print("%s  %s  %d bytes" % (path, magic, len(data)))
    bad = []
    for m in MARKERS:
        n = data.count(m.encode("utf-8"))
        print("   %-46s x%d" % (m[:44], n))
        if n == 0:
            bad.append(m)
    return bad


def nm_symbols(path):
    out = subprocess.run([GO, "tool", "nm", path], capture_output=True)
    if out.returncode != 0:
        return None
    return out.stdout.decode("utf-8", "replace")


def main():
    bad = []
    for p in [r".tmp_bin/dstpanel.exe", r".tmp_bin/dstpanel-linux"]:
        bad += ["%s :: %s" % (p, x) for x in check(p)]
        syms = nm_symbols(p)
        if syms is None:
            print("   go tool nm 读不了这个文件，符号检查没做成")
            bad.append(p + " :: nm")
            continue
        missing = [s for s in SYMBOLS if s not in syms]
        for s in missing:
            print("   缺符号 %s" % s)
        print("   符号命中 %d/%d" % (len(SYMBOLS) - len(missing), len(SYMBOLS)))
        bad += ["%s :: %s" % (p, s) for s in missing]
    print("\nFAILED: %s" % (" | ".join(bad) if bad else "none"))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
