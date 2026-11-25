# 操作记录页的时间筛选：那三个快捷档到底筛没筛，自定义那两头有没有算错时区。
#
# 这一条最容易"看着在用、其实在骗人"：
#   - 前端把 "2026-11-02" 这种日期串直接发给后端，后端按 UTC 比，东八区的"今天"
#     会变成"昨天下午四点以后"——条数看着正常，内容差一天，没人会发现；
#   - 分段控件切了档但没把 offset 归零，人还停在第 4 页，看到的是一条空表；
#   - 两头写反时接口返回空表，而空表在界面上和"那段时间确实没操作"长得一样。
# 所以这里把真正发出去的请求 URL 抓下来核对参数，而不是只看界面文字。
#
# 跑法：python .tools/ui_audit_when_check.py [宽度]
# 前提：python .tools/ui_dev.py 已把面板起在 127.0.0.1:8963。
import json
import os
import re
import sys
import time
import urllib.parse
from datetime import datetime, timedelta, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, ".tools", "tmp", "shots")
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# 装一个 fetch 记录器：只记 /api/v1/audit 那几条，别的接口不掺进来。
SPY = r"""(() => {
  if (window.__auditSpy) return 'already';
  window.__auditSpy = [];
  const orig = window.fetch.bind(window);
  window.fetch = (url, init) => {
    const u = String(url);
    if (u.indexOf('/api/v1/audit') >= 0 && u.indexOf('audit-actions') < 0) window.__auditSpy.push(u);
    return orig(url, init);
  };
  return 'on';
})()"""

SPY_ON = r"""(!!window.__auditSpy)"""

READ = r"""(() => {
  const seg = document.querySelector('.audit-when .seg');
  const chip = document.querySelector('.audit-when .chip');
  const dates = [...document.querySelectorAll('.audit-when input[type=date]')];
  const note = document.querySelector('.when-note');
  return {
    segs: seg ? [...seg.querySelectorAll('button')].map((x) => ({
      label: x.textContent.trim(), on: x.className.indexOf('on') >= 0,
      pressed: x.getAttribute('aria-pressed'),
    })) : null,
    chip: chip ? chip.textContent.trim() : '',
    dates: dates.map((d) => ({ id: d.id, value: d.value, w: Math.round(d.getBoundingClientRect().width) })),
    note: note ? note.textContent.trim() : '',
    noteBad: note ? note.className.indexOf('bad') >= 0 : false,
    rows: document.querySelectorAll('.audit-page table.data tbody tr').length,
    cards: document.querySelectorAll('.audit-page .mod-row').length,
    empty: ((document.querySelector('.audit-page .e-title') || {}).textContent || '').trim(),
    pager: ((document.querySelector('.audit-page .pager .muted') || {}).textContent || '').trim(),
    overflow: document.documentElement.scrollWidth - innerWidth,
  };
})()"""

# 表格里"什么时候"那一列显示的时刻（本地时区、带日期），用来核对筛出来的行。
TIMES = r"""[...document.querySelectorAll('.audit-page table.data tbody tr td:first-child b')]
  .map((x) => x.textContent.trim())"""

CLICK_SEG = r"""(() => {
  const el = [...document.querySelectorAll('.audit-when .seg button')]
    .find((x) => x.textContent.trim() === %s);
  if (!el) return false;
  el.click();
  return true;
})()"""

SET_DATE = r"""(() => {
  const el = document.getElementById(%s);
  if (!el) return false;
  el.value = %s;
  // v-model 在没有 lazy 的时候听的是 input，光发 change 组件里的值不会动。
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  return el.value;
})()"""

# 分页的下一页：按钮没有 id，只能按文字找，所以先确认它真点着了。
CLICK_NEXT = r"""(() => {
  const el = [...document.querySelectorAll('.audit-page .pager button')]
    .find((x) => x.textContent.trim().indexOf('下一页') >= 0);
  if (!el) return false;
  el.click();
  return true;
})()"""


def seg(b, label):
    assert b.js(CLICK_SEG % json.dumps(label)), "分段控件里没有「%s」这一档" % label
    time.sleep(1.2)


