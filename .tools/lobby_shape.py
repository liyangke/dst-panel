#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""抽样分析公网房间列表的真实形状，用来定界面那一列的预算。

为什么单独一个脚本：这一列的数据是别人手填的，长度和字符完全不可控（名字里塞了
私有区字形、标签填了九十多个词）。不量一遍就按"大概十几个字"排版，线上一定崩。
"""
import collections
import json
import sys
import urllib.request

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

BASE = "http://127.0.0.1:8963/api/v1/lobby/rooms"
PARAMS = [
    "size=100&page=1", "size=100&page=2", "size=100&page=3",
    "size=100&sort=capacity", "size=100&sort=free&page=5",
    "size=100&access=open", "size=100&players=none&sort=capacity",
    "size=100&intent=survival", "size=100&q=steam",
]


def get(q):
    req = urllib.request.Request(BASE + "?" + q,
                                 headers={"Authorization": "Basic " + b64()})
    with urllib.request.urlopen(req, timeout=90) as r:
        return json.loads(r.read().decode("utf-8"))["data"]


def b64():
    import base64
    return base64.b64encode(b"admin:admin").decode()


def invisible(s):
    out = []
    for ch in s:
        o = ord(ch)
        if o < 0x20 or o == 0x7f or 0x80 <= o <= 0x9f:
            out.append("ctrl:%04x" % o)
        elif 0xE000 <= o <= 0xF8FF or 0x100000 <= o <= 0x10FFFF:
            out.append("pua:%04x" % o)
        elif 0xD800 <= o <= 0xDFFF:
            out.append("surrogate:%04x" % o)
    return out


rooms = []
for p in PARAMS:
    rooms.append(get(p)["rooms"])
allr = [x for g in rooms for x in g]
uniq = {(x["addr"], x["port"], x["name"]): x for x in allr}
print("样本 %d 条，去重后 %d 条" % (len(allr), len(uniq)))
rows = list(uniq.values())

print("名字最长 %d 字" % max(len(x["name"]) for x in rows))
tags = [len(x["tags"]) for x in rows]
print("标签最多 %d 个，中位 %d 个，空标签 %d 条" % (max(tags), sorted(tags)[len(tags) // 2], tags.count(0)))
print("单个标签最长 %d 字" % max((len(t) for x in rows for t in x["tags"]), default=0))

bad = [x for x in rows if invisible(x["name"])]
print("名字里有控制符或私有区字形的 %d 条" % len(bad))
for x in bad[:5]:
    print("   ", repr(x["name"][:48]), invisible(x["name"])[:5])
badtag = [t for x in rows for t in x["tags"] if invisible(t)]
print("标签里同样情况的 %d 个，例如 %s" % (len(badtag), [repr(t[:20]) for t in badtag[:3]]))

print("季节分布", collections.Counter(x["seasonLabel"] for x in rows))
print("玩法分布", collections.Counter(x["intentLabel"] for x in rows))
print("上限分布", collections.Counter(x["capacity"] for x in rows).most_common(10))
print("在线分布", collections.Counter(x["players"] for x in rows).most_common(6))
print("端口分布", collections.Counter(x["port"] for x in rows).most_common(8))
print("门槛", sum(1 for x in rows if not x["fullAccess"]), "暂停", sum(1 for x in rows if x["paused"]),
      "满员", sum(1 for x in rows if x["free"] == 0), "官方服", sum(1 for x in rows if x["kleiOfficial"]))
print("要密码", sum(1 for x in rows if x["password"]), "只收战队", sum(1 for x in rows if x["clanOnly"]),
      "仅局域网", sum(1 for x in rows if x["lanOnly"]), "不收新人", sum(1 for x in rows if not x["allowNew"]))
print("accessLabel 种类", collections.Counter(x["accessLabel"] for x in rows))
