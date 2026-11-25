# Executed inside the disposable Ubuntu builder.
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
umask 022
src=$(mktemp -d /var/tmp/pulse-install.XXXXXXXX)
stage=10
trap 'rc=$?; rm -rf -- "$src"; if [ "$rc" != 0 ]; then exit "$stage"; fi' EXIT
tar xf /var/tmp/pulse-bootstrap-tools.tar -C "$src"
rm /var/tmp/pulse-bootstrap-tools.tar
install -m 0755 "$src/c2j" /usr/local/bin/c2j
stage=11
tar xJf "$src/nix.tar.xz" -C "$src"
set -- "$src"/nix-*-linux
test "$#" = 1
dist=$1
mkdir -p /nix/store /nix/var/nix/profiles /etc/nix
cp -a "$dist/store/." /nix/store/
set -- /nix/store/*-nix-2.28.5
test "$#" = 1
ln -s "$1" /nix/var/nix/profiles/default
printf 'build-users-group =\n' > /etc/nix/nix.conf
chown -R 1000:1000 /nix
runuser -u worker -- /nix/var/nix/profiles/default/bin/nix-store --load-db < "$dist/.reginfo"
rm -rf "$dist" "$src/nix.tar.xz"
stage=12
tar xzf "$src/docker.tgz" -C /usr/local/bin --strip-components=1
ln -sf /usr/local/bin/docker /usr/bin/docker
group=$(getent group 1000 | cut -d: -f1)
test -n "$group"
cat > /etc/systemd/system/docker.service <<UNIT
[Unit]
Description=Docker in the Pulse worker
After=network.target
[Service]
Type=notify
ExecStart=/usr/local/bin/dockerd --group=$group
Delegate=yes
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
stage=13
systemctl enable --now docker.service
stage=14
/usr/bin/docker load -i "$src/image.tar"
