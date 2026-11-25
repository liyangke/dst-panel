-- DST 面板助手：把服务端内部状态导出成文件，供管理面板读取。
-- 只在服务端运行，客户端不需要同步（all_clients_require_mod = false）。
version = "1.0.0"
-- 必须跟引擎的 MOD_API_VERSION 一致（v760944：scripts/mods.lua 里是 10）。
-- 写高了引擎会打 "api_version ... is in the future" 并拒绝加载这个 MOD。
api_version = 10
name = "DST 管理面板助手"
author = "dstpanel"
room_only = false
server_only = true
all_clients_require_mod = false
client_only_mod = false
dedicated_server_only = true

description = [[
为自建服务器管理面板提供结构化运行数据：
世界天数/季节、玩家生命饥饿理智、以及一条命令回执通道。
不改变任何玩法数值，卸载后存档不受影响。
]]

local POLL_CHOICES =
{
    {description = "每 1 秒", data = 1},
    {description = "每 2 秒", data = 2},
    {description = "每 5 秒", data = 5},
    {description = "每 10 秒", data = 10},
}

settings =
{
    {
        name = "poll_interval",
        label = "数据回写间隔",
        typing = "number",
        values = POLL_CHOICES,
        default = 1,
    },
    {
        name = "player_detail",
        label = "玩家明细",
        typing = "bool",
        default = true,
        description = "关闭后只回传人数，减少每秒的文件写入量",
    },
}
