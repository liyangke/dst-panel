#!/bin/bash
# 等本地 tar/ssh 传输进程全部退出，再做逐文件核对。
# 为什么轮询而不是通知：这两个流是我从两个后台任务里起的，没有 pid 文件可等。
for i in $(seq 1 360); do
  if ! ps -W 2>/dev/null | grep -qE '/usr/bin/(tar|ssh)$'; then
    echo "=== 传输进程已退出 $(date +%H:%M:%S) ==="
    break
  fi
  sleep 20
done
bash .tools/xfer_verify.sh 2>&1
