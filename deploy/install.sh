#!/usr/bin/env bash
# 安装或升级 DST 面板的"文件层"：二进制、配置、systemd 单元。
#
#   sudo DST_USER=dst ./deploy/install.sh ./dstpanel-linux
#
# 机器层面的准备（建用户、装 screen、下载 steamcmd）不在这里，在 deploy/start.sh。
# 分成两个脚本是因为它们的失败方式不同：这一份只碰磁盘上的几个文件，可重入、可单独跑；
# 那一份要动用户表和 apt，跑一次就少一次。
#
# 一条硬约束：面板运行用户的 HOME 必须就是业务根目录 DST_ROOT。
# 服务端二进制忽略 -savedir，存档只会落在运行用户的 ~/.klei；steamcmd 的缓存同样按 $HOME 走。
# 所以"所有东西在一个目录里"和"删掉这个用户就卸干净"其实是同一件事，
# 一旦 HOME 和 DST_ROOT 不一致，症状是面板写的配置引擎根本不读（全程无报错）。
set -euo pipefail

BIN="${1:-}"
DST_USER="${DST_USER:-ubuntu}"
DST_GROUP="${DST_GROUP:-$DST_USER}"
CONF_NAME="cfg.yaml"

if [ -z "$BIN" ]; then
  echo "用法：sudo DST_USER=<服务用户> $0 <dstpanel 二进制路径>" >&2
  echo "     （机器准备请用 deploy/start.sh）" >&2
  exit 1
fi
if [ ! -f "$BIN" ]; then
  echo "找不到二进制 $BIN；先构建：GOOS=linux GOARCH=amd64 go build -o dstpanel-linux ./cmd/dstpanel" >&2
  exit 1
fi
if [ "$(id -u)" -ne 0 ]; then
  echo "要用 root 跑：sudo DST_USER=$DST_USER $0 $BIN" >&2
  exit 1
fi
if ! id "$DST_USER" >/dev/null 2>&1; then
  echo "没有用户 $DST_USER。新建一个专用用户（业务根目录就是它的家目录）：" >&2
  echo "  sudo useradd -m -d /data/dst -s /bin/bash $DST_USER" >&2
  exit 1
fi
if ! getent group "$DST_GROUP" >/dev/null 2>&1; then
  echo "没有用户组 $DST_GROUP；用 DST_GROUP=别的组 再跑一次" >&2
  exit 1
fi

# DST_ROOT 默认取这个用户自己的家目录：这是上面那条硬约束，不是可选项。
USER_HOME=$(getent passwd "$DST_USER" | cut -d: -f6)
DST_ROOT="${DST_ROOT:-$USER_HOME}"
if [ "$DST_ROOT" != "$USER_HOME" ]; then
  echo "DST_ROOT=$DST_ROOT 和用户 $DST_USER 的家目录 $USER_HOME 不一致，面板停在这里。" >&2
  echo "引擎只认运行用户的 ~/.klei，配置和 steamcmd 缓存也都按家目录落盘；" >&2
  echo "要么把家目录改成业务目录（sudo usermod -d $DST_ROOT $DST_USER），要么别传 DST_ROOT。" >&2
  exit 1
fi

PANEL_DIR="$DST_ROOT/panel"
DST_BIN="$PANEL_DIR/dstpanel"
DST_CONF="$PANEL_DIR/$CONF_NAME"
UNIT=/etc/systemd/system/dstpanel.service

if ! command -v screen >/dev/null 2>&1; then
  echo "警告：PATH 里没有 screen。面板靠它拉起分片：sudo apt install -y screen" >&2
fi

install -d -o "$DST_USER" -g "$DST_GROUP" -m 0755 "$PANEL_DIR"
# 二进制不能直接覆盖正在跑的文件（Linux 会返回 ETXTBSY，升级时机不巧就把脚本打断在半路），
# 先落成临时文件再改名——同目录内的 rename 是原子的，旧的照常在内存里跑完这一版。
install -o "$DST_USER" -g "$DST_GROUP" -m 0755 "$BIN" "$DST_BIN.tmp"
mv -f "$DST_BIN.tmp" "$DST_BIN"

# 配置由面板自己生成，而且要在那个用户的环境里生成：示例配置里的四条路径是从
# "当前用户的 HOME"推出来的，用 root 跑就会写出一套 /root/ 开头、服务用户读不到的路径。
if [ ! -f "$DST_CONF" ]; then
  runuser -u "$DST_USER" -- env HOME="$DST_ROOT" "$DST_BIN" -gen-config "$DST_CONF"
  echo "已生成配置 $DST_CONF（路径按 $DST_ROOT 展开）。启动前确认 server.bind / adminPassword。"
else
  echo "保留现有配置 $DST_CONF（本脚本不覆盖它）。"
fi
# 配置里有 adminPassword，只给服务用户自己读。
chown "$DST_USER:$DST_GROUP" "$DST_CONF"
chmod 0600 "$DST_CONF"

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
sed -e "s|__DST_USER__|$DST_USER|" \
    -e "s|__DST_GROUP__|$DST_GROUP|" \
    -e "s|__DST_ROOT__|$DST_ROOT|" \
    -e "s|__DST_BIN__|$DST_BIN|" \
    -e "s|__DST_CONF__|$DST_CONF|" \
    "$SCRIPT_DIR/dstpanel.service" > "$UNIT"
chmod 0644 "$UNIT"

systemctl daemon-reload
systemctl enable dstpanel.service >/dev/null
if systemctl is-active --quiet dstpanel.service; then
  systemctl restart dstpanel.service
  echo "面板已升级并重启：systemctl status dstpanel"
else
  echo "已安装，尚未启动。改完 $DST_CONF 后："
  echo "  sudo systemctl start dstpanel"
  echo "  journalctl -u dstpanel -f"
fi
echo
echo "业务根目录 $DST_ROOT：引擎 $DST_ROOT/dedicated_server，存档 $DST_ROOT/.klei，"
echo "steamcmd $DST_ROOT/tools/steamcmd，面板与备份 $DST_ROOT/panel。"
echo "要卸掉整套：sudo ./deploy/uninstall.sh"
