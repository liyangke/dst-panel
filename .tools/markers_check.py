"""artifact_check 的 MARKERS 清单是否还能在源码里找到。

成品检查用的是"字符串在不在二进制里"，跑之前先把源码这一层过一遍：
文案改动（#53）会让清单里的某几条永远查不到，那时成品检查会报"这段代码没接上"，
其实是标记本身过期了。这一支脚本就是为了先把这种假警报挑出来。

用法：python -X utf8 .tools/markers_check.py
"""
import io
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, os.path.join(ROOT, ".tools"))
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

import artifact_check  # noqa: E402


def all_sources():
    for base in ("internal", "cmd"):
        for dirpath, dirnames, filenames in os.walk(os.path.join(ROOT, base)):
            dirnames[:] = [d for d in dirnames if d != "vendor"]
            for fn in filenames:
                if fn.endswith((".go", ".lua", ".js", ".html", ".css", ".json", ".sql")):
                    yield os.path.join(dirpath, fn)


def main():
    blob = []
    for path in all_sources():
        with io.open(path, encoding="utf-8", errors="replace") as f:
            blob.append(f.read())
    whole = "\n".join(blob)
    missing = []
    for m in artifact_check.MARKERS:
        if m in whole:
            continue
        # 只有写法里带反斜杠的（artifact_check 里 progress:\s* 那一条）才当正则再兜一次。
        # 这个门槛是 2026-11-05 补的：原来无条件 re.search，把 "v-if=\"open || live\"" 放行了——
        # || 在正则里读作"两种写法，而且每一边都可以是空"，空分支对任何文本都成立，
        # 于是凡是含 || 的标记永远"查得到"，源码里那句话早被改掉也照样绿，藏了好几轮。
        if "\\" in m:
            try:
                if re.search(m, whole):
                    continue
            except re.error:
                pass
        missing.append(m)
    for m in missing:
        print("源码里查不到: %r" % m)
    print("\n共 %d 条标记，%d 条对不上" % (len(artifact_check.MARKERS), len(missing)))
    return 1 if missing else 0


if __name__ == "__main__":
    sys.exit(main())
