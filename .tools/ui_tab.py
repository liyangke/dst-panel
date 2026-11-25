# 拍某一页里某个标签页（以及标签页里某个控件）的样子。
#
# 为什么单独要一个：ui_shots.py 每页只拍默认那一栏，而这轮加的东西全在第二栏以后
# （远程指令、集群设置）。那些页光看默认截图等于没看——按钮、参数表单、危险色都在里面。
#
# 参数一律用序号不用中文：Git Bash 里命令行带中文会直接 exit 127，
# 序号既绕开了这个坑，也和 .tabs 的 DOM 顺序一一对应，改导航时一眼能看出来。
#
#   python .tools/ui_tab.py room 2                 # 房间页第 3 个标签
#   python .tools/ui_tab.py room 1 --chip 2        # 再点第 3 枚指令胶囊（展开参数）
#   python .tools/ui_tab.py room 1 --chip 0 --send # 点发送并确认，看回执和提示条
#   python .tools/ui_tab.py room 2 --w 390 --light
import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tmp", "shots")
HREF = {"overview": "/", "room": "/room", "players": "/players",
        "map": "/map", "mods": "/mods", "snaps": "/snaps", "logs": "/logs",
        "tasks": "/tasks", "audit": "/audit", "accounts": "/accounts", "settings": "/settings"}

# 这一栏有没有真的挂上：报出标签名、胶囊数、以及展开后看得见的按钮文字。
# 胶囊的类名是 .op（不是 .cmd——那三个字符被"加入命令弹窗"占了，
# 用它会量到弹窗那一段，屏幕上有没有胶囊都会报出一堆）。
PROBE = r"""(() => {
  const tabs = [...document.querySelectorAll('.tabs button')].map((x) => x.textContent.trim());
  const on = document.querySelector('.tabs button.on');
  const chips = [...document.querySelectorAll('.op')].map((x) => x.textContent.trim());
  const facts = [...document.querySelectorAll('.fact')].map((x) => x.textContent.trim().replace(/\s+/g, ' '));
  const btns = [...document.querySelectorAll('.main button')].filter((el) => {
    const r = el.getBoundingClientRect();
    return r.height > 0;
  }).length;
  const txt = (s) => { const e = document.querySelector(s); return e ? e.textContent.trim().replace(/\s+/g, ' ') : ''; };
  return { overflowPx: document.documentElement.scrollWidth - innerWidth,
           on: on ? on.textContent.trim() : '', tabs, chips, facts, btns,
           desc: txt('.op-desc'), send: txt('.op-panel .btn'), last: txt('.op-last'),
           toasts: [...document.querySelectorAll('.toast')].map((x) => x.textContent.trim().replace(/\s+/g, ' ')),
           err: [...document.querySelectorAll('.err-box')].map((x) => x.textContent.trim()) };
})()"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("page")
    ap.add_argument("tab", type=int)
    ap.add_argument("--chip", type=int, default=-1)
    ap.add_argument("--send", action="store_true",
                    help="点开发送按钮并顺带确认，用来验指令真的能发出去")
    ap.add_argument("--w", type=int, default=1440)
    ap.add_argument("--light", action="store_true")
    a = ap.parse_args()

    href = HREF.get(a.page)
    if href is None:
        raise SystemExit("页面名不认得：%s（可用：%s）" % (a.page, "、".join(HREF)))
    os.makedirs(OUT, exist_ok=True)
    h = 844 if a.w <= 560 else 900
    theme = "light" if a.light else "dark"
    b = Browser(a.w, h)
    try:
        b.goto(BASE + "/#/")
        b.js("sessionStorage.removeItem('dstpanel.creds')")
        b.reload()
        b.wait("!!document.querySelector('.login-form')")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("带着凭据重进还是停在登录层")
        b.js("document.documentElement.setAttribute('data-theme', %s)" % json.dumps(theme))
        b.js("location.hash = %s" % json.dumps("#" + href))
        b.wait("!!document.querySelector('.main') && !!document.querySelector('.topbar-title')")
        time.sleep(1.6)
        if not b.js("(() => { const t = document.querySelectorAll('.tabs button')[%d];"
                    " t && t.click(); return !!t; })()" % a.tab):
            raise SystemExit("第 %d 个标签不存在" % a.tab)
        time.sleep(0.9)
        if a.chip >= 0:
            if not b.js("(() => { const c = document.querySelectorAll('.op')[%d];"
                        " c && c.click(); return !!c; })()" % a.chip):
                raise SystemExit("第 %d 枚胶囊不存在" % a.chip)
            time.sleep(0.5)
        name = "%s_t%d%s%s%s_%d.png" % (a.page, a.tab,
                                        "_c%d" % a.chip if a.chip >= 0 else "",
                                        "_sent" if a.send else "",
                                        "_light" if a.light else "", a.w)
        if a.send:
            if not b.js("(() => { const p = document.querySelector('.op-panel');"
                        " const btn = p && [...p.querySelectorAll('.btn')].pop();"
                        " btn && btn.click(); return !!btn; })()"):
                raise SystemExit("没找到发送按钮（胶囊没展开？）")
            time.sleep(0.8)
            # 危险指令会先弹确认框。那一句文案是"点下去会发生什么"的唯一说明，
            # 所以单独拍一张，别让它被点掉之后再也看不见。
            dlg = b.js("(() => { const d = document.querySelector('.mask .dialog');"
                       " return d ? d.textContent.trim().replace(/\\s+/g, ' ') : ''; })()")
            if dlg:
                print("确认框：%s" % dlg)
                b.shoot(os.path.join(OUT, name.replace(".png", "_confirm.png")))
                b.js("(() => { const d = document.querySelector('.mask .dialog .btn:not(.ghost)');"
                     " d && d.click(); return !!d; })()")
                time.sleep(2.0)
        p = b.js(PROBE)
        # 手机上这一栏在折叠线以下：不滚过去拍到的永远是页头，等于没验。
        b.js("(() => { const el = document.querySelector('.op.on') || document.querySelector('.ops-row')"
             " || document.querySelector('.tabs');"
             " el && el.scrollIntoView({block: 'start'}); return !!el; })()")
        time.sleep(0.4)
        b.shoot(os.path.join(OUT, name))
        print("标签：%s / 全部：%s" % (p["on"], " | ".join(p["tabs"])))
        print("横向溢出：%d px   可见按钮：%d" % (p["overflowPx"], p["btns"]))
        if p["chips"]:
            print("胶囊 %d 枚：%s" % (len(p["chips"]), "、".join(c.replace(" ", "") for c in p["chips"])))
        if p["desc"]:
            print("说明：%s" % p["desc"])
        if p["send"]:
            print("发送按钮：%s" % p["send"].replace(" ", ""))
        if p["last"]:
            print("回执：%s" % p["last"])
        if p["toasts"]:
            print("提示：%s" % "；".join(p["toasts"]))
        if p["facts"]:
            print("事实条 %d 格：%s" % (len(p["facts"]), " | ".join(p["facts"])))
        if p["err"]:
            print("报错：%s" % "；".join(p["err"]))
        print("截图：%s" % os.path.join(OUT, name))
    finally:
        b.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
