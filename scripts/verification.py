#!/usr/bin/env python3
"""Nonsecret, bounded verification inputs and events. Never persist child output."""
import hashlib
import json
import os
import pathlib
import re
import signal
import shutil
import stat
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]
ARCHES = {'arm64'}
DIGEST = re.compile(r'[0-9a-f]{64}\Z')
IDENTITY = re.compile(r'[-A-Za-z0-9._:/@]{1,200}\Z')
MAX_BYTES = 16 * 1024 * 1024
MAX_EVENTS = 100000


def use_installed_toolchain():
    # These versions were inspected in the allocated worker. Discover existing
    # executables only; never invoke Nix, download tools or change worker config.
    for name, pattern in [('go', '*-go-1.26.8/bin/go'), ('gcc', '*-gcc-wrapper-15.3.0/bin/gcc')]:
        if shutil.which(name):
            continue
        for path in sorted(pathlib.Path('/nix/store').glob(pattern)):
            if path.is_file() and os.access(path, os.X_OK):
                os.environ['PATH'] = str(path.parent) + os.pathsep + os.environ.get('PATH', '')
                break


def require(value):
    if not value:
        raise ValueError('invalid verification input')


def unique_pairs(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result)
        result[key] = value
    return result


def decode(data):
    try:
        return json.loads(data, object_pairs_hook=unique_pairs)
    except RecursionError:
        raise ValueError('verification JSON nesting limit') from None


def read_json(path):
    with open(path, 'rb') as f:
        data = f.read(MAX_BYTES + 1)
    require(len(data) <= MAX_BYTES)
    return decode(data)


def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def source_digest(root=ROOT):
    """Bind dirty/new/deleted files, modes and link targets without following links."""
    paths = subprocess.check_output(['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z'], cwd=root).split(b'\0')
    h = hashlib.sha256()
    def field(value):
        h.update(len(value).to_bytes(8, 'big'))
        h.update(value)
    for raw in sorted(set(paths) - {b''}):
        require(not raw.startswith(b'/') and b'..' not in raw.split(b'/') and b'.git' not in raw.split(b'/'))
        path = os.path.join(os.fsencode(root), raw)
        field(raw)
        blocked = False
        parent = os.fsencode(root)
        for component in raw.split(b'/')[:-1]:
            parent = os.path.join(parent, component)
            if os.path.islink(parent):
                field(b'symlink-parent')
                field(os.readlink(parent))
                blocked = True
                break
        if blocked:
            continue
        try:
            info = os.lstat(path)
        except FileNotFoundError:
            field(b'missing')
            continue
        field(str(stat.S_IMODE(info.st_mode)).encode())
        if stat.S_ISLNK(info.st_mode):
            field(b'symlink')
            field(os.readlink(path))
        else:
            require(stat.S_ISREG(info.st_mode))
            field(b'file')
            # Hash streams to avoid retaining images/binaries in memory.
            field(bytes.fromhex(digest(path)))
    return h.hexdigest()


def contract(root=ROOT):
    ids = re.findall(r'^\| ([A-Z]+-\d+(?:\.a)?) \|', (root/'docs/cloudhypervisor-tests.md').read_text(), re.M)
    require(len(ids) == 89 and len(set(ids)) == 89)
    mapping = read_json(root/'tests/expectations.json')
    require(isinstance(mapping, dict) and set(mapping) == set(ids))
    for tests in mapping.values():
        require(isinstance(tests, list))
        for pair in tests:
            require(isinstance(pair, list) and len(pair) == 2 and all(isinstance(s, str) and s and len(s) < 300 and not any(ord(c) < 32 for c in s) for s in pair))
    prereqs = {'TestNativeHost', 'TestNativeAllocationAccess', 'TestNativePreflight', 'TestNativeRebootPreflight'}
    require(all(test.split('/')[0] not in prereqs for key, tests in mapping.items()
                if key.startswith('LIVE-') for _, test in tests))
    return mapping


def split_mapping(mapping):
    live = {k: v for k, v in mapping.items() if k.startswith('LIVE-')}
    return {k: v for k, v in mapping.items() if k not in live}, live


