# -*- coding: utf-8 -*-
"""前端静态体检（免构建 Vue 的几类哑点）。

1. 模板里出现的 class 必须在 app.css 有定义：少一条规则不会报错，
   只会让那块布局退化成浏览器默认样式，而截图里很容易看成"设计本来就这样"。
   例外只有"验收脚本拿来点这一枚的定位钩子"（见 harness_class_tokens），
   判据是那个名字真在 .tools/*.py 里出现过，不是手写一份豁免名单。
2. 入口链上的生命周期导出（init*/boot*/start*/...）必须真的有人调用。
   实测踩过一次——store.js 导出了 initTheme()，但入口 app.js 从没调它，
   于是"记住主题"这个功能整个是空的，界面看起来却完全正常。
3. 组件 emits 里声明的事件必须有人在模板里 @绑上（见 audit_emits）。
4. 从 this 上解构出来的键必须真的存在（见 audit_destructure）。
   实测踩过：地图页写了 const { cw, ch } = this.box，而 box 的键是 w/h，
   于是 cw/ch 是 undefined，一次缩放把平移量算成 NaN，之后每帧都空白——
   浏览器不报错，只有真的滚一下滚轮才知道。
5. computed 项不许写成带参函数（模板里当方法调会炸）。
6. 每个 JS 文件必须能被 node 解析（见 audit_syntax）。
   实测踩过：refreshAll 里两个 const jobs 撞名，页面只剩 index.html 那个静态占位，
   "正在读取房间与分片状态"永远转不完——Go 单测和接口探针全绿，只有真打开浏览器才看得见。
7. import 进来的名字必须真的被对侧导出（见 audit_imports）。
   实测踩过：rel 从 fmt.js 里 import（它其实在 store.js），ESM 链接期就失败，
   整页不挂载，控制台只留一行。
8. 模板里当函数调的裸名字必须在组件实例上（见 audit_template_calls）。
   实测踩过：placeOf 写成模块级函数，模板取不到，那一栏整块渲染不出来，
   只弹三句"placeOf is not a function"。免构建 Vue 没有编译器帮忙解析作用域。
9. 组件里调自己的方法必须写 this.（见 audit_bare_method_calls）。
   第八条的镜像：把 placeOf 挪进 methods 后，模板那几处对了，JS 里那句忘了加点。
10. 模板里写死的图标名必须在 icons.js 真有其名（见 audit_icon_names）。
    拼错一个字母不会报错，只会让那一块什么都不显示——这一轮给二十来个卡片标题
    和统计块配了图，手打的图标名比 JS 名字更容易错。
11. 返回值的 computed 不许当函数调，同名也不许同时出现在 computed 和 methods
    （见 audit_value_computed_calls，第五条的反方向）。
    实测踩过：账号页 roleDict 是 computed，三处调用点写成 roleDict()，
    界面上的表现是"这一页一张卡都没有"，和夹具空了长得一模一样。
12. mixins: [X] 里的 X 必须在本文件里 import 过或声明过（见 audit_undefined_option_refs）。
    实测踩过：总览页漏了 import { RoomAccess }，模块求值阶段就炸，
    连登录页都渲染不出来，控制台只留一行 ReferenceError。
    第七条管的是"import 了但对侧没导出"，这一条补上反方向。

用法：python .tools/ui_audit.py
"""
import io
import os
import re
import sys

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ASSETS = os.path.join(ROOT, "internal", "webui", "assets")
JS_DIR = os.path.join(ASSETS, "js")


def read(p):
    return io.open(p, encoding="utf-8").read()


def walk_js():
    files = [os.path.join(ASSETS, "app.js")]
    for root, _d, fs in os.walk(JS_DIR):
        files += [os.path.join(root, f) for f in fs if f.endswith(".js")]
    return sorted(files)


def classes(value):
    """从 class 属性值里取出纯选择器名；插值和表达式片段一律丢掉。"""
    value = re.sub(r"\{\{[^}]*\}\}", " ", value)
    out = []
    for tok in value.split():
        if any(c in tok for c in ".():?"):
            continue
        if re.match(r"^[A-Za-z][A-Za-z0-9_-]*$", tok):
            out.append(tok)
    return out


def harness_class_tokens():
    """验收脚本（.tools/*.py）里当定位钩子用过的类名。

    为什么要给第一条留这个口子：导入预览那几块上有 imp-verdict / imp-warn /
    imp-blocked / imp-shape 四枚，它们没有样式（外观全由 .note / .hint 本身给），
    存在的意义是让 ui_import_check.py 能在一片 x-note 里点到那一枚。
    按"没样式=写错了"一刀切会把这四枚删掉，脚本立刻瞎；写成豁免名单又没人维护得动。
    所以判据换成"这一枚真被某个脚本引用过"——名字拼错的那一枚不会出现在任何脚本里，
    照样报；将来脚本不再用它，它也会重新变成一条该清的哑类名。
    """
    tools = os.path.join(ROOT, ".tools")
    out = set()
    for name in sorted(os.listdir(tools)):
        if not name.endswith(".py"):
            continue
        try:
            text = read(os.path.join(tools, name))
        except OSError:
            continue
        out |= set(re.findall(r"\.([A-Za-z][A-Za-z0-9_-]*)", text))
    return out


def audit_classes():
    defined = set(re.findall(r"\.([A-Za-z][A-Za-z0-9_-]*)", read(os.path.join(ASSETS, "app.css"))))
    hooks = harness_class_tokens()
    used = {}
    files = [os.path.join(ASSETS, "index.html")] + walk_js()
    for f in files:
        text = read(f)
        # 静态 class="..."
        for m in re.findall(r'(?<![:\w])class="([^"]*)"', text):
            for tok in classes(m):
                used.setdefault(tok, os.path.relpath(f, ROOT))
        # :class 表达式里写死的字面量（'danger'、'on'）同样是选择器
        for expr in re.findall(r'(?<![\w]):class="([^"]*)"', text):
            # 先去掉比较运算里的字面量：theme === 'dark' 中的 'dark' 是主题名，不是类名
            expr = re.sub(r"[=!]==?\s*'[^']*'", " ", expr)
            # 再整段丢掉函数调用：mark('displayName') 里的字符串是参数（字段名），
            # 不是这个元素会挂上的类；函数返回什么，静态解析本来就看不出来。
            expr = re.sub(r"[A-Za-z_$][\w$]*\s*\([^()]*\)", " ", expr)
            for lit in re.findall(r"'([^']*)'", expr):
                for tok in classes(lit):
                    used.setdefault(tok, os.path.relpath(f, ROOT))
    return sorted(k for k in used if k not in defined and k not in hooks), len(used)


