// 「指派归属」弹窗：决定一间房归哪个账号管。
//
// 为什么单独一个弹窗，而不是在房间行上放一个下拉框直接选：
//   1. 这件事改的不是房间，而是"这台机器怎么分工"。点错一下就把一间正在跑的房
//      派给别人，成员那边当场就看不见它了，所以必须有一张卡让人先看清现状再按。
//   2. 名单不是"所有账号"：管理员和只读账号被后端明确拒掉（409）。
//      下拉框里只摆收得下的那几项，并把为什么另两类不行说在旁边——
//      留一个按下去报红的选项，等于让人替我们读一遍后端注释。
//   3. 取消指派也是这个弹窗的动作（选「不派给谁」）。它和派给某人的后果相反：
//      房间从某个成员的所有页面里消失，变回"人人看得见、只有管理员改得动"。
//
// 当前归属从 /clusters 那一条带出来（ownerName / ownerAccountId / assignedNote），
// 这里不再单独发一次读请求：弹窗只在管理员手里开，而他已经把整份房间名单拿到了。
import * as api from "./api.js";
import { state, act, assignedText } from "./store.js";
import { go } from "./router.js";

const { reactive } = Vue;

export const ownerBox = reactive({
  open: false, cluster: "", roomName: "",
  loading: false, error: "",
  accounts: [],        // 接口给的完整账号名单，挑得出角色的地方都用它
  current: null,       // 打开那一刻这间的归属（来自 state.clusters）
  pick: 0,             // 下拉框的值：账号 id，0 = 不派给谁
  saving: false,
});

export async function openOwner(cluster, roomName) {
  const c = state.clusters.find((x) => x.name === cluster) || null;
  ownerBox.open = true;
  ownerBox.cluster = cluster;
  ownerBox.roomName = (c && c.roomName) || roomName || cluster;
  ownerBox.loading = true;
  ownerBox.error = "";
  ownerBox.accounts = [];
  ownerBox.current = c;
  ownerBox.pick = c ? Number(c.ownerAccountId) || 0 : 0;
  ownerBox.saving = false;

  const res = await api.get("/api/v1/accounts");
  // 和加入命令那个弹窗同一个防护：请求回来时用户可能已经关掉了它，
  // 或者已经换开另一间房的弹窗，晚到的这一份不能再改状态。
  if (!ownerBox.open || ownerBox.cluster !== cluster) return;
  ownerBox.loading = false;
  if (!res.ok) {
    ownerBox.error = res.error || ("接口返回 " + res.status);
    return;
  }
  ownerBox.accounts = res.data || [];
}

export function closeOwner() {
  ownerBox.open = false;
}

