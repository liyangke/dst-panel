#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""天数换算的界面验收：引擎没有 state.day，第几天由面板从 cycles 现算，这里看画出来的那个数。

为什么要在浏览器里跑：这条链的三段（助手文件 → Go 换算 → 界面文案）各自都能"看着对"，
而上一版的 bug 恰好长在中间那段的接缝上——协议里根本没有 day 这个键，Go 拿到零值 0，
界面老老实实把 0 摆成「第 0 天」。接口探针已经钉了响应那头，这里钉的是人眼这一头。

夹具是 .tools/ui_dev.py 写的那份：地表 cycles=20 → 第 21 天，洞穴 cycles=19 → 第 20 天；
这一季过完 5 天、还剩 14 天（秋天的 20 天里第 6 天）。
"""
import os
import sys
import time

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from cdp import Browser, hover  # noqa: E402

BASE = os.environ.get("DAY92_BASE", "http://127.0.0.1:8963")
# 期望的那个天数：本地夹具是地表 cycles=20 → 第 21 天；云机是真实世界，给 cycles+1。
WANT = os.environ.get("DAY92_DAY", "21")
SHOTS = os.environ.get("DAY92_SHOTS") or os.path.join(HERE, "tmp", "day92")
DESKTOP = (int(os.environ.get("DAY92_W", 1440)), int(os.environ.get("DAY92_H", 900)))

READ = r"""(() => {
  const txt = (el) => (el.innerText || '').replace(/\s+/g, ' ').trim();
  const body = txt(document.body);
  // 凡是对页面上"看得见"的文本收天数，不挑容器：天数进度是 .fact、总览是 .chip、
  // 房间表是 td，挨个写选择器迟早漏掉一个，而这条链要证的恰恰是"画出来的那个数"。
  // innerText 本身就只给可见文本，隐藏节点不会混进来。
  return {
    hash: location.hash,
    dayBits: [...new Set(body.match(/第\s*\d+\s*天/g) || [])],
    hasZeroDay: /第\s*0\s*天/.test(body),
    zeroWhere: (body.match(/.{0,40}第\s*0\s*天.{0,40}/) || [''])[0],
    // 季节那一枚：文案是「秋天（第 6 天）」，把世界年龄和季节天数分开摆。
    // 悬停那句是原生 title，不是要点开的气泡，所以直接读属性。
    seasonChips: [...new Set([...document.querySelectorAll('.chip')]
      .map((el) => ({t: (el.innerText || '').replace(/\s+/g, ' ').trim(),
                     h: el.getAttribute('title') || ''}))
      .filter((x) => /^(春天|夏天|秋天|冬天)/.test(x.t)))],
    // 溢出只对内容区量，整页会把侧栏宽度也算进来。
    overflow: (() => {
      const el = document.querySelector('.main') || document.documentElement;
      return el.scrollWidth - el.clientWidth;
    })(),
    pageErrors: (window.__errs || []).slice(0, 5),
    // 收空了要说得出原因：是页面没起来、还是那一页本来就不报天数。
    notFound: /这个地址没有对应的页面/.test(body),
    shellReady: !!document.querySelector('.nav-item.on'),
  };
})()"""

fails = []

# 本地夹具里地表 cycles=20（第 21 天）、洞穴 cycles=19（第 20 天），
# 所以"只剩洞穴在回话"这件事能直接从画出来的数字上看见，不用去点开那枚问号。
MASTER_SNAP = os.path.join(os.path.dirname(HERE), ".tmp_boot",
                           "klei", "Cluster_1", "Master", "save", "dstpanel_status.json")

LOGIN_JS = r"""(() => {
  const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
  const f = document.querySelector('.login-form');
  if (!f) return false;
  set(f.querySelector('input[type=text], input:not([type])'), 'admin');
  set(f.querySelector('input[type=password]'), 'admin');
  sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}));
  f.querySelector('.login-submit').click();
  return true;
})()"""


def day_bits(b, timeout=14.0):
    """等页面把天数画出来，返回那串「第 N 天」。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        got = b.js(READ)
        if got and got["dayBits"]:
            return got
        time.sleep(0.4)
    return got


