#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""把内嵌的 ES 模块复制到临时 .mjs 再让 node --check 解一遍。

为什么需要它：免构建的前端没有打包步骤，一个引号写错、一个模板字符串里的双引号没换成
中文引号，Go 那边照样编译通过（文件只是被 embed 进去），要等浏览器打开才炸。
node --check 只解语法不执行，所以 window/Vue 全局这些它在命令行里拿到的东西不影响判断。
"""
import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
JS = os.path.join(ROOT, "internal", "webui", "assets", "js")

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

targets = sys.argv[1:] or []
files = []
for base, _dirs, names in os.walk(JS):
    for n in names:
        if n.endswith(".js"):
            files.append(os.path.join(base, n))
files.sort()
if targets:
    files = [f for f in files if any(t in f.replace("\\", "/") for t in targets)]

bad = 0
tmp = tempfile.mkdtemp(prefix="jscheck")
for f in files:
    dst = os.path.join(tmp, os.path.basename(f)[:-3] + ".mjs")
    shutil.copyfile(f, dst)
    p = subprocess.run(["node", "--check", dst], capture_output=True, text=True,
                       encoding="utf-8", errors="replace")
    rel = os.path.relpath(f, ROOT).replace("\\", "/")
    if p.returncode != 0:
        bad += 1
        print("语法不过：%s" % rel)
        print((p.stderr or p.stdout).strip()[:900])
print("检查了 %d 个文件，%d 个不过" % (len(files), bad))
sys.exit(1 if bad else 0)
