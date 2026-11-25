// 模组卡片的共用零件：封面（含没图时的色块兜底）、收藏/评分那一格、订阅热度条。
//
// 为什么单开一个文件而不是每个视图各写一遍：模组页有三个屏
// （找模组 / 装到这台机器 / 给这个世界开），同一个模组的同一张封面在三处都得长成
// 同一个样子——同一个色块算法、同一句悬停、同样"图挂了退回色块"。写三份的话
// 改一处忘两处，用户看到的就是"检索页有图、配置页没图"这种不一致（这次要修的正是它）。
import { nameColor } from "./gameinfo.js";
import { count, num } from "./fmt.js";

// 没有预览图（磁盘来源、图床挂了）时色块上的字：中文名取前两个，英文名取每个词的首字母
// （最多两个）。工坊标题长度不可控，有个固定的视觉锚点才扫得开。
export function tileText(name, fallback) {
  const n = String(name || fallback || "?").trim();
  if (/[^\x00-\x7f]/.test(n)) return n.slice(0, 2);
  const words = n.split(/[\s_\-.]+/).filter(Boolean);
  if (words.length > 1) return (words[0][0] + words[1][0]).toUpperCase();
  return n.slice(0, 2).toUpperCase();
}

// 色块底色按"ID|名字"取，不是只按名字：
// 只按名字的话两个同名不同 ID 的模组会撞成同一个颜色；
// 只按 ID 的话磁盘上那些 workshop-数字的条目全挤在同一档色相里（实测一屏一个色）。
export function tileStyle(it) {
  return nameColor((it.workshopId || it.dir || "") + "|" + (it.name || ""));
}

// ModCover 自己记住"这张图挂过"：预览图是浏览器在加载时才知道 403/404 的，
// 父层拿不到那个事件，只能由图自己退回色块。
export const ModCover = {
  props: {
    it: { type: Object, required: true },
    // badge：一张可选的角标（{kind,text,title}），检索页用它标"已装/没装"。
    badge: { type: Object, default: null },
    // cls：额外套一个尺寸类。详情层那格的宽度和卡片里那格不是一回事。
    cls: { type: String, default: "" },
  },
  data() { return { broken: false }; },
  computed: {
    src() { return !this.broken && this.it.previewUrl ? this.it.previewUrl : ""; },
    text() { return tileText(this.it.name, this.it.workshopId || this.it.dir); },
    style() { return tileStyle(this.it); },
    alt() { return this.it.name ? this.it.name + " 的预览图" : "模组预览图"; },
  },
  template: `
    <div class="mod-cover" :class="cls" :style="style">
      <img v-if="src" class="mod-cover-img" :src="src" :alt="alt"
           loading="lazy" decoding="async" @error="broken = true">
      <span v-else class="mod-cover-ph">{{ text }}</span>
      <span class="mod-cover-state" v-if="badge">
        <x-badge :kind="badge.kind" :text="badge.text" :title="badge.title"></x-badge>
      </span>
    </div>`,
};

// rate 是收藏数与工坊评分那一格。两个数各说各的，不并成一个"评分"：
// favorited 是工坊上那颗星按下去的次数（实测热门模组收藏 10 万级、订阅 300 万级）；
// voteScore 是 Steam 自己的 vote_data.score，实测（2026-11-03，400 条样本）它是加权分，
// 和 votes_up/(up+down) 并不相等（290/400 偏离超过 2%，投票少的模组差得更远），
// 所以这里只写「工坊评分」，绝不说成"多少投票里的百分之几"那种推导口气。
// 没有投票时 votesTotal 为 0，那一格整个不画：0 个人评分画一个分数是编出来的。
export function rate(it) {
  const out = {};
  const fav = it.favorited | 0;
  if (fav > 0) out.fav = { text: count(fav), exact: num(fav) + " 人收藏" };
  const votes = it.votesTotal | 0;
  if (votes > 0 && it.voteScore > 0) {
    const p = Math.round(it.voteScore * 100);
    out.score = { text: p + "%", exact: "工坊评分 " + p + "%（" + num(votes) + " 人投过票）" };
  }
  if (!out.fav && !out.score) return null;
  return out;
}

// 订阅热度条。条长按对数折：热门的二百多万、冷门的几百，线性画的话大半页都是空条。
// heatMax 必须由调用方给"这一屏里最高的那条"——接口不提供全局分位数，
// 拿一个编出来的满分画就是假刻度。
export function heat(it, heatMax) {
  const s = it.subscriptions | 0;
  if (s < 1 || heatMax < 2) return null;
  const pct = Math.max(6, Math.round(Math.log10(Math.max(10, s)) /
                                     Math.log10(Math.max(10, heatMax)) * 100));
  // 条边那一格只留 count（"850 万"）：千分位的八位数要占 90px，条就被挤没了。
  // 它是取整过的，所以精确那句放在悬停标题和详情层里。
  return { pct: Math.min(100, pct), text: count(s), exact: num(s) + " 人订阅" };
}

export const heatNote =
  "条长和条边那个数都是订阅人数：条按对数折算（热门的二百多万、冷门的几百，" +
  "线性画的话大半页都是空条），数最多留三位有效数字，精确值把指针停在数上就能看到。" +
  "订阅数只说明少人在用，不说明好不好——好不好看的是卡片上那颗星（收藏数）和工坊评分。";
