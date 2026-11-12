#!/usr/bin/env python3
"""Private capture, strict verification and selected-CA mTLS for both cells."""
import base64
import json
import os
import re
import signal
import time
from pathlib import Path
import ssl
import subprocess
import urllib.request


class Refused(Exception):
    pass


def verify_result(result, context):
    checks = result.get('checks')
    if (result.get('version') != 1 or result.get('ok') is not True
            or result.get('context') != context or not isinstance(checks, list)
            or not checks or any(c.get('ok') is not True for c in checks)):
        raise Refused('installation verification did not pass every check')
    codes = [c.get('code') for c in checks]
    if None in codes or len(set(codes)) != len(codes):
        raise Refused('verification check identities are invalid')


def secret_values(path):
    values = []
    if not path.exists():
        return values
    for line in path.read_bytes().splitlines():
        if not line or line.startswith(b'#'):
            continue
        name, sep, value = line.partition(b'=')
        if not sep or not value:
            raise Refused('invalid private ceremony file')
        values.append(value)
        if name.endswith(b'_BASE64'):
            try:
                decoded = base64.b64decode(value, validate=True)
            except ValueError:
                raise Refused('invalid private ceremony encoding') from None
            values.append(decoded)
            # A logged fragment of a PEM private key also counts as exposure.
            values.extend(line for line in decoded.splitlines()
                          if line and not line.startswith(b'-----'))
    return values


def scan_logs(paths, values):
    """Never report matching values or snippets, even when the scan fails."""
    if not values or any(not value for value in values):
        raise Refused('secret scan has no complete credential inventory')
    for path in paths:
        with path.open('rb') as stream:
            overlap = max(map(len, values)) - 1
            tail = b''
            while chunk := stream.read(65536):
                block = tail + chunk
                if any(value in block for value in values):
                    raise Refused('secret detected in captured output; artifacts withheld')
                tail = block[-overlap:] if overlap else b''


def safe_go_test(name):
    top = name.split('/')[0] if isinstance(name, str) else ''
    return top if re.fullmatch(r'Test[A-Za-z0-9_]{1,180}', top) else 'unavailable'


def active_go_test(path):
    active = set()
    try:
        with path.open('rb') as stream:
            for line in stream:
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                name = safe_go_test(event.get('Test'))
                if name == 'unavailable':
                    continue
                if event.get('Action') == 'run':
                    active.add(name)
                elif event.get('Action') in ('pass', 'fail', 'skip'):
                    active.discard(name)
    except OSError:
        pass
    return next(iter(sorted(active)), 'unavailable')


def go_inventory(directory):
    values, tests = [], set()
    for path in directory.glob('installer-credential-*.json'):
        if path.stat().st_size > 4 * 1024 * 1024:
            raise Refused('fixture inventory exceeds its bound')
        record = json.loads(path.read_bytes())
        tests.add(record['test'])
        for encoded in record.get('values') or []:
            value = base64.b64decode(encoded, validate=True)
            if not value:
                raise Refused('fixture inventory has an empty credential')
            values.extend((value, base64.b64encode(value)))
            values.extend(line for line in value.splitlines()
                          if len(line) >= 16 and not line.startswith(b'-----'))
    return list(set(values)), tests


def scan_go_events(path, values):
    # test2json escapes PEM newlines and other bytes. Scan decoded Output,
    # retaining an overlap across events so split writes cannot evade a match.
    if not values:
        raise Refused('Go output scan has no credential inventory')
    overlap, tail = max(map(len, values)) - 1, b''
    with path.open('rb') as stream:
        for line in stream:
            if len(line) > 16 * 1024 * 1024:
                raise Refused('Go event exceeds the capture bound')
            event = json.loads(line)
            output = event.get('Output', '')
            if not isinstance(output, str):
                raise Refused('invalid Go output event')
            block = tail + output.encode()
            if any(value in block for value in values):
                raise Refused('stage=secret-scan test=' + safe_go_test(event.get('Test')) + ' secret detected in decoded Go output; artifacts withheld')
            tail = block[-overlap:] if overlap else b''


