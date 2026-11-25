const PlayerTimeline = {
  props: { data: { type: Object, default: null } },
  setup() { return { clock, dateText, seconds, rel }; },
  data() { return { trackW: 0 }; },
  mounted() {
    const el = this.$el && this.$el.querySelector(".tl-axis-track");
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
      const crossDay = w.span > 48 * 3600 * 1000;
      // 跨天那一窗只写日期是够用的：一天一个点，读者要认的是"哪天"，不是"几点"。
      const withClock = !crossDay || this.trackW >= 4 * 74;
      const labelPx = crossDay ? (withClock ? 74 : 44) : 40;
      const n = Math.max(2, Math.min(5, Math.floor(this.trackW / labelPx) + 1));
      const out = [];
      for (let i = 0; i < n; i++) {
        const ms = w.a + (w.span * i) / (n - 1);
        const day = dateText(ms).slice(5);
        out.push({
          p: (100 * i) / (n - 1),
          // 近 3 天 / 近 7 天只写钟点会重样：三个 00:00 分不清是哪天凌晨。
          text: crossDay ? (withClock ? day + " " + clock(ms) : day) : clock(ms),
        });
      }
      return out;
    },
    // 图例只列这一窗里真出现过的形状，文字也一律取自接口：
    // 死亡/复活/幽灵消散的短名是后端那份行为目录给的（和事件页同一个来源），
    // 前端另写一套的话，两边迟早会对不上。
    markKinds() {
      const seen = {}, out = [];
      this.people.forEach((p) => (p.marks || []).forEach((m) => {
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
    spanTitle(sp) {
      return clock(sp.from, true) + " → " + clock(sp.to, true) +
        "（" + seconds((Date.parse(sp.to) - Date.parse(sp.from)) / 1000) + "）" +
        (sp.clippedStart ? " · 这一段没有起点记录，从窗口边界画起" : "") +
        " · " + sp.reasonLabel;
    },
    whoTitle(p) {
      return (p.userid || "没有账户编号：引擎的进出公告只带名字，他改了名会被当成两个人") +
        " · 这个窗口里一共在线 " + seconds(p.seconds) +
        " · " + (p.onlineNow ? "现在还在里面" : "最后一次活动 " + rel(p.lastAt));
    },
    // 码值只用来挑形状与颜色，上屏的是接口那句中文 label。
    tone(ev) { return ev === "player_resurrect" ? "revive" : ev === "ghost_gone" ? "ghost" : "death"; },
  },
  template: `
    <div class="tl">
      <div class="tl-row tl-axis" v-if="win">
        <span class="tl-who"></span>
        <span class="tl-track tl-axis-track">
          <i v-for="t in ticks" :key="t.p" class="tl-tick" :style="{ left: t.p + '%' }"
             :class="t.p === 0 ? 'at-start' : (t.p === 100 ? 'at-end' : 'at-mid')">{{ t.text }}</i>
        </span>
        <span class="tl-sum"></span>
      </div>

      <div class="tl-row" v-for="p in people" :key="p.userid || p.name" :title="whoTitle(p)">
        <span class="tl-who">
          <b class="tl-live" v-if="p.onlineNow"></b>
          <span class="tl-name">{{ p.name }}</span>
        </span>
        <span class="tl-track">
          <i class="tl-span" v-for="(sp, i) in p.spans" :key="'s' + i"
             :class="{ open: sp.reason === 'open', clip: sp.clippedStart }"
             :style="barStyle(sp)" :title="spanTitle(sp)"></i>
          <b class="tl-mark" v-for="(m, i) in p.marks" :key="'m' + i" :class="tone(m.event)"
             :style="dotStyle(m)" :title="m.label + ' · ' + clock(m.at, true)"></b>
        </span>
        <span class="tl-sum" :class="p.onlineNow ? 'live' : ''">{{ seconds(p.seconds) }}</span>
      </div>

      <div class="tl-legend" v-if="spanKinds.length || markKinds.length">
        <span v-for="k in spanKinds" :key="'b' + k.key"><i class="k" :class="k.key"></i>{{ k.label }}</span>
        <span v-for="k in markKinds" :key="'d' + k.event"><i class="d" :class="k.tone"></i>{{ k.label }}</span>
      </div>
    </div>`,
};
