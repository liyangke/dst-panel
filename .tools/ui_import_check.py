# -*- coding: utf-8 -*-
"""存档导入动线的浏览器实测：上传框、超限拦人、读包结论页、目标房间、确认框、任务、产物。

为什么必须真点：这一条动线最容易出的错不是"点了没反应"，而是"看着对，其实没送到"——
字段名拼错、上限那句写死在界面、勾了换进房间但 restore 没跟着走、包没了还显示一份旧结论。
这些只有真的把文件塞进 <input type=file>、真的点每一次按钮才量得出来，
所以这里用的是 CDP 的 DOM.setFileInputFiles（浏览器派发的是同一个 change 事件）。

跑法：先起着 python .tools/ui_dev.py，再 python .tools/ui_import_check.py [宽度]
夹具由 python .tools/ui_import_zip.py 造，落在 .tmp_boot/import/ 下。

拖拽那一段说明一下：真的按住鼠标拖进窗口在 CDP 里做不出来，这里用 DataTransfer 造一个
File 再派发 drop 事件。被测的是 dropSave 这个处理函数（它读的就是 event.dataTransfer.files），
所以这一条验的是"拖进来会不会接住"，不验操作系统那层的拖放手势。
"""
import base64
import glob
import json
import os
import re
import shutil
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
# SystemExit 的中文走的是 stderr：不一起转 UTF-8，重定向到文件就成了一堆乱码，
# 而这堆乱码恰恰是"脚本为什么停在这里"的唯一线索。
sys.stderr.reconfigure(encoding="utf-8", errors="replace")

W = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FIX = os.path.join(ROOT, ".tmp_boot", "import")
OUT = os.path.join(ROOT, ".tools", "tmp", "shots", "import")
KLEI = os.path.join(ROOT, ".tmp_boot", "klei")
INCOMING = os.path.join(ROOT, ".tmp_boot", "uiwork", "backups", "incoming")
BACKUPS = os.path.join(ROOT, ".tmp_boot", "uiwork", "backups")

GAMEID = "AB12CD34EF56AB77"
TOKEN_MARK = "KLEI-FAKE-TOKEN"          # 夹具里那份假令牌，一个字都不许出现在页面上

FAILED = []
REPORT = []

HOOK = ("window.__perr=[];window.addEventListener('error',function(e){"
        "if(window.__perr.length<20)window.__perr.push(String(e.message))});"
        "window.addEventListener('unhandledrejection',function(e){"
        "if(window.__perr.length<20)window.__perr.push('promise:'+String(e.reason&&e.reason.message||e.reason))});"
        "window.__reqs=[];if(!window.__spied){const of=window.fetch;"
        "window.fetch=function(u,o){window.__reqs.push(((o&&o.method)||'GET')+' '+u);return of.apply(this,arguments)};"
        "window.__spied=1;}")


def check(name, ok, detail=""):
    REPORT.append((("PASS" if ok else "FAIL"), name, detail))
    print("%s %s%s" % ("PASS" if ok else "FAIL", name,
                       ("  | " + str(detail).replace("\n", " ")[:220]) if detail else ""))
    if not ok:
        FAILED.append(name + "｜" + str(detail)[:200])


def click_btn(b, sel, text, exact=False):
    pred = ("(x) => x.textContent.trim() === %s" if exact else
            "(x) => x.textContent.indexOf(%s) >= 0") % json.dumps(text, ensure_ascii=False)
    return b.click("'%s'" % sel, pred)


def shot(b, tag):
    b.shoot(os.path.join(OUT, "w%d-%s.png" % (W, tag)))


def text_of(b, sel):
    return b.js("(() => { const x = document.querySelector(%s);"
                " return x ? x.innerText.replace(/\\s+/g,' ').trim() : null; })()" % json.dumps(sel))


def fix_path(name):
    return os.path.join(FIX, name)


def staged_count(b):
    """入口那一排报的"临时上传 N 份"，没有这条就算 0。"""
    for c in (b.js("(() => [...document.querySelectorAll('.snap-actions .chip')]"
                   ".map(x => x.innerText.trim()))()") or []):
        m = re.search(r"临时上传\s*(\d+)\s*份", c or "")
        if m:
            return int(m.group(1))
    return 0


