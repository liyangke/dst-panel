# 玩家时间线的一次真点：切看法、看归人、点芯片跳回逐行日志、把窗口收到最小再看空状态。
#
# 为什么值得单独跑：这一页的全部价值在于"数字是真的"。接口层测的是聚合本身
# （internal/gamelog/timeline_test.go），这一支测的是界面把它说成了什么——
# 最容易犯的三种错都是"看着对"：芯片点过去筛出空列表（假链接）、
# 码值漏到界面上（player_death 这种）、窗口收窄以后屏幕空着却不告诉人只数了 50 行。
#
# 跑法：python .tools/ui_timeline_check.py [宽度]
# 前提：python .tools/ui_dev.py 已经把面板起在 127.0.0.1:8963。
import json
import os
import re
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, ".tools", "tmp", "shots", "timeline")
# 折叠那一条要临时往真夹具里灌几十行聊天（后端每人只列最近 40 行，正常夹具一个人
# 也就 8 行，走不到那一步）。灌完必须原样写回去，所以先存字节再还原。
CHAT = os.path.join(ROOT, ".tmp_boot", "klei", "Cluster_1", "Master", "server_chat_log.txt")
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HOOK = ("window.__perr=[];window.addEventListener('error',function(e){"
        "if(window.__perr.length<20)window.__perr.push(String(e.message))});"
        "window.addEventListener('unhandledrejection',function(e){"
        "if(window.__perr.length<20)window.__perr.push('promise:'+String(e.reason&&e.reason.message||e.reason))});")

FAILED = []
REPORT = []
# 行首的时间戳：墙上时间 hh:mm:ss（界面优先给这个），引擎时钟是 [hh:mm:ss]。
TIME = re.compile(r"^\d{2}:\d{2}:\d{2}")
# "最后一次"那一栏整段就是一个时间，前后不许再混别的字。
TIME_ONLY = re.compile(r"^\d{2}:\d{2}:\d{2}$")


def check(name, ok, detail=""):
    REPORT.append(("PASS" if ok else "FAIL", name, detail))
    print("%s %s%s" % ("PASS" if ok else "FAIL", name,
                       ("  ← " + str(detail).replace("\n", " ")[:220]) if detail else ""))
    if not ok:
        FAILED.append(name + "｜" + str(detail)[:160])


READ = r"""(() => {
  const segText = (sel) => [...document.querySelectorAll(sel)].map((s) =>
    [...s.querySelectorAll('button')].map((b) => b.textContent.trim() + (b.classList.contains('on') ? '[开]' : '')));
  const card = document.querySelector('.tl-list');
  const blocks = card ? [...card.querySelectorAll('.tl-player')] : [];
  const head = document.querySelector('.tl-head');
  const empty = document.querySelector('.tl-empty-note');
  const notes = [...document.querySelectorAll('.card .note')].map((n) => n.textContent.replace(/\s+/g, ' ').trim());
  // 整张卡上的可见文字：码值扫描就用它，因为界面露出去的是文字而不是 DOM。
  const cardText = head ? (head.textContent + (card ? card.textContent : '') + (empty ? empty.textContent : '')) : '';
  return {
    viewSegs: segText('.log-controls .seg').slice(0, 1),
    rangeSegs: segText('.log-controls .seg').slice(1),
    switchOn: !!document.querySelector('.log-controls .switch input:checked'),
    filtersShown: !!document.querySelector('details.group'),
    metaShown: !!document.querySelector('.stream-meta'),
    head: head ? head.textContent.replace(/\s+/g, ' ').trim() : '',
    notes, cardText,
    people: blocks.length,
    blocks: blocks.map((p) => ({
      who: (p.querySelector('.tl-who') || {}).textContent || '',
      chips: [...p.querySelectorAll('.tl-who-row button.chip')].map((b) => ({
        t: b.textContent.trim(),
        tone: ['warn', 'bad', 'ok'].filter((c) => b.classList.contains(c))[0] || '',
      })),
      rows: [...p.querySelectorAll('.tl-events .logline')].length,
      lastTag: (p.querySelector('.tl-who-row .dim') || {}).textContent || '',
      more: (p.querySelector('.tl-more') || {}).textContent || '',
      last: [...p.querySelectorAll('.tl-events .logline')].map((r) =>
        r.textContent.replace(/\s+/g, ' ').trim().slice(0, 140)),
    })),
    emptyShown: !!document.querySelector('.tl-list') === false && !!document.querySelector('.card .empty'),
    logRows: [...document.querySelectorAll('.logbox .logline')].length,
    kindOn: segText('.log-controls .seg').slice(1, 2),
    eventChoice: (() => {
      const f = [...document.querySelectorAll('.field')].find((x) => {
        const l = x.querySelector('label'); return l && l.textContent.indexOf('事件') >= 0; });
      const s = f && f.querySelector('select');
      return s ? s.value : '';
    })(),
    overflow: document.documentElement.scrollWidth - window.innerWidth,
  };
})()"""


