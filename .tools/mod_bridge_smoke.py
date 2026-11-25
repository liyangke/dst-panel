"""助手 MOD 的离线冒烟测试：用真 Lua 解释器跑 MOD 代码，游戏侧 API 用桩件顶替。

为什么要这一层：MOD 只能在专用服务端里加载，而服务端在云上、启动一次要几分钟。
把 panel_bridge / panel_json 的语义在本机验掉（协议往返、去重、JSON 合法性、
Lua 5.1 语法），云上那次启动只需要再验"文件出现在预期目录"这一件事。

用法：python .tools/mod_bridge_smoke.py
依赖：pip install lupa（lupa.lua51 的语义最接近 DST 内嵌的 LuaJIT）
"""

import json
import os
import shutil
import sys
import tempfile

try:
    from lupa.lua51 import LuaRuntime
except ImportError:  # 有些 lupa 版本没单独暴露 lua51
    from lupa import LuaRuntime

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MOD_SRC_DIR = os.path.join(ROOT, "internal", "modbridge", "mods", "DSTPanelHelper")
# 相对 MOD 根目录的路径。panel_* 必须在 scripts/ 下：Klei 只把
# MODS_ROOT..modname.."\\scripts\\?.lua" 拼进 package.path，放 MOD 根目录 require 不到。
MOD_FILES = ["modinfo.lua", "modmain.lua", "scripts/panel_json.lua", "scripts/panel_bridge.lua"]

failures = []


def check(name, cond, detail=""):
    if cond:
        print("  ok    %s" % name)
    else:
        failures.append(name)
        print("  FAIL  %s  %r" % (name, detail))


def bs(text):
    """Lua 侧全是字节串，比较时也要用字节。"""
    return text.encode("utf-8")


def posix(path):
    return path.replace("\\", "/").rstrip("/") + "/"


class FakeGame:
    """只实现 MOD 真会用到的那几个引擎接口，调用形状对着服务端自带脚本抄。

    TheSim 的相对路径基准是分片的 save 目录，所以这里以 save_dir 为根；
    文件按字节读写，和 Lua 一样不做编码转换（MOD 传的是 UTF-8 字节串）。
    """

    def __init__(self, save_dir):
        self.save_dir = save_dir
        self.announced = []
        self.kicked = []

    # 云机 xxd 实测到的引擎文件头。
    HEADER = b"KLEI     1 "

    @classmethod
    def _strip_header(cls, data):
        if data.startswith(cls.HEADER):
            return data[len(cls.HEADER):]
        return data

    def _full(self, path):
        if isinstance(path, bytes):
            path = path.decode("utf-8")
        return os.path.join(self.save_dir, os.path.normpath(path))

    def set_persistent_string(self, path, text, encode=False):
        # 引擎实测行为（2026-10-31 云机）有两条必须照抄，否则本地"通过"是假的：
        # * 写出的文件开头带 11 字节文本头 "KLEI     1 "，读的时候引擎自己剥；
        # * 不创建多级目录。父目录不存在时整次写入失败，日志里只有一句
        #   "[CRITICAL] Failed to save file"——MOD 原先写 panel/status.json 就是这么
        #   全军覆没的，而 TheSim.MakeDirectory 在这个构建里是 nil，它也没法自己建。
        full = self._full(path)
        if not os.path.isdir(os.path.dirname(full)):
            return
        data = text if isinstance(text, bytes) else str(text).encode("utf-8")
        with open(full, "wb") as f:
            f.write(self.HEADER + data)

    def get_persistent_string(self, path, cb):
        full = self._full(path)
        if not os.path.exists(full):
            # 游戏里读不到文件时回调的 success 是 false，这里保持一致
            cb(False, bs(""))
            return
        with open(full, "rb") as f:
            cb(True, self._strip_header(f.read()))

    def read(self, rel):
        full = self._full(rel)
        if not os.path.exists(full):
            return None
        with open(full, "rb") as f:
            return self._strip_header(f.read())


