#!/bin/bash
# 连跑两次实机探测，看有没有偶发的时序抖动（任务队列那组检查依赖真子进程的耗时）。
# 命令里不能出现中文：Git Bash 会直接 exit 127，所以过滤条件也只能是 ASCII。
set -u
cd "$(dirname "$0")/.."
RUNS=${1:-2}
for i in $(seq 1 "$RUNS"); do
  echo "=== run $i ==="
  python .tools/api_probe.py >".tmp_boot/probe_run$i.txt" 2>&1
  echo "rc=$?"
  echo "PASS=$(grep -c '^\[PASS\]' ".tmp_boot/probe_run$i.txt")  FAIL=$(grep -c '^\[FAIL\]' ".tmp_boot/probe_run$i.txt")"
  tail -2 ".tmp_boot/probe_run$i.txt"
  echo "--- last checks ---"
  grep -n '^\[' ".tmp_boot/probe_run$i.txt" | tail -12
done
