# -*- coding: utf-8 -*-
"""工坊检索那一页的分页与卡片：靠点来验，不靠截图。

为什么要单独写：用户报的第 1 条就是"点下一页没有效果"。上一轮改完只看了截图，
截图只能证明"第一页长这样"，证明不了第二页换了内容。这一次每一页都真的点过去，
并且比对两页的卡片名有没有重叠——重叠了才说明 @go 根本没接上。

本机可能是在线模式也可能是缓存模式（取决于 .tmp_boot/steam_key.txt 在不在），
两种模式下"翻页要换内容"都成立，所以那几条断言两边都跑；
只有"共 N 条来自缓存计数"这类需要已知数据集的，才在种了假条目的缓存模式下验。

种进去的 30 条模组写的是本地那份 ui_dev 库（.tmp_boot/uiwork/panel），
不动探针自己的库，末尾按 ID 删干净。

用法：先起着 python .tools/ui_dev.py，再 python .tools/ui_mods.py
"""
import base64
import json
import math
import os
import re
import sqlite3
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
sys.stderr.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DB = os.path.join(ROOT, ".tmp_boot", "uiwork", "dstpanel.db")
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tmp", "shots")

# 9 位纯数字，形状和真工坊 ID 一致；用 99 开头这一段基本撞不上真东西。
SEED_PREFIX = "9900000"
IDS = ["%s%02d" % (SEED_PREFIX, i) for i in range(10, 40)]        # 30 条，够翻 3 页
RARE = ["%s90" % SEED_PREFIX, "%s91" % SEED_PREFIX, "%s92" % SEED_PREFIX]
PAGE = 12                                                          # 和 mods.js 的 size 同一个数

# 30 个严格递减的订阅数，跨 5 个数量级：卡片上那一格用的是有损的 count()，
# 只有数量级拉开了，"条长跟着条上数字走"才是真在比对，而不是比一堆同一个字符串。
SPREAD = [90_000_000, 20_000_000, 8_500_000, 3_200_000, 1_950_000, 950_000,
          480_000, 210_000, 96_000, 52_000, 31_000, 18_000, 9_900, 5_400,
          2_600, 1_200, 640, 300, 160, 90, 55, 33, 21, 13, 8, 5, 3, 2, 1, 1]
assert len(SPREAD) == len(IDS), "SPREAD 要和 IDS 同长，不然种下去的行数对不上"

results = []


def check(name, ok, detail=""):
    results.append((bool(ok), name, detail))
    print("%s %s%s" % ("PASS" if ok else "FAIL", name, ("  | " + str(detail)[:220]) if detail else ""))


# ---- 条上那一格的"读法"，与 fmt.js 的 count() / num() 逐字对齐 ----
#
# 为什么不直接从 DOM 抠数字比对：count() 是有损的，"0.9 亿"和接口给的 89,999,900
# 拿数字是撞不上的。要验"界面没说谎"，就得把界面那套舍入在这里重做一遍，
# 然后要求两边字符串一模一样——舍入方向、单位、空格都得对上。
def jsround(x):
    """JS 的 Math.round：正数四舍五入。Python 的 round 是银行家舍入（10.5 → 10），
    直接用会让这一轮比对在边界值上随机翻脸。"""
    return math.floor(x + 0.5)


def jsnum(x):
    """按 JS 的方式把 double 打成字符串：整数值不带 .0，小数走最短往返表示。
    Python 3 的 repr(float) 和 Number.toString() 都是最短往返，所以两者一致。"""
    return str(int(x)) if float(x).is_integer() else repr(x)


def sig(q):
    """fmt.js 的 sig：折到最多三位有效数字，不留 .0 尾巴。"""
    d = 2 if abs(q) < 10 else 1 if abs(q) < 100 else 0
    f = 10.0 ** d
    return jsround(q * f) / f


def count_text(v):
    """fmt.js 的 count()：一万以内是千分位精确值，往上带单位且最多三位有效数字。"""
    v = int(v)
    a = abs(v)
    if a < 1e4:
        return "{:,}".format(v)
    if a < 1e8:
        return jsnum(sig(v / 1e4)) + " 万"
    return jsnum(sig(v / 1e8)) + " 亿"


def exact_text(subs):
    """heat().exact，也就是悬停标题：num() 的千分位 + " 人订阅"。"""
    return "{:,}".format(int(subs)) + " 人订阅"