def build_lua(game, mods_root, mod_name):
    # encoding=None：Lua 字符串一律按字节进出，和 DST 里 MOD 读到的原始字节一致。
    # 代价是给 Lua 表设键必须用 bytes，用 str 设键不会报错但设不进去（踩过）。
    lua = LuaRuntime(unpack_returned_tuples=True, encoding=None)
    g = lua.globals()

    # 引擎桩件用"Lua 侧写调用形状、Python 侧干活"的方式接：
    # 桥里是 TheSim:SetPersistentString(...) 冒号调用，会多传一个 self，
    # 所以在 Lua 里显式收掉，Python 函数只保持纯粹的 (path, text) 形状。
    g.PY_SET = game.set_persistent_string
    g.PY_GET = game.get_persistent_string
    lua.execute(
        """
        TheSim = {}
        function TheSim:SetPersistentString(path, text, encode)
            return PY_SET(path, text)
        end
        function TheSim:GetPersistentString(path, cb)
            return PY_GET(path, cb)
        end
        """
    )

    net_factory = lua.eval("""
        function(announce, kick)
            return {
                Announce = announce,
                Kick = kick,
                GetSessionIdentifier = function() return "3AD15F911E034B3B" end,
                GetServerName = function() return "周末联机房" end,
                GetServerMaxPlayers = function() return 6 end,
            }
        end
    """)
    g.TheNet = net_factory(
        lambda _, text, *rest: game.announced.append(text),
        lambda _, userid, *rest: game.kicked.append(userid),
    )

    # 分片身份：modmain 用 TheShard:GetShardId()，冒号调用同样吃掉 self。
    # 桩件这里刻意返回 "1" 而不是 "Master"：云机实测 GetShardId 给的就是 server.ini
    # 里 [SHARD] id 的文本（地表 1、洞穴 2），早先桩件写死 "Master" 恰好替 MOD 掩盖了
    # "快照里自报 1" 这个 bug。现在这条桩逼着 modmain 走一遍真实的 id→目录名映射。
    g.TheShard = lua.eval("""
        function()
            return {GetShardId = function(self) return "1" end}
        end
    """)()

    g.TheWorld = lua.eval("""
        function()
            return {
                ismastersim = true,
                state = {day = 6, cycles = 5.5, season = "winter", phase = "dusk",
                         temperature = -8.2, wetness = 0},
            }
        end
    """)()

    # 两个玩家：第二个刻意没有 hunger 与 inventory 组件，用来验证缺字段不会被编成 0。
    g.AllPlayers = lua.eval("""
        function()
            local a = {
                name = "阿伟", prefab = "wilson", userid = "KU_abcdef123456",
                components = {
                    health = {currenthealth = 118.5, maxhealth = 150,
                              IsDead = function(self) return false end},
                    hunger = {current = 92, max = 150},
                    sanity = {current = 80, max = 200},
                    inventory = {NumItems = function(self) return 12 end},
                },
                Transform = {GetWorldPosition = function(self) return -120.7, 0, 88.3 end},
            }
            local b = {
                name = "小美", prefab = "wendy", userid = "KU_zzz999",
                components = {
                    health = {currenthealth = 0, maxhealth = 100,
                              IsDead = function(self) return true end},
                    sanity = {current = 30, max = 200},
                },
                Transform = {GetWorldPosition = function(self) return 12, 0, -7 end},
            }
            return {a, b}
        end
    """)()

    # 路径里有反斜杠，拼进 Lua 字符串字面量会变成非法转义；改成全局变量传进去。
    g.MODS_ROOT_TEST = posix(mods_root).encode("utf-8")
    g.MOD_NAME_TEST = bs(mod_name)
    # 照抄 Klei 的形状：mods.lua:528 拼的是 MODS_ROOT..modname.."\\scripts\\?.lua"，
    # 它的自定义 loader 再把反斜杠归一成斜杠（main.lua:142）。这里直接给归一后的形式。
    lua.execute(
        "package.path = MODS_ROOT_TEST .. MOD_NAME_TEST .. '/scripts/?.lua;' .. package.path"
    )
    return lua


