# 一次性诊断：为什么点了「玩家时间线」以后视图没切过去。
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from cdp import BASE, Browser

HOOK = ("window.__perr=[];window.addEventListener('error',function(e){"
        "if(window.__perr.length<20)window.__perr.push(String(e.message))});"
        "window.addEventListener('unhandledrejection',function(e){"
        "if(window.__perr.length<20)window.__perr.push('promise:'+String(e.reason&&e.reason.message||e.reason))});")

b = Browser(1440, 900)
try:
    b.goto(BASE + "/#/")
    b.wait("location.origin === %s" % json.dumps(BASE))
    b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
    b.reload()
    b.wait("!!document.querySelector('.shell')")
    b.js(HOOK)
    b.js("location.hash = '#/logs'")
    b.wait("!!document.querySelector('.log-controls')")
    time.sleep(1.5)
    print("seg 按钮:", b.js("[...document.querySelectorAll('.log-controls .seg button')].map(x=>x.textContent.trim())"))
    print("localStorage:", b.js("JSON.stringify(Object.keys(localStorage).map(k=>[k, (localStorage.getItem(k)||'').slice(0,80)]))"))
    clicked = b.js("""(() => {
      const el = [...document.querySelectorAll('.log-controls .seg button')]
        .find((x) => x.textContent.trim() === '玩家时间线');
      if (!el) return 'not-found';
      el.click();
      return 'clicked:' + (el.className || '');
    })()""")
    print("点击:", clicked)
    time.sleep(2.0)
    print("hash:", b.js("location.hash"))
    print("有的块:", b.js("[...document.querySelectorAll('.log-controls,.stream-meta,.tl-head,.tl-list,.err-box,.logbox,details')].map(x=>x.className)"))
    print("__perr:", b.js("window.__perr || []"))
    print("timeline 接口:", b.js("""(async () => {
      const r = await fetch('/api/v1/clusters/Cluster_1/shards/Master/timeline?lines=500',
        {headers: {Authorization: 'Basic ' + btoa('admin:admin')}});
      const t = await r.text();
      return r.status + ' ' + t.slice(0, 200);
    })()"""))
    time.sleep(1.0)
    b.shoot(os.path.join(os.path.dirname(os.path.abspath(__file__)), "tmp", "shots", "timeline", "dbg.png"))
finally:
    b.close()
