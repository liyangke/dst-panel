// 世界地图：地形底图 + 建筑/资源分布 + 在线玩家。
//
// 三件事决定了这一页的画法：
//   - 地形先画进一张 grid×grid 的离屏画布，交互时只是 drawImage 缩放。
//     144×144 有两万多个格子，拖动时每帧重画两万次 fillRect 会直接掉帧。
//     逐格看邻居才算得出的东西（区域边界、海岸线、明度抖动）也一起在这一次算完。
//   - 建筑/资源和玩家每帧重画：点位只有几千个，玩家每五秒就会动一次。
//   - 玩家位置不触发表格重导：状态快照里已经有世界坐标，按导出时同一个公式
//     换算成格子，否则打开地图页就要每分钟让游戏线程采样两万格。
import * as api from "../api.js";
import { state, rel } from "../store.js";
import { charName, charShort, charStyle, placeName } from "../gameinfo.js";

const { computed } = Vue;

// 地形颜色按主题各一套。只用 key 上色，界面上显示的是接口给的中文 label，
// Klei 的 tile 编号一个都不出现（前端也拿不到，接口已经归好组）。
// 调色按"相邻的地形必须看得出区别"排：草地/ Savanna / 森林是玩家最关心的一组，
// 三者的明度拉开而不是只换色相，否则深浅色板在投影仪和偏色屏上会糊成一片绿。
const TERRAIN_COLORS = {
  // 一套"自然史地图"的权重：绿系占主导（森林图就该看着是绿的），土黄只当配角，
  // 沼泽从青蓝挪到墨绿（青蓝和海水同族，小图上会被读成"这里有一片水"），
  // "其它地形"是中性灰褐——它是不认识时的兜底，颜色不该比认得的还显眼。
  dark: {
    void: "#0e1219", road: "#a89877", rocky: "#6f7783", dirt: "#7a5c41",
    savanna: "#9aa84f", grass: "#5b9c52", forest: "#2c5f39", marsh: "#3f6b52",
    sand: "#c9b078", cave: "#4f4740", fungus: "#7d4f96", floor: "#7f8894",
    web: "#a9b2c0", coast: "#3d8ec2", ocean: "#16406f", farm: "#6b4a30",
    wall: "#3a3941", other: "#7f7a72",
  },
  light: {
    void: "#c6ccd6", road: "#dcc79c", rocky: "#b1aca3", dirt: "#c2a184",
    savanna: "#cdd38f", grass: "#9ad383", forest: "#5c9e63", marsh: "#93bda2",
    sand: "#ecd9a8", cave: "#b8b1a5", fungus: "#d3b5da", floor: "#dbe0e7",
    web: "#e3ddec", coast: "#a5d5f0", ocean: "#6ba3d8", farm: "#c39a78",
    wall: "#9aa0ac", other: "#cfc7bc",
  },
};

// 点位用形状区分，不只靠颜色：截图发灰、色盲、投影仪偏色时形状还在。
// 色值必须和 CSS 里图例用的同一套（--warn / --info / --bad），否则浅色主题下
// 画布上的建筑点和下面图例的方块不是一个颜色，"照着图例读图"就读错了。
const KIND_COLORS = {
  dark: { build: "#f2a54c", res: "#57c7e6", danger: "#ff6b6b" },
  light: { build: "#b8730d", res: "#1f87a8", danger: "#cf3b3b" },
};
// 伙伴随从的颜色只有一个来源：CSS 的 --map-companion（图例的菱形块用的就是它）。
// 其余三类是历史遗留的两份 hex，改的时候要记得两边一起改。
function cssVar(name) {
  return (getComputedStyle(document.documentElement).getPropertyValue(name) || "").trim();
}

// 选中/定位那一个玩家的高亮环，和地图底色拉开。
const FOCUS_RING = { dark: "#ffd479", light: "#8a5a00" };

const rgbCache = {};

function hexRgb(hex) {
  if (rgbCache[hex]) return rgbCache[hex];
  const v = String(hex || "").replace("#", "");
  const n = parseInt(v.length === 3 ? v.replace(/(.)/g, "$1$1") : v, 16);
  const out = Number.isFinite(n) ? [(n >> 16) & 255, (n >> 8) & 255, n & 255] : [128, 128, 128];
  rgbCache[hex] = out;
  return out;
}

function clamp(v, lo, hi) {
  return Math.max(lo, Math.min(hi, v));
}

// 同组内地形的明度抖动（0.955~1.051）。整片纯色会被读成"这块没数据"，
// 一点固定跟着格子的噪点就有地面的纹理。哈希必须稳定：否则每次换主题
// 重画时整张图的花纹都换一个样子，看着像地图内容变了。
function cellShade(x, y) {
  // 每 4 格换一个明度档，而不是每格一档：每格一档放大后就是一张棋盘格
  // （"只有纯色小方块"这句话有一半是在说这个），4 格一片读出来才是地面的起伏。
  return 0.958 + (cellHash(x >> 2, y >> 2) % 88) / 1000;
}

function cellHash(x, y) {
  let n = Math.imul(x + 1, 73856093) ^ Math.imul(y + 1, 19349663);
  n = Math.imul(n ^ (n >>> 13), 1274126177);
  return (n ^ (n >>> 16)) >>> 0;
}

// 一格一个纯色方块，就是他说的"放大后只有马赛克"。放大到一格有十几像素时，
// 这里按地形给每一格补两三笔自己的纹理：草是短竖线、岩石是砾石、海是波纹、
// 洞穴蘑菇自己发光。笔画的位置由格子坐标哈希决定，所以同一格每次画都长一样，
// 拖动时纹理跟着地图走，不会闪成电视雪花。
const TEX = {
  grass: "blades", savanna: "blades", forest: "clumps", marsh: "reeds",
  dirt: "speck", road: "speck", sand: "speck", cave: "speck", other: "speck",
  farm: "speck",
  rocky: "pebble", wall: "pebble", floor: "plank", fungus: "glow", web: "cross",
  ocean: "waves", coast: "waves", void: "star",
};

// 高倍率下的个体图标：第三版，改的是 mock 里暴露的四件事——
// 树（三层枝叶糊成黑块 → 两层、层间留缝、亮面提出来）、危险和石头撞形
// （都是灰圆顶 → 蜘蛛丘改紫黑加网，石头改成有棱角的块）、草和芦苇太细
// （线宽跟着图标尺寸走，不再固定）、深底上描边把物体糊掉（暗底补一圈浅光晕）。
// 只借 Klei 的美术语汇（暖黑描边、土色、顶亮下暗、脚下投影），不取它的任何素材。
// 统一调色表：地图里所有颜色只在这一处集中，渲染函数一律按语义 key 取色，
// 不再散落颜色字面量。深浅主题都从同一入口取（uiColors / palette / kindPalette）：
//   theme[dark|light] —— 地形底图沿用 TERRAIN_COLORS 的两套值（深底浅绿、浅底
//     墨绿，各有对比度，不能合成一套），加上画布文字、标签、参考线、高亮环等
//     主题色；同一组语义 key 两套值，改主题只切这一层，不再逐处改字面量；
//   kind  —— 点位色兜底。正常路径由 kindPalette() 读 CSS 主题变量
//     （--warn / --info / --bad / --map-companion），和页面图例同源；
//   shared —— 不随主题变的图标笔画、描边、兜底色，深浅共用一份。
const MAP_COLORS = {
  shared: {
    ink: "#241a12",
    groundShadow: "rgba(18,12,8,.28)",
    fallback: "#888",
    playerFallback: "#4c8dff",
    playerDead: "#8a8f99",
    playerRing: "#ffffff",
    playerCross: "#1a2230",
    icon: {
      leaf: "#41602f", leafHi: "#5d8040", berry: "#b04048",
      trunk: "#5a3f26", leafDark: "#2f5a3a", leafMid: "#3c6d45", treeHi: "#5b8f5e",
      rock: "#6f6a61", rockHi: "#9c968b",
      grass: "#6f7f34", reed: "#5f8468", reedTip: "#8a6a34", saplingLeaf: "#4f7a3a",
      stem: "#ded4c0", cap: "#9c4146", dots: "#efe6d4",
      spider: "#4a3f55", spiderWeb: "#d5dbe8",
      wall: "#7a5f42", roof: "#9a7b52", door: "#3f2f1f",
      pet: "#5b3f74", petTop: "#7d5a99", eyes: "#f0c94a",
      generic: "#4f6b3a",
    },
  },
  theme: {
    dark: {
      terrain: TERRAIN_COLORS.dark,
      text: "#e7edf7", textAlt: "#e7ebf2", focus: FOCUS_RING.dark,
      countStroke: "rgba(8, 11, 18, .9)",
      gridLine: "rgba(255, 255, 255, .075)",
      aggStroke: "rgba(0, 0, 0, .65)",
      labelBg: "rgba(15, 18, 22, .86)", labelLine: "rgba(255, 255, 255, .22)",
      hoverLine: "rgba(255, 255, 255, .9)",
    },
    light: {
      terrain: TERRAIN_COLORS.light,
      text: "#26303f", textAlt: "#1a2230", focus: FOCUS_RING.light,
      countStroke: "rgba(255, 255, 255, .9)",
      gridLine: "rgba(24, 32, 46, .08)",
      aggStroke: "rgba(255, 255, 255, .55)",
      labelBg: "rgba(255, 255, 255, .94)", labelLine: "rgba(24, 32, 46, .2)",
      hoverLine: "rgba(20, 26, 36, .85)",
    },
  },
  kind: {
    dark: { ...KIND_COLORS.dark, companion: "#f472b6" },
    light: { ...KIND_COLORS.light, companion: "#be1877" },
  },
};

