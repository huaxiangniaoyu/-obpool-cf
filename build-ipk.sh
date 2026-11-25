#!/bin/sh
# 从源码构建 obpool 的 OpenWrt ipk 安装包 (aarch64)
# 用法: ./build-ipk.sh   产物: obpool_1.0.0_aarch64_generic.ipk
set -e
cd "$(dirname "$0")"
VERSION=1.0.0
ARCH=aarch64_generic

echo "编译 Go 二进制..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o obpool-bin .

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/data/opt/open-box/pool" "$WORK/control"
cp obpool-bin "$WORK/data/opt/open-box/pool/obpool"
cp run.sh "$WORK/data/opt/open-box/pool/run.sh"
sed -i 's#\./obpool-arm64#./obpool#' "$WORK/data/opt/open-box/pool/run.sh"
cp subs.txt.example "$WORK/data/opt/open-box/pool/subs.txt.example"
chmod 755 "$WORK/data/opt/open-box/pool/obpool" "$WORK/data/opt/open-box/pool/run.sh"
rm -f obpool-bin

cat > "$WORK/control/control" <<EOF
Package: obpool
Version: $VERSION
Architecture: $ARCH
Maintainer: huaxiangniaoyu
Section: utils
Priority: optional
Description: Cloudflare Worker node pool optimizer. Pulls all subscription
 nodes, crosses them with bestcf front domains, latency+bandwidth tests them
 on the router itself, and writes a weighted top-N mihomo YAML pool.
 Runs nightly at 20:40 via cron (edit /etc/crontabs/root to change).
EOF

cat > "$WORK/control/postinst" <<'EOF'
#!/bin/sh
[ -n "$IPKG_INSTROOT" ] && exit 0
POOL=/opt/open-box/pool
chmod 755 $POOL/obpool $POOL/run.sh
[ -f $POOL/subs.txt ] || cp $POOL/subs.txt.example $POOL/subs.txt
touch /etc/crontabs/root
grep -q 'open-box/pool/run.sh' /etc/crontabs/root || echo '40 20 * * * /opt/open-box/pool/run.sh' >> /etc/crontabs/root
/etc/init.d/cron enable >/dev/null 2>&1
/etc/init.d/cron restart >/dev/null 2>&1
echo "obpool installed. Put your subscriptions into $POOL/subs.txt, first run: $POOL/run.sh"
exit 0
EOF

cat > "$WORK/control/prerm" <<'EOF'
#!/bin/sh
[ -n "$IPKG_INSTROOT" ] && exit 0
sed -i '/open-box\/pool\/run.sh/d' /etc/crontabs/root 2>/dev/null
/etc/init.d/cron restart >/dev/null 2>&1
exit 0
EOF

printf '/opt/open-box/pool/subs.txt\n' > "$WORK/control/conffiles"
chmod 755 "$WORK/control/postinst" "$WORK/control/prerm"
echo '2.0' > "$WORK/debian-binary"

cd "$WORK/control" && tar --numeric-owner --owner=0 --group=0 -czf ../control.tar.gz ./control ./postinst ./prerm ./conffiles
cd "$WORK/data" && tar --numeric-owner --owner=0 --group=0 -czf ../data.tar.gz ./opt
cd "$WORK" && tar --numeric-owner --owner=0 --group=0 -czf "$OLDPWD/obpool_${VERSION}_${ARCH}.ipk" ./debian-binary ./control.tar.gz ./data.tar.gz
echo "完成: obpool_${VERSION}_${ARCH}.ipk"
echo "安装: 在路由器上 opkg install obpool_${VERSION}_${ARCH}.ipk"
