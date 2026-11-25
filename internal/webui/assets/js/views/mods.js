// 模组页：这台机器上的模组从哪来、这个房间开了哪些、每一项怎么配、两个世界是不是一致。
//
// 这一页必须把三件事分开摆（和 internal/api/mods.go 顶部那段注释同源），混在一起
// 用户就只能问"我明明装了怎么没生效"：
//   1. 文件在不在磁盘上（工坊下载 / 手工上传）；
//   2. 订阅了没有（下载清单里有没有这一行，服务端下次启动会不会去拉）；
//   3. 这个房间开没开（modoverrides.lua）。
// 这三件事以前是三个页签、三次点击，用户的抱怨是"这三步太繁琐"。现在它们是
// **三个视图**而不是三个步骤：找模组那一屏的「订阅并开启」一次把 2 和 3 都写完
// （POST /api/v1/mods/subscribe-and-open），另外两屏留着是为了核对与回退。
// 唯一还留在点击之后的事是重启：文件要服务端启动时才从工坊下载，面板不抢这一步
// （理由见 internal/mods/mods.go 顶部——工坊目录那套约定是引擎自己对好的）。
// "两个世界是否一致"原来占一个一级 tab，可它既不是动线的一步、也不会有人每天来看，
// 现在降级成开关那一屏底下那一句状态提示，要细节再点开（Consistency 的 compact 模式）。
//
// 配置项一律走"档位名"，界面不出现 `size = 2` 这种码值：字典来自
// GET /api/v1/mods/{dir}/schema，是面板从模组自己的 modinfo.lua 里读出来的。
// 读不懂的项会明确标成"按原值编辑"，不猜、也不藏。
import * as api from "../api.js";
import { state, act, actMany, askConfirm, roomWritable, readOnlyNote } from "../store.js";
import { bytes, num } from "../fmt.js";
import { rate, heat, heatNote } from "../modcard.js";

// openLabel 把"这个模组在眼前这个房间的开关状态"折成按钮上那一句。
// 三个视图共用它，否则会出现"检索页说没开、开关页里它已经在开着"这种自相矛盾。
// canOpen 为假时按钮走的是另一条路：没有世界就只能订阅；done 为真时不画按钮、只陈述。
// tone 给那句陈述挑颜色：'已订阅'只说明清单里有一行，和'已经开在这个房间'不是同一件事。
function openLabel(registered, worlds, opened) {
  if (!worlds.length) {
    return { text: registered ? "已订阅" : "订阅", canOpen: false, done: !!registered, tone: "info" };
  }
  if (opened.length >= worlds.length) {
    return { text: "已开在这个房间", canOpen: false, done: true, tone: "ok" };
  }
  if (!registered) return { text: "订阅并开启", canOpen: true, done: false, tone: "ok" };
  if (opened.length) {
    return { text: "补齐 " + (worlds.length - opened.length) + " 个世界", canOpen: true, done: false, tone: "ok" };
  }
  return { text: "在这个房间打开", canOpen: true, done: false, tone: "ok" };
}

// openedIn 从"开着它的世界"名单（接口逐个世界读 modoverrides.lua 得来的，写法 Cluster / Master）
// 里挑出眼前这一间房的那几个世界。
function openedIn(usedBy, cluster, worlds) {
  const list = usedBy || [];
  return worlds.filter((w) => list.indexOf(cluster + " / " + w) >= 0);
}

// subscribeAndOpen 是"一次点完"那一条请求，检索卡片和待下载那一栏都走它：
// 两处点的是同一件事，写成两份迟早会有一份的字段和后端对不上。
async function subscribeAndOpen(id, cluster, worlds, okTitle) {
  return act("mod.open:" + id, "POST", "/api/v1/mods/subscribe-and-open",
    { workshopId: String(id), cluster: cluster, shards: worlds },
    { okTitle: okTitle, okBody: (x) => (x && x.hint) || "", badTitle: "这一步没做成", clusters: false });
}

// ---- 分片启用与逐项配置 ----

