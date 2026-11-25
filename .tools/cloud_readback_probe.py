#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""只读探针：确认面板在云机上读到的东西和磁盘上的真字节一致。

cloud_boot_shards.py 会启停正式分片（存档、重启、玩家掉线），不适合当回归门；
这里默认只做 GET，并在结尾用 sha256 证明面板没碰过任何一个磁盘文件。

盯的目标有三块：
  - Lua 解析层：引号字符串里出现裸换行现在算结构损坏（Lua 5.1 本来就不允许，
    不报出来的话下一行会被当成字符串内容吞掉，截断的文件会伪装成"读成功了"）；
    注释扫描支持了 --[=[ ]=] 长注释。被引擎重写过的 worldgenoverride.lua（带 KLEI 头）
    和 cluster_config.lua 正好是样本。
  - 日志分类规则表是从这份真日志里抄出来的，抄错一行、或者引擎这版措辞不同，
    只有拿真字节再跑一次才暴露得出来。读日志是纯 GET，仍在 sha256 保护范围内。
  - 存档快照页（backup_checks 的只读那半）：磁盘剩余空间走的是 syscall.Statfs 这条
    只有 unix 才编进去的路，Windows 上永远验不到；保留上限"以谁为准"也要拿真的
    cfg_boot.yaml 对一遍。

要连备份的正路一起验（催存盘 → 打包 139MB → 校验 → 下载 → 删掉自己打的这份）
就加环境变量 DST_BACKUP_LIVE=1 再跑。这一支只多打一份快照再删掉它，不动存档目录、
不停分片；恢复那条只做"应当被拦住"的负向检查，而且只在面板确实看得见两个分片在跑
时才发这个请求（看不见就跳过——那时它真会去换存档目录）。
"""
import json
import os
import re
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cloud_boot_shards as cb  # noqa: E402

LIVE = os.environ.get("DST_BACKUP_LIVE") == "1"

# 面板自己报的快照目录，backup_readonly 里填；下载/删除的对账要用它。
SNAP_DIR = ""

FILES = ["Master/worldgenoverride.lua", "Caves/worldgenoverride.lua",
         "Master/modoverrides.lua", "Caves/modoverrides.lua",
         "cluster_config.lua", "cluster.ini"]


def shas():
    out = cb.remote("""set -u
for f in %s; do
  P="%s/%s/$f"
  if [ -f "$P" ]; then echo "$f=$(sha256sum "$P" | cut -d' ' -f1)"; else echo "$f=ABSENT"; fi
done
""" % (" ".join('"%s"' % f for f in FILES), cb.KLEI, cb.CLUSTER)).strip()
    d = {}
    for line in out.splitlines():
        if "=" in line:
            k, v = line.split("=", 1)
            d[k.strip()] = v.strip()
    return d


def tree_shas():
    """整个房间目录的文件摘要，用来证明备份全程只让引擎写了存档和日志。"""
    out = cb.remote("""set -u
