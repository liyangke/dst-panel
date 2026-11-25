// 全局状态：房间列表、每个分片的状态快照、任务概览、主题、Toast、确认框。
//
// 为什么集中在一处而不是每个页面自己 fetch：顶栏要显示"几个在跑 / 助手掉线几个 /
// 几个任务没结束"，总览页和后面的模组页都要用同一批房间数据；分散拉取会出现
// 同一个房间在两个卡片里状态不一致（一个刚刷新、一个是三分钟前的）。
//
// 轮询失败一律不弹 Toast，只把错误留在页面内联的位置：面板挂了的时候
// 每五秒弹一条会刷满屏幕，而用户真正需要的是那句"连不上面板接口"。
import * as api from "./api.js";
import { keyOf, ago, dateTime } from "./fmt.js";

const { reactive, computed } = Vue;

export const POLL_MS = 5000;

export const state = reactive({
  checking: true,        // 第一次问接口还没回来（这时不该显示登录框）
  needLogin: false,
  // 已经成功进来过一次：会话过期时用它决定"登录层浮在现有内容之上"，
  // 而不是把 .shell 整个销毁、丢掉用户填了一半的表单。
  everLoggedIn: false,
  loginError: "",
  loggingIn: false,

  theme: "dark",

  // 当前登录身份：username / role / roleLabel / isAdmin。
  // 界面靠它决定"账号管理"是给列表还是只给改密码；不要在页面里硬编码管理员判断。
  me: null,

  clusters: [],
  clustersError: "",
  clustersLoaded: false,
  refreshing: false,

  // statuses["Cluster_1/Master"] = { loading, data, error, at }
  statuses: {},
  // busy 按调用点给的键位（"start:Cluster_1/Master"）记按钮的进行中状态。
  busy: {},

  tasks: [],
  tasksError: "",

  // host 是这台机器的运行状态（CPU/内存/磁盘/网络/负载）；hostByPid 按进程号
  // 存每个在跑分片的占用，房间行直接按 pid 取。
  host: null,
  hostError: "",
  hostByPid: {},

  sel: { cluster: "" },
  toasts: [],
  // 通知中心：Toast 是"看一眼就走"，这里留一份可回看的历史（最多 60 条）。
  // 失败提示 7/14 秒就消失，用户回头想找"刚才那句报错说了什么"时无处可查。
  notices: [],
  noticeOpen: false,
  unread: 0,
  // 短时撤销：破坏性动作成功后给一条 12 秒的回退入口。
  undo: null,
  // 实时流状态：connected / connecting / reconnecting / error / closed / idle。
  stream: { status: "idle", error: "", at: "", attempt: 0 },
  // 会话过期和"第一次登录"是两回事，登录层的文案要分开说。
  sessionExpired: false,
  confirm: null,
  lastSync: "",
  // tick 每秒加一：界面上的"32 秒前"要自己往前走，不能等下一次轮询才跳。
  tick: 0,
  // 「下一步」清单的共用快照：总览那张卡与顶栏「待办」的数字都从它算，
  // 所以两处不会再各数一份、对不上。/api/v1/rooms 是一次磁盘扫描，
  // 由 loadChecklist 每四次轮询取一回，不跟着五秒轮询跑。
  checklist: { loading: false, error: "", data: null, at: "", seq: 0, tick: 0 },
});

const THEME_KEY = "dstpanel.theme";

export function initTheme() {
  let t = localStorage.getItem(THEME_KEY);
  if (t !== "dark" && t !== "light") {
    // 没选过就跟着系统：面板常年在第二块屏幕上开着，跟着系统色至少不会出现
    // "白天一块亮屏刺眼、晚上这块还是黑的"。
    t = window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  }
  applyTheme(t);
}

function applyTheme(t) {
  state.theme = t;
  document.documentElement.dataset.theme = t;
}

export function setTheme(t) {
  applyTheme(t);
  localStorage.setItem(THEME_KEY, t);
}

