#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""「照这间开服」：从公网房间那一行带一份预填进开服向导，逐步在浏览器里对账。

为什么要单独跑这一遍：这条链路是纯前端的（后端没改一个字），Go 单测和接口探针都碰不到它。
它最容易错的三件事都只有浏览器能验：
  1. 带过去的值是不是真落进了表单——不是"我以为 v-model 收到了"，而是第 2 步那颗分段按钮
     真亮在「生存」上、人数框里真是那个数；
  2. 搬不过来的项有没有当场说出来——静悄悄少掉一栏，用户会以为那一间本来就没这个设定；
  3. 界面上的字和真发出去的是不是一致——收条写「无尽」而表单里是别的，就是面板在撒谎。

它只读：不建房间、不改配置、不碰云机；公网列表读的是 Klei 公开快照。
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
SHOTS = os.path.join(HERE, "tmp", "borrow61")
DESKTOP = (1440, 900)
MOBILE = (390, 844)

# 面板建房间时这一栏收的三个值（对照 internal/api/bootstrap.go 的 gameModes）。
MODES = {"survival": "生存", "endless": "无尽", "wilderness": "荒野"}
DEFAULTS = {"gameMode": "无尽", "pvp": "关", "maxPlayers": "6"}

fails = []


def check(name, ok, detail=""):
    print("  %s  %s%s" % ("ok  " if ok else "FAIL", name, ("  ｜" + detail) if detail else ""))
    if not ok:
        fails.append(name + ("（" + detail + "）" if detail else ""))
    return ok


def say(text):
    print("  ·  " + text)


def js_arg(v):
    return json.dumps(v, ensure_ascii=False)


def api_get(path, params=None, timeout=200):
    q = {k: v for k, v in (params or {}).items() if v not in ("", None)}
    url = BASE + path + ("?" + urllib.parse.urlencode(q) if q else "")
    req = urllib.request.Request(url, headers={"Authorization": AUTH})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        body = json.loads(r.read().decode("utf-8"))
    if not body.get("ok"):
        raise SystemExit("接口 %s 回了失败：%s" % (path, json.dumps(body, ensure_ascii=False)[:300]))
    return body["data"]


# 向导那一屏的状态：只读渲染出来的东西，不碰 Vue 内部状态——组件里的 borrow 再对，
# 收条上写错了句子也还是错的。
WIZ_READ = r"""(() => {
  const card = document.querySelector('.stepper');
  if (!card) return null;
  const sec = card.closest('section') || document;
  const vis = (el) => !!(el && el.offsetParent !== null);
  // 收条认准"这一份是照公网那一间填的"那句：它才是用户看得见的那块，
  // 而不是某个我起给测试用的 class。
  const note = [...sec.querySelectorAll('.note')].find((x) => x.innerText.indexOf('这一份是照公网那一间') >= 0);
  const rowText = (t) => (t || '').replace(/\s+/g, ' ').trim();
  return {
    stepCount: (sec.querySelector('.stp-count') || {}).textContent || '',
    hasReceipt: !!note,
    receipt: note ? rowText(note.innerText) : '',
    applied: note ? [...note.querySelectorAll('.chip.ok')].map((x) => rowText(x.textContent)) : [],
    skipped: note ? [...note.querySelectorAll('.chip.warn')].map((x) => rowText(x.textContent)) : [],
    // 每一枚"没搬"的圆片后面必须真跟着一句话：光列名字等于把原因咽回去，
    // 而这一条只在渲染之后才看得出来（Vue 的模板里写着 {} 也照样画得出圆片）。
    skipReasons: note ? [...note.querySelectorAll('.chip.warn')].map((x) => {
      const n = x.nextElementSibling;
      return rowText(n ? n.textContent : '');
    }) : [],
    hasDrop: note ? [...note.querySelectorAll('button')].some((x) => x.textContent.trim() === '不要这份预填') : false,
    dropDisabled: (() => { const b = [...sec.querySelectorAll('button')].find((x) => x.textContent.trim() === '不要这份预填');
                            return b ? !!b.disabled : null; })(),
    errBoxes: [...sec.querySelectorAll('.err-box')].map((x) => rowText(x.textContent)),
    kv: [...sec.querySelectorAll('.kv dt')].map((dt, i) => ({
      k: rowText(dt.textContent),
      v: rowText((dt.nextElementSibling || {}).textContent || ''),
    })),
    overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    pageErrors: (window.__errs || []).slice(0, 6),
  };
})()"""

