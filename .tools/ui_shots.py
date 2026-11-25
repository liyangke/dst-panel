# 界面验收（全站逐页）：在几档视口下把每个页面都渲染一遍，量布局、截图、并给每页打"文字密度"分。
#
# 为什么需要它：ui_shot.py 只盯模组卡片那一页，而这轮改的是整个外壳——手机端底部导航、
# 抽屉、每页的色相、把长说明收进悬浮提示。这些都要"每页都看一眼"才验得完，
# 手工在浏览器里点 11 页 × 3 个宽度不现实，也没有可比较的数字。
#
# 两个不靠眼睛的判断：
#   横向溢出：找到右边缘越过视口的元素并把选择器路径打出来，否则只知道"溢出了"不知道是谁。
#   文字密度：首屏可见正文的字符数，以及触不到 44px 高按钮的个数。
#     用户说"一个屏幕上的字太多了""手机太难操作"，这两条就是它们的度量；
#     改完必须让数字真的降下来，而不是我自己觉得变清爽了。
#
#   python .tools/ui_shots.py                    # 390 / 768 / 1440 三档全跑
#   python .tools/ui_shots.py 390                # 只跑一档
#   python .tools/ui_shots.py 390 --page map     # 只跑一页（调试用）
#   python .tools/ui_shots.py 1440 --light       # 用浅色主题（默认深色）
import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# 顺序就是 nav.js 里的顺序；href 也是从那张表抄来的，页面改名时这里要跟着改。
# 「公网房间」不在这里：那一页要出门取 Klei 的公开桶，量布局时网络一抖就是一条假红。
PAGES = [
    ("overview", "/"),
    ("room", "/room"),
    ("players", "/players"),
    ("map", "/map"),
    ("mods", "/mods"),
    ("snaps", "/snaps"),
    ("logs", "/logs"),
    ("tasks", "/tasks"),
    ("audit", "/audit",),
    ("accounts", "/accounts"),
    ("settings", "/settings"),
]

# 相对仓库根，不是相对当前工作目录：从 .tools/ 里直接跑也要写到同一份截图目录，
# 否则会出现"报告打出来了、图却落在别处"，然后拿着旧图判断新改动。
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tmp", "shots")

# 一次量完一页：外壳状态 + 溢出源 + 首屏文字密度 + 触控尺寸。
MEASURE = r"""(() => {
  const vw = innerWidth, vh = innerHeight;
  const cx = document.documentElement;
  const shown = (el) => {
    const r = el.getBoundingClientRect();
    const s = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && s.display !== 'none' && s.visibility !== 'hidden'
      && r.top < vh && r.bottom > 0;
  };
  const path = (el) => {
    const parts = [];
    let n = el;
    for (let i = 0; i < 4 && n && n !== document.body; i++) {
      let s = n.tagName.toLowerCase();
      if (n.className && typeof n.className === 'string') s += '.' + n.className.trim().split(/\s+/)[0];
      parts.unshift(s);
      n = n.parentElement;
    }
    return parts.join(' > ');
  };
  // 溢出源：只报越界最多的三个，全报的话一条长行能把输出冲掉。
  const over = [];
  document.querySelectorAll('.shell *').forEach((el) => {
    const r = el.getBoundingClientRect();
    if (r.right > vw + 1 && r.width <= vw * 3) over.push({ p: path(el), px: Math.round(r.right - vw) });
  });
  over.sort((a, b) => b.px - a.px);

  const main = document.querySelector('.main');
  let chars = 0, blocks = 0, hints = 0, hintChars = 0, small = 0, tapMin = 999;
  const smalls = [];
  // 手机上低于 40px 手指点不准；桌面上 24px 的图标按钮是正常尺寸，
  // 用同一把尺子量会得到一屏"小按钮"，那个数字就没有意义了。
  const tapFloor = vw <= 560 ? 40 : 24;
  if (main) {
    main.querySelectorAll('button, input, select, a, .stat, .chip, .kv, .hint, .note, .muted, h1, h2, h3, td, th, li, label')
      .forEach((el) => {
        if (!shown(el)) return;
        // 直接文本：只有挂在元素自己身上的文本才算一次，否则父子会重复计。
        let own = '';
        el.childNodes.forEach((n) => { if (n.nodeType === 3) own += n.textContent; });
        own = own.trim();
        if (own) { chars += own.length; blocks++; }
        const cls = el.className && typeof el.className === 'string' ? el.className : '';
        if (/(^|\s)(hint|note|muted)(\s|$)/.test(cls) && own) { hints++; hintChars += own.length; }
        if (el.tagName === 'BUTTON') {
          const r = el.getBoundingClientRect();
          tapMin = Math.min(tapMin, Math.round(r.height));
          if (r.height < tapFloor) {
            small++;
            // 只给计数的话，我照样得一个个页面翻截图找元凶；把名字报出来才能改。
            smalls.push({
              p: path(el),
              cls: (typeof el.className === 'string' ? el.className : '').trim(),
              h: Math.round(r.height),
              text: (el.textContent || '').trim().slice(0, 12),
            });
          }
        }
      });
  }
  const sidebar = document.querySelector('.sidebar');
  const tabbar = document.querySelector('.tabbar');
  const tiles = [...document.querySelectorAll('.ico-tile')].map((x) => getComputedStyle(x).color);
  return {
    vw, vh,
    overflowPx: cx.scrollWidth - vw,
    overflowWho: over.slice(0, 3),
    accent: getComputedStyle(cx).getPropertyValue('--page-accent').trim(),
    sidebarShown: sidebar ? getComputedStyle(sidebar).display !== 'none' : false,
    tabbarShown: tabbar ? getComputedStyle(tabbar).display !== 'none' : false,
    tabs: tabbar ? tabbar.children.length : 0,
    navOn: (document.querySelector('.nav-item.on span') || {}).textContent || '',
    topbarTitle: (document.querySelector('.topbar-title') || {}).textContent || '',
    tileColor: tiles[0] || '',
    chars, blocks, hintLines: hints, hintChars,
    tapMin: tapMin === 999 ? null : tapMin,
    smallButtons: small,
    smallWho: (() => {
      // 同一个类名的按钮往往有好几个（一行一个"重读"），并成一条报出来才看得清。
      const by = {};
      smalls.forEach((s) => {
        const k = s.cls + '|' + s.h;
        if (!by[k]) by[k] = { cls: s.cls, h: s.h, text: s.text, n: 0, p: s.p };
        by[k].n++;
      });
      return Object.values(by).sort((a, b) => a.h - b.h).slice(0, 4);
    })(),
    cards: document.querySelectorAll('.main .card').length,
    stats: document.querySelectorAll('.main .stat').length,
    tips: document.querySelectorAll('.main .tip-btn, .topbar .tip-btn').length,
    empty: document.querySelectorAll('.main .empty').length,
    skeletons: document.querySelectorAll('.main .sk').length,
    errBoxes: [...document.querySelectorAll('.main .err-box')].map((x) => x.textContent.trim().slice(0, 40)),
  };
})()"""

