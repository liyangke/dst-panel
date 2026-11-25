#!/usr/bin/env bash
# 卸载 DST 面板与这套业务的全部数据。
#
#   sudo DST_USER=dst ./deploy/uninstall.sh            # 会先问一次
#   sudo DST_USER=dst ./deploy/uninstall.sh --yes      # 给脚本用
#
# 删的东西：systemd 单元、面板二进制、运行用户连同它的家目录（= 业务根目录，
# 里面是引擎本体、存档、MOD、steamcmd、面板数据库与备份）。
#
# 为什么敢用 userdel -r 一把梭：布局的前提就是"面板运行用户的 HOME 就是业务根目录"
# （见 internal/config/config.go），所以这一棵树之外不该有任何数据。
# 但前提不等于事实，所以脚本最后必须自己扫一遍全盘并公开结果——
# 卸载脚本最大的失败模式是"它说自己删干净了"。
set -euo pipefail

DST_USER="${DST_USER:-dst}"
ASSUME_YES=0
for a in "$@"; do
  case "$a" in
  --yes | -y) ASSUME_YES=1 ;;
  *) echo "不认识的参数：$a（只有 --yes）" >&2; exit 1 ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "要用 root 跑：sudo $0" >&2
  exit 1
fi
if ! id "$DST_USER" >/dev/null 2>&1; then
  echo "没有用户 $DST_USER，这台机器上大概没装过（或装在了别的用户下）。" >&2
  echo "先确认单元还在不在：systemctl status dstpanel.service" >&2
  exit 1
fi

DST_ROOT=$(getent passwd "$DST_USER" | cut -d: -f6)
if [ -z "$DST_ROOT" ] || [ "$DST_ROOT" = "/" ] || [ "$DST_ROOT" = "/home" ] || [ "$DST_ROOT" = "/data" ]; then
  echo "$DST_USER 的家目录是 '$DST_ROOT'，看着不像业务根目录，卸载停在这里。" >&2
  echo "这个脚本会连家目录一起删，不敢拿它猜。请先把家目录改对，或手工处理。" >&2
  exit 1
fi
if mountpoint -q "$DST_ROOT"; then
  echo "$DST_ROOT 是一个挂载点，删目录会连带删掉那块盘上的所有东西（不只是饥荒）。停在这里。" >&2
  echo "确认这是你要的：先 umount，再跑本脚本。" >&2
  exit 1
fi

echo "将要删除："
echo "  systemd 单元 /etc/systemd/system/dstpanel.service"
echo "  运行用户 $DST_USER 及其家目录 $DST_ROOT（引擎、存档、MOD、steamcmd、面板数据库与备份都在里面）"
echo "  老版本可能留下的 /usr/local/bin/dstpanel 与 /etc/dstpanel"
echo
echo "存档删了就没有了。想留一份的话先执行：sudo cp -a $DST_ROOT/panel/backups /root/dst-backups"
echo

if [ "$ASSUME_YES" -ne 1 ]; then
  read -r -p "确认卸载？输入 yes 继续：" reply
  [ "$reply" = "yes" ] || { echo "没输入 yes，什么都不动。"; exit 1; }
fi

# 分片跑在 screen 会话里，属于这个用户。先停：
# 一是进程占着正在被删的目录，二是"世界还在跑、盘已经被清空"是最难解释的现场。
if runuser -u "$DST_USER" -- screen -ls >/dev/null 2>&1; then
  echo "停止该用户下的 screen 会话（游戏分片）……"
  runuser -u "$DST_USER" -- screen -ls | grep -oE '[0-9]+\.[A-Za-z0-9_.-]+' | while read -r sess; do
    runuser -u "$DST_USER" -- screen -S "$sess" -X quit || true
  done
fi

systemctl stop dstpanel.service >/dev/null 2>&1 || true
systemctl disable dstpanel.service >/dev/null 2>&1 || true
rm -f /etc/systemd/system/dstpanel.service
systemctl daemon-reload
rm -rf /etc/dstpanel /usr/local/bin/dstpanel
echo "面板服务与单元已移除。"

# userdel -r 会删家目录；目录若已被人手工挪走，它只报错不报错退出码不一定干净，
# 所以删完再用下面的全盘扫描判定，而不是信这一步的返回值。
if ! userdel -r "$DST_USER" 2>/dev/null; then
  echo "userdel -r 没能一次删干净（常见于目录里有别的用户的数据），剩下的按目录处理。"
  userdel "$DST_USER" 2>/dev/null || true
  [ -e "$DST_ROOT" ] && rm -rf "$DST_ROOT"
fi
echo "用户 $DST_USER 与 $DST_ROOT 已移除。"

# 残余自证：扫全盘（同一文件系统，跳过 /proc /sys /dev /run）。
# 命中的判定按文件名来，不按目录约定来——正是为了抓"当初没按约定放"的那份。
echo
echo "扫描全盘残余（跳挂载点，请稍候）……"
residues=$(find / -xdev \
  -path /proc -prune -o -path /sys -prune -o -path /dev -prune -o -path /run -prune -o \
  \( -name 'dstpanel*' -o -name 'dedicated_server' -o -name 'DoNotStarveTogether' -o \
     -name '.klei' -o -name 'steamcmd*' -o -name 'cluster_token.txt' \) -print 2>/dev/null)

if [ -z "$residues" ]; then
  echo "残余扫描：0 命中。这台机器上已经没有饥荒与面板相关文件。"
else
  echo "残余扫描命中以下文件，请逐个确认后再删（脚本不替你判断哪些是别人的东西）："
  echo "$residues"
  exit 2
fi
