#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""用面板真起两个分片：#13 的后半段，也是今天那个"引擎会重写世界文件"缺陷的现场验证。

前置状态（.tmp_boot/recon5.sh 勘察过）：
  - Cluster_1 已经在引擎真读的目录里，cluster_token.txt 0600 且能用；这一支不碰令牌；
  - Master 由我做 A/B 勘察的进程占着（screen wgab-wgc），必须先停，否则同一个存档
    被两个服务端打开；
  - Caves 还留着 leveldataoverride 时代那份手工文件，面板不读它但会提示冲突，
    这一支把它改名挪开（不删，随时能挪回来）；
  - Caves 没有 worldgenoverride.lua，也没有存档 → 起它等于第一次实测 DST_CAVE 生洞穴。

顺序：停勘察进程 → 挪手工文件 → 推成品 → 起面板 → 世界页读写（此刻 Master 磁盘上
是引擎重写过的 186 字节那一份）→ 起 Master 等助手回话 → 起 Caves 等助手回话 →
ping/announce 往返 → 收证据。分片跑着不关，面板关掉。
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
CLUSTER = "Cluster_1"
PORT = 8942
ADMIN = "admin"
SCRATCH = "wgab-wgc"
# 手工文件挪过去之后的名字：引擎只按 leveldataoverride.lua 这个文件名加载，改名即失效。
STASH_SUFFIX = ".stashed-by-panel"

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok)))
    print("[%s] %s%s" % ("PASS" if ok else "FAIL", name,
                         ("  -- " + str(detail)[:300]) if detail else ""))


def remote(script, timeout=420):
    p = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", HOST, "bash -s"],
                       input=script.encode("utf-8"), capture_output=True, timeout=timeout)
    out = p.stdout.decode("utf-8", "replace")
    if p.returncode != 0:
        out += "[ssh rc=%d] %s" % (p.returncode, p.stderr.decode("utf-8", "replace").strip()[:200])
    return out


def password():
    # 只取那一行的值；万一形状不对就停下，不把整份配置打出来（里面有密码）。
    out = remote("grep adminPassword %s | head -1 | sed 's/.*: \"//; s/\"$//'" % CFG)
    if out.count("\n") > 1 or " " in out.strip():
        sys.exit("读不出面板密码，配置形状和预期不一样：" + out[:120])
    return out.strip()


def call(method, path, payload_file=None):
    data = "--data-binary @%s " % payload_file if payload_file else ""
    script = ('curl -s -m 240 -u "%s:%s" -X %s -H "Content-Type: application/json" %s'
              '"http://127.0.0.1:%d%s" -w \'\\nHTTP=%%{http_code}\\n\''
              % (ADMIN, PW, method, data, PORT, path))
    raw = remote(script)
    text, _, tail = raw.rpartition("HTTP=")
    try:
        return int(tail.strip()), json.loads(text.strip())
    except Exception:  # noqa: BLE001
        return -1, {"raw": text.strip()[:400]}


def plain_call(method, path, body=None):
    if body is not None:
        text = json.dumps(body, ensure_ascii=False)
        if "PAYEOF" in text:
            sys.exit("请求体撞了分隔符")
        remote("cat > /tmp/shard_call.json <<'PAYEOF'\n%s\nPAYEOF\n" % text)
    return call(method, path, "/tmp/shard_call.json" if body is not None else None)


