"""Read-only ARM64 KVM capability observation, not reboot authority."""
import fcntl
import json
import os
import pathlib
import platform
import stat
import subprocess
import sys

from placement import read_placement
from verification import ROOT, decode, digest, require, write_json

KVM_GET_API_VERSION = 0xAE00
KVM_CHECK_EXTENSION = 0xAE03
KVM_CAP_ARM_EL2 = 240
UNPROVEN = {'api': None, 'arm_el2': None, 'status': 'unproven'}


def query():
    require(platform.system() == 'Linux' and platform.machine() == 'aarch64')
    fd = os.open('/dev/kvm', os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        require(stat.S_ISCHR(os.fstat(fd).st_mode))
        api = fcntl.ioctl(fd, KVM_GET_API_VERSION, 0)
        el2 = fcntl.ioctl(fd, KVM_CHECK_EXTENSION, KVM_CAP_ARM_EL2) if api == 12 else None
        status = 'supported' if el2 is not None and el2 > 0 else 'unsupported' if el2 == 0 else 'unproven'
        return {'api': api, 'arm_el2': el2, 'status': status}
    finally:
        os.close(fd)


def checked(value):
    require(isinstance(value, dict) and set(value) == set(UNPROVEN))
    api, el2 = value['api'], value['arm_el2']
    require(api is None or type(api) is int and 0 <= api <= 2**31-1)
    require(el2 is None or type(el2) is int and 0 <= el2 <= 2**31-1 and api == 12)
    expected = 'supported' if el2 is not None and el2 > 0 else 'unsupported' if el2 == 0 else 'unproven'
    require(value['status'] == expected)
    return value


def observe(timeout=5):
    try:
        result = subprocess.run([sys.executable, str(pathlib.Path(__file__).resolve())],
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                timeout=timeout, check=True)
        require(len(result.stdout) <= 256)
        return checked(decode(result.stdout))
    except (OSError, ValueError, TypeError, UnicodeError, subprocess.SubprocessError):
        return UNPROVEN.copy()


def binding_fields(placement):
    return {field: getattr(placement, field) for field in ('job', 'tenant', 'task', 'event', 'worker', 'boot', 'base')}


def identity(placement, candidate, source):
    return {'placement': binding_fields(placement), 'candidate': candidate,
            'source_sha256': source, 'mapping_sha256': digest(ROOT/'tests/expectations.json')}


def record(path, placement, candidate, source, observation, run):
    write_json(path, {'version': 1, 'run': run, 'identity': identity(placement, candidate, source),
                      'kvm': checked(observation)})


def validate(path, candidate, source, run):
    require(isinstance(run, str) and len(run) == 32 and all(char in '0123456789abcdef' for char in run))
    path = pathlib.Path(path)
    require(path.is_absolute())
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK), 'rb') as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and info.st_uid == os.geteuid())
        data = stream.read(8193)
    require(len(data) <= 8192)
    evidence = decode(data)
    require(isinstance(evidence, dict) and set(evidence) == {'version', 'run', 'identity', 'kvm'})
    require(type(evidence['version']) is int and evidence['version'] == 1 and evidence['run'] == run)
    expected = identity(read_placement(), candidate, source)
    require(expected == evidence['identity'])
    return checked(evidence['kvm']), expected


if __name__ == '__main__':
    try:
        print(json.dumps(checked(query())))
    except (OSError, ValueError):
        sys.exit(1)
