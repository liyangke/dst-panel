// 世界规则那 189 项的图标与配色。
//
// 为什么按 key 匹配、不按中文标签匹配：标签是文案，#53 那次清扫已经改过一轮、以后还会改；
// 而 key 是引擎写进 leveldataoverride.lua 的字段名，改它等于换存档格式。
// 拿一个不会动的东西当依据，这张表才不会因为改了个"的"字就整片掉回默认图标。
//
// 为什么不逐项硬写：189 项里有三十来项是同一个东西的"数量"和"再生速度"两条
// （berrybush / berrybush_regrowth），写两遍就会改一遍漏一遍。所以先查精确键，
// 再剥一层壳（mutated_ / _regrowth 这类）查本体的精确键，再查包含关系（长的优先），
// 最后才落到分组默认值。
//
// 图标名全部要能在 icons.js 里找到——ui_audit 第十条会查，拼错一个字母这里就是空图。
// 同一张表还顺带报覆盖率（232 / 270 那一句）：字典以后加字段，覆盖率往下掉就是提醒。

export const GROUP_ICON = {
  // 一组一档色：一屏十组、四十多个下拉框，眼睛先按颜色分块，再按形状找具体那一项。
  // 顺序跟着接口给的分组走，改这里不会动到存档，只是让相邻两组别撞色。
  global: { icon: "calendar", tint: "tint-slate" },
  survivors: { icon: "user", tint: "tint-green" },
  misc: { icon: "world", tint: "tint-azure" },
  resources: { icon: "plant", tint: "tint-teal" },
  portal_resources: { icon: "portal", tint: "tint-violet" },
  animals: { icon: "paw", tint: "tint-gold" },
  monsters: { icon: "skull", tint: "tint-coral" },
  giants: { icon: "crown", tint: "tint-amber" },
  lunar_mutations: { icon: "moon", tint: "tint-cyan" },
  events: { icon: "flag", tint: "tint-rose" },
};

// 精确键：包含关系会认错的，都在这里写死（"start_location" 里有 "star"、
// "beequeen" 里有 "bee"，靠顺序赌不如写下来）。
const FIELD_ICON = {
  // ---- 全局 ----
  season_start: "calendar", specialevent: "flag", autumn: "leaf", winter: "snow",
  spring: "sprout", summer: "sun", day: "clock", spawnmode: "pin",
  ghostenabled: "ghost", portalresurection: "portal", ghostsanitydrain: "ghost",
  resettime: "clock", beefaloheat: "paw", krampus: "gift",
  // ---- 冒险家 ----
  extrastartingitems: "gift", seasonalstartingitems: "gift", spawnprotection: "shield",
  dropeverythingondespawn: "chest", healthpenalty: "heart", lessdamagetaken: "shield",
  temperaturedamage: "thermo", hunger: "meat", darkness: "eyeoff",
  shadowcreatures: "ghost", brightmarecreatures: "sparkle",
  // ---- 世界 ----
  task_set: "map", start_location: "pin", world_size: "expand",
  branching: "route", loop: "route", roads: "route", touchstone: "rock",
  boons: "sparkle", prefabswaps_start: "shuffle", balatro: "sparkle",
  frograin: "rain", hunt: "meat", alternatehunt: "sparkle", junkyard: "chest",
  lightning: "bolt", lunarhail_frequency: "snow", meteorshowers: "star",
  moon_fissure: "portal", petrification: "rock", stageplays: "moon",
  terrariumchest: "chest", wanderingtrader_enabled: "bag",
  weather: "rain", wildfires: "flame",
  // 洞穴那几项在 Master 的字典里没有，容易漏：光照 / 酸雨 / 通往月岛的闸 / 地震 / 蠕虫
  cavelight: "sun", acidrain_enabled: "rain", atriumgate: "portal",
  earthquakes: "pulse", wormattacks: "worm", wormattacks_boss: "worm",
  // ---- 资源再生 ----
  berrybush: "berry", marshbush: "berry", moon_berrybush: "berry", banana: "berry",
  bananabush: "berry", cactus: "cactus", grass: "grass", reeds: "plant",
  moon_tree: "tree", moon_sapling: "sprout", moon_rock: "rock",
  moon_starfish: "fish", moon_hotspring: "thermo", moon_bullkelp: "plant",
  ocean_bullkelp: "plant", ocean_seastack: "drop", saltstack_regrowth: "rock",
  flint: "rock", rock: "rock", rock_ice: "snow", mushroom: "shroom",
  fern: "sprout", lichen: "leaf", wormlights: "bug", meteorspawner: "star",
  ponds: "drop", cave_ponds: "drop",
  tumbleweed: "wind", basicresource_regrowth: "refresh",
  // ---- 生物 ----
  beefalo: "paw", bees: "bug", bees_setting: "bug", birds: "bird", butterfly: "bug",
  buzzard: "bird", catcoon: "tree", catcoons: "tree", gnarwail: "fish",
  lightninggoat: "bolt", moon_carrot: "sprout", moon_fruitdragon: "fish",
  ocean_shoal: "fish", ocean_wobsterden: "crab", penguins: "bird", perd: "bird",
  wobsters: "crab", grassgekkos: "plant", bunnymen: "paw",
  mushgnome: "shroom", slurper: "rock", slurtles: "rock", snurtles: "rock",
  lightfliers: "bug", monkey: "paw",
  // ---- 敌对生物 ----
  angrybees: "bug", chess: "gear", cookiecutters: "gear", frogs: "bug",
  lureplants: "plant", mosquitos: "bug", ocean_waterplant: "plant",
  pirateraids: "fish", sharks: "fish", squid: "fish", tallbirds: "bird",
  tentacles: "plant", walrus: "paw", walrus_setting: "paw", wasps: "bug",
  moon_spiders: "web", worms: "worm", chest_mimics: "chest", itemmimics: "bag",
  fissure: "portal", nightmarecreatures: "ghost", merm: "fish",
  // ---- 巨兽 ----
  // 全用王冠的话这一组十六项就是一个图。巨兽各自是一种东西，所以按本体画：
  // 冰鹿是雪、龙蝇是火、蜂王是虫、克劳斯是礼物（它就是捡礼物那个）。
  antliontribute: "coin", bearger: "paw", beequeen: "bug", crabking: "crab",
  daywalker: "skull", daywalker2: "skull", deciduousmonster: "leaf", deerclops: "snow",
  dragonfly: "flame", eyeofterror: "eye", fruitfly: "berry", goosemoose: "flower",
  klaus: "gift", liefs: "tree", malbatross: "bird", sharkboi: "fish",
  spiderqueen: "web", toadstool: "shroom",
  // ---- 月亮变异 ----
  moon_spider: "web", penguins_moon: "bird",
  // ---- 传送门资源 ----
  portal_spawnrate: "portal",
  // ---- 活动（十二项全是"某年开不开"，共用一枚旗子等于十二行空图）----
  // 按那一年纪念的东西画：兔人之年住蘑菇房、猪王之年收金币、座狼之年看满月。
  crow_carnival: "sun", hallowed_nights: "ghost", winters_feast: "snow",
  year_of_the_gobbler: "bird", year_of_the_varg: "moon", year_of_the_pig: "coin",
  year_of_the_carrat: "sprout", year_of_the_beefalo: "paw",
  year_of_the_catcoon: "tree", year_of_the_bunnyman: "shroom",
  year_of_the_dragonfly: "flame", year_of_the_snake: "worm",
};

