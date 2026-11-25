#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""#76 的前提探针：不给关键词时，工坊接口能不能一页一页翻。

用户那句是"我不知道要什么模组，只想一页一页翻，一个个看"，而现在面板的在线那一路
只在给了关键词（或选了分类）时才打接口——没有关键词就只翻本地缓存，缓存里只有"见过的"。
要补这条，先得钉住三件事，猜一条都会做出一个假分页：

1. 空关键词（不带 search_text）到底回不回条目，total 是多少；
2. page=2 是不是真的换一批（Steam 会不会像忽略标签参数那样把 page 也吃掉）；
3. 排序口径是什么（第一次翻和第二次翻必须是同一个序，否则"下一页"会重复老面孔）。

key 只从环境变量 DST_STEAM_KEY 读，不进 argv、不进日志、不进任何输出：
和 steam_probe.py 同一套做法——脚本喂给远端 bash -s，远端写进 0600 文件后用 curl 的 key@文件 传。

用法：DST_STEAM_KEY=<key> python .tools/workshop_browse_probe.py
"""
import json
import os
import re
import subprocess
import sys

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HOST = "dst-cloud"
API = "https://api.steampowered.com"
QUERY_FILES = API + "/IPublishedFileService/QueryFiles/v1/"
KEY_RE = re.compile(r"^[0-9A-Fa-f]{16,64}$")

HEAD = """set -u
umask 077
K=/tmp/.dstbrowse_key
printf '%s' '__KEY__' > "$K"
run() { curl -s -m 25 -w '\\nHTTP=%{http_code}\\n' --get "$1" --data-urlencode "key@$K" "${@:2}"; }
"""
TAIL = 'rm -f "$K"\n'

COMMON = ["appid=322330", "numperpage=10", "return_tags=true", "return_children=true",
          "return_vote_data=true", "language=6"]


def load_key():
    key = os.environ.get("DST_STEAM_KEY", "").strip()
    if not key:
        sys.exit("没有 DST_STEAM_KEY 环境变量，探针不猜 key")
    if not KEY_RE.match(key):
        sys.exit("DST_STEAM_KEY 不是十六进制串，像是被改坏了；拒绝把它拼进远端脚本")
    return key


def call(key, params):
    script = HEAD.replace("__KEY__", key) + 'run "%s" %s\n' % (
        QUERY_FILES, " ".join('--data-urlencode "%s"' % p for p in params)) + TAIL
    p = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", HOST, "bash", "-s"],
                       input=script.encode("utf-8"), capture_output=True, timeout=150)
    out = (p.stdout or b"").decode("utf-8", "replace")
    body, _, code = out.rpartition("HTTP=")
    return body.strip(), code.strip(), (p.stderr or b"").decode("utf-8", "replace").strip()


def query(params):
    """同名参数只留最后一个。

    上一版在这里栽过：COMMON 里带着 page=1，变体又追加一个 page=2，
    curl 拼出 `page=1&page=2`，Steam 取第一个，于是"page 被吃掉"这个结论是探针自己造的，
    不是接口给的。numperpage 同理。
    """
    keep = {}
    order = []
    for p in params:
        k = p.split("=", 1)[0]
        if k not in keep:
            order.append(k)
        keep[k] = p
    return [keep[k] for k in order]


def show(tag, key, extra):
    body, code, err = call(key, query(COMMON + ["page=1"] + extra))
    try:
        j = json.loads(body)
    except Exception:
        print("  %-26s HTTP=%s 不是 JSON：%s" % (tag, code, body[:160]))
        if err:
            print("     stderr=%s" % err[:120])
        return None
    resp = j.get("response") or {}
    items = resp.get("publishedfiledetails") or []
    ids = [str(x.get("publishedfileid")) for x in items]
    subs = [x.get("subscriptions") for x in items[:4]]
    names = [(x.get("title") or "")[:16] for x in items[:3]]
    print("  %-26s HTTP=%s total=%s 条数=%d 前几订阅=%s" % (
        tag, code, resp.get("total"), len(items), subs))
    print("     前 3 个：%s ｜ ID %s" % (names, ids[:3]))
    return {"ids": ids, "total": resp.get("total"), "n": len(items)}


def main():
    key = load_key()

    print("===== 1 空关键词，第 1 页 vs 第 2 页 vs 第 3 页（这条需求的全部前提）")
    pages = []
    for n in (1, 2, 3):
        pages.append(show("空词 page=%d" % n, key, ["page=%d" % n]))
    if pages[0] and pages[1]:
        for i, p in enumerate(pages[1:], start=2):
            if not p:
                continue
            same = set(pages[0]["ids"]) & set(p["ids"])
            print("     第 1 页与第 %d 页重叠 %d/%d 条 → %s" % (
                i, len(same), len(p["ids"]),
                "page 生效" if not same else "page 被吃掉，这条路走不通"))

    print("\n===== 2 空关键词 + numperpage 变体（一次能取多少，决定翻页粒度）")
    for n in (20, 100):
        show("空词 numperpage=%d" % n, key, ["numperpage=%d" % n])

    print("\n===== 3 排序稳不稳：同一页连打两次是不是同一批（翻页途中序变了会重复老面孔）")
    a = show("stack 第 1 页 第一次", key, ["search_text=stack"])
    b = show("stack 第 1 页 第二次", key, ["search_text=stack"])
    if a and b:
        print("     两次差异 ID 数：%d" % len(set(a["ids"]) ^ set(b["ids"])))

    print("\n===== 4 带关键词翻页 + sort_by 认不认")
    c = show("stack page=2", key, ["search_text=stack", "page=2"])
    d = show("stack page=2 +sort_by", key, ["search_text=stack", "page=2", "sort_by=total_decreases"])
    if c and d:
        print("     带 sort_by 与不带的差异 ID 数：%d" % len(set(c["ids"]) ^ set(d["ids"])))

    print("\n===== 5 空词时 offset/cursor 这类参数有没有第二种分页口径")
    show("空词 +offset=10", key, ["offset=10"])
    show("空词 +start=10", key, ["start=10"])
    return 0


if __name__ == "__main__":
    sys.exit(main())
