// 存档快照页：有哪些档、每份能不能拿来恢复、点恢复到底会发生什么。
//
// 这一页只有一条主线：恢复是"整棵换掉存档目录"的动作，所以每一个"能点"都必须是
// 后端对着**当前进程状态**判过的结论（见 internal/api/backup.go 里 shardRuns 那段注释）。
// 因此这里不去读总览那份状态缓存——那是五秒前的快照，用来显示人数够，
// 用来决定"现在能不能换存档目录"就是拿过时信息做一个不可逆的动作。
// 列表里的 restorable / restoreNote / runningNote 出自同一次请求，一起有效、一起过期。
//
// 备份和恢复都只是"入队"：接口回 202 + 任务 id，磁盘上多没多出东西要看 worker。
// 所以这一页不说"已备份完成"这种话，只做两件事：说清任务接住了，
// 以及任务从"还有在跑的"变成"全结束了"时自己刷新一次列表。
import * as api from "../api.js";
import { state, act, actMany, askConfirm, rel, isBusy, toast, loadTasks,
  roomWritable, readOnlyNote } from "../store.js";
import { bytes, num, dateTime } from "../fmt.js";

// 这一页自己提交的三类任务。只有它们没结束时才值得自动刷新列表。
// 导入也算：导入做完磁盘上多出的正是一份快照，列表不刷就看不见。
const OWN_KINDS = ["backup.create", "backup.restore", "backup.import"];
const OPEN_STATUS = ["queued", "running"];

function path(id) {
  // 编号是后端给的，但筛选与缓存都可能把它原样带回来，拼路径的每一个环节都当作不可信。
  return "/api/v1/backups/" + encodeURIComponent(id);
}

// 来源的中文标签一律由后端给（backup.LabelFor 对认不出的来源也会编一句）。
// 这里拿到空值只有那一种情况：清单根本没读出来，所以不能替它编一个"手工备份"。
function srcLab(s) {
  return s.labelLabel || "读不出来（清单没读到）";
}

// ---- 一份快照展开后的那一屏：清单、校验、下载、删除、恢复 ----

