#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""云机首启（无 Klei 令牌的那半段）。

为什么要单独一个脚本，而不是把这段塞进 cloud_search_probe.py：
那支探针的铁律是"绝不碰真存档、绝不起停分片"，而首启要做的正是这两件事。
所以这里换成一份真配置：
  kleiRoot   = /home/ubuntu/.klei/DoNotStarveTogether   （实测引擎忽略 -savedir，只认这里）
  dstInstall = /home/ubuntu/dst/dedicated_server        （真服务端本体）
  workDir    = /home/ubuntu/dst/panel-boot              （面板自己的库，和 panel-probe 分开）

这一段能验完的：成品在 Linux 上起得来、开服接口真建出两分片集群、总览页列出它、
世界文件按各自地形的字典写全、没有令牌时"服务端拒绝启动"的判定真的命中，
并且留下的 screen 会话能被强制停止清干净。
剩下只差用户自己的 Klei 令牌：PUT 进去再点启动才算真开服。
"""
import hashlib
import json
import os
import secrets
import subprocess
import sys
import time

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOST = "dst-cloud"
BIN_LOCAL = os.path.join(ROOT, ".tmp_bin", "dstpanel-linux")
BOOT_DIR = "/home/ubuntu/dst/panel-boot"
BIN_REMOTE = BOOT_DIR + "/dstpanel"
KLEI_REAL = "/home/ubuntu/.klei/DoNotStarveTogether"
DST_REAL = "/home/ubuntu/dst/dedicated_server"
PORT = 8942
ADMIN = "admin"
CFG_NAME = "cfg_boot.yaml"

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok)))
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  -- " + str(detail)[:300]) if detail else ""))


def remote(script, timeout=240):
    """整段 bash 走 stdin：中文不进本机 argv，密码也不出现在本机的命令行里。"""
    p = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", HOST, "bash -s"],
                       input=script.encode("utf-8"), capture_output=True, timeout=timeout)
    out = p.stdout.decode("utf-8", "replace")
    if p.returncode != 0:
        out += "\n[ssh rc=%d] %s" % (p.returncode, p.stderr.decode("utf-8", "replace").strip()[:200])
    return out


def token_state(cluster, fake):
    """只看集群令牌的"身份"，绝不打印内容：ABSENT / FAKE / REAL。

    这一支要在别人的服务器上动 cluster_token.txt，先把身份问清楚再决定动不动。
    REAL 时连长度都不打印（79 字节这种数字留着也没意义，还可能被当成线索）。
    """
    out = remote("""F="%(klei)s/%(n)s/cluster_token.txt"
if [ ! -f "$F" ]; then echo ABSENT;
elif [ "$(tr -d '\\r\\n' < "$F")" = "%(fake)s" ]; then echo FAKE;
else echo REAL; fi
""" % {"klei": KLEI_REAL, "n": cluster, "fake": fake})
    for line in out.splitlines():
        if line.strip() in ("ABSENT", "FAKE", "REAL"):
            return line.strip()
    return "?"


def session_count(cluster, shard):
    """数某个集群某个分片的 screen 会话，不受别的分片影响。"""
    out = remote('screen -ls 2>&1 | grep -c "dst-%s-%s" || true' % (cluster, shard))
    return (out.strip().splitlines() or ["?"])[-1].strip()


def push_binary():
    if not os.path.isfile(BIN_LOCAL):
        sys.exit("先跑 .tools/rebuild.sh 生成 .tmp_bin/dstpanel-linux")
    local = hashlib.sha256(open(BIN_LOCAL, "rb").read()).hexdigest()
    remote("mkdir -p %s\n" % BOOT_DIR)
    r = subprocess.run(["scp", "-q", BIN_LOCAL, "%s:%s" % (HOST, BIN_REMOTE)],
                       capture_output=True, timeout=600)
    if r.returncode != 0:
        sys.exit("scp 失败：" + r.stderr.decode("utf-8", "replace")[:200])
    out = remote("chmod 755 %s\nsha256sum %s | cut -d' ' -f1\nfile -b %s | cut -c1-40\n"
                 % (BIN_REMOTE, BIN_REMOTE, BIN_REMOTE)).split("\n")
    check("云机成品与本地 sha256 一致", out[0].strip() == local,
          "远端 %s / 本地 %s" % (out[0].strip()[:16], local[:16]))
    check("推过去的是 Linux 可执行文件", "ELF" in (out[1] if len(out) > 1 else ""), out[1:2])


def config_yaml(pw):
    return """# 云机首启用配置。kleiRoot 必须是引擎自己那份：服务端二进制忽略 -savedir。