def audit_exports():
    """入口链上的"生命周期导出"有没有人调用。

    只盯 init*/boot*/start*/stop*/setup*/register*/apply*/mount* 这类名字：
    它们存在的唯一意义就是"被别人调一次"，没人调就等于功能整个是空的，
    而界面看起来完全正常。普通工具函数（fmt.bytes 这种）留给后面的页面用，
    现在没人 import 是计划内的事，报出来只会变成没人看的噪音。
    """
    files = walk_js()
    texts = {f: read(f) for f in files}
    all_text = "\n".join(texts.values())
    dead = []
    for f, text in texts.items():
        for m in re.finditer(r"export\s+(?:async\s+)?function\s+([A-Za-z0-9_]+)\s*\(", text):
            name = m.group(1)
            if not re.match(r"^(init|boot|start|stop|setup|register|apply|mount)", name):
                continue
            # 声明之外再出现一次就算有人用：本文件里调用、别处 import、别处 ns.name 都行。
            uses = len(re.findall(r"\b%s\s*\(" % re.escape(name), all_text))
            declared = len(re.findall(r"export\s+(?:async\s+)?function\s+%s\s*\(" % re.escape(name), all_text))
            if uses <= declared:
                dead.append((os.path.relpath(f, ROOT), name))
    return dead


def audit_emits():
    """组件声明的 emits 必须有人在模板里 @绑上。

    实测踩过：快照页的"确认恢复"按钮 emit('restore-run')，页面模板只绑了另外六个事件，
    于是点下去什么也不发生——不报错、不留日志，只有真点一次才知道。
    免构建 Vue 没有 SFC 编译器帮忙查这个，就在这里查：声明了却全项目没人绑的，就是死按钮。
    """
    files = walk_js()
    all_text = "\n".join(read(f) for f in files)
    dead = []
    for f in files:
        text = read(f)
        for m in re.finditer(r"emits:\s*\[([^\]]*)\]", text):
            for ev in re.findall(r'"([^"]+)"', m.group(1)):
                if ("@%s=" % ev) not in all_text:
                    dead.append((os.path.relpath(f, ROOT), ev))
    return dead


PAIRS = {"(": ")", "[": "]", "{": "}"}
CLOSES = set(")]}")


def skip_string(text, i):
    """text[i] 是引号，返回串尾的下一位。"""
    q = text[i]
    i += 1
    while i < len(text):
        if text[i] == "\\":
            i += 2
            continue
        if text[i] == q:
            return i + 1
        i += 1
    return i


def span(text, i):
    """text[i] 是左括号，返回配对的右括号下标；没配上返回 -1。"""
    depth = 0
    j = i
    while j < len(text):
        c = text[j]
        if c in "\"'`":
            j = skip_string(text, j)
            continue
        if c in PAIRS:
            depth += 1
        elif c in CLOSES:
            depth -= 1
            if depth == 0:
                return j
        j += 1
    return -1


def obj_keys(text, i):
    """取 text[i]=='{' 这一层的键名，返回 (键集合, 可信吗)。

    可信吗=False 出现在展开（...x）和计算键（[k]: v）上：这两种写法下键集合
    静态算不出来，宁可整条跳过，也不要报一个不存在的问题。
    """
    end = span(text, i)
    if end < 0:
        return set(), False
    body = text[i + 1:end]
    parts, cur, depth, j = [], "", 0, 0
    while j < len(body):
        c = body[j]
        if c in "\"'`":
            e = skip_string(body, j)
            cur += body[j:e]
            j = e
            continue
        if c in PAIRS:
            depth += 1
        elif c in CLOSES:
            depth -= 1
        if c == "," and depth == 0:
            parts.append(cur)
            cur = ""
        else:
            cur += c
        j += 1
    parts.append(cur)

    keys, ok = [], True
    for p in parts:
        s = p.strip()
        if not s:
            continue
        m = re.match(r"^(['\"])(.*?)\1\s*:", s)
        if m:
            keys.append(m.group(2))
            continue
        m = re.match(r"^([A-Za-z_$][\w$]*)\s*(?::|\()", s)
        if m:
            keys.append(m.group(1))
            continue
        if re.match(r"^[A-Za-z_$][\w$]*$", s):
            keys.append(s)
            continue
        ok = False
    return set(keys), ok


def literal_keys_for_prop(text, name):
    """data 里 `name: { ... }` 和本文件里 `this.name = { ... }` 的键合集；拿不到返回 None。"""
    pats = [r"^\s*%s:\s*\{" % re.escape(name), r"this\.%s\s*=\s*\{" % re.escape(name)]
    keys, found, trusted = set(), False, True
    for pat in pats:
        for m in re.finditer(pat, text, re.M):
            found = True
            k, ok = obj_keys(text, m.end() - 1)
            keys |= k
            trusted = trusted and ok
    return keys if (found and trusted) else None


def literal_keys_for_method(text, name):
    """本文件里 `name(...) { ... }` 的每个 return 都是对象字面量时，返回它们的键合集。"""
    m = re.search(r"\n\s*%s\s*\([^()]*\)\s*\{" % re.escape(name), text)
    if not m:
        return None
    end = span(text, m.end() - 1)
    if end < 0:
        return None
    body = text[m.end():end]
    keys, any_returned = set(), False
    for r in re.finditer(r"\breturn\b", body):
        j = r.end()
        while j < len(body) and body[j] in " \t\n":
            j += 1
        if j >= len(body):
            return None
        if body[j] == "{":
            k, ok = obj_keys(body, j)
            if not ok:
                return None
            keys |= k
            any_returned = True
        elif body[j] != ";":
            return None
        # 裸 return; 是提前退出，解构方拿到的是 undefined，本来就轮不到键名判断
    return keys if any_returned else None


