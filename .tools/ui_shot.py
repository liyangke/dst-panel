# 界面验收（模组卡片这一页）：在指定视口宽度下渲染面板，量卡片布局并截图。
#
# 为什么需要它：Qoder 内置浏览器现在没有渲染面（innerWidth=0），
# DOM 结构读得到、布局量不出来，而"手机上会不会横向溢出""一行几张卡"只能按真实
# 布局判。所以这里直连本机 Chrome 的 DevTools 口（没有就自己拉一个无头实例）。
#
# DevTools 客户端、以及"只杀自己拉的 chrome 进程"这条约束都在 .tools/cdp.py；
# 全站逐页的量测在 .tools/ui_shots.py。这个文件只留模组页特有的东西：
# 卡片网格怎么量、详情层怎么量、以及给兜底路径造数据的 --inject。
#
#   python .tools/ui_shot.py 390 844 out.png            # 模组页默认列表
#   python .tools/ui_shot.py 1440 900 out.png --q bag   # 走一次真在线检索
#   python .tools/ui_shot.py 390 844 out.png --detail    # 点开第一张卡的详情层
#   python .tools/ui_shot.py 390 844 out.png --dark      # 深色主题
#
# --inject 是给"图挂了要退回色块"这类兜底路径造数据用的：它只在这一条接口上补字段，
# 不写进任何产物。默认列表没有预览图（磁盘来源本来就没有），要看带图的效果就配 --q。
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

TAB = "找模组"

# 和 ui_dev.py 同一件事：Windows 控制台默认不是 UTF-8，中文输出会变成一堆乱码。
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

STUB = r"""(() => {
  const svg = '<svg xmlns="http://www.w3.org/2000/svg" width="460" height="215">' +
    '<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1">' +
    '<stop offset="0" stop-color="#2b6cb0"/><stop offset="1" stop-color="#e67e22"/></linearGradient></defs>' +
    '<rect width="460" height="215" fill="url(#g)"/>' +
    '<text x="24" y="126" font-size="40" font-family="sans-serif" fill="#fff">PREVIEW 460x215</text></svg>';
  window.__img = "data:image/svg+xml;charset=utf-8," + encodeURIComponent(svg);
  window.__real = window.fetch;
  window.fetch = async (u, o) => {
    const r = await window.__real(u, o);
    if (typeof u === "string" && u.indexOf("/api/v1/mods/search") >= 0) {
      const j = await r.json();
      ((j && j.data && j.data.items) || []).forEach((it, i) => {
        it.previewUrl = (i === 0 && window.__bad) ? "http://127.0.0.1:9/nope.jpg" : window.__img;
        it.subscriptions = 20134 - i * 120;
        it.sizeLabel = (480 + i * 90) + " KB";
        it.updatedLabel = "2026-10-0" + (1 + i);
      });
      return new Response(JSON.stringify(j), { status: 200, headers: { "content-type": "application/json" } });
    }
    return r;
  };
  return "stubbed";
})()"""

MEASURE = r"""(() => {
  const g = document.querySelector('.mod-cards');
  if (!g) return { error: 'no .mod-cards' };
  const cs = getComputedStyle(g);
  const cards = [...g.children].map((c) => {
    const b = c.getBoundingClientRect();
    const img = c.querySelector('img.mod-cover-img');
    const desc = c.querySelector('.mod-card-desc');
    return {
      w: Math.round(b.width), h: Math.round(b.height),
      ph: (c.querySelector('.mod-cover-ph') || {}).textContent || '',
      imgOk: img ? !!(img.complete && img.naturalWidth > 0) : null,
      badge: (c.querySelector('.mod-cover-state .badge') || {}).textContent || '',
      name: ((c.querySelector('.mod-card-name') || {}).textContent || '').trim().slice(0, 28),
      meta: ((c.querySelector('.mod-card-meta') || {}).textContent || '').trim(),
      descShown: desc ? getComputedStyle(desc).display !== 'none' : false,
      desc: ((desc || {}).textContent || '').trim().slice(0, 40),
      warns: [...c.querySelectorAll('.mod-warn')].map((x) => x.textContent.trim()),
      foot: [...c.querySelectorAll('.mod-card-foot .btn')].map((x) => {
        const r = x.getBoundingClientRect();
        return x.textContent.trim() + ' ' + Math.round(r.width) + 'x' + Math.round(r.height);
      }),
    };
  });
  return { vw: innerWidth, vh: innerHeight, overflow: document.documentElement.scrollWidth > innerWidth,
           gridW: Math.round(g.getBoundingClientRect().width), cols: cs.gridTemplateColumns,
           gap: cs.gap, n: cards.length, cards };
})()"""

