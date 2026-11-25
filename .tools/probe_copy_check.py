"""探针脚本里"把界面文案当断言"的行：改完 #53 的文案后要逐个核对。

这次不只找 `"中文串" in 变量`，还把 % 格式化拼出来的期望值算出来再比：
像 ("共 %d 个房间" % TOTAL) in p1["pager"] 这种就是这一类，
正则直接看字面量会漏掉它。

用法：python -X utf8 .tools/probe_copy_check.py
"""
import io
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
TOOLS = os.path.join(ROOT, ".tools")
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

OLD = ["分片", "世界设定", "快照", "没打过", "助手心跳", "状态快照"]

# 断言的三种写法：字面量 in x / x in 字面量 / 用 % 拼出来的期望值 in x
IN_RE = re.compile(r'"[^"]*[\u4e00-\u9fff][^"]*"\s*(?:not\s+)?in\b')
FMT_RE = re.compile(r'\("[^"]*[\u4e00-\u9fff][^"]*"\s*%\s*[^)]*\)\s*(?:not\s+)?in\b')


def main():
    total = 0
    for fn in sorted(os.listdir(TOOLS)):
        if not fn.endswith(".py") or fn in ("copy_inventory.py", "copy_sweep.py",
                                            "probe_copy_check.py", "markers_check.py"):
            continue
        path = os.path.join(TOOLS, fn)
        with io.open(path, encoding="utf-8") as f:
            for n, raw in enumerate(f, 1):
                s = raw.strip()
                if s.startswith("#") or s.startswith('"""'):
                    continue
                for pat in (IN_RE, FMT_RE):
                    hit = any(any(w in m.group(0) for w in OLD) for m in pat.finditer(s))
                    if hit:
                        print("%s:%d  %s" % (fn, n, s[:170]))
                        total += 1
                        break
    print("\n共 %d 处断言引用了旧文案" % total)
    return 1 if total else 0


if __name__ == "__main__":
    main()
