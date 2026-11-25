#!/bin/bash
# 只报读数的传输进度检查：本地源 vs 云端目标。
echo "=== 本地源 ==="
du -sh /d/dst-transfer/server_linux
find /d/dst-transfer/server_linux -type f | wc -l

echo "=== 云端目标 ==="
ssh dst-cloud "D=/home/ubuntu/dst/dedicated_server; du -sh \"\$D\"; find \"\$D\" -type f | wc -l; ls -l \"\$D/bin64/dontstarve_dedicated_server_nullrenderer_x64\" | awk '{print \$1, \$5}'; date +%T"

echo "=== 本地是否还有 tar/ssh 在传 ==="
ps -W 2>/dev/null | grep -iE "tar|ssh" | head -5
