// 房间页：开一个新房间、填 Klei 令牌、改世界设定。
//
// 这三块放在同一页，是因为它们都发生在"分片还没起来之前"这一段：
// 向导建目录、令牌决定这个房间能不能上 Klei 的服务器列表、世界设定决定生成出来
// 是一张什么图。拆成三个导航项的话，用户建完房间要自己找"下一步去哪填令牌"。
import * as api from "../api.js";
import { state, act, actMany, pushUndo, askConfirm, isBusy, rel, assignedText } from "../store.js";
import { keyOf, num, bytes, dateTime } from "../fmt.js";
import { go, route, setQuery } from "../router.js";
import { openConnect } from "../connect.js";
import { openOwner } from "../owner.js";
import { RoomAccess } from "../access.js";
// 地表/洞穴的叫法走 gameinfo.js 那一份：这一页以前自己抄了一张表，于是同一个页面里
// "世界规则"的标题还写着 Master，指令台却已经说地表了。
import { placeName } from "../gameinfo.js";
// 世界规则那 189 项的图标查表：查表逻辑单独一个文件，因为它是纯数据，
// 而这一页已经有 1600 行了——再往里塞两张百来行的映射表，改图标的人得先读懂开服向导。
import { fieldIcon, groupTint, groupIcon } from "../worldicons.js";

// 五个页签里有四个在"没有房间可指"时停在同一栏空状态，说法共用这一句。
// 为什么只有一种说法：房间列表一非空，store 就自动选中一个（见 store.js 的
// applyClusters），"有房间但没选"这个状态在界面上到不了；而一间都没有时页顶那排
// 房间下拉整排收掉了（收掉的道理见页面模板），说"先在上面的下拉里选一个"就是
// 让人去点一个不在屏上的控件。
function noRoomHint() {
  return "这台机器上还没有房间，所以这几个页签都还没有内容：切到「开服向导」建第一个。";
}

// ---- 未提交草稿：切页签会销毁这组 v-if 子组件，靠 sessionStorage 把它接回来 ----
//
// 按房间名分键：在 A 房间填一半、切到 B 房间，不该把 A 的草稿灌进 B 的表单。
// 密码和 Klei 令牌一律不落盘（见各组件 persist 里挑字段那份白名单）：
// sessionStorage 在同一个标签页里是明文可读的，凭据不进这里。
const DRAFT_NS = "dstpanel.room.draft.";
function draftKey(kind, name) { return DRAFT_NS + kind + "." + (name || "_"); }
function readDraft(kind, name) {
  try { const raw = sessionStorage.getItem(draftKey(kind, name)); return raw ? JSON.parse(raw) : null; }
  catch (e) { return null; }
}
function writeDraft(kind, name, obj) {
  try { sessionStorage.setItem(draftKey(kind, name), JSON.stringify(obj)); } catch (e) { /* 忽略配额/禁用 */ }
}
function clearDraft(kind, name) {
  try { sessionStorage.removeItem(draftKey(kind, name)); } catch (e) { /* 忽略 */ }
}

// 后端那句中文错误挂到最像的字段上，用 x-field-error 显示在输入框下面；
// 猜不准时落到表单级 "_form"，总之不留"整表只有一个 err-box"那种没有出口的报错。
// 后端原文另外进 errNote（x-note），act 内部那条 Toast 由 store 统一弹，这里不重复弹。
function attachError(vm, res, defs) {
  const text = (res && res.error) || "接口没给原因";
  vm.errNote = text;
  const hit = (defs || []).find((d) => d.keys.some((k) => text.indexOf(k) >= 0));
  vm.fieldErrors = { [hit ? hit.field : "_form"]: text };
}
function clearFieldErrors(vm) { vm.fieldErrors = {}; vm.errNote = ""; }

// 下面这几张表的后端对照物是 internal/api/bootstrap.go 里那几个白名单 map。
// value 必须一字不差（Klei 对认不出的取值不报错，只是静默按默认走），
// label 才是这一侧的中文；界面提交的一律是 value，用户看到的只有 label。
const GAME_MODES = [
  { value: "survival", label: "生存", note: "死了就结束，世界随团灭重置" },
  { value: "endless", label: "无尽", note: "死了在肉块雕像上复活，能一直建家" },
  { value: "wilderness", label: "荒野", note: "一次性地图" },
];
const INTENTIONS = [
  { value: "cooperative", label: "合作" },
  { value: "competitive", label: "竞争" },
  { value: "social", label: "社交" },
  { value: "madness", label: "疯狂" },
];
const LANGUAGES = [
  { value: "zh", label: "中文" },
  { value: "en", label: "英文" },
];
// tickRate 是"服务端每秒和客户端通信几次"，越高越顺、越吃 CPU。
// 4G 小机跑双分片时 30 会明显抢 CPU，所以默认停在 15，并把这句摆在旁边。
const TICK_RATES = [
  { value: "15", label: "15 次/秒", note: "默认值，4G 两个世界建议留这档" },
  { value: "20", label: "20 次/秒" },
  { value: "30", label: "30 次/秒", note: "手感最顺，也最吃 CPU" },
];

// 向导自己的默认值。这一份和后端 bootstrap.go 里 toSettings 的默认值必须一致
// （endless / cooperative / 6 人 / PVP 关）——界面写着默认、发出去是另一套的话，
// 用户按界面那套理解自己的服务器，实际起的是另一间。
const WIZARD_DEFAULTS = { gameMode: "endless", intention: "cooperative", maxPlayers: 6, pvp: false };

// 世界设定字典和房间无关，整台机器就一份，所以缓存在模块级。
// 存的是 promise 而不是结果：两个组件同时挂载时只打一次接口；失败要能重来，所以失败即清。
let schemaPromise = null;
function loadSchema() {
  if (!schemaPromise) {
    schemaPromise = api.get("/api/v1/world-schema").then((res) => {
      if (res.ok) return { data: res.data || [], error: "" };
      schemaPromise = null;
      return { data: [], error: res.error || ("接口返回 " + res.status) };
    });
  }
  return schemaPromise;
}

// ---- 开服向导 ----

const ROOM_TABS = ["wizard", "cmd", "world", "cluster", "ports", "token"];

// 页签可以由地址带进来（总览的「下一步」要能一步落到 Klei 令牌那一栏，而不是让用户自己找）。
// 认不出的参数仍落在向导：拼错的地址不该把人丢在一页空白上。
// 优先吃组件收到的 query prop（hash 前进/后退时它才是刚变的那个），其次才是全局 route.query。
function tabFromUrl(q) {
  const t = String((q && q.tab) || (route.query && route.query.tab) || "");
  return ROOM_TABS.indexOf(t) >= 0 ? t : "wizard";
}

// 向导草稿里允许落盘的字段：密码/令牌这两项明文不进 sessionStorage。
const WIZARD_DRAFT_KEYS = [
  "name", "numShards", "gamePort", "displayName", "description",
  "gameMode", "intention", "maxPlayers", "pvp", "pauseWhenEmpty", "lanOnly",
  "offlineCluster", "language", "tickRate", "consoleEnabled", "writeWorld",
  "presetMaster", "presetCaves", "installHelper",
];
// 后端中文错误 → 字段的启发式映射：命中哪个关键词就挂到哪个输入框下面。
const WIZARD_ERR_DEFS = [
  { field: "displayName", keys: ["名称", "服务器名", "cluster_name"] },
  { field: "gamePort", keys: ["端口", "port"] },
  { field: "name", keys: ["目录名", "目录"] },
  { field: "maxPlayers", keys: ["人数", "玩家数"] },
  { field: "token", keys: ["令牌", "token"] },
];
const CLUSTER_ERR_DEFS = [
  { field: "displayName", keys: ["名称", "服务器名"] },
  { field: "maxPlayers", keys: ["人数", "玩家数", "max_players"] },
  { field: "whitelistSlots", keys: ["白名单", "whitelist"] },
  { field: "description", keys: ["说明"] },
  { field: "_form", keys: ["密码", "password"] },
];
const PORTS_ERR_DEFS = [
  { field: "master", keys: ["地表", "master", "端口"] },
  { field: "caves", keys: ["洞穴", "caves"] },
];
const TOKEN_ERR_DEFS = [{ field: "input", keys: ["令牌", "token"] }];
const WORLD_ERR_DEFS = [
  { field: "preset", keys: ["预设", "preset"] },
  { field: "_form", keys: ["世界规则", "override", "校验"] },
];

