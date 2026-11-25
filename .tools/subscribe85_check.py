# #85 验收：一次「订阅并开启」点下去，界面说的、接口回的、文件里写的是不是同一件事。
#
# 这一条改的是动线本身：以前用户要在三个页签之间来回（找模组 → 订阅 → 开关那一屏再打开），
# 现在一枚按钮写完下载清单 + 这个房间所有世界的开关。所以验收盯六件事：
#   1. 卡片上那枚按钮的文案，能从接口的 registered/usedBy 一格一格推出来（不是界面自己声称）；
#   2. 屏顶那句话说得出的房间和世界，就是这一发真的会写的那几个文件；
#   3. 点完以后：吐司、卡片、待下载那一栏、开关那一屏四处口径一致；
#   4. 落盘的是真文件——dedicated_server_mods_setup.lua 一行，两个世界的 modoverrides.lua 各一条 enabled=true；
#   5. 还没下载的那一行在开关那一屏也认得出名字和封面，不再只剩 workshop-数字（#78 批过的事）；
#   6. 「有条目、磁盘上没目录」的两种来路各说各的话：清单里有的是「文件还没下载」，
#      清单里也没有的是「目录不见了」——后者写成"还没下载"就是给了句假承诺。
#
# 第 6 件事不用另造夹具：上一轮跑完留下的那条（订阅过又取消、开关留在文件里）正好是「目录不见了」，
# 所以点之前先扫一遍开关那一屏，点完再扫一遍，两种徽标各自都被验到。
# 测完把清单和开关都还原，最后要靠 ui_dev 重铺夹具（modoverrides 的条目只能关掉、不能整条删）。
# 宽度从命令行给：390 那一档要单独看有没有横向溢出。
import base64
import io
import json
import os
import re
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
sys.stderr.reconfigure(encoding="utf-8", errors="replace")

AUTH = "Basic " + base64.b64encode(b"admin:admin").decode()
SETUP = os.path.join(ROOT, ".tmp_boot", "dst", "mods", "dedicated_server_mods_setup.lua")
OVERRIDES = os.path.join(ROOT, ".tmp_boot", "klei")


