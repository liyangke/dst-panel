#!/bin/bash
# 看专用服务端到底自带哪些脚本，以及 workshop 这个词在包里出现在哪。
SRC=/d/dst-transfer/server_linux
echo "=== bin64/scripts 里的文件数 ==="
ls "$SRC/bin64/scripts" | wc -l
ls "$SRC/bin64/scripts" | head -40
echo "=== 有没有 mods 相关文件 ==="
ls "$SRC/bin64/scripts" | grep -iE "^mod|shard|network" | head
echo "=== mods 目录 ==="
ls -la "$SRC/mods" | head
echo "=== 含 workshop 字样的脚本 ==="
grep -rIl --exclude-dir=content -i workshop "$SRC/bin64/scripts" 2>/dev/null | head
