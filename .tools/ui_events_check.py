# 玩家事件表的一次真点：每个筛选控件各按一遍、翻页、开列设置、跳原文、手机上换成卡片。
#
# 为什么值得单独跑：这张表的全部价值在于"数字和列是真的"。接口层测的是聚合本身
# （internal/gamelog/events_test.go），这一支测的是界面把它说成了什么——
# 最容易犯的四种错都"看着对"：
#   - 底部那句"第 1~20 条 / 共 312 条"其实只是这一页下下来的行数；
#   - 筛完停在第 4 页的空列表，看着像"这个人什么都没干过"；
#   - 「看原文」跳过去筛出空列表（假链接）；
#   - 码值（player_death 这种）漏到界面上。
# 最后一条在这里只查"面板自己写的字"：说明那一列是引擎原文，本来就该是英文。
#
# 跑法：python .tools/ui_events_check.py [宽度]
# 前提：python .tools/ui_dev.py 已经把面板起在 127.0.0.1:8963。
import json
import os
import re
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, ".tools", "tmp", "shots", "events")
# 这一支只认 ui_dev.py 造的那个房间（探针建的其他房间没有日志文件）。
ROOM = "Cluster_1"
# 「拉黑」会往这份名单里追加一行，验完必须原样写回去（它是夹具，不是被测数据）。
BLOCKLIST = os.path.join(ROOT, ".tmp_boot", "klei", ROOM, "blocklist.txt")
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HOOK = ("window.__perr=[];window.addEventListener('error',function(e){"
        "if(window.__perr.length<20)window.__perr.push(String(e.message))});"
        "window.addEventListener('unhandledrejection',function(e){"
        "if(window.__perr.length<20)window.__perr.push('promise:'+String(e.reason&&e.reason.message||e.reason))});")

FAILED = []
REPORT = []

READ = r"""(() => {
  const txt = (el) => (el ? (el.textContent || '').trim() : '');
  const norm = (el) => (el ? (el.textContent || '').replace(/\s+/g, ' ').trim() : '');
  const vis = (el) => !!el && el.offsetParent !== null;
  const segText = (sel) => [...document.querySelectorAll(sel)].map((s) =>
    [...s.querySelectorAll('button')].map((b) => b.textContent.trim() + (b.classList.contains('on') ? '[开]' : '')));
  const wrap = document.querySelector('.table-wrap');
  const table = document.querySelector('table.ev-table');
  const cardBox = document.querySelector('.ev-cards');
  const useTable = vis(table) && vis(wrap);
  const useCards = !useTable && vis(cardBox);
  const heads = table ? [...table.querySelectorAll('thead th')].map((x) => txt(x)) : [];
  const at = (name) => heads.indexOf(name);
  const items = [];
  if (useTable) {
    [...table.querySelectorAll('tbody tr')].forEach((tr) => {
      const tds = [...tr.querySelectorAll('td')];
      const chip = tr.querySelector('td .chip');
      const ban = [...tr.querySelectorAll('button')].find((x) => txt(x) === '拉黑');
      items.push({
        who: txt(tr.querySelector('td b')), ava: txt(tr.querySelector('.player-avatar')),
        time: txt(tds[at('时间')]), action: txt(chip),
        tone: chip ? ([...chip.classList].filter((c) => c !== 'chip')[0] || '') : '',
        raw: txt(tr.querySelector('.ev-raw')),
        cells: tds.map((x) => norm(x)), all: norm(tr),
        ban: ban ? (ban.disabled ? 'off' : 'on') : 'none',
      });
    });
  } else if (useCards) {
    [...cardBox.querySelectorAll('.ev-card')].forEach((c) => {
      const chip = c.querySelector('.row .chip');
      const ban = [...c.querySelectorAll('button')].find((x) => txt(x) === '拉黑');
      items.push({
        who: txt(c.querySelector('b.ellipsis')), ava: txt(c.querySelector('.player-avatar')),
        time: txt(c.querySelector('.mono.dim')), action: txt(chip),
        tone: chip ? ([...chip.classList].filter((x) => x !== 'chip')[0] || '') : '',
        raw: txt(c.querySelector('.ev-raw')),
        cells: [], all: norm(c),
        ban: ban ? (ban.disabled ? 'off' : 'on') : 'none',
      });
    });
  }
  const sw = document.querySelector('.log-controls label.switch input');
  const foot = document.querySelector('.ev-foot');
  return {
    vw: innerWidth, overflow: document.documentElement.scrollWidth - innerWidth,
    viewSegs: segText('.log-controls .seg')[0] || [],
    kindSegs: segText('.log-controls .seg')[1] || [],
    filtersShown: !!document.querySelector('details.group[open] .grid-form'),
    followShown: vis(document.querySelector('.log-controls label.switch')),
    followOn: sw ? !!sw.checked : null,
    useTable: useTable, useCards: useCards, heads: heads, items: items,
    foot: txt(foot),
    pager: [...document.querySelectorAll('.pg-num')].map((x) => x.textContent.trim() + (x.classList.contains('on') ? '[开]' : '')),
    pgBtns: [...document.querySelectorAll('.pg-btn')].map((x) => x.textContent.trim() + (x.disabled ? '[关]' : '')),
    head: txt(document.querySelector('.card-head')),
    note: txt(document.querySelector('.card .note')),
    meta: txt(document.querySelector('.stream-meta')),
    logLines: document.querySelectorAll('.logline').length,
    popShown: !!document.querySelector('.pop'),
    popItems: [...document.querySelectorAll('.pop .pop-i')].map((x) =>
      x.textContent.trim() + (x.classList.contains('on') ? '[开]' : '') + (x.disabled ? '[锁]' : '')),
    empty: txt(document.querySelector('.empty .e-title')),
    emptyAll: norm(document.querySelector('.empty')),
    emptyBtns: [...document.querySelectorAll('.empty button, .empty a')].map((x) => x.textContent.trim()),
    cardText: txt(document.querySelector('.card')),
    errBox: txt(document.querySelector('.err-box')),
  };
})()"""


