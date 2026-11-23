#!/usr/bin/env python3
"""Append the measured, builder-only guest bootstrap to a pinned Ubuntu initrd."""
import argparse
import gzip
import hashlib
import hmac
import os
from pathlib import Path
import struct
import tempfile

from native_assets import digest

ROOT = Path(__file__).resolve().parents[1]
MAX_AGENT = 32 * 1024**2

# This init runs only in the pinned test initramfs. Mount the supplied GPT
# partition directly. No firmware, udev database or cloud metadata is needed.
INIT = b'''#!/bin/sh
set -eu
export PATH=/bin:/sbin:/usr/bin:/usr/sbin
mkdir -p /dev /proc /sys /run /root
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs tmpfs /run
for driver in virtio_pci virtio_blk virtio_rng virtio_net vmw_vsock_virtio_transport ext4; do modprobe "$driver" || :; done
i=0
while [ ! -b /dev/vda1 ] && [ "$i" -lt 60 ]; do sleep 1; i=$((i+1)); done
mount -t ext4 -o rw /dev/vda1 /root
# Match the block device reported by the mounted filesystem before writing.
test "$(cat /sys/class/block/vda1/dev)" = "$(awk '$5=="/root" {print $3}' /proc/self/mountinfo)"
for fs in dev proc sys run; do mount -o move "/$fs" "/root/$fs"; done
case " $(cat /root/proc/cmdline) " in
    *" pulse.prepare="*) /pulse-bootstrap/install ;;
esac
exec run-init /root /sbin/init
'''

# Resolve os-release inside the chroot when booting. Unit tests use the same
# predicate with temporary roots containing incorrect identity or stale intent.
ROOT_CHECK = b'''set -eu
root=${1%/}
. "$root/etc/os-release"
test "$ID/$VERSION_ID" = ubuntu/24.04
test "$(uname -m)" = aarch64
test ! -e "$root/var/lib/pulse-guest/worker-intent"
'''

INSTALL = b'''#!/bin/sh
set -eu
root=/root
case " $(cat "$root/proc/cmdline") " in *" pulse.prepare="*) ;; *) exit 1 ;; esac
chroot "$root" /bin/sh -ec '@ROOT_CHECK@' sh /
chroot "$root" /usr/sbin/resize2fs /dev/vda1
cp /pulse-bootstrap/agent "$root/usr/local/bin/pulse-cloudhypervisor-guest"
cp /pulse-bootstrap/service "$root/etc/systemd/system/pulse-cloudhypervisor-guest.service"
chroot "$root" /bin/sh -ec '
    chmod 0755 /usr/local/bin/pulse-cloudhypervisor-guest
    chmod 0644 /etc/systemd/system/pulse-cloudhypervisor-guest.service
    install -d -m 0700 /var/lib/pulse-guest
    # The pinned base has no UID/GID 1000. Install only the locked local
    # identity the guest agent uses, rejecting name or numeric collisions.
    group="worker:!:1000:"
    account="worker:!:1000:1000:Pulse worker:/home/worker:/bin/sh"
    for db in group passwd; do
        record=$group
        if [ "$db" = passwd ]; then record=$account; fi
        matches=$(grep -E "^worker:|^[^:]*:[^:]*:1000:" "/etc/$db") || [ "$?" = 1 ]
        if [ -z "$matches" ]; then
            printf "%s\\n" "$record" >> "/etc/$db"
        else
            test "$matches" = "$record"
        fi
    done
    install -d -o 1000 -g 1000 -m 0700 /home/worker
    install -d /etc/systemd/system/multi-user.target.wants
    ln -sf ../pulse-cloudhypervisor-guest.service /etc/systemd/system/multi-user.target.wants/pulse-cloudhypervisor-guest.service
    touch /etc/cloud/cloud-init.disabled
'
'''.replace(b'@ROOT_CHECK@', ROOT_CHECK)


def newc(stream, number, name, mode, data):
    # All names are fixed below. Root ownership, stable inodes and zero times.
    name = name.encode('ascii') + b'\0'
    fields = [number, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name), 0]
    header = b'070701' + ''.join(f'{x:08x}' for x in fields).encode()
    stream.write(header + name + bytes(-(len(header) + len(name)) % 4))
    stream.write(data + bytes(-len(data) % 4))


def build(initrd, agent, output, initrd_sha, agent_sha):
    if not output.is_absolute():
        raise ValueError('absolute output path required')
    with agent.open('rb') as f:
        program = f.read(MAX_AGENT + 1)
    if (not 64 <= len(program) <= MAX_AGENT or program[:6] != b'\x7fELF\x02\x01'
            or struct.unpack_from('<H', program, 18)[0] != 183
            or not hmac.compare_digest(hashlib.sha256(program).hexdigest(), agent_sha)):
        raise ValueError('candidate ARM64 agent mismatch')
    unit = (ROOT/'guest/pulse-cloudhypervisor-guest.service').read_bytes()
    entries = [('pulse-bootstrap', 0o40755, b''),
               ('pulse-bootstrap/install', 0o100755, INSTALL),
               ('pulse-bootstrap/agent', 0o100755, program),
               ('pulse-bootstrap/service', 0o100644, unit),
               ('init', 0o100755, INIT),
               ('TRAILER!!!', 0, b'')]
    # Publish the complete file without ever replacing a caller's destination.
    with tempfile.NamedTemporaryFile(dir=output.parent) as tmp:
        with initrd.open('rb') as base:
            copied = hashlib.sha256()
            while block := base.read(1024 * 1024):
                copied.update(block)
                tmp.write(block)
        if not hmac.compare_digest(copied.hexdigest(), initrd_sha):
            raise ValueError('Ubuntu initramfs mismatch')
        tmp.write(bytes(-tmp.tell() % 4))
        with gzip.GzipFile(filename='', mode='wb', fileobj=tmp, mtime=0) as zipped:
            for number, entry in enumerate(entries, 1):
                newc(zipped, number, *entry)
        tmp.flush()
        os.fsync(tmp.fileno())
        os.link(tmp.name, output)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('initrd', 'agent', 'output'):
        parser.add_argument('--'+name, type=Path, required=True)
    parser.add_argument('--agent-sha256', required=True)
    args = parser.parse_args()
    from native_assets import SOURCES
    build(args.initrd, args.agent, args.output, SOURCES['initramfs'][1], args.agent_sha256)
    print('builder initramfs sha256=' + digest(args.output))