server:
  bind: 127.0.0.1
  port: %d
  adminUser: %s
  adminPassword: "%s"
paths:
  kleiRoot: "%s"
  dstInstall: "%s"
  steamCmd: ""
  workDir: "%s"
network:
  shardPortBase: 10998
  shardPortEnd: 11900
  masterIntentPortBase: 11000
backup:
  keep: 5
helper:
  pollMs: 1000
  staleSeconds: 15
steamApi:
  webApiKey: ""
  proxy: ""
""" % (PORT, ADMIN, pw, KLEI_REAL, DST_REAL, BOOT_DIR)


class Panel:
    def __init__(self, pw):
        self.pw = pw
        self.pid = ""

    def start(self):
        cfg = config_yaml(self.pw)
        if "CFGEOF" in cfg:
            sys.exit("配置里撞上了 heredoc 分隔符，拒绝拼脚本")
        script = """set -u
umask 077
mkdir -p %(dir)s
cat > %(dir)s/%(name)s <<'CFGEOF'
%(cfg)s
CFGEOF
chmod 600 %(dir)s/%(name)s
pgrep -f '%(killpat)s' | xargs -r kill 2>/dev/null
sleep 1
nohup %(bin)s -config %(dir)s/%(name)s > %(dir)s/boot.log 2>&1 &
echo "PID=$!"
up=0
for i in $(seq 1 60); do
  if curl -s -m 2 -o /dev/null http://127.0.0.1:%(port)s/healthz; then up=1; break; fi
  sleep 0.5