# 抽屉单独量一次：手机端"更多"里必须能到全部页面，且不能横向溢出。
SHEET = r"""(() => {
  const s = document.querySelector('.sheet');
  if (!s) return { error: '没有抽屉' };
  const r = s.getBoundingClientRect();
  return { overflowPx: document.documentElement.scrollWidth - innerWidth,
           width: Math.round(r.width), top: Math.round(r.top), height: Math.round(r.height),
           items: [...s.querySelectorAll('.sheet-item')].map((x) => {
             const b = x.getBoundingClientRect();
             return x.textContent.trim() + ' ' + Math.round(b.width) + 'x' + Math.round(b.height);
           }),
           status: [...s.querySelectorAll('.kv > *')].map((x) => x.textContent.trim()).slice(0, 8) };
})()"""

LOGIN = r"""(() => {
  const c = document.querySelector('.login-shell');
  const b = c ? c.getBoundingClientRect() : null;
  return { overflowPx: document.documentElement.scrollWidth - innerWidth,
           vw: innerWidth, vh: innerHeight,
           shell: b ? Math.round(b.width) + 'x' + Math.round(b.height) : '',
           heroShown: (() => { const e = document.querySelector('.login-hero');
             return e ? getComputedStyle(e).display !== 'none' : false; })(),
           slogan: ((document.querySelector('.login-slogan') || {}).textContent || '').trim(),
           points: document.querySelectorAll('.login-points li').length,
           fieldH: Math.round((document.querySelector('.login-submit') || { getBoundingClientRect: () => ({ height: 0 }) }).getBoundingClientRect().height),
           chars: [...document.querySelectorAll('.login .login-shell, .login .login-card-foot')].length,
           eye: (() => { const e = document.querySelector('.login-eye');
             return e ? Math.round(e.getBoundingClientRect().height) : null; })() };
})()"""


def login_layer(b, theme):
    # 只清 sessionStorage 是不够的：应用还活在内存里，`creds` 变量没被清掉，
    # 换个 hash 也不会重载页面。所以清完必须显式 reload，否则拍到的是上一个页面。
    # goto 必须在清凭据之前：刚拉起来的无头实例停在 about:blank，那里读 sessionStorage
    # 直接 SecurityError（第一次跑全站量测就是栽在这一步）。
    b.goto(BASE + "/#/")
    b.js("sessionStorage.removeItem('dstpanel.creds')")
    b.reload()
    b.wait("!!document.querySelector('.login-form')")
    b.js("document.documentElement.setAttribute('data-theme', %s)" % json.dumps(theme))
    time.sleep(0.4)
    return b.js(LOGIN)


