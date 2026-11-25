# -*- coding: utf-8 -*-
"""模组页三步动线的验收：页签顺序、被降级的一致性、以及"没保存就想切走"那一问。

为什么单独写这一支：动线重排把四个平级页签改成三步，一致性从一级 tab 变成第 3 步底下
一句状态提示。这类改动最坏的不是点不动，而是"看着还在、其实丢了"——配置表单活在
第 3 步那个组件的内存里，切走就卸组件，用户填了一半的东西会没声没息地消失。
所以这一支必须真改一项、真点切走、真按确认框的两个按钮各走一遍，
而不是只看页签上写了什么字。

跑法：先起着 python .tools/ui_dev.py，再 python .tools/ui_flow_check.py [宽度]
"""
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

W = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tmp", "shots")

FAILED = []
REPORT = []

HOOK = ("window.__perr=[];window.addEventListener('error',function(e){"
        "if(window.__perr.length<20)window.__perr.push(String(e.message))});"
        "window.addEventListener('unhandledrejection',function(e){"
        "if(window.__perr.length<20)window.__perr.push('promise:'+String(e.reason&&e.reason.message||e.reason))});")


def check(name, ok, detail=""):
    REPORT.append((("PASS" if ok else "FAIL"), name, detail))
    print("%s %s%s" % ("PASS" if ok else "FAIL", name, ("  | " + str(detail).replace("\n", " ")[:200]) if detail else ""))
    if not ok:
        FAILED.append(name + "｜" + str(detail)[:160])


def click_btn(b, sel, text, exact=False):
    pred = ("(x) => x.textContent.trim() === %s" if exact else
            "(x) => x.textContent.indexOf(%s) >= 0") % json.dumps(text, ensure_ascii=False)
    return b.click("'%s'" % sel, pred)


def tabs_state(b):
    """页签那一排长什么样：文字、序号、当前选中哪一个。"""
    return b.js(r"""(() => {
      const bs = [...document.querySelectorAll('.tabs button')];
      const stepOf = (x) => { const s = x.querySelector('.tab-step'); return s ? s.textContent.trim() : ''; };
      return {
        n: bs.length,
        texts: bs.map((x) => x.textContent.replace(/\s+/g, ' ').trim()),
        steps: bs.map(stepOf),
        on: (() => { const o = document.querySelector('.tabs button.on');
                     return o ? o.textContent.replace(/\s+/g, ' ').trim() : ''; })(),
        onStep: (() => { const o = document.querySelector('.tabs button.on');
                     return o ? stepOf(o) : ''; })(),
        // 页签不许把页面撑出横向滚动：三步动线在手机上更要紧。
        overflow: document.documentElement.scrollWidth - window.innerWidth,
      };
    })()""")


def click_tab(b, step):
    """按页签上那颗序号点，不按整段文字。

    #85 把标签改成"这一屏回答哪个问题"（装到这台机器→这台机器上的文件、
    给这个房间开→这个房间的开关），这一支当时就是被这种改名点红的：
    脚本按文字找按钮，界面一改措辞就假失败。序号那一层是动线本身，不改名也不会动，
    所以标签只在这一支开头核对一次，后面全部靠序号点。
    """
    return b.js(r"""(() => {
      const t = [...document.querySelectorAll('.tabs button')].find(
        (x) => { const s = x.querySelector('.tab-step'); return s && s.textContent.trim() === %s; });
      if (!t) return false;
      t.click();
      return true;
    })()""" % json.dumps(str(step)))


def dialog(b):
    return b.js(r"""(() => {
      const d = document.querySelector('.mask .dialog');
      if (!d) return null;
      return { title: ((d.querySelector('h3') || {}).textContent || '').trim(),
               body: ((d.querySelector('.muted') || {}).textContent || '').trim(),
               btns: [...d.querySelectorAll('button')].map((x) => x.textContent.trim()) };
    })()""")


