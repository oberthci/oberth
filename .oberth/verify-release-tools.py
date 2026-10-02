#!/usr/bin/env python3
"""Verify publisher code and executable bytes before secretstore runs."""
import hashlib
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

TOOLS = frozenset(('cosign', 'helm', 'trivy', 'oberth-release-support', 'oberth-release-image'))
SOURCE_FILES = ('.oberth/release.sh', '.oberth/pins/bootstrap-tools.sha256',
                '.oberth/pins/release-cosign.pub', '.oberth/verify-release-tools.py')


class VerificationError(Exception):
    """Fixed diagnostics only; never relay command output or file contents."""


def directory(path):
    if not stat.S_ISDIR(os.lstat(path).st_mode):
        raise VerificationError('tool directory is not a real directory')


def regular_bytes(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise VerificationError('expected a regular source file')
        body = stream.read(limit + 1)
    if len(body) > limit:
        raise VerificationError('source file exceeds size limit')
    return body


def verify_source(source, sha, tag):
    if not re.fullmatch(r'[0-9a-f]{40}', sha):
        raise VerificationError('expected a full admitted source SHA')
    if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?', tag):
        raise VerificationError('invalid admitted release tag')
    directory(source)
    directory(source / '.oberth')
    directory(source / '.oberth/pins')

    def git(*args):
        result = subprocess.run(['/usr/bin/git', '--no-replace-objects', '-C', str(source),
                                 '-c', 'safe.directory=' + str(source), *args],
                                check=True, capture_output=True, timeout=30,
                                env={'PATH': '/usr/bin:/bin', 'HOME': '/tmp',
                                     'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null'})
        return result.stdout

    for ref in ('HEAD', 'refs/tags/' + tag + '^{commit}'):
        if git('rev-parse', '--verify', ref).strip() != sha.encode('ascii'):
            raise VerificationError('source or tag differs from admitted release SHA')
    # Compare committed blobs directly, independent of the index's stat cache
    # and assume-unchanged flags. These are the only repository-authored inputs
    # the credentialed release shell and its pin verifier execute/read.
    for relative in SOURCE_FILES:
        if git('cat-file', 'blob', sha + ':' + relative) != regular_bytes(source / relative, 1024 * 1024):
            raise VerificationError('publisher source bytes differ from admitted commit')


def verify_tools(root, manifest):
    directory(root)
    directory(root / 'bin')
    expected = {}
    for line in manifest.decode('ascii').splitlines():
        match = re.fullmatch(r'([0-9a-f]{64})  ([a-z-]+)', line)
        if not match or match[2] not in TOOLS or match[2] in expected:
            raise VerificationError('publisher tool pin manifest is malformed')
        expected[match[2]] = match[1]
    if set(expected) != TOOLS:
        raise VerificationError('publisher tool pin manifest is incomplete')
    for name, wanted in expected.items():
        fd = os.open(root / 'bin' / name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, 'rb') as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or not info.st_mode & 0o111:
                raise VerificationError('publisher tool is not a regular executable')
            if not 0 < info.st_size <= 512 * 1024 * 1024:
                raise VerificationError('publisher tool exceeds size bounds')
            digest = hashlib.file_digest(stream, 'sha256').hexdigest()
            if digest != wanted:
                raise VerificationError('publisher tool bytes differ from reviewed pin: ' + name)


def main():
    source = Path('/work/src')
    verify_source(source, os.environ.get('OBERTH_RELEASE_SHA', ''), os.environ.get('OBERTH_RELEASE_TAG', ''))
    verify_tools(Path('/tmp/oberth-tools'), regular_bytes(source / SOURCE_FILES[1], 16384))
    print('publisher source and tool pins verified before secretstore')


if __name__ == '__main__':
    try:
        main()
    except VerificationError as error:
        sys.exit('publisher verifier: ' + str(error))
    except (OSError, ValueError, UnicodeError, subprocess.SubprocessError):
        sys.exit('publisher verifier: invalid input or failed verification')
