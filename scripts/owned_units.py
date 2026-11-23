"""Track supervised test units outside the fixture and release exact matches."""
from dataclasses import astuple, dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import threading

from verification import require

BOOT_ID = Path('/proc/sys/kernel/random/boot_id')


def key(text):
    return hashlib.sha256(text.encode()).hexdigest()[:24]


@dataclass(frozen=True)
class Unit:
    name: str
    identity: str
    directory: str
    digest: str


def owned_spec(path, state, owner, boot):
    rel = path.relative_to(state)
    require(len(rel.parts) == 3 and rel.name == 'record.json')
    for folder in [state, path.parent.parent, path.parent]:
        mode = folder.lstat()
        require(stat.S_ISDIR(mode.st_mode) and mode.st_uid == os.getuid() and mode.st_mode & 0o077 == 0)
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), 'rb') as stream:
        mode = os.fstat(stream.fileno())
        require(stat.S_ISREG(mode.st_mode) and mode.st_uid == os.getuid() and
                stat.S_IMODE(mode.st_mode) == 0o600 and mode.st_nlink == 1)
        data = stream.read(65537)
    require(len(data) <= 65536)
    spec = json.loads(data)
    ident = spec['identity']
    launch = spec['launch_id']
    # runner.BootID preserves the newline from procfs in the durable spec.
    require(spec['version'] == 1 and spec['owner'] == owner and
            spec['boot_id'] in (boot, boot + '\n') and re.fullmatch('[0-9a-f]{32}', ident))
    if rel.parts[0] == 'jobs':
        require(re.fullmatch(r'[A-Za-z0-9_.-]{1,128}', launch) and rel.parts[1] == key(launch))
        name = 'pulse-ch-' + key(str(state)) + '-' + key(launch) + '.service'
    else:
        require(rel.parts[0] == 'sources' and re.fullmatch('build-[0-9a-f]{8}', rel.parts[1])
                and launch == 'prepare')
        name = 'pulse-ch-build-' + key(str(path.parent)) + '.service'
    require(spec['unit'] == name)
    return Unit(name, ident, str(path.parent), hashlib.sha256(data.rstrip(b'\n')).hexdigest())


def start_time(pid):
    try:
        fields = Path('/proc', str(pid), 'stat').read_text().rsplit(')', 1)[1].split()
    except FileNotFoundError:
        return None
    require(len(fields) >= 20 and fields[19].isdigit() and int(fields[19]) > 0)
    return fields[19]


def empty_group(group):
    if not group:
        return True
    path = Path('/sys/fs/cgroup' + group)
    if not path.exists():
        return True
    return 'populated 0' in (path / 'cgroup.events').read_text().splitlines()


