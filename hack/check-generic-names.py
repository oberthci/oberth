#!/usr/bin/env python3
"""Refuse retired branding in tracked paths and file contents."""
from pathlib import Path
import subprocess
import sys

FORBIDDEN = 'cloud' + 'taser'


def check(root):
    paths = subprocess.run(['git', 'ls-files', '-z'], cwd=root, check=True,
                           capture_output=True).stdout.split(b'\0')
    bad_paths = [p for p in paths if FORBIDDEN.encode() in p.lower()]
    result = subprocess.run(['git', 'grep', '-i', '-l', '-F', '-e', FORBIDDEN, '--', '.'],
                            cwd=root, capture_output=True)
    if result.returncode not in (0, 1):
        raise RuntimeError('tracked-file name check could not run')
    if bad_paths or result.returncode == 0:
        print('Retired product name found in tracked files; use generic names.', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(check(Path(__file__).resolve().parents[1]))