// rel 包一层 fmt.ago：读一下 state.tick 让模板把这条文本记成有依赖，
// 秒针走过一秒就会重算。不这么写的话"32 秒前"会停在第一次渲染的那个数上。
export function rel(v) {
  void state.tick;
  return ago(v);
}

let toastSeq = 0, noticeSeq = 0;

// toast 弹一条。kind 用 ok/bad/warn/info，和界面上的状态色同源。
export function toast(kind, title, body) {
  const id = ++toastSeq;
  state.toasts.push({ id, kind: kind || "info", title, body: body || "" });
  pushNotice(kind, title, body);
  // 出错的信息要留久一点：用户常常是先去看接口返回什么，再回头找这条提示。
  setTimeout(() => dropToast(id), kind === "bad" ? 14000 : 7000);
}

export function dropToast(id) {
  const i = state.toasts.findIndex((t) => t.id === id);
  if (i >= 0) state.toasts.splice(i, 1);
}

// 通知中心留的是"已经发生过什么"，不是又一份弹窗。
// 失败提示尤其要留档：用户切走一页再回来，7 秒的 Toast 早就没了。
export function pushNotice(kind, title, body) {
  state.notices.unshift({ id: ++noticeSeq, kind: kind || "info", title,
    body: body || "", at: new Date().toISOString() });
  if (state.notices.length > 60) state.notices.length = 60;
  if (!state.noticeOpen) state.unread = Math.min(60, state.unread + 1);
}

export function clearNotices() {
  state.notices = [];
  state.unread = 0;
}

export function toggleNoticeCenter(open) {
  state.noticeOpen = open === undefined ? !state.noticeOpen : !!open;
  if (state.noticeOpen) state.unread = 0;
}

// SSE/实时流状态由订阅方写，界面上给一块显式「已断开 · 点击重连」的牌。
export function setStreamState(status, info) {
  state.stream.status = status;
  state.stream.error = (info && info.error) || "";
  state.stream.attempt = (info && info.attempt) || 0;
  state.stream.at = new Date().toISOString();
}

// 短时撤销：把"怎么退回去"作为一个闭包挂在全局，不要求每个页面各存一份。
// 12 秒是刻意的：够手滑之后立刻反悔，又不会让一条覆盖整页的浮条一直挡着。
let undoTimer = null;
export function pushUndo(label, run, seconds) {
  if (undoTimer) clearTimeout(undoTimer);
  state.undo = { label: label || "撤销刚才这一步", run, busy: false };
  undoTimer = setTimeout(() => { state.undo = null; undoTimer = null; }, (seconds || 12) * 1000);
}

export function dismissUndo() {
  if (undoTimer) clearTimeout(undoTimer);
  undoTimer = null;
  state.undo = null;
}

export async function runUndo() {
  const u = state.undo;
  if (!u || u.busy) return;
  u.busy = true;
  try {
    await u.run();
    dismissUndo();
    toast("ok", "已撤销", u.label + " 已经退回去了。");
  } catch (e) {
    toast("bad", "撤销没成", (e && e.message) || String(e));
  } finally {
    u.busy = false;
  }
}

// askConfirm 是写操作前唯一的确认入口。返回 Promise<boolean>。
//
// 为什么不用 window.confirm：它的文案不能带上下文，而面板里最要紧的几句
// 恰好是上下文——"会先让 Master、Caves 存盘再打包""整棵替换存档目录"。
// 用户点"确定"时看到的必须是那句话，否则这个确认框只是在训练人盲点。
export function askConfirm(opts) {
  return new Promise((resolve) => {
    state.confirm = { ...opts, resolve };
  });
}

export function answerConfirm(ok) {
  const c = state.confirm;
  state.confirm = null;
  if (c && c.resolve) c.resolve(!!ok);
}

// open 是"还没结束"那两种状态。顶栏的计数和"本体更新刚结束"的判断用同一把尺子，
// 免得一处把 canceled 算成还在跑、另一处不算。
const open = { queued: true, running: true };

