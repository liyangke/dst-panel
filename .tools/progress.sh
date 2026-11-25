#!/bin/bash
echo "--- 机房后台下载进度"
tail -4 ~/dst_dl.log 2>/dev/null | cat -v
echo "--- 落盘量"
du -sh ~/dst/dedicated_server 2>/dev/null || echo "  仍未落盘（还在 steamcmd 自更新阶段）"
echo "--- 进程"
ps -eo pid,etimes,rss,args | grep -E "linux32/steamcmd" | grep -v grep | head -3 | cut -c1-120
echo "--- 自更新是否已完成(完成会出现 Installing update / Update complete)"
grep -acE "Update complete|Installing update" ~/dst_dl.log 2>/dev/null | sed 's/^/  匹配行数: /'
echo "--- 磁盘余量"
df -h / | tail -1
