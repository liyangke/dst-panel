# 开服向导步骤条的一次真点：编号、走过的打勾、点得回去、没走到的点不动。
#
# 为什么值得单独跑：这一步条的全部意义是"不用连点上一步回去改名字"，
# 而它最容易犯的错正好是让人点回第 1 步之后再也点不回第 3 步（或者反过来，
# 直接点第 4 步的汇总，看到一页空表格）。所以这里真点：填名字 → 走到第 3 步
# → 点第 1 步 → 断言回到第 1 步且第 3 步仍然点得回去，同时把两档宽度的截图留下。
#
# 跑法：python .tools/ui_stepper_check.py [宽度]
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
  const bar = document.querySelector('.stepper');
  if (!bar) return { stepper: null };
  const steps = [...bar.querySelectorAll('.stp')].map((el) => ({
    num: (el.querySelector('.num') || {}).textContent,
    name: (el.querySelector('.stp-name') || {}).textContent,
    cls: el.className.replace(/\s*stp/, ''),
    disabled: el.disabled,
    visibleName: getComputedStyle(el.querySelector('.stp-name')).display !== 'none',
  }));
  const cnt = bar.querySelector('.stp-count');
  const f = [...document.querySelectorAll('.field')].find((x) => {
    const l = x.querySelector('label'); return l && l.textContent.indexOf('服务器名称') >= 0;
  });
  return {
    steps,
    count: cnt ? cnt.textContent.trim() : '',
    // 第 1 步独有的那一栏在不在：用它判断"现在到底停在哪一步"，比读高亮实在。
    onNameField: !!f,
    overflow: document.documentElement.scrollWidth - innerWidth,
  };
})()"""


def set_text(b, label, value):
    return b.js("""(() => {
      const f = [...document.querySelectorAll('.field')].find((x) => {
        const l = x.querySelector('label'); return l && l.textContent.indexOf(%s) >= 0; });
      const el = f && (f.querySelector('input') || f.querySelector('textarea'));
      if (!el) return false;
      const proto = el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
      Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, %s);
      el.dispatchEvent(new Event('input', { bubbles: true }));
      return true;
    })()""" % (json.dumps(label), json.dumps(value)))


def click_next(b):
    ok = b.js("(() => { const el = [...document.querySelectorAll('.card button')]"
              ".find((x) => x.textContent.trim() === '下一步'); if (!el) return false;"
              " el.click(); return true; })()")
    if not ok:
        raise SystemExit("页面上没有「下一步」")
    time.sleep(0.5)


def click_step(b, idx):
    """按圆点序号点步骤条（idx 从 1 开始），返回点没点到。"""
    return b.js("""(() => { const el = document.querySelectorAll('.stepper .stp')[%d];
                   if (!el || el.disabled) return false; el.click(); return true; })()""" % (idx - 1))


def show(tag, v):
    print("-- %s --" % tag)
    print(json.dumps(v, ensure_ascii=False, indent=2))


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
        b.wait("!!document.querySelector('.stepper')")
        time.sleep(1.5)

        show("第 1 步（还没走）", b.js(READ))
        if not set_text(b, "服务器名称", "步骤条自测房"):
            raise SystemExit("第 1 步没找到服务器名称那一栏")
        time.sleep(0.4)
        click_next(b)
        click_next(b)
        v3 = b.js(READ)
        show("第 3 步（走过两步）", v3)
        b.shoot(os.path.join(OUT, "stepper_step3_%d.png" % w))
        done = [s for s in v3["steps"] if "done" in s["cls"]]
        assert len(done) == 2, "第 3 步该有两枚走过的圆点，实际 %s" % len(done)
        assert all(s["num"].strip() == "✓" for s in done), "走过的圆点没打勾: %s" % done
        assert v3["steps"][3]["disabled"], "没走到过的第 4 步不该点得动"
        assert not v3["onNameField"], "还停在第 1 步，下一步没生效"

        if not click_step(b, 1):
            raise SystemExit("点不回第 1 步")
        time.sleep(0.5)
        v1 = b.js(READ)
        show("点第 1 步回去之后", v1)
        assert v1["onNameField"], "点了圆点没回到第 1 步"
        assert not v1["steps"][2]["disabled"], "回到第 1 步后，走到的第 3 步该还点得回去"
        assert v1["steps"][3]["disabled"], "第 4 步仍然没走到过，不该点得动"

        if not click_step(b, 3):
            raise SystemExit("回不去第 3 步")
        time.sleep(0.5)
        v3b = b.js(READ)
        show("再点回第 3 步", v3b)
        assert not v3b["onNameField"], "第 3 步没回去成功"
        assert v3b["count"].startswith("第 3"), "计数说的不是第 3 步: %s" % v3b["count"]
        names = [s["visibleName"] for s in v3b["steps"]]
        print("  圆点名字可见情况: %s" % names)
        if w > 560:
            assert all(names), "桌面宽度下四步的名字都该可见: %s" % names
        else:
            assert names == [False, False, True, False], "手机上只该露当前那一步的名字: %s" % names
        # 手机上一屏到不了步骤条（上面还有两条横幅和房间卡片），截图前把它滚进视口。
        b.js("document.querySelector('.stepper').scrollIntoView({block:'start'});")
        time.sleep(0.3)
        b.shoot(os.path.join(OUT, "stepper_%d.png" % w))
        print("\n步骤条 %s 宽度：全部通过（溢出 %s px）" % (w, v3b["overflow"]))
    finally:
        b.close()


if __name__ == "__main__":
    main()
