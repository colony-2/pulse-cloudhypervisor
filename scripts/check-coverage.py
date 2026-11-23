#!/usr/bin/env python3
"""Aggregate current local checks and exactly one candidate-bound ARM64 native receipt."""
import argparse
import sys
import subprocess
from verification import (ROOT, contract, digest, read_json, require, source_digest,
                          split_mapping, validate_local, validate_native)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--events', required=True)
    parser.add_argument('--consumer', required=True)
    parser.add_argument('--native', action='append', required=True)
    parser.add_argument('--pins', required=True)
    parser.add_argument('--candidate', required=True)
    parser.add_argument('--source', required=True)
    parser.add_argument('--guest-agent-arm64', required=True)
    args = parser.parse_args()
    mapping = contract()
    local, live = split_mapping(mapping)
    validate_local(local, read_json(args.events), read_json(args.consumer))
    require(args.source == source_digest())
    pins = read_json(args.pins)
    require(subprocess.check_output(['go', 'env', 'GOVERSION'], text=True).strip() == pins['common']['go_version'])
    validate_native([read_json(path) for path in args.native], args.candidate, args.source,
                    digest(ROOT/'tests/expectations.json'), pins, live,
                    {a: digest(getattr(args, 'guest_agent_'+a)) for a in ('arm64',)})
    print('PASS: all 89 mappings; current local, consumer and ARM64 native result')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, OSError, TypeError, UnicodeError):
        print('FAIL: missing, failed, skipped or mismatched candidate verification')
        sys.exit(1)
