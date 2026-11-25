#!/bin/bash
# 单独跑一次交叉构建，把 stderr 和退出码原样留着，判断 dstpanel-linux 为什么是 PE。
export PATH="/c/coding/go/sdk/go1.23.1.windows-amd64/go/bin:$PATH"
export GOTOOLCHAIN=local

rm -f .tmp_bin/dstpanel-linux
echo "=== go env ==="
go env GOOS GOARCH
echo "=== build ==="
GOOS=linux GOARCH=amd64 go build -o .tmp_bin/dstpanel-linux ./cmd/dstpanel
echo "exit=$?"
echo "=== ls ==="
ls -la .tmp_bin/
