#!/usr/bin/env python3
"""Run a Go suite, exporting recognized events only, without raw stdout/stderr."""
import argparse
import sys
from verification import collect, contract, write_json

if __name__ == '__main__':
    try:
        parser = argparse.ArgumentParser()
        parser.add_argument('--events', required=True)
        parser.add_argument('command', nargs=argparse.REMAINDER)
        args = parser.parse_args()
        command = args.command
        if command and command[0] == '--':
            command = command[1:]
        result = collect(command, contract())
        write_json(args.events, result)
        sys.exit(0 if result['exit_code'] == 0 and result['valid'] else 1)
    except (ValueError, KeyError, OSError, TypeError, UnicodeError):
        print('FAIL: test event collection')
        sys.exit(1)
