#!/bin/bash
# 在服务端包里找引擎自带的 modindex.lua 等脚本，确认 workshop 模组是怎么被发现的。
SRC=/d/dst-transfer/server_linux
echo "=== scripts 目录 ==="
find "$SRC" -maxdepth 4 -type d -name scripts | head
echo "=== modindex / modutil ==="
find "$SRC" -name "modindex.lua" -o -name "modutil.lua" -o -name "mods.lua" | head
echo "=== steamapps ==="
ls -la "$SRC/steamapps" 2>/dev/null | head
echo "=== workshop content ==="
find "$SRC/steamapps/workshop" -maxdepth 3 2>/dev/null | head
