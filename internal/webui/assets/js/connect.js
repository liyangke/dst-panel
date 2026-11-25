// 「加入房间」弹窗：把玩家在游戏控制台里要敲的那一句摆出来，并负责复制它。
//
// 为什么不是一个"点房间名 → 直接进剪贴板"的动作就完事：
//   1. 面板默认跑在明文 HTTP 上，浏览器不一定允许写剪贴板（见 clip.js）。
//      悄悄失败等于让用户拿一条没进剪贴板的命令去粘贴，他只会以为命令是错的。
//   2. 地址是面板从这次请求的 Host 猜出来的。反代或内网穿透时它会猜错，
//      而用户唯一能核对的办法是当场看到"面板听到的地址是哪个"。
//   3. 端口每个房间不一样，命令拼不出来时（没写端口 / 这个服还没开）必须说明原因。
// 所以点房间名打开的是这一张卡：命令在中间，来源和警告在下面，复制失败也不会白按。
import * as api from "./api.js";
import { copyText } from "./clip.js";
import { placeName } from "./gameinfo.js";

const { reactive } = Vue;

export const connectBox = reactive({
  open: false, cluster: "", roomName: "",
  loading: false, error: "", view: null,
  // copied 记的是"上一次的复制结果"，只有 ok/none 两态：
  // 按钮文案跟着变，用户才知道剪贴板到底进去没有。
  copied: false,
  // copyFailed 单独一态：写剪贴板被浏览器拒绝时，"已复制"那三个字是假话，
  // 而用户手上正拿着一条没进剪贴板的命令去游戏里粘贴。
  copyFailed: false,
});

export async function openConnect(cluster, roomName) {
  connectBox.open = true;
  connectBox.cluster = cluster;
  connectBox.roomName = roomName || cluster;
  connectBox.loading = true;
  connectBox.error = "";
  connectBox.view = null;
  connectBox.copied = false;
  connectBox.copyFailed = false;

  const res = await api.get("/api/v1/clusters/" + encodeURIComponent(cluster) + "/connect");
  // 弹窗期间用户可能已经关掉了它：晚到的那份不能再改状态，否则会盖掉下一次打开。
  if (!connectBox.open || connectBox.cluster !== cluster) return;
  connectBox.loading = false;
  if (!res.ok) {
    connectBox.error = res.error || ("接口返回 " + res.status);
    return;
  }
  connectBox.view = res.data;
}

export function closeConnect() {
  connectBox.open = false;
}

export const ConnectDialog = {
  setup() { return { connectBox, closeConnect, placeName }; },
  data() { return { busy: false }; },
  computed: {
    title() { return "加入「" + (this.connectBox.view ? this.connectBox.view.roomName : this.connectBox.roomName) + "」"; },
    rows() {
      const v = this.connectBox.view;
      if (!v) return [];
      const out = [
        { k: "游戏端口", v: v.port > 0 ? String(v.port) : "没写在配置里", mono: true },
        { k: "连的是", v: placeName(v.shard) + "（" + v.shard + "）" },
        { k: "地址怎么来的", v: v.source },
      ];
      return out;
    },
  },
  methods: {
    async copy() {
      const v = this.connectBox.view;
      if (!v || !v.command || this.busy) return;
      this.busy = true;
      const ok = await copyText(v.command);
      this.busy = false;
      this.connectBox.copied = ok;
      this.connectBox.copyFailed = !ok;
      // 复制结果只写在弹窗里（copyFailed / copied 那两行）：Toast 会自己消失，
      // 用户回来时只看得到"已复制"那种旧文案，反而是谎报。
    },
  },
  template: `
    <div class="mask" v-if="connectBox.open" @click.self="closeConnect()">
      <div class="dialog connect-dialog" role="dialog" aria-modal="true">
        <div class="row spread connect-head">
          <h3>{{ title }}</h3>
          <button class="icon-btn" aria-label="关闭" @click="closeConnect()"><x-icon name="close" :size="16"></x-icon></button>
        </div>

        <!-- 那个键名写成 HTML 实体：这一段整个在 JS 模板字符串里，直接放一个反引号会提前结束字符串。
             后面补一句"波浪号键"：反引号这个字符在等宽字里就一个小点，只画键帽没人认得出按哪个。 -->
        <p class="muted connect-lede">在游戏里按 <kbd class="tick">&#96;</kbd>（波浪号键）打开控制台，把下面这句粘进去，回车。</p>

        <x-sk v-if="connectBox.loading" :lines="2"></x-sk>
        <div class="err-box" v-else-if="connectBox.error">{{ connectBox.error }}</div>

        <template v-else-if="connectBox.view">
          <div class="cmd-row" v-if="connectBox.view.command">
            <code class="cmd mono">{{ connectBox.view.command }}</code>
            <button class="btn primary sm" :disabled="busy" @click="copy()">
              <x-icon :name="connectBox.copied ? 'check' : 'copy'" :size="14"></x-icon>
              <span>{{ busy ? '正在写……' : (connectBox.copied ? '已复制' : '复制这句') }}</span>
            </button>
          </div>
          <div class="err-box" v-else>这句命令拼不出来，先把下面的原因解决掉。</div>

          <p class="muted" v-if="connectBox.copied">命令已经在剪贴板里了。关掉这个窗口也不会丢，可以直接切到游戏粘贴。</p>
          <p class="muted is-warn" v-if="connectBox.copyFailed">这个浏览器不让面板写剪贴板。请在上面那句上双击选中，再按 Ctrl+C。</p>

          <x-note v-if="connectBox.view.note" kind="warn">{{ connectBox.view.note }}</x-note>

          <dl class="kv connect-kv">
            <dt>地址</dt>
            <dd class="mono">{{ connectBox.view.address || "没带上地址" }}</dd>
            <template v-for="r in rows" :key="r.k">
              <dt>{{ r.k }}</dt>
              <dd :class="r.mono ? 'mono' : ''">{{ r.v }}</dd>
            </template>
          </dl>
        </template>

        <div class="dialog-foot">
          <button class="btn ghost" @click="closeConnect()">关闭</button>
        </div>
      </div>
    </div>`,
};
