# 房间归属（多租户）的实测：接口层的可见集与拒绝，加上三档身份各自的界面形态。
#
# 要盯住的是这几件事：
#   - 成员的房间名单在后端就裁好：他自己的 + 没指派的；未指派的那间他能看不能改。
#   - 界面上"改得动吗"只有一个判据（isAdmin 或 ownedByMe），而且改不动时必须说得出为什么。
#   - 指派弹窗只列接口收得下的那类账号（房间成员），并且不会留下"点了才知道被拒"的按钮。
#   - 开服向导对成员整个不出现：让他填完五步再被 403 打回来，是最坏的一种引导。
#   - 两个页面（总览 / 账号）说的归属必须是同一个来源，不能各拼一套话。
#     「所有房间」那一页原来也算一处，#129 把它整页下掉了，跨房间的指派搬进账号页。
#
# 数据全部落在本地夹具（.tmp_boot）里，一次都不碰云机。
import sys, os, time, json, base64, urllib.request, urllib.error
from datetime import datetime, timedelta

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import Browser, BASE

SHOTS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".tmp_boot", "shots")
os.makedirs(SHOTS, exist_ok=True)

MINE = "Cluster_1"      # 夹具里会被派给 bob 的那间
OTHER = "ProbeWorld"    # 保持未指派的那间，用来验"看得见但改不动"
BOB = ("bob_owner", "bob-pass-1")
# 第二个成员：没有房间。弹窗要验"派给某人的后果"，就得真有一项"和当前不同的成员"可选，
# 而唯一的成员就是当前负责人时，选他等于没变化，那句后果根本不会渲染出来。
BOB2 = ("bob_two", "bob-pass-2")
GUEST = ("guest_read", "guest-pass-1")