// "助手该回话却没回话"的那一个世界。只在"进程在跑、助手没回话、而且这次读不到是真故障"
// 时算：没跑的分片本来就不该回话，没人时引擎自己冻住世界也会让助手停摆，
// 那两种都算进来的话，顶栏那枚会常年挂着个数字，点过去却发现什么都没坏。
// 这条判据只写一遍：计数和"点进来落哪一间"分家的话，数字说 2 而落地是一间好房间。
export const quietShard = (s) => s.running && !s.helperOnline && s.helperNoteUrgent;

export const gauges = computed(() => {
  let running = 0, total = 0, offline = 0;
  state.clusters.forEach((c) => (c.shards || []).forEach((s) => {
    total++;
    if (s.running) running++;
    if (quietShard(s)) offline++;
  }));
  const live = state.tasks.filter((t) => open[t.status] !== undefined).length;
  return { running, total, offline, live };
});

// 顶栏「助手没回话」那枚点进来的落点：第一间有这种世界的房。
// 「所有房间」那一页下掉之后（#129），逐间筛没了，这枚计数只能靠"把页签切过去"给入口。
export function firstQuietCluster() {
  return state.clusters.find((c) => (c.shards || []).some(quietShard)) || null;
}

// selected 是当前选中的房间对象（可能为 null）。
export const selected = computed(() =>
  state.clusters.find((c) => c.name === state.sel.cluster) || state.clusters[0] || null);

export async function login(user, pass, remember) {
  state.loggingIn = true;
  state.loginError = "";
  api.setCredentials(user, pass, remember);
  const res = await api.get("/api/v1/clusters");
  state.loggingIn = false;
  if (!res.ok) {
    if (res.status === 401) state.loginError = res.error || "管理员账号或密码不对";
    else if (res.status === 503) state.loginError = res.error || "面板还没配好管理员密码";
    else if (res.status === 0) state.loginError = res.error || "连不上面板接口";
    else state.loginError = res.error || ("接口返回 " + res.status);
    // 503 说明账号是对的（服务端已经放行到业务层），只是这台面板缺配置：
    // 那种情况下不能把凭据清掉，否则用户每次刷新都要重输。
    if (res.status !== 401) api.setCredentials(user, pass, remember);
    return false;
  }
  state.needLogin = false;
  state.everLoggedIn = true;
  state.sessionExpired = false;
  state.loginError = "";
  applyClusters(res.data);
  await loadSession();
  // 登录成功这一刻起才开始轮询。不在这儿起的话，靠"打开标签页时 boot 认出存下来的凭据"
  // 那条路走一遍才知道要轮：手动输完账号密码进来的人，页面停在登录那一次拿到的数字上，
  // 任务百分比不动、在线人数也不动，只能靠刷新标签页——实测就是这样卡住的。
  startPolling();
  return true;
}

// loadSession 拉当前登录身份。失败不挡登录流程：老版本接口没有这一条时，
// 界面只是不显示账号页的管理按钮，而不是整页卡住。
export async function loadSession() {
  const res = await api.get("/api/v1/session");
  if (res.ok) {
    state.me = res.data || null;
    // 身份一确定就能算清单：顶栏那枚数字和总览「下一步」都要它，
    // 不能等到第四次轮询（约 20 秒后）才出现。
    void loadChecklist();
  }
  return res.ok;
}

// 「下一步」清单的取数：全局只跑这一处，顶栏和总览读同一份快照。
// loading 时直接跳过：登录完成触发一次、总览挂载的 immediate watcher 也触发一次，
// 两下撞在一起只应该有一次磁盘扫描。
export async function loadChecklist() {
  const c = state.checklist;
  if (c.loading) return;
  // 只读账号不做这次扫描：它看不到房间 / 令牌 / 备份那几条，
  // 清单里只剩 /session 给的安全与环境提醒，两处口径照样一致。
  if (!(state.me && state.me.isAdmin)) {
    c.data = null; c.error = ""; c.at = "";
    return;
  }
  const seq = (c.seq += 1);
  c.loading = true;
  const res = await api.get("/api/v1/rooms");
  // 连点「重新核对」或和轮询撞上时，晚到的那份不许写界面：
  // 旧快照里"缺令牌"这类行可能正好是刚补上的，会催人去做一个已经做完的事。
  if (seq !== c.seq) return;
  c.loading = false;
  if (res.ok) {
    c.data = res.data || null;
    c.at = (res.data && res.data.generatedAt) || "";
    c.error = "";
  } else {
    c.error = res.error || ("接口返回 " + res.status);
  }
}

