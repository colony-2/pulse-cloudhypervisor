#!/usr/bin/env python3
"""Synthetic receipts below test gates only; they do not demonstrate VM execution."""
import copy
import gzip
import importlib.util
import io
import json
import os
import pathlib
import shutil
import subprocess
import struct
import sys
import tarfile
import tempfile
import unittest
from unittest import mock
import zipfile
import zlib

ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT/'scripts'))
import verification as gate
import native_assets
import native_setup
import owned_units
import native_outcomes
import owned_process
import recovery_preflight


def receipt(package='pkg', test='TestRequired'):
    return {'version': 1, 'exit_code': 0, 'valid': True, 'events': [
        {'Action': 'start', 'Package': package},
        {'Action': 'run', 'Package': package, 'Test': test},
        {'Action': 'pass', 'Package': package, 'Test': test},
        {'Action': 'pass', 'Package': package}]}


def pins():
    def assets(d):
        return dict(hypervisor=d*64, disk=d*64, kernel=d*64, initramfs=None,
                    preparation=d*64, bootstrap_agent=d*64,
                    tools={k: d*64 for k in ('c2j','nix','docker')}, docker_image='sha256:'+d*64)
    return dict(version=1, common=dict(provider_api='v0.0.0-20261120061856-2919f1ed5e42', plugin_sdk='v0.0.0-20261120061856-2919f1ed5e42', sdk_baseline=3, c2j_revision='a'*40, go_version='go1.26.8'), architectures=dict(arm64=assets('b')))