// 自定义属性若写成 var(--x) 引用，getComputedStyle 可能原样返回这段字符串，
// 直接塞进 canvas 的 fillStyle 会被忽略、沿用上一次的颜色（点位串色）。
// 只认 #hex，取不到合法值就回退到调色表的兜底色。
function cssHex(name, fallback) {
  const v = cssVar(name);
  return /^#(?:[0-9a-f]{3,4}|[0-9a-f]{6}|[0-9a-f]{8})$/i.test(v) ? v : fallback;
}
function uiColors(dark) {
  return MAP_COLORS.theme[dark ? "dark" : "light"] || MAP_COLORS.theme.dark;
}
const ICON_COLORS = MAP_COLORS.shared.icon;
const INK = MAP_COLORS.shared.ink;
function iconFrame(ctx, s, dark) {
  ctx.lineJoin = "round"; ctx.lineCap = "round";
  ctx.lineWidth = Math.max(0.8, s * 0.07);
  ctx.strokeStyle = INK;
  // 深色底上光有暗描边等于没有：树压在森林格上会整块消失，所以加一圈浅光晕。
  ctx.shadowColor = dark ? "rgba(226,235,250,.55)" : "rgba(255,255,255,0)";
  ctx.shadowBlur = dark ? Math.max(1.5, s * 0.16) : 0;
}
function iconGround(ctx, x, y, r) {
  ctx.save(); ctx.shadowBlur = 0;
  ctx.fillStyle = MAP_COLORS.shared.groundShadow;
  ctx.beginPath(); ctx.ellipse(x, y + r * 0.8, r * 0.85, r * 0.26, 0, 0, 6.2832); ctx.fill();
  ctx.restore();
}
const SUB_ICON = {
  "浆果丛": "berry", "树": "tree", "树苗": "sapling", "草": "grass", "芦苇": "reed",
  "石料": "rock", "蘑菇": "shroom", "花": "flower", "仙人掌": "cactus", "采集点": "generic",
};
const ICONS = {
  berry: (ctx, x, y, s) => {
    const r = s * 0.44;
    iconGround(ctx, x, y, r); iconFrame(ctx, s);
    ctx.fillStyle = ICON_COLORS.leaf; ctx.beginPath();
    ctx.arc(x - r * .5, y + r * .16, r * .55, 0, 6.2832);
    ctx.arc(x + r * .5, y + r * .16, r * .55, 0, 6.2832);
    ctx.arc(x, y - r * .26, r * .62, 0, 6.2832); ctx.fill(); ctx.stroke();
    ctx.save(); ctx.shadowBlur = 0; ctx.fillStyle = ICON_COLORS.leafHi; ctx.beginPath();
    ctx.arc(x - r * .3, y - r * .34, r * .3, 0, 6.2832); ctx.fill(); ctx.restore();
    ctx.save(); ctx.shadowBlur = 0; ctx.fillStyle = ICON_COLORS.berry;
    for (const [a, b] of [[-.48, .24], [.34, .3], [.02, -.16], [.6, -.1]]) {
      ctx.beginPath(); ctx.arc(x + r * a, y + r * b, r * .17, 0, 6.2832); ctx.fill(); }
    ctx.restore();
  },
  tree: (ctx, x, y, s) => {
    const r = s * 0.48;
    iconGround(ctx, x, y, r); iconFrame(ctx, s);
    ctx.fillStyle = ICON_COLORS.trunk; ctx.fillRect(x - r * .11, y + r * .5, r * .22, r * .45);
    // 两层、中间留缝：三层在 15px 上会糊成一坨黑
    ctx.fillStyle = ICON_COLORS.leafDark;
    ctx.beginPath(); ctx.moveTo(x - r * .95, y + r * .58); ctx.lineTo(x + r * .95, y + r * .58);
    ctx.lineTo(x + r * .1, y - r * .18); ctx.lineTo(x - r * .62, y - r * .1); ctx.closePath();
    ctx.fill(); ctx.stroke();
    ctx.fillStyle = ICON_COLORS.leafMid;
    ctx.beginPath(); ctx.moveTo(x - r * .7, y - r * .12); ctx.lineTo(x + r * .72, y - r * .16);
    ctx.lineTo(x + r * .05, y - r * 1.02); ctx.closePath(); ctx.fill(); ctx.stroke();
    ctx.save(); ctx.shadowBlur = 0; ctx.fillStyle = ICON_COLORS.treeHi; ctx.beginPath();
    ctx.moveTo(x - r * .1, y - r * .9); ctx.lineTo(x + r * .34, y - r * .22);
    ctx.lineTo(x - r * .12, y - r * .24); ctx.closePath(); ctx.fill(); ctx.restore();
  },
  rock: (ctx, x, y, s) => {
    const r = s * 0.42;
    iconGround(ctx, x, y, r); iconFrame(ctx, s);
    // 有棱角的块，不用圆弧——圆弧和蜘蛛丘在 15px 下是同一个形状
    ctx.fillStyle = ICON_COLORS.rock; ctx.beginPath();
    ctx.moveTo(x - r, y + r * .6); ctx.lineTo(x - r * .86, y - r * .06);
    ctx.lineTo(x - r * .28, y - r * .84); ctx.lineTo(x + r * .58, y - r * .66);
    ctx.lineTo(x + r * .96, y + r * .12); ctx.lineTo(x + r * .56, y + r * .62);
    ctx.closePath(); ctx.fill(); ctx.stroke();
    ctx.save(); ctx.shadowBlur = 0; ctx.fillStyle = ICON_COLORS.rockHi; ctx.beginPath();
    ctx.moveTo(x - r * .8, y - r * .04); ctx.lineTo(x - r * .26, y - r * .74);
    ctx.lineTo(x + r * .5, y - r * .56); ctx.lineTo(x + r * .06, y - r * .1);
    ctx.closePath(); ctx.fill(); ctx.restore();
  },
  grass: (ctx, x, y, s) => {
    const r = s * 0.4;
    iconFrame(ctx, s); ctx.strokeStyle = ICON_COLORS.grass;
    ctx.lineWidth = Math.max(1, s * 0.1);
    for (const [a, bend] of [[-.5, -.3], [-.16, -.08], [.18, .1], [.52, .3]]) {
      ctx.beginPath(); ctx.moveTo(x + r * a, y + r * .75);
      ctx.quadraticCurveTo(x + r * (a + bend), y - r * .1, x + r * (a + bend * 1.8), y - r * .95);
      ctx.stroke();
    }
  },
  reed: (ctx, x, y, s) => {
    const r = s * 0.44;
    iconFrame(ctx, s); ctx.strokeStyle = ICON_COLORS.reed; ctx.lineWidth = Math.max(1, s * 0.085);
    for (const [a, bend] of [[-.34, -.12], [0, .05], [.34, .16]]) {
      ctx.beginPath(); ctx.moveTo(x + r * a, y + r * .85);
      ctx.quadraticCurveTo(x + r * (a + bend), y - r * .2, x + r * (a + bend * 2), y - r * 1.05);
      ctx.stroke();
    }
    ctx.fillStyle = ICON_COLORS.reedTip; ctx.beginPath();
    ctx.ellipse(x - r * .06, y - r * 1.0, r * .16, r * .4, .05, 0, 6.2832); ctx.fill(); ctx.stroke();
  },
  sapling: (ctx, x, y, s) => {
    const r = s * 0.38;
    iconFrame(ctx, s); ctx.strokeStyle = ICON_COLORS.trunk; ctx.lineWidth = Math.max(1, s * 0.08);
    ctx.beginPath(); ctx.moveTo(x, y + r * .85); ctx.quadraticCurveTo(x + r * .12, y, x - r * .04, y - r * .6); ctx.stroke();
    ctx.fillStyle = ICON_COLORS.saplingLeaf;
    for (const [a, b, rot] of [[-.5, -.2, -.6], [.5, -.5, .55]]) {
      ctx.beginPath(); ctx.ellipse(x + r * a, y + r * b, r * .46, r * .24, rot, 0, 6.2832);
      ctx.fill(); ctx.stroke(); }
  },
  shroom: (ctx, x, y, s) => {
    const r = s * 0.4;
    iconGround(ctx, x, y, r); iconFrame(ctx, s);
    ctx.fillStyle = ICON_COLORS.stem; ctx.fillRect(x - r * .2, y - r * .1, r * .4, r * .9);
    ctx.strokeRect(x - r * .2, y - r * .1, r * .4, r * .9);
    ctx.fillStyle = ICON_COLORS.cap; ctx.beginPath();
    ctx.moveTo(x - r * .88, y - r * .06);
    ctx.quadraticCurveTo(x, y - r * 1.25, x + r * .88, y - r * .06);
    ctx.closePath(); ctx.fill(); ctx.stroke();
    ctx.save(); ctx.shadowBlur = 0; ctx.fillStyle = ICON_COLORS.dots;
    for (const [a, b] of [[-.4, -.44], [.14, -.62], [.48, -.28]]) {
      ctx.beginPath(); ctx.arc(x + r * a, y + r * b, r * .12, 0, 6.2832); ctx.fill(); }
    ctx.restore();
  },
  danger: (ctx, x, y, s) => {
    const r = s * 0.46;
    iconGround(ctx, x, y, r); iconFrame(ctx, s);
    ctx.fillStyle = ICON_COLORS.spider; ctx.beginPath();
    ctx.moveTo(x - r, y + r * .6);
    ctx.bezierCurveTo(x - r * .86, y - r * .5, x - r * .2, y - r * 1.0, x + r * .08, y - r * .92);
    ctx.bezierCurveTo(x + r * .74, y - r * .74, x + r * .98, y - r * .06, x + r, y + r * .6);
    ctx.closePath(); ctx.fill(); ctx.stroke();
    // 网用亮色几笔拉出交叉，跟石头的"实心块"彻底分开
    ctx.save(); ctx.shadowBlur = 0; ctx.strokeStyle = ICON_COLORS.spiderWeb;
    ctx.lineWidth = Math.max(.7, s * 0.042);
    for (const a of [-.6, -.18, .24, .62]) {
      ctx.beginPath(); ctx.moveTo(x + r * a, y + r * .55); ctx.lineTo(x + r * a * .25, y - r * .72); ctx.stroke(); }
    ctx.beginPath(); ctx.moveTo(x - r * .72, y - r * .06); ctx.lineTo(x + r * .74, y + r * .1); ctx.stroke();
    ctx.restore();
  },
  build: (ctx, x, y, s) => {
    const r = s * 0.44;
    iconGround(ctx, x, y, r); iconFrame(ctx, s);
    ctx.fillStyle = ICON_COLORS.wall; ctx.fillRect(x - r * .76, y - r * .1, r * 1.52, r * .8);
    ctx.strokeRect(x - r * .76, y - r * .1, r * 1.52, r * .8);
    ctx.fillStyle = ICON_COLORS.roof; ctx.beginPath();
    ctx.moveTo(x - r * .96, y - r * .08); ctx.lineTo(x, y - r * .92);
    ctx.lineTo(x + r * .96, y - r * .08); ctx.closePath(); ctx.fill(); ctx.stroke();
    ctx.save(); ctx.shadowBlur = 0; ctx.fillStyle = ICON_COLORS.door;
    ctx.fillRect(x - r * .18, y + r * .16, r * .36, r * .54); ctx.restore();
  },
  companion: (ctx, x, y, s) => {
    const r = s * 0.42;
    iconGround(ctx, x, y, r); iconFrame(ctx, s);
    ctx.fillStyle = ICON_COLORS.pet; ctx.beginPath();
    ctx.moveTo(x - r * .78, y - r * .18); ctx.lineTo(x + r * .78, y - r * .18);
    ctx.lineTo(x + r * .7, y + r * .6); ctx.lineTo(x - r * .7, y + r * .6);
    ctx.closePath(); ctx.fill(); ctx.stroke();
    ctx.fillStyle = ICON_COLORS.petTop; ctx.fillRect(x - r * .86, y - r * .56, r * 1.72, r * .38);
    ctx.strokeRect(x - r * .86, y - r * .56, r * 1.72, r * .38);
    ctx.save(); ctx.shadowBlur = 0; ctx.fillStyle = ICON_COLORS.eyes;
    for (const a of [-.32, .32]) { ctx.beginPath(); ctx.arc(x + r * a, y + r * .14, r * .15, 0, 6.2832); ctx.fill(); }
    ctx.restore();
  },
  generic: (ctx, x, y, s) => {
    const r = s * 0.36;
    iconGround(ctx, x, y, r); iconFrame(ctx, s);
    ctx.fillStyle = ICON_COLORS.generic; ctx.beginPath(); ctx.arc(x, y, r * .82, 0, 6.2832);
    ctx.fill(); ctx.stroke();
  },
};
// 图标查表：资源按子类中文名映射到具体图标；类别是接口给的键。
// 类别的中文标签（"危险"）不能当键用，改一个字就查不到，所以单独列一张。
const KIND_ICON = { res: "generic", danger: "danger", build: "build", companion: "companion" };
const FILTER_KEY = "dstpanel.mapfilter";
function loadHidden() {
  try { return JSON.parse(localStorage.getItem(FILTER_KEY) || "{}") || {}; }
  catch (e) { return {}; }   // 隐私模式或旧数据坏了：当作没筛过，不该连图都打不开
}
function drawEntIcon(ctx, kind, subLabel, x, y, s, dark) {
  const f = ICONS[SUB_ICON[subLabel] || KIND_ICON[kind] || "generic"];
  ctx.save();
  f(ctx, x, y, s);
  ctx.restore();
}

