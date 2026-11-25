#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""总览页这一轮的六件事，在浏览器里逐条对账（任务 #95~#100）。

2026-11-04 用户看着总览页提了六条，其中两条是真问题、四条是"看不懂 / 看不出来"：
  1. 这一页能不能看到面板所在机器的 CPU 配置——以前只有核数，型号根本没读过。
  2. 「平均负载 0.12 / 0.30 / 0.25」是什么意思——三个裸数字，没有单位也没有对照物。
  3. 房间详情展开那一块的排版。
  4. 「这台机器上的占用 内存 2.0 MB · CPU 0%」合理吗——不合理，而且是采错了对象：
     界面拿的是 screen 会话进程的 pid，引擎是它的子进程。云机实测会话 2024 kB、
     引擎 882900 kB，所以这一格报的一直是那 2 MB 空壳。
  5. 在线趋势上方写着 Cluster_1（存档目录名），用户看不懂，要写房间名。
  6. 房间名可以点下来复制加入命令，但那枚图标只在悬停时才出现，太隐蔽
     ——用户明确说不要靠加文字解决。

为什么只能这样测：六条全在前端渲染层（第 4 条的口径在 Go 侧，由
internal/hoststats/hoststats_test.go 钉住"进程树要加起来"这一半），
接口探针看的是 JSON，看不出"这屏上有没有一颗看得见的图标""那三个数旁边有没有一句解释"。

它只读：不启停、不改配置。第 4 条在本地开发机上必然读不到进程占用（Windows 没有这一项），
脚本会明说是跳过的，真数字要到云机上跑这一份：
    CDP_BASE=http://119.29.239.62:8942 python .tools/overview96_check.py