class VerificationTests(unittest.TestCase):
    def setUp(self):
        self.mapping = {'A': [['pkg', 'TestRequired']]}
        self.receipt = receipt()

    def bootstrap_inputs(self, root):
        image, agent = root/'initrd', root/'agent'
        image.write_bytes(b'pinned Ubuntu initrd')
        program = bytearray(64)
        program[:6] = b'\x7fELF\x02\x01'
        struct.pack_into('<H', program, 18, 183)
        agent.write_bytes(program)
        return image, agent, gate.digest(image), gate.digest(agent)

    def test_S01(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            image, agent, disk_hash, agent_hash = self.bootstrap_inputs(root)
            first, second = root/'one', root/'two'
            for target in (first, second):
                native_setup.build(image, agent, target, disk_hash, agent_hash)
            self.assertEqual(first.read_bytes(), second.read_bytes())
            self.assertEqual(gate.digest(image), disk_hash)
            self.assertEqual(gate.digest(agent), agent_hash)
            data = first.read_bytes()
            offset = (image.stat().st_size + 3) & ~3
            archive = gzip.decompress(data[offset:])
            files = {}
            while archive:
                self.assertEqual(archive[:6], b'070701')
                fields = [int(archive[n:n+8], 16) for n in range(6, 110, 8)]
                size, namesize = fields[6], fields[11]
                name = archive[110:110+namesize-1].decode()
                start = (110 + namesize + 3) & ~3
                files[name] = archive[start:start+size]
                self.assertEqual(fields[2:4], [0, 0])
                archive = archive[(start+size+3) & ~3:]
            self.assertEqual(files['pulse-bootstrap/agent'], agent.read_bytes())
            self.assertEqual(files['pulse-bootstrap/service'], (ROOT/'guest/pulse-cloudhypervisor-guest.service').read_bytes())
            self.assertIn(b'/dev/vda1', files['init'])
            self.assertIn(b'pulse.prepare=', files['pulse-bootstrap/install'])

    def test_S02(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            image, agent, disk_hash, agent_hash = self.bootstrap_inputs(root)
            for disk, binary in [('0'*64, agent_hash), (disk_hash, '0'*64)]:
                with self.assertRaises(ValueError):
                    native_setup.build(image, agent, root/'bad', disk, binary)
                self.assertEqual(set(root.iterdir()), {agent, image})
            agent.write_bytes(b'wrong architecture')
            with self.assertRaises(ValueError):
                native_setup.build(image, agent, root/'bad', disk_hash, gate.digest(agent))
            (root/'etc').mkdir()
            program = root/'uname'
            program.write_text('#!/bin/sh\nprintf "%s\\n" "$TEST_ARCH"\n')
            program.chmod(0o700)
            for distro, arch, blocked, passed in [('ubuntu/24.04','aarch64',False,True),
                    ('debian/12','aarch64',False,False),('ubuntu/22.04','aarch64',False,False),
                    ('ubuntu/24.04','x86_64',False,False),('ubuntu/24.04','aarch64',True,False)]:
                family, release = distro.split('/')
                (root/'etc/os-release').write_text(f'ID={family}\nVERSION_ID="{release}"\n')
                if blocked:
                    intent = root/'var/lib/pulse-guest/worker-intent'
                    intent.parent.mkdir(parents=True)
                    intent.touch()
                result = subprocess.run(['/bin/sh','-c',native_setup.ROOT_CHECK.decode(),'sh',str(root)],
                                        env={'PATH':str(root),'TEST_ARCH':arch})
                self.assertEqual(result.returncode == 0, passed)
            self.test_boot_archive_boundaries()

    def test_S03(self):
        from unittest import mock
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            image, agent, disk_hash, agent_hash = self.bootstrap_inputs(root)
            prior = root/'old'
            prior.write_bytes(b'previous source')
            for target in [prior, root/'link']:
                if target != prior:
                    target.symlink_to(prior)
                with self.assertRaises(FileExistsError):
                    native_setup.build(image, agent, target, disk_hash, agent_hash)
            before = set(root.iterdir())
            with mock.patch.object(native_setup, 'newc', side_effect=InterruptedError):
                with self.assertRaises(InterruptedError):
                    native_setup.build(image, agent, root/'partial', disk_hash, agent_hash)
            self.assertEqual(set(root.iterdir()), before)
            self.assertEqual(prior.read_bytes(), b'previous source')

    def test_owned_unit_record_and_ambiguity(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            state = root/'state'
            name = 'launch-a'
            boot = '12345678-1234-1234-1234-123456789abc'
            work = state/'jobs'/owned_units.key(name)
            work.mkdir(parents=True)
            for folder in (state, work.parent, work):
                folder.chmod(0o700)
            record = work/'record.json'
            unit_name = 'pulse-ch-'+owned_units.key(str(state))+'-'+owned_units.key(name)+'.service'
            data = dict(version=1, launch_id=name, unit=unit_name, identity='01'*16,
                        owner='owned', boot_id=boot+'\n')
            record.write_text(json.dumps(data))
            record.chmod(0o600)
            unit = owned_units.owned_spec(record, state, 'owned', boot)
            self.assertEqual(unit.name, unit_name)
            canary = root/'canary'
            canary.write_text('independent')
            units = owned_units.Units(dict(state_dir=str(state), owner='owned', runner='/runner'), root/'ledger')
            with mock.patch.object(units, 'show', return_value=None), mock.patch('owned_units.subprocess.run') as stop:
                with self.assertRaises(ValueError):
                    units.release(unit)
                stop.assert_not_called()
            for field, value in [('boot_id', 'stale'), ('owner', 'other'), ('unit', 'foreign.service'),
                                 ('identity', 'unbound'), ('launch_id', 'wrong')]:
                record.write_text(json.dumps(dict(data, **{field: value})))
                with self.assertRaises(ValueError):
                    owned_units.owned_spec(record, state, 'owned', boot)
            record.unlink()
            record.symlink_to(canary)
            with self.assertRaises(OSError):
                owned_units.owned_spec(record, state, 'owned', boot)
            self.assertEqual(canary.read_text(), 'independent')

    def test_managed_settings_rejected_before_access(self):
        with HookHarness() as harness:
            for raw in [b'null', b'{', b'{"variables":{"command":"run"}}',
                        b'{"variables":{"profile":"/tmp/profile"}}', b' ' * 16385]:
                harness.settings.write_bytes(raw)
                result = harness.run()
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('fixture setup', harness.log.read_text())
                self.assertNotIn('-tags=live', harness.log.read_text())

    def test_complete_results(self):
        gate.validate_events(self.mapping, self.receipt)
        mapping = gate.contract()
        self.assertEqual(len(mapping), 89)
        ids = set(mapping)
        synthetic = {i: [['pkg', 'TestRequired']] for i in ids}
        gate.validate_events(synthetic, self.receipt)

    def test_boot_archive_boundaries(self):
        # Synthetic bytes test staging only; these are never native receipts.
        disk = bytearray(1024 * 1024)
        disk[510:520] = b'\x55\xaaEFI PART'
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            archive, target = root/'archive', root/'disk'
            def pack(members):
                with tarfile.open(archive, 'w:gz') as tar:
                    for name, data, kind in members:
                        info = tarfile.TarInfo(name)
                        info.type, info.size = kind, len(data)
                        tar.addfile(info, io.BytesIO(data) if kind == tarfile.REGTYPE else None)
            good = [('disk1.img', disk, tarfile.REGTYPE)]
            pack(good)
            with self.assertRaises(ValueError):
                native_assets.extract_disk(archive, '0'*64, target)
            self.assertFalse(target.exists())
            native_assets.extract_disk(archive, gate.digest(archive), target)
            self.assertEqual(target.read_bytes(), disk)
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                native_assets.extract_disk(archive, gate.digest(archive), target)
            self.assertEqual(target.read_bytes(), disk)
            target.unlink()
            for members in [
                [('../escape.img', disk, tarfile.REGTYPE)],
                [('/escape.img', disk, tarfile.REGTYPE)],
                [('disk1.img', b'', tarfile.SYMTYPE)],
                [('disk1.img', b'', tarfile.LNKTYPE)],
                [('disk1.img', b'', tarfile.CHRTYPE)],
                [('metadata', b'no disk', tarfile.REGTYPE)],
                [('disk1.img', b'QFI\xfb'+bytes(1024), tarfile.REGTYPE)],
                good + good,
            ]:
                pack(members)
                with self.assertRaises(ValueError):
                    native_assets.extract_disk(archive, gate.digest(archive), target)
                self.assertFalse(target.exists())

    def test_boot_kernel_and_partial_file_cleanup(self):
        image = bytearray(64)
        image[56:60] = b'ARM\x64'
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            source, target = root/'source', root/'Image'
            for encoded in (bytes(image), gzip.compress(image)):
                source.write_bytes(encoded)
                native_assets.unpack_kernel(source, target)
                self.assertEqual(target.read_bytes(), image)
                target.unlink()
            source.write_bytes(b'invalid kernel')
            with self.assertRaises(ValueError):
                native_assets.unpack_kernel(source, target)
            self.assertFalse(target.exists())
            target.write_bytes(b'foreign canary')
            with self.assertRaises(FileExistsError):
                native_assets.unpack_kernel(source, target)
            self.assertEqual(target.read_bytes(), b'foreign canary')
            target.unlink()
            with self.assertRaises(ValueError):
                native_assets.copy_sparse(io.BytesIO(bytes(65)), target, 64)
            self.assertFalse(target.exists())
            target.symlink_to(source)
            with self.assertRaises(FileExistsError):
                native_assets.copy_sparse(io.BytesIO(image), target, 64)
            self.assertTrue(target.is_symlink())
            self.assertEqual(source.read_bytes(), b'invalid kernel')

    def test_uec_rootfs_becomes_reproducible_gpt_vda1(self):
        # Ubuntu's pinned UEC archive actually contains a bare ext4 filesystem.
        # Validate the generated disk independently, including both GPT CRCs.
        rootfs = bytearray(2 * 1024 * 1024)
        rootfs[1080:1082] = b'\x53\xef'
        rootfs[-16:] = b'root-data-canary'
        with tempfile.TemporaryDirectory() as d:
            root = pathlib.Path(d)
            archive, target = root/'archive', root/'disk'
            with tarfile.open(archive, 'w:gz') as tar:
                info = tarfile.TarInfo('disk1.img')
                info.size = len(rootfs)
                tar.addfile(info, io.BytesIO(rootfs))
            native_assets.extract_disk(archive, gate.digest(archive), target)
            disk = target.read_bytes()
            self.assertEqual(disk[450], 0xee)
            self.assertEqual(disk[510:512], b'\x55\xaa')
            entries = disk[1024:1024+16384]
            self.assertEqual(entries, disk[-16896:-512])
            for offset, current, other in [(512, 1, len(disk)//512-1), (len(disk)-512, len(disk)//512-1, 1)]:
                header = bytearray(disk[offset:offset+92])
                self.assertEqual(header[:8], b'EFI PART')
                self.assertEqual(struct.unpack_from('<QQ', header, 24), (current, other))
                crc = struct.unpack_from('<I', header, 16)[0]
                struct.pack_into('<I', header, 16, 0)
                self.assertEqual(zlib.crc32(header), crc)
                self.assertEqual(struct.unpack_from('<I', header, 88)[0], zlib.crc32(entries))
            first, last = struct.unpack_from('<QQ', entries, 32)
            self.assertEqual(first, 2048)
            self.assertEqual((last-first+1)*512, len(rootfs))
            self.assertEqual(disk[first*512:(last+1)*512], rootfs)
            before = gate.digest(target)
            target.unlink()
            native_assets.extract_disk(archive, gate.digest(archive), target)
            self.assertEqual(gate.digest(target), before)

    def test_missing_empty_skipped_failed_incomplete(self):
        cases = []
        for events in ([], self.receipt['events'][1:], self.receipt['events'][:-1], self.receipt['events'][:2]+self.receipt['events'][3:]):
            r = receipt(); r['events'] = events; cases.append(r)
        for action in ('skip', 'fail'):
            r = receipt(); r['events'].insert(-1, dict(Action=action, Package='pkg', Test='TestRequired')); cases.append(r)
        for field, value in [('exit_code', 1), ('valid', False), ('version', 2), ('exit_code', True)]:
            r = receipt(); r[field] = value; cases.append(r)
        r = receipt(); r['events'].append(r['events'][-1]); cases.append(r)
        r = receipt(); r['events'][2]['Output'] = 'synthetic-private-lease'; cases.append(r)
        for r in cases:
            with self.subTest(receipt=r), self.assertRaises((ValueError, TypeError)):
                gate.validate_events(self.mapping, r)
        for mapping in ({}, {'A': []}, {'A': [['pkg', 'TestMissing']]}):
            with self.assertRaises(ValueError): gate.validate_events(mapping, self.receipt)
        sub = {'A': [['pkg', 'TestRequired/child']]}
        with self.assertRaises(ValueError): gate.validate_events(sub, self.receipt)
        r = receipt()
        r['events'][2:2] = [dict(Action='run', Package='pkg', Test='TestRequired/child'), dict(Action='pass', Package='pkg', Test='TestRequired/child')]
        gate.validate_events(sub, r)

    def native(self):
        p = pins()
        return [dict(version=1, architecture=a, host_architecture=a, candidate='candidate', source_sha256='c'*64,
                     mapping_sha256='d'*64, pins=p, selected_assets=p['architectures'][a],
                     fixture_identity='synthetic-gate-test', guest_agent_sha256='e'*64, tests=receipt()) for a in ('arm64',)]

    def validate_native(self, results, p=None):
        gate.validate_native(results, 'candidate', 'c'*64, 'd'*64, p or pins(), self.mapping, {'arm64':'e'*64})

    def test_native_identity_and_pins(self):
        self.validate_native(self.native())
        cases = [[], self.native()+self.native()]
        for field, value in [('architecture','amd64'), ('host_architecture','wrong'), ('candidate','stale'),
                             ('source_sha256','f'*64), ('mapping_sha256','f'*64), ('pins',{'image':'changed'}),
                             ('selected_assets',{}), ('fixture_identity',''),
                             ('guest_agent_sha256','f'*64), ('tests',{}), ('version',2)]:
            rs = copy.deepcopy(self.native()); rs[0][field] = value; cases.append(rs)
        for rs in cases:
            with self.subTest(results=rs), self.assertRaises((ValueError, TypeError)):
                self.validate_native(rs)
        for change in ('missing_arch','extra_arch','missing_tool','missing_asset','unknown_field','mutable_revision'):
            p = pins()
            if change == 'missing_arch': del p['architectures']['arm64']
            elif change == 'extra_arch': p['architectures']['amd64'] = p['architectures']['arm64']
            elif change == 'missing_tool': del p['architectures']['arm64']['tools']['docker']
            elif change == 'missing_asset': del p['architectures']['arm64']['kernel']
            elif change == 'unknown_field': p['unknown'] = True
            else: p['common']['c2j_revision'] = 'main'
            with self.assertRaises(ValueError): self.validate_native(self.native(), p)

    def test_recognized_hook_contract(self):
        hook = (ROOT/'.c2/recipes/hooks/build/verify.yaml').read_text()
        for text in ('op: command_execution', 'scripts/verify.sh', 'umask 077', 'api_version: c2.build-hook/v1', 'artifact_refs:', 'printf failed', 'printf passed'):
            self.assertIn(text, hook)

    def test_hook_script_runs_children_and_propagates_failure(self):
        with HookHarness() as h:
            result = h.run()
            self.assertEqual(result.returncode, 0, result.stdout)
            calls = h.log.read_text()
            for text in ('test -count=1 -json ./...', 'test -race -count=1 ./...', 'vet ./...', '-C tests/fixture test -json -race -count=1 -timeout=90s ./...', '-C tests/fixture vet ./...', 'build -trimpath -buildvcs=false -o', 'test -json -count=1 -tags=live -run=^TestNativeHost$ -timeout=30s ./tests/live', 'acceptance --arch '+h.arch):
                self.assertIn(text, calls)
            for probe in h.probes:
                self.assertEqual(calls.count('-run='+probe), 1)
                self.assertLess(calls.index('-run='+probe), calls.index('acceptance --arch'))
            self.assertNotIn('acceptance --arch amd64', calls)
            self.assertNotIn('guest-amd64', calls)
            for failure in ('local','race','vet','build','build-provider','build-guest','consumer','host','native','hook','verifier','fixture','fixture-vet'):
                with self.subTest(failure=failure):
                    self.assertNotEqual(h.run(CHILD_FAIL=failure).returncode, 0)
            self.assertNotEqual(h.run(HOOK_MACHINE='x86_64').returncode, 0)
            self.assertNotEqual(h.run(NO_NATIVE_RESULT='1', VERIFY_LOCAL='1').returncode, 0)
            for mutation in ('candidate', 'source_sha256', 'architecture', 'mapping_sha256', 'pins', 'tests'):
                with self.subTest(mutation=mutation):
                    self.assertNotEqual(h.run(NATIVE_MUTATION=mutation).returncode, 0)
            # Empty native mappings still fail through the actual hook/CLI.
            m = gate.read_json(h.root/'tests/expectations.json'); m['LIVE-02'] = []
            (h.root/'tests/expectations.json').write_text(json.dumps(m))
            self.assertNotEqual(h.run().returncode, 0)

    def test_managed_placement_fails_before_probes(self):
        with HookHarness() as h:
            self.assertEqual(h.run(BINDING='paged').returncode, 0)
            for case in ('denied', 'malformed', 'oversized', 'stalled', 'tenant', 'job', 'task', 'inactive', 'expired', 'lease', 'worker', 'duplicate', 'cursor-loop'):
                with self.subTest(case=case):
                    result = h.run(BINDING=case)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn('-tags=live', h.log.read_text())
                    self.assertNotIn('acceptance', h.log.read_text())
            for allocation in ('', '[]', '{"schema_version":2,"base":"ubuntu-24.04-arm64","platform":"linux/arm64","capabilities":["not-kvm"]}', '{"schema_version":2,"base":"debian-arm64","platform":"linux/arm64","capabilities":["kvm"]}'):
                self.assertNotEqual(h.run(C2J_EXECUTION_ALLOCATION=allocation).returncode, 0)
                self.assertNotIn('-tags=live', h.log.read_text())
            self.assertEqual(h.run(local=True, C2J_EXECUTION_ALLOCATION='', BINDING='denied').returncode, 0)

    def test_access_failures_are_independent_and_block_guests(self):
        with HookHarness() as h:
            for changes in ({'CHILD_FAIL': 'host'}, {'CHILD_FAIL': 'tap'}, {'CHILD_FAIL': 'manager'},
                            {'OMIT_PROBE': 'tap'}, {'SLEEP_CHILD': 'tap'}, {'OMIT_PACKAGE': 'manager'},
                            {'CHILD_FAIL': 'host', 'OMIT_PROBE': 'manager'}):
                with self.subTest(changes=changes):
                    result = h.run(**changes)
                    self.assertNotEqual(result.returncode, 0)
                    calls = h.log.read_text()
                    for probe in h.probes:
                        self.assertEqual(calls.count('-run='+probe), 1)
                    self.assertNotIn('acceptance', calls)
            for change in ('boot', 'worker', 'task'):
                result = h.run(CHANGED_BINDING=change)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('acceptance', h.log.read_text())
            result = h.run(CHANGED_BINDING='boot', CHANGE_ON_PROBE='host')
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn('-run='+h.probes[1], h.log.read_text())
            self.assertNotIn('acceptance', h.log.read_text())

    def test_el2_probe_is_read_only_and_classifies_missing_proof(self):
        probe = recovery_preflight
        with mock.patch.object(probe.platform, 'system', return_value='Linux'), \
             mock.patch.object(probe.platform, 'machine', return_value='aarch64'), \
             mock.patch.object(probe.os, 'open', return_value=5) as opened, \
             mock.patch.object(probe.os, 'fstat', return_value=mock.Mock(st_mode=0o20600)), \
             mock.patch.object(probe.os, 'close') as closed, \
             mock.patch.object(probe.fcntl, 'ioctl') as ioctl:
            for replies, expected in (([12, 1], 'supported'), ([12, 0], 'unsupported'), ([11], 'unproven')):
                ioctl.reset_mock()
                ioctl.side_effect = replies
                self.assertEqual(probe.checked(probe.query())['status'], expected)
                expected_calls = [mock.call(5, probe.KVM_GET_API_VERSION, 0)]
                if len(replies) == 2:
                    expected_calls.append(mock.call(5, probe.KVM_CHECK_EXTENSION, probe.KVM_CAP_ARM_EL2))
                self.assertEqual(ioctl.call_args_list, expected_calls)
                self.assertEqual(opened.call_args.args[1] & os.O_ACCMODE, os.O_RDONLY)
            self.assertEqual(closed.call_count, 3)
        for error in (PermissionError(), subprocess.TimeoutExpired('kvm', 1)):
            with mock.patch.object(probe.subprocess, 'run', side_effect=error):
                self.assertEqual(probe.observe(), probe.UNPROVEN)

    def test_missing_el2_preserves_independent_dispatch_but_fails_verification(self):
        with HookHarness() as harness:
            for mode in ('unsupported', 'unproven', 'wrong-api', 'denied'):
                with self.subTest(mode=mode):
                    result = harness.run(EL2=mode)
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    calls = harness.log.read_text()
                    self.assertLess(calls.index(harness.probes[-1]), calls.index('el2-check'))
                    self.assertLess(calls.index('el2-check'), calls.index('acceptance'))
                    self.assertIn(b'ARM EL2 recovery preflight:', result.stdout)
                    report = gate.read_json(harness.outcomes)
                    self.assertEqual(report['preflight']['LIVE-09'], 'blocked')
            self.assertEqual(harness.run(local=True).returncode, 0)
            self.assertNotIn('el2-check', harness.log.read_text())

    def test_native_mixed_failure_preserves_only_complete_bound_outcomes(self):
        with HookHarness() as harness:
            for mode, integrity in (('mixed', 'complete'), ('malformed', 'damaged'), ('tampered', 'untrusted')):
                with self.subTest(mode=mode):
                    result = harness.run(EL2='unsupported', NATIVE_STREAM=mode)
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertIn(b'FAIL: candidate-bound ARM64 aggregation', result.stdout)
                    diagnostic = gate.read_json(harness.outcomes)
                    self.assertEqual(diagnostic['integrity'], integrity)
                    self.assertEqual(diagnostic['preflight']['LIVE-09'], 'blocked')
                    self.assertNotIn('private-native-output', json.dumps(diagnostic))
                    if integrity == 'complete':
                        self.assertEqual(diagnostic['outcomes']['LIVE-03'], 'pass')
                        self.assertNotIn('LIVE-09', diagnostic['outcomes'])
                    else:
                        self.assertNotIn('outcomes', diagnostic)

    def test_native_audit_rejects_incoherent_unmapped_events(self):
        base = receipt()['events']
        for extra in ([{'Action': 'run', 'Package': 'pkg', 'Test': 'TestOther'}],
                      [{'Action': 'pass', 'Package': 'pkg', 'Test': 'TestOther'}], base[1:2], base[2:3]):
            audit = native_outcomes.NativeAudit({'pkg'})
            for entry in base[:-1] + extra + base[-1:]:
                audit.feed((json.dumps(entry)+'\n').encode())
            self.assertEqual(audit.integrity(0)[0], 'damaged')
        audit = native_outcomes.NativeAudit({'pkg'})
        for entry in base:
            audit.feed((json.dumps(entry)+'\n').encode())
        self.assertEqual(audit.integrity(0)[0], 'complete')
        self.assertEqual(audit.integrity(-9)[0], 'damaged')
        self.assertEqual(audit.integrity(1)[0], 'damaged')

    @unittest.skipUnless(sys.platform == 'linux' and hasattr(os, 'pidfd_open'), 'Linux pidfds required')
    def test_managed_timeout_reaps_escaped_descendant(self):
        with tempfile.TemporaryDirectory() as scratch:
            pid_path = pathlib.Path(scratch)/'pid'
            escaped = ('import os, pathlib, signal, sys, time; '
                       'signal.signal(signal.SIGTERM, signal.SIG_IGN); '
                       'pathlib.Path(sys.argv[1]).write_text(str(os.getpid())); time.sleep(60)')
            parent = ('import subprocess, sys, time; '
                      'subprocess.Popen([sys.executable,"-c",sys.argv[1],sys.argv[2]], start_new_session=True); '
                      'time.sleep(60)')
            self.assertEqual(owned_process.run([sys.executable, '-c', parent, escaped, str(pid_path)],
                                              timeout=0.7, grace=0.15), (False, True))
            self.assertFalse(pathlib.Path('/proc', pid_path.read_text()).exists())

    def test_explicit_acceptance_fails_without_prerequisites(self):
        for arch in ('amd64', 'arm64'):
            env = dict(os.environ, CLOUDHYPERVISOR_FIXTURE='', CLOUDHYPERVISOR_PINS='')
            r = subprocess.run(['bash', str(ROOT/'scripts/acceptance.sh'), '--arch', arch], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=10)
            self.assertNotEqual(r.returncode, 0)
            self.assertTrue(b'FAIL:' in r.stdout or b'invalid choice' in r.stdout)

    def test_diagnostics_do_not_export_input(self):
        secret = 'synthetic-private-lease'
        collector = gate.Events(self.mapping)
        for event in self.receipt['events']:
            collector.feed(json.dumps(dict(event, Output=secret)).encode())
        collector.feed(json.dumps(dict(Action='output', Package='pkg', Output=secret)).encode())
        self.assertNotIn(secret, json.dumps(collector.result(0)))
        collector.feed(json.dumps(dict(Action='skip', Package='command-with-no-test-files')).encode())
        self.assertTrue(collector.result(0)['valid'])
        mandatory_skip = gate.Events(self.mapping)
        mandatory_skip.feed(json.dumps(dict(Action='skip', Package='pkg')).encode())
        self.assertFalse(mandatory_skip.result(0)['valid'])
        for bad in [b'{"Action":"output","Package":"pkg","Output":"'+secret.encode()+b'"',
                    b'{"Action":"pass","Action":"fail","Package":"pkg"}', b'x'*(1024*1024+1)]:
            collector.feed(bad)
        self.assertFalse(collector.result(0)['valid'])
        child = [sys.executable, '-c', 'import sys; print(\'{"Action":"output","Package":"pkg","Output":"'+secret+'"}\'); print("'+secret+'",file=sys.stderr)']
        captured = gate.collect(child, self.mapping)
        self.assertNotIn(secret, json.dumps(captured))
        with HookHarness() as h:
            result = h.run(CHILD_FAIL='vet', SENTINEL=secret)
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn(secret.encode(), result.stdout)
            self.assertEqual(list(h.root.rglob('*lease*')), [])
            result = h.run(local=True, CHILD_FAIL='vet', SENTINEL=secret)
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn(secret.encode(), result.stdout)
        with tempfile.NamedTemporaryFile(mode='w') as f:
            f.write('{"bad":"'+secret+'"}'); f.flush()
            result = subprocess.run([sys.executable,str(ROOT/'scripts/check-coverage.py'), '--events',f.name,'--consumer',f.name,
                                     '--native',f.name,'--pins',f.name,'--candidate','candidate','--source','c'*64,
                                     '--guest-agent-arm64',f.name], stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
            self.assertNotEqual(result.returncode,0)
            self.assertNotIn(secret.encode(),result.stdout)

    def test_local_command_and_full_boundary(self):
        with HookHarness() as h:
            # Poison VM-only inputs: local verification must never open them.
            h.pinfile.unlink()
            result = h.run(local=True, CLOUDHYPERVISOR_FIXTURE='/unreadable/fixture')
            self.assertEqual(result.returncode, 0, result.stdout)
            self.assertIn(b'VM execution was not tested by this command', result.stdout)
            calls = h.log.read_text()
            for command in ('test -count=1 -json ./...', 'test -race -count=1 ./...', '-C tests/fixture test -json -race -count=1 -timeout=90s ./...', '-C tests/fixture vet ./...'):
                self.assertEqual(calls.count(command), 1)
            self.assertEqual(calls.splitlines().count('vet ./...'), 1)
            self.assertEqual(calls.count('build -trimpath'), 2)
            for command in ('verifier', 'hook', 'consumer'):
                self.assertEqual(calls.splitlines().count(command), 1)
            self.assertNotIn('acceptance', calls)
            self.assertNotIn('-tags=live', calls)
            for failure in ('local','race','vet','build','build-provider','build-guest','consumer','hook','verifier','fixture','fixture-vet'):
                with self.subTest(failure=failure):
                    result = h.run(local=True, CHILD_FAIL=failure)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(b'VM execution was not tested by this command', result.stdout)
            self.assertNotEqual(h.run(VERIFY_LOCAL='1', CLOUDHYPERVISOR_LOCAL='1').returncode, 0)
            self.assertIn('acceptance', h.log.read_text())
            for local in (False, True):
                self.assertNotEqual(h.run(local=local, HOOK_MACHINE='x86_64').returncode, 0)
                self.assertEqual(h.log.read_text(), '')
                invalid = h.run(local=local, args=['--local'])
                self.assertNotEqual(invalid.returncode, 0)
                if local:self.assertIn(b'VM execution was not tested by this command', invalid.stdout)
            r = subprocess.run([sys.executable,str(h.root/'scripts/verify.py'),'--unsupported'], stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
            self.assertNotEqual(r.returncode, 0)
            # Empty LIVE coverage is allowed locally, never by the full CLI.
            h.pinfile.write_text(json.dumps(pins()))
            m = gate.read_json(h.root/'tests/expectations.json');m['LIVE-02'] = []
            (h.root/'tests/expectations.json').write_text(json.dumps(m))
            self.assertEqual(h.run(local=True).returncode, 0)
            self.assertNotEqual(h.run().returncode, 0)

    def test_S24(self):
        with HookHarness() as h:
            for local in (True, False):
                self.assertEqual(h.run(local=local).returncode, 0)
                for missing in ('missing', 'skip', 'fail', 'package'):
                    with self.subTest(local=local, missing=missing):
                        result = h.run(local=local, HELPER_EVENTS=missing)
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn(b'FAIL: isolated lease fixture tests', result.stdout)
                        self.assertIn(b'PASS: lease fixture vet', result.stdout)
                        self.assertIn(b'PASS: all 78 local requirement mappings', result.stdout)
                        if not local:
                            self.assertNotIn('acceptance', h.log.read_text())
            mapping = gate.read_json(h.root/'tests/expectations.json')
            mapping['IMG-03'].append(['live', 'TestNative'])
            (h.root/'tests/expectations.json').write_text(json.dumps(mapping))
            self.assertNotEqual(h.run().returncode, 0)
            mapping['IMG-03'].pop()
            mapping['LIVE-03'].append(['github.com/colony-2/pulse-cloudhypervisor/tests/fixture', 'TestS26/terminal'])
            (h.root/'tests/expectations.json').write_text(json.dumps(mapping))
            self.assertNotEqual(h.run().returncode, 0)
            for name in ('TestNativeHost/KVM', 'TestNativeAllocationAccess/TAP', 'TestNativePreflight', 'TestNativeRebootPreflight'):
                with self.subTest(preflight=name):
                    mapping['LIVE-03'] = [['live', name]]
                    (h.root/'tests/expectations.json').write_text(json.dumps(mapping))
                    with self.assertRaises(ValueError):
                        gate.contract(h.root)

    def test_S25(self):
        self.test_native_identity_and_pins()
        self.test_missing_empty_skipped_failed_incomplete()
        self.test_hook_script_runs_children_and_propagates_failure()

    def test_local_mapping_partitions(self):
        mapping = {'SDK-01': [['pkg','TestRequired'],['example.com/cloudhypervisor-consumer','TestConsumer']]}
        consumer = receipt('example.com/cloudhypervisor-consumer', 'TestConsumer')
        gate.validate_local(mapping, receipt(), consumer)
        for m, r, c in [(dict(mapping, missing=[]), receipt(), consumer),
                        (mapping, receipt('pkg','TestOmitted'), consumer),
                        (mapping, receipt(), receipt('example.com/cloudhypervisor-consumer','TestOmitted'))]:
            with self.assertRaises(ValueError):gate.validate_local(m, r, c)
        with HookHarness() as h:
            m = gate.read_json(h.root/'tests/expectations.json');m['SDK-01'] = []
            (h.root/'tests/expectations.json').write_text(json.dumps(m))
            for local in (False, True): self.assertNotEqual(h.run(local=local).returncode, 0)

    def test_child_timeout_and_absence(self):
        with HookHarness() as h:
            p = h.root/'scripts/verify.py'
            p.write_text(p.read_text().replace('timeout=1200', 'timeout=0.2').replace('budget(deadline, 900)', 'budget(deadline, 0.2)'))
            v = h.root/'scripts/verification.py'
            v.write_text(v.read_text().replace('timeout=900', 'timeout=0.2'))
            for local in (False, True):
                for kind in ('local', 'vet'):
                    self.assertNotEqual(h.run(local=local, SLEEP_CHILD=kind).returncode, 0)
            p.write_text(p.read_text().replace("['c2j',", "['missing-required-hook-tool',"))
            for local in (False, True): self.assertNotEqual(h.run(local=local).returncode, 0)

    def test_source_mutations_fail_commands(self):
        with HookHarness() as h:
            for local in (False, True):
                for mutation in ('content','mode','symlink','missing','new'):
                    with self.subTest(local=local, mutation=mutation):
                        result = h.run(local=local, SOURCE_MUTATION=mutation)
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn(b'FAIL: source changed during verification', result.stdout)
                        self.assertNotIn('acceptance', h.log.read_text())
                        p=h.root/'tracked';p.unlink(missing_ok=True);p.write_text('initial');p.chmod(0o644)
                        (h.root/'new-input').unlink(missing_ok=True)

    def test_candidate_proxy_excludes_nested_modules(self):
        with tempfile.TemporaryDirectory(prefix='ch-proxy-test-') as temp:
            root=pathlib.Path(temp)/'repo'
            (root/'scripts').mkdir(parents=True)
            shutil.copyfile(ROOT/'scripts/candidate-proxy.py',root/'scripts/candidate-proxy.py')
            (root/'go.mod').write_text('module example.com/provider\n\ngo 1.22\n')
            (root/'provider.go').write_text('package provider\n')
            (root/'tests').mkdir()
            (root/'tests/outer_test.go').write_text('package tests\n')
            for name in ('fixture','consumer'):
                nested=root/'tests'/name
                nested.mkdir()
                (nested/'go.mod').write_text('module example.com/private-'+name+'\n')
                (nested/'private.go').write_text('package private\n')
            subprocess.run(['git','init','-q',str(root)],check=True)
            proxy=pathlib.Path(temp)/'proxy'
            subprocess.run([sys.executable,str(root/'scripts/candidate-proxy.py'),str(proxy)],check=True)
            with zipfile.ZipFile(proxy/'github.com/colony-2/pulse-cloudhypervisor/@v/v0.0.0-candidate.zip') as archive:
                prefix='github.com/colony-2/pulse-cloudhypervisor@v0.0.0-candidate/'
                paths={name.removeprefix(prefix) for name in archive.namelist()}
            self.assertIn('go.mod',paths)
            self.assertIn('provider.go',paths)
            self.assertIn('tests/outer_test.go',paths)
            self.assertFalse(any(p.startswith(('tests/fixture/','tests/consumer/')) for p in paths))

    def test_source_binding(self):
        with tempfile.TemporaryDirectory() as temp:
            root = pathlib.Path(temp)
            subprocess.run(['git','init','-q',temp],check=True)
            a = root/'input'; a.write_text('first'); a.chmod(0o644)
            subprocess.run(['git','add','.'],cwd=root,check=True)
            initial = gate.source_digest(root)
            for change in ('content','mode','symlink','missing'):
                a.unlink(missing_ok=True); a.write_text('first'); a.chmod(0o644)
                if change == 'content': a.write_text('changed')
                elif change == 'mode': a.chmod(0o755)
                elif change == 'symlink': a.unlink(); a.symlink_to('target')
                else: a.unlink()
                self.assertNotEqual(gate.source_digest(root),initial)
            a.write_text('first'); a.chmod(0o644)
            added=root/'new';added.write_text('new')
            self.assertNotEqual(gate.source_digest(root),initial)
            added.unlink()
            self.assertEqual(gate.source_digest(root),initial)
            (root/'.git'/'irrelevant').write_text('metadata')
            self.assertEqual(gate.source_digest(root),initial)


class HookHarness:
    """Actual hook scripts, isolated Git inputs and fake children. No product evidence."""
    def __enter__(self):
        self.arch = 'arm64'
        self.probes = ('^TestNativeHost$', '^TestNativeAllocationAccess$/^TAP$', '^TestNativeAllocationAccess$/^UserManager$')
        self.temp = tempfile.TemporaryDirectory(prefix='ch-gate-test-')
        self.dir = pathlib.Path(self.temp.name)
        self.root = self.dir/'repo'; self.root.mkdir()
        (self.root/'.gitignore').write_text('__pycache__/\n')
        (self.root/'scripts').mkdir(); (self.root/'docs').mkdir(); (self.root/'tests').mkdir()
        for file in ('verify.sh','verify-local.sh','verify.py','verification.py','check-coverage.py','placement.py','recovery_preflight.py','native_outcomes.py','owned_process.py'):
            shutil.copyfile(ROOT/'scripts'/file, self.root/'scripts'/file)
        (self.root/'scripts/managed_fixture.py').write_text(
            "import contextlib,os,pathlib\n"
            "@contextlib.contextmanager\n"
            "def prepared(scratch,confirm,deadline):\n"
            " assert confirm()\n"
            " with open(os.environ['CHILD_INVOCATIONS'],'a') as f: f.write('fixture setup\\n')\n"
            " yield scratch/'fixture', pathlib.Path(os.environ['CLOUDHYPERVISOR_PINS'])\n")
        self.settings = self.dir/'build-settings.json'
        self.settings.write_text('{"variables":{}}')
        self.boot = self.dir/'boot'
        placement = self.root/'scripts/placement.py'
        placement.write_text(placement.read_text().replace("'/proc/sys/kernel/random/boot_id'", repr(str(self.boot)))
                             .replace('QUERY_SECONDS = 10', 'QUERY_SECONDS = 1'))
        verifier = self.root/'scripts/verify.py'
        verifier.write_text(verifier.read_text().replace('budget(deadline, 45)', 'budget(deadline, 0.3)'))
        recovery = self.root/'scripts/recovery_preflight.py'
        recovery.write_text(recovery.read_text().split("if __name__ == '__main__':")[0]+'''
if __name__ == '__main__':
    with open(os.environ['CHILD_INVOCATIONS'], 'a') as invocations:
        invocations.write('el2-check\\n')
    mode = os.environ.get('EL2', 'supported')
    if mode == 'denied':
        sys.exit(1)
    results = {'supported': (12, 1), 'unsupported': (12, 0), 'unproven': (None, None), 'wrong-api': (11, None)}
    api, el2 = results[mode]
    print(json.dumps({'api': api, 'arm_el2': el2, 'status': 'unproven' if mode == 'wrong-api' else mode}))
''')
        (self.root/'scripts/platform.py').write_text("import os\ndef system(): return 'Linux'\ndef machine(): return os.environ.get('HOOK_MACHINE','aarch64')\n")
        shutil.copyfile(ROOT/'docs/cloudhypervisor-tests.md',self.root/'docs/cloudhypervisor-tests.md')
        mapping={k: [['live','TestNative']] if k.startswith('LIVE-') else [['pkg','TestRequired'],['example.com/cloudhypervisor-consumer','TestConsumer']] if k.startswith('SDK-') else [['pkg','TestRequired']] for k in gate.contract()}
        (self.root/'tests/expectations.json').write_text(json.dumps(mapping))
        (self.root/'scripts/verify_test.py').write_text('import os,sys\nwith open(os.environ["CHILD_INVOCATIONS"],"a") as f:f.write("verifier\\n")\nsys.exit(1 if os.environ.get("CHILD_FAIL")=="verifier" else 0)\n')
        self.bin=self.dir/'bin';self.bin.mkdir()
        self.log=self.dir/'invocations'
        self.outcomes=self.dir/'native-outcomes.json'
        self.make(self.bin/'go', '''#!/usr/bin/env python3
import json,os,pathlib,sys
args=sys.argv[1:]
if args==['env','GOVERSION']:print('go1.26.8');sys.exit(0)
with open(os.environ['CHILD_INVOCATIONS'],'a') as f:f.write(' '.join(args)+'\\n')
if os.environ.get('SENTINEL'):print(os.environ['SENTINEL'],file=sys.stderr)
pattern=next((arg for arg in args if arg.startswith('-run=')), '')
kind=('fixture-vet' if 'vet' in args else 'fixture') if args[:2]==['-C','tests/fixture'] else ('tap' if '/^TAP$' in pattern else 'manager' if '/^UserManager$' in pattern else 'host') if '-tags=live' in args else 'local' if '-json' in args else 'race' if '-race' in args else args[0]
if os.environ.get('SLEEP_CHILD')==kind:__import__('time').sleep(5)
if kind=='vet' and os.environ.get('SOURCE_MUTATION'):
 p=pathlib.Path('tracked');change=os.environ['SOURCE_MUTATION']
 if change=='content':p.write_text('changed')
 elif change=='mode':p.chmod(0o755)
 elif change=='symlink':p.unlink();p.symlink_to('docs')
 elif change=='new':pathlib.Path('new-input').write_text('new')
 else:p.unlink()
if '-o' in args:
 pathlib.Path(args[args.index('-o')+1]).write_text('guest-'+os.environ.get('GOARCH',''))
if kind in ('host','tap','manager'):
 package='github.com/colony-2/pulse-cloudhypervisor/tests/live'
 leaves={'host':('KVM','HypervisorControl'),'tap':('TAP',),'manager':('UserManager',)}[kind]
 parent='TestNativeHost' if kind=='host' else 'TestNativeAllocationAccess'
 def emit(action,test=None):
  event=dict(Action=action,Package=package)
  if test:event['Test']=test
  print(json.dumps(event))
 emit('start')
 emit('run',parent)
 if os.environ.get('OMIT_PROBE')!=kind:
  for leaf in leaves:
   emit('run',parent+'/'+leaf)
   emit('pass',parent+'/'+leaf)
 emit('pass',parent)
 if os.environ.get('OMIT_PACKAGE')!=kind:emit('pass')
 if kind==os.environ.get('CHANGE_ON_PROBE','manager') and os.environ.get('CHANGED_BINDING'):
  pathlib.Path(os.environ['BINDING_MARKER']).touch()
  if os.environ['CHANGED_BINDING']=='boot':pathlib.Path(os.environ['BINDING_BOOT']).write_text('00000000-0000-0000-0000-000000000002')
elif kind=='fixture' and '-json' in args:
 package='github.com/colony-2/pulse-cloudhypervisor/tests/fixture'
 mode=os.environ.get('HELPER_EVENTS')
 def emit(action,test=None):
  event=dict(Action=action,Package=package)
  if test:event['Test']=test
  print(json.dumps(event))
 emit('start')
 emit('run','TestS26')
 for leaf in ('profiles','terminal','renewal','retry','replacement'):
  test='TestS26/'+leaf
  if leaf=='renewal' and mode=='missing':continue
  emit('run',test)
  emit(mode if leaf=='renewal' and mode in ('skip','fail') else 'pass',test)
 emit('pass','TestS26')
 if mode!='package':emit('pass')
elif '-json' in args:
 for action,test in [('start',None),('run','TestRequired'),('pass','TestRequired'),('pass',None)]:
  e=dict(Action=action,Package='pkg')
  if test:e['Test']=test
  print(json.dumps(e))
failure=os.environ.get('CHILD_FAIL')
build_kind='build-guest' if '-o' in args and '/guest-' in args[args.index('-o')+1] else 'build-provider'
sys.exit(1 if failure==kind or kind=='build' and failure==build_kind else 0)
''')
        self.make(self.bin/'c2j', '''#!/usr/bin/env python3
import json,os,pathlib,sys
args=sys.argv[1:]
mode=os.environ.get('BINDING')
changed=os.environ.get('CHANGED_BINDING') if pathlib.Path(os.environ['BINDING_MARKER']).exists() else ''
if args[:1]==['test']:
 with open(os.environ['CHILD_INVOCATIONS'],'a') as log:log.write('hook\\n')
 sys.exit(os.environ.get('CHILD_FAIL')=='hook')
if mode=='denied':sys.exit(1)
if mode=='malformed':print('private-malformed');sys.exit(0)
if mode=='oversized':print('private-oversized'*32768);sys.exit(0)
if mode=='stalled':__import__('time').sleep(5)
if args==['self','--json']:
 print(json.dumps(dict(schema_version=1,execution=dict(source='runtime_helper',status='present',node_id='other' if mode=='task' else 'check',op_type='command_execution',task_id='task2' if changed=='task' else 'task1',tenant_id='foreign' if mode=='tenant' else 'tenant1',job_id='foreign' if mode=='job' else 'job1'),lease=dict(status='lost' if mode=='inactive' else 'active',lease_id='private-lease',expires_at='2000-01-01T00:00:00Z' if mode=='expired' else '2099-01-01T00:00:00Z'))))
elif args[:3]==['job','events','self']:
 event=dict(eventType='lease_acquired',eventId='event1',leaseId='wrong' if mode=='lease' else 'private-lease',workerId='other' if mode=='worker' or changed=='worker' else 'worker1',jobKey=dict(jobId='job1',tenantId='tenant1'))
 if mode=='cursor-loop' or mode=='paged' and '--page-token' not in args:print(json.dumps(dict(events=[],nextPageToken='page2')))
 else:print(json.dumps(dict(events=[event,event] if mode=='duplicate' else [event])))
else:sys.exit(1)
''')
        helper = '''import json,os,pathlib,sys
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parent))
from verification import *
import recovery_preflight
import native_outcomes
def events(package,test):
 return dict(version=1,exit_code=0,valid=True,events=[dict(Action='start',Package=package),dict(Action='run',Package=package,Test=test),dict(Action='pass',Package=package,Test=test),dict(Action='pass',Package=package)])
'''
        (self.root/'scripts/synthetic.py').write_text(helper+'''
args=sys.argv[1:]
if args[0]=='consumer':
 with open(os.environ['CHILD_INVOCATIONS'],'a') as f:f.write('consumer\\n')
 if os.environ.get('CHILD_FAIL')=='consumer':sys.exit(1)
 write_json(args[2],events('example.com/cloudhypervisor-consumer','TestConsumer'))
else:
 arch=args[args.index('--arch')+1]
 with open(os.environ['CHILD_INVOCATIONS'],'a') as f:f.write('acceptance --arch '+arch+'\\n')
 run_id=args[args.index('--run-id')+1]
 _, binding=recovery_preflight.validate(args[args.index('--recovery-preflight')+1], 'candidate', source_digest(), run_id)
 if os.environ.get('CHILD_FAIL')=='native':sys.exit(1)
 if os.environ.get('NO_NATIVE_RESULT'):sys.exit(0)
 p=read_json(os.environ['CLOUDHYPERVISOR_PINS'])
 r=dict(version=1,architecture=arch,host_architecture=arch,candidate='candidate',source_sha256=source_digest(),mapping_sha256=digest(ROOT/'tests/expectations.json'),pins=p,selected_assets=p['architectures'][arch],fixture_identity='synthetic-gate-test',guest_agent_sha256=__import__('hashlib').sha256(('guest-'+arch).encode()).hexdigest(),tests=events('live','TestNative'))
 mutation=os.environ.get('NATIVE_MUTATION')
 if mutation:r[mutation] = {} if mutation in ('pins','tests') else 'stale'
 path=pathlib.Path(args[args.index('--result')+1])
 stream=os.environ.get('NATIVE_STREAM','')
 verdict=('complete','coherent_event_stream')
 if stream:
  go_events=events('live','TestNative')['events']
  go_events[-1]['Action']='fail'
  go_events.insert(-1,dict(Action='run',Package='live',Test='TestNativeRebootPreflight'))
  go_events.insert(-1,dict(Action='output',Package='live',Test='TestNativeRebootPreflight',Output='private-native-output'))
  go_events.insert(-1,dict(Action='fail',Package='live',Test='TestNativeRebootPreflight'))
  raw=''.join(json.dumps(event)+'\\n' for event in go_events)
  if stream=='malformed':raw+='private-native-output\\n'
  audit=native_outcomes.NativeAudit({'live'})
  r['tests']=collect([sys.executable,'-c','import sys; sys.stdout.write(sys.argv[1]); sys.exit(1)',raw],{'LIVE-03':[['live','TestNative']]},audit=audit)
  verdict=audit.integrity(r['tests']['exit_code'])
 write_json(path,r)
 native_outcomes.bind(path,binding,run_id,verdict)
 if stream=='tampered':
  with path.open('a') as output:output.write(' ')
 sys.exit(1 if stream else 0)
''')
        (self.root/'scripts/external-consumer.sh').write_text('#!/bin/sh\nexec python3 scripts/synthetic.py consumer "$@"\n')
        (self.root/'scripts/acceptance.sh').write_text('#!/bin/sh\nexec python3 scripts/synthetic.py native "$@"\n')
        (self.root/'tracked').write_text('initial')
        subprocess.run(['git','init','-q',str(self.root)],check=True)
        subprocess.run(['git','add','.'],cwd=self.root,check=True)
        self.pinfile=self.dir/'pins.json';self.pinfile.write_text(json.dumps(pins()))
        self.log.write_text('')
        return self

    def make(self,path,text):
        path.write_text(text);path.chmod(0o700)

    def run(self,local=False,args=(),**changes):
        self.log.write_text('')
        self.outcomes.unlink(missing_ok=True)
        self.boot.write_text('00000000-0000-0000-0000-000000000001')
        marker = self.dir/'changed'; marker.unlink(missing_ok=True)
        env=dict(os.environ,PATH=str(self.bin)+':'+os.environ['PATH'],CANDIDATE='candidate',CLOUDHYPERVISOR_PINS=str(self.pinfile), CH_NATIVE_OUTCOMES=str(self.outcomes),
                 CLOUDHYPERVISOR_BUILD_SETTINGS=str(self.settings), CHILD_INVOCATIONS=str(self.log),CHILD_FAIL='',NATIVE_MUTATION='',HOOK_MACHINE='aarch64',
                 BINDING_MARKER=str(marker),BINDING_BOOT=str(self.boot),C2J_CURRENT_TENANT_ID='tenant1',C2J_CURRENT_JOB_ID='job1',
                 PULSE_LAUNCH_ID='worker1',C2J_EXECUTION_ALLOCATION=json.dumps(dict(schema_version=2,base='ubuntu-24.04-arm64',platform='linux/arm64',capabilities=['kvm'])))
        env.update(changes)
        return subprocess.run(['bash',str(self.root/'scripts'/('verify-local.sh' if local else 'verify.sh')), *args],env=env,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=30)

    def __exit__(self,*args):
        self.temp.cleanup()


if __name__ == '__main__':
    unittest.main()