DESTRUCTURE = re.compile(r"(?:const|let|var)\s*\{([^{}]*)\}\s*=\s*this\.([A-Za-z0-9_]+)(\(\s*\))?(?![.\w])")


def audit_destructure():
    bad = []
    for f in walk_js():
        text = read(f)
        for m in DESTRUCTURE.finditer(text):
            target, is_call = m.group(2), bool(m.group(3))
            keys = literal_keys_for_method(text, target) if is_call else literal_keys_for_prop(text, target)
            if keys is None:
                continue
            for raw in m.group(1).split(","):
                tok = raw.strip()
                if not tok:
                    continue
                name = re.split(r"[:=]", tok)[0].strip()
                if not re.match(r"^[A-Za-z_$][\w$]*$", name):
                    continue
                if name not in keys:
                    line = text[:m.start()].count("\n") + 1
                    bad.append((os.path.relpath(f, ROOT), line,
                                "%s()" % target if is_call else target, name))
    return bad


def computed_with_params():
    """computed 里写成 name(k) {…} 的项。

    实测踩过：任务页把 viaNote(k) 放进了 computed，Vue 调它时不传参，
    模板里 viaNote(k) 就报"不是函数"，整页空白只留一句红字。
    这一类要么写成 computed 返回一个函数（otherHref 那种），要么挪进 methods。
    """
    bad = []
    for f in walk_js():
        text = read(f)
        for m in re.finditer(r"\bcomputed:\s*\{", text):
            end = span(text, m.end() - 1)
            if end < 0:
                continue
            body = text[m.end():end]
            depth, j = 0, 0
            while j < len(body):
                c = body[j]
                if c in "\"'`":
                    j = skip_string(body, j)
                    continue
                if c in PAIRS:
                    depth += 1
                elif c in CLOSES:
                    depth -= 1
                if depth == 0:
                    mm = re.match(r"([A-Za-z_$][\w$]*)\s*\(([^()]*)\)\s*\{", body[j:])
                    if mm:
                        if mm.group(2).strip():
                            line = text[:m.end() + j].count("\n") + 1
                            bad.append((os.path.relpath(f, ROOT), line, mm.group(1), mm.group(2).strip()))
                        j += mm.end() - 1
                        continue
                j += 1
    return bad


OPTION_BLOCK = re.compile(r"\b(computed|methods):\s*\{")
MEMBER_DECL = re.compile(r"\n\s*([A-Za-z_$][\w$]*)\s*\(([^()]*)\)\s*\{")


def block_members(text, lo, hi):
    """对象字面量第一层里 `name(args) {…}` 形式的成员：[(name, 函数体起止)]。

    命中一个成员后就从它的收尾花括号之后接着扫：函数体里再写的 `foo() {…}`
    是嵌套的，不能算成第一层。
    """
    out = []
    m = MEMBER_DECL.search(text, lo, hi)
    while m:
        open_idx = m.end() - 1
        end = span(text, open_idx)
        if end < 0:
            break
        out.append((m.group(1), open_idx, end))
        m = MEMBER_DECL.search(text, end + 1, hi)
    return out


def returns_function(text, lo, hi):
    """这个函数体交出来的是不是一副函数（computed 返回函数的写法，如 otherHref）。"""
    m = re.search(r"\breturn\s+([^\n]{0,90})", text[lo:hi])
    if not m:
        return False
    seg = m.group(1).strip()
    if seg.startswith("function"):
        return True
    return "=>" in seg.split("{", 1)[0]


def option_members(text, lo, hi):
    """这一段里 computed / methods 两个块的第一层成员。

    computed 那份记着每一项"返不返回函数"：返回函数的 computed 本来就该带括号调
    （ownerChip(c) 这种），把它们一起报出来就是假问题一堆。
    """
    got = {"computed": {}, "methods": set()}
    m = OPTION_BLOCK.search(text, lo, hi)
    while m:
        kind = m.group(1)
        open_idx = m.end() - 1
        end = span(text, open_idx)
        if end > 0:
            for name, blo, bhi in block_members(text, open_idx, end):
                if kind == "computed":
                    got["computed"][name] = returns_function(text, blo, bhi)
                else:
                    got["methods"].add(name)
        m = OPTION_BLOCK.search(text, m.end(), hi)
    return got


def audit_value_computed_calls():
    """把返回值的 computed 当函数调：JS 里 this.roleDict()，模板里 roleDict()。

    实测踩过：账号页的 roleDict 是 computed（交出来的是那三档角色数组），三处调用点
    写成了 roleDict()。运行时只报一句 "is not a function"，界面上的表现却是
    "这一页一张卡都没有"，和夹具空了长得一模一样——只有把控制台接住才分得清。
    第五条查的是反方向（computed 写成带参函数），这一条补上正方向。
    """
    bad = []
    for f in walk_js():
        text = read(f)
        masked = mask_noise(text)
        spans = component_spans(text)
        cache = {}

        def members_of(idx):
            for lo, hi in spans:
                if lo <= idx < hi:
                    if lo not in cache:
                        cache[lo] = option_members(text, lo, hi)
                    return cache[lo]
            return None

        for m in re.finditer(r"this\.([A-Za-z_$][\w$]*)\s*\(", masked):
            opts = members_of(m.start())
            if not opts:
                continue
            name = m.group(1)
            if name in opts["computed"] and not opts["computed"][name]:
                bad.append((os.path.relpath(f, ROOT), text[:m.start()].count("\n") + 1,
                            "this.%s()" % name, "它是 computed，取值不要加括号"))
        for body, base in template_spans(text):
            opts = members_of(base)
            if not opts:
                continue
            value = {n for n, fn in opts["computed"].items() if not fn}
            exprs = EXPR_IN_MUSTACHE.findall(body)
            for am in EXPR_IN_ATTR.finditer(body):
                exprs += [g for g in am.groups()[1:] if g]
            for expr in exprs:
                for callee in BARE_CALL.findall(expr_of(expr)):
                    if callee not in value:
                        continue
                    at = base + body.find(expr)
                    bad.append((os.path.relpath(f, ROOT), text[:at].count("\n") + 1,
                                "%s()" % callee, "模板里它是值，加括号会把这一栏整个弄没"))
        for lo, hi in spans:
            opts = option_members(text, lo, hi)
            for name in sorted(set(opts["computed"]) & opts["methods"]):
                bad.append((os.path.relpath(f, ROOT), text[:lo].count("\n") + 1,
                            name, "同名出现在 computed 和 methods 里，模板取到哪个看运气"))
    seen, out = set(), []
    for item in bad:
        if item not in seen:
            seen.add(item)
            out.append(item)
    return out