const Wizard = {
  props: { schema: { type: Array, default: () => [] }, schemaError: String,
           query: { type: Object, default: () => ({}) } },
  emits: ["tab"],
  setup() { return { state, GAME_MODES, INTENTIONS, LANGUAGES, TICK_RATES }; },
  data() {
    return {
      step: 1,
      steps: ["房间与世界", "玩法", "世界规则", "令牌与助手"],
      // 走到过最远的那一步。步骤条只允许跳回这里以内：往前跳会绕过「下一步」里的校验，
      // 而最后一步的汇总要读前面几步的结果，跳过去就是一页空表格。
      furthest: 1,
      err: "",
      reply: null,
      fieldErrors: {}, errNote: "",
      // 端口预览：房间还没建，只能问"这台机器上哪些号被占了、面板会挑哪个"。
      // 有了这一份，用户填号时才不是蒙一个，也不会填到一半提交才发现撞车。
      plan: null, planErr: "",
      form: {
        name: "",
        numShards: 2,
        gamePort: "",
        displayName: "",
        description: "",
        password: "",
        gameMode: WIZARD_DEFAULTS.gameMode,
        intention: WIZARD_DEFAULTS.intention,
        maxPlayers: WIZARD_DEFAULTS.maxPlayers,
        pvp: WIZARD_DEFAULTS.pvp,
        pauseWhenEmpty: false,
        lanOnly: false,
        offlineCluster: false,
        language: "zh",
        tickRate: "15",
        consoleEnabled: true,
        writeWorld: true,
        presetMaster: "",
        presetCaves: "",
        token: "",
        installHelper: true,
      },
    };
  },
  created() {
    // 进页面先把未提交的草稿接回来：切页签会销毁这个 v-if 子组件，
    // 填过一半的字不该因为切走一次就消失。
    const d = readDraft("wizard", this.draftName());
    if (d) {
      if (d.form) WIZARD_DRAFT_KEYS.forEach((k) => {
        if (d.form[k] !== undefined) this.form[k] = d.form[k];
      });
      const s = Number(d.step);
      if (s >= 1 && s <= this.steps.length) {
        this.step = s;
        this.furthest = Math.max(this.furthest, s);
      }
    }
    // URL 里的 step 优先（刷新、前进后退要回到同一屏）：它比草稿更能代表上一次的落点。
    const qs = Number(this.query && this.query.step);
    if (qs >= 1 && qs <= this.steps.length) {
      this.step = qs;
      this.furthest = Math.max(this.furthest, qs);
    }
  },
  computed: {
    busy() { return !!state.busy["cluster.create"]; },
    portTaken() { return this.plan ? this.plan.taken || [] : []; },
    // 填号这件事唯一能免费拿到的检查就是"撞不撞车"，所以放在提交前说：
    // 后端也会拒，但要等第 4 步按下去才回得来，而那时人已经填完四屏了。
    portProblem() {
      const raw = String(this.form.gamePort).trim();
      if (!raw) return "";
      const p = Number(raw);
      if (!Number.isInteger(p) || p < 1024 || p > 65534) {
        return "游戏端口要是 1024 到 65534 之间的整数（最后一个号要留给配套端口）。留空则由面板挑。";
      }
      for (const o of this.portTaken) {
        if (o.port === p || o.port === p + 1) {
          return p + " 这组号用不了：" + o.port + " 已被 " + o.room + " 的" + o.world +
            "世界占着（它是那边的" + (o.kind === "intent" ? "配套端口" : "游戏端口") + "）。";
        }
      }
      return "";
    },
    // 填到一个空闲号时也要回一句：这一栏安静着，人分不清"没问题"和"没检查"。
    // 顺带把配套端口和防火墙一起说进去——这是全流程里唯一有上下文提它的地方。
    // 短句：这一栏在四列表单里只有 ~250px 宽，长句会挤成四行。
    portFree() {
      const raw = String(this.form.gamePort).trim();
      if (!raw || this.portProblem) return "";
      const p = Number(raw);
      return p + " 现在没人用。配套的 " + (p + 1) + " 也要一起放行，只放一个房间起得来但连不上。";
    },
    modeNote() { const m = GAME_MODES.find((x) => x.value === this.form.gameMode); return m ? m.note : ""; },
    tickNote() { const m = TICK_RATES.find((x) => x.value === this.form.tickRate); return m ? m.note : ""; },
    presetsMaster() { return this.presetsOf("Master"); },
    presetsCaves() { return this.presetsOf("Caves"); },
    tokenLen() { return this.form.token.trim().length; },
    // 端口那一行的汇总：把两个号都写出来，因为玩家照着的就是这两个号——
    // 云安全组只放行了游戏端口、没放配套端口时，房间起得来但列表里连不上。
    portSummary() {
      const gp = String(this.form.gamePort).trim();
      const auto = this.plan && this.plan.autoPort ? Number(this.plan.autoPort) : 0;
      const master = gp ? Number(gp) : auto;
      if (!master) return gp ? "地表 " + gp + "（号还没法确认，面板读不到这台机器的占用表）"
                             : "面板的默认区间已经排不出空闲号，要在端口一栏自己填一个";
      let v = "地表 " + master + " 与配套 " + (master + 1) + (gp ? "" : "（面板挑的）");
      if (Number(this.form.numShards) === 2) v += " · 洞穴的号由面板挑，建完可以在「游戏端口」页签改";
      return v;
    },
    // 汇总里要点名这一次会落进文件的预设。理由很实际：预设选了洞穴的那张表放到地表，
    // 后端会当场拒（那是它在保护你现在这张图），而"什么都没选、用默认"恰恰是最容易
    // 在看汇总时漏掉的一栏——只写"见上一步"等于让人再翻一屏。
    presetSummary() {
      const one = (shard, id) => {
        const hit = this.presetsOf(shard).find((p) => p.value === id);
        if (hit) return hit.label;
        const s = this.schema.find((x) => x.shard === shard);
        return s && s.defaultPreset && s.defaultPreset.label ? s.defaultPreset.label + "（该地形默认）" : "该地形默认";
      };
      const parts = ["地表 " + one("Master", this.form.presetMaster)];
      if (Number(this.form.numShards) === 2) parts.push("洞穴 " + one("Caves", this.form.presetCaves));
      return parts.join(" · ");
    },
    // 最后一步给一份汇总。这是唯一一处"把前面四步摊开看"的地方——建完才发现名字打错
    // 要删目录重来，代价比在这里多看一眼大得多。
    summary() {
      const f = this.form;
      return [
        { k: "世界", v: f.numShards === 2 ? "地表 + 洞穴（2 个）" : "只有地表（1 个）" },
        { k: "目录名", v: f.name.trim() || "留给面板挑下一个空号（Cluster_N）" },
        { k: "端口", v: this.portSummary },
        { k: "服务器名称", v: f.displayName || "（空，提交会被拒）" },
        { k: "模式 / 偏好", v: this.labelOf(GAME_MODES, f.gameMode) + " · " + this.labelOf(INTENTIONS, f.intention) },
        { k: "人数 / 通信", v: f.maxPlayers + " 人上限 · " + f.tickRate + " 次/秒 · PVP " + (f.pvp ? "开" : "关") },
        { k: "空服暂停", v: f.pauseWhenEmpty ? "没人时暂停世界" : "没人时继续跑（这样助手才会持续回传状态）" },
        { k: "世界规则", v: f.writeWorld ? "由面板写入 · " + this.presetSummary : "不写文件，交给引擎默认世界" },
        { k: "Klei 令牌", v: this.tokenLen ? "随这次一起写入（" + this.tokenLen + " 位）" : "这次不填" },
        { k: "助手 MOD", v: f.installHelper ? "装到每个世界" : "不装（面板将看不到在线人数与世界状态）" },
      ];
    },
  },
  methods: {
    labelOf(list, value) {
      const hit = list.find((o) => o.value === value);
      return hit ? hit.label : value;
    },
    presetsOf(shard) {
      const want = shard === "Caves" ? "cave" : "forest";
      const s = this.schema.find((x) => x.location === want);
      return s ? s.presets || [] : [];
    },
    back() { this.err = ""; this.step = Math.max(1, this.step - 1); },
    // 步骤条只负责"回去改"：填到第 3 步发现名字打错了，点一下就回去，不用连点两次上一步。
    // 没走到过的步骤点不动，理由写在 data 里那句注释上。
    goStep(n) {
      if (n > this.furthest || n === this.step) return;
      this.err = "";
      this.step = n;
    },
    // 端口预览：房间还不存在，问不了"这个房间的端口"，只能问这台机器现在的号。
    async loadPlan() {
      const res = await api.get("/api/v1/ports/plan?shards=" + Number(this.form.numShards));
      if (res.ok) { this.plan = res.data; this.planErr = ""; }
      else { this.plan = null; this.planErr = res.error || ("接口返回 " + res.status); }
    },
    next() {
      this.err = "";
      if (this.step === 1 && !this.form.displayName.trim()) {
        // 后端也会拒，但那句要到第 4 步提交才回得来；名称是这一步唯一的必填项，
        // 在这里就说，比让人填完四屏再被弹回去好。
        this.err = "服务器名称还没填：玩家在服务器列表里看到的就是它。";
        return;
      }
      if (this.step === 1 && this.portProblem) { this.err = this.portProblem; return; }
      this.step = Math.min(this.steps.length, this.step + 1);
      this.furthest = Math.max(this.furthest, this.step);
    },
    buildBody() {
      const f = this.form;
      const body = {
        numShards: Number(f.numShards),
        game: {
          displayName: f.displayName,
          description: f.description,
          password: f.password,
          gameMode: f.gameMode,
          intention: f.intention,
          maxPlayers: Number(f.maxPlayers),
          pvp: !!f.pvp,
          pauseWhenEmpty: !!f.pauseWhenEmpty,
          lanOnly: !!f.lanOnly,
          offlineCluster: !!f.offlineCluster,
          language: f.language,
          tickRate: Number(f.tickRate),
          consoleEnabled: !!f.consoleEnabled,
        },
        installHelper: !!f.installHelper,
      };
      // 目录名留空 = 整个键不发，让后端走 NextClusterName。发一个空串过去虽然结果一样，
      // 但那是把"我没填"说成"我填了个空的"，排查时两种语义要能分开。
      if (f.name.trim()) body.name = f.name.trim();
      // 端口同理：留空是"交给面板"，发 0 过去会被当成一个非法号。
      const gp = String(f.gamePort).trim();
      if (gp) body.gamePort = Number(gp);
      if (this.tokenLen) body.token = f.token.trim();
      if (f.writeWorld) {
        // 预设留空 = 不发这一项，由后端补该地形的默认预设。
        // 不替它填上，是为了让"我没选"和"我选了标准森林"在请求里区分得开。
        const presets = {};
        if (f.presetMaster) presets.Master = f.presetMaster;
        if (Number(f.numShards) === 2 && f.presetCaves) presets.Caves = f.presetCaves;
        body.world = { presets };
      }
      return body;
    },
    draftName() { return state.sel.cluster || "_new"; },
    // 表单每动一下就存一份；建完（reply 有值）之后不再写，免得刚清掉的草稿又长回来。
    persist() {
      if (this.reply) return;
      const form = {};
      WIZARD_DRAFT_KEYS.forEach((k) => { form[k] = this.form[k]; });
      writeDraft("wizard", this.draftName(), { form, step: this.step, furthest: this.furthest });
    },
    async submit() {
      this.err = "";
      clearFieldErrors(this);
      const dn = this.draftName();
      const res = await act("cluster.create", "POST", "/api/v1/clusters", this.buildBody(), {
        okTitle: "房间已经建好",
        okBody: (d) => (d && d.name ? d.name + "：" + (d.shards || []).length + " 个世界" : ""),
        badTitle: "这个房间没建成",
        onError: (r) => attachError(this, r, WIZARD_ERR_DEFS),
      });
      if (res.ok) {
        clearDraft("wizard", dn);           // 成功保存：草稿的使命结束了
        this.reply = res.data;
        this.form.token = "";                 // 明文不停在输入框里
        state.sel.cluster = res.data.name;     // 建完就切到新房间：令牌页签和世界页签指着的都是它
      }
      // 失败时后端原文已由 onError 挂到字段与 Note 上，不再塞回整表的 err-box。
    },
    again() {
      clearDraft("wizard", this.draftName());   // 用户点「再开一个」：清掉草稿
      clearFieldErrors(this);
      this.reply = null;
      this.step = 1;
      this.furthest = 1;
      this.err = "";
      ["name", "displayName", "description", "password", "token", "gamePort"].forEach((k) => { this.form[k] = ""; });
      // 上一间房间刚建好，它占的那几个号已经变了：不重读，建议号和"已占用"都是旧的。
      this.loadPlan();
    },
  },
  watch: {
    // 世界数量决定要占几个号，改了就得重新问一次建议号。
    "form.numShards"() { this.loadPlan(); },
    form: { deep: true, handler() { this.persist(); } },
    // 第几步写进 URL（setQuery 只改 hash 的 query，不重建组件），同一帧也存草稿。
    step(v) { setQuery({ step: v }); this.persist(); },
    // hash 前进/后退带来的 step 变化：path 没变、组件不重建，只能靠 watch 同步。
    "query.step"(v) {
      const n = Number(v) || 1;
      if (n >= 1 && n <= this.steps.length && n !== this.step) {
        this.step = n;
        this.furthest = Math.max(this.furthest, n);
      }
    },
  },
  mounted() { this.loadPlan(); },
  template: `
    <div class="col" style="gap:14px">
      <section class="card" v-if="!reply">
        <div class="stepper">
          <button v-for="(s, i) in steps" :key="s" type="button" class="stp"
                  :class="step === i + 1 ? 'on' : (step > i + 1 ? 'done' : 'todo')"
                  :disabled="i + 1 > furthest" :title="'第 ' + (i + 1) + ' 步：' + s"
                  @click="goStep(i + 1)">
            <span class="num">{{ step > i + 1 ? '✓' : i + 1 }}</span><span class="stp-name">{{ s }}</span>
          </button>
          <span class="stp-count">第 {{ step }} / {{ steps.length }} 步</span>
        </div>

        <!-- 第 1 步：目录与名片 -->
        <div class="grid-form" v-if="step === 1">
          <div class="field">
            <div class="field-head">
              <label>世界数量</label>
              <x-hint text="洞穴必须挂在 Master 上，没有「只开一个洞穴」这种房间；要洞穴就选第二个，两个世界一起建。"></x-hint>
            </div>
            <div class="seg">
              <button :class="form.numShards === 1 ? 'on' : ''" @click="form.numShards = 1">只有地表</button>
              <button :class="form.numShards === 2 ? 'on' : ''" @click="form.numShards = 2">地表 + 洞穴</button>
            </div>
          </div>
          <div class="field">
            <div class="field-head">
              <label>目录名（可留空）</label>
              <x-hint text="只认字母、数字、下划线、短横线，最多 32 个字符。这是磁盘上的目录名，不是玩家看到的名字。"></x-hint>
            </div>
            <input type="text" v-model="form.name" placeholder="留空则由面板取下一个空号，如 Cluster_3">
          </div>
          <div class="field">
            <div class="field-head">
              <label>游戏端口（可留空）</label>
              <x-hint text="玩家连进来用的是「服务器 IP : 这个号」。引擎每个世界还会同时绑紧跟它的那个号，两个都要在防火墙和云安全组放行。留空就由面板在默认区间里挑一个空闲号。"></x-hint>
            </div>
            <div class="row" style="gap:8px;align-items:flex-end">
              <input type="number" v-model="form.gamePort" class="grow"
                     :placeholder="plan && plan.autoPort ? '留空则用 ' + plan.autoPort : '留空由面板挑号'"
                     min="1024" max="65534">
              <button class="btn sm ghost" type="button" v-if="plan && plan.autoPort"
                      @click="form.gamePort = String(plan.autoPort)">用 {{ plan.autoPort }}</button>
            </div>
            <div class="row" style="gap:6px" v-if="plan && plan.taken && plan.taken.length">
              <span class="hint">已占用</span>
              <span class="chip mono" v-for="o in plan.taken" :key="o.port"
                    :title="o.room + ' 的' + o.world + '世界 · ' + o.kindLabel">{{ o.port }}</span>
            </div>
            <span class="hint" v-if="planErr">读不到占用表（{{ planErr }}）；这一栏可以先留空，建完再到「游戏端口」页签改。</span>
            <span class="hint" v-else-if="portProblem" style="color:var(--bad)">{{ portProblem }}</span>
            <span class="hint ok" v-else-if="portFree">{{ portFree }}</span>
            <span class="hint" v-else-if="plan && plan.note">{{ plan.note }}</span>
            <x-field-error :text="fieldErrors.gamePort"></x-field-error>
          </div>
          <div class="field">
            <div class="field-head">
              <label>服务器名称（必填）</label>
              <x-hint text="这一行会写进 cluster.ini 的 cluster_name，玩家在服务器列表上看到的就是它，最多 64 个字，不能带换行。"></x-hint>
            </div>
            <input type="text" v-model="form.displayName" placeholder="玩家列表上看到的那一行">
            <x-field-error :text="fieldErrors.displayName"></x-field-error>
          </div>
          <div class="field">
            <label>服务器说明</label>
            <input type="text" v-model="form.description" placeholder="一句话说明这是个什么局">
          </div>
          <div class="field">
            <div class="field-head">
              <label>连接密码（留空表示公开）</label>
              <x-hint text="名称、说明、密码这三项会一行一个键写进 cluster.ini，所以都不能带换行。这一栏不回显旧值，空着就是不加密码。"></x-hint>
            </div>
            <input type="password" v-model="form.password" autocomplete="new-password">
          </div>
        </div>

        <!-- 第 2 步：玩法 -->
        <div class="grid-form" v-else-if="step === 2">
          <div class="field">
            <div class="field-head">
              <label>游戏模式</label>
              <x-hint :text="modeNote"></x-hint>
            </div>
            <div class="seg">
              <button v-for="o in GAME_MODES" :key="o.value" :class="form.gameMode === o.value ? 'on' : ''"
                      @click="form.gameMode = o.value">{{ o.label }}</button>
            </div>
          </div>
          <div class="field">
            <div class="field-head">
              <label>游戏偏好</label>
              <x-hint text="只影响服务器列表上的标签，不改世界规则。"></x-hint>
            </div>
            <div class="seg">
              <button v-for="o in INTENTIONS" :key="o.value" :class="form.intention === o.value ? 'on' : ''"
                      @click="form.intention = o.value">{{ o.label }}</button>
            </div>
          </div>
          <div class="field">
            <div class="field-head">
              <label>最大玩家数</label>
              <x-hint text="服务端模板写明的上限是 16；填 0 不会变成「只有房主」，服务端会直接按默认走。"></x-hint>
            </div>
            <input type="number" v-model="form.maxPlayers" min="1" max="16">
          </div>
          <div class="field">
            <div class="field-head">
              <label>每秒通信次数</label>
              <x-hint :text="tickNote"></x-hint>
            </div>
            <div class="seg">
              <button v-for="o in TICK_RATES" :key="o.value" :class="form.tickRate === o.value ? 'on' : ''"
                      @click="form.tickRate = o.value">{{ o.label }}</button>
            </div>
          </div>
          <div class="field">
            <label>服务器语言</label>
            <div class="seg">
              <button v-for="o in LANGUAGES" :key="o.value" :class="form.language === o.value ? 'on' : ''"
                      @click="form.language = o.value">{{ o.label }}</button>
            </div>
          </div>
          <div class="field">
            <div class="field-head">
              <label>开关</label>
              <x-hint text="「没人时暂停」一开，助手 MOD 就不再回传状态，面板上会一直显示「数据已过期」，所以默认是关。「离线集群」一开公网玩家进不来，也不再需要 Klei 令牌。"></x-hint>
            </div>
            <label class="switch"><input type="checkbox" v-model="form.pvp"><span class="track"></span>开启 PVP</label>
            <label class="switch"><input type="checkbox" v-model="form.pauseWhenEmpty"><span class="track"></span>没人时暂停世界</label>
            <label class="switch"><input type="checkbox" v-model="form.consoleEnabled"><span class="track"></span>允许控制台命令</label>
            <label class="switch"><input type="checkbox" v-model="form.lanOnly"><span class="track"></span>只允许局域网</label>
            <label class="switch"><input type="checkbox" v-model="form.offlineCluster"><span class="track"></span>离线集群（不上 Klei 列表）</label>
          </div>
        </div>

        <!-- 第 3 步：世界 -->
        <div class="col" v-else-if="step === 3" style="gap:12px">
          <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap">
            <span class="card-title-sm">选一张预设就够了</span>
            <x-hint label="逐项设定在哪儿改" text="这一步只决定「用哪张预设」。逐项设定（世界大小、季节、牛群数量那些）在下面「世界规则」页签里改——那里一次能看上百项，向导里塞一份同样的表只会让人以为非填不可。"></x-hint>
            <span class="grow"></span>
            <button class="btn sm ghost" @click="$emit('tab', 'world')">去世界规则</button>
          </div>
          <label class="switch"><input type="checkbox" v-model="form.writeWorld"><span class="track"></span>由面板写入世界规则文件</label>
          <div class="grid-form" v-if="form.writeWorld">
            <div class="field">
              <div class="field-head">
                <label>地表预设</label>
                <x-hint text="留着不选就用这个地形自己的默认预设。"></x-hint>
              </div>
              <select v-model="form.presetMaster">
                <option value="">不选，用这个地形的默认预设</option>
                <option v-for="p in presetsMaster" :key="p.value" :value="p.value">{{ p.label }}</option>
              </select>
              <span class="hint" v-if="!presetsMaster.length">{{ schemaError || '字典还没拿到，这一项只能建完再到世界规则里改。' }}</span>
            </div>
            <div class="field" v-if="form.numShards === 2">
              <label>洞穴预设</label>
              <select v-model="form.presetCaves">
                <option value="">不选，用这个地形的默认预设</option>
                <option v-for="p in presetsCaves" :key="p.value" :value="p.value">{{ p.label }}</option>
              </select>
            </div>
          </div>
          <x-note v-else kind="warn">
            不写文件时引擎用它自己的默认世界，日志里是 <b>Not applying world gen overrides</b>。
            之后想改，回到这一步勾上，或者直接去世界规则页签。
          </x-note>
        </div>

        <!-- 第 4 步：令牌、助手与汇总 -->
        <div class="col" v-else style="gap:12px">
          <div class="field">
            <div class="field-head">
              <label>Klei 令牌（可以留空，之后在令牌页签补）</label>
              <x-hint text="令牌只写进 cluster_token.txt（权限 0600），不会出现在界面、日志或操作记录里；这一栏也不回显已存的内容。"></x-hint>
            </div>
            <input type="password" v-model="form.token" autocomplete="off"
                   placeholder="在 Klei 个人中心的 Don't Starve Together 服务器页面生成">
            <x-field-error :text="fieldErrors.token"></x-field-error>
            <span class="hint" v-if="tokenLen">现在已输入 {{ tokenLen }} 位。</span>
            <span class="hint warn" v-if="tokenLen > 0 && tokenLen < 40">
              这个长度不像一份完整令牌（通常 60 位以上），八成是粘贴时漏了头或尾。</span>
          </div>
          <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap">
            <label class="switch"><input type="checkbox" v-model="form.installHelper"><span class="track"></span>给每个世界装上助手 MOD</label>
            <x-hint label="不装会怎样" text="不装的话面板起得来这个房间，但看不到在线人数、世界天数，也发不了游戏命令。"></x-hint>
          </div>

          <div class="section-title mt"><h2>这次会建出来的东西</h2></div>
          <x-kv :rows="summary"></x-kv>
        </div>

        <div class="err-box mt" v-if="err">{{ err }}</div>
        <x-note class="mt" kind="warn" v-if="errNote">{{ errNote }}</x-note>
        <x-field-error :text="fieldErrors._form"></x-field-error>

        <div class="row spread mt" style="gap:10px">
          <button class="btn ghost" :disabled="step === 1 || busy" @click="back">上一步</button>
          <span class="grow"></span>
          <button class="btn primary" v-if="step < steps.length" @click="next">下一步</button>
          <button class="btn primary" v-else :disabled="busy" @click="submit">
            {{ busy ? '正在建目录……' : '创建房间' }}</button>
        </div>
      </section>

      <!-- 建完之后的这一段是"接下来会发生什么"：后端把结论一条条给过来，这里原样列出。 -->
      <section class="card" v-else>
        <div class="card-head">
          <span class="ico-tile tint-ok"><x-icon name="check" :size="16"></x-icon></span>
          <h2>{{ reply.name }} 已经建好</h2>
          <span class="chip mono">{{ reply.shards.length }} 个世界</span>
          <span class="grow"></span>
          <button class="btn sm ghost" @click="again">再开一个</button>
        </div>
        <div class="stat-note mono" style="word-break:break-all">{{ reply.dir }}</div>
        <div class="table-wrap mt">
          <table class="data">
            <thead><tr><th>世界</th><th>地形</th><th class="mono">端口</th><th>世界规则文件</th><th>助手 MOD</th></tr></thead>
            <tbody>
              <tr v-for="s in reply.shards" :key="s.shard">
                <td class="strong">{{ s.shard }}</td>
                <td>{{ s.locationLabel }}</td>
                <td class="mono">{{ s.port }}</td>
                <td>{{ s.worldFileWritten ? ('已写 · ' + (s.presetLabel || '文件里没写预设名')) : '未写，用引擎默认世界' }}</td>
                <td>{{ s.helperEnabled ? '已配置' : '未装' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <div class="note info mt">
          <b>接下来要知道的</b>
          <ul><li v-for="(n, i) in reply.notes" :key="i">{{ n }}</li></ul>
        </div>
        <div class="row mt" style="gap:8px;flex-wrap:wrap">
          <button class="btn primary" @click="$emit('tab', 'overview')">去总览开它</button>
          <button class="btn" @click="$emit('tab', 'token')">填令牌</button>
          <button class="btn ghost" @click="$emit('tab', 'world')">改世界规则</button>
        </div>
      </section>
    </div>`,
};