def dirty_marks(b):
    """第 3 步那些"配置"按钮上挂着几个"已改 N"，以及总共改了几项。

    行级 class 是 .mod-line，不是 .mod-row：#84 把开关那一屏重做成"开着 / 没开"
    两栏的条目行，.mod-row 现在只剩跨世界一致性那张差异表在用（下面点开细节那处量的就是它）。
    """
    return b.js(r"""(() => {
      const xs = [...document.querySelectorAll('.mod-line .btn .tab-count')]
        .map((x) => x.textContent.trim()).filter((t) => t.indexOf('已改') >= 0);
      return { marks: xs.length, sum: xs.reduce((n, t) => n + (parseInt((t.match(/\d+/) || [0])[0], 10) || 0), 0) };
    })()""")


def page_note(b):
    """失败时把"屏上到底是什么"一起报出来。
    第二轮验收里这一句只留下"登录层没出来"四个字，而同一份脚本单独跑两次都是 17 条全过——
    没有现场就只能在猜测里挑方向，那种红查一次要花的时间够跑三轮。
    """
    try:
        return " ｜hash=%s ｜readyState=%s ｜屏上=%s" % (
            b.js("location.hash"), b.js("document.readyState"),
            str(b.js("document.body.innerText.slice(0, 160)")))
    except Exception as e:
        return " ｜连现场都没取到：%s" % e


