// 外壳：左导航、顶栏、手机端底部导航与"更多"抽屉、主题开关、登录层、Toast 与确认框的挂载点。
//
// 登录层之所以画在这里而不是"某个页面"：面板的认证是 HTTP Basic，服务端不存会话，
// 凭据只在内存与 sessionStorage 里。任何一个接口回 401 都要能把整个界面换成登录层，
// 放在页面级就只能每个页面各自处理"没权限"。
//
// 这一版还负责一件事：把当前页的色相写到 documentElement 的 --page-accent 上。
// 色相只记在 nav.js 那条表里，这里只搬运，不再另抄一份——
// 否则加一个页面就要改两处，迟早出现"标题是紫的、卡片左沿是蓝的"。
import { state, login, logout, setTheme, refreshAll, gauges, rel,
  firstQuietCluster, quietShard, toggleNoticeCenter } from "./store.js";
import { route, current, currentView, go } from "./router.js";
import { navPages, navGroups, tabPages, badgeOf } from "./nav.js";
import { ConnectDialog } from "./connect.js";
import { OwnerDialog } from "./owner.js";
// 待办条目只在 pending.js 里定义一份。顶栏和总览「下一步」都调 checklistRows
// 拿同一份清单，所以顶栏这枚数字与卡头「N 项待办」永远是同一个数。
import { checklistRows, pendingSummary } from "./pending.js";

const { watch } = Vue;

const Login = {
  // state 必须由 setup 暴露：Vue 的模板只认这个组件自己的实例属性，
  // 直接写 state.loginError 会拿到 undefined（实测第一次打开就白在这行）。
  setup() {
    return { state, setTheme };
  },
  data() {
    return { user: "", pass: "", remember: true, showPass: false };
  },
  methods: {
    async submit() {
      if (!this.user || !this.pass) {
        state.loginError = "用户名和密码都要填。";
        return;
      }
      const ok = await login(this.user, this.pass, this.remember);
      if (ok) this.pass = "";
    },
  },
  template: `
    <div class="login">
      <div class="login-bg" aria-hidden="true"></div>
      <div class="login-shell">
        <section class="login-hero">
          <div class="login-brand">
            <span class="brand-mark lg" aria-hidden="true"><x-icon name="world" :size="22"></x-icon></span>
            <span class="login-brand-text">
              <span class="login-brand-name">饥荒联机版</span>
              <span class="login-brand-sub">服务器面板</span>
            </span>
          </div>
          <h1 class="login-slogan">开服、看人、管世界，<br>都在一个页面里。</h1>
          <p class="login-lede">直接在浏览器里操作这台机器上的饥荒服务端：启停、模组、存档、玩家和世界地图，不用再登 SSH 敲命令。</p>
          <ul class="login-points">
            <li><span class="lp-ico lp-a" aria-hidden="true"><x-icon name="servers" :size="15"></x-icon></span>
              <span><b>房间与世界</b>：进程、端口、游戏里传回来的数据，掉线马上能看出来。</span></li>
            <li><span class="lp-ico lp-b" aria-hidden="true"><x-icon name="map" :size="15"></x-icon></span>
              <span><b>世界地图</b>：地形、玩家位置、建筑与资源分布。</span></li>
            <li><span class="lp-ico lp-c" aria-hidden="true"><x-icon name="users" :size="15"></x-icon></span>
              <span><b>玩家管理</b>：管理员、禁言、拉黑与白名单，在线离线都能处理。</span></li>
          </ul>
          <div class="login-foot">面板只连你自己的服务器 · 登录凭据不会写入磁盘</div>
        </section>

        <section class="login-card">
          <div class="login-card-head">
            <h2>登录面板</h2>
            <div class="theme-switch" role="group" aria-label="配色">
              <button type="button" :class="state.theme === 'dark' ? 'on' : ''" @click="setTheme('dark')">
                <x-icon name="moon" :size="14"></x-icon><span>深色</span></button>
              <button type="button" :class="state.theme === 'light' ? 'on' : ''" @click="setTheme('light')">
                <x-icon name="sun" :size="14"></x-icon><span>浅色</span></button>
            </div>
          </div>
          <p class="muted login-hint">首次登录用部署时写下的管理员账号，出厂默认值见 README。
            <x-hint text="只要还在用出厂默认密码，总览「下一步」就会列出这一条、顶栏「待办」入口也一直在——面板直接开在公网端口上，默认密码等于门没锁。"></x-hint></p>
          <form class="login-form" @submit.prevent="submit">
            <div class="note warn" v-if="state.sessionExpired" style="margin-bottom:12px">
              登录状态失效了（密码被改过或面板刚重启）。重新登录后回到刚才那一页，已填的内容还在。
            </div>
            <div class="field">
              <label for="lg-user">管理员账号</label>
              <input id="lg-user" type="text" v-model.trim="user" autocomplete="username" placeholder="账号">
            </div>
            <div class="field">
              <label for="lg-pass">密码</label>
              <div class="login-pass">
                <input id="lg-pass" :type="showPass ? 'text' : 'password'" v-model="pass"
                       autocomplete="current-password" placeholder="密码">
                <button type="button" class="login-eye" @click="showPass = !showPass"
                        :aria-label="showPass ? '隐藏密码' : '显示密码'">
                  <x-icon :name="showPass ? 'eyeoff' : 'eye'" :size="16"></x-icon></button>
              </div>
            </div>
            <label class="switch">
              <input type="checkbox" v-model="remember"><span class="track"></span>
              <span>这个标签页内记住（关掉就清）</span>
            </label>
            <div class="err-box" v-if="state.loginError">{{ state.loginError }}</div>
            <button class="btn primary block login-submit" type="submit" :disabled="state.loggingIn">
              {{ state.loggingIn ? "正在核对……" : "登录" }}</button>
          </form>
          <div class="login-card-foot">认证走 HTTP Basic，面板不存会话；凭据只在这个标签页里，关掉标签页就失效。对外访问请套 HTTPS。</div>
        </section>
      </div>
    </div>`,
};

