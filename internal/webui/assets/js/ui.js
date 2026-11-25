// 复用组件。都只做一件事：把"接口给的字段"变成"有颜色、有形状、有文字的一句"。
//
// 这里不许出现任何接口调用：组件只吃 props。这样总览页里的一个分片行
// 和后面模组页里的一个分片行是同一个东西，改样式只需要改一处。
import { state, answerConfirm, dropToast, rel,
  toggleNoticeCenter, clearNotices, runUndo, dismissUndo } from "./store.js";
import { bytes, bytesPair, seconds } from "./fmt.js";
import { ICONS } from "./icons.js";
import { ModCover } from "./modcard.js";

const { computed } = Vue;

// 图标：一个 v-for 把 path 数组画完，所以整站没有 innerHTML/v-html，
// 也不需要为深浅主题各画一套（描边用 currentColor，跟着文字颜色走）。
// 名字写错时不静默吞掉——退化成"?"并留一句控制台警告，否则界面会莫名其妙少个图标。
const Icon = {
  props: { name: { type: String, required: true }, size: [Number, String] },
  computed: {
    paths() {
      const p = ICONS[this.name];
      if (p) return p;
      if (typeof console !== "undefined") console.warn("没有这个图标名：" + this.name);
      return ICONS.help;
    },
    style() {
      return this.size ? { width: this.size + "px", height: this.size + "px" } : null;
    },
  },
  template: `
    <svg class="ic" :style="style" viewBox="0 0 24 24" aria-hidden="true" focusable="false">
      <path v-for="(d, i) in paths" :key="i" :d="d"></path>
    </svg>`,
};

// Hint 是"这段说明不该占一整行"的去处：PC 上悬浮/键盘聚焦出现，
// 触屏上点一下出现、点别处或滚动就收起。文案仍然在 DOM 里（只是不铺开），
// 所以读屏和 Ctrl+F 都还能找到它。
//
// 气泡用 fixed 并按按钮的位置算左边距：老写成 absolute + translateX(-50%)，
// 卡片右边那一枚提示会把气泡顶出屏幕，手机上进而多出一条横向滚动条。
const Hint = {
  props: { text: String, icon: { type: String, default: "" }, size: [Number, String],
           label: String,                       // 给了 label 就不再是纯图标，而是一枚带字的把手
           tone: { type: String, default: "" } },  // warn / bad：这枚把手背后是问题而不是解释
  computed: {
    // 带字的那一枚用向下箭头：它的意思是"点开看全文"，不是"这里有个疑问"。
    glyph() { return this.icon || (this.label ? "down" : "help"); },
    // 上百字的说明铺满一整块气泡会盖住下面的表单，也读不进去。
    // 先给一句短的，要看全文再点一下；slot 版本由调用方自己控制长度。
    longText() { return (this.text || "").length > 80; },
    brief() { return this.longText && !this.expanded ? this.text.slice(0, 80) + "…" : (this.text || ""); },
  },
  data() { return { open: false, box: null, expanded: false }; },
  methods: {
    show() {
      const r = this.$el.getBoundingClientRect();
      const w = Math.min(300, window.innerWidth - 24);
      const left = Math.max(12, Math.min(r.left + r.width / 2 - w / 2, window.innerWidth - w - 12));
      // 上方放不下（贴在顶栏下面）就翻到下面，取空间大的那一侧。
      const above = r.top > 150;
      this.box = Object.assign(
        { width: w + "px", left: left + "px" },
        above ? { bottom: (window.innerHeight - r.top + 8) + "px" }
              : { top: (r.bottom + 8) + "px" });
      this.open = true;
    },
    hide() { this.open = false; },
    toggle() { this.open ? this.hide() : this.show(); },
    outside(e) { if (this.open && this.$el && !this.$el.contains(e.target)) this.hide(); },
  },
  mounted() {
    // 捕获阶段听 scroll：气泡是相对视口定位的，页面一滚就必须重算，
    // 与其算不如直接收起——滚动时手指已经不在那一行了。
    window.addEventListener("scroll", this.hide, true);
    window.addEventListener("resize", this.hide);
    document.addEventListener("click", this.outside, true);
  },
  unmounted() {
    window.removeEventListener("scroll", this.hide, true);
    window.removeEventListener("resize", this.hide);
    document.removeEventListener("click", this.outside, true);
  },
  template: `
    <span class="tip" :class="open ? 'open' : ''" @keydown.escape="hide">
      <button type="button" class="tip-btn" :class="[label ? 'labeled' : '', tone]"
              :aria-expanded="open ? 'true' : 'false'"
              :aria-label="label || '这条说明'" @mouseenter="show" @mouseleave="hide"
              @focus="show" @blur="hide" @click="toggle">
        <span v-if="label">{{ label }}</span>
        <x-icon :name="glyph" :size="size || 15"></x-icon>
      </button>
      <span class="tip-bub" role="tooltip" v-if="open" :style="box">
        <slot v-if="$slots.default"></slot>
        <template v-else><span>{{ brief }}</span>
          <button v-if="longText" type="button" class="linkish" @click.stop="expanded = !expanded">
            {{ expanded ? "收起" : "展开全文" }}</button></template>
      </span>
    </span>`,
};

