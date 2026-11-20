#!/bin/bash
# 构建飞牛 fnOS 应用包（.fpk）
# 用法: build-fpk.sh <arch> <server|client> [version]
# 依赖: fnpack（自动下载到 build/tools/fnpack）
set -euo pipefail

ARCH="$1"
KIND="$2"
VERSION="${3:-0.1.0}"
ZT_VER="${ZT_VERSION:-1.14.2}"

case "$ARCH" in
    arm64) FPK_PLATFORM="arm" ;;
    x86_64) FPK_PLATFORM="x86" ;;
    *) echo "unsupported arch: $ARCH"; exit 1 ;;
esac

case "$KIND" in
    server)
        APP="iptv-zt-bridge-server"
        DISPLAY="IPTV-ZT-Bridge 服务端"
        DESC="IPTV 组网服务端（内置 rtp2httpd + ZeroTier）"
        ;;
    client)
        APP="iptv-zt-bridge-client"
        DISPLAY="IPTV-ZT-Bridge 客户端"
        DESC="IPTV 组网客户端（ZeroTier + 本地代理）"
        ;;
    *) echo "invalid kind: $KIND"; exit 1 ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD="$ROOT/build/$ARCH"
STAGE="$ROOT/build/fpk-stage/${KIND}-${ARCH}"
PROJ="$STAGE/$APP"
DIST="$ROOT/dist"
TOOLS="$ROOT/build/tools"
FNPACK="${FNPACK_BIN:-$TOOLS/fnpack}"

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

# fnpack 打包工具
if [ ! -x "$FNPACK" ]; then
    mkdir -p "$TOOLS"
    HOST_ARCH="$(uname -m)"
    case "$HOST_ARCH" in
        x86_64) FN_ASSET="linux-amd64" ;;
        aarch64|arm64) FN_ASSET="linux-arm64" ;;
        *) echo "unsupported host arch: $HOST_ARCH"; exit 1 ;;
    esac
    echo "[fpk] downloading fnpack..."
    curl -fsSL -o "$FNPACK" "https://static2.fnnas.com/fnpack/fnpack-1.2.3-${FN_ASSET}"
    chmod +x "$FNPACK"
fi

rm -rf "$STAGE"
mkdir -p "$PROJ/app/bin" "$PROJ/app/web" "$PROJ/app/ui/images" \
         "$PROJ/cmd" "$PROJ/config" "$PROJ/wizard" "$DIST"

# 运行文件
install -m 0755 "$BUILD/iptv-zt-bridge" "$PROJ/app/bin/iptv-zt-bridge"
install -m 0755 "$BUILD/bin/zerotier-one" "$PROJ/app/bin/zerotier-one"
if [ "$KIND" = "server" ]; then
    install -m 0755 "$BUILD/bin/rtp2httpd" "$PROJ/app/bin/rtp2httpd"
fi
cp -r "$ROOT/web/static/." "$PROJ/app/web/"

# 图标
cp "$ROOT/spk/PACKAGE_ICON.PNG" "$PROJ/app/ui/images/icon_64.png"
cp "$ROOT/spk/PACKAGE_ICON_256.PNG" "$PROJ/app/ui/images/icon_256.png"
cp "$ROOT/spk/PACKAGE_ICON.PNG" "$PROJ/ICON.PNG"
cp "$ROOT/spk/PACKAGE_ICON_256.PNG" "$PROJ/ICON_256.PNG"

# manifest
cat > "$PROJ/manifest" <<EOF
appname               = $APP
version               = $VERSION
display_name          = $DISPLAY
desc                  = $DESC
platform              = $FPK_PLATFORM
source                = thirdparty
maintainer            = IPTV-ZT-Bridge
distributor           = IPTV-ZT-Bridge
os_min_version        = 0.9.0
ctl_stop              = true
service_port          = 8089
checkport             = true
desktop_uidir         = ui
desktop_applaunchname = $APP.main
changelog             = 支持飞牛 fnOS，内置 ZeroTier $ZT_VER
EOF

# 权限：ZeroTier 需要创建 tun 设备与路由，必须以 root 运行
cat > "$PROJ/config/privilege" <<'EOF'
{
    "defaults":
    {
        "run-as": "root"
    }
}
EOF

cat > "$PROJ/config/resource" <<'EOF'
{
}
EOF

# 桌面入口：点击图标打开 WebUI
cat > "$PROJ/app/ui/config" <<EOF
{
    ".url": {
        "$APP.main": {
            "title": "$DISPLAY",
            "icon": "images/icon_{0}.png",
            "type": "url",
            "protocol": "http",
            "port": "8089",
            "url": "/",
            "allUsers": true
        }
    }
}
EOF

# 生命周期脚本
for s in install_init install_callback upgrade_init upgrade_callback \
         uninstall_init uninstall_callback config_init config_callback; do
    cat > "$PROJ/cmd/$s" <<'EOF'
