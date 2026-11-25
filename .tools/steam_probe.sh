#!/bin/bash
# 决定性实验:SteamCMD 能否连上内容服务器并下载 app 343050
# 必须落盘执行,不能让 steamcmd 从 stdin 抢走脚本剩余内容
set +e
cd ~/tools/steamcmd || exit 9

echo "=== [1] 环境确认"
ls steamcmd.sh >/dev/null 2>&1 && echo "    steamcmd.sh 存在" || echo "    steamcmd.sh 缺失"
echo "    32位加载器: $(ls /lib/ld-linux.so.2 >/dev/null 2>&1 && echo 有 || echo 无)"

echo
echo "=== [2] 尝试 app_update 343050(最多 240 秒)"
echo "    开始: $(date +%T)"
timeout --preserve-status 240 bash ./steamcmd.sh \
    +login anonymous \
    +force_install_dir "$HOME/dst/dedicated_server" \
    +app_update 343050 \
    progress \
    +quit < /dev/null > steamcmd_run.log 2>&1
rc=$?
echo "    结束: $(date +%T)  rc=$rc"

echo
echo "=== [3] 关键行"
grep -aE "Logging in|Connected|Waiting for job|Pending update|Preventing download|Downloading|Bytes Received|Success|ERROR|Failed|Timeout|Couldn.t|No update|Unable|dlopen|error while loading" steamcmd_run.log | head -30 | sed 's/^/    /'

echo
echo "=== [4] 原始输出全文前 60 行"
head -60 steamcmd_run.log | cat -v | sed 's/^/    /'

echo
echo "=== [5] 落盘量"
du -sh "$HOME/dst/dedicated_server" 2>/dev/null | sed 's/^/    /' || echo "    无目录"
echo "    文件数: $(find "$HOME/dst/dedicated_server" -type f 2>/dev/null | wc -l)"
