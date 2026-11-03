#!/usr/bin/env python3
"""Refuse retired branding in tracked paths and file contents."""
from pathlib import Path
import ast
import re
import subprocess
import sys

PARTS = ('cloud', 'taser')
FORBIDDEN = ''.join(PARTS)
PATTERN = r'[[:space:]_.-]{0,3}'.join(PARTS)
PATH_PATTERN = re.compile(rb'[\s_.-]{0,3}'.join(p.encode() for p in PARTS), re.I)


def literal_value(node):
    """Decode static string expressions without executing source code."""
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    if isinstance(node, (ast.Tuple, ast.List)):
        values = [literal_value(item) for item in node.elts]
        return values if all(isinstance(item, str) for item in values) else None
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        left, right = literal_value(node.left), literal_value(node.right)
        if isinstance(left, str) and isinstance(right, str):
            return left + right
    if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
            and node.func.attr == 'join' and len(node.args) == 1 and not node.keywords):
        separator = literal_value(node.func.value)
        values = literal_value(node.args[0])
        if isinstance(separator, str) and isinstance(values, list):
            return separator.join(values)
    return None


def assembled_name(source):
    try:
        tree = ast.parse(source)
    except (SyntaxError, ValueError):
        return False
    for node in ast.walk(tree):
        value = literal_value(node)
        if isinstance(value, str) and PATH_PATTERN.search(value.encode()):
            return True
    return False


def check(root):
    root = Path(root).resolve()
    git = ['git', '-c', 'safe.directory=' + str(root)]
    paths = subprocess.run([*git, 'ls-files', '-z'], cwd=root, check=True,
                           capture_output=True).stdout.split(b'\0')
    bad_paths = [p for p in paths if PATH_PATTERN.search(p)]
    assembled = any(assembled_name((root / path.decode()).read_bytes())
                    for path in paths if path.endswith(b'.py') and (root / path.decode()).is_file())
    result = subprocess.run([*git, 'grep', '-i', '-l', '-E', '-e', PATTERN, '--', '.'],
                            cwd=root, capture_output=True)
    if result.returncode not in (0, 1):
        raise RuntimeError('tracked-file name check could not run')
    if bad_paths or assembled or result.returncode == 0:
        print('Retired product name found in tracked files; use generic names.', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(check(Path(__file__).resolve().parents[1]))