def goto_page(b, href):
    b.js("location.hash = %s" % json.dumps("#" + href))
    # 换页要等外壳在、且这一页的接口至少回来一轮：量太早会拍到骨架屏。
    if not b.wait("!!document.querySelector('.main') && !!document.querySelector('.topbar-title')", tries=30):
        return
    time.sleep(1.6)


def run(widths, only_page, theme):
    only = [p for p in (only_page or "").split(",") if p]
    os.makedirs(OUT, exist_ok=True)
    b = Browser(widths[0], 900 if widths[0] > 560 else 844)
    rows = []
    try:
        for w in widths:
            h = 844 if w <= 560 else (1024 if w <= 900 else 900)
            b.set_viewport(w, h)
            login = login_layer(b, theme)
            login_row = {"page": "登录", "w": w, "measure": login}
            b.shoot(os.path.join(OUT, "login_%d.png" % w))
            rows.append(login_row)

            b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
            b.reload()
            if not b.wait("!!document.querySelector('.shell')"):
                raise SystemExit("带着凭据重进还是停在登录层")
            b.js("document.documentElement.setAttribute('data-theme', %s)" % json.dumps(theme))

            for name, href in PAGES:
                if only and name not in only:
                    continue
                goto_page(b, href)
                m = b.js(MEASURE)
                b.shoot(os.path.join(OUT, "%s_%d.png" % (name, w)))
                rows.append({"page": name, "w": w, "measure": m})

            if w <= 900 and (not only or "overview" in only):
                # 手机上其余页面藏在"更多"里：这一条是本轮新导航的主路径，必须单独量。
                b.js("location.hash = '#/'")
                time.sleep(0.6)
                opened = b.click("'.tabbar .tab'", "(x) => x.textContent.indexOf('更多') >= 0")
                time.sleep(0.5)
                s = b.js(SHEET) if opened else {"error": "底部栏里没有更多按钮"}
                b.shoot(os.path.join(OUT, "sheet_%d.png" % w))
                rows.append({"page": "抽屉", "w": w, "measure": s})
    finally:
        b.close()

    with open(os.path.join(OUT, "pages.json"), "w", encoding="utf-8") as f:
        json.dump(rows, f, ensure_ascii=False, indent=1)
    report(rows)


def report(rows):
    print("%-9s %5s %6s %6s %6s %6s %5s %5s %s" % (
        "页面", "视口", "溢出", "首屏字", "说明行", "最小触", "小按钮", "卡/图", "色相/备注"))
    for r in rows:
        m = r["measure"]
        if r["page"] == "登录":
            print("%-9s %5d %6s %6s %6s %6s %5s %5s %s" % (
                "登录", r["w"], m.get("overflowPx"), "-", "-", m.get("eye"), "-", "-",
                "hero=%s 要点%d条 标语%d字" % (m.get("heroShown"), m.get("points", 0),
                                               len(m.get("slogan", "")))))
            continue
        if r["page"] == "抽屉":
            items = m.get("items") or []
            print("%-9s %5d %6s %6s %6s %6s %5s %5s %s" % (
                "抽屉", r["w"], m.get("overflowPx"), "-", "-", "-", "-", len(items),
                " | ".join(items[:3]) if items else str(m)))
            continue
        note = m.get("accent") or ""
        if m.get("overflowPx", 0) > 0 and m.get("overflowWho"):
            note += " 越界最多：%s +%dpx" % (m["overflowWho"][0]["p"], m["overflowWho"][0]["px"])
        if m.get("errBoxes"):
            note += " 报错：%s" % "; ".join(m["errBoxes"])[:60]
        if m.get("skeletons"):
            note += " 还在骨架屏"
        print("%-9s %5d %6s %6d %6d %6s %5d %5s %s" % (
            r["page"], r["w"], m.get("overflowPx"), m.get("chars", 0), m.get("hintLines", 0),
            m.get("tapMin"), m.get("smallButtons", 0),
            "%d/%d" % (m.get("cards", 0), m.get("stats", 0)), note[:78]))

    # 点不准的按钮：光有计数我只能去翻截图，这里直接把类名和高度报出来。
    hits = [(r["page"], r["w"], m) for r in rows
            for m in [r["measure"]] if m.get("smallWho")]
    if hits:
        print("\n小按钮明细（低于本档触控下限的）：")
        for page, w, m in hits:
            for s in m["smallWho"]:
                print("   %-9s %4dpx  h=%d ×%d  %s  「%s」" % (
                    page, w, s["h"], s["n"], s["cls"] or s["p"], s["text"]))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("widths", nargs="*", type=int)
    ap.add_argument("--page", default="")
    ap.add_argument("--light", action="store_true")
    a = ap.parse_args()
    run(a.widths or [390, 768, 1440], a.page, "light" if a.light else "dark")


if __name__ == "__main__":
    main()
