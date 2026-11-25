# 每一页的筛选控件都真去点一次，然后核对"界面上剩下的东西"确实就是该留下的那些。
#
# 为什么不能只看"请求发出去了"：筛选最常见的四种骗人是
#   1) 控件切了、参数发了，但渲染用的是另一份数组（看着筛了其实没筛）；
#   2) 筛完还停在第 3 页，新条件下根本没有第 3 页 → 一张空表，和"没东西"长得一样；
#   3) 下拉里的取值后端不认（或本地过滤条件写反），选了永远空；
#   4) 界面写的条数和列表真正用的条数不同源——按钮上"189 项"，点进去还是 189 项。
# 所以每一项都拿接口给的原始数据算一遍期望值，再和 DOM 里真看到的对比；
# 最后还要看这一路有没有把页面跑出水报错（画布类控件炸了是不出声的）。
#
# 跑法：python .tools/ui_filters_check.py [宽度]     （默认 1440，另跑 390）
# 前提：python .tools/ui_dev.py 已把面板起在 127.0.0.1:8963。
# 这一轮会在夹具里写一次世界规则（改一项再改回去），跑完自己复原。
import base64
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from cdp import BASE, Browser

OUT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), ".tools", "tmp", "shots")
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

FAILED = []
REPORT = []
W = 1440


def check(name, ok, detail=""):
    REPORT.append((("PASS" if ok else "FAIL"), name, detail))
    if not ok:
        FAILED.append(name + "｜" + detail)


# open_page 记下"这一段该在哪一页"。找不到控件时先看一眼是不是真的还在这一页：
# 上一轮浏览器停在了别的页上，脚本一路当自己在操作记录页，112 项里红了 106 项，
# 每一条都只写 no-el，看不出根因。同一页上少个控件是真问题，照旧记 FAIL；
# 页都不对了，后面每一条都是噪音，直接停。
EXPECT_PAGE = []


def off_page(b):
    want = EXPECT_PAGE[0] if EXPECT_PAGE else None
    if want is None:
        return None
    got = (b.js("location.hash") or "").split("?")[0].replace("#", "").strip("/")
    return got if got != want else None


def missing(b, what):
    bad = off_page(b)
    if bad is not None:
        raise RuntimeError("%s 找不到，而浏览器已经不在 #%s，停在 #%s" % (what, EXPECT_PAGE[0], bad or "(空)"))


# ---------- 接口侧：期望值一律从原始数据算，不跟界面商量 ----------
def auth(user, pw):
    return "Basic " + base64.b64encode(("%s:%s" % (user, pw)).encode()).decode()


# 这一轮临时用的账号口令。夹具里跑完就删；绝不写进日志或界面文本。
BOT = "flt_bot"
BOT2 = "flt_bot2"
BOT_PW = "flt-bot-pass-1"
BOT2_PW = "flt-bot2-pass"