MIXINS_LIST = re.compile(r"\bmixins:\s*\[([^\]]*)\]")
IMPORT_ALL = re.compile(r"import\s+(?:([A-Za-z_$][\w$]*)|\*\s*as\s+([A-Za-z_$][\w$]*))\s+from")
TOP_DECL = re.compile(r"^(?:export\s+)?(?:async\s+)?(?:const|let|var|function|class)\s+([A-Za-z_$][\w$]*)", re.M)
DESTRUCT_DECL = re.compile(r"^(?:export\s+)?(?:const|let|var)\s*\{([^}]*)\}\s*=", re.M)


def local_names(text):
    """这个文件里"叫得出名字"的东西：import 进来的、顶层声明的、解构出来的。"""
    have = set()
    for m in IMPORT_NAMED.finditer(text):
        for raw in m.group(1).split(","):
            part = raw.strip()
            if part:
                have.add(part.split(" as ")[-1].strip())
    for m in IMPORT_ALL.finditer(text):
        have.add(m.group(1) or m.group(2))
    have |= set(TOP_DECL.findall(text))
    for m in DESTRUCT_DECL.finditer(text):
        have |= {p.strip().split(":")[-1].strip() for p in m.group(1).split(",") if p.strip()}
    return have


def audit_undefined_option_refs():
    """mixins: [X] 里的 X 必须在本文件里 import 过或声明过。

    实测踩过：总览页的 ShardRow 写了 mixins: [RoomAccess] 却漏了 import 那一行。
    这一条比第八条更致命——它是在模块求值阶段炸的，整个应用连登录页都渲染不出来，
    而控制台只留一行 ReferenceError，截图上看着就是"页面没数据"。
    第七条管的是"import 了但对侧没导出"，反方向（用了却没 import）当时没人管。
    """
    bad = []
    for f in walk_js():
        text = mask_noise(read(f))
        raw = read(f)
        have = local_names(raw)
        for m in MIXINS_LIST.finditer(text):
            for name in re.findall(r"[A-Za-z_$][\w$]*", m.group(1)):
                if name not in have:
                    bad.append((os.path.relpath(f, ROOT), raw[:m.start()].count("\n") + 1, name))
    return bad


def audit_syntax():
    """每个 .js 都过一遍 node 的语法检查。

    为什么值得单列一条：语法错的模块浏览器只在控制台留一行，页面上最多停在
    index.html 那个"正在读取房间与分片状态"的静态占位——Go 的单测、接口探针都看不出，
    只有真人打开才发现整页没挂载。这一条就是给那种失败兜底的。
    """
    import shutil
    import subprocess
    import tempfile

    if not shutil.which("node"):
        return [], "（本机没有 node，跳过语法检查）"
    d = tempfile.mkdtemp(prefix="dstpanel-js-")
    bad = []
    try:
        for p in walk_js():
            tmp = os.path.join(d, "m-%s.mjs" % os.path.basename(p))
            shutil.copyfile(p, tmp)
            r = subprocess.run(["node", "--check", tmp], capture_output=True,
                               text=True, encoding="utf-8", errors="replace")
            if r.returncode != 0:
                first = (r.stderr or "").strip().splitlines()
                bad.append((os.path.relpath(p, ROOT), first[-1] if first else "node --check 失败"))
    finally:
        shutil.rmtree(d, ignore_errors=True)
    return bad, ""


IMPORT_NAMED = re.compile(r"import\s*\{([^}]*)\}\s*from\s*[\"']([^\"']+)[\"']")
EXPORT_NAMES = re.compile(r"export\s+(?:async\s+)?(?:function|const|let|var|class)\s+([A-Za-z_$][\w$]*)")
EXPORT_LIST = re.compile(r"export\s*\{([^}]*)\}")


def exports_of(text):
    names = set(EXPORT_NAMES.findall(text))
    for lst in EXPORT_LIST.findall(text):
        for part in lst.split(","):
            part = part.strip()
            if not part:
                continue
            # `export { a as b }`：外面看到的是 b。
            names.add(part.split(" as ")[-1].strip() if " as " in part else part)
    return names


def audit_imports():
    """import { x } from "./y.js" 里的 x 必须真的被 y.js 导出。

    为什么单列一条：ES module 少一个导出不是"这一格空着"，而是整条 import 链
    链接失败——页面停在 index.html 那句静态占位上，控制台才有一行红字。
    实测踩过：rel 从 fmt.js 里 import（它其实在 store.js），Go 单测、接口探针、
    node --check 全绿，只有浏览器知道这一页根本没挂载。
    """
    bad = []
    for f in walk_js():
        text = read(f)
        for m in IMPORT_NAMED.finditer(text):
            spec = m.group(2)
            if not spec.startswith("."):
                continue          # vue 这类走 vendor 全局，不在本仓的导出表里
            target = os.path.normpath(os.path.join(os.path.dirname(f), spec))
            if not os.path.exists(target):
                line = text[:m.start()].count("\n") + 1
                bad.append((os.path.relpath(f, ROOT), line, spec, "没有这个文件", ""))
                continue
            have = exports_of(read(target))
            for raw in m.group(1).split(","):
                name = raw.strip().split(" as ")[0].strip()
                if not name or not re.match(r"^[A-Za-z_$][\w$]*$", name):
                    continue
                if name not in have:
                    line = text[:m.start()].count("\n") + 1
                    bad.append((os.path.relpath(f, ROOT), line, spec, name,
                                "；这个文件导出的是：%s" % ", ".join(sorted(have)[:8])))
    return bad