// ---- Klei 令牌 ----

const TokenCard = {
  props: { cluster: String },
  mixins: [RoomAccess],
  setup() { return { state, noRoomHint }; },
  data() { return { view: null, loading: false, error: "", err: "", input: "", savedHint: "",
                    fieldErrors: {}, errNote: "" }; },
  computed: {
    busy() { return !!state.busy["token:" + this.cluster]; },
    rows() {
      if (!this.view) return [];
      return [
        { k: "配没配", v: this.view.configured ? "已配置" : "还没配" },
        { k: "位数", v: this.view.configured ? this.view.length + " 位（明文不回显）" : "—" },
        { k: "文件", v: this.view.path, mono: true },
      ];
    },
  },
  watch: { cluster: { immediate: true, handler() { this.load(); } } },
  methods: {
    async load() {
      this.view = null; this.error = ""; this.err = ""; this.input = ""; this.savedHint = "";
      if (!this.cluster) return;
      this.loading = true;
      const res = await api.get("/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/token");
      this.loading = false;
      if (res.ok) this.view = res.data;
      else this.error = res.error || ("接口返回 " + res.status);
    },
    async save() {
      const token = this.input.trim();
      if (!token) { this.err = "输入框是空的。要清空令牌请用旁边那个按钮——它会更明确地问你一次。"; return; }
      this.err = "";
      clearFieldErrors(this);
      const res = await act("token:" + this.cluster, "PUT",
        "/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/token", { token },
        { okTitle: "令牌已写入", okBody: (d) => (d && d.hint) || "", badTitle: "令牌没写进去",
          onError: (r) => attachError(this, r, TOKEN_ERR_DEFS) });
      if (res.ok) {
        this.view = res.data;
        this.input = "";                 // 明文不留存在界面上
        this.savedHint = res.data.hint || "";
        clearFieldErrors(this);
      }
      // 失败：原文由 onError 挂在输入框与 Note 上，不再塞回整表 err-box。
    },
    async clear() {
      // 清空令牌不可反向（旧令牌明文从不回显，撤销时重建不出来）：只把后果说清，不伪造撤销。
      const ok = await askConfirm({
        title: "清空 " + this.cluster + " 的 Klei 令牌？",
        body: "会删掉 cluster_token.txt，连它旁边那份存着上一个令牌的 cluster_token.txt.bak 一起删——" +
          "摘令牌通常就是因为那个串泄露了，留一份旧副本在磁盘上等于只摘了一半。这一步没有撤销。",
        warn: "没有令牌这个房间起不来：引擎会打印「!!!! Your Server Will Not Start !!!!」然后拒绝上列表。" +
          "正在跑的世界不会因此掉线，但重连时进不来。已经出现在 Klei 服务器列表里的那一条要自己去个人中心下架，面板删不掉它。",
        danger: true, okLabel: "清空令牌",
      });
      if (!ok) return;
      this.err = "";
      clearFieldErrors(this);
      const res = await act("token:" + this.cluster, "DELETE",
        "/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/token", undefined,
        { okTitle: "令牌已摘下", okBody: (d) => (d && d.hint) || "", badTitle: "清空没成",
          onError: (r) => attachError(this, r, TOKEN_ERR_DEFS) });
      if (res.ok) { this.view = res.data; this.input = ""; }
    },
  },
  template: `
    <section class="card">
      <div class="card-head">
        <span class="ico-tile tint-gold"><x-icon name="lock" :size="16"></x-icon></span>
        <h2>Klei 令牌</h2>
        <span class="chip" v-if="view">{{ view.configured ? '已配置 ' + view.length + ' 位' : '还没配' }}</span>
        <span class="grow"></span>
        <button class="btn sm ghost" :disabled="loading || !cluster" @click="load">重新读一次</button>
      </div>

      <x-sk v-if="loading" :lines="3"></x-sk>
      <div class="err-box" v-else-if="error">{{ error }}</div>
      <x-empty v-else-if="!cluster" icon="servers" title="这台机器上还没有房间"
               :note="noRoomHint()"></x-empty>
      <template v-else-if="view">
        <x-note v-if="readOnlyNote" kind="warn">{{ readOnlyNote }}</x-note>
        <x-note :kind="view.configured ? 'ok' : 'warn'">
          <b v-if="view.configured">这个房间已经有令牌了。</b>
          <b v-else>这个房间还没有令牌。</b>
          没有令牌的房间面板照样能启动它，但引擎会在日志里打印
          <b>!!!! Your Server Will Not Start !!!!</b> 然后拒绝上列表；进程不会自己退出，
          所以看着像"起来了却没反应"。令牌在 Klei 个人中心的 Don't Starve Together 服务器页面生成。
        </x-note>
        <x-kv :rows="rows" class="mt"></x-kv>
        <div class="field mt">
          <label>换一份新令牌</label>
          <input type="password" v-model="input" autocomplete="off">
          <x-field-error :text="fieldErrors.input"></x-field-error>
          <span class="hint">当前已输入 {{ input.trim().length }} 位。这一栏不回显旧令牌，
            所以"框里是空的"不代表没配——上面那行位数才是事实。</span>
        </div>
        <div class="err-box mt" v-if="err">{{ err }}</div>
        <x-note class="mt" kind="warn" v-if="errNote">{{ errNote }}</x-note>
        <x-field-error :text="fieldErrors._form"></x-field-error>
        <div class="note ok mt" v-if="savedHint">{{ savedHint }}</div>
        <div class="row mt" style="gap:8px;flex-wrap:wrap">
          <button class="btn primary" :disabled="busy || !writable || !input.trim()"
                  :title="!writable ? readOnlyNote : ''" @click="save">{{ busy ? '写入中……' : '写入令牌' }}</button>
          <button class="btn danger" :disabled="busy || !writable || !view.configured"
                  :title="!writable ? readOnlyNote : ''" @click="clear">清空</button>
        </div>
      </template>
    </section>`,
};

// ---- 世界设定 ----

const WorldCard = {
  props: {
    cluster: String,
    shards: { type: Array, default: () => [] },
    schema: { type: Array, default: () => [] },
  },
  mixins: [RoomAccess],
  // fieldIcon / groupTint 必须出现在这里：模板编译成 with(this)，只看得到实例上的名字。
  // 少了这一行，界面上是"分组标题和每一项全都没有图标"，控制台一句 not a function。
  setup() { return { state, noRoomHint, placeName, fieldIcon, groupTint, groupIcon }; },
  data() {
    // docs["Master"] = { loading, error, view, vals, preset }
    // vals 只存"这一屏被用户动过的项"，没动过的取值走 val()（文件值 → 字典默认值）。
    // 提交时仍发整份表（见 allVals）：只发增量会踩一个坑——把某项改回默认值时它不在
    // 增量里，而后端又会把旧文件的值并进来，结果就是"界面上改回去了，文件里没改"。
    return { docs: {}, shard: "", onlyChanged: false, q: "", err: "",
      fieldErrors: {}, errNote: "", worldNote: "", worldRegenNote: "",
      // openKeys 记"这一组用户明确展开/收起过"，没记的就走默认：宽屏全展开、窄屏只展开第一组。
      // 一张表两百项在手机上是一万六千像素，两屏之内根本找不到自己要改的那一项；
      // 默认收起而不是全部展开，代价只是多一次点击，换来的是首屏能看完"这里有哪些门类"。
      openKeys: {},
      narrow: !!(window.matchMedia && window.matchMedia("(max-width: 900px)").matches) };
  },
  computed: {
    loc() { return this.schemaFor(this.shard); },
    doc() { return this.docs[this.shard] || null; },
    busy() { return !!state.busy["world:" + this.cluster + "/" + this.shard]; },
    fileCount() { return this.doc && this.doc.view ? Object.keys(this.doc.view.overrides || {}).length : 0; },
    groups() {
      if (!this.loc) return [];
      const q = this.q.trim().toLowerCase();
      const out = [];
      this.loc.groups.forEach((g) => {
        const items = (g.items || []).filter((it) => {
          if (this.onlyChanged && !this.isChanged(it)) return false;
          if (!q) return true;
          return (it.label || "").toLowerCase().indexOf(q) >= 0 || it.key.toLowerCase().indexOf(q) >= 0;
        });
        if (items.length) out.push({ key: g.key, label: g.label, items });
      });
      return out;
    },
    filteredCount() { return this.groups.reduce((n, g) => n + g.items.length, 0); },
    changedCount() {
      if (!this.loc || !this.doc) return 0;
      let n = 0;
      this.loc.groups.forEach((g) => (g.items || []).forEach((it) => { if (this.isChanged(it)) n++; }));
      return n;
    },
    totalCount() { return this.loc ? this.loc.itemCount : 0; },
    // 落盘用的整份表：字典里每一项的当前取值。用户没动过的项也在里面，
    // 因为后端要的是"这一屏想要的状态"，不是差量（原因见 data() 那段注释）。
    allVals() {
      const out = {};
      if (!this.loc) return out;
      this.loc.groups.forEach((g) => (g.items || []).forEach((it) => { out[it.key] = this.val(it); }));
      return out;
    },
    // 过滤条件一挂上就自动展开所有组：搜索结果藏在一个合着的分组里，
    // 比收起之前更糟——用户会以为"这一项不存在"。
    searching() { return !!this.q.trim() || this.onlyChanged; },
    firstGroupKey() { return this.loc && this.loc.groups && this.loc.groups.length ? this.loc.groups[0].key : ""; },
    // 文件里那个预设这个地形用不用得了，判据要和后端一致：看它在不在这个地形的可选表里。
    // 不能只按"有没有中文名"判——接口的 presetLabel 是全局查的，
    // 把洞穴预设写在 Master 上时它照样给得出中文名，而那种提交必吃 409。
    filePresetUsable() {
      const v = this.doc && this.doc.view;
      if (!v || !v.preset || !this.loc) return false;
      return (this.loc.presets || []).some((p) => p.value === v.preset);
    },
    // 文件存在、里面那个预设又用不了（没写 preset 也算）：这时"不改动"会被后端拒，
    // 因为它不能替你补一个默认预设上去——那等于换了世界图。在这里提前说同一句话，
    // 比让人点提交、撞回 409、再回来找那一栏快。
    mustChoosePreset() {
      const v = this.doc && this.doc.view;
      return !!(v && v.present && !this.filePresetUsable);
    },
    presetKeepText() {
      const v = this.doc && this.doc.view;
      if (!v) return "不改动";
      if (!v.present) return "不改动（还没有文件，第一次生成世界用该地形的默认预设）";
      if (this.filePresetUsable) return "不改动，仍用「" + (v.presetLabel || v.preset) + "」";
      return "不改动（文件里的预设这个地形用不了，提交会被拒）";
    },
  },
  watch: {
    cluster: { immediate: true, handler() { this.reload(); } },
    // 房间列表是异步到的：挂载时 shards 还是空数组，光靠 watch cluster 会一直停在"没有分片"。
    shards(list) { if (!list.length) { this.shard = ""; return; } if (!this.shard) this.shard = list[0].name; },
    shard() { this.ensureDoc(); },
  },
  methods: {
    schemaFor(shard) {
      if (!shard) return this.schema[0] || null;
      return this.schema.find((s) => s.shard === shard) ||
        this.schema.find((s) => s.shard === "Master") || this.schema[0] || null;
    },
    reload() {
      this.docs = {};
      this.err = "";
      this.shard = this.shards.length ? this.shards[0].name : "";
      this.ensureDoc();
    },
    async ensureDoc(force) {
      if (!this.shard || !this.cluster) return;
      if (this.docs[this.shard] && !force) return;
      this.docs[this.shard] = { loading: true, error: "", view: null, vals: {}, preset: "" };
      const res = await api.get("/api/v1/clusters/" + encodeURIComponent(this.cluster) +
        "/shards/" + encodeURIComponent(this.shard) + "/world");
      // 重读期间可能已经切走了房间：那份迟到的响应不能写回旧房间的表里。
      const d = this.docs[this.shard];
      if (!d) return;
      d.loading = false;
      if (!res.ok) { d.error = res.error || ("接口返回 " + res.status); return; }
      d.view = res.data;
      d.preset = "";                     // 默认"不改预设"，理由见模板里那行 hint
    },
    // 这一项现在该显示什么：用户在这一屏改过的值 → 文件里的值 → 字典默认值。
    //
    // vals 为什么只存"改过的"：以前是拿到世界文件后把整份表并进 vals，
    // 而字典是另一个请求。字典慢一步时并出来的是空表，189 项全被标成"已改"，
    // 点写入还会把那张空表发过去——界面看着正常，底下是空的。
    // 改成按当前取值算，就没有"谁先到"这个问题，也不用心跳重并。
    val(it) {
      const d = this.doc;
      if (!d) return it.default;
      // 为什么直接取值而不是 hasOwnProperty：这版 Vue（3.5.13）里"这个键存在吗"
      // 不会给 computed 建依赖，往 vals 里加第一个键时谁都收不到通知——
      // 表现是改完一项，"已改"要等下一次心跳才亮，而"与默认不同 N 项"永远停在 0。
      // 取值本身会建依赖，新键落进去时该重算的都会重算（已实测）。
      const u = d.vals[it.key];
      if (u !== undefined) return u;
      const v = d.view && d.view.overrides ? d.view.overrides[it.key] : undefined;
      return v === undefined || v === null || v === "" ? it.default : String(v);
    },
    setVal(it, v) { if (this.doc) this.doc.vals[it.key] = v; },
    isChanged(it) { return this.val(it) !== it.default; },
    labelOf(it, value) {
      const hit = (it.options || []).find((o) => o.value === value);
      return hit ? hit.label : value;
    },
    optionsOf(it) {
      const cur = this.val(it);
      const list = (it.options || []).slice();
      // 手工改过的文件里会出现字典没有的取值。这时候必须把它作为一条真实选项放进下拉框：
      // 否则 select 显示成空，用户一动下拉就把原值丢了——那等于面板替他改了世界。
      if (cur && !list.some((o) => o.value === cur)) {
        list.unshift({ value: cur, label: cur + "（字典里没有这一项，文件里的原值）" });
      }
      return list;
    },
    presetOptions() { return this.loc ? this.loc.presets || [] : []; },
    // 分组的展开状态：搜索或"只看改过的"时一律展开，其余按 openKeys 走，没记过的按屏宽给默认。
    isOpen(key) {
      if (this.searching) return true;
      const o = this.openKeys[key];
      if (o !== undefined) return o;
      return !this.narrow || key === this.firstGroupKey;
    },
    // details 被用户点着开合时改的是 property 而不是 attribute；这里把它同步回 openKeys，
    // 否则下一次重渲染会按 :open 的旧值把分组猛地合上，点下去像"没反应"。
    syncGroup(key, ev) { this.openKeys[key] = !!(ev.target && ev.target.open); },
    groupChanged(g) {
      if (!this.doc) return 0;
      return (g.items || []).filter((it) => this.isChanged(it)).length;
    },
    expandAll() {
      if (!this.loc) return;
      this.loc.groups.forEach((g) => { this.openKeys[g.key] = true; });
    },
    collapseAll() {
      if (!this.loc) return;
      this.loc.groups.forEach((g) => { this.openKeys[g.key] = false; });
    },
    resetChanged() {
      // 只把"和默认不一样"的项改回默认，不动其它：
      // 一键"全部还原"看着方便，但它会把文件里手工调过、默认值并不一样的项一起抹掉。
      const d = this.doc;
      if (!d || !this.loc) return;
      this.loc.groups.forEach((g) => (g.items || []).forEach((it) => {
        if (this.isChanged(it)) d.vals[it.key] = it.default;
      }));
    },
    async save() {
      this.err = "";
      clearFieldErrors(this);
      const d = this.doc;
      if (!this.loc) {
        // 字典没到的时候 allVals 是一张空表，发出去等于"把每一项都改成默认值"。
        this.err = "世界规则字典还在读取，等它出来再点写入。";
        return;
      }
      if (this.mustChoosePreset && !d.preset) {
        this.err = "先在「世界预设」里选一个再写入：" +
          (d.view.preset ? "文件里那个预设 " + d.view.preset + " 不属于「" +
            (this.loc ? this.loc.label : "这个地形") + "」可用的那批。"
            : "这份文件里根本没写预设，而「" + (this.loc ? this.loc.label : "这个地形") + "」必须有一个。") +
          "选「不改动」等于让面板补一个默认预设上去，那是把引擎的「并进当前存档」改成「整张重造世界」，" +
          "这一步面板不替你猜。";
        return;
      }
      const body = { overrides: this.allVals };
      if (d.preset) body.preset = d.preset;
      // 写文件是可反向的：把写入前那份文件的 overrides（和仍可用的旧 preset）留作撤销体。
      // 旧文件是稀疏的、而且预设可能不可用，所以这是"尽力恢复"而不是逐项等价，见清单里的风险说明。
      const undoBody = { overrides: Object.assign({}, (d.view && d.view.overrides) || {}) };
      if (d.view && d.view.preset && this.filePresetUsable) undoBody.preset = d.view.preset;
      const url = "/api/v1/clusters/" + encodeURIComponent(this.cluster) +
        "/shards/" + encodeURIComponent(this.shard) + "/world";
      const res = await act("world:" + this.cluster + "/" + this.shard, "PUT", url, body,
        { okTitle: this.shard + " 的世界规则已写入", badTitle: "这次没写进去",
          onError: (r) => attachError(this, r, WORLD_ERR_DEFS),
          undo: { label: "撤销刚才写入的世界规则", method: "PUT", path: url,
                  body: undoBody, seconds: 12 } });
      // 成功与失败都把后端那句中文原样留着：400 是某一项取值非法，409 是文件里的预设
      // 不是这个地形可用的那批——两种都要照着那句话改，改写成"保存失败"等于让人猜。
      if (res.ok) {
        // 后端给回的是它写进去之后那份文件的真实状态，直接换掉 view；
        // 用户在这一屏动过的标记已经没有意义了，清掉，剩下的取值全从新文件读。
        d.view = res.data;
        d.vals = {};
        clearFieldErrors(this);
        this.worldNote = res.data.note || "";     // 后端原文：只影响下一次世界生成，现有地图不变
        // 用后端回执字段说话，不自己猜"重生到底执行没执行"：请求收到了、但面板没有执行。
        this.worldRegenNote = (res.data.regenerateRequested && !res.data.regenerateApplied)
          ? ("重生请求已收到但未执行（regenerateRequested=true、regenerateApplied=false）：" +
             (res.data.effective ? "这次改动" + res.data.effective + "，" : "") + "现有地图不变。")
          : "";
      }
      // 失败：原文由 onError 挂在字段与 Note 上，不再塞回整表 err-box。
    },
  },
  // 窄屏与否只决定"默认展开几组"，所以跟着视口走：用户把窗口拖窄/拖宽时不必刷新页面。
  mounted() {
    if (!window.matchMedia) return;
    this.mq = window.matchMedia("(max-width: 900px)");
    this.onMq = (e) => { this.narrow = e.matches; };
    if (this.mq.addEventListener) this.mq.addEventListener("change", this.onMq);
    else if (this.mq.addListener) this.mq.addListener(this.onMq);
  },
  beforeUnmount() {
    if (this.mq && this.mq.removeEventListener) this.mq.removeEventListener("change", this.onMq);
  },
  template: `
    <div class="col" style="gap:14px">
      <x-empty v-if="!cluster" icon="servers" title="这台机器上还没有房间"
               :note="noRoomHint()"></x-empty>
      <x-empty v-else-if="!shards.length" icon="servers" title="这个房间没有世界"
               note="目录里一个世界都没有，多半是手工建的半成品；用向导重开一个更省事。"></x-empty>

      <template v-else>
        <div class="tabs" v-if="shards.length > 1">
          <button v-for="s in shards" :key="s.name" :class="shard === s.name ? 'on' : ''" @click="shard = s.name">
            {{ s.name }}<span class="tab-count">{{ s.running ? '在跑' : '没在跑' }}</span>
          </button>
        </div>

        <section class="card" v-if="doc">
          <div class="card-head">
            <span class="ico-tile tint-teal"><x-icon name="world" :size="16"></x-icon></span>
            <h2>{{ placeName(shard) }} · {{ loc ? loc.label : '未知地形' }}</h2>
            <span class="chip" v-if="doc.view">{{ doc.view.present ? '文件里 ' + fileCount + ' 项' : '还没有世界规则文件' }}</span>
            <span class="chip" v-if="doc.view">与默认不同 {{ changedCount }} 项</span>
            <span class="grow"></span>
            <button class="btn sm ghost" :disabled="doc.loading" @click="ensureDoc(true)">重读</button>
          </div>

          <x-sk v-if="doc.loading" :lines="4"></x-sk>
          <div class="err-box" v-else-if="doc.error">{{ doc.error }}</div>

          <template v-else-if="doc.view">
            <x-note v-if="readOnlyNote" kind="warn">{{ readOnlyNote }}</x-note>
            <div class="note warn" v-if="doc.view.levelDataConflict">
              <b>这个世界同时存在 leveldataoverride.lua。</b>{{ doc.view.levelDataConflict }}
            </div>
            <div class="note info">{{ doc.view.note }}</div>

            <div class="grid-form mt">
              <div class="field">
                <label>世界预设</label>
                <select v-model="doc.preset">
                  <option value="" :disabled="mustChoosePreset">{{ presetKeepText }}</option>
                  <option v-for="p in presetOptions()" :key="p.value" :value="p.value">
                    {{ p.label }}{{ p.english ? ' · ' + p.english : '' }}</option>
                </select>
                <x-field-error :text="fieldErrors.preset"></x-field-error>
                <span class="hint warn" v-if="mustChoosePreset">
                  <b>这一栏必须选一个预设，否则写不进去。</b>
                  <template v-if="doc.view.preset">文件里那个 <b class="mono">{{ doc.view.preset }}</b> 不属于</template>
                  <template v-else>文件里压根没写预设，而</template>
                  「{{ loc ? loc.label : '这个地形' }}」可用的只有上面这几项；多半是拷来的存档或手工改过的文件。
                </span>
                <span class="hint" v-else-if="!doc.view.present">还没有这个文件：不改预设的话，第一次生成世界用该地形的默认预设。</span>
              </div>
              <div class="field">
                <label>找一项</label>
                <input type="search" v-model="q" placeholder="按名字或键名过滤，例如 季节 / autumn">
              </div>
              <div class="field">
                <label>显示范围</label>
                <div class="seg">
                  <button :class="!onlyChanged ? 'on' : ''" @click="onlyChanged = false">全部 {{ totalCount }} 项</button>
                  <button :class="onlyChanged ? 'on' : ''" @click="onlyChanged = true">只看改过的 {{ changedCount }} 项</button>
                </div>
              </div>
            </div>

            <div class="empty mt" v-if="!groups.length">
              <div class="e-title">这一屏没有匹配的设定项</div>
              <div class="e-note">{{ onlyChanged ? '这个世界目前没有任何一项和字典默认值不一样。' : '换个词试试，或者把显示范围切回全部。' }}</div>
            </div>

            <div class="col mt" style="gap:10px" v-else>
              <div class="row g-tools">
                <span class="muted">分 {{ groups.length }} 组 · 共 {{ filteredCount }} 项</span>
                <span class="grow"></span>
                <button class="linkish" @click="expandAll">全部展开</button>
                <button class="linkish" @click="collapseAll">全部收起</button>
              </div>
              <details class="group" :class="groupTint(g.key)" v-for="g in groups" :key="g.key"
                       :open="isOpen(g.key)" @toggle="syncGroup(g.key, $event)">
                <summary class="g-head">
                  <span class="g-chev" aria-hidden="true">›</span>
                  <span class="ico-tile sm" aria-hidden="true"><x-icon :name="groupIcon(g.key)" :size="14"></x-icon></span>
                  <b>{{ g.label }}</b>
                  <span class="g-count">{{ g.items.length }} 项</span>
                  <span class="grow"></span>
                  <span class="g-changed" v-if="groupChanged(g)">已改 {{ groupChanged(g) }} 项</span>
                </summary>
                <div class="grid-form">
                  <div class="field" v-for="it in g.items" :key="it.key">
                    <label>
                      <span class="f-ico" aria-hidden="true"><x-icon :name="fieldIcon(it.key, g.key)" :size="13"></x-icon></span>{{ it.label }}
                    </label>
                    <select :class="isChanged(it) ? 'changed' : ''" :value="val(it)"
                            @change="setVal(it, $event.target.value)">
                      <option v-for="o in optionsOf(it)" :key="o.value" :value="o.value">{{ o.label }}</option>
                    </select>
                    <span class="hint" v-if="isChanged(it)">已改 · 字典默认「{{ labelOf(it, it.default) }}」</span>
                  </div>
                </div>
              </details>
            </div>

            <div class="err-box mt" v-if="err">{{ err }}</div>
            <x-note class="mt" kind="warn" v-if="errNote">{{ errNote }}</x-note>
            <x-field-error :text="fieldErrors._form"></x-field-error>
            <x-note class="mt" kind="info" v-if="worldNote">{{ worldNote }}</x-note>
            <x-note class="mt" kind="warn" v-if="worldRegenNote">{{ worldRegenNote }}</x-note>
            <div class="row spread mt" style="gap:8px;flex-wrap:wrap">
              <button class="btn ghost" :disabled="!changedCount" @click="resetChanged">把改过的项还原</button>
              <span class="grow"></span>
              <button class="btn primary" :disabled="busy || !writable"
                      :title="!writable ? readOnlyNote : ''" @click="save">{{ busy ? '写入中……' : '写入 ' + shard }}</button>
            </div>
            <span class="hint mt">写文件不会影响已经在跑的存档：它只在下一次世界生成时生效。
              要重做世界得用控制台那条重生命令，它会清掉当前存档，面板不会替你按。</span>
          </template>
        </section>
      </template>
    </div>`,
};

