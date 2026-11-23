# Executed by the authenticated guest agent only inside a dedicated builder.
# Substitutions are a numeric URL on the private bridge and content digests.
set -eu
test -x /usr/local/bin/pulse-cloudhypervisor-guest
test ! -e /var/lib/pulse-guest/worker-intent
mkdir -m 0755 /opt/pulse-test-tools
cd /opt/pulse-test-tools
# Stream the pinned tar through a second pass after authentication.
curl -fsS --noproxy '*' --connect-timeout 5 --max-time 120 '@PAYLOAD_URL@' -o payload.tgz
printf '%s  payload.tgz\n' '@PAYLOAD_DIGEST@' | sha256sum -c -
tar -xzf payload.tgz
rm payload.tgz
printf '%s\n' /opt/pulse-test-tools/lib > /etc/ld.so.conf.d/zz-pulse-test-tools.conf
/sbin/ldconfig
mkdir -p /nix/var/nix/profiles/default/bin
ln -s /opt/pulse-test-tools/bin/nix /nix/var/nix/profiles/default/bin/nix
ln -s /opt/pulse-test-tools/bin/nix /usr/local/bin/nix-store
export NIX_CONFIG='build-users-group ='
/usr/local/bin/nix-store --init
for nar in nar/*; do
    /usr/local/bin/nix-store --restore "/nix/store/${nar##*/}" < "$nar"
    rm "$nar"
done
/usr/local/bin/nix-store --register-validity --hash-given < registration
rmdir nar
rm registration
ln -s '@C2J@' /usr/local/bin/c2j
ln -s '@DOCKER@' /usr/bin/docker
for program in containerd containerd-shim-runc-v2 runc dockerd; do
    ln -s "/opt/pulse-test-tools/bin/$program" "/usr/local/bin/$program"
done
group=$(getent group 1000 | cut -d: -f1)
test -n "$group"
cat > /etc/systemd/system/docker.service <<EOF
[Unit]
Description=Disposable guest-local Docker
After=local-fs.target
[Service]
Type=notify
ExecStart=/usr/local/bin/dockerd --group=$group --storage-driver=vfs --iptables=false --ip6tables=false --ip-forward=false --ip-masq=false --bridge=none --userland-proxy=false
Delegate=yes
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable docker.service
systemctl start docker.service
/usr/bin/docker load -i image.tar
rm image.tar
test "$(/usr/bin/docker image inspect --format '{{.Id}}' pulse-fixture:local)" = '@IMAGE@'
test -x /usr/bin/git
test ! -e /var/lib/pulse-guest/worker-intent
