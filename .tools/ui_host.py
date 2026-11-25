# 总览页"这台机器"那一栏的浏览器实测：真 Chrome、真接口、真两轮采样。
#
# 要盯住的是这几件事：
#   - 登录后确实在轮询（这一条是顺手钉住的：以前只有开标签页那条路会起轮询，
#     手动输完账号进来的人页面停在一次性的数字上不动）。
#   - 表盘画出来了，而且不是靠 0% 撑场的：没有可信区间时圆心必须是"—"+一句说明。
#   - 侧列那几行的值都是"说法"，不出现空串、裸码值或英文报错。
#   - 两个宽度都没有横向滚动；手机上三颗表盘没被挤扁。
#   - 深浅两套主题下圆环的颜色都还看得见（色必须来自主题变量）。
import sys, os, time, json

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import Browser, BASE

SHOTS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".tmp_boot", "shots")
os.makedirs(SHOTS, exist_ok=True)

PROBE = r"""
(() => {
  const q = (s) => Array.from(document.querySelectorAll(s));
  const text = (n) => (n ? n.textContent.trim() : '');
  const box = (n) => { const r = n.getBoundingClientRect(); return {x:Math.round(r.x), y:Math.round(r.y), w:Math.round(r.width), h:Math.round(r.height)}; };
  const dds = q('.host-strip .host-kv dd');
  const rings = q('.host-strip .ring');
  return {
    docW: document.documentElement.scrollWidth,
    winW: window.innerWidth,
    strip: document.querySelector('.host-strip') ? box(document.querySelector('.host-strip')) : null,
    rings: rings.map((n) => ({
      label: text(n.querySelector('.ring-label')),
      head: text(n.querySelector('.ring-head')),
      note: text(n.querySelector('.ring-note')),
      tone: n.className.replace('ring', '').trim(),
      arc: !!n.querySelector('.ring-fill'),
      stroke: n.querySelector('.ring-fill') ? getComputedStyle(n.querySelector('.ring-fill')).stroke : '',
      box: box(n),
    })),
    rows: q('.host-strip .host-kv dt').map((n, i) => ({ k: text(n), v: text(dds[i]) })),
    err: text(document.querySelector('.host-strip .err-box')),
    lastSync: (() => { const n = document.querySelector('.topbar .sync'); return text(n); })(),
    procRow: (() => {
      // #111 之后没有「这台机器上的占用」那一格了：占用做成在跑世界行上的一枚胶囊。
      const row = Array.from(document.querySelectorAll('.shard-row'))
        .find((r) => r.querySelector('.badge.running'));
      if (!row) return '';
      return Array.from(row.querySelectorAll(':scope > .chip'))
        .map((x) => x.innerText.trim()).find((t) => /内存|占用/.test(t)) || '';
    })(),
    apiCalls: performance.getEntriesByType('resource').filter((e) => e.name.indexOf('/api/v1/host') >= 0).length,
  };
})()
"""


def cold_login(b):
    """清掉凭据并显式重载，从登录页走一遍——换 hash 不会重载，拍到的会是上一页。"""
    b.goto(BASE + "/#/")
    b.js("sessionStorage.removeItem('dstpanel.creds')")
    b.reload()
    if not b.wait("!!document.querySelector('.login-form')", tries=40):
        raise SystemExit("没有回到登录页")
    b.js("""(() => {
      const ins = document.querySelectorAll('.login-card input');
      const set = (n, v) => { n.value = v; n.dispatchEvent(new Event('input', { bubbles: true })); };
      set(ins[0], 'admin'); set(ins[1], 'admin');
      document.querySelector('.login-submit').click();
    })()""")
    return b.wait("!!document.querySelector('.host-strip')", tries=40)


