#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""模组检索页这一屏的浏览器实测：分类那一排、星星那两格、封面图到底裁没裁。

和 ui_lobby_check.py 同一个套路：只读真正渲染出来的 DOM，不看 Vue 内部状态——
接口再对，卡片上的句子写错了也还是错的。

每一「点一下」之后都轮询到界面不再加载为止再读：在线那一路要真打 Steam，
本机链路上几秒到几十秒都有，读早了会把还在转的一屏读成"空列表"，
那是探针的错，不是页面的错（上一版就栽在这儿）。
"""
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.stdout.reconfigure(encoding="utf-8", errors="replace")
from cdp import Browser  # noqa: E402

BASE = "http://127.0.0.1:8963"
OUT = os.path.join(HERE, "tmp", "mods74")
DESKTOP = (1440, 900)
MOBILE = (390, 844)

# 裁没裁只看一件事：object-fit 必须是 contain。cover 才会把图切掉；
# 而 contain 下 img 的 clientWidth/Height 恒等于盒子（那是元素尺寸不是画幅），
# 拿它和原图比例比永远比不出结论——上一版就是这么误报"还在裁"的。
READ = r"""(() => {
  const vis = (e) => !!e && e.offsetParent !== null;
  const cats = [...document.querySelectorAll('.mod-cat')].map((b) => ({
    t: b.textContent.trim().replace(/\s+/g, ''),
    on: b.classList.contains('on'),
    ti: b.getAttribute('title') || '',
  }));
  const cards = [...document.querySelectorAll('.mod-card')].map((c) => {
    const cover = c.querySelector('.mod-cover');
    const img = c.querySelector('.mod-cover-img');
    const cr = cover ? cover.getBoundingClientRect() : null;
    return {
      name: ((c.querySelector('.mod-card-name') || {}).textContent || '').trim().slice(0, 22),
      box: cr ? Math.round(cr.width) + 'x' + Math.round(cr.height) : 'no-box',
      ratio: cr ? +(cr.width / cr.height).toFixed(2) : null,
      nat: img && img.complete ? img.naturalWidth + 'x' + img.naturalHeight : 'no-img',
      fit: img ? getComputedStyle(img).objectFit : '',
      rate: [...c.querySelectorAll('.mod-rate span')].map((s) =>
        s.textContent.trim() + '⟨' + (s.getAttribute('title') || '') + '⟩'),
      heat: ((c.querySelector('.mod-heat-num') || {}).textContent || '').trim(),
      hasRate: !!c.querySelector('.mod-rate'),
    };
  });
  return {
    cats: cats,
    lab: [...document.querySelectorAll('.mod-cats .lab')].map((n) => n.textContent.trim()),
    nCards: cards.length,
    cards: cards.slice(0, 5),
    note: [...document.querySelectorAll('.note.info, .note.warn')].map((n) => n.textContent.trim()).filter(Boolean),
    pager: ((document.querySelector('.pager') || {}).textContent || '').replace(/\s+/g, ' ').trim(),
    sk: vis(document.querySelector('.sk')),
    err: [...document.querySelectorAll('.err-box')].map((x) => x.textContent.trim()),
    errs: (window.__errs || []).slice(0, 6),
    overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
  };
})()"""

fails = []
# 这一轮"没验到"的东西单列一笔：链路不通时界面照样有结果（缓存那一路是真的），
# 只按 FAIL 会把"Steam 没连上"和"页面写错了"混成一件事。
misses = []


def check(name, ok, detail=""):
    print("  %s  %s%s" % ("ok  " if ok else "FAIL", name, ("  ｜" + detail) if detail else ""))
    if not ok:
        fails.append(name + ("（" + detail + "）" if detail else ""))


def submit(b, q):
    """在界面上真的填框并按那颗「搜」——不走内部方法，验的就是用户那条路。"""
    return b.js("""(() => {
      const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
      const i = document.querySelector('input[type=search]');
      if (!i) return '没有搜索框';
      set(i, %s);
      const btn = [...document.querySelectorAll('.card button')].find((x) => x.textContent.trim() === '搜');
      if (btn) btn.click();
      return btn ? '已提交搜索' : '没有搜到那颗按钮';
    })()""" % json.dumps(q))


def say(t):
    print("  ·  " + t)


def settle(b, tries=150, pause=0.6):
    last = None
    for _ in range(tries):
        last = b.js(READ)
        if last and not last.get("sk") and not last.get("err"):
            return last, True
        time.sleep(pause)
    return last, False


def is_live(d):
    """这一屏到底是不是在线结果，只认说明里那句在线成功的原文。

    上一版在这里栽过一次：链路上不通那一轮，界面按承诺退回缓存，卡片照样有 12 张，
    于是"在线那一屏的分类口径"两条断言被拿去过一屏缓存，报成 FAIL。
    缓存屏写"本地缓存里「人物」有 N 条"是对的，不是 bug。
    """
    return any("结果来自 Steam 在线检索" in n for n in (d.get("note") or []))


def live_problem(d):
    for n in d.get("note") or []:
        if "在线检索没成功" in n:
            return n.split("。")[0]
    return ""


def dump(d, tag=""):
    print("--- %s：分类 %d 枚、卡片 %d 张、还在加载=%s、接口错=%s ---" % (
        tag, len(d.get("cats") or []), d.get("nCards"), d.get("sk"), d.get("err")))
    for c in d.get("cats") or []:
        print("    [%s] %s%s" % ("x" if c["on"] else " ", c["t"], (" title=%r" % c["ti"]) if c["ti"] else ""))
    for n in d.get("note") or []:
        if "模组" in n or "分类" in n or "缓存" in n or "工坊" in n:
            print("    说明：%s" % n[:190])
    print("    分页：%s" % d.get("pager"))
    for c in d.get("cards") or []:
        print("    %s | 盒 %s 比 %s | 原图 %s | fit=%s | 订阅 %s | 星星 %s" % (
            c["name"], c["box"], c["ratio"], c["nat"], c["fit"], c["heat"], c["rate"]))


def main():
    os.makedirs(OUT, exist_ok=True)
    b = Browser(*DESKTOP)
    b.goto(BASE + "/?cdp=%d#/mods" % int(time.time()))
    if b.wait("!!document.querySelector('.login-form')", tries=20, pause=0.25):
        b.js("""(() => {
          const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
          const f = document.querySelector('.login-form');
          set(f.querySelector('input[type=text], input:not([type])'), 'admin');
          set(f.querySelector('input[type=password]'), 'admin');
          sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}));
          f.querySelector('.login-submit').click();
        })()""")
    if not b.wait("!!document.querySelector('.mod-cats')", tries=90, pause=0.4):
        b.shoot(os.path.join(OUT, "boot_failed.png"))
        raise SystemExit("检索页没出分类排：%s" % (b.js("document.body.innerText.slice(0,300)") or ""))
    b.js("window.__errs = [];addEventListener('error',(e)=>window.__errs.push(String(e.message||e.error)));"
         "addEventListener('unhandledrejection',(e)=>window.__errs.push('promise:'+String(e.reason)));")

    # —— 1. 默认屏：没关键词、没分类，走的是本地缓存 ——
    d, ok = settle(b)
    dump(d, "默认屏（不限分类、无关键词）")
    check("分类排画全 15 枚（不限分类 + 14 类）", ok and len(d["cats"]) == 15, str(len(d["cats"])))
    check("默认选中「不限分类」", ok and d["cats"] and d["cats"][0]["on"])
    check("默认屏没有 JS 报错", ok and not d["errs"], json.dumps(d["errs"], ensure_ascii=False))
    check("分类数字的口径写的是本地缓存", ok and all("本地缓存里" in c["ti"] for c in d["cats"][1:]),
          (d["cats"][1] or {}).get("ti", ""))
    check("封面盒是正方形（工坊预览图本来就是方的）",
          ok and d["cards"] and all(c["ratio"] == 1 for c in d["cards"] if c["ratio"]),
          json.dumps([c["box"] for c in d["cards"]]))
    check("封面完整显示不裁图（object-fit: contain）",
          ok and all(c["fit"] == "contain" for c in d["cards"] if c["nat"] != "no-img"),
          json.dumps([c["fit"] for c in d["cards"]]))
    b.shoot(os.path.join(OUT, "01_default.png"))

    # —— 2. 在线那一路：给关键词，等真结果，看星星 ——
    say(submit(b, "stack"))
    time.sleep(1.0)
    d2, ok2 = settle(b)
    # 本机到 api.steampowered.com 的链路是出了名的抖（这一轮就断了三次里两次）。
    # 断了界面会照承诺退回缓存，那一屏该按缓存屏验；但"在线那一路"本身必须重按几次，
    # 不然这一轮就等于没验到用户要的那件事。
    tries = 1
    while ok2 and not is_live(d2) and tries < 4:
        say("第 %d 次没走通在线：%s，重按一次" % (tries, live_problem(d2)))
        time.sleep(4.0)
        say(submit(b, "stack"))
        time.sleep(1.0)
        d2, ok2 = settle(b)
        tries += 1
    dump(d2, "在线关键词 stack")
    if not is_live(d2):
        # 链路与否不是页面的属性：这一屏退回缓存、并且把退回了什么写在说明里，
        # 本身就是对的行为（有单测盯着）。所以这里记"没验到"，不记"不过"——
        # 记成不过会让人以为界面坏了，而真正没验的是在线那一路。
        misses.append("在线那一路这一轮四次都没走通（%s）：分类口径的在线措辞、在线卡片星星都没复验到，"
                      "等链路恢复再跑一遍" % live_problem(d2))
        say("这一轮在线链路不通，界面按承诺退回缓存（下面按缓存屏的口径验）")
    check("在线那一路出结果了", ok2 and (is_live(d2) or d2["nCards"] > 0),
          "%d 张 ｜live=%s ｜%s" % (d2["nCards"], is_live(d2), d2["err"]))
    starred = [c for c in d2["cards"] if c["hasRate"]]
    say("卡片里带星星那一格的：%d / %d" % (len(starred), d2["nCards"]))
    for c in starred[:4]:
        print("      %s → %s" % (c["name"], c["rate"]))
    check("在线卡片画出了收藏数或工坊评分", ok2 and len(starred) > 0)
    check("在线屏没有 JS 报错", ok2 and not d2["errs"], json.dumps(d2["errs"], ensure_ascii=False))
    b.shoot(os.path.join(OUT, "02_online.png"))

    # —— 3. 在线 + 分类：点一枚带数字的 ——
    say("点了分类：" + b.js("""(() => {
      const bs = [...document.querySelectorAll('.mod-cat')].filter((x) => !/不限分类/.test(x.textContent));
      const t = bs.find((x) => x.querySelector('.n')) || bs[0];
      if (!t) return '没有分类按钮';
      const label = t.textContent.trim();
      t.click();
      return label;
    })()"""))
    time.sleep(1.0)
    d3, ok3 = settle(b)
    dump(d3, "在线 + 分类")
    check("点分类后选中态跟着走", ok3 and any(c["on"] and "不限" not in c["t"] for c in d3["cats"]),
          json.dumps([c["t"] for c in d3["cats"] if c["on"]], ensure_ascii=False))
    # 口径断言按这一屏真的是什么来分：在线屏说"这次从工坊取回的一批"，
    # 退回缓存的屏说"本地缓存里"。拿在线的句子去考缓存屏是探针写错了，不是页面写错了
    # （上一版就是这么报出两条假 FAIL 的）。
    scoped = "这次从工坊取回" if is_live(d3) else "本地缓存里"
    check("分类数字的口径和这一屏真的取的是哪一批一致",
          ok3 and all(scoped in c["ti"] for c in d3["cats"][1:]),
          "%s ｜%s" % (scoped, (d3["cats"][1] or {}).get("ti", "")))
    if is_live(d3):
        check("说明里写清了分类是面板筛的", any("不认标签参数" in n for n in d3["note"]),
              json.dumps((d3["note"] or [""])[-1], ensure_ascii=False)[:170])
        # 带分类那一屏的"共 N 条"是面板在这一批里筛出来的，说明里不能反过来把它当成工坊总数。
        check("翻页说明不把批次命中数说成工坊总数",
              not any("工坊那边还有更多" in n for n in d3["note"]),
              json.dumps(d3["note"], ensure_ascii=False)[:220])
    check("翻页停在第 1 页", ok3 and "第 1 页" in d3["pager"], d3["pager"])
    b.shoot(os.path.join(OUT, "03_tagged.png"))

    # —— 4. 详情层：封面形状 + 收藏/评分那两行 ——
    b.js("""(() => { const btn = [...document.querySelectorAll('.mod-card-foot button')]
             .find((x) => /详细信息/.test(x.textContent)); if (btn) btn.click(); return btn ? 1 : 0; })()""")
    time.sleep(1.0)
    kv = b.js("""(() => {
      const d = document.querySelector('.mod-detail');
      if (!d) return {none: '详情层没开'};
      const c = d.querySelector('.mod-detail-cover');
      const i = d.querySelector('.mod-detail-cover img');
      const r = c ? c.getBoundingClientRect() : null;
      return {
        txt: d.textContent.replace(/\\s+/g, ' ').slice(0, 600),
        box: r ? (Math.round(r.width) + 'x' + Math.round(r.height)) : 'no-cover',
        ratio: r ? (r.width / r.height) : 0,
        fit: i ? getComputedStyle(i).objectFit : '',
        nat: i && i.complete ? i.naturalWidth + 'x' + i.naturalHeight : 'no-img',
      };
    })()""")
    say("详情层：%s" % json.dumps(kv, ensure_ascii=False)[:760])
    check("详情层封面也是正方形", abs(float(kv.get("ratio") or 0) - 1) < 0.02,
          "%s 比 %s" % (kv.get("box"), kv.get("ratio")))
    check("详情层也完整显示封面", kv.get("fit") == "contain", str(kv.get("fit")))
    check("详情层写了收藏数", "收藏" in str(kv.get("txt", "")))
    b.shoot(os.path.join(OUT, "04_detail.png"))
    b.js("(() => { const x = document.querySelector('.mod-detail .icon-btn'); if (x) x.click(); return 1; })()")
    time.sleep(0.6)

    # —— 5. 窄屏：分类排横向滚动，不许把页面撑出横向溢出 ——
    b.set_viewport(*MOBILE)
    time.sleep(1.2)
    d4, _ = settle(b, tries=40)
    mob = b.js("""(() => {
      const c = document.querySelector('.mod-cats');
      if (!c) return {none: '没有分类排'};
      const r = c.getBoundingClientRect();
      const chip = document.querySelector('.mod-cat');
      return {w: Math.round(r.width), scrollW: c.scrollWidth, wrap: getComputedStyle(c).flexWrap,
              canScroll: c.scrollWidth > Math.round(r.width) + 1, h: Math.round(r.height),
              chipH: chip ? Math.round(chip.getBoundingClientRect().height) : 0};
    })()""")
    say("窄屏分类排：%s" % json.dumps(mob, ensure_ascii=False))
    check("窄屏分类排改成横向滚动（不折成七行）", mob.get("wrap") == "nowrap" and mob.get("canScroll"), json.dumps(mob))
    check("窄屏分类按钮按得到（>=34px 高）", mob.get("chipH", 0) >= 34, str(mob.get("chipH")))
    check("窄屏没有横向溢出", d4["overflow"] <= 0, "溢出 %s px ｜卡片 %s 张" % (d4["overflow"], d4["nCards"]))
    dump(d4, "窄屏（390）")
    b.shoot(os.path.join(OUT, "05_mobile.png"))
    b.close()

    print("\n%d 条不过" % len(fails))
    for f in fails:
        print("  - " + f)
    if misses:
        print("%d 条这一轮没验到（不是页面错，是没打到那条路）" % len(misses))
        for m in misses:
            print("  ! " + m)
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()
