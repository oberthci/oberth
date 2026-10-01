#!/usr/bin/env python3
"""Read back the published oberth.ci website against the admitted source.

Tokenless release-website-readback leaf, after release-website deployed the
Worker. It proves publication, not upload: the public HTTPS URLs customers use
must serve bytes identical to the admitted commit's website/public files, with
the content types website/public/_headers declares. Requests carry no cache
busting, so a stale edge copy counts as not yet published; the poll window
exceeds the 300 s max-age _headers sets for the scripts.

It also re-asserts the #647 contract on the admitted source: both maintained
setup-secretstore.sh copies carry the byte-identical semantic audit preflight
(JSON parsed with Python, never the pre-#647 grep). Byte equality of the
served helper with the tagged public copy then extends that proof to the
bytes customers download.
"""
import hashlib
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

TARGETS = (
    ('https://oberth.ci/', 'website/public/index.html', 'text/html'),
    ('https://www.oberth.ci/', 'website/public/index.html', 'text/html'),
    ('https://oberth.ci/setup-secretstore.sh', 'website/public/setup-secretstore.sh', 'text/plain'),
    ('https://www.oberth.ci/setup-secretstore.sh', 'website/public/setup-secretstore.sh', 'text/plain'),
    ('https://oberth.ci/install.sh', 'website/public/install.sh', 'text/plain'),
)
HELPERS = ('scripts/setup-secretstore.sh', 'website/public/setup-secretstore.sh')
AUDIT_BEGIN = '# --- audit preflight (read-only; before any store mutation)'
AUDIT_END = '# --- end audit preflight'
PRE_647_DETECTION = "grep -q '\"type\":\"file\"'"
MAX_BODY = 8 * 1024 * 1024
CONVERGENCE_SECONDS = 900
POLL_SECONDS = 15


class ReadbackError(Exception):
    """A publication or source-contract failure with a fixed diagnostic."""


def git(source, *args):
    result = subprocess.run(['/usr/bin/git', '--no-replace-objects', '-C', str(source),
                             '-c', 'safe.directory=' + str(source), *args],
                            check=True, capture_output=True, timeout=60,
                            env={'PATH': '/usr/bin:/bin', 'HOME': '/tmp',
                                 'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null'})
    return result.stdout


def admitted_bytes(source, sha, tag, relatives):
    """Return the committed bytes of each path after binding source to sha/tag."""
    if not re.fullmatch(r'[0-9a-f]{40}', sha):
        raise ReadbackError('expected a full admitted source SHA')
    if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?', tag):
        raise ReadbackError('invalid admitted release tag')
    for ref in ('HEAD', 'refs/tags/' + tag + '^{commit}'):
        if git(source, 'rev-parse', '--verify', ref).strip() != sha.encode('ascii'):
            raise ReadbackError('source or tag differs from the admitted release SHA')
    committed = {}
    for relative in relatives:
        body = git(source, 'cat-file', 'blob', sha + ':' + relative)
        if (source / relative).read_bytes() != body:
            raise ReadbackError(relative + ' differs from the admitted commit')
        committed[relative] = body
    return committed


def audit_block(text, name):
    lines = text.splitlines(keepends=True)
    begins = [index for index, line in enumerate(lines) if line.startswith(AUDIT_BEGIN)]
    ends = [index for index, line in enumerate(lines) if line.startswith(AUDIT_END)]
    if len(begins) != 1 or len(ends) != 1 or ends[0] <= begins[0]:
        raise ReadbackError(name + ' lacks exactly one #647 audit preflight block')
    return ''.join(lines[begins[0]:ends[0] + 1])


def verify_helper_sources(committed):
    blocks = []
    for name in HELPERS:
        text = committed[name].decode('utf-8')
        if PRE_647_DETECTION in text:
            raise ReadbackError(name + ' still detects audit devices with the pre-#647 grep')
        block = audit_block(text, name)
        if 'json.load(sys.stdin)' not in block:
            raise ReadbackError(name + ' does not parse the audit list as JSON')
        blocks.append(block)
    if blocks[0] != blocks[1]:
        raise ReadbackError('the two setup-secretstore.sh copies disagree on the #647 audit preflight')
    return hashlib.sha256(blocks[0].encode('utf-8')).hexdigest()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def fetch(url):
    """Return (status, body, content type); a redirect or an oversize body never matches."""
    if not url.startswith('https://'):
        raise ReadbackError('readback URLs must use HTTPS')
    opener = urllib.request.build_opener(NoRedirect)
    request = urllib.request.Request(url, headers={'User-Agent': 'oberth-release-website-readback/1'})
    try:
        with opener.open(request, timeout=30) as response:
            body = response.read(MAX_BODY + 1)
            if len(body) > MAX_BODY:
                return response.status, b'', ''
            return response.status, body, response.headers.get('Content-Type', '')
    except urllib.error.HTTPError as error:
        return error.code, b'', ''
    except (urllib.error.URLError, OSError, ValueError):
        return 0, b'', ''


def readback(targets, expected, fetcher=fetch, clock=time.monotonic, sleep=time.sleep,
             budget=CONVERGENCE_SECONDS, interval=POLL_SECONDS, log=None):
    """Poll until every target serves its expected digest and content type."""
    log = log or (lambda line: print(line, file=sys.stderr, flush=True))
    deadline = clock() + budget
    attempt = 0
    while True:
        attempt += 1
        pending = []
        for url, _, content_type in targets:
            status, body, served_type = fetcher(url)
            digest = hashlib.sha256(body).hexdigest()
            if status != 200 or digest != expected[url] or not served_type.startswith(content_type):
                pending.append(f'{url}: HTTP {status}, sha256 {digest}, content-type {served_type!r}')
        if not pending:
            return attempt
        if clock() >= deadline:
            raise ReadbackError('public oberth.ci did not converge on the admitted website: ' + '; '.join(pending))
        if attempt == 1 or attempt % 4 == 0:
            for line in pending:
                log(f'attempt {attempt}: waiting for {line}')
        sleep(interval)


def main():
    source = Path('/work/src')
    tag = os.environ.get('OBERTH_RELEASE_TAG', '')
    sha = os.environ.get('OBERTH_RELEASE_SHA', '')
    relatives = sorted({path for _, path, _ in TARGETS} | set(HELPERS))
    committed = admitted_bytes(source, sha, tag, relatives)
    block = verify_helper_sources(committed)
    expected = {url: hashlib.sha256(committed[path]).hexdigest() for url, path, _ in TARGETS}
    attempts = readback(TARGETS, expected)
    for url, path, _ in TARGETS:
        print(f'readback {url} == {path} sha256 {expected[url]}')
    print(f'#647 audit preflight identical in both helper copies, sha256 {block}')
    print(f'oberth.ci serves the website of {tag} ({sha}) after {attempts} attempt(s)')


if __name__ == '__main__':
    try:
        main()
    except ReadbackError as error:
        sys.exit('website readback: ' + str(error))
    except (OSError, ValueError, UnicodeError, subprocess.SubprocessError):
        sys.exit('website readback: invalid input or failed verification')