const ShardMods = {
  props: {
    cluster: String,
    shards: { type: Array, default: () => [] },
    shard: String,
    mods: { type: Array, default: () => [] },      // GET /api/v1/mods 的 mods 数组
    // 还没下载的那几条（订阅清单里有、mods/ 里还没有目录）。开关这一屏也会碰到它们——
    // "订阅并开启"之后文件要等重启才下来——这时候名字和封面同样只能来自面板见过的工坊条目。
    // 不带进来的话这一行就只剩 workshop-数字，正是用户在下载那一屏批过的那件事。
    pendingByDir: { type: Object, default: () => ({}) },
  },
  emits: ["pick-shard"],
  setup() { return { state }; },
  data() {
    return {
      view: null, loading: false, error: "",
      // drawers[dir] = { loading, error, schema, vals, base, prio }
      drawers: {},
      // 这一屏不再放「全部 / 已开 / 没开」那三个按钮：列表本身就按开着和没开分成了两组，
      // 那三个按钮只是把已经在眼前的东西藏到点击后面。剩下的"有问题"才是要单独开关的一档。
      onlyProblem: false, q: "", err: "",
      // 多选：按目录名记，本屏（搜索/筛选之后）内批量操作。
      selected: {},
    };
  },
  computed: {
    // rows 是"这个分片可能碰到的所有模组"：覆盖文件里有条目的，加上磁盘上还没写条目的。
    // 只列覆盖文件里的条目会造成一个很实的坑——模组刚装好、还没开过，列表里根本没有它，
    // 用户找不到"打开"的按钮，只能回检索页再看一遍。
    rows() {
      const byDir = {};
      (this.view && this.view.mods ? this.view.mods : []).forEach((m) => {
        byDir[m.dir] = { dir: m.dir, inFile: true, enabled: !!m.enabled, present: !!m.present,
          priority: m.priority, options: m.options || {}, nested: m.nestedOptions || [], onDisk: !!m.onDisk };
      });
      (this.mods || []).forEach((m) => {
        if (!byDir[m.dir]) {
          byDir[m.dir] = { dir: m.dir, inFile: false, enabled: false, present: false,
            priority: null, options: {}, nested: [], onDisk: true };
        }
        byDir[m.dir].mod = m;
      });
      Object.keys(byDir).forEach((d) => {
        // 覆盖文件里有、磁盘上还没有（清单登记了等重启下载）：这时候名字只能靠目录名。
        if (!byDir[d].mod) byDir[d].mod = this.guessFromDir(d);
      });
      return Object.keys(byDir).sort().map((d) => byDir[d]);
    },
    shown() {
      const q = this.q.trim().toLowerCase();
      return this.rows.filter((r) => {
        if (this.onlyProblem && !this.rowProblem(r)) return false;
        if (!q) return true;
        // 名字要现取：rows 里那一项自己不带 name（名字在 r.mod 上），
        // 早先写的是 r.name，于是这个搜索框按名字搜永远搜不到，只能搜目录名。
        return this.name(r).toLowerCase().indexOf(q) >= 0 || r.dir.toLowerCase().indexOf(q) >= 0;
      });
    },
    // sections 是这一屏的分栏本身：「开着」和「没开」是这一页唯一会变的两个状态，
    // 混在一列里就得逐行读开关才知道这个世界装了什么——用户问的正是这句话。
    // 空的那一组整个不画（宁可少一栏，也不摆一个"这里什么都没有"的标题）。
    sections() {
      const on = this.shown.filter((r) => r.enabled);
      const off = this.shown.filter((r) => !r.enabled);
      const out = [];
      if (on.length) {
        out.push({ key: "on", dot: "dot-live", title: "开着", rows: on,
                   sub: "重启 " + (this.shard || "这个世界") + " 时它们会被加载" });
      }
      if (off.length) {
        out.push({ key: "off", dot: "dot-off", title: "没开", rows: off,
                   sub: "打开才会写进这个世界的 modoverrides.lua" });
      }
      return out;
    },
    problemCount() { return this.rows.filter((r) => this.rowProblem(r)).length; },
    selectedRows() { return this.shown.filter((r) => this.selected[r.dir]); },
    selectedCount() { return this.selectedRows.length; },
    bulkBusy() { return !!state.busy["mods.bulk"]; },
    // 同一条原因在好几行上一字不差地重复时，整屏只说一次（行上留那枚短徽标）。
    // 只在重复≥2 时收：只有一行时那句话属于那一行，挪到顶上反而对不上号。
    sharedNotes() {
      const by = {};
      this.rows.forEach((r) => {
        const t = r.mod && r.mod.modinfoError;
        if (!t) return;
        (by[t] = by[t] || []).push(this.name(r));
      });
      return Object.keys(by).filter((t) => by[t].length > 1)
        .map((t) => ({ text: t, names: by[t] }));
    },
    depsByDir() {
      const out = {};
      ((this.view && this.view.dependencyProblems) || []).forEach((p) => {
        out[p.dir] = out[p.dir] || [];
        out[p.dir].push(p);
      });
      return out;
    },
    // 这一间房的 modoverrides.lua 里读到的条目，按目录索引。
    // 磁盘上没有目录的那几行要靠它认名字（接口那侧的 seenName）。
    shardEntry() {
      const out = {};
      ((this.view && this.view.mods) || []).forEach((e) => { out[e.dir] = e; });
      return out;
    },
    enabledCount() { return this.rows.filter((r) => r.enabled).length; },
    canWrite() { return roomWritable(this.cluster); },
    writeNote() { return readOnlyNote(this.cluster); },
  },
  watch: {
    cluster: { immediate: true, handler() { this.load(); } },
    shard() { this.load(); },
  },
  methods: {
    name(r) { return (r.mod && r.mod.name) || r.dir; },
    guessFromDir(dir) {
      // 覆盖文件里有条目但磁盘上还没下载。这时候能认出来的东西只有一个来源：
      // 面板见过的工坊条目（接口的 pendingDownload 那一堆），因为文件还没在这台机器上。
      // 不带它的话这一行就只剩 workshop-数字，而用户在下那一屏批过的正是"放个编号谁来核对"。
      const m = /^workshop-(\d+)$/.exec(dir);
      const id = m ? m[1] : "";
      const seen = id ? this.pendingByDir[dir] : null;
      // 这里绝不能带 modinfoOk:false——那会让这张卡挂上"modinfo 读不懂"，
      // 而实情只是文件还没下载（这一屏上面本来就有一枚说这件事的徽标）。
      return { dir, name: this.rowName(dir, seen), workshop: !!m, workshopId: id,
        previewUrl: seen && seen.previewUrl, modinfoOk: true, inSetupList: !!seen,
        // pendingView 里的 clientRequired 就是"玩家自己也得订阅"，与磁盘那批同口径。
        allClientsRequire: !!(seen && seen.clientRequired) };
    },
    // 这一行的名字。三种来路，按可信度排：待下载条目的工坊标题 → 这一间接口带回来的
    // 缓存标题（清单里也没有的那种残留条目走这一条）→ 空（界面回落到目录名）。
    // 空的时候绝不编一个：那时候面板是真的没见过这个 ID。
    rowName(dir, seen) {
      if (seen && seen.nameKnown) return seen.name;
      const e = this.shardEntry[dir];
      return (e && e.seenName) || "";
    },
    rowProblem(r) {
      if (this.depsByDir[r.dir]) return true;
      // 只在"开着"的时候才算问题：这一屏本来就分了两栏，关着的那一行磁盘上没目录
      // 不是故障（面板关一个模组是留条目写 enabled = false，不会把目录删掉）。
      if (r.inFile && r.enabled && !r.onDisk) return true;
      return !!(r.mod && r.mod.modinfoOk === false);
    },
    // 这一行到底配了什么。以前这一屏看不出区别——想知道"这个模组配过几项"
    // 只能点配置，九行就是九次点开再收起。这些数全来自已经读到的 modoverrides 条目，
    // 不是猜的：条目里没有配置项，才会说"没写配置项"。
    // 措辞刻意短：这一句在每一行都会出现，写成整句的话（早期版本是）
    // 一屏九行就有九行小作文，而它们说的其实是同一件事。
    cfgSummary(r) {
      if (!r.inFile) return "没配过，打开时全按模组默认";
      const n = Object.keys(r.options || {}).length;
      const bits = [n ? "配了 " + n + " 项" : "有条目，但没写配置项"];
      if (r.nested && r.nested.length) bits.push(r.nested.length + " 项嵌套面板不动");
      if (r.priority !== null && r.priority !== undefined) bits.push("顺序 " + r.priority);
      // "值还留着"只在这一栏需要：上面那行开关关掉时条目不会被删，这是这一屏最容易让人
      // 以为"白配了"的一件事。开着的那一栏不需要，所以也不重复。
      if (!r.enabled) bits.push("值还留着");
      return bits.join(" · ");
    },
    // 整屏已经说过一次的原因，行上就不再重复第二遍（见 sharedNotes）。
    rowNote(r) {
      // 目录不见了的这条要写成整行，不能只挂在徽标的 title 上：手机上没有 hover，
      // title 里那句"这一行其实已经开不起来了"一辈子也看不见。
      // 带 enabled 是因为关着的那一行本来就没在开什么，这句会变成假话
      //（取消订阅之后留下的就是那种条目）。
      if (r.inFile && r.enabled && !r.onDisk && !(r.mod && r.mod.inSetupList)) {
        return "这个世界开着它，但这个世界没有它的文件（mods/ 和引擎自己的下载区里都没有），订阅清单里也没有这一行：重启不会把它拉回来。" +
          "要留它就回「找模组」那一屏重新订阅，不留就在这行把它关掉。";
      }
      const t = r.mod && r.mod.modinfoError;
      if (!t) return "";
      return this.rows.some((x) => x !== r && x.mod && x.mod.modinfoError === t) ? "" : t;
    },
    async load() {
      if (!this.cluster || !this.shard) return;
      this.loading = true;
      this.error = "";
      const res = await api.get("/api/v1/clusters/" + encodeURIComponent(this.cluster) +
        "/shards/" + encodeURIComponent(this.shard) + "/mods");
      // 迟到响应不能写进已经切走的分片：切房间时这里挂的还是上一个分片的 view。
      if (this.cluster !== this.$props.cluster || this.shard !== this.$props.shard) return;
      this.loading = false;
      if (!res.ok) { this.error = res.error || ("接口返回 " + res.status); this.view = null; return; }
      this.view = res.data;
      this.resetDrawers();
    },
    resetDrawers() { this.drawers = {}; },
    reload() { this.load(); },
    openDir(dir) { return !!this.drawers[dir]; },
    toggleDir(dir) {
      if (this.drawers[dir]) { delete this.drawers[dir]; return; }
      this.ensureDrawer(dir);
    },
    // 字典按目录缓存在本页：同一个模组在地表看过一次，切到洞穴不用再打接口。
    ensureDrawer(dir) {
      if (!this.drawers[dir]) {
        this.drawers[dir] = { loading: true, error: "", schema: null, vals: {}, base: {}, prio: "", prioBase: "" };
        api.get("/api/v1/mods/" + encodeURIComponent(dir) + "/schema").then((res) => {
          const d = this.drawers[dir];
          if (!d) return;
          d.loading = false;
          if (!res.ok) {
            d.error = res.error || ("接口返回 " + res.status);
            // 字典没拿到也要把文件里已有的键铺出来：这一屏下面那句"按文件里的原值显示"
            // 是给了用户的承诺，键都不显示等于让人对着一个空表单承诺"照样能写回去"。
            this.seed(dir);
            return;
          }
          d.schema = res.data;
          this.seed(dir);
        });
      }
      return this.drawers[dir];
    },
    // 一个配置项底下只留一行说明：模组的悬停文案 + "模组默认 X" 合成一句。
    // 以前这里是最多三行小字，表单看着比模组自己的设置界面还长，
    // 而其中"模组默认"和当前值往往说的是同一件事。
    hintOf(dir, name) {
      const o = this.optOf(dir, name);
      if (!o) return "这项在模组的 modinfo.lua 里读不出字典，按原值编辑。";
      const bits = [];
      if (o.hover) bits.push(o.hover);
      let d = o.defaultLabel;
      if (!d && o.default !== undefined && o.default !== null) d = String(o.default);
      if (d) bits.push("模组默认 " + d);
      return bits.join(" · ");
    },
    row(dir) { return this.rows.find((r) => r.dir === dir) || null; },
    // seed 把"当前覆盖文件里的值"铺进表单。字典里没有的键也一起铺：
    // 提交时整份发回去，漏一个键就等于面板替用户删了那一键。
    seed(dir) {
      const d = this.drawers[dir];
      const r = this.row(dir);
      if (!d || !r) return;
      const vals = {};
      const put = (k, v) => { vals[k] = v === undefined ? null : v; };
      ((d.schema && d.schema.options) || []).forEach((o) => put(o.name, r.options[o.name]));
      Object.keys(r.options).forEach((k) => put(k, r.options[k]));
      d.vals = vals;
      d.base = JSON.parse(JSON.stringify(vals));
      d.prio = r.priority === null || r.priority === undefined ? "" : String(r.priority);
      d.prioBase = d.prio;
    },
    optOf(dir, name) {
      const d = this.drawers[dir];
      if (!d || !d.schema) return null;
      return (d.schema.options || []).find((o) => o.name === name) || null;
    },
    // 下拉里必须有文件里那个原值这一格，哪怕字典没有它：
    // 否则用户一展开下拉就看到空选中，动一下把原值丢了——那等于面板替他改了配置。
    choicesOf(dir, name) {
      const o = this.optOf(dir, name);
      const cur = this.drawers[dir] ? this.drawers[dir].vals[name] : null;
      const list = ((o && o.choices) || []).slice();
      if (cur !== null && cur !== undefined && !list.some((c) => c.value === cur)) {
        list.unshift({ label: String(cur) + "（模组字典里没有这一档，文件里的原值）", value: cur });
      }
      return list;
    },
    dirty(dir) {
      const d = this.drawers[dir];
      if (!d) return 0;
      let n = 0;
      Object.keys(d.vals).forEach((k) => { if (!this.same(d.vals[k], d.base[k])) n++; });
      if (d.prio !== d.prioBase) n++;
      return n;
    },
    // anyDirty 给页面在切走之前问一句：这一屏的表单是本组件的内存，切 tab 就把组件卸了，
    // 用户填了一半的配置会没声没息地丢掉。动线改成"1→2→3 来回点"之后，
    // 这一问从可有可无变成了必须。
    anyDirty() {
      return Object.keys(this.drawers).reduce((n, dir) => n + this.dirty(dir), 0);
    },
    same(a, b) {
      const na = a === undefined ? null : a, nb = b === undefined ? null : b;
      if (na === null || nb === null) return na === nb;
      return String(na) === String(nb);
    },
    restore(dir) {
      const d = this.drawers[dir];
      if (!d) return;
      Object.keys(d.vals).forEach((k) => { d.vals[k] = d.base[k] === undefined ? null : d.base[k]; });
      d.prio = d.prioBase;
    },
    // 文本框里的东西要变回它本来的类型：Lua 里 8 和 "8" 是两回事，
    // 而 JSON 提交回来的值会原样写进 modoverrides.lua。
    typed(dir, name, raw) {
      const o = this.optOf(dir, name);
      const kind = o ? o.kind : "raw";
      if (raw === "") return null;
      if (kind === "number") {
        const n = Number(raw);
        return Number.isFinite(n) ? n : raw;
      }
      if (kind === "toggle") {
        if (raw === true || raw === "true") return true;
        if (raw === false || raw === "false") return false;
        return raw;
      }
      // 字典认不出的项：文件里原来是数字就继续当数字写，别把它改成字符串。
      if (kind === "unknown" || kind === "raw") {
        const was = this.drawers[dir] ? this.drawers[dir].base[name] : null;
        const n = Number(raw);
        if (typeof was === "number" && !Number.isNaN(n) && String(n) === String(raw).trim()) return n;
        return raw;
      }
      return raw;
    },
    setVal(dir, name, raw) {
      const d = this.drawers[dir];
      if (!d) return;
      d.vals[name] = this.typed(dir, name, raw);
    },
    // 下拉框专用。"不写"那一项的 value 是字符串 "null"，永远不是空串：
    // 拿到空串只可能是 DOM 里还没有对应 option（列表刚被 choicesOf 改过、渲染和取值抢了先后），
    // 这时候必须什么都不改。把空串当成"不写"会静默删掉用户原本的那一值——
    // 界面显示的是"跟随模组默认"，文件里却是另一个数，没人能从屏幕上看出这件事。
    setSelectVal(dir, name, raw) {
      if (raw === "") return;
      this.setVal(dir, name, this.jqval(raw));
    },
    async save(dir) {
      this.err = "";
      const d = this.drawers[dir];
      if (!d) return;
      const options = {};
      let bad = "";
      Object.keys(d.vals).forEach((k) => {
        const v = d.vals[k];
        if (typeof v === "number" && !Number.isFinite(v)) { bad = k; return; }
        if (typeof v === "object") { bad = k; return; }
        options[k] = v === undefined ? null : v;
      });
      if (bad) {
        this.err = "「" + bad + "」这一项现在装不下要写的东西：配置项只能是文本、数字或是否，" +
          "不能再套一层。这一项请去模组自己的说明里核对写法。";
        return;
      }
      const body = { options };
      if (d.prio !== d.prioBase) {
        const n = Number(d.prio);
        if (d.prio.trim() !== "" && Number.isFinite(n)) body.priority = n;
        else if (d.prio.trim() !== "") { this.err = "priority 要填一个数字，或者干脆别动这一栏。"; return; }
      }
      const res = await act("mods:save:" + this.cluster + "/" + this.shard + "/" + dir,
        "PUT", shardModsPath(this.cluster, this.shard, dir), body,
        { okTitle: "配置已写入", okBody: (x) => (x && x.hint) || "", badTitle: "写入没成",
          // 写 modoverrides.lua 不会改变分片状态：不用为此重拉整个集群快照，
          // 接口回的那份 view 已经足够把这一行更新掉。
          clusters: false });
      if (res.ok && res.data) {
        this.view = res.data;
        this.seedFresh(dir);
      }
    },
    // 写完要把"当前值"当成新的基线：否则刚保存的改动还会一直显示"已改"。
    seedFresh(dir) {
      const d = this.drawers[dir];
      if (!d) return;
      const r = this.row(dir);
      if (!r) return;
      Object.keys(d.vals).forEach((k) => {
        const v = r.options[k];
        if (v !== undefined) d.vals[k] = v;
      });
      d.base = JSON.parse(JSON.stringify(d.vals));
      d.prioBase = d.prio;
    },
    async setEnabled(r, next) {
      this.err = "";
      const res = await act("mods:enable:" + this.cluster + "/" + this.shard + "/" + r.dir,
        "PUT", shardModsPath(this.cluster, this.shard, r.dir), { enabled: next },
        { okTitle: next ? "已在这个世界打开" : "已在这个世界关掉",
          okBody: (x) => (x && x.hint) || "", badTitle: "开关没写成", clusters: false,
          // 开关可反向：关掉的能再打开、打开的能再关掉，给一条短时撤销。
          undo: {
            label: "撤销「" + (next ? "打开" : "关掉") + "」" + (this.name(r) || r.dir),
            method: "PUT", path: shardModsPath(this.cluster, this.shard, r.dir),
            body: { enabled: !next }, seconds: 12,
          },
        });
      if (res.ok && res.data) {
        this.view = res.data;
        if (this.drawers[r.dir]) this.seedFresh(r.dir);
      }
    },
    isBusy(dir) {
      const k = this.cluster + "/" + this.shard + "/" + dir;
      return !!(state.busy["mods:save:" + k] || state.busy["mods:enable:" + k]);
    },
    toggleSel(dir) { this.selected = { ...this.selected, [dir]: !this.selected[dir] }; },
    selectAllShown() {
      const all = {};
      this.shown.forEach((r) => { all[r.dir] = true; });
      this.selected = all;
    },
    // 批量开/关：整批一个 busy 键、串行下发、结果一条汇总；完成后重读这一屏。
    async bulkSet(next) {
      const list = this.selectedRows;
      if (!list.length) return;
      const items = list.map((r) => ({
        method: "PUT", path: shardModsPath(this.cluster, this.shard, r.dir),
        body: { enabled: next }, label: this.name(r) || r.dir,
      }));
      const res = await actMany("mods.bulk", items,
        { okTitle: next ? "批量开启完成" : "批量关闭完成", badTitle: "批量开关有没成的", clusters: false, after: false });
      if (res.ok) this.selected = {};
      await this.reload();
    },
    // 批量取消订阅：只移下载清单，不删本地文件；不可逆，确认框写清后果、不伪造撤销。
    async bulkUnregister() {
      const list = this.selectedRows.filter((r) => r.mod && r.mod.workshopId && r.mod.inSetupList);
      const skipped = this.selectedCount - list.length;
      if (!list.length) {
        this.err = "选中的这些模组都不在下载清单里（本地上传或没有工坊 ID），没有可以取消的订阅。";
        return;
      }
      const ok = await askConfirm({
        title: "取消选中 " + list.length + " 个模组的订阅？",
        body: "只改下载清单，服务端下次启动不会再从工坊拉它们。工坊 ID：" +
          list.map((r) => r.mod.workshopId).join("、") + "。",
        warn: "不删本地文件，也不改这个世界的开关；但订阅移掉后想再装回来要重新订阅。" +
          (skipped ? " 另有 " + skipped + " 项没有工坊订阅，会跳过。" : ""),
        danger: true, okLabel: "取消订阅",
      });
      if (!ok) return;
      const items = list.map((r) => ({
        method: "DELETE", path: "/api/v1/mods/setup/" + encodeURIComponent(r.mod.workshopId),
        label: this.name(r) || r.dir,
      }));
      const res = await actMany("mods.bulk", items,
        { okTitle: "批量取消订阅完成", badTitle: "批量取消订阅有没成的", clusters: false, after: false });
      if (res.ok) this.selected = {};
      await this.reload();
    },
    // 下拉框里的值带类型（8 / "a" / true / 不写=null），而 DOM 的 option.value 只能是字符串。
    // 不编码的话 Vue 认不出哪个 option 该选中，一渲染就变成空白。
    jval(v) { return JSON.stringify(v === undefined ? null : v); },
    jqval(raw) { try { return JSON.parse(raw); } catch (e) { return raw; } },
  },
  template: `
    <section class="card">
      <div class="card-head">
        <span class="ico-tile tint-gold"><x-icon name="cube" :size="15"></x-icon></span>
        <h3>这个世界开了哪些模组</h3>
        <span class="chip" v-if="view">{{ enabledCount }} / {{ rows.length }} 开着</span>
        <span class="chip" v-else-if="loading">读取中</span>
        <span class="chip" v-else>没拿到</span>
        <!-- 依赖对不上是这一页最需要看见的一句话，但它下面每一行还会各自重复一遍原因，
             所以标题行只留一枚能点开的短标签，整句收进气泡。 -->
        <x-hint v-if="view && view.hint" tone="warn"
                :label="'依赖对不上 ' + view.dependencyProblems.length" :text="view.hint"></x-hint>
        <span class="grow"></span>
        <button class="linkish" @click="reload" :disabled="loading">重读</button>
      </div>

      <div class="seg mb" v-if="shards.length">
        <button v-for="s in shards" :key="s.name" :class="s.name === shard ? 'on' : ''"
                @click="$emit('pick-shard', s.name)">{{ s.name }}</button>
      </div>

      <!-- 为什么这一句这么短：页顶那条已经把"没有房间"连同原因和去处说过一遍了
           （见页面模板里 !state.clusters.length 那条），一个事实不该在屏上说三遍。
           "有房间但没选"这一档在界面上到不了——房间一非空 store 就自动选中一个
           （store.js 的 applyClusters），所以这里只需要说没有房间这一种。 -->
      <x-note v-if="!cluster" kind="info">还没有房间，也就还没有哪个世界的开关可看。</x-note>
      <x-note v-else-if="!shards.length" kind="warn">这个房间里还没有世界。开服向导建完世界再回来。</x-note>

      <x-sk v-else-if="loading" :lines="4"></x-sk>
      <div class="err-box" v-else-if="error">{{ error }}</div>

      <template v-else-if="view">
        <div class="row" style="gap:10px;flex-wrap:wrap;align-items:center">
          <div class="field" style="flex:1;min-width:150px">
            <input type="search" v-model="q" placeholder="找模组：名字或目录名" aria-label="找模组">
          </div>
          <button class="chipbtn" v-if="problemCount" :class="onlyProblem ? 'on' : ''"
                  @click="onlyProblem = !onlyProblem">只看有问题 {{ problemCount }}</button>
        </div>

        <!-- 多选批量：勾中的行一直显示在「已选 N 项」，整批期间按钮禁用，结果一条汇总。 -->
        <div class="row mt" style="gap:8px;align-items:center;flex-wrap:wrap" v-if="shown.length">
          <button class="btn sm ghost" @click="selectAllShown">全选本屏</button>
          <span class="chip" v-if="selectedCount">已选 {{ selectedCount }} 项</span>
          <span class="grow"></span>
          <!-- bulkBusy 是 computed，模板里按值读。写成 bulkBusy() 在没勾行时被前面的
               短路挡着不发作，一勾就抛 "not a function"；静态体检第十一条抓的就是它。 -->
          <button class="btn sm" :disabled="!canWrite || !selectedCount || bulkBusy" :title="canWrite ? '' : writeNote" @click="bulkSet(true)">批量开启</button>
          <button class="btn sm" :disabled="!canWrite || !selectedCount || bulkBusy" :title="canWrite ? '' : writeNote" @click="bulkSet(false)">批量关闭</button>
          <button class="btn sm danger" :disabled="!canWrite || !selectedCount || bulkBusy" :title="canWrite ? '' : writeNote" @click="bulkUnregister">批量取消订阅</button>
          <button class="btn sm ghost" v-if="selectedCount" @click="selected = {}">清空选择</button>
        </div>

        <!-- 同一条原因在几行上一字不差时，整屏只在这里说一次，行上留那枚短徽标。 -->
        <x-note v-for="(t, i) in sharedNotes" :key="'sn' + i" kind="warn">
          这一屏有 {{ t.names.length }} 行是同一种情况（{{ t.names.join('、') }}）：{{ t.text }}
        </x-note>

        <x-empty v-if="!shown.length" icon="cube" title="这里没有对得上的模组"
          :note="q || onlyProblem ? '换个搜法，或者把「只看有问题」关掉。「这台机器上的文件」那一屏能确认磁盘上到底装了什么。' :
                 '这个世界一个模组都没开。到「找模组」那一屏点「订阅并开启」，清单和这个房间的开关会一次写完。'">
        </x-empty>


        <div class="mod-lines mt" v-if="shown.length">
          <!-- 「开着 / 没开」分成两栏：这一屏唯一会变的就是这个状态，混在一列里就得
               逐行读开关才知道这个世界到底装了什么。空的那一栏整个不画。
               原来的「全部/已开/没开」三个按钮因此删掉——它们只是把眼前的东西藏到点击后面。 -->
          <template v-for="sec in sections" :key="sec.key">
            <div class="mod-group-head">
              <span :class="sec.dot"></span>
              <b>{{ sec.title }}</b>
              <span class="g-count">{{ sec.rows.length }}</span>
              <span class="muted">{{ sec.sub }}</span>
            </div>
            <!-- 封面在这一屏只用来认脸，不参与任何判断，所以比第 2 步那档窄；
                 开没开由分栏标题说，行上不再叠一枚说同一件事的角标。 -->
            <article class="mod-line sm" :class="sec.key === 'on' ? 'is-on' : ''"
                     v-for="r in sec.rows" :key="r.dir">
              <x-mod-cover :it="r.mod"></x-mod-cover>
              <div class="mod-line-main">
                <div class="mod-line-head">
                  <input type="checkbox" style="flex:none" :checked="!!selected[r.dir]"
                         :aria-label="'选中 ' + name(r)" @change="toggleSel(r.dir)">
                  <span class="mod-line-name">{{ name(r) }}</span>
                  <!-- 同样是"这个世界有条目、磁盘上却没目录"，来路有两种，说法必须两种：
                       订阅清单里有它的，重启真的会拉下来；清单里也没有的（目录被删了，或这行是残留），
                       重启根本不会管它——那种还写"文件还没下载"就是给了一句假承诺。
                       两枚都只给"开着"的那一行：面板关模组是留条目写 enabled = false，
                       关着的那一行没目录不是问题，标红只会让人去找一个不存在的故障。 -->
                  <x-badge v-if="r.inFile && r.enabled && !r.onDisk && r.mod && r.mod.inSetupList" kind="warn"
                           text="文件还没下载"
                           title="订阅清单里有它，这个世界还没拉到这一份（引擎按世界各存一份）：这个世界重启时才会去工坊下载"></x-badge>
                  <x-badge v-else-if="r.inFile && r.enabled && !r.onDisk" kind="bad" text="目录不见了"
                           title="这个世界开着它，但它的文件在这台机器上找不到（mods/ 和引擎的下载区里都没有），订阅清单里也没有这一行：重启不会把它拉回来。要么回「找模组」那一屏重新订阅，要么在这一行把它关掉。"></x-badge>
                  <x-badge v-if="r.mod && r.mod.allClientsRequire" kind="warn" text="玩家也要订阅"
                           title="没在工坊订阅它的玩家会被服务端直接断线"></x-badge>
                  <x-badge v-else-if="r.mod && r.mod.clientOnly" kind="info" text="只影响客户端"
                           title="服务端开不开它都不影响玩家，效果只出现在客户端"></x-badge>
                  <x-badge v-if="r.mod && !r.mod.modinfoOk" kind="bad" text="modinfo 读不懂"
                           :title="r.mod.modinfoError"></x-badge>
                  <x-badge v-else-if="r.mod && r.mod.modinfoError" kind="warn" text="模组没写出名字"
                           :title="r.mod.modinfoError"></x-badge>
                  <span class="grow"></span>
                  <label class="switch" :title="canWrite ? (r.inFile ? '改这个世界的启用状态（配置项原样留着）' : '会在 modoverrides.lua 里新建条目') : writeNote">
                    <input type="checkbox" :checked="r.enabled" :disabled="!canWrite || isBusy(r.dir)"
                           @change="setEnabled(r, $event.target.checked)">
                    <span class="track"></span>
                  </label>
                  <button class="btn sm" @click="toggleDir(r.dir)">{{ openDir(r.dir) ? '收起配置' : '配置' }}
                    <span class="tab-count" v-if="dirty(r.dir)">已改 {{ dirty(r.dir) }}</span>
                  </button>
                </div>
                <!-- 这一行只补一句"配到什么程度"：想知道具体哪几项得点开配置，
                     但一屏九行都长一个样时，这一句足够决定要不要点开。 -->
                <div class="mod-line-sum">{{ cfgSummary(r) }}</div>
                <!-- 原因写成整行文字，不做成 chip 把正文塞进 title：手机上没有 hover，
                     title 里的话一辈子也看不见。整句由后端写（mods.go 那三种情况各有说法），
                     这里不许再套一层"读不懂"前缀：文件读得通、只是名字被作者写成表达式，
                     那种被冠上"读不懂"就是假警报。重复出现的那种已收到屏顶那一句里。 -->
                <div class="shard-note" v-if="rowNote(r)">{{ rowNote(r) }}</div>
                <div class="shard-note bad" v-for="(p, i) in (depsByDir[r.dir] || [])" :key="i">
                  {{ p.status }} —— {{ p.hint }}
                </div>
                <div class="shard-note" v-if="r.mod && r.mod.dependencies && r.mod.dependencies.length">
                  它声明的依赖：
                  <span class="chip mono" v-for="(g, i) in r.mod.dependencies" :key="i">{{ g.join(' 或 ') }}</span>
                </div>
              </div>

              <div class="drawer" v-if="openDir(r.dir)">
                <x-sk v-if="drawers[r.dir].loading" :lines="2"></x-sk>
                <template v-else>
                  <div class="row spread drawer-head">
                    <span class="drawer-title">配置 · {{ name(r) }}</span>
                    <span class="muted">写进 {{ cluster }} / {{ shard }}</span>
                  </div>
                  <!-- 来路这一排从行上挪进抽屉：九行都挂着同样四个 chip 时它是噪声，
                       而真要动手改配置时，"改的是这台机器上的哪一份"必须先对得上。 -->
                  <div class="row mod-meta">
                    <span class="chip mono">{{ r.dir }}</span>
                    <span class="chip" v-if="r.mod && r.mod.workshopId">工坊 {{ r.mod.workshopId }}</span>
                    <span class="chip" v-else-if="r.mod && !r.mod.workshop">本地上传</span>
                    <span class="chip" v-if="r.mod && r.mod.version">这台机器上是 {{ r.mod.version }}</span>
                    <span class="chip" v-if="r.mod && r.mod.inSetupList">在下载清单里</span>
                    <span class="chip" v-else-if="r.mod && r.mod.workshop">不在清单：重启不会更新它</span>
                  </div>
                  <x-note v-if="drawers[r.dir].error" kind="warn">
                    配置项字典没拿到：{{ drawers[r.dir].error }}
                    下面这一屏按文件里的原值显示，改完照样能写回去。
                  </x-note>
                  <x-note v-else-if="drawers[r.dir].schema && drawers[r.dir].schema.note" kind="info">
                    {{ drawers[r.dir].schema.note }}
                  </x-note>

                  <div class="grid-form mt">
                    <div class="field" v-for="(val, k) in drawers[r.dir].vals" :key="k">
                      <label>{{ (optOf(r.dir, k) && optOf(r.dir, k).label) || k }}
                        <span class="muted mono" v-if="!optOf(r.dir, k)">（{{ k }}）</span>
                      </label>

                      <select v-if="optOf(r.dir, k) && optOf(r.dir, k).kind === 'select'"
                              :class="!same(val, drawers[r.dir].base[k]) ? 'changed' : ''"
                              :value="jval(val)" @change="setSelectVal(r.dir, k, $event.target.value)">
                        <option :value="jval(null)">跟随模组默认（这一项不写）</option>
                        <option v-for="(c, ci) in choicesOf(r.dir, k)" :key="k + ':' + ci" :value="jval(c.value)">
                          {{ c.label }}
                        </option>
                      </select>

                      <select v-else-if="optOf(r.dir, k) && optOf(r.dir, k).kind === 'toggle'"
                              :class="!same(val, drawers[r.dir].base[k]) ? 'changed' : ''"
                              :value="jval(val)" @change="setSelectVal(r.dir, k, $event.target.value)">
                        <option :value="jval(null)">跟随模组默认（这一项不写）</option>
                        <option :value="jval(true)">开</option>
                        <option :value="jval(false)">关</option>
                      </select>

                      <input v-else type="text" inputmode="decimal"
                             :class="!same(val, drawers[r.dir].base[k]) ? 'changed' : ''"
                             :value="val === null || val === undefined ? '' : String(val)"
                             @input="setVal(r.dir, k, $event.target.value)">

                      <!-- 说明和"模组默认"合成一行：以前一项底下三行小字，
                           表单看着比模组自己的设置界面还长。 -->
                      <span class="hint" v-if="hintOf(r.dir, k)">{{ hintOf(r.dir, k) }}</span>
                      <span class="hint ok" v-if="!same(val, drawers[r.dir].base[k])">
                        <template v-if="val === null">将改成「不写」，交回模组默认值</template>
                        <template v-else>已改，原来文件里是 {{ drawers[r.dir].base[k] === null || drawers[r.dir].base[k] === undefined ? '没写' : String(drawers[r.dir].base[k]) }}</template>
                      </span>
                    </div>

                    <div class="field">
                      <label>priority（加载顺序，可留空不改）</label>
                      <input type="number" step="1" :class="drawers[r.dir].prio !== drawers[r.dir].prioBase ? 'changed' : ''"
                             v-model="drawers[r.dir].prio">
                      <span class="hint">这个版本的引擎排序只看模组自己写的 priority，手工改它多半没有效果。</span>
                    </div>
                  </div>

                  <div class="hint mt" v-if="r.nested && r.nested.length">
                    这几项是嵌套结构，面板不读也不改，写回时会原样留着：{{ r.nested.join('、') }}
                  </div>
                  <div class="hint mt" v-if="drawers[r.dir].schema && drawers[r.dir].schema.skipped && drawers[r.dir].schema.skipped.length">
                    字典里认不出的项：{{ drawers[r.dir].schema.skipped.join('；') }}
                  </div>

                  <div class="row spread mt" style="gap:8px;flex-wrap:wrap">
                    <button class="btn ghost" :disabled="!dirty(r.dir)" @click="restore(r.dir)">还原这次改的</button>
                    <span class="grow"></span>
                    <button class="btn primary" :disabled="!canWrite || !dirty(r.dir) || isBusy(r.dir)" :title="canWrite ? '' : writeNote" @click="save(r.dir)">
                      {{ isBusy(r.dir) ? '写入中……' : '写入 ' + shard }}
                    </button>
                  </div>
                </template>
              </div>
            </article>
          </template>
        </div>

        <div class="err-box mt" v-if="err">{{ err }}</div>
        <x-note kind="info">
          改完开关或配置都要<b>重启这个世界</b>才会重新加载模组——专用服务端每次启动会先把所有模组关掉。
          总览页上有重启按钮；它会让所有在线玩家回到大厅。
        </x-note>
      </template>
    </section>`,
};

