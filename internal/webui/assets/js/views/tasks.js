// 任务中心：队列里进过什么、哪条还在跑、哪条失败过、还能不能取消。
//
// 三个取舍：
//   - 只列最近 200 条。任务是"运维回头看"的东西，历史更久远的在库里有、页面不追；
//     留着"加载更多"却从不点，只是给数据库加负担。
//   - "能提交的任务"由 /task-kinds 决定，按钮只对 runnable 的开放。
//     存档备份与恢复不在这里提交：它们的参数必须是面板对着磁盘核过的房间名/快照编号，
//     所以这里只给一句"去哪一页"，而不是摆一个点了必然 400 的按钮。
//   - 取消只对"排队中"开放。"执行中"的取消会半路掐断正在写的文件，后端也会拒；
//     界面上先说清楚，而不是让用户点了才看到 409。
import * as api from "../api.js";
import { state, act, askConfirm, rel, isBusy } from "../store.js";
import { dateTime, seconds } from "../fmt.js";

const LIST_LIMIT = 200;
const OPEN = ["queued", "running"];

const FILTERS = [
  { code: "", label: "全部" },
  { code: "open", label: "没结束" },
  { code: "succeeded", label: "已完成" },
  { code: "failed", label: "失败" },
  { code: "canceled", label: "已取消" },
];

// 状态色与总览页同源：排队=信息色，执行中=会呼吸的绿，成功=绿，失败=红，取消=灰。
function statusKind(s) {
  if (s === "running") return "running";
  if (s === "queued") return "info";
  if (s === "succeeded") return "ok";
  if (s === "failed") return "bad";
  return "idle";
}