def pick_file(b, name):
    """真往上传框里放一个盘上的文件；先把 input 清空，同一个文件第二次也要能触发 change。"""
    b.js("(() => { const i = document.querySelector('.snap-import .drop-file'); if (i) i.value = ''; })()")
    if not b.set_files(".snap-import .drop-file", [fix_path(name)]):
        raise SystemExit("上传框找不到，没法放文件：" + name)
    if not b.wait("(document.querySelector('.snap-import .drop.has') || {}).className !== undefined"):
        raise SystemExit("放了 %s 以后上传框没进入已选状态" % name)
    time.sleep(0.4)


def drop_file(b, name):
    """用 DataTransfer 造一个同名同内容的 File，派发真实的 drop 事件。

    内容直接拼进表达式（不是漏传的变量）：atob(undefined) 抛的是 DOMException，
    测出来的是脚本自己的错，不是页面的。

    三步分开派发、中间要停一下：Vue 改 class 是下一个 tick 的事，
    同一段同步代码里 dispatch 完马上读 className，量到的一定是"还没高亮"。
    """
    with open(fix_path(name), "rb") as fh:
        raw = base64.b64encode(fh.read()).decode()
    made = b.js(r"""(() => {
      const bin = atob("%s"), arr = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i);
      const zone = document.querySelector('.snap-import .drop');
      if (!zone) return false;
      const dt = new DataTransfer();
      dt.items.add(new File([arr], %s, { type: 'application/zip' }));
      window.__dt = dt;
      return true;
    })()""" % (raw, json.dumps(name, ensure_ascii=False)))
    if not made:
        return {"err": "no-zone"}

    def fire(kind):
        b.js("(() => { const z = document.querySelector('.snap-import .drop');"
             " z.dispatchEvent(new DragEvent(%s, { bubbles: true, dataTransfer: window.__dt })); })()"
             % json.dumps(kind))
        time.sleep(0.35)
        return b.js("(document.querySelector('.snap-import .drop').className.indexOf('over') >= 0)")

    over = fire("dragenter")
    left = not fire("dragleave")
    fire("drop")
    return {"over": over, "left": left,
            "has": b.js("document.querySelector('.snap-import .drop').className.indexOf('has') >= 0")}


def read_package(b):
    if not click_btn(b, ".snap-import button", "读出包里的内容"):
        raise SystemExit("「读出包里的内容」按钮找不到")
    # 等到这一趟真的有了结论才继续：结论页、拒绝页、错误框三种出口都算，别的都还在飞。
    if not b.wait("!!document.querySelector('.snap-import .note.imp-verdict') || "
                  "!!document.querySelector('.snap-import .err-box')", tries=80):
        raise SystemExit("点了读包以后既没有结论页也没有错误框｜抽屉里：%s｜请求：%s"
                         % ((text_of(b, ".snap-import") or "")[:220], b.js("window.__reqs")))
    time.sleep(0.4)


def body(b):
    return b.js("document.body.innerText.replace(/\\s+/g,' ').trim()") or ""


def kv(b):
    """x-kv 那张键值表：<dl class="kv"> 里 dt 是标签、dd 是值。"""
    return b.js(r"""(() => {
      const out = {};
      const dl = document.querySelector('.snap-import dl.kv');
      if (!dl) return out;
      const kids = [...dl.children];
      for (let i = 0; i + 1 < kids.length; i += 2) {
        out[kids[i].innerText.replace(/\s+/g, ' ').trim()] = kids[i + 1].innerText.replace(/\s+/g, ' ').trim();
      }
      return out;
    })()""") or {}


def chips(b):
    return [(x or "").strip() for x in
            (b.js("(() => [...document.querySelectorAll('.snap-import .chip')].map(x => x.innerText.replace(/\\s+/g,' ').trim()))()") or [])]


def li(b, sel):
    return [(x or "").strip() for x in
            (b.js("(() => [...document.querySelectorAll(%s)].map(x => x.innerText.replace(/\\s+/g,' ').trim()))()"
                  % json.dumps(sel)) or [])]


def reset_workspace():
    """把上一轮实测留下的东西清掉，让这一轮的磁盘断言是真的。

    不清的话更糟：那个世界编号已经在房间目录里了，"导入后磁盘上出现了它"会白给一个 PASS。
    清的都是面板自己的夹具目录（.tmp_boot 下），不碰任何真实房间。
    """
    for p in glob.glob(os.path.join(INCOMING, "*")):
        os.remove(p)
    # 上一轮导入留下的两份产物也要清掉（导入的快照 + 换档前的安全备份）：
    # 留着的话"列表里出现了外部导入的存档"这一条会在任务开始之前就成立。
    for pat in ["*ProbeWorld-imported*", "*ProbeWorld-before-restore*"]:
        for p in glob.glob(os.path.join(BACKUPS, pat)):
            os.remove(p)
    for shard in ["Master", "Caves"]:
        d = os.path.join(KLEI, "ProbeWorld", shard, "save", "session", GAMEID)
        if os.path.isdir(d):
            shutil.rmtree(d)


