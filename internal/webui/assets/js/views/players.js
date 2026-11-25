// 玩家管理：在线玩家的即时操作 + 三份游戏身份名单 + 这一窗里谁在什么时候来过。
//
// 页面把两条通道分开摆，是因为它们的生效时间完全不同：
//   - 在线操作（踢出/禁言/拉黑）走助手 MOD，点完立刻生效，只对正在回话的分片有用；
//   - 名单文件（管理员/白名单/黑名单）写的是引擎启动时读的文件，改完不代表马上生效。
// 把两者混成一个"玩家管理按钮"会让用户以为"加了管理员，游戏里已经变了"。
//
// 所有操作都按 userid 下发：玩家名可以改，改完按名字操作就会打到别人身上。
//
// 最后那张「玩家在线时间」（#125 从总览搬来）住这一页的理由和上面那句同源：花名册讲的是
// "这些人是谁"，这一张讲的是"他们各待了多久"——逐人明细本来就不该占总览那一眼的位置。
import * as api from "../api.js";
import { state, act, actMany, askConfirm, rel, isBusy } from "../store.js";
import { charName, charShort, charStyle, placeName } from "../gameinfo.js";
import { dateText, clock } from "../fmt.js";
import PlayerTimeline, { TL_WINDOWS } from "../timeline.js";

const ACTION_WORDS = {
  kick: "踢出",
  mute: "禁言",
  unmute: "解除禁言",
  ban: "拉黑",
  unban: "解除拉黑",
};

const LIST_LABEL = { admin: "管理员", whitelist: "白名单", blocklist: "黑名单" };
// 只有成对存在的在线动作给撤销；踢出没有反向接口，保持确认框说明后果、不伪造撤销。
const PAIR_UNDO = { mute: "unmute", unmute: "mute", ban: "unban" };