export function logout() {
  api.clearCredentials();
  // 凭据一清，轮询出去的每一条都是 401：先停表，别让退出后的页面每五秒敲一次接口。
  stopPolling();
  state.needLogin = true;
  state.everLoggedIn = false;
  state.sessionExpired = false;
  state.me = null;
  state.clusters = [];
  state.statuses = {};
  state.tasks = [];
  // 运行状态里全是这台机器的数字（进程号、磁盘余量、开机时长）。
  // 退出后留着，下一个人打开会看到上一个账号采到的采样值。
  state.host = null;
  state.hostByPid = {};
  state.hostError = "";
  state.clustersLoaded = false;
  // 退出后别把上一个账号扫出来的房间 / 备份待办留在屏上；顺手清掉 loading，
  // 否则在途的那次回来会把新会话的清单锁住。
  state.checklist.loading = false;
  state.checklist.data = null;
  state.checklist.error = "";
  state.checklist.at = "";
  state.checklist.seq += 1;
  state.checklist.tick = 0;
  toast("info", "已退出", "凭据只存在这个标签页里，关掉就没了。");
}

// ---- 房间归属：界面上"改得动吗"的唯一依据 ----
//
// 这三个判断都只吃后端给的两个字段（sessionView.isAdmin / clusterView.ownedByMe），
// 前端不自己数角色、也不按房间名前缀猜归属。理由和 ownerLabel 由后端拼是同一条：
// 权限规则一旦在两处各写一遍，界面就会亮出一个接口拒你的按钮。
//
// roomWritable 之所以能化简成"管理员 或 这间归我"：只读账号既不是管理员，
// 后端也永远不会把房间派给他（指派那一条会 409），两个条件天然都是 false，
// 于是"改不动"在界面上和接口里说的是同一件事，不需要第三种说法。
export function roomWritable(cluster) {
  const me = state.me;
  if (!me) return false;
  if (me.isAdmin) return true;
  const c = state.clusters.find((x) => x.name === cluster);
  return !!(c && c.ownedByMe);
}

export function canAssignOwner() { return !!(state.me && state.me.canAssignOwner); }

// 归属那一枚的三档：未指派（普通灰）、派给别人（青）、派给我（绿）。
// 颜色只按 accountId / ownedByMe 两个字段判，不去比 ownerLabel 的中文——
// 说法改一个字就让它配色失灵，是最难查的那种"看着像坏了"。
export function ownerTone(c) { return !c.ownerAccountId ? "" : (c.ownedByMe ? "ok" : "info"); }
export function ownerIcon(c) { return c.ownerAccountId ? "user" : "lock"; }

// assignedText 把"由谁指派"那一句和指派时刻拼在一起。
//
// 时刻必须在这一侧格式化：接口给的是 RFC3339 的 UTC，而面板上别的时间都按浏览器
// 所在时区显示（运维看的是自己表上的时间）。早先这一句是后端整句拼好的，里面带着
// UTC 钟点，于是同一屏里"最近备份 18:04"和"由 admin 指派，09:41"差着七小时，
// 而用户没有任何线索能发现哪个才对。
export function assignedText(cluster) {
  const c = typeof cluster === "string"
    ? state.clusters.find((x) => x.name === cluster)
    : cluster;
  if (!c || !c.assignedNote) return "";
  return c.assignedAt ? c.assignedNote + "，" + dateTime(c.assignedAt) : c.assignedNote;
}

