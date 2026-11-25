#!/bin/bash
# 重新构建两个目标 + 立刻核对产物身份。
# 之前吃过一次亏：dstpanel-linux 其实是上一版 Windows PE 留下的同名旧文件，
# 尺寸和 sha256 跟 dstpanel.exe 一模一样，差点被我当成"交叉构建成功"的证据。
# 所以现在一律先删再建，并且直接验 magic。
export PATH="/c/coding/go/sdk/go1.23.1.windows-amd64/go/bin:$PATH"
export GOTOOLCHAIN=local
set -e
rm -f .tmp_bin/dstpanel.exe .tmp_bin/dstpanel-linux
go build -o .tmp_bin/dstpanel.exe ./cmd/dstpanel
GOOS=linux GOARCH=amd64 go build -o .tmp_bin/dstpanel-linux ./cmd/dstpanel
ls -la .tmp_bin/
sha256sum .tmp_bin/dstpanel.exe .tmp_bin/dstpanel-linux
python - <<'PY'
for p in ['.tmp_bin/dstpanel.exe', '.tmp_bin/dstpanel-linux']:
    head = open(p, 'rb').read(4)
    kind = 'ELF(linux)' if head == b'\x7fELF' else 'PE(windows)' if head[:2] == b'MZ' else '??'
    blob = open(p, 'rb').read()
    marks = ['collect_status', 'panel_bridge', 'GetPersistentString', 'poll_interval',
             'DSTPanelHelper', 'modmain.lua', 'ismastersim']
    print('%s  %s  %d bytes' % (p, kind, len(blob)))
    for m in marks:
        print('   %-22s x%d' % (m, blob.count(m.encode())))
PY
