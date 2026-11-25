#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""在云服务器上真跑一遍面板：Linux 成品能不能起、路径读得动、检索降级说实话。

为什么必须去那台机器：本机（Windows）出不了网，api.steampowered.com 直连超时，
所以本地探针只能验降级路径。降级验得再全也不等于验过成功路径——
"接口返回的形状对不对"只有真接口能回答，"Go 成品在 Linux 上到底跑不跑"也只有真机能回答。

没配 DST_STEAM_KEY 时只跑不需要 key 的两段（真安装目录只读 + 磁盘入库持久化），
配了才加跑在线三段。每段之前库都清零，这样"后面那段命中缓存"只能解释为"前面那段写进去的"。

远端写操作只落在 panel-probe/ 下（自己的库、自己的假安装目录），
真安装目录只读借用，不碰真存档、不启停任何分片。

key 的处理：只从环境变量 DST_STEAM_KEY 读；先验字符集再拼进 stdin 里的 bash 脚本
（ssh 走 bash -s，脚本本身占着 stdin，所以配置用 heredoc 落盘），
远端 umask 077，每段结束立刻删配置；本脚本任何一行输出都不含 key。
"""
import hashlib
import json
import os
import re
import secrets
import subprocess
import sys
import time

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOST = "dst-cloud"
BIN_LOCAL = os.path.join(ROOT, ".tmp_bin", "dstpanel-linux")
PROBE_DIR = "/home/ubuntu/dst/panel-probe"
BIN_REMOTE = PROBE_DIR + "/dstpanel"
# 只读地用真安装目录：检索会扫 mods/ 认"装没装"，这一步必须在真实环境里做。
DST_REAL = "/home/ubuntu/dst/dedicated_server"
# 需要写模组的段用自己的一份假安装目录，绝不往真目录里塞东西。
DST_FAKE = PROBE_DIR + "/dst"
PORT = 8941
ADMIN = "probe"
KEY_RE = re.compile(r"^[0-9A-Fa-f]{16,64}$")
BAD_KEY = "DEADBEEFDEADBEEFDEADBEEFDEADBEEF"

results = []
skipped = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  -- " + str(detail)[:280]) if detail else ""))


def skip(name, why):
    skipped.append(name)
    print("[SKIP] %s  -- %s" % (name, why))


def remote(script, timeout=180):
    """整段 bash 走 stdin 送进去：中文和 key 都不进本机 argv，也不会被 ps 看到。"""
    p = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", HOST, "bash -s"],
                       input=script.encode("utf-8"), capture_output=True, timeout=timeout)
    out = p.stdout.decode("utf-8", "replace")
    if p.returncode != 0:
        out += "\n[ssh rc=%d] %s" % (p.returncode, p.stderr.decode("utf-8", "replace").strip()[:200])
    return out


def push_binary():
    """传成品并比对 sha256：上一轮就是靠"尺寸一样"抓到过拿错文件的事。"""
    if not os.path.isfile(BIN_LOCAL):
        sys.exit("先跑 .tools/rebuild.sh 生成 .tmp_bin/dstpanel-linux")
    local = hashlib.sha256(open(BIN_LOCAL, "rb").read()).hexdigest()
    subprocess.run(["ssh", "-o", "BatchMode=yes", HOST, "mkdir -p " + PROBE_DIR], check=True, timeout=60)
    r = subprocess.run(["scp", "-q", BIN_LOCAL, "%s:%s" % (HOST, BIN_REMOTE)],
                       capture_output=True, timeout=600)
    if r.returncode != 0:
        sys.exit("scp 失败：" + r.stderr.decode("utf-8", "replace")[:200])
    out = remote("chmod 755 %s\nsha256sum %s | cut -d' ' -f1\nfile -b %s | cut -c1-40\n"
                 % (BIN_REMOTE, BIN_REMOTE, BIN_REMOTE))
    got = out.split("\n")[0].strip()
    check("云机上的成品和本地一致", got == local, "远端 %s / 本地 %s" % (got[:16], local[:16]))
    return out.strip().replace("\n", "  |  ")


def config_yaml(dst_install, key, pw):
    return """# 云机探针专用，跑完就删。
server:
  bind: 127.0.0.1
  port: %d
  adminUser: %s
  adminPassword: "%s"
paths:
  kleiRoot: "%s/klei"
  dstInstall: "%s"
  steamCmd: ""
  workDir: "%s"
