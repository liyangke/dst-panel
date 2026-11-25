#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""总览这一屏，在浏览器里逐条对账（任务 #105 / #106 / #124 / #126）。

2026-11-04 用户提的分法：「这台机器的位置分成两部分，左边维持现在的内容，右边放未完成的任务；
下方放一个 tab 栏选房间，在房间里显示当前信息、在线趋势、各个玩家的在线时间图；
默认展示正在运行的房间。」三处都做完以后，只有浏览器能回答"这屏到底读不读得下去"：
接口探针看 JSON，看不出两栏有没有并排、页签默认落在哪一枚、图底那句峰值写没写时刻。

（2026-11-06 #125 把「玩家在线时间」那张泳道卡搬去了「玩家管理」——逐人明细不该占总览那一眼的
位置。这一屏因此只剩两张图卡，泳道那组几何对账（条数/行高/段数/刻度/文字段）跟着卡一起搬到
`.tools/players125_check.py`，这里只留一条"泳道不许在总览画出来"的留守检查。）

它盯四类事：
  #105 一张卡两栏 —— 左右并排（窄档折成一上一下）；空态只留一行短话，"空着不等于闲着"那句在气泡里。
  #106 房间页签 —— 只有一枚选中、默认那枚有世界在跑、下面只摊开这一间、点别的会换。
  #124/#126 右列两张图卡 —— 行为构成各行加起来要等于接口那句 rows（红线：同一条公告两个世界
       各写一遍的那份重复，就是在这里被界面数字暴露出来的）；在线情况卡上新加的「这一窗出现过
       N 人」和「峰值 N（什么时候）」也必须等于接口 points/people 的数。
  公共 —— 四档宽度不横向溢出、没有未捕获异常、码值一个都不上屏。

跑法（先把本机实例起起来：python .tools/ui_dev.py && python .tools/ui_dev.py --reseed）：
    python .tools/overview105_check.py
