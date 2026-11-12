#!/usr/bin/env python3
"""Private bootstrap probe for an operator-provisioned, disposable cluster.

This is a partial cell, not a release acceptance receipt. See README.md.
"""
import argparse
import base64
import json
import ipaddress
import os
from pathlib import Path
import shutil
import ssl
import subprocess
import sys
import tempfile
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
                    if k in ('PATH', 'LANG', 'TMPDIR', 'SSL_CERT_FILE', 'SSL_CERT_DIR')}
        self.env.update(HOME=str(work / 'home'), KUBECONFIG=str(kubeconfig),
                        CLAUDE_CONFIG_DIR=str(work / 'claude'), GIT_CONFIG_NOSYSTEM='1')
        (work / 'home').mkdir(mode=0o700)
        (work / 'claude').mkdir(mode=0o700)
        self.secrets = work / 'ceremony'

    def run(self, name, args, timeout=1800, allowed_codes=(0,)):
        # Never print argv, exceptions, stdout, or stderr. Logs are owner-only
        # and must pass the final scan before any artifact can leave the cell.
        out, err = self.work / (name + '.stdout'), self.work / (name + '.stderr')
        self.logs.extend((out, err))
        with out.open('xb') as stdout, err.open('xb') as stderr:
            try:
                done = subprocess.run(args, env=self.env, cwd=self.work,
                                      stdin=subprocess.DEVNULL, stdout=stdout,
                                      stderr=stderr, timeout=timeout, check=False)
            except (OSError, subprocess.TimeoutExpired):
                raise Refused('bootstrap command failed; private artifacts withheld') from None
        if done.returncode not in allowed_codes:
            raise Refused('bootstrap command failed; private artifacts withheld')
        return out.read_bytes()

    def mcp(self, url, ca, token, method, params, request_id):
        # Explicit CA-only trust: do not load OS roots. No redirects/proxies.
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        tls.load_verify_locations(cafile=ca)
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                    urllib.request.HTTPSHandler(context=tls), NoRedirect())
        request = urllib.request.Request(url, data=json.dumps({
            'jsonrpc': '2.0', 'id': request_id, 'method': method, 'params': params
        }).encode(), headers={'Content-Type': 'application/json',
                             'Accept': 'application/json',
                             'Authorization': 'Bearer ' + token})
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--context', required=True)
    parser.add_argument('--kubeconfig', required=True, type=Path)
    parser.add_argument('--answers', required=True, type=Path)
    parser.add_argument('--version', required=True)
    parser.add_argument('--reboot-driver', type=Path, help='operator-provided disposable runner reboot executable')
    parser.add_argument('--openbao-namespace', default='openbao')
    parser.add_argument('--confirm-disposable-cluster', action='store_true')
    args = parser.parse_args()
    if not args.confirm_disposable_cluster:
        raise Refused('explicit confirmation of the disposable cluster is required')
    for name in ('helm', 'kubectl', 'openssl', 'ssh-keygen'):
        if not shutil.which(name):
            raise Refused('install ' + name + ' using its official platform instructions before running the probe')
    os.umask(0o077)
    root = Path(__file__).resolve().parents[2]
    # The private directory is outside the checkout and is always removed.
    with tempfile.TemporaryDirectory(prefix='oberth-bootstrap-') as directory:
        work = Path(directory)
        probe = Probe(root, work, args.context, args.kubeconfig.resolve())
        try:
            probe.run('fetch', ['sh', str(root / 'claude-plugin/scripts/fetch-oberth'), args.version])
            cli = str(work / 'home/.local/bin/oberth')
            probe.run('ssh-key', ['ssh-keygen', '-q', '-t', 'ed25519', '-N', '',
                                 '-f', str(work / 'uplink')])
            common = [cli, 'install', '--context', args.context, '--kubeconfig',
                      str(args.kubeconfig.resolve()), '--answers', str(args.answers.resolve())]
            plan = json.loads(probe.run('plan', common + ['--plan', '--json']))
            verify_result(plan | {'context': plan.get('identity', {}).get('context')}, args.context)
            if plan.get('review', {}).get('production_store') is not True:
                raise Refused('acceptance requires a persistent production secret store')
            probe.run('install', common + ['--yes', '--json', '--secrets-file', str(probe.secrets),
                                         '--admin-ssh-public-key', str(work / 'uplink.pub')])
            result = json.loads(probe.run('verify', common + ['--verify', '--json']))
            verify_result(result, args.context)
            url, pin = result.get('mcp_url', ''), result.get('ca_sha256', '')
            from urllib.parse import urlsplit
            parsed = urlsplit(url)
            if parsed.scheme != 'https' or parsed.username or parsed.password or parsed.query or parsed.fragment:
                raise Refused('verification did not return a safe HTTPS MCP URL')
            try:
                ipaddress.ip_address(parsed.hostname or '')
            except ValueError:
                pass
            else:
                raise Refused('clients must connect by DNS name, never by IP')
            probe.run('trust', ['sh', str(root / 'claude-plugin/scripts/trust'), 'connect',
                                '--url', url, '--pin', pin])
            settings = json.loads((work / 'claude/settings.json').read_text())
            ca = settings['env']['NODE_EXTRA_CA_CERTS']
            credentials = dict(line.split('=', 1) for line in probe.secrets.read_text().splitlines()
                               if line and not line.startswith('#'))
            token = credentials.get('OBERTH_UPLINK_TOKEN', '')
            if not token:
                raise Refused('certificate-mode probe integration awaits the mutual TLS bundle')
            listed = probe.mcp(url, ca, token, 'tools/list', {}, 1)
            if 'system_status' not in [t['name'] for t in listed.get('tools', [])]:
                raise Refused('MCP did not expose system_status')
            status = probe.mcp(url, ca, token, 'tools/call', {'name': 'system_status', 'arguments': {}}, 2)
            if status.get('isError') is True:
                raise Refused('system_status returned an error')
            if args.reboot_driver:
                # Driver receives public coordinates only and must return once
                # the same disposable cluster's API and pod exec are available.
                probe.run('reboot', [str(args.reboot_driver.resolve()), args.context])
                recovery = [cli, 'unseal', '--context', args.context,
                            '--kubeconfig', str(args.kubeconfig.resolve()),
                            '--openbao-namespace', args.openbao_namespace]
                status_cmd = recovery + ['--status', '--json']
                before = json.loads(probe.run('sealed', status_cmd))
                if before.get('initialized') is not True or before.get('sealed') is not True:
                    raise Refused('reboot did not leave initialized OpenBao sealed')
                probe.run('unseal', recovery + ['--secrets-file', str(probe.secrets)])
                after = json.loads(probe.run('unsealed', status_cmd))
                if after.get('initialized') is not True or after.get('sealed') is not False:
                    raise Refused('OpenBao recovery did not complete')
                verify_result(json.loads(probe.run('verify-recovered', common + ['--verify', '--json'])), args.context)

        finally:
            if probe.secrets.exists():
                values = secret_values(probe.secrets)
                key = work / 'uplink'
                if key.exists():
                    values += [line for line in key.read_bytes().splitlines()
                               if line and not line.startswith(b'-----')]
                scan_logs(probe.logs, values)
        print('Bootstrap probe completed; this partial probe is not full release acceptance.')


if __name__ == '__main__':
    try:
        main()
    except Refused as error:
        # Refused messages are fixed local diagnostics, never command output.
        print('Bootstrap probe refused: ' + str(error), file=sys.stderr)
        sys.exit(1)
    except Exception:  # No server/credential-derived exception reaches the transcript.
        # Exceptions can include server content, credentials, or command text.
        print('Bootstrap probe failed; private artifacts withheld.', file=sys.stderr)
        sys.exit(1)
