-- 面板 ↔ 服务端 的文件桥。
--
-- 通道协议（刻意设计成"每个文件只有一个写者"，避免半写覆盖）：
--   dstpanel_queue.txt     面板写：每行 `id|命令|参数1|参数2`
--   dstpanel_processed.txt MOD 写：已执行过的 id，防重复执行
--   dstpanel_result.json   MOD 写：id -> {ok, error, value}
--   dstpanel_status.json   MOD 写：心跳 + 世界/玩家快照
-- 读是异步回调（TheSim:GetPersistentString），所以同一时刻只允许一个在途读请求。
--
-- 文件名必须是平铺的，不能带子目录：云机实测 TheSim:SetPersistentString 不会创建多级
-- 目录，写 "panel/status.json" 只得到一句 "[CRITICAL] Failed to save file"，
-- 而 TheSim.MakeDirectory 在这个构建里是 nil，MOD 没法自己建目录。
local json = require("panel_json")

local FILE_QUEUE = "dstpanel_queue.txt"
local FILE_PROCESSED = "dstpanel_processed.txt"
local FILE_RESULT = "dstpanel_result.json"
local FILE_STATUS = "dstpanel_status.json"
local FILE_MAP = "dstpanel_map.json"

local MAX_PROCESSED = 500
local MAX_RESULT_CHARS = 200000

-- 地图网格边长。一格的采样点覆盖约 3×3 个 tile：144 在桌面和手机上都能画出清楚的
-- 地形轮廓，JSON 又只有几十 KB。再细就会把 4h4g 的小机拖进无谓的字符串拼接。
local MAP_GRID = 144
-- 个体点位上限。云机实测一张 425×425 的森林图有 9,487 个能认出的实体，
-- 12000 留了近三倍余量；超了只丢位置、计数照旧（见 add_entity）。
local MAP_MAX_ENTS = 12000

local bridge = {}

function bridge.new(opts)
    local self = setmetatable({}, {__index = bridge})
    self.shard = opts.shard or "Master"
    self.processed = {}
    self.processed_order = {}
    self.results = {}
    self.reading = false
    self.dirty_processed = false
    self.dirty_results = false
    self.seq = 0
    return self
end

local function write_file(path, text)
    TheSim:SetPersistentString(path, text, false)
end

local function read_file(path, cb)
    TheSim:GetPersistentString(path, function(load_success, data)
        if not load_success or data == nil or data == "" then
            cb(nil)
            return
        end
        cb(data)
    end)
end

