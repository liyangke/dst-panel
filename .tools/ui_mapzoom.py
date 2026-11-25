# -*- coding: utf-8 -*-
"""地图放大后到底有没有"实质性的变化"——用画布像素量，不靠眼睛。

起因是一句很具体的抱怨："将地图进行放大后，地图的内容其实没有实质性的变化，
甚至放大后出现了马赛克一样的东西，只有纯色小方块了。"

这句话可以证伪。一格如果是纯色方块，那么沿一条横线扫过去，颜色只在换格那一瞬
变一次，"每格变色次数"就是 1；如果每格有自己的纹理（草是短竖线、岩石是砾石、
海是波纹），同一个数应该在 3 以上。所以这里量的就是这个数。

倍数不从组件里读（vendored 的是 vue.global.prod.js，元素上没有 __vueParentComponent），
而是由脚本自己按点击次数算：每点一次 ＋ 就是 ×1.3，到上限不再变。
上限和面板同一个算法：一格最多 44 像素。

用法：python .tools/ui_mapzoom.py            （1440 视口）
      python .tools/ui_mapzoom.py --w 390
      python .tools/ui_mapzoom.py --theme light
"""
import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
sys.stderr.reconfigure(encoding="utf-8", errors="replace")
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tmp", "shots")

STEP = 1.3        # 和 map.js 里 zoomBy(1.3) 同一个数
CELL_CAP = 44     # 和 zoomCap() 里"一格最多 44 像素"同一个数

# 一根横线扫过去：变色次数、最长连续同色段、不同颜色数。
# 格数从"采样网格"那一行读——画布画的是采样网格，不是徽章上的世界尺寸。
SCAN = r"""(() => {
  const cv = document.querySelector('.map-canvas');
  if (!cv) return { error: '没有画布' };
  const ctx = cv.getContext('2d');
  const y = Math.round(cv.height / 2);
  let g = 0;
  document.querySelectorAll('.kv').forEach((x) => {
    const m = (x.textContent || '').match(/采样网格\D*(\d+)/);
    if (m && !g) g = Number(m[1]);
  });
  if (!g) return { error: '读不到采样网格那行数' };
  const css = Math.min(cv.clientWidth, cv.clientHeight);
  const n = Math.max(8, Math.min(300, css));
  const x0 = Math.round((cv.width - n) / 2);
  const d = ctx.getImageData(x0, y, n, 1).data;
  let runs = 0, prev = -1, maxRun = 0, cur = 0;
  const set = new Set();
  for (let i = 0; i < n; i++) {
    const k = (d[i * 4] << 16) | (d[i * 4 + 1] << 8) | d[i * 4 + 2];
    if (k !== prev) { runs++; cur = 1; prev = k; } else { cur++; if (cur > maxRun) maxRun = cur; }
    set.add(k);
  }
  const btns = [...document.querySelectorAll('.map-tools .btn')]
    .map((x) => (x.disabled ? "禁" : "可") + x.textContent.trim()).join(',');
  return { g, css, base: Math.round(css / g * 100) / 100, scanPx: n,
           runs, maxRun, distinct: set.size, btns };
})()"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--w", type=int, default=1440)
    ap.add_argument("--theme", default="dark")
    ap.add_argument("--steps", type=int, default=10)
    a = ap.parse_args()
    os.makedirs(OUT, exist_ok=True)
    w, h = a.w, (844 if a.w <= 560 else 900)
    b = Browser(w, h)
    try:
        # 隔离 profile 会被别的量图脚本复用，里面可能还留着上次的凭据；
        # 但清凭据必须排在 goto 之后：标签页进来时可能停在 about:blank 或别的源上，
        # 先 removeItem 清的是别人的存储，面板那份登录态还在，reload 就进不了登录页
        # （同 ui_shots.py 里写死的那条）。
        b.goto(BASE + "/#/")
        if not b.wait("location.origin === %s" % json.dumps(BASE)):
            raise SystemExit("没落到面板这一页")
        b.js("sessionStorage.removeItem('dstpanel.creds')")
        b.reload()
        if not b.wait("!!document.querySelector('.login-form')"):
            raise SystemExit("登录层没出来")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("带着凭据重进还是停在登录层")
        b.js("document.documentElement.setAttribute('data-theme', %s)" % json.dumps(a.theme))
        b.js("location.hash = '#/map'")
        if not b.wait("!!document.querySelector('.map-canvas') && !!document.querySelector('.map-cover')"):
            raise SystemExit("地图一直没就绪")
        time.sleep(1.2)

        first = b.js(SCAN) or {}
        if first.get("error"):
            raise SystemExit(first["error"])
        g, base = first["g"], first["base"]
        cap = max(3.0, min(26.0, CELL_CAP / base))
        print("视口 %d×%d 主题 %s ｜ 采样网格 %d 格 ｜ 一格 %.2fpx（1 倍）｜ 放大上限 %.1f 倍"
              % (w, h, a.theme, g, base, cap))
        print("%-6s %-8s %-6s %-9s %-8s %-8s %s" % (
            "倍数", "一格px", "变色", "每格变色", "最长同色", "不同色", "按钮"))
        cur = first
        for k in range(a.steps + 1):
            zoom = min(cap, STEP ** k)
            s = base * zoom
            per = round(cur["runs"] / (cur["scanPx"] / s), 2)
            verdict = "有纹理" if per >= 2.5 else ("纯色块" if per < 1.6 else "偏少")
            print("%-6.1f %-8.1f %-6d %-9s %-8d %-8d %s  ← %s" % (
                zoom, s, cur["runs"], per, cur["maxRun"], cur["distinct"], cur["btns"], verdict))
            b.shoot(os.path.join(OUT, "zoom_%d_%d.png" % (w, k)))
            if zoom >= cap:
                break
            if not b.click("'.map-tools .btn'", "(x) => x.textContent.indexOf('＋') >= 0"):
                print("找不到放大按钮")
                break
            time.sleep(0.5)
            cur = b.js(SCAN) or cur
    finally:
        b.close()


if __name__ == "__main__":
    main()