def stop_scratch():
    """只停我做 A/B 勘察起的那个进程（命令行里带 wgab）。

    上一版写的是 pgrep dedicated_server_nullrenderer 全杀：那会儿正式分片还没起，
    看起来没问题；现在 Cluster_1 两个分片正跑着，同样的脚本会把它们一起干掉，
    而且是不存档就杀——玩家掉线加世界回档。所以这里按勘察目录 wgab 精确匹配。
    """
    out = remote("""set -u
screen -ls 2>/dev/null | grep %s | head -2
screen -S %s -X quit 2>/dev/null; sleep 2
pgrep -af wgab | while read p rest; do
  case "$rest" in *nullrenderer*|*dedicated_server*) kill $p; echo "killed $p";; esac
done
sleep 4
echo "REMAIN=$(pgrep -af wgab | grep -c 'nullrenderer\\|dedicated_server')"
echo "SCREEN=$(screen -ls 2>&1 | grep -c wgab)"
echo "FORMAL=$(pgrep -c -f 'nullrenderer_x64 -console -cluster %s')"
echo "MEM=$(free -m | sed -n 2p)"
""" % (SCRATCH, SCRATCH, CLUSTER))
    remain, screens, formal = "?", "?", "?"
    for line in out.splitlines():
        if line.startswith("REMAIN="):
            remain = line[7:].strip()
        if line.startswith("SCREEN="):
            screens = line[7:].strip()
        if line.startswith("FORMAL="):
            formal = line[7:].strip()
    check("勘察用的服务端进程已停干净", remain == "0" and screens == "0",
          out.replace("\n", " ")[:200])
    # 这一条是上一条的保险：只停勘察进程不许误伤正式分片。
    check("正式分片的两个服务端没有被顺手杀掉", formal != "0", "cluster=%s 进程数=%s" % (CLUSTER, formal))
    print("   内存占用(MB used/total): %s" % [l for l in out.splitlines() if l.startswith("MEM=")])


def stash_caves_leftover():
    """把 leveldataoverride 时代那份手工文件改名挪开。

    挪之前先对身份：说明文字与 2394 字节都对得上才动，对不上就停下让人来看。
    这既是"不擅自处理别人的东西"，也是因为改名挪走一样是破坏性动作。
    """
    f = "%s/%s/Caves/leveldataoverride.lua" % (KLEI, CLUSTER)
    out = remote("""set -u
F="%s"
test -f "$F" || { echo "ABSENT"; exit 0; }
MARK=$(grep -c '只做开服链路验证' "$F")
SIZE=$(wc -c < "$F")
if [ "$MARK" != "1" ] || [ "$SIZE" != "2394" ]; then
  echo "IDENTITY-MISMATCH mark=$MARK size=$SIZE"; exit 0
fi
mv "$F" "$F%s" && echo "MOVED"
ls -1 "%s/%s/Caves" | head -8
""" % (f, STASH_SUFFIX, KLEI, CLUSTER))
    if "ABSENT" in out:
        check("洞穴没有手工世界文件要挪", True, "本来就不在")
        return
    if "IDENTITY-MISMATCH" in out:
        check("挪走前对上了身份", False, out.replace("\n", " ")[:200] + " —— 没有动这份文件")
        return
    check("身份核对通过才改名挪开（不删，随时能挪回来）", "MOVED" in out, out.replace("\n", " ")[:160])
    # 这一条单独重新列目录：上一份输出里本来就带着改名后的文件名，
    # 拿它做 substring 判断永远为假阳性（"leveldataoverride.lua" 是新名字的前缀）。
    names = [l.strip() for l in remote('ls -1 "%s/%s/Caves"' % (KLEI, CLUSTER)).splitlines()]
    check("洞穴目录里已无 leveldataoverride.lua（改名后的那份留在旁边）",
          "leveldataoverride.lua" not in names and any(n.endswith(STASH_SUFFIX) for n in names),
          " ".join(names)[:180])


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
nohup ./dstpanel -config %s > shards_boot.log 2>&1 &
echo "PID=$!"
up=0
for i in $(seq 1 60); do
  if curl -s -m 2 -o /dev/null http://127.0.0.1:%d/healthz; then up=1; break; fi
  sleep 0.5
