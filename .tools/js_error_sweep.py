# 前端报错清扫：按侧栏顺序点遍每一页，任何 console.error / 警告 / 落到"没有对应页面"
# 都算失败。
#
# 为什么单独立一个脚本：免构建 Vue 没有编译期，组件里引用一个没 import 的标识符
# （mixins: [RoomAccess] 就这么写过）是在模块求值时炸的，炸完整个应用连登录页都渲染不出来；
# 而 computed 当函数调（this.roleDict()）只炸那一块，页面看起来像"没数据"。
# 这两种在截图上和"夹具空了"长得一模一样，只有把控制台接住才分得清。
#
# 报错收集必须走 Page.addScriptToEvaluateOnNewDocument：应用脚本在页面加载时就跑完了，
# 加载完再注入只能收到之后的错，boot 期那条永远漏掉。
#
# 走侧栏按钮而不是写死 hash 列表：写死过一次，把 #/snaps 记成 #/snapshots，
# 结果脚本"通过"了但那条路由根本不存在。侧栏有什么就点什么，导航和路由对不上时会当场红。
import sys, os, time, json
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import Browser, BASE

ARM = r"""
(() => {
  if (window.__armed) return 'again';
  window.__errs = []; window.__warns = [];
  const ce = console.error, cw = console.warn;
  console.error = function () { window.__errs.push(Array.from(arguments).join(' ')); ce.apply(console, arguments); };
  console.warn = function () { window.__warns.push(Array.from(arguments).join(' ')); cw.apply(console, arguments); };
  window.addEventListener('error', (e) => window.__errs.push(String(e.message) + ' @' + (e.filename || '') + ':' + e.lineno));
  window.addEventListener('unhandledrejection', (e) => window.__errs.push('promise: ' + String(e.reason)));
  window.__armed = true;
  return 'armed';
})()
"""

SNAP = """(() => {
  const t = (n) => (n ? n.textContent.trim() : '');
  return {
    hash: location.hash,
    errs: (window.__errs || []).slice(0, 4),
    warns: (window.__warns || []).slice(0, 3),
    docW: document.documentElement.scrollWidth, winW: window.innerWidth,
    cards: document.querySelectorAll('.card').length,
    modRows: document.querySelectorAll('.mod-row').length,
    rows: document.querySelectorAll('table.data tbody tr').length,
    sk: document.querySelectorAll('.sk').length,
    why: (Array.from(document.querySelectorAll('.err-box, .x-note')).map(t).filter(x => x)[0] || '').slice(0, 120),
    empty: Array.from(document.querySelectorAll('.e-title')).map(t).slice(0, 2),
  };
})()"""


def rendered(got):
    return bool(got["cards"] or got["modRows"] or got["rows"] or got["empty"])