# 模板表达式里能直接调用的名字：Vue 3 的运行时编译器把模板编译成 with(this){…}，
# 所以只有挂在组件实例上的成员才取得到，JS 自带的这几个是环境给的。
TEMPLATE_GLOBALS = {
    "Number", "String", "Boolean", "Math", "JSON", "Date", "Object", "Array", "RegExp",
    "parseInt", "parseFloat", "isNaN", "isFinite", "encodeURIComponent",
    "decodeURIComponent", "Promise", "Set", "Map", "Symbol", "Error",
    # Vue 给实例的这几个带 $ 的成员不在组件的 methods 里声明，但模板确实取得到。
    "$emit", "$nextTick", "$forceUpdate", "$watch",
}
# 从模板里抠出表达式：插值、绑定属性、指令值三类。
EXPR_IN_MUSTACHE = re.compile(r"\{\{([^{}]*)\}\}")
# :style 单独排掉：那一段的值是 CSS（color-mix(in srgb, var(--x) 14%) 这种），
# 不是 JS 表达式，按 JS 去抠函数名只会得到一串假问题。
EXPR_IN_ATTR = re.compile(r"(?<![\w-])[:@](?!style\b)([\w.-]+)\s*=\s*\"([^\"]*)\""
                          r"|v-[\w.-]+(?:\.[\w-]+)*\s*=\s*\"([^\"]*)\"")
BARE_CALL = re.compile(r"(?<![.\w$])([A-Za-z_$][\w$]*)\s*\(")
METHOD_DECL = re.compile(r"^[ \t]{2,}(?:async\s+|function\s+)?([A-Za-z_$][\w$]*)\s*(?:\([^()]*\)\s*)?\{", re.M)
# 组件方法里裸调自己的另一个方法：Vue 不报错，运行时 ReferenceError。
# 前一行是"声明"（`foo() {`），所以只找后面那些调用点。
BARE_METHOD_CALL = re.compile(r"(?<![.\w$])([A-Za-z_$][\w$]*)\s*\(")
THIS_OR_DOT = re.compile(r"(?:this|_this|self|\w+)\s*\.\\s*$")
OBJ_KEY = re.compile(r"([A-Za-z_$][\w$]*)\s*:")
# 收尾用零宽断言：`dateTime, ago,` 这样连着写的两项，若把逗号吃掉就只剩第一项。
SHORTHAND = re.compile(r"[,{]\s*([A-Za-z_$][\w$]*)\s*(?=[,，}\s])")
IMPORT_STMT = re.compile(r"^\s*import\b[^;\n]*", re.M)


def template_spans(text):
    """每个 `template: `…`` 的内容与它在文件里的起始下标。"""
    out = []
    for m in re.finditer(r"template:\s*`", text):
        start = m.end() - 1
        j, end = start + 1, -1
        while j < len(text):
            if text[j] == "\\":
                j += 2
                continue
            if text[j] == "`":
                end = j
                break
            j += 1
        if end > 0:
            out.append((text[start + 1:end], start))
    return out


def instance_names(text):
    """这个文件的组件实例上叫得出什么名字。

    口径故意放宽（把全文件的对象键都算进来）：宁可漏报几个真的写错的，
    也不要报出一堆"其实没问题"的——这条一旦变吵，没人会再看它。
    反过来，模块级的 function / const 一定不算：那正是这一条要抓的东西。
    import 进来的名字也不算：它只在模块里可见，模板照样取不到
    （accounts.js 那行 `methods: { dateTime, ago }` 就是把 import 的名字
    显式挂回实例，挂了才用得了）。
    """
    body = IMPORT_STMT.sub("", text)
    names = set(OBJ_KEY.findall(body))
    names |= set(METHOD_DECL.findall(body))
    names |= set(SHORTHAND.findall(body))
    for m in re.finditer(r"setup\s*(?:\([^()]*\))?\s*\{", body):
        end = span(body, m.end() - 1)
        if end < 0:
            continue
        for r in re.finditer(r"return\s*\{", body[m.end():end]):
            k, _ok = obj_keys(body, m.end() + r.end() - 1)
            names |= k
    return names


def expr_of(raw):
    """一条模板属性值里真正是 JS 表达式的那一段。

    v-for / v-if 都写作「左 in 右」，左半边是 `(p, i)` 这种绑定模式，
    按函数调用去抠会得到一个不存在的 in()。取 in 之后的那一段就够。
    """
    return raw.rsplit(" in ", 1)[-1] if " in " in raw else raw


# 一个 views 文件里往往有好几个组件（房间页就有表头、向导、指令台、页面本身）。
# 模板能看见的名字是**它自己那个组件**的实例成员，不是全文件的并集——
# 上一条口径放得太宽，结果 isBusy 在别的组件里 setup 过，这一栏没挂也照样放行，
# 而界面上就是"点开源指令台，参数面板一片空白"。
TOP_STMT = re.compile(r"^(?:export\s+)?(?:const|let|var|function|class)\s", re.M)


def code_of(text, lo, hi):
    """text[lo:hi] 这一段，把其中的模板字面量挖空（HTML 里的 `foo:` 不是 JS 名字）。"""
    body = list(text[lo:hi])
    for tpl, base in template_spans(text[lo:hi]):
        for j in range(base + 1, min(base + 1 + len(tpl), len(body))):
            if body[j] != "\n":
                body[j] = " "
    return "".join(body)


