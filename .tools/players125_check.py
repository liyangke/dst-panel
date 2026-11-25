#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""「玩家在线时间」这张卡搬到玩家管理页之后的界面验收（任务 #125）。

2026-11-06 用户的分法：「优化一下在线情况图，在这个卡片上多呈现一些信息。然后把玩家在线时间
这张卡移到玩家管理这个模块里。毕竟第一夜是总览，不太适合详细到这个地步。」
搬动本身接口探针一行都量不出来——JSON 里那些段、记号、次数在哪个页面渲染是看不见的事，
只有浏览器能回答这三问：泳道画在新位置对不对、总览有没有留半截、新地方还看得清吗
（这张卡当初就是因为右列太窄被 #122 挪出去横跨整幅的，这次搬到整幅单列的一页，宽度只会更好，
所以分钟/像素那一条断言留着，它是这次搬家唯一的可量化收益）。

它盯四类事：
  #125 落点 —— 这张卡住 #/players、卡头名字没变、四档时间窗和总览共用同一份档位；
              轨道宽度、泳道几何在新位置照样成立。
  #119/#122 几何 —— 段与记号的**个数和位置**对着接口 JSON 核；泳道整块不超过预算、
              行高由条数分出来；摘要条一段一人恒定 28px；时长文字段的顺序与中文时长等于接口。
  红线 —— 卡头那句「N 人 / M 次行为」必须等于接口给的数（同一条公告两个世界各写一遍的重复，
              最早就是在这里被界面数字暴露出来的）。
  公共 —— 五档宽度不横向溢出、没有未捕获异常、码值一个都不上屏。

跑法（先把本机实例起起来：python .tools/ui_dev.py && python .tools/ui_dev.py --reseed）：
    python .tools/players125_check.py
