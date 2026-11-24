#!/usr/bin/env python3
"""Aggregate current local checks and candidate-bound native receipts."""
import argparse
import sys
import subprocess
from verification import (ARCHES, ROOT, contract, digest, read_json, require, source_digest,
                          split_mapping, validate_local, validate_native)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--events', required=True)
    parser.add_argument('--consumer', required=True)
    parser.add_argument('--native', action='append', required=True)
    parser.add_argument('--pins', required=True)
    parser.add_argument('--candidate', required=True)
    parser.add_argument('--source', required=True)
    parser.add_argument('--host-run', choices=sorted(ARCHES), help='verify a single host contribution; omitted for release qualification')
    for arch in sorted(ARCHES):
        parser.add_argument('--guest-agent-'+arch)
    args = parser.parse_args()
    mapping = contract()
    local, live = split_mapping(mapping)
    validate_local(local, read_json(args.events), read_json(args.consumer))
    require(args.source == source_digest())
    pins = read_json(args.pins)
    require(subprocess.check_output(['go', 'env', 'GOVERSION'], text=True).strip() == pins['common']['go_version'])
    expected = {args.host_run} if args.host_run else ARCHES
    require(all(getattr(args, 'guest_agent_'+a) for a in expected))
    validate_native([read_json(path) for path in args.native], args.candidate, args.source,
                    digest(ROOT/'tests/expectations.json'), pins, live,
                    {a: digest(getattr(args, 'guest_agent_'+a)) for a in expected}, expected)
    print(f'PASS: all {len(mapping)} mappings; current local, consumer and native results for {sorted(expected)}')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, TypeError, UnicodeError):
        print('FAIL: missing, failed, skipped or mismatched candidate verification')
        sys.exit(1)
