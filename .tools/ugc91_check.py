# -*- coding: utf-8 -*-
"""#91 验收：工坊内容在引擎自己的下载区里时，界面每一处说法都得对。

云机实测：服务端启动时把工坊模组拉到 <dstInstall>/ugc_mods/<房间>/<世界>/content/322330/<id>，
mods/ 下面不会多出目录，而且每个世界各存一份。面板以前只扫 mods/，
于是日志里明明 "Loading mod: workshop-378160973" 的模组，界面上报「文件还没下载」。

这一支盯四件事，全部按"界面上写的每个字都要能被接口或磁盘反驳"来判：
  1. 整台机器那一屏（第 2 屏）：这一位算已下载，名字版本读得出来，不挂在待下载里，
     并且有一枚说清"文件在引擎的下载区、mods/ 里没有目录"的标记；
  2. 同一个模组在世界之间要分开判：文件只给了地表，洞穴那一行就得说「文件还没下载」，
     说"文件在"就是假话（洞穴重启前它开着也不会生效）；
  3. 删除的确认框不能再承诺"删掉 mods/<目录>"——那删的是不存在的路径，
     真正该说的是引擎下载区里的每一份；
  4. 删完（keepSetup=1）这一位要回到「已订阅、等重启下载」，订阅行和开关都不动，
     这才是"删文件重下"的兑现；随后把夹具还原。

跑法：先起着 python .tools/ui_dev.py（引擎下载区那一份由它铺），再
      python .tools/ugc91_check.py [宽度] [高度]
"""
import base64
import io
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
ID = "900000800"
DIR = "workshop-" + ID
NAME = "Global Positions 位置共享"
DST = os.path.join(ROOT, ".tmp_boot", "dst")
UGC = os.path.join(DST, "ugc_mods", "Cluster_1", "Master", "content", "322330", ID)
SETUP = os.path.join(DST, "mods", "dedicated_server_mods_setup.lua")
MODINFO = (u'name = "%s"\nversion = "1.7.6"\napi_version = 10\nauthor = "夹具作者"\n'
           u'server_only = false\nall_clients_require_mod = true\n'
           u'description = [[\n在地图上标出所有玩家的位置。\n]]\ntags = { "Map Info" }\nsettings = {}\n'
           % NAME)