// Pager 是这一版新加的：以前只有"上一页/下一页"两个灰按钮，
// 用户既不知道一共几页，也不能直接跳。现在页码、总数、跳转都在一条上，
// 窄屏只留"上一页 · 第 x/y · 下一页"，页码横向滚而不是换行。
const Pager = {
  props: {
    page: { type: Number, default: 1 },
    size: { type: Number, default: 12 },
    total: { type: Number, default: 0 },     // 0 = 后端没给总数，只能"猜有没有下一页"
    hasMore: { type: Boolean, default: false },
    loading: { type: Boolean, default: false },
    unit: { type: String, default: "条" },
  },
  emits: ["go"],
  computed: {
    pages() { return this.total > 0 ? Math.max(1, Math.ceil(this.total / this.size)) : 0; },
    // 页码窗口：首尾常在，当前页左右各一个，中间断了用省略号占位。
    // 全列出来的话翻到第 20 页时这一条会横向撑满屏幕。
    steps() {
      const last = this.pages;
      if (!last) return [];
      const want = new Set([1, last, this.page, this.page - 1, this.page + 1]);
      const nums = [...want].filter((n) => n >= 1 && n <= last).sort((a, b) => a - b);
      const out = [];
      nums.forEach((n, i) => {
        if (i && n - nums[i - 1] > 1) out.push("…");
        out.push(n);
      });
      return out;
    },
    nextEnabled() { return this.pages ? this.page < this.pages : this.hasMore; },
    countText() {
      return this.total > 0 ? "共 " + this.total + " " + this.unit : "";
    },
    // 一条都没有的时候这一排整条都不该在：空列表没有"下一页"可翻，
    // 而两个灰按钮加一句"第 1 页"会让人以为下面本来该有东西、是没加载出来
    // （冷启动实测里模组页就是这样：空状态 + 一排在旁边的分页条）。
    useful() {
      return this.page > 1 || this.total > 0 || this.hasMore;
    },
  },
  methods: {
    go(p) {
      if (p < 1 || p === this.page) return;
      this.$emit("go", this.pages ? Math.min(p, this.pages) : p);
    },
  },
  template: `
    <nav class="pager" :class="steps.length ? '' : 'no-nums'" aria-label="分页" v-if="useful">
      <button class="pg-btn" :disabled="page <= 1 || loading" @click="go(page - 1)">
        <x-icon name="left" :size="16"></x-icon><span>上一页</span></button>
      <span class="pg-nums" v-if="steps.length">
        <template v-for="(s, i) in steps" :key="i">
          <span class="pg-gap" v-if="s === '…'">…</span>
          <button v-else class="pg-num" :class="s === page ? 'on' : ''"
                  :aria-current="s === page ? 'page' : null" @click="go(s)">{{ s }}</button>
        </template>
      </span>
      <span class="pg-fallback">第 {{ page }} 页</span>
      <button class="pg-btn" :disabled="!nextEnabled || loading" @click="go(page + 1)">
        <span>下一页</span><x-icon name="right" :size="16"></x-icon></button>
      <span class="pg-total" v-if="countText">{{ countText }}</span>
    </nav>`,
};

