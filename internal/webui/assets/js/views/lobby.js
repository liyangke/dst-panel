// 公网房间：别人开的饥荒服务器，Klei 公开快照里那一整份列表。
//
// 这一页和总览下那一排本机房间长得像（都是"房间 + 玩法 + 人数"），但两件事必须一眼分得开：
// 总览上那些是这台机器上的房间，面板能启停、能改配置；这一页是别人的房间，
// 面板动不了它任何一份文件，只有一个动作：把连接命令复制走。所以这里一个「启动」按钮都没有。
//
// 这里曾经有过第二个动作「照这间开服」，2026-11-04 拆掉了（见 docs/学习指南.md 第 5 节
// 第 17 条）。原因是那份快照公开的就只有玩法、PVP、人数上限三栏，而这三栏向导自己
// 一屏就能点到；世界大小、资源、Boss、季节长度这些真正想抄的东西根本不在列表里。
// 起点那一排也一起拆了：同日在向导第 2 步补的「常见配置」是同一份众数换成三个按钮，
// 还是一样的没料可给——而且第一枚和向导默认逐字段相同，按下去什么都不变。
//
// 和总览的另一处区别是筛选发生在哪：本机那几间房全在本地，页签一点就切过去；
// 这一页一个大区上万间（2026-11-04 实测亚太 10069 间，同一份快照当天两次抽样在 1.03 万上下浮动），
// 全搬到前端只为筛一个词是不讲道理的（也真会拖慢刷新）。
// 所以这里的筛选、排序、分页全部发给接口，前端只保留"当前条件"这一小份状态。
//
// 三条不能省的诚实：
//   - 列表是 CDN 快照，本身有延迟，房主随时可能已经关了房。界面每次都说"这是几点取到的"，
//     并写明"能不能进要以游戏里真连一次为准"。
//   - 出不去公网时接口给 503 加一句中文原因，这一页显示错误而不是空表格。空表格会被读成
//     "这个大区一间房都没有"，跟真相（这台机器出不去）的下一步操作完全相反。
//   - 「取最新」是真去打一次 Klei（refresh=1），不是把同一份缓存再切一页。十几 MB 的下载
//     只绑在真人的一次点击上，自动轮询一律不带这个参数。
import * as api from "../api.js";
import { state } from "../store.js";
import { copyText } from "../clip.js";
import { dateTime } from "../fmt.js";
import { known, plain } from "../text.js";

// 30 秒：列表的最长新鲜度是配置里的 refreshSeconds（默认 60 秒），
// 比它短就总有一轮只是命中缓存，白头发一次请求。
const AUTO_REFRESH = 30 * 1000;

// 一行的标签最多先摆这么多个。实测 504 条抽样（.tools/lobby_shape.py，2026-11-03）：
// 标签数中位 21 个、最多 101 个，单个标签最长 328 字——全摆出来的话一行能撑到半屏高，
// 这一列就没法扫了。所以只摆前面几个，剩下的点一下展开。
const TAG_SHOWN = 6;

// 季节那一格写的是"现在跑到第几季"，很容易被读成"这是一张冬天开局的世界"。
// 世界生成的配置公网列表里根本没有（2026-11-04 对亚太那一份逐字段数过），所以这句只能挂
// 在这一格上：等用户把它当成开局设定抄进自己的房间，再解释就晚了。
const SEASON_TITLE = "这里写的季节是那一间现在跑到第几季，不是它开局设定的季节；"
  + "世界大小、资源多寡、Boss、季节长度这些公网列表都不公开。";

const SIZES = [20, 50, 100];

const ACCESS = [
  { value: "", label: "不限门槛" },
  { value: "open", label: "门槛全开" },
  { value: "locked", label: "有门槛" },
];

const SLOTS = [
  { value: "", label: "不限空位" },
  { value: "free", label: "还有空位" },
  { value: "full", label: "已经满了" },
];

const PLAYERS = [
  { value: "", label: "不限人数" },
  { value: "has", label: "有人在里面" },
  { value: "none", label: "一个人都没有" },
];

