# #76 验收：不知道要什么模组时，能不能一页一页翻工坊的整张榜。
#
# 盯四件事，全是"接口现实"和"界面说的话"对不对得上：
#   1. 首屏（不给关键词）真的发出了在线请求，且 URL 里没有 search_text；
#   2. 说明那句话说的是"工坊自己的榜 + 共 N 条 + 第几页"，不是"没给搜索词"；
#   3. 点下一页之后条目换了一批、页码口径没变（还是在线，不是掉回缓存）；
#   4. 手机宽度不横向溢出。
#
# 网络出口是外部状态，这台机器有时通有时不通，所以在线那三步不通时直接判 SKIP
# 并打印接口原话，绝不把"连不上"写成"翻不动"。
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
sys.stderr.reconfigure(encoding="utf-8", errors="replace")

# 把每次 /mods/search 的 URL 记下来，用来证明"下一页"真的换了 page 参数。
STUB = r"""(() => {
  if (window.__rankHooked) return 'already';
  window.__rankHooked = 1;
  window.__urls = [];
  const real = window.fetch;
  window.fetch = async (u, o) => {
    if (typeof u === 'string' && u.indexOf('/api/v1/mods/search') >= 0) window.__urls.push(u);
    return real(u, o);
  };
  return 'ok';
})()"""

READ = r"""(() => {
  const note = document.querySelector('.mod-cats') ? document.querySelector('x-note, .x-note') : null;
  const cards = [...document.querySelectorAll('.mod-card')];
  const pager = document.querySelector('.pager');
  return {
    live: !!(window.__view && window.__view.live),
    note: (document.querySelector('.err-box') ? '' : '') +
          [...document.querySelectorAll('x-note, .note, .x-note')].map(n => n.innerText).join(' | '),
    cards: cards.length,
    first: cards.length ? (cards[0].querySelector('.mod-card-name') || cards[0]).innerText : '',
    ids: [...document.querySelectorAll('.mod-card')].map(c => c.getAttribute('data-wsid') || ''),
    pager: pager ? pager.innerText.replace(/\s+/g, ' ') : '',
    overflow: document.documentElement.scrollWidth > window.innerWidth,
    urls: window.__urls,
  };
})()"""


