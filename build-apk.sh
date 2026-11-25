#!/bin/sh
# 从源码构建 obpool 的 apk 安装包 (apk-tools 3.x, ImmortalWrt/OpenWrt 25.x)
# 需要: Go (交叉编译) + apk-tools 的 apk.static (Alpine apk-tools-static 包里有)
# 用法: ./build-apk.sh [目标架构, 默认 aarch64_cortex-a53]
# 产物: obpool-1.0.2-r0.apk
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
mkdir -p "$ROOT/opt/open-box/pool"
cp obpool-bin "$ROOT/opt/open-box/pool/obpool"
cp run.sh "$ROOT/opt/open-box/pool/run.sh"
sed -i 's#\./obpool-arm64#./obpool#' "$ROOT/opt/open-box/pool/run.sh"
cp subs.txt.example "$ROOT/opt/open-box/pool/subs.txt.example"
chmod 755 "$ROOT/opt/open-box/pool/obpool" "$ROOT/opt/open-box/pool/run.sh"
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
echo "obpool installed. Put subscriptions into $POOL/subs.txt, first run: $POOL/run.sh"
exit 0
EOF
cat > "$SCRIPTS/pre-deinstall" <<'EOF'
#!/bin/sh
sed -i '/open-box\/pool\/run.sh/d' /etc/crontabs/root 2>/dev/null
/etc/init.d/cron restart >/dev/null 2>&1
exit 0
EOF
chmod 755 "$SCRIPTS/post-install" "$SCRIPTS/pre-deinstall"

apk.static mkpkg \
    --info "name:obpool" \
    --info "version:1.0.2-r0" \
    --info "arch:$ARCH" \
    --info "description:Cloudflare Worker node pool optimizer: aggregates subscriptions, crosses bestcf fronts, latency and bandwidth tests on-router, writes a weighted mihomo YAML pool nightly" \
    --info "license:MIT" \
    --info "maintainer:huaxiangniaoyu" \
    --script "post-install:$SCRIPTS/post-install" \
    --script "pre-deinstall:$SCRIPTS/pre-deinstall" \
    --file "$ROOT" \
    --output "obpool-1.0.2-r0.apk"
rm -rf "$SCRIPTS"
echo "完成: obpool-1.0.2-r0.apk (arch=$ARCH)"
echo "安装: 传到路由器后 apk add --allow-untrusted obpool-1.0.2-r0.apk"