export default {
  name: "Lobby",
  props: { query: Object },
  setup() { return { dateTime, SEASON_TITLE }; },
  data() {
    return {
      regions: [], defaultRegion: "", regionsLoaded: false, regionsError: "",
      reply: null, loading: false, loaded: false, error: "",
      region: "", q: "", intent: "", access: "", slots: "", players: "", sort: "",
      page: 1, size: 20,
      // 复制结果按行记（地址:端口 就是这一行的身份）：一次只有一枚按钮变成"已复制"，
      // 用户才看得清刚才复制的是哪一间。
      copied: {}, copyFailed: false, failedCmd: "",
      // 展开标签的行（键同上）。默认收起：一屏二十行、每行摊出二十个标签，
      // 这一页就没法看了，而"这间房填了哪些标签"不是挑房间时的主要依据。
      openTags: {},
      refreshing: false,
      timer: null,
    };
  },
  computed: {
    rows() { return (this.reply && this.reply.rooms) || []; },
    // 可以直接进的那几间。判定（有空位 + 收新人 + 没暂停）由后端算成 joinable，
    // 前端不再重判一遍——判两遍迟早给出两个答案。
    joinable() { return this.rows.filter((r) => r.joinable).length; },
    accessOptions() { return ACCESS; },
    slotOptions() { return SLOTS; },
    playerOptions() { return PLAYERS; },
    sizes() { return SIZES; },
    regionOptions() { return this.regions.map((r) => ({ value: r.id, label: r.label })); },
    // 玩法选项来自这一份列表本身（接口回的 intents），不是面板字典。
    // 下拉里摆一个这份列表一间都没有的玩法，点下去必然是一张空表。
    intentOptions() {
      return [{ value: "", label: "全部玩法" }]
        .concat(((this.reply && this.reply.intents) || []).map((i) => ({ value: i.value, label: i.label })));
    },
    sortOptions() { return (this.reply && this.reply.sorts) || []; },
    activeFilters() {
      const out = [];
      const text = this.q.trim();
      if (text) out.push({ key: "q", text: "名字或地址含「" + text + "」" });
      if (this.intent) out.push({ key: "intent", text: labelOf(this.intentOptions, this.intent) + "玩法" });
      if (this.access) out.push({ key: "access", text: labelOf(ACCESS, this.access) });
      if (this.slots) out.push({ key: "slots", text: labelOf(SLOTS, this.slots) });
      if (this.players) out.push({ key: "players", text: labelOf(PLAYERS, this.players) });
      return out;
    },
    // 空表那句话要把生效条件全报上：只说"没有符合的房间"，用户分不清是这个大区真没有这种房，
    // 还是他把三个条件叠在了一起。
    emptyTitle() {
      if (this.activeFilters.length) return "没有同时满足「" + this.activeFilters.map((f) => f.text).join("」+「") + "」的房间";
      return "这个大区现在没有可用的房间";
    },
    emptyNote() {
      if (this.activeFilters.length) {
        return "上面那几枚条件各自点 × 就能放开一条。这一页只有别人的房间，你自己开的那几间在「总览」下面。";
      }
      return "读的是 Klei 的公开快照，一个可用房间都没有是少见的：换个大区，或者点上面的「取最新」重新拉一份。";
    },
    // "第 1-20 间 / 共 16713 间"。用的是接口回的 page 和这一屏真实行数：
    // 页码超界时后端会钳到最后一页，跟着它报才和下面这二十行对得上。
    countText() {
      const r = this.reply;
      if (!r || !r.total || !this.rows.length) return "";
      const start = (r.page - 1) * r.size + 1;
      return "第 " + start + "-" + (start + this.rows.length - 1) + " 间 / 共 " + r.total + " 间";
    },
    // 这一屏里被筛掉的量：上万间筛到二十一间的时侯，"共 21 间"很容易被读成
    // "这个大区只有 21 间"。所以把分母也说出来。
    filteredNote() {
      const r = this.reply;
      if (!r || !r.usableRooms) return "";
      if (r.total === r.usableRooms) return "";
      return "这个大区一共 " + r.usableRooms + " 间，当前条件留下 " + r.total + " 间";
    },
    // 这一页是别人房间的只读快照，没有写操作；按契约仍暴露这两项，组件没有 cluster 故用退化判定。
    canWrite() { return !!(state.me && state.me.isAdmin); },
    writeNote() { return "这个账号是只读：能看状态，但修改会被拒（改自己的密码除外）"; },
  },
  mounted() {
    this.boot();
    this.timer = setInterval(() => {
      // 切走的标签页不打这一轮：这一页每次重取最坏要下载整个大区的快照。
      if (!document.hidden && !this.loading) this.load({ quiet: true });
    }, AUTO_REFRESH);
  },
  unmounted() {
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
  },
  methods: {
    async boot() {
      await this.loadRegions();
      const want = String((this.query && this.query.region) || "");
      if (want && this.regions.some((r) => r.id === want)) this.region = want;
      else this.region = this.defaultRegion || (this.regions[0] && this.regions[0].id) || "";
      // 大区列表都没拿到时也发一次：接口会自己回默认大区，那一屏照样有内容，
      // 而不是因为一个下拉读失败就整页空白。
      this.load();
    },
    async loadRegions() {
      const res = await api.get("/api/v1/lobby/regions");
      if (!res.ok) {
        this.regionsError = res.error || ("接口返回 " + res.status);
        return;
      }
      this.regions = res.data.regions || [];
      this.defaultRegion = res.data.defaultRegion || "";
      this.regionsLoaded = true;
      this.regionsError = "";
    },
    async load(opt) {
      const o = opt || {};
      if (o.quiet && this.loading) return;
      if (!o.quiet) this.loading = true;
      if (o.force) this.refreshing = true;
      this.error = "";
      const res = await api.get("/api/v1/lobby/rooms" + api.qs({
        region: this.region, q: this.q.trim(), intent: this.intent,
        access: this.access, slots: this.slots, players: this.players,
        sort: this.sort, page: this.page, size: this.size,
        refresh: o.force ? "1" : "",
      }));
      this.loading = false;
      if (o.force) this.refreshing = false;
      if (!res.ok) {
        // 安静轮询失败时不清空已有那一屏：一次抖动不该把用户正在挑的房间抹掉，
        // 但要在界面上留下"这是上一次的结果"，否则会拿旧列表当现况判断。
        if (!o.quiet) this.reply = null;
        this.error = res.error || ("接口返回 " + res.status);
        return;
      }
      this.reply = res.data;
      this.loaded = true;
      const q = res.data.query || {};
      // 条件以接口回的那一份为准（不认的值它已经拒掉了）。这里只在"值其实被改掉"时同步，
      // 否则用户正在下拉框里挑下一个的时候，选项会被一次晚到的响应拨回去。
      if ((q.intent || "") !== this.intent) this.intent = q.intent || "";
      // 排序也跟上接口回的那一份。这一条不只是"状态同步"：这三枚排序没有"不限"可选项，
      // 前端一开始留空的话，<select> 找不到 value 对得上的 <option>，浏览器只能画一个空框，
      // 用户看不出这一屏是按什么排的——后端现在没带 sort 就明确回 players，这里照它填上。
      if ((q.sort || "") !== this.sort) this.sort = q.sort || "";
      // 页码超界时后端钳到最后一页并把结果回过来，必须跟着它改，不然分页条会亮着
      // 一个当前并不存在的页，"下一页"点下去永远回同一批。
      if (res.data.page && res.data.page !== this.page) this.page = res.data.page;
    },
    // 改筛选条件：一次调用只做一件事——回到第 1 页并发一次请求。
    // 不用 watch 分别盯着五个条件，那样"清掉全部"会连着发六次请求。
    apply() {
      this.page = 1;
      this.load();
    },
    setRegion(v) {
      if (v === this.region) return;
      this.region = v;
      // 换大区必须把玩法清掉：那张选项表是从上一个大区的列表里推出来的，
      // 带着旧玩法去筛新区，多半只剩一张空表，而条件藏在下拉里看不出来。
      this.intent = "";
      this.apply();
    },
    go(p) {
      this.page = p;
      this.load();
    },
    // 每间房的连接命令。地址和端口都直接来自这一行，面板不猜、不补、不改写。
    cmd(r) { return 'c_connect("' + r.addr + '", ' + r.port + ")"; },
    rowKey(r) { return r.addr + ":" + r.port; },
    async copy(r) {
      const ok = await copyText(this.cmd(r));
      const keys = {};
      if (ok) keys[this.rowKey(r)] = true;
      this.copied = keys;
      this.copyFailed = !ok;
      // 复制失败时把这一行的命令原样摊在界面上。只挂在按钮的悬停标题里不算：
      // 悬停读不到也没法选中，而"这个浏览器不让写剪贴板，请把上面那句选中"
      // 里的"上面那句"必须是屏幕上真存在的一行字，否则这句话把人指到一个不存在的东西上。
      this.failedCmd = ok ? "" : this.cmd(r);
    },
    clear(key) {
      if (key === "q") this.q = "";
      else if (key === "intent") this.intent = "";
      else if (key === "access") this.access = "";
      else if (key === "slots") this.slots = "";
      else if (key === "players") this.players = "";
      this.apply();
    },
    clearAll() {
      this.q = ""; this.intent = ""; this.access = ""; this.slots = ""; this.players = "";
      this.apply();
    },
    // 名字与季节都走 text.js 那两条规则（裁掉画不出来的字符 / 只写认得的词）。
    // 实测依据写在 text.js 与 .tools/lobby_shape.py：504 条抽样里见过以 \x01 开头的名字、
    // 夹私有区表情的名字，还有三条把 "fuck"、"tranquil"、"wet" 填进季节字段。
    // 那一格的位置是留给"现在什么季"的，把噪声原样搬上来等于给了一个假答案。
    roomName(r) { return plain(r.name, r.addr + " 那一间"); },
    season(r) { return known(r.season, r.seasonLabel); },
    shownTags(r) {
      // 标签同名字一类（也是房主自己填的），先裁再摆。清完变空串的那种直接丢掉：
      // 一枚没有字的圆片在界面上像个渲染故障，而不是"这条标签本来就是空的"。
      const t = (r.tags || []).map((x) => plain(x)).filter(Boolean);
      return this.openTags[this.rowKey(r)] ? t : t.slice(0, TAG_SHOWN);
    },
    hiddenTags(r) {
      // 数的必须是裁完之后剩下的条数，否则"还有 3 个"点开展开成 2 个，
      // 界面自己跟自己差了 1。
      const t = (r.tags || []).map((x) => plain(x)).filter(Boolean);
      return t.length > TAG_SHOWN && !this.openTags[this.rowKey(r)] ? t.length - TAG_SHOWN : 0;
    },
    toggleTags(r) {
      const k = this.rowKey(r);
      const on = {};
      Object.keys(this.openTags).forEach((x) => { on[x] = true; });
      on[k] = !this.openTags[k];
      this.openTags = on;
    },
    // 行末那枚结论。三个"点进去也进不去"的原因各说一句，不叠成一句"有问题"：
    // 满员、不收新人、被暂停，在"要不要私聊房主"上是三件不同的事。
    mark(r) {
      if (r.paused) return { tone: "warn", text: "房主按了暂停" };
      if (r.free <= 0) return { tone: "warn", text: "已经满员" };
      if (!r.allowNew) return { tone: "warn", text: "不再收新人" };
      if (!r.fullAccess) return { tone: "info", text: "有门槛" };
      return { tone: "ok", text: "可以直接进" };
    },
  },
  template: `
    <div class="col lobby-page" style="gap:14px">
      <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap">
        <span class="card-title-sm">别人开的房间</span>
        <x-hint label="这一屏的数据是哪来的"
               :text="(reply && reply.note) || '读的是 Klei 放在 CDN 上的公开房间快照，和游戏里「加入房间」那个列表同源；不要账号、不填任何凭据。'"></x-hint>
        <span class="grow"></span>
        <x-seg :options="regionOptions" :model-value="region" label="切换大区" @update="setRegion"
               v-if="regionOptions.length > 1"></x-seg>
        <button class="btn ghost sm" :disabled="loading" @click="load({ force: true })"
                title="重新去 Klei 取一份最新列表（要把整个大区的快照下载一遍，会慢几秒）">
          <x-icon name="refresh" :size="14"></x-icon>
          <span>{{ refreshing ? '正在取' : '取最新' }}</span></button>
      </div>

      <div class="err-box" v-if="regionsError">大区列表没读到：{{ regionsError }}</div>

      <div class="row chip-strip only-sm" style="gap:6px" v-if="reply">
        <span class="chip">{{ reply.regionLabel }} {{ reply.usableRooms }} 间</span>
        <span class="chip">有人 {{ reply.withPlayers }}</span>
        <span class="chip" v-if="reply.total">筛出 {{ reply.total }}</span>
        <span class="chip" :class="reply.stale ? 'warn' : ''">{{ reply.stale ? '这是旧列表' : reply.fetchedAgo + '前取到' }}</span>
      </div>

      <div class="grid grid-4 hide-sm" v-if="reply">
        <x-stat :label="reply.regionLabel + '这一份'" icon="globe" tint="tint-cyan" :value="reply.usableRooms + ' 间'"
                :hint="'面板从 Klei 那份原始记录里认下 ' + reply.usableRooms + ' 间（原始 ' + reply.sourceRecords + ' 条，解压后 ' + reply.decodedSize + '）。这份快照是 ' + dateTime(reply.fetchedAt) + ' 取到的。'">
          <span>{{ reply.fetchedAgo }}前取到 · 原始 {{ reply.sourceRecords }} 条</span>
        </x-stat>
        <x-stat label="里面有人" icon="users" tint="tint-green" :value="reply.withPlayers + ' 间'"
                hint="数的是「这一刻有玩家在里面」的房间数，不是在线总人数。列表里多数房间是空的，这很正常：房主把服开着等人。"></x-stat>
        <x-stat label="当前条件筛出" icon="filter" tint="tint-azure" :value="reply.total + ' 间'"
                :tone="reply.total ? '' : 'warn'"
                hint="筛选、排序、分页都发给后端算：一个大区上万间，全搬到浏览器里只为筛一个词不值得。"></x-stat>
        <x-stat label="这一屏可以直接进" icon="check" tint="tint-ok" :value="joinable + ' 间'"
                :hint="'这一页只数当前这一屏（' + rows.length + ' 间里）。有空位、还收新人、没被暂停，三个都满足才算。要密码、只收战队、只有局域网这些门槛写在每一行后面那一句里。'">
          <span>{{ rows.length }} 间里的 {{ joinable }} 间</span>
        </x-stat>
      </div>

      <div class="col lobby-filters" style="gap:9px">
        <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap">
          <div class="field" style="min-width:190px;flex:1 1 220px">
            <input type="search" v-model="q" placeholder="找房间名或地址，回车生效"
                   aria-label="找房间" @keyup.enter="apply" @change="apply">
          </div>
          <div class="field" style="min-width:132px" v-if="intentOptions.length > 1">
            <select v-model="intent" aria-label="按玩法筛选" @change="apply">
              <option v-for="o in intentOptions" :key="o.value" :value="o.value">{{ o.label }}</option>
            </select>
          </div>
          <x-seg :options="accessOptions" :model-value="access" label="按门槛筛选" @update="access = $event; apply()"></x-seg>
          <x-seg :options="slotOptions" :model-value="slots" label="按空位筛选" @update="slots = $event; apply()"></x-seg>
          <x-seg :options="playerOptions" :model-value="players" label="按人数筛选" @update="players = $event; apply()"></x-seg>
        </div>
        <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap">
          <div class="field" style="min-width:150px">
            <select v-model="sort" aria-label="排序方式" @change="apply">
              <option v-for="o in sortOptions" :key="o.value" :value="o.value">{{ o.label }}</option>
            </select>
          </div>
          <div class="field" style="min-width:112px">
            <select v-model="size" aria-label="每屏几间" @change="apply">
              <option v-for="s in sizes" :key="s" :value="s">每屏 {{ s }} 间</option>
            </select>
          </div>
          <span class="grow"></span>
          <span class="muted" v-if="filteredNote">{{ filteredNote }}</span>
        </div>
        <div class="row" style="gap:6px;align-items:center;flex-wrap:wrap" v-if="activeFilters.length">
          <span class="muted">现在只看</span>
          <button class="fchip" v-for="f in activeFilters" :key="f.key" @click="clear(f.key)"
                  :aria-label="'清掉这条筛选：' + f.text">{{ f.text }}<span class="fx" aria-hidden="true">×</span></button>
          <button class="linkish" @click="clearAll">全部清掉</button>
        </div>
      </div>

      <x-note kind="warn" v-if="reply && reply.stale">
        这一屏是 {{ reply.fetchedAgo }}前那次取到的列表，这一次没能刷新。接口给的原因是：{{ reply.staleWhy }}
      </x-note>

      <div class="err-box" v-if="error">{{ error }}</div>
      <x-sk v-if="loading && !loaded" :lines="6"></x-sk>

      <x-empty v-else-if="reply && !reply.enabled" icon="globe" title="公网房间列表在配置里是关着的"
               note="配置项 lobby.enabled 为 false 时面板不会去连 Klei。要看得把那一项打开再重启面板。"></x-empty>
      <x-empty v-else-if="!rows.length && !error" icon="search" :title="emptyTitle" :note="emptyNote">
        <button class="btn ghost sm" v-if="activeFilters.length" @click="clearAll">清掉筛选</button>
        <button class="btn ghost sm" @click="load({ force: true })">取最新一次</button>
      </x-empty>

      <template v-if="rows.length">
        <!-- 桌面：一行一间。地址和端口分两列写是因为复制走的那句命令是这一行唯一的动作，
             并成一列就变成一段没法扫的文本。 -->
        <div class="table-wrap hide-sm">
          <table class="data">
            <thead>
              <tr>
                <th>房间名</th><th>地址</th><th>端口</th><th class="right">在线 / 上限</th>
                <th>玩法 · 季节</th><th>能不能进</th><th>房主填的标签</th><th></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="r in rows" :key="rowKey(r)" :class="r.joinable ? '' : 'dim'">
                <td class="wrap room-name-cell"><b :title="roomName(r)">{{ roomName(r) }}</b></td>
                <td class="mono">{{ r.addr }}</td>
                <td class="mono num">{{ r.port }}</td>
                <td class="right num">
                  {{ r.players }} / {{ r.capacity }}
                  <span class="muted" v-if="r.free">（空 {{ r.free }}）</span>
                  <span class="muted" v-else>（满）</span>
                </td>
                <td>
                  <span>{{ r.intentLabel || "没写玩法" }}</span>
                  <span class="muted" v-if="season(r)" :title="SEASON_TITLE"> · {{ season(r) }}</span>
                </td>
                <td>
                  <div class="col" style="gap:2px">
                    <x-chip :text="mark(r).text" :tone="mark(r).tone"></x-chip>
                    <span class="muted access-line">{{ r.accessLabel }}</span>
                  </div>
                </td>
                <td class="wrap">
                  <div class="row lobby-tags" style="gap:5px;flex-wrap:wrap" v-if="shownTags(r).length">
                    <span class="chip" v-for="(t, i) in shownTags(r)" :key="i" :title="t">{{ t }}</span>
                    <button class="linkish tag-more" v-if="hiddenTags(r)" @click="toggleTags(r)"
                            title="把剩下的标签摊开看">还有 {{ hiddenTags(r) }} 个</button>
                    <button class="linkish tag-more" v-else-if="openTags[rowKey(r)]" @click="toggleTags(r)">收起</button>
                  </div>
                  <span class="muted" v-else>没填</span>
                </td>
                <td class="right">
                  <div class="row" style="gap:6px;justify-content:flex-end;flex-wrap:wrap">
                    <button class="btn ghost sm" @click="copy(r)"
                            :title="'复制这句，进游戏后打开控制台（键盘左上角那个键）粘贴：' + cmd(r)">
                      <x-icon :name="copied[rowKey(r)] ? 'check' : 'copy'" :size="14"></x-icon>
                      <span>{{ copied[rowKey(r)] ? '已复制' : '复制连接' }}</span></button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 手机：卡片。这一页在手机上只做两件事——看清是哪间、把命令复制走。
             所以地址单独一行、按钮贴在底下，而不是横着滑一张八列表格。 -->
        <div class="col only-sm" style="gap:10px">
          <div class="card room-card" v-for="r in rows" :key="'m' + rowKey(r)">
            <div class="row spread" style="align-items:flex-start;gap:8px">
              <b class="room-card-name">{{ roomName(r) }}</b>
              <x-chip :text="mark(r).text" :tone="mark(r).tone"></x-chip>
            </div>
            <div class="mono muted" style="margin-top:6px">{{ r.addr }}:{{ r.port }}</div>
            <div class="row" style="gap:6px;margin-top:8px;flex-wrap:wrap">
              <span class="chip">在线 {{ r.players }} / {{ r.capacity }}</span>
              <span class="chip" v-if="r.intentLabel">{{ r.intentLabel }}</span>
              <span class="chip" v-if="season(r)" :title="SEASON_TITLE">{{ season(r) }}</span>
              <span class="chip" v-if="r.pvp">开放 PVP</span>
            </div>
            <div class="muted" style="margin-top:6px;font-size:12px">{{ r.accessLabel }}</div>
            <div class="row lobby-tags" style="gap:5px;flex-wrap:wrap;margin-top:6px" v-if="shownTags(r).length">
              <span class="chip" v-for="(t, i) in shownTags(r)" :key="'t' + i" :title="t">{{ t }}</span>
              <button class="linkish tag-more" v-if="hiddenTags(r)" @click="toggleTags(r)">还有 {{ hiddenTags(r) }} 个</button>
              <button class="linkish tag-more" v-else-if="openTags[rowKey(r)]" @click="toggleTags(r)">收起</button>
            </div>
            <div class="row" style="margin-top:8px;gap:6px">
              <span class="grow"></span>
              <button class="btn ghost sm" @click="copy(r)">
                <x-icon :name="copied[rowKey(r)] ? 'check' : 'copy'" :size="14"></x-icon>
                <span>{{ copied[rowKey(r)] ? '已复制' : '复制连接' }}</span></button>
            </div>
          </div>
        </div>

        <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap">
          <span class="muted" v-if="countText">{{ countText }}</span>
          <span class="muted" v-if="reply && !reply.live">（读的是缓存，这一次没有重打 Klei）</span>
          <span class="grow"></span>
          <x-pager :page="(reply && reply.page) || 1" :size="size" :total="(reply && reply.total) || 0"
                   :loading="loading" unit="间" @go="go"></x-pager>
        </div>

        <x-note kind="info" v-if="reply && reply.joinNote">
          {{ reply.joinNote }}
          <template v-if="copyFailed">这个浏览器不让面板写剪贴板，所以这句没能替你复制走；下面这一行字可以直接选中，再按 Ctrl+C。</template>
          <div class="cmd copy-cmd" v-if="copyFailed" aria-label="这一间房的连接命令，可以整段选中复制">{{ failedCmd }}</div>
        </x-note>
      </template>
    </div>`,
};

function labelOf(list, value) {
  const hit = list.find((o) => o.value === value);
  return hit ? hit.label : value;
}
