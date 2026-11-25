// 总览页（2026-11-09 定的顺序）：页顶是会催人做事的「下一步」，没有待办时收成一行；
// 中间是房间实况——页签选一间，左边这间的启停与玩家，右边在线趋势与行为构成；
// 最下面才是「这台机器」那张诊断卡（数值密、低频，出问题才查）。
// 不再用 CSS order 重排：DOM 顺序 = 视觉顺序 = 键盘 / 读屏顺序。
// （"分别是谁、各待了多久"那种逐人明细在「玩家管理」页，#125 搬过去的——第一眼看的是形状，不是名单。）
//
// 这一页的取舍：
//   - 进程在不在跑、助手回不回话是两件事，分开标（接口本来就是两个字段）。
//     把它们合成一个"正常/异常"会让"起来了但没人能进来看"这种现场看不出来。
//   - 所有会动到进程或存档的按钮都走 store.askConfirm，确认文案里必须带上后果，
//     而不是"你确定吗"。
//   - 状态快照拿不到（503）时按状态显示，不按错误显示：分片没启动时本来就没有快照。
//   - 一次只摊开一个房间：房间多起来时纵向排卡会把第四间推到折叠线以下，
//     而同一时刻真正要盯的只有一间（2026-11-04 用户提的页签方案）。
import * as api from "../api.js";
import { state, act, askConfirm, loadStatus, rel, isBusy, refreshAll,
  canAssignOwner, roomWritable, readOnlyNote, assignedText, ownerTone, ownerIcon,
  loadChecklist as syncChecklist } from "../store.js";
import { go, queryOf } from "../router.js";
// 安全 / 环境那两条「待处理提醒」并进这一栏（2026-11-09 用户）。
// 页顶那块提醒后来整个下掉了（同一轮用户："总览页面，下一步的卡片已经展示了这些信息，
// 其他页面还需要如图这个吗？看上去很丑"），这一张卡于是成了全站唯一说待办的地方：
// 顶栏那枚计数跳进来，管理员看到完整动作，只读账号看到同一批行但动作换成一句说明。
// 清单与取数都搬到全局（pending.js / store.js）：顶栏数字和这张卡读的是同一份快照。
import { checklistRows } from "../pending.js";
import { keyOf, num, statText, coord, dateTime, bytes, bytesPair, seconds } from "../fmt.js";
import { openConnect } from "../connect.js";
import { openOwner } from "../owner.js";
// 角色名走 gameinfo.js 那一份：详情里原来直接摆 p.prefab，界面上就是 wilson 这种码值。
import { charName, charStyle } from "../gameinfo.js";
// ShardRow 用混入拿 writable / readOnlyNote，这一行漏了就是整页空白：
// mixins 里引用一个不存在的标识符在模块求值时就炸，连登录页都渲染不出来。
import { RoomAccess } from "../access.js";
// 时间窗那四档和「玩家管理」里那张在线时间线共用一份，所以它住在 timeline.js 里：
// 两边各留一份的话，迟早漂成"上面选的近 3 天、下面写着近 7 天"。
import { TL_WINDOWS } from "../timeline.js";

// 分片名到地形说法。Master/Caves 是引擎与目录用的名字，界面上留着便于对着文件排查，
// 但玩家视角的地形说法要给出来——接口里没有这个字段，而它是固定的两条。
const PLACE = { Master: "地表", Caves: "洞穴" };

// 紧凑行里三围是三段等宽细条：每行的三段位置固定，纵向对齐后仍能在人与人之间比同一条。
// 助手没采到某个组件时给的是 null，不能画成 0%（那会被读成"快饿死了"）：
// 返回 null 让模板把整段留成淡底，悬停里写「未知」。
function vitalStyle(v, max) {
  if (v == null || max == null || !(max > 0)) return null;
  return { width: Math.max(0, Math.min(100, Math.round((v / max) * 100))) + "%" };
}

