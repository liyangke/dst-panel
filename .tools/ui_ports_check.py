# 游戏端口页的一次真点：从浏览器里改端口，看三份文件到底变成了什么。
#
# 为什么单独一个脚本而不是只看截图：这一页有两个"截图看不出来"的断言——
# 一是配套端口那一格坏了的时候（手工造的房间根本没写这一行），一个号都不用改
# 也得能点写入把它补回来；二是改一个号要三份文件一起对齐，少一份就是洞穴连不上地面，
# 而引擎一句错都不报。所以这里走完 GET → 点按钮 → PUT → 回读整条链路，
# 并把三份文件里所有端口行的前后差异打出来。
#
# 跑法：python .tools/ui_ports_check.py [宽度]
# 前提：python .tools/ui_dev.py 已经把面板起在 127.0.0.1:8963。
# 它会先把 Cluster_1 的两个世界停下来（端口只在启动时读一次，在跑就该被拒），
# 跑完要恢复"在跑"的夹具状态就重启一次 ui_dev。
import io
import json
import os
import re
import sys
import time
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
KLEI = os.path.join(ROOT, ".tmp_boot", "klei", "Cluster_1")
FILES = {
    "cluster.ini": os.path.join(KLEI, "cluster.ini"),
    "Master/server.ini": os.path.join(KLEI, "Master", "server.ini"),
    "Caves/server.ini": os.path.join(KLEI, "Caves", "server.ini"),
}
OUT = os.path.join(ROOT, ".tools", "tmp", "shots")
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

READ = r"""(() => {
  const txt = (s) => { const e = document.querySelector(s); return e ? e.textContent.trim().replace(/\s+/g, ' ') : ''; };
  const btn = [...document.querySelectorAll('.card .btn.primary')].pop();
  return {
    path: txt('.stat-note.mono'),
    stats: [...document.querySelectorAll('.card .stat')].map((el) => ({
      k: txt2(el, '.stat-k'), v: txt2(el, '.stat-v'), note: txt2(el, '.stat-note'), cls: el.className,
    })),
    notes: [...document.querySelectorAll('.card .note')].map((x) => x.textContent.trim().replace(/\s+/g, ' ')),
    err: txt('.err-box'),
    btn: btn ? { text: btn.textContent.trim().replace(/\s+/g, ' '), disabled: btn.disabled } : null,
    inputs: [...document.querySelectorAll('.card input[type=number]')].map((i) => ({ v: i.value, disabled: i.disabled })),
    chips: [...document.querySelectorAll('.card .chip.mono')].map((x) => x.textContent.trim()),
    toast: [...document.querySelectorAll('.toast')].map((x) => x.textContent.trim().replace(/\s+/g, ' ')),
    overflow: document.documentElement.scrollWidth - innerWidth,
  };
  function txt2(el, s) { const e = el.querySelector(s); return e ? e.textContent.trim().replace(/\s+/g, ' ') : ''; }
})()"""


