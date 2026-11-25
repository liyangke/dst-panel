#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""本地界面开发服务器：在 Windows 上把面板跑起来，让浏览器能点得动总览页。

为什么需要它：进程状态全部来自 `screen -ls`，这台机器上没有 screen，
于是本地起的面板只能显示"问不到进程状态"那一条分支——在跑 / 没在跑 / 助手没回话 /
按钮随状态切换这些最要紧的界面形态一个都看不见。这里配一个 screen 替身
（.tools/fake_screen），只模仿面板会调的那几条命令。

它只碰本地临时目录：kleiRoot 用探针已经造好的 .tmp_boot/klei 夹具，
数据库与工作目录另起一份 .tmp_boot/uiwork，不会碰探针的库，更不碰云机。

令牌用假的那一句，且启动前会当场比对：万一键贴过真令牌的文件在这个目录里，
宁可直接退出也不要让界面开发服务器把它读一遍。
"""
import datetime
import io
import json
import math
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BOOT = os.path.join(ROOT, ".tmp_boot")
KLEI = os.path.join(BOOT, "klei")
DST = os.path.join(BOOT, "dst")
WORK = os.path.join(BOOT, "uiwork")
BIN = os.path.join(ROOT, ".tmp_bin")
EXE = os.path.join(BIN, "dstpanel.exe")
SCREEN_EXE = os.path.join(BIN, "screen.exe")
CFG = os.path.join(BOOT, "panel_ui.yaml")
PID = os.path.join(BOOT, "ui_dev.pid")
# Steam Web API key 不写进这个脚本，也不从命令行传（argv 会被别的进程看到）：
# 只从环境变量 DST_STEAM_KEY 或这份 0600 的本地文件读，文件在 .tmp_boot/ 里，不是产物。
KEYFILE = os.path.join(BOOT, "steam_key.txt")
GO = r"C:/coding/go/sdk/go1.23.1.windows-amd64/go/bin/go.exe"

PORT = 8963
BASE = "http://127.0.0.1:%d" % PORT
USER, PW = "admin", "admin"
FAKE_TOKEN_PREFIX = "pds-g^KU_fakeid001^"
# 这串是会话里被粘贴过的真令牌片段，只用来做"绝不能用在这里"的比对，绝不写进任何文件。
LIVE_TOKEN_MARK = "tvX4ySmb"

# 界面开发服务器读的是探针夹具里的那个房间；顺带把状态快照写成"刚刚采集"的。
CLUSTER = "Cluster_1"
SHARDS = ["Master", "Caves"]


def sh(cmd, cwd=ROOT):
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, encoding="utf-8", errors="replace")
    if p.returncode != 0:
        print("命令失败：%s\n%s\n%s" % (" ".join(cmd), p.stdout[-2000:], p.stderr[-2000:]))
        sys.exit(1)
    return p.stdout


def build():
    sh([GO, "build", "-o", SCREEN_EXE, "./.tools/fake_screen"])
    sh([GO, "build", "-o", EXE, "./cmd/dstpanel"])
    print("成品已重建：%s / %s" % (os.path.basename(EXE), os.path.basename(SCREEN_EXE)))


def check_token_files():
    """令牌只许是假的那一条。真的被粘进夹具的话这里必须拦住，而不是继续往外发。"""
    bad = []
    for root, _dirs, files in os.walk(KLEI):
        for name in files:
            if name != "cluster_token.txt":
                continue
            p = os.path.join(root, name)
            text = io.open(p, encoding="utf-8", errors="replace").read()
            # 三档：粘过的真令牌片段、真令牌形状但不是夹具那句假的 —— 都拦。
            # 探针（api_probe.py）在 ProbeWorld 里写的是 ProbeToken… 这种一眼假的
            # 占位串，不是 Klei 令牌形状，界面开发服务器读它没有泄露风险，放行；
            # 否则每次跑完探针都要手工改夹具才能起界面，实测就被卡死在这一步。
            if LIVE_TOKEN_MARK in text:
                bad.append(p)
            elif text.lstrip().startswith("pds-g^KU_") and FAKE_TOKEN_PREFIX not in text:
                bad.append(p)
    if bad:
        print("这些令牌文件不是夹具里那句假令牌，界面开发服务器拒绝起在它们上面：")
        for p in bad:
            print("  " + p)
        print("把它们改成 " + FAKE_TOKEN_PREFIX + "THIS_TEXT_MUST_NEVER_BE_BACKED_UP 再来。")
        sys.exit(1)


def write_status():
    """给两个分片各写一份"刚刚采集"的助手快照。

    故意留了形状差异：Caves 少一个玩家、Master 里第二个人没采到饥饿值（界面要显示"未知"
    而不是 0），并让一个玩家处于死亡状态。总览页的每种状态都得在这儿有样本，
    不然实测只能验到最好走的那一条。
    """
    now = time.time()
    for shard in SHARDS:
        d = os.path.join(KLEI, CLUSTER, shard, "save")
        if not os.path.isdir(d):
            os.makedirs(d)
        players = [
            {"name": "阿栗", "prefab": "wilson", "userid": "KU_fakeid001",
             "health": 118.0, "max_health": 150.0, "is_dead": False,
             "hunger": 92.5, "max_hunger": 150.0, "sanity": 66.0, "max_sanity": 100.0,
             "x": -128.5, "z": 233.25, "items": 14},
        ]
        if shard == "Master":
            players.append(
                {"name": "小满", "prefab": "wormwood", "userid": "KU_fakeid002",
                 "health": 0.0, "max_health": 150.0, "is_dead": True,
                 # 饥饿与理智刻意缺着：助手采不到这两个组件时接口给的是 null。
                 "hunger": None, "max_hunger": None, "sanity": None, "max_sanity": None,
                 "x": 40.0, "z": -12.0, "items": 0})
        payload = {
            "shard": shard,
            "seq": int(now),
            "game_id": "DCF8E1BCE43CE58A",
            "heartbeat": now,
            "server_name": "本地界面实测房" if shard == "Master" else "本地实测洞穴",
            "max_players": 6,
            # cycles 是引擎上报的"过完了几个整天"，界面那个第几天由面板 +1 换算：
            # 这里地表 20 → 第 21 天，洞穴 19 → 第 20 天。助手那份协议里没有 day 这个键。
            # 这一季的两个读数照秋天的 20 天摆：过完 5 天 → 界面说第 6 天，还剩 14 天，
            # 6 + 14 = 20 正好一季（第 6 天本身不在"还剩"里）。
            "world": {"cycles": 20 if shard == "Master" else 19,
                      "season": "autumn", "phase": "day",
                      "elapsed_days_in_season": 5, "remaining_days_in_season": 14,
                      "temperature": 18.6, "wetness": 0 if shard == "Master" else 12.4},
            "players": players,
            "player_count": len(players),
            "error": "",
        }
        with io.open(os.path.join(d, "dstpanel_status.json"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write(json.dumps(payload, ensure_ascii=False, indent="  "))
    # 顺手留一份旧快照给"数据过期"那条分支？没有：那个状态由 staleSeconds 控制，
    # 界面开发服务器把它调大，免得每次刷新都显示过期。


# —— 地图夹具 ————————————————————————————————————————————————
# 真实地图由助手 MOD 采样出来的是一张 144×144 的方格，不能手写一小块：
# 界面上的缩放、图例占比、点密度都只有在这个体量下才看得出问题。
# 这里造一座中间高、外圈海水的岛：Master 是森林/草原/岩石，Caves 是洞穴/蘑菇地，
# 点位按地形撒，基地固定在中心偏下，方便看"建筑和资源会不会糊成一团"。
MAP_GRID = 144
MAP_WORLD = 512
MAP_GAME_ID = "DCF8E1BCE43CE58A"


def _field(x, y, seed):
    """低频起伏：三个不同周期的正弦 + 一点方块噪声，得到像样的地形块，而不是椒盐点。"""
    n = (int(x * 374761393) + int(y * 668265263) + int(seed * 100) * 1442695041) & 0xFFFFFFFF
    n = ((n ^ (n >> 13)) * 1274126177) & 0xFFFFFFFF
    grain = ((n ^ (n >> 16)) & 0xFFFFFFFF) / 0xFFFFFFFF
    return (0.5 * math.sin(x / 11.0 + seed) +
            0.35 * math.sin(y / 7.3 + seed * 2.1) +
            0.25 * math.sin((x + y) / 17.0 + seed * 3.7) +
            0.2 * grain)


# 地形编号取自引擎自己的 scripts/constants.lua:686 GROUND 表，和面板 api/map.go
# 是同一份依据。早前这份夹具按另一版编号造，所以本地看久了和线上一直对不上——
# 夹具要是错的，浏览器实测就是在自欺。
G_IMPASSABLE = 1    # IMPASSABLE
G_ROAD = 2
G_ROCKY = 3
G_DIRT = 4
G_SAVANNA = 5
G_GRASS = 6
G_FOREST = 7
G_MARSH = 8
G_WEB = 9
G_CAVE = 13
G_FUNGUS = 14
G_UNDERROCK = 16
G_SHELLBEACH = 44
G_FARM = 47         # FARMING_SOIL
G_WALL_ROCKY = 151
G_OCEAN_COASTAL = 201
G_OCEAN_SWELL = 203
G_OCEAN_ROUGH = 204

# 类别与资源子类编号，要和 internal/modbridge/map.go 里的 Cat*/ResSub* 对上。
CAT_COMPANION = 4
SUB_TREE, SUB_SAPLING, SUB_GRASS, SUB_REEDS, SUB_BERRY = 1, 2, 3, 4, 5
SUB_MUSHROOM, SUB_FLOWER, SUB_CACTUS, SUB_STONE = 6, 7, 8, 9


def _tile_master(v, r, x, y, bx, by):
    if r > 1.06:
        return G_OCEAN_ROUGH                     # 远海
    if r > 1.0:
        return G_OCEAN_COASTAL                   # 近岸浅水
    if r > 0.93:
        return G_SHELLBEACH                      # 贝壳滩
    if abs(y - by) <= 1 and x >= bx - 2:
        return G_ROAD                            # 从基地往东的一条通道
    if abs(x - bx) <= 1 and abs(y - by) <= 22:
        return G_ROAD                            # 基地南北向的一小段
    if bx + 4 <= x <= bx + 8 and by - 8 <= y <= by - 4:
        return G_FARM                            # 基地边上的耕地（翻过的地）
    if (x - 104) ** 2 + (y - 46) ** 2 < 16:
        return G_WEB                             # 蛛网地
    if v > 0.62:
        return G_FOREST                          # 森林
    if v > 0.34:
        return G_GRASS                           # 草地
    if v > 0.04:
        return G_SAVANNA                         # 稀树草原
    if v > -0.28:
        return G_DIRT                            # 泥地
    if v > -0.56:
        return G_ROCKY                           # 岩石地
    return G_MARSH                               # 沼泽


def _tile_caves(v, r, x, y):
    if r > 1.05:
        return G_IMPASSABLE                      # 不可通行
    if r > 0.99:
        return G_WALL_ROCKY                      # 洞穴壁
    if v > 0.55:
        return G_FUNGUS                          # 蘑菇地
    if v > -0.18:
        return G_CAVE                            # 洞穴地面
    if v > -0.55:
        return G_ROCKY                           # 岩石地
    return G_UNDERROCK                           # 底层岩


def write_map():
    now = time.time()
    for si, shard in enumerate(SHARDS):
        seed = 3.1 if shard == "Master" else 8.7
        # 基地放在网格正中：界面上的"放大"是以画布中心为锚点的，基地挪在中心
        # 才能让 复位→连点放大 一直停在同一片格子上，否则验高倍率时视野会飘走。
        bx, by = MAP_GRID // 2, MAP_GRID // 2
        tiles = []
        cells = {}
        subs = {}
        for y in range(MAP_GRID):
            for x in range(MAP_GRID):
                r = math.hypot(x - MAP_GRID / 2, y - MAP_GRID / 2) / (MAP_GRID * 0.58)
                v = _field(x, y, seed)
                tile = _tile_master(v, r, x, y, bx, by) if shard == "Master" else _tile_caves(v, r, x, y)
                tiles.append(tile)

                def add(cat, n=1, sub=0):
                    cur = cells.setdefault((x, y), [0, 0, 0, 0])
                    cur[cat - 1] += n
                    if cat == 2 and sub:
                        s = subs.setdefault((x, y), {})
                        s[sub] = s.get(sub, 0) + n

                # 基地：一圈木墙、科学机器、火堆、箱子，再加上几块农田。
                if shard == "Master":
                    if (x, y) in ((bx, by - 3), (bx - 3, by), (bx + 3, by), (bx, by + 3)):
                        add(1, 1)
                    if abs(x - bx) + abs(y - by) == 6 and (x + y) % 2 == 0:
                        add(1, 1)
                    if (x, y) == (bx + 1, by - 1):
                        add(1, 2)
                    if (x, y) == (bx - 2, by + 2):
                        add(1, 1)
                    # 切斯特/格罗姆这种跟着人的伙伴：底图里它是导出那一刻的位置，
                    # 和每秒刷新的玩家点不是一回事，界面上另有说明。
                    if (x, y) in ((bx, by), (bx - 1, by + 1), (bx + 2, by - 2)):
                        add(CAT_COMPANION, 1)
                    # 刻意在基地旁放几个"数量很大、而且混了两种资源"的格子：
                    # ×N 角标只在数量够大时才出现，子类名又要一格里有多种才看得出会并排。
                    if (x, y) in ((bx + 2, by + 2), (bx - 2, by - 1), (bx + 1, by + 3)):
                        add(2, 12, SUB_BERRY)
                        add(2, 3, SUB_TREE)
                    if (x, y) in ((bx - 3, by - 3), (bx + 4, by), (bx, by - 4)):
                        add(2, 5, SUB_GRASS)
                # 资源按地形撒：同一种东西聚成片，图上才有"林区/矿区"的读法。
                h = _field(x * 1.7, y * 1.7, seed + 4)
                if tile == G_FOREST and h > 0.45:
                    add(2, 1 + int((h - 0.45) * 12), SUB_TREE)
                elif tile == G_GRASS and h > 0.62:
                    add(2, 1 + int((h - 0.62) * 8), SUB_BERRY)
                elif tile == G_ROCKY and h > 0.55:
                    add(2, 1 + int((h - 0.55) * 10), SUB_STONE)
                elif tile == G_SAVANNA and h > 0.72:
                    add(2, 1 + int((h - 0.72) * 6), SUB_SAPLING)
                elif tile == G_MARSH and h > 0.58:
                    add(2, 1 + int((h - 0.58) * 6), SUB_REEDS)
                elif tile in (G_CAVE, G_FUNGUS) and h > 0.6:
                    add(2, 1 + int((h - 0.6) * 7), SUB_MUSHROOM)
                # 危险：远离基地，数量少但零星分布。
                hd = _field(x * 2.3, y * 2.3, seed + 9)
                if tile == G_FOREST and hd > 0.86:
                    add(3, 1)
                elif tile == G_MARSH and hd > 0.82:
                    add(3, 1)
                elif tile == G_SAVANNA and hd > 0.9:
                    add(3, 1)
                elif tile == G_WEB and hd > 0.5:
                    add(3, 1)

        # 夹具按新版助手的格式写：逐个实体一条，坐标是引擎 tile（不是 144 网格）。
        # 一格里的多个实体要在格内散开，否则高倍率贴图会全叠在同一个点上，
        # 看起来就像只有一个。抖动用坐标算术，不用 hash()：后者带进程盐，每次跑图会长得不一样。
        ents = []
        totals = {"build": 0, "res": 0, "danger": 0, "companion": 0}
        per_cat = {1: "build", 2: "res", 3: "danger", CAT_COMPANION: "companion"}

        def emit(cat, sub, n, x, y):
            for i in range(n):
                if len(ents) >= 12000:
                    continue  # 只丢位置，计数照旧累加，和助手那边同一个道理
                jx = ((x * 31 + y * 17 + i * 7 + cat * 3 + sub) % 10 + 0.5) / 10.0
                jy = ((x * 13 + y * 29 + i * 11 + cat * 5 + sub) % 10 + 0.5) / 10.0
                tx = min(max(int((x + jx) * MAP_WORLD / MAP_GRID), 0), MAP_WORLD - 1)
                ty = min(max(int((y + jy) * MAP_WORLD / MAP_GRID), 0), MAP_WORLD - 1)
                ents.append([tx, ty, cat, sub])

        for (x, y), (b, r, d, cp) in sorted(cells.items()):
            for cat, n in ((1, b), (3, d), (CAT_COMPANION, cp)):
                if n:
                    emit(cat, 0, n, x, y)
                    totals[per_cat[cat]] += n
            kinds = subs.get((x, y)) or {}
            for sub in sorted(kinds):
                emit(2, sub, kinds[sub], x, y)
                totals["res"] += kinds[sub]
            if r and not kinds:
                emit(2, 0, r, x, y)
                totals["res"] += r
        truncated = len(ents) >= 12000
        payload = {
            "shard": shard,
            "at": int(now),
            "game_id": MAP_GAME_ID,
            "width": MAP_WORLD,
            "height": MAP_WORLD,
            "grid": MAP_GRID,
            "tiles": tiles,
            "ents": ents,
            "totals": totals,
            "truncated": truncated,
        }
        d = os.path.join(KLEI, CLUSTER, shard, "save")
        if not os.path.isdir(d):
            os.makedirs(d)
        with io.open(os.path.join(d, "dstpanel_map.json"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write("KLEI     1 ")
            fh.write(json.dumps(payload, ensure_ascii=False, separators=(",", ":")))
    print("地图夹具已写入两个分片（%d×%d 网格）" % (MAP_GRID, MAP_GRID))


# —— 日志夹具 ——————————————————————————————————————————————————
# 为什么把规则表里每一类都塞进同一份文件：日志页的四种形态——三种颜色的级别徽标、
# 按事件筛、聊天行把发言者拆出来、超长行截断——只有在一份文件里同时出现，
# 一次浏览器实测才看得完。代价是它不是一轮真实启动的时间线（真实的一轮不会
# 既有世界生成失败又有玩家进服），所以样本段前有一行分隔把这件事写在界面上。
WEEKDAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]
MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun",
          "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]

# 形状合法的假令牌。真令牌永远不进这个文件；它在这里的唯一用途是验脱敏那一条：
# 引擎绝不该打这行，但日志会经由接口、SSE、探针输出好几条路离开进程，必须有样本挡着。
FAKE_LOG_TOKEN = ("pds-g^KU_fakeid001^AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
                  "^BBBBBBBBBBBBBBBBBBBBBBBBBBBB")


def ansic(dt):
    """引擎的 Current time 行是 ANSIC 形态的本地时间，日是一位数时前面补空格。

    不能用 %a/%b：这台机器的 locale 是中文，它们会给"周四"和"10月"，
    而面板按 time.ANSIC 解，解不出来就等于整份日志没有时间。
    """
    return "%s %s %2d %02d:%02d:%02d %d" % (
        WEEKDAYS[dt.weekday()], MONTHS[dt.month - 1], dt.day,
        dt.hour, dt.minute, dt.second, dt.year)


def stamp(secs):
    return "[%02d:%02d:%02d]: " % (secs // 3600, (secs // 60) % 60, secs % 60)


def render(lines):
    """把 (相对秒数, 正文) 拼成日志文本；正文为空就是引擎那种空白行。"""
    return "".join(stamp(s) + t + "\n" if t else "\n" for s, t in lines)


def boot_current_time():
    """这一轮往日志头上写的那个时刻：上一份还新鲜就照抄，落盘太久才按"现在减 105 分钟"重造。

    为什么必须照抄：面板把每条行为换算成墙上时刻、以及"这一轮"是哪个 round，用的都是
    这一行（internal/gamelog/reader.go 的 Start → internal/monitor/events.go 里的 base
    与 Round），而事件表的唯一键是 (cluster, shard, round, at, event, userid, name)。
    真引擎每轮启动会截断重写日志并打一个新时刻，所以同一批内容只可能属于一轮；
    而验收一轮里 `--reseed` 要跑十几遍，每遍都现算"现在减 105 分钟"的话，同一批行为行
    就被写成两三份 round 不同、at 只差十几秒的行，唯一键谁也撞不上谁，
    玩家时间线上凭空多出成对的死亡/变幽灵（players125_check 报的就是这个：
    红的是夹具的重写，不是面板的合并判定）。
    照抄之后重复的 reseed 每次 at 和 round 都完全一样，ON CONFLICT DO NOTHING 正好把
    它吸收掉——这才是引擎"同一份日志"的语义。
    还是要重造的情形只有一种：夹具从上一次开柜到现在搁了太久，日志里的时刻已经掉出
    各条检查默认的那个 24 小时窗口，界面会看着"一间房都没有人"。用文件落盘时间判，
    比解析 ANSIC 那串省事，也不用跟本机中文 locale 抢星期名。
    """
    fresh = ansic(datetime.datetime.now() - datetime.timedelta(minutes=105))
    for shard in SHARDS:
        p = os.path.join(KLEI, CLUSTER, shard, "server_log.txt")
        if not os.path.isfile(p):
            continue
        head = None
        with io.open(p, encoding="utf-8", errors="replace") as fh:
            lines_head = fh.readlines()[:20]
        # 只看前 20 行，和 reader.go 的 headBytes 窗口一个口径。
        for line in lines_head:
            m = re.search(r"Current time: (.+?)\s*$", line)
            if m:
                head = m.group(1)
                break
        if head and time.time() - os.path.getmtime(p) < 6 * 3600:
            return head
        return fresh
    return fresh


def boot_lines(shard, current):
    """本轮启动到进世界这一段，两个分片各有一份真实会打出来的行。

    current 是已经拼成 ANSIC 形态的那串时刻，由 boot_current_time() 决定，
    这里不再自己算——算一次就多抖一次。
    """
    root = path(os.path.join(KLEI, CLUSTER, shard)) + "/"
    port = 10999 if shard == "Master" else 11000
    out = [
        (0, "PersistRootStorage is now %s " % root),
        (0, "Current time: %s" % current),
        (0, "Version: 760944"),
        (0, "[WARNING] -console has been deprecated: Use the [MISC] / console_enabled setting instead."),
        (1, "Starting Up"),
        (1, ""),
        (2, "Serving shards from %s" % path(KLEI)),
        (3, "[Shard] Telling Client our new session identifier: DCF8E1BCE43CE58A"),
        (4, "[200] Account Communication Success (6)"),
        (4, "Received (KU_fakeid001) from TokenPurpose"),
        (5, "[Shard] Starting master server and waiting for connections"),
        (6, "Shard server started on port %d" % port),
        (7, "Overwriting savedata with override file."),
        (8, "PlayerDeaths could not load morgue"),
        (9, "[ModIndex] Registering Mods:"),
        (9, "modoverrides.lua enabling workshop-3781609700"),
        (10, "SUCCESS: Loaded modoverrides.lua (1)"),
        (10, "applying configuration_options from modoverrides.lua to mod workshop-3781609700"),
        (11, "[ModIndex] ModIndex: Load sequence finished successfully."),
        (12, "api_version for workshop-9999999777's modinfo.lua is 1.0.0, "
             "which is incompatible with our version: Don't Starve Together (151542)."),
        (12, "WARNING loading modinfo.lua: SomeRandomMod does not specify a configuration_options table"),
        (13, 'MOD ERROR: workshop-9999999777 has an unknown configuration option "colour"'),
        (14, '[OnLoadPermissionList] OnLoadPermissionList: "%s/adminlist.txt" (2 entries)' % root),
    ]
    if shard == "Caves":
        out += [
            (15, "[Shard] Connecting to master..."),
            (16, "[Shard] Secondary shard is waiting for LUA..."),
        ]
    else:
        out += [(15, "[OnLoadPermissionList] OnLoadPermissionList: \"%s/blocklist.txt\" (0 entries)" % root)]
    out += [
        (17, "[Shard] secondary shard LUA is now ready!"),
        (18, "[SyncWorldSettings] Resyncing master world option day_length = 20 to secondary shards."),
        (19, "[100] Validating portal [4417]: (108.62, 0.00, -359.34)"),
        (20, "[Shard] Skipping portal check for teleport destination 4417"),
        (21, "Sim unpaused"),
        # 玩家事件表那四列（IP / Steam ID / 账户编号 / 角色）不是每一行自己写的，
        # 而是面板把这一轮里"连接 → 验证 → 落地"的三条线索接到同一个人身上凑出来的。
        # 形状取自云机 server_log.txt：同一刻两条连接行都会打，guid 在三条行里对得上，
        # netid 那一槽就是 Steam ID（自机行是空的，所以那一条不给 netid）。
        # IP 用 RFC5737 的文档地址段，编号和 Steam ID 是编的，不指向任何真实账号。
        (25, "New incoming connection 203.0.113.11|16537 <5242766981745090001>"),
        (25, "Client connected from 203.0.113.11|16537 <5242766981745090001>"),
        (25, "[ClientObject] Added (pre-auth) on server: guid=5242766981745090001"),
        (25, "Client authenticated: (KU_fakeid001) 阿栗"),
        (26, "[ClientObject] Initialized (authenticated) on server: guid=5242766981745090001 "
             "userid=KU_fakeid001 netid=76561198800000011 admin=1"),
        (27, "Spawn request: wilson from 阿栗"),
        (28, "New incoming connection 203.0.113.12|17004 <5242766981745090002>"),
        (28, "Client connected from 203.0.113.12|17004 <5242766981745090002>"),
        (28, "[ClientObject] Added (pre-auth) on server: guid=5242766981745090002"),
        (28, "Client authenticated: (KU_fakeid002) 小满"),
        (29, "[ClientObject] Initialized (authenticated) on server: guid=5242766981745090002 "
             "userid=KU_fakeid002 netid=76561198800000012 admin=0"),
        (30, "Spawn request: wendy from 小满"),
        # 第三条只演"验证行给的是数字而不是显示名"这种账户：他没有连接行，
        # 于是事件表里 IP 和 Steam ID 两栏就该是空的——界面得有个真实的空样例可看。
        (31, "Client authenticated: (KU_fakeid003) 14672355439136578013"),
        (26, "小满 has joined the game"),
        (27, "[GameMode] DoWork: SaveGame"),
        (60, 'RemoteCommandInput: "c_save()"'),
        (61, "Serializing world: session/DCF8E1BCE43CE58A/0000000085"),
        (62, "Truncating to snapshot #85"),
        (90, "Available disk space for save files: 41209 MB"),
        (95, '[Http] Curl failed[1] with HTTP_500, retrying (2 times). Response: _{"Error":{"Code":"E_ROWID_EXIST"}}_'),
        (96, "[Error] Master Server Broadcast Error: E_ROWID_EXIST"),
        (120, "阿黎 has left the game"),
        # 玩家行为的那几句：句子部分用的是引擎自带文案表里的原文
        # （scripts/strings.lua 第 10753~10765 行，2026-11-03 在云机核对），
        # 踢人那条带 [Kick] 标签，因为那是服务端二进制里写死的形状。
        # 事件表要演的是"谁死过、谁被踢过"，只给进出的话那几类事件永远不出现在界面上。
        (126, "小满 went away for a bit."),
        (130, "小满 was killed by SpiderdenGuardian. They became a spooky ghost!"),
        (138, "阿黎 lost all of their humanity and is gone forever."),
        (145, "[Kick] (KU_fakeid001) 阿栗 kicked (KU_fakeid002) 小满"),
        # 封禁那条：事件表的"动作"下拉里有「有人被封禁」这一档，界面就得真给得出一次，
        # 否则用户筛它是空表、筛完还以为是这个房间没人被封禁过。
        # 顺序也按真实流程演：阿黎先离开（120 行），管理员事后补一封。
        (146, "[Ban] (KU_fakeid001) 阿栗 banned (KU_fakeid003) 阿黎"),
        (148, "小满 has returned from their relaxing time away."),
        (150, "Sim paused"),
        (152, "小满 was resurrected by 阿栗."),
    ]
    return out


def noise_lines(count, from_secs):
    """世界跑着跑着就会反复写的那几行（存档落盘一类），插在行为行之前。

    这一堆不是为了演规则表，而是为了让"回看多少行"这一档有东西可切：
    真实 server_log 一晚上就是几千行，夹具只有四十几行的话，
    界面把窗口收到 200 行永远切不掉任何内容，那句"更早的行为没算进来"
    和 scanned 到底准不准就永远验不到。
    """
    out = [(from_secs, "—— 从这里起是界面夹具补的日常落盘行：世界跑一段时间，"
                       "引擎就会反复写这几句，不是这个房间特有的事 ——")]
    for i in range(count - 1):
        s = from_secs + 1 + i // 12
        kind = i % 4
        if kind == 0:
            out.append((s, "Serializing world: session/DCF8E1BCE43CE58A/%010d" % (86 + i // 4)))
        elif kind == 1:
            out.append((s, "[GameMode] DoWork: SaveGame"))
        elif kind == 2:
            out.append((s, "Truncating to snapshot #%d" % (86 + i // 4)))
        else:
            out.append((s, "Available disk space for save files: %d MB" % (41209 - i)))
    return out


# 每一类规则的样本各来一行，顺序按界面上一眼扫过去要好读：先说明这下面是夹具，
# 再报错、警告、其余形态。
# 一条玩家真的会打出来的长发言（超过 160 字）。它存在的唯一理由：事件表"说明"那一列的
# 「展开全文」只给长行用，夹具里没有长行的话，这一枚控件在本机一次都没被点过。
LONG_CHAT_TEXT = (
    "谁能来帮忙看一下坐标：从传送门出来往北走两百步有一片蜘蛛森林，中间那个巢穴我上周打过一次"
    "它这周又长回来了，右边那条河的上游住着一群牛，牛旁边的石地上有切斯主的遗迹可入口被三棵树挡着，"
    "我要先把树砍掉再回来搬石头，谁缺木材自己过来这边拿，另外别踩我种在营地门口的那片浆果，"
    "昨天晚上有小狐狸把最边上一丛拱掉了，我看着心疼但没抓手所以只能来群里说一句"
)


def sample_lines():
    filler = "workshop-" + "-".join("%04d" % (700000 + i) for i in range(90))
    return [
        (3600, "—— 上面是这一轮启动真的会打出来的行；从这一行起是界面夹具，"
               "把面板日志规则表的每一类都演一遍，不是这个房间发生过的事 ——"),
        (3601, "!!!! Your Server Will Not Start !!!!"),
        (3601, "No auth token could be found."),
        (3602, "[string \"../scripts/worldgen_main.lua\"]:112: Must provide complete level data to worldgen."),
        (3603, "[string \"../scripts/map/level.lua\"]:92: Must specify the task set for a level!"),
        (3604, "[string \"../scripts/map/level.lua\"]:34: attempt to get length of field 'random_set_pieces' (a nil value)"),
        (3605, "[string \"../scripts/map/storygen.lua\"]:865: Must specify a layout mode for your level."),
        (3606, "Worldgen had an error, the leveldataoverride.lua is probably broken"),
        (3607, "Level data override is invalid!"),
        (3608, "PANIC: extracting annotation string"),
        (3609, "[string \"../scripts/update.lua\"]:229: Update() called on paused server!"),
        (3610, "[Error] Failed to load ../mods/workshop-0000000000/modinfo.lua"),
        (3611, "[Error] 这一行引擎只记成错误，没有任何可认的措辞，面板只能说不出是什么错"),
        (3612, "[WARNING] 这一行同理，引擎记成警告"),
        (3613, "Server Autopaused"),
        (3614, "Server Paused"),
        (3615, "Server Unpaused"),
        (3616, "token=%s" % FAKE_LOG_TOKEN),
        (3617, "Registering Mods: %s" % filler),
        (3618, ""),
        (3618, ""),
        (3619, "[AnonymousSafeThread:149225] " + "Suspected mod abuse detected; this filler line exists so the "
         "panel has to prove it cuts an over-long line instead of handing kilobytes to the browser. " * 8),
    ]


def write_logs():
    """给两个分片各写一份 server_log.txt 与 server_chat_log.txt。

    引擎每轮启动都会截断重写这两份文件，路径就是 <分片目录>/server_log.txt
    （见 internal/dst/paths.go），面板读的是这一份而不是 backup/ 里的旧副本。
    起始时间照上一份的 Current time 抄，抄不到才取一小时四十五分前，界面上那些
    "几分钟前"才像一份跑了一段时间的日志（为什么不能每次现算，见 boot_current_time）。
    """
    current = boot_current_time()
    for shard in SHARDS:
        d = os.path.join(KLEI, CLUSTER, shard)
        if not os.path.isdir(d):
            os.makedirs(d)
        lines = boot_lines(shard, current)
        # 把落盘噪音插在"阿黎 has left"之前：行为行必须留在文件尾部，
        # 否则界面把窗口收到 200 行时会连行为一起切掉，那一屏就变成空表。
        cut = next(i for i, (s, _) in enumerate(lines) if s >= 120)
        lines = lines[:cut] + noise_lines(180, 97) + lines[cut:]
        if shard == "Master":
            lines += sample_lines()
        with io.open(os.path.join(d, "server_log.txt"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write(render(lines))

        chat = [
            # 两条公告抄本的时钟跟服务端那一行一致（同一个进程往两个文件里打，时钟同源）。
            # 标签两种各演一条：[Join Announcement] 是服务端二进制里的模板形状，面板把它
            # 认成"玩家进服"，于是和服务端那份合成一记——头部那句"抄本合掉了 N 条"就是这么
            # 来的；[Announcement] 是实测见过的通用公告标签，它不算玩家行为。
            # 引擎在聊天文件里到底用哪种标签还没在真机抓到过样本，所以两种都留着：
            # 不管走哪条路，"玩家进服"都只该是一记。
            (26, "[Join Announcement] 小满 has joined the game"),
            (30, "[Announcement] Server is now running. Join from the server list or by cluster."),
            (48, "[Announcement] 面板首启验证：这条中文公告来自管理面板"),
            (60, "(KU_fakeid002) 小满：有人在洞穴吗"),
            (66, "(KU_fakeid001) 阿栗：我在森林这头，走传送门过去要两分钟"),
            (70, "阿黎(KU_fakeid003)：别砍我种的那片树"),
            (75, "(KU_fakeid002) 小满：好，我只采浆果"),
            (80, ""),
            # 故意留一条超长发言：界面上"展开全文"那一枚只给超过 160 字的原文行用，
            # 夹具里没有这种行的话，那一枚控件在本机永远走不到，写多少验收都测不到它。
            (85, "(KU_fakeid002) 小满：" + LONG_CHAT_TEXT),
            (90, "(KU_fakeid001) 阿栗：天黑了，先躲一躲"),
            # 离服公告在聊天文件里是带标签的那种写法，正文只有名字；
            # 服务端文件里同一刻是整句 "阿黎 has left the game"。同一秒、同一个人、
            # 同一种进出，面板合成一记（见 gamelog.Events 里那把合抄本的键）。
            (120, "[Leave Announcement] 阿黎"),
        ]
        if shard == "Caves":
            chat = chat[:4] + [(61, "(KU_fakeid003) 阿黎：洞穴里太黑了，谁能给我个火把")] + chat[4:]
        with io.open(os.path.join(d, "server_chat_log.txt"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write(render(chat))


def seed_sessions():
    state = os.path.join(WORK, "fake_screen.json")
    sessions = [{"name": "dst-%s-%s" % (CLUSTER, s), "pid": 90001 + i} for i, s in enumerate(SHARDS)]
    with io.open(state, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(json.dumps(sessions, indent="  "))
    os.environ["DST_FAKE_SCREEN"] = state
    return state


def write_mods():
    """在模组目录里放三个工坊模组，让检索页第一次打开就有默认列表可看。

    为什么非要工坊模组（workshop-数字 这种目录名）：面板只把带工坊 ID 的模组写进
    mods_seen 缓存（internal/api/modsearch.go 的 rememberDiskMods），本地目录模组
    没有 ID、进不了缓存，检索页的默认列表就永远是空的——那样这一页最有信息量的
    那条路径在本地一次都没被走过。

    modinfo.lua 的写法照引擎契约来：文件本身是一串顶层裸赋值，不是 return 一张表；
    description 用 [[ ]] 长字符串，tags 是字符串数组。样本各占一种状态：
    纯服务端、玩家也要订阅、带子模组依赖、名字写成算出来的表达式（多语言模组的常见写法，
    面板不执行 Lua，这一行只能按目录名显示）。
    """
    root = os.path.join(DST, "mods")
    samples = [
        ("900000001", "Stack Size Configurable", "3.2.1", "server_only = true",
         "把木头、石头这类可堆叠物品的上限调到 999，按物品种类分别可调。"
         "只影响服务端，玩家那边不用订阅。改了数值之后老存档里地上的物品要重新捡起才会合并。",
         '{ "QoL", "Gameplay" }', ""),
        ("900000002", "小地图共享（队友位置）", "1.4", "all_clients_require_mod = true",
         "在小地图上显示同队玩家的位置与生命值，玩家自己也得到工坊订阅它，"
         "否则服务端装了没效果、没订阅的人会直接被断线。",
         '{ "Map Info", "Utility Mod" }', ""),
        ("900000003", "Auto Stack 自动整理背包", "2024-11-08", "server_only = false",
         "每隔一段时间把玩家背包里的同类物品叠到一起，顺带把箱子整理一遍。"
         "依赖下面的库模组，两个 ID 都要在清单里，缺一个服务端会直接报错退出。",
         '{ "QoL" }', '\nmod_dependencies = {\n    { workshop = "workshop-900000001" },\n}\n'),
        ("900000004", "", "1.0.3", "server_only = true",
         "这一位的 name 不是字符串，而是按客户端语言算出来的：面板只解析不执行 Lua，"
         "所以读不出模组自己写的名字，界面上会按目录名显示。",
         '{ "QoL" }', ""),
    ]
    made = 0
    for wid, name, ver, kind, desc, tags, deps in samples:
        d = os.path.join(root, "workshop-" + wid)
        f = os.path.join(d, "modinfo.lua")
        if os.path.exists(f):
            continue
        if not os.path.isdir(d):
            os.makedirs(d)
        # 第四位的名字走 ChooseTranslationTable 这种表达式，其余都是裸字符串赋值。
        name_line = ('name = ChooseTranslationTable({ en = "Multi Language Mod", '
                     'zh = "多语言模组" })\n'
                     if name == "" else 'name = "%s"\n' % name)
        with io.open(f, "w", encoding="utf-8", newline="\n") as fh:
            fh.write('%sversion = "%s"\napi_version = 10\nauthor = "夹具作者"\n%s\n'
                     'description = [[\n%s\n]]\ntags = %s%s\nsettings = {}\n'
                     % (name_line, ver, kind, desc, tags, deps))
        made += 1
    if made:
        print("模组夹具已放 %d 个工坊模组（%s）" % (made, path(root)))
    # 再放一位"文件只在引擎自己的下载区里"的：云机实测服务端把工坊内容拉到
    # ugc_mods/<房间>/<世界>/content/322330/<id>，mods/ 下面不会多出目录，
    # 而且每个世界各存一份。这里只给 Master 放，洞穴那一份留给"还没重启"的那种状态，
    # 于是同一模组在地表要说"文件在"、在洞穴要说"这个世界还没拉到"——两种来路的说法都得有样本。
    ugc_id, ugc_name, ugc_ver = "900000800", "Global Positions 位置共享", "1.7.6"
    ugc_dir = os.path.join(DST, "ugc_mods", CLUSTER, "Master", "content", "322330", ugc_id)
    if not os.path.exists(os.path.join(ugc_dir, "modinfo.lua")):
        os.makedirs(ugc_dir)
        with io.open(os.path.join(ugc_dir, "modinfo.lua"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write('name = "%s"\nversion = "%s"\napi_version = 10\nauthor = "夹具作者"\n'
                     'server_only = false\nall_clients_require_mod = true\n'
                     'description = [[\n在地图上标出所有玩家的位置。这一位的文件是服务端自己'
                     '按订阅清单拉下来的，存在引擎的下载区里（每个世界各一份），'
                     'mods/ 下面没有对应目录。\n]]\ntags = { "Map Info", "Multiplayer" }\n'
                     'settings = {}\n' % (ugc_name, ugc_ver))
        print("引擎下载区夹具已放 %s（%s）" % (ugc_id, path(ugc_dir)))
    # 把第一个登记进下载清单：这样检索页那三行能同时给出两种结论
    # （"已下载也在清单"和"文件在但不在清单"），后者正是"重启不会更新它"的那种坑。
    setup = os.path.join(root, "dedicated_server_mods_setup.lua")
    lines = ['ServerModSetup("900000001")', 'ServerModSetup("%s")' % ugc_id]
    raw = io.open(setup, encoding="utf-8").read() if os.path.exists(setup) else ""
    # 接口探针会往清单里塞一行故意写坏的 Lua 去验"看不懂的行要被报出来"，跑完不擦。
    # 留着的话以后每次起界面都挂着那条"服务端启动即退出"的红色说明，像夹具坏了。
    have = "\n".join(x for x in raw.splitlines() if x.strip() != "local broken =")
    have = have + "\n" if have else ""
    todo = [x for x in lines if x not in have]
    if todo or have != raw:
        with io.open(setup, "w", encoding="utf-8", newline="\n") as fh:
            fh.write(have + "\n".join(todo) + "\n")


def steam_key():
    """取一把可用的 Steam Web API key，没有就返回空串。

    形状校验和面板自己的规则一致（16~64 位十六进制）：文件读错了也不会把无关字符串当 key 写进配置。
    """
    k = os.environ.get("DST_STEAM_KEY", "").strip()
    if not k and os.path.exists(KEYFILE):
        k = io.open(KEYFILE, encoding="utf-8").read().strip()
    return k if re.fullmatch(r"[0-9a-fA-F]{16,64}", k) else ""


def write_cfg():
    key = steam_key()
    text = """# 界面开发用配置：只在本机 127.0.0.1 上起，密码是随手编的。
