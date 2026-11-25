#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""把界面上某一张卡**整块**裁成图（不是视口截图）。

为什么不用视口截图：`Page.captureScreenshot` 默认只拍视口，1440 那一档长卡的下半截
正好落在折叠线以外，看图会以为下面什么都没有（#122 那次就是这么差点把"泳道没画"当成缺陷）。
这里先量卡片在文档里的坐标，再把页面滚回 (0,0)（顶部标题条是吸顶的，滚动后裁会盖住裁片上沿），
然后用 captureBeyondViewport + clip 按坐标裁整块。

跑法（先起本机实例并按需 --reseed）：
    python .tools/card_shots.py                       # 五档 × 两窗 + 深色两档，默认拍总览那张趋势卡
    CARD_SEL='.card.ov-behaviour' python .tools/card_shots.py
    CARD_SEL='title:玩家在线时间' HASH='#/players' python .tools/card_shots.py   # 搬到玩家管理页那张
    CDP_BASE=http://119.29.239.62:8942 python .tools/card_shots.py   # 公网同一张卡
    OUT=pub python .tools/card_shots.py               # 换输出子目录，别把上一批同名覆盖掉

CARD_SEL 有两种写法：CSS 选择器，或 title:卡头那几个字（#125 之后新落点上的卡没有专属类名，
按名字认比给它加一个只为截图用的类名诚实——那个类没有样式，静态审计会问它是干什么的）。
"""
import base64
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

from cdp import Browser  # noqa: E402
from overview105_check import login, VIEWS  # noqa: E402

CARD = os.environ.get("CARD_SEL", ".card.ov-trend")
# 先落到哪一页再拍：登录之后页面默认停在总览，拍别的页上的卡要自己走过去。
HASH = os.environ.get("HASH", "#/")
OUT = os.path.join(HERE, "tmp", "card_shots", os.environ.get("OUT", ""))
# 卡头那排时间窗按钮上的字样。两页都有窗控件（总览那一枚管这一列两张图，玩家页那一枚管泳道），
# 所以选择器两种都要认，否则下面"没点到"那一句会明明点中了却报出来。
WINDOWS = [("24 小时", "d24"), ("7 天", "d7")]
DARK_TAGS = os.environ.get("DARK_TAGS", "wide,phone").split(",")

# 定位有两种写法：CSS 选择器，或 title:<卡头那六个字>。
# title: 这种是给"没有专属类名的卡"用的——#125 搬过去那张卡只有 section.card，
# 为截图给它加一个不带样式的类名，静态审计就会问这个类管什么，而它什么都不管。
FIND = r"""(() => {
  const sel = %s;
  const e = sel.lastIndexOf('title:', 0) === 0
    ? (() => {
        const h = Array.from(document.querySelectorAll('.card-head h3'))
          .find((x) => x.textContent.trim() === sel.slice(6));
        return h ? h.closest('section.card') : null;
      })()
    : document.querySelector(sel);
  if (!e) return null;
  const r = e.getBoundingClientRect();
  return {x: r.left + scrollX, y: r.top + scrollY, w: r.width, h: r.height};
})()""" % json.dumps(CARD)


def shoot_card(b, png):
    rect = b.js(FIND)
    if not rect:
        print("  卡片没找到（%s），跳过 %s" % (CARD, os.path.basename(png)))
        return None
    b.js("window.scrollTo(0, 0)")
    time.sleep(0.25)
    d = b.ws.call("Page.captureScreenshot", {
        "format": "png", "captureBeyondViewport": True,
        "clip": {"x": rect["x"], "y": rect["y"], "width": rect["w"],
                 "height": rect["h"], "scale": 1}})
    os.makedirs(os.path.dirname(png), exist_ok=True)
    with open(png, "wb") as f:
        f.write(base64.b64decode(d["data"]))
    print("  写了 %s（卡 %dx%d）" % (os.path.basename(png), round(rect["w"]), round(rect["h"])))
    return rect


def pick_window(b, text):
    hit = b.js("""((want) => {
      const bs = Array.from(document.querySelectorAll('.ov-insight .seg button, .tl-when .seg button'));
      const t = bs.find((x) => x.textContent.indexOf(want) >= 0);
      if (t) t.click();
      return !!t;
    })("%s")""" % text)
    time.sleep(1.2)
    return hit


def toggle_theme(b):
    hit = b.js("""(() => {
      const t = document.querySelector('[aria-label^="切到深色"], [aria-label^="切到浅色"]');
      if (t) t.click();
      return t ? t.getAttribute('aria-label') : null;
    })()""")
    time.sleep(1.2)
    return hit, b.js("document.documentElement.dataset.theme")


def open_page(b, tag):
    login(b, tag)
    if HASH and HASH != "#/":
        b.js('location.hash = "%s"' % HASH)
        # 泳道那张卡在玩家页要自己发一次问数，等它答完再拍，不然拍到的是那两行骨架。
        time.sleep(2.2)


def main():
    for w, h, tag in VIEWS:
        b = Browser(w, h)
        open_page(b, tag)
        theme0 = b.js("document.documentElement.dataset.theme")
        for win, wtag in WINDOWS:
            if not pick_window(b, win):
                print("  没点到「近 %s」这一枚，只拍当前窗" % win)
            shoot_card(b, os.path.join(OUT, "%s_%s.png" % (tag, wtag)))
        b.close()
        if theme0 != "light":
            print("  提示：进页面时主题就是 %s（profile 里存的），这批图按它拍的" % theme0)
    # 深色单量几档（默认 1440 与 390），拍完把主题切回进入时的样子
    for w, h, tag in [(x, y, z) for x, y, z in VIEWS if z in DARK_TAGS]:
        b = Browser(w, h)
        open_page(b, tag)
        theme0 = b.js("document.documentElement.dataset.theme")
        got, now = toggle_theme(b)
        print("  %s 深色档：按钮「%s」→ data-theme=%s（进页面时 %s）" % (tag, got, now, theme0))
        pick_window(b, WINDOWS[0][0])
        shoot_card(b, os.path.join(OUT, "%s_%s_dark.png" % (tag, WINDOWS[0][1])))
        if now != "dark":
            got, now = toggle_theme(b)
            print("  %s 补切一次 → %s" % (tag, now))
            shoot_card(b, os.path.join(OUT, "%s_%s_dark.png" % (tag, WINDOWS[0][1])))
        if theme0 == "light" and b.js("document.documentElement.dataset.theme") != "light":
            toggle_theme(b)
        print("  %s 收尾 data-theme = %s" % (tag, b.js("document.documentElement.dataset.theme")))
        b.close()
    print("截图目录：%s" % OUT)


if __name__ == "__main__":
    main()
