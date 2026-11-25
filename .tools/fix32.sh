#!/bin/bash
set +e
echo "=== [1] 启用 i386 架构并装 32 位运行时"
sudo -n dpkg --add-architecture i386 && echo "    i386 已启用"
echo "    开始 apt update: $(date +%T)"
sudo -n apt-get update -qq > /tmp/apt_update.log 2>&1
echo "    apt update rc=$? 结束: $(date +%T)"
tail -5 /tmp/apt_update.log | sed 's/^/      /'

echo
echo "=== [2] 安装 32 位依赖"
sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -qq lib32gcc-s1 libc6-i386 lib32stdc++6 > /tmp/apt_install.log 2>&1
echo "    rc=$?"
tail -8 /tmp/apt_install.log | sed 's/^/      /'
echo "    32位加载器现在: $(ls -la /lib/ld-linux.so.2 2>/dev/null || echo 仍缺失)"
dpkg --print-foreign-architectures | sed 's/^/    foreign arch: /'

echo
echo "=== [3] 立刻复测:直接尝试下载 app 343050 共 240 秒,量实测吞吐"
cd ~/tools/steamcmd || exit 9
file linux32/steamcmd 2>/dev/null | sed 's/^/    /'
before=$(date +%s)
timeout 240 bash ./steamcmd.sh +login anonymous +force_install_dir "$HOME/dst/dedicated_server" +app_update 343050 validate +quit < /dev/null > /tmp/sc2.log 2>&1
rc=$?
after=$(date +%s)
echo "    rc=$rc 用时 $((after-before)) 秒 (124=超时中断,属正常)"
head -25 /tmp/sc2.log | cat -v | sed 's/^/      /'

echo
echo "=== [4] 判定:连接与下载量"
grep -aiE "Logging in|Connected to|Requesting job|Received job|Pending|Downloading|Preventing|Success|ERROR|Failed|Timeout|Unable to connect|No connection|Retrying" /tmp/sc2.log | head -20 | sed 's/^/    /'
got=$(du -sb "$HOME/dst/dedicated_server" 2>/dev/null | awk '{print $1}')
got=${got:-0}
echo "    落盘字节: $got"
echo "    实测吞吐: $(awk -v b=$got -v t=$((after-before)) 'BEGIN{printf "%.0f KB/s", b/1024/(t>0?t:1)}')"
echo "    推算 4.5GB 需要: $(awk -v b=$got -v t=$((after-before)) 'BEGIN{if(b<=0){print "无法推算(没下动)"}else{printf "%.0f 分钟", 4.5*1024*1024*1024/(b/t)/60}}')"
