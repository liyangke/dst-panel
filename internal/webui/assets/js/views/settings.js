// 面板设置页：只放"改了立刻生效、不用重启面板"的那几个数字。
//
// 这一页刻意不重复配置文件里的路径、端口、账号——那些东西改了要重启，
// 而且写错了面板可能起不来；放在网页上改，等于给一次误操作一个不可回滚的入口。
// 这里四项都是运行时参数：保留期与两个存档策略，后端每次用之前现读。
//
// 每一项都要说清三件事：现在生效的是多少、没改过时用的是什么（面板默认还是 yaml 里那份）、
// 以及 0 是什么意思。少一句，用户就会自己去猜"0 是立刻删还是永远留"。
//
// 下面「监控与自愈」是另一套接口（/api/v1/monitor），和运行参数各读各的、各存各的：
// 一个接口挂了不该把另一节也藏起来，所以这一节放在主表单的 v-else 之外。
import * as api from "../api.js";
import { act, toast, state } from "../store.js";

// PUT /api/v1/monitor/config 只认这些键（见 internal/monitor/options.go 的 settingsKeys），
// 多一个都会被后端整单拒绝。字段表同时驱动表单渲染、本地校验和错误定位，不散着写三遍。
const MON_FIELDS = [
  { key: "cpuThreshold", label: "CPU 告警阈值", unit: "%", note: "整机 CPU 超过这个百分比就记一条告警；填 0 表示关闭这一项。" },
  { key: "memThreshold", label: "内存告警阈值", unit: "%", note: "整机内存占用超过这个百分比就告警；填 0 表示关闭这一项。" },
  { key: "diskThreshold", label: "存档盘告警阈值", unit: "%", note: "存档所在磁盘占用超过这个百分比就告警；填 0 表示关闭这一项。" },
  { key: "helperSilenceSeconds", label: "助手失联判定", unit: "秒", note: "在跑的分片这么久没有新的助手快照，就算「长时间无快照」并告警。" },
  { key: "shutdownWaitSeconds", label: "停服等待上限", unit: "秒", note: "发出停止命令后，最多等这么久确认 screen 会话真的退出（1~600）。" },
  { key: "webhookTimeoutSeconds", label: "Webhook 超时", unit: "秒", note: "单次 Webhook 请求的超时；发失败只记日志，不影响采样。" },
  { key: "alertCooldownMinutes", label: "同类告警冷却", unit: "分钟", note: "同一类告警在这个时间内不重复记，避免每分钟刷一条。" },
  { key: "maxSamples", label: "采样保留条数", unit: "条", note: "samples.jsonl 最多留几条，防止无限增长（1~10000）。" },
];
const MON_NUM_KEYS = MON_FIELDS.map((f) => f.key);
const MON_KEYS = [...MON_NUM_KEYS, "webhookUrl", "autoRecover"];
const MON_INT_KEYS = ["shutdownWaitSeconds", "helperSilenceSeconds", "webhookTimeoutSeconds", "alertCooldownMinutes", "maxSamples"];
const MON_PCT_KEYS = ["cpuThreshold", "memThreshold", "diskThreshold"];

