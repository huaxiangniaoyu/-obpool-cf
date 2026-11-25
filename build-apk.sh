#!/bin/sh
# 从源码构建 obpool 的 apk 安装包 (apk-tools 3.x, ImmortalWrt/OpenWrt 25.x)
# 需要: Go (交叉编译) + apk-tools 的 apk.static (Alpine apk-tools-static 包里有)
# 用法: ./build-apk.sh [目标架构, 默认 aarch64_cortex-a53]
# 产物: obpool-1.0.6-r0.apk
set -e
cd "$(dirname "$0")"
ARCH="${1:-aarch64_cortex-a53}"

command -v apk.static >/dev/null 2>&1 || {
    echo "缺少 apk.static: 请从 Alpine 的 apk-tools-static 包解出, 放进 PATH" >&2
    exit 1
}

echo "编译 Go 二进制 (arm64)..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o obpool-bin .

ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT
mkdir -p "$ROOT/opt/open-box/pool" "$ROOT/etc/init.d"
cp obpool-bin "$ROOT/opt/open-box/pool/obpool"
cp run.sh "$ROOT/opt/open-box/pool/run.sh"
sed -i 's#\./obpool-arm64#./obpool#' "$ROOT/opt/open-box/pool/run.sh"
# 每轮跑完把池子同步到订阅服务目录 (只放池子文件, 不放 subs.txt)
cat >> "$ROOT/opt/open-box/pool/run.sh" <<'EOF'
mkdir -p /opt/open-box/pool/www
cp -f pool.yaml pool-proxies.yaml /opt/open-box/pool/www/ 2>/dev/null; chmod 644 /opt/open-box/pool/www/* 2>/dev/null
EOF
cp subs.txt.example "$ROOT/opt/open-box/pool/subs.txt.example"
chmod 755 "$ROOT/opt/open-box/pool/obpool" "$ROOT/opt/open-box/pool/run.sh"
# 订阅服务: 独立 uhttpd 实例把 www 目录挂在 8088 端口, 供 OpenClash/mihomo 拉取
cat > "$ROOT/etc/init.d/poolsub" <<'EOF'
#!/bin/sh /etc/rc.common
START=99
USE_PROCD=1
start_service() {
    mkdir -p /opt/open-box/pool/www
    procd_open_instance
    procd_set_param command /usr/sbin/uhttpd -f -p 8088 -h /opt/open-box/pool/www
    procd_set_param respawn
    procd_close_instance
}
EOF
chmod 755 "$ROOT/etc/init.d/poolsub"
chmod 755 "$ROOT" "$ROOT/opt" "$ROOT/opt/open-box" "$ROOT/opt/open-box/pool"
rm -f obpool-bin

SCRIPTS=$(mktemp -d)
cat > "$SCRIPTS/post-install" <<'EOF'
#!/bin/sh
POOL=/opt/open-box/pool
chmod 755 $POOL/obpool $POOL/run.sh
[ -f $POOL/subs.txt ] || cp $POOL/subs.txt.example $POOL/subs.txt
touch /etc/crontabs/root
grep -q 'open-box/pool/run.sh' /etc/crontabs/root || echo '40 20 * * * /opt/open-box/pool/run.sh' >> /etc/crontabs/root
/etc/init.d/cron enable >/dev/null 2>&1
/etc/init.d/cron restart >/dev/null 2>&1
mkdir -p $POOL/www
cp -f $POOL/pool.yaml $POOL/pool-proxies.yaml $POOL/www/ 2>/dev/null; chmod 644 $POOL/www/* 2>/dev/null
/etc/init.d/poolsub enable >/dev/null 2>&1
/etc/init.d/poolsub restart >/dev/null 2>&1
echo "obpool installed. Put subscriptions into $POOL/subs.txt, first run: $POOL/run.sh"
echo "Pool subscription: http://<router-ip>:8088/pool.yaml"
exit 0
EOF
cat > "$SCRIPTS/pre-deinstall" <<'EOF'
#!/bin/sh
sed -i '/open-box\/pool\/run.sh/d' /etc/crontabs/root 2>/dev/null
/etc/init.d/cron restart >/dev/null 2>&1
/etc/init.d/poolsub stop >/dev/null 2>&1
/etc/init.d/poolsub disable >/dev/null 2>&1
exit 0
EOF
chmod 755 "$SCRIPTS/post-install" "$SCRIPTS/pre-deinstall"
# apk 升级时只跑 post-upgrade 不跑 post-install — 同一份脚本打两份
cp "$SCRIPTS/post-install" "$SCRIPTS/post-upgrade"
chmod 755 "$SCRIPTS/post-upgrade"

apk.static mkpkg \
    --info "name:obpool" \
    --info "version:1.0.6-r0" \
    --info "arch:$ARCH" \
    --info "description:Cloudflare Worker node pool optimizer: aggregates subscriptions, crosses bestcf fronts, latency and bandwidth tests on-router, writes a weighted mihomo YAML pool nightly" \
    --info "license:MIT" \
    --info "maintainer:huaxiangniaoyu" \
    --script "post-install:$SCRIPTS/post-install" \
    --script "post-upgrade:$SCRIPTS/post-upgrade" \
    --script "pre-deinstall:$SCRIPTS/pre-deinstall" \
    --file "$ROOT" \
    --output "obpool-1.0.6-r0.apk"
rm -rf "$SCRIPTS"
echo "完成: obpool-1.0.6-r0.apk (arch=$ARCH)"
echo "安装: 传到路由器后 apk add --allow-untrusted obpool-1.0.6-r0.apk"