def component_spans(text):
    """[(起始下标, 结束下标)]，一条顶层语句一段。"""
    starts = [m.start() for m in TOP_STMT.finditer(text)]
    return [(s, starts[i + 1] if i + 1 < len(starts) else len(text)) for i, s in enumerate(starts)]


def audit_template_calls():
    """模板里当函数调的裸名字，必须在**它所在那个组件**的实例上找得到。

    实测踩过三次：任务页把 viaNote(k) 放进 computed（Vue 不传参），房间页把
    placeOf 写成模块级函数（模板取不到，整栏渲染不出来，只弹三句
    "placeOf is not a function"），指令台里 isBusy 只在别的组件 setup 过。
    免构建 Vue 没有编译器帮忙解析作用域，这类错误 node --check 全绿，
    只有真点开那一栏才看得见。
    """
    bad = []
    for f in walk_js():
        text = read(f)
        spans = component_spans(text)
        cache = {}
        for body, base in template_spans(text):
            own = [(lo, hi) for lo, hi in spans if lo <= base < hi]
            if not own:                       # 顶层语句之外没有组件可言
                continue
            key = own[0]
            if key not in cache:
                cache[key] = instance_names(code_of(text, key[0], key[1]))
            names = cache[key]
            exprs = EXPR_IN_MUSTACHE.findall(body)
            for m in EXPR_IN_ATTR.finditer(body):
                exprs += [g for g in m.groups()[1:] if g]
            for expr in exprs:
                for callee in BARE_CALL.findall(expr_of(expr)):
                    if callee in TEMPLATE_GLOBALS or callee in names:
                        continue
                    # 报模板起始行等于没报：一个组件的模板有几百行。用表达式在模板里
                    # 的第一次出现定位到行，改的时候才跳得过去。
                    line = text[:base + body.find(expr) + 1].count("\n") + 1
                    bad.append((os.path.relpath(f, ROOT), line, callee))
    seen, out = set(), []
    for item in bad:
        if item not in seen:
            seen.add(item)
            out.append(item)
    return out


# 关键字后面紧跟着 `(`（`if (x)`、`return (a || b)`），会被"名字+左括号"抠成一个
# 不存在的函数名。这张表把它们挡掉。
JS_KEYWORDS = {"if", "for", "while", "switch", "catch", "return", "typeof", "new", "delete",
               "void", "do", "else", "case", "in", "of", "function", "yield", "await"}

# 注释和字符串里的 `foo(` 不是调用：第九条头一遍就跑在 logs.js 的注释上
# （"一条 apply()"），连着报两条假的。逐字符走一遍，把注释、字符串、模板串、
# 正则字面量整段抹成空格——长度和换行位置都不动，行号与偏移仍对得上原文。
REGEX_OK_AFTER = "(,=:[!&|?{};+-*%~^<>\n"


def regex_end(text, i):
    """text[i] 是正则的起始斜杠，返回结尾的下一位；一行内没闭合就当除法。"""
    j, cls, n = i + 1, False, len(text)
    while j < n:
        c = text[j]
        if c == "\\":
            j += 2
            continue
        if c == "\n":
            return i + 1
        if c == "[":
            cls = True
        elif c == "]":
            cls = False
        elif c == "/" and not cls:
            j += 1
            while j < n and text[j].isalpha():
                j += 1
            return j
        j += 1
    return n


def mask_noise(text):
    out = list(text)
    i, n, prev = 0, len(text), "\n"
    while i < n:
        c = text[i]
        end = i
        if text[i:i + 2] == "//":
            k = text.find("\n", i)
            end = n if k < 0 else k
        elif text[i:i + 2] == "/*":
            k = text.find("*/", i + 2)
            end = n if k < 0 else k + 2
        elif c in "\"'`":
            end = skip_string(text, i)
        elif c == "/" and prev in REGEX_OK_AFTER:
            end = regex_end(text, i)
        if end > i:
            for j in range(i, end):
                if out[j] != "\n":
                    out[j] = " "
            prev = c
            i = end
            continue
        if not c.isspace():
            prev = c
        i += 1
    return "".join(out)


# 对象字面量里的方法简写（`watch: { cluster() { … } }`）也是"名字 + ("，
# 但 METHOD_DECL 只认行首缩进，同一行紧跟在 `{` 后面的那种会漏。
METHOD_DECL_INLINE = re.compile(r"[{,]\s*(?:async\s+)?([A-Za-z_$][\w$]*)\s*\([^()]*\)\s*\{")


def audit_bare_method_calls():
    """组件里裸调本文件某个方法名（少写 this.）。

    实测踩过：placeOf 从模块函数挪进 methods 之后，模板那几处跟着改成了
    this 上的成员，但 send() 里那句 `placeOf(this.shard)` 忘了加点——
    模板取不到它和 JS 里少个 this，是同一个坑的两面，所以两条一起立。
    只认"本文件确实有个同名的方法声明"：这样不会去管 Number()、map() 这些，
    也不会替整个 JS 语言做作用域分析。
    """
    bad = []
    for f in walk_js():
        text = read(f)
        # 模板整段剔掉：那里本来就不写 this，属于第八条的管辖区。
        masked = mask_noise(text)
        for body, base in template_spans(text):
            masked = masked[:base] + " " * (len(body) + 2) + masked[base + len(body) + 2:]
        methods = set(METHOD_DECL.findall(masked)) | set(METHOD_DECL_INLINE.findall(masked))
        if not methods:
            continue
        # 声明本身（`load() {`）也是"名字 + ("，先按偏移把它排除掉。
        decls = set(m.start(1) for m in METHOD_DECL.finditer(masked))
        decls |= set(m.start(1) for m in METHOD_DECL_INLINE.finditer(masked))
        for m in BARE_METHOD_CALL.finditer(masked):
            name = m.group(1)
            # 一个缩进两格、后面紧跟花括号的 `if (x) {`，会被 METHOD_DECL 当成方法声明，
            # 于是所有 `if (...)` 都成了"裸调一个叫 if 的方法"。关键字先出局。
            if name in JS_KEYWORDS or name not in methods or m.start(1) in decls:
                continue
            before = masked[:m.start(1)].rstrip()
            if before.endswith((".", "function", "async", "return", "case", "new", "typeof", "*")):
                continue
            line = text[:m.start(1)].count("\n") + 1
            bad.append((os.path.relpath(f, ROOT), line, name))
    seen, out = set(), []
    for item in bad:
        if item not in seen:
            seen.add(item)
            out.append(item)
    return out


