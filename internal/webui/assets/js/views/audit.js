// 操作记录页：谁在什么时候对面板做了什么、成功没有、失败的原话是什么。
//
// 这一页的价值在出错之后：用户点了一个按钮、界面报了一句话，回头要能对上
// "那一下到底提交了什么"。所以每条都留着对象和详情，而不是只留一句"修改了设置"。
//
// 分页是偏移量式的（后端 limit/offset）。动作筛选按前缀匹配：选「分片启停（全部）」
// 会捞出这一族的所有动作，选具体动作就只筛那一条——这一层语义由后端定义，
// 这里只负责把 /audit-actions 给的目录按族摆进下拉。
//
// 时间区间是另一条轴：出事后第一句总是"就今天下午那会儿"，只按动作筛还得自己数页。
// 界面上给的是"今天/近 7 天/近 30 天"这种说法，落到接口是换算过的 UTC 时刻——
// 面板存的是 UTC 毫秒，把"2026-11-02"这个字符串直接交给后端会差一个时区，
// 东八区的"今天"就变成"昨天下午 4 点以后"。
import * as api from "../api.js";
import { rel, state } from "../store.js";
import { dateTime } from "../fmt.js";

const PAGE = 50;

// localDay(n) 取"本地日历上从今天往前数 n 天"那一天的零点，是一个确定的时刻。
function localDay(n) {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  d.setDate(d.getDate() - (n || 0));
  return d;
}

// z 把一个时刻写成接口要的 UTC 文本（秒级精度，去掉浏览器给的 .000）。
function z(d) {
  return d.toISOString().replace(/\.\d{3}Z$/, "Z");
}

// dayInput 把 <input type=date> 的 "2026-11-02" 变成本地零点。
// 不能用 new Date("2026-11-02")：那一条按规范是 UTC 零点，东八区看就是早上 8 点，
// 于是"8 点以前发生的那几条"会莫名其妙少掉。
function dayInput(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(s || ""));
  if (!m) return null;
  return new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), 0, 0, 0, 0);
}

// RANGE 里每一项都给出 since / until 这一对时刻；until 是"不含"，
// 所以"今天"的 until 是明天零点，而不是现在——否则刷新一下页面，
// 刚刚那一条就会因为跨了秒而消失。
const RANGES = [
  { value: "all", label: "不限时间" },
  { value: "today", label: "今天" },
  { value: "7d", label: "近 7 天" },
  { value: "30d", label: "近 30 天" },
  { value: "custom", label: "自定义" },
];

