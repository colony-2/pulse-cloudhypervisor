"""Build credential-free tools for the disposable Noble ARM64 builder."""
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile

from native_assets import digest

# Executable bytes observed on the qualified Ubuntu base. Nix store identity
# and SHA pin the c2j binary separately from the isolated JobDB test module.
C2J_STORE = '/nix/store/bn18vzyxp555n5f8n5air3vyb33vf5y1-c2j-unstable-a61afac'
C2J_REVISION = 'a61afac98358c0e34b5a351a153f2704a99b2ae5'
C2J_DERIVER = '/nix/store/ay3xvz6214hwrghl4asyl79ijhmn5y0p-c2j-unstable-a61afac.drv'
C2J_SOURCE = '/nix/store/6qi4rm1ar4x3za04jkk3cpbbv8y7lknw-source'
# The derivation's filtered source was compared file by file with this
# exact upstream Git commit. The cloud binary embeds only its short revision.
C2J_SOURCE_NAR = 'sha256-6S9R70njMkIUxSoMzzuKRIIPu7ZWe76W78OoqtWz1H8='
DOCKER_STORE = '/nix/store/afml0pzyz1pwqz85mpnb91mbhy262jh2-docker-29.8.1'
MANIFEST_SHA256 = '8a153ec1db9e2bb6dd6afd0c216401314712852f3f54171fce41f9e61ff614cd'
PINS = {
    C2J_STORE+'/bin/c2j': 'f67c31be5101f991dd764a90dc5ddcc6da43407e3bac9e63467188e92cd97016',
    DOCKER_STORE+'/bin/docker': '961267bb8479d8e5a7afb786a0dac13d5c8c3d1c526e06c94711740bccd0cf81',
    '/usr/bin/nix': '65e2c7e929ed43db5398fd7eeb10075ab381c479aa63abee9410dbc62c30d981',
    '/usr/bin/dockerd': '5e3505b18446d5e1b0841cf0cdad340260879b82b3a8bc49443adeb4fccf1046',
    '/usr/bin/containerd': 'c2812d361c73f6d2441406032d825a2c5039ea15333ab2171a1423df02b3b9d8',
    '/usr/bin/containerd-shim-runc-v2': '8a0426ca0a45194dbcec660b5d634adfec3a73d07dbdedc15ba8e40f3a3f933d',
    '/usr/bin/runc': '4dc400fbfdb9d64049cde25d751bf8520edb70a85afb67f595dcd5e35116c42b',
    '/usr/bin/busybox': '52151e7f322f926b64049cdaa1410dc3ea6485525e0624b05813791c219ae933',
}


def libraries(program):
    proc = subprocess.run(['ldd', program], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                          text=True, timeout=20, env={'PATH': '/usr/bin:/bin', 'LC_ALL': 'C'})
    if 'not a dynamic executable' in proc.stdout or 'statically linked' in proc.stdout:
        return {}
    proc.check_returncode()
    libs = {}
    for line in proc.stdout.splitlines():
        if 'linux-vdso.so' in line:
            continue
        match = re.fullmatch(r'\s*(?:\S+ => )?(/(?:usr/)?lib/[^\s]+) \(0x[0-9a-f]+\)', line)
        if not match or '..' in Path(match[1]).parts:
            raise ValueError('unresolved guest tool dependency')
        libs[match[1].lstrip('/')] = Path(match[1]).resolve()
    return libs


def add(archive, name, source, mode=0o755):
    info = tarfile.TarInfo(name)
    info.mode = mode
    if isinstance(source, bytes):
        info.size = len(source)
        archive.addfile(info, io.BytesIO(source))
    else:
        with source.open('rb') as f:
            info.size = os.fstat(f.fileno()).st_size
            archive.addfile(info, f)


def docker_archive(path, busybox):
    # No registry access. Import a Docker save archive containing a deterministic
    # layer built from the pinned BusyBox. Verify its config ID with docker load.
    layer = io.BytesIO()
    with tarfile.open(fileobj=layer, mode='w') as tar:
        for name, src in sorted(libraries(busybox).items()):
            add(tar, name, src)
        add(tar, 'bin/busybox', busybox)
        for applet in ('sh', 'id', 'true'):
            info = tarfile.TarInfo('bin/'+applet)
            info.type, info.linkname, info.mode = tarfile.SYMTYPE, 'busybox', 0o777
            tar.addfile(info)
    data = layer.getvalue()
    layer_id = hashlib.sha256(data).hexdigest()
    config = json.dumps({'architecture': 'arm64', 'os': 'linux',
                         'config': {'Env': ['PATH=/bin']},
                         'rootfs': {'type': 'layers', 'diff_ids': ['sha256:'+layer_id]}},
                        separators=(',', ':')).encode()
    config_id = hashlib.sha256(config).hexdigest()
    with tarfile.open(path, 'x') as tar:
        add(tar, layer_id+'/layer.tar', data, 0o644)
        add(tar, config_id+'.json', config, 0o644)
        manifest = [{'Config': config_id+'.json', 'RepoTags': ['pulse-fixture:local'], 'Layers': [layer_id+'/layer.tar']}]
        add(tar, 'manifest.json', json.dumps(manifest).encode(), 0o644)
    return 'sha256:'+config_id


