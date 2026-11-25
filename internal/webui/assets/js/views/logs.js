// 游戏日志页：这个分片这一轮启动到现在，引擎自己记下了什么。
//
// 同一页有两个看法，因为它们回答的是两个问题：
//   - 玩家事件：这一轮里谁进过服、谁死了、谁被请下去了。一行一记，带角色、账户编号、
//     Steam ID 和 IP —— 管理员查人是顺着 IP 或编号去找的，不是顺着昵称（昵称随时能改）。
//     筛选和分页都在后端做：界面底部那句"第 1~20 条 / 共 312 条"必须是对真的发出去
//     的那些行说的。前端自己截一遍，那个总数就只是"这一页恰好下下来的行数"。
//   - 逐行日志：这一份文件最近写了什么（一次一份，可以跟着流走）。
// 放在一页而不是拆成两条导航：事件表每一行都要能点回逐行日志看原文，跨页那个"点过去"就断了。
//
// 两个看法的参数各走各的一套（逐行读的是 level/event/关键词，事件表读的是人/动作/关键词），
// 但都跟着"房间 + 世界"这一对选择器走，所以切换看法不会把人绕丢。
//
// 界面上的三条硬约束：
//   - 手机上不做横向滚动：事件表在窄屏换成竖排的卡片（见 .ev-card），逐行日志一律换行。
//   - scanned 和 matched 两个数必须同时出现：筛完只剩两条很容易被读成"这台服务器只出过两件事"。
//   - 内存里只留最近这么多行并说明丢了多少：一个开着跟随的手机挂一小时，
//     行数能到几万，那时先崩的是浏览器而不是面板。
//
// 事件名分两层：列前缀和下拉用短名（name），整句解释（label）只在选中时展开——
// 每一行都挂一句说明，正文就被挤出屏幕了。
import * as api from "../api.js";
import { state, act, askConfirm, isBusy, rel, setStreamState } from "../store.js";
import { bytes, num, dateTime, dateText, clock } from "../fmt.js";
import { charName, charShort, charStyle } from "../gameinfo.js";

// 界面这边自己加的一项：面板没命中任何规则的行，后端给了 plain 这个合法筛选项，
// 但它不在事件字典里（字典是从规则表推出来的），所以下拉框要自己补这一条。
const PLAIN = { code: "plain", name: "未归类", label: "没归类的那些行" };
// 一屏最多留这么多行。超过就丢最早的，并把丢掉的行数说清楚。
const KEEP_ROWS = 2000;
// 事件表的窗口档位。默认值不写死在这里：后端 limits.eventDefaultLines 给了才算数，
// 前端自己记一个 500，后端改成 800 那天界面上的"最近 500 行"就成了一句假话。
const EV_FALLBACK = [200, 500, 1000, 2000];

// 列定义。opt 为真的列可以在"列设置"里收掉；玩家、时间、动作、操作四列不给关——
// 全关掉只剩一列的表格不如不摆，而"关掉时间"会让这张表失去排序依据。
const COLUMNS = [
  { key: "who", name: "玩家" },
  { key: "char", name: "角色", opt: true },
  { key: "userid", name: "账户编号", opt: true },
  { key: "steamid", name: "Steam ID", opt: true },
  { key: "time", name: "时间" },
  { key: "ip", name: "IP", opt: true },
  { key: "action", name: "动作" },
  { key: "desc", name: "说明", opt: true },
  { key: "ops", name: "操作" },
];

