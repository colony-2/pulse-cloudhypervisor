# Executed by the authenticated guest agent only inside a dedicated builder.
# Substitutions are a numeric URL on the private bridge and content digests.
set -eu
# Install the credential-free toolchain for UID 1000. The guest service's
# private umask would make newly created Nix directories root-only.
umask 022
test -x /usr/local/bin/pulse-cloudhypervisor-guest
test ! -e /var/lib/pulse-guest/worker-intent
mkdir -m 0755 /opt/pulse-test-tools
cd /opt/pulse-test-tools
# Authenticate the transport archive in volatile storage. Preserve root space
# for the installed executables, restored Nix closure and local Docker image.
temporary=$(mktemp -d /run/pulse-tools.XXXXXXXX)
trap 'rm -rf -- "$temporary"' EXIT
curl -fsS --noproxy '*' --connect-timeout 5 --max-time 120 '@PAYLOAD_URL@' -o "$temporary/payload.tgz"
printf '%s  %s\n' '@PAYLOAD_DIGEST@' "$temporary/payload.tgz" | sha256sum -c -
tar -xzf "$temporary/payload.tgz" --exclude='nar/*'
printf '%s\n' /opt/pulse-test-tools/lib > /etc/ld.so.conf.d/zz-pulse-test-tools.conf
/sbin/ldconfig
mkdir -p /nix/var/nix/profiles/default/bin
ln -s /opt/pulse-test-tools/bin/nix /nix/var/nix/profiles/default/bin/nix
ln -s /opt/pulse-test-tools/bin/nix /usr/local/bin/nix-store
export NIX_CONFIG='build-users-group ='
/usr/local/bin/nix-store --init
python3 - "$temporary/payload.tgz" <<'PY'
import re
import shutil
import subprocess
import sys
import tarfile

# Stream each authenticated NAR directly into its installed store path.
with tarfile.open(sys.argv[1], 'r|gz') as archive:
    for member in archive:
        if not member.name.startswith('nar/'):
            continue
        name = member.name[4:]
        if not member.isfile() or not re.fullmatch(r'[a-z0-9]{32}-[^/\s]+', name):
            raise ValueError('invalid NAR path')
        with archive.extractfile(member) as source, subprocess.Popen(
            ['/usr/local/bin/nix-store', '--restore', '/nix/store/' + name],
            stdin=subprocess.PIPE,
        ) as restore:
            with restore.stdin:
                shutil.copyfileobj(source, restore.stdin)
            if restore.wait() != 0:
                raise RuntimeError('NAR restore failed')
PY
rm "$temporary/payload.tgz"
/usr/local/bin/nix-store --register-validity --hash-given < registration
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
