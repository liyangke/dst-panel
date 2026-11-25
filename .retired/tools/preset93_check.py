#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""开服向导那一排「常见配置起个头」+ 公网页拆掉「照这间开服」，逐步在浏览器里对账。

为什么要单独跑这一遍：这一排是纯前端的（后端没改一个字），Go 单测和接口探针都碰不到它。
它最容易错的四件事只有浏览器能验：
  1. 按下去到底改没改表单——不是"我以为 @click 收到了"，而是第 2 步那颗分段按钮真亮在
     「生存」上、人数框里真是 16、PVP 那个复选框真是勾着的；
  2. 界面显示的数字和真发出去的是不是一致——点完第三枚，第 4 步的汇总必须写 8 人、
     PVP 开，而不是还留着上一枚那一份；
  3. 哪一枚处于"已经套上"的状态——三枚都灰着，用户会反复按或以为坏了；全亮着也一样；
  4. 拆掉的那个入口是真的没了：桌面表格和手机卡片两处都查，顺带确认「复制连接」没被
     一起带走（它才是这一页唯一该留的动作）。

它只读：不建房间、不改配置。公网列表读的是 Klei 公开快照，出不去公网时那两条自动跳过。
"""
import json
import os
import sys
import time

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from cdp import Browser, BASE  # noqa: E402

SHOTS = os.path.join(HERE, "tmp", "preset93")
DESKTOP = (int(os.environ.get("P93_W", 1440)), int(os.environ.get("P93_H", 900)))
MOBILE = (390, 844)

# 这一排三枚，取值必须和 views/room.js 的 PRESETS 一字不差。
# 在这儿重抄一遍是有意的：测试要是从被测代码里读期望值，那就等于什么都没验。
PRESETS = [
    {"label": "6 人无尽合作", "gameMode": "无尽", "intention": "合作", "maxPlayers": "6", "pvp": False},
    {"label": "16 人生存公网局", "gameMode": "生存", "intention": "合作", "maxPlayers": "16", "pvp": False},
    {"label": "8 人 PVP 竞争", "gameMode": "无尽", "intention": "竞争", "maxPlayers": "8", "pvp": True},
]

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


def login(b, path):
    b.goto(BASE + "/?p93=%d%s" % (int(time.time()), path))
    if b.wait("!!document.querySelector('.login-form')", tries=40, pause=0.3):
        b.js("""(() => {
          const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})); };
          const f = document.querySelector('.login-form');
          set(f.querySelector('input[type=text], input:not([type])'), 'admin');
          set(f.querySelector('input[type=password]'), 'admin');
          sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}));
          f.querySelector('.login-submit').click();
        })()""")
    if not b.wait("!!document.querySelector('.stepper')", tries=60, pause=0.3):
        b.shoot(os.path.join(SHOTS, "boot_failed.png"))
        raise SystemExit("没进到账本向导：hash=%s 正文=%s" % (
            b.js("location.hash"), (b.js("document.body.innerText.slice(0,200)") or "")))
    b.js("window.__errs = [];"
         "addEventListener('error', (e) => window.__errs.push(String(e.message || e.error)));"
         "addEventListener('unhandledrejection', (e) => window.__errs.push('promise:' + String(e.reason)));")


# 第 2 步那一屏的真实状态：分段按钮亮在哪一枚、复选框勾没勾、人数框里是哪个数、
# 还有那一排"起个头"里哪一枚处于已套上。全部读渲染出来的东西，不碰 Vue 内部状态。
STEP2_READ = r"""(() => {
  const sec = document.querySelector('.stepper').closest('section');
  const vis = (el) => !!(el && el.offsetParent !== null);
  const field = (label) => [...sec.querySelectorAll('.grid-form .field')].find((f) => {
    const l = f.querySelector('label');
    return l && l.textContent.trim() === label;
  });
  const onOf = (label) => { const f = field(label);
    if (!f) return null; const b = [...f.querySelectorAll('.seg button')].find((x) => x.classList.contains('on'));
    return b ? b.textContent.trim() : ''; };
  const row = field('常见配置起个头');
  const chips = row ? [...row.querySelectorAll('.seg button')] : [];
  const pv = [...sec.querySelectorAll('.switch')].find((x) => x.textContent.indexOf('开启 PVP') >= 0);
  const mp = field('最大玩家数');
  const note = row ? [...row.querySelectorAll('.hint')].map((x) => x.textContent.replace(/\s+/g, ' ').trim()) : [];
  return {
    stepCount: (sec.querySelector('.stp-count') || {}).textContent || '',
    hasRow: !!row,
    chips: chips.map((x) => x.textContent.trim()),
    chipsVisible: chips.map((x) => vis(x)),
    chipOn: chips.filter((x) => x.classList.contains('on')).map((x) => x.textContent.trim()),
    chipDisabled: chips.map((x) => !!x.disabled),
    gameMode: onOf('游戏模式'),
    intention: onOf('游戏偏好'),
    pvp: pv ? !!pv.querySelector('input').checked : null,
    maxPlayers: mp ? String(mp.querySelector('input[type=number]').value) : null,
    note: note.join(' / '),
    hintLabels: row ? [...row.querySelectorAll('.tip-btn')].map((x) => x.textContent.replace(/\s+/g, ' ').trim()) : [],
    errBoxes: [...sec.querySelectorAll('.err-box')].map((x) => x.textContent.trim()),
    overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    pageErrors: (window.__errs || []).slice(0, 6),
  };
})()"""

# 第 4 步汇总里的那几行（.kv），用来把"界面显示的"和"将要发出去的"对上。
SUMMARY_READ = r"""(() => {
  const sec = document.querySelector('.stepper').closest('section');
  const rowText = (t) => (t || '').replace(/\s+/g, ' ').trim();
  return {
    stepCount: (sec.querySelector('.stp-count') || {}).textContent || '',
    kv: [...sec.querySelectorAll('.kv dt')].map((dt) => ({
      k: rowText(dt.textContent),
      v: rowText((dt.nextElementSibling || {}).textContent || ''),
    })),
    hasBorrowRow: [...sec.querySelectorAll('.kv dt')].some((dt) => dt.textContent.indexOf('借鉴来源') >= 0),
    hasReceipt: sec.innerText.indexOf('这一份是照公网那一间') >= 0,
    overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
  };
})()"""

CLICK_CHIP = r"""(() => {
  const sec = document.querySelector('.stepper').closest('section');
  const f = [...sec.querySelectorAll('.grid-form .field')].find((x) => {
    const l = x.querySelector('label'); return l && l.textContent.trim() === '常见配置起个头';
  });
  if (!f) return 'no-row';
  const b = [...f.querySelectorAll('.seg button')].find((x) => x.textContent.trim() === %s);
  if (!b) return 'no-chip:' + [...f.querySelectorAll('.seg button')].map((x) => x.textContent.trim()).join('/');
  b.click();
  return 'ok';
})()"""

STEP2_MARK = r"""(() => {
  const sec = document.querySelector('.stepper').closest('section');
  const row = [...sec.querySelectorAll('.grid-form .field')].find((f) => {
    const l = f.querySelector('label'); return l && l.textContent.trim() === '常见配置起个头'; });
  if (!row) return null;
  row.setAttribute('data-p93', 'preset-row');
  return [...row.querySelectorAll('.seg button')].map((x) => x.textContent.trim());
})()"""


def step2(b):
    b.js(STEP2_MARK)
    # 把那一排滚进视野再读：截图要拍的就是这一排，页面停在顶上时拍出来的是别的东西，
    # 而"看着像通过了"的截图比没有截图更糟。
    b.js("""(() => {
      const row = document.querySelector('[data-p93=preset-row]');
      if (row) row.scrollIntoView({block: 'center'});
    })()""")
    time.sleep(0.2)
    return b.js(STEP2_READ)


def click_chip(b, label):
    return b.js(CLICK_CHIP % js_arg(label))


def at_step(b):
    return (b.js("((document.querySelector('.stp-count') || {}).textContent || '')") or "").strip()


def click_step(b, n):
    """步骤条上点第 n 枚。只在"已经走到过"时点得动（往前跳会绕过校验），
    所以这一枚用来从第 4 步退回第 1 步，而不是用来跳步。"""
    return b.js("""(() => {
      const t = [...document.querySelectorAll('.stepper button.stp')]
        .find((x) => (x.getAttribute('title') || '').indexOf('第 %d 步') === 0);
      if (!t || t.disabled) return false;
      t.click(); return true;
    })()""" % n)


def to_step(b, n):
    """往前走 n-1 步：第 1 步的服务器名称是必填，靠步骤条跳会被校验挡住，
    所以老老实实按「下一步」——顺便也验了每一屏的校验放行放行得对不对。"""
    for _ in range(n * 2):
        if at_step(b).startswith("第 %d " % n) or at_step(b).startswith("第%d" % n):
            return True
        moved = b.js("""(() => {
          const t = [...document.querySelectorAll('button')].find((x) => /^(下一步)/.test(x.textContent.trim()) && !x.disabled);
          if (!t) return false; t.click(); return true;
        })()""")
        if not moved:
            return False
        time.sleep(0.35)
    return False


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


def open_hint(b):
    """悬停那种要真鼠标，这里用 focus：x-hint 同时听 @focus，程序化聚焦同样能把气泡打开。"""
    b.js("""(() => {
      const row = document.querySelector('[data-p93=preset-row]');
      const t = row && row.querySelector('.tip-btn');
      if (t) t.focus();
    })()""")
    time.sleep(0.35)
    return b.js("""(() => {
      const row = document.querySelector('[data-p93=preset-row]');
      const b = row && row.querySelector('.tip-bub');
      return b ? b.textContent.replace(/\\s+/g, ' ').trim() : '';
    })()""") or ""


def lobby_absent(b, path, where):
    """这一页上「照这间开服」该一枚都不剩，而「复制连接」一枚都不能少。"""
    b.js("location.hash = '%s'" % path)
    if not b.wait("!!document.querySelector('.lobby-page table.data tbody tr, .lobby-page .room-card')",
                  tries=160, pause=0.5):
        return None
    time.sleep(0.6)
    return b.js("""(() => {
      const p = document.querySelector('.lobby-page');
      const btns = [...p.querySelectorAll('button')].map((x) => x.textContent.replace(/\\s+/g, ' ').trim());
      const titled = [...p.querySelectorAll('[title]')].filter((x) => x.title.indexOf('现在跑到第几季') >= 0);
      return {
        borrow: btns.filter((t) => t.indexOf('照这间开服') >= 0).length,
        copy: btns.filter((t) => t.indexOf('复制连接') >= 0 || t === '已复制').length,
        seasonTitled: titled.length,
        rows: p.querySelectorAll('table.data tbody tr').length,
        cards: [...p.querySelectorAll('.room-card')].filter((c) => c.offsetParent !== null).length,
        overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      };
    })()""")


def main():
    os.makedirs(SHOTS, exist_ok=True)
    b = Browser(*DESKTOP)
    try:
        login(b, "#/room?tab=wizard")

        # —— 1. 第 2 步顶部那一排存在，三枚都在，默认那枚亮着 ——————————————————
        if not check("第 1 步的服务器名称填得进去（走下一步的门槛）",
                     fill_name(b, "常见配置验收-临时名") == "常见配置验收-临时名"):
            raise SystemExit("名字填不进去，后面没法走")
        if not check("走得到第 2 步", to_step(b, 2)):
            raise SystemExit("走不到第 2 步")
        s = step2(b)
        say("那一排读到：%s" % json.dumps(
            {k: s[k] for k in ("hasRow", "chips", "chipOn", "gameMode", "intention", "maxPlayers", "pvp")},
            ensure_ascii=False))
        if not check("第 2 步顶上摆出了「常见配置起个头」那一排", s["hasRow"], json.dumps(s, ensure_ascii=False)[:200]):
            raise SystemExit("那一排没有，后面每一条都无从比起")
        check("三枚都在，且文案就是那三句", s["chips"] == [p["label"] for p in PRESETS],
              "界面 %s｜该是 %s" % (s["chips"], [p["label"] for p in PRESETS]))
        check("三枚都渲染出来、没有一枚是灰的（按下去必须有反应）",
              all(s["chipsVisible"]) and not any(s["chipDisabled"]), json.dumps(s["chipsVisible"]))
        check("向导的默认值就是第一枚那套（第一枚一开始就亮着）",
              s["chipOn"] == [PRESETS[0]["label"]],
              "亮着 %s｜第一枚 %s" % (s["chipOn"], PRESETS[0]["label"]))
        check("没套的时候那四栏确实是向导默认（无尽 · 合作 · 6 人 · PVP 关）",
              s["gameMode"] == "无尽" and s["intention"] == "合作"
              and s["maxPlayers"] == "6" and s["pvp"] is False, json.dumps(s, ensure_ascii=False)[:180])
        check("说清了边界：世界大小、资源、Boss 这些不在公网那份数据里",
              "世界大小" in s["note"] and "季节" in s["note"], s["note"][:220])
        check("那一排边上有一句实话（不是只摆三枚按钮不给说明）", len(s["note"]) > 40, s["note"][:80])
        b.shoot(os.path.join(SHOTS, "01_row_default.png"))

        # —— 2. 那枚带字的说明按得开，里面写的是来源 ——————————————————————
        check("「这三套是哪来的」那枚小按钮找得到", bool(s["hintLabels"]), str(s["hintLabels"]))
        bub = open_hint(b)
        say("气泡里：%s" % bub[:180])
        check("气泡点名了来源（Klei 亚太那份公开快照）", "Klei" in bub and "亚太" in bub, bub[:180])
        check("气泡写明了只改哪四栏", "PVP" in bub and "最大玩家数" in bub, bub[:180])

        # —— 3. 逐枚按下去：表单真的跟着变，亮的那枚也跟着变 ——————————————
        for i, want in enumerate(PRESETS):
            got = click_chip(b, want["label"])
            if not check("第 %d 枚「%s」按得到" % (i + 1, want["label"]), got == "ok", str(got)):
                continue
            time.sleep(0.3)
            st = step2(b)
            say("按「%s」之后：%s" % (want["label"], json.dumps(st, ensure_ascii=False)[:180]))
            check("按「%s」之后游戏模式亮在「%s」上" % (want["label"], want["gameMode"]),
                  st["gameMode"] == want["gameMode"], str(st["gameMode"]))
            check("按「%s」之后游戏偏好亮在「%s」上" % (want["label"], want["intention"]),
                  st["intention"] == want["intention"], str(st["intention"]))
            check("按「%s」之后最大玩家数框里真是 %s" % (want["label"], want["maxPlayers"]),
                  st["maxPlayers"] == want["maxPlayers"], str(st["maxPlayers"]))
            check("按「%s」之后 PVP 复选框真是%s" % (want["label"], "勾着的" if want["pvp"] else "空着的"),
                  st["pvp"] is want["pvp"], str(st["pvp"]))
            check("亮着的那一枚就是刚按的（状态跟着表单走，不是一排永远亮着）",
                  st["chipOn"] == [want["label"]], json.dumps(st["chipOn"], ensure_ascii=False))
            b.shoot(os.path.join(SHOTS, "02_chip%d.png" % (i + 1)))

        # —— 4. 自己改一栏，那一枚就该灭（不能挂着"已套上"骗人）——————————
        b.js("""(() => {
          const sec = document.querySelector('.stepper').closest('section');
          const f = [...sec.querySelectorAll('.grid-form .field')].find((x) => {
            const l = x.querySelector('label'); return l && l.textContent.trim() === '最大玩家数';
          });
          const i = f.querySelector('input[type=number]');
          i.value = '10'; i.dispatchEvent(new Event('input', {bubbles: true}));
        })()""")
        time.sleep(0.3)
        s = step2(b)
        check("把人数手动改成 10 之后，那一枚不再显示已套上",
              s["chipOn"] == [], json.dumps(s["chipOn"], ensure_ascii=False) + "｜" + str(s["maxPlayers"]))

        # —— 5. 界面显示 == 真发出去：套第三枚，走到汇总对那两行 ————————
        click_chip(b, PRESETS[2]["label"])
        time.sleep(0.3)
        s3 = step2(b)
        if not check("走得到第 4 步的汇总", to_step(b, 4), "第 4 步进不去"):
            raise SystemExit("汇总进不去，口径没法比")
        d = b.js(SUMMARY_READ)
        say("汇总：%s" % json.dumps(d["kv"], ensure_ascii=False)[:400])
        mode = [r for r in d["kv"] if r["k"] == "模式 / 偏好"]
        ppl = [r for r in d["kv"] if r["k"] == "人数 / 通信"]
        check("汇总那行的玩法与偏好 == 第 2 步读到的",
              bool(mode) and mode[0]["v"] == "%s · %s" % (s3["gameMode"], s3["intention"]),
              json.dumps(mode, ensure_ascii=False))
        check("汇总那行的人数与 PVP == 第 2 步读到的",
              bool(ppl) and ("%s 人上限" % s3["maxPlayers"]) in ppl[0]["v"]
              and ("PVP " + ("开" if s3["pvp"] else "关")) in ppl[0]["v"],
              json.dumps(ppl, ensure_ascii=False))
        check("汇总里不再有「借鉴来源」那一行（拆掉的入口不在这里留个鬼影）",
              not d["hasBorrowRow"] and not d["hasReceipt"], json.dumps(d["kv"], ensure_ascii=False)[:200])
        check("第 4 步没有报错条", not s["errBoxes"], " ".join(s["errBoxes"]))
        check("桌面 1440 下这一排没把页面撑出横向滚动", d["overflow"] <= 0, "溢出 %d px" % d["overflow"])
        b.shoot(os.path.join(SHOTS, "03_summary.png"))

        # —— 6. 公网页：那个入口真的没了，复制连接还在（出不去公网时跳过）——
        try:
            r = lobby_absent(b, "#/lobby", "桌面表格")
        except Exception as e:
            r = None
            say("桌面公网页没读出来（这台机器出不去公网就跳过这两条）：%s" % str(e)[:120])
        if r is None:
            say("桌面公网页没有行，跳过入口检查")
        else:
            check("桌面公网页上「照这间开服」一枚都不剩", r["borrow"] == 0, json.dumps(r, ensure_ascii=False))
            check("桌面公网页每行还有「复制连接」（唯一该留的动作没被一起带走）",
                  r["rows"] > 0 and r["copy"] >= r["rows"], json.dumps(r, ensure_ascii=False))
            check("季节那一格带着「这不是开局设定」那句说明",
                  r["seasonTitled"] > 0, json.dumps(r, ensure_ascii=False))

        # —— 7. 手机布局：那一排可见可点，公网卡片上也没有那枚按钮 ————
        b.set_viewport(*MOBILE)
        time.sleep(1.0)
        b.js("location.hash = '#/room?tab=wizard'")
        b.wait("!!document.querySelector('.stepper')", tries=40, pause=0.3)
        # 上一段已经走到第 4 步；这里先退回第 1 步再重新走一遍，验的是同一排在手机宽度下的样子。
        check("手机布局下步骤条点得回第 1 步", click_step(b, 1), at_step(b))
        fill_name(b, "常见配置验收-手机")
        to_step(b, 2)
        m = step2(b)
        check("手机上那一排也在、三枚都渲染出来",
              m["hasRow"] and len(m["chips"]) == 3 and all(m["chipsVisible"]), json.dumps(m, ensure_ascii=False)[:220])
        check("手机上没有横向溢出", m["overflow"] <= 0, "溢出 %d px" % m["overflow"])
        got = click_chip(b, PRESETS[1]["label"])
        m2 = step2(b) if got == "ok" else m
        check("手机上按得动第二枚，人数框真的变 16",
              got == "ok" and m2["maxPlayers"] == "16" and m2["chipOn"] == [PRESETS[1]["label"]],
              "%s｜%s" % (got, json.dumps(m2, ensure_ascii=False)[:180]))
        b.shoot(os.path.join(SHOTS, "04_mobile_row.png"))
        try:
            mc = lobby_absent(b, "#/lobby", "手机卡片")
        except Exception:
            mc = None
        if mc is None:
            say("手机公网页没有卡片，跳过")
        else:
            check("手机卡片上也没有「照这间开服」", mc["borrow"] == 0, json.dumps(mc, ensure_ascii=False))
            check("手机卡片每张都还有「复制连接」", mc["cards"] > 0 and mc["copy"] >= mc["cards"],
                  json.dumps(mc, ensure_ascii=False))
            check("手机卡片上的季节那一枚也带着那句说明", mc["seasonTitled"] > 0,
                  json.dumps(mc, ensure_ascii=False))
            check("手机上公网页没有横向溢出", mc["overflow"] <= 0, "溢出 %d px" % mc["overflow"])
            b.shoot(os.path.join(SHOTS, "05_mobile_lobby.png"))
        b.set_viewport(*DESKTOP)

        errs = b.js("(window.__errs || []).slice(0,6)") or []
        check("整轮下来页面没有抛错", not errs, " ; ".join(errs))
    finally:
        b.close()

    print("\n%d 条不过" % len(fails))
    for f in fails:
        print("  - " + f)
    print("截图在 %s" % SHOTS)
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