def click_seg(b, text):
    """点 .log-controls 里那一排分段按钮中的某一枚（按可见文字）。"""
    return b.js("""(() => {
      const el = [...document.querySelectorAll('.log-controls .seg button')]
        .find((x) => x.textContent.trim() === %s);
      if (!el) return false;
      el.click(); return true;
    })()""" % json.dumps(text))


def click_chip(b, who, text):
    return b.js("""(() => {
      const p = [...document.querySelectorAll('.tl-player')]
        .find((x) => ((x.querySelector('.tl-who') || {}).textContent || '').trim() === %s);
      const el = p && [...p.querySelectorAll('.tl-who-row button.chip')]
        .find((b) => b.textContent.trim().indexOf(%s) === 0);
      if (!el) return false;
      el.click(); return true;
    })()""" % (json.dumps(who), json.dumps(text)))


def click_btn(b, text):
    """点卡头上那枚按文字找的按钮（重新数一遍），不是分段控件。"""
    return b.js("""(() => {
      const el = [...document.querySelectorAll('.tl-head button')]
        .find((x) => x.textContent.trim() === %s);
      if (!el) return false;
      el.click(); return true;
    })()""" % json.dumps(text))


def chat_lines(n):
    """造 n 行小满的发言。时钟逐行递增，免得同钟同句被去重键合成一条。"""
    return "".join("[00:10:%02d]: (KU_fakeid002) 小满：折叠实测第 %d 句\n" % (i, i) for i in range(n))