// 包含关系（命中即取，按字符串长度从长到短试）：同一类东西的复数、_setting、
// _regrowth 这些变体不用一个个列。
// 这里刻意不放 mutated / portalrate：它们是"壳"，放进来会因为比本体长而先命中，
// 十个变异就都成了同一个月亮。壳由下面 candidates() 剥。
const FIELD_ICON_PART = {
  regrowth: "refresh", portal: "portal",
  spiders: "web", spider: "web", hound_mounds: "paw", hound: "paw",
  bees: "bug", bee: "bug", birds: "bird", bird: "bird", buzzard: "bird",
  bats: "bug", bat: "bug", wasp: "bug", moth: "bug", flier: "bug", worm: "worm",
  beefalo: "paw", bunnymen: "paw", pigs: "paw", moles: "paw", otters: "paw",
  rabbits: "sprout", monkeytail: "plant", powder_monkey: "paw", monkey: "paw",
  merm: "fish", sharks: "fish", fish: "fish", ocean: "fish",
  wobster: "crab", crab: "crab", palmcone: "tree", evergreen: "tree",
  deciduous: "leaf", twiggytrees: "tree", tree: "tree", sapling: "sprout",
  carrot: "sprout", berrybush: "berry", grass: "grass", reeds: "plant",
  kelp: "plant", flower: "flower", shroom: "shroom", mushroom: "shroom",
  cactus: "cactus", rock: "rock", pond: "drop", lightcrab: "crab",
  moon: "moon", rift: "portal", season: "calendar", shadow: "ghost",
  ghost: "ghost", sanity: "ghost", heat: "thermo",
};

const PARTS = Object.keys(FIELD_ICON_PART).sort((a, b) => b.length - a.length);

const PREFIXES = ["mutated_", "moon_"];
const SUFFIXES = ["_regrowth", "_portalrate"];

/** 一个键可能要查的几种写法：原样 → 剥前缀 → 剥后缀 → 两头都剥。 */
function candidates(key) {
  const out = [key];
  const push = (s) => { if (s && out.indexOf(s) < 0) out.push(s); };
  const unprefix = (s) => {
    for (const p of PREFIXES) if (s.length > p.length && s.indexOf(p) === 0) return s.slice(p.length);
    return "";
  };
  for (const s of SUFFIXES) {
    if (key.length > s.length && key.slice(-s.length) === s) push(key.slice(0, -s.length));
  }
  push(unprefix(key));
  for (let i = 1; i < out.length; i++) push(unprefix(out[i]));
  return out;
}

function partIcon(name) {
  for (let i = 0; i < PARTS.length; i++) {
    if (name.indexOf(PARTS[i]) >= 0) return FIELD_ICON_PART[PARTS[i]];
  }
  return "";
}

/** 这一项该用哪枚图标：精确键 → 剥壳后的精确键 → 包含关系 → 分组默认。永远给得出一个名字。 */
export function fieldIcon(key, group) {
  const k = key || "";
  const cands = candidates(k);
  for (let i = 0; i < cands.length; i++) {
    const hit = FIELD_ICON[cands[i]];
    if (hit) return hit;
  }
  for (let i = 0; i < cands.length; i++) {
    const hit = partIcon(cands[i]);
    if (hit) return hit;
  }
  return groupIcon(group);
}

/** 这一组那一档色。组名来自接口，认不出来就用页面主色（不硬编一个错的色档）。 */
export function groupTint(group) {
  return (GROUP_ICON[group] || {}).tint || "";
}

/** 分组标题上那枚图标：就是这一组没有精确命中时各项用的那个默认值。 */
export function groupIcon(group) {
  return (GROUP_ICON[group] || { icon: "list" }).icon;
}
