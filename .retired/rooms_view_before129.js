// 所有房间：这台机器上跑着的每一个饥荒服务器，一行一个分片。
//
// 这一页回答的是总览页答不了的那个问题。总览页是"我现在选的这个房间怎么样"，
// 而开服以后常有这种情况：机器上有三个房间，用户想知道"哪个还开着、哪个没人、
// 哪一个忘了配令牌"，以前得逐个切下拉框看六遍。这里一次摆完。
//
// 三个约定：
//   - 一行 = 一个分片，不是一个房间。地表和洞穴各有进程、各有端口、各有天数，
//     合成一行就没法说清"停的是哪一个"。
//   - 筛选、搜索、分页都在浏览器里做。数据量是"几个进程"，服务端分页只会让
//     刷新变慢、把搜索变成跨页搜索这种伪需求。
//     代价是"筛出来的结果"只覆盖已经取回的那一份——所以每次筛选前不发请求，
//     而每次进入页面和每 20 秒会真的重取一次。
//   - 读不到的字段留空，不补 0。模组数显示"—"是对的，显示 0 会让人以为模组被清空了。
import * as api from "../api.js";
import { state, rel, roomWritable, readOnlyNote, canAssignOwner } from "../store.js";
import { dateTime, num } from "../fmt.js";
import { placeName } from "../gameinfo.js";
import { openConnect } from "../connect.js";
import { openOwner } from "../owner.js";

// 20 秒和总览页的轮询同量级：房间配置（端口、上限、预设）一天改不了几次，
// 再快只是多盘一次磁盘。
const AUTO_REFRESH = 20 * 1000;

const STATUS = [
  { value: "", label: "全部" },
  { value: "running", label: "在跑" },
  { value: "stopped", label: "没在跑" },
  { value: "attention", label: "要注意" },
];

const SORTS = [
  { value: "room", label: "按房间" },
  { value: "attention", label: "要先处理的在前" },
  { value: "online", label: "在线人数多的在前" },
];

// 归属筛选的两套选项。
//
// 为什么按角色换：成员的房间名单里本来就只有「我名下的 + 未指派的」（后端裁好的），
// 给他一个「归别人管」等于送他一个永远为空的选项；反过来，管理员看不见「我名下」这一档
// ——他的房间就是全部房间，那一筛每次都是空表。
const OWNER_ALL = { value: "", label: "不限归属" };
const OWNER_UNOWNED = { value: "unowned", label: "未指派" };
const OWNER_MINE = { value: "mine", label: "我名下" };
const OWNER_ASSIGNED = { value: "assigned", label: "已指派" };