export default {
  name: "Logs",
  setup() { return { state, rel, isBusy, bytes, num, dateTime, dateText, clock, charName, KEEP_ROWS }; },
  data() {
    return {
      cat: null, catError: "",
      cluster: "", shard: "",
      // events 是玩家事件表，log 是逐行读一份文件。默认给事件表：进这一页的人
      // 十有八九是来找"谁出过事"，逐行原文就在同一页第二个看法里。
      view: "events",
      // 逐行日志的筛选：和后端 parseLogQuery 一一对应。
      kind: "server", lines: 200, level: "", event: "", q: "",
      follow: false, pollMs: 1000,
      // 逐行日志的结果
      meta: null, rows: [], dropped: 0,
      loading: false, error: "",
      live: false, stopped: false,
      // 事件表的筛选（和后端 parseEventQuery 一一对应）与结果。
      // evErr 配 evErrStatus：503 是"还没有日志文件"，那是状态不是故障，
      // 用红框说会把"世界没启动过"读成"日志坏了"。
      ev: { player: "", event: "", q: "", chat: false, lines: 500, size: 20, page: 1 },
      evp: null, evLoading: false, evErr: "", evErrStatus: 0,
      cols: { char: true, userid: true, steamid: true, ip: true, desc: true },
      colsOpen: false,
      // 一次改动会同时动到几个筛选字段，每个监视器都叫一次 apply()；合并到同一帧只读一次。
      applyQueued: false,
      // 滚动
      stick: true, below: 0,
      // 手机上被折成长块的那些行：同一时刻只放开一行，记的是这一行的键。
      expanded: null,
      filtersOpen: true,
      ctrl: null,
      // SSE 续读：lastId 是最后一帧的 SSE id（就是行 offset），meta 帧的 startOffset 作初始锚点。
      // streamStatus 由 streamReconnect 的 onState 写，同时用 setStreamState 同步到全局 state.stream。
      lastId: "", streamStatus: "idle", streamInfo: {},
      resetNote: "",        // 轮转帧给用户的一句明确说明
      filterError: "",      // 筛选区附近："是哪个条件没生效"
      // 历史翻页（/log/history）：和"末尾窗口"是两套，offset/limit 按整份文件筛完算。
      hist: null, histLoading: false, histError: "", histOffset: 0, histLimit: 200,
      // 导出
      exportFormat: "text", exporting: false, exportError: "",
      // 9 列表格的密度：comfortable / dense，作用在表格容器上。
      density: "comfortable",
    };
  },
  computed: {
    kinds() { return (this.cat && this.cat.kinds) || [{ code: "server", label: "服务端日志" }, { code: "chat", label: "聊天与公告" }]; },
    levels() { return (this.cat && this.cat.levels) || []; },
    events() {
      const list = ((this.cat && this.cat.events) || []).slice();
      if (!list.some((e) => e.code === PLAIN.code)) list.push(PLAIN);
      return list;
    },
    // 动作下拉只给"定性的那十一种"（后端从规则表推出来的一份）。字典里那些引擎运行事件
    // （启动、模组加载、存档落盘）在这张表里没有对应的行，列进来就是筛不出的选项。
    behaviours() { return (this.cat && this.cat.behaviours) || []; },
    limits() { return (this.cat && this.cat.limits) || { defaultLines: 200, maxLines: 2000, minPollMs: 200, maxPollMs: 10000 }; },
    // 选中某个事件后，筛选框下面换成它的整句解释：短名能在一排选项里分清楚，
    // 但要弄明白"这条规则到底在说什么"还是得看整句。
    selectedEvent() { return this.events.find((e) => e.code === this.event) || null; },
    selectedBehaviour() { return this.behaviours.find((b) => b.code === this.ev.event) || null; },
    lineChoices() {
      // 只给四档，最大值取后端说的那个上限：超过就是 400，界面不该把人往那儿引。
      const max = Number(this.limits.maxLines) || 2000;
      return [50, 200, 500, max].filter((n, i, a) => n <= max && a.indexOf(n) === i);
    },
    // 事件表的窗口档位：不超过上限，并且必须包含后端那个默认值——
    // 默认值不在档位里，界面第一屏写的行数和实际数的行数就不是一个数。
    evChoices() {
      const max = Number(this.limits.maxLines) || 2000;
      const def = Number(this.limits.eventDefaultLines) || 500;
      const set = EV_FALLBACK.concat([def, max]).filter((n) => n >= 1 && n <= max);
      return Array.from(new Set(set)).sort((a, b) => a - b);
    },
    sizeChoices() {
      const max = Number(this.limits.eventMaxSize) || 100;
      return Array.from(new Set([10, 20, 50, max].filter((n) => n >= 1 && n <= max))).sort((a, b) => a - b);
    },
    shards() {
      const c = state.clusters.find((x) => x.name === this.cluster);
      return c ? c.shards || [] : [];
    },
    allCols() { return COLUMNS; },
    // 一句话把"这张表为什么有列是空的"说清，而不是每一行挂一句：
    // 连接行写得很早，早就滚出回看窗口了，那时这一行的 IP 就是空的。
    evIdentityNote() {
      if (!this.evp) return "";
      const n = Number(this.evp.identitySeen) || 0;
      return n
        ? "这一轮里认出了 " + num(n) + " 个人的连接线索（IP、Steam ID、角色都是从这些线索凑出来的）。"
          + "窗口里太早的那些行凑不齐，所以列会是空的——不是面板漏读了那份文件。"
        : "这一轮的窗口里还没有连接线索：更早的行没算进来时，IP 和 Steam ID 这两列就是空的。";
    },
    // 一份日志都还没写出来时给空状态，不给红框（同上面那条理由）。
    evNoFiles() { return !!this.evErr && this.evErrStatus === 503; },
    // 页码一律读后端给的那一个：越界的请求它已经夹回来了，
    // 界面写"第 3 页"而数据是第 2 页的，这句话就是假的。
    evPage() { return this.evp ? Number(this.evp.page) || 1 : 1; },
    evRange() {
      if (!this.evp) return "";
      const n = (this.evp.rows || []).length;
      if (!n) return "0";
      const from = (this.evPage - 1) * (Number(this.evp.size) || this.ev.size) + 1;
      return num(from) + "–" + num(from + n - 1);
    },
    evActiveFilters() {
      let n = 0;
      if (this.ev.player.trim()) n++;
      if (this.ev.event) n++;
      if (this.ev.q.trim()) n++;
      if (this.ev.chat) n++;
      return n;
    },
    activeFilters() {
      if (this.view === "events") return this.evActiveFilters;
      let n = 0;
      if (this.level) n++;
      if (this.event) n++;
      if (this.q.trim()) n++;
      return n;
    },
    counts() {
      const c = { info: 0, warn: 0, error: 0 };
      this.rows.forEach((r) => { c[r.level] = (c[r.level] || 0) + 1; });
      return c;
    },
    // 这一屏里出现过的事件：手机上不想逐个下拉框试，就从这里点进去。
    hotEvents() {
      const by = {};
      this.rows.forEach((r) => {
        const code = r.event || "plain";
        if (!by[code]) by[code] = { code, name: r.eventName || "未归类", label: r.eventLabel || "", n: 0, warn: r.level !== "info" };
        by[code].n++;
      });
      return Object.keys(by).map((k) => by[k]).sort((a, b) => b.n - a.n).slice(0, 8);
    },
    canWrite() { return !!(state.me && state.me.isAdmin); },
    // 状态牌给一条有出口的话：断线就说在重连、能点立即重连，不再写"按刷新看"。
    streamText() {
      if (this.live) return "正在跟这条流";
      if (this.stopped) return "流已收尾";
      if (this.streamStatus === "reconnecting") return "连接断开了，正在自动重连（可点上方「立即重连」）";
      if (this.streamStatus === "connecting") return "正在连接…";
      return "没在跟随：打开上面的「实时跟随」，或点「立即重连」";
    },
    histFrom() { return this.hist ? Number(this.hist.offset) || 0 : 0; },
    histTo() { return this.hist ? this.histFrom + (this.hist.lines || []).length : 0; },
    histTotal() { return this.hist ? Number(this.hist.total) || 0 : 0; },
    histHasMore() { return !!(this.hist && this.hist.hasMore); },
    histNextOffset() { return this.hist ? Number(this.hist.nextOffset) || 0 : 0; },
  },
  watch: {
    cluster() {
      this.shard = this.shards.length ? this.shards[0].name : "";
      this.apply();
    },
    shard() { this.apply(); },
    kind() { this.apply(); },
    level() { this.apply(); },
    event() { this.apply(); },
    lines() { this.apply(); },
    follow() { this.apply(); },
    // 事件表：除了页码，动任何一个条件都必须回到第 1 页——
    // 带着"第 4 页"去筛一个新玩家，很可能直接落进空列表，看着像"他没干过任何事"。
    "ev.player"() { this.evSearch(); },
    // 动作里点了「玩家发言」：那一道开关必须跟着打开。不这么做的话这一档筛出来是空表
    // （默认开关把发言整档收掉了），而后端会替用户打开——界面显示"不算发言"、表里全是发言，
    // 两边各说一套。这里跟着改开关，让屏上的状态就是真的状态。
    "ev.event"(v) {
      if (v === "chat" && !this.ev.chat) { this.ev.chat = true; return; }
      this.evSearch();
    },
    "ev.q"() { this.evSearch(); },
    "ev.chat"(v) {
      // 反过来把开关关掉时，"只看发言"这个动作筛选就自相矛盾了：清掉它，
      // 别留一栏在那里让后端和开关争。
      if (!v && this.ev.event === "chat") { this.ev.event = ""; return; }
      this.evSearch();
    },
    "ev.lines"() { this.evSearch(); },
    "ev.size"() { this.evSearch(); },
    "ev.page"() { this.apply(); },
    // 选中房间跟着顶栏/别的页面走：总览里切的房间，进这一页不该还是上一个。
    "state.sel.cluster"(v) { if (v && v !== this.cluster) this.cluster = v; },
  },
  mounted() {
    // 手机上默认把筛选折起来：那一排五个字段在 390px 下就是六行，
    // 会把"日志本身"挤出第一屏，而用户进这一页要看的是日志。
    if (window.matchMedia && window.matchMedia("(max-width: 560px)").matches) this.filtersOpen = false;
    this.loadCatalog();
    this.cluster = state.sel.cluster || (state.clusters[0] && state.clusters[0].name) || "";
    if (!this.cluster && !state.clusters.length) {
      // 房间列表还没轮到（面板刚起来、总览那次轮询还没回），等一次再取。
      api.get("/api/v1/clusters").then((res) => {
        if (!res.ok || this.cluster) return;
        const list = res.data || [];
        if (list.length) {
          this.cluster = list[0].name;
        }
        // 一间都没有就不写 this.error：那是一条红框，而"还没有房间"不是出错，
        // 页面下面那栏空状态已经把它说清了（红框再加一句同样的话，只会让人以为日志坏了）。
      });
      return;
    }
    this.apply();
  },
  beforeUnmount() {
    this.stop();                 // 离开这一页一定要把 SSE 断掉：后端只靠连接断开来收尾
    setStreamState("idle", {});  // 顺手把全局流状态收起来，别把断线牌带到别的页面
  },
  methods: {
    async loadCatalog() {
      const res = await api.get("/api/v1/log-events");
      if (!res.ok) { this.catError = res.error || ("接口返回 " + res.status); return; }
      this.cat = res.data;
      const L = this.cat.limits || {};
      this.lines = Number(L.defaultLines) || this.lines;
      this.ev.lines = Number(L.eventDefaultLines) || this.ev.lines;
      this.ev.size = Number(L.eventDefaultSize) || this.ev.size;
    },
    query(extra) {
      return api.qs(Object.assign({
        kind: this.kind, lines: this.lines,
        level: this.level, event: this.event, q: this.q.trim(),
      }, extra || {}));
    },
    base() {
      return "/api/v1/clusters/" + encodeURIComponent(this.cluster) +
        "/shards/" + encodeURIComponent(this.shard) + "/log";
    },
    // 流式是另一条路由（后端 /log 只回 JSON）。少了 "/stream" 这一段不会 404：
    // 那次读到的是 200 的一整行 JSON，按 SSE 的帧分隔切不出东西，于是界面既不报错也不出行。
    streamBase() { return this.base() + "/stream"; },
    eventsBase() {
      return "/api/v1/clusters/" + encodeURIComponent(this.cluster) +
        "/shards/" + encodeURIComponent(this.shard) + "/events";
    },
    // 只把非默认的东西发出去：空格子留在 URL 和请求里会让"筛了没筛"看不出来。
    evQuery() {
      return api.qs({
        lines: this.ev.lines, page: this.ev.page, size: this.ev.size,
        player: this.ev.player.trim(), event: this.ev.event,
        q: this.ev.q.trim(), chat: this.ev.chat ? "1" : "",
      });
    },

    // ---- 一次动作：先断掉在跑的，再按看法和开关选一种读法 ----

    apply() {
      // 一次改动可能同时动到好几个字段（点"清掉"会连着改三个条件，从事件表跳过来会
      // 同时改 kind 和 event），每个监视器都会叫一次 apply()。合并到同一帧只读一次：
      // 并发读时最后落进 rows 的是哪一份不确定，跟随模式下还会连着断建三条流。
      if (this.applyQueued) return;
      this.applyQueued = true;
      this.$nextTick(() => { this.applyQueued = false; this.reload(); });
    },
    evSearch() {
      if (this.ev.page !== 1) { this.ev.page = 1; return; }  // 改页码本身就是一次读取
      this.apply();
    },
    reload() {
      this.stop();
      // 换房间/换筛选/换看法 = 另一份文件：旧的续读锚点、轮转说明、历史页都作废。
      this.lastId = ""; this.streamStatus = "idle"; this.streamInfo = {};
      this.resetNote = ""; this.filterError = "";
      this.hist = null; this.histError = "";
      // 非跟随 / 事件表没有流在跑：把全局流状态收成 idle，别让断线牌挂在一屏没订阅的页面上。
      setStreamState("idle", {});
      this.rows = []; this.dropped = 0; this.meta = null;
      this.error = ""; this.below = 0; this.stick = true;
      this.expanded = null; this.colsOpen = false;
      this.evp = null; this.evErr = ""; this.evErrStatus = 0;
      if (!this.cluster || !this.shard) return;
      if (this.view === "events") { this.readEvents(); return; }
      if (this.follow) this.openStream(); else this.readOnce();
    },
    setView(v) {
      if (this.view === v) return;
      this.view = v;
      this.apply();
    },

    async readEvents() {
      this.evLoading = true;
      const res = await api.get(this.eventsBase() + this.evQuery());
      this.evLoading = false;
      if (!res.ok) {
        this.evErrStatus = res.status || 0;
        this.evErr = res.error || ("接口返回 " + res.status);
        // 503 是"还没有日志文件"，那是状态不是条件没生效；其余失败才在筛选区点名条件。
        if (res.status !== 503) this.markFilterError("玩家事件表");
        return;
      }
      this.evErr = ""; this.evErrStatus = 0;
      this.clearFilterError();
      this.evp = res.data || null;
    },
    // 事件表里点"看原文"：把这一行的出处（哪份文件、哪个事件）和人一起带过去当条件。
    // 必须连 kind 一起带：/log 一次只读一份，只带事件码的话，聊天那一记会在服务端
    // 日志里筛成空列表——那是个永远为空的链接，看着像"他没说过话"。
    showRaw(r) {
      this.view = "log";
      this.kind = r.kind || "server";
      this.event = r.event || "";
      this.level = "";
      this.q = r.player || "";
      this.lines = Number(this.limits.maxLines) || 2000;
      this.follow = false;
      this.apply();
    },
    gotoPage(n) { this.ev.page = n; },
    resetFilters() {
      if (this.view === "events") {
        this.ev.player = ""; this.ev.event = ""; this.ev.q = ""; this.ev.chat = false;
      } else {
        this.level = ""; this.event = ""; this.q = "";
      }
    },
    toggleCol(key) { this.cols[key] = !this.cols[key]; },
    async banUser(r) {
      if (!r.userid) return;
      const who = r.player || r.userid;
      const ok = await askConfirm({
        title: "把 " + who + " 加进黑名单？",
        body: "会写进这个房间的 blocklist.txt，引擎下次启动世界时读这份名单。",
        warn: "这一页按历史行拉黑，不会马上切断连接：他此刻还在线的话，请到玩家管理页对他用「拉黑」。",
        danger: true,
        okLabel: "加进黑名单",
      });
      if (!ok) return;
      const key = "evban:" + r.userid;
      const res = await act(key, "POST",
        "/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/players/lists/blocklist",
        { action: "add", userid: r.userid },
        {
          clusters: false, after: false,
          okTitle: "已加进黑名单",
          okBody: () => "写进了 blocklist.txt；要重启世界才会被引擎读到。",
          badTitle: "黑名单没改成",
        });
      if (res.ok) this.readEvents();
    },

    async readOnce() {
      this.loading = true;
      const res = await api.get(this.base() + this.query());
      this.loading = false;
      if (!res.ok) {
        this.error = res.error || ("接口返回 " + res.status);
        this.markFilterError("逐行日志");
        return;
      }
      this.clearFilterError();
      const v = res.data || {};
      this.meta = v;
      this.rows = v.entries || [];
      this.trim();
      this.jump(true);
    },
    // 订阅走 api.streamReconnect：它自动带 Last-Event-ID 续读、指数退避重连。
    // 每帧的 id（行 offset）记进 lastId，meta 帧的 startOffset 作初始锚点；
    // onState 同步到全局 state.stream，顶部 x-reconnect-banner 才有得显示。
    // 不 await：长期流的 streamReconnect 不会 resolve（除非被 abort），await 会把这条挂住。
    openStream() {
      const ctrl = new AbortController();
      this.ctrl = ctrl;
      this.live = true;
      this.loading = true;
      this.stopped = false;
      const handlers = {
        onFrame: (ev, d, id) => {
          if (this.ctrl !== ctrl) return;    // 被新订阅取代的旧连接，迟到的帧不写进当前表
          this.frame(ev, d, id);
        },
        onState: (status, info) => {
          if (this.ctrl !== ctrl) return;
          this.onStreamState(status, info);
        },
        getLastId: () => this.lastId,
      };
      api.streamReconnect(this.streamBase() + this.query({ pollMs: this.pollMs }), handlers, ctrl.signal)
        .then(() => {
          if (this.ctrl !== ctrl) return;
          this.live = false; this.loading = false;
        })
        .catch(() => {
          if (this.ctrl !== ctrl) return;
          this.live = false; this.loading = false;
        });
    },
    onStreamState(status, info) {
      this.streamStatus = status;
      this.streamInfo = info || {};
      setStreamState(status, info || {});   // 写进全局：x-reconnect-banner 读 state.stream.status
      if (status === "connecting") {
        this.live = true; this.loading = true;
      } else if (status === "connected") {
        this.live = true; this.loading = false;   // 首帧到了：连接已建立，别再转圈
      } else if (status === "reconnecting" || status === "closed" || status === "error") {
        this.live = false; this.loading = false;
      }
    },
    // 顶部断线牌的「立即重连」：保留 lastId，新订阅会带 Last-Event-ID 续读，
    // 断线期间新打出来的行补得回来；先 abort 掉正在退避等待的那条，再立刻重发。
    manualReconnect() {
      const keep = this.lastId;
      this.stop();
      this.lastId = keep;
      this.error = "";
      this.stopped = false;
      setStreamState("connecting", { attempt: 0 });
      if (!this.cluster || !this.shard) return;
      this.openStream();
    },
    frame(ev, d, id) {
      // 每帧的 SSE id 就是这一行的 offset：记下来给 getLastId() 续读用。
      if (id !== undefined && id !== null && String(id) !== "") this.lastId = String(id);
      this.loading = false;
      if (ev === "meta") {
        this.meta = d;
        // meta 帧带 startOffset（本次流实际从哪个 offset 开始），和首帧 id 是同一个值；
        // 明确再定一次锚点，断线重连的 Last-Event-ID 才落在正确位置。
        if (d && d.startOffset !== undefined && d.startOffset !== null) this.lastId = String(d.startOffset);
        return;
      }
      if (ev === "line") { this.rows.push(d); this.trim(); this.jump(false); return; }
      if (ev === "reset") {
        // 引擎把日志截断重写（或换了文件）：新一轮的 offset 从 0 重新往上走。
        this.rows = []; this.dropped = 0;
        // 归零用空串（等价于不带 Last-Event-ID）：写 "0" 会让后端从文件头把整轮重发一遍。
        this.lastId = "";
        if (this.meta) {
          this.meta.startAt = d.startAt;
          this.meta.sizeBytes = d.sizeBytes;
          if (d.startOffset !== undefined) this.meta.startOffset = d.startOffset;
          if (d.fileId) this.meta.fileId = d.fileId;
        }
        this.resetNote = "日志文件已轮转，列表从新一轮重新开始（更早的行请用「加载更早的行」翻历史）。";
        this.error = "";
        return;
      }
      if (ev === "error") {
        this.error = d.error || "日志流出错了";
        this.live = false;
        this.markFilterError("日志流");
        return;
      }
      if (ev === "done") { this.live = false; this.stopped = true; }
    },
    stop() {
      if (this.ctrl) { this.ctrl.abort(); this.ctrl = null; }
      this.live = false; this.loading = false;
    },
    trim() {
      if (this.rows.length <= KEEP_ROWS) return;
      const over = this.rows.length - KEEP_ROWS;
      this.rows.splice(0, over);
      this.dropped += over;
    },
    jump(force) {
      if (!force && !this.stick) { this.below++; return; }
      this.$nextTick(() => {
        const el = this.$refs.box;
        if (!el) return;
        el.scrollTop = el.scrollHeight;
        if (force) this.below = 0;
      });
    },
    onScroll() {
      const el = this.$refs.box;
      if (!el) return;
      // 手指往上滑就停止自动滚：手机上读一段历史时最烦的就是画面自己跳。
      this.stick = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
      if (this.stick) this.below = 0;
    },
    pickEvent(code) {
      this.event = this.event === code ? "" : code;      // 再点一次取消这条筛选
    },
    lv(r) { return r.level === "error" ? "err" : (r.level === "warn" ? "warn" : ""); },
    // 一行的原文被折起来时放开的键：事件表里两份文件的 offset 会撞，所以带上出处。
    rowKey(r) { return (r.kind || "") + ":" + r.offset; },
    // 手机上/宽屏上都一样：引擎那种 1000 字的长行（Lua 堆栈、modconfig 转储）
    // 单独一行就能把整屏占满，而这一屏要看的是"最近发生了什么"。
    isLong(r) { return (r.raw || "").length > 160; },
    rowCls(r) { return this.lv(r) + (this.expanded === r.offset ? " expanded" : ""); },
    toggleRow(r) { this.expanded = this.expanded === r.offset ? null : r.offset; },
    toggleRaw(r) { const k = this.rowKey(r); this.expanded = this.expanded === k ? null : k; },
    // 一条记录上的钟点：换算得出墙上时间就用它，否则退回引擎自己的相对秒数。
    stamp(r) { return r.at ? dateTime(r.at).slice(11) : (r.clock ? "[" + r.clock + "]" : "—"); },
    // 头像上的字：知道角色就写角色短名，不知道就取昵称首字。
    avaText(r) { return r.prefab ? charShort(r.prefab) : String(r.player || "？").slice(0, 1); },
    avaStyle(r) { return charStyle(r.prefab || r.player || ""); },
    avaTitle(r) { return r.prefab ? charName(r.prefab) : "不知道这一记是谁做的、用的哪个角色"; },
    evRowCls(r) { return this.expanded === this.rowKey(r) ? "expanded" : ""; },

    // ---- 筛选失败时，在筛选区把"是哪个条件没生效"说清，并给「重试」 ----
    describeFailedFilters() {
      const parts = [];
      if (this.view === "events") {
        if (this.ev.player.trim()) parts.push("玩家「" + this.ev.player.trim() + "」");
        if (this.ev.event) parts.push("动作「" + this.ev.event + "」");
        if (this.ev.q.trim()) parts.push("关键词「" + this.ev.q.trim() + "」");
        if (this.ev.chat) parts.push("含玩家发言");
      } else {
        if (this.level) parts.push("级别「" + this.level + "」");
        if (this.event) parts.push("事件「" + this.event + "」");
        if (this.q.trim()) parts.push("关键词「" + this.q.trim() + "」");
        parts.push("文件「" + this.kind + "」");
      }
      return parts.length ? parts.join("、") : "没有额外条件（全量）";
    },
    markFilterError(what) {
      this.filterError = what + "没读成，这次没生效的条件是：" + this.describeFailedFilters() +
        "。改完条件或点「重试」再读一次。";
    },
    clearFilterError() { this.filterError = ""; },

    // ---- 历史日志翻页：整份文件按 offset/limit 读，不再只有末尾 200/2000 行 ----
    historyBase() { return this.base() + "/history"; },
    histQuery(offset) {
      return api.qs({ kind: this.kind, level: this.level, event: this.event,
                      q: this.q.trim(), offset, limit: this.histLimit });
    },
    async loadHistory(offset) {
      if (!this.cluster || !this.shard) return;
      this.histLoading = true;
      this.histError = "";
      const res = await api.get(this.historyBase() + this.histQuery(offset));
      this.histLoading = false;
      if (res.ok) {
        this.clearFilterError();
        this.hist = res.data || null;
        // 越界的 offset 后端会夹回来，照它回写的 offset 校准页码（界面上的"第几页"不能是假的）。
        this.histOffset = Number(this.hist && this.hist.offset) || 0;
      } else {
        this.histError = res.error || ("接口返回 " + res.status);
        this.markFilterError("历史翻页");
      }
    },
    histReload() { this.loadHistory(this.histOffset); },
    histPrev() { this.loadHistory(Math.max(0, this.histOffset - this.histLimit)); },
    histNext() { this.loadHistory(this.histNextOffset); },
    // 「加载更早的行」：没读过就从第 0 条拉第一批，读过就顺着 nextOffset 再拉一批。
    histLoadMore() { this.loadHistory(this.hist ? this.histNextOffset : 0); },

    // ---- 导出：带鉴权下载，格式 text / ndjson ----
    async exportLog() {
      if (!this.cluster || !this.shard) return;
      this.exporting = true;
      this.exportError = "";
      const q = api.qs({ kind: this.kind, level: this.level, event: this.event,
                         q: this.q.trim(), format: this.exportFormat });
      const ext = this.exportFormat === "ndjson" ? "ndjson" : "txt";
      const filename = [this.cluster, this.shard, this.kind, "log"].join("_") + "." + ext;
      const res = await api.download(this.base() + "/export" + q, filename);
      this.exporting = false;
      if (!res.ok) this.exportError = res.error || "导出没成";
    },
  },
  template: `
    <div class="col" style="gap:14px">
      <!-- 断线牌由 state.stream.status 驱动：streamReconnect 的 onState 写进去，
           点「立即重连」立刻重发订阅（保留 lastId 补漏）。只在逐行日志 + 跟随时挂。 -->
      <x-reconnect-banner v-if="view === 'log' && follow" title="游戏日志"
                          @reconnect="manualReconnect"></x-reconnect-banner>
      <!-- 没有房间就没有日志文件，也就没有可筛的东西：这一整排控件（两个空下拉、
           哪一份、实时跟随）和下面的筛选面板一起收掉，只留一句"这一页要什么"。 -->
      <div class="log-controls" v-if="state.clusters.length">
        <div class="field" style="min-width:150px;flex:1">
          <label>房间</label>
          <select v-model="cluster">
            <option value="" disabled>选一个房间</option>
            <option v-for="c in state.clusters" :key="c.name" :value="c.name">{{ c.name }}</option>
          </select>
        </div>
        <div class="field" style="min-width:120px">
          <label>世界</label>
          <select v-model="shard" :disabled="!cluster">
            <option value="" disabled>选一个世界</option>
            <option v-for="s in shards" :key="s.name" :value="s.name">{{ s.name }}</option>
          </select>
        </div>
        <div class="lc-right">
          <div class="col">
            <label class="card-title-sm hide-sm">看什么</label>
            <div class="seg">
              <button :class="view === 'events' ? 'on' : ''" @click="setView('events')">玩家事件</button>
              <button :class="view === 'log' ? 'on' : ''" @click="setView('log')">逐行日志</button>
            </div>
          </div>
          <!-- 表格密度：9 列的事件表默认舒适，挤的时候一键切紧凑（作用在表格容器上）。 -->
          <div class="col">
            <label class="card-title-sm hide-sm">疏密</label>
            <div class="seg">
              <button :class="density === 'comfortable' ? 'on' : ''" @click="density = 'comfortable'">舒适</button>
              <button :class="density === 'dense' ? 'on' : ''" @click="density = 'dense'">紧凑</button>
            </div>
          </div>
          <!-- "哪一份"和"实时跟随"只对逐行日志有意义：事件表一次读两份文件（不然
               聊天里的进服公告会看不见），也没有"跟着流走"这种看法。
               在这里收掉而不是灰着：留一枚按下去没反应的开关，比少一枚更难解释。 -->
          <div class="col" v-if="view === 'log'">
            <label class="card-title-sm hide-sm">哪一份</label>
            <div class="seg">
              <button v-for="k in kinds" :key="k.code" :class="kind === k.code ? 'on' : ''"
                      @click="kind = k.code">{{ k.label }}</button>
            </div>
          </div>
          <label class="switch" v-if="view === 'log'" title="开着就跟着这条日志流走，切页面或关掉就断开">
            <input type="checkbox" v-model="follow" />
            <span class="track"></span>
            <span>实时跟随</span>
          </label>
        </div>
      </div>

      <x-note v-if="catError" kind="warn">{{ catError }}</x-note>

      <!-- 接口失败时除了下面那块整表 err-box，在筛选区把"哪个条件没生效"点名并留「重试」。
           放在 details 外面：窄屏筛选折起时这句话也还在，不会跟着被收掉。 -->
      <x-note kind="warn" v-if="filterError">
        {{ filterError }}
        <button class="linkish" @click="apply">重试</button>
      </x-note>

      <!-- 筛选在窄屏默认折起：五个字段摊开就是六行，正文本身会被挤出第一屏。 -->
      <details class="group" v-if="state.clusters.length"
               :open="filtersOpen" @toggle="filtersOpen = $event.target.open">
        <summary class="g-head">
          <span class="g-chev">›</span>
          <b>筛选</b>
          <span class="g-count" v-if="activeFilters">已筛 {{ activeFilters }} 项</span>
          <span class="g-count" v-else>没筛，全部都给</span>
          <span class="grow"></span>
          <button class="linkish" v-if="activeFilters" @click.prevent.stop="resetFilters">清掉</button>
        </summary>

        <!-- 事件表的筛选：人、动作、关键词三个条件各管一栏，是"与"的关系。
             这里没有"日期区间"：后端没这个条件，摆一个进去就是按下去不改任何东西的控件。 -->
        <div class="grid-form" v-if="view === 'events'">
          <div class="field">
            <div class="field-head">
              <label>玩家</label>
              <x-hint text="在昵称和账户编号里找，忽略大小写；填一段就够，不用整串抄全。"></x-hint>
            </div>
            <input type="search" v-model="ev.player" placeholder="昵称，或 KU_ 开头的账户编号"
                   @keyup.enter="apply" />
          </div>
          <div class="field">
            <div class="field-head">
              <label>动作</label>
              <x-hint :text="'面板能从引擎日志里定性出 ' + behaviours.length + ' 种玩家行为；没选中时这里不做筛选。下面那一行是选中项的整句解释。'"></x-hint>
            </div>
            <select v-model="ev.event">
              <option value="">全部动作</option>
              <option v-for="b in behaviours" :key="b.code" :value="b.code">{{ b.name }}</option>
            </select>
            <span class="hint" v-if="selectedBehaviour">{{ selectedBehaviour.label }}</span>
          </div>
          <div class="field">
            <div class="field-head">
              <label>关键词</label>
              <x-hint text="在整行原文里找，也盖住 IP、Steam ID、账户编号和角色——顺着编号捞人就是靠这一栏。"></x-hint>
            </div>
            <input type="search" v-model="ev.q" placeholder="IP、Steam ID、任意一段原文"
                   @keyup.enter="apply" />
          </div>
          <div class="field">
            <div class="field-head">
              <label>回看多少行</label>
              <x-hint :text="'两份文件各读这么多行，上限由后端定（' + limits.maxLines + ' 行）。更早的行为得在逐行日志里翻那一份文件。'"></x-hint>
            </div>
            <select v-model.number="ev.lines">
              <option v-for="n in evChoices" :key="n" :value="n">{{ n }} 行</option>
            </select>
          </div>
          <div class="field">
            <div class="field-head">
              <label>每页几条</label>
              <x-hint :text="'上限由后端定（最多 ' + limits.eventMaxSize + ' 条）。改这一档会回到第 1 页。'"></x-hint>
            </div>
            <select v-model.number="ev.size">
              <option v-for="n in sizeChoices" :key="n" :value="n">{{ n }} 条</option>
            </select>
          </div>
          <div class="field">
            <div class="field-head">
              <label>玩家发言</label>
              <x-hint text="开着才把聊天记录算进这张表和那个总数里。默认关着：聊十句就是一页，进出和死亡会被冲散。"></x-hint>
            </div>
            <label class="switch" title="把玩家说的话也列进这张表">
              <input type="checkbox" v-model="ev.chat" />
              <span class="track"></span>
              <span>{{ ev.chat ? '一起列进来' : '不算发言' }}</span>
            </label>
          </div>
          <div class="field" style="justify-content:flex-end">
            <label>&nbsp;</label>
            <button class="btn primary sm" @click="apply" :disabled="!shard || evLoading">
              {{ evLoading ? '读取中…' : '就这样筛' }}</button>
          </div>
        </div>

        <!-- 逐行日志的筛选：级别、事件、关键词，全是这一份文件的条件。 -->
        <div class="grid-form" v-else>
          <div class="field">
            <div class="field-head">
              <label>级别下限</label>
              <x-hint text="选“警告”会连报错一起给，选“普通”就是不分档。"></x-hint>
            </div>
            <select v-model="level">
              <option value="">全部级别</option>
              <option v-for="l in levels" :key="l.code" :value="l.code">{{ l.label }}及以上</option>
            </select>
          </div>
          <div class="field">
            <div class="field-head">
              <label>事件</label>
              <x-hint :text="'字典是面板从引擎日志里归纳的，共 ' + events.length + ' 类；没选中时这里不做筛选。下面那一行是选中项的整句解释。'"></x-hint>
            </div>
            <select v-model="event">
              <option value="">全部事件</option>
              <option v-for="e in events" :key="e.code" :value="e.code">{{ e.name }}</option>
            </select>
            <span class="hint" v-if="selectedEvent">{{ selectedEvent.label }}</span>
          </div>
          <div class="field">
            <div class="field-head">
              <label>关键词</label>
              <x-hint text="在原文、玩家名和编号里找，忽略大小写；回车或点“就这样筛”。"></x-hint>
            </div>
            <input type="search" v-model="q" placeholder="玩家名、物品名、任意一段原文"
                   @keyup.enter="apply" />
          </div>
          <div class="field">
            <div class="field-head">
              <label>回看多少行</label>
              <x-hint :text="'上限由后端定（' + limits.maxLines + ' 行）。要更早的内容不用上机器 grep：下面「历史日志」里点「加载更早的行」按整份文件翻页。'"></x-hint>
            </div>
            <select v-model.number="lines">
              <option v-for="n in lineChoices" :key="n" :value="n">{{ n }} 行</option>
            </select>
          </div>
          <div class="field" v-if="follow">
            <div class="field-head">
              <label>多久问一次</label>
              <x-hint text="越小越跟手，也越费那点磁盘读取；4h4g 上 1 秒是个够用的折中。"></x-hint>
            </div>
            <select v-model.number="pollMs">
              <option :value="500">0.5 秒</option>
              <option :value="1000">1 秒</option>
              <option :value="2000">2 秒</option>
              <option :value="5000">5 秒</option>
            </select>
          </div>
          <div class="field" style="justify-content:flex-end">
            <label>&nbsp;</label>
            <button class="btn primary sm" @click="apply" :disabled="!shard || loading">
              {{ loading ? '读取中…' : '就这样筛' }}</button>
          </div>
        </div>
      </details>

      <!-- 导出：带鉴权下载，格式可选 text / ndjson，导出期间按钮转圈。 -->
      <div class="row" style="gap:8px;flex-wrap:wrap;align-items:center"
           v-if="view === 'log' && state.clusters.length">
        <span class="card-title-sm">导出这一份日志</span>
        <select v-model="exportFormat" style="width:auto;min-width:150px">
          <option value="text">纯文本（text）</option>
          <option value="ndjson">逐行 JSON（ndjson）</option>
        </select>
        <button class="btn sm" :disabled="exporting || !shard" @click="exportLog">
          {{ exporting ? '导出中…' : '导出' }}</button>
        <span class="hint" v-if="exportError" style="color:var(--bad)">{{ exportError }}</span>
        <span class="hint" v-else>按当前筛选条件导出整份文件，不只是这一屏。</span>
      </div>

      <!-- 读不出东西时只留一栏空状态，原因分两种说清。一间房间都没有的时候
           根本没有日志文件，所以那一屏不能再写"文件在，但这一轮还没写出内容"。 -->
      <x-empty v-if="!cluster || !shard" icon="lines"
        :title="state.clusters.length ? '先选房间和世界' : '这台机器上还没有房间'"
        :note="state.clusters.length
                 ? '日志是一个世界一份文件（地表和洞穴各写自己的），所以得先说清楚看哪一个。'
                 : '日志文件是世界启动后由引擎一行行写出来的：先建一个房间并把世界开起来，这一页才有东西可看。'">
        <a class="btn sm primary" href="#/room" v-if="!state.clusters.length">去开服向导</a>
      </x-empty>

      <!-- ---- 玩家事件表 ---- -->
      <template v-else-if="view === 'events'">
        <div class="err-box" v-if="evErr && !evNoFiles">{{ evErr }}</div>
        <x-empty v-else-if="evNoFiles" icon="lines" title="这个世界还没有日志文件" :note="evErr">
          <a class="btn sm primary" href="#/room">去开服向导</a>
          <button class="btn sm ghost" @click="apply">再读一遍</button>
        </x-empty>
        <x-sk v-else-if="evLoading && !evp" :lines="6"></x-sk>

        <div class="card card-flush" v-else-if="evp">
          <div class="card-head" style="flex-wrap:wrap">
            <span class="ico-tile tint-gold"><x-icon name="users" :size="15"></x-icon></span>
            <h3>{{ evp.cluster }} / {{ evp.shard }}：玩家事件</h3>
            <span class="chip">每份文件回看 {{ num(evp.window) }} 行</span>
            <span class="chip" v-if="evp.roundStart">本轮从 {{ dateText(evp.roundStart) }} {{ clock(evp.roundStart) }} 开始</span>
            <span class="chip" v-if="!evp.chatIn">玩家发言没算进来</span>
            <span class="grow"></span>
            <x-hint label="怎么读" :text="'一行是一记行为，新的一记排在最上面。动作那列的颜色只分三种档：变糟的事（死亡、消散、挂机）、被人处置的事（请下线、封禁）、好转的事（被复活、回来继续）；进出和不上色的中性档保持原色。IP、Steam ID、角色这三列是从这一轮里关于这个人的连接线索凑出来的，不一定是这一行本身写的。' + evIdentityNote"></x-hint>
            <div class="pop-wrap">
              <button class="btn sm ghost" @click="colsOpen = !colsOpen" :aria-expanded="colsOpen ? 'true' : 'false'">
                <x-icon name="grid" :size="14"></x-icon>列设置</button>
              <div class="pop" v-if="colsOpen">
                <span class="pop-t">勾选的列才出现在表里（手机上这几列本来就摊在卡片里，不受这里影响）</span>
                <button class="pop-i" :class="cols[c.key] ? 'on' : ''" type="button"
                        v-for="c in allCols" :key="c.key"
                        :disabled="!c.opt" @click="toggleCol(c.key)">
                  <x-icon :name="c.opt && cols[c.key] ? 'check' : (c.opt ? 'close' : 'lock')" :size="13"></x-icon>
                  <span>{{ c.name }}</span>
                </button>
              </div>
            </div>
            <button class="btn sm ghost" @click="apply" :disabled="evLoading">
              {{ evLoading ? '读着…' : '刷新' }}</button>
          </div>

          <x-note v-if="evp.note" kind="warn" style="margin:0 12px 8px">{{ evp.note }}</x-note>

          <x-empty v-if="!evp.rows.length" icon="filter" title="这些条件在窗口里一条都没命中"
            :note="evActiveFilters
                     ? '条件还筛着：一共回看了 ' + num(evp.scanned) + ' 行，定性的行为 ' + num(evp.matched) + ' 条都没对上。把动作或关键词放开再看看。'
                     : '文件在，但这一轮的窗口里还没有玩家行为。世界刚启动时这是常态：把上面的回看行数放大，或者去看逐行日志。'">
            <button class="btn sm primary" v-if="evActiveFilters" @click="resetFilters">清掉条件</button>
            <button class="btn sm ghost" @click="ev.lines = evChoices[evChoices.length - 1]" v-else>窗口放到最大</button>
            <button class="btn sm ghost" @click="setView('log')">去看逐行日志</button>
          </x-empty>

          <template v-else>
            <!-- 桌面端：一行一记的表。九列在窄一点的窗口里会横向滚，
                 那比把每一列压到看不全一个字要好；手机上是下面的卡片。 -->
            <div class="table-wrap hide-sm" :class="density">
              <table class="data ev-table">
                <thead>
                  <tr style="white-space:nowrap">
                    <th>玩家</th>
                    <th v-if="cols.char">角色</th>
                    <th v-if="cols.userid" class="mono">账户编号</th>
                    <th v-if="cols.steamid" class="mono">Steam ID</th>
                    <th>时间</th>
                    <th v-if="cols.ip" class="mono">IP</th>
                    <th>动作</th>
                    <th v-if="cols.desc" class="wrap">说明</th>
                    <th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="r in evp.rows" :key="rowKey(r)">
                    <td>
                      <div class="row" style="gap:8px;align-items:center;min-width:0">
                        <span class="player-avatar sm" :style="avaStyle(r)" :title="avaTitle(r)">{{ avaText(r) }}</span>
                        <div class="col" style="gap:0;min-width:0">
                          <b class="ellipsis">{{ r.player || '没署名' }}</b>
                          <span class="hint" v-if="r.shard">{{ r.shard }}</span>
                        </div>
                        <x-badge kind="info" text="管理员" v-if="r.admin"></x-badge>
                      </div>
                    </td>
                    <td v-if="cols.char">
                      <span v-if="r.prefab">{{ charName(r.prefab) }}</span>
                      <span class="hint" v-else>不知道</span>
                    </td>
                    <td v-if="cols.userid" class="mono"><span class="ellipsis">{{ r.userid || '—' }}</span></td>
                    <td v-if="cols.steamid" class="mono"><span class="ellipsis">{{ r.steamid || '—' }}</span></td>
                    <td class="mono" style="white-space:nowrap">
                      {{ stamp(r) }}
                      <div class="hint" v-if="r.at">{{ dateText(r.at) }}</div>
                    </td>
                    <td v-if="cols.ip" class="mono"><span class="ellipsis">{{ r.ip || '—' }}</span></td>
                    <td>
                      <span class="chip" :class="r.tone" :title="r.label || ''">{{ r.name }}</span>
                      <div class="hint">{{ r.kindLabel }}</div>
                    </td>
                    <td v-if="cols.desc" class="wrap">
                      <div :class="'ev-raw ' + evRowCls(r)">{{ r.raw }}<span class="dim" v-if="r.cut">（这一行太长，只给了前 1000 字）</span></div>
                      <button class="linkish" v-if="isLong(r)" @click="toggleRaw(r)">
                        {{ expanded === rowKey(r) ? '收起' : '展开全文' }}</button>
                      <div class="hint" v-if="r.target && (r.raw || '').indexOf(r.target) < 0">对象：{{ r.target }}</div>
                    </td>
                    <td>
                      <div class="btn-group">
                        <button class="btn sm ghost" @click="showRaw(r)"
                                title="带着这一行的出处跳到逐行日志">看原文</button>
                        <button class="btn sm" :class="canWrite ? 'danger' : 'ghost'" v-if="canWrite"
                                :disabled="!r.userid || isBusy('evban:' + r.userid)"
                                @click="banUser(r)">拉黑</button>
                      </div>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>

            <!-- 手机上换成竖排卡片：九列摊进 390px 只会得到一条横向滚动的表，
                 而"看这一记"要的是从上往下读。 -->
            <div class="col ev-cards only-sm" style="gap:8px">
              <div class="ev-card" v-for="r in evp.rows" :key="rowKey(r)">
                <div class="row" style="gap:8px;align-items:center">
                  <span class="player-avatar sm" :style="avaStyle(r)" :title="avaTitle(r)">{{ avaText(r) }}</span>
                  <b class="ellipsis">{{ r.player || '没署名' }}</b>
                  <x-badge kind="info" text="管理员" v-if="r.admin"></x-badge>
                  <span class="grow"></span>
                  <span class="chip" :class="r.tone">{{ r.name }}</span>
                </div>
                <div class="row" style="gap:8px">
                  <span class="mono dim">{{ stamp(r) }}</span>
                  <span class="dim" v-if="r.at">{{ dateText(r.at) }}</span>
                  <span class="grow"></span>
                  <span class="dim">{{ r.kindLabel }}</span>
                </div>
                <div :class="'ev-raw ' + evRowCls(r)">{{ r.raw }}<span class="dim" v-if="r.cut">（这一行太长，只给了前 1000 字）</span></div>
                <button class="linkish" v-if="isLong(r)" @click="toggleRaw(r)">
                  {{ expanded === rowKey(r) ? '收起' : '展开全文' }}</button>
                <div class="row ev-card-id">
                  <span class="chip mono" v-if="r.userid">{{ r.userid }}</span>
                  <span class="chip mono" v-if="r.steamid">Steam {{ r.steamid }}</span>
                  <span class="chip mono" v-if="r.ip">{{ r.ip }}</span>
                  <span class="chip" v-if="r.prefab">{{ charName(r.prefab) }}</span>
                </div>
                <div class="btn-group">
                  <button class="btn sm ghost" @click="showRaw(r)">看原文</button>
                  <button class="btn sm danger" v-if="canWrite"
                          :disabled="!r.userid || isBusy('evban:' + r.userid)"
                          @click="banUser(r)">拉黑</button>
                  <span class="hint" v-else>只读账号不能改黑名单。</span>
                </div>
              </div>
            </div>

            <!-- 底部这一排说的是"这一页给了哪些、一共筛出多少"。三个数必须一起出现：
                 只有"共 4 条"会被读成"这台服务器只出过四件事"，所以带上扫过的行数。
                 这里不给"删除"：这两份文件归引擎管（每轮启动重写），面板没有删日志的接口，
                 摆一枚按下去会被拒的按钮不如不摆。 -->
            <div class="ev-foot">
              <span class="muted">第 {{ evRange }} 条 / 共 {{ num(evp.matched) }} 条
                · 涉及 {{ num(evp.people) }} 个人</span>
              <span class="hint">从 {{ num(evp.scanned) }} 行里定性出来的</span>
              <span class="grow"></span>
              <label class="ev-pagesize">每页
                <select v-model.number="ev.size">
                  <option v-for="n in sizeChoices" :key="n" :value="n">{{ n }}</option>
                </select>条</label>
              <x-pager :page="evPage" :size="Number(evp.size) || ev.size" :total="Number(evp.matched) || 0"
                       :loading="evLoading" @go="gotoPage"></x-pager>
            </div>
          </template>
        </div>
      </template>

      <!-- ---- 逐行日志 ---- -->
      <template v-else>
        <!-- 这一屏的上下文：读的是哪个文件、这一轮什么时候起的、回看了多少、给出了多少。 -->
        <div class="stream-meta" v-if="meta">
          <span :class="live ? 'dot-live' : 'dot-off'"></span>
          <span>{{ streamText }}</span>
          <span class="chip mono" v-if="meta.gameVersion">服务端 {{ meta.gameVersion }}</span>
          <span class="chip mono">{{ meta.kindLabel }}</span>
          <span class="chip">回看 {{ num(meta.scanned) }} 行 · 给出 {{ num(meta.returned) }} 行</span>
          <span class="chip" v-if="meta.blanks">空行 {{ num(meta.blanks) }} 行没算进来</span>
          <span class="chip">{{ bytes(meta.sizeBytes) }} · {{ rel(meta.modTime) }}写过</span>
          <!-- 那一轮启动时间与"引擎每次启动都重写日志"这句话只在需要时才摊开：
               这一排本来就要回答"读的是哪一份"，多两行正文会把日志挤出第一屏。 -->
          <x-hint v-if="meta.startAt" :label="'这一轮 ' + dateTime(meta.startAt).slice(11,16) + ' 起'"
                  :text="'这一轮启动于 ' + dateTime(meta.startAt) + '（' + rel(meta.startAt) + '）。文件里只有这一轮的内容——引擎每启动一次都会把这两份日志重写，所以往上翻到最早一行就是这一轮的开头。'"></x-hint>
        </div>
        <x-note kind="warn" v-if="meta && meta.startNote && !meta.startAt">{{ meta.startNote }}</x-note>
        <x-note kind="info" v-if="meta && meta.older">
          上面只回了末尾 {{ num(meta.window) }} 行；更早的内容不用上机器 grep，
          点下面「历史日志」里的「加载更早的行」按整份文件翻页。</x-note>

        <div class="row hot-events" style="gap:6px" v-if="hotEvents.length">
          <span class="card-title-sm">这一屏里的事件</span>
          <button class="chip" v-for="h in hotEvents" :key="h.code" @click="pickEvent(h.code)"
                  :class="event === h.code ? 'on' : ''"
                  :title="(h.label || '没归类的那些行') + '（再点一次取消这条筛选）'">{{ h.name }} {{ h.n }}</button>
        </div>

        <x-note kind="info" v-if="resetNote">{{ resetNote }}</x-note>
        <div class="err-box" v-if="error">{{ error }}</div>
        <x-sk v-else-if="loading && !rows.length" :lines="6"></x-sk>

        <div class="card card-flush" v-else-if="meta">
          <div class="row" style="padding:10px 12px;gap:8px;flex-wrap:wrap">
            <b style="font-size:13px">{{ meta.path }}</b>
            <span class="grow"></span>
            <x-badge kind="bad" :text="'报错 ' + num(counts.error || 0)" v-if="counts.error"></x-badge>
            <x-badge kind="warn" :text="'警告 ' + num(counts.warn || 0)" v-if="counts.warn"></x-badge>
            <x-badge kind="ok" :text="'普通 ' + num(counts.info || 0)"></x-badge>
          </div>
          <button class="btn sm ghost" style="margin:0 12px 8px" v-if="below && !stick" @click="stick = true; jump(true)">
            下面有 {{ below }} 行新的，跳到底部</button>
          <div class="logbox" :class="density" ref="box" @scroll="onScroll" style="border-top:0;border-radius:0 0 10px 10px">
            <x-empty v-if="!rows.length" icon="filter" title="这一屏里没有符合条件的行"
              :note="activeFilters ? '条件还在筛着；把级别或事件放开看看。'
                                   : '文件在，但这一轮还没写出内容。世界刚启动时这是常态。'"></x-empty>
            <template v-else>
              <div class="hint" v-if="dropped" style="padding:0 4px 6px">
                只留着最近 {{ num(KEEP_ROWS) }} 行，上面丢掉了 {{ num(dropped) }} 行——开太久就是这样，
                要查更早的内容点下面「历史日志」里的「加载更早的行」。</div>
              <div class="logline" :class="rowCls(r)" v-for="r in rows" :key="r.offset">
                <span class="ts">{{ stamp(r) }}</span>
                <span class="lv">{{ r.levelLabel || '普通' }}</span>
                <span class="body">
                  <span class="txt"><span class="who" v-if="r.eventName" :title="r.eventLabel">{{ r.eventName }}　</span><span class="who" v-if="r.player">{{ r.player }}　</span>{{ r.raw }}<span class="dim" v-if="r.cut">（这一行太长，只给了前 1000 字）</span></span> <button class="linkish row-more only-sm" v-if="isLong(r)" @click="toggleRow(r)">{{ expanded === r.offset ? '收起这一行' : '展开这一行' }}</button>
                </span>
              </div>
            </template>
          </div>
        </div>

        <!-- 历史日志：/log/history 按整份文件翻页，不再只有末尾 200/2000 行 + 一句"去服务端 grep"。 -->
        <div class="card card-flush mt">
          <div class="row" style="padding:10px 12px;gap:8px;flex-wrap:wrap;align-items:center">
            <b style="font-size:13px">历史日志（整份文件翻页）</b>
            <span class="grow"></span>
            <button class="btn sm" :disabled="histLoading || !shard" @click="histLoadMore">
              {{ histLoading ? '读着…' : '加载更早的行' }}</button>
            <button class="btn sm ghost" :disabled="histLoading || !hist || histOffset <= 0" @click="histPrev">上一页</button>
            <span class="muted" v-if="hist">第 {{ num(histFrom + 1) }}–{{ num(histTo) }} 条 / 共 {{ num(histTotal) }} 条</span>
            <button class="btn sm ghost" :disabled="histLoading || !histHasMore" @click="histNext">下一页</button>
          </div>
          <x-note kind="info" v-if="hist && hist.truncated" style="margin:0 12px 8px">
            这份文件超过单次读取上限，只查到末尾一段，上面的总数不是全量。</x-note>
          <x-note kind="info" v-if="hist && hist.note" style="margin:0 12px 8px">{{ hist.note }}</x-note>
          <div class="err-box" v-if="histError" style="margin:0 12px 8px">
            {{ histError }} <button class="linkish" @click="histReload">重试</button></div>
          <x-empty v-else-if="hist && !(hist.lines || []).length" icon="lines" title="这一页没有符合条件的行"
                   note="把级别、事件或关键词放开，或者点「加载更早的行」。"></x-empty>
          <div class="logbox" :class="density" v-else-if="hist && (hist.lines || []).length" style="max-height:360px">
            <div class="logline" :class="lv(r)" v-for="r in hist.lines" :key="r.offset">
              <span class="ts">{{ stamp(r) }}</span>
              <span class="lv">{{ r.levelLabel || '普通' }}</span>
              <span class="body"><span class="txt">{{ r.raw }}</span></span>
            </div>
          </div>
          <x-empty v-else icon="lines" title="还没翻过历史"
                   note="点上面的「加载更早的行」按整份文件读第一批；和上面的末尾窗口是两回事。"></x-empty>
        </div>
      </template>
    </div>`,
};