done
echo "UP=$up"
""" % (BOOT_DIR, CFG, PORT))
    pid, up = "", "0"
    for line in out.splitlines():
        if line.startswith("PID="):
            pid = line[4:].strip()
        if line.startswith("UP="):
            up = line[3:].strip()
    check("面板起得来", up == "1", out.replace("\n", " ")[:200])
    return pid


def overview():
    code, r = plain_call("GET", "/api/v1/clusters")
    for c in (r.get("data") or []):
        if c.get("name") == CLUSTER:
            return code, c
    return code, None


def shard_view(cluster, shard):
    for s in (cluster or {}).get("shards", []):
        if s.get("name") == shard:
            return s
    return None


def disk_state():
    out = remote("""set -u
for s in Master Caves; do
  D="%s/%s/$s"
  for f in worldgenoverride.lua leveldataoverride.lua; do
    if [ -f "$D/$f" ]; then echo "$s/$f SIZE=$(wc -c < "$D/$f") HEAD=$(head -c 9 "$D/$f")"; else echo "$s/$f ABSENT"; fi
  done
done
grep -E '^[[:space:]]*(override_enabled|preset|worldgen_preset|settings_preset) ' "%s/%s/Master/worldgenoverride.lua" | cut -c1-70
""" % (KLEI, CLUSTER, KLEI, CLUSTER))
    return out


def world_readback(shard, expect_preset, want_rewritten):
    code, r = plain_call("GET", "/api/v1/clusters/%s/shards/%s/world" % (CLUSTER, shard))
    d = r.get("data", {})
    check("%s 世界页读得回来（200 且有预设）" % shard,
          code == 200 and d.get("preset") == expect_preset, "%s %s" % (code, str(d)[:150]))
    check("%s 世界页给界面的是中文说法不是码值" % shard,
          bool(d.get("presetLabel")) and d.get("presetLabel") != d.get("preset"), str(d)[:150])
    if want_rewritten:
        # 这一条是今天这个缺陷的现场验收：磁盘上是引擎存世界后重写的那一份（带 KLEI 头、
        # 预设拆成 worldgen_preset/settings_preset），界面必须既读得出预设，又说明这是引擎干的。
        check("Master 读到的是引擎重写过的版本，界面解释了原因",
              "引擎" in (d.get("note") or "") and "重写" in (d.get("note") or ""),
              (d.get("note") or "")[:200])
        check("Master 的世界页没有被 409 锁住", code == 200, str(d)[:120])
    return d


def helper_preflight(shard):
    """起服之前先确认助手 MOD 在这个分片是"文件在位 + enabled = true"。

    不说这一句就可能白等七分钟：进程活着、MOD 没被打开，界面只会显示助手没回话。
    PUT /helper 走的是面板自己的通道（同步文件 + 改 modoverrides.lua），不是手改磁盘。
    """
    path = "/api/v1/clusters/%s/shards/%s/helper" % (CLUSTER, shard)
    code, r = plain_call("GET", path)
    before = r.get("data", {})
    print("   %s 助手现状: installed=%s configured=%s enabled=%s missing=%s differs=%s extra=%s"
          % (shard, before.get("installed"), before.get("configured"), before.get("enabled"),
             before.get("missing"), before.get("differs"), before.get("extra")))
    code, r = plain_call("PUT", path, {"enabled": True})
    check("%s 的助手 MOD 启用接口没有拒绝" % shard, code == 200, "%s %s" % (code, str(r)[:200]))
    code, r = plain_call("GET", path)
    d = r.get("data", {})
    check("%s 的助手 MOD 文件与内嵌版本一致" % shard,
          d.get("installed") is True and not d.get("missing") and not d.get("differs"),
          json.dumps(d, ensure_ascii=False)[:240])
    check("%s 的助手 MOD 在 modoverrides.lua 里是开着的" % shard,
          d.get("configured") is True and d.get("enabled") is True, json.dumps(d, ensure_ascii=False)[:200])
    return d


def ensure_stopped(shard, wait_seconds=240):
    """启动之前先让面板把它关掉。

    两个原因：一是 procrun.Start 对已在跑的分片直接报错（避免同存档双开），
    不关就验不了"面板能不能起它"；二是面板的优雅关闭会先落盘，
    而落盘正是引擎重写 worldgenoverride.lua 的触发器，等于多一次现场证据。
    """
    v = shard_view(overview()[1], shard)
    if v is None:
        check("%s 关闭前总览里看得见" % shard, False, "总览没有这个分片")
        return False
    if not v.get("running"):
        check("%s 本来没在跑，不需要先关" % shard, True, "running=false")
        return True
    code, r = plain_call("POST", "/api/v1/clusters/%s/shards/%s/stop" % (CLUSTER, shard),
                         {"saveFirst": True})
    check("%s 的优雅关闭（先落盘）接口没有拒绝" % shard, code == 200,
          "%s %s" % (code, json.dumps(r, ensure_ascii=False)[:200]))
    deadline = time.time() + wait_seconds
    while time.time() < deadline:
        time.sleep(10)
        v = shard_view(overview()[1], shard)
        if v is None or not v.get("running"):
            check("%s 已经自己退出会话" % shard, True, "pid=%s" % (v or {}).get("pid"))
            return True
    v = shard_view(overview()[1], shard) or {}
    check("%s 已经自己退出会话" % shard, False,
          "等满 %ds 还在跑: %s" % (wait_seconds, json.dumps(v, ensure_ascii=False)[:200]))
    return False


def boot(shard, wait_seconds):
    code, r = plain_call("POST", "/api/v1/clusters/%s/shards/%s/start" % (CLUSTER, shard))
    msg = json.dumps(r, ensure_ascii=False)
    check("%s 启动接口没有拒绝" % shard, code == 200, msg[:280])
    if code != 200:
        return False
    deadline = time.time() + wait_seconds
    waited = 0
    while time.time() < deadline:
        time.sleep(15)
        waited += 15
        c2, cur = overview()
        view = shard_view(cur, shard)
        if view is None:
            check("%s 助手回话（真进了世界）" % shard, False, "总览里连分片都不见了: %s" % str(c2))
            return False
        if view.get("helperOnline"):
            check("%s 助手回话（真进了世界）" % shard, True,
                  "等待 %ds，进程 pid=%s" % (waited, view.get("pid")))
            return True
        if view.get("running") is False:
            check("%s 助手回话（真进了世界）" % shard, False,
                  "进程在 %ds 时自己退出了: %s" % (waited, json.dumps(view, ensure_ascii=False)[:220]))
            return False
    c3, cur = overview()
    check("%s 助手回话（真进了世界）" % shard, False,
          "等满 %ds: %s" % (wait_seconds, json.dumps(shard_view(cur, shard) or {}, ensure_ascii=False)[:260]))
    return False


def command(shard, name, args=None, wait=25):
    body = {"command": name, "args": args or [], "waitSeconds": wait}
    return plain_call("POST", "/api/v1/clusters/%s/shards/%s/commands" % (CLUSTER, shard), body)


def round_trip(shard):
    code, r = command(shard, "ping")
    d = r.get("data", {})
    check("%s 的 ping 走完整条链路拿到回执" % shard,
          code == 200 and d.get("done") is True, json.dumps(r, ensure_ascii=False)[:250])
    code, r = command(shard, "announce", ["面板首启验证：这条中文公告来自管理面板"])
    d = r.get("data", {})
    check("%s 的中文公告拿到成功回执（文本进 Lua 的转义没问题）" % shard,
          code == 200 and d.get("done") is True, json.dumps(r, ensure_ascii=False)[:250])
    code, r = plain_call("GET", "/api/v1/clusters/%s/shards/%s/status" % (CLUSTER, shard))
    st = r.get("data", {}) or {}
    # 字段名照 status.go 的 json tag 写：世界是 season/phase（还各带一个中文 Label），
    # 在线人数是 online/maxPlayers。上一版拿 "playerCount" 去比，那是我自己编的名字，
    # 于是接口明明什么都没丢，探针却报失败。
    check("%s 的状态快照里有世界与在线人数" % shard,
          code == 200 and st.get("season") and st.get("seasonLabel") and st.get("phaseLabel")
          and isinstance(st.get("online"), int) and st.get("maxPlayers"),
          json.dumps(st, ensure_ascii=False)[:240])
    check("%s 的快照给界面的是中文季节与时段" % shard,
          st.get("seasonLabel") != st.get("season") and st.get("phaseLabel") != st.get("phase"),
          "%s/%s" % (st.get("seasonLabel"), st.get("phaseLabel")))
    return st


def evidence(shard):
    # 上一版这里给 $F 加了转义引号，bash 收到的是字面量 \"，路径找不到，
    # size 一直报 0——结论看起来对（后面 tail 是真读到了），数字却是假的。
    out = remote("""set -u
