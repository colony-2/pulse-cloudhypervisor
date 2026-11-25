"""Construct immutable native boot assets from pinned upstream releases."""
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

import native_assets
import native_setup

C2J = 'v0.0.129-0.20261123054030-a61afac98358'
PLATFORM = {'arm64': 'aarch64', 'amd64': 'x86_64'}
PINS = {
    'arm64': {
        'vmm': '446de7552dacc83bc86d1137e7ffa7c2d204cdda4850eac4f2d2666d63c0f697',
        'nix': 'a7d20e2897de8044ed35a481d455e88130b6d70e40f2470d1b7c26727f6c8d8c',
        'docker': 'ee2507268dc15345b5dc132e8246e0f29f2d629cf8cebb966e2a2008418b7d76',
        'alpine': '6bf491907b705caa5fc65773fbbc1d0954530b6c455d4446f670a4c1fdcea489',
        'go': '211ffced9dcb9633a55eac6364816ec0ddd951389a740e88fa8b3337971bdda0'},
    'amd64': {
        'vmm': 'ce7133bf535ed5a4e2a63ea1f26025445a5b1241f73ddfb81ff2fbcb2e803a59',
        'nix': '2b7e9fda8e17e2143c302a700908e86698c265b5862fc7e6b805942aceef6668',
        'docker': '079e491e942e7a7a090c593db7eb63f3d657b5ca166c38dab59678b165b91eca',
        'alpine': '412454ed98025ad9cd910d13f3d448b184e81502baa83180e89f98d0f13674be',
        'go': 'd0f743b33e8d8945e6b1f432edd15785c70507121d6e2a723b21285eddf8b57b'}}


def download(cache, url, sha):
    path = cache / sha
    if path.is_file() and native_assets.digest(path) == sha:
        return path
    print(f'Downloading {url} (sha256 {sha})', flush=True)
    with tempfile.NamedTemporaryFile(dir=cache) as tmp:
        subprocess.run(['curl', '-fLsS', '--proto', '=https', '--proto-redir', '=https',
                        '--retry', '2', '--connect-timeout', '15', '--max-time', '600',
                        '--output', tmp.name, url], check=True)
        if native_assets.digest(Path(tmp.name)) != sha:
            raise ValueError(f'digest mismatch for {url}')
        with open(tmp.name, 'rb') as f:
            os.fsync(f.fileno())
        # Unique download temp; existing inodes in use are never modified.
        link = Path(tmp.name + '.complete')
        os.link(tmp.name, link)
        os.replace(link, path)
    return path


def toolchain(arch, cache, work):
    go = shutil.which('go')
    if go:
        result = subprocess.run([go, 'env', 'GOVERSION'], capture_output=True, text=True)
        import re
        version = re.match(r'go(\d+)\.(\d+)', result.stdout)
        if version and tuple(map(int, version.groups())) >= (1, 26):
            return go
    tar = download(cache, f'https://go.dev/dl/go1.26.8.linux-{arch}.tar.gz', PINS[arch]['go'])
    subprocess.run(['tar', 'xzf', tar, '-C', work], check=True)
    return str(work / 'go/bin/go')


def tool_hash(archive, predicate):
    with tarfile.open(archive, 'r:*') as tar:
        member, = [m for m in tar if m.isfile() and predicate(m.name)]
        with tar.extractfile(member) as f:
            return hashlib.file_digest(f, 'sha256').hexdigest() if hasattr(hashlib, 'file_digest') else stream_digest(f)


def stream_digest(stream):
    h = hashlib.sha256()
    for block in iter(lambda: stream.read(1 << 20), b''):
        h.update(block)
    return h.hexdigest()


def docker_image(arch, alpine, path):
    # Construct a Docker save archive with a real shell and userland, offline.
    with gzip.open(alpine, 'rb') as layer:
        data = layer.read()
    layer_hash = hashlib.sha256(data).hexdigest()
    config = json.dumps({'architecture':arch,'os':'linux','config':{'Env':['PATH=/usr/bin:/bin']},
                         'rootfs':{'type':'layers','diff_ids':['sha256:'+layer_hash]}},
                        separators=(',', ':')).encode()
    image = hashlib.sha256(config).hexdigest()
    manifest = [{'Config':image+'.json','RepoTags':['pulse-fixture:local'],'Layers':[layer_hash+'/layer.tar']}]
    with tarfile.open(path, 'w') as tar:
        for name, content in [(layer_hash+'/layer.tar', data), (image+'.json', config),
                              ('manifest.json', json.dumps(manifest).encode())]:
            info = tarfile.TarInfo(name)
            info.size = len(content)
            tar.addfile(info, io.BytesIO(content))
    return 'sha256:'+image


