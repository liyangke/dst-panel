#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""把在线检索用的 Steam Web API key 配到云服务器上那套面板里，并当场验一次在线搜索。

为什么要专门写一支脚本而不是 ssh 上去手工 sed：这一支要在别人的机器上改一个含凭据的文件。
三件事必须同时成立——key 不落在本机 argv/日志里、改完的配置权限没走样、改坏了能退回原样。

key 的来源和本地开发用的是同一份（.tmp_boot/steam_key.txt 或环境变量 DST_STEAM_KEY），
形状先过一遍校验：读错文件把无关字符串当 key 写进生产配置，症状是每次搜索都 403，
比写不进去更难查。

改动只碰 steamApi.webApiKey 这一行；改之前先留一份不含 key 的备份（改之前的值就是空串，
所以备份里没有凭据，敢落盘）。备份文件同样 0600，别让同目录里多出一个能读的东西。
"""
import io
import json
import os
import re
import subprocess
import sys
import time

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOST = "dst-cloud"
KEYFILE = os.path.join(ROOT, ".tmp_boot", "steam_key.txt")
DST_USER = "dst"
CONF = "/data/dst/panel/cfg.yaml"
PORT = 8942
ADMIN_USER = "admin"
ADMIN_PW = "admin"
KEY_RE = re.compile(r"^[0-9A-Fa-f]{16,64}$")

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok)))
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  -- " + str(detail)[:280]) if detail else ""))


def remote(script, timeout=180):
    """整段 bash 走 stdin：key 和密码都不进本机 argv，远端 ps 里也看不到。"""
    p = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o "
                        "ConnectTimeout=10", HOST, "bash -s"],
                       input=script.encode("utf-8"), capture_output=True, timeout=timeout)
    out = p.stdout.decode("utf-8", "replace")
    if p.returncode != 0:
        out += "\n[ssh rc=%d] %s" % (p.returncode, p.stderr.decode("utf-8", "replace").strip()[:200])
    return out


def local_key():
    k = os.environ.get("DST_STEAM_KEY", "").strip()
    if not k and os.path.exists(KEYFILE):
        k = io.open(KEYFILE, encoding="utf-8").read().strip()
    if not KEY_RE.match(k):
        sys.exit("拿不到可用的 key（要 16~64 位十六进制）。key 只从环境变量 DST_STEAM_KEY "
                 "或 %s 读，脚本不会猜。" % KEYFILE)
    return k


def read_state():
    """只看当前那一行的形状，绝不打印值本身。"""
    out = remote("""sudo -n python3 - <<'PYEOF'
import re
t=open("%(conf)s").read()
ms=re.findall(r"^[ \\t]*webApiKey:[ \\t]*(?P<v>[^\\r\\n]*)", t, re.M)
print("COUNT=%%d" %% len(ms))
for v in ms:
    s=v.strip().strip('"')
    print("LEN=%%d HEX=%%s EMPTY=%%s" %% (len(s), bool(re.fullmatch(r"[0-9a-fA-F]+", s)) and len(s)>0, len(s)==0))
print("STEAMAPI=%%s" %% ("steamApi:" in t))
print("MODE=%%o" %% (__import__("os").stat("%(conf)s").st_mode & 0o777))
print("OWNER=%%s" %% __import__("pwd").getpwuid(__import__("os").stat("%(conf)s").st_uid).pw_name)
PYEOF
""" % {"conf": CONF})
    st = {"count": -1, "len": -1, "hex": False, "empty": None, "mode": "", "owner": "", "steamapi": False}
    for line in out.splitlines():
        line = line.strip()
        if line.startswith("COUNT="):
            st["count"] = int(line[6:] or -1)
        elif line.startswith("LEN="):
            # LEN=32 HEX=True EMPTY=False
            parts = line.split()
            st["len"] = int(parts[0][4:])
            st["hex"] = parts[1].split("=", 1)[1] == "True"
            st["empty"] = parts[2].split("=", 1)[1] == "True"
        elif line.startswith("MODE="):
            st["mode"] = line[5:]
        elif line.startswith("OWNER="):
            st["owner"] = line[6:]
        elif line.startswith("STEAMAPI="):
            st["steamapi"] = line[9:] == "True"
    st["raw"] = out.strip().replace("\n", " | ")[:200]
    return st


def wait_health(sec=40):
    out = remote("""for i in $(seq 1 %d); do
  if curl -s -m 2 -o /dev/null http://127.0.0.1:%d/healthz; then echo UP=1; exit 0; fi
  sleep 1
