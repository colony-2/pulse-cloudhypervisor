"""Create job-local Ubuntu/KVM verification inputs on the qualified allocation."""
import contextlib
import http.server
import ipaddress
import json
import os
from pathlib import Path
import secrets
import signal
import shutil
import socket
import stat
import subprocess
import tempfile
import threading
import time

import guest_payload
import native_assets
import native_setup
from owned_units import Units, builder_phase
from verification import NATIVE_RUN_SECONDS, ROOT, digest, read_json, require, write_json

API_REVISION = 'v0.0.0-20261120061856-2919f1ed5e42'
ROOT_BYTES = 2432 * 1024**2
IMAGE_ID = 'sha256:31bcf98bb32499ed6b60e2388c821cde72e02f584686adab03f34e83ad1ecf07'


def asset(path):
    return {'path': str(path), 'sha256': digest(path)}


def quiet(args, timeout):
    subprocess.run(args, check=True, timeout=timeout, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


@contextlib.contextmanager
def network(identity, can_release=lambda: True):
    descriptor = read_json(os.environ.get('PULSE_KVM_NETWORK', '/etc/pulse-macvz/network.json'))
    boot = Path('/proc/sys/kernel/random/boot_id').read_text().strip()
    require(descriptor['version'] == 1 and descriptor['boot_id'] == boot)
    bridge = descriptor['bridge']
    require(bridge == 'pulsebr0' and Path('/sys/class/net',bridge,'ifalias').read_text().strip() in ('pulse-macvz:kvm:v1', 'pulse-cloudhypervisor:kvm:v1'))
    subnet = ipaddress.IPv4Network(descriptor['subnet'])
    gateway = ipaddress.IPv4Address(descriptor['gateway'])
    first = ipaddress.IPv4Address(descriptor['first_guest_address'])
    last = ipaddress.IPv4Address(descriptor['last_guest_address'])
    require(first in subnet and last in subnet and gateway in subnet and last > first and descriptor['dns']==[str(gateway)])
    slots = []
    with contextlib.ExitStack() as cleanup:
        for i in range(2):
            address = first+i
            require(address != gateway and address <= last)
            name = 'ch'+identity[-8:]+str(i)
            quiet(['ip','tuntap','add','dev',name,'mode','tap','vnet_hdr','user',str(os.getuid())], 5)
            index = socket.if_nametoindex(name)
            def release(name=name,index=index):
                if not can_release():
                    return
                require(socket.if_nametoindex(name)==index and Path('/proc/sys/kernel/random/boot_id').read_text().strip()==boot)
                quiet(['ip','tuntap','del','dev',name,'mode','tap'],5)
            cleanup.callback(release)
            quiet(['ip','link','set','dev',name,'master',bridge,'up'],5)
            slots.append({'tap':name,'address':str(address),'gateway':str(gateway),'netmask':str(subnet.netmask),'dns':str(gateway)})
        yield slots


@contextlib.contextmanager
def serve_payload(path, slots):
    peers = {s['address'] for s in slots}
    class Download(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass
        def do_GET(self):
            if self.path!='/tools.tgz' or self.client_address[0] not in peers:
                self.send_error(404)
                return
            self.connection.settimeout(30)
            with path.open('rb') as source:
                self.send_response(200)
                self.send_header('Content-Length',str(os.fstat(source.fileno()).st_size))
                self.end_headers()
                shutil.copyfileobj(source,self.wfile)
    with http.server.ThreadingHTTPServer((slots[0]['gateway'],0), Download) as server:
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            yield 'http://%s:%d/tools.tgz'%server.server_address
        finally:
            server.shutdown()
            thread.join()


def config_and_pins(dir, binaries, slots, url, identity, payload_sha, boot, profile, image_id):
    arch = native_assets.NATIVE_ARCH
    prep = (ROOT/'scripts/prepare-guest.sh').read_text()
    for key,value in {'PAYLOAD_URL':url,'PAYLOAD_DIGEST':payload_sha,'IMAGE':image_id,
                      'C2J':profile.c2j+'/bin/c2j','DOCKER':profile.docker+'/bin/docker'}.items():
        prep=prep.replace('@'+key+'@',value)
    recipe=dir/'prepare.sh'
    recipe.write_text(prep)
    disk,kernel=boot/'ubuntu.raw',boot/'Image'
    disk_bytes=disk.stat().st_size
    agent=asset(binaries/('guest-'+arch))
    rd=dir/'builder.initrd'
    native_setup.build(boot/'initramfs',binaries/('guest-'+arch),rd,native_assets.SOURCES_BY_ARCH[arch]['initramfs'][1],agent['sha256'],arch)
    vmm=shutil.which('cloud-hypervisor')
    if arch == 'amd64':
        # The full verifier runs on the VMM selected by the pinned nixpkgs package.
        approved = subprocess.check_output(['nix', '--extra-experimental-features', 'nix-command flakes', 'path-info',
            '--offline', 'github:NixOS/nixpkgs/680275f0bb6afce84fbac3e75fc47c5f2272400b#cloud-hypervisor'],
            text=True, timeout=30).strip()
        require(vmm and Path(vmm).resolve() == Path(approved)/'bin/cloud-hypervisor')
    else:
        require(vmm and Path(vmm).resolve()==Path('/nix/store/anm6kwkpalhqww10wfysypvqq0y96a2l-cloud-hypervisor-55.0/bin/cloud-hypervisor'))
        require(digest(vmm) == '1b467d1877e41448b2f3910cb6cf8488b625e1cfb5d5f473205535685bffeeef')
    tools={'c2j':profile.pins[profile.c2j+'/bin/c2j'],'docker':profile.pins[profile.docker+'/bin/docker'],'nix':profile.pins['/usr/bin/nix']}
    base={'name':'ubuntu-24.04-'+arch,'architecture':arch,'disk':asset(disk),'kernel':asset(kernel),'initramfs':asset(rd),
          'preparation':asset(recipe),'recipe':digest(recipe),'docker_fixture_digest':image_id,'tools':dict(tools,**{'guest-agent':agent['sha256']}),
          'capabilities':['docker'],'usable_cpu_millis':1000,'usable_memory_bytes':512<<20}
    c={'version':1,'owner':identity,'state_dir':str(dir/'s'),'count':2,'vcpus':1,'memory_bytes':1536<<20,'disk_bytes':disk_bytes,
       'host_cpu_millis':2000,'host_memory_bytes':3072<<20,'host_disk_bytes':disk_bytes*3,'hypervisor':asset(Path(vmm)),
       'runner':str(binaries/('provider-'+arch)),'systemd_run':shutil.which('systemd-run'),'systemctl':shutil.which('systemctl'),
       'user_manager':True,'network':slots,'bases':[base]}
    selected={k:base[k]['sha256'] for k in ('disk','kernel','initramfs','preparation')}
    selected.update(hypervisor=c['hypervisor']['sha256'],bootstrap_agent=agent['sha256'],tools=tools,docker_image=image_id)
    pins={'version':1,'common':{'provider_api':API_REVISION,'plugin_sdk':API_REVISION,'sdk_baseline':3,'c2j_revision':guest_payload.C2J_REVISION,
           'go_version':subprocess.check_output(['go','env','GOVERSION'],text=True).strip()},'architectures':{arch:selected}}
    settings = dir / 'config.yaml'
    subprocess.run(['go', 'run', './scripts/testdata/settings.go', 'write', settings],
                   input=json.dumps(c).encode(), check=True, cwd=ROOT, timeout=90)
    desc={'version':1,'identity':identity,'bootstrap_agent':agent,'config':str(settings),'control_socket':str(dir/'control'),
          'faults':['controller-stop','runner-kill']}
    for name, doc in [('pins',pins),('fixture',desc)]:
        write_json(dir/(name+'.json'),doc)
    return c


def health(sock,identity):
    with socket.socket(socket.AF_UNIX,socket.SOCK_STREAM) as conn:
        conn.settimeout(2)
        mode=os.lstat(sock)
        require(stat.S_ISSOCK(mode.st_mode) and mode.st_mode&0o777==0o600 and mode.st_uid==os.getuid())
        conn.connect(str(sock))
        import struct
        _,uid,_=struct.unpack('3i',conn.getsockopt(socket.SOL_SOCKET,socket.SO_PEERCRED,12))
        require(uid==os.getuid())
        conn.sendall(('GET /health HTTP/1.0\r\nHost: fixture\r\nX-Pulse-Fixture: '+identity+'\r\n\r\n').encode())
        import http.client
        reply=http.client.HTTPResponse(conn)
        reply.begin()
        require(reply.status==200 and reply.getheader('Cache-Control')=='no-store' and json.loads(reply.read(128))=={'ready':True})


def available_memory():
    return 1024 * int(next(line.split()[1] for line in Path('/proc/meminfo').read_text().splitlines()
                           if line.startswith('MemAvailable:')))


@contextlib.contextmanager
def prepared(binaries, confirm, deadline=None):
    """The full verifier owns its private directory, helper and TAPs."""
    dir=Path(tempfile.mkdtemp(prefix='chf-',dir='/tmp'))
    setup_limit = 25 * 60
    # Allow aggregation after the supervised run, then reap units and the helper.
    native_limit = NATIVE_RUN_SECONDS + 2 * 60
    cleanup_limit = 4 * 60
    overall = min(deadline or float("inf"), time.monotonic() + setup_limit + native_limit + cleanup_limit)
    identity='ch-'+secrets.token_hex(8)
    safe_to_remove=True
    boot_tmp = None
    ledger=None
    previous = signal.getsignal(signal.SIGALRM)
    def expired(_signal, _frame):
        raise TimeoutError('fixture phase deadline')
    def arm(limit, reserve):
        left = min(limit, overall - time.monotonic() - reserve)
        require(left > 0)
        signal.setitimer(signal.ITIMER_REAL, left)
    signal.signal(signal.SIGALRM, expired)
    try:
        arm(setup_limit, cleanup_limit)
        print('Building the isolated JobDB helper', flush=True)
        quiet(['go','-C',str(ROOT/'tests/fixture'),'build','-o',str(dir/'helper'),'.'],180)
        fault_runner = dir/'ack-loss-runner'
        quiet(['go','test','-c','-trimpath','-tags=livefault','-o',str(fault_runner),'./internal/runner'],120)
        require(confirm())
        boot_parent = dir
        # The fixed source and two job disks fit independently on workers with
        # adequate RAM-backed staging or sufficient filesystem scratch.
        shm = Path('/dev/shm')
        separate_shm = shm.is_dir() and shm.stat().st_dev != dir.stat().st_dev
        if separate_shm and available_memory() >= 6 * 1024**3 and shutil.disk_usage(shm).free >= 3 * 1024**3:
            boot_tmp = Path(tempfile.mkdtemp(prefix='ch-input-', dir=shm))
            boot_parent = boot_tmp
        boot = boot_parent/'boot'
        native_assets.stage(boot, root_bytes=ROOT_BYTES, arch=native_assets.NATIVE_ARCH)
        for name in ('archive','kernel'):
            (boot/name).unlink()
        print('Assembling the private guest tool payload', flush=True)
        profile = guest_payload.profile(native_assets.NATIVE_ARCH)
        payload,image_id=guest_payload.build(dir, profile)
        if native_assets.NATIVE_ARCH == "arm64":
            require(image_id==IMAGE_ID)
        # Only the builder downloads this archive. Keep 3 GiB for its 1.5 GiB
        # VM and the host. The archive is unlinked before the two-job tests.
        payload_size = payload.stat().st_size
        if (separate_shm and shutil.disk_usage(shm).free >= payload_size and
                available_memory() >= 3 * 1024**3 + payload_size):
            if boot_tmp is None:
                boot_tmp = Path(tempfile.mkdtemp(prefix='ch-input-', dir=shm))
            payload = Path(shutil.move(payload, boot_tmp / payload.name))
            print(f'Staged {payload_size} guest tool payload bytes on {shm}', flush=True)
        payload_sha=native_assets.digest(payload)
        print('PASS: pinned guest tools and Docker image', flush=True)
        require(confirm())
        with network(identity, lambda: safe_to_remove) as slots, contextlib.ExitStack() as finish:
            print('PASS: two owned TAPs reserved on the allocation bridge', flush=True)
            # This ledger lives outside the workspace used for builds and guests.
            fd, path = tempfile.mkstemp(prefix='ch-units-',dir='/tmp')
            os.close(fd)
            ledger=Path(path)
            helper=None
            def reap():
                nonlocal safe_to_remove
                arm(cleanup_limit, 0)
                units.close()
                if helper is not None:
                    helper.terminate()
                    try:
                        helper.wait(5)
                    except subprocess.TimeoutExpired:
                        helper.kill()
                        helper.wait(5)
                safe_to_remove=True

            with serve_payload(payload,slots) as url:
                config=config_and_pins(dir,binaries,slots,url,identity,payload_sha,boot,profile,image_id)
                state=Path(config['state_dir'])
                state.mkdir(mode=0o700)
                free = shutil.disk_usage(state).free
                needed = 3 * config['disk_bytes']
                print(f'Builder disk capacity: available={free} required={needed} bytes', flush=True)
                require(os.cpu_count() >= 2 and free >= needed)
                require(confirm())
                units=Units(config,ledger, fault_runner)
                units.thread.start()
                safe_to_remove=False
                finish.callback(reap)
                print('Starting the supervised guest builder', flush=True)
                try:
                    quiet([config['runner'],'setup','--config',str(dir/'config.yaml')],900)
                except (subprocess.CalledProcessError, subprocess.TimeoutExpired):
                    phase = builder_phase(state, identity, units.boot)
                    print(f'FAIL: Ubuntu source preparation stopped (builder phase {phase})', flush=True)
                    raise
                print('PASS: stopped builder promoted a qualified native Ubuntu source',flush=True)
            payload.unlink()
            require(confirm())
            arm(native_limit, cleanup_limit)
            helper=subprocess.Popen([str(dir/'helper'),'--control-socket',str(dir/'control'),'--identity',identity,
                '--listen',slots[0]['gateway']+':0','--endpoint','http://'+slots[0]['gateway']+':0','--lease-duration','45s',
                '--docker-image',image_id,'--tool-pins',str(dir/'pins.json'),'--guest-addresses',','.join(s['address'] for s in slots)],
                env={'PATH':os.environ['PATH'],'HOME':str(dir),'TMPDIR':str(dir)},stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
            end=time.monotonic()+40
            while True:
                require(helper.poll() is None and time.monotonic()<end)
                try:
                    health(dir/'control',identity)
                    break
                except (FileNotFoundError, ConnectionRefusedError):
                    time.sleep(.2)
            print('PASS: authenticated private JobDB fixture',flush=True)
            yield dir/'fixture.json', dir/'pins.json'
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)
        if safe_to_remove:
            shutil.rmtree(dir)
            if boot_tmp is not None:
                shutil.rmtree(boot_tmp)
            if ledger is not None:
                ledger.unlink(missing_ok=True)
        else:
            print('FAIL: owned state retained for reconciliation',flush=True)