class Units:
    def __init__(self, config, ledger):
        self.state = Path(config['state_dir'])
        self.owner = config['owner']
        self.boot = BOOT_ID.read_text().strip()
        self.runner = config['runner']
        self.hypervisor = config['hypervisor']['path']
        self.systemctl = os.path.realpath(config['systemctl'])
        self.busctl = os.path.realpath(Path(config['systemctl']).parent/'busctl')
        self.ledger = ledger
        self.seen = {}
        self.samples = {}
        self.vm_samples = {}
        self.error = False
        self.stop_event = threading.Event()
        self.thread = threading.Thread(target=self.watch)

    def record(self, entry):
        with self.ledger.open('a') as out:
            out.write(json.dumps(entry) + '\n')
            out.flush()
            os.fsync(out.fileno())

    def scan(self):
        for pattern in ('jobs/*/record.json', 'sources/build-*/record.json'):
            for path in self.state.glob(pattern):
                try:
                    unit = owned_spec(path, self.state, self.owner, self.boot)
                except FileNotFoundError:
                    continue
                previous = self.seen.get(unit.name)
                require(previous is None or previous == unit)
                if previous is None:
                    self.seen[unit.name] = unit
                    self.record((self.boot, self.owner, *astuple(unit)))
                fields = self.show(unit.name)
                if fields and fields.get('MainPID', '0') != '0':
                    self.sample(unit, fields)

    def watch(self):
        try:
            while not self.stop_event.wait(.2):
                self.scan()
        except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
            self.error = True

    def close(self):
        self.stop_event.set()
        self.thread.join()
        require(BOOT_ID.read_text().strip() == self.boot)
        try:
            self.scan()
        except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
            self.error = True
        for unit in self.seen.values():
            try:
                self.release(unit)
            except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError):
                self.error = True
        require(not self.error)
        print('PASS: independently supervised test units reconciled', flush=True)

    def show(self, unit):
        result = subprocess.run(
            ['systemctl', '--user', '--no-ask-password', 'show', unit,
             '--property=LoadState,Description,InvocationID,ActiveState,ControlGroup,MainPID,ExecStart'],
            capture_output=True, text=True, timeout=4)
        fields = dict(line.split('=', 1) for line in result.stdout.splitlines() if '=' in line)
        if fields.get('LoadState') == 'not-found':
            return None
        require(result.returncode == 0 and fields.get('LoadState') == 'loaded')
        return fields

    def observe_members(self, unit, group, main_pid):
        if not group:
            return
        processes = Path('/sys/fs/cgroup' + group, 'cgroup.procs')
        try:
            members = processes.read_text().splitlines()
        except FileNotFoundError:
            require(empty_group(group))
            return
        for entry in members:
            pid = int(entry)
            if pid == main_pid:
                continue
            proc = Path('/proc', str(pid))
            try:
                started = start_time(pid)
                if started is None:
                    continue
                exe = os.readlink(proc/'exe')
                argv = (proc/'cmdline').read_bytes().split(b'\0')[:-1]
                require(proc.stat().st_uid == os.getuid() and
                        (proc/'cgroup').read_text().splitlines() == ['0::' + group])
                if exe == self.systemctl and unit.name.encode() in argv and (b'show' in argv or b'set-property' in argv):
                    continue
                if exe == self.busctl and b'get-property' in argv and b'RuntimeMaxUSec' in argv:
                    continue
                require(exe == self.hypervisor and b'--api-socket' in argv)
                index = argv.index(b'--api-socket') + 1
                require(index < len(argv) and argv[index] == os.fsencode(Path(unit.directory, 'api')))
                require(start_time(pid) == started)
                sample = (group, pid, started)
                previous = self.vm_samples.get(unit.name)
                require(previous is None or previous == sample)
                if previous is None:
                    self.vm_samples[unit.name] = sample
                    self.record((self.boot, unit.name, 'vmm', *sample))
            except FileNotFoundError:
                require(start_time(pid) is None)

    def sample(self, unit, fields):
        require(fields['Description'] == 'pulse:' + unit.identity)
        role = 'prepare-runner' if unit.name.startswith('pulse-ch-build-') else 'runner'
        argv = [self.runner, role, '--dir', unit.directory, '--digest', unit.digest]
        require('argv[]=' + ' '.join(argv) + ' ;' in fields['ExecStart'])
        group = fields['ControlGroup']
        pid = int(fields['MainPID'])
        if group:
            require(group.startswith('/') and Path(group).name == unit.name and '..' not in Path(group).parts)
        self.observe_members(unit, group, pid)
        if pid:
            require(pid > 1 and group and re.fullmatch('[0-9a-f]{32}', fields['InvocationID']))
            proc = Path('/proc', str(pid))
            started = start_time(pid)
            if started is None:
                # The runner can exit between the manager response and procfs.
                return
            try:
                require(proc.stat().st_uid == os.getuid() and
                        (proc/'cmdline').read_bytes().split(b'\0')[:-1] == [s.encode() for s in argv])
                require((proc/'cgroup').read_text().splitlines() == ['0::' + group])
            except FileNotFoundError:
                require(start_time(pid) is None)
                return
            sample = (fields['InvocationID'], group, pid, started)
            previous = self.samples.get(unit.name)
            require(previous is None or previous == sample)
            if previous is None:
                self.samples[unit.name] = sample
                self.record((self.boot, unit.name, *sample))

    def release(self, unit):
        require(BOOT_ID.read_text().strip() == self.boot)
        fields = self.show(unit.name)
        sample = self.samples.get(unit.name)
        vmm = self.vm_samples.get(unit.name)
        if fields is None:
            if vmm:
                require(start_time(vmm[1]) != vmm[2] and empty_group(vmm[0]))
            if sample:
                _, group, pid, started = sample
                require(start_time(pid) != started and empty_group(group))
            # A record left by the provider still needs reconciliation. Only its
            # removal plus manager/process absence proves a completed release.
            require(not Path(unit.directory, 'record.json').exists())
            return
        self.sample(unit, fields)
        sample = self.samples.get(unit.name)
        if sample:
            require(fields['InvocationID'] == sample[0])
        require(self.show(unit.name) == fields)
        subprocess.run(['systemctl', '--user', '--no-ask-password', 'stop', unit.name],
                       check=True, timeout=12, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        after = self.show(unit.name)
        require(after is None or (after['Description'] == 'pulse:' + unit.identity
                and after['ActiveState'] == 'inactive' and after['MainPID'] == '0'))
        require(empty_group(fields['ControlGroup']))
        vmm = self.vm_samples.get(unit.name)
        if vmm:
            require(start_time(vmm[1]) != vmm[2] and empty_group(vmm[0]))
        if sample:
            require(start_time(sample[2]) != sample[3] and empty_group(sample[1]))
