# 界面图册：把指定页面按"整页高度"出图，并攒成一个单文件 HTML，方便发给手机看。
#
# 为什么和 ui_shots.py 分开：那一支是量具（溢出像素、文字密度、触控尺寸），拍的是**视口**；
# 出门在手机上想看"这一屏下面塌没塌"，视口截图只会给出首屏，剩下的要靠本地滚动才能看见。
# 这一支只解决"看图"，所以把视口高度直接设成页面的 scrollHeight，一次拍全。
#
# 为什么需要它：开发面板只绑 127.0.0.1，手机不在同一个局域网就打不开。图册是零安装的那条路——
# 图落到一个目录，把这个目录发出去（托管或隧道都行）就是一个能点开的链接。
#
#   python .tools/ui_gallery.py                      # 总览，1440 + 390，浅色
#   python .tools/ui_gallery.py --page overview,players
#   python .tools/ui_gallery.py --widths 390 --dark
#
# 拍完打印目录和每张图的文件名；index.html 里图片按 100% 宽铺满，手机上不用缩放。
import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser
from ui_shots import PAGES, login_layer

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tmp", "gallery")

# 一次拍全的高度和正常视口不一样，所以要单独确认"这一页真的渲染完了"：
# 外壳在、这一页的接口至少回来一轮，否则拍到的是一整页骨架屏。
READY = "!!document.querySelector('.shell') && !!document.querySelector('.topbar-title')"

TALL = r"""(() => {
  const c = document.documentElement;
  return Math.max(c.scrollHeight, document.body.scrollHeight);
})()"""


def shoot_page(b, name, href, w, theme, rows):
    b.js("location.hash = %s" % json.dumps("#" + href))
    if not b.wait(READY, tries=40):
        rows.append({"name": name, "w": w, "file": None, "error": "页面没渲染出来"})
        return
    time.sleep(1.8)
    full_h = b.js(TALL)
    if not full_h:
        rows.append({"name": name, "w": w, "file": None, "error": "读不到整页高度"})
        return
    # 高度封顶：整页拉到几千 CSS px 时 Chrome 的 device metrics 会开始丢内容，
    # 而且那种长度的图手机上也没法看。超过这一档就退回首屏视口，并在报告里写清楚。
    if full_h > 4200:
        full_h = 900 if w > 560 else 844
    b.set_viewport(w, full_h)
    b.js("window.scrollTo(0, 0)")
    time.sleep(0.6)
    fn = "%s_%d_%s.png" % (name, w, theme)
    b.shoot(os.path.join(OUT, fn))
    rows.append({"name": name, "w": w, "h": full_h, "file": fn,
                 "docW": b.js("document.documentElement.scrollWidth")})


def write_index(rows, theme, built):
    items = []
    for r in rows:
        if not r.get("file"):
            items.append("<p class=bad>%s（%d）：%s</p>" % (r["name"], r["w"], r.get("error")))
            continue
        over = "" if r["docW"] <= r["w"] + 1 else '<em class=over>横向溢出 %dpx</em>' % (r["docW"] - r["w"])
        items.append(
            '<figure><figcaption>%s · %d×%d %s</figcaption><img src="%s" alt="%s"></figure>'
            % (r["name"], r["w"], r["h"], over, r["file"], r["name"]))
    html = """<!doctype html><html lang=zh><head><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>面板图册 %s</title>
<style>body{margin:0;padding:16px;background:#f4f6fa;color:#1c2430;
font:15px/1.6 system-ui,"Microsoft YaHei",sans-serif}
h1{font-size:17px;margin:0 0 4px}.meta{color:#66707d;font-size:13px;margin-bottom:18px}
figure{margin:0 0 22px}figcaption{font-size:13px;color:#66707d;margin-bottom:6px}
img{width:100%%;height:auto;display:block;border:1px solid #dde2ea;border-radius:10px}
.over{color:#c0392b;font-style:normal;margin-left:6px}.bad{color:#c0392b}</style>
</head><body>
<h1>饥荒服务器面板 · 图册</h1>
<p class=meta>主题 %s · 生成于 %s · 数据来源：本地开发面板的夹具（不是真机）</p>
%s
</body></html>""" % (theme, theme, built, "\n".join(items))
    path = os.path.join(OUT, "index.html")
    with open(path, "w", encoding="utf-8") as f:
        f.write(html)
    return path


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--page", default="overview", help="逗号分隔，名字照 ui_shots 的 PAGES")
    ap.add_argument("--widths", default="1440,390")
    ap.add_argument("--dark", action="store_true")
    a = ap.parse_args()
    theme = "dark" if a.dark else "light"
    want = [p for p in a.page.split(",") if p]
    widths = [int(x) for x in a.widths.split(",")]
    pages = [(n, h) for (n, h) in PAGES if n in want]
    if len(pages) != len(want):
        known = ", ".join(n for (n, _) in PAGES)
        print("页面名对不上，可用的是：%s" % known)
        return 1

    os.makedirs(OUT, exist_ok=True)
    for w in widths:
        for fn in os.listdir(OUT):
            if fn.endswith(".png") and ("_%d_" % w) in fn:
                os.remove(os.path.join(OUT, fn))

    rows = []
    b = Browser(widths[0], 900)
    try:
        for w in widths:
            b.set_viewport(w, 900 if w > 560 else 844)
            login_layer(b, theme)
            b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
            b.reload()
            if not b.wait("!!document.querySelector('.shell')", tries=40):
                print("带着凭据重进还是停在登录层")
                return 1
            b.js("document.documentElement.setAttribute('data-theme', %s)" % json.dumps(theme))
            for name, href in pages:
                shoot_page(b, name, href, w, theme, rows)
    finally:
        b.close()

    path = write_index(rows, theme, time.strftime("%Y-%m-%d %H:%M"))
    for r in rows:
        print("%-10s %5d %s %s" % (r["name"], r["w"], r.get("file") or "-", r.get("error") or ""))
    print("图册目录：%s（入口 %s）" % (OUT, path))
    return 0 if all(r.get("file") for r in rows) else 1


if __name__ == "__main__":
    sys.exit(main())
