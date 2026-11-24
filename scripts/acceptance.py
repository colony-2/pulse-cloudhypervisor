#!/usr/bin/env python3
"""Execute native tests on this architecture; cross-builds never qualify a run."""
import argparse
import os
import pathlib
import platform
import subprocess
import sys
import tempfile
import time
import native_outcomes
from placement import validate_native_run
from verification import (ROOT, IDENTITY, NATIVE_COLLECTION_SECONDS, NATIVE_SUITE_MINUTES,
                          collect, contract, digest, read_json,
                          require, source_digest, split_mapping, validate_events,
                          validate_pins, use_installed_toolchain, write_json)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--arch', required=True, choices=['arm64','amd64'])
    parser.add_argument('--result')
    parser.add_argument('--candidate')
    parser.add_argument('--run-binding')
    parser.add_argument('--run-id')
    args = parser.parse_args()
    arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
    require(arch == args.arch)
    os.chdir(ROOT)
    os.environ.update(GOWORK='off', GOTOOLCHAIN='local')
    use_installed_toolchain()
    source = source_digest()
    candidate = args.candidate or os.environ.get('CANDIDATE') or subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    require(IDENTITY.fullmatch(candidate))
    binding = None
    if args.run_binding or args.run_id:
        require(args.run_binding and args.run_id)
        binding = validate_native_run(args.run_binding, candidate, source, args.run_id)
    # No assignment/lease is accepted in argv, env, a manifest or evidence.
    pins = read_json(os.environ.get('CLOUDHYPERVISOR_PINS', ''))
    validate_pins(pins, {arch})
    require(subprocess.check_output(['go', 'env', 'GOVERSION'], text=True, timeout=10).strip() == pins['common']['go_version'])
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
        build_limit = time.monotonic() + 120
        for binary, package in [('guest', 'pulse-cloudhypervisor-guest'), ('provider', 'pulse-cloudhypervisor')]:
            remaining = build_limit - time.monotonic()
            require(remaining > 0)
            require(subprocess.run(['env', 'CGO_ENABLED=0', 'GOOS=linux', 'GOARCH='+arch, 'go', 'build', '-trimpath', '-buildvcs=false', '-o', str(scratch/binary), './cmd/'+package], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=remaining).returncode == 0)
        agent = digest(scratch/'guest')
        require(base['tools']['guest-agent'] == agent)
        os.environ['CLOUDHYPERVISOR_PROVIDER_BINARY'] = str(scratch/'provider')
        audit = native_outcomes.NativeAudit({'github.com/colony-2/pulse-cloudhypervisor/tests/live'})
        def started():
            if args.result:
                write_json(native_outcomes.started_path(args.result), {'version': 1})
        tests = collect(['go', 'test', '-json', '-count=1', '-tags=live', f'-timeout={NATIVE_SUITE_MINUTES}m', './tests/live'], live,
                        timeout=NATIVE_COLLECTION_SECONDS, audit=audit, started=started, owned_group=binding is None)
        result = {'version': 1, 'architecture': arch, 'host_architecture': arch,
                  'candidate': candidate, 'source_sha256': source,
                  'mapping_sha256': digest(ROOT/'tests/expectations.json'),
                  'pins': pins, 'selected_assets': selected,
                  'fixture_identity': fixture['identity'], 'guest_agent_sha256': agent,
                  'tests': tests}
        require(source_digest() == source)
        verdict = audit.integrity(tests['exit_code'])
        if args.result:
            write_json(args.result, result)
            if binding is not None:
                native_outcomes.bind(args.result, binding, args.run_id, verdict)
        require(verdict[0] == 'complete')
        validate_events(live, tests)
    print('PASS: every mandatory native assertion executed')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, TypeError, UnicodeError, subprocess.SubprocessError):
        print('FAIL: native architecture, fixture authority, fixed inputs or mandatory assertions unavailable')
        sys.exit(1)