local function split_lines(text)
    local out = {}
    for line in (text .. "\n"):gmatch("([^\n]*)\n") do
        if line ~= "" then
            out[#out + 1] = line
        end
    end
    return out
end

local function split_pipe(line)
    local parts = {}
    for token in (line .. "|"):gmatch("(.-)|") do
        parts[#parts + 1] = token
    end
    return parts
end

function bridge:mark_processed(id)
    if self.processed[id] then
        return false
    end
    self.processed[id] = true
    self.processed_order[#self.processed_order + 1] = id
    while #self.processed_order > MAX_PROCESSED do
        local drop = table.remove(self.processed_order, 1)
        self.processed[drop] = nil
    end
    self.dirty_processed = true
    return true
end

function bridge:record_result(id, res)
    self.results[id] = res
    self.dirty_results = true
end

-- 结果表只保留最近若干条，防止跑久了把存档目录塞满。
function bridge:trim_results()
    local ids = {}
    for id in pairs(self.results) do
        ids[#ids + 1] = id
    end
    table.sort(ids)
    while #ids > MAX_PROCESSED do
        self.results[table.remove(ids, 1)] = nil
    end
end

function bridge:flush_processed()
    if not self.dirty_processed then
        return
    end
    write_file(FILE_PROCESSED, table.concat(self.processed_order, "\n"))
    self.dirty_processed = false
end

function bridge:flush_results()
    if not self.dirty_results then
        return
    end
    self:trim_results()
    local text = json.encode(self.results)
    if #text > MAX_RESULT_CHARS then
        text = json.encode({overflow = true, note = "结果过多，仅保留最近部分"})
    end
    write_file(FILE_RESULT, text)
    self.dirty_results = false
end

function bridge:load_processed(done)
    read_file(FILE_PROCESSED, function(data)
        if data then
            for _, id in ipairs(split_lines(data)) do
                if not self.processed[id] then
                    self.processed[id] = true
                    self.processed_order[#self.processed_order + 1] = id
                end
            end
        end
        done()
    end)
end

-- ---------- 世界与玩家快照 ----------

local function safe_num(v)
    if type(v) == "number" then
        return v
    end
    return nil
end

function bridge:collect_player(inst)
    -- 字段名全部对着服务端 scripts 里 Klei 自己的用法核对：
    -- health.currenthealth/maxhealth、hunger.current/max、sanity.current/max、inventory:NumItems()。
    -- 角色等级（experience）这个版本的服务端脚本里没有对应组件，所以不采集，免得写个永远为 nil 的键。
    local out = {
        name = inst.name or "",
        prefab = inst.prefab or "",
        userid = inst.userid or "",
    }
    local health = inst.components.health
    if health then
        out.health = safe_num(health.currenthealth)
        out.max_health = safe_num(health.maxhealth)
        out.is_dead = health:IsDead() and true or false
    end
    local hunger = inst.components.hunger
    if hunger then
        out.hunger = safe_num(hunger.current)
        out.max_hunger = safe_num(hunger.max)
    end
    local sanity = inst.components.sanity
    if sanity then
        out.sanity = safe_num(sanity.current)
        out.max_sanity = safe_num(sanity.max)
    end
    if inst.Transform then
        local x, _, z = inst.Transform:GetWorldPosition()
        out.x = safe_num(math.floor(x or 0))
        out.z = safe_num(math.floor(z or 0))
    end
    local inv = inst.components.inventory
    if inv then
        out.items = inv:NumItems() or 0
    end
    return out
end

function bridge:collect_status()
    local st = TheWorld.state
    -- 这里没有 day，是故意的：世界状态上没有这个字段（把这版服务端的 lua 全文搜
    -- state.day 一处都没有），写 st.day 只会取到 nil，于是面板读出一个永远为 0 的天数。
    -- 第几天由面板按 Klei 自己的算法从 cycles 换算（worldstate.lua 的 GetWorldAge）。
    local world = {
        cycles = st and safe_num(st.cycles),
        season = st and st.season,
        phase = st and st.phase,
        temperature = st and safe_num(st.temperature),
        wetness = st and safe_num(st.wetness),
        -- 这一季过完了几天、还剩几天。引擎在 components/seasons.lua 里按天维护这两个读数，
        -- 换季时 elapsed 归 0，再由 OnSeasonTick 复制进世界状态（worldstate.lua:122-123），
        -- 所以这里只如实抄出来，不自己算：季节长度在 Klei 那里是分模式拼的（春秋那份
        -- totaldaysinseason 被人为翻倍、无尽模式 remaining 直接报 10000），一算就错。
        -- 取不到（这版引擎没有这两个读数）时保持没有这个键，面板据此不说季内天数。
        elapsed_days_in_season = st and safe_num(st.elapseddaysinseason),
        remaining_days_in_season = st and safe_num(st.remainingdaysinseason),
    }
    local players = {}
    if AllPlayers then
        for _, inst in ipairs(AllPlayers) do
            players[#players + 1] = self:collect_player(inst)
        end
    end
    self.seq = self.seq + 1
    return {
        shard = self.shard,
        seq = self.seq,
        -- GetSessionIdentifier 就是 save/session/<GAMEID> 里那个 GAMEID，
        -- Klei 自己在 shardindex.lua 里用它当 session_id，面板据此定位当前世界目录。
        game_id = TheNet:GetSessionIdentifier() or "",
        heartbeat = os.clock and os.clock() or 0,
        server_name = TheNet:GetServerName() or "",
        max_players = TheNet:GetServerMaxPlayers() or 0,
        world = world,
        players = json.array(players),
        player_count = #players,
    }
end

function bridge:write_status(detail_enabled)
    local ok, snap = pcall(function()
        local s = self:collect_status()
        if not detail_enabled then
            s.players = nil
        end
        return s
    end)
    if ok then
        write_file(FILE_STATUS, json.encode(snap))
    else
        write_file(FILE_STATUS, json.encode({shard = self.shard, error = tostring(snap)}))
    end
end

-- ---------- 世界地图 ----------
--
-- 地形按网格等距采样：每个格子取中心附近的一个 tile。不逐 tile 导出，
-- 是因为默认地表 425×425 有十八万个格子，JSON 会大到写不进存档。
--
-- 建筑/资源来自服务端的 Ents 表，只保留"能认出来"的那几类；按格子聚合成
-- [格x, 格y, 类别, 数量] 或 [格x, 格y, 类别, 数量, 资源子类]，
-- 既压小了体积，也避免一张基地图上叠几十个一样的图标。
-- 玩家不在这里导出：状态快照每秒都带坐标，面板直接拿那份数据画，人动了地图不用重导。
--
-- 类别编号要和 Go 侧 internal/modbridge/map.go 的 Cat* 常量一致。
local CAT_BUILD = 1
local CAT_RES = 2
local CAT_DANGER = 3
local CAT_COMPANION = 4
-- 资源子类。按 prefab 名前缀判，不按 tag：实测 berrybush.lua 给自己加的 tag 是
-- bush/plant/renewable 这一套，根本没有 "berrybush" tag，靠 tag 分只会全部落到"其它"。
local SUB_TREE, SUB_SAPLING, SUB_GRASS, SUB_REEDS, SUB_BERRY = 1, 2, 3, 4, 5
local SUB_MUSHROOM, SUB_FLOWER, SUB_CACTUS, SUB_STONE, SUB_OTHER = 6, 7, 8, 9, 10

local DANGER_PREFABS = {
    spiderden = true, spiderden_2 = true, spiderden_3 = true,
    tentacle = true, tentacle_pillar = true, houndmound = true,
    killerbeehive = true, walrus_camp = true,
}

local RES_TAGS = {
    "tree", "boulder", "rock", "berrybush", "grass", "sapling",
    "flower", "mushroom", "reeds", "cactus", "planted",
}

-- 有些资源没有上面那些 tag（或版本换了 tag 名），用 prefab 前缀兜底。
-- 前缀只取不会误伤物品名的几段：物品在下面先被 replica.inventoryitem 挡掉了。
local RES_PREFABS = {
    "evergreen", "deciduoustree", "twiggy", "sapling", "berrybush",
    "grass", "flower", "mushroom", "reeds", "cactus", "marsh_bush",
    "rock", "boulder", "marble",
}

local function starts_with(s, prefix)
    return s:sub(1, #prefix) == prefix
end

local function resource_sub(prefab)
    -- 顺序有讲究：rock_flintless 之类要先落在石料，不能被后面的宽前缀抢走。
    if prefab:find("^berrybush") or prefab:find("^berry") then return SUB_BERRY end
    if prefab:find("^grass") then return SUB_GRASS end
    if prefab:find("^sapling") then return SUB_SAPLING end
    if prefab:find("^reeds") then return SUB_REEDS end
    if prefab:find("^flower") then return SUB_FLOWER end
    if prefab:find("^cactus") then return SUB_CACTUS end
    if prefab:find("^mushroom") or prefab:find("^glowcap") or prefab:find("^cap_") then return SUB_MUSHROOM end
    if prefab:find("^rock") or prefab:find("^boulder") or prefab:find("^marble") then return SUB_STONE end
    if prefab:find("^evergreen") or prefab:find("^deciduoustree") or prefab:find("^palm")
        or prefab:find("^jungle") or prefab:find("^swamp") or prefab:find("^mangrove")
        or prefab:find("^tree") or prefab:find("^conifer") then return SUB_TREE end
    return SUB_OTHER
end

-- 返回 (类别, 资源子类)。类别 0 表示"这格子上不画它"。
local function classify_entity(inst)
    if not inst or not inst.prefab or inst.prefab == "" then
        return 0
    end
    if inst:HasTag("player") or inst:HasTag("INLIMBO") or inst:HasTag("FX") then
        return 0
    end
    -- 地上/箱子里的物品也有 Transform，不排掉的话整张图会被木头和石头铺满。
    if inst.replica and inst.replica.inventoryitem then
        return 0
    end
    -- 伙伴放在危险之前：阿比盖尔、切斯特这些引擎自己也带 monster/shadow 类的 tag，
    -- 先判敌我才会把一个跟着人的东西标成"这里有危险"。
    if inst:HasTag("companion") then
        return CAT_COMPANION, 0
    end
    if DANGER_PREFABS[inst.prefab] then
        return CAT_DANGER, 0
    end
    if inst:HasTag("monster") or inst:HasTag("hostile") then
        return CAT_DANGER, 0
    end
    if inst:HasTag("spiderden") or inst:HasTag("houndmound") or inst:HasTag("tentacle") then
        return CAT_DANGER, 0
    end
    if inst:HasTag("structure") then
        return CAT_BUILD, 0
    end
    for _, tag in ipairs(RES_TAGS) do
        if inst:HasTag(tag) then
            return CAT_RES, resource_sub(inst.prefab)
        end
    end
    for _, prefix in ipairs(RES_PREFABS) do
        if starts_with(inst.prefab, prefix) then
            return CAT_RES, resource_sub(inst.prefab)
        end
    end
    return 0
end

-- 世界坐标 -> tile 坐标。优先问引擎（不同世界尺寸下它最准），
-- 拿不到再按"一格 4 个世界单位、原点在地图中心"换算。
local function tile_coords(map, width, height, x, z)
    if map.GetTileCoordsAtPoint then
        local ok, tx, ty = pcall(map.GetTileCoordsAtPoint, map, x, 0, z)
        if ok and type(tx) == "number" and type(ty) == "number" then
            return tx, ty
        end
    end
    return math.floor(x / 4 + width / 2), math.floor(z / 4 + height / 2)
end

local function clamp(v, lo, hi)
    if v < lo then return lo end
    if v > hi then return hi end
    return v
end

function bridge:collect_map()
    local map = TheWorld and TheWorld.Map
    if not map or not map.GetSize or not map.GetTile then
        return nil, "这个世界还没有地图（地形可能还在生成）"
    end
    local width, height = map:GetSize()
    width, height = math.floor(width or 0), math.floor(height or 0)
    if width <= 0 or height <= 0 then
        return nil, "世界地图尺寸还读不出来"
    end

    local grid = MAP_GRID
    local tiles = {}
    for gy = 1, grid do
        for gx = 1, grid do
            local tx = clamp(math.floor((gx - 0.5) * width / grid), 0, width - 1)
            local ty = clamp(math.floor((gy - 0.5) * height / grid), 0, height - 1)
            local ok, tile = pcall(map.GetTile, map, tx, ty)
            if not ok or type(tile) ~= "number" then
                tile = -1
            end
            tiles[(gy - 1) * grid + gx] = tile
        end
    end

    -- 逐个实体导出，不在助手这边聚合：一格里有 12 丛浆果，要画成 12 个图标就得
    -- 知道 12 个位置；聚合过的数据只能说"这格有 12 个"。聚合改由面板按当前缩放现算。
    local ents = {}
    local totals = {build = 0, res = 0, danger = 0, companion = 0}
    local totals_by_cat = {0, 0, 0, 0}
    local truncated = false

    local function add_entity(inst)
        local cat, sub = classify_entity(inst)
        if cat == 0 then
            return
        end
        if not inst.Transform then
            return
        end
        local ok, x, _, z = pcall(function()
            return inst.Transform:GetWorldPosition()
        end)
        if not ok or type(x) ~= "number" or type(z) ~= "number" then
            return
        end
        -- 计数在容量判断之前：超上限只丢个体位置，汇总数字必须还是全量的，
        -- 否则界面上"资源 9096"会悄悄变成截断后的假数。
        totals_by_cat[cat] = totals_by_cat[cat] + 1
        if #ents >= MAP_MAX_ENTS then
            truncated = true
            return
        end
        local tx, ty = tile_coords(map, width, height, x, z)
        ents[#ents + 1] = {clamp(tx, 0, width - 1), clamp(ty, 0, height - 1), cat, sub}
    end

    for _, inst in pairs(Ents or {}) do
        pcall(add_entity, inst)
    end
    totals.build = totals_by_cat[1]
    totals.res = totals_by_cat[2]
    totals.danger = totals_by_cat[3]
    totals.companion = totals_by_cat[4]

    -- 排序只为稳定：这份文件会被逐条比对（测试断言、人工 diff），
    -- pairs 的顺序每次都不一样就没法比。
    table.sort(ents, function(a, b)
        if a[2] ~= b[2] then return a[2] < b[2] end
        if a[1] ~= b[1] then return a[1] < b[1] end
        if a[3] ~= b[3] then return a[3] < b[3] end
        return a[4] < b[4]
    end)

    return {
        shard = self.shard,
        at = os.time and os.time() or 0,
        game_id = TheNet:GetSessionIdentifier() or "",
        width = width,
        height = height,
        grid = grid,
        tiles = json.array(tiles),
        ents = json.array(ents),
        totals = totals,
        truncated = truncated,
    }
end

local function map_export(bridge_obj)
    local ok, data, err = pcall(function()
        return bridge_obj:collect_map()
    end)
    if not ok then
        return {ok = false, error = "导出地图时出错: " .. tostring(data)}
    end
    if type(data) ~= "table" then
        return {ok = false, error = err or "这个世界暂时导不出地图"}
    end
    local text = json.encode(data)
    -- 上限是防线不是目标：144×144 的地形约 45 KB，逐个实体导出后总大小在 200 KB 上下
    -- （云机实测一张森林图 9,487 个实体）。真超了说明世界尺寸或实体数异常，
    -- 宁可报错也不要把存档目录写爆。
    if #text > 2000000 then
        return {ok = false, error = "地图数据太大，暂时无法导出"}
    end
    write_file(FILE_MAP, text)
    return {ok = true, value = {
        width = data.width, height = data.height, grid = data.grid,
        ents = #data.ents, truncated = data.truncated,
    }}
end

-- ---------- 命令执行 ----------

local handlers = {}

function handlers.ping(bridge_obj)
    return {ok = true, value = "pong", shard = bridge_obj.shard}
end

function handlers.status(bridge_obj)
    return {ok = true, value = bridge_obj:collect_status()}
end

handlers.map = map_export

function handlers.announce(_, args)
    local text = table.concat(args, "|")
    if text == "" then
        return {ok = false, error = "公告内容为空"}
    end
    TheNet:Announce(text)
    return {ok = true, value = text}
end

function handlers.kick(_, args)
    local target = args[1]
    if not target or target == "" then
        return {ok = false, error = "缺少玩家名"}
    end
    local found = nil
    for _, inst in ipairs(AllPlayers or {}) do
        if inst.name == target or inst.userid == target then
            found = inst
            break
        end
    end
    if not found then
        return {ok = false, error = "没有找到在线玩家 " .. target}
    end
    local reason = args[2] or ""
    local call_ok, err = pcall(function()
        TheNet:Kick(found.userid or target)
    end)
    if not call_ok then
        return {ok = false, error = "踢人接口调用失败: " .. tostring(err)}
    end
    return {ok = true, value = {kicked = target, reason = reason}}
end

-- 按 userid 或玩家名找在线角色。AllPlayers 只包含"已经在这个分片里生成出来"的玩家；
-- 还在连接中的角色查不到，这时宁可报"没找到在线玩家"，也不要凭名字猜一个 userid。
local function find_online_player(target)
    if not target or target == "" then
        return nil
    end
    for _, inst in ipairs(AllPlayers or {}) do
        if inst.userid == target or inst.name == target then
            return inst
        end
    end
    return nil
end

-- 拉黑/解封允许对离线玩家操作，所以除了在线角色，也接受"看起来就是 userid"的输入。
-- 这里不校验格式的合法性（Klei 自己会拒），只避免把玩家名当成 userid 写进黑名单。
local function resolve_userid(target)
    local inst = find_online_player(target)
    if inst then
        return inst.userid, inst.name
    end
    if target and (target:match("^KU_") or target:match("^OU_") or target:match("^%d+$")) then
        return target, nil
    end
    return nil, nil
end

function handlers.mute(_, args)
    local target = args[1]
    local inst = find_online_player(target)
    if not inst then
        return {ok = false, error = "没有找到在线玩家 " .. tostring(target) .. "；禁言只对在线玩家生效"}
    end
    if not inst.userid or inst.userid == "" then
        return {ok = false, error = "这个玩家还没拿到 user id，等他完全进入世界再试"}
    end
    if type(TheNet.SetPlayerMuted) ~= "function" then
        return {ok = false, error = "这个版本的服务端没有禁言接口，改用黑名单"}
    end
    local call_ok, err = pcall(function()
        TheNet:SetPlayerMuted(inst.userid, true)
    end)
    if not call_ok then
        return {ok = false, error = "禁言接口调用失败: " .. tostring(err)}
    end
    return {ok = true, value = {userid = inst.userid, name = inst.name, muted = true}}
end

function handlers.unmute(_, args)
    local target = args[1]
    local inst = find_online_player(target)
    if not inst then
        return {ok = false, error = "没有找到在线玩家 " .. tostring(target) .. "；解除禁言只对在线玩家生效"}
    end
    if type(TheNet.SetPlayerMuted) ~= "function" then
        return {ok = false, error = "这个版本的服务端没有禁言接口"}
    end
    local call_ok, err = pcall(function()
        TheNet:SetPlayerMuted(inst.userid, false)
    end)
    if not call_ok then
        return {ok = false, error = "解除禁言失败: " .. tostring(err)}
    end
    return {ok = true, value = {userid = inst.userid, name = inst.name, muted = false}}
end

function handlers.ban(_, args)
    local userid, name = resolve_userid(args[1])
    if not userid or userid == "" then
        return {ok = false, error = "需要一个 user id（KU_ / OU_ 开头或纯数字），或者一个在线玩家名"}
    end
    if type(TheNet.Ban) ~= "function" then
        return {ok = false, error = "这个版本的服务端没有拉黑接口；到面板的名单管理里写 blocklist.txt，重启这个世界后生效"}
    end
    local call_ok, err = pcall(function()
        TheNet:Ban(userid)
    end)
    if not call_ok then
        return {ok = false, error = "拉黑接口调用失败: " .. tostring(err)}
    end
    return {ok = true, value = {userid = userid, name = name, banned = true}}
end

function handlers.unban(_, args)
    local userid, name = resolve_userid(args[1])
    if not userid or userid == "" then
        return {ok = false, error = "需要一个 user id"}
    end
    if type(TheNet.Unban) ~= "function" then
        return {ok = false, error = "这个版本的服务端没有解封接口；到面板的名单管理里改 blocklist.txt，重启这个世界后生效"}
    end
    local call_ok, err = pcall(function()
        TheNet:Unban(userid)
    end)
    if not call_ok then
        return {ok = false, error = "解封接口调用失败: " .. tostring(err)}
    end
    return {ok = true, value = {userid = userid, name = name, banned = false}}
end

function bridge:execute(line)
    local parts = split_pipe(line)
    local id, cmd = parts[1], parts[2]
    if not id or id == "" or not cmd or cmd == "" then
        return
    end
    if not self:mark_processed(id) then
        return -- 已经执行过，面板还没来得及裁剪 queue
    end
    local args = {}
    for i = 3, #parts do
        args[#args + 1] = parts[i]
    end
    local fn = handlers[cmd]
    if not fn then
        self:record_result(id, {ok = false, error = "未知命令: " .. cmd})
        return
    end
    local call_ok, res = pcall(fn, self, args)
    if not call_ok then
        res = {ok = false, error = "命令执行异常: " .. tostring(res)}
    elseif type(res) ~= "table" then
        res = {ok = true, value = res}
    end
    self:record_result(id, res)
end

-- 一轮：读队列 -> 执行 -> 回写三个文件。读是异步的，用 reading 防止重入。
function bridge:tick(detail_enabled)
    if self.reading then
        return
    end
    self.reading = true
    local finish = function()
        self.reading = false
    end
    read_file(FILE_QUEUE, function(data)
        local queue_ok, queue_err = pcall(function()
            if data then
                for _, line in ipairs(split_lines(data)) do
                    self:execute(line)
                end
            end
            self:write_status(detail_enabled)
            self:flush_processed()
            self:flush_results()
        end)
        finish()
        if not queue_ok then
            -- 绝不能让一次异常把整个分片带崩。
            print("[DSTPanelHelper] 桥接轮询异常: " .. tostring(queue_err))
        end
    end)
end

return bridge