// ---- 集群设置 ----

// 这一页改的是 cluster.ini，取值白名单在后端（validateGameSettings，和新建集群共用）。
// 界面只做两件事：只把改过的那几项发回去（没动的不发），以及说清楚"什么时候生效"——
// cluster.ini 是分片启动时读一次的，页面上最大的认知陷阱不是"能不能改"，而是
// "改完为什么没反应"。
const CLUSTER_FIELD_GROUPS = {
  basic: ["displayName", "description"],
  gameplay: ["gameMode", "intention", "maxPlayers", "tickRate", "language", "pvp", "pauseWhenEmpty"],
  advanced: ["voteEnabled", "voteKickEnabled", "lanOnly", "offlineCluster", "whitelistSlots", "consoleEnabled"],
};

const ClusterSettingsCard = {
  props: { cluster: String },
  mixins: [RoomAccess],
  // bytes/dateTime/isBusy 给历史版本面板用：模板是 with(this) 编译的，只看得见实例上的名字。
  setup() { return { state, noRoomHint, GAME_MODES, INTENTIONS, LANGUAGES, TICK_RATES,
                     bytes, dateTime, isBusy }; },
  data() {
    return {
      view: null, orig: null, form: null,
      loading: false, error: "", err: "", savedNote: "",
      pw: "", clearPw: false,
      fieldErrors: {}, errNote: "",
      // 保存前的只读 diff 预览，以及保存回执里的生效口径。
      diff: null, diffLoading: false, diffError: "",
      savedDiff: null, savedEffective: "", savedRequiresRestart: false,
      // cluster.ini 的多版本备份（列出 + 回滚）。
      backups: null, backupsLoading: false, backupsError: "", rollbackNote: "",
      // 和世界设定页同一套折叠记忆：窄屏只展开第一组，免得一进来就是三屏表单。
      openKeys: {},
      narrow: !!(window.matchMedia && window.matchMedia("(max-width: 900px)").matches),
    };
  },
  computed: {
    busy() { return !!state.busy["cluster-settings:" + this.cluster]; },
    changedTotal() {
      return ["basic", "gameplay", "advanced"].reduce((n, g) => n + this.groupChanged(g), 0);
    },
    hasChanges() { return this.changedTotal > 0; },
    runningText() { return this.view ? (this.view.runningShards || []).join("、") : ""; },
    modeNote() {
      if (!this.form) return "";
      const m = GAME_MODES.find((x) => x.value === this.form.gameMode);
      return m ? m.note : "";
    },
    tickNote() {
      if (!this.form) return "";
      const m = TICK_RATES.find((x) => x.value === String(this.form.tickRate));
      return m ? m.note : "";
    },
    diffChangedItems() {
      return this.diff && this.diff.items ? this.diff.items.filter((it) => it.changed) : [];
    },
  },
  watch: {
    cluster: { immediate: true, handler() { this.load(); } },
    // 先点了"清空密码"又往输入框里打字，说明改主意了：以新密码为准，
    // 免得两句话到提交时才打架。
    pw(v) { if (v.trim() && this.clearPw) this.clearPw = false; },
    // 未提交的改动落进 sessionStorage（pw 明文不进），并让上一份 diff 预览失效：
    // 条件变了，屏上那份 old→new 就不再是"将要写入"的那一份。
    form: { deep: true, handler() { this.persist(); this.diff = null; } },
    clearPw() { this.persist(); },
  },
  methods: {
    pick(v) {
      return {
        displayName: v.displayName || "", description: v.description || "",
        gameMode: v.gameMode || "", intention: v.intention || "",
        maxPlayers: v.maxPlayers, tickRate: v.tickRate,
        language: v.language || "", pvp: !!v.pvp, pauseWhenEmpty: !!v.pauseWhenEmpty,
        voteEnabled: !!v.voteEnabled, voteKickEnabled: !!v.voteKickEnabled,
        lanOnly: !!v.lanOnly, offlineCluster: !!v.offlineCluster,
        whitelistSlots: v.whitelistSlots, consoleEnabled: !!v.consoleEnabled,
      };
    },
    apply(v) {
      this.view = v;
      this.form = this.pick(v);
      this.orig = this.pick(v);
    },
    // ---- 未提交草稿：切页签会销毁这个 v-if 子组件，靠 sessionStorage 把字接回来 ----
    draftName() { return this.cluster || "_"; },
    persist() {
      // pw 是明文密码，绝不落盘；clearPw 这个"打算清空"的标记可以存。
      if (!this.form || !this.orig) return;
      // 表单已经回到文件现值（刚保存成功 / 刚还原）时没有"未提交的改动"可留，
      // 顺手把草稿清掉。否则 apply() 触发的这次 watcher 会在 clearSaved() 之后
      // 又把一份空草稿写回来，下次进页面还要做一次无意义的恢复。
      const changed = Object.keys(this.form).some((k) => this.form[k] !== this.orig[k]) || this.clearPw;
      if (!changed) { this.clearSaved(); return; }
      writeDraft("cluster-settings", this.draftName(),
        { form: Object.assign({}, this.form), clearPw: this.clearPw });
    },
    restoreDraft() {
      const d = readDraft("cluster-settings", this.draftName());
      if (!d || !d.form || !this.form) return;
      Object.keys(this.form).forEach((k) => {
        if (d.form[k] !== undefined) this.form[k] = d.form[k];
      });
      if (d.clearPw) this.clearPw = true;
    },
    clearSaved() { clearDraft("cluster-settings", this.draftName()); },
    async load() {
      this.view = null; this.orig = null; this.form = null;
      this.error = ""; this.err = ""; this.savedNote = "";
      this.pw = ""; this.clearPw = false;
      clearFieldErrors(this);
      this.diff = null; this.diffError = "";
      this.savedDiff = null; this.savedEffective = ""; this.savedRequiresRestart = false;
      if (!this.cluster) return;
      this.loading = true;
      const res = await api.get("/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/settings");
      this.loading = false;
      if (res.ok) {
        this.apply(res.data);
        this.restoreDraft();     // 文件现值打底，再把没提交的改动盖回来
        this.loadBackups();
      } else this.error = res.error || ("接口返回 " + res.status);
    },
    async reread() {
      if (this.hasChanges) {
        const ok = await askConfirm({
          title: "重读会丢掉还没保存的改动",
          body: "这一页有几项已经改过但还没写入 cluster.ini，重读会按文件里的现值把表单刷回去，草稿也会一起清掉。",
          okLabel: "丢掉改动并重读",
        });
        if (!ok) return;
      }
      this.clearSaved();
      await this.load();
    },
    fieldChanged(k) { return !!(this.form && this.orig && this.form[k] !== this.orig[k]); },
    mark(k) { return this.fieldChanged(k) ? "changed" : ""; },
    groupChanged(g) {
      if (!this.form) return 0;
      const n = (CLUSTER_FIELD_GROUPS[g] || []).filter((k) => this.fieldChanged(k)).length;
      return g === "basic" && (!!this.pw.trim() || this.clearPw) ? n + 1 : n;
    },
    isOpen(key) {
      const o = this.openKeys[key];
      if (o !== undefined) return o;
      return !this.narrow || key === "basic";
    },
    syncOpen(key, ev) { this.openKeys[key] = !!(ev.target && ev.target.open); },
    known(list, v) { return (list || []).some((o) => o.value === v); },
    revert() {
      this.form = this.pick(this.orig);
      this.pw = ""; this.clearPw = false; this.err = ""; this.savedNote = "";
      this.diff = null; this.diffError = "";
      this.clearSaved();       // 「把改过的项还原」就是用户点的重置：草稿一起清
    },
    async requestClear() {
      const ok = await askConfirm({
        title: "清空这个房间的连接密码？",
        body: "保存之后房间就是公开的：任何人都能连进来，不再需要密码。正在玩的玩家不会因此被踢下线。",
        warn: "这一下只是把「清空」排进表单，还得再点一次「写入 cluster.ini」才会真的生效。",
        danger: true, okLabel: "排进待保存的改动",
      });
      if (!ok) return;
      this.clearPw = true;
      this.pw = "";
    },
    validate() {
      const f = this.form;
      if (!f.displayName.trim()) return "服务器名称不能为空：玩家在服务器列表上看到的就是它。";
      const max = Number(f.maxPlayers);
      if (!Number.isInteger(max) || max < 1 || max > 16) return "最大玩家数要是 1~16 之间的整数。";
      const slots = Number(f.whitelistSlots);
      if (!Number.isInteger(slots) || slots < 0) return "白名单保留位不能是负数。";
      if (slots > max) return "白名单保留位不能超过最大玩家数，否则会有一批位置空着谁都不能进。";
      if (this.clearPw && this.pw.trim()) return "既填了新密码又要求清空，先决定一个。";
      return "";
    },
    buildBody() {
      const body = {};
      Object.keys(CLUSTER_FIELD_GROUPS).forEach((g) => {
        CLUSTER_FIELD_GROUPS[g].forEach((k) => {
          if (this.form[k] !== this.orig[k]) body[k] = this.form[k];
        });
      });
      if (this.pw.trim()) body.password = this.pw.trim();
      if (this.clearPw) body.clearPassword = true;
      return body;
    },
    // diff 预览是 GET，参数只能走 query（GET 不能带请求体）。密码明文绝不进 URL：
    // 用 setPassword=1 占位、clearPassword=1 表达"要清空"，后端 diff 里密码那项
    // 本来就只显示"已设置/未设置"，不需要真实值。
    buildDiffQuery() {
      const body = this.buildBody();
      const q = {};
      Object.keys(body).forEach((k) => {
        if (k === "password") { if (String(body[k]).trim()) q.setPassword = 1; return; }
        if (k === "clearPassword") { if (body[k]) q.clearPassword = 1; return; }
        q[k] = body[k];
      });
      return api.qs(q);
    },
    async previewDiff() {
      this.diffError = "";
      this.diffLoading = true;
      const res = await api.get("/api/v1/clusters/" + encodeURIComponent(this.cluster) +
        "/settings/diff" + this.buildDiffQuery());
      this.diffLoading = false;
      if (res.ok) this.diff = res.data;
      else { this.diff = null; this.diffError = res.error || ("接口返回 " + res.status); }
      return res.ok;
    },
    // 保存前先给一份逐项 diff，用户确认过再 PUT；确认框把 old→new 作为 details 逐条列出。
    async save() {
      this.err = "";
      clearFieldErrors(this);
      const bad = this.validate();
      if (bad) { this.err = bad; return; }
      const ok = await this.previewDiff();
      if (!ok) { this.err = this.diffError || "改动预览没读到，先不写入。"; return; }
      const d = this.diff || {};
      const changed = (d.items || []).filter((it) => it.changed);
      const lines = changed.length
        ? changed.map((it) => it.label + "：" + (it.old || "（空）") + " → " + (it.new || "（空）") +
            "（" + (it.effective || "") + "）")
        : ["没有任何一项和文件现值不同，写入会被后端当作「无改动」跳过。"];
      const confirmed = await askConfirm({
        title: "把 " + (d.changedCount || 0) + " 项改动写进 cluster.ini？",
        body: "下面逐项是「文件现值 → 将要写入的值」，与后端 /settings/diff 算出来的完全一致。" +
          (d.effective ? "整体生效时机：" + d.effective + "。" : ""),
        warn: d.requiresRestart
          ? "有世界正在跑：cluster.ini 只在世界启动时读一次，保存后要重启这些世界才生效（重启会先落盘，在线玩家会掉线重连）。"
          : (d.note || ""),
        details: lines,
        okLabel: "确认写入",
      });
      if (!confirmed) return;
      // 能反向的改动给一条 12 秒撤销：把改过的字段写回保存前的原值。
      // 涉及密码时不给撤销——旧密码明文从不回显，重建不出来，伪造一条撤销等于骗人。
      const canUndo = !this.pw.trim() && !this.clearPw && changed.length > 0;
      const undoBody = {};
      Object.keys(CLUSTER_FIELD_GROUPS).forEach((g) => {
        CLUSTER_FIELD_GROUPS[g].forEach((k) => {
          if (this.form[k] !== this.orig[k]) undoBody[k] = this.orig[k];
        });
      });
      const url = "/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/settings";
      const opts = {
        okTitle: "集群设置已写入", badTitle: "这次没写进去",
        onError: (r) => attachError(this, r, CLUSTER_ERR_DEFS),
      };
      if (canUndo && Object.keys(undoBody).length) {
        opts.undo = { label: "撤销刚才的集群设置改动", method: "PUT", path: url,
                      body: undoBody, seconds: 12 };
      }
      const res = await act("cluster-settings:" + this.cluster, "PUT", url, this.buildBody(), opts);
      if (!res.ok) return;   // onError 已把原文挂到字段与 Note
      const data = res.data || {};
      this.apply(data);
      this.pw = "";
      this.clearPw = false;
      this.clearSaved();     // 成功保存：草稿清掉
      this.savedNote = data.note || "";
      this.savedDiff = data.diff || null;
      this.savedEffective = data.effective || "";
      this.savedRequiresRestart = !!data.requiresRestart;
      this.diff = null; this.diffError = "";
      this.loadBackups();
    },
    // ---- cluster.ini 历史版本：列出 /settings/backups，点回滚先确认再 POST ----
    async loadBackups() {
      if (!this.cluster) return;
      this.backupsLoading = true;
      const res = await api.get("/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/settings/backups");
      this.backupsLoading = false;
      if (res.ok) { this.backups = res.data || null; this.backupsError = ""; }
      else { this.backups = null; this.backupsError = res.error || ("接口返回 " + res.status); }
    },
    async rollback(b) {
      const ok = await askConfirm({
        title: "回滚到 " + b.name + "？",
        body: "会把这份备份整个写回 " + (this.backups && this.backups.path ? this.backups.path : "cluster.ini") +
          "，当前表单里还没保存的改动不会跟着写进去。回滚前后端会先把当前文件另存一份，所以回滚本身也可逆。",
        warn: (this.backups && this.backups.note) ||
          "cluster.ini 只在世界启动时读一次，回滚后要重启在跑的世界才生效。",
        danger: true, okLabel: "回滚到这一版",
      });
      if (!ok) return;
      const res = await act("cluster-settings:rollback:" + this.cluster, "POST",
        "/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/settings/backups/rollback",
        { name: b.name },
        { okTitle: "已回滚到 " + b.name, okBody: (d) => (d && d.note) || "", badTitle: "回滚没成" });
      if (!res.ok) return;
      // 后端 note 里带着「回滚前的当前版本另存为 X」这个副本名，原样留在页面上。
      const note = (res.data && res.data.note) || "";
      this.clearSaved();
      await this.load();
      this.rollbackNote = note;
    },
  },
  mounted() {
    if (!window.matchMedia) return;
    this.mq = window.matchMedia("(max-width: 900px)");
    this.onMq = (e) => { this.narrow = e.matches; };
    if (this.mq.addEventListener) this.mq.addEventListener("change", this.onMq);
    else if (this.mq.addListener) this.mq.addListener(this.onMq);
  },
  beforeUnmount() {
    if (this.mq && this.mq.removeEventListener) this.mq.removeEventListener("change", this.onMq);
  },
  template: `
    <section class="card">
      <div class="card-head">
        <span class="ico-tile tint-violet"><x-icon name="gear" :size="16"></x-icon></span>
        <h2>集群设置</h2>
        <span class="chip" v-if="view">{{ runningText ? runningText + ' 在跑' : '世界都没在跑' }}</span>
        <span class="grow"></span>
        <button class="btn sm ghost" :disabled="loading || !cluster" @click="reread">重新读一次</button>
      </div>

      <x-sk v-if="loading" :lines="5"></x-sk>
      <div class="err-box" v-else-if="error">{{ error }}</div>
      <x-empty v-else-if="!cluster" icon="servers" title="这台机器上还没有房间"
               :note="noRoomHint()"></x-empty>
      <x-empty v-else-if="!view" icon="warn" title="没读到集群设置"
               note="cluster.ini 读不出来时，这一页不会显示一份猜测的表单；上面那句错误就是原因。"></x-empty>

      <template v-else>
        <x-note v-if="readOnlyNote" kind="warn">{{ readOnlyNote }}</x-note>
        <x-note v-if="view.runningShards.length && !savedNote" kind="warn">
          <b>{{ runningText }} 正在运行。</b>cluster.ini 只在世界启动时读一次：保存后要重启这些世界才生效。
          改设置本身不会把在线玩家踢下线，但重启会。
        </x-note>
        <x-note v-else-if="view.note && !savedNote" kind="info">{{ view.note }}</x-note>
        <x-note v-else-if="!savedNote" kind="info">世界都没在跑：保存后下次启动直接读到新值。</x-note>

        <div class="col mt" style="gap:10px">
          <details class="group" :open="isOpen('basic')" @toggle="syncOpen('basic', $event)">
            <summary class="g-head">
              <span class="g-chev" aria-hidden="true">›</span>
              <b>基本信息</b>
              <span class="g-count">名称 / 说明 / 密码</span>
              <span class="grow"></span>
              <span class="g-changed" v-if="groupChanged('basic')">已改 {{ groupChanged('basic') }} 项</span>
            </summary>
            <div class="grid-form">
              <div class="field">
                <div class="field-head">
                  <label>服务器名称（必填）</label>
                  <x-hint text="玩家在服务器列表上看到的就是这一行，最多 64 个字，不能带换行。"></x-hint>
                </div>
                <input type="text" v-model="form.displayName" maxlength="64" :class="mark('displayName')">
                <x-field-error :text="fieldErrors.displayName"></x-field-error>
              </div>
              <div class="field">
                <div class="field-head">
                  <label>服务器说明</label>
                  <x-hint text="列表里名称下面那行小字；同样不能带换行。"></x-hint>
                </div>
                <input type="text" v-model="form.description" :class="mark('description')">
              </div>
              <div class="field">
                <div class="field-head">
                  <label>连接密码
                    <span class="chip">{{ view.hasPassword ? '已设密码' : '无密码' }}</span></label>
                  <x-hint text="这一栏不回填旧密码，框里空着就是「不改」；要清空得用下面的按钮。"></x-hint>
                </div>
                <input type="password" v-model="pw" autocomplete="new-password"
                       :placeholder="view.hasPassword ? '留空表示不改密码' : '留空表示公开房间'">
                <span class="hint warn" v-if="clearPw">
                  保存后会清空密码，房间变成公开的。
                  <button class="linkish" @click="clearPw = false">算了</button>
                </span>
                <button class="btn sm ghost mt" v-if="view.hasPassword && !clearPw && !pw.trim()"
                        @click="requestClear">清空密码</button>
              </div>
            </div>
          </details>

          <details class="group" :open="isOpen('gameplay')" @toggle="syncOpen('gameplay', $event)">
            <summary class="g-head">
              <span class="g-chev" aria-hidden="true">›</span>
              <b>玩法与人数</b>
              <span class="g-count">模式 / 人数 / 通信</span>
              <span class="grow"></span>
              <span class="g-changed" v-if="groupChanged('gameplay')">已改 {{ groupChanged('gameplay') }} 项</span>
            </summary>
            <div class="grid-form">
              <div class="field">
                <div class="field-head">
                  <label>游戏模式</label>
                  <x-hint :text="known(GAME_MODES, form.gameMode) ? modeNote : '面板认识的取值只有那几个；文件里当前这个值多半是手工改出来的。不动它就保持原样。'"></x-hint>
                </div>
                <div class="seg">
                  <button v-for="o in GAME_MODES" :key="o.value" :class="form.gameMode === o.value ? 'on' : ''"
                          @click="form.gameMode = o.value">{{ o.label }}</button>
                </div>
                <span class="hint" v-if="!known(GAME_MODES, form.gameMode)">
                  文件里的当前值不在面板认识的取值里（多半是手工改过）；不动它就保持原样。</span>
              </div>
              <div class="field">
                <div class="field-head">
                  <label>游戏偏好</label>
                  <x-hint text="只影响服务器列表上的标签，不改世界规则。"></x-hint>
                </div>
                <div class="seg">
                  <button v-for="o in INTENTIONS" :key="o.value" :class="form.intention === o.value ? 'on' : ''"
                          @click="form.intention = o.value">{{ o.label }}</button>
                </div>
                <span class="hint" v-if="!known(INTENTIONS, form.intention)">
                  文件里的当前值不在面板认识的取值里；不动它就保持原样。</span>
              </div>
              <div class="field">
                <div class="field-head">
                  <label>最大玩家数</label>
                  <x-hint text="上限 16（服务端模板注释写明的）。改小不会把已经在线的玩家踢掉，只影响下一次进人。"></x-hint>
                </div>
                <input type="number" v-model.number="form.maxPlayers" min="1" max="16" :class="mark('maxPlayers')">
                <x-field-error :text="fieldErrors.maxPlayers"></x-field-error>
              </div>
              <div class="field">
                <div class="field-head">
                  <label>每秒通信次数</label>
                  <x-hint :text="known(TICK_RATES, String(form.tickRate)) ? tickNote : '只有 15/20/30 三档；文件里那个值不认识时不动它就保持原样。'"></x-hint>
                </div>
                <div class="seg">
                  <button v-for="o in TICK_RATES" :key="o.value"
                          :class="String(form.tickRate) === o.value ? 'on' : ''"
                          @click="form.tickRate = Number(o.value)">{{ o.label }}</button>
                </div>
                <span class="hint" v-if="!known(TICK_RATES, String(form.tickRate))">
                  文件里的当前值不在 15/20/30 里；不动它就保持原样。</span>
              </div>
              <div class="field">
                <label>服务器语言</label>
                <div class="seg">
                  <button v-for="o in LANGUAGES" :key="o.value" :class="form.language === o.value ? 'on' : ''"
                          @click="form.language = o.value">{{ o.label }}</button>
                </div>
                <span class="hint" v-if="!known(LANGUAGES, form.language)">
                  文件里的当前值不在面板认识的两档里；不动它就保持原样。</span>
              </div>
              <div class="field">
                <div class="field-head">
                  <label>对局开关</label>
                  <x-hint text="「没人时暂停」一开，助手 MOD 也随之停摆，面板会一直显示数据已过期，所以默认不暂停。"></x-hint>
                </div>
                <label class="switch"><input type="checkbox" v-model="form.pvp"><span class="track"></span>开启 PVP</label>
                <label class="switch"><input type="checkbox" v-model="form.pauseWhenEmpty"><span class="track"></span>没人时暂停世界</label>
              </div>
            </div>
          </details>

          <details class="group" :open="isOpen('advanced')" @toggle="syncOpen('advanced', $event)">
            <summary class="g-head">
              <span class="g-chev" aria-hidden="true">›</span>
              <b>高级</b>
              <span class="g-count">白名单 / 投票 / 联机范围</span>
              <span class="grow"></span>
              <span class="g-changed" v-if="groupChanged('advanced')">已改 {{ groupChanged('advanced') }} 项</span>
            </summary>
            <div class="grid-form">
              <div class="field">
                <div class="field-head">
                  <label>白名单保留位</label>
                  <x-hint text="给白名单玩家预留的位置数，不能超过最大玩家数；0 表示不预留。白名单名单本身由游戏侧维护，面板只管这个数。"></x-hint>
                </div>
                <input type="number" v-model.number="form.whitelistSlots" min="0" :max="form.maxPlayers"
                       :class="mark('whitelistSlots')">
                <x-field-error :text="fieldErrors.whitelistSlots"></x-field-error>
              </div>
              <div class="field">
                <div class="field-head">
                  <label>对局开关</label>
                  <x-hint text="局域网/离线一开，公网玩家就进不来或列表里找不到；离线集群也不再需要 Klei 令牌。控制台指的是玩家那一侧的 ~ 键，不是管理员权限——PVP 局常关掉它防作弊。"></x-hint>
                </div>
                <label class="switch"><input type="checkbox" v-model="form.voteEnabled"><span class="track"></span>允许投票</label>
                <label class="switch"><input type="checkbox" v-model="form.voteKickEnabled"><span class="track"></span>允许投票踢人</label>
                <label class="switch"><input type="checkbox" v-model="form.lanOnly"><span class="track"></span>只允许局域网连接</label>
                <label class="switch"><input type="checkbox" v-model="form.offlineCluster"><span class="track"></span>离线集群（不上 Klei 列表）</label>
                <label class="switch"><input type="checkbox" v-model="form.consoleEnabled"><span class="track"></span>允许玩家打开控制台</label>
              </div>
            </div>
          </details>
        </div>

        <div class="err-box mt" v-if="err">{{ err }}</div>
        <x-note class="mt" kind="warn" v-if="errNote">{{ errNote }}</x-note>
        <x-field-error :text="fieldErrors._form"></x-field-error>
        <div class="note ok mt" v-if="savedNote">{{ savedNote }}</div>
        <x-note class="mt" kind="warn" v-if="savedRequiresRestart">
          <b>这些改动要重启才生效（requiresRestart=true）。</b>cluster.ini 只在世界启动时读一次：
          重启在跑的世界会先落盘，在线玩家会掉线重连。{{ savedEffective ? '整体口径：' + savedEffective + '。' : '' }}
        </x-note>
        <x-note class="mt" kind="info" v-else-if="savedEffective">
          已写入，生效口径：{{ savedEffective }}（requiresRestart=false，无需重启）。
        </x-note>

        <!-- 保存前的 diff 预览：逐项 old→new 与生效时机。点「写入」也会先出这份让用户确认。 -->
        <div class="col mt" style="gap:6px" v-if="diff || diffError">
          <div class="row" style="gap:8px;flex-wrap:wrap;align-items:center">
            <span class="chip" v-if="diff">将改动 {{ diff.changedCount }} 项 · {{ diff.effective }}</span>
            <span class="hint" v-if="diff && diff.requiresRestart">这批改动需重启在跑的世界才生效</span>
            <span class="err-box" v-if="diffError">{{ diffError }}</span>
          </div>
          <div class="table-wrap" v-if="diffChangedItems.length">
            <table class="data">
              <thead><tr><th>项目</th><th>现值</th><th>将写入</th><th>生效时机</th></tr></thead>
              <tbody>
                <tr v-for="it in diffChangedItems" :key="it.key">
                  <td>{{ it.label }}</td>
                  <td class="mono">{{ it.old || '（空）' }}</td>
                  <td class="mono">{{ it.new || '（空）' }}</td>
                  <td>{{ it.effective }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <x-note kind="info" v-if="diff && diff.note">{{ diff.note }}</x-note>
        </div>

        <div class="row spread mt" style="gap:8px;flex-wrap:wrap;align-items:center">
          <button class="btn ghost" :disabled="!hasChanges || busy || !writable" @click="revert">把改过的项还原</button>
          <span class="grow"></span>
          <button class="btn sm ghost" :disabled="diffLoading || !cluster || loading" @click="previewDiff">
            {{ diffLoading ? '预览中…' : '预览改动' }}</button>
          <x-hint label="这一页动的是哪些文件" text="这一页只动 cluster.ini 里的玩法项，改完下一次启动世界生效。游戏端口在「游戏端口」页签改；cluster_key 和目录名要动只能重建房间。"></x-hint>
          <button class="btn primary" :disabled="busy || !writable || !hasChanges || diffLoading"
                  :title="!writable ? readOnlyNote : ''" @click="save">
            {{ busy ? '写入中……' : '写入 cluster.ini' }}</button>
        </div>
        <div class="stat-note mono" style="word-break:break-all">{{ view.path }}</div>

        <!-- 历史版本：列出 /settings/backups，回滚前 askConfirm 说明后果 -->
        <div class="group mt">
          <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap;padding:8px 2px">
            <b>历史版本</b>
            <span class="chip" v-if="backups">留 {{ backups.keep }} 份</span>
            <span class="grow"></span>
            <button class="btn sm ghost" :disabled="backupsLoading" @click="loadBackups">
              {{ backupsLoading ? '读着…' : '重新读一次' }}</button>
          </div>
          <x-note kind="info" v-if="backups && backups.note">{{ backups.note }}</x-note>
          <div class="err-box" v-if="backupsError">{{ backupsError }}</div>
          <x-empty v-else-if="backups && !(backups.backups || []).length" icon="archive" title="还没有历史版本"
                   note="每次写入 cluster.ini 前后端会留一份旧版；改过一次之后这里就有内容。"></x-empty>
          <div class="table-wrap" v-else-if="backups && (backups.backups || []).length">
            <table class="data">
              <thead><tr><th>备份文件</th><th>大小</th><th>时间</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="b in backups.backups" :key="b.name">
                  <td class="mono">{{ b.name }}</td>
                  <td class="mono">{{ bytes(b.size) }}</td>
                  <td class="mono">{{ dateTime(b.modTime) }}</td>
                  <td>
                    <button class="btn sm" :class="writable ? 'danger' : 'ghost'"
                            :disabled="!writable || isBusy('cluster-settings:rollback:' + cluster)"
                            :title="!writable ? readOnlyNote : ''" @click="rollback(b)">回滚</button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <x-note kind="ok" v-if="rollbackNote">{{ rollbackNote }}</x-note>
        </div>
      </template>
    </section>`,
};