ICONS_FILE = os.path.join(JS_DIR, "icons.js")

# 图标名有三种写法：模板里的静态属性（`<x-icon name="gear">`、`<x-hint icon="info">`）、
# JS 数据里的字面量（导航表和总览事实条是 `icon: "pulse"` 这样喂给 :icon 的），
# 以及三元表达式和指令图标映射表里的字面量（`:name="ok ? 'check' : 'copy'"`、
# CMD_ICON 那张表）——这一种抠的是表达式里的引号串。
# 前面那个 `(?<!:)` 是必需的：`:name="icon"`（动态绑组件自己的 prop）里的 `name="` 会被
# 当成静态图标名，于是把 prop 的名字 `icon` 报成"图标库里没这个名字"。
X_ICON_NAME = re.compile(r'<x-icon\b[^>]*?(?<![:\w])name="([A-Za-z][\w-]*)"', re.S)
ICON_ATTR = re.compile(r'(?<![:\w])(?:icon|glyph)="([A-Za-z][\w-]*)"')
ICON_VALUE = re.compile(r'(?<![:\w])(?:icon|glyph)\s*:\s*"([A-Za-z][\w-]*)"')
DYN_ICON_ATTR = re.compile(r'(?<!\w):(?:name|icon)="([^"]*)"')
ICON_MAP_STMT = re.compile(r'\bconst\s+[A-Za-z_$][\w$]*ICON[S]?\s*=\s*\{')
# `tint:` 那一档是色板名（tint-teal），不是图标名——GROUP_ICON 里图标和配色并排写，
# 整张表的字符串值一起当图标名查就会假警报。
ICON_MAP_VALUE = re.compile(r'(?:^|[{,\s])(?!tint\s*:)[A-Za-z_$][\w$]*\s*:\s*"([A-Za-z][\w-]*)"')


def icon_catalog():
    """icons.js 里真有的图标名。

    自己抠两格缩进的 `name: [`，不用 obj_keys：那个函数按逗号切项，
    而这里每个图标前面都顶着一行 `// ---- 动作 ----` 分组注释，
    注释会黏在第一项前面，导致整组第一个名字被丢掉（第一次跑就漏了 dashboard、search）。
    """
    text = read(ICONS_FILE)
    m = re.search(r"export const ICONS\s*=\s*\{", text)
    if not m:
        return set()
    end = span(text, text.index("{", m.end() - 1))
    body = text[m.end(): end if end > 0 else len(text)]
    return set(re.findall(r"^[ \t]{2}([A-Za-z][A-Za-z0-9_]*):\s*\[", body, re.M))


def audit_icon_names():
    """写死的图标名必须在图标库里真有其名。

    拼错一个字母不会报错：Icon 组件退化成 "?" 并留一句 console.warn，
    截图上就是那个位置少了个图，很容易被当成"设计本来就这样"。
    这一轮给二十来个卡片标题和统计块手打了图标名，靠肉眼核对不现实。
    """
    known = icon_catalog()
    if not known:
        # 图标库没解析出来就别装作查过了，直接全报——漏报比误报更坑这条。
        return [(os.path.relpath(ICONS_FILE, ROOT), 1, "图标库没解析出任何名字")]
    bad = []
    for f in walk_js():
        if os.path.abspath(f) == os.path.abspath(ICONS_FILE):
            continue
        text = read(f)
        for rx in (X_ICON_NAME, ICON_ATTR, ICON_VALUE):
            for m in rx.finditer(text):
                name = m.group(1)
                if name in known:
                    continue
                line = text[:m.start(1)].count("\n") + 1
                bad.append((os.path.relpath(f, ROOT), line, name))
        # 三元表达式里的图标名（`:name="ok ? 'check' : 'copy'"`）。
        # 比较运算那一侧的字面量不是图标名：`state.theme === 'dark' ? 'sun' : 'moon'`
        # 里的 'dark' 是主题名，跟着报就成假警报了。
        for m in DYN_ICON_ATTR.finditer(text):
            expr = re.sub(r"'[a-z][a-z_-]*'\s*[=!]==?|[=!]==?\s*'[a-z][a-z_-]*'", " ", m.group(1))
            for lit in re.finditer(r"'([a-z][a-z_-]*)'", expr):
                name = lit.group(1)
                if name in known:
                    continue
                line = text[:m.start(1) + lit.start(1)].count("\n") + 1
                bad.append((os.path.relpath(f, ROOT), line, name))
        # 名字里带 ICON 的映射表（CMD_ICON 那种：键是指令名，值是图标名）。
        # 地图页自己有一张画布图标表（`const ICONS = { berry: (ctx…) }`），
        # 那里的 SUB_ICON / KIND_ICON 指的是那些画法，跟 icons.js 无关，整文件跳过。
        if not re.search(r"^const ICONS\s*=", text, re.M):
            for m in ICON_MAP_STMT.finditer(text):
                end = span(text, text.index("{", m.end() - 1))
                if end < 0:
                    continue
                for vm in ICON_MAP_VALUE.finditer(text[m.end():end]):
                    name = vm.group(1)
                    if name in known:
                        continue
                    line = text[:m.end() + vm.start(1)].count("\n") + 1
                    bad.append((os.path.relpath(f, ROOT), line, name))
    seen, out = set(), []
    for item in bad:
        if item not in seen:
            seen.add(item)
            out.append(item)
    return out


WORLD_DICT = os.path.join(ROOT, "internal", "dst", "assets", "world_setting.json")
WORLD_ICONS = os.path.join(JS_DIR, "worldicons.js")


