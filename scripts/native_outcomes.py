"""Audit complete native streams before retaining filtered failure evidence."""
import os
import pathlib
import tempfile

from verification import MAX_EVENTS, decode, digest, read_json, require, write_json

TERMINAL = {'pass', 'fail', 'skip'}


class NativeAudit:
    def __init__(self, packages):
        self.expected = frozenset(packages)
        self.packages = {}
        self.tests = {}
        self.active = {}
        self.failed = set()
        self.count = 0
        self.damaged = False

    def feed(self, line):
        self.count += 1
        if self.damaged:
            return
        try:
            require(self.count <= MAX_EVENTS and len(line) <= 1024 * 1024 and line.endswith(b'\n'))
            event = decode(line)
            require(isinstance(event, dict))
            action, package, test = (event.get(key) for key in ('Action', 'Package', 'Test'))
            require(isinstance(action, str) and isinstance(package, str) and 0 < len(package) < 300)
            require('Test' not in event or isinstance(test, str) and 0 < len(test) < 300)
            require('Output' not in event or isinstance(event['Output'], str))
            if action == 'start':
                require(test is None and package not in self.packages)
                self.packages[package] = 'active'
                self.active[package] = 0
                return
            require(self.packages.get(package) == 'active')
            if action == 'output':
                return
            if test is None:
                require(action in TERMINAL and self.active[package] == 0)
                require(action == 'fail' or package not in self.failed)
                self.packages[package] = action
                return
            key = package, test
            previous = self.tests.get(key)
            if action == 'run':
                require(previous is None)
                self.tests[key] = 'running'
                self.active[package] += 1
            elif action == 'pause':
                require(previous == 'running')
                self.tests[key] = 'paused'
            elif action == 'cont':
                require(previous == 'paused')
                self.tests[key] = 'running'
            else:
                require(action in TERMINAL and previous == 'running')
                self.tests[key] = action
                self.active[package] -= 1
                if action == 'fail':
                    self.failed.add(package)
        except (ValueError, KeyError, TypeError, UnicodeError):
            self.damaged = True

    def integrity(self, code):
        if self.damaged:
            return 'damaged', 'invalid_event_stream'
        if type(code) is not int or code < 0:
            return 'damaged', 'interrupted_collection'
        if not self.expected or not self.expected.issubset(self.packages) or 'active' in self.packages.values():
            return 'damaged', 'incomplete_event_stream'
        if (code != 0) != ('fail' in self.packages.values()):
            return 'damaged', 'inconsistent_exit_status'
        return 'complete', 'coherent_event_stream'


VERDICTS = {
    'complete': {'coherent_event_stream'},
    'damaged': {'invalid_event_stream', 'interrupted_collection', 'incomplete_event_stream', 'inconsistent_exit_status'},
}


def sidecar_path(receipt):
    return pathlib.Path(receipt).with_suffix('.integrity.json')


def started_path(receipt):
    return pathlib.Path(receipt).with_suffix('.started.json')


def bind(receipt, binding, run, verdict):
    integrity, reason = verdict
    require(reason in VERDICTS.get(integrity, ()))
    write_json(sidecar_path(receipt), {'version': 1, 'identity': binding, 'run': run,
                                     'receipt_sha256': digest(receipt), 'integrity': integrity, 'reason': reason})


def report(integrity='not_run', reason='collector_not_started', preflight='not_run'):
    return {'version': 1, 'integrity': integrity, 'reason': reason,
            'preflight': {'arm_el2': preflight, 'LIVE-09': 'blocked' if preflight in ('unsupported', 'unproven') else 'unverified'}}


def project(receipt, binding, run, mapping, preflight):
    try:
        if not any(path.exists() for path in (pathlib.Path(receipt), sidecar_path(receipt), started_path(receipt))):
            return report(preflight=preflight)
        sidecar = read_json(sidecar_path(receipt))
        require(isinstance(sidecar, dict) and set(sidecar) == {'version', 'identity', 'run', 'receipt_sha256', 'integrity', 'reason'})
        require(type(sidecar['version']) is int and sidecar['version'] == 1 and sidecar['identity'] == binding and sidecar['run'] == run)
        require(sidecar['receipt_sha256'] == digest(receipt))
        verdict, reason = sidecar['integrity'], sidecar['reason']
        require(reason in VERDICTS.get(verdict, ()))
        result = report(verdict, reason, preflight)
        if verdict != 'complete':
            return result
        data = read_json(receipt)['tests']
        require(isinstance(data, dict) and type(data['exit_code']) is int and data['exit_code'] >= 0)
        terminals = {(event['Package'], event['Test']): event['Action'] for event in data['events']
                     if 'Test' in event and event['Action'] in TERMINAL}
        results = {}
        for case, pairs in mapping.items():
            if case == 'LIVE-09' and preflight != 'supported':
                continue
            states = [terminals.get(tuple(pair)) for pair in pairs]
            if 'fail' in states:
                results[case] = 'fail'
            elif 'skip' in states:
                results[case] = 'skip'
            elif states and all(outcome == 'pass' for outcome in states):
                results[case] = 'pass'
        result['outcomes'] = results
        return result
    except (ValueError, KeyError, OSError, TypeError, UnicodeError):
        return report('untrusted', 'missing_or_mismatched_binding', preflight)


def publish(path, result):
    if not path:
        return
    path = pathlib.Path(path)
    require(path.is_absolute())
    with tempfile.TemporaryDirectory(prefix='.native-outcomes-', dir=path.parent) as scratch:
        temporary = pathlib.Path(scratch)/'report.json'
        write_json(temporary, result)
        os.replace(temporary, path)