def said(s):
    """把条上那一串读回一个数（"9.6 万" → 96000）。
    万/亿必须乘回去：只抠数字的话"1.23 亿"会读成 123，
    "条长跟着数字走"那条就变成了比较谁的字面量短。"""
    m = re.search(r"([\d,]+(?:\.\d+)?)\s*(亿|万)?", s)
    if not m:
        return 0
    v = float(m.group(1).replace(",", ""))
    unit = {"亿": 1e8, "万": 1e4}.get(m.group(2), 1)
    return int(v * unit)


def seed():
    """往缓存里塞 30 条模组（其中 3 条带独特词，用来验搜索后回到第 1 页）。"""
    if not os.path.isfile(DB):
        raise SystemExit("找不到 ui_dev 的库：%s（先跑 python .tools/ui_dev.py）" % DB)
    con = sqlite3.connect(DB, timeout=15)
    cur = con.cursor()
    now = int(time.time() * 1000)
    rows = []
    # 订阅数刻意拉开数量级（近 9 千万一直到几百），因为真实工坊数据就是这么个分布，
    # 而卡片上那一格走的是有损的 count()（"0.9 亿""9.6 万"）。上一版给的是
    # 90,000,000 起步每次减 100，12 张卡全被折成同一个"0.9 亿"，
    # "条长跟着条上数字走"和"条上数字就是接口给的那个"两条一起变成空转。
    # 头几条仍比库里任何真模组都高（Combined Status 约 190 万），
    # 这样"第一页第一张就是我种的"那条断言还立得住。
    for i, wid in enumerate(IDS):
        rows.append((wid, "界面验收模组 %02d" % (i + 1), "这一条是本地界面验收写进去的假模组，跑完就删。",
                     "1.%d" % (i % 4), json.dumps(["验收"], ensure_ascii=False),
                     SPREAD[i], now - i * 86400_000, 200_000 + i * 10_000, "",
                     0, 1 if i % 7 == 0 else 0, 0, "workshop", now, now))
    for j, wid in enumerate(RARE):
        rows.append((wid, "孤卡测试 %d 号" % (j + 1), "只有三条，用来验搜完只剩一页时下一页该禁用。",
                     "2.0", json.dumps(["验收"], ensure_ascii=False),
                     100 - j, now - j * 86400_000, 50_000, "", 0, 0, 0, "workshop", now, now))
    cur.executemany("""
        INSERT INTO mods_seen (workshop_id, name, description, version, tags, subscriptions,
          mod_updated_at, file_size, preview_url, has_children, client_required, server_only,
          origin, first_seen_at, last_seen_at)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(workshop_id) DO UPDATE SET name = excluded.name, subscriptions = excluded.subscriptions""",
                    rows)
    con.commit()
    total = cur.execute("SELECT COUNT(*) FROM mods_seen").fetchone()[0]

    def restore():
        cur.executemany("DELETE FROM mods_seen WHERE workshop_id = ?", [(s,) for s in IDS + RARE])
        con.commit()
        con.close()
    return restore, total


CARDS = r"""(() => {
  const vis = (x) => x.offsetParent !== null;
  const c = [...document.querySelectorAll('.mod-card')].filter(vis);
  const btn = [...document.querySelectorAll('.pager .pg-btn')];
  const chip = [...document.querySelectorAll('.chip')].map((x) => x.textContent.trim());
  return {
    n: c.length,
    names: c.map((x) => ((x.querySelector('.mod-card-name') || {}).textContent || '').trim()),
    // 热度条：条长的像素和条上那个数字都要读，"看着像"不算数。
    heat: c.map((x) => { const h = x.querySelector('.mod-heat'); if (!h) return null;
      const i = h.querySelector('i'); const n = h.querySelector('.mod-heat-num');
      return { w: i ? Math.round(i.getBoundingClientRect().width) : -1,
               cw: Math.round(x.getBoundingClientRect().width),
               tw: Math.round(h.querySelector('.mod-heat-bar').getBoundingClientRect().width),
               pct: i ? (i.style.width || '') : '', t: n ? n.textContent.trim() : '',
               tt: n ? (n.getAttribute('title') || '') : '' }; }),
    withImg: c.filter((x) => x.querySelector('img.mod-cover-img')).length,
    foot: c.length ? [...c[0].querySelectorAll('.mod-card-foot button')].map((x) => x.textContent.trim()) : [],
    pager: (document.querySelector('.pager') || {}).innerText || '',
    pages: [...document.querySelectorAll('.pager .pg-num')].map((x) => x.textContent.trim()),
    on: (document.querySelector('.pager .pg-num.on') || {}).textContent || '',
    prevOff: btn.length ? !!btn[0].disabled : null,
    nextOff: btn.length > 1 ? !!btn[1].disabled : null,
    live: chip.some((t) => t.indexOf('在线结果') >= 0),
    cols: c.length ? getComputedStyle(document.querySelector('.mod-cards')).gridTemplateColumns.split(' ').length : 0,
    overflow: document.documentElement.scrollWidth - window.innerWidth,
    // 手机端那几个按钮的文字是 display:none 的（只留图标），所以"能不能点"
    // 只能看结构和几何，读 innerText 会测到一个假失败。
    vw: window.innerWidth,
    pagerBtns: btn.length,
    pagerBtnsVis: btn.filter(vis).length,
    pagerRight: (() => {
      const p = document.querySelector('.pager');
      return p ? Math.round(p.getBoundingClientRect().right) : -1;
    })(),
  };
})()"""