def enqueue(game, *lines):
    """扮演面板：把新命令拼到队列末尾（Go 侧同样是整份原子重写）。

    面板是直接写文件的，不经过引擎，所以这里既不接 set_persistent_string 那条
    "加头 + 不建目录"的严格规则，写出来的就是裸文本——云上实测 Lua 的
    GetPersistentString 读这种无头文件完全正常，命令才走得住。
    """
    full = os.path.join(game.save_dir, "dstpanel_queue.txt")
    existing = b""
    if os.path.exists(full):
        with open(full, "rb") as f:
            existing = f.read()
    with open(full, "wb") as f:
        f.write(existing + bs("\n".join(lines) + "\n"))


def read_json(game, rel):
    raw = game.read(rel)
    if raw is None:
        return None
    return json.loads(raw.decode("utf-8"))


# 复刻 Klei 给 modmain 的沙箱 env：键表逐条对着 scripts/mods.lua:258-320 与
# scripts/modutil.lua 的 InsertPostInitFunctions 抄，刻意不包含 pcall/next/rawget/
# assert/setmetatable/package —— 真实 env 里就是没有（util.lua:788 的
# RunInEnvironment 只是裸 setfenv，没有 __index 回退）。
SANDBOX_FACTORY = """
function(cfg, modname, mods_root)
    local captured = {}
    local env = {
        pairs = pairs, ipairs = ipairs, print = print, math = math, table = table,
        type = type, string = string, tostring = tostring, require = require,
        Class = Class,
        GLOBAL = _G,
        modname = modname,
        MODROOT = mods_root .. modname .. "/",
        GetModConfigData = function(key, get_local_config) return cfg[key] end,
        AddPrefabPostInit = function(prefab, fn)
            captured.prefab = prefab
            captured.init = fn
        end,
    }
    env.env = env
    return env, captured
end
"""

RUN_IN_SANDBOX = """
function(src, env)
    local chunk, compile_err = loadstring(src)
    if chunk == nil then
        return false, "编译失败: " .. tostring(compile_err)
    end
    setfenv(chunk, env)
    local ok, err = xpcall(chunk, function(m) return tostring(m) end)
    return ok, err
end
"""