// ---- 游戏端口 ----

// 端口单独一个页签，而不是并进集群设置：改人数是"重启就生效"，改端口是
// "改错了玩家连不上、面板还显示着旧地址"。这一页要回答三个问题——
// 现在用的是哪几个号、要放行哪几个号、以及为什么现在不让我改。
const PortsCard = {
  props: { cluster: String },
  mixins: [RoomAccess],
  setup() { return { state, noRoomHint, act, placeName, openConnect }; },
  data() {
    return {
      view: null, loading: false, error: "", err: "", savedNote: "",
      form: { master: "", caves: "" },
      fieldErrors: {}, errNote: "",
    };
  },
  computed: {
    busy() { return !!state.busy["ports:" + this.cluster]; },
    // 房间的门牌名在总览那份列表里（cluster.ini 的 cluster_name），端口视图只带目录名，
    // 所以复制弹窗的标题去 state 里取，别把目录名当房间名显示。
    roomName() {
      const c = state.clusters.find((x) => x.name === this.cluster);
      return c && c.roomName ? c.roomName : this.cluster;
    },
    runningText() {
      if (!this.view) return "";
      return (this.view.runningWorlds || []).map((s) => placeName(s)).join("、");
    },
    mine() { return this.view ? (this.view.taken || []).filter((t) => t.self) : []; },
    others() { return this.view ? (this.view.taken || []).filter((t) => !t.self) : []; },
    // 要放行的号：每个世界两个。这一行是这一页最"值钱"的一句话——
    // 安全组少开一个号，面板上一切正常，玩家那边是"连接中"卡到超时。
    openPortsText() {
      if (!this.view) return "";
      const out = [this.view.masterPort, this.view.masterPort + 1];
      if (this.view.hasCaves) out.push(this.view.cavesPort, this.view.cavesPort + 1);
      // 去重：一组号本来就重叠的房间（手工造的坏夹具）会说成"10999、11000、10998、10999"，
      // 重复的号让人以为是四个，而这一页正是要人照着它开安全组。
      return [...new Set(out.filter((p) => p > 0))].join("、");
    },
    changed() {
      if (!this.view) return false;
      const m = Number(this.form.master), c = Number(this.form.caves);
      // 端口得是个像样的号才算改过：清空输入框（Number('') 是 0）不能把按钮点亮，
      // 否则一点就报"非法"，看起来像页面的毛病。
      const ok = (p) => Number.isInteger(p) && p >= 1024 && p <= 65534;
      if (ok(m) && m !== this.view.masterPort) return true;
      return !!(this.view.hasCaves && ok(c) && c !== this.view.cavesPort);
    },
    // 磁盘上配套端口和地表端口对不上（手工造的房间、或早期版本漏写那一行）。
    // 这一条只管"这一格是坏的"，所以页面上一律按它上色；能不能一键补回来是另一回事，
    // 见 intentFix。
    intentBad() {
      return !!this.view && this.view.intentPort !== this.view.masterPort + 1;
    },
    // 洞穴的 server.ini 里"去哪找地表"那一行空着：这是端口页能修的第二种毛病，
    // 症状和配套端口写错一模一样（洞穴起得来、进不去地面），引擎同样一句错都不报。
    cavesIpBad() {
      return !!this.view && this.view.hasCaves && !String(this.view.cavesMasterIp || "").trim();
    },
    // "点一下就补好"的承诺。它不能和 diskProblem 同时说：现在这一组号本身重叠的话，
    // 点下去只会得到一句"这两组重叠了"，那种情况要说的是毛病本身，不是补法。
    intentFix() {
      if (!this.view || this.diskProblem) return "";
      const want = this.view.masterPort + 1;
      const parts = [];
      if (this.intentBad) {
        parts.push(this.view.intentPort
          ? ("配套端口这一格读出来是 " + this.view.intentPort + "，但它必须等于地表端口 + 1（" + want + "），否则洞穴找不到地表")
          : ("cluster.ini 里没有配套端口那一行，洞穴会起得来但进不去地面（要补的是 " + want + "）"));
      }
      if (this.cavesIpBad) {
        parts.push("洞穴的 server.ini 里没写主机地址，光有端口它也连不上地表");
      }
      if (!parts.length) return "";
      return parts.join("；") + "。点「补齐端口配置」会把这三份文件一起补好，一个号都不用改。";
    },
    // 输入框还没动时，badRequest 报的就是磁盘上这一组号自己的毛病（不是用户填错了）。
    diskProblem() {
      if (!this.view || this.changed) return "";
      return this.badRequest();
    },
    canWrite() { return this.changed || !!this.intentFix; },
  },
  watch: {
    cluster: { immediate: true, handler() { this.load(); } },
    // 端口草稿（两个号都是数字，不敏感）：切页签销毁组件后靠它接回来。
    form: { deep: true, handler() { this.persist(); } },
  },
  methods: {
    draftName() { return this.cluster || "_"; },
    persist() {
      if (!this.form || !this.view) return;
      const changed = Number(this.form.master) !== this.view.masterPort ||
        (this.view.hasCaves && Number(this.form.caves) !== this.view.cavesPort);
      if (!changed) { this.clearSaved(); return; }
      writeDraft("ports", this.draftName(), { form: Object.assign({}, this.form) });
    },
    restoreDraft() {
      const d = readDraft("ports", this.draftName());
      if (!d || !d.form || !this.form) return;
      if (d.form.master !== undefined) this.form.master = d.form.master;
      if (d.form.caves !== undefined) this.form.caves = d.form.caves;
    },
    clearSaved() { clearDraft("ports", this.draftName()); },
    apply(v) {
      this.view = v;
      this.form = { master: String(v.masterPort || ""), caves: String(v.cavesPort || "") };
    },
    async load() {
      this.view = null; this.err = ""; this.error = ""; this.savedNote = "";
      clearFieldErrors(this);
      if (!this.cluster) return;
      this.loading = true;
      const res = await api.get("/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/ports");
      this.loading = false;
      if (res.ok) { this.apply(res.data); this.restoreDraft(); }
      else this.error = res.error || ("接口返回 " + res.status);
    },
    // 只发改过的那一栏：后端把"没带字段"和"带了一个 0"分得很清，前端不该糊上去。
    buildBody() {
      const body = {};
      if (Number(this.form.master) !== this.view.masterPort) body.masterPort = Number(this.form.master);
      if (this.view.hasCaves && Number(this.form.caves) !== this.view.cavesPort) body.cavesPort = Number(this.form.caves);
      // 只补配套端口的那一次也得带个号过去：后端要知道"配套端口该等于谁 + 1"。
      if (!body.masterPort && !body.cavesPort) body.masterPort = this.view.masterPort;
      return body;
    },
    useSuggested() {
      if (!this.view || !this.view.autoPort) return;
      this.form.master = String(this.view.autoPort);
      this.err = "";
    },
    // 这一条和后端 checkPort 是同一套规则。在这里先说一遍，是因为端口撞车
    // 要等按下"写入"才发现的话，人已经翻过一整屏数字了。
    badRequest() {
      const v = this.view;
      const m = Number(this.form.master);
      const c = v.hasCaves ? Number(this.form.caves) : 0;
      const inRange = (p) => Number.isInteger(p) && p >= 1024 && p <= 65534;
      if (!inRange(m)) return "地表端口要是 1024 到 65534 之间的整数：1024 以下需要 root 权限，最后一个号要留给配套端口。";
      if (v.hasCaves && !inRange(c)) return "洞穴端口要是 1024 到 65534 之间的整数。";
      if (v.hasCaves && (c === m || c === m + 1 || c + 1 === m)) {
        return "两个世界各要两个号：地表 " + m + " 和 " + (m + 1) + "，洞穴 " + c + " 和 " + (c + 1) + "，这两组重叠了，换一组。";
      }
      const mine = [m, m + 1];
      if (v.hasCaves) mine.push(c, c + 1);
      const hit = (v.taken || []).find((t) => !t.self && mine.indexOf(t.port) >= 0);
      if (hit) {
        return hit.port + " 已被 " + hit.room + " 的" + hit.world + "世界用作" +
          (hit.kind === "intent" ? "配套端口" : "游戏端口") + "，换一组号。";
      }
      return "";
    },
    async save() {
      this.err = ""; this.savedNote = "";
      clearFieldErrors(this);
      const bad = this.badRequest();
      if (bad) { this.err = bad; return; }
      // 改端口是可反向的：把保存前的那组号留作 12 秒撤销体。
      const undoBody = { masterPort: this.view.masterPort };
      if (this.view.hasCaves) undoBody.cavesPort = this.view.cavesPort;
      const url = "/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/ports";
      const res = await act("ports:" + this.cluster, "PUT", url, this.buildBody(),
        { okTitle: "端口已经写进文件", badTitle: "这次没写进去",
          onError: (r) => attachError(this, r, PORTS_ERR_DEFS),
          undo: { label: "撤销刚才的端口改动", method: "PUT", path: url,
                  body: undoBody, seconds: 12 } });
      if (!res.ok) return;   // onError 已把原文挂到字段与 Note
      this.apply(res.data);
      this.clearSaved();
      this.savedNote = res.data.note || "";
    },
  },
  template: `
    <section class="card">
      <div class="card-head">
        <span class="ico-tile tint-cyan"><x-icon name="signal" :size="16"></x-icon></span>
        <h2>游戏端口</h2>
        <span class="chip" v-if="view">{{ runningText ? runningText + ' 在跑' : '世界都没在跑' }}</span>
        <span class="grow"></span>
        <button class="btn sm ghost" :disabled="loading || !cluster" @click="load">重新读一次</button>
      </div>

      <x-sk v-if="loading" :lines="4"></x-sk>
      <div class="err-box" v-else-if="error">{{ error }}</div>
      <x-empty v-else-if="!cluster" icon="servers" title="这台机器上还没有房间"
               :note="noRoomHint()"></x-empty>
      <x-empty v-else-if="!view" icon="warn" title="没读到端口"
               note="端口读自 cluster.ini 和每个世界的 server.ini；这三份文件读不出号时，这一页不猜一个号给你看。"></x-empty>

      <template v-else>
        <x-note v-if="runningText" kind="warn">
          <b>{{ runningText }} 正在运行，这一页暂时不能写入。</b>端口只在世界启动时读一次：
          正在跑的世界还守着旧号，这时候改完，面板显示的加入地址和复制出来的连接命令都是连不上的那个。
          先把世界停下来，再点上面的「重新读一次」。
        </x-note>
        <x-note v-else-if="savedNote" kind="ok">{{ savedNote }}</x-note>
        <x-note v-else kind="info">
          玩家连进来用的是 IP 加地表那一个号；每个世界还会同时绑紧跟它的号，
          所以云安全组和防火墙要放行的是 <b class="mono">{{ openPortsText }}</b> 这几个 UDP 端口。
        </x-note>
        <x-note v-if="diskProblem && !runningText" kind="warn">{{ diskProblem }}</x-note>
        <x-note v-else-if="intentFix && !runningText" kind="warn">{{ intentFix }}</x-note>
        <x-note v-if="readOnlyNote" kind="warn">{{ readOnlyNote }}</x-note>

        <div class="grid grid-3 mt">
          <x-stat label="地表游戏端口" icon="globe" tint="tint-cyan" :value="view.masterPort"
                  hint="玩家在服务器列表里连的就是这个号；改它要先把世界停下来。">
            <span class="mono">c_connect 用的就是它</span>
          </x-stat>
          <x-stat label="配套端口" icon="shield" :tint="intentBad ? '' : 'tint-gold'"
                  :tone="intentBad ? 'warn' : ''" :value="view.intentPort || '没写到文件'"
                  hint="Klei 的硬规则：这个号永远等于地表端口 + 1，写在 cluster.ini 和洞穴的 server.ini 里，洞穴靠它找地面。游戏里看不到它，但一样占着。">
            <span class="mono">{{ intentBad ? '这一格和地表端口对不上' : '不用填，跟着地表走' }}</span>
          </x-stat>
          <x-stat v-if="view.hasCaves" label="洞穴游戏端口" icon="portal" tint="tint-violet" :value="view.cavesPort"
                  hint="洞穴自己也绑两个号（这一个和紧跟它的）。玩家不直接连它，穿传送门时是服务端内部用的。"></x-stat>
          <x-stat v-else label="洞穴游戏端口" icon="portal" tint="tint-slate" value="没有洞穴世界"
                  hint="这个房间只有一个世界。要洞穴得重建房间（面板目前不在已有房间里加世界）。"></x-stat>
        </div>

        <div class="grid-form mt">
          <div class="field">
            <div class="field-head">
              <label>地表游戏端口</label>
              <x-hint text="可以填一个自己熟的号（比如 10999），省掉去查面板自动挑了哪个；它后面那个号会被自动占用。"></x-hint>
            </div>
            <div class="row" style="gap:8px;align-items:flex-end">
              <input type="number" class="grow" v-model="form.master" min="1024" max="65534"
                     :disabled="!!runningText" :class="changed ? 'changed' : ''">
              <button class="btn sm ghost" type="button" v-if="view.autoPort && !runningText"
                      @click="useSuggested">用面板挑的 {{ view.autoPort }}</button>
            </div>
            <x-field-error :text="fieldErrors.master"></x-field-error>
            <span class="hint" v-if="view.autoPort">面板自动挑号的区间是配置里的 {{ view.autoRange }}；
              填在区间外也能用，只是以后自动挑号不会再挑到它。</span>
          </div>
          <div class="field" v-if="view.hasCaves">
            <div class="field-head">
              <label>洞穴游戏端口</label>
              <x-hint text="不能和地表的两个号重叠：地表用 n 和 n+1，洞穴就得另起一组。"></x-hint>
            </div>
            <input type="number" v-model="form.caves" min="1024" max="65534" :disabled="!!runningText">
          </div>
        </div>

        <div class="col mt" style="gap:6px" v-if="mine.length || others.length">
          <div class="row" style="gap:6px" v-if="mine.length">
            <span class="hint">这个房间占的号</span>
            <span class="chip mono" v-for="t in mine" :key="t.port"
                  :title="t.world + '世界 · ' + t.kindLabel">{{ t.port }}</span>
          </div>
          <div class="row" style="gap:6px" v-if="others.length">
            <span class="hint">别的房间占的号</span>
            <span class="chip mono warn" v-for="t in others" :key="t.port"
                  :title="t.room + ' 的' + t.world + '世界 · ' + t.kindLabel">{{ t.port }}</span>
          </div>
          <span class="hint" v-if="others.length">
            这些号已经被别的房间用着，往它们上改会连不上；配套端口那几枚在游戏列表里看不见，但同样占着。</span>
        </div>

        <div class="err-box mt" v-if="err">{{ err }}</div>
        <x-note class="mt" kind="warn" v-if="errNote">{{ errNote }}</x-note>
        <x-field-error :text="fieldErrors._form"></x-field-error>
        <div class="row spread mt" style="gap:8px;flex-wrap:wrap;align-items:center">
          <button class="btn" @click="openConnect(cluster, roomName)">复制加入命令</button>
          <span class="grow"></span>
          <x-hint label="这一页动的是哪些文件" text="写入会一次改三份：cluster.ini 的 master_port、地表的 server_port、洞穴的 server_port（还有洞穴那行的 master_port 与主机地址）。少改一份就是洞穴连不上地面，而引擎不会报错。"></x-hint>
          <button class="btn primary" :disabled="busy || !writable || !canWrite || !!runningText"
                  :title="!writable ? readOnlyNote : ''" @click="save">
            {{ busy ? '写入中……' : (changed ? '写入端口' : (intentFix ? '补齐端口配置' : '写入端口')) }}</button>
        </div>
        <div class="stat-note mono" style="word-break:break-all">{{ view.path }}</div>
      </template>
    </section>`,
};