done
echo "UP=$up"
echo "WARNCOUNT=$(grep -c 'savedir' %(dir)s/boot.log || true)"
""" % {"dir": BOOT_DIR, "name": CFG_NAME, "cfg": cfg.rstrip("\n"),
       "bin": BIN_REMOTE, "port": PORT, "killpat": "%s/%s" % (BOOT_DIR, CFG_NAME)}
        out = remote(script)
        up, warns = "0", "?"
        for line in out.splitlines():
            if line.startswith("PID="):
                self.pid = line[4:].strip()
            if line.startswith("UP="):
                up = line[3:].strip()
            if line.startswith("WARNCOUNT="):
                warns = line[len("WARNCOUNT="):].strip()
        ok = up == "1"
        check("Linux 成品在真配置下起得来", ok, out.replace("\n", " ")[:240])
        # 配置指向的是真 kleiRoot，所以那句存档目录提示不该出现；
        # 出现了就是 kleiRoot 又被写歪了，后面整段都是在跟一个假目录较劲。
        check("真 kleiRoot 下不会再喊存档目录提示", ok and warns == "0", "WARNCOUNT=%s" % warns)
        if not ok:
            check("启动日志", False, self.log_tail())
        return ok

    def log_tail(self):
        return remote("tail -12 %s/boot.log 2>/dev/null | tr '\\n' ' '" % BOOT_DIR)[:500]

    def call(self, method, path, payload=None):
        body = json.dumps(payload, ensure_ascii=False) if payload is not None else ""
        if "PAYEOF" in body:
            sys.exit("请求体撞上了 heredoc 分隔符")
        write_part = ""
        data_part = ""
        if payload is not None:
            write_part = "cat > %s/payload.json <<'PAYEOF'\n%s\nPAYEOF\n" % (BOOT_DIR, body)
            data_part = "--data-binary @%s/payload.json " % BOOT_DIR
        script = ('%scurl -s -m 120 -u "%s:%s" -X %s -H "Content-Type: application/json" %s'
                  '"http://127.0.0.1:%d%s" -w \'\\nHTTP=%%{http_code}\\n\'\n'
                  % (write_part, ADMIN, self.pw, method, data_part, PORT, path))
        raw = remote(script, timeout=180)
        text, _, tail = raw.rpartition("HTTP=")
        code = tail.strip()
        try:
            return (int(code) if code.isdigit() else -1), json.loads(text.strip())
        except Exception:  # noqa: BLE001
            return (int(code) if code.isdigit() else -1), {"raw": text.strip()[:400]}

    def stop(self):
        if self.pid:
            remote("kill %s 2>/dev/null; sleep 1; kill -9 %s 2>/dev/null; true\n" % (self.pid, self.pid))
            self.pid = ""


def pick(schema_view, location):
    """从真字典里挑一个"和默认值不同"的取值：改了等于没改的话，回显断言就是在比空气。"""
    for loc in schema_view:
        if loc.get("location") != location:
            continue
        for g in loc.get("groups", []):
            for it in g.get("items", []):
                for o in it.get("options", []):
                    if o["value"] != it["default"]:
                        return it["key"], o["value"]
    return None, None


def main():
    # 面板密码：本机随机生成，只落进远端那份 0600 配置，不进本机 argv、不进任何输出。
    pw = secrets.token_urlsafe(12)
    push_binary()
    p = Panel(pw)
    if not p.start():
        return finish(pw, "")

    # 1) 开服接口：真建一个地表 + 洞穴集群，两边各带一项世界设定。
    code, r = p.call("GET", "/api/v1/world-schema")
    schema = r.get("data") or []
    fkey, fval = pick(schema, "forest")
    ckey, cval = pick(schema, "cave")
    check("世界设定字典在云机读得动（两张表）", code == 200 and len(schema) == 2 and fkey and ckey,
          "forest=%s cave=%s" % (fkey, ckey))
    item_count = {l.get("location"): l.get("itemCount", 0) for l in schema}
    # 预设通道：面板只写 override_enabled / preset / overrides 三个顶层键
    # （2026-10-31 云机实测），完整世界定义由 preset 从 scripts/map/levels.lua 带进来。
    default_preset = {l.get("location"): (l.get("defaultPreset") or {}).get("value") for l in schema}
    preset_ids = {l.get("location"): [p.get("value") for p in (l.get("presets") or [])] for l in schema}
    check("字典在云机也给出每个地形的可选预设与默认预设",
          all(default_preset.get(l) and default_preset.get(l) in preset_ids.get(l, [])
              for l in ("forest", "cave")),
          json.dumps({"default": default_preset, "ids": preset_ids}, ensure_ascii=False)[:220])

    # 重复运行要能接着验：面板没有"删集群"接口（删存档这种不可逆的事不留接口），
    # 所以这里认出上次建的这一份，直接往下验文件与启动，不重复建、也不远程删目录。
    # 认领条件必须两条都对：上一版只按显示名 grep，而云机上正式在跑的 Cluster_1
    # 显示名也叫"首启验证服"（就是这一支建出来的），于是下面 4b 会把那个唯一能用
    # 的集群当真令牌探针用——把用户的真令牌覆盖成 32 个十六进制字符再 rm -f。
    # 描述那句是本探针独有的，加上它才只认自己建的那一份。
    found = remote('grep -l "面板自建，用来验开服链路" "%s"/*/cluster.ini 2>/dev/null | head -1'
                   % KLEI_REAL).strip()
    name = os.path.basename(os.path.dirname(found)) if found else ""
    if name:
        check("重复运行时沿用已有集群（面板不提供删集群）", True, name)
        # 沿用时要先让面板把世界设定重写一遍：这台机器上做世界生成实验时手工拷过
        # worldgenoverride.lua（甚至删过 leveldataoverride.lua），不重写的话下面那些
        # 磁盘断言验的是上一次实验的残留，不是面板的输出。
        for shard, loc, key, val in (("Master", "forest", fkey, fval), ("Caves", "cave", ckey, cval)):
            code, r = p.call("PUT", "/api/v1/clusters/%s/shards/%s/world" % (name, shard),
                             {"preset": default_preset.get(loc), "overrides": {key: val}})
            check("沿用已有集群时先用 PUT 重写 %s 的世界设定" % shard, code == 200,
                  json.dumps(r, ensure_ascii=False)[:200])
    else:
        code, r = p.call("POST", "/api/v1/clusters", {
            "numShards": 2,
            "installHelper": True,
            "game": {"displayName": "首启验证服", "description": "面板自建，用来验开服链路",
                     "maxPlayers": 6, "gameMode": "survival", "pvp": False, "language": "zh"},
            "world": {"presets": {"Master": default_preset.get("forest"),
                                  "Caves": default_preset.get("cave")},
                      "flatOverrides": {fkey: fval},
                      "overrides": {"Caves": {ckey: cval}}},
        })
        d = r.get("data") or {}
        name = d.get("name") or ""
        check("开服接口建出集群并给回集群名", code == 200 and bool(name), str(r)[:240])
        if not name:
            p.stop()
            return finish(pw, "")
        shards = [s.get("shard") for s in d.get("shards", [])]
        check("两个分片都建好了", shards == ["Master", "Caves"], str(shards))
        # 界面要拿到中文说法；给出 SURVIVAL_TOGETHER 这种码值等于把引擎字符串甩给用户。
        check("开服回显给每个分片标了预设的中文说法",
              all(s.get("presetLabel") and s.get("presetLabel") != s.get("preset")
                  for s in d.get("shards", [])),
              json.dumps([[s.get("shard"), s.get("preset"), s.get("presetLabel")]
                          for s in d.get("shards", [])], ensure_ascii=False)[:200])
        notes = "\n".join(d.get("notes", []))
        # 没给令牌时，接口必须把引擎的原话讲出来，而不是"配置写好了，去试试吧"。
        check("响应里先说清了没令牌引擎会拒绝", "Your Server Will Not Start" in notes
              or "拒绝" in notes, notes[:240])

    # 2) 总览页：这台机器有 screen，所以这条是真断言（本机 Windows 只能验到 503 那一半）。
    code, r = p.call("GET", "/api/v1/clusters")
    listed = [c for c in (r.get("data") or []) if c.get("name") == name]
    check("总览页列出新建的集群", code == 200 and len(listed) == 1
          and [s.get("name") for s in listed[0].get("shards", [])] == ["Master", "Caves"],
          json.dumps(r.get("data"), ensure_ascii=False)[:200])
    check("未启动时分片报未运行而不是报错", listed and
          all(s.get("running") is False for s in listed[0].get("shards", [])),
          json.dumps(listed[0].get("shards") if listed else [], ensure_ascii=False)[:200])

    # 3) 磁盘上到底留下了什么：接口说成功不算成功，引擎读的那几个文件得对。
    # 世界文件走预设通道：开关 + 预设 + overrides 表，三样顶层键，一个不多。
    # 缺开关时引擎打一句 "Found world gen overrides but not enabled" 把整份文件当没有
    # ——界面显示的设定和引擎用的世界就此分家，而且不报错。
    disk = remote("""set -u
