#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""在云机上验模组侧新增的三件事：跨分片一致性、依赖预警、模组列表的新字段。

只读为主，唯一写盘动作是在 mods/ 里造一个探针目录再删掉它——
Cluster_1 的两份 modoverrides.lua 全程不动，脚本开头结尾各取一次 sha256 来证明这点。

为什么值得单独跑一遍：api_probe 那份用的是 Windows 上的夹具文件，
云机上是引擎真实读写过的那一份（recon_mod6 实测 149 字节、没有 KLEI 头）。
解析器只有在真文件上跑通才算接上。
"""
import hashlib
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cloud_boot_shards as cb  # noqa: E402

PROBE_DIR = "PanelProbeDep"


def sha_of_modoverrides(shard):
    # KLEI 常量已经是 ~/.klei/DoNotStarveTogether，不要再叠一层。
    out = cb.remote("sha256sum \"%s/Cluster_1/%s/modoverrides.lua\" 2>/dev/null | cut -d' ' -f1"
                    % (cb.KLEI, shard)).strip().splitlines()
    return out[0] if out else ""


def ls_modoverrides_guard(before):
    after = dict((sh, sha_of_modoverrides(sh)) for sh in ("Master", "Caves"))
    cb.check("探针全程没有改动正式集群的 modoverrides.lua",
             all(before[sh] == after[sh] for sh in before),
             " ".join("%s=%s..%s" % (sh, before[sh][:8], after[sh][:8]) for sh in sorted(before)))


def main():
    cb.PW = cb.password()
    before = dict((sh, sha_of_modoverrides(sh)) for sh in ("Master", "Caves"))
    cb.check("正式集群的两份 modoverrides.lua 读得到",
             all(before[sh] for sh in before), str(before)[:160])

    cb.push_binary()
    pid = cb.start_panel()
    try:
        # 1) 模组列表的新字段：标记、依赖、以及磁盘级预警。
        code, r = cb.plain_call("GET", "/api/v1/mods")
        v = r.get("data") or {}
        rows = v.get("mods") or []
        helper = [x for x in rows if x["dir"] == "DSTPanelHelper"]
        cb.check("模组列表认出助手模组", code == 200 and len(helper) == 1, str(rows)[:200])
        # 这一条是 2026-10-31 那次真机发现的正主：助手 modinfo.lua 是一串裸赋值，
        # 面板当时只会按 `return { ... }` 读，于是云机上连自己写的 MOD 都显示成
        # "名字空白、只能按目录名识别"。要在引擎真正加载的那份文件上验名字读得出来。
        cb.check("助手 modinfo.lua 的中文名字读出来了", helper and
                 helper[0].get("modinfoOk") is True and helper[0].get("name") == "DST 管理面板助手",
                 str(helper[0])[:240])
        cb.check("版本也从 modinfo 里带出来了", helper and helper[0].get("version") == "1.0.0",
                 str(helper[0])[:200])
        cb.check("裸赋值里的 local / 长字符串没把解析带崩", helper and
                 not (helper[0].get("modinfoError") or ""), str(helper[0])[:240])
        cb.check("列表带出两个客户端标记字段", helper and
                 "clientOnly" in helper[0] and "allClientsRequire" in helper[0], str(helper[0])[:200])
        cb.check("助手自己没声明依赖，所以磁盘级预警为空",
                 helper and helper[0].get("dependencies") in (None, [])
                 and (v.get("unmetDependencies") or []) == [],
                 str(v.get("unmetDependencies"))[:160])

        # 2) 跨分片一致性：云机上这份 modoverrides 是面板写的、引擎读过的真文件。
        code, r = cb.plain_call("GET", "/api/v1/clusters/Cluster_1/mods/consistency")
        rpt = r.get("data") or {}
        shards = rpt.get("shards") or []
        cb.check("一致性接口认出两个分片", code == 200 and len(shards) == 2, str(shards)[:220])
        cb.check("两个分片都真的解析出了启用的助手模组（不是空表糊过去）",
                 all("DSTPanelHelper" in (s.get("enabled") or []) for s in shards),
                 " ".join("%s=%s" % (s.get("shard"), s.get("enabled")) for s in shards))
        cb.check("两边写齐时问题清单为空", (rpt.get("issues") or []) == [],
                 str(rpt.get("issues"))[:220])
        cb.check("没问题时给的是人话而不是一片空白", "一致" in (rpt.get("note") or ""),
                 rpt.get("note", ""))

        # 3) 分片级视图：依赖清单也在里面。
        code, r = cb.plain_call("GET", "/api/v1/clusters/Cluster_1/shards/Master/mods")
        sv = r.get("data") or {}
        cb.check("分片模组页读到助手条目且启用", code == 200
                 and any(m["dir"] == "DSTPanelHelper" and m["enabled"] and m["onDisk"]
                         for m in (sv.get("mods") or [])), str(sv.get("mods"))[:220])
        cb.check("分片页的依赖清单为空数组而不是缺字段",
                 sv.get("dependencyProblems") == [], str(sv.get("dependencyProblems"))[:120])

        # 4) 真依赖缺失：在 mods/ 造一个声明了不存在依赖的模组，不启用它。
        # 这份 modinfo 按引擎真正认的写法来（一串裸赋值 + 长字符串 + 变量引用）：
        # `return { ... }` 面板以前能读，引擎却读不出任何字段，拿它做真机验证等于没验。
        made = cb.remote("""set -u
