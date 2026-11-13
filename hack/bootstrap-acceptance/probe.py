#!/usr/bin/env python3
"""Private capture, strict verification and selected-CA mTLS for the contract cell."""
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
    def __init__(self, message, *, check='assertion', stage=None, package=None, test=None, channel=None):
        super().__init__(message)
        self.check, self.stage = check, stage
        self.package, self.test, self.channel = package, test, channel
        self.cleanup_failure = None


def identifier(value, pattern):
    return value if isinstance(value, str) and re.fullmatch(pattern, value) else 'unavailable'


def diagnostic(error):
    # Only bounded identity metadata is public, never exception text/Output.
    fields = dict(stage=identifier(error.stage, r'[a-z][a-z0-9-]{0,79}'),
                  check=identifier(error.check, r'[A-Za-z][A-Za-z0-9_-]{0,79}'),
                  package=identifier(error.package, r'(?:github\.com/oberthci/oberth(?:/[A-Za-z0-9_-]+)*|test_[a-z_]+)'),
                  test=identifier(error.test, r'[A-Za-z][A-Za-z0-9_.]{0,239}'))
    if error.channel:
        fields['channel'] = identifier(error.channel, r'(?:stdout|stderr|go-output)')
    return ' '.join(key + '=' + value for key, value in fields.items()) + '; private artifacts withheld'


def stage_failure(error, stage):
    if not isinstance(error, Refused):
        error = Refused('internal assertion failed', check='internal-error')
    if error.stage is None:
        error.stage = stage
    return error


def verify_result(result, context):
    if not isinstance(result, dict):
        raise Refused('invalid verification result', check='result-schema')
    checks = result.get('checks')
    if result.get('version') != 1 or not isinstance(checks, list) or not checks:
        raise Refused('invalid verification result', check='result-schema')
    if result.get('context') != context:
        raise Refused('verification selected another context', check='context-identity')
    if any(not isinstance(c, dict) or type(c.get('ok')) is not bool
           or type(c.get('warning', False)) is not bool for c in checks):
        raise Refused('invalid verification checks', check='check-schema')
    codes = [c.get('code') for c in checks]
    if any(not isinstance(code, str) or not code for code in codes) or len(set(codes)) != len(codes):
        raise Refused('verification check identities are invalid', check='check-identities')
    for check in checks:
        if check['ok'] is False and check.get('warning') is not True:
            # Node names and free-form reasons/remedies are never diagnostics.
            name = identifier(check['code'].split('/')[0], r'[a-z][a-z0-9_]{0,79}')
            raise Refused('installation verification check failed', check=name)
    if result.get('ok') is not True:
        raise Refused('installation verification did not pass', check='result-ok')


def secret_values(path):
    values = []
    if not path.exists():
        return values
    for line in path.read_bytes().splitlines():
        if not line or line.startswith(b'#'):
            continue
        name, sep, value = line.partition(b'=')
        if not sep or not value:
            raise Refused('invalid private ceremony file', check='ceremony-schema')
        values.append(value)
        if name.endswith(b'_BASE64'):
            try:
                decoded = base64.b64decode(value, validate=True)
            except ValueError:
                raise Refused('invalid private ceremony encoding', check='ceremony-encoding') from None
            values.append(decoded)
            # A logged fragment of a PEM private key also counts as exposure.
            values.extend(line for line in decoded.splitlines()
                          if line and not line.startswith(b'-----'))
    return values


def scan_logs(paths, values):
    """Never report matching values or snippets, even when the scan fails."""
    if not values or any(not value for value in values):
        raise Refused('secret scan has no complete credential inventory', check='credential-inventory')
    for path in paths:
        with path.open('rb') as stream:
            overlap = max(map(len, values)) - 1
            tail = b''
            while chunk := stream.read(65536):
                block = tail + chunk
                if any(value in block for value in values):
                    raise Refused('secret detected in captured output; artifacts withheld', check='credential-exposure',
                                  channel='stderr' if path.suffix == '.stderr' else 'stdout')
                tail = block[-overlap:] if overlap else b''