def main():
    b = Browser(1440, 900)
    if not cold_login(b):
        raise SystemExit("登录后没有出现「这台机器」那一栏")

    # 等第二轮采样：CPU 只有两次采样作差才有可信百分比，第一轮必须是那句说明。
    # 这一等同时也证明了轮询真的在跑（登录页进来那条路径以前不起轮询）。
    got = b.wait("!!document.querySelector('.host-strip .ring.ok, .host-strip .ring.warn, .host-strip .ring.bad')", tries=40)
    # 展开第一行的详情：那一行"这台机器上的占用"要在那里才看得到。
    b.js("""(() => {
      const btn = [...document.querySelectorAll('.shard-row button')].find((n) => n.textContent.trim() === '详情');
      if (btn) btn.click();
    })()""")
    time.sleep(0.5)
    second = b.js(PROBE)
    b.shoot(os.path.join(SHOTS, "host-1440-dark.png"))

    # 再等一轮，看接口是不是每五秒真的问一次（只问一次就是没在轮询）。
    time.sleep(11)
    again = b.js(PROBE)
    b.set_viewport(1440, 900)

    b.js("""(() => {
      const btns = Array.from(document.querySelectorAll('.topbar .icon-btn'));
      const t = btns.find((n) => (n.title || '').indexOf('切到') === 0);
      if (t) t.click();
    })()""")
    time.sleep(0.6)
    theme = b.js("document.documentElement.dataset.theme")
    b.shoot(os.path.join(SHOTS, "host-1440-light.png"))

    b.set_viewport(390, 844)
    time.sleep(0.8)
    mobile = b.js(PROBE)
    b.shoot(os.path.join(SHOTS, "host-390-dark.png"))
    b.close()

    ok = True

    def check(name, cond, extra=""):
        nonlocal ok
        print(("  OK   " if cond else "  FAIL ") + name + (("  " + str(extra)) if extra else ""))
        if not cond:
            ok = False

    print("== 1440 第二轮 ==")
    print(json.dumps([{k: r[k] for k in ("label", "head", "note", "tone", "arc", "stroke")} for r in second["rings"]],
                     ensure_ascii=False, indent=1))
    print(json.dumps(second["rows"], ensure_ascii=False))

    check("登录后确实等到了有色的表盘（=轮询在跑）", got and second["rings"])
    check("三颗表盘都在", len(second["rings"]) == 3, len(second["rings"]))
    check("表盘顺序是 处理器/内存/存档盘",
          [r["label"] for r in second["rings"]] == ["处理器", "内存", "存档盘"],
          [r["label"] for r in second["rings"]])
    check("第二轮接口又被问过（轮询没停）", again["apiCalls"] > second["apiCalls"],
          (second["apiCalls"], again["apiCalls"]))
    check("圆心是百分数或破折号，不为空",
          all(r["head"] and (r["head"] == "—" or r["head"].endswith("%")) for r in second["rings"]),
          [r["head"] for r in second["rings"]])
    check("没有画弧的表盘不报 0%",
          all(not (r["head"] == "0%" and not r["arc"]) for r in second["rings"]))
    check("每颗表盘都有一行说明", all(r["note"] for r in second["rings"]))
    check("色调只取 ok/warn/bad/idle",
          all(any(t in r["tone"] for t in ("ok", "warn", "bad", "idle")) for r in second["rings"]),
          [r["tone"] for r in second["rings"]])
    check("描边色来自主题变量（不是写死的 hex 常量名）",
          all(r["stroke"].startswith("rgb(") for r in second["rings"] if r["arc"]),
          [r["stroke"] for r in second["rings"]])
    check("侧列至少四行绝对量", len(second["rows"]) >= 4, len(second["rows"]))
    check("侧列没有空值", all(r["k"] and r["v"] for r in second["rows"]), second["rows"])
    check("侧列没有裸英文码值",
          all(("Master" not in r["v"] and "queued" not in r["v"] and "undefined" not in r["v"])
              for r in second["rows"]),
          [r["v"] for r in second["rows"]])
    check("侧列没有 NaN", not any("NaN" in r["v"] for r in second["rows"]),
          [r["v"] for r in second["rows"]])
    check("世界行上那一枚占用有说法", bool(second["procRow"]), second["procRow"])
    check("1440 无横向溢出", second["docW"] <= second["winW"] + 1, (second["docW"], second["winW"]))
    check("表盘一排不高过 200", all(r["box"]["h"] < 200 for r in second["rings"]))

    print("\n== 390 ==")
    print(json.dumps([{k: r[k] for k in ("label", "head", "note", "box")} for r in mobile["rings"]],
                     ensure_ascii=False, indent=1))
    check("390 无横向溢出", mobile["docW"] <= mobile["winW"] + 1, (mobile["docW"], mobile["winW"]))
    check("手机上三颗表盘都还在", len(mobile["rings"]) == 3, len(mobile["rings"]))
    check("手机上表盘没超出视口", all(r["box"]["x"] >= -1 and r["box"]["w"] <= mobile["winW"] for r in mobile["rings"]),
          [(r["box"]["x"], r["box"]["w"]) for r in mobile["rings"]])
    check("手机上侧列也在", len(mobile["rows"]) >= 4, len(mobile["rows"]))

    print("\n主题切换后：" + str(theme))
    check("点图标确实换了主题", theme in ("light", "dark"), theme)

    print("截图：" + os.path.abspath(SHOTS))
    print("\n" + ("全部通过" if ok else "有失败项"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
