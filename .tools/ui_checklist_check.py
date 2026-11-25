# 总览页那张「下一步」清单：它到底有没有照现状说话。
#
# 这一栏是新加的，也是最容易「看着对、其实一直没变」的一块，所以实测四件事：
#   1. 只读账号也看得到它，但行内没有动作按钮（2026-11-09 第二轮）：
#      别页那块页顶提醒整块下掉之后，这里成了全站唯一说待办的地方，
#      安全提醒不挑角色；只读账号的动作那一格换成「需要管理员处理」，
#      不会变成「我以为我能修」；
#   2. 把夹具里的 Klei 令牌挪开 → 点一次「重新核对」，这一行必须自己冒出来；
#   3. 每一行的按钮落到对的页，其中「去填令牌」必须直接落在 Klei 令牌那一栏
#      （地址栏带 tab 参数，这条如果没接上就是整个功能的半成品）；
#   4. 窄屏不横向溢出，按钮换行后仍够得着。
#
# 跑法：python .tools/ui_checklist_check.py [宽度]
# 前提：python .tools/ui_dev.py 已把面板起在 127.0.0.1:8963。
import json
import os
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, ".tools", "tmp", "shots")
TOKEN = os.path.join(ROOT, ".tmp_boot", "klei", "Cluster_1", "cluster_token.txt")
VIEWER, VIEWER_PW = "cl_check_viewer", "Check-pass-1"
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

READ = r"""(() => {
  const card = document.querySelector('.next-steps');
  if (!card) return {present: false};
  const row = card.querySelector('.step-row');
  return {
    present: true,
    head: (card.querySelector('.card-head h3') || {}).textContent,
    chip: (card.querySelector('.card-head .chip') || {}).textContent,
    rows: [...card.querySelectorAll('.step-row:not(.ok)')].map((r) => {
      const t = r.querySelector('.ico-tile');
      const btn = r.querySelector('.btn');
      return {
        tint: t ? [...t.classList].find((c) => c.startsWith('tint-')) : '',
        text: r.querySelector('p').textContent.trim(),
        btn: btn ? btn.textContent.trim() : '',
      };
    }),
    allClear: !!card.querySelector('.step-row.ok'),
    clearText: ((card.querySelector('.step-row.ok p') || {}).textContent || '').trim(),
    err: (card.querySelector('.err-box') || {textContent: ''}).textContent.trim(),
    rowH: row ? Math.round(row.getBoundingClientRect().height) : 0,
  };
})()"""

# 行按钮的落点约定：文案里出现哪个词，就该跳到哪一页（哪一栏）。
EXPECT = [
    ("令牌", "#/room", "Klei 令牌"),
    ("备份", "#/snaps", None),
    ("助手", "#/mods", None),
    ("房间", "#/room", "开服向导"),
    ("没在跑", "#/room", None),
    ("开服向导", "#/room", "开服向导"),
    ("任务", "#/tasks", None),
    # 2026-11-09 起「待处理提醒」并进这一栏：默认密码那行也要验按钮落点，
    # 不能因为它是新加的就让 expect_for 返回 None、整行跳过不点。
    ("密码", "#/accounts", None),
]


def call(method, path, body=None, user=VIEWER, pw=None, admin=False):
    import base64
    auth = ("admin:admin") if admin else ("%s:%s" % (user, pw or VIEWER_PW))
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Authorization", "Basic " + base64.b64encode(auth.encode()).decode())
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    req.data = data
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")[:200]


def login(b, user, pw):
    b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:%s,pass:%s}))"
         % (json.dumps(user), json.dumps(pw)))
    b.reload()
    if not b.wait("!!document.querySelector('.shell')"):
        raise SystemExit("登录层没过去：%s" % user)


def expect_for(row):
    # 落点先看按钮文字，再看整句：按钮上写的才是要去的地方。
    # 整句顺带提到"房间"两个字，不该把「去任务中心下载」判成该跳房间页——
    # 上一轮那条假红就是这么来的（那一行的句子是"服务端本体还没装，房间建好也起不了服"）。
    for text in (row["btn"], row["text"]):
        if not text:
            continue
        for kw, page, tab in EXPECT:
            if kw in text:
                return page, tab
    return None, None


def active_tab(b):
    # 页签按钮里还嵌着一个 tab-count（当前房间名），所以只取第一段文字，
    # 否则断言会拿 "Klei 令牌 Cluster_1" 去比 "Klei 令牌"。
    return b.js("(() => { const on = document.querySelector('.tabs button.on');"
                "if (!on) return '';"
                "const t = Array.from(on.childNodes).find((n) => n.nodeType === 3);"
                "return (t || on).textContent.trim(); })()")


