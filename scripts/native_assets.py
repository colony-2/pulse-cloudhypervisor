#!/usr/bin/env python3
"""Stage measured, unprepared Ubuntu ARM64 boot inputs without host privileges.

These are source assets only: no guest tools, preparation proof or LIVE receipt
is produced. Never run downloaded code or copy attributes from the archive.
"""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import subprocess
import struct
import tarfile
import uuid
import zlib

RELEASE = 'https://cloud-images.ubuntu.com/releases/noble/release-20261017/'
# Recovered from the immutable release's SHA256SUMS and unpacked/SHA256SUMS.
SOURCES = {
    'archive': ('ubuntu-24.04-server-cloudimg-arm64.tar.gz',
                '8f9178835f6535cbc2e65eb7a8e7f705d4a145df2fa67d8da3896b40c1304097'),
    'kernel': ('unpacked/ubuntu-24.04-server-cloudimg-arm64-vmlinuz-generic',
               '353bab8eeaad9caf931dd85fd7a7bf8800d2669d759c0369ffaba7abc03a7aff'),
    'initramfs': ('unpacked/ubuntu-24.04-server-cloudimg-arm64-initrd-generic',
                  '86e1b682fc0655e7b6d455c44f8ea2d7f0d35a8ff097e292da6ee46a206041fd'),
}
BLOCK = 1024 * 1024
MAX_DISK = 16 * 1024**3
MAX_KERNEL = 256 * 1024**2


def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(BLOCK), b''):
            h.update(block)
    return h.hexdigest()


def copy_sparse(source, target, limit, offset=0, prefix=b''):
    """Write a new private file, bounding logical size and retaining zero holes."""
    count = 0
    try:
        with target.open('xb') as out:
            os.chmod(target, 0o600)
            out.seek(offset)
            while True:
                block = prefix or source.read(BLOCK)
                prefix = b''
                if not block:
                    break
                count += len(block)
                if count > limit:
                    raise ValueError('asset exceeds size bound')
                if block.strip(b'\0'):
                    out.write(block)
                else:
                    out.seek(len(block), os.SEEK_CUR)
            out.truncate(offset + count)
    except FileExistsError:
        raise
    except BaseException:
        target.unlink(missing_ok=True)
        raise
    return count


def partition_rootfs(target, size, identity):
    """Wrap Ubuntu's UEC ext4 root in deterministic GPT partition 1 (vda1)."""
    start = 2048
    sectors = start + size // 512 + 2048
    disk_id = uuid.UUID(identity[:32])
    linux = uuid.UUID('0fc63daf-8483-4772-8e79-3d69d8477de4')
    entry = struct.pack('<16s16sQQQ72s', linux.bytes_le,
                        uuid.uuid5(disk_id, 'root-vda1').bytes_le,
                        start, start + size // 512 - 1, 0,
                        'root'.encode('utf-16-le'))
    entries = entry.ljust(128 * 128, b'\0')
    def header(current, other, table):
        data = bytearray(struct.pack('<8sIIIIQQQQ16sQIII', b'EFI PART',
                                    0x10000, 92, 0, 0, current, other, 34,
                                    sectors - 34, disk_id.bytes_le,
                                    table, 128, 128, zlib.crc32(entries)))
        struct.pack_into('<I', data, 16, zlib.crc32(data))
        return data.ljust(512, b'\0')
    mbr = bytearray(512)
    struct.pack_into('<B3sB3sII', mbr, 446, 0, b'\x00\x02\x00', 0xee,
                     b'\xff\xff\xff', 1, min(sectors - 1, 0xffffffff))
    mbr[510:512] = b'\x55\xaa'
    with target.open('r+b') as f:
        f.write(mbr)
        f.write(header(1, sectors - 1, 2))
        f.write(entries)
        f.seek((sectors - 33) * 512)
        f.write(entries)
        f.write(header(sectors - 1, 1, sectors - 33))
        f.truncate(sectors * 512)


