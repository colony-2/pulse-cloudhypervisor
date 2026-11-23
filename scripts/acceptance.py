#!/usr/bin/env python3
"""Execute native tests on this architecture; cross-builds never qualify a run."""
import argparse
import os
import pathlib
import platform
import subprocess
import sys
import tempfile
from native_evidence import StreamAudit
from verification import (ROOT, IDENTITY, collect, contract, digest, read_json,
                          require, source_digest, split_mapping, validate_events,
                          validate_pins, use_installed_toolchain, write_json)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--arch', required=True, choices=['arm64'])
    parser.add_argument('--result')
    parser.add_argument('--candidate')
    args = parser.parse_args()
    arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
    require(arch == args.arch)
    os.chdir(ROOT)
    os.environ.update(GOWORK='off', GOTOOLCHAIN='local')
    use_installed_toolchain()
    source = source_digest()
    candidate = args.candidate or os.environ.get('CANDIDATE') or subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    require(IDENTITY.fullmatch(candidate))
    # No assignment/lease is accepted in argv, env, a manifest or evidence.
    pins = read_json(os.environ.get('CLOUDHYPERVISOR_PINS', ''))
    validate_pins(pins)
    require(subprocess.check_output(['go', 'env', 'GOVERSION'], text=True).strip() == pins['common']['go_version'])
    fixture = read_json(os.environ.get('CLOUDHYPERVISOR_FIXTURE', ''))
    require(isinstance(fixture, dict) and set(fixture) == {'version', 'identity', 'config', 'control_socket', 'bootstrap_agent', 'faults'})
    require(fixture['version'] == 1 and isinstance(fixture['identity'], str) and IDENTITY.fullmatch(fixture['identity']))
    config = read_json(fixture['config'])
    base = [b for b in config['bases'] if b['architecture'] == arch]
    require(len(base) == 1)
    base = base[0]
    selected = pins['architectures'][arch]
    assets = {'hypervisor': config['hypervisor'], 'disk': base['disk'], 'kernel': base['kernel'],
              'preparation': base['preparation'], 'bootstrap_agent': fixture['bootstrap_agent']}
    if selected['initramfs'] is not None:
        assets['initramfs'] = base['initramfs']
    else:
        require(not base.get('initramfs', {}).get('path'))
    for name, asset in assets.items():
        require(set(asset) == {'path', 'sha256'} and asset['sha256'] == selected[name] and digest(asset['path']) == selected[name])
    require(base['recipe'] == selected['preparation'] and base['docker_fixture_digest'] == selected['docker_image'])
    require(all(base['tools'][name] == value for name, value in selected['tools'].items()))
    _, live = split_mapping(contract())
    with tempfile.TemporaryDirectory(prefix='ch-native-') as scratch:
        scratch = pathlib.Path(scratch)
        for binary, package in [('guest', 'pulse-cloudhypervisor-guest'), ('provider', 'pulse-cloudhypervisor')]:
            require(subprocess.run(['env', 'CGO_ENABLED=0', 'GOOS=linux', 'GOARCH='+arch, 'go', 'build', '-trimpath', '-buildvcs=false', '-o', str(scratch/binary), './cmd/'+package], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0)
        agent = digest(scratch/'guest')
        require(base['tools']['guest-agent'] == agent)
        os.environ['CLOUDHYPERVISOR_PROVIDER_BINARY'] = str(scratch/'provider')
        audit = StreamAudit(live)
        binding = None
        if 'CLOUDHYPERVISOR_RUN_BINDING' in os.environ:
            from placement import confirmed
            current = confirmed()
            binding = read_json(os.environ['CLOUDHYPERVISOR_RUN_BINDING'])
            require(current and binding['job']==current.job and binding['task']==current.task and binding['acquisition']==current.event
                    and binding['worker']==current.worker and binding['boot']==current.boot and binding['candidate']==candidate
                    and binding['source']==source and binding['mapping']==digest(ROOT/'tests/expectations.json'))
        tests = collect(['go', 'test', '-json', '-count=1', '-tags=live', '-timeout=21m', './tests/live'], live, timeout=21*60+10, audit=audit)
        result = {'version': 1, 'architecture': arch, 'host_architecture': arch,
                  'candidate': candidate, 'source_sha256': source,
                  'mapping_sha256': digest(ROOT/'tests/expectations.json'),
                  'pins': pins, 'selected_assets': selected,
                  'fixture_identity': fixture['identity'], 'guest_agent_sha256': agent,
                  'tests': tests}
        require(source_digest() == source)
        if args.result:
            write_json(args.result, result)
            if binding:
                write_json(pathlib.Path(args.result).with_suffix('.audit'),
                           {'binding':binding,'sha256':digest(args.result),'audit':audit.result(tests['exit_code'])})
        validate_events(live, tests)
    print('PASS: every mandatory native assertion executed')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, TypeError, UnicodeError):
        print('FAIL: native architecture, fixture authority, fixed inputs or mandatory assertions unavailable')
        sys.exit(1)
