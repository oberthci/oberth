"""Public source binding; importing this module performs no work."""
from pathlib import Path
import hashlib
import os
from watch_csa_admission import public_json


def refuse():
    raise RuntimeError('watch fixture source binding refused')


def digest(path):
    fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
    with os.fdopen(fd, 'rb') as file:
        return hashlib.file_digest(file, 'sha256').hexdigest()


def source_guard(packet_path, expected):
    packet, actual = public_json(packet_path, 131072)
    if actual != expected or packet.get('base') != '4676484d826295ff7e668849b69adfb8f57e85c5':
        refuse()
    root = Path(packet_path).parent / 'source'
    for name, row in packet['files'].items():
        relative = Path(name)
        if relative.is_absolute() or '..' in relative.parts or digest(root / relative) != row['sha256']:
            refuse()
    return packet, root