def extract_disk(archive, expected_digest, target):
    if digest(archive) != expected_digest:
        raise ValueError('archive digest mismatch')
    if target.exists() or target.is_symlink():
        raise FileExistsError('disk destination already exists')
    found, created = False, False
    try:
        with tarfile.open(archive, 'r|gz') as tar:
            for member in tar:
                name = member.name.removeprefix('./')
                # The authenticated release may contain metadata alongside its
                # one disk. No path traversal, links, devices or extraction API.
                if '/' in name or name in ('', '.', '..') or not member.isfile():
                    raise ValueError('unsafe archive member')
                if not name.endswith('.img'):
                    continue
                if found or not 512 <= member.size <= MAX_DISK:
                    raise ValueError('ambiguous or oversized archive disk')
                found = True
                with tar.extractfile(member) as source:
                    first = source.read(4096)
                    rootfs = first[1080:1082] == b'\x53\xef'
                    if rootfs and member.size % 512:
                        raise ValueError('unaligned ext4 image')
                    count = copy_sparse(source, target, member.size,
                                        offset=1024*1024 if rootfs else 0, prefix=first)
                    created = True
                    if count != member.size:
                        raise ValueError('truncated archive disk')
                    if rootfs:
                        partition_rootfs(target, member.size, expected_digest)
        if not found:
            raise ValueError('archive contains no raw disk')
        with target.open('rb') as f:
            header = f.read(520)
        if header[510:512] != b'\x55\xaa' or header[512:520] != b'EFI PART':
            raise ValueError('partitioned raw GPT disk required')
    except BaseException:
        if created:
            target.unlink(missing_ok=True)
        raise


def unpack_kernel(source, target):
    if target.exists() or target.is_symlink():
        raise FileExistsError('kernel destination already exists')
    with source.open('rb') as f:
        compressed = f.read(2) == b'\x1f\x8b'
    opener = gzip.open if compressed else open
    created = False
    try:
        with opener(source, 'rb') as f:
            copy_sparse(f, target, MAX_KERNEL)
            created = True
        with target.open('rb') as f:
            header = f.read(64)
        if header[56:60] != b'ARM\x64':
            raise ValueError('ARM64 Image kernel required')
    except FileExistsError:
        raise
    except BaseException:
        if created:
            target.unlink(missing_ok=True)
        raise


def materialize(directory):
    """Recover only verified downloads; existing outputs are never replaced."""
    sources = {}
    for name, (relative, expected) in SOURCES.items():
        path = directory / name
        if digest(path) != expected:
            raise ValueError('download digest mismatch')
        sources[name] = {'url': RELEASE + relative, 'sha256': expected}
    disk, kernel = directory/'ubuntu.raw', directory/'Image'
    extract_disk(directory/'archive', SOURCES['archive'][1], disk)
    unpack_kernel(directory/'kernel', kernel)
    result = {'version': 1, 'stage': 'unprepared-ubuntu-24.04-arm64', 'sources': sources}
    for name, path in [('disk', disk), ('kernel', kernel), ('initramfs', directory/'initramfs')]:
        result[name] = {'path': str(path), 'sha256': digest(path)}
    with (directory/'assets.json').open('x') as f:
        json.dump(result, f, indent=2)
        f.write('\n')
    print('PASS: measured unprepared raw disk, ARM64 Image and initramfs', flush=True)
    print('Guest bootstrap/tools and native execution remain untested', flush=True)


def stage(directory):
    if not directory.is_absolute():
        raise ValueError('absolute new output directory required')
    directory.mkdir(mode=0o700)  # Never replace an existing collection.
    for name, (relative, expected) in SOURCES.items():
        path = directory / name
        subprocess.run(['curl', '--proto', '=https', '--proto-redir', '=https',
                        '--fail', '--silent', '--show-error', '--location',
                        '--max-time', '300', '--output', str(path), RELEASE + relative],
                       check=True, timeout=310)
        if digest(path) != expected:
            raise ValueError('download digest mismatch')
        print('PASS: downloaded and verified ' + name, flush=True)
    materialize(directory)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    stage(args.output)
