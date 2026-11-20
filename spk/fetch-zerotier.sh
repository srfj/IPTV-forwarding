#!/bin/bash
# 下载并解包 ZeroTier 1.14.2 官方 deb（debian/jessie 构建为静态链接，
# 不依赖宿主机 glibc，DSM 6/7 与 fnOS 均可直接运行）
# 用法: fetch-zerotier.sh <x86_64|armv7|arm64>
set -euo pipefail

ARCH="$1"
ZT_VERSION="${ZT_VERSION:-1.14.2}"

case "$ARCH" in
    x86_64) DEB_ARCH="amd64" ;;
    armv7)  DEB_ARCH="armhf" ;;
    arm64)  DEB_ARCH="arm64" ;;
    *) echo "unsupported arch: $ARCH"; exit 1 ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/build/$ARCH/bin"
URL="https://download.zerotier.com/RELEASES/${ZT_VERSION}/dist/debian/jessie/zerotier-one_${ZT_VERSION}_${DEB_ARCH}.deb"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "[zt] downloading $URL"
curl -fsSL -o "$TMP/zt.deb" "$URL"

if command -v dpkg-deb >/dev/null 2>&1; then
    dpkg-deb -x "$TMP/zt.deb" "$TMP/root"
else
    (cd "$TMP" && ar x zt.deb)
    DATA="$(ls "$TMP"/data.tar.* | head -1)"
    mkdir -p "$TMP/root"
    tar -xf "$DATA" -C "$TMP/root"
fi

if [ ! -f "$TMP/root/usr/sbin/zerotier-one" ]; then
    echo "[zt] zerotier-one not found in deb"
    exit 1
fi

mkdir -p "$DEST"
install -m 0755 "$TMP/root/usr/sbin/zerotier-one" "$DEST/zerotier-one"
echo "[zt] installed $DEST/zerotier-one (v$ZT_VERSION, $DEB_ARCH)"
