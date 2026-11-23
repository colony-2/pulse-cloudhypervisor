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
C2J_STORE = '/nix/store/lfnk1pwadjcalsk19ayl8f333lmp5jvm-c2j-unstable-3991db6'
DOCKER_STORE = '/nix/store/afml0pzyz1pwqz85mpnb91mbhy262jh2-docker-29.8.1'
PINS = {
    C2J_STORE+'/bin/c2j': '8fbf660e09492daac03ed6f753bfeb345e5c235b52c8ba37788c7740a3cc89af',
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


def build(directory):
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
    return payload, image_id
