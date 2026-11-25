# -*- coding: utf-8 -*-
"""量"全屏看地图"这一档：点按钮前后画布真的变大了没有、Esc 能不能退回来。

不靠眼睛看截图判断：直接量 .map-canvas-wrap 的 CSS 尺寸、算占视口的比例，
并确认全屏那一档下画布仍然是正方（aspect-ratio 生效，不会被卡片拉扁）。
"""
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tmp", "shots")

MEASURE = r"""(() => {
  const w = document.querySelector('.map-canvas-wrap');
  if (!w) return { error: '没有画布容器' };
  const r = w.getBoundingClientRect();
  const page = document.querySelector('.map-page');
  const cs = getComputedStyle(page);
  // 浮层是被"压在图上"的，一旦画布高度算大了，最下面那行就会被顶到视口外面去，
  // 而整页又能滚——眼睛看截图很容易漏掉，所以这里逐个量。
  const hud = {}, clipped = [];
  ['map-readout', 'map-key', 'map-filter', 'map-legend'].forEach((cls) => {
    const el = document.querySelector('.' + cls);
    if (!el) return;
    const q = el.getBoundingClientRect();
    if (!q.width && !q.height) return;
    hud[cls] = [Math.round(q.top), Math.round(q.bottom), Math.round(q.right)];
    if (q.bottom > innerHeight + 1 || q.right > innerWidth + 1 || q.top < -1) clipped.push(cls);
  });
  return {
    vw: innerWidth, vh: innerHeight,
    wrapW: Math.round(r.width), wrapH: Math.round(r.height),
    top: Math.round(r.top),
    square: Math.abs(r.width - r.height) <= 2,
    fillPct: Math.round(r.width * r.height / (innerWidth * innerHeight) * 100),
    fullClass: page.classList.contains('map-full'),
    position: cs.position,
    bodyOverflow: getComputedStyle(document.body).overflow,
    hud, clipped,
    fits: page.scrollHeight <= page.clientHeight + 2,
    btns: [...document.querySelectorAll('.map-tools .btn')].map(
      (x) => (x.getAttribute('aria-label') || x.textContent.trim())).join('|'),
  };
})()"""


def main():
    os.makedirs(OUT, exist_ok=True)
    b = Browser(1440, 900)
    try:
        b.goto(BASE + "/#/")
        b.js("sessionStorage.removeItem('dstpanel.creds')")
        b.reload()
        if not b.wait("!!document.querySelector('.login-form')"):
            raise SystemExit("登录层没出来")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("带着凭据重进还是停在登录层")
        b.js("location.hash = '#/map'")
        if not b.wait("!!document.querySelector('.map-canvas') && !!document.querySelector('.map-cover')"):
            raise SystemExit("地图一直没就绪")
        time.sleep(1.0)

        before = b.js(MEASURE)
        print("普通档：", json.dumps(before, ensure_ascii=False))
        b.shoot(os.path.join(OUT, "full_before.png"))

        ok = b.click("'.map-tools .btn'",
                     "(x) => (x.getAttribute('aria-label')||'').indexOf('全屏') >= 0")
        if not ok:
            raise SystemExit("找不到全屏按钮（aria-label 里没有「全屏」）")
        time.sleep(0.8)
        full = b.js(MEASURE)
        print("全屏档：", json.dumps(full, ensure_ascii=False))
        b.shoot(os.path.join(OUT, "full_on.png"))

        # 全屏里缩放和切世界还得能用
        b.click("'.map-tools .btn'", "(x) => x.textContent.trim() === '＋'")
        time.sleep(0.4)
        zoomed = b.js(MEASURE)

        b.js("document.dispatchEvent(new KeyboardEvent('keydown', {key:'Escape', bubbles:true}))")
        time.sleep(0.8)
        after = b.js(MEASURE)
        print("Esc 后：", json.dumps(after, ensure_ascii=False))
        b.shoot(os.path.join(OUT, "full_off.png"))

        checks = [
            ("按钮在工具条里", "全屏" in (before.get("btns") or "")),
            ("全屏后浮层生效", full.get("position") == "fixed" and full.get("fullClass") is True),
            ("画布明显变大", full.get("wrapW", 0) > before.get("wrapW", 0) * 1.15),
            ("全屏仍是正方", full.get("square") is True),
            ("滚动锁掉", full.get("bodyOverflow") == "hidden"),
            ("全屏里缩放仍可用", zoomed.get("fullClass") is True),
            ("Esc 退得回来", after.get("fullClass") is False and after.get("bodyOverflow") != "hidden"),
            ("退回后尺寸复原", abs(after.get("wrapW", 0) - before.get("wrapW", 0)) <= 3),
            ("读数/图例没被视口裁掉", not full.get("clipped")),
            # 只要求"图 + 压在图上的浮层"这一叠落在视口里。右侧那两张卡片（这张图里有什么、
            # 玩家）本来就可能比屏幕高，整页能滚是设计内的，不该算失败。
            ("画布完整落在视口内",
             full.get("wrapH", 0) + full.get("top", 0) <= full.get("vh", 0) + 2),
        ]
        for name, verdict in checks:
            print(("  OK   " if verdict else "  FAIL ") + name)
        print("占视口：普通 %s%% → 全屏 %s%%" % (before.get("fillPct"), full.get("fillPct")))
        print("全屏浮层 [上,下,右]：", json.dumps(full.get("hud"), ensure_ascii=False),
              "｜被裁掉：", full.get("clipped") or "无")
        if not all(v for _, v in checks):
            sys.exit(1)

        # 手机上再来一遍：底栏(50) 会被浮层盖住，画布不能超出视口宽
        b.set_viewport(390, 844)
        time.sleep(0.6)
        m0 = b.js(MEASURE)
        b.click("'.map-tools .btn'", "(x) => (x.getAttribute('aria-label')||'').indexOf('全屏') >= 0")
        time.sleep(0.8)
        m1 = b.js(MEASURE)
        b.shoot(os.path.join(OUT, "full_390.png"))
        print("390 视口：普通 %s×%s → 全屏 %s×%s（溢出=%s）｜浮层 %s ｜被裁掉 %s ｜要滚 %s" % (
            m0.get("wrapW"), m0.get("wrapH"), m1.get("wrapW"), m1.get("wrapH"),
            (m1.get("wrapW") or 0) > 390,
            json.dumps(m1.get("hud"), ensure_ascii=False),
            m1.get("clipped") or "无",
            "是" if not m1.get("fits") else "否"))
        b.js("document.dispatchEvent(new KeyboardEvent('keydown', {key:'Escape', bubbles:true}))")
    finally:
        b.close()


if __name__ == "__main__":
    main()
