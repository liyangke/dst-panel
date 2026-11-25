#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""冷启动视角：把面板指向三个空目录，看一个刚装好的人第一眼看到什么。

为什么要单独起一份：`ui_dev.py` 造的是"已经跑了一阵"的夹具（两个房间、日志、地图、快照、
模组缓存），那正是验收需要的状态，却也把新手第一条路径全部跳过了——
没有房间、没装本体、没令牌、没日志的时候，每一页摆的是什么，现有脚本一次都没看过。
这一份只读不写：单独一个端口（8965）、单独一套目录，不碰 ui_dev 的库，也不碰云机。
"""
import io
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import Browser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BOOT = os.path.join(ROOT, ".tmp_boot")
COLD = os.path.join(BOOT, "cold")
BIN = os.path.join(ROOT, ".tmp_bin")
EXE = os.path.join(BIN, "dstpanel.exe")
SHOTS = os.path.join(ROOT, ".tools", "tmp", "cold")
CFG = os.path.join(COLD, "panel.yaml")
STATE = os.path.join(COLD, "fake_screen.json")
REPORT = os.path.join(COLD, "pages.json")
PORT = 8965
BASE = "http://127.0.0.1:%d" % PORT
USER, PW = "admin", "admin"
_NAV_N = 0
# 手机那一轮（--mobile）换视口，截图名带 m_ 前缀，两套图并存才看得出是同一轮的。
TAG = ""
VIEW = (1440, 900)

# 页面不再由这里写死 hash，改成按顺序点侧栏那 12 个按钮（见 collect）。
# 之前这里手写过一份 ("/snapshots", "存档快照")，而真实路由是 /snaps：
# 于是取样脚本自己撞进 404 页，还把它当成一屏内容记进报告里。手抄的路由表一定会和
# nav.js 漂移，而漂移的样子恰好是"页面看着像坏了"，最难分辨。
NAV_ITEMS = 12

# 出现这些字样就说明这一页没正常渲染，整轮要判红。
RED_MARKS = ["这个地址没有对应的页面", "界面出错了"]


def p(path):
    return path.replace("\\", "/")


def stop_stale():
    """清掉上一轮遗留、还占着 8965 的面板进程。

    为什么会有遗留：这一份脚本用 --serve-only 挂着看过界面，Ctrl+C 只杀了 python 自己，
    它 Popen 出来的面板成了孤儿，还开着 panel.log。下一个动作就想删 .tmp_boot/cold，
    Windows 上是"另一个程序正在使用此文件"一条 PermissionError，看不出跟端口有关系。
    只认名字是 dstpanel 的 PID：这一台机器上还有 8963/8964 两套夹具面板在跑，不能碰。
    """
    out = subprocess.run(["netstat", "-ano"], capture_output=True, text=True,
                         errors="replace").stdout
    for pid in sorted({ln.split()[-1] for ln in out.splitlines()
                       if (":%d " % PORT) in ln and "LISTENING" in ln}):
        info = subprocess.run(["tasklist", "/FI", "PID eq %s" % pid],
                              capture_output=True, text=True, errors="replace").stdout
        if "dstpanel" not in info:
            continue
        subprocess.run(["taskkill", "/PID", pid, "/F"], capture_output=True)
        print("  ·  上一轮遗留的冷启动面板（PID %s）还占着 %d，已经杀掉" % (pid, PORT))
        time.sleep(0.8)


def fresh_dirs():
    """三个空目录：没装本体、没有房间目录、没有库。"""
    if os.path.isdir(COLD):
        shutil.rmtree(COLD)
    for d in ("klei", "dst", "work"):
        os.makedirs(os.path.join(COLD, d))
    # screen 替身在场，但一个会话都没有——"这台机器上什么都没在跑"要能问得出答案，
    # 而不是 503"问不到进程状态"。
    with io.open(STATE, "w", encoding="utf-8", newline="\n") as fh:
        fh.write("[]\n")


def write_cfg():
    text = """# 冷启动实测用配置：只在本机 127.0.0.1 上起，指向三个空目录。
