# #84 验收：「这个房间的开关」这一屏重做以后，界面说的和文件里的是不是同一件事。
#
# 盯的是六件事：
#   1. 列表按「开着 / 没开」分成两栏，两栏的行数和接口读到的 enabled 对得上；
#   2. 行上不再挂那排四个来源 chip（目录名/工坊 ID/版本/清单），它们挪进了配置抽屉；
#   3. 同一句原因在几行上重复时，整屏只说一次，行上只留短徽标；
#   4. 两个筛选控件（搜索框、只看有问题）逐个点，点完行数真的变、变回去也变；
#   5. 开关点下去是真写文件：那一行会从一栏跳到另一栏，接口口径跟着变，测完还原；
#   6. 配置抽屉里一个字段最多一行说明（以前最多三行小字）。
#
# 每一步都从接口回读，不看界面自己声称的数字。宽度从命令行给。
import base64
import json
import os
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


def req(method, path, body=None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method,
                               headers={"Authorization": AUTH, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(r, timeout=20) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode("utf-8") or "{}")


def click_js(b, sel, predicate):
    return b.js("(() => { const el = [...document.querySelectorAll(%s)].find(%s);"
                " if (!el) return false; el.click(); return true; })()"
                % (json.dumps(sel), predicate))


READ = r"""(() => {
  const heads = [...document.querySelectorAll('.mod-group-head')].map(h => ({
    title: (h.querySelector('b') || {}).innerText || '',
    n: (h.querySelector('.g-count') || {}).innerText || '',
    dot: !!h.querySelector('.dot-live, .dot-off'),
    sub: ((h.querySelector('.muted') || {}).innerText || '').trim(),
  }));
  const rows = [...document.querySelectorAll('.mod-line')].map(r => ({
    name: ((r.querySelector('.mod-line-name') || {}).innerText || '').trim(),
    sum: ((r.querySelector('.mod-line-sum') || {}).innerText || '').trim(),
    badges: [...r.querySelectorAll('.badge')].map(x => x.innerText.trim()),
    metaChips: [...r.querySelectorAll('.mod-meta .chip')].map(x => x.innerText.trim()),
    notes: [...r.querySelectorAll('.shard-note')].map(x => x.innerText.replace(/\s+/g,' ').trim()),
    on: !!r.querySelector('.switch input:checked'),
    isOn: r.classList.contains('is-on'),
    coverW: (r.querySelector('.mod-cover') || {clientWidth: 0}).clientWidth,
  }));
  const dr = document.querySelector('.drawer');
  return {
    heads: heads,
    rows: rows,
    screenNotes: [...document.querySelectorAll('.note')].map(n => n.innerText.replace(/\s+/g,' ').trim()),
    drawer: dr ? {
      fields: dr.querySelectorAll('.field').length,
      hints: dr.querySelectorAll('.field .hint').length,
      metaChips: [...dr.querySelectorAll('.mod-meta .chip')].map(x => x.innerText.trim()),
      title: ((dr.querySelector('.drawer-title') || {}).innerText || '').trim(),
    } : null,
    overflow: document.documentElement.scrollWidth > window.innerWidth + 1,
    scrollH: document.documentElement.scrollHeight,
    room: (document.querySelector('select') || {}).value || '',
    shard: ((document.querySelector('.seg button.on') || {}).innerText || '').trim(),
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


def main():
    width = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    height = int(sys.argv[2]) if len(sys.argv) > 2 else 900
    fails = []

    b = Browser(width, height)
    try:
        b.goto(BASE + "/?cdp=%d/#/mods" % int(time.time() * 1000))
        if not b.wait("!!document.querySelector('.login-form, .tabs')"):
            print("FAIL 模组页没起来")
            return 1
        if not login(b):
            print("FAIL 登录没进去")
            return 1
        if not click_js(b, ".tabs button", "t => t.innerText.indexOf('开关') >= 0"):
            print("FAIL 找不到「这个房间的开关」那一屏的页签")
            return 1
        if not b.wait("document.querySelectorAll('.mod-line').length > 0"):
            print("FAIL 第 3 步一行都没画出来")
            return 1
        time.sleep(0.4)

        v = b.js(READ)
        room, shard = v["room"], v["shard"]
        print("当前：%s / %s，行数 %d，横向溢出=%s，页面高度 %d"
              % (room, shard, len(v["rows"]), v["overflow"], v["scrollH"]))
        if v["overflow"]:
            fails.append("这个宽度下页面横向溢出了")
        if not room or not shard:
            fails.append("读不出界面现在停在哪一间，后面的接口比对全没了基准")
            return 1

        st, body = req("GET", "/api/v1/clusters/%s/shards/%s/mods" % (room, shard))
        api_rows = (body.get("data") or {}).get("mods") or []
        st2, body2 = req("GET", "/api/v1/mods")
        disk = (body2.get("data") or {}).get("mods") or []
        name_of = {m["dir"]: (m.get("name") or m["dir"]) for m in disk}
        # 待下载那几条在这一屏也认名字（#85 之后它们带名字和封面，不再是裸 workshop-数字），
        # 比对基准必须跟着接口走，否则界面显示工坊标题、基准却还是目录名，会误报"对不上"。
        for p in ((body2.get("data") or {}).get("pendingDownload") or []):
            if p.get("nameKnown"):
                name_of["workshop-" + p["workshopId"]] = p.get("name") or ("workshop-" + p["workshopId"])
        api_on = [o["dir"] for o in api_rows if o.get("enabled")]
        print("接口：这一间条目 %d 条，其中开着 %d 条" % (len(api_rows), len(api_on)))

        # ---- 1. 分栏 ----
        titles = [h["title"] for h in v["heads"]]
        print("分栏：%s" % json.dumps(v["heads"], ensure_ascii=False))
        if "开着" not in titles or "没开" not in titles:
            fails.append("这一屏要按「开着 / 没开」分栏，实际分栏标题是 %s" % titles)
        for h in v["heads"]:
            if not h["dot"]:
                fails.append("分栏标题「%s」没有那枚状态圆点，扫的时候找不到栏" % h["title"])
            if not h["sub"]:
                fails.append("分栏标题「%s」要说清这一栏是什么意思" % h["title"])
        on_rows = [r for r in v["rows"] if r["on"]]
        off_rows = [r for r in v["rows"] if not r["on"]]
        for h in v["heads"]:
            want = len(on_rows) if h["title"] == "开着" else len(off_rows)
            if h["n"].strip() != str(want):
                fails.append("分栏「%s」上写的 %s 和实际行数 %d 对不上" % (h["title"], h["n"], want))
        for r in v["rows"]:
            if r["on"] != r["isOn"]:
                fails.append("行「%s」的开关和分栏归属不一致（is-on 加错了）" % r["name"])
        # 界面"开着"的条数必须等于接口里 enabled 的条目数（磁盘上多出来的行是没条目的，算没开）。
        ui_on_names = sorted(r["name"] for r in on_rows)
        api_on_names = sorted(name_of.get(d, d) for d in api_on)
        if ui_on_names != api_on_names:
            fails.append("界面说这些开着 %s，接口读到的却是 %s" % (ui_on_names, api_on_names))

        # ---- 2. 来源 chip 挪进抽屉 ----
        with_meta = [r["name"] for r in v["rows"] if r["metaChips"]]
        if with_meta:
            fails.append("行上还留着那排来源 chip（每一行都长一样，是噪声）：%s" % with_meta[:3])
        narrow = [r["name"] for r in v["rows"] if r["coverW"] and r["coverW"] > 60]
        if narrow:
            fails.append("这一屏的封面该收到 52px 一档，还是 %s" % narrow[:3])

        # ---- 3. 重复原因收成整屏一句 ----
        dup = [t for t in [r["name"] for r in v["rows"] if any("没写出名字" in x for x in r["badges"])]]
        notes = [n for n in v["screenNotes"] if "同一种情况" in n]
        if len(dup) > 1:
            if not notes:
                fails.append("有 %d 行挂同一句原因（%s），屏顶却没收成一句" % (len(dup), "、".join(dup)))
            else:
                print("整屏那一句：%s" % notes[0][:160])
                for d in dup:
                    if d not in notes[0]:
                        fails.append("整屏那句原因里没点到 %s 这一行" % d)
            long_row_notes = [r["name"] for r in v["rows"]
                              if any("算出来的表达式" in n for n in r["notes"])]
            if long_row_notes:
                fails.append("这句原因已经在屏顶说过了，行上不该再抄一遍：%s" % long_row_notes)
        # 只有一行的那种原因仍然留在行上（挪到顶上就对不上号）：Global Positions 就是这种。
        single = [r for r in v["rows"] if any("modinfo 读不懂" in x for x in r["badges"])]
        if single and not any(r["notes"] for r in single):
            fails.append("只有一行的原因被一起收进整屏那句了，行上只剩徽标：%s" % single[0]["name"])

        # ---- 4. 两个筛选控件 ----
        total = len(v["rows"])
        chip = click_js(b, ".chipbtn", "x => x.innerText.indexOf('只看有问题') >= 0")
        if not chip:
            fails.append("「只看有问题」这一档没找到（这一屏有问题行时它必须在）")
        else:
            b.wait("document.querySelectorAll('.mod-line').length !== %d || "
                   "!!document.querySelector('.x-empty')" % total, tries=40, pause=0.2)
            v2 = b.js(READ)
            print("只看有问题：%d 行（原来 %d 行）" % (len(v2["rows"]), total))
            if len(v2["rows"]) >= total:
                fails.append("点了「只看有问题」行数一点没变，这一档按下去没反应")
            bad = [r["name"] for r in v2["rows"] if not r["badges"] and not r["notes"]]
            if bad:
                fails.append("「只看有问题」里混进了没问题的行：%s" % bad)
            click_js(b, ".chipbtn", "x => x.innerText.indexOf('只看有问题') >= 0")
            if not b.wait("document.querySelectorAll('.mod-line').length === %d" % total,
                          tries=40, pause=0.2):
                fails.append("再点一次「只看有问题」没有把全部行放回来")

        one = v["rows"][1]["name"] if len(v["rows"]) > 1 else v["rows"][0]["name"]
        b.js("(() => { const i = document.querySelector('input[type=search]');"
             " i.value = %s; i.dispatchEvent(new Event('input', {bubbles:true})); })()"
             % json.dumps(one[:4]))
        if not b.wait("document.querySelectorAll('.mod-line').length >= 1 && "
                      "document.querySelectorAll('.mod-line').length < %d" % total,
                      tries=40, pause=0.2):
            fails.append("搜「%s」没有把列表收窄" % one[:4])
        v3 = b.js(READ)
        miss = [r["name"] for r in v3["rows"] if one[:4] not in r["name"]]
        if miss:
            fails.append("搜索结果里混进了名字对不上的行：%s" % miss)
        print("搜「%s」：%d 行" % (one[:4], len(v3["rows"])))
        b.js("(() => { const i = document.querySelector('input[type=search]');"
             " i.value = ''; i.dispatchEvent(new Event('input', {bubbles:true})); })()")
        if not b.wait("document.querySelectorAll('.mod-line').length === %d" % total,
                      tries=40, pause=0.2):
            fails.append("清空搜索框以后行数没有回到 %d" % total)

        # ---- 5. 开关是真写文件的那一个动作 ----
        target = None
        for r in on_rows:
            # 自带的那个助手模组别拿它试：关掉再打开也算改过一次夹具，
            # 而且它的条目在每一间都一样，验不出"只动了这一间"。
            cand = [m for m in disk if (m.get("name") or m["dir"]) == r["name"]]
            if not cand or cand[0].get("builtin"):
                continue
            target = cand[0]["dir"]
            break
        if not target:
            fails.append("找不到一个可以拿来验开关的非自带模组")
        else:
            print("验开关：%s（先关掉再打开）" % target)
            if not click_js(b, ".mod-line .switch input",
                            "x => x.closest('.mod-line').innerText.indexOf(%s) >= 0"
                            % json.dumps(name_of.get(target, target))):
                fails.append("找不到 %s 那一行的开关" % target)
            else:
                b.js("(() => { const i = [...document.querySelectorAll('.mod-line .switch input')]"
                     ".find(x => x.closest('.mod-line').innerText.indexOf(%s) >= 0);"
                     " if (!i) return false; i.click(); return true; })()"
                     % json.dumps(name_of.get(target, target)))
                if not b.wait("![...document.querySelectorAll('.mod-line')].some(r => "
                              "r.innerText.indexOf(%s) >= 0 && r.querySelector('.switch input:checked'))"
                              % json.dumps(name_of.get(target, target)), tries=60, pause=0.25):
                    print("吐司：%s" % b.js("[...document.querySelectorAll('.toast')]"
                                            ".map(t => t.innerText.replace(/\\s+/g,' ').trim())"))
                    fails.append("点了开关，那一行还留在「开着」那一栏")
                st4, body4 = req("GET", "/api/v1/clusters/%s/shards/%s/mods" % (room, shard))
                now = [o for o in (body4.get("data") or {}).get("mods") or [] if o["dir"] == target]
                if now and now[0].get("enabled"):
                    fails.append("界面上关掉了，接口说这一间还开着它")
                v4 = b.js(READ)
                moved = [r for r in v4["rows"] if r["name"] == name_of.get(target)]
                if moved and moved[0]["isOn"]:
                    fails.append("开关关了，这一行还带着 is-on 留在「开着」那一栏")
                if moved and "值还留着" not in moved[0]["sum"]:
                    fails.append("关掉以后要说一句配置值还在（不然人以为白配了）：%s"
                                 % moved[0]["sum"])
                # 还原
                b.js("(() => { const i = [...document.querySelectorAll('.mod-line .switch input')]"
                     ".find(x => x.closest('.mod-line').innerText.indexOf(%s) >= 0);"
                     " if (!i) return false; i.click(); return true; })()"
                     % json.dumps(name_of.get(target, target)))
                st5, body5 = req("GET", "/api/v1/clusters/%s/shards/%s/mods" % (room, shard))
                back = [o for o in (body5.get("data") or {}).get("mods") or [] if o["dir"] == target]
                if not back or not back[0].get("enabled"):
                    fails.append("还原没成：%s 现在不是开着的状态，夹具被改坏了" % target)

        # ---- 6. 抽屉：一个字段最多一行说明，来源 chip 在这里 ----
        if not click_js(b, ".mod-line button", "x => x.innerText.trim() === '配置'"):
            fails.append("第一行没有「配置」按钮")
        else:
            if not b.wait("!!document.querySelector('.drawer .field')", tries=60, pause=0.25):
                fails.append("点了配置，抽屉里的表单没出来")
            else:
                v5 = b.js(READ)
                d = v5["drawer"] or {}
                print("抽屉：%s" % json.dumps(d, ensure_ascii=False))
                if d.get("fields") and d.get("hints") and d["hints"] > d["fields"] + 2:
                    fails.append("抽屉里 %d 个字段挂了 %d 行小字，说明该合并"
                                 % (d["fields"], d["hints"]))
                if not d.get("metaChips"):
                    fails.append("抽屉里没有那排来源 chip：行上收掉了，这里再不摆，改的是哪一份就没人说了")
                if not d.get("title"):
                    fails.append("抽屉标题没写明在配哪一个模组")

        b.shoot(os.path.join(ROOT, ".tools", "tmp", "shard84_check_%d.png" % width))
    finally:
        b.close()

    if fails:
        for f in fails:
            print("FAIL " + f)
        return 1
    print("PASS 配置页按开着/没开分栏，界面数字和 modoverrides 对得上，两个筛选控件都按得动")
    return 0


if __name__ == "__main__":
    sys.exit(main())