# 第 2 步那三栏的真实状态：分段按钮亮在哪一枚、复选框勾没勾、人数框里是哪个数。
STEP2_READ = r"""(() => {
  const sec = document.querySelector('.stepper').closest('section');
  const field = (label) => [...sec.querySelectorAll('.grid-form .field')].find((f) => {
    const l = f.querySelector('label');
    return l && l.textContent.trim() === label;
  });
  const on = (label) => { const f = field(label);
    if (!f) return null; const b = [...f.querySelectorAll('.seg button')].find((x) => x.classList.contains('on'));
    return b ? b.textContent.trim() : ''; };
  const pv = [...sec.querySelectorAll('.switch')].find((x) => x.textContent.indexOf('开启 PVP') >= 0);
  const mp = field('最大玩家数');
  return {
    gameMode: on('游戏模式'),
    intention: on('游戏偏好'),
    pvp: pv ? !!pv.querySelector('input').checked : null,
    maxPlayers: mp ? String(mp.querySelector('input[type=number]').value) : null,
  };
})()"""


def wiz(b):
    return b.js(WIZ_READ)


def settle(b, pred, tries=40, pause=0.4):
    last = None
    for _ in range(tries):
        last = wiz(b)
        if last is not None and pred(last):
            return last, True
        time.sleep(pause)
    return last, False


def login_and_open_lobby(b, path):
    b.goto(BASE + "/?cdp=%d%s" % (int(time.time()), path))
    if b.wait("!!document.querySelector('.login-form')", tries=20, pause=0.25):
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
    b.js("window.__errs = [];"
         "addEventListener('error', (e) => window.__errs.push(String(e.message || e.error)));"
         "addEventListener('unhandledrejection', (e) => window.__errs.push('promise:' + String(e.reason)));")
    if not b.wait("!!document.querySelector('.lobby-page table.data tbody tr')", tries=160, pause=0.5):
        b.shoot(os.path.join(SHOTS, "no_rows.png"))
        raise SystemExit("公网列表首屏没出行（这台机器出不去公网时就跑不了这一轮）")


def borrow_row(b, room):
    """在表格里认准那一行（地址 + 端口两个单元格都对上才按），点它那颗「照这间开服」。

    按地址端口定位而不是"第一行"：这一页每 30 秒自己重取，快照在换，
    我手里那份接口数据必须和屏幕上那一行是同一间，否则后面比的每一条都是假的。
    """
    got = b.js("""(() => {
      const rows = [...document.querySelectorAll('.lobby-page table.data tbody tr')];
      const hit = rows.find((r) => {
        const c = r.querySelectorAll('td');
        return c[1] && c[1].textContent.trim() === %s && c[2] && c[2].textContent.trim() === %s;
      });
      if (!hit) return 'no-row';
      const btn = [...hit.querySelectorAll('button')].find((x) => x.textContent.indexOf('照这间开服') >= 0);
      if (!btn) return 'no-btn:' + [...hit.querySelectorAll('button')].map((x) => x.textContent.trim()).join('/');
      const name = hit.querySelectorAll('td')[0].textContent.trim();
      window.__borrowName = name;
      btn.click();
      return name;
    })()""" % (js_arg(room["addr"]), js_arg(str(room["port"]))))
    return got


def click_step(b, label):
    return b.js("""(() => {
      const t = [...document.querySelectorAll('.stepper button.stp')].find((x) => (x.getAttribute('title') || '').indexOf(%s) >= 0);
      if (!t || t.disabled) return false;
      t.click(); return true;
    })()""" % js_arg(label))


def click_next(b):
    return b.js("""(() => {
      const t = [...document.querySelectorAll('button')].find((x) => /^(下一步|下一步 →|下一步)/.test(x.textContent.trim()) && !x.disabled);
      if (!t) return false; t.click(); return true;
    })()""")