只读，不启停世界、不改名单（这一页有拉黑/改名单的按钮，脚本一律不碰）。
"""
import base64
import json
import math
import os
import re
import sys
import time
import urllib.request
from datetime import datetime

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from cdp import Browser, BASE  # noqa: E402

SHOTS = os.path.join(HERE, "tmp", "players125")
CLUSTER = os.environ.get("PL_CLUSTER", "Cluster_1")
HOURS = int(os.environ.get("PL_HOURS", "24"))
USER = os.environ.get("PL_USER", "admin")
PW = os.environ.get("PL_PW", "admin")
CARD_TITLE = "玩家在线时间"

# 和总览探针同一批视口：2226 是用户外接屏那一档（这张卡 #122 就是从这一档的"太小了看不清"
# 改起来的），390 是手机——这一页在手机上还会多出花名册那种卡片式布局，所以要各量一次。
VIEWS = [(2226, 1082, "ultra"), (1440, 900, "wide"), (1080, 800, "mid"),
         (760, 1000, "narrow"), (390, 844, "phone")]

# 上屏不许出现的码值：区间收尾方式 + 行为事件码 + Go 类型名。
CODES = ["player_join", "player_leave", "player_death", "ghost_gone", "player_resurrect",
         "TimelineSpan", "SpanEnd"]

# 泳道那块的规则，这里独立抄一份（不 import 前端）：对账要的是"界面按不按这套走"，
# 跟着组件改常量就等于没测。数值来自 timeline.js 里的 TL_*（#125 起组件从 overview.js 抽了出来）。
TL_SHOW, TL_MERGE, TL_CAP, TL_GAP, TL_ROW_MAX = 10, 2, 260, 4, 30

# 时间窗那四档的档位与写法，来自 timeline.js 导出的 TL_WINDOWS。
# 两个页面各留一份迟早漂成"上面选的近 3 天、下面写着近 7 天"，所以这里钉的是同一份。
WINDOWS = ["近 6 小时", "近 24 小时", "近 3 天", "近 7 天"]


def lanes_rule(n):
    """n 个人该有几条泳道、行高多少、要不要并出'其他 N 人'那一条。"""
    merged = n <= TL_SHOW + TL_MERGE
    lanes = n if merged else TL_SHOW + 1
    row_h = int(math.floor(min(TL_ROW_MAX, (TL_CAP - TL_GAP * (lanes - 1)) / lanes)))
    return lanes, row_h, not merged


# 刻度的规则（#122 第二问）：判据是"这一窗会不会有两枚落在同一个钟点上"（≥21 小时就会），
# 预算跟着换成带日期那一档（74px）。首尾两枚贴边不居中，所以规则里先扣半枚宽。
TL_TICK_REPEAT_S, TL_TICK_PX, TL_TICK_PX_DATE, TL_TICK_MIN, TL_TICK_MAX = 21 * 3600, 46, 74, 2, 5


def ticks_rule(axis_w, span_s):
    """该画几枚刻度。注意传进来的是**轴那一行的轨道宽**（组件量的就是它），不是泳道行的轨道宽。"""
    label_px = TL_TICK_PX_DATE if span_s >= TL_TICK_REPEAT_S else TL_TICK_PX
    return max(TL_TICK_MIN,
               min(TL_TICK_MAX, int(math.floor((axis_w - label_px / 2) / label_px)) + 1))


def tick_parts(text):
    """把一枚刻度拆成（日期, 钟点分钟数）。写法只认这三种，别的算读不出。

    裸日期「11-05」单独认出来（钟点为 None）：它确实带上了日期，只是读不出几点。
    """
    t = text.strip()
    m = re.match(r"^(\d{1,2}-\d{1,2}) (\d{1,2}):(\d{2})$", t)
    if m:
        return m.group(1), int(m.group(2)) * 60 + int(m.group(3))
    m = re.match(r"^(\d{1,2}):(\d{2})$", t)
    if m:
        return None, int(m.group(1)) * 60 + int(m.group(2))
    m = re.match(r"^(\d{1,2}-\d{1,2})$", t)
    if m:
        return m.group(1), None
    return None, None


def seconds_cn(n):
    """时长上屏那几句话，抄一份 fmt.js 的算法（不 import 前端，同一份对账思路）。

    界面这一层每个名字后面都跟着它，判据要是没有独立一份，前端改格式就等于自证通过。
    """
    n = float(n)
    if n < 0:
        return "—"
    if n < 1:
        return "不到 1 秒"
    rnd = lambda x: int(math.floor(x + 0.5))  # JS 的 Math.round 是 .5 向上，Python 的 round 是四舍六入五取偶
    if n < 60:
        return ("%s 秒" % ("%.1f" % n if n < 10 else rnd(n)))
    if n < 3600:
        return "%d 分 %02d 秒" % (int(n // 60), rnd(n % 60))
    return "%d 小时 %02d 分" % (int(n // 3600), int((n % 3600) // 60))


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


def ts(s):
    """接口那串 UTC 时刻变成可比的 datetime（只在算时间差时用，不上屏）。"""
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


def login_to_players(b, tag):
    """进面板（落到总览），再走到玩家管理那一页。

    先进总览不是多余的一步：默认落在哪一间房由 store 的 pickDefaultCluster 挑（优先有世界在跑的），
    这一页的房间下拉读的就是同一个 state.sel.cluster，从总览过去才拿得到那个默认值。
    """
    b.goto(BASE + "/?pl=%d%s#/" % (int(time.time()), tag))
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
    # 异常钩子和取数记录要在切页之前装好：切过去那一刻这一页就去问时间线了，
    # 装在后面就只能看着卡画得出来、__calls 却一条没有（那会被读成"没接上接口"）。
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
    b.js('location.hash = "#/players"')
    if not b.wait("!!document.querySelector('.players-page')", tries=40, pause=0.3):
        b.shoot(os.path.join(SHOTS, tag + "_noplayers.png"))
        raise SystemExit("进不到玩家管理页：hash=%s" % b.js("location.hash"))


RECT = r"""(() => {
  const r = (el) => {
    if (!el) return null;
    const b = el.getBoundingClientRect();
    return {top: Math.round(b.top + scrollY), left: Math.round(b.left),
            w: Math.round(b.width), h: Math.round(b.height), right: Math.round(b.right)};
  };
  // 这张卡在新落点上没有专属类名（只有一个 section.card），所以按卡头那个名字认它。
  // 按名字而不是按下标：这一页从上到下已经有四张卡，再加一张也要改下标，而名字是给人读的。
  const heads = Array.from(document.querySelectorAll('.players-page .card-head h3'));
  const hit = heads.find((x) => x.textContent.trim() === %s);
  const card = hit ? hit.closest('section.card') : null;
  const tl2 = card ? card.querySelector('.tl2') : null;
  const when = card ? card.querySelector('.row.tl-when') : null;
  const segBtns = when ? Array.from(when.querySelectorAll('.seg button')).map((x) => ({
    on: x.classList.contains('on'), label: x.textContent.trim()})) : [];
  const track = tl2 ? tl2.querySelector('.tl-row:not(.tl-axis) .tl-track') : null;
  const tr = track ? track.getBoundingClientRect() : null;
  const rowOf = (x) => { const row = x.closest('.tl-row');
    const who = row ? row.querySelector('.tl-name') : null;
    return who ? who.textContent.trim() : ''; };
  const spans = tl2 ? Array.from(tl2.querySelectorAll('.tl-row:not(.tl-axis) .tl-span')).map((x) => {
    const b = x.getBoundingClientRect();
    return {who: rowOf(x), w: Math.round(b.width * 10) / 10,
            left: Math.round((b.left - (tr ? tr.left : 0)) * 10) / 10,
            cls: x.className, title: x.getAttribute('title') || ''};
  }) : [];
  const marks = tl2 ? Array.from(tl2.querySelectorAll('.tl-row:not(.tl-axis) .tl-mark')).map((x) => {
    const b = x.getBoundingClientRect();
    return {who: rowOf(x), left: Math.round((b.left - (tr ? tr.left : 0)) * 10) / 10,
            cls: x.className, title: x.getAttribute('title') || ''};
  }) : [];
  const ticks = tl2 ? Array.from(tl2.querySelectorAll('.tl-axis .tl-tick')).map((x) => {
    const q = x.getBoundingClientRect();
    return {text: x.textContent.trim(), left: Math.round(q.left),
            right: Math.round(q.right), w: Math.round(q.width)};
  }) : [];
  const chips = card ? Array.from(card.querySelectorAll('.card-head .chip')).map((x) => ({
    t: x.textContent.trim(), tip: x.getAttribute('title') || ''})) : [];
  const sel = document.querySelector('#players-cluster');
  const empty = card ? card.querySelector('.empty') : null;
  return {
    hash: location.hash,
    pageReady: !!document.querySelector('.players-page'),
    cardFound: !!card,
    cardTitle: hit ? hit.textContent.trim() : '',
    cardRect: r(card),
    // 这一页排一列：卡离它那一列的可用宽还差多少（差的是卡片内边距之外的那点余量）。
    // 判据是"卡 = 所在那一列的内容宽"而不是"卡 > 某个像素数"，侧栏以后再变窄也不会误报。
    cardSpan: (() => { if (!card) return null;
      const col = card.parentElement;
      return col ? {delta: Math.round(col.clientWidth - card.getBoundingClientRect().width),
                    cardW: Math.round(card.getBoundingClientRect().width)} : null; })(),
    selValue: sel ? sel.value : '',
    tl2: !!tl2,
    heads: heads.map((x) => x.textContent.trim()),
    // 上面那几张卡还在（这张卡是补在最下面，不是把花名册顶掉）。
    rosterAbove: (() => { const h = heads.findIndex((x) => x.textContent.trim() === '玩家花名册');
      const t = heads.findIndex((x) => x.textContent.trim() === %s);
      return {roster: h, timeline: t}; })(),
    chips: chips,
    countChip: (chips.find((c) => /行为/.test(c.t)) || {t: ''}).t,
    whenText: when ? when.innerText.replace(/\s+/g, ' ').trim() : '',
    segBtns: segBtns,
    rows: tl2 ? tl2.querySelectorAll('.tl-row:not(.tl-axis)').length : 0,
    lanesH: (() => { const x = tl2 && tl2.querySelector('.tl2-lanes');
      return x ? Math.round(x.getBoundingClientRect().height * 10) / 10 : 0; })(),
    lanesScroll: (() => { const x = tl2 && tl2.querySelector('.tl2-lanes'); return x ? x.scrollHeight : 0; })(),
    rowHs: tl2 ? Array.from(tl2.querySelectorAll('.tl2-lanes .tl-row'))
      .map((x) => Math.round(x.getBoundingClientRect().height * 10) / 10) : [],
    stackH: (() => { const x = tl2 && tl2.querySelector('.tl2-stack');
      return x ? Math.round(x.getBoundingClientRect().height * 10) / 10 : 0; })(),
    stackSegs: tl2 ? tl2.querySelectorAll('.tl2-stack i').length : 0,
    restSpans: tl2 ? tl2.querySelectorAll('.tl2-lanes .tl-span.rest').length : 0,
    otherName: (() => { const x = tl2 && tl2.querySelector('.tl2-lanes .tl-row.other .tl-name');
      return x ? x.textContent.trim() : ''; })(),
    sumLine: (() => { const x = tl2 && tl2.querySelector('.tl2-total');
      return x ? x.innerText.replace(/\s+/g, ' ').trim() : ''; })(),
    sums: tl2 ? Array.from(tl2.querySelectorAll('.tl2-sums span')).map((x) => {
      const dot = x.querySelector('i');
      return {t: x.textContent.replace(/\s+/g, ' ').trim(),
              rest: x.classList.contains('rest'),
              dot: dot ? getComputedStyle(dot).backgroundColor : '',
              title: x.getAttribute('title') || ''};
    }) : [],
    stackDots: tl2 ? Array.from(tl2.querySelectorAll('.tl2-stack i')).map(
      (x) => getComputedStyle(x).backgroundColor) : [],
    bandTitles: tl2 ? Array.from(tl2.querySelectorAll('.tl2-stack i')).map(
      (x) => x.getAttribute('title') || '') : [],
    trackW: tr ? Math.round(tr.width) : 0,
    trackLeft: tr ? Math.round(tr.left) : -1,
    // 轴那一行的轨道：组件数刻度时量的就是它，所以镜像规则必须用同一个元素。
    axisW: (() => { const a = tl2 && tl2.querySelector('.tl-axis .tl-track');
      return a ? Math.round(a.getBoundingClientRect().width) : 0; })(),
    axisLeft: (() => { const a = tl2 && tl2.querySelector('.tl-axis .tl-track');
      return a ? Math.round(a.getBoundingClientRect().left) : -1; })(),
    legend: tl2 ? Array.from(tl2.querySelectorAll('.tl-legend span')).map((x) => x.innerText.trim()) : [],
    emptyText: empty ? empty.innerText.replace(/\s+/g, ' ').trim() : '',
    spans: spans, marks: marks, ticks: ticks,
    bodyText: document.body.innerText,
    // 这一页整幅只放一列，溢出量对内容区量：把侧栏宽度算进来会永远多出一截假数。
    overflow: (() => { const el = document.querySelector('.main') || document.documentElement;
      return el.scrollWidth - el.clientWidth; })(),
  };
})()""" % (json.dumps(CARD_TITLE, ensure_ascii=False), json.dumps(CARD_TITLE, ensure_ascii=False))


def last_timeline_call(b):
    """页面自己最后问过的那条 player-timeline 地址（带 cluster/hours 的 path+query）。

    对账必须拿这一条，不能拿脚本开头写死的 Cluster_1/24 小时：换过时间窗、
    或者这一页默认落的房间和脚本认的那间不一样时，界面问的和探针问的就不是同一间/同一窗，
    报出来的差异是脚本自己的错，不是界面的。
    """
    calls = b.js("window.__calls") or []
    for c in reversed(calls):
        if "player-timeline" in c:
            return c
    return ""


def check_landing(v, data):
    """#125 这一趟搬家本身：卡在不在这儿、四档档位是不是那一份、有没有把别的卡顶掉。"""
    check("#125 玩家管理页起来了", v["pageReady"], v["hash"])
    check("#125 这一页有「%s」这张卡" % CARD_TITLE, v["cardFound"],
          "卡头们：%s" % " / ".join(v["heads"]))
    loc = v.get("rosterAbove") or {}
    if v["cardFound"] and loc.get("roster", -1) >= 0:
        # 明细排在花名册后面：先"这些人是谁"，再"他们什么时候来、各待多久"。
        check("#125 这张卡排在「玩家花名册」下面（顺序是明细在后）",
              loc["timeline"] > loc["roster"],
              "花名册第 %d 张、这张第 %d 张" % (loc["roster"] + 1, loc["timeline"] + 1))
    labels = [x["label"] for x in v["segBtns"]]
    check("#125 时间窗那四档和总览是同一份（%s）" % " / ".join(WINDOWS), labels == WINDOWS,
          "界面 %s" % labels)
    on = [x for x in v["segBtns"] if x["on"]]
    check("时间窗只有一枚是选中的", len(on) == 1, "%d 枚选中" % len(on))
    # 卡在这一页吃满那一列：搬来的理由之一就是宽度（总览右列那档轨道只有 ~430px，
    # 一次四分钟的进贴在上一枚上看不出来）。
    sp = v.get("cardSpan") or {}
    check("#125 这张卡吃满这一页那一列（没有缩成半幅）",
          sp and abs(sp.get("delta", 99)) <= 2, "列宽 − 卡宽 = %spx / 卡宽 %spx" % (
              sp.get("delta"), sp.get("cardW")))
    if not v["tl2"]:
        # 空态：这一窗没记录时这张卡本来就不画泳道（画的是那句诚实的说明），
        # 但这条要说得出为什么，别让下面的几何对账静默跑成"0 段 = 0 段"的假绿。
        if data.get("people"):
            check("#125 接口有人却画了空态", False,
                  "接口 %d 人 / 界面：%s" % (len(data.get("people") or []), v["emptyText"][:70]))
        else:
            say("这一窗这台机器上没有任何玩家行为记录，走的是空态那一条：%s（几何对账跳过）"
                % (v["emptyText"][:70] or "(空态那句也没渲染)"))
        return False
    return True


