# #86 验收：窄屏页签条里"选中那一枚"必须完整落在视野内。
#
# 三条来路都要量，因为它们的 DOM 变化方式不一样：
#   1. 用户自己点最后那一枚（click 改 class）；
#   2. 代码切过去（#85 点完「订阅并开启」会 this.tab = "shard"，没有 click 事件）；
#   3. 整页重进时页签条是新建的（只有 childList，没有 class 变更）。
# 判定只看一件事：那一枚的左右边是不是都完整落在页签条的可视框里。
# 用 0 容差而不是"留几个像素余量"：这一排没有内边距，第一枚的左边就是条的左边，
# 留余量会把"贴着边但完整可见"误判成被裁（上一版就是这么假红过五条）。
import json
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import subscribe85_check as sc
from cdp import Browser

sys.stdout.reconfigure(encoding="utf-8", errors="replace")

# 第 3 个参数给地址就能拿去验公网那一份（同一套判定，本地/公网各跑一遍才算装对了）。
PANEL = sys.argv[3] if len(sys.argv) > 3 else "http://127.0.0.1:8963"

READ = r"""(() => {
  const bar = document.querySelector('.tabs');
  if (!bar) return null;
  const bb = bar.getBoundingClientRect();
  const on = bar.querySelector('button.on');
  const ob = on ? on.getBoundingClientRect() : null;
  return {
    barLeft: Math.round(bb.left), barRight: Math.round(bb.right),
    scrollable: bar.scrollWidth > bar.clientWidth,
    scrollLeft: Math.round(bar.scrollLeft),
    onText: on ? on.innerText.replace(/\s+/g, ' ').trim() : '',
    onLeft: ob ? Math.round(ob.left) : null,
    onRight: ob ? Math.round(ob.right) : null,
    count: bar.querySelectorAll('button').length,
  };
})()"""


def check(v, tag, fails):
    if not v:
        fails.append(tag + "：页签条没读到")
        return
    room = "可横滑=%s 共 %d 枚，选中「%s」占 %s~%s，条可视区 %s~%s" % (
        v["scrollable"], v["count"], v["onText"], v["onLeft"], v["onRight"],
        v["barLeft"], v["barRight"])
    print("%s：%s" % (tag, room))
    if not v["scrollable"]:
        return
    if v["onLeft"] is None:
        fails.append(tag + "：这一排没有选中态，切页签没生效")
        return
    # 0 容差：第一枚的左边就是条的左边（实测都是 12），留余量会把"贴着边但完整可见"判成被裁。
    if v["onLeft"] < v["barLeft"] - 1 or v["onRight"] > v["barRight"] + 1:
        fails.append(tag + "：选中那一枚被裁在视野外（%s~%s 对 %s~%s）"
                     % (v["onLeft"], v["onRight"], v["barLeft"], v["barRight"]))


def main():
    width = int(sys.argv[1]) if len(sys.argv) > 1 else 390
    height = int(sys.argv[2]) if len(sys.argv) > 2 else 844
    fails = []
    b = Browser(width, height)
    try:
        b.goto(PANEL + "/?v=%d#/mods" % int(time.time() * 1000))
        if not b.wait("!!document.querySelector('.login-form, .tabs')", tries=90):
            print("FAIL 面板没起来：%s" % PANEL)
            return 1
        sc.login(b)
        b.wait("document.querySelectorAll('.tabs button').length >= 3", tries=60)
        check(b.js(READ), "进页（第 1 枚选中）", fails)

        # 1. 用户自己点最后一枚
        if not sc.click_js(b, ".tabs button", "t => t.innerText.indexOf('开关') >= 0"):
            fails.append("找不到「这个房间的开关」那一枚")
        else:
            b.wait("document.querySelectorAll('.mod-line').length > 0", tries=60)
            time.sleep(0.9)
            check(b.js(READ), "点最后一枚", fails)

        # 先点回第 1 枚：不然第 2 段一上来选中的已经是最后一枚，那条来路等于没验。
        if not sc.click_js(b, ".tabs button", "t => t.innerText.indexOf('找模组') >= 0"):
            fails.append("回不到第 1 屏")
        else:
            b.wait("document.querySelectorAll('.mod-cards .mod-card').length > 0", tries=60)
            time.sleep(0.9)
            check(b.js(READ), "点回第 1 枚", fails)

        # 2. 代码切过去：#85 点完「订阅并开启」以后组件自己跳到开关那一屏，
        #    那条来路没有 click 事件，只有 .on 这一处 class 变更。
        #    这里不真点那枚按钮（会往夹具和两个世界的文件里写条目，探针不该留东西），
        #    改成只改 class：Vue 切 :class 绑定时就是原地改这一枚的 class，
        #    观察者看到的 DOM 变化一模一样。
        moved = b.js(r"""(() => {
          const bar = document.querySelector('.tabs');
          const list = [...bar.querySelectorAll('button')];
          const cur = bar.querySelector('button.on');
          const last = list[list.length - 1];
          if (cur === last) return 'already';
          cur.classList.remove('on');
          last.classList.add('on');
          return 'moved';
        })()""")
        print("只改 class 的切换（无 click 事件）：%s" % moved)
        time.sleep(0.9)
        check(b.js(READ), "代码切到最后一枚", fails)
        # 换回真实状态，别把界面留成一个假选中
        b.reload()
        b.wait("!!document.querySelector('.login-form, .tabs')", tries=60)
        sc.login(b)
        b.wait("document.querySelectorAll('.tabs button').length >= 3", tries=60)

        # 3. 整页重进：页签条是新建的，只有 childList 记录
        b.goto(PANEL + "/?v=%d#/room" % int(time.time() * 1000))
        b.wait("!!document.querySelector('.login-form, .tabs')", tries=90)
        sc.login(b)
        b.wait("document.querySelectorAll('.tabs button').length > 0", tries=60)
        time.sleep(0.8)
        v = b.js(READ)
        print("开服那一页（%d 枚一排）：%s" % (v["count"], json.dumps(v, ensure_ascii=False)))
        if v["scrollable"] and (v["onLeft"] < v["barLeft"] - 1 or v["onRight"] > v["barRight"] + 1):
            fails.append("整页重进时选中那一枚在视野外（开服那一页）")
        # 六枚一排才是这一条的最坏情况：最后一枚离视野最远，滚到底才看得见。
        if not sc.click_js(b, ".tabs button", "t => t.innerText.indexOf('令牌') >= 0"):
            fails.append("开服那一页找不到最后一枚页签")
        else:
            time.sleep(1.6)
            check(b.js(READ), "开服那一页点最后一枚（6 枚一排）", fails)
        b.shoot(os.path.join(HERE, "tab86_room_%d.png" % width))
    finally:
        b.close()

    print("\n合计失败 %d 项%s" % (len(fails), ("：" + " | ".join(fails)) if fails else ""))
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
