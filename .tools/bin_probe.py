#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""在成品二进制里找内嵌 Lua 的指纹，确认 embed 真的进包了。"""
import sys

MARKERS = [b"collect_status", b"panel_bridge", b"GetPersistentString", b"poll_interval"]
# 基线：这些必然在二进制里，用来判断"搜索方法本身是不是失效了"。
BASELINE = [b"dstpanel", b"DSTPanelHelper", b"runtime."]

def check(path):
    with open(path, "rb") as f:
        blob = f.read()
    print("%s  %d bytes" % (path, len(blob)))
    for m in BASELINE:
        print("   基线 %-18s %s" % (m.decode(), blob.count(m)))
    for m in MARKERS:
        print("   %-22s %s" % (m.decode(), "HIT x%d" % blob.count(m) if blob.count(m) else "MISS"))

for p in sys.argv[1:]:
    check(p)