def builder_initrd(initrd, tools, target):
    shutil.copyfile(initrd, target)
    with target.open('ab') as f:
        f.write(bytes(-f.tell() % 4))
        with gzip.GzipFile(fileobj=f, mode='wb', compresslevel=1, mtime=0, filename='') as gz:
            # Stream the potentially large tool archive as one cpio member.
            name = b'pulse-bootstrap/tools.tar\0'
            size = tools.stat().st_size
            fields = [1, 0o100600, 0, 0, 1, 0, size, 0, 0, 0, 0, len(name), 0]
            header = b'070701'+b''.join(f'{n:08x}'.encode() for n in fields)+name
            gz.write(header+bytes(-len(header)%4))
            with tools.open('rb') as src:
                shutil.copyfileobj(src, gz, 1<<20)
            gz.write(bytes(-size%4))
            native_setup.newc(gz, 2, 'TRAILER!!!', 0, b'')


def build(root, cache, staging, final, arch, disk_bytes):
    platform = PLATFORM[arch]
    pins = PINS[arch]
    def asset(name):
        return {'path': str(final/name), 'sha256': native_assets.digest(staging/name)}
    urls = {'vmm':'https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v55.0/cloud-hypervisor-static'+('-aarch64' if arch=='arm64' else ''),
            'nix': f'https://releases.nixos.org/nix/nix-2.28.5/nix-2.28.5-{platform}-linux.tar.xz',
            'docker': f'https://download.docker.com/linux/static/stable/{platform}/docker-29.0.2.tgz',
            'alpine': f'https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/{platform}/alpine-minirootfs-3.22.2-{platform}.tar.gz'}
    with tempfile.TemporaryDirectory(dir=staging) as tmp:
        work = Path(tmp)
        parts = {k:download(cache,url,pins[k]) for k,url in urls.items()}
        shutil.copyfile(parts['vmm'], staging/'cloud-hypervisor')
        (staging/'cloud-hypervisor').chmod(0o700)
        imgs = {k:download(cache,native_assets.RELEASE+url,sha) for k,(url,sha) in native_assets.SOURCES_BY_ARCH[arch].items()}
        # The GPT wrapper reserves 2 MiB outside the root partition.
        native_assets.extract_disk(imgs['archive'], imgs['archive'].name, staging/'ubuntu.raw', disk_bytes-(2<<20))
        if (staging/'ubuntu.raw').stat().st_size != disk_bytes:
            raise ValueError('configured disk is smaller than the Ubuntu source')
        native_assets.unpack_kernel(imgs['kernel'], staging/'kernel', arch)
        go = toolchain(arch, cache, work)
        env = {**os.environ, 'GOOS':'linux','GOARCH':arch,'CGO_ENABLED':'0','GOWORK':'off',
               'GOBIN':str(work),'GOFLAGS':'-mod=readonly' if not os.environ.get('GOFLAGS') else os.environ['GOFLAGS']}
        print(f'Building the matching guest agent and c2j {C2J} for linux/{arch}', flush=True)
        subprocess.run([go,'build','-trimpath','-buildvcs=false','-o',work/'agent','./cmd/pulse-cloudhypervisor-guest'],cwd=root,env=env,check=True)
        module = json.loads(subprocess.check_output([go,'mod','download','-json','github.com/colony-2/c2j@'+C2J],cwd=work,env=env))
        if module.get('Sum') != 'h1:WxPX7qkSHjrKn3jIjSf+G6S2ZuGBdpovQirRACoakZc=':
            raise ValueError('guest c2j module checksum mismatch')
        subprocess.run([go,'install','-trimpath','-buildvcs=false','github.com/colony-2/c2j/cmd/c2j@'+C2J],cwd=work,env=env,check=True)
        agent_sha = native_assets.digest(work/'agent')
        native_setup.build(imgs['initramfs'], work/'agent', staging/'worker.initrd', imgs['initramfs'].name, agent_sha, arch)
        image = docker_image(arch,parts['alpine'],work/'image.tar')
        with tarfile.open(work/'tools.tar','w') as tar:
            for name, source in [('c2j',work/'c2j'),('image.tar',work/'image.tar'),('nix.tar.xz',parts['nix']),('docker.tgz',parts['docker'])]:
                tar.add(source,arcname=name,recursive=False)
        builder_initrd(staging/'worker.initrd',work/'tools.tar',staging/'builder.initrd')
        shutil.copyfile(root/'scripts/setup/prepare.sh',staging/'prepare.sh')
        prep = asset('prepare.sh')
        base = {'name':'ubuntu-24.04-'+arch,'architecture':arch,'disk':asset('ubuntu.raw'),'kernel':asset('kernel'),
                'initramfs':asset('worker.initrd'),'builder_initramfs':asset('builder.initrd'),'preparation':prep,
                'recipe':prep['sha256'],'docker_fixture_digest':image,'capabilities':['docker'],
                'tools':{'guest-agent':agent_sha,'c2j':native_assets.digest(work/'c2j'),
                         'nix':tool_hash(parts['nix'],lambda n:n.endswith('-nix-2.28.5/bin/nix')),
                         'docker':tool_hash(parts['docker'],lambda n:n=='docker/docker')}}
    return {'base':base,'hypervisor':asset('cloud-hypervisor')}