def fill_name(b, text):
    return b.js("""(() => {
      const sec = document.querySelector('.stepper').closest('section');
      const f = [...sec.querySelectorAll('.field')].find((x) => {
        const l = x.querySelector('label'); return l && l.textContent.trim() === '服务器名称（必填）';
      });
      if (!f) return null;
      const i = f.querySelector('input');
      i.value = %s; i.dispatchEvent(new Event('input', {bubbles: true}));
      return i.value;
    })()""" % js_arg(text))


def click_drop(b):
    return b.js("""(() => {
      const sec = document.querySelector('.stepper').closest('section');
      const t = [...sec.querySelectorAll('button')].find((x) => x.textContent.trim() === '不要这份预填');
      if (!t) return false; t.click(); return true;
    })()""")


def expect_join(room):
    """接口那一行的原始字段 → 收条上该出现的枚数。判定表和 room.js 的 takeBorrow 同源。"""
    applied = []
    skipped = []
    if room["intent"] in MODES:
        applied.append("游戏模式 · %s" % MODES[room["intent"]])
    else:
        skipped.append("游戏模式")
    applied.append("PVP · %s" % ("开" if room["pvp"] else "关"))
    cap = int(room["capacity"] or 0)
    if 1 <= cap <= 16:
        applied.append("最大玩家数 · %d 人" % cap)
    else:
        skipped.append("最大玩家数")
    skipped.append("服务器名称")
    if room["password"]:
        skipped.append("连接密码")
    if not room["allowNew"]:
        skipped.append("收新人")
    return applied, skipped


def pick(rooms, want_match, other_defaults):
    """挑一行：want_match=True 要三项全能搬过来的，False 要至少有一项搬不过来的。

    两种都得真跑一遍，因为 takeBorrow 的分叉就在这儿——只测顺的那条，
    "对不上要报出来"那一支一辈子没被执行过。
    """
    for r in rooms:
        mode_ok = r["intent"] in MODES
        cap_ok = 1 <= int(r["capacity"] or 0) <= 16
        differs = (r["intent"] != "endless" or int(r["capacity"] or 0) != 6 or bool(r["pvp"]))
        if want_match and mode_ok and cap_ok and differs:
            return r
        if not want_match and (not mode_ok or not cap_ok):
            return r
    return None


