#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""数一遍文档里那几个"规模"数字，改 README 和学习指南前先来一次。"""
import io
import os


def walk_lines(base, exts, skip=("vendor", ".retired")):
    files, total = 0, 0
    for dp, dirs, fs in os.walk(base):
        path = dp.replace("\\", "/")
        if any(s in path for s in skip):
            continue
        for f in fs:
            if f.endswith(exts):
                files += 1
                with io.open(os.path.join(dp, f), encoding="utf-8", errors="replace") as fh:
                    total += sum(1 for _ in fh)
    return files, total


js = walk_lines("internal/webui/assets/js", (".js",))
css = walk_lines("internal/webui/assets", (".css",))
go_test = 0
go_src = [0, 0]
for base in ("internal", "cmd"):
    for dp, dirs, fs in os.walk(base):
        path = dp.replace("\\", "/")
        if "vendor" in path:
            continue
        for f in fs:
            if not f.endswith(".go"):
                continue
            p = os.path.join(dp, f)
            text = io.open(p, encoding="utf-8", errors="replace").read()
            go_test += text.count("\nfunc Test")
            if f.endswith("_test.go"):
                continue
            go_src[0] += 1
            go_src[1] += text.count("\n") + 1
print("JS  文件 %d 个，%d 行" % js)
print("CSS 文件 %d 个，%d 行" % css)
print("Go  非测试 %d 个文件，约 %.2f 万行" % (go_src[0], go_src[1] / 10000.0))
print("Go  测试函数 %d 个" % go_test)