K="%(klei)s/%(name)s"
echo "== ini 段头 =="; grep -n '^\\[' "$K/cluster.ini" | head -6
echo "== 关键项 =="; grep -E 'cluster_name|max_players|game_mode|shard_enabled' "$K/cluster.ini"
echo "== 地表世界文件头 =="; sed -n '1,4p' "$K/Master/worldgenoverride.lua"
echo "== 洞穴世界文件头 =="; sed -n '1,4p' "$K/Caves/worldgenoverride.lua"
echo "== 面板不该留下的东西 =="
ls "$K/Master" "$K/Caves" | grep -E 'leveldataoverride' ;
grep -cE '^[[:space:]]+(layout_mode|has_ocean|wormhole_prefab|keep_disconnected_tiles|no_joining_islands|random_set_pieces|required_setpieces)[[:space:]]*=' "$K/Master/worldgenoverride.lua" "$K/Caves/worldgenoverride.lua" ;
echo "== ENDSAME =="
echo "== 助手 MOD 目录 =="; ls %(dst)s/mods 2>&1 | head -6
echo "== 助手 override 条目 =="; grep -E 'DSTPanelHelper|enabled|configuration_options' "$K/Master/modoverrides.lua" | head -6
echo "== 令牌文件 =="; ls -l "$K/cluster_token.txt" 2>&1 | head -2
""" % {"klei": KLEI_REAL, "name": name, "dst": DST_REAL})
    check("cluster.ini 没有引擎不认的双层段头", "[[" not in disk, disk[:200].replace("\n", " | "))
    check("cluster.ini 写了首启验证服与 6 人上限", "首启验证服" in disk and "max_players = 6" in disk,
          disk[:260].replace("\n", " | "))

    def head_lines(marker, stop):
        sec = disk[disk.find(marker):disk.find(stop)]
        lines = [ln.strip() for ln in sec.splitlines() if ln.strip()]
        return lines[1:]  # 去掉 echo 的段名那行

    for shard, loc, marker, stop in (("Master", "forest", "== 地表世界文件头 ==", "== 洞穴世界文件头 =="),
                                     ("Caves", "cave", "== 洞穴世界文件头 ==", "== 面板不该留下的东西 ==")):
        want = 'preset = "%s",' % (default_preset.get(loc) or "")
        got = head_lines(marker, stop)
        check("%s 的世界文件就是开关+预设+表这三样" % shard,
              got == ["return {", "override_enabled = true,", want, "overrides = {"],
              str(got)[:200].replace("\n", " | "))
    # 面板不写 leveldataoverride.lua（GAME USE 文件，要一整份完整世界定义，
    # 手工写半套会让世界生成死在 random_set_pieces 的取长度上）；
    # 引擎内生项由预设带进来，写进这份文件只会在日志里换一条 WARNING。
    # 这一段是上面 ls/grep 的输出，干净时只有两份文件各一个 :0。
    same = disk[disk.find("== 面板不该留下的东西 =="):disk.find("== ENDSAME ==")]
    check("磁盘上没有 leveldataoverride.lua，世界文件里也没有面板补的内生项",
          "leveldataoverride" not in same and same.count(":0") == 2, same[:220].replace("\n", " | "))
    # 引擎不剥引号：game_mode = "survival" 会被读成 '"survival"'，然后
    # "Game mode '\"survival\"' not found in GAME_MODES"——云机日志里就是这么写的。
    ini_sec = disk[disk.find("== 关键项 =="):disk.find("== 地表世界文件头 ==")]
    check("cluster.ini 的字符串值不带引号", '= "' not in ini_sec and "game_mode = survival" in ini_sec,
          ini_sec[:220].replace("\n", " | "))

    helper_sec = disk[disk.find("== 助手 MOD 目录 =="):]
    check("助手 MOD 进了真安装目录", "DSTPanelHelper" in helper_sec, helper_sec[:220].replace("\n", " | "))
    # 键名形态：Lua 写出的是 `DSTPanelHelper = {`（合法标识符就不加引号），
    # 工坊模组那种带连字符的才会是 `["workshop-123"] = {`。两种都要认。
    check("modoverrides 启用了助手并带配置项",
          ("DSTPanelHelper = {" in helper_sec or '["DSTPanelHelper"]' in helper_sec)
          and "enabled = true" in helper_sec and "configuration_options" in helper_sec,
          helper_sec[:240].replace("\n", " | "))
    token_sec = disk.split("== 令牌文件 ==")[-1]
    check("没令牌时磁盘上确实没有令牌文件", "No such file" in token_sec, token_sec[:160].replace("\n", " | "))

    # 世界设定的回显：面板写全整张字典，所以读回来的项数就是这张表的条目数
    # （内生项由预设带进来，不在这份文件里，也就不会出现在回显里）。
    for shard, loc, key, val in (("Master", "forest", fkey, fval), ("Caves", "cave", ckey, cval)):
        code, r = p.call("GET", "/api/v1/clusters/%s/shards/%s/world" % (name, shard))
        v = r.get("data") or {}
        check("%s 的世界文件按本表写全（%d 项）" % (shard, item_count.get(loc, 0)),
              code == 200 and v.get("present") is True and len(v.get("overrides") or {}) == item_count.get(loc, 0),
              "读回 %d 项 / 应为 %d 项" % (len(v.get("overrides") or {}), item_count.get(loc, 0)))
        check("%s 回显认出提交的那项改动" % shard,
              key in (v.get("changedFromDefault") or []) and (v.get("overrides") or {}).get(key) == val,
              "changed=%s %s=%s" % ((v.get("changedFromDefault") or [])[:4], key,
                                    (v.get("overrides") or {}).get(key)))
        check("%s 的预设回显带中文说法且指对文件" % shard,
              v.get("preset") == default_preset.get(loc) and v.get("presetLabel")
              and v.get("presetLabel") != v.get("preset")
              and (v.get("path") or "").endswith("worldgenoverride.lua"), str(v)[:170])
        # 上一次实验在这台机器上留过手工文件；面板建的世界不该带着它。
        check("%s 没有手工 leveldataoverride.lua 的冲突提示" % shard, not v.get("levelDataConflict"),
              str(v.get("note"))[:120])

    # 4) 令牌这一节改到"探针专用集群"上做。
    # 2026-10-31 云机实测：Cluster_1 就是这一支建出来的（cluster_description 对得上），
    # 而它现在的 cluster_token.txt 是 79 字节的真令牌（0600）。继续在原集群上做假令牌
    # 实验，重跑一次就把用户唯一的可用令牌覆盖成 32 个十六进制字符再 rm -f ——
    # 那个只能去 accounts.klei.com 重新取，云机上这个能开的集群当场废掉。
    fake = "0123456789abcdef0123456789abcdef"
    probe = name
    name_state = token_state(name, fake)
    probe_state = name_state
    if name_state != "ABSENT":
        print("   %s 的令牌状态=%s（真令牌只看状态，从不打印内容），这一支绝不覆盖它" % (name, name_state))
        code, r = p.call("POST", "/api/v1/clusters", {
            "numShards": 1,
            "installHelper": False,
            "game": {"displayName": "令牌探针（用完可删）",
                     "description": "只用来验假令牌会被引擎拒绝，跟正式集群无关",
                     "maxPlayers": 2, "gameMode": "survival", "pvp": False, "language": "zh"},
            "world": {"presets": {"Master": default_preset.get("forest")},
                      "flatOverrides": {fkey: fval}},
        })
        d = r.get("data") or {}
        # 面板自己挑下一个空号，所以这里认它返回的名字，不猜 Cluster_2。
        probe = d.get("name") or ""
        check("另建一个没有令牌的探针集群来做假令牌实验", code == 200 and bool(probe), str(r)[:240])
        if probe:
            probe_state = token_state(probe, fake)
            check("探针集群建出来本来就没有令牌文件", probe_state == "ABSENT", "state=%s" % probe_state)

    if probe_state != "ABSENT":
        check("假令牌实验没有可做的集群，这一段跳过而不是硬跑", False,
              "探针集群没建出来或已有令牌，状态=%s" % probe_state)
    else:
        # 4a) 完全没有令牌文件：前置检查就该当场拦住，绝不放一个必输的进程出去占内存。
        code, r = p.call("POST", "/api/v1/clusters/%s/shards/Master/start" % probe)
        msg = json.dumps(r, ensure_ascii=False)
        check("没有令牌文件时启动被前置检查拦住", code == 400 and "cluster_token.txt" in msg, msg[:240])
        check("前置检查拦下时没有留下探针会话", session_count(probe, "Master") == "0",
              "dst-%s-Master 会话数=%s" % (probe, session_count(probe, "Master")))

        # 4b) 形状合法、但不是真令牌的串：进程真会起来，然后在日志里写"不会启动"，
        # 而 screen 会话照旧挂着。这是"会话在跑 ≠ 服务器起来了"的唯一真实验证机会，
        # 本机（Windows）没有服务端二进制，永远测不到这一段。
        code, r = p.call("PUT", "/api/v1/clusters/%s/token" % probe, {"token": fake})
        check("写入合法形状的令牌成功且响应不回明文",
              code == 200 and fake not in json.dumps(r, ensure_ascii=False), str(r)[:160])
        t0 = time.time()
        code, r = p.call("POST", "/api/v1/clusters/%s/shards/Master/start" % probe)
        took = time.time() - t0
        msg = json.dumps(r, ensure_ascii=False)
        check("假令牌下真引擎承认拒绝启动，面板报 409", code == 409, msg[:300])
        check("结论引用了引擎日志原话",
              "Your Server Will Not Start" in msg or "No auth token" in msg, msg[:300])
        check("结论交代了会话还挂着、要先强停", "强制停止" in msg, msg[:300])
        # 409 只是面板的说法，得跟磁盘上的真相核对：引擎自己写的日志里真有那三行，
        # 这条判定才不算蒙的。耗时只作观察记录（窗口 10 秒，实测 5~7 秒命中）。
        truth = remote('grep -c "Your Server Will Not Start" "%s/%s/Master/server_log.txt" 2>/dev/null || echo 0'
                       % (KLEI_REAL, probe)).strip()
        check("409 与引擎日志里的原话对得上", truth.splitlines()[0].strip() != "0",
              "日志命中 %s，接口耗时 %.1fs" % (truth.splitlines()[0].strip(), took))

        # 会话这时候还挂着（进程没退出），必须能用强停清干净，不能留僵尸会话。
        code, r = p.call("POST", "/api/v1/clusters/%s/shards/Master/kill" % probe)
        check("挂着的会话能被强制停止清掉", code == 200, json.dumps(r, ensure_ascii=False)[:200])
        # 这两条只针对探针集群：正式分片这会儿正在跑，全局 "No Sockets" 永远不成立，
        # 上一版就是靠这条误判"清干净了"。
        sessions = session_count(probe, "Master")
        procs = remote('pgrep -af "cluster %s" | wc -l' % probe).strip().splitlines()[-1].strip()
        check("探针会话已清干净", sessions == "0", "dst-%s-* 会话数=%s" % (probe, sessions))
        check("探针的服务端进程没留下", procs == "0", "匹配到 %s 个进程" % procs)

        # 假令牌是探针写进去的，收尾必须清掉：留着它，用户填真令牌之前点启动会继续撞 409。
        # 删除前先对内容——只对得上这一串十六进制才动手，对不上就停下让人来看。
        gone = remote("""F="%(klei)s/%(n)s/cluster_token.txt"