const Detail = {
  props: {
    s: { type: Object, required: true },          // 列表里的那一行
    d: { type: Object, required: true },          // 页面当前唯一展开的那份
    clusters: { type: Array, default: () => [] },
  },
  emits: ["reload", "verify", "download", "remove", "restore", "cancel-restore", "pick-target"],
  setup() { return { state, rel, bytes, num, dateTime, isBusy, srcLab, roomWritable, readOnlyNote }; },
  computed: {
    snap() { return (this.d.detail && this.d.detail.snapshot) || null; },
    files() { return (this.snap && this.snap.files) || []; },
    skipped() { return (this.snap && this.snap.skipped) || []; },
    rows() {
      const s = this.s;
      return [
        { k: "备份编号", v: s.id, mono: true },
        { k: "房间", v: s.cluster },
        { k: "来源", v: srcLab(s) },
        { k: "什么时候打的", v: dateTime(s.createdAt) + "（" + rel(s.createdAt) + "）" },
        { k: "归档大小", v: bytes(s.archiveBytes) },
        { k: "解压以后", v: bytes(s.uncompressedBytes) },
        { k: "登记的文件", v: num(s.fileCount) + " 个" },
        { k: "带存档的世界", v: (s.saveShards && s.saveShards.length) ? s.saveShards.join("、") : "没有带存档的世界" },
        { k: "打包时的面板版本", v: s.panelVersion },
      ].filter((r) => r.v !== "" && r.v !== undefined && r.v !== null);
    },
    // 每个分片的 save 目录多大：判断"这份档是不是那天玩到很晚的那一份"经常靠这个。
    shardSizes() {
      const m = (this.snap && this.snap.shardSaveBytes) || {};
      return Object.keys(m).sort().map((k) => ({ k, v: bytes(m[k]) }));
    },
    targetOptions() {
      // 目标房间只能从后端这次给出的名单里选，界面不自己拼房间名。
      const list = this.clusters.slice();
      if (list.indexOf(this.s.cluster) < 0) list.unshift(this.s.cluster);
      return list;
    },
    // 一份快照归它原来那间房管：成员只对自己名下的房间能删、能恢复，
    // 不能因为"不是管理员"就把整页写按钮灰掉。
    canWrite() { return roomWritable(this.s.cluster); },
    writeNote() { return readOnlyNote(this.s.cluster) || "这个账号没有修改这间房的权限"; },
  },
  methods: {
    // 校验：后端同步跑（读盘 + 逐文件比哈希），几十 MB 的包几十秒，所以按钮必须自己报进度。
    verify() { this.$emit("verify", this.s); },
    download() { this.$emit("download", this.s); },
    remove() { this.$emit("remove", this.s); },
    ask() { this.$emit("restore", this.s); },
    run() { this.$emit("restore-run", { s: this.s, target: this.d.target }); },
  },
  template: `
    <div class="col" style="gap:10px">
      <div class="drawer-head">
        <b>{{ s.cluster }}</b>
        <span class="muted">{{ srcLab(s) }} · {{ dateTime(s.createdAt) }}</span>
        <span class="grow"></span>
        <x-badge v-if="s.restorable" kind="ok" text="清单完整、归档在、带存档"></x-badge>
        <x-badge v-else kind="bad" text="不能用来恢复"></x-badge>
      </div>

      <x-note kind="bad" v-if="s.restoreNote">{{ s.restoreNote }}</x-note>
      <x-note kind="warn" v-else-if="s.runningNote">{{ s.runningNote }}</x-note>

      <x-sk v-if="d.loading" :lines="3"></x-sk>
      <div class="err-box" v-else-if="d.error">{{ d.error }}
        <div class="row mt" style="gap:8px">
          <button class="btn sm ghost" @click="$emit('reload')">重试读清单</button>
        </div>
      </div>

      <template v-else>
        <x-kv :rows="rows"></x-kv>
        <div class="row mod-meta" v-if="shardSizes.length">
          <span class="chip mono" v-for="x in shardSizes" :key="x.k">{{ x.k }} 的存档 {{ x.v }}</span>
        </div>
        <x-note kind="info" v-if="d.detail && d.detail.note">{{ d.detail.note }}</x-note>

        <!-- 校验结果紧跟在按钮下面：那是一句"这份能不能拿来恢复"的结论，
             弹成 Toast 三秒就没了，而用户接下来要做的是滚动到恢复那一栏。 -->
        <div class="col" style="gap:6px" v-if="d.verify">
          <div class="row" style="gap:8px">
            <x-badge :kind="d.verify.archiveOk ? 'ok' : 'bad'"
                     :text="d.verify.archiveOk ? '校验通过' : '校验不通过'"></x-badge>
            <span class="muted">逐文件比过 {{ d.verify.checked }} 个</span>
            <span class="dim">{{ rel(d.verify.at) }}</span>
          </div>
          <div class="hint">{{ d.verify.detail }}</div>
          <div class="hint" v-if="d.verify.note">{{ d.verify.note }}</div>
          <ul class="list-plain" v-if="!d.verify.archiveOk">
            <li v-if="d.verify.missing && d.verify.missing.length">
              <b>清单登记了、包里找不到：</b><span class="mono">{{ d.verify.missing.join('、') }}</span></li>
            <li v-if="d.verify.mismatched && d.verify.mismatched.length">
              <b>内容和登记的哈希不一样：</b><span class="mono">{{ d.verify.mismatched.join('、') }}</span></li>
            <li v-if="d.verify.unexpected && d.verify.unexpected.length">
              <b>包里有、清单没登记：</b><span class="mono">{{ d.verify.unexpected.join('、') }}</span></li>
          </ul>
        </div>

        <div class="row" style="gap:8px">
          <button class="btn sm" :disabled="isBusy('backup.verify:' + s.id)" @click="verify">
            {{ isBusy('backup.verify:' + s.id) ? '正在整包读…' : '校验这份归档' }}</button>
          <button class="btn sm ghost" :disabled="isBusy('backup.download:' + s.id)" @click="download">
            {{ isBusy('backup.download:' + s.id) ? '正在取文件…' : '下载归档' }}</button>
          <button class="btn sm danger" :disabled="!canWrite || isBusy('backup.delete:' + s.id)" :title="canWrite ? '' : writeNote" @click="remove">删除备份</button>
        </div>

        <details class="group">
          <summary class="g-head">
            <span class="g-chev">›</span>
            <b>包里的文件清单</b>
            <span class="g-count">{{ files.length }} 项</span>
            <span class="grow"></span>
            <span class="g-count">跳过 {{ skipped.length }} 项</span>
          </summary>
          <div class="col" style="padding:10px;gap:8px">
            <x-note kind="info" v-if="skipped.length">
              打这份包时刻意跳过了 {{ skipped.length }} 项，每一项后面写着原因。
              最常见的是 Klei 令牌和面板的命令通道文件——备份是可以下载的，那两样不能跟着走。
            </x-note>
            <div class="file-tree">
              <div class="ft" v-for="f in files" :key="f.path">
                <span>{{ f.path }}</span><span class="why">{{ bytes(f.size) }}</span>
              </div>
              <div class="ft" v-for="(why, i) in skipped" :key="'sk' + i">
                <span>{{ why }}</span><span class="why">跳过</span>
              </div>
            </div>
          </div>
        </details>

        <!-- 恢复：目标房间要先选再确认，所以它不能只靠一个"你确定吗"的对话框。 -->
        <div class="drawer" v-if="d.restoring">
          <div class="col" style="gap:8px">
            <div class="drawer-head"><b>恢复到哪个房间</b></div>
            <div class="grid-form">
              <div class="field">
                <label>目标房间</label>
                <select :value="d.target" @change="$emit('pick-target', $event.target.value)">
                  <option v-for="c in targetOptions" :key="c" :value="c">
                    {{ c }}{{ c === s.cluster ? '（这份备份原来的房间）' : '（跨房间迁移）' }}</option>
                </select>
                <span class="hint" v-if="d.target === s.cluster">
                  换掉 {{ s.cluster }} 现在的存档目录。房间配置（cluster.ini 和两个世界的 server.ini）不动。</span>
                <span class="hint warn" v-else>
                  {{ d.target }} 现在那个世界会被这份备份整个替换掉，替换完就回不去原来那个世界了。</span>
              </div>
            </div>
            <x-note kind="warn">
              队列里的执行顺序是：先整包校验这份备份 → 自动打一份「恢复前的安全备份」→
              换掉存档目录 → 再校验一遍，中间任何一步失败就把原存档换回去。
            </x-note>
            <div class="row" style="gap:8px">
              <button class="btn ghost sm" @click="$emit('cancel-restore')">先不恢复</button>
              <span class="grow"></span>
              <button class="btn danger sm" :disabled="!canWrite || isBusy('backup.restore:' + s.id)" :title="canWrite ? '' : writeNote" @click="run">
                {{ isBusy('backup.restore:' + s.id) ? '排队中…' : '确认恢复' }}</button>
            </div>
          </div>
        </div>
        <div class="row" style="gap:8px" v-else>
          <button class="btn danger sm" :disabled="!canWrite || !s.restorable || !!s.runningNote" :title="canWrite ? '' : writeNote" @click="ask">
            用这份备份恢复…</button>
          <span class="hint" v-if="!s.restorable">这份不能用来恢复，原因见上面那句。</span>
          <span class="hint" v-else-if="s.runningNote">先把这个房间的世界全停掉，这一栏才会放开。</span>
        </div>
      </template>
    </div>`,
};

// ---- 页面 ----