def manifest_digest(payload):
    rows = []
    names = set()
    with tarfile.open(payload, 'r|gz') as tar:
        for item in tar:
            if (not item.isreg() or item.name in names or
                    not re.fullmatch(r'(?:[\w.+-]+/)?[\w.+-]+', item.name) or
                    any(part in ('.', '..') for part in item.name.split('/')) or
                    item.uid != 0 or item.gid != 0):
                raise ValueError('unexpected guest tool archive member')
            names.add(item.name)
            with tar.extractfile(item) as source:
                sha = hashlib.file_digest(source, 'sha256').hexdigest()
            rows.append([item.name, item.mode, item.size, sha])
    encoded = json.dumps(sorted(rows), separators=(',', ':'), ensure_ascii=True).encode()
    return hashlib.sha256(encoded).hexdigest()


def build(directory):
    for command, expected in (
        (['nix-store', '-q', '--deriver', C2J_STORE], C2J_DERIVER),
        (['nix-store', '-q', '--binding', 'src', C2J_DERIVER], C2J_SOURCE),
        (['nix', '--extra-experimental-features', 'nix-command', 'hash', 'path', C2J_SOURCE], C2J_SOURCE_NAR),
    ):
        if subprocess.check_output(command, text=True, timeout=30).strip() != expected:
            raise ValueError('c2j executable provenance mismatch')
    for path, sha in PINS.items():
        if digest(path) != sha:
            raise ValueError('installed guest payload executable pin mismatch')
    image = directory/'docker-image.tar'
    image_id = docker_archive(image, Path('/usr/bin/busybox'))
    # Export closures from the local store; export/import neither evaluates nor
    # builds a derivation. Never archive the worker home, configuration or DB.
    closure = subprocess.check_output(['nix-store','-qR',C2J_STORE, DOCKER_STORE], text=True, timeout=30).splitlines()
    if not closure or any(not re.fullmatch(r'/nix/store/[a-z0-9]{32}-[^/\s]+', p) for p in closure):
        raise ValueError('invalid Nix closure')
    # The order of equivalent requisites and the deriver metadata in a store
    # export can depend on the host DB. Use canonical NAR dumps and the exact
    # reference graph to register those paths in the fresh guest.
    nar_dir=directory/'nar'
    nar_dir.mkdir(mode=0o700)
    nar_files={}
    registrations=[]
    for store in sorted(closure):
        name=Path(store).name
        path=nar_dir/name
        with path.open('xb') as f:
            subprocess.run(['nix-store','--dump',store],stdout=f,check=True,timeout=90)
        refs=subprocess.check_output(['nix-store','-q','--references',store],text=True,timeout=20).splitlines()
        if not set(refs)<=set(closure):
            raise ValueError('incomplete Nix closure')
        nar_files['nar/'+name]=path
        # nix-store --register-validity --hash-given reads one ValidPathInfo
        # per record: path, hex SHA256 of NAR, NAR size, deriver, refs.
        registrations.extend([store,digest(path),str(path.stat().st_size),'',str(len(refs)),*sorted(refs)])
    db=('\n'.join(registrations)+'\n').encode()
    payload = directory/'tools.tgz'
    try:
        files = {}
        for name in ('nix','dockerd','containerd','containerd-shim-runc-v2','runc'):
            src = Path('/usr/bin')/name
            files['bin/'+name] = src
            for path, real in libraries(src).items():
                target = 'lib/'+Path(path).name
                if target in files and files[target]!=real:
                    raise ValueError('ambiguous tool library')
                files[target] = real
        files.update(nar_files)
        files.update({'registration':db,'image.tar': image})
        with payload.open('xb') as f, gzip.GzipFile(filename='', fileobj=f, mode='wb', compresslevel=1, mtime=0) as zipped:
            with tarfile.open(fileobj=zipped, mode='w|') as tar:
                for name, src in sorted(files.items()):
                    add(tar, name, src)
    finally:
        for path in nar_files.values():
            path.unlink()
        nar_dir.rmdir()
        image.unlink(missing_ok=True)
    measured = manifest_digest(payload)
    if measured != MANIFEST_SHA256:
        print('FAIL: guest tool manifest sha256=' + measured, flush=True)
        payload.unlink()
        raise ValueError('guest tool closure or library digest mismatch')
    return payload, image_id