def main():
    wide = "--wide" in sys.argv
    req_w = 1440 if wide else 390
    b = Browser(req_w, 900 if wide else 844)
    b.ws.call("Page.addScriptToEvaluateOnNewDocument", {"source": ARM})
    b.goto(BASE + "/#/")
    b.js("sessionStorage.clear()")
    b.reload()
    # 等登录页出现，不按固定秒数睡：本机 2.5 秒够，CDP_BASE 指到公网面板时
    # 一份前端资源要走一趟跨国链路，固定秒数只会让脚本在云机上假失败。
    if not b.wait("!!document.querySelector('.login-form')", tries=40, pause=0.5):
        print("登录页没渲染出来，boot 期报错：%s" % json.dumps(
            b.js("window.__errs || []"), ensure_ascii=False))
        b.close()
        return 1
    b.js("""(() => {
      const ins = document.querySelectorAll('.login-card input');
      const set = (n, v) => { n.value = v; n.dispatchEvent(new Event('input', { bubbles: true })); };
      set(ins[0], 'admin'); set(ins[1], 'admin');
      document.querySelector('.login-submit').click();
    })()""")
    if not b.wait("document.querySelectorAll('.nav-item').length > 0", tries=40, pause=0.5):
        print("登录后没出导航，boot 期报错：%s" % json.dumps(
            b.js("window.__errs || []"), ensure_ascii=False))
        b.close()
        return 1
    n = b.js("document.querySelectorAll('.nav-item').length")
    bad = []
    skipped = []
    for i in range(n):
        b.js("""(() => {
          const it = document.querySelectorAll('.nav-item')[%d];
          if (it) it.click();
        })()""" % i)
        time.sleep(1.5)
        got = b.js(SNAP)
        b.js("window.__errs = []; window.__warns = []")
        print("%-12s %s" % (got["hash"], json.dumps(got, ensure_ascii=False)))
        if got["errs"]:
            bad.append((got["hash"], "报错：" + " | ".join(got["errs"])))
        if got["warns"]:
            bad.append((got["hash"], "警告：" + " | ".join(got["warns"])))
        if any("没有对应的页面" in x for x in got["empty"]):
            bad.append((got["hash"], "侧栏指到了一个没有注册的路由"))
        # 溢出跟"我们要求的那一档宽度"比，不跟 window.innerWidth 比：
        # 390 这一档走的是 mobile 仿真，内容放不下时 Chrome 会把布局视口自己放大，
        # 于是 winW 跟着 docW 一起变成 451，这一条就成了永真式（2026-11-07 世界行
        # 那枚「占用」胶囊把整句解释摆进 nowrap 里，就是这么漏掉手机的 61px 横向滚动的）。
        if got["docW"] > req_w + 1:
            bad.append((got["hash"], "横向溢出 %d > %d" % (got["docW"], req_w)))
        # "什么都没渲染"的判据不能只看 .card：快照页和操作记录页的主体是表格 +
        # 手机端的 .mod-row 卡，压根不用 .card，第一版就是在这里把它们误判成"页面断了"。
        if not rendered(got):
            # 骨架屏还挂着时"三者皆无"不是断页，是还没回。公网房间那一页要等 Klei 的 CDN，
            # 固定那 1.5 秒在这条链路上就是不够，于是同一份代码有时量到 20 行、有时量到 0。
            # 给它一次有上限的等待（骨架消失且真渲染出东西），等不到才往下判。
            b.wait("!document.querySelector('.sk') && "
                   "document.querySelectorAll('.card, .mod-row, table.data tbody tr, .e-title').length > 0",
                   tries=60)
            got = b.js(SNAP)
            print("%-12s 补量一次 %s" % (got["hash"], json.dumps(got, ensure_ascii=False)))
            # 等到头了还没渲染，也分三种，只有最后那种才是断页：
            # 页面上写了一句原因（.err-box / .x-note）——那是产品正常在说话；
            # 骨架还挂着——那是上游没回，面板自己也没法更快；
            # 两者都没有、又空无一物——那一屏是真的断了。
            # 上一轮 #/lobby 就是被这里当成断页报红的：实测那一页正常 5 秒出 20 行，
            # 而那次链路慢到 18 秒之外（.tools/tmp/lobby_sweep_probe.py 量过），
            # 同一份代码两次量出来一个 20 一个 0，红的是链路不是页面。
            if rendered(got):
                pass
            elif got["why"]:
                print("%-12s 页面上写了原因，按活页算：%s" % (got["hash"], got["why"]))
            elif got["sk"]:
                skipped.append(got["hash"])
                print("%-12s 骨架屏还挂着（上游没回），这一页这轮没判" % got["hash"])
            else:
                bad.append((got["hash"], "卡片、行、空状态三者皆无，页面上也没写原因：这一页是渲染断了"))
    b.close()
    if bad:
        print("\n有失败项：")
        for h, why in bad:
            print("  %s  %s" % (h, why))
        return 1
    print("\n%d 个页面全部干净（无报错、无警告、无溢出、无断掉的页面）" % n
          + ("；这轮因上游没回而没判的：%s" % "、".join(skipped) if skipped else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