D="%s/%s"
mkdir -p "$D" || exit 1
cat > "$D/modinfo.lua" <<'LUAEOF'
-- 面板探针模组
name = "面板依赖探针"
version = "1.0"
api_version = 10
all_clients_require_mod = true
description = [[
探针说明：这一行在长字符串里，上面那行有 -- 和 = 号。
]]
local CHOICES = { {description = "一", data = 1} }
mod_dependencies = {
    { workshop = "workshop-998877" },
}
settings = {
    { name = "probe", label = "探针项", values = CHOICES, default = 1 },
}
LUAEOF
echo "SIZE=$(stat -c %%s $D/modinfo.lua)"
""" % (cb.DST_REAL, "mods/" + PROBE_DIR)).strip().replace("\n", " ")
        cb.check("探针模组落地", "SIZE=" in made and "SIZE=0" not in made, made[:120])

        code, r = cb.plain_call("GET", "/api/v1/mods")
        v = r.get("data") or {}
        row = [x for x in (v.get("mods") or []) if x["dir"] == PROBE_DIR]
        cb.check("裸赋值格式的名字和版本在云机上读得出来",
                 row and row[0].get("modinfoOk") is True
                 and row[0].get("name") == "面板依赖探针",
                 str(row[0])[:240] if row else str(v)[:160])
        cb.check("读真文件读出了依赖声明",
                 row and row[0].get("dependencies") == [["workshop-998877"]],
                 str(row[0].get("dependencies")) if row else str(v)[:160])
        cb.check("磁盘级预警点名这个模组缺哪一组",
                 any(u["dir"] == PROBE_DIR and u["candidates"] == ["workshop-998877"]
                     and u["onDisk"] is False for u in (v.get("unmetDependencies") or [])),
                 str(v.get("unmetDependencies"))[:220])
        cb.check("总提示把后果说成静默禁用", "静默禁用" in (v.get("hint") or ""), v.get("hint", ""))
        cb.check("all_clients_require_mod 读出来了", row and row[0].get("allClientsRequire") is True,
                 str(row[0])[:200] if row else "")

        # 探针模组没被任何分片启用，所以它不该污染一致性报告。
        code, r = cb.plain_call("GET", "/api/v1/clusters/Cluster_1/mods/consistency")
        rpt = r.get("data") or {}
        cb.check("没启用的模组不进跨分片问题清单",
                 (rpt.get("issues") or []) == []
                 and all(PROBE_DIR not in (s.get("enabled") or []) for s in (rpt.get("shards") or [])),
                 str(rpt.get("issues"))[:200])

        gone = cb.remote('rm -rf "%s/mods/%s"; ls -d "%s/mods/%s" 2>/dev/null | wc -l'
                         % (cb.DST_REAL, PROBE_DIR, cb.DST_REAL, PROBE_DIR)).strip().splitlines()[-1]
        cb.check("探针模组已清掉", gone == "0", "ls 结果=%s" % gone)
    finally:
        cb.remote("kill %s 2>/dev/null; pgrep -f %s | xargs -r kill 2>/dev/null; sleep 1"
                  % (pid or "0", cb.CFG))
        ls_modoverrides_guard(before)

    bad = [n for n, ok in cb.results if not ok]
    print("\n合计 %d 项，失败 %d 项" % (len(cb.results), len(bad)))
    for n in bad:
        print("失败：%s" % n)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