"""
import json
import os
import re
import sys
import time

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from cdp import Browser, BASE, hover  # noqa: E402

SHOTS = os.path.join(HERE, "tmp", "overview96")
DESKTOP = (int(os.environ.get("OV_W", 1440)), int(os.environ.get("OV_H", 900)))
MOBILE = (390, 844)

fails = []


def check(name, ok, detail=""):
    print("  %s  %s%s" % ("ok  " if ok else "FAIL", name, ("  ｜" + detail) if detail else ""))
    if not ok:
        fails.append(name + ("（" + detail + "）" if detail else ""))
    return ok


def say(text):
    print("  ·  " + text)


# 存档目录名的样子：Cluster_1 / ProbeWorld 这类是面板读文件夹用的名字，
# 不该出现在"给人看的标题"位置上。
DIR_NAME = re.compile(r"^(cluster[_-]?\d+|probeworld|dst[_-]?\w*)$", re.I)


def login(b, tag):
    """进总览页。登录后装一遍异常钩子，整屏跑完再收。"""
    b.goto(BASE + "/?ov=%d%s#/" % (int(time.time()), tag))
    if b.wait("!!document.querySelector('.login-form')", tries=40, pause=0.3):
        b.js("""(() => {
          const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
          const f = document.querySelector('.login-form');
          set(f.querySelector('input[type=text], input:not([type])'), 'admin');
          set(f.querySelector('input[type=password]'), 'admin');
          sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}));
          f.querySelector('.login-submit').click();
        })()""")
    if not b.wait("!!document.querySelector('.host-strip')", tries=80, pause=0.3):
        b.shoot(os.path.join(SHOTS, tag + "_nologin.png"))
        raise SystemExit("没进到总览页：hash=%s 正文=%s" % (
            b.js("location.hash"), (b.js("document.body.innerText.slice(0,200)") or "")))
    b.js("window.__errs = [];"
         "addEventListener('error', (e) => window.__errs.push(String(e.message || e.error)));"
         "addEventListener('unhandledrejection', (e) => window.__errs.push('promise:' + String(e.reason)));")


# 「这台机器」那一栏的每一行：短标签、值、有没有一颗按需展开的说明。
HOST_ROWS = r"""(() => {
  const out = [];
  document.querySelectorAll('.host-strip .kv dt').forEach((dt) => {
    const dd = dt.nextElementSibling;
    if (!dd) return;
    out.push({
      k: dt.innerText.trim(),
      v: dd.innerText.trim(),
      hasHint: !!dd.querySelector('.tip-btn'),
    });
  });
  return out;
})()"""

# 打开某一行那颗说明并把气泡原文读回来。气泡是 v-if，不展开时它根本不在 DOM 里。
#
# 展开走真鼠标悬停：云机那一次 btn.focus() 之后 activeElement 已经是 .tip-btn，
# focus 事件却没走完（文档还没拿到焦点时 Chrome 就这个行为），气泡量不到、
# 四条说明断言全报"读回空串"；同一个页面上把鼠标移到那颗按钮上立刻就读到了。
# 本地夹具两种都开得了，所以这条红在本地永远看不见——只有指到公网那份成品才露出来。
def open_hint(b, row_key):
    got = b.js("""((key) => {
      const dts = Array.from(document.querySelectorAll('.host-strip .kv dt'));
      const dt = dts.find((x) => x.innerText.trim() === key);
      if (!dt) return {at: -1, noBtn: false, x: 0, y: 0};
      const btn = dt.nextElementSibling.querySelector('.tip-btn');
      if (!btn) return {at: dts.indexOf(dt), noBtn: true, x: 0, y: 0};
      const r = btn.getBoundingClientRect();
      return {at: dts.indexOf(dt), noBtn: false, x: r.left + r.width / 2, y: r.top + r.height / 2};
    })(""" + json.dumps(row_key, ensure_ascii=False) + ")")
    if not got or got.get("at", -1) < 0 or got.get("noBtn"):
        return ""
    b.js("window.__bubAt = %d;" % got["at"])
    read = """(() => {
      const dt = document.querySelectorAll('.host-strip .kv dt')[window.__bubAt];
      const bub = dt && dt.nextElementSibling.querySelector('.tip-bub');
      return bub ? bub.innerText.trim() : '';
    })()"""
    for _ in range(12):
        hover(b, got["x"], got["y"])
        time.sleep(0.25)
        t = b.js(read) or ""
        if t:
            return t
    # 悬停十二次都没开成才退到"点一下"（触屏用户走的就是这条路），读完由 close_hint 关。
    b.js("""(() => {
      const dt = document.querySelectorAll('.host-strip .kv dt')[window.__bubAt];
      const btn = dt && dt.nextElementSibling.querySelector('.tip-btn');
      if (btn) btn.click();
    })()""")
    b.wait("!!(document.querySelectorAll('.host-strip .kv dt')[window.__bubAt]"
           " && document.querySelectorAll('.host-strip .kv dt')[window.__bubAt]"
           ".nextElementSibling.querySelector('.tip-bub'))", tries=12, pause=0.2)
    return b.js(read) or ""


def close_hint(b):
    # 鼠标要挪开出这一行的悬停区，否则下一枚按钮的 mouseenter 不一定补得上；
    # 再补一次 blur，把上一次 focus 遗留的状态清掉。
    hover(b, 6, 6)
    b.js("""(() => {
      document.querySelectorAll('.tip-btn:focus').forEach((x) => x.blur());
      const dt = document.querySelectorAll('.host-strip .kv dt')[window.__bubAt || -1];
      const btn = dt && dt.nextElementSibling.querySelector('.tip-btn');
      if (btn && btn.closest('.tip') && btn.closest('.tip').classList.contains('open')) btn.click();
    })()""")
    time.sleep(0.2)


# 房间卡头部那枚名字按钮：图标在不悬停时是否可见、名字有没有下划线、有没有多写文字。
NAME_READ = r"""(() => {
  const btn = document.querySelector('.room-name');
  if (!btn) return null;
  const box = btn.querySelector('.ic-box');
  const ic = box ? box.querySelector('.ic') : null;
  const sp = btn.querySelector('span');
  const r = box ? box.getBoundingClientRect() : null;
  const cs = box ? getComputedStyle(box) : null;
  const ss = sp ? getComputedStyle(sp) : null;
  return {
    text: btn.textContent.trim(),
    onlySpan: Array.from(btn.childNodes).filter((n) => n.nodeType === 3 && n.textContent.trim()).length,
    hasIconSvg: !!(ic && ic.tagName.toLowerCase() === "svg"),
    iconOpacity: cs ? cs.opacity : null,
    iconShadow: cs ? cs.boxShadow : null,
    // DOMRect 的 width/height 挂在原型上，直接返回会被序列化成空对象，读回来是 None。
    iconBox: r ? {w: r.width, h: r.height} : null,
    decoLine: ss ? ss.textDecorationLine : null,
    decoStyle: ss ? ss.textDecorationStyle : null,
    cursor: getComputedStyle(btn).cursor,
    // 字色和它落在的那张卡的底色：深色主题那一次要拿它们算对比，
    // 否则"白底白字"这种红在截图里才看得见，量 opacity 是量不出来的。
    fg: ss ? ss.color : null,
    bg: (() => {
      let el = btn;
      while (el && el !== document.body) {
        const c = getComputedStyle(el).backgroundColor;
        if (c && c !== "rgba(0, 0, 0, 0)" && c !== "transparent") return c;
        el = el.parentElement;
      }
      return getComputedStyle(document.body).backgroundColor;
    })(),
  };
})()"""


def rgb_of(s):
    """把计算样式里的颜色串成 (r,g,b) 0~1；解析不了返回 None。

    Chrome 现在会把 color-mix() 序列化成 "color(srgb 0.95 0.96 0.98 / 0.6)"，
    而普通颜色是 "rgb(219, 226, 238)"，两种都得吃——只认前者的话这一条会静默变成 None。
    """
    if not s:
        return None
    m = re.match(r"rgba?\(\s*([0-9.]+)[ ,]+([0-9.]+)[ ,]+([0-9.]+)", s)
    if m:
        return tuple(float(x) / 255.0 for x in m.groups())
    m = re.match(r"color\(\s*srgb\s+([0-9.]+)[ ,]+([0-9.]+)[ ,]+([0-9.]+)", s)
    if m:
        return tuple(float(x) for x in m.groups())
    return None


def contrast(a, b):
    """WCAG 的相对亮度比，1=一样看不见，21=黑白。任一侧解析不了就返回 None。"""
    if not a or not b:
        return None
    lum = lambda c: 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]  # noqa: E731
    la, lb = lum(a), lum(b)
    hi, lo = max(la, lb), min(la, lb)
    return (hi + 0.05) / (lo + 0.05)

# 两张卡现在各一张（#110 拆的家；#125 把泳道搬去玩家管理页、#124 补了行为构成），
# 一次读回来：标题 + 卡头里最后一枚胶囊 + 它的悬停。
# 胶囊只取卡头那几枚（.card-head .chip），免得把正文里的"在线 2/6"这类也算进来。
TREND_READ = r"""(() => {
  const pick = (title) => {
    const cards = Array.from(document.querySelectorAll('.card'));
    const card = cards.find((c) => {
      const h = c.querySelector('.card-head h3');
      return h && h.innerText.trim() === title;
    });
    if (!card) return null;
    const chips = Array.from(card.querySelectorAll('.card-head .chip'));
    const last = chips[chips.length - 1];
    return {
      chip: last ? last.textContent.trim() : '',
      title: last ? (last.getAttribute('title') || '') : '',
      chips: chips.map((x) => x.textContent.trim()),
      // 时间窗控件在不在自己这一张卡里，用来认拆家有没有拆对。
      hasSeg: !!card.querySelector('.seg'),
    };
  };
  return {trend: pick('在线情况'), beh: pick('行为构成')};
})()"""

# 房间里"标题用的名字"和"目录名"分别是什么：用来判断趋势那格写对了没有。
# 手机上目录名那枚胶囊是 hide-sm（display:none），innerText 会读成空串，所以一律 textContent。
ROOMS_READ = r"""(() => {
  const out = [];
  document.querySelectorAll('section.card').forEach((c) => {
    const btn = c.querySelector('.card-head h2 .room-name');
    if (!btn) return;
    const dirChip = Array.from(c.querySelectorAll('.card-head .chip.mono'))[0];
    out.push({name: btn.textContent.trim(), dir: dirChip ? dirChip.textContent.trim() : ''});
  });
  return out;
})()"""


# #104 + #111：世界行上现在该看得见什么，一行一行读。
# 玩家实况自己摊开的判据仍然由接口给（进程在跑 + 采集没停在旧时刻 + 这次真有人），
# 而「详情」那一档在 #111 整块拆掉了：摊开的只剩玩家，明细表不再有任何来路。
# 占用那一枚（proc）和四段悬停（hover）就是拆家之后信息没丢的凭证，所以要一行行读回来。
LIVE_READ = r"""(() => {
  return Array.from(document.querySelectorAll('.shard-row')).map((r) => {
    const d = r.querySelector(':scope > .shard-detail');
    const chipText = Array.from(r.querySelectorAll(':scope > .chip')).map((x) => x.innerText.trim());
    return {
      name: (r.querySelector('.shard-name') || {}).innerText || '',
      // 进程在跑由那颗绿徽决定（.badge.running 就是 x-shard-row 里"在跑"那一枚）。
      running: !!r.querySelector('.badge.running'),
      // 这一行报了几个人：从"在线 2/6"那枚胶囊里取，取不到算 0。
      // 摊不摊开的判据就是"在跑 + 有人"，所以断言得先知道有没有人。
      online: Number((r.innerText.match(/在线\s*([0-9]+)\s*\/\s*[0-9]+/) || [0, 0])[1]),
      hasDetail: !!d,
      players: d ? d.querySelectorAll('.detail-players .stat').length : 0,
      // 明细表（服务器名/世界/在线那五行）在 #111 之后不该在任何地方出现。
      hasTop: !!r.querySelector('.detail-top'),
      blocks: d ? d.querySelectorAll('.detail-players').length : 0,
      // 行上这一枚就是原来那格「这台机器上的占用」；停着的世界不该有它。
      proc: (chipText.find((t) => /内存|占用/.test(t)) || '').trim(),
      // 原来明细表独有的四段话，现在全在悬停里，一处都不能丢。
      hover: Array.from(r.querySelectorAll('[title]')).map((x) => x.getAttribute('title')),
      // 重启/强停后来搬进了 .actions-danger 那一组（危险操作单独分组，别和查看类按钮
      // 等大并列）；只数 :scope > .btn 会把这两枚漏成"功能没了"。分组仍是这一行的孩子，
      // 所以"行上直接点得到"这条断言照旧成立，只是选择器要连那一组一起数。
      buttons: Array.from(r.querySelectorAll(':scope > .btn, :scope > .actions-danger > .btn'))
        .map((x) => x.innerText.trim()),
      // 有没有那一排数字胶囊 = 这一轮拿没拿到快照（拿不到时下面那些悬停本来就不该有）。
      hasSt: !!r.querySelector(':scope > .row'),
    };
  });
})()"""


# 摊开的那一块只剩玩家：三维是不是一行一个、角色名是不是中文译名，都从这里读。
PLAYERS_READ = r"""(() => {
  const d = document.querySelector('.shard-detail');
  if (!d) return null;
  const cs = getComputedStyle(d);
  const firstBars = d.querySelector('.detail-players .stat .player-bars');
  return {
    border: cs.borderTopWidth + ' ' + cs.borderTopStyle,
    bg: cs.backgroundColor,
    playerCount: d.querySelectorAll('.detail-players .stat').length,
    barsPerPlayer: (() => {
      const p = d.querySelector('.detail-players .stat .player-bars');
      return p ? p.children.length : 0;
    })(),
    barCols: firstBars ? getComputedStyle(firstBars).gridTemplateColumns.split(' ').length : 0,
    // 一行一个 = 三条的顶边必须一条比一条低（并排时是同一个 top）。
    barTops: firstBars ? Array.from(firstBars.children).map((x) =>
      Math.round(x.getBoundingClientRect().top)) : [],
    // 角色那一行：#102 之前这里摆的是引擎码值（wilson），现在必须是中文译名。
    charNotes: Array.from(d.querySelectorAll('.detail-players .stat .stat-note'))
      .map((x) => x.innerText.trim()),
  };
})()"""


def first_expanded(b):
    """等到有一个世界自己摊开了（有人在），返回有没有。

    #111 之后没有「详情」这颗按钮了，摊开只由那一轮采集里真有人决定，
    所以这里不再点击，只等夹具里那两个玩家进服的那一行把块画出来。
    """
    return b.wait("!!document.querySelector('.shard-detail')", tries=40, pause=0.3)


def no_detail_button(b):
    """页面上还有没有"详情"这颗按钮（拆档这一刀的钉子）。"""
    return b.js("""[...document.querySelectorAll('button')]
      .filter((x) => x.innerText.trim() === '详情').length""") or 0


def rss_from(text):
    """从「内存 862 MB · CPU 3%」里取出内存那个数，统一换成 MB；取不到返回 None。"""
    m = re.search(r"内存\s*([0-9.]+)\s*(B|KB|MB|GB|G|T)", text)
    if not m:
        return None
    v = float(m.group(1))
    unit = m.group(2).upper()
    scale = {"B": 1.0 / 1024 / 1024, "MB": 1.0,
             "GB": 1024.0, "G": 1024.0, "T": 1024.0 * 1024}[unit]
    return v * scale


def detail_after_row_bits(b):
    """玩家实况块在这一行里排到最后了没有（窄屏那一处 order 事故的钉子）。

    为什么只能这么量：`.shard-row` 是 flex-wrap 容器，≤560 那段用 order 把按钮和
    「第几天」提前，而这块当时没写 order（=0），于是它插到了它们前面——
    截图上看着只是"顺序有点怪"，量 rect 才说得清。这里比的是画出来的上下位置，
    不是 DOM 顺序（DOM 顺序两边都对）。
    """
    return b.js(r"""(() => {
      const d = document.querySelector('.shard-detail');
      if (!d) return null;
      const row = d.closest('.shard-row');
      const dTop = d.getBoundingClientRect().top;
      const after = [];
      row.querySelectorAll(':scope > .btn, :scope > .btn-group, :scope > .row, :scope > .shard-note').forEach((el) => {
        const cs = getComputedStyle(el);
        if (cs.display === 'none') return;
        if (el.getBoundingClientRect().top > dTop) after.push((el.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 10));
      });
      const total = Array.from(row.querySelectorAll(':scope > .btn, :scope > .btn-group, :scope > .row, :scope > .shard-note'))
        .filter((el) => getComputedStyle(el).display !== 'none').length;
      return {dTop: Math.round(dTop), after: after, total: total};
    })()""")


def align_rects(b):
    """量总览页各板块的顶边（2026-11-04 用户指出"各个板块似乎没有对齐"）。

    为什么只能这么量：三处毛病都不改变 DOM 顺序，只在画出来的位置上——
      ① 「房间」那行标题原来塞在左列里，左列首卡被推下去 43px（实测 567 / 524）；
      ② dl 的浏览器默认上边距 1em（这里 12.5px）把「这台机器」右侧明细表整块压低，
         第一行比表盘低一截（333 / 321），详情块里那颗"世界号"同样（937 / 924）；
      ③ 三枚环里带说明气泡的那一枚（存档盘）名字行高出 5px，把说明文字顶下去，
         三枚于是各排各的（顶边 450 / 450 / 455，底边 466 / 482 / 471）。
    接口探针和单测都看不见这些，截图上又容易被当成"差不多"，所以直接比 rect。

    带回来的是 rect（含 bottom / left）不只是顶边：窄屏里这两块本来就是上下排的，
    只有并排时才该比顶边，判据见 side_by_side。
    """
    return b.js(r"""(() => {
      const rect = (el) => {
        if (!el) return null;
        const r = el.getBoundingClientRect();
        return {top: Math.round(r.top + scrollY), bottom: Math.round(r.bottom + scrollY),
                left: Math.round(r.left), width: Math.round(r.width)};
      };
      const grid = document.querySelector('.grid-wide:not(.host-row)');
      const cols = grid ? Array.from(grid.children).filter((c) => c.classList.contains('col')) : [];
      const cardRects = cols.map((c) => rect(c.querySelector('.card')));
      const colRects = cols.map((c) => rect(c));
      // #117 顶部这一档是两张卡：量两卡各自的边界，和下方那一档的两列比。
      // #109/#115/#116 三代都在这里量"一栏之内"的东西（中缝、竖线、留白），那时候两半
      // 共用一张卡；现在右半搬出去了，该量的是两张卡的边是不是真的重合到下方两列。
      const row = document.querySelector('.host-row');
      const strip = document.querySelector('.host-strip');
      // 2026-11-09 那张「未结束的任务」整张下掉：这里反过来钉"没留半截"。
      const live = document.querySelector('.host-live');
      const liveRows = document.querySelectorAll('.host-live .list-plain > li').length;
      const rightCol = cols[1] ? rect(cols[1]) : null;
      // 右侧那列明细表每一行的"值"那一格有多宽多高：被挤成一个字一行时，宽度只剩几十字、
      // 高度却是几十行（2026-11-04 两栏拆开后 1440 视口实测 80px 宽 / 700px 高）。
      const kvCells = Array.from(document.querySelectorAll('.host-kv dd')).map((el) => {
        const q = el.getBoundingClientRect();
        return {w: Math.round(q.width), h: Math.round(q.height),
                t: (el.innerText || '').trim().slice(0, 24)};
      });
      // #110 两张图各自一张卡：量两卡之间的实际空隙（上一张的底到下一张的顶）。
      const insight = Array.from(document.querySelectorAll('.card.ov-insight'));
      const gapBetween = insight.length > 1
        ? Math.round(rect(insight[1]).top - rect(insight[0]).bottom) : null;
      // 这一栏的卡头里不该再有「每 5 秒自动刷新」那几个字（用户：根本不需要）。
      const headText = (() => {
        const h = document.querySelector('.host-strip .card-head');
        return h ? h.innerText.replace(/\s+/g, '') : '';
      })();
      return {
        cardTops: cardRects.map((r) => r && r.top),
        cardLefts: colRects.map((r) => r && r.left),
        cardWidths: colRects.map((r) => r && r.width),
        dial: rect(document.querySelector('.host-rings .ring-dial')),
        kv: rect(document.querySelector('.host-kv')),
        kvCells: kvCells,
        notes: Array.from(document.querySelectorAll('.host-rings .ring-note')).map((n) => rect(n).top),
        // 两卡之间的缝 = 左卡右缘到右卡左缘；它应当等于下方两列之间的那道缝。
        rowGap: row ? getComputedStyle(row).columnGap : '',
        rowCols: row ? getComputedStyle(row).gridTemplateColumns : '',
        rowKids: row ? row.children.length : 0,
        strip: rect(strip),
        stripHead: strip ? strip.querySelector('.card-head h3').innerText.trim() : null,
        liveGone: !live,
        liveRows: liveRows,
        rightColLeft: rightCol ? rightCol.left : null,
        rightColRight: rightCol ? rightCol.left + rightCol.width : null,
        // 下方两列之间那道缝实测多宽：上面那一行的列间距要和它一样，才叫同一种分隔。
        belowGap: cols.length > 1
          ? Math.round(rect(cols[1]).left - (rect(cols[0]).left + rect(cols[0]).width)) : null,
        hostHead: headText,
        cards: insight.length,
        gapBetween: gapBetween,
        segInFirst: !!(insight[0] && insight[0].querySelector('.seg')),
        segInSecond: !!(insight[1] && insight[1].querySelector('.seg')),
        heads: insight.map((c) => {
          const h = c.querySelector('.card-head h3');
          return h ? h.innerText.trim() : '';
        }),
      };
    })()""")


def side_by_side(a, b):
    """两块是不是排在同一行（左右并排），还是上下排。

    对齐断言只能在并排时比顶边：窄屏里 .host-body 收成单列、「详情块」那一栏的两块也会折行，
    那是设计如此，不是错位（实测 390：表 642 / 盘 478）。判据取纵向重叠——并排时两块必然
    在竖直方向上叠着，上下排则中间有空隙。"""
    if not a or not b:
        return False
    return min(a["bottom"], b["bottom"]) - max(a["top"], b["top"]) > 6


def main():
    os.makedirs(SHOTS, exist_ok=True)
    for (w, h), tag in ((DESKTOP, "wide"), (MOBILE, "narrow")):
        print("===== 视口 %dx%d（%s）=====" % (w, h, tag))
        b = Browser(w, h)
        try:
            login(b, tag)
            # 390 档主机指标按设计默认折叠（首屏先让给房间）。这一组断言问的是
            # 「这一栏有没有处理器型号 / 平均负载」，折叠态下 kv 表根本不在 DOM，
            # 读成"功能没了"是探针的错。先展开再读，不改产品行为。
            b.js("(() => { const btn = [...document.querySelectorAll('.host-strip .btn')]"
                 ".find(x => /展开指标/.test(x.textContent)); if (btn) btn.click(); return true; })()")
            time.sleep(0.3)
            if not b.wait("!!document.querySelector('.shard-row')", tries=80, pause=0.3):
                b.shoot(os.path.join(SHOTS, tag + "_norooms.png"))
                check("这一屏有房间行", False, "页面停在" + str(b.js("location.hash")))
                continue
            # 等过一次完整轮询：CPU/网速那一格要有区间才有数
            time.sleep(6)
            rows = b.js(HOST_ROWS) or []
            by_key = {r["k"]: r for r in rows}

            # ---- #96 处理器型号 ----
            model = by_key.get("处理器型号")
            if check("这台机器那一栏有「处理器型号」这一行", model is not None,
                     "现有行：" + " / ".join(r["k"] for r in rows)):
                v = model["v"]
                check("型号读到了真东西，不是空或「没读到」",
                      bool(v) and "没读到" not in v and "读不到" not in v, v)
                check("同一行里带了核数", "核" in v, v)
                check("型号旁边有一颗按需展开的说明", model["hasHint"])
                cores = None
                m = re.search(r"([0-9]+)\s*核", v)
                if m:
                    cores = m.group(1)
                tip = open_hint(b, "处理器型号")
                close_hint(b)
                # 先钉"展得开"再钉"写清了"：少了这一条，气泡没出来时只会表现成
                # 四条"内容不对"，读报告的人会去改文案，而真要修的是展开那条链路。
                check("那颗说明展得开（读回的是气泡原文，不是空串）", bool(tip),
                      "悬停与点击两条路都没让气泡出现" if not tip else tip[:40])
                check("说明里把「和整机那圈同一个分母」说清了",
                      bool(tip) and ("分母" in tip or "跑满一个核" in tip), tip[:60])
                if tip and cores:
                    check("说明里写的就是这台机器的核数（%s 核），不是举例" % cores,
                          cores + " 核" in tip, tip[:80])

            # ---- #97 平均负载 ----
            load = by_key.get("平均负载")
            if check("「平均负载」这一行还在", load is not None):
                if "/" in load["v"]:
                    check("三个数是摆出来的（不是只剩一句说明）",
                          bool(re.match(r"^[0-9.]+\s*/\s*[0-9.]+\s*/\s*[0-9.]+$", load["v"])), load["v"])
                    check("摆出三个数时旁边有说明", load["hasHint"])
                    tip = open_hint(b, "平均负载")
                    close_hint(b)
                    check("这句说明展得开（读回的是气泡原文，不是空串）", bool(tip),
                          "悬停与点击两条路都没让气泡出现" if not tip else tip[:40])
                    ok_words = all(x in tip for x in ("1 分钟", "5 分钟", "15 分钟"))
                    check("说明逐个点名了这三个数是哪三段", ok_words, tip[:80])
                    check("说明给了对照物（这台机器的核数）",
                          bool(cores) and (cores + " 核" in tip), tip[:120] if cores else "没拿到核数")
                    check("说明说了怎么判断（接近就是排满）", "排满" in tip, tip[:100])
                else:
                    check("这台机器没有平均负载这一项时，只留那句说明、不编三个数",
                          not load["hasHint"], load["v"])
                    say("这台机器读不到平均负载（%s），那三条断言到 Linux 上才会跑到" % load["v"])

            # ---- #100 房间名的可发现性（不加文字） ----
            nm = b.js(NAME_READ)
            if check("房间名是一颗按得下来的按钮", nm is not None and nm.get("cursor") == "pointer"):
                check("复制图标不悬停也在（opacity 为 1）", nm["iconOpacity"] == "1",
                      str(nm["iconOpacity"]))
                check("图标有自己可见的一小块（带边框/底）",
                      nm["iconShadow"] and nm["iconShadow"] != "none",
                      str(nm["iconShadow"])[:40])
                box = nm.get("iconBox") or {}
                check("那一小块真有尺寸（不是 0 宽）",
                      float(box.get("w", 0)) >= 14 and float(box.get("h", 0)) >= 14,
                      "%sx%s" % (box.get("w"), box.get("h")))
                check("名字带下划线这类可点暗示",
                      nm["decoLine"] and "underline" in nm["decoLine"], str(nm["decoLine"]))
                check("没有靠加文字实现（按钮里除了名字没有别的裸文字）",
                      nm["onlySpan"] == 0, "裸文字节点 %s 个" % nm["onlySpan"])
                ct = contrast(rgb_of(nm.get("fg")), rgb_of(nm.get("bg")))
                check("名字和它落在的底色读得出来（浅色主题）", ct is not None and ct >= 4.5,
                      "%s on %s = %s" % (nm.get("fg"), nm.get("bg"),
                                         ("%.1f" % ct) if ct else "算不出"))

            # ---- #99 在线趋势那格写房间名 + #110 两张图各一张卡（#125 之后下面那张是行为构成） ----
            tr = b.js(TREND_READ)
            rooms = b.js(ROOMS_READ) or []
            both = bool(tr) and tr.get("trend") and tr.get("beh")
            if check("「在线情况」和「行为构成」两张卡都找得到", bool(both),
                     "趋势卡%s / 行为卡%s" % ("在" if tr and tr.get("trend") else "缺",
                                              "在" if tr and tr.get("beh") else "缺")):
                dirs = [r["dir"] for r in rooms if r["dir"]]
                names = [r["name"] for r in rooms]
                tc = tr["trend"]
                check("趋势那格的标题不是存档目录名", bool(tc["chip"]) and not DIR_NAME.match(tc["chip"]),
                      "chip=「%s」，页面上的目录名有 %s" % (tc["chip"], dirs))
                if names:
                    check("它等于页面上某一个房间名", tc["chip"] in names,
                          "chip=「%s」，房间名有 %s" % (tc["chip"], names))
                check("目录名没丢，挪进了悬停",
                      any(d and d in tc["title"] for d in dirs) or not dirs, tc["title"])
                # 分家之后最容易出的两个岔子：窗控件被复制成两枚（两边各选各的窗），
                # 或者下面那张卡不写自己用的是哪一窗（读者得记住上面刚点了什么）。
                check("时间窗控件只在上面那张卡里", tc["hasSeg"] and not tr["beh"]["hasSeg"],
                      "上面%s / 下面%s" % (tc["hasSeg"], tr["beh"]["hasSeg"]))
                win = [c for c in tr["beh"]["chips"] if c.startswith("近")]
                check("下面那张卡自己写着用的是哪一窗", len(win) == 1,
                      "卡头胶囊：%s" % " / ".join(tr["beh"]["chips"]))
                # 合计那枚跟着搬到下面这张卡（逐类条形的加起来等不等于它，由 overview105_check 钉）。
                check("「N 次行为」这一枚在下面那张卡上",
                      any("次行为" in c for c in tr["beh"]["chips"]),
                      " / ".join(tr["beh"]["chips"]))
                check("上面那张卡不再报行为次数（那是下面那张的事）",
                      not any("行为" in c for c in tc["chips"]), " / ".join(tc["chips"]))

            # ---- #104 + #111：行上该看得见什么 ----
            # 首页那片空不是页面底下（1440x900 实测尾部只空 20px），是"在跑的世界
            # 不点详情就只剩一行胶囊"。#104 让有人的世界自己摊开，#111 把「详情」那一档整块拆掉：
            # 占用做成行上的一枚胶囊，原来那张明细表独有的四段话进悬停。
            # 所以这一组钉子看四件事：该摊的摊了、不该摊的没摊、行上没有「详情」也没有明细表、
            # 而拆掉那四行话在悬停里一个字都没少。
            rows = b.js(LIVE_READ) or []
            if check("这一轮读到了世界行", bool(rows),
                     "%d 行，在跑 %d 行" % (len(rows), len([r for r in rows if r["running"]]))):
                busy = [r for r in rows if r["running"] and r["online"] > 0]
                idle_open = [r["name"] for r in rows if not r["running"] and r["hasDetail"]]
                half = [r for r in busy if r["hasDetail"] and r["players"]]
                if busy:
                    check("在跑且有人的世界，玩家不点任何东西也摊着",
                          len(half) == len(busy),
                          "该摊 %d 行 / 摊对 %d 行 ｜ %s" % (
                              len(busy), len(half),
                              " / ".join("%s 在线%s 摊开%s" % (
                                  r["name"], r["online"], r["hasDetail"]) for r in busy)))
                    check("摊出来的那一块里人真的在（不是空壳）",
                          all(r["players"] > 0 for r in half),
                          " / ".join("%s %s 人" % (r["name"], r["players"]) for r in half))
                else:
                    say("这一轮没有在跑且有人的世界，摊开那条断言跳过")
                check("停着的世界不摊开（旧快照不冒充现在）", not idle_open,
                      "摊开的：%s" % " / ".join(idle_open) if idle_open else "没有一行摊着")
                # ---- #111 拆档这一刀的三条款 ----
                check("页面上没有「详情」这颗按钮了", no_detail_button(b) == 0,
                      "还剩 %s 颗" % no_detail_button(b))
                check("那张五行明细表没有任何来路", not any(r["hasTop"] for r in rows),
                      "还带着明细表的：%s" % " / ".join(r["name"] for r in rows if r["hasTop"]) or "没有")
                run_rows = [r for r in rows if r["running"]]
                check("在跑的世界行上都挂着一枚占用", all(r["proc"] for r in run_rows),
                      " / ".join("%s=「%s」" % (r["name"], r["proc"]) for r in run_rows) or "没有在跑的世界")
                check("停着的世界不报占用（没有进程就没有占用）",
                      not [r for r in rows if not r["running"] and r["proc"]],
                      "报了的：%s" % " / ".join("%s=「%s」" % (r["name"], r["proc"])
                                                for r in rows if not r["running"] and r["proc"]) or "没有")
                check("重启和强停在行上直接点得到（原来要先进详情）",
                      all({"重启", "强停"} <= set(r["buttons"]) for r in run_rows) if run_rows else True,
                      " / ".join("%s=%s" % (r["name"], r["buttons"]) for r in run_rows))
                # 原来明细表独有的四段话现在全在悬停里，一个字都不许丢。
                # （这个局部变量别叫 hover——它是 cdp.hover 那个发真鼠标事件的函数名，
                #   在 main 里一赋值就把助手在本函数内遮掉了。）
                tips_text = " ".join(" ".join(r["hover"]) for r in rows if r["running"] and r["hasSt"])
                for phrase, whence in (("服务器名", "名字那一枚胶囊"), ("世界号", "同上"),
                                       ("最后一次采集", "数字那一排的落款"),
                                       ("这一轮世界规则", "季节胶囊"), ("screen 会话进程号", "在跑那一枚")):
                    check("悬停里还写着「%s」（%s）" % (phrase, whence),
                          phrase in tips_text, tips_text[:90] + "……")

            # 摊开只由"那一轮采集里真有人"决定（#111 拆掉「详情」档之后没有别的来路），
            # 所以云机此刻没玩家时这一整块没有东西可量——那是实况，不是 bug（2026-11-04 云机实测：
            # 两个世界在跑、在线 0 人，脚本原来无条件断言"摊开了"，报的是假红）。
            busy_now = [r for r in rows if r.get("running") and r.get("online", 0) > 0] if rows else []
            if not busy_now:
                say("这一轮没有人在里面，玩家实况那一块的读法跳过（本地夹具要跑到它请 reseed）")
            expanded = bool(busy_now) and check("有人的世界自己把玩家实况摊开了", first_expanded(b))
            if expanded:
                d = b.js(PLAYERS_READ)
                if check("摊开的那一块读得到（.shard-detail）", d is not None):
                    check("这块有自己的边框和底（不再是一行脚注）",
                          d["border"].startswith("1px solid") and d["bg"] != "rgba(0, 0, 0, 0)",
                          "%s / %s" % (d["border"], d["bg"]))
                    dup = [r["name"] for r in (b.js(LIVE_READ) or [])
                           if r["hasDetail"] and r["blocks"] != 1]
                    check("玩家块只有一份（摊开和别的来路不会各画一遍）", not dup,
                          "份数不是 1 的：%s" % " / ".join(dup) if dup
                          else " / ".join("%s=%s 份" % (r["name"], r["blocks"])
                                          for r in (b.js(LIVE_READ) or []) if r["hasDetail"]))
                    # 这块要压在这一行的最后：窄屏那段 order 规则没带上它时，
                    # 它会插到「停止 / 第几天」上面，把一行切成四截。
                    seq = detail_after_row_bits(b)
                    if seq:
                        check("摊开的块排在这一行最后（按钮与「第几天」在它上面）",
                              len(seq["after"]) == 0,
                              "块 top=%s，仍在它下面的：%s" % (seq["dTop"], " / ".join(seq["after"]) or "无"))
                    if d["playerCount"]:
                        # 2026-11-04 用户要求回到一行一个：并排只看得出长短，
                        # 竖排才比得出"同一条计量在人与人之间差多少"。
                        check("每人三条计量都在，且一行一个",
                              d["barsPerPlayer"] == 3 and d["barCols"] == 1,
                              "每人 %s 条 / %s 列" % (d["barsPerPlayer"], d["barCols"]))
                        tops = d["barTops"]
                        if len(tops) == 3:
                            check("三条确实是上下排的（顶边一条比一条低）",
                                  tops[0] < tops[1] < tops[2], "顶边 %s" % tops)
                        # 角色名：那一行以前摆的是引擎码值（wilson / wormwood）。
                        # 认不得的角色界面写"角色未识别"，所以这里凡是冒出四个以上连续
                        # 小写字母就算漏（WX-78 是大写，不会误伤）。
                        leak = [t for t in d["charNotes"] if re.search(r"[a-z][a-z0-9_]{3,}", t)]
                        check("角色那一行是中文译名，没有露引擎码值",
                              not leak,
                              "还是码值：%s" % " / ".join(leak) if leak
                              else " / ".join(d["charNotes"]))
                    else:
                        say("这次采集里没有玩家，三条计量与角色名的断言跳过")
                    # ---- #95 + #111 行上那一枚占用的数字是不是引擎的 ----
                    occ = [r["proc"] for r in (b.js(LIVE_READ) or []) if r["running"] and r["proc"]]
                    occ = occ[0] if occ else ""
                    if not occ:
                        say("这一屏没有在跑的世界，占用那一枚的断言跳过")
                    elif "刚开始采样" in occ or "采不到" in occ or "这个系统" in occ:
                        say("这台机器读不到进程占用（%s）——内存那一条到云机上验" % occ)
                    else:
                        mb = rss_from(occ)
                        check("行上那枚占用报的不是 2 MB 那层空壳",
                              mb is not None and mb >= 100, occ)
            # ---- #101 各板块的顶边（用户："总览里的各个板块似乎没有对齐"）----
            a = align_rects(b)
            # ---- #115 这一栏的卡头不该出现「每 5 秒自动刷新」----
            check("这一栏的卡头里没有「每 5 秒自动刷新」那几个字",
                  "秒自动刷新" not in a["hostHead"], a["hostHead"][:70])
            tops, lefts = a["cardTops"], a["cardLefts"]
            if None not in tops and len(tops) == 2 and lefts[0] != lefts[1]:
                check("两列首卡在同一条线上", max(tops) - min(tops) <= 1,
                      "左 %s / 右 %s" % (tops[0], tops[1]))
            elif None not in tops and len(tops) == 2:
                say("这一档两列是上下排的（窄屏），首卡顶边那条比对跳过")
            # ---- 2026-11-09：右半幅「未结束的任务」整张下掉，这一行只剩一张卡 ----
            # 顶栏那枚计数实时、可点、直达任务中心；任务中心本来就有进度、明细和结论，
            # 总览再铺半幅空态等于同一件事说两遍。判据从"两卡的边压在下方两列上"
            # 换成"一张卡横跨整幅"，顺带钉住没有留下半截 .host-live / 任务列表。
            check("总览上不再有「未结束的任务」那张卡",
                  a["liveGone"] and a["liveRows"] == 0,
                  "还留着卡=%s / %d 行任务" % (not a["liveGone"], a["liveRows"]))
            check("这一行只剩「这台机器」一张卡",
                  a["rowKids"] == 1 and a["stripHead"] == "这台机器",
                  "%d 个直接子元素 ｜ 卡头「%s」" % (a["rowKids"], a["stripHead"]))
            if a["strip"] and a["cardLefts"] and a["rightColRight"] is not None:
                stripRight = a["strip"]["left"] + a["strip"]["width"]
                check("这台机器卡横跨整幅（左缘压下方左列、右缘压下方右列）",
                      abs(a["strip"]["left"] - a["cardLefts"][0]) <= 1
                      and abs(stripRight - a["rightColRight"]) <= 1,
                      "卡 %s~%s / 下方 %s~%s" % (
                          a["strip"]["left"], stripRight,
                          a["cardLefts"][0], a["rightColRight"]))
            else:
                check("这台机器卡或下方两列没量到", False,
                      "strip=%s rightColRight=%s" % (a["strip"], a["rightColRight"]))
            # 「环在左、那列绝对量在右」仍是主读法：宽档不许折到环底下。
            # 整幅单卡之后行宽比原来半幅宽得多，宽档判定改看这张卡自己有多宽，
            # 不再看 .host-row 解析出几轨（现在恒为单轨）。
            wide = bool(a["strip"] and a["strip"]["width"] >= 900)
            if wide:
                check("wide 档明细表就排在表盘右边（不许折到环底下）",
                      side_by_side(a["kv"], a["dial"]),
                      "表 top=%s left=%s / 盘 top=%s left=%s ｜ 卡宽 %s" % (
                          a["kv"] and a["kv"]["top"], a["kv"] and a["kv"]["left"],
                          a["dial"] and a["dial"]["top"], a["dial"] and a["dial"]["left"],
                          a["strip"]["width"]))
                if side_by_side(a["kv"], a["dial"]):
                    check("「这台机器」右侧明细表与表盘顶边齐",
                          abs(a["kv"]["top"] - a["dial"]["top"]) <= 2,
                          "表 %s / 盘 %s" % (a["kv"]["top"], a["dial"]["top"]))
            else:
                say("窄档由 .host-body 的 flex-wrap 自己折行，主读法那两条跳过")
            # 这一条是 2026-11-04 那处回归的守门：两栏拆开后左栏只有 680px，
            # 而表盘按内容要 572px，明细表被挤到 80px——处理器型号竖着排了一个字一行。
            # 顶边那条断言对这种塌法完全无感（两块都还在，只是其中一块被压成一条）。
            thin = [c for c in a["kvCells"] if c["w"] < 100]
            tall = [c for c in a["kvCells"] if c["h"] > 70]
            check("明细表每一格都放得下话（没有挤成一个字一行）",
                  not thin and not tall,
                  "最窄 %spx / 最高 %spx ｜ %s" % (
                      min([c["w"] for c in a["kvCells"]] or [0]),
                      max([c["h"] for c in a["kvCells"]] or [0]),
                      " / ".join(c["t"] for c in (thin or tall)[:2])))
            if len(a["notes"]) >= 2:
                check("三枚环的说明文字在同一条线上",
                      max(a["notes"]) - min(a["notes"]) <= 1, "顶边 %s" % a["notes"])
            # ---- #110 两张图之间的缝 ----
            # #109 那套"量一栏之内最右的东西到标题左缘"的读法已经跟着 #117 的结构一起退场：
            # 顶部这一档现在钉的是两张卡的边框、左右缘和列间距（见上面那一段），
            # 这里只剩下方那两张图还是"两张卡之间留没留出缝"。
            check("在线情况和行为构成是两张卡", a["cards"] == 2,
                  "读到 %s 张：%s" % (a["cards"], a["heads"]))
            if a["gapBetween"] is not None:
                check("两张卡之间留出了真正的缝", a["gapBetween"] >= 8, "%spx" % a["gapBetween"])
            # 截图要滚到那一块再拍：这一屏从上到下是"这台机器 / 房间 / 两张图"，
            # 不滚的话两张图拍的都是同一页顶部，而世界行正是这一轮要人看的那处改动。
            b.js("""(() => {
              const d = document.querySelector('.shard-row') || document.querySelector('.ov-insight');
              if (d) d.scrollIntoView({block: 'start'});
            })()""")
            time.sleep(0.4)
            b.shoot(os.path.join(SHOTS, "%s_rooms.png" % tag))

            # 用户看的是深色主题那两屏，图标常驻这一块在两套主题里都得成立：
            # 浅色下那颗小方块靠 #dae2ee 的描边，深色下换成 --line，是另一个颜色。
            b.js("""(() => {
              const btn = document.querySelector('[aria-label=\"切到深色\"]');
              if (btn) btn.click();
            })()""")
            b.wait("document.documentElement.getAttribute('data-theme') === 'dark'", tries=20, pause=0.2)
            # 等过渡跑完再量：这一轮给 .room-name 和 .btn 都加了 color/background 过渡
            # （约 180ms），点完立刻读会读到浅色那一头的值，截图上则是白底白字的一坨。
            time.sleep(1.0)
            nm2 = b.js(NAME_READ)
            if nm2:
                check("深色主题下图标照样常驻（不悬停）", nm2["iconOpacity"] == "1",
                      str(nm2["iconOpacity"]))
                check("深色主题下那一小块仍有描边", nm2["iconShadow"] not in (None, "none"),
                      str(nm2["iconShadow"])[:46])
                ct2 = contrast(rgb_of(nm2.get("fg")), rgb_of(nm2.get("bg")))
                check("深色主题下名字不是白底白字", ct2 is not None and ct2 >= 4.5,
                      "%s on %s = %s" % (nm2.get("fg"), nm2.get("bg"),
                                         ("%.1f" % ct2) if ct2 else "算不出"))
            b.shoot(os.path.join(SHOTS, "%s_dark.png" % tag))
            # 主题是要复原的：验收轮里每个脚本共用同一个 Chrome 配置目录，
            # localStorage 跟着跨页签留到下一个脚本，后面那些检查看到的就是深色主题。
            b.js("""(() => {
              const btn = document.querySelector('[aria-label=\"切到浅色\"]');
              if (btn) btn.click();
            })()""")
            b.wait("document.documentElement.getAttribute('data-theme') !== 'dark'", tries=20, pause=0.2)

            # ---- 这一屏有没有溢出 / 有没有异常 ----
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