def world_icon_coverage():
    """世界规则那 189 项里，图标查表精确覆盖到多少项（只报数，不算失败）。

    为什么要报：查表逻辑是"没命中就用分组默认图标"，所以键名拼错不会有任何动静——
    那一项只是静悄悄换成一个通用图，界面看着仍然"每项都有图标"。
    字典（internal/dst/assets/world_setting.json）是引擎自己的东西，Klei 加字段时
    这一行数字会掉，那才是提醒我该补表的信号。
    """
    import json
    if not (os.path.exists(WORLD_DICT) and os.path.exists(WORLD_ICONS)):
        return None
    text = read(WORLD_ICONS)

    def table(name, obj=False):
        m = re.search(r"const %s\s*=\s*\{" % name, text)
        if not m:
            return {}
        end = span(text, text.index("{", m.end() - 1))
        body = text[m.end(): end if end > 0 else len(text)]
        if obj:
            return dict(re.findall(r'([A-Za-z_]\w*)\s*:\s*\{\s*icon:\s*"([A-Za-z][\w-]*)"', body))
        return dict(re.findall(r'([A-Za-z_]\w*)\s*:\s*"([A-Za-z][\w-]*)"', body))

    exact, parts = table("FIELD_ICON"), table("FIELD_ICON_PART")
    groups = table("GROUP_ICON", obj=True)
    ordered = sorted(parts, key=lambda p: -len(p))
    data = json.loads(read(WORLD_DICT))
    hit = total = 0
    no_group = set()
    for terrain in ("forest", "cave"):
        for g in data.get("zh", {}).get(terrain, {}).get("groups", []):
            gk = g.get("key", "")
            if gk not in groups:
                no_group.add(gk)
            for it in g.get("items", []):
                key = it.get("key", "")
                total += 1
                if key in exact or any(p in key for p in ordered):
                    hit += 1
    return {"hit": hit, "total": total, "no_group": sorted(no_group)}


def main():
    missing, total = audit_classes()
    print("一、模板 class 是否都有样式（共 %d 个）" % total)
    for k in missing:
        print("   缺少定义：.%s" % k)
    if not missing:
        print("   全部有定义")

    dead = audit_exports()
    print("二、导出符号是否有人用")
    for f, name in dead:
        print("   没人 import：%s -> %s()" % (f, name))
    if not dead:
        print("   没有孤立导出")

    unbound = audit_emits()
    print("三、组件声明的 emits 是否有人绑")
    for f, ev in unbound:
        print("   没人绑：%s -> @%s（点它不会有反应）" % (f, ev))
    if not unbound:
        print("   全部有绑")

    mismatch = audit_destructure()
    print("四、从 this 上解构的键是否真的存在")
    for f, line, target, name in mismatch:
        print("   %s:%d 里 %s 没有这个键：%s" % (f, line, target, name))
    if not mismatch:
        print("   没有解构错的键")

    arity = computed_with_params()
    print("五、computed 项有没有被写成带参函数（模板里当方法调就会炸）")
    for f, line, name, params in arity:
        print("   %s:%d 的 computed %s(%s)：Vue 不传参，模板里 %s(...) 报「不是函数」" % (f, line, name, params, name))
    if not arity:
        print("   没有这种写法")

    bad, note = audit_syntax()
    print("六、每个 JS 文件能不能被 node 解析（语法错会让整页停在静态占位，别的手段看不出来）")
    for f, why in bad:
        print("   %s：%s" % (f, why))
    if not bad:
        print("   全部解析通过%s" % note)

    missing_import = audit_imports()
    print("七、import 的名字在对侧文件里到底导没导出（少一个导出＝整页不挂载）")
    for f, line, spec, name, hint in missing_import:
        print("   %s:%d 从 %s 里拿 %s，但它没导出%s" % (f, line, spec, name, hint))
    if not missing_import:
        print("   全部对得上")

    calls = audit_template_calls()
    print("八、模板里当函数调的裸名字，在组件实例上找不找得到")
    for f, line, name in calls:
        print("   %s:%d 的模板调了 %s()，但它不在组件上（模块级函数模板取不到）" % (f, line, name))
    if not calls:
        print("   全部挂在组件上")

    bare = audit_bare_method_calls()
    print("九、组件里调自己的方法有没有写 this.（少一个点＝运行时 ReferenceError）")
    for f, line, name in bare:
        print("   %s:%d 裸调 %s()，它是本文件某个组件的方法，得写 this.%s()" % (f, line, name, name))
    if not bare:
        print("   没有漏掉 this 的")

    icons = audit_icon_names()
    print("十、写死的图标名在 icons.js 里到底有没有（拼错只会少个图，不报错）")
    for f, line, name in icons:
        print("   %s:%d 用了「%s」，图标库里没这个名字（那一块会空掉）" % (f, line, name))
    if not icons:
        print("   全部对得上")
    cov = world_icon_coverage()
    if cov is None:
        print("   （世界规则字典不在本地，跳过覆盖率）")
    elif cov["total"]:
        print("   世界规则图标查表覆盖 %d / %d 项，其余走分组默认图标%s" % (
            cov["hit"], cov["total"],
            "；这些分组没配到图标：" + "、".join(cov["no_group"]) if cov["no_group"] else ""))

    vcc = audit_value_computed_calls()
    print("十一、返回值的 computed 有没有被当函数调（第五条的反方向）")
    for f, line, call, why in vcc:
        print("   %s:%d 的 %s：%s" % (f, line, call, why))
    if not vcc:
        print("   没有这种写法")

    undef = audit_undefined_option_refs()
    print("十二、mixins 里引用的名字在本文件里到底有没有（漏一行 import＝整页白屏）")
    for f, line, name in undef:
        print("   %s:%d 用了 %s，但这一页既没 import 它也没声明它" % (f, line, name))
    if not undef:
        print("   全部对得上")

    return 1 if (missing or dead or unbound or mismatch or arity or bad
                 or missing_import or calls or bare or icons or vcc or undef) else 0


if __name__ == "__main__":
    sys.exit(main())
