#!/usr/bin/env python3
"""Private capture, strict verification and selected-CA mTLS for both cells."""
import base64
import json
import os
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


class Probe:
    def __init__(self, root, work, context, kubeconfig):
        self.root, self.work, self.context = root, work, context
        self.logs = []
        self.env = {k: v for k, v in os.environ.items()
                    if k in ('PATH', 'LANG', 'TMPDIR', 'SSL_CERT_FILE', 'SSL_CERT_DIR',
                             'GOCACHE', 'GOMODCACHE', 'GOTOOLCHAIN', 'GOMAXPROCS',
                             'GOFLAGS', 'GOPROXY', 'GOENV', 'GOWORK', 'CGO_ENABLED')}
        self.env.update(HOME=str(work / 'home'), KUBECONFIG=str(kubeconfig),
                        CLAUDE_CONFIG_DIR=str(work / 'claude'), GIT_CONFIG_NOSYSTEM='1')
        (work / 'home').mkdir(mode=0o700, exist_ok=True)
        (work / 'claude').mkdir(mode=0o700, exist_ok=True)
        self.secrets = work / 'ceremony'

    def run(self, name, args, timeout=1800, allowed_codes=(0,), cwd=None, stdin=None, umask=0o077):
        # Never print argv, exceptions, stdout, or stderr. Logs are owner-only
        # and must pass the final scan before any artifact can leave the cell.
        out, err = self.work / (name + '.stdout'), self.work / (name + '.stderr')
        self.logs.extend((out, err))
        with out.open('xb') as stdout, err.open('xb') as stderr:
            try:
                done = subprocess.run(args, env=self.env, cwd=cwd or self.work,
                                      stdout=stdout,
                                      stderr=stderr, timeout=timeout, check=False, umask=umask,
                                      **({"input": stdin} if stdin is not None else {"stdin": subprocess.DEVNULL}))
            except (OSError, subprocess.TimeoutExpired):
                raise Refused('bootstrap command failed; private artifacts withheld') from None
        if done.returncode not in allowed_codes:
            raise Refused('bootstrap command failed; private artifacts withheld')
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