// Seg 是分段控件（主题、视图切换）。用 role=radiogroup 而不是并排按钮：
// 它们是互斥的同一件事，读屏要能听出"三选一，当前第二项"。
const Seg = {
  props: {
    options: { type: Array, required: true },   // [{value, label, icon}]
    modelValue: { type: [String, Number], required: true },
    label: String,
  },
  emits: ["update"],
  template: `
    <div class="seg" role="group" :aria-label="label || '切换'">
      <button v-for="o in options" :key="o.value" type="button"
              :class="o.value === modelValue ? 'on' : ''" :aria-pressed="o.value === modelValue ? 'true' : 'false'"
              @click="$emit('update', o.value)">
        <x-icon v-if="o.icon" :name="o.icon" :size="15"></x-icon><span>{{ o.label }}</span>
      </button>
    </div>`,
};

// Stat 是页顶那几个数字块。以前每页自己写一遍 div + 内联样式，
// 于是同一个"当前在线"在三个页面上有三种大小——现在只有这一种。
//
// 类名沿用各页已经在用的 stat-k / stat-v / stat-note：这样老页面不改也能拿到新样式，
// 新页面则只需要写 <x-stat>。
const Stat = {
  props: {
    label: String, value: [String, Number],
    hint: String,                                 // 收进按需提示的气泡，不占一行
    tone: { type: String, default: "" },          // ok / warn / bad / info
    icon: String,                                 // 有就在标签前放一枚图标
    tint: String,                                 // tint-开头的色板名（不给就跟着本页主色）
  },
  template: `
    <div class="stat" :class="[tone, tint]">
      <div class="stat-k"><x-icon v-if="icon" :name="icon" :size="14"></x-icon><span>{{ label }}</span>
        <x-hint v-if="hint" :text="hint"></x-hint></div>
      <div class="stat-v">{{ value }}</div>
      <div class="stat-note" v-if="$slots.default"><slot></slot></div>
    </div>`,
};

const Chip = {
  props: { text: String, tone: { type: String, default: "" }, icon: String, title: String },
  template: `
    <span class="chip" :class="tone" :title="title || ''">
      <x-icon v-if="icon" :name="icon" :size="13"></x-icon><span>{{ text }}</span>
    </span>`,
};

// 状态标记：颜色 + 形状 + 文字三样一起（见 app.css 顶部第 3 条约定）。
// Badge 的 text 之后留一个 slot：窄屏要藏掉的那半截（比如 pid）挂在这个 slot 上，
// 而不是另开一个 props 传"手机版文案"——一个状态两份文案迟早会对不上。
const Badge = {
  props: { kind: { type: String, default: "idle" }, text: { type: String, required: true }, title: String },
  template: `<span class="badge" :class="kind" :title="title || ''">{{ text }}<slot></slot></span>`,
};

const Note = {
  props: { kind: { type: String, default: "info" } },
  template: `<div class="note" :class="kind"><slot></slot></div>`,
};

// Empty 是"这里确实没有东西"，和"还没加载出来"（Sk）严格分开。
// 混用会让人以为接口坏了，或者以为房间里真的没发生过任何操作。
// icon 只收 js/icons.js 里的名字，写错了由 Icon 自己退化成占位图标并在控制台留话。
const Empty = {
  props: { icon: { type: String, default: "info" }, title: String, note: String },
  template: `
    <div class="empty">
      <div class="e-ico"><x-icon :name="icon" :size="26"></x-icon></div>
      <div class="e-title" v-if="title">{{ title }}</div>
      <div class="e-note" v-if="note">{{ note }}</div>
      <div class="row" style="justify-content:center;margin-top:12px"><slot></slot></div>
    </div>`,
};

const Sk = {
  props: { lines: { type: Number, default: 3 } },
  template: `<div class="col">
      <div v-for="n in lines" :key="n" class="sk" :style="{width: (n===lines? 55 : 100)+'%'}"></div>
    </div>`,
};

