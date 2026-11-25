#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""从云机向 Steam 官方 Web API 发真请求，把"检索"这一环的响应结构钉住。

为什么绕到云机去打：本机直连 api.steampowered.com 超时（curl rc=28），只有 dst-cloud 实测可达。

key 的处理：只从环境变量 DST_STEAM_KEY 读（不落盘、不进 argv），脚本用 stdin 喂给远端 `bash -s`，所以它不出现在本机命令行的
argv 里；远端把它写进 /tmp 的 0600 文件，再用 curl 的 `key@文件` 传，这样 ps 里也看不到它，
结尾删掉。拼进脚本前先校验字符集——不合法就说明配置被改坏了，绝不能原样塞进 shell。
"""
import json
import os
import re
import subprocess
import sys

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOST = "dst-cloud"
API = "https://api.steampowered.com"
QUERY_FILES = API + "/IPublishedFileService/QueryFiles/v1/"
GET_DETAILS = API + "/IPublishedFileService/GetDetails/v1/"

KEY_RE = re.compile(r"^[0-9A-Fa-f]{16,64}$")


def load_key():
    # 只从环境变量读：配置文件里那份已经清空了，凭据不该留在项目目录里。
    # 用法：DST_STEAM_KEY=xxxx python .tools/steam_probe.py
    key = os.environ.get("DST_STEAM_KEY", "").strip()
    if not key:
        sys.exit("没有 DST_STEAM_KEY 环境变量，探针不猜 key；"
                 "用法：DST_STEAM_KEY=<你的 key> python .tools/steam_probe.py")
    if not KEY_RE.match(key):
        sys.exit("DST_STEAM_KEY 不是十六进制串，像是被改坏了；探针拒绝把它拼进远端脚本")
    return key


HEAD = """set -u
umask 077
K=/tmp/.dstprobe_key
printf '%s' '__KEY__' > "$K"
run() { curl -s -m 25 -w '\\nHTTP=%{http_code}\\n' --get "$1" --data-urlencode "key@$K" "${@:2}"; }
"""


TAIL = 'rm -f "$K"\n'


def call(key, url, params):
    script = HEAD.replace("__KEY__", key) + 'run "%s" %s\n' % (
        url, " ".join('--data-urlencode "%s"' % p for p in params)) + TAIL
    # stdin 必须自己编码成 bytes：Windows 上 text=True 会把 \n 翻成 \r\n，
    # 远端 bash 就把整行脚本当成带 \r 的参数，报一堆 "invalid option"。
    p = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", HOST, "bash", "-s"],
                       input=script.encode("utf-8"), capture_output=True, timeout=150)
    err = (p.stderr or b"").decode("utf-8", "replace").strip()
    if p.returncode != 0:
        print("  ssh rc=%d %s" % (p.returncode, err[:200]))
    out = (p.stdout or b"").decode("utf-8", "replace")
    body, _, code = out.rpartition("HTTP=")
    return body.strip(), code.strip()


def describe(body):
    try:
        return json.loads(body)
    except Exception:
        return None


def main():
    key = load_key()

    print("===== A 关键词检索 QueryFiles")
    a, code = call(key, QUERY_FILES, ["appid=322330", "search_text=stack", "numperpage=4", "page=1",
                                      "return_tags=true", "return_children=true",
                                      "return_vote_data=true", "language=6"])
    print("  HTTP=%s" % code)
    ja = describe(a)
    first_id = preview_url = None
    if ja is None:
        print("  不是 JSON，前 500 字：%s" % a[:500])
    else:
        resp = ja.get("response") or {}
        items = resp.get("publishedfiledetails") or []
        print("  顶层=%s" % sorted(ja.keys()))
        print("  response=%s success=%s total=%s 条数=%d" % (
            sorted(resp.keys()), resp.get("success"), resp.get("total"), len(items)))
        if items:
            print("  条目字段=%s" % sorted(items[0].keys()))
        for it in items:
            print("  - id=%s name=%r subs=%s updated=%s num_children=%s vote=%s tags=%s" % (
                it.get("publishedfileid"), (it.get("title") or "")[:20], it.get("subscriptions"),
                it.get("time_updated"), it.get("num_children"), it.get("vote_data"),
                [t.get("tag") for t in (it.get("tags") or [])][:4]))
        if items:
            first_id = items[0].get("publishedfileid")
            preview_url = items[0].get("preview_url")

    if first_id:
        print("\n===== B 按 ID 取详情 GetDetails")
        b, code = call(key, GET_DETAILS, ["publishedfileids[0]=%s" % first_id, "include_children=true"])
        print("  HTTP=%s" % code)
        jb = describe(b)
        if jb is None:
            print("  不是 JSON，前 500 字：%s" % b[:500])
        else:
            resp = jb.get("response") or {}
            items = resp.get("publishedfiledetails") or []
            print("  response=%s success=%s 条数=%d" % (sorted(resp.keys()), resp.get("success"), len(items)))
            for it in items:
                print("  条目字段=%s" % sorted(it.keys()))
                print("  - id=%s result=%s name=%r file_url=%s hcontent=%s" % (
                    it.get("publishedfileid"), it.get("result"), (it.get("title") or "")[:20],
                    bool(it.get("file_url")), bool(it.get("hcontent_file"))))

    if preview_url:
        print("\n===== C 工坊预览图 CDN 在云机可达吗（决定界面放不放图）")
        # 必须走 bash -s 并把 URL 放进单引号：URL 里有 & ，拼成命令串会被远端 shell 当后台符切断。
        script = "curl -s -m 12 -o /tmp/.dstprobe_img -w 'http=%%{http_code} time=%%{time_total}' '%s'\n" % preview_url
        script += "ls -l /tmp/.dstprobe_img | awk '{print \"bytes=\"$5}'\nrm -f /tmp/.dstprobe_img\n"
        p = subprocess.run(["ssh", HOST, "bash", "-s"], input=script.encode("utf-8"),
                           capture_output=True, timeout=60)
        host = re.sub(r"^https?://([^/]+)/.*$", r"\1", preview_url)
        print("  host=%s  %s" % (host, (p.stdout or b"").decode("utf-8", "replace").strip()[:160]))

    print("\n===== D 无效 key 的错误形状（降级文案要照着这个写）")
    bad, code = call("0123456789ABCDEF0123456789ABCDEF", QUERY_FILES,
                     ["appid=322330", "search_text=stack", "numperpage=1"])
    print("  HTTP=%s 前 300 字：%s" % (code, (bad or "(空)")[:300]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
