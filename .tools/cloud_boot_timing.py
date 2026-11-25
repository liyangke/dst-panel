#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""量一件事：真引擎从"会话出现"到"把拒绝启动写进 server_log.txt"隔了多久。

成品里 awaitBootVerdict 只盯 2 秒。本机单测里那点是够的（假日志一写就落盘），
真机不是：云机首启那一轮就是被这个差距打穿的——假令牌那次接口返回了"运行中"，
而几秒之后日志里明明白白写着 "!!!! Your Server Will Not Start !!!!"。
没有这个数，窗口时长就还是拍脑袋。
"""
import json
import subprocess
import sys
import time

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

HOST = "dst-cloud"
KLEI = "/home/ubuntu/.klei/DoNotStarveTogether"
CLUSTER = "Cluster_1"
BOOT_DIR = "/home/ubuntu/dst/panel-boot"
LOG = "%s/%s/Master/server_log.txt" % (KLEI, CLUSTER)
TOKEN = "%s/%s/cluster_token.txt" % (KLEI, CLUSTER)
CFG = BOOT_DIR + "/cfg_boot.yaml"
PORT = 8942
FAKE = "0123456789abcdef0123456789abcdef"


def remote(script, timeout=180):
    p = subprocess.run(["ssh", "-o", "BatchMode=yes", HOST, "bash -s"],
                       input=script.encode("utf-8"), capture_output=True, timeout=timeout)
    out = p.stdout.decode("utf-8", "replace")
    if p.returncode != 0:
        out += "[ssh rc=%d] %s" % (p.returncode, p.stderr.decode("utf-8", "replace").strip()[:160])
    return out


def password():
    out = remote("grep adminPassword %s | head -1 | sed 's/.*: \"//; s/\"$//'" % CFG)
    if out.count("\n") > 1 or " " in out.strip():
        sys.exit("读不出面板密码，配置形状和预期不一样：" + out[:160])
    return out.strip()


PW = password()


def call(method, path, payload=None):
    part = ""
    if payload is not None:
        body = json.dumps(payload, ensure_ascii=False)
        if "PAYEOF" in body:
            sys.exit("请求体撞了分隔符")
        part = "cat > /tmp/timing_payload.json <<'PAYEOF'\n%s\nPAYEOF\n" % body
        payload_flag = "--data-binary @/tmp/timing_payload.json "
    else:
        payload_flag = ""
    script = ('%s\n'
              'curl -s -m 120 -u "admin:%s" -X %s -H "Content-Type: application/json" %s'
              '"http://127.0.0.1:%d%s" -w \'\\nHTTP=%%{http_code}\\n\''
              % (part, PW, method, payload_flag, PORT, path))
    raw = remote(script)
    text, _, tail = raw.rpartition("HTTP=")
    try:
        return int(tail.strip()), json.loads(text.strip())
    except Exception:  # noqa: BLE001
        return -1, {"raw": text.strip()[:300]}


def log_state():
    """一次 ssh 往返里问齐：日志大小/写出时刻/拒绝指纹命中/会话在不在。"""
    out = remote("""F="%s"
if [ -f "$F" ]; then
  echo "size=$(wc -c < "$F") mtime=$(stat -c %%Y "$F") first=$(head -1 "$F" | cut -c1-24) refuse=$(grep -c 'Your Server Will Not Start' "$F")"
else
  echo "missing"
fi
echo "screens=$(screen -ls 2>/dev/null | grep -c dst-%s-Master)"
echo "sim=$(pgrep -c -f dontstarve_dedicated)"
""" % (LOG, CLUSTER))
    return " ".join(out.split())


def ensure_panel():
    """量的是真引擎，面板得先在跑；已经起着就沿用，不要重复起一份。"""
    if remote('curl -s -m 3 -o /dev/null -w "%%{http_code}" http://127.0.0.1:%d/healthz' % PORT).strip() == "200":
        print("面板已在跑，沿用")
        return True
    out = remote("""cd %(dir)s
nohup ./dstpanel -config %(cfg)s > timing_panel.log 2>&1 &
echo "PID=$!"
up=0
for i in $(seq 1 40); do
  if curl -s -m 2 -o /dev/null http://127.0.0.1:%(port)s/healthz; then up=1; break; fi
  sleep 0.5
done
echo "UP=$up"
""" % {"dir": BOOT_DIR, "cfg": CFG, "port": PORT})
    print("起面板:", " ".join(out.split())[:200])
    return "UP=1" in out


def stop_panel():
    """按配置名收尾：面板是相对路径起的，匹配二进制路径会漏（上一轮就是这么留下进程的）。"""
    out = remote("pkill -f %s; sleep 1; pgrep -af %s | grep -v pgrep || echo PANEL_GONE"
                 % (CFG_NAME, CFG_NAME))
    print("关面板:", " ".join(out.split())[:160])
    return "PANEL_GONE" in out


def main():
    if not ensure_panel():
        sys.exit("面板起不来，量不了")
    remote('rm -f "%s" "%s"\n' % (LOG, TOKEN))
    started = int(remote("date +%s").strip())
    print("基准时间 launched=%d" % started)

    t0 = time.time()
    code, r = call("PUT", "/api/v1/clusters/%s/token" % CLUSTER, {"token": FAKE})
    print("PUT 假令牌 -> %s" % code)
    code, r = call("POST", "/api/v1/clusters/%s/shards/Master/start" % CLUSTER)
    print("start 返回 code=%s 接口耗时 %.1fs :: %s"
          % (code, time.time() - t0, json.dumps(r, ensure_ascii=False)[:200]))

    first_seen = None
    for i in range(1, 31):
        time.sleep(1)
        state = log_state()
        if "refuse=1" in state and first_seen is None:
            first_seen = i
        print("T+%02ds %s" % (i, state[:200]))

    code, r = call("GET", "/api/v1/clusters/%s/shards/Master/status" % CLUSTER)
    print("status -> %s :: %s" % (code, json.dumps(r, ensure_ascii=False)[:240]))
    code, r = call("POST", "/api/v1/clusters/%s/shards/Master/kill" % CLUSTER)
    print("kill -> %s :: %s" % (code, json.dumps(r, ensure_ascii=False)[:160]))
    remote('rm -f "%s"\n' % TOKEN)
    print("清理:", " ".join(remote('ls "%s"; screen -ls 2>&1 | head -2' % (KLEI + "/" + CLUSTER)).split()))
    stop_panel()
    print("\n结论：拒绝指纹第一次可读大约在 start 之后 %s 秒；面板同步窗口目前只盯 2 秒。"
          % (first_seen if first_seen else "≥30（没在 30 秒内出现）"))


if __name__ == "__main__":
    main()