// Bar 给血量/饥饿/理智这类"有上限的当前值"。上限缺失或为 0 时报"未知"而不是画满。
const Bar = {
  props: { value: [Number, String], max: [Number, String], kind: { type: String, default: "" }, label: String },
  computed: {
    ratio() {
      const v = Number(this.value), m = Number(this.max);
      if (!Number.isFinite(v) || !Number.isFinite(m) || m <= 0) return 0;
      return Math.max(0, Math.min(100, (v / m) * 100));
    },
    unknown() {
      return this.value === null || this.value === undefined || !Number.isFinite(Number(this.max)) || Number(this.max) <= 0;
    },
  },
  template: `
    <div class="col" style="gap:3px">
      <div class="row spread" style="font-size:var(--t-xs)">
        <span class="muted">{{ label }}</span>
        <span class="num">{{ unknown ? "未知" : Math.round(Number(value)) + " / " + Math.round(Number(max)) }}</span>
      </div>
      <div class="bar" :class="kind"><i :style="{width: unknown ? '0' : ratio + '%'}"></i></div>
    </div>`,
};

// Ring 是一枚占比表盘：CPU、内存、存档磁盘各一颗。
//
// 形状选型：这类"一个带上限的百分比"用进度条会排成一列同质的横杠，扫一眼分不出
// 三件事；圆环把数字顶到中心，颜色只负责"紧不紧张"，三颗并排就是一台机器的体检表。
// 阈值（70 / 88）留在组件里而不是每个页面各写一遍：同一屏里"内存黄、磁盘绿"
// 必须意味着同一档占用。
//
// 拿不到可信数字时不画弧也不报 0%：圆心放那句中文说明。
// "刚开始采样"和"占用为零"是两件事，写成 0% 会让人以为这台机器很闲。
const RING_R = 26;
const Ring = {
  props: {
    label: { type: String, required: true },
    icon: String,
    percent: [Number, String],
    head: String,          // 圆心大字
    sub: String,           // 圆心第二行：可对质的原始数
    note: String,          // 表盘下面那一行
    hint: String,          // 挂在标签后面那句：这一项到底统计的是什么
    tone: { type: String, default: "" },
  },
  computed: {
    p() {
      const v = Number(this.percent);
      if (!Number.isFinite(v) || this.percent === null || this.percent === undefined || this.percent === "") return null;
      return Math.max(0, Math.min(100, v));
    },
    toneKey() {
      if (this.tone) return this.tone;
      if (this.p === null) return "idle";
      return this.p >= 88 ? "bad" : this.p >= 70 ? "warn" : "ok";
    },
    dash() {
      const c = 2 * Math.PI * RING_R;
      const v = (this.p === null ? 0 : this.p / 100) * c;
      return v.toFixed(2) + " " + (c - v).toFixed(2);
    },
  },
  template: `
    <div class="ring" :class="toneKey">
      <div class="ring-dial">
        <svg class="ring-svg" viewBox="0 0 64 64" aria-hidden="true" focusable="false">
          <circle class="ring-track" cx="32" cy="32" r="26"></circle>
          <circle v-if="p !== null" class="ring-fill" cx="32" cy="32" r="26"
                  :stroke-dasharray="dash" transform="rotate(-90 32 32)"></circle>
        </svg>
        <div class="ring-mid">
          <div class="ring-head">{{ head || (p === null ? "—" : Math.round(p) + "%") }}</div>
          <div class="ring-sub" v-if="sub">{{ sub }}</div>
        </div>
      </div>
      <div class="ring-label">
        <x-icon v-if="icon" :name="icon" :size="14"></x-icon><span>{{ label }}</span>
        <x-hint v-if="hint" :text="hint" :size="13"></x-hint>
      </div>
      <div class="ring-note" v-if="note">{{ note }}</div>
    </div>`,
};