// readOnlyNote 是改不动时那句"为什么"。能改时返回空串，调用方直接 v-if。
//
// 未指派和"归别人管"要分开说：前者用户还能找管理员把房间派给自己，
// 后者在那个人自己的账号上根本不成立（成员的房间名单是接口裁好的，
// 别人的房间压根不出现），会看到这一句的只有管理员把名单之外的房间
// 留给了自己——那种时候说清归谁，比说"你没权限"有用。
//
// 判"有没有主"用 ownerAccountId 这个数，不拿后端那句中文比（`ownerLabel === "未指派"`）：
// 文案是要改的，一旦有人把它改成"还没派人"，这一句就会悄悄退化成下面那条"归 X 管"，
// 而对一个真的没主的房间说"这间房，不在你的房间名单里"是最没用的回答。
export function readOnlyNote(cluster) {
  const me = state.me;
  if (!me || roomWritable(cluster)) return "";
  const c = state.clusters.find((x) => x.name === cluster) || {};
  if (me.role === "viewer") {
    return "这个账号是只读：房间的状态都看得到，但任何修改都会被拒（改自己的密码除外）。";
  }
  if (!c.ownerAccountId) {
    return "这间房还没指派给谁，未指派的就是管理员自己在管：你能看到它的状态，配置改不了。" +
      "要接手的话，让管理员在「指派归属」里把它派给你。";
  }
  return "这间房" + (c.ownerLabel || "有人管") + "，不在你的房间名单里，所以只能看不能改。";
}

async function loadClusters() {
  const res = await api.get("/api/v1/clusters");
  if (res.ok) {
    state.clustersError = "";
    applyClusters(res.data);
    return true;
  }
  // 401 由 api 层转成登录层，这里只负责不再刷屏。
  if (res.status !== 401) state.clustersError = res.error || ("接口返回 " + res.status);
  return false;
}

function applyClusters(list) {
  state.clusters = list || [];
  state.clustersLoaded = true;
  // 选中的房间被删掉了要换一个：右侧那一栏（趋势、模组一致性）要是继续指着
  // 不存在的房间，会一直显示上一次的数字。换的时候优先挑有分片在跑的那个——
  // 目录顺序把空房间排在前面，一进面板就看到一屏"没在跑"，不如先看到实况。
  if (!state.clusters.some((c) => c.name === state.sel.cluster)) {
    state.sel.cluster = pickDefaultCluster(state.clusters);
  }
}

function pickDefaultCluster(list) {
  if (!list.length) return "";
  const live = list.find((c) => (c.shards || []).some((s) => s.running));
  return (live || list[0]).name;
}

export async function loadStatus(cluster, shard) {
  const k = keyOf(cluster, shard);
  const cur = state.statuses[k];
  if (cur && cur.loading) return;
  state.statuses[k] = { ...(cur || {}), loading: true };
  const res = await api.get(`/api/v1/clusters/${encodeURIComponent(cluster)}/shards/${encodeURIComponent(shard)}/status`);
  const entry = state.statuses[k];
  entry.loading = false;
  entry.at = new Date();
  if (res.ok) {
    entry.data = res.data;
    entry.error = "";
  } else {
    // 503 的原文是"这个分片还没有状态快照：助手 MOD 未加载，或者它还没进过一次世界"，
    // 那是有用的状态而不是故障，所以留着原文，页面按状态色显示。
    entry.data = null;
    entry.error = res.error || ("接口返回 " + res.status);
    entry.status503 = res.status === 503;
  }
}

// seenStatus 记上一次每条任务的状态，只为一个判断服务：
// "有没有一条本体更新刚刚结束"。见下面 loadTasks 里那句补问。
const seenStatus = {};

