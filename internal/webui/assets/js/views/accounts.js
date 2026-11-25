// 账号管理页：面板登录账号，不是游戏内身份。
//
// 两条边界要在页面上说清楚，否则用户会以为"在这里建个号，游戏里就有管理员"：
//   1. 这里管的是"谁能打开面板"，权限分管理员 / 房间成员 / 只读三档；
//   2. 游戏内的管理员、白名单、黑名单在玩家管理页，靠 Klei 的文件与助手 MOD 生效。
//
// 只读账号也能进这一页：没有管理列表，但可以改自己的密码。
//
// 三档角色和"房间归属"是两套东西，但它们住同一页：角色决定这个账号能做什么级别的改动，
// 指派决定具体哪几间房归他管。「房间归属」那张卡按房间列，下面那张按账号列，
// 是同一张表的两个视角（「所有房间」那一页下掉之后，跨房间的指派就落在这里）。
// 会影响归属的两条接口规则（名下有房间不能改只读、最后一个管理员不能降）
// 提前筛掉——摆一个按下去报 409 的选项，等于让用户替我们读一遍后端注释。
import * as api from "../api.js";
import { state, act, toast, askConfirm, logout, loadSession, canAssignOwner, assignedText,
         ownerTone, ownerIcon } from "../store.js";
import { dateTime, ago } from "../fmt.js";
import { openOwner } from "../owner.js";

