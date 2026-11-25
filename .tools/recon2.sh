#!/bin/bash
echo "=== A. DNS 解析:Steam 相关域名到底解析到什么"
for h in client-download.steampowered.com steamcontent.com cdn.steamcommunity.com api.steampowered.com steamcommunity.com client-http01.officialservers.content.steamfoundation.org; do
    ip=$(getent hosts "$h" 2>/dev/null | awk '{print $1}' | head -1)
    echo "    $h -> ${ip:-解析失败}"
done

echo
echo "=== B. TCP 建连能力(用 curl 显式计时,区分超时/拒绝/DNS)"
for h in client-download.steampowered.com steamcontent.com steamcommunity.com; do
    out=$(curl -m 10 -sS -o /dev/null -w 'code=%{http_code} connect=%{time_connect} total=%{time_total}' "https://$h/" 2>&1)
    echo "    $h :: $out"
done

echo
echo "=== C. UDP 27017(SteamCMD 内容分发走 UDP,常被云机房封) "
timeout 6 bash -c 'cat > /tmp/ut.py <<PYEOF
import socket,sys
try:
    s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.settimeout(5)
    s.sendto(b"\xff\xff\xff\xffTSource Engine Query\x00ff",(sys.argv[1],27017))
    d=r=s.recvfrom(1024); print("    UDP 27017 有响应:",len(d[0]),"字节")
except Exception as e:
    print("    UDP 27017 无响应:",type(e).__name__,e)
PYEOF
python3 /tmp/ut.py client-download.steampowered.com' 2>&1

echo
echo "=== D. docker 到底能不能用(ubuntu 用户)"
docker ps 2>&1 | head -3
echo "    groups: $(id -nG)"
echo "    sudo docker ps:"; sudo -n docker ps --format '{{.Names}} {{.Ports}}' 2>&1 | head -15

echo
echo "=== E. 实测 SteamCMD 能不能动(先只下载 steamcmd 本体 10MB 级)"
mkdir -p /tmp/steamtest && cd /tmp/steamtest
curl -m 60 -sS -o sc.tar.bz2 "https://steamcdn-a.akamaihd.net/client/install/steamforlinux/steam_latest_installer.tar.gz" 2>&1 | head -2
ls -la sc.tar.bz2 2>/dev/null | awk '{print "    下载字节: "$5}'
echo "--- 备用源:github 上的 steamcmd 发布"
curl -m 25 -sSL -o /dev/null -w '    repo.steamcmd.net code=%{http_code} time=%{time_total}\n' "https://repo.steamcmd.net/" 2>&1 | head -2

echo
echo "=== F. 通用外网速度参照(判断是 Steam 被墙还是整体慢)"
curl -m 25 -sS -o /dev/null -w '    tunamirror  code=%{http_code} size=%{size_download} speed=%{speed_download}B/s\n' "https://mirrors.tuna.tsinghua.edu.cn/" 2>&1 | head -1
curl -m 25 -sS -o /dev/null -w '    github      code=%{http_code} size=%{size_download} speed=%{speed_download}B/s\n' "https://github.com/" 2>&1 | head -1
curl -m 25 -sS -o /dev/null -w '    cloudflare  code=%{http_code} size=%{size_download} speed=%{speed_download}B/s\n' "https://cdnjs.cloudflare.com/ajax/libs/jquery/3.7.1/jquery.min.js" 2>&1 | head -1

echo
echo "=== G. DST 默认端口段是否空闲(10998-11002)"
for po in 10997 10998 10999 11000 11001 11002; do
    ss -tulnp 2>/dev/null | grep -q ":$po " && echo "    $po 被占用" || echo "    $po 空闲"
done
echo "    注:37000/37400 这类端口更像 frp 转发段,后面规划端口要避开"

echo
echo "=== H. 系统内核与安全模块(新 glibc 上跑老二进制的风险点)"
echo "    kernel: $(uname -r)"
echo "    systemd: $(systemctl --version 2>/dev/null | head -1)"
echo "    1panel/frp 之后是否还有空间装 4.5GB:"; df -h / | tail -1 | awk '{print "      剩余 "$4}'
