#!/usr/bin/env python3
"""Verify website publication inputs before the release-website secretstore runs.

This is the first init container of the credentialed release-website leaf. No
secret exists in the Pod yet: `oberth secretstore exec` runs only in the main
container, after every init container succeeded.

It proves that the publisher source this leaf executes or uploads is exactly
the admitted commit and tag: release.sh, this verifier, the Node pins, the npm
manifest and lockfile, wrangler.jsonc, and the complete website/public tree
(every tracked file byte-identical, nothing untracked, no symlinks). It then
copies the two inputs the credential-free setup steps staged in the
bootstrap-website-inputs claim into Pod-private scratch:

- the Node tarball, hashed while copied and compared with the reviewed pin;
- the npm cache, whose content the next init container re-verifies with
  `npm ci --offline` against the committed lockfile's sha512 integrity.

Nothing executable crosses Pods: the claim carries only hash-checked inputs,
and every later step of this leaf uses the Pod-private copies.
"""
import hashlib
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

SOURCE_FILES = ('.oberth/release.sh', '.oberth/verify-website-inputs.py',
                '.oberth/pins/node.url', '.oberth/pins/node.sha256',
                '.oberth/pins/node-bin.sha256', 'website/package.json',
                'website/package-lock.json', 'website/wrangler.jsonc')
PUBLIC_TREE = 'website/public'
NODE_PIN_PATH = '/tmp/oberth-website-inputs/node.tar.gz'
NODE_URL = re.compile(r'https://nodejs\.org/dist/v[0-9]+\.[0-9]+\.[0-9]+/node-v[0-9]+\.[0-9]+\.[0-9]+-linux-x64\.tar\.gz')
MAX_SOURCE_BYTES = 16 * 1024 * 1024
MAX_TARBALL_BYTES = 256 * 1024 * 1024
MAX_CACHE_BYTES = 1024 * 1024 * 1024
MAX_CACHE_ENTRIES = 100000


class VerificationError(Exception):
    """Fixed diagnostics only; never relay command output or file contents."""


def directory(path):
    if not stat.S_ISDIR(os.lstat(path).st_mode):
        raise VerificationError('expected a real directory')


