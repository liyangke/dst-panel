// 核对 js/text.js 那两条外部文本规则。
//
// 这里读的是界面真正用的那一份源码，不是抄一份正则过来：抄的那份会跟着源码漂移，
// 测过了也不算验过。用 data: URL 动态 import 是因为 assets/js 那边是浏览器 ES 模块、
// 仓库里没有 package.json 声明 type:module，直接 import .js 会被 Node 当 CommonJS。
//
// 样本形状是从 Klei 真实列表里抄的（.tools/lobby_shape.py 抓到过以 \x01 开头、
// 中间夹私有区字形的房间名，以及把闲话填进季节字段的三条）。
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const src = readFileSync(
  path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "internal", "webui", "assets", "js", "text.js"),
  "utf8");
const { known, plain } = await import("data:text/javascript;base64," + Buffer.from(src, "utf8").toString("base64"));

const cases = [
  // [函数, 入参, 期望]
  ["plain", ["\x01多彩服\u{F0008}黄金航线\u{F0015}瀚海夺宝", "兜底"], "多彩服 黄金航线 瀚海夺宝"],
  ["plain", ["【十月新档】武林大会·大世界·组队熔炉·跑刀", "兜底"], "【十月新档】武林大会·大世界·组队熔炉·跑刀"],
  ["plain", ["Steam[怪服]<落日熔金>全新棱彩", "兜底"], "Steam[怪服]<落日熔金>全新棱彩"],
  // 真的 emoji（平面 1，不属于私有区）必须留着：那是房主故意写的，不是乱码。
  ["plain", ["\u{1F41F} 钓鱼服  \u{26CF}️   挖矿", "兜底"], "\u{1F41F} 钓鱼服 \u{26CF}️ 挖矿"],
  ["plain", ["\u{1F41F}\u{F0008}\u{1F3BA}", ""], "\u{1F41F} \u{1F3BA}"],
  // 平面 1 的标签字符（\u{E0020} 起）也是同样的隐形垃圾。
  ["plain", ["国旗\u{E0061}\u{E0062}服", ""], "国旗 服"],
  ["plain", ["\x01\x02", "1.2.3.4 那一间"], "1.2.3.4 那一间"],
  ["plain", ["\ue000\uf8ff\u{F0000}\u{10FFFF}", ""], ""],
  ["plain", [null, "空"], "空"],
  ["plain", [undefined, ""], ""],
  ["plain", ["  两边有空格\t还有制表符\n换行也要压掉  ", ""], "两边有空格 还有制表符 换行也要压掉"],
  // 认得出的季节要写出来，认不出的（后端原样回传）那一格留空。
  ["known", ["winter", "冬季"], "冬季"],
  ["known", ["fuck", "fuck"], ""],
  ["known", ["tranquil", "tranquil"], ""],
  ["known", ["", "未知"], ""],
  ["known", ["autumn", ""], ""],
];

const fns = { plain, known };
let bad = 0;
for (const [fn, args, want] of cases) {
  const got = fns[fn](...args);
  if (got !== want) {
    bad++;
    console.log("不对:", fn, JSON.stringify(args), "->", JSON.stringify(got), "期望", JSON.stringify(want));
  } else {
    console.log("ok  :", fn, "->", JSON.stringify(got));
  }
}
if (bad) console.log("有 " + bad + " 条不符");
process.exit(bad ? 1 : 0);