def main():
    os.makedirs(OUT, exist_ok=True)
    b = Browser(width=W, height=900 if W > 560 else 844)
    try:
        # 顺序必须是"先落到面板这个源上，再清 sessionStorage"。
        # 之前是 removeItem 在前、goto 在后：标签页是跨脚本复用的，进来时它可能停在
        # about:blank 或别的源上，那一下清掉的是别人的存储，面板自己那份登录态还在，
        # reload 直接进了总览页，于是报"登录层没出来"——而它自己打印的现场里整个侧栏都在。
        # 整轮里那些"上一个脚本一红，下一个跟着红"的连带失败，根就在这儿。
        b.goto(BASE + "/#/")
        if not b.wait("location.origin === %s" % json.dumps(BASE)):
            raise SystemExit("没落到面板这一页" + page_note(b))
        b.js("sessionStorage.removeItem('dstpanel.creds')")
        b.reload()
        if not b.wait("!!document.querySelector('.login-form')"):
            raise SystemExit("登录层没出来" + page_note(b))
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.js(HOOK)
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("带凭据重进还是停在登录层" + page_note(b))

        b.js("location.hash = '#/mods'")
        if not b.wait("document.querySelectorAll('.tabs button').length > 0"):
            raise SystemExit("模组页没出来：" + str(b.js("document.body.innerText.slice(0,200)")))
        time.sleep(0.8)
        b.js(HOOK)

        # —— 一、三步就是三步，顺序写在脸上 ——
        t = tabs_state(b)
        check("模组页只剩三个页签（一致性不再占一级）", t["n"] == 3, t["texts"])
        check("页签里不再出现「一致性」这种一级入口",
              not [x for x in t["texts"] if "一致性" in x], t["texts"])
        # 标签文字只在这一次核对；下面点页签一律走序号（见 click_tab）。
        check("三步的顺序还是 1 找 → 2 看这台机器上有什么 → 3 管这个房间的开关",
              t["steps"] == ["1", "2", "3"] and "找模组" in t["texts"][0]
              and "这台机器上的文件" in t["texts"][1] and "这个房间的开关" in t["texts"][2],
              "%s / 序号 %s" % (t["texts"], t["steps"]))
        check("打开页面就落在第 1 步（小白动线的起点）", t["onStep"] == "1" and "找模组" in t["on"], t["on"])
        check("页签这一排没把页面撑出横向滚动", t["overflow"] <= 1, "overflow=%s" % t["overflow"])

        # —— 二、一致性降级成第 3 步里的一句 ——
        if not click_tab(b, 3):
            raise SystemExit("第 3 步点不动")
        # 一进来就得有一个世界是选中的。这里曾经停在"Master、Caves 两颗都在、一颗没选中"，
        # 整栏既不转圈也不报错，就是一行模组都没有——因为当前世界只在"房间变了"的侦听里赋值，
        # 而面板是 SPA，从别的页切进来时房间早就选好了，那个侦听一次也不会触发。
        time.sleep(0.5)
        seg = b.js(r"""(() => {
          const bs = [...document.querySelectorAll('.seg button')];
          const on = bs.find((x) => x.classList.contains('on'));
          const t = [...document.querySelectorAll('.tabs button')].find(
            (x) => { const s = x.querySelector('.tab-step'); return s && s.textContent.trim() === '3'; });
          const c = t ? t.querySelector('.tab-count') : null;
          return { n: bs.length, on: on ? on.textContent.trim() : '',
                   badge: c ? c.textContent.trim() : '' };
        })()""")
        check("第 3 步一进来就有一个世界被选中（不用先点一下 Master）",
              seg["n"] >= 1 and bool(seg["on"]), seg)
        check("页签上那颗计数跟着变成同一个世界名",
              seg["badge"] == seg["on"] and seg["badge"] != "", seg)
        if not b.wait("!!document.querySelector('.mod-line')"):
            raise SystemExit("第 3 步没有模组行：" + str(b.js("document.body.innerText.slice(0,240)")))
        time.sleep(0.8)
        strip = b.js(r"""(() => {
          const cs = [...document.querySelectorAll('section.card')];
          const c = cs.find((x) => (x.innerText || '').indexOf('两个世界的模组规则') >= 0);
          if (!c) return null;
          return { text: c.innerText.replace(/\s+/g, ' ').trim().slice(0, 160),
                   btns: [...c.querySelectorAll('button')].map((x) => x.textContent.trim()),
                   // 收起态不许把整张差异表、那段引擎说明一起摊开：那才是它原来占一级 tab 的样子。
                   long: (c.innerText || '').indexOf("引擎只在地表与洞穴之间同步") >= 0 };
        })()""")
        check("第 3 步底下有一句跨世界状态（默认收起，只给一句）",
              bool(strip) and "看细节" in (strip or {}).get("btns", []) and not strip["long"],
              (strip or {}).get("text") or "没找到那一句")
        check("那一句带着结论性的数字或\"一样\"，不是光一个标题",
              bool(strip) and any(k in strip["text"] for k in ("一样", "处不一样", "核对中", "没对上")),
              (strip or {}).get("text", "")[:90])
        if not click_btn(b, "section.card button", "看细节"):
            raise SystemExit("「看细节」点不动")
        time.sleep(1.2)
        full = b.js(r"""(() => {
          const cs = [...document.querySelectorAll('section.card')];
          const c = cs.find((x) => (x.innerText || '').indexOf('两个世界是不是同一套模组规则') >= 0);
          if (!c) return null;
          return { btns: [...c.querySelectorAll('button')].map((x) => x.textContent.trim()),
                   rows: c.querySelectorAll('.mod-row').length,
                   hasNote: (c.innerText || '').indexOf("引擎只在地表与洞穴之间同步") >= 0 };
        })()""")
        check("点开细节才有那张差异表（含\"去某个世界改\"那种按钮位）",
              bool(full) and "收起成一句" in (full or {}).get("btns", []) and full["hasNote"],
              (full or {}).get("btns"))
        if not click_btn(b, "section.card button", "收起成一句"):
            raise SystemExit("「收起成一句」点不动")
        time.sleep(0.6)
        back = b.js("(() => { const c = [...document.querySelectorAll('section.card')]"
                    ".find((x) => (x.innerText || '').indexOf('两个世界的模组规则') >= 0);"
                    " return c ? c.innerText.replace(/\\s+/g, ' ').trim().slice(0, 120) : null; })()")
        check("收回去还是那一句（这一进一出没把组件搞坏）", bool(back) and "看细节" in back, back)

        # —— 三、没保存就想切走：必须问，且两个按钮各管各的 ——
        # 造一次"改了没保存"要真去改一个下拉。哪一行有下拉可改是夹具决定的
        # （助手模组没有配置项），所以一行一行开抽屉试，开到有下拉的那行为止。
        changed = "no-drawer"
        for _ in range(6):
            opened = b.js(r"""(() => {
              const bs = [...document.querySelectorAll('.mod-line .btn')]
                .filter((x) => x.textContent.indexOf('配置') >= 0 && x.textContent.indexOf('收起') < 0);
              if (!bs.length) return false;
              bs[0].click(); return true;
            })()""")
            if not opened:
                break
            b.wait("!document.querySelector('.drawer .sk')", tries=60)
            time.sleep(0.3)
            changed = b.js(r"""(() => {
              const ds = [...document.querySelectorAll('.drawer')];
              const d = ds.length ? ds[ds.length - 1] : null;
              const s = d ? d.querySelector('select') : null;
              if (!s || s.options.length < 2) return 'no-select';
              s.value = s.options[s.selectedIndex === 0 ? 1 : 0].value;
              s.dispatchEvent(new Event('change', { bubbles: true }));
              return 'ok:' + s.value;
            })()""")
            if changed.startswith("ok:"):
                break
        if not changed.startswith("ok:"):
            check("夹具里至少有一行模组带着能改的下拉配置项", False, changed)
        time.sleep(0.4)
        d1 = dirty_marks(b)
        check("改一项配置，页签上的按钮立刻写\"已改 1\"（改动看得见）",
              changed.startswith("ok:") and d1["marks"] == 1 and d1["sum"] == 1,
              "%s / %s" % (changed, d1))

        click_tab(b, 1)
        time.sleep(0.5)
        dlg = dialog(b)
        check("带着没保存的改动切走会先问一句", bool(dlg) and "还没保存" in (dlg or {}).get("body", ""),
              str(dlg)[:200])
        check("那一句里两个按钮说的是人话（丢掉 / 留下，不是确定取消）",
              bool(dlg) and any("丢掉" in x for x in dlg["btns"]) and any("留在" in x for x in dlg["btns"]),
              (dlg or {}).get("btns"))
        click_btn(b, ".mask .dialog button", "留在这一步")
        time.sleep(0.6)
        t2 = tabs_state(b)
        d2 = dirty_marks(b)
        check("点\"留在这一步\"就真没切走，改动还在",
              t2["onStep"] == "3" and d2["sum"] == 1 and dialog(b) is None,
              "%s / %s" % (t2["on"], d2))

        click_tab(b, 1)
        time.sleep(0.5)
        click_btn(b, ".mask .dialog button", "丢掉")
        time.sleep(0.8)
        t3 = tabs_state(b)
        check("点\"丢掉改动，切过去\"才真切走", t3["onStep"] == "1" and dialog(b) is None, t3["on"])
        # 切回来必须真的是那张空表单：只把组件藏起来（v-show）的话，"已改 1" 会还在。
        click_tab(b, 3)
        time.sleep(1.2)
        d3 = dirty_marks(b)
        drawer_open = b.js("!!document.querySelector('.drawer')")
        check("切回来时那份改动确实没了（组件是真被卸了，不是藏起来）",
              d3["sum"] == 0 and drawer_open is False, "%s / 抽屉还开着=%s" % (d3, drawer_open))

        # —— 四、来回走一遍不许漏 ——
        for step in (2, 1, 3, 2):
            if not click_tab(b, step):
                check("切到第 %s 步点得动" % step, False, "序号为 %s 的页签找不到" % step)
                break
            time.sleep(0.7)
        else:
            ok4 = tabs_state(b)
            check("三步来回点四趟，最后落在点的那一步", ok4["onStep"] == "2", ok4["on"])
        b.shoot(os.path.join(OUT, "mods_flow.png"))

        errs = b.js("window.__perr || []")
        check("这一路没有未捕获异常", not errs, " ｜ ".join(errs)[:300])
    finally:
        b.close()

    print("\n合计 %d 条，失败 %d 条" % (len(REPORT), len(FAILED)))
    for f in FAILED:
        print("失败：" + f)
    sys.exit(1 if FAILED else 0)


if __name__ == "__main__":
    main()
