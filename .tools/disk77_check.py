# #77 + #78 验收：这一屏能不能"删掉一个模组"，以及待下载那几条还是不是一串裸 ID。
#
# 盯的是五件事：
#   1. 一屏分成「已下载」/「已订阅、等重启下载」/「上传本地模组包」三张卡；
#   2. 待下载那一条显示的是模组名字，没见过的那条老实说"还没读到这个名字"，绝不拿 ID 当名字；
#   3. 「开着它的世界」这一栏说的是各世界 modoverrides.lua 里真读到的，不是猜的；
#   4. 删除一次做三件事（文件 + 订阅 + 开关），确认框当场把这几件说完，点完磁盘/清单/开关真的都变了；
#   5. 「删文件重下」（工坊版本对不上那条）只删文件，订阅和开关一个都不动。
#
# 造数据走接口 + 夹具目录 + 开发库里的这一行缓存（本机出不去 Steam，在线那一路只能这样等价），
# 不动仓库里的东西；每一步都从接口回读，不看界面自己声称的数字。
import base64
import io
import json
import os
import sqlite3
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from cdp import BASE, Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")
sys.stderr.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(HERE)
MODS = os.path.join(ROOT, ".tmp_boot", "dst", "mods")
SETUP = os.path.join(MODS, "dedicated_server_mods_setup.lua")
DB = os.path.join(ROOT, ".tmp_boot", "uiwork", "dstpanel.db")
AUTH = "Basic " + base64.b64encode(b"admin:admin").decode()