export default {
  name: "Rooms",
  props: { params: Array, query: Object },
  setup() { return { state, rel, dateTime, num, openOwner }; },
  data() {
    return {
      reply: null, loading: false, loaded: false, error: "",
      q: "", status: "", cluster: "", sort: "room", owner: "",
      page: 1, size: 12,
      timer: null,
    };
  },
  computed: {
    rows() { return (this.reply && this.reply.items) || []; },
    // 在跑但这一刻读不到实时状态的世界数。顶部那个「在线玩家」要用它：
    // 暂停中的世界不计入在线数，于是合计是 0，而 0 会被读成"没人在玩"——
    // 这正是用户在云机上指着屏幕问"这合理吗"的那一件事。
    onlineBlind() { return this.rows.filter((r) => r.running && !r.worldReady).length; },
    statusOptions() { return STATUS; },
    sortOptions() { return SORTS; },
    clusterOptions() {
      const seen = {};
      this.rows.forEach((r) => { if (!seen[r.cluster]) seen[r.cluster] = true; });
      return Object.keys(seen).sort();
    },
    canAssign() { return canAssignOwner(); },
    isMemberAccount() { return !!state.me && state.me.role === "member"; },
    ownerOptions() {
      if (this.isMemberAccount) return [OWNER_ALL, OWNER_MINE, OWNER_UNOWNED];
      return [OWNER_ALL, OWNER_UNOWNED, OWNER_ASSIGNED];
    },
    // 一间房的两行（地表、洞穴）带着同一个归属，所以只在"这一屏里这间房的第一行"上写它。
    // 判据取当前这一页而不是整张表：换了排序或翻了页，同一间房哪一行在前会变，
    // 按整张表算的话第二页可能一行归属都不带，看着像那间房没人管了。
    ownerFirst() {
      const seen = {};
      const out = {};
      this.paged.forEach((r) => {
        if (seen[r.cluster]) return;
        seen[r.cluster] = true;
        out[r.cluster + "/" + r.shard] = true;
      });
      return out;
    },
    // 先筛再排：顺序影响的是"哪几条在第一屏"，而筛选决定的是"哪些条目存在"。
    visible() {
      const q = this.q.trim().toLowerCase();
      const out = this.rows.filter((r) => {
        if (this.cluster && r.cluster !== this.cluster) return false;
        if (this.status === "running" && !r.running) return false;
        if (this.status === "stopped" && r.running) return false;
        if (this.status === "attention" && !r.alert) return false;
        if (this.owner === "unowned" && r.ownerAccountId) return false;
        if (this.owner === "assigned" && !r.ownerAccountId) return false;
        if (this.owner === "mine" && !r.ownedByMe) return false;
        if (!q) return true;
        // 搜房间名、目录名、分片名和端口：这五样是用户在别的页面上能看到的名字，
        // 他拿这些来这一页找东西，找不到就该说找不到，而不是假装没有这个房间。
        return [r.roomName, r.cluster, r.shardName, r.shard, String(r.port || "")]
          .some((v) => String(v || "").toLowerCase().indexOf(q) >= 0);
      });
      if (this.sort === "attention") {
        // 有话说的一律在前，同组内保持房间顺序：这一列的意义是"先看哪条"，
        // 把 alert 文案排字序只会让同一个房间的两条散落两处。
        out.sort((a, b) => (b.alert ? 1 : 0) - (a.alert ? 1 : 0));
      } else if (this.sort === "online") {
        out.sort((a, b) => (b.online || 0) - (a.online || 0));
      }
      return out;
    },
    paged() {
      const start = (this.page - 1) * this.size;
      return this.visible.slice(start, start + this.size);
    },
    filteredOut() { return this.rows.length - this.visible.length; },
  },
  watch: {
    // 顶栏"助手没回话 2"点进来时地址里带着筛选意图，落地就要只列那两条。
    // 已经在这一页时再点一次不会重新 mounted，所以这里也得听 query。
    query(q) { if (q && q.focus === "attention") this.status = "attention"; },
    // 换了筛选条件还停在第 3 页是最容易出现的空结果：筛选本身就可能把
    // 剩下的条目数压到不足三页，界面只会显示"没有符合的"。
    q() { this.page = 1; },
    status() { this.page = 1; },
    cluster() { this.page = 1; },
    sort() { this.page = 1; },
    owner() { this.page = 1; },
  },
  mounted() {
    const q = this.query || {};
    if (q.focus === "attention") this.status = "attention";
    this.load();
    this.timer = setInterval(() => {
      // 切走的标签页不 polling：用户在别的页时这一页的数据没人看，
      // 而每次重取都要读十几个文件。
      if (!document.hidden) this.load(true);
    }, AUTO_REFRESH);
  },
  unmounted() {
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
  },
  methods: {
    async load(quiet) {
      if (this.loading) return;
      if (!quiet) this.loading = true;
      this.error = "";
      const res = await api.get("/api/v1/rooms");
      this.loading = false;
      if (!res.ok) {
        // 安静刷新失败时不清空已有表格：一次读盘抖动不该把用户正在看的那一屏抹掉，
        // 但必须在界面上留下"这是上一次的结果"，否则会拿旧数当下限判断。
        if (!quiet) this.reply = null;
        this.error = res.error || ("接口返回 " + res.status);
        return;
      }
      this.reply = res.data;
      this.loaded = true;
      // 条数变少时当前页可能已经空了（房间被删、筛选还在）。
      const pages = Math.max(1, Math.ceil(this.visible.length / this.size));
      if (this.page > pages) this.page = pages;
    },
    place(r) { return placeName(r.shard); },
    // 手机上那一行的副标题。server.ini 里的分片名默认就是 Master/Caves 这个码值本身，
    // 和"地表"重复摆一遍既占位又像漏翻，所以只有它真被人改过名字时才出现。
    sub(r) {
      const n = r.shardName && r.shardName !== r.shard ? " · " + r.shardName : "";
      return this.place(r) + n;
    },
    // 点"进入"等于把这个房间设为全面板当前房间，然后回总览：
    // 总览、玩家、地图、日志四页都跟着 state.sel.cluster 走，这里不该再造一套详情页。
    open(r) {
      state.sel.cluster = r.cluster;
      location.hash = "#/";
    },
    // 点房间名 = 拿加入命令。放在名字上而不是加一个按钮列：这一页每多一列，
    // 手机上那张卡片就多一行，而"我要把人拉进来"本来就是看着房间名时冒出来的念头。
    join(r) { openConnect(r.cluster, r.roomName); },
    // 三种"没有世界数据"要分开说：在跑却没进世界是真问题（引擎卡在生成或登录上），
    // 没人时引擎自己把世界冻住是设计如此，没在跑则天经地义，不必占一行。
    // 措辞由后端给（worldNote）：前端只看得见 running，照着它猜就会把暂停中的世界
    // 说成"还没进世界"，和同一行助手那栏的"正被引擎暂停着"当场矛盾。
    world(r) {
      if (r.worldReady) return "第 " + Math.round(r.day) + " 天 · " + (r.seasonLabel || "") + " · " + (r.phaseLabel || "");
      return r.worldNote || "";
    },
    // 配置那一行的第二句。空值一律跳过：cluster.ini 没写游戏模式时引擎按默认跑，
    // 而"未知 / 未知 · 上限 6"会被读成"这个房间坏了"。
    // brief 是手机上用的：世界预设的中文名很长（"经典森林（无巨人的国度）"），
    // 在卡片里会把整行撑成三行，而它只在"这个房间是怎么生成的"时才需要看。
    meta(r, brief) {
      const parts = [r.gameModeLabel, r.intentionLabel];
      if (r.presetLabel && !brief) parts.push(r.presetLabel);
      parts.push("上限 " + r.maxPlayers);
      if (r.pvp) parts.push("开放 PVP");
      return parts.filter(Boolean).join(" · ");
    },
    mods(r) {
      if (r.modsEnabled === null || r.modsEnabled === undefined) return "未知";
      return r.modsEnabled + " / " + (r.modsTotal || 0);
    },
    badgeKind(r) {
      if (r.alert) return "warn";
      return r.running ? "running" : "idle";
    },
    // 在线人数只在助手真回了话时才有意义。写"0"会被读成"没人玩"，
    // 而这一列要回答的其实是"要不要现在上去看"。
    online(r) { return r.worldReady ? num(r.online) + " 人" : "未知"; },
    // 顶部那个合计：有世界在跑却读不到时不能直接报和——"0 人"会被读成"没人在玩"，
    // 而真实情况是引擎暂停了上报。已知的那部分照报，未知的部分明说。
    onlineStat() {
      const n = (this.reply && this.reply.online) || 0;
      if (!this.onlineBlind) return n + " 人";
      return n ? "至少 " + n + " 人" : "读不到";
    },
    // 接入方式正常时不上色，不对劲时给警告色：这一列是新手最容易踩的坑
    // （开了机、日志没报错、公网就是搜不到房间）。
    accessTone(r) { return r.access === "online" ? "" : "warn"; },
    // 归属那一枚的三档：未指派（普通）、派给别人（青）、派给我（绿）。
    // 颜色按 accountId / ownedByMe 这两个字段判，不去比 ownerLabel 的中文——
    // 说法改了配色不该跟着失灵。
    // 为什么这页能直接读 r.ownedByMe 而总览页要走 roomWritable：/rooms 每行本来就带着
    // 这一间的归属（后端按同一套规则算好的），而这一页没有启停按钮要判，
    // 只有"为什么改不动"那句说明需要，那句仍然从 store 拿，跟接口拒你时是同一份文案。
    ownerChip(r) {
      const why = roomWritable(r.cluster)
        ? "这一间你改得动：到房间页可以启停、改配置。"
        : readOnlyNote(r.cluster);
      return {
        tone: !r.ownerAccountId ? "" : (r.ownedByMe ? "ok" : "info"),
        icon: r.ownerAccountId ? "user" : "lock",
        label: r.ownerLabel || "读不到归属",
        hint: why,
      };
    },
  },
  template: `
    <div class="col" style="gap:14px">
      <div class="row chip-strip only-sm" style="gap:6px">
        <span class="chip">房间 {{ (reply && reply.clusters) || 0 }}</span>
        <span class="chip">世界 {{ (reply && reply.rooms) || 0 }}</span>
        <span class="chip">在跑 {{ (reply && reply.running) || 0 }}</span>
        <span class="chip">在线 {{ onlineStat() }}</span>
        <span class="chip" :class="reply && reply.attention ? 'warn' : ''">
          要注意 {{ (reply && reply.attention) || 0 }}</span>
      </div>

      <div class="grid grid-4 hide-sm">
        <x-stat label="房间" icon="house" tint="tint-azure" :value="(reply && reply.clusters) || 0"
                hint="一个房间就是一个集群目录，里面通常有地表和洞穴两个世界。下面数的是世界个数。"></x-stat>
        <x-stat label="在跑的世界" icon="power" tint="tint-green" :value="((reply && reply.running) || 0) + ' / ' + ((reply && reply.rooms) || 0)"
                hint="看的是服务端进程在不在跑，和游戏里有没有传回数据是两件事：进程刚起来时还没进存档。"></x-stat>
        <x-stat label="在线玩家" icon="users" tint="tint-cyan" :value="onlineStat()"
                :hint="onlineBlind
                  ? '有 ' + onlineBlind + ' 个世界在跑但这一刻读不到实时状态（没人时引擎会自己暂停，助手随之停摆），所以这个数不是「没人」，是「不知道」。'
                  : '地表和洞穴各报自己那边的玩家数，这里把这些数加起来；还没进存档的那边不计入。'">
          <span v-if="onlineBlind">{{ onlineBlind }} 个世界这一刻读不到</span>
          <span v-else>来自正在回话的世界</span>
        </x-stat>
        <x-stat label="要先处理" icon="warn" :value="(reply && reply.attention) || 0"
                :tone="reply && reply.attention ? 'warn' : 'ok'"
                hint="进程在跑但助手不回话、或者没配登录令牌导致公网搜不到——这两类会被数进来。点下面的「要注意」只看它们。">
          <span v-if="reply && reply.attention">换「要注意」这个筛选就能只看到它们</span>
          <span v-else>没有异常</span>
        </x-stat>
      </div>

      <div class="row" style="gap:8px;align-items:center">
        <div class="field" style="min-width:170px;flex:1 1 170px">
          <input type="search" v-model="q" placeholder="找房间：房间名、目录名或端口" aria-label="找房间">
        </div>
        <x-seg :options="statusOptions" :model-value="status" label="按运行状态筛选" @update="status = $event"></x-seg>
        <x-seg :options="ownerOptions" :model-value="owner" label="按归属筛选" @update="owner = $event"></x-seg>
        <div class="field" style="min-width:118px;flex:1 1 118px" v-if="clusterOptions.length > 1">
          <select v-model="cluster" aria-label="按房间筛选">
            <option value="">所有房间</option>
            <option v-for="c in clusterOptions" :key="c" :value="c">{{ c }}</option>
          </select>
        </div>
        <div class="field" style="min-width:118px;flex:1 1 118px">
          <select v-model="sort" aria-label="排序方式">
            <option v-for="o in sortOptions" :key="o.value" :value="o.value">{{ o.label }}</option>
          </select>
        </div>
        <span class="grow"></span>
        <x-hint v-if="reply && reply.generatedAt" label="数据时刻"
                :text="'这次取数是 ' + dateTime(reply.generatedAt) + '。之后 20 秒会自动重取一次；正在读的那次不会清空你已经看到的表格。'"></x-hint>
        <button class="btn ghost sm" :disabled="loading" @click="load()">
          {{ loading ? '读取中' : '刷新' }}</button>
        <!-- 这一页只看得到这台机器上的房间；"别人开的服在哪"是同一个念头的另一半，
             所以入口留在这里而不是让人自己去导航里找。 -->
        <a class="btn ghost sm" href="#/lobby"
           title="看 Klei 公开列表里别人开的房间：只能挑、只能复制连接命令，面板管不着它们">公网房间</a>
      </div>

      <div class="err-box" v-if="error">{{ error }}</div>
      <x-sk v-if="loading && !loaded" :lines="5"></x-sk>

      <x-empty v-else-if="loaded && !rows.length" icon="servers" title="这台机器上还没有房间"
               note="面板会在配置目录里找每个含 cluster.ini 的文件夹。一个都没有时先去开服向导建一个。">
        <a class="btn primary sm" href="#/room">去开服向导</a>
      </x-empty>
      <x-empty v-else-if="!visible.length" icon="search" title="没有符合筛选的房间"
               :note="'当前一共 ' + rows.length + ' 个世界，都被筛选条件挡住了。换个关键词，或者把上面两组筛选放回「全部」和「不限归属」。'">
        <button class="btn ghost sm" @click="q = ''; status = ''; cluster = ''; owner = ''">清掉筛选</button>
      </x-empty>

      <template v-if="visible.length">
        <!-- 桌面：一行一个世界。九列里有四列是"按需展开"的，屏幕上只留数字和短名。 -->
        <div class="table-wrap hide-sm">
          <table class="data">
            <thead>
              <tr>
                <th>房间</th><th>世界</th><th>状态</th><th>连接</th>
                <th>世界</th><th class="right">在线</th><th class="right">模组</th>
                <th>最近备份</th><th></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="r in paged" :key="r.cluster + '/' + r.shard" :class="r.alert ? 'flag' : ''">
                <td>
                  <div class="col" style="gap:2px">
                    <button class="room-name" @click="join(r)"
                            title="点这里取加入这个房间的命令：复制到游戏控制台粘贴就能进">
                      <span>{{ r.roomName }}</span><i class="ic-box"><x-icon name="copy" :size="13"></x-icon></i>
                    </button>
                    <span class="muted mono" v-if="r.roomName !== r.cluster">{{ r.cluster }}</span>
                    <!-- 同一间房的地表和洞穴归同一个负责人，所以只在每间房的第一行写一次。 -->
                    <span class="chip owner-chip" v-if="ownerFirst[r.cluster + '/' + r.shard]"
                          :class="ownerChip(r).tone" :title="ownerChip(r).hint">
                      <x-icon :name="ownerChip(r).icon" :size="11"></x-icon>
                      <span>{{ ownerChip(r).label }}</span></span>
                  </div>
                </td>
                <td>
                  <div class="row" style="gap:6px;align-items:center">
                    <span>{{ place(r) }}</span>
                    <x-hint :label="r.isMaster ? '主机' : '连主机'"
                            :text="(r.isMaster
                              ? '这个世界是房间的主机（master），洞穴世界要靠它才能连上；主机换了端口而洞穴还写着旧端口时，玩家穿传送门会失败。'
                              : '洞穴世界连的是 ' + (r.masterAddr || '没写在配置里') + '。这一条来自洞穴自己的 server.ini。')"></x-hint>
                  </div>
                </td>
                <td>
                  <div class="row" style="gap:6px;align-items:center">
                    <x-badge :kind="badgeKind(r)" :text="r.runningLabel">
                      <span class="hide-sm" v-if="r.running && r.pid">· {{ r.pid }}</span>
                    </x-badge>
                    <!-- 上色跟着后端那一列走：暂停/没起来这类"这一刻读不到"用中性，
                         令牌无效、世界生成卡住才用警告色。 -->
                    <x-hint v-if="r.running && r.helperNote" :tone="r.helperNoteUrgent ? 'warn' : 'info'"
                            label="助手" :text="r.helperNote"></x-hint>
                    <x-hint v-else-if="r.note" tone="warn" label="配置" :text="r.note"></x-hint>
                  </div>
                </td>
                <td>
                  <div class="col" style="gap:2px">
                    <div class="row" style="gap:6px;align-items:center">
                      <span class="mono">{{ r.port || '—' }}</span>
                      <x-chip :text="r.accessShort" :title="r.accessLabel" :tone="accessTone(r)"></x-chip>
                    </div>
                    <!-- 正常那一句每个世界都一样，重复四遍就是文字墙；不对劲的那句必须铺开说。 -->
                    <span class="muted" v-if="r.access !== 'online'">{{ r.accessLabel }}</span>
                  </div>
                </td>
                <td>
                  <div class="col" style="gap:2px">
                    <span v-if="world(r)">{{ world(r) }}</span>
                    <span class="muted">{{ meta(r) }}</span>
                  </div>
                </td>
                <td class="right num">{{ online(r) }}</td>
                <td class="right num">{{ mods(r) }}</td>
                <td>
                  <span v-if="r.lastBackupAt" :title="dateTime(r.lastBackupAt)">{{ rel(r.lastBackupAt) }}</span>
                  <span class="muted" v-else>未备份</span>
                </td>
                <td class="right">
                  <div class="row" style="gap:6px;justify-content:flex-end">
                    <button class="btn ghost sm" @click="open(r)">进入</button>
                    <!-- 指派挂在每间房的第一行：两行各摆一枚的话，同一件事看起来像有两个决定要做。 -->
                    <button class="btn ghost sm" v-if="canAssign && ownerFirst[r.cluster + '/' + r.shard]"
                            @click="openOwner(r.cluster, r.roomName)"
                            title="决定这间房归谁管：派出去之后那位成员就能自己启停、改配置">指派</button>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- 手机：同一批字段换成卡片。表格在这里要横向滑，而滑着找房间是这一页最差的用法。 -->
        <div class="col only-sm" style="gap:10px">
          <div class="card room-card" :class="r.alert ? 'flag' : ''" v-for="r in paged" :key="'m' + r.cluster + r.shard">
            <div class="row spread" style="align-items:flex-start;gap:8px">
              <div class="col" style="gap:2px">
                <button class="room-name" @click="join(r)"
                        title="点这里取加入这个房间的命令：复制到游戏控制台粘贴就能进">
                  <span>{{ r.roomName }}</span><i class="ic-box"><x-icon name="copy" :size="13"></x-icon></i>
                </button>
                <span class="muted">{{ sub(r) }}</span>
              </div>
              <x-badge :kind="badgeKind(r)" :text="r.runningLabel"></x-badge>
            </div>
            <div class="row" style="gap:6px;margin-top:8px">
              <!-- 手机上这一枚每张卡片都带：表格里有"同一间房的两行挨在一起"可靠分组，
                   卡片是一张张独立的，第二张不写就等于这一间没人管。 -->
              <span class="chip" :class="ownerChip(r).tone" :title="ownerChip(r).hint">
                <x-icon :name="ownerChip(r).icon" :size="11"></x-icon>
                <span>{{ ownerChip(r).label }}</span></span>
              <span class="chip">端口 {{ r.port || '—' }}</span>
              <span class="chip">在线 {{ online(r) }}</span>
              <span class="chip">模组 {{ mods(r) }}</span>
              <span class="chip" :class="accessTone(r)" :title="r.accessLabel">{{ r.accessShort }}</span>
              <span class="chip" v-if="r.lastBackupAt">备份 {{ rel(r.lastBackupAt) }}</span>
            </div>
            <div class="muted" style="margin-top:6px;font-size:12px" v-if="r.access !== 'online'">{{ r.accessLabel }}</div>
            <div class="row" style="gap:6px;align-items:center;margin-top:8px">
              <!-- 没在跑的世界没有世界状态可说，这一行改说"它是怎么配的"，不留一整格空白。 -->
              <span v-if="world(r)">{{ world(r) }}</span>
              <span class="muted" v-else>{{ meta(r, true) }}</span>
              <span class="grow"></span>
              <button class="btn ghost sm" v-if="canAssign" @click="openOwner(r.cluster, r.roomName)">指派</button>
              <button class="btn ghost sm" @click="open(r)">进入</button>
            </div>
            <div class="row bad-note" v-if="r.alert" style="margin-top:8px">
              <x-icon name="warn" :size="14"></x-icon>
              <span>{{ r.alert }}</span>
              <x-hint v-if="r.helperNote || r.note" tone="warn" label="详情"
                      :text="r.helperNote || r.note"></x-hint>
            </div>
          </div>
        </div>

        <div class="row" style="gap:8px;align-items:center">
          <span class="muted" v-if="filteredOut">筛选掉了 {{ filteredOut }} 条</span>
          <span class="grow"></span>
          <x-pager :page="page" :size="size" :total="visible.length" unit="个世界"
                  :loading="loading" @go="page = $event"></x-pager>
        </div>
      </template>
    </div>`,
};