const ShardRow = {
  props: { cluster: String, shard: Object },
  mixins: [RoomAccess],
  setup() { return { state, act, askConfirm, loadStatus, rel, isBusy, keyOf, num, statText, coord, dateTime, bytes, PLACE, charName, charStyle, vitalStyle }; },
  // 最近一次生命周期接口回的阶段与警示。phase/phaseReason 会跟着 /clusters 的轮询刷新，
  // 而强停那句"会丢档"、停服没确认退出的 warning 只在动作发生那一刻回来，
  // 得自己留一份，否则下一次轮询把它冲掉，用户就再看不到那句提醒了。
  // rosterOpen 只管「9 人以上」那一档的名单展开：人数掉回 5~8 时形态自动变回紧凑行，
  // 这个状态留着，用户再回到人多那一档时不必重新点一次。
  data() { return { lastAction: null, rosterOpen: false }; },
  computed: {
    key() { return keyOf(this.cluster, this.shard.name); },
    entry() { return state.statuses[this.key] || null; },
    st() { return this.entry ? this.entry.data : null; },
    busy() { return (verb) => isBusy(verb + ":" + this.key); },
    // 只有"进程在跑"却没有快照时才值得提醒：没跑的分片不该有快照。
    warnHelper() { return this.shard.running && !this.shard.helperOnline; },
    players() { return this.st && this.st.players ? this.st.players : []; },
    // 玩家展示形态按人数分档（2026-11-09 用户："既然多个角色会乱，你打算怎么解决"）。
    // 根子是富信息卡的信息密度固定、人数却不定长：六张卡在窄列里竖着叠成一堵墙，
    // 手机上只能露出两张、其余全靠在格子里滑。把卡片压窄、给容器封顶都是把人藏起来，
    // 所以改成形态跟着人数走——人越多每人占的高度越省，任何人数都不会把房间卡撑爆。
    //   ≤4 富卡：三条三围竖排，人少时横向比"谁快饿死了"最有用，别动；
    //   5~8 紧凑行：一行一人，单条约 34px，六人一屏读完；
    //   >8 摘要 + 展开：先只报人数，展开后仍是紧凑行。
    playerMode() {
      const n = this.players.length;
      return n <= 4 ? "cards" : n <= 8 ? "rows" : "fold";
    },
    // 紧凑行只在这儿算一次：三围的百分比 / 颜色 / 悬停文案和富卡吃同一批字段，
    // 别让两种形态各写一遍"采不到是 null"，否则迟早一处画成 0%、一处画成未知。
    rosterRows() {
      return this.players.map((p) => {
        const raw = [
          { key: "hp", label: "血量", v: p.health, max: p.maxHealth, kind: "ok" },
          { key: "food", label: "饥饿", v: p.hunger, max: p.maxHunger, kind: "warn" },
          { key: "san", label: "理智", v: p.sanity, max: p.maxSanity, kind: "azure" },
        ];
        return {
          p,
          // 富卡里那行「角色 · 坐标 · 物品」在紧凑行里放进整行悬停，不额外占高度。
          tip: charName(p.prefab) + " · 坐标 " + coord(p.x, p.z) +
               " · 物品 " + statText(p.items) + " 件",
          vitals: raw.map((x) => ({
            key: x.key, kind: x.kind, style: vitalStyle(x.v, x.max),
            unknown: x.v == null || x.max == null,
            tip: x.label + " " + (x.v == null || x.max == null
              ? "未知" : Math.round(x.v) + "/" + Math.round(x.max)),
          })),
        };
      });
    },
    // 玩家实况要不要摊开（2026-11-04 查"首页有点空"查出来的改法）。
    // 判据只有一条：进程在跑、采集没停在旧时刻、这次真的报上来人。
    // 停着的世界那份快照是旧的，摊开等于把几小时前的人摆在首页上当"现在"；
    // 没人时引擎自己把世界冻住，stale 为真，于是也不摊——首页不该有一片假的热闹。
    live() { return !!(this.shard.running && this.st && !this.st.stale && this.players.length); },
    // 这个世界在这台机器上占了多少。数字来自 /host 那一轮的按进程号采样，
    // 所以它和上面那排仪表同一个区间、同一个口径（占全部核的百分比）。
    proc() { return this.shard.running ? (state.hostByPid[this.shard.pid] || null) : null; },
    procText() {
      const p = this.proc;
      if (!p) return "占用刚开始采样";
      const rss = p.rssBytes > 0 ? bytes(p.rssBytes) : "—";
      // 采不到和还在测要分得开，但只用两个字的短词分开：接口那句"这个系统上采不到
      // 单个进程的占用（面板跑在 Linux 上时才有）"整句摆进这枚 nowrap 的胶囊，
      // 在 390 的手机上会把文档撑到 451（2026-11-07 量出来的），整屏多一条横向滚动。
      // 那句话本身没坏，它该住的地方是下面的悬停。
      if (!p.cpuReady) return "内存 " + rss + " · CPU " + (p.note ? "采不到" : "还在测");
      return "内存 " + rss + " · CPU " + Math.round(p.cpuPercent) + "%";
    },
    // 这一枚现在直接摆在世界行上，悬停就得把"这两个数是怎么来的"讲完：
    // 它是 #95 那次采错对象的口径说明（那时候界面报的是 screen 会话那 2 MB 空壳），
    // 也是"为什么这里比仪表小得多"的答案——一个世界，不是整台机器。
    // 接口那句"采不到"的原因（后端按平台给的话）也并到这里，而不是挤进胶囊。
    procHint() {
      const p = this.proc;
      return "这一个世界在这台机器上占的内存和 CPU：把 screen 会话（进程号 " + this.shard.pid +
        "）和它底下来的饥荒服务端进程加在一起算。只数会话那一个会读出 2 MB 的空壳，" +
        "引擎其实是它的子进程、占的是几百 MB。CPU 和上面那排仪表同一个口径（占全部核的百分比），" +
        "每五秒采一轮，刚起来那一轮还没有数。" + (p && p.note ? "（" + p.note + "）" : "");
    },
    // "这一季第几天"这句话有没有得说，判据全在后端（internal/modbridge/status.go）：
    // 引擎那两个读数缺一个、或者季节根本不轮换（无尽模式那边"还剩几天"报的是哨兵值），
    // 接口就不给这个键。这里只照着有没有说话，不自己凑一个"第 1 天"出来。
    // 括号的写法是为了跟左边那枚「第 21 天」（世界年龄）分开摆——两个"第几天"挨着写，
    // 读的人一定会以为其中之一是编出来的。
    seasonChip() {
      const st = this.st;
      if (!st) return "";
      const day = st.dayInSeason ? ("（第 " + num(st.dayInSeason) + " 天）") : "";
      return st.seasonLabel + day;
    },
    seasonHint() {
      const st = this.st;
      if (!st) return "";
      const base = "这一轮世界规则：" + st.seasonLabel + " · " + st.phaseLabel + "。";
      if (!st.dayInSeason) {
        return base + "这一季的第几天要助手把季节读数一起报回来才说得出（老版助手没报，或这个世界季节不轮换）。";
      }
      const left = st.daysLeftInSeason === undefined || st.daysLeftInSeason === null
        ? "" : (st.daysLeftInSeason > 0 ? ("，还剩 " + num(st.daysLeftInSeason) + " 天") : "，今天过完就换季");
      return base + "这一季走到第 " + num(st.dayInSeason) + " 天" + left + "；换季时重新从第 1 天算。";
    },
    // 启动阶段：不再把"screen 会话存在"显示成"运行中，一切正常"。
    // 阶段值来自后端——状态快照的 bootPhase 最实时，其次 /clusters 的 phase，
    // 再退回本次生命周期响应的 phase。reason 挂悬停，并在行上写一句短说明。
    phaseView() {
      const s = this.shard;
      const st = this.st && !this.st.stale ? this.st : null;
      const a = this.lastAction;
      const phase = (st && st.bootPhase) || s.phase || (a && a.phase) || "";
      const reason = (st && st.bootPhaseReason) || s.phaseReason || (a && a.phaseReason) || "";
      const map = {
        starting: { text: "已发出启动命令", kind: "info" },
        loading:  { text: "世界还在加载", kind: "warn" },
        ready:    { text: "可进入", kind: "ok" },
        failed:   { text: "启动失败", kind: "bad" },
      };
      const v = map[phase];
      if (v) return { text: v.text, kind: v.kind, title: reason || v.text, note: reason };
      if (!s.running) return null;
      // 面板没亲自拉起这个世界（比如面板刚重启、或世界是手工开的）时后端没有
      // 启动事务记录，phase 会是空。这是常态，不该摆成一句像故障的提示；
      // 但也别写成"一切正常"——会话在跑和世界能进终究是两件事，理由放悬停里。
      return { text: "运行中（阶段未知）", kind: "running",
        title: "screen 会话进程号 " + (s.pid || "未知") + " 在跑；这个世界不是由本次面板启动流程拉起的，所以没有加载阶段记录。会话存在不等于世界已经能进。", note: "" };
    },
  },
  methods: {
    async start() {
      const res = await act("start:" + this.key, "POST",
        `/api/v1/clusters/${encodeURIComponent(this.cluster)}/shards/${encodeURIComponent(this.shard.name)}/start`,
        {}, {
          okTitle: "已让 " + this.shard.name + " 启动",
          okBody: (d) => (d && d.note) || "第一次进世界要生成地图，通常一到两分钟",
          badTitle: this.shard.name + " 启动失败",
        });
      // 启动成功后立刻补一次状态读取：轮询最快也要五秒，而用户这会儿正盯着屏幕。
      if (res.ok) { this.lastAction = res.data || null; await loadStatus(this.cluster, this.shard.name); }
    },
    async stop() {
      const ok = await askConfirm({
        title: "停止 " + this.cluster + " 的 " + this.shard.name + "？",
        body: "会先注入存盘命令，再等服务端自己退出（这是 Klei 的默认落盘顺序）。",
        warn: "在线玩家会掉线；正在进行的战斗与掉落物停在存档里。",
        okLabel: "停止",
      });
      if (!ok) return;
      const res = await act("stop:" + this.key, "POST",
        `/api/v1/clusters/${encodeURIComponent(this.cluster)}/shards/${encodeURIComponent(this.shard.name)}/stop`,
        { saveFirst: true }, { okTitle: "已让 " + this.shard.name + " 落盘退出", badTitle: "停止没成" });
      if (res.ok) this.lastAction = res.data || null;
    },
    async restart() {
      const ok = await askConfirm({
        title: "重启 " + this.cluster + " 的 " + this.shard.name + "？",
        body: "先落盘停止，等 screen 会话真的退出，再重新起来。中途没有取消的余地。",
        warn: "模组、助手配置、世界规则的改动都要重启世界才生效。",
        okLabel: "重启",
      });
      if (!ok) return;
      const res = await act("restart:" + this.key, "POST",
        `/api/v1/clusters/${encodeURIComponent(this.cluster)}/shards/${encodeURIComponent(this.shard.name)}/restart`,
        {}, { okTitle: "已重启 " + this.shard.name, badTitle: "重启没成" });
      if (res.ok) { this.lastAction = res.data || null; await loadStatus(this.cluster, this.shard.name); }
    },
    async kill() {
      const ok = await askConfirm({
        title: "强制关闭 " + this.shard.name + "？",
        body: "直接掐掉 screen 会话，不等存档落盘。",
        warn: "存档会停在最后一次自动保存，最近几分钟的变化可能丢失；只有在这个进程卡死、" +
          "连停止命令都没反应时才该用它。这一步会在操作记录里标成「强制关闭世界（未落盘）」。",
        danger: true,
        okLabel: "强制关闭",
      });
      if (!ok) return;
      const res = await act("kill:" + this.key, "POST",
        `/api/v1/clusters/${encodeURIComponent(this.cluster)}/shards/${encodeURIComponent(this.shard.name)}/kill`,
        {}, { okTitle: "会话已强关", badTitle: "强制关闭没成" });
      if (res.ok) this.lastAction = res.data || null;
    },
  },
  template: `
    <div class="shard-row">
      <span class="shard-name">{{ shard.name }}</span>
      <span class="chip" v-if="PLACE[shard.name]">{{ PLACE[shard.name] }}</span>

      <!-- 服务器名和引擎那一局的编号合并挂在这一枚上（2026-11-04 拆掉「详情」那一档时定下的去处）：
           名字本身宽屏摆得下，编号只给翻日志的人对，摆出来是噪声，所以收进悬停。 -->
      <span class="chip hide-sm" v-if="st && st.serverName"
            :title="'服务器名（引擎自己上报的那一份）。世界号 ' + st.gameId +
                     ' 是引擎给这一局编的号，翻 server_log.txt、对存档目录时拿它找。'">{{ st.serverName }}</span>

      <!-- pid 在手机上藏掉：它是拿去敲 ps / 翻日志的，不是扫一眼要拿到的数。
           它同时写进了右边那枚占用的悬停——占用本来就是"会话加它的饥荒子进程"，
           两个号摆在一句话里才对得上账。 -->
      <!-- 启动阶段按后端 phase 显示中文，不再把"screen 会话存在"写成"在跑，一切正常"：
           会话起来只说明进程在，世界可能还在加载，也可能启动已经失败。 -->
      <x-badge v-if="shard.running" :kind="phaseView ? phaseView.kind : 'running'"
               :text="phaseView ? phaseView.text : '运行中（阶段未知）'"
               :title="phaseView ? phaseView.title : ('screen 会话进程号 ' + shard.pid + '；会话存在不等于能进')">
        <span class="hide-sm">pid {{ shard.pid }}</span></x-badge>
      <x-badge v-else-if="phaseView && phaseView.kind === 'bad'" kind="bad"
               :text="phaseView.text" :title="phaseView.title"></x-badge>
      <x-badge v-else kind="idle" text="没在跑"></x-badge>

      <!-- 进程停了还说"助手在回话"是自相矛盾：那句绿字只描述活着的进程。
           这里改成"上次数据"，并把下面那排数字压暗——它们确实是最后一次采集，
           但已经不是现在。 -->
      <!-- 手机上这一颗让位：那排数字本来就压暗了（.chips-old），再加一颗灰胶囊
           只是把第一行挤到折行；"没在跑"已经把"不是现在"说清楚了。 -->
      <x-badge v-if="!shard.running && st" class="hide-sm" kind="idle" text="上次数据"
               :title="'进程已经停了，下面这排是最后一次采集（本地 ' + dateTime(st.updatedAt) + '）'"></x-badge>
      <!-- 绿色那句"助手在回话"在手机上先藏掉：它和"在跑 pid"说的是同一件事，
           而窄屏每多一个标记就要多折一行。"助手没回话"是另一回事，它必须一直看得见。 -->
      <x-badge v-else-if="shard.helperOnline" class="hide-sm" kind="ok" text="助手在回话"></x-badge>
      <x-badge v-else-if="warnHelper" kind="warn" :text="'助手没回话'" :title="shard.helperNote"></x-badge>
      <!-- 这颗也按行宽让位：紧跟其后的那行红字（.shard-note.bad）说的就是同一件事，
           而 helperState 在助手不在线时一定会给出那句话，所以藏掉不会丢信息。 -->
      <x-badge v-else class="hide-sm" kind="idle" text="没有数据" :title="shard.helperNote"></x-badge>

      <!-- 这台机器上的占用：以前它只活在「详情」那张表里，为看一个内存数要点一次展开。
           进程停着时不挂这一枚——没有进程就没有占用，摆一个"—"是假信息。 -->
      <span class="chip mono" v-if="shard.running" :title="procHint">{{ procText }}</span>

      <!-- 采集停在旧时刻时，这一排数字同样是旧的（暂停时最常见：最后一报是有人那一刻）。
           压暗要跟"进程停了"一个待遇，否则旁边写着"没人"、这里写着"在线 1/6"，两句互相打脸。 -->
      <!-- 数据时间挂在「第几天」这一枚的悬停上：它是这一排数字共同的落款（原来是详情表最后一行），
           而这一枚是这排的第一个，眼睛先从它起。 -->
      <span class="row" :class="(shard.running && !st.stale) ? '' : 'chips-old'" v-if="st"
            :title="'最后一次采集：' + rel(st.updatedAt) + '（本地 ' + dateTime(st.updatedAt) + '）'">
        <span class="chip mono">第 {{ num(st.day) }} 天</span>
        <span class="chip mono" :title="'这局世界最多 ' + st.maxPlayers + ' 人，其中 ' +
                players.filter((p) => !p.dead).length + ' 人这会儿还活着'">在线 {{ st.online }}/{{ st.maxPlayers }}</span>
        <span class="chip hide-sm" :title="seasonHint">{{ seasonChip }}</span>
        <span class="chip hide-sm">{{ st.phaseLabel }}</span>
        <span class="chip mono hide-sm" title="洞穴里没有地表季节与昼夜这一说">温度 {{ Math.round(st.temperature) }}° · 潮湿 {{ Math.round(st.wetness) }}</span>
        <x-badge v-if="st.stale" kind="warn" :text="'数据停在 ' + rel(st.updatedAt)" :title="'采集于本地 ' + dateTime(st.updatedAt)"></x-badge>
      </span>

      <span class="grow"></span>

      <!-- 只读账号不把按钮藏掉：灰掉并在 title 里写清为什么（原因来自 access 混入的 readOnlyNote）。
           消失的按钮会让人以为功能不存在，也不知道该找谁要权限。 -->
      <button class="btn sm primary" v-if="!shard.running" :disabled="!writable || busy('start')"
              :title="writable ? '' : readOnlyNote" @click="start">
        {{ busy('start') ? '启动中……' : '启动' }}</button>
      <button class="btn sm" v-else :disabled="!writable || busy('stop')"
              :title="writable ? '' : readOnlyNote" @click="stop">停止</button>
      <!-- 危险操作单独分组：重启会踢掉在线玩家、强停会丢档，别和查看类按钮同一排等大并列。
           .actions-danger 自带左侧分隔线、窄屏整组换到第二行；style 里的 order 只为对齐
           窄屏 .shard-row > .btn 那一档的排序，不改动桌面端的读法。 -->
      <div class="actions-danger" v-if="shard.running" style="order:2">
        <button class="btn sm" :disabled="!writable || busy('restart')"
                :title="writable ? '先落盘停止，等 screen 会话真的退出再起来；中途没有取消的余地' : readOnlyNote"
                @click="restart">重启</button>
        <button class="btn sm danger" :disabled="!writable || busy('kill')"
                :title="writable ? '直接掐掉会话、不等存档落盘，只在进程卡死、连停止都没反应时用' : readOnlyNote"
                @click="kill">强停</button>
      </div>
      <span class="chip lock-chip" v-if="!writable" :title="readOnlyNote">
        <x-icon name="lock" :size="12"></x-icon><span>看得到，动不了</span></span>

      <!-- 上色由后端那一列决定，不靠前端猜句子：没人时引擎冻住世界（等有人进来就行）
           和令牌无效（得人去改文件）都写在这同一行里，一律染红就等于把两种
           完全不同的下一步压成一句"出错了"。 -->
      <div class="shard-note" :class="shard.helperNoteUrgent ? 'bad' : 'calm'" v-if="shard.helperNote">{{ shard.helperNote }}</div>

      <!-- 生命周期响应的 warning（强停会丢档、停服没确认退出）必须显眼，不能只当一句普通说明。 -->
      <x-note v-if="lastAction && lastAction.warning" kind="warn" style="flex-basis:100%">
        <b>注意：</b>{{ lastAction.warning }}
      </x-note>
      <div class="shard-note" v-if="phaseView && phaseView.note">启动阶段：{{ phaseView.note }}</div>

      <div class="shard-note" v-if="entry && entry.error && shard.running">
        状态数据问不到：{{ entry.error }}</div>
      <div class="shard-note" v-if="st && st.collectError">
        采集这轮出了错，下面的数字可能不完整：{{ st.collectError }}</div>

      <!-- 这一块只在"这轮真的有人"时自己摊开（判据见 computed live）。
           「详情」那一档整块拆掉的账（2026-11-04 用户问"还有必要吗"）：
           原来那张五行明细表里，服务器名和世界号进了上面那枚名字胶囊的悬停，数据时间挂在
           这一排数字的悬停上，四季与昼夜写在季节胶囊里，在线那一行本来就是行上那枚人数；
           唯一独有的「这台机器上的占用」做成了行上的一枚胶囊。
           留一档要点开才看得见的表格，等于让人多点一次才拿到同一屏已经有的东西。 -->
      <div class="shard-detail" v-if="live">
        <!-- 9 人以上先只报人数：名单要点开才铺开，铺开后仍是下面的紧凑行。
             一枚 button 而不是 div，键盘能聚焦、读屏能报「已展开 / 已收起」。 -->
        <button class="roster-summary" type="button" v-if="playerMode === 'fold'"
                @click="rosterOpen = !rosterOpen"
                :aria-expanded="rosterOpen ? 'true' : 'false'">
          <span class="ico-tile sm tint-azure"><x-icon name="users" :size="14"></x-icon></span>
          <span class="grow">{{ players.length }} 人在线</span>
          <span class="chip">{{ rosterOpen ? '收起' : '展开名单' }}</span>
        </button>

        <!-- 4 人以内：富信息卡。三条三围竖排（2026-11-04 用户提的），
             并排时三条都短、看不出"这一条比那个人少多少"，竖着排才好横向比。 -->
        <div class="grid grid-cards detail-players" v-if="playerMode === 'cards'">
          <div class="stat" v-for="p in players" :key="p.userid || p.name">
            <div class="row spread">
              <span class="strong">{{ p.name }}</span>
              <x-badge v-if="p.dead" kind="bad" text="已死亡"></x-badge>
              <x-badge v-else kind="ok" text="活着"></x-badge>
            </div>
            <div class="stat-note">{{ charName(p.prefab) }} · 坐标 <span class="mono">{{ coord(p.x, p.z) }}</span> · 物品 {{ statText(p.items) }} 件</div>
            <div class="player-bars">
              <x-bar label="血量" :value="p.health" :max="p.maxHealth" kind="ok"></x-bar>
              <x-bar label="饥饿" :value="p.hunger" :max="p.maxHunger" kind="warn"></x-bar>
              <x-bar label="理智" :value="p.sanity" :max="p.maxSanity"></x-bar>
            </div>
          </div>
        </div>

        <!-- 5 人以上：一行一人。三条三围是同一行里三段等宽细条，每行位置对齐，
             纵向仍能在人与人之间比同一条；坐标 / 物品数 / 精确数值收进悬停，不占高度。 -->
        <div class="player-roster" v-if="playerMode === 'rows' || (playerMode === 'fold' && rosterOpen)"
             :class="playerMode === 'fold' ? 'is-expanded' : ''">
          <div class="player-line" v-for="r in rosterRows" :key="r.p.userid || r.p.name" :title="r.tip">
            <span class="player-avatar sm" :style="charStyle(r.p.prefab)">{{ (r.p.name || '?').slice(0, 1) }}</span>
            <span class="pl-id">
              <span class="strong ellipsis">{{ r.p.name }}</span>
              <x-badge v-if="r.p.dead" kind="bad" text="已死亡"></x-badge>
            </span>
            <span class="pl-char ellipsis">{{ charName(r.p.prefab) }}</span>
            <span class="pl-vitals">
              <span class="v-track" v-for="v in r.vitals" :key="v.key" :title="v.tip">
                <i class="v-fill" :class="'v-' + v.kind + (v.unknown ? ' is-unknown' : '')"
                   :style="v.style || { width: '100%' }"></i>
              </span>
            </span>
          </div>
        </div>
      </div>
    </div>`,
};