def req(method, path, body=None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method,
                               headers={"Authorization": AUTH, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(r, timeout=20) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode("utf-8") or "{}")


def setup_text():
    return io.open(SETUP, encoding="utf-8").read() if os.path.exists(SETUP) else ""


def master_enabled(dir):
    """地表这一间现在是不是开着这个目录（走接口读，不看 modoverrides.lua 的原文：
    面板关它是把 enabled 写成 false、条目还留着，按原文找字符串会误判成"没关掉"）。"""
    st, body = req("GET", "/api/v1/clusters/Cluster_1/shards/Master/mods")
    for o in (body.get("data") or {}).get("mods") or []:
        if o.get("dir") == dir:
            return bool(o.get("enabled"))
    return False


def write_modinfo(wid, name, version):
    d = os.path.join(MODS, "workshop-" + wid)
    if not os.path.isdir(d):
        os.makedirs(d)
    io.open(os.path.join(d, "modinfo.lua"), "w", encoding="utf-8", newline="\n").write(
        'name = "%s"\nversion = "%s"\napi_version = 10\nauthor = "夹具"\n'
        'server_only = true\ndescription = [[\n验收用的假模组\n]]\ntags = { "QoL" }\nsettings = {}\n'
        % (name, version))


def seed_workshop_cache(wid, name, workshop_version):
    """种一条"面板在线检索时见过它"的缓存，工坊版本写 workshop_version。

    为什么走数据库而不是接口：这一列只有在线来源会填，而这台 Windows 出不去
    api.steampowered.com（在线那一路由 .tools/cloud_search_probe.py 在云机上验）。
    这一行就是"上次问工坊时它写 9.9"这件事的等价品，种完立刻把磁盘刷成 0.0.1，
    「工坊版本对不上」的徽标和「删文件重下」才是真的能被点到，而不是夹具自说自话。
    """
    if not os.path.exists(DB):
        raise SystemExit("找不到开发库 %s（ui_dev 起来过吗）" % DB)
    now_ms = int(time.time() * 1000)
    con = sqlite3.connect(DB, timeout=15)
    try:
        con.execute("""
            INSERT INTO mods_seen (workshop_id, name, description, version, workshop_version,
              tags, subscriptions, mod_updated_at, file_size, preview_url, has_children,
              client_required, server_only, origin, first_seen_at, last_seen_at,
              favorited, vote_score, votes_total)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
            ON CONFLICT(workshop_id) DO UPDATE SET
              name = excluded.name, version = excluded.version,
              workshop_version = excluded.workshop_version, origin = 'workshop',
              last_seen_at = excluded.last_seen_at""",
                    (wid, name, "在线检索见过的那一条，用来验工坊更新提示与删文件重下。",
                     workshop_version, workshop_version, '["QoL"]',
                     12345, int(time.time()) - 86400, 300_000, "",
                     0, 0, 0, "workshop", now_ms, now_ms, 777, 0.93, 120))
        con.commit()
    finally:
        con.close()


READ_ROWS = r"""(() => {
  const heads = [...document.querySelectorAll('.card-head h3')].map(h => h.innerText.trim());
  const rows = [...document.querySelectorAll('.mod-line')].map(r => ({
    name: (r.querySelector('.mod-line-name') || {}).innerText || '',
    badges: [...r.querySelectorAll('.badge')].map(b => b.innerText.trim()),
    used: (r.querySelector('.mod-line-used') || {}).innerText || '',
    acts: [...r.querySelectorAll('.mod-line-acts .linkish')].map(a => a.innerText.trim()),
    hrefs: [...r.querySelectorAll('a.linkish')].map(a => a.getAttribute('href') || ''),
    chips: [...r.querySelectorAll('.chip')].map(c => c.innerText.trim()),
    cover: !!r.querySelector('.mod-cover img'),
    coverPh: (r.querySelector('.mod-cover-ph') || {}).innerText || '',
    dir: [...r.querySelectorAll('.chip.mono')].map(c => c.innerText.trim()).join(' '),
  }));
  const cards = [...document.querySelectorAll('.mod-card')].map(c => ({
    name: (c.querySelector('.mod-card-name') || {}).innerText || '',
    meta: [...c.querySelectorAll('.mod-card-meta')].map(m => m.innerText.trim()).join(' | '),
    note: (c.querySelector('.mod-card-note') || {}).innerText || '',
    foot: [...c.querySelectorAll('.mod-card-foot .btn')].map(b => b.innerText.trim()),
  }));
  return {
    heads: heads,
    rows: rows,
    cards: cards,
    notes: [...document.querySelectorAll('.note')].map(n => n.innerText.replace(/\s+/g, ' ').trim()),
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
    btns: [...d.querySelectorAll('.dialog-foot .btn')].map(b => b.innerText.trim()),
  };
})()"""


def accept(b):
    """点确认框里的确定按钮（不是取消、不是遮罩）。"""
    return b.js("(() => { const d = document.querySelector('.mask .dialog');"
                " if (!d) return false;"
                " const ok = [...d.querySelectorAll('.dialog-foot .btn')].pop();"
                " if (!ok) return false; ok.click(); return true; })()")


def click_js(b, sel, predicate):
    """按选择器 + 条件点一个元素。cdp.click 把选择器原样拼进 querySelectorAll，
    这里替它加好引号，中文选择器不会被拆成两截。"""
    return b.js("(() => { const el = [...document.querySelectorAll(%s)].find(%s);"
                " if (!el) return false; el.click(); return true; })()"
                % (json.dumps(sel), predicate))


def wait_row(b, name, gone, tries=60):
    expr = ("![...document.querySelectorAll('.mod-line')].some(r => "
            "((r.querySelector('.mod-line-name') || {}).innerText || '').indexOf(%s) >= 0)" % json.dumps(name)
            if gone else
            "[...document.querySelectorAll('.mod-line')].some(r => "
            "((r.querySelector('.mod-line-name') || {}).innerText || '').indexOf(%s) >= 0)" % json.dumps(name))
    return b.wait(expr, tries=tries, pause=0.25)


def clusters_running():
    st, body = req("GET", "/api/v1/clusters")
    out = {}
    for c in body.get("data") or []:
        for s in c.get("shards") or []:
            out["%s / %s" % (c.get("name"), s.get("name"))] = bool(s.get("running"))
    return st, out


def stop_running():
    """把夹具里标成"在跑"的那几间先停掉。

    删除模组对正在跑的世界会直接 409（这是设计：面板不替用户停服，停服会踢玩家），
    所以删文件那两步必须先把世界停下来才验得动——真实用户走的就是这条路。
    """
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


def toasts(b):
    """界面上飘着的那几句（失败时后端那句中文就在这儿，不读它只能看到"没反应"）。"""
    return b.js("[...document.querySelectorAll('.toast')].map(t => t.innerText.replace(/\\s+/g,' ').trim())")


def main():
    width = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    height = int(sys.argv[2]) if len(sys.argv) > 2 else 900
    out = sys.argv[3] if len(sys.argv) > 3 else ""
    fails = []

    # ---- 造前提：一个"工坊版本对不上"的模组 + 一条没见过的手工 ID ----
    seed_workshop_cache("900000700", "背包整理（夹具）", "9.9")
    write_modinfo("900000700", "背包整理（夹具）", "0.0.1")  # 磁盘上这份是旧的
    st, body = req("GET", "/api/v1/mods")
    # 这一次 GET 本身就是扫盘：它把 version 刷成 0.0.1 的同时不该碰到工坊那一列。
    # 先在这里验一次，后面界面上真出问题时分得清是后端没算出来还是前端没画出来。
    row = [x for x in (body.get("data") or {}).get("mods") or []
           if x.get("workshopId") == "900000700"]
    print("接口那一条：%s" % json.dumps(row, ensure_ascii=False))
    if not row or not row[0].get("versionDiffers") or row[0].get("workshopVersion") != "9.9":
        print("FAIL 接口就没算出版本差异，后面界面上的失败都是它的连带")
        return 1
    req("POST", "/api/v1/mods/setup", {"workshopId": "900000700"})
    req("POST", "/api/v1/mods/setup", {"workshopId": "900000777"})
    # 「删除」那一步要一个"目录在磁盘上、也在订阅清单里、还被某个开着的世界用着"的靶子。
    # 上一轮跑完它已经被真删掉了，所以这里自己把它造回来，而不是依赖 ui_dev 的夹具没被动过。
    write_modinfo("900000002", "小地图共享（队友位置）", "1.0")
    req("POST", "/api/v1/mods/setup", {"workshopId": "900000002"})
    # 让地表真的开着它俩：删除那一行的"开着它的世界"才不是空话，
    # 而 900000700 开着正用来证明"删文件重下"不会把开关改掉。
    req("PUT", "/api/v1/clusters/Cluster_1/shards/Master/mods/workshop-900000002",
        {"enabled": True})
    req("PUT", "/api/v1/clusters/Cluster_1/shards/Master/mods/workshop-900000700",
        {"enabled": True})
    # 停在这一步之后：开关要留着（那才是"删文件重下不许动开关"的样本），
    # 但世界得停下来，否则删除会被 409 拒掉——那一条拒是设计，不是 bug。
    if not stop_running():
        return 1

    b = Browser(width, height)
    try:
        b.goto(BASE + "/?cdp=%d/#/mods" % int(time.time() * 1000))
        if not b.wait("!!document.querySelector('.login-form, .tabs')"):
            print("FAIL 模组页没起来")
            return 1
        if b.js("!!document.querySelector('.login-form')"):
            b.js("(() => { const f = document.querySelectorAll('.login-form input');"
                 " f[0].value='admin'; f[1].value='admin';"
                 " f[0].dispatchEvent(new Event('input', {bubbles:true}));"
                 " f[1].dispatchEvent(new Event('input', {bubbles:true})); })()")
            b.js("(() => { const f = document.querySelector('.login-form');"
                 " if (!f) return false;"
                 " const s = f.querySelector('.login-submit') ||"
                 " [...f.querySelectorAll('button')].pop(); s.click(); return true; })()")
            if not b.wait("!!document.querySelector('.tabs')"):
                print("FAIL 登录后没进到面板（账号或密码不对，/api/v1/host 挂了？）")
                return 1
        # 按页签上那颗序号点，不按整段文字：#85 把这屏的标签从「装到这台机器」改成了
        # 「这台机器上的文件」，按文字找的探针那次就点空了。
        click_js(b, ".tabs button",
                 "t => { const s = t.querySelector('.tab-step'); return s && s.textContent.trim() === '2'; }")
        if not b.wait("document.querySelectorAll('.mod-line').length > 0"):
            print("FAIL 第 2 步的磁盘清单没出来")
            return 1

        v = b.js(READ_ROWS)
        print("卡片标题：%s" % " / ".join(v["heads"]))
        print("磁盘行数：%d  待下载卡数：%d" % (len(v["rows"]), len(v["cards"])))
        for want in ("已下载", "已订阅、等重启下载", "上传本地模组包"):
            if not any(want in h for h in v["heads"]):
                fails.append("这一屏该分成三张卡，缺「%s」" % want)

        stale = [r for r in v["rows"] if "背包整理" in r["name"]]
        if not stale:
            fails.append("夹具里那个版本对不上的模组没出现在磁盘清单")
        else:
            r0 = stale[0]
            print("版本对不上那行：徽标=%s 动作=%s 在用=%s" % (r0["badges"], r0["acts"], r0["used"]))
            if not any("工坊版本对不上" in x for x in r0["badges"]):
                fails.append("工坊版本和磁盘版本不一致时这行要有徽标（用户问的就是"
                             "「工坊更新了这里会不会提示」）")
            if not any("删文件重下" in x for x in r0["acts"]):
                fails.append("版本对不上这行该给「删文件重下」，实际动作只有 %s" % r0["acts"])
        helper = [r for r in v["rows"] if any("面板自带" in x for x in r["badges"])]
        if not helper:
            fails.append("助手模组这一行要标成「面板自带」")
        elif any("删除" in x for x in helper[0]["acts"]):
            fails.append("面板自带的助手模组不该给删除按钮（删了等于把面板的眼睛拆了）")
        else:
            print("助手那行：徽标=%s 动作=%s" % (helper[0]["badges"], helper[0]["acts"]))
        used = [r for r in v["rows"] if r["used"].startswith("开着它的世界")]
        print("在用一行：%s" % (used[0]["used"] if used else "（没有）"))
        if not used:
            fails.append("刚在地表开了 workshop-900000002，这一栏却说不出哪个世界开着它")
        elif "Cluster_1 / Master" not in used[0]["used"]:
            fails.append("「开着它的世界」要写 Cluster_1 / Master 这种房间口径，实际 %r" % used[0]["used"])

        # 这两条从旧的 mods_cards_check.py 接过来（那一支已经按新结构重写掉了）：
        # 给本地模组挂一个 ?id=undefined 的工坊链接，点下去是工坊的一张错误页；
        # 封面没图时必须有那块字，不能留一个空白格让人以为图坏了。
        bad_links = [r["name"] for r in v["rows"]
                     if r["hrefs"] and not any(c.startswith("工坊 ") for c in r["chips"])]
        if bad_links:
            fails.append("没有工坊 ID 的行却给了工坊页链接：%s" % bad_links[:3])
        blank = [r["name"] for r in v["rows"] if not r["cover"] and not r["coverPh"].strip()]
        if blank:
            fails.append("封面格又没图又没字（空白格）：%s" % blank[:3])

        # 待下载：一条是缓存见过的名字，一条是没见过的
        print("待下载卡：%s" % json.dumps(v["cards"], ensure_ascii=False))
        if len(v["cards"]) < 1:
            fails.append("订阅清单里有没下载的 ID，这张卡却没列出来")
        known = [c for c in v["cards"] if "还没读到这个名字" not in c["name"]]
        unknown = [c for c in v["cards"] if "还没读到这个名字" in c["name"]]
        for c in v["cards"]:
            if c["name"].strip() in ("900000777", "398558175") or \
                    c["name"].strip().isdigit():
                fails.append("待下载那张卡把裸 ID 当名字显示了：%s" % c["name"])
            if not any("取消订阅" in x for x in c["foot"]):
                fails.append("待下载那一条要能取消订阅（填错 ID 时唯一的退路），实际 %s" % c["foot"])
        if not unknown:
            print("（这次没造出「没见过的那一条」，只验了有名字的一条）")
        elif not unknown[0]["note"].strip():
            fails.append("没见过的 ID 要说明为什么没有名字，不能只甩一句「还没读到」")
        if known and not any("900000700" in c["meta"] or "背包整理" in c["name"] for c in known):
            print("注意：900000700 文件还在磁盘上，它这会儿不算待下载（正常）")

        # ---- 操作 1：删文件重下（只删文件） ----
        click_js(b, ".mod-line-acts .linkish",
                "a => a.innerText.indexOf('删文件重下') >= 0 && "
                "a.closest('.mod-line').innerText.indexOf('背包整理') >= 0")
        dlg = None
        for _ in range(20):
            dlg = b.js(READ_DIALOG)
            if dlg:
                break
            time.sleep(0.2)
        print("重下确认框：%s" % json.dumps(dlg, ensure_ascii=False))
        if not dlg:
            fails.append("「删文件重下」点下去没弹确认框")
        else:
            for want in ("订阅", "开关"):
                if want not in (dlg["body"] + dlg["warn"]):
                    fails.append("重下这一步的确认框要说清「%s」不会被动，实际 %s"
                                 % (want, dlg["body"] + " / " + dlg["warn"]))
            accept(b)
            if not wait_row(b, "背包整理", True):
                fails.append("点了确认以后那一行还在磁盘清单里（吐司：%s）" % toasts(b))
            if os.path.isdir(os.path.join(MODS, "workshop-900000700")):
                fails.append("界面上说删掉了，磁盘上目录还在")
            if "900000700" not in setup_text():
                fails.append("重下不是卸载：订阅清单那一行必须留着，否则下次启动没有东西会重新下载")
            if not master_enabled("workshop-900000700"):
                fails.append("重下不该把地表的启用开关关掉——文件回来它就该照常生效")
            # 这一条同时是"待下载那张卡有没有名字"的最好样本：面板在线检索时见过它，
            # 现在它只剩清单里那一行，卡上必须显示缓存里那个名字，而不是 900000700。
            v3 = b.js(READ_ROWS)
            print("重下之后的待下载：%s" % json.dumps([c["name"] for c in v3["cards"]], ensure_ascii=False))
            if not any("背包整理" in c["name"] for c in v3["cards"]):
                fails.append("刚被重下的那条该出现在待下载里，并用缓存见过的名字显示（不是裸 ID）")

        # ---- 操作 2：删除一个真的被开着的模组（三件事一起做） ----
        click_js(b, ".mod-line-acts .linkish",
                "a => a.innerText.trim() === '删除' && "
                "a.closest('.mod-line').innerText.indexOf('小地图共享') >= 0")
        dlg = None
        for _ in range(20):
            dlg = b.js(READ_DIALOG)
            if dlg:
                break
            time.sleep(0.2)
        print("删除确认框：%s" % json.dumps(dlg, ensure_ascii=False))
        if not dlg:
            fails.append("「删除」点下去没弹确认框")
        else:
            if "Cluster_1 / Master" not in (dlg["body"] + dlg["warn"]):
                fails.append("确认框要说清会关掉哪几个世界的启用（这是不可逆操作）：%s" % dlg["body"])
            if "找不回来" not in dlg["warn"] and "停服" not in dlg["warn"]:
                fails.append("确认框要把代价说出口（文件删了找不回来 / 不替你停服）：%s" % dlg["warn"])
            accept(b)
            if not wait_row(b, "小地图共享", True):
                fails.append("点了确认以后那一行还在（吐司：%s）" % toasts(b))
            if os.path.isdir(os.path.join(MODS, "workshop-900000002")):
                fails.append("界面上说删掉了，workshop-900000002 还在磁盘上")
            # 判据走接口而不是原文：面板关这条启用是把 enabled 写成 false、条目留着
            # （配置还在，将来重新订阅不用重配），所以"文件里还找得到目录名"不等于"还开着"。
            if master_enabled("workshop-900000002"):
                fails.append("文件都没了、接口却说地表还开着它，引擎会静默禁用（模组开了没效果）")

        # ---- 操作 3：待下载那条的取消订阅 ----
        b.js("window.__before = document.querySelectorAll('.mod-card').length")
        click_js(b, ".mod-card-foot .btn",
                 "a => a.innerText.indexOf('取消订阅') >= 0 && "
                 "a.closest('.mod-card').innerText.indexOf('900000777') >= 0")
        dlg = None
        for _ in range(20):
            dlg = b.js(READ_DIALOG)
            if dlg:
                break
            time.sleep(0.2)
        print("取消订阅确认框：%s" % json.dumps(dlg, ensure_ascii=False))
        if not dlg:
            fails.append("「取消订阅」点下去没弹确认框")
        else:
            if "不会被删掉" not in dlg["warn"] and "已下载" not in dlg["warn"]:
                fails.append("取消订阅要说清它只撤清单、不删文件（文件在「已下载」那张卡里删）：%s"
                             % dlg["warn"])
            accept(b)
            if not b.wait("document.querySelectorAll('.mod-card').length < window.__before"):
                fails.append("点了确认以后那张待下载的卡还在")
            for line in setup_text().splitlines():
                if "900000777" in line:
                    fails.append("界面上撤了订阅，清单里还留着那一行")

        # ---- 上传那一格：默认名不再让用户填 ----
        v2 = b.js(READ_ROWS)
        labels = b.js("[...document.querySelectorAll('.field label')].map(l => l.innerText.trim()).join(' | ')")
        print("表单标签：%s" % labels)
        if "目录名（可留空" in (labels or ""):
            fails.append("落地目录名还在要求用户填")

        b.shoot(out or os.path.join(ROOT, ".tools", "tmp", "disk77_%d.png" % width))
        if v["overflow"]:
            fails.append("这个宽度下页面横向溢出了")
        print("截图：%s" % (out or "默认路径"))
    finally:
        b.close()

    if fails:
        for f in fails:
            print("FAIL " + f)
        return 1
    print("PASS 删掉一个模组这一条路通了，待下载那几条也不再是一串裸 ID")
    return 0


if __name__ == "__main__":
    sys.exit(main())