def check_timeline(v, data):
    people = data.get("people") or []
    api_spans = sum(len(p.get("spans") or []) for p in people)
    api_marks = sum(len(p.get("marks") or []) for p in people)
    check("#108 接口给到了人（%d 个 / %d 段 / %d 记号）" % (len(people), api_spans, api_marks), True)
    # #119 起这张卡不再"一人一行、行高写死"：泳道整块有高度预算，人数只分行高。
    lanes, row_h, has_other = lanes_rule(len(people))
    ranked = sorted(people, key=lambda p: -(p.get("seconds") or 0))
    shown_people = ranked if not has_other else ranked[:TL_SHOW]
    rest_people = [] if not has_other else ranked[TL_SHOW:]
    api_marks_shown = sum(len(p.get("marks") or []) for p in shown_people)
    api_spans_rest = sum(len(p.get("spans") or []) for p in rest_people)
    check("泳道条数按 #119 的规则分（%d 人 → %d 条）" % (len(people), lanes), v["rows"] == lanes,
          "界面 %d 条 / 规则 %d 条" % (v["rows"], lanes))
    check("泳道整块不超过 %dpx（人数再多也只是变薄）" % TL_CAP,
          v["lanesH"] <= TL_CAP and v["lanesScroll"] <= TL_CAP,
          "块高 %s / 内容 %s" % (v["lanesH"], v["lanesScroll"]))
    check("行高一致且不超过 %dpx" % TL_ROW_MAX,
          v["rowHs"] and all(abs(x - row_h) < 0.6 for x in v["rowHs"]),
          "行高 %s / 规则 %s" % (sorted(set(v["rowHs"])), row_h))
    check("摘要条一段一人、恒定 28px", v["stackSegs"] == len(people) and abs(v["stackH"] - 28) < 0.6,
          "%d 段 / %s px（接口 %d 人）" % (v["stackSegs"], v["stackH"], len(people)))
    # 这次搬家唯一能量化的收益就是宽度：轨道一个像素承载的时间越短，
    # 隔几分钟的两次进出来不贴成一坨。3 分钟/px 是 #122 那次诊断里那条线。
    if v.get("vw", 0) >= 1080:
        check("近 24 小时这一窗，轨道一个像素承载不到 3 分钟",
              v["trackW"] >= 480, "轨道 %spx → %.1f 分钟/px" % (v["trackW"], 1440.0 / max(1, v["trackW"])))
    else:
        say("窄档（视口 %s）不钉分钟/像素那条：轨道只有 %spx，这一档本来就只读形状"
            % (v.get("vw"), v["trackW"]))
    check("段画出来的根数和接口一致", len(v["spans"]) == api_spans,
          "界面 %d / 接口 %d" % (len(v["spans"]), api_spans))
    check("记号只画摊开那几条（并进去的人不占一行）", len(v["marks"]) == api_marks_shown,
          "界面 %d / 摊开的那 %d 人应 %d / 接口全部 %d" % (
              len(v["marks"]), len(shown_people), api_marks_shown, api_marks))
    if has_other:
        check("并出来那条写的是「其他 %d 人」" % len(rest_people),
              v["otherName"] == "其他 %d 人" % len(rest_people), v["otherName"])
        check("那条里每个人的段仍逐段画（不是合成一条平均色）",
              v["restSpans"] == api_spans_rest, "界面 %d / 接口 %d" % (v["restSpans"], api_spans_rest))
        check("汇总那行说明并了人", "并进最后一条" in v["sumLine"], v["sumLine"])
    else:
        check("人不该并（没有「其他」那条）", not v["otherName"], v["otherName"])
        check("汇总那行只报人数和合计", "并进" not in v["sumLine"], v["sumLine"])
    chip = v["countChip"]
    want = "%d 人 / %d 次行为" % (len(people), data.get("rows", 0))
    check("卡头上那句「N 人 / M 次行为」就是接口的数", chip == want, "界面「%s」/ 期望「%s」" % (chip, want))
    # 「重复没被合掉」和「时长被算了两遍」这两条原来是用像素距离判的，2026-11-04 改成按时刻判：
    # 在 24 小时这一窗里，手机那一档轨道只有 181 px（1 px ≈ 8 分钟），而一次真实在场常常只有
    # 一两分钟，横条还得留最小可见宽度（0.7% ≈ 10 分钟）——同一个人的相邻两段必然贴在一起，
    # 两个真隔了四分钟的幽灵记号也必然重合。像素在近档上根本分辨不出一分钟和十分钟。
    dup = []
    for p in people:
        ms = sorted(p.get("marks") or [], key=lambda x: x.get("at") or "")
        for i in range(1, len(ms)):
            if ms[i].get("event") != ms[i - 1].get("event"):
                continue
            gap = (ts(ms[i].get("at")) - ts(ms[i - 1].get("at"))).total_seconds()
            if gap < 60:
                dup.append("%s 的两个 %s 只差 %d 秒" % (p.get("name"), ms[i].get("event"), gap))
    check("同一人身上的同类记号没有几十秒一对（跨分片重复没合掉的话就是它）", not dup, " / ".join(dup[:3]))
    zero = [s for s in v["spans"] if s["w"] < 1]
    check("每段都画得见（没有 0 宽的横条）", not zero,
          " / ".join("宽 %s" % s["w"] for s in zero[:3]))
    outside = [s for s in v["spans"]
               if s["left"] < -1.5 or s["left"] + s["w"] > v["trackW"] + 1.5]
    check("横条都画在自己那条轨道里（轨道宽 %s px）" % v["trackW"], not outside,
          " / ".join("%s left=%s w=%s" % (s["who"], s["left"], s["w"]) for s in outside[:3]))
    crush = []
    for p in people:
        lst = sorted(p.get("spans") or [], key=lambda x: x.get("from") or x.get("to") or "")
        for i in range(1, len(lst)):
            prev_end = ts(lst[i - 1].get("to") or data.get("to"))
            this_start = ts(lst[i].get("from") or data.get("from"))
            if this_start < prev_end:
                crush.append("%s 的第 %d 段（%s 开始）压住前一段（%s 才结束）" % (
                    p.get("name"), i + 1, this_start.strftime("%H:%M:%S"),
                    prev_end.strftime("%H:%M:%S")))
    check("同一个人的两段在场在时间上互不重叠（重叠就是时长被算了两遍）", not crush, " / ".join(crush[:2]))
    titles = [s["title"] for s in v["spans"]]
    check("每段的悬停里有起止时刻和那句中文收尾说法",
          all(re.search(r"\d{2}:\d{2}:\d{2} → \d{2}:\d{2}:\d{2}", t) for t in titles),
          (titles[0][:70] if titles else ""))
    reason_labels = set()
    for p in people:
        for sp in p.get("spans") or []:
            reason_labels.add(sp.get("reasonLabel") or "")
    hit = [t for t in titles if any(rl and rl in t for rl in reason_labels)]
    check("悬停里那句收尾说法取自接口（不是前端另写一套）", len(hit) == len(titles),
          "%d/%d，接口给的说法：%s" % (len(hit), len(titles), " / ".join(sorted(x for x in reason_labels if x))))
    check("图例只列这一窗真出现过的形状", len(v["legend"]) >= 1, " / ".join(v["legend"]))
    bad_tone = [m["cls"] for m in v["marks"]
                if not re.match(r"^tl-mark (death|ghost|revive)$", m["cls"])]
    check("记号的形状档是从接口的事件码挑出来的，且只认这三档", not bad_tone,
          " / ".join(bad_tone) if bad_tone else "%d 个记号（死亡/幽灵/复活）" % len(v["marks"]))
    # 时长：#122 第三问起它做成摘要条那一层的文字段，按待得最久排在前面。
    shown_sums = [s for s in v["sums"] if not s["rest"]]
    rest_sums = [s for s in v["sums"] if s["rest"]]
    check("时长的文字段一条对应一个摊开的人（%d 人 → %d 段）" % (len(shown_people), len(shown_sums)),
          len(shown_sums) == len(shown_people),
          "界面 %d 段 / 摊开 %d 人" % (len(shown_sums), len(shown_people)))
    check("并了人才有「其他 %d 人」那一段" % len(rest_people),
          len(rest_sums) == (1 if has_other else 0), "界面 %d 段：%s" % (
              len(rest_sums), " / ".join(s["t"] for s in rest_sums)))
    wrong = []
    for i, p in enumerate(shown_people):
        want = "%s %s" % (p.get("name"), seconds_cn(p.get("seconds") or 0))
        if i >= len(shown_sums) or shown_sums[i]["t"] != want:
            wrong.append("第 %d 段 界面「%s」/ 期望「%s」" % (
                i + 1, shown_sums[i]["t"] if i < len(shown_sums) else "（没有这一段）", want))
    check("文字段按待得最久排在前面，每段的名字和时长都等于接口", not wrong, " ｜ ".join(wrong[:2]))
    if rest_sums:
        want_rest = "其他 %d 人 %s" % (len(rest_people), seconds_cn(
            sum(p.get("seconds") or 0 for p in rest_people)))
        check("并出来那一段写的是「其他 N 人 + 这几个人的合计」",
              rest_sums[0]["t"] == want_rest, "界面「%s」/ 期望「%s」" % (rest_sums[0]["t"], want_rest))
    check("时长写的不是裸秒数也不是 Go 时长",
          all(re.search(r"[\u4e00-\u9fff]", s["t"]) for s in v["sums"]),
          " / ".join(s["t"] for s in v["sums"][:3]))
    # 色点和摘要条那段一人一色是同一份色序（hue(i)），两层对不上就是各说各话。
    mismatch = []
    for i, s in enumerate(shown_sums):
        if i < len(v["stackDots"]) and s["dot"] != v["stackDots"][i]:
            mismatch.append("第 %d 段 文字层 %s / 摘要条 %s" % (i + 1, s["dot"], v["stackDots"][i]))
    check("文字段前面那枚色点和摘要条上同一个人的那一段同色", not mismatch, " ｜ ".join(mismatch[:2]))
    # 悬停口径必须唯一：摘要条那一段、文字段、泳道那一行，三处写的是同一句话。
    band_titles = v["bandTitles"]
    diff = [i + 1 for i, s in enumerate(shown_sums[:len(band_titles)]) if s["title"] != band_titles[i]]
    check("摘要条那一段与文字段的悬停是同一句话（口径唯一）", not diff,
          "第 %s 段不一致" % ",".join(str(x) for x in diff[:3]) if diff else
          "%d 段各 %d 字" % (len(band_titles), len(band_titles[0]) if band_titles else 0))
    span_s = (ts(data.get("to")) - ts(data.get("from"))).total_seconds()
    check("轴上的轨道和泳道的轨道同宽同起点（否则最后一枚刻度不再对着横条的终点）",
          v["axisW"] == v["trackW"] and v["axisLeft"] == v["trackLeft"],
          "轴 %spx 起于 %s / 泳道 %spx 起于 %s" % (v["axisW"], v["axisLeft"], v["trackW"], v["trackLeft"]))
    want_n = ticks_rule(v["axisW"], span_s)
    check("刻度枚数由这条轨道有多宽定（轴轨 %spx → 规则 %d 枚）" % (v["axisW"], want_n),
          len(v["ticks"]) == want_n,
          "界面 %d 枚：%s" % (len(v["ticks"]), " / ".join(t["text"] for t in v["ticks"])))
    parts = [tick_parts(t["text"]) for t in v["ticks"]]
    odd = [t["text"] for t, q in zip(v["ticks"], parts) if q[1] is None]
    check("每枚刻度都写得出钟点（不许出现只有日期、读不出几点的那枚）", not odd, " / ".join(odd[:2]))
    texts = [t["text"] for t in v["ticks"]]
    dups = sorted({x for x in texts if texts.count(x) > 1})
    check("刻度文字一枚都不重样（重样说明这一窗跨了零点却没标出来）", not dups, " / ".join(dups[:3]))
    unlabeled = []
    for i in range(1, len(v["ticks"])):
        pd, pc = parts[i - 1]
        cd, cc = parts[i]
        if cc is not None and pc is not None and cc <= pc and cd is None:
            unlabeled.append("「%s」接在「%s」后面却没带日期" % (v["ticks"][i]["text"], v["ticks"][i - 1]["text"]))
    check("跨过零点那一枚刻度带上了日期", not unlabeled, " / ".join(unlabeled[:2]))
    if any(cd is not None for cd, _ in parts):
        check("轴起点那一枚也说得出是哪天（有日期就不许只在中间冒出来）", parts[0][0] is not None,
              "第一枚「%s」" % texts[0])
    lefts = [t["left"] for t in v["ticks"]]
    check("刻度从左到右排（第一枚贴着轨头、最后一枚贴着轨尾）",
          len(lefts) >= 2 and all(b > a for a, b in zip(lefts, lefts[1:])), str(lefts))
    collide = []
    for a, b in zip(v["ticks"], v["ticks"][1:]):
        if b["left"] < a["right"]:
            collide.append("「%s」和「%s」压住（%spx / %spx）" % (a["text"], b["text"], a["right"], b["left"]))
    check("刻度标签互不压字", not collide, " / ".join(collide[:2]))
    check("记录起点那枚胶囊在屏上", "记录从" in v["whenText"] or data.get("rows", 0) == 0,
          v["whenText"][:56])


