// 极简 hash 路由。
//
// 为什么不用 vue-router：这个面板的"路由"就是把九个页面切换显示，
// 没有嵌套、没有守卫、没有异步分块。多引一个库换来的是"发版要在浏览器里
// 多下几十 KB 且要靠打包工具"，而这里刻意没有打包工具。
//
// 为什么用 hash：面板把静态文件内嵌在程序里，任何非 /api/ 的路径都会被兜底成外壳
// （见 internal/webui/webui.go）。用 /backup 这种真路径的话，刷新时靠的是那条兜底；
// 用 hash 就不依赖服务端配合，收藏夹、新标签页打开、放在子路径下反代都同样能跳到位。
// Vue 是 vendor 里那份 global 构建挂在 window 上的，不是模块，所以这里不能 import。
const { reactive, computed } = Vue;

export const route = reactive({ name: "", path: "/", params: [], query: {} });

// register 由 app.js 在挂载前调用一次，把 nav.js 那张表交给路由器。
let pages = [];
export function register(list) { pages = list; }

function parse(hash) {
  let raw = String(hash || "").replace(/^#/, "");
  if (!raw || raw === "/") return { name: pages.length ? pages[0].name : "", params: [], query: {} };
  const qIdx = raw.indexOf("?");
  const query = {};
  if (qIdx >= 0) {
    raw.slice(qIdx + 1).split("&").forEach((kv) => {
      if (!kv) return;
      const i = kv.indexOf("=");
      const k = i < 0 ? kv : kv.slice(0, i);
      const v = i < 0 ? "" : kv.slice(i + 1);
      query[decodeURIComponent(k)] = decodeURIComponent(v.replace(/\+/g, " "));
    });
    raw = raw.slice(0, qIdx);
  }
  const parts = raw.split("/").filter(Boolean).map(decodeURIComponent);
  // 匹配规则：注册表里每条 page.match 是一个前缀段数组（如 ["cluster", "*"]），
  // "*" 吃掉一段并变成 params。按注册顺序取第一个命中的。
  for (const p of pages) {
    const segs = p.segments || [];
    if (segs.length !== parts.length) continue;
    const params = [];
    let ok = true;
    for (let i = 0; i < segs.length; i++) {
      if (segs[i] === "*") { params.push(parts[i]); continue; }
      if (segs[i] !== parts[i]) { ok = false; break; }
    }
    if (ok) return { name: p.name, params, query };
  }
  // 认不出来不要落到第一个页面：那会让人以为"我点的那条没生效"，
  // 而实际上只是地址拼错了。落到 notFound 页，把两种情况说清楚。
  return { name: "notFound", params: parts, query };
}

export function current() {
  return pages.find((p) => p.name === route.name) || null;
}

export const currentView = computed(() => {
  const p = current();
  return p ? p.view : null;
});

export function go(name, ...args) {
  const p = pages.find((x) => x.name === name);
  if (!p) return;
  location.hash = "#" + p.href(...args);
}

// setQuery 把页内状态（选中的页签、向导第几步、筛选条件）写进 URL。
// 只改 hash 的 query 部分，path 不变，所以 currentView 不变、组件不会被销毁重建——
// 这正是"切页签不丢草稿"的前提；同时刷新、前进后退、收藏都能回到同一屏。
// 传空值表示删掉这个键，别把 ?tab= 这种空串留给下次解析。
export function setQuery(patch) {
  const raw = (location.hash || "#/").replace(/^#/, "");
  const pathPart = raw.split("?")[0] || "/";
  const q = Object.assign({}, route.query);
  Object.keys(patch || {}).forEach((k) => {
    const v = patch[k];
    if (v === undefined || v === null || v === "") delete q[k];
    else q[k] = String(v);
  });
  const pairs = Object.keys(q).map((k) => encodeURIComponent(k) + "=" + encodeURIComponent(q[k]));
  const next = "#" + pathPart + (pairs.length ? "?" + pairs.join("&") : "");
  // 地址没变就什么都不做。这里早先会再调一次 sync()，而 sync 每次都把
  // route.query 换成一个新对象，watch("query.tab") 又会回头调 setQuery——
  // 幂等写入会变成 hashchange → sync → watch → setQuery 的死循环。
  if (next !== location.hash) location.hash = next;
}

export function queryOf(k) { return route.query[k] === undefined ? "" : route.query[k]; }

// sync 把当前 hash 写进 route。hashchange 与初次加载都走它。
export function sync() {
  const r = parse(location.hash);
  route.name = r.name;
  route.params = r.params;
  // query 内容没变就别换对象：页签组件 watch 的是 route.query 里的键，
  // 无意义的新对象会让"值没变"的写入也被当成一次变更，来回写 hash。
  if (JSON.stringify(route.query || {}) !== JSON.stringify(r.query || {})) route.query = r.query;
  route.path = location.hash.replace(/^#/, "") || "/";
}

export function start() {
  window.addEventListener("hashchange", sync);
  sync();
}

export function pageOf(name) {
  return pages.find((p) => p.name === name) || null;
}