def main():
    os.makedirs(SHOTS, exist_ok=True)
    regions = api_get("/api/v1/lobby/regions")
    if not check("公网列表可用", regions.get("enabled") is True,
                 json.dumps(regions, ensure_ascii=False)[:160]):
        say("这一轮需要这台机器出得去公网（读 Klei 公开快照），否则只能跑前端其余检查")
        return 0
    reply = api_get("/api/v1/lobby/rooms", {"region": regions.get("defaultRegion")})
    rooms = reply["rooms"]
    row_a = pick(rooms, True, None)
    say("接口这一屏 %d 间，可用 %s 间；A 行（三项都能搬）= %s" % (
        len(rooms), reply["usableRooms"], json.dumps({k: row_a[k] for k in ("name", "intent", "pvp", "capacity", "password")}, ensure_ascii=False)[:200] if row_a else "没有"))
    if not row_a:
        raise SystemExit("这一屏挑不出三项都能搬的行，换一行再来")

    b = Browser(*DESKTOP)
    try:
        login_and_open_lobby(b, "#/lobby")

        # —— 1. 点那一行的「照这间开服」：落到向导第 1 步，带着一份收条 ——————————
        name_on_screen = borrow_row(b, row_a)
        if not check("那一行上找得到「照这间开服」并按下了",
                     name_on_screen not in ("no-row",) and not str(name_on_screen).startswith("no-btn"),
                     str(name_on_screen)[:120]):
            raise SystemExit("按钮没找到，后面没法跑")
        if not b.wait("!!document.querySelector('.stepper')", tries=40, pause=0.3):
            b.shoot(os.path.join(SHOTS, "wizard_missing.png"))
            raise SystemExit("点完没有落到开服向导：hash=%s 正文=%s" % (
                b.js("location.hash"), (b.js("document.body.innerText.slice(0,160)") or "")))
        d, ok = settle(b, lambda x: x["hasReceipt"])
        if not check("向导第 1 步顶上摆出了那份收条", ok, json.dumps(d or {}, ensure_ascii=False)[:260]):
            raise SystemExit("收条没渲染，后面每一条都无从比起")
        check("落地就在第 1 步（不是把人丢在最后一步）",
              "第 1 /" in (d["stepCount"] or "").replace(" ", "") or "第 1" in (d["stepCount"] or ""),
              d["stepCount"])
        want_applied, want_skipped = expect_join(row_a)
        check("搬过来的枚数与内容 == 接口那行字段推出来的",
              sorted(d["applied"]) == sorted(want_applied),
              "界面 %s｜该是 %s" % (d["applied"], want_applied))
        check("没搬的枚数与内容 == 该有的那几项（一项都不静悄悄少掉）",
              sorted(d["skipped"]) == sorted(want_skipped),
              "界面 %s｜该是 %s" % (d["skipped"], want_skipped))
        check("收条点名了那一间的名字（就是屏幕上那一行的名字）",
              name_on_screen and name_on_screen in d["receipt"],
              "屏幕 %r｜收条 %r" % (name_on_screen, d["receipt"][:120]))
        check("没搬的每一项都跟着一句原因（不是光列个名字）",
              len(d["skipReasons"]) == len(d["skipped"]) and all(len(t) > 12 for t in d["skipReasons"]),
              json.dumps(d["skipReasons"], ensure_ascii=False)[:260])
        check("说清了边界：世界生成的细节不在那份数据里",
              "世界大小" in d["receipt"] and "季节" in d["receipt"], d["receipt"][-160:])
        check("「不要这份预填」按得动（不是枚灰按钮）",
              d["hasDrop"] and d["dropDisabled"] is False, "hasDrop=%s disabled=%s" % (d["hasDrop"], d["dropDisabled"]))
        check("向导这一屏没有报错条", not d["errBoxes"], " ".join(d["errBoxes"]))
        check("桌面 1440 下收条没把页面撑出横向滚动", d["overflow"] <= 0, "溢出 %d px" % d["overflow"])
        b.shoot(os.path.join(SHOTS, "01_receipt.png"))

        # —— 2. 预填真落进了表单：去第 2 步读那三栏 ——————————————————————
        put = fill_name(b, "借鉴验收-临时名")
        check("第 1 步的服务器名称填得进去（下一步的门槛）", put == "借鉴验收-临时名", str(put))
        if not check("点「下一步」进得了第 2 步", click_next(b)):
            raise SystemExit("下一步按不到")
        b.wait("document.querySelectorAll('.stepper ~ * .seg button, .seg button').length > 0", tries=20, pause=0.2)
        s2 = b.js(STEP2_READ)
        say("第 2 步读到：%s" % json.dumps(s2, ensure_ascii=False))
        check("游戏模式那颗亮在接口那一行写的玩法上",
              s2["gameMode"] == MODES.get(row_a["intent"], DEFAULTS["gameMode"]),
              "界面 %s｜接口 intent=%s" % (s2["gameMode"], row_a["intent"]))
        check("PVP 复选框跟着那一间走", s2["pvp"] == bool(row_a["pvp"]),
              "界面 %s｜接口 %s" % (s2["pvp"], row_a["pvp"]))
        check("最大玩家数就是那一行写的人数上限", s2["maxPlayers"] == str(row_a["capacity"]),
              "界面 %s｜接口 %s" % (s2["maxPlayers"], row_a["capacity"]))
        check("没被借鉴碰到的那一栏还是向导自己的默认（游戏偏好=合作）",
              s2["intention"] == "合作", str(s2["intention"]))
        b.shoot(os.path.join(SHOTS, "02_step2_prefill.png"))

        # —— 3. 第 4 步的汇总里有这一行；数字和第 2 步看到的同一个 ——————————
        click_next(b)
        click_next(b)
        d4, ok = settle(b, lambda x: any(r["k"] == "借鉴来源" for r in x["kv"]), tries=30)
        if not check("汇总里写着「借鉴来源」这一行", ok, json.dumps((d4 or {}).get("kv", []), ensure_ascii=False)[:300]):
            raise SystemExit("汇总没出借鉴来源")
        src = [r for r in d4["kv"] if r["k"] == "借鉴来源"][0]["v"]
        check("汇总那句的两个数 == 收条上那两枚计数",
              ("%d 项" % len(want_applied)) in src and ("%d 项没搬" % len(want_skipped)) in src and name_on_screen in src,
              src)
        ppl = [r for r in d4["kv"] if r["k"] == "人数 / 通信"][0]["v"]
        check("汇总那句的人数与 PVP == 第 2 步表单里的（一路没走样）",
              ("%s 人上限" % s2["maxPlayers"]) in ppl and ("PVP " + ("开" if s2["pvp"] else "关")) in ppl,
              ppl)
        b.shoot(os.path.join(SHOTS, "03_summary.png"))

        # —— 4. 「不要这份预填」退回默认，且只退回这三栏 ————————————————
        check("步骤条点得回第 1 步", click_step(b, "第 1 步"))
        b.wait("!!document.querySelector('.stepper')", tries=10, pause=0.2)
        check("「不要这份预填」按得到", click_drop(b))
        d, ok = settle(b, lambda x: not x["hasReceipt"])
        check("收条随之消失（不留一块已经不生效的说明）", ok, json.dumps(d or {}, ensure_ascii=False)[:200])
        click_next(b)
        s2b = b.js(STEP2_READ)
        say("退回之后第 2 步：%s" % json.dumps(s2b, ensure_ascii=False))
        check("三栏回到向导自己的默认值",
              s2b["gameMode"] == DEFAULTS["gameMode"] and s2b["pvp"] is False and s2b["maxPlayers"] == DEFAULTS["maxPlayers"],
              json.dumps(s2b, ensure_ascii=False))
        click_next(b)
        click_next(b)
        d5 = wiz(b)
        check("退回之后汇总里不再有「借鉴来源」（不给下一间留下假来源）",
              not any(r["k"] == "借鉴来源" for r in d5["kv"]), json.dumps(d5["kv"], ensure_ascii=False)[:220])
        nm = [r for r in d5["kv"] if r["k"] == "服务器名称"][0]["v"]
        check("退回预填没有把刚填的名字一起抹掉", nm == "借鉴验收-临时名", nm)
        b.shoot(os.path.join(SHOTS, "04_dropped.png"))

        # —— 5. 搬不过来的那一条分支（对不上的玩法 / 超界的人数）————————————
        row_b = pick(b_reply_rooms(), False, None)
        if row_b is None:
            say("这一屏没有对不上的那一行（玩法都在三种里、人数都 ≤16）——换一屏再验")
            for r in api_get("/api/v1/lobby/rooms", {"region": reply["region"], "page": 2})["rooms"]:
                row_b = pick([r], False, None) or row_b
                if row_b:
                    break
        if row_b is not None:
            say("B 行（至少一项搬不过来）= %s" % json.dumps(
                {k: row_b[k] for k in ("name", "intent", "intentLabel", "capacity")}, ensure_ascii=False)[:200])
            b.js("location.hash = '#/lobby'")
            b.wait("!!document.querySelector('.lobby-page table.data tbody tr')", tries=120, pause=0.5)
            time.sleep(0.6)
            nb = borrow_row(b, row_b)
            if not check("B 行也按到了（屏幕名 %s）" % str(nb)[:40],
                         nb not in ("no-row",) and not str(nb).startswith("no-btn"), str(nb)[:120]):
                pass
            else:
                b.wait("!!document.querySelector('.stepper')", tries=40, pause=0.3)
                db, ok = settle(b, lambda x: x["hasReceipt"])
                if check("B 行也带出一份收条", ok, json.dumps(db or {}, ensure_ascii=False)[:200]):
                    a2, s2list = expect_join(row_b)
                    check("B 行搬过来的那几枚对得上", sorted(db["applied"]) == sorted(a2),
                          "界面 %s｜该是 %s" % (db["applied"], a2))
                    check("B 行没搬的那几枚全列出来了", sorted(db["skipped"]) == sorted(s2list),
                          "界面 %s｜该是 %s" % (db["skipped"], s2list))
                    bad = []
                    if row_b["intent"] not in MODES and "游戏模式" in db["skipped"]:
                        bad += [t for t in db["applied"] if t.startswith("游戏模式")]
                        check("对不上的玩法在收条里点了那一间的原话",
                              (row_b.get("intentLabel") or "没写玩法") in db["receipt"], db["receipt"][:200])
                    if not (1 <= int(row_b["capacity"] or 0) <= 16):
                        check("人数超界时写清了面板只收 1~16",
                              "1~16" in db["receipt"], db["receipt"][:200])
                    check("搬不过来的那一项没有同时出现在「搬过来」里",
                          not bad, str(bad))
                    b.shoot(os.path.join(SHOTS, "05_skip_branch.png"))
                    click_next_ok = fill_name(b, "借鉴验收-B") and click_next(b)
                    if check("B 行也进得了第 2 步", click_next_ok):
                        s2c = b.js(STEP2_READ)
                        if row_b["intent"] not in MODES:
                            check("搬不过来的那一栏留的是默认值（不是把认不出的词塞进表单）",
                                  s2c["gameMode"] == DEFAULTS["gameMode"], str(s2c["gameMode"]))
                        if not (1 <= int(row_b["capacity"] or 0) <= 16):
                            check("人数超界时表单里还是默认的 6 人（不是那一间写的 %s）" % row_b["capacity"],
                                  s2c["maxPlayers"] == DEFAULTS["maxPlayers"], str(s2c["maxPlayers"]))

        # —— 6. 那份预填只活过一次跳转：自己再进向导不该被旧数据改表单 ——————
        b.js("location.hash = '#/lobby'")
        b.wait("!!document.querySelector('.lobby-page')", tries=60, pause=0.3)
        b.js("location.hash = '#/room?tab=wizard'")
        b.wait("!!document.querySelector('.stepper')", tries=40, pause=0.3)
        d6 = wiz(b)
        check("直接进向导时没有旧预填偷偷改表单（收条不再出现）",
              d6 is not None and not d6["hasReceipt"], json.dumps(d6 or {}, ensure_ascii=False)[:200])

        # —— 7. 手机布局：那一行也得有这颗按钮，且不撑破 ——————————————
        b.set_viewport(*MOBILE)
        time.sleep(1.0)
        b.js("location.hash = '#/lobby'")
        b.wait("!!document.querySelector('.lobby-page .room-card')", tries=60, pause=0.4)
        m = b.js("""(() => {
          const p = document.querySelector('.lobby-page');
          const cards = [...p.querySelectorAll('.room-card')];
          const btns = cards.filter((c) => c.offsetParent !== null
                     && [...c.querySelectorAll('button')].some((x) => x.textContent.indexOf('照这间开服') >= 0));
          return {cards: cards.filter((c) => c.offsetParent !== null).length,
                  withBtn: btns.length,
                  copyBtn: cards.filter((c) => [...c.querySelectorAll('button')].some((x) => x.textContent.indexOf('复制连接') >= 0)).length,
                  overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth};
        })()""")
        check("手机卡片每一张都给「照这间开服」（不只给桌面表格）",
              m["cards"] > 0 and m["withBtn"] == m["cards"], json.dumps(m, ensure_ascii=False))
        check("手机上「复制连接」还在（两件事都在，没被挤掉）",
              m["copyBtn"] == m["cards"], json.dumps(m, ensure_ascii=False))
        check("手机上没有横向溢出", m["overflow"] <= 0, "溢出 %d px" % m["overflow"])
        b.shoot(os.path.join(SHOTS, "06_mobile.png"))
        b.set_viewport(*DESKTOP)

        d = wiz(b) or {}
        errs = b.js("(window.__errs || []).slice(0,6)") or []
        check("整轮下来页面没有抛错", not errs, " ; ".join(errs))
    finally:
        b.close()

    print("\n%d 条不过" % len(fails))
    for f in fails:
        print("  - " + f)
    print("截图在 %s" % SHOTS)
    return 1 if fails else 0


_CACHED = {}


def b_reply_rooms():
    if not _CACHED.get("rooms"):
        _CACHED["rooms"] = api_get("/api/v1/lobby/rooms")["rooms"]
    return _CACHED["rooms"]


if __name__ == "__main__":
    sys.exit(main())
