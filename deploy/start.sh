#!/usr/bin/env bash
# DST 面板部署入口。把整个部署包传到服务器上，跑这一个脚本就够。
#
#   sudo ./start.sh            # 交互菜单：安装 / 升级面板 / 卸载
#   sudo ./start.sh install    # 免交互（脚本化部署）
#   sudo ./start.sh upgrade
#   sudo ./start.sh uninstall
#
# 分工：本脚本管"这台机器"（依赖包、专用用户、steamcmd），
# 文件落盘（二进制、配置、systemd 单元）交给同目录的 install.sh，卸载交给 uninstall.sh。
# 之所以包一层而不是把 install.sh 复制一遍：路径布局只允许有一个出处，
# 两份实现迟早对不上，而对不上的症状是面板写的配置引擎不读。
#
# 布局（写在这里，也写在 internal/config/config.go 的默认值里）：
#   $DST_ROOT                      = 专用用户的家目录，整套业务的根
#   $DST_ROOT/tools/steamcmd       = steamcmd
#   $DST_ROOT/dedicated_server     = 饥荒服务端本体 + MOD（app 343050）
#   $DST_ROOT/.klei                = 集群配置与存档（引擎只认这个位置）
#   $DST_ROOT/panel                = 面板二进制、配置、数据库、备份
set -euo pipefail

# 环境变量指定过用户就别再自动探测：重装、迁移、多套并存的场景里，
# 探测出来的值一定指向"已经装着的那一套"，而调用这里的人多半正要指向另一套。
FROM_ENV=0
if [ -n "${DST_USER:-}" ] || [ -n "${DST_ROOT:-}" ]; then FROM_ENV=1; fi
DST_USER="${DST_USER:-dst}"
DST_ROOT="${DST_ROOT:-/data/dst}"
# steamcmd 只是一个会自动自更新的壳（几十 KB），下载它几乎不花钱。
# 真正的大头是饥荒服务端 depot（整包大小随构建变化，面板任务里按 steamcmd 报的字节数显示进度）。
# 这台机器连不上 steam CDN 时，把同名的 steamcmd_linux.tar.gz 放进部署包目录即可离线安装。
STEAMCMD_URL="${STEAMCMD_URL:-https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz}"

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# 部署包里的文件名是固定的；单独 scp 一个二进制来时用 BIN= 指过去。
BIN="${BIN:-$SCRIPT_DIR/dstpanel-linux}"

say() { printf '%s\n' "$*"; }
die() { printf '%s\n' "$*" >&2; exit 1; }

require_root() {
  [ "$(id -u)" -eq 0 ] || die "要用 root 跑：sudo $0 ${1:-}"
}

# verify_package 只回答"这个包是不是完整的"，不回答"能不能跑"。
# 校验文件不存在时不拦路：本地直接 scp 单个二进制的调试场景很常见，
# 那种时候没有 SHA256SUMS 是应当的，报错只会逼人绕过脚本。
verify_package() {
  [ -f "$BIN" ] || die "部署包里没有可执行文件 $BIN"
  chmod 0755 "$BIN"
  if [ -f "$SCRIPT_DIR/VERSION" ]; then
    say "部署包版本：$(cat "$SCRIPT_DIR/VERSION")"
  fi
  if [ -f "$SCRIPT_DIR/SHA256SUMS" ] && command -v sha256sum >/dev/null 2>&1; then
    (cd "$SCRIPT_DIR" && sha256sum -c SHA256SUMS >/dev/null 2>&1) \
      || die "SHA256SUMS 校验没过：包在传输里坏了或者被动过。重新打包再传。"
    say "包校验：通过（sha256）"
  fi
}