只读，不启停世界。
"""
import base64
import json
import os
import re
import sys
import time
import urllib.request

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from cdp import Browser, BASE, hover  # noqa: E402

SHOTS = os.path.join(HERE, "tmp", "overview105")
CLUSTER = os.environ.get("OV_CLUSTER", "Cluster_1")
HOURS = int(os.environ.get("OV_HOURS", "24"))
USER = os.environ.get("OV_USER", "admin")
PW = os.environ.get("OV_PW", "admin")

# 五档：2226 是用户外接屏那一档（#122 那句"太小了看不清"就是从这一档来的，
# 右列那张卡在这个宽度上才量得出挤不挤），1440 是日常那一档，1080 是"总览两栏但卡片已经窄了"那一档，
# 760 是容器断点(820)已生效的一档，390 是手机。
VIEWS = [(2226, 1082, "ultra"), (1440, 900, "wide"), (1080, 800, "mid"),
         (760, 1000, "narrow"), (390, 844, "phone")]

# 上屏不许出现的码值：区间收尾方式 + 行为事件码 + Go 类型名。
CODES = ["player_join", "player_leave", "player_death", "ghost_gone", "player_resurrect",
         "TimelineSpan", "SpanEnd"]

# 泳道那组几何规则（条数 / 行高 / 刻度 / 中文时长）的镜像抄在 .tools/players125_check.py（#125）：
# 卡片搬去玩家管理页，跟着卡一起搬的还有那一组判据。这里只留一条守护：
# 总览不许再画泳道，也不许留下半截控件。


fails = []


def check(name, ok, detail=""):
    print("  %s  %s%s" % ("ok  " if ok else "FAIL", name, ("  ｜" + detail) if detail else ""))
    if not ok:
        fails.append(name + ("（" + detail + "）" if detail else ""))
    return ok


def say(text):
    print("  ·  " + text)


def api(path):
    """带基本认证取接口原文。用户名口令只走 header，绝不进命令行、绝不打印。"""
    req = urllib.request.Request(BASE + path)
    req.add_header("Authorization", "Basic " + base64.b64encode(
        ("%s:%s" % (USER, PW)).encode()).decode())
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["data"]


def login(b, tag):
    b.goto(BASE + "/?ov=%d%s#/" % (int(time.time()), tag))
    if b.wait("!!document.querySelector('.login-form')", tries=40, pause=0.3):
        b.js("""(() => {
          const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
          const f = document.querySelector('.login-form');
          set(f.querySelector('input[type=text], input:not([type])'), %s);
          set(f.querySelector('input[type=password]'), %s);
          f.querySelector('.login-submit').click();
        })()""" % (json.dumps(USER), json.dumps(PW)))
    if not b.wait("!!document.querySelector('.room-tabs')", tries=100, pause=0.3):
        b.shoot(os.path.join(SHOTS, tag + "_nologin.png"))
        raise SystemExit("没进到总览页：hash=%s 正文=%s" % (
            b.js("location.hash"), (b.js("document.body.innerText.slice(0,200)") or "")))
    # 装一次异常钩子 + 一次取数记录，整屏跑完再收。
    b.js("""(() => {
      window.__errs = []; window.__calls = [];
      addEventListener('error', (e) => window.__errs.push(String(e.message || e.error)));
      addEventListener('unhandledrejection', (e) => window.__errs.push('promise:' + String(e.reason)));
      const real = window.fetch;
      window.fetch = async (u, o) => {
        const s = String(u);
        if (s.indexOf('/api/v1/') >= 0) window.__calls.push(s.replace(location.origin, ''));
        return real(u, o);
      };
    })()""")


RECT = r"""(() => {
  const r = (el) => {
    if (!el) return null;
    const b = el.getBoundingClientRect();
    return {top: Math.round(b.top + scrollY), left: Math.round(b.left),
            w: Math.round(b.width), h: Math.round(b.height), right: Math.round(b.right)};
  };
  const rowCards = Array.from(document.querySelectorAll('.host-row > .card')).map((c) => ({
    cls: c.className, rect: r(c),
    border: getComputedStyle(c).borderTopWidth,
    head: (() => { const h = c.querySelector('.card-head h3'); return h ? h.textContent.trim() : ''; })(),
  }));
  const card = document.querySelector('.card.host-strip');
  const tabs = Array.from(document.querySelectorAll('.room-tabs button')).map((t) => ({
    on: t.classList.contains('on'),
    name: (t.querySelector('span:nth-child(2)') || {}).textContent || '',
    count: (t.querySelector('.tab-count') || {}).textContent || '',
    dot: t.querySelector('.tab-dot') ? t.querySelector('.tab-dot').className : '',
    rect: r(t),
  }));
  const room = document.querySelector('.card.tinted .room-name');
  // 两张卡（#110 拆的家、#124 补了「行为构成」、#125 把「玩家在线时间」整张搬去玩家管理）：
  // 上面是「在线情况」，窗控件在它手里；下面是「行为构成」，读的是同一次时间线回答。
  // 按类名取而不是按 cards[0]/cards[1] 取——这一列中间插过卡，
  // 靠下标认"下面那张"当场就会认成刚插进来的这一张。
  const trendCard = document.querySelector('.card.ov-trend');
  const behCard = document.querySelector('.card.ov-behaviour');
  // 这一组只认"带图的卡"（.ov-insight），按 DOM 顺序：在线情况 → 行为构成。
  const cards = Array.from(document.querySelectorAll('.ov-insight'));
  // #123 右列那一档：列本身、左边那张房间卡、列里的「下一步」。
  // 房间卡取 .card.tinted（页签那一条选中的那一间），它的高度就是这一行的行高——
  // 上一版右列被 grid 拉去和它齐高，多出来的那一截全灌进了趋势图。
  const asideEl = document.querySelector('.grid-wide > aside');
  const seg = Array.from(document.querySelectorAll('.ov-insight .seg button')).map((x) => ({
    on: x.classList.contains('on'), label: x.textContent.trim()}));
  // #125 搬家要验的是"没留半截"：泳道本体、它自己那排窗控件、横跨整幅那个类名，
  // 任何一件还在总览出现都算搬不干净——残件不会报错，只会让读者以为图坏了。
  const tlLeft = document.querySelectorAll('.tl2, .tl-when, .tl-row, .tl-legend, .tl-wide').length;
  // 「在线情况」那一排胶囊（#126 往里加了「这一窗出现过 N 人」）和图底那一行
  // 「峰值 N（什么时候）· 当前 M」：这两处是新上的数字，都得对上接口，不能是前端自己推的。
  const when = trendCard ? trendCard.querySelector('.row.ov-when') : null;
  const whenChips = when ? Array.from(when.querySelectorAll('.chip')).map((x) => ({
    t: x.textContent.trim(), tip: x.getAttribute('title') || ''})) : [];
  const foot = trendCard ? trendCard.querySelector('.row.spread') : null;
  const footSpans = foot ? Array.from(foot.querySelectorAll('span')).map(
    (x) => x.innerText.replace(/\s+/g, ' ').trim()) : [];
  return {
    cardRect: r(card),
    rowRect: r(document.querySelector('.host-row')),
    // #117 顶部这一档是两张卡：各自的类名、边框、卡头和边界。
    // #105~#116 那几版量的是"一张卡之内的两栏"（栏数、栏内缝隙），那一层结构已经退场。
    rowCards: rowCards,
    rowCols: (() => {
      const s = document.querySelector('.host-row');
      return s ? getComputedStyle(s).gridTemplateColumns : '';
    })(),
    // 两卡之间的缝：左卡右缘到右卡左缘。它必须是一段真空白（列间距），不是零。
    rowSeam: rowCards.length > 1
      ? Math.round(rowCards[1].rect.left - rowCards[0].rect.right) : null,
    // #110 两张图拆家之后：几张卡、中间留了多少缝、窗控件在哪一张里。
    cardCount: cards.length,
    cardHeads: cards.map((c) => {
      const h = c.querySelector('.card-head h3');
      return h ? h.textContent.trim() : '';
    }),
    cardsGaps: cards.slice(1).map((c, i) =>
      Math.round(c.getBoundingClientRect().top -
                 (cards[i].getBoundingClientRect().top + cards[i].getBoundingClientRect().height))),
    segCards: cards.filter((c) => c.querySelector('.seg')).length,
    // 2026-11-09：这张卡整张下掉（顶栏那枚计数一键直达任务中心，那里有完整进度与明细）。
    // 判据反过来钉"没留半截"，并确认顶栏那枚入口还点得动、悬停把口径说完。
    liveGone: !document.querySelector('.host-live'),
    liveRows: document.querySelectorAll('.host-live .list-plain > li').length,
    singleCardHead: (() => {
      const c = document.querySelector('.host-row > .card .card-head h3');
      return c ? c.textContent.trim() : '';
    })(),
    taskGauge: (() => {
      const g = Array.from(document.querySelectorAll('header .gauge')).find(
        (x) => (x.textContent || '').indexOf('未结束任务') >= 0);
      return g ? {tag: g.tagName.toLowerCase(), title: g.getAttribute('title') || ''} : null;
    })(),
    syncBtn: !!Array.from(document.querySelectorAll('.host-strip .btn')).some((x) => /立即同步/.test(x.textContent)),
    tabs: tabs,
    tabScrollW: (() => {
      const t = document.querySelector('.room-tabs');
      return t ? {sw: t.scrollWidth, cw: Math.round(t.getBoundingClientRect().width)} : null;
    })(),
    roomName: room ? room.textContent.trim() : '',
    trendRect: r(trendCard),
    // #123：图场是定值（`app.css` 的 `.card.ov-trend .spark-field { height: 180px }`），
    // 右列按自己两张卡的内容排（`align-self: start`），所以这里量三件事：
    // 图场实际多高、右列和房间卡谁先结束、「下一步」是不是还在右列里。
    fieldH: (() => { const x = document.querySelector('.card.ov-trend .spark-field');
      return x ? Math.round(x.getBoundingClientRect().height) : -1; })(),
    asideRect: r(asideEl),
    asideAlign: asideEl ? getComputedStyle(asideEl).alignSelf : '',
    roomRect: r(document.querySelector('.card.tinted')),
    nextSteps: (() => { const n = document.querySelector('.card.next-steps');
      return n ? {rect: r(n), inAside: !!asideEl && n.closest('aside') === asideEl} : null; })(),
    // #124 右列第二张卡「行为构成」：行、条长、色档、分母那一枚，还有它是不是挂在这一列里。
    behaviour: (() => {
      if (!behCard) return null;
      const px = (x) => Math.round(x.getBoundingClientRect().width);
      const tx = (p, sel) => { const e = p.querySelector(sel); return e ? e.textContent.trim() : ""; };
      const rows = Array.from(behCard.querySelectorAll('.bh-line')).map((x) => ({
        k: tx(x, '.bh-k'),
        v: tx(x, '.bh-v'),
        bar: px(x.querySelector('.bh-bar b') || x.querySelector('.bh-bar')),
        track: px(x.querySelector('.bh-bar')),
        accent: getComputedStyle(x).getPropertyValue('--page-accent').trim(),
      }));
      const bc = Array.from(behCard.querySelectorAll('.card-head .chip'))
          .map((x) => x.textContent.trim());
      const empty = behCard.querySelector('.empty');
      return {
        rect: r(behCard),
        inAside: !!asideEl && behCard.closest('aside') === asideEl,
        rows: rows,
        countChip: (bc.find((t) => /行为/.test(t)) || ''),
        winChip: (bc.find((t) => /^近/.test(t)) || ''),
        emptyText: empty ? empty.innerText.replace(/\s+/g, ' ').trim() : '',
        emptyHasNote: !!behCard.querySelector('.empty .e-note'),
        overflow: behCard.scrollWidth > behCard.clientWidth,
      };
    })(),
    seg: seg,
    tlLeft: tlLeft,
    whenChips: whenChips,
    // 「这一窗出现过 N 人」那枚（#126 新上屏的人数），按文字认，不按位置认。
    peopleChip: (whenChips.find((c) => /出现过/.test(c.t)) || {t: '', tip: ''}),
    footSpans: footSpans,
    bodyText: document.body.innerText,
    overflow: document.documentElement.scrollWidth - window.innerWidth,
  };
})()"""


def overlap(a, b):
    """两块 rect 纵向是不是叠着（= 左右并排）。"""
    if not a or not b:
        return False
    return min(a["top"] + a["h"], b["top"] + b["h"]) - max(a["top"], b["top"]) > 6


def check_host_split(v):
    # 2026-11-09 起这一档只剩一张卡：右半幅「未结束的任务」整张下掉。它平时是一整块空态，
    # 顶栏那枚计数实时、可点、直达任务中心，那一页本来就有进度条、字节明细和结论。
    # #105~#117 量的是"一张卡里的两栏"或"两张卡之间的缝"，那两层结构整个退场了。
    cards = v["rowCards"]
    tracks = [x for x in v["rowCols"].split(" ") if x.strip()]
    check("顶部这一档只剩「这台机器」一张卡",
          len(cards) == 1 and v["singleCardHead"] == "这台机器",
          "%d 张：%s ｜ 卡头「%s」" % (
              len(cards), " / ".join(c["head"] for c in cards), v["singleCardHead"]))
    check("这一行是单列（不再给任务留半幅）", len(tracks) == 1, v["rowCols"])
    check("总览没有留下 .host-live 或任务列表残件",
          v["liveGone"] and v["liveRows"] == 0,
          "还有卡=%s / %d 行任务" % (not v["liveGone"], v["liveRows"]))
    if cards and v["rowRect"]:
        check("这台机器卡横跨整幅（宽度等于这一行）",
              abs(cards[0]["rect"]["w"] - v["rowRect"]["w"]) <= 2,
              "卡 %spx / 行 %spx" % (cards[0]["rect"]["w"], v["rowRect"]["w"]))
    # #110 把「在线情况」和「玩家在线时间」拆成两张卡，#124 又在右列补了「行为构成」（三张）；
    # #125 把那张逐人泳道整页搬到玩家管理，于是这一屏回到两张，窗控件仍然只在上面那张里。
    check("这一屏的图各自成卡（泳道搬走之后剩两张）", v["cardCount"] == 2,
          "%d 张：%s" % (v["cardCount"], v["cardHeads"]))
    if v["cardCount"] == 2:
        check("两张卡的名字各自点名了内容（在线情况 / 行为构成）",
              v["cardHeads"] == ["在线情况", "行为构成"], " / ".join(v["cardHeads"]))
        check("每两张卡之间都留出了真正的缝",
              all(g >= 8 for g in v["cardsGaps"]),
              " / ".join("%spx" % g for g in v["cardsGaps"]))
        check("时间窗控件只在上面那张卡里（不会两边各选各的窗）", v["segCards"] == 1,
              "%d 张卡里有控件" % v["segCards"])
        check("下面那张卡自己写着用的是哪一窗", bool(v["behaviour"]["winChip"]),
              "chip=「%s」" % v["behaviour"]["winChip"])
    # 搬家要验的是"没留半截"：泳道本体、它那一排窗控件、横跨整幅那个类名，
    # 任何一件还在总览出现都算搬不干净——残件不报错，只会让读者以为图坏了。
    check("#125 总览上没有泳道的任何残件（.tl2 / .tl-when / .tl-row / .tl-legend / .tl-wide）",
          v["tlLeft"] == 0, "还留着 %d 个" % v["tlLeft"])
    check("左卡保留了那颗「立即同步」", v["syncBtn"])
    # 卡下掉之后，站内唯一入口就是顶栏那枚计数；原来写在卡头气泡里的口径
    # （只数四类耗时操作、空着不等于机器闲着）挪到这枚仪表的悬停上。
    g = v["taskGauge"]
    check("顶栏「未结束任务」是唯一入口，且可点",
          bool(g) and g["tag"] == "button", "%r" % g)
    check("悬停把落点与口径说清（去任务中心、只数四类耗时操作）",
          bool(g) and "任务中心" in g["title"] and "四类" in g["title"],
          (g or {}).get("title", "")[:90] or "没这枚仪表")


def check_right_col_123(v, data=None):
    """右列那一档：#123 钉"图不许被拉长"，#124 补上第二张卡并把它压到左边的底线上。

    2026-11-05 #123：上一版让趋势图用 flex 吃掉整行富余（第 33 条第二问），前提是那一列里
    还压着第二张卡；#122 把时间线卡挪出去之后那一列只剩一张图，富余全灌进图场——
    2226 实测图场从 118 被拉到 536。所以图场定值 180px 这一条留着，它现在管的是
    "这张图读不读得出东西"，不再管"谁吃留白"。

    2026-11-06 #124：用户把「下一步」要回页顶（整机维度），又说"我感觉你没有和左边的房间对齐"，
    于是这一列补了「行为构成」，同时撤掉 align-self: start 让这一列重新跟着行高走。
    富余由下面那张卡的行距吃，不吃上面那张图的图场——这两条要一起成立才算修好。
    """
    check("#123 趋势图的图场是定值 180px（不随行高拉伸）", v["fieldH"] == 180,
          "%spx" % v["fieldH"])
    ns = v["nextSteps"]
    if ns is None:
        say("这一档没有「下一步」那一栏（只读账号看不到它），落点那条跳过")
    else:
        # #124 用户把它要回页顶："这一块是针对整个机器而言的"——它查的四条都是这台机器的事，
        # 和页签当前选中哪一间无关，所以不许再挂在右列那一格下面。
        check("#124「下一步」在页顶整幅那一档，不挂在某一间房右列下面", not ns["inAside"],
              "卡 left=%s top=%s 在右列里=%s" % (ns["rect"]["left"], ns["rect"]["top"], ns["inAside"]))
    a, rm = v["asideRect"], v["roomRect"]
    if not a or not rm:
        check("#123 右列和房间卡都量得到", False, "右列=%s 房间卡=%s" % (a, rm))
        return
    ab, rb = a["top"] + a["h"], rm["top"] + rm["h"]
    if not overlap(a, rm):
        say("右列底 %s / 房间卡底 %s ｜ 这一档折成一列，齐高那条跳过" % (ab, rb))
    else:
        # #123 那一轮"两边谁先结束"只报数不钉，因为那一列当时只有一张卡、富余注定落在列外；
        # #124 补了第二张卡之后富余由它吃，用户挑的缺陷正是"右列比房间卡先结束 113px"，
        # 所以钉的是**方向**而不是相等：右列不许早于房间卡结束（±2px 内齐平算最好）。
        # 反过来那一档不钉死：这一列的内容（图场定值 180 的那张 + 这张）自然高过
        # 一张没人在线的房间卡时，行高由右列定，房间卡先结束、多出来那一截是页面底色——
        # 这正是 #123 选定的形状；要把两边强行拉平只能去拉伸房间卡，代价是卡里出现
        # 一大片空白（本机打桩量到最坏 300px），那条路被 #123 明确否过。
        check("#124 右列不许比房间卡先结束（底边齐平或压过它）", ab >= rb - 2,
              "右列底 %s / 房间卡底 %s ｜ 差 %spx" % (ab, rb, ab - rb))
        if ab - rb > 2:
            say("这一档右列自然高过房间卡 %spx（房间卡先结束，富余落在卡外）" % (ab - rb))
        elif abs(ab - rb) <= 2:
            say("两边底边齐平（差 %spx）" % (ab - rb))
    check_behaviour(v, data, ab)


def check_behaviour(v, data, aside_bottom):
    """#124 那张「行为构成」：位置、分母、条长、配色、空态。

    分母这条最要紧：这张卡、下面那句「N 次行为」、那张泳道读的是同一次回答，
    各行加起来不等于那个数就是接口或前端有一处算了两遍。
    """
    beh = v.get("behaviour")
    if beh is None:
        check("#124 右列有「行为构成」这张卡", False, "卡找不到（这一档是没房间的空态？）")
        return
    check("#124「行为构成」挂在右列里、在趋势图下面", beh["inAside"],
          "在右列里=%s 卡 top=%s" % (beh["inAside"], beh["rect"]["top"]))
    check("#124 这张卡不横向溢出", not beh["overflow"], "溢出=%s" % beh["overflow"])
    if not beh["rows"]:
        say("这一窗没有行为行，走的是空态那一条：%s" % (beh["emptyText"][:70] or "(空态那句也没渲染)"))
        # #127：空态只留一行短话；"0 次不等于没人在线"那句从正文搬进气泡，
        # 搬走不等于搬丢——这一窗真没人进出时，读者点开「这一张怎么读」还得能看见它。
        check("#127 行为空态就一行短话（不带第二段说明）",
              bool(beh["emptyText"]) and len(beh["emptyText"]) <= 12 and not beh["emptyHasNote"],
              "「%s」/ 还有 e-note=%s" % (beh["emptyText"][:30], beh["emptyHasNote"]))
        if v.get("behHint"):
            check("「空着不等于没人在线，人数看上面那条线」还在气泡里",
                  "不等于没人在线" in v["behHint"] and "上面那条线" in v["behHint"],
                  v["behHint"][:70])
        return
    counts = [int(re.sub(r"\D", "", x["v"]) or 0) for x in beh["rows"]]
    total = sum(counts)
    chip_n = int(re.sub(r"\D", "", beh["countChip"] or "") or 0)
    check("#124 卡头那枚分母写的是「N 次行为」", "行为" in beh["countChip"], beh["countChip"])
    check("#124 各行加起来 = 卡头那枚次数", total == chip_n, "%d 行合计 %s vs 卡头 %s" % (
        len(counts), total, chip_n))
    if data is not None:
        check("#124 各行加起来 = 接口那句 rows（和下面那张泳道同一个分母）",
              total == data.get("rows"), "合计 %s vs 接口 rows=%s" % (total, data.get("rows")))
        want = [(e.get("code"), e.get("count")) for e in (data.get("events") or [])]
        got = [(x["k"], int(re.sub(r"\D", "", x["v"]) or 0)) for x in beh["rows"]]
        check("#124 行数与接口一致（%d 类）" % len(want), len(got) == len(want),
              "界面 %s ｜ 接口 %s" % ([g[0] for g in got], [w[0] for w in want]))
        check("#124 行的次序按次数从多到少", counts == sorted(counts, reverse=True),
              " / ".join("%s=%s" % (g[0], g[1]) for g in got))
    # 条长是"按最多的那一类归一"，所以最长那条得占满轨道，其余按比例——
    # 这句在卡里那句说明里写了，界面要是不照做就是拿文字骗人。
    mx = max(counts)
    ratios = [x["bar"] / float(x["track"] or 1) for x in beh["rows"]]
    top = ratios[counts.index(mx)]
    check("#124 最多的那一类条长占满轨道", top > 0.97, "最长那条占 %.2f" % top)
    bad = [x["k"] for x, rt, c in zip(beh["rows"], ratios, counts)
           if abs(rt - c / float(mx)) > 0.06]
    check("#124 其余条长和它那一类的次数成比例", not bad, "不成比例：%s" % bad)
    accents = [x["accent"] for x in beh["rows"]]
    check("#124 每一类各占一档色（%d 类 %d 档）" % (len(accents), len(set(accents))),
          len(set(accents)) == len(accents), " / ".join(accents))
    for x in beh["rows"]:
        if re.search(r"[A-Za-z_]{4,}", x["k"]):
            check("行标签里没有码值", False, x["k"])
    say("行为构成：%s ｜ 卡底 %s（右列底 %s）" % (
        " / ".join("%s=%s" % (x["k"], x["v"]) for x in beh["rows"]),
        beh["rect"]["top"] + beh["rect"]["h"], aside_bottom))


def check_tabs(v, rooms):
    tabs = v["tabs"]
    names = [t["name"] for t in tabs]
    check("#106 页签一枚不少（%d 间房）" % len(tabs), len(tabs) == len(rooms),
          "页签 %s ｜ 接口房间 %s" % (names, [r.get("roomName") or r.get("name") for r in rooms]))
    on = [t for t in tabs if t["on"]]
    if check("只有一枚是选中的", len(on) == 1, "选中 %d 枚" % len(on)):
        sel = on[0]
        check("下面摊开的就是选中那一间", sel["name"] in v["roomName"],
              "页签「%s」/ 卡上「%s」" % (sel["name"], v["roomName"][:24]))
        if any("run" in t["dot"] for t in tabs):
            check("默认落在有世界在跑的那一枚", "run" in sel["dot"],
                  "选中「%s」的点 = %s" % (sel["name"], sel["dot"]))
        else:
            say("这台机器上没有在跑的世界，默认落点那条断言跳过")
    running = [t for t in tabs if "run" in t["dot"]]
    say("页签上的点：%d 枚在跑 / 共 %d 枚" % (len(running), len(tabs)))
    bad_count = [t["name"] for t in tabs if not re.match(r"^\d+/\d+$", t["count"])]
    check("每枚页签后面那枚是「在跑/总数」两个数", bool(tabs) and not bad_count,
          " / ".join("%s=%s" % (t["name"], t["count"]) for t in tabs))
    check("一次只摊开一间（房间卡找得到）", v["roomName"] != "", v["roomName"][:24])
    if v["tabScrollW"]:
        sw, cw = v["tabScrollW"]["sw"], v["tabScrollW"]["cw"]
        if sw > cw:
            say("页签条在 %s 档需要横向滚动（scrollWidth %s > 可见 %s）" % (
                v["tag"], sw, cw))


def click_other_tab(b, v):
    """切到另一间房，返回切完之后的房间名；只有一间时返回 None。"""
    other = next((i for i, t in enumerate(v["tabs"]) if not t["on"]), -1)
    if other < 0:
        return None
    want = v["tabs"][other]["name"]
    b.js("document.querySelectorAll('.room-tabs button')[%d].click()" % other)
    # 判据是"卡上的房间名变成了刚点的那一枚"，不是"和原来不一样"——
    # 上一版写成后者，点中唯一正确的那一间时反而报"点了没换"。
    if not b.wait("(() => { const r = document.querySelector('.card.tinted .room-name');"
                  "return !!r && r.textContent.trim() === %s; })()" % json.dumps(want, ensure_ascii=False),
                  tries=40, pause=0.25):
        return False
    time.sleep(1.2)
    return b.js("(() => { const r = document.querySelector('.card.tinted .room-name');"
                "return r ? r.textContent.trim() : ''; })()")


def check_trend_126(v, stats, data):
    """#126「在线情况」这张卡上新上的两个数：这一窗出现过几人、峰值那一次发生在什么时候。

    两个数各有各的来源，也就各有各的错法：人数取自同一次 player-timeline 的 people
    （和下面「行为构成」同一个分母，不另发一次问数），峰值和峰值时刻取自 /stats 那条采样线本身。
    光写"峰值 3"读者没法判断那是刚刚还是昨夜，而这一页每五秒重画一次——
    时刻是唯一会自己过期的那一项，所以它必须走 store 里那个跟着秒针重算的 rel()。
    """
    pts = (stats or {}).get("points") or []
    people = (data or {}).get("people") or []
    chip = (v.get("peopleChip") or {}).get("t") or ""
    tip = (v.get("peopleChip") or {}).get("tip") or ""
    if not pts:
        say("这一窗接口没给采样点（points=0），#126 那三条比对跳过")
        return
    want = "这一窗出现过 %d 人" % len(people)
    check("#126 卡上那句「这一窗出现过 N 人」就是接口的头数", chip == want,
          "界面「%s」/ 期望「%s」" % (chip, want))
    # 人头数和那条线的峰值不是一件事（六个人轮着来、每次只来一个：线全程在 1，头数是 6）。
    # 这句不写在气泡里，就会有读者拿两个数互相校对然后报一个假 bug。
    check("#126 那枚气泡说清了它和峰值不是一回事、并指了逐人明细的落点",
          "峰值" in tip and "玩家管理" in tip, tip[:80])
    foot = v.get("footSpans") or []
    txt = " ｜ ".join(foot)
    hi = max(int(p.get("online") or 0) for p in pts)
    last = int(pts[-1].get("online") or 0)
    hit = [x for x in foot if "峰值" in x]
    if not check("#126 图底那一行写着峰值和当前两个数", bool(hit), txt[:90]):
        return
    t = hit[0]
    m = re.match(r"^峰值 (\d+)（(.+)）· 当前 (\d+)$", t)
    check("#126 那一行的写法是「峰值 N（什么时候）· 当前 M」", bool(m), t[:70])
    if not m:
        return
    check("#126 峰值那个数等于这条采样线里最高的那一次", int(m.group(1)) == hi,
          "界面 %s / 接口 %s（共 %d 个点）" % (m.group(1), hi, len(pts)))
    check("#126 当前那个数等于最后一个采样点", int(m.group(3)) == last,
          "界面 %s / 接口最后一点 %s" % (m.group(3), last))
    when = m.group(2).strip()
    check("#126 峰值那一次带的是读得出、且会跟着秒针走的相对时刻（不是空括号，也不是钉死的绝对时刻）",
          bool(re.match(r"^(刚刚|不到 1 秒|\d+ 秒前|\d+ 分钟前|\d+ 小时前|\d+ 天前)$", when))
          or bool(re.match(r"^\d{4}-\d{2}-\d{2}$", when)) or when == "—",
          "括号里是「%s」" % when)


def check_no_codes(v, data, stats=None):
    """码值/Go 类型名/裸英文一律不许上屏，接口那两句说明也一起扫。

    说明那句为什么单扫：它走 x-hint，只在气泡点开时才上屏，整页 innerText 里没有它，
    正文扫干净不代表气泡扫干净。
    """
    text = v["bodyText"]
    leak = [c for c in CODES if c in text]
    check("屏上没有码值", not leak, " / ".join(leak))
    if leak:
        # 截图已经在流程里拍了，这里只把命中的那一行摘出来给排查用
        lines = [ln.strip() for ln in text.splitlines() if any(c in ln for c in leak)]
        say("命中所在行：" + " ｜ ".join(lines[:3])[:200])
    notes = [("时间线", (data or {}).get("note") or ""),
             ("采样线", (stats or {}).get("note") or "")]
    for label, note in notes:
        if not note:
            continue
        check("%s那句说明里也没有码值" % label, not any(c in note for c in CODES), note[:60])
        m = re.search(r"\d{2}-\d{2} \d{2}:\d{2}", note)
        check("%s那句说明里没有服务端拼的 UTC 钟点（会和本地钟差一截）" % label,
              not m, m.group(0) if m else "")


def open_card_hint(b, card_sel):
    """点开某一这张卡卡头那枚「这一张怎么读 / 这一栏收哪些事」，读气泡正文，读完关掉。

    #127 把空态那一大段删了以后，那几句口径的唯一落点就是这枚气泡，
    所以判据必须跟着搬到这儿读。走真鼠标悬停（第 36 条）：headless 里 focus() 不补事件，
    本地夹具两种都开得开、只有指到公网成品才露馅，所以悬停失败再退到点击那一条。
    """
    got = b.js("""((sel) => {
      const btn = document.querySelector(sel + ' .card-head .tip-btn');
      if (!btn) return null;
      const r = btn.getBoundingClientRect();
      return {x: r.left + r.width / 2, y: r.top + r.height / 2};
    })(""" + json.dumps(card_sel, ensure_ascii=False) + ")")
    if not got:
        return ""
    read = """(() => {
      const btn = document.querySelector(%s + ' .card-head .tip-btn');
      const bub = btn && btn.parentElement.querySelector('.tip-bub');
      return bub ? bub.innerText.replace(/\\s+/g, ' ').trim() : '';
    })()""" % json.dumps(card_sel, ensure_ascii=False)
    for _ in range(8):
        hover(b, got["x"], got["y"])
        time.sleep(0.2)
        t = b.js(read) or ""
        if t:
            hover(b, 6, 6)
            return t
    b.js("""((sel) => {
      const btn = document.querySelector(sel + ' .card-head .tip-btn');
      if (btn) btn.click();
    })(""" + json.dumps(card_sel, ensure_ascii=False) + ")")
    b.wait("!!document.querySelector('%s .card-head .tip-bub')" % card_sel, tries=10, pause=0.2)
    t = b.js(read) or ""
    b.js("""((sel) => {
      const btn = document.querySelector(sel + ' .card-head .tip-btn');
      if (btn) btn.click();
    })(""" + json.dumps(card_sel, ensure_ascii=False) + ")")
    hover(b, 6, 6)
    return t


def last_call(b, needle):
    """页面自己最后问过的那条接口地址（带 cluster/hours 的 path+query）。

    对账必须拿这一条，不能拿脚本开头那个写死的 Cluster_1/24 小时：
    页签切过、时间窗换过之后，界面问的和探针问的就不是同一间/同一窗，
    报出来的差异是脚本自己的错，不是界面的。一条都没记到就返回空串，让调用方退回首屏那份数据。
    """
    calls = b.js("window.__calls") or []
    for c in reversed(calls):
        if needle in c:
            return c
    return ""


def main():
    os.makedirs(SHOTS, exist_ok=True)
    data = api("/api/v1/player-timeline?cluster=%s&hours=%d" % (CLUSTER, HOURS))
    rooms = api("/api/v1/clusters")
    if isinstance(rooms, dict):
        rooms = rooms.get("clusters") or rooms.get("items") or []
    live = [r for r in rooms if any(s.get("running") for s in (r.get("shards") or []))]
    print("接口：%d 个房间（%d 个有世界在跑）｜时间线 %d 人 / %d 次行为 / 窗口 %d 小时" % (
        len(rooms), len(live), len(data.get("people") or []), data.get("rows", 0),
        data.get("hours", HOURS)))
    for w, h, tag in VIEWS:
        print("===== 视口 %dx%d（%s）=====" % (w, h, tag))
        b = Browser(w, h)
        try:
            login(b, tag)
            time.sleep(2.5)                      # 等首屏两张图都答完
            v = b.js(RECT)
            v["tag"] = tag
            # 任务那张卡已经整张下掉，口径改由顶栏仪表的悬停承载（见 check_host_split）。
            if v.get("behaviour") and not v["behaviour"]["rows"]:
                v["behHint"] = open_card_hint(b, ".ov-behaviour")
            # 记住一进来选中的是哪一间（默认那枚由 store 的 pickDefaultCluster 挑，
            # 它优先选有世界在跑的），下面切走还要按名字切回来。
            first_on = next((t["name"] for t in v["tabs"] if t["on"]), "")
            check_host_split(v)
            check_right_col_123(v, data)
            check_tabs(v, rooms)
            switched = click_other_tab(b, v) if len(v["tabs"]) > 1 else None
            if switched is None:
                say("这台机器上只有一间房，切换那一条跳过")
            elif switched is False:
                check("#106 点另一枚页签会换房间", False, "点了没换")
            else:
                calls = b.js("window.__calls") or []
                check("#106 点另一枚页签会换房间", bool(switched), "切到了「%s」" % switched[:24])
                check("换房间会重新问这两张图",
                      any("player-timeline" in c for c in calls) and any("/stats" in c for c in calls),
                      " / ".join(dict.fromkeys(c.split("?")[0] for c in calls[-6:])))
                # 切回来必须按名字点。上一版在这里点了 button[0]，而目录顺序把没有世界在跑
                # 那一间排在前面，于是页面停在了一间从没记过玩家行为的房上，
                # 接口探针问的却是另一间——看着就是"界面 0 人 / 接口 2 人"的假红。
                b.js("""((want) => {
                  const t = Array.from(document.querySelectorAll('.room-tabs button'))
                    .find((x) => x.textContent.indexOf(want) >= 0);
                  if (t) t.click();
                  return !!t;
                })("%s")""" % first_on.replace('"', ""))
                if not b.wait("(() => { const on = document.querySelector('.room-tabs button.on');"
                              "return on && on.textContent.indexOf('%s') >= 0; })()"
                              % first_on.replace("'", ""), tries=20, pause=0.3):
                    check("切回默认那一间页签成功", False, "还停在别的页签上")
                time.sleep(1.2)
                v = b.js(RECT)
            v["tag"] = tag
            # 对账的基准一律取"页面自己最后问的那一次"，不拿脚本开头那一次的常量：
            # 页签切过、窗换过之后两边问的就不是同一间/同一窗，那是假红不是 bug。
            q = last_call(b, "player-timeline")
            qs = last_call(b, "/api/v1/stats")
            data_now = api(q) if q else data
            stats_now = api(qs) if qs else {}
            if q:
                say("对账基准取页面最后那两次问数：%s ｜ %s" % (q, qs))
            check_trend_126(v, stats_now, data_now)
            check_no_codes(v, data_now, stats_now)
            # 换窗：这一列两张图共用一枚窗控件（#110 分家、#125 搬走泳道之后仍然只有一枚），
            # 换完两张图必须一起重问，不然就是"上面那张已经到近 7 天、下面那张还在写近 24 小时"那种分家。
            b.js("""(() => {
              const bs = Array.from(document.querySelectorAll('.ov-insight .seg button'));
              const t = bs.find((x) => /7 天/.test(x.textContent));
              if (t) t.click();
              return !!t;
            })()""")
            if b.wait("(() => { const on = document.querySelector('.ov-insight .seg button.on');"
                      "return !!on && on.textContent.indexOf('7 天') >= 0; })()", tries=20, pause=0.2):
                time.sleep(1.4)
                q7 = last_call(b, "player-timeline")
                s7 = last_call(b, "/api/v1/stats")
                big = api(q7) if q7 else None
                v7 = b.js(RECT)
                check("换窗两张图一起重问，问的都是 168 小时这一窗",
                      bool(q7 and s7 and "hours=168" in q7 and "hours=168" in s7),
                      "%s ｜ %s" % (q7, s7))
                if big is None or not v7["behaviour"]:
                    say("换窗之后没读回时间线那份或行为卡没渲染，这一组比对跳过")
                else:
                    check("下面那张卡自己写着用的是哪一窗，换完也跟着换",
                          v7["behaviour"]["winChip"].startswith("近 7 天"),
                          "chip=「%s」" % v7["behaviour"]["winChip"])
                    # 分母这条红线换完窗还要再对一次：两张卡读同一次回答，
                    # 只在首屏对的话，换窗那一趟漏问/错问一侧就正好漏过去。
                    rows7 = [int(re.sub(r"\D", "", x["v"]) or 0) for x in v7["behaviour"]["rows"]]
                    check("换到近 7 天后各行加起来仍等于接口 rows",
                          sum(rows7) == big.get("rows"),
                          "界面 %d 行合计 %s / 接口 rows=%s" % (
                              len(rows7), sum(rows7), big.get("rows")))
                    check_trend_126(v7, api(s7) if s7 else {}, big)
            else:
                say("没找到「近 7 天」那一枚，换窗那组断言跳过")
            b.js("window.scrollBy(0, -10)")
            b.shoot(os.path.join(SHOTS, "%s_top.png" % tag))
            b.js("""(() => {
              const e = document.querySelector('.card.ov-trend');
              if (e) e.scrollIntoView({block: 'start'});
            })()""")
            time.sleep(0.4)
            b.shoot(os.path.join(SHOTS, "%s_trend.png" % tag))
            # #124 新卡单独裁一张：手机档整页截图会被 clip 的 1900px 上限截断，
            # 这一张按卡片自身滚动，保证五档都看得见它。
            b.js("""(() => {
              const e = document.querySelector('.card.ov-behaviour');
              if (e) e.scrollIntoView({block: 'center'});
            })()""")
            time.sleep(0.4)
            b.shoot(os.path.join(SHOTS, "%s_behaviour.png" % tag))
            over = b.js("document.documentElement.scrollWidth - window.innerWidth")
            check("横向没有溢出（%s px）" % over, over is not None and int(over) <= 1, str(over))
            errs = b.js("window.__errs") or []
            check("这一路没有未捕获异常", not errs, json.dumps(errs, ensure_ascii=False)[:160])
        finally:
            b.close()

    print("")
    if fails:
        print("有 %d 条没通过：" % len(fails))
        for f in fails:
            print("  - " + f)
        sys.exit(1)
    print("全部通过。截图在 %s" % SHOTS)


if __name__ == "__main__":
    main()
