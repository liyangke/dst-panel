#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""公网房间页：把每一个筛选控件真点一遍，并把界面上的数字和接口真正回的数字对一遍。

为什么要这么一遍：这一页有六个筛选条件、两套布局（表格 + 手机卡片）、三种空态
（出不去公网 / 列表关着 / 条件叠死了），而它们全是"点下去才知道对不对"的东西。
Go 单测能证明接口自洽，证明不了页面：条件写错、分页钳位没跟上、
清掉全部连发六个请求、按下去什么都不发生的那颗按钮，只有在浏览器里才露出来。

每一条操作都做同一件事——点控件，然后同时读「界面」和「接口对同一组条件的回答」，
比这三个数：筛出多少间、这一屏实际渲染了几行、底部那句"第 X-Y 间 / 共 N 间"。
界面说 21 间而下面只排了 20 行，也是 bug。

它只打本机 127.0.0.1:8963 那个面板，读的是 Klei 公开快照：不写业务文件、不改配置、不碰云机。
「取最新」会真去下载一次整张大区快照（解压后十几 MB），一轮只点一次。
"""
import base64
import json
import os
import re
import sys
import time
import urllib.parse
import urllib.request

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from cdp import Browser  # noqa: E402

BASE = "http://127.0.0.1:8963"
AUTH = "Basic " + base64.b64encode(b"admin:admin").decode()
SHOTS = os.path.join(HERE, "tmp", "lobby")
DESKTOP = (1440, 900)
MOBILE = (390, 844)
DEFAULT = {"region": "", "q": "", "intent": "", "access": "", "slots": "",
           "players": "", "sort": "players", "page": 1, "size": 20}
# sort 这一项存的是"界面同步之后下一轮真正发出去的那枚排序"，不是"用户在框里看到了什么字"。
# 后端没带 sort 时也是这个值（见 lobbyQuery 那个默认），下面第 5 段专门比这两处对不对得上：
# 三份默认值（后端 / lobby.js 跟着接口填的那枚下拉 / 这份脚本）只要有一份改口径，那条就会红。
# 三枚分段控件的"中文标签 → 接口参数值"。这一份在 lobby.js 里也写着一遍，
# 两边对不上的话 expect() 会当场报出来（界面一个数、接口另一个数），不会静默放过。
SEG_VALUE = {
    ("access", "门槛全开"): "open", ("access", "有门槛"): "locked",
    ("slots", "还有空位"): "free", ("slots", "已经满了"): "full",
    ("players", "有人在里面"): "has", ("players", "一个人都没有"): "none",
}

fails = []


def check(name, ok, detail=""):
    print("  %s  %s%s" % ("ok  " if ok else "FAIL", name, ("  ｜" + detail) if detail else ""))
    if not ok:
        fails.append(name + ("（" + detail + "）" if detail else ""))
    return ok


def say(text):
    print("  ·  " + text)


def api_get(path, params=None, timeout=200):
    """打接口并剥掉外壳：面板每个回答都包成 {ok, data}，这里要的是 data 那一份。"""
    q = {k: v for k, v in (params or {}).items() if v not in ("", None)}
    url = BASE + path + ("?" + urllib.parse.urlencode(q) if q else "")
    req = urllib.request.Request(url, headers={"Authorization": AUTH})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        body = json.loads(r.read().decode("utf-8"))
    if not body.get("ok"):
        raise SystemExit("接口 %s 回了失败：%s" % (path, json.dumps(body, ensure_ascii=False)[:300]))
    return body["data"]


# 只读界面上真正说出口的那些数字，不读 Vue 内部状态：组件里的 reply 再对，
# 渲染出来的句子写错了也还是错的。offsetParent 用来分清"在 DOM 里"和"看得见"——
# 两套布局同时存在于文档里，手机上必须看得见的是卡片那一套。
STATE_JS = r"""(() => {
  const p = document.querySelector('.lobby-page');
  if (!p) return null;
  const vis = (el) => !!(el && el.offsetParent !== null);
  const statV = (key) => {
    const s = [...p.querySelectorAll('.stat')].find((x) => {
      const k = x.querySelector('.stat-k span');
      return k && k.textContent.trim().indexOf(key) >= 0;
    });
    return s ? s.querySelector('.stat-v').textContent.trim() : null;
  };
  // 只认看得见的那个元素：手机/桌面两套布局同时挂在文档里，
  // 拿隐藏那一套的文本去比接口，量的就不是用户读到的东西了。
  const noteOf = (re) => {
    const e = [...p.querySelectorAll('span')].find((x) => vis(x) && re.test(x.textContent.trim()));
    return e ? e.textContent.trim() : '';
  };
  const rows = [...p.querySelectorAll('table.data tbody tr')];
  const cards = [...p.querySelectorAll('.room-card')];
  const btn = (sel, re) => {
    const t = [...p.querySelectorAll(sel)].find((x) => re.test(x.textContent));
    return t ? {text: t.textContent.trim(), disabled: !!t.disabled} : null;
  };
  return {
    rowCount: rows.length,
    tableShown: vis(p.querySelector('.table-wrap')),
    cardCount: cards.length,
    cardsShown: vis(p.querySelector('.only-sm')),
    filtered: statV('当前条件筛出'),
    joinableStat: statV('这一屏可以直接进'),
    countText: noteOf(/^第 \d+-\d+ 间 \/ 共 \d+ 间$/),
    cacheNote: noteOf(/^（读的是缓存/),
    fetchedNote: noteOf(/前取到/),
    pagerTotal: (() => { const e = p.querySelector('.pager .pg-total'); return e ? e.textContent.trim() : ''; })(),
    pageNums: [...p.querySelectorAll('.pager .pg-num')].map((x) => x.textContent.trim()),
    onNum: (() => { const x = p.querySelector('.pager .pg-num.on'); return x ? x.textContent.trim() : null; })(),
    prev: btn('.pager .pg-btn', /上一页/),
    next: btn('.pager .pg-btn', /下一页/),
    filterChips: [...p.querySelectorAll('.lobby-filters button.fchip')].map((x) => x.textContent.replace(/×/g, '').trim()),
    hasClearAll: [...p.querySelectorAll('.lobby-filters button.linkish')].some((x) => x.textContent.trim() === '全部清掉'),
    errBoxes: [...p.querySelectorAll('.err-box')].map((x) => x.textContent.trim()),
    emptyTitle: (() => { const e = p.querySelector('.empty .e-title'); return e ? e.textContent.trim() : ''; })(),
    emptyButtons: [...p.querySelectorAll('.empty .btn')].map((x) => x.textContent.trim()),
    staleNote: [...p.querySelectorAll('.note.warn')].map((x) => x.textContent.trim()).join(' | '),
    joinNote: [...p.querySelectorAll('.note.info')].map((x) => x.textContent.trim()).join(' | '),
    // 复制失败时摊出来的那一句，以及按钮悬停标题里写的那一句：
    // 前者必须真的存在才谈得上"选中再 Ctrl+C"，后者是复制成功时用户唯一能核对的地方。
    copyCmd: (() => { const e = p.querySelector('.copy-cmd'); return e ? e.textContent.trim() : ''; })(),
    // 下拉框画成空框是这一页最难查的一种错：select 的当前值对一个不上的 option 时，
    // 浏览器不报错也不提示，只是把框里那行字留空，而列表照样出得来——
    // 用户看不出这一屏是按什么排的，我也就看不出后端其实没排。
    selBlank: [...p.querySelectorAll('select')].filter(vis).map((x) => ({
      aria: x.getAttribute('aria-label') || '',
      shown: x.selectedOptions.length ? x.selectedOptions[0].textContent.trim() : '',
      value: x.value,
    })).filter((x) => !x.shown),
    copyTitle: (() => { const e = p.querySelector('table.data tbody td.right button[title]');
                        return e ? (e.getAttribute('title') || '') : ''; })(),
    rowTexts: rows.map((r) => r.innerText.replace(/\s+/g, ' ')),
    firstRowTagChips: rows.length ? rows[0].querySelectorAll('.lobby-tags .chip').length : 0,
    skShown: vis(p.querySelector('.sk')),
    overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    theme: document.documentElement.getAttribute('data-theme') || 'light',
    pageErrors: (window.__errs || []).slice(0, 6),
  };
})()"""


def dom(b):
    return b.js(STATE_JS)


def settle(b, predicate, tries=70, pause=0.5):
    """轮询到界面满足条件为止：这一页每 30 秒自己重取一次，晚一拍属正常。"""
    last = None
    for _ in range(tries):
        last = dom(b)
        if last is not None and predicate(last):
            return last, True
        time.sleep(pause)
    return last, False


def rooms_of(cur):
    return api_get("/api/v1/lobby/rooms", cur)


# 把界面上那句"多久前取到"读回成秒数。
# 后端 humanAgo 会按大小换成秒/分钟/小时/天（"3 小时 5 分钟前取到"），
# 所以这里按出现的顺序把每一段加起来，而不是去匹配某一条固定文案——
# 固定文案一旦跟着后端改了口径，这条检查就会假红。
AGE_UNITS = {"秒": 1, "分钟": 60, "小时": 3600, "天": 86400}


def ago_seconds(note):
    if not note:
        return None
    if note.startswith("刚刚"):
        return 0
    parts = re.findall(r"(\d+)\s*(秒|分钟|小时|天)", note)
    if not parts:
        return None
    return sum(int(n) * AGE_UNITS[u] for n, u in parts)


SEG_LABEL = {(k, v): lab for (k, lab), v in SEG_VALUE.items()}
INTENT_LABEL = {}      # 玩法值 -> 中文标签。只从接口回的 intents 里填，不写面板字典


def expected_chips(cur):
    """这一步该让界面上摆出哪几枚条件牌，顺序照 lobby.js 里 activeFilters 的拼法。

    为什么要有这个：条件牌是界面自己承认的"我现在按什么在筛"。比数字之前先对它，
    两边不一致就是"我把条件打在了页面上但页面没认"——那属于操作没生效，
    拿去比数字只会报出一个假的页面 bug（上一轮 20 条红全是这么来的）。
    """
    out = []
    if (cur.get("q") or "").strip():
        out.append("名字或地址含「%s」" % cur["q"].strip())
    if cur.get("intent"):
        out.append("%s玩法" % INTENT_LABEL.get(cur["intent"], cur["intent"]))
    for k in ("access", "slots", "players"):
        v = cur.get(k) or ""
        if v:
            out.append(SEG_LABEL.get((k, v)) or ("?" + v))
    return out


def wait_chips(b, cur, tries=12, pause=0.25):
    want = expected_chips(cur)
    got = []
    for _ in range(tries):
        got = dom(b)["filterChips"]
        if got == want:
            return got, True
        time.sleep(pause)
    return got, False


def expect(b, cur, tag, want_rows=None):
    """点完一个条件之后的主检查：界面那三个数字必须和接口对同一组条件的回答一致。

    为什么这里要重试几轮：这一页每 30 秒自己重取一次列表，接口那份快照也有 60 秒新鲜度。
    我先读接口、再去等界面追上，中间正好换了一份快照的话，两边当然对不上——
    那不是 bug，是我拿了一张旧数字去比。所以每轮重新读一次接口再等。
    """
    r = rooms_of(cur)
    d, ok, synced = None, False, False
    got = []
    for _ in range(3):
        got, synced = wait_chips(b, cur)
        if not synced:
            break
        # 每一轮都重读一次接口：这份快照本身在换（一分钟漂十几间），
        # 拿两分钟前的总数来比今天的界面，红的会是我自己。
        r = rooms_of(cur)
        total, size, page, n = r["total"], r["size"], r["page"], len(r["rooms"])
        head = (page - 1) * size + 1
        want_count = "第 %d-%d 间 / 共 %d 间" % (head, head + n - 1, total) if n else ""

        def hit(x):
            return (x["filtered"] == "%d 间" % total
                    and x["rowCount"] == n
                    and x["countText"] == want_count
                    and not x["errBoxes"]
                    and not x["pageErrors"])

        d, ok = settle(b, hit, tries=16, pause=0.5)
        if ok:
            break
    if not synced:
        check(tag + "：条件牌先对上（界面承认了这一步的条件）", False,
              "界面 %s｜这一步该是 %s" % (got, expected_chips(cur)))
        return dom(b), r
    if not ok:
        detail = ("界面：筛出 %s｜%d 行｜%r｜接口：%d 间｜%d 行｜%r｜错 %s｜页错 %s"
                  % (d["filtered"], d["rowCount"], d["countText"], total, n, want_count,
                     d["errBoxes"], d["pageErrors"])) if d else "读不到 .lobby-page"
        check(tag + "：界面数字 == 接口数字", False, detail)
        return d, r
    check(tag + "：界面数字 == 接口数字（筛出 %d 间、这一屏 %d 行、%s）" % (total, n, want_count), True)
    if want_rows is not None:
        check(tag + "：这一屏就该是 %d 行" % want_rows, d["rowCount"] == want_rows,
              "实际 %d 行" % d["rowCount"])
    check(tag + "：分页条那句和统计卡那句同一个数",
          d["pagerTotal"] == "共 %d 间" % total,
          "分页条 %r vs 统计卡 %r" % (d["pagerTotal"], d["filtered"]))
    check(tag + "：看得见的下拉都落在有字的选项上", not d["selBlank"],
          json.dumps(d["selBlank"], ensure_ascii=False))
    return d, r


def js_arg(v):
    return json.dumps(v, ensure_ascii=False)


def pick_select(b, aria, text):
    return b.js("""(() => {
      const s = [...document.querySelectorAll('.lobby-page select')].find((x) => x.getAttribute('aria-label') === %s);
      if (!s) return 'no-select';
      const i = [...s.options].findIndex((o) => o.textContent.trim().indexOf(%s) >= 0);
      if (i < 0) return 'no-option:' + [...s.options].map((o) => o.textContent.trim()).join('/');
      s.selectedIndex = i;
      s.dispatchEvent(new Event('change', {bubbles: true}));
      return s.options[i].textContent.trim();
    })()""" % (js_arg(aria), js_arg(text)))


def click_seg(b, aria, label):
    return b.js("""(() => {
      const g = [...document.querySelectorAll('.lobby-page .seg')].find((x) => x.getAttribute('aria-label') === %s);
      if (!g) return 'no-group';
      const t = [...g.querySelectorAll('button')].find((x) => x.textContent.trim() === %s);
      if (!t) return 'no-btn:' + [...g.querySelectorAll('button')].map((x) => x.textContent.trim()).join('/');
      t.click();
      return t.textContent.trim();
    })()""" % (js_arg(aria), js_arg(label)))


def chips_now(b):
    return b.js("""(() => [...document.querySelectorAll('.lobby-page .lobby-filters button.fchip')]
                     .map((x) => x.textContent.replace(/×/g, '').trim()))()""") or []


# 那颗搜索框的定位只写这一处：三处各抄一遍时少一个引号，浏览器直接回 SyntaxError
# （上一轮就红在这里，而红的位置离真正的错处隔了两个函数）。
FIND_BOX = ("(() => [...document.querySelectorAll(%s)]"
            ".find((x) => x.getAttribute('aria-label') === '找房间'))()" % js_arg(".lobby-page input[type=search]"))


def search_value(b):
    return b.js("(() => { const i = %s; return i ? i.value : null; })()" % FIND_BOX)


def type_search(b, text):
    """填搜索框并回车。走 input + keyup 两个事件，和真人敲键盘之后 Vue 收到的完全一样。

    返回值隔了一个 tick 再读：Vue 的渲染是异步的，同一个 JS 任务里读到的还是旧 DOM——
    上一轮就是在这里同步读了条件牌，把"还没渲染"读成"页面没认这个条件"，
    于是一个假条件串在后面二十步里。
    """
    put = b.js("""(() => {
      const i = %s;
      if (!i) return null;
      i.value = %%s;
      i.dispatchEvent(new Event('input', {bubbles: true}));
      i.dispatchEvent(new KeyboardEvent('keyup', {key: 'Enter', bubbles: true}));
      return i.value;
    })()""" % FIND_BOX % js_arg(text))
    if put is None:
        return None
    time.sleep(0.3)
    return {"value": search_value(b), "chips": chips_now(b)}


def click_text(b, selector, pattern):
    return b.js("""(() => {
      const t = [...document.querySelectorAll(%s)].find((x) => %s.test(x.textContent));
      if (!t) return false;
      t.click();
      return true;
    })()""" % (js_arg(selector), ("new RegExp(%s)" % js_arg(pattern))))


def click_aria(b, selector, pattern):
    return b.js("""(() => {
      const t = [...document.querySelectorAll(%s)].find((x) => %s.test(x.getAttribute('aria-label') || ''));
      if (!t) return false;
      t.click();
      return true;
    })()""" % (js_arg(selector), ("new RegExp(%s)" % js_arg(pattern))))


def reset(cur, **kw):
    cur.clear()
    cur.update(DEFAULT)
    cur.update(kw)


def set_q(b, cur, text, tag):
    """改搜索词必须真敲进框里，并当场确认界面认了这个值。

    只在 python 里把 cur 改掉然后去比数字是最容易骗过自己的做法：
    界面停在旧条件上，比出来的差额会被当成页面的 bug 报出去。
    """
    r = type_search(b, text)
    if not check(tag + "：搜索框在页面上", r is not None, json.dumps(r, ensure_ascii=False)[:120]):
        return False
    if not check(tag + "：框里就是这一步要填的", r["value"] == text, json.dumps(r, ensure_ascii=False)[:220]):
        return False
    cur["q"], cur["page"] = text, 1
    got, synced = wait_chips(b, cur)
    if not check(tag + "：条件牌跟着变了（界面认下这个条件）", synced,
                 "界面 %s｜该是 %s" % (got, expected_chips(cur))):
        return False
    return True


def seg_to(b, cur, aria, key, label, tag):
    got = click_seg(b, aria, label)
    if not check(tag, got == label, "点到的是 %s" % got):
        return False
    cur[key] = SEG_VALUE.get((key, label), "")
    cur["page"] = 1
    return True


def select_to(b, cur, aria, label, key, value, tag):
    got = pick_select(b, aria, label)
    if not check(tag, got == label, "选到的是 %s" % got):
        return False
    cur[key] = value
    cur["page"] = 1
    if key == "intent" and value:
        INTENT_LABEL.setdefault(value, label)
    return True


def main():
    os.makedirs(SHOTS, exist_ok=True)
    regions = api_get("/api/v1/lobby/regions")
    check("大区接口可用", regions.get("enabled") is True, json.dumps(regions, ensure_ascii=False)[:200])
    rlist = regions.get("regions") or []
    default_region = regions.get("defaultRegion") or ""
    say("接口给的大区：%s，默认 %s" % ("、".join(x["label"] for x in rlist), default_region))

    b = Browser(*DESKTOP)
    try:
        b.goto(BASE + "/?cdp=%d#/lobby" % int(time.time()))
        # 登录：这个 profile 可能已经带着上一次的凭据（sessionStorage 在同源同标签里留着），
        # 那就没有登录框可填。所以先看到登录框才去填，而不是照抄固定步骤。
        # 判"进去了"用的是页面根节点 .lobby-page，不是 .tabs——底部那一排在桌面上不渲染。
        if b.wait("!!document.querySelector('.login-form')", tries=20, pause=0.25):
            # 填表必须走 input 事件：直接改 .value 的话 Vue 那份 model 还是空的，
            # 点登录等于拿空凭据去打接口。
            b.js("""(() => {
              const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
              const f = document.querySelector('.login-form');
              set(f.querySelector('input[type=text], input:not([type])'), 'admin');
              set(f.querySelector('input[type=password]'), 'admin');
              sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}));
              f.querySelector('.login-submit').click();
            })()""")
        if not b.wait("!!document.querySelector('.lobby-page')", tries=60, pause=0.3):
            b.shoot(os.path.join(SHOTS, "boot_failed.png"))
            raise SystemExit("登录后没进到账本页：%s" % (b.js("document.body.innerText.slice(0,200)") or ""))
        # 页面级错误钩子必须在整页加载之后打：渲染中途抛错只会在控制台留一句，
        # 只看 DOM 的话那一屏可能看起来"好好的"。
        b.js("window.__errs = [];"
             "addEventListener('error', (e) => window.__errs.push(String(e.message || e.error)));"
             "addEventListener('unhandledrejection', (e) => window.__errs.push('promise:' + String(e.reason)));")
        if not b.wait("!!document.querySelector('.lobby-page table.data tbody tr')", tries=160, pause=0.5):
            d = dom(b)
            b.shoot(os.path.join(SHOTS, "boot_failed.png"))
            raise SystemExit("首屏没出表格：%s" % json.dumps(d, ensure_ascii=False)[:600])

        cur = dict(DEFAULT, region=default_region)

        # —— 1. 默认那一屏 ——————————————————————————————————————————
        d, r = expect(b, cur, "默认（%s 第 1 页 20 间）" % default_region, want_rows=20)
        check("首屏没有加载骨架残留", d["skShown"] is False)
        check("桌面 1440 下没有横向溢出", d["overflow"] <= 0, "溢出 %d px" % d["overflow"])
        labels = {x["accessLabel"] for x in r["rooms"]}
        check("门槛那一列全中文、没有码值漏出来",
              all(not re.search(r"[A-Za-z_]", t) for t in labels), "、".join(sorted(labels)))
        check("统计卡那句「可以直接进」= 这一屏 joinable 行数",
              d["joinableStat"] == "%d 间" % sum(1 for x in r["rooms"] if x["joinable"]),
              "%s vs %d" % (d["joinableStat"], sum(1 for x in r["rooms"] if x["joinable"])))
        say("默认一屏：可用 %s 间，筛出 %d 间，原始记录 %s 条" % (r["usableRooms"], r["total"], r["sourceRecords"]))
        b.shoot(os.path.join(SHOTS, "01_desktop_light.png"))

        # —— 2. 搜索框（有结果 / 没结果两条分支）———————————————————————
        if set_q(b, cur, "服", "填进「服」"):
            d, r = expect(b, cur, "搜索「服」")
            if r["total"]:
                check("搜出来的每一行名字或地址里真的有那个字",
                      all("服" in (x["name"] + x["addr"]) for x in r["rooms"]))
        set_q(b, cur, "", "清空搜索框")

        junk = "zzqq不存在的房间名xx"
        if set_q(b, cur, junk, "填进一个搜不到的词"):
            r = rooms_of(cur)
            check("没搜到时接口回 0 间而不是报错", r["total"] == 0 and isinstance(r["rooms"], list),
                  "total=%s rooms=%s" % (r["total"], type(r["rooms"]).__name__))
            d, ok = settle(b, lambda x: bool(x["emptyTitle"]) and not x["rowCount"] and not x["errBoxes"])
            if check("没搜结果显示空态而不是空表格", ok,
                     json.dumps(d or {}, ensure_ascii=False)[:260]):
                check("空态里有「清掉筛选」这颗按得动的按钮", "清掉筛选" in " ".join(d["emptyButtons"]),
                      " ".join(d["emptyButtons"]))
                check("空态那句把生效条件写了出来", junk in (d["emptyTitle"] or ""), d["emptyTitle"])
            b.shoot(os.path.join(SHOTS, "02_empty.png"))
            # 空态里那颗「清掉筛选」是真按一次：它才是用户在这种情况下唯一抓得到的出口。
            check("空态里那颗「清掉筛选」按得到", click_text(b, ".lobby-page .empty button", "清掉筛选"))
            cur["q"] = ""
            expect(b, cur, "从空态清掉筛选之后")

        # —— 3. 玩法下拉（选项必须来自这一份列表本身）————————————————————
        intents = rooms_of(cur).get("intents") or []
        sels = b.js("[...document.querySelectorAll('.lobby-page select')].map((x) => x.getAttribute('aria-label'))")
        say("页面上的下拉：%s" % "、".join([x for x in (sels or []) if x]))
        if len(intents) >= 2:
            if select_to(b, cur, "按玩法筛选", intents[1]["label"], "intent", intents[1]["value"],
                         "玩法下拉点到「%s」" % intents[1]["label"]):
                d, r = expect(b, cur, "玩法=%s" % intents[1]["label"])
                check("筛完之后每一行都是这个玩法",
                      all((x.get("intent") or "") == intents[1]["value"] for x in r["rooms"]),
                      "、".join(sorted({str(x.get("intent")) for x in r["rooms"]})))
                check("生效条件里出现了那枚 fchip",
                      any(intents[1]["label"] in t for t in d["filterChips"]), " ".join(d["filterChips"]))
                select_to(b, cur, "按玩法筛选", "全部玩法", "intent", "", "玩法放开（选回「全部玩法」）")
                expect(b, cur, "玩法放开之后")
        else:
            say("这一份列表里推不出第二种玩法，玩法下拉按设计不渲染（跳过）")

        # —— 4. 三个分段控件：两个方向各点一次，再点回"不限" ————————————————
        for aria, key, unlimited in (("按门槛筛选", "access", "不限门槛"),
                                     ("按空位筛选", "slots", "不限空位"),
                                     ("按人数筛选", "players", "不限人数")):
            labels = [x for x in b.js("""(() => {
              const g = [...document.querySelectorAll('.lobby-page .seg')].find((x) => x.getAttribute('aria-label') === %s);
              return g ? [...g.querySelectorAll('button')].map((b) => b.textContent.trim()) : [];
            })()""" % js_arg(aria)) if x != unlimited]
            if not check("%s 上有两颗可选的分段（实测拿到 %s）" % (aria, "、".join(labels)), len(labels) == 2):
                continue
            totals = []
            for label in labels:
                if not seg_to(b, cur, aria, key, label, "%s 点到了「%s」" % (aria, label)):
                    continue
                d, r = expect(b, cur, "%s=%s" % (aria, label))
                totals.append(r["total"])
            seg_to(b, cur, aria, key, unlimited, "%s 点回「%s」" % (aria, unlimited))
            d, r = expect(b, cur, "%s 放开之后" % aria)
            if len(totals) == 2:
                # 互补这条留 1% 余量：这三段各读一次接口，而 Klei 那份快照每几十秒就换一版
                # （实测一分钟里总数会漂 10 间上下）。真漏了一种取值会差几千间，不会只差这点。
                room = max(20, int(r["total"] * 0.01))
                check("%s 两个方向加起来 == 放开时的总数（互补，没有第三种取值漏在外面）" % aria,
                      abs(totals[0] + totals[1] - r["total"]) <= room,
                      "%d + %d vs %d（允许差 %d）" % (totals[0], totals[1], r["total"], room))

        # —— 5. 排序与每屏几间 ————————————————————————————————————————
        sorts = [x for x in (rooms_of(cur).get("sorts") or []) if x.get("value")]
        # 不带 sort 参数时后端自己选的序，必须当场验出来。这一页的下拉没有"不限"这一项，
        # 前端只能跟着后端回的那个值填框；后端回空的话浏览器画得出列表、画不出选项文字，
        # 于是给用户一个空白框 + 一份没有任何含义的原序（Klei 快照里的出现顺序）。
        noSort = rooms_of(dict(cur, sort=""))
        ds = (noSort.get("query") or {}).get("sort") or ""
        check("不带 sort 参数时接口自己选定了一枚下拉里真有的排序",
              ds in {x["value"] for x in sorts}, "回的是 %r，可选项 %s" % (ds, [x["value"] for x in sorts]))
        check("不带 sort 参数时那一屏真按它说的那序排", bool(ds) and ordered(noSort["rooms"], ds),
              "前四行 %s" % [(x["players"], x["free"], x["capacity"]) for x in noSort["rooms"][:4]])
        # 这一条是三份默认值（后端 / lobby.js / 这份验收脚本）唯一的对齐点。
        check("接口那个默认排序 == 脚本这一页当作默认的那一枚", ds == cur["sort"],
              "接口 %r vs 脚本 %r" % (ds, cur["sort"]))
        for s in sorts[:3]:
            if not select_to(b, cur, "排序方式", s["label"], "sort", s["value"], "排序选到「%s」" % s["label"]):
                continue
            d, r = expect(b, cur, "排序=%s" % s["label"])
            check("排序「%s」之后接口真的有序" % s["label"], ordered(r["rooms"], s["value"]),
                  "前四行 %s" % [(x["players"], x["free"], x["capacity"]) for x in r["rooms"][:4]])
        if sorts:
            select_to(b, cur, "排序方式", sorts[0]["label"], "sort", sorts[0]["value"], "排序选回默认那一项")
            d, r = expect(b, cur, "排序回默认之后")
            check("选回默认那一项之后还是有序的", ordered(r["rooms"], sorts[0]["value"]),
                  "前四行 %s" % [(x["players"], x["free"], x["capacity"]) for x in r["rooms"][:4]])

        for size in (50, 100, 20):
            if not select_to(b, cur, "每屏几间", "每屏 %d 间" % size, "size", size, "每屏切到 %d 间" % size):
                continue
            d, r = expect(b, cur, "每屏 %d 间" % size)
            check("每屏 %d 间：这一屏行数 == min(条件剩下的, %d)" % (size, size),
                  d["rowCount"] == min(size, r["total"]),
                  "行数 %d，接口 %d 行 / 共 %d 间" % (d["rowCount"], len(r["rooms"]), r["total"]))

        # —— 6. 分页 ——————————————————————————————————————————————————
        rows0 = dom(b)["rowTexts"]
        first = rows0[0] if rows0 else ""
        click_text(b, ".lobby-page .pager .pg-btn", "下一页")
        cur["page"] = 2
        d, r = expect(b, cur, "下一页", want_rows=20)
        check("下一页真的换了一批房间", d and d["rowTexts"] and d["rowTexts"][0] != first,
              (d["rowTexts"][0][:60] if d and d["rowTexts"] else "没有行"))
        check("第 2 页时上一页可点、页码 2 高亮",
              bool(d["prev"]) and d["prev"]["disabled"] is False and d["onNum"] == "2",
              "onNum=%s prev=%s" % (d["onNum"], d["prev"]))
        # 页码超界：接口钳到最后一页，界面必须跟着报最后一页，
        # 不然分页条亮着一个不存在的页，"下一页"点下去永远回同一批。
        probe = dict(cur, page=10 ** 6)
        rr = rooms_of(probe)
        check("页码超界时接口钳到最后一页而不是回空表",
              rr["page"] == rr["pages"] and len(rr["rooms"]) > 0,
              "page=%s pages=%s rows=%s" % (rr["page"], rr["pages"], len(rr["rooms"])))
        cur["page"] = rr["page"]
        nums = dom(b)["pageNums"]
        check("分页条上摆着最后一页那个数字（点得到）", str(rr["page"]) in nums, "页码 %s" % nums)
        click_text(b, ".lobby-page .pager .pg-num", "^%s$" % rr["page"])
        d, r = expect(b, cur, "点最后一页那个页码 %d" % rr["page"])
        check("最后一页的「下一页」是灰的（不做按下去没反应的按钮）", bool(d["next"]) and d["next"]["disabled"] is True,
              "next=%s" % d["next"])
        click_text(b, ".lobby-page .pager .pg-btn", "下一页")
        d = dom(b)
        check("已经最后一页时点下一页不改页码", d["onNum"] == str(cur["page"]), "onNum=%s" % d["onNum"])
        # 上一页一次只走一页：从最后一页点它到的是倒数第二页，不是第 1 页。
        # 上一轮我在这里直接把 cur 拨回第 1 页去比，比出来的是我自己造的差额。
        cur["page"] = max(1, rr["page"] - 1)
        click_text(b, ".lobby-page .pager .pg-btn", "上一页")
        expect(b, cur, "最后一页点一次上一页")
        click_text(b, ".lobby-page .pager .pg-num", "^1$")
        cur["page"] = 1
        expect(b, cur, "点页码 1 回到第一页")

        # —— 7. 多条件叠加 + 单枚 fchip 摘掉 + 全部清掉 ———————————————————
        # 三枚条件全部用真控件打上去：只在 python 里改参数的话，
        # 比出来的差额是我自己造的，不是页面的。
        if not set_q(b, cur, "服", "叠加：填「服」"):
            raise SystemExit("叠加那一步打不上搜索词，后面没法比")
        seg_to(b, cur, "按空位筛选", "slots", "还有空位", "叠加：按空位=还有空位")
        seg_to(b, cur, "按人数筛选", "players", "有人在里面", "叠加：按人数=有人在里面")
        d, r = expect(b, cur, "叠三个条件")
        check("三枚条件各有一枚 fchip", len(d["filterChips"]) == 3, " ".join(d["filterChips"]))
        check("叠了条件就出现「全部清掉」", d["hasClearAll"])
        check("叠加之后的总数不会大于任一单条件",
              r["total"] <= rooms_of(dict(cur, slots="", players=""))["total"],
              "叠三个 %d 间 vs 只带搜索 %d 间" % (r["total"], rooms_of(dict(cur, slots="", players=""))["total"]))
        # 摘掉搜索那一枚（它是三枚里唯一带「含「」的），另外两枚必须原样留着。
        b.js("""(() => {
          const t = [...document.querySelectorAll('.lobby-page .lobby-filters button.fchip')]
            .find((x) => x.textContent.indexOf('含「') >= 0);
          if (!t) return false; t.click(); return true;
        })()""")
        cur["q"] = ""
        d, r = expect(b, cur, "摘掉搜索那一枚 fchip")
        check("摘掉一枚之后剩下两枚", len(d["filterChips"]) == 2, " ".join(d["filterChips"]))
        # 摘掉条件之后搜索框里必须跟着空掉：框里还写着"服"而条件已经不生效，
        # 是这一页最容易骗人的一种不一致。
        box = search_value(b)
        check("摘掉搜索那枚条件之后，搜索框也跟着空了", box == "", "框里还剩 %r" % box)
        click_text(b, ".lobby-page .lobby-filters button.linkish", "全部清掉")
        # 「全部清掉」把界面那五枚条件一起抹掉，python 这一侧必须跟着一起清，
        # 不然 expect 会拿"还带着两枚条件"去比一张空表（上一轮就是这么红的）。
        reset(cur, region=cur.get("region") or default_region)
        d, r = expect(b, cur, "全部清掉")
        check("全部清掉之后 fchip 一个都不剩", not d["filterChips"], " ".join(d["filterChips"]))
        check("全部清掉之后筛出数 == 这个大区的可用数",
              d["filtered"] == "%d 间" % r["usableRooms"], "%s vs %d 间" % (d["filtered"], r["usableRooms"]))
        check("没有条件时不出现「全部清掉」", not d["hasClearAll"])

        # —— 8. 标签展开 / 收起 ————————————————————————————————————————
        # 认准"有 tag-more 的那一行"再操作：第一行未必就是标签超过 6 枚的那一行，
        # 数错了行就等于自己造出一条 FAIL。
        more = b.js("""(() => {
          const b = [...document.querySelectorAll('.lobby-page table.data button.tag-more')]
            .find((x) => /还有 \\d+ 个/.test(x.textContent));
          if (!b) return null;
          const tr = b.closest('tr');
          tr.dataset.probe = '1';
          return {text: b.textContent.trim(), chips: tr.querySelectorAll('.lobby-tags .chip').length};
        })()""")
        if more:
            n_hidden = int("".join(ch for ch in more["text"] if ch.isdigit()))
            b.js("document.querySelector('.lobby-page table.data tr[data-probe]').querySelector('button.tag-more').click()")
            after = b.js("""(() => {
              const tr = document.querySelector('.lobby-page table.data tr[data-probe]');
              const b = [...tr.querySelectorAll('button.tag-more')].find((x) => x.textContent.trim() === '收起');
              return {chips: tr.querySelectorAll('.lobby-tags .chip').length, hasCollapse: !!b};
            })()""")
            check("摊开标签：多出来的枚数 == 那句「还有 %d 个」" % n_hidden,
                  after["chips"] == more["chips"] + n_hidden,
                  "%d + %d vs %d" % (more["chips"], n_hidden, after["chips"]))
            check("摊开之后那颗按钮变成「收起」（不是一枚按下去没反应的按钮）", after["hasCollapse"])
            b.js("document.querySelector('.lobby-page table.data tr[data-probe]').querySelector('button.tag-more').click()")
            back = b.js("""(() => {
              const tr = document.querySelector('.lobby-page table.data tr[data-probe]');
              tr.removeAttribute('data-probe');
              return tr.querySelectorAll('.lobby-tags .chip').length;
            })()""")
            check("收回去之后还是原来那 %d 枚" % more["chips"], back == more["chips"], "%d vs %d" % (back, more["chips"]))
        else:
            say("这一屏 20 行里没有一行标签超过 6 枚（跳过展开那一条）")

        # —— 9. 复制连接：命令里的地址端口必须就是这一行写的 ——————————————————
        row = b.js("""(() => {
          const r = document.querySelector('.lobby-page table.data tbody tr');
          const c = r.querySelectorAll('td');
          return {addr: c[1].textContent.trim(), port: c[2].textContent.trim(),
                  name: c[0].textContent.trim()};
        })()""")
        click_text(b, ".lobby-page table.data tbody button", "复制连接")
        time.sleep(0.8)
        d = dom(b)
        api0 = rooms_of(cur)["rooms"][0]
        want_cmd = 'c_connect("%s", %s)' % (api0["addr"], api0["port"])
        check("那一句的地址和端口就是这一行的（面板没改写、没补默认值）",
              row["addr"] == api0["addr"] and str(row["port"]) == str(api0["port"]),
              "界面 %s 接口 %s:%s" % (json.dumps(row, ensure_ascii=False), api0["addr"], api0["port"]))
        say("复制走的那句：%s" % want_cmd)
        copied = "已复制" in (d["rowTexts"][0] if d["rowTexts"] else "")
        # 两条路都要有一条真的说得出话：剪贴板写成功就按钮变「已复制」，
        # 写失败就必须把那一句摊在屏幕上让人自己选中——
        # "请把上面那句选中"而屏幕上没有那一句，等于把用户指到一个不存在的东西上。
        if copied:
            check("点复制之后按钮变成「已复制」", True)
            check("复制成功时不摆出那句兜底文本（界面上不多出一行没人要的东西）",
                  not d["copyCmd"], "摊出来的是 %r" % d["copyCmd"])
        else:
            check("复制失败时把那一行的命令原样摊出来（可以直接选中）", d["copyCmd"] == want_cmd,
                  "界面 %r vs 该是 %r" % (d["copyCmd"], want_cmd))
        check("按钮的悬停标题里写的也是同一句", want_cmd in d["copyTitle"], d["copyTitle"][:120])
        check("加入方式那句说明写在页面上（不是只有按钮）",
              "控制台" in d["joinNote"] and "IP" in d["joinNote"], d["joinNote"][:160])
        check("那句说明不承诺一个屏幕上不存在的东西（不再叫用户去找「下面这句」）",
              "下面这句" not in d["joinNote"] or bool(d["copyCmd"]), d["joinNote"][:160])

        # —— 10. 取最新（真去打一次 Klei）——————————————————————————————————
        # 「多久前取到」这句话由后端 humanAgo 生成，秒/分钟/小时/天四种形状都可能出现，
        # 所以这里把它解析成秒数再比，而不是去猜某一句固定文案。
        ago0 = dom(b)["fetchedNote"]
        before = rooms_of(cur)
        click_text(b, ".lobby-page button", "取最新")
        # 判"真重打过了"只认一件事：那份快照的取用时间被拨回了当下。
        # 不去断言"读的是缓存"那句话消失——三十秒后自动轮询命中缓存时它本来就该再出现。
        # 上限给 45 秒：页面自己 30 秒一轮，快照在缓存里最多待 60 秒（TTL），
        # 比 45 大只可能是这句话还没跟上，不是按钮没生效。
        def just_refetched(x):
            s = ago_seconds(x["fetchedNote"])
            return x["rowCount"] > 0 and not x["errBoxes"] and s is not None and s <= 45

        d, ok = settle(b, just_refetched, tries=120, pause=1.0)
        ago1 = d["fetchedNote"] if d else None
        after = rooms_of(cur)
        check("取最新真的把快照重新取了一遍（那句变回几十秒内）", ok,
              "取到时间 %r -> %r，界面 %s" % (ago0, ago1, json.dumps(d or {}, ensure_ascii=False)[:160]))
        check("接口那侧同一份快照的取用时刻确实往前推了（不是拿旧缓存换个说法）",
              after["fetchedAt"] != before["fetchedAt"],
              "%s -> %s" % (before["fetchedAt"], after["fetchedAt"]))
        say("取最新前后：%r -> %r，快照时刻 %s -> %s，接口 live=%s，原始记录 %s 条" % (
            ago0, ago1, before["fetchedAt"], after["fetchedAt"], after["live"], after["sourceRecords"]))

        # —— 11. 换大区 —————————————————————————————————————————————————
        if len(rlist) > 1:
            other = [x for x in rlist if x["id"] != default_region][0]
            home = [x for x in rlist if x["id"] == default_region][0]
            live_intents = rooms_of(cur).get("intents") or []
            if len(live_intents) >= 2 and select_to(b, cur, "按玩法筛选", live_intents[1]["label"],
                                                    "intent", live_intents[1]["value"],
                                                    "换大区前先带上玩法"):
                expect(b, cur, "带上玩法之后（给下一步换大区做铺垫）")
            got = click_seg(b, "切换大区", other["label"])
            if check("大区分段点到了「%s」" % other["label"], got == other["label"], str(got)):
                reset(cur, region=other["id"])
                d, r = expect(b, cur, "切到 %s" % other["label"], want_rows=20)
                check("换大区把玩法清掉了（旧大区推出来的玩法不该跟着走）",
                      not any("玩法" in t for t in d["filterChips"]), " ".join(d["filterChips"]))
                click_seg(b, "切换大区", home["label"])
                reset(cur, region=default_region)
                expect(b, cur, "切回默认大区")
        b.shoot(os.path.join(SHOTS, "03_desktop_after.png"))

        # —— 12. 手机布局（390 CSS 像素）———————————————————————————————
        b.set_viewport(*MOBILE)
        time.sleep(1.0)
        d = dom(b)
        check("手机上出卡片并且表格整块藏掉",
              d["cardsShown"] and not d["tableShown"] and d["cardCount"] == 20,
              "卡片 %d 张可见=%s，表格可见=%s" % (d["cardCount"], d["cardsShown"], d["tableShown"]))
        check("手机上没有横向溢出", d["overflow"] <= 0, "溢出 %d px" % d["overflow"])
        rr = rooms_of(cur)
        check("手机上读到的也是同一份筛选结果", d["filtered"] == "%d 间" % rr["total"],
              "%s vs 接口 %d 间" % (d["filtered"], rr["total"]))
        check("手机卡片数 == 接口这一屏的行数", d["cardCount"] == len(rr["rooms"]),
              "%d vs %d" % (d["cardCount"], len(rr["rooms"])))
        b.shoot(os.path.join(SHOTS, "04_mobile.png"))
        b.set_viewport(*DESKTOP)
        time.sleep(0.6)

        # —— 13. 两套配色 ——————————————————————————————————————————————
        click_aria(b, ".icon-btn", "切到深色")
        time.sleep(0.6)
        d = dom(b)
        if check("深色主题切得过去", d["theme"] == "dark", d["theme"]):
            check("深色下这一屏还是 20 行（主题切换不动数据）", d["rowCount"] == 20, "rowCount=%d" % d["rowCount"])
            b.shoot(os.path.join(SHOTS, "05_desktop_dark.png"))
        click_aria(b, ".icon-btn", "切到浅色")
        time.sleep(0.5)
        d = dom(b)
        check("切回浅色", d["theme"] == "light", d["theme"])

        d = dom(b)
        check("整轮操作下来页面没有抛错", not d["pageErrors"], " ; ".join(d["pageErrors"]))
    finally:
        b.close()

    print("\n%d 条不过" % len(fails))
    for f in fails:
        print("  - " + f)
    print("截图在 %s" % SHOTS)
    return 1 if fails else 0


def ordered(rooms, sort):
    """接口承诺的三种排序逐行核对。sort 为空算不过：这一页的列表顺序一直是对的
    （lobby.sortRooms 的 default 分支就是"在线人多在前"），空值要拦的是接口没把
    用的那枚排序回出来——界面据此填下拉，回空就画成一个空白框。"""
    field = {"players": "players", "free": "free", "capacity": "capacity"}.get(sort)
    if not field:
        return False
    keys = [x[field] for x in rooms]
    return all(keys[i] >= keys[i + 1] for i in range(len(keys) - 1))


if __name__ == "__main__":
    sys.exit(main())
