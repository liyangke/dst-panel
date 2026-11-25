// 玩家在线时间线：摘要条 + 有上限泳道（#119，2026-11-05 用户在三个方案里选定乙）。
//
// 2026-11-06 #125 把它从总览搬到「玩家管理」：总览是第一眼，回答"多少人、什么形状"，
// 逐人在场段这种明细住在这里更对（花名册答"这些人是谁"，这张答"他们什么时候来、待多久"）。
// 组件本身一行没改，改的只有它挂在哪个页面、以及谁发那一次 player-timeline 请求。
//
// 横向位置只认接口给的那一条 from/to，段落怎么收尾也只认它给的 reason / reasonLabel /
// clippedStart："他在线多久"这件事 store 是唯一说了算的地方，前端一参与推断，
// 就会出现"界面画着还在里面、库里早写了他走了"。
//
// 三种形状各表一件事，不只靠颜色：实心绿是这会儿还在（开口段）、半透明是已经收口、
// 左沿一条虚线是"这一段没有起点记录，从窗口边界画起"。形状各表一件事是因为
// 这张图整屏都是青绿色系，光靠深浅分不出"哪种是现在"。
//
// 为什么是这个形状：原来"一人一行、行高写死 15px"，这张卡的高度 = 人数 × 行高。
// 满房 24 人时它自己长到 400 多 px，把总览右列的底部推来推去（用户先问的是"右列下面缺不缺东西"，
// 量下去才发现真正的不稳定来自这里）——一张图的高度不该由"今天来了几个人"决定。
//
// 换完之后这张卡分两层，各回答一件事：
//   摘要条（28px，永远这么高）：每人一段，段宽 = 这一窗里他的在场时长占比 → "谁待得最久"；
//   泳道（整块最多 260px）：还是每人一条轨道，行高 = 预算分给条数，最宽 30px → "谁在什么时候来的"。
// 预算和上限在 2026-11-05 一起抬过一次：这张卡挪出右列、横跨整幅之后，行高不再是"省着给"的东西，
// 而 12px 那一档薄轨道上的记号点根本认不出颜色。
// 人少时一人一条；排到 13 名及以后的人并成最后一条"其他 N 人"（只剩一两个人时不并，直接摊开），
// 那条里每个人的段仍然逐段画出来，所以"段数等于接口段数"这条对账不破。
import { clock, dateText, seconds } from "./fmt.js";
// rel 只能用 store.js 那一份：它包的是 fmt.ago，但会读一下 state.tick，
// 秒针走过一秒这一句才会重算；直接 import fmt 的 ago 会把"32 秒前"钉死在首次渲染那个数上。
import { rel } from "./store.js";

// 时间窗那四档。总览的「在线情况」「行为构成」和这一页的在线时间线用的是同一个筛选，
// 所以只做一份放在这里（#125）：各留一份的话，两处迟早漂成"上面选的近 3 天、下面写着近 7 天"。
// 窗口一变后端给的格子大小跟着变（近 6 小时每分钟一个点，近 7 天每 30 分钟一个点），
// 这几个数只在"要不要重新问一次"时有用；图上那句"多少分钟一个点"由后端的 note 来说，前端不自己拼。
export const TL_WINDOWS = [
  { value: 6, label: "近 6 小时" },
  { value: 24, label: "近 24 小时" },
  { value: 72, label: "近 3 天" },
  { value: 168, label: "近 7 天" },
];

const TL_SHOW = 10;        // 泳道最多摊开几条，多出来的并进"其他"
const TL_MERGE = 2;        // 剩下的人不多于这个数就不做"其他"那一条，直接全摊开
const TL_LANES_MAX = 260;  // 泳道整块的高度预算（px），CSS 那边不再设 max-height
const TL_ROW_GAP = 4;      // 泳道行间距，和 app.css 里 .tl2-lanes 的 gap 是同一个数
const TL_ROW_MAX = 30;     // 人少时的行高上限，别让一条轨道细成一条缝
const TL_HUES = ["azure", "cyan", "teal", "green", "gold", "amber", "coral", "rose", "violet", "slate"];

