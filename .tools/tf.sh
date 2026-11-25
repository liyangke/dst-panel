#!/bin/bash
# gofmt + go test，完整输出写进 .tmp_bin/last_test.txt，终端只打失败摘要。
# 为什么要这个而不是 t.sh：t.sh 结尾是 tail -40，一次真失败会把"哪个用例挂了"
# 挤掉，我就会被迫去猜（已经吃过两次）。这里失败用例名和它下面的断言原文一定留在屏幕上。
export PATH="/c/coding/go/sdk/go1.23.1.windows-amd64/go/bin:$PATH"
export GOTOOLCHAIN=local
set -e
gofmt -l -w cmd internal
out=.tmp_bin/last_test.txt
mkdir -p .tmp_bin
if [ $# -gt 0 ]; then
  go test -count=1 "$@" > "$out" 2>&1 || true
else
  go test -count=1 ./... > "$out" 2>&1 || true
fi
echo "== 包结果 =="
grep -E '^(ok|FAIL|---|\?)' "$out" | grep -v '^--- PASS' || true
echo "== 失败用例详情（前 80 行）=="
grep -n -A14 '^--- FAIL' "$out" | head -80 || true