def check_no_codes(v):
    """码值/Go 类型名/裸英文一律不许上屏（整页扫，不只这张卡）。"""
    text = v["bodyText"]
    leak = [c for c in CODES if c in text]
    check("屏上没有码值", not leak, " / ".join(leak))
    if leak:
        lines = [ln.strip() for ln in text.splitlines() if any(c in ln for c in leak)]
        say("命中所在行：" + " ｜ ".join(lines[:3])[:200])


def main():
    os.makedirs(SHOTS, exist_ok=True)
    data = api("/api/v1/player-timeline?cluster=%s&hours=%d" % (CLUSTER, HOURS))
    print("接口（脚本自己问的那一次，只做开场白）：%s 房间 / %d 小时 → %d 人 / %d 次行为" % (
        CLUSTER, HOURS, len(data.get("people") or []), data.get("rows", 0)))
    for w, h, tag in VIEWS:
        print("===== 视口 %dx%d（%s）=====" % (w, h, tag))
        b = Browser(w, h)
        try:
            login_to_players(b, tag)
            time.sleep(2.5)                      # 等这一页两次问数都答完
            v = b.js(RECT)
            v["tag"] = v["vw"] = w
            q = last_timeline_call(b)
            data_now = api(q) if q else data
            if q:
                say("对账基准取页面自己问的那一次：%s" % q)
            else:
                check("这一页问过时间线（一次都没问就是没接上）", False, "window.__calls 里没有 player-timeline")
            check_landing(v, data_now)
            if v["tl2"]:
                check_timeline(v, data_now)
            check_no_codes(v)
            # 换窗：这一页的窗控件在自己卡上，换完只有这张卡重问（上面那排名单与窗口无关）。
            b.js("""(() => {
              const bs = Array.from(document.querySelectorAll('.tl-when .seg button'));
              const t = bs.find((x) => /7 天/.test(x.textContent));
              if (t) t.click();
              return !!t;
            })()""")
            if b.wait("(() => { const on = document.querySelector('.tl-when .seg button.on');"
                      "return !!on && on.textContent.indexOf('7 天') >= 0; })()", tries=20, pause=0.2):
                time.sleep(1.4)
                q7 = last_timeline_call(b)
                big = api(q7) if q7 else None
                v7 = b.js(RECT)
                v7["tag"] = v7["vw"] = w
                if big is None:
                    check("换窗之后重问了一次时间线", False, "没记到新的 player-timeline 调用")
                elif "hours=168" not in q7:
                    check("换到近 7 天问的是 168 小时这一窗", False, q7)
                elif not v7["tl2"]:
                    # 云机上这一窗真的一条玩家行为都没记过（或人少到没段），那是实况。
                    say("换到近 7 天后没有画泳道（rows=%s）：这一窗这台机器没记录，几何比对跳过"
                        % big.get("rows"))
                else:
                    say("换到近 7 天，对账基准：%s" % q7)
                    check_timeline(v7, big)
            else:
                check("找得到「近 7 天」那一枚", False, "换窗那组断言全跳过")
            b.js("window.scrollBy(0, -10)")
            b.shoot(os.path.join(SHOTS, "%s_top.png" % tag))
            b.js("""(() => {
              const h = Array.from(document.querySelectorAll('.players-page .card-head h3'))
                .find((x) => x.textContent.trim() === %s);
              if (h) h.closest('section.card').scrollIntoView({block: 'start'});
            })()""" % json.dumps(CARD_TITLE, ensure_ascii=False))
            time.sleep(0.4)
            b.shoot(os.path.join(SHOTS, "%s_timeline.png" % tag))
            check("横向没有溢出（%s px）" % v["overflow"], v["overflow"] is not None
                  and int(v["overflow"]) <= 0, str(v["overflow"]))
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