server:
  bind: 127.0.0.1
  port: %d
  adminUser: %s
  adminPassword: "%s"
paths:
  kleiRoot: "%s"
  dstInstall: "%s"
  steamCmd: "%s"
  workDir: "%s"
network:
  shardPortBase: 10998
  shardPortEnd: 11900
  masterIntentPortBase: 11000
backup:
  keep: 3
helper:
  pollMs: 1000
  # 界面开发服务器里快照是我手工写的一次，不会再更新；把判定放宽到一天，
  # 否则三小时后每个分片都会挂着"快照已过期"，看着像缺陷。
  staleSeconds: 86400
steamApi:
  webApiKey: "%s"
  proxy: ""
""" % (PORT, USER, PW, path(KLEI), path(DST), path(os.path.join(BIN, "fake_steamcmd.exe")), path(WORK), key)
    with io.open(CFG, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(text)
    os.chmod(CFG, 0o600)
    # 只报形状，不报内容：key 进过对话就要当作会被看见来对待。
    print("在线检索：key %s" % ("%d 位已配" % len(key) if key else "未配（走本地缓存降级）"))


def path(p):
    return p.replace("\\", "/")


def ensure_fake_steamcmd():
    src = os.path.join(ROOT, ".tools", "fake_steamcmd")
    if not os.path.isdir(src):
        return
    # 每次都重建：这份替身是界面实测唯一的"长任务"来源，
    # 留着上一轮的旧 exe 就等于拿旧行为演新界面（本轮就被一个 2 秒跑完的老版本骗过一次）。
    sh([GO, "build", "-o", os.path.join(BIN, "fake_steamcmd.exe"), "./.tools/fake_steamcmd"])


def port_pids(port):
    """找出正在监听这个端口的进程。

    为什么不能只信 pid 文件：新进程绑端口失败会直接退出，而 pid 文件里留下的是
    它的号；healthz 仍然被上一轮的老进程答着，于是界面看起来"起来了"，
    实际发出去的还是老二进制里的资源（实测被这件事骗过一次）。
    """
    out = subprocess.run(["netstat", "-ano"], capture_output=True, text=True,
                         encoding="utf-8", errors="replace").stdout
    pids = set()
    for line in out.splitlines():
        parts = line.split()
        # 形如：TCP  127.0.0.1:8963  0.0.0.0:0  LISTENING  25032
        if len(parts) >= 5 and parts[0].upper() == "TCP" and parts[1].endswith(":%d" % port):
            if parts[3].upper() == "LISTENING":
                pids.add(int(parts[4]))
    return pids


def stop():
    killed = set()
    if os.path.exists(PID):
        try:
            pid = int(io.open(PID).read().strip())
            subprocess.run(["taskkill", "/PID", str(pid), "/T", "/F"], capture_output=True)
            killed.add(pid)
        except Exception as e:  # noqa: BLE001
            print("停不掉 pid 文件里的进程：%s" % e)
        os.remove(PID)
    for pid in port_pids(PORT) - killed:
        subprocess.run(["taskkill", "/PID", str(pid), "/T", "/F"], capture_output=True)
        killed.add(pid)
        print("停掉了占着 %d 端口的旧进程 %d（不是这次 pid 文件里那个）" % (PORT, pid))
    if not killed:
        print("没有在跑的界面开发服务器")


def healthz():
    try:
        with urllib.request.urlopen(BASE + "/healthz", timeout=2) as r:
            return r.status == 200
    except Exception:
        return False


def wait_task(task_id, timeout=180):
    """轮任务表直到这一条落进终态。没有单条查询接口，列表里挑就是接口给界面的做法。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        page = req("GET", "/api/v1/tasks?limit=50")
        # req 在 HTTP 错误时回的是 {"httpError":…,"raw":…}，直接 for 会遍历到键名（字符串），
        # 报错会长成 "'str' object has no attribute 'get'"，看不出是登不上还是接口变了。
        if not isinstance(page, list):
            return {"status": "probe_error",
                    "message": "任务列表不是数组：%s" % json.dumps(page, ensure_ascii=False)[:220]}
        for t in page:
            if t.get("id") == task_id and t.get("status") in ("succeeded", "failed", "canceled"):
                return t
        time.sleep(0.5)
    return {"status": "timeout", "message": "等不到任务结束"}