def req(method, path, body=None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method,
                               headers={"Authorization": AUTH, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode("utf-8") or "{}")


def click_js(b, sel, predicate):
    return b.js("(() => { const el = [...document.querySelectorAll(%s)].find(%s);"
                " if (!el) return false; el.click(); return true; })()"
                % (json.dumps(sel), predicate))


# 卡片脚上那枚控件：either 一个 primary 按钮，either 一句陈述（chip）。
# 把 isChip 一起读回来是因为"开齐了就不给按钮"正是这次的设计，光看文字分不出点不点得动。
READ_CARDS = r"""(() => {
  const cards = [...document.querySelectorAll('.mod-cards .mod-card')].map(c => {
    const foot = c.querySelector('.mod-card-foot');
    const btn = foot && (foot.querySelector('button.primary') || foot.querySelector('.chip'));
    return {
      name: ((c.querySelector('.mod-card-name') || {}).innerText || '').trim(),
      control: btn ? btn.innerText.replace(/\s+/g, '').trim() : '',
      isChip: !!(btn && btn.classList.contains('chip')),
      isBtn: !!(btn && btn.tagName === 'BUTTON'),
      tip: btn ? (btn.getAttribute('title') || '') : '',
      badge: ((c.querySelector('.mod-cover .badge') || {}).innerText || '').trim(),
    };
  });
  return {
    cards: cards,
    notes: [...document.querySelectorAll('.note')].map(n => n.innerText.replace(/\s+/g, ' ').trim()),
    room: (document.querySelector('select') || {}).value || '',
    overflow: document.documentElement.scrollWidth > window.innerWidth + 1,
    scrollH: document.documentElement.scrollHeight,
  };
})()"""

# 开关那一屏的行：名字、开关、落在哪一栏、徽标、整行说明、封面有没有真图。
READ_SHARD = r"""(() => {
  const rows = [...document.querySelectorAll('.mod-line')].map(r => ({
    name: ((r.querySelector('.mod-line-name') || {}).innerText || '').trim(),
    on: !!r.querySelector('.switch input:checked'),
    isOn: r.classList.contains('is-on'),
    badges: [...r.querySelectorAll('.badge')].map(x => x.innerText.trim()),
    notes: [...r.querySelectorAll('.shard-note')].map(x => x.innerText.replace(/\s+/g, ' ').trim()),
    sum: ((r.querySelector('.mod-line-sum') || {}).innerText || '').trim(),
    coverImg: !!r.querySelector('.mod-cover img'),
  }));
  return {
    rows: rows,
    shard: ((document.querySelector('.seg button.on') || {}).innerText || '').trim(),
    overflow: document.documentElement.scrollWidth > window.innerWidth + 1,
  };
})()"""


def login(b):
    if not b.js("!!document.querySelector('.login-form')"):
        return True
    b.js("(() => { const f = document.querySelectorAll('.login-form input');"
         " f[0].value='admin'; f[1].value='admin';"
         " f[0].dispatchEvent(new Event('input', {bubbles:true}));"
         " f[1].dispatchEvent(new Event('input', {bubbles:true})); })()")
    b.js("(() => { const f = document.querySelector('.login-form');"
         " if (!f) return false;"
         " const s = f.querySelector('.login-submit') ||"
         " [...f.querySelectorAll('button')].pop(); s.click(); return true; })()")
    return b.wait("!!document.querySelector('.tabs')")


def expected_label(registered, worlds, opened):
    """和 mods.js 里 openLabel() 同一套规则，逐条抄一遍。

    这里是故意重写的、不是 import 的：界面那一份如果哪天改了规则，这一屏要红给人看，
    而不是跟着一起改——两处一致才叫"界面说的=接口回的"。
    """
    if not worlds:
        return ("已订阅" if registered else "订阅", False)
    if len(opened) >= len(worlds):
        return ("已开在这个房间", False)
    if not registered:
        return ("订阅并开启", True)
    if opened:
        return ("补齐 %d 个世界" % (len(worlds) - len(opened)), True)
    return ("在这个房间打开", True)


def open_dirs(view):
    """目录 -> 开着它的世界；磁盘和待下载两批都要算进来（前端 openByDir 同口径）。"""
    out = {}
    for m in view.get("mods") or []:
        if m.get("usedBy"):
            out[m["dir"]] = m["usedBy"]
    for p in view.get("pendingDownload") or []:
        if p.get("usedBy"):
            out["workshop-" + p["workshopId"]] = p["usedBy"]
    return out


def ui_names(view):
    """目录 -> 这一屏应该显示的名字（工坊模组优先用磁盘 modinfo，其次用缓存标题）。"""
    out = {}
    for m in view.get("mods") or []:
        out[m["dir"]] = m.get("name") or m["dir"]
    for p in view.get("pendingDownload") or []:
        d = "workshop-" + p["workshopId"]
        out[d] = p["name"] if p.get("nameKnown") else d
    return out


def setup_ids():
    return set(re.findall(r'ServerModSetup\(\s*"(\d+)"', io.open(SETUP, encoding="utf-8").read()))


def override_has(text, mod_dir, want_enabled):
    """modoverrides.lua 里这个目录的条目在不在、enabled 是不是那个值。

    只看文本不够：面板关一个模组是留着条目把 enabled 写成 false，所以必须按条目切段再取值。
    """
    i = text.find('["%s"]' % mod_dir)
    if i < 0:
        return False
    nxt = text.find('["workshop-', i + 1)
    seg = text[i:nxt if nxt > 0 else len(text)]
    return bool(re.search(r"enabled\s*=\s*" + ("true" if want_enabled else "false"), seg))


def check_missing_dirs(s, room, shard, view, fails):
    """每一行"这个世界有条目、磁盘上却没目录"的徽标口径，逐行对着订阅清单验。"""
    st, body = req("GET", "/api/v1/clusters/%s/shards/%s/mods" % (room, shard))
    ents = (body.get("data") or {}).get("mods") or []
    ids = setup_ids()
    names = ui_names(view)
    seen = 0
    for e in ents:
        if e.get("onDisk"):
            continue
        d = e["dir"]
        if not e.get("enabled"):
            # 关着的这一行不该再挂缺目录那两枚徽标：面板关一个模组是留条目写 enabled = false，
            # 磁盘上没目录不是故障，标红了等于让用户去找一个不存在的问题。
            off_label = names.get(d) or e.get("seenName") or d
            off_row = [x for x in s["rows"] if x["name"] == off_label]
            if off_row and any(("目录不见了" in x or "文件还没下载" in x)
                               for x in off_row[0]["badges"]):
                fails.append("%s 是关着的，行上却挂了缺目录的徽标：%s" % (d, off_row[0]["badges"]))
            continue
        m = re.fullmatch(r"workshop-(\d+)", d)
        in_setup = bool(m) and m.group(1) in ids
        want = "文件还没下载" if in_setup else "目录不见了"
        # 这一行该显示什么名字：待下载那条有工坊标题；清单里也没有的那种残留，
        # 只能靠这一间接口带回来的缓存标题。两个都没有才允许露目录名。
        label = names.get(d) or e.get("seenName") or d
        row = [x for x in s["rows"] if x["name"] == label]
        if not row:
            other = [x for x in s["rows"] if x["name"] == d]
            if other and label != d:
                fails.append("%s 这一行还顶着裸编号，接口明明给了名字「%s」" % (d, label))
            continue
        seen += 1
        if want not in " ".join(row[0]["badges"]):
            fails.append("%s 没有目录、清单里%s，行上该写「%s」，实际徽标是 %s"
                         % (d, "有这一行" if in_setup else "没这一行", want, row[0]["badges"]))
        if in_setup and any("目录不见了" in x for x in row[0]["badges"]):
            fails.append("%s 明明在订阅清单里（重启会拉），却被说成目录不见了" % d)
        if not in_setup and not any("订阅清单里也没有" in x for x in row[0]["notes"]):
            fails.append("%s 的「目录不见了」只留在 hover 里，这一行没有整句说明（手机上看不见 hover）" % d)
    return seen


def shoot_gone_row(b, s, width):
    """把「目录不见了」那一行滚到眼前拍一张：这一种全靠文字说清，看图才看得出够不够读。"""
    gone = [r["name"] for r in s["rows"] if any("目录不见了" in x for x in r["badges"])]
    if not gone:
        return ""
    b.js("(() => { const r = [...document.querySelectorAll('.mod-line')]"
         ".find(x => x.innerText.indexOf(%s) >= 0);"
         " r && r.scrollIntoView({block:'center'}); return !!r; })()" % json.dumps(gone[0]))
    time.sleep(0.3)
    b.shoot(os.path.join(ROOT, ".tools", "tmp", "subscribe85_%d_gone.png" % width))
    return gone[0]


def main():
    width = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    height = int(sys.argv[2]) if len(sys.argv) > 2 else 900
    fails = []

    st, cl = req("GET", "/api/v1/clusters")
    clusters = cl.get("data") or []
    worlds_of = {c["name"]: [s["name"] for s in (c.get("shards") or [])] for c in clusters}

    b = Browser(width, height)
    target_id = None
    try:
        b.goto(BASE + "/?cdp=%d/#/mods" % int(time.time() * 1000))
        if not b.wait("!!document.querySelector('.login-form, .tabs')"):
            print("FAIL 模组页没起来")
            return 1
        if not login(b):
            print("FAIL 登录没进去")
            return 1
        if not b.wait("document.querySelectorAll('.mod-cards .mod-card').length > 0", tries=90):
            print("FAIL 第 1 屏一张卡片都没画出来")
            return 1
        time.sleep(0.5)

        v = b.js(READ_CARDS)
        room = v["room"]
        worlds = worlds_of.get(room) or []
        print("当前房间：%s，世界：%s，卡片 %d 张，横向溢出=%s，页面高度 %d"
              % (room, worlds, len(v["cards"]), v["overflow"], v["scrollH"]))
        if v["overflow"]:
            fails.append("这个宽度下第 1 屏横向溢出了")
        if not room or not worlds:
            fails.append("读不出界面现在停在哪一间（或这一间没有世界），后面的比对全没了基准")
            return 1

        st, sv = req("GET", "/api/v1/mods")
        sview = sv.get("data") or {}
        by_dir = open_dirs(sview)

        # ---- 0. 点之前先扫一遍开关那一屏：上一轮留下的"目录不见了"那种要被验到 ----
        if not click_js(b, ".tabs button", "t => t.innerText.indexOf('开关') >= 0"):
            fails.append("找不到「这个房间的开关」那一屏的页签")
            return 1
        if not b.wait("document.querySelectorAll('.mod-line').length > 0", tries=60):
            fails.append("开关那一屏一行都没画出来")
            return 1
        s0 = b.js(READ_SHARD)
        n0 = check_missing_dirs(s0, room, s0["shard"] or worlds[0], sview, fails)
        g0 = shoot_gone_row(b, s0, width)
        print("点之前：开关那一屏缺目录的行 %d 条（这一间是 %s），已按订阅清单逐行核对徽标；%s"
              % (n0, s0["shard"] or worlds[0],
                 "其中「目录不见了」是 %s" % g0 if g0 else "这一屏这会儿没有「目录不见了」的行，那一种没验到"))
        if not click_js(b, ".tabs button", "t => t.innerText.indexOf('找模组') >= 0"):
            fails.append("找不到「找模组」那一屏的页签")
            return 1
        if not b.wait("document.querySelectorAll('.mod-cards .mod-card').length > 0", tries=60):
            fails.append("回到第 1 屏以后卡片没重画出来")
            return 1

        # 屏顶那句必须点名房间和每一个世界：它是这一发请求唯一的说明书。
        note = " ".join(b.js(READ_CARDS)["notes"])
        if room not in note:
            fails.append("屏顶那句话没点名当前房间 %s：%s" % (room, note[:120]))
        for w in worlds:
            if w not in note:
                fails.append("屏顶那句话漏了世界 %s（点下去要写它的文件）：%s" % (w, note[:120]))
        if "订阅并开启" not in note:
            fails.append("屏顶那句话没说出这枚按钮叫什么：%s" % note[:120])
        print("屏顶：%s" % note[note.index("卡片上"):] if "卡片上" in note else "屏顶：%s" % note[:180])

        # ---- 1. 逐格比对按钮文案 vs 接口 ----
        # 在线那一路通不通是这台机器的链路决定的（api.steampowered.com 时常连不上，
        # 那时候屏顶换成"下面是本地缓存里对得上的老结果"），所以第一页有可能整页都
        # 已经订阅过了。这不算产品的错，也不算"这一支没验到"——往前翻几页总能找到
        # 一枚还没订过的卡；翻到头还没有才算红，并且要把原因写清楚。
        picked = None
        pg = 1
        compared = 0
        live = None
        while True:
            v = b.js(READ_CARDS)
            st, ss = req("GET", "/api/v1/mods/search?size=12&page=%d" % pg)
            sdata = ss.get("data") or {}
            items = sdata.get("items") or []
            if live is None:
                live = bool(sdata.get("live"))
            compared += min(len(items), len(v["cards"]))
            if len(items) != len(v["cards"]):
                fails.append("接口第 %d 页 %d 条，界面画了 %d 张卡，对不上"
                             % (pg, len(items), len(v["cards"])))
            for i, (it, card) in enumerate(zip(items, v["cards"])):
                want_name = it.get("name") or it["workshopId"]
                if card["name"] != want_name:
                    fails.append("第 %d 页第 %d 张卡界面写「%s」，接口那条的名字是「%s」"
                                 % (pg, i + 1, card["name"], want_name))
                    continue
                opened = [w for w in worlds
                          if ("%s / %s" % (room, w)) in (by_dir.get("workshop-" + it["workshopId"]) or [])]
                text, can = expected_label(bool(it.get("registered")), worlds, opened)
                if card["control"] != text:
                    fails.append("「%s」按钮上写的是「%s」，按接口的订阅状态(%s)和开着它的世界(%s)应该是「%s」"
                                 % (want_name, card["control"], it.get("registered"), opened, text))
                if card["isChip"] == can:
                    fails.append("「%s」该是%s，画成了%s"
                                 % (want_name, "按钮" if can else "陈述",
                                    "按钮" if card["isBtn"] else "陈述"))
                if can and not picked and it.get("name") and not it.get("downloaded"):
                    picked = it
            if picked or pg >= 5:
                break
            if not click_js(b, ".pager .pg-btn", "x => x.innerText.indexOf('下一页') >= 0 && !x.disabled"):
                break
            if not b.wait("document.querySelectorAll('.mod-cards .mod-card').length > 0", tries=60):
                fails.append("翻到第 %d 页以后卡片没画出来" % (pg + 1))
                break
            pg += 1
            time.sleep(0.8)
        print("按钮文案逐张比对 %d 张，停在第 %d 页（接口 registered/usedBy 推出来的；这一轮%s）"
              % (compared, pg, "走的是工坊在线" if live else "在线没通、走的是本地缓存"))
        if not picked:
            fails.append("翻了 %d 页都没有一张「订阅并开启」的卡，后面那次点击验不了——"
                         "夹具或缓存变了？" % pg)
            return 1
        it = picked
        target_id = it["workshopId"]
        print("拿这张验：%s（工坊 ID %s）" % (it["name"], target_id))
        b.shoot(os.path.join(ROOT, ".tools", "tmp", "subscribe85_%d_tab1_before.png" % width))

        # ---- 2. 点一次 ----
        if not click_js(b, ".mod-cards .mod-card .mod-card-foot button.primary",
                        "x => ((x.closest('.mod-card').querySelector('.mod-card-name') || {}).innerText || '')"
                        ".trim() === %s" % json.dumps(it["name"])):
            print("FAIL 找不到那张卡的「订阅并开启」按钮")
            return 1
        if not b.wait("!!document.querySelector('.toast')", tries=80, pause=0.25):
            print("FAIL 点完没有任何回话（吐司一条都没有）")
            return 1
        toasts = b.js("[...document.querySelectorAll('.toast')]"
                      ".map(t => t.innerText.replace(/\\s+/g,' ').trim())")
        print("吐司：%s" % json.dumps(toasts, ensure_ascii=False))
        joined = " ".join(toasts or [])
        if "订阅" not in joined:
            fails.append("点完的回话里没提订阅：%s" % joined[:120])
        if "重启" not in joined:
            fails.append("回话必须说清文件要等重启才下来（这是这一次点击唯一没做完的事）：%s" % joined[:160])
        for w in worlds:
            if w not in joined:
                fails.append("回话里没点到世界 %s：%s" % (w, joined[:160]))

        # ---- 3. 三处口径：卡片 / 接口 / 真文件 ----
        if not b.wait("document.querySelectorAll('.mod-cards .mod-card').length > 0", tries=90):
            fails.append("点完列表没重画出来")
        time.sleep(0.6)
        v2 = b.js(READ_CARDS)
        after = [c for c in v2["cards"] if c["name"] == it["name"]]
        if not after:
            fails.append("点完这张卡从第 1 屏消失了（%s）" % it["name"])
        else:
            c2 = after[0]
            if c2["control"] != "已开在这个房间" or not c2["isChip"]:
                fails.append("点完这张卡上写的还是「%s」，应该变成陈述「已开在这个房间」" % c2["control"])
            if c2["badge"] != "等重启下载":
                fails.append("点完封面角标该说「等重启下载」，实际是「%s」" % c2["badge"])
            if "重启" not in c2["tip"]:
                fails.append("按钮悬停那句得说清文件什么时候才下来，实际是：%s" % c2["tip"][:120])

        st, sv2 = req("GET", "/api/v1/mods")
        sview2 = sv2.get("data") or {}
        pend = [p for p in (sview2.get("pendingDownload") or []) if p["workshopId"] == target_id]
        if not pend:
            fails.append("接口里找不到工坊 ID %s 的待下载条目（订阅那一步没写进清单？）" % target_id)
        else:
            used = pend[0].get("usedBy") or []
            want = sorted("%s / %s" % (room, w) for w in worlds)
            if sorted(used) != want:
                fails.append("接口说 %s 开着它的是 %s，这一发要写的是全部世界 %s"
                             % (target_id, sorted(used), want))
            if not pend[0].get("nameKnown"):
                fails.append("这条明明是从第 1 屏的检索结果点的（缓存里就有它），接口却说名字没读到")

        setup_text = io.open(SETUP, encoding="utf-8").read()
        n_line = len(re.findall(r'ServerModSetup\(\s*"%s"\s*\)' % re.escape(target_id), setup_text))
        if n_line != 1:
            fails.append("订阅清单里 %s 这一行有 %d 处（应该正好 1 处）" % (target_id, n_line))
        for w in worlds:
            f = os.path.join(OVERRIDES, room, w, "modoverrides.lua")
            if not os.path.exists(f):
                fails.append("%s 的 modoverrides.lua 不存在，开关根本没写" % w)
                continue
            if not override_has(io.open(f, encoding="utf-8").read(), "workshop-" + target_id, True):
                fails.append("%s / %s 里没有 workshop-%s 的 enabled=true 条目" % (room, w, target_id))
        print("落盘：清单 1 行，%s 各 1 条 enabled=true" % "、".join(worlds))

        # ---- 4. 第 2 屏：待下载那一栏的按钮口径 ----
        if not click_js(b, ".tabs button", "t => t.innerText.indexOf('这台机器') >= 0"):
            fails.append("找不到「这台机器上的文件」那一屏的页签")
        else:
            if not b.wait("document.querySelectorAll('.mod-cards .mod-card').length > 0", tries=60):
                fails.append("第 2 屏一张卡都没画出来")
            else:
                d = b.js(READ_CARDS)
                hit = [c for c in d["cards"] if c["name"] == it["name"]]
                if not hit:
                    fails.append("刚点完的 %s 没出现在「已订阅、等重启下载」那一栏" % it["name"])
                elif hit[0]["control"] != "已开在这个房间":
                    fails.append("第 2 屏那张卡上写的还是「%s」，和第 1 屏不是同一句话" % hit[0]["control"])

        # ---- 5. 第 3 屏：待下载那一行也认得出名字，不再是裸 workshop-数字 ----
        if not click_js(b, ".tabs button", "t => t.innerText.indexOf('开关') >= 0"):
            fails.append("找不到「这个房间的开关」那一屏的页签")
        else:
            if not b.wait("document.querySelectorAll('.mod-line').length > 0", tries=60):
                fails.append("第 3 屏一行都没画出来")
            else:
                time.sleep(0.4)
                s = b.js(READ_SHARD)
                if s["overflow"]:
                    fails.append("第 3 屏这个宽度下横向溢出了")
                row = [r for r in s["rows"] if r["name"] == it["name"]]
                if not row:
                    ids = [r["name"] for r in s["rows"] if r["name"].startswith("workshop-")]
                    fails.append("第 3 屏找不到「%s」这一行；还画着裸编号的有 %s"
                                 % (it["name"], json.dumps(ids, ensure_ascii=False)))
                else:
                    r0 = row[0]
                    if not r0["on"]:
                        fails.append("第 3 屏这一行开关没勾上（前两屏都说开在这个房间了）")
                    if not r0["isOn"]:
                        fails.append("第 3 屏这一行没落进「开着」那一栏")
                    if not any("文件还没下载" in x for x in r0["badges"]):
                        fails.append("第 3 屏这一行缺「文件还没下载」那枚徽标：%s" % r0["badges"])
                    if not r0["coverImg"]:
                        fails.append("第 3 屏这一行没有封面（缓存里明明有预览图，认脸全靠它）")
                    print("第 3 屏：%s 开着=%s 徽标=%s 说明=%s"
                          % (r0["name"], r0["on"], r0["badges"], r0["sum"]))
                n1 = check_missing_dirs(s, room, s["shard"] or worlds[0], sview2, fails)
                print("点之后：开关那一屏缺目录的行 %d 条，两种徽标都对着订阅清单核过" % n1)
                # 面板没见过标题的那几条（作者把名字写成表达式）才会留 workshop-数字，
                # 那是无从得知，不是把编号当名字——打印出来给人看一眼，不算失败。
                print("仍是裸编号的行：%s"
                      % json.dumps([r["name"] for r in s["rows"]
                                    if re.fullmatch(r"workshop-\d+", r["name"])], ensure_ascii=False))
        b.shoot(os.path.join(ROOT, ".tools", "tmp", "subscribe85_%d_tab3.png" % width))

        # ---- 6. 残留那一种：取消订阅、但这个世界的开关还开着 ----
        # 「取消订阅」只摘下载清单那一行，不会替用户关世界里的开关（那是 #77 定的口径）。
        # 剩下的这一行开着、磁盘上没目录、清单里也没这一行：重启永远不会把它拉回来，
        # 所以徽标必须从「文件还没下载」换成「目录不见了」，那句整行说明也要出现
        #（手机上没有 hover，只挂在 title 上等于没说）。
        if target_id:
            req("DELETE", "/api/v1/mods/setup/%s" % target_id)
            # 整页重进，等价于用户在界面上点「取消订阅」之后那一次 reload：
            # 只切页签不会重取页面数据（组件只在挂载时读一次），
            # 拿陈旧视图去判徽标会得出一个假 FAIL。
            b.goto(BASE + "/?cdp=%d/#/mods" % int(time.time() * 1000))
            b.wait("!!document.querySelector('.login-form, .tabs')", tries=60)
            login(b)
            if not b.wait("document.querySelectorAll('.mod-cards .mod-card').length > 0", tries=90):
                fails.append("重进面板以后第 1 屏没画出来，第 6 步没做成")
            elif not click_js(b, ".tabs button", "t => t.innerText.indexOf('开关') >= 0"):
                fails.append("切不到「这个房间的开关」那一屏")
            elif not b.wait("document.querySelectorAll('.mod-line').length > 0", tries=60):
                fails.append("取消订阅以后开关那一屏空了")
            else:
                time.sleep(0.4)
                s3 = b.js(READ_SHARD)
                st, sv4 = req("GET", "/api/v1/mods")
                row = [r for r in s3["rows"] if r["name"] == it["name"]]
                if not row:
                    fails.append("取消订阅以后这一行整个不见了（开关还开着，行应当留着）")
                else:
                    r3 = row[0]
                    if not r3["on"] or not r3["isOn"]:
                        fails.append("取消订阅不该动世界里的开关，这一行现在开着=%s 落在开着那栏=%s"
                                     % (r3["on"], r3["isOn"]))
                    if not any("目录不见了" in x for x in r3["badges"]):
                        fails.append("清单里已经没这一行了，徽标还写着 %s，等于承诺重启会拉它"
                                     % r3["badges"])
                    if any("文件还没下载" in x for x in r3["badges"]):
                        fails.append("这一行已经不在订阅清单里，还写「文件还没下载」是假承诺")
                    if not any("订阅清单里也没有" in x for x in r3["notes"]):
                        fails.append("「目录不见了」没有整行说明，手机上看不见 hover 里那句")
                    print("取消订阅以后：%s 开着=%s 徽标=%s" % (r3["name"], r3["on"], r3["badges"]))
                n3 = check_missing_dirs(s3, room, s3["shard"] or worlds[0],
                                        (sv4.get("data") or {}), fails)
                g3 = shoot_gone_row(b, s3, width)
                print("残留那一种：缺目录且清单里没这一行的开着条目 %d 条，界面写成「%s」"
                      % (n3, g3 or "这一屏这会儿没有"))
    finally:
        # 还原：先关开关（清单里还有它，所以放行），再取消订阅。
        if target_id:
            for w in worlds:
                req("PUT", "/api/v1/clusters/%s/shards/%s/mods/workshop-%s" % (room, w, target_id),
                    {"enabled": False})
            req("DELETE", "/api/v1/mods/setup/%s" % target_id)
            st, sv3 = req("GET", "/api/v1/mods")
            left = [p for p in ((sv3.get("data") or {}).get("pendingDownload") or [])
                    if p["workshopId"] == target_id]
            if left:
                print("注意：%s 没从清单里摘掉，夹具要靠 ui_dev 重铺" % target_id)
        b.close()
        # 逐条红必须在这里打印，不能等 try 走完。上一轮这一支在 1440 那一档退出码 1、
        # 日志里却一个字的原因都没有：中间那些 `fails.append(...)；return 1` 是**从 try 里
        # 直接返回**的，写在 try 后面的打印段一辈子轮不到。少了现场，一条红只能靠猜。
        for f in fails:
            print("FAIL " + f)
        if fails:
            print("合计 %d 条红" % len(fails))
        else:
            print("PASS 一次点击写完清单和两个世界的开关，三屏口径一致，"
                  "徽标两种来路都对得上订阅清单，界面文案能从接口推出来")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