def post(path, body, user="admin", pw="admin"):
    req = urllib.request.Request(BASE + path, method="POST",
                                 headers={"Authorization": auth(user, pw), "Content-Type": "application/json"},
                                 data=json.dumps(body).encode("utf-8"))
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, json.loads(r.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return e.code, {}


def delete(path, user="admin", pw="admin"):
    req = urllib.request.Request(BASE + path, method="DELETE", headers={"Authorization": auth(user, pw)})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def api(path, user="admin", pw="admin"):
    req = urllib.request.Request(BASE + path, headers={"Authorization": auth(user, pw)})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, json.loads(r.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode("utf-8"))
        except Exception:
            return e.code, {}


def put(path, body):
    req = urllib.request.Request(BASE + path, method="PUT",
                                 headers={"Authorization": auth("admin", "admin"), "Content-Type": "application/json"},
                                 data=json.dumps(body).encode("utf-8"))
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, json.loads(r.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode("utf-8"))
        except Exception:
            return e.code, {}


def qs(**kw):
    return urllib.parse.urlencode({k: v for k, v in kw.items() if v not in ("", None)})


def audit_count():
    # 列表接口的 limit 上限是 500，拿一次返回的长度当"库里一共多少条"会在
    # 库里超过 500 条时把总数说小，翻页那段期望值跟着错。所以按 500 一页翻到底。
    n, off = 0, 0
    while True:
        _, rp = api("/api/v1/audit?" + qs(limit=500, offset=off))
        d = rp.get("data") or []
        n += len(d)
        if len(d) < 500:
            return n
        off += 500


# ---------- 浏览器侧 ----------
# 页面上任何未捕获异常都收下来：画布类控件（地图的"显示"开关）出错不会在界面上留痕迹，
# 只有这里能看见。
HOOK = ("window.__perr=[];window.addEventListener('error',function(e){"
        "if(window.__perr.length<20)window.__perr.push(String(e.message))});"
        "window.addEventListener('unhandledrejection',function(e){"
        "if(window.__perr.length<20)window.__perr.push('promise:'+String(e.reason&&e.reason.message||e.reason))});")

# 下拉一律"按选项文字找那个 select"，不按下标：select 的先后顺序会随布局改动变，
# 按下标写的断言一改布局就假失败。
PICK_BY_OPT = r"""(() => {
  const want = %(want)s;
  const hit = (o) => o.textContent.trim() === want || o.value === want;
  const el = [...document.querySelectorAll('select')].find((s) => [...s.options].some(hit));
  if (!el) return 'no-option';
  el.value = (el.value === want && want) || [...el.options].find(hit).value;
  el.dispatchEvent(new Event('change', { bubbles: true }));
  return el.value;
})()"""

SELECT_VALUE = r"""(() => {
  const want = %(want)s;
  const el = [...document.querySelectorAll('select')]
    .find((s) => [...s.options].some((o) => o.textContent.trim() === want || o.value === want));
  return el ? el.value : 'no-el';
})()"""

TYPE_INPUT = r"""(() => {
  const el = document.querySelector(%(sel)s);
  // 报 no-el 一定要带上"当时在哪一页"：这一串里有 20 多处 typed()，
  // 页面被谁换掉时红出来的是同一个 'no-el'，两轮下来只能靠猜（上一轮就这样）。
  if (!el) return 'no-el @ ' + location.hash;
  el.value = %(want)s;
  el.dispatchEvent(new Event('input', { bubbles: true }));
  if (%(enter)s) {
    // 日志那一页的关键词写的是 @keyup.enter：只发 keydown 的话界面收不到，
    // 看着像"筛了没反应"，其实是脚本按错了键。keydown + keyup 两种都发，
    // 不管界面绑的是哪一个都能生效。
    const k = { key: 'Enter', code: 'Enter', bubbles: true };
    el.dispatchEvent(new KeyboardEvent('keydown', k));
    el.dispatchEvent(new KeyboardEvent('keyup', k));
  } else el.dispatchEvent(new Event('change', { bubbles: true }));
  return 'ok';
})()"""

CLICK_TEXT = r"""(() => {
  const els = [...document.querySelectorAll(%(sel)s)];
  const want = %(want)s;
  const norm = (x) => x.textContent.trim().replace(/\s+/g, ' ');
  const el = els.find((x) => norm(x) === want) || els.find((x) => norm(x).indexOf(want) >= 0);
  if (!el) return 'no-el: ' + els.map(norm).slice(0, 8).join(' / ');
  el.click();
  return 'ok';
})()"""

# 一行的"身份"：行内第一个房间名按钮（表格和手机卡片都有它），比整行文本可靠。
ROWS = r"""(() => {
  const els = [...document.querySelectorAll(%(sel)s)];
  const clean = (x) => x.textContent.trim().replace(/\s+/g, ' ');
  return { n: els.length,
           texts: els.map(clean).map((t) => t.slice(0, 120)),
           names: els.map((e) => { const k = e.querySelector('.room-name'); return k ? clean(k) : ''; }) };
})()"""

ALL_SELECTS = r"""[...document.querySelectorAll('select')].map((s) => ({
  id: s.id, aria: s.getAttribute('aria-label'),
  opts: [...s.options].map((o) => o.textContent.trim()).slice(0, 12)
}))"""

ERRS = "window.__perr || []"


def pick(b, want, settle=1.2):
    r = b.js(PICK_BY_OPT % {"want": json.dumps(want)})
    if r == "no-option":
        msg = "没有任何 select 有这个选项：%s" % json.dumps(b.js(ALL_SELECTS), ensure_ascii=False)[:400]
        missing(b, "下拉项「%s」" % want)
        check("选下拉项「%s」" % want, False, msg)
        return None
    time.sleep(settle)
    return r


def selected(b, want):
    return b.js(SELECT_VALUE % {"want": json.dumps(want)})


def typed(b, sel, want, enter=False, settle=1.2):
    r = b.js(TYPE_INPUT % {"sel": json.dumps(sel), "want": json.dumps(want),
                           "enter": "true" if enter else "false"})
    if r != "ok":
        missing(b, "输入框 %s" % sel)
        check("输入框 %s 填「%s」" % (sel, want), False, str(r))
        return False
    time.sleep(settle)
    return True


def clicked(b, sel, want, settle=1.2):
    r = b.js(CLICK_TEXT % {"sel": json.dumps(sel), "want": json.dumps(want)})
    if r != "ok":
        missing(b, "%s 里的「%s」" % (sel, want))
        check("点 %s 里的「%s」" % (sel, want), False, str(r))
        return False
    time.sleep(settle)
    return True


def open_page(b, frag, wait_sel):
    b.goto(BASE + frag)
    b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
    b.reload()
    if not b.wait("!!document.querySelector(%s)" % json.dumps(wait_sel), tries=100):
        # 等不到不代表这一页断了。上一轮 #/map 在 390 报了这么一条红，事后单独复现
        # 是 0 秒就出画面——那一会儿整机在跑别的探针，是探针没等到。
        # 所以这里把"页面上到底有什么"一起写进报错：骨架屏还在挂、有空状态、
        # 有报错文案、还是真的什么都没有，三种结论完全不一样，日志必须分得出来。
        snap = b.js("""(() => {
          const t = (n) => (n ? n.textContent.trim() : '');
          const q = (s) => Array.from(document.querySelectorAll(s));
          return JSON.stringify({hash: location.hash, want: %s,
            sk: q('.sk').length, cards: q('.card').length, modRows: q('.mod-row').length,
            rows: q('table.data tbody tr').length, empty: q('.e-title').map(t).slice(0, 2),
            why: (q('.err-box, .x-note').map(t).filter(x => x)[0] || '').slice(0, 120)});
        })()""" % json.dumps(wait_sel))
        raise RuntimeError("页面没出来：%s（现场=%s）" % (frag, snap))
    # 等到的那一屏必须真的是要测的那一页。上一轮这里出的是一百多条红：
    # 浏览器其实停在别的页（读下拉时只读到 map-cluster 那一个 select），
    # 而脚本一路以为自己在操作操作记录页，每个 no-el 都不带页码，只能靠猜。
    want = frag.split("?")[0].replace("#", "").strip("/")
    got = (b.js("location.hash") or "").split("?")[0].replace("#", "").strip("/")
    if got != want:
        raise RuntimeError("open_page(%s) 之后浏览器停在 #%s" % (frag, got or "(空)"))
    EXPECT_PAGE[:] = [want]
    time.sleep(1.4)
    b.js(HOOK)


def rows(b, sel):
    return b.js(ROWS % {"sel": json.dumps(sel)})


def take(b, name):
    b.js("window.scrollTo(0,0)")
    b.shoot(os.path.join(OUT, "filters_%s_%d.png" % (name, W)))


def page_errors(b, where):
    e = b.js(ERRS) or []
    check("%s：这一路没有未捕获异常" % where, not e, " ｜ ".join(e)[:300])


# ================= 世界规则 =================
def world_page(b):
    open_page(b, "/#/room?tab=world", "details.group")
    cluster = b.js("((document.querySelector('#room-pick')||{}).value)||''")
    _, sch = api("/api/v1/world-schema")
    _, wv = api("/api/v1/clusters/%s/shards/Master/world" % urllib.parse.quote(cluster))
    view = wv.get("data") or {}
    loc = next((s for s in (sch.get("data") or []) if s.get("shard") == "Master"), None)
    if not loc:
        check("世界规则：拿到 Master 那张字典表", False, str(list((sch.get("data") or [{}]))[0].keys()))
        return
    items = [it for g in loc.get("groups") or [] for it in (g.get("items") or [])]
    total = len(items)
    changed_api = len(view.get("changedFromDefault") or [])

    got = rows(b, "details.group .grid-form .field")
    check("世界规则：默认列出整张表 %d 项" % total, got["n"] == total, "界面 %d 项" % got["n"])

    # 界面自己数的"与默认不同"必须和后端那句同一口径。
    chip = b.js("""(() => { const x = [...document.querySelectorAll('.chip')]
        .find((y) => y.textContent.indexOf('与默认不同') >= 0); return x ? x.textContent.trim() : ''; })()""")
    n_ui = int((re.search(r"(\d+)", chip) or [None, -1])[1]) if re.search(r"(\d+)", chip) else -1
    check("世界规则：界面数的「与默认不同」和后端同源（%d 项）" % changed_api, n_ui == changed_api,
          "界面 %r，后端 changedFromDefault=%d" % (chip, changed_api))
    check("世界规则：没有把整张表都标成已改", got["n"] == 0 or n_ui != got["n"],
          "屏上 %d 项、已改 %d 项——数字一样说明默认值比较写反了" % (got["n"], n_ui))

    # 搜索：期望条数按字典里 label/key 小写包含算出来。
    typed(b, "input[type=search]", "秋")
    kw = "秋"
    want = [it for it in items if kw in (it.get("label") or "").lower() or kw in (it.get("key") or "").lower()]
    got = rows(b, "details.group .grid-form .field")
    check("世界规则：搜「秋」= %d 项" % len(want), got["n"] == len(want), "界面 %d 项" % got["n"])
    check("世界规则：搜出来的每一项都含「秋」", all(kw in t for t in got["texts"]),
          json.dumps(got["texts"][:3], ensure_ascii=False))
    typed(b, "input[type=search]", "绝不可能出现的设定名")
    got = rows(b, "details.group .grid-form .field")
    title = b.js("((document.querySelector('.e-title')||{}).textContent||'').trim()")
    check("世界规则：搜不到时给空状态而不是白屏", got["n"] == 0 and bool(title), "%d 项，%r" % (got["n"], title))
    typed(b, "input[type=search]", "")

    # 「只看改过的」：按钮上的数字必须就是点进去以后的条数。
    btn = b.js("""(() => { const x = [...document.querySelectorAll('.seg button')]
        .find((y) => y.textContent.indexOf('只看改过的') >= 0); return x ? x.textContent.trim() : ''; })()""")
    m = re.search(r"(\d+)", btn)
    n_btn = int(m.group(1)) if m else -1
    clicked(b, ".seg button", "只看改过的")
    got = rows(b, "details.group .grid-form .field")
    check("世界规则：只看改过的 = 按钮上写的 %d 项" % n_btn, got["n"] == n_btn and n_btn == changed_api,
          "界面 %d 项，按钮 %r，后端 %d 项" % (got["n"], btn, changed_api))
    if changed_api == 0:
        note = b.js("((document.querySelector('.e-note')||{}).textContent||'').trim()")
        check("世界规则：一项都没改过时空状态说清原因", "没有任何一项" in note, repr(note))
    clicked(b, ".seg button", "全部")

    # 改一项 → 界面认它已改 → 写入 → 重开页面还在 → 改回默认 → 又归零。
    # 这一条是"界面显示的值"和"真正发出去的值"之间唯一的闭环证据。
    #
    # 挑哪一项不能闭眼拿屏上第一项：夹具里那一项如果已经被上一轮改成别的值
    # （验收整轮跑下来真会出现，端口那组实测会把服停掉、房间默认落点跟着换），
    # "改到另一个取值"可能正好改回默认，于是"已改 N+1 项"变成"已改 N-1 项"，
    # 报出来的红看着像面板认不出改动。所以只挑"当前就等于字典默认值"的那一项。
    fields = b.js("""(() => [...document.querySelectorAll('details.group .grid-form .field')].map((f) => {
      const s = f.querySelector('select');
      const label = (f.querySelector('label') || {}).textContent || '';
      return { label: label.trim().replace(/\\s+/g, ' '),
               value: s ? s.value : null,
               opts: s ? [...s.options].map((o) => ({ v: o.value, t: o.textContent.trim() })) : [] };
    }))()""")
    same = lambda v: str(v).strip().lower()
    first = it = other = None
    for f in fields:
        d = next((x for x in items if (x.get("label") or "").strip() == f["label"]), None)
        if not d or len(f["opts"]) < 2 or same(f["value"]) != same(d.get("default")):
            continue
        o = next((x for x in f["opts"] if x["v"] != f["value"]), None)
        if not o:
            continue
        first, it, other = f, d, o
        break
    if not first:
        check("世界规则：找得到一项当前等于默认值的下拉来改", False,
              "屏上 %d 项，后端说 %d 项已改，%r" % (len(fields), changed_api, fields[:1]))
        return
    SET_ONE = r"""(() => {
      const f = [...document.querySelectorAll('details.group .grid-form .field')]
        .find((x) => ((x.querySelector('label')||{}).textContent||'').trim().replace(/\s+/g,' ') === %(label)s);
      if (!f) return 'no-field';
      const s = f.querySelector('select');
      s.value = %(val)s;
      s.dispatchEvent(new Event('change', { bubbles: true }));
      return s.value;
    })()"""
    b.js(SET_ONE % {"label": json.dumps(first["label"]), "val": json.dumps(other["v"])})
    time.sleep(1.0)
    got = rows(b, "details.group .grid-form .field")
    n_sel = b.js("document.querySelectorAll('details.group .grid-form select.changed').length")
    # 期望值写成"改之前 + 1"而不是硬编码 1：夹具本来就带着几项和默认不一样时，
    # 硬写 1 会让这一条在"跑完整轮验收"和"单独跑"之间忽红忽绿。
    check("世界规则：改完这一项就多半枚「已改」标记（%d → %d）" % (changed_api, changed_api + 1),
          n_sel == changed_api + 1, "%d 个下拉被标为已改（改之前 %d）" % (n_sel, changed_api))
    check("世界规则：改完以后框里还是新值",
          b.js(SET_ONE % {"label": json.dumps(first["label"]), "val": json.dumps(other["v"])}) == other["v"],
          "回读框里的值")
    clicked(b, ".card button.btn.primary", "写入")
    time.sleep(1.8)
    _, after = api("/api/v1/clusters/%s/shards/Master/world" % urllib.parse.quote(cluster))
    dv = (after.get("data") or {}).get("overrides") or {}
    check("世界规则：写入真的落到文件里（%s=%s）" % (it["key"], other["v"]),
          str(dv.get(it["key"])) == other["v"], "文件里是 %r，后端 changed=%d 项"
          % (dv.get(it["key"]), len((after.get("data") or {}).get("changedFromDefault") or [])))
    b.reload()
    b.wait("!!document.querySelector('details.group')")
    time.sleep(1.6)
    b.js(HOOK)
    # 重开以后必须先把房间钉回写进去那一间再读值：#room-pick 的默认落点是"挑一间在跑的"
    # （store.js 的 pickDefaultCluster），而这一轮跑下来哪间在跑会变。不钉住的话读到的是
    # 另一间房的值，看着像"写入没落盘"——上次那两条红就是这么来的，不是面板的错。
    back_room = pick(b, cluster)
    check("世界规则：重开页面能切回写进去那一间（%s）" % cluster, back_room == cluster,
          "房间选择器给的是 %r" % (back_room,))
    back = b.js("""(() => {
      const f = [...document.querySelectorAll('details.group .grid-form .field')]
        .find((x) => ((x.querySelector('label')||{}).textContent||'').trim().replace(/\\s+/g,' ') === %(label)s);
      const s = f && f.querySelector('select');
      return s ? { v: s.value, changed: s.className.indexOf('changed') >= 0 } : null;
    })()""" % {"label": json.dumps(first["label"])})
    check("世界规则：重开页面读回来的还是那个值，并且标着已改",
          bool(back) and back["v"] == other["v"] and back["changed"], str(back))

    # 复原：改回字典默认值再写一次，夹具不能被这一轮测试留在脏状态。
    b.js(SET_ONE % {"label": json.dumps(first["label"]), "val": json.dumps(it.get("default"))})
    time.sleep(0.8)
    clicked(b, ".card button.btn.primary", "写入")
    time.sleep(1.8)
    _, rest = api("/api/v1/clusters/%s/shards/Master/world" % urllib.parse.quote(cluster))
    rd = rest.get("data") or {}
    check("世界规则：改回默认以后「与默认不同」回到改之前的 %d 项" % changed_api,
          len(rd.get("changedFromDefault") or []) == changed_api
          and it["key"] not in (rd.get("changedFromDefault") or []),
          "文件里 %s=%r，还有 %d 项和默认不一样"
          % (it["key"], (rd.get("overrides") or {}).get(it["key"]), len(rd.get("changedFromDefault") or [])))
    take(b, "world")
    page_errors(b, "世界规则页")


# ================= 游戏日志 =================
def logs_page(b):
    # 这一页现在默认落在玩家事件表，而这一节验的是逐行日志那一种看法，
    # 所以先把看法切过去再等行——直接等 .logline 会在事件表那一屏等到超时。
    open_page(b, "/#/logs", ".seg button")
    if not clicked(b, ".seg button", "逐行日志"):
        return
    if not b.wait("!!document.querySelector('.logline')"):
        raise RuntimeError("逐行日志切过去了但没有行出来")
    cluster = selected(b, "Cluster_1") or selected(b, "ProbeWorld")
    if not cluster:
        cluster = b.js("""(() => { const s = [...document.querySelectorAll('select')]
            .find((x) => [...x.options].some((o) => o.textContent.indexOf('选一个房间') >= 0));
        return s ? s.value : ''; })()""")
    shard = b.js("""(() => { const s = [...document.querySelectorAll('select')]
        .find((x) => [...x.options].some((o) => o.textContent.indexOf('选一个世界') >= 0));
      return s ? s.value : ''; })()""") or "Master"

    def read(**kw):
        st, rp = api("/api/v1/clusters/%s/shards/%s/log?%s"
                     % (urllib.parse.quote(cluster), urllib.parse.quote(shard),
                        qs(kind=kw.get("kind", "server"), lines=kw.get("lines", 200),
                           level=kw.get("level", ""), event=kw.get("event", ""), q=kw.get("q", ""))))
        return ((rp.get("data") or {}).get("entries") or []), st

    def expect(why, **kw):
        want, st = read(**kw)
        g = rows(b, ".logline")
        check("日志：%s = 接口给的 %d 行" % (why, len(want)), g["n"] == len(want) and st == 200,
              "界面 %d 行，接口 %d 行（HTTP %d）" % (g["n"], len(want), st))
        return want

    base = expect("默认这一屏")
    check("日志：默认屏上确实有内容", len(base) > 0, "%d 行" % len(base))

    # 类别：服务端日志 / 聊天与公告 是两个不同的文件，切换必须换内容。
    clicked(b, ".seg button", "聊天与公告")
    chat = expect("切到「聊天与公告」", kind="chat")
    check("日志：两类日志不是同一份东西",
          sorted([r.get("raw") for r in base]) != sorted([r.get("raw") for r in chat]),
          "%d 行 vs %d 行" % (len(base), len(chat)))
    clicked(b, ".seg button", "服务端日志")

    # 关键词：按界面说明是回车生效。
    typed(b, "input[type=search]", "Server", enter=True)
    want = expect("关键词 Server", q="Server")
    check("日志：筛完屏上每一行都含这个词",
          all("server" in r.get("raw", "").lower() for r in want) and len(want) > 0,
          "%d 行，第一行=%r" % (len(want), (want[0].get("raw") if want else "")[:60]))
    typed(b, "input[type=search]", "notawordinworld", enter=True)
    g = rows(b, ".logline")
    title = b.js("((document.querySelector('.e-title')||{}).textContent||'').trim()")
    check("日志：没命中的词给空状态", g["n"] == 0 and bool(title), "%d 行，%r" % (g["n"], title))
    typed(b, "input[type=search]", "", enter=True)

    # 级别下限：三档各算一遍期望条数，并确认真的越来越严。
    lv = {}
    for label, code in [("普通及以上", ""), ("警告及以上", "warn"), ("报错及以上", "error")]:
        pick(b, label)
        lv[label] = len(expect("级别下限「%s」" % label, level=code))
    check("日志：级别越高手里剩下的行不会变多",
          lv["普通及以上"] >= lv["警告及以上"] >= lv["报错及以上"],
          " / ".join("%s=%d" % (k.split("及")[0], v) for k, v in lv.items()))
    pick(b, "全部级别")

    # 事件字典里的每一项都必须筛得动（选了永远空 = 下拉里摆了个假选项）。
    # 三个口径都得说清，不然这条检查会把自己问倒：
    # 一是窗口——夹具插了一大截落盘噪音以后，默认 200 行已经把开机那两行切到窗口外，
    #    那样量出来的"筛不出东西"说的是窗口，不是这个选项是不是假的；
    # 二是文件——玩家发言和引擎公告只写在聊天那一份里，在服务端那份当然筛不到；
    # 三是世界——"洞穴正在连地表"这类行按引擎的设计只有洞穴那份会打，地表永远筛不到。
    # 所以问"这个选项在四个（世界 × 文件）组合里是不是一行都没有"，一次都没有才是死的。
    def rows_at(sh, kd, code):
        st, rp = api("/api/v1/clusters/%s/shards/%s/log?%s"
                     % (urllib.parse.quote(cluster), urllib.parse.quote(sh),
                        qs(kind=kd, lines=2000, event=code)))
        if st != 200:
            return None          # 这个组合压根不存在（没有这个世界 / 还没写文件）
        return (rp.get("data") or {}).get("entries") or []

    evsel = b.js("""(() => { const s = [...document.querySelectorAll('select')]
        .find((x) => [...x.options].some((o) => o.textContent.trim() === '全部事件'));
      return s ? [...s.options].map((o) => o.value).filter(Boolean) : []; })()""")
    dead, mismatch = [], []
    for code in evsel:
        pick(b, code, settle=0.5)
        # 界面这一屏用的还是当前窗口（默认 200 行），比对就得按同一档问接口。
        want, st = read(event=code)
        g = rows(b, ".logline")
        if st != 200 or g["n"] != len(want):
            mismatch.append("%s 界面%d/接口%d" % (code, g["n"], len(want)))
        if not any(rows_at(sh, kd, code) for sh in ("Master", "Caves") for kd in ("server", "chat")):
            dead.append(code)
    check("日志：每一个事件选项点下去，屏上的行数都等于接口给的",
          not mismatch, "对不上：" + " ".join(mismatch) + "｜字典 %d 项" % len(evsel))
    check("日志：事件字典里没有哪一份文件都筛不出东西的假选项（整份文件都翻过一遍）",
          not dead, "四处都空：" + " ".join(dead))
    pick(b, "全部事件")

    # 回看行数：改小必须真的少读（这一项直接进请求参数）。
    pick(b, "50 行")
    check("日志：回看行数改成 50 以后请求跟着变", len(expect("回看 50 行", lines=50)) <= 50, "")
    pick(b, "200 行")
    take(b, "logs")
    page_errors(b, "日志页")


# ================= 存档快照 =================
def snaps_page(b):
    _, reply = api("/api/v1/backups")
    d = reply.get("data") or {}
    snaps = d.get("snapshots") or []
    clusters = d.get("clusters") or []
    open_page(b, "/#/snaps", ".snap-page")
    SEL = ".snap-page .mod-row"
    got = rows(b, SEL)
    check("快照：默认列出 %d 份" % len(snaps), got["n"] == len(snaps), "界面 %d" % got["n"])

    opts = b.js("""(() => { const s = [...document.querySelectorAll('select')]
        .find((x) => [...x.options].some((o) => o.textContent.trim() === '全部房间'));
      return s ? [...s.options].map((o) => o.textContent.trim()) : []; })()""")
    check("快照：房间下拉列出了全部 %d 个房间" % len(clusters),
          all(c in " ".join(opts) for c in clusters), str(opts))
    for c in clusters:
        pick(b, c)
        want = [s for s in snaps if s.get("cluster") == c]
        got = rows(b, SEL)
        check("快照：只看 %s = %d 份" % (c, len(want)), got["n"] == len(want), "界面 %d 份" % got["n"])
        check("快照：筛完每份都写着 %s" % c, all(c in t for t in got["texts"]),
              json.dumps(got["texts"][:2], ensure_ascii=False))
    pick(b, "全部房间")
    got = rows(b, SEL)
    check("快照：切回全部又回到 %d 份" % len(snaps), got["n"] == len(snaps), "界面 %d 份" % got["n"])
    take(b, "snaps")
    page_errors(b, "快照页")


# ================= 任务中心 =================
def seed_backup_task():
    """真打一次备份，造出一条"标题 ≠ 正式名"的任务。

    backup.create 的正式名是「备份存档」，而任务标题带着房间名（"备份存档 · Cluster_1"）。
    夹具里常常只剩一种任务、标题又恰好等于正式名，那种情况下"选项要用正式名"的比对是空转的，
    改坏了也不会红。返回 (任务 id, 跑之前已有的备份 id 集合)；打不成也返回集合，让下面把
    空转这件事明说，而不是默默给一个 PASS。
    """
    _, rp = api("/api/v1/backups")
    before = {bk.get("id") for bk in (rp.get("data") or {}).get("snapshots") or []}
    st, r = post("/api/v1/clusters/Cluster_1/backups", {})
    if st not in (200, 201, 202):
        return None, before
    tid = (r.get("data") or {}).get("taskId")
    # 任务没有按 id 单查的接口（列表 + 取消才有），只能从列表里找这一条。
    for _ in range(60):
        _, tr = api("/api/v1/tasks?limit=200")
        mine = [t for t in (tr.get("data") or []) if t.get("id") == tid]
        if mine and mine[0].get("status") in ("succeeded", "failed", "cancelled"):
            return tid, before
        time.sleep(1.0)
    return tid, before


def tasks_page(b):
    tid, bk_before = seed_backup_task()
    _, reply = api("/api/v1/tasks?limit=200")
    tasks = reply.get("data") or []
    open_page(b, "/#/tasks", ".task-page")
    SEL = ".task-page .mod-row"
    got = rows(b, SEL)
    check("任务：默认列出 %d 条" % len(tasks), got["n"] == len(tasks), "界面 %d 条" % got["n"])

    def expect(why, pred):
        want = [t for t in tasks if pred(t)]
        g = rows(b, SEL)
        check("任务：%s = %d 条" % (why, len(want)), g["n"] == len(want), "界面 %d 条" % g["n"])
        if want:
            check("任务：%s 筛完每行都带着这一类的标题" % why,
                  all(any(t["title"] in x for t in want) for x in g["texts"]),
                  json.dumps(g["texts"][:1], ensure_ascii=False))
        return want

    seen = {}
    for label, pred in [("没结束", lambda t: t["status"] in ("queued", "running")),
                        ("已完成", lambda t: t["status"] == "succeeded"),
                        ("失败", lambda t: t["status"] == "failed"),
                        ("已取消", lambda t: t["status"] == "cancelled"),
                        ("全部", lambda t: True)]:
        clicked(b, ".task-filters .seg button", label)
        seen[label] = expect("状态「%s」" % label, pred)
    check("任务：四个状态档的条数加起来等于全部",
          sum(len(v) for k, v in seen.items() if k != "全部") == len(seen["全部"]),
          " / ".join("%s=%d" % (k, len(v)) for k, v in seen.items()))

    kinds = sorted({t["kind"] for t in tasks})
    # 选项文字用 /api/v1/task-kinds 那份正式名字：一行的标题带着房间名和时间
    # （"定时备份存档 · Cluster_1"），拿它当选项写的是一间、点下去筛的是这一类全部。
    _, kr = api("/api/v1/task-kinds")
    names = {k["kind"]: k["label"] for k in (kr.get("data") or []) if isinstance(k, dict)}
    first_title = {}
    for t in tasks:
        first_title.setdefault(t["kind"], t["title"])
    want_opts = {k: (names.get(k) or first_title[k]) for k in kinds}
    opts = b.js("""(() => { const s = document.querySelector('.task-filters select')
        || [...document.querySelectorAll('select')].find((x) => [...x.options].some((o) => o.textContent.trim() === '全部类型'));
      return s ? [...s.options].map((o) => ({ v: o.value, t: o.textContent.trim() })) : []; })()""")
    by_value = {o["v"]: o["t"] for o in opts if o["v"]}
    miss = [k for k in kinds if k not in by_value]
    bad_lab = [k for k in kinds if k in by_value and by_value[k] != want_opts[k]]
    check("任务：类型下拉列出了这 %d 条任务里的全部类型" % len(tasks), not miss,
          "下拉里没有：%s（下拉=%s）" % (" ".join(miss), [o["t"] for o in opts]))
    check("任务：每一枚选项写的就是那一类的正式名字（不是某一行自己的标题）",
          not bad_lab, " ".join("%s：界面「%s」应为「%s」" % (k, by_value.get(k), want_opts.get(k))
                                for k in bad_lab)
                     or " ".join("%s=「%s」" % (k, v) for k, v in sorted(want_opts.items())))
    # 上面那条比对只在"确实有一种任务，它的标题和正式名不一样"时才有牙齿。
    # 夹具里只剩 server.update 时，标题恰好就等于正式名，改坏了也照样绿——所以把这个
    # 前提本身也钉成断言，前提没了就报出来，别让它悄悄退化成空转。
    diverged = [k for k in kinds if k in names and first_title[k] != names[k]]
    check("任务：这一轮的夹具里确实有标题≠正式名的类型（否则上面的比对是空转）",
          bool(diverged), "分叉的：%s；标题=%s / 正式名=%s" % (
              " ".join(diverged),
              " ".join(sorted(set(first_title.get(k, "") for k in kinds))),
              " ".join(sorted(names.get(k, "-") for k in kinds))))
    for k in kinds[:3]:
        pick(b, want_opts[k])
        expect("按类型「%s」" % want_opts[k], lambda t, x=k: t["kind"] == x)
    pick(b, "全部类型")
    take(b, "tasks")
    page_errors(b, "任务页")
    cleanup_seeded_backups(bk_before)
    return kinds, sorted(want_opts.values())


def cleanup_seeded_backups(before):
    """删掉这一轮打出来的备份，别一次比试往夹具目录里堆一个包。
    任务本身留着：任务中心本来就容得下任意条数，删任务反而要动别的页的数据。"""
    _, rp = api("/api/v1/backups")
    after = (rp.get("data") or {}).get("snapshots") or []
    fresh = [bk.get("id") for bk in after if bk.get("id") not in before]
    for bid in fresh:
        delete("/api/v1/backups/%s" % bid)
    _, rp = api("/api/v1/backups")
    left = [bk.get("id") for bk in ((rp.get("data") or {}).get("snapshots") or [])
            if bk.get("id") in fresh]
    check("任务：这一轮种的备份删干净了", not left,
          "种了 %s 个，还剩 %s 个" % (len(fresh), len(left)))


# ================= 操作记录 =================
def drop_bots():
    """上一轮如果中途炸了，号会留在库里；这一轮建号前先按 id 清一遍，否则 409 假失败。"""
    _, accts = api("/api/v1/accounts")
    for a in accts.get("data") or []:
        if isinstance(a, dict) and a.get("username") in (BOT, BOT2):
            delete("/api/v1/accounts/%s" % a["id"])


def audit_page(b):
    # 操作人下拉要有第二、第三个人，就得让这两个账号各真做过一次写操作。
    # 挑的都是只动临时账号自己的写入：建号、用别人的身份再建一个号、管理员重置自己的密码，
    # 跑完把号删掉，夹具里没有别的东西被改过（面板设置那些键一被显式写入就会标成"已改"，不能碰）。
    drop_bots()
    st, _ = post("/api/v1/accounts", {"username": BOT, "password": BOT_PW, "role": "admin"})
    check("操作记录：临时账号 %s 建得出来" % BOT, st in (200, 201), "HTTP %d" % st)
    st, _ = post("/api/v1/accounts", {"username": BOT2, "password": BOT2_PW, "role": "admin"}, BOT, BOT_PW)
    check("操作记录：%s 建出第二个账号" % BOT, st in (200, 201), "HTTP %d" % st)
    _, accts = api("/api/v1/accounts")
    ids = {a.get("username"): a.get("id") for a in (accts.get("data") or []) if isinstance(a, dict)}
    st, _ = put("/api/v1/accounts/%s/password" % ids.get(BOT2), {"password": BOT2_PW + "-x"})
    check("操作记录：管理员给 %s 重置一次密码" % BOT2, st == 200, "HTTP %d（id=%r）" % (st, ids.get(BOT2)))

    n_total = audit_count()
    open_page(b, "/#/audit", ".audit-page")
    # 窄屏上这四组筛选默认收起（.audit-filters 那条断点），真人得先点「筛选」才够得着。
    # 这里必须先点开展开再往下测：JS 点得到 display:none 的控件，
    # 不点的话整轮都在替用户点他根本看不见的按钮，那种绿是假的。
    vis = b.js("(() => { const f = document.querySelector('.audit-filters');"
               "const t = document.getElementById('audit-filters-toggle');"
               "return { filters: !!(f && f.offsetParent), toggle: !!(t && t.offsetParent) }; })()")
    if W <= 560:
        check("操作记录：窄屏上筛选默认收起、展开按钮看得见",
              vis.get("filters") is False and vis.get("toggle") is True, json.dumps(vis))
        clicked(b, "#audit-filters-toggle", "筛选")
        opened = b.js("!!(document.querySelector('.audit-filters') || {}).offsetParent")
        check("操作记录：点「筛选」把那四组控件展开了", opened is True, str(opened))
    else:
        check("操作记录：宽屏上筛选一直是展开的、不给展开按钮",
              vis.get("filters") is True and vis.get("toggle") is False, json.dumps(vis))
    SEL = ".audit-page .mod-row"
    PAGE = 50
    got = rows(b, SEL)
    check("操作记录：默认列出第一页 %d 条" % min(PAGE, n_total),
          got["n"] == min(PAGE, n_total), "界面 %d 条，库里 %d 条" % (got["n"], n_total))

    def expect(why, **kw):
        st, rp = api("/api/v1/audit?" + qs(limit=PAGE, **kw))
        want = (rp.get("data") or []) if st == 200 else None
        g = rows(b, SEL)
        check("操作记录：%s 筛得动" % why, st == 200 and want is not None, "HTTP %d" % st)
        if want is not None:
            # 界面是异步的：下拉改完要等它自己发请求、等回答落地。上一轮这里读到的是
            # "筛选还没追上界面"的旧条数，一条红引出上百条连带红。给六秒追平，
            # 追不平才算真不一致（真不筛的话六秒后照样报红，数字用的是最后一次读到的）。
            for _ in range(20):
                g = rows(b, SEL)
                if g["n"] == len(want):
                    break
                time.sleep(0.3)
            check("操作记录：%s = 接口给的 %d 条" % (why, len(want)), g["n"] == len(want),
                  "界面 %d 条，接口 %d 条" % (g["n"], len(want)))
        return want

    # 下拉该有的操作人取自 /api/v1/audit-actions（界面那枚下拉就是这么来的），
    # 而不是从列表接口的前 500 条里凑——库里早于 500 条的人会被漏掉，
    # 于是"下拉少个人"这种真 bug 会被测成"库里本来就没有"。
    _, aa = api("/api/v1/audit-actions")
    actors = sorted((aa.get("data") or {}).get("actors") or [])
    opts = b.js("""(() => { const s = document.querySelector('#audit-actor');
      return s ? [...s.options].map((o) => o.textContent.trim()) : []; })()""")
    check("操作记录：操作人下拉列出了库里出现过的人（%d 个）" % len(actors),
          all(a in opts for a in actors), "下拉=%s 库里=%s" % (opts, actors))
    for a in actors:
        pick(b, a)
        expect("只看操作人 %s" % a, actor=a)

    pick(b, "全部操作人")
    typed(b, "#audit-target", "Cluster_1")
    expect("对象含 Cluster_1", target="Cluster_1")
    typed(b, "#audit-target", "cluster_1")
    expect("对象小写 cluster_1（SQLite LIKE 大小写不敏感）", target="cluster_1")
    typed(b, "#audit-target", "")

    # 动作下拉：每一项都要能被列表接口接受，不然就是"选了永远空"。
    acts = b.js("""(() => { const s = document.querySelector('#audit-action');
      return s ? [...s.options].map((o) => o.value).filter(Boolean) : []; })()""")
    bad = []
    for code in acts:
        st, _ = api("/api/v1/audit?" + qs(limit=5, action=code))
        if st != 200:
            bad.append("%s→%d" % (code, st))
    check("操作记录：动作下拉的 %d 项全被接口接受" % len(acts), not bad, " ".join(bad[:6]))

    # 四条轴同时开着：生效条件一条一枚，点掉一枚就少一条。
    pick(b, acts[0], settle=0.6) if acts else None
    clicked(b, ".audit-when .seg button", "近 7 天")
    if actors:
        pick(b, actors[0])
    typed(b, "#audit-target", "Cluster_1")
    chips = b.js("""[...document.querySelectorAll('.audit-on .fchip')].map((x) =>
      x.textContent.trim().replace(/×$/,'').trim())""")
    check("操作记录：四条轴挂着四枚生效条件", len(chips) == 4, str(chips))
    # 翻页这一项要在不带条件时测：挂着四枚筛选去点下一页，那一屏本来就没第二页，
    # 测出来的"上一页点不动"是筛选的功劳，不是翻页的。
    clicked(b, ".audit-on button.linkish", "全部清掉")
    # 总数按接口一页 500 翻到底重数一遍：这一路只有界面在读，没有别的地方在写
    # （审计表自己不会长行，实测过），所以翻到第几页该有几条是钉死的。
    n_total = audit_count()
    pages = (n_total + PAGE - 1) // PAGE

    def btn_state(text):
        return b.js("""(() => { const x = [...document.querySelectorAll('button')]
            .find((y) => y.textContent.indexOf(%s) >= 0); return x ? !x.disabled : null; })()"""
                    % json.dumps(text))

    PG = ("""(() => { const x = document.querySelector('.pager .muted');
          return !!x && x.textContent.indexOf('第 ' + %d + ' 页') >= 0; })()""")

    def on_page(want_page):
        return b.js(PG % want_page)

    def raw_next():
        b.js("""(() => { const x = [...document.querySelectorAll('button')]
            .find((y) => y.textContent.indexOf('下一页') >= 0 && !y.disabled); if (x) x.click(); })()""")

    def click_next(want_page):
        raw_next()
        # 等页码那一句真的变成第 N 页，不等固定 1.2 秒：这一圈要跑两百多页，
        # 睡死的时长既把整段拖成几分钟，又挡不住"接口慢了一点就读到上一页"。
        return bool(b.wait(PG % want_page, tries=25, pause=0.2))

    bad = []
    lost = ""
    for p in range(1, pages + 1):
        want = min(PAGE, n_total - (p - 1) * PAGE)
        g = rows(b, SEL)
        # 表 0 条 + 两枚翻页按钮都不在 DOM = 这一页整块不在画面上（界面空表时连
        # .pager 都不渲染，所以"掉出画面"和"这一页只剩 49 条"是两件事）。上一轮这一段
        # 从第 130 页起连抄 90 条"0 条（应 50）"，最后一句"浏览器停在 #(空)"才露出根因，
        # 每一条红说的都是同一个事实。现在第一页掉出就停，并把三条线索一起报出来：
        # 有没有那面错误框（接口那次失败也会把 rows 清空、pager 整块消失，长得和
        # "掉出画面"一模一样）、地址（页被换掉了）、正文长度（只剩几个字是标签页崩了）。
        if g["n"] == 0 and btn_state("下一页") is None and btn_state("上一页") is None:
            lost = ("第 %d 页起这一页整块不在画上了｜错误框=%r｜href=%r｜正文长度=%r"
                    % (p, b.js("""(() => { const x = document.querySelector('.err-box');
                        return x ? x.textContent.slice(0, 160) : ''; })()"""),
                       b.js("location.href"),
                       b.js("(document.body && document.body.textContent.length) || -1")))
            break
        if g["n"] != want:
            bad.append("第 %d 页 %d 条（应 %d）" % (p, g["n"], want))
        if btn_state("下一页") != (p < pages):
            bad.append("第 %d 页下一页可点=%r（应 %r）" % (p, btn_state("下一页"), p < pages))
        if btn_state("上一页") != (p > 1):
            bad.append("第 %d 页上一页可点=%r" % (p, btn_state("上一页")))
        if p < pages:
            click_next(p + 1)
            time.sleep(0.15)
    check("操作记录：%d 条分 %d 页，每页条数和翻页按钮都对得上" % (n_total, pages),
          not bad and not lost,
          ("翻页途中掉出画面：" + lost) if lost
          else (" ｜ ".join(bad) or "每页都只多要一条来判有无下一页"))
    # 换筛选必须回第一页（否则新条件下没有第二页 → 看着像空）。
    if pages > 1 and not lost:
        clicked(b, ".audit-when .seg button", "近 30 天")
        check("操作记录：换了时间档就回到第一页", btn_state("上一页") is False,
              "上一页仍可点=%r" % btn_state("上一页"))

    # 一键清掉要在"确实挂着条件"的状态下点，否则点的是空气。上面翻页那一段按设计
    # 是把四条轴清干净才开始翻的，所以这里必须自己挂一枚最小的条件回来。
    # （早先不是这么写的：它靠翻页中间那句「近 30 天」顺手重新挂条件，于是这一条
    # 只在审计表够两页时才有点得到——1440 那档 36 条只有一页，就报成"按钮不见了"。）
    clicked(b, ".audit-when .seg button", "近 7 天")
    hung = b.js("document.querySelectorAll('.audit-on .fchip').length")
    check("操作记录：清掉之前确实挂着条件（先挂一枚时间档）", hung == 1, "%d 枚" % hung)

    clicked(b, ".audit-on button.linkish", "全部清掉")
    n = b.js("document.querySelectorAll('.audit-on .fchip').length")
    g = rows(b, SEL)
    check("操作记录：全部清掉回到不带任何条件", n == 0 and g["n"] == min(PAGE, n_total),
          "%d 枚条件 / %d 条" % (n, g["n"]))

    # 满页边界：恰好 50 条时不许给出"点下去是一片空表"的下一页。
    # 这一份夹具本身凑不出正好 50 条的筛选（每轮记录数都在长），所以自己造一个：
    # 建一批用户名带同一串独占前缀的临时账号，一条建号 = 一条审计（target=用户名），
    # 于是"对象含这串前缀"的条数由我们说了算。测完立刻全删掉。
    pfx = "bnd" + os.urandom(3).hex()
    made = []
    try:
        exact = None
        for i in range(PAGE):
            st, rp = post("/api/v1/accounts",
                          {"username": "%s%03d" % (pfx, i), "password": BOT_PW, "role": "viewer"})
            if st not in (200, 201):
                break
            made.append((rp.get("data") or {}).get("id"))
            if i + 1 == PAGE:
                break
        _, rp = api("/api/v1/audit?" + qs(limit=PAGE + 1, target=pfx))
        got = rp.get("data") or []
        _, rp2 = api("/api/v1/audit?" + qs(limit=1, offset=PAGE, target=pfx))
        n_api = len(got)
        check("操作记录：造出恰好 %d 条的筛选（前缀 %s）" % (PAGE, pfx),
              n_api == PAGE and not (rp2.get("data") or []),
              "接口 %d 条，第 %d 条=%r" % (n_api, PAGE + 1, bool(rp2.get("data"))))
        if n_api == PAGE:
            exact = pfx
        if exact:
            typed(b, "#audit-target", exact)
            g = rows(b, SEL)
            check("操作记录：恰好 %d 条（对象含「%s」）时不许有假的下一页" % (PAGE, exact),
                  g["n"] == PAGE and btn_state("下一页") is False,
                  "界面 %d 条，下一页可点=%r" % (g["n"], btn_state("下一页")))
            raw_next()
            after = rows(b, SEL)
            check("操作记录：满页时硬点下一页不会掉进空屏", after["n"] == PAGE,
                  "点完剩 %d 条（按钮可点=%r）" % (after["n"], btn_state("下一页")))
            clicked(b, ".audit-on button.linkish", "全部清掉")
    finally:
        left = 0
        for acct_id in made:
            if acct_id and delete("/api/v1/accounts/%s" % acct_id) in (200, 204):
                continue
            left += 1
        check("操作记录：%d 个满页临时账号都清了" % len(made), left == 0, "剩 %d 个没删掉" % left)
    take(b, "audit")
    page_errors(b, "操作记录页")

    for u in (BOT2, BOT):
        _, accts = api("/api/v1/accounts")
        ids = {a.get("username"): a.get("id") for a in (accts.get("data") or []) if isinstance(a, dict)}
        if ids.get(u):
            st = delete("/api/v1/accounts/%s" % ids[u])
            check("操作记录：临时账号 %s 删掉了" % u, st in (200, 204, 404), "HTTP %d" % st)


# ================= 总览：在线趋势的时间窗 =================
def overview_page(b):
    # 时间窗是这一张卡唯一的筛选，所以期望值全部从 /api/v1/stats 现取现比：
    # 界面写"过去 24 小时"就得真覆盖 24 小时，写"每 5 分钟一个点"就得真是 5 分钟。
    open_page(b, "/#/", ".ov-trend .ov-when")
    # 房间是异步加载出来的，趋势那一段亮着不代表能点到房间；
    # 不等它就往下走，第一次进页面会"no-tab"，看着像趋势卡坏了。
    if not b.wait("!!document.querySelector('.room-tabs button')"):
        raise RuntimeError("房间页签一直没出来")
    SEG = ".ov-trend .ov-when .seg button"
    cur = {"cluster": ""}

    def pick_room(text):
        # #106 之后切房间是页签条，不再是每张房间卡上那枚「看这个房间」/「当前查看」。
        # 接口要的存档目录名从这一枚页签的悬停里现取（那句写的就是「存档目录名：X」）：
        # 以前脚本把 Cluster_1 写死在 URL 里，而界面选的是另一间——夹具一改目录名，
        # 两边各算各的都照样绿，比的是两个房间。取不到目录名就直接红，不许兜底。
        raw = b.js("""(() => {
          const t = [...document.querySelectorAll('.room-tabs button')].find((x) =>
            x.textContent.indexOf(%s) >= 0);
          if (!t) return JSON.stringify(['no-tab']);
          const m = (t.title || '').match(/存档目录名：([^\\s·]+)/);
          t.click();
          return JSON.stringify(['ok', m ? m[1] : '']);
        })()""" % json.dumps(text))
        try:
            st, got = json.loads(raw)
        except Exception:
            st, got = "bad-parse", ""
        if st == "ok" and not got:
            st = "no-cluster"
        if st == "ok":
            cur["cluster"] = got
        check("趋势：点到房间「%s」" % text, st == "ok",
              "%s 目录名=%r" % (st, got))
        time.sleep(2.2)
        b.wait("!!document.querySelector('.ov-trend .ov-when .chip.mono')"
               " || !!document.querySelector('.ov-trend .err-box')")
        time.sleep(0.4)
        # 点了哪一间，卡片头上那枚房间把手就得写着哪一间的目录名——
        # 这一条把「界面选中的」和「脚本拿去问接口的」钉成同一个房间。
        head = b.js("""(() => { const x = document.querySelector('.ov-trend .card-head .chip');
          return x ? (x.getAttribute('title') || '') + '|' + x.textContent.trim() : ''; })()""")
        check("趋势：卡片头那间就是刚点的那间",
              cur["cluster"] in str(head) and text in str(head), str(head)[:120])

    def chips():
        return b.js("""[...document.querySelectorAll('.ov-trend .ov-when .chip')].map((x) => x.textContent.trim())""")

    def on_btn():
        return b.js("""(() => { const x = document.querySelector('.ov-trend .ov-when .seg button.on');
          return x ? x.textContent.trim() : ''; })()""")

    def api_trend(hours):
        st, rep = api("/api/v1/stats?" + qs(cluster=cur["cluster"], hours=hours))
        return st, (rep.get("data") or {})

    def bracket(hours, act, fresh_load=False):
        # 界面写的那两个数（几个点、几条采样）来自它自己那一次请求，而这个房间每分钟
        # 每个世界都要落一条采样，拿"某一次接口读数 == 界面"做严格相等会变成偶发假失败。
        # 判据钉在区间上：先读一次接口，再让界面去重取（act），读到界面写的数以后再读一次
        # 接口——界面那一次一定夹在这两次中间，所以点数与条数必须落在 [前, 后] 里，
        # 格子说法两头都得一模一样。区间是接口自己给的两次读数，不是我留的余量：
        # 窗口切错（还留着上一个窗口的曲线）会一下差出几个量级，照样出界。
        st0, d0 = api_trend(hours)
        act()
        if fresh_load:
            # 整页刷新以后 Vue 还要起来一轮才轮到这张卡，等它把数写上来再读。
            b.wait("!!document.querySelector('.ov-trend .ov-when .chip.mono')")
        time.sleep(2.0)
        got = chips()
        st1, d1 = api_trend(hours)
        nums = re.findall(r"(\d+) 点 / (\d+) 条采样", " ".join(got))
        plo, phi = sorted((len(d0.get("points") or []), len(d1.get("points") or [])))
        slo, shi = sorted((d0.get("samples") or 0, d1.get("samples") or 0))
        bl = d0.get("bucketLabel") or ""
        ok = (st0 == 200 and st1 == 200 and nums and bl and bl == d1.get("bucketLabel")
              and plo <= int(nums[0][0]) <= phi and slo <= int(nums[0][1]) <= shi
              and any(bl in c for c in got))
        detail = "界面 %s ／ 接口两次 %d~%d 点、%d~%d 条采样、格子 %r" % (got, plo, phi, slo, shi, bl)
        return ok, detail, d1

    pick_room("本地界面实测房")
    check("趋势：默认窗口是近 24 小时", on_btn() == "近 24 小时", on_btn())
    ok, detail, d = bracket(24, lambda: b.reload(), fresh_load=True)
    check("趋势：默认窗口那几个数就是接口给的那次回答", ok, detail)
    check("趋势：24 小时窗口给回了点", len(d.get("points") or []) > 1,
          "%d 点" % len(d.get("points") or []))
    check("趋势：画布真的画出来了", b.js("!!document.querySelector('svg.spark')") is True)
    check("趋势：亮着的仍然是近 24 小时", on_btn() == "近 24 小时", on_btn())

    # 说明气泡只在这张卡的范围内找（两张图各有一枚「这一张怎么读」，
    # 全站还有三枚环的说明，写裸 .tip-btn 会点到 DOM 里最先出现的那一枚）。
    _, dd24 = api_trend(24)
    clicked(b, ".ov-trend .tip-btn", "这一张怎么读")
    bub = b.js("""(() => { const x = document.querySelector('.tip-bub'); return x ? x.textContent : ''; })()""")
    check("趋势：说明里写着这份数据实际覆盖到什么时候",
          "采样" in bub and "距今" in bub, bub[:150])
    # 前端以前自己写死"每分钟一个点"，后端一改聚合口径那句就成了假说明。
    # 所以钉的是接口这一次给的格子说法真的出现在气泡里。
    check("趋势：说明里那格就是接口给的格子说法",
          (dd24.get("bucketLabel") or "—") in bub,
          "接口 %r ／ 气泡 %s" % (dd24.get("bucketLabel"), bub[:150]))

    for label, hours in (("近 6 小时", 6), ("近 3 天", 72), ("近 7 天", 168), ("近 24 小时", 24)):
        ok, detail, _ = bracket(hours, lambda lb=label: clicked(b, SEG, lb, settle=0.2))
        check("趋势：切到「%s」后点数与条数跟着接口重算" % label, ok, detail)
    check("趋势：回到近 24 小时仍然是那一枚亮着", on_btn() == "近 24 小时", on_btn())

    a = (api("/api/v1/stats?" + qs(cluster=cur["cluster"], hours=24))[1] or {}).get("data") or {}
    c7 = (api("/api/v1/stats?" + qs(cluster=cur["cluster"], hours=168))[1] or {}).get("data") or {}
    check("趋势：窗口越长格子越大（而不是砍掉一段）",
          c7.get("bucketSeconds", 0) > a.get("bucketSeconds", 0),
          "24h=%s / 168h=%s" % (a.get("bucketSeconds"), c7.get("bucketSeconds")))
    check("趋势：图右端是新鲜的（最新采样就在刚才）",
          a.get("latestAgoMinutes", 999) <= 5, "距今 %s 分钟" % (a.get("latestAgoMinutes")))

    def fresh_badge():
        return b.js("""(() => { const card = document.querySelector('.ov-trend .ov-when').closest('section.card');
          const x = card.querySelector('.card-head .badge'); return x ? x.textContent.trim() : ''; })()""")
    check("趋势：有采样的房间，卡片头上写着最近一条距今多久",
          "分钟前" in fresh_badge(), fresh_badge())

    # 换一个从来没采过样的房间：不许留着上一间的曲线，也不许白屏。
    # 这一间从页签上现取目录名（下面那句空状态判据要对的就是它自己的接口回答）。
    pick_room("探针改名")
    _, dp = api("/api/v1/stats?" + qs(cluster=cur["cluster"], hours=24))
    dd = dp.get("data") or {}
    got = b.js("""(() => { const card = document.querySelector('.ov-trend .ov-when').closest('section.card');
      const t = card.querySelector('.empty .e-title');
      const n = card.querySelector('.empty .e-note');
      return JSON.stringify([t ? t.textContent.trim() : '', n ? n.textContent.trim() : '',
        !!card.querySelector('svg.spark')]); })()""")
    title, note, has_svg = json.loads(got)
    check("趋势：没有采样的房间给空状态而不是上一间的图",
          dd.get("points") == [] and title != "" and has_svg is False, "「%s」svg=%r" % (title, has_svg))
    # #127 用户「没有数据就直接写暂无数据…写这么大一堆，用户也懒得看」：
    # 空态收成一行短标题，接口那句长说明不再铺在正文里，改由卡头那枚「这一张怎么读」带着。
    check("趋势：空态就一行短话，正文里不再摊一段说明",
          len(title) <= 10 and note == "", "「%s」／正文 %r" % (title, note[:40]))
    # 上面那一条已经把这张卡的气泡点开过（headless 里鼠标不挪，open 会一直留着），
    # 所以先读一次，读到就直接用；确实是关着的才点一下。反过来写会点成"关"，读出空串假红。
    read_bub = """(() => { const card = document.querySelector('.ov-trend .ov-when').closest('section.card');
      const x = card.querySelector('.card-head .tip-bub'); return x ? x.textContent : ''; })()"""
    bub0 = (b.js(read_bub) or "").strip()
    if not bub0:
        clicked(b, ".ov-trend .tip-btn", "这一张怎么读")
        bub0 = b.js(read_bub) or ""
    check("趋势：接口那句说明搬进气泡后一个字没少",
          (dd.get("note") or "")[:8] in bub0, "%s ／ 气泡 %s" % (str(dd.get("note"))[:70], bub0[:70]))
    # 一条采样都没有时后端不许报"距今 0 分钟"（那是说"最新一条就在刚才"），
    # 界面这边也就没有那枚新鲜度把手——空图配"就在刚才"是最难被发现的一句假话。
    check("趋势：没采样的房间不许挂着新鲜度把手", fresh_badge() == "", fresh_badge())
    pick_room("本地界面实测房")
    check("趋势：切回来图又画出来了", b.js("!!document.querySelector('svg.spark')") is True)
    take(b, "overview")
    page_errors(b, "总览页")


# ================= 世界地图 =================
def map_page(b):
    open_page(b, "/#/map", "canvas")
    PX = "(() => { const c = document.querySelector('canvas'); return c ? c.toDataURL().length : 0; })()"
    base = b.js(PX)
    check("地图：默认倍率把图画出来了", base > 4000, "dataURL %d 字节" % base)

    # 换房间：屏幕上这张图必须换成那个房间的（或者那个房间没图时，不能再挂着上一间的图）。
    HASH = ("(() => { const c = document.querySelector('canvas');"
            " if (!c) return 'nocanvas'; const u = c.toDataURL();"
            " let h = 0; for (let i = 0; i < u.length; i += 7) h = (h * 31 + u.charCodeAt(i)) >>> 0;"
            " return 'h' + h + ':' + u.length; })()")
    cur = b.js("(() => { const s = document.querySelector('#map-cluster'); return s ? s.value : ''; })()")
    opts = b.js("[...document.querySelectorAll('#map-cluster option')].map((o) => o.value).filter(Boolean)")
    base = b.js(HASH)

    def tiles_of(c, sh):
        st, rp = api("/api/v1/clusters/%s/shards/%s/map" % (urllib.parse.quote(c), urllib.parse.quote(sh)))
        d = rp.get("data") or {}
        if st != 200 or not d.get("ready"):
            return None
        return sum(d.get("tiles") or [])

    shard_now = "Master"
    for c in [o for o in opts if o != cur][:1]:
        pick(b, c)
        time.sleep(2.5)
        after = b.js(HASH)
        here, there = tiles_of(cur, shard_now), tiles_of(c, shard_now)
        if there is None and here != there:
            check("地图：切到没有地图数据的 %s 以后，上一间的图没留在屏上" % c,
                  after != base, "画面哈希 %s → %s" % (base, after))
        elif there is None:
            REPORT.append(("INFO", "地图：%s 没有地图数据，界面走的是加载失败那条分支" % c, ""))
        else:
            check("地图：切到房间 %s 以后图和它的数据对得上" % c,
                  after != base if here != there else after == base,
                  "数据 %s ／ 画面 %s → %s" % ("不同" if here != there else "相同", base, after))
        pick(b, cur)
        time.sleep(2.0)

    # 类别开关（放大到 5 倍以上才出现）：关掉一类，画面必须真的变。
    # 一直点到开关出来为止——每次点多少倍、上限在哪，都由地图自己定，写死次数会因夹具而变。
    for _ in range(14):
        if b.js("document.querySelectorAll('.map-filter .chipbtn').length") > 0:
            break
        if b.js("(() => { const el = document.querySelector('[aria-label=\"放大地图\"]');"
                " if (!el || el.disabled) return 'capped'; el.click(); return 'ok'; })()") == "capped":
            break
        time.sleep(0.5)
    chips = b.js("""[...document.querySelectorAll('.map-filter .chipbtn')].map((x) => x.textContent.trim())""")
    if not chips:
        REPORT.append(("INFO", "地图：这一份夹具在高倍率下没出类别开关（助手没回实体表时就是这样）", ""))
    else:
        # 有些类别在当前视野里本来就一个都没有，关掉它画面不会动——那不算 bug。
        # 所以逐枚试，只要有一枚关掉确实改了画面，就认定开关通到了渲染；一枚都不改才是假的。
        hit = None
        for label in chips[:10]:
            before = b.js(PX)
            clicked(b, ".map-filter .chipbtn", label)
            cls = b.js("""(() => { const x = [...document.querySelectorAll('.map-filter .chipbtn')]
                .find((y) => y.textContent.trim() === %(want)s); return x ? x.className : 'no-chip'; })()"""
                       % {"want": json.dumps(label)})
            if "off" not in cls:
                check("地图：开关「%s」点下去自己写着 off" % label, False, cls)
                continue
            after = b.js(PX)
            if after != before:
                hit = label
                break
            clicked(b, ".map-filter .chipbtn", label)
        check("地图：这一排类别开关里至少一枚真的改了画面（%d 枚都试过）" % min(len(chips), 10),
              hit is not None, "开关=%s" % chips[:10])
        if hit:
            # 刷新以后：开关还写着 off（记住了），而且这一类确实没画出来——
            # 判据不是拿刷新前后的像素比（缩放平移状态不跟着存，一比就假失败），
            # 而是在同一屏把它点开：画面变了，才证明刷新后的那张图本来是筛过的。
            b.reload()
            b.wait("!!document.querySelector('canvas')")
            time.sleep(2.2)
            b.js("""(() => { const el = document.querySelector('[aria-label="放大地图"]');
                  for (let i = 0; i < 12 && !document.querySelector('.map-filter .chipbtn.off'); i++) {
                    if (!el || el.disabled) break; el.click(); } })()""")
            time.sleep(1.2)
            cls2 = b.js("""(() => { const x = [...document.querySelectorAll('.map-filter .chipbtn')]
                .find((y) => y.textContent.trim() === %(want)s); return x ? x.className : 'no-chip'; })()"""
                        % {"want": json.dumps(hit)})
            n = b.js("document.querySelectorAll('.map-filter .chipbtn.off').length")
            check("地图：刷新以后「%s」还写着关掉（off 数 %d）" % (hit, n), "off" in cls2 and n >= 1, cls2)
            px_off = b.js(PX)
            clicked(b, ".map-filter .chipbtn", hit)
            px_on = b.js(PX)
            check("地图：刷新以后把它点开，画面确实多出这一类来", px_on != px_off,
                  "%s → %s 字节" % (px_off, px_on))
            clicked(b, ".map-filter .chipbtn", hit)
            back = b.js(PX)
            check("地图：再关回去又是那张筛过的图", back == px_off,
                  "又是 %s 字节，与筛过那张一致（点开时 %s）" % (back, px_on))
        b.js("localStorage.removeItem('dstpanel.mapfilter')")
    take(b, "map")
    page_errors(b, "地图页")


def main():
    global W
    W = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    b = Browser(W, 900 if W > 560 else 844)
    b.ws.call("Page.addScriptToEvaluateOnNewDocument", {"source": HOOK})
    try:
        for fn in (overview_page, world_page, logs_page, snaps_page,
                   tasks_page, audit_page, map_page):
            try:
                fn(b)
            except Exception as e:
                check(fn.__name__, False, "跑挂了：%r" % (e,))
    finally:
        b.close()

    for st, name, detail in REPORT:
        print("%-5s %s%s" % (st, name, ("  ← " + detail) if detail else ""))
    print("\n视口 %d：%d 项检查，%d 项没通过" % (W, len(REPORT), len(FAILED)))
    for f in FAILED:
        print("  - " + f)
    if FAILED:
        sys.exit(1)


if __name__ == "__main__":
    main()
