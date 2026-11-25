// 页面注册表：左导航的每一项、以及 hash 到组件的对应关系。
//
// 导航条目和路由共用这一张表，所以"页面上有点、路由却跳不过去"这种不一致
// 只可能出现在漏写 view 的时候，而那种情况会在启动时就报错（见 app.js 的检查）。
//
// 每条上的三个新字段都是给"外壳"用的，页面组件自己不需要知道：
//   icon   —— js/icons.js 里的图标名（不再用 ◇ ▦ 这类装饰字符：它们在不同字体下
//             会长成不一样的东西，也没法跟着主题变色）
//   hue    —— 这一页的色相。侧栏选中态、顶栏图标块、以及整页的 --page-accent 都取它，
//             所以"我在哪一页"这件事在界面上有一整层颜色在说，而不用读标题。
//   group  —— 侧栏的分区标题；手机端底部那一排不带分区，靠 tab: true 挑出四页。
import Overview from "./views/overview.js";
import Lobby from "./views/lobby.js";
import Room from "./views/room.js";
import Players from "./views/players.js";
import MapView from "./views/map.js";
import Mods from "./views/mods.js";
import Snapshots from "./views/snapshots.js";
import Logs from "./views/logs.js";
import Tasks from "./views/tasks.js";
import Audit from "./views/audit.js";
import Accounts from "./views/accounts.js";
import Settings from "./views/settings.js";
import { state } from "./store.js";

const NotFound = {
  props: { params: Array },
  template: `
    <x-empty icon="help" title="这个地址没有对应的页面"
      :note="'地址段是 ' + (params || []).join('/') + '。左边的导航里能找到全部页面。'"></x-empty>`,
};

export const pages = [
  {
    name: "overview",
    label: "总览",
    icon: "dashboard",
    hue: "var(--h-azure)",
    group: "日常",
    tab: true,
    hint: "房间与世界现在什么样",
    segments: [],
    href: () => "/",
    view: Overview,
    // badge 返回导航项右侧那个小数字；返回 0 或 null 就不显示。
    badge: () => 0,
  },
  {
    // 公网房间排在总览后面：这一页看的是别人开的房间，而"现在有哪些房间在跑"这个念头
    // 一半在自己机器上（总览那一屏的页签逐间给），一半在外面。它不进底部四格——
    // 挑别人的房间是偶尔一次的事，不是每天盯的那一屏。
    name: "lobby",
    label: "公网房间",
    icon: "globe",
    hue: "var(--h-cyan)",
    group: "日常",
    hint: "Klei 公开列表里别人开的房间：挑一间，把连接命令复制走",
    segments: ["lobby"],
    href: () => "/lobby",
    view: Lobby,
  },
  {
    // 玩家管理和开服/世界是两条正交的线：前者改"谁能进来、谁能当管理员"，
    // 后者改"世界长什么样"。它排在日常这一段靠前的位置，是因为天天用。
    name: "players",
    label: "玩家管理",
    icon: "users",
    hue: "var(--h-green)",
    group: "日常",
    tab: true,
    hint: "谁在线、踢人禁言、管理员与黑白名单",
    segments: ["players"],
    href: () => "/players",
    view: Players,
  },
  {
    // 地图排在玩家管理后面：它同样天天看，但"谁在管理服务器"比"世界长什么样"更常用。
    name: "map",
    label: "世界地图",
    icon: "map",
    hue: "var(--h-teal)",
    group: "日常",
    tab: true,
    hint: "地形、建筑与资源分布、玩家位置",
    segments: ["map"],
    href: () => "/map",
    view: MapView,
  },
  {
    // 配置这一段的第一项。开服要做的三件事都发生在"分片还没起来之前"：
    // 建目录、给令牌、定世界。拆成三个导航项的话，用户建完房间还得自己找"下一步去哪填令牌"。
    name: "room",
    label: "开服与世界",
    icon: "world",
    hue: "var(--h-amber)",
    group: "配置",
    hint: "建房间、填 Klei 令牌、改世界规则",
    segments: ["room"],
    href: () => "/room",
    view: Room,
  },
  {
    name: "mods",
    label: "模组",
    icon: "cube",
    hue: "var(--h-violet)",
    group: "配置",
    tab: true,
    hint: "装了哪些、开了哪些、怎么配",
    segments: ["mods"],
    href: () => "/mods",
    view: Mods,
  },
  {
    name: "snaps",
    label: "存档备份",
    icon: "archive",
    hue: "var(--h-lime)",
    group: "配置",
    hint: "存档备份、恢复与保留份数",
    segments: ["snaps"],
    href: () => "/snaps",
    view: Snapshots,
    // 备份/恢复是队列里的活：导航上留个数字，用户从别的页面也知道"这一页还在动"。
    badge: () => state.tasks.filter((t) =>
      (t.kind === "backup.create" || t.kind === "backup.restore") &&
      (t.status === "queued" || t.status === "running")).length,
  },
  {
    name: "logs",
    label: "游戏日志",
    icon: "lines",
    hue: "var(--h-gold)",
    group: "运维",
    hint: "玩家事件表（谁进服、谁死了、谁被请下线）与逐行原文",
    segments: ["logs"],
    href: () => "/logs",
    view: Logs,
  },
  {
    // 任务中心排在日志后面：动手（启停/备份/更新）时才需要盯它，
    // 但它是"面板还在干活"的唯一去处，所以不能藏进某个页面里。
    name: "tasks",
    label: "任务中心",
    icon: "clock",
    hue: "var(--h-coral)",
    group: "运维",
    hint: "排队的、在跑的、失败过的后台任务",
    segments: ["tasks"],
    href: () => "/tasks",
    view: Tasks,
    badge: () => state.tasks.filter((t) => t.status === "queued" || t.status === "running").length,
  },
  {
    name: "audit",
    label: "操作记录",
    icon: "clipboard",
    hue: "var(--h-rose)",
    group: "运维",
    hint: "谁在什么时候改了哪样东西",
    segments: ["audit"],
    href: () => "/audit",
    view: Audit,
  },
  {
    name: "accounts",
    label: "账号管理",
    icon: "idcard",
    hue: "var(--h-magenta)",
    group: "系统",
    hint: "谁能登录面板、各是什么权限",
    segments: ["accounts"],
    href: () => "/accounts",
    view: Accounts,
  },
  {
    name: "settings",
    label: "面板设置",
    icon: "gear",
    hue: "var(--h-slate)",
    group: "系统",
    hint: "保留期、备份份数与定时备份间隔",
    segments: ["settings"],
    href: () => "/settings",
    view: Settings,
  },
  { name: "notFound", label: "没这个页面", hidden: true, view: NotFound },
];

export function navPages() {
  return pages.filter((p) => !p.hidden);
}

// 侧栏按 group 分段，顺序就是上面这张表的顺序：
// 把"日常看的"放最前，是这一版按"新手第一次打开先看什么"重排的结果。
export function navGroups() {
  const out = [];
  navPages().forEach((p) => {
    const g = p.group || "其他";
    let last = out[out.length - 1];
    if (!last || last.name !== g) { last = { name: g, items: [] }; out.push(last); }
    last.items.push(p);
  });
  return out;
}

// 手机端底部那一排：固定四页 + "更多"。挑这四页的理由是"每天会看好几次"，
// 而不是"配置上排得靠前"——开服与世界只在开服那天用，不该占掉一格。
export function tabPages() {
  return navPages().filter((p) => p.tab);
}

export function sheetPages() {
  return navPages().filter((p) => !p.tab);
}

export function badgeOf(p) {
  return p.badge ? p.badge() : 0;
}

export { state };
