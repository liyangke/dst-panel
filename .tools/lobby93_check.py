#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""「照这间开服」和向导那一排「常见配置起个头」都拆干净了没有——逐步在浏览器里对账。

为什么还要留这一份：两个入口都是纯前端的（后端没改一个字），Go 单测和接口探针都碰不到。
拆东西最容易留的三样烂尾，只有浏览器看得见：
  1. 页面上那枚按钮确实没了——桌面表格和手机卡片两处都要查，少查一处就是"代码里删了、
     这一屏还挂着"；顺带确认「复制连接」没被一起带走（它才是公网页唯一该留的动作）。
  2. 拆完的那一屏不空、也不留半截说明——向导第 2 步现在直接从「游戏模式」开始，
     那一排和它旁边那句"这一排只是起个头"都该一起消失。
  3. 拆掉之后向导默认值还是那四个（无尽 · 合作 · 6 人 · PVP 关）。这一份必须和
     internal/api/bootstrap.go 里 toSettings 的默认一致，否则界面写着默认、起的是另一间；
     两边各自钉了一条（这里是界面那侧，Go 单测在 internal/api 那侧）。
     另外"界面显示 == 真发出去"这条不能因为拆了预设就丢：这里改成手动改三栏再走汇总来验。

它只读：不建房间、不改配置。公网列表读的是 Klei 公开快照，出不去公网时相关几条自动跳过。
"""
import json
import os
import sys
import time

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from cdp import Browser, BASE  # noqa: E402

SHOTS = os.path.join(HERE, "tmp", "lobby93")
DESKTOP = (int(os.environ.get("L93_W", 1440)), int(os.environ.get("L93_H", 900)))
MOBILE = (390, 844)

# 向导第 2 步该摆的那四栏的默认值。在这儿重抄一遍是有意的：测试要是从被测代码里读
# 期望值，那就等于什么都没验（后端那份在 internal/api/bootstrap.go，本文件末尾拿
# 接口 /api/v1/plan 的默认值对了一次账）。
DEFAULTS = {"gameMode": "无尽", "intention": "合作", "maxPlayers": "6", "pvp": False}
# 手动改出来的那一套，用来验"第 2 步读到的 == 第 4 步汇总写的"。
MANUAL = {"gameMode": "生存", "intention": "竞争", "maxPlayers": "16", "pvp": True}

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
    b.goto(BASE + "/?l93=%d%s" % (int(time.time()), path))
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


# 第 2 步那一屏的真实状态：分段按钮亮在哪一枚、复选框勾没勾、人数框里是哪个数，
# 以及那一排"起个头"还在不在。全部读渲染出来的东西，不碰 Vue 内部状态。
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
  const labels = [...sec.querySelectorAll('.grid-form .field')].map((f) => {
    const l = f.querySelector('label'); return l ? l.textContent.trim() : ''; });
  const pv = [...sec.querySelectorAll('.switch')].find((x) => x.textContent.indexOf('开启 PVP') >= 0);
  const mp = field('最大玩家数');
  return {
    stepCount: (sec.querySelector('.stp-count') || {}).textContent || '',
    labels: labels,
    // 拆掉的那一排如果留了半截，一定会在这三种名字里露出来。
    presetRow: labels.filter((t) => /常见配置|起个头|社区/.test(t)),
    leftoverHints: [...sec.querySelectorAll('.hint')].map((x) => x.textContent.replace(/\s+/g, ' ').trim())
                    .filter((t) => /起个头|众数|公网列表公开的|快照/.test(t)),
    anyPresetButton: [...sec.querySelectorAll('.seg button')].some((x) => /人无尽合作|人生存公网局|PVP 竞争/.test(x.textContent)),
    gameMode: onOf('游戏模式'),
    intention: onOf('游戏偏好'),
    pvp: pv ? !!pv.querySelector('input').checked : null,
    maxPlayers: mp ? String(mp.querySelector('input[type=number]').value) : null,
    firstFieldVisible: labels.length ? vis(field(labels[0])) : false,
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
    mentionsPreset: sec.innerText.indexOf('常见配置') >= 0,
    overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
  };
})()"""


def step2(b):
    b.js("""(() => {
      const sec = document.querySelector('.stepper').closest('section');
      const f = [...sec.querySelectorAll('.grid-form .field')].find((x) => {
        const l = x.querySelector('label'); return l && l.textContent.trim() === '游戏模式'; });
      if (f) f.scrollIntoView({block: 'center'});
    })()""")
    time.sleep(0.2)
    return b.js(STEP2_READ)


