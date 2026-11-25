# 「点房间名取加入命令」的浏览器实测：真 Chrome、真接口、真剪贴板三条路。
#
# 要盯住的是这几件事：
#   - 房间名在总览的房间卡上成了可点的把手，而且点出来的标题是房间名不是目录名
#     （「所有房间」那一页 2026-11-07 整页下掉了，#129，这里就不再多页验一遍同一个把手；
#      总览自 #106 起一次只摊开一间，所以"每一间都够得着"改成切页签验那间的把手）。
#   - 命令是整句能粘进游戏控制台的 c_connect("地址", 端口)，端口来自地表那个 server.ini。
#   - 地址来源与警告当场写明白（本地这台机器必然回环，所以回环那句一定要出现）。
#   - 复制按钮不许"按了没反应"：真写进去了就说已复制，浏览器不让写就说要手动选。
#   - 命令拼不出来的那格（用替身响应造）不能只剩一个空框。
#   - 弹窗在 1440 / 390 两个宽度下都不横向溢出，深浅两套主题都读得清。
import sys, os, time, json, base64, urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import Browser, BASE

SHOTS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".tmp_boot", "shots")
os.makedirs(SHOTS, exist_ok=True)

ROOM = "Cluster_1"
OTHER = "ProbeWorld"

PROBE = r"""
(() => {
  const q = (s) => Array.from(document.querySelectorAll(s));
  const text = (n) => (n ? n.textContent.trim() : '');
  const box = (n) => { const r = n.getBoundingClientRect(); return {x:Math.round(r.x), y:Math.round(r.y), w:Math.round(r.width), h:Math.round(r.height)}; };
  const d = document.querySelector('.connect-dialog');
  if (!d) return {dialog: null, docW: document.documentElement.scrollWidth, winW: window.innerWidth};
  const kvs = q('.connect-dialog .connect-kv dt').map((n) => ({k: text(n), v: text(n.nextElementSibling)}));
  return {
    docW: document.documentElement.scrollWidth,
    winW: window.innerWidth,
    title: text(d.querySelector('h3')),
    lede: text(d.querySelector('.connect-lede')),
    cmd: text(d.querySelector('.cmd')),
    cmdBox: d.querySelector('.cmd') ? box(d.querySelector('.cmd')) : null,
    userSelect: d.querySelector('.cmd') ? getComputedStyle(d.querySelector('.cmd')).userSelect : '',
    copyBtn: text(q('.connect-dialog .cmd-row .btn')[0]),
    note: text(d.querySelector('.note')),
    err: text(d.querySelector('.err-box')),
    kvs,
    okLine: text(d.querySelector('.muted:not(.is-warn):not(.connect-lede)')),
    warnLine: text(d.querySelector('.muted.is-warn')),
    box: box(d),
    masked: !!d.querySelector('.cmd') === false,
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
    return b.wait("!!document.querySelector('.room-name')", tries=40)


def names(b):
    return b.js("""Array.from(document.querySelectorAll('.card-head .room-name')).map(
      (n) => ({text: n.textContent.trim(), hidden: n.querySelector('svg') ?
        getComputedStyle(n.querySelector('svg')).opacity : ''}))""")


def open_for(b, room_title):
    """按卡片标题里那个房间名点进去：夹具里有两个房间，写死顺序会在多一个房间时点错。"""
    return b.js("""(() => {
      const n = Array.from(document.querySelectorAll('.card-head .room-name'))
        .find((x) => x.textContent.trim() === %s);
      if (!n) return false;
      n.click();
      return true;
    })()""" % json.dumps(room_title, ensure_ascii=False))


def close_dialog(b):
    b.js("""(() => { const btn = document.querySelector('.connect-dialog .dialog-foot .btn');
      if (btn) btn.click(); })()""")
    time.sleep(0.3)


def pick_tab(b, room_title):
    """总览一次只摊开一间（#106 起），要另一间的把手得先点它的页签。
    页签那一枚的文字是"房间名 + 在跑几个/共几个"粘在一起的，所以按前缀认。"""
    hit = b.js("""(() => {
      const t = Array.from(document.querySelectorAll('.room-tabs button')).find((x) =>
        x.textContent.trim().indexOf(%s) === 0);
      if (t) t.click();
      return !!t;
    })()""" % json.dumps(room_title, ensure_ascii=False))
    time.sleep(0.5)
    return hit


def api_get(path):
    """读接口当基准，不拿界面上的数字对界面：端口这一格夹具里会变（端口那一条验收会改它）。"""
    auth = "Basic " + base64.b64encode(b"admin:admin").decode()
    req = urllib.request.Request(BASE + path, headers={"Authorization": auth})
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.loads(r.read().decode("utf-8")).get("data")


def main():
    b = Browser(1440, 900)
    if not cold_login(b):
        raise SystemExit("登录后没有出现「这台机器」那一栏")
    time.sleep(0.6)

    overview_names = names(b)
    joined = open_for(b, "本地界面实测房")
    if not joined:
        raise SystemExit("总览页上没找到那个房间名的把手")
    b.wait("!!document.querySelector('.connect-dialog .cmd, .connect-dialog .err-box')", tries=30)
    time.sleep(0.4)
    running = b.js(PROBE)
    b.shoot(os.path.join(SHOTS, "connect-1440-dark.png"))

    # 第一条：不动任何替身，真点一次复制。要么写进剪贴板并说"已复制"，
    # 要么老实承认浏览器不让写——两种都行，唯独不许按了没反应。
    b.click("'.connect-dialog .cmd-row .btn'", "(x) => !!x")
    time.sleep(0.7)
    real = b.js(PROBE)
    # 第二条：换掉 clipboard 实现，验"送进剪贴板的那串字符"确实等于屏幕上这句。
    b.js("""(() => {
      window.__clip = [];
      const fake = { writeText: (t) => { window.__clip.push(t); return Promise.resolve(); } };
      Object.defineProperty(navigator, 'clipboard', {value: fake, configurable: true});
    })()""")
    close_dialog(b)
    open_for(b, "本地界面实测房")
    b.wait("!!document.querySelector('.connect-dialog .cmd')", tries=30)
    b.click("'.connect-dialog .cmd-row .btn'", "(x) => !!x")
    time.sleep(0.7)
    spied = b.js("({clip: window.__clip, probe: null})")
    spy_probe = b.js(PROBE)

    # 第三条：两条路都失败时要老实说"请手动选中"，不能显示已复制。
    b.js("""(() => {
      if (typeof navigator.clipboard === 'undefined') {
        Object.defineProperty(navigator, 'clipboard', {value: undefined, configurable: true, writable: true});
      } else {
        navigator.clipboard.writeText = () => Promise.reject(new Error('浏览器拒绝'));
      }
      document.execCommand = () => false;
    })()""")
    close_dialog(b)
    open_for(b, "本地界面实测房")
    b.wait("!!document.querySelector('.connect-dialog .cmd')", tries=30)
    b.click("'.connect-dialog .cmd-row .btn'", "(x) => !!x")
    time.sleep(0.7)
    failed = b.js(PROBE)

    # 第四条：同一个把手、另一间房、进程没在跑（夹具里 ProbeWorld 就是停着的）。
    # 这一步顺带钉住"每间房都够得着这个把手"：切过去以后卡片标题上那一枚必须在。
    close_dialog(b)
    b.js("location.hash = '#/'")
    time.sleep(0.5)
    switched = pick_tab(b, "探针改名")
    stopped_names = names(b)
    open_for(b, "探针改名")
    b.wait("!!document.querySelector('.connect-dialog .cmd')", tries=30)
    time.sleep(0.4)
    stopped = b.js(PROBE)

    # 第五条：命令拼不出来那一格（端口没写在配置里）。真接口造不出这个房间，
    # 用替身响应把这一格摆出来看版式——分支本身由 Go 用例钉。
    close_dialog(b)
    pick_tab(b, "本地界面实测房")   # 上一节把摊开的那间切走了，替身是给这间造的
    b.js("""(() => {
      const real = window.fetch;
      window.__realFetch = real;
      // api.js 要 headers.get('content-type') 才走 JSON 分支，替身得把这几样都补齐。
      const payload = {ok: true, data: {
        cluster: 'Cluster_1',
        roomName: '本地界面实测房',
        shard: 'Master',
        address: '10.0.0.5',
        port: 0,
        command: '',
        source: '地址取自你打开面板时用的那个',
        note: '这个世界的 server.ini 里没写游戏端口，命令拼不出来。去「开服与世界」把端口填上再来复制。'
      }};
      const reply = {
        ok: true, status: 200,
        headers: { get: (k) => (String(k).toLowerCase() === 'content-type' ? 'application/json; charset=utf-8' : '') },
        json: () => Promise.resolve(payload)
      };
      window.fetch = (u, i) => real(u, i).then((r) => (String(u).indexOf('/connect') < 0 ? r : reply));
    })()""")
    open_for(b, "本地界面实测房")
    b.wait("!!document.querySelector('.connect-dialog .err-box')", tries=30)
    time.sleep(0.4)
    nocmd = b.js(PROBE)
    # 替身必须在这里收掉：下面只剩换页签（hash 不变就不重载），
    # 留着它，手机上那一屏点开的会是"端口没写"那个假分支。
    b.js("window.fetch = window.__realFetch")

    close_dialog(b)
    # 换窄屏时先回总览、点回在跑那间的页签：一次只摊开一间，
    # 停在上一节那间（停着的 ProbeWorld）上拍出来的就不是平时那一屏。
    b.js("location.hash = '#/'")
    time.sleep(0.5)
    pick_tab(b, "本地界面实测房")
    b.set_viewport(390, 844)
    time.sleep(0.7)
    b.js("""(() => { const n = document.querySelector('.card-head .room-name'); if (n) n.click(); })()""")
    b.wait("!!document.querySelector('.connect-dialog')", tries=30)
    time.sleep(0.5)
    mobile = b.js(PROBE)
    mobile_names = b.js("""Array.from(document.querySelectorAll('.card-head .room-name')).map((n) => ({
      text: n.textContent.trim(), iconOpacity: n.querySelector('svg') ? getComputedStyle(n.querySelector('svg')).opacity : ''}))""")
    b.shoot(os.path.join(SHOTS, "connect-390-dark.png"))

    # 浅色主题（切主题只能点顶栏那枚图标）。
    b.set_viewport(1440, 900)
    time.sleep(0.5)
    b.js("""(() => {
      const btns = Array.from(document.querySelectorAll('.topbar .icon-btn'));
      const t = btns.find((n) => (n.title || '').indexOf('切到') === 0);
      if (t) t.click();
    })()""")
    time.sleep(0.6)
    theme = b.js("document.documentElement.dataset.theme")
    b.shoot(os.path.join(SHOTS, "connect-1440-light.png"))
    b.close()

    ok = True

    def check(name, cond, extra=""):
        nonlocal ok
        print(("  OK   " if cond else "  FAIL ") + name + (("  " + str(extra)) if extra else ""))
        if not cond:
            ok = False

    # 弹窗没开出来时 PROBE 只回 {dialog:null, docW, winW}，后面每一条都会 KeyError，
    # 真正的毛病（选择器指着已经下掉的版式）反倒被 traceback 盖住。先当一条断言收掉。
    opened = {tag: ("title" in p) for tag, p in
              (("running", running), ("real", real), ("spy", spy_probe), ("failed", failed),
               ("stopped", stopped), ("nocmd", nocmd), ("mobile", mobile))}
    check("七个动作里弹窗都开出来了（没开＝这一份脚本的选择器过期）", all(opened.values()), opened)
    if not all(opened.values()):
        print("\n有 %d 个分支没弹窗，后面不跑了" % sum(1 for v in opened.values() if not v))
        return 1

    print("== 总览页 · 进程在跑的房间 ==")
    print(json.dumps(overview_names, ensure_ascii=False))
    print(json.dumps({k: running.get(k) for k in ("title", "cmd", "note", "kvs", "copyBtn")}, ensure_ascii=False, indent=1))

    # 总览一次只摊开一间（#106 起），这一屏就只有那一间的把手；"每一间都够得着"
    # 由下面切页签那一条来钉，不靠这里数个数。
    check("摊开那间的卡片标题成了可点的把手", len(overview_names) == 1, overview_names)
    check("把手显示的是房间名而不是目录名",
          any(n["text"] == "本地界面实测房" for n in overview_names), [n["text"] for n in overview_names])
    check("弹窗标题带上房间名", running["title"] == "加入「本地界面实测房」", running["title"])
    check("命令是整句 c_connect", running["cmd"].startswith('c_connect("127.0.0.1", ') and running["cmd"].endswith(")"),
          running["cmd"])
    # 端口不写死数字：夹具里那一格会被端口那条验收改过，写死就成了猜。
    # 拿接口给的地表端口当基准，界面上那句命令和那一格读数都得是它。
    mport = (api_get("/api/v1/clusters/%s/ports" % ROOM) or {}).get("masterPort")
    check("端口取的是地表那个号（接口说 %s）" % mport,
          mport and running["cmd"].endswith('%d)' % mport) and
          any(r["k"] == "游戏端口" and r["v"] == str(mport) for r in running["kvs"]),
          (running["cmd"], running["kvs"]))
    check("地址来源写在弹窗里", any(r["k"] == "地址怎么来的" and r["v"] for r in running["kvs"]), running["kvs"])
    check("回环地址那句警告出现", "回环" in running["note"], running["note"])
    check("告诉你按哪个键开控制台", "打开控制台" in running["lede"], running["lede"])
    check("命令框整段可选（user-select:all）", running["userSelect"] == "all", running["userSelect"])
    check("命令框宽度够放下一句", running["cmdBox"] and running["cmdBox"]["w"] > 200, running["cmdBox"])

    print("\n== 复制的三条路 ==")
    check("真点一次复制后有明确结果",
          ("已复制" in real["copyBtn"] and real["okLine"]) or real["warnLine"],
          (real["copyBtn"], real["warnLine"]))
    check("真点时没有同时说已复制又说要手动",
          not ("已复制" in real["copyBtn"] and real["warnLine"]), (real["copyBtn"], real["warnLine"]))
    check("送进剪贴板的那串 == 屏幕上那句",
          spied["clip"] == [running["cmd"]], spied["clip"])
    check("复制成功后按钮与说明都改口",
          "已复制" in spy_probe["copyBtn"] and "剪贴板" in spy_probe["okLine"],
          (spy_probe["copyBtn"], spy_probe["okLine"]))
    check("两条路都失败时说「双击选中」，不说已复制",
          "双击选中" in failed["warnLine"] and "已复制" not in failed["copyBtn"],
          (failed["copyBtn"], failed["warnLine"]))

    print("\n== 进程没在跑 / 命令拼不出来 ==")
    # 这一条替掉了原来"两个房间的把手都在 DOM 里"那句：一次只摊开一间之后，
    # 另一间的把手是切页签切出来的，切不动就等于用户够不着这个功能。
    check("切到另一间后，那间的卡片标题也是把手",
          switched and any(n["text"] == "探针改名" for n in stopped_names),
          [n["text"] for n in stopped_names])
    check("没开服时给出那句提醒但命令照给", "先开服" in stopped["note"] and stopped["cmd"],
          (stopped["note"], stopped["cmd"]))
    check("命令拼不出来时不留空框而是说原因",
          nocmd["masked"] and nocmd["err"] and "端口" in (nocmd["note"] + nocmd["err"]),
          (nocmd["err"], nocmd["note"]))

    print("\n== 390 ==")
    check("手机上卡片标题也是把手", bool(mobile_names), mobile_names)
    check("手机上复制图标常驻（没悬停也用得到）",
          all(float(n["iconOpacity"] or 0) > 0.3 for n in mobile_names), mobile_names)
    check("手机上点开的是真接口那一屏（替身响应已经收掉）",
          "加入「" in (mobile["title"] or ""), mobile["title"])
    check("手机上弹窗里有命令", mobile["cmd"].startswith('c_connect("'), mobile["cmd"])
    # 同样跟"要求的那一档"比，不跟 window.innerWidth 比：390 那档是 mobile 仿真，
    # 内容放不下时 Chrome 自己把布局视口撑大，winW 会跟着 docW 走，这一条就永真了。
    for tag, reqw, p in (("1440", 1440, running), ("390", 390, mobile)):
        check("%s 无横向溢出" % tag, p["docW"] <= reqw + 1, (p["docW"], reqw))
    check("弹窗宽度没超出视口", running["box"]["x"] >= 0 and
          running["box"]["x"] + running["box"]["w"] <= 1440 + 1,
          (running["box"], 1440))
    check("手机上弹窗没超出视口", mobile["box"]["x"] >= -1 and
          mobile["box"]["x"] + mobile["box"]["w"] <= 390 + 1,
          (mobile["box"], 390))
    check("命令文字没被截断（允许换行）",
          running["cmdBox"]["h"] >= 34, running["cmdBox"])

    print("\n主题切换后：" + str(theme))
    check("点图标确实换了主题", theme in ("light", "dark"), theme)

    print("截图：" + os.path.abspath(SHOTS))
    print("\n" + ("全部通过" if ok else "有失败项"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