FIND_BTN = """(() => {
  const f = [...document.querySelectorAll('.fact')]
    .find((el) => (el.innerText || '').includes('天数进度'));
  const btn = f && f.querySelector('.tip-btn');
  if (!btn) return null;
  btn.scrollIntoView({block: 'center'});   // 悬停按视口坐标发，元素在折外就永远移不到它身上
  const r = btn.getBoundingClientRect();
  return {x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2)};
})()"""

CLICK_BTN = """(() => {
  const f = [...document.querySelectorAll('.fact')]
    .find((el) => (el.innerText || '').includes('天数进度'));
  const btn = f && f.querySelector('.tip-btn');
  if (!btn) return false;
  btn.click();
  return true;
})()"""

READ_HINT = """(() => {
  const f = [...document.querySelectorAll('.fact')]
    .find((el) => (el.innerText || '').includes('天数进度'));
  const bub = f && f.querySelector('.tip-bub');
  return bub ? (bub.innerText || '').replace(/\\s+/g, ' ').trim() : '';
})()"""


def day_hint(b):
    """把「天数进度」那枚问号展开，读回气泡原文。

    展开走真鼠标悬停，十二次都没开才退到点一下（那是触屏用户的正路）。
    原来这里写的是 `btn.focus()` 并注明"比合成 mouseenter 可靠"——2026-11-05 换装到云机
    那一轮它就是被这句红掉的：headless Chrome 在文档本身还没拿到焦点时只改 activeElement、
    不发 focus 事件，气泡（v-if）永远不渲染，于是两条判据都读回空串，本地两种开法都正常
    所以看不见。悬停与点击都不开时返回空串，让"展不开"自己报一条红。
    """
    at = b.js(FIND_BTN)
    if not at:
        return ""
    for _ in range(12):
        hover(b, at["x"], at["y"])
        time.sleep(0.25)
        got = b.js(READ_HINT)
        if got:
            return got
    if b.js(CLICK_BTN):
        b.wait(READ_HINT, tries=12, pause=0.2)
        return b.js(READ_HINT) or ""
    return ""


def caves_only_day(b):
    """把地表那份快照挪开，房间页那一格不能再报地表的天数。"""
    if not os.path.exists(MASTER_SNAP):
        check("地表夹具在（这条要它）", False, MASTER_SNAP)
        return False
    aside = MASTER_SNAP + ".aside-by-check"
    os.replace(MASTER_SNAP, aside)
    try:
        b.goto(BASE + "/?d92c=%d#/room" % int(time.time()))
        if b.wait("!!document.querySelector('.login-form')", tries=20, pause=0.3):
            b.js(LOGIN_JS)
        b.wait("!!document.querySelector('.fact')", tries=40, pause=0.3)
        got = day_bits(b)
        bits = (got or {}).get("dayBits") or []
        hint = day_hint(b)
        print("  ·  地表快照挪开后 #/room 画出来的天数：%s ｜说明：%s" % (bits, hint))
        b.shoot(os.path.join(SHOTS, "room-caves-only.png"))
        check("地表挪开后那枚问号也展得开", bool(hint),
              "悬停与点击两条路都没让气泡出现" if not hint else hint[:40])
        # 说明那句也要跟着数字一起换：数字是洞穴的，气泡却还在说"取地表这一侧"，
        # 那就是界面在替一个它没读到的东西背书。
        check("只洞穴回话时那句说明也跟着换（说的是地表没回话）",
              "地表" in hint and "还没回话" in hint, hint[:120])
        return ("第 20 天" in bits) and ("第 21 天" not in bits)
    finally:
        os.replace(aside, MASTER_SNAP)


def check(name, ok, detail=""):
    print("  %s  %s%s" % ("ok  " if ok else "FAIL", name, ("  ｜" + detail) if detail else ""))
    if not ok:
        fails.append(name + ("（" + detail + "）" if detail else ""))


