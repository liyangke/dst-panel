#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""总览页「按人数分档」的真实浏览器验收。

为什么不用 cloneNode：分档是 Vue 按 players.length 算出来的（views/overview.js 的
playerMode），克隆 DOM 节点不会触发重渲染，验出来的还是旧的富卡网格。这里改的是
本地界面服务器那份助手快照（.tmp_boot/klei/Cluster_1/Master/save/dstpanel_status.json）
——后端每次 /status 都重新读文件，于是走的是「磁盘 → 接口 → store → Vue」完整链路，
跟真实进服的人走同一条路。跑完无论成败都把快照还原，别污染后面的验收脚本。

    python .tools/ui_dev.py            # 另开一个终端先把面板跑起来
    python .tools/player_modes_check.py
"""
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, os.path.join(HERE, ".tools"))
from cdp import BASE, Browser, KILL_LEFTOVERS

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

SNAP = os.path.join(HERE, ".tmp_boot", "klei", "Cluster_1", "Master", "save", "dstpanel_status.json")
OUT = os.path.join(HERE, ".tools", "tmp", "shots")
os.makedirs(OUT, exist_ok=True)
LOG = os.path.join(HERE, ".tmp_boot", "player_modes_check.json")

NAMES = ["阿栗", "小满", "初七", "木木", "阿岩", "小雪", "枝枝", "阿岚",
         "麦子", "阿禾", "冬青", "小满", "青柚", "阿栗"]
COUNTS = (1, 4, 5, 6, 8, 9, 12)
VIEWS = (1440, 768, 390)


def read_snap():
    with open(SNAP, encoding="utf-8") as fh:
        return json.load(fh)


def write_snap(d):
    with open(SNAP, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(d, fh, ensure_ascii=False, indent="  ")


def set_players(orig, n):
    """造 n 个人：奇数位用那份「饥饿/理智采不到」的样本，验 null 不会画成 0%。"""
    d = json.loads(json.dumps(orig))
    a = orig["players"][0]
    b = orig["players"][1] if len(orig["players"]) > 1 else a
    out = []
    for i in range(n):
        p = dict(b if i % 2 == 1 else a)
        p["userid"] = "KU_modes_%03d" % i
        p["name"] = NAMES[i % len(NAMES)]
        out.append(p)
    d["players"] = out
    d["player_count"] = n
    d["max_players"] = max(6, n)
    d["seq"] = int(orig.get("seq") or 0) + n + 1
    d["heartbeat"] = time.time()
    write_snap(d)


def boot(b, theme):
    b.goto(BASE + "/#/")
    if not b.wait("!!document.querySelector('#app')", tries=80):
        raise SystemExit("页面没导航到面板")
    b.js("try { sessionStorage.removeItem('dstpanel.creds') } catch(e){}")
    b.reload()
    if not b.wait("!!document.querySelector('.login-form') || !!document.querySelector('.shell')", tries=80):
        raise SystemExit("登录层没出现")
    time.sleep(0.3)
    b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
    b.reload()
    if not b.wait("!!document.querySelector('.shell')", tries=80):
        raise SystemExit("带凭据重进仍停在登录层")
    b.js("document.documentElement.setAttribute('data-theme', %s)" % json.dumps(theme))
    time.sleep(0.9)


# 等人数真的到界面上：Master 那格的人数（富卡数 / 紧凑行数 / 摘要里的 N）取最大值。
WAIT_N = r"""(() => {
  const sds = [...document.querySelectorAll('.shard-detail')];
  const rows = sds.map(sd => sd.querySelectorAll('.detail-players .stat').length +
                            sd.querySelectorAll('.player-line').length);
  const sums = [...document.querySelectorAll('.roster-summary .grow')]
                 .map(x => parseInt(x.textContent, 10) || 0);
  return Math.max(0, ...rows, ...sums) === %d;
})()"""

SYNC = r"""(() => {
  const b = [...document.querySelectorAll('.host-strip .head-actions .btn')]
              .find(x => x.textContent.includes('立即同步'));
  if (b) { b.click(); return true; }
  return false;
})()"""

READ = r"""(() => {
  const de = document.documentElement;
  const sds = [...document.querySelectorAll('.shard-detail')];
  const score = (sd) => sd.querySelectorAll('.detail-players .stat').length +
                        sd.querySelectorAll('.player-line').length +
                        (sd.querySelector('.roster-summary') ? 1000 : 0);
  const sd = sds.length ? sds.reduce((a, b) => score(b) > score(a) ? b : a) : null;
  const cards = sd ? sd.querySelectorAll('.detail-players .stat') : [];
  const lines = sd ? sd.querySelectorAll('.player-line') : [];
  const sum = sd ? sd.querySelector('.roster-summary') : null;
  const grid = sd ? sd.querySelector('.detail-players') : null;
  const first = lines.length ? lines[0] : null;
  const vitals = first ? first.querySelectorAll('.v-track').length : 0;
  // null 三围只出现在奇数位那半样本里，得数整格而不是第一行。
  const unknown = sd ? sd.querySelectorAll('.v-fill.is-unknown').length : 0;
  return {
    mode: !sd ? 'none' : grid ? 'cards' : lines.length ? 'rows' : 'fold',
    cards: cards.length,
    rows: lines.length,
    hasSummary: !!sum,
    summary: sum ? (sum.querySelector('.grow') || {}).textContent.trim().replace(/\s+/g, ' ') : '',
    expanded: sum ? sum.getAttribute('aria-expanded') : null,
    lineH: first ? Math.round(first.getBoundingClientRect().height) : 0,
    vitals, unknown,
    detailH: sd ? Math.round(sd.getBoundingClientRect().height) : 0,
    scrollH: sd ? sd.scrollHeight : 0, clientH: sd ? sd.clientHeight : 0,
    scrolled: sd ? sd.scrollHeight > sd.clientHeight + 2 : false,
    cols: grid ? getComputedStyle(grid).gridTemplateColumns : '',
    overflow: de.scrollWidth - de.clientWidth,
    vueErrors: [...document.querySelectorAll('.toast')]
      .filter(t => /界面出错了|渲染出错/.test(t.innerText)).map(t => t.innerText.slice(0, 140)),
  };
})()"""


def apply_players(b, orig, n):
    set_players(orig, n)
    for _ in range(7):
        b.js(SYNC)
        if b.wait(WAIT_N % n, tries=10, pause=0.2):
            return True
        # 「立即同步」可能被 state.refreshing 吞掉，或这一档被折叠区挡住：
        # 兜底就是等下一次自动轮询（5 秒）自己把新快照读回来。
        time.sleep(1.1)
    return False


def main():
    if not os.path.isfile(SNAP):
        raise SystemExit("找不到助手快照夹具：%s（先跑 python .tools/ui_dev.py）" % SNAP)
    orig = read_snap()
    rows = []
    fails = []
    # 复用残留的 Chrome 没有 --no-proxy-server，本地地址会走系统代理回 502。
    subprocess.run(["powershell.exe", "-NoProfile", "-Command", KILL_LEFTOVERS], capture_output=True)
    b = Browser(1440, 900)
    try:
        for w in VIEWS:
            b.set_viewport(w, 900 if w > 560 else 844)
            for theme in ("dark", "light"):
                boot(b, theme)
                b.js("location.hash = '#/'")
                if not b.wait("!!document.querySelector('.host-strip')", tries=60):
                    raise SystemExit("总览「这台机器」没渲染")
                time.sleep(0.8)
                for n in COUNTS:
                    if not apply_players(b, orig, n):
                        fails.append((w, theme, n, "人数没同步到界面"))
                        continue
                    time.sleep(0.12)
                    r = b.js(READ)
                    r.update({"w": w, "theme": theme, "n": n})
                    rows.append(r)

                    want = "cards" if n <= 4 else "rows" if n <= 8 else "fold"
                    if r["mode"] != want:
                        fails.append((w, theme, n, "形态 %s，期望 %s" % (r["mode"], want)))
                    if want == "cards" and r["cards"] != n:
                        fails.append((w, theme, n, "富卡 %s 张，期望 %s" % (r["cards"], n)))
                    if want == "rows":
                        if r["rows"] != n:
                            fails.append((w, theme, n, "紧凑行 %s 行，期望 %s" % (r["rows"], n)))
                        if r["vitals"] != 3:
                            fails.append((w, theme, n, "三围细条 %s 段，期望 3" % r["vitals"]))
                        if r["unknown"] < 1:
                            fails.append((w, theme, n, "没验到 null 三围的淡底分支"))
                        # 6 人是这一轮的正题：整格必须一屏读完，不能靠 420px 藏人。
                        if n == 6 and r["scrolled"]:
                            fails.append((w, theme, n, "6 人仍被滚动遮住 scroll %s / client %s"
                                          % (r["scrollH"], r["clientH"])))
                        if n == 6 and r["detailH"] > 330:
                            fails.append((w, theme, n, "6 人整格 %spx，比右列两张图还高" % r["detailH"]))
                    if want == "fold":
                        if not r["hasSummary"] or str(n) not in r["summary"]:
                            fails.append((w, theme, n, "人数摘要不对 %r" % r["summary"]))
                        if r["rows"] or r["expanded"] != "false":
                            fails.append((w, theme, n, "默认没收起：rows %s / aria %s"
                                          % (r["rows"], r["expanded"])))
                        # 点一下展开，名单要走紧凑行；再点一下收回去。
                        b.js("document.querySelector('.roster-summary').click()")
                        time.sleep(0.15)
                        ex = b.js(READ)
                        ex.update({"w": w, "theme": theme, "n": n})
                        if ex["rows"] != n or ex["expanded"] != "true":
                            fails.append((w, theme, n, "展开后 rows %s / aria %s"
                                          % (ex["rows"], ex["expanded"])))
                        if ex["overflow"] > 1:
                            fails.append((w, theme, n, "展开后横向溢出 %s" % ex["overflow"]))
                        rows.append(dict(ex, phase="expanded"))
                        if theme == "dark" and w in (1440, 390) and n == 12:
                            b.shoot(os.path.join(OUT, "modes12_expanded_%d.png" % w))
                        b.js("document.querySelector('.roster-summary').click()")
                        time.sleep(0.1)

                    if r["overflow"] > 1:
                        fails.append((w, theme, n, "横向溢出 %s" % r["overflow"]))
                    if r["vueErrors"]:
                        fails.append((w, theme, n, "Vue 报错 %s" % r["vueErrors"]))

                    if theme == "dark" and n in (1, 6, 9) and w in (1440, 390):
                        b.shoot(os.path.join(OUT, "modes%d_%d.png" % (n, w)))
                print("视口 %d / %s：%d 档人数验完，最近一组 %s"
                      % (w, theme, len(COUNTS), json.dumps(rows[-1], ensure_ascii=False)), flush=True)
    finally:
        # 还原夹具：后面的 overview96 / js_error_sweep 都按「地表 2 人、洞穴 1 人」写断言。
        write_snap(orig)
        b.close()

    with open(LOG, "w", encoding="utf-8") as fh:
        json.dump(rows, fh, ensure_ascii=False, indent=1)

    print("\n=== 结论 ===")
    if fails:
        for f in fails:
            print("FAIL", f)
        raise SystemExit(1)
    print("全部通过：%d 组读数（%d 档人数 × %d 视口 × 深浅主题，含展开/收起）"
          % (len(rows), len(COUNTS), len(VIEWS)))
    for r in rows:
        if r["theme"] == "dark" and r["w"] == 1440 and r.get("phase") != "expanded":
            print("%2d 人：%-5s 卡 %s / 行 %s / 整格 %spx / 滚动 %s / 摘要 %r"
                  % (r["n"], r["mode"], r["cards"], r["rows"], r["detailH"],
                     r["scrolled"], r["summary"]))


if __name__ == "__main__":
    main()
