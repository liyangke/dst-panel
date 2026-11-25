#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""#72 的浏览器实测：下载页与配置页也改成带封面的卡片之后，图、角标、开关、抽屉是不是真的。

用户那条要求是"既然搜索页是带图片的卡片，下载和配置两处也应该是，以保证一致"。
"一致"不能靠看截图点头，所以这里逐条量：

1. 两个屏的每一张卡都真的有一个封面盒（不是只有 CSS 类名）。
2. 在线检索见过的模组，浏览器真的把那张图加载出来了（naturalWidth>0）。
   接口给了 previewUrl 但图挂了 ≠ 有图，所以认的是解码后的尺寸。
3. 同一个模组在两个屏上用的是同一张图（比 src，不是比"都有图"）。
4. 没见过一次的模组退回色块——那是正常状态，不是坏图，所以色块上必须有字。
5. 封面角标：下载页只会是 在清单/不在清单/本地；配置页只会是 已开/有条目/没开。
   本地上传的模组绝不被标成"不在清单"（它本来就没有工坊 ID，那是编出来的问题）。
6. 每张卡上的按钮只在这张卡真能点的时候出现：没有"移除登记"按钮的卡必须真的没登记，
   有"工坊页"链接的卡必须真的带 workshopId。
7. 配置页的开关按下去真的写盘（用接口回读 enabled），抽屉展开后占满整行宽度。
8. 四个筛选按钮 + 搜索框各点一次，界面剩下的行数与接口口径一致。
9. modinfo.lua 读不出时那句提示必须是中文、且不带安装目录的绝对路径。
10. 「modinfo 读不懂」只许挂在文件真读不懂的那几行：名字被作者写成算出来的表达式（文件读得通、
    只是面板不执行 Lua）那种要挂「模组没写出名字」，并讲清这一行按目录名显示；
    已登记还没下载的那一排更不许说模组坏了。
11. 用户点名删掉的那两句"这条信息从哪来"的解说（封面来源、上传场景）不得再出现在屏上，
    上传场景那句里还夹着裸英文的服务端分支名。

种数据的方式：从面板自己的工坊缓存里挑"这次真带封面"的 ID，在夹具 mods/ 下造目录。
这样封面不是探针编的，是面板在线检索真的见过的那张。

