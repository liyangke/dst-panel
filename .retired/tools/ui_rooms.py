# -*- coding: utf-8 -*-
"""「所有房间」这一页的交互验收：分页、搜索、筛选、异常行，全部靠点来验。

为什么单独写一个脚本：这一页的数据是聚合出来的，界面又是纯前端筛选分页，
静态截图只能证明"它长这样"，证不了"点下一页真的换了十条"。
上一轮模组页的分页就是因为只看了截图而没点，交付时才发现点了没反应。

本机（Windows）只有 2 个房间 4 个分片，一页就装完了，分页那条路径压根走不到。
所以这里临时把夹具里的 Cluster_1 复制成 10 个房间（20 个分片），凑够两页再点，
结束时把副本和 screen 替身状态一起还原——夹具是 api_probe 共用的，不能留脏。

副本的分片一律不带状态快照文件，其中一个额外登记成"进程在跑"：
这样界面上就有了唯一一条"进程在跑但助手不回话"的行，
醒目色、要注意筛选、顶栏仪表盘点进来这三条路径才真的被走过。

用法：python .tools/ui_rooms.py
（要先起着 python .tools/ui_dev.py —— 接口那一段断言的是"有分片在跑"时的形状，
 本机只有 ui_dev 的 screen 替身能提供这个前提，api_probe 在本机必然走 503 分支。）
"""
import base64
import io
import json
import os
import re
import shutil
import sys
import time
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
sys.stderr.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
KLEI = os.path.join(ROOT, ".tmp_boot", "klei")
SRC = os.path.join(KLEI, "Cluster_1")
SCREEN_STATE = os.path.join(ROOT, ".tmp_boot", "uiwork", "fake_screen.json")
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tmp", "shots")

EXTRA = ["Cluster_%d" % n for n in range(90, 100)]
RUN_ONE = "Cluster_90"          # 这一个登记成在跑，用来造异常行
PAGE = 12                       # 和 rooms.js 里的 size 同一个数
TOTAL = 24                      # 夹具原有 2 个房间 + 10 个副本，每个房间 2 个分片

results = []


def check(name, ok, detail=""):
    results.append((bool(ok), name, detail))
    print("%s %s%s" % ("PASS" if ok else "FAIL", name, ("  | " + str(detail)[:220]) if detail else ""))