#!/bin/bash
exit 0
EOF
done

cat > "$PROJ/cmd/install_callback" <<EOF
#!/bin/bash
# 安装后：准备数据目录、tun 设备
mkdir -p "\$TRIM_PKGVAR/zerotier" "\$TRIM_PKGVAR/rtp2httpd" "\$TRIM_PKGVAR/uploads" "\$TRIM_PKGVAR/log"
mkdir -p "\$TRIM_PKGETC"

modprobe tun 2>/dev/null || true
if [ ! -e /dev/net/tun ]; then
    mkdir -p /dev/net
    mknod /dev/net/tun c 10 200 2>/dev/null || true
    chmod 600 /dev/net/tun 2>/dev/null || true
fi
exit 0
EOF

cat > "$PROJ/cmd/uninstall_init" <<'EOF'
#!/bin/bash
if [ -x "/var/apps/${TRIM_APPNAME}/cmd/main" ]; then
    "/var/apps/${TRIM_APPNAME}/cmd/main" stop || true
fi
exit 0
EOF

# 卸载后清除配置与数据，避免重装后仍带着旧配置。
# 升级走 upgrade_callback，不会执行这里，所以升级不会丢配置。
cat > "$PROJ/cmd/uninstall_callback" <<'EOF'
#!/bin/bash
rm -f "$TRIM_PKGETC/config.yaml"
rm -rf "$TRIM_PKGVAR"
mkdir -p "$TRIM_PKGVAR"
exit 0
EOF

cat > "$PROJ/cmd/main" <<EOF
#!/bin/bash

LOG_FILE="\${TRIM_PKGVAR}/info.log"
PID_FILE="\${TRIM_PKGVAR}/app.pid"

CMD="\${TRIM_APPDEST}/bin/iptv-zt-bridge -mode $KIND -config \${TRIM_PKGETC}/config.yaml -data-dir \${TRIM_PKGVAR} -web-root \${TRIM_APPDEST}/web"

log_msg() {
    echo "\$(date '+%Y-%m-%d %H:%M:%S') - \$1" >> "\${LOG_FILE}"
}

check_process() {
    kill -0 "\$1" 2>/dev/null
}

status() {
    if [ -f "\${PID_FILE}" ]; then
        pid=\$(head -n 1 "\${PID_FILE}" | tr -d '[:space:]')
        if check_process "\${pid}"; then
            return 0
        fi
        rm -f "\${PID_FILE}"
    fi
    return 1
}

start_process() {
    if status; then
        return 0
    fi

    modprobe tun 2>/dev/null || true
    if [ ! -e /dev/net/tun ]; then
        mkdir -p /dev/net
        mknod /dev/net/tun c 10 200 2>/dev/null || true
        chmod 600 /dev/net/tun 2>/dev/null || true
    fi

    log_msg "Starting \$TRIM_APPNAME \$TRIM_APPVER ..."
    bash -c "\${CMD}" >> "\${LOG_FILE}" 2>&1 &
    printf "%s" "\$!" > "\${PID_FILE}"
    return 0
}

stop_process() {
    log_msg "Stopping process ..."
    if [ -r "\${PID_FILE}" ]; then
        pid=\$(head -n 1 "\${PID_FILE}" | tr -d '[:space:]')
        if check_process "\${pid}"; then
            kill -TERM "\${pid}" >> "\${LOG_FILE}" 2>&1
            count=0
            while check_process "\${pid}" && [ \${count} -lt 10 ]; do
                sleep 1
                count=\$((count + 1))
            done
            if check_process "\${pid}"; then
                kill -KILL "\${pid}" 2>/dev/null || true
            fi
        fi
        rm -f "\${PID_FILE}"
    fi
    return 0
}

case "\$1" in
start)  start_process ;;
stop)   stop_process ;;
status) if status; then exit 0; else exit 3; fi ;;
*)      exit 1 ;;
esac
EOF

chmod 0755 "$PROJ"/cmd/*

# 打包
OUT_NAME="${APP}_${VERSION}_${ARCH}.fpk"
(cd "$PROJ" && "$FNPACK" build --directory . >/dev/null 2>&1) || {
    (cd "$PROJ" && "$FNPACK" build --directory .)
    exit 1
}

BUILT="$(ls -1 "$PROJ"/*.fpk 2>/dev/null | head -n 1 || true)"
if [ -z "$BUILT" ]; then
    echo "fnpack build failed: no .fpk produced"
    exit 1
fi

mv -f "$BUILT" "$DIST/$OUT_NAME"

for old in "$DIST/${APP}_"*"_${ARCH}.fpk"; do
    [ "$old" = "$DIST/$OUT_NAME" ] || rm -f "$old"
done

echo "[fpk] $DIST/$OUT_NAME"