def seed_world():
    """把 Cluster_1 补成"走向导开出来的房间"：game_mode 有值、两个世界各有一份世界规则文件。

    为什么值得单列：房间页顶部那排事实条里有"游戏模式""地形预设"两格，本地这个集群
    是早期手工造的，cluster.ini 里 game_mode 是空的、也没有 worldgenoverride.lua，
    于是那两格永远显示"没读到"——设计好不好看被夹具缺数据挡住了，每次实测都得猜。
    云机上经接口开的房间不会缺这两项，所以这不是代码问题，是夹具问题。
    """
    set_ = req("GET", "/api/v1/clusters/%s/settings" % CLUSTER) or {}
    if not (set_.get("gameMode") or "").strip():
        req("PUT", "/api/v1/clusters/%s/settings" % CLUSTER,
            {"gameMode": "endless", "intention": "cooperative"})
    for shard, preset in (("Master", "SURVIVAL_TOGETHER"), ("Caves", "DST_CAVE")):
        cur = req("GET", "/api/v1/clusters/%s/shards/%s/world" % (CLUSTER, shard)) or {}
        if (cur.get("preset") or "").strip():
            continue
        req("PUT", "/api/v1/clusters/%s/shards/%s/world" % (CLUSTER, shard), {"preset": preset})


