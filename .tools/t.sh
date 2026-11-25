#!/bin/bash
# gofmt + go test，参数全部传 ASCII 包路径，避免中文进 shell 命令行（会 exit 127）。
export PATH="/c/coding/go/sdk/go1.23.1.windows-amd64/go/bin:$PATH"
export GOTOOLCHAIN=local
set -e
gofmt -l -w cmd internal
if [ $# -gt 0 ]; then
  go test -count=1 "$@" 2>&1 | tail -40
else
  go test -count=1 ./... 2>&1 | tail -40
fi