def check(name, ok, detail=""):
    REPORT.append(("PASS" if ok else "FAIL", name, detail))
    print("%s %s%s" % ("PASS" if ok else "FAIL", name,
                       ("  ← " + str(detail).replace("\n", " ")[:220]) if detail else ""))
    if not ok:
        FAILED.append(name + "｜" + str(detail)[:160])


def skip(name, why):
    """这一档场景在当前夹具里不存在（比如表里没有超长行），不算过也不算红。"""
    REPORT.append(("SKIP", name, why))
    print("SKIP %s  ← %s" % (name, str(why)[:160]))


def fixture_lines(shard):
    """夹具里这两份文件各有多少"算数的行"——用来核对界面那句"从 N 行里定性出来的"。
    空行不算（面板的解析器把它们归进 blanks），数字从盘上取：夹具一改行数，脚本跟着改。"""
    n = 0
    for name in ("server_log.txt", "server_chat_log.txt"):
        p = os.path.join(ROOT, ".tmp_boot", "klei", ROOM, shard, name)
        if os.path.exists(p):
            with open(p, "r", encoding="utf-8", errors="replace") as fh:
                n += sum(1 for line in fh if line.strip())
    return n


def has_cjk(s):
    return re.search(r"[一-鿿]", s or "") is not None


def click_seg(b, text):
    return b.js("""(() => {
      const el = [...document.querySelectorAll('.log-controls .seg button')]
        .find((x) => x.textContent.trim() === %s);
      if (!el) return false;
      el.click(); return true;
    })()""" % json.dumps(text))


def click_btn(b, text, scope=""):
    """按文字找一枚按钮并点它。scope 收窄到某个容器里找，避免点到导航上同名的东西。"""
    return b.js("""(() => {
      const root = %s ? document.querySelector(%s) : document;
      if (!root) return false;
      const el = [...root.querySelectorAll('button')].find((x) => x.textContent.trim() === %s);
      if (!el) return false;
      el.click(); return true;
    })()""" % (json.dumps(scope), json.dumps(scope), json.dumps(text)))


def click_live_btn(b, text, scope):
    """点第一枚没被禁用的同名按钮。禁用的那些（比如没有账户编号的那行拉黑）按下去什么都不发生，
    脚本要是先撞上它，就会报成界面坏了。"""
    return b.js("""(() => {
      const root = document.querySelector(%s);
      if (!root) return false;
      const el = [...root.querySelectorAll('button')].find(
        (x) => x.textContent.trim() === %s && !x.disabled);
      if (!el) return false;
      el.click(); return true;
    })()""" % (json.dumps(scope), json.dumps(text)))


def pick_room(b, name):
    """把界面锁到夹具那个房间：这台机器上还留着探针建的其他房间，它们没有日志文件，
    落在那些房间上只会看到空状态，测的就不是这张表了。"""
    return b.js("""(() => {
      const s = document.querySelector('.log-controls .field select');
      if (!s) return false;
      const o = [...s.options].find((x) => x.value === %s || x.textContent.trim() === %s);
      if (!o) return false;
      if (s.value === o.value) return true;
      s.value = o.value;
      s.dispatchEvent(new Event('change', { bubbles: true }));
      return true;
    })()""" % (json.dumps(name), json.dumps(name)))


def open_filters(b):
    """手机上筛选默认折着：折着的 details 里的控件在 DOM 里但不显示，点不动。"""
    if b.js("!!document.querySelector('details.group[open] .grid-form')"):
        return True
    if not b.js("(() => { const s = document.querySelector('details.group summary');"
                "if (!s) return false; s.click(); return true; })()"):
        return False
    return b.wait("!!document.querySelector('details.group[open] .grid-form')")


def field_value(b, label):
    """读某一栏筛选当前的值。"""
    return b.js("""(() => {
      const f = [...document.querySelectorAll('.grid-form .field')]
        .find((x) => ((x.querySelector('label') || {}).textContent || '').trim() === %s);
      if (!f) return null;
      const i = f.querySelector('input:not([type=checkbox])'), s = f.querySelector('select');
      if (i) return { kind: 'input', value: i.value };
      if (s) return { kind: 'select', value: s.value, text: s.options[s.selectedIndex].textContent.trim(),
                      options: [...s.options].map((o) => o.textContent.trim()) };
      const sw = f.querySelector('input[type=checkbox]');
      if (sw) return { kind: 'switch', value: sw.checked ? '1' : '' };
      return { kind: 'none', value: '' };
    })()""" % json.dumps(label))


def type_into(b, label, value):
    return b.js("""(() => {
      const f = [...document.querySelectorAll('.grid-form .field')]
        .find((x) => ((x.querySelector('label') || {}).textContent || '').trim() === %s);
      const el = f && (f.querySelector('input[type=search]') || f.querySelector('input'));
      if (!el) return false;
      el.value = %s;
      el.dispatchEvent(new Event('input', { bubbles: true }));
      return true;
    })()""" % (json.dumps(label), json.dumps(value)))