function shardModsPath(cluster, shard, dir) {
  return "/api/v1/clusters/" + encodeURIComponent(cluster) + "/shards/" + encodeURIComponent(shard) +
    "/mods/" + encodeURIComponent(dir);
}

// ---- 文件与下载 ----

const DiskMods = {
  props: {
    view: Object,
    loading: Boolean,
    error: String,
    // 待下载那一栏也要能一次点完（订阅已经写了，缺的是开关），所以这一屏同样要知道
    // 眼前是哪间房、有几个世界、哪些世界已经开着它。
    cluster: String,
    worlds: { type: Array, default: () => [] },
    openByDir: { type: Object, default: () => ({}) },
  },
  emits: ["reload"],
  setup() { return { state }; },
  data() { return { name: "", file: null, drag: false, err: "" }; },
  computed: {
    busy() { return !!state.busy["mod.upload"]; },
    // 选文件的两条路（点着选 / 拖进来）最后都只写 this.file 一个来源，
    // 所以下面这一句就是"这一次到底会不会真上传"的唯一判据。
    fileName() { return this.file ? this.file.name : ""; },
    fileSize() { return this.file ? bytes(this.file.size) : ""; },
    mods() { return (this.view && this.view.mods) || []; },
    setupProblems() { return (this.view && this.view.setupProblems) || []; },
    pending() { return (this.view && this.view.pendingDownload) || []; },
    localOnly() { return (this.view && this.view.localOnly) || []; },
    unmet() { return (this.view && this.view.unmetDependencies) || []; },
    // 启用列表读不动的那几间。"哪个世界开着这个模组"这一栏的可信度全靠它：
    // 读不到不能当"没人开着"，所以这一句必须在页顶出现一次，而不是每张卡上重复一遍。
    overrideProblems() { return (this.view && this.view.overrideReadProblems) || []; },
    // 同一条原因在几行上一字不差时收成整屏一句（九行里两行挂同一句长说明，
    // 读第二遍的人只会以为是自己看串了）。只有一行时不收成：那句话属于那一行。
    sharedNotes() {
      const by = {};
      this.mods.forEach((m) => {
        const t = m.modinfoError;
        if (!t) return;
        (by[t] = by[t] || []).push(m.name || m.dir);
      });
      return Object.keys(by).filter((t) => by[t].length > 1)
        .map((t) => ({ text: t, names: by[t] }));
    },
    // 落地目录名不再让用户填：默认就是 zip 文件名去掉后缀，和后端 nameFromUpload 同一套算法。
    // 只有这个默认值当不了目录名时（中文文件名、以点开头）才把输入框亮出来，
    // 那种情况下用户必须知道为什么要多填一格。
    dirPreview() { return this.file ? this.derivedDir(this.file.name) : ""; },
    dirBad() { return !!this.dirPreview && !/^[A-Za-z0-9_.-]+$/.test(this.dirPreview) ||
      (this.dirPreview || "").startsWith("."); },
    canWrite() { return roomWritable(this.cluster); },
    writeNote() { return readOnlyNote(this.cluster); },
  },
  methods: {
    derivedDir(filename) {
      const base = String(filename || "").split(/[\\/]/).pop() || "";
      return base.replace(/\.zip$/i, "");
    },
    kbusy(k) { return !!state.busy[k]; },
    wsUrl(id) {
      return "https://steamcommunity.com/sharedfiles/filedetails/?id=" + encodeURIComponent(id);
    },
    pick(ev) {
      this.file = (ev.target.files && ev.target.files[0]) || null;
    },
    choose() { this.$refs.file.click(); },
    // 拖进来的文件不往 input.files 里塞（那要现造一个 DataTransfer，Safari 上还不一定成），
    // 上传只看 this.file，所以两条路在这里合成一条。
    drop(ev) {
      this.drag = false;
      const f = ev.dataTransfer && ev.dataTransfer.files && ev.dataTransfer.files[0];
      if (!f) return;
      this.file = f;
      this.err = "";
    },
    async upload() {
      this.err = "";
      const input = this.$refs.file;
      if (!this.file) {
        this.err = "先选一个 zip 文件。";
        return;
      }
      const fd = new FormData();
      if (this.name.trim()) fd.append("name", this.name.trim());
      fd.append("mod", this.file, this.file.name);
      const res = await act("mod.upload", "POST", "/api/v1/mods/upload", fd,
        { okTitle: "模组包已落地", okBody: (x) => (x && x.hint) || "", badTitle: "上传没成", clusters: false });
      if (res.ok) {
        this.name = "";
        this.file = null;
        if (input) input.value = "";
        this.$emit("reload");
      }
    },
    async unregisterId(id, label) {
      const ok = await askConfirm({
        title: (label ? "取消订阅 " + label : "取消订阅 " + id) + "？",
        body: "只改下载清单（dedicated_server_mods_setup.lua）：服务端下次启动不会再去工坊拉它。",
        warn: "已经下载好的那份文件不会被删掉（mods/ 里的目录、引擎自己下载区里的每一份都留着），服务端也照样加载它。" +
          "要连文件一起清掉，用「已下载」那一栏里的删除。",
        okLabel: "取消订阅",
      });
      if (!ok) return;
      const res = await act("mod.setup.remove:" + id, "DELETE",
        "/api/v1/mods/setup/" + encodeURIComponent(id), undefined,
        { okTitle: "已取消订阅", okBody: (x) => (x && x.hint) || "", badTitle: "取消订阅没成", clusters: false });
      if (res.ok) this.$emit("reload");
    },
    unregister(m) { return this.unregisterId(m.workshopId, m.name || m.dir); },
    unregisterPending(p) {
      return this.unregisterId(p.workshopId, p.nameKnown ? p.name : "");
    },
    // 删掉已下载的模组文件。keepSetup 为真时是"重下"而不是"卸载"：清单和开关都不动，
    // 下次启动服务端照清单把它重新拉回来——工坊更新了、这台机器上还是旧那份，走的就是这条。
    async removeFiles(m, keepSetup) {
      const rooms = m.usedBy || [];
      // 这一句得说清删的是哪一处的文件：工坊模组真正躺在引擎自己的下载区里
      //（每个世界一份），mods/ 下面可能压根没有目录。只写 mods/ 的话，
      // 用户点完会去 mods/ 里核对，然后得到一个"我明明没删掉"的结论。
      const from = m.downloadedByEngine
        ? "删掉引擎下载区里这个模组的每一份（每个重启过它的世界各一份，mods/ 里本来就没有这个目录）"
        : "删掉 mods/" + m.dir + " 这个目录（引擎下载区里如果还留着这个模组的份，会一起删掉）";
      const steps = [keepSetup ? from + "；订阅和各个世界的开关都不动，下次重启服务端会重新下载它" : from];
      if (!keepSetup && m.workshopId) {
        steps.push("从下载清单里取消订阅 " + m.workshopId + "，下次重启不会再把它下回来");
      }
      if (!keepSetup && rooms.length) {
        steps.push("关掉 " + rooms.join("、") + " 对它的启用");
      }
      const warn = keepSetup
        ? "文件删掉到下次重启之间，这个世界里它是没效果的。正在跑的世界会拒绝这一步，" +
          "面板不会替你停服（停服会踢掉正在玩的玩家）。"
        : "文件删了就找不回来——模组在磁盘上被改过的东西会一起没了，要再用只能重新订阅、重启下载。" +
          (rooms.length ? "正在跑的世界会拒绝这一步：先去房间列表停掉 " + rooms.join("、") + "，面板不替你停服。" : "");
      const ok = await askConfirm({
        title: (keepSetup ? "删掉文件、等下次重下？" : "删掉 " + (m.name || m.dir) + "？"),
        body: "这一步做 " + steps.length + " 件事：" + steps.join("；") + "。",
        warn: warn,
        okLabel: keepSetup ? "删掉文件" : "删掉并取消订阅",
      });
      if (!ok) return;
      const key = "mod.files.remove:" + m.dir;
      const res = await act(key, "DELETE",
        "/api/v1/mods/files/" + encodeURIComponent(m.dir) + (keepSetup ? "?keepSetup=1" : ""),
        undefined,
        { okTitle: keepSetup ? "旧文件已清掉，等下次重启重下" : "模组已删干净",
          okBody: (x) => (x && x.hint) || "", badTitle: "删除没成", clusters: false });
      if (res.ok) this.$emit("reload");
    },
    rate(m) { return rate(m); },
    // 整屏已经说过一次的原因，行上就不再重复第二遍（徽标的 title 里还在）。
    rowNote(m) {
      const t = m.modinfoError;
      if (!t) return "";
      return this.mods.some((x) => x !== m && x.modinfoError === t) ? "" : t;
    },
    // 工坊版本和磁盘上这份对不上时，界面只把两个数摆出来，不判谁新——
    // 版本号不是数字（1.10 未必比 1.9 大），判错就是"提示有更新其实没有"。
    versionTip(m) {
      return "面板上次从工坊读到的是 " + (m.workshopVersion || "（没读到）") +
        "，这台机器上 modinfo.lua 写的是 " + (m.version || "（没读到）") +
        "。服务端每次启动会自己按订阅清单去工坊拉，重启一次一般就更新了；" +
        "重启过还是这两个数不一样，就用「删文件重下」把这份旧的清掉。";
    },
    // 开着它的世界。这一句是接口扫过每个世界的 modoverrides.lua 带回来的，不是这里猜的。
    usedText(m) {
      const rooms = m.usedBy || [];
      if (!rooms.length) return "没有世界开着它";
      return "开着它的世界：" + rooms.join("、");
    },
    usedTip(m) {
      const rooms = m.usedBy || [];
      if (!rooms.length) {
        return this.overrideProblems.length
          ? "现在没有任何世界开着它。" + this.overrideProblems.join("、") + " 的启用列表没读到，那几间要自己核对。"
          : "现在没有任何世界开着它。";
      }
      return "开着它的世界：" + rooms.join("、") + "（各世界的 modoverrides.lua 里读到的）";
    },
    // 封面底那枚标记说的是"这目录在不在订阅清单里"，这是这一屏唯一会变的可逆开关。
    // 本地上传的模组必须单独说"本地"：它压根没有工坊 ID，标成"没订阅"会让人以为
    // 少订阅了一行，其实它本来就靠目录本身加载。
    setupBadge(m) {
      if (!m.workshopId) {
        return { kind: "info", text: "本地", title: "没有工坊 ID，是上传到 mods/ 里的本地模组，服务端直接按目录加载" };
      }
      if (m.inSetupList) {
        return { kind: "ok", text: "已订阅", title: "订阅清单里有这一行，服务端启动时会从工坊拉它" };
      }
      return { kind: "idle", text: "没订阅", title: "磁盘上有目录，但订阅清单里没有这一行：不会再从工坊更新它，服务端照样按目录加载" };
    },
    pendBadge(p) {
      return { kind: "warn", text: "等重启下载",
        title: "订阅清单里有这一行，磁盘上还没有目录：服务端下次启动才会去工坊拉它" +
          (p.version ? "（清单上记的版本：" + p.version + "）" : "") };
    },
    // 待下载这一栏缺的只是开关：订阅那一步它本来就是订阅清单里的一行。
    // 所以这一栏也要能给一枚「在这个房间打开」，否则用户还得自己找到开关那一屏。
    pendBtn(p) {
      return openLabel(true, this.worlds,
        openedIn(this.openByDir["workshop-" + p.workshopId], this.cluster, this.worlds));
    },
    pendBusy(p) { return !!state.busy["mod.open:" + p.workshopId]; },
    async openPending(p) {
      const res = await subscribeAndOpen(p.workshopId, this.cluster, this.worlds, "已开在这个房间");
      if (res.ok) this.$emit("reload");
    },
    pendTitle(p) {
      if (p.nameKnown) return p.name;
      return "还没读到这个名字";
    },
    // 待下载那张卡的第二行：缓存里有什么就写什么，一个都没有时整行不画（不写"版本未知 大小未知"）。
    pendMeta(p) {
      const out = [];
      if (p.version) out.push("版本 " + p.version);
      if (p.sizeLabel) out.push(p.sizeLabel);
      if (p.updatedLabel) out.push("更新于 " + p.updatedLabel);
      return out.join(" · ");
    },
  },
  template: `
    <div class="col" style="gap:16px">
      <!-- 一屏按"文件到底在不在磁盘上"分成三张卡：已下载 / 已订阅还没下载 / 上传本地包。
           以前三段挤在同一张卡里，"已订阅还没下载"那一栏只剩一串数字——
           用户问的"放个编号上去，难道还要用户自己去核对是哪个 mod 吗"就是这一条。
           现在待下载那几条也带封面和名字（名字与封面只能来自面板见过的工坊条目，
           没见过的那一串就老实写"还没读到"，绝不把 ID 当模组名显示）。 -->
      <section class="card">
        <div class="card-head">
          <span class="ico-tile tint-cyan"><x-icon name="archive" :size="15"></x-icon></span>
          <h3>已下载</h3>
          <span class="chip" v-if="view">{{ mods.length }} 个模组</span>
          <span class="chip" v-if="pending.length">还有 {{ pending.length }} 个待下载</span>
          <span class="grow"></span>
          <button class="linkish" @click="$emit('reload')" :disabled="loading">重读</button>
        </div>

        <x-sk v-if="loading" :lines="4"></x-sk>
        <div class="err-box" v-else-if="error">{{ error }}</div>

        <template v-else-if="view">
          <x-note v-if="view.hint" kind="warn">{{ view.hint }}</x-note>

          <div class="col" style="gap:8px" v-if="setupProblems.length">
            <x-note kind="bad">订阅清单里有 {{ setupProblems.length }} 行面板看不懂。
              这个文件加载失败会让服务端直接退出，所以宁可现在核对。</x-note>
            <ul class="list-plain">
              <li v-for="(p, i) in setupProblems" :key="i" class="mono muted">{{ p }}</li>
            </ul>
          </div>

          <x-note kind="warn" v-if="overrideProblems.length">
            {{ overrideProblems.join('、') }} 的模组启用列表读不出来，卡片上"开着它的世界"这一栏
            对这几间说了不算——要删模组之前自己核对一下那几间。
          </x-note>

          <!-- 同一条原因在几行上一字不差时，整屏只说一次，行上留那枚短徽标
               （和「给这个房间开」那一屏同一套做法）。 -->
          <x-note v-for="(t, i) in sharedNotes" :key="'sn' + i" kind="warn">
            这一屏有 {{ t.names.length }} 行是同一种情况（{{ t.names.join('、') }}）：{{ t.text }}
          </x-note>

          <!-- 磁盘清单：一行一张卡，封面在左。
               以前这里是"桌面一张表格 + 手机一列无图行"两套写法，两套都没有图，
               同一个模组在检索页有封面、到这一屏就只剩一行字——用户说的"不一致"就是这个。
               现在三个屏共用 x-mod-cover（同一张图、同一个色块兜底、同一套标记）。 -->
          <div class="mod-lines mt" v-if="mods.length">
            <article class="mod-line" v-for="m in mods" :key="m.dir">
              <x-mod-cover :it="m" :badge="setupBadge(m)"></x-mod-cover>
              <div class="mod-line-main">
                <div class="mod-line-head">
                  <!-- 名字读不出来时用目录名顶上：卡上留一个"（读不出名字）"比目录名更难核对，
                       而这一行到底为什么没有名字，下面那句后端写的说明会讲。 -->
                  <span class="mod-line-name">{{ m.name || m.dir }}</span>
                  <x-badge v-if="m.builtin" kind="info" text="面板自带"
                           title="这是面板自己带的助手模组，世界状态靠它回传；删掉它面板就看不到这个世界了，所以这里不给删除。"></x-badge>
                  <!-- 文件在引擎自己的下载区里（每个世界各存一份），mods/ 下面并没有这个目录。
                       这一枚说的是来路，不是故障：开关照常用，引擎两种都认。 -->
                  <x-badge v-if="m.downloadedByEngine" kind="info" text="引擎下载区"
                           title="服务端启动时按订阅清单自己拉下来的那一份，存在引擎的下载区里（每个世界各一份），mods/ 下面没有这个目录。开关和删除都照常管它，只是别去 mods/ 里找它。"></x-badge>
                  <x-badge v-if="m.versionDiffers" kind="warn" text="工坊版本对不上"
                           :title="versionTip(m)"></x-badge>
                  <x-badge v-if="m.allClientsRequire" kind="warn" text="玩家也要订阅"
                           title="没在工坊订阅它的玩家会被服务端直接断线"></x-badge>
                  <x-badge v-else-if="m.clientOnly" kind="info" text="只影响客户端"
                           title="服务端开不开它都不影响玩家，效果只出现在客户端"></x-badge>
                  <x-badge v-if="!m.modinfoOk" kind="bad" text="modinfo 读不懂"
                           :title="m.modinfoError"></x-badge>
                  <x-badge v-else-if="m.modinfoError" kind="warn" text="模组没写出名字"
                           :title="m.modinfoError"></x-badge>
                  <div class="mod-line-acts">
                    <a class="linkish" v-if="m.workshopId" :href="wsUrl(m.workshopId)"
                       target="_blank" rel="noopener noreferrer">工坊页</a>
                    <!-- 版本对不上时给的是"删文件重下"，不是"重新订阅"：订阅那行本来就还在，
                         缺的是这台机器上这份旧文件。 -->
                    <button class="linkish" v-if="m.versionDiffers && m.inSetupList"
                            :disabled="!canWrite || kbusy('mod.files.remove:' + m.dir)" :title="canWrite ? '' : writeNote" @click="removeFiles(m, true)">删文件重下</button>
                    <button class="linkish" v-if="m.workshopId && m.inSetupList"
                            :disabled="!canWrite || kbusy('mod.setup.remove:' + m.workshopId)" :title="canWrite ? '' : writeNote" @click="unregister(m)">取消订阅</button>
                    <button class="linkish danger" v-if="!m.builtin"
                            :disabled="!canWrite || kbusy('mod.files.remove:' + m.dir)" :title="canWrite ? '' : writeNote" @click="removeFiles(m, false)">删除</button>
                  </div>
                </div>
                <div class="row mod-meta">
                  <span class="chip mono">{{ m.dir }}</span>
                  <span class="chip mono" v-if="m.workshopId">工坊 {{ m.workshopId }}</span>
                  <span class="chip" v-else>本地上传</span>
                  <span class="chip" v-if="m.version">版本 {{ m.version }}</span>
                  <span class="chip" v-if="m.sizeLabel">{{ m.sizeLabel }}</span>
                  <span class="chip" v-if="m.updatedLabel">更新于 {{ m.updatedLabel }}</span>
                </div>
                <div class="mod-line-used" :title="usedTip(m)">{{ usedText(m) }}</div>
                <div class="mod-rate" v-if="rate(m)">
                  <span v-if="rate(m).fav" :title="rate(m).fav.exact">
                    <x-icon name="star" :size="12"></x-icon>{{ rate(m).fav.text }}</span>
                  <span class="pct" v-if="rate(m).score" :title="rate(m).score.exact">{{ rate(m).score.text }}</span>
                </div>
                <!-- 整句由后端写（ mods.go 里那三种情况各有说法），这里不许再套一层"读不懂"前缀：
                     文件读得通、只是名字被作者写成了算出来的表达式，那种写成"读不懂"就是假警报。
                     重复出现的那几种已经收到屏顶那一句里了（sharedNotes）。 -->
                <div class="shard-note bad" v-if="!m.modinfoOk && rowNote(m)">{{ rowNote(m) }}</div>
                <div class="shard-note" v-else-if="rowNote(m)">{{ rowNote(m) }}</div>
                <div class="shard-note" v-if="m.dependencies && m.dependencies.length">
                  它声明的依赖：
                  <span class="chip mono" v-for="(g, i) in m.dependencies" :key="i">{{ g.join(' 或 ') }}</span>
                </div>
              </div>
            </article>
          </div>
          <x-empty v-else icon="cube" title="这台机器上一个模组都没有"
            note="到「找模组」那一屏点「订阅并开启」，下载清单和这个房间的开关会一次写完；
                  本地模组包就上传到下面那张卡，上传完在「这个房间的开关」里打开它。
                  订阅好的模组要等这个房间的世界重启时才会从工坊下载，所以它这会儿只会出现在「已订阅、等重启下载」那张卡里。">
          </x-empty>

          <x-note kind="info" v-if="localOnly.length">
            有 {{ localOnly.length }} 个目录不在订阅清单里（{{ localOnly.join('、') }}）：
            它们通常是手工上传的本地模组，或者是取消订阅后留在磁盘上的旧文件。服务端照样会加载它们。
          </x-note>
        </template>
      </section>

      <section class="card" v-if="!loading && view && pending.length">
        <div class="card-head">
          <span class="ico-tile tint-amber"><x-icon name="download" :size="15"></x-icon></span>
          <h3>已订阅、等重启下载</h3>
          <span class="chip">{{ pending.length }} 个</span>
        </div>
        <div class="mod-cards">
          <article class="mod-card" v-for="p in pending" :key="p.workshopId">
            <x-mod-cover :it="p" :badge="pendBadge(p)"></x-mod-cover>
            <div class="mod-card-body">
              <div class="mod-card-name">{{ pendTitle(p) }}</div>
              <div class="mod-card-meta mono">工坊 ID {{ p.workshopId }}</div>
              <div class="mod-rate" v-if="rate(p)">
                <span v-if="rate(p).fav" :title="rate(p).fav.exact">
                  <x-icon name="star" :size="12"></x-icon>{{ rate(p).fav.text }}</span>
                <span class="pct" v-if="rate(p).score" :title="rate(p).score.exact">{{ rate(p).score.text }}</span>
              </div>
              <div class="mod-card-meta" v-if="pendMeta(p)">{{ pendMeta(p) }}</div>
              <div class="mod-card-note" v-if="!p.nameKnown">
                面板还没在工坊里见过这串 ID，名字和封面只能空着；它下载完就会用模组自己写的名字。
              </div>
              <div class="mod-card-warns" v-if="p.clientRequired || p.hasChildren">
                <span class="mod-warn" v-if="p.clientRequired">要玩家自己订阅</span>
                <span class="mod-warn" v-if="p.hasChildren">带子模组</span>
              </div>
            </div>
            <div class="mod-card-foot">
              <!-- 这一栏的模组只缺开关：订阅那一步它本来就是清单里的一行。 -->
              <span class="chip" :class="pendBtn(p).tone" v-if="pendBtn(p).done">
                <x-icon name="check" :size="13"></x-icon>{{ pendBtn(p).text }}</span>
              <button v-else class="btn sm primary" :disabled="!canWrite || pendBusy(p)"
                      :title="canWrite ? '' : writeNote" @click="openPending(p)">{{ pendBtn(p).text }}</button>
              <button class="btn sm ghost" :disabled="!canWrite || kbusy('mod.setup.remove:' + p.workshopId)"
                      :title="canWrite ? '' : writeNote" @click="unregisterPending(p)">取消订阅</button>
              <a class="btn sm ghost" :href="wsUrl(p.workshopId)" target="_blank" rel="noopener noreferrer">工坊页</a>
            </div>
          </article>
        </div>
        <!-- 这一栏必须能撤销：ID 填错了、或者工坊那边根本没这个东西，目录就永远不会出现，
             而"工坊检索"只列面板见过的模组——那时候磁盘上没目录、检索里没条目，
             清单里那一行就成了撤不掉的残留。 -->
        <div class="hint mt">工坊内容只在服务端启动时拉取，所以这里等的是重启，不是进度条。
          等不及或者 ID 填错了，就在这张卡上取消订阅。</div>
      </section>

      <section class="card" v-if="!loading && view && unmet.length">
        <div class="card-head">
          <span class="ico-tile tint-rose"><x-icon name="warn" :size="15"></x-icon></span>
          <h3>依赖在这台机器上找不到</h3>
          <span class="chip">{{ unmet.length }} 组</span>
        </div>
        <x-note kind="warn">服务端碰到这种模组是静默禁用：不报错、也不拒绝启动，
          看上去就是"开了却没效果"。</x-note>
        <ul class="list-plain mt">
          <li v-for="(p, i) in unmet" :key="i">
            <b>{{ p.name || p.dir }}</b>
            <span class="muted"> —— {{ p.status }}</span>
            <div class="hint">{{ p.hint }}</div>
            <div class="hint mono" v-if="p.candidates && p.candidates.length">候选标识：{{ p.candidates.join(' / ') }}</div>
          </li>
        </ul>
      </section>

      <section class="card" v-if="!loading && view">
        <div class="card-head">
          <span class="ico-tile tint-violet"><x-icon name="upload" :size="15"></x-icon></span>
          <h3>上传本地模组包</h3>
          <span class="chip">上限 64 MB</span>
        </div>
        <x-note kind="info">面板不会覆盖同名目录，也不会执行包里的任何 Lua。</x-note>
        <div class="grid-form mt">
          <div class="field">
            <label>zip 文件</label>
            <div class="drop" :class="drag ? 'over' : (file ? 'has' : '')" tabindex="0" role="button"
                 @click="choose" @keydown.enter.prevent="choose" @keydown.space.prevent="choose"
                 @dragenter.prevent="drag = true" @dragover.prevent="drag = true"
                 @dragleave.prevent="drag = false" @drop.prevent="drop($event)">
              <x-icon name="upload" :size="18"></x-icon>
              <div class="drop-t">{{ fileName || '把模组包拖到这里' }}</div>
              <div class="drop-s">{{ fileName ? '已拿到 ' + fileSize + '，要换包再点一次这一片' : '或者点这里选出文件' }}</div>
            </div>
            <input class="drop-file" type="file" ref="file" accept=".zip,application/zip" @change="pick">
          </div>
          <div class="field">
            <label>落地目录名</label>
            <span class="chip mono" v-if="!dirBad">{{ dirPreview || '选完 zip 自动定' }}</span>
            <input type="text" v-else v-model="name" :placeholder="dirPreview">
            <span class="hint" v-if="!dirBad">不用填：默认就是 zip 的文件名（去掉 .zip），
              它会成为 modoverrides.lua 里的键。</span>
            <span class="hint" v-else>文件名里那串当不了目录名（只能是字母、数字、下划线、点或连字符，
              而且不能以点开头），给它起一个。</span>
          </div>
        </div>
        <div class="row spread mt" style="gap:8px;flex-wrap:wrap">
          <span class="grow"></span>
          <button class="btn primary" :disabled="!canWrite || busy" :title="canWrite ? '' : writeNote" @click="upload">{{ busy ? '上传中……' : '上传并落地' }}</button>
        </div>
        <div class="err-box mt" v-if="err">{{ err }}</div>
      </section>
    </div>`,
};