def api(method, path, body=None, user="admin", pw="admin"):
    data = None if body is None else json.dumps(body).encode("utf-8")
    auth = "Basic " + base64.b64encode(("%s:%s" % (user, pw)).encode()).decode()
    headers = {"Authorization": auth, "Content-Type": "application/json"}
    last = None
    # 本机这台面板偶尔会把连接直接断掉（实测出现过两次 10054，同一个请求立刻重试就 403）。
    # 第二次是在整轮验收里发生的：面板日志里那一发 POST 两次都在 0ms 内正常回了 403，
    # 客户端却两次都收到重置——是链路的问题，不是接口的。所以这里重试而不是让整轮
    # 半路 Traceback：这一轮里所有写动作都是幂等的（指派/改角色/启停重复一次只会得到
    # "已经是这样了"），不会把好端端的数据改坏。间隔逐次拉长（0.5s→2s→5s），
    # 因为两次挨得那么近的尝试（0.4s）会一起撞上同一阵抖动。
    for i, wait in enumerate((0.5, 2.0, 5.0)):
        req = urllib.request.Request(BASE + path, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                return r.status, json.loads(r.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            raw = e.read().decode("utf-8")
            try:
                return e.code, json.loads(raw)
            except Exception:
                return e.code, {"raw": raw[:300]}
        except (ConnectionResetError, urllib.error.URLError) as e:
            last = e
            print("  ·  连接被断开了（%s），第 %d 次重试" % (type(e).__name__, i + 1))
            time.sleep(wait)
    raise last


ok = True

# 别的实测脚本留下的临时账号前缀（flt_bot 那两个是管理员）。
LEFTOVER_PREFIXES = ("flt_", "probe-", "probe_", "ui_", "tmp_")


def sweep_leftovers():
    """开局把上一轮被打断的实测留下的临时账号清掉。

    这一轮有半边判断的前提是"这台机器上只剩 admin 一个管理员"（账号页要据此把
    降级和删除两个按钮藏起来）。ui_filters_check 会建两个管理员，正常收尾会删，
    但那次跑到一半被我 kill 了，号就留在了 .tmp_boot 的库里——于是界面是对的、
    报错却是红的，人得先想明白"为什么 admin 现在不是最后一个管理员"。
    脏环境不该由被检查的代码来背，所以这里先清，再把前提本身摊成一条检查。
    """
    st, accs = api("GET", "/api/v1/accounts")
    for a in (accs.get("data") or []):
        name = a.get("username") or ""
        if a.get("self") or not name.startswith(LEFTOVER_PREFIXES):
            continue
        if a.get("role") == "member":
            # 名下还有房间的删不掉（接口 409 要先撤指派），先把它派回未指派。
            st2, cl = api("GET", "/api/v1/clusters")
            for c in (cl.get("data") or []):
                if c.get("ownerAccountId") == a.get("id"):
                    api("PUT", "/api/v1/clusters/%s/owner" % c["name"], {"accountId": 0})
        api("DELETE", "/api/v1/accounts/%s" % a["id"])


def check(name, cond, extra=""):
    global ok
    print(("  OK   " if cond else "  FAIL ") + name + (("  " + str(extra)) if extra else ""))
    if not cond:
        ok = False


def api_phase():
    print("\n== 接口层 ==")
    sweep_leftovers()
    st, pre = api("GET", "/api/v1/accounts")
    admins = [a.get("username") for a in (pre.get("data") or []) if a.get("role") == "admin"]
    check("前提：这台机器上只剩 admin 一个管理员", admins == ["admin"], admins)
    st, sess = api("GET", "/api/v1/session")
    d = sess.get("data", {})
    check("管理员身份读得到", st == 200 and d.get("username") == "admin", st)
    roles = d.get("roles") or []
    check("角色字典有三档且带中文说法", len(roles) == 3 and all(r.get("label") for r in roles),
          [r.get("label") for r in roles])
    check("角色字典里没有裸码值", all("member" != r.get("label") and "viewer" != r.get("label") for r in roles),
          [r.get("label") for r in roles])
    check("管理员可指派", d.get("canAssignOwner") is True)

    for name, pw, role in (("bob_owner", BOB[1], "member"),
                           ("bob_two", BOB2[1], "member"),
                           ("guest_read", GUEST[1], "viewer")):
        st, _ = api("POST", "/api/v1/accounts", {"username": name, "password": pw, "role": role})
        if st not in (200, 201, 409):
            check("建测试账号 " + name, False, st)
    st, accs = api("GET", "/api/v1/accounts")
    lst = accs.get("data") or []
    by = {a["username"]: a for a in lst}
    check("账号列表里有 bob", "bob_owner" in by, list(by))
    check("角色说法由后端给（不出现 member）",
          all(a.get("roleLabel") and a["roleLabel"] not in ("admin", "member", "viewer") for a in lst),
          [(a["username"], a.get("roleLabel")) for a in lst])
    check("归属列有说法字段", all(a.get("ownedLabel") for a in lst),
          [(a["username"], a.get("ownedLabel")) for a in lst])
    bob = by.get("bob_owner", {})
    guest = by.get("guest_read", {})

    st, r = api("PUT", "/api/v1/clusters/%s/owner" % MINE, {"accountId": bob.get("id")})
    check("把 %s 派给 bob = 200" % MINE, st == 200, (st, r))
    check("指派回答里带中文说法", (r.get("data") or {}).get("ownerLabel", "").find("bob_owner") >= 0, r.get("data"))

    st, r = api("PUT", "/api/v1/clusters/%s/owner" % MINE, {"accountId": by.get("admin", {}).get("id")})
    check("派给管理员被拒（409）", st == 409, (st, r.get("error")))
    st, r = api("PUT", "/api/v1/clusters/%s/owner" % MINE, {"accountId": guest.get("id")})
    check("派给只读账号被拒（409）", st == 409, (st, r.get("error")))

    st, cl = api("GET", "/api/v1/clusters", user=BOB[0], pw=BOB[1])
    rows = {c["name"]: c for c in (cl.get("data") or [])}
    check("bob 看见自己那间", MINE in rows, list(rows))
    check("未指派的房间对成员也看得见（否则他会以为房间没了）", OTHER in rows, list(rows))
    check("bob 那间的 ownedByMe 为真", rows.get(MINE, {}).get("ownedByMe") is True, rows.get(MINE))
    check("未指派那间对 bob 也是改不动", rows.get(OTHER, {}).get("ownedByMe") is False, rows.get(OTHER))
    check("归属说法在房间条目里（弹窗与徽标共用）",
          (rows.get(MINE, {}).get("ownerLabel") or "").find("bob_owner") >= 0,
          rows.get(MINE, {}).get("ownerLabel"))
    check("指派记录那句带指派人", "由 admin 指派" in (rows.get(MINE, {}).get("assignedNote") or ""),
          rows.get(MINE, {}).get("assignedNote"))

    st, s2 = api("GET", "/api/v1/session", user=BOB[0], pw=BOB[1])
    sd = s2.get("data", {})
    check("bob 不能指派", sd.get("canAssignOwner") is False, sd.get("canAssignOwner"))
    check("bob 的 myClusters 里有那间", MINE in (sd.get("myClusters") or []), sd.get("myClusters"))

    st, _ = api("POST", "/api/v1/clusters/%s/owner" % MINE, {}, user=BOB[0], pw=BOB[1])
    check("bob 自己转派被拒（403）", st == 403, st)
    st, r = api("POST", "/api/v1/clusters/%s/shards/Master/start" % OTHER, {}, user=BOB[0], pw=BOB[1])
    check("bob 起未指派的房间被拒（403）", st == 403, (st, r.get("error")))
    check("那句拒的原因说得出下一步", "指派" in (r.get("error") or ""), r.get("error"))
    st, r = api("POST", "/api/v1/clusters/%s/shards/Master/start" % MINE, {}, user=BOB[0], pw=BOB[1])
    check("bob 起自己那间不被权限层拦", st != 403, (st, r.get("error")))
    if st in (200, 201):
        api("POST", "/api/v1/clusters/%s/shards/Master/stop" % MINE, {"saveFirst": True}, user=BOB[0], pw=BOB[1])
    st, r = api("POST", "/api/v1/clusters/%s/shards/Master/start" % MINE, {}, user=GUEST[0], pw=GUEST[1])
    check("只读账号任何写都被拒", st in (401, 403), (st, r.get("error")))

    st, rooms = api("GET", "/api/v1/rooms", user=BOB[0], pw=BOB[1])
    items = rooms.get("data", {}).get("items") or []
    names = sorted({x["cluster"] for x in items})
    check("bob 的房间总览同样裁过", OTHER in names and MINE in names, names)
    check("每行带归属说法", all(x.get("ownerLabel") for x in items),
          [(x["cluster"], x["shard"], x.get("ownerLabel"), x.get("ownerAccountId")) for x in items])
    mine_rows = [x for x in items if x["cluster"] == MINE]
    check("自己那间的每一行都标了 ownedByMe", all(x.get("ownedByMe") for x in mine_rows), mine_rows[:1])

    # 第二个成员（一间房都没派给他）：界面上"别人的房间不出现在列表里"这句话，
    # 只有拿一个真有别人的房间的账号才验得出来。
    st, cl2 = api("GET", "/api/v1/clusters", user=BOB2[0], pw=BOB2[1])
    rows2 = {c["name"]: c for c in (cl2.get("data") or [])}
    check("没有房间的成员看不见别人名下的那间，只看见未指派那间", list(rows2) == [OTHER], list(rows2))
    st, s3 = api("GET", "/api/v1/session", user=BOB2[0], pw=BOB2[1])
    sd3 = s3.get("data", {})
    check("他的名下房间是空的", (sd3.get("myClusters") or []) == [], sd3.get("myClusters"))
    check("他也不能指派", sd3.get("canAssignOwner") is False, sd3.get("canAssignOwner"))
    two = by.get("bob_two", {})
    st, r = api("PUT", "/api/v1/accounts/%s/role" % two.get("id"), {"role": "viewer"})
    check("名下没有房间的成员可以直接改只读（409 只卡真有名下房间的那个）", st == 200, (st, r.get("error")))

    st, r = api("PUT", "/api/v1/accounts/%s/role" % bob.get("id"), {"role": "viewer"})
    check("名下有房间的成员直接改只读会被拒（409）", st == 409, (st, r.get("error")))
    check("那句 409 说得出要先撤哪一步",
          "归属" in (r.get("error") or "") and "撤" in (r.get("error") or "")
          and "派给" in (r.get("error") or ""),
          r.get("error"))

    st, r = api("PUT", "/api/v1/clusters/%s/owner" % MINE, {"accountId": 0})
    check("取消指派 = 200", st == 200, (st, r.get("data")))
    st, cl = api("GET", "/api/v1/clusters")
    back = {c["name"]: c for c in (cl.get("data") or [])}
    check("取消后回到未指派", back.get(MINE, {}).get("ownerLabel") == "未指派",
          back.get(MINE, {}).get("ownerLabel"))
    check("取消后 accountId 归零", back.get(MINE, {}).get("ownerAccountId") == 0,
          back.get(MINE, {}).get("ownerAccountId"))
    # 浏览器阶段要"派给了人"的那间，所以重新派一次。
    api("PUT", "/api/v1/clusters/%s/owner" % MINE, {"accountId": bob.get("id")})
    # 上面那次"改成只读"是拿来验规则的，验完改回去：弹窗那一段要真有两位成员可选。
    st, r = api("PUT", "/api/v1/accounts/%s/role" % two.get("id"), {"role": "member"})
    check("改回房间成员 = 200（这条换法接口本来就该收）", st == 200, (st, r.get("error")))
    return bob.get("id")


def arm(b):
    """把前端自己的报错接住。

    没有这一层，"整页空白"、"接口没数据"和"渲染到一半炸了"在截图上长得一模一样。
    Vue 的"模板里用了没有的属性"走 console.warn 而不是 error，所以两条都收，
    但只有 error 判失败、warn 单独报出来看——不然别处一句无关的提醒就会盖掉这一页真正的病。
    """
    return b.js(r"""
    (() => {
      if (window.__armed) return 'again';
      window.__errs = []; window.__warns = [];
      const ce = console.error, cw = console.warn;
      console.error = function () { window.__errs.push(Array.from(arguments).join(' ')); ce.apply(console, arguments); };
      console.warn = function () { window.__warns.push(Array.from(arguments).join(' ')); cw.apply(console, arguments); };
      window.addEventListener('error', (e) => window.__errs.push(String(e.message)));
      window.addEventListener('unhandledrejection', (e) => window.__errs.push('promise: ' + String(e.reason)));
      window.__armed = true;
      return 'armed';
    })()""")


def peek(b, tag):
    """把这一路攒下的前端报错摊开，摊完清空——不然上一次路过的那条会算到下一页头上。"""
    got = b.js("({errs: window.__errs || ['没装探针'], warns: window.__warns || []})")
    check(tag + "这一路没有前端报错", not got["errs"], got["errs"][:3])
    if got["warns"]:
        print("       （告警）" + json.dumps(got["warns"][:3], ensure_ascii=False))
    b.js("window.__errs = []; window.__warns = []")


def cold_login(b, user, pw):
    b.goto(BASE + "/#/")
    b.js("sessionStorage.removeItem('dstpanel.creds')")
    b.reload()
    if not b.wait("!!document.querySelector('.login-form')", tries=40):
        print("       登录页没出现：" + json.dumps(page_state(b), ensure_ascii=False))
        return False
    b.js("""(() => {
      const ins = document.querySelectorAll('.login-card input');
      const set = (n, v) => { n.value = v; n.dispatchEvent(new Event('input', { bubbles: true })); };
      set(ins[0], %s); set(ins[1], %s);
      document.querySelector('.login-submit').click();
    })()""" % (json.dumps(user), json.dumps(pw)))
    gone = b.wait("!document.querySelector('.login-form')", tries=40)
    if not gone:
        # 停在登录页时把接口回的那句摊开：不然大半夜只能对着一个空白页猜是密码错了
        # 还是前端根本没发出请求。
        print("       仍停在登录页：" + json.dumps(page_state(b), ensure_ascii=False))
    # 重载会把探针连同 window 一起冲掉，所以每次登录完都要重新装。
    arm(b)
    return gone


def page_state(b):
    return b.js(r"""
    (() => {
      const t = (n) => (n ? n.textContent.trim() : '');
      return {
        hash: location.hash,
        stillForm: !!document.querySelector('.login-form'),
        loginErr: t(document.querySelector('.login-error, .err-box')),
        toast: Array.from(document.querySelectorAll('.toast, .toast-card')).map(t).slice(0, 3),
        ins: Array.from(document.querySelectorAll('.login-card input')).map((n) => n.type),
        roomCards: document.querySelectorAll('.room-name').length,
      };
    })()""")


def goto_view(b, hash, until_expr, tries=40):
    """换页 = 改 hash + 显式重载 + 重装探针。三件事少做任何一件，后面读到的都是上一页。"""
    b.js("location.hash = %s" % json.dumps(hash))
    b.reload()
    if not b.wait(until_expr, tries=tries):
        return False
    arm(b)
    return True


def js_owner_chips(b):
    """总览页每张房间卡上的归属那一枚，以及每个世界那一行的按钮/那句"动不了"。

    lock-chip 的可见文字是固定的一句"看得到，动不了"，为什么动不了写在 title 上
    （成员和只读账号的理由不一样），所以这里把文字和 title 都读回来。
    """
    return b.js(r"""
    (() => {
      const t = (n) => (n ? n.textContent.trim() : '');
      const a = (n, k) => (n ? (n.getAttribute(k) || '') : '');
      return Array.from(document.querySelectorAll('section.card'))
        .filter((c) => c.querySelector('.room-name'))
        .map((c) => ({
          room: t(c.querySelector('.room-name')),
          owner: t(c.querySelector('.card-head .chip:not(.hide-sm)')),
          ownerWhy: a(c.querySelector('.card-head .chip:not(.hide-sm)'), 'title'),
          chips: Array.from(c.querySelectorAll('.card-head .chip')).map(t),
          btns: Array.from(c.querySelectorAll('.card-head .btn')).map(t),
          shardBtns: Array.from(c.querySelectorAll('.shard-row')).map((n) => ({
            name: t(n.querySelector('.shard-name')),
            // 「详情/收起」是展开用的把手，跟"改不改得动"无关；留在按钮清单里
            // 会让"改不动的那行一个动作按钮都没有"这条判断永远判不过。
            btns: Array.from(n.querySelectorAll('.btn')).map(t).filter((x) => x !== '详情' && x !== '收起'),
            lock: t(n.querySelector('.lock-chip')),
            lockWhy: a(n.querySelector('.lock-chip'), 'title'),
          })),
        }));
    })()""")


def room_tab_dirs(b):
    """总览那一排页签各自的存档目录名（页签 title 写的正是「存档目录名：X · 在跑 n/m 个世界」）。"""
    return b.js(r"""(() => {
      const dir = (n) => ((n.getAttribute('title') || '').match(/存档目录名：([^ ·]+)/) || [])[1] || '';
      return Array.from(document.querySelectorAll('.room-tabs button')).map(dir).filter((x) => x);
    })()""")


def select_room(b, dirname):
    """点到那一间的页签上，并等到卡片头那枚目录名真的换成它。

    等的是证据不是秒数：切页签会重取接口，睡固定时长在慢的那一档读到的是上一间，
    于是"未指派那间标出来了"这条量的其实是别的房间（#106 把一次摊开所有房间改成
    一次只摊一间之后，这个脚本就是这么连着红了三条的）。
    """
    hit = b.js("""(() => {
      const dir = (n) => ((n.getAttribute('title') || '').match(/存档目录名：([^ ·]+)/) || [])[1] || '';
      const t = Array.from(document.querySelectorAll('.room-tabs button')).find((x) => dir(x) === %s);
      t && t.click();
      return !!t;
    })()""" % json.dumps(dirname))
    if not hit:
        return False
    for _ in range(24):
        if b.js("""(() => { const n = document.querySelector('section.card .room-name');
          return !!n && ((n.closest('.card-head') || {}).textContent || '').indexOf(%s) >= 0; })()"""
                 % json.dumps(dirname)):
            return True
        time.sleep(0.25)
    return False


def all_room_cards(b):
    """逐间点过去收房间卡：#106 之后总览一次只摊一间，"每一间的归属章"只能这么收。"""
    out, seen = [], set()
    for name in room_tab_dirs(b):
        if name in seen or not select_room(b, name):
            continue
        seen.add(name)
        out.extend(js_owner_chips(b))
    return out


def js_dialog(b):
    return b.js(r"""
    (() => {
      const t = (n) => (n ? n.textContent.trim() : '');
      const d = document.querySelector('.owner-dialog');
      if (!d) return null;
      return {
        title: t(d.querySelector('h3')),
        lede: t(d.querySelector('.owner-lede')),
        rule: t(d.querySelector('.owner-rule')),
        note: t(d.querySelector('.owner-dialog .note')),
        options: Array.from(d.querySelectorAll('#owner-pick option')).map((o) => t(o)),
        // 没换选项时那句"按下去不会有任何变化"顶掉的是后果说明那一格，两个不会同时出现。
        sameLine: t(d.querySelector('p.muted:not(.owner-lede):not(.owner-rule)')),
        saveDisabled: d.querySelector('.dialog-foot .btn.primary') ?
          d.querySelector('.dialog-foot .btn.primary').disabled : null,
        box: (() => { const r = d.getBoundingClientRect();
                      return {x: Math.round(r.x), w: Math.round(r.width)}; })(),
        docW: document.documentElement.scrollWidth, winW: window.innerWidth,
      };
    })()""")


def pick_option(b, label_part):
    return b.js("""(() => {
      const s = document.querySelector('#owner-pick');
      if (!s) return false;
      const o = Array.from(s.options).find((x) => x.textContent.indexOf(%s) >= 0);
      if (!o) return false;
      s.value = o.value;
      s.dispatchEvent(new Event('change'));
      return true;
    })()""" % json.dumps(label_part, ensure_ascii=False))


def toggle_theme(b):
    """点顶栏那枚主题图标。找不到按钮就不算点过——只看 dataset.theme 会骗人。"""
    hit = b.js("""(() => {
      const btns = Array.from(document.querySelectorAll('.topbar .icon-btn'));
      const t = btns.find((n) => (n.title || '').indexOf('切到') === 0);
      if (!t) return false;
      t.click();
      return true;
    })()""")
    if not hit:
        raise SystemExit("顶栏找不到主题图标，后面的主题断言没有意义")
    time.sleep(0.6)


def admin_browser():
    print("\n== 界面 · 管理员 ==")
    b = Browser(1440, 900)
    if not cold_login(b, "admin", "admin"):
        raise SystemExit("管理员登录没成")
    b.wait("!!document.querySelector('.room-name')", tries=40)
    time.sleep(0.8)
    cards = all_room_cards(b)
    mine = [c for c in cards if any("bob_owner" in x for x in c["chips"])]
    check("总览上有一枚写着负责人名字的归属章", bool(mine), cards[:2])
    check("未指派那间也标出来了", any(any(x == "未指派" for x in c["chips"]) for c in cards),
          [c["chips"] for c in cards])
    check("管理员每间房都有「指派」", all(any("指派" in x for x in c["btns"]) for c in cards),
          [c["btns"] for c in cards])
    b.shoot(os.path.join(SHOTS, "owner-overview-admin-1440-dark.png"))

    # 打开弹窗：选项里只应有成员 + "不派给谁"。上面逐间点过去收卡之后停在最后一间，
    # 这里显式点回 Cluster_1（和房间页那一段同一个道理：别拿顺序赌当前选中的是哪间）。
    check("点得回 Cluster_1 那一间", select_room(b, MINE), MINE)
    b.js("""(() => {
      const n = Array.from(document.querySelectorAll('section.card')).find((c) =>
        c.textContent.indexOf('Cluster_1') >= 0 || c.textContent.indexOf('本地界面实测房') >= 0);
      const btn = n ? Array.from(n.querySelectorAll('.card-head .btn')).find((x) => x.textContent.indexOf('指派') >= 0) : null;
      if (btn) btn.click();
    })()""")
    b.wait("!!document.querySelector('.owner-dialog')", tries=30)
    time.sleep(0.6)
    dlg = js_dialog(b)
    check("指派弹窗开了", bool(dlg), dlg)
    if dlg:
        check("弹窗标题带着房间名", "指派" in (dlg["title"] or ""), dlg["title"])
        check("选项里有「不派给谁」", any("不派给谁" in o for o in dlg["options"]), dlg["options"])
        check("选项里有 bob_owner", any("bob_owner" in o for o in dlg["options"]), dlg["options"])
        check("选项里两位成员都在（当前负责人也还在）",
              sum(1 for o in dlg["options"] if "bob_" in o) == 2, dlg["options"])
        check("选项里没有只读那类账号", not any("guest_read" in o for o in dlg["options"]), dlg["options"])
        # 不能写成"选项里没有'管理员'四个字"：「不派给谁」那句本来就带"由管理员自己管"。
        # 要验的是名单里没把管理员账号列成可派的对象。
        check("选项里没有管理员那一档",
              not any("（管理员" in o for o in dlg["options"]) and
              not any(o.startswith("admin") for o in dlg["options"]), dlg["options"])
        check("当前那一项选中时保存是禁的", dlg["saveDisabled"] is True, dlg["saveDisabled"])
        check("禁着的这一项也说了为什么", "还是这一项" in (dlg["sameLine"] or ""), dlg["sameLine"])
        check("解释了为什么没有管理员与只读", "只读" in (dlg["rule"] or "") and "管理员" in (dlg["rule"] or ""),
              dlg["rule"])
        check("弹窗说得出当前归属", "现在" in (dlg["lede"] or ""), dlg["lede"])
        # 指派那句的时刻必须按浏览器所在时区渲染：接口给的是 UTC，早先后端把 UTC 拼进
        # 整句话里，同一屏就出现两个相差几小时的钟点。拿浏览器的时区偏移反推预期字符串，
        # 比写死 +8 可靠——换一台机器跑也不会假失败。
        st_c, cl_c = api("GET", "/api/v1/clusters")
        row_c = {c["name"]: c for c in (cl_c.get("data") or [])}.get(MINE, {})
        stamp = row_c.get("assignedAt") or ""
        note_c = row_c.get("assignedNote") or ""
        off = b.js("new Date().getTimezoneOffset()")
        want = ""
        try:
            want = (datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ")
                    - timedelta(minutes=int(off))).strftime("%Y-%m-%d %H:%M:%S")
        except (ValueError, TypeError):
            pass
        check("接口那句不带钟点（时刻是单独一个字段）",
              bool(note_c) and ":" not in note_c, note_c)
        check("弹窗里那句时刻 = 浏览器时区的 assignedAt",
              bool(want) and want in (dlg["lede"] or ""), (stamp, off, want, dlg["lede"]))
        b.shoot(os.path.join(SHOTS, "owner-dialog-admin-1440-dark.png"))
        # 换一项 → 必须当场说会发生什么，而且两种换法的后果是相反的两句话。
        pick_option(b, "不派给谁")
        time.sleep(0.5)
        after = js_dialog(b)
        check("改成不派给谁后出现了后果说明", "未指派" in (after["note"] or ""), after["note"])
        # 未指派 ≠ 藏起来：接口模型是"人人看得见、谁都改不动（管理员除外）"，
        # 这句说反了会误导管理员以为撤指派等于下架。
        check("收回来那句说的是成员还能看见但改不动",
              "未指派" in (after["note"] or "") and "看到它的状态" in after["note"]
              and "改它" in after["note"], after["note"])
        check("改了选项保存就不禁", after["saveDisabled"] is False, after["saveDisabled"])
        pick_option(b, "bob_two")
        time.sleep(0.5)
        again = js_dialog(b)
        check("派给某人的后果说的是他的房间名单变化", "进他的房间名单" in (again["note"] or ""), again["note"])
        check("那句后果同时说清未指派的房间他还是看得见改不动",
              "改不动" in (again["note"] or ""), again["note"])
        check("派给谁那一句里带的是选中那个人的名字", "bob_two" in (again["note"] or ""), again["note"])
        b.js("document.querySelector('.owner-dialog .dialog-foot .btn.ghost').click()")
        time.sleep(0.4)
        check("关掉后弹窗不在了", js_dialog(b) is None)
        # 关掉再开必须回到"当前那一项"：上一轮选中的那项没保存，不该留下来。
        b.js("""(() => {
          const n = Array.from(document.querySelectorAll('section.card')).find((c) =>
            c.textContent.indexOf('Cluster_1') >= 0 || c.textContent.indexOf('本地界面实测房') >= 0);
          const btn = n ? Array.from(n.querySelectorAll('.card-head .btn')).find((x) => x.textContent.indexOf('指派') >= 0) : null;
          if (btn) btn.click();
        })()""")
        b.wait("!!document.querySelector('.owner-dialog')", tries=30)
        time.sleep(0.6)
        reopened = js_dialog(b)
        check("重开弹窗时选中回到当前负责人",
              reopened and any("bob_owner" in o for o in reopened["options"]) and
              reopened["saveDisabled"] is True, reopened and reopened["saveDisabled"])
        b.js("document.querySelector('.owner-dialog .dialog-foot .btn.ghost').click()")
        time.sleep(0.4)

    # 「所有房间」那一页在 #129 整页下掉了，跨房间的指派搬到账号页的「房间归属」那张卡。
    # 这一节因此改成量那张卡：逐间列得齐不齐、归属说法和接口是不是同一句、点得到指派吗。
    # （那一页原先还有的搜索/状态分段/归属筛选/排序没有搬过来——本机一共就几间房，
    #   总览下面那一排页签逐间给，筛它们等于给一个五指数的东西造尺子。）
    goto_view(b, "#/rooms", "!!document.querySelector('.e-title')")
    time.sleep(0.6)
    gone = b.js(r"""(() => {
      const t = (n) => (n ? n.textContent.trim() : '');
      return {
        title: t(document.querySelector('.e-title')),
        nav: Array.from(document.querySelectorAll('.nav-item span, .tabbar span')).map(t),
      };
    })()""")
    check("#/rooms 落到「没这个页面」而不是半截房间表", "没有对应的页面" in gone["title"], gone["title"])
    check("导航里也没有「所有房间」这一枚",
          not any("所有房间" in x for x in gone["nav"]), gone["nav"])

    # 这一句读的是接口原始包（{ok, data:[...]}），不是前端 api.js 拆过的那一层：
    # 本文件的 api() 走 urllib，拿到的是原始 body，data 直接就是房间数组。
    st, clusters = api("GET", "/api/v1/clusters")
    crow = clusters.get("data") or []
    goto_view(b, "#/accounts", "!!document.querySelector('table.data')")
    time.sleep(0.8)
    own = b.js(r"""(() => {
      const t = (n) => (n ? n.textContent.trim() : '');
      const card = Array.from(document.querySelectorAll('section.card')).find((c) =>
        Array.from(c.querySelectorAll('h3')).some((h) => h.textContent.trim() === '房间归属'));
      if (!card) return null;
      return {
        head: t(card.querySelector('.card-head .hint')),
        rows: Array.from(card.querySelectorAll('table.data tbody tr')).map((tr) => ({
          room: t(tr.querySelector('td b')),
          dir: t(tr.querySelector('td .muted.mono')),
          owner: t(tr.querySelector('.chip')),
          tip: (tr.querySelector('.chip') || {}).title || '',
          btns: Array.from(tr.querySelectorAll('.btn')).map(t),
        })),
      };
    })()""")
    check("账号页上有「房间归属」这一张卡", own is not None, own is None)
    if own:
        check("卡里的行数 == 接口给的房间数（同一份名单，不另扫一次盘）",
              len(own["rows"]) == len(crow),
              "界面 %d 行，接口 %d 间" % (len(own["rows"]), len(crow)))
        check("每行都有归属那一枚和「指派」",
              all(r["owner"] and any("指派" in x for x in r["btns"]) for r in own["rows"]),
              [(r["room"], r["owner"], r["btns"]) for r in own["rows"]])
        for c in crow:
            name = c.get("roomName") or c.get("name")
            row = [r for r in own["rows"] if r["room"] == name]
            check("「%s」那行的归属就是接口那句 ownerLabel" % name,
                  bool(row) and row[0]["owner"] == (c.get("ownerLabel") or ""),
                  (row[0]["owner"] if row else None, c.get("ownerLabel")))
        named = [r for r in own["rows"] if r["dir"]]
        check("房间名和存档目录名不同时，两个都摆出来",
              all(r["dir"] and r["room"] != r["dir"] for r in named), [(r["room"], r["dir"]) for r in named])
        b.js("""(() => {
          const card = Array.from(document.querySelectorAll('section.card')).find((c) =>
            Array.from(c.querySelectorAll('h3')).some((h) => h.textContent.trim() === '房间归属'));
          const tr = Array.from(card.querySelectorAll('table.data tbody tr')).find((x) =>
            (x.querySelector('td b') || {}).textContent.trim() === %s);
          const btn = tr ? Array.from(tr.querySelectorAll('.btn')).find((x) => x.textContent.indexOf('指派') >= 0) : null;
          if (btn) btn.click();
          return !!btn;
        })()""" % json.dumps(own["rows"][0]["room"]))
        b.wait("!!document.querySelector('.owner-dialog')", tries=30)
        time.sleep(0.6)
        dlg = js_dialog(b)
        check("从那张卡点「指派」开得出弹窗，标题是房间名",
              dlg and own["rows"][0]["room"] in dlg["title"], dlg and dlg["title"])
        b.js("document.querySelector('.owner-dialog .dialog-foot .btn.ghost').click()")
        time.sleep(0.4)
        b.shoot(os.path.join(SHOTS, "owner-rooms-card-admin-1440-dark.png"))

    # 账号页：三档角色 + 归属列。
    goto_view(b, "#/accounts", "!!document.querySelector('table.data')")
    time.sleep(0.8)
    acc = b.js(r"""
    (() => {
      const t = (n) => (n ? n.textContent.trim() : '');
      return {
        heads: Array.from(document.querySelectorAll('table.data thead th')).map(t),
        rows: Array.from(document.querySelectorAll('table.data tbody tr')).filter((tr) => tr.querySelector('td b')).map((tr) => ({
          user: t(tr.querySelector('td b')),
          badges: Array.from(tr.querySelectorAll('.badge')).map(t),
          // 归属固定在第三列（账号|权限|归属|最近登录|操作）。读整行的 .chip 会把
          // 第一列那两枚"当前/默认密码"混进来，那一列的说法就没法判了。
          owner: t(tr.children[2]),
          btns: Array.from(tr.querySelectorAll('.btn')).map(t),
          hints: Array.from(tr.querySelectorAll('.hint')).map(t),
        })),
        createOptions: Array.from(document.querySelectorAll('#acc-role option')).map(t),
      };
    })()""")
    check("表头有「归属」这一列", "归属" in acc["heads"], acc["heads"])
    check("新建账号的角色下拉有三档", len(acc["createOptions"]) == 3, acc["createOptions"])
    check("角色下拉里没有裸码值",
          not any(o.strip() in ("admin", "member", "viewer") for o in acc["createOptions"]), acc["createOptions"])
    bobrow = [r for r in acc["rows"] if r["user"] == "bob_owner"]
    check("账号页列出了 bob", bool(bobrow), acc["rows"])
    if bobrow:
        r0 = bobrow[0]
        check("bob 那行标着房间成员", any("房间成员" in x for x in r0["badges"]), r0["badges"])
        check("bob 那行写着名下 1 间", "名下 1 间" in r0["owner"], r0["owner"])
        # 悬浮标签以前写成 "1 间"，跟接口那句拼起来就是"名下 1 间1 间"。
        check("归属那一列没把房间数说两遍", "1 间1 间" not in r0["owner"], r0["owner"])
        check("那一枚悬浮说的是「哪几间」", "哪几间" in r0["owner"], r0["owner"])
        check("名下有房间时不给「改成只读」这个按下去会被拒的选项",
              not any("改成只读" in x for x in r0["btns"]), r0["btns"])
        check("被筛掉的那条把理由写在同一行", any("先撤指派" in x for x in r0["btns"] + r0["hints"]),
              (r0["btns"], r0["hints"]))
    adminrow = [r for r in acc["rows"] if r["user"] == "admin"]
    if adminrow:
        check("最后一个管理员不能降级或删除",
              not any("改成" in x for x in adminrow[0]["btns"]) and
              not any("删除" in x for x in adminrow[0]["btns"]),
              adminrow[0]["btns"])
        check("那一行自己说了为什么没有这两个按钮",
              any("最后一个管理员" in x for x in adminrow[0]["btns"] + adminrow[0]["hints"]),
              (adminrow[0]["btns"], adminrow[0]["hints"]))
        # admin 自己就是最后一个管理员：两条理由会同时成立，v-if 链只能显示一条，
        # 那一行就永远看不出"降级/删除是被规则藏起来的"。
        check("两条理由在同一句里都说全",
              any("最后一个管理员" in x and "改自己的密码" in x for x in adminrow[0]["hints"]),
              adminrow[0]["hints"])
    b.shoot(os.path.join(SHOTS, "owner-accounts-admin-1440-dark.png"))

    # 窄屏：总览与弹窗都不许横向溢出。
    b.set_viewport(390, 844)
    goto_view(b, "#/", "!!document.querySelector('.room-name')")
    time.sleep(0.9)
    narrow = all_room_cards(b)
    overflow = b.js("({docW: document.documentElement.scrollWidth, winW: window.innerWidth})")
    check("390 宽度下总览不横向溢出", overflow["docW"] <= overflow["winW"] + 1, overflow)
    check("手机上每一间都还列在页签条里", len(narrow) == 2, [c["room"] for c in narrow])
    check("手机上归属那一枚还在（不能靠悬停才看得见）",
          all(c["owner"] for c in narrow) and bool(narrow), narrow[:2])
    select_room(b, MINE)
    b.js("""(() => {
      const btn = Array.from(document.querySelectorAll('.card-head .btn')).find((x) => x.textContent.indexOf('指派') >= 0);
      if (btn) btn.click();
    })()""")
    b.wait("!!document.querySelector('.owner-dialog')", tries=30)
    time.sleep(0.6)
    nd = js_dialog(b)
    check("手机上弹窗没超出视口", nd and nd["box"]["x"] >= -1 and nd["box"]["x"] + nd["box"]["w"] <= nd["winW"] + 1,
          nd and (nd["box"], nd["winW"]))
    check("手机上弹窗不撑出横向滚动", nd and nd["docW"] <= nd["winW"] + 1, nd and (nd["docW"], nd["winW"]))
    b.shoot(os.path.join(SHOTS, "owner-dialog-admin-390-dark.png"))

    # 浅色主题（只能点顶栏那枚图标）。
    b.set_viewport(1440, 900)
    time.sleep(0.4)
    b.js("document.querySelector('.owner-dialog .dialog-foot .btn.ghost').click()")
    # 上一轮跑完留在 localStorage 里的可能就是浅色，所以不假设初始是深色：
    # 先把它压回深色，再点一次，验"确实换了"而不是"正好落在浅色上"。
    toggle_theme(b)
    if b.js("document.documentElement.dataset.theme") == "light":
        toggle_theme(b)
    before = b.js("document.documentElement.dataset.theme")
    toggle_theme(b)
    theme = b.js("document.documentElement.dataset.theme")
    check("那枚图标点下去确实换了主题", theme != before, (before, theme))
    b.js("""(() => {
      const btn = Array.from(document.querySelectorAll('.card-head .btn')).find((x) => x.textContent.indexOf('指派') >= 0);
      if (btn) btn.click();
    })()""")
    b.wait("!!document.querySelector('.owner-dialog')", tries=30)
    time.sleep(0.5)
    b.shoot(os.path.join(SHOTS, "owner-dialog-admin-1440-light.png"))
    b.js("location.hash = '#/'")
    time.sleep(0.5)
    b.shoot(os.path.join(SHOTS, "owner-overview-admin-1440-light.png"))
    check("点图标确实换到了浅色主题", theme == "light", theme)

    # 手机上的账号页是另一套模板（卡片而不是表格）。同一条规则在两处必须说一样的话，
    # 否则桌面解释了"为什么没有降级按钮"，手机只剩一排凭空少掉的按钮。
    b.set_viewport(390, 844)
    goto_view(b, "#/accounts", "!!document.querySelector('.mod-row')")
    time.sleep(0.8)
    mobile_acc = b.js(r"""
    (() => {
      const t = (n) => n ? n.textContent.trim() : '';
      return Array.from(document.querySelectorAll('.mod-row')).map((c) => ({
        user: t(c.querySelector('b')),
        txt: t(c),
        btns: Array.from(c.querySelectorAll('.btn')).map(t),
      }));
    })()""")
    acard = [c for c in mobile_acc if c["user"] == "admin"]
    check("手机上 admin 那张卡也说清了最后一个管理员",
          bool(acard) and "最后一个管理员" in acard[0]["txt"], acard[:1])
    check("手机上 admin 那张卡没有降级或删除按钮",
          bool(acard) and not any(("改成" in x or "删除" in x) for x in acard[0]["btns"]),
          acard[:1] and acard[0]["btns"])
    b.shoot(os.path.join(SHOTS, "owner-accounts-admin-390-light.png"))
    b.set_viewport(1440, 900)
    peek(b, "管理员")
    return b


def member_browser(b):
    print("\n== 界面 · 房间成员 ==")
    b.set_viewport(1440, 900)
    if not cold_login(b, BOB[0], BOB[1]):
        raise SystemExit("bob 登录没成")
    b.wait("!!document.querySelector('.room-name')", tries=40)
    time.sleep(1.0)
    cards = all_room_cards(b)
    check("bob 只看得到两间（自己那间 + 未指派那间）", len(cards) == 2, [c["room"] for c in cards])
    check("bob 的总览上没有「指派」按钮",
          all(not any("指派" in x for x in c["btns"]) for c in cards), [c["btns"] for c in cards])
    mine = [c for c in cards if any("bob_owner" in x for x in c["chips"])]
    other = [c for c in cards if any(x == "未指派" for x in c["chips"])]
    other_rows = other[0]["shardBtns"] if other else []
    check("自己那间有可点的启停按钮",
          mine and any(x for c in mine for s in c["shardBtns"] for x in s["btns"]),
          mine and mine[0]["shardBtns"])
    check("未指派那间也有世界行（不然下面两条是空转）", bool(other_rows), other and other[0])
    check("未指派那间收掉了启停按钮并说出为什么",
          bool(other_rows) and all(s["lock"] and not s["btns"] for s in other_rows), other_rows)
    check("那句说明写的是「看得到，动不了」",
          any("看得到，动不了" in s["lock"] for s in other_rows), [s["lock"] for s in other_rows])
    check("那句为什么挂在 title 上，说的是未指派这一档",
          all("还没指派" in (s["lockWhy"] or "") for s in other_rows) and bool(other_rows),
          [s["lockWhy"] for s in other_rows][:1])
    b.shoot(os.path.join(SHOTS, "owner-overview-member-1440-dark.png"))

    # 房间页：开服向导不该出现；自己那间的按钮还在，未指派那间整排换成一句为什么。
    # 进这一页时选中的是哪间由本地记录决定，所以先显式点到 Cluster_1，别拿顺序赌。
    goto_view(b, "#/room", "!!document.querySelector('#room-pick')")
    time.sleep(0.9)
    tabs = b.js(r"""
    (() => {
      const t = (n) => (n ? n.textContent.trim() : '');
      return {
        tabs: Array.from(document.querySelectorAll('.tabs > button')).map(t),
      };
    })()""")
    check("成员看不到「开服向导」这一格",
          not any("开服向导" in x for x in tabs["tabs"]), tabs["tabs"])

    def pick_room(b, name):
        return b.js("""(() => {
          const sel = document.querySelector('#room-pick');
          if (!sel) return false;
          sel.value = %s;
          sel.dispatchEvent(new Event('change'));
          return true;
        })()""" % json.dumps(name))

    check("换房间那个下拉用的是 #room-pick（两间都在里面）",
          pick_room(b, MINE), None)
    b.wait("document.querySelector('#room-pick') && document.querySelector('.action-row')", tries=30)
    time.sleep(1.0)
    mine_head = b.js(r"""
    (() => {
      const t = (n) => (n ? n.textContent.trim() : '');
      return {
        picked: (document.querySelector('#room-pick') || {}).value || '',
        btns: Array.from(document.querySelectorAll('.action-row .btn')).map(t),
      };
    })()""")
    check("确实切到了自己那间", mine_head["picked"] == MINE, mine_head["picked"])
    check("自己那间照样给启停和备份（撤按钮不是把成员整页缴械）",
          any("启动世界" in x or "停止世界" in x for x in mine_head["btns"]) and
          any("立即备份" in x for x in mine_head["btns"]), mine_head["btns"])
    check("自己那间没有「指派归属」这枚会拒的按钮",
          not any("指派" in x for x in mine_head["btns"]), mine_head["btns"])

    pick_room(b, OTHER)
    b.wait("document.querySelector('#room-pick') && document.querySelector('.action-row .assign-note')", tries=30)
    time.sleep(0.8)
    blocked = b.js(r"""
    (() => {
      const t = (n) => (n ? n.textContent.trim() : '');
      return {
        picked: (document.querySelector('#room-pick') || {}).value || '',
        notes: Array.from(document.querySelectorAll('.action-row .assign-note')).map(t),
        btns: Array.from(document.querySelectorAll('.action-row .btn')).map(t),
      };
    })()""")
    check("确实切到了未指派那间", blocked["picked"] == OTHER, blocked["picked"])
    check("未指派那间的动作行改成了那句为什么而不是按钮",
          any("还没指派" in x or "改不了" in x for x in blocked["notes"]), blocked["notes"][:2])
    check("这一行没有启动世界/立即备份这种按下去必然被拒的按钮",
          not any("启动世界" in x or "停止世界" in x or "立即备份" in x for x in blocked["btns"]),
          blocked["btns"])
    check("取加入命令与看地图还留着（它们不改任何东西）",
          any("取加入命令" in x for x in blocked["btns"]) and any("世界地图" in x for x in blocked["btns"]),
          blocked["btns"])
    b.shoot(os.path.join(SHOTS, "owner-room-member-1440-dark.png"))
    peek(b, "成员")


def viewer_browser():
    print("\n== 界面 · 只读账号 ==")
    b = Browser(1440, 900)
    if not cold_login(b, GUEST[0], GUEST[1]):
        raise SystemExit("只读账号登录没成")
    b.wait("!!document.querySelector('.room-name')", tries=40)
    time.sleep(1.0)
    cards = all_room_cards(b)
    check("只读账号也看不见「指派」", all(not any("指派" in x for x in c["btns"]) for c in cards),
          [c["btns"] for c in cards])
    rows = [s for c in cards for s in c["shardBtns"]]
    check("只读账号的每一行都标了动不了", rows and all(s["lock"] for s in rows),
          [s["btns"] for s in rows][:2])
    check("自己那间也不给只读账号留按钮", not any(s["btns"] for s in rows),
          [(s["name"], s["btns"]) for s in rows][:3])
    # 可见文字是固定那句"看得到，动不了"，只读账号专属的理由挂在 title 上。
    why = [s["lockWhy"] for c in cards for s in c["shardBtns"] if s["lockWhy"]]
    check("那句理由说的是只读账号本身", why and any("只读" in x for x in why), why[:2])
    check("归属那一枚也对只读账号解释了为什么动不了",
          any("只读" in (c["ownerWhy"] or "") for c in cards), [c["ownerWhy"] for c in cards][:2])
    b.shoot(os.path.join(SHOTS, "owner-overview-viewer-1440-dark.png"))
    peek(b, "只读")
    b.close()


def main():
    # 前端渲染期报错要能报回来：否则"整页空白"和"接口没数据"在现场分不出来。
    os.environ.setdefault("PYTHONIOENCODING", "utf-8")
    api_phase()
    b = admin_browser()
    member_browser(b)
    b.close()
    viewer_browser()
    print("\n截图：" + os.path.abspath(SHOTS))
    print("\n" + ("全部通过" if ok else "有失败项"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