done
echo UP=0
""" % (sec, PORT))
    return "UP=1" in out


def api_get(path, params):
    """带基本认证的 GET。密码走 curl --config 的 stdin，不进远端 argv；
    响应体和状态码分家（-o 写文件、-w 只打码），不然拼在一起的 JSON 没法可靠地切。"""
    q = " ".join('--data-urlencode "%s"' % re.sub(r'[`$"\']', "", kv) for kv in params)
    script = """printf 'user = "%(user)s:%(pw)s"\\n' | curl -s -m 60 --config - --get \\
  -o /tmp/dst_key_probe_body.json -w '%%{http_code}' \\
  "http://127.0.0.1:%(port)s%(path)s" %(q)s
echo
echo "HTTPCODE_DONE"
cat /tmp/dst_key_probe_body.json 2>/dev/null
echo
echo "HTTPCODE_DONE"
rm -f /tmp/dst_key_probe_body.json
""" % {"user": ADMIN_USER, "pw": ADMIN_PW, "port": PORT, "path": path, "q": q}
    out = remote(script, timeout=120)
    parts = out.split("HTTPCODE_DONE")
    if len(parts) < 3:
        return -1, {"raw": out.strip()[:400]}
    code = parts[0].strip().splitlines()[-1].strip() if parts[0].strip() else ""
    body = parts[1].strip()
    try:
        return int(code), json.loads(body)
    except Exception:  # noqa: BLE001
        return (int(code) if code.isdigit() else -1), {"raw": body[:400]}


def main():
    key = local_key()

    # 0) 这台云机出不出得去 steam：出不去的话配了 key 也搜不到，别把网络的锅算成 key 的锅。
    out = remote('curl -s -o /dev/null -m 12 -w "CODE=%{http_code}\\n" '
                 '"https://api.steampowered.com/ISteamWebAPIUtil/GetSupportedAPIList/v0001/" '
                 '|| echo CODE=FAIL')
    reachable = "CODE=200" in out or "CODE=403" in out
    check("云机连得上 api.steampowered.com", reachable, out.strip()[:120])

    before = read_state()
    print("改前状态（只有长度与形状，没有值）：" + before["raw"])
    check("配置里有 steamApi 段", before["steamapi"], before["raw"])
    check("webApiKey 只有一行（多处就不知道该改哪个）", before["count"] == 1, str(before["count"]))
    if before["count"] != 1:
        return 1
    already = before["empty"] is False and before["hex"] and before["len"] == len(key)
    if not before["empty"]:
        print("注意：云上已经有一个非空 key（长度 %d）。按“和本地一样”的要求整行替换；"
              "这种情况下不另存备份，免得磁盘上多出一把能读出来的钥匙。" % before["len"])

    # 1) 备份 + 改这一行。
    #    只在改之前那一行是空串时才落备份文件：那种情况下备份不含凭据，敢多存一份；
    #    要是原来就有 key，备份等于在磁盘上多复制一把钥匙，宁可不做这份备份。
    #    key 拼进 heredoc 里的 python 源码（走 stdin 送给 bash），既不进本机 argv 也不进远端 argv。
    BAK = CONF + ".bak-before-steamkey"
    # 已有 key 时不去覆盖那份旧备份（覆盖等于把钥匙多复制一份）；
    # 真留下过一份含 key 的旧备份，下面那道全目录扫描会把它报出来，比现在悄悄 rm 掉好。
    pre = "sudo -n install -o %(user)s -g %(user)s -m 600 %(conf)s %(bak)s" % {
        "user": DST_USER, "conf": CONF, "bak": BAK} if before["empty"] else "true"
    script = """set -eu
umask 077
%(pre)s
sudo -n python3 - <<'PYEOF'
import io, os, re
conf = "%(conf)s"
key = "%(key)s"
t = io.open(conf, encoding="utf-8").read()
# 替换串用 lambda 而不是格式化拼：整段脚本本身要过一次百分号格式化，
# 里层再出现裸百分号就得写成四个连着的，写错一个的后果是把 key 拼没了、
# 配置里留下一行字面量占位符——这种坏法面板照样起得来，只有搜的时候才暴露。
new, n = re.subn(r'(?m)^([ \\t]*webApiKey:[ \\t]*)[^\\r\\n]*$',
                 lambda mo: mo.group(1) + '"' + key + '"', t, count=1)
if n != 1:
    raise SystemExit("没找到 webApiKey 那一行，什么都不改")
d = os.path.dirname(conf)
tmp = os.path.join(d, ".cfg.yaml.tmp")
with io.open(tmp, "w", encoding="utf-8", newline="") as f:
    f.write(new)
