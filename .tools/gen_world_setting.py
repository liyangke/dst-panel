#!/usr/bin/env python3
# 从 dst-admin-go 参考仓库的 dst_world_setting.json 生成面板用的世界设定字典。
# 只保留界面与校验需要的字段：分组顺序、中文名、可选项、每个取值的中文说明、默认值。
# 丢弃 atlas/image 坐标（Klei 图集定位用，面板不渲染原版图标）。
import json
import os
import sys

SRC = os.path.expandvars(
    r"C:\Users\26898\AppData\Local\Temp\dstref\dst-admin-go-main\scripts\py-dst-cli\dst_world_setting.json")
DST = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                   "internal", "dst", "assets", "world_setting.json")

BIG = 10 ** 6  # 没有 order 的项排在最后


def build(lang_doc):
    out = {}
    for location in ("forest", "cave"):
        doc = lang_doc.get(location)
        if not isinstance(doc, dict):
            continue
        seen = set()
        merged = {}
        for section in ("WORLDSETTINGS_GROUP", "WORLDGEN_GROUP"):
            for gkey, g in (doc.get(section) or {}).items():
                grp = merged.setdefault(gkey, {
                    "key": gkey,
                    "text": g.get("text") or gkey,
                    "order": g.get("order", BIG),
                    "items": [],
                })
                group_desc = g.get("desc") or {}
                for ikey, it in (g.get("items") or {}).items():
                    if ikey in seen:
                        continue  # 同一键可能在两个分组里重复出现，取首次出现
                    seen.add(ikey)
                    opts = it.get("desc") or group_desc
                    # 选项必须输出成数组而不是对象：Go 的 map 无序，
                    # 而"无/很少/默认/较多/大量"这类下拉顺序是有语义的。
                    ordered = [{"value": k, "text": v} for k, v in opts.items()]
                    grp["items"].append({
                        "key": ikey,
                        "text": it.get("text") or ikey,
                        "options": ordered,
                        "default": it.get("value") or "default",
                        "order": it.get("order", BIG),
                    })
        groups = list(merged.values())
        for g in groups:
            g["items"].sort(key=lambda x: (x["order"], x["key"]))
        groups.sort(key=lambda x: (x["order"], x["key"]))
        out[location] = {"groups": groups}
    return out


def main():
    if not os.path.exists(SRC):
        sys.exit("找不到参考字典: %s" % SRC)
    raw = json.load(open(SRC, encoding="utf-8"))
    doc = {"zh": build(raw["zh"]), "en": build(raw["en"])}
    os.makedirs(os.path.dirname(DST), exist_ok=True)
    with open(DST, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False, separators=(",", ":"))
    n_items = sum(len(g["items"]) for loc in doc["zh"].values() for g in loc["groups"])
    print("写入 %s  %.1f KB  分组=%d 条目(去重)=%d" % (
        DST, os.path.getsize(DST) / 1024,
        sum(len(loc["groups"]) for loc in doc["zh"].values()), n_items))


if __name__ == "__main__":
    main()
