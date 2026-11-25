# 界面文案清单：把"用户真会看到的中文"从代码里挑出来，按文件分组打出来。
#
# 为什么要有这个脚本：#53 那次术语清扫（分片→世界、世界设定→世界规则、删技术自证句）
# 要动的只有界面文案，而仓库里"分片"三个字有九成在 Go 注释和测试里——那些是写给
# 维护者看的，用准确的工程词才对，不该跟着界面一起改。
# 所以这里先把注释剥掉，只留下字符串字面量里的中文，让人一眼看清要审的是哪些行。
#
# 用法：python -X utf8 .tools/copy_inventory.py [--all]
#   默认只列带 分片/世界设定/快照/助手 MOD/模组 这些待清扫词的行；--all 列全部含中文行。
import os, re, sys

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")

# 界面文案的家：前端 + 后端会写进 JSON 响应/审计标签的那几处。
SCAN = [
    ("internal/webui/assets/index.html", "html"),
    ("internal/webui/assets/app.css", "css"),
]
for dirpath, dirnames, filenames in os.walk(os.path.join(ROOT, "internal/webui/assets/js")):
    dirnames[:] = [d for d in dirnames if d not in ("vendor", "mods")]
    for fn in sorted(filenames):
        if fn.endswith(".js"):
            SCAN.append((os.path.relpath(os.path.join(dirpath, fn), ROOT).replace("\\", "/"), "js"))
for fn in sorted(os.listdir(os.path.join(ROOT, "internal/api"))):
    if fn.endswith(".go") and not fn.endswith("_test.go"):
        SCAN.append(("internal/api/" + fn, "go"))

# 界面文案不只在 internal/api：世界规则的选项名在 dst/worldgen.go，
# 事件名在 gamelog/entry.go，启动失败的诊断句在 procrun，任务说明在 worker。
# 这些包的中文最后都会被 writeOK/writeErr 或 JSON 字段带到界面上，所以一起扫。
for dirpath, dirnames, filenames in os.walk(os.path.join(ROOT, "internal")):
    dirnames[:] = [d for d in dirnames if d not in ("mods", "webui", "api")]
    for fn in sorted(filenames):
        if fn.endswith(".go") and not fn.endswith("_test.go"):
            rel = os.path.relpath(os.path.join(dirpath, fn), ROOT).replace("\\", "/")
            SCAN.append((rel, "go"))

WORDS = sys.argv[1:] or ["分片", "世界设定", "快照", "助手", "模组", "没打过", "备份"]

# --left 用：清扫之后复查还剩哪些待换词。命令行里不能打中文（Git Bash 会挂），
# 所以这几个词写死在这儿，由脚本自己挑。
LEFTOVER = ["分片", "世界设定", "快照", "screen", "pid", "PATH", "kleiRoot", "workDir",
            "心跳", "modoverrides", "leveldataoverride", "worldgenoverride", "DSTPanelHelper"]
if "--left" in sys.argv:
    WORDS = [w for w in LEFTOVER if w not in ("screen", "pid", "PATH", "kleiRoot", "workDir",
                                              "modoverrides", "leveldataoverride",
                                              "worldgenoverride", "DSTPanelHelper")]

STR_RE = re.compile(r'"(?:[^"\\]|\\.)*"|`(?:[^`\\]|\\.)*`')
TAG_TEXT_RE = re.compile(r">([^<>{}]*[\u4e00-\u9fff][^<>{}]*)<")


def strip_js_comments(line, in_block):
    """粗粒度剥注释：够用来判断"这串中文是代码注释还是界面文案"，不做词法分析。"""
    out = []
    i = 0
    while i < len(line):
        if in_block:
            j = line.find("*/", i)
            if j < 0:
                return "".join(out), True
            i, in_block = j + 2, False
            continue
        two = line[i:i + 2]
        if two == "/*":
            in_block = True
            i += 2
            continue
        if two == "//":
            break
        out.append(line[i])
        i += 1
    return "".join(out), in_block


def strings_of(code, kind):
    if kind == "go":
        return STR_RE.findall(code)
    if kind == "html":
        return TAG_TEXT_RE.findall(code) + STR_RE.findall(code)
    return STR_RE.findall(code) + TAG_TEXT_RE.findall(code)


def main():
    hit_total = 0
    for rel, kind in SCAN:
        path = os.path.join(ROOT, rel.replace("/", os.sep))
        if not os.path.exists(path):
            continue
        in_block = False
        rows = []
        with open(path, encoding="utf-8") as f:
            for n, raw in enumerate(f, 1):
                line = raw.rstrip("\n")
                keep = line
                if kind in ("js", "css"):
                    keep, in_block = strip_js_comments(line, in_block)
                if kind == "go":
                    s = line.strip()
                    if s.startswith("//"):
                        continue
                if not any(w in keep for w in WORDS):
                    continue
                texts = [t for t in strings_of(keep, kind) if re.search(r"[\u4e00-\u9fff]", t)]
                if not texts:
                    # Vue 模板里的中文既不在引号里、又夹着 {{ }}，上面两种取法都会空手而归。
                    # 这一支兜住它：整行原样列出，宁可多给几行也别漏掉界面上的旧词。
                    bare = keep.strip()
                    if bare:
                        texts = [bare]
                rows.append((n, texts))
        if rows:
            hit_total += len(rows)
            print("\n#### " + rel)
            for n, texts in rows:
                print("  %4d  %s" % (n, "  |  ".join(t if len(t) < 300 else t[:300] + "…" for t in texts)))
    print("\n合计 %d 行待看" % hit_total)
    return 0


if __name__ == "__main__":
    sys.exit(main())