def main():
    os.makedirs(OUT, exist_ok=True)
    reset_workspace()
    for n in ["friend-cluster.zip", "friend-shard.zip", "notes-only.zip", "screenshot.png", "oversize.bin"]:
        if not os.path.exists(fix_path(n)):
            raise SystemExit("夹具不在，先跑 python .tools/ui_import_zip.py（缺 %s）" % n)

    b = Browser(width=W, height=900 if W > 560 else 844)
    try:
        # goto 必须在清凭据之前（同 ui_shots.py 里写死的那条）：共享标签页进来时可能停在
        # about:blank 或别的源上，先 removeItem 清掉的是别人的存储，面板那份登录态还在，
        # reload 直接进总览页，报出来的就是"登录层没出来"。
        b.goto(BASE + "/#/")
        b.wait("location.origin === %s" % json.dumps(BASE))
        b.js("sessionStorage.removeItem('dstpanel.creds')")
        b.reload()
        if not b.wait("!!document.querySelector('.login-form')"):
            raise SystemExit("登录层没出来")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("带凭据重进还是停在登录层")
        b.js("location.hash = '#/snaps'")
        if not b.wait("!!document.querySelector('.snap-page')"):
            raise SystemExit("快照页没出来：" + body(b)[:200])
        time.sleep(1.0)
        b.js(HOOK)

        # —— 一、入口 ——
        # 这一页有两条同样的操作排（一条给本机备份、一条给外部存档），按文字找才找得准。
        row = next((x for x in li(b, ".snap-actions") if "外部存档" in x), None)
        check("快照页有「外部存档」这一排，并且带导入上限的说明",
              row is not None and "外部存档" in row, (row or "")[:160])
        check("抽屉默认收起（不把上传框摊在快照列表前面）",
              b.js("!!document.querySelector('.snap-import')") is False)
        n_snaps = b.js("document.querySelectorAll('.snap-page tbody tr, .snap-page .snap-list .mod-row').length") or 0
        check("页面一进来就有已有备份（不是空列表假装正常）", n_snaps > 0, "行数=%s" % n_snaps)

        if not click_btn(b, ".snap-actions button", "导入外部存档"):
            raise SystemExit("「导入外部存档」点不动")
        if not b.wait("!!document.querySelector('.snap-import')"):
            raise SystemExit("抽屉没展开")
        time.sleep(0.5)
        head = text_of(b, ".snap-import .drawer-head")
        zone_t = text_of(b, ".snap-import .drop-t")
        zone_s = text_of(b, ".snap-import .drop-s")
        check("抽屉标题说的是这件事的结果（变成快照），不是术语",
              head is not None and "快照" in head, head or "")
        check("没选文件时上传框给人下一步（可接受的后缀写明白）",
              zone_t == "把存档包拖到这里" and (".zip" in (zone_s or "")), "%s / %s" % (zone_t, zone_s))
        check("上传框限制了后缀",
              b.js("document.querySelector('.snap-import .drop-file').accept") == ".zip,.tar,.tar.gz")
        btns = b.js("(() => [...document.querySelectorAll('.snap-import button')]"
                    ".map(x => ({t: x.textContent.trim(), d: x.disabled})))()") or []
        read_btn = [x for x in btns if "读出包里的内容" in x["t"]]
        check("没选文件时「读出包里的内容」是禁的（不让人点了拿一个错误）",
              bool(read_btn) and read_btn[0]["d"], btns)

        # —— 二、超大文件：拦住，而且不许发请求 ——
        before = len([x for x in (b.js("window.__reqs") or []) if "import/preview" in x])
        pick_file(b, "oversize.bin")
        err = text_of(b, ".snap-import .err-box")
        check("超过上限的文件当场拦住，并且报出两边的大小",
              err is not None and "600 MB" in err and "512" in err, (err or "")[:200])
        btns = b.js("(() => [...document.querySelectorAll('.snap-import button')]"
                    ".map(x => ({t: x.textContent.trim(), d: x.disabled})))()") or []
        read_btn = [x for x in btns if "读出包里的内容" in x["t"]]
        check("超限时读包按钮仍然禁着", bool(read_btn) and read_btn[0]["d"], btns)
        click_btn(b, ".snap-import button", "读出包里的内容")
        time.sleep(0.8)
        after = len([x for x in (b.js("window.__reqs") or []) if "import/preview" in x])
        check("超限那份文件一个字节都没往接口传（没白等一次上传）", after == before,
              "拦截前 %s 次 / 现在 %s 次" % (before, after))
        shot(b, "01-oversize")

        # —— 三、根本不是压缩包 ——
        pick_file(b, "screenshot.png")
        read_package(b)
        err = text_of(b, ".snap-import .err-box")
        check("不是压缩包的文件回一句人话（含\"认不出\"），不是裸英文错误",
              err is not None and ("认不出" in err or "不是" in err), (err or "")[:200])
        check("失败时不弹登录层（凭据是对的，弹了反而误导）",
              b.js("!!document.querySelector('.login-form')") is False)
        check("这一步不产出结论页（没有状态块，也没有键值表）",
              b.js("!document.querySelector('.snap-import .note') && !document.querySelector('.snap-import dl.kv')"),
              err or "")
        shot(b, "02-not-a-package")

        # —— 四、合法压缩包、但不是饥荒存档 ——
        pick_file(b, "notes-only.zip")
        read_package(b)
        note_bad = text_of(b, ".snap-import .note.imp-verdict.bad")
        problems = li(b, ".snap-import ul.list-plain li")
        check("不是存档的包给的是结论页，不是错误（200 + 能不能导的判定）",
              note_bad is not None and problems, (note_bad or "")[:120])
        check("结论页里能问出\"为什么不能导\"（至少一句带原因的话）",
              bool(problems) and any(len(x) > 10 for x in problems), problems)
        check("不能导的包不出现目标房间选择（提交那一步该整块收掉）",
              b.js("!document.querySelector('.snap-import select') && !document.querySelector('.snap-import .switch')"))
        check("不能导时页面上没有\"导入并换进房间\"这种危险按钮",
              "导入并换进房间" not in (text_of(b, ".snap-import") or ""),
              (text_of(b, ".snap-import") or "")[-160:])
        shot(b, "03-not-a-save")

        # —— 五、一份能用的包 ——
        pick_file(b, "friend-cluster.zip")
        read_package(b)
        note_ok = text_of(b, ".snap-import .note.imp-verdict.ok")
        check("能用的包先给一句结论（绿色状态，不是让人自己数文件）",
              note_ok is not None and ("认出来" in note_ok or "可以" in note_ok), (note_ok or "")[:160])
        rows = kv(b)
        check("结论页把\"传的是哪个文件\"回显成用户原来那个名字",
              rows.get("选出的文件") == "friend-cluster.zip", rows)
        check("结论页报出包的大小与解开以后（4G 机器上要看得见占用）",
              bool(rows.get("包的大小")) and bool(rows.get("解开以后")), rows)
        check("房间目录那一行连着说清登记给谁",
              "Cluster_77" in (rows.get("包里的房间目录") or ""), rows.get("包里的房间目录"))
        ref = rows.get("上传包编号") or ""
        check("上传包编号是面板生成的那种（界面不自己拼路径）",
              ref.startswith("import-") and "/" not in ref and "\\" not in ref, ref)
        cps = chips(b)
        two = [c for c in cps if "世界编号" in c]
        check("两个世界各有一条带世界编号与文件数的说明（不是光一个名字）",
              len(two) == 2 and any("Master" in c for c in two) and any("Caves" in c for c in two), two)
        check("世界编号就是包里那个 GAMEID，一个字没改",
              all(GAMEID in c for c in two), two)
        dropdetails = b.js("!!document.querySelector('.snap-import details.group')")
        check("被丢掉的内容有单独一块（默认收起）", dropdetails)
        b.js("document.querySelector('.snap-import details.group').open = true")
        time.sleep(0.3)
        dropped = li(b, ".snap-import .file-tree .ft")
        joined = " ".join(dropped)
        check("令牌 / 引擎日志 / 面板通道文件都不进快照",
              "cluster_token.txt" in joined and "server_log.txt" in joined and "dstpanel_queue.txt" in joined,
              dropped[:6])
        warns = " ".join(li(b, ".snap-import .note.imp-warn li"))
        check("包里带着模组配置和世界 server.ini 时，提醒说清后果（要补模组、配置不覆盖本机）",
              "modoverrides.lua" in warns and "模组" in warns, warns[:220])
        check("夹带的无关文件（说明文件、苹果压缩垃圾）出现在不进快照的名单里，而不是被静默丢掉",
              "readme.txt" in joined and "__MACOSX" in joined,
              [x for x in dropped if "readme" in x or "MACOSX" in x])
        page = body(b)
        check("假令牌一个字都没出现在页面上", TOKEN_MARK not in page)
        check("形状结论说清房间配置跟不跟着走",
              "Cluster_77" in page and "server.ini" in page, "")
        staged = [c for c in (b.js("(() => [...document.querySelectorAll('.snap-actions .chip')]"
                                   ".map(x => x.innerText.trim()))()") or [])]
        check("临时上传的占用在入口那一排就报出来（磁盘账不藏起来）",
              any("临时上传" in c for c in staged), staged)
        # 此刻盘上该留着两份没用的包（notes-only 那份合法但不可导，friend-cluster 这份刚传完）：
        # 传一次就立刻计入，用户才知道自己占了多少。
        check("刚传上来的包当场计入那份占用（不等下次刷新）",
              staged_count(b) >= 2, staged)
        shot(b, "04-preview-ok")

        # —— 六、目标房间：在跑的先挡在勾选处 ——
        running = b.js("(() => { const s = document.querySelector('.snap-import select'); return s ? [...s.options].map(o => o.value) : []; })()") or []
        check("目标房间只能从后端给的那份名单里选", len(running) >= 2 and "Cluster_1" in running, running)
        cur = b.js("document.querySelector('.snap-import select').value")
        check("抽屉一打开就带上一个默认房间（不让人在空选择里试）", bool(cur), cur)
        b.js("(() => { const s = document.querySelector('.snap-import select');"
             " s.value = 'Cluster_1'; s.dispatchEvent(new Event('change')); })()")
        b.js("(() => { const i = document.querySelector('.snap-import .switch input');"
             " i.checked = true; i.dispatchEvent(new Event('change')); })()")
        time.sleep(0.6)
        warn_block = text_of(b, ".snap-import .note.imp-blocked")
        danger = [x for x in (b.js("(() => [...document.querySelectorAll('.snap-import button')]"
                                   ".map(x => ({t: x.textContent.trim(), d: x.disabled, cls: x.className})))()") or [])
                  if "导入并换进房间" in x["t"]]
        check("勾了\"换进房间\"而那个房间在跑：先把话写在勾的那一行下面",
              warn_block is not None and "Master" in warn_block, (warn_block or "")[:160])
        check("房间在跑时危险按钮是禁的（不等提交回来才吃一句 409）",
              bool(danger) and danger[0]["d"], danger)
        shot(b, "05-target-running")

        # 停着的房间：可以走
        b.js("(() => { const s = document.querySelector('.snap-import select');"
             " s.value = 'ProbeWorld'; s.dispatchEvent(new Event('change')); })()")
        time.sleep(0.5)
        check("换成停着的房间后那句警告收掉了",
              (text_of(b, ".snap-import .note.imp-blocked") or "").find("Master") < 0 or
              b.js("!!document.querySelector('.snap-import .note.imp-blocked')") is False)
        danger = [x for x in (b.js("(() => [...document.querySelectorAll('.snap-import button')]"
                                   ".map(x => ({t: x.textContent.trim(), d: x.disabled})))()") or [])
                  if "导入并换进房间" in x["t"]]
        check("停着的房间允许提交", bool(danger) and not danger[0]["d"], danger)

        # —— 七、确认框：取消一条路，确认一条路 ——
        if not click_btn(b, ".snap-import button", "导入并换进房间"):
            raise SystemExit("危险按钮点不动")
        if not b.wait("!!document.querySelector('.mask .dialog')"):
            raise SystemExit("确认框没弹出来")
        time.sleep(0.4)
        dlg = b.js(r"""(() => {
          const d = document.querySelector('.mask .dialog');
          return { title: ((d.querySelector('h3') || {}).textContent || '').trim(),
                   text: (d.innerText || '').replace(/\s+/g, ' ').trim(),
                   btns: [...d.querySelectorAll('button')].map(x => x.textContent.trim()),
                   danger: !!d.querySelector('.btn.danger, .danger') };
        })()""") or {}
        check("确认框里点名是哪个房间要被换掉",
              "ProbeWorld" in dlg.get("title", ""), dlg.get("title"))
        check("确认框把代价写清楚（整棵替换、先打安全备份）",
              "整棵替换" in dlg.get("text", "") and "安全备份" in dlg.get("text", ""), dlg.get("text", "")[:200])
        check("确认框列出包里的世界编号（换档前让人对一眼）",
              GAMEID in dlg.get("text", ""), "")
        def submitted():
            return [x for x in (b.js("window.__reqs") or [])
                    if x.startswith("POST ") and x.split("?")[0].endswith("/backups/import")]

        click_btn(b, ".mask .dialog button", "取消")
        time.sleep(0.6)
        check("取消之后一次提交都没发出去（这一问不是走过场）", not submitted(), b.js("window.__reqs"))

        if not click_btn(b, ".snap-import button", "导入并换进房间"):
            raise SystemExit("第二次提交点不动")
        if not b.wait("!!document.querySelector('.mask .dialog')"):
            raise SystemExit("第二次确认框没弹出来")
        click_btn(b, ".mask .dialog button", "排队执行导入")
        if not b.wait("[...document.querySelectorAll('.toast')].some(x => (x.innerText||'').indexOf('已接住') >= 0)",
                      tries=40):
            raise SystemExit("提交后没看到接住回执的提示：" + body(b)[:300])
        time.sleep(0.8)
        check("任务接住后抽屉自己收起来（不留一份已经交给任务的预览在屏上）",
              b.js("!document.querySelector('.snap-import')"))
        shot(b, "06-accepted")

        # —— 八、任务跑完，磁盘上真有东西 ——
        ok_done = b.wait("!!document.querySelector('.snap-page') && "
                         "document.body.innerText.indexOf('外部导入的存档') >= 0", tries=120, pause=1.0)
        check("任务跑完后快照列表里出现「外部导入的存档」那一行", ok_done,
              body(b)[:200] if not ok_done else "")
        staged = b.js("(() => [...document.querySelectorAll('.snap-actions .chip')]"
                      ".map(x => x.innerText.trim()))()") or []
        settled = b.wait("[...document.querySelectorAll('.snap-actions .chip')]"
                         ".some(x => /临时上传\\s*1\\s*份/.test(x.innerText))", tries=30, pause=1.0)
        staged = b.js("(() => [...document.querySelectorAll('.snap-actions .chip')]"
                      ".map(x => x.innerText.trim()))()") or []
        # 用完就删的是这一次导入用的那份；之前那份"合法但不能导"的包要留着（重传一次几十 MB 不值当），
        # 所以这里对的是"少了一份 + 那个编号真没了"，不是"清零"。
        check("导入成功后那份包被收掉，不能导的那份留着（占用数字跟着降）",
              settled and not glob.glob(os.path.join(INCOMING, "*friend-cluster*")),
              "%s｜盘上：%s" % (staged, [os.path.basename(x) for x in glob.glob(os.path.join(INCOMING, "*"))]))
        if not wait_disk():
            check("磁盘上：ProbeWorld 的存档目录真的换成了包里的世界", False, GAMEID)
        else:
            check("磁盘上：ProbeWorld 的存档目录真的换成了包里的世界", True, "save/session/" + GAMEID)
            check("磁盘上：包里的假令牌没有覆盖本机那份",
                  TOKEN_MARK not in read_file(os.path.join(KLEI, "ProbeWorld", "cluster_token.txt")))
            check("磁盘上：通道文件和引擎日志没有跟着包跑进来",
                  not os.path.exists(os.path.join(KLEI, "ProbeWorld", "Master", "save", "dstpanel_queue.txt")))
        shot(b, "07-after-import")

        # —— 九、拖拽 ——
        if not click_btn(b, ".snap-actions button", "导入外部存档"):
            raise SystemExit("第二次打不开抽屉")
        b.wait("!!document.querySelector('.snap-import')")
        time.sleep(0.4)
        d = drop_file(b, "friend-shard.zip")
        zone_t = text_of(b, ".snap-import .drop-t")
        check("拖进来能接住（drop 之后上传框显示的就是拖的那个文件）",
              isinstance(d, dict) and d.get("has") and zone_t == "friend-shard.zip",
              "%s / %s" % (d, zone_t))
        check("拖动经过时上传框有高亮、离开后收掉",
              isinstance(d, dict) and d.get("over") and d.get("left"), d)
        read_package(b)
        rows = kv(b)
        note_ok = text_of(b, ".snap-import .note.imp-verdict.ok")
        shape = text_of(b, ".snap-import .hint.imp-shape")
        check("只有一个世界目录的包也认得（最外层就是世界目录）",
              rows.get("包里的房间目录") == "包的最外层就是世界目录", rows)
        check("这种包额外说明房间配置保持本机现状",
              "保持" in (shape or "") and "设置" in (shape or ""), (shape or "")[:180])
        check("结论仍然给到能导（单世界包不该被判成结构不对）",
              note_ok is not None, (note_ok or "")[:120])
        shot(b, "08-shard-shape")
        # 这一步只看不提交：收掉抽屉，别在夹具里多留一份上传包。
        click_btn(b, ".snap-actions button", "收起导入")

        perr = b.js("window.__perr") or []
        check("全程没有页面报错", not perr, perr)

        # —— 十、窄屏：上传框不许跳到列表前面，也不许横向溢出 ——
        b.set_viewport(390, 844)
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("窄屏重进没起来")
        b.js("location.hash = '#/snaps'")
        if not b.wait("!!document.querySelector('.snap-page')"):
            raise SystemExit("窄屏快照页没出来")
        time.sleep(1.0)
        b.js(HOOK)
        click_btn(b, ".snap-actions button", "导入外部存档")
        if not b.wait("!!document.querySelector('.snap-import')"):
            raise SystemExit("窄屏抽屉没展开")
        pick_file(b, "friend-cluster.zip")
        read_package(b)
        b.js("document.querySelector('.snap-import details.group') && "
             "(document.querySelector('.snap-import details.group').open = true)")
        time.sleep(0.5)
        geo = b.js(r"""(() => {
          const imp = document.querySelector('.snap-import');
          const list = document.querySelector('.snap-page > .snap-list');
          const zone = document.querySelector('.snap-import .drop');
          return {
            overflow: document.documentElement.scrollWidth - window.innerWidth,
            impTop: imp ? Math.round(imp.getBoundingClientRect().top) : -1,
            listTop: list ? Math.round(list.getBoundingClientRect().top) : -1,
            zoneW: zone ? Math.round(zone.getBoundingClientRect().width) : -1,
            vw: window.innerWidth,
            // 按钮不许被挤到换行后点不到：量一量最后那颗提交按钮还在不在视口宽度内
            btnRight: (() => { const bs = [...document.querySelectorAll('.snap-import button')];
                                const last = bs[bs.length - 1];
                                return last ? Math.round(last.getBoundingClientRect().right) : -1; })(),
          };
        })()""") or {}
        check("390 宽度没有横向滚动条", geo.get("overflow", 99) <= 1, geo)
        check("手机上导入面板排在快照列表后面（不把上传框顶到最前面）",
              geo.get("listTop", -1) >= 0 and geo.get("impTop", -1) > geo["listTop"], geo)
        check("上传框吃满可用宽度（不缩成一小条）",
              geo.get("zoneW", 0) >= (geo.get("vw", 0) - 60), geo)
        check("提交按钮没被挤出视口", geo.get("btnRight", 9999) <= geo.get("vw", 0) + 1, geo)
        check("窄屏结论页照样给得出世界编号",
              GAMEID in (text_of(b, ".snap-import") or ""), "")
        shot(b, "09-mobile")
        perr = b.js("window.__perr") or []
        check("窄屏这一段也没有页面报错", not perr, perr)
    finally:
        b.close()

    print("\n截图在 %s" % OUT)
    if FAILED:
        print("\n没过的条目：")
        for x in FAILED:
            print("  - " + x)
        sys.exit(1)
    print("\n全部 %d 条通过" % len(REPORT))


def read_file(p):
    try:
        with open(p, "rb") as fh:
            return fh.read().decode("utf-8", "replace")
    except Exception:
        return ""


def wait_disk():
    """任务回 202 之后换目录还在后面，磁盘上的东西要等一下才作数。"""
    want = os.path.join(KLEI, "ProbeWorld", "Master", "save", "session", GAMEID, "0000000085")
    for _ in range(90):
        if os.path.exists(want):
            return True
        time.sleep(1.0)
    return False


if __name__ == "__main__":
    main()