def cards(b):
    return b.js(CARDS)


def wait_settled(b, tries=140):
    """等这一次检索真的落地（骨架屏撤掉，剩下卡片 / 空状态 / 报错三者之一）。

    带着 key 时线上那一步要先撞到超时才回落到本地缓存：本机现在连不上
    api.steampowered.com，一次要等十几秒。固定 sleep 会把"还在路上"读成
    "搜到了 0 条"，于是后面每一步都跟着假失败——这一轮就栽在这儿。
    """
    return b.wait("!document.querySelector('.sk') && ("
                  "document.querySelectorAll('.mod-card').length > 0 || "
                  "!!document.querySelector('.empty') || !!document.querySelector('.err-box'))",
                  tries=tries)


def api(path):
    """接口侧的期望值：热度条那种"画出来像不像"的判据必须对着原始数字算，
    不能只对着 DOM 自证（DOM 自己跟自己对齐，条长画错了也照样一致）。"""
    req = urllib.request.Request(BASE + path, headers={
        "Authorization": "Basic " + base64.b64encode(b"admin:admin").decode()})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read().decode("utf-8")).get("data") or {}


def click_btn(b, sel, text, exact=False):
    pred = ("(x) => x.textContent.trim() === %s" if exact else
            "(x) => x.textContent.indexOf(%s) >= 0") % json.dumps(text, ensure_ascii=False)
    return b.click("'%s'" % sel, pred)


def set_input(b, sel, value):
    # 只挑当前看得见的这一个：模组页好几个标签里都有 input[type=search]，
    # 按文档顺序取会填到没打开的那个标签上。
    b.js("""(() => {
      const el = [...document.querySelectorAll('%s')].filter((x) => x.offsetParent !== null)[0];
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      setter.call(el, %s);
      el.dispatchEvent(new Event('input', { bubbles: true }));
    })()""" % (sel, json.dumps(value, ensure_ascii=False)))


def open_search_tab(b):
    b.js("location.hash = '#/mods'")
    if not b.wait("document.querySelectorAll('.tabs button').length > 0"):
        raise SystemExit("模组页没出来：" + str(b.js("document.body.innerText.slice(0,200)")))
    time.sleep(0.4)
    if not click_btn(b, ".tabs button", "找模组"):
        raise SystemExit("第 1 步「找模组」点不动")
    if not b.wait("document.querySelectorAll('.mod-card').length > 0"):
        raise SystemExit("卡片一直没出来：" + str(b.js("document.body.innerText.slice(0,260)")))
    time.sleep(0.6)


