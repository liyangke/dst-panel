#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""看 /api/v1/rooms 的中文取值。只打印，不改任何东西。

单独写一个文件而不是在命令行里拼：Bash 里带中文的命令会直接 exit 127，
而输出重定向又会被 Windows 控制台码页转成乱码，两个坑都是"看不出对错"的坑。
"""
import io
import json
import os
import sys
import urllib.request
import base64

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "rooms_cn.txt")
BASE = "http://127.0.0.1:8963"


def main():
    req = urllib.request.Request(BASE + "/api/v1/rooms")
    tok = base64.b64encode(b"admin:admin").decode()
    req.add_header("Authorization", "Basic " + tok)
    with urllib.request.urlopen(req, timeout=20) as r:
        body = json.loads(r.read().decode("utf-8"))
    lines = []
    d = body["data"]
    lines.append("汇总：房间 %s 分片 %s 在跑 %s 在线 %s 要注意 %s" % (
        d["clusters"], d["rooms"], d["running"], d["online"], d["attention"]))
    for it in d["items"]:
        lines.append("%s / %s ｜ 房间名 %s ｜ %s ｜ %s ｜ 端口 %s ｜ 世界 %s ｜ 在线 %s ｜ 模组 %s/%s ｜ 预设 %s ｜ %s" % (
            it["cluster"], it["shard"], it["roomName"], it["runningLabel"],
            it.get("terrainLabel", ""), it.get("port", "-"),
            "%s %s %s" % (it.get("day", ""), it.get("seasonLabel", ""), it.get("phaseLabel", "")),
            it.get("online", ""), it.get("modsEnabled", "-"), it.get("modsTotal", "-"),
            it.get("presetLabel", ""), it.get("accessLabel", "")))
        if it.get("helperNote"):
            lines.append("    助手：%s" % it["helperNote"])
        if it.get("gameModeLabel") or it.get("intentionLabel"):
            lines.append("    模式：%s / %s，上限 %s，PVP %s" % (
                it.get("gameModeLabel", ""), it.get("intentionLabel", ""),
                it.get("maxPlayers"), it.get("pvp")))
        if it.get("masterAddr"):
            lines.append("    连主机：%s" % it["masterAddr"])
    io.open(OUT, "w", encoding="utf-8").write("\n".join(lines) + "\n")
    sys.stdout.write("written %s\n" % OUT)


if __name__ == "__main__":
    main()