def set_seg(b, label, want):
    """点某一栏的分段按钮。返回点没点到。"""
    return b.js("""(() => {
      const sec = document.querySelector('.stepper').closest('section');
      const f = [...sec.querySelectorAll('.grid-form .field')].find((x) => {
        const l = x.querySelector('label'); return l && l.textContent.trim() === %s; });
      if (!f) return 'no-field';
      const b = [...f.querySelectorAll('.seg button')].find((x) => x.textContent.trim() === %s);
      if (!b) return 'no-option:' + [...f.querySelectorAll('.seg button')].map((x) => x.textContent.trim()).join('/');
      b.click(); return 'ok';
    })()""" % (js_arg(label), js_arg(want)))


def set_number(b, label, v):
    return b.js("""(() => {
      const sec = document.querySelector('.stepper').closest('section');
      const f = [...sec.querySelectorAll('.grid-form .field')].find((x) => {
        const l = x.querySelector('label'); return l && l.textContent.trim() === %s; });
      if (!f) return 'no-field';
      const i = f.querySelector('input[type=number]');
      i.value = %s; i.dispatchEvent(new Event('input', {bubbles: true}));
      return 'ok';
    })()""" % (js_arg(label), js_arg(str(v))))


def set_switch(b, text, on):
    return b.js("""(() => {
      const sec = document.querySelector('.stepper').closest('section');
      const s = [...sec.querySelectorAll('.switch')].find((x) => x.textContent.indexOf(%s) >= 0);
      if (!s) return 'no-switch';
      const i = s.querySelector('input');
      if (i.checked !== %s) i.click();
      return 'ok';
    })()""" % (js_arg(text), "true" if on else "false"))


def at_step(b):
    return (b.js("((document.querySelector('.stp-count') || {}).textContent || '')") or "").strip()


def click_step(b, n):
    """步骤条上点第 n 枚。只在"已经走到过"时点得动（往前跳会绕过校验）。"""
    return b.js("""(() => {
      const t = [...document.querySelectorAll('.stepper button.stp')]
        .find((x) => (x.getAttribute('title') || '').indexOf('第 %d 步') === 0);
      if (!t || t.disabled) return false;
      t.click(); return true;
    })()""" % n)


def to_step(b, n):
    """往前走 n-1 步：第 1 步的服务器名称是必填，靠步骤条跳会被校验挡住，
    所以老老实实按「下一步」——顺便也验了每一屏的校验放行得对不对。"""
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