// ---- 工坊检索 ----

const Workshop = {
  props: { cluster: String,
           // worlds 是当前房间的世界名单（Master / Caves），openByDir 是"目录 -> 开着它的世界"。
           // 有了这两样，这一屏才敢把「订阅」和「开在这个房间」合成一个按钮。
           worlds: { type: Array, default: () => [] },
           openByDir: { type: Object, default: () => ({}) } },
  emits: ["reload", "tab"],
  setup() { return { state, num }; },
  data() {
    // size=12：这是一张卡片网格，10 张在 2/3/4/5 列下都会留下半行孤卡，
    // 12 能被 2、3、4 整除，最后一行基本是排满的。
    return { q: "", id: "", tag: "", view: null, loading: false, error: "", page: 1, size: 12,
             detail: null };
  },
  // 一进来就发一次空查询：接口在没给搜索词时列的是 Steam 工坊自己的榜（实测能一页页翻），
  // 用户"不知道要什么模组、只想翻一翻"的那条路就是这一屏。
  // 等用户点「搜」才加载，这个标签页打开就是半屏空白，看着像坏了。
  mounted() { this.run(1); },
  computed: {
    // 检索是只读的 GET，不走 act，所以它的"忙"就是 loading 本身。
    busy() { return this.loading; },
    adding() { return !!state.busy["mod.setup.add"]; },
    items() { return (this.view && this.view.items) || []; },
    // 分类那一排的名目和条数都由接口给（modsearch.go 的 modCategories），前端不再存一份，
    // 否则两边字典会各自长歪，界面写"宠物"而请求发出去的是别的键。
    cats() { return (this.view && this.view.categories) || []; },
    // countsKnown 说"这一排后面的条数是不是这一屏的口径"。接口在没法同一口径计数时
    // （在线不带分类、按 ID 精确查）宁可不给数字，所以这里也不画。
    countsKnown() { return !!(this.view && this.view.categoryCountsKnown); },
    // 同一排数字在两种屏上数的是两批东西：在线那一屏数的是这次取回来筛的那 100 条，
    // 纯缓存那一屏数的是缓存里对得上关键词的那些。悬停标题必须跟着换说法，
    // 否则缓存屏上的"这一批里有 6 条"就是在把 49 条缓存说成别的东西。
    countsScope() {
      if (!this.view) return "";
      return this.view.live ? "这次从工坊取回的一批里" : "本地缓存里";
    },
    // 空列表有两种完全不同的原因，得分开说：一种是"搜了但没有"（该换词），
    // 一种是"面板一条都还没见过"（该去配 key 或先装一个）。混成一句"没搜到"，
    // 第一次打开这一页的人只会以为面板坏了。
    emptyKind() {
      if (!this.view) return "failed";
      if (this.items.length) return "";
      return this.view.cacheCount ? "noresult" : "nocache";
    },
    // 热度条的刻度只能对着"这一页里最高的那条"折，接口不给全局分位数，
    // 拿一个编出来的满分画就是假刻度。
    heatMax() { return this.items.reduce((m, it) => Math.max(m, it.subscriptions | 0), 0); },
    heatNote() { return heatNote; },
    // 一次点击能不能连带把开关写好，取决于这一间房里到底有没有世界。
    // 没有房间时这个按钮退回纯订阅，而不是把请求发出去等一个 404。
    canOpen() { return !!this.cluster && this.worlds.length > 0; },
    // 工坊「订阅」是全局动作（下载清单），没选房间也该能做，用房间权限判会把
    // 名下暂时没房间的成员一起挡掉；这里只挡只读账号。「订阅并开启」另有 canOpen 把关。
    canWrite() { return !!(state.me && state.me.role !== "viewer"); },
    writeNote() { return "这个账号是只读：能看状态，但修改会被拒（改自己的密码除外）"; },
  },
  methods: {
    // 封面格、星星那一格、热度条、色块上的字都在 js/modcard.js 里：
    // 检索页、下载页、配置页画的是同一个模组，三处不能各画各的。
    heat(it) { return heat(it, this.heatMax); },
    rate(it) { return rate(it); },
    // 安装状态在卡片上只用两个字：一行一张卡的手机上，"已下载，也在下载清单里"
    // 会占满整条图底。完整那句在详情里，也在这行的 title 上。
    statusBadge(it) {
      if (it.downloaded && it.registered) return { kind: "ok", text: "已装", title: it.statusLabel };
      if (it.registered) return { kind: "info", text: "等重启下载", title: it.statusLabel };
      if (it.downloaded) return { kind: "warn", text: "没订阅", title: it.statusLabel };
      return { kind: "idle", text: "没装", title: it.statusLabel };
    },
    // 卡片上只有一行元信息的位置，所以拼成一句然后省略，而不是排五个 chip：
    // 五个 chip 在 150px 宽的卡上会折成三行，卡与卡就高低不齐了。
    // 订阅数不在这儿——它有自己那一根条，见 heat()。
    metaLine(it) {
      const bits = [];
      if (it.sizeLabel) bits.push(it.sizeLabel);
      if (it.updatedLabel) bits.push("更新于 " + it.updatedLabel);
      if (it.version) bits.push("版本 " + it.version);
      return bits.join(" · ");
    },
    // 工坊说明里满是论坛排版码：[h3]…[/h3]、[url=…]…[/…]、[*]、[list]。
    // 实测 2026-11-01 一屏 12 张卡里有 8 张开头就是这些。卡片是给人扫的，
    // 把这些码原样画出来等于把作者的编辑器当成内容。只删成对的方括号标记：
    // 必须以字母或 * 开头，所以 [2024-11-08] 这种真的方括号内容不会被误伤。
    plainDesc(it) {
      return String(it.desc || "")
        .replace(/\[(?:\/?[A-Za-z][A-Za-z0-9_]*|\*)(?:=[^\]\n]*)?\]/g, " ")
        .replace(/\s+/g, " ")
        .trim();
    },
    detailRows(it) {
      const rows = [{ k: "工坊 ID", v: it.workshopId, mono: true }];
      if (it.version) rows.push({ k: "版本", v: it.version });
      if (it.subscriptions) rows.push({ k: "工坊订阅数", v: num(it.subscriptions) });
      if (it.favorited) rows.push({ k: "工坊收藏数", v: num(it.favorited) });
      // 评分和投票人数写成同一行的两个字段：它们来自 Steam 的两个不同数，
      // 而这个分数不是按投票人数算出来的（见 rate() 那段实测），并列展示但不互相推导。
      if ((it.votesTotal | 0) > 0 && it.voteScore > 0) {
        rows.push({ k: "工坊评分", v: Math.round(it.voteScore * 100) + "%（" +
          num(it.votesTotal) + " 人投过票）" });
      }
      if (it.sizeLabel) rows.push({ k: "文件大小", v: it.sizeLabel });
      if (it.updatedLabel) rows.push({ k: "模组最后更新", v: it.updatedLabel });
      rows.push({ k: "这条信息从哪来", v: it.sourceLabel || "本地缓存" });
      rows.push({ k: "在这台机器上", v: it.statusLabel || "不知道" });
      return rows;
    },
    workshopUrl(it) {
      return "https://steamcommunity.com/sharedfiles/filedetails/?id=" + encodeURIComponent(it.workshopId);
    },
    // 换分类必须回到第 1 页：停在第 4 页换一个只有 3 条的分类，界面会变成"什么都没有"，
    // 而那只是因为它翻过了这一类的末尾（同一条规则在房间列表那一页也踩过）。
    pickTag(key) {
      if (key === this.tag) return;
      this.tag = key;
      this.run(1);
    },
    async run(page) {
      this.error = "";
      this.loading = true;
      const p = page || 1;
      const res = await api.get("/api/v1/mods/search" +
        api.qs({ q: this.q.trim(), tag: this.tag, page: p, size: this.size }));
      this.loading = false;
      if (!res.ok) { this.error = res.error || ("接口返回 " + res.status); this.view = null; return; }
      this.view = res.data;
      this.page = this.view.page || p;
      // 选中的分类以接口回显为准：按 ID 精确查时后端会把分类丢掉（标签不参与筛选），
      // 前端跟着它把高亮退回「不限分类」，而不是留一个"看着选中了其实没生效"的按钮。
      this.tag = this.view.tag || "";
      this.syncDetail();
    },
    // 详情可能是开着的时候被订阅刷新掉的：那批 items 是接口重新拿的新对象，
    // 继续拿着旧的那条，按钮上会一直写着"订阅"，点了第二次才知道已经订阅过了。
    syncDetail() {
      if (!this.detail) return;
      const fresh = this.items.find((x) => x.workshopId === this.detail.workshopId);
      if (fresh) this.detail = fresh;
    },
    // 主按钮那一句：订阅状态 + 这个房间的开关状态折出来的。
    btn(it) {
      return openLabel(!!it.registered, this.canOpen ? this.worlds : [],
        openedIn(this.openByDir["workshop-" + it.workshopId], this.cluster, this.worlds));
    },
    // 悬停那句要把"开在哪几个世界"和"还差哪个"说清：卡片上只有一个按钮的宽度，
    // 但点之前人应该知道这一发要写几个文件。
    btnTip(it) {
      if (!this.canOpen) return "这一间房里还没有世界，所以只能先订阅；建好世界再去开关那一屏打开。";
      const opened = openedIn(this.openByDir["workshop-" + it.workshopId], this.cluster, this.worlds);
      const left = this.worlds.filter((w) => opened.indexOf(w) < 0);
      const bits = ["会写两个文件：下载清单，以及 " +
        this.cluster + " / " + (left.length ? left.join("、") : this.worlds.join("、")) +
        " 的 modoverrides.lua"];
      if (opened.length && left.length) bits.push("已经开在 " + opened.join("、"));
      if (!it.downloaded) bits.push("文件要等这个世界下次重启时才从工坊下载");
      return bits.join("；") + "。";
    },
    openBusy(it) {
      return this.adding || !!state.busy["mod.open:" + it.workshopId];
    },
    isOpenIn(it, w) {
      return openedIn(this.openByDir["workshop-" + it.workshopId], this.cluster, [w]).length > 0;
    },
    async onPrimary(it) {
      const b = this.btn(it);
      if (b.canOpen) return this.openIn(it.workshopId);
      return this.addOne(it.workshopId);
    },
    // 一次点击做完"订阅 + 在这个房间的所有世界打开"。
    // 默认两个世界一起写是刻意的：引擎只同步世界规则，模组清单各世界读各自的覆盖文件，
    // 只开地表会安静地开出两个规则不同的世界（玩家穿过传送门才发现模组没生效）。
    async openIn(id) {
      const res = await subscribeAndOpen(id, this.cluster, this.worlds, "已订阅并开在这个房间");
      if (res.ok) { this.$emit("reload"); await this.run(this.page); }
    },
    async addOne(workshopId) {
      const res = await act("mod.setup.add", "POST", "/api/v1/mods/setup", { workshopId: String(workshopId) },
        { okTitle: "已订阅，进下载清单", okBody: (x) => (x && x.hint) || "", badTitle: "订阅没成", clusters: false });
      // 订阅接口回的是"磁盘清单"（modsView），不是这里的检索结果（modSearchView）：
      // 两者的字段不一样，直接塞进 this.view 会把列表清空。这里只重跑一次当前页的检索，
      // 让"已订阅/已下载"那几个标记从接口重新拿，而不是自己改内存里的那条。
      if (res.ok) { this.$emit("reload"); await this.run(this.page); }
    },
    async addById() {
      this.error = "";
      const raw = this.id.trim();
      if (!raw) { this.error = "把工坊页地址里那串 id 填进来（?id= 后面那一串数字）。"; return; }
      if (!/^\d+$/.test(raw)) {
        this.error = "那一串里只能有数字：工坊页地址是 …/sharedfiles/filedetail.php?id=【一串数字】。";
        return;
      }
      await (this.canOpen ? this.openIn(raw) : this.addOne(raw));
      this.id = "";
    },
    async removeOne(it) {
      const ok = await askConfirm({
        title: "取消订阅 " + it.name + "？",
        body: "只改下载清单，服务端下次启动不会再拉它。工坊 ID " + it.workshopId + "。",
        warn: "已经下载好的文件不会删（mods/ 里的目录和引擎自己下载区里的那几份都留着）。要连文件一起清掉，去「这台机器上的文件」那一屏删。" +
          "已经打开它的世界，开关也要在那一屏关掉。",
        okLabel: "取消订阅",
      });
      if (!ok) return;
      const res = await act("mod.setup.remove:" + it.workshopId, "DELETE",
        "/api/v1/mods/setup/" + encodeURIComponent(it.workshopId), undefined,
        { okTitle: "已取消订阅", okBody: (x) => (x && x.hint) || "", badTitle: "取消订阅没成", clusters: false });
      if (res.ok) { this.$emit("reload"); await this.run(this.page); }
    },
  },
  template: `
    <section class="card">
      <div class="card-head">
        <span class="ico-tile tint-azure"><x-icon name="search" :size="15"></x-icon></span>
        <h3>找模组</h3>
        <!-- 在线/缓存这一枚是这一屏数据的来路，位置不动；房间那一句单独占一行，
             因为它说的是"点下去写到哪"，和来路不是一回事，挤在标题行里两个都读不清。 -->
        <span class="chip accent" v-if="view && view.live"><x-icon name="globe" :size="13"></x-icon>在线结果</span>
        <span class="chip" v-else-if="view"><x-icon name="archive" :size="13"></x-icon>本地缓存</span>
        <x-hint v-if="heatMax > 1" label="订阅热度条" :text="heatNote"></x-hint>
        <span class="grow"></span>
        <span class="chip mono" v-if="view">缓存 {{ view.cacheCount }} 条</span>
      </div>

      <x-note v-if="canOpen" kind="info">
        卡片上那枚「订阅并开启」一次做完两件事：写进下载清单，并把 {{ cluster }} 的
        <b>{{ worlds.join('、') }}</b> 都打开。文件要等这些世界下次重启时才从工坊下载，所以点完重启一次再看效果。
      </x-note>
      <x-note v-else kind="warn">
        {{ cluster ? '这个房间里还没有世界' : '这台机器上还没有房间' }}，所以这一屏只能先订阅，
        开关要等世界建好再打开。
      </x-note>

      <div class="grid-form">
        <div class="field">
          <label>按名字搜 <x-hint text="中文和英文都试：工坊标题大多还是英文名。"></x-hint></label>
          <div class="row" style="gap:8px">
            <input type="search" v-model="q" placeholder="比如 背包 扩容 stack" @keyup.enter="run(1)" style="flex:1">
            <button class="btn" :disabled="busy" @click="run(1)">
              <x-icon name="search" :size="15"></x-icon>{{ busy ? '搜中……' : '搜' }}</button>
          </div>
        </div>
        <div class="field">
          <label>已经有 ID，{{ canOpen ? '订阅并开在这个房间' : '直接订阅' }}
            <x-hint :text="canOpen ? '订阅不需要 Steam key。点一次同时写下载清单和这个房间各世界的开关；文件要等世界重启时才从工坊下载。'
                                    : '订阅不需要 Steam key；文件要等这个房间的世界重启时才会下载。'"></x-hint></label>
          <div class="row" style="gap:8px">
            <input type="text" v-model="id" placeholder="工坊页 ?id= 后面的数字" @keyup.enter="addById" style="flex:1">
            <button class="btn" :disabled="!canWrite || adding" :title="canWrite ? '' : writeNote" @click="addById">
              <x-icon name="plus" :size="15"></x-icon>{{ canOpen ? '订阅并开启' : '订阅' }}</button>
          </div>
        </div>
      </div>

      <!-- 分类那一排：名目、条数、选中的是哪一枚全都来自接口（见 cats / countsKnown）。
           在线那一屏的分类是面板在一次取回的整批里筛的，接口不认标签参数（实测），
           所以"这一类有几条"只有接口知道，这里绝不自己数 items。 -->
      <div class="mod-cats" v-if="cats.length">
        <span class="lab">分类</span>
        <button class="mod-cat" :class="!tag ? 'on' : ''" @click="pickTag('')">不限分类</button>
        <button class="mod-cat" v-for="c in cats" :key="c.key"
                :class="tag === c.key ? 'on' : ''" @click="pickTag(c.key)"
                :title="countsKnown ? countsScope + '「' + c.label + '」有 ' + c.count + ' 条' : ''">
          {{ c.label }}<span class="n" v-if="countsKnown">{{ c.count }}</span>
        </button>
      </div>

      <x-note v-if="view && view.note" kind="info">{{ view.note }}</x-note>
      <div class="row mt" style="gap:6px" v-else-if="!loading">
        <span class="muted" style="font-size:12.5px">这一屏连一句说明都没拿到，点上面的「搜」重试一次。</span>
        <x-hint text="在线那一整张榜要配了 steamApi.webApiKey 才发得出去；订阅这一步永远不需要它。" icon="info"></x-hint>
      </div>
      <div class="err-box" v-if="error">{{ error }}</div>

      <x-sk v-if="loading" :lines="5"></x-sk>
      <x-empty v-else-if="emptyKind === 'failed'" icon="warn" title="这批结果没拿到"
        note="上面是接口返回的原话。接口活着的时候这一页会默认列出面板已知的模组，重试一次看看。">
        <button class="btn sm ghost" @click="run(1)"><x-icon name="refresh" :size="14"></x-icon>再试一次</button>
      </x-empty>
      <x-empty v-else-if="emptyKind === 'nocache'" icon="cube" title="面板还没见过任何模组"
        :note="view && view.onlineEnabled ? '这台机器的模组目录是空的，在线那一路这会儿也没答上来（上面那句是原因）——它通了的时候不给关键词也有一整张榜可翻，等不了就把工坊 ID 直接订阅上。'
                                             : '这台机器的模组目录是空的，在线检索又还没配 key，所以这一屏只能先空着。把工坊页地址里那串 ID 填到上面订阅，不需要 key。'">
        <button class="btn sm ghost" @click="$emit('tab', 'disk')">去上传一个模组</button>
      </x-empty>
      <x-empty v-else-if="emptyKind === 'noresult'" icon="search" title="一条都没搜到"
        note="换关键词再试，或者把工坊页地址里那串 ID 直接填到上面订阅。这里搜不到不等于工坊没有。"></x-empty>

      <div class="mod-cards" v-else-if="view">
        <article class="mod-card" v-for="it in items" :key="it.workshopId">
          <x-mod-cover :it="it" :badge="statusBadge(it)"></x-mod-cover>
          <div class="mod-card-body">
            <div class="mod-card-name">{{ it.name || it.workshopId }}</div>
            <div class="mod-heat" v-if="heat(it)" :title="heatNote">
              <span class="mod-heat-bar"><i :style="{ width: heat(it).pct + '%' }"></i></span>
              <span class="mod-heat-num" :title="heat(it).exact">{{ heat(it).text }}</span>
            </div>
            <div class="mod-rate" v-if="rate(it)">
              <span v-if="rate(it).fav" :title="rate(it).fav.exact">
                <x-icon name="star" :size="12"></x-icon>{{ rate(it).fav.text }}</span>
              <span class="pct" v-if="rate(it).score" :title="rate(it).score.exact">{{ rate(it).score.text }}</span>
            </div>
            <div class="mod-card-meta" v-if="metaLine(it)">{{ metaLine(it) }}</div>
            <div class="mod-card-desc" v-if="plainDesc(it)">{{ plainDesc(it) }}</div>
            <div class="mod-card-warns" v-if="it.clientRequired || it.hasChildren">
              <span class="mod-warn" v-if="it.clientRequired">要玩家自己订阅</span>
              <span class="mod-warn" v-if="it.hasChildren">带子模组</span>
            </div>
          </div>
          <div class="mod-card-foot">
            <!-- 一枚按钮跟着状态走：没订阅是「订阅并开启」，订阅了但这个世界没开是「在这个房间打开」，
                 开齐了就不给按钮了——按下去什么都不改的按钮是这一页最没用的东西，改成一句陈述。
                 取消订阅收进详情层和「这台机器上的文件」那一屏：卡片脚只有两枚控件的位置，
                 而撤销这件事的主场景是"我在管这台机器上的文件"，不是"我在挑新模组"。 -->
            <span class="chip" :class="btn(it).tone" v-if="btn(it).done" :title="btnTip(it)">
              <x-icon name="check" :size="13"></x-icon>{{ btn(it).text }}</span>
            <button v-else class="btn sm primary" :disabled="!canWrite || openBusy(it)" :title="canWrite ? btnTip(it) : writeNote"
                    @click="onPrimary(it)">{{ btn(it).text }}</button>
            <button class="btn sm ghost" @click="detail = it">详细信息</button>
          </div>
        </article>
      </div>

      <div class="row spread mt" style="gap:8px;flex-wrap:wrap" v-if="view">
        <button class="linkish" @click="$emit('tab', 'disk')">看磁盘上装成了什么</button>
        <x-pager :page="page" :size="size" :total="view.total || 0" :has-more="!!view.hasMore"
                 :loading="loading" @go="run"></x-pager>
      </div>

      <!-- 详情层：卡片只有两行的地方，所以说明全文、标签、依赖提醒都放这里。
           点遮罩和右上角都能关；订阅/取消订阅成功后不关，卡片和这层会一起刷新。
           必须 teleport 到 body：这一页的 .card 有 container-type，那等于 contain:layout，
           卡片里的 position:fixed 会以卡片为定位父级，遮罩就只盖得住这张卡。 -->
      <teleport to="body">
        <div class="mask" v-if="detail" @click.self="detail = null">
          <div class="dialog mod-detail" role="dialog" aria-modal="true">
            <x-mod-cover :it="detail" cls="mod-detail-cover"></x-mod-cover>
            <div class="mod-detail-body">
              <div class="row spread mod-detail-head">
                <h3>{{ detail.name || detail.workshopId }}</h3>
                <button class="icon-btn" @click="detail = null" aria-label="关闭这个模组的详情">
                  <x-icon name="close" :size="16"></x-icon></button>
              </div>
              <div class="mod-card-meta" v-if="metaLine(detail)">{{ metaLine(detail) }}</div>
              <x-kv :rows="detailRows(detail)"></x-kv>
              <div class="mod-detail-desc" v-if="plainDesc(detail)">{{ plainDesc(detail) }}</div>
              <div class="mod-detail-desc muted" v-else>工坊和缓存里都没有这个模组的说明文字。</div>
              <div class="row mod-tags" v-if="detail.tags && detail.tags.length">
                <span class="mod-tag" v-for="t in detail.tags" :key="t">{{ t }}</span>
              </div>
              <x-note v-if="detail.clientRequired" kind="warn">
                这个模组要求每个玩家自己在工坊订阅它。服务端装上只解决一半，没订阅的人会直接被断线。
              </x-note>
              <x-note v-else-if="detail.serverOnly" kind="info">只给服务端用，玩家那边不用装。</x-note>
              <x-note v-if="detail.hasChildren" kind="warn">
                它带子模组。子模组的 ID 要一起订阅，否则服务端会因为缺文件报错——
                面板不会替你把别人的依赖列表写进清单，这一步请照模组说明补。
              </x-note>
              <!-- 逐世界的开关状态：卡片上那一枚按钮写的是"这个房间"，但一个房间有两个世界，
                   只开了一半时这一屏要能指出差的是哪一个。 -->
              <div class="row mod-meta" v-if="canOpen">
                <span class="chip" v-for="w in worlds" :key="w"
                      :class="isOpenIn(detail, w) ? 'ok' : 'warn'">{{ w }}：{{ isOpenIn(detail, w) ? '开着' : '没开' }}</span>
              </div>
              <div class="dialog-foot">
                <a class="btn ghost" :href="workshopUrl(detail)" target="_blank" rel="noopener noreferrer">工坊页</a>
                <button class="btn ghost" v-if="detail.registered" :disabled="!canWrite || adding"
                        :title="canWrite ? '' : writeNote" @click="removeOne(detail)">取消订阅</button>
                <span class="grow"></span>
                <button class="btn ghost" @click="detail = null">关闭</button>
                <span class="chip" :class="btn(detail).tone" v-if="btn(detail).done">
                  <x-icon name="check" :size="13"></x-icon>{{ btn(detail).text }}</span>
                <button class="btn primary" v-else :disabled="!canWrite || openBusy(detail)" :title="canWrite ? btnTip(detail) : writeNote"
                        @click="onPrimary(detail)">{{ btn(detail).text }}</button>
              </div>
            </div>
          </div>
        </div>
      </teleport>
    </section>`,
};