export async function loadTasks() {
  const res = await api.get("/api/v1/tasks?limit=30");
  if (!res.ok) {
    if (res.status !== 401) state.tasksError = res.error || ("接口返回 " + res.status);
    return;
  }
  const list = res.data || [];
  // 本体下载完，"这台机器有没有服务端本体"这个事实就变了，而界面里只有 /session 会带它。
  // 五秒轮询不去 stat 磁盘，所以只在切换的那一瞬补问一次：必须是上一轮还在排队/执行、
  // 这一轮已经结束的 server.update，一次任务只会多问一回。
  const settled = list.some((t) =>
    t.kind === "server.update" && open[t.status] === undefined && seenStatus[t.id] !== undefined);
  list.forEach((t) => {
    if (open[t.status] !== undefined) seenStatus[t.id] = true;
    else delete seenStatus[t.id];
  });
  state.tasks = list;
  state.tasksError = "";
  if (settled) await loadSession();
}

// loadHost 问这台机器的运行状态。进程号是界面回传上去的：/clusters 本来就带着
// 每个分片的 pid，让接口自己再问一次 screen 会让这一轮多等一程。
export async function loadHost() {
  const pids = [];
  state.clusters.forEach((c) => (c.shards || []).forEach((s) => {
    if (s.running && s.pid) pids.push(s.pid);
  }));
  const q = pids.length ? "?pid=" + pids.join("&pid=") : "";
  const res = await api.get("/api/v1/host" + q);
  if (!res.ok) {
    if (res.status !== 401) state.hostError = res.error || ("接口返回 " + res.status);
    return;
  }
  const byPid = {};
  (res.data.processes || []).forEach((p) => { byPid[p.pid] = p; });
  state.host = res.data;
  state.hostByPid = byPid;
  state.hostError = "";
}

export async function refreshAll(withMe) {  if (state.refreshing) return;
  state.refreshing = true;
  try {
    // withMe 是"用户主动按的"：/session 要真算一遍 PBKDF2 还要 stat 磁盘，
    // 每五秒自动轮询的那一组接口里都不该有它，所以默认不问。
    const jobs = [loadClusters(), loadTasks()];
    if (withMe) jobs.push(loadSession());
    await Promise.all(jobs);
    const alive = [loadHost()];
    state.clusters.forEach((c) => (c.shards || []).forEach((s) => {
      // 只有进程在跑的分片才值得问状态：没跑的时候那个接口必然 503，
      // 每五秒拿一批 503 既没用也会在运行日志里刷屏。
      if (s.running) alive.push(loadStatus(c.name, s.name));
    }));
    await Promise.all(alive);
    // 清单每四次轮询核一遍（约 20 秒），不并进上面的 Promise.all：
    // 它是一次全盘扫描，别让它拖住五秒一次的房间 / 任务 / 主机刷新。
    const cl = state.checklist;
    cl.tick += 1;
    if (cl.tick % 4 === 0) void loadChecklist();
    state.lastSync = new Date().toISOString();
  } finally {
    state.refreshing = false;
  }
}

// act 是所有写动作的公共外壳：占住按钮、给结论、成功后刷新。
//
// 为什么统一在这儿弹 Toast：面板里每一个写操作都会改变磁盘或进程，
// 用户需要"它接住了"和"它做完了"两层反馈；散在各页面里写的话，
// 一定会有某条路径失败时什么都不显示，只剩一个转圈的按钮。
export async function act(busyKey, method, path, body, opts) {
  const o = opts || {};
  state.busy[busyKey] = true;
  try {
    const res = await api.request(method, path, body === undefined ? {} : body);
    if (res.ok) {
      if (!o.quiet) toast("ok", o.okTitle || "操作已受理", o.okBody ? o.okBody(res.data) : "");
      // 破坏性动作给一个短时回退入口。反向请求由调用点给，界面不猜"怎么算撤销"。
      if (o.undo) {
        pushUndo(o.undo.label, async () => {
          const back = await api.request(o.undo.method, o.undo.path,
            o.undo.body === undefined ? {} : o.undo.body);
          if (!back.ok) throw new Error(back.error || ("接口返回 " + back.status));
          await refreshAfter(true, o.undo.path);
        }, o.undo.seconds);
      }
      if (o.after !== false) await refreshAfter(o.clusters !== false, path);
    } else if (res.status !== 401) {
      toast("bad", o.badTitle || "这一步没成", res.error || ("接口返回 " + res.status));
      // 把错误交给表单自己定位到具体字段；整表一个 err-box 说不清是哪个输入错了。
      if (o.onError) o.onError(res);
    }
    return res;
  } finally {
    delete state.busy[busyKey];
  }
}