def req(method, path, body=None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method,
                               headers={"Authorization": AUTH, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode("utf-8") or "{}")


def click_js(b, sel, predicate):
    return b.js("(() => { const el = [...document.querySelectorAll(%s)].find(%s);"
                " if (!el) return false; el.click(); return true; })()"
                % (json.dumps(sel), predicate))


def find_tab(b, word):
    return click_js(b, ".tabs button", "t => t.innerText.indexOf(%s) >= 0" % json.dumps(word))


ROOM = "Cluster_1"


def pick_room(b):
    """把界面切到接口断言用的那一间。

    房间下拉框的默认值是浏览器记住的上一次（夹具里有 ProbeWorld 和 Cluster_1 两间），
    不固定住的话，接口在 Cluster_1 上做的写入和界面在另一间上读到的行就对不上，
    报出来的是一条"开关没生效"的假红。
    """
    got = b.js("(() => { const sels = [...document.querySelectorAll('select')];"
               " for (const s of sels) {"
               "   const o = [...s.options].find(x => x.value === %s || x.innerText.indexOf(%s) >= 0);"
               "   if (!o) continue;"
               "   s.value = o.value; s.dispatchEvent(new Event('change', {bubbles:true}));"
               "   return s.value; }"
               " return 'no-option'; })()" % (json.dumps(ROOM), json.dumps(ROOM)))
    b.wait("((document.querySelector('.card-head .chip') || {}).innerText || '').length > 0", tries=40)
    return got


# 第 2 屏的一张卡：徽标文字 + 徽标的 hover 文案 + 这张卡上的控件文字。
READ_DISK_CARD = r"""(() => {
  const c = [...document.querySelectorAll('.mod-lines .mod-line')]
    .find(x => ((x.querySelector('.mod-line-name') || {}).innerText || '').trim() === %s);
  if (!c) return null;
  return {
    name: ((c.querySelector('.mod-line-name') || {}).innerText || '').trim(),
    badges: [...c.querySelectorAll('.badge')].map(x => ({
      text: x.innerText.trim(), title: x.getAttribute('title') || '' })),
    meta: [...c.querySelectorAll('.mod-meta .chip, .mod-meta .mono')]
      .map(x => x.innerText.replace(/\s+/g, ' ').trim()),
    acts: [...c.querySelectorAll('.mod-line-acts .linkish, .mod-line-acts button')]
      .map(x => x.innerText.trim()),
    used: ((c.querySelector('.mod-line-used') || {}).innerText || '').trim(),
  };
})()""" % json.dumps(NAME)

# 开关那一屏：这一位的行（名字 + 徽标 + 整行说明 + 落在哪一栏）。
READ_SHARD_ROW = r"""(() => {
  const rows = [...document.querySelectorAll('.mod-lines .mod-line')].map(r => ({
    name: ((r.querySelector('.mod-line-name') || {}).innerText || '').trim(),
    on: !!r.querySelector('.switch input:checked'),
    isOn: r.classList.contains('is-on'),
    badges: [...r.querySelectorAll('.badge')].map(x => x.innerText.trim()),
    notes: [...r.querySelectorAll('.shard-note')].map(x => x.innerText.replace(/\s+/g, ' ').trim()),
  }));
  return {
    rows: rows,
    shard: ((document.querySelector('.seg button.on') || {}).innerText || '').trim(),
    overflow: document.documentElement.scrollWidth > window.innerWidth + 1,
  };
})()"""

READ_DIALOG = r"""(() => {
  const d = document.querySelector('.mask .dialog');
  if (!d) return null;
  return {
    title: (d.querySelector('h3') || {}).innerText || '',
    body: (d.querySelector('p') || {}).innerText || '',
    warn: (d.querySelector('.note') || {}).innerText || '',
    btns: [...d.querySelectorAll('.dialog-foot .btn')].map(x => x.innerText.trim()),
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


def clusters_running():
    st, body = req("GET", "/api/v1/clusters")
    out = {}
    for c in body.get("data") or []:
        for s in c.get("shards") or []:
            out["%s / %s" % (c.get("name"), s.get("name"))] = bool(s.get("running"))
    return st, out


def stop_running():
    """删文件这一步对正在跑的世界会直接 409（设计：面板不替用户停服，停服会踢玩家）。
    夹具把两个世界标成在跑，所以真删之前先按界面那条路把它们停下来。"""
    _, state = clusters_running()
    for room, running in state.items():
        if not running:
            continue
        cluster, shard = room.split(" / ", 1)
        req("POST", "/api/v1/clusters/%s/shards/%s/stop" % (cluster, shard))
    for _ in range(40):
        _, state = clusters_running()
        if not any(state.values()):
            return True
        time.sleep(0.25)
    print("FAIL 停完世界还在跑：%s" % state)
    return False


def start_all():
    """还原成"两个世界在跑"，别把夹具留在停掉的状态。"""
    _, state = clusters_running()
    for room in state:
        cluster, shard = room.split(" / ", 1)
        req("POST", "/api/v1/clusters/%s/shards/%s/start" % (cluster, shard))


def row_of(view, key=DIR):
    for m in view.get("mods") or []:
        if m.get("dir") == key:
            return m
    return None


def pending_of(view, key=ID):
    for p in view.get("pendingDownload") or []:
        if p.get("workshopId") == key:
            return p
    return None


def main():
    width = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    height = int(sys.argv[2]) if len(sys.argv) > 2 else 900
    fails = []
    shot = lambda name: os.path.join(HERE, "tmp", "ugc91_%s_%d.png" % (name, width))

    if not os.path.isfile(os.path.join(UGC, "modinfo.lua")):
        print("FAIL 夹具里没有引擎下载区那一份（%s）——先跑 python .tools/ui_dev.py --reseed" % UGC)
        return 1

    # 两个世界都打开它：这正是"订阅并开启"之后的现场（文件却只给了地表）。
    for shard in ("Master", "Caves"):
        st, r = req("PUT", "/api/v1/clusters/Cluster_1/shards/%s/mods/%s" % (shard, DIR),
                    {"enabled": True})
        if st != 200:
            print("FAIL 前置在 %s 打开这一位没成：%s" % (shard, r.get("error")))
            return 1

    # ---- 1. 接口口径 ----
    st, sv = req("GET", "/api/v1/mods")
    view = sv.get("data") or {}
    m = row_of(view)
    if not m:
        fails.append("第 2 屏的数据里根本没有这一位（引擎下载区没被扫到）")
    else:
        if m.get("downloadedByEngine") is not True:
            fails.append("接口没标 downloadededByEngine：%s" % json.dumps(m, ensure_ascii=False)[:160])
        if m.get("name") != NAME or m.get("version") != "1.7.6":
            fails.append("下载区那一份的名字版本读不出来：%s / %s" % (m.get("name"), m.get("version")))
        if m.get("inSetupList") is not True:
            fails.append("订阅清单里有这一行，接口却说没：%s" % m.get("inSetupList"))
    if pending_of(view):
        fails.append("文件已经在这台机器上了，接口还把它列进待下载：%s"
                     % json.dumps(pending_of(view), ensure_ascii=False)[:160])
    for shard, want in (("Master", True), ("Caves", False)):
        st, sv2 = req("GET", "/api/v1/clusters/Cluster_1/shards/%s/mods" % shard)
        e = row_of(sv2.get("data") or {})
        if not e:
            fails.append("%s 的覆盖文件里该有这一条" % shard)
            continue
        if bool(e.get("onDisk")) != want:
            fails.append("%s 的文件在不在判错了，期望 %s 实际 %s" % (shard, want, e.get("onDisk")))
    print("接口：第 2 屏有这一位（引擎下载区=%s）｜待下载 %d 条｜地表 onDisk=%s 洞穴 onDisk=%s"
          % (m and m.get("downloadedByEngine"), len(view.get("pendingDownload") or []),
             (row_of(req("GET", "/api/v1/clusters/Cluster_1/shards/Master/mods")[1].get("data") or {}) or {}).get("onDisk"),
             (row_of(req("GET", "/api/v1/clusters/Cluster_1/shards/Caves/mods")[1].get("data") or {}) or {}).get("onDisk")))

    # ---- 2. 界面 ----
    b = Browser(width, height)
    try:
        b.goto(BASE + "/?cdp=%d/#/mods" % int(time.time() * 1000))
        if not b.wait("!!document.querySelector('.login-form, .tabs')"):
            print("FAIL 模组页没起来")
            return 1
        if not login(b):
            print("FAIL 登录没进去")
            return 1
        if not b.wait("document.querySelectorAll('.mod-cards .mod-card').length > 0", tries=90):
            print("FAIL 第 1 屏一张卡片都没画出来")
            return 1
        picked = pick_room(b)
        print("界面当前房间：%s" % picked)
        if picked != ROOM:
            fails.append("界面切不到 %s（现在是 %s），接口与界面的对照没有基准" % (ROOM, picked))
            return 1
        time.sleep(0.4)

        if not find_tab(b, "文件"):
            fails.append("找不到「这台机器上的文件」那一屏的页签")
            return 1
        if not b.wait("[...document.querySelectorAll('.mod-line-name')]"
                      ".some(x => x.innerText.indexOf(%s) >= 0)" % json.dumps(NAME), tries=60):
            fails.append("第 2 屏没有画出 %s 这张卡（界面把它吞了）" % NAME)
            return 1
        b.wait("!document.querySelectorAll('.sk').length", tries=60)
        time.sleep(0.3)
        card = b.js(READ_DISK_CARD)
        print("第 2 屏这张卡：%s" % json.dumps(card, ensure_ascii=False))
        if not card:
            fails.append("第 2 屏读不到这张卡的 DOM")
            return 1
        texts = [x["text"] for x in card["badges"]]
        if "引擎下载区" not in " ".join(texts):
            fails.append("卡上没有说清文件来自引擎的下载区（徽标是 %s）" % texts)
        here = [x for x in card["badges"] if x["text"] == "引擎下载区"]
        if here and "mods/ 下面没有这个目录" not in here[0]["title"]:
            fails.append("那枚标记的说明没告诉用户 mods/ 里找不到它：%s" % here[0]["title"])
        for x in ("还没下载", "不见了"):
            if any(x in t for t in texts):
                fails.append("文件明明在机器上，卡上还挂着「%s」那种说法：%s" % (x, texts))
        if not any(ID in t for t in card["meta"]):
            fails.append("卡的元信息里没有工坊 ID（用户要靠它对工坊页）：%s" % card["meta"])
        # 把这一张卡滚到眼前再拍：截图要能直接看出那枚标记长什么样、和别的标记挤不挤。
        b.js("(() => { const c = [...document.querySelectorAll('.mod-line')]"
             ".find(x => x.innerText.indexOf(%s) >= 0);"
             " c && c.scrollIntoView({block:'center'}); return !!c; })()" % json.dumps(NAME))
        time.sleep(0.3)
        b.shoot(shot("tab2"))

        # 删除确认框：这一步的说法必须对着真正会被删的地方。
        if not click_js(b, ".mod-line .mod-line-acts .linkish",
                        "x => x.innerText.trim() === '删除' && "
                        "x.closest('.mod-line').innerText.indexOf(%s) >= 0" % json.dumps(NAME)):
            fails.append("这张卡上没有「删除」那枚控件")
        else:
            dlg = None
            for _ in range(30):
                dlg = b.js(READ_DIALOG)
                if dlg:
                    break
                time.sleep(0.2)
            print("删除确认框：%s" % json.dumps(dlg, ensure_ascii=False))
            if not dlg:
                fails.append("点了删除没弹确认框")
            else:
                all_text = dlg["title"] + " / " + dlg["body"] + " / " + dlg["warn"]
                if "mods/" + DIR in all_text:
                    fails.append("确认框还在承诺「删掉 mods/%s」，这台机器上并没有那个目录" % DIR)
                if "引擎下载区" not in dlg["body"]:
                    fails.append("确认框没说清删的是引擎下载区里的那几份：%s" % dlg["body"])
                if "每一份" not in dlg["body"]:
                    fails.append("确认框没说「每个世界各一份」这件事：%s" % dlg["body"])
                # 取消，不真删：真删走接口那一步（下面第 3 段），界面只验说法。
                b.js("(() => { const d = document.querySelector('.mask .dialog');"
                     " const no = [...d.querySelectorAll('.dialog-foot .btn')][0];"
                     " no.click(); return true; })()")
                time.sleep(0.3)

        # 开关那一屏：同一位，两个世界说法要分开。
        if not find_tab(b, "开关"):
            fails.append("找不到「这个房间的开关」那一屏的页签")
        else:
            for shard, want in (("Master", None), ("Caves", "文件还没下载")):
                if not click_js(b, ".seg button", "t => t.innerText.trim() === %s" % json.dumps(shard)):
                    fails.append("切不到 %s 那一间" % shard)
                    continue
                if not b.wait("[...document.querySelectorAll('.mod-line-name')]"
                              ".some(x => x.innerText.indexOf(%s) >= 0)" % json.dumps(NAME), tries=60):
                    fails.append("%s 那一屏没有画出这一位" % shard)
                    continue
                # 骨架屏还在就是这一间的覆盖文件没读完；这时候读行会读到"还没写条目"那一套默认值，
                # 报出"开关没生效"的假红（390 那一跑就是这么红的）。
                if not b.wait("!document.querySelectorAll('.sk').length", tries=60):
                    fails.append("%s 那一屏一直在读（骨架屏没消失）" % shard)
                    continue
                s = b.js(READ_SHARD_ROW)
                row = [x for x in s["rows"] if x["name"] == NAME]
                if not row:
                    fails.append("%s 的行读不到：%s" % (shard, [x["name"] for x in s["rows"]]))
                    continue
                print("%s：%s 开着=%s 落在开着那栏=%s 徽标=%s 说明=%s"
                      % (shard, s["shard"], row[0]["on"], row[0]["isOn"], row[0]["badges"], row[0]["notes"]))
                if not row[0]["on"] or not row[0]["isOn"]:
                    fails.append("%s 这一位该是开着并落在「开着」那一栏" % shard)
                if want and not any(want in x for x in row[0]["badges"]):
                    fails.append("%s 这个世界没有这一份文件，行上该写「%s」：%s" % (shard, want, row[0]["badges"]))
                if any("目录不见了" in x for x in row[0]["badges"]):
                    fails.append("%s 在订阅清单里（重启会拉），却被说成目录不见了：%s" % (shard, row[0]["badges"]))
                if want is None and any(("还没下载" in x or "不见了" in x) for x in row[0]["badges"]):
                    fails.append("%s 的文件就在这一间的下载区里，行上还挂着缺文件的话：%s" % (shard, row[0]["badges"]))
                if s["overflow"]:
                    fails.append("%s 那一屏在这个宽度下横向溢出了" % shard)
                if want:
                    b.shoot(shot("tab3_caves"))

        # ---- 3. 真删一次（keepSetup=1）：删的要真是盘上那一份，界面口径跟着变回待下载 ----
        if not stop_running():
            fails.append("停世界没停成，删除那一步验不动")
        st, dv = req("DELETE", "/api/v1/mods/files/%s?keepSetup=1" % DIR)
        d = dv.get("data") or {}
        if st != 200:
            fails.append("删除没成：%s" % dv.get("error"))
        else:
            print("删除以后那句：%s" % (d.get("hint") or ""))
            if os.path.isdir(UGC):
                fails.append("界面上说删掉了，引擎下载区里那一份还在盘上")
            hint = d.get("hint") or ""
            if "引擎下载区" not in hint or "1 份" not in hint:
                fails.append("删除结果没说清删的是下载区里的几份：%s" % hint)
            if "重新下载" not in hint:
                fails.append("保留订阅的删除要给出下一步（下次启动会重下）：%s" % hint)
            st, sv3 = req("GET", "/api/v1/mods")
            v3 = sv3.get("data") or {}
            if row_of(v3):
                fails.append("文件删掉了，第 2 屏还留着这一位")
            p = pending_of(v3)
            if not p:
                fails.append("删完该回到待下载那一栏")
            elif len(p.get("usedBy") or []) != 2:
                fails.append("待下载那条要说清两个世界的开关都还开着：%s" % p.get("usedBy"))
        # 还原夹具：文件回到地表的下载区，界面口径跟着回正。
        os.makedirs(UGC, exist_ok=True)
        with io.open(os.path.join(UGC, "modinfo.lua"), "w", encoding="utf-8", newline="\n") as fh:
            fh.write(MODINFO)
        setup = io.open(SETUP, encoding="utf-8").read()
        if 'ServerModSetup("%s")' % ID not in setup:
            with io.open(SETUP, "w", encoding="utf-8", newline="\n") as fh:
                fh.write(setup + 'ServerModSetup("%s")\n' % ID)
        b.reload()
        if not b.wait("document.querySelectorAll('.mod-cards .mod-card').length > 0", tries=90):
            fails.append("还原以后页面没重画出来")
        else:
            find_tab(b, "文件")
            b.wait("[...document.querySelectorAll('.mod-line-name')]"
                   ".some(x => x.innerText.indexOf(%s) >= 0)" % json.dumps(NAME), tries=60)
            c2 = b.js(READ_DISK_CARD)
            if not c2 or "引擎下载区" not in " ".join(x["text"] for x in (c2 or {}).get("badges", [])):
                fails.append("还原以后这张卡又挂不回「引擎下载区」那枚标记了")
            st, sv4 = req("GET", "/api/v1/clusters/Cluster_1/shards/Master/mods")
            e4 = row_of(sv4.get("data") or {})
            if not e4 or not e4.get("onDisk"):
                fails.append("还原以后地表这一间还说文件不在")
        start_all()

        print("截图：%s ｜ %s" % (shot("tab2"), shot("tab3_caves")))
    finally:
        b.close()

    if fails:
        print("\n%d 处不对：" % len(fails))
        for x in fails:
            print(" - " + x)
        return 1
    print("\nPASS 引擎下载区里那份工坊内容：第 2 屏认成已下载并说清来路，"
          "两个世界的文件在不在分开判，删除的说法对着真正会删的位置，删完回到待下载")
    return 0


if __name__ == "__main__":
    sys.exit(main())
