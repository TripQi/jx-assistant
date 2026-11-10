#!/bin/sh
# jx-assistant Linux 安装/升级脚本（POSIX sh，适配 dash）
#
# 用法：
#   sh install.sh                       # 从更新源下载最新版安装（需服务端已发布 Linux 附件）
#   sh install.sh --file <tar.gz>       # 离线安装本地包
#   sh install.sh --base https://example.com:2227   # 指定更新源
#   sh install.sh --dest /opt/jx-assistant         # 指定安装目录
#   sh install.sh --no-service          # 只放文件，不注册 systemd
#   sh install.sh --insecure            # 下载源为自签证书时（curl -k）
#
# 安装目录只覆盖 jx-assistant / README-Linux.md / systemd unit；
# config.json、data/、技能/、工作区 永不触碰——重复执行即为原地升级。
set -eu

BASE="https://example.com:2227"
DEST="/opt/jx-assistant"
PKG_FILE=""
NO_SERVICE=0
INSECURE=""
SUDO=""

usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --base)      BASE="${2?}"; shift 2 ;;
    --dest)      DEST="${2?}"; shift 2 ;;
    --file)      PKG_FILE="${2?}"; shift 2 ;;
    --no-service) NO_SERVICE=1; shift ;;
    --insecure)  INSECURE="-k"; shift ;;
    -h|--help)   usage ;;
    *) echo "未知参数: $1（--help 看用法）" >&2; exit 1 ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少依赖: $1" >&2; exit 1; }; }
need curl; need tar

# root 权限（写 /opt 与 /etc/systemd/system）
if [ "$(id -u)" != "0" ]; then
  if command -v sudo >/dev/null 2>&1; then SUDO="sudo"
  else echo "请用 root 运行（或安装 sudo）" >&2; exit 1; fi
fi

HAVE_SYSTEMD=0
if [ "$NO_SERVICE" = "0" ] && command -v systemctl >/dev/null 2>&1 \
   && [ "$(ps -p 1 -o comm= 2>/dev/null)" = "systemd" ]; then
  HAVE_SYSTEMD=1
fi

TMPDIR_INST="$(mktemp -d)"
cleanup() { rm -rf "$TMPDIR_INST"; }
trap cleanup EXIT INT TERM

PKG_NAME="jx-assistant-linux-amd64.tar.gz"

# ---- 取包：本地文件或更新源 ----
if [ -n "$PKG_FILE" ]; then
  case "$PKG_FILE" in /*) ;; *) PKG_FILE="$PWD/$PKG_FILE" ;; esac
  [ -f "$PKG_FILE" ] || { echo "找不到包文件: $PKG_FILE" >&2; exit 1; }
  cp "$PKG_FILE" "$TMPDIR_INST/$PKG_NAME"
  echo "== 使用本地包: $(basename "$PKG_FILE")"
else
  echo "== 查询最新版本: $BASE/updates/latest"
  # shellcheck disable=SC2086
  if ! curl -fsSL $INSECURE --connect-timeout 15 --retry 2 \
       -o "$TMPDIR_INST/latest.json" "$BASE/updates/latest"; then
    echo "拉取更新源失败（源不可达或尚未发布任何版本）。" >&2
    echo "可离线安装: sh install.sh --file $PKG_NAME" >&2
    exit 1
  fi
  URL="$(grep -o '"browser_download_url":"[^"]*"' "$TMPDIR_INST/latest.json" \
        | cut -d'"' -f4 | grep "/$PKG_NAME\$" | head -n 1 || true)"
  if [ -z "$URL" ]; then
    TAG="$(grep -o '"tag_name":"[^"]*"' "$TMPDIR_INST/latest.json" | cut -d'"' -f4)"
    echo "最新版本 ${TAG:-?} 的附件里没有 $PKG_NAME。" >&2
    echo "发版时需把 Linux 包一并上传（见仓库 AGENT.md 发版章程），或离线安装:" >&2
    echo "  sh install.sh --file $PKG_NAME" >&2
    exit 1
  fi
  echo "== 下载: $URL"
  # shellcheck disable=SC2086
  curl -fL $INSECURE --connect-timeout 15 --retry 3 -o "$TMPDIR_INST/$PKG_NAME" "$URL"
fi

# ---- 解压 ----
EXDIR="$TMPDIR_INST/ex"
mkdir -p "$EXDIR"
tar -xzf "$TMPDIR_INST/$PKG_NAME" -C "$EXDIR"
BIN_SRC="$EXDIR/jx-assistant-linux-amd64"
[ -f "$BIN_SRC" ] || { echo "包内缺 jx-assistant-linux-amd64（包结构不符）" >&2; exit 1; }
UNIT_SRC="$EXDIR/jx-assistant.service"
README_SRC="$EXDIR/README-Linux.md"

# ---- 升级时先停服务（换二进制后再拉起） ----
WAS_RUNNING=0
if [ "$HAVE_SYSTEMD" = "1" ] && systemctl is-active --quiet jx-assistant 2>/dev/null; then
  WAS_RUNNING=1
  echo "== 停止运行中的 jx-assistant（升级）"
  $SUDO systemctl stop jx-assistant
fi

# ---- 落位：只覆盖程序/README/unit，数据一概不动 ----
echo "== 安装到 $DEST"
$SUDO mkdir -p "$DEST"
$SUDO install -m 0755 "$BIN_SRC" "$DEST/jx-assistant"
[ -f "$README_SRC" ] && $SUDO install -m 0644 "$README_SRC" "$DEST/README-Linux.md"

# ---- systemd ----
if [ "$HAVE_SYSTEMD" = "1" ]; then
  UNIT_DST="/etc/systemd/system/jx-assistant.service"
  if [ -f "$UNIT_SRC" ]; then
    if [ "$DEST" != "/opt/jx-assistant" ]; then
      sed "s|/opt/jx-assistant|$DEST|g" "$UNIT_SRC" > "$TMPDIR_INST/unit"
    else
      cp "$UNIT_SRC" "$TMPDIR_INST/unit"
    fi
    $SUDO install -m 0644 "$TMPDIR_INST/unit" "$UNIT_DST"
  fi
  echo "== 注册并启动 systemd 服务"
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable jx-assistant >/dev/null 2>&1 || true
  $SUDO systemctl start jx-assistant
  sleep 1
  if systemctl is-active --quiet jx-assistant; then
    echo ""
    echo "安装完成，服务运行中。"
  else
    echo "服务启动异常，查看日志: journalctl -u jx-assistant -n 50" >&2
    $SUDO journalctl -u jx-assistant -n 20 --no-pager || true
    exit 1
  fi
else
  echo ""
  echo "== 文件已就位（本机无 systemd 或 --no-service，手动启动）："
  echo "   cd $DEST && ./jx-assistant -noopen"
fi

echo ""
echo "首次启动会生成 $DEST/config.json（默认监听 127.0.0.1:8788）。"
echo "其他机器访问：bindHost 改 0.0.0.0 后 systemctl restart jx-assistant（注意防火墙）。"
echo "离线体验：config.json 的 baseURL 填 mock，无需 API Key。"
