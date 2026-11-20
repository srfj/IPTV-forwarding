#!/bin/bash
# 构建单个 SPK 套件
# 用法: build-spk.sh <arch> <server|client> [version]
set -euo pipefail

ARCH="$1"
KIND="$2"
VERSION="${3:-0.1.0}"

case "$ARCH" in
    x86_64) DSM_ARCH="x86_64" ;;
    armv7)  DSM_ARCH="armv7 armada38x armada370 armada375" ;;
    arm64)  DSM_ARCH="armv8 armada37xx rtd1296" ;;
    *) echo "unsupported arch: $ARCH"; exit 1 ;;
esac

case "$KIND" in
    server)
        PKG="IPTV-ZT-Bridge-Server"
        DISPLAY="IPTV ZT Bridge Server"
        TITLE="IPTV-ZT-Bridge 服务端"
        DESC="IPTV 组网服务端（内置 rtp2httpd + ZeroTier）"
        ;;
    client)
        PKG="IPTV-ZT-Bridge-Client"
        DISPLAY="IPTV ZT Bridge Client"
        TITLE="IPTV-ZT-Bridge 客户端"
        DESC="IPTV 组网客户端（ZeroTier + 本地代理）"
        ;;
    *)
        echo "invalid kind: $KIND"; exit 1 ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD="$ROOT/build/$ARCH"
STAGE="$ROOT/build/spk-stage/${KIND}-${ARCH}"
PAYLOAD="$STAGE/payload"
DIST="$ROOT/dist"

if [ ! -f "$BUILD/iptv-zt-bridge" ]; then
    echo "missing $BUILD/iptv-zt-bridge, run make build-all first"
    exit 1
fi
if [ ! -f "$BUILD/bin/zerotier-one" ]; then
    echo "missing $BUILD/bin/zerotier-one, run make download-zerotier first"
    exit 1
fi
if [ "$KIND" = "server" ] && [ ! -f "$BUILD/bin/rtp2httpd" ]; then
    echo "missing $BUILD/bin/rtp2httpd, run make download-rtp2httpd first"
    exit 1
fi

rm -rf "$STAGE"
mkdir -p "$PAYLOAD/bin" "$PAYLOAD/web" "$STAGE/scripts" "$DIST"

install -m 0755 "$BUILD/iptv-zt-bridge" "$PAYLOAD/bin/iptv-zt-bridge"
install -m 0755 "$BUILD/bin/zerotier-one" "$PAYLOAD/bin/zerotier-one"
echo "$KIND" > "$PAYLOAD/bin/mode"

if [ "$KIND" = "server" ]; then
    install -m 0755 "$BUILD/bin/rtp2httpd" "$PAYLOAD/bin/rtp2httpd"
fi

cp -r "$ROOT/web/static/." "$PAYLOAD/web/"

# DSM 主菜单入口：点击图标打开 WebUI（由 dsmuidir="ui" 注册）
mkdir -p "$PAYLOAD/ui"
cp -r "$ROOT/spk/ui/." "$PAYLOAD/ui/"
cat > "$PAYLOAD/ui/config" <<EOF
{
  ".url": {
    "$PKG": {
      "type": "url",
      "icon": "images/icon_{0}.png",
      "title": "$TITLE",
      "desc": "$DESC",
      "url": "/webman/3rdparty/$PKG/index.html"
    }
  }
}
EOF
chmod 644 "$PAYLOAD/ui/config"

tar -C "$PAYLOAD" -czf "$STAGE/package.tgz" .

for s in preinst postinst preun postun start-stop-status; do
    install -m 0755 "$ROOT/spk/scripts/$s" "$STAGE/scripts/$s"
done

cp "$ROOT/spk/PACKAGE_ICON.PNG" "$STAGE/PACKAGE_ICON.PNG"
cp "$ROOT/spk/PACKAGE_ICON_256.PNG" "$STAGE/PACKAGE_ICON_256.PNG"

TS="$(date +%s)"
sed -e "s|@PKG@|$PKG|g" \
    -e "s|@VERSION@|$VERSION|g" \
    -e "s|@ARCH@|$DSM_ARCH|g" \
    -e "s|@DISPLAY@|$DISPLAY|g" \
    -e "s|@DESC@|$DESC|g" \
    "$ROOT/spk/INFO.tmpl" > "$STAGE/INFO"

sed -e "s|@PKG@|$PKG|g" \
    -e "s|@VERSION@|$VERSION|g" \
    -e "s|@ARCH@|$DSM_ARCH|g" \
    -e "s|@DISPLAY@|$DISPLAY|g" \
    -e "s|@DESC@|$DESC|g" \
    -e "s|@TS@|$TS|g" \
    "$ROOT/spk/PACKAGE.tmpl" > "$STAGE/PACKAGE"

OUT="$DIST/${PKG}_${VERSION}_${ARCH}.spk"
tar -C "$STAGE" -czf "$OUT" INFO PACKAGE package.tgz scripts PACKAGE_ICON.PNG PACKAGE_ICON_256.PNG

# 只保留当前版本：清理同套件同架构的旧包
for old in "$DIST/${PKG}_"*"_${ARCH}.spk"; do
    [ "$old" = "$OUT" ] || rm -f "$old"
done

echo "[spk] $OUT"