class Probe:
    def __init__(self, root, work, context, kubeconfig):
        self.root, self.work, self.context = root, work, context
        self.logs = []
        self.go_events = []
        self.deadline = None
        self.stage = "setup"
        self.race_finished = None
        self.last_returncode = None
        self.env = {k: v for k, v in os.environ.items()
                    if k in ('PATH', 'LANG', 'TMPDIR', 'SSL_CERT_FILE', 'SSL_CERT_DIR',
                             'GOCACHE', 'GOMODCACHE', 'GOTOOLCHAIN', 'GOMAXPROCS',
                             'GOFLAGS', 'GOPROXY', 'GOENV', 'GOWORK', 'CGO_ENABLED',
                             'GOMEMLIMIT', 'OBERTH_TEST_TIMEOUT_SCALE', 'GOPATH', 'GIT_TERMINAL_PROMPT',
                             'OBERTH_ARCHIVE_PARITY_REPO', 'OBERTH_TEST_NODE_ARCHIVE',
                             'OBERTH_RELEASE_SHA', 'OBERTH_RELEASE_TAG', 'OBERTH_TOOLS_DIR',
                             'GOBIN', 'KUBEBUILDER_ASSETS')}
        self.env.update(HOME=str(work / 'home'), KUBECONFIG=str(kubeconfig),
                        CLAUDE_CONFIG_DIR=str(work / 'claude'), GIT_CONFIG_NOSYSTEM='1')
        (work / 'home').mkdir(mode=0o700, exist_ok=True)
        (work / 'claude').mkdir(mode=0o700, exist_ok=True)
        self.secrets = work / 'ceremony'

    def budget(self, seconds):
        self.deadline = time.monotonic() + seconds

    def run(self, name, args, timeout=1800, allowed_codes=(0,), cwd=None, stdin=None,
            umask=0o077, go_events=False):
        # Captures are private. Safe stage names, never argv/output/exception text.
        self.stage = name
        if self.deadline is not None:
            timeout = min(timeout, self.deadline - time.monotonic())
        if timeout <= 0:
            raise Refused('stage=' + name + ' time budget exceeded; private artifacts withheld')
        out, err = self.work / (name + '.stdout'), self.work / (name + '.stderr')
        self.logs.extend((out, err))
        if go_events:
            self.go_events.append(out)
        with out.open('xb') as stdout, err.open('xb') as stderr:
            try:
                process = subprocess.Popen(args, env=self.env, cwd=cwd or self.work,
                    stdout=stdout, stderr=stderr, start_new_session=True, umask=umask,
                    stdin=subprocess.PIPE if stdin is not None else subprocess.DEVNULL)
                try:
                    process.communicate(input=stdin, timeout=timeout)
                except BaseException as error:
                    if process.returncode is None:
                        try:
                            os.killpg(process.pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    process.wait(timeout=5)
                    if isinstance(error, subprocess.TimeoutExpired):
                        identity = ' test=' + active_go_test(out) if go_events else ''
                        raise Refused('stage=' + name + identity + ' time budget exceeded; private artifacts withheld') from None
                    raise
            except OSError:
                raise Refused('stage=' + name + ' command could not start; private artifacts withheld') from None
        self.last_returncode = process.returncode
        if process.returncode not in allowed_codes:
            raise Refused('stage=' + name + ' command failed; private artifacts withheld')
        return out.read_bytes()

    def mcp(self, url, ca, cert, key, method, params, request_id):
        # Explicit CA-only trust: do not load OS roots. No redirects/proxies.
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        tls.load_verify_locations(cafile=ca)
        tls.load_cert_chain(certfile=cert, keyfile=key)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                    urllib.request.HTTPSHandler(context=tls), NoRedirect())
        request = urllib.request.Request(url, data=json.dumps({
            'jsonrpc': '2.0', 'id': request_id, 'method': method, 'params': params
        }).encode(), headers={'Content-Type': 'application/json',
                             'Accept': 'application/json'})
        try:
            with opener.open(request, timeout=60) as response:
                raw = response.read(8 * 1024 * 1024 + 1)
            if len(raw) > 8 * 1024 * 1024:
                raise Refused('MCP response exceeds the probe bound')
            path = self.work / ('mcp-' + str(request_id) + '.stdout')
            path.write_bytes(raw)
            self.logs.append(path)
            result = json.loads(raw)
        except (OSError, ValueError):
            raise Refused('HTTPS MCP probe failed; private artifacts withheld') from None
        if result.get('id') != request_id or 'error' in result or 'result' not in result:
            raise Refused('MCP returned an invalid response')
        return result['result']


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise Refused('MCP redirects are refused')
