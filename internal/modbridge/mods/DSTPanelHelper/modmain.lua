-- modmain：把桥接器挂到世界对象的周期任务上。
--
-- 要点：
-- * 只在服务端主世界（ismastersim）跑，客户端和副本不需要。
-- * 任何异常都用 pcall 兜住并 print，绝不让面板助手把玩家的服务器带崩。
-- * 助手加载失败时静默降级：面板靠 status.json 心跳缺失判定"助手离线"，
--   而不是靠这里抛错。
--
-- 这里的 API 用法都对着服务端自带的 scripts/modutil.lua 与 scripts/mods.lua 核过：
--   GetModConfigData / AddPrefabPostInit 是 Klei 注入给 modmain 沙箱环境的全局函数，
--   不需要 require("modutil")（那个版本要求显式传 modname，是给引擎内部用的）。
local _G = GLOBAL

-- 沙箱里缺什么就自己补什么，别指望标准库都在。
-- 依据：util.lua:788 的 RunInEnvironment 就是裸 setfenv(fn, env)，env 没有 __index 回退；
-- env 里只有 pairs/ipairs/print/math/table/type/string/tostring/require/Class、
-- GLOBAL/modname/MODROOT，加上 modutil 注入的那批 Add* 与 GetModConfigData。
-- 也就是说 pcall、next、rawget、assert、setmetatable、tonumber 在这里都是 nil。
-- 这里踩过的代价：裸写 pcall 会让 modmain 首次加载即报错，
-- 引擎随即 DisableBecauseBad 把助手永久禁用（mods.lua:585），面板从此收不到快照。
local pcall = _G.pcall

-- 兄弟模块的加载方式（这段结论是从服务端脚本里读出来的，不是猜的）：
-- * modmain 跑在 Klei 现造的 env 表里，那张表没有 package 这一项，
--   所以这里改不了也不能改 package.path（modutil.lua 把 AddClassPostConstruct
--   的形参取名成 package，正好说明环境里根本没有这个全局）。
-- * Klei 在跑 modmain 之前，把 MODS_ROOT..modname.."\\scripts\\?.lua" 拼进了真正的
--   package.path（mods.lua:528），而它自己的 loader 会把这条路径里的反斜杠归一成斜杠
--   （main.lua:142）。所以 panel_bridge 只要放在本 MOD 的 scripts/ 目录下，
--   Windows 客户端和 Linux 专用服务端都能 require 到，不需要我们插手路径。
-- * require 回来的 chunk 用的是引擎真实全局环境（不是这个沙箱），
--   所以 panel_bridge 里面 TheSim/TheNet/AllPlayers/os 都直接可用。
local ok, bridge_class = pcall(require, "panel_bridge")
if not ok or type(bridge_class) ~= "table" then
    print("[DSTPanelHelper] 载入 scripts/panel_bridge.lua 失败，助手已停用: " .. tostring(bridge_class))
    return
end

local function setting(key, fallback)
    local ok2, v = pcall(GetModConfigData, key)
    if not ok2 or v == nil then
        return fallback
    end
    return v
end

local poll = setting("poll_interval", 1)
if type(poll) ~= "number" or poll < 1 then
    poll = 1
end
local detail = setting("player_detail", true)

-- 分片名只用于回执与快照里的自报身份：面板是按分片目录去读快照的，快照自报的名字
-- 必须和目录名对得上，界面才不会出现"1"这种码值，跨分片串档也才看得出来。
--
-- 2026-10-31 云机实测：TheShard:GetShardId() 返回的是 server.ini 里 [SHARD] id 的文本，
-- 地表是 "1"、洞穴是 "2"，**不是**目录名（原先这里以为是 "Master"/"Caves"，所以快照里
-- 直接写了个 1）。id 到目录名的这条对应关系同时是引擎的 SHARDID.MASTER=1 约定和面板
-- 自己写 server.ini 的模板约定，两边都成立。
-- 沙箱里没有 tonumber，所以统一 tostring 之后按字符串查表。
local SHARD_ID_NAMES = {["1"] = "Master", ["2"] = "Caves"}
local shard_name = "Master"
pcall(function()
    if _G.TheShard and _G.TheShard.GetShardId then
        local named = SHARD_ID_NAMES[tostring(_G.TheShard:GetShardId())]
        if named then
            shard_name = named
        end
    end
end)

local bridge = bridge_class.new({shard = shard_name})

AddPrefabPostInit("world", function(inst)
    if not inst.ismastersim then
        return
    end
    -- 首轮先把已处理 id 读回来，防止面板重启后重复执行旧命令。
    inst:DoTaskInTime(3, function()
        local ok2, err = pcall(function()
            bridge:load_processed(function()
                bridge:flush_processed()
            end)
        end)
        if not ok2 then
            print("[DSTPanelHelper] 初始化失败: " .. tostring(err))
        end
    end)
    inst:DoPeriodicTask(poll, function()
        local ok2, err = pcall(function()
            bridge:tick(detail)
        end)
        if not ok2 then
            print("[DSTPanelHelper] 轮询异常: " .. tostring(err))
        end
    end, 6)
end)