// DlStats 是进度条背后那组可对质的数：已下 / 整包 / 速度 / 还要。
//
// 为什么单独做组件：总览页和任务中心都要显示同一条长任务，
// 措辞在两处各写一遍迟早会分叉（"总量"和"整包"会同时出现）。
// detail 缺字段时一律说"还没报出来"而不是补 0：
// 数字全部来自 steamcmd 实时输出，面板不知道的时候就是不知道。
const DlStats = {
  props: { detail: Object },
  computed: {
    cells() {
      const d = this.detail || {};
      const out = [];
      // 阶段放在最前面：这一排数替掉的正是 message 那句长话，而那句开头说的就是"现在在做什么"。
      if (d.phase) out.push({ k: "阶段", v: d.phase });
      // 校验已下载文件时字节量也在涨，但那不是下载：界面跟着阶段换词，
      // 否则一次 validate 会顶着 200 MB/s 报成"下载速度"。
      const doneWord = d.downloading ? "已下" : "已处理";
      const pair = bytesPair(d.doneBytes, d.totalBytes);
      out.push({ k: doneWord, v: d.doneBytes > 0 ? pair[0] : "还没开始动" });
      out.push({ k: "整包", v: d.totalBytes > 0 ? pair[1] : "还没报出来" });
      out.push({ k: d.downloading ? "速度" : "吞吐", v: d.bytesPerSec > 0 ? bytes(d.bytesPerSec) + "/s" : "还在测" });
      // 剩余时间没有速度就没有依据；速度有了但整包不知道，同样算不出，
      // 这两种情况都说"算不出"，不写"大约几分"那种凭空的话。
      out.push({ k: "还要", v: d.etaSeconds > 0 ? "约 " + seconds(d.etaSeconds) : "算不出" });
      return out;
    },
  },
  template: `
    <div class="dlstats">
      <span class="ds" v-for="c in cells" :key="c.k">
        <i>{{ c.k }}</i><b :class="c.k === '阶段' ? '' : 'num'">{{ c.v }}</b>
      </span>
    </div>`,
};