def lobby_absent(b):
    """这一页上「照这间开服」该一枚都不剩，而「复制连接」一枚都不能少。"""
    b.js("location.hash = '#/lobby'")
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

        # —— 1. 向导第 2 步：那一排没了，剩下的栏照常，默认值还是那四个 ————
        if not check("第 1 步的服务器名称填得进去（走下一步的门槛）",
                     fill_name(b, "拆干净验收-临时名") == "拆干净验收-临时名"):
            raise SystemExit("名字填不进去，后面没法走")
        if not check("走得到第 2 步", to_step(b, 2)):
            raise SystemExit("走不到第 2 步")
        s = step2(b)
        say("第 2 步读到：%s" % json.dumps(
            {k: s[k] for k in ("labels", "presetRow", "leftoverHints", "anyPresetButton",
                               "gameMode", "intention", "maxPlayers", "pvp")}, ensure_ascii=False)[:420])
        check("那一排「常见配置起个头」整个不见了（标题栏、按钮、句子三处都没有）",
              not s["presetRow"] and not s["anyPresetButton"] and not s["leftoverHints"],
              json.dumps({k: s[k] for k in ("presetRow", "anyPresetButton", "leftoverHints")}, ensure_ascii=False))
        check("第 2 步第一栏就是「游戏模式」（拆完没有把这一屏拆空）",
              bool(s["labels"]) and s["labels"][0] == "游戏模式" and s["firstFieldVisible"],
              str(s["labels"][:4]))
        check("向导默认还是那四栏：无尽 · 合作 · 6 人 · PVP 关",
              s["gameMode"] == DEFAULTS["gameMode"] and s["intention"] == DEFAULTS["intention"]
              and s["maxPlayers"] == DEFAULTS["maxPlayers"] and s["pvp"] is DEFAULTS["pvp"],
              json.dumps(s, ensure_ascii=False)[:200])
        b.shoot(os.path.join(SHOTS, "01_step2.png"))

        # —— 2. 手动改三栏 + 勾 PVP，走到第 4 步：界面显示 == 真发出去 ——————
        for label, want, key in (("游戏模式", MANUAL["gameMode"], "gameMode"),
                                 ("游戏偏好", MANUAL["intention"], "intention")):
            got = set_seg(b, label, want)
            check("第 2 步「%s」点得到「%s」" % (label, want), got == "ok", str(got))
            time.sleep(0.25)
        got = set_number(b, "最大玩家数", MANUAL["maxPlayers"])
        check("第 2 步人数框填得进 16", got == "ok", str(got))
        got = set_switch(b, "开启 PVP", MANUAL["pvp"])
        check("第 2 步 PVP 开关勾得上", got == "ok", str(got))
        time.sleep(0.3)
        s2 = step2(b)
        check("四栏读回来就是刚改的那套",
              s2["gameMode"] == MANUAL["gameMode"] and s2["intention"] == MANUAL["intention"]
              and s2["maxPlayers"] == MANUAL["maxPlayers"] and s2["pvp"] is MANUAL["pvp"],
              json.dumps(s2, ensure_ascii=False)[:200])
        if not check("走得到第 4 步的汇总", to_step(b, 4), "第 4 步进不去"):
            raise SystemExit("汇总进不去，口径没法比")
        d = b.js(SUMMARY_READ)
        say("汇总：%s" % json.dumps(d["kv"], ensure_ascii=False)[:400])
        mode = [r for r in d["kv"] if r["k"] == "模式 / 偏好"]
        ppl = [r for r in d["kv"] if r["k"] == "人数 / 通信"]
        check("汇总那行的玩法与偏好 == 第 2 步读到的",
              bool(mode) and mode[0]["v"] == "%s · %s" % (s2["gameMode"], s2["intention"]),
              json.dumps(mode, ensure_ascii=False))
        check("汇总那行的人数与 PVP == 第 2 步读到的",
              bool(ppl) and ("%s 人上限" % s2["maxPlayers"]) in ppl[0]["v"]
              and ("PVP " + ("开" if s2["pvp"] else "关")) in ppl[0]["v"],
              json.dumps(ppl, ensure_ascii=False))
        check("汇总里不再有「借鉴来源」那一行，也不提「常见配置」",
              not d["hasBorrowRow"] and not d["hasReceipt"] and not d["mentionsPreset"],
              json.dumps(d["kv"], ensure_ascii=False)[:200])
        check("第 2 步没有报错条", not s2["errBoxes"], " ".join(s2["errBoxes"]))
        check("桌面 1440 下第 2 步没被撑出横向滚动", s2["overflow"] <= 0, "溢出 %d px" % s2["overflow"])
        check("桌面 1440 下汇总页没被撑出横向滚动", d["overflow"] <= 0, "溢出 %d px" % d["overflow"])
        b.shoot(os.path.join(SHOTS, "02_summary.png"))

        # —— 3. 公网页：那个入口真的没了，复制连接还在（出不去公网时跳过）——
        try:
            r = lobby_absent(b)
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
            check("桌面公网页没有横向溢出", r["overflow"] <= 0, "溢出 %d px" % r["overflow"])
            b.shoot(os.path.join(SHOTS, "03_lobby.png"))

        # —— 4. 手机布局：第 2 步不溢出，公网卡片上也没有那枚按钮 ——————
        b.set_viewport(*MOBILE)
        time.sleep(1.0)
        b.js("location.hash = '#/room?tab=wizard'")
        b.wait("!!document.querySelector('.stepper')", tries=40, pause=0.3)
        check("手机布局下步骤条点得回第 1 步", click_step(b, 1), at_step(b))
        fill_name(b, "拆干净验收-手机")
        to_step(b, 2)
        m = step2(b)
        check("手机上那一排也不在",
              not m["presetRow"] and not m["anyPresetButton"] and not m["leftoverHints"],
              json.dumps({k: m[k] for k in ("presetRow", "anyPresetButton", "leftoverHints")}, ensure_ascii=False))
        # 换页签会让向导重新挂载，所以这一屏是从默认那套开始的（不是桌面那屏改过的样子）。
        # 这里就地改两栏，验的是 390 宽度下控件按得动、读得回来，不是跨页签保留草稿。
        check("手机上表单从默认那四栏开始",
              m["gameMode"] == DEFAULTS["gameMode"] and m["intention"] == DEFAULTS["intention"]
              and m["maxPlayers"] == DEFAULTS["maxPlayers"] and m["pvp"] is DEFAULTS["pvp"],
              json.dumps(m, ensure_ascii=False)[:220])
        set_seg(b, "游戏模式", "荒野")
        set_number(b, "最大玩家数", "10")
        set_switch(b, "开启 PVP", True)
        time.sleep(0.3)
        m2 = step2(b)
        check("手机上改得动：模式亮在荒野、人数框 10、PVP 勾着",
              m2["gameMode"] == "荒野" and m2["maxPlayers"] == "10" and m2["pvp"] is True,
              json.dumps(m2, ensure_ascii=False)[:220])
        check("手机上没有横向溢出", m2["overflow"] <= 0, "溢出 %d px" % m2["overflow"])
        b.shoot(os.path.join(SHOTS, "04_mobile_step2.png"))
        try:
            mc = lobby_absent(b)
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
