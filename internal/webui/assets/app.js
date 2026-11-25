// 界面入口。挂载顺序是固定的：注册页面表 -> 起路由 -> 装组件 -> 挂载 -> 再去要数据。
//
// 路由要先起：外壳第一次渲染就要知道当前在哪个页面，否则登录层闪一下
// 又跳回总览，用户会以为面板把他的地址吃掉了。
import { components } from "./js/ui.js";
import { Shell } from "./js/shell.js";
import { pages } from "./js/nav.js";
import { register, start } from "./js/router.js";
import { boot, toast, initTheme } from "./js/store.js";

const { createApp } = Vue;

// 主题要在建应用之前落定：它读的是 localStorage，不依赖任何接口。
// 晚一步就会先按深色画一帧、再跳成浅色，用户看到的是"面板闪了一下"。
initTheme();

// 注册表自检。少写一个 view 的话，左导航上会出现一个点了没反应的按钮，
// 而那看起来完全像产品坏了 —— 这种错要在加载时就喊出来。
pages.forEach((p) => {
  if (!p.view) throw new Error("页面注册表里 " + p.name + " 没有 view");
  if (!p.hidden && (!p.label || !p.icon || typeof p.href !== "function")) {
    throw new Error("页面注册表里 " + p.name + " 少了导航要用的 label/icon/href");
  }
});

register(pages);
start();

const app = createApp(Shell);
Object.keys(components).forEach((name) => app.component(name, components[name]));

// 渲染期抛错默认只进控制台：用户看到的是一片空白，而且刷新也没用。
// 兜到 Toast 上至少能说清"是界面自己错了还是接口错了"，并带着能报回来的信息。
app.config.errorHandler = (err, _inst, info) => {
  const msg = (err && err.message) ? err.message : String(err);
  console.error("界面渲染出错", err, info);
  toast("bad", "界面出错了", msg + "（环节：" + info + "）");
};

app.mount("#app");
boot();

// 页签条在窄屏上是横着滚的（.tabs 带 overflow-x:auto），这本身没问题；
// 缺的是"选中那一枚要跟着滚进视野"：390 那一档点第 3 屏、或者点完
// 「订阅并开启」由代码切到第 3 屏，用户看到的都是半截标签——
// 既读不出这一屏叫什么，也分不清是没切过去还是界面坏了。
// 观察 class 而不是绑点击：程序化切换只改 .on，不产生 click 事件，
// 只绑点击等于把这条动线里最常被程序切的那一次漏掉。
function revealActiveTab(btn) {
  const bar = btn.closest(".tabs");
  if (!bar || bar.scrollWidth <= bar.clientWidth) return;
  // 用两个矩形算位移，不用 offsetLeft：.tabs 不一定是这个按钮的 offsetParent，
  // 而且这样算出来的差值不受容器有没有定位影响。
  const bb = bar.getBoundingClientRect(), rb = btn.getBoundingClientRect();
  let d = 0;
  if (rb.left < bb.left + 8) d = rb.left - bb.left - 8;
  else if (rb.right > bb.right - 8) d = rb.right - bb.right + 8;
  // 只滚这一条，不碰整页：scrollIntoView 会连带把页面往下拽，
  // 切个页签结果屏顶那句话跑出视野，是另一种读不到。
  if (d) bar.scrollBy({ left: d, behavior: "smooth" });
}

new MutationObserver((records) => {
  for (const rec of records) {
    if (rec.type === "attributes") {
      const el = rec.target;
      if (el.matches && el.matches(".tabs button.on")) revealActiveTab(el);
      continue;
    }
    // 切页时整条页签是新插入的，class 上没有变更记录，得从插入的节点里找。
    for (const n of rec.addedNodes) {
      if (n.nodeType !== 1 || !n.querySelector) continue;
      const on = n.matches(".tabs button.on") ? n : n.querySelector(".tabs button.on");
      if (on) { revealActiveTab(on); break; }
    }
  }
}).observe(document.getElementById("app"), {
  subtree: true, childList: true, attributes: true, attributeFilter: ["class"],
});