F="%s/%s/%s/server_log.txt"
if [ -f "$F" ]; then
  echo "size=$(wc -c < "$F")"
  echo "拒绝启动=$(grep -c 'Your Server Will Not Start' "$F")"
  echo "世界生成失败=$(grep -cE 'Worldgen had an error|missing required prefab|nonexistent preset' "$F")"
  echo "存过世界=$(grep -c 'Serializing world' "$F")"
  tail -3 "$F" | cut -c1-110
else
  echo "NOLOG"
fi
""" % (KLEI, CLUSTER, shard))
    return " ".join(out.split())


def admin_save(shard):
    """用面板自己的白名单命令下发一次 c_save()：这是"引擎什么时候重写世界文件"的触发器。

    2026-10-31 实测：重写不在启动时，而在存世界的时候（mainfunctions.lua 的世界保存回调
    里 UpdateWorldGenOverride 走 SetPersistentString）。不等自动存档、当场存一次，
    下面那条断言才是确定的，而不是"看运气等多久"。
    """
    code, r = plain_call("POST", "/api/v1/clusters/%s/shards/%s/admin-commands" % (CLUSTER, shard),
                         {"command": "save", "args": {}, "confirm": False})
    check("%s 的立即保存命令下发成功" % shard, code == 200, json.dumps(r, ensure_ascii=False)[:200])
    return code == 200


def main():
    global PW
    PW = password()
    stop_scratch()
    stash_caves_leftover()
    push_binary()
    pid = start_panel()
    if not pid:
        return finish()
    try:
        c0, cur = overview()
        check("面板认得出 %s（磁盘扫描，不依赖数据库）" % CLUSTER,
              c0 == 200 and cur is not None, str(c0) + " " + json.dumps(cur, ensure_ascii=False)[:150])
        print("挪开与停服之后的磁盘现状:\n" + disk_state())
        # 0) 先把上一轮起着的两个分片优雅关掉（先落盘）。不关的话：
        #    procrun.Start 会拒绝已在跑的分片，验不了"面板起不起得来"；
        #    而且接着写洞穴的世界文件，分片关机会把它重写回引擎那份，白写。
        for shard in ("Caves", "Master"):
            if not ensure_stopped(shard):
                return finish()
        # 1) 读现场：Master 磁盘上是引擎重写过的文件，Caves 还没有世界文件。
        world_readback("Master", "SURVIVAL_TOGETHER_CLASSIC", want_rewritten=True)
        code, r = plain_call("PUT", "/api/v1/clusters/%s/shards/Master/world" % CLUSTER,
                             {"overrides": {}})
        check("重写过的 Master 现在改得动设定（不再 409）", code == 200,
              "%s %s" % (code, json.dumps(r, ensure_ascii=False)[:200]))
        d = r.get("data", {})
        check("沿用引擎留下的预设而不是打回默认", d.get("preset") == "SURVIVAL_TOGETHER_CLASSIC", str(d)[:150])
        print("   Master 磁盘: %s" % remote('head -c 12 "%s/%s/Master/worldgenoverride.lua"; echo; wc -c < "%s/%s/Master/worldgenoverride.lua"'
                                            % (KLEI, CLUSTER, KLEI, CLUSTER)).replace("\n", " ")[:120])
        # 2) 洞穴：面板第一次为洞穴写世界文件，走 DST_CAVE 预设。
        code, r = plain_call("PUT", "/api/v1/clusters/%s/shards/Caves/world" % CLUSTER,
                             {"preset": "DST_CAVE", "overrides": {}})
        d = r.get("data", {})
        check("洞穴世界文件写成功且预设是 DST_CAVE", code == 200 and d.get("preset") == "DST_CAVE",
              "%s %s" % (code, str(d)[:150]))
        top = remote("""sed -n '1,4p' "%s/%s/Caves/worldgenoverride.lua" | cut -c1-60""" % (KLEI, CLUSTER))
        check("洞穴文件是面板那三种顶层键的写法",
              "return {" in top and "override_enabled = true," in top and 'preset = "DST_CAVE",' in top,
              top.replace("\n", " | ")[:200])
        check("洞穴没有 leveldataoverride 冲突提示", not d.get("levelDataConflict"), str(d)[:140])

        # 3) 真起两个分片。Master 有存档（不会重造世界），Caves 没有（第一次生洞穴）。
        for shard in ("Master", "Caves"):
            helper_preflight(shard)
        ok_master = boot("Master", 300)
        ok_caves = boot("Caves", 420) if ok_master else None
        if not ok_master:
            check("Caves 因地表没进世界而跳过", True, "先查 Master 的日志")

        # 4) 命令往返只在助手在线时才有意义。
        if ok_master:
            round_trip("Master")
        if ok_caves:
            round_trip("Caves")

        # 5) 起完之后再看一次：引擎会把两份世界文件都重写成它自己的拼法。
        #    触发器是"存世界"，所以先用面板的 c_save() 各存一次，再等它落盘。
        for shard in ("Master", "Caves"):
            print("%s 日志尾巴: %s" % (shard, evidence(shard)[:320]))
        saved = [admin_save(s) for s in ("Master", "Caves")]
        if all(saved):
            time.sleep(20)
        print("启动之后的磁盘现状:\n" + disk_state())
        c4, cur = overview()
        for shard in ("Master", "Caves"):
            v = shard_view(cur, shard) or {}
            print("%s 总览: %s" % (shard, json.dumps(v, ensure_ascii=False)[:260]))
        for shard, preset in (("Master", "SURVIVAL_TOGETHER_CLASSIC"), ("Caves", "DST_CAVE")):
            rewritten = remote('head -c 4 "%s/%s/%s/worldgenoverride.lua"' % (KLEI, CLUSTER, shard)).strip()
            if rewritten.startswith("KLEI"):
                # 引擎已经重写过：这一份才是世界页此后天天要读的东西。
                world_readback(shard, preset, want_rewritten=True)
            else:
                # 还没重写（保存是异步的，或这一发没轮到）：不能因此判失败，
                # 但必须说出来——不然这条检查就变成了"永远绿"的摆设。
                check("%s 本轮没等到引擎重写，只验面板自己那份读得回" % shard, True,
                      "文件头 4 字节=%s" % rewritten[:8])
                world_readback(shard, preset, want_rewritten=False)
    finally:
        remote("kill %s 2>/dev/null; sleep 1; pgrep -f %s | xargs -r kill; true" % (pid, CFG))
        print("面板已关；分片留在 screen 会话里跑着。重开面板："
              "  ssh %s 'cd %s && nohup ./dstpanel -config %s > shards_boot.log 2>&1 &'" % (HOST, BOOT_DIR, CFG))
        print("当前会话: %s" % " ".join(remote("screen -ls 2>&1 | head -6").split())[:220])
    return finish()


def finish():
    bad = [n for n, ok in results if not ok]
    print("\n合计 %d 项，失败 %d 项" % (len(results), len(bad)))
    if bad:
        print("失败：" + "、".join(bad))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
