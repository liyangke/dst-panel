# 开服向导第 1 步的「游戏端口」一栏：真点一遍，看四种填法各说什么话。
#
# 为什么要单独跑：这一栏是建房间前唯一能改号的地方，而后端要等到第 4 步按下去才回话。
# 也就是说"填错了说什么"这件事只能在浏览器里验，接口测试是验不出来的。
# 这里把四类填法都走一遍：留空、撞别人的游戏端口、撞别人的配套端口、超出可填区间，
# 最后填一个空闲号点到第 4 步，看汇总那一行把两个号都写出来没有。
#
# 跑法：python .tools/ui_wizard_port_check.py [宽度]
# 前提：python .tools/ui_dev.py 已经把面板起在 127.0.0.1:8963。
import json
import os
import sys
import time
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, ".tools", "tmp", "shots")
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# 第 1 步那一栏的可见状态：占位符、下面的提示行、已占用号、按钮。
PROBE = r"""(() => {
  const field = [...document.querySelectorAll('.field')].find((f) => {
    const l = f.querySelector('label'); return l && l.textContent.includes('游戏端口');
  });
  const inp = field ? field.querySelector('input') : null;
  const hints = field ? [...field.querySelectorAll('.hint')].map((x) => x.textContent.trim().replace(/\s+/g, ' ')) : [];
  return {
    placeholder: inp ? inp.placeholder : '',
    value: inp ? inp.value : '',
    hints,
    chips: field ? [...field.querySelectorAll('.chip')].map((x) => x.textContent.trim()) : [],
    quick: field ? [...field.querySelectorAll('button')].map((x) => x.textContent.trim()) : [],
    overflow: document.documentElement.scrollWidth - innerWidth,
  };
})()"""

# 第 4 步的汇总表（x-kv 渲染成 dl.kv）：只要"端口"那一行。
SUMMARY = r"""(() => {
  const rows = [...document.querySelectorAll('dl.kv')].flatMap((dl) =>
    [...dl.querySelectorAll('dt')].map((dt, i) => dt.textContent.trim() + ' = ' +
      (dt.nextElementSibling ? dt.nextElementSibling.textContent.trim().replace(/\s+/g, ' ') : '')));
  return { port: rows.find((t) => t.startsWith('端口')) || '(没找到端口那一行)',
           all: rows,
           err: (document.querySelector('.err-box') || {}).textContent || '',
           btn: [...document.querySelectorAll('.btn.primary')].map((x) => x.textContent.trim()).join('|') };
})()"""


def set_text(b, value):
    """改输入框要走原生 setter + input 事件：直接赋 .value，Vue 的 v-model 收不到。"""
    return b.js("""(() => {
      const field = [...document.querySelectorAll('.field')].find((f) => {
        const l = f.querySelector('label'); return l && l.textContent.includes('游戏端口');
      });
      const el = field && field.querySelector('input');
      if (!el) return false;
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
      setter.call(el, %s);
      el.dispatchEvent(new Event('input', { bubbles: true }));
      return true;
    })()""" % json.dumps(str(value)))


def main():
    w = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    b = Browser(w, 900 if w > 560 else 844)
    try:
        b.goto(BASE + "/#/")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("登录层没过去")
        b.js("location.hash = '#/room'")
        b.wait("!!document.querySelector('.wizard-steps')")
        time.sleep(2.0)

        cases = [("留空", ""), ("撞别人的游戏端口", "11000"),
                 ("撞别人的配套端口", "10999"), ("超出区间", "999"),
                 ("空闲号", "11010")]
        for name, v in cases:
            if not set_text(b, v):
                raise SystemExit("页面上没有「游戏端口」这一栏")
            time.sleep(0.5)
            s = b.js(PROBE)
            print("-- %s（填 %r）--" % (name, v))
            print(json.dumps(s, ensure_ascii=False, indent="  "))
            if name == "空闲号":
                b.shoot(os.path.join(OUT, "wizard_port_%d.png" % w))

        # 撞号要拦在第 1 步：填了名字点下一步、还停在第 1 步才算对。
        # 名称按占位符找：这一步有两个文本框，按序号点迟早填到"目录名"上去。
        set_text(b, "11000")
        b.js("""(() => {
          const el = document.querySelector('input[placeholder="玩家列表上看到的那一行"]');
          const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
          setter.call(el, '端口自测房'); el.dispatchEvent(new Event('input', { bubbles: true }));
        })()""")
        time.sleep(0.4)
        b.js("(() => { const x=[...document.querySelectorAll('.btn.primary')].pop(); x && x.click(); })()")
        time.sleep(0.9)
        print("-- 撞号点下一步之后 --")
        print(json.dumps({"step": b.js("(() => { const e=[...document.querySelectorAll('.wizard-steps .step.on')]"
                                       ".find((x)=>x.textContent.trim()); return e?e.textContent.trim().replace(/\\s+/g,' '):''; })()"),
                          "err": (b.js("(() => { const e=document.querySelector('.err-box');"
                                       "return e?e.textContent.trim().replace(/\\s+/g,' '):''; })()")),
                          "hint": b.js(PROBE)["hints"]}, ensure_ascii=False, indent="  "))

        # 换成空闲号，一路点到第 4 步看汇总。
        set_text(b, "11010")
        time.sleep(0.4)
        for _ in range(3):
            b.js("(() => { const x=[...document.querySelectorAll('.btn.primary')].pop(); x && x.click(); })()")
            time.sleep(0.8)
        print("-- 第 4 步汇总 --")
        print(json.dumps(b.js(SUMMARY), ensure_ascii=False, indent="  "))
        b.shoot(os.path.join(OUT, "wizard_review_%d.png" % w))
    finally:
        b.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