// 内联 SVG 折线，不引图表库。
//
// preserveAspectRatio=none 会让竖直线段被横向拉伸，所以线条加了
// vector-effect="non-scaling-stroke"；图上的文字一律放在 SVG 外面，
// 否则中文会被拉成扁的。
const Spark = {
  props: {
    points: { type: Array, default: () => [] },   // [{at, online}]，按时间升序
    height: { type: Number, default: 110 },
  },
  setup(props) {
    const W = 300, H = 100, PAD = 6;
    // 纵轴从 0 起、按整人数取刻度。原来这条线是按这一窗的最低~最高人数铺满整个高度的，
    // 那样拉高只是把同一条线撑开，中间全是空的；有了 0 起点和人数刻度，多出来的高度
    // 就是"这段时间里这些人落在哪一档"，图拉到多高都还读得出东西。
    const axisOf = (hi) => {
      const step = hi > 20 ? 10 : (hi > 10 ? 5 : (hi > 5 ? 2 : 1));
      const max = (Math.floor(hi / step) + 1) * step;   // 顶上一格留给线，不让它贴住卡边
      const ticks = [];
      for (let t = 0; t <= max; t += step) ticks.push(t);
      return { max, ticks };
    };
    const calc = computed(() => {
      const pts = (props.points || []).filter((p) => Number.isFinite(Number(p.online)));
      if (pts.length < 2) return null;
      const ys = pts.map((p) => Number(p.online));
      const hi = Math.max(...ys), lo = Math.min(...ys);
      const axis = axisOf(hi);
      const yOf = (v) => H - PAD - (v / axis.max) * (H - PAD * 2);
      const step = (W - PAD * 2) / (pts.length - 1);
      const xy = pts.map((p, i) => [PAD + i * step, yOf(Number(p.online))]);
      const segs = xy.map(([x, y]) => x.toFixed(1) + "," + y.toFixed(1)).join(" L ");
      // 填充从线落到 0 那一格：轴有了 0 起点之后这块色就是"这段时间坐了多少人"的面积，
      // 全程一个人数时也照样填——那时候它说的正是"这么多人数一直没变过"。
      const area = "M " + xy[0][0].toFixed(1) + "," + yOf(0).toFixed(1) + " L " + segs +
        " L " + xy[xy.length - 1][0].toFixed(1) + "," + yOf(0).toFixed(1) + " Z";
      const grid = axis.ticks.map((t) => ({ v: t, y: yOf(t), pct: ((H - yOf(t)) / H) * 100 }));
      // hiAt 是"最高那一次发生在什么时候"（同一个人数的几个点里取最早那个）。
      // 光写"峰值 3"读者没法判断那是刚刚还是昨夜：峰值时刻和"当前"这一头是两件事。
      const hiAt = pts[ys.indexOf(hi)].at;
      return { line: "M " + segs, area, grid, hi, hiAt, lo, last: Number(pts[pts.length - 1].online), lastAt: pts[pts.length - 1].at, firstAt: pts[0].at };
    });
    const flat = computed(() => {
      const pts = (props.points || []).filter((p) => Number.isFinite(Number(p.online)));
      if (pts.length < 2) return null;
      const ys = pts.map((p) => Number(p.online));
      const hi = Math.max(...ys), lo = Math.min(...ys);
      // 人数一动没动时图照画：平线 + 面积本身就说明"这段时间一直是 N 人"，
      // 0 起点纵轴还给了刻度可对照。这里只算一句图下的短说明，不再把整张图
      // 换成一段文字（2026-11-09 用户："在线人数没有变化……图怎么没有了？"）。
      if (hi !== lo) return null;
      return { value: hi, from: pts[0].at, to: pts[pts.length - 1].at, count: pts.length };
    });
    return { W, H, calc, flat, rel };
  },
  template: `
    <div>
      <div class="spark-field" v-if="calc">
        <svg class="spark" :viewBox="'0 0 ' + W + ' ' + H" preserveAspectRatio="none" aria-hidden="true">
          <line v-for="g in calc.grid" :key="'g' + g.v" class="grid" x1="0" x2="300"
                :y1="g.y" :y2="g.y" vector-effect="non-scaling-stroke"></line>
          <path class="area" :d="calc.area"></path>
          <path class="line" :d="calc.line" vector-effect="non-scaling-stroke"></path>
        </svg>
        <!-- 刻度写在 SVG 外面：viewBox 被横向拉得过，图内的中文和数字会跟着扁掉。 -->
        <span v-for="g in calc.grid" :key="'t' + g.v" class="spark-axis"
              :style="{bottom: g.pct.toFixed(2) + '%'}">{{ g.v }}</span>
      </div>
      <!-- 采够两个点之前不画那条横线：一块 118px 的空白加一条线，看着像图坏了，
           而事实只是"面板刚装好，还没攒够采样"。 -->
      <!-- 全程同一个人数时图不撤，只在图下补一句结论：这条平线不是坏图，
           是这一窗里确实没人进出。 -->
      <div v-if="flat" class="spark-flat-note">
        这 {{ flat.count }} 次采样一直是 <b>{{ flat.value }}</b> 人，没有变化 —— 图上是一条平线（{{ rel(flat.from) }} — {{ rel(flat.to) }}）。
      </div>
      <div v-if="!calc" class="spark-empty">数据点不足：要等面板攒够两个采样点</div>
      <div class="row spread" style="font-size:var(--t-xs);color:var(--text-3)" v-if="calc">
        <span>{{ rel(calc.firstAt) }} 起</span>
        <span>峰值 {{ calc.hi }}（{{ rel(calc.hiAt) }}）· 当前 {{ calc.last }}</span>
      </div>
    </div>`,
};

// Kv 是"标签: 值"的紧凑列表。用 dl/dt/dd 而不是 table：
// 它没有列对齐的语义需求，而 table 在窄屏上会逼出横向滚动。
const Kv = {
  props: { rows: { type: Array, default: () => [] } },   // [{k, v, mono}]
  template: `
    <dl class="kv">
      <template v-for="(r, i) in rows" :key="i">
        <dt>{{ r.k }}</dt>
        <dd :class="r.mono ? 'mono' : ''">{{ r.v }}</dd>
      </template>
    </dl>`,
};