def seed():
    """复制房间、抹掉副本的状态快照、把其中一个登记成在跑。返回还原用的闭包。"""
    for name in EXTRA:
        leftover = os.path.join(KLEI, name)
        if os.path.isdir(leftover):
            shutil.rmtree(leftover)
        shutil.copytree(SRC, leftover)
        for shard in ("Master", "Caves"):
            st = os.path.join(leftover, shard, "save", "dstpanel_status.json")
            if os.path.isfile(st):
                os.remove(st)
    with io.open(SCREEN_STATE, encoding="utf-8") as fh:
        before = fh.read()
    sessions = json.loads(before)
    sessions.append({"name": "dst-%s-Master" % RUN_ONE, "pid": 90990})
    with io.open(SCREEN_STATE, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(json.dumps(sessions, indent="  "))

    def restore():
        for name in EXTRA:
            p = os.path.join(KLEI, name)
            if os.path.isdir(p):
                shutil.rmtree(p)
        with io.open(SCREEN_STATE, "w", encoding="utf-8", newline="\n") as fh:
            fh.write(before)
    return restore


# 表格当前这页的每一行：房间目录名、分片、状态徽章文字、有没有醒目色。
# 只数看得见的行：手机上表格是 display:none 而不是被撤掉，按 DOM 计数会两边都算上。
ROWS = r"""(() => {
  const vis = (x) => x.offsetParent !== null;
  const tr = [...document.querySelectorAll('table.data tbody tr')].filter(vis);
  return {
    n: tr.length,
    keys: tr.map((x) => {
      const c = x.querySelectorAll('td');
      return (c[0] ? c[0].innerText.trim().replace(/\s+/g, ' ') : '') + '|' +
             (c[1] ? c[1].innerText.trim().replace(/\s+/g, ' ') : '') + '|' +
             (c[2] ? c[2].innerText.trim().replace(/\s+/g, ' ') : '');
    }),
    flagged: tr.filter((x) => x.className.indexOf('flag') >= 0).length,
    pager: (document.querySelector('.pager') || {}).innerText || '',
    pages: [...document.querySelectorAll('.pager .pg-num')].map((x) => x.textContent.trim()),
    on: (document.querySelector('.pager .pg-num.on') || {}).textContent || '',
    seg: [...document.querySelectorAll('.seg button.on')].map((x) => x.textContent.trim()),
    cards: [...document.querySelectorAll('.room-card')].filter(vis).length,
  };
})()"""

def api_rooms():
    """直接问接口，拿这次种下去的"有分片在跑"的形状验 200 分支的不变量。

    为什么放在这里而不是 api_probe.py：本机 Windows 的 PATH 里没有 screen，
    api_probe 里 /api/v1/rooms 必然走 503 分支，那一大段 200 分支的断言在本机
    一次都没跑过。ui_dev 有 screen 替身，只有在这里才真的能碰到"在跑的行"。
    """
    req = urllib.request.Request(BASE + "/api/v1/rooms")
    req.add_header("Authorization", "Basic " + base64.b64encode(b"admin:admin").decode())
    with urllib.request.urlopen(req, timeout=20) as resp:
        d = json.loads(resp.read().decode("utf-8"))["data"]
    items = d["items"]

    check("接口这边也是 %d 个分片" % TOTAL, d["rooms"] == len(items) == TOTAL and d["clusters"] == 12,
          "clusters=%s rooms=%s items=%s" % (d["clusters"], d["rooms"], len(items)))
    running = [i for i in items if i["running"]]
    check("在跑的数量与逐行一致，且每行都带 pid",
          d["running"] == len(running) and all(i.get("pid", 0) > 0 for i in running)
          and all(i.get("pid", 0) == 0 for i in items if not i["running"]),
          "running=%s" % len(running))
    check("只有进程在跑的行才可能被点名",
          d["attention"] == len([i for i in items if i.get("alert")])
          and all(i["running"] for i in items if i.get("alert")),
          "attention=%s 带 alert=%s" % (d["attention"], len([i for i in items if i.get("alert")])))
    bad = [i for i in items if i["cluster"] == RUN_ONE and i["shard"] == "Master"]
    check("跑着但不回话的那一行说的是助手",
          bool(bad) and bad[0].get("alert") == "进程在跑，但助手 MOD 没有回话",
          str(bad[0].get("alert"))[:80] if bad else "没有这一行")
    # 顶部"在线玩家"必须和逐行的 worldReady 讲同一个故事。
    ready = [i for i in items if i["worldReady"]]
    check("在线合计只算世界就绪的行",
          d["online"] == sum(i["online"] for i in ready) and ready,
          "online=%s 就绪行=%s" % (d["online"], len(ready)))
    check("世界就绪的行都在跑", all(i["running"] for i in ready), str([i["cluster"] for i in ready if not i["running"]])[:80])
    # 有码值就必须有中文标签，而且标签不能原样重复码值（那是"没翻译"最常见的样子）。
    pairs = (("gameMode", "gameModeLabel"), ("intention", "intentionLabel"),
             ("season", "seasonLabel"), ("phase", "phaseLabel"), ("terrain", "terrainLabel"))
    naked = []
    for i in items:
        for c, l in pairs:
            if i.get(c) and (not i.get(l) or i[l] == i[c]):
                naked.append("%s/%s=%s" % (i["shard"], c, i[c]))
        if not i.get("accessShort") or not i.get("runningLabel"):
            naked.append("%s 少了短名或状态名" % i["cluster"])
    check("每个码值都配了中文说法", not naked, str(naked)[:200])
    # 洞穴连的主机端口写在洞穴自己的 server.ini 里（master_ip + master_port），
    # 而那个端口号是 cluster.ini 的 [SHARD] master_port——主机的 intent 端口，不是它的 server_port。
    # 夹具里有两种房间：面板建出来的（两份都写了）和手搓的最小集群（都没写）。
    # 没写就不许报——给用户一个 127.0.0.1:0 比留空更糟。
    mismatch, declared, reported = [], 0, 0
    for i in items:
        addr = i.get("masterAddr") or ""
        if addr:
            reported += 1
        if i.get("isMaster"):
            continue
        cdir = os.path.join(KLEI, i["cluster"])
        cluster_ini, shard_ini = os.path.join(cdir, "cluster.ini"), os.path.join(cdir, i["shard"], "server.ini")
        if not (os.path.isfile(cluster_ini) and os.path.isfile(shard_ini)):
            continue
        want_ip = re.search(r'^\s*master_ip\s*=\s*(\S+)', io.open(shard_ini, encoding="utf-8").read(), re.M)
        want_port = re.search(r'^\s*master_port\s*=\s*(\d+)', io.open(shard_ini, encoding="utf-8").read(), re.M)
        if not (want_ip and want_port):
            if addr:
                mismatch.append("%s/%s: 洞穴没写连主机，行里却有 %s" % (i["cluster"], i["shard"], addr))
            continue
        declared += 1
        # 这一栏的用处就是"地表换过端口、洞穴还写着旧的"那种现场，
        # 所以它必须照抄洞穴自己的配置；面板拿 cluster.ini 现拼一个"看起来对"的就把线索抹了。
        want = "%s:%s" % (want_ip.group(1), want_port.group(1))
        if addr != want:
            mismatch.append("%s/%s: 行上 %s，磁盘上配的是 %s" % (i["cluster"], i["shard"], addr, want))
    check("洞穴行的连主机端口对得上磁盘上的配置", not mismatch, str(mismatch)[:220])
    check("配了主机端口的房间都报出来了", declared == reported and declared > 0,
          "配置里写了 %s 个，行上报了 %s 个" % (declared, reported))
    # 模组数：读不出来时两个字段要一起缺席，出现一半就是前端会算错的那种形状。
    half = [i["cluster"] for i in items if (i.get("modsEnabled") is None) != (i.get("modsTotal") is None)]
    check("模组件数要么都有要么都没有", not half, str(half)[:120])


def rows(b):
    return b.js(ROWS)


def click_text(b, sel, text):
    # cdp.click 直接把选择器拼进 JS，所以引号要在这里加（它自己不带）。
    return b.click("'%s'" % sel, "(x) => x.textContent.indexOf(%s) >= 0" % json.dumps(text, ensure_ascii=False))


def main():
    os.makedirs(OUT, exist_ok=True)
    if not os.path.isdir(SRC):
        raise SystemExit("夹具不在了：%s（先跑 python .tools/ui_dev.py）" % SRC)
    restore = seed()
    try:
        b = Browser(width=1440, height=900)
        # goto 在清凭据之前：标签页是共享的，先 removeItem 清的是别的源的存储，
        # 面板那份登录态还在，reload 就进不了登录页（同 ui_shots.py 里写死的那条）。
        b.goto(BASE + "/#/")
        b.js("sessionStorage.removeItem('dstpanel.creds')")
        b.reload()
        if not b.wait("!!document.querySelector('.login-form')"):
            raise SystemExit("登录层没出来")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("带着凭据重进还是停在登录层")
        b.js("location.hash = '#/rooms'")
        if not b.wait("document.querySelectorAll('table.data tbody tr').length > 0"):
            raise SystemExit("房间表格一直没出来：" + str(b.js("document.body.innerText.slice(0,200)")))
        time.sleep(0.6)

        p1 = rows(b)
        check("第一页装满 %d 行" % PAGE, p1["n"] == PAGE, p1["n"])
        check("分页条说得出总数", ("共 %d 个世界" % TOTAL) in p1["pager"], p1["pager"].replace("\n", " "))
        check("页码列出 1 和 2", p1["pages"] == ["1", "2"], p1["pages"])
        check("当前页是 1", str(p1["on"]).strip() == "1", p1["on"])

        # 接口层：以下不变量只有"这台机器上有分片在跑"才测得到，
        # 而本机唯一满足这个前提的就是 ui_dev 的 screen 替身，所以放这里跑。
        api_rooms()

        click_text(b, ".pager .pg-btn", "下一页")
        time.sleep(0.5)
        p2 = rows(b)
        inter = set(x.split("|")[0] for x in p1["keys"]) & set(x.split("|")[0] for x in p2["keys"])
        check("下一页换了内容（第一列没有重叠）", p2["n"] == TOTAL - PAGE and not inter,
              "第2页 %s 行，重叠 %s" % (p2["n"], sorted(inter)[:3]))
        check("页码高亮跟到 2", str(p2["on"]).strip() == "2", p2["on"])

        # 搜索：直接命中副本的目录名
        b.js("location.hash = '#/rooms'")
        time.sleep(0.4)
        b.js("""(() => {
          const el = document.querySelector('input[type=search]');
          const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
          setter.call(el, 'Cluster_91');
          el.dispatchEvent(new Event('input', { bubbles: true }));
        })()""")
        time.sleep(0.5)
        s = rows(b)
        hit = [k for k in s["keys"] if "Cluster_91" not in k]
        check("搜索按目录名过滤，剩下的全是它", s["n"] == 2 and not hit, "%s 行，漏网 %s" % (s["n"], hit[:2]))
        check("搜索后回到第 1 页", str(s["on"]).strip() == "1" and s["pages"] == ["1"], s["pages"])

        # 状态筛选：在跑
        b.js("""(() => { const el = document.querySelector('input[type=search]');
          const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
          setter.call(el, ''); el.dispatchEvent(new Event('input', { bubbles: true })); })()""")
        time.sleep(0.4)
        click_text(b, ".seg button", "在跑")
        time.sleep(0.5)
        run = rows(b)
        bad = [k for k in run["keys"] if "运行中" not in k]
        check("「在跑」只剩运行中的分片", run["n"] == 3 and not bad, "%s 行，混入 %s" % (run["n"], bad[:2]))

        # 异常行：进程在跑但助手不回话的那一条
        click_text(b, ".seg button", "要注意")
        time.sleep(0.5)
        att = rows(b)
        check("「要注意」正好一条（造出来的那条异常）", att["n"] == 1 and att["flagged"] == 1,
              "%s 行 / 醒目 %s" % (att["n"], att["flagged"]))
        check("异常行说的是助手不回话", att["keys"] and "助手" in att["keys"][0], att["keys"][:1])
        b.shoot(os.path.join(OUT, "rooms_attention.png"))

        # 顶栏仪表盘点进来要直接落到这个筛选上
        b.js("location.hash = '#/'")
        time.sleep(0.6)
        clicked = click_text(b, "button.gauge.warn", "助手没回话")
        time.sleep(0.8)
        g = rows(b)
        check("顶栏「助手没回话」点进所有房间并带着筛选",
              clicked and b.js("location.hash").startswith("#/rooms") and g["n"] == 1 and "要注意" in g["seg"],
              "hash=%s 行数=%s 选中段=%s" % (b.js("location.hash"), g["n"], g["seg"]))

        # 手机上换成卡片，同一批数据必须还在
        b.set_viewport(390, 844)
        b.js("location.hash = '#/'")
        time.sleep(0.4)
        b.js("location.hash = '#/rooms'")
        if not b.wait("document.querySelectorAll('.room-card').length > 0"):
            raise SystemExit("手机上卡片没出来")
        time.sleep(0.6)
        m = b.js(ROWS)
        check("手机上走卡片而不是横向滚的表格", m["cards"] == PAGE and m["n"] == 0,
              "卡片 %s 张，表格行 %s" % (m["cards"], m["n"]))
        b.shoot(os.path.join(OUT, "rooms_phone.png"))
        b.close()
    finally:
        restore()

    bad = [r for r in results if not r[0]]
    print("\n合计 %d 条，失败 %d 条" % (len(results), len(bad)))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