// ---- 房间抬头：这一页现在讲的是哪个房间 ----
//
// 原来这一页第一屏只有一个下拉框：人在这里改世界规则时，看不到自己改的房间
// 在不在跑、几个人、第几天、能不能进——那些全在总览页。事实条 + 动作条放在
// 标签页上面，是为了让"看情况 → 动手"在这一屏里走完，不用来回切页。
//
// 数字只从 /api/v1/rooms 取：它把 cluster.ini、server.ini、助手回传三处的东西
// 拼成了一份，面板这边不再自己凑（凑出来的口径和总览页对不上就是两头打脸）。
const RoomHeader = {
  props: { cluster: { type: String, required: true } },
  mixins: [RoomAccess],
  setup() { return { state, isBusy, keyOf, num, rel, openConnect, openOwner, go }; },
  data() { return { rows: [], loading: false, error: "" }; },
  computed: {
    // 整批启停期间（actMany 占住一个 busy 键）把启/停/备份都禁用，防重复提交。
    batchBusy() {
      return isBusy("batch:" + this.cluster + ":start") || isBusy("batch:" + this.cluster + ":stop");
    },
    room() { return state.clusters.find((c) => c.name === this.cluster) || null; },
    liveShards() { return this.room ? this.room.shards || [] : []; },
    runningCount() { return this.liveShards.filter((s) => s.running).length; },
    // "归谁管"这一格按归属分两种颜色：未指派（灰）和派给了人（青）。
    // 只说"归 alice 管"不够——管理员要在这一格上决定"要不要派"，成员要靠它明白自己为什么改不动。
    ownerTint() { return this.isUnowned ? "tint-slate" : "tint-cyan"; },
    isUnowned() {
      const r = this.room;
      return !r || !r.ownerAccountId;
    },
    // 地表那一行是这个房间的"门面"：模式、端口、谁能进都记在它身上。
    master() { return this.rows.find((r) => r.cluster === this.cluster && r.isMaster) || null; },
    // 一个房间里两个世界各报各的天数与人数，只有"助手此刻真的在报数"的那些能相加；
    // worldReady 就是接口给的那把尺子，过期快照里的数不算现在。
    live() { return this.rows.filter((r) => r.cluster === this.cluster && r.worldReady); },
    // 天数那一格点名要地表：两个世界各算各的 cycles，而"第几天"这句话默认说的是玩家
    // 站着的那块地。只有洞穴在回话时退回洞穴，但 hint 要跟着换成实话，不能还说"取地表"。
    dayRow() {
      const live = this.live;
      return live.find((r) => r.isMaster) || live[0] || null;
    },
    facts() {
      const m = this.master;
      const live = this.live;
      const out = [];
      // 归属排在第一格：它决定这一页上其余每一格能不能被改。
      // 说法（"归 alice 管" / "未指派"）整个由后端拼好，前端不拿 accountId 去凑人名——
      // 成员账号打不开账号名单，凑不出来就会显示成"归  管"。
      const ownerNote = assignedText(this.room);
      out.push({ icon: "user", tint: this.ownerTint, label: "归谁管",
        value: this.room ? (this.room.ownerLabel || "读不到") : "读不到",
        hint: ownerNote
          ? ownerNote + "。未指派的房间由管理员自己管，成员看得见但改不动。"
          : "这间还没指派给谁：未指派就是管理员自己在管，成员看得见但改不动。要派出去点下面「指派归属」。" });
      out.push({ icon: "flame", tint: "tint-coral", label: "游戏模式",
        value: m ? (m.gameModeLabel || "没填这一项") : "读不到配置" });
      // 谁能进：accessLabel 那句完整的话（"已注册到公网服务器列表"）塞进这一格
      // 一定会被省略号截断，所以格子里放短说法、整句收进旁边的问号气泡。
      out.push({ icon: "shield", tint: "tint-amber", label: "谁能进",
        value: m ? (m.accessShort || "读不出来") : "读不到配置",
        hint: m ? m.accessLabel : "" });
      out.push({ icon: "users", tint: "tint-teal", label: "在线人数",
        value: live.length
          ? (live.reduce((n, r) => n + (Number(r.online) || 0), 0) + " / " + (m ? m.maxPlayers : "?"))
          : (this.runningCount ? "还没回话" : "没在跑"),
        hint: "地表和洞穴各报各的人数，这一格是两边相加；上限是集群设置里那个总人数。" });
      // 这一季的第几天/还剩几天由后端判过才给（判据见 internal/modbridge/status.go）：
      // 季节不轮换的老世界给不出这句话，接口就不给这个键，这里也就一个字都不补。
      const d = this.dayRow;
      const seasonDay = d && d.dayInSeason
        ? ("这一季走到第 " + num(d.dayInSeason) + " 天" +
            (d.daysLeftInSeason === undefined || d.daysLeftInSeason === null ? ""
              : (d.daysLeftInSeason > 0 ? "，还剩 " + num(d.daysLeftInSeason) + " 天" : "，今天过完就换季")) +
            "；换季时重新从第 1 天算。")
        : "";
      out.push({ icon: "calendar", tint: "tint-rose", label: "天数进度",
        value: d
          ? ("第 " + num(d.day) + " 天 · " + d.seasonLabel +
              (d.dayInSeason ? ("（第 " + num(d.dayInSeason) + " 天）") : "") + " " + d.phaseLabel)
          : (this.runningCount ? "还没进世界" : "没在跑"),
        hint: seasonDay + (d && d.isMaster
          ? "取地表这一侧的进度。洞穴没有地表这套季节与昼夜。"
          : "地表还没回话，这一格是洞穴那边的进度。") });
      out.push({ icon: "link", tint: "tint-azure", label: "游戏端口",
        value: m && m.port ? String(m.port) : "读不到配置",
        hint: "玩家连的是地表这个端口；洞穴那个端口只给服务端内部传送用。" });
      out.push({ icon: "cube", tint: "tint-gold", label: "模组",
        // 读到文件却一条没有 = 没装；压根没读到（modoverrides.lua 解析失败）才说"没读到"。
        // 早先两种情况都写成"没装模组"，那是替服务端下了一个它没给的结论。
        value: m && m.modsTotal !== null && m.modsTotal !== undefined
          ? (m.modsTotal ? ("开着 " + m.modsEnabled + " / 共 " + m.modsTotal) : "没装模组")
          : "没读到",
        hint: "这一格只数地表。两边模组对不对得齐，去模组那一页的一致性检查里看。" });
      out.push({ icon: "layers", tint: "tint-violet", label: "地形预设",
        value: m && m.presetLabel ? m.presetLabel : "没读到",
        hint: "生成一张什么图由这一项决定，要改它去「世界规则」那一栏。" });
      const backups = this.rows.filter((r) => r.cluster === this.cluster && r.lastBackupAt)
        .map((r) => r.lastBackupAt).sort();
      out.push({ icon: "archive", tint: "tint-green", label: "最近备份",
        value: backups.length ? rel(backups[backups.length - 1]) : "还没备份过",
        hint: backups.length ? "" : "恢复只能用已经存在的备份，所以这一步不能省。" });
      return out;
    },
  },
  watch: { cluster() { this.load(); } },
  mounted() { this.load(); },
  methods: {
    async load() {
      this.loading = true;
      const res = await api.get("/api/v1/rooms");
      this.loading = false;
      if (res.ok) {
        this.rows = (res.data && res.data.items) || [];
        this.error = "";
      } else {
        this.rows = [];
        this.error = res.error || ("接口返回 " + res.status);
      }
    },
    // 启/停一律走 actMany：串行执行、整批占一个 busy 键、最后只给一条汇总 Toast，
    // 不再 N 个世界叠 N 条 Toast；批量期间 batchBusy 把相关按钮全禁用。
    async startAll() {
      const list = this.liveShards.filter((s) => !s.running);
      if (!list.length) return;
      const ok = await askConfirm({
        title: "启动这 " + list.length + " 个世界？",
        body: "地表和洞穴要一起起来，洞穴里才进得去人。第一次进世界要生成地图，通常一到两分钟。",
        okLabel: "启动",
      });
      if (!ok) return;
      const items = list.map((s) => ({
        method: "POST",
        path: `/api/v1/clusters/${encodeURIComponent(this.cluster)}/shards/${encodeURIComponent(s.name)}/start`,
        body: {}, label: s.name + " 启动",
      }));
      const r = await actMany("batch:" + this.cluster + ":start", items,
        { okTitle: "这 " + list.length + " 个世界都启动完了", badTitle: "有世界没启起来" });
      // actMany 不接 opts.undo，反向撤销改由 store.pushUndo 挂整批（启动 → 停止）。
      pushUndo("撤销刚才启动的 " + list.length + " 个世界", async () => {
        const back = list.map((s) => ({
          method: "POST",
          path: `/api/v1/clusters/${encodeURIComponent(this.cluster)}/shards/${encodeURIComponent(s.name)}/stop`,
          body: { saveFirst: true }, label: s.name + " 停止",
        }));
        await actMany("batch:" + this.cluster + ":stop", back, { quiet: true });
      }, 12);
      this.load();
      return r;
    },
    async stopAll() {
      const list = this.liveShards.filter((s) => s.running);
      if (!list.length) return;
      const ok = await askConfirm({
        title: "停止这 " + list.length + " 个世界？",
        body: "会先给每个世界注入存盘命令，再等它自己退出，这是 Klei 默认的落盘顺序。",
        warn: "在线玩家会掉线；正在进行的战斗与掉落物停在存档里。",
        okLabel: "停止",
      });
      if (!ok) return;
      const items = list.map((s) => ({
        method: "POST",
        path: `/api/v1/clusters/${encodeURIComponent(this.cluster)}/shards/${encodeURIComponent(s.name)}/stop`,
        body: { saveFirst: true }, label: s.name + " 停止",
      }));
      const r = await actMany("batch:" + this.cluster + ":stop", items,
        { okTitle: "这 " + list.length + " 个世界都落盘退出了", badTitle: "有世界没停成" });
      // 停服 → 启服：整批反向撤销。
      pushUndo("撤销刚才停止的 " + list.length + " 个世界", async () => {
        const back = list.map((s) => ({
          method: "POST",
          path: `/api/v1/clusters/${encodeURIComponent(this.cluster)}/shards/${encodeURIComponent(s.name)}/start`,
          body: {}, label: s.name + " 启动",
        }));
        await actMany("batch:" + this.cluster + ":start", back, { quiet: true });
      }, 12);
      this.load();
      return r;
    },
    async backup() {
      const res = await act("backup.create:" + this.cluster, "POST",
        "/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/backups", { cluster: this.cluster },
        { okTitle: "备份任务已接住", okBody: (x) => (x && x.note) || "",
          badTitle: "备份没排进队列", clusters: false });
      if (res.ok) this.load();
    },
  },
  template: `
    <section class="card tinted">
      <div class="card-head">
        <span class="ico-tile tint-cyan"><x-icon name="house" :size="16"></x-icon></span>
        <h2>{{ room ? (room.roomName || room.name) : cluster }}</h2>
        <x-badge v-if="runningCount" kind="running"
                 :text="'在跑 ' + runningCount + '/' + liveShards.length"></x-badge>
        <x-badge v-else-if="liveShards.length" kind="idle" text="都没在跑"></x-badge>
        <span class="grow"></span>
        <!-- 换房间的下拉框挂在标题同一行：它和标题讲的是同一个东西，
             分成两行就会读成"上面是名字、下面是另一个名字"。 -->
        <slot name="picker"></slot>
        <button class="btn sm ghost" @click="load()" :disabled="loading">重新读一遍</button>
      </div>

      <div class="err-box" v-if="error">{{ error }}</div>
      <div class="muted" style="font-size:12.5px" v-else-if="loading && !master">这个房间的情况还在读……</div>

      <div class="facts">
        <div class="fact" :class="f.tint" v-for="f in facts" :key="f.label">
          <x-icon :name="f.icon" :size="17"></x-icon>
          <div><i>{{ f.label }}<x-hint v-if="f.hint" :text="f.hint" :size="12"></x-hint></i><b>{{ f.value }}</b></div>
        </div>
      </div>

      <div class="action-row mt">
        <!-- 写按钮一律渲染：只读账号看到的是"灰掉 + :title 写明为什么"（x-perm-btn），
             而不是整排消失——消失会让人以为这些功能根本不存在。 -->
        <x-perm-btn v-if="runningCount < liveShards.length"
                    :allowed="writable && !!liveShards.length"
                    :reason="writable ? '这个房间没有世界可启动' : readOnlyNote"
                    :busy="batchBusy" tone="lg primary" @go="startAll">
          <x-icon name="play" :size="15"></x-icon>启动世界</x-perm-btn>
        <x-perm-btn :allowed="writable" :reason="readOnlyNote"
                    :busy="batchBusy || isBusy('backup.create:' + cluster)" tone="lg" @go="backup">
          <x-icon name="archive" :size="15"></x-icon>{{ isBusy('backup.create:' + cluster) ? '排队中……' : '立即备份' }}</x-perm-btn>
        <span class="assign-note" v-if="!writable">
          <x-icon name="lock" :size="14"></x-icon><span>{{ readOnlyNote }}</span>
        </span>
        <!-- 指派只有 canAssignOwner 的角色能按；成员/只读账号整个不出现（不是本页的写按钮）。 -->
        <button class="btn lg" v-if="canAssign" @click="openOwner(cluster, room ? room.roomName : '')">
          <x-icon name="user" :size="15"></x-icon>指派归属</button>
        <button class="btn lg" @click="openConnect(cluster, room ? room.roomName : '')">
          <x-icon name="link" :size="15"></x-icon>取加入命令</button>
        <button class="btn lg" @click="go('map')">
          <x-icon name="map" :size="15"></x-icon>看世界地图</button>
      </div>
      <!-- 停止世界是实心红的破坏性动作，单独收进 .actions-danger，不再和「看世界地图」并排等大。 -->
      <div class="actions-danger mt" v-if="liveShards.length && runningCount >= liveShards.length">
        <x-perm-btn :allowed="writable" :reason="readOnlyNote" :busy="batchBusy"
                    tone="lg danger solid" @go="stopAll">
          <x-icon name="stop" :size="15"></x-icon>停止世界</x-perm-btn>
        <span class="hint">在线玩家会掉线；停止前会先让每个世界存盘。</span>
      </div>
    </section>`,
};