# ensure_packages 缺什么补什么。缺 apt 时不猜发行版，直接把要装的写清楚让人执行。
ensure_packages() {
  local need=()
  local p
  for p in curl screen tar; do
    command -v "$p" >/dev/null 2>&1 || need+=("$p")
  done
  [ ${#need[@]} -eq 0 ] && return 0
  if command -v apt-get >/dev/null 2>&1; then
    say "安装依赖：${need[*]}"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq || say "apt-get update 没成功，继续尝试安装"
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${need[@]}" \
      || die "装不上 ${need[*]}；screen 是必需的（面板靠它拉起分片）"
  else
    die "缺少 ${need[*]}，而这台机器上没有 apt-get。请手工装好后重跑（screen 必需）。"
  fi
}

# ensure_user 建专用用户，家目录就是业务根目录。
# 为什么值得为它建一个用户而不是用现成的 ubuntu/root：
# 引擎忽略 -savedir，存档、steamcmd 缓存、MOD 全都按"运行用户的家目录"落盘。
# 让家目录本身当根目录，"数据都在一个地方"和"删掉一个用户就卸干净"才是同一件事。
ensure_user() {
  if id "$DST_USER" >/dev/null 2>&1; then
    local home
    home=$(getent passwd "$DST_USER" | cut -d: -f6)
    if [ "$home" != "$DST_ROOT" ]; then
      say "用户 $DST_USER 已存在，家目录是 $home，改成业务根目录 $DST_ROOT"
      mkdir -p "$DST_ROOT"
      # -m 会把旧家目录整个搬过去；已存在的新目录会合并，不会覆盖同名文件。
      usermod -m -d "$DST_ROOT" "$DST_USER"
    fi
    return 0
  fi
  say "创建用户 $DST_USER（家目录 $DST_ROOT）"
  # 只建父目录：家目录留给 useradd -m 自己建。抢着建好会触发
  # "the home directory already exists / Not copying any file from skel"，
  # 结果是这个用户的家目录里没有 .bashrc 与 .profile，之后 runuser 进去看到的是一副生壳。
  mkdir -p "$(dirname "$DST_ROOT")"
  # -m 建家目录，-s bash 让 runuser/screen 能用；不给密码、不进 sshd 白名单，
  # 这个账号只用于跑服务，日常操作还是 root 或你自己的账号。
  useradd -m -d "$DST_ROOT" -s /bin/bash "$DST_USER" || die "useradd 失败：确认 $DST_ROOT 之前不存在残留属主"
  chown "$DST_USER:$DST_USER" "$DST_ROOT"
  chmod 0755 "$DST_ROOT"
}

# ensure_steamcmd 在目标机上下载 steamcmd 并完成一次自更新。
# 不在本地打好包再传：steamcmd 是自更新的壳，随包带上等于把版本冻结在安装那一刻。
#
# 但"壳的压缩包"和"壳跑起来"不是一回事：解压出来只有几十 KB，第一次运行它要先给
# 自己拉一个两百多兆的更新（云机实测 218 MB 上下，慢的要几分钟）。
# 所以这一步必须提前打招呼、并且限时——不设限的话安装会像卡死在这里，
# 而它其实只是在一句输出都不给地下载自己的新版本。
ensure_steamcmd() {
  local dir="$DST_ROOT/tools/steamcmd"
  if [ -x "$dir/steamcmd.sh" ]; then
    say "steamcmd 已在 $dir，跳过下载"
    return 0
  fi
  mkdir -p "$dir"
  local pkg="$SCRIPT_DIR/steamcmd_linux.tar.gz"
  if [ -f "$pkg" ]; then
    say "用部署包自带的 steamcmd（$pkg）"
  else
    say "下载 steamcmd 到 $dir ……"
    curl -fsSL --retry 3 -o "$dir/steamcmd_linux.tar.gz" "$STEAMCMD_URL" \
      || { rm -f "$dir/steamcmd_linux.tar.gz"; say "steamcmd 下载失败（这台机器大概连不上 steam CDN）。"; say "面板会照常安装；把包里的 steamcmd_linux.tar.gz 放到 $dir 后重跑本脚本即可。"; return 0; }
    pkg="$dir/steamcmd_linux.tar.gz"
  fi
  tar -xzf "$pkg" -C "$dir"
  rm -f "$dir/steamcmd_linux.tar.gz"
  chmod 0755 "$dir/steamcmd.sh"
  chown -R "$DST_USER:$DST_USER" "$DST_ROOT/tools"
  # 首次运行会自更新到最新壳版本。这一步失败不影响文件在不在，
  # 所以只提示，不拦：真正把 steamcmd 用明白的是后面面板里的下载任务。
  local log="$dir/first_run.log" rc=0
  say "首次运行 steamcmd：它会先更新自己（两百多兆，一到几分钟；最多等 10 分钟）"
  # 参数一律走 argv，不拼 shell 字符串：路径里出现空格或引号都不该改变执行的东西。
  # timeout 属于 coreutils，正常发行版都有；真没有就退化成不限时，不比原来差。
  if command -v timeout >/dev/null 2>&1; then
    runuser -u "$DST_USER" -- env HOME="$DST_ROOT" \
      timeout 600 bash "$dir/steamcmd.sh" +quit >"$log" 2>&1 || rc=$?
  else
    runuser -u "$DST_USER" -- env HOME="$DST_ROOT" \
      bash "$dir/steamcmd.sh" +quit >"$log" 2>&1 || rc=$?
  fi
  if [ "$rc" -ne 0 ]; then
    say "steamcmd 首次自更新没跑成（退出码 $rc，网络原因居多；124 是到了 10 分钟上限），最后几行："
    # 用 awk 而不是 sed 加缩进：steamcmd 的进度行以 \r 结尾、最后一行常常没有换行符，
    # sed 原样透传的话，下面那句"不影响安装"会直接接在同一行尾巴上（云机实测过）。
    tail -n 5 "$log" 2>/dev/null | awk '{print "    " $0}'
    say "不影响安装：面板里那条下载任务会自己再更新一次，进度写在任务中心。"
  fi
}

# detect_installed_user 让"重装/升级/卸载"落在已经装着的那一套上。
# 单元的 User 与它的家目录是一体的（install.sh 会拒绝不一致的组合），所以两个值一起取。
detect_installed_user() {
  [ "$FROM_ENV" -eq 1 ] && return 0
  [ "${DETECTED:-}" = 1 ] && return 0
  DETECTED=1
  [ -f /etc/systemd/system/dstpanel.service ] || return 0
  local u home
  u=$(systemctl show -p User --value dstpanel.service 2>/dev/null || true)
  if [ -z "$u" ] || [ "$u" = "nobody" ]; then
    return 0
  fi
  DST_USER="$u"
  home=$(getent passwd "$u" 2>/dev/null | cut -d: -f6 || true)
  if [ -n "$home" ] && [ "$home" != "/" ]; then
    DST_ROOT="$home"
  fi
  say "检测到已装的面板：服务用户 $DST_USER，业务根目录 $DST_ROOT"
}

do_install() {
  require_root install
  verify_package
  detect_installed_user
  ensure_packages
  ensure_user
  ensure_steamcmd
  DST_USER="$DST_USER" DST_ROOT="$DST_ROOT" bash "$SCRIPT_DIR/install.sh" "$BIN"
  ask_public_bind
  say ""
  say "下一步（顺序有讲究）："
  say "  1) 启动面板：sudo systemctl start dstpanel"
  say "  2) 打开面板，登录后在【任务中心】提交「下载/更新服务端本体」——没有本体就没有存档目录，向导也建不出能用令牌验证的房间。"
  say "  3) 用【开服向导】建房间、填 Klei 令牌，再启动分片。"
  say "  4) 防火墙要放的是每个分片的 server_port（UDP），不是面板端口；细节见 README。"
}

do_upgrade() {
  require_root upgrade
  verify_package
  detect_installed_user
  say "只换面板文件（用户 $DST_USER）。引擎本体与 MOD 请走面板里的任务，不要让这个脚本碰。"
  DST_USER="$DST_USER" bash "$SCRIPT_DIR/install.sh" "$BIN"
}

# ask_public_bind 是安装流程里唯一动配置的一步，而且只在明确回答 yes 之后才动。
# 默认 127.0.0.1 不是为了麻烦：默认账号是 admin/admin，公网直开等于把这对凭据挂在扫段脚本面前。
ask_public_bind() {
  local conf="$DST_ROOT/panel/cfg.yaml"
  [ -f "$conf" ] || return 0
  local reply=""
  printf '要让面板直接监听公网端口吗？现在的地址只在本机（127.0.0.1，从别的机器连不上）。输入 yes 才改：'
  read -r reply || return 0
  [ "$reply" = "yes" ] || { say "保持只监听本机。要用浏览器访问：ssh -L 8921:127.0.0.1:8921 <这台机器>，或者之后自行改 bind。"; return 0; }
  local port=""
  printf '监听端口（回车取 8921）：'
  read -r port || port=""
  [ -n "$port" ] || port=8921
  sed -i -e 's/^  bind: .*/  bind: 0.0.0.0/' -e "s/^  port: .*/  port: $port/" "$conf"
  chown "$DST_USER:$DST_USER" "$conf"
  chmod 0600 "$conf"
  say "已改为 0.0.0.0:$port。登录密码还是默认的 admin/admin，第一件事就是进【账号管理】改掉它。"
}

menu() {
  require_root ""
  detect_installed_user
  say "DST 面板部署包。业务根目录 $DST_ROOT（服务用户 $DST_USER 的家目录，引擎、存档、MOD、steamcmd、面板数据都在这棵树里）。"
  if [ -f /etc/systemd/system/dstpanel.service ]; then
    say "面板服务：$(systemctl is-active dstpanel.service 2>/dev/null || echo unknown)（$(systemctl is-enabled dstpanel.service 2>/dev/null || echo unknown)）"
  else
    say "面板服务：这台机器上还没装。"
  fi
  say ""
  say "  1) 安装系统（建用户 + 装 steamcmd + 装面板）"
  say "  2) 升级面板（只换面板文件，不动引擎与存档）"
  say "  3) 卸载（清掉面板、引擎、存档、MOD 与用户，最后全盘扫残余）"
  printf '选一个：'
  local c=""
  read -r c || die "没读到选择，退出。"
  case "$c" in
  1) do_install ;;
  2) do_upgrade ;;
  3) DST_USER="$DST_USER" bash "$SCRIPT_DIR/uninstall.sh" ;;
  *) die "不认识的选择：$c" ;;
  esac
}

case "${1:-}" in
install) do_install ;;
upgrade) do_upgrade ;;
uninstall)
  require_root uninstall
  detect_installed_user
  DST_USER="$DST_USER" bash "$SCRIPT_DIR/uninstall.sh" "${@:2}"
  ;;
"") menu ;;
*) die "用法：$0 [install|upgrade|uninstall]（不带参数进交互菜单）" ;;
esac
