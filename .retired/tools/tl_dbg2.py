# 一次性排障：点「玩家时间线」以后界面为什么落回 #/。
# 分辨两种可能：整页真的重新导航了（marker 丢），还是有人写了 location.hash。
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from cdp import BASE, Browser
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

b = Browser(1440, 900)
b.goto(BASE + "/")
b.wait("location.origin === '%s'" % BASE)
b.js("""(() => {
  sessionStorage.setItem('dstpanel.user', 'admin');
  sessionStorage.setItem('dstpanel.pass', 'admin');
})()""")
b.goto(BASE + "/#/logs")
b.wait("!!document.querySelector('.log-controls')")
b.js("""(() => {
  window.__marker = 'alive';
  window.__nav = [];
  window.addEventListener('hashchange', (e) => {
    if (window.__nav.length < 10) window.__nav.push('hashchange ' + (e.oldURL || '') + ' -> ' + (e.newURL || ''));
  });
  document.addEventListener('click', (e) => {
    if (!window.__nav) return;
    const t = e.composedPath().map((x) => (x.tagName || '') + '.' + ((x.className || '') + '').split(' ')[0]).slice(0, 6).join(' < ');
    window.__nav.push('click-path ' + t);
  }, true);
  window.addEventListener('beforeunload', () => { window.__nav = window.__nav || []; window.__nav.push('beforeunload'); });
  return document.querySelectorAll('.log-controls .seg button').length;
})()""")
print("按钮数:", b.js("document.querySelectorAll('.log-controls .seg button').length"))
print("点之前 hash:", b.js("location.hash"))
print("点击结果:", repr(b.js("""(() => {
  const el = [...document.querySelectorAll('.log-controls .seg button')].find((x) => x.textContent.trim() === '玩家时间线');
  if (!el) return 'no-button';
  el.click(); return 'clicked';
})()""")))
import time
time.sleep(1.2)
print("hash:", b.js("location.hash"))
print("marker:", repr(b.js("window.__marker")))
print("nav:", b.js("JSON.stringify(window.__nav || null)"))
print("controls:", b.js("!!document.querySelector('.log-controls')"))
print("body 头部文字:", (b.js("document.body.innerText.slice(0,200)") or "").replace("\n", " / "))
b.close()