def main():
    os.makedirs(OUT, exist_ok=True)
    restore, total = seed()
    try:
        b = Browser(width=1440, height=900)
        # 顺序必须是"先落到面板这个源上，再清 sessionStorage"：标签页跨脚本复用，
        # 进来时它可能停在 about:blank 或别的源上，那一下清掉的是别人的存储，
        # 面板自己那份登录态还在，reload 就进不了登录页（同 ui_shots.py 里写死的那条）。
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
            raise SystemExit("带凭据重进还是停在登录层")
        open_search_tab(b)

        p1 = cards(b)
        check("第一页装满 %d 张卡" % PAGE, p1["n"] == PAGE, p1["n"])
        check("每条卡都有订阅或登记的按钮，也有详细信息",
              any("详细信息" in x for x in p1["foot"]) and len(p1["foot"]) >= 2, p1["foot"])
        if p1["live"]:
            print("（这一轮是在线模式：分页条的总数来自接口，缓存计数那两条留给云机那一轮）")
        else:
            # 分页条上的"共 N 条"必须和库里此刻的行数一致：
            # 之前出现过"共 12 条，第 2 页还有内容"，那是总数和列表用了两套 WHERE。
            flat = p1["pager"].replace("\n", " ")
            check("分页条的总数就是库里的行数", ("共 %d 条" % total) in flat,
                  "%s（库里有 %d 条）" % (flat[:90], total))
        check("第一页就是按热度排的（刚种的高订阅数在前）",
              p1["live"] or (p1["names"] and p1["names"][0].startswith("界面验收模组 01")),
              p1["names"][:2])

        # 热度条这一轮新加的。判据分三层：
        #  1. 每张卡的条可用长度必须一致——数字那一格按内容伸缩会把条挤成长短不一，
        #     那样"谁的条更长"这个唯一的作用当场作废（上一轮就是这么翻的车）；
        #  2. 条长跟条上那个数字必须同向（只看 DOM 就能判）；
        #  3. "那个数字是不是接口给的"必须对着接口算，不然 DOM 自己跟自己对齐，
        #     订阅数取错字段照样全绿。
        bars = [(h["t"], h["w"], h["cw"], h["tw"], h["tt"]) for h in p1["heat"] if h]
        check("热度条：这一页有订阅数的卡都画出了条", len(bars) >= 1,
              "%d/%d 张有条" % (len(bars), p1["n"]))
        tracks = sorted({tw for _, _, _, tw, _ in bars})
        check("热度条：同一页里每条的轨道一样长（数字槽是固定宽度，条长才可比）",
              len(tracks) < 2 or tracks[-1] - tracks[0] <= 1, tracks)
        short = [(a, wa, b, wb) for (a, wa, _, _, _), (b, wb, _, _, _) in zip(bars, bars[1:])
                 if said(a) > said(b) and wa < wb]
        check("热度条：条长跟着条上的数字走（订阅多的不许更短）", not short, short[:3])
        check("热度条：条长真的落在卡里（不是 0 宽、也不溢出卡）",
              all(0 < w <= cw for _, w, cw, _, _ in bars), [(w, cw) for _, w, cw, _, _ in bars][:6])
        # 条上那格是有损的（"1.23 亿"），所以精确值必须在这一行的悬停标题里读得到，
        # 不然界面就是在拿一个近似数当事实。
        loose = [(t, tt) for t, _, _, _, tt in bars if tt == ""]
        check("热度条：有订阅数的卡，悬停标题里带着不带万/亿的精确值", not loose, loose[:2])
        if not p1["live"]:
            items = api("/api/v1/mods/search?page=1&size=%d" % PAGE).get("items") or []
            by_name = {it.get("name"): it for it in items}
            missing = [nm for nm, h in zip(p1["names"], p1["heat"])
                       if by_name.get(nm) and (by_name[nm].get("subscriptions") or 0) and not h]
            invented = [nm for nm, h in zip(p1["names"], p1["heat"])
                        if h and not ((by_name.get(nm) or {}).get("subscriptions") or 0)]
            wrong = [(nm, h["t"], (by_name.get(nm) or {}).get("subscriptions"))
                     for nm, h in zip(p1["names"], p1["heat"])
                     if h and by_name.get(nm) and h["t"] != count_text(by_name[nm]["subscriptions"])]
            rough = [(nm, h["tt"], (by_name.get(nm) or {}).get("subscriptions"))
                     for nm, h in zip(p1["names"], p1["heat"])
                     if h and by_name.get(nm) and h["tt"] != exact_text(by_name[nm]["subscriptions"])]
            check("热度条：接口说有订阅数的卡，条一定在场", not missing, missing[:3])
            check("热度条：没有订阅数的卡不许凭空画一条", not invented, invented[:3])
            check("热度条：条上那个数字就是接口给的订阅数（按 count 的取整比）", not wrong, wrong[:2])
            check("热度条：悬停标题里那个精确数就是接口给的订阅数", not rough, rough[:2])

        click_btn(b, ".pager .pg-btn", "下一页")
        time.sleep(0.9)
        p2 = cards(b)
        overlap = sorted(set(p1["names"]) & set(p2["names"]))
        check("下一页换了内容（卡片名没有重叠）", p2["n"] > 0 and not overlap,
              "第2页 %s 张，重叠 %s" % (p2["n"], overlap[:3]))
        check("页码高亮跟到 2", str(p2["on"]).strip() == "2", "%s / %s" % (p2["on"], p2["pages"]))
        check("上一页不再是禁用态", p2["prevOff"] is False, p2["prevOff"])

        click_btn(b, ".pager .pg-btn", "上一页")
        time.sleep(0.9)
        back = cards(b)
        check("上一页回到第 1 页原样", back["names"] == p1["names"] and str(back["on"]).strip() == "1",
              "%s 张，%s" % (back["n"], back["names"][:2]))

        # 直接点页码跳页：这是这一版分页条新增的能力，光有上一页/下一页不算做完。
        # 页码那一串是"窗口"（首页 / 尾页 / 当前页左右），不是全量页码，
        # 所以只能点当前窗口里在场的那几个——上一轮点了个不在场的 3，点了等于没点。
        click_btn(b, ".pager .pg-num", "2", exact=True)
        time.sleep(1.0)
        p2b = cards(b)
        check("点页码 2 落回刚才第 2 页那批", p2b["names"] == p2["names"] and str(p2b["on"]).strip() == "2",
              "%s 张 / 第 %s 页" % (p2b["n"], p2b["on"]))

        last_num = p1["pages"][-1]
        click_btn(b, ".pager .pg-num", last_num, exact=True)
        time.sleep(1.0)
        pl = cards(b)
        check("点尾页能直接跳过去", str(pl["on"]).strip() == last_num and
              not (set(pl["names"]) & set(p1["names"])),
              "第 %s 页 / 尾页 %s，%s 张" % (pl["on"], last_num, pl["n"]))
        check("尾页上下一页是禁用态", pl["nextOff"] is True, pl["nextOff"])

        # 搜索：填一个纯数字的工坊 ID。带着 key 时这条走的是"精确查"（GetDetails），
        # 假 ID 在线一定查不到，剩下的都由缓存补——所以不管本机配没配 key，
        # 这一屏都只有种下去的那一条，"搜完只剩一页"这个前提才是确定的。
        want_name = "界面验收模组 06"
        set_input(b, ".grid-form input[type=search]", IDS[5])
        click_btn(b, ".grid-form button", "搜")
        if not wait_settled(b):
            print("（这一次检索没在 42 秒内落地，后面几条会跟着不准）")
        s = cards(b)
        check("按 ID 搜只剩那一条", s["n"] == 1 and s["names"] == [want_name],
              "%s 张：%s" % (s["n"], s["names"][:3]))
        check("搜完回到第 1 页且下一页禁用", str(s["on"]).strip() == "1" and s["nextOff"] is True,
              "on=%s nextOff=%s pages=%s" % (s["on"], s["nextOff"], s["pages"]))
        check("分页条在只有一页时也说了总数", "共 1 条" in s["pager"].replace("\n", " "),
              s["pager"].replace("\n", " ")[:90])

        set_input(b, ".grid-form input[type=search]", "")
        click_btn(b, ".grid-form button", "搜")
        wait_settled(b)
        r = cards(b)
        check("清掉搜索词后回到完整列表", r["n"] == PAGE and str(r["on"]).strip() == "1",
              "%s 张 / 第 %s 页" % (r["n"], r["on"]))

        # 在线模式的分页是另一条链路：page 要一路传到 Steam 的 QueryFiles 才行。
        # 用户报的"点下一页没有效果"就发生在他真的打了关键词的时候，所以这一段单独点一次。
        # 本机到工坊的网络时好时坏，live=false 时不硬测，说明原因并留给云机那一轮。
        set_input(b, ".grid-form input[type=search]", "背包")
        click_btn(b, ".grid-form button", "搜")
        wait_settled(b, tries=40)
        time.sleep(1.0)
        l1 = cards(b)
        if not l1["live"]:
            why = b.js("(() => { const n = document.querySelector('.note-box, .x-note');"
                       " return n ? n.innerText.slice(0, 120) : ''; })()")
            print("（这一轮在线检索没通，在线分页留给云机那一轮：%s）" % why)
        else:
            check("在线搜索也排满了第一页", l1["n"] > 0, "%s 张" % l1["n"])
            check("在线搜索的分页条报了工坊那边的总数",
                  "共 " in l1["pager"] and len(l1["pages"]) >= 2 and l1["nextOff"] is False,
                  l1["pager"].replace("\n", " ")[:90])
            b.js("document.querySelectorAll('.pager .pg-btn')[1].click()")
            time.sleep(3.0)
            l2 = cards(b)
            dup_live = sorted(set(l1["names"]) & set(l2["names"]))
            # 不要求零重叠：QueryFiles 的排序在页界处偶尔会把同一条再给一遍，
            # 但"整页一张没换"只可能是 page 参数根本没传下去。
            check("在线搜索下一页换掉了大部分卡片（page 真的传到了接口）",
                  l2["n"] > 0 and len(dup_live) * 2 < l2["n"] and str(l2["on"]).strip() == "2",
                  "%s 张 / 第 %s 页 / 重复 %s 张" % (l2["n"], l2["on"], len(dup_live)))
        set_input(b, ".grid-form input[type=search]", "")
        click_btn(b, ".grid-form button", "搜")
        # 这一句不是收尾的礼貌：详情弹窗、手机端那两屏都读当前这一屏。
        # 不等到落地就会在骨架屏上点「详细信息」，然后一路假失败。
        wait_settled(b)

        # 详情弹窗：卡片上只给一行元信息，完整说明在这里。
        click_btn(b, ".mod-card-foot button", "详细信息")
        time.sleep(0.5)
        dlg = b.js("""(() => {
          const d = document.querySelector('.mod-detail');
          if (!d || d.offsetParent === null) return null;
          return { name: ((d.querySelector('.mod-detail-head') || {}).innerText || '').trim().slice(0, 40),
                   rows: [...d.querySelectorAll('.kv-list li, .mod-detail-body .row')].length,
                   close: [...d.querySelectorAll('button')].map((x) => x.textContent.trim()) };
        })()""")
        check("详细信息弹得出来并带着正文", bool(dlg) and dlg["name"], str(dlg)[:160])
        click_btn(b, ".mod-detail button", "关闭")
        time.sleep(0.4)
        check("关闭后遮罩撤掉了", b.js("!!document.querySelector('.mod-detail')") is False, "")

        # 手机端：卡片网格要还是多列，而且不许把页面撑出横向滚动。
        b.set_viewport(390, 844)
        time.sleep(0.7)
        m = cards(b)
        check("手机上卡片还是多列网格", m["n"] > 0 and m["cols"] >= 2, "%s 张 / %s 列" % (m["n"], m["cols"]))
        check("手机上没有横向溢出", m["overflow"] <= 1, "overflow=%s" % m["overflow"])
        check("手机上分页条还是那两个可点按钮，且没被撑出屏幕",
              m["pagerBtns"] == 2 and m["pagerBtnsVis"] == 2 and m["pagerRight"] <= m["vw"],
              "按钮 %s 个（可见 %s）/ 右边界 %s / 视口 %s" % (
                  m["pagerBtns"], m["pagerBtnsVis"], m["pagerRight"], m["vw"]))
        # 用户报的就是"在手机上点下一页没反应"，所以这一屏也必须真点一次，
        # 而不是只看它在不在。
        on_phone = m["names"]
        b.js("document.querySelectorAll('.pager .pg-btn')[1].click()")
        time.sleep(1.0)
        m2 = cards(b)
        dup_phone = sorted(set(on_phone) & set(m2["names"]))
        check("手机上下一页也换了内容", m2["n"] > 0 and not dup_phone,
              "%s 张，重叠 %s" % (m2["n"], dup_phone[:3]))
        b.shoot(os.path.join(OUT, "mods_phone.png"))
        b.set_viewport(1440, 900)
        time.sleep(0.5)
        b.shoot(os.path.join(OUT, "mods_cards.png"))
        b.close()
    finally:
        restore()

    bad = [x for x in results if not x[0]]
    print("\n合计 %d 条，失败 %d 条" % (len(results), len(bad)))
    for ok, name, detail in bad:
        print("失败：%s  |  %s" % (name, str(detail)[:200]))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