// ---- 跨分片一致性 ----

const Consistency = {
  props: { cluster: String, shard: String, compact: Boolean },
  emits: ["fix-shard"],
  setup() { return { state }; },
  data() { return { view: null, loading: false, error: "", open: false }; },
  computed: {
    issues() { return (this.view && this.view.issues) || []; },
    shards() { return (this.view && this.view.shards) || []; },
    // compactNote 是降级成一句话时的那半句人话：光说"3 处不一样"不告诉人要不要管。
    compactNote() {
      // 只说这一栏对不了，不重说"没有房间"的理由——那句在页顶那条里已经说过了。
      if (!this.cluster) return "还没有房间，两边都还没有模组清单可对。";
      if (this.loading) return "正在对两个世界的模组清单。";
      if (this.error) return "这一眼没对上：" + this.error;
      if (!this.view) return "";
      return this.issues.length
        ? "地表与洞穴各读各的模组清单，现在两边有 " + this.issues.length +
          " 处不一样。点开看是哪几项、怎么改。"
        : "地表与洞穴各读各的模组清单，现在两边一样，不用管。";
    },
  },
  watch: {
    cluster: { immediate: true, handler() { this.load(); } },
  },
  methods: {
    async load() {
      if (!this.cluster) return;
      this.loading = true;
      this.error = "";
      const res = await api.get("/api/v1/clusters/" + encodeURIComponent(this.cluster) + "/mods/consistency");
      this.loading = false;
      if (this.cluster !== this.$props.cluster) return;
      if (!res.ok) { this.error = res.error || ("接口返回 " + res.status); this.view = null; return; }
      this.view = res.data;
    },
    // byShard 那张表的键是后端给的中文状态，逐个分片摊开成两行，
    // 省得用户自己去开文件比对。
    states(issue) {
      const m = issue.byShard || {};
      return Object.keys(m).map((k) => ({ shard: k, text: m[k] }));
    },
  },
  template:
    `
    <section class="card" v-if="!compact || open">
      <div class="card-head">
        <span class="ico-tile tint-teal"><x-icon name="shield" :size="15"></x-icon></span>
        <h3>两个世界是不是同一套模组规则</h3>
        <span class="chip" v-if="view">{{ issues.length ? issues.length + ' 处不一致' : '一致' }}</span>
        <span class="grow"></span>
        <button class="linkish" @click="load" :disabled="loading">重查</button>
        <button class="linkish" v-if="compact" @click="open = false">收起成一句</button>
      </div>

      <x-note kind="info">
        引擎只在地表与洞穴之间同步<b>世界规则</b>，模组清单各世界读各自的文件。
        所以"地表开了、洞穴没开"会安静地开出两个规则不同的世界，玩家穿传送门过去才发现模组加的东西不存在。
      </x-note>

      <x-sk v-if="loading" :lines="3"></x-sk>
      <div class="err-box" v-else-if="error">{{ error }}</div>

      <template v-else-if="view">
        <div class="row mod-meta" style="margin-bottom:8px">
          <span class="chip" v-for="s in shards" :key="s.shard">
            {{ s.shard }}：{{ (s.enabled || []).length }} 个开着{{ s.present ? '' : '（还没有覆盖文件）' }}
          </span>
        </div>
        <div class="hint" v-if="view.note">{{ view.note }}</div>

        <x-empty v-if="!issues.length" icon="check" title="各世界加载的模组与配置项一致"
          :note="view.note || '上面那几个世界的清单对得上。'"></x-empty>

        <div class="col" style="gap:2px" v-else>
          <div class="mod-row" v-for="(is, i) in issues" :key="i">
            <div class="col mod-main">
              <div class="row" style="gap:8px;flex-wrap:wrap">
                <x-badge kind="warn" :text="is.kind"></x-badge>
                <b>{{ is.dir }}</b>
              </div>
              <div class="hint">{{ is.detail }}</div>
              <div class="row mod-meta">
                <span class="chip" v-for="st in states(is)" :key="st.shard">{{ st.shard }}：{{ st.text }}</span>
              </div>
              <div class="hint" v-if="is.fix">怎么做：{{ is.fix }}</div>
              <div class="row mt" style="gap:8px">
                <button class="btn sm" v-for="st in states(is)" :key="st.shard"
                        @click="$emit('fix-shard', st.shard)">去 {{ st.shard }} 改</button>
              </div>
            </div>
          </div>
        </div>
      </template>
    </section>
    <section class="card" v-else>
      <div class="card-head">
        <span class="ico-tile tint-teal"><x-icon name="shield" :size="15"></x-icon></span>
        <h3>两个世界的模组规则</h3>
        <x-badge v-if="view && !loading" :kind="issues.length ? 'warn' : 'ok'"
                 :text="issues.length ? issues.length + ' 处不一样' : '一样'"></x-badge>
        <span class="chip" v-else>{{ loading ? '核对中' : '没对上' }}</span>
        <span class="grow"></span>
        <button class="linkish" @click="open = true">看细节</button>
      </div>
      <div class="hint">{{ compactNote }}</div>
    </section>`,
};