export default {
  name: "Settings",
  data() {
    return {
      items: [], loading: false, error: "",
      form: {},          // key -> 输入框里的字符串
      saving: false,
      // 监控与自愈：独立数据源，不和上面的运行参数共用 loading / error
      mon: null, monLoading: false, monError: "", monSaving: false,
      monForm: {}, monErrors: {}, monFields: MON_FIELDS,
    };
  },
  computed: {
    canWrite() { return !!(state.me && state.me.isAdmin); },
    // 只把真改过的项发给后端：全量 PUT 会把没动过的项也写成"面板里设过"，
    // 往后配置文件的默认值就再也盖不回来了。
    patch() {
      const out = {};
      this.items.forEach((it) => {
        const v = String(this.form[it.key] === undefined ? it.value : this.form[it.key]).trim();
        if (v !== String(it.value)) out[it.key] = v;
      });
      return out;
    },
    dirtyCount() { return Object.keys(this.patch).length; },
    groups() {
      const g = [
        { key: "keep", title: "记录与趋势", note: "面板自己攒的数据，只占库文件。", items: [] },
        { key: "backup", title: "存档备份", note: "占磁盘的是归档文件；数字只决定留几份、多久打一次。", items: [] },
      ];
      this.items.forEach((it) => {
        (String(it.key).indexOf("backup_") === 0 ? g[1] : g[0]).items.push(it);
      });
      return g.filter((x) => x.items.length);
    },
    // 监控配置也是补丁提交：数字项转成 Number，URL 保持字符串，开关是布尔。
    monPatch() {
      const out = {};
      const c = (this.mon && this.mon.config) || {};
      const cur = (k) => (c[k] === undefined ? "" : String(c[k]));
      MON_FIELDS.forEach((f) => {
        const v = String(this.monForm[f.key] === undefined ? cur(f.key) : this.monForm[f.key]).trim();
        if (v !== cur(f.key)) out[f.key] = Number(v);
      });
      const w = String(this.monForm.webhookUrl === undefined ? (c.webhookUrl || "") : this.monForm.webhookUrl);
      if (w !== String(c.webhookUrl || "")) out.webhookUrl = w;
      const ar = !!this.monForm.autoRecover;
      if (ar !== !!c.autoRecover) out.autoRecover = ar;
      return out;
    },
    monDirtyCount() { return Object.keys(this.monPatch).length; },
  },
  mounted() { this.load(); this.loadMonitor(); },
  methods: {
    async load() {
      this.loading = true;
      this.error = "";
      const res = await api.get("/api/v1/settings");
      this.loading = false;
      if (!res.ok) { this.error = res.error || ("接口返回 " + res.status); return; }
      this.apply(res.data || []);
    },
    apply(list) {
      this.items = list;
      const form = {};
      list.forEach((it) => { form[it.key] = String(it.value); });
      this.form = form;
    },
    reset() {
      const form = {};
      this.items.forEach((it) => { form[it.key] = String(it.value); });
      this.form = form;
    },
    // 默认值那行的说法：yaml 里也有一份的项必须说清"没改过时按配置文件来"，
    // 否则用户改了 yaml、界面还显示 20，会以为设置没生效。
    defaultNote(it) {
      if (it.fromConfig) return "配置文件里写的是 " + it.default + " " + it.unit + "（" + it.fromConfig + "）；在面板里改过之后以这里为准。";
      return "没改过时用 " + it.default + " " + it.unit + "。";
    },
    // 标签下面只留一句"现在没填就是几"，其余（配置文件那份怎么说、这个值是谁设的、
    // 0 以外还有什么讲究）收进气泡：文案还在 DOM 里，读屏和 Ctrl+F 都能找到，但不铺满一屏。
    shortNote(it) {
      const n = Number(it.default);
      const lab = it.fromConfig ? "配置文件 " : "面板默认 ";
      return lab + (Number.isFinite(n) ? n : it.default) + " " + it.unit;
    },
    fieldNote(it) {
      const parts = [this.defaultNote(it)];
      if (it.set) parts.push("现在这个值是之前在面板里存过的。");
      if (it.specialNote) parts.push(it.specialNote);
      return parts.join(" ");
    },
    async save() {
      const bad = Object.keys(this.patch).filter((k) => !/^-?\d+$/.test(this.patch[k]));
      if (bad.length) {
        const it = this.items.find((x) => x.key === bad[0]);
        toast("bad", "这几项要填整数", "「" + (it ? it.label : bad[0]) + "」不是整数；要填负数也写成 -1 这种形式。");
        return;
      }
      this.saving = true;
      try {
        const res = await act("settings.save", "PUT", "/api/v1/settings", this.patch,
          { okTitle: "运行参数已保存", okBody: "改完立刻生效，不用重启面板；正在跑的任务不受影响。", badTitle: "设置没保存", clusters: false });
        if (res.ok) this.apply(res.data || []);
      } finally {
        this.saving = false;
      }
    },

    // ===== 监控与自愈 =====
    async loadMonitor() {
      this.monLoading = true;
      const res = await api.get("/api/v1/monitor");
      this.monLoading = false;
      if (!res.ok) { this.monError = res.error || ("接口返回 " + res.status); return; }
      this.monError = "";
      this.applyMonitor(res.data || {});
    },
    applyMonitor(data) {
      this.mon = {
        config: {}, samples: [], alerts: [], expected: [], recovered: 0,
        ...(data || {}),
      };
      const c = this.mon.config || {};
      const form = { autoRecover: !!c.autoRecover, webhookUrl: c.webhookUrl || "" };
      MON_FIELDS.forEach((f) => { form[f.key] = String(c[f.key] === undefined ? "" : c[f.key]); });
      this.monForm = form;
      this.monErrors = {};
    },
    monReset() { this.applyMonitor(this.mon); },
    // 本地先校验一遍：后端对填歪的数字是「兜回默认值」而不是报错，
    // 等它兜回来用户会以为自己改成功了，所以越界值必须在发请求前就定位到字段。
    validateMonitor(patch) {
      const errs = {};
      MON_INT_KEYS.forEach((k) => {
        if (patch[k] === undefined) return;
        const v = patch[k];
        if (!Number.isFinite(v) || !Number.isInteger(v) || v <= 0) errs[k] = "要填正整数。";
      });
      if (patch.maxSamples !== undefined && patch.maxSamples > 10000) errs.maxSamples = "最多 10000 条。";
      if (patch.shutdownWaitSeconds !== undefined && patch.shutdownWaitSeconds > 600) errs.shutdownWaitSeconds = "最多 600 秒。";
      MON_PCT_KEYS.forEach((k) => {
        if (patch[k] === undefined) return;
        const v = patch[k];
        if (!Number.isFinite(v) || v < 0 || v > 100) errs[k] = "填 0~100；填 0 表示关闭这一项告警。";
      });
      if (patch.webhookUrl !== undefined && patch.webhookUrl && !/^https?:\/\//i.test(patch.webhookUrl)) {
        errs.webhookUrl = "要填 http:// 或 https:// 开头的地址，留空表示不发通知。";
      }
      return errs;
    },
    async saveMonitor() {
      if (!this.mon || !this.monDirtyCount) return;
      const patch = this.monPatch;
      const errs = this.validateMonitor(patch);
      if (Object.keys(errs).length) { this.monErrors = errs; return; }
      this.monErrors = {};
      this.monSaving = true;
      try {
        const res = await act("monitor.save", "PUT", "/api/v1/monitor/config", patch, {
          okTitle: "监控设置已保存",
          okBody: "改完立刻生效，不用重启面板；自愈开关默认关闭。",
          badTitle: "监控设置没保存",
          clusters: false,
          // 后端拒绝时把错误定位到具体字段；认不出键名就落到表单顶部一条。
          onError: (r) => {
            const msg = r.error || ("接口返回 " + r.status);
            const hit = MON_KEYS.find((k) => msg.indexOf(k) >= 0);
            this.monErrors = hit ? { [hit]: msg } : { _form: msg };
          },
        });
        if (res.ok) await this.loadMonitor();
      } finally {
        this.monSaving = false;
      }
    },
  },
  template: `
    <div class="col settings-page" style="gap:14px">
      <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap">
        <span class="card-title-sm">改完就生效的运行参数</span>
        <x-hint label="生效时机" text="这四项改完立刻生效，不用重启面板。保留期清理在面板启动时跑一次，之后每 6 小时一次；所以把保留天数改小，旧记录也要等下一轮清理才消失，不是马上没。"></x-hint>
        <span class="grow"></span>
        <button class="btn ghost sm" @click="load" :disabled="loading">{{ loading ? '读取中' : '刷新' }}</button>
      </div>

      <x-sk v-if="loading && !items.length" :lines="4"></x-sk>
      <div class="err-box" v-else-if="error">{{ error }}</div>

      <template v-else>
        <section class="card" v-for="g in groups" :key="g.key">
          <div class="card-head">
            <span class="ico-tile"><x-icon name="gear" :size="15"></x-icon></span>
            <h3>{{ g.title }}</h3>
            <x-hint :text="g.note"></x-hint>
          </div>
          <div class="grid-form">
            <div class="field" v-for="it in g.items" :key="it.key">
              <div class="field-head">
                <label :for="'set-' + it.key">{{ it.label }}（{{ it.unit }}）</label>
                <x-hint :text="fieldNote(it)"></x-hint>
              </div>
              <input :id="'set-' + it.key" type="number" inputmode="numeric" step="1"
                     v-model="form[it.key]" :class="String(form[it.key]) !== String(it.value) ? 'changed' : ''">
              <span class="hint">{{ shortNote(it) }}</span>
              <span class="hint warn" v-if="it.specialNote && Number(form[it.key]) <= 0">{{ it.specialNote }}</span>
            </div>
          </div>
          <div class="row mt" style="gap:8px" v-if="g.key === 'keep'">
            <a class="linkish" href="#/audit">去看看操作记录</a>
            <span class="dim">·</span>
            <a class="linkish" href="#/">在线趋势在总览页</a>
          </div>
          <div class="row mt" style="gap:8px" v-if="g.key === 'backup'">
            <a class="linkish" href="#/snaps">去存档备份页</a>
            <span class="dim">·</span>
            <x-hint label="定时备份怎么跑" text="保留份数超了就删最早的；定时备份要面板一直开着，关着的时候就只在存档页手工打。"></x-hint>
          </div>
        </section>

        <div class="row" style="gap:8px;align-items:center">
          <button class="btn primary" :disabled="!dirtyCount || saving" @click="save">
            {{ saving ? '保存中……' : (dirtyCount ? '保存 ' + dirtyCount + ' 项改动' : '没有改动') }}</button>
          <button class="btn ghost sm" :disabled="!dirtyCount || saving" @click="reset">改回现在的值</button>
          <x-hint v-if="dirtyCount" label="只写改动过的项" text="只有改动过的那几项会被写入；没碰过的继续跟着配置文件。全量保存会把没动过的项也标成「面板里设过」，往后配置文件的默认值就盖不回来了。"></x-hint>
        </div>
      </template>

      <!-- 监控与自愈：和上面的运行参数各读各的接口、各存各的，一个挂了不影响另一节 -->
      <section class="card" v-if="mon || monLoading || monError">
        <div class="card-head">
          <span class="ico-tile"><x-icon name="gear" :size="15"></x-icon></span>
          <h3>监控与自愈</h3>
          <x-hint label="这一节管什么" text="面板每 60 秒采一次整机指标：超阈值、在跑的分片长时间没快照、或分片意外消失时记一条告警。自愈开关默认关闭。"></x-hint>
          <span class="grow"></span>
          <button class="btn ghost sm" @click="loadMonitor" :disabled="monLoading">{{ monLoading ? '读取中' : '刷新' }}</button>
        </div>

        <x-sk v-if="monLoading && !mon" :lines="5"></x-sk>
        <div class="err-box" v-else-if="monError && !mon">{{ monError }}</div>

        <template v-if="mon">
          <div class="field">
            <div class="field-head"><label for="mon-autoRecover">自动拉起（自愈总开关）</label></div>
            <label class="row" style="gap:8px;align-items:center;cursor:pointer">
              <input id="mon-autoRecover" type="checkbox" v-model="monForm.autoRecover">
              <span>{{ monForm.autoRecover ? '已开启' : '已关闭（默认）' }}</span>
            </label>
            <div class="hint warn">开启后面板重启 / 分片消失时会尝试自动拉起；关闭时只告警、绝不自动重启。</div>
            <x-field-error :text="monErrors.autoRecover"></x-field-error>
          </div>

          <div class="row" style="gap:14px;flex-wrap:wrap;margin:8px 0 4px">
            <span class="dim">采样 <b>{{ (mon.samples || []).length }}</b> 条（上限 {{ mon.config.maxSamples }}）</span>
            <span class="dim">期望在跑 <b>{{ (mon.expected || []).length }}</b> 个分片</span>
            <span class="dim">自愈已恢复 <b>{{ mon.recovered || 0 }}</b> 次</span>
          </div>

          <div class="card-head" style="margin-top:8px"><h3>最近告警</h3><span class="dim">{{ (mon.alerts || []).length }} 条</span></div>
          <div class="empty" v-if="!(mon.alerts && mon.alerts.length)">还没有告警。</div>
          <div class="dense table-wrap hide-sm" v-else>
            <table class="data">
              <thead><tr><th>时间</th><th>类别</th><th>内容</th></tr></thead>
              <tbody>
                <tr v-for="(a, i) in mon.alerts.slice(-8).reverse()" :key="i">
                  <td>{{ a.at }}</td><td>{{ a.kind }}</td><td>{{ a.message }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <div class="card-head" style="margin-top:12px"><h3>告警阈值与通知</h3></div>
          <x-field-error :text="monErrors._form"></x-field-error>
          <div class="grid-form">
            <div class="field" v-for="f in monFields" :key="f.key">
              <div class="field-head">
                <label :for="'mon-' + f.key">{{ f.label }}（{{ f.unit }}）</label>
                <x-hint :text="f.note"></x-hint>
              </div>
              <input :id="'mon-' + f.key" type="number" inputmode="decimal" step="any"
                     v-model="monForm[f.key]"
                     :class="String(monForm[f.key]) !== String(mon.config[f.key]) ? 'changed' : ''">
              <x-field-error :text="monErrors[f.key]"></x-field-error>
            </div>
            <div class="field">
              <div class="field-head">
                <label for="mon-webhookUrl">Webhook 地址</label>
                <x-hint text="留空就不发外部通知；填了之后每次告警会 POST 一份 JSON 过去。只接受 http:// 或 https://。"></x-hint>
              </div>
              <input id="mon-webhookUrl" type="text" v-model="monForm.webhookUrl" placeholder="留空 = 不发通知"
                     :class="monForm.webhookUrl !== (mon.config.webhookUrl || '') ? 'changed' : ''">
              <x-field-error :text="monErrors.webhookUrl"></x-field-error>
            </div>
          </div>

          <div class="row mt" style="gap:8px;align-items:center;flex-wrap:wrap">
            <x-perm-btn :allowed="canWrite" reason="这个账号没有修改权限（需要管理员）"
                        :busy="monSaving || !monDirtyCount" tone="primary" @go="saveMonitor">
              {{ monSaving ? '保存中……' : (monDirtyCount ? '保存 ' + monDirtyCount + ' 项监控设置' : '没有改动') }}
            </x-perm-btn>
            <button class="btn ghost sm" :disabled="!monDirtyCount || monSaving" @click="monReset">改回当前值</button>
            <x-hint label="改完立刻生效" text="监控配置写盘后立刻生效，不用重启面板；不认识的配置项会被后端拒绝，填歪的数字会被兜回默认值，所以保存前先在界面上校验一遍。"></x-hint>
          </div>
        </template>
      </section>
    </div>`,
};
