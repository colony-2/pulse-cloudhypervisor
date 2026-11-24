#!/usr/bin/env python3
"""Shared local/full verification; only fixed labels reach diagnostics."""
import argparse
import os
import pathlib
import platform
import secrets
import subprocess
import sys
import tempfile
import time
from placement import confirmed, native_identity, record_native_run
import native_outcomes
import owned_process
from verification import (decode, IDENTITY, NATIVE_RUN_SECONDS, ROOT, collect, contract, read_json, require, source_digest, split_mapping,
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


def run(command, label, timeout=1200, deadline=None):
    try:
        code = subprocess.run(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              timeout=budget(deadline, timeout)).returncode
    except (OSError, ValueError, subprocess.TimeoutExpired):
        code = 1
    print(('PASS: ' if code == 0 else 'FAIL: ') + label, flush=True)
    return code == 0


def budget(deadline, limit):
    remaining = limit if deadline is None else min(limit, deadline-time.monotonic())
    require(remaining > 0)
    return remaining


def check_helper(deadline, race_go):
    try:
        receipt = collect(race_go + ['-C', 'tests/fixture', 'test', '-json', '-race', '-count=1',
                           '-timeout=90s', './...'], HELPER_ASSERTIONS, timeout=budget(deadline, 1200))
        validate_events(HELPER_ASSERTIONS, receipt)
        ok = True
    except (OSError, ValueError, KeyError, TypeError, UnicodeError):
        ok = False
    print(('PASS: ' if ok else 'FAIL: ') + 'isolated lease fixture tests (required S26 assertions)', flush=True)
    return ok


def check_access(binding, deadline):
    passed = True
    for pattern, label, tests in ACCESS:
        if confirmed(binding) is None:
            return False
        mapping = {label: [(LIVE_PACKAGE, test) for test in tests]}
        try:
            receipt = collect(['go', 'test', '-json', '-count=1', '-tags=live', '-run='+pattern,
                               '-timeout=30s', './tests/live'], mapping, timeout=budget(deadline, 45))
            validate_events(mapping, receipt)
            ok = True
        except (OSError, ValueError, KeyError, TypeError, UnicodeError):
            ok = False
        print(('PASS: ' if ok else 'FAIL: ') + label, flush=True)
        passed &= ok
    return passed


def local_checks(scratch, local, deadline, race_go):
    failed = False
    try:
        receipt = collect(['go', 'test', '-count=1', '-json', './...'], local, timeout=budget(deadline, 900))
    except (OSError, ValueError):
        receipt = {'version': 1, 'exit_code': 1, 'valid': False, 'events': []}
    write_json(scratch/'local.json', receipt)
    ok = receipt['exit_code'] == 0 and receipt['valid']
    print(('PASS: ' if ok else 'FAIL: ') + 'deterministic Go tests', flush=True)
    failed |= not ok
    checks = [(['python3', 'scripts/verify_test.py'], 'verifier failure tests'),
              (['c2j', 'test', 'run', '--file', 'tests/hook.test.yaml', '--strict', '--out-dir', str(scratch/'hook')], 'recipe hook tests'),
              (race_go + ['test', '-race', '-count=1', './...'], 'Go race tests'),
              (['go', 'vet', './...'], 'Go vet'),
              (['go', '-C', 'tests/fixture', 'vet', './...'], 'lease fixture vet')]
    for command, label in checks:
        failed |= not run(command, label, deadline=deadline)
    failed |= not check_helper(deadline, race_go)
    for target in ('arm64', 'amd64'):
        for name, package in (('provider', 'pulse-cloudhypervisor'), ('guest', 'pulse-cloudhypervisor-guest')):
            command = ['env', 'CGO_ENABLED=0', 'GOOS=linux', 'GOARCH='+target, 'go', 'build', '-trimpath', '-buildvcs=false', '-o', str(scratch/(name+'-'+target)), './cmd/'+package]
            failed |= not run(command, name+' cross-build '+target, deadline=deadline)
    failed |= not run(['bash', 'scripts/external-consumer.sh', '--events', str(scratch/'consumer.json')], 'published API/SDK external consumers', deadline=deadline)
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
    start = time.monotonic()
    local_deadline = start + 30*60
    parser = argparse.ArgumentParser()
    parser.add_argument("--local", action="store_true")
    args = parser.parse_args()
    os.chdir(ROOT)
    os.environ.update(GOWORK='off', GOTOOLCHAIN='local')
    use_installed_toolchain()
    mapping = contract()
    local, live = split_mapping(mapping)
    source = source_digest()
    candidate = os.environ.get('CANDIDATE') or subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    require(IDENTITY.fullmatch(candidate))
    arch = {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(platform.machine())
    if arch not in ('arm64', 'amd64'):
        print('FAIL: verification requires an ARM64 or AMD64 worker', flush=True)
        if args.local:
            print('VM execution was not tested by this command', flush=True)
        return 1
    binding = None if args.local else confirmed()
    if not args.local and binding is None:
        return 1
    if binding:
        settings = pathlib.Path(os.environ['CLOUDHYPERVISOR_BUILD_SETTINGS'])
        with settings.open('rb') as f:
            raw = f.read(16385)
        require(len(raw)<=16384)
        profile = decode(raw)
        require(isinstance(profile,dict) and set(profile)<= {'variables'} and profile.get('variables',{}) in ({},{'profile':'ubuntu-24.04-'+arch}))
        print(f'Verification inputs: candidate={candidate} source_sha256={source}', flush=True)
    failed = False
    with tempfile.TemporaryDirectory(prefix='ch-verify-') as scratch:
        scratch = pathlib.Path(scratch)
        # Race-instrumented objects are only used by the local checks. Reclaim
        # their private cache before reserving the three full-size guest disks.
        with tempfile.TemporaryDirectory(prefix='race-', dir=scratch) as cache:
            race_go = ['go'] if args.local else ['env', 'GOCACHE=' + cache, 'go']
            failed |= local_checks(scratch, local, local_deadline, race_go)
        if not args.local:
            if failed or not source_unchanged(source) or not check_access(binding, local_deadline) or not confirmed(binding) or not source_unchanged(source):
                print('FAIL: guest validation blocked before dispatch', flush=True)
                return 1
            native = scratch/'native.json'
            run_id = secrets.token_hex(16)
            run_binding = scratch/'run-binding.json'
            record_native_run(run_binding, binding, candidate, source, run_id)
            outcome_path = os.environ.get('CH_NATIVE_OUTCOMES')
            native_outcomes.publish(outcome_path, native_outcomes.report())
            from managed_fixture import prepared
            def still_owned():
                return confirmed(binding) is not None and source_unchanged(source)
            fixture_deadline = start + 85*60
            with prepared(scratch, still_owned, fixture_deadline) as (fixture, pins):
                require(still_owned())
                pins = str(pins)
                os.environ.update(CLOUDHYPERVISOR_FIXTURE=str(fixture), CLOUDHYPERVISOR_PINS=pins)
                native_outcomes.publish(outcome_path, native_outcomes.report('untrusted', 'collection_in_progress'))
                ok, reaped = owned_process.isolated_run(['bash', 'scripts/acceptance.sh', '--arch', arch, '--result', str(native), '--candidate', candidate,
                                               '--run-binding', str(run_binding), '--run-id', run_id], timeout=budget(fixture_deadline - 6*60, NATIVE_RUN_SECONDS))
                print(('PASS: ' if ok else 'FAIL: ') + 'native acceptance '+arch, flush=True)
                print(('PASS: ' if reaped else 'FAIL: ') + 'native acceptance process cleanup', flush=True)
                failed |= not ok
                if not reaped:
                    return 1
                if not confirmed(binding) or not source_unchanged(source):
                    return 1
                outcome = native_outcomes.project(native, native_identity(binding, candidate, source), run_id, live)
                native_outcomes.publish(outcome_path, outcome)
                failed |= outcome['integrity'] != 'complete'
                command = ['python3', 'scripts/check-coverage.py', '--events', str(scratch/'local.json'),
                           '--consumer', str(scratch/'consumer.json'), '--native', str(native),
                           '--candidate', candidate, '--source', source, '--guest-agent-'+arch, str(scratch/('guest-'+arch))]
                other = os.environ.get('CH_OTHER_NATIVE')
                if other:
                    peer = 'amd64' if arch == 'arm64' else 'arm64'
                    prior = read_json(other)
                    require(prior['architecture'] == peer)
                    combined = read_json(pins)
                    combined['architectures'][peer] = prior['pins']['architectures'][peer]
                    combined_path = scratch/'combined-pins.json'
                    write_json(combined_path, combined)
                    command += ['--pins', str(combined_path), '--native', other, '--guest-agent-'+peer, str(scratch/('guest-'+peer))]
                else:
                    command += ['--host-run', arch, '--pins', pins]
                failed |= not run(command, 'candidate-bound '+arch+' verification', timeout=60)
                export = os.environ.get('CH_NATIVE_RESULT')
                if export and not failed and still_owned():
                    write_json(export, read_json(native))
                failed |= confirmed(binding) is None
        failed |= not source_unchanged(source)
    if args.local:
        print("VM execution was not tested by this command", flush=True)
    return int(failed)


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, TypeError, UnicodeError, subprocess.SubprocessError):
        print('FAIL: verification inputs or tooling are unavailable')
        if '--local' in sys.argv[1:]:
            print('VM execution was not tested by this command')
        sys.exit(1)