def main():
    save_dir = tempfile.mkdtemp(prefix="dstpanel-bridge-")
    # 工程目录名带中文，Lua 的 require 在 Windows 上按本地代码页找文件会找不着，
    # 所以把待测文件拷到纯 ASCII 的临时目录再加载；测的仍然是同一份内容。
    # 目录形状也照线上摆：mods_root/DSTPanelHelper/{modinfo,modmain}.lua + scripts/*.lua
    mods_root = tempfile.mkdtemp(prefix="dstpanel-mods-")
    mod_name = "DSTPanelHelper"
    mod_dir = os.path.join(mods_root, mod_name)
    try:
        for name in MOD_FILES:
            dst = os.path.join(mod_dir, name)
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            shutil.copy(os.path.join(MOD_SRC_DIR, name), dst)
        game = FakeGame(save_dir)
        lua = build_lua(game, mods_root, mod_name)

        print("Lua 版本:", lua.execute("return _VERSION"))
        print("save 根目录:", save_dir)

        # 1) 四个文件都要能被 Lua 5.1 编译（loadstring 本身就是 5.1 的标志）。
        compile_probe = lua.eval(
            "function(src) local fn, err = loadstring(src) return fn ~= nil, tostring(err) end")
        for name in MOD_FILES:
            with open(os.path.join(mod_dir, name), "rb") as f:
                ok, err = compile_probe(f.read())
            check("%s 能被 Lua 5.1 编译" % name, ok is True, err)

        panel_json = lua.eval("require('panel_json')")
        panel_bridge = lua.eval("require('panel_bridge')")
        new_bridge = lua.eval("function(cls) return cls.new({shard = 'Master'}) end")
        bridge = new_bridge(panel_bridge)
        # lupa 的 LuaTable 没有 call_method，方法调用要在 Lua 侧包一层
        tick = lua.eval("function(b, detail) return b:tick(detail) end")
        load_processed = lua.eval("function(b, done) return b:load_processed(done) end")
        noop = lua.eval("function() end")

        # 2) 空队列跑一轮：status.json 要落地，而且是合法 JSON。
        tick(bridge, True)
        snap = read_json(game, "dstpanel_status.json")
        check("第一轮就写出 status.json", snap is not None)
        if snap is None:
            print("\n桥没有写出快照，后面的断言没有意义")
            return 1
        check("快照自报分片", snap.get("shard") == "Master", snap.get("shard"))
        check("game_id 取自 GetSessionIdentifier", snap.get("game_id") == "3AD15F911E034B3B", snap.get("game_id"))
        check("中文服务器名没被截断", snap.get("server_name") == "周末联机房", snap.get("server_name"))
        check("世界状态齐全", snap["world"]["season"] == "winter" and snap["world"]["phase"] == "dusk"
              and abs(snap["world"]["temperature"] + 8.2) < 1e-6, snap.get("world"))
        check("天数与周期", snap["world"]["day"] == 6 and snap["world"]["cycles"] == 5.5, snap["world"])
        players = snap.get("players")
        check("players 是数组而不是对象", isinstance(players, list) and len(players) == 2, players)
        check("玩家血量与取整后的坐标", players[0]["health"] == 118.5 and players[0]["x"] == -121
              and players[0]["z"] == 88, players[0])
        check("缺组件的字段不出现，而不是被编成 0",
              "hunger" not in players[1] and "items" not in players[1], players[1])
        check("死亡标记", players[1]["is_dead"] is True, players[1])
        check("背包件数", players[0]["items"] == 12, players[0])
        check("玩家数与上限", snap.get("player_count") == 2 and snap.get("max_players") == 6, snap)

        # 3) 空玩家列表也要是 []，否则 Go 侧解成 struct 会报类型错。
        empty = lua.eval("function(json) return json.encode({list = json.array({})}) end")(panel_json)
        check("空数组编码成 []", json.loads(empty.decode("utf-8")) == {"list": []}, empty)

        # 4) 命令往返：公告文本里带竖线，验证拆开再拼回不丢内容。
        enqueue(game,
                "1000-1|announce|有玩家|在刷屏 请把麦闭上",
                "1000-2|kick|小美|刷屏",
                "1000-3|ping",
                "1000-4|nuke")
        tick(bridge, True)
        check("公告走到了 TheNet:Announce", game.announced == [bs("有玩家|在刷屏 请把麦闭上")], game.announced)
        check("踢人按 userid 走", game.kicked == [bs("KU_zzz999")], game.kicked)

        results = read_json(game, "dstpanel_result.json")
        check("四条命令都有回执", results is not None and len(results) == 4, list(results or {}))
        check("公告回执带原文", results["1000-1"]["ok"] is True
              and results["1000-1"]["value"] == "有玩家|在刷屏 请把麦闭上", results.get("1000-1"))
        check("ping 返回 pong", results["1000-3"]["value"] == "pong", results.get("1000-3"))
        check("未知命令被拒绝", results["1000-4"]["ok"] is False
              and "未知命令" in results["1000-4"]["error"], results.get("1000-4"))

        processed = (game.read("dstpanel_processed.txt") or bs("")).decode("utf-8").split("\n")
        check("执行过的 id 全部登记", processed == ["1000-1", "1000-2", "1000-3", "1000-4"], processed)

        # 5) 去重：队列还没被裁剪时，同一批命令不能重放。
        tick(bridge, True)
        check("重复 tick 不重放公告", len(game.announced) == 1, game.announced)
        check("重复 tick 不重放踢人", len(game.kicked) == 1, game.kicked)

        # 6) seq 递增，面板用它区分"新快照"与"同一份快照被重读"。
        first = read_json(game, "dstpanel_status.json")["seq"]
        tick(bridge, True)
        second = read_json(game, "dstpanel_status.json")["seq"]
        check("seq 每轮递增", second > first, (first, second))

        # 7) MOD 设置项 player_detail=false 时不该带玩家明细。
        tick(bridge, False)
        lite = read_json(game, "dstpanel_status.json")
        check("关闭明细时 players 缺席", "players" not in lite, list(lite))

        # 8) 重启场景：新 bridge 先读回 processed.txt，就不会把旧命令再执行一遍。
        revived = new_bridge(panel_bridge)
        load_processed(revived, noop)
        enqueue(game, "1000-1|announce|重启后又发了一遍")
        tick(revived, True)
        check("已处理命令不会重放", len(game.announced) == 1, game.announced)

        # 9) 采集异常不能打断桥：世界状态缺失时也要写出可解析的快照。
        lua.execute("TheWorld.state = nil")
        tick(bridge, True)
        broken = read_json(game, "dstpanel_status.json")
        check("世界缺失时仍有快照", broken is not None and broken.get("shard") == "Master", broken)

        # 10) 让 modmain 在"和 Klei 一样的沙箱 env"里真跑一遍。
        # 只编译是不够的：env 里没有 pcall/package 这类标准库，
        # 裸用一个不存在的全局在真服务端会让 MOD 被 DisableBecauseBad 永久禁用。
        sandbox_factory = lua.eval(SANDBOX_FACTORY)
        run_in_sandbox = lua.eval(RUN_IN_SANDBOX)

        env_probe = lua.eval("function(e) return e.pcall, e.package, e.next, e.rawget, e.setmetatable end")
        world_inst_src = """
            function()
                local w = {ismastersim = true}
                function w:DoTaskInTime(delay, fn) self.scheduled = fn return nil end
                function w:DoPeriodicTask(interval, fn, offset) self.periodic = fn self.period = interval return nil end
                return w
            end
        """
        make_world = lua.eval(world_inst_src)
        call0 = lua.eval("function(fn) return fn() end")

        # 10a) 先自证 fixture 有效：沙箱里确实没有这些标准库。
        cfg = lua.table()
        cfg[b"poll_interval"] = 2
        cfg[b"player_detail"] = True
        env, captured = sandbox_factory(cfg, bs(mod_name), posix(mods_root).encode("utf-8"))
        missing = env_probe(env)
        check("沙箱 env 里没有 pcall/package/next/rawget/setmetatable",
              all(v is None for v in missing), missing)

        # 10b) 反面对照：故意裸用 pcall 的 modmain 必须被判失败，
        # 否则 10c 的"通过"可能只是因为断言根本没跑。
        bad_ok, bad_err = run_in_sandbox(
            bs('local x = pcall(function() return 1 end)') , env)
        check("裸用 pcall 的 modmain 会被沙箱判错（对照）", bad_ok is False, bad_err)

        # 10c) 真 modmain：加载 -> 注册 world post-init -> 驱动任务 -> 看文件落地。
        # 注意：encoding=None 下 LuaTable 的键必须是 bytes，用 str 存取都对不上。
        lua.execute("TheWorld.state = {day = 6, cycles = 5.5, season = 'winter', phase = 'dusk',"
                    " temperature = -8.2, wetness = 0}")
        with open(os.path.join(mod_dir, "modmain.lua"), "rb") as f:
            modmain_src = f.read()
        ok, err = run_in_sandbox(modmain_src, env)
        check("modmain 在沙箱 env 里跑通", ok is True, err)
        check("modmain 注册了 world 的 PostInit", captured[b"prefab"] == bs("world"), captured[b"prefab"])

        world = make_world()
        call1 = lua.eval("function(fn, w) return fn(w) end")
        call1(captured[b"init"], world)
        check("间隔取 MOD 设置项的值（poll_interval=2）", world[b"period"] == 2, world[b"period"])

        # 面板此时投一条命令，完全由 modmain 装起来的桥来消费。
        enqueue(game, "2000-1|announce|modmain 装的桥")
        call0(world[b"scheduled"])
        call0(world[b"periodic"])
        snap = read_json(game, "dstpanel_status.json")
        check("modmain 装的桥写出了快照", snap is not None and snap.get("shard") == "Master", snap)
        check("modmain 装的桥执行了队列里的命令",
              game.announced and game.announced[-1] == bs("modmain 装的桥"), game.announced)
        wired = read_json(game, "dstpanel_result.json")
        check("modmain 装的桥回写了回执", wired.get("2000-1", {}).get("ok") is True, wired.get("2000-1"))

        print()
        if failures:
            print("失败 %d 项: %s" % (len(failures), ", ".join(failures)))
            return 1
        print("全部通过")
        return 0
    finally:
        shutil.rmtree(save_dir, ignore_errors=True)
        shutil.rmtree(mods_root, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