if [ ! -f "$F" ]; then echo ALREADY-GONE;
elif [ "$(tr -d '\\r\\n' < "$F")" = "%(fake)s" ]; then rm -f "$F"; echo CLEANED;
else echo NOT-OURS; fi
ls -1 "%(klei)s/%(n)s" | head -8""" % {"klei": KLEI_REAL, "n": probe, "fake": fake})
        check("探针的假令牌按内容核对后才清掉，集群目录留着",
              "NOT-OURS" not in gone and "cluster_token.txt" not in gone,
              gone[:200].replace("\n", " | "))

    # 最后再验一次：在用集群的令牌从头到尾没被碰过。
    name_state_after = token_state(name, fake)
    check("在用集群的令牌状态跑前跑后一致（没有被覆盖或删除）",
          name_state_after == name_state, "前=%s 后=%s" % (name_state, name_state_after))
    if probe != name:
        print("   探针集群 %s 用完了：面板没有删集群的接口（不可逆的事不留接口），"
              "里面没有存档也没有令牌，要清就手工 rm -rf %s/%s" % (probe, KLEI_REAL, probe))

    p.stop()
    return finish(pw, name)


def finish(pw, created=""):
    bad = [n for n, ok in results if not ok]
    print("\n合计 %d 项，失败 %d 项" % (len(results), len(bad)))
    if bad:
        print("失败：" + "、".join(bad))
    print("面板配置留在云机 %s/%s（0600，随机密码写在那份文件里，没有打印在这里）" % (BOOT_DIR, CFG_NAME))
    if created:
        print("集群 %s 已建在 %s；拿到 Klei 令牌后 PUT /api/v1/clusters/%s/token 再点启动就是真开服。"
              % (created, KLEI_REAL, created))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