// 批量写操作：串行跑（同一台机器上的配置/启停并行会互相抢文件与进程锁），
// 整批占一个 busy 键防重复提交，最后给一条汇总而不是 N 条叠在一起的 Toast。
export async function actMany(busyKey, items, opts) {
  const o = opts || {};
  if (state.busy[busyKey]) return { ok: false, error: "这批操作还在进行中" };
  state.busy[busyKey] = true;
  let done = 0, failed = 0;
  const errors = [];
  try {
    for (const it of items) {
      const res = await api.request(it.method, it.path, it.body === undefined ? {} : it.body);
      if (res.ok) done++;
      else { failed++; errors.push((it.label || it.path) + "：" + (res.error || res.status)); }
    }
    if (failed) toast("bad", o.badTitle || "这批操作有没成的",
      "成功 " + done + " 项，失败 " + failed + " 项。" + errors.slice(0, 3).join("；"));
    else if (!o.quiet) toast("ok", o.okTitle || "批量操作完成", "共 " + done + " 项。");
    if (o.after !== false) await refreshAfter(true, "");
    return { ok: failed === 0, done, failed, errors };
  } finally {
    delete state.busy[busyKey];
  }
}

async function refreshAfter(withClusters, path) {
  if (withClusters) { await refreshAll(); return; }
  if (String(path).indexOf("/tasks") >= 0) await loadTasks();
}

// busy 判断按钮该不该禁用。
export function isBusy(k) { return !!state.busy[k]; }

let timerPoll = null, timerTick = null;

export async function boot() {
  api.onUnauthorizedSet(() => {
    // 401 不再等于"把页面清空重来"：停轮询、把登录层浮在现有内容上，
    // 已经填了一半的向导/表单留在 DOM 里，重新登录后还在原处。
    const wasInside = !state.needLogin;
    stopPolling();
    state.needLogin = true;
    if (wasInside) {
      state.sessionExpired = true;
      state.loginError = "登录状态失效了（密码被改过、或面板刚重启）。重新登录后回到刚才那一页，已填的内容还在。";
    }
  });
  if (!api.hasStoredCredentials()) {
    state.checking = false;
    state.needLogin = true;
    return;
  }
  const ok = await loadClusters();
  state.checking = false;
  if (ok) {
    state.everLoggedIn = true;
    await loadSession();
    startPolling();
    return;
  }
  // 带着存下来的凭据却没拿到数据，只有两种可能：密码不对（401，凭据已被清），
  // 或者面板自己没配好（503）。两种都落到登录层，并把后端那句中文摆在框里。
  state.loginError = state.clustersError;
  state.needLogin = true;
}

export function startPolling() {
  stopPolling();
  refreshAll();
  timerPoll = setInterval(() => {
    // 标签页在后台时不打接口：这台机器上面板同时开着四五个标签是常态。
    if (document.visibilityState === "hidden") return;
    refreshAll();
  }, POLL_MS);
  timerTick = setInterval(() => { state.tick++; }, 1000);
  document.addEventListener("visibilitychange", onVisibility);
}

export function stopPolling() {
  if (timerPoll) clearInterval(timerPoll);
  if (timerTick) clearInterval(timerTick);
  timerPoll = timerTick = null;
  document.removeEventListener("visibilitychange", onVisibility);
}

// 从后台切回来时立刻补一次同步。轮询在标签页藏着时是短路的（上面那个 hidden 判断），
// 不补这一下，用户看到的就是"离开前那一刻的数字"：百分比不动、耗时也不动，
// 只能靠手动刷新，实测会让人以为整台面板卡住了。
function onVisibility() {
  if (document.visibilityState !== "visible") return;
  state.tick++;
  refreshAll();
}
