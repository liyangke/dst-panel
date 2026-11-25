"""助手 MOD 的 Lua 侧检查（.tools/mod_lua_test.py）。

为什么要这么麻烦：panel_bridge.lua 是唯一一份"面板读不到就只能靠猜"的代码——
它跑在游戏进程里，Go 测试碰不到，改错了要等分片重启才暴露，而且引擎只会打一行
日志，界面上看到的是"地图导不出来"。lupa 能在这里直接把它跑起来：
先做语法检查，再用桩替掉 TheWorld/Ents/TheNet，验 collect_map 真的产出
面板协议认得的四元组/五元组。

跑法：python .tools/mod_lua_test.py
"""

import io
import json
import os
import sys

from lupa import LuaRuntime

SCRIPTS = os.path.join("internal", "modbridge", "mods", "DSTPanelHelper", "scripts")
FILES = ["panel_json.lua", "panel_bridge.lua", "../modmain.lua"]

# 桩：一个 512×512 tile 的世界，地表全是 FOREST(7)；实体覆盖到每一类别。
STUB = r"""
package.path = [[%(path)s]] .. ";" .. package.path

local function ent(prefab, tags, x, z)
  local self = { prefab = prefab, Transform = { GetWorldPosition = function(s) return x, 0, z end } }
  self.HasTag = function(s, t) return tags[t] == true end
  self.replica = {}
  return setmetatable(self, {__index = self})
end

TheNet = { GetSessionIdentifier = function() return "LUA-CHECK" end }
TheWorld = { Map = {
  GetSize = function() return 512, 512 end,
  GetTile = function() return 7 end,
  GetTileCoordsAtPoint = function(map, x, y, z) return math.floor(x / 4 + 256), math.floor(z / 4 + 256) end,
} }

Ents = {
  berry = ent("berrybush", {plant = true, renewable = true}, 8, 8),
  bush2 = ent("berrybush_salted", {}, 12, 8),
  grass = ent("grass", {plant = true}, 8, 12),
  tree  = ent("evergreen", {tree = true}, 12, 12),
  rock  = ent("rock1", {}, 16, 8),
  den   = ent("spiderden", {spiderden = true}, 20, 20),
  chester = ent("chester", {companion = true}, 24, 24),
  -- 跟着人的随从即使也带 monster，也不能被算成危险点
  abigail = ent("abigail", {companion = true, monster = true}, 28, 24),
  -- 掉在地上的木头：有 inventoryitem，必须整个跳过
  log = ent("log", {inventoryitem = true}, 32, 32),
  ghost = ent("ghost", {player = true}, 36, 36),
  fxtree = ent("tree_part", {FX = true}, 40, 40),
}

local bridge = require("panel_bridge")
bridge.shard = "Master"
local data, err = bridge:collect_map()
if not data then return {err = tostring(err)} end
-- 结果用 MOD 自己的 JSON 编码器转成字符串再交给 Python：
-- 省掉 lupa 的表迭代差异，顺带把"这份 payload 真能编码成 JSON"也验了一遍。
local json2 = require("panel_json")
return {text = json2.encode({
  ents = data.ents, totals = data.totals, grid = data.grid, tiles = #data.tiles,
  truncated = data.truncated,
})}
"""


def fail(msg):
    print("FAIL: " + msg)
    sys.exit(1)


def main():
    for f in FILES:
        p = os.path.normpath(os.path.join(SCRIPTS, f))
        if not os.path.exists(p):
            fail("找不到 %s" % p)
        src = io.open(p, encoding="utf-8").read()
        try:
            LuaRuntime().execute("do end")  # 运行时可用性自检
        except Exception as e:  # noqa: BLE001
            fail("lupa 起不来: %s" % e)
        lua = LuaRuntime()
        try:
            lua.compile(src)
        except Exception as e:  # noqa: BLE001
            fail("%s 语法不过: %s" % (p, e))
        print("语法 OK  %s" % p)

    lua = LuaRuntime(unpack_returned_tuples=True)
    code = STUB % {"path": SCRIPTS.replace("\\", "/") + "/?.lua"}
    res = lua.execute(code)
    if res is None:
        fail("桩脚本没返回东西")
    if getattr(res, "err", None):
        fail("collect_map 返回错误: %s" % str(res.err))

    payload = json.loads(str(res.text))
    if "points" in payload:
        fail("新版助手不该再输出聚合 points：%s" % list(payload))
    # [tileX, tileY, 类别, 子类]：坐标是引擎 tile，不是 144 网格，
    # 一格 12 丛浆果要能画成 12 个图标，靠的就是这里没被量化过。
    ents = [[int(v) for v in p] for p in payload["ents"]]
    by_pos = {}
    for p in ents:
        by_pos.setdefault((p[0], p[1]), []).append(p)

    def at(x, z):
        return (int(x / 4 + 256), int(z / 4 + 256))

    checks = [
        ("浆果丛 -> 资源·子类5", at(8, 8), 2, 5),
        ("草 -> 资源·子类3", at(8, 12), 2, 3),
        ("树 -> 资源·子类1", at(12, 12), 2, 1),
        ("石头 -> 资源·子类9", at(16, 8), 2, 9),
        ("蜘蛛巢 -> 危险3", at(20, 20), 3, 0),
        ("切斯特 -> 伙伴4", at(24, 24), 4, 0),
        ("阿比盖尔 -> 伙伴4（带 monster 也不算危险）", at(28, 24), 4, 0),
    ]
    for label, c, want_cat, want_sub in checks:
        rows = [p for p in by_pos.get(c, []) if p[2] == want_cat]
        if not rows:
            fail("%s：tile %s 上没有类别 %d，实际 = %s" % (label, c, want_cat, by_pos.get(c)))
        if want_sub and not any(p[3] == want_sub for p in rows):
            fail("%s：子类应为 %d，实际 = %s" % (label, want_sub, rows))
        print("分类 OK  %s" % label)

    for label, c in [("掉落物不进图", at(32, 32)), ("玩家不算", at(36, 36)), ("特效不算", at(40, 40))]:
        if c in by_pos:
            fail("%s：tile %s 不该有点，实际 %s" % (label, c, by_pos[c]))
    print("排除 OK  掉落物/玩家/特效实体都不进图")

    totals = {str(k): int(v) for k, v in payload["totals"].items()}
    if totals.get("companion") != 2:
        fail("汇总 companion 应为 2，实际 %s" % totals)
    if totals.get("res") != 5:
        fail("汇总 res 应为 5（两种浆果丛各一、草、树、石头），实际 %s" % totals)
    print("汇总 OK  %s" % totals)
    print("全部通过")


if __name__ == "__main__":
    main()