def ini_set(text, section, key, value):
    """在 ini 文本里把某个 section 下的某个键改成 value；段或行不存在就补上。

    不复用面板自己的 ini 读写：这个脚本要能在面板起不来时也把夹具修好。
    段名带不带方框都认（写成 "[SHARD]" 会去找 [SHARD]，不会找出 [[SHARD]]）。
    """
    lines = text.splitlines()
    hdr = "[" + section.strip("[]") + "]"
    if hdr not in [l.strip() for l in lines]:
        lines += ["", hdr, "%s = %s" % (key, value)]
        return "\n".join(lines) + "\n"
    si = next(i for i, l in enumerate(lines) if l.strip() == hdr)
    end = next((i for i in range(si + 1, len(lines)) if lines[i].strip().startswith("[")), len(lines))
    pat = re.compile(r"^\s*" + re.escape(key) + r"\s*=")
    for i in range(si + 1, end):
        if pat.match(lines[i]):
            lines[i] = "%s = %s" % (key, value)
            return "\n".join(lines) + "\n"
    lines.insert(end, "%s = %s" % (key, value))
    return "\n".join(lines) + "\n"


def seed_ports():
    """把 Cluster_1 的端口补成一个自洽的组合。

    这个夹具是早期手工造的：地表 10999、洞穴 10998（两个号挨着，等于洞穴的配套号正好压住
    地表的游戏端口），cluster.ini 里连 master_port 那一行都没有。留着它，端口页每次打开
    都只在演"坏数据"，而健康房间长什么样反而看不见。
    挪到 10998 + 11004 是因为探针房 ProbeWorld 占着 11000~11003。
    """
    want = [
        (os.path.join(KLEI, CLUSTER, "cluster.ini"), "[SHARD]", "master_port", "10999"),
        (os.path.join(KLEI, CLUSTER, "Master", "server.ini"), "[NETWORK]", "server_port", "10998"),
        (os.path.join(KLEI, CLUSTER, "Caves", "server.ini"), "[NETWORK]", "server_port", "11004"),
        (os.path.join(KLEI, CLUSTER, "Caves", "server.ini"), "[SHARD]", "master_port", "10999"),
        # 洞穴光有端口没有"去哪找地表"，引擎是静默连不上而不是报错；面板自己建的房间
        # 两份都写，所以健康夹具也得两份都写。
        (os.path.join(KLEI, CLUSTER, "Caves", "server.ini"), "[SHARD]", "master_ip", "127.0.0.1"),
    ]
    changed = 0
    for path_, section, key, value in want:
        if not os.path.exists(path_):
            continue
        raw = io.open(path_, encoding="utf-8", errors="replace").read()
        fixed = ini_set(raw, section, key, value)
        if fixed != raw:
            with io.open(path_, "w", encoding="utf-8", newline="\n") as fh:
                fh.write(fixed)
            changed += 1
    if changed:
        print("Cluster_1 的端口夹具已补齐（%d 处）" % changed)


