#!/bin/bash
# DST 平台部署前环境摸底：只读，不做任何修改
echo "############ 1. CPU / 内存 / 磁盘真实规格"
echo "--- lscpu 关键项"; lscpu | grep -E '^(Architecture|CPU\(s\)|Thread|Core|Socket|Model name|Flags)' | sed 's/^/    /'
echo "--- nproc(受亲和性影响) = $(nproc)"
echo "--- /proc/cpuinfo 逻辑核数 = $(grep -c ^processor /proc/cpuinfo)"
echo "--- cgroup 内存上限: $(cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null || echo 无)"
free -h | sed 's/^/    /'
echo "--- 精确可用内存(KB)"; grep -E 'MemTotal|MemAvailable' /proc/meminfo | sed 's/^/    /'
df -h / | tail -1 | sed 's/^/    disk /'
echo "--- 是否有 systemd 资源限制"
systemd-detect-virt 2>/dev/null | sed 's/^/    virt=/'

echo
echo "############ 2. 那 5 个不能动的服务在吃什么"
for p in frp 1panel dsh syncrelay agent; do
    echo "--- 匹配关键词: $p"
    pgrep -af "$p" 2>/dev/null | head -5 | sed 's/^/      /'
done
echo "--- systemd 里相关 unit"
systemctl list-units --type=service --state=running --no-pager 2>/dev/null | grep -iE 'frp|panel|dsh|sync|relay|agent' | sed 's/^/      /'
echo "--- docker 容器(1panel 可能跑在容器里)"
docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Ports}}' 2>/dev/null | head -25 | sed 's/^/      /'

echo
echo "############ 3. 当前内存占用排行 TOP15(RSS, MB)"
ps -eo rss,comm,args --sort=-rss --no-headers 2>/dev/null | head -15 | awk '{printf "    %8.1f MB  %s\n", $1/1024, substr($0, index($0,$2))}'
echo "--- 已用量合计"
ps -eo rss --no-headers | awk '{s+=$1} END {printf "    全部进程 RSS 合计 = %.2f GB\n", s/1024/1024}'

echo
echo "############ 4. 端口占用总览(避免和现有服务撞)"
ss -tulnp 2>/dev/null | awk 'NR>1{print $1, $5, $6, $7}' | head -40 | sed 's/^/    /'

echo
echo "############ 5. DST 运行前置条件"
echo "--- screen 版本: $(screen --version 2>/dev/null)"
echo "--- 32 位运行能力(专用服务端老二进制可能需要):"
echo "    i386 架构已启用? $(dpkg --print-foreign-architectures 2>/dev/null | grep -q i386 && echo YES || echo NO)"
echo "    /lib/ld-linux.so.2 (32位加载器)? $(ls /lib/ld-linux.so.2 >/dev/null 2>&1 && echo 存在 || echo 缺失)"
echo "--- gcc/glibc 版本: $(ldd --version 2>/dev/null | head -1)"
echo "--- steamcmd 是否已存在: $(command -v steamcmd || ls ~/steamcmd 2>/dev/null | head -1 || echo 未安装)"
echo "--- 已存在的 DST 相关目录:"
ls -d ~/.klei ~/dst* ~/*steam* 2>/dev/null | sed 's/^/      /' || echo "      无"

echo
echo "############ 6. Steam 连通性(决定 MOD 下载设计)"
for u in "https://api.steamcmd.net/v1/info/343050" "https://steamcommunity.com" "https://api.steampowered.com/ISteamRemoteStorage/GetPublishedFileDetails/v1/"; do
    code=$(curl -m 12 -s -o /dev/null -w '%{http_code}' "$u" 2>/dev/null)
    echo "    [$code] $u"
done
echo "--- Steam 内容分发 CDN 端口(27017/443)可达性"
for hp in "client-download.steampowered.com:443" "steamcontent.com:443"; do
    h=${hp%%:*}; po=${hp##*:}
    timeout 8 bash -c "echo > /dev/tcp/$h/$po" 2>/dev/null && echo "    OK    $hp" || echo "    FAIL  $hp"
done

echo
echo "############ 7. 出网带宽参考(下 4.5GB 服务端要多久)"
sz=$(curl -m 20 -s -o /dev/null -w '%{size_download}' "https://cdn.cloudflare.steamstatic.com/steam/apps/233777/116233582f6706fbed0333195688353e19938c9e.ico" 2>/dev/null)
echo "    小文件测试字节: $sz"
echo
echo "摸底结束"
