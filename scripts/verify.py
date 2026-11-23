#!/usr/bin/env python3
"""Shared local/full verification; only fixed labels reach diagnostics."""
import argparse
import os
import pathlib
import platform
import subprocess
import sys
import tempfile
from placement import confirmed
from verification import (IDENTITY, ROOT, collect, contract, read_json, require, source_digest, split_mapping,
                          use_installed_toolchain, validate_events, validate_local, write_json)

HELPER_PACKAGE = 'github.com/colony-2/pulse-cloudhypervisor/tests/fixture'
HELPER_ASSERTIONS = {'S26': [(HELPER_PACKAGE, 'TestS26/'+leaf) for leaf in
                            ('profiles', 'terminal', 'renewal', 'retry', 'replacement')]}
LIVE_PACKAGE = 'github.com/colony-2/pulse-cloudhypervisor/tests/live'
ACCESS = (
    ('^TestNativeHost$', 'KVM and VMM control', ('TestNativeHost/KVM', 'TestNativeHost/HypervisorControl')),
    ('^TestNativeAllocationAccess$/^TAP$', 'TAP creation and removal', ('TestNativeAllocationAccess/TAP',)),
    ('^TestNativeAllocationAccess$/^UserManager$', 'independent user supervision', ('TestNativeAllocationAccess/UserManager',)),
)


def run(command, label):
    try:
        code = subprocess.run(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=1200).returncode
    except (OSError, subprocess.TimeoutExpired):
        code = 1
    print(('PASS: ' if code == 0 else 'FAIL: ') + label, flush=True)
    return code == 0


def check_helper():
    try:
        receipt = collect(['go', '-C', 'tests/fixture', 'test', '-json', '-race', '-count=1',
                           '-timeout=90s', './...'], HELPER_ASSERTIONS, timeout=1200)
        validate_events(HELPER_ASSERTIONS, receipt)
        ok = True
    except (OSError, ValueError, KeyError, TypeError, UnicodeError):
        ok = False
    print(('PASS: ' if ok else 'FAIL: ') + 'isolated lease fixture tests (required S26 assertions)', flush=True)
    return ok


def check_access(binding):
    passed = True
    for pattern, label, tests in ACCESS:
        if confirmed(binding) is None:
            return False
        mapping = {label: [(LIVE_PACKAGE, test) for test in tests]}
        try:
            receipt = collect(['go', 'test', '-json', '-count=1', '-tags=live', '-run='+pattern,
                               '-timeout=30s', './tests/live'], mapping, timeout=45)
            validate_events(mapping, receipt)
            ok = True
        except (OSError, ValueError, KeyError, TypeError, UnicodeError):
            ok = False
        print(('PASS: ' if ok else 'FAIL: ') + label, flush=True)
        passed &= ok
    return passed


def local_checks(scratch, local):
    failed = False
    try:
        receipt = collect(['go', 'test', '-count=1', '-json', './...'], local)
    except OSError:
        receipt = {'version': 1, 'exit_code': 1, 'valid': False, 'events': []}
    write_json(scratch/'local.json', receipt)
    ok = receipt['exit_code'] == 0 and receipt['valid']
    print(('PASS: ' if ok else 'FAIL: ') + 'deterministic Go tests', flush=True)
    failed |= not ok
    checks = [(['python3', 'scripts/verify_test.py'], 'verifier failure tests'),
              (['c2j', 'test', 'run', '--file', 'tests/hook.test.yaml', '--strict', '--out-dir', str(scratch/'hook')], 'recipe hook tests'),
              (['go', 'test', '-race', '-count=1', './...'], 'Go race tests'),
              (['go', 'vet', './...'], 'Go vet'),
              (['go', '-C', 'tests/fixture', 'vet', './...'], 'lease fixture vet')]
    for command, label in checks:
        failed |= not run(command, label)
    failed |= not check_helper()
    for target in ('arm64',):
        for name, package in (('provider', 'pulse-cloudhypervisor'), ('guest', 'pulse-cloudhypervisor-guest')):
            command = ['env', 'CGO_ENABLED=0', 'GOOS=linux', 'GOARCH='+target, 'go', 'build', '-trimpath', '-buildvcs=false', '-o', str(scratch/(name+'-'+target)), './cmd/'+package]
            failed |= not run(command, name+' cross-build '+target)
    failed |= not run(['bash', 'scripts/external-consumer.sh', '--events', str(scratch/'consumer.json')], 'published API/SDK external consumers')
    try:
        validate_local(local, read_json(scratch/'local.json'), read_json(scratch/'consumer.json'))
        ok = True
    except (ValueError, KeyError, OSError, TypeError, UnicodeError):
        ok = False
    print(('PASS: ' if ok else 'FAIL: ') + 'all 78 local requirement mappings', flush=True)
    return failed or not ok


def source_unchanged(source):
    if source_digest() == source:
        return True
    print('FAIL: source changed during verification', flush=True)
    return False


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--local", action="store_true")
    args = parser.parse_args()
    os.chdir(ROOT)
    os.environ.update(GOWORK='off', GOTOOLCHAIN='local')
    use_installed_toolchain()
    mapping = contract()
    local, _ = split_mapping(mapping)
    source = source_digest()
    candidate = os.environ.get('CANDIDATE') or subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    require(IDENTITY.fullmatch(candidate))
    arch = {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(platform.machine())
    if arch != 'arm64':
        print('FAIL: verification requires an ARM64 worker', flush=True)
        if args.local:
            print('VM execution was not tested by this command', flush=True)
        return 1
    binding = None if args.local else confirmed()
    if not args.local and binding is None:
        return 1
    if binding:
        print(f'Verification inputs: candidate={candidate} source_sha256={source}', flush=True)
    failed = False
    with tempfile.TemporaryDirectory(prefix='ch-verify-') as scratch:
        scratch = pathlib.Path(scratch)
        failed |= local_checks(scratch, local)
        if not args.local:
            if failed or not source_unchanged(source) or not check_access(binding) or not confirmed(binding) or not source_unchanged(source):
                print('FAIL: guest validation blocked before dispatch', flush=True)
                return 1
            native = scratch/'native.json'
            pins = os.environ.get('CLOUDHYPERVISOR_PINS', '')
            # These paths contain nonsecret operator inputs. No lease is read from env.
            failed |= not run(['bash', 'scripts/acceptance.sh', '--arch', arch, '--result', str(native), '--candidate', candidate], 'native acceptance '+arch)
            if not confirmed(binding) or not source_unchanged(source):
                return 1
            failed |= not run(['python3', 'scripts/check-coverage.py', '--events', str(scratch/'local.json'),
                              '--consumer', str(scratch/'consumer.json'), '--native', str(native),
                              '--pins', pins, '--candidate', candidate, '--source', source,
                              '--guest-agent-arm64', str(scratch/'guest-arm64')], 'candidate-bound ARM64 aggregation')
            failed |= confirmed(binding) is None
        failed |= not source_unchanged(source)
    if args.local:
        print("VM execution was not tested by this command", flush=True)
    return int(failed)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, TypeError, UnicodeError):
        print('FAIL: verification inputs or tooling are unavailable')
        if '--local' in sys.argv[1:]:
            print('VM execution was not tested by this command')
        sys.exit(1)