export default {
  name: "PlayerTimeline",
  props: { data: { type: Object, default: null } },
  setup() { return { clock, dateText, seconds, rel }; },
  data() { return { trackW: 0 }; },
  mounted() {
    // 轴那一行里的轨道；泳道行也有 .tl-track，所以带上 .tl-axis 这一层限定。
    const el = this.$el && this.$el.querySelector(".tl-axis .tl-track");
    if (!el) return;
    this.trackW = el.getBoundingClientRect().width;
    // 轨道宽度是这张卡自己挤出来的（右栏只有 ~250px，手机上更窄），只能量，不能猜。
    this._ro = new ResizeObserver((es) => { this.trackW = es[0].contentRect.width; });
    this._ro.observe(el);
  },
  unmounted() { if (this._ro) this._ro.disconnect(); },
  computed: {
    win() {
      const d = this.data;
      if (!d || !d.from || !d.to) return null;
      const a = Date.parse(d.from), b = Date.parse(d.to);
      if (!Number.isFinite(a) || !Number.isFinite(b) || b <= a) return null;
      return { a, b, span: b - a };
    },
    people() { return this.data && this.data.people ? this.data.people : []; },
    // 轴上的刻度：一条轨道放得下几枚，由这条轨道有多宽决定。
    // 原来固定五枚、跨天时每枚写满"10-28 05:25"（约 74px），右栏那张卡只有 255px 的轨道，
    // 五枚直接压成一串糊字（2026-11-04 截图里那行 "10-2810529723:17…"）。
    ticks() {
      const w = this.win;
      if (!w) return [];
      // #122 第二问：轴上原来只保证"排得开、不压字"，2226 那一档量到的是
      // 13:06 / 19:06 / 01:06 / 07:06 / 13:06——首末两枚同字，读者分不清最后那枚是今天还是明天。
      // 根因是这里的判据写成了"窗口超过 48 小时"，而 24 小时这一窗照样跨过零点。
      // 现在改成看钟点会不会重样：窗口 ≥21 小时就可能有两枚落在同一个钟点上。
      const repeats = w.span >= 21 * 3600 * 1000;
      // 带日期那一枚实测 71px（11px 字号），只写钟点的约 36px；预算各留一点余量。
      // 首尾两枚是贴着轨头/轨尾对齐的（不居中），它把整枚宽度都吃在一边，
      // 所以先扣掉半枚的宽度再数能放几枚——不扣就会像 2026-11-05 那一版，
      // 手机那一档算出 4 枚、第 2 枚压住第 1 枚 4px。
      const labelPx = repeats ? 74 : 46;
      const n = Math.max(2, Math.min(5, Math.floor((this.trackW - labelPx / 2) / labelPx) + 1));
      const out = [];
      let prev = null;
      for (let i = 0; i < n; i++) {
        const ms = w.a + (w.span * i) / (n - 1);
        const day = dateText(ms).slice(5);
        // 只在"日历日换过去"的那一枚（和第一枚）带日期，其余只写钟点，窄轨才放得下。
        // 近 3 天 / 近 7 天每一枚都是新的一天，所以每枚都带日期——原来那句"三个 00:00 分不清哪天"照旧成立。
        out.push({ p: (100 * i) / (n - 1), text: day === prev ? clock(ms) : day + " " + clock(ms) });
        prev = day;
      }
      return out;
    },
    // 摘要条和泳道都按"这一窗里待得最久的排前面"：读者最想知道的是谁来得最久，
    // 而接口那份顺序是第一次出现的先后，两个人互换位置都不会被发现，排序才对得上摘要条。
    ranked() {
      return this.people.slice().sort((a, b) => (b.seconds || 0) - (a.seconds || 0));
    },
    shown() { return this.ranked.slice(0, this.ranked.length - this.rest.length); },
    rest() {
      // 只把一两个人折进"其他 N 人"那条反而更难看（写着"其他 1 人"），
      // 所以人数刚过线时全摊开，多出来的确实成批才并。
      return this.ranked.length > TL_SHOW + TL_MERGE ? this.ranked.slice(TL_SHOW) : [];
    },
    total() { return this.people.reduce((s, p) => s + (p.seconds || 0), 0); },
    restTotal() { return this.rest.reduce((s, p) => s + (p.seconds || 0), 0); },
    lanes() { return this.shown.length + (this.rest.length ? 1 : 0); },
    // 行高由"这一块的预算减去行间距之后分给几条"算出来，向下取整。
    // 间距必须一起算：只拿预算除条数的话，十二条各 12.5px 加上十一条 2px 间距实际是 172px，
    // 比预算高出 22px，最后一条会被整块的高度上限无声裁掉。
    rowH() {
      if (!this.lanes) return 0;
      return Math.floor(Math.min(TL_ROW_MAX, (TL_LANES_MAX - TL_ROW_GAP * (this.lanes - 1)) / this.lanes));
    },
    trackH() { return Math.max(6, Math.round(this.rowH - 4)); },
    // "其他 N 人"那条不画一条平均色了事：把这几个人的在场段逐段画上去，
    // 叠着的地方自然加深，读的是"这段时间里这几个人里有人"。
    restSpans() {
      const out = [];
      this.rest.forEach((p) => (p.spans || []).forEach((sp) => out.push(sp)));
      return out.sort((a, b) => String(a.from || "").localeCompare(String(b.from || "")));
    },
    // 图例只列这一窗里真画出来的形状，文字也一律取自接口：
    // 死亡/复活/幽灵消散的短名是后端那份行为目录给的（和事件页同一个来源），
    // 前端另写一套的话，两边迟早会对不上。
    markKinds() {
      const seen = {}, out = [];
      this.shown.forEach((p) => (p.marks || []).forEach((m) => {
        if (seen[m.event]) return;
        seen[m.event] = true;
        out.push({ event: m.event, tone: this.tone(m.event), label: m.label });
      }));
      return out;
    },
    spanKinds() {
      const has = { open: false, closed: false, clip: false };
      this.people.forEach((p) => (p.spans || []).forEach((sp) => {
        if (sp.reason === "open") has.open = true; else has.closed = true;
        if (sp.clippedStart) has.clip = true;
      }));
      const out = [];
      if (has.clip) out.push({ key: "clip", label: "这一段没有起点记录" });
      if (has.closed) out.push({ key: "", label: "这一段已经结束" });
      if (has.open) out.push({ key: "open", label: "这会儿还在里面" });
      return out;
    },
  },
  methods: {
    pct(iso) {
      const w = this.win;
      return w ? ((Date.parse(iso) - w.a) / w.span) * 100 : 0;
    },
    barStyle(sp) {
      const a = this.pct(sp.from);
      // 最窄 0.7%：近 7 天那一窗里，一次二十分钟的进出按真实比例只有两个像素，
      // 看着像"他根本没来过"，而这一行的全部作用就是让人看见他来没来过。
      return { left: a + "%", width: Math.max(this.pct(sp.to) - a, 0.7) + "%" };
    },
    dotStyle(m) { return { left: this.pct(m.at) + "%" }; },
    hue(i) { return "var(--h-" + (TL_HUES[i] || "slate") + ")"; },
    // flex 给的是占比（0~1），最小 0.6%：只来了一分钟的人不该在条上彻底消失。
    flexOf(p) { return this.total ? Math.max(100 * (p.seconds || 0) / this.total, 0.6) / 100 : 1; },
    spanTitle(sp) {
      return clock(sp.from, true) + " → " + clock(sp.to, true) +
        "（" + seconds((Date.parse(sp.to) - Date.parse(sp.from)) / 1000) + "）" +
        (sp.clippedStart ? " · 这一段没有起点记录，从窗口边界画起" : "") +
        " · " + sp.reasonLabel;
    },
    // 摘要条那一段和泳道那一行的悬停是同一句话（2026-11-05 用户："悬停文案改成和摘要条同一句话（口径唯一）"）：
    // 原来两处各写一套时长口径——一处「在线 57 分 12 秒（1/2）」、一处「这个窗口里一共在线 57 分 12 秒」，
    // 同一件事迟早飘成两个说法。这里只留一句，后面补的两项（账户编号、现在在不在）是泳道才需要的。
    rowTitle(p, i) {
      return p.name + " · 在线 " + seconds(p.seconds) + "（" + (i + 1) + "/" + this.ranked.length + "）" +
        " · " + (p.userid || "没有账户编号：引擎的进出公告只带名字，他改了名会被当成两个人") +
        " · " + (p.onlineNow ? "现在还在里面" : "最后一次活动 " + rel(p.lastAt));
    },
    restTitle() {
      return "其余 " + this.rest.length + " 人 · 合计在线 " + seconds(this.restTotal) +
        " · 这几个人按总时长排在 " + TL_SHOW + " 名之后，段还是逐段画的，叠着的地方颜色深";
    },
    // 码值只用来挑形状与颜色，上屏的是接口那句中文 label。
    tone(ev) { return ev === "player_resurrect" ? "revive" : ev === "ghost_gone" ? "ghost" : "death"; },
  },
  template: `
    <div class="tl2">
      <!-- 第一层：摘要条。每个人一段，段宽就是他在这一窗里的在场时长占比。
           这一条永远 28px，人数变化只改段宽不改高度。 -->
      <div class="tl2-stack">
        <i v-for="(p, i) in ranked" :key="'k' + (p.userid || p.name)" :style="{flex: flexOf(p), background: hue(i)}"
           :title="rowTitle(p, i)"></i>
      </div>
      <div class="tl2-total">
        <span>{{ people.length }} 人 · 合计 {{ seconds(total) }}</span>
        <span v-if="rest.length">前 {{ shown.length }} 名单列，其余 {{ rest.length }} 人并进最后一条</span>
      </div>

      <!-- 每个人的时长做成文字段（2026-11-05 用户："时长那一列不该存在……把每个人的时长并入摘要条那一层"）。
           原来它们站在泳道右边，那一列把轴和泳道的轨道撑成两个宽度（实测轴 1798px / 泳道 1731px，
           差的就是这一列），最后一枚刻度因此不再对着横条的终点。
           顺序照摘要条那一段一段来（待得最久的在前），前面那枚点就是那一段的颜色。 -->
      <div class="tl2-sums" v-if="shown.length">
        <span v-for="(p, i) in shown" :key="'t' + (p.userid || p.name)" :title="rowTitle(p, i)">
          <i :style="{ background: hue(i) }"></i>{{ p.name }} {{ seconds(p.seconds) }}</span>
        <span v-if="rest.length" class="rest"><i></i>其他 {{ rest.length }} 人 {{ seconds(restTotal) }}</span>
      </div>

      <div class="tl-row tl-axis" v-if="win">
        <span class="tl-who"></span>
        <span class="tl-track">
          <!-- 首尾两枚的位移在 app.css 的 .tl-tick.at-end / .at-mid 里；0% 那一枚本来就贴着轨头，
               不给类名（原来挂着个没有样式的 at-start，看着像有规则在管它）。 -->
          <i v-for="t in ticks" :key="t.p" class="tl-tick" :style="{ left: t.p + '%' }"
             :class="t.p === 100 ? 'at-end' : (t.p > 0 ? 'at-mid' : '')">{{ t.text }}</i>
        </span>
      </div>

      <!-- 第二层：泳道。行高由"这一块的预算分给几条"算出来，人再多也只是每条变薄，
           整块不超过 260px——以前是每人固定一行，满房时这张卡自己长到四百多 px。 -->
      <div class="tl2-lanes">
        <div class="tl-row" v-for="(p, i) in shown" :key="p.userid || p.name"
             :style="{ height: rowH + 'px' }" :title="rowTitle(p, i)">
          <span class="tl-who">
            <b class="tl-live" v-if="p.onlineNow"></b>
            <span class="tl-name">{{ p.name }}</span>
          </span>
          <span class="tl-track" :style="{ height: trackH + 'px' }">
            <i class="tl-span" v-for="(sp, k) in p.spans" :key="'s' + k"
               :class="{ open: sp.reason === 'open', clip: sp.clippedStart }"
               :style="barStyle(sp)" :title="spanTitle(sp)"></i>
            <b class="tl-mark" v-for="(m, k) in p.marks || []" :key="'m' + k" :class="tone(m.event)"
               :style="dotStyle(m)" :title="m.label + ' · ' + clock(m.at, true)"></b>
          </span>
        </div>

        <!-- 并出来的这一条和别的泳道一样只有"名字 + 轨道"两格。 -->
        <div class="tl-row other" v-if="rest.length" :style="{ height: rowH + 'px' }" :title="restTitle">
          <span class="tl-who"><span class="tl-name">其他 {{ rest.length }} 人</span></span>
          <span class="tl-track" :style="{ height: trackH + 'px' }">
            <i class="tl-span rest" v-for="(sp, k) in restSpans" :key="'r' + k"
               :style="barStyle(sp)" :title="spanTitle(sp)"></i>
          </span>
        </div>
      </div>

      <div class="tl-legend" v-if="spanKinds.length || markKinds.length">
        <span v-for="k in spanKinds" :key="'b' + k.key"><i class="k" :class="k.key"></i>{{ k.label }}</span>
        <span v-for="k in markKinds" :key="'d' + k.event"><i class="d" :class="k.tone"></i>{{ k.label }}</span>
      </div>
    </div>`,
};
