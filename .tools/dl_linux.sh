#!/bin/bash
# 重新拉 Linux 版服务端：SteamCMD 默认按当前操作系统选 depot，
# 在 Windows 上跑会拿到 .exe/.dll，云服务器用不了。
# 用 @sSteamCmdForcePlatformType linux 强制选 Linux depot，装到独立目录避免和 Windows 版混。
set +e
WORK=/d/dst-transfer
SERVER=$WORK/server_linux
mkdir -p "$SERVER" "$WORK/logs"
cd "$WORK/steamcmd" || exit 9

./steamcmd.exe \
    +@sSteamCmdForcePlatformType linux \
    +force_install_dir "D:/dst-transfer/server_linux" \
    +login anonymous \
    +app_update 343050 validate \
    +quit > "$WORK/logs/dl_linux.log" 2>&1
echo "rc=$?"
tail -c 600 "$WORK/logs/dl_linux.log" | tr '\r' '\n' | grep -v '^$' | tail -6
echo "=== 结构核对 ==="
ls "$SERVER" 2>/dev/null | head
echo "--- bin64 ---"
file "$SERVER"/bin64/* 2>/dev/null | head -5
ls -la "$SERVER"/bin64 2>/dev/null | head -8
du -sh "$SERVER"