def known_tests(mapping):
    tests = {tuple(p) for pairs in mapping.values() for p in pairs}
    # Parent events must also be accounted for when a mapped assertion is a subtest.
    return tests | {(p, '/'.join(t.split('/')[:n])) for p, t in tests for n in range(1, len(t.split('/')))}


class Events:
    """Keep only mapped identifiers and safe actions, discarding all Output fields."""
    def __init__(self, mapping):
        self.known = known_tests(mapping)
        self.packages = {p for p, _ in self.known}
        self.events = []
        self.invalid = False
        self.count = 0

    def feed(self, data):
        self.count += 1
        if self.count > MAX_EVENTS or len(data) > 1024 * 1024:
            self.invalid = True
            return
        try:
            event = decode(data)
            require(isinstance(event, dict))
            action = event.get('Action')
            require(action in {'start', 'run', 'pause', 'cont', 'pass', 'fail', 'skip', 'output', 'bench'})
            package, test = event.get('Package'), event.get('Test')
            require(isinstance(package, str) and (test is None or isinstance(test, str)))
            # Go emits a package-level skip for commands with no test files.
            # Such packages cannot cover an assertion. Every skipped test and
            # every skip in a mapped package still latches failure.
            if action == 'fail' or action == 'skip' and (test or package in self.packages):
                self.invalid = True
            if action in {'output', 'bench'}:
                return
            if package not in self.packages or (test and (package, test) not in self.known):
                return
            safe = {'Action': action, 'Package': package}
            if test:
                safe['Test'] = test
            self.events.append(safe)
        except (ValueError, TypeError, UnicodeError):
            self.invalid = True

    def result(self, exit_code):
        return {'version': 1, 'exit_code': exit_code, 'valid': not self.invalid, 'events': self.events}


def collect(command, mapping, timeout=900):
    """Consume stdout in memory; stderr never reaches disk or diagnostics."""
    events = Events(mapping)
    with subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True) as child:
        # A timer kills only this owned child session, even if stdout stays open.
        import threading
        expired = threading.Event()
        def kill():
            expired.set()
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        timer = threading.Timer(timeout, kill)
        timer.start()
        try:
            while True:
                line = child.stdout.readline(1024 * 1024 + 1)
                if not line:
                    break
                events.feed(line)
            code = child.wait()
        finally:
            timer.cancel()
        if expired.is_set():
            events.invalid = True
    return events.result(code)


def write_json(path, data):
    # Evidence must be outside source inputs, with private permissions.
    path = pathlib.Path(path).absolute()
    require(not path.resolve().is_relative_to(ROOT.resolve()))
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as f:
        json.dump(data, f, sort_keys=True, separators=(',', ':'))
        f.write('\n')


def validate_events(mapping, receipt):
    require(isinstance(receipt, dict) and set(receipt) == {'version', 'exit_code', 'valid', 'events'})
    require(type(receipt['version']) is int and receipt['version'] == 1 and type(receipt['exit_code']) is int and receipt['exit_code'] == 0 and receipt['valid'] is True)
    events = receipt['events']
    require(isinstance(events, list) and 0 < len(events) <= MAX_EVENTS)
    known = known_tests(mapping)
    packages = {p for p, _ in known}
    started, completed, running, passed = set(), set(), set(), set()
    for event in events:
        require(isinstance(event, dict) and set(event) in ({'Action', 'Package'}, {'Action', 'Package', 'Test'}))
        package, test, action = event.get('Package'), event.get('Test'), event.get('Action')
        require(package in packages and action in {'start', 'run', 'pause', 'cont', 'pass'})
        if test:
            key = (package, test)
            require(key in known and package in started and package not in completed)
            if action == 'run':
                require(key not in running and key not in passed)
                running.add(key)
            elif action == 'pass':
                require(key in running and key not in passed)
                passed.add(key)
            else:
                require(action in {'pause', 'cont'} and key in running and key not in passed)
        elif action == 'start':
            require(package not in started)
            started.add(package)
        else:
            require(action == 'pass' and package in started and package not in completed)
            require(all(p != package or (p, t) in passed for p, t in running))
            completed.add(package)
    require(started == completed == packages and running == passed)
    require(mapping and all(pairs and all(tuple(pair) in passed for pair in pairs) for pairs in mapping.values()))