// ---- 远程指令 ----

// 分片名在界面上说"地表/洞穴"；认不得的分片名原样留着——那多半是用户自己在
// server.ini 里改过的名字，不是我们能翻的码值。这一页不再自己抄一张表，
// 统一走 gameinfo.js 的 placeName。
// 用它仍要写进下面那个 setup() 的返回值：模板只看得见组件身上的成员，
// 光 import 进来用会得到"placeName is not a function"，整栏渲染不出来。

// 指令 key → 图标。这一排是十来枚一模一样的圆角块，只有图标能让手在鼠标上
// 停一下就知道点的是哪条；后端加了新 key 而这张表没跟上时退回 terminal，不留空白。
const CMD_ICON = {
  save: "save", rollback: "history", reset_rollback: "refresh", announce: "megaphone",
  netstats: "signal", worldstate: "list", auto_teleport: "locate", skip_days: "clock",
  force_phase: "sun", regenerate_shard: "shuffle",
};

// 这一排的顺序。接口给的是按 key 排的字母序（announce、auto_teleport、netstats、save……），
// 那个顺序对写代码的人有意义、对站着看面板的人没有：天天按的是"立即保存"，
// 一年按不了一次"重生这个世界"，而后者按错了要重开一档。所以常用在前、危险垫底，
// 认不得的新指令排在最后面（宁可排错位置，也不要把它挤到"立即保存"旁边）。
const CMD_ORDER = ["save", "announce", "netstats", "worldstate", "auto_teleport",
                   "skip_days", "force_phase", "rollback", "reset_rollback", "regenerate_shard"];