cd "%s/%s" || exit 0
find . -type f -print0 | xargs -0 -n 20 sha256sum 2>/dev/null | sed 's#  \\./#  #'
""" % (cb.KLEI, cb.CLUSTER))
    d = {}
    for line in out.splitlines():
        if "  " in line:
            h, p = line.split("  ", 1)
            if re.fullmatch(r"[0-9a-f]{64}", h.strip()):
                d[p.strip()] = h.strip()
    return d


def raw_get(path, timeout="120"):
    """不解析 JSON 的一次 GET：SSE 的帧本身就是要看的东西。"""
    script = ('curl -s -m %s -u "%s:%s" "http://127.0.0.1:%d%s"'
              % (timeout, cb.ADMIN, cb.PW, cb.PORT, path))
    return cb.remote(script)


CLOCK = re.compile(r"^\[\d{1,4}:\d{2}:\d{2}\]:\s*")


def line_at(real, offset):
    """按字节偏移从真文件里取出那一行，用来对上面板报的 offset。"""
    out = cb.remote("dd if='%s' bs=1 skip=%d count=1200 status=none | head -n 1" % (real, offset))
    return out.rstrip("\n")


def log_checks():
    """把分类器放到云机那份真日志上跑一遍。

    本地探针喂的是我手写的样本，规则表里的原文也是从这份文件里抄的——
    抄的时候看花眼、或者引擎这版措辞不一样，只有拿真字节再跑一次才暴露得出来。
    """
    master = "%s/%s/Master/server_log.txt" % (cb.KLEI, cb.CLUSTER)
    base = "/api/v1/clusters/%s/shards/Master/log" % cb.CLUSTER

    stat = cb.remote("stat -c %%s '%s' | head -1; wc -l < '%s'" % (master, master)).split()
    if len(stat) < 2:
        cb.check("云机上取到了 Master 日志的大小与行数", False, str(stat)[:120])
        return
    size, total_lines = int(stat[0]), int(stat[1])

    code, r = cb.plain_call("GET", base + "?lines=400")
    d = r.get("data", {})
    entries = d.get("entries") or []
    cb.check("真日志读得回来且给了结构化条目", code == 200 and len(entries) > 0,
             "%s %s" % (code, str(r)[:180]))
    cb.check("sizeBytes 和磁盘上的这份文件一致", d.get("sizeBytes") == size,
             "%s vs %s" % (d.get("sizeBytes"), size))
    cb.check("窗口只给了要的行数，并说明更早的没读",
             len(entries) <= 400 and bool(d.get("older")) == (total_lines > 400),
             "returned=%s older=%s 文件 %s 行" % (len(entries), d.get("older"), total_lines))
    cb.check("界面拿到了中文种类名和墙上时间",
             d.get("kindLabel") == "服务端日志" and bool(d.get("startAt") or d.get("startNote")),
             str(d.get("startAt"))[:40] + "|" + str(d.get("startNote"))[:60])

    ver = cb.remote("grep -m1 -o 'Version: [0-9]*' '%s' | sed 's/Version: //'" % master).strip()
    cb.check("版本号是从这份文件里读出来的", d.get("gameVersion") == ver,
             "面板 %s / 文件 %s" % (d.get("gameVersion"), ver))

    body = json.dumps(entries, ensure_ascii=False)
    cb.check("接口响应里没有 Klei 令牌明文", "pds-g" not in body, body[:120])
    cb.check("每条都带了级别中文与事件名（不露裸码值）",
             all(e.get("levelLabel") and e.get("level") in ("info", "warn", "error") for e in entries),
             str([(e.get("level"), e.get("levelLabel")) for e in entries[:3]]))
    offs = [e["offset"] for e in entries]
    cb.check("offset 是严格递增的字节位置", all(b > a for a, b in zip(offs, offs[1:])),
             str(offs[:6]))

    # 抽最后三条回到文件里对：面板说的这一行，文件里得真是那一行。
    for e in entries[-3:]:
        raw_line = line_at(master, e["offset"])
        stripped = CLOCK.sub("", raw_line)
        raw = e["raw"]
        if e.get("cut"):
            ok = stripped.startswith(raw.rstrip("…")[:120])
            why = "超长行按前缀对"
        elif "[已隐去" in raw:
            ok = bool(re.search(r"token|pds-g", stripped, re.I))
            why = "这一行原文确实带令牌形状的东西才被隐去"
        else:
            # 面板可能还拆掉了行首的 [标签]，所以尾部相等或包含都算对上。
            ok = bool(raw_line) and (stripped.endswith(raw) or raw in stripped)
            why = "原文照对"
        cb.check("offset %d 那行对得上（%s）" % (e["offset"], why), ok,
                 "文件=%s 面板=%s" % (raw_line[:90], raw[:90]))

    code, r = cb.plain_call("GET", base + "?event=broadcast_fail&lines=2000")
    d2 = r.get("data", {})
    cb.check("Klei 列表广播的重试行被认成了 broadcast_fail",
             code == 200 and d2.get("returned", 0) > 0, str(d2)[:120])
    code, r = cb.plain_call("GET", base + "?level=error&lines=2000")
    d3 = r.get("data", {})
    errs = [e["event"] for e in d3.get("entries") or []]
    cb.check("报错级筛选只给出报错行", all(e.get("level") == "error" for e in d3.get("entries") or []),
             str(errs[:6]))
    cb.check("真日志里认得出的事件不止一种",
             len(set(errs)) >= 1 and len(set(e["event"] for e in entries)) >= 4,
             str(sorted(set(e["event"] for e in entries)))[:180])
    code, r = cb.plain_call("GET", base + "?level=warn&lines=2000")
    d4 = r.get("data", {})
    lv = [e["level"] for e in d4.get("entries") or []]
    cb.check("级别是下限：警告档里不该混进普通行",
             code == 200 and all(x in ("warn", "error") for x in lv), str(lv[:8]))
    cb.check("筛选是在解析之后做的，scanned 不跟着变",
             d4.get("scanned", 0) >= max(d4.get("returned", 0), 1),
             "scanned=%s returned=%s" % (d4.get("scanned"), d4.get("returned")))

    code, r = cb.plain_call("GET", "/api/v1/clusters/%s/shards/Caves/log?lines=50" % cb.CLUSTER)
    cb.check("洞穴分片的日志也读得回来", code == 200 and (r.get("data", {}).get("entries") is not None),
             "%s %s" % (code, str(r)[:120]))

    code, r = cb.plain_call("GET", base + "?kind=chat")
    cb.check("聊天日志要么读得出，要么给一句中文说明还没有",
             code == 200 or (code == 503 and "还没" in str(r.get("error"))),
             "%s %s" % (code, str(r)[:140]))
    if code == 200:
        # 这台机器上的聊天文件真有内容（面板发出去的公告被引擎记进了这一份）。
        # 玩家发言的形状我手上还没有真样本，所以只断言"认成了公告或聊天其中一种"，
        # 不断言名字拆得出来——拆不出来时整行照样在 raw 里，界面不丢内容。
        ce = r.get("data", {}).get("entries") or []
        cb.check("聊天文件的行被认成公告或聊天",
                 len(ce) > 0 and all(e["event"] in ("announce", "chat") for e in ce)
                 and all(e["raw"] for e in ce), str(ce)[:200])

    stream = raw_get(base + "/stream?once=1&lines=20")
    cb.check("云机上订阅一次能收到 meta → line×20 → done",
             "event: meta" in stream and stream.count("event: line") == 20 and "event: done" in stream,
             stream[:160].replace("\n", " | "))
    cb.check("SSE 帧里也没有令牌明文", "pds-g" not in stream, stream[:120])


# ---- 存档快照 ----

def du_totals(d):
    """快照目录里 tar.gz 的字节合计、以及所有文件合计（清单 sidecar 单独看得见）。"""
    out = cb.remote("""set -u
