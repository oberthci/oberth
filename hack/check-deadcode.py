#!/usr/bin/env python3
"""Reject new unreachable production functions and stale allowlist entries."""
import json
import sys
from pathlib import Path


def check(report, allowed):
    if not isinstance(report, list) or not isinstance(allowed, dict):
        raise ValueError('deadcode report must be an array and allowlist an object')
    for name, reason in allowed.items():
        if not isinstance(name, str) or not isinstance(reason, str) or not reason.strip():
            raise ValueError('every exception needs an exact symbol and a reason')
    found = set()
    for package in report:
        for function in package['Funcs']:
            symbol = package['Path'] + '.' + function['Name']
            if symbol in found:
                raise ValueError('duplicate deadcode symbol: ' + symbol)
            found.add(symbol)
    return sorted(found - allowed.keys()), sorted(allowed.keys() - found)


def main():
    try:
        report = json.load(sys.stdin)
        allowed = json.loads(Path(sys.argv[1]).read_text())
        unexpected, stale = check(report, allowed)
    except (ValueError, KeyError, TypeError, OSError, IndexError) as error:
        print('invalid deadcode input: ' + str(error), file=sys.stderr)
        return 1
    for symbol in unexpected:
        print('unallowlisted unreachable function: ' + symbol, file=sys.stderr)
    for symbol in stale:
        print('remove stale deadcode exception: ' + symbol, file=sys.stderr)
    if unexpected or stale:
        return 1
    print(f'deadcode: no new debt ({len(allowed)} explicit exceptions)')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
