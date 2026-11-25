# #53 术语清扫的批量替换：只动界面会显示的字符串，不动写给维护者看的注释。
#
# 为什么要用脚本而不是一家一家手改：这次要换的是同一批词在二十多个文件里的几十处，
# 手改必然漏；而全文件 sed 会把 Go 注释里的"分片"也换掉——那是工程上更准确的词，
# 注释该保留。所以这里按"引号外才是注释、引号内才是文案"切一遍，只替换该替换的部分。
#
# 用法：
#   python -X utf8 .tools/copy_sweep.py          # 只打印将要改的行（默认）
#   python -X utf8 .tools/copy_sweep.py --write  # 真的写盘
import os, re, sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from copy_inventory import SCAN, ROOT

# 顺序很重要：先换更长的固定说法，再换裸词，否则"双分片"会被"分片"那条拆坏。
RULES = [
    # ---- 世界设定 → 世界规则（"世界"这个词要让给分片，见下面一组） ----
    ("世界设定", "世界规则"),

    # ---- 分片 → 世界：界面上没人说"分片"，玩家看到的是"地表"和"洞穴"两个世界 ----
    ("双分片", "两个世界"),
    ("每一个分片", "每一个世界"),
    ("重生本分片世界", "重生这个世界"),
    ("本分片", "这个世界"),
    ("主分片", "地表"),
    ("次分片", "洞穴"),
    ("个分片", "个世界"),
    ("分片", "世界"),

    # ---- 快照 → 备份：存档那一套在界面上叫"备份"，"快照"是工程说法 ----
    # 但"状态快照/在线快照"是运行时数据，不是备份。这几条必须排在通用规则前面，
    # 否则 "快照 %s" 会先把 "状态快照 %s" 吃掉，界面上就出现"状态备份"这种词。
    ("状态快照", "状态数据"),
    ("在线快照", "在线人数记录"),
    ("记快照", "记在线人数"),
    ("读取快照", "读取备份"),
    ("快照清单", "备份清单"),
    ("快照时刻", "备份时刻"),
    ("快照可以下载", "备份可以下载"),
    ("不在快照里", "不打包进备份"),
    ("存档快照", "存档备份"),
    ("这份快照里没有玩家", "这次采集里没有玩家"),
    ("回传第一份快照", "回传第一份数据"),
    ("第一份快照", "第一份数据"),
    ("打一份快照", "打一份备份"),
    ("没打过快照", "未备份"),
    ("还没有任何快照", "还没有任何备份"),
    ("还没有快照", "还没有备份"),
    ("这份快照", "这份备份"),
    ("那一份快照", "那一份备份"),
    ("一份快照", "一份备份"),
    ("打快照", "打备份"),
    ("快照目录", "备份目录"),
    ("快照编号", "备份编号"),
    ("快照列表", "备份列表"),
    ("快照已删除", "备份已删除"),
    ("快照不能用", "备份不能用"),
    ("快照来自", "备份来自"),
    ("更早的快照", "更早的备份"),
    ("最近一份快照", "最近一份备份"),
    ("快照最多保留", "备份最多保留"),
    ("快照 %s", "备份 %s"),
    ("快照（", "备份（"),
    # 助手的运行时状态不是备份，说"快照"会让人以为要找的是存档文件。
    ("上次快照", "上次数据"),
    ("没有快照", "没有数据"),
    ("快照已过期", "数据已过期"),
    ("快照时间", "数据时间"),
    ("份快照", "份数据"),

    # ---- 第三轮：backup / modbridge / monitor 这些包里的说法 ----
    ("助手快照", "助手回传的数据"),
    ("完整快照", "完整数据"),
    ("读快照", "读状态数据"),
    ("快照已停更", "数据已停更"),
    ("不可信的快照", "不可信的数据"),
    ("助手心跳", "助手上报"),
    ("清理快照失败", "清理旧备份失败"),
    ("清理快照时", "清理备份时"),
    ("份最旧的快照", "份最旧的备份"),
    ("受保护的快照", "受保护的备份"),
    ("同名快照", "同名备份"),
    ("快照已经打好", "备份已经打好"),
    ("不是面板生成的快照", "不是面板生成的备份"),
    ("恢复后的快照", "恢复后的备份"),
    ("快照里的", "备份里的"),
    ("照快照那套", "照备份那套"),
    ("快照是可以下载的", "备份是可以下载的"),
]