export default {
  name: "Snapshots",
  components: { "x-detail": Detail },
  setup() { return { state, rel, bytes, num, dateTime, isBusy, srcLab, roomWritable, readOnlyNote }; },
  data() {
    return {
      view: null, loading: false, error: "",
      filter: "",                 // 房间筛选，空 = 全部房间
      // 导入那条动线：选文件 → 读包看结构 → 提交。三段的状态各留一份，
      // 因为"读出结构"要几十秒到几分钟（几十 MB 的上传），中途不能把已选的文件抹掉。
      imp: {
        open: false, drag: false, file: null, fileName: "", sizeLabel: "",
        overSize: false, busy: false, sending: false, err: "",
        preview: null, cluster: "", restore: false,
      },
      // 展开的面板只有一份：同屏摊开两份文件清单，就没人分得清"我要恢复的是哪天那个档"。
      d: { id: "", loading: false, error: "", detail: null, verify: null, restoring: false, target: "" },
      // 这一页自己排队出去的任务编号。列表要在它们结束的那一刻刷一次。
      waiting: [],
      // 批量删除的多选：按备份编号记；刷新后不在列表里的编号自然被 selectedSnaps 过滤掉。
      sel: {},
    };
  },
  computed: {
    snaps() { return (this.view && this.view.snapshots) || []; },
    selectedSnaps() { return this.snaps.filter((x) => this.sel[x.id]); },
    // 批量删除要每一份都属于我管得着的房间，才不算"能对别人的存档下手"。
    selectedWritable() { return this.selectedSnaps.every((s) => roomWritable(s.cluster)); },
    policy() { return (this.view && this.view.policy) || {}; },
    blocked() { return (this.view && this.view.restoreBlocked) || []; },
    clusters() { return (this.view && this.view.clusters) || []; },
    newest() { return this.snaps.length ? this.snaps[0] : null; },
    openTasks() {
      return state.tasks.filter((t) => OWN_KINDS.indexOf(t.kind) >= 0 && OPEN_STATUS.indexOf(t.status) >= 0);
    },
    // 导入这件事的参数（上限、字段名、两条 URL、临时上传占了多少）全出自后端那一份，
    // 这里抄一个字段名就少一处会和接口跑偏的地方。
    ip() { return (this.view && this.view.import) || {}; },
    impRows() {
      const p = this.imp.preview;
      if (!p) return [];
      return [
        { k: "选出的文件", v: p.sourceName || p.package },
        { k: "认出的格式", v: p.kindLabel },
        { k: "包的大小", v: p.archiveLabel },
        { k: "解开以后", v: p.uncompressedLabel },
        { k: "包里的条目", v: num(p.entryCount) + " 条，其中文件 " + num(p.fileCount) + " 个" },
        {
          k: "包里的房间目录",
          v: p.root ? ("「" + p.root + "」，导入后登记给 " + this.imp.cluster) : "包的最外层就是世界目录",
        },
        { k: "上传包编号", v: p.packageRef, mono: true },
      ].filter((r) => r.v !== "" && r.v !== undefined && r.v !== null);
    },
    // 勾了"导入后就换进这个房间"时，那个房间在不在跑必须由这一页当场说出来：
    // 后端会拒掉这次提交，而拒在提交之后不如写在勾选的那一行下面。
    impTargetRunning() {
      const hit = (this.blocked || []).filter((b) => b.cluster === this.imp.cluster)[0];
      return hit ? hit.note : "";
    },
    // 等自己排出去的那几个任务落地。要盯编号而不是盯"还有没有在跑的任务"：
    // 快任务会在两次五秒轮询之间从排队直接变成成功，那一轮读到的已经是终态，
    // "从有到无"根本没成立过，列表就停在提交那一刻的样子——用户以为导入没生效。
    settledWaiting() {
      if (!this.waiting.length) return [];
      const seen = {};
      (state.tasks || []).forEach((t) => { seen[t.id] = t.status; });
      // 只在"这条任务确实被读到过、而且已经不是排队/执行"时才算落地：
      // 名单里根本没它的时候说明这一轮拿到的还是旧列表，不能当结束。
      return this.waiting.filter((id) => seen[id] !== undefined && OPEN_STATUS.indexOf(seen[id]) < 0);
    },
    // 页面级开关：管理员，或者名下至少有一间房可写，才显示可写态。
    // 具体到某个房间/某份快照的按钮，模板里另用 roomWritable(c) 逐个判，
    // 免得"我管 A 房"的人能对 B 房的备份点下删除。
    canWrite() {
      const me = state.me;
      return !!(me && (me.isAdmin || (state.clusters || []).some((c) => c.ownedByMe)));
    },
    writeNote() { return "这个账号是只读：能看状态，但修改会被拒（改自己的密码除外）"; },
  },
  watch: {
    filter() { this.close(); this.load(); },
    // 任务从"还有在跑的"变成"全结束了"：磁盘上的东西这会儿才刚变完，刷一次列表。
    openTasks(now, prev) { if (prev && prev.length && now && !now.length) this.load(); },
    settledWaiting(ids) {
      if (!ids || !ids.length) return;
      this.waiting = this.waiting.filter((id) => ids.indexOf(id) < 0);
      this.load();
    },
  },
  mounted() { this.load(); },
  methods: {
    async load() {
      this.loading = true;
      this.error = "";
      const res = await api.get("/api/v1/backups" + api.qs({ cluster: this.filter }));
      this.loading = false;
      if (!res.ok) { this.error = res.error || ("接口返回 " + res.status); this.view = null; return; }
      this.view = res.data;
      // 筛完那一份已经不在列表里了就把面板收掉，不然屏幕上留着的是别的房间的清单。
      if (this.d.id && !this.snaps.some((s) => s.id === this.d.id)) this.close();
    },
    // 刚传上来的包正占着备份目录，把"临时上传几份、占多少"跟着补一次，
    // 否则入口那一排还是页面最初加载时的数字，用户看不到自己刚占了多少盘。
    // 这里不直接复用 load()：那条路失败会把 view 抹成空，抽屉会连着刚读出来的结论一起消失。
    async refreshPolicy() {
      const res = await api.get("/api/v1/backups" + api.qs({ cluster: this.filter }));
      if (res.ok) this.view = res.data;
    },
    // 写动作接住的是任务编号，不是结果：把编号记下来，它结束的那一轮才刷列表。
    // 顺手补一次 loadTasks()，因为这三条提交走的都是 clusters:false，
    // 不补的话手上那份任务名单里还没有这条新任务，"落地"就永远等不到。
    track(res) {
      const id = res && res.data && res.data.taskId;
      if (!id) return;
      if (this.waiting.indexOf(id) < 0) this.waiting.push(id);
      loadTasks();
    },
    close() {
      this.d = { id: "", loading: false, error: "", detail: null, verify: null, restoring: false, target: "" };
    },
    toggle(s) {
      if (this.d.id === s.id) { this.close(); return; }
      this.openFor(s);
    },
    // 行尾那个「恢复」按钮要先展开：直接弹确认框的话，用户点的是一份他还没看过清单的档。
    restoreFrom(s) {
      if (this.d.id !== s.id) this.openFor(s);
      this.d.restoring = true;
      if (!this.d.target || this.d.target === "") this.d.target = s.cluster;
    },
    openFor(s) {
      this.d = { id: s.id, loading: true, error: "", detail: null, verify: null, restoring: false, target: s.cluster };
      this.loadDetail(s.id);
    },
    async loadDetail(id) {
      const res = await api.get(path(id));
      if (this.d.id !== id) return;      // 迟到响应不能盖到已经切走的那一份下面
      this.d.loading = false;
      if (!res.ok) { this.d.error = res.error || ("接口返回 " + res.status); return; }
      this.d.detail = res.data;
    },
    reloadDetail() {
      if (!this.d.id) return;
      this.d.loading = true;
      this.d.error = "";
      this.loadDetail(this.d.id);
    },
    shardsOf(s) {
      return (s.saveShards && s.saveShards.length) ? s.saveShards.join("、") : "没有带存档的世界";
    },
    level(s) { return !s.restorable ? "bad" : (s.runningNote ? "warn" : "ok"); },
    verdict(s) {
      if (!s.restorable) return "不能用来恢复";
      return s.runningNote ? "能恢复，但现在不能点" : "可以用来恢复";
    },

    // ---- 导入外部存档 ----

    toggleImport() {
      this.imp.open = !this.imp.open;
      if (this.imp.open && !this.imp.cluster) {
        // 默认跟着上面的筛选走：先筛到某个房间再想给它导档，是这一页最常见的顺序。
        this.imp.cluster = this.filter || this.clusters[0] || "";
      }
    },
    chooseSave() { this.$refs.saveFile.click(); },
    pickSave(e) {
      const f = (e.target.files || [])[0];
      if (f) this.setSaveFile(f);
      e.target.value = "";        // 清掉值，同一个文件重新选一次也还能触发 change
    },
    dropSave(e) {
      this.imp.drag = false;
      const f = ((e.dataTransfer && e.dataTransfer.files) || [])[0];
      if (f) this.setSaveFile(f);
    },
    setSaveFile(f) {
      this.imp.file = f;
      this.imp.fileName = f.name;
      this.imp.sizeLabel = bytes(f.size);
      // 上限在前头先挡一句：几十 MB 传到一半才被后端拒掉，用户看到的是浏览器进度条断掉，
      // 而这里这句是点下去之前就能知道的。
      this.imp.overSize = !!(this.ip.maxArchiveBytes && f.size > this.ip.maxArchiveBytes);
      this.imp.preview = null;
      this.imp.err = "";
    },
    clearSaveFile() {
      this.imp.file = null; this.imp.fileName = ""; this.imp.sizeLabel = "";
      this.imp.overSize = false; this.imp.preview = null; this.imp.err = "";
      this.imp.restore = false;
    },
    // 读包：上传 + 后端只读包目录。这一步既不动房间也不打快照，
    // 所以"能不能导入"是这里唯一要回答的问题，错了就重写那一句，不用刷新整页。
    async readPackage() {
      if (!this.imp.file) return;
      if (!this.ip.previewPath) {
        this.imp.err = "页面还没读到导入的参数，先点上面的刷新";
        return;
      }
      this.imp.busy = true;
      this.imp.err = "";
      this.imp.preview = null;
      const fd = new FormData();
      fd.append(this.ip.uploadField, this.imp.file, this.imp.file.name);
      const res = await api.request("POST", this.ip.previewPath, fd);
      this.imp.busy = false;
      if (!res.ok) { this.imp.err = res.error || ("接口返回 " + res.status); return; }
      this.imp.preview = res.data;
      if (!res.data.canImport) this.imp.restore = false;   // 不能导的包不该留着勾"换进房间"
      this.refreshPolicy();
    },
    async submitImport() {
      const p = this.imp.preview;
      if (!p || !p.canImport || this.imp.sending) return;
      if (!this.imp.cluster) { this.imp.err = "要先选导入给哪个房间"; return; }
      if (this.imp.restore) {
        const ok = await askConfirm({
          title: "导入完就换掉「" + this.imp.cluster + "」的存档？",
          body: "这份包里有 " + (p.shards.length || 0) + " 个世界的存档（" +
            p.shards.map((w) => w.shard + "，世界编号 " + w.sessionId).join("、") + "）。" +
            "面板会先把它打成一份快照，再整棵替换 " + this.imp.cluster + " 的存档目录。",
          warn: this.impTargetRunning
            ? "后端会拒掉这一次：" + this.impTargetRunning
            : "动手前面板会先自动打一份恢复前的安全备份；房间配置不会被覆盖，端口和服务器名保持现状。",
          danger: true, okLabel: "排队执行导入",
          details: ["包里的 Klei 令牌、面板命令通道文件和引擎日志都不会进快照——" +
            "这个房间的令牌还是本机那一份。"],
        });
        if (!ok) return;
      }
      this.imp.sending = true;
      const res = await act("backup.import:" + this.imp.cluster, "POST", this.ip.importPath,
        { package: p.packageRef, cluster: this.imp.cluster, restore: this.imp.restore, confirm: this.imp.restore },
        {
          okTitle: "导入任务已接住", okBody: (x) => (x && x.note) || "",
          badTitle: "导入没排进队列", clusters: false,
        });
      this.imp.sending = false;
      if (res.ok) {
        // 文件与预览都收掉：这份上传包的使命已经交给任务了，留着会让用户以为还要再点一次提交。
        this.clearSaveFile();
        this.imp.open = false;
        this.track(res);
        this.load();
      }
    },

    async backup(cluster) {
      const res = await act("backup.create:" + cluster, "POST",
        "/api/v1/clusters/" + encodeURIComponent(cluster) + "/backups", { cluster },
        { okTitle: "备份任务已接住", okBody: (x) => (x && x.note) || "", badTitle: "备份没排进队列", clusters: false });
      if (res.ok) { this.track(res); this.load(); }
    },
    async verifyNow(s) {
      const res = await act("backup.verify:" + s.id, "POST", path(s.id) + "/verify", undefined,
        { quiet: true, badTitle: "校验没跑成", clusters: false });
      if (res.ok) this.d.verify = res.data;
    },
    async download(s) {
      // 下载不是 JSON 接口，走 api.download（它自己带认证头、自己造 <a download>），
      // 所以 busy 这一格要手动占住：按钮得在几十 MB 取回来期间显示"正在取文件"，
      // 不然用户只会以为这一下没按着，连着点五次。
      const key = "backup.download:" + s.id;
      state.busy[key] = true;
      let got;
      try {
        got = await api.download(s.downloadPath, s.id + ".tar.gz");
      } finally {
        delete state.busy[key];
      }
      if (!got.ok) { toast("bad", "归档没拿到", got.error); return; }
      toast("ok", "归档已开始下载", s.id + ".tar.gz，" + bytes(got.size) + "。包可以直接解开看；" +
        "但面板只认自己清单里的那一份，解开改过之后再放回去就不一定校验得过。");
    },
    async remove(s) {
      const ok = await askConfirm({
        title: "删除这份备份？",
        body: "会删掉 " + s.id + " 的归档和清单：房间 " + s.cluster + "，" +
          srcLab(s) + "，" + dateTime(s.createdAt) + "，" + bytes(s.archiveBytes) + "。",
        warn: "磁盘上少 " + bytes(s.archiveBytes) + "。要是这是这个房间最近的一份，往后就只能退回更早的备份了。",
        danger: true, okLabel: "删除这份备份",
        details: s.restorable ? [] : ["这份本来也不能用来恢复：" + (s.restoreNote || "原因见列表那一行")],
      });
      if (!ok) return;
      await act("backup.delete:" + s.id, "DELETE", path(s.id), undefined,
        { okTitle: "备份已删除", okBody: (x) => (x && x.note) || "", badTitle: "删除没成", clusters: false });
      this.close();
      this.load();
    },
    toggleSel(id) { this.sel = { ...this.sel, [id]: !this.sel[id] }; },
    selectAllSnaps() {
      const all = {};
      this.snaps.forEach((x) => { all[x.id] = true; });
      this.sel = all;
    },
    // 删备份不可逆：确认框写清后果，界面不给假的撤销。整批一个 busy 键、串行下发、结果汇总。
    async removeSelected() {
      const list = this.selectedSnaps;
      if (!list.length) return;
      const total = list.reduce((n, x) => n + (Number(x.archiveBytes) || 0), 0);
      const ok = await askConfirm({
        title: "删除选中的 " + list.length + " 份备份？",
        body: "会删掉这些备份的归档和清单：" + list.map((x) => x.id).join("、") + "。",
        warn: "磁盘上会少掉约 " + bytes(total) + "，删掉就找不回来了：往后只能退回更早的备份。这一步没有撤销。",
        danger: true, okLabel: "删除这 " + list.length + " 份",
        details: ["这是不可逆操作，界面不会给假的撤销；请确认这些归档都不再需要。"],
      });
      if (!ok) return;
      const items = list.map((x) => ({ method: "DELETE", path: path(x.id), label: x.id }));
      const res = await actMany("backup.bulk.delete", items,
        { okTitle: "批量删除完成", badTitle: "批量删除有没成的", clusters: false, after: false });
      if (res.ok) this.sel = {};
      this.close();
      this.load();
    },
    async runRestore(payload) {
      const s = payload.s;
      const to = String(payload.target || s.cluster).trim();
      const cross = to !== s.cluster;
      const ok = await askConfirm({
        title: cross ? "把这份档恢复到「" + to + "」？" : "换掉 " + to + " 现在的存档？",
        body: "目标房间 " + to + " 的存档目录会被整棵替换（备份来自 " + s.cluster +
          "，带存档的世界：" + this.shardsOf(s) + "）。房间配置不会被覆盖，端口和服务器名保持现状。",
        warn: cross
          ? "这是跨房间迁移：" + to + " 现在那个世界会被这份备份替换掉，替换完就回不去原来那个世界了（除非再恢复一次）。"
          : "面板会先整包校验、再自动打一份恢复前的安全备份才动手；中途失败会把原存档换回去。",
        danger: true, okLabel: "排队执行恢复",
        details: s.runningShards && s.runningShards.length
          ? ["这个房间还在跑：" + s.runningShards.join("、") + "。后端会拒掉这次提交，要先把世界全停掉。"]
          : [],
      });
      if (!ok) return;
      const res = await act("backup.restore:" + s.id, "POST", s.restorePath,
        { targetCluster: cross ? to : "", confirm: true },
        { okTitle: "恢复任务已接住", okBody: (x) => (x && x.note) || "", badTitle: "恢复没排进队列", clusters: true });
      if (res.ok) { this.d.restoring = false; this.track(res); }
    },
  },
  template: `
    <div class="col snap-page" style="gap:16px">
      <div class="row" style="gap:12px;flex-wrap:wrap;align-items:flex-end">
        <!-- 一间都没有时这个下拉里只有"全部房间"一个选项：点开是空的，等于让人试一次。 -->
        <div class="field" style="min-width:170px;flex:1" v-if="clusters.length">
          <label>只看某个房间</label>
          <select v-model="filter">
            <option value="">全部房间</option>
            <option v-for="c in clusters" :key="c" :value="c">{{ c }}</option>
          </select>
        </div>
        <button class="btn ghost sm" @click="load" :disabled="loading">{{ loading ? '读取中' : '刷新' }}</button>
        <span class="chip" v-if="newest && newest.createdAt">最近一份：{{ rel(newest.createdAt) }}</span>
        <span class="chip" v-else-if="newest">最新那份的清单读不出来，时间也就没处取</span>
        <span class="chip" v-else-if="!loading && !error && view && clusters.length">还没有备份</span>
        <!-- 后端那句"目录在哪、用了多少、还剩多少、按什么规则留"原来摊成三行蓝底说明：
             这四个数页顶四张卡各说了一遍，这里收成一枚把手，点开才是整句。 -->
        <x-hint v-if="view && view.note" label="目录与保留规则" :text="view.note"></x-hint>
      </div>

      <x-sk v-if="loading && !view" :lines="4"></x-sk>
      <div class="err-box" v-else-if="error">{{ error }}</div>

      <template v-else-if="view">
        <!-- 目录用量、磁盘余量、保留上限、定时与在跑的任务：这四块合起来回答
             "备份这套机制还在不在正常工作"，而不是只回答"有没有文件"。
             手机上四张卡要 433px，正好把"有哪些档"挤出第一屏，所以换成一条横向芯片带；
             未结束任务的计数顶栏也报了一次。 -->
        <div class="row snap-strip only-sm" style="gap:6px">
          <span class="chip mono">目录 {{ policy.usedLabel || '0 B' }}</span>
          <span class="chip mono">磁盘还剩 {{ view.freeLabel || '未知' }}</span>
          <span class="chip">最多留 {{ policy.keep }} 份</span>
          <span class="chip">定时 {{ view.scheduleHours > 0 ? ('每 ' + view.scheduleHours + ' 小时') : '没开' }}</span>
          <span class="chip" v-if="openTasks.length">{{ openTasks.length }} 个任务没结束</span>
        </div>
        <div class="grid grid-2 hide-sm">
          <x-stat label="备份目录已用" icon="archive" tint="tint-gold" :value="policy.usedLabel || '0 B'"
                  :hint="'这些归档放在：' + (policy.dir || '还没读出来')">
            <span class="mono ellipsis">{{ policy.dir }}</span>
          </x-stat>
          <x-stat label="这块磁盘还剩" icon="servers" tint="tint-teal" :value="view.freeLabel || '未知'"
                  :hint="'保留上限是 ' + policy.keep + ' 份（' + (policy.keepSource || '默认值') + '）。到数以后删最早的那份。'">
            最多留 {{ policy.keep }} 份
          </x-stat>
          <x-stat label="定时备份" icon="clock" tint="tint-cyan" :value="view.scheduleHours > 0 ? ('每 ' + view.scheduleHours + ' 小时') : '没开'"
                  :hint="view.scheduleRunning
                    ? '到点给每个有存档的房间各打一份，超出保留上限就删最早的。'
                    : '现在只在这一页手工打。要开就去面板设置把间隔改成大于 0，改完立刻生效、不用重启。'">
            {{ view.scheduleRunning ? '到点自动打' : '只手工打' }}
          </x-stat>
          <x-stat label="这一页的任务" icon="list" :value="openTasks.length ? (openTasks.length + ' 个没结束') : '没有'"
                  tone="info"
                  hint="备份与恢复都是排进队列的活，这一栏跟着总览的轮询走，面板重启也不丢。">
            <template v-if="openTasks.length">
              <span v-for="t in openTasks.slice(0, 2)" :key="t.id">{{ t.title }}：{{ t.statusLab }}</span>
            </template>
            <template v-else>排队与执行中的都会显示在这</template>
          </x-stat>
        </div>

        <x-note kind="warn" v-if="blocked.length">
          <div class="row" style="gap:8px;flex-wrap:wrap;align-items:center">
            <x-icon name="warn" :size="15"></x-icon>
            <b>{{ blocked.length }} 个房间在跑，这些备份这会儿恢复会被拒</b>
            <x-hint label="哪些世界" :text="blocked.map(b => b.note).join('；')"></x-hint>
            <span class="grow"></span>
            <a class="btn sm" href="#/">去停世界</a>
          </div>
        </x-note>

        <!-- 两排动作在没有房间时整排收掉：备份要指定房间、导入也要指定房间，
             留一个灰按钮加一句"还没有房间"，不如把这一句一次说在下面的空状态里
             （冷启动实测那一屏把同一句意思说过三遍）。 -->
        <div class="row snap-actions" style="gap:8px" v-if="clusters.length">
          <span class="card-title-sm">立即备份</span>
          <button class="btn sm" v-for="c in clusters" :key="c"
                  :disabled="!roomWritable(c) || isBusy('backup.create:' + c)" :title="roomWritable(c) ? '' : (readOnlyNote(c) || writeNote)" @click="backup(c)">
            {{ isBusy('backup.create:' + c) ? '排队中…' : ('备份 ' + c) }}</button>
        </div>

        <!-- 导入外部存档：三段动线（选出文件 → 读出包里的内容 → 提交给哪个房间）。
             前两段只往备份目录的临时区放一个包、读它的名单，房间目录一下都不碰；
             所以"这个包能不能用"必须在提交之前就说清——几十 MB 传完才发现包不对，是这条动线最贵的一种错。 -->
        <div class="row snap-actions" style="gap:8px" v-if="clusters.length">
          <span class="card-title-sm">外部存档</span>
          <button class="btn sm" @click="toggleImport">
            {{ imp.open ? '收起导入' : '导入外部存档' }}</button>
          <span class="chip mono" v-if="ip.stagedCount">临时上传 {{ ip.stagedCount }} 份 · {{ ip.stagedLabel }}</span>
          <x-hint v-if="ip.note" label="导入这条动线" :text="ip.note"></x-hint>
        </div>

        <div class="drawer snap-import" v-if="imp.open && clusters.length">
          <div class="col" style="gap:10px">
            <div class="drawer-head row spread">
              <b>把别人给的存档包变成这个面板里的快照</b>
              <span class="chip">单个包最大 {{ ip.maxArchiveLabel || '未知' }}</span>
            </div>

            <div class="drop" :class="imp.drag ? 'over' : (imp.fileName ? 'has' : '')" tabindex="0" role="button"
                 @click="chooseSave" @keydown.enter.prevent="chooseSave" @keydown.space.prevent="chooseSave"
                 @dragenter.prevent="imp.drag = true" @dragover.prevent="imp.drag = true"
                 @dragleave.prevent="imp.drag = false" @drop.prevent="dropSave($event)">
              <x-icon name="upload" :size="18"></x-icon>
              <div class="drop-t">{{ imp.fileName || '把存档包拖到这里' }}</div>
              <div class="drop-s">{{ imp.fileName
                    ? ('已选出 ' + imp.sizeLabel + '，要换包再点这一片')
                    : ('点这里选出 .zip、.tar.gz 或 .tar（连房间目录整个压一个包最稳）') }}</div>
            </div>
            <input class="drop-file" type="file" ref="saveFile" accept=".zip,.tar,.tar.gz" @change="pickSave">

            <div class="err-box" v-if="imp.overSize">
              这个文件 {{ imp.sizeLabel }}，已经超过面板 {{ ip.maxArchiveLabel }} 的上限，传上去也是白等：
              先只压要的那个世界，或者把包里不属于存档的东西（截图、模组解压出来的目录）剔掉。
            </div>

            <div class="row" style="gap:8px;flex-wrap:wrap">
              <button class="btn primary sm" :disabled="!canWrite || !imp.fileName || imp.busy || imp.overSize" :title="canWrite ? '' : writeNote" @click="readPackage">
                {{ imp.busy ? '正在读包……' : '读出包里的内容' }}</button>
              <button class="btn ghost sm" v-if="imp.fileName" @click="clearSaveFile">换一份文件</button>
              <span class="grow"></span>
              <span class="hint" v-if="!imp.preview && !imp.busy && !imp.overSize && imp.fileName">
                这一步只读包里的名单，既不占房间也不打快照。</span>
            </div>
            <x-sk v-if="imp.busy" :lines="2"></x-sk>
            <div class="err-box" v-if="imp.err">{{ imp.err }}</div>

            <template v-if="imp.preview">
              <x-note :kind="imp.preview.canImport ? 'ok' : 'bad'" class="imp-verdict">{{ imp.preview.note }}</x-note>
              <x-kv :rows="impRows"></x-kv>

              <div class="row mod-meta" style="flex-wrap:wrap" v-if="imp.preview.shards.length">
                <span class="chip" v-for="w in imp.preview.shards" :key="w.shard">
                  {{ w.shard }}：世界编号 <span class="mono">{{ w.sessionId }}</span> ·
                  {{ num(w.fileCount) }} 个文件 · {{ bytes(w.bytes) }}</span>
              </div>
              <ul class="list-plain" v-if="imp.preview.problems.length">
                <li v-for="(x, i) in imp.preview.problems" :key="'pb' + i" class="muted">{{ x }}</li>
              </ul>
              <x-note kind="warn" class="imp-warn" v-if="imp.preview.warnings.length">
                <ul>
                  <li v-for="(x, i) in imp.preview.warnings" :key="'w' + i">{{ x }}</li>
                </ul>
              </x-note>

              <details class="group" v-if="imp.preview.dropped.length">
                <summary class="g-head">
                  <span class="g-chev">›</span>
                  <b>这些内容不进快照</b>
                  <span class="g-count">{{ imp.preview.dropped.length }} 项</span>
                </summary>
                <div class="col" style="padding:10px;gap:8px">
                  <x-note kind="info">
                    最常见的是 Klei 令牌、面板的命令通道文件和引擎日志。快照是可以下载的，
                    那几样不能跟着走；这个房间用的还是它自己原来那份令牌。
                  </x-note>
                  <div class="file-tree">
                    <div class="ft" v-for="(d, i) in imp.preview.dropped" :key="'d' + i">
                      <span>{{ d }}</span><span class="why">丢掉</span>
                    </div>
                  </div>
                </div>
              </details>

              <div class="hint imp-shape">{{ imp.preview.shapeNote }}</div>

              <template v-if="imp.preview.canImport">
                <div class="grid-form">
                  <div class="field">
                    <label>导入给哪个房间</label>
                    <select v-model="imp.cluster">
                      <option v-for="c in clusters" :key="c" :value="c">{{ c }}</option>
                    </select>
                    <span class="hint">包里那个房间叫什么不影响这一步：做完的快照登记给你选的这一个，
                      房间配置（cluster.ini 和各个世界的 server.ini）保持本机这一份，端口和服务器名不会跟着包走。</span>
                  </div>
                </div>
                <label class="switch">
                  <input type="checkbox" v-model="imp.restore"><span class="track"></span>导入后就换进这个房间</label>

                <!-- 在跑的世界先把这一步挡在勾选处，而不是等提交回来一句 409：
                     后端那道检查还在（这里只是提前说），因为进程状态是这一秒的事。 -->
                <x-note kind="warn" class="imp-blocked" v-if="imp.restore && impTargetRunning">
                  {{ impTargetRunning }}。这一条先拒掉：要换存档得去总览把那个房间的世界全停掉。</x-note>

                <div class="row" style="gap:8px;flex-wrap:wrap">
                  <span class="grow"></span>
                  <button class="btn danger sm" v-if="imp.restore"
                          :disabled="!roomWritable(imp.cluster || filter) || imp.sending || !!impTargetRunning" :title="roomWritable(imp.cluster || filter) ? '' : (readOnlyNote(imp.cluster || filter) || writeNote)" @click="submitImport">
                    {{ imp.sending ? '排队中…' : '导入并换进房间' }}</button>
                  <button class="btn primary sm" v-else :disabled="!roomWritable(imp.cluster || filter) || imp.sending" :title="roomWritable(imp.cluster || filter) ? '' : (readOnlyNote(imp.cluster || filter) || writeNote)" @click="submitImport">
                    {{ imp.sending ? '排队中…' : '只做成快照' }}</button>
                </div>
                <span class="hint" v-if="imp.restore && !impTargetRunning">
                  提交前后端都会再对着当前进程状态核一遍：那个房间只要有世界在跑就拒收，房间里的存档一点不动。</span>
                <span class="hint" v-else>
                  不勾这一条就只是多出一份快照：它和手工备份一样能下载、能校验、能事后恢复。</span>
              </template>
            </template>
          </div>
        </div>

        <x-empty v-if="!snaps.length" icon="archive"
          :title="filter ? '这个房间还没有备份' : (clusters.length ? '还没有备份' : '这台机器上还没有房间')"
          :note="filter ? ('「' + filter + '」这个房间一份备份都没有；切回全部房间能看到别的房间的。')
                        : (!clusters.length
                            ? '备份是给「某个房间的存档目录」打的，导入外部存档也要指定导给哪个房间——两件事都排在有房间之后。'
                            : '先点上面的「备份 房间名」打一份。恢复只能用已经存在的备份，所以这一步不能省。')">
          <a class="btn sm primary" href="#/room" v-if="!clusters.length && !filter">去开服向导</a>
        </x-empty>

        <!-- 批量删除：勾中的备份一直显示在「已选 N 份」，整批期间按钮禁用，结果一条汇总。 -->
        <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap" v-if="snaps.length">
          <button class="btn sm ghost" @click="selectAllSnaps">全选</button>
          <span class="chip" v-if="selectedSnaps.length">已选 {{ selectedSnaps.length }} 份</span>
          <span class="grow"></span>
          <button class="btn sm danger" :disabled="!selectedSnaps.length || !selectedWritable || isBusy('backup.bulk.delete')"
                  :title="selectedWritable ? '' : (readOnlyNote(selectedSnaps[0] && selectedSnaps[0].cluster) || writeNote)" @click="removeSelected">{{ isBusy('backup.bulk.delete') ? '删除中……' : '批量删除' }}</button>
          <button class="btn sm ghost" v-if="selectedSnaps.length" @click="sel = {}">清空选择</button>
        </div>

        <!-- 桌面端：一份备份一行，行尾只留两个动作（详情 / 恢复），其余进展开面板。 -->
        <div class="table-wrap hide-sm" v-if="snaps.length">
          <table class="data">
            <thead>
              <tr>
                <th style="width:30px"></th>
                <th>什么时候</th><th>房间</th><th>来源</th>
                <th class="right">归档</th><th class="right">解压前</th>
                <th class="right">文件</th><th>带存档的世界</th><th>能不能恢复</th><th></th>
              </tr>
            </thead>
            <tbody>
              <template v-for="s in snaps" :key="s.id">
                <tr>
                  <td><input type="checkbox" :checked="!!sel[s.id]" :aria-label="'选中备份 ' + s.id"
                             @change="toggleSel(s.id)"></td>
                  <td>
                    <b>{{ dateTime(s.createdAt) }}</b>
                    <div class="hint">{{ rel(s.createdAt) }} · <span class="mono">{{ s.id }}</span></div>
                  </td>
                  <td>{{ s.cluster }}</td>
                  <td>{{ srcLab(s) }}</td>
                  <td class="right mono">{{ bytes(s.archiveBytes) }}</td>
                  <td class="right mono muted">{{ bytes(s.uncompressedBytes) }}</td>
                  <td class="right mono">{{ num(s.fileCount) }}</td>
                  <td>{{ shardsOf(s) }}</td>
                  <td>
                    <!-- 结论留在行里，原因收进把手：同一句"还有世界在跑"在十几行里各摊三行，
                         整张表就成了说明文，而真正要扫的是哪几行不能点。 -->
                    <div class="row" style="gap:6px">
                      <x-badge :kind="level(s)" :text="verdict(s)"></x-badge>
                      <x-hint v-if="s.restoreNote" :text="s.restoreNote"></x-hint>
                      <x-hint v-else-if="s.runningNote" :text="s.runningNote"></x-hint>
                    </div>
                  </td>
                  <td>
                    <div class="btn-group" style="justify-content:flex-end">
                      <button class="btn sm ghost" @click="toggle(s)">{{ d.id === s.id ? '收起' : '详情' }}</button>
                      <button class="btn sm danger" v-if="d.id !== s.id"
                              :disabled="!roomWritable(s.cluster) || !s.restorable || !!s.runningNote" :title="roomWritable(s.cluster) ? '' : (readOnlyNote(s.cluster) || writeNote)" @click="restoreFrom(s)">恢复</button>
                    </div>
                  </td>
                </tr>
                <tr class="detail" v-if="d.id === s.id">
                  <td colspan="10">
                    <x-detail :s="s" :d="d" :clusters="clusters"
                      @reload="reloadDetail" @verify="verifyNow" @download="download"
                      @remove="remove" @restore="restoreFrom" @restore-run="runRestore"
                      @cancel-restore="d.restoring = false" @pick-target="d.target = $event"></x-detail>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>

        <!-- 窄屏：一行只留"哪天 + 哪个房间 + 能不能恢复 + 那两个按钮"。 -->
        <div class="col snap-list only-sm" style="gap:8px" v-if="snaps.length">
          <div class="mod-row" v-for="s in snaps" :key="s.id">
            <div class="col mod-main">
              <div class="row" style="gap:8px">
                <input type="checkbox" style="flex:none" :checked="!!sel[s.id]" :aria-label="'选中备份 ' + s.id"
                       @change="toggleSel(s.id)">
                <b>{{ dateTime(s.createdAt) }}</b>
                <span class="grow"></span>
                <x-badge :kind="level(s)" :text="verdict(s)"></x-badge>
              </div>
              <div class="row mod-meta">
                <span class="chip">{{ s.cluster }}</span>
                <span class="chip">{{ srcLab(s) }}</span>
                <span class="chip mono">{{ bytes(s.archiveBytes) }}</span>
                <span class="chip mono">{{ num(s.fileCount) }} 个文件</span>
                <span class="chip">{{ shardsOf(s) }}</span>
              </div>
              <!-- 整句原因每一行都重复一遍（手机上尤其占地方）：这里只留短名，
                   点开才是"缺了什么/要停哪个世界"的全文。桌面那张表同理。 -->
              <div class="row bad-note" v-if="s.restoreNote || s.runningNote">
                <x-icon name="warn" :size="14"></x-icon>
                <span>{{ s.restoreNote ? '这份本身缺东西' : '要先停服' }}</span>
                <x-hint :text="s.restoreNote || s.runningNote"></x-hint>
              </div>
              <div class="row" style="gap:6px;margin-top:2px">
                <button class="btn sm ghost" @click="toggle(s)">{{ d.id === s.id ? '收起' : '详情' }}</button>
                <button class="btn sm danger" v-if="d.id !== s.id"
                        :disabled="!roomWritable(s.cluster) || !s.restorable || !!s.runningNote" :title="roomWritable(s.cluster) ? '' : (readOnlyNote(s.cluster) || writeNote)" @click="restoreFrom(s)">恢复</button>
              </div>
              <div class="drawer" v-if="d.id === s.id">
                <x-detail :s="s" :d="d" :clusters="clusters"
                  @reload="reloadDetail" @verify="verifyNow" @download="download"
                  @remove="remove" @restore="restoreFrom" @restore-run="runRestore"
                  @cancel-restore="d.restoring = false" @pick-target="d.target = $event"></x-detail>
              </div>
            </div>
          </div>
        </div>
      </template>
    </div>`,
};
