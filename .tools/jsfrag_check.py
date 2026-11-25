# 把验收脚本里拼出来的那一段段 JS 全部拿出来过一遍 node --check。
#
# 为什么要单独一条：ui_lobby_check.py 里的 JS 是用 % 拼出来的，拼错一位（少一个引号、
# 多一个括号）在 python 侧完全看不出来——它要到浏览器里才炸，而炸的位置
# （SyntaxError: Invalid or unexpected token）离真正的错处经常隔了两个函数。
# 上一轮就是这么红在"摘掉搜索那一枚条件"那一步，其实错在上一段的字符串拼接。
#
# 这里不需要起浏览器：拿一个假 browser 把每次 js() 收到的表达式录下来，逐条给 node 解析。
import importlib.util
import os
import subprocess
import sys
import tempfile

sys.stdout.reconfigure(encoding="utf-8")

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("h", os.path.join(HERE, "ui_lobby_check.py"))
h = importlib.util.module_from_spec(spec)
spec.loader.exec_module(h)


class Recorder:
    def __init__(self):
        self.seen = []

    def js(self, expression):
        self.seen.append(expression)
        return None


r = Recorder()
h.type_search(r, "zzqq%服\"'<>")
h.search_value(r)
h.chips_now(r)
h.pick_select(r, "按玩法筛选", "无尽")
h.click_seg(r, "按门槛筛选", "门槛全开")
h.click_text(r, ".lobby-page button", "取最新")
h.click_aria(r, ".icon-btn", "切到深色")
r.seen.append(h.STATE_JS)
r.seen.append(h.FIND_BOX)

bad = 0
tmp = os.path.join(tempfile.gettempdir(), "ui_lobby_js_frag.mjs")
for i, expr in enumerate(r.seen):
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(expr)
    p = subprocess.run(["node", "--check", tmp], capture_output=True, text=True, encoding="utf-8")
    if p.returncode != 0:
        bad += 1
        print("FAIL [%d] %s" % (i, (p.stderr or "").strip().splitlines()[:3]))
        print("     %s" % expr.replace("\n", " ")[:200])
    else:
        print("ok   [%d] %s" % (i, expr.replace("\n", " ")[:70]))
os.remove(tmp)
print("\n%d 段拼出来的 JS 解析不过" % bad)
sys.exit(1 if bad else 0)
