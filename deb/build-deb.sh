#!/bin/bash
# 构建飞牛 fnOS / Debian 系安装包（.deb）
# 用法: build-deb.sh <arch> <server|client> [version]
set -euo pipefail

ARCH="$1"
KIND="$2"
VERSION="${3:-0.1.0}"

case "$ARCH" in
    x86_64) DEB_ARCH="amd64" ;;
    arm64)  DEB_ARCH="arm64" ;;
    armv7)  DEB_ARCH="armhf" ;;
    *) echo "unsupported arch: $ARCH"; exit 1 ;;
esac

case "$KIND" in
    server)
        PKG="iptv-zt-bridge-server"
        UNIT="iptv-zt-bridge-server"
        DISPLAY="IPTV-ZT-Bridge Server"
        DESC="IPTV 组网服务端（内置 rtp2httpd + ZeroTier）"
        ;;
    client)
        PKG="iptv-zt-bridge-client"
        UNIT="iptv-zt-bridge-client"
        DISPLAY="IPTV-ZT-Bridge Client"
        DESC="IPTV 组网客户端（ZeroTier + 本地代理）"
        ;;
    *)
        echo "invalid kind: $KIND"; exit 1 ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD="$ROOT/build/$ARCH"
STAGE="$ROOT/build/deb-stage/${KIND}-${ARCH}"
DIST="$ROOT/dist"

if [ ! -f "$BUILD/iptv-zt-bridge" ]; then
    echo "missing $BUILD/iptv-zt-bridge, run make build-$ARCH first"
    exit 1
fi
if [ ! -f "$BUILD/bin/zerotier-one" ]; then
    echo "missing $BUILD/bin/zerotier-one, run make download-zerotier-$ARCH first"
    exit 1
fi
if [ "$KIND" = "server" ] && [ ! -f "$BUILD/bin/rtp2httpd" ]; then
    echo "missing $BUILD/bin/rtp2httpd, run make download-rtp2httpd-$ARCH first"
    exit 1
fi

rm -rf "$STAGE"
mkdir -p "$STAGE/DEBIAN" "$STAGE/opt/iptv-zt-bridge/bin" \
         "$STAGE/opt/iptv-zt-bridge/web" "$STAGE/lib/systemd/system" "$DIST"

install -m 0755 "$BUILD/iptv-zt-bridge" "$STAGE/opt/iptv-zt-bridge/bin/iptv-zt-bridge"
install -m 0755 "$BUILD/bin/zerotier-one" "$STAGE/opt/iptv-zt-bridge/bin/zerotier-one"
if [ "$KIND" = "server" ]; then
    install -m 0755 "$BUILD/bin/rtp2httpd" "$STAGE/opt/iptv-zt-bridge/bin/rtp2httpd"
fi

cp -r "$ROOT/web/static/." "$STAGE/opt/iptv-zt-bridge/web/"

cat > "$STAGE/DEBIAN/control" <<EOF
Package: $PKG
Version: $VERSION
Section: net
Priority: optional
Architecture: $DEB_ARCH
Conflicts: $( [ "$KIND" = "server" ] && echo iptv-zt-bridge-client || echo iptv-zt-bridge-server )
Maintainer: IPTV-ZT-Bridge <noreply@localhost>
Description: $DISPLAY
 $DESC
EOF

cat > "$STAGE/lib/systemd/system/$UNIT.service" <<EOF
[Unit]
Description=$DISPLAY
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/iptv-zt-bridge/bin/iptv-zt-bridge -mode $KIND -config /etc/iptv-zt-bridge/config.yaml -data-dir /var/lib/iptv-zt-bridge -web-root /opt/iptv-zt-bridge/web
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

cat > "$STAGE/DEBIAN/postinst" <<EOF
#!/bin/sh
set -e

mkdir -p /etc/iptv-zt-bridge
mkdir -p /var/lib/iptv-zt-bridge/zerotier /var/lib/iptv-zt-bridge/rtp2httpd \\
         /var/lib/iptv-zt-bridge/uploads /var/lib/iptv-zt-bridge/log

modprobe tun 2>/dev/null || true
if [ ! -e /dev/net/tun ]; then
    mkdir -p /dev/net
    mknod /dev/net/tun c 10 200 2>/dev/null || true
    chmod 600 /dev/net/tun 2>/dev/null || true
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl enable $UNIT.service >/dev/null 2>&1 || true
    systemctl restart $UNIT.service || true
fi

exit 0
EOF

cat > "$STAGE/DEBIAN/prerm" <<EOF
#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop $UNIT.service || true
    if [ "\$1" = "remove" ] || [ "\$1" = "purge" ]; then
        systemctl disable $UNIT.service || true
    fi
fi

exit 0
EOF

cat > "$STAGE/DEBIAN/postrm" <<EOF
#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

if [ "\$1" = "purge" ]; then
    rm -rf /etc/iptv-zt-bridge /var/lib/iptv-zt-bridge
fi

exit 0
EOF

chmod 0755 "$STAGE/DEBIAN/postinst" "$STAGE/DEBIAN/prerm" "$STAGE/DEBIAN/postrm"
find "$STAGE" -type d -exec chmod 0755 {} +
find "$STAGE/opt/iptv-zt-bridge/web" -type f -exec chmod 0644 {} +

OUT="$DIST/${PKG}_${VERSION}_${DEB_ARCH}.deb"
dpkg-deb --build --root-owner-group "$STAGE" "$OUT" >/dev/null

# 只保留当前版本：清理同套件同架构的旧包
for old in "$DIST/${PKG}_"*"_${DEB_ARCH}.deb"; do
    [ "$old" = "$OUT" ] || rm -f "$old"
done

echo "[deb] $OUT"