def safe_go_test(name):
    top = name.split('/')[0] if isinstance(name, str) else ''
    return top if re.fullmatch(r'Test[A-Za-z0-9_]{1,180}', top) else 'unavailable'


def active_go_identity(path):
    active = set()
    try:
        with path.open('rb') as stream:
            for line in stream:
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(event, dict) or '/' in str(event.get('Test', '')):
                    continue
                name = safe_go_test(event.get('Test'))
                if name == 'unavailable':
                    continue
                if event.get('Action') in ('run', 'cont'):
                    active.add((event.get('Package'), name))
                elif event.get('Action') in ('pause', 'pass', 'fail', 'skip'):
                    active.discard((event.get('Package'), name))
    except OSError:
        pass
    return next(iter(sorted(active, key=lambda identity: str(identity))), (None, None))


def go_inventory(directory):
    values, tests = [], set()
    for path in directory.glob('installer-credential-*.json'):
        if path.stat().st_size > 4 * 1024 * 1024:
            raise Refused('fixture inventory exceeds its bound', check='inventory-size')
        try:
            record = json.loads(path.read_bytes())
        except ValueError:
            raise Refused('invalid fixture inventory JSON', check='inventory-json') from None
        if not isinstance(record, dict) or not isinstance(record.get('test'), str):
            raise Refused('invalid fixture inventory', check='inventory-schema')
        tests.add(record['test'])
        for encoded in record.get('values') or []:
            try:
                value = base64.b64decode(encoded, validate=True)
            except (ValueError, TypeError):
                raise Refused('invalid fixture credential encoding', check='inventory-encoding') from None
            if not value:
                raise Refused('fixture inventory has an empty credential', check='credential-inventory')
            values.extend((value, base64.b64encode(value)))
            values.extend(line for line in value.splitlines()
                          if len(line) >= 16 and not line.startswith(b'-----'))
    return list(set(values)), tests


def scan_go_events(path, values):
    # test2json escapes PEM newlines and other bytes. Scan decoded Output,
    # retaining an overlap across events so split writes cannot evade a match.
    if not values:
        raise Refused('Go output scan has no credential inventory', check='credential-inventory')
    overlap, tail = max(map(len, values)) - 1, b''
    with path.open('rb') as stream:
        for line in stream:
            if len(line) > 16 * 1024 * 1024:
                raise Refused('Go event exceeds the capture bound', check='event-size')
            try:
                event = json.loads(line)
            except ValueError:
                raise Refused('invalid Go event', check='event-json') from None
            if not isinstance(event, dict):
                raise Refused('invalid Go event', check='event-schema')
            output = event.get('Output', '')
            if not isinstance(output, str):
                raise Refused('invalid Go output event', check='event-output')
            block = tail + output.encode()
            if any(value in block for value in values):
                raise Refused('secret detected in decoded Go output; artifacts withheld',
                              check='credential-exposure', package=event.get('Package'),
                              test=safe_go_test(event.get('Test')), channel='go-output')
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
            umask=0o077, go_events=False, package=None, test=None):
        # Captures are private. Safe stage names, never argv/output/exception text.
        self.stage = name
        if self.deadline is not None:
            timeout = min(timeout, self.deadline - time.monotonic())
        if timeout <= 0:
            raise Refused('time budget exceeded', stage=name, check='command-timeout', package=package, test=test)
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
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        raise Refused('command teardown timed out', stage=name, check='command-reap-timeout',
                                      package=package, test=test) from None
                    if isinstance(error, subprocess.TimeoutExpired):
                        if go_events:
                            package, test = active_go_identity(out)
                        raise Refused('time budget exceeded', stage=name, check='command-timeout',
                                      package=package, test=test) from None
                    if isinstance(error, Refused):
                        error.package = error.package or package
                        error.test = error.test or test
                    raise
            except OSError:
                raise Refused('command could not start', stage=name, check='command-start', package=package, test=test) from None
        self.last_returncode = process.returncode
        if process.returncode not in allowed_codes:
            raise Refused('command failed', stage=name, check='command-exit', package=package, test=test)
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