export default {
  name: "Tasks",
  setup() { return { state, rel, dateTime, isBusy }; },
  data() {
    return {
      rows: [], loading: false, error: "",
      status: "", kind: "",
      kinds: [], kindsError: "",
      // 手机上一行长消息最多三行，展开的记在这里（同一时刻可以展开多条：任务消息彼此独立）。
      expanded: {},
      // 上一次同步时最近 30 条任务的指纹。轮询没带来变化就不重读整张表。
      sig: "",
      isPhone: false,
    };
  },
  computed: {
    // x-seg 要的是 {value,label}；这张表里存的是 code，只在这一处换算，
    // 免得两个名字散在模板里，改一个忘一个。
    filters() { return FILTERS.map((f) => ({ value: f.code, label: f.label })); },
    kindOptions() {
      // 选项文字用这一类在 /task-kinds 里的正式名字，不是某一行自己的标题：
      // 标题带着房间名和时间（"备份 Cluster_1 存档"），拿第一行的标题当选项，
      // 点下去筛出来的却是这一类的全部任务，写着的是一个、做的是另一个。
      const names = {};
      this.kinds.forEach((k) => { names[k.kind] = k.label; });
      const seen = {};
      this.rows.forEach((t) => { if (!seen[t.kind]) seen[t.kind] = t.title; });
      return Object.keys(seen).map((k) => ({ kind: k, label: names[k] || seen[k] }));
    },
    visible() {
      return this.rows.filter((t) => {
        if (this.kind && t.kind !== this.kind) return false;
        if (!this.status) return true;
        if (this.status === "open") return OPEN.indexOf(t.status) >= 0;
        return t.status === this.status;
      });
    },
    counts() {
      const c = { queued: 0, running: 0, failed: 0 };
      this.rows.forEach((t) => { if (c[t.status] !== undefined) c[t.status]++; });
      return c;
    },
    lastFailed() {
      return this.rows.find((t) => t.status === "failed") || null;
    },
    submittable() { return this.kinds.filter((k) => !k.submitVia); },
    viaOthers() { return this.kinds.filter((k) => k.submitVia); },
    // 备份/恢复这两类只在存档页提交，这里给的是"去哪一页"，不照搬接口路径。
    otherHref() { return (k) => (String(k.kind).indexOf("backup.") === 0 ? "#/snaps" : ""); },
    otherWhere() { return (k) => (String(k.kind).indexOf("backup.") === 0 ? "存档备份页" : "对应的功能页"); },
    viaNote() {
      return (k) => "「" + k.label + "」要带房间名或备份编号，参数得由面板对着磁盘上的名单核过才拼得出来，" +
        "所以只能在" + this.otherWhere(k) + "提交，那里给的选项都是当前真实存在的。";
    },
    runningShards() {
      const out = [];
      state.clusters.forEach((c) => (c.shards || []).forEach((s) => {
        if (s.running) out.push(c.name + " 的 " + s.name);
      }));
      return out;
    },
    // 任务中心：viewer 只读，其余角色可写（取消 / 重试 / 提交）。
    canWrite() { return !!(state.me && state.me.role !== "viewer"); },
    writeNote() { return "这个账号是只读：能看状态，但修改会被拒（改自己的密码除外）"; },
  },
  watch: {
    // 顶栏那份轮询只带最近 30 条。它一变，说明队列有了新进展，整张表重读一次。
    "state.tasks"() {
      if (this.loading) return;
      if (this.signal() !== this.sig) this.load(true);
    },
  },
  mounted() {
    this.isPhone = !!(window.matchMedia && window.matchMedia("(max-width: 560px)").matches);
    this.load();
    this.loadKinds();
  },
  methods: {
    signal() {
      return (state.tasks || []).map((t) => t.id + ":" + t.status + ":" + t.progress + ":" + t.message).join("|");
    },
    async load(quiet) {
      if (!quiet) { this.loading = true; this.error = ""; }
      const res = await api.get("/api/v1/tasks" + api.qs({ limit: LIST_LIMIT }));
      this.loading = false;
      if (!res.ok) {
        if (!quiet) { this.error = res.error || ("接口返回 " + res.status); this.rows = []; }
        return;
      }
      this.rows = res.data || [];
      this.sig = this.signal();
    },
    async loadKinds() {
      const res = await api.get("/api/v1/task-kinds");
      if (!res.ok) { this.kindsError = res.error || ("接口返回 " + res.status); return; }
      this.kinds = res.data || [];
      this.kindsError = "";
    },
    badgeOf(t) { return statusKind(t.status); },
    isOpen(t) { return OPEN.indexOf(t.status) >= 0; },
    // 耗时只在任务结束之后算：执行中的耗时每秒都在变，为了它让整张表每秒重绘不值得，
    // "开始于 3 分钟前"已经回答了"卡了多久"。
    dur(t) {
      if (!t.startedAt || !t.finishedAt) return "";
      const s = (new Date(t.finishedAt) - new Date(t.startedAt)) / 1000;
      return Number.isFinite(s) && s >= 0 ? seconds(s) : "";
    },
    msgLong(t) { return String(t.message || "").length > (this.isPhone ? 80 : 10000); },
    // 执行中的"已经跑了多久"。耗时要结束才算得出，长下载要的就是这一句，
    // 缺了它整张表只剩一个几分钟不动的百分比。
    // 第一帧常常落在 1 秒以内（提交后立刻回读列表），那时"已跑 不到 1 秒"是句废话，
    // 说"已开始"就够了。
    runningFor(t) {
      if (t.status !== "running" || !t.startedAt) return "";
      void state.tick;
      const s = (Date.now() - new Date(t.startedAt).getTime()) / 1000;
      if (!Number.isFinite(s) || s < 0) return "";
      return s < 1 ? "已开始" : "已跑 " + seconds(s);
    },
    toggleMsg(t) { this.expanded[t.id] = !this.expanded[t.id]; },
    // 长任务要说清"现在到哪一步"：detail.phase 是后端给的中文阶段名
    // （见 internal/store/task.go 的 TaskDetail），没有明细时退回 worker 写的那句进度。
    phaseText(t) {
      if (t.detail && t.detail.phase) return t.detail.phase;
      if (this.isOpen(t) && t.message) return String(t.message).slice(0, 48);
      return t.statusLabel || "进行中";
    },
    // 提交前的拦路话。服务端同样会拒，这里提前说一句是为了不让人白等一次失败。
    blockNote(k) {
      if (!k.runnable) return k.note || "这台机器现在做不了这件事，检查配置后重启面板。";
      if (this.runningShards.length) {
        return "还有世界在跑（" + this.runningShards.join("、") + "）。这类任务要求全部世界先停掉，否则一执行就会失败。";
      }
      return "";
    },
    async submit(k) {
      const ok = await askConfirm({
        title: "提交「" + k.label + "」？",
        body: k.desc,
        warn: "任务进队列后后台按顺序执行，面板重启也不丢；做完以前世界不要启动。",
        danger: !!k.danger,
        okLabel: "排队执行",
        details: this.runningShards.length
          ? ["现在还在跑：" + this.runningShards.join("、") + "。先停掉它们再提交。"]
          : [],
      });
      if (!ok) return;
      const res = await act("task.submit:" + k.kind, "POST", "/api/v1/tasks",
        { kind: k.kind, confirm: true },
        { okTitle: "任务已排队", okBody: (x) => (x && x.title ? x.title + "，进度看下面这张表" : ""), badTitle: "任务没排进队列", clusters: false });
      if (res.ok) await this.load(true);
    },
    async cancel(t) {
      const ok = await askConfirm({
        title: "取消排队中的任务 #" + t.id + "？",
        body: "「" + t.title + "」还没开始跑，取消后这一条不会再执行。",
        warn: "取消只是把这一行划掉，不会撤掉它之前已经改过的文件；要重做请重新提交。",
        okLabel: "取消这条任务",
      });
      if (!ok) return;
      const res = await act("task.cancel:" + t.id, "DELETE", "/api/v1/tasks/" + encodeURIComponent(t.id), undefined,
        { okTitle: "任务已取消", okBody: (x) => (x && x.note) || "", badTitle: "取消没成", clusters: false });
      if (res.ok) await this.load(true);
    },
  },
  template: `
    <div class="col task-page" style="gap:14px">
      <!-- 手机上先给一行数字：这一页要回答"还有没有活没干完"，那三个数比筛选器更该占第一行。 -->
      <div class="row chip-strip only-sm" style="gap:6px">
        <span class="chip">排队 {{ counts.queued }}</span>
        <span class="chip">执行中 {{ counts.running }}</span>
        <span class="chip">失败 {{ counts.failed }}</span>
        <span class="chip" v-if="lastFailed">最近一次失败：{{ rel(lastFailed.finishedAt || lastFailed.createdAt) }}</span>
        <span class="chip" v-else>没有失败的任务</span>
      </div>

      <div class="grid grid-2 hide-sm">
        <x-stat label="排队中" icon="clock" tint="tint-gold" :value="counts.queued" hint="按提交顺序执行，一条跑完才轮到下一条。"></x-stat>
        <x-stat label="执行中" icon="play" :value="counts.running" tone="ok"
                hint="执行中的任务不能取消：半路掐断会留下没写完的文件。要停只能等它跑完。"></x-stat>
        <x-stat label="失败" icon="warn" :value="counts.failed" tone="bad"
                hint="点下面表里那一行看结论；这一栏数的是最近 200 条里失败过的次数。">
          <span v-if="lastFailed">{{ lastFailed.title }} · {{ rel(lastFailed.finishedAt || lastFailed.createdAt) }}</span>
          <span v-else>最近 {{ rows.length }} 条里没有失败的</span>
        </x-stat>
        <x-stat label="这一页的范围" icon="list" tint="tint-slate" :value="'最近 ' + rows.length + ' 条'"
                hint="更早的按保留期清理，翻不到就是不在了。这一页只取最近 200 条，更长久的历史留在库文件里。"></x-stat>
      </div>

      <section class="card task-submit" v-if="kindsError">
        <div class="card-head"><span class="ico-tile tint-gold"><x-icon name="clipboard" :size="15"></x-icon></span><h3>能提交的任务</h3></div>
        <div class="err-box">{{ kindsError }}</div>
      </section>
      <section class="card task-submit" v-else-if="kinds.length">
        <div class="card-head">
          <span class="ico-tile tint-gold"><x-icon name="clipboard" :size="15"></x-icon></span>
          <h3>能提交的任务</h3>
          <span class="grow"></span>
          <span class="chip mono">{{ submittable.length }} 类</span>
        </div>
        <div class="col" style="gap:10px">
          <div v-for="k in submittable" :key="k.kind" class="col" style="gap:5px">
            <div class="row" style="gap:8px;align-items:center">
              <b>{{ k.label }}</b>
              <x-hint :text="k.desc || '接口没给这一类的说明。'"></x-hint>
              <x-badge v-if="blockNote(k)" kind="warn" text="现在提交会失败"></x-badge>
              <x-badge v-else kind="ok" text="可以提交"></x-badge>
              <span class="grow"></span>
              <button class="btn sm" :class="blockNote(k) ? '' : 'primary'"
                      :disabled="!canWrite || !!blockNote(k) || isBusy('task.submit:' + k.kind)" :title="canWrite ? '' : writeNote" @click="submit(k)">
                {{ isBusy('task.submit:' + k.kind) ? '排队中…' : '提交' }}</button>
            </div>
            <div class="hint warn" v-if="blockNote(k)">{{ blockNote(k) }}</div>
          </div>
          <!-- 备份与恢复不在这里提交：与其摆一个点了必然 400 的按钮，不如直接把那一步的入口递过去。 -->
          <div class="row" style="gap:8px;align-items:center" v-for="k in viaOthers" :key="k.kind">
            <b>{{ k.label }}</b>
            <x-hint :text="viaNote(k)"></x-hint>
            <span class="grow"></span>
            <a class="btn sm ghost" :href="otherHref(k)">{{ otherWhere(k) }}</a>
          </div>
        </div>
      </section>

      <div class="row task-filters" style="gap:8px">
        <x-seg :options="filters" :model-value="status" label="按状态筛选" @update="status = $event"></x-seg>
        <div class="field" style="min-width:150px">
          <select v-model="kind" aria-label="按任务类型筛选">
            <option value="">全部类型</option>
            <option v-for="k in kindOptions" :key="k.kind" :value="k.kind">{{ k.label }}</option>
          </select>
        </div>
        <span class="grow"></span>
        <button class="btn ghost sm" @click="load()" :disabled="loading">{{ loading ? '读取中' : '刷新' }}</button>
      </div>

      <x-sk v-if="loading && !rows.length" :lines="4"></x-sk>
      <div class="err-box" v-else-if="error">{{ error }}</div>
      <x-empty v-else-if="!visible.length" icon="clock"
        :title="rows.length ? '没有符合筛选的任务' : '还没有任何后台任务'"
        :note="rows.length ? '换一个状态或类型看看；筛选只作用在这一页的最近 ' + rows.length + ' 条上。'
                           : '下载服务端、备份存档、恢复存档这类耗时操作都会进这个队列，面板重启也不丢。'"></x-empty>

      <!-- 桌面端：一行一条，消息列吸收剩余宽度。 -->
      <div class="table-wrap hide-sm" v-if="visible.length">
        <table class="data">
          <thead>
            <tr>
              <th>提交时间</th><th>任务</th><th>状态</th>
              <th>进度与结论</th><th class="right">耗时</th><th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in visible" :key="t.id">
              <td>
                <b>{{ dateTime(t.createdAt) }}</b>
                <div class="hint">{{ rel(t.createdAt) }} · <span class="mono">#{{ t.id }}</span></div>
              </td>
              <td>
                <b>{{ t.title }}</b>
                <div class="hint" v-if="t.attempts > 1">第 {{ t.attempts }} 次尝试</div>
              </td>
              <td>
                <x-badge :kind="badgeOf(t)" :text="t.statusLabel"></x-badge>
                <div class="hint" v-if="t.note">{{ t.note }}</div>
              </td>
              <td class="wrap">
                <div class="row" style="gap:6px;align-items:center;flex-wrap:wrap" v-if="isOpen(t)">
                  <x-badge kind="info" :text="phaseText(t)"></x-badge>
                  <span class="hint">仍在进行</span>
                </div>
                <div class="bar" :class="{live: t.status === 'running'}" v-if="isOpen(t)">
                  <i :style="{width: (t.progress || 0) + '%'}"></i>
                </div>
                <div class="hint mono" v-if="isOpen(t)">{{ t.progress || 0 }}% · {{ dur(t) || runningFor(t) || '未开始' }}</div>
                <div class="hint" v-if="isOpen(t)">仍在进行，可以切走：面板重启也不丢，回到这一页就能接着看进度。</div>
                <!-- 正在下载时那一排数就是 message 的内容，两处都显示会把行高撑爆；
                     任务结束（含失败）后又把 message 放回来，那才是结论。 -->
                <x-dlstats v-if="t.detail && isOpen(t)" :detail="t.detail"></x-dlstats>
                <div class="task-msg" v-else-if="t.message">{{ t.message }}</div>
                <div class="hint" v-else-if="isOpen(t)">worker 还没写进度</div>
              </td>
              <td class="right mono">{{ dur(t) || runningFor(t) || "—" }}</td>
              <td>
                <button class="btn sm danger" v-if="t.status === 'queued'"
                        :disabled="!canWrite || isBusy('task.cancel:' + t.id)" :title="canWrite ? '' : writeNote" @click="cancel(t)">取消</button>
                <span class="hint" v-else-if="t.status === 'running'">跑着，不能取消</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- 窄屏：一条任务一张卡，长结论先折三行。 -->
      <div class="col only-sm" style="gap:8px" v-if="visible.length">
        <div class="mod-row" v-for="t in visible" :key="t.id">
          <div class="col mod-main">
            <div class="row" style="gap:8px">
              <b>{{ t.title }}</b>
              <span class="grow"></span>
              <x-badge :kind="badgeOf(t)" :text="t.statusLabel"></x-badge>
            </div>
            <div class="row mod-meta">
              <span class="chip mono">#{{ t.id }}</span>
              <span class="chip">{{ rel(t.createdAt) }}提交</span>
              <span class="chip" v-if="dur(t)">跑了 {{ dur(t) }}</span>
              <span class="chip" v-else-if="runningFor(t)">{{ runningFor(t) }}</span>
              <span class="chip" v-if="t.attempts > 1">第 {{ t.attempts }} 次</span>
            </div>
            <div class="shard-note" v-if="t.note">{{ t.note }}</div>
            <div class="row" style="gap:6px;align-items:center;flex-wrap:wrap" v-if="isOpen(t)">
              <x-badge kind="info" :text="phaseText(t)"></x-badge>
              <span class="chip">{{ runningFor(t) || '排队中' }}</span>
            </div>
            <div class="row" style="gap:8px;align-items:center" v-if="isOpen(t)">
              <div class="bar grow" :class="{live: t.status === 'running'}"><i :style="{width: (t.progress || 0) + '%'}"></i></div>
              <span class="hint mono">{{ t.progress || 0 }}%</span>
            </div>
            <div class="hint" v-if="isOpen(t)">仍在进行，可以切走：面板重启也不丢，回到这一页就能接着看进度。</div>
            <!-- 手机上这排数折成两列，message 那句和它是同一组数，只显示一个。 -->
            <x-dlstats v-if="t.detail && isOpen(t)" :detail="t.detail"></x-dlstats>
            <div class="task-msg" v-else-if="t.message" :class="expanded[t.id] ? 'open' : ''">{{ t.message }}</div>
            <div class="hint" v-else-if="isOpen(t)">worker 还没写进度</div>
            <div class="row" style="gap:6px">
              <button class="linkish" v-if="msgLong(t)" @click="toggleMsg(t)">{{ expanded[t.id] ? '收起这条结论' : '展开这条结论' }}</button>
              <button class="btn sm danger" v-if="t.status === 'queued'"
                      :disabled="!canWrite || isBusy('task.cancel:' + t.id)" :title="canWrite ? '' : writeNote" @click="cancel(t)">取消排队</button>
              <span class="hint" v-else-if="t.status === 'running'">执行中不能取消</span>
            </div>
          </div>
        </div>
      </div>
    </div>`,
};
