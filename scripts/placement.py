"""Require the runtime task and JobDB acquisition to identify this native worker."""
import datetime
import os
import pathlib
import platform
import re
import selectors
import stat
import subprocess
import time
from dataclasses import dataclass

from verification import ROOT, decode, digest, require, write_json

BOOT_ID = pathlib.Path('/proc/sys/kernel/random/boot_id')
MAX_REPLY = 256 * 1024
QUERY_SECONDS = 10
PAGE_SIZE = 50
MAX_PAGES = 20
IDENTIFIER = re.compile(r'[A-Za-z0-9._:-]{1,200}\Z')
TASK = re.compile(r'[ -~]{1,4096}\Z')


def identifier(value):
    require(isinstance(value, str) and IDENTIFIER.fullmatch(value))
    return value


def runtime_json(*args):
    deadline = time.monotonic() + QUERY_SECONDS
    reply = bytearray()
    with subprocess.Popen(['c2j', *args], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL) as child:
        try:
            with selectors.DefaultSelector() as ready:
                ready.register(child.stdout, selectors.EVENT_READ)
                while True:
                    remaining = deadline - time.monotonic()
                    require(remaining > 0 and ready.select(remaining))
                    block = os.read(child.stdout.fileno(), min(65536, MAX_REPLY + 1 - len(reply)))
                    if not block:
                        break
                    reply.extend(block)
                    require(len(reply) <= MAX_REPLY)
            require(child.wait(timeout=max(0, deadline - time.monotonic())) == 0)
        finally:
            if child.poll() is None:
                child.kill()
    require(reply)
    value = decode(reply)
    require(isinstance(value, dict))
    return value


def acquisition(tenant, job, lease, worker):
    token, visited, matches = '', set(), []
    for _ in range(MAX_PAGES):
        args = ['job', 'events', 'self', '--event-type', 'lease_acquired', '--page-size', str(PAGE_SIZE), '--json']
        if token:
            args.extend(['--page-token', token])
        page = runtime_json(*args)
        events = page['events']
        require(isinstance(events, list) and len(events) <= PAGE_SIZE)
        for event in events:
            require(isinstance(event, dict) and event.get('eventType') == 'lease_acquired')
            key = event.get('jobKey')
            require(isinstance(key, dict) and key.get('tenantId') == tenant and key.get('jobId') == job)
            if identifier(event.get('leaseId')) == lease:
                require(identifier(event.get('workerId')) == worker)
                matches.append(identifier(event.get('eventId')))
                require(len(matches) == 1)
        token = page.get('nextPageToken', '')
        require(isinstance(token, str) and len(token) <= 4096 and not any(ord(c) < 32 for c in token))
        if not token:
            require(len(matches) == 1)
            return matches[0]
        require(token not in visited)
        visited.add(token)
    raise ValueError('acquisition pagination limit')


@dataclass(frozen=True)
class Placement:
    job: str
    tenant: str
    task: str
    lease: str
    event: str
    worker: str
    boot: str
    base: str

    def summary(self):
        return f'job={self.job} task={self.task} acquisition={self.event} worker={self.worker} boot={self.boot} base={self.base}'


def read_placement():
    require(platform.system() == 'Linux' and platform.machine() == 'aarch64')
    tenant = identifier(os.environ['C2J_CURRENT_TENANT_ID'])
    job = identifier(os.environ['C2J_CURRENT_JOB_ID'])
    worker = identifier(os.environ['PULSE_LAUNCH_ID'])
    raw = os.environ['C2J_EXECUTION_ALLOCATION']
    require(len(raw) <= MAX_REPLY)
    allocation = decode(raw)
    require(isinstance(allocation, dict) and type(allocation.get('schema_version')) is int and allocation['schema_version'] == 2)
    base, caps = allocation.get('base'), allocation.get('capabilities')
    arch = {'aarch64':'arm64', 'x86_64':'amd64'}.get(platform.machine())
    require(arch and isinstance(base, str) and re.fullmatch(r'ubuntu[A-Za-z0-9.-]*-'+arch, base))
    require(allocation.get('platform') == 'linux/'+arch and isinstance(caps, list) and 0 < len(caps) <= 128)
    require(all(isinstance(cap, str) and IDENTIFIER.fullmatch(cap) for cap in caps) and caps.count('kvm') == 1)
    state = runtime_json('self', '--json')
    require(type(state.get('schema_version')) is int and state['schema_version'] == 1)
    execution, lease = state['execution'], state['lease']
    require(isinstance(execution, dict) and isinstance(lease, dict))
    require(execution.get('source') == 'runtime_helper' and execution.get('status') == 'present')
    require(execution.get('tenant_id') == tenant and execution.get('job_id') == job)
    require(execution.get('op_type') == 'command_execution' and execution.get('node_id') in ('check', 'check_'+arch))
    task = execution.get('task_id')
    require(isinstance(task, str) and TASK.fullmatch(task))
    require(lease.get('status') == 'active')
    lease_id = identifier(lease.get('lease_id'))
    expires = lease.get('expires_at')
    require(isinstance(expires, str) and 0 < len(expires) < 64)
    expires = datetime.datetime.fromisoformat(expires)
    require(expires.tzinfo is not None and expires > datetime.datetime.now(datetime.timezone.utc))
    event = acquisition(tenant, job, lease_id, worker)
    boot = BOOT_ID.read_text().strip()
    require(re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}', boot))
    return Placement(job, tenant, task, lease_id, event, worker, boot, base)


def confirmed(expected=None):
    try:
        actual = read_placement()
        require(expected is None or expected == actual)
    except (ValueError, KeyError, OSError, TypeError, UnicodeError, subprocess.SubprocessError):
        print('FAIL: managed Ubuntu native KVM task, acquisition or boot could not be verified', flush=True)
        return None
    if expected is None:
        print('PASS: managed Ubuntu native KVM placement '+actual.summary(), flush=True)
        capabilities = {}
        try:
            for line in pathlib.Path('/proc/self/status').read_text().splitlines():
                field, _, value = line.partition(':')
                if field in ('CapEff', 'CapAmb') and re.fullmatch(r'[0-9a-f]{16}', value.strip()):
                    capabilities[field] = value.strip()
        except (OSError, UnicodeError):
            pass
        print(f'Execution identity: uid={os.geteuid()} gid={os.getegid()} '
              f'CapEff={capabilities.get("CapEff", "unavailable")} CapAmb={capabilities.get("CapAmb", "unavailable")}', flush=True)
    return actual


def binding_fields(placement):
    return {field: getattr(placement, field) for field in ('job', 'tenant', 'task', 'event', 'worker', 'boot', 'base')}


def native_identity(placement, candidate, source):
    return {'placement': binding_fields(placement), 'candidate': candidate,
            'source_sha256': source, 'mapping_sha256': digest(ROOT/'tests/expectations.json')}


def record_native_run(path, placement, candidate, source, run):
    write_json(path, {'version': 1, 'run': run, 'identity': native_identity(placement, candidate, source)})


def validate_native_run(path, candidate, source, run):
    require(isinstance(run, str) and len(run) == 32 and all(char in '0123456789abcdef' for char in run))
    path = pathlib.Path(path)
    require(path.is_absolute())
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK), 'rb') as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and info.st_uid == os.geteuid())
        data = stream.read(8193)
    require(len(data) <= 8192)
    evidence = decode(data)
    require(isinstance(evidence, dict) and set(evidence) == {'version', 'run', 'identity'})
    require(type(evidence['version']) is int and evidence['version'] == 1 and evidence['run'] == run)
    expected = native_identity(read_placement(), candidate, source)
    require(expected == evidence['identity'])
    return expected