// CommandConsole 是这一版新加的一栏。后端早就有 /api/v1/admin-commands 这条白名单
// 通道（internal/dst/console.go），但整站没有一个界面在用它——等于开好了门没装把手。
// 这里刻意不做"命令输入框"：界面只能从服务端列出的这几条里挑，
// 任何手打的东西都到不了引擎，这条边界是这一栏存在的前提。
const CommandConsole = {
  props: { cluster: { required: true } },
  mixins: [RoomAccess],
  // isBusy 是 store 里导出的函数，模板看不见它——只有挂在实例上的那一份能用。
  // 这一栏的按钮要在"同一条指令正在发"的时候自己变灰，所以非挂不可。
  setup() { return { isBusy, placeName, noRoomHint }; },
  data() {
    return { cmds: [], loading: false, error: "",
             shard: "", picked: "", args: {}, last: null };
  },
  computed: {
    shards() {
      const c = state.clusters.find((x) => x.name === this.cluster);
      return c ? c.shards || [] : [];
    },
    runningCount() { return this.shards.filter((s) => s.running).length; },
    ordered() {
      const rank = (k) => { const i = CMD_ORDER.indexOf(k); return i < 0 ? CMD_ORDER.length : i; };
      return this.cmds.slice().sort((a, b) => rank(a.key) - rank(b.key));
    },
    current() { return this.cmds.find((c) => c.key === this.picked) || null; },
    // 参数没填齐就不给发。服务端本来也会拒，但那样要点一次、看一次红条才知道，
    // 而这一栏的按钮是"发一条命令进正在跑的世界"，不该拿它当校验器用。
    // 改不动的房间同样算"发不了"：这一栏讲的是往正在跑的世界里下命令，那是操作不是查看。
    blocked() {
      const c = this.current;
      if (!c || !this.shard || !this.writable) return true;
      for (const a of c.args) {
        const v = this.args[a.name];
        const s = v === undefined || v === null ? "" : String(v).trim();
        if (!s) return true;
        if (a.kind === "int") {
          const n = Number(s);
          if (!Number.isInteger(n)) return true;
          if (a.min && n < a.min) return true;
          if (a.max && n > a.max) return true;
        }
        if (a.kind === "enum" && !(a.choices || []).some((x) => x.value === s)) return true;
      }
      return false;
    },
  },
  watch: {
    cluster() { this.pickShard(); this.picked = ""; this.last = null; },
    shards() { this.pickShard(); },
    shard() { this.last = null; },
  },
  async mounted() {
    this.pickShard();
    if (!this.cmds.length) this.load();
  },
  methods: {
    icon(key) { return CMD_ICON[key] || "terminal"; },
    async load() {
      this.loading = true;
      const res = await api.get("/api/v1/admin-commands");
      this.loading = false;
      if (res.ok) {
        this.cmds = res.data || [];
        this.error = "";
      } else {
        this.cmds = [];
        this.error = res.error || ("接口返回 " + res.status);
      }
    },
    pickShard() {
      const names = this.shards.map((s) => s.name);
      if (names.indexOf(this.shard) < 0) this.shard = names[0] || "";
    },
    pick(cmd) {
      this.last = null;
      if (this.picked === cmd.key) { this.picked = ""; return; }
      this.picked = cmd.key;
      const a = {};
      for (const x of cmd.args) {
        const first = x.choices && x.choices.length ? x.choices[0].value : "";
        a[x.name] = x.default !== undefined && x.default !== "" ? x.default : first;
      }
      this.args = a;
    },
    async send() {
      const c = this.current;
      if (!c || this.blocked) return;
      if (c.danger) {
        // 重生世界 / 强停这类会清掉当前存档的指令不可反向：确认框把后果说死，
        // 也不挂撤销条（没有反向接口，伪造一条只会让人以为还退得回去）。
        const irreversible = c.key === "regenerate_shard"
          ? "这一条会清掉当前存档、重造世界：现有地图、建筑和进度都没了，且不能撤销。"
          : "这一条会动当前世界的状态，且不能撤销。";
        const ok = await askConfirm({
          title: c.label + "·发到" + this.placeName(this.shard),
          body: c.desc,
          warn: irreversible + "先「立即备份」一次，出问题才有地方回。",
          danger: true, okLabel: "确定执行",
        });
        if (!ok) return;
      }
      const res = await act("admin:" + keyOf(this.cluster, this.shard) + ":" + c.key, "POST",
        "/api/v1/clusters/" + encodeURIComponent(this.cluster) +
          "/shards/" + encodeURIComponent(this.shard) + "/admin-commands",
        { command: c.key, args: this.args, confirm: !!c.danger },
        { okTitle: c.label + "已经发出去了", okBody: (d) => (d && d.line) || "",
          badTitle: c.label + "没发出去", clusters: false });
      if (res.ok) this.last = res.data;
    },
  },
  template: `
    <section class="card tinted">
      <div class="card-head">
        <span class="ico-tile tint-azure"><x-icon name="terminal" :size="16"></x-icon></span>
        <h2>远程指令</h2>
        <span class="chip" v-if="loading">可发的指令读取中……</span>
        <span class="chip" v-else>{{ cmds.length }} 条白名单</span>
        <span class="grow"></span>
        <div class="row picker">
          <label for="cmd-shard">发到哪个世界</label>
          <select id="cmd-shard" v-model="shard">
            <option v-for="s in shards" :key="s.name" :value="s.name">
              {{ placeName(s.name) }}{{ s.running ? '' : ' · 没在跑' }}</option>
          </select>
          <x-hint text="指令是打进这个世界的服务端控制台会话的，所以只有它在跑的时候才收得到。地表与洞穴各管各的存档与玩家。"></x-hint>
        </div>
        <button class="btn sm ghost" @click="load()" :disabled="loading">重新读一遍</button>
      </div>

      <!-- 没有房间时这一栏连"这个房间"都没有，那排灰按钮和"先点上面那排里的启动世界"
           都是指不存在的东西；发命令这件事的前提是一个正在跑的世界，先把这一步说清。 -->
      <x-empty v-if="!cluster" icon="servers" title="这台机器上还没有房间" :note="noRoomHint()"></x-empty>
      <div class="err-box" v-else-if="error">{{ error }}</div>
      <template v-else>
        <x-note kind="warn" v-if="!runningCount">
          这个房间现在没有世界在跑，指令发过去也没有进程收。要试这一栏，先点上面那排里的「启动世界」。
        </x-note>

        <div class="ops-row">
          <button v-for="c in ordered" :key="c.key" type="button"
                  class="op" :class="[c.danger ? 'danger' : '', picked === c.key ? 'on' : '']"
                  @click="pick(c)">
            <x-icon :name="icon(c.key)" :size="14"></x-icon>{{ c.label }}
            <i v-if="c.args.length">{{ c.args.length }} 项要填</i>
          </button>
        </div>

        <div class="op-panel" v-if="current">
          <p class="op-desc">{{ current.desc }}</p>
          <div class="op-args" v-if="current.args.length">
            <div class="field" v-for="a in current.args" :key="a.name">
              <div class="field-head">
                <label :for="'op-a-' + a.name">{{ a.label }}</label>
                <x-hint v-if="a.kind === 'int' && (a.min || a.max)"
                        :text="'可以填 ' + (a.min || 0) + ' 到 ' + (a.max || '不限') + '。'"></x-hint>
              </div>
              <select v-if="a.kind === 'enum'" :id="'op-a-' + a.name" v-model="args[a.name]">
                <option v-for="o in a.choices" :key="o.value" :value="o.value">{{ o.label }}</option>
              </select>
              <input v-else-if="a.kind === 'int'" :id="'op-a-' + a.name" type="number"
                     :min="a.min" :max="a.max" v-model="args[a.name]" />
              <input v-else :id="'op-a-' + a.name" type="text" :maxlength="a.maxLen"
                     v-model="args[a.name]" :placeholder="a.label" />
            </div>
          </div>
          <div class="row" style="gap:10px;align-items:center">
            <button class="btn lg" :class="current.danger ? 'danger' : 'primary'"
                    :disabled="blocked || isBusy('admin:' + cluster + '/' + shard + ':' + current.key)"
                    @click="send()">
              <x-icon name="power" :size="15"></x-icon>{{ current.label }}</button>
            <span class="muted" style="font-size:12.5px" v-if="!shard">先选一个世界</span>
            <span class="muted" style="font-size:12.5px" v-else-if="blocked && current.args.length">还有参数没填对</span>
            <span class="muted" style="font-size:12.5px" v-else>发到{{ placeName(shard) }}</span>
          </div>
        </div>

        <div class="op-last" v-if="last">
          <div class="row" style="gap:8px;flex-wrap:wrap">
            <span class="muted" style="font-size:12.5px">实际打进控制台的是这一行：</span>
            <code>{{ last.line }}</code>
          </div>
          <div class="muted" style="font-size:12.5px" v-if="last.hint">{{ last.hint }}</div>
        </div>
      </template>
    </section>`,
};

// ---- 页面 ----

export default {
  name: "Room",
  components: { "x-wizard": Wizard, "x-token": TokenCard, "x-world": WorldCard,
                "x-cluster": ClusterSettingsCard, "x-ports": PortsCard,
                "x-room-head": RoomHeader, "x-cmd": CommandConsole },
  // query 就是 route.query：hash 前进/后退时它变、但 path 不变、组件不重建。
  props: { query: { type: Object, default: () => ({}) } },
  setup() { return { state }; },
  data() { return { tab: tabFromUrl(this.query), schema: [], schemaErr: "", schemaLoading: true }; },
  watch: {
    // 用户点页签：标识写进 hash query（setQuery 只改 query，不销毁组件，向导草稿不丢）。
    tab(v) { setQuery({ tab: v }); },
    // 刷新 / 前进后退：hash 变了但组件不重建，只能靠 watch 把 tab 同步回来。
    // 不在 mounted 里写 hash，也不在这里回写，避免 hash 变化来回打转。
    "query.tab"(v) {
      const t = tabFromUrl({ tab: v });
      if (t !== this.tab) this.tab = t;
    },
  },
  computed: {
    // 建房间改的是这台机器的目录，不属于任何一间房，所以它归管理员（后端那条判断在
    // scope.go：成员打 POST /api/v1/clusters 会被拒）。向导这一栏对成员整个不出现，
    // 而不是摆一张填完按下去报红的表——填到第三步才被拒是最亏的一次。
    isAdminAccount() { return !!(state.me && state.me.isAdmin); },
    // 成员落在向导（地址里带 ?tab=wizard 或第一次进来）时换到远程指令那一栏：
    // 认不出的落点不该是一页空白。
    shownTab() { return this.isAdminAccount || this.tab !== "wizard" ? this.tab : "cmd"; },
    cluster: {
      get() { return state.sel.cluster; },
      set(v) { state.sel.cluster = v; },
    },
    shards() {
      const c = state.clusters.find((x) => x.name === this.cluster);
      return c ? c.shards || [] : [];
    },
  },
  async mounted() {
    this.schemaLoading = true;
    const r = await loadSchema();
    this.schema = r.data;
    this.schemaErr = r.error;
    this.schemaLoading = false;
  },
  methods: {
    use(t) {
      // 向导建完之后的"去总览开它"要跳页，不能只改本页的 tab。
      if (t === "overview") { location.hash = "#/"; return; }
      this.tab = t;
    },
  },
  template: `
    <div class="col" style="gap:16px">
      <x-room-head v-if="cluster" :cluster="cluster">
        <template #picker>
          <div class="row picker">
            <label for="room-pick">当前房间</label>
            <select id="room-pick" v-model="cluster">
              <option value="" disabled>选一个房间</option>
              <option v-for="c in state.clusters" :key="c.name" :value="c.name">{{ c.name }}</option>
            </select>
            <span class="chip" v-if="schemaLoading">世界规则字典读取中……</span>
            <span class="chip" v-else-if="schemaErr" :title="schemaErr">字典没拿到</span>
            <span class="chip" v-else>{{ schema.length }} 张地形表</span>
          </div>
        </template>
      </x-room-head>

      <!-- 一间都没有时这一排整个不收起来的话，屏幕上留下的是一个只有"选一个房间"一项的下拉，
           和一枚孤零零的「2 张地形表」——那枚芯片说的是世界规则字典有几张表，跟房间无关，
           但摆在这个位置只会被读成"这个房间有 2 张表"。下面那句蓝底说明才是这一屏的结论。 -->
      <div class="row" style="gap:12px;flex-wrap:wrap;align-items:flex-end"
           v-else-if="state.clusters.length">
        <div class="field" style="min-width:180px;flex:1">
          <label>当前房间</label>
          <select v-model="cluster">
            <option value="" disabled>选一个房间</option>
            <option v-for="c in state.clusters" :key="c.name" :value="c.name">{{ c.name }}</option>
          </select>
        </div>
        <span class="chip" v-if="schemaLoading">世界规则字典读取中……</span>
        <span class="chip" v-else-if="schemaErr" :title="schemaErr">字典没拿到</span>
        <span class="chip" v-else>{{ schema.length }} 张地形表</span>
      </div>

      <x-note v-if="!state.clusters.length" kind="info">
        <template v-if="isAdminAccount">这台机器上还没有房间。开服向导建的就是第一个：<b>目录、cluster.ini、世界端口、助手 MOD</b> 一次配齐。</template>
        <template v-else>这台机器上现在没有派给你的房间：要么一间都没建，要么建的房间还没指派。
          建房间和指派都归管理员，这一页你只能看到自己名下的那几间。</template>
      </x-note>

      <div class="tabs">
        <button :class="shownTab === 'wizard' ? 'on' : ''" v-if="isAdminAccount" @click="tab = 'wizard'">开服向导</button>
        <button :class="shownTab === 'cmd' ? 'on' : ''" @click="tab = 'cmd'">远程指令
          <span class="tab-count">{{ cluster || '还没有房间' }}</span></button>
        <button :class="shownTab === 'world' ? 'on' : ''" @click="tab = 'world'">世界规则
          <span class="tab-count">{{ shards.length }} 个世界</span></button>
        <button :class="shownTab === 'cluster' ? 'on' : ''" @click="tab = 'cluster'">集群设置</button>
        <button :class="shownTab === 'ports' ? 'on' : ''" @click="tab = 'ports'">游戏端口
          <span class="tab-count">{{ cluster || '还没有房间' }}</span></button>
        <button :class="shownTab === 'token' ? 'on' : ''" @click="tab = 'token'">Klei 令牌
          <span class="tab-count">{{ cluster || '还没有房间' }}</span></button>
      </div>
      <x-wizard v-if="shownTab === 'wizard'" :schema="schema" :schema-error="schemaErr"
                :query="query" @tab="use"></x-wizard>
      <x-cmd v-else-if="shownTab === 'cmd'" :cluster="cluster"></x-cmd>
      <x-world v-else-if="shownTab === 'world'" :cluster="cluster" :shards="shards" :schema="schema"></x-world>
      <x-cluster v-else-if="shownTab === 'cluster'" :cluster="cluster"></x-cluster>
      <x-ports v-else-if="shownTab === 'ports'" :cluster="cluster"></x-ports>
      <x-token v-else :cluster="cluster"></x-token>
    </div>`,
};