def validate_local(mapping, deterministic_receipt, consumer_receipt):
    # Check coverage before removing empty partitions: an unmapped ID must not
    # disappear. A single SDK ID may require assertions in both packages.
    require(mapping and all(mapping.values()))
    consumer = {k: [p for p in v if p[0] == 'example.com/cloudhypervisor-consumer']
                for k, v in mapping.items()}
    deterministic = {k: [p for p in v if p[0] != 'example.com/cloudhypervisor-consumer']
                     for k, v in mapping.items()}
    validate_events({k: v for k, v in deterministic.items() if v}, deterministic_receipt)
    validate_events({k: v for k, v in consumer.items() if v}, consumer_receipt)


def validate_pins(pins):
    require(isinstance(pins, dict) and set(pins) == {'version', 'common', 'architectures'} and type(pins['version']) is int and pins['version'] == 1)
    common = pins['common']
    require(isinstance(common, dict) and set(common) == {'provider_api', 'plugin_sdk', 'sdk_baseline', 'c2j_revision', 'go_version'})
    require(common['provider_api'] == common['plugin_sdk'] == 'v0.0.0-20261120061856-2919f1ed5e42' and type(common['sdk_baseline']) is int and common['sdk_baseline'] == 3)
    require(isinstance(common['c2j_revision'], str) and re.fullmatch('[0-9a-f]{40}', common['c2j_revision']))
    require(isinstance(common['go_version'], str) and re.fullmatch(r'go1\.[0-9]+\.[0-9]+', common['go_version']))
    architectures = pins['architectures']
    require(isinstance(architectures, dict) and set(architectures) == ARCHES)
    for a in architectures.values():
        require(isinstance(a, dict) and set(a) == {'hypervisor', 'disk', 'kernel', 'initramfs', 'preparation', 'bootstrap_agent', 'tools', 'docker_image'})
        for k in ('hypervisor', 'disk', 'kernel', 'preparation', 'bootstrap_agent'):
            require(isinstance(a[k], str) and DIGEST.fullmatch(a[k]))
        require(a['initramfs'] is None or isinstance(a['initramfs'], str) and DIGEST.fullmatch(a['initramfs']))
        require(isinstance(a['tools'], dict) and set(a['tools']) == {'c2j', 'nix', 'docker'})
        require(all(isinstance(v, str) and DIGEST.fullmatch(v) for v in a['tools'].values()))
        require(isinstance(a['docker_image'], str) and a['docker_image'].startswith('sha256:') and DIGEST.fullmatch(a['docker_image'][7:]))


def validate_native(results, candidate, source, mapping_sha256, pins, required, agents):
    validate_pins(pins)
    require(isinstance(candidate, str) and IDENTITY.fullmatch(candidate) and DIGEST.fullmatch(source) and DIGEST.fullmatch(mapping_sha256))
    require(isinstance(results, list) and len(results) == len(ARCHES) and all(isinstance(r, dict) for r in results))
    require({r.get('architecture') for r in results} == ARCHES and set(agents) == ARCHES)
    for result in results:
        require(isinstance(result, dict) and set(result) == {'version', 'architecture', 'host_architecture', 'candidate', 'source_sha256', 'mapping_sha256', 'pins', 'selected_assets', 'fixture_identity', 'guest_agent_sha256', 'tests'})
        arch = result['architecture']
        require(type(result['version']) is int and result['version'] == 1 and result['host_architecture'] == arch)
        require(result['candidate'] == candidate and result['source_sha256'] == source and result['mapping_sha256'] == mapping_sha256 and result['pins'] == pins)
        require(result['selected_assets'] == pins['architectures'][arch] and result['guest_agent_sha256'] == agents[arch] and DIGEST.fullmatch(agents[arch]))
        require(isinstance(result['fixture_identity'], str) and IDENTITY.fullmatch(result['fixture_identity']))
        validate_events(required, result['tests'])