def main():
    w = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    b = Browser(w, 900 if w > 560 else 844)
    moved = False
    acct = None
    try:
        b.goto(BASE + "/#/")
        login(b, "admin", "admin")
        if not b.wait("!!document.querySelector('.next-steps')"):
            raise SystemExit("总览页没有「下一步」这一栏")
        time.sleep(1.5)
        d0 = b.js(READ)
        print("-- 现状 --")
        print(json.dumps(d0, ensure_ascii=False, indent=2))
        assert d0["head"].strip() == "下一步", "标题不对：%r" % d0["head"]
        assert "核对于" in (d0["chip"] or ""), "没说这一轮是什么时候核的：%r" % d0["chip"]
        assert not d0["err"], "一进去就报错：%r" % d0["err"]
        for r in d0["rows"]:
            assert r["tint"], "这一行没有配色块：%r" % r["text"]
            assert len(r["text"]) > 12, "这一行没把现状说清楚：%r" % r["text"]
            assert r["btn"], "这一行没有按钮：%r" % r["text"]
        if d0["allClear"]:
            assert "都齐了" in d0["clearText"], "全绿那句说法变了：%r" % d0["clearText"]
        b.js("document.querySelector('.next-steps').scrollIntoView({block:'center'})")
        time.sleep(0.3)
        b.shoot(os.path.join(OUT, "checklist_%d.png" % w))

        # 待办正文只在总览「下一步」出现一次（2026-11-09 第二轮）：
        # 上一轮还留着"别页页顶一条摘要 + 展开清单"，用户嫌它占首屏又难看，
        # 现在连那块一起下掉，别的页只在顶栏留一枚「待办」入口（不写数字）。
        # 判据改成：正文里哪儿都不许再有 .warnbar / .pending-list，
        # 顶栏那枚计数在、能点、点完带 focus=todo 落回这张卡。
        def pending_blocks():
            return b.js("(() => document.querySelectorAll('.warnbar, .warnbar-slim, .pending-list').length)()")
        has_engine_row = any("服务端本体" in r["text"] for r in d0["rows"])
        assert pending_blocks() == 0, \
            "总览页正文里还挂着待办块（%s 块）" % pending_blocks()
        b.js("location.hash = '#/snaps'")
        # 等的是这一页自己的根节点 .snap-page（见 views/snapshots.js 的模板），
        # 不写 .empty/.card 那种"任何一页都可能有"的泛选择器——上一轮这里红了半天，
        # 就是因为猜成了 .snaps-page，然后又靠泛选择器把"页还没出来"读成了"页出来了但横幅不对"。
        if not b.wait("!!document.querySelector('.snap-page')"):
            raise SystemExit("切到存档备份页没出来")
        time.sleep(0.8)
        got = pending_blocks()
        assert got == 0, "别的页正文里还挂着待办块（%s 块）" % got
        todo = b.js("""(() => { const el = document.querySelector('button.gauge.todo');
            return el ? { text: (el.textContent || '').trim(), title: el.getAttribute('title') || '' } : null; })()""")
        assert todo and "待办" in todo["text"], \
            "顶栏没留下「待办」入口：%r" % todo
        if has_engine_row:
            assert "服务端本体" in todo["title"] or "没有服务端本体" in todo["title"], \
                "计数悬停没把缺本体说出来：%r" % todo["title"]
        b.js("document.querySelector('button.gauge.todo').click()")
        if not b.wait("location.hash.indexOf('focus=todo') >= 0", tries=30):
            raise SystemExit("顶栏「待办」点完没落到总览「下一步」：%s" % b.js("location.hash"))
        print("-- 待办块：总览 0、别页 0；顶栏 %s → %s --" % (todo["text"], b.js("location.hash")))
        b.js("location.hash = '#/'")
        if not b.wait("!!document.querySelector('.next-steps')"):
            raise SystemExit("切回总览页没出来")
        time.sleep(0.8)

        # 顶栏仪表盘点下去都要有去处。第三枚「未结束任务」原来是 span：
        # 旁边两枚能点、它不能，而"是哪几个任务还没结束"只有任务中心答得出。
        #
        # 判法用一条与夹具状态无关的不变量：除了"还没问到房间"那句占位和"当前登录"
        # 那枚身份牌（它不是数字、背后没有"点进去看明细"这回事），
        # 顶栏每一枚带数字的仪表都必须是可以点的 BUTTON。
        # 「助手没回话」带 v-if="gauges.offline"，没有异常世界时本来就不在场，
        # 逐枚硬点名会把夹具状态当成被测对象；而"凡是数字皆可点"这条才是真正要守的东西。
        # 用 JS 点而不是真鼠标：窄屏那档有的仪表带 hide-sm（display:none），
        # 真鼠标点不到，但"有没有入口"和宽度无关。
        listed = b.js("""(() => [...document.querySelectorAll('.gauge')].map((x) => ({
                text: (x.textContent || '').trim().replace(/\\s+/g, ' ').slice(0, 24),
                tag: x.tagName, cls: x.className })))()""") or []
        assert len(listed) >= 3, "顶栏仪表只找到 %s 枚，类名或渲染条件改了" % len(listed)
        still = [x["text"] for x in listed
                 if x["tag"] != "BUTTON" and "还没问到房间" not in x["text"] and "user" not in x["cls"]]
        assert not still, "顶栏有仪表点不动：%s" % still
        print("-- 顶栏仪表 %s 枚，非按钮的（去掉占位和身份牌）：%s --" % (len(listed), still or "无"))

        # #129 把「所有房间」那一页下掉之后，「在跑」的去处从"另一页"变成"本页那一排页签"。
        # 站内点站内是量不出什么的（点了 hash 不变，看着像坏了也看着像没坏），
        # 所以这两枚串起来点：先由「未结束任务」把人带去任务中心，再在别页上点「在跑」，
        # 这时"落回总览"才是一次真的跳转。
        for label, dest in (("未结束任务", "#/tasks"), ("在跑", "#/")):
            # 认文字而不是认顺序：窄屏那档 hide-sm 会把标签藏掉，但 textContent 还在。
            tag = b.js("(() => { const el = [...document.querySelectorAll('button.gauge')]"
                       ".find((x) => (x.textContent || '').indexOf('%s') >= 0);"
                       "if (!el) return 'no-el'; el.click(); return el.tagName; })()" % label)
            time.sleep(0.7)
            got = b.js("location.hash") or ""
            assert tag == "BUTTON" and (got.startswith(dest + "?") or got == dest), \
                "顶栏「%s」该跳 %s，点完是 tag=%s hash=%s" % (label, dest, tag, got)
            print("-- 顶栏「%s」→ %s --" % (label, got))
        b.js("location.hash = '#/'")
        if not b.wait("!!document.querySelector('.next-steps')"):
            raise SystemExit("切回总览页没出来")
        time.sleep(0.6)

        # 把夹具里的令牌挪开，看这一栏会不会自己去查、并把这一条列出来。
        if not os.path.exists(TOKEN):
            raise SystemExit("夹具里没有 %s，这一步没法验「现状变了行就变」" % TOKEN)
        backup = TOKEN + ".held"
        os.rename(TOKEN, backup)
        moved = True
        b.js("(() => { const el = [...document.querySelectorAll('.next-steps .btn')]"
             ".find((x) => x.textContent.indexOf('重新核对') >= 0);"
             "el && el.click(); return !!el; })()")
        if not b.wait("(document.querySelector('.next-steps')||{}).textContent && "
                      "Array.from(document.querySelectorAll('.next-steps .step-row'))"
                      ".some((r) => r.textContent.indexOf('Klei 令牌') >= 0)", tries=40):
            raise SystemExit("令牌挪开了、也点了重新核对，清单里却没有这一条")
        time.sleep(0.4)
        d1 = b.js(READ)
        print("-- 把 Klei 令牌挪开之后 --")
        print(json.dumps(d1, ensure_ascii=False, indent=2))
        row = [r for r in d1["rows"] if "Klei 令牌" in r["text"]]
        assert len(row) == 1, "该只多出一行令牌：%r" % [r["text"] for r in d1["rows"]]
        assert row[0]["tint"] == "tint-gold", "令牌这行的配色不是 gold：%r" % row[0]["tint"]
        assert "搜不到" in row[0]["text"], "没说清后果（列表里搜不到）：%r" % row[0]["text"]
        assert row[0]["btn"] == "去填令牌", "按钮说法变了：%r" % row[0]["btn"]
        assert not d1["allClear"], "还有事没做完却同时显示「都齐了」"
        b.js("document.querySelector('.next-steps').scrollIntoView({block:'center'})")
        time.sleep(0.3)
        b.shoot(os.path.join(OUT, "checklist_missing_%d.png" % w))

        # 点这一条 → 房间页，而且直接落在 Klei 令牌那一栏。
        idx = d1["rows"].index(row[0])
        b.js("(() => { const r = document.querySelectorAll('.next-steps .step-row')[%d];"
             "r.querySelector('.btn').click(); return true; })()" % idx)
        if not b.wait("!!document.querySelector('.tabs')", tries=40):
            raise SystemExit("点了「去填令牌」没落到房间页，hash=%s" % b.js("location.hash"))
        time.sleep(1.5)
        h = b.js("location.hash")
        tab = active_tab(b)
        print("-- 点「去填令牌」之后 --")
        print(json.dumps({"hash": h, "activeTab": tab}, ensure_ascii=False))
        assert "?tab=token" in h, "地址没带页签参数：%s" % h
        assert tab == "Klei 令牌", "落在的不是令牌那一栏，而是 %r" % tab
        b.shoot(os.path.join(OUT, "checklist_deep_link_%d.png" % w))

        # 每一行都得落到对的页（不止令牌那一条）。
        os.replace(backup, TOKEN)
        moved = False
        login(b, "admin", "admin")
        b.js("location.hash = '#/'")
        time.sleep(1.0)
        b.js("(() => { const el = [...document.querySelectorAll('.next-steps .btn')]"
             ".find((x) => x.textContent.indexOf('重新核对') >= 0); el && el.click(); return true; })()")
        time.sleep(1.5)
        d2 = b.js(READ)
        print("-- 令牌放回去之后 --")
        print(json.dumps({"chip": d2["chip"], "rows": [r["text"] for r in d2["rows"]],
                          "allClear": d2["allClear"]}, ensure_ascii=False, indent=2))
        assert not any("Klei 令牌" in r["text"] for r in d2["rows"]), \
            "令牌已经回来了，清单里却还挂着那一行"
        for i, r in enumerate(d2["rows"]):
            page, tab = expect_for(r)
            if not page:
                continue
            b.js("(() => { const r = document.querySelectorAll('.next-steps .step-row')[%d];"
                 "r.querySelector('.btn').click(); })()" % i)
            if not b.wait("location.hash.indexOf('%s') === 0" % page, tries=30):
                raise SystemExit("「%s」没跳到 %s，实际 %s" % (r["btn"], page, b.js("location.hash")))
            time.sleep(1.0)
            if tab:
                got = active_tab(b)
                assert got == tab, "「%s」落在了 %r，不是 %r" % (r["btn"], got, tab)
            b.js("location.hash = '#/'")
            b.wait("!!document.querySelector('.next-steps')", tries=30)
            time.sleep(0.6)

        assert int(b.js("document.documentElement.scrollWidth - innerWidth")) <= 0, "横向溢出了"

        # 只读账号：别页的页顶待办块下掉之后，这一栏是全站唯一说待办的地方，
        # 所以他也得看得见；但行内不能给按钮，动作那一格统一写「需要管理员处理」。
        st, body = call("POST", "/api/v1/accounts",
                        {"username": VIEWER, "password": VIEWER_PW}, admin=True)
        assert st in (200, 201), "建只读账号失败：%s %s" % (st, body)
        st, lst = call("GET", "/api/v1/accounts", admin=True)
        acct = next((a["id"] for a in (lst.get("data") or []) if a["username"] == VIEWER), None)
        assert acct is not None, "账号建出来了但名单里找不到"
        b.goto(BASE + "/#/")
        login(b, VIEWER, VIEWER_PW)
        if not b.wait("!!document.querySelector('.host-strip') || !!document.querySelector('.next-steps')", tries=40):
            raise SystemExit("只读账号登录后总览页没出来")
        time.sleep(2.0)
        d3 = b.js(READ)
        ro = b.js("""(() => { const card = document.querySelector('.next-steps');
            return { btns: card ? card.querySelectorAll('.step-row:not(.ok) .btn').length : -1,
                     tails: card ? [...card.querySelectorAll('.step-row:not(.ok) .step-tail')]
                                    .map((x) => (x.textContent || '').trim()) : [] }; })()""")
        print("-- 只读账号 --")
        print(json.dumps({"present": d3["present"], "rows": [r["text"] for r in d3["rows"]],
                          "action": ro}, ensure_ascii=False))
        assert d3["present"] is True, "只读账号看不到「下一步」，待办就全站不可见了"
        assert ro["btns"] == 0, "只读账号那一栏还挂着动作按钮：%s 枚" % ro["btns"]
        assert ro["tails"] and all("需要管理员处理" in t for t in ro["tails"]), \
            "只读账号的动作格没换成说明：%r" % ro["tails"]
    finally:
        b.close()
        if moved:
            os.replace(TOKEN + ".held", TOKEN)
            print("令牌文件已放回：%s" % TOKEN)
        if acct is not None:
            print("清掉只读账号：", call("DELETE", "/api/v1/accounts/%s" % acct, admin=True)[0])
    print("\n下一步清单 %s 宽度：全部通过" % w)


if __name__ == "__main__":
    main()