DETAIL = r"""(() => {
  const d = document.querySelector('.mod-detail');
  if (!d) return { error: 'no dialog' };
  const m = document.querySelector('.mask').getBoundingClientRect();
  const b = d.getBoundingClientRect();
  const img = d.querySelector('img.mod-cover-img');
  return { vw: innerWidth, vh: innerHeight,
           mask: Math.round(m.width) + 'x' + Math.round(m.height),
           dialog: Math.round(b.width) + 'x' + Math.round(b.height),
           teleportedToBody: document.querySelector('.mask').parentElement === document.body,
           title: (d.querySelector('h3') || {}).textContent,
           imgOk: img ? !!(img.complete && img.naturalWidth > 0) : null,
           kv: [...d.querySelectorAll('.kv > *')].map((x) => x.textContent).slice(0, 14),
           btns: [...d.querySelectorAll('.dialog-foot .btn')].map((x) => x.textContent.trim()),
           overflowX: document.documentElement.scrollWidth > innerWidth };
})()"""


def main():
    a = sys.argv[1:]
    w_arg = int(a[0]) if a else 390
    h_arg = int(a[1]) if len(a) > 1 else 844
    out_png = a[2] if len(a) > 2 and not a[2].startswith("--") else ""
    query = a[a.index("--q") + 1] if "--q" in a else ""
    detail = "--detail" in a
    tab = a[a.index("--tab") + 1] if "--tab" in a else TAB

    b = Browser(w_arg, h_arg)
    try:
        b.goto(BASE + "/#/mods")
        b.wait("!!document.querySelector('.login-form')")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.tabs')"):
            raise SystemExit("登录后还停在登录页")
        # 用序号切标签页是因为 Git Bash 传中文参数会当场挂（exit 127），
        # 而这三步本来就是固定顺序：1 找模组、2 这台机器上的文件、3 这个房间的开关。
        if tab.isdigit():
            ok = b.js("(() => { const bs = [...document.querySelectorAll('.tabs button')];"
                      " const t = bs[%s]; if (!t) return false; t.click(); return true; })()"
                      % (int(tab) - 1))
        else:
            ok = b.click("'.tabs button'", "(x) => x.textContent.indexOf('%s') >= 0" % tab)
        if not ok:
            raise SystemExit("找不到标签页：%s" % tab)
        if not b.wait("!!document.querySelector('.mod-cards')"):
            raise SystemExit("卡片网格没渲染出来")

        if query:
            b.js("(() => { const i = document.querySelector('input[type=\"search\"]');"
                 " i.value = %s; i.dispatchEvent(new Event('input', {bubbles:true})); })()"
                 % json.dumps(query, ensure_ascii=False))
            b.click("'button'", "(x) => /^搜/.test(x.textContent.trim())")
            if not b.wait("[...document.querySelectorAll('.card-head .chip')]"
                          ".some((x) => x.textContent.indexOf('在线') >= 0)"):
                raise SystemExit("在线检索没答上来（本机到 api.steampowered.com 不通时属正常，换 --q 或用 --inject）")
            time.sleep(1.5)          # 等预览图从 CDN 进来

        if "--inject" in a:
            b.js("window.__bad = %s" % ("true" if "--bad" in a else "false"))
            b.js(STUB)
            b.click("'button'", "(x) => /^搜/.test(x.textContent.trim())")
            b.wait("document.querySelectorAll('.mod-cover-img, .mod-cover-ph').length > 0")
            time.sleep(1.2)          # 给故意取不到的那张图留出失败时间

        time.sleep(0.5)
        report = b.js(MEASURE)
        os.makedirs(".tools/tmp", exist_ok=True)
        tag = "%s%s" % (query or "local", "_detail" if detail else "")
        with open(".tools/tmp/measure_%dx%d.json" % (w_arg, h_arg), "w", encoding="utf-8") as f:
            json.dump({"tag": tag, **report}, f, ensure_ascii=False, indent=1)

        if detail:
            b.click("'.mod-card-foot .btn'", "(x) => x.textContent.trim() === '详细信息'")
            b.wait("!!document.querySelector('.mod-detail')")
            time.sleep(0.5)
            with open(".tools/tmp/measure_detail_%dx%d.json" % (w_arg, h_arg), "w", encoding="utf-8") as f:
                json.dump(b.js(DETAIL), f, ensure_ascii=False, indent=1)

        if out_png:
            if "--dark" in a:
                b.js("document.documentElement.setAttribute('data-theme','dark')")
                time.sleep(0.3)
            if not detail:
                b.js("document.querySelector('.mod-cards').scrollIntoView({block:'start'}); window.scrollBy(0,-10)")
            time.sleep(0.4)
            b.shoot(out_png)
    finally:
        b.close()

    cards = report.get("cards") or []
    print("视口 %dx%d｜一行 %d 张｜卡宽 %spx｜横向溢出 %s｜图 %d/%d 张加载成功" % (
        w_arg, h_arg, report.get("cols", "").count("px"), cards[0]["w"] if cards else 0,
        report.get("overflow"), sum(1 for c in cards if c.get("imgOk")), len(cards)))


if __name__ == "__main__":
    main()