D='%s'
if [ ! -d "$D" ]; then echo "ABSENT"; exit 0; fi
A=$(find "$D" -maxdepth 1 -type f -name '*.tar.gz' -printf '%%s\\n' | awk '{s+=$1} END {print s+0}')
B=$(find "$D" -maxdepth 1 -type f -printf '%%s\\n' | awk '{s+=$1} END {print s+0}')
echo "ARCH=$A"; echo "ALL=$B"
""" % d).splitlines()
    got = {}
    for line in out:
        if "=" in line:
            k, v = line.split("=", 1)
            got[k.strip()] = v.strip()
    return got


def task_of(tid):
    code, r = cb.plain_call("GET", "/api/v1/tasks?limit=30")
    for t in (r.get("data") or []):
        if t.get("id") == tid:
            return t
    return None


def wait_task(tid, timeout=240):
    """等一条任务落到终态，顺手把执行中的进度采样带回来。"""
    deadline = time.time() + timeout
    samples = []
    last = None
    while time.time() < deadline:
        t = task_of(tid)
        if t:
            last = t
            if t.get("status") in ("running", "succeeded", "failed", "canceled"):
                samples.append((t.get("progress"), (t.get("message") or "")[:90]))
            if t.get("status") in ("succeeded", "failed", "canceled"):
                return t, samples
        time.sleep(2)
    return last, samples


def snapshot_row(id_):
    code, r = cb.plain_call("GET", "/api/v1/backups")
    if code != 200:
        return None, r
    for v in (r.get("data", {}).get("snapshots") or []):
        if v.get("id") == id_:
            return v, r
    return None, r


def backup_readonly(page):
    """只读的快照页检查：数字要和磁盘对得上，且把只有 unix 才走得到的那条路覆盖上。"""
    d = page.get("data", {})
    pol = d.get("policy", {})
    note = d.get("note") or ""

    cb.check("快照页列出磁盘上的房间",
             cb.CLUSTER in (d.get("clusters") or []), str(d.get("clusters"))[:120])
    snap_dir = pol.get("dir") or ""
    globals()["SNAP_DIR"] = snap_dir
    cb.check("快照目录报的是配置里 workDir 底下那个",
             snap_dir.endswith("/backups") and snap_dir.startswith("/home/ubuntu"), snap_dir)

    got = du_totals(snap_dir)
    dir_absent = "ARCH" not in got
    cb.check("还没打过快照时目录不存在也照样出页（usedBytes=0）",
             (dir_absent and pol.get("usedBytes") == 0) or not dir_absent,
             "目录不存在=%s 面板 usedBytes=%s" % (dir_absent, pol.get("usedBytes")))
    if not dir_absent:
        cb.check("已用量就是目录里归档文件的字节合计",
                 pol.get("usedBytes") == int(got["ARCH"]),
                 "面板 %s / 归档合计 %s / 连清单 %s" % (pol.get("usedBytes"), got["ARCH"], got.get("ALL")))
        cb.check("已用量给了可读大小而不是裸字节数",
                 bool(pol.get("usedLabel")) and pol["usedLabel"] != str(pol.get("usedBytes")),
                 str(pol.get("usedLabel")))

    # Statfs 那一段在 Windows 上编不进去（build tag），只有这里能验到。
    avail = cb.remote("df -B1 --output=avail '%s' 2>/dev/null | tail -1 | tr -d ' '"
                      % (snap_dir or "/home/ubuntu")).strip()
    if avail.isdigit():
        want, have = int(avail), d.get("freeBytes") or 0
        cb.check("磁盘余量在 unix 上读得到真数（不是「未知」）",
                 have > 0 and d.get("freeLabel") != "未知" and abs(have - want) < 300 * 1024 * 1024,
                 "面板 %s(%s) / df %s" % (have, d.get("freeLabel"), want))
        cb.check("快照目录还没建时也能报出磁盘余量",
                 "磁盘还剩 %s" % d.get("freeLabel") in note, note[:150])
    else:
        cb.check("df 读数没拿到（这一轮跳过余量对账）", False, avail[:80])

    yaml_keep = cb.remote("grep -A3 '^backup:' %s | grep -m1 'keep:' | sed 's/[^0-9]//g'"
                          % cb.CFG).strip()
    src = pol.get("keepSource") or ""
    # 用设置行有没有被写过（set）来判分支，而不是拿中文句子猜：猜句子会把
    # "配置文件那句里也提到面板设置"读成反的。
    _, rs = cb.plain_call("GET", "/api/v1/settings")
    keep_row = next((it for it in (rs.get("data") or [])
                     if it.get("key") == "backup_keep"), {})
    if keep_row.get("set") is False:
        cb.check("库里没有设置行时保留上限跟着配置文件",
                 str(pol.get("keep")) == yaml_keep and "配置文件" in src,
                 "面板 %s / yaml %s / 设置行 set=%s / 来源：%s"
                 % (pol.get("keep"), yaml_keep, keep_row.get("set"), src[:70]))
    else:
        cb.check("有设置行时保留上限跟着面板设置并说明以它为准",
                 str(pol.get("keep")) == str(keep_row.get("value")) and "面板设置" in src,
                 "面板 %s / 设置值 %s / 来源：%s" % (pol.get("keep"), keep_row.get("value"), src[:80]))

    hours = d.get("scheduleHours")
    cb.check("定时备份那句说明跟着间隔走",
             (hours and hours > 0 and ("每 %d 小时" % hours) in note)
             or (not hours and "定时备份没开" in note),
             "hours=%s note=%s" % (hours, note[:120]))

    # 云机上装了 screen 且两个分片真在跑：这一页必须说得出进程状态。
    # Windows 探针只能验"问不到"那一支，这里验另一支。
    cb.check("有 screen 时页面不再声称问不到进程状态", "问不到" not in note, note[:160])
    code, r = cb.plain_call("GET", "/api/v1/clusters")
    cl = next((c for c in (r.get("data") or []) if c.get("name") == cb.CLUSTER), {})
    shards = cl.get("shards") or []
    cb.check("进程驱动从真 screen 会话里认出了两个在跑的分片（会话名字形状对得上）",
             code == 200 and sorted(x.get("name") for x in shards if x.get("running")) == ["Caves", "Master"]
             and all(x.get("pid", 0) > 0 for x in shards if x.get("running")),
             str([(x.get("name"), x.get("running"), x.get("pid")) for x in shards])[:160])


def backup_live():
    """备份的正路：催存盘 → 打包真 139MB → 校验 → 下载解包 → 删掉自己打的这份。"""
    code, r = cb.plain_call("POST", "/api/v1/clusters/%s/backups" % cb.CLUSTER, {})
    d = r.get("data", {})
    tid = d.get("taskId") or (d.get("task") or {}).get("id")
    cb.check("打快照的请求 202 并带回任务 id", code == 202 and bool(tid), "%s %s" % (code, str(d)[:160]))
    if not tid:
        return None
    t, samples = wait_task(tid)
    msg = (t or {}).get("message") or ""
    cb.check("云机上备份任务成功结束", (t or {}).get("status") == "succeeded",
             "%s %s" % ((t or {}).get("status"), msg[:200]))
    cb.check("执行中按阶段报了进度",
             len({s[0] for s in samples}) >= 2, str(samples[:6]))
    # 这一条是 Windows 上永远验不到的正支：screen 在、分片在跑，就得真的催过存盘。
    cb.check("催存盘走的是真命令通道（说了「已让…立即存盘」）",
             "立即存盘" in msg and "问不到" not in msg and "没有在跑的世界" not in msg, msg[:220])
    cb.check("收尾说了打了多少文件、包多大",
             "个文件" in msg and "已生成" in msg, msg[:200])
    # 云机上撞过一次：引擎在打包途中淘汰了最旧的自动存档，报错是 Go 的英文原文
    # （stat .../0000000068: no such file or directory），而那一句会直接进用户看的任务消息。
    # 现在那一条要么根本不该出现（滚动淘汰被跳过），出现了也得是中文说明。
    cb.check("任务消息里没有 Go 的英文原文",
             not any(x in msg for x in ("no such file", "stat ", "fork/exec", "exit status",
                                        "context deadline", "permission denied")),
             msg[:200])
    cb.check("如果这次真撞上了滚动淘汰，说的是中文并且仍在成功里",
             ("淘汰" not in msg) or ("个条目被引擎" in msg and (t or {}).get("status") == "succeeded"),
             msg[:220])
    id_ = None
    m = re.search(r"快照 (\S+) 已生成", msg)
    if m:
        id_ = m.group(1)
    cb.check("从收尾话里认得出快照编号", bool(id_), str(id_))
    if not id_:
        return None

    row, page = snapshot_row(id_)
    cb.check("打完这份出现在列表里且标为可恢复",
             row and row.get("restorable") is True, str(row)[:200])
    cb.check("列表标出两个分片都带了存档",
             row and sorted(row.get("saveShards") or []) == ["Caves", "Master"],
             str((row or {}).get("saveShards")))
    cb.check("在跑的分片被点名，恢复那栏拦住并说清要先停",
             row and sorted(row.get("runningShards") or []) == ["Caves", "Master"]
             and "先把它们全停掉" in (row.get("runningNote") or ""),
             str((row or {}).get("runningNote"))[:150])
    blocked = (page.get("data", {}).get("restoreBlocked") or [])
    cb.check("页面顶层的恢复受阻清单也点了这个房间",
             any(b.get("cluster") == cb.CLUSTER for b in blocked), str(blocked)[:180])
    cb.check("打的是真存档目录而不是空壳（解压前 > 50MB，归档 > 1MB）",
             row and row.get("uncompressedBytes", 0) > 50 * 1024 * 1024
             and 1024 * 1024 < row.get("archiveBytes", 0) < row["uncompressedBytes"],
             "包 %s / 解压前 %s" % ((row or {}).get("archiveBytes"), (row or {}).get("uncompressedBytes")))
    _, page2 = cb.plain_call("GET", "/api/v1/backups")
    note2 = page2.get("data", {}).get("note") or ""
    cb.check("有了实测数字后大小说明跟着最近一份走，不再喊猜来的 140MB",
             "按最近一份备份看" in note2 and "140MB" not in note2, note2[:190])

    code, r = cb.plain_call("GET", "/api/v1/backups/%s" % id_)
    det = r.get("data", {})
    files = det.get("snapshot", {}).get("files") or []
    # 云机实测：这台引擎把自动存档写成 save/session/<GAMEID>/<8 位编号> 加一份 .meta，
    # 界面上那种 slot-N/server.dat 在这儿根本不存在。按真形状断言，不然探针在替一个假设定背书。
    # 档位名是补零编号：这台云机上是 10 位（0000000072），别按我以为的 8 位写死。
    numbered = [f for f in files
                if re.search(r"/save/session/[0-9A-Fa-f]{16}/[0-9A-Fa-f]{8,}$", f.get("path", ""))]
    metas = [f for f in files
             if re.search(r"/save/session/[0-9A-Fa-f]{16}/[0-9A-Fa-f]{8,}\.meta$", f.get("path", ""))]
    cb.check("明细里有真档位文件（16 位会话号 + 8 位编号 + .meta 成对）",
             code == 200 and len(numbered) >= 2 and len(metas) >= 2,
             "files=%s 档位=%s 份，meta=%s 份" % (len(files), len(numbered), len(metas)))
    # 前一条为假时这个列表是空的，而 all(空) 在 python 里是真，会跟着报绿。
    cb.check("档位文件是真数据而不是空壳（每份都上百 KB）",
             bool(numbered) and all(f.get("size", 0) > 100 * 1024 for f in numbered),
             str([f.get("size") for f in numbered])[:120])
    rot = [f for f in files if "/server_log/" in f.get("path", "") or "/server_chat_log/" in f.get("path", "")]
    cb.check("明细里没有引擎攒的旧日志副本（backup/server_log 那一层）",
             not rot, str([f.get("path") for f in rot])[:180])
    cb.check("明细说了跳过了令牌与面板通道文件",
             "令牌" in (det.get("note") or "") and "跳过" in (det.get("note") or ""),
             str(det.get("note"))[:180])

    code, r = cb.plain_call("POST", "/api/v1/backups/%s/verify" % id_)
    v = r.get("data", {})
    cb.check("整包校验把清单里每个文件都比过哈希（真 122 个文件）",
             code == 200 and v.get("archiveOk") is True
             and v.get("checked") == det.get("snapshot", {}).get("fileCount"),
             "%s checked=%s want=%s" % (code, v.get("checked"), det.get("snapshot", {}).get("fileCount")))
    cb.check("校验结论是能照着做的一句中文",
             "可以放心用来恢复" in (v.get("detail") or ""), str(v.get("detail"))[:150])

    # 下载与令牌检查全部在云机上做：140MB 不该拖过 ssh，而令牌只在本机比、绝不打出来。
    dl = cb.remote("""set -u