server:
  bind: 127.0.0.1
  port: %d
  adminUser: %s
  adminPassword: "%s"
paths:
  kleiRoot: "%s"
  dstInstall: "%s"
  steamCmd: "%s"
  workDir: "%s"
network:
  shardPortBase: 10998
  shardPortEnd: 11900
  masterIntentPortBase: 11000
backup:
  keep: 3
helper:
  pollMs: 1000
  staleSeconds: 86400
steamApi:
  webApiKey: ""
  proxy: ""
""" % (PORT, USER, PW, p(os.path.join(COLD, "klei")), p(os.path.join(COLD, "dst")),
       p(os.path.join(BIN, "fake_steamcmd.exe")), p(os.path.join(COLD, "work")))
    with io.open(CFG, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(text)
    os.chmod(CFG, 0o600)


def healthz():
    try:
        with urllib.request.urlopen(BASE + "/healthz", timeout=3) as r:
            return r.status == 200
    except Exception:
        return False


def start():
    env = dict(os.environ)
    env["PATH"] = BIN + os.pathsep + env.get("PATH", "")
    env["DST_FAKE_SCREEN"] = STATE
    log = io.open(os.path.join(COLD, "panel.log"), "w", encoding="utf-8")
    proc = subprocess.Popen([EXE, "-config", CFG], cwd=ROOT, stdout=log,
                            stderr=subprocess.STDOUT, env=env)
    for _ in range(60):
        if proc.poll() is not None:
            raise SystemExit("冷启动面板自己退了，看 %s" % os.path.join(COLD, "panel.log"))
        if healthz():
            return proc, log
        time.sleep(0.25)
    proc.terminate()
    raise SystemExit("冷启动面板 15 秒没起来")


BOOT_ERRORS = r"""
(() => {
  if (window.__bootErrs) return 'again';
  window.__bootErrs = [];
  const ce = console.error;
  console.error = function () { window.__bootErrs.push(Array.from(arguments).join(' ')); ce.apply(console, arguments); };
  window.addEventListener('error', (e) => window.__bootErrs.push(String(e.message) + ' @' + (e.filename || '') + ':' + e.lineno));
  window.addEventListener('unhandledrejection', (e) => window.__bootErrs.push('promise: ' + String(e.reason)));
  return 'armed';
})()
"""

PAGE = r"""(() => {
  const VW = __VW__;
  const t = (n) => ((n || {}).textContent || '').trim().replace(/\s+/g, ' ');
  const empties = [...document.querySelectorAll('.empty')].map((e) => ({
    title: t(e.querySelector('.e-title')),
    note: t(e.querySelector('.e-note')),
    btns: [...e.querySelectorAll('button, a')].map((x) => t(x)).filter(Boolean),
  }));
  return {
    hash: location.hash,
    h1: t(document.querySelector('.card h1, .card h2, h2')),
    cards: document.querySelectorAll('.card').length,
    empties: empties,
    banners: [...document.querySelectorAll('.banner, .x-note')].map((x) => t(x)).filter(Boolean),
    btns: [...document.querySelectorAll('button')].map(t).filter(Boolean).slice(0, 14),
    rows: document.querySelectorAll('tbody tr, .list-row, .shard-row').length,
    text: document.body.innerText.replace(/\s+/g, ' ').slice(0, 300),
    // 横向溢出：这一套界面的约定是窄屏只换列、不许左右拖。
    // 宽度基准用的是"脚本要求的那一档"（VW），不是 window.innerWidth —— 移动模拟下
    // 内容一溢出，Chrome 会把布局视口一起撑宽（实测：撑一个 900px 的块以后
    // innerWidth 直接变成 912），拿 innerWidth 比就等于永远量不到。
    // 越界的元素只报"没有越界孩子"的那一层，报出来的是真正撑宽的东西，不是它的祖先。
    overflow: (() => {
      const doc = document.documentElement;
      if (doc.scrollWidth <= VW + 1) return null;
      const over = (e) => e.getBoundingClientRect().right > VW + 1;
      const bad = [...document.querySelectorAll('body *')].filter(over)
        .filter((e) => ![...e.children].some(over)).slice(0, 5)
        .map((e) => ((typeof e.className === 'string' ? e.className : '') || e.tagName)
                    + ' → ' + Math.round(e.getBoundingClientRect().right) + 'px');
      return { sw: doc.scrollWidth, vw: VW, bad: bad };
    })(),
  };
})()"""


def page_js():
    """把 PAGE 里的宽度基准换成这一轮实际要求的那一档。

    用 replace 不用 % 格式化：PAGE 是一段 JS，里面将来出现任何一个百分号（比如算个百分比）
    就会让格式化器报"unsupported format character"，而那和探针本身没关系，排查很费时间。
    """
    return PAGE.replace("__VW__", str(VIEW[0]))


# 这一页自己的页签条（开服页、模组页有；别的页没有就跳过）。
# 只取 main 里第一条，那是页面级页签；页签内部再套的控件条不算。
TABS_JS = r"""(() => {
  const strip = document.querySelector('main .tabs');
  if (!strip) return [];
  return Array.from(strip.querySelectorAll('button'))
    .map((x) => (x.textContent || '').trim().replace(/\s+/g, ' '));
})()"""

# 读完就把攒着的报错清掉：换页签不产生新文档，不清的话第二格报的是第一格的遗留。
DRAIN_ERRS = r"""(() => { const e = (window.__bootErrs||[]).slice(0,3); window.__bootErrs = []; return e; })()"""


def reds_of(info):
    """一屏取样算不出"这页有问题"的两种证据：命中红线词、横向溢出。"""
    reds = [m for m in RED_MARKS if m in (info.get("text") or "")]
    ov = info.get("overflow")
    if ov:
        reds.append("横向溢出到 %dpx（视口 %d）：%s" % (ov["sw"], ov["vw"], "、".join(ov["bad"])))
    return reds


def sweep(b, page_i, slug):
    """把这一页的页签逐个点一遍，每个各记一份取样。

    为什么默认那一栏不够：冷启动里"这一页没问题"和"这一页默认那一栏没问题"是两件事。
    开服页那几个页签停在同一句空状态上，而上一轮的错恰好藏在第二个页签里
    （「远程指令」那栏让人去点一个根本没渲染出来的房间下拉），
    只截默认页签的话它永远不会出现在报告里。
    """
    labels = b.js(TABS_JS) or []
    if len(labels) < 2:
        return []
    out = []
    for j, label in enumerate(labels):
        sel = "document.querySelectorAll('main .tabs button')[%d]" % j
        b.js(sel + ".click()")
        on = b.wait(sel + ".classList.contains('on')")
        time.sleep(1.2)
        try:
            info = b.js(page_js())
        except Exception as e:
            info = {"error": str(e)}
        info["label"] = label
        info["errs"] = b.js(DRAIN_ERRS) or []
        info["reds"] = reds_of(info)
        if not on:
            info["reds"].append("点了第 %d 个页签（%s）以后没有变成选中态" % (j + 1, label))
        png = os.path.join(SHOTS, "cold_%s%02d_%s_t%d.png" % (TAG, page_i + 1, slug, j + 1))
        try:
            b.shoot(png)
            info["shot"] = png
        except Exception as e:
            info["shotError"] = str(e)
        out.append(info)
    return out


def api_check():
    """起完先问一遍接口，把"页面为什么转圈"变成一句能读的话。
    screen 替身没配好时 /clusters 是 503，界面上只表现为一直"正在读取房间与世界状态……"——
    那种截图看不出原因，所以取样前先落一份接口现场。
    """
    import base64
    res = {}
    auth = "Basic " + base64.b64encode(("%s:%s" % (USER, PW)).encode()).decode()
    for path in ("/healthz", "/api/v1/session", "/api/v1/host", "/api/v1/clusters",
                 "/api/v1/rooms", "/api/v1/tasks"):
        req = urllib.request.Request(BASE + path, headers={"Authorization": auth})
        try:
            with urllib.request.urlopen(req, timeout=12) as r:
                body = r.read().decode("utf-8", "replace")
                res[path] = "%d %s" % (r.status, body[:110])
        except urllib.error.HTTPError as e:
            res[path] = "%d %s" % (e.code, e.read().decode("utf-8", "replace")[:110])
        except Exception as e:
            res[path] = "连接失败 %s" % e
    for k, v in res.items():
        print("  接口 %-22s %s" % (k, v.replace("\n", " ")))
    return res


def sample():
    b = Browser(width=VIEW[0], height=VIEW[1])
    # 每个新文档一加载就装上报错钩子：冷启动里"界面没出来"这种失败，原因几乎全在
    # boot 期的控制台里（一次 mount 异常就能让启动画面永远停在那一句），
    # 不抓的话只剩"登录层没出来"四个字，和实际原因之间隔着一整轮猜测。
    hook = b.ws.call("Page.addScriptToEvaluateOnNewDocument", {"source": BOOT_ERRORS}).get("identifier")
    # 这里不用 Page.reload：Chrome 刚拉起来时那一格是 about:blank，从它跨到 http 源以后
    # reload 会报 "Not attached to an active page"（实测撞上过一次）。改成先 goto 一次
    # 拿到面板源，写凭据，再带一个查询参数 goto 一次做整页刷新——路由只认 hash，参数不影响页面。
    try:
        return collect(b)
    finally:
        if hook:
            try:
                b.ws.call("Page.removeScriptToEvaluateOnNewDocument", {"identifier": hook})
            except Exception:
                pass


def note(b):
    """失败时把"界面到底走到哪一步了"一次报全。

    光有"登录层没出来"这句话的话，冷启动的失败至少有三种完全不同的原因（Vue 没加载、
    模块里抛异常、接口没通），而它们在截图上长得一模一样。这里一次性把
    window.Vue 在不在、#app 里是什么、资源状态码，和 boot 期报错一起打出来。
    """
    probe = r"""(() => {
      const app = document.getElementById('app');
      const res = performance.getEntriesByType('resource').filter((e) => e.responseStatus >= 400
        || e.responseStatus === 0)
        .map((e) => e.name.replace(location.origin, '') + ' ' + e.responseStatus);
      return {
        vue: typeof window.Vue,
        ready: document.readyState,
        appHead: app ? app.innerHTML.slice(0, 120) : null,
        boot: !!document.querySelector('.boot'),
        badRes: res.slice(0, 6),
        errs: (window.__bootErrs || []).slice(0, 6),
      };
    })()"""
    try:
        return json.dumps(b.js(probe), ensure_ascii=False)
    except Exception as e:
        return "连现场都没取到：%s" % e


def nav(b, path):
    """整页加载一次冷启动面板，并且保证这次是真的重新加载。

    为什么带一个每次都变的查询参数：这台机器上的无头 Chrome 是多个脚本共用的
    （cdp.py 会复用已经开着的那个），标签页经常正好停在上一轮用过的那个地址上。
    Page.navigate 到一个"连 fragment 都一样"的地址时，Chrome 只做同文档导航，
    整份文档不会重新加载——于是这一轮读到的是上一轮那张旧页面，接口现场却是新的，
    看上去就像"界面卡在启动骨架上起不来"。
    之前两次"登录层没出来"在改成每次都换参数（并重建 exe）以后没有再现；
    但那时能留下的现场只有那句屏上文字，所以原因我并没有钉死，这一条只当防呆。
    """
    global _NAV_N
    _NAV_N += 1
    b.goto("%s/?cold=%d#%s" % (BASE, _NAV_N, path))


def collect(b):
    out = []
    # 先把这一格标签页可能留下的登录态清掉：面板源固定（127.0.0.1:8965），
    # 而标签页是跨脚本复用的，上一轮写进去的凭据这一轮还在，
    # 那样第一屏直接进外壳，"新手第一眼看到登录页"这一条就永远测不到。
    nav(b, "/")
    b.wait("document.readyState === 'complete'")
    b.js("sessionStorage.clear()")
    nav(b, "/")
    if not b.wait("!!document.querySelector('.login-form')"):
        raise SystemExit("冷启动面板的登录层没出来 ｜屏上=%s ｜现场=%s ｜接口现场=%s"
                         % (str(b.js("document.body.innerText.slice(0,200)")), note(b), api_check()))
    api_check()
    b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:%s,pass:%s}))"
         % (json.dumps(USER), json.dumps(PW)))
    nav(b, "/")
    if not b.wait("!!document.querySelector('.shell')"):
        raise SystemExit("冷启动面板登录后界面没出来 ｜屏上=%s ｜现场=%s"
                         % (str(b.js("document.body.innerText.slice(0,200)")), note(b)))
    labels = b.js("Array.from(document.querySelectorAll('.nav-item')).map((n) => n.textContent.replace(/\\s+/g, ' ').trim())") or []
    if len(labels) != NAV_ITEMS:
        raise SystemExit("侧栏只有 %d 个入口，应该是 %d 个：%s" % (len(labels), NAV_ITEMS, labels))
    for i, label in enumerate(labels):
        # 点侧栏而不是改 location.hash：新手走的就是这条路径，而且点了以后路由自己决定
        # 落在哪个地址，这一份脚本就没机会和 nav.js 抄错名字（那种错只会以"某页是 404"
        # 的形式出现，最难看出来）。
        b.js("document.querySelectorAll('.nav-item')[%d].click()" % i)
        if not b.wait("document.querySelectorAll('.nav-item')[%d].classList.contains('on')" % i):
            raise SystemExit("点了侧栏第 %d 项（%s）以后没有变成选中态" % (i + 1, label))
        time.sleep(1.1)
        try:
            info = b.js(page_js())
        except Exception as e:
            info = {"error": str(e)}
        info["label"] = label
        # 换页不产生新文档，报错会一直攒着；每页读完就清空，这样报告里那一格
        # 报的是"这一页自己冒出来的错"，而不是上一页遗留的。
        info["errs"] = b.js(DRAIN_ERRS) or []
        slug = re.sub(r"[^0-9a-zA-Z]+", "-", str(info.get("hash", ""))).strip("-") or "overview"
        info["reds"] = reds_of(info)
        png = os.path.join(SHOTS, "cold_%s%02d_%s.png" % (TAG, i + 1, slug))
        try:
            b.shoot(png)
            info["shot"] = png
        except Exception as e:
            info["shotError"] = str(e)
        # 页签要在这张默认截图之后才点：先留下"新手进来第一眼"那一格，再逐个翻。
        info["tabs"] = sweep(b, i, slug)
        out.append(info)
    # 登录页本身（还没凭据时看到的那一屏）也算新手入口的一部分。
    b.js("sessionStorage.removeItem('dstpanel.creds')")
    nav(b, "/")
    b.wait("!!document.querySelector('.login-form')")
    b.shoot(os.path.join(SHOTS, "cold_%slogin.png" % TAG))
    return out


def write_report(out):
    with io.open(REPORT, "w", encoding="utf-8") as fh:
        fh.write(json.dumps(out, ensure_ascii=False, indent=1))

    def say(info, indent="  "):
        empt = "、".join(e["title"] for e in (info.get("empties") or []) if e["title"])
        print("%s%-6s %-9s 卡片 %d 空状态 %s%s" % (
            indent, str(info.get("hash", ""))[:16] or "-", info["label"], info.get("cards", 0),
            str(len(info.get("empties") or [])), ("：" + empt[:34]) if empt else ""))
        for r in info.get("reds") or []:
            print("%s   红：%s" % (indent, r))
        for e in info.get("errs") or []:
            print("%s   报错：%s" % (indent, e))

    for info in out:
        say(info)
        for t in info.get("tabs") or []:
            say(t, "     页签 ")
    print("冷启动 %d 页取样完成：%s" % (len(out), REPORT))
    print("截图：%s" % SHOTS)
    reds = ["%s（%s）" % (i["label"], r) for i in out for r in (i.get("reds") or [])]
    reds += ["%s 有报错：%s" % (i["label"], e) for i in out for e in (i.get("errs") or [])]
    reds += ["%s/%s（%s）" % (i["label"], t["label"], r)
             for i in out for t in (i.get("tabs") or []) for r in (t.get("reds") or [])]
    reds += ["%s/%s 有报错：%s" % (i["label"], t["label"], e)
             for i in out for t in (i.get("tabs") or []) for e in (t.get("errs") or [])]
    if reds:
        print("===== 冷启动里有页面不正常：\n- " + "\n- ".join(reds))
        return 1
    return 0


def build_exe():
    """每次跑之前重建二进制。

    为什么必须写死在这里：前端那套 js/css 是 go:embed 进二进制的，磁盘上改了、
    exe 里还是旧的。上一轮我改完日志页那条红框就跑了这一份脚本，报告全绿、截图里
    红框还在——差的就是这一步。脚本自己不说"用的是哪一版"，这种绿和这种红都不可信。
    """
    import ui_dev
    ui_dev.build()


def main():
    args = sys.argv[1:]
    global TAG, VIEW, REPORT
    if "--mobile" in args:
        # 390 是这一套界面承诺的最窄一档（手机上不许横向滚动），冷启动也要在这一档走一遍。
        TAG, VIEW = "m_", (390, 844)
        REPORT = os.path.join(BOOT, "cold", "pages_mobile.json")
    # --attach 是"接着看已经在跑的那一版"，这时候重建 exe 反而自相矛盾。
    if "--no-build" not in args and "--attach" not in args:
        build_exe()
    if not os.path.exists(EXE):
        raise SystemExit("没有 %s，先跑一次 python .tools/ui_dev.py" % EXE)
    os.makedirs(SHOTS, exist_ok=True)
    # 截图按页面序号命名，改名或换页序以后旧文件会一直留在目录里；
    # 对着两张同名不同代的图判断"这页修好了没有"是很容易看错的，所以每次跑先清掉。
    # 只清自己这一档（桌面/手机）的：两轮都要留着才比得出同一页在两种宽度下各是什么样。
    for f in os.listdir(SHOTS):
        if not f.startswith("cold_" + TAG) or not f.endswith(".png"):
            continue
        # 桌面那一档的 TAG 是空串，"cold_" 会把手机档的 cold_m_* 一起匹配上，
        # 于是跑一次桌面就把手机那套删干净了——而这两套要并存才比得出
        # 同一页在两种宽度下各是什么样。
        if not TAG and f.startswith("cold_m_"):
            continue
        os.remove(os.path.join(SHOTS, f))
    if "--attach" in args:
        return write_report(sample())
    if "--serve-only" in args:
        stop_stale()
        proc, log = start()
        api_check()
        print("冷启动面板挂在 %s 上（Ctrl+C 或 taskkill 停掉）" % BASE)
        try:
            proc.wait()
        except KeyboardInterrupt:
            proc.terminate()
        log.close()
        return 0
    stop_stale()
    fresh_dirs()
    write_cfg()
    proc, log = start()
    try:
        out = sample()
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=8)
        except Exception:
            proc.kill()
        log.close()
    return write_report(out)


if __name__ == "__main__":
    sys.exit(main())
