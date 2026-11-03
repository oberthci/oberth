#!/usr/bin/env python3
"""Refuse retired branding in tracked paths and file contents."""
from pathlib import Path
import re
import subprocess
import sys

PARTS = ('cloud', 'taser')
FORBIDDEN = ''.join(PARTS)
PATTERN = r'[[:space:]_.-]{0,3}'.join(PARTS)
PATH_PATTERN = re.compile(rb'[\s_.-]{0,3}'.join(p.encode() for p in PARTS), re.I)

INSTANCE_NAMES = [('tux', 'box'), ('skip', 'ops'), ('ct', 'tv'), ('bea', 'con'),
                  ('codex-', 'lab'), ('demo-', 'lab'), ('watch.', 'oberth')]
INSTANCE_PATTERN = r'(^|[^a-zA-Z0-9])(' + '|'.join(re.escape(''.join(p)) for p in INSTANCE_NAMES) + r')([^a-zA-Z0-9]|$)'
RELEASE_PATHS = [':(exclude)LICENSE', ':(exclude).oberth/**', ':(exclude)scripts/release-wif-credential.py',
                 ':(exclude)scripts/test_release_wif.py']


def check(root):
    root = Path(root).resolve()
    git = ['git', '-c', 'safe.directory=' + str(root)]
    paths = subprocess.run([*git, 'ls-files', '-z'], cwd=root, check=True,
                           capture_output=True).stdout.split(b'\0')
    bad_paths = [p for p in paths if PATH_PATTERN.search(p)]
    result = subprocess.run([*git, 'grep', '-i', '-l', '-E', '-e', PATTERN, '--', '.'],
                            cwd=root, capture_output=True)
    if result.returncode not in (0, 1):
        raise RuntimeError('tracked-file name check could not run')
    instance = subprocess.run([*git, 'grep', '-i', '-l', '-E', '-e', INSTANCE_PATTERN,
                               '--', '.', *RELEASE_PATHS], cwd=root, capture_output=True)
    if instance.returncode not in (0, 1):
        raise RuntimeError('tracked-file instance check could not run')
    if instance.returncode == 0:
        print('Installation-specific names found in tracked product files:', file=sys.stderr)
        print(instance.stdout.decode(errors='replace'), file=sys.stderr)
        return 1
    if bad_paths or result.returncode == 0:
        print('Retired product name found in tracked files; use generic names.', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(check(Path(__file__).resolve().parents[1]))