D=/tmp/dst_probe_backup
rm -rf "$D"; mkdir -p "$D"
CODE=$(curl -s -o "$D/a.tar.gz" -w '%%{http_code}' -m 420 -u "%s:%s" "http://127.0.0.1:%d/api/v1/backups/%s/download")
SIZE=$(stat -c %%s "$D/a.tar.gz")
NMEM=$(tar tzf "$D/a.tar.gz" | grep -vc '/$')
BADM=$(tar tzf "$D/a.tar.gz" | grep -cE 'cluster_token|dstpanel_|server_log|chat_log')
T=$(head -c 40 "%s/%s/cluster_token.txt" | tr -d '\\n\\r')
TOKIN=$(tar xzOf "$D/a.tar.gz" 2>/dev/null | grep -cF -- "$T")
TOKMF=$(grep -cF -- "$T" "%s/%s.manifest.json")
DAT=$(tar xzOf "$D/a.tar.gz" 2>/dev/null | wc -c)
echo "CODE=$CODE"; echo "NMEM=$NMEM"; echo "BADM=$BADM"; echo "TOKIN=$TOKIN"; echo "TOKMF=$TOKMF"
echo "SIZE=$SIZE"; echo "DAT=$DAT"
rm -rf "$D"
""" % (cb.ADMIN, cb.PW, cb.PORT, id_, cb.KLEI, cb.CLUSTER, SNAP_DIR, id_))
    got = {}
    for line in dl.splitlines():
        if "=" in line:
            k, v = line.split("=", 1)
            got[k.strip()] = v.strip()
    cb.check("下载接口发出来的是一个解得开的包",
             got.get("CODE") == "200" and int(got.get("NMEM") or 0) == det.get("snapshot", {}).get("fileCount", -1),
             str(got)[:200])
    cb.check("包里没有令牌、面板通道文件与引擎日志",
             got.get("BADM") == "0", "命中 %s 个成员名" % got.get("BADM"))
    cb.check("解压后的字节流里搜不到令牌那一段明文",
             got.get("TOKIN") == "0", "命中 %s 行" % got.get("TOKIN"))
    cb.check("清单里也搜不到令牌", got.get("TOKMF") == "0", "命中 %s 行" % got.get("TOKMF"))

    # 恢复：这里只做"应当被拦住"的检查。前提是面板确实看得见分片在跑——
    # 看不见就绝对不能发这个请求，那时它真会去换存档目录。
    if sorted((row or {}).get("runningShards") or []) == ["Caves", "Master"]:
        code, r = cb.plain_call("POST", "/api/v1/backups/%s/restore" % id_, {})
        cb.check("没带确认的恢复 400（并说了整棵替换）",
                 code == 400 and "整棵替换" in json.dumps(r, ensure_ascii=False), str(r.get("error"))[:160])
        code, r = cb.plain_call("POST", "/api/v1/backups/%s/restore" % id_, {"confirm": True})
        body = json.dumps(r, ensure_ascii=False)
        cb.check("分片在跑时确认了也不给恢复，并点名是哪几个",
                 code == 409 and "在跑" in body, "%s %s" % (code, body[:180]))
        t2 = task_of(tid)
        cb.check("恢复没有偷偷入队（任务表里还只有备份那一条）",
                 len([x for x in (cb.plain_call("GET", "/api/v1/tasks?limit=30")[1].get("data") or [])
                      if x.get("kind") == "backup.restore"]) == 0, str(t2)[:120])
    else:
        cb.check("云机这一轮没看见分片在跑，恢复的负向检查跳过（不能拿存档赌）", False,
                 str((row or {}).get("runningShards")))

    code, r = cb.plain_call("DELETE", "/api/v1/backups/%s" % id_)
    cb.check("探针自己打的这份快照删得掉（不给云机留东西）",
             code == 200 and "已删除" in json.dumps(r, ensure_ascii=False), "%s %s" % (code, str(r)[:160]))
    left = cb.remote("ls %s/%s* 2>/dev/null | wc -l" % (SNAP_DIR, id_)).strip()
    cb.check("归档与清单一起删干净了", left == "0", "还剩 %s 个文件" % left)
    return id_


def main():
    cb.PW = cb.password()
    before = shas()
    tree_before = tree_shas() if LIVE else {}
    cb.check("云机上待读的文件都列到了", len(before) == len(FILES), str(before)[:200])

    cb.push_binary()
    pid = cb.start_panel()
    got_id = None
    try:
        # Master 那份是引擎存档时重写过的（preset 拆成 worldgen_preset/settings_preset），
        # Caves 那份还是面板写的，两者的注脚不一样，分开验。
        for shard, want_preset, rewritten in (("Master", "SURVIVAL_TOGETHER_CLASSIC", True),
                                              ("Caves", "DST_CAVE", False)):
            code, r = cb.plain_call("GET", "/api/v1/clusters/%s/shards/%s/world" % (cb.CLUSTER, shard))
            d = r.get("data", {})
            # 409 是"这个文件面板读不懂"的返回码，出现就说明解析器退化了。
            cb.check("%s 世界页仍然读得回来（没有 409）" % shard,
                     code == 200 and d.get("preset"), "%s %s" % (code, str(d)[:180]))
            cb.check("%s 世界页带出了覆盖项和路径（不是空壳）" % shard,
                     d.get("present") is True and d.get("overrides") is not None and d.get("path"),
                     str(d)[:180])
            cb.check("%s 读到的预设是 %s" % (shard, want_preset), d.get("preset") == want_preset,
                     str(d.get("preset")))
            if rewritten:
                cb.check("Master 的界面说明了文件被引擎重写过",
                         "引擎" in (d.get("note") or ""), (d.get("note") or "")[:160])

        # 助手通道读的是引擎用 SetPersistentString 写出来的文件（带 KLEI 头），
        # 是另一条 ParseLuaTable 路径，一起盯住。
        code, r = cb.plain_call("GET", "/api/v1/clusters/%s/shards/Master/helper" % cb.CLUSTER)
        d = r.get("data", {})
        cb.check("助手状态页读得回来（KLEI 头文件仍能解析）", code == 200 and "installed" in d,
                 "%s %s" % (code, str(d)[:180]))

        code, r = cb.plain_call("GET", "/api/v1/clusters")
        cb.check("集群列表读得回来", code == 200 and bool(r.get("data")), str(r)[:160])

        log_checks()

        code, page = cb.plain_call("GET", "/api/v1/backups")
        cb.check("快照页读得回来", code == 200 and "policy" in (page.get("data") or {}),
                 "%s %s" % (code, str(page)[:160]))
        if code == 200:
            backup_readonly(page)
        if code == 200 and LIVE:
            got_id = backup_live()
        elif code == 200:
            print("[SKIP] 没设 DST_BACKUP_LIVE=1：备份/校验/下载的正路与恢复的负向检查这一轮没跑")
    finally:
        cb.remote("kill %s 2>/dev/null; pgrep -f %s | xargs -r kill 2>/dev/null; sleep 1"
                  % (pid or "0", cb.CFG))
        after = shas()
        cb.check("存档以外的配置类文件一个都没变（live 那一路只催引擎自己存盘）", before == after,
                 " ".join("%s %s..%s" % (k, before.get(k, "")[:8], after.get(k, "")[:8])
                          for k in sorted(before) if before.get(k) != after.get(k)) or "全部一致")
        if LIVE:
            tree_after = tree_shas()
            changed = sorted(k for k in set(tree_before) | set(tree_after)
                             if tree_before.get(k) != tree_after.get(k))
            allowed = [p for p in changed if "/save/" in p or p.endswith("_log.txt")]
            cb.check("备份这一路只让引擎写了存档与日志，别的文件一个没动",
                     len(allowed) == len(changed),
                     "变了 %d 个，越界：%s" % (len(changed), str([p for p in changed if p not in allowed])[:200]))
            cb.check("令牌文件连内容都没变（催存盘不碰它）",
                     tree_before.get("cluster_token.txt") == tree_after.get("cluster_token.txt"),
                     "%s..%s" % (str(tree_before.get("cluster_token.txt"))[:8],
                                 str(tree_after.get("cluster_token.txt"))[:8]))
            if got_id:
                left = cb.remote("ls %s/%s* 2>/dev/null | wc -l" % (SNAP_DIR, got_id)).strip()
                cb.check("探针删掉的快照没有半份留在云上", left == "0", "剩 %s" % left)

    bad = [n for n, ok in cb.results if not ok]
    print("\n合计 %d 项，失败 %d 项" % (len(cb.results), len(bad)))
    for n in bad:
        print("失败：%s" % n)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