def regular_bytes(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise VerificationError('expected a regular source file')
        body = stream.read(limit + 1)
    if len(body) > limit:
        raise VerificationError('source file exceeds size limit')
    return body


def git(source, *args):
    result = subprocess.run(['/usr/bin/git', '--no-replace-objects', '-C', str(source),
                             '-c', 'safe.directory=' + str(source), *args],
                            check=True, capture_output=True, timeout=60,
                            env={'PATH': '/usr/bin:/bin', 'HOME': '/tmp',
                                 'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null'})
    return result.stdout


def tracked_public_tree(source, sha):
    """Return {relative path: blob id} for every file under website/public."""
    listing = git(source, 'ls-tree', '-r', '-z', '--full-tree', sha, '--', PUBLIC_TREE)
    tracked = {}
    for record in listing.split(b'\0'):
        if not record:
            continue
        meta, _, name = record.partition(b'\t')
        mode, kind, blob = meta.decode('ascii').split(' ')
        if kind != 'blob' or mode not in ('100644', '100755'):
            raise VerificationError('published tree may contain only regular files')
        tracked[name.decode('utf-8')] = blob
    if not tracked:
        raise VerificationError('published tree is empty')
    return tracked


def verify_public_tree(source, sha):
    tracked = tracked_public_tree(source, sha)
    root = source / PUBLIC_TREE
    directory(root)
    present = set()
    for current, directories, files in os.walk(root, followlinks=False):
        for name in directories:
            if stat.S_ISLNK(os.lstat(os.path.join(current, name)).st_mode):
                raise VerificationError('published tree contains a directory symlink')
        for name in files:
            path = os.path.join(current, name)
            present.add(os.path.relpath(path, source))
    if present != set(tracked):
        raise VerificationError('published tree differs from the admitted commit')
    for relative in sorted(tracked):
        if git(source, 'cat-file', 'blob', sha + ':' + relative) != regular_bytes(source / relative, MAX_SOURCE_BYTES):
            raise VerificationError('published file bytes differ from admitted commit')


def verify_source(source, sha, tag):
    if not re.fullmatch(r'[0-9a-f]{40}', sha):
        raise VerificationError('expected a full admitted source SHA')
    if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?', tag):
        raise VerificationError('invalid admitted release tag')
    directory(source)
    for ref in ('HEAD', 'refs/tags/' + tag + '^{commit}'):
        if git(source, 'rev-parse', '--verify', ref).strip() != sha.encode('ascii'):
            raise VerificationError('source or tag differs from admitted release SHA')
    for relative in SOURCE_FILES:
        if git(source, 'cat-file', 'blob', sha + ':' + relative) != regular_bytes(source / relative, MAX_SOURCE_BYTES):
            raise VerificationError('publisher source bytes differ from admitted commit')
    verify_public_tree(source, sha)


def node_pin(source):
    url = regular_bytes(source / '.oberth/pins/node.url', 4096).decode('ascii')
    if not NODE_URL.fullmatch(url.rstrip('\n')) or url.count('\n') != 1:
        raise VerificationError('Node URL pin is malformed')
    match = re.fullmatch(r'([0-9a-f]{64})  (\S+)\n',
                         regular_bytes(source / '.oberth/pins/node.sha256', 4096).decode('ascii'))
    if not match or match[2] != NODE_PIN_PATH:
        raise VerificationError('Node tarball pin is malformed')
    return match[1]


def copy_verified(source_path, destination, wanted):
    fd = os.open(source_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= MAX_TARBALL_BYTES:
            raise VerificationError('staged Node tarball is not a bounded regular file')
        out = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        digest = hashlib.sha256()
        copied = 0
        with os.fdopen(out, 'wb') as sink:
            while chunk := stream.read(1024 * 1024):
                copied += len(chunk)
                if copied > MAX_TARBALL_BYTES:
                    raise VerificationError('staged Node tarball exceeds size bounds')
                digest.update(chunk)
                sink.write(chunk)
    if digest.hexdigest() != wanted:
        os.unlink(destination)
        raise VerificationError('staged Node tarball differs from the reviewed pin')


def copy_tree(source_root, destination):
    """Copy a directory tree of regular files; refuse links and special files."""
    directory(source_root)
    os.mkdir(destination, 0o700)
    budget = {'bytes': 0, 'entries': 0}

    def walk(current, target):
        with os.scandir(current) as entries:
            for entry in entries:
                budget['entries'] += 1
                if budget['entries'] > MAX_CACHE_ENTRIES:
                    raise VerificationError('staged npm cache exceeds entry bounds')
                mode = os.lstat(entry.path).st_mode
                path = os.path.join(target, entry.name)
                if stat.S_ISDIR(mode):
                    os.mkdir(path, 0o700)
                    walk(entry.path, path)
                elif stat.S_ISREG(mode):
                    fd = os.open(entry.path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                    with os.fdopen(fd, 'rb') as stream:
                        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
                            raise VerificationError('staged npm cache entry changed type')
                        out = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
                        with os.fdopen(out, 'wb') as sink:
                            while chunk := stream.read(1024 * 1024):
                                budget['bytes'] += len(chunk)
                                if budget['bytes'] > MAX_CACHE_BYTES:
                                    raise VerificationError('staged npm cache exceeds size bounds')
                                sink.write(chunk)
                else:
                    raise VerificationError('staged npm cache contains a link or special file')

    walk(source_root, destination)
    if budget['entries'] == 0:
        raise VerificationError('staged npm cache is empty')


def stage_private_inputs(source, inputs, private):
    wanted = node_pin(source)
    directory(inputs)
    # A fresh Pod starts with an empty /tmp; an existing directory means this
    # scratch was not created by this verifier.
    os.mkdir(private, 0o700)
    copy_verified(inputs / 'node.tar.gz', private / 'node.tar.gz', wanted)
    copy_tree(inputs / 'npm-cache', private / 'npm-cache')


def main():
    source = Path('/work/src')
    verify_source(source, os.environ.get('OBERTH_RELEASE_SHA', ''), os.environ.get('OBERTH_RELEASE_TAG', ''))
    stage_private_inputs(source, Path('/tmp/oberth-website-inputs'), Path('/tmp/oberth-website'))
    print('website source, Node pin and staged inputs verified before secretstore')


if __name__ == '__main__':
    try:
        main()
    except VerificationError as error:
        sys.exit('website verifier: ' + str(error))
    except (OSError, ValueError, UnicodeError, subprocess.SubprocessError):
        sys.exit('website verifier: invalid input or failed verification')
