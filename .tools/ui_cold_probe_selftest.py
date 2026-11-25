#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""自证：ui_cold_start 里那两条"看不见就等于没问题"的探针真的会算。

为什么要单独跑：手机那一轮 12 页全绿、溢出全是 null，这句话的可信度全押在探针真的
会算这件事上。探针写错（比如选择器拼错、异常被吞掉）时报出来的也是 null，
和"确实没有溢出"在输出上一模一样。这里手动撑一个 900px 宽的元素进去，看它抓不抓得到。

跑法：python .tools/ui_cold_probe_selftest.py（自己拉起 8965 那份冷配置，跑完关掉；
它要的是"三个空目录"那一套现场，所以会先把 .tmp_boot/cold 重建一遍，不影响 ui_dev 的库）
"""
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ui_cold_start as C
from cdp import Browser


def main():
    # 探针里的宽度基准读的是 ui_cold_start.VIEW，这里既然按手机一档来验，就得把它也设成
    # 手机一档，否则比的是 1440，永远"没有溢出"。
    C.VIEW = (390, 844)
    C.stop_stale()
    # 自己造现场：这一份要能单独跑，不依赖上一轮冷启动留下的目录和配置。
    C.fresh_dirs()
    C.write_cfg()
    proc, log = C.start()
    n = [0]

    def url(path):
        n[0] += 1
        return "%s/?selftest=%d#%s" % (C.BASE, n[0], path)

    b = Browser(width=390, height=844)
    try:
        b.goto(url("/"))
        b.wait("!!document.querySelector('.login-form')")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:%s,pass:%s}))"
             % (json.dumps(C.USER), json.dumps(C.PW)))
        b.goto(url("/"))
        b.wait("!!document.querySelector('.shell')")
        # PAGE 自己就是一个立刻执行的表达式，这里只取它返回值里的 overflow，
        # 不去拼字符串改它的结尾（拼错一次就静默返回 None，和"确实没有溢出"长得一样）。
        print("干净状态：", (b.js(C.page_js()) or {}).get("overflow"))
        b.js("(() => { const d = document.createElement('div');"
             "d.id='wide-probe';d.style.cssText='width:900px;height:8px';"
             "document.querySelector('main').appendChild(d); })()")
        got = (b.js(C.page_js()) or {}).get("overflow")
        # 没抓到先分清是"探针算错了"还是"这个宽元素根本没进文档/被容器包住了"：
        # 直接把宽高打出来，不猜。
        print("现场：", json.dumps(b.js("""(() => ({
          has: !!document.getElementById('wide-probe'),
          right: Math.round((document.getElementById('wide-probe') || {}).getBoundingClientRect
            ? document.getElementById('wide-probe').getBoundingClientRect().right : -1),
          docSW: document.documentElement.scrollWidth,
          bodySW: document.body.scrollWidth,
          seSW: (document.scrollingElement || {}).scrollWidth,
          cw: document.documentElement.clientWidth, iw: window.innerWidth,
          overflowX: getComputedStyle(document.documentElement).overflowX + '/'
                     + getComputedStyle(document.body).overflowX,
        }))()"""), ensure_ascii=False))
        print("撑出 900px 以后：", json.dumps(got, ensure_ascii=False))
        ok = bool(got) and got.get("sw", 0) > got.get("vw", 0)
        print("探针抓到了吗：%s" % ("抓到" if ok else "没抓到 —— 这条检查是摆设"))

        # 页签那一层同样要自证：sweep() 拿不到页签就返回 []，报告里"这一页没有页签"
        # 和"选择器写错了"长得一模一样，而冷启动里最藏得住 bug 的正是第二、三个页签。
        # 无页签那一页用「面板设置」：#129 之前用的是「所有房间」，那一页已经整页下掉了。
        b.goto(url("/mods"))
        b.wait("!!document.querySelector('.shell')")
        tabs = b.js(C.TABS_JS) or []
        b.goto(url("/settings"))
        b.wait("!!document.querySelector('.shell')")
        no_tabs = b.js(C.TABS_JS) or []
        print("模组页抓到 %d 个页签 %s；面板设置页抓到 %d 个（应为 0）"
              % (len(tabs), tabs, len(no_tabs)))
        ok = ok and len(tabs) >= 2 and not no_tabs
        return 0 if ok else 1
    finally:
        b.close()
        proc.terminate()
        try:
            proc.wait(timeout=8)
        except Exception:
            proc.kill()
        log.close()


if __name__ == "__main__":
    sys.exit(main())