def pick_option(b, label, needle):
    """在某一栏的 select 里按可见文字选一项（选项文字是中文，值才是码）。"""
    return b.js("""(() => {
      const f = [...document.querySelectorAll('.grid-form .field')]
        .find((x) => ((x.querySelector('label') || {}).textContent || '').trim() === %s);
      const el = f && f.querySelector('select');
      if (!el) return false;
      const o = [...el.options].find((x) => x.textContent.trim().indexOf(%s) >= 0);
      if (!o) return false;
      el.value = o.value;
      el.dispatchEvent(new Event('change', { bubbles: true }));
      return true;
    })()""" % (json.dumps(label), json.dumps(needle)))


def toggle_switch(b, label):
    return b.js("""(() => {
      const f = [...document.querySelectorAll('.grid-form .field')]
        .find((x) => ((x.querySelector('label') || {}).textContent || '').trim() === %s);
      const el = f && f.querySelector('input[type=checkbox]');
      if (!el) return false;
      el.click(); return true;
    })()""" % json.dumps(label))


def open_hint(b, label):
    """把某一枚按需说明点开再读全文：气泡是 v-if，没点开时它根本不在 DOM 里。"""
    if not b.js("""(() => {
      const el = [...document.querySelectorAll('.tip-btn')].find((x) => x.getAttribute('aria-label') === %s);
      if (!el) return false;
      if (el.closest('.tip').classList.contains('open')) return true;
      el.click(); return true;
    })()""" % json.dumps(label)):
        return ""
    if not b.wait("!!document.querySelector('.tip-bub')"):
        return ""
    t = b.js("(() => { const x = document.querySelector('.tip-bub');"
             "return x ? x.textContent.replace(/\\s+/g, ' ').trim() : ''; })()")
    b.js("""(() => { const el = [...document.querySelectorAll('.tip-btn')].find((x) => x.getAttribute('aria-label') === %s);
      if (el) el.click(); })()""" % json.dumps(label))
    return t


def field_text(b, label, sel):
    """某一栏里某个子元素的文字（用来读选中项下面展开的那句整话）。"""
    return b.js("""(() => {
      const f = [...document.querySelectorAll('.grid-form .field')]
        .find((x) => ((x.querySelector('label') || {}).textContent || '').trim() === %s);
      return f ? ((f.querySelector(%s) || {}).textContent || '').trim() : '';
    })()""" % (json.dumps(label), json.dumps(sel)))


def col(b, header):
    """某一列在可见那几行里的文字（表格里按表头找列，列顺序变了也不用改脚本）。"""
    return b.js(r"""(() => {
      const t = document.querySelector('table.ev-table');
      if (!t) return null;
      const ths = [...t.querySelectorAll('thead th')].map((x) => x.textContent.trim());
      const i = ths.indexOf(%s);
      if (i < 0) return null;
      return [...t.querySelectorAll('tbody tr')].map((tr) => {
        const td = tr.querySelectorAll('td')[i];
        return td ? td.textContent.replace(/\s+/g, ' ').trim() : '';
      });
    })()""" % json.dumps(header))


def total_of(foot):
    m = re.search(r"共 ([\d,]+) 条", foot or "")
    return int(m.group(1).replace(",", "")) if m else -1


def scanned_of(foot):
    m = re.search(r"从 ([\d,]+) 行里定性出来的", foot or "")
    return int(m.group(1).replace(",", "")) if m else -1


