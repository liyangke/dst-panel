#!/bin/bash
echo "=== A. 当前 DNS 配置"
resolvectl status 2>/dev/null | grep -E 'DNS Servers|DNS Domain|Link|Protocol' | head -20 | sed 's/^/    /'
echo "    /etc/resolv.conf:"; grep -E '^(nameserver|options)' /etc/resolv.conf 2>/dev/null | sed 's/^/      /'

echo
echo "=== B. 用干净的 DoH 重新解析,判断是'污染'还是'真不可达'"
# 只取 A 记录,不跟随 CNAME 目标,看权威答案
q() {
  local host="$1" api="$2" name="$3"
  echo "    --- $host via $name"
  curl -m 15 -sS -H 'accept: application/dns-json' "$api?name=$host&type=A" 2>&1 | head -c 400
  echo
}
q client-download.steampowered.com "https://dns.alidns.com/resolve" "AliDNS"
q steamcontent.com "https://dns.alidns.com/resolve" "AliDNS"
q client-download.steampowered.com "https://doh.pub/resolve" "DNSPod"

echo
echo "=== C. 腾讯/阿里公共 DNS 直连查询(53端口是否被封)"
python3 - <<'PYEOF'
import socket, struct, sys

def build(name):
    q = b'\xe4\xA3\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00'
    for part in name.split('.'):
        q += bytes([len(part)]) + part.encode()
    q += b'\x00\x00\x01\x00\x01'
    return q

def ask(server, name):
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(6)
        s.sendto(build(name), (server, 53))
        data, _ = s.recvfrom(4096)
        # 极简解析:扫描尾部 4 字节候选 IPv4
        ips = []
        i = 12
        # 跳过 question
        while i < len(data) and data[i] != 0:
            i += data[i] + 1
        i += 5
        # 解析 answer
        while i < len(data):
            # name(压缩指针 2字节)
            if data[i] & 0xC0:
                i += 2
            else:
                while i < len(data) and data[i] != 0:
                    i += data[i] + 1
                i += 1
            if i + 10 > len(data):
                break
            rtype, rclass, ttl, rdlen = struct.unpack('>HHIH', data[i:i+10])
            i += 10
            rd = data[i:i+rdlen]
            i += rdlen
            if rtype == 5:
                cname = '.'.join(bytes(b).decode() for b in rd.split(b'\xc0\x0c')[0:1]) if False else '(CNAME)'
                print(f'      [{server}] {name} CNAME -> {cname}')
            elif rtype == 1 and len(rd) == 4:
                ip = '.'.join(str(b) for b in rd)
                print(f'      [{server}] {name} A -> {ip}')
        if not ips:
            pass
    except Exception as e:
        print(f'      [{server}] {name} 失败: {type(e).__name__}: {e}')

for srv in ('223.5.5.5', '119.29.29.29'):
    for host in ('client-download.steampowered.com', 'steamcontent.com', 'api.steampowered.com'):
        ask(srv, host)
PYEOF

echo
echo "=== D. 强制用真实 IP 建连,验证网络层是否可达(排除纯 DNS 问题)"
for spec in "client-download.steampowered.com:23.212.62.214" "steamcontent.com:23.212.62.214" "lancache.steamcommunity.com:104.115.76.44"; do
    h=${spec%%:*}; ip=${spec##*:}
    r=$(curl -m 12 -sS -o /dev/null -w 'code=%{http_code} connect=%{time_connect} size=%{size_download}' --resolve "$h:443:$ip" "https://$h/" 2>&1 | head -c 200)
    echo "    $h -> $ip :: $r"
done
echo
echo "=== E. 真正能用的 Steam 内容源候选:能否下到一个实测文件"
# steamcmd 本体的官方直链在 steamdownload 上
for u in "https://steamcdn-a.akamaihd.net/client/install/steamforlinux/steam_latest_installer.tar.gz" "https://cdn.akamai.steamstatic.com/client/steamcmd_linux.tar.gz" "https://api.steamcmd.net/v1/info/343050"; do
    r=$(curl -m 20 -sS -o /dev/null -w 'code=%{http_code} size=%{size_download} speed=%{speed_download}' "$u" 2>&1 | head -c 160)
    echo "    $r  <- $u"
done

echo
echo "=== F. 全局代理是否已在跑(你机器上有 frp,可能有 http 代理)"
for po in 7890 7892 10809 1080 8118 8888 33306; do
    ss -tlnp 2>/dev/null | grep -q ":$po " && echo "    端口 $po 在听(可能是代理): $(ss -tlnp 2>/dev/null | grep ":$po " | head -1 | awk '{print $6,$7}')"
done
echo "    http_proxy=$http_proxy https_proxy=$https_proxy"

echo
echo "=== G. 已有 frps 开放的高位端口段(避撞)"
cat /etc/frp/frps.toml 2>/dev/null | head -30 | sed 's/^/    /' || echo "    读不到 /etc/frp/frps.toml(需要 root)"
sudo -n head -40 /etc/frp/frps.toml 2>/dev/null | sed 's/^/    /'
