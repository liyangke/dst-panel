#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""把「照这间开服」那条验收（.tools/borrow61_check.py）打到云服务器上再跑一遍。

换装之后必须验这一条，因为成品是 go:embed 进二进制的前端资源：
本地 ui_dev 看着对，不代表装到 /data/dst/panel 那份里也是新的。

这里读的是公网链路下云服务器取回的 Klei 快照（面板那侧去拉，这台机器不出公网也行）。
全程只读：按的都是预填、导航与「不要这份预填」，最后那枚建房间的按钮一次都没碰。
"""
import os
import sys

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HERE = os.path.dirname(os.path.abspath(__file__))
TOOLS = os.path.dirname(HERE)
sys.path.insert(0, TOOLS)
import borrow61_check as bc  # noqa: E402

bc.BASE = os.environ.get("P61_BASE", "http://119.29.239.62:8942")
bc.SHOTS = os.path.join(HERE, "public61")
os.makedirs(bc.SHOTS, exist_ok=True)
print("目标 = %s（截图 -> %s）\n" % (bc.BASE, bc.SHOTS))
sys.exit(bc.main())
