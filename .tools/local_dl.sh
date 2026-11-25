#!/bin/bash
# 本地拉取 DST 专用服务端(343050)，拉完再 rsync/scp 上云服务器。
# 背景：云服务器自身到 Steam 内容分发节点只有 ~45 KB/s，本地实测 ~870 KB/s。
set +e
WORK=/d/dst-transfer

mkdir -p "$WORK/steamcmd" "$WORK/server" "$WORK/logs" || exit 9
cd "$WORK/steamcmd" || exit 9

if [ ! -f steamcmd.exe ]; then
    python - <<'PY'
import urllib.request, zipfile, io
url = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"
data = urllib.request.urlopen(url, timeout=60).read()
print("steamcmd.zip bytes=%d" % len(data))
z = zipfile.ZipFile(io.BytesIO(data))
names = z.namelist()
print("entries=%s" % names)
z.extractall(".")
PY
fi
ls -la

echo "=== force_install_dir=D:/dst-transfer/server, app_update 343050 ==="
./steamcmd.exe +force_install_dir "D:/dst-transfer/server" +login anonymous +app_update 343050 validate +quit \
    > "$WORK/logs/dl.log" 2>&1
echo "rc=$?"
tail -c 400 "$WORK/logs/dl.log" | tr '\r' '\n' | tail -6