// 每格现算三次 "rgb(...)" 拼接的话，字符串分配就把帧时间吃光了。
// 颜色字符串按 (地形, 明度档) 缓存：一帧要上万个格子，
// 每格现算三次 "rgb(...)" 拼接的话，字符串分配就把帧时间吃光了。
const toneCache = {};
function tone(hex, mul) {
  const m = Math.round(mul * 100);      // 明度量化到 1/100，缓存才长得有限
  const k = hex + "|" + m;
  let c = toneCache[k];
  if (c) return c;
  const r = hexRgb(hex);
  c = "rgb(" + Math.round(clamp(r[0] * m / 100, 0, 255)) + "," +
    Math.round(clamp(r[1] * m / 100, 0, 255)) + "," +
    Math.round(clamp(r[2] * m / 100, 0, 255)) + ")";
  toneCache[k] = c;
  return c;
}

export default {
  name: "Map",
  setup() {
    return { state, rel, charName, charShort, charStyle, placeName };
  },
  data() {
    return {
      view: null,
      loading: false,
      error: "",
      shard: "",
      zoom: 1,
      panX: 0,
      panY: 0,
      hover: null,
      hoverAt: null,
      hoverPlayer: null,
      pollStopped: false,
      box: { w: 0, h: 0 },
      // 全屏看地图：这一页的信息密度全在图上，小屏或多人围着看时想要一整屏都是图。
      full: false,
      // 全屏那一档要从视口高度里扣掉的像素，由 syncFullBudget 实测画布上沿得到；
      // 0 表示还没测到，先用兜底常数。
      fullBudget: 0,
      // 类别/子类筛选：默认全显示，关掉的记在本地（同一台机器同一浏览器记得住）
      hidden: loadHidden(),
    };
  },
  computed: {
    // 个体点位按网格分桶。一帧只遍历视野里的桶，不去遍历一万条点位；
    // 协议编号也在这里翻成键和中文，往下不再传数字码。
    entBuckets() {
      const v = this.view;
      if (!v || !v.ents || !v.ents.length || !v.width || !v.grid) return null;
      const kindOf = {}, subOf = {};
      (v.kinds || []).forEach((k) => { kindOf[k.code] = k.key; });
      (v.subs || []).forEach((k) => { subOf[k.code] = k.label; });
      const g = v.grid, h = v.height || v.width, m = new Map();
      for (const e of v.ents) {
        const k = kindOf[e[2]];
        if (!k) continue;
        const gx = clamp(Math.floor(e[0] * g / v.width), 0, g - 1);
        const gy = clamp(Math.floor(e[1] * g / h), 0, g - 1);
        const key = gy * g + gx;
        let a = m.get(key);
        if (!a) { a = []; m.set(key, a); }
        a.push({ tx: e[0], ty: e[1], kind: k, subLabel: subOf[e[3]] || "" });
      }
      return m;
    },
    // 筛选条只列这张图上真有的类别与子类，顺序跟接口给的一致。
    filterOptions() {
      const v = this.view;
      if (!v) return [];
      const out = (v.kinds || []).map((k) => ({ key: k.key, label: k.label }));
      (v.subs || []).forEach((k) => out.push({ key: k.label, label: k.label }));
      return out;
    },
    cluster: {
      get() { return state.sel.cluster; },
      set(v) { state.sel.cluster = v; },
    },
    clusterObj() {
      return state.clusters.find((c) => c.name === this.cluster) || null;
    },
    // 当前这个世界在 /clusters 上的那一行：后端把"助手这一刻为什么不回话"算成了
    // 一句话（暂停 / 没进世界 / 读不出来），地图页直接引用它。
    // 自己另编一句"数据已过期"等于把已经查到的原因藏回去，还会和别的页面对不上。
    shardObj() {
      return ((this.clusterObj && this.clusterObj.shards) || [])
        .find((s) => s.name === this.shard) || null;
    },
    shardNames() {
      return ((this.clusterObj && this.clusterObj.shards) || []).map((s) => s.name);
    },
    mapKey() {
      return this.cluster && this.shard ? this.cluster + "/" + this.shard : "";
    },
    palette() {
      return (MAP_COLORS.theme[state.theme] || MAP_COLORS.theme.dark).terrain;
    },
    kindPalette() {
      // 四类点位色都从 CSS 主题变量取，和页面图例同源；变量读不到时才用兜底色。
      const fb = MAP_COLORS.kind[state.theme] || MAP_COLORS.kind.dark;
      return {
        build: cssHex("--warn", fb.build),
        res: cssHex("--info", fb.res),
        danger: cssHex("--bad", fb.danger),
        companion: cssHex("--map-companion", fb.companion),
      };
    },
    ready() {
      return !!(this.view && this.view.ready && this.view.grid);
    },
    aspect() {
      const v = this.view;
      if (!v || !v.ready || !v.width || !v.height) return 1;
      return v.width / v.height;
    },
    // 画布的高度上限（68vh）会横向留白：卡片比 68vh 宽的时候，宽度还按整张卡片 stretch，
    // 结果 aspect-ratio 被 max-height 顶掉，正方形地图变成扁的。
    // 所以把宽度也按同一个比例锁住，让它始终能维持真实比例并居中。
    wrapStyle() {
      const ratio = this.aspect;
      // 全屏那一档：画布要让压在图上的读数、类别筛选、图例全都完整落在屏幕里，
      // 所以高度预算 = 实测的画布上沿到视口顶的距离 + 底下留的一小条。
      // 这个数不能写死：上面那行房间选择在窄屏会换行，写死就把读数顶出屏幕外
      // （第一版就是按常数算的，实测裁掉了最下面一行）。
      if (this.full) {
        const h = "calc(100vh - " + (this.fullBudget || 130) + "px)";
        return { aspectRatio: ratio + " / 1", maxHeight: h, maxWidth: "calc(" + h + " * " + ratio + ")" };
      }
      return {
        aspectRatio: ratio + " / 1",
        maxWidth: "calc(68vh * " + ratio + ")",
      };
    },
    // 触屏上没有滚轮和悬停：操作提示要换成手指的说法，否则照着提示找鼠标滚轮是白找。
    // 写在 computed 里会被缓存成首次结果——桌面和手机不会在同一个会话里互换外接设备，够用。
    gestureHint() {
      const coarse = typeof window !== "undefined" && window.matchMedia
        && window.matchMedia("(hover: none) and (pointer: coarse)").matches;
      return coarse
        ? "双指缩放、单指拖动、双击复位；点一下地图上的位置能读出这一格的地形和点位。"
        : "滚轮缩放、拖动平移、双击复位；把指针放到地图上能读出这一格的地形和点位。";
    },
    // 在线玩家：优先取状态快照（每五秒刷新），没有快照时才用接口随地图带回的那份。
    // 快照里的坐标是游戏世界坐标，这里换算成网格坐标，和助手导出地形用的公式一致。
    livePlayers() {
      const v = this.view;
      const st = this.mapKey ? state.statuses[this.mapKey] : null;
      if (st && st.data && !st.data.stale) {
        const g = v && v.ready ? v.grid : 0;
        return (st.data.players || []).map((p) => {
          const out = {
            userid: p.userid, name: p.name, prefab: p.prefab,
            dead: !!p.dead, wx: p.x, wz: p.z, gx: null, gy: null,
          };
          if (g && p.x !== null && p.x !== undefined && p.z !== null && p.z !== undefined) {
            out.gx = (p.x / 4 + v.width / 2) / v.width * g;
            out.gy = (p.z / 4 + v.height / 2) / v.height * g;
          }
          return out;
        });
      }
      const list = (v && v.players) || [];
      return list.map((p) => ({
        userid: p.userid, name: p.name, prefab: p.prefab, dead: !!p.dead,
        wx: p.worldX, wz: p.worldZ, gx: p.x === undefined ? null : p.x,
        gy: p.y === undefined ? null : p.y,
      }));
    },
    plotPlayers() {
      return this.livePlayers.filter((p) => Number.isFinite(p.gx) && Number.isFinite(p.gy));
    },
    // "没人在线" 与 "这一刻问不到" 不能共用一句话：世界被引擎暂停时快照是停的，
    // 这时候断言没人在玩就是假结论（玩家可能只是刚进洞穴，或者世界冻在你上次看到的时刻）。
    playersUnknown() {
      if (this.livePlayers.length) return false;
      const st = this.mapKey ? state.statuses[this.mapKey] : null;
      return !st || !st.data || st.data.stale;
    },
    playerNote() {
      const st = this.mapKey ? state.statuses[this.mapKey] : null;
      if (st && st.data && !st.data.stale) return "玩家位置来自每秒的状态数据，每五秒刷新一次。";
      // 后端那句话已经把"这一刻为什么读不到"说全了（没人时引擎暂停、没进世界、还是文件读不出），
      // 这里再编一句自己的判断只会重复它，或者跟它打架。
      const why = (this.shardObj && this.shardObj.helperNote) || "这一刻读不到实时位置。";
      // 状态停着时用的是接口随底图带回的那一份位置：点照样画得出，但只到导出那一刻。
      return this.plotPlayers.length
        ? "地图上画的是上次导出底图时的那一份位置。" + why
        : why;
    },
    // 图例是十几行色块，看不出"这张图主要是什么地"；先给一句结论，下面那条占比 bar
    // 负责比例。terrain 是接口按格子数从多到少排好的，所以前三项就是这张图的主角。
    coverSummary() {
      const t = (this.view && this.view.terrain) || [];
      const total = t.reduce((a, x) => a + (x.cells || 0), 0);
      if (!t.length || !total) return "";
      const top = t.slice(0, 3).filter((x) => x.cells / total >= 0.05);
      if (!top.length) return "地形很碎，没有占大头的";
      return "以" + top.map((x) => x.label).join("、") + "为主";
    },
  },
  watch: {
    cluster() {
      const before = this.shard;
      this.pickShard();
      // pickShard 改了分片时由 shard 的 watcher 负责加载，这里只在分片没变时补一次，
      // 否则同一个请求会发两遍（地图接口还会因此多触发一次导出判定）。
      if (this.shard === before) this.reload();
    },
    shard(v) {
      if (v) this.reload();
    },
    "state.theme"() {
      this.buildTerrain();
    },
    livePlayers() {
      this.scheduleDraw();
    },
  },
  mounted() {
    this._off = null;
    this._offTheme = "";
    this._pointsAt = new Map();
    this._ptrs = new Map();
    this._drag = null;
    this._pinch = null;
    this._pointer = null;
    this._pollTimer = null;
    this._polls = 0;
    this._boundCv = null;
    this._roWrap = null;

    // 这一页的 canvas 出现在「房间加载完」之后，mounted 时它还不存在；
    // 而 v-if 切换会换掉整块 DOM。所以监听和 ResizeObserver 不能只在 mounted 绑一次，
    // 每次拿到数据后的下一帧都要确认绑的还是当前这个元素。
    this._handlers = {
      down: (e) => this.pointerDown(e),
      move: (e) => this.pointerMove(e),
      up: (e) => this.pointerUp(e),
      leave: () => {
        this.hover = null;
        this.hoverAt = null;
        this.hoverPlayer = null;
        this.scheduleDraw();
      },
      wheel: (e) => this.wheel(e),
      dbl: () => this.resetView(),
    };
    this._ro = new ResizeObserver(() => {
      this.measure();
      this.scheduleDraw();
    });
    this._onKey = (e) => this.onKeydown(e);
    document.addEventListener("keydown", this._onKey);
    // 全屏时窗口一改（转屏、分屏、浏览器窗口拖动），画布上沿的距离就得重新量一次，
    // 否则用的还是上一次那个预算，读数会被顶出视口。
    this._onResize = () => { if (this.full) this.syncFullBudget(); };
    window.addEventListener("resize", this._onResize);

    this.bindCanvas();
    this.pickShard();
    if (this.shard) this.reload();
  },
  beforeUnmount() {
    if (this._pollTimer) clearTimeout(this._pollTimer);
    if (this._raf) cancelAnimationFrame(this._raf);
    if (this._ro) this._ro.disconnect();
    if (this._boundCv) this.detach(this._boundCv);
    if (this._onKey) document.removeEventListener("keydown", this._onKey);
    if (this._onResize) window.removeEventListener("resize", this._onResize);
    document.documentElement.classList.remove("map-fullscreen");
  },
  methods: {
    // 全屏只切一个 class：图、缩放、悬浮明细、筛选都还是同一套 DOM 和事件，
    // 不做"另开一个页面"，否则两份 canvas 状态一定会走岔。
    toggleFull() {
      this.full = !this.full;
      document.documentElement.classList.toggle("map-fullscreen", this.full);
      if (!this.full) return;
      this.$nextTick(() => {
        this.syncFullBudget();
        requestAnimationFrame(() => { this.measure(); this.scheduleDraw(); });
      });
    },
    // 量画布上沿到视口顶的距离：上面那行房间选择器在窄屏会换成两行，卡片头的高度也随
    // 主题字号变，所以这个数只能实测。量的是"未滚动时"的位置，滚动条拉下来也不受影响。
    syncFullBudget() {
      const wrap = this.$refs.wrap;
      const page = this.$el;
      if (!wrap || !page) return;
      const top = wrap.getBoundingClientRect().top - page.getBoundingClientRect().top + page.scrollTop;
      const px = Math.round(top + 16);
      if (px > 60 && px !== this.fullBudget) this.fullBudget = px;
    },
    onKeydown(e) {
      if (e.key === "Escape" && this.full) this.toggleFull();
    },
    toggleFilter(key) {
      const h = Object.assign({}, this.hidden);
      if (h[key]) delete h[key]; else h[key] = true;
      this.hidden = h;
      try { localStorage.setItem(FILTER_KEY, JSON.stringify(h)); } catch (e) { /* 存不下只是记不住，不影响这次筛选 */ }
      this.scheduleDraw();
    },
    bindCanvas() {
      const wrap = this.$refs.wrap;
      if (wrap && this._ro && this._roWrap !== wrap) {
        if (this._roWrap) this._ro.unobserve(this._roWrap);
        this._ro.observe(wrap);
        this._roWrap = wrap;
      }
      const cv = this.$refs.cv;
      if (cv && cv !== this._boundCv) {
        if (this._boundCv) this.detach(this._boundCv);
        const h = this._handlers;
        cv.addEventListener("pointerdown", h.down);
        cv.addEventListener("pointermove", h.move);
        cv.addEventListener("pointerup", h.up);
        cv.addEventListener("pointercancel", h.up);
        cv.addEventListener("pointerleave", h.leave);
        cv.addEventListener("wheel", h.wheel, { passive: false });
        cv.addEventListener("dblclick", h.dbl);
        this._boundCv = cv;
      }
      this.measure();
    },
    detach(cv) {
      const h = this._handlers;
      cv.removeEventListener("pointerdown", h.down);
      cv.removeEventListener("pointermove", h.move);
      cv.removeEventListener("pointerup", h.up);
      cv.removeEventListener("pointercancel", h.up);
      cv.removeEventListener("pointerleave", h.leave);
      cv.removeEventListener("wheel", h.wheel);
      cv.removeEventListener("dblclick", h.dbl);
    },
    enc(v) { return encodeURIComponent(v); },
    pickShard() {
      const names = this.shardNames;
      if (names.indexOf(this.shard) >= 0) return;
      const shards = (this.clusterObj && this.clusterObj.shards) || [];
      const live = shards.find((s) => s.running);
      this.shard = (live && live.name) || (names.indexOf("Master") >= 0 ? "Master" : names[0]) || "";
    },
    reload() {
      this.zoom = 1;
      this.panX = 0;
      this.panY = 0;
      this.hover = null;
      this.hoverAt = null;
      this.hoverPlayer = null;
      this.pollStopped = false;
      this._polls = 0;
      this.load(false);
    },
    async load(force) {
      if (!this.mapKey) { this.view = null; return; }
      this.loading = true;
      this.error = "";
      const want = this.mapKey;
      const path = "/api/v1/clusters/" + this.enc(this.cluster) +
        "/shards/" + this.enc(this.shard) + "/map" + (force ? "?refresh=1" : "");
      if (force) this.pollStopped = false;
      const res = await api.get(path);
      // 请求飞在路上时用户可能换了分片：旧分片的地图不能盖到新分片上。
      if (want !== this.mapKey) return;
      this.loading = false;
      if (!res.ok) {
        this.view = null;
        this._off = null;
        this.error = res.error || ("接口返回 " + res.status);
        this.scheduleDraw();
        return;
      }
      this.apply(res.data);
      if (res.data && res.data.pending && !this.pollStopped) this.schedulePoll();
    },
    apply(v) {
      if (!v || (v.cluster && v.cluster !== this.cluster) || (v.shard && v.shard !== this.shard)) return;
      this.view = v;
      if (v.pending) {
        this._polls = this._polls || 0;
      } else {
        this._polls = 0;
      }
      if (v.ready) {
        this._pointsAt = new Map();
        (v.points || []).forEach((p) => {
          const k = p.x + "," + p.y;
          const cur = this._pointsAt.get(k) || { build: 0, res: 0, danger: 0, companion: 0, subs: {} };
          cur[p.kind] = (cur[p.kind] || 0) + (p.count || 0);
          // 资源子类按名字并起来：同一格里既有树又有浆果丛时，读数条要分开说。
          if (p.subLabel) cur.subs[p.subLabel] = (cur.subs[p.subLabel] || 0) + (p.count || 0);
          this._pointsAt.set(k, cur);
        });
        this.buildTerrain();
      } else {
        this._off = null;
        this._pointsAt = new Map();
      }
      this.scheduleDraw();
      this.$nextTick(() => { this.bindCanvas(); this.scheduleDraw(); });
    },
    schedulePoll() {
      if (this._pollTimer) clearTimeout(this._pollTimer);
      this._polls = (this._polls || 0) + 1;
      if (this._polls > 12) {
        // 助手在跑但一直没写地图，多半是卡在别的命令上：停下自动轮询，
        // 把出口留给「重新导出」按钮，而不是每两秒发一次请求。
        this.pollStopped = true;
        return;
      }
      this._pollTimer = setTimeout(() => {
        this._pollTimer = null;
        this.load(false);
      }, 2500);
    },
    buildTerrain() {
      this._off = null;
      const v = this.view;
      if (!v || !v.ready || !v.grid || !v.tiles) { this.scheduleDraw(); return; }
      const g = v.grid;
      const pal = this.palette;
      const colors = (v.terrain || []).map((t) => hexRgb(pal[t.key] || pal.other));
      const last = colors.length ? colors[colors.length - 1] : [90, 96, 105];
      // 海岸线要认得出"哪几组是水"，用的是接口给的 key（中文 label 只给界面看）。
      // 近岸和远海是两组，只认 ocean 的话浪线会画在浅水里而不是滩边上。
      const water = {};
      (v.terrain || []).forEach((t, i) => { if (t.key === "ocean" || t.key === "coast") water[i] = 1; });
      const tiles = v.tiles;

      const off = document.createElement("canvas");
      off.width = g;
      off.height = g;
      const ctx = off.getContext("2d");
      const img = ctx.createImageData(g, g);
      const d = img.data;
      for (let y = 0; y < g; y++) {
        const row = y * g;
        for (let x = 0; x < g; x++) {
          const i = row + x;
          const cur = tiles[i];
          const c = colors[cur] || last;
          // 越界的邻居按"和自己同组"算，地图边缘就不会被描上一圈假边界。
          const left = x > 0 ? tiles[i - 1] : cur;
          const right = x + 1 < g ? tiles[i + 1] : cur;
          const up = y > 0 ? tiles[i - g] : cur;
          const down = y + 1 < g ? tiles[i + g] : cur;
          let k = cellShade(x, y);
          // 交界压暗只有 12%：再重就像在格子上描了一圈黑边，
          // 144 格的图缩到半屏时那种"棋盘感"就是这么来的。
          if (cur !== left || cur !== right || cur !== up || cur !== down) k *= 0.88;
          let r = c[0] * k, gr = c[1] * k, gb = c[2] * k;
          if (water[cur] && (!water[left] || !water[right] || !water[up] || !water[down])) {
            r += (236 - r) * 0.3; gr += (246 - gr) * 0.3; gb += (255 - gb) * 0.3;
          }
          const o = i * 4;
          d[o] = r; d[o + 1] = gr; d[o + 2] = gb; d[o + 3] = 255;
        }
      }
      ctx.putImageData(img, 0, 0);
      this._off = off;
      this._offTheme = state.theme;
      this.scheduleDraw();
    },
    // 高倍率下的底图：只遍历视野里那几千格，每格先铺底色再补自己的纹理，
    // 最后统一描交界线。这样放大看到的是"这一片是森林"，而不是"这一块是绿的"。
    // 个体图标层：只在高倍率走。一格里画不下的用 "+N" 角标兜底——
    // 一格 12 丛浆果在 8× 下只有三个图标的位置，硬叠就成一坨。
    drawEnts(ctx, w, h, dx, dy, s, dark) {
      const ui = uiColors(dark);
      const v = this.view, g = v.grid, side = g * s;
      const size = Math.max(10, Math.min(26, s * 0.66));
      const perCell = Math.max(1, Math.floor((s * s) / (size * size * 0.8)));
      const n0 = Math.max(0, Math.floor(-dx / s)), n1 = Math.min(g - 1, Math.ceil((w - dx) / s));
      const m0 = Math.max(0, Math.floor(-dy / s)), m1 = Math.min(g - 1, Math.ceil((h - dy) / s));
      const hidden = this.hidden;
      ctx.font = "600 10px system-ui, 'PingFang SC', 'Microsoft YaHei', sans-serif";
      ctx.textBaseline = "middle";
      for (let my = m0; my <= m1; my++) {
        for (let mx = n0; mx <= n1; mx++) {
          const list = this.entBuckets.get(my * g + mx);
          if (!list) continue;
          let shown = 0, left = 0, ax = 0, ay = 0, asub = "", anchored = false;
          for (const e of list) {
            if (hidden[e.kind] || (e.subLabel && hidden[e.subLabel])) continue;
            if (shown >= perCell) { left++; continue; }
            const px = dx + (e.tx / v.width) * side;
            const py = dy + (e.ty / (v.height || v.width)) * side;
            if (!anchored) { ax = px; ay = py; asub = e.subLabel; anchored = true; }
            drawEntIcon(ctx, e.kind, e.subLabel, px, py, size, dark);
            shown++;
          }
          if (left > 1 && anchored) {
            const txt = (this.zoom >= 7 && asub ? asub : "") + "+" + left;
            ctx.save();
            ctx.lineWidth = 3;
            ctx.strokeStyle = ui.countStroke;
            ctx.fillStyle = ui.text;
            ctx.strokeText(txt, ax + size * 0.8, ay);
            ctx.fillText(txt, ax + size * 0.8, ay);
            ctx.restore();
          }
        }
      }
      ctx.textBaseline = "alphabetic";
    },

    // 聚合层：一格一个点 / 一块晕染。没有个体点位（旧版助手）或低倍率时走这条。
    drawAggregated(ctx, w, h, dx, dy, s, dark, kp) {
      const ui = uiColors(dark);
    // 按缩放分级出信息：
    // <2.2 倍 一格一格的深浅晕染，只回答"哪一带有东西、密不密"。这时几千个圆点
    //          会把地形盖成地毯，青色资源点还常被读成一片水塘。
    // 2.2~5 倍 按类别画形状（方=建筑、圆=资源、三角=危险、菱=伙伴），描边才有意义。
    // ≥5 倍    形状旁边写出数量，"这一格有 12 个采集点"直接落在图上。
    // ≥5 倍且有个体点位：改画"每个实体一个图标"，聚合层整个让位——
    // 两套同时画只会互相压，而个体数据本来就包含建筑/危险/伙伴。
    const v = this.view;
    const wash = this.zoom < 2.2;
    const dotStroke = this.zoom >= 2;
    const showCount = this.zoom >= 5;
    const hidden = this.hidden;
    // 子类名（"浆果丛×12"）要更晚才出：三个汉字加计数约 50px，
    // 一格在 7 倍以下还不宽时就互相压字了。
    const labelSub = this.zoom >= 7;
    if (showCount) {
      ctx.font = "600 10px system-ui, 'PingFang SC', 'Microsoft YaHei', sans-serif";
      ctx.textBaseline = "middle";
    }
    (v.points || []).forEach((p) => {
      // 那一排"显示"开关在这里也必须生效：没有个体点位（旧版助手，或助手还没导出
      // 实体表）时高倍率走的正是这一条路径，之前只有 drawEnts 认 hidden，
      // 于是关掉"危险"图上一点反应都没有——开关看着坏了。
      if (hidden[p.kind] || (p.subLabel && hidden[p.subLabel])) return;
      const cx = dx + (p.x + 0.5) * s;
      const cy = dy + (p.y + 0.5) * s;
      if (cx < -12 || cy < -12 || cx > w + 12 || cy > h + 12) return;
      const n = p.count || 1;
      // 只有资源走晕染：它动辄几千格，逐个画点就把地形盖住了。
      // 建筑/危险/伙伴本来就少（一张图几十到几百个），低倍率下也要保留形状——
      // 把它们也晕成半透明方块，图上就会出现一块块说不是什么的淡斑。
      if (wash && p.kind === "res") {
        ctx.globalAlpha = Math.min(0.3, 0.07 + Math.sqrt(n) * 0.045);
        ctx.fillStyle = kp[p.kind] || MAP_COLORS.shared.fallback;
        ctx.fillRect(cx - s / 2, cy - s / 2, s, s);
        ctx.globalAlpha = 1;
        return;
      }
      // 点位的直径不能超过一格（s 是一格在屏幕上占几像素），否则一片连着的资源
      // 会糊成一整块纯色，看不出"这里有一堆树"和"这里有一整片树"的区别。
      const r = Math.min(clamp(1.1 + Math.sqrt(n) * 0.85 + s * 0.16, 1.5, 7), s * 0.45);
      ctx.globalAlpha = 0.88;
      ctx.fillStyle = kp[p.kind] || MAP_COLORS.shared.fallback;
      if (p.kind === "danger") {
        ctx.beginPath();
        ctx.moveTo(cx, cy - r * 1.15);
        ctx.lineTo(cx + r, cy + r * 0.8);
        ctx.lineTo(cx - r, cy + r * 0.8);
        ctx.closePath();
        ctx.fill();
        if (dotStroke) ctx.stroke();
      } else if (p.kind === "res") {
        ctx.beginPath();
        ctx.arc(cx, cy, r * 0.85, 0, Math.PI * 2);
        ctx.fill();
        if (dotStroke) ctx.stroke();
      } else if (p.kind === "companion") {
        // 跟着人走的东西单独一个形状：它和"这一带有什么"是两回事，
        // 而且底图是按需导出的，位置只代表导出那一刻。
        ctx.beginPath();
        ctx.moveTo(cx, cy - r);
        ctx.lineTo(cx + r, cy);
        ctx.lineTo(cx, cy + r);
        ctx.lineTo(cx - r, cy);
        ctx.closePath();
        ctx.fill();
        if (dotStroke) ctx.stroke();
      } else {
        ctx.fillRect(cx - r * 0.8, cy - r * 0.8, r * 1.6, r * 1.6);
        if (dotStroke) ctx.strokeRect(cx - r * 0.8, cy - r * 0.8, r * 1.6, r * 1.6);
      }
      ctx.globalAlpha = 1;
      if (showCount && n >= 3) {
        const txt = (labelSub && p.subLabel ? p.subLabel : "") + "×" + n;
        ctx.strokeStyle = ui.countStroke;
        ctx.fillStyle = ui.text;
        ctx.lineWidth = 3;
        ctx.strokeText(txt, cx + r + 2, cy);
        ctx.fillText(txt, cx + r + 2, cy);
      }
    });
    if (showCount) { ctx.textBaseline = "alphabetic"; ctx.lineWidth = 1; }
    },

    drawCells(ctx, w, h) {
      const v = this.view, g = v.grid, tiles = v.tiles;
      const keys = (v.terrain || []).map((t) => t.key);
      const { s, dx, dy } = this.geom();
      const n0 = clamp(Math.floor(-dx / s), 0, g), n1 = clamp(Math.ceil((w - dx) / s), 0, g);
      const m0 = clamp(Math.floor(-dy / s), 0, g), m1 = clamp(Math.ceil((h - dy) / s), 0, g);
      const pal = this.palette;
      for (let my = m0; my < m1; my++) {
        const row = my * g;
        for (let mx = n0; mx < n1; mx++) {
          this.paintCell(ctx, pal, keys[tiles[row + mx]] || "other", mx, my,
            dx + mx * s, dy + my * s, s);
        }
      }
      this.drawEdges(ctx, keys, tiles, g, n0, n1, m0, m1);
    },
    paintCell(ctx, pal, key, gx, gy, px, py, s) {
      const hex = pal[key] || pal.other;
      const shade = cellShade(gx, gy);
      ctx.fillStyle = tone(hex, shade);
      ctx.fillRect(px, py, s, s);
      const h = cellHash(gx, gy);
      const a = (h & 255) / 255, b = ((h >>> 8) & 255) / 255, c = ((h >>> 16) & 255) / 255;
      const u = Math.max(1, s * 0.12);          // 一笔的粗细，跟着格子一起放大
      const dk = tone(hex, shade * 0.72), lt = tone(hex, shade * 1.3);
      const kind = TEX[key] || "speck";
      if (kind === "blades") {
        ctx.fillStyle = dk;
        ctx.fillRect(px + a * s * 0.7, py + b * s * 0.5, u, s * 0.34);
        ctx.fillRect(px + c * s * 0.78, py + s * 0.2 + a * s * 0.42, u, s * 0.28);
        ctx.fillStyle = lt;
        ctx.fillRect(px + b * s * 0.72, py + s * 0.1 + c * s * 0.5, u, s * 0.22);
      } else if (kind === "clumps") {
        const r = s * 0.3;                       // 一团树冠，左上给一点高光
        ctx.fillStyle = dk;
        ctx.fillRect(px + a * (s - r), py + b * (s - r), r, r);
        ctx.fillRect(px + c * (s - r * 0.8), py + (1 - b) * (s - r * 0.8), r * 0.8, r * 0.8);
        ctx.fillStyle = lt;
        ctx.fillRect(px + a * (s - r) + r * 0.15, py + b * (s - r) + r * 0.12, r * 0.4, r * 0.28);
      } else if (kind === "pebble") {
        ctx.fillStyle = lt;                      // 砾石压在地面上
        ctx.fillRect(px + a * s * 0.6, py + b * s * 0.6, s * 0.26, s * 0.2);
        ctx.fillRect(px + c * s * 0.66, py + (1 - a) * s * 0.6, s * 0.18, s * 0.16);
        ctx.fillStyle = dk;
        ctx.fillRect(px + a * s * 0.6, py + b * s * 0.6 + s * 0.2, s * 0.26, u);
      } else if (kind === "waves") {
        ctx.fillStyle = lt;                      // 两道浪，第三道用暗色压住
        ctx.fillRect(px + a * s * 0.5, py + b * s * 0.62, s * 0.34, u);
        ctx.fillRect(px + c * s * 0.52, py + (1 - b) * s * 0.6, s * 0.26, u);
        ctx.fillStyle = dk;
        ctx.fillRect(px + b * s * 0.5, py + c * s * 0.62 + s * 0.12, s * 0.2, u);
      } else if (kind === "reeds") {
        ctx.fillStyle = lt;
        ctx.fillRect(px + a * s * 0.72, py + b * s * 0.4, u * 0.8, s * 0.44);
        ctx.fillRect(px + c * s * 0.72, py + (1 - a) * s * 0.4, u * 0.8, s * 0.36);
        ctx.fillStyle = dk;
        ctx.fillRect(px, py + b * s * 0.7, s, u);
      } else if (kind === "glow") {
        ctx.fillStyle = lt;                      // 蘑菇自己会亮，所以亮笔优先
        ctx.fillRect(px + a * s * 0.7, py + b * s * 0.7, u * 1.6, u * 1.6);
        ctx.fillRect(px + c * s * 0.74, py + (1 - b) * s * 0.72, u, u);
        ctx.fillStyle = tone(hex, shade * 0.62);
        ctx.fillRect(px + (1 - a) * s * 0.6, py + c * s * 0.6, s * 0.2, u);
      } else if (kind === "cross") {
        ctx.fillStyle = lt;                      // 蛛网：四小段近似一条斜丝
        ctx.fillRect(px + a * s * 0.6, py + b * s * 0.6, u, u);
        ctx.fillRect(px + a * s * 0.6 + u, py + b * s * 0.6 + u, u, u);
        ctx.fillRect(px + a * s * 0.6 + u * 2, py + b * s * 0.6 + u * 2, u, u);
        ctx.fillRect(px + c * s * 0.55, py + (1 - b) * s * 0.6, u, u);
      } else if (kind === "plank") {
        ctx.fillStyle = dk;                      // 人造地面：一块板一条缝
        ctx.fillRect(px, py + s * 0.86, s, u);
        ctx.fillRect(px + a * s * 0.7, py, u, s * 0.86);
      } else if (kind === "star") {
        ctx.fillStyle = lt;
        ctx.fillRect(px + a * s * 0.8, py + c * s * 0.8, u * 0.8, u * 0.8);
      } else {
        ctx.fillStyle = dk;                      // speck：沙、土、碎石地的细颗粒
        ctx.fillRect(px + a * s * 0.8, py + b * s * 0.8, u, u);
        ctx.fillRect(px + c * s * 0.82, py + a * s * 0.8, u, u);
        ctx.fillStyle = lt;
        ctx.fillRect(px + b * s * 0.8, py + c * s * 0.8, u, u);
      }
    },
    // 交界线：两坨颜色直接挨着读不出"这里换了一片林子"，一条暗线就有边界。
    // 水陆交界另描一道浅色——海岸线是这张图上最有用的信息之一（找落脚点是第一问）。
    drawEdges(ctx, keys, tiles, g, n0, n1, m0, m1) {
      const { s, dx, dy } = this.geom();
      const segs = [], coast = [];
      for (let my = m0; my < m1; my++) {
        const row = my * g;
        for (let mx = n0; mx < n1; mx++) {
          const i = row + mx, cur = tiles[i];
          const isW = keys[cur] === "ocean";
          if (mx + 1 < g && tiles[i + 1] !== cur) {
            const nb = keys[tiles[i + 1]] === "ocean";
            const x = dx + (mx + 1) * s, y = dy + my * s;
            (isW !== nb ? coast : segs).push([x, y, x, y + s]);
          }
          if (my + 1 < g && tiles[i + g] !== cur) {
            const nb = keys[tiles[i + g]] === "ocean";
            const x = dx + mx * s, y = dy + (my + 1) * s;
            (isW !== nb ? coast : segs).push([x, y, x + s, y]);
          }
        }
      }
      const stroke = (list, style) => {
        if (!list.length) return;
        ctx.strokeStyle = style;
        ctx.beginPath();
        for (let i = 0; i < list.length; i++) {
          const q = list[i];
          ctx.moveTo(q[0], q[1]);
          ctx.lineTo(q[2], q[3]);
        }
        ctx.stroke();
      };
      ctx.lineWidth = 1;
      stroke(segs, state.theme !== "light" ? "rgba(8,12,18,.5)" : "rgba(32,44,62,.3)");
      stroke(coast, state.theme !== "light" ? "rgba(206,234,255,.55)" : "rgba(255,255,255,.9)");
    },
    measure() {
      const wrap = this.$refs.wrap;
      if (!wrap) return;
      const w = wrap.clientWidth, h = wrap.clientHeight;
      if (w !== this.box.w || h !== this.box.h) {
        this.box.w = w;
        this.box.h = h;
      }
    },
    geom() {
      const g = (this.view && this.view.grid) || 1;
      const cw = this.box.w, ch = this.box.h;
      const base = Math.min(cw / g, ch / g) || 1;
      const s = base * this.zoom;
      const side = g * s;
      return {
        g, cw, ch, s, side,
        dx: (cw - side) / 2 + this.panX,
        dy: (ch - side) / 2 + this.panY,
      };
    },
    clampPan() {
      const { cw, ch, side } = this.geom();
      const mx = Math.max(0, (side - cw) / 2);
      const my = Math.max(0, (side - ch) / 2);
      this.panX = clamp(this.panX, -mx, mx);
      this.panY = clamp(this.panY, -my, my);
      if (this.zoom <= 1) { this.panX = 0; this.panY = 0; }
    },
    resetView() {
      this.zoom = 1;
      this.panX = 0;
      this.panY = 0;
      this.scheduleDraw();
    },
    zoomBy(f) {
      this.zoomAt(this.box.w / 2, this.box.h / 2, this.zoom * f);
    },
    // 放大上限按"一格最多 44 像素"算，而不是写死一个倍数：144 格的小地图本来就
    // 一格很大，固定 14 倍会放大成一堵纯色墙；512 格的地图 14 倍又还不够看清一棵树。
    zoomCap() {
      const g = (this.view && this.view.grid) || 1;
      const base = Math.min(this.box.w || 0, this.box.h || 0) / g || 1;
      // 一格放大到 64px 才停（原来是 44px）：个体图标要 20~26px 才认得出形状，
      // 44px 那一档一格里只塞得下两个，看着还是"一堆点"。
      return clamp(64 / base, 3, 26);
    },
    zoomAt(mx, my, z) {
      z = clamp(z, 1, this.zoomCap());
      if (z === this.zoom) return;
      // box 的键是 w/h（measure 从 wrap.clientWidth 写进去的那对）；解构错名字
      // 会把 pan 算成 NaN，之后每帧都空白。ui_audit 第四条专门盯这种写法。
      const cw = this.box.w, ch = this.box.h;
      const k = z / this.zoom;
      this.panX = mx - cw / 2 - (mx - cw / 2 - this.panX) * k;
      this.panY = my - ch / 2 - (my - ch / 2 - this.panY) * k;
      this.zoom = z;
      this.clampPan();
      this.scheduleDraw();
    },
    scheduleDraw() {
      if (this._raf) return;
      this._raf = requestAnimationFrame(() => {
        this._raf = 0;
        this.draw();
      });
    },
    draw() {
      const cv = this.$refs.cv;
      if (!cv) return;
      // 画之前再量一次：画布尺寸只在这里写，宁可每次多读一个 clientWidth，
      // 也不要出现"位图分辨率还是上次布局的、CSS 又把它拉伸成另一副比例"。
      this.measure();
      const dpr = window.devicePixelRatio || 1;
      const w = Math.max(1, Math.round(this.box.w));
      const h = Math.max(1, Math.round(this.box.h));
      const bw = Math.round(w * dpr), bh = Math.round(h * dpr);
      if (cv.width !== bw || cv.height !== bh) {
        cv.width = bw;
        cv.height = bh;
      }
      const ctx = cv.getContext("2d");
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, w, h);
      const v = this.view;
      if (!v || !v.ready || !this._off) return;
      if (this._offTheme !== state.theme) { this.buildTerrain(); return; }

      const { g, s, side, dx, dy } = this.geom();
      // 视野里的格子数决定走哪条路：放大到一格有十几像素时逐格画纹理（下面那条路），
      // 格子还很小、或者屏幕大到一帧要画两万多格时，回到离屏位图整体缩放——
      // 宁可少一点细节，也不要拖动时掉帧。
      const visCells = Math.ceil(w / s) * Math.ceil(h / s);
      if (s >= 7 && visCells <= 16000) {
        this.drawCells(ctx, w, h);
      } else {
        // 一格不到三个像素时最近邻采样会打出摩尔纹（海岸线锯齿尤其明显），
        // 这时双线性那一点糊反而更像一张航拍图。
        ctx.imageSmoothingEnabled = s < 3.2;
        ctx.drawImage(this._off, 0, 0, g, g, dx, dy, side, side);
      }

      const kp = this.kindPalette;
      const dark = state.theme !== "light";
      const ui = uiColors(dark);

      // 每 24 格一条参考线：缩放平移时给一个刻度感，也让人能口述"往东北两格"。
      // 只画落在视野里的那段，地图推到边角时不会把线甩到画布外。
      ctx.strokeStyle = ui.gridLine;
      ctx.lineWidth = 1;
      ctx.beginPath();
      for (let i = 24; i < g; i += 24) {
        const px = dx + i * s, py = dy + i * s;
        if (px > 0 && px < w) { ctx.moveTo(px, Math.max(0, dy)); ctx.lineTo(px, Math.min(h, dy + side)); }
        if (py > 0 && py < h) { ctx.moveTo(Math.max(0, dx), py); ctx.lineTo(Math.min(w, dx + side), py); }
      }
      ctx.stroke();

      ctx.lineWidth = 1;
      ctx.strokeStyle = ui.aggStroke;

      // 三层信息按缩放切换：低倍晕染、中倍聚合形状、高倍个体图标。
      // 两套不叠画：个体数据本来就包含建箑/危险/伙伴，叠着只会互相压。
      if (this.zoom >= 5 && this.entBuckets) this.drawEnts(ctx, w, h, dx, dy, s, dark);
      else this.drawAggregated(ctx, w, h, dx, dy, s, dark, kp);

      const labelSize = 11;
      ctx.font = "600 " + labelSize + "px " + "system-ui, 'PingFang SC', 'Microsoft YaHei', sans-serif";
      this.plotPlayers.forEach((p) => {
        const cx = dx + p.gx * s;
        const cy = dy + p.gy * s;
        if (cx < -20 || cy < -20 || cx > w + 20 || cy > h + 20) return;
        const rr = clamp(s * 1.05, 4.5, 9);
        if (this.hoverPlayer === p.userid) {
          ctx.beginPath();
          ctx.arc(cx, cy, rr + 3.5, 0, Math.PI * 2);
          ctx.lineWidth = 1.5;
          ctx.strokeStyle = ui.focus;
          ctx.stroke();
        }
        ctx.beginPath();
        ctx.arc(cx, cy, rr, 0, Math.PI * 2);
        ctx.fillStyle = p.dead ? MAP_COLORS.shared.playerDead : (charStyle(p.prefab).background || MAP_COLORS.shared.playerFallback);
        ctx.fill();
        ctx.lineWidth = 2;
        ctx.strokeStyle = MAP_COLORS.shared.playerRing;
        ctx.stroke();
        if (p.dead) {
          ctx.beginPath();
          ctx.moveTo(cx - rr * 0.55, cy - rr * 0.55);
          ctx.lineTo(cx + rr * 0.55, cy + rr * 0.55);
          ctx.moveTo(cx + rr * 0.55, cy - rr * 0.55);
          ctx.lineTo(cx - rr * 0.55, cy + rr * 0.55);
          ctx.lineWidth = 1.5;
          ctx.strokeStyle = MAP_COLORS.shared.playerCross;
          ctx.stroke();
        }
        const label = p.name || charName(p.prefab);
        if (this.hoverPlayer === p.userid || this.zoom >= 3.2) {
          const tw = ctx.measureText(label).width;
          const th = labelSize + 6;
          // 玩家贴着上边缘时标签翻到点下面，横向也夹进画布内：
          // 名字被裁掉一半，比不显示更难读。
          let ty = cy - rr - th - 3;
          if (ty < 1) ty = cy + rr + 3;
          if (ty + th > h - 1) ty = Math.max(1, h - th - 1);
          const tx = clamp(cx - tw / 2 - 4, 1, Math.max(1, w - tw - 9));
          ctx.fillStyle = ui.labelBg;
          ctx.fillRect(tx, ty, tw + 8, th);
          ctx.lineWidth = 1;
          ctx.strokeStyle = ui.labelLine;
          ctx.strokeRect(tx + 0.5, ty + 0.5, tw + 7, th - 1);
          ctx.fillStyle = ui.textAlt;
          ctx.textBaseline = "middle";
          ctx.fillText(label, tx + 4, ty + th / 2 + 0.5);
        }
      });

      if (this.hover) {
        const hx = dx + this.hover.x * s, hy = dy + this.hover.y * s;
        ctx.strokeStyle = ui.hoverLine;
        ctx.lineWidth = 1.5;
        ctx.strokeRect(hx + 0.5, hy + 0.5, Math.max(1, s - 1), Math.max(1, s - 1));
      }
    },
    cellAt(mx, my) {
      const v = this.view;
      if (!v || !v.ready) return null;
      const { g, s, dx, dy } = this.geom();
      const x = Math.floor((mx - dx) / s);
      const y = Math.floor((my - dy) / s);
      if (x < 0 || y < 0 || x >= g || y >= g) return null;
      return { x, y };
    },
    pointAt(mx, my) {
      const v = this.view;
      if (!v || !v.ready) return null;
      const { s, dx, dy } = this.geom();
      let best = null, bestD = 15 * 15;
      this.plotPlayers.forEach((p) => {
        const px = dx + p.gx * s, py = dy + p.gy * s;
        const d = (px - mx) * (px - mx) + (py - my) * (py - my);
        if (d < bestD) { bestD = d; best = p; }
      });
      return best;
    },
    setHover(mx, my) {
      const cell = this.cellAt(mx, my);
      if (!cell) {
        this.hover = null;
        this.hoverAt = null;
        this.hoverPlayer = null;
        return;
      }
      const terr = (this.view.terrain || [])[this.view.tiles[cell.y * this.view.grid + cell.x]];
      const pts = this._pointsAt.get(cell.x + "," + cell.y) || null;
      this.hover = {
        x: cell.x, y: cell.y,
        label: terr ? terr.label : "地形未知",
        build: pts ? pts.build : 0,
        res: pts ? pts.res : 0,
        danger: pts ? pts.danger : 0,
        companion: pts ? pts.companion : 0,
        // 资源子类拼成一句"浆果丛 12 · 树 3"：只有放大到看得出单格的倍率才有意义，
        // 而且它属于"这一格有什么"，不该占掉地形那一行的位置。
        subText: pts ? Object.keys(pts.subs).sort((a, b) => pts.subs[b] - pts.subs[a])
          .map((k) => k + " " + pts.subs[k]).join(" · ") : "",
      };
      const p = this.pointAt(mx, my);
      this.hoverPlayer = p ? p.userid : null;
      this.hoverAt = p || null;
    },
    pointerDown(e) {
      if (e.pointerType === "mouse" && e.button !== 0) return;
      const cv = this.$refs.cv;
      if (cv && cv.setPointerCapture) cv.setPointerCapture(e.pointerId);
      const r = cv.getBoundingClientRect();
      const pt = { x: e.clientX - r.left, y: e.clientY - r.top };
      this._ptrs.set(e.pointerId, pt);
      if (this._ptrs.size === 2) {
        const list = Array.from(this._ptrs.values());
        this._pinch = {
          dist: Math.hypot(list[0].x - list[1].x, list[0].y - list[1].y) || 1,
          midX: (list[0].x + list[1].x) / 2,
          midY: (list[0].y + list[1].y) / 2,
          zoom: this.zoom, panX: this.panX, panY: this.panY,
        };
        this._drag = null;
        return;
      }
      if (this._ptrs.size === 1) {
        this._drag = {
          id: e.pointerId, x: pt.x, y: pt.y,
          panX: this.panX, panY: this.panY, moved: false,
        };
        this._pointer = pt;
        this.hover = null;
        this.hoverAt = null;
        this.hoverPlayer = null;
      }
    },
    pointerMove(e) {
      const cv = this.$refs.cv;
      if (!cv) return;
      const r = cv.getBoundingClientRect();
      const mx = e.clientX - r.left, my = e.clientY - r.top;
      if (this._ptrs.has(e.pointerId)) this._ptrs.set(e.pointerId, { x: mx, y: my });

      if (this._pinch && this._ptrs.size >= 2) {
        const list = Array.from(this._ptrs.values());
        const dist = Math.hypot(list[0].x - list[1].x, list[0].y - list[1].y) || 1;
        const midX = (list[0].x + list[1].x) / 2;
        const midY = (list[0].y + list[1].y) / 2;
        const p = this._pinch;
        const z = clamp(p.zoom * (dist / p.dist), 1, this.zoomCap());
        const k = z / p.zoom;
        const cw = this.box.w, ch = this.box.h;
        this.panX = midX - cw / 2 - (p.midX - cw / 2 - p.panX) * k;
        this.panY = midY - ch / 2 - (p.midY - ch / 2 - p.panY) * k;
        this.zoom = z;
        this.clampPan();
        this.scheduleDraw();
        return;
      }

      if (this._drag && this._drag.id === e.pointerId) {
        const dx = mx - this._drag.x, dy = my - this._drag.y;
        if (Math.abs(dx) + Math.abs(dy) > 3) this._drag.moved = true;
        this.panX = this._drag.panX + dx;
        this.panY = this._drag.panY + dy;
        this.clampPan();
        this.scheduleDraw();
        return;
      }

      this._pointer = { x: mx, y: my };
      this.setHover(mx, my);
      this.scheduleDraw();
    },
    pointerUp(e) {
      if (this._ptrs.has(e.pointerId)) this._ptrs.delete(e.pointerId);
      if (this._ptrs.size < 2) this._pinch = null;
      if (this._drag && this._drag.id === e.pointerId) {
        const moved = this._drag.moved;
        this._drag = null;
        if (!moved && this._pointer) this.setHover(this._pointer.x, this._pointer.y);
      }
      if (this._ptrs.size === 1) {
        const left = Array.from(this._ptrs.entries())[0];
        this._drag = {
          id: left[0], x: left[1].x, y: left[1].y,
          panX: this.panX, panY: this.panY, moved: true,
        };
      }
    },
    wheel(e) {
      e.preventDefault();
      const r = this.$refs.cv.getBoundingClientRect();
      const f = e.deltaY < 0 ? 1.2 : 1 / 1.2;
      this.zoomAt(e.clientX - r.left, e.clientY - r.top, this.zoom * f);
    },
    locate(p) {
      if (!Number.isFinite(p.gx) || !Number.isFinite(p.gy)) return;
      this.zoom = Math.max(this.zoom, 4.5);
      const { g, cw, ch, s } = this.geom();
      const side = g * s;
      this.panX = side / 2 - p.gx * s;
      this.panY = side / 2 - p.gy * s;
      this.clampPan();
      this.hoverPlayer = p.userid;
      this.hoverAt = p;
      this.scheduleDraw();
    },
    pct(cells) {
      const total = (this.view && this.view.grid ? this.view.grid * this.view.grid : 0) || 1;
      const v = (cells / total) * 100;
      return v < 1 ? "<1%" : Math.round(v) + "%";
    },
    coordText(p) {
      if (p.wx === null || p.wx === undefined || p.wz === null || p.wz === undefined) return "";
      return Math.round(p.wx) + ", " + Math.round(p.wz);
    },
  },
  template: `
    <div class="col map-page" :class="{ 'map-full': full }" style="gap:14px">
      <!-- 没有房间时这一整行（房间下拉、世界切换、重新导出）都没有可操作的对象，
           留着一个点开还是空的下拉框，看着像页面坏了。 -->
      <div class="row" style="gap:12px;flex-wrap:wrap;align-items:flex-end" v-if="state.clusters.length">
        <div class="field" style="min-width:190px;flex:1 1 220px">
          <label for="map-cluster">当前房间</label>
          <select id="map-cluster" v-model="cluster">
            <option value="" disabled>选一个房间</option>
            <option v-for="c in state.clusters" :key="c.name" :value="c.name">{{ c.name }}</option>
          </select>
        </div>
        <div class="field" v-if="shardNames.length" style="flex:0 0 auto">
          <label>世界</label>
          <div class="seg" role="group" aria-label="选择世界">
            <button v-for="s in shardNames" :key="s" :class="s === shard ? 'on' : ''"
                    @click="shard = s">{{ placeName(s) }}</button>
          </div>
        </div>
        <span class="grow"></span>
        <div class="row" style="gap:8px">
          <x-badge v-if="view && view.pending" kind="info" :text="loading ? '正在问助手' : '正在导出'"></x-badge>
          <x-badge v-else-if="view && view.stale" kind="warn" text="底图已过期"></x-badge>
          <button class="btn ghost sm" @click="load(true)" :disabled="loading || !shard">
            {{ loading ? '读取中' : '重新导出' }}</button>
        </div>
      </div>

      <x-empty v-else icon="map" title="这台机器上还没有房间"
        note="地图是游戏里的助手 MOD 导出来的：先建一个房间、把世界开起来，助手才会开始往面板送地形。">
        <a class="btn sm primary" href="#/room">去开服向导</a>
      </x-empty>

      <x-note v-if="view && view.note" kind="info">{{ view.note }}</x-note>
      <x-note v-if="pollStopped" kind="warn">
        地图导出比平时久。助手可能正忙着别的命令，先看看
        <a class="linkish" href="#/tasks">任务中心</a>，或者稍后点「重新导出」。
      </x-note>
      <x-note v-if="error" kind="bad">
        {{ error }}
        <div class="row" style="margin-top:8px">
          <button class="btn sm" @click="load(true)" :disabled="loading || !shard">再试一次</button>
        </div>
      </x-note>

      <x-sk v-if="loading && !ready && !error" :lines="4"></x-sk>

      <template v-if="cluster && shard && !error && (ready || loading || (view && view.pending))">
        <div class="grid grid-wide map-layout">
          <section class="card">
            <div class="card-head">
              <span class="ico-tile tint-teal"><x-icon name="world" :size="15"></x-icon></span>
              <h3>{{ placeName(shard) }}地形</h3>
              <x-badge v-if="ready" kind="ok"
                       :text="view.width + '×' + view.height + ' 格'"></x-badge>
              <span class="grow"></span>
              <!-- 整句操作说明收进气泡：这一页的主角是图，不是说明文字；
                   手机上没有悬停，气泡点得开，所以两个端都只占一枚小标签。 -->
              <x-hint label="怎么操作" :text="gestureHint"></x-hint>
              <div class="btn-group map-tools">
                <button class="btn sm ghost" @click="zoomBy(1 / 1.3)" :disabled="zoom <= 1" aria-label="缩小地图">−</button>
                <button class="btn sm ghost" @click="zoomBy(1.3)" :disabled="zoom >= zoomCap()" aria-label="放大地图">＋</button>
                <button class="btn sm ghost" @click="resetView" :disabled="zoom === 1 && !panX && !panY">复位</button>
                <button class="btn sm ghost" @click="toggleFull"
                        :title="full ? '退出全屏（也可以按 Esc）' : '让这张图占满整个屏幕'"
                        aria-label="全屏看地图"><x-icon :name="full ? 'collapse' : 'expand'" :size="14"></x-icon></button>
              </div>
            </div>

            <div class="map-canvas-wrap" ref="wrap" :style="wrapStyle">
              <canvas ref="cv" class="map-canvas" aria-label="世界地形图"></canvas>
              <div class="map-overlay" v-if="!ready">
                <div class="col" style="gap:6px;align-items:center;text-align:center">
                  <span class="map-spin"></span>
                  <b>{{ view && view.pending ? '助手正在导出地形' : '读取中' }}</b>
                  <span class="hint">第一次导出要采样整张地图，通常几秒内完成。</span>
                </div>
              </div>
            </div>

            <div class="map-readout">
              <template v-if="hoverAt">
                <span class="player-avatar sm" :style="charStyle(hoverAt.prefab)">{{ charShort(hoverAt.prefab) }}</span>
                <b>{{ hoverAt.name || charName(hoverAt.prefab) }}</b>
                <span class="hint">{{ charName(hoverAt.prefab) }}</span>
                <x-badge v-if="hoverAt.dead" kind="bad" text="已倒地"></x-badge>
                <span class="hint mono" v-if="coordText(hoverAt)">{{ placeName(shard) }} {{ coordText(hoverAt) }}</span>
              </template>
              <template v-else-if="hover">
                <span class="chip mono">{{ hover.x + 1 }}, {{ hover.y + 1 }}</span>
                <b class="map-readout-name">{{ hover.label }}</b>
                <span class="map-key-item" v-if="hover.build"><i class="mki build"></i>建筑 {{ hover.build }}</span>
                <span class="map-key-item" v-if="hover.res"><i class="mki res"></i>资源 {{ hover.res }}</span>
                <span class="map-key-item" v-if="hover.danger"><i class="mki danger"></i>危险 {{ hover.danger }}</span>
                <span class="map-key-item" v-if="hover.companion"><i class="mki companion"></i>伙伴 {{ hover.companion }}</span>
                <span class="hint" v-if="hover.subText">{{ hover.subText }}</span>
                <span class="hint" v-if="!hover.build && !hover.res && !hover.danger && !hover.companion">这一格没有建筑或资源</span>
              </template>
              <span class="hint" v-else>选中一格看它的地形和点位</span>
            </div>

            <div class="map-key">
              <span class="map-key-item"><i class="mki build"></i>建筑</span>
              <span class="map-key-item"><i class="mki res"></i>资源</span>
              <span class="map-key-item"><i class="mki danger"></i>危险</span>
              <span class="map-key-item"><i class="mki companion"></i>伙伴随从</span>
              <span class="map-key-sep"></span>
              <span class="map-key-item"><i class="mki player"></i>玩家（按角色）</span>
              <span class="map-key-item"><i class="mki dead"></i>倒地的玩家</span>
            </div>

            <div class="map-filter" v-if="zoom >= 5 && filterOptions.length">
              <span class="map-filter-label">显示</span>
              <button v-for="o in filterOptions" :key="o.key" class="chipbtn"
                      :class="{ off: hidden[o.key] }" @click="toggleFilter(o.key)">{{ o.label }}</button>
              <x-hint label="为什么有 +N" text="一格塞不下的个体不硬叠，用 +N 标出还有多少个没画。"></x-hint>
            </div>

            <div class="map-cover" v-if="ready">
              <b class="map-cover-note">{{ coverSummary }}</b>
              <x-hint label="这条怎么读" text="占比条按格子数分段，颜色就是地图上的颜色。"></x-hint>
            </div>
            <div class="map-cover-bar" v-if="ready">
              <i v-for="t in view.terrain" :key="'c-' + t.key" class="mcs"
                 :style="{ flexGrow: t.cells, background: palette[t.key] || palette.other }"
                 :title="t.label + ' ' + t.cells + ' 格'"></i>
            </div>

            <div class="map-legend" v-if="ready">
              <span class="map-legend-item" v-for="t in view.terrain" :key="t.key">
                <i class="mli" :style="{ background: palette[t.key] || palette.other }"></i>
                {{ t.label }}
                <span class="num">{{ t.cells }}</span>
                <span class="hint">{{ pct(t.cells) }}</span>
              </span>
            </div>
          </section>

          <div class="col" style="gap:14px">
            <section class="card" v-if="ready">
              <div class="card-head">
                <span class="ico-tile tint-gold"><x-icon name="layers" :size="15"></x-icon></span>
                <h3>这张图里有什么</h3>
                <span class="grow"></span>
                <span class="hint" v-if="view.updatedAt">{{ rel(view.updatedAt) }}导出</span>
              </div>
              <div class="grid map-stats">
                <x-stat label="建筑" icon="cube" tint="tint-gold" :value="view.totals.build" hint="箱子、墙、工作台"></x-stat>
                <x-stat label="资源" icon="tree" tint="tint-green" :value="view.totals.res" hint="树、石头、浆果丛"></x-stat>
                <x-stat label="危险" icon="skull" tint="tint-rose" :value="view.totals.danger" hint="蜘蛛巢、触手、猎狗窝"></x-stat>
              </div>
              <x-kv class="mt" :rows="[
                { k: '世界尺寸', v: view.width + ' × ' + view.height + ' 格' },
                { k: '采样网格', v: view.grid + ' × ' + view.grid },
                { k: '导出时间', v: view.updatedAt ? rel(view.updatedAt) : '未知' },
              ]"></x-kv>
              <p class="hint mt-0" style="margin-top:8px">
                地形和建筑资源是助手按需导出的；玩家位置走每秒的状态数据，所以人在动、底图不用重导。
              </p>
            </section>

            <section class="card">
              <div class="card-head">
                <span class="ico-tile tint-green"><x-icon name="users" :size="15"></x-icon></span>
                <h3>这个世界的玩家</h3>
                <x-badge :kind="livePlayers.length ? 'ok' : (playersUnknown ? (shardObj && shardObj.helperNoteUrgent ? 'warn' : 'idle') : 'idle')"
                         :text="livePlayers.length ? livePlayers.length + ' 人' : (playersUnknown ? '这一刻问不到' : '没人在线')"></x-badge>
              </div>
              <p class="hint" style="margin:0 0 8px">{{ playerNote }}</p>

              <div class="list-plain" v-if="livePlayers.length">
                <div class="player-list-row" v-for="p in livePlayers" :key="p.userid">
                  <span class="player-avatar sm" :style="charStyle(p.prefab)">{{ charShort(p.prefab) }}</span>
                  <div class="col" style="gap:0;min-width:0">
                    <div class="row" style="gap:6px;flex-wrap:nowrap">
                      <b class="ellipsis">{{ p.name || charName(p.prefab) }}</b>
                      <x-badge v-if="p.dead" kind="bad" text="倒地"></x-badge>
                    </div>
                    <span class="hint ellipsis">
                      {{ charName(p.prefab) }}<template v-if="coordText(p)"> · {{ coordText(p) }}</template>
                    </span>
                  </div>
                  <span class="grow"></span>
                  <button v-if="Number.isFinite(p.gx)" class="btn sm ghost" @click="locate(p)">定位</button>
                  <span v-else class="hint">没采到位置</span>
                </div>
              </div>
              <div class="hint" v-else>
                {{ playersUnknown
                  ? '地图上的点停在最后一次上报。这不代表没人在玩——上面那句就是这一刻读不到的原因。'
                  : (shard === 'Caves' ? '这个世界现在没有玩家。有人在洞穴时会出现在这里。' : '这个世界现在没有玩家。有人进服后会出现在这里。') }}
              </div>
            </section>
          </div>
        </div>
      </template>

      <x-empty v-else-if="cluster && shard && !loading && !error" icon="map"
               title="还没有这张地图"
               note="地图要由游戏里的助手导出：先把这个世界开起来，等它进入存档后回到这一页。"></x-empty>
    </div>`,
};
