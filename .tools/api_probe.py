#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""起真面板进程，把这一轮新加的接口按用户会走的顺序过一遍。

单测全绿不代表成品能用：上次就是靠这一步才发现 screen 报错和 file does not exist
漏到了响应里。这里检查的是"响应里出现的字"，不是内部函数返回值。
"""
import base64
import glob
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from datetime import datetime, timedelta, timezone

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BOOT = os.path.join(ROOT, ".tmp_boot")
KLEI = os.path.join(BOOT, "klei")
DST = os.path.join(BOOT, "dst")
WORK = os.path.join(BOOT, "work")
EXE = os.path.join(ROOT, ".tmp_bin", "dstpanel.exe")
FAKE_EXE = os.path.join(ROOT, ".tmp_bin", "fake_steamcmd.exe")
GO = r"C:/coding/go/sdk/go1.23.1.windows-amd64/go/bin/go.exe"
PORT = 8931
BASE = "http://127.0.0.1:%d" % PORT
AUTH = "Basic " + base64.b64encode(b"admin:boot-check-pw").decode()
# 和 config.DefaultAdminPassword 必须是同一个值：这一组验的就是"还在用出厂默认密码"
# 这个状态本身，探针里另起一个口令就永远走不到那条分支。
DEFAULT_PW = "admin"

results = []

# key 的形状校验：十六进制 16~64 位。不合法就当没配，绝不把它拼进任何命令或文件。
KEY_SHAPE = re.compile(r"^[0-9A-Fa-f]{16,64}$")


def _once(req):
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read().decode("utf-8")
            return resp.status, json.loads(raw)
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8")
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"raw": raw}
    except Exception as e:  # noqa: BLE001
        return -1, {"error": "%s: %s" % (type(e).__name__, e)}


# WinError 10054 = 连接被强制关闭。本机连着两轮在同一条检查上撞到它，
# 而面板日志里那次请求其实正常写了 403（见 work/panel_probe.log），单独复现三次也全对。
# 所以只重发一次：真是接口把连接掐了，第二次还是红的；是本机 socket 抖动，就不会再浪费
# 一整轮去追一个不存在缺陷的"缺陷"。
RESET = "10054"


def _do(req, path):
    code, body = _once(req)
    if code == -1 and RESET in str(body.get("error", "")):
        print("[RETRY] %s：连接被强制关闭，重发一次" % path)
        time.sleep(0.5)
        code, body = _once(req)
    return code, body


def call(method, path, body=None):
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=data, method=method,
                                 headers={"Authorization": AUTH, "Content-Type": "application/json"})
    return _do(req, path)


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name, ("  -- " + detail) if detail else ""))


def msg_of(r):
    """接口给用户的中文说法：错误串，或 data 里那句 note。"""
    return str((r.get("error") or (r.get("data") or {}).get("note") or ""))


def search_url(base, q):
    """中文搜索词必须转义：urllib 直接发非 ASCII 请求行会炸在客户端，
    然后报成"面板没返回中文"，看着像产品缺陷。浏览器会自己转，探针得手动。"""
    return base + "/api/v1/mods/search?q=" + urllib.parse.quote(q, safe="")


def make_zip(entries):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        for name, content in entries:
            z.writestr(name, content)
    return buf.getvalue()


def call_upload(path, fields, filename, body, file_field="mod"):
    """手工拼 multipart：这一轮要验的就是上传接口，不能改用例来迁就成品。
    file_field 跟着接口走：模组上传收 "mod"，存档导入收 "save"。"""
    boundary = "----dstpanelprobe74329"
    buf = b""
    for k, v in fields.items():
        buf += ('--%s\r\nContent-Disposition: form-data; name="%s"\r\n\r\n%s\r\n'
                % (boundary, k, v)).encode("utf-8")
    if filename:
        buf += ('--%s\r\nContent-Disposition: form-data; name="%s"; filename="%s"\r\n'
                'Content-Type: application/zip\r\n\r\n' % (boundary, file_field, filename)).encode("utf-8")
        buf += body + b"\r\n"
    buf += ("--%s--\r\n" % boundary).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=buf, method="POST",
                                 headers={"Authorization": AUTH,
                                          "Content-Type": "multipart/form-data; boundary=%s" % boundary})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8")
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"raw": raw}
    except Exception as e:  # noqa: BLE001
        return -1, {"error": str(e)}


def live_checks(key):
    """用带 key 的配置再起一个面板，只验"key 真的走到了成品"。

    这台 Windows 出不了网，所以在线成功路径不在这里断言；在云机上验
    （.tools/cloud_search_probe.py）。这里要钉住的是两件事：
    onlineEnabled 必须变 true（否则等于配置项是个摆设），以及失败结论是中文且不带 key。
    """
    if not KEY_SHAPE.match(key):
        check("DST_STEAM_KEY 形状合法", False, "不是 16~64 位十六进制，跳过在线检查")
        return
    port = PORT + 2
    text = open(os.path.join(BOOT, "panel_boot.yaml"), encoding="utf-8").read()
    text = re.sub(r'webApiKey:\s*"[^"]*"', 'webApiKey: "%s"' % key, text)
    text = re.sub(r"(^|\n)\s*port:\s*%d" % PORT, "\n  port: %d" % port, text)
    live_cfg = os.path.join(BOOT, "panel_boot_live.yaml")
    base = "http://127.0.0.1:%d" % port
    with open(live_cfg, "w", encoding="utf-8") as fh:
        fh.write(text)
    os.chmod(live_cfg, 0o600)
    log = open(os.path.join(WORK, "panel_probe_live.log"), "w", encoding="utf-8")
    proc = subprocess.Popen([EXE, "-config", live_cfg], cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
    try:
        up = False
        for _ in range(60):
            try:
                with urllib.request.urlopen(base + "/healthz", timeout=2) as resp:
                    up = resp.status == 200
                    break
            except Exception:
                time.sleep(0.25)
        if not up:
            check("带 key 的第二个面板起来了", False, "看 .tmp_boot/work/panel_probe_live.log")
            return
        check("带 key 的第二个面板起来了", True)
        req = urllib.request.Request(base + "/api/v1/mods/search?q=stack",
                                     headers={"Authorization": AUTH})
        with urllib.request.urlopen(req, timeout=40) as resp:
            v = json.loads(resp.read().decode("utf-8")).get("data", {})
        body = json.dumps(v, ensure_ascii=False)
        # 这一条是整个函数的存在理由：配置项读到了才算接线成功。
        check("配置里的 key 走到了成品（onlineEnabled）", v.get("onlineEnabled") is True, body[:150])
        if v.get("live"):
            items = v.get("items") or []
            check("在线检索真的拿到了条目", len(items) > 0 and all(i.get("name") for i in items),
                  json.dumps(items[:2], ensure_ascii=False)[:220])
            check("来源标成 Steam 在线检索", bool(items) and "Steam" in items[0]["sourceLabel"],
                  items[0]["sourceLabel"] if items else "")
        else:
            problem = v.get("liveProblem") or ""
            # 本机没出口时必然走这里；结论必须是中文、并且指出该配哪一项。
            check("出不去时给的是中文诊断而不是英文串",
                  bool(problem) and not all(ord(c) < 128 for c in problem)
                  and "steamApi.proxy" in (v.get("note") or problem), body[:200])
        check("带 key 的响应里没有 key 值", key not in body, body[:120])
        # 在线那批（或那批失败前扫到的）应该已经进库，重启后还能搜到"几何"。
        req = urllib.request.Request(search_url(base, "几何"), headers={"Authorization": AUTH})
        with urllib.request.urlopen(req, timeout=40) as resp:
            v2 = json.loads(resp.read().decode("utf-8")).get("data", {})
        check("换了进程缓存还在（同一库）", len(v2.get("items") or []) == 1
              and (v2.get("items") or [{}])[0].get("workshopId") == "398558175",
              json.dumps(v2.get("items"), ensure_ascii=False)[:160])
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except Exception:
            proc.kill()
        log.close()
        # 带 key 的配置只在这一刻存在，用完立刻删掉，不给它留在项目目录里的机会。
        try:
            os.remove(live_cfg)
        except OSError:
            pass


def bootstrap_checks():
    """开服引导：新建集群 → 令牌 → 世界设定，全程按界面上的点击顺序走。

    这一组存在的理由和别处一样：单测看的是函数返回值，这里看的是磁盘上真的留下了什么、
    响应里真的出现了什么字。cluster.ini 的段名被写成 [[NETWORK]] 那次就是只有这里能抓到。
    """
    name = "ProbeWorld"
    root = os.path.join(KLEI, name)

    # 先拿真字典：写死键名会在某次更新 Klei 资产后变成假失败。
    code, r = call("GET", "/api/v1/world-schema")
    locs = r.get("data", [])
    check("世界设定字典给出两张表", code == 200 and len(locs) == 2, str([x.get("location") for x in locs]))
    by_loc = {x.get("location"): x for x in locs}
    forest, cave = by_loc.get("forest"), by_loc.get("cave")
    check("两张表的条目数不一样（洞穴没有季节那组）",
          forest and cave and forest.get("itemCount", 0) > cave.get("itemCount", 0),
          "forest=%s cave=%s" % (forest and forest.get("itemCount"), cave and cave.get("itemCount")))
    # 界面上绝不能出现 world_size 这种键名当标签。
    bad_labels = []
    for loc in locs:
        if not loc.get("label") or loc["label"] == loc["location"]:
            bad_labels.append(loc["location"])
        for g in loc.get("groups", []):
            if not g.get("label") or g["label"] == g.get("key"):
                bad_labels.append(g.get("key"))
            for it in g.get("items", []):
                if not it.get("label") or it["label"] == it.get("key"):
                    bad_labels.append(it.get("key"))
                if any(not o.get("label") for o in it.get("options", [])):
                    bad_labels.append(it.get("key") + ":空选项")
    check("字典里每一项都有中文文案", not bad_labels, str(bad_labels[:5]))

    def pick(location):
        # 挑一个"和默认值不同"的取值：改了等于没改的话，changedFromDefault 那类断言就是在比空气。
        for g in by_loc[location]["groups"]:
            for it in g["items"]:
                for o in it["options"]:
                    if o["value"] != it["default"]:
                        return it["key"], o["value"]
        return None, None

    fkey, fval = pick("forest")
    ckey, cval = pick("cave")

    # 键名集合：用来确认两张表确实各有独占键，不然下面的负向断言就是在比空气。
    def keys_of(loc):
        return {it["key"] for g in (loc or {}).get("groups", []) for it in g.get("items", [])}

    # 闭世界校验：一张表能写什么，由它自己的键名和取值档位说了算。
    # 上一版只比对"另一张表的键名不许出现"，结果把两张表都有、但档位含义不同的键
    #（洞穴的 birds/merm/ponds 是数量档位，地表的同名字面是出现与否）判成了混表，
    # 反过来 Master 文件里真出现的洞穴键也抓不出来。
    def legal_of(loc):
        table = {}
        for g in (loc or {}).get("groups", []):
            for it in g.get("items", []):
                vals = {str(o["value"]) for o in it.get("options", [])}
                vals.add(str(it.get("default")))
                table[it["key"]] = vals
        return table

    LEGAL = {l["location"]: legal_of(l) for l in locs}

    # 世界预设：2026-10-31 云机实测之后，面板写的是 worldgenoverride.lua 的 preset 通道
    # （只三样顶层键：override_enabled / preset / overrides）。
    # layout_mode、has_ocean、wormhole_prefab、required_set_pieces 那批由预设从
    # scripts/map/levels.lua 带进来，面板一律不写——写进去只会让
    # shardindex.lua 的 SanityCheckWorldGenOverride 打一条 WARNING。
    ENGINE_INTERNALS = ("layout_mode", "has_ocean", "wormhole_prefab", "keep_disconnected_tiles",
                        "no_joining_islands", "random_set_pieces", "required_setpieces")
    presets_of = {l["location"]: l.get("presets") or [] for l in locs}
    default_of = {l["location"]: (l.get("defaultPreset") or {}).get("value") for l in locs}
    check("字典给每个地形列出可选预设，且默认预设一定在表里",
          all(presets_of[l["location"]] and default_of[l["location"]]
              in [p["value"] for p in presets_of[l["location"]]] for l in locs),
          str({k: [p["value"] for p in v] for k, v in presets_of.items()})[:170])
    bad_preset_labels = [p["value"] for v in presets_of.values() for p in v
                         if not p.get("label") or p["label"] == p["value"]]
    check("预设给界面的是中文而不是 SURVIVAL_TOGETHER 这种码值",
          not bad_preset_labels, str(bad_preset_labels[:4]))
    forest_ids = {p["value"] for p in presets_of.get("forest", [])}
    cave_ids = {p["value"] for p in presets_of.get("cave", [])}
    check("两张预设表不重叠（把洞穴预设点到地表要能被当场拒掉）",
          bool(forest_ids) and bool(cave_ids) and not (forest_ids & cave_ids),
          str(sorted(forest_ids))[:90] + " / " + str(sorted(cave_ids))[:70])
    # 选一个"不是默认值"的地表预设来提交：都用默认值的话，下面那条"回显的预设就是点的那个"在比空气。
    alt_forest = next((p["value"] for p in presets_of.get("forest", [])
                       if p["value"] != default_of.get("forest")), None)
    check("地表有第二个预设可点", bool(alt_forest), str(sorted(forest_ids))[:90])

    def written_overrides(text):
        i = text.find("overrides = {")
        if i < 0:
            return {}
        # 引擎自己的示例键：override_string 的值里带着 "["x"] = 5," 这种片段，
        # 正则会在示例内容里误读出键名，这三条是元键，本来就不受字典管。
        meta = {"override_string", "override_int", "override_bool"}
        out = {}
        for k, v in re.findall(r'(\w+) = (".*?"|[-0-9.]+|true|false),?', text[i:]):
            if k not in meta:
                out[k] = v.strip('"')
        return out

    forest_only = sorted(keys_of(forest) - keys_of(cave))
    cave_only = sorted(keys_of(cave) - keys_of(forest))
    check("两张表确有各自独占的键（负向断言有料可测）",
          bool(forest_only) and bool(cave_only),
          "只在地表:%s 只在洞穴:%s" % (forest_only[:6], cave_only[:6]))

    # 1) 建集群：地表 + 洞穴、带世界预设与设定、先不给令牌
    code, r = call("POST", "/api/v1/clusters", {
        "name": name, "numShards": 2,
        "game": {"displayName": "探针开服服", "maxPlayers": 9, "gameMode": "survival"},
        "world": {"presets": {"Master": alt_forest},
                  "flatOverrides": {fkey: fval},
                  "overrides": {"Caves": {ckey: cval}}},
    })
    d = r.get("data", {})
    body = json.dumps(r, ensure_ascii=False)
    check("开服接口建出两分片集群", code == 200 and len(d.get("shards", [])) == 2, body[:160])
    notes = "\n".join(d.get("notes", []))
    check("没给令牌时把引擎的拒绝原话说出来", "Your Server Will Not Start" in notes, notes[:200])
    check("说明第一次启动要生成世界", "生成世界" in notes, notes[:120])
    # 世界名不在这条路上：能改的玩家可见身份是集群显示名与分片名。
    check("建集群的回复里点明世界名字归谁管", "世界名字" in notes, notes[:160])
    by_shard = {sh.get("shard"): sh for sh in d.get("shards", [])}
    check("回复里给的是预设的中文说法，不是码值",
          (by_shard.get("Master") or {}).get("presetLabel")
          and (by_shard.get("Master") or {}).get("presetLabel") != alt_forest,
          str([sh.get("presetLabel") for sh in d.get("shards", [])])[:120])

    ini_path = os.path.join(root, "cluster.ini")
    ini = open(ini_path, encoding="utf-8").read() if os.path.isfile(ini_path) else ""
    check("cluster.ini 没有引擎不认的双层段头", "[[" not in ini, ini[:80].replace("\n", " | "))
    # 值一律不带引号：引擎不剥引号，game_mode = "survival" 会被读成 '"survival"'
    # 然后报 not found in GAME_MODES（云机首启的日志原话）。
    for want in ("cluster_name = 探针开服服", "max_players = 9", "game_mode = survival",
                 "shard_enabled = true"):
        check("cluster.ini 写了 %s" % want, want in ini, "")
    check("cluster.ini 里没有带引号的字符串值", '= "' not in ini, ini[:120].replace("\n", " | "))

    for shard, loc, key, val, want_preset in (
            ("Master", "forest", fkey, fval, alt_forest),
            ("Caves", "cave", ckey, cval, default_of.get("cave"))):
        p = os.path.join(root, shard, "worldgenoverride.lua")
        text = open(p, encoding="utf-8").read() if os.path.isfile(p) else ""
        written = written_overrides(text)
        table = LEGAL[loc]

        # 顶层只有三个键，一个不多：引擎的 SanityCheckWorldGenOverride 只认
        # overrides / preset / settings_preset / worldgen_preset / override_enabled，
        # 面板只需要前三个里的三个。多写一个（比如 leveldata 时代的 name）就换一条 WARNING。
        tops = re.findall(r'^\t(\w+)\s*=', text, re.M)
        check("%s 的世界文件顶层只有 override_enabled/preset/overrides" % shard,
              tops == ["override_enabled", "preset", "overrides"], str(tops)[:90].replace("\n", " | "))
        # 这一句是开关：缺了它，引擎打一句 "Found world gen overrides but not enabled"
        # 然后把整份文件当没有——界面显示的是刚保存的设定，引擎用的却是默认世界。
        check("%s 的世界文件带着生效开关" % shard,
              bool(re.search(r'^\toverride_enabled = true,$', text, re.M)),
              text[:90].replace("\n", " | "))
        check("%s 的世界文件 preset 写的是 %s" % (shard, want_preset),
              bool(re.search(r'^\tpreset = "%s",$' % re.escape(want_preset or ""), text, re.M)),
              text[:120].replace("\n", " | "))

        # 闭世界：写进 overrides 的每一项都必须是这一张表承认的键名+档位。
        # 这条比"别混另一张表的键"更硬——它同时挡住拼错的键、越界的档位，
        # 以及引擎会因为不认而整段忽略掉的那类静默失败。
        illegal = ["%s = %s" % (k, v) for k, v in sorted(written.items())
                   if k not in table or v not in table[k]]
        check("%s 的世界设定全在本表键名与档位内（本表 %d 项）" % (shard, len(table)),
              len(written) > 10 and not illegal, "越界 %s" % illegal[:5])
        check("%s 写全了本表 %d 项" % (shard, len(table)),
              len(written) == len(table), "写了 %d 项" % len(written))
        # 内生项由预设从 levels.lua 带进来；面板写进去不会生效，只会污染日志。
        injected = [k for k in ENGINE_INTERNALS if re.search(r'^\s*%s\s*=' % re.escape(k), text, re.M)]
        check("%s 没把引擎内生项写进世界文件" % shard, not injected, str(injected)[:90])
        check("%s 的世界文件带上提交的 %s" % (shard, key), written.get(key) == val,
              "文件里是 %s" % written.get(key))
        # 面板不写 leveldataoverride.lua（那是 GAME USE 文件，要一整份完整世界定义，
        # 客户端才下发得全）；留一份半成品在目录里，日志只有一句 assert。
        check("%s 没有留下 leveldataoverride.lua" % shard,
              not os.path.isfile(os.path.join(root, shard, "leveldataoverride.lua")), "")
        # 键名必须整词匹配。之前用 `key + " = "` 的子串判混表，结果 moon_fissure
        # 被读成 fissure、mutated_birds 被读成 birds，两条永远红。
        own, other = (forest_only, cave_only) if shard == "Master" else (cave_only, forest_only)

        def written_key(k):
            return re.search(r'^\s*%s\s*=' % re.escape(k), text, re.M)

        leaked = [k for k in other if written_key(k)]
        check("%s 的世界文件没混进另一张表独占的键" % shard,
              bool(other) and not leaked,
              "反例键 %s，漏了 %s" % (other[:4], leaked))
        present = [k for k in own if written_key(k)]
        check("%s 的世界文件写全了本表独占的 %d 个键" % (shard, len(own)),
              bool(own) and len(present) == len(own),
              "少了 %s" % sorted(set(own) - set(present))[:4])
        # 本表独有的键要用本表默认值：洞穴独有的 groundloot 得是 cave 的答案，
        # 而不是跟着地表那份。提交的键除外——它的期望是提交值，上面单独验。
        want = forest if shard == "Master" else cave
        wrong = []
        for k in own:
            if k == key:
                continue
            d = next((it["default"] for g in want["groups"] for it in g["items"] if it["key"] == k), None)
            if isinstance(d, bool):
                d = "true" if d else "false"
            if d is None:
                continue
            got = written.get(k)
            if got is not None and got != str(d):
                wrong.append("%s = %s（本表默认 %s）" % (k, got, d))
        check("%s 的本表独有键没串进另一张表的答案" % shard, not wrong, str(wrong[:4]))

    # 2) 新集群要被面板认出来。
    # 总览页在本机（Windows，PATH 里没有 screen）必然 503：它宁可不报进程态，
    # 也不能列出"全都未运行"骗用户去重复点启动。所以这里按环境分支断言，
    # 集群识别本身走不碰 screen 的 world/mods 路由——那两条同样先拿 URL 名字对过磁盘。
    code, r = call("GET", "/api/v1/clusters")
    body = json.dumps(r, ensure_ascii=False)
    if code == 200:
        check("总览页列出新建的集群", name in [c.get("name") for c in r.get("data", [])], body[:160])
    else:
        check("无 screen 时总览页拒绝编造进程态", code == 503 and "screen" in body, body[:140])

    # 2b) 房间总览：一行一个分片，且这条新集群正好是"刚建好、没令牌、没起进程"的形状，
    # 每个字段的降级路径都能在这里验到（探针不会再有第二次这个机会）。
    code, r = call("GET", "/api/v1/rooms")
    rbody = json.dumps(r, ensure_ascii=False)
    if code == 200:
        d = r.get("data", {})
        items = d.get("items", [])
        mine = [i for i in items if i.get("cluster") == name]
        check("房间总览按分片出行（新集群两行）",
              len(mine) == 2 and sorted(i.get("shard") for i in mine) == ["Caves", "Master"],
              str([i.get("shard") for i in mine])[:90])
        check("总览的行数与房间/分片计数对得上",
              d.get("rooms") == len(items) and d.get("clusters") >= 1 and items,
              "clusters=%s rooms=%s items=%s" % (d.get("clusters"), d.get("rooms"), len(items)))
        by = dict((i.get("shard"), i) for i in mine)
        # 显示名和目录名是两件事，界面上必须都拿得到，否则照着房间名找不到目录。
        check("总览把显示名和目录名分开给",
              by.get("Master", {}).get("roomName") == "探针开服服"
              and by.get("Master", {}).get("cluster") == name,
              str([by.get("Master", {}).get(k) for k in ("roomName", "cluster")])[:120])
        check("总览的地形列是中文",
              (by.get("Master") or {}).get("terrainLabel") == "地表"
              and (by.get("Caves") or {}).get("terrainLabel") == "洞穴"
              and all(i.get("terrain") != i.get("terrainLabel") for i in mine),
              str([[i.get("terrain"), i.get("terrainLabel")] for i in mine])[:120])
        check("总览带上了开服时点的预设中文",
              by.get("Master", {}).get("presetLabel") not in ("", None, alt_forest),
              str(by.get("Master", {}).get("presetLabel"))[:60])
        # 洞穴那一行的主机地址来自洞穴自己的 server.ini，地表换端口时它是唯一线索。
        # 注意它指的是主机的 intent 端口（cluster.ini 的 [SHARD] master_port），
        # 不是主机的 server_port——按后者断言会永远差 1，那是引擎的规矩不是面板的 bug。
        m_intent = re.search(r'^\s*master_port\s*=\s*(\d+)', ini, re.M)
        addr = str((by.get("Caves") or {}).get("masterAddr") or "")
        check("洞穴行报出的主机端口就是 cluster.ini 里那个",
              bool(m_intent) and addr.endswith(":%s" % m_intent.group(1)),
              "masterAddr=%s cluster.ini master_port=%s" % (addr, m_intent.group(1) if m_intent else "?"))
        m_ip = re.search(r'^\s*master_ip\s*=\s*(\S+)',
                         open(os.path.join(root, "Caves", "server.ini"), encoding="utf-8").read(), re.M)
        want_addr = "%s:%s" % (m_ip.group(1), m_intent.group(1)) if (m_ip and m_intent) else ""
        check("洞穴行的主机地址是完整的 ip:端口，且照抄盘上那两行",
              addr == want_addr and (bool(m_ip) == bool(addr)),
              "行上 %s，盘上 %s" % (addr or "（空）", want_addr or "（没配）"))
        check("主机行不带 masterAddr（它自己就是主机）",
              not by.get("Master", {}).get("masterAddr"), str(by.get("Master", {}).get("masterAddr"))[:60])
        # 这一步还没写令牌，所以"缺令牌"必须是这一页给的结论，而不是含糊的"未注册"。
        check("没令牌时总览说出这一档并给出短名",
              all(i.get("access") == "no-token" and i.get("accessShort") == "缺令牌" for i in mine)
              and all("搜不到" in str(i.get("alert") or "") for i in mine),
              str([[i.get("access"), i.get("accessShort"), i.get("alert")] for i in mine])[:160])
        check("没起的分片不许报世界就绪",
              all(i.get("running") is False and i.get("worldReady") is False and i.get("online") == 0
                  for i in mine),
              str([[i.get("running"), i.get("worldReady"), i.get("online")] for i in mine])[:140])
        check("进程态标签是中文而不是 true/false",
              all(i.get("runningLabel") == "未运行" for i in mine),
              str([i.get("runningLabel") for i in mine])[:60])
        # 全表通用约定：有码值就必须有对应的中文标签，且不能原样重复码值。
        no_label = []
        for i in items:
            for code_key, label_key in (("gameMode", "gameModeLabel"), ("intention", "intentionLabel"),
                                         ("season", "seasonLabel"), ("phase", "phaseLabel"),
                                         ("terrain", "terrainLabel")):
                v = i.get(code_key)
                if v and (not i.get(label_key) or i.get(label_key) == v):
                    no_label.append("%s/%s:%s" % (i.get("cluster"), code_key, v))
        check("码值都配了中文标签（全表）", not no_label, str(no_label)[:200])
        flagged = [i for i in items if i.get("alert")]
        check("要注意的数量等于带 alert 的行数",
              d.get("attention") == len(flagged),
              "attention=%s 行数=%s" % (d.get("attention"), len(flagged)))
        ready = [i for i in items if i.get("worldReady")]
        check("在线合计只算世界就绪的行",
              d.get("online") == sum(i.get("online") or 0 for i in ready),
              "online=%s 就绪行数=%s" % (d.get("online"), len(ready)))
        check("总览给了生成时刻", bool(d.get("generatedAt")), str(d.get("generatedAt"))[:40])
    else:
        check("无 screen 时房间总览同样拒绝编造进程态",
              code == 503 and "screen" in rbody, rbody[:140])

    code, r = call("GET", "/api/v1/clusters/%s/shards/Master/world" % name)
    d = r.get("data", {})
    check("新集群地表分片立刻读得到世界设定", code == 200 and d.get("present") is True, str(d)[:120])
    # 读回来的是文件里那个码值（排查要照着它对文件），中文说法在 presetLabel。
    check("世界设定读回的预设就是建集群时点的", code == 200 and d.get("preset") == alt_forest
          and d.get("presetLabel") not in ("", None, alt_forest), str(d)[:150])
    check("世界设定指给使用者的文件路径是 worldgenoverride.lua",
          (d.get("path") or "").endswith("worldgenoverride.lua"), str(d.get("path"))[-60:])
    check("面板建出来的集群不该有 leveldata 残留冲突", not d.get("levelDataConflict"), str(d.get("note"))[:90])
    code, r = call("GET", "/api/v1/clusters/%s/shards/Caves/mods" % name)
    check("新集群洞穴分片的模组页可达", code == 200, str(r.get("error"))[:120])

    # 3) 重名要给结论，不能默默复用
    code, r = call("POST", "/api/v1/clusters", {"name": name, "numShards": 1})
    check("重名集群 400 并说已存在", code == 400 and "已存在" in json.dumps(r, ensure_ascii=False), str(r.get("error"))[:100])

    # 4) 令牌：错的形式先挡住，写进去之后接口和审计都不许出现明文
    secret = "ProbeToken1234567890abcdef"
    code, r = call("PUT", "/api/v1/clusters/%s/token" % name, {"token": "short"})
    check("过短的令牌被拒 400", code == 400 and "过短" in json.dumps(r, ensure_ascii=False), str(r.get("error"))[:90])
    code, r = call("PUT", "/api/v1/clusters/%s/token" % name, {"token": secret})
    body = json.dumps(r, ensure_ascii=False)
    check("写入令牌成功但响应里没有明文", code == 200 and secret not in body, body[:120])
    tok_file = os.path.join(root, "cluster_token.txt")
    disk = open(tok_file, encoding="utf-8").read().strip() if os.path.isfile(tok_file) else ""
    check("cluster_token.txt 真的写了这枚", disk == secret, disk[:6] + "...")
    code, r = call("GET", "/api/v1/clusters/%s/token" % name)
    d = r.get("data", {})
    check("读令牌只回是否配置与长度", code == 200 and d.get("configured") is True
          and d.get("length") == len(secret) and secret not in json.dumps(r, ensure_ascii=False), str(d)[:120])

    # 5) 世界设定：只提交一项，其余写全默认；预设错配要当场拒；改完要说清生效时机
    code, r = call("PUT", "/api/v1/clusters/%s/shards/Master/world" % name,
                   {"overrides": {"world_size": "not_a_value"}})
    check("字典外的世界设定取值 400", code == 400 and "world_size" in json.dumps(r, ensure_ascii=False),
          str(r.get("error"))[:120])
    # 洞穴的预设点到地表：引擎查不到时不报错，只是静默按另一张图生成，所以必须在这儿挡住。
    code, r = call("PUT", "/api/v1/clusters/%s/shards/Master/world" % name,
                   {"preset": default_of.get("cave")})
    check("给地表点洞穴预设 400 并把可选项说出来", code == 400
          and (default_of.get("cave") or "?") in json.dumps(r, ensure_ascii=False), str(r.get("error"))[:170])
    text_before = open(os.path.join(root, "Master", "worldgenoverride.lua"), encoding="utf-8").read()
    code, r = call("PUT", "/api/v1/clusters/%s/shards/Master/world" % name,
                   {"preset": "NOT_A_REAL_PRESET"})
    check("字典外的预设名 400，不许静默换成默认值", code == 400 and "NOT_A_REAL_PRESET" in str(r.get("error")),
          str(r.get("error"))[:170])
    check("被拒的两次 PUT 没动文件",
          open(os.path.join(root, "Master", "worldgenoverride.lua"), encoding="utf-8").read() == text_before, "")
    code, r = call("PUT", "/api/v1/clusters/%s/shards/Master/world" % name,
                   {"overrides": {fkey: fval}, "regenerate": True})
    d = r.get("data", {})
    check("世界设定写入后回显完整表", code == 200 and len(d.get("overrides", {})) > 20
          and d.get("overrides", {}).get(fkey) == fval, "共 %d 项" % len(d.get("overrides", {})))
    # 只提交设定、没提交预设时沿用文件里那个，不能悄悄打回默认预设。
    check("没点预设时沿用文件里已有的那个", d.get("preset") == alt_forest, str(d)[:130])
    check("生效时机说清是下一次生成", "下一次世界生成" in (d.get("note") or ""), (d.get("note") or "")[:140])
    check("带了 regenerate 也明确没重生世界", "没有重生世界" in (d.get("note") or ""), (d.get("note") or "")[-60:])
    check("回复里点明房间名归集群设置管", "显示名" in (d.get("note") or ""), (d.get("note") or "")[-80:])

    # 分片成功进过世界之后，磁盘上这份文件是引擎重写过的：加 "KLEI     1 " 头、
    # preset 展开成 worldgen_preset + settings_preset（shardindex.lua:434/437）、
    # overrides 收缩成它认下的那几项。下面这一串是 2026-10-31 从云机
    # Cluster_1/Master/worldgenoverride.lua 逐字节抄下来的（186 字节）。
    # 第一次成功启动之后世界页读的就是它：读不出预设的话世界页 500，
    # 而 PUT 会永久停在"文件里没有可用预设"的 409 上——服务器跑得好好的，
    # 界面却一个字节都改不动，这正是最难被发现的一类缺陷。
    eng = os.path.join(root, "Master", "worldgenoverride.lua")
    eng_text = ('KLEI     1 return {\n\toverride_enabled = true,\n'
                '\tworldgen_preset = "SURVIVAL_TOGETHER_CLASSIC",\n'
                '\tsettings_preset = "SURVIVAL_TOGETHER_CLASSIC",\n'
                '\toverrides = {\n\t\ttask_set = "default",\n\t},\n}')
    open(eng, "w", encoding="utf-8").write(eng_text)
    code, r = call("GET", "/api/v1/clusters/%s/shards/Master/world" % name)
    d = r.get("data", {})
    check("引擎改写过的文件读得出预设（世界页不会 500）",
          code == 200 and d.get("preset") == "SURVIVAL_TOGETHER_CLASSIC", "%s %s" % (code, str(d)[:130]))
    check("改写过的文件也有中文说法，不露码值",
          bool(d.get("presetLabel")) and d.get("presetLabel") != d.get("preset"), str(d)[:130])
    check("界面说明了这是引擎按存档重写的，不是面板删的",
          "引擎" in (d.get("note") or "") and "重写" in (d.get("note") or ""), (d.get("note") or "")[:150])
    code, r = call("PUT", "/api/v1/clusters/%s/shards/Master/world" % name, {"overrides": {fkey: fval}})
    d = r.get("data", {})
    check("成功启动过的分片不该被 409 锁住编辑", code == 200, "%s %s" % (code, str(r.get("error"))[:130]))
    check("没点预设时沿用引擎留下的那一个", d.get("preset") == "SURVIVAL_TOGETHER_CLASSIC", str(d)[:130])
    body = open(eng, encoding="utf-8").read()
    check("面板按自己的拼法接管了这份文件",
          '\tpreset = "SURVIVAL_TOGETHER_CLASSIC",' in body and not body.startswith("KLEI"), body[:110])

    # 手工改过、没有预设（或预设不认识）的文件：面板不替你猜一个预设再写下去。
    hand = os.path.join(root, "Caves", "worldgenoverride.lua")
    hand_text = 'return {\n\toverride_enabled = true,\n\toverrides = {\n\t\tworld_size = "small",\n\t},\n}\n'
    open(hand, "w", encoding="utf-8").write(hand_text)
    code, r = call("PUT", "/api/v1/clusters/%s/shards/Caves/world" % name, {"overrides": {ckey: cval}})
    check("文件里没有可用预设时 PUT 要 409 而不是猜一个", code == 409, "%s %s" % (code, str(r.get("error"))[:130]))
    check("409 之前没把文件改掉",
          open(hand, encoding="utf-8").read() == hand_text, "")
    code, r = call("GET", "/api/v1/clusters/%s/shards/Caves/world" % name)
    d = r.get("data", {})
    check("GET 不受 409 限制，如实回显现状", code == 200 and d.get("overrides", {}).get("world_size") == "small"
          and not d.get("presetLabel"), str(d)[:150])
    code, r = call("PUT", "/api/v1/clusters/%s/shards/Caves/world" % name,
                   {"preset": default_of.get("cave"), "overrides": {ckey: cval}})
    d = r.get("data", {})
    check("明确点了预设之后同一份请求就过了", code == 200 and d.get("preset") == default_of.get("cave"),
          str(d)[:130])

    # 手工放一份 leveldataoverride.lua：面板不写它，但界面上得说清它在那儿、且不生效。
    stale = os.path.join(root, "Caves", "leveldataoverride.lua")
    open(stale, "w", encoding="utf-8").write('return { id = "default", name = "拷来的", desc = "", location = "cave", overrides = { } }\n')
    code, r = call("GET", "/api/v1/clusters/%s/shards/Caves/world" % name)
    d = r.get("data", {})
    check("检测到手工 leveldataoverride.lua 并说清它不生效",
          bool(d.get("levelDataConflict")) and "leveldataoverride" in (d.get("levelDataConflict") or ""),
          str(d.get("levelDataConflict"))[:130])
    os.remove(stale)
    check("洞穴分片读回的是洞穴地形", code == 200 and d.get("present") is True
          and d.get("location") == "cave", str(d)[:120])

    # 6) 越界与非法名：URL 里的集群名必须先跟磁盘对过
    code, r = call("GET", "/api/v1/clusters/NopeThere/token")
    check("不存在的集群令牌接口 404", code == 404 and "ProbeWorld" in json.dumps(r, ensure_ascii=False), str(r.get("error"))[:110])
    code, r = call("POST", "/api/v1/clusters", {"name": "..evil", "numShards": 1})
    check("带点的集群目录名被拒", code == 400 and "点开头" in json.dumps(r, ensure_ascii=False), str(r.get("error"))[:90])
    check("被拒的越界名字没建出目录", not os.path.isdir(os.path.join(KLEI, "..evil"))
          and not os.path.isdir(os.path.join(os.path.dirname(KLEI), "evil")), "")

    code, r = call("GET", "/api/v1/audit?limit=80")
    acts = [e["action"] for e in r.get("data", [])]
    check("开服/令牌/世界设定都留了审计", "cluster.create" in acts and "cluster.token" in acts
          and "world.update" in acts, str(acts[:16]))
    check("审计里没有令牌明文", secret not in json.dumps(r.get("data", []), ensure_ascii=False), "")


def cluster_settings_checks():
    """集群设置：建好之后改 cluster.ini 的玩法项。

    这一组盯三件事：读回来的值对不对、改完盘上是不是真的变了、
    以及密码原值绝不出现在响应里（它只该躺在 0600 的配置文件里）。
    """
    name = "ProbeWorld"
    ini = os.path.join(KLEI, name, "cluster.ini")

    code, r = call("GET", "/api/v1/clusters/%s/settings" % name)
    d = r.get("data", {})
    check("集群设置读得回来（建群时填的值）",
          code == 200 and d.get("displayName") == "探针开服服" and d.get("maxPlayers") == 9,
          "%s %s" % (code, str(d)[:160]))
    check("没设密码时如实报无密码", d.get("hasPassword") is False)
    check("设置响应里不带密码字段（明文没有可回显的来源）",
          "password" not in d and "cluster_password" not in json.dumps(r, ensure_ascii=False),
          str(d)[:120])

    code, r = call("PUT", "/api/v1/clusters/%s/settings" % name, {
        "displayName": "探针改名", "maxPlayers": 7, "tickRate": 30,
        "pauseWhenEmpty": True, "password": "probepass", "whitelistSlots": 2,
    })
    d = r.get("data", {})
    check("补丁写入并回读", code == 200 and d.get("displayName") == "探针改名"
          and d.get("maxPlayers") == 7 and d.get("tickRate") == 30 and d.get("hasPassword") is True,
          "%s %s" % (code, str(d)[:170]))
    check("改完的响应里同样没有密码明文", "probepass" not in json.dumps(r, ensure_ascii=False))
    body = open(ini, encoding="utf-8").read()
    check("cluster.ini 真的改了这几项",
          all(x in body for x in ("cluster_name = 探针改名", "max_players = 7",
                                  "tick_rate = 30", "pause_when_empty = true",
                                  "cluster_password = probepass", "whitelist_slots = 2")),
          body[:200])
    check("没提到的项原样留着（补丁不是整份覆盖）",
          "game_mode = survival" in body and "shard_enabled = true" in body, body[:160])
    check("写设置留了 .bak", os.path.isfile(ini + ".bak"))

    before = open(ini, encoding="utf-8").read()
    code, r = call("PUT", "/api/v1/clusters/%s/settings" % name, {"gameMode": "creative"})
    check("Klei 会静默忽略的模式值当场 400", code == 400, str(r.get("error"))[:110])
    code, r = call("PUT", "/api/v1/clusters/%s/settings" % name, {"maxPlayers": 17})
    check("超出模板上限的人数当场 400", code == 400, str(r.get("error"))[:110])
    code, r = call("PUT", "/api/v1/clusters/%s/settings" % name, {"whitelistSlots": 8})
    check("白名单位超过人数当场 400（否则有位置永远空着）", code == 400, str(r.get("error"))[:110])
    check("被拒的请求没动 cluster.ini", open(ini, encoding="utf-8").read() == before)

    code, r = call("PUT", "/api/v1/clusters/%s/settings" % name, {"password": ""})
    check("空密码是「没填」不是「清空」", code == 200 and r.get("data", {}).get("hasPassword") is True)
    code, r = call("PUT", "/api/v1/clusters/%s/settings" % name, {"clearPassword": True})
    check("清空密码要单独给 clearPassword", code == 200 and r.get("data", {}).get("hasPassword") is False)
    check("清空后文件里不再有旧密码", "probepass" not in open(ini, encoding="utf-8").read())

    code, r = call("GET", "/api/v1/clusters/NopeThere/settings")
    check("不存在的集群设置接口 404", code == 404 and "ProbeWorld" in json.dumps(r, ensure_ascii=False))

    code, r = call("GET", "/api/v1/audit?limit=80")
    acts = [e["action"] for e in r.get("data", [])]
    check("集群设置留了审计且没记密码明文", "cluster.settings" in acts
          and "probepass" not in json.dumps(r.get("data", []), ensure_ascii=False), str(acts[:16]))


def ports_checks():
    """游戏端口：向导的挑号预览、端口页的读盘、改号那一步的护栏。

    这一组只验"读"和"挡"，不验写盘：这台机器上没有 screen，面板问不到世界进程状态
    就会先拒掉任何端口改动——而"拒得对不对、有没有偷偷写半个文件"正是要验的点。
    真把号改到三份文件里是 .tools/ui_ports_check.py 的活（它带 screen 替身，在浏览器里跑）。
    """
    name = "ProbeWorld"
    root = os.path.join(KLEI, name)
    files = {
        "cluster.ini": os.path.join(root, "cluster.ini"),
        "Master": os.path.join(root, "Master", "server.ini"),
        "Caves": os.path.join(root, "Caves", "server.ini"),
    }

    def num(path, key):
        m = re.search(r"^\s*%s\s*=\s*(\d+)" % key, open(path, encoding="utf-8").read(), re.M)
        return int(m.group(1)) if m else 0

    disk_master = num(files["Master"], "server_port")
    disk_caves = num(files["Caves"], "server_port")
    disk_intent = num(files["cluster.ini"], "master_port")

    def snap():
        return tuple(open(p, encoding="utf-8").read() for p in files.values())

    code, r = call("GET", "/api/v1/ports/plan?shards=2")
    d = r.get("data", {})
    taken = d.get("taken", [])
    ports = [t.get("port") for t in taken]
    check("向导的挑号预览给了空闲号与区间",
          code == 200 and d.get("shards") == 2 and d.get("autoPort", 0) > 0
          and "-" in (d.get("autoRange") or ""), str(d)[:150])
    check("占用表把探针房间的游戏端口和配套端口都列出来",
          disk_master in ports and disk_intent in ports, str(ports)[:150])
    check("占用表说的是房间名与世界种类，不是目录名和码值",
          bool(taken) and all(t.get("room") and t.get("world") in ("地表", "洞穴")
                              and t.get("kindLabel") for t in taken), str(taken[:2])[:200])
    check("面板挑的号压不到已经占用的那组上",
          d.get("autoPort") not in ports and d.get("autoPort", 0) + 1 not in ports,
          str(d.get("autoPort")))

    code, r = call("GET", "/api/v1/ports/plan?shards=9")
    check("世界数量超出 1 或 2 当场 400", code == 400 and "世界数量" in str(r.get("error")),
          str(r.get("error"))[:90])

    code, r = call("GET", "/api/v1/clusters/%s/ports" % name)
    d = r.get("data", {})
    check("端口页读回来的号就是盘上那三个号",
          code == 200 and d.get("masterPort") == disk_master and d.get("intentPort") == disk_intent
          and d.get("cavesPort") == disk_caves and d.get("hasCaves") is True, str(d)[:170])
    check("配套端口就是地表端口加一", d.get("intentPort") == disk_master + 1, str(d)[:120])
    check("端口页指到 cluster.ini 这个文件", (d.get("path") or "").endswith("cluster.ini"),
          str(d.get("path"))[-40:])
    # 洞穴"去哪找地表"那一行：没写就必须读回空串。面板替用户补一个 127.0.0.1
    # 会同时抹掉线索（看不出这是坏了）和动力（端口页不会提示补）。
    m_caves_ip = re.search(r"^\s*master_ip\s*=\s*(\S+)",
                           open(files["Caves"], encoding="utf-8").read(), re.M)
    disk_ip = m_caves_ip.group(1).strip() if m_caves_ip else ""
    check("洞穴主机地址照抄盘上的那一行",
          str(d.get("cavesMasterIp") or "") == disk_ip,
          "接口给了 %r，盘上 %r" % (d.get("cavesMasterIp"), disk_ip))

    before = snap()
    bak_before = {k: os.path.isfile(p + ".bak") for k, p in files.items()}
    code, r = call("PUT", "/api/v1/clusters/%s/ports" % name, {"masterPort": 12345})
    check("问不到世界进程状态时拒绝改端口（宁可不改）",
          code == 500 and "没有写入端口" in str(r.get("error")), str(r.get("error"))[:140])
    check("被拒的这次三份文件一个字节都没动", snap() == before, "")
    check("被拒的这次没新留下 .bak",
          bak_before == {k: os.path.isfile(p + ".bak") for k, p in files.items()}, str(bak_before))

    # 建房间时自己填号：用户照着这个号去开云安全组，所以号必须原样落盘、
    # 并且立刻出现在别的房间的占用表里（否则第二个房间会挑中同一个号）。
    p2 = os.path.join(KLEI, "PortProbe")
    shutil.rmtree(p2, ignore_errors=True)
    code, r = call("POST", "/api/v1/clusters", {
        "name": "PortProbe", "numShards": 1, "installHelper": False, "gamePort": 12345,
        "game": {"displayName": "端口自测服"},
    })
    d = r.get("data", {})
    ini2 = open(os.path.join(p2, "cluster.ini"), encoding="utf-8").read() if os.path.isdir(p2) else ""
    ms2 = open(os.path.join(p2, "Master", "server.ini"), encoding="utf-8").read() if os.path.isdir(p2) else ""
    check("建房间时填的端口原样落盘（配套端口跟着 +1）",
          code == 200 and "server_port = 12345" in ms2 and "master_port = 12346" in ini2,
          str(r)[:150])
    check("建房间的回复里带上这个号",
          any(sh.get("port") == 12345 for sh in d.get("shards", [])), str(d.get("shards"))[:120])
    check("区间外的号要说一句它归用户管",
          "区间" in "\n".join(d.get("notes", [])), str(d.get("notes"))[:200])
    code, r = call("GET", "/api/v1/ports/plan?shards=1")
    ports = [(t.get("port"), t.get("room")) for t in r.get("data", {}).get("taken", [])]
    check("新房间的两个号立刻进占用表且说的是房间名",
          (12345, "端口自测服") in ports and (12346, "端口自测服") in ports, str(ports)[:170])
    code, r = call("POST", "/api/v1/clusters", {
        "name": "PortProbe2", "numShards": 1, "installHelper": False, "gamePort": 12346,
        "game": {"displayName": "压别人的配套号"},
    })
    check("压到别的房间配套端口时建群被拒", code == 400 and "12346" in str(r.get("error")),
          str(r.get("error"))[:130])
    check("被拒的这次没建出目录", not os.path.isdir(os.path.join(KLEI, "PortProbe2")), "")
    code, r = call("POST", "/api/v1/clusters", {
        "name": "PortProbe3", "numShards": 1, "installHelper": False, "gamePort": 999,
        "game": {"displayName": "太小的号"},
    })
    check("1024 以下的端口当场拒（要 root 权限）", code == 400 and "1024" in str(r.get("error")),
          str(r.get("error"))[:120])
    shutil.rmtree(p2, ignore_errors=True)

    code, r = call("GET", "/api/v1/audit?limit=80")
    acts = [e["action"] for e in r.get("data", [])]
    check("端口这一页没有偷偷写坏审计（改端口被拒时不留成功记录）",
          "cluster.ports" not in acts, str(acts[:12]))


def stream_call(path, timeout=15):
    """SSE 专用：once=1 时面板发完就关连接，所以 read() 会自然返回全部内容。

    不能用 call()：它假定响应是 JSON，而这里要验的正是帧的写法。
    """
    req = urllib.request.Request(BASE + path, method="GET",
                                 headers={"Authorization": AUTH, "Accept": "text/event-stream"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.headers.get("Content-Type", ""), resp.read().decode("utf-8")
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Content-Type", ""), e.read().decode("utf-8")
    except Exception as e:  # noqa: BLE001
        return -1, "", str(e)


# 一份按真机形态拼的日志：启动流水、令牌诊断、存档、广播报错、世界生成报错、进服，
# 最后一条是"引擎不该打但必须挡住"的令牌明文。
# 令牌用的是形状合法的假值——真令牌永远不进这个文件，也不进库。
LOG_SAMPLE = (
    "[00:00:00]: PersistRootStorage is now /home/ubuntu/.klei/DoNotStarveTogether/Cluster_1/Master/ \n"
    "[00:00:00]: Current time: Sat Oct 31 02:17:35 2026\n"
    "[00:00:00]: Version: 760944\n"
    "[00:00:01]: Starting Up\n"
    "[00:00:02]: Account Communication Success\n"
    "[00:00:03]: Token retrieved from: /home/ubuntu/.klei/DoNotStarveTogether/Cluster_1/cluster_token.txt\n"
    "[00:00:04]: Retrieved token from TokenPurpose, KU_fakeid001\n"
    "[00:00:05]: Serializing world: session/2026-10-31/02_17_35/\n"
    "[00:00:06]: [Error] Master Server Broadcast Error: [E_ROWID_EXIST]\n"
    "[00:00:07]: [string \"../scripts/map/storygen.lua\"]:865: Must specify a layout mode for your level.\n"
    "[00:00:08]: Client authenticated: (KU_abcdef01) 阿黎\n"
    "[00:00:09]: token=pds-g^KU_fakeid001^AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=^BBBBBBBBBBBBBBBBBBBBBBBBBBBB\n"
)
FAKE_TOKEN_TAIL = "AAAA" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="


def log_checks():
    """日志三个接口 + 审计中文标签。这里验的全是"响应里出现的字"。"""
    path = os.path.join(KLEI, "Cluster_1", "Master", "server_log.txt")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    # 这份样本里有引擎的"拒绝启动"指纹行，留着会被后面的启动检查当真。
    # 但直接删掉会把 ui_dev 播种的那份夹具一起清空，日志页从此一片空白——
    # 所以先记下原内容，最后原样放回去。
    prev = None
    if os.path.exists(path):
        with open(path, "rb") as fh:
            prev = fh.read()
    with open(path, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(LOG_SAMPLE)

    log_base = "/api/v1/clusters/Cluster_1/shards/Master/log"
    try:
        code, r = call("GET", log_base)
        d = r.get("data", {})
        entries = d.get("entries") or []
        check("日志接口 200 且回结构化条目", code == 200 and len(entries) == 12,
              "%s %s" % (code, str(d)[:120]))
        check("日志页头有中文种类与版本号",
              d.get("kindLabel") == "服务端日志" and d.get("gameVersion") == "760944", str(d)[:90])
        check("换算出了本轮启动的墙上时间", bool(d.get("startAt")) and not d.get("startNote"),
              str(d.get("startAt")) + "/" + str(d.get("startNote"))[:60])
        check("每条都带中文级别与事件名，不露裸码值",
              all(e.get("levelLabel") and e.get("levelLabel") != e.get("level") for e in entries),
              str([(e.get("level"), e.get("levelLabel")) for e in entries[:3]]))
        # 行前缀要短、要能和筛选下拉对上：短名空着界面就不挂前缀，整句挂上去把原文挤出屏幕。
        check("归类行同时给短名和整句，未归类行两个都空",
              all(
                  (e.get("event") == "plain" and not e.get("eventName") and not e.get("eventLabel"))
                  or (e.get("eventName") and e.get("eventLabel") and e.get("eventName") != e.get("eventLabel"))
                  for e in entries
              ),
              str([(e.get("event"), e.get("eventName"), e.get("eventLabel")) for e in entries[:4]])[:170])

        body = json.dumps(r, ensure_ascii=False)
        check("令牌明文没出接口", "pds-g" not in body and FAKE_TOKEN_TAIL not in body, body[:160])
        check("令牌那一行标了隐去说明", "[已隐去Klei令牌]" in body, "")
        check("引擎的令牌诊断行（只给路径）不被误伤",
              "Token retrieved from:" in body and "TokenPurpose" in body, "")

        code, r = call("GET", log_base + "?level=error")
        d = r.get("data", {})
        check("级别筛选是下限而不是等于",
              code == 200 and d.get("returned") == 2 and d.get("scanned") == 12,
              str(d)[:110])
        code, r = call("GET", log_base + "?event=save")
        check("事件筛选精确命中存档行",
              r.get("data", {}).get("returned") == 1
              and r["data"]["entries"][0]["event"] == "save", str(r.get("data"))[:120])
        code, r = call("GET", log_base + "?q=" + urllib.parse.quote("阿黎", safe=""))
        check("关键词能命中玩家名",
              r.get("data", {}).get("returned") == 1
              and r["data"]["entries"][0].get("userid") == "KU_abcdef01", str(r.get("data"))[:130])

        for bad in ("?kind=syslog", "?lines=99999", "?level=fatal", "?event=no_such", "?pollMs=5"):
            code, r = call("GET", log_base + bad)
            check("非法参数 %s 当场 400" % bad,
                  code == 400 and bool(r.get("error")), "%s %s" % (code, str(r.get("error"))[:80]))

        code, r = call("GET", "/api/v1/log-events")
        d = r.get("data", {})
        check("筛选下拉有种类/级别/事件三份目录",
              code == 200 and len(d.get("kinds") or []) == 2 and len(d.get("levels") or []) == 3
              and len(d.get("events") or []) > 20,
              str({k: len(v) for k, v in d.items() if isinstance(v, list)})[:120])
        check("事件目录每条都有中文说明",
              all(e.get("label") and e.get("label") != e.get("code") for e in d.get("events") or []), "")
        check("事件目录每条都有短名，且短名不是码值或整句",
              all(e.get("name") and e.get("name") != e.get("code") and e.get("name") != e.get("label")
                  for e in d.get("events") or []),
              str([(e.get("code"), e.get("name")) for e in (d.get("events") or [])[:4]])[:140])
        bad_codes = []
        for e in d.get("events") or []:
            c, _ = call("GET", log_base + "?event=" + e["code"])
            if c != 200:
                bad_codes.append(e["code"])
        check("事件目录里的码都能被筛选接受", not bad_codes, str(bad_codes[:5]))

        code, ct, raw = stream_call(log_base + "/stream?once=1")
        check("SSE 用 event-stream 且关掉了反代缓冲",
              code == 200 and ct.startswith("text/event-stream"), "%s %s" % (code, ct))
        events = re.findall(r"^event: (\S+)$", raw, flags=re.M)
        check("帧序是 meta → line… → done",
              events[:1] == ["meta"] and events[-1:] == ["done"] and events.count("line") == 12,
              str(events[:4]) + "…" + str(events[-2:]))
        check("SSE 响应里同样没有令牌明文", "pds-g" not in raw, raw[:120])

        # 聊天文件有两种常态：引擎还没写过（首启、或这个房间根本没开过聊天）。
        # ui_dev 现在会播种它，所以"有内容"和"还没有"两种状态都得当场造出来验，
        # 不能靠目录里碰巧缺着——那样这条检查会随重启顺序忽过忽败。
        chat = os.path.join(KLEI, "Cluster_1", "Master", "server_chat_log.txt")
        code, r = call("GET", log_base + "?kind=chat")
        d = r.get("data", {})
        ents = d.get("entries") or []
        check("聊天日志读到的是聊天那份文件",
              code == 200 and d.get("kindLabel") == "聊天与公告" and len(ents) >= 6,
              "%s %s" % (code, str(d)[:110]))
        check("公告与玩家发言各归各类，发言者被拆出名字",
              any(e.get("event") == "announce" for e in ents)
              and any(e.get("event") == "chat" and e.get("player") for e in ents),
              str([(e.get("event"), e.get("player")) for e in ents[:4]])[:150])
        check("聊天条目也都带中文事件名，不露裸码值",
              all(e.get("eventLabel") and e.get("eventLabel") != e.get("event")
                  and e.get("eventName") and e.get("eventName") != e.get("eventLabel") for e in ents),
              str([(e.get("event"), e.get("eventName"), e.get("eventLabel")) for e in ents[:3]])[:160])

        if os.path.exists(chat):
            hidden = chat + ".probe-hidden"
            os.replace(chat, hidden)
            try:
                code, r = call("GET", log_base + "?kind=chat")
                check("没有聊天文件时报 503 并说清原因",
                      code == 503 and "还没" in str(r.get("error")),
                      "%s %s" % (code, str(r.get("error"))[:90]))
            finally:
                os.replace(hidden, chat)
        else:
            check("没有聊天文件时报 503 并说清原因",
                  code == 503 and "还没" in str(r.get("error")),
                  "%s %s（这条走的是本来就没有文件的分支）" % (code, str(r.get("error"))[:60]))

        # 未知分片必须挡在读文件之前。
        code, r = call("GET", "/api/v1/clusters/Cluster_1/shards/Nope/log")
        check("未知分片的日志 404", code == 404, "%s %s" % (code, str(r.get("error"))[:80]))
    finally:
        # 样本里有引擎的"拒绝启动"指纹行，留着会被后面的启动检查当真；原内容放回去。
        if prev is None:
            os.remove(path)
        else:
            with open(path, "wb") as fh:
                fh.write(prev)

    # 操作记录：界面只看 actionLabel，而它必须覆盖库里出现过的每一个码。
    code, r = call("GET", "/api/v1/audit?limit=200")
    rows = r.get("data") or []
    leaked = [e["action"] for e in rows if not e.get("actionLabel") or e["actionLabel"] == e["action"]]
    check("每条操作记录都有中文动作名", code == 200 and rows and not leaked, str(leaked[:6]))
    check("动作名里没有没登记说明这种兜底话",
          not any("没登记" in (e.get("actionLabel") or "") for e in rows),
          str([e.get("actionLabel") for e in rows if "没登记" in (e.get("actionLabel") or "")][:3]))

    code, r = call("GET", "/api/v1/audit-actions")
    d = r.get("data", {})
    check("动作目录有族与具体动作两层",
          code == 200 and len(d.get("families") or []) >= 8 and len(d.get("actions") or []) >= 20
          and "前缀" in (d.get("note") or ""), str(d)[:120])
    bad_codes = []
    for it in d.get("actions") or []:
        c, _ = call("GET", "/api/v1/audit?action=" + urllib.parse.quote(it["code"], safe=""))
        if c != 200:
            bad_codes.append(it["code"])
    check("动作目录里的取值都能被列表接口用", not bad_codes, str(bad_codes[:5]))
    code, r = call("GET", "/api/v1/audit?action=shard")
    check("少写点的族前缀被 400 拒绝并指路目录",
          code == 400 and "audit-actions" in str(r.get("error")), "%s %s" % (code, str(r.get("error"))[:90]))
    code, r = call("GET", "/api/v1/audit?action=shard.")
    check("族前缀按前缀捞出一族",
          code == 200 and all(e["action"].startswith("shard.") for e in r.get("data") or []),
          str([e["action"] for e in r.get("data") or []][:6]))

    audit_range_checks(call, check)


# 玩家事件表的样本按真机形状拼。四处细节是这份夹具存在的理由：
# 一是聊天文件里那条进服抄本带的是引擎二进制里的标签形状（[Join Announcement] %s），
# 服务端那份是同一句、同一个时钟——面板要把它们合成一记，这是"两份抄本合成一记"
# 在真面板上唯一的验法；二是另一条离开抄本用的是实测见过的 [Announcement] 标签，
# 它会被归成"公告"而不是"玩家离开"，所以那一记只能来自服务端那份。
# 引擎在聊天文件里到底用哪种标签，本机没有抓到过真样本（只有二进制里的模板串），
# 所以两种形状都得演一遍：不管走哪条路，"玩家进服 1""玩家离开 1"都不许变成 2。
# 三是身份链（连接 → 验证 → 落地）只写在服务端那一份里，界面那三列
# （Steam ID / IP / 角色）全靠它，所以夹具必须把这几条真行摆进去——
# 不然那三列全空着也能"通过"。
# 四是阿黎只有公告行、没有连接行：她那一行的 IP 与编号必须留空。
# 把别人的 IP 安到她头上比空着严重得多，这条是这张表的底线。
# 最后一条服务端行是令牌明文：它不是玩家行为，绝不该出现在表里的任何一行。
EV_SERVER_SAMPLE = (
    "[00:00:00]: Current time: Sat Oct 31 02:17:35 2026\n"
    "[00:00:00]: Version: 760944\n"
    "[00:01:00]: New incoming connection 203.0.113.11|16537 <5242766981745090001>\n"
    "[00:01:00]: Client connected from 203.0.113.11|16537 <5242766981745090001>\n"
    "[00:01:00]: [ClientObject] Added (pre-auth) on server: guid=5242766981745090001\n"
    "[00:01:00]: Client authenticated: (KU_1a2b3c4d) 小满\n"
    "[00:01:00]: [ClientObject] Initialized (authenticated) on server: guid=5242766981745090001 "
    "userid=KU_1a2b3c4d netid=76561198800000011 admin=1\n"
    "[00:01:01]: Spawn request: wilson from 小满\n"
    "[00:01:01]: 小满 has joined the game\n"
    "[00:02:00]: 小满 was killed by SpiderdenGuardian. They became a spooky ghost!\n"
    "[00:02:40]: 小满 went away for a bit.\n"
    "[00:03:00]: [Kick] (KU_9z8y7x6w) 房主 kicked (KU_5e6f7g8h) 阿黎\n"
    "[00:03:10]: 阿黎 has left the game\n"
    "[00:03:20]: token=pds-g^KU_1a2b3c4d^" + FAKE_TOKEN_TAIL + "^BBBBBBBBBBBBBBBBBBBBBBBBBBBB\n"
)
EV_CHAT_SAMPLE = (
    "[00:01:01]: [Join Announcement] 小满 has joined the game\n"
    "[00:02:20]: (KU_1a2b3c4d) 小满: 谁来帮忙打龙\n"
    "[00:03:10]: [Announcement] 阿黎 has left the game\n"
)


def events_checks():
    """GET …/shards/{shard}/events：玩家事件表那九列、筛选与分页的依据。"""
    server = os.path.join(KLEI, "Cluster_1", "Master", "server_log.txt")
    chat = os.path.join(KLEI, "Cluster_1", "Master", "server_chat_log.txt")
    os.makedirs(os.path.dirname(server), exist_ok=True)
    prev = {}
    for path, text in ((server, EV_SERVER_SAMPLE), (chat, EV_CHAT_SAMPLE)):
        prev[path] = open(path, "rb").read() if os.path.exists(path) else None
        with open(path, "w", encoding="utf-8", newline="\n") as fh:
            fh.write(text)

    base = "/api/v1/clusters/Cluster_1/shards/Master/events"
    try:
        code, r = call("GET", base + "?size=50")
        d = r.get("data") or {}
        rows = d.get("rows") or []
        check("事件表 200 且表里有行", code == 200 and len(rows) > 0,
              "%s %s 行" % (code, len(rows)))
        # 底部那句"共 N 条"吃的就是 matched。这里 size 大到一页装得下全部，
        # 所以行数必须等于 matched；不相等就是报总数的和分页切的是两段东西。
        check("一页装得下时行数就等于总数那句",
              d.get("matched") == len(rows) and d.get("pages") == 1,
              "matched=%s 行数=%s pages=%s" % (d.get("matched"), len(rows), d.get("pages")))
        # 合抄本只合得掉一条：进服那条聊天里用的是 [Join Announcement]（被认成"玩家进服"，
        # 于是跟服务端那一份撞在同一记下）；离开那条用的是通用 [Announcement] 标签，
        # 引擎的这句本来就被归成"公告"而不是"玩家离开"，压根不在这张表里竞争。
        # 两种标签形状各演一条，量的就是这个差别。
        check("两份文件里同一句进服抄本被合成一记，通用公告不冒充玩家离开",
              d.get("dupled") == 1 and not any(e.get("event") == "announce" for e in rows),
              "dupled=%s 表里有公告行=%s" % (
                  d.get("dupled"), any(e.get("event") == "announce" for e in rows)))
        # 人：小满（进服/死亡/离开又回来）、阿黎（离开那一句）、房主（动手踢人的）。
        check("涉及的人数对得上（界面那句「涉及 M 个人」的依据）",
              d.get("people") == 3, "people=%s" % d.get("people"))
        check("回看窗口就是默认那档，且筛的行数不会超出两份文件各自的窗口",
              d.get("window") == 500 and 0 < (d.get("scanned") or 0) <= 2 * d.get("window"),
              "window=%s scanned=%s" % (d.get("window"), d.get("scanned")))

        xm = [e for e in rows if e.get("player") == "小满"]
        check("同一个人的行都接上了账户编号",
              bool(xm) and all(e.get("userid") == "KU_1a2b3c4d" for e in xm),
              str([(e.get("player"), e.get("userid")) for e in xm])[:160])
        check("Steam ID / IP / 角色是从连接那几条真行接出来的，不是猜的",
              bool(xm) and all(e.get("steamid") == "76561198800000011"
                               and e.get("ip") == "203.0.113.11"
                               and e.get("prefab") == "wilson" for e in xm),
              str([(e.get("steamid"), e.get("ip"), e.get("prefab")) for e in xm])[:170])
        # 阿黎这一轮没有连接行：她的 IP 与编号必须空着（见文件头第四条）。
        ali = [e for e in rows if "阿黎" in ((e.get("player") or "") + (e.get("target") or ""))]
        check("只从公告里出现过的人不给她编 IP 与编号",
              bool(ali) and all(not e.get("ip") and not e.get("steamid") for e in ali),
              str([(e.get("event"), e.get("ip"), e.get("steamid")) for e in ali])[:170])
        kick = [e for e in rows if e.get("event") == "player_kick"]
        check("踢人记在动手的人名下、被踢的记成对象",
              len(kick) == 1 and kick[0].get("player") == "房主" and kick[0].get("target") == "阿黎",
              str([(e.get("player"), e.get("target")) for e in kick]))

        check("每一行都带中文短名和整句说明，不露裸码值",
              all(e.get("name") and e.get("label")
                  and e.get("name") != e.get("event") and e.get("label") != e.get("event")
                  for e in rows),
              str([(e.get("event"), e.get("name"), e.get("label")) for e in rows[:2]])[:170])
        tones = sorted({e.get("tone") or "" for e in rows})
        check("色档只可能是界面认的那四档",
              all(t in ("", "warn", "bad", "ok") for t in tones), str(tones))
        check("行按时间从新到旧排（界面把最新一行放在最上）",
              all(rows[i].get("clock", "") >= rows[i + 1].get("clock", "")
                  for i in range(len(rows) - 1)),
              str([e.get("clock") for e in rows])[:150])
        check("每一行都带时钟，聊天来的行也换算出了墙上时间",
              all(e.get("clock") and e.get("at") for e in rows),
              str([(e.get("kind"), e.get("clock"), bool(e.get("at"))) for e in rows[:3]]))
        # 聊天文件没有 Current time 行，墙上时间得从服务端那份借；借不到时 at 为空，
        # 界面那一栏就显示 —，所以起始时间这一句必须自己解得开。
        rs = d.get("roundStart") or ""
        check("本轮起始时间给得出且解得开",
              bool(rs) and datetime.strptime(rs, "%Y-%m-%dT%H:%M:%SZ") is not None, str(rs))
        body = json.dumps(r, ensure_ascii=False)
        check("令牌明文没进事件表", "pds-g" not in body and FAKE_TOKEN_TAIL not in body, body[:120])
        check("服务端那几条身份行没有当成玩家行为列出来",
              not any(e.get("raw", "").startswith(("Client connected", "New incoming",
                                                   "[ClientObject]", "Spawn request"))
                      for e in rows),
              str([e.get("raw") for e in rows
                   if e.get("raw", "").startswith("[ClientObject]")])[:120])

        # ---- 分页：界面底部"第 x–y 条 / 共 N 条"就是这几个数拼的 ----
        code, first = call("GET", base + "?size=3&page=1")
        code, second = call("GET", base + "?size=3&page=2")
        d1, d2 = first.get("data") or {}, second.get("data") or {}
        total = d1.get("matched") or 0
        check("每页 3 行时页数等于向上取整",
              code == 200 and len(d1.get("rows") or []) == 3 and d1.get("pages") == (total + 2) // 3,
              "matched=%s pages=%s" % (total, d1.get("pages")))
        # 这条钉的是"假下一页"：翻页后总数不许变、内容不许重。
        check("翻页后总数不变、内容不重",
              d2.get("matched") == total and len(d2.get("rows") or []) > 0
              and not ({e["offset"] for e in d1.get("rows") or []}
                       & {e["offset"] for e in d2.get("rows") or []}),
              str([(d1.get("page"), d2.get("page"), d2.get("matched"))]))
        code, r = call("GET", base + "?size=3&page=999")
        d3 = r.get("data") or {}
        check("页码越界夹到最后一页并把实际页号回写（不报空页也不 400）",
              code == 200 and d3.get("page") == d3.get("pages") and bool(d3.get("rows")),
              "%s page=%s pages=%s 行=%s" % (
                  code, d3.get("page"), d3.get("pages"), len(d3.get("rows") or [])))

        # ---- 筛选：界面上每一个条件都得在后端真的生效 ----
        code, r = call("GET", base + "?event=player_death&size=50")
        dd = r.get("data") or {}
        check("按动作筛出来只剩那一种动作",
              code == 200 and bool(dd.get("rows"))
              and all(e.get("event") == "player_death" for e in dd.get("rows") or []),
              str([e.get("event") for e in dd.get("rows") or []]))
        code, r = call("GET", base + "?player=%E5%B0%8F%E6%BB%A1&size=50")
        dp = r.get("data") or {}
        check("按人名筛只出这个人（含被踢的那一行，人是谁写在对象栏）",
              code == 200 and bool(dp.get("rows"))
              and all("小满" in ((e.get("player") or "") + (e.get("target") or "")
                                 + (e.get("userid") or "")) for e in dp.get("rows") or []),
              str([(e.get("player"), e.get("target")) for e in dp.get("rows") or []])[:150])
        code, r = call("GET", base + "?q=203.0.113.11&size=50")
        dq = r.get("data") or {}
        check("顺着 IP 能捞出这个人（关键词盖住身份列是这张表存在的意义）",
              code == 200 and bool(dq.get("rows"))
              and all(e.get("ip") == "203.0.113.11" for e in dq.get("rows") or []),
              str([(e.get("player"), e.get("ip")) for e in dq.get("rows") or []])[:150])
        code, r = call("GET", base + "?q=%E6%89%93%E9%BE%99&size=50")
        dk = r.get("data") or {}
        check("默认不看发言时，只在发言里出现的关键词筛不出行",
              code == 200 and not (dk.get("rows") or []), str(dk.get("matched")))

        # ---- 发言那一档：界面那个开关让总数从 11 变 16 的依据 ----
        off = d.get("matched") or 0
        code, r = call("GET", base + "?size=50&chat=1")
        dc = r.get("data") or {}
        check("打开发言后总数变大，且多出来的确实来自聊天那份",
              code == 200 and (dc.get("matched") or 0) > off
              and any(e.get("kind") == "chat" for e in dc.get("rows") or []),
              "关=%s 开=%s" % (off, dc.get("matched")))
        check("默认那一档把发言收掉时说了实话（头部那枚提示的依据）",
              d.get("chatHidden") == 1 and not any(e.get("kind") == "chat" for e in rows),
              "chatHidden=%s 表里有聊天行=%s" % (
                  d.get("chatHidden"), any(e.get("kind") == "chat" for e in rows)))

        # ---- 窗口收窄：头部"每份文件回看 N 行"与底部那句必须自洽 ----
        code, r = call("GET", base + "?lines=5&size=50")
        dn = r.get("data") or {}
        check("窗口收到 5 行时，筛的行数不超过两份文件各自的窗口",
              code == 200 and dn.get("window") == 5 and 0 < (dn.get("scanned") or 0) <= 10,
              "window=%s scanned=%s" % (dn.get("window"), dn.get("scanned")))
        check("窗口切掉了更早的行就明说（不许把截断演成完整）",
              dn.get("older") is True and "最近 5 行" in (dn.get("note") or ""),
              str([(dn.get("older"), (dn.get("note") or "")[:44])]))
        # 收窄只会让总数变小或持平，最新那行不会被切走。
        # （不写"已经筛出来的人不许消失"：身份行排在行为行之前，
        # 窗口一收就把最早那几记切掉是设计如此，不是 bug。）
        check("收窄窗口只会让总数变小或持平，最新那行不会被切走",
              (dn.get("matched") or 0) <= off
              and (dn.get("rows") or [{}])[0].get("clock") == rows[0].get("clock"),
              "共=%s（原 %s）最新=%s（原 %s）" % (
                  dn.get("matched"), off, (dn.get("rows") or [{}])[0].get("clock"),
                  rows[0].get("clock")))

        # ---- 假链接检查：界面"看原文"跳逐行日志带的就是 (种类, 事件) 这一对 ----
        bad = []
        for e in rows:
            url = ("/api/v1/clusters/Cluster_1/shards/Master/log?kind=" + e["kind"]
                   + "&event=" + urllib.parse.quote(e["event"], safe=""))
            c2, r2 = call("GET", url)
            got = [x.get("event") for x in (r2.get("data") or {}).get("entries") or []]
            if c2 != 200 or e["event"] not in got:
                bad.append("%s/%s" % (e["kind"], e["event"]))
        check("事件表里每一行都能跳回逐行日志筛出原文", not bad, str(bad[:4]))

        # ---- 参数校验：写错必须当场 400，不能默默按默认值跑 ----
        for bad_q in ("?lines=0", "?lines=abc", "?lines=99999", "?size=0", "?size=99999",
                      "?page=0", "?page=x", "?event=not_a_code"):
            code, r = call("GET", base + bad_q)
            check("非法参数 %s 当场 400" % bad_q,
                  code == 400 and bool(r.get("error")), "%s %s" % (code, str(r.get("error"))[:70]))
        code, r = call("GET", "/api/v1/clusters/Cluster_1/shards/Nope/events")
        check("未知分片的事件表 404", code == 404, "%s %s" % (code, str(r.get("error"))[:70]))

        # 两份文件都没写出来是"世界还没跑过"的常态：给 503 并说清原因。
        # 报 404 会让前端当成地址写错、也不去重试。
        moved = []
        try:
            for path in (server, chat):
                hidden = path + ".probe-hidden"
                os.replace(path, hidden)
                moved.append((path, hidden))
            code, r = call("GET", base)
            check("两份日志都还没有时报 503 并说清原因",
                  code == 503 and "还没" in str(r.get("error")),
                  "%s %s" % (code, str(r.get("error"))[:90]))
        finally:
            for path, hidden in moved:
                os.replace(hidden, path)
    finally:
        for path, saved in prev.items():
            if saved is None:
                if os.path.exists(path):
                    os.remove(path)
            else:
                with open(path, "wb") as fh:
                    fh.write(saved)


def audit_range_checks(call, check):
    # 时间区间这条轴要拿"真数据自己核对"，不能只看条数变没变：
    # 一次全量、一次带区间，两次的 at 摆在一起才能证明边界是含头不含尾。
    now = datetime.now(timezone.utc).replace(microsecond=0)
    fmt = lambda d: urllib.parse.quote(d.strftime("%Y-%m-%dT%H:%M:%SZ"))
    code, r = call("GET", "/api/v1/audit?limit=200")
    all_rows = r.get("data") or []
    check("不带时间参数时是全量", code == 200 and len(all_rows) >= 1, "给了 %d 条" % len(all_rows))
    if not all_rows:
        return
    times = sorted(e["at"] for e in all_rows)

    # 区间放到未来：一条都不该有。这证明筛选真的在筛，而不是永远返回全量。
    code, r = call("GET", "/api/v1/audit?since=" + fmt(now + timedelta(days=2)))
    check("since 在未来时一条都不给",
          code == 200 and (r.get("data") or []) == [], "%s %s" % (code, str(r)[:120]))

    # 中点切开：带 since 的那一份 + 带 until 的那一份，合起来正好是全量，不重不漏。
    mid = times[len(times) // 2]
    code, a = call("GET", "/api/v1/audit?limit=200&since=" + fmt(datetime.strptime(mid, "%Y-%m-%dT%H:%M:%SZ")))
    code2, b = call("GET", "/api/v1/audit?limit=200&until=" + fmt(datetime.strptime(mid, "%Y-%m-%dT%H:%M:%SZ")))
    ids_a = sorted(e["id"] for e in a.get("data") or [])
    ids_b = sorted(e["id"] for e in b.get("data") or [])
    ids_all = sorted(e["id"] for e in all_rows)
    check("since 与 until 两半不重不漏地拼回全量（含头不含尾）",
          code == 200 and code2 == 200 and sorted(ids_a + ids_b) == ids_all and not set(ids_a) & set(ids_b),
          "前半 %d 条 / 后半 %d 条 / 全量 %d 条" % (len(ids_a), len(ids_b), len(ids_all)))
    check("带 since 的那一份里每条都在区间内",
          all(e["at"] >= mid for e in a.get("data") or []),
          str([e["at"] for e in a.get("data") or []][:4]))

    # 区间和动作族是"并且"：族对但时间在区间外，也不许出现。
    fam = next((e["action"] for e in all_rows if "." in e["action"]), "")
    pre = fam.split(".")[0] + "."
    code, r = call("GET", "/api/v1/audit?action=" + urllib.parse.quote(pre, safe="")
                   + "&since=" + fmt(now + timedelta(days=1)))
    check("动作族和时间区间同时生效（并且，不是或者）",
          code == 200 and (r.get("data") or []) == [], "%s %s" % (code, str(r)[:120]))

    for bad, why in [("?since=2026-11-02", "只写日期不给时刻"),
                     ("?since=%2026-11-02T00:00:00Z", "时刻没带时区"),
                     ("?since=" + fmt(now) + "&until=" + fmt(now - timedelta(hours=1)), "两头写反"),
                     ("?since=" + fmt(now) + "&until=" + fmt(now), "两头同一时刻")]:
        code, r = call("GET", "/api/v1/audit" + bad)
        check("%s 被 400 拒绝并说中文" % why,
              code == 400 and str(r.get("error") or "").strip() != "", "%s %s" % (code, str(r)[:120]))



def raw_get(path):
    """下载接口返回的是文件字节，不是 JSON 信封，得单独走一条路。"""
    req = urllib.request.Request(BASE + path, headers={"Authorization": AUTH})
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:  # noqa: BLE001
        return -1, str(e).encode("utf-8")


def tar_members(blob):
    """把下载到的 tar.gz 摊成 {名字: 内容}，令牌这类断言只能对着解压内容查。"""
    out = {}
    with tarfile.open(fileobj=io.BytesIO(blob), mode="r:gz") as tf:
        for m in tf.getmembers():
            # 目录条目不进这个字典：清单只登记文件，两边数量才配得上直接比。
            if not m.isfile():
                continue
            fh = tf.extractfile(m)
            out[m.name] = fh.read() if fh else b""
    return out


def task_of(tid, timeout=60.0):
    """等一条任务跑到终态。备份走的是常驻 worker，这一步同时证明 main.go 真的把线接上了。"""
    deadline = time.time() + timeout
    last = {}
    while time.time() < deadline:
        c, r = call("GET", "/api/v1/tasks?limit=40")
        if c == 200:
            last = {t["id"]: t for t in (r.get("data") or [])}.get(tid, {})
            if last.get("status") in ("succeeded", "failed", "canceled"):
                return last
        time.sleep(0.25)
    return last


def backup_page():
    c, r = call("GET", "/api/v1/backups")
    return c, (r.get("data") or {})


def one_backup(cluster):
    """提交一次手工备份并等它跑完，回 (任务行, 接口回复)。"""
    c, r = call("POST", "/api/v1/clusters/%s/backups" % cluster)
    d = r.get("data") or {}
    tid = d.get("taskId") or 0
    if not tid:
        return None, (c, r)
    return task_of(tid), (c, r)


def backup_checks():
    """存档快照端到端：接口只入队，真打包的是常驻 worker。

    单测里 worker 是同进程直接调的，看不出 main.go 有没有把 Backups 和 KeepFunc 接上；
    漏一根线的现象是"接口照回 202，队列永远不动"，只有跑真进程才看得见。
    """
    marker = "TOKEN-MUST-NOT-BE-BACKED-UP"

    # 1) 空页也要出得来，并且告诉用户下一步做什么。
    code, page = backup_page()
    pol = page.get("policy") or {}
    check("快照列表可读", code == 200, str(page)[:120])
    check("列表页给出房间名单", "Cluster_1" in (page.get("clusters") or []), str(page.get("clusters"))[:90])
    check("开局没有快照时给出下一步", page.get("someBackups") is False
          and "先点「备份存档」" in (page.get("note") or ""), (page.get("note") or "")[:150])
    check("保留上限说的是配置文件那份", pol.get("keep") == 3 and "配置" in (pol.get("keepSource") or ""), str(pol)[:170])
    note0 = page.get("note") or ""
    check("快照目录写清了，磁盘余量要么给数要么写未知", bool(pol.get("dir"))
          and ("磁盘还剩 未知" in note0 or any(u in note0 for u in (" B", " KB", " MB"))), note0[:170])
    check("空目录的已用量写成 0 B 而不是半句话", (pol.get("usedLabel") or "") == "0 B", str(pol)[:130])
    check("定时备份没开时说不开", page.get("scheduleHours") == 0 and page.get("scheduleRunning") is False,
          str(page)[:100])

    # 2) 带参数的任务不能从通用提交口进。
    for kind in ("backup.create", "backup.restore", "backup.import"):
        code, r = call("POST", "/api/v1/tasks", {"kind": kind, "confirm": True})
        body = json.dumps(r, ensure_ascii=False)
        check("通用任务口拒绝 %s 并指出入口" % kind,
              code == 400 and "通用提交口不接受" in body and "/api/v1/" in body, str(r.get("error"))[:150])

    # 3) 手工备份端到端。
    task, (code, reply) = one_backup("Cluster_1")
    d = reply.get("data") or {}
    check("备份请求 202 且带回任务壳子",
          code == 202 and d.get("taskId") and (d.get("task") or {}).get("statusLabel"), str(d)[:140])
    check("入队回话里说清了不直接动磁盘", "备份任务已入队" in (d.get("note") or ""), (d.get("note") or "")[:120])
    if not task:
        check("备份任务跑完", False, str(reply)[:160])
        return
    check("备份任务成功结束", task.get("status") == "succeeded", "%s %s" % (task.get("status"), str(task.get("message"))[:150]))
    msg = task.get("message") or ""
    check("收尾说清打了几个文件", "已生成" in msg and "个文件" in msg, msg[:170])
    # 没有 screen（本机 Windows）时说"问不到"，有 screen 且没开会话说"没有在跑的分片"：
    # 两句都算对，唯独不能说成"已经催过引擎存盘"。
    check("交代了这份档从哪来", "磁盘上现有的档" in msg and "已让" not in msg, msg[:190])
    check("任务消息里没有 Go 的英文原文",
          not any(x in msg for x in ("no such file", "fork/exec", "exit status", "context deadline")), msg[:170])

    code, page = backup_page()
    rows = page.get("snapshots") or []
    check("打完一份列表里就多一份", code == 200 and len(rows) == 1, str(len(rows)))
    row = rows[0] if rows else {}
    # 有了实测数字，"一份快照多大"那句必须跟着最近一份走。
    # 之前这里写死过一句"大约 140MB"，而云机实测 137.9MB 的存档目录打成的是 19.0MB 的包：
    # 拿猜来的数去算磁盘预算，会把成本夸到七倍。
    note1 = page.get("note") or ""
    check("大小说明跟着最近一份的实测走，不再喊猜来的 140MB",
          "按最近一份备份看" in note1 and "140MB" not in note1, note1[:190])
    sid = row.get("id", "")
    check("列表行给齐了编号、大小和文件数",
          bool(sid) and row.get("fileCount", 0) > 0 and row.get("archiveBytes", 0) > 0, str(row)[:160])
    check("列表行标出带存档的两个分片", sorted(row.get("saveShards") or []) == ["Caves", "Master"],
          str(row.get("saveShards")))
    check("来源给的是中文而不是 manual", row.get("labelLabel") == "手工备份" and row.get("label") == "manual",
          str(row.get("labelLabel")))
    check("这份可以用来恢复", row.get("restorable") is True, str(row.get("restoreNote"))[:120])
    check("列表行把三个按钮的路径都拼好了",
          row.get("downloadPath") == "/api/v1/backups/%s/download" % sid
          and row.get("restorePath") == "/api/v1/backups/%s/restore" % sid, str(row)[:150])

    # 4) 明细 + 校验 + 下载。
    code, r = call("GET", "/api/v1/backups/" + sid)
    det = r.get("data") or {}
    files = [f["path"] for f in ((det.get("snapshot") or {}).get("files") or [])]
    check("明细列出包里的文件", code == 200 and len(files) >= 8, "files=%d" % len(files))
    check("明细里能看到 slot 档位文件",
          any("Master/save/session/0000000000000001/slot-0001/server.dat" == f for f in files), str(files[:3]))
    check("令牌与面板通道文件没进包",
          not any("cluster_token" in f or "dstpanel_" in f for f in files), str(files[:4]))
    check("明细里没有引擎攒的旧日志副本（backup/server_log 那一层）",
          not any("/server_log/" in f or "/server_chat_log/" in f for f in files),
          str([f for f in files if "backup" in f])[:150])
    check("明细里有云机那种编号档位文件（16 位会话号 + 8 位编号 + .meta）",
          any(f.endswith("/0000000085") for f in files) and any(f.endswith("/0000000085.meta") for f in files),
          str([f for f in files if "/session/" in f and "slot-" not in f])[:170])
    labels = det.get("labels") or {}
    check("明细把大小和可用性问题翻成人话",
          labels.get("source") == "手工备份" and labels.get("restorable") == "可以用来恢复"
          and any(labels.get(k, "").endswith(u) for k in ("archiveBytes", "uncompressed")
                  for u in (" B", " KB", " MB")), str(labels)[:150])
    check("明细点出跳过了多少项", "刻意跳过了" in (det.get("note") or ""), (det.get("note") or "")[:150])

    code, r = call("POST", "/api/v1/backups/%s/verify" % sid)
    v = r.get("data") or {}
    check("校验整包跑得完", code == 200 and v.get("archiveOk") is True and v.get("checked", 0) == len(files),
          "%s checked=%s want=%d" % (code, v.get("checked"), len(files)))
    check("校验通过给的是能照着做的结论", "可以放心用来恢复" in (v.get("detail") or ""), str(v.get("detail"))[:140])

    code, blob = raw_get("/api/v1/backups/" + sid + "/download")
    members = tar_members(blob) if code == 200 else {}
    check("下载拿到的是一个解得开的包", code == 200 and len(members) == len(files),
          "%s got=%d want=%d" % (code, len(members), len(files)))
    body = b"".join(members.values())
    check("下载的包里没有令牌明文", marker.encode("utf-8") not in body
          and not any("cluster_token" in k for k in members), "")
    check("下载的包里没有面板通道文件", not any("dstpanel_" in k for k in members), str(list(members)[:4]))
    check("下载的包里没有引擎攒的旧日志副本",
          not any("/server_log/" in k or "/server_chat_log/" in k for k in members),
          str([k for k in members if "backup" in k])[:150])
    check("下载到的档位内容就是磁盘上那份",
          b"Master slot-0001" in members.get("Master/save/session/0000000000000001/slot-0001/server.dat", b""),
          str(members.get("Master/save/session/0000000000000001/slot-0001/server.dat"))[:80])

    # 5) 没存档的房间：备份照样打得出，但必须当场说"这份不能用来恢复"。
    task2, _ = one_backup("ProbeWorld")
    msg2 = (task2 or {}).get("message") or ""
    check("没有存档的房间备份仍算完成", (task2 or {}).get("status") == "succeeded", str((task2 or {}).get("status"))[:90])
    check("没有存档时喊出这份不能用", "不能用来恢复" in msg2, msg2[:190])

    # 6) 恢复的闸门：没确认 / 进程状态问不清 / 在跑，都要在入队之前拦下。
    code, r = call("POST", "/api/v1/backups/%s/restore" % sid, {"confirm": False})
    body = json.dumps(r, ensure_ascii=False)
    check("恢复没二次确认 400 并说要整棵替换",
          code == 400 and "整棵替换" in body, str(r.get("error"))[:140])
    code, r = call("POST", "/api/v1/backups/%s/restore" % sid, {"confirm": True, "targetCluster": "NopeThere"})
    check("恢复目标房间不在磁盘名单上 404", code == 404, "%s %s" % (code, str(r.get("error"))[:110]))
    code, r = call("POST", "/api/v1/backups/%s/restore" % sid, {"targetCluster": "Cluster_1"})
    check("恢复没确认时不看房间名也照样拦", code == 400, str(r.get("error"))[:110])

    code, r = call("POST", "/api/v1/backups/%s/restore" % sid, {"confirm": True})
    body = json.dumps(r, ensure_ascii=False)
    if code == 503:
        check("问不到进程状态时恢复关门并说清为什么", "screen" in body and "问不到" in body, str(r.get("error"))[:160])
        code, rr = call("GET", "/api/v1/tasks?limit=40")
        check("被拦住的恢复没进队列",
              not [t for t in (rr.get("data") or []) if t.get("kind") == "backup.restore"], str(body)[:120])
    elif code == 409:
        check("有分片在跑时恢复让路并点名是哪个", "先把它们" in body, str(r.get("error"))[:160])
    else:
        check("分片都关着时恢复入队", code == 202 and "整包校验" in body, "%s %s" % (code, body[:150]))
        tid = ((r.get("data") or {}).get("taskId")) or 0
        rt = task_of(tid) if tid else None
        check("恢复任务跑完并留下恢复前的安全备份",
              rt and rt.get("status") == "succeeded" and "before-restore" in (rt.get("message") or ""),
              str((rt or {}).get("message"))[:180])

    # 7) 保留上限：每打一份就按 Keep 收一次尾，最旧的先走。
    first_id = sid
    for _ in range(2):
        t, _ = one_backup("Cluster_1")
        if not t or t.get("status") != "succeeded":
            check("补打快照成功", False, str(t)[:150])
    code, page = backup_page()
    ids = [s2.get("id") for s2 in (page.get("snapshots") or [])]
    check("打完 4 份后按上限只留 3 份", len(ids) == 3, str(ids))
    check("被清掉的是最旧那一份", first_id not in ids, str(ids))
    check("清理真的把两个文件都删了",
          not any(os.path.isfile(os.path.join(pol.get("dir", ""), first_id + x))
                  for x in (".tar.gz", ".manifest.json")), first_id)
    used = (page.get("policy") or {}).get("usedLabel") or ""
    check("已用量报成可读的大小", bool(used) and "B" in used, used[:40])

    # 8) 保留上限和定时间隔能在面板设置里改，改完不用重启就跟着变。
    code, r = call("PUT", "/api/v1/settings", {"backup_keep": "2", "backup_schedule_hours": "6"})
    check("改保留上限与定时间隔", code == 200, "%s %s" % (code, str(r.get("error"))[:110]))
    code, page = backup_page()
    pol2 = page.get("policy") or {}
    check("保留上限立刻跟着改", pol2.get("keep") == 2 and "面板设置" in (pol2.get("keepSource") or ""), str(pol2)[:170])
    check("定时间隔开起来并说明要等满一个间隔",
          page.get("scheduleHours") == 6 and page.get("scheduleRunning") is True
          and "每 6 小时" in (page.get("note") or ""), (page.get("note") or "")[:170])
    t, _ = one_backup("Cluster_1")
    code, page = backup_page()
    check("上限改成 2 后下一次备份就收到 2 份",
          t and t.get("status") == "succeeded" and len(page.get("snapshots") or []) == 2,
          str([s2.get("id") for s2 in (page.get("snapshots") or [])]))

    # 9) 删除：说清删了什么，磁盘上两个文件一起走。
    victim = (page.get("snapshots") or [{}])[0].get("id", "")
    code, r = call("DELETE", "/api/v1/backups/" + victim)
    check("删除快照回话点名房间与文件数", code == 200 and victim in json.dumps(r, ensure_ascii=False),
          "%s %s" % (code, str(r.get("error"))[:120]))
    check("删除后列表少一份", len((backup_page()[1].get("snapshots") or [])) == 1, "")
    check("删除把归档和清单一起清掉",
          not any(os.path.isfile(os.path.join(pol.get("dir", ""), victim + x))
                  for x in (".tar.gz", ".manifest.json")), victim)
    code, r = call("DELETE", "/api/v1/backups/" + victim)
    check("删过的快照再删给的是可读的错误", code != 200, "%s %s" % (code, str(r.get("error"))[:110]))

    # 10) 越界与脏数据。
    for bad in ("..%2F..%2Fetc", "not-a-snapshot-id"):
        code, r = call("GET", "/api/v1/backups/" + bad)
        check("编号形状不对就 400 而不是去读磁盘（%s）" % bad, code == 400, str(r.get("error"))[:110])
    code, r = call("GET", "/api/v1/backups?cluster=Cluster_9")
    check("按不存在的房间过滤 404", code == 404, str(r.get("error"))[:100])
    code, blob = raw_get("/api/v1/backups/not-a-snapshot-id/download")
    check("下载接口不吃路径形状的编号", code in (400, 404), str(code))

    # 11) 导入外部存档：判断和提交两条路都在真进程上走一遍。
    import_checks(pol)

    # 12) 审计：这一页的每个动作都要留痕。
    code, r = call("GET", "/api/v1/audit?limit=120")
    acts = [e.get("action") for e in (r.get("data") or [])]
    check("备份/校验/下载/删除都留了审计",
          all(a in acts for a in ("backup.create", "backup.verify", "backup.download", "backup.delete")), str(acts[:16]))
    check("审计里被拒的动作也在列（失败要留痕）",
          any(e.get("action") == "backup.restore" and e.get("ok") is False for e in (r.get("data") or [])),
          str(acts[:16]))
    check("导入被拒的那次也留了痕",
          any(e.get("action") == "backup.import" and e.get("ok") is False for e in (r.get("data") or [])),
          str(acts[:16]))
    check("审计列表给动作配了中文说法",
          all(e.get("actionLabel") and e.get("actionLabel") != e.get("action")
              for e in (r.get("data") or []) if str(e.get("action", "")).startswith("backup.")), str(acts[:8]))


def import_package_bytes(kind):
    """造外部存档包。形状和 .tools/ui_import_zip.py 那套浏览器夹具共用一份实现，
    免得浏览器验的是 A 形状、接口探针验的是 B 形状，两边都绿却都不是真的。"""
    import ui_import_zip as fix
    files = {}
    if kind == "cluster":
        for role in ("Master", "Caves"):
            files.update(fix.world_tree("Cluster_77/" + role, role.upper()))
        files.update(fix.cluster_tree("Cluster_77"))
        # 朋友压包时顺手带进来的杂物，和浏览器夹具保持一致：它们该被点名丢掉。
        files["readme.txt"] = "这个档我打到第三年，冬天差点过不去\n"
        files["__MACOSX/._cluster.ini"] = "苹果压缩留下的垃圾\n"
    elif kind == "shard":
        files.update(fix.world_tree("Master"))
    elif kind == "notes":
        files = {"notes/plan.txt": "下次想建个森林房\n"}
    elif kind == "junk":
        return (b"\x89PNG\r\n\x1a\n" + ("这根本不是压缩包 " * 40).encode("utf-8"))
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        for n in sorted(files):
            z.writestr(n, files[n])
    return buf.getvalue()


def import_checks(pol):
    """存档导入的端到端：上传判断 → 提交 → worker 真的换到磁盘上。

    单测里判包和落盘是同进程调函数，看不出 multipart 字段名、202 壳子、
    临时上传目录的收放这几处接缝；这里全部走真 HTTP + 真常驻 worker。
    """
    prev_path = "/api/v1/backups/import/preview"
    imp_path = "/api/v1/backups/import"

    # 入口参数由后端一份说了算：界面拿的是同一份，这里验它自洽。
    code, page = backup_page()
    ip = page.get("import") or {}
    check("导入参数从列表页给全（上限/字段名/两条 URL）",
          code == 200 and ip.get("maxArchiveBytes") and ip.get("uploadField") == "save"
          and ip.get("previewPath") == prev_path and ip.get("importPath") == imp_path, str(ip)[:170])
    field = ip.get("uploadField") or "save"

    # 1) 根本不是压缩包：400 一句人话，不落临时文件。
    code, r = call_upload(prev_path, {}, "screenshot.png", import_package_bytes("junk"), field)
    msg = msg_of(r)
    check("非压缩包上传给中文说法而不是堆栈", code == 400 and "认不出" in msg and "格式" in msg,
          "%s %s" % (code, msg[:130]))

    # 2) 是压缩包但不是存档：200 出结论，三个数组键必须都在。
    code, r = call_upload(prev_path, {}, "notes.zip", import_package_bytes("notes"), field)
    p = r.get("data") or {}
    check("不是存档的包照样回结论并说清怎么办",
          code == 200 and p.get("canImport") is False and (p.get("problems") or []), str(p)[:170])
    check("结论里的数组字段一个都不能缺键（界面要先量长度再遍历）",
          all(isinstance(p.get(k), list) for k in ("shards", "dropped", "warnings", "problems")),
          str({k: type(p.get(k)).__name__ for k in ("shards", "dropped", "warnings", "problems")}))
    staged1 = (backup_page()[1].get("import") or {}).get("stagedCount")

    # 3) 合法的房间包：认出房间目录、两个世界、世界编号原样保留。
    code, r = call_upload(prev_path, {}, "friend-cluster.zip", import_package_bytes("cluster"), field)
    okp = r.get("data") or {}
    ref = okp.get("packageRef") or ""
    check("朋友给的房间包认出可导入", code == 200 and okp.get("canImport") is True and okp.get("root") == "Cluster_77",
          str(okp)[:170])
    check("包里的世界逐个点名并带上世界编号",
          sorted([s.get("shard") for s in (okp.get("shards") or [])]) == ["Caves", "Master"]
          and all(s.get("sessionId") == "AB12CD34EF56AB77" for s in (okp.get("shards") or [])),
          str(okp.get("shards"))[:130])
    check("模组配置跟着包走这件事写在提醒里",
          any("modoverrides" in w and "模组" in w for w in (okp.get("warnings") or [])),
          str(okp.get("warnings"))[:170])
    check("夹带的无关文件进的是丢掉名单，不是被静默忽略",
          any("readme" in d or "__MACOSX" in d for d in (okp.get("dropped") or []))
          or "readme.txt" not in json.dumps(okp, ensure_ascii=False),
          str(okp.get("dropped"))[:150])
    staged2 = (backup_page()[1].get("import") or {}).get("stagedCount")
    check("刚传上来的包当场计入临时上传占用", (staged2 or 0) > (staged1 or 0), "%s → %s" % (staged1, staged2))

    # 4) 闸门：房间不在磁盘名单 / 勾了换档却没二次确认 / 房间在跑，都得在入队之前挡下。
    code, r = call("POST", imp_path, {"package": ref, "cluster": "NopeThere", "restore": False})
    check("导入目标房间不在磁盘名单就 404", code == 404, "%s %s" % (code, msg_of(r)[:120]))
    code, r = call("POST", imp_path, {"package": ref, "cluster": "ProbeWorld", "restore": True, "confirm": False})
    check("勾了换进房间却没二次确认 400", code == 400 and "confirm" in json.dumps(r, ensure_ascii=False),
          "%s %s" % (code, msg_of(r)[:120]))

    # 5) 只导入不换档：任务跑完要能在列表里看到那份"外部导入的存档"。
    code, r = call("POST", imp_path, {"package": ref, "cluster": "ProbeWorld", "restore": False})
    d = r.get("data") or {}
    tid = d.get("taskId") or 0
    check("导入请求 202 且带回任务壳子", code == 202 and tid and (d.get("task") or {}).get("statusLabel"),
          "%s %s" % (code, str(d)[:130]))
    t = task_of(tid) if tid else {}
    check("导入任务跑完，来源写的是外部导入", (t or {}).get("status") == "succeeded", str((t or {}).get("message"))[:170])
    code, page = backup_page()
    row = [s for s in (page.get("snapshots") or []) if "imported" in (s.get("id") or "")]
    check("导入出来的快照进列表并标成外部导入",
          bool(row) and row[0].get("cluster") == "ProbeWorld" and "外部导入" in json.dumps(row[0], ensure_ascii=False),
          str(row)[:170])
    staged3 = (page.get("import") or {}).get("stagedCount")
    check("导入成功后那份包被收掉（磁盘账不虚增）", (staged3 or 0) < (staged2 or 0), "%s → %s" % (staged2, staged3))
    check("用完的上传包在盘上真的没了",
          not glob.glob(os.path.join(pol.get("dir", ""), "incoming", os.path.basename(ref))), ref[:60])

    # 6) 导入并换进房间：这一步会整棵替换存档目录，进程状态问不清时必须先拒。
    code, r = call_upload(prev_path, {}, "friend-shard.zip", import_package_bytes("shard"), field)
    sp = (r.get("data") or {}).get("packageRef") or ""
    code, r = call("POST", imp_path, {"package": sp, "cluster": "ProbeWorld", "restore": True, "confirm": True})
    msg = msg_of(r)
    if code == 503:
        check("问不到进程状态时换档导入不动磁盘并说清为什么", "问不到" in msg, msg[:150])
        check("被挡下的换档导入没进队列",
              not [t for t in (call("GET", "/api/v1/tasks?limit=40")[1].get("data") or [])
                   if t.get("kind") == "backup.import" and t.get("status") in ("queued", "running")], msg[:120])
    else:
        tid = (r.get("data") or {}).get("taskId") or 0
        check("换进房间的导入也接住了", code == 202 and tid, "%s %s" % (code, msg[:130]))
        t = task_of(tid) if tid else {}
        check("导入并恢复跑完", (t or {}).get("status") == "succeeded", str((t or {}).get("message"))[:190])
        sess = os.path.join(KLEI, "ProbeWorld", "Master", "save", "session", "AB12CD34EF56AB77")
        check("磁盘上换成了包里的世界编号目录", os.path.isdir(sess), sess)
        tok = os.path.join(KLEI, "ProbeWorld", "cluster_token.txt")
        token_text = io.open(tok, encoding="utf-8", errors="replace").read() if os.path.isfile(tok) else ""
        check("包里的令牌没有覆盖本机那一份", "KLEI-FAKE-TOKEN" not in token_text, tok)
        check("通道文件与引擎日志没有跟着包跑进来",
              not os.path.exists(os.path.join(KLEI, "ProbeWorld", "Master", "save", "dstpanel_queue.txt")), "")
        code, page = backup_page()
        check("恢复前自动打的那份安全备份也在列表里",
              any("before-restore" in (s.get("id") or "") for s in (page.get("snapshots") or [])),
              str([s.get("id") for s in (page.get("snapshots") or [])])[:170])

    # 7) "哪些房间在跑"这件事要么报全，要么如实说问不到——两种都是结论，半截话不是。
    # 探针跑的那台机器上没有 screen，所以这里走的是后者；前者由浏览器实测和 Go 用例钉住。
    code, page = backup_page()
    note = page.get("note") or ""
    if "问不到世界进程状态" in note:
        check("问不到进程状态时不编出被拦的房间，并且把原因写在页面上",
              not (page.get("restoreBlocked") or []), str(page.get("restoreBlocked"))[:120])
    else:
        code, cl = call("GET", "/api/v1/clusters")
        data = cl.get("data") or {}
        rows = data.get("clusters") if isinstance(data, dict) else data
        running = {c.get("name") for c in (rows or []) if any(s.get("running") for s in (c.get("shards") or []))}
        blocked = {b.get("cluster") for b in (page.get("restoreBlocked") or [])}
        check("每个在跑的房间都在 restoreBlocked 里（一份备份都没有也算）",
              running and not (running - blocked),
              "在跑=%s 已报=%s 漏=%s" % (sorted(running), sorted(blocked), sorted(running - blocked)))



TERRAIN_LABELS = {"不可通行", "道路", "岩石地", "泥地", "稀树草原", "草地", "森林", "沼泽",
                  "沙地", "洞穴", "蘑菇地", "人造地面", "蛛网", "近岸浅水", "海水",
                  "耕地", "洞穴壁", "其它地形"}


def map_checks():
    """世界地图：助手没回话 / 第一次导出 / 重新导出 / 换世界，四条路都在真进程上走一遍。

    这几条分支单测都过了，但界面上真正会撞车的是"时间戳"：面板判 pending 比的是
    底图文件的 mtime 和我们排队的那一刻谁新谁旧，这种判断只有真文件 + 真进程能看出来。
    """
    base = os.path.join(KLEI, "Cluster_1", "Master", "save")
    map_path = os.path.join(base, "dstpanel_map.json")
    status_path = os.path.join(base, "dstpanel_status.json")
    queue_path = os.path.join(base, "dstpanel_queue.txt")
    path = "/api/v1/clusters/Cluster_1/shards/Master/map"

    def touch(p, age):
        t = time.time() - age
        os.utime(p, (t, t))

    def write_status(age=0.0, game_id="MAPGAME"):
        # 坐标 0,0 在 32x32 的世界里应当落在 8x8 网格的正中（第 4 格），下面拿它验换算公式。
        raw = ('KLEI     1 {"shard":"Master","game_id":"%s","seq":7,"phase":"day","season":"autumn",'
               '"players":[{"name":"小王","prefab":"wilson","userid":"KU_1","x":0,"z":0,"is_dead":false}]}'
               % game_id)
        with io.open(status_path, "wb") as fh:
            fh.write(raw.encode("utf-8"))
        touch(status_path, age)

    def write_map(age=0.0, game_id="MAPGAME"):
        # 40 格森林 + 18 草地 + 5 海水 + 一个引擎自定义编号（200 FAKE_GROUND）：
        # 200 必须归进"其它地形"，不能被当成海或者把原始编号吐给界面。
        # 编号取自引擎 constants.lua 的 GROUND 表（实测 OCEAN_START=201）：
        # 早前这张表只认到 40，一整片海被划成"其它地形"，界面上就是"海为什么是红的"。
        tiles = [7] * 40 + [6] * 18 + [204] * 5 + [200]
        # 一格两种资源各一条（带子类）+ 一个伙伴随从；第一条故意还是旧版四元组，
        # 云机上要等分片重启才会换成新版助手，混着写必须照常读。
        pts = [[1, 1, 1, 2], [2, 2, 2, 5, 3], [2, 2, 2, 12, 5], [4, 4, 2, 3], [8, 8, 3, 1], [3, 3, 4, 1]]
        raw = json.dumps({"shard": "Master", "at": int(time.time()), "game_id": game_id,
                          "width": 32, "height": 32, "grid": 8, "tiles": tiles, "points": pts,
                          "totals": {"build": 2, "res": 20, "danger": 1, "companion": 1}})
        with io.open(map_path, "wb") as fh:
            fh.write(b"KLEI     1 " + raw.encode("utf-8"))
        touch(map_path, age)

    def drop(p):
        if os.path.isfile(p):
            os.remove(p)

    def map_lines():
        if not os.path.isfile(queue_path):
            return 0
        with io.open(queue_path, encoding="utf-8", errors="replace") as fh:
            return sum(1 for line in fh if line.strip().split("|")[1:2] == ["map"])

    drop(map_path)
    # 队列文件在这里再删一次，不等 main() 开头那轮清理：探针和 ui_dev 的面板共用同一棵
    # klei 夹具树，浏览器只要开着地图页就会几秒一轮地往同一个队列里塞 map 命令。
    # 上一轮清理到这一步之间隔着几十秒，够它塞一条进来——于是"助手没回话时不该入队"
    # 被顶红，报的还是别的世界里发生的写。谁要断言队列内容，谁自己先把队列清干净。
    for shard in ("Master", "Caves"):
        for name in ("dstpanel_queue.txt", "dstpanel_queue.txt.bak",
                     "dstpanel_processed.txt", "dstpanel_result.json"):
            q = os.path.join(KLEI, "Cluster_1", shard, "save", name)
            if os.path.isfile(q):
                os.remove(q)
    write_status(age=90)

    # 1) 助手掉线且从没导过图：只能如实说"要等分片进世界"，不能吐 Go 的报错。
    code, r = call("GET", path)
    msg = msg_of(r)
    check("助手没回话时地图给 503 + 中文说明", code == 503 and "回话" in msg
          and "no such file" not in msg and "open " not in msg, "%s %s" % (code, msg[:70]))
    check("助手没回话时不往队列里塞导出命令", map_lines() == 0, "队列里 map 命令 %d 条" % map_lines())

    # 2) 助手上线、第一次要底图：先 202 排队，界面靠轮询等文件。
    write_status()
    code, r = call("GET", path)
    v = r.get("data") or {}
    check("第一次打开地图先排队再等文件", code == 202 and v.get("pending") is True
          and map_lines() == 1, "%s pending=%s 队列=%d" % (code, v.get("pending"), map_lines()))

    # 3) 排队没落地之前重复点「重新导出」不能堆命令：助手要跑两遍两万格采样。
    code, r = call("GET", path + "?refresh=1")
    check("重复强制刷新不叠加队列", code == 202 and map_lines() == 1
          and (r.get("data") or {}).get("pending") is True, "队列 %d 条" % map_lines())

    # 4) 助手把文件写回来：分组、点位、玩家换算一次验完。
    time.sleep(1.2)
    write_map()
    code, r = call("GET", path)
    v = r.get("data") or {}
    check("底图落地后 ready 并且不再报进行中", code == 200 and v.get("ready") is True
          and v.get("pending") is False, "%s ready=%s pending=%s" % (code, v.get("ready"), v.get("pending")))
    labels = [t.get("label") for t in (v.get("terrain") or [])]
    check("地形图例只有中文分组名，Klei 编号不外泄",
          labels and set(labels) <= TERRAIN_LABELS and "其它地形" in labels
          and max(v.get("tiles") or [0]) < len(TERRAIN_LABELS), str(labels))
    # 海必须自成一组：它落进"其它地形"就是那句"海为什么是红的"的根因。
    check("海面编号归进海水组而不是其它地形", "海水" in labels, str(labels))
    check("格子序号跟着图例重排（长度 = 网格平方）", len(v.get("tiles") or []) == 64, "tiles=%d" % len(v.get("tiles") or []))
    kinds = [(p.get("kind"), p.get("kindLabel")) for p in (v.get("points") or [])]
    check("点位只给中文分类，不吐 1/2/3 这种码值",
          len(kinds) == 6 and all(k and lab != k and lab in ("建筑", "资源", "危险", "伙伴随从") for k, lab in kinds),
          str(kinds))
    subs = [(p.get("sub"), p.get("subLabel")) for p in (v.get("points") or []) if p.get("kind") == "res"]
    check("资源点带中文子类名；旧版四元组没子类也不报错",
          sorted([lab for _, lab in subs if lab]) == ["浆果丛", "草"]
          and sum(1 for _, lab in subs if not lab) == 1, str(subs))
    cp = [p for p in (v.get("points") or []) if p.get("kind") == "companion"]
    check("伙伴随从（切斯特那一类）单独成类", len(cp) == 1 and cp[0].get("kindLabel") == "伙伴随从", str(cp))
    check("汇总数字用助手给的全量",
          (v.get("totals") or {}) == {"build": 2, "res": 20, "danger": 1, "companion": 1},
          str(v.get("totals")))
    pl = (v.get("players") or [{}])[0]
    check("玩家世界坐标换算成网格坐标（和助手采样同一个公式）",
          abs(pl.get("x", -1) - 4.0) < 0.001 and abs(pl.get("y", -1) - 4.0) < 0.001
          and pl.get("worldX") == 0, str(pl))

    def write_map_ents():
        # 新版助手的格式：逐个实体一条，坐标是引擎 tile（32×32 世界、8×8 网格 → 一格 4 tile）。
        # 上面那份 points 是旧版助手在云机上还没重启时读的样子，两份都要能出图。
        tiles = [7] * 40 + [6] * 18 + [204] * 5 + [200]
        ents = [[4, 4, 2, 5], [5, 5, 2, 5], [4, 4, 2, 1], [31, 31, 3, 0], [0, 0, 4, 0]]
        raw = json.dumps({"shard": "Master", "at": int(time.time()), "game_id": "MAPGAME",
                          "width": 32, "height": 32, "grid": 8, "tiles": tiles, "ents": ents,
                          "totals": {"build": 0, "res": 3, "danger": 1, "companion": 1}})
        with io.open(map_path, "wb") as fh:
            fh.write(b"KLEI     1 " + raw.encode("utf-8"))

    # 4b) 个体点位：原样传到界面 + 编号字典跟着走 + 聚合视图照旧给得出。
    write_map_ents()
    code, r = call("GET", path)
    v = r.get("data") or {}
    check("个体点位原样到界面", code == 200 and len(v.get("ents") or []) == 5, str(v.get("ents"))[:90])
    check("个体点位带编号字典，界面不用自己猜码值",
          any(k.get("code") == 4 and k.get("key") == "companion" for k in (v.get("kinds") or []))
          and any(s.get("code") == 5 and s.get("label") == "浆果丛" for s in (v.get("subs") or [])),
          "kinds=%s subs=%s" % (v.get("kinds"), v.get("subs")))
    check("个体点位能推导出聚合视图（低倍率那一层不能空）",
          len(v.get("points") or []) == 4
          and max((p.get("count") or 0) for p in (v.get("points") or [])) == 2, str(v.get("points"))[:140])
    check("个体点位下汇总仍用助手给的全量",
          (v.get("totals") or {}).get("res") == 3, str(v.get("totals")))

    # 5) 用户点「重新导出」：这次排队之后，不带 refresh 的轮询也必须继续报进行中。
    #    早先这里只看"文件够不够新"，底图才导出两分钟时轮询第一次就收到 pending:false，
    #    前端提前收工，等新图真写回来时已经没人再问了。
    time.sleep(9)
    code, r = call("GET", path + "?refresh=1")
    v = r.get("data") or {}
    check("强制刷新先把旧图给出来再说进行中", code == 200 and v.get("ready") is True
          and v.get("pending") is True, "%s pending=%s" % (code, v.get("pending")))
    code, r = call("GET", path)
    check("没带 refresh 的轮询也认这次没落地的导出", (r.get("data") or {}).get("pending") is True,
          msg_of(r)[:60])

    # 6) 新文件写回来：进行中必须收掉，否则界面永远挂着假的"正在导出"。
    time.sleep(1.2)
    write_map()
    code, r = call("GET", path)
    check("新底图落地后不再报进行中", (r.get("data") or {}).get("pending") is False
          and code == 200, str((r.get("data") or {}).get("pending")))

    # 7) 换了世界（game_id 对不上）：旧底图还能看，但必须算过期并重导。
    #    状态快照每十几秒就算掉线，前面那几次 sleep 之后得先把它续上，
    #    否则这一步测到的是"助手没回话"而不是"世界变了"。
    write_status()
    time.sleep(9)
    write_map(game_id="OTHERWORLD")
    code, r = call("GET", path)
    v = r.get("data") or {}
    check("换了世界后旧底图触发重导", code in (200, 202) and v.get("pending") is True,
          "%s pending=%s %s" % (code, v.get("pending"), msg_of(r)[:60]))

    # 8) 洞穴分片从没导出过、也没状态：要的是"先去开服"这条路，不是空图。
    code, r = call("GET", "/api/v1/clusters/Cluster_1/shards/Caves/map")
    msg = msg_of(r)
    check("没有底图也没有状态的洞穴给 503 中文说明", code == 503 and ("回话" in msg or "世界" in msg),
          "%s %s" % (code, msg[:70]))
    drop(map_path)


def players_checks():
    """玩家管理：三份名单、在线区，以及"读不到人"的两种说法，在真进程上过一遍。

    这一页在云机上崩过整页：一个列表字段在 Go 里是 nil 切片，进 JSON 成了 null，
    前端模板直接读它的 .length，Vue 渲染整个断掉。当时单测全绿——因为没人从
    HTTP 响应这一头看形状。所以"整个响应里不许有 null"要在探针里也钉一份，
    单测那份（TestNoNullInAnyGetReply）管的是同一件事的另一半。

    本机 PATH 里没有 screen，所以"进程活着但被引擎暂停"那一支走不到
    （问不到进程就只能按没在跑算）；那一支由 internal/api 的单测加
    .tools/tmp/pause_fixture.py 在浏览器里验。这里能控的是"有没有世界在回话"，
    所以两种说法各钉一条。
    """
    path = "/api/v1/clusters/Cluster_1/players"
    snap = os.path.join(KLEI, "Cluster_1", "Master", "save", "dstpanel_status.json")
    caves = os.path.join(KLEI, "Cluster_1", "Caves", "save", "dstpanel_status.json")
    queue_path = os.path.join(KLEI, "Cluster_1", "Master", "save", "dstpanel_queue.txt")

    def fresh_status(season_days=True):
        # 面板判新鲜只看文件 mtime，所以"让 Master 回话"只要把这份文件的时间推到当下。
        # world 这一块照助手真实写出的形状来：引擎那边没有 day，只有 cycles（过完了几个整天），
        # 这一季过完几天/还剩几天是两个独立的读数（季节长度不由面板自己算）。
        days = '"elapsed_days_in_season":5,"remaining_days_in_season":14,' if season_days else ""
        raw = ('{"shard":"Master","seq":9,'
               '"world":{"cycles":20,"season":"autumn","phase":"day",' + days +
               '"temperature":18.6,"wetness":0},'
               '"players":[{"name":"小王","prefab":"wilson","userid":"KU_PROBE1","x":0,"z":0}]}')
        # 先自己把这份夹具验一遍再写盘。上一轮我在这里多打了一个引号，面板老实报
        # "不是合法 JSON"，看着像产品坏了——夹具坏了也得当场说，不能让它冒充成 bug。
        json.loads(raw)
        with io.open(snap, "wb") as fh:
            fh.write(b"KLEI     1 " + raw.encode("utf-8"))

    def silence():
        for p in (snap, caves):
            if os.path.isfile(p):
                os.remove(p)

    silence()
    code, r = call("GET", path)
    body = json.dumps(r, ensure_ascii=False)
    d = r.get("data") or {}
    check("玩家管理页 200", code == 200, "%s %s" % (code, msg_of(r)[:80]))
    check("玩家管理的响应里一个 null 都没有（整页崩溃的根因）", ": null" not in body, body[:160])
    groups = {g.get("key"): g for g in (d.get("lists") or [])}
    check("三份名单都在，且每份都带中文组名和各自的生效说明",
          set(groups) == {"admin", "whitelist", "blocklist"}
          and all((g.get("label") or "") and (g.get("effect") or "") for g in groups.values()),
          str([(g.get("key"), g.get("label")) for g in (d.get("lists") or [])])[:150])
    check("在线/名单/在跑/暂停四个列表字段都是数组",
          isinstance(d.get("online"), list) and isinstance(d.get("roster"), list)
          and isinstance(d.get("runningShards"), list) and isinstance(d.get("pausedShards"), list),
          str(d)[:150])
    note = d.get("note") or ""
    check("没有世界回话时说的是「没有世界在回话」，不谎称暂停",
          "没有世界在回话" in note and "暂停" not in note, note[:120])
    check("玩家管理这一页没有 Go 的英文报错串",
          "no such file" not in body and "open Cluster" not in body, "")

    # 离线拉黑在这一步必然做不成（一个世界都没在回话），要的是那句对的下一步。
    code, r = call("POST", "/api/v1/clusters/Cluster_1/players/KU_PROBE1/ban", {"confirm": True})
    msg = msg_of(r)
    check("没有世界回话时离线拉黑 503 并让人先把世界开起来",
          code == 503 and "先把世界开起来" in msg and "暂停" not in msg,
          "%s %s" % (code, msg[:110]))

    # 换一支：世界正在回话时，同一句话就成了错的建议——用户会去重启一个好好的服务器。
    # 本机没有引擎，所以命令写进队列后永远等不到回执：面板能给的只有两个诚实答案，
    # 503（判定助手掉了）或 202（已入队、没回执，它恢复后可能仍会执行）。
    # 两者都不许再让用户去开世界，也都必须仍然把那条命令真的留在队列文件里——
    # 界面上说"已提交"而磁盘上没有这条，就是用户看不到也撤不掉的幽灵操作。
    fresh_status()
    code, r = call("GET", path)
    d = r.get("data") or {}
    check("有世界回话时在线区不再报「没有世界在回话」",
          (d.get("runningShards") or []) == ["Master"] and "没有世界在回话" not in (d.get("note") or ""),
          str(d.get("runningShards"))[:60])
    # 天数走的是另一条链：助手只报 cycles（引擎没有 day 这个字段），面板换算成第几天。
    # 这一条从 HTTP 响应那头看这个换算，是因为界面上那个"第 0 天"曾经在每个房间上都成立，
    # 而单测当时全绿—— fixtures 里塞的是一个协议里根本不存在的键。
    # 走 /status 而不是 /rooms：探针这台机器 PATH 里没有 screen，总览那一页会先 503，
    # 而换算发生在同一份 statusView 上，总览和总览页读的是它。
    code, r = call("GET", "/api/v1/clusters/Cluster_1/shards/Master/status")
    sv = r.get("data") or {}
    check("cycles=20 在接口上报成第 21 天（不是第 20 天也不是第 0 天）",
          code == 200 and sv.get("day") == 21 and sv.get("cycles") == 20,
          "%s %s" % (code, str({k: sv.get(k) for k in ("day", "cycles")})[:120]))
    check("换算出来的天数配得上中文说法（不是码值）",
          sv.get("seasonLabel") == "秋天" and sv.get("phaseLabel") == "白天",
          str({k: sv.get(k) for k in ("season", "seasonLabel", "phase", "phaseLabel")})[:150])
    # 这一季的第几天：过完 5 天 → 第 6 天，还剩 14 天（6+14=20，正好一个秋天）。
    # 这两个数是引擎自己维护的，面板只做 +1，绝不自己凑季节长度——Klei 那边春、秋的
    # totaldaysinseason 被人为翻倍，自己算就会在每个春秋季上错出一段。
    check("季节里第几天与还剩几天照引擎读数报出来",
          sv.get("dayInSeason") == 6 and sv.get("daysLeftInSeason") == 14,
          str({k: sv.get(k) for k in ("dayInSeason", "daysLeftInSeason")})[:120])
    # 反过来也要钉住：老版助手（或季节根本不轮换的世界）没给这两个读数时，
    # 接口里不能出现"第 1 天"这种看着像真事的默认值。
    fresh_status(season_days=False)
    code, r = call("GET", "/api/v1/clusters/Cluster_1/shards/Master/status")
    sv = r.get("data") or {}
    check("助手没报季节读数时这两个键整体不出现（不冒充第 1 天）",
          code == 200 and "dayInSeason" not in sv and "daysLeftInSeason" not in sv
          and sv.get("day") == 21,
          str({k: sv.get(k) for k in ("day", "dayInSeason", "daysLeftInSeason")})[:150])
    fresh_status()
    code, r = call("POST", "/api/v1/clusters/Cluster_1/players/KU_PROBE1/ban", {"confirm": True})
    msg = msg_of(r)
    check("世界在回话时拉黑不再让人去开世界（只说助手或只说没回执）",
          code in (202, 503) and "先把世界开起来" not in msg, "%s %s" % (code, msg[:110]))
    with io.open(queue_path, encoding="utf-8", errors="replace") as fh:
        queued = [ln.strip() for ln in fh if ln.strip().split("|")[1:2] == ["ban"]]
    check("这条拉黑真的写进了队列文件（界面说的和发出去的一致）",
          len(queued) == 1 and queued[0].split("|")[2] == "KU_PROBE1", str(queued)[:120])
    silence()


def lobby_checks():
    """公网房间这一页在探针里只钉"口径"，不钉数量。

    它读的是 Klei 放在 CDN 上的公开快照：此刻有几间、这台机器出不出得去，都不是面板能决定的
    （本机对 api/lobby 域名的链路一直在抖）。所以这里三条恒真的断言：
    大区表必须全中文、一次都没成功过又刷不动时必须 503 并给一句中文（绝不降级成空列表）、
    参数填错必须当场拒。数量对不对、筛选生效不生效，由 .tools/ui_lobby_check.py 在浏览器里
    逐个控件对着接口比，那才是这一页的主检查。
    """
    code, r = call("GET", "/api/v1/lobby/regions")
    d = r.get("data") or {}
    check("大区表 200", code == 200, "%s %s" % (code, msg_of(r)[:80]))
    labels = [x.get("label") for x in (d.get("regions") or [])]
    check("大区只有三个且都是中文名（面板只读 Klei 公开桶的那几个）",
          len(labels) == 3 and all(re.search(r"[一-鿿]", u or "") for u in labels), str(labels))
    check("默认落在亚太（用面板的人在国内）", d.get("defaultRegion") == "ap-southeast-1",
          str(d.get("defaultRegion")))
    check("顶部那句来历说明说了不要凭据", "凭据" in (d.get("note") or ""), (d.get("note") or "")[:80])

    code, r = call("GET", "/api/v1/lobby/rooms")
    d = r.get("data") or {}
    if code == 200:
        check("公网房间列表的响应里一个 null 都没有",
              ": null" not in json.dumps(r, ensure_ascii=False), json.dumps(r, ensure_ascii=False)[:120])
        # 原始条数和能用房间数必须分开说：只报后者的话，Klei 改字段形状那天
        # 界面会安静地少一半房间而没人知道。
        check("原始条数 >= 能用房间数（两个数分开说）",
              d.get("sourceRecords", 0) >= d.get("usableRooms", 0),
              "%s 条 / %s 间" % (d.get("sourceRecords"), d.get("usableRooms")))
        sorts = {s.get("value") for s in (d.get("sorts") or []) if s.get("value")}
        check("没带 sort 时接口自己选定了一枚下拉里真有的排序（空值会让界面画成空白框）",
              d.get("query", {}).get("sort") in sorts, str(d.get("query"))[:90])
        for room in (d.get("rooms") or [])[:3]:
            check("每一行给得出地址端口和一句中文门槛",
                  bool(room.get("addr")) and bool(room.get("port")) and bool(room.get("accessLabel")),
                  str(room)[:140])
        # 列表每一行要画的那几列（views/lobby.js 的行模板直读这些字段）。
        # 那条链路是纯前端的，页面自己不做类型校验：哪天后端把 capacity 改成字符串或改了名，
        # 界面上那一格会安静地空掉，而表格看着照样是齐的。
        # 「照这间开服」曾经在 2026-11-04 拆掉前也读这五个字段，字段本身留不留只看这一页画不画。
        row_keys = ("intent", "pvp", "capacity", "password", "allowNew")
        miss = [k for k in row_keys if any(k not in room for room in (d.get("rooms") or []))]
        check("列表要读的五个字段每一行都在", not miss, "缺 %s" % miss)
        check("人数上限是数字（那一行要拿它算还能进几个人）",
              all(isinstance(room.get("capacity"), int) for room in (d.get("rooms") or [])),
              str([room.get("capacity") for room in (d.get("rooms") or [])[:5]]))
        check("玩法给的是原始取值不是中文（中文那份在 intentLabel）",
              all(isinstance(room.get("intent"), str) for room in (d.get("rooms") or [])),
              str([room.get("intent") for room in (d.get("rooms") or [])[:5]]))
        check("连接说明指得对（游戏没有按 IP 加入的输入框，也不指页面里不存在的行）",
              "控制台" in (d.get("joinNote") or "")
              and "上面那句" not in (d.get("joinNote") or ""), (d.get("joinNote") or "")[:100])
    else:
        # 出不去公网不是面板的错，这一支只要求那句是中文、说的是连不上。
        check("读不到公网列表时给的是 503 加一句中文（不降级成空列表）",
              code == 503 and "Klei" in msg_of(r), "%s %s" % (code, msg_of(r)[:110]))

    for path, want in (("/api/v1/lobby/rooms?region=eu-central-9", "没有这个大区"),
                       ("/api/v1/lobby/rooms?sort=bogus", "sort 只能填")):
        code, r = call("GET", path)
        check("公网房间参数填错当场拒：%s" % path,
              code == 400 and want in msg_of(r), "%s %s" % (code, msg_of(r)[:110]))


def call_as(method, path, body=None, user="admin", pw="boot-check-pw"):
    """多账号那一组要用别的身份发请求，所以不能共用全局 AUTH。"""
    auth = "Basic " + base64.b64encode(("%s:%s" % (user, pw)).encode("utf-8")).decode()
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=data, method=method,
                                 headers={"Authorization": auth, "Content-Type": "application/json"})
    return _do(req, path)


def account_checks():
    # 出厂默认凭据是 admin/admin，而面板可能直接开在公网端口上。这一组守的是两件事：
    # 1) 密码规则不能比默认值严（下限 5），否则照文档部署的人会被接口拦住；
    # 2) "还在用默认密码"这个状态必须从接口读得到——明文没入库，界面自己算不出来。
    code, r = call_as("GET", "/api/v1/session")
    d = r.get("data") or {}
    check("session 报身份与中文角色", code == 200 and d.get("username") == "admin"
          and d.get("roleLabel") == "管理员" and d.get("isAdmin") is True, str(d)[:120])
    check("探针自己的密码不是默认值", d.get("usingDefaultPassword") is False, str(d.get("usingDefaultPassword")))

    code, r = call_as("POST", "/api/v1/accounts",
                      {"username": "probe-viewer", "password": "abcde", "role": "viewer"})
    d = r.get("data") or {}
    check("5 位密码能建档（规则不比出厂默认值严）", code == 201 and d.get("roleLabel") == "只读",
          "%s %s" % (code, str(r.get("error"))[:90]))
    code, r = call_as("POST", "/api/v1/accounts",
                      {"username": "probe-4char", "password": "abcd", "role": "viewer"})
    check("4 位密码 400 且是中文规则", code == 400 and "5" in msg_of(r) and "至少" in msg_of(r), msg_of(r)[:90])

    code, r = call_as("POST", "/api/v1/accounts",
                      {"username": "probe-default", "password": DEFAULT_PW, "role": "viewer"})
    d = r.get("data") or {}
    check("用出厂默认密码建档成功并标出默认状态",
          code == 201 and d.get("usingDefaultPassword") is True, "%s %s" % (code, str(d)[:120]))
    default_id = d.get("id")

    code, r = call_as("GET", "/api/v1/session", user="probe-default", pw=DEFAULT_PW)
    check("默认密码账号的 session 也带出该状态",
          code == 200 and (r.get("data") or {}).get("usingDefaultPassword") is True, str(r.get("data"))[:120])

    code, r = call_as("GET", "/api/v1/accounts")
    rows = {a["username"]: a for a in (r.get("data") or [])}
    check("列表里逐行标出谁还在用默认密码", code == 200
          and rows.get("probe-default", {}).get("usingDefaultPassword") is True
          and rows.get("probe-viewer", {}).get("usingDefaultPassword") is False
          and rows.get("admin", {}).get("usingDefaultPassword") is False,
          str({k: v.get("usingDefaultPassword") for k, v in rows.items()})[:160])
    body = json.dumps(r, ensure_ascii=False)
    check("账号响应里没有密码字段或哈希", "password" not in body and "pbkdf2" not in body, body[:120])
    check("列表里当前账号被标成自己", rows.get("admin", {}).get("self") is True, str(rows.get("admin"))[:120])

    code, r = call_as("GET", "/api/v1/accounts", user="probe-viewer", pw="abcde")
    check("只读账号看账号列表 403 并给中文说明", code == 403 and "管理员" in msg_of(r), msg_of(r)[:90])

    admin_id = rows.get("admin", {}).get("id")
    code, r = call_as("PUT", "/api/v1/accounts/%s/role" % admin_id, {"role": "viewer"},
                      user="probe-viewer", pw="abcde")
    check("只读账号改不了任何人的权限", code == 403, "%s %s" % (code, msg_of(r)[:80]))

    code, r = call_as("PUT", "/api/v1/session/password",
                      {"oldPassword": "no-such", "newPassword": "abcdefgh"}, user="probe-viewer", pw="abcde")
    check("改自己密码要旧密码对", code == 400 and "旧密码" in msg_of(r), msg_of(r)[:90])
    code, r = call_as("PUT", "/api/v1/session/password",
                      {"oldPassword": "abcde", "newPassword": "abcdefgh"}, user="probe-viewer", pw="abcde")
    check("只读账号可以改自己的密码", code == 200, "%s %s" % (code, msg_of(r)[:80]))
    code, r = call_as("GET", "/api/v1/session", user="probe-viewer", pw="abcde")
    check("改完旧密码立刻失效", code == 401, str(code))

    code, r = call_as("DELETE", "/api/v1/accounts/%s" % default_id, user="probe-viewer", pw="abcdefgh")
    check("只读账号删不掉账号", code == 403, "%s %s" % (code, msg_of(r)[:80]))
    code, r = call_as("DELETE", "/api/v1/accounts/%s" % default_id)
    check("默认密码账号能被删掉", code == 200, "%s %s" % (code, msg_of(r)[:80]))

    code, r = call("GET", "/api/v1/audit?action=account.&limit=50")
    acts = [e["action"] for e in (r.get("data") or [])]
    check("账号建、改密、删都留了审计",
          code == 200 and all(a in acts for a in ("account.create", "account.password", "account.delete")),
          str(acts)[:140])


def blocked_by_screen(r):
    """两张房间列表（/clusters、/rooms）在本机必然 503：面板宁可不报进程态。

    归属裁切正好挂在这两张表上，所以本机跑不到的那几条要明说"跳过了、为什么"，
    而不是拿一个空列表去比——那会把"接口没数据"报成"归属没裁对"。
    同一批断言在云机真机验收（有 screen）和 .tools/ui_owner_check.py（带 screen 替身）里都跑。
    """
    return "screen" in json.dumps(r, ensure_ascii=False)


def owner_checks():
    """房间归属（多租户）的接口契约：可见集、指派、以及三条"照做就会把自己锁死"的规则。

    这一组只走接口、不碰浏览器，因为云机真机验收跑的就是这个脚本；
    界面那一版在 .tools/ui_owner_check.py，两边守的是同一批字段。
    """
    cl = "Cluster_1"          # 夹具里一直在的那间，用来做"派出去 / 收回来"
    unowned = "ProbeWorld"    # 开服向导那组建的房间，保持未指派
    code, r = call("GET", "/api/v1/session")
    d = r.get("data") or {}
    roles = d.get("roles") or []
    check("管理员可指派，角色字典三档且没有裸码值",
          code == 200 and d.get("canAssignOwner") is True
          and [x.get("value") for x in roles] == ["admin", "member", "viewer"]
          and all(x.get("label") and x["label"] != x["value"] for x in roles), str(roles)[:120])

    code, r = call("POST", "/api/v1/accounts",
                   {"username": "probe-owner", "password": "owner-pass-1", "role": "member"})
    mid = (r.get("data") or {}).get("id")
    check("建一个成员用于归属实测", code == 201 and mid, "%s %s" % (code, str(r.get("error"))[:80]))
    code, r = call("POST", "/api/v1/accounts",
                   {"username": "probe-owner-view", "password": "viewer-pass", "role": "viewer"})
    vid = (r.get("data") or {}).get("id")
    code, r = call("GET", "/api/v1/accounts")
    aid = {a["username"]: a["id"] for a in (r.get("data") or [])}.get("admin")

    code, r = call("PUT", "/api/v1/clusters/%s/owner" % cl, {"accountId": aid})
    check("派给管理员被拒：管理员负责所有房间", code == 409 and "管理员" in msg_of(r), msg_of(r)[:90])
    code, r = call("PUT", "/api/v1/clusters/%s/owner" % cl, {"accountId": vid})
    check("派给只读账号被拒并说出先改成什么", code == 409 and "房间成员" in msg_of(r), msg_of(r)[:90])
    code, r = call("PUT", "/api/v1/clusters/%s/owner" % cl, {"accountId": 999999})
    check("派给不存在的账号 404 而不是留下半个归属", code == 404, "%s %s" % (code, msg_of(r)[:70]))
    code, r = call("PUT", "/api/v1/clusters/Nope_99/owner", {"accountId": mid})
    check("派给磁盘上没有的房间被拒", code in (400, 404), "%s %s" % (code, msg_of(r)[:70]))

    code, r = call("PUT", "/api/v1/clusters/%s/owner" % cl, {"accountId": mid})
    d = r.get("data") or {}
    check("派给成员 200，回答里带中文说法与指派注记",
          code == 200 and "probe-owner" in (d.get("ownerLabel") or "") and d.get("assignedNote"),
          str(d)[:150])
    # 指派注记里不许夹钟点：时刻是前端按浏览器时区格式化的（见 store.js assignedText），
    # 后端把 UTC 拼进这句话，同一屏就会出现两个相差几小时的钟点。
    note, stamp = (d.get("assignedNote") or ""), (d.get("assignedAt") or "")
    check("指派注记不带钟点，时刻留给 assignedAt",
          bool(note) and not re.search(r"\d\d:\d\d", note) and stamp.endswith("Z"),
          "note=%r at=%r" % (note, stamp))
    try:
        parsed_at = datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ")
    except ValueError:
        parsed_at = None
    check("assignedAt 解得开（前端 dateTime 只吃 RFC3339）",
          parsed_at is not None and abs((datetime.now(timezone.utc) - parsed_at.replace(tzinfo=timezone.utc)).total_seconds()) < 600,
          stamp)
    code, r = call("GET", "/api/v1/clusters")
    if blocked_by_screen(r):
        print("[SKIP] 本机 PATH 里没有 screen：集群列表 503，逐行归属改由云机与 ui_owner_check 覆盖")
    else:
        row = {c["name"]: c for c in (r.get("data") or [])}.get(cl, {})
        check("集群列表逐行带归属", row.get("ownerAccountId") == mid
              and "probe-owner" in (row.get("ownerLabel") or "")
              and (row.get("assignedNote") or "").find(":") < 0 and row.get("assignedAt"),
              str(row)[:140])

    code, r = call_as("GET", "/api/v1/session", user="probe-owner", pw="owner-pass-1")
    d = r.get("data") or {}
    check("成员的身份回答里带自己名下的房间，且不能指派",
          code == 200 and cl in (d.get("myClusters") or []) and d.get("canAssignOwner") is False,
          str(d)[:140])
    code, r = call_as("PUT", "/api/v1/clusters/%s/owner" % cl, {"accountId": vid},
                   user="probe-owner", pw="owner-pass-1")
    check("成员把名下的房间转派出去被拒", code == 403 and "管理员" in msg_of(r), msg_of(r)[:90])
    code, r = call_as("POST", "/api/v1/clusters/%s/shards/Master/start" % cl, {},
                   user="probe-owner", pw="owner-pass-1")
    check("成员起自己名下的房间不被权限层拦（夹具里没有引擎，400 是另一回事）",
          code != 403, "%s %s" % (code, msg_of(r)[:80]))

    # 未指派的那间对成员是"看得见、改不动"，不是从列表里消失：
    # 藏起来的话，管理员永远看不出自己漏派了哪间。
    code, r = call_as("GET", "/api/v1/rooms", user="probe-owner", pw="owner-pass-1")
    if blocked_by_screen(r):
        print("[SKIP] 本机 PATH 里没有 screen：成员的房间名单裁切改由云机与 ui_owner_check 覆盖")
    else:
        items = (r.get("data") or {}).get("items") or []
        names = sorted({x["cluster"] for x in items})
        check("成员的房间总览含自己那间与未指派那间", cl in names and unowned in names, names)
        mine = [x for x in items if x["cluster"] == cl]
        check("每一行都带归属说法与 ownedByMe", bool(mine) and all(x.get("ownerLabel") for x in mine)
              and all(x.get("ownedByMe") for x in mine), str(mine[:1])[:150])
    code, r = call_as("POST", "/api/v1/clusters/%s/shards/Master/start" % unowned, {},
                      user="probe-owner", pw="owner-pass-1")
    check("成员动未指派那间被拒，且说得出该谁去指派",
          code == 403 and "指派" in msg_of(r), msg_of(r)[:90])

    code, r = call("PUT", "/api/v1/accounts/%s/role" % mid, {"role": "viewer"})
    check("名下有房间的成员不能直接改只读", code == 409 and "撤" in msg_of(r), msg_of(r)[:90])
    code, r = call("PUT", "/api/v1/clusters/%s/owner" % cl, {"accountId": 0})
    d = r.get("data") or {}
    check("取消指派回未指派", code == 200 and d.get("ownerLabel") == "未指派"
          and d.get("accountId") == 0, str(d)[:120])
    code, r = call("PUT", "/api/v1/accounts/%s/role" % mid, {"role": "viewer"})
    check("撤了指派之后改只读就收下了（409 只卡真有名下房间的那个）", code == 200, msg_of(r)[:80])
    call("PUT", "/api/v1/accounts/%s/role" % mid, {"role": "member"})

    code, r = call("GET", "/api/v1/audit?action=cluster.owner&limit=20")
    evs = r.get("data") or []
    check("指派与取消都留了审计，且动作有中文说法",
          code == 200 and len(evs) >= 2 and all(e.get("actionLabel") != e.get("action") for e in evs)
          and any(e.get("target") == cl for e in evs), str(evs[:1])[:150])
    code, r = call_as("GET", "/api/v1/audit?limit=30", user="probe-owner", pw="owner-pass-1")
    evs = r.get("data") or []
    check("成员读到的操作记录只有自己做的", code == 200 and bool(evs)
          and all(e.get("actor") == "probe-owner" for e in evs),
          str([(e.get("actor"), e.get("action")) for e in evs[:3]])[:140])
    code, r = call_as("GET", "/api/v1/audit?actor=admin&limit=10", user="probe-owner", pw="owner-pass-1")
    check("成员指名要看别人的操作被拒并说清该谁看",
          code == 403 and "管理员" in msg_of(r), msg_of(r)[:90])

    for uid in (mid, vid):
        call("DELETE", "/api/v1/accounts/%s" % uid)


def main():
    if not os.path.isfile(EXE):
        print("先跑 .tools/rebuild.sh")
        return 1
    # 任务队列这一组要真跑一个外部命令，所以每次都重编假 steamcmd：
    # 用上一次留下的 exe 等于拿旧行为验新代码。
    # 替身的源码在 .tools/fake_steamcmd（和 .tools/ui_dev.py 共用一份）：以前它躺在
    # .tmp_boot 里，清一次临时目录探针就编不出替身，报错还长得像"面板的任务链路坏了"。
    build = subprocess.run([GO, "build", "-o", FAKE_EXE, "./.tools/fake_steamcmd"],
                           cwd=ROOT,
                           env=dict(os.environ, GOTOOLCHAIN="local"),
                           capture_output=True)
    if build.returncode != 0:
        print("假 steamcmd 编译失败：" + build.stderr.decode("utf-8", "replace"))
        return 1
    # 清掉上一轮的助手通道文件与 override，让"启用"这一步是从零发生的。
    helper_dir = os.path.join(DST, "mods", "DSTPanelHelper")
    shutil.rmtree(helper_dir, ignore_errors=True)
    cluster = os.path.join(KLEI, "Cluster_1")
    for shard in ("Master", "Caves"):
        p = os.path.join(cluster, shard, "modoverrides.lua")
        if os.path.isfile(p):
            os.remove(p)
        # 通道早年放在 save/panel/ 子目录里，引擎的 SetPersistentString 不建多级目录，
        # 每次写都只得到 "[CRITICAL] Failed to save file"。旧版本留下的目录和文件
        # 在这里清干净，好让下面那条"面板不再写子目录通道"是真断言而不是比空气。
        shutil.rmtree(os.path.join(cluster, shard, "save", "panel"), ignore_errors=True)
        for name in ("dstpanel_queue.txt", "dstpanel_result.json", "dstpanel_processed.txt",
                     "dstpanel_map.json"):
            q = os.path.join(cluster, shard, "save", name)
            if os.path.isfile(q):
                os.remove(q)
    # 快照故意留一份"过期"的（mtime 推到 90 秒前）：面板判新鲜度看的是文件时间，
    # 这样"助手停更"那条诊断才真的走到，而不是只验"文件不存在"那一半。
    # 文件带上引擎给 SetPersistentString 加的 "KLEI     1 " 头，顺带把剥头那段也接进链路。
    stale_snap = os.path.join(cluster, "Master", "save", "dstpanel_status.json")
    os.makedirs(os.path.dirname(stale_snap), exist_ok=True)
    with open(stale_snap, "wb") as f:
        f.write(b'KLEI     1 {"shard":"Master","seq":1,"phase":"day","season":"autumn"}')
    old = time.time() - 90
    os.utime(stale_snap, (old, old))
    # 备份那一段要有真形状的存档可打：slot 目录是"恢复要整棵替换"的那一层，
    # 只放一个 save/world 文件的话，覆盖式和整棵替换在测试里长得一模一样。
    for shard, game in (("Master", "0000000000000001"), ("Caves", "0000000000000002")):
        for slot in ("slot-0001", "slot-0002"):
            d = os.path.join(cluster, shard, "save", "session", game, slot)
            os.makedirs(d, exist_ok=True)
            with open(os.path.join(d, "server.dat"), "wb") as fh:
                fh.write(("%s %s 世界内容\n" % (shard, slot)).encode("utf-8"))
            with open(os.path.join(d, "worldsnippet.bin"), "wb") as fh:
                fh.write(b"snippet\n")
        with open(os.path.join(cluster, shard, "save", "world"), "wb") as fh:
            fh.write(b"worldsave-header\n")
        # 云机实测的另一种真形状：这台引擎把自动存档写成 <8 位编号> 文件加 .meta，
        # 压根没有 slot 目录。两种都摆上，面板认的必须一直是"save 下的都算世界状态"
        # 这条规则本身，而不是我编的目录名。
        sess = os.path.join(cluster, shard, "save", "session", game)
        for num in ("0000000084", "0000000085"):
            with open(os.path.join(sess, num), "wb") as fh:
                fh.write(("local savedata = {} -- %s\n" % num).encode("utf-8"))
            with open(os.path.join(sess, num + ".meta"), "wb") as fh:
                fh.write(b"return {clock={cycles=74}}\n")
        # 引擎自己攒的旧日志副本：分片把上一轮的日志挪进 backup/server_log/ 存着。
        # 它们曾经被黑名单漏掉、打进快照（一份里混进 22 个日志文件）。
        for d, name in (("server_log", "server_log_2026-10-31-02-18-18.txt"),
                        ("server_chat_log", "server_chat_log_2026-10-31-02-18-19.txt")):
            rd = os.path.join(cluster, shard, "backup", d)
            os.makedirs(rd, exist_ok=True)
            with open(os.path.join(rd, name), "wb") as fh:
                fh.write(b"[00:00:00]: old rotated log line\n")
    # 令牌必须在跳过名单里：快照是可以下载的，带着它等于把凭据挂到网页上。
    with open(os.path.join(cluster, "cluster_token.txt"), "w", encoding="utf-8") as fh:
        fh.write("pds-g^KU_fakeid001^TOKEN-MUST-NOT-BE-BACKED-UP\n")

    for p in (os.path.join(WORK, "dstpanel.db"), os.path.join(WORK, "dstpanel.db-wal"),
              os.path.join(WORK, "dstpanel.db-shm")):
        if os.path.isfile(p):
            os.remove(p)
    # 模组这一片也从零开始：清单、上传目标目录、以及原子写留下的备份。
    # 快照目录每轮从零开始：保留上限那几条断言比的是"打完几份剩几份"，
    # 上一轮留下来的快照会把它们全部带歪（面板启动时会自己把这个目录建回来）。
    shutil.rmtree(os.path.join(WORK, "backups"), ignore_errors=True)
    setup_file = os.path.join(DST, "mods", "dedicated_server_mods_setup.lua")
    for suffix in ("", ".bak"):
        if os.path.isfile(setup_file + suffix):
            os.remove(setup_file + suffix)
    shutil.rmtree(os.path.join(DST, "mods", "ProbeMod"), ignore_errors=True)
    shutil.rmtree(os.path.join(DST, "mods", "ProbeDep"), ignore_errors=True)
    # 开服引导那一组每次都要从零建集群，否则第二轮就撞上"已存在"。
    shutil.rmtree(os.path.join(KLEI, "ProbeWorld"), ignore_errors=True)

    log = open(os.path.join(WORK, "panel_probe.log"), "w", encoding="utf-8")
    proc = subprocess.Popen([EXE, "-config", os.path.join(BOOT, "panel_boot.yaml")],
                            cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
    try:
        up = False
        for _ in range(60):
            try:
                with urllib.request.urlopen(BASE + "/healthz", timeout=2) as r:
                    up = r.status == 200
                    break
            except Exception:
                time.sleep(0.25)
        if not up:
            print("面板没起来，看 .tmp_boot/work/panel_probe.log")
            return 1

        # 探针这套 kleiRoot 故意指向 .tmp_boot/klei（引擎根本不会读那里），
        # 所以启动日志必须喊一句：实测服务端忽略 -savedir，这是 kleiRoot 指错时
        # 唯一会外露的地方，只在单测里验等于没验。
        boot_log = open(os.path.join(WORK, "panel_probe.log"), encoding="utf-8", errors="replace").read()
        check("启动日志对不像引擎目录的 kleiRoot 给提示",
              "-savedir" in boot_log and "DoNotStarveTogether" in boot_log,
              boot_log[:150].replace("\n", " | "))

        # 1) 启用之前：助手文件缺、没有 override 条目
        code, r = call("GET", "/api/v1/clusters/Cluster_1/shards/Master/helper")
        v = r.get("data", {})
        check("helper 初始状态报缺失", code == 200 and v.get("installed") is False
              and len(v.get("missing", [])) == 4 and v.get("configured") is False,
              "missing=%s files=%d" % (v.get("missing"), len(v.get("files", []))))
        check("helper 回给界面轮询范围", v.get("pollMinSeconds") == 1 and v.get("pollMaxSeconds") == 60)

        # 2) 点启用：文件落地 + override 写入
        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Master/helper",
                       {"enabled": True, "pollSeconds": 3, "playerDetail": True})
        v = r.get("data", {})
        landed = all(os.path.isfile(os.path.join(helper_dir, *rel.split("/")))
                     for rel in ("modinfo.lua", "modmain.lua", "scripts/panel_bridge.lua", "scripts/panel_json.lua"))
        ov = os.path.join(cluster, "Master", "modoverrides.lua")
        ov_text = open(ov, encoding="utf-8").read() if os.path.isfile(ov) else ""
        check("启用后 4 个文件真的落地", code == 200 and landed, v.get("note", ""))
        check("启用写了 override", "DSTPanelHelper" in ov_text and "enabled = true" in ov_text
              and "poll_interval = 3" in ov_text)
        check("启用后状态自洽", v.get("installed") is True and v.get("enabled") is True
              and v.get("pollSeconds") == 3 and v.get("online") is False)
        check("提示要让人重启分片", "重启" in (v.get("hint") or ""), v.get("hint", ""))

        # 3) 非法参数不能留下半个配置
        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Caves/helper", {"enabled": True, "pollSeconds": 99})
        check("越界轮询被拒 400", code == 400 and "秒之间" in json.dumps(r, ensure_ascii=False), str(r.get("error")))
        check("被拒的分片没有 override", not os.path.isfile(os.path.join(cluster, "Caves", "modoverrides.lua")))

        # 4) 命令白名单与队列
        code, r = call("GET", "/api/v1/commands")
        names = [c["name"] for c in r.get("data", [])]
        # 白名单会长（禁言、名单、地图导出都是后面加的），这里不比条数：
        # 要守的是"每条都有中文说法和参数提示"，以及玩家管理那几条没被删掉。
        check("命令列表带中文标签", code == 200
              and all(c.get("label") and c["label"] != c["name"] and c.get("argsHint")
                      for c in r.get("data", []))
              and {"announce", "kick", "mute", "unmute", "ban", "unban", "map"} <= set(names),
              str(names))
        code, r = call("POST", "/api/v1/clusters/Cluster_1/shards/Master/commands", {"command": "c_shutdown"})
        check("未收录命令 400 并列出可用", code == 400 and "announce" in json.dumps(r, ensure_ascii=False))
        code, r = call("POST", "/api/v1/clusters/Cluster_1/shards/Master/commands",
                       {"command": "announce", "args": ["服务器 10 分钟后维护"], "waitSeconds": 0})
        # 通道文件平铺在 save/ 下（dstpanel_queue.txt），不在 save/panel/ 里：
        # 引擎的 SetPersistentString 不会建子目录，写进子目录只会得到
        # "[CRITICAL] Failed to save file"。
        q = os.path.join(cluster, "Master", "save", "dstpanel_queue.txt")
        qtext = open(q, encoding="utf-8").read() if os.path.isfile(q) else ""
        d = r.get("data", {})
        check("公告入队且回显 pending", code == 200 and d.get("pending") == 1 and qtext.startswith(d.get("id", "") + "|announce|"),
              qtext.strip()[:60])
        check("没有留下旧的 save/panel/ 子目录通道",
              not os.path.isdir(os.path.join(cluster, "Master", "save", "panel")), "")

        # 5) 助手没心跳时等回执要给结论，不是 500 也不是英文串
        code, r = call("POST", "/api/v1/clusters/Cluster_1/shards/Master/commands",
                       {"command": "ping", "waitSeconds": 2})
        body = json.dumps(r, ensure_ascii=False)
        # 上面把状态种成了"90 秒前"，所以这里走的必须是"数据已停更"那一支；
        # 写成三种说法任一的那种类子断言，这一支坏了也照样绿。
        check("无心跳时 ping 给 503 中文诊断", code == 503 and "助手" in body
              and "数据已停更" in body and "file does not exist" not in body, body[:200])

        # 6) 设置项：改完要能读回来，并且记一条操作记录
        code, r = call("PUT", "/api/v1/settings", {"audit_retention_days": "45"})
        items = {x["key"]: x for x in r.get("data", [])}
        check("设置写入并回显", code == 200 and items.get("audit_retention_days", {}).get("value") == "45"
              and items.get("audit_retention_days", {}).get("set") is True)
        code, r = call("PUT", "/api/v1/settings", {"nonsense": "1"})
        check("未知设置项被拒", code == 400 and "nonsense" in json.dumps(r, ensure_ascii=False))
        code, r = call("GET", "/api/v1/audit?limit=50")
        acts = [e["action"] for e in r.get("data", [])]
        check("审计里有 helper/settings/bridge", code == 200
              and "helper.enable" in acts and "settings.update" in acts and "bridge.announce" in acts, str(acts))
        check("审计操作者不是空的", all(e.get("actor") for e in r.get("data", [])))

        # 7) 趋势接口：给的是"按时间窗归好的格子 + 这份数据实际覆盖到哪"。
        #    没有采样点的房间必须是空数组和一句人话，而不是 null、报错，或者上一房间的点。
        code, r = call("GET", "/api/v1/stats?cluster=NoSuchCluster&hours=24")
        d = r.get("data") or {}
        check("stats 没采样点的房间返回空点和说明",
              code == 200 and d.get("points") == [] and d.get("samples") == 0 and d.get("note"),
              str(r)[:160])
        code, r = call("GET", "/api/v1/stats?cluster=Cluster_1&hours=24")
        d = r.get("data") or {}
        pts = d.get("points") or []
        check("stats 每个格子都是相加后的总人数且带格子说明",
              code == 200 and isinstance(pts, list) and d.get("bucketLabel")
              and all(("online" in x and "at" in x) for x in pts), str(d)[:160])
        # 这条是趋势图的命门：窗口多长，数据就得覆盖多长，右端必须是最新一条采样；
        # 反过来，一条采样都没有时不许拿零值时刻凑出一句"最早 0001-01-01"。
        # 老实现取"前 2000 行"，双世界跑一天就超了，界面写着 24 小时、图停在 6 小时前。
        # 这一份夹具是新建的库，Cluster_1 还没有采样，所以按有没有数据分两判；
        # "覆盖满窗口"那一句在真数据上是 Go 侧 stats_test.go 和浏览器侧
        # ui_filters_check.py 的趋势段各自验的。
        has = bool(d.get("samples"))
        cover_ok = ((d.get("dataFrom") and d.get("dataTo") and d.get("latestAgoMinutes") is not None)
                    if has else (not d.get("dataFrom") and not d.get("dataTo")))
        check("stats 有采样就报覆盖范围、没采样就不许编",
              code == 200 and cover_ok and len(pts) <= 600,
              "%s 条采样，覆盖字段=%s/%s，上限内点数=%d" % (d.get("samples"), d.get("dataFrom"),
                                                    d.get("dataTo"), len(pts)))
        code, r = call("GET", "/api/v1/stats?cluster=Cluster_1&hours=168")
        d168 = r.get("data") or {}
        check("stats 长时间窗把格子放大而不是丢数据",
              code == 200 and d168.get("bucketSeconds", 0) > (d.get("bucketSeconds") or 0)
              and len(d168.get("points") or []) <= 600,
              str({k: d168.get(k) for k in ("bucketSeconds", "bucketLabel")})[:160])
        code, r = call("GET", "/api/v1/stats?hours=0")
        check("stats 缺 cluster 被拒", code == 400)

        # 8) 越界路径不能写进别处
        code, r = call("GET", "/api/v1/clusters/Cluster_9/shards/Master/helper")
        check("不存在的集群 404", code == 404 and "Cluster_1" in json.dumps(r, ensure_ascii=False))

        # 8b) 开服引导：新建集群 / 令牌 / 世界设定 / 建好之后的集群设置
        bootstrap_checks()
        cluster_settings_checks()
        ports_checks()

        # 9) 模组：清单登记 -> 待下载、手工上传 -> 本地模组、按分片启用
        code, r = call("GET", "/api/v1/mods")
        v = r.get("data", {})
        dirs = [x["dir"] for x in v.get("mods", [])]
        check("模组列表认出助手目录", code == 200 and "DSTPanelHelper" in dirs, str(dirs))
        # 助手那份 modinfo.lua 是从仓库里直接拷进来的真文件（一串裸赋值 + 长字符串 +
        # values = POLL_CHOICES 这种变量引用），云机上引擎就是读它。名字读不出来的话，
        # 面板上所有依赖名称的功能——检索缓存入库、模组列表、一致性说明——全是空的。
        helper_row = [x for x in v.get("mods", []) if x["dir"] == "DSTPanelHelper"]
        check("助手 modinfo.lua 的中文名与版本读得出来", helper_row
              and helper_row[0].get("modinfoOk") is True
              and helper_row[0].get("name") == "DST 管理面板助手"
              and helper_row[0].get("version") == "1.0.0",
              str(helper_row[0])[:200] if helper_row else "")
        check("清单路径回给界面", bool(v.get("setupPath")), v.get("setupPath", ""))

        code, r = call("POST", "/api/v1/mods/setup", {"workshopId": "1); os.execute('rm -rf /') //"})
        check("注入形态的工坊 id 被拒 400", code == 400 and "数字" in json.dumps(r, ensure_ascii=False), str(r.get("error")))
        setup_text = open(setup_file, encoding="utf-8").read() if os.path.isfile(setup_file) else ""
        check("被拒的 id 没进清单", "os.execute" not in setup_text, setup_text[:80])

        code, r = call("POST", "/api/v1/mods/setup", {"workshopId": "378545146"})
        v = r.get("data", {})
        setup_text = open(setup_file, encoding="utf-8").read() if os.path.isfile(setup_file) else ""
        check("订阅写进清单并标为待下载", code == 200 and 'ServerModSetup("378545146")' in setup_text
              and [p.get("workshopId") for p in v.get("pendingDownload") or []] == ["378545146"],
              str(v.get("pendingDownload")))
        check("提示说清要重启才拉取", "重启" in (v.get("hint") or ""), v.get("hint", ""))

        code, r = call("POST", "/api/v1/mods/setup", {"workshopId": "378545146"})
        v = r.get("data", {})
        check("重复订阅幂等且不重复写行", code == 200 and setup_text.count("378545146") == 1
              and "早就在订阅清单里" in (v.get("hint") or ""), v.get("hint", ""))

        # 手工上传：先试穿越包，再传正常包
        # modinfo 一律按引擎真正认的"一串裸赋值"写，见 internal/dst/lua.go 的 ParseLuaAssignments。
        bad = make_zip([("modinfo.lua", 'name = "坏包"\n'), ("../../outside_probe.lua", "bad")])
        code, r = call_upload("/api/v1/mods/upload", {"name": "ProbeBad"}, "bad.zip", bad)
        check("穿越包被拒 400", code == 400, str(r.get("error"))[:120])
        check("穿越条目没写到 mods/ 外面",
              not os.path.exists(os.path.join(DST, "outside_probe.lua"))
              and not os.path.exists(os.path.join(ROOT, "outside_probe.lua")))
        leftovers = [d for d in os.listdir(os.path.join(DST, "mods")) if d.startswith(".upload-")]
        check("被拒的上传没留临时目录", not leftovers, str(leftovers))

        good = make_zip([("modinfo.lua", 'name = "探针模组"\nversion = "1.0"\n'),
                         ("modmain.lua", "print('probe')")])
        code, r = call_upload("/api/v1/mods/upload", {"name": "ProbeMod"}, "ProbeMod.zip", good)
        v = r.get("data", {})
        landed = os.path.isfile(os.path.join(DST, "mods", "ProbeMod", "modinfo.lua"))
        check("正常包上传后文件落地", code == 200 and landed, str(r.get("error"))[:120])
        check("上传的模组进 localOnly", "ProbeMod" in (v.get("localOnly") or []), str(v.get("localOnly")))
        check("上传后提示还要按分片启用", "打开" in (v.get("hint") or ""), v.get("hint", ""))

        code, r = call_upload("/api/v1/mods/upload", {"name": "ProbeMod"}, "again.zip", good)
        check("同名模组拒绝覆盖", code == 400 and "已经存在" in json.dumps(r, ensure_ascii=False), str(r.get("error")))

        # 按分片启用：不能把第二步写好的助手条目挤掉
        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Master/mods/ProbeMod",
                       {"enabled": True, "priority": -100, "options": {"show_names": True, "radius": 3}})
        ov_text = open(ov, encoding="utf-8").read() if os.path.isfile(ov) else ""
        check("分片启用写了 ProbeMod 条目", code == 200 and "ProbeMod" in ov_text and "priority = -100" in ov_text,
              str(r.get("error"))[:120])
        check("通用覆盖没有挤掉助手条目", "DSTPanelHelper" in ov_text and "poll_interval = 3" in ov_text,
              ov_text.replace("\n", " ")[:200])
        v = r.get("data", {})
        pm = [x for x in v.get("mods", []) if x["dir"] == "ProbeMod"]
        check("启用后读回状态与配置项", pm and pm[0]["enabled"] is True and pm[0]["onDisk"] is True
              and pm[0]["priority"] == -100 and pm[0]["options"].get("show_names") is True, str(pm))

        # 只改一个配置项时另一个必须还在
        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Master/mods/ProbeMod",
                       {"options": {"radius": 5}})
        v = r.get("data", {})
        pm = [x for x in v.get("mods", []) if x["dir"] == "ProbeMod"]
        opts = pm[0]["options"] if pm else {}
        check("改单项不丢别的配置项", opts.get("show_names") is True and opts.get("radius") == 5, str(opts))
        ov2 = open(ov, encoding="utf-8").read()
        check("enabled 没被单项改动抹掉", "DSTPanelHelper" in ov2 and "enabled = true" in ov2)

        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Master/mods/ProbeMod",
                       {"options": {"nested": {"a": 1}}})
        body = json.dumps(r, ensure_ascii=False)
        check("嵌套配置项被拒 400", code == 400 and "文本、数字或是否" in body
              and "map[" not in body and "interface" not in body, str(r.get("error")))
        # 分片之间互不影响：Caves 有自己的 modoverrides.lua，Master 的那份不该被捎带改
        caves_ov = os.path.join(cluster, "Caves", "modoverrides.lua")
        master_before = open(ov, encoding="utf-8").read()
        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Caves/mods/ProbeMod", {"enabled": True})
        caves_text = open(caves_ov, encoding="utf-8").read() if os.path.isfile(caves_ov) else ""
        check("洞穴分片写自己的文件", code == 200 and "ProbeMod" in caves_text, caves_text.replace("\n", " ")[:120])
        check("改洞穴没动主分片", open(ov, encoding="utf-8").read() == master_before)
        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Master/mods/NoSuchMod", {"enabled": True})
        check("磁盘上没有的模组 404 并给下一步", code == 404 and "上传" in json.dumps(r, ensure_ascii=False), str(r.get("error")))

        # 9b) 跨分片一致性。引擎不比各分片的模组清单（分片间只同步世界设置，见日志里的
        # [SyncWorldSettings]），所以"地表开了洞穴没开"这种配置它照收，玩家穿门过去
        # 才会撞到规则不同的世界。这一组验的就是面板替它把这件事查出来。
        code, r = call("GET", "/api/v1/clusters/Cluster_1/mods/consistency")
        v = r.get("data", {})
        issues = v.get("issues") or []
        kinds = dict((i["dir"], i["kind"]) for i in issues)
        check("一致性检查列出两个分片", code == 200 and len(v.get("shards") or []) == 2,
              json.dumps(v.get("shards"), ensure_ascii=False)[:160])
        check("助手只开在主分片被报成启用状态不一致",
              kinds.get("DSTPanelHelper") == "启用状态不一致", str(kinds))
        check("两边都开但配置项不同被单独报出来", kinds.get("ProbeMod") == "配置项不一致", str(kinds))
        pm_issue = [i for i in issues if i["dir"] == "ProbeMod"]
        detail = pm_issue[0]["detail"] if pm_issue else ""
        check("说清是哪个配置项、哪边没写", "radius" in detail and "没写" in detail, detail[:140])
        check("逐分片状态给界面用", pm_issue and pm_issue[0]["byShard"].get("Master") == "开着"
              and pm_issue[0]["byShard"].get("Caves") == "开着",
              json.dumps(pm_issue[0].get("byShard"), ensure_ascii=False) if pm_issue else "")
        check("不一致时给出面板能做的下一步", pm_issue and "模组页" in pm_issue[0]["fix"],
              pm_issue[0]["fix"] if pm_issue else "")

        # 把两边写齐之后问题要真的消失，而不是一直挂着
        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Caves/mods/ProbeMod",
                       {"options": {"show_names": True, "radius": 5}})
        check("补齐洞穴配置项", code == 200, str(r.get("error"))[:120])
        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Caves/helper",
                       {"enabled": True, "pollSeconds": 3, "playerDetail": True})
        check("洞穴也启用助手", code == 200, str(r.get("error"))[:120])
        code, r = call("GET", "/api/v1/clusters/Cluster_1/mods/consistency")
        v = r.get("data", {})
        check("两边写齐后跨分片问题清空", code == 200 and (v.get("issues") or []) == [],
              json.dumps(v.get("issues"), ensure_ascii=False)[:200])
        check("没问题时也要有一句人话", "一致" in (v.get("note") or ""), v.get("note", ""))

        # 9b2) 引擎自己的下载区（ugc_mods）。云机实测：服务端按订阅清单把工坊内容拉到
        #      <dstInstall>/ugc_mods/<房间>/<世界>/content/322330/<id>，mods/ 下面不会多出目录，
        #      而且每个世界各存一份。以前只扫 mods/ 时，日志里明明 "Loading mod" 的模组会被
        #      界面报成「文件还没下载」。夹具只给 Master 放了这一份，两种结论刚好都有样本。
        ugc = "workshop-900000800"
        code, r = call("GET", "/api/v1/mods")
        allv = r.get("data") or {}
        urow = [x for x in (allv.get("mods") or []) if x["dir"] == ugc]
        check("引擎下载区里那一份算已下载并读得出名字版本",
              code == 200 and bool(urow) and urow[0].get("downloadedByEngine") is True
              and urow[0].get("name") == "Global Positions 位置共享"
              and urow[0].get("version") == "1.7.6", str(urow)[:200])
        check("引擎下载区里那一份不挂进待下载",
              "900000800" not in [p.get("workshopId") for p in (allv.get("pendingDownload") or [])],
              str(allv.get("pendingDownload")))
        for shard in ("Master", "Caves"):
            code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/%s/mods/%s" % (shard, ugc),
                           {"enabled": True})
            row = [x for x in ((r.get("data") or {}).get("mods") or []) if x["dir"] == ugc]
            want = os.path.isdir(os.path.join(DST, "ugc_mods", "Cluster_1", shard,
                                              "content", "322330", "900000800"))
            check("%s 的文件在不在按引擎下载区里这一份自己判" % shard,
                  code == 200 and bool(row) and row[0].get("enabled") is True
                  and row[0].get("onDisk") is want,
                  "接口=%s 磁盘=%s" % (row[0].get("onDisk") if row else None, want))
        code, r = call("GET", "/api/v1/clusters/Cluster_1/mods/consistency")
        check("两个世界都开着它不算启用不一致",
              code == 200 and all(i["dir"] != ugc for i in ((r.get("data") or {}).get("issues") or [])),
              json.dumps((r.get("data") or {}).get("issues"), ensure_ascii=False)[:200])

        # 9c) 依赖。引擎找不到 mod_dependencies 时是 DisableBecauseBad——静默禁用，
        # 服务端不报错也不拒绝启动，用户只会看到"模组开了没效果"。
        depmodinfo = ('name = "要库的探针"\nversion = "1.0"\napi_version = 10\n'
                      'all_clients_require_mod = true\n'
                      'description = [[\n探针说明，长字符串里的 = 和 , 号不算代码\n]]\n'
                      'local CHOICES = { {description = "一", data = 1} }\n'
                      'mod_dependencies = {\n    { workshop = "workshop-999999" },\n}\n'
                      'settings = { { name = "probe", label = "探针项", values = CHOICES, default = 1 } }\n')
        dep = make_zip([("modinfo.lua", depmodinfo), ("modmain.lua", "print('probe')")])
        code, r = call_upload("/api/v1/mods/upload", {"name": "ProbeDep"}, "ProbeDep.zip", dep)
        v = r.get("data", {})
        check("依赖在磁盘上就对不上", code == 200
              and any(u["dir"] == "ProbeDep" for u in (v.get("unmetDependencies") or [])),
              json.dumps(v.get("unmetDependencies"), ensure_ascii=False)[:200])
        row = [x for x in (v.get("mods") or []) if x["dir"] == "ProbeDep"]
        check("模组列表带出依赖候选", row and row[0].get("dependencies") == [["workshop-999999"]],
              str(row[0].get("dependencies")) if row else "")
        check("模组列表带出 all_clients_require_mod", row and row[0].get("allClientsRequire") is True,
              str(row[0]) if row else "")

        code, r = call("PUT", "/api/v1/clusters/Cluster_1/shards/Master/mods/ProbeDep", {"enabled": True})
        d = r.get("data", {})
        probs = d.get("dependencyProblems") or []
        check("启用后分片页点名缺的那组依赖",
              len(probs) == 1 and probs[0]["dir"] == "ProbeDep"
              and probs[0]["candidates"] == ["workshop-999999"] and probs[0]["onDisk"] is False,
              json.dumps(probs, ensure_ascii=False)[:220])
        check("后果要说成静默禁用", probs and "静默禁用" in probs[0]["hint"]
              and "找不到" in probs[0]["status"],
              json.dumps(probs[0], ensure_ascii=False)[:200] if probs else "")
        check("分片页总提示说清不报错", "静默禁用" in (d.get("hint") or ""), d.get("hint", ""))
        # 没开这个模组的分片不该被捎带报同样的问题
        code, r = call("GET", "/api/v1/clusters/Cluster_1/shards/Caves/mods")
        d = r.get("data", {})
        check("没开它的分片不报它的依赖",
              all(p["dir"] != "ProbeDep" for p in (d.get("dependencyProblems") or [])),
              json.dumps(d.get("dependencyProblems"), ensure_ascii=False)[:160])

        # 清单被手工改坏时，界面要提前说出来（这种文件会让服务端启动即退出）
        open(setup_file, "a", encoding="utf-8").write("local broken = \n")
        code, r = call("GET", "/api/v1/mods")
        v = r.get("data", {})
        check("看不懂的清单行被报出来", code == 200 and len(v.get("setupProblems") or []) == 1
              and "退出" in (v.get("hint") or ""), str(v.get("setupProblems")))

        code, r = call("DELETE", "/api/v1/mods/setup/378545146")
        setup_text = open(setup_file, encoding="utf-8").read()
        check("取消订阅只动那一行", code == 200 and "378545146" not in setup_text and "local broken" in setup_text,
              setup_text.replace("\n", " ")[:120])

        code, r = call("GET", "/api/v1/audit?limit=50")
        acts = [e["action"] for e in r.get("data", [])]
        check("模组操作全留审计", all(a in acts for a in
              ("mod.setup.add", "mod.setup.remove", "mod.upload", "mod.override")), str(acts[:12]))

        # 12) 工坊检索：这台 Windows 出不去 api.steampowered.com，所以这一组验的是降级路径
        # 和"见过即入库"的持久化。在线成功路径由 .tools/cloud_search_probe.py 在云机上验。
        wid = "398558175"
        wdir = os.path.join(DST, "mods", "workshop-" + wid)
        code, r = call("POST", "/api/v1/mods/setup", {"workshopId": wid})
        check("检索前能订阅工坊 id", code == 200, str(r.get("error"))[:80])

        code, r = call("GET", "/api/v1/mods/search?q=" + wid)
        v = r.get("data", {})
        check("没配 key 时不报错而是指出去配哪一项", code == 200 and v.get("onlineEnabled") is False
              and "webApiKey" in (v.get("note") or ""), (v.get("note") or "")[:120])
        check("没见过的 id 结果为空但给了下一步", v.get("items") == []
              and "工坊 ID" in (v.get("note") or ""), str(v.get("items"))[:80])

        # 造出"磁盘上真有这个模组"的前提：面板扫到就该记住它的中文名。
        os.makedirs(wdir, exist_ok=True)
        open(os.path.join(wdir, "modinfo.lua"), "w", encoding="utf-8").write(
            'name = "几何防御 Geometric Defense"\nversion = "1.4"\napi_version = 10\n')
        code, r = call("GET", search_url("", "几何"))
        v = r.get("data", {})
        items = v.get("items") or []
        check("按中文名能搜到已下载的工坊模组", code == 200 and len(items) == 1
              and items[0]["workshopId"] == wid and items[0]["downloaded"] is True
              and items[0]["registered"] is True, json.dumps(items, ensure_ascii=False)[:200])
        if items:
            check("状态说的是两个事实而不是一个词",
                  "已下载" in items[0]["statusLabel"] and "清单" in items[0]["statusLabel"],
                  items[0]["statusLabel"])
            check("来源标成这台机器上的文件", "这台机器" in items[0]["sourceLabel"], items[0]["sourceLabel"])
            check("版本从 modinfo 里带出来了", items[0].get("version") == "1.4", str(items[0].get("version")))

        # 关键一步：把文件删掉再搜。还能搜到就证明缓存真的写进了库，
        # 而不是每次都靠扫磁盘——这是"断网也能按名字找 ID"的全部依据。
        shutil.rmtree(wdir, ignore_errors=True)
        code, r = call("GET", search_url("", "几何"))
        v = r.get("data", {})
        items = v.get("items") or []
        check("删掉文件后仍能从缓存搜到（见过即入库）", code == 200 and len(items) == 1
              and items[0]["source"] == "cache" and items[0]["downloaded"] is False,
              json.dumps(items, ensure_ascii=False)[:200])
        if items:
            check("文件没了但清单还在时要改成等重启", "重启" in items[0]["statusLabel"], items[0]["statusLabel"])
        check("缓存条数进了响应", (v.get("cacheCount") or 0) >= 1, str(v.get("cacheCount")))

        code, r = call("GET", "/api/v1/mods/search?q=%25")
        v = r.get("data", {})
        check("搜索框里的通配符当字面量处理", code == 200 and v.get("items") == [],
              json.dumps(v.get("items"), ensure_ascii=False)[:120])

        for bad_q, word in (("?page=abc", "page"), ("?page=0", "page"),
                            ("?size=0", "size"), ("?size=99", "size")):
            code, r = call("GET", "/api/v1/mods/search" + bad_q)
            check("翻页参数非法给 400（%s）" % bad_q, code == 400 and word in json.dumps(r, ensure_ascii=False),
                  str(r.get("error"))[:80])

        code, r = call("GET", search_url("", "几何"))
        body = json.dumps(r, ensure_ascii=False)
        check("检索响应里没有 key 形状的串",
              re.search(r"[0-9A-Fa-f]{32}", body.replace(wid, "")) is None, body[:100])
        req = urllib.request.Request(BASE + "/api/v1/mods/search?q=x")
        try:
            urllib.request.urlopen(req, timeout=10)
            noauth = False
        except urllib.error.HTTPError as e:
            noauth = e.code == 401
        check("检索也要认证", noauth)

        # 10) 启停与控制台白名单。这台 Windows 上没有 screen，所以启动必然失败——
        # 但"失败得干净"本身就是这一组接口最容易做错的地方：之前 nm 检查发现启停
        # 根本没进成品，而单测因为用假 Runner 全绿。这里验的是真进程下的结论。
        code, r = call("GET", "/api/v1/admin-commands")
        cmds = r.get("data", [])
        keys = [c["key"] for c in cmds]
        check("白名单命令给的是中文标签", code == 200 and len(cmds) >= 6
              and all(c.get("label") and c["label"] != c["key"] and c.get("desc") for c in cmds), str(keys[:8]))
        check("危险命令标出来了且排在后面",
              any(c["danger"] for c in cmds) and not any(c["danger"] for c in cmds[:2]), str(keys))
        check("参数带中文选项与范围",
              any(a.get("choices") for c in cmds for a in c.get("args", []))
              and any(a.get("min") for c in cmds for a in c.get("args", [])))

        base = "/api/v1/clusters/Cluster_1/shards/Master"
        code, r = call("POST", base + "/admin-commands", {"command": "rollback", "args": {"count": "1"}})
        body = json.dumps(r, ensure_ascii=False)
        check("危险命令没确认时 400", code == 400 and ("confirm" in body or "回档" in body), str(r.get("error"))[:100])

        code, r = call("POST", base + "/admin-commands", {"command": "c_shutdown()"})
        check("白名单外的命令 400 并列出可用", code == 400 and "announce" in json.dumps(r, ensure_ascii=False), str(r.get("error"))[:100])

        code, r = call("POST", base + "/admin-commands",
                       {"command": "rollback", "args": {"count": "99"}, "confirm": True})
        check("越界次数 400 并带原值", code == 400 and "99" in json.dumps(r, ensure_ascii=False), str(r.get("error")))

        # 参数校验在注入之前：这条能走到 503 就说明白名单内命令的路由是通的，
        # 挂掉的只是这台机器没有 screen（转义本身由单测逐字节钉住）。
        code, r = call("POST", base + "/admin-commands", {"command": "save"})
        check("白名单内命令路由可达（无 screen 时 503）",
              code == 503 and "screen" in json.dumps(r, ensure_ascii=False), str(r.get("error"))[:120])

        # /start 在这台 Windows 上必然失败：轻则缺服务端二进制（400 前置检查），
        # 重则缺 screen（503）。两条都是"告诉用户缺什么"，但不能把 Go 的英文原文漏出去。
        # 503 那条分支由单测用真 ExecRunner 覆盖（TestShardStartWithRealRunnerAndNoScreen），
        # 这里不去造假的 bin64 文件：那个目录是往云端同步的源，塞个 0 字节二进制是给自己埋雷。
        code, r = call("POST", base + "/start")
        body = json.dumps(r, ensure_ascii=False)
        check("无 screen/无二进制时 start 给中文结论",
              code in (400, 503) and ("screen" in body or "服务端二进制" in body)
              and "executable file not found" not in body and "no such file" not in body.lower(),
              str(r.get("error"))[:120])

        for path in ("/stop", "/kill", "/restart"):
            code, r = call("POST", base + path)
            body = json.dumps(r, ensure_ascii=False)
            check("无 screen 时 %s 给 503 中文结论" % path,
                  code == 503 and "screen" in body and "executable file not found" not in body, str(r.get("error"))[:100])

        code, r = call("POST", "/api/v1/clusters/Cluster_9/shards/Master/start")
        check("不存在的集群启动 404", code == 404)

        code, r = call("GET", "/api/v1/audit?limit=80")
        acts = [e["action"] for e in r.get("data", [])]
        check("启停与命令都留了审计",
              all(a in acts for a in ("shard.start", "shard.stop", "admin.rollback")), str(acts[:14]))
        check("失败的启动也被记下来",
              any(e["action"] == "shard.start" and e.get("ok") is False for e in r.get("data", [])))

        # 11) 异步任务队列端到端。这里的价值全在"真子进程"上：单测用包内的 fakeLines，
        # main 有没有起 worker、配置里的 steamCmd 有没有传到 Worker.SteamCmd，单测看不出来。
        code, r = call("GET", "/api/v1/task-kinds")
        kinds = {k["kind"]: k for k in (r.get("data") or [])}
        check("任务类型表可读且四类齐全",
              code == 200 and set(kinds) == {"backup.create", "backup.restore", "backup.import", "server.update"},
              str(sorted(kinds))[:140])
        for k in (r.get("data") or []):
            if not k.get("label") or k.get("label") == k.get("kind") or not k.get("desc"):
                check("任务类型有中文标签和说明", False, str(k)[:150])
        upd = kinds.get("server.update") or {}
        check("本体更新标为危险动作", upd.get("danger") is True, str(upd.get("label")))
        # 本体不在时 Note 不再是"做不了"的警告，而是"这一次是全新下载"的说明，
        # 所以这里只要求 runnable，另外单独盯住那句说明在不在。
        check("steamcmd 就位时标为可做", upd.get("runnable") is True, str(upd.get("note"))[:110])
        check("本体缺失时说明是全新下载且不写死大小",
              "还没有服务端本体" in (upd.get("note") or "") and "4.6" not in (upd.get("note") or ""),
              str(upd.get("note"))[:110])
        # 带参数的两类不能从通用提交口走：这里要提前把"该从哪个入口提交"说给界面。
        for nm in ("backup.create", "backup.restore", "backup.import"):
            k = kinds.get(nm) or {}
            check("%s 标为要从存档页提交且现在就可做" % nm,
                  k.get("needsPayload") is True and "/api/v1/" in (k.get("submitVia") or "")
                  and k.get("runnable") is True, str(k)[:150])

        code, r = call("POST", "/api/v1/tasks", {"kind": "server.update"})
        check("危险任务没确认就 400", code == 400 and "需要先确认" in json.dumps(r, ensure_ascii=False),
              str(r.get("error"))[:110])
        code, r = call("GET", "/api/v1/tasks")
        check("被拒的提交没进队列", code == 200 and not r.get("data"), str(r.get("data"))[:80])

        code, r = call("POST", "/api/v1/tasks", {"kind": "rm -rf /", "confirm": True})
        body = json.dumps(r, ensure_ascii=False)
        check("白名单外的任务类型 400 并列出可提交", code == 400 and "server.update" in body, str(r.get("error"))[:120])

        ids = []
        for _ in range(2):
            code, r = call("POST", "/api/v1/tasks", {"kind": "server.update", "confirm": True})
            d = r.get("data", {})
            check("提交返回 202 与任务行", code == 202 and d.get("id")
                  and d.get("statusLabel") in ("排队中", "执行中"), "%s %s" % (code, str(d)[:110]))
            if d.get("id"):
                ids.append(d["id"])

        def snapshot():
            c, rr = call("GET", "/api/v1/tasks?limit=20")
            return {t["id"]: t for t in (rr.get("data") or [])} if c == 200 else {}

        # 等到"一条在执行、一条还在排队"——串行执行是取消规则成立的前提，
        # 而这条只能靠真 worker 的领任务节奏看出来。
        # 顺带把执行期间的进度采样存下来：只有真子进程能证明"边读输出边入库"。
        seen = {}
        samples = []
        for _ in range(80):
            seen = snapshot()
            for i in ids:
                t = seen.get(i) or {}
                if t.get("status") == "running":
                    samples.append((t.get("progress"), t.get("message") or "", t.get("detail")))
            sts = [seen.get(i, {}).get("status") for i in ids]
            if "running" in sts and "queued" in sts:
                break
            time.sleep(0.25)
        running = [i for i in ids if seen.get(i, {}).get("status") == "running"]
        queued = [i for i in ids if seen.get(i, {}).get("status") == "queued"]
        check("两条任务串行：一跑一等", len(running) == 1 and len(queued) == 1, str(seen)[:170])

        if queued:
            code, r = call("DELETE", "/api/v1/tasks/%d" % queued[0])
            d = r.get("data", {})
            check("取消排队中的任务", code == 200 and d.get("status") == "canceled"
                  and d.get("statusLabel") == "已取消", str(d)[:120])
        if running:
            code, r = call("DELETE", "/api/v1/tasks/%d" % running[0])
            check("执行中的任务不给取消", code == 409 and "不做半路掐断" in json.dumps(r, ensure_ascii=False),
                  str(r.get("error"))[:130])

        done = None
        for _ in range(120):
            cur = snapshot()
            t = cur.get(running[0]) if running else None
            if t and t.get("status") == "running":
                samples.append((t.get("progress"), t.get("message") or "", t.get("detail")))
            if t and t.get("status") in ("succeeded", "failed", "canceled"):
                done = t
                break
            time.sleep(0.25)
        check("任务最终落到终态", done is not None, str(done)[:130])
        # 只有真子进程能证明"边读输出边入库"：将来谁把 RunLines 换成一次性攒完，
        # 这条会立刻变成"只在 0 和 100 之间跳"。
        # 认的是中文那句"已下 X / Y"：界面从不显示 steamcmd 的英文原文，
        # 所以这里再盯 "Downloading" 就等于要求面板把英文漏到用户眼前。
        check("执行中确实在逐行报整包进度",
              any(0 < (p or 0) < 100 for p, _, _ in samples)
              and any("已下" in m and "/ 4.3 GB" in m for _, m, _ in samples),
              str(samples[:5])[:170])
        # 数字块那组数：接口必须原样带出 doneBytes/totalBytes，
        # 而且整包量就是替身吐的那个真实总量（面板不许自己估）。
        with_detail = [d for _, _, d in samples if d]
        check("执行中的样本带着可对质的字节明细",
              any(d.get("totalBytes") == 4594664662 and d.get("doneBytes", 0) > 0
                  and d.get("phase") for d in with_detail),
              str(with_detail[:2])[:170])
        # 反过来也要成立：还没有字节明细（= 本体一个字节都没开始下）时，
        # 条子只许停在开局那 1%。哪天解析又收进自更新的百分比，这条先炸。
        check("自更新那一段没有被当成本体进度",
              not any(not d and (p or 0) > 1 for p, _, d in samples),
              str([(p, bool(d)) for p, _, d in samples][:6])[:170])
        if done:
            body = json.dumps(done, ensure_ascii=False)
            # 收尾那句按这台机器有没有本体分两种说法，两种都算过。
            check("跑完的任务进度满格且消息是结论",
                  done.get("status") == "succeeded" and done.get("progress") == 100
                  and any(w in (done.get("message") or "") for w in ("服务端本体已更新", "服务端本体已下载")),
                  body[:200])
            # 跑完之后"实测速度"已经没有意义，收尾那条按设计不带字节明细。
            check("跑完的任务不再留着速度那组数", not done.get("detail"), body[:200])
            check("任务消息里没有 Go 的英文原文",
                  not any(x in body for x in ("executable file not found", "fork/exec",
                                              "exit status", "no such file", "context deadline")),
                  body[:200])
        if queued:
            t = snapshot().get(queued[0], {})
            check("取消后 worker 没有再领它", t.get("status") == "canceled" and not t.get("startedAt"), str(t)[:130])
            code, r = call("DELETE", "/api/v1/tasks/%d" % queued[0])
            check("已取消的任务不再让取消", code == 409 and "已经是" in json.dumps(r, ensure_ascii=False),
                  str(r.get("error"))[:110])

        code, r = call("DELETE", "/api/v1/tasks/abc")
        check("非数字任务 id 400 并带原值", code == 400 and "数字" in json.dumps(r, ensure_ascii=False)
              and "abc" in json.dumps(r, ensure_ascii=False), str(r.get("error"))[:90])
        code, r = call("DELETE", "/api/v1/tasks/424242")
        check("不存在的任务 404", code == 404, str(r.get("error"))[:90])

        code, r = call("GET", "/api/v1/audit?limit=80")
        acts = [e["action"] for e in r.get("data", [])]
        check("提交与取消都留了审计", "task.submit" in acts and "task.cancel" in acts, str(acts[:14]))

        backup_checks()
        log_checks()
        events_checks()
        map_checks()
        players_checks()
        lobby_checks()
        account_checks()
        owner_checks()
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except Exception:
            proc.kill()
        log.close()

    # 有 key 就多跑一次：这一组只验一件事——配置里的 key 真的走到了成品里。
    # 没有它，main.go 写成 steam.New("", "") 也照样能过前面所有检查。
    key = os.environ.get("DST_STEAM_KEY", "").strip()
    if key:
        live_checks(key)
    else:
        print("[SKIP] 没设 DST_STEAM_KEY：在线路径与配置接线改由云机探针验证")

    bad = [n for n, ok, _ in results if not ok]
    print("\n合计 %d 项，失败 %d 项" % (len(results), len(bad)))
    if bad:
        print("失败：" + "、".join(bad))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
