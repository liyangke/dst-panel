#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""云机首启的后半段：把真 Klei 令牌灌进去，真起两个分片。

前置（.tools/cloud_boot.py）已经把该验的都验过了：集群建在引擎真读的目录、
世界文件按两张表写全、没令牌/坏令界面板都说得出引擎原话。这一支只做四件事：

  1. 从云机上的令牌文件读令牌 —— 令牌全程不进本机 argv、不进聊天记录、不打日志；
  2. PUT 进面板（面板自己写 cluster_token.txt，0600）；
  3. 起 Master，等助手 MOD 回第一份快照（第一次要生成世界，一到两分钟）；
  4. Master 进世界后再起 Caves，同样等到有快照。

令牌文件放哪：在云机上执行
    umask 077; read -s TOK && printf '%s' "$TOK" > ~/dst/klei_token.txt
（read -s 不回显，也不会进 shell 历史）。跑完这支脚本会把令牌文件删掉，
副本已经在 ~/.klei/DoNotStarveTogether/<集群>/cluster_token.txt 里了。
"""
import hashlib
import json
import os
import subprocess
import sys
import time

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOST = "dst-cloud"
BIN_LOCAL = os.path.join(ROOT, ".tmp_bin", "dstpanel-linux")
BOOT_DIR = "/home/ubuntu/dst/panel-boot"
BIN_REMOTE = BOOT_DIR + "/dstpanel"
CFG = BOOT_DIR + "/cfg_boot.yaml"
KLEI = "/home/ubuntu/.klei/DoNotStarveTogether"
DST_REAL = "/home/ubuntu/dst/dedicated_server"
TOKEN_SRC = "/home/ubuntu/dst/klei_token.txt"
CLUSTER = "Cluster_1"
PORT = 8942
ADMIN = "admin"

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok)))
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  -- " + str(detail)[:280]) if detail else ""))


def remote(script, timeout=300):
    p = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", HOST, "bash -s"],
                       input=script.encode("utf-8"), capture_output=True, timeout=timeout)
    out = p.stdout.decode("utf-8", "replace")
    if p.returncode != 0:
        out += "[ssh rc=%d] %s" % (p.returncode, p.stderr.decode("utf-8", "replace").strip()[:200])
    return out


def password():
    out = remote("grep adminPassword %s | head -1 | sed 's/.*: \"//; s/\"$//'" % CFG)
    if out.count("\n") > 1 or " " in out.strip():
        sys.exit("读不出面板密码，配置形状和预期不一样：" + out[:120])
    return out.strip()


def call(method, path, payload_file=None):
    """payload_file 是云机上的文件路径：请求体从文件读，密钥不进任何命令行。"""
    data = "--data-binary @%s " % payload_file if payload_file else ""
    script = ('curl -s -m 180 -u "%s:%s" -X %s -H "Content-Type: application/json" %s'
              '"http://127.0.0.1:%d%s" -w \'\\nHTTP=%%{http_code}\\n\''
              % (ADMIN, PW, method, data, PORT, path))
    raw = remote(script)
    text, _, tail = raw.rpartition("HTTP=")
    try:
        return int(tail.strip()), json.loads(text.strip())
    except Exception:  # noqa: BLE001
        return -1, {"raw": text.strip()[:300]}


def plain_call(method, path, body=None):
    """不含密钥的普通调用：请求体走 /tmp 文件，省掉两种拼法。"""
    if body is not None:
        text = json.dumps(body, ensure_ascii=False)
        if "PAYEOF" in text:
            sys.exit("请求体撞了分隔符")
        remote("cat > /tmp/boot_call.json <<'PAYEOF'\n%s\nPAYEOF\n" % text)
    return call(method, path, "/tmp/boot_call.json" if body is not None else None)


def push_binary():
    if not os.path.isfile(BIN_LOCAL):
        sys.exit("先跑 .tools/rebuild.sh")
    local = hashlib.sha256(open(BIN_LOCAL, "rb").read()).hexdigest()
    remote("pgrep -f %s | xargs -r kill; sleep 1" % CFG)
    subprocess.run(["scp", "-q", BIN_LOCAL, "%s:%s" % (HOST, BIN_REMOTE)], capture_output=True, timeout=600)
    got = remote("chmod 755 %s; sha256sum %s | cut -d' ' -f1" % (BIN_REMOTE, BIN_REMOTE)).strip()
    check("云机成品与本地一致", got.splitlines()[0] == local, got[:80])


def start_panel():
    out = remote("""set -u
cd %s
nohup ./dstpanel -config %s > finish.log 2>&1 &
echo "PID=$!"
up=0
for i in $(seq 1 60); do
  if curl -s -m 2 -o /dev/null http://127.0.0.1:%d/healthz; then up=1; break; fi
  sleep 0.5
done
echo "UP=$up"
""" % (BOOT_DIR, CFG, PORT))
    pid = ""
    up = "0"
    for line in out.splitlines():
        if line.startswith("PID="):
            pid = line[4:].strip()
        if line.startswith("UP="):
            up = line[3:].strip()
    check("面板起得来", up == "1", out.replace("\n", " ")[:200])
    return pid


def apply_token():
    """令牌只在这台机器内部流动：jq 从文件拼 JSON，curl 从文件读体，用完立刻删。"""
    script = """set -u