export const OwnerDialog = {
  setup() { return { ownerBox, state, closeOwner }; },
  computed: {
    // 能当负责人的只有"房间成员"。判断读的是后端给的 role，
    // 界面上摆出来的永远是 roleLabel，不出现 member 这种码值。
    candidates() {
      return ownerBox.accounts.filter((a) => a.role === "member");
    },
    // 另两类账号各有多少，用来把那句话说成实际的数，而不是空泛的规则。
    adminCount() { return ownerBox.accounts.filter((a) => a.role === "admin").length; },
    viewerCount() { return ownerBox.accounts.filter((a) => a.role === "viewer").length; },
    // 当前派给了谁：优先用弹窗打开那一刻从 /clusters 带来的说法；
    // 账号刚被删掉时归属表里的名字可能是空的，那句由后端负责说清（见 ownerTitle）。
    nowLabel() { return (ownerBox.current && ownerBox.current.ownerLabel) || "未指派"; },
    // "由谁指派"那一句要带时刻，而时刻得按浏览器所在时区格式化（assignedText 在 store.js）。
    // 模板只能碰到实例上的名字，所以这里过一道 computed。
    assignedLine() { return assignedText(ownerBox.current); },
    changed() { return ownerBox.pick !== ((ownerBox.current && Number(ownerBox.current.ownerAccountId)) || 0); },
    // 保存那一句到底会发生什么，按目标分两种说法：派出去 / 收回来。
    // 措辞必须和 scope.go 那套判断同一条：成员的可见名单是"我自己名下的 + 未指派的"，
    // 所以派一间房并不等于"他只能看这一间"，说成那样会让他以为自己看不见别的房是出了 bug。
    effectNote() {
      if (ownerBox.pick === 0) {
        return "这间房回到「未指派」：由管理员自己管。每个成员都还能在列表里看到它的状态，" +
          "但没有任何成员能改它——未指派的房间只有管理员动得了。";
      }
      const a = this.candidates.find((x) => x.id === ownerBox.pick);
      const name = a ? a.username : "对方";
      return "派给 " + name + " 之后：这间房进他的房间名单，他能启停世界、改配置、做备份；" +
        "他自己名下的房间照常都在，未指派的房间他也还是看得见、但改不动。";
    },
  },
  methods: {
    goAccounts() { closeOwner(); go("accounts"); },
    async save() {
      if (!this.changed || ownerBox.saving) return;
      ownerBox.saving = true;
      const res = await act("cluster.owner:" + ownerBox.cluster, "PUT",
        "/api/v1/clusters/" + encodeURIComponent(ownerBox.cluster) + "/owner",
        { accountId: ownerBox.pick }, {
          okTitle: ownerBox.pick === 0 ? "已取消指派" : "已指派",
          okBody: (d) => (d && d.ownerLabel) || "",
          badTitle: "这一步没成",
        });
      ownerBox.saving = false;
      // 只有接口收下才关窗：被 409 拒掉时（账号刚被人改了角色）留在原地，
      // 用户看得见自己刚才选的是哪一项。act 已经把失败原因弹成 Toast 了。
      if (res.ok) closeOwner();
    },
  },
  template: `
    <div class="mask" v-if="ownerBox.open" @click.self="closeOwner()">
      <div class="dialog owner-dialog" role="dialog" aria-modal="true">
        <div class="row spread owner-head">
          <h3><x-icon name="user" :size="16"></x-icon> 指派「{{ ownerBox.roomName }}」</h3>
          <button class="icon-btn" aria-label="关闭" @click="closeOwner()"><x-icon name="close" :size="16"></x-icon></button>
        </div>
        <p class="muted owner-lede">现在：<b>{{ nowLabel }}</b>
          <span v-if="assignedLine"> · {{ assignedLine }}</span></p>

        <x-sk v-if="ownerBox.loading" :lines="2"></x-sk>
        <div class="err-box" v-else-if="ownerBox.error">{{ ownerBox.error }}</div>

        <template v-else>
          <div class="field" v-if="candidates.length">
            <label for="owner-pick">交给谁管</label>
            <select id="owner-pick" v-model.number="ownerBox.pick">
              <option :value="0">不派给谁（未指派，由管理员自己管）</option>
              <option v-for="a in candidates" :key="a.id" :value="a.id">
                {{ a.username }}（{{ a.roleLabel }}{{ a.ownedLabel ? " · " + a.ownedLabel : "" }}）
              </option>
            </select>
          </div>

          <x-note v-else kind="warn">
            现在还没有能当负责人的账号：指派只接受「房间成员」，
            而这台机器上的账号是 {{ adminCount }} 个管理员、{{ viewerCount }} 个只读。
            <div class="row mt-0" style="gap:8px;align-items:center">
              <button class="btn sm primary" @click="goAccounts">去账号管理建一个</button>
              <span class="muted">建的时候角色选「房间成员」。</span>
            </div>
          </x-note>

          <p class="muted owner-rule" v-if="candidates.length">
            管理员不负责单间房（他管的本来就是所有房间），只读账号一点修改都做不了，
            所以这两类不在这个名单里。</p>

          <x-note v-if="changed" kind="info">{{ effectNote }}</x-note>
          <p class="muted" v-else-if="candidates.length">
            还是这一项，按下去不会有任何变化。</p>
        </template>

        <div class="dialog-foot">
          <button class="btn ghost" @click="closeOwner()">关闭</button>
          <button class="btn primary" :disabled="ownerBox.saving || !changed || !candidates.length"
                  @click="save">{{ ownerBox.saving ? "正在写……" : "保存指派" }}</button>
        </div>
      </div>
    </div>`,
};