export default {
  name: "Accounts",
  setup() { return { state, openOwner, ownerTone, ownerIcon }; },
  data() {
    return {
      list: [], loading: false, error: "",
      form: { username: "", password: "", role: "member" },
      creating: false,
      resetID: 0, resetPass: "",
      selfForm: { oldPassword: "", newPassword: "", repeat: "" },
      savingSelf: false,
    };
  },
  computed: {
    isAdmin() { return !!(state.me && state.me.isAdmin); },
    // 账号管理是管理员专属：新建 / 改角色 / 删除 / 指派归属都只在 canWrite 为真时可用。
    canWrite() { return !!(state.me && state.me.isAdmin); },
    writeNote() { return "需要管理员账号才能改账号与归属"; },
    canAssign() { return canAssignOwner(); },
    me() { return state.me || {}; },
    // 「房间归属」那一张的行：直接用全局那份房间名单（外壳每五秒取一次 /clusters），
    // 这一页不另发一次扫盘请求。指派弹窗写完会让 store 重取，表跟着变。
    roomRows() { return state.clusters || []; },
    unassignedCount() { return this.roomRows.filter((c) => !c.ownerAccountId).length; },
    // 这一行那枚归属胶囊的悬停：先说"上一次是谁在什么时候派的"，再说派出去之后会有什么不同。
    // 说法整句来自后端（assignedNote / assignedAt），前端不自己拼人名。
    roomHint() {
      return (c) => {
        const note = assignedText(c);
        return (note ? note + "。" : "") +
          (c.ownerAccountId
            ? "他从此可以自己启停这间房的世界、改世界规则、填令牌。"
            : "还没派出去：谁都能看见这间房的状态，但只有管理员改得动它。");
      };
    },
    // 角色字典从 /session 带出来（api/accounts.go 的 roleChoices）：
    // 下拉里的说法、顺序，和接口拒你那句"角色只能是…"必须是同一份。
    roleDict() { return (state.me && state.me.roles) || []; },
    adminCount() { return this.list.filter((a) => a.role === "admin").length; },
    resetAcc() { return this.list.find((a) => a.id === this.resetID) || null; },
    // 新建账号那一句"这个角色能做什么"：跟着下拉当前项走，不写死。
    createRoleHint() {
      const r = this.roleDict.find((x) => x.value === this.form.role);
      return r ? r.hint : "";
    },
    // 重置密码那一格下面的说明：没填时讲"多长算合格"，填了以后讲"按下保存会发生什么"。
    // 桌面和手机共用这一句，否则同一个动作在两处会有两种说法。
    resetNote() {
      if (!this.resetPass) return "新密码至少 5 个字符；短于 8 位会提示容易被猜到，但可以保存。";
      return "重置后" + (this.resetAcc ? this.resetAcc.username : "对方") +
        "当前的登录立刻失效，需要用新密码重新登录。";
    },
    resetNoteLabel() { return this.resetPass ? "会发生什么" : "多长算合格"; },
  },
  watch: {
    // 页面挂载时 state.me 可能还没拉回来（登录/刷新是两条并行请求），
    // 只靠 mounted 会永远显示 0 个；身份一到就补拉一次。
    isAdmin(v) { if (v) this.load(); },
  },
  mounted() { if (this.isAdmin) this.load(); },
  methods: {
    dateTime, ago,
    // 界面这边的长度只做提示：放行与否由接口说。下限 5 是为了让出厂默认凭据能过自己的规则，
    // 而 8 位以下"短到会被猜到"这件事得让人在按下按钮之前就知道。
    chars(p) { return [...(p || "")].length; },
    passHint(p) {
      const n = this.chars(p);
      if (!n) return "";
      if (n < 5) return "少于 5 个字符，保存会被接口拒绝。";
      if (n < 8) return "可以保存，但短于 8 位很容易被猜到，建议再加几个字。";
      return "";
    },
    async load() {
      this.loading = true;
      this.error = "";
      const res = await api.get("/api/v1/accounts");
      this.loading = false;
      if (!res.ok) { this.error = res.error || ("接口返回 " + res.status); return; }
      this.list = res.data || [];
    },
    async create() {
      const f = this.form;
      if (!f.username.trim() || !f.password) {
        toast("bad", "还差两项", "用户名和初始密码都要填。密码至少 5 个字符。");
        return;
      }
      if (this.chars(f.password) < 5) {
        toast("bad", "密码太短", "至少要 5 个字符，接口不会收更短的。");
        return;
      }
      this.creating = true;
      try {
        const res = await act("account.create", "POST", "/api/v1/accounts", {
          username: f.username.trim(), password: f.password, role: f.role,
        }, {
          okTitle: "账号建好了",
          // 成员这个角色只建一半：没有房间派给他，他登进来能做的事和只读一样。
          // 那一步在归属那一张卡上，所以成功提示里直接说去哪按。
          okBody: () => "把用户名和初始密码告诉对方；对方可以在「账号管理」里改自己的密码。" +
            (f.role === "member"
              ? "还要在这一页「房间归属」那张卡里点「指派」，把要他管的房间划到他名下——没派房间的成员什么都动不了。"
              : ""),
          badTitle: "账号没建成", clusters: false,
        });
        if (res.ok) {
          this.form = { username: "", password: "", role: "member" };
          await this.load();
        }
      } finally {
        this.creating = false;
      }
    },
    beginReset(a) { this.resetID = a.id; this.resetPass = ""; },
    cancelReset() { this.resetID = 0; this.resetPass = ""; },
    async saveReset(a) {
      if (!this.resetPass) { toast("bad", "先填新密码", "密码至少 5 个字符。"); return; }
      if (this.chars(this.resetPass) < 5) {
        toast("bad", "密码太短", "至少要 5 个字符，接口不会收更短的。");
        return;
      }
      const res = await act("account.reset." + a.id, "PUT",
        "/api/v1/accounts/" + a.id + "/password", { password: this.resetPass }, {
          okTitle: "密码已重置", okBody: () => a.username + " 的旧密码已经失效，请把新密码告诉对方。",
          badTitle: "密码没改成", clusters: false,
        });
      if (res.ok) this.cancelReset();
    },
    // 这一行能切过去哪些角色。两种会被接口拒的换法在这里就不出现：
    //   - 最后一个管理员降级或删除（库里会留下"没人能改设置"的死局）；
    //   - 名下还有房间的人改成只读（接口回 409，要你先去把指派撤掉）。
    // 被筛掉的那条理由由 rowNote 写在旁边，不让人靠猜。
    roleOptionsFor(a) {
      const lastAdmin = a.role === "admin" && this.adminCount <= 1;
      const hasRooms = this.roomsOf(a).length > 0;
      return this.roleDict.filter((r) => {
        if (r.value === a.role) return false;
        if (lastAdmin && r.value !== "admin") return false;
        if (hasRooms && r.value === "viewer") return false;
        return true;
      });
    },
    // 一行没有按钮时那句"为什么"，三条理由可以叠着出现（admin 自己就是最后一个管理员，
    // 两句话都得说）。之前是一条 v-if 链，后面那条永远轮不到，
    // 于是那一行看起来只是"少摆了按钮"，看不出是被规则藏起来的。
    rowNote(a) {
      const bits = [];
      if (a.self) bits.push("改自己的密码用下面那块");
      if (a.role === "admin" && this.adminCount <= 1) bits.push("最后一个管理员：不能降级也不能删");
      if (!a.self && a.role !== "viewer" && this.roomsOf(a).length) {
        bits.push("名下还有房间，先撤指派才能改成只读");
      }
      return bits.join("；");
    },
    // 改角色的确认文案：说"改成之后他能做什么、不能做什么"，而不是"你确定吗"。
    roleChangeBody(a, r) {
      const rooms = this.roomsOf(a);
      const bits = [r.hint + "。"];
      if (r.value === "member" && rooms.length) {
        bits.push("他名下的 " + rooms.length + " 间房还是他的，指派不会跟着角色变。");
      }
      if (r.value === "admin") {
        bits.push("他从此能动这台机器上的一切，包括建房间和指派归属。");
      }
      if (a.role === "admin" && r.value !== "admin") {
        bits.push("他不再是管理员后，面板设置与账号管理那一页对他只剩改自己的密码。");
      }
      return bits.join("");
    },
    async setRole(a, next) {
      const r = this.roleDict.find((x) => x.value === next);
      if (!r) return;
      const ok = await askConfirm({
        title: "把 " + a.username + " 改成" + r.label + "？",
        body: this.roleChangeBody(a, r),
        okLabel: "改成" + r.label,
        danger: next === "viewer" || (a.role === "admin" && next !== "admin"),
      });
      if (!ok) return;
      const res = await act("account.role." + a.id, "PUT",
        "/api/v1/accounts/" + a.id + "/role", { role: next }, {
          okTitle: "权限已调整", okBody: () => a.username + " 现在是" + r.label,
          badTitle: "权限没改成", clusters: false,
        });
      if (res.ok) {
        await this.load();
        // 改的可能是自己：刷新身份，页面上的管理按钮会跟着变。
        if (a.self) await loadSession();
      }
    },
    // 三档角色三档颜色：一行的权限要一眼分得清，两个角色共用一种灰就不算分清。
    roleBadge(role) {
      if (role === "admin") return "info";
      if (role === "member") return "ok";
      return "idle";
    },
    // roomsOf 是归属表里派给这个账号的房间目录名（接口直接给了，不再拼）。
    roomsOf(a) { return a.ownedClusters || []; },
    // 那一列的悬浮说明：把房间目录名列出来，管理员看完就知道在这一页上面那张卡里改哪几行。
    roomsHint(a) {
      const rooms = this.roomsOf(a);
      if (!rooms.length) {
        return a.role === "member"
          ? "还没有房间派给他。在这一页「房间归属」那张卡里点那一行的「指派」就能划到他名下。"
          : "归属只派给「房间成员」，其他角色不领房间。";
      }
      return "派给他的房间：" + rooms.join("、") + "。要改指派，在这一页「房间归属」那张卡里点那一行。";
    },
    // 删除按钮的两种不出现：当前登录的账号（接口也拒），以及最后一个管理员。
    canDelete(a) {
      if (a.self) return false;
      return !(a.role === "admin" && this.adminCount <= 1);
    },
    removeNote(a) {
      const rooms = this.roomsOf(a);
      if (!rooms.length) return "删掉后这个用户名和密码都不能再登录面板。游戏里的角色不受影响。";
      return "删掉后这个用户名和密码都不能再登录面板。他名下那 " + rooms.length +
        " 间房会一起变回「未指派」：房间目录和存档都不会被动，只是暂时没有负责人。" +
        "游戏里的角色不受影响。";
    },
    removeWarn(a) {
      const rooms = this.roomsOf(a);
      if (!rooms.length) return "";
      return "这几间房从此谁都能看、但谁都改不了，直到管理员重新指派：" + rooms.join("、");
    },
    async remove(a) {
      const ok = await askConfirm({
        title: "删除账号 " + a.username + "？",
        body: this.removeNote(a),
        warn: this.removeWarn(a),
        okLabel: "删除账号", danger: true,
      });
      if (!ok) return;
      const res = await act("account.delete." + a.id, "DELETE", "/api/v1/accounts/" + a.id, undefined, {
        okTitle: "账号已删除", badTitle: "没删成", clusters: false,
      });
      if (res.ok) await this.load();
    },
    async saveSelf() {
      const f = this.selfForm;
      if (!f.oldPassword || !f.newPassword) {
        toast("bad", "还差两项", "旧密码和新密码都要填。");
        return;
      }
      if (f.newPassword !== f.repeat) {
        toast("bad", "两次输入不一致", "新密码和确认框里的内容不一样。");
        return;
      }
      if (this.chars(f.newPassword) < 5) {
        toast("bad", "密码太短", "至少要 5 个字符，接口不会收更短的。");
        return;
      }
      this.savingSelf = true;
      try {
        const res = await act("account.self", "PUT", "/api/v1/session/password", {
          oldPassword: f.oldPassword, newPassword: f.newPassword,
        }, { quiet: true, clusters: false });
        if (!res.ok) {
          if (res.status !== 401) toast("bad", "密码没改成", res.error || ("接口返回 " + res.status));
          return;
        }
        toast("ok", "密码已修改",
          (this.chars(f.newPassword) < 8 ? "新密码短于 8 位，还是容易被猜到。" : "")
          + "当前页面的旧凭据已作废，请用新密码重新登录。");
        this.selfForm = { oldPassword: "", newPassword: "", repeat: "" };
        logout();
      } finally {
        this.savingSelf = false;
      }
    },
  },
  template: `
    <div class="col accounts-page" style="gap:14px">
      <div class="row" style="gap:8px;align-items:center;flex-wrap:wrap">
        <span class="card-title-sm">面板登录账号，不是游戏里的身份</span>
        <x-hint label="两套账号的边界" text="这里管的是「谁能在浏览器里打开这个后台」。游戏里的管理员、白名单、黑名单是另一套，在「玩家管理」按角色处理，靠 Klei 的文件与助手 MOD 生效。这里的权限分三档：管理员可以改这台机器上的一切；房间成员只能动指派给自己的那几间房；只读账号能看完所有页面，任何修改都会被拒（改自己的密码除外）。"></x-hint>
        <x-hint label="角色和「归谁管」是两回事" text="角色决定这个账号能做什么级别的改动；一间房归谁管是另一张表，就在这一页下面那张「房间归属」里点。两张卡说的是同一份数据，只是一个人对着房间、一个房间对着人。"></x-hint>
      </div>

      <section class="card" v-if="!isAdmin">
        <div class="card-head">
          <span class="ico-tile tint-cyan"><x-icon name="user" :size="15"></x-icon></span>
          <h3>我的账号</h3>
          <x-badge :kind="roleBadge(me.role)" :text="me.roleLabel || '读不到角色'"></x-badge>
        </div>
        <p class="hint" v-if="me.roleHint">当前账号 <b>{{ me.username }}</b>：{{ me.roleHint }}。</p>
        <div class="row" style="gap:6px;flex-wrap:wrap" v-if="me.myClusters && me.myClusters.length">
          <span class="chip">我名下的房间 {{ me.myClusters.length }} 间</span>
          <span class="chip mono" v-for="c in me.myClusters" :key="c">{{ c }}</span>
        </div>
        <p class="hint" v-else-if="me.role === 'member'">还没有房间派给你：这一页下面改不了，
          要房间请管理员在「房间归属」那张卡里点「指派」（那张卡只对管理员显示）。没指派的房间你看得见状态，但改不动。</p>
      </section>

      <section class="card" :style="isAdmin ? '' : 'opacity:.55'">
        <div class="card-head">
          <span class="ico-tile tint-green"><x-icon name="plus" :size="15"></x-icon></span>
          <h3>新建账号</h3>
          <x-badge v-if="!isAdmin" kind="warn" text="需要管理员"></x-badge>
          <x-hint text="账号不绑定 Klei 账号或游戏角色，只用于登录面板。"></x-hint>
          <span class="grow"></span>
        </div>
        <x-note v-if="!isAdmin" kind="warn">当前账号不是管理员：能看见这些管理项，但没有权限新建账号、改角色或删除。需要管理员来操作。</x-note>
        <div class="grid-form">
          <div class="field">
            <label for="acc-user">用户名</label>
            <input id="acc-user" type="text" v-model.trim="form.username" autocomplete="off"
                   :disabled="!canWrite" :title="canWrite ? '' : writeNote" placeholder="2~32 个字符，中英文都行">
          </div>
          <div class="field">
            <label for="acc-pass">初始密码</label>
            <input id="acc-pass" type="password" v-model="form.password" autocomplete="new-password"
                   :disabled="!canWrite" :title="canWrite ? '' : writeNote" placeholder="至少 5 个字符">
            <div class="hint warn" v-if="passHint(form.password)">{{ passHint(form.password) }}</div>
          </div>
          <div class="field">
            <label for="acc-role">权限</label>
            <select id="acc-role" v-model="form.role" :disabled="!canWrite" :title="canWrite ? '' : writeNote">
              <option v-for="r in roleDict" :key="r.value" :value="r.value">{{ r.label }}</option>
            </select>
            <!-- 这一句跟着上面选中那档走：三档角色的差别就在"能改到哪一步"，
                 而建号之后要重新进这一页才能改角色，选错了代价最大。 -->
            <div class="hint" v-if="createRoleHint">{{ createRoleHint }}</div>
          </div>
        </div>
        <div class="row mt" style="gap:8px;align-items:center">
          <button class="btn primary" :disabled="!canWrite || creating"
                  :title="canWrite ? '' : writeNote" @click="create">
            {{ creating ? "正在创建……" : "创建账号" }}</button>
          <span class="hint" v-if="!canWrite">需要管理员账号</span>
          <x-hint label="建好以后" text="创建后把用户名和初始密码告诉对方，让对方自己在这一页改密码；面板不替你保管密码。选的是「房间成员」时还要在下面那张「房间归属」里点「指派」，把要他管的房间划到他名下——角色只是让他能动，指派才决定他能动哪几间。"></x-hint>
        </div>
      </section>

      <section class="card" :style="isAdmin ? '' : 'opacity:.55'">
        <div class="card-head">
          <span class="ico-tile tint-azure"><x-icon name="users" :size="15"></x-icon></span>
          <h3>已有账号</h3>
          <x-badge v-if="!isAdmin" kind="warn" text="需要管理员"></x-badge>
          <span class="hint">{{ list.length }} 个</span>
          <span class="grow"></span>
          <button class="btn sm ghost" :disabled="loading" @click="load">刷新</button>
        </div>
        <x-note v-if="!isAdmin" kind="warn">账号列表只有管理员能读取；当前账号不是管理员，下面的操作都用不了。需要管理员。</x-note>
        <div class="err-box" v-if="error">{{ error }}</div>
        <x-sk v-if="loading && !list.length" :lines="3"></x-sk>

        <div class="table-wrap hide-sm" v-if="list.length">
          <table class="data">
            <thead>
              <tr><th>账号</th><th>权限</th><th>归属</th><th>最近登录</th><th>操作</th></tr>
            </thead>
            <tbody>
              <template v-for="a in list" :key="a.id">
                <tr>
                  <td><b>{{ a.username }}</b>
                    <x-badge v-if="a.self" kind="ok" text="当前"></x-badge>
                    <x-badge v-if="a.usingDefaultPassword" kind="warn" text="默认密码"
                             title="还在用出厂默认密码，任何人都猜得到"></x-badge></td>
                  <td><x-badge :kind="roleBadge(a.role)" :text="a.roleLabel" :title="a.roleHint"></x-badge></td>
                  <td>
                    <div class="row" style="gap:6px;align-items:center">
                      <!-- 说法（"没有房间" / "名下 2 间"）来自接口，量词和 0 间怎么说不在前端拼。
                           悬浮标签不能再写"1 间"：接口那句话里已经数过一遍了。 -->
                      <span>{{ a.ownedLabel || "没有房间" }}</span>
                      <x-hint v-if="roomsOf(a).length" label="哪几间" :text="roomsHint(a)"></x-hint>
                      <x-hint v-else label="怎么派" :text="roomsHint(a)"></x-hint>
                    </div>
                  </td>
                  <td>
                    <span v-if="a.lastLoginAt">{{ dateTime(a.lastLoginAt) }}
                      <div class="hint">{{ ago(a.lastLoginAt) }}</div></span>
                    <span v-else class="hint">还没登录过</span>
                  </td>
                  <td>
                    <div class="row" style="gap:6px;flex-wrap:wrap">
                      <button class="btn sm ghost" v-if="!a.self" :disabled="!canWrite" :title="canWrite ? '' : writeNote" @click="beginReset(a)">重置密码</button>
                      <!-- 每个合法目标一枚按钮，按下去都收得到：会被接口拒的那两种换法
                           （最后一个管理员降级、名下有房间改只读）根本不出现，理由写在同一行。 -->
                      <button class="btn sm ghost" v-for="r in roleOptionsFor(a)" :key="r.value"
                              :disabled="!canWrite" :title="canWrite ? r.hint : writeNote" @click="setRole(a, r.value)">改成{{ r.label }}</button>
                      <button class="btn sm danger" v-if="canDelete(a)" :disabled="!canWrite" :title="canWrite ? '' : writeNote" @click="remove(a)">删除</button>
                      <span class="hint" v-if="rowNote(a)">{{ rowNote(a) }}</span>
                    </div>
                  </td>
                </tr>
                <tr class="detail" v-if="resetID === a.id">
                  <td colspan="5">
                    <div class="row" style="gap:8px;align-items:flex-end">
                      <div class="field grow">
                        <label :for="'reset-' + a.id">给 {{ a.username }} 设置新密码</label>
                        <input :id="'reset-' + a.id" type="password" v-model="resetPass"
                               :disabled="!canWrite" :title="canWrite ? '' : writeNote" autocomplete="new-password" placeholder="至少 5 个字符">
                        <div class="hint warn" v-if="passHint(resetPass)">{{ passHint(resetPass) }}</div>
                      </div>
                      <button class="btn primary sm" :disabled="!canWrite" :title="canWrite ? '' : writeNote" @click="saveReset(a)">保存新密码</button>
                      <button class="btn sm ghost" @click="cancelReset">取消</button>
                      <x-hint :label="resetNoteLabel" :text="resetNote"></x-hint>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>

        <div class="col only-sm" style="gap:8px" v-if="list.length">
          <div class="mod-row" v-for="a in list" :key="a.id">
            <div class="col mod-main">
              <div class="row" style="gap:8px;flex-wrap:wrap">
                <b>{{ a.username }}</b>
                <x-badge v-if="a.self" kind="ok" text="当前"></x-badge>
                <x-badge v-if="a.usingDefaultPassword" kind="warn" text="默认密码"
                         title="还在用出厂默认密码，任何人都猜得到"></x-badge>
                <x-badge :kind="roleBadge(a.role)" :text="a.roleLabel" :title="a.roleHint"></x-badge>
                <span class="grow"></span>
              </div>
              <div class="hint">{{ a.ownedLabel || '没有房间' }}<template v-if="roomsOf(a).length">（{{ roomsOf(a).join('、') }}）</template></div>
              <div class="hint">{{ a.lastLoginAt ? '最近登录 ' + ago(a.lastLoginAt) : '还没登录过' }}</div>
              <div class="row" style="gap:6px;flex-wrap:wrap" v-if="!a.self">
                <button class="btn sm ghost" :disabled="!canWrite" :title="canWrite ? '' : writeNote" @click="beginReset(a)">重置密码</button>
                <button class="btn sm ghost" v-for="r in roleOptionsFor(a)" :key="r.value"
                        :disabled="!canWrite" :title="canWrite ? '' : writeNote" @click="setRole(a, r.value)">改成{{ r.label }}</button>
                <button class="btn sm danger" v-if="canDelete(a)" :disabled="!canWrite" :title="canWrite ? '' : writeNote" @click="remove(a)">删除</button>
              </div>
              <div class="hint" v-if="rowNote(a)">{{ rowNote(a) }}</div>
              <div class="row" style="gap:8px;align-items:center" v-if="resetID === a.id">
                <input class="grow" type="password" v-model="resetPass" :disabled="!canWrite" :title="canWrite ? '' : writeNote" autocomplete="new-password"
                       placeholder="新密码，至少 5 位">
                <button class="btn primary sm" :disabled="!canWrite" :title="canWrite ? '' : writeNote" @click="saveReset(a)">保存</button>
                <button class="btn sm ghost" @click="cancelReset">取消</button>
              </div>
              <x-hint v-if="resetID === a.id" :label="resetNoteLabel" :text="resetNote"></x-hint>
            </div>
          </div>
        </div>
      </section>

      <!-- 「房间归属」：同一张归属表的第二个视角（上面那张按账号列，这一张按房间列）。
           #129 把「所有房间」那一页下掉之后，跨房间的指派就住在这里——
           指派本来就是"账号 ↔ 房间"这张表上的事，和角色同一页才不用来回找。
           数据直接用外壳每五秒取回的那份房间名单，这一页不另发一次扫盘请求。 -->
      <section class="card" :style="isAdmin ? '' : 'opacity:.55'">
        <div class="card-head">
          <span class="ico-tile tint-green"><x-icon name="house" :size="15"></x-icon></span>
          <h3>房间归属</h3>
          <x-badge v-if="!isAdmin" kind="warn" text="需要管理员"></x-badge>
          <span class="hint">{{ roomRows.length }} 间<template v-if="unassignedCount">，{{ unassignedCount }} 间还没派出去</template></span>
          <x-hint label="这一张管什么" text="一间房归谁管。派给某个成员之后，他在那间房可以自己启停、改世界规则、填令牌；没派出去的房间谁都能看，但只有管理员改得动。指派不会停掉正在跑的世界，也不会改动磁盘上的存档。"></x-hint>
          <span class="grow"></span>
        </div>

        <x-note v-if="!isAdmin" kind="warn">房间归属可以看得见，但指派只有管理员能做。需要管理员。</x-note>

        <x-empty v-if="!roomRows.length" icon="house" title="这台机器上还没有房间"
                 note="面板只看这台机器上的存档文件夹，一个都没有时先去开服向导建一间。">
          <a class="btn primary sm" href="#/room">去开服向导</a>
        </x-empty>

        <div class="table-wrap hide-sm" v-if="roomRows.length">
          <table class="data">
            <thead><tr><th>房间</th><th>归属</th><th>操作</th></tr></thead>
            <tbody>
              <tr v-for="c in roomRows" :key="c.name">
                <td>
                  <div class="col" style="gap:2px">
                    <b>{{ c.roomName || c.name }}</b>
                    <span class="muted mono" v-if="c.roomName && c.roomName !== c.name">{{ c.name }}</span>
                  </div>
                </td>
                <td>
                  <span class="chip" :class="ownerTone(c)" :title="roomHint(c)">
                    <x-icon :name="ownerIcon(c)" :size="11"></x-icon>
                    <span>{{ c.ownerLabel || "读不到归属" }}</span></span>
                </td>
                <td>
                  <button class="btn sm ghost" :disabled="!canWrite || !canAssign"
                          :title="canWrite ? '' : writeNote"
                          @click="openOwner(c.name, c.roomName || c.name)">指派</button>
                  <span class="hint" v-if="!canWrite">需要管理员账号才能指派归属</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <div class="col only-sm" style="gap:8px" v-if="roomRows.length">
          <div class="mod-row" v-for="c in roomRows" :key="'m' + c.name">
            <div class="col mod-main" style="gap:6px">
              <div class="row" style="gap:8px;flex-wrap:wrap">
                <b>{{ c.roomName || c.name }}</b>
                <span class="chip" :class="ownerTone(c)" :title="roomHint(c)">
                  <x-icon :name="ownerIcon(c)" :size="11"></x-icon>
                  <span>{{ c.ownerLabel || "读不到归属" }}</span></span>
                <span class="grow"></span>
                <button class="btn sm ghost" :disabled="!canWrite || !canAssign"
                        :title="canWrite ? '' : writeNote"
                        @click="openOwner(c.name, c.roomName || c.name)">指派</button>
                <span class="hint" v-if="!canWrite">需要管理员账号才能指派归属</span>
              </div>
              <div class="hint" v-if="c.roomName && c.roomName !== c.name">存档目录名 {{ c.name }}</div>
            </div>
          </div>
        </div>
      </section>

      <section class="card">
        <div class="card-head">
          <span class="ico-tile tint-gold"><x-icon name="lock" :size="15"></x-icon></span>
          <h3>修改我的密码</h3>
          <span class="hint">当前账号：{{ me.username }} · {{ me.roleLabel }}</span>
        </div>
        <x-note v-if="me.usingDefaultPassword" kind="warn" class="mb">
          这个账号还在用<strong>出厂默认密码</strong>：面板开在公网端口上时任何人都能试出这一组，建议现在就改。</x-note>
        <div class="grid-form">
          <div class="field">
            <label for="self-old">旧密码</label>
            <input id="self-old" type="password" v-model="selfForm.oldPassword" autocomplete="current-password">
          </div>
          <div class="field">
            <label for="self-new">新密码</label>
            <input id="self-new" type="password" v-model="selfForm.newPassword" autocomplete="new-password"
                   placeholder="至少 5 个字符">
            <div class="hint warn" v-if="passHint(selfForm.newPassword)">
              {{ passHint(selfForm.newPassword) }}</div>
          </div>
          <div class="field">
            <label for="self-repeat">再输一次新密码</label>
            <input id="self-repeat" type="password" v-model="selfForm.repeat" autocomplete="new-password"
                   placeholder="和新密码一致">
          </div>
        </div>
        <div class="row mt" style="gap:8px;align-items:center">
          <button class="btn primary" :disabled="savingSelf" @click="saveSelf">
            {{ savingSelf ? "正在修改……" : "修改密码" }}</button>
          <x-hint label="改完会发生什么" text="改完会自动退出，用新密码重新登录；其他设备上的旧凭据也会失效。"></x-hint>
        </div>
      </section>
    </div>`,
};