def seed_snapshots():
    """打三份快照，然后把其中两份改坏。

    为什么必须改坏：快照页上"能恢复 / 校验对不上 / 归档不见了"三种结论的界面形态完全不同，
    只有好数据的话实测永远看不到后两种，而它们恰恰是恢复动作最需要拦下来的那两种。
    改坏只动 .tmp_boot/uiwork/backups 里面板自己刚生成的那两份（清单是 JSON，改清单
    就能造出"包和清单对不上"，不必去动 .tar.gz 本体）。
    """
    page = req("GET", "/api/v1/backups") or {}
    # 只认这个夹具房间自己生成的编号：面板的保留上限是全局计数的，
    # 导入实测在别的房间下打两份"恢复前的安全副本"就会把这三份挤掉。
    # 按"任意以 2 开头的编号"判断会误认为数据还在，于是永远不再补种。
    had = [s for s in (page.get("snapshots") or [])
           if s.get("id", "").startswith("2") and s.get("cluster") == CLUSTER]
    if had:
        print("快照已有 %d 份，跳过造数据（要重造就先删掉 %s）"
              % (len(had), (page.get("policy") or {}).get("dir", WORK)))
        return

    for _ in range(3):
        acc = req("POST", "/api/v1/clusters/%s/backups" % CLUSTER, {}) or {}
        done = wait_task(acc.get("taskId"))
        if done.get("status") != "succeeded":
            print("备份任务没收敛成成功：%s" % json.dumps(done, ensure_ascii=False)[:220])
            return

    page = req("GET", "/api/v1/backups") or {}
    # 只认面板生成的编号（列表会把认不出的文件也列成一条"(无法识别的文件)"），
    # 并且只挑这个夹具房间自己那几份来改坏：导入实测用的 ProbeWorld 快照是它的输入，
    # 把它们改坏会让那一组检查报出和快照页毫无关系的假失败。
    snaps = [s for s in (page.get("snapshots") or [])
             if s.get("id", "")[:1] == "2" and s.get("cluster") == CLUSTER]
    if len(snaps) < 3:
        print("只拿到 %d 份快照，够不到三种形态：%s" % (len(snaps), [s.get("id") for s in snaps]))
        return
    newest, middle = snaps[0]["id"], snaps[1]["id"]
    d = (page.get("policy") or {}).get("dir") or os.path.join(WORK, "backups")

    os.remove(os.path.join(d, newest + ".tar.gz"))

    mf_path = os.path.join(d, middle + ".manifest.json")
    mf = json.loads(io.open(mf_path, encoding="utf-8").read())
    files = mf.get("files") or []
    if not files:
        print("清单里没有文件，改不出对不上的样本：%s" % mf_path)
        return
    files[0]["sha256"] = "0" * 64
    files.append({"path": "Master/save/session/0000000000000001/slot-0002/ghost.dat",
                  "size": 12, "sha256": "0" * 64})
    mf["files"] = files
    mf["fileCount"] = len(files)
    with io.open(mf_path, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(json.dumps(mf, ensure_ascii=False, indent="  "))

    print("快照已造好 3 份：最新一份只留清单（不能恢复），中间一份改了哈希并多登记一个不存在的文件"
          "（校验对不上），最早一份是好的")


def start(no_build=False):
    if not no_build:
        build()
    if not os.path.exists(EXE):
        print("找不到 %s，先跑一次 .tools/rebuild.sh" % EXE)
        sys.exit(1)
    if not os.path.isdir(KLEI):
        print("探针夹具不在 %s：先跑一次 .tools/api_probe.py 造出来" % KLEI)
        sys.exit(1)
    check_token_files()
    busy = port_pids(PORT)
    if busy:
        print("%d 端口已经被进程 %s 占着：先跑 python .tools/ui_dev.py --stop，"
              "否则起来的是老进程、发的是老资源" % (PORT, sorted(busy)))
        sys.exit(1)
    if not os.path.isdir(WORK):
        os.makedirs(WORK)
    ensure_fake_steamcmd()
    write_status()
    write_map()
    write_logs()
    write_mods()
    state = seed_sessions()
    write_cfg()

    env = dict(os.environ)
    env["PATH"] = BIN + os.pathsep + env.get("PATH", "")
    env["DST_FAKE_SCREEN"] = state
    # 替身 steamcmd 放慢到一分多钟：进度条、速度、"已跑 X 秒"这些只有在任务真的还在跑
    # 的时候才看得见，两秒跑完的替身演不出这一页（实测被老替身骗过一次，拍到的是已完成行）。
    env.setdefault("DST_FAKE_GAP", "2200ms")
    env.setdefault("DST_FAKE_STEPS", "25")
    log = io.open(os.path.join(BOOT, "ui_dev.log"), "w", encoding="utf-8")
    proc = subprocess.Popen([EXE, "-config", CFG], cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, env=env)
    with io.open(PID, "w") as fh:
        fh.write(str(proc.pid))
    for _ in range(80):
        if proc.poll() is not None:
            print("面板进程退出了，看 %s" % os.path.join(BOOT, "ui_dev.log"))
            sys.exit(1)
        if healthz():
            break
        time.sleep(0.25)
    else:
        print("8963 端口 20 秒内没应答，看 %s" % os.path.join(BOOT, "ui_dev.log"))
        sys.exit(1)

    clusters = req("GET", "/api/v1/clusters")
    seed_world()
    seed_ports()
    if not os.environ.get("DST_UI_NO_SEED"):
        seed_snapshots()
    print("界面：      %s/   账号 %s / 密码 %s（只在本机 127.0.0.1）" % (BASE, USER, PW))
    print("screen 替身状态：%s" % state)
    print("日志夹具：  %s" % path(os.path.join(KLEI, CLUSTER, "Master", "server_log.txt")))
    print("房间与分片：%s" % json.dumps(clusters, ensure_ascii=False)[:400])
    print("停止：python .tools/ui_dev.py --stop")


def req(method, api_path, body=None):
    data = None if body is None else json.dumps(body).encode("utf-8")
    import base64
    auth = "Basic " + base64.b64encode(("%s:%s" % (USER, PW)).encode()).decode()
    r = urllib.request.Request(BASE + api_path, data=data, method=method,
                               headers={"Authorization": auth, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return json.loads(resp.read().decode("utf-8")).get("data")
    except urllib.error.HTTPError as e:
        return {"httpError": e.code, "raw": e.read().decode("utf-8")[:300]}


def seed_world_rules():
    """把各房间"和世界默认值不一样"的那几项世界规则清回默认，返回清掉的项数。

    为什么 reseed 要管这个：世界规则页的实测是"改一项 → 写盘 → 重开页面读回来 →
    再改回默认"，中途被打断（我 kill 过一次整轮验收）就把夹具留在脏状态。
    下一轮挑"屏上第一项"来改时，那一项本来就已经改过，改到"另一个取值"正好改回默认，
    于是报出"改完却没认出已改"——面板是对的，红是夹具脏出来的。
    这个文件不在 --reseed 原来的覆盖范围里（它只管 fake_screen/日志/地图/快照），
    所以脏一次就一直脏下去，每轮都得有人重新踩一遍。
    """
    sch = req("GET", "/api/v1/world-schema") or []
    defaults = {}
    for s in sch if isinstance(sch, list) else []:
        for g in s.get("groups") or []:
            for it in g.get("items") or []:
                defaults[(s.get("shard"), it.get("key"))] = it.get("default")
    clusters = req("GET", "/api/v1/clusters") or []
    n = 0
    for c in clusters if isinstance(clusters, list) else []:
        for sh in c.get("shards") or []:
            path = "/api/v1/clusters/%s/shards/%s/world" % (c["name"], sh["name"])
            view = req("GET", path) or {}
            dirty = view.get("changedFromDefault") or []
            if not dirty:
                continue
            ov = dict(view.get("overrides") or {})
            for k in dirty:
                ov[k] = defaults.get((sh["name"], k))
            req("PUT", path, {"overrides": ov})
            n += len(dirty)
    return n


def reseed():
    """只把夹具状态写回去，不重新编译也不重启面板。

    为什么需要这个：fake_screen 的状态是一个 json 文件，面板每次问进程状态都重读它，
    而端口那一条实测必须先停服、导入那一条又要求"目标房间正在跑"。
    一排脚本共用同一个面板时，前面脚本留下的停服状态会让后面的检查报出
    "界面 0 点／接口 271 点"这种看着像产品缺陷的假失败。
    重启一次面板当然也能恢复，但那样每轮要多等好几秒、还得重造快照。
    """
    if not os.path.isdir(KLEI):
        print("探针夹具不在 %s：先跑一次 .tools/api_probe.py 造出来" % KLEI)
        sys.exit(1)
    state = seed_sessions()
    write_status()
    write_map()
    write_logs()
    # 快照夹具也一起补：存档页的三种结论（能恢复／校验对不上／归档不见了）
    # 靠这三份样本才全部露得出来，而实测跑到后面会把它们删掉或 prune 掉。
    # 这一步只在面板活着的时候做——造快照走的是接口，不是直接写库。
    if healthz():
        seed_snapshots()
        n = seed_world_rules()
        if n:
            print("顺手把 %d 项被上一轮改脏的世界规则清回了默认值" % n)
    print("夹具状态已写回：%s（%s 的 %s 记为在跑）" % (state, CLUSTER, "/".join(SHARDS)))


def main():
    args = sys.argv[1:]
    if "--stop" in args:
        stop()
        return
    if "--reseed" in args:
        reseed()
        return
    if "--seed-task" in args:
        # 界面实测用的"正在下载"样本：本体更新要求所有分片先停，所以这里替界面把
        # 分片停掉（fake_screen 的状态文件一改，面板看到的就是停着的状态），
        # 再提交任务。执行的是 .tools/fake_steamcmd 那个替身，一行真网络都不走。
        for shard in SHARDS:
            req("POST", "/api/v1/clusters/%s/shards/%s/stop" % (CLUSTER, shard), {})
        acc = req("POST", "/api/v1/tasks", {"kind": "server.update", "confirm": True})
        print(json.dumps(acc, ensure_ascii=False))
        return
    start(no_build=("--no-build" in args))


if __name__ == "__main__":
    main()