os.chmod(tmp, 0o600)
st = os.stat(conf)
os.chown(tmp, st.st_uid, st.st_gid)
os.replace(tmp, conf)
print("WRITTEN=1")
PYEOF
sudo -n chown %(user)s:%(user)s %(conf)s
sudo -n chmod 600 %(conf)s
[ -f %(bak)s ] && sudo -n chown %(user)s:%(user)s %(bak)s && sudo -n chmod 600 %(bak)s || true
stat -c "AFTER_MODE=%%a AFTER_OWNER=%%U" %(conf)s
echo DONE=1
""" % {"conf": CONF, "user": DST_USER, "key": key, "bak": BAK, "pre": pre}
    if "PYEOF" in key or "CFG" in key:  # key 是十六进制，永远命中不了；这道检查是给模板留的保险
        sys.exit("key 里含脚本分隔符，拒绝拼脚本")
    out = remote(script)
    check("配置写入成功", "WRITTEN=1" in out and "DONE=1" in out, out.strip()[:260])

    after = read_state()
    print("改后状态：" + after["raw"])
    check("改后就是本地那把 key（长度与字符集一致）",
          after["len"] == len(key) and after["hex"] and not after["empty"],
          "len=%s hex=%s" % (after["len"], after["hex"]))
    check("配置权限还是 0600、属主还是服务用户",
          after["mode"] == "600" and after["owner"] == DST_USER,
          "mode=%s owner=%s" % (after["mode"], after["owner"]))

    # 2) 重启面板才读得到新配置（面板没有热重载 key 这回事，代码里 key 是建 client 时喂进去的）。
    if not already:
        out = remote("sudo -n systemctl restart dstpanel && echo RESTART=0 || echo RESTART=$?")
        check("面板已重启", "RESTART=0" in out, out.strip()[:160])
        check("重启后健康检查通过", wait_health(), "")

    # 3) key 不许出现在任何日志或备份里。模式走 grep -f - 的 stdin（printf 是 bash 内建，
    #    不会像 grep -- <key> 那样在远端 ps 里留一行）。
    out = remote("""printf '%%s\\n' "%(key)s" | sudo -n grep -rlF -f - %(dir)s 2>/dev/null | sed 's#^#HIT #'
echo SCAN_DONE=1
""" % {"key": key, "dir": os.path.dirname(CONF)})
    hits = [l for l in out.splitlines() if l.startswith("HIT ")]
    check("key 只出现在 cfg.yaml 一处", len(hits) == 1 and hits[0].endswith(CONF),
          ("命中：" + "，".join(h.replace("HIT ", "") for h in hits))[:200])

    # 4) 端到端：在云上真搜一次。
    code, r = api_get("/api/v1/mods/search", ["q=stack"])
    v = (r.get("data") or {}) if isinstance(r, dict) else {}
    check("云上搜索接口 200", code == 200, "code=%s %s" % (code, str(r)[:160]))
    check("在线开关为真（key 被面板读到了）", v.get("onlineEnabled") is True,
          "onlineEnabled=%s" % v.get("onlineEnabled"))
    items = v.get("items") or []
    check("在线检索真的回来了结果", v.get("live") is True and (v.get("total") or 0) > 0 and len(items) > 0,
          "live=%s total=%s 条数=%d problem=%s" % (v.get("live"), v.get("total"), len(items),
                                                 (v.get("liveProblem") or "")[:120]))
    if items:
        check("结果里有订阅数和封面（在线字段带回来了）",
              (items[0].get("subscriptions") or 0) > 0
              and "steamusercontent" in (items[0].get("previewUrl") or ""),
              "subs=%s cover=%s" % (items[0].get("subscriptions"),
                                    (items[0].get("previewUrl") or "")[:60]))
    check("响应里没有 key", key not in json.dumps(r, ensure_ascii=False), str(code))
    note = v.get("note") or ""
    check("界面那句话已经不说“没配 key”", "没配" not in note, note[:180])
    print("\n检索说明原文：" + note[:220])
    print("前 3 条：" + json.dumps([{"name": i.get("name"), "subs": i.get("subscriptions"),
                                    "src": i.get("source")} for i in items[:3]], ensure_ascii=False)[:300])
    return 0 if all(ok for _, ok in results) else 1


if __name__ == "__main__":
    t0 = time.time()
    rc = main()
    bad = [n for n, ok in results if not ok]
    print("\n合计 %d 项，失败 %d 项%s" % (len(results), len(bad),
                                          ("：" + "、".join(bad)) if bad else ""))
    print("耗时 %.1fs" % (time.time() - t0))
    sys.exit(rc)
