#!/bin/bash
# 把 Linux 版 DST 专用服务端整包推到云服务器。
# 为什么不用 scp -r：一万多个小文件，scp 每个文件一次往返；tar 单流管道跑满带宽。
# 为什么不用 gzip：包里全是已压缩的资源，压不动还占 CPU。
# Windows 打的 tar 不带可执行位，解完必须补 chmod，否则分片起不来。
set +e
SRC=/d/dst-transfer/server_linux
DEST_HOST=dst-cloud
# 必须写绝对路径：本地脚本里的 ~ 会被本地 shell 展开成 Windows 家目录。
DEST_DIR=/home/ubuntu/dst/dedicated_server

echo "=== 源目录 ==="
du -sh "$SRC"
find "$SRC" -type f | wc -l

echo "=== 清理云端旧内容 $(date +%H:%M:%S) ==="
ssh "$DEST_HOST" 'rm -rf $0 && mkdir -p $0' "$DEST_DIR"

echo "=== 传输开始 $(date +%H:%M:%S) ==="
tar -C "$SRC" -cf - . | ssh "$DEST_HOST" 'tar -xf - -C "'"$DEST_DIR"'"'
echo "tar rc=$?"

echo "=== 补权限并核对 $(date +%H:%M:%S) ==="
ssh "$DEST_HOST" 'D="'"$DEST_DIR"'";
 chmod 755 "$D"/bin64/dontstarve_dedicated_server_nullrenderer_x64 "$D"/bin64/dst_crashreporter "$D"/bin64/dontstarve 2>/dev/null;
 chmod -R a+rX "$D"/data "$D"/mods 2>/dev/null;
 du -sh "$D"; find "$D" -type f | wc -l;
 echo "--- 关键文件 ---";
 ls -la "$D"/bin64/dontstarve_dedicated_server_nullrenderer_x64;
 file "$D"/bin64/dontstarve_dedicated_server_nullrenderer_x64 | cut -c1-120;
 ldd "$D"/bin64/dontstarve_dedicated_server_nullrenderer_x64 2>&1 | grep -i "not found" | head -5 || echo "  依赖完整";
 head -3 "$D"/mods/dedicated_server_mods_setup.lua'