# 把请求里真正带去的 since/until 翻回"几月几日"，用来看界面那句话有没有说对。
# until 是不含的那头，说给人听要退一天——和组件里那个写法是同一条规则，
# 这里独立算一遍，两边不一致就是筛的和说的不是一回事。
def say_range(url):
    q = urllib.parse.parse_qs(url.split("?", 1)[1])

    def say(raw, exclusive):
        if not raw:
            return "不限"
        d = datetime.strptime(raw, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).astimezone()
        if exclusive:
            d = d - timedelta(days=1)
        return "%d 月 %d 日" % (d.month, d.day)

    return say(q.get("since", [""])[0], False) + " 到 " + say(q.get("until", [""])[0], True)


# 把界面上填的那个本地日期换成接口要的 UTC 时刻。方向反过来算一遍：
# 日期框里写的是哪天、请求里就该筛哪天的零点，中间不许有时区漂移。
def local_midnight_utc(datestr, plus_days=0):
    naive = datetime.strptime(datestr, "%Y-%m-%d")
    local = naive.replace(tzinfo=datetime.now().astimezone().tzinfo)
    return (local + timedelta(days=plus_days)).astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def q_of(url, key):
    q = urllib.parse.parse_qs(url.split("?", 1)[1])
    return (q.get(key) or [""])[0]