def api(method, path, body=None):
    """直接叫接口：停世界这种准备动作不该混进浏览器步骤里。"""
    import base64
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, method=method, headers={
        "Authorization": "Basic " + base64.b64encode(b"admin:admin").decode(),
        "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode())


def read_files():
    return {n: io.open(p, encoding="utf-8", errors="replace").read() for n, p in FILES.items()}


def write_file(name, text):
    with io.open(FILES[name], "w", encoding="utf-8", newline="\n") as fh:
        fh.write(text)


def port_lines(text):
    return " / ".join(l.strip() for l in text.splitlines()
                      if re.match(r"^\s*(master_port|server_port)\s*=", l)) or "（没有端口行）"


def diff(before, after):
    for n in FILES:
        mark = "改了" if before[n] != after[n] else "未动"
        print("  [%s] %s: %s" % (mark, n, port_lines(after[n])))
        if before[n] != after[n]:
            print("        之前: %s" % port_lines(before[n]))


def set_number(b, idx, value):
    """改输入框要走原生 setter + input 事件：直接赋 .value，Vue 的 v-model 收不到。"""
    return b.js("""(() => {{
      const el = document.querySelectorAll('.card input[type=number]')[{idx}];
      if (!el) return false;
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      setter.call(el, {value});
      el.dispatchEvent(new Event('input', {{ bubbles: true }}));
      return true;
    }})()""".format(idx=idx, value=json.dumps(str(value))))


def click_by_text(b, label):
    """按可见文案点按钮：这一页有不止一枚 ghost 小按钮，按序号点迟早会点到错的。"""
    ok = b.js("(() => { const el = [...document.querySelectorAll('.card button')]"
              ".find((x) => x.textContent.trim() === %s); if (!el) return false;"
              " el.click(); return true; })()" % json.dumps(label))
    if not ok:
        raise SystemExit("页面上没有文案为「%s」的按钮" % label)


def click_write(b):
    """点当前那颗主按钮（写入端口 / 补上配套端口），回来案文字。"""
    label = b.js("(() => { const btn = [...document.querySelectorAll('.card .btn.primary')].pop();"
                 " return btn ? btn.textContent.trim() : ''; })()")
    if not label:
        raise SystemExit("没找到主按钮")
    click_by_text(b, label)
    time.sleep(1.6)
    return label


def main():
    w = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    # 夹具默认是"两个世界都在跑"，而这一页在跑的时候只该显示拒绝写入，点不动。
    for shard in ("Master", "Caves"):
        api("POST", "/api/v1/clusters/Cluster_1/shards/%s/stop" % shard, {})
    time.sleep(0.4)

    b = Browser(w, 900 if w > 560 else 844)
    try:
        b.goto(BASE + "/#/")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("登录层没过去")
        b.js("location.hash = '#/room'")
        b.wait("!!document.querySelector('.tabs')")
        time.sleep(1.8)
        # 面板默认选中"在跑的那个房间"，这里要的是端口夹具那个房间。
        if not b.js("(() => { const s = document.querySelector('select');"
                    " if (!s) return false; s.value = 'Cluster_1';"
                    " s.dispatchEvent(new Event('change')); return true; })()"):
            raise SystemExit("没找到房间下拉")
        time.sleep(1.0)
        b.js("(() => { const t = document.querySelectorAll('.tabs button')[4]; t && t.click(); return !!t; })()")
        time.sleep(1.2)

        # —— 场景一：配套端口那一行压根没写（手工造的房间就是这样），
        #    顺带把洞穴的 master_ip 也抹掉：那是端口页能修的另一种"洞穴连不上地面"。——
        before = read_files()
        broken = re.sub(r"^\s*master_port\s*=.*\n", "", before["cluster.ini"], flags=re.M)
        if broken == before["cluster.ini"]:
            raise SystemExit("夹具的 cluster.ini 里没有 master_port 那一行，造不出坏样本")
        write_file("cluster.ini", broken)
        no_ip = re.sub(r"^\s*master_ip\s*=.*\n", "", before["Caves/server.ini"], flags=re.M)
        if no_ip == before["Caves/server.ini"]:
            raise SystemExit("夹具的洞穴 server.ini 里没有 master_ip 那一行，造不出坏样本")
        write_file("Caves/server.ini", no_ip)
        click_by_text(b, "重新读一次")
        time.sleep(1.2)
        s1 = b.js(READ)
        print("== 场景一：配套端口没写到文件 ==")
        print(json.dumps({"stats": s1["stats"], "notes": s1["notes"], "btn": s1["btn"],
                          "chips": s1["chips"], "path": s1["path"], "overflow": s1["overflow"]},
                         ensure_ascii=False, indent="  "))
        b.shoot(os.path.join(OUT, "ports_broken_%d.png" % w))
        if not s1["btn"] or s1["btn"]["disabled"]:
            raise SystemExit("坏样本下按钮还是灰的：这一格修不了")
        print("-- 点主按钮：%s" % click_write(b))
        mid = read_files()
        s2 = b.js(READ)
        print("-- 补写之后 --")
        print(json.dumps({"stats": s2["stats"], "notes": s2["notes"], "err": s2["err"],
                          "btn": s2["btn"], "toast": s2["toast"]}, ensure_ascii=False, indent="  "))
        b.shoot(os.path.join(OUT, "ports_repaired_%d.png" % w))
        diff({"cluster.ini": broken, "Master/server.ini": before["Master/server.ini"],
              "Caves/server.ini": before["Caves/server.ini"]}, mid)

        # —— 场景二：真的改一个号，看三份文件是否一起对齐 ——
        if not set_number(b, 1, 11006):
            raise SystemExit("没找到洞穴端口那一栏")
        time.sleep(0.4)
        s3 = b.js(READ)
        print("-- 改洞穴端口为 11006，按钮：%s" % json.dumps(s3["btn"], ensure_ascii=False))
        print("-- 点主按钮：%s" % click_write(b))
        time.sleep(0.4)
        after = read_files()
        s4 = b.js(READ)
        print("== 场景二结果 ==")
        print(json.dumps({"stats": s4["stats"], "notes": s4["notes"], "err": s4["err"],
                          "chips": s4["chips"], "toast": s4["toast"], "overflow": s4["overflow"]},
                         ensure_ascii=False, indent="  "))
        b.shoot(os.path.join(OUT, "ports_moved_%d.png" % w))
        diff(mid, after)
        for n in FILES:
            if os.path.exists(FILES[n] + ".bak"):
                print("  %s 留了 .bak" % n)
    finally:
        b.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