export default {
  name: "Players",
  components: { "x-timeline": PlayerTimeline },
  setup() {
    return { state, rel, isBusy, charName, charShort, charStyle, placeName };
  },
  data() {
    return {
      view: null,
      loading: false,
      error: "",
      addForm: { admin: "", whitelist: "", blocklist: "" },
      // 「玩家在线时间」那份逐人在场段。它是这一页自己发的第二次问数（/players 给的是
      // 此刻的名单，给不了"这一窗里谁来过几次"）。
      // hours 归这一页自己选：总览那一栏的时间窗是它自己的两张图的事，两边各选各的，
      // 档位共用 timeline.js 那一份 TL_WINDOWS。
      // seq 管"晚到的那份不许写界面"：连点两下时间窗、或者切房间和换窗口撞上时，
      // 只有最后发出去的那次能落进图里。
      timeline: { loading: false, error: "", data: null, hours: 24, seq: 0 },
      // 多选：在线玩家按 userid、花名册按 userid，两套互不串。
      selOnline: {}, selRoster: {},
      // 长表密度：.dense / .comfortable 是 app.css 已落地的钩子，这里只切类名。
      dense: false,
    };
  },
  computed: {
    cluster: {
      get() { return state.sel.cluster; },
      set(v) { state.sel.cluster = v; },
    },
    tl() { return this.timeline.data; },
    tlWindows() { return TL_WINDOWS; },
    tlPeople() { return this.tl && this.tl.people ? this.tl.people : []; },
    // 记录起点这一枚常驻（不只放在气泡里）：面板是从装上那一版起才开始把进出记进库的，
    // 不说清起点，一张只有今天下午的图会被读成"这房间一直就这么点人"。
    // 时刻走 fmt 的本地化——接口那句 note 里不带钟点，就是为了这一处不和时间打架。
    tlRecordFrom() {
      const d = this.tl;
      return d && d.recordFrom ? dateText(d.recordFrom).slice(5) + " " + clock(d.recordFrom) : "";
    },
    onlineCount() { return this.view ? this.view.online.length : 0; },
    rosterCount() { return this.view ? this.view.roster.length : 0; },
    runningCount() { return this.view ? this.view.runningShards.length : 0; },
    // 进程活着但引擎把世界暂停了（没人时会自动暂停）：这时候的 0 是"读不到"，不是"没人"。
    pausedText() {
      if (!this.view || !this.view.pausedShards.length) return "";
      return this.view.pausedShards.map(placeName).join("、");
    },
    // "没人" 与 "问不到" 是两件事，混成一句就会被当成前者：
    // 暂停中的世界读不到名单，这时候写"先把世界开起来"是让人去重启一个本来好好的进程。
    emptyOnlineText() {
      if (this.pausedText) {
        return "读不到在线名单：" + this.pausedText + " 正被引擎暂停着（没人时会自动暂停，暂停期间引擎连助手的调度一起冻住）。"
          + "有人进来自会自动恢复；这个 0 只代表这一刻问不到，不代表没人在玩。";
      }
      return this.runningCount
        ? "此刻没有玩家在线。玩家进服后会出现在这里。"
        : "此刻没有玩家在线。没有世界在回话时也读不到在线名单，先把世界开起来。";
    },
    canWrite() { return !!(state.me && state.me.isAdmin); },
    readOnlyReason() { return "这个账号是只读账号，没有修改权限（需要管理员）"; },
    selectedOnline() { return (this.view ? this.view.online : []).filter((p) => this.selOnline[p.userid]); },
    selectedRoster() { return (this.view ? this.view.roster : []).filter((e) => this.selRoster[e.userid]); },
    tableDensity() { return this.dense ? "dense" : "comfortable"; },
    lastSync() { return state.lastSync; },
  },
  watch: {
    cluster(v) { if (v) { this.load(); this.loadTimeline(); } },
    // 泳道上"这会儿还在里面"那一根条是画到查询这一刻的，不跟着轮询刷新就等于
    // 把五分钟前的人当成现在的人，而刚进服的人会一直不出现（总览那张图同一条规矩）。
    // 走 silent：这一页每五秒闪一次骨架比不刷新还烦。
    lastSync() { if (this.cluster) this.loadTimeline(null, true); },
  },
  mounted() {
    if (this.cluster) { this.load(); this.loadTimeline(); }
  },
  methods: {
    path(suffix) {
      return "/api/v1/clusters/" + encodeURIComponent(this.cluster) + suffix;
    },
    // 时间线读的是全局那条 /player-timeline（带 cluster 参数），不是本页其余请求那种
    // /clusters/名字/... 的挂法：它要跨这一间房的两个世界归并段与次数，接口本来就是房间级的。
    async loadTimeline(cluster, silent) {
      const want = cluster || this.cluster;
      if (!want) { this.timeline.data = null; this.timeline.error = ""; return; }
      const seq = (this.timeline.seq = (this.timeline.seq || 0) + 1);
      if (!silent) this.timeline.loading = true;
      const res = await api.get("/api/v1/player-timeline" +
        api.qs({ cluster: want, hours: this.timeline.hours }));
      this.timeline.loading = false;
      if (seq !== this.timeline.seq) return;
      if (res.ok) {
        this.timeline.data = res.data || null;
        this.timeline.error = "";
      } else { this.timeline.data = null; this.timeline.error = res.error || ("接口返回 " + res.status); }
    },
    setHours(h) {
      const n = Number(h);
      if (!Number.isFinite(n) || n === this.timeline.hours) return;
      this.timeline.hours = n;
      // 换窗口只重问时间线这一份：上面那排名单是"此刻"的，和时间窗无关。
      this.loadTimeline();
    },
    refresh() { this.load(); this.loadTimeline(); },
    async load() {
      if (!this.cluster) { this.view = null; return; }
      this.loading = true;
      this.error = "";
      const want = this.cluster;
      const res = await api.get(this.path("/players"));
      // 请求飞在路上时用户可能切了房间：旧房间的响应不能盖到新房间的页面上。
      if (want !== this.cluster) return;
      this.loading = false;
      if (!res.ok) { this.error = res.error || ("接口返回 " + res.status); return; }
      this.apply(res.data);
    },
    apply(v) {
      if (v && v.cluster === this.cluster) this.view = v;
    },
    fileName(p) {
      return String(p || "").split(/[\\/]/).pop();
    },
    // 管理员是"一份名单"，白名单/黑名单本身就是名单名，
    // 统一拼上"名单"会出现"白名单名单"这种话。
    listLabel(kind) {
      if (kind === "admin") return "管理员名单";
      return LIST_LABEL[kind] || kind;
    },
    listHint(kind, add) {
      if (!add) return "名单已更新；引擎下次读这份文件时生效。";
      if (kind === "admin") return "写进了 adminlist.txt；玩家重新连接后生效，还不生效就重启世界。";
      if (kind === "whitelist") return "写进了 whitelist.txt；要重启世界才会被引擎读到。";
      return "写进了 blocklist.txt；要重启世界才会被引擎读到。在线玩家要立刻断开，用上面的「拉黑」。";
    },
    async toggleList(kind, userid, add) {
      const label = this.listLabel(kind);
      if (add && kind === "blocklist") {
        const ok = await askConfirm({
          title: "把 " + userid + " 加进黑名单？",
          body: "会写进集群的 blocklist.txt，引擎启动时读这份名单。",
          warn: "这不是立刻断开：在线玩家要等世界重启后才被拦下。要马上断开，请在该玩家的在线卡片里点「拉黑」。",
          danger: true,
          okLabel: "加进黑名单",
        });
        if (!ok) return false;
      }
      const key = "list:" + kind + ":" + userid + ":" + (add ? "add" : "remove");
      const res = await act(key, "POST", this.path("/players/lists/" + kind), {
        action: add ? "add" : "remove", userid,
      }, {
        clusters: false, after: false,
        okTitle: (add ? "已加入" : "已移出") + label,
        okBody: () => this.listHint(kind, add),
        badTitle: label + "没改成",
        // 名单项可反向：加进去的能移出、移出的能加回，给一条短时撤销。
        undo: {
          label: (add ? "撤销「加入" : "撤销「移出") + label + "」",
          method: "POST", path: this.path("/players/lists/" + kind),
          body: { action: add ? "remove" : "add", userid }, seconds: 12,
        },
      });
      if (res.ok) this.apply(res.data);
      return res.ok;
    },
    async addFromList(g) {
      const id = String(this.addForm[g.key] || "").trim();
      if (!id) return;
      const ok = await this.toggleList(g.key, id, true);
      if (ok) this.addForm[g.key] = "";
    },
    async playerAction(p, action) {
      const who = p.name || p.userid;
      if (action === "kick") {
        const ok = await askConfirm({
          title: "把 " + who + " 踢出服务器？",
          body: "由游戏服务端的助手 MOD 执行，这个角色会立刻掉线。",
          warn: "他还能马上重连；要阻止回来，请用「拉黑」。",
          okLabel: "踢出",
        });
        if (!ok) return;
      }
      if (action === "ban") {
        const ok = await askConfirm({
          title: "把 " + who + " 拉黑？",
          body: "会让游戏服务端把这个 userid 写进黑名单，并立刻断开他。",
          warn: "拉黑按 userid 生效，改名也绕不过去；要放他回来得先「解除拉黑」。",
          danger: true,
          okLabel: "拉黑",
        });
        if (!ok) return;
      }
      const res = await act("pa:" + action + ":" + p.userid, "POST",
        this.path("/players/" + encodeURIComponent(p.userid) + "/" + action),
        { confirm: action === "kick" || action === "ban" }, {
          clusters: false, after: false,
          okTitle: "已执行「" + ACTION_WORDS[action] + "」",
          okBody: (d) => (d && d.note) ? d.note
            : "游戏侧已经回执。禁言/解禁的状态在游戏内聊天框里能看到。",
          badTitle: ACTION_WORDS[action] + "没成",
          // 禁言↔解禁、拉黑↔解除拉黑可反向；踢出没有反向接口，不给假撤销。
          undo: PAIR_UNDO[action] ? {
            label: "撤销「" + ACTION_WORDS[action] + "」",
            method: "POST",
            path: this.path("/players/" + encodeURIComponent(p.userid) + "/" + PAIR_UNDO[action]),
            body: {}, seconds: 12,
          } : undefined,
        });
      if (res.ok) await this.load();
    },
    vitalKind(v, max) {
      if (v === null || v === undefined) return "";
      if (max) return Number(v) / Number(max) <= 0.3 ? "bad" : "ok";
      return Number(v) <= 30 ? "bad" : "ok";
    },
    coord(p) {
      if (p.x === null || p.x === undefined || p.z === null || p.z === undefined) return "";
      return Math.round(p.x) + ", " + Math.round(p.z);
    },
    toggleOnline(p) { this.selOnline = { ...this.selOnline, [p.userid]: !this.selOnline[p.userid] }; },
    toggleRoster(e) { this.selRoster = { ...this.selRoster, [e.userid]: !this.selRoster[e.userid] }; },
    selectAllOnline() {
      const all = {};
      (this.view ? this.view.online : []).forEach((p) => { all[p.userid] = true; });
      this.selOnline = all;
    },
    selectAllRoster() {
      const all = {};
      (this.view ? this.view.roster : []).forEach((e) => { all[e.userid] = true; });
      this.selRoster = all;
    },
    clearSel() { this.selOnline = {}; this.selRoster = {}; },
    // 批量踢出：不可逆，确认框写清后果；整批一个 busy 键，actMany 串行下发并汇总。
    async bulkKick() {
      const list = this.selectedOnline;
      if (!list.length || !this.canWrite) return;
      const ok = await askConfirm({
        title: "把选中的 " + list.length + " 人踢出服务器？",
        body: "由游戏服务端的助手 MOD 执行，这些人会立刻掉线。按 userid 下发：" +
          list.map((p) => p.name || p.userid).join("、") + "。",
        warn: "他们还能马上重连；要拦住回来请用拉黑。踢出没有反向操作，界面不会给假的撤销。",
        danger: true, okLabel: "踢出这 " + list.length + " 人",
      });
      if (!ok) return;
      const items = list.map((p) => ({
        method: "POST", path: this.path("/players/" + encodeURIComponent(p.userid) + "/kick"),
        body: { confirm: true }, label: p.name || p.userid,
      }));
      const res = await actMany("players.bulk.kick", items,
        { okTitle: "批量踢出完成", badTitle: "批量踢出有没成的", clusters: false, after: false });
      if (res.ok) this.selOnline = {};
      await this.load();
    },
    // 批量加/移名单：黑名单是破坏性的，沿用单个操作的确认说明；其余直接整批下发。
    async bulkList(kind, add) {
      const list = this.selectedRoster;
      if (!list.length || !this.canWrite) return;
      if (add && kind === "blocklist") {
        const ok = await askConfirm({
          title: "把选中的 " + list.length + " 人加进黑名单？",
          body: "会写进集群的 blocklist.txt，引擎启动时读这份名单。",
          warn: "这不是立刻断开：在线玩家要等世界重启后才被拦下。要马上断开，请在该玩家的在线卡片里点「拉黑」。",
          danger: true, okLabel: "加进黑名单",
        });
        if (!ok) return;
      }
      const items = list.map((e) => ({
        method: "POST", path: this.path("/players/lists/" + kind),
        body: { action: add ? "add" : "remove", userid: e.userid }, label: e.name || e.userid,
      }));
      const res = await actMany("players.bulk.list", items,
        { okTitle: "批量" + (add ? "加入" : "移出") + "名单完成", badTitle: "批量改名单有没成的", clusters: false, after: false });
      if (res.ok) this.selRoster = {};
      await this.load();
    },
  },
  template: `
    <div class="col players-page" style="gap:14px">
      <!-- 一个房间都没有时不摆那个空下拉框：它点开还是空的，等于让用户去操作一个
           没有东西可选的控件。整行换成一句"这一页要什么、去哪一步"。 -->
      <div class="row" style="gap:12px;flex-wrap:wrap;align-items:flex-end" v-if="state.clusters.length">
        <div class="field" style="min-width:190px;flex:1 1 220px">
          <label for="players-cluster">当前房间</label>
          <select id="players-cluster" v-model="cluster">
            <option value="" disabled>选一个房间</option>
            <option v-for="c in state.clusters" :key="c.name" :value="c.name">{{ c.name }}</option>
          </select>
        </div>
        <span class="grow"></span>
        <x-hint v-if="view" label="这一页怎么生效" :text="view.note"></x-hint>
        <button class="btn ghost sm" @click="refresh" :disabled="loading || !cluster">
          {{ loading ? '读取中' : '刷新' }}</button>
      </div>

      <x-empty v-else icon="users" title="这台机器上还没有房间"
        note="玩家要在世界里才会出现：先建一个房间并把世界开起来，这一页才有名单可管。">
        <a class="btn sm primary" href="#/room">去开服向导</a>
      </x-empty>

      <x-note v-if="view && !canWrite" kind="warn">
        当前账号是只读：能看名单，不能踢人、禁言或改名单。
      </x-note>
      <x-note v-if="error" kind="bad">{{ error }}</x-note>
      <x-sk v-if="loading && !view" :lines="5"></x-sk>

      <template v-if="view && !error">
        <div class="grid players-stats">
          <x-stat label="此刻在线" icon="users" tint="tint-green" :value="onlineCount"
                  :hint="onlineCount ? '人数取自正在回话的世界，每五秒跟总览一起刷新一次。'
                    : pausedText ? pausedText + ' 正被引擎暂停着（没人时会自动暂停），暂停期间引擎连助手的调度一起冻住，所以读不到名单——这个 0 是读不到，不是没人在玩。'
                    : '没有世界在回话，读不到在线名单；先把世界开起来。'">
            {{ onlineCount ? '来自正在回话的世界' : (pausedText ? '暂停中的世界读不到' : '还没有世界在回话') }}
          </x-stat>
          <x-stat label="回话世界" icon="signal" tint="tint-cyan" :value="runningCount"
                  hint="禁言、踢出、拉黑这类在线操作只有世界活着并装了助手 MOD 才做得了，这一栏说的就是那几个。">
            <span class="mono ellipsis">{{ view.runningShards.length ? view.runningShards.join(' · ') : '在线操作暂时不可用' }}</span>
          </x-stat>
          <x-stat label="花名册" icon="idcard" tint="tint-violet" :value="rosterCount"
                  hint="面板见过的 userid 都会留在库里：正在线上的、三份名单里的、以及以前进过服现在已经离开的。">
            见过的、名单里的、正在线上的
          </x-stat>
        </div>

        <section class="card">
          <div class="card-head">
            <span class="ico-tile tint-green"><x-icon name="users" :size="15"></x-icon></span>
            <h3>在线玩家</h3>
            <x-badge :kind="onlineCount ? 'ok' : (pausedText ? 'warn' : 'idle')"
                     :text="onlineCount ? onlineCount + ' 人在线' : (pausedText ? '暂停中，读不到' : '没人在线')"></x-badge>
            <span class="grow"></span>
            <x-hint label="命令发给谁"
                    text="踢出和禁言只对在线的角色有效，命令按 userid 下发，不认名字。"></x-hint>
          </div>

          <!-- 多选批量踢出：勾中的人一直显示在「已选 N 人」这枚 chip 里，整批期间按钮禁用。 -->
          <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap" v-if="onlineCount">
            <button class="btn sm ghost" :disabled="!canWrite" :title="canWrite ? '' : readOnlyReason"
                    @click="selectAllOnline">全选在线</button>
            <span class="chip" v-if="selectedOnline.length">已选 {{ selectedOnline.length }} 人</span>
            <span class="grow"></span>
            <button class="btn sm danger" :disabled="!canWrite || !selectedOnline.length || isBusy('players.bulk.kick')"
                    :title="canWrite ? '整批执行期间按钮会一直禁用，结果给一条汇总' : readOnlyReason"
                    @click="bulkKick">{{ isBusy('players.bulk.kick') ? '批量踢出中……' : '批量踢出' }}</button>
            <button class="btn sm ghost" v-if="selectedOnline.length" @click="selOnline = {}">清空选择</button>
          </div>

          <div class="hint" v-if="!onlineCount">{{ emptyOnlineText }}</div>

          <div class="players-grid" v-else>
            <div class="player-card" v-for="p in view.online" :key="p.userid"
                 :style="selOnline[p.userid] ? 'box-shadow:0 0 0 1px var(--warn) inset' : ''">
              <div class="row" style="gap:9px;align-items:center">
                <input type="checkbox" style="flex:none" :checked="!!selOnline[p.userid]" :disabled="!canWrite"
                       :aria-label="'选中 ' + (p.name || p.userid)" @change="toggleOnline(p)">
                <span class="player-avatar" :style="charStyle(p.prefab)">{{ charShort(p.prefab) }}</span>
                <div class="col" style="gap:1px;min-width:0">
                  <b class="ellipsis">{{ p.name || '名字还没读到' }}</b>
                  <span class="hint mono ellipsis">{{ p.userid }}</span>
                </div>
                <span class="grow"></span>
                <div class="col" style="gap:3px;align-items:flex-end">
                  <x-badge :kind="p.dead ? 'bad' : 'ok'" :text="p.dead ? '已倒地' : '在线'"></x-badge>
                  <span class="hint">{{ placeName(p.shard) }}</span>
                </div>
              </div>

              <div class="row" style="gap:5px;flex-wrap:wrap">
                <span class="chip">{{ charName(p.prefab) }}</span>
                <span class="chip" v-if="coord(p)" :title="'角色所在坐标'">{{ placeName(p.shard) }} {{ coord(p) }}</span>
              </div>

              <div class="player-bars" v-if="p.health != null || p.hunger != null || p.sanity != null">
                <x-bar v-if="p.health != null" label="生命" :value="p.health" :max="p.maxHealth"
                       :kind="vitalKind(p.health, p.maxHealth)"></x-bar>
                <x-bar v-if="p.hunger != null" label="饥饿" :value="p.hunger" :max="p.maxHunger"
                       :kind="vitalKind(p.hunger, p.maxHunger)"></x-bar>
                <x-bar v-if="p.sanity != null" label="理智" :value="p.sanity" :max="p.maxSanity"
                       :kind="vitalKind(p.sanity, p.maxSanity)"></x-bar>
              </div>

              <div class="btn-group" style="margin-top:2px">
                <button class="btn sm ghost" v-if="!p.dead" :disabled="!canWrite || isBusy('pa:mute:' + p.userid)"
                        :title="canWrite ? '' : readOnlyReason" @click="playerAction(p, 'mute')">禁言</button>
                <button class="btn sm ghost" :disabled="!canWrite || isBusy('pa:unmute:' + p.userid)"
                        :title="canWrite ? '' : readOnlyReason" @click="playerAction(p, 'unmute')">解除禁言</button>
                <span class="grow"></span>
                <button class="btn sm" :disabled="!canWrite || isBusy('pa:kick:' + p.userid)"
                        :title="canWrite ? '' : readOnlyReason" @click="playerAction(p, 'kick')">踢出</button>
                <button class="btn sm danger" :disabled="!canWrite || isBusy('pa:ban:' + p.userid)"
                        :title="canWrite ? '' : readOnlyReason" @click="playerAction(p, 'ban')">拉黑</button>
              </div>
            </div>
          </div>
        </section>

        <div class="players-lists">
          <section class="card" v-for="g in view.lists" :key="g.key">
            <div class="card-head">
              <span class="ico-tile tint-slate"><x-icon name="list" :size="15"></x-icon></span>
              <h3>{{ listLabel(g.key) }}</h3>
              <x-badge kind="idle" :text="g.entries.length + ' 人'"></x-badge>
              <!-- "什么时候生效"是这三张卡最要紧的一句，但三份各摊两行就把名单挤到第二屏了：
                   收进带字的把手，点开的还是接口给的原文。 -->
              <x-hint label="何时生效" :text="g.effect"></x-hint>
              <span class="grow"></span>
              <span class="chip mono" :title="g.file">{{ fileName(g.file) }}</span>
            </div>

            <div class="list-plain" v-if="g.entries.length">
              <div class="player-list-row" v-for="e in g.entries" :key="e.userid">
                <div class="col" style="gap:0;min-width:0">
                  <b class="ellipsis">{{ e.name || '还没见过上线' }}</b>
                  <span class="hint mono ellipsis">{{ e.userid }}</span>
                </div>
                <span class="grow"></span>
                <x-badge v-if="e.online" kind="ok" text="在线"></x-badge>
                <span class="hint" v-else-if="e.lastSeenAt">{{ rel(e.lastSeenAt) }}来过</span>
                <button class="btn sm ghost"
                        :disabled="!canWrite || isBusy('list:' + g.key + ':' + e.userid + ':remove')"
                        :title="canWrite ? '' : readOnlyReason"
                        @click="toggleList(g.key, e.userid, false)">移出</button>
              </div>
            </div>
            <div class="hint" v-else>还没有人进这份名单。</div>

            <div class="row" style="gap:8px;margin-top:10px;flex-wrap:wrap">
              <input type="text" v-model.trim="addForm[g.key]" :placeholder="'userid，例如 KU_ 开头或 Steam 数字 ID'"
                     :aria-label="'加入' + listLabel(g.key) + '的 userid'" style="flex:1 1 190px"
                     :disabled="!canWrite" :title="canWrite ? '' : readOnlyReason"
                     @keyup.enter="addFromList(g)">
              <button class="btn sm" :disabled="!canWrite || !addForm[g.key] || isBusy('list:' + g.key + ':' + addForm[g.key] + ':add')"
                      :title="canWrite ? '' : readOnlyReason" @click="addFromList(g)">加入{{ listLabel(g.key) }}</button>
            </div>
          </section>
        </div>

        <section class="card">
          <div class="card-head">
            <span class="ico-tile tint-violet"><x-icon name="idcard" :size="15"></x-icon></span>
            <h3>玩家花名册</h3>
            <x-badge kind="idle" :text="rosterCount + ' 人'"></x-badge>
            <span class="grow"></span>
            <x-hint icon="info"
                    text="在线的人排在最前；名单里的和历史见过的都在这里。"></x-hint>
            <button class="btn sm ghost" @click="dense = !dense"
                    :title="dense ? '切成宽松行距' : '切成紧凑行距'">{{ dense ? '紧凑' : '宽松' }}</button>
          </div>

          <!-- 花名册多选：勾中的人批量加入/移出名单，整批期间按钮禁用，结果一条汇总。 -->
          <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap" v-if="view.roster.length">
            <button class="btn sm ghost" :disabled="!canWrite" :title="canWrite ? '' : readOnlyReason"
                    @click="selectAllRoster">全选花名册</button>
            <span class="chip" v-if="selectedRoster.length">已选 {{ selectedRoster.length }} 人</span>
            <span class="grow"></span>
            <button class="btn sm" :disabled="!canWrite || !selectedRoster.length || isBusy('players.bulk.list')"
                    :title="canWrite ? '' : readOnlyReason" @click="bulkList('admin', true)">批量设管理员</button>
            <button class="btn sm" :disabled="!canWrite || !selectedRoster.length || isBusy('players.bulk.list')"
                    :title="canWrite ? '' : readOnlyReason" @click="bulkList('whitelist', true)">批量加白名单</button>
            <button class="btn sm danger" :disabled="!canWrite || !selectedRoster.length || isBusy('players.bulk.list')"
                    :title="canWrite ? '' : readOnlyReason" @click="bulkList('blocklist', true)">批量拉黑</button>
            <button class="btn sm ghost" :disabled="!canWrite || !selectedRoster.length || isBusy('players.bulk.list')"
                    :title="canWrite ? '' : readOnlyReason" @click="bulkList('blocklist', false)">批量解除拉黑</button>
            <button class="btn sm ghost" v-if="selectedRoster.length" @click="selRoster = {}">清空选择</button>
          </div>

          <div class="table-wrap hide-sm" :class="tableDensity" v-if="view.roster.length">
            <table class="data">
              <thead>
                <tr><th style="width:30px"></th><th>玩家</th><th>现在</th><th>身份</th><th>最近</th><th>名单操作</th></tr>
              </thead>
              <tbody>
                <tr v-for="e in view.roster" :key="e.userid">
                  <td><input type="checkbox" :checked="!!selRoster[e.userid]" :disabled="!canWrite"
                             :aria-label="'选中 ' + (e.name || e.userid)" @change="toggleRoster(e)"></td>
                  <td>
                    <div class="row" style="gap:8px;align-items:center">
                      <span class="player-avatar sm" :style="charStyle(e.prefab)">{{ charShort(e.prefab) }}</span>
                      <div class="col" style="gap:0;min-width:0">
                        <b>{{ e.name || '还没见过上线' }}</b>
                        <span class="hint mono">{{ e.userid }}</span>
                      </div>
                    </div>
                  </td>
                  <td>
                    <x-badge v-if="e.online" kind="ok" text="在线"></x-badge>
                    <x-badge v-else kind="idle" text="离线"></x-badge>
                    <div class="hint" v-if="e.online">{{ placeName(e.lastShard) }}</div>
                  </td>
                  <td>
                    <div class="row" style="gap:4px;flex-wrap:wrap">
                      <x-badge v-if="e.admin" kind="info" text="管理员"></x-badge>
                      <x-badge v-if="e.whitelisted" kind="ok" text="白名单"></x-badge>
                      <x-badge v-if="e.blocked" kind="bad" text="黑名单"></x-badge>
                      <span class="hint" v-if="!e.admin && !e.whitelisted && !e.blocked">普通玩家</span>
                    </div>
                  </td>
                  <td>
                    <span v-if="e.lastSeenAt">{{ rel(e.lastSeenAt) }}</span>
                    <span class="hint" v-else>还没上过线</span>
                  </td>
                  <td>
                    <div class="btn-group">
                      <button class="btn sm ghost" :disabled="!canWrite || isBusy('list:admin:' + e.userid + ':remove') || isBusy('list:admin:' + e.userid + ':add')"
                              :title="canWrite ? '' : readOnlyReason" @click="toggleList('admin', e.userid, !e.admin)">{{ e.admin ? '撤销管理员' : '设为管理员' }}</button>
                      <button class="btn sm ghost" :disabled="!canWrite || isBusy('list:whitelist:' + e.userid + ':remove') || isBusy('list:whitelist:' + e.userid + ':add')"
                              :title="canWrite ? '' : readOnlyReason" @click="toggleList('whitelist', e.userid, !e.whitelisted)">{{ e.whitelisted ? '移出白名单' : '加入白名单' }}</button>
                      <button class="btn sm" :class="e.blocked ? '' : 'danger'"
                              :disabled="!canWrite || isBusy('list:blocklist:' + e.userid + ':remove') || isBusy('list:blocklist:' + e.userid + ':add')"
                              :title="canWrite ? '' : readOnlyReason" @click="toggleList('blocklist', e.userid, !e.blocked)">{{ e.blocked ? '解除拉黑' : '拉黑' }}</button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div class="col only-sm players-roster-cards" style="gap:8px" v-if="view.roster.length">
            <div class="mod-row" v-for="e in view.roster" :key="e.userid">
              <div class="col mod-main">
                <div class="row" style="gap:8px;align-items:center">
                  <input type="checkbox" style="flex:none" :checked="!!selRoster[e.userid]" :disabled="!canWrite"
                         :aria-label="'选中 ' + (e.name || e.userid)" @change="toggleRoster(e)">
                  <span class="player-avatar sm" :style="charStyle(e.prefab)">{{ charShort(e.prefab) }}</span>
                  <b class="ellipsis">{{ e.name || '还没见过上线' }}</b>
                  <x-badge v-if="e.online" kind="ok" text="在线"></x-badge>
                  <x-badge v-else kind="idle" text="离线"></x-badge>
                  <span class="grow"></span>
                </div>
                <span class="hint mono ellipsis">{{ e.userid }}</span>
                <div class="row" style="gap:4px;flex-wrap:wrap">
                  <x-badge v-if="e.admin" kind="info" text="管理员"></x-badge>
                  <x-badge v-if="e.whitelisted" kind="ok" text="白名单"></x-badge>
                  <x-badge v-if="e.blocked" kind="bad" text="黑名单"></x-badge>
                  <span class="hint" v-if="e.online">{{ placeName(e.lastShard) }}</span>
                  <span class="hint" v-else-if="e.lastSeenAt">{{ rel(e.lastSeenAt) }}来过</span>
                </div>
                <div class="btn-group">
                  <button class="btn sm ghost" :disabled="!canWrite" :title="canWrite ? '' : readOnlyReason"
                          @click="toggleList('admin', e.userid, !e.admin)">{{ e.admin ? '撤销管理员' : '设为管理员' }}</button>
                  <button class="btn sm ghost" :disabled="!canWrite" :title="canWrite ? '' : readOnlyReason"
                          @click="toggleList('whitelist', e.userid, !e.whitelisted)">{{ e.whitelisted ? '移出白名单' : '加入白名单' }}</button>
                  <button class="btn sm" :class="e.blocked ? '' : 'danger'" :disabled="!canWrite"
                          :title="canWrite ? '' : readOnlyReason" @click="toggleList('blocklist', e.userid, !e.blocked)">{{ e.blocked ? '解除拉黑' : '拉黑' }}</button>
                </div>
              </div>
            </div>
          </div>

          <div class="hint" v-if="!view.roster.length">
            花名册还是空的：助手 MOD 会在采样时把见过的玩家记下来，
            也可以先用上面的名单把人加进去。
          </div>
        </section>

        <!-- 玩家在线时间（#125 从总览搬过来）：上面那张花名册回答"这些人是谁"，这一张回答
             "他们这一窗里什么时候来、各待了多久"。搬来这一页还顺带解决了宽度——
             总览那一档它是横跨整幅才看得清的，这一页整幅只放一列，轨道天然够长。 -->
        <section class="card">
          <div class="card-head">
            <span class="ico-tile sm tint-cyan"><x-icon name="history" :size="15"></x-icon></span>
            <h3>玩家在线时间</h3>
            <x-hint label="这一张怎么读" :text="(tl && tl.note) || ''"></x-hint>
            <span class="grow"></span>
            <span class="chip mono" v-if="tl"
                  title="这一窗里认出的次数，不是库里的行数：同一条公告两个世界各写一遍的算一次，跟图上的段和记号数得上"
            >{{ tlPeople.length }} 人 / {{ tl.rows }} 次行为</span>
          </div>

          <div class="row tl-when">
            <x-seg :options="tlWindows" :model-value="timeline.hours"
                   label="这一窗多长" @update="setHours"></x-seg>
            <!-- 起点这一枚常驻，而且时刻按浏览器所在时区格式化：接口那句 note 里故意不带钟点，
                 就是因为整句拼好的 UTC 时刻会和同一屏的本地钟差一截。 -->
            <span class="chip mono" v-if="tlRecordFrom"
                  title="这个房间在库里最早的一条玩家行为。面板是从装上这一版起才开始记进出的，比它更早的历史真的不存在。"
            >记录从 {{ tlRecordFrom }} 起</span>
            <span class="chip bad" v-if="tl && tl.truncated"
                  title="这一窗读到的行为行数到了上限，图里的人不全——换一个短一点的时间窗"
            >行数到上限，人没算全</span>
          </div>

          <x-sk v-if="timeline.loading && !tl" :lines="2"></x-sk>
          <div class="err-box" v-else-if="timeline.error">{{ timeline.error }}</div>
          <x-timeline v-else-if="tlPeople.length" :data="tl"></x-timeline>
          <div class="empty" v-else>
            <div class="e-title">暂无进出记录</div>
          </div>
        </section>
      </template>
    </div>`,
};