用法：先起着 python .tools/ui_dev.py，再 python .tools/mods_cards_check.py
"""
import base64
import json
import os
import sqlite3
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.stdout.reconfigure(encoding="utf-8", errors="replace")
sys.stderr.reconfigure(encoding="utf-8", errors="replace")
from cdp import Browser  # noqa: E402

ROOT = os.path.dirname(HERE)
BASE = "http://127.0.0.1:8963"
DB = os.path.join(ROOT, ".tmp_boot", "uiwork", "dstpanel.db")
MODS = os.path.join(ROOT, ".tmp_boot", "dst", "mods")
OUT = os.path.join(HERE, "tmp", "mods72")
DESKTOP = (1440, 900)
MOBILE = (390, 844)
# 探针自己造的那一位（名字是表达式）用这个固定 ID，好在屏上一眼定位。
EXPR_DIR = "workshop-900000011"
# 房间名和世界名都不在这里硬写：夹具里同时躺着 Cluster_1 和 ProbeWorld 两个房间，
# 页面选中哪个由 store 决定，探针从 DOM 上读（见 picked_cluster / picked_shard）。

AUTH = "Basic " + base64.b64encode(b"admin:admin").decode()
fails = []
misses = []


def check(name, ok, detail=""):
    print("  %s  %s%s" % ("ok  " if ok else "FAIL", name, ("  ｜" + detail) if detail else ""))
    if not ok:
        fails.append(name + ("（" + detail + "）" if detail else ""))


def say(t):
    print("  ·  " + t)


def call(method, path, body=None):
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=data, method=method,
                                 headers={"Authorization": AUTH, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return json.loads(r.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return {"ok": False, "error": "HTTP %d %s" % (e.code, e.read().decode("utf-8", "replace")[:200])}


def data_of(r):
    return (r or {}).get("data") or {}


def cached_with_cover(limit=3):
    """从面板自己的缓存里挑真带封面的 ID。

    只认 preview_url 非空的那几条：色块兜底那条路夹具里已经有 workshop-900000003 覆盖了，
    这里要补的是"有图"那一路。缓存里没有带封面的条目时先打一次在线检索（见过即入库）。
    """
    con = sqlite3.connect(DB)
    con.row_factory = sqlite3.Row

    def pick():
        cur = con.execute(
            "SELECT workshop_id, name, preview_url, subscriptions, favorited "
            "FROM mods_seen WHERE preview_url <> '' ORDER BY subscriptions DESC LIMIT ?", (limit,))
        return [dict(x) for x in cur.fetchall()]

    rows = pick()
    if not rows:
        con.close()
        call("GET", "/api/v1/mods/search?q=stack&size=12")
        con = sqlite3.connect(DB)
        con.row_factory = sqlite3.Row
        rows = pick()
    con.close()
    return rows


def write_modinfo(dirpath, body):
    d = os.path.join(MODS, dirpath)
    os.makedirs(d, exist_ok=True)
    p = os.path.join(d, "modinfo.lua")
    if os.path.exists(p):
        os.remove(p)
    if body is not None:
        with open(p, "w", encoding="utf-8", newline="\n") as f:
            f.write(body)
    return d


def seed():
    """造四种真实状态：A 有图+在清单、B 有图+没 modinfo、C（夹具自带）在缓存但没封面、
    D 文件读得通但 name 是算出来的表达式（多语言模组的写法，面板不执行 Lua）。

    D 为什么探针自己造而不靠 ui_dev 的样本：这一条要验的是"读得通 ≠ 读不懂"这两种说法
    在屏上各归各位，样本哪天被改名或删掉，检查会静默失去覆盖——探针自己写盘才一直验得到。
    """
    write_modinfo(EXPR_DIR,
                  'name = ChooseTranslationTable({ en = "Expr Name Mod", zh = "表达式名字模组" })\n'
                  'version = "1.0.7"\napi_version = 10\nauthor = "夹具作者"\n'
                  'description = [[\n名字是算出来的，面板只解析不执行。\n]]\nsettings = {}\n')
    rows = cached_with_cover()
    if len(rows) < 2:
        misses.append("工坊缓存里没有两条带封面的条目，在线检索那一路这一轮没走通；"
                      "卡片退回色块是允许的，但'有图'这一条就没验到")
        return [], []
    a, b = rows[0], rows[1]
    # A：磁盘上读得出名字，所以卡上的名字必须是磁盘那份（工坊标题不能盖掉它）。
    write_modinfo("workshop-" + a["workshop_id"],
                  'name = "磁盘名字优先验证"\nversion = "9.9"\napi_version = 10\n')
    # B：故意不放 modinfo.lua——工坊下载中途被手工删过就是这么个样子。
    write_modinfo("workshop-" + b["workshop_id"], None)
    # 只登记 A：这样"在清单"和"不在清单"两种角标在同一屏里都有。
    r = call("POST", "/api/v1/mods/setup", {"workshopId": a["workshop_id"]})
    if not r.get("ok"):
        say("登记 A 没成（可能本来就在清单里）：%s" % r.get("error"))
    return [a, b], []


def CARDS():
    return r"""(() => {
  const vis = (e) => !!e && e.offsetParent !== null;
  const lines = [...document.querySelectorAll('.mod-line')].map((c) => {
    const cover = c.querySelector('.mod-cover');
    const img = c.querySelector('.mod-cover-img');
    const ph = c.querySelector('.mod-cover-ph');
    const badge = c.querySelector('.mod-cover-state .badge');
    const cr = cover ? cover.getBoundingClientRect() : null;
    const drawer = c.querySelector('.drawer');
    const sw = c.querySelector('input[type=checkbox]');
    const name = c.querySelector('.mod-line-name');
    return {
      name: (name ? name.textContent : '').trim().slice(0, 26),
      text: c.innerText.replace(/\s+/g, ' ').trim().slice(0, 700),
      hasCover: !!cover,
      box: cr ? Math.round(cr.width) + 'x' + Math.round(cr.height) : 'no-box',
      src: img ? img.getAttribute('src') : '',
      nat: img && img.complete ? img.naturalWidth + 'x' + img.naturalHeight : 'no-img',
      fit: img ? getComputedStyle(img).objectFit : '',
      ph: ph ? ph.textContent.trim() : '',
      badge: badge ? badge.textContent.trim() : '',
      badgeTitle: badge ? (badge.getAttribute('title') || '') : '',
      chips: [...c.querySelectorAll('.mod-meta .chip')].map((x) => x.textContent.trim()),
      rate: [...c.querySelectorAll('.mod-rate span')].map((s) =>
        s.textContent.trim() + '⟨' + (s.getAttribute('title') || '') + '⟩'),
      switch: !!sw,
      on: sw ? sw.checked : null,
      btns: [...c.querySelectorAll('button')].map((x) => x.textContent.trim().replace(/\s+/g, '')),
      links: [...c.querySelectorAll('a')].map((x) => (x.getAttribute('href') || '') + '|' + x.textContent.trim()),
      drawerW: drawer && vis(drawer) ? Math.round(drawer.getBoundingClientRect().width) : 0,
      drawerL: drawer && vis(drawer) ? Math.round(drawer.getBoundingClientRect().left) : 0,
      coverL: cover ? Math.round(cover.getBoundingClientRect().left) : 0,
      cardW: Math.round(c.getBoundingClientRect().width),
      coverShown: vis(cover),
    };
  });
  return {
    n: lines.length,
    lines: lines,
    hints: [...document.querySelectorAll('.hint')].map((h) => h.textContent.trim()),
    // 「已登记、还没下载」那一栏：用户说的"放个编号谁看得懂"就是这一排。
    pend: [...document.querySelectorAll('.row.pend')].map((x) =>
      x.innerText.replace(/\s+/g, ' ').trim()),
    segs: [...document.querySelectorAll('.seg')].map((g) =>
      [...g.querySelectorAll('button')].map((b) =>
        b.textContent.trim().replace(/\s+/g, ' ') + (b.classList.contains('on') ? '[on]' : ''))),
    title: ((document.querySelector('.card-head h3') || {}).textContent || '').trim(),
    heads: [...document.querySelectorAll('.card-head h3')].map((h) => h.textContent.trim()),
    empty: vis(document.querySelector('.empty')),
    sk: vis(document.querySelector('.sk')),
    err: [...document.querySelectorAll('.err-box')].map((x) => x.textContent.trim()),
    errs: (window.__errs || []).slice(0, 8),
    overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
  };
})()"""


def settle(b, tries=90, pause=0.5):
    last = None
    for _ in range(tries):
        last = b.js(CARDS())
        if last and not last.get("sk"):
            return last, True
        time.sleep(pause)
    return last, False


def tab(b, label):
    b.js("""(() => { const t = [...document.querySelectorAll('.tabs button')]
        .find((x) => x.textContent.indexOf(%s) >= 0); if (t) t.click(); return t ? 1 : 0; })()"""
         % json.dumps(label))
    time.sleep(0.8)


def by_name(d, key):
    for x in d.get("lines") or []:
        if key in x["name"] or key in x["text"]:
            return x
    return None


def by_dir(d, dirpath):
    """按目录名那一枚 chip 定位某一行：名字会退回目录名，用 name 找会撞车。"""
    for x in d.get("lines") or []:
        if any(dirpath == c for c in x["chips"]):
            return x
    return None


def picked_shard(b):
    return b.js("""(() => { const g = [...document.querySelectorAll('.seg')]
        .find((x) => /Master|Caves/.test(x.textContent));
      const on = g && [...g.querySelectorAll('button')].find((y) => y.classList.contains('on'));
      return on ? on.textContent.trim() : ''; })()""") or ""


def picked_cluster(b):
    """界面上当前选中的房间名。

    这里绝不能再硬写一个房间名：夹具里同时存在 Cluster_1 和 ProbeWorld 两个房间，
    页面选中的是哪一个由 store 决定。上一版探针就是拿硬写的 ProbeWorld 去回读接口，
    而界面画的是 Cluster_1 的覆盖文件（3 项已开），于是把对的页面报成了 FAIL。
    """
    return b.js("""(() => {
      const lab = [...document.querySelectorAll('label')].find((l) => l.textContent.trim() === '当前房间');
      const sel = lab && lab.parentElement.querySelector('select');
      return sel ? sel.value : '';
    })()""") or ""


def login(b):
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
    if not b.wait("!!document.querySelector('.tabs button')", tries=90, pause=0.4):
        b.shoot(os.path.join(OUT, "boot_failed.png"))
        raise SystemExit("模组页没起来：%s" % (b.js("document.body.innerText.slice(0,300)") or ""))
    b.js("window.__errs = [];addEventListener('error',(e)=>window.__errs.push(String(e.message||e.error)));"
         "addEventListener('unhandledrejection',(e)=>window.__errs.push('promise:'+String(e.reason)));")


def main():
    os.makedirs(OUT, exist_ok=True)
    seeds, _ = seed()
    id_a = seeds[0]["workshop_id"] if seeds else ""
    id_b = seeds[1]["workshop_id"] if len(seeds) > 1 else ""

    b = Browser(*DESKTOP)
    login(b)
    cluster = shard = ""

    # ---- 1. 下载页（第 2 步）----
    tab(b, "装到这台机器")
    d, ok = settle(b)
    say("标题：%s ｜卡片 %d 张" % (d.get("title"), d.get("n")))
    check("下载页每张卡都有封面盒", ok and d["n"] > 0 and all(x["hasCover"] for x in d["lines"]),
          "%d 张里缺 %d" % (d["n"], len([x for x in d["lines"] if not x["hasCover"]])))
    check("下载页没有 JS 报错（模板能编译）", ok and not d["errs"],
          json.dumps(d["errs"], ensure_ascii=False))
    imgs = [x for x in d["lines"] if x["nat"] != "no-img"]
    loaded = [x for x in imgs if x["nat"].split("x")[0] not in ("0", "")]
    say("有 img 元素 %d 张，其中真解码出尺寸的 %d 张" % (len(imgs), len(loaded)))
    check("在线检索见过的模组这次真把封面加载出来了", ok and len(loaded) > 0,
          json.dumps([x["nat"] for x in d["lines"][:6]]))
    check("封面完整显示不裁图（object-fit: contain）",
          ok and all(x["fit"] == "contain" for x in imgs),
          json.dumps([x["fit"] for x in imgs]))
    card_a = by_name(d, "磁盘名字优先验证")
    card_b = None
    for x in d["lines"]:
        if id_b and ("workshop-" + id_b) in " ".join(x["chips"]):
            card_b = x
    check("A 卡上的名字用的是磁盘 modinfo 而不是缓存标题",
          card_a is not None and "磁盘名字优先验证" in card_a["name"],
          (card_a or {}).get("name", "没找到 A 卡"))
    check("A 卡有封面图", card_a is not None and card_a["nat"] != "no-img",
          (card_a or {}).get("nat", ""))
    check("A 卡在清单 → 角标「在清单」", card_a is not None and card_a["badge"] == "在清单",
          (card_a or {}).get("badge", ""))
    check("B 卡（没 modinfo.lua）也有封面，名字退回缓存标题",
          card_b is not None and card_b["nat"] != "no-img" and "磁盘名字" not in card_b["name"],
          json.dumps({k: card_b[k] for k in ("name", "nat", "badge")}, ensure_ascii=False) if card_b else "没找到 B 卡")
    check("B 卡读不懂 modinfo 的那句是中文、不带绝对路径",
          card_b is not None and "没有可读的 modinfo.lua" in card_b["text"]
          and "The system cannot find" not in card_b["text"] and ":\\" not in card_b["text"],
          (card_b or {}).get("text", "")[:120])
    tile = [x for x in d["lines"] if x["nat"] == "no-img"]
    check("没封面的卡退回色块且色块上有字", all(x["ph"] for x in tile),
          json.dumps([x["ph"] for x in tile], ensure_ascii=False))
    local = [x for x in d["lines"] if "本地上传" in " ".join(x["chips"])]
    say("本地上传的卡：%d 张" % len(local))
    check("本地模组的角标是「本地」，不会被标成「不在清单」",
          all(x["badge"] == "本地" for x in local) and len(local) > 0,
          json.dumps([x["badge"] for x in local], ensure_ascii=False))
    check("本地模组卡上没有「移除登记」按钮（按下去没用的按钮不许存在）",
          all("移除登记" not in x["text"] for x in local),
          (local[0] or {}).get("text", "")[:100] if local else "")
    # 只有真的带工坊 ID 的卡才许有"工坊页"链接：给本地模组挂一个 ?id=undefined 的链接，
    # 点下去是工坊的一张错误页。
    bad_links = [x["name"] for x in d["lines"]
                 if x["links"] and not any(c.startswith("工坊") for c in x["chips"])]
    check("「工坊页」链接只出现在带 workshopId 的卡上", not bad_links,
          json.dumps(bad_links, ensure_ascii=False)[:150])
    # ---- #79：那两句"这条信息从哪来"的解说是用户点名不要的，屏上不许再留 ----
    left = [h for h in d["hints"] if "缓存" in h or "没见过一次" in h]
    check("封面/订阅数的来源解说已经不在屏上（用户点名删的那句）", not left,
          json.dumps(left, ensure_ascii=False)[:180])
    # 上传那一栏的"什么时候要用它"整句也是点名的，其中半句是裸英文，界面一律不许出现。
    check("屏上没有任何裸英文的服务端分支名",
          not any("Steam Workshop functionality" in h for h in d["hints"]),
          json.dumps(d["hints"], ensure_ascii=False)[:180])

    # ---- #80：「读不懂」这一句只许挂在真的读不懂的文件上 ----
    ex = by_dir(d, EXPR_DIR)
    check("名字写成表达式的那一位在屏上找得到（探针自己造的夹具）", ex is not None, EXPR_DIR)
    if ex:
        say("表达式那一行的原文：" + ex["text"][:200])
        check("读得通、只是名字算不出来的那一位不挂「modinfo 读不懂」",
              "modinfo 读不懂" not in ex["text"], ex["text"][:220])
        check("那一位挂的是「模组没写出名字」",
              "模组没写出名字" in ex["text"], ex["text"][:220])
        check("那一句把屏幕上顶上来的是目录名说清楚了",
              "这一行按目录名显示" in ex["text"], ex["text"][:220])
        check("那一位不编名字，位置留给目录名", ex["name"] == EXPR_DIR, ex["name"])
    # 角标和说明必须同真同假：挂了「modinfo 读不懂」的每一行，都要同时写着"没有可读的文件"。
    wrong = [x["name"] for x in d["lines"]
             if "modinfo 读不懂" in x["text"] and "没有可读的 modinfo.lua" not in x["text"]]
    check("「modinfo 读不懂」只出现在真的没有可读文件的那几行", not wrong,
          json.dumps(wrong, ensure_ascii=False)[:160])
    check("已登记还没下载的那一排不许说模组坏了",
          not any("读不懂" in p for p in d["pend"]),
          json.dumps(d["pend"], ensure_ascii=False)[:160])
    check("下载页没有横向溢出", ok and d["overflow"] <= 1, str(d["overflow"]))
    b.shoot(os.path.join(OUT, "01_disk_light.png"))

    # ---- 2. 配置页（第 3 步）：同一个模组的同一张图 ----
    tab(b, "给这个房间开")
    d2, ok2 = settle(b)
    say("标题：%s ｜卡片 %d 张" % (d2.get("title"), d2["n"]))
    check("配置页每张卡都有封面盒", ok2 and d2["n"] > 0 and all(x["hasCover"] for x in d2["lines"]),
          "%d 张里缺 %d" % (d2["n"], len([x for x in d2["lines"] if not x["hasCover"]])))
    check("配置页没有 JS 报错", ok2 and not d2["errs"], json.dumps(d2["errs"], ensure_ascii=False))
    a2 = None
    for x in d2["lines"]:
        if id_a and ("workshop-" + id_a) in " ".join(x["chips"]):
            a2 = x
    check("同一个模组在两个屏上用的是同一张封面（一致性的那条）",
          card_a is not None and a2 is not None and card_a["src"] == a2["src"] and a2["nat"] != "no-img",
          json.dumps([ (card_a or {}).get("src", "")[-40:], (a2 or {}).get("src", "")[-40:] ], ensure_ascii=False))
    badges2 = [x["badge"] for x in d2["lines"]]
    check("配置页角标只会是 已开/有条目/没开",
          all(x in ("已开", "有条目", "没开") for x in badges2) and len(badges2) > 0,
          json.dumps(badges2, ensure_ascii=False))
    check("配置页每行都有开关", ok2 and all(x["switch"] for x in d2["lines"]),
          "%d 行里 %d 行有" % (d2["n"], len([x for x in d2["lines"] if x["switch"]])))
    check("配置页也没有那两句封面来源解说",
          not any("缓存" in h or "Steam Workshop functionality" in h for h in d2["hints"]),
          json.dumps(d2["hints"], ensure_ascii=False)[:160])
    # 同一个模组在两个屏上的说法必须一致：配置页不许把它讲成"读不懂"而下载页讲"没写出名字"。
    ex2 = by_dir(d2, EXPR_DIR)
    check("表达式名字那一位在配置页也不挂「modinfo 读不懂」",
          ex2 is None or "modinfo 读不懂" not in ex2["text"],
          (ex2 or {}).get("text", "")[:200])

    # 点 A 那一行的开关：界面角标要跟着变，而且接口那边真的写进去了。
    if a2 is not None:
        cluster = picked_cluster(b)
        shard = picked_shard(b)
        say("界面上当前选中的是房间 %r / 世界 %r，回读就按这两个走" % (cluster, shard))
        say("点了 A 行的开关：" + b.js("""(() => {
          const c = [...document.querySelectorAll('.mod-line')].find((x) => x.innerText.indexOf(%s) >= 0);
          if (!c) return '没有 A 行';
          const sw = c.querySelector('input[type=checkbox]');
          if (!sw) return '没有开关';
          sw.click();
          return '已点';
        })()""" % json.dumps("workshop-" + id_a)))
        time.sleep(1.2)
        d3, _ = settle(b)
        a3 = None
        for x in d3["lines"]:
            if ("workshop-" + id_a) in " ".join(x["chips"]):
                a3 = x
        check("开关按下去这一行的角标变成「已开」", a3 is not None and a3["badge"] == "已开",
              (a3 or {}).get("badge", "没找到"))
        back = data_of(call("GET", "/api/v1/clusters/%s/shards/%s/mods" % (
            urllib.parse.quote(cluster, safe=""), urllib.parse.quote(shard, safe=""))))
        on = [m for m in (back.get("mods") or []) if m["dir"] == "workshop-" + id_a]
        check("接口回读确认 modoverrides 里真的开了", bool(on) and bool(on[0].get("enabled")),
              json.dumps(on, ensure_ascii=False)[:160])
        # 收回这一次改动：夹具的默认状态是都没开，留着会污染后面的用例。
        b.js("""(() => {
          const c = [...document.querySelectorAll('.mod-line')].find((x) => x.innerText.indexOf(%s) >= 0);
          const sw = c && c.querySelector('input[type=checkbox]'); if (sw) sw.click(); return sw ? 1 : 0;
        })()""" % json.dumps("workshop-" + id_a))
        time.sleep(1.2)
        d4, _ = settle(b)
        check("再点一次能关回去（开关不是单向的）",
              any(("workshop-" + id_a) in " ".join(x["chips"]) and x["badge"] != "已开" for x in d4["lines"]),
              json.dumps([x["badge"] for x in d4["lines"]], ensure_ascii=False))

        # 配置抽屉：展开后必须占满整行，不能被挤在第二列里。
        b.js("""(() => {
          const c = [...document.querySelectorAll('.mod-line')].find((x) => x.innerText.indexOf(%s) >= 0);
          const btn = c && [...c.querySelectorAll('button')].find((y) => y.textContent.indexOf('配置') >= 0);
          if (btn) btn.click(); return btn ? 1 : 0;
        })()""" % json.dumps("workshop-" + id_a))
        time.sleep(1.5)
        d5, _ = settle(b)
        dr = None
        for x in d5["lines"]:
            if ("workshop-" + id_a) in " ".join(x["chips"]) and x["drawerW"]:
                dr = x
        # 抽屉必须是横跨整行：判据用它的左沿——整行铺开时它和封面同一条竖线上，
        # 被塞进第二列的话它会从封面右边开始（差一个封面宽）。宽度本身分不清这两种，
        # 因为第二列也有 90% 的行宽。
        check("抽屉展开且横跨整行（左沿和封面对齐，没被挤进第二列）",
              dr is not None and dr["drawerL"] <= dr["coverL"] + 2 and
              dr["drawerW"] >= dr["cardW"] - 28,
              json.dumps({"抽屉左": dr.get("drawerL") if dr else 0, "封面左": dr.get("coverL") if dr else 0,
                          "抽屉宽": dr.get("drawerW") if dr else 0, "卡宽": dr.get("cardW") if dr else 0}))
        check("抽屉里没有报错", d5 is not None and not d5["errs"], json.dumps(d5["errs"], ensure_ascii=False))
        b.shoot(os.path.join(OUT, "02_shard_drawer.png"))
        b.js("""(() => { const btn = [...document.querySelectorAll('.mod-line button')]
            .find((y) => y.textContent.indexOf('收起配置') >= 0); if (btn) btn.click(); return btn ? 1 : 0; })()""")
        time.sleep(0.8)
    else:
        misses.append("配置页没找到 A 那一行（在线检索见过的那条），开关/抽屉/角标那几条这一轮没验到")

    # ---- 3. 筛选控件逐个点 ----
    if not cluster:
        cluster, shard = picked_cluster(b), picked_shard(b)
    counts = {}
    for label in ["全部", "已开", "没开", "有问题"]:
        b.js("""(() => { const g = [...document.querySelectorAll('.seg')]
            .find((x) => /全部/.test(x.textContent));
            const t = g && [...g.querySelectorAll('button')].find((y) => y.textContent.indexOf(%s) === 0);
            if (t) t.click(); return t ? 1 : 0; })()""" % json.dumps(label))
        time.sleep(0.7)
        dd, _ = settle(b)
        counts[label] = dd["n"]
        on = [s for s in (dd["segs"] or []) if any(x.startswith(label) and "[on]" in x for x in s)]
        say("筛「%s」→ %d 行，按钮选中态=%s" % (label, dd["n"], bool(on)))
        check("筛「%s」后按钮进入选中态" % label, bool(on), json.dumps(dd["segs"], ensure_ascii=False)[:150])
    base_all = call("GET", "/api/v1/clusters/%s/shards/%s/mods" % (
        urllib.parse.quote(cluster, safe=""), urllib.parse.quote(shard, safe="")))
    total = len((data_of(base_all).get("mods") or []))
    check("「全部」的行数 = 覆盖文件条目 ∪ 磁盘目录（不比接口少）",
          counts.get("全部", 0) >= max(total, 1) and counts.get("全部", 0) > 0,
          "界面 %s ｜接口 mods %d" % (counts.get("全部"), total))
    check("「已开」比「全部」少或相等，且和顶部计数同口径",
          counts.get("已开", 0) <= counts.get("全部", 0),
          json.dumps(counts, ensure_ascii=False))
    # 「有问题」这一筛必须每一行都带着那三种原因之一，否则它就是随机挑了几行。
    b.js("""(() => { const g = [...document.querySelectorAll('.seg')]
        .find((x) => /全部/.test(x.textContent));
        const t = g && [...g.querySelectorAll('button')].find((y) => y.textContent.indexOf('有问题') === 0);
        if (t) t.click(); })()""")
    time.sleep(0.7)
    dp, _ = settle(b)
    reasons = ("依赖", "modinfo 读不懂", "文件还没下载")
    bad_rows = [x["name"] for x in dp["lines"] if not any(r in x["text"] for r in reasons)]
    check("「有问题」筛出来的每一行都带着那三种原因之一",
          dp["n"] > 0 and not bad_rows,
          "%d 行 ｜不像有原因的：%s" % (dp["n"], json.dumps(bad_rows, ensure_ascii=False)[:160]))
    b.js("""(() => { const g = [...document.querySelectorAll('.seg')]
        .find((x) => /全部/.test(x.textContent));
        const t = g && [...g.querySelectorAll('button')].find((y) => y.textContent.indexOf('全部') === 0);
        if (t) t.click(); })()""")
    time.sleep(0.6)
    # 搜索框：打一个磁盘目录名，只剩那一行。
    b.js("""(() => { const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
        const i = document.querySelector('input[type=search]'); if (i) set(i, %s); })()"""
         % json.dumps("workshop-" + (id_a or "900000001")))
    time.sleep(0.9)
    d6, _ = settle(b)
    check("搜目录名只剩对得上的那一行", d6["n"] >= 1 and
          all(("workshop-" + (id_a or "900000001")) in " ".join(x["chips"]) for x in d6["lines"]),
          "%d 行 ｜%s" % (d6["n"], json.dumps([x["name"] for x in d6["lines"]], ensure_ascii=False)[:120]))
    b.js("""(() => { const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
        const i = document.querySelector('input[type=search]'); if (i) set(i, '绝不存在的东西zzz'); })()""")
    time.sleep(0.9)
    d7, _ = settle(b)
    check("搜不存在的词给的是空状态而不是一屏空白", d7["n"] == 0 and d7["empty"],
          "行 %d ｜empty=%s" % (d7["n"], d7["empty"]))
    b.js("""(() => { const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
        const i = document.querySelector('input[type=search]'); if (i) set(i, ''); })()""")
    time.sleep(0.8)

    # ---- 4. 深色主题 + 窄屏 ----
    # 切主题只能点那颗图标按钮（aria-label 写的就是"切到深色/切到浅色"），
    # 别去改 localStorage——那验的不是用户那条路。
    # 点一次只保证"换一个"，不保证换到深色：上一轮的 theme 存在 localStorage 里，
    # 所以这里按当前值决定点不点，两张截图才各自对得上自己的文件名。
    def to_theme(want):
        for _ in range(2):
            cur = b.js("document.documentElement.getAttribute('data-theme')")
            if cur == want:
                return True
            b.js("""(() => { const t = [...document.querySelectorAll('.icon-btn')].find((x) =>
                /切到深色|切到浅色/.test(x.getAttribute('aria-label') || '')); if (t) t.click(); return t ? 1 : 0; })()""")
            time.sleep(0.7)
        return b.js("document.documentElement.getAttribute('data-theme')") == want

    check("能切到深色主题", to_theme("dark"), str(b.js("document.documentElement.getAttribute('data-theme')")))
    b.shoot(os.path.join(OUT, "03_dark.png"))
    check("深色主题下没有 JS 报错", not (b.js("window.__errs.slice(0,4)") or []),
          json.dumps(b.js("window.__errs.slice(0,4)") or [], ensure_ascii=False))
    to_theme("light")
    b.shoot(os.path.join(OUT, "03b_light.png"))

    b.set_viewport(*MOBILE)
    time.sleep(0.8)
    dm, _ = settle(b)
    check("窄屏下卡片没有横向溢出", dm["overflow"] <= 1, str(dm["overflow"]))
    check("窄屏下封面仍在（没被 display:none 掉）",
          dm["n"] > 0 and all(x["coverShown"] for x in dm["lines"]),
          "%d 行里没显示封面 %d" % (dm["n"], len([x for x in dm["lines"] if not x["coverShown"]])))
    cover_w = [x["box"] for x in dm["lines"]]
    say("窄屏封面盒尺寸：%s" % json.dumps(cover_w[:6]))
    b.shoot(os.path.join(OUT, "04_mobile_shard.png"))
    tab(b, "装到这台机器")
    time.sleep(0.8)
    dd2, _ = settle(b)
    check("窄屏下载页同样有封面卡", dd2["n"] > 0 and all(x["coverShown"] for x in dd2["lines"]),
          "%d 行" % dd2["n"])
    check("窄屏下载页没有 JS 报错", not dd2["errs"], json.dumps(dd2["errs"], ensure_ascii=False))
    b.shoot(os.path.join(OUT, "05_mobile_disk.png"))

    print("\n--- 结果 ---")
    print("FAIL %d 条" % len(fails))
    for f in fails:
        print("  ✗ " + f)
    for m in misses:
        print("  ·  没验到：" + m)
    b.close()
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