COMPILED = [(re.compile(re.escape(a)), b) for a, b in RULES]

QUOTES = "\"'`"


def split(line, kind, in_block):
    """把一行切成 (代码, 行尾注释, 新的块注释状态)。

    引号里面的 // 和 <!-- 不是注释，字符串里的中文才是要改的文案；
    反过来，行首注释里出现"分片"是写给维护者看的，得原样留着。
    """
    code, i = [], 0
    quote = ""
    while i < len(line):
        c = line[i]
        two = line[i:i + 2]
        if quote:
            code.append(c)
            if c == "\\" and quote != "`":
                if i + 1 < len(line):
                    code.append(line[i + 1])
                i += 2
                continue
            if c == quote:
                quote = ""
            i += 1
            continue
        if in_block:
            end = "*/" if kind != "html" else "-->"
            j = line.find(end, i)
            if j < 0:
                return "".join(code), line[i:], True
            return "".join(code) + line[i:j + len(end)], "", False
        if c in QUOTES:
            quote = c
            code.append(c)
            i += 1
            continue
        if two == "//" and kind != "html":
            return "".join(code), line[i:], False
        if two == "/*" and kind != "html":
            return "".join(code), line[i:], True
        if kind == "html" and line.startswith("<!--", i):
            j = line.find("-->", i)
            if j < 0:
                return "".join(code), line[i:], True
            return "".join(code) + line[j + 3:], "", False
        code.append(c)
        i += 1
    return "".join(code), "", in_block


def main():
    write = "--write" in sys.argv
    # --tpl：只按"整行都是代码"处理，给前端那种跨几十行的 Vue 模板字符串用。
    # 上面那套按引号状态切的做法在模板字符串里会一路当成"还在字符串里"，
    # 结果模板里的中文一行都没被扫到——界面上就还留着"那些快照"这种旧词。
    # 只限 JS：CSS 和 Go 的注释里"分片"是写给维护者的准确说法，不该跟着界面改。
    tpl_only = "--tpl" in sys.argv
    changed = 0
    for rel, kind in SCAN:
        if tpl_only and not rel.endswith(".js"):
            continue
        path = os.path.join(ROOT, rel.replace("/", os.sep))
        if not os.path.exists(path):
            continue
        with open(path, encoding="utf-8") as f:
            lines = f.read().split("\n")
        out, in_block, touched = [], False, []
        for n, raw in enumerate(lines, 1):
            stripped = raw.strip()
            if stripped.startswith("//") or stripped.startswith("#"):
                out.append(raw)
                continue
            if stripped.startswith("*") or stripped.startswith("<!--"):
                out.append(raw)
                continue
            if tpl_only:
                code, comment = raw, ""
            else:
                code, comment, in_block = split(raw, kind, in_block)
            new_code = code
            for pat, rep in COMPILED:
                new_code = pat.sub(rep, new_code)
            if new_code != code:
                touched.append((n, raw, new_code + comment))
                out.append(new_code + comment)
            else:
                out.append(raw)
        if not touched:
            continue
        changed += len(touched)
        print("\n#### " + rel + ("  (已写入)" if write else ""))
        for n, before, after in touched:
            print("  - %4d  %s" % (n, before.strip()))
            print("  + %4d  %s" % (n, after.strip()))
        if write:
            with open(path, "w", encoding="utf-8", newline="\n") as f:
                f.write("\n".join(out))
    print("\n共 %d 行%s" % (changed, "已改" if write else "待改（加 --write 才写盘）"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