network:
  shardPortBase: 10998
  shardPortEnd: 11900
  masterIntentPortBase: 11000
backup:
  keep: 3
helper:
  pollMs: 1000
  staleSeconds: 15
steamApi:
  webApiKey: "%s"
  proxy: ""
""" % (PORT, ADMIN, pw, PROBE_DIR, dst_install, PROBE_DIR, key)


class Panel:
    """一次起停 + 若干次接口调用。密码在各段之间保持不变，好让调用侧简单。"""

    def __init__(self, pw):
        self.pw = pw
        self.pid = ""

    def start(self, cfg_name, dst_install, key):
        cfg = config_yaml(dst_install, key, self.pw)
        if "CFGEOF" in cfg:
            sys.exit("配置里出现了 heredoc 分隔符，拒绝拼脚本")
        script = """set -u
umask 077
mkdir -p %(dir)s
cat > %(dir)s/%(name)s <<'CFGEOF'
%(cfg)s
CFGEOF
chmod 600 %(dir)s/%(name)s
nohup %(bin)s -config %(dir)s/%(name)s > %(dir)s/%(name)s.log 2>&1 &
echo "PID=$!"
up=0
for i in $(seq 1 60); do
  if curl -s -m 2 -o /dev/null http://127.0.0.1:%(port)s/healthz; then up=1; break; fi
  sleep 0.5
done
echo "UP=$up"
""" % {"dir": PROBE_DIR, "name": cfg_name, "cfg": cfg.rstrip("\n"),
       "bin": BIN_REMOTE, "port": PORT}
        out = remote(script)
        up = "0"
        for line in out.splitlines():
            if line.startswith("PID="):
                self.pid = line[4:].strip()
            if line.startswith("UP="):
                up = line[3:].strip()
        ok = up == "1"
        check("%s：Linux 成品起得来" % cfg_name, ok, out.replace("\n", " ")[:260])
        if not ok:
            check("%s 的启动日志" % cfg_name, False, self.log_tail())
        return ok

    def log_tail(self):
        return remote("tail -8 %s/cfg_*.log 2>/dev/null | tr '\\n' ' '" % PROBE_DIR)[:400]

    def get(self, path, params, user=None, pw=None):
        args = " ".join('--data-urlencode "%s"' % kv for kv in params)
        script = ('curl -s -m 45 --get -u "%s:%s" "http://127.0.0.1:%d%s" %s '
                  "-w '\\nHTTP=%%{http_code}\\n'\n") % (user or ADMIN, pw or self.pw,
                                                        PORT, path, args)
        raw = remote(script, timeout=90)
        body, _, tail = raw.rpartition("HTTP=")
        code = tail.strip()
        try:
            return int(code), json.loads(body.strip())
        except Exception:  # noqa: BLE001
            return (int(code) if code.isdigit() else -1), {"raw": body.strip()[:400]}

    def stop(self):
        if self.pid:
            remote("kill %s 2>/dev/null; sleep 1; kill -9 %s 2>/dev/null; true\n" % (self.pid, self.pid))
            self.pid = ""
        remote("rm -f %s/cfg_*.yaml %s/cfg_*.log\n" % (PROBE_DIR, PROBE_DIR))


def fresh_db():
    remote("rm -f %s/dstpanel.db %s/dstpanel.db-wal %s/dstpanel.db-shm\n"
           % (PROBE_DIR, PROBE_DIR, PROBE_DIR))


def phase_real_install(p):
    """段 R：只读借真安装目录。这一段不需要 key，任何时候都要跑。"""
    fresh_db()
    if not p.start("cfg_real.yaml", DST_REAL, ""):
        return
    code, r = p.get("/api/v1/mods", [])
    check("Linux 上读得动真安装目录", code == 200, str(r)[:200])

    code, r = p.get("/api/v1/clusters", [])
    check("没有 ~/.klei 时列集群不炸", code == 200 and isinstance(r.get("data"), list), str(r)[:160])

    code, r = p.get("/api/v1/mods/search", ["q=stack"])
    v = r.get("data") or {}
    check("没配 key 时在线开关为假且不给 500", code == 200 and v.get("onlineEnabled") is False
          and v.get("live") is False, "code=%s online=%s" % (code, v.get("onlineEnabled")))
    note = v.get("note") or ""
    check("降级说明是中文而不是 Go 串", any(ord(c) > 127 for c in note) and "webApiKey" in note,
          note[:160])
    # 这里不敢断言"一定搜不到东西"：真安装目录里万一有模组，磁盘入库本来就该给结果。
    # 能钉住的是——没有一条冒充在线结果，也没有一条冒充 Steam 来的。
    check("降级时没有条目冒充在线结果",
          all((i.get("source") or "") != "live" for i in (v.get("items") or [])),
          json.dumps([i.get("source") for i in (v.get("items") or [])], ensure_ascii=False)[:120])

    # /api/v1/stats 的契约是"必须点明看哪个集群"：没带 cluster 就该 400 说人话，
    # 而不是返回一份看起来像全服的 0 值统计。
    code, r = p.get("/api/v1/stats", [])
    check("统计接口缺 cluster 时 400 且说中文", code == 400
          and any(ord(c) > 127 for c in json.dumps(r, ensure_ascii=False)), str(r)[:160])
    code, r = p.get("/api/v1/task-kinds", [])
    check("任务类型表读得动（embed 的建表脚本没漏）", code == 200 and len(r.get("data") or []) > 0,
          str(r)[:160])

    # 这份配置里 steamCmd 故意留空：提交本体更新必须被当场拦住，而不是排队到 worker 里再失败。
    script = ('curl -s -m 20 -u "%s:%s" -X POST -H "Content-Type: application/json" '
              "-d '{\"kind\":\"server.update\"}' -w '\\nHTTP=%%{http_code}\\n' "
              '"http://127.0.0.1:%d/api/v1/tasks"\n') % (ADMIN, p.pw, PORT)
    raw = remote(script)
    body, _, tail = raw.rpartition("HTTP=")
    check("没配 steamCmd 时更新任务被当场拦住", tail.strip() == "400"
          and any(ord(c) > 127 for c in body), raw[:200])

    code, r = p.get("/api/v1/mods/search", ["q=stack", "size=99"], user=ADMIN, pw="wrong-pw")
    check("错密码在 Linux 上也是 401", code == 401, str(code))
    p.stop()


def phase_disk_cache(p):
    """段 D：磁盘扫到就入库，然后把文件删掉再搜——还能搜到才证明是真入库。"""
    fresh_db()
    wid = "350811795"
    # 清单文件刻意写成 CRLF：Klei 自带模板就是 \r\n 的（真机上 cat -A 看得见），
    # 用户在 Windows 上手工加一行也很常见。解析器不认 CRLF 的话，"已登记"就永远为假。
    remote("""set -u
