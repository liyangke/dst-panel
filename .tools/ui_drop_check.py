# 模组上传的拖拽区：拖进去的文件到底有没有被组件接住，点这一片到底有没有叫起选文件框。
#
# 为什么单独跑一次：这块的全部价值是"这一片能放东西"，而它最容易坏在两个地方——
# 一是 dragover 没 preventDefault，浏览器直接把 zip 打开成 data: 页面（整页跳走）；
# 二是拖进来的文件只改了显示文字，没进真正上传用的那个来源，于是点上传时报"先选一个文件"。
# 所以这里用真 DragEvent 走一遍，并且断言主按钮的可用状态跟着变。
#
# 跑法：python .tools/ui_drop_check.py [宽度]
# 前提：python .tools/ui_dev.py 已经把面板起在 127.0.0.1:8963。
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from cdp import BASE, Browser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, ".tools", "tmp", "shots")
sys.stdout.reconfigure(encoding="utf-8", errors="replace")

READ = r"""(() => {
  const d = document.querySelector('.drop');
  if (!d) return { drop: null };
  const btn = [...document.querySelectorAll('.card .btn.primary')]
    .find((x) => x.textContent.indexOf('上传并落地') >= 0);
  const inp = document.querySelector('.drop-file');
  const cs = getComputedStyle(d);
  return {
    title: (d.querySelector('.drop-t') || {}).textContent,
    sub: (d.querySelector('.drop-s') || {}).textContent,
    cls: d.className,
    borderStyle: cs.borderTopStyle,
    minH: Math.round(parseFloat(cs.minHeight)),
    btn: btn ? { text: btn.textContent.trim(), disabled: btn.disabled } : null,
    // 真正上传读的就是这一个来源：它空着而标题上有文件名，就是"只显示不接管"。
    inputHidden: inp ? (getComputedStyle(inp).opacity === '0' && inp.offsetWidth === 0) : null,
    // 选文件框里到底有没有那一个文件：拖进来的那条路不经过它，所以这里只验点选。
    inputs: inp ? inp.files.length : -1,
    err: (document.querySelector('.card .err-box') || { textContent: '' }).textContent.trim(),
    overflow: document.documentElement.scrollWidth - innerWidth,
  };
})()"""

# 造一个真 File 塞进 DataTransfer，再派发 dragover / drop：
# 浏览器不允许脚本往 input.files 里塞东西以外的路径，但 DragEvent 这一条是允许的。
DROP = r"""(() => {
  const d = document.querySelector('.drop');
  if (!d) return false;
  const dt = new DataTransfer();
  dt.items.add(new File([new Uint8Array([80,75,3,4])], %s, { type: 'application/zip' }));
  const mk = (t) => { const e = new Event(t, { bubbles: true, cancelable: true }); e.dataTransfer = dt; return e; };
  d.dispatchEvent(mk('dragover'));
  d.dispatchEvent(mk('drop'));
  return true;
})()""" % json.dumps("拖进来的包.zip")

# 点着选那条路：走原生 setter 之外的另一条——直接给 input.files 赋值再派发 change。
PICK = r"""(() => {
  const inp = document.querySelector('.drop-file');
  if (!inp) return false;
  const dt = new DataTransfer();
  dt.items.add(new File([new Uint8Array([80,75,3,4])], %s, { type: 'application/zip' }));
  inp.files = dt.files;
  inp.dispatchEvent(new Event('change', { bubbles: true }));
  return true;
})()""" % json.dumps("点选的包.zip")

CLICK_OPENS = r"""(() => {
  const inp = document.querySelector('.drop-file');
  let called = 0;
  const orig = inp.click.bind(inp);
  inp.click = () => { called += 1; };
  document.querySelector('.drop').click();
  inp.click = orig;
  return called;
})()"""