def main():
    w = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    b = Browser(w, 900 if w > 560 else 844)
    try:
        b.goto(BASE + "/#/audit")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.audit-page')"):
            raise SystemExit("操作记录页没出来")
        if w <= 560:
            # 窄屏上这四组筛选默认收起（见 audit.js），真人要先点「筛选」才看得到日期框。
            # 不先点开的话，下面量到的日期框宽度是 0，那是一条假红。
            b.js("document.getElementById('audit-filters-toggle').click()")
        time.sleep(0.4)
        if not b.wait("!!document.querySelector('.audit-when')"):
            raise SystemExit("操作记录页没有时间那一行筛选")
        time.sleep(1.5)
        # 记录器要装在第一次请求之后、下一次点击之前：先清掉再装，只抓后面这几条。
        b.js(SPY)
        assert b.js(SPY_ON), "fetch 记录器没装上"

        d0 = b.js(READ)
        print("-- 默认（不限时间） --")
        print(json.dumps(d0, ensure_ascii=False, indent=2))
        labels = [s["label"] for s in d0["segs"]]
        for want in ["不限时间", "今天", "近 7 天", "近 30 天", "自定义"]:
            assert want in labels, "分段控件少了 %s：%s" % (want, labels)
        assert d0["segs"][0]["on"], "默认档不是「不限时间」"
        assert d0["chip"] == "", "默认档不该报一个具体日期区间"
        base_rows = d0["rows"] or d0["cards"]
        assert base_rows > 0, "夹具里没有任何操作记录，这一轮验不出筛选效果"

        # 今天：请求里必须带上换算过的 UTC 时刻，而不是本地日期串。
        seg(b, "今天")
        d1 = b.js(READ)
        url1 = b.js("window.__auditSpy[window.__auditSpy.length-1]")
        print("-- 切到「今天」 --")
        print(json.dumps({"chip": d1["chip"], "pager": d1["pager"], "url": url1}, ensure_ascii=False))
        q1 = urllib.parse.parse_qs(url1.split("?", 1)[1])
        assert "since" in q1 and "until" in q1, "今天这一档没发时间参数：%s" % url1
        assert re.match(r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$", q1["since"][0]), \
            "发出去的不是 UTC 的整秒时刻：%r" % q1["since"][0]
        assert "月" in d1["chip"] and "到" in d1["chip"], "没把「今天」翻成具体日期：%r" % d1["chip"]
        # 那句摘要必须就是真正筛的那一段：光看请求参数不核对文字的话，
        # 界面写着 11 月 2 日、实际筛到 11 月 3 日，用户按错的那句去翻游戏日志。
        assert d1["chip"] == say_range(url1), "「今天」那句摘要 = %r，实际筛的是 %r" % (
            d1["chip"], say_range(url1))

        # 近 7 天：区间比今天宽，条数不该变少（夹具里的记录都在最近几天）。
        seg(b, "近 7 天")
        d2 = b.js(READ)
        url2 = b.js("window.__auditSpy[window.__auditSpy.length-1]")
        print("-- 切到「近 7 天」 --")
        print(json.dumps({"chip": d2["chip"], "rows": d2["rows"], "cards": d2["cards"],
                          "url": url2}, ensure_ascii=False))
        since7 = urllib.parse.parse_qs(url2.split("?", 1)[1])["since"][0]
        assert since7 < q1["since"][0], "近 7 天的起点 (%s) 比今天 (%s) 还晚" % (since7, q1["since"][0])
        assert d2["chip"] == say_range(url2), "「近 7 天」那句摘要 = %r，实际筛的是 %r" % (
            d2["chip"], say_range(url2))
        assert (d2["rows"] or d2["cards"]) >= (d1["rows"] or d1["cards"]), "放宽区间反而变少"

        # 切档必须回到第一页：停在第 4 页看空表是最容易被当成 bug 的一种。
        assert "第 1 页" in d2["pager"], "切档没回到第一页：%r" % d2["pager"]

        # 自定义：两个日期框出现，两个都空着时按"不限"算，只给一句说明，不许拦请求。
        seg(b, "自定义")
        d3 = b.js(READ)
        url3 = b.js("window.__auditSpy[window.__auditSpy.length-1]")
        print("-- 切到「自定义」，两头都空 --")
        print(json.dumps({"dates": d3["dates"], "note": d3["note"], "bad": d3["noteBad"],
                          "url": url3, "rows": d3["rows"], "cards": d3["cards"]}, ensure_ascii=False))
        assert len(d3["dates"]) == 2, "自定义没有两个日期框"
        assert [x["id"] for x in d3["dates"]] == ["audit-from", "audit-to"], "日期框 id 不对"
        assert d3["dates"][0]["w"] >= 120, "日期框被挤窄了：%s" % d3["dates"][0]["w"]
        assert "不限" in d3["note"] and not d3["noteBad"], "两头空时该是一句说明，不是报错：%r" % d3["note"]
        assert "since=" not in url3 and "until=" not in url3, "两头空却发了时间参数：%s" % url3
        assert (d3["rows"] or d3["cards"]) > 0, "两头空却把表清空了"

        # 这一条是前面漏掉的一整类：区间改了以后如果人还停在第 4 页，
        # 新窗口里根本没有第 4 页，摆出来的是一张空表，看着就像"那几天没操作"。
        # 所以先真的翻到第 2 页，再动日期，核对发出去的偏移量。
        assert b.js(CLICK_NEXT), "分页里没有下一页按钮"
        time.sleep(1.2)
        urlp = b.js("window.__auditSpy[window.__auditSpy.length-1]")
        print("-- 翻到第 2 页 --")
        print(json.dumps({"url": urlp}, ensure_ascii=False))
        assert "offset=" in urlp, "点下一页没带上偏移量：%s" % urlp
        assert b.js(SET_DATE % (json.dumps("audit-from"), json.dumps("2026-10-01")))
        time.sleep(1.2)
        urlq = b.js("window.__auditSpy[window.__auditSpy.length-1]")
        dq = b.js(READ)
        print("-- 翻过页之后改开始日期 --")
        print(json.dumps({"url": urlq, "pager": dq["pager"]}, ensure_ascii=False))
        assert "offset=" not in urlq, "改了日期还停在第 2 页：%s" % urlq
        assert "第 1 页" in dq["pager"], "界面还显示第 2 页：%r" % dq["pager"]

        today = time.strftime("%Y-%m-%d")
        assert b.js(SET_DATE % (json.dumps("audit-from"), json.dumps(today)))
        time.sleep(1.2)
        url4 = b.js("window.__auditSpy[window.__auditSpy.length-1]")
        d4 = b.js(READ)
        print("-- 只填开始那天 --")
        print(json.dumps({"note": d4["note"], "url": url4}, ensure_ascii=False))
        assert "since=" in url4 and "until=" not in url4, "只填一头时不该把另一头也发出去：%s" % url4
        assert d4["note"] == "", "只填一头还在提示：%r" % d4["note"]
        # 自定义这一档不给"几月几日 到 几月几日"那句摘要（用户刚自己填完，
        # 日期框就是那句话），所以这里核对的是框里的日期和发出去的时刻对不对得上：
        # 只填开始那天，就该从那天本地零点开始筛，而不是 UTC 零点（那会差 8 小时）。
        assert q_of(url4, "since") == local_midnight_utc(today), \
            "框里写 %s，发出去的起点却是 %s" % (today, q_of(url4, "since"))
        assert d4["dates"][0]["value"] == today, "日期框里的值被组件改写了：%r" % d4["dates"][0]["value"]

        # 两头写反：必须当场说，而且不许发请求（发了就是一张空表，看着像"那段时间没操作"）。
        n_before = b.js("window.__auditSpy.length")
        assert b.js(SET_DATE % (json.dumps("audit-to"), json.dumps("2020-01-01")))
        time.sleep(1.2)
        d5 = b.js(READ)
        n_after = b.js("window.__auditSpy.length")
        print("-- 结束早于开始 --")
        print(json.dumps({"note": d5["note"], "bad": d5["noteBad"], "requests": n_after - n_before},
                         ensure_ascii=False))
        assert "之前" in d5["note"] and d5["noteBad"], "写反了没给红色的原因：%r" % d5["note"]
        assert n_after == n_before, "区间不成立还是发了请求（会摆出一张空表）"

        # 含当天：结束日期填今天，今天那条记录必须在表里，而且 until 是明天零点。
        assert b.js(SET_DATE % (json.dumps("audit-to"), json.dumps(today)))
        time.sleep(1.2)
        d6 = b.js(READ)
        url6 = b.js("window.__auditSpy[window.__auditSpy.length-1]")
        print("-- 起止都是今天 --")
        print(json.dumps({"rows": d6["rows"], "cards": d6["cards"], "url": url6}, ensure_ascii=False))
        assert (d6["rows"] or d6["cards"]) > 0, "结束那天填今天却一条都没有——多半是没把当天算进去"
        q = urllib.parse.parse_qs(url6.split("?", 1)[1])
        since6, until6 = q["since"][0], q["until"][0]
        assert until6 > since6, "until (%s) 没在 since (%s) 之后" % (until6, since6)
        # 本地今天的次日零点 = until；用 datetime 换一次，不靠字符串猜。
        local = datetime.now().astimezone()
        day0 = local.replace(hour=0, minute=0, second=0, microsecond=0)
        want_until = (day0 + timedelta(days=1)).astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        want_since = day0.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        assert since6 == want_since, "since 不是今天本地零点：给了 %s，期望 %s" % (since6, want_since)
        assert until6 == want_until, "until 不是明天本地零点（没把结束那天算进去）：给了 %s，期望 %s" % (
            until6, want_until)
        assert d6["dates"][1]["value"] == today, "结束日期框没留住今天：%r" % d6["dates"][1]["value"]

        # 到输出这一头再核一遍：表里每一行显示的日期都必须落在填的那一天。
        # 只核对请求参数验不出"参数发对了、界面按 UTC 摆日期"这一类错——
        # 那正好是这一页最容易骗人的地方：看着筛的是今天，列出来的却是昨天。
        shown = b.js(TIMES)
        assert shown, "起止都填今天，表里却一行都没有"
        for t in shown:
            assert t.startswith(today + " "), "筛的是今天，界面上出现了 %r" % t

        b.js("document.querySelector('.audit-when').scrollIntoView({block:'center'})")
        time.sleep(0.3)
        b.shoot(os.path.join(OUT, "audit_when_%d.png" % w))

        assert d6["overflow"] <= 0, "横向溢出了 %s px" % d6["overflow"]
        print("\n操作记录时间筛选 %s 宽度：全部通过（溢出 %s px）" % (w, d6["overflow"]))
    finally:
        b.close()


if __name__ == "__main__":
    main()