export default {
  name: "Audit",
  setup() { return { rel, dateTime }; },
  data() {
    return {
      cat: null, catError: "",
      action: "",
      range: "all",
      from: "", to: "",
      actor: "", target: "",
      offset: 0,
      rows: [], more: false, loading: false, error: "",
      expanded: {},
      isPhone: false,
      filtersOpen: false,
      // 长表密度：.dense / .comfortable 是 app.css 已落地的钩子，这里只切类名。
      dense: false,
    };
  },
  computed: {
    ranges() { return RANGES; },
    tableDensity() { return this.dense ? "dense" : "comfortable"; },
    // 两个日期都没填不算写错——"只填一头"是常见用法，所以那头按不限算。
    // 这一句只是把规则说出来，不拦请求。
    rangeNote() {
      if (this.range !== "custom" || this.from || this.to) return "";
      return "起止都可以不填：没填的那一头按不限算。结束那天算进去。";
    },
    // 这一条才拦：写反了或写坏了，发出去只会得到一张空表，
    // 而空表在界面上和"那段时间确实没操作过"长得一模一样。
    rangeError() {
      if (this.range !== "custom") return "";
      const a = this.from ? dayInput(this.from) : null;
      const b = this.to ? dayInput(this.to) : null;
      if (this.from && !a) return "开始日期没写对，要像 2026-11-02 那样。";
      if (this.to && !b) return "结束日期没写对，要像 2026-11-02 那样。";
      if (a && b && b < a) return "结束日期在开始日期之前，中间是空的。";
      return "";
    },
    // 区间能成立才发给接口；不成立时下面不发时间参数，也不去覆盖用户已经填的日期。
    timeWindow() {
      if (this.rangeError) return null;
      if (this.range === "all") return { since: "", until: "" };
      if (this.range === "today") return { since: z(localDay(0)), until: z(localDay(-1)) };
      if (this.range === "7d") return { since: z(localDay(6)), until: z(localDay(-1)) };
      if (this.range === "30d") return { since: z(localDay(29)), until: z(localDay(-1)) };
      const a = this.from ? dayInput(this.from) : null;
      // 结束那天算进去：用户写 11-02 的意思是"到 11 月 2 日为止"，不是"到凌晨零点为止"。
      const b = this.to ? new Date(dayInput(this.to).getTime() + 86400000) : null;
      return { since: a ? z(a) : "", until: b ? z(b) : "" };
    },
    // 把"近 7 天"这种说法翻成具体日期：不翻的话，用户过两周回来看到"近 7 天"亮着，
    // 会以为自己看的是当时那一份表。
    windowText() {
      const w = this.timeWindow;
      if (!w || (!w.since && !w.until)) return "";
      const say = (iso, exclusive) => {
        if (!iso) return "不限";
        const d = new Date(iso);
        if (exclusive) d.setDate(d.getDate() - 1); // until 是不含的那头，说给人听要退一天
        return (d.getMonth() + 1) + " 月 " + d.getDate() + " 日";
      };
      return say(w.since) + " 到 " + say(w.until, true);
    },
    groups() {
      const d = this.cat;
      if (!d || !d.families) return [];
      return d.families.map((f) => ({
        code: f.code,
        label: f.label,
        items: (d.actions || []).filter((a) => String(a.code).indexOf(f.code) === 0),
      })).filter((g) => g.items.length);
    },
    page() { return Math.floor(this.offset / PAGE) + 1; },
    hasPrev() { return this.offset > 0; },
    // 有没有下一页，只认接口给的那一条证据：每次多要一条（PAGE+1），
    // 第 51 条到手才算后面还有。原来写的是"这一页满 50 条就有下一页"，
    // 而最后一页正好满 50 条时那是假的——点下去是一片空表，看着像记录被筛没了。
    hasNext() { return this.more; },
    // 空表那句话必须把筛过的每一条轴都报上：只说"这一类操作还没有记录"，
    // 用户会以为真没发生过，而实际是他把时间限定在了今天、或者还挂着一个人名字。
    emptyTitle() {
      const bits = this.activeFilters.map((f) => f.text);
      if (!bits.length) return "还没有任何操作记录";
      return "没有同时满足「" + bits.join("」+「") + "」的记录";
    },
    // 现在生效的筛选，一条一项：既给空表那句话用，也给"清掉"那排按钮用。
    // 每一项都必须是"关掉它就能回到上一级"的独立条件，所以时间轴拆成一句、
    // 动作一句、人一句、对象一句。
    activeFilters() {
      const out = [];
      if (this.action) out.push({ key: "action", text: this.actionLabel || this.action });
      if (this.windowText) out.push({ key: "time", text: this.windowText });
      if (this.actor) out.push({ key: "actor", text: this.actor + " 操作的" });
      if (this.target) out.push({ key: "target", text: "对象含「" + this.target + "」" });
      return out;
    },
    actors() { return (this.cat && this.cat.actors) || []; },
    actionLabel() {
      const hit = (this.cat && (this.cat.actions || []).find((x) => x.code === this.action));
      return hit ? hit.label : "";
    },
    // 审计页是只读的操作记录，没有写操作；按契约仍暴露这两项，组件没有 cluster 故用退化判定。
    canWrite() { return !!(state.me && state.me.isAdmin); },
    writeNote() { return "这个账号是只读：能看状态，但修改会被拒（改自己的密码除外）"; },
  },
  mounted() {
    this.isPhone = !!(window.matchMedia && window.matchMedia("(max-width: 560px)").matches);
    this.loadCatalog();
    this.load();
  },
  methods: {
    async loadCatalog() {
      const res = await api.get("/api/v1/audit-actions");
      if (!res.ok) { this.catError = res.error || ("接口返回 " + res.status); return; }
      this.cat = res.data;
      this.catError = "";
    },
    async load(reset) {
      if (reset) this.offset = 0;
      if (this.rangeError) { this.rows = []; this.more = false; this.error = ""; return; }
      this.loading = true;
      this.error = "";
      const w = this.timeWindow || { since: "", until: "" };
      const res = await api.get("/api/v1/audit" + api.qs({
        limit: PAGE + 1, offset: this.offset || "", action: this.action,
        since: w.since, until: w.until, actor: this.actor, target: this.target,
      }));
      this.loading = false;
      if (!res.ok) { this.error = res.error || ("接口返回 " + res.status); this.rows = []; this.more = false; return; }
      const got = res.data || [];
      this.more = got.length > PAGE;
      this.rows = got.slice(0, PAGE);
    },
    setRange(v) {
      this.range = v;
      // 从自定义切走时留着那两个日期：用户多半是"先看看近 7 天再回来"，
      // 清空等于让他重新想一遍自己填的是哪两天。
      this.load(true);
    },
    prev() { if (!this.hasPrev) return; this.offset = Math.max(0, this.offset - PAGE); this.load(); },
    next() { if (!this.hasNext) return; this.offset += PAGE; this.load(); },
    // 关掉某一条筛选：每一项都得能单独撤掉。只给一个"全部清掉"的话，
    // 用户想连着换两个人名看，每次都得把动作和时间重挑一遍。
    clearFilter(key) {
      if (key === "action") this.action = "";
      else if (key === "time") { this.range = "all"; this.from = ""; this.to = ""; }
      else if (key === "actor") this.actor = "";
      else if (key === "target") this.target = "";
      this.load(true);
    },
    clearAll() {
      this.action = ""; this.range = "all"; this.from = ""; this.to = "";
      this.actor = ""; this.target = "";
      this.load(true);
    },
    toggle(e) { this.expanded[e.id] = !this.expanded[e.id]; },
    long(e) {
      const n = String(e.detail || "").length + String(e.err || "").length;
      return n > (this.isPhone ? 70 : 10000);
    },
  },
  template: `
    <div class="col audit-page" style="gap:14px">
      <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap">
        <span class="card-title-sm">谁提交了什么、成功没有</span>
        <x-hint label="这份记录怎么来的" text="面板自己记的账：写入失败的那条也留着，因为「没改成」同样是一次操作。超过保留天数（在面板设置里改）会自动清理，启动面板时与之后每 6 小时收拾一次。这里看不到游戏里的事，游戏内的动作在日志页。"></x-hint>
        <span class="grow"></span>
        <!-- 这两个按钮包在一起：手机上标题加那枚说明已经占满第一行，
             不包的话「刷新」会被单独挤到第三行，看着像两个不相干的东西。 -->
        <span class="row" style="gap:8px;align-items:center;margin-left:auto">
          <!-- 手机上这四组筛选竖着排要吃掉一整屏，而这一页真正要看的是那张表：
               所以窄屏默认把它收起，只留「现在只看」那排已经生效的条件。
               宽屏上筛选本来就是一行，没有这个问题，所以这个按钮只在窄屏出现。 -->
          <button id="audit-filters-toggle" class="btn ghost sm only-sm" @click="filtersOpen = !filtersOpen">
            {{ activeFilters.length ? (filtersOpen ? '收起筛选 · ' + activeFilters.length + ' 条在用' : '筛选 · ' + activeFilters.length + ' 条在用') : (filtersOpen ? '收起筛选' : '筛选') }}</button>
          <button class="btn ghost sm" @click="load()" :disabled="loading">{{ loading ? '读取中' : '刷新' }}</button>
        </span>
      </div>

      <div class="col audit-filters" :class="filtersOpen ? 'open' : ''" style="gap:10px">
        <div class="row" style="gap:8px">
          <div class="field" style="min-width:220px;flex:1 1 260px">
            <div class="field-head">
              <label for="audit-action">只看某一类操作</label>
              <x-hint text="选项是从面板登记过的动作种类推出来的，按功能分组；选了以后只影响下面这张表，翻页也带着这个条件。"></x-hint>
            </div>
            <select id="audit-action" v-model="action" @change="load(true)">
              <option value="">全部操作</option>
              <optgroup v-for="g in groups" :key="g.code" :label="g.label">
                <option v-for="a in g.items" :key="a.code" :value="a.code">{{ a.label }}</option>
              </optgroup>
            </select>
          </div>
        </div>

        <!-- 时间这一条轴单独一行：它和"哪一类操作"是两件事，挤在同一行里
             下拉框会把分段控件挤到折行，两个筛选条件反而看不清彼此。 -->
        <div class="row audit-when">
          <x-seg label="只看某一段时间" :options="ranges" :model-value="range" @update="setRange"></x-seg>
          <template v-if="range === 'custom'">
            <div class="field when-field">
              <label for="audit-from">从哪天起</label>
              <input id="audit-from" type="date" v-model="from" @change="load(true)">
            </div>
            <span class="when-dash">到</span>
            <div class="field when-field">
              <label for="audit-to">到哪天（含当天）</label>
              <input id="audit-to" type="date" v-model="to" @change="load(true)">
            </div>
          </template>
          <span class="chip mono" v-else-if="windowText">{{ windowText }}</span>
        </div>
        <div class="when-note" v-if="rangeError || rangeNote" :class="rangeError ? 'bad' : ''">
          {{ rangeError || rangeNote }}
        </div>

        <!-- 谁做的、对哪个房间：出事后第二句总是"是不是他""是不是那个房间"。
             这两个和动作、时间是同一张表上四条互相独立的轴，所以各占一个控件、
             各自能单独关掉。 -->
        <div class="row audit-who">
          <div class="field who-field">
            <div class="field-head">
              <label for="audit-actor">谁做的</label>
              <x-hint text="名单是从这份记录里现查的：只有真在面板上操作过的人才会出现在这里，所以选谁都有结果。"></x-hint>
            </div>
            <select id="audit-actor" v-model="actor" @change="load(true)">
              <option value="">全部操作人</option>
              <option v-for="a in actors" :key="a" :value="a">{{ a }}</option>
            </select>
          </div>
          <div class="field who-field">
            <div class="field-head">
              <label for="audit-target">对象里含</label>
              <x-hint text="房间名、世界名、备份编号都行，按子串找（英文大小写不敏感）。写完回车或点到别处生效。"></x-hint>
            </div>
            <input id="audit-target" type="search" v-model="target" placeholder="例如 Cluster_1"
                   @change="load(true)" @keyup.enter="load(true)">
          </div>
        </div>
      </div>

      <!-- 这一排在收起的筛选块外面：手机上把筛选收起来以后，还得看得见"现在被什么挡着"，
           并且点一下就能撤掉，不用先把那四组控件展开。 -->
      <div class="row audit-on" v-if="activeFilters.length">
        <span class="muted">现在只看</span>
        <button class="fchip" v-for="f in activeFilters" :key="f.key" @click="clearFilter(f.key)"
                :aria-label="'清掉这条筛选：' + f.text">
          {{ f.text }}<span class="fx" aria-hidden="true">×</span>
        </button>
        <span class="grow"></span>
        <button class="linkish" @click="clearAll">全部清掉</button>
      </div>

      <div class="err-box" v-if="catError">筛选目录没读到：{{ catError }}</div>
      <x-sk v-if="loading && !rows.length" :lines="4"></x-sk>
      <div class="err-box" v-else-if="error">{{ error }}</div>
      <x-empty v-else-if="!rows.length" icon="clipboard"
        :title="emptyTitle"
        :note="activeFilters.length
          ? '上面那几枚条件各自点 × 就能放开一条；这一栏只翻面板自己的账，游戏里的事在日志页。'
          : '启动世界、改世界规则、装模组、备份存档，都会在这里留一行。'"></x-empty>

      <div class="row" style="gap:8px;align-items:center" v-if="rows.length">
        <span class="grow"></span>
        <button class="btn sm ghost" @click="dense = !dense"
                :title="dense ? '切成宽松行距' : '切成紧凑行距'">{{ dense ? '紧凑' : '宽松' }}</button>
      </div>

      <div class="table-wrap hide-sm" :class="tableDensity" v-if="rows.length">
        <table class="data">
          <thead>
            <tr><th>什么时候</th><th>做了什么</th><th>对象</th><th>是谁</th><th>结果</th><th>详情</th></tr>
          </thead>
          <tbody>
            <tr v-for="e in rows" :key="e.id">
              <td>
                <b>{{ dateTime(e.at) }}</b>
                <div class="hint">{{ rel(e.at) }}</div>
              </td>
              <td class="wrap"><b>{{ e.actionLabel }}</b></td>
              <td class="mono">{{ e.target || '—' }}</td>
              <td>{{ e.actor || '—' }}</td>
              <td>
                <x-badge v-if="e.ok" kind="ok" text="成功"></x-badge>
                <x-badge v-else kind="bad" text="失败"></x-badge>
              </td>
              <td class="wrap">
                <div class="task-msg" v-if="e.detail">{{ e.detail }}</div>
                <div class="task-msg bad" v-if="e.error">{{ e.error }}</div>
                <div class="hint" v-if="!e.detail && !e.error">没有附加说明</div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="col only-sm" style="gap:8px" v-if="rows.length">
        <div class="mod-row" v-for="e in rows" :key="e.id">
          <div class="col mod-main">
            <div class="row" style="gap:8px">
              <b>{{ e.actionLabel }}</b>
              <span class="grow"></span>
              <x-badge v-if="e.ok" kind="ok" text="成功"></x-badge>
              <x-badge v-else kind="bad" text="失败"></x-badge>
            </div>
            <div class="row mod-meta">
              <span class="chip">{{ rel(e.at) }}</span>
              <span class="chip mono" v-if="e.target">{{ e.target }}</span>
              <span class="chip" v-if="e.actor">{{ e.actor }}</span>
            </div>
            <div class="task-msg" v-if="e.detail" :class="expanded[e.id] ? 'open' : ''">{{ e.detail }}</div>
            <div class="task-msg bad" v-if="e.error" :class="expanded[e.id] ? 'open' : ''">{{ e.error }}</div>
            <div class="hint" v-if="!e.detail && !e.error">没有附加说明</div>
            <div class="hint">{{ dateTime(e.at) }}</div>
            <div class="row" style="gap:6px">
              <button class="linkish" v-if="long(e)" @click="toggle(e)">{{ expanded[e.id] ? '收起这一条' : '展开这一条' }}</button>
            </div>
          </div>
        </div>
      </div>

      <!-- 一条都没有时这一排不说"第 1 页 · 这一页 0 条"：空状态已经解释了为什么是空的，
           再摆一个分页器像是"还有下一页没翻出来"。 -->
      <div class="pager" v-if="rows.length">
        <span class="muted">第 {{ page }} 页 · 这一页 {{ rows.length }} 条</span>
        <button class="btn sm ghost" :disabled="!hasPrev || loading" @click="prev">上一页</button>
        <button class="btn sm ghost" :disabled="!hasNext || loading" @click="next">下一页</button>
      </div>
    </div>`,
};
