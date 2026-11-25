#!/bin/sh
# 本地验收一轮：按顺序跑，每个脚本自己开一个浏览器页签驱动 8963 上的面板。
# 放一个脚本里是为了一次看全红绿，而不是跑一个看一个。
#
# 开头必须先重启一次 ui_dev：api_probe 和界面实测共用 .tmp_boot/klei 那棵树，
# 探针会删掉日志/地图/模组那几份夹具文件。不重新播种的话，界面脚本报的"页面没出来"
# 其实是夹具没了，看着像产品缺陷。
#
# 每一步的输出去向：先进 .tmp_boot/acceptance/<步骤>.log，屏上只留尾巴几行，
# 但退出码是自己收的。之前这里写成 `python xxx.py | tail -4`，管道把退出码换成了
# tail 的 0，于是 ui_filters_check 报着 2 条红、整轮还是 exit 0，看着像全绿。
# 屏上省行可以，拿掉红不行。
set -u
export PYTHONIOENCODING=utf-8
# 输出必须无缓冲。上一轮 ui_filters_check 那两步报红、日志却是 0 字节：
# 它把整份报告攒到退出时才打印，而 stdout 一重定向就是按块缓冲，脚本中途抛异常
# 就等于把已经跑出来的那几十行一起丢掉。少了现场，那种红只能靠猜。
export PYTHONUNBUFFERED=1
LOGD=.tmp_boot/acceptance
mkdir -p "$LOGD"
FAILS=""

run() {
    # run 标签 屏上留几行 命令…
    label="$1"; lines="$2"; shift 2
    echo "##### $label"
    "$@" > "$LOGD/$label.log" 2>&1
    rc=$?
    tail -n "$lines" "$LOGD/$label.log"
    if [ "$rc" -ne 0 ]; then
        echo "↑ $label 退出码 $rc，完整输出：$LOGD/$label.log"
        FAILS="$FAILS
- $label（exit $rc）"
    fi
}

python .tools/ui_dev.py --stop >/dev/null 2>&1
python .tools/ui_dev.py > "$LOGD/ui_dev.log" 2>&1 \
  || { echo "ui_dev 没起来，后面的界面脚本都会假失败（$LOGD/ui_dev.log）"; exit 1; }
sleep 2

# 体检单独跑、完整输出：它现在有十二条检查，跟着循环用 tail 会把前面几条的报错
# 顶出屏幕，红着也看着像"全绿"。
echo "##### ui_audit"
python .tools/ui_audit.py > "$LOGD/ui_audit.log" 2>&1
rc=$?
cat "$LOGD/ui_audit.log"
if [ "$rc" -ne 0 ]; then
    echo "↑ ui_audit 退出码 $rc"
    FAILS="$FAILS
- ui_audit（exit $rc）"
fi

for s in markers_check ui_flow_check ui_drop_check ui_checklist_check ui_ports_check ui_stepper_check; do
    run "$s" 4 python .tools/$s.py
done

# 端口那一条要先停服才改得动，于是把 fake_screen 掏空了。
# 不写回的话，下面两组检查看到的"没有任何房间在跑"会让总览默认落到没数据的房间，
# 报出一串"界面 0 点／接口 271 点"的假失败。
python .tools/ui_dev.py --reseed >/dev/null 2>&1
for w in 1440 390; do
    run "ui_filters_check_$w" 14 python .tools/ui_filters_check.py "$w"
    python .tools/ui_dev.py --reseed >/dev/null 2>&1
done

# 玩家事件表：逐档点「动作」下拉、列设置的收放、九列不错位、点芯片跳回逐行日志、
# 把回看窗口收到最小再看空状态。
# 这一页最容易出的三种错——筛选项点过去是一张空表（假链接）、码值漏到界面上、
# 窗口收窄了却不说明只数了 N 行——都只在浏览器里看得见，接口测试全绿也照样会犯。
# 必须放在 --reseed 之后：脚本靠页面默认落到有日志的那个房间，
# 上面端口那一条把夹具掏空过，默认落点会变成一个没日志的房间。
for w in 1440 390; do
    run "ui_events_check_$w" 8 python .tools/ui_events_check.py "$w"