const Toasts = {
  setup() { return { state, dropToast }; },
  template: `
    <div class="toasts" v-if="state.toasts.length" role="region" aria-label="操作提示" aria-live="polite">
      <div v-for="t in state.toasts" :key="t.id" class="toast" :class="t.kind"
           :role="t.kind === 'bad' ? 'alert' : 'status'">
        <div class="row spread">
          <div class="t-title">{{ t.title }}</div>
          <button class="linkish" @click="dropToast(t.id)" aria-label="关掉这条提示"><x-icon name="close" :size="15"></x-icon></button>
        </div>
        <div class="t-body" v-if="t.body">{{ t.body }}</div>
      </div>
    </div>`,
};

// 确认框的无障碍：读屏要能念出标题，键盘要能 Escape 退出、Tab 不跑出对话框，
// 关闭后焦点回到原来那个按钮上。破坏性操作的确认框尤其不能只靠鼠标。
const Confirm = {
  setup() { return { state, answerConfirm }; },
  data() { return { lastFocus: null, unwatch: null }; },
  methods: {
    onKey(e) {
      if (!state.confirm) return;
      if (e.key === "Escape") { e.preventDefault(); answerConfirm(false); return; }
      if (e.key !== "Tab") return;
      const d = document.querySelector(".mask .dialog");
      if (!d) return;
      const list = [...d.querySelectorAll("button:not([disabled]), a[href], input, select, textarea")]
        .filter((x) => x.offsetParent !== null);
      if (!list.length) return;
      const first = list[0], last = list[list.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    },
  },
  mounted() {
    document.addEventListener("keydown", this.onKey);
    this.unwatch = this.$watch(() => state.confirm, (v) => {
      if (v) {
        this.lastFocus = document.activeElement;
        this.$nextTick(() => {
          const d = document.querySelector(".mask .dialog");
          const btns = d ? [...d.querySelectorAll("button")] : [];
          // 默认落在"取消"上：误按回车不该直接执行危险动作。
          const safe = btns.find((b) => /取消|返回|再想想/.test(b.textContent));
          const target = safe || btns[0];
          if (target && target.focus) target.focus();
        });
      } else if (this.lastFocus && this.lastFocus.focus) {
        this.lastFocus.focus();
        this.lastFocus = null;
      }
    });
  },
  unmounted() {
    document.removeEventListener("keydown", this.onKey);
    if (this.unwatch) this.unwatch();
  },
  template: `
    <div class="mask" v-if="state.confirm" @click.self="answerConfirm(false)">
      <div class="dialog" :class="state.confirm.danger ? 'danger' : ''" role="dialog" aria-modal="true"
           aria-labelledby="confirm-title" aria-describedby="confirm-body">
        <h3 id="confirm-title">{{ state.confirm.title }}</h3>
        <p id="confirm-body" class="muted">{{ state.confirm.body }}</p>
        <x-note v-if="state.confirm.warn" kind="warn">{{ state.confirm.warn }}</x-note>
        <ul v-if="state.confirm.details && state.confirm.details.length" class="list-plain">
          <li v-for="(d, i) in state.confirm.details" :key="i" class="muted">{{ d }}</li>
        </ul>
        <div class="dialog-foot">
          <button class="btn ghost" @click="answerConfirm(false)">{{ state.confirm.cancelLabel || "取消" }}</button>
          <button class="btn" :class="state.confirm.danger ? 'danger' : 'primary'"
                  @click="answerConfirm(true)">{{ state.confirm.okLabel || "确定" }}</button>
        </div>
      </div>
    </div>`,
};

// 通知中心：Toast 会消失，历史不会。失败信息、批量结果、SSE 断线都在这里留一份。
const NoticeCenter = {
  setup() { return { state, toggleNoticeCenter, clearNotices, rel }; },
  template: `
    <div class="notice-center" v-if="state.noticeOpen" role="region" aria-label="通知中心">
      <div class="nc-head">
        <b>最近的通知</b>
        <button class="linkish" @click="clearNotices">清空</button>
        <button class="linkish" @click="toggleNoticeCenter(false)" aria-label="收起通知中心">
          <x-icon name="close" :size="15"></x-icon></button>
      </div>
      <div class="nc-list" aria-live="polite">
        <div v-for="n in state.notices" :key="n.id" class="nc-item" :class="n.kind">
          <div class="row spread"><div class="nc-t">{{ n.title }}</div><div class="nc-time">{{ rel(n.at) }}</div></div>
          <div v-if="n.body" class="muted">{{ n.body }}</div>
        </div>
        <div class="nc-empty" v-if="!state.notices.length">还没有通知</div>
      </div>
    </div>`,
};

// 短时撤销条：只在 state.undo 还在时出现，12 秒后由 store 自己收掉。
const UndoBar = {
  setup() { return { state, runUndo, dismissUndo }; },
  template: `
    <div class="undo-bar" v-if="state.undo" role="status">
      <span>{{ state.undo.label }}</span>
      <button class="btn sm" :disabled="state.undo.busy" @click="runUndo">
        {{ state.undo.busy ? "正在撤销…" : "撤销" }}</button>
      <button class="linkish" @click="dismissUndo" aria-label="不撤销，收起这条">
        <x-icon name="close" :size="14"></x-icon></button>
    </div>`,
};

// 实时流断线：给一块明确的状态牌和手动重连入口，不再只留一句"按刷新看"。
const ReconnectBanner = {
  props: { title: { type: String, default: "实时日志" } },
  emits: ["reconnect"],
  setup() { return { state }; },
  computed: {
    // 重连尝试中的 connecting 也算断线：否则每次退避结束都会把提示牌闪掉再亮回来。
    broken() {
      const s = state.stream;
      return ["error", "reconnecting", "closed"].indexOf(s.status) >= 0 ||
        (s.status === "connecting" && s.attempt > 0);
    },
  },
  template: `
    <div class="reconnect-banner" v-if="broken" role="status">
      <x-icon name="warn" :size="15"></x-icon>
      <span>{{ title }}已断开{{ state.stream.attempt ? "（正在第 " + state.stream.attempt + " 次重连）" : "" }}：
        {{ state.stream.error || "连接中断，断线期间的新日志会在重连后补上" }}</span>
      <button class="btn sm" @click="$emit('reconnect')">立即重连</button>
    </div>`,
};

// 字段级错误：后端说"端口被占"时挂在端口输入框下面，而不是整张表顶一个 err-box。
const FieldError = {
  props: { text: String },
  template: `<div class="field-error" v-if="text" role="alert">{{ text }}</div>`,
};

// 只读账号看见的是"灰掉的按钮 + 为什么"，不是消失的按钮：
// 消失会让人以为这个功能根本不存在，或者不知道该找谁要权限。
const PermBtn = {
  props: {
    allowed: { type: Boolean, default: true },
    reason: { type: String, default: "这个账号没有修改权限" },
    busy: Boolean,
    tone: { type: String, default: "" },
  },
  emits: ["go"],
  template: `
    <button class="btn" :class="tone" :disabled="!allowed || busy"
            :title="allowed ? '' : reason" @click="$emit('go')"><slot></slot></button>`,
};

export const components = {
  "x-icon": Icon,
  "x-hint": Hint,
  "x-pager": Pager,
  "x-seg": Seg,
  "x-stat": Stat,
  "x-chip": Chip,
  "x-badge": Badge,
  "x-note": Note,
  "x-empty": Empty,
  "x-sk": Sk,
  "x-bar": Bar,
  "x-ring": Ring,
  "x-dlstats": DlStats,
  "x-spark": Spark,
  "x-kv": Kv,
  // 模组封面格：检索页、下载页、配置页共用同一个（见 js/modcard.js）。
  "x-mod-cover": ModCover,
  "x-toasts": Toasts,
  "x-confirm": Confirm,
  "x-notice-center": NoticeCenter,
  "x-undo-bar": UndoBar,
  "x-reconnect-banner": ReconnectBanner,
  "x-field-error": FieldError,
  "x-perm-btn": PermBtn,
};