rm -rf %(fake)s
mkdir -p %(fake)s/mods/workshop-%(wid)s
cat > %(fake)s/mods/workshop-%(wid)s/modinfo.lua <<'MEOF'
name = "几何防御 Geometric Defense"
version = "1.4"
api_version = 10
description = "把建筑叠起来"
MEOF
printf 'ServerModSetup("%(wid)s")\\r\\n' > %(fake)s/mods/dedicated_server_mods_setup.lua
cat -A %(fake)s/mods/dedicated_server_mods_setup.lua
""" % {"fake": DST_FAKE, "wid": wid})

    if not p.start("cfg_disk.yaml", DST_FAKE, ""):
        return
    code, r = p.get("/api/v1/mods", [])
    d = r.get("data") or {}
    rows = d.get("mods") or []
    check("假安装目录列得出模组", code == 200 and any(x.get("workshopId") == wid for x in rows),
          json.dumps(rows, ensure_ascii=False)[:200])
    if rows:
        check("CRLF 写的清单也算已登记", rows[0].get("inSetupList") is True,
              "inSetupList=%s" % rows[0].get("inSetupList"))

    code, r = p.get("/api/v1/mods/search", ["q=几何"])
    v = r.get("data") or {}
    items = v.get("items") or []
    check("Linux 上按中文名搜得到磁盘模组", code == 200 and len(items) == 1
          and items[0].get("workshopId") == wid and items[0].get("downloaded") is True
          and items[0].get("registered") is True, json.dumps(items, ensure_ascii=False)[:240])
    if items:
        check("版本从 modinfo.lua 里带出来", items[0].get("version") == "1.4", items[0].get("version"))
        check("来源标注区分磁盘与缓存", "这台机器" in (items[0].get("sourceLabel") or ""),
              items[0].get("sourceLabel"))

    # 通配符必须当字面量：LIKE 没转义的话 % 会匹配一切，这条是黑盒防线。
    # 这里传的是裸的 %，curl --data-urlencode 会自己编码成 %25。
    code, r = p.get("/api/v1/mods/search", ["q=%"])
    v = r.get("data") or {}
    check("搜百分号不会把所有模组都捞出来", code == 200 and (v.get("items") or []) == [],
          json.dumps(v.get("items"), ensure_ascii=False)[:160])

    code, r = p.get("/api/v1/mods/search", ["q=stack", "size=99"])
    check("size 越界在 Linux 上也是 400", code == 400, str(r)[:120])

    # 关键一步：文件删了还能搜到，才叫"见过即入库"，而不是每次现扫盘。
    # 清单行还留着，所以这时候的诚实状态是"已登记、等重启时由引擎下载"——
    # 这正是登记与下载分离这条设计要给出的那句话。
    remote("rm -rf %s/mods/workshop-%s\n" % (DST_FAKE, wid))
    code, r = p.get("/api/v1/mods/search", ["q=几何"])
    v = r.get("data") or {}
    items = v.get("items") or []
    check("删掉文件后仍按名字搜得到（缓存真落库）", code == 200 and len(items) == 1
          and items[0].get("source") == "cache" and items[0].get("downloaded") is False,
          json.dumps(items, ensure_ascii=False)[:240])
    if items:
        check("登记过但还没下载时说清是等重启", items[0].get("registered") is True
              and "重启" in (items[0].get("statusLabel") or ""), items[0].get("statusLabel"))
    check("缓存条数进了响应", (v.get("cacheCount") or 0) > 0, v.get("cacheCount"))

    # 换个进程重开：库里的东西必须跨进程活着。
    p.stop()
    if p.start("cfg_disk2.yaml", DST_FAKE, ""):
        code, r = p.get("/api/v1/mods/search", ["q=几何"])
        v = r.get("data") or {}
        check("重启面板后缓存还在", code == 200 and len(v.get("items") or []) == 1,
              json.dumps(v.get("items"), ensure_ascii=False)[:160])
    p.stop()


def phase_online(p, key):
    """段 L：真接口成功路径。只有本机出得去的那台机器 + 真 key 才能跑。"""
    fresh_db()
    if not p.start("cfg_live.yaml", DST_FAKE, key):
        return False
    code, r = p.get("/api/v1/mods/search", ["q=stack"])
    v = r.get("data") or {}
    items = v.get("items") or []
    check("有 key 时 onlineEnabled 为真", code == 200 and v.get("onlineEnabled") is True,
          "code=%s %s" % (code, str(r)[:160]))
    check("在线检索成功并给出总数", v.get("live") is True and (v.get("total") or 0) > 0,
          "total=%s 条数=%d" % (v.get("total"), len(items)))
    check("在线返回条目且名字非空", len(items) > 0 and all(i.get("name") for i in items),
          json.dumps([i.get("name") for i in items[:4]], ensure_ascii=False))
    if items:
        check("来源标成 Steam 在线检索", "Steam" in (items[0].get("sourceLabel") or ""),
              items[0].get("sourceLabel"))
        check("订阅数从接口带回来了", (items[0].get("subscriptions") or 0) > 0,
              items[0].get("subscriptions"))
        check("预览图是接口给的真实域名", "steamusercontent" in (items[0].get("previewUrl") or ""),
              (items[0].get("previewUrl") or "")[:70])
    check("响应里没有 key 值", key not in json.dumps(r, ensure_ascii=False), str(code))

    code, r = p.get("/api/v1/mods/search", ["q=stack", "size=99"])
    check("size 越界在线路径也 400", code == 400, str(r)[:120])

    # 中文搜索词：带 language=6 时接口直接回中文名，面板不自己翻译。
    code, r = p.get("/api/v1/mods/search", ["q=堆叠"])
    v = r.get("data") or {}
    items = v.get("items") or []
    zh = [i.get("name", "") for i in items if any(ord(c) > 127 for c in i.get("name", ""))]
    check("中文搜索词搜得到中文标题", code == 200 and len(items) > 0 and len(zh) > 0,
          json.dumps([i.get("name") for i in items[:3]], ensure_ascii=False))

    # 纯数字 ID 走精确查询：用户手里往往只有一串抄来的数字。
    code, r = p.get("/api/v1/mods/search", ["q=350811795"])
    v = r.get("data") or {}
    items = v.get("items") or []
    check("按工坊 ID 精确查得到名字", code == 200 and len(items) == 1
          and items[0].get("workshopId") == "350811795" and items[0].get("name"),
          json.dumps(items, ensure_ascii=False)[:220])

    # 不存在的 ID：接口活着但查不到，这必须和"没配 key"分开说。
    code, r = p.get("/api/v1/mods/search", ["q=4000000001"])
    v = r.get("data") or {}
    check("查不到的 ID 说清是下架或抄错", code == 200 and v.get("live") is True
          and any(w in (v.get("liveProblem") or "") for w in ("抄错", "下架")),
          (v.get("liveProblem") or "")[:140])
    p.stop()
    return True


def phase_cache_only(p):
    """段 C：无 key 只翻缓存——上一段在线搜过的词必须还能搜到。"""
    if not p.start("cfg_nokey.yaml", DST_FAKE, ""):
        return
    code, r = p.get("/api/v1/mods/search", ["q=stack"])
    v = r.get("data") or {}
    items = v.get("items") or []
    check("无 key 时不再打接口", code == 200 and v.get("onlineEnabled") is False
          and v.get("live") is False, "code=%s online=%s" % (code, v.get("onlineEnabled")))
    check("上一阶段见过的词靠缓存命中", len(items) > 0 and items[0].get("source") == "cache",
          json.dumps([i.get("name") for i in items[:3]], ensure_ascii=False))
    check("缓存条数进了响应", (v.get("cacheCount") or 0) > 0, v.get("cacheCount"))
    code, r = p.get("/api/v1/mods/search", ["q=几何防御"])
    v2 = r.get("data") or {}
    check("中文标题也进了缓存", any(i.get("source") == "cache" for i in (v2.get("items") or [])),
          json.dumps(v2.get("items"), ensure_ascii=False)[:200])
    p.stop()


def phase_bad_key(p):
    """段 B：错 key 必须是被 Steam 拒绝的中文结论 + 退回缓存，而不是 500。"""
    if not p.start("cfg_bad.yaml", DST_FAKE, BAD_KEY):
        return
    code, r = p.get("/api/v1/mods/search", ["q=stack"])
    v = r.get("data") or {}
    body = json.dumps(r, ensure_ascii=False)
    problem = v.get("liveProblem") or ""
    check("错 key 不给 500", code == 200 and BAD_KEY not in body, str(code))
    check("真 403 的结论指向换 key", v.get("live") is False
          and ("403" in problem or "key" in problem), problem[:180])
    check("被 Steam 拒了仍给缓存结果", len(v.get("items") or []) > 0, len(v.get("items") or []))
    check("降级说明是中文而不是 Go 串", any(ord(c) > 127 for c in (v.get("note") or "")),
          (v.get("note") or "")[:180])
    p.stop()


def main():
    key = os.environ.get("DST_STEAM_KEY", "").strip()
    if key and not KEY_RE.match(key):
        sys.exit("DST_STEAM_KEY 不是 16~64 位十六进制，像是被改坏了，探针拒绝把它发出去")
    if not key:
        skip("在线三段（真接口成功 / 无 key 翻缓存 / 真 403）",
             "没设 DST_STEAM_KEY。key 不落盘，只从环境读：DST_STEAM_KEY=$(cat 你的key文件) python .tools/cloud_search_probe.py")

    print("上传成品：" + push_binary())
    pw = secrets.token_hex(12)
    p = Panel(pw)
    try:
        phase_real_install(p)
        phase_disk_cache(p)
        if key:
            if phase_online(p, key):
                phase_cache_only(p)
            phase_bad_key(p)
    finally:
        out = remote("rm -f %s/cfg_*.yaml %s/cfg_*.log\nls %s | tr '\\n' ' '\n" % (PROBE_DIR, PROBE_DIR, PROBE_DIR))
        print("\n远端清理后剩余：" + out.strip()[:200])

    bad = [n for n, ok, _ in results if not ok]
    print("\n合计 %d 项，失败 %d 项，跳过 %d 项" % (len(results), len(bad), len(skipped)))
    if bad:
        print("失败：" + "、".join(bad))
    return 1 if bad else 0


if __name__ == "__main__":
    t0 = time.time()
    rc = main()
    print("耗时 %.1fs" % (time.time() - t0))
    sys.exit(rc)
