#!/usr/bin/env python3
"""Refuse retired branding, installation labels and workstation tool paths."""
from functools import lru_cache
import hashlib
from pathlib import Path
import re
import subprocess
import sys

# SHA-256 of lowercase alphanumeric labels. Keep the labels themselves out of
# public source, including the gate and its negative controls.
BRAND_DIGESTS = {
    10: {
        'ae87e18457eeee44eb012a45dfc3b261413407941de02c96064bff4f442d30bb',
    },
}
# Retain the distinctive-fragment rule without publishing its label.
FRAGMENT_DIGESTS = {
    5: {
        'de3fa92e1bec4d9540740be90cee4f04c71c93f3a329d6f4caf5b46493b3cda4',
    },
}
INSTANCE_DIGESTS = {
    6: {
        '38a5015a3b8ccd8f080fe05a085be6395a210e4dad37390c42f647f1329afe4a',
        'd91a9253d1a718cc1daa55047530c896802d8daffce0e2791672dda02e937f5f',
    },
    8: {
        'e5ab8e8e867614604d8b0e99ecfb8f0f2640b94ada62afefa56f61b5439a12d7',
    },
}
LEGACY_DIGESTS = {
    4: {
        '700a82bdbd221a7d92487552f0b9f4e39627a19f34a7ad2dfd0da3cd1048b7b2',
    },
    6: {
        '8a62e967fcd6dfa5d75308c37808b4668a7faf1cdb06e09ac0a7161827603887',
    },
    7: {
        '9fc01603cc42044f0ec14f7a24443098f40e565d173c0021711577f30ce4ff4f',
        'ddf50e466ba0b03136df02a019c0dd65fc11b83c5b504e93f620bb8c5864b408',
    },
    11: {
        'eb5df3aaccdecd003675124859ce47d15a1137b20db078e45a0e66f21a9bf58f',
    },
}
# Legal attribution and the retained endpoint migration contract predate this
# sweep. Only legacy labels are allowed here; host labels are always rejected.
LEGACY_FILES = {'LICENSE', 'cmd/oberth/mcp_legacy_default.go',
                'cmd/oberth/mcp_environment_test.go'}
HEX_ESCAPE = re.compile(rb'\\(?:x([0-9a-f]{2})|u00([0-9a-f]{2}))', re.I)
LITERAL_JOIN = re.compile(rb"[\"']\s*(?:[+,]\s*)?[\"']")
WORD = re.compile(rb'[a-z0-9]+')
BRAND_SPAN = re.compile(rb'[a-z0-9]+(?:[\s_.-]{1,3}[a-z0-9]+)*')
SEPARATOR = re.compile(rb'[\s_.-]{1,3}')
WORKSTATION_PATH = re.compile(
    rb'/(?:home|users)/[^/\s]+/(?:work/|go/pkg/mod/|\.local/share/kubebuilder-envtest/)')


def normalize(data):
    data = HEX_ESCAPE.sub(lambda m: bytes([int(m[1] or m[2], 16)]), data)
    return data.lower()


@lru_cache(maxsize=65536)
def digest(value):
    return hashlib.sha256(value).hexdigest()


def forbidden(data, allow_legacy=False):
    data = normalize(data)
    joined = LITERAL_JOIN.sub(b'', data)
    # Inspect both views: joining a list of complete labels must not hide one
    # by merging it with its neighbor. Joining also exposes split literals.
    return (forbidden_view(data, allow_legacy) or
            (joined != data and forbidden_view(joined, allow_legacy)))


def forbidden_view(data, allow_legacy):
    if WORKSTATION_PATH.search(data):
        return True
    words = list(WORD.finditer(data))
    for word in words:
        value = word[0]
        for length, hashes in FRAGMENT_DIGESTS.items():
            if any(digest(value[i:i + length]) in hashes
                   for i in range(len(value) - length + 1)):
                return True
    # Product spelling is forbidden inside longer words, including when its
    # pieces straddle separators inside those words.
    for span in BRAND_SPAN.finditer(data):
        value = re.sub(rb'[\s_.-]', b'', span[0])
        for length, hashes in BRAND_DIGESTS.items():
            if any(digest(value[i:i + length]) in hashes
                   for i in range(len(value) - length + 1)):
                return True
    instances = {length: set(hashes) for length, hashes in INSTANCE_DIGESTS.items()}
    if not allow_legacy:
        for length, hashes in LEGACY_DIGESTS.items():
            instances.setdefault(length, set()).update(hashes)
    max_length = max(instances)
    for index, word in enumerate(words):
        value = word[0]
        # Installation labels are whole tokens. This preserves official project
        # IDs containing a short label as a prefix of a longer identifier.
        end = word.end()
        for following in range(index, len(words)):
            if len(value) > max_length:
                break
            if digest(value) in instances.get(len(value), ()):
                return True
            if following + 1 == len(words):
                break
            next_word = words[following + 1]
            if not SEPARATOR.fullmatch(data[end:next_word.start()]):
                break
            value += next_word[0]
            end = next_word.end()
    return False


def check(root):
    root = Path(root).resolve()
    git = ['git', '-c', 'safe.directory=' + str(root)]
    paths = subprocess.run([*git, 'ls-files', '-z'], cwd=root, check=True,
                           capture_output=True).stdout.split(b'\0')
    bad = []
    for path in filter(None, paths):
        name = path.decode()
        if (forbidden(path) or
                forbidden((root / name).read_bytes(), allow_legacy=name in LEGACY_FILES)):
            bad.append(path.decode(errors='replace'))
    if bad:
        print('Non-generic names or workstation paths found in tracked files:', file=sys.stderr)
        print('\n'.join(bad), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(check(Path(__file__).resolve().parents[1]))