// ---- 页面 ----

export default {
  name: "Mods",
  components: { "x-shard-mods": ShardMods, "x-disk": DiskMods, "x-workshop": Workshop, "x-consistency": Consistency },
  setup() { return { state }; },
  data() {
    // 落在第 1 步：这一页的主顾是"我想加个模组"，而不是"我要核对文件"。
    return { tab: "search", view: null, loading: false, error: "", shard: "" };
  },
  computed: {
    cluster: {
      get() { return state.sel.cluster; },
      set(v) { state.sel.cluster = v; },
    },
    canWrite() { return roomWritable(this.cluster); },
    writeNote() { return readOnlyNote(this.cluster); },
    shards() {
      const c = state.clusters.find((x) => x.name === this.cluster);
      return c ? c.shards || [] : [];
    },
    mods() { return (this.view && this.view.mods) || []; },
    // 当前房间的世界名单。三个视图都要它：「订阅并开启」要知道写哪几个文件，
    // 而开关那一屏本来就在用它切 Master/Caves。
    worlds() { return this.shards.map((s) => s.name); },
    // 目录 -> 开着它的世界。磁盘上那批来自接口的 usedBy；还没下载的那批同样要带上，
    // 因为"订阅并开启"之后的正常状态就是"清单里有、文件还没下来、开关已经写好"。
    // 少了这一半，刚点完的那一枚按钮会在下一屏变回"在这个房间打开"，等于自相矛盾。
    openByDir() {
      const out = {};
      (this.view && this.view.mods || []).forEach((m) => { if (m.usedBy && m.usedBy.length) out[m.dir] = m.usedBy; });
      (this.view && this.view.pendingDownload || []).forEach((p) => {
        if (p.usedBy && p.usedBy.length) out["workshop-" + p.workshopId] = p.usedBy;
      });
      return out;
    },
    // 目录 -> 还没下载的那条工坊信息。开关那一屏也会画出这些行（订阅并开启之后
    // 文件要等世界重启才下来），那时磁盘上没有 modinfo，名字和封面只能来自这里。
    pendingByDir() {
      const out = {};
      ((this.view && this.view.pendingDownload) || []).forEach((p) => {
        out["workshop-" + p.workshopId] = p;
      });
      return out;
    },
    // 三个页签是三个**视图**，不是三道必点的步骤：一次「订阅并开启」把清单和开关都写完，
    // 另外两屏留着是为了核对与回退。标签因此按"这一屏回答哪个问题"起名。
    tabs() {
      return [
        { key: "search", step: 1, label: "找模组", count: "" },
        { key: "disk", step: 2, label: "这台机器上的文件", count: this.mods.length + " 个模组" },
        // 计数没跟上时不说"没选世界"：世界一存在就自动选中一个（见 shards 侦听和 mounted），
        // 所以空着只有两种原因——没有房间，或有房间但里面还没建世界。
        { key: "shard", step: 3, label: "这个房间的开关", count: this.shard || (this.cluster ? "没有世界" : "还没有房间") },
      ];
    },
  },
  watch: {
    cluster() {
      this.shard = this.shards.length ? this.shards[0].name : "";
      this.load();
    },
    shards(list) { if (!list.length) { this.shard = ""; return; } if (!this.shard) this.shard = list[0].name; },
  },
  mounted() {
    // 第 3 步那个"当前世界"必须在进来就有选中项。上面两个侦听都不是 immediate 的，
    // 而面板是 SPA：从别的页切进来时房间早就选好了，侦听一次也不会触发，
    // 于是这一栏停在一个"两个世界都在、一个都没选中"的空白卡片上（连空状态都没有，
    // 因为子组件的 load 在 shard 为空时直接返回，既不 loading 也不报错）。
    if (!this.shard && this.shards.length) this.shard = this.shards[0].name;
    this.load();
  },
  methods: {
    async load() {
      this.loading = true;
      this.error = "";
      const res = await api.get("/api/v1/mods");
      this.loading = false;
      if (!res.ok) { this.error = res.error || ("接口返回 " + res.status); this.view = null; return; }
      this.view = res.data;
    },
    fixAt(shard) {
      // 一致性那句"去某个世界改"要连着把人送到那个世界的开关旁边，
      // 只切 tab 的话他还得自己再点一次世界。
      if (shard) this.shard = shard;
      this.tab = "shard";
    },
    async go(t) {
      if (t === this.tab) return;
      // 第 3 步的表单活在本组件内存里，切走就卸组件。动线改成三步来回点之后，
      // "没保存就静默丢掉"从理论风险变成点两下就能踩到的坑，所以先问一句。
      const el = this.tab === "shard" ? this.$refs.shard : null;
      const n = el ? el.anyDirty() : 0;
      if (n) {
        const to = (this.tabs.find((x) => x.key === t) || {}).label || "别的一屏";
        const ok = await askConfirm({
          title: "切到" + to + "？",
          body: "这一屏上有 " + n + " 项配置改了还没保存。",
          warn: "切走会丢掉这些没保存的改动，面板不替人存草稿。要留着就先点\"留在这一步\"，去把它保存了再切。",
          okLabel: "丢掉改动，切过去",
          cancelLabel: "留在这一步",
        });
        if (!ok) return;
      }
      this.tab = t;
    },
  },
  template: `
    <div class="col" style="gap:16px">
      <!-- 没有房间时这一行只剩一个点开还是空的下拉框；模组这一页在没房间时
           照样能搜、能订阅、能上传，所以只收掉选择器，页面其余部分照旧。 -->
      <div class="row" style="gap:12px;flex-wrap:wrap;align-items:flex-end" v-if="state.clusters.length">
        <div class="field" style="min-width:180px;flex:1">
          <label>当前房间</label>
          <select v-model="cluster">
            <option value="" disabled>选一个房间</option>
            <option v-for="c in state.clusters" :key="c.name" :value="c.name">{{ c.name }}</option>
          </select>
        </div>
        <span class="chip" v-if="loading">读取中</span>
        <span class="chip" v-else-if="error" :title="error">模组清单没拿到</span>
        <span class="chip" v-else>{{ mods.length }} 个模组目录</span>
      </div>

      <!-- 「这台机器上的文件」那一栏就是把这份清单摆出来的地方，它自己会说读不到（同一个原因、
           同一句话），所以切到那一栏时页顶不再重复一遍。别的页签（找模组、这个房间的开关）
           不显示这份清单，那句原因只能靠这里说。 -->
      <x-note v-if="error && tab !== 'disk'" kind="bad">{{ error }}</x-note>
      <x-note v-if="!state.clusters.length" kind="info">
        这台机器上还没有房间。模组文件是全局的，但"开不开"要落到某个世界的 modoverrides.lua 上，
        所以先在<a class="linkish" href="#/room">开服与世界</a>建一个房间。
      </x-note>

      <div class="tabs">
        <button v-for="t in tabs" :key="t.key" :class="tab === t.key ? 'on' : ''" @click="go(t.key)">
          <span class="tab-step">{{ t.step }}</span>{{ t.label }}
          <span class="tab-count" v-if="t.count">{{ t.count }}</span>
        </button>
      </div>

      <x-workshop v-if="tab === 'search'" :cluster="cluster" :worlds="worlds" :open-by-dir="openByDir"
                  @reload="load" @tab="go"></x-workshop>
      <x-disk v-else-if="tab === 'disk'" :view="view" :loading="loading" :error="error"
              :cluster="cluster" :worlds="worlds" :open-by-dir="openByDir" @reload="load"></x-disk>
      <template v-else>
        <x-shard-mods ref="shard" :cluster="cluster" :shards="shards" :shard="shard"
                      :mods="mods" :pending-by-dir="pendingByDir"
                      @pick-shard="shard = $event"></x-shard-mods>
        <x-consistency compact :cluster="cluster" :shard="shard" @fix-shard="fixAt"></x-consistency>
      </template>
    </div>`,
};