// 「行为构成」每一行的配色。死亡 / 幽灵 / 复活三档直接沿用「玩家在线时间」那条泳道上
// 三个圆点的语义色（app.css 里 .tl-mark.death 用 --bad、.ghost 用 --warn、.revive 用 --ok）：
// 同一个意思在两个页面里不能换颜色，否则读者要各学一遍色。
// 进服、离开没有好坏含义，从色相盘里各取一档。目录里万一多出这一张表没见过的种类，
// 落到 --h-slate 那档中性色：宁可颜色普通，也不能没有颜色。
const BH_TONE = {
  player_join: "azure", player_leave: "slate",
  player_death: "bad", ghost_gone: "warn", player_resurrect: "ok",
};

export default {
  name: "Overview",
  components: { "x-shard-row": ShardRow },
  setup() { return { state, rel, num, refreshAll, api, go, openOwner }; },
  data() {
    return {
      trend: { loading: false, error: "", points: [], hours: 24, cluster: "", meta: null },
      // 「行为构成」读的就是这一次 player-timeline 回答里的 events，不另发一次问数。
      // 泳道那张明细卡已经在 #125 搬到「玩家管理」，这里留它只为那几行计数。
      // seq 管"晚到的那份不许写界面"：连点两下页签、或者切房间和换窗口撞上时，
      // 只有最后发出去的那次能落进图里。
      timeline: { loading: false, error: "", data: null, seq: 0 },
      // 清单快照住在 store.state.checklist 里（顶栏也要读），这里不再自己存一份；
      // 取数同样由 store 统一跑，避免挂在总览页时才刷新、切到别页顶栏数字就旧了。
      // 主机指标默认在窄屏折叠：首屏先把「房间与世界状态」让出来，宽屏照旧摊开。
      hostOpen: !(window.matchMedia && window.matchMedia("(max-width: 720px)").matches),
      // 从别页「去下一步」跳过来时给这张卡一次短暂高亮，2.6 秒后自己退掉。
      focused: false,
    };
  },
  computed: {
    trendWindows() { return TL_WINDOWS; },
    // 时间窗控件只在「在线情况」那张卡上，但这一列两张图共用它（同一次 setHours 会同时重取两份）。
    // 「玩家管理」里那张泳道自带一排同样的按钮，档位是同一份 TL_WINDOWS，只是各选各的窗口。
    hoursLabel() {
      const w = TL_WINDOWS.find((x) => x.value === this.trend.hours);
      return w ? w.label : "";
    },
    // 图右边那枚把手：数据新不新是这张图唯一会骗人的地方。
    // 采样每来一次轮询才写一条，房间停着不跑就一条都不写，所以"线停在几小时前"
    // 既可能是没人玩，也可能是这个房间根本没开——后端给的 latestAgoMinutes 只说事实。
    trendFresh() {
      const m = this.trend.meta;
      if (!m) return null;
      const mins = Number(m.latestAgoMinutes);
      if (!Number.isFinite(mins)) return null;
      if (mins <= 5) return { kind: "ok", text: "最近一条采样 " + mins + " 分钟前" };
      if (mins <= 90) return { kind: "warn", text: "最近一条采样 " + mins + " 分钟前" };
      const h = Math.round(mins / 60);
      return { kind: "warn", text: "最近一条采样 " + h + " 小时前" };
    },
    runningClusters() {
      return state.clusters.filter((c) => (c.shards || []).some((s) => s.running)).length;
    },
    // 图上方那枚标题写房间名，不写存档目录名。Cluster_1 是面板拿去读文件夹的名字，
    // 用户在游戏服务器列表里从来没见过它——2026-11-04 用户就问了"这看得懂？"。
    // 目录名没丢，挪进了悬停：排查的人要拿它去对 cluster.ini。
    trendRoom() {
      if (!this.trend.cluster) return "";
      const c = state.clusters.find((x) => x.name === this.trend.cluster);
      if (!c) return this.trend.cluster;
      return c.roomName || c.name;
    },
    // 页签选中的那一间。选中的永远是一间真实存在的房间：store 在列表里
    // 找不到当前选中时会自动换一个（优先挑有世界在跑的），所以这里不用兜底。
    room() { return state.clusters.find((c) => c.name === state.sel.cluster) || null; },
    tl() { return this.timeline.data; },
    // 这一窗里"出现过几个人"和上面那条线的"峰值几人"是两件事，两个都要：
    // 峰值是同一时刻最多几个人，这个数是把这一窗里露过面的人头数一遍——
    // 六个人轮着来、每次只来一个，那条线全程在 1，而这个数是 6。
    // 逐人的明细（谁、各待多久）在「玩家管理」那张泳道上（#125 搬走的），总览只留这个数。
    tlPeople() { return this.tl && this.tl.people ? this.tl.people : []; },
    // 「行为构成」读的就是上面那一次时间线回答里的 events，不另发一次问数：
    // 这一列两张卡共用同一个分母，多问一次就会多出两个"次数"。
    behRows() { return this.tl && this.tl.events ? this.tl.events : []; },
    behMax() { return this.behRows.reduce((m, e) => Math.max(m, e.count || 0), 0); },
    // 「这一张怎么读 / 这些数怎么来的」那些气泡整组下掉了（2026-11-09 用户：
    //   "完全没必要"）。图本身与图下那句结论还在，口径要看去接口或文档。
    // 未结束任务的清单与口径不在这里：总览原来那张卡整张下掉（2026-11-09），
    // 顶栏那枚计数一键直达任务中心，那一页自己写着进度、明细和"空着不等于闲着"的口径。
    isAdmin() { return !!(state.me && state.me.isAdmin); },
    // 顶栏那枚「待办」入口会带 #/?focus=todo 过来。
    // 走 computed 才能被 watch 追到：route.query 是响应式对象，直接读一次不会回头通知。
    focusQuery() { return queryOf("focus"); },
    // 能不能指派归属由后端说（canAssignOwner），前端不自己按角色推：
    // 将来若要放开"成员也能转派自己的房间"，这里不用动。
    canAssign() { return canAssignOwner(); },
    // 归属这一枚的三档说法整个来自后端（"未指派" / "归 bob 管"）——成员账号打不开账号名单，
    // 前端拼不出人名；而"为什么这排按钮没了"直接拿 store 的 readOnlyNote，和接口拒他时那句是同一份文案。
    ownerChip() {
      return (c) => {
        // 指派那一句带时刻，时刻按浏览器所在时区显示（见 store 的 assignedText）。
        const note = assignedText(c);
        return {
          tone: ownerTone(c), icon: ownerIcon(c),
          label: c.ownerLabel || "读不到归属",
          hint: (note ? note + "。" : "") +
            (roomWritable(c.name)
              ? "这一间你改得动：启停、配置、令牌都在你的权限范围内。"
              : readOnlyNote(c.name)),
        };
      };
    },
    // 最近备份这一枚读的是上面「下一步」那一次取数已经带回来的字段：备份时间只有
    // /api/v1/rooms 给，而那一次是一次磁盘扫描，本来就不跟着五秒的轮询跑（见 loadChecklist）。
    // 在这里再发一次请求，等于把一次全盘扫描绑到高频轮询上。
    //
    // 三种情况分开：取数没回来 / 这一间不在结果里 → 整枚不上屏（读不到就不说）；
    // 结果里有这一间但没有备份时间 → 才是真的「未备份」。
    // 时间取这间房各世界最新的每一份：快照按房间打（一份里带上地表和洞穴），
    // 所以这一枚说的是整个房间，不是下面哪一个世界。
    backupChip() {
      return (name) => {
        const rows = (this.check.data && this.check.data.items) || [];
        const mine = rows.filter((x) => x.cluster === name);
        if (!mine.length) return null;
        const times = mine.map((x) => x.lastBackupAt).filter(Boolean).sort();
        if (!times.length) {
          return { text: "未备份", tone: "warn",
            hint: "这个房间一次都没有备份过：存档坏了只能重开。快照按房间打，一份里带上这间的每个世界。" };
        }
        const at = times[times.length - 1];
        return { text: "备份 " + rel(at), tone: "",
          hint: "最近一次存档备份是本地 " + dateTime(at) + "，这一枚说的是整个房间（快照一份带上这间的每个世界）。" +
            "要恢复或手工再打一份，去「存档备份」。" };
      };
    },
    // 首屏那份"下一步"：与顶栏「待办」共用 pending.js 的 checklistRows 和
    // store.state.checklist 那一份快照，所以卡头「N 项待办」与顶栏数字必然是同一个数。
    // 一条都不缺时这一栏收成一行"都齐了"，不占首屏；有待办时才摊开成整张卡
    // （2026-11-09 用户问"下一步和这台机器放顶部还是底部"，按整机级 / 诊断级分开排）。
    check() { return state.checklist; },
    checkItems() { return checklistRows(state, state.checklist.data); },
    // 走 computed 而不是直接 watch "state.sel.cluster"：watcher 的路径字符串
    // 要在实例上找得到那条链，而 state 是 setup 里带出来的外部对象，
    // 版本一换就容易变成"看着写了、其实一次都没触发"。
    selCluster() { return state.sel.cluster; },
    host() { return state.host; },
    // 口径说明整行下掉了（2026-11-09 用户："这台机器里得口径，我也觉得没必要"）。
    // 后端 /host 仍返回 metricsNote / metricsScope，界面不再铺开；想看的人去接口或文档。
    lastSync() { return state.lastSync; },
    // 三颗表盘按"这台机器最吃紧的三样东西"排：算力、内存、存档落在哪块盘还剩多少。
    // 每一项在没有可信数字时交 note（接口那句中文），由 Ring 决定"不画弧也不报 0%"。
    rings() {
      const h = state.host;
      if (!h) return [];
      const out = [];

      out.push({
        key: "cpu", label: "处理器", icon: "pulse",
        percent: h.cpuReady ? h.cpuPercent : null,
        note: h.cpuReady ? (h.cpuCores + " 核 · 最近一轮的平均") : (h.cpuNote || "这一项没读到"),
      });

      if (h.memTotalBytes > 0) {
        const pair = bytesPair(h.memUsedBytes, h.memTotalBytes);
        out.push({
          key: "mem", label: "内存", icon: "cube", percent: h.memPercent,
          // 交换区只在真配了的时候提：没配交换的机器上"交换 0 B"会被读成"快用完了"。
          note: "已用 " + pair[0] + " / 共 " + pair[1] +
            (h.swapTotalBytes > 0 ? " · 交换 " + bytesPair(h.swapUsedBytes, h.swapTotalBytes)[0] : ""),
        });
      } else {
        out.push({ key: "mem", label: "内存", icon: "cube", percent: null, note: h.memNote || "这一项没采到" });
      }

      if (h.diskTotalBytes > 0) {
        out.push({
          key: "disk", label: "存档盘", icon: "archive", percent: h.diskPercent,
          // 这块盘是面板在读的那个存档目录所在的分隔区——多个分区时不说清楚就会看错盘。
          note: "还剩 " + bytes(h.diskFreeBytes) + " / 共 " + bytes(h.diskTotalBytes),
          hint: "统计的是存档目录所在的那块盘：" + h.diskPath,
        });
      } else {
        out.push({ key: "disk", label: "存档盘", icon: "archive", percent: null, note: h.diskNote || "这一项没采到" });
      }
      return out;
    },
    hostRows() {
      const h = state.host;
      if (!h) return [];
      const rows = [];
      // 型号排在最前面：它是"这台机器是什么"，其余几行都是"它现在怎么样"。
      // 读不到时不留空格也不写"未知"两个字了事，接口那句为什么读不到要原样上屏。
      rows.push({
        k: "处理器型号",
        v: h.cpuModel ? (h.cpuModel + " · " + h.cpuCores + " 核") : (h.cpuModelNote || "这一项没读到"),
        hint: "型号是这台机器自己上报的原文，面板只去掉了厂商那些注册商标标记。" +
          "核数是面板能用上的逻辑核数，和上面那圈百分比同一个分母：这台机器 " + h.cpuCores + " 核，" +
          "饥荒服务端跑满一个核时，整机那一格显示的是 " + (Math.round(1000 / h.cpuCores) / 10) + "%。",
      });
      rows.push({
        k: "网络",
        v: h.netNote ? h.netNote : ("收 " + bytes(Math.round(h.netRxBytesPerSecond)) + "/s · 发 " +
          bytes(Math.round(h.netTxBytesPerSecond)) + "/s"),
      });
      const loadRow = {
        k: "平均负载",
        v: h.loadNote ? h.loadNote : (h.load1.toFixed(2) + " / " + h.load5.toFixed(2) + " / " + h.load15.toFixed(2)),
        warn: !h.loadNote && h.cpuCores > 0 && h.load1 > h.cpuCores,
      };
      // 那句话解释的是"三个裸数字"，所以只有真摆出三个数字时才带它。
      // 这个系统没有这一项时（Windows 开发机）上面已经是那句中文说明，
      // 再挂一颗"三个数依次是……"的问号就在解释一组屏上根本不存在的东西。
      if (!h.loadNote) {
        // 对照物就是核数，所以那句话里必须把这台机器的核数写进去，不是写"和核数比"。
        loadRow.hint = "三个数依次是最近 1 分钟 / 5 分钟 / 15 分钟里，平均有多少件事正在 CPU 上跑着、" +
          "或者排队等它。这个数要拿核数看：" + h.cpuCores + " 核的机器上，1 分钟那一格接近 " +
          h.cpuCores + " 就是排满，超过就是有事在等。" +
          "三个一起看才准：只有第一格高是刚来的一阵风，三格都高才是持续吃紧。";
      }
      rows.push(loadRow);
      rows.push({ k: "面板自身（Go 堆）",
        v: bytes(h.panelHeapSysBytes || h.panelRssBytes),
        hint: "panelRssBytes 是历史字段，实际取 runtime.MemStats.Sys（Go 运行时向系统申请的内存，不是操作系统意义上的常驻集 RSS）；" +
          "语义准确的同一份数据在 panelHeapSysBytes，当前两者同值。不含饥荒服务端进程。" });
      rows.push({ k: "开机时长", v: h.uptimeNote ? h.uptimeNote : seconds(h.uptimeSeconds) });
      rows.push({ k: "数据时间", v: rel(h.collectedAt) });
      return rows;
    },
  },
  watch: {
    // 清单取数搬去了 store（登录与轮询都会跑）。这里保留 immediate watcher 兜住
    // "总览先挂载、state.me 后回来"那一档：store.loadChecklist 自带 loading 去重，
    // 两下撞在一起只会扫一次盘。
    isAdmin: {
      handler(v) { if (v) syncChecklist(); },
      immediate: true,
    },
    // 从别页跳过来时把「下一步」滚进视野并闪一下；hash 里带着 focus=todo，
    // 刷新、前进后退都能复现同一屏。
    focusQuery(v) { if (v === "todo") this.focusSteps(); },
    // 选中房间一变就换这两张图的数据：留着上一个房间的数字会让人以为它的在线在掉。
    selCluster(v) { this.loadTrend(v); this.loadTimeline(v); },
    // 清单的轮询由 store.refreshAll 统一每四次跑（约 20 秒一次磁盘扫描），
    // 页面不再自己记 tick——否则切到别的页时顶栏数字就停在旧快照上。
    // 两张图跟着轮询走，但走 silent：时间线上"这会儿还在里面"那一根条是画到
    // 查询这一刻的，不跟着刷新就等于把三分钟前的人当成现在的人，
    // 而刚进服的人也会一直不出现（要等人去点一下页签）。
    lastSync() {
      this.loadTrend(state.sel.cluster, true);
      this.loadTimeline(state.sel.cluster, true);
    },
  },
  mounted() {
    this.loadTrend(state.sel.cluster);
    this.loadTimeline(state.sel.cluster);
    // 清单的首次取数由 store.loadSession / 轮询负责，上面那个 immediate watcher 兜底
    // "总览先挂载、state.me 后回来"的情况（state.me 可能还没回来）。
    // 直接带着 focus=todo 打开总览（收藏夹、刷新、新标签页）也要落在那一栏上。
    if (queryOf("focus") === "todo") this.focusSteps();
  },
  methods: {
    // 一个房间里在跑的世界数，页签那颗点和后面那个分数用它算。
    liveCount(c) { return (c.shards || []).filter((s) => s.running).length; },
    tabTitle(c) {
      const live = this.liveCount(c);
      return "存档目录名：" + c.name + " · " +
        (live ? "在跑 " + live + "/" + c.shards.length + " 个世界" : "这个房间的世界现在都没在跑");
    },
    // 卡头「重新核对」按下去只调全局那一次取数：seq 防晚到、loading 防并发
    // 都在 store.loadChecklist 里，页面不再各写一份。
    loadChecklist() { return syncChecklist(); },
    jump(href) { location.hash = href; },
    focusSteps() {
      this.focused = true;
      this.$nextTick(() => {
        const el = document.getElementById("next-steps");
        // 这张卡现在就在页顶：已经在视野里就别再滚，"center" 会把首屏往下推一截。
        if (el) el.scrollIntoView({ block: "nearest", behavior: "smooth" });
      });
      clearTimeout(this._focusTimer);
      this._focusTimer = setTimeout(() => { this.focused = false; }, 2600);
    },
    async loadTrend(cluster, silent) {
      if (!cluster) { this.trend.points = []; this.trend.meta = null; this.trend.error = ""; return; }
      if (!silent) this.trend.loading = true;
      this.trend.cluster = cluster;
      const hoursAt = this.trend.hours;
      const res = await api.get("/api/v1/stats" + api.qs({ cluster, hours: hoursAt }));
      this.trend.loading = false;
      // 请求期间用户可能又切了房间或换了窗口：晚到的那份必须丢掉，否则图会跳回上一个房间。
      if (this.trend.cluster !== state.sel.cluster || this.trend.hours !== hoursAt) return;
      if (res.ok) {
        const d = res.data || {};
        // 后端已经按时间窗把每个世界的采样相加归好了，这里只管画，不再自己归并：
        // 前端一旦也做一遍归并，两处对"多少个点、每点几分钟"的说法迟早会分家。
        this.trend.points = d.points || [];
        this.trend.meta = d;
        this.trend.error = "";
      } else { this.trend.points = []; this.trend.meta = null; this.trend.error = res.error || ("接口返回 " + res.status); }
    },
    async loadTimeline(cluster, silent) {
      if (!cluster) { this.timeline.data = null; this.timeline.error = ""; return; }
      const seq = (this.timeline.seq = (this.timeline.seq || 0) + 1);
      if (!silent) this.timeline.loading = true;
      const hoursAt = this.trend.hours;
      const res = await api.get("/api/v1/player-timeline" + api.qs({ cluster, hours: hoursAt }));
      this.timeline.loading = false;
      if (seq !== this.timeline.seq) return;
      if (res.ok) {
        this.timeline.data = res.data || null;
        this.timeline.error = "";
      } else { this.timeline.data = null; this.timeline.error = res.error || ("接口返回 " + res.status); }
    },
    pick(c) { state.sel.cluster = c; },
    setHours(h) {
      const n = Number(h);
      if (!Number.isFinite(n) || n === this.trend.hours) return;
      this.trend.hours = n;
      // 窗口是这一栏唯一的筛选，两张图共用它，所以换了就要两张都重问：
      // 留着的还是上一个窗口的点，而标题已经写成新窗口了，那就是在拿旧数据讲新故事。
      this.loadTrend(state.sel.cluster);
      this.loadTimeline(state.sel.cluster);
    },
    // 点房间名取加入命令：这一屏是用户最常停下来的地方，"喊人来玩"的入口就该摆在眼前，
    // 而不是要他先去某一页把房间列出来。
    join(c) { openConnect(c.name, c.roomName || c.name); },
    // 整串类名在这里拼，模板里只留一个函数调用：模板里写 'bh-' + … 的话，静态审计会把
    // 这半截串当一个叫 .bh- 的类去查样式——它只看字面量，看不出拼完是什么。
    behClass(e) { return "bh-" + (BH_TONE[e.code] || "slate"); },
    // 条长按最多的那一类归一（不是按总数），所以只有一类时那条也是满格——
    // 这一行的长短说的是"谁比谁多"，不是"占了几成"，悬停那句和卡里那句说明都这么说。
    behWidth(e) {
      if (!this.behMax) return "0%";
      return Math.max(6, Math.round((e.count || 0) / this.behMax * 100)) + "%";
    },
    behTitle(e) {
      return e.name + " " + e.count + " 次 · " + (e.label || "这一类行为在这一窗里发生的次数");
    },
  },
  template: `
    <div class="col" style="gap:16px">
      <x-note v-if="state.clustersError" kind="bad">
        <b>房间列表没拿到。</b>{{ state.clustersError }}
      </x-note>

      <!-- 「下一步」住在页顶：2026-11-06 用户拍板"这一块是针对整个机器而言的"，
           2026-11-09 又确认按整机级 / 诊断级分开排——它会催人做事，得在首屏；
           「这台机器」是出问题时才查的诊断卡，挪到整页最下面。
           判据与页签选中的那一间无关，所以绝不能挂在房间区下面。
           之前靠 CSS order 把房间排到它前面（名义页顶、实际底部），这轮去掉：
           DOM 顺序与视觉顺序一致，键盘 Tab 和读屏才不会反着走。 -->
      <!-- 全站唯一说待办的地方：别的页不再铺提醒块，顶栏那枚「待办」点进来就落在这儿。
           只读账号也看得见（安全提醒不挑角色），只是行尾改成一句「需要管理员处理」。
           没有待办时整张卡收成一行「都齐了」（.is-quiet），不在首屏白占一块。 -->
      <section class="card next-steps" id="next-steps"
               :class="[focused ? 'focus-flash' : '', checkItems.length ? '' : 'is-quiet']">
        <div class="card-head" v-if="checkItems.length">
          <span class="ico-tile sm tint-azure"><x-icon name="clipboard" :size="15"></x-icon></span>
          <h3>下一步</h3>
          <!-- 时间和按钮算一组：窄屏折行时它俩一起挪到第二行，
               而不是留那颗按钮孤零零在左边，看着像没排好。 -->
          <span class="head-actions">
            <!-- 不用 .chip：验收脚本按 .card-head .chip 的第一枚读"核对于"，
                 插到它前面会把那句判据读成待办数。换一个类名，语义上也不冲突。 -->
            <span class="head-count" v-if="checkItems.length">{{ checkItems.length }} 项待办</span>
            <span class="chip mono" v-if="check.at">核对于 {{ rel(check.at) }}</span>
            <button class="btn sm ghost" v-if="isAdmin" @click="loadChecklist">
              {{ check.loading ? '核对中……' : '重新核对' }}</button>
          </span>
        </div>

        <!-- 密码、本体这两条不依赖 /rooms：核对没回来时骨架屏不把它们一起吞掉，
             核对失败时错误框也只占一行，清单照旧摊在下面。 -->
        <x-sk v-if="check.loading && !check.data && !checkItems.length" :lines="2"></x-sk>

        <template v-else>
          <div class="err-box" v-if="check.error">这一步没核对成：{{ check.error }}</div>
          <div class="steps-list">
            <div class="step-row" v-for="it in checkItems" :key="it.key">
              <span class="ico-tile sm" :class="it.tint"><x-icon :name="it.icon" :size="15"></x-icon></span>
              <span class="step-tag" v-if="it.tag">{{ it.tag }}</span>
              <p class="grow">{{ it.text }}</p>
              <!-- 动作那一格三种情况共用同一行：管理员给按钮、只读账号给一句说明、
                   已经在目标页时给位置提示。不另画第二种卡片或横幅。 -->
              <button class="btn sm" v-if="isAdmin && it.btn" @click="jump(it.href)">{{ it.btn }}</button>
              <span class="step-tail" v-else-if="!isAdmin">需要管理员处理</span>
              <span class="step-tail" v-else>{{ it.tail }}</span>
            </div>
            <div class="step-row ok" v-if="!checkItems.length && !check.error">
              <span class="ico-tile sm tint-ok"><x-icon name="check" :size="15"></x-icon></span>
              <p class="grow">{{ isAdmin ? '房间、令牌、助手回传、备份这四样现在都齐了，没有卡住的一步。' : '现在没有要提醒你的事。' }}</p>
            </div>
          </div>
        </template>
      </section>

      <!-- 房间与世界状态：紧跟在页顶那份「下一步」之后，进首屏就能看到实况。
           「这台机器」那张诊断卡挪到整页最下面（见模板末尾）：数值密、低频，
           不该和"服在不在跑、几个人在线"抢首屏。外层 .col 是 flex column，
           这一版不再用 CSS order 重排，DOM 顺序就是视觉顺序。 -->
      <div class="grid grid-wide">
        <!-- 「房间」这行标题是 grid 的直接孩子、横跨两列，不放在左列里面：
             放里面就把左列首卡推下去 43px，右列那张卡的顶边比它高一截，
             整页看着就是各板块没对齐（2026-11-04 实测 rect：左 567 / 右 524）。
             页签条同理，也必须横跨两列，否则它会占掉左列第一格、把两张卡错开一行。 -->
        <div class="section-title">
          <h2>房间</h2>
          <span class="sub">{{ state.clusters.length }} 个，其中 {{ runningClusters }} 个有世界在跑</span>
          <span class="grow"></span>
        </div>

        <!-- 一次只看一间（2026-11-04 用户提的）：以前 N 张房间卡纵向排下来，
             第四间以后要滚动才看得见，而同一时刻真正盯的只有一间。
             名字前面那颗点是"该点哪一枚"的快答案（绿=有世界在跑），后面那个分数给准数。
             默认选中哪一间由 store 的 pickDefaultCluster 定：优先挑有世界在跑的那间。 -->
        <div class="tabs room-tabs" v-if="state.clusters.length">
          <button v-for="c in state.clusters" :key="c.name"
                  :class="state.sel.cluster === c.name ? 'on' : ''"
                  :title="tabTitle(c)" @click="pick(c.name)">
            <span class="tab-dot" :class="liveCount(c) ? 'run' : 'idle'"></span>
            <span>{{ c.roomName || c.name }}</span>
            <span class="tab-count">{{ liveCount(c) }}/{{ (c.shards || []).length }}</span>
          </button>
        </div>

        <div class="col" style="gap:14px">
          <x-sk v-if="!state.clustersLoaded"></x-sk>

          <x-empty v-else-if="!state.clusters.length" icon="world" title="这台机器上还没有房间"
            note="面板只看这台机器上的存档文件夹。里面一个房间都没有，要么是从没开过服，要么是面板指错了文件夹。"
          >
            <button class="btn sm primary" @click="go('room')">去建一个房间</button>
            <button class="btn sm" @click="go('settings')">确认面板读的是哪个目录</button>
            <x-hint text="要建房间，这台机器上得先有饥荒联机版的专用服务端；面板默认读 ~/.klei/DoNotStarveTogether，路径不对就在「面板设置」里改 kleiRoot。"></x-hint>
          </x-empty>

          <section class="card tinted" v-else-if="room">
            <div class="card-head">
              <span class="ico-tile sm" :class="liveCount(room) ? 'tint-ok' : 'tint-slate'">
                <x-icon name="house" :size="15"></x-icon></span>
              <h2><button class="room-name" @click="join(room)"
                          title="点这里取加入这个房间的命令：复制到游戏控制台粘贴就能进">
                <span>{{ room.roomName || room.name }}</span><i class="ic-box"><x-icon name="copy" :size="13"></x-icon></i>
              </button></h2>
              <span class="chip hide-sm mono" v-if="room.roomName && room.roomName !== room.name">{{ room.name }}</span>
              <span class="chip hide-sm">{{ (room.shards || []).length }} 个世界</span>
              <!-- 归属这一枚在窄屏上不 hide-sm：成员第一眼要弄明白的正是"这堆按钮为什么没了"，
                   而这里没有别的字能回答它。说法整个来自后端（"未指派" / "归 bob 管"）。 -->
              <span class="chip" :class="ownerChip(room).tone" :title="ownerChip(room).hint">
                <x-icon :name="ownerChip(room).icon" :size="12"></x-icon>
                <span>{{ ownerChip(room).label }}</span></span>
              <!-- 最近备份（#129 从「所有房间」那一页搬过来的）。快照按房间打，所以它挂在
                   房间卡头而不是某个世界行上；那一枚「未备份」是有结论的说法，
                   取数没回来时整枚不出现。 -->
              <span class="chip" v-if="backupChip(room.name)"
                    :class="backupChip(room.name).tone" :title="backupChip(room.name).hint">
                <x-icon name="archive" :size="12"></x-icon>
                <span>{{ backupChip(room.name).text }}</span></span>
              <x-badge v-if="liveCount(room)" kind="running"
                       :text="'在跑 ' + liveCount(room) + '/' + room.shards.length"></x-badge>
              <x-badge v-else kind="idle" text="都没在跑"></x-badge>
              <span class="grow"></span>
              <button class="btn sm ghost" v-if="canAssign" @click="openOwner(room.name, room.roomName)"
                      title="决定这间房归谁管：派出去之后那位成员就能自己启停、改配置">
                <x-icon name="user" :size="13"></x-icon>指派</button>
            </div>
            <x-shard-row v-for="s in (room.shards || [])" :key="s.name" :cluster="room.name" :shard="s"></x-shard-row>
          </section>
        </div>

        <!-- 这一列两张卡：上面是趋势图，下面是「行为构成」。
             「下一步」原来也住在这里，#124 用户把它要回了页顶——那张清单的判据是整机的事。
             列本身不挂 v-if：没房间时两张卡各自让位，这一列跟着空掉，不影响别处。
             这一列和左边那张房间卡齐高（app.css 里 .grid-wide > aside 那条已撤），
             富余由下面那张卡的行距吃，不吃上面那张图的图场。 -->
        <aside class="col" style="gap:14px">
          <!-- 「在线情况」与「行为构成」各占一张卡（2026-11-04 用户指出"在线时间图该和在线情况分开"，
               2026-11-06 #125 又把那张逐人泳道搬去了「玩家管理」：总览是第一眼，回答"几个人、什么形状"，
               "分别是谁、各待了多久"这种明细放到玩家那一页去）。
               搬走之后这一列只剩两张卡，时间窗控件仍然只有上面这一枚，下面那张不另配一套，
               只把当前这一窗原样写回自己的标题里：读者不用记住刚才选的是近 6 小时。 -->
          <section class="card ov-insight ov-trend" v-if="state.clusters.length">
            <div class="card-head">
              <span class="ico-tile sm tint-teal"><x-icon name="pulse" :size="15"></x-icon></span>
              <h3>在线情况</h3>
              <span class="grow"></span>
              <x-badge v-if="trendFresh" :kind="trendFresh.kind" :text="trendFresh.text"></x-badge>
              <span class="chip" v-if="trend.cluster" :title="'存档目录名：' + trend.cluster">{{ trendRoom }}</span>
            </div>

            <div class="row ov-when">
              <x-seg :options="trendWindows" :model-value="trend.hours"
                     label="这一列两张图共用一个时间窗，选一次两边一起换"
                     @update="setHours"></x-seg>
              <span class="chip" v-if="trend.meta && trend.meta.bucketLabel">{{ trend.meta.bucketLabel }}</span>
              <span class="chip mono" v-if="trend.meta">{{ trend.points.length }} 点 / {{ trend.meta.samples }} 条采样</span>
              <!-- "出现过几个人"和那条线的"峰值几人"都要，因为它们不是一件事：
                   六个人轮着来、每次只来一个，那条线全程在 1，这个数是 6。
                   数取自同一次 player-timeline（和下面那张「行为构成」同一个分母），不另发一次问数。 -->
              <span class="chip mono" v-if="tl"
                    title="这一窗里露过面的人数，和上面那条线的峰值不是一回事：峰值是同一时刻最多几个人，这里是把人头数一遍。逐个人待了多久看「玩家管理」页">这一窗出现过 {{ tlPeople.length }} 人</span>
            </div>

            <x-sk v-if="trend.loading" :lines="2"></x-sk>
            <div class="err-box" v-else-if="trend.error">{{ trend.error }}</div>
            <x-spark v-else-if="trend.points.length" :points="trend.points"></x-spark>
            <div class="empty" v-else>
              <div class="e-title">暂无在线记录</div>
            </div>
          </section>

          <!-- 行为构成（#124 用户在四版对照里选定这一版）：这一间房、这一个窗口里
               进服 / 离开 / 死亡 / 幽灵消散 / 被复活各发生了多少次。
               数据是上面那次时间线请求一起带回来的 events，这里不另发一次问数——
               这一屏两处（这张卡的五行、上面那句"这一窗出现过 N 人"）共用同一份回答，
               逐人泳道在「玩家管理」页读的也是同一个接口。 -->
          <section class="card ov-insight ov-behaviour" v-if="state.clusters.length">
            <div class="card-head">
              <span class="ico-tile sm tint-azure"><x-icon name="layers" :size="15"></x-icon></span>
              <h3>行为构成</h3>
              <span class="grow"></span>
              <span class="chip" :title="'和上面那张图同一个窗口，切换在上面那排按钮'">{{ hoursLabel }}</span>
              <span class="chip mono" v-if="tl"
                    title="这几行的次数加起来就是这个数：同一条公告两个世界各写一遍的算一次">{{ tl.rows }} 次行为</span>
            </div>

            <x-sk v-if="timeline.loading" :lines="3"></x-sk>
            <div class="err-box" v-else-if="timeline.error">{{ timeline.error }}</div>
            <div class="bh-list" v-else-if="behRows.length">
              <div class="bh-line" v-for="e in behRows" :key="e.code"
                   :class="behClass(e)" :title="behTitle(e)">
                <span class="bh-k"><i></i>{{ e.name }}</span>
                <span class="bh-bar"><b :style="{ width: behWidth(e) }"></b></span>
                <span class="bh-v num">{{ e.count }}</span>
              </div>
            </div>
            <div class="empty" v-else-if="tl">
              <div class="e-title">暂无行为记录</div>
            </div>
            <x-sk v-else :lines="3"></x-sk>
          </section>

        </aside>
      </div>

      <!-- 这台机器：出问题时才查的诊断卡，所以留在整页最后（2026-11-09 用户拍板）。
           一张卡横跨整幅：.host-row 在 CSS 里写成单列，三枚环与那列绝对量按整幅宽度均分。
           页头那枚「这些数怎么来的」和卡底那行常驻「口径」都按同一轮要求删掉了；
           指标本身与每行的悬停解释还在，想细究口径去看接口或文档。 -->
      <div class="host-wrap">
        <div class="grid grid-wide host-row">
          <section class="card host-strip">
            <div class="card-head">
              <span class="ico-tile sm tint-azure"><x-icon name="servers" :size="15"></x-icon></span>
              <h3>这台机器</h3>
              <span class="grow"></span>
              <span class="head-actions">
                <button class="btn sm ghost" @click="hostOpen = !hostOpen">{{ hostOpen ? '收起指标' : '展开指标' }}</button>
                <button class="btn sm ghost" @click="refreshAll()">立即同步</button>
              </span>
            </div>

            <template v-if="hostOpen">
              <div class="err-box" v-if="state.hostError">{{ state.hostError }}</div>

              <div class="host-body" v-else-if="host">
                <div class="host-rings">
                  <x-ring v-for="r in rings" :key="r.key" :label="r.label" :icon="r.icon"
                          :percent="r.percent" :note="r.note" :hint="r.hint"></x-ring>
                </div>
                <dl class="kv host-kv">
                  <template v-for="r in hostRows" :key="r.k">
                    <dt>{{ r.k }}</dt>
                    <dd :class="r.warn ? 'is-warn' : ''">{{ r.v }}<x-hint v-if="r.hint" :text="r.hint"></x-hint></dd>
                  </template>
                </dl>
              </div>

              <x-sk v-else :lines="2"></x-sk>
            </template>
            <div class="hint" v-else>
              主机指标已收起（窄屏默认折叠）。点「展开指标」看 CPU / 内存 / 存档盘与明细。
            </div>
          </section>
        </div>
      </div>
    </div>`,
};