def main():
    w = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    os.makedirs(OUT, exist_ok=True)
    b = Browser(w, 900 if w > 560 else 844)
    try:
        # 顺序必须是"先落到面板这个源上，再写凭据"：这个页签是共享的，
        # 停在 about:blank 或别的源上写 sessionStorage 等于没写（同 ui_shots.py 那条）。
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
        time.sleep(1.2)

        v = b.js(READ)
        segs = (v["viewSegs"] or [[]])[0]
        check("默认还是逐行日志（老用户进来看到的和以前一样）",
              segs == ["逐行日志[开]", "玩家时间线"], segs)
        check("逐行日志那一屏照常给：日志框在、筛选面板在、流信息在",
              v["logRows"] > 0 and v["filtersShown"] and v["metaShown"],
              "行数 %s 筛选 %s 元信息 %s" % (v["logRows"], v["filtersShown"], v["metaShown"]))
        check("看什么这一排只有两枚，且只有一枚是开的",
              sum(1 for s in segs if s.endswith("[开]")) == 1, segs)

        if not click_seg(b, "玩家时间线"):
            raise SystemExit("「玩家时间线」这一枚没找到")
        if not b.wait("!!document.querySelector('.tl-list')"):
            raise SystemExit("切过去以后时间线没出来")
        time.sleep(0.8)
        v = b.js(READ)
        check("切到时间线以后，逐行日志那三样都收掉了（不留按下去没反应的东西）",
              not v["filtersShown"] and not v["metaShown"] and not v["switchOn"],
              "筛选 %s 元信息 %s 跟随 %s" % (v["filtersShown"], v["metaShown"], v["switchOn"]))
        check("归出了人（一个都不归就是聚合或夹具坏了）", v["people"] >= 3, v["people"])
        check("头部说清了窗口、人数和下数",
              "每份文件回看" in v["head"] and "个人" in v["head"] and "行为" in v["head"], v["head"][:150])
        # 头部那句"N 下行为"和每个人右边芯片上的数字必须能对上：这两处一处错就是
        # 界面在报一个自己数不出来的数。
        nums = []
        for blk in v["blocks"]:
            for c in blk["chips"]:
                m = re.search(r"(\d+)\s*$", c["t"] or "")
                if m:
                    nums.append(int(m.group(1)))
        head_n = re.search(r"(\d+) 下行为", v["head"])
        check("头部那句「N 下行为」等于每个人芯片数字加起来",
              head_n is not None and sum(nums) == int(head_n.group(1)) and sum(nums) > 0,
              "头部 %s / 芯片合计 %s" % (head_n and head_n.group(1), sum(nums)))
        # 夹具里那条 [Join Announcement] 抄本与服务端那行同钟同句，合掉了才说明
        # "玩家进服 1"不是把同一下数了两遍。
        check("两份文件里同一条公告合成一记，头部说了合掉几条",
              "抄本合掉了 1 条" in v["head"], v["head"][:170])
        check("回看范围那一排含后端给的默认档",
              any("500 行" in x for x in (v["rangeSegs"] or [[]])[0]), v["rangeSegs"])

        codes = [x for x in ("player_", "afk_", "ghost_gone", "auth_", "_start", "_end")
                 if x in v["cardText"]]
        check("卡面上一个码值都没有（只能有中文）", not codes, codes)

        bywho = {x["who"]: x for x in v["blocks"]}
        check("那个死过又被复活的人被归到同一条（名字进服、死亡、复活都在）",
              "小满" in bywho and len(bywho["小满"]["chips"]) >= 6,
              bywho.get("小满", {}).get("chips"))
        xm = bywho.get("小满", {"chips": [], "last": [], "lastTag": ""})
        tones = {c["t"]: c["tone"] for c in xm["chips"]}
        check("坏消息和好转上了色，进出没上色",
              any(t.startswith("玩家死亡") and tones[t] == "warn" for t in tones) and
              any(t.startswith("玩家被复活") and tones[t] == "ok" for t in tones) and
              all(tones[t] == "" for t in tones if t.startswith("玩家进服")),
              tones)
        # 时间写法必须整屏统一：这一屏问的就是"谁先谁后"，
        # 一行 22:59:10 一行 [00:01:00] 的话，眼睛得先做一次换算。
        check("逐条行为的时间全用同一种写法（这份夹具能换算出墙上时间）",
              len(xm["last"]) >= 6 and all(TIME.match(r) for r in xm["last"]),
              xm["last"][:3])
        check("「最后一次」跟下面的行同一写法",
              bool(TIME_ONLY.match((xm["lastTag"] or "").replace("最后一次", "").strip())),
              xm["lastTag"])
        # 原文里已经点出对象的事件，后面那句"（对象：…）"就是重复。
        dup = []
        for blk in v["blocks"]:
            for r in blk["last"]:
                m = re.search(r"（对象：([^）]+)）", r)
                if m and m.group(1) in r[:m.start()]:
                    dup.append(r)
        check("同一个名字不在一行里被「对象」说两遍", not dup, dup[:2])

        # 假链接检查：芯片点过去必须筛得出原文，而且带上了它出自哪一份文件。
        if not click_chip(b, "阿栗", "玩家聊天"):
            raise SystemExit("阿栗名下那枚「玩家聊天」芯片没找到")
        if not b.wait("!!document.querySelector('.logbox .logline')"):
            raise SystemExit("点了芯片以后逐行日志没有行——那是个假链接")
        time.sleep(0.6)
        v = b.js(READ)
        check("点聊天芯片以后自动切回逐行日志，并且筛出了话",
              v["logRows"] > 0 and v["eventChoice"] == "chat",
              "行数 %s 事件筛选 %s" % (v["logRows"], v["eventChoice"]))
        check("聊天那一记带的是聊天与公告这份文件（不是拿服务端日志去筛）",
              any("聊天与公告[开]" in x for grp in v["kindOn"] for x in grp), v["kindOn"])

        # 回到时间线，把窗口收到最小：必须说出"只数了 50 行"，
        # 并且空着的时候给的是同一屏上点得动的出口。
        if not click_seg(b, "玩家时间线"):
            raise SystemExit("回不到玩家时间线")
        if not b.wait("!!document.querySelector('.tl-list')"):
            raise SystemExit("回到时间线以后列表没出来")
        if not click_seg(b, "50 行"):
            raise SystemExit("回看范围里没有 50 行这一档")
        if not b.wait("document.querySelector('.tl-head') && document.querySelector('.tl-head').textContent.indexOf('50 行') >= 0"):
            raise SystemExit("窗口切到 50 行以后头部没跟着改")
        time.sleep(0.8)
        v = b.js(READ)
        check("窗口切到 50 行以后，头部和提示都说的是 50 行（数字必须和真发出去的一致）",
              "每份文件回看 50 行" in v["head"] and any("最近 50 行" in n for n in v["notes"]),
              {"head": v["head"][:120], "notes": v["notes"]})
        if v["people"] == 0:
            check("窗口里没有行为时给的是出口而不是空白（两个按钮都在同一屏）",
                  v["emptyShown"] and bool(b.js(
                      "(() => { const t=[...document.querySelectorAll('.card button')]"
                      ".map((x)=>x.textContent.trim());"
                      "return t.some((x)=>x.indexOf('窗口放到最大')===0)&&t.some((x)=>x.indexOf('去看逐行日志')===0); })()")),
                  v["emptyShown"])
            if not click_seg(b, "2000 行"):
                raise SystemExit("最大窗口那一档没点着")
            if not b.wait("!!document.querySelector('.tl-list')"):
                raise SystemExit("放到最大以后还是没归出人")
            time.sleep(0.8)
            v = b.js(READ)
        check("把窗口放到 2000 行时又归得出人（空状态是真窗口太小，不是聚合坏了）",
              v["people"] >= 3, v["people"])

        # 回到默认档再截图：这张图要代表的是"人正常进来看到的那一屏"，
        # 而不是上一项为了试空状态临时压到 50 行的样子。
        if not click_seg(b, "500 行"):
            raise SystemExit("回不到默认窗口那一档")
        if not b.wait("document.querySelector('.tl-head') && document.querySelector('.tl-head').textContent.indexOf('500 行') >= 0"):
            raise SystemExit("回到默认档以后头部没跟着改")
        time.sleep(0.8)
        v = b.js(READ)
        check("切回默认档以后头部又写回 500 行",
              "每份文件回看 500 行" in v["head"] and v["people"] >= 3, v["head"][:120])
        check("头部那句「本轮从 … 开始」用的是日志开头那行 Current time",
              bool(re.search(r"本轮从 \d{4}-\d{2}-\d{2} \d{2}:\d{2} 开始", v["head"])), v["head"][:170])

        def chat_count_of(blocks, who):
            blk = blocks.get(who) or {}
            for c in blk.get("chips") or []:
                m = re.search(r"(\d+)\s*$", c["t"] or "")
                if m and (c["t"] or "").startswith("玩家聊天"):
                    return int(m.group(1))
            return 0

        # 一个人几百行时后端只列最近 40 行（数字照旧全算）。正常夹具一个人就八行，
        # 走不到这一档，所以这里临时往聊天夹具里灌 45 行，验完原样还原。
        # 这一条值得为它动一次文件：折了明细不折数字、以及"折掉多少"那句话，
        # 只在真的折的时候才看得见，而这两处恰恰是最容易说错话的地方。
        before = open(CHAT, "rb").read() if os.path.exists(CHAT) else None
        base_chat = chat_count_of({x["who"]: x for x in v["blocks"]}, "小满")
        try:
            with open(CHAT, "ab") as fh:
                fh.write(chat_lines(45).encode("utf-8"))
            if not click_btn(b, "重新数一遍"):
                raise SystemExit("找不到「重新数一遍」那一枚")
            if not b.wait("[...document.querySelectorAll('.tl-more')].length > 0"):
                raise SystemExit("灌进 45 行以后没有出现折叠说明")
            time.sleep(0.6)
            v = b.js(READ)
            folded = {x["who"]: x for x in v["blocks"]}
            xm2 = folded.get("小满") or {}
            check("一个人行多了以后，列出的明细停在最近那一档",
                  xm2.get("rows") == 40, xm2.get("rows"))
            now_chat = chat_count_of(folded, "小满")
            check("折的是明细、不是数字（发言计数把灌进去的 45 句全算了）",
                  now_chat == base_chat + 45, "现在 %s，折之前 %s" % (now_chat, base_chat))
            check("折叠那句说明报出了折掉几条，并指回逐行日志",
                  "另有" in (xm2.get("more") or "") and "点芯片" in (xm2.get("more") or ""),
                  (xm2.get("more") or "")[:140])
            nums2 = []
            for blk in v["blocks"]:
                for c in blk["chips"]:
                    m = re.search(r"(\d+)\s*$", c["t"] or "")
                    if m:
                        nums2.append(int(m.group(1)))
            head2 = re.search(r"(\d+) 下行为", v["head"])
            check("折过明细以后头部那句「N 下行为」仍然等于芯片之和",
                  head2 is not None and sum(nums2) == int(head2.group(1)),
                  "头部 %s / 芯片合计 %s" % (head2 and head2.group(1), sum(nums2)))
        finally:
            if before is not None:
                with open(CHAT, "wb") as fh:
                    fh.write(before)
        click_btn(b, "重新数一遍")
        time.sleep(1.0)
        v = b.js(READ)
        restored = {x["who"]: x for x in v["blocks"]}
        check("把灌进去的行还原以后，折叠说明跟着消失（不是常驻文案）",
              not (restored.get("小满") or {}).get("more") and
              chat_count_of(restored, "小满") == base_chat,
              (restored.get("小满") or {}).get("more"))

        b.js("document.querySelector('.tl-head') && document.querySelector('.tl-head').scrollIntoView({block:'start'});")
        time.sleep(0.3)
        b.shoot(os.path.join(OUT, "timeline_%d.png" % w))
        check("这一档宽度没有横向溢出", v["overflow"] <= 0, v["overflow"])
        check("这一路没有未捕获异常", not (b.js("window.__perr || []") or []), b.js("window.__perr || []"))
    finally:
        b.close()

    print("\n视口 %s：%d 项检查，%d 项没通过" % (w, len(REPORT), len(FAILED)))
    if FAILED:
        for f in FAILED:
            print("  红: %s" % f)
        sys.exit(1)


if __name__ == "__main__":
    main()
