#!/bin/bash
# 让 SteamCMD 在服务器上后台自更新并下载 DST 本体，用 nohup 保证 SSH 断开不影响。
set +e
cd ~/tools/steamcmd || exit 9
if pgrep -f "steamcmd" >/dev/null 2>&1; then
    echo "已有 steamcmd 在跑，跳过启动"
else
    nohup bash ./steamcmd.sh \
        +login anonymous \
        +force_install_dir "$HOME/dst/dedicated_server" \
        +app_update 343050 validate \
        +quit > ~/dst_dl.log 2>&1 &
    echo "已后台启动，PID=$!  日志 ~/dst_dl.log"
fi
sleep 20
echo "--- 20 秒后进度"
tail -3 ~/dst_dl.log | cat -v
echo "--- 落盘"
du -sh "$HOME/dst/dedicated_server" 2>/dev/null || echo "  (还没开始落盘，在自更新阶段)"