def main():
    w = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    desk = w > 560
    # 行所在的容器：宽屏是表，手机是卡片。后面每一处"点这一行的某枚按钮"都用它。
    scope = "table.ev-table" if desk else ".ev-cards"
    os.makedirs(OUT, exist_ok=True)
    b = Browser(w, 900 if desk else 844)
    try:
        # 顺序必须是"先落到面板这个源上，再写凭据"：这个页签是共享的，
        # 停在 about:blank 或别的源上写 sessionStorage 等于没写。
        b.goto(BASE + "/#/")
        if not b.wait("location.origin === %s" % json.dumps(BASE)):
            raise SystemExit("没落到面板这一页")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("登录层没过去")
        b.js(HOOK)
        b.js("location.hash = '#/logs'")
        if not b.wait("!!document.querySelector('.log-controls')"):
            raise SystemExit("日志页没出来")
        if not pick_room(b, ROOM):
            raise SystemExit("房间下拉里没有夹具那个房间（%s）" % ROOM)
        if not b.wait("!!document.querySelector('.card-head')"):
            raise SystemExit("事件表这一屏没出来")
        if not b.wait("(document.querySelector('.card-head')||{}).offsetParent !== null"):
            raise SystemExit("事件表那一卡没显示出来（可能落在加载态或错误框上）")
        time.sleep(1.2)
        if not open_filters(b):
            raise SystemExit("筛选面板展不开")
        time.sleep(0.4)

        v = b.js(READ)
        check("默认落在玩家事件表（两枚看法里开的是它）",
              v["viewSegs"] == ["玩家事件[开]", "逐行日志"], v["viewSegs"])
        check("事件表这一屏没有报错框", not v["errBox"], v["errBox"])
        if desk:
            check("宽屏上出的是表格，不是卡片", v["useTable"] and not v["useCards"],
                  "表格 %s 卡片 %s" % (v["useTable"], v["useCards"]))
            check("九列都在，且列名是中文",
                  v["heads"] == ["玩家", "角色", "账户编号", "Steam ID", "时间", "IP", "动作", "说明", "操作"],
                  v["heads"])
            check("逐行日志那两个控件在事件表这一屏是收掉的（哪一份／实时跟随）",
                  not v["followShown"] and len(v["viewSegs"]) == 2,
                  "%s / %s" % (v["followShown"], v["kindSegs"]))
        else:
            check("手机上出的是竖排卡片，横向不溢出", v["useCards"] and not v["useTable"],
                  "卡片 %s 表格 %s" % (v["useCards"], v["useTable"]))
        check("表里有行（一行都没有就是夹具或聚合坏了）", len(v["items"]) > 0, len(v["items"]))

        # ---- 底部那句总数：必须等于接口说的 matched，且翻页后会跟着变 ----
        m = re.search(r"第 ([\d,]+)–([\d,]+) 条 / 共 ([\d,]+) 条\s*· 涉及 ([\d,]+) 个人", v["foot"])
        check("底部那句是「第 x–y 条 / 共 N 条 · 涉及 M 个人」", m is not None, v["foot"])
        if m:
            n1 = int(m.group(1).replace(",", ""))
            n2 = int(m.group(2).replace(",", ""))
            check("那句「第 x–y 条」说的是真的：行数对得上", n2 - n1 + 1 == len(v["items"]),
                  "%s–%s 对 %s 行" % (n1, n2, len(v["items"])))
            check("「涉及 M 个人」是个数（至少 1 个人）",
                  int(m.group(4).replace(",", "")) >= 1, m.group(4))
        check("总数那句带上了被筛的行数（scanned），不会只剩一个孤零零的共 N 条",
              scanned_of(v["foot"]) > 0, v["foot"])
        base_total = total_of(v["foot"])
        base_scanned = scanned_of(v["foot"])
        # 界面读的是哪一个世界，从头部那句里取，不自己猜：夹具那两个世界的行数不一样。
        ms = re.search(r"/ (\S+?)：玩家事件", v["head"])
        shard_now = ms.group(1) if ms else "Master"
        expect_scanned = fixture_lines(shard_now)
        check("没切窗口时那句「从 N 行里定性出来的」就是这两份文件的行数（数字对得上盘）",
              base_scanned == expect_scanned,
              "%s / %s：界面 %s ← 盘上 %s" % (ROOM, shard_now, base_scanned, expect_scanned))
        check("头部说了窗口行数，也说了发言算不算进来",
              "每份文件回看" in v["head"] and "玩家发言没算进来" in v["head"], v["head"][:200])
        tip = open_hint(b, "怎么读")
        check("「怎么读」点开以后解释了身份列的来处（认出了几个人的连接线索）",
              re.search(r"认出了 \d+ 个人的连接线索", tip) is not None, tip[:200])
        check("note 里说了玩家发言默认不算进来",
              "玩家发言" in v["note"], v["note"][:160])

        # ---- 每一行的身份列：真接得上人，且该空的敢空 ----
        if desk:
            sid = col(b, "Steam ID") or []
            ip = col(b, "IP") or []
            pf = col(b, "角色") or []
            check("Steam ID 这一列接得上那一轮里的连接线索（17 位数字）",
                  any(re.search(r"\d{17}", x) for x in sid), sid[:6])
            check("IP 这一列给的是点得清的地址",
                  any(re.match(r"^\d{1,3}(\.\d{1,3}){3}$", x) for x in ip), ip[:6])
            check("认不出来的那几行敢留空（不是拿别人的地址填上去）",
                  any(x in ("—", "") for x in ip) and any(x in ("—", "") for x in sid),
                  "ip=%s sid=%s" % (ip[:6], sid[:6]))
            check("角色这一列全是中文（不是 wilson 这种 prefab）",
                  all(x == "不知道" or has_cjk(x) for x in pf), pf[:6])
            check("第一行是最新的一记，时间是 hh:mm:ss 这种墙上时间",
                  re.match(r"^\d{2}:\d{2}:\d{2}", v["items"][0]["time"]) is not None,
                  v["items"][0]["time"])
        else:
            check("手机上每一张卡片都带着时间",
                  all(re.match(r"^\d{2}:\d{2}:\d{2}", x["time"]) for x in v["items"]),
                  [x["time"] for x in v["items"]][:4])
            check("卡片里有真实的空身份（阿黎那种只有验证行的，IP 和 Steam ID 就该是空的）",
                  any("203.0.113." not in x["all"] for x in v["items"]),
                  [x["who"] for x in v["items"] if "203.0.113." not in x["all"]][:4])
        check("头像上写的是角色短名", all(x["ava"] for x in v["items"]),
              [x["ava"] for x in v["items"]][:4])
        check("每一行的动作芯片上只有中文（码值没漏到界面上）",
              all(has_cjk(x["action"]) for x in v["items"]),
              [x["action"] for x in v["items"]][:8])

        # ---- 每个筛选控件各按一遍 ----
        HANDLES = ("玩家", "动作", "关键词", "回看多少行", "每页几条", "玩家发言")
        missing = [x for x in HANDLES if not field_value(b, x)]
        check("每一栏筛选都有把手（玩家/动作/关键词/回看/每页/发言）", not missing, missing)
        act_opts = (field_value(b, "动作") or {}).get("options") or []
        check("动作下拉的选项全是中文，且第一条是「全部动作」",
              bool(act_opts) and act_opts[0] == "全部动作" and
              all(has_cjk(o) for o in act_opts), act_opts)

        # 下拉里每一档都点过去：这一列的选项是后端从行为目录推出来的，
        # 目录里摆一档「有人被封禁」而表里永远没有这一行，用户筛完就会以为这个房间没人被封过。
        dead_act = []
        for label in act_opts[1:]:
            if not pick_option(b, "动作", label):
                dead_act.append("%s（下拉里点不到）" % label)
                continue
            if not b.wait("document.querySelectorAll('.ev-card, table.ev-table tbody tr').length >= 1"):
                dead_act.append(label)
                continue
            time.sleep(0.5)
            v = b.js(READ)
            # 留下的行必须全是这一档——筛完还显示别的行为，说明点下去根本没生效。
            if not v["items"] or any(x["action"] != label for x in v["items"]):
                dead_act.append("%s←%s" % (label, [x["action"] for x in v["items"]][:3]))
        check("动作下拉里没有筛不出东西的一档（逐档点过去，留下的行也都对得上）",
              not dead_act, dead_act)
        if not pick_option(b, "动作", "全部动作"):
            raise SystemExit("逐档点完以后放不回「全部动作」")
        if not b.wait("document.querySelectorAll('.ev-card, table.ev-table tbody tr').length > 1"):
            raise SystemExit("放开动作以后表没回来")
        time.sleep(0.8)

        if not type_into(b, "玩家", "阿栗"):
            raise SystemExit("「玩家」那一栏没找到")
        if not b.wait("document.querySelectorAll('.ev-card, table.ev-table tbody tr').length >= 1"):
            raise SystemExit("按玩家筛以后一行都没有")
        time.sleep(0.8)
        v = b.js(READ)
        check("按玩家筛：留下的每一行都是这个人",
              v["items"] and all(x["who"] == "阿栗" for x in v["items"]),
              [x["who"] for x in v["items"]])
        check("按玩家筛以后总数真的变小了（不是只截了这一页）",
              0 < total_of(v["foot"]) < base_total, "%s → %s（筛前）" % (base_total, total_of(v["foot"])))
        check("筛完回到第 1 页（底部那句从第 1 条开始）",
              re.match(r"^第 1–", v["foot"]) is not None, v["foot"])

        if not type_into(b, "玩家", ""):
            raise SystemExit("清空「玩家」那一栏没成功")
        if not b.wait("document.querySelectorAll('.ev-card, table.ev-table tbody tr').length > 1"):
            raise SystemExit("清空条件以后表没回来")
        time.sleep(0.8)

        if not pick_option(b, "动作", "死亡"):
            raise SystemExit("动作下拉里没找到「死亡」这一项")
        if not b.wait("document.querySelectorAll('.ev-card, table.ev-table tbody tr').length >= 1"):
            raise SystemExit("按动作筛以后一行都没有")
        time.sleep(0.8)
        v = b.js(READ)
        # 下拉里那一档显示的是"玩家死亡"（选项文字），筛出来的行动作芯片也是同一句。
        check("按动作筛：留下的每一行动作都是那一个",
              v["items"] and all(x["action"] == "玩家死亡" for x in v["items"]),
              [x["action"] for x in v["items"]])
        check("变糟的那一记上了色（warn 档）",
              all(x["tone"] == "warn" for x in v["items"]), [x["tone"] for x in v["items"]])
        hint = field_text(b, "动作", ".hint")
        check("选中动作以后下面展开的是整句解释（不只是一个短名）",
              len(hint) > 12 and has_cjk(hint), hint)

        if not pick_option(b, "动作", "全部动作"):
            raise SystemExit("动作放不回「全部动作」这一项")
        if not b.wait("document.querySelectorAll('.ev-card, table.ev-table tbody tr').length > 1"):
            raise SystemExit("放开动作以后表没回来")
        time.sleep(0.8)

        if not type_into(b, "关键词", "203.0.113.11"):
            raise SystemExit("「关键词」那一栏没找到")
        if not b.wait("document.querySelectorAll('.ev-card, table.ev-table tbody tr').length >= 1"):
            raise SystemExit("按 IP 当关键词筛以后一行都没有")
        time.sleep(0.8)
        v = b.js(READ)
        check("关键词能顺着 IP 捞人：留下的每一行都来自那个地址",
              v["items"] and all("203.0.113.11" in x["all"] for x in v["items"]),
              [x["who"] for x in v["items"]])
        check("顺着 IP 捞出来的是阿栗（那一行自己没写 IP，是面板接的身份）",
              all(x["who"] == "阿栗" for x in v["items"]), [x["who"] for x in v["items"]])

        # 一个故意筛不出来的关键词：给的必须是空状态 + 出口，不是报错也不是白屏。
        if not type_into(b, "关键词", "这条一定筛不出"):
            raise SystemExit("「关键词」那一栏没找到")
        if not b.wait("!!document.querySelector('.empty .e-title')"):
            raise SystemExit("筛不出东西时没给空状态")
        time.sleep(0.6)
        v = b.js(READ)
        check("筛不出时给的是空状态加「清掉条件」，不是一句报错",
              v["empty"] and "清掉条件" in " ".join(v["emptyBtns"]) and not v["errBox"],
              v["empty"] + "｜" + str(v["emptyBtns"]) + "｜" + v["errBox"])
        check("空状态同时说了窗口里一共扫了多少行（不说成这儿本来就没日志）",
              re.search(r"一共回看了 [\d,]+ 行", v["emptyAll"]) is not None, v["emptyAll"][:160])
        if not click_btn(b, "清掉条件", ".empty"):
            raise SystemExit("「清掉条件」没点着")
        if not b.wait("document.querySelectorAll('.ev-card, table.ev-table tbody tr').length >= 1"):
            raise SystemExit("清掉条件以后表还是空的")
        time.sleep(0.9)
        left = [x for x in ("玩家", "关键词", "动作") if (field_value(b, x) or {}).get("value")]
        check("条件清掉了：玩家/关键词/动作三栏都回到空", not left, left)

        # ---- 发言开关是真的：总数会变大 ----
        before = total_of(b.js(READ)["foot"])
        if not toggle_switch(b, "玩家发言"):
            raise SystemExit("「玩家发言」那一栏的开关没找到")
        if not b.wait("!!document.querySelector('.ev-foot')"):
            raise SystemExit("开发言以后底部那句没了")
        time.sleep(1.0)
        v = b.js(READ)
        after = total_of(v["foot"])
        check("打开玩家发言以后总数真的变大了", after > before, "开前 %s 开后 %s" % (before, after))
        check("头部那句跟着改（不再写「玩家发言没算进来」）",
              "玩家发言没算进来" not in v["head"], v["head"][:180])
        if not toggle_switch(b, "玩家发言"):
            raise SystemExit("关不掉「玩家发言」")
        if not b.wait("((document.querySelector('.card-head')||{}).textContent||'').indexOf('玩家发言没算进来') >= 0"):
            raise SystemExit("关掉发言以后头部那句没回来")
        time.sleep(0.8)

        # ---- 分页：每页 10 条 → 出页码 → 翻到第 2 页，那句范围跟着改 ----
        if not pick_option(b, "每页几条", "10"):
            raise SystemExit("每页几条里没找到 10")
        if not b.wait("document.querySelectorAll('.pg-num').length >= 2"):
            raise SystemExit("每页 10 条以后没翻出第二页")
        time.sleep(1.0)
        v = b.js(READ)
        check("每页 10 条时这一页确实不超过 10 行", 0 < len(v["items"]) <= 10, len(v["items"]))
        check("页码出来了，且第 1 页是选中态",
              v["pager"] and v["pager"][0].endswith("[开]"), v["pager"])
        first_time = v["items"][0]["time"]
        if not click_btn(b, "2", ".pg-nums"):
            raise SystemExit("页码 2 没点着")
        if not b.wait("!!document.querySelector('.ev-card, table.ev-table tbody tr')"):
            raise SystemExit("翻到第 2 页以后没出行")
        time.sleep(1.0)
        v = b.js(READ)
        m2 = re.search(r"第 ([\d,]+)–([\d,]+) 条 / 共 ([\d,]+) 条", v["foot"])
        check("第 2 页那句范围跟着改到从 11 开始", m2 is not None and m2.group(1) == "11", v["foot"])
        check("第 2 页的内容和第 1 页不是同一批（第一行的时间变了）",
              v["items"] and v["items"][0]["time"] != first_time,
              "%s → %s" % (first_time, v["items"] and v["items"][0]["time"]))
        check("页码上选中态跟着走到第 2 页", "2[开]" in v["pager"], v["pager"])
        check("上一页这一会儿是能点的", bool(v["pgBtns"]) and "[关]" not in v["pgBtns"][0], v["pgBtns"])
        check("翻页不会把总数说小（共 N 条还是那个 N）",
              m2 is not None and total_of(v["foot"]) == before, v["foot"])
        if not pick_option(b, "每页几条", "20"):
            raise SystemExit("每页几条里没找到 20")
        if not b.wait("((document.querySelector('.ev-foot')||{}).textContent||'').indexOf('第 1–') >= 0"):
            raise SystemExit("改每页条数以后没回到第 1 页")
        time.sleep(1.0)
        v = b.js(READ)
        check("回到每页 20 条时页码回到第 1 页（改每页条数不该停在第 2 页）",
              re.match(r"^第 1–", v["foot"]) is not None, v["foot"])

        # ---- 窗口档位：收窄到 200 行，界面上那几句和 scanned 都要跟着改 ----
        opts = (field_value(b, "回看多少行") or {}).get("options") or []
        check("回看档位含后端默认 500 行", any("500" in x for x in opts), opts)
        matched_wide = total_of(v["foot"])
        newest_wide = v["items"][0]["time"] if v["items"] else ""
        if not pick_option(b, "回看多少行", "200"):
            raise SystemExit("窗口收不到 200 行那一档")
        if not b.wait("!!document.querySelector('.ev-foot')"):
            raise SystemExit("收窄窗口以后底部那句没了")
        time.sleep(1.0)
        v = b.js(READ)
        check("窗口改成 200 行以后头部那句跟着改", "每份文件回看 200 行" in v["head"], v["head"][:150])
        check("窗口切掉了更早的行，就得有一句话说明",
              "只回看了每份文件最近 200 行" in v["note"], v["note"][:180])
        check("scanned 说的就是被筛的那一段（不是身份索引多读的那一截）",
              200 <= scanned_of(v["foot"]) <= 260, scanned_of(v["foot"]))
        # 夹具里那几条连接/进服行排在落盘噪音之前，也就是排在行为行的更前面，
        # 所以把窗口从 500 收到 200 一定会切掉几记——切掉是允许的，那句 note 就是在说这件事，
        # 不允许的是反过来：收窄窗口还多出行为行，那只能是数错了。
        check("收窄窗口只会让总数变小或持平（不会凭空多出行为行）",
              0 < total_of(v["foot"]) <= matched_wide,
              "%s → %s" % (matched_wide, total_of(v["foot"])))
        check("收窄以后排在最上面的还是同一记（最新的那条不会被窗口切走）",
              v["items"] and v["items"][0]["time"] == newest_wide,
              "%s → %s" % (newest_wide, v["items"] and v["items"][0]["time"]))
        check("身份列在收窄以后照样接得上（连接行被切出行为窗口，身份窗口还认得他）",
              any(re.search(r"\d{17}", x) for x in (col(b, "Steam ID") or [])) if desk else True,
              (col(b, "Steam ID") or [])[:4])
        if not pick_option(b, "回看多少行", "2000"):
            raise SystemExit("窗口放不回 2000 行")
        if not b.wait(r"((document.querySelector('.card-head')||{}).textContent||'').indexOf('每份文件回看 2,000 行') >= 0"):
            raise SystemExit("窗口放回 2000 行以后头部没跟着改")
        time.sleep(0.8)
        v = b.js(READ)
        check("整个文件都在窗口里时，那句「更早的行为没算进来」不该还挂着",
              "只回看了每份文件最近" not in v["note"], v["note"][:180])
        check("放回最大窗口以后 scanned 又回到两份文件的全部行数",
              scanned_of(v["foot"]) == expect_scanned,
              "界面 %s ← 盘上 %s" % (scanned_of(v["foot"]), expect_scanned))

        # ---- 列设置 ----
        if desk:
            if not click_btn(b, "列设置", ".card-head"):
                raise SystemExit("「列设置」没点着")
            if not b.wait("!!document.querySelector('.pop')"):
                raise SystemExit("列设置面板没出来")
            time.sleep(0.4)
            v = b.js(READ)
            check("列设置里九列都列出来了，固定那四枚是锁着的",
                  len(v["popItems"]) == 9 and sum(1 for x in v["popItems"] if "[锁]" in x) == 4,
                  v["popItems"])
            check("列设置里可关的那五枚写着当前是开着的",
                  sum(1 for x in v["popItems"] if "[开]" in x) == 5, v["popItems"])
            if not click_btn(b, "IP", ".pop"):
                raise SystemExit("列设置里的 IP 没点着")
            time.sleep(0.6)
            v = b.js(READ)
            check("收掉 IP 以后表头真的少一列",
                  "IP" not in v["heads"] and len(v["heads"]) == 8, v["heads"])
            check("收掉一列以后每一行的格子数跟着少（不错位）",
                  all(len(x["cells"]) == 8 for x in v["items"]),
                  [len(x["cells"]) for x in v["items"]][:6])
            if not click_btn(b, "IP", ".pop"):
                raise SystemExit("IP 放不回来")
            time.sleep(0.6)
            v = b.js(READ)
            check("放回来以后又是九列，每行九个格子",
                  len(v["heads"]) == 9 and all(len(x["cells"]) == 9 for x in v["items"]), v["heads"])
            b.js("document.body.click()")

        # ---- 展开全文：说明那一列默认只给两行 ----
        # 这一屏里唯一超过 160 字的原文行是夹具那条长发言（玩家真的会打这么长），
        # 而发言默认不进这张表，所以得先把它加进来才测得到这一枚控件。
        if not toggle_switch(b, "玩家发言"):
            raise SystemExit("「玩家发言」那一栏的开关没找到")
        if not b.wait("!!document.querySelector('.ev-foot')"):
            raise SystemExit("开发言以后底部那句没了")
        time.sleep(1.2)
        # 查询必须限定在当前这一档宽度真的显示出来的那个容器里：桌面表格和手机卡片
        # 同时在 DOM 里，只靠 CSS 隐藏其中一个，全局 find() 会量到那个 display:none 的，
        # 高度就成了 0 → 0，看着像控件坏了。
        long_row = b.js(r"""(() => {
          const box = document.querySelector(%(scope)s);
          const btn = [...(box || document).querySelectorAll('button')].find((x) => x.textContent.trim() === '展开全文');
          if (!btn) return null;
          const cell = btn.closest('td') || btn.closest('.ev-card');
          const el = cell && cell.querySelector('.ev-raw');
          if (!el) return null;
          const h0 = el.getBoundingClientRect().height;
          btn.click();
          return { h0: h0 };
        })()""" % {"scope": json.dumps(scope)})
        if long_row:
            time.sleep(0.4)
            now = b.js(r"""(() => { const box = document.querySelector(%(scope)s);
              const root = box || document;
              const el = root.querySelector('.ev-raw.expanded') || root.querySelector('.ev-raw');
              const btn = [...root.querySelectorAll('button')].find((x) => /^(展开全文|收起)$/.test(x.textContent.trim()));
              return { h: el ? el.getBoundingClientRect().height : 0, label: btn ? btn.textContent.trim() : '' }; })()""" % {"scope": json.dumps(scope)})
            check("展开全文那枚是真的展开（行高变大，文字换成收起）",
                  now["h"] > long_row["h0"] and now["label"] == "收起",
                  "%s → %s｜%s" % (long_row["h0"], now["h"], now["label"]))
            click_btn(b, "收起", scope)
            time.sleep(0.3)
        else:
            check("展开全文那枚出现在长行上（夹具那条长发言就该在这一屏）", False,
                  "屏上没有「展开全文」按钮")

        # 测完把发言收回默认那档：后面的检查都按默认口径（发言不进表）走。
        if not toggle_switch(b, "玩家发言"):
            raise SystemExit("「玩家发言」那一栏的开关没找到（收回去时）")
        if not b.wait("((document.querySelector('.card-head')||{}).textContent||'')"
                      ".indexOf('玩家发言没算进来') >= 0"):
            raise SystemExit("关掉发言以后头部那句没回来")
        time.sleep(0.6)

        # ---- 看原文：跳过去的必须是逐行日志且有行 ----
        if not click_btn(b, "看原文", scope):
            raise SystemExit("第一行的「看原文」没点着")
        if not b.wait("!!document.querySelector('.stream-meta')"):
            raise SystemExit("跳过去以后逐行日志的上下文没出来")
        time.sleep(1.0)
        lv = b.js(READ)
        check("「看原文」把我带到逐行日志（这一枚是开的）",
              lv["viewSegs"] == ["玩家事件", "逐行日志[开]"], lv["viewSegs"])
        check("带过去的条件不是空的：日志框里有原文行（不是假链接）",
              lv["logLines"] >= 1, "logline=%s｜%s" % (lv["logLines"], lv["empty"]))
        check("带过去以后「哪一份」和「实时跟随」都回来了",
              lv["followShown"] and len(lv["kindSegs"]) >= 2,
              "%s / %s" % (lv["followShown"], lv["kindSegs"]))
        check("过去以后「实时跟随」是关着的（不是一次 SSE 订阅挂在后台）",
              lv["followOn"] is False, lv["followOn"])
        check("逐行日志那一边说的是「给出 N 行」，跟屏幕上的行数量级一致",
              re.search(r"给出 ([\d,]+) 行", lv["meta"]) is not None, lv["meta"][:150])
        if not click_seg(b, "玩家事件"):
            raise SystemExit("回不到玩家事件")
        if not b.wait("!!document.querySelector('.ev-foot')"):
            raise SystemExit("回到事件表以后底部那句没回来")
        time.sleep(1.0)

        # ---- 拉黑：确认框 → 写名单 → 提示 → 还原文件 ----
        before_bytes = open(BLOCKLIST, "rb").read() if os.path.exists(BLOCKLIST) else None
        try:
            if not click_live_btn(b, "拉黑", scope):
                raise SystemExit("「拉黑」没点着（可能这一屏的行都没有账户编号）")
            if not b.wait("!!document.querySelector('.mask .dialog')"):
                raise SystemExit("确认框没出来")
            time.sleep(0.4)
            c = b.js("""(() => { const d = document.querySelector('.mask .dialog');
              return { t: (d.querySelector('h3') || {}).textContent || '',
                       b: (d.querySelector('p.muted') || {}).textContent || '',
                       w: (d.querySelector('.note') || {}).textContent || '',
                       btns: [...d.querySelectorAll('button')].map((x) => x.textContent.trim()) }; })()""")
            check("拉黑之前先说清写进哪份文件、什么时候生效",
                  "blocklist.txt" in c["b"] and re.search(r"下次启动|重启", c["b"]) and
                  "不会马上切断" in c["w"], c)
            check("确认框里那枚的文案是「加进黑名单」（不是含糊的确定）",
                  "加进黑名单" in " ".join(c["btns"]), c["btns"])
            if not click_btn(b, "加进黑名单", ".mask"):
                raise SystemExit("确认框里那枚没点着")
            if not b.wait("!!document.querySelector('.toast')"):
                raise SystemExit("按下去以后没有任何提示")
            time.sleep(1.0)
            now = open(BLOCKLIST, "rb").read().decode("utf-8") if os.path.exists(BLOCKLIST) else ""
            check("点完拉黑，KU_ 编号真的写进了 blocklist.txt",
                  "KU_fakeid" in now and now != (before_bytes or b"").decode("utf-8"), now[:120])
            t = b.js(r"""(() => { const x = document.querySelector('.toast');
              return x ? x.textContent.replace(/\s+/g, ' ').trim() : ''; })()""")
            check("提示说的是这份名单什么时候被引擎读到（不是「已拉黑，他断线了」）",
                  has_cjk(t) and "重启" in t, t)
            b.js("""(() => { const x = document.querySelector('.toast .close, .toast button'); if (x) x.click(); })()""")
        finally:
            if before_bytes is not None:
                with open(BLOCKLIST, "wb") as fh:
                    fh.write(before_bytes)
            else:
                try:
                    os.remove(BLOCKLIST)
                except OSError:
                    pass

        b.js("document.querySelector('.card-head') && document.querySelector('.card-head').scrollIntoView({block:'start'});")
        time.sleep(0.4)
        v = b.js(READ)
        check("这一路点下来表还在（没有哪一步把它筛成永久空白）", bool(v["items"]), v["foot"])
        check("这一档宽度没有横向溢出", v["overflow"] <= 0, v["overflow"])
        b.shoot(os.path.join(OUT, "events_%d.png" % w))
        errs = b.js("window.__perr || []") or []
        check("这一路没有未捕获异常", not errs, errs)
    finally:
        b.close()

    print("\n视口 %s：%d 项检查，%d 项没通过" % (w, len(REPORT), len(FAILED)))
    if FAILED:
        for f in FAILED:
            print("  红: %s" % f)
        sys.exit(1)


if __name__ == "__main__":
    main()