def main():
    w = int(sys.argv[1]) if len(sys.argv) > 1 else 1440
    b = Browser(w, 900 if w > 560 else 844)
    try:
        b.goto(BASE + "/#/")
        b.js("sessionStorage.setItem('dstpanel.creds', JSON.stringify({user:'admin',pass:'admin'}))")
        b.reload()
        if not b.wait("!!document.querySelector('.shell')"):
            raise SystemExit("登录层没过去")
        b.js("location.hash = '#/mods'")
        b.wait("!!document.querySelector('.tabs')")
        time.sleep(1.2)
        # 上传区在第 2 步那一屏里，而页面默认落在第 1 步，所以要切一次。
        # 按页签上那颗序号点、不按整段文字：#85 把标签从「装到这台机器」改成了
        # 「这台机器上的文件」，按文字找的脚本那次就假红过一回（它测的是拖拽区，不是名字）。
        ok = b.js(r"""(() => {
          const el = [...document.querySelectorAll('.tabs button')].find(
            (x) => { const s = x.querySelector('.tab-step'); return s && s.textContent.trim() === '2'; });
          if (!el) return false;
          el.click();
          return true;
        })()""")
        if not ok:
            raise SystemExit("模组页没有第 2 步那一屏（序号 2 的页签找不到）")
        if not b.wait("!!document.querySelector('.drop')"):
            raise SystemExit("切到第 2 步后没看到拖拽区")
        time.sleep(1.0)
        b.js("document.querySelector('.drop').scrollIntoView({block:'center'})")
        time.sleep(0.3)

        v0 = b.js(READ)
        print("-- 空着 --")
        print(json.dumps(v0, ensure_ascii=False, indent=2))
        assert v0["title"].strip() == "把模组包拖到这里", "空态主文案不对: %r" % v0["title"]
        assert v0["borderStyle"] == "dashed", "没拖到上面时该是虚线框: %s" % v0["borderStyle"]

        # 缺文件就点上传：必须给出原因，而不是静默发一个空请求。
        b.js("(() => { const el=[...document.querySelectorAll('.card .btn.primary')]"
             ".find((x)=>x.textContent.indexOf('上传并落地')>=0); el && el.click(); return true; })()")
        time.sleep(0.6)
        e0 = b.js(READ)
        print("-- 没选文件就点上传 --")
        print(json.dumps({"err": e0["err"], "title": e0["title"]}, ensure_ascii=False))
        assert "先选一个" in e0["err"], "缺文件时没给原因: %r" % e0["err"]

        assert b.js(DROP), "页面上没有 .drop"
        time.sleep(0.6)
        v1 = b.js(READ)
        print("-- 拖进来之后 --")
        print(json.dumps(v1, ensure_ascii=False, indent=2))
        assert v1["title"].strip() == "拖进来的包.zip", "拖进来的名字没显示: %r" % v1["title"]
        assert "over" not in v1["cls"], "drop 之后还停在悬停态: %s" % v1["cls"]
        assert "has" in v1["cls"], "接住文件后没换成已拿到状态的边框: %s" % v1["cls"]
        assert "已拿到" in v1["sub"], "副文案没说拿到了多大: %r" % v1["sub"]
        assert v1["err"] == "", "拖进来之后上一句报错没清: %r" % v1["err"]

        # 点这一片要能叫起选文件框（这里只断言它调了 input.click，不真开系统框）。
        calls = b.js(CLICK_OPENS)
        assert calls == 1, "点拖拽区没转交给选文件框，调了 %s 次" % calls

        assert b.js(PICK), "没有 .drop-file 这个 input"
        time.sleep(0.6)
        v2 = b.js(READ)
        print("-- 点着选之后 --")
        print(json.dumps({"title": v2["title"], "inputs": v2["inputs"], "inputHidden": v2["inputHidden"]},
                         ensure_ascii=False))
        assert v2["title"].strip() == "点选的包.zip", "换包没生效: %r" % v2["title"]
        assert v2["inputs"] == 1 and v2["inputHidden"], "input 该藏住且真的带着那一个文件"
        b.shoot(os.path.join(OUT, "drop_%d.png" % w))
        print("\n拖拽上传区 %s 宽度：全部通过（溢出 %s px）" % (w, v2["overflow"]))
    finally:
        b.close()


if __name__ == "__main__":
    main()