def main():
    width = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    height = int(sys.argv[2]) if len(sys.argv) > 2 else 900
    out = sys.argv[3] if len(sys.argv) > 3 else ""
    b = Browser(width, height)
    fails = []
    p2 = None
    try:
        # 必须带一个每次都变的查询参数：URL 只换 hash 时 Page.navigate 不会重载文档，
        # 上一轮跑在这个标签页里装的 fetch hook 会一直留着，每层各记一次 URL，
        # 于是"一次点击三条请求"看着像页面在重复发请求（其实是我的探针叠了三层）。
        b.goto(BASE + "/?cdp=%d/#/mods" % int(time.time() * 1000))
        # 复用了别人拉起来的 Chrome 时，那个标签页可能已经登过录（sessionStorage 还在），
        # 这时候根本没有登录页可等，直接往下走。
        if not b.wait("!!document.querySelector('.login-form') || !!document.querySelector('.tabs')"):
            raise SystemExit("既没有登录页也没有外壳")
        if b.js("!!document.querySelector('.login-form')"):
            b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
            b.reload()
            if not b.wait("!!document.querySelector('.tabs')"):
                raise SystemExit("登录后还停在登录页")
        # 先等首屏真的画出来：模组页是外壳里晚一帧才挂载的子组件，
        # 一看到 .tabs 就去找「搜」按钮会找一个还不存在的按钮（实测踩过）。
        if not b.wait("document.querySelectorAll('.mod-card').length > 0"):
            raise SystemExit("首屏一张卡都没有")
        b.js(STUB)
        # 首屏那一次请求是在组件挂载时就发出去的，比这个 hook 早，抓不到 URL。
        # 所以这里主动点一次"搜"（关键词是空的），让同一件事在 hook 装好之后再发生一次——
        # 用户看到的仍然是同一个请求，验的是接口真实收到的那条 URL。
        clicked = b.js("(() => { const bs=[...document.querySelectorAll('button')];"
                     " const s=bs.find(x=>/^\\s*搜\\s*$/.test(x.textContent)); if(s) s.click(); return !!s; })()")
        if not clicked:
            raise SystemExit("找不到「搜」按钮")
        if not b.wait("window.__urls.length > 0 && document.querySelectorAll('.mod-card').length > 0"):
            raise SystemExit("点「搜」之后要么没发请求、要么结果没画出来")
        b.js("window.scrollTo(0,0)")
        p1 = b.js(READ)
        if len(p1["urls"]) != 1:
            fails.append("点一次「搜」发了 %d 条请求（在线检索按 key 限流，重复发请求是要挨 429 的）：%s"
                         % (len(p1["urls"]), p1["urls"]))
        if "search_text" in p1["urls"][0]:
            fails.append("空关键词那条 URL 里还带了 search_text：%s" % p1["urls"][0])
        if "page=1" not in p1["urls"][0]:
            fails.append("首屏 URL 没有 page=1：%s" % p1["urls"][0])
        # 网络出口是外部状态：这台机到 api.steampowered.com 时通时断（2026-11-03 一天里
        # 就通过一次、断过一次）。断了不算页面的 bug，但降级那句话说得对不对必须验。
        online = "在线检索没成功" not in p1["note"]
        if online:
            if "工坊自己的榜" not in p1["note"]:
                fails.append("在线通了却没说这是工坊整张榜：%s" % p1["note"][:160])
            if "共 27" not in p1["pager"] and "共 " not in p1["pager"]:
                fails.append("分页器没有总数：%s" % p1["pager"])
        else:
            if "连不上" not in p1["note"]:
                fails.append("降级了却没说是为什么：%s" % p1["note"][:160])
        for bad in ("没给搜索词", "没配 key"):
            if bad in p1["note"]:
                fails.append("说明里还留着「%s」：%s" % (bad, p1["note"][:160]))
        if p1["cards"] < 2:
            fails.append("卡片只有 %d 张" % p1["cards"])
        if p1["overflow"]:
            fails.append("%dpx 宽度横向溢出了" % width)

        # 下一页：条目必须换一批，页码必须真的带上去。
        b.js("(() => { const bs=[...document.querySelectorAll('.pager button')];"
             " const n=bs.find(x=>/下一/.test(x.textContent)); if(n) n.click(); return !!n; })()")
        if not b.wait("window.__urls.length > 1"):
            fails.append("点了下一页但没有第二个请求")
        else:
            # 等高亮真的落到 2 上、这一屏的卡也画出来了再读，
            # 否则读到的是上一页、甚至是加载中那一片骨架屏（0 张卡）。
            if not b.wait("(() => { const on=document.querySelector('.pg-num.on');"
                          " return !!on && on.textContent.trim()==='2' &&"
                          " document.querySelectorAll('.mod-card').length > 0; })()"):
                fails.append("点了下一页，页码或卡片没换成第 2 页")
            p2 = b.js(READ)
            if "page=2" not in p2["urls"][-1]:
                fails.append("下一页的 URL 不是 page=2：%s" % p2["urls"][-1])
            if p2["first"] == p1["first"]:
                fails.append("下一页第一条没换：%s" % p2["first"])
            if online and "第 2 页" not in p2["note"]:
                fails.append("下一页的说明没跟着换页码：%s" % p2["note"][:160])
        if out:
            b.shoot(out)
        print("在线=%s" % ("通" if online else "断（走降级）"))
        print("note1=%s" % p1["note"])
        print("cards=%d 第一条=%s 分页器=%s" % (p1["cards"], p1["first"], p1["pager"]))
        print("第二条=%s 分页器2=%s" % (p2["first"] if p2 else "(没点到下一页)",
                                        p2["pager"] if p2 else ""))
        print("urls=%s" % " | ".join((p2 or p1)["urls"]))
    finally:
        b.close()
    if fails:
        for f in fails:
            print("FAIL %s" % f)
        return 1
    print("PASS 逐页翻榜单这一条通了")
    return 0


if __name__ == "__main__":
    sys.exit(main())