export const Shell = {
  components: { "x-login": Login, "x-connect": ConnectDialog, "x-owner": OwnerDialog },
  setup() {
    // 换页时把这一页的色相搬到一个 CSS 变量上，别的所有颜色都由 CSS 从它派生。
    // 没 hue（登录层、notFound）就把它撤掉，退回 :root 里那条全局主色。
    const applyHue = () => {
      const p = current();
      const el = document.documentElement;
      if (p && p.hue) el.style.setProperty("--page-accent", p.hue);
      else el.style.removeProperty("--page-accent");
    };
    watch(() => route.name, applyHue, { immediate: true });
    return {
      state, route, gauges, rel, navPages, navGroups, tabPages, badgeOf, current, currentView, go,
      setTheme, refreshAll, logout, toggleNoticeCenter,
    };
  },
  data() {
    return { more: false };
  },
  computed: {
    page() { return current(); },
    // 没数据可显示时顶栏那几个计数说"0/0"会比空着更误导：
    // 它看着像"问过了、确实一个都没在跑"，而实际是还没问到或问失败了。
    haveData() { return state.clustersLoaded; },
    tabs() { return tabPages(); },
    // 「助手没回话」点过去只切一间房，所以标题要先把话说清：这 N 个世界分布在 K 间房里，
    // 点进去落在第一间，其余几间看房签上那颗点和世界行上那枚徽标。
    quietRooms() {
      return state.clusters.filter((c) => (c.shards || []).some(quietShard)).length;
    },
    // "更多"上那个数字：抽屉里那些页面各自的任务数合计。
    // 手机上底部栏只有五格，未结束任务不单独占一格，但它也不能完全看不见。
    moreBadge() {
      return navPages().filter((p) => !p.tab)
        .reduce((n, p) => n + badgeOf(p), 0);
    },
    // 待办只在这一个地方说清：总览「下一步」那张卡。别的页不该在正文顶部
    // 再铺一块橙色的提醒（2026-11-09 用户："总览页面，下一步的卡片已经展示了这些信息，
    // 其他页面还需要如图这个吗？看上去很丑"）。
    // 第四轮用户："顶栏的代办……我还是希望能显示数字，他的口径和卡片栏里的待办一致即可。"
    // 因此这里不再自己数 /session 那两条，直接读与总览卡同一份 checklistRows，
    // 数字、摘要、跳转落点三件事全跟着那一份走。
    checklist() { return checklistRows(state, state.checklist.data); },
    pendingCount() { return this.checklist.length; },
    pendingTip() {
      if (!this.pendingCount) return "";
      return "有 " + this.pendingCount + " 项待办：" + pendingSummary(this.checklist) +
        "。点这里去总览「下一步」看清单。";
    },
    // 抽屉里把状态用整句写一遍：顶部那排仪表在手机上只剩图标和数字，
    // 想确认"这个数是什么意思"就来这里，不用回桌面端看。
    // 手机上顶栏的身份牌也收在这儿——那一行留给标题，标题被省略号截断最伤。
    statusRows() {
      const me = state.me ? state.me.username + " · " + state.me.roleLabel : "未知";
      return [
        { k: "登录身份", v: me },
        { k: "世界在跑", v: this.gauges.running + " / " + this.gauges.total, mono: true },
        { k: "助手没回话", v: String(this.gauges.offline), mono: true },
        { k: "未结束任务", v: String(this.gauges.live), mono: true },
        { k: "上次同步", v: state.lastSync ? rel(state.lastSync) : "还没开始" },
      ];
    },
  },
  watch: {
    // 换页就把抽屉关掉：从抽屉里点进"任务中心"之后再按手机系统的返回，
    // 抽屉还开着会盖在任务中心上面，看着像那一页没打开。
    "route.name"() { this.more = false; },
  },
  methods: {
    closeMore() { this.more = false; },
    toggleTheme() { setTheme(state.theme === "dark" ? "light" : "dark"); },
    // 「所有房间」那一页下掉之后（#129），顶栏这两枚计数的去处：
    // 「在跑」回总览逐间看页签；「助手没回话」额外把当前页签切到有这问题的第一间，
    // 否则点完只是回到自己已经站着的那一屏，等于没给入口。
    toOverview() { location.hash = "#/"; },
    toQuietRoom() {
      const c = firstQuietCluster();
      if (c) state.sel.cluster = c.name;
      location.hash = "#/";
    },
    toTasks() { location.hash = "#/tasks"; },
    // 顶栏「待办」入口的落点：总览「下一步」清单。带上 focus=todo 让那张卡
    // 自己滚进视野并闪一下，否则从别的页跳过去落在页顶，还得自己找那一栏。
    toNextSteps() { location.hash = "#/?focus=todo"; },
  },
  template: `
    <div>
      <div class="boot" v-if="state.checking">
        <div class="boot-bar"><span class="boot-dot"></span>饥荒联机版 服务器面板</div>
        <div><div class="boot-line" style="width:42%"></div>
          <div class="boot-line" style="width:68%"></div>
          <p class="boot-note">正在核对管理员凭据并读取房间列表……</p></div>
      </div>

      <template v-else>
      <!-- 会话过期时 .shell 留在 DOM 里，登录层浮在它上面：填了一半的向导不会被销毁。
           首次登录 / 主动退出则连 .shell 都不挂载，免得页面组件对着 401 反复发请求。 -->
      <div class="login-overlay" v-if="state.needLogin"><x-login></x-login></div>

      <div class="shell" v-if="state.everLoggedIn || !state.needLogin">
        <aside class="sidebar">
          <div class="brand">
            <span class="brand-mark" aria-hidden="true"><x-icon name="world" :size="17"></x-icon></span>
            <span class="brand-text">
              <span class="brand-name">饥荒服务器面板</span>
              <span class="brand-sub">{{ state.clusters.length }} 个房间 · {{ gauges.total }} 个世界</span>
            </span>
          </div>

          <template v-for="g in navGroups()" :key="g.name">
            <div class="nav-sep">{{ g.name }}</div>
            <button v-for="p in g.items" :key="p.name" class="nav-item"
                    :class="route.name === p.name ? 'on' : ''" :style="{ '--page-accent': p.hue }"
                    :title="p.hint" @click="go(p.name)">
              <x-icon :name="p.icon" class="nav-ic"></x-icon>
              <span>{{ p.label }}</span>
              <span class="nav-badge" :class="badgeOf(p) ? 'hot' : ''" v-if="badgeOf(p)">{{ badgeOf(p) }}</span>
            </button>
          </template>

          <div class="sidebar-foot">
            <div class="sf-row" v-if="state.clustersError" style="color:var(--bad)">
              <x-icon name="warn" :size="14"></x-icon>房间列表没拿到</div>
            <div class="sf-row"><x-icon name="clock" :size="14"></x-icon>
              上次同步 {{ state.lastSync ? rel(state.lastSync) : "还没开始" }}</div>
          </div>
        </aside>

        <header class="topbar">
          <div class="topbar-id">
            <span class="ico-tile" aria-hidden="true"><x-icon :name="page ? page.icon : 'dashboard'" :size="17"></x-icon></span>
            <span class="topbar-titles">
              <!-- 12 个导航项分 4 组：标题前标出所在组，切页时不用回侧栏找位置。
                   窄屏顶栏要留给计数仪表，这枚面包屑在手机上收起。 -->
              <span class="crumb hide-sm" v-if="page && page.group">{{ page.group }} ›</span>
              <span class="topbar-title">{{ page ? page.label : "" }}</span>
              <x-hint v-if="page && page.hint" :text="page.hint" icon="info"></x-hint>
            </span>
          </div>
          <div class="gauges">
            <span class="gauge user" v-if="state.me" :title="'当前登录：' + state.me.username">
              <x-icon name="idcard" :size="14"></x-icon>{{ state.me.username }} · {{ state.me.roleLabel }}</span>
            <!-- 待办入口：别的页不再铺一整块提醒，这里一枚带数字的入口就是全部。
                 有条目才出现，数字直接取与总览「下一步」同一份清单的长度，
                 悬停把"都是什么"说完；点进去落在总览那张卡并高亮。
                 排在最前：手机上 gauges 不许换行，靠前才留得住。 -->
            <button class="gauge warn todo" v-if="pendingCount" @click="toNextSteps()"
                    :title="pendingTip" :aria-label="pendingTip">
              <x-icon name="warn" :size="14"></x-icon><span class="hide-sm">待办</span>
              <b>{{ pendingCount }}</b></button>
            <button class="gauge" v-if="haveData" @click="toOverview()"
                    title="进程在跑的世界数：点这里回总览，下面那一排页签逐间给「在跑几个／共几个」">
              <x-icon name="power" :size="14"></x-icon><span class="hide-sm">在跑</span>
              <b>{{ gauges.running }}/{{ gauges.total }}</b></button>
            <button class="gauge warn" v-if="haveData && gauges.offline" @click="toQuietRoom()"
                    :title="'进程在跑但助手 MOD 没回话的世界数：共 ' + gauges.offline + ' 个，分布在 ' + quietRooms +
                            ' 间房里。点这里跳到第一间，其余的在页签上接着看'">
              <x-icon name="signal" :size="14"></x-icon><span class="hide-sm">助手没回话</span>
              <b>{{ gauges.offline }}</b></button>
            <!-- 第三枚原来是个 span：旁边两枚都能点、它不能，而"是哪几个任务还没结束"
                 恰恰只有任务中心答得出。数字挂在一整页内容前面却不给入口，等于让人自己猜。
                 2026-11-09 总览那张「未结束的任务」卡也下掉了，这一枚成了站内唯一入口；
                 原来写在卡头气泡里的口径（只数四类耗时操作、空着不等于机器闲着）挪到悬停，
                 任务中心那一页本身有完整进度、明细和结论，不必在总览再抄一遍。 -->
            <button class="gauge hide-sm" v-if="haveData" @click="toTasks()"
                    title="还在排队或正在执行的面板任务：点这里去任务中心看进度、明细和结论。这个数只收下载/更新服务端本体、备份、恢复、导入这四类耗时操作；模组下载、世界启停不进来，机器忙不忙看旁边「在跑」和总览那排表盘。">
              <x-icon name="clock" :size="14"></x-icon>未结束任务 <b>{{ gauges.live }}</b></button>
            <span class="gauge hide-sm" v-if="!haveData">还没问到房间</span>
          </div>
          <button class="icon-btn" :class="state.refreshing ? 'spinning' : ''" :disabled="state.refreshing"
                  title="立即同步" aria-label="立即同步" @click="refreshAll(true)">
            <x-icon name="refresh" :size="17"></x-icon></button>
          <button class="icon-btn" :title="'切到' + (state.theme === 'dark' ? '浅色' : '深色')"
                  :aria-label="state.theme === 'dark' ? '切到浅色' : '切到深色'" @click="toggleTheme">
            <x-icon :name="state.theme === 'dark' ? 'sun' : 'moon'" :size="17"></x-icon></button>
          <button class="icon-btn" :title="'通知中心（' + state.unread + ' 条未读）'"
                  :aria-label="'打开通知中心，' + state.unread + ' 条未读'" @click="toggleNoticeCenter()">
            <x-icon name="history" :size="17"></x-icon>
            <span class="nav-badge hot" v-if="state.unread">{{ state.unread }}</span></button>
          <button class="icon-btn" title="退出登录" aria-label="退出登录" @click="logout">
            <x-icon name="logout" :size="17"></x-icon></button>
        </header>

        <main class="main">
          <!-- 待办不在这里画（2026-11-09 第二轮，用户："总览页面，下一步的卡片已经展示了
               这些信息，其他页面还需要如图这个吗？看上去很丑"）。上一轮已经把两条横幅
               统一成一份 .step-row 清单，但别的页仍在正文顶部占掉一整块橙色区域：
               摘要行 + 展开列表 + 一句"这几条也在…"，和那一页自己的内容抢首屏，
               而信息本身在总览「下一步」里一条不少。现在整块下掉，
               入口收进顶栏那枚「待办」（390px 档在「更多」抽屉里），点一下带 #/?focus=todo 直达那张卡。 -->
          <component v-if="currentView" :is="currentView" :params="route.params" :query="route.query"></component>
        </main>

        <nav class="tabbar" aria-label="页面切换">
          <button v-for="p in tabs" :key="p.name" class="tab" :class="route.name === p.name ? 'on' : ''"
                  :style="{ '--page-accent': p.hue }" @click="go(p.name)">
            <x-icon :name="p.icon" :size="21"></x-icon>
            <span>{{ p.label }}</span>
            <span class="tab-badge" :class="badgeOf(p) ? 'hot' : ''" v-if="badgeOf(p)">{{ badgeOf(p) }}</span>
          </button>
          <button class="tab" :class="more ? 'on' : ''" @click="more = !more" aria-label="打开其余页面">
            <x-icon name="more" :size="21"></x-icon>
            <span>更多</span>
            <span class="tab-badge hot" v-if="moreBadge">{{ moreBadge }}</span>
          </button>
        </nav>

        <template v-if="more">
          <div class="sheet-mask" @click="closeMore"></div>
          <div class="sheet" role="dialog" aria-modal="true" aria-label="其余页面">
            <div class="sheet-grip" aria-hidden="true"></div>
            <div class="sheet-head">
              <h3>全部页面</h3>
              <button class="icon-btn" aria-label="收起" @click="closeMore"><x-icon name="close" :size="16"></x-icon></button>
            </div>
            <div class="sheet-grid">
              <button v-for="p in navPages()" :key="p.name" class="sheet-item"
                      :class="route.name === p.name ? 'on' : ''" :style="{ '--page-accent': p.hue }"
                      @click="go(p.name)">
                <x-icon :name="p.icon" :size="20"></x-icon>
                <span>{{ p.label }}</span>
                <span class="tab-badge" :class="badgeOf(p) ? 'hot' : ''" v-if="badgeOf(p)">{{ badgeOf(p) }}</span>
              </button>
            </div>
            <div class="sheet-list">
              <x-kv :rows="statusRows"></x-kv>
              <!-- 390px 顶栏本来就挤到刚好，再塞一枚「待办」会把标题压成「开…」、
                   退出登录那枚也会滑出视口。手机上这一枚入口收进抽屉，仍是一键直达：
                   点完关抽屉并带 #/?focus=todo 落在总览那张卡上。 -->
              <button class="sheet-todo" v-if="pendingCount" @click="closeMore(); toNextSteps()">
                <x-icon name="warn" :size="15"></x-icon>
                <span class="grow">待办 {{ pendingCount }} 项 · 去总览「下一步」看清单</span>
              </button>
            </div>
          </div>
        </template>
      </div>
      </template>

      <x-toasts></x-toasts>
      <x-notice-center></x-notice-center>
      <x-undo-bar></x-undo-bar>
      <x-confirm></x-confirm>
      <x-connect></x-connect>
      <!-- 指派归属的弹窗挂在外壳而不是房间页：总览页的房间卡上也要能指派，
           而挂载点跟着某个页面走的话，换一页就把正开着的弹窗关掉。 -->
      <x-owner></x-owner>
    </div>`,
};
