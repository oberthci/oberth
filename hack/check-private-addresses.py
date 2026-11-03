#!/usr/bin/env python3
"""Reject deployment-specific private IPv4 literals in tracked files."""
from pathlib import Path
import re
import subprocess
import sys


PREFIX = b'192' + b'.168.'
ADDRESS = re.compile(rb'(?<![0-9])' + re.escape(PREFIX) +
                     rb'[0-9]{1,3}\.[0-9]{1,3}(?![0-9])')
# Exact path/value exceptions for private-network classification. New fixture
# addresses should use RFC 5737 documentation ranges instead.
ALLOWLIST = {
    'internal/installer/installer.go': {PREFIX + b'0.0'},
    'internal/installer/installer_test.go': {PREFIX + b'1.100'},
}


def addresses(data):
    return {match[0] for match in ADDRESS.finditer(data)
            if all(int(part) <= 255 for part in match[0].split(b'.'))}


def check(root):
    root = Path(root).resolve()
    paths = subprocess.run(
        ['git', '-c', 'safe.directory=' + str(root), 'ls-files', '-z'],
        cwd=root, check=True, capture_output=True).stdout.split(b'\0')
    bad = []
    for path in filter(None, paths):
        name = path.decode()
        if (addresses(path) or
                addresses((root / name).read_bytes()) - ALLOWLIST.get(name, set())):
            bad.append(name)
    if bad:
        print('Private IPv4 literals found outside the explicit allowlist:', file=sys.stderr)
        print('\n'.join(bad), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(check(Path(__file__).resolve().parents[1]))