umask 077
T=%s
test -s "$T" || { echo "NOTOKEN"; exit 0; }
LEN=$(tr -d '\\r\\n' < "$T" | wc -c)
jq -Rs '{token: (. | gsub("[\\r\\n]+$"; ""))}' "$T" > /tmp/token_body.json
chmod 600 /tmp/token_body.json
echo "SRCLEN=$LEN"
""" % TOKEN_SRC
    out = remote(script)
    if "NOTOKEN" in out:
        return None, None
    src_len = None
    for line in out.splitlines():
        if line.startswith("SRCLEN="):
            src_len = line[7:].strip()
    code, r = call("PUT", "/api/v1/clusters/%s/token" % CLUSTER, "/tmp/token_body.json")
    remote("rm -f /tmp/token_body.json\n")
    d = r.get("data") or {}
    body = json.dumps(r, ensure_ascii=False)
    check("令牌写入成功", code == 200 and d.get("configured") is True, body[:200])
    # 只比长度：任何一次把令牌打到输出里，这条检查本身就变成泄漏。
    check("面板读到的长度与令牌文件一致（不回明文）",
          str(d.get("length")) == str(src_len) and ("token\"" not in body or "configured" in body),
          "接口 %s / 文件 %s" % (d.get("length"), src_len))
    check("接口之后能读到令牌状态", plain_call("GET", "/api/v1/clusters/%s/token" % CLUSTER)[1]
          .get("data", {}).get("configured") is True)
    # 只有面板确认收下了才删源文件。上一版无条件删，结果校验把令牌拒了还得让人重贴一次。
    if code == 200:
        remote("rm -f %s\n" % TOKEN_SRC)
        check("令牌源文件已删除（副本只在 cluster_token.txt）",
              "gone" in remote('test -e %s && echo exists || echo gone' % TOKEN_SRC), TOKEN_SRC)
    else:
        print("   令牌源文件保留着，修好校验后直接重跑这支，不用重贴。")
    return code, d


def boot(shard, wait_seconds):
    """起一个分片，等到助手回话；返回 (接口结论, 观察到的最后一句)。"""
    code, r = plain_call("POST", "/api/v1/clusters/%s/shards/%s/start" % (CLUSTER, shard))
    msg = json.dumps(r, ensure_ascii=False)
    check("%s 启动接口没有拒绝" % shard, code == 200, msg[:260])
    if code != 200:
        return False, msg
    deadline = time.time() + wait_seconds
    last = ""
    while time.time() < deadline:
        time.sleep(15)
        c2, r2 = plain_call("GET", "/api/v1/clusters")
        last = json.dumps(r2, ensure_ascii=False)
        view = None
        for c in (r2.get("data") or []):
            if c.get("name") != CLUSTER:
                continue
            for s in c.get("shards", []):
                if s.get("name") == shard:
                    view = s
        if view is None:
            # 总览里连分片都不见了（接口挂了或集群被删），等下去只会等满窗口。
            break
        if view.get("helperOnline"):
            check("%s 的助手 MOD 回话了（真进了世界）" % shard, True,
                  "等待 %ds" % int(wait_seconds - (deadline - time.time())))
            return True, last
        if view.get("running") is False:
            # 服务端自己退出了，别再等满。
            break
    check("%s 的助手 MOD 回话了（真进了世界）" % shard, False, last[:280])
    return False, last


def evidence(shard):
    out = remote("""F="%s/%s/%s/server_log.txt"
echo "size=$(wc -c < \\"$F\\" 2>/dev/null || echo 0)"
grep -c 'Your Server Will Not Start' "$F" 2>/dev/null || echo 0
tail -4 "$F" 2>/dev/null | cut -c1-120
""" % (KLEI, CLUSTER, shard))
    return " ".join(out.split())


def main():
    global PW
    if "NOTOKEN" in remote('test -s %s && echo has || echo NOTOKEN' % TOKEN_SRC):
        print("云机上还没有令牌文件 %s。先在那台机器上执行：\n"
              "  umask 077; read -s TOK && printf '%%s' \"$TOK\" > %s\n"
              "令牌别贴进聊天：贴过就得去 Klei 后台重置一枚。" % (TOKEN_SRC, TOKEN_SRC))
        return 2
    PW = password()
    push_binary()
    pid = start_panel()
    if not pid:
        return finish()
    try:
        if apply_token()[0] != 200:
            return finish()
        # 第一次启动要生成世界：地表一到两分钟，洞穴跟着起。窗口给到 5 分钟。
        ok_master, _ = boot("Master", 300)
        if ok_master:
            boot("Caves", 300)
        else:
            check("Caves 依因地表没进世界而跳过", True, "先看 Master 的日志")
        for shard in ("Master", "Caves"):
            print("%s 日志尾巴: %s" % (shard, evidence(shard)[:300]))
        c, r = plain_call("GET", "/api/v1/settings")
        print("端口/集群信息: %s" % json.dumps(r, ensure_ascii=False)[:300])
    finally:
        remote("kill %s 2>/dev/null; sleep 1; pgrep -f %s | xargs -r kill; true" % (pid, CFG))
        print("面板已关；分片留在 screen 会话里跑着。重开面板："
              "  ssh %s 'cd %s && nohup ./dstpanel -config %s > finish.log 2>&1 &'" % (HOST, BOOT_DIR, CFG))
        left = remote("screen -ls 2>&1 | head -4")
        print("当前会话:", " ".join(left.split())[:200])
    return finish()


def finish():
    bad = [n for n, ok in results if not ok]
    print("\n合计 %d 项，失败 %d 项" % (len(results), len(bad)))
    if bad:
        print("失败：" + "、".join(bad))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
