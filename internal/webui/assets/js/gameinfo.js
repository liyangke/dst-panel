// 游戏侧名字到界面说法的映射。
//
// prefab 是引擎内部的角色名（wilson、wx78……），直接把码值摆到界面上
// 等于让用户自己查表。这里只做"认得的翻中文、认不得的给个中性说法"，
// 认不得时绝不回退成 prefab 原文。
//
// 键名必须是引擎自己那一份 prefab，不是英文艺名：麦斯威尔是 `waxwell`、
// 女武神是 `wathgrithr`（2026-11-04 从服务端自带的 `data/bigportraits/` 里取的清单，
// 每个角色一枚 `<prefab>_none`，那一档就是引擎认为存在的全部可玩角色）。
// 写成 `maxwell`、`wigfrid` 的话这两个角色永远走"未识别"那支，界面上看不出漏了。
const CHARACTERS = {
  wilson: ["威尔逊", "威尔"],
  willow: ["薇洛", "薇洛"],
  wolfgang: ["沃尔夫冈", "沃尔"],
  wendy: ["温蒂", "温蒂"],
  wx78: ["WX-78", "WX"],
  wickerbottom: ["薇克伯顿", "薇克"],
  woodie: ["伍迪", "伍迪"],
  wes: ["韦斯", "韦斯"],
  waxwell: ["麦斯威尔", "麦斯"],
  wathgrithr: ["薇格弗德", "薇格"],
  winona: ["薇诺娜", "薇诺"],
  wagstaff: ["瓦格斯塔夫", "瓦格"],
  warly: ["沃利", "沃利"],
  woby: ["沃比", "沃比"],
  wortox: ["沃拓克斯", "沃拓"],
  wormwood: ["沃姆伍德", "沃姆"],
  wurt: ["沃特", "沃特"],
  walter: ["沃尔特", "沃尔"],
  wanda: ["旺达", "旺达"],
  wonkey: ["猴王", "猴王"],
  webber: ["韦伯", "韦伯"],
};

const PLACES = { Master: "地表", Caves: "洞穴" };

export function charName(prefab) {
  const c = CHARACTERS[prefab];
  return c ? c[0] : "角色未识别";
}

export function charShort(prefab) {
  const c = CHARACTERS[prefab];
  return c ? c[1] : "人";
}

// 名字→固定色：同一个字符串在哪一页都是同一个颜色，不依赖中文译名也不会随刷新变。
// 角色头像和模组色块共用这一套，所以两处看到的"同一个绿"真的是同一个东西。
export function nameColor(s) {
  let h = 0;
  for (const ch of String(s || "")) h = (h * 31 + ch.charCodeAt(0)) % 360;
  return { background: "hsl(" + h + " 46% 38%)" };
}

// 头像底色按 prefab 定。
export function charStyle(prefab) {
  return nameColor(prefab);
}

export function placeName(shard) {
  return PLACES[shard] || shard || "未知世界";
}