done
python .tools/ui_dev.py --reseed >/dev/null 2>&1

echo "##### ui_import_check 1440"
# 导入那几条要真文件上传，包是本地造的，先重造一遍免得上一轮把它删了。
python .tools/ui_import_zip.py >/dev/null 2>&1
run ui_import_check 6 python .tools/ui_import_check.py 1440

# 模组页那一条一次点完的动线（#85）：卡片上那枚按钮的文案能不能从接口的
# registered/usedBy 一格一格推出来、点完吐司/卡片/待下载/开关四处口径是否一致、
# 落盘是不是真文件、"清单里有它还没下载"和"清单里也没了"两种缺目录徽标各自说各自的。
# 它会真写夹具（订阅→开→取消→还原），跑完补一次 reseed 把下载清单铺回去。
for w in 1440 390; do
    run "subscribe85_$w" 6 python .tools/subscribe85_check.py "$w"
done
python .tools/ui_dev.py --reseed >/dev/null 2>&1
# 窄屏页签条：选中那一枚要完整在视野里，三种来路（点击 / 代码切 / 整页新建）各量一次。
run tab86_390 8 python .tools/tab86_check.py 390 844

# 天数那一格：引擎的世界状态上没有 day，"第几天"是面板从 cycles 现算的（就是那个
# 每个房间都显示「第 0 天」的 bug，见学习指南第 5 节第 16 条）。三页各量一次画出来的数；
# 脚本里还会把地表夹具挪开一次，验"只有洞穴在回话"时那一格换成洞穴的天数而不是冒充地表。
run day92_check 8 python .tools/day92_check.py
python .tools/ui_dev.py --reseed >/dev/null 2>&1

# 拆干净没有：公网页那枚「照这间开服」和向导第 2 步那排「常见配置起个头」都已经下线
# （取舍见 .retired/README.md）。这里验两处页面都不留半截按钮或半截说明、向导默认还是
# 无尽·合作·6 人·PVP 关，并改成"手动改三栏再走汇总"来钉住界面显示 == 真发出去。
run lobby93_check 8 python .tools/lobby93_check.py

# 总览这一屏的六条（CPU 型号 / 平均负载的解释 / 房间详情那块 / 进程占用采的是谁 /
# 趋势标题用房间名 / 房间名点击复制的可发现性）。全在前端 + 采集层，接口探针碰不到，
# 而其中"占用那一格是不是 2 MB 的空壳"只有 Linux 上量得出来——本机跑会打出跳过的 · 行。
python .tools/ui_dev.py --reseed >/dev/null 2>&1
run overview96_check 10 python .tools/overview96_check.py

# 总览整屏与玩家页整屏这两支是"五档 × 全流程"的重探针（各五十几条断言），
# 一支盯总览这一屏的形状（两栏/页签/两张图卡的分母/#126 那两个新数），
# 一支盯 #125 搬过去的泳道（落点、档位是同一份、几何与接口逐条核）。
# 都放在 reseed 之后：它们靠页面默认落到有玩家行为记录的那间房，
# 上面端口那一条把夹具掏空过的话会报出一串"界面 0 段／接口 271 段"的假失败。
python .tools/ui_dev.py --reseed >/dev/null 2>&1
run overview105_check 12 python .tools/overview105_check.py
run players125_check 12 python .tools/players125_check.py

# 免构建 Vue 没有编译期：组件里引用一个没 import 的标识符是在模块求值时炸的，
# 炸完连登录页都渲染不出来，而截图上看着只像"页面没数据"。静态那条已经并进 ui_audit
# 的第十一、十二，这里留浏览器侧的两条，放在最后跑：这时夹具最完整，
# 扫出来的空页才是真问题。
python .tools/ui_dev.py --reseed >/dev/null 2>&1
run ui_owner_check 6 python .tools/ui_owner_check.py
run js_error_sweep_wide 3 python .tools/js_error_sweep.py --wide
run js_error_sweep_narrow 3 python .tools/js_error_sweep.py

echo
if [ -n "$FAILS" ]; then
    echo "===== 这一轮有步骤报了非 0：$FAILS"
    exit 1
fi
echo "===== 全部步骤退出码 0（各步完整输出在 $LOGD/）"