def main():
    os.makedirs(SHOTS, exist_ok=True)
    print("BASE = %s  视口 %dx%d  期望第 %s 天" % (BASE, DESKTOP[0], DESKTOP[1], WANT))
    b = Browser(*DESKTOP)
    try:
        b.goto(BASE + "/?d92=%d#/" % int(time.time()))
        if b.wait("!!document.querySelector('.login-form')", tries=30, pause=0.3):
            b.js(LOGIN_JS)
        if not b.wait("!!document.querySelector('.tabs, .nav-item')", tries=40, pause=0.3):
            print("FAIL 面板没起来（ui_dev 在跑吗？）")
            return 1
        b.js("window.__errs = [];"
             "addEventListener('error', (e) => window.__errs.push(String(e.message || e.error)));"
             "addEventListener('unhandledrejection', (e) => window.__errs.push('promise:' + String(e.reason)));")

        # 两个会写天数的地方：总览的房间卡（#/）、开服与世界页顶上的"天数进度"事实卡。
        # （原先还有第三处「所有房间」那张表，2026-11-07 那一页整个下掉了，#129。）
        # 路由里总览那一项的 href 是 "/"，写 #/overview 会撞 404 页。
        for page in ("#/", "#/room"):
            b.js("location.hash = %s" % '"%s"' % page)
            b.wait("true", tries=2, pause=0.5)
            got = None
            for _ in range(30):
                got = b.js(READ)
                if got and got["dayBits"]:
                    break
                time.sleep(0.4)
            print("  ·  %s 画出来的天数：%s" % (page, (got or {}).get("dayBits")))
            check("%s 不是 404 页，导航也起来了" % page,
                  got is not None and not got["notFound"] and got["shellReady"],
                  "404=%s nav=%s" % ((got or {}).get("notFound"), (got or {}).get("shellReady")))
            check("%s 上有第几天，且是换算出来的第 %s 天" % (page, WANT),
                  got is not None and any(("第 %s 天" % WANT) in t for t in got["dayBits"]),
                  str((got or {}).get("dayBits"))[:160])
            check("%s 上没有「第 0 天」" % page,
                  got is not None and not got["hasZeroDay"], (got or {}).get("zeroWhere", "")[:120])
            check("%s 没有横向溢出" % page, got is not None and got["overflow"] <= 0,
                  "溢出 %s px" % (got or {}).get("overflow"))
            if page == "#/":
                # 这一季第几天是 #128 新加的那半句话，只活在季节那枚胶囊上。
                chips = (got or {}).get("seasonChips") or []
                print("  ·  总览上季节那枚：%s" % str([(c["t"], c["h"][:30]) for c in chips])[:220])
                check("总览的季节胶囊带上这一季第几天（夹具过完 5 天 → 第 6 天）",
                      any(c["t"] == "秋天（第 6 天）" for c in chips), str(chips)[:180])
                check("悬停说的是还剩几天（夹具还剩 14 天）",
                      any("还剩 14 天" in c["h"] for c in chips),
                      str([c["h"] for c in chips])[:220])
            if page == "#/room":
                # 两个世界都在回话时，那一格该说取地表；这句和下一条是同一处文案的两面。
                hint = day_hint(b)
                print("  ·  #/room 天数进度的说明：%s" % hint)
                # "读不到"和"读到了但不对"是两种红：少了这一条，气泡没展开只会表现成
                # 下面那句"说明不对"，读报告的人跑去改文案，真要修的是展开那条链路。
                check("那枚问号展得开（读回的是气泡原文，不是空串）", bool(hint),
                      "悬停与点击两条路都没让气泡出现" if not hint else hint[:40])
                check("两边都回话时那句说明指的是地表", "取地表" in hint, hint[:120])
            b.shoot(os.path.join(SHOTS, (page.lstrip("#/") or "overview") + ".png"))

        errs = b.js("(window.__errs || []).slice(0,5)") or []
        check("整轮下来页面没有抛错", not errs, " ; ".join(errs))

        # 只有洞穴在回话时，那一格不能还说"取地表这一侧"。
        # 这条只在本地跑：它要把 Master 那份快照挪开，云机上的文件一个都不许碰。
        if "127.0.0.1" in BASE or "localhost" in BASE:
            check("只洞穴回话时，那一格换成洞穴的天数", caves_only_day(b))
    finally:
        b.close()
    print("\n%d 条不过" % len(fails))
    for f in fails:
        print("  - " + f)
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
