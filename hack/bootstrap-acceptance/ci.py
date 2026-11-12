#!/usr/bin/env python3
"""Blocking bootstrap contract cell: local fixtures, no VM or Kubernetes cluster."""
import base64
import json
import os
from pathlib import Path
import secrets
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fixtures import private_values, tls_server
from probe import Probe, Refused, scan_logs, secret_values, verify_result

# Exact test identities are checked in go test's event stream: an empty regex
# match or a skipped required fixture cannot manufacture a green cell.
TESTS = (
    'TestPluginFetchVerifiedRelease', 'TestPluginFetchOverHTTPS',
    'TestPinnedKeyIsTheReleasePipelinePin',
    'TestPluginInstallRequiresRecoveryFile', 'TestPluginInstallUsesVerifiedCLIAndSharedModes',
    'TestAnswersStrictRoundTrip', 'TestSecretsFilePathRefusals',
    'TestSecretsFileHoldsCredentialsAndOutputOnlyItsDigest', 'TestInstallReservesTheSecretsFileFirst',
    'TestMultifileKubeconfigKeepsExplicitContext', 'TestPlanReportsSelectedRemoteClusterWithoutAnotherGate',
    'TestPluginConnectRefusesWithoutAMatchingPin', 'TestPluginConnectStoresTheCAAndMergesSettings',
    'TestPluginConnectRefusesSettingsItCannotMerge', 'TestPluginConnectIgnoresTheDefaultTrustOfOpenSSL',
    'TestFirstAdminCSRDoesNotMintBearer', 'TestPluginInviteConfiguresMutualTLSAfterRedemption',
    'TestAddRepoClusterActionsKeepTheChosenContext',
)


def plan_fixture(probe, cli):
    token = secrets.token_hex(32)
    with probe.secrets.open('ab') as stream:
        stream.write(b'FIXTURE_API_TOKEN=' + token.encode() + b'\n')
    requests = []

    def respond(method, path, headers, body):
        requests.append((method, path))
        if method != 'GET' or headers.get('Authorization') != 'Bearer ' + token:
            return 403, {}
        if path == '/api/v1/nodes':
            return 200, {'apiVersion': 'v1', 'kind': 'NodeList', 'items': [{
                'metadata': {'name': 'fixture-node', 'annotations': {'k3s.io/node-args': '["server"]'}},
                'status': {'nodeInfo': {'architecture': 'amd64', 'kubeletVersion': 'v1.34.3+k3s1'},
                           'conditions': [{'type': 'Ready', 'status': 'True'}]}}]}
        if path == '/apis/storage.k8s.io/v1/storageclasses':
            return 200, {'apiVersion': 'storage.k8s.io/v1', 'kind': 'StorageClassList', 'items': [{
                'metadata': {'name': 'fixture-storage', 'annotations': {
                    'storageclass.kubernetes.io/is-default-class': 'true'}}, 'provisioner': 'fixture'}]}
        return 404, {}

    with tls_server(probe, 'api', respond) as (url, cert, _):
        config = probe.work / 'config'
        config.write_text(json.dumps({'apiVersion': 'v1', 'kind': 'Config',
            'clusters': [{'name': 'selected', 'cluster': {'server': url,
                'certificate-authority-data': base64.b64encode(cert.read_bytes()).decode()}}],
            'users': [{'name': 'fixture', 'user': {'token': token}}],
            'contexts': [{'name': name, 'context': {'cluster': 'selected', 'user': 'fixture'}}
                         for name in ['selected', 'other']], 'current-context': 'other'}))
        tools = probe.work / 'tools'
        tools.mkdir()
        kubectl = tools / 'kubectl'
        kubectl.write_text('#!/bin/sh\nset -eu\n[ "$1" = --kubeconfig ] && shift 2\n'
                          '[ "$1" = --context ] && [ "$2" = selected ] && shift 2\n'
                          '[ "$*" = "get --raw /api/v1/nodes/fixture-node/proxy/stats/summary" ]\n'
                          'echo \'{"node":{"fs":{"availableBytes":4294967296}}}\'\n')
        (tools / 'helm').write_text('#!/bin/sh\nexit 99\n')
        for tool in tools.iterdir():
            tool.chmod(0o700)
        probe.env['PATH'] = str(tools) + os.pathsep + probe.env['PATH']
        answers = probe.work / 'answers.json'
        answers.write_text(json.dumps({'version': 1, 'config': {
            'ContextName': 'other', 'InstallSecretStore': True, 'ClientAccess': 'none'}}))
        args = [str(cli), 'install', '--answers', str(answers), '--kubeconfig', str(config)]
        result = json.loads(probe.run('plan-selected', args + ['--context', 'selected', '--plan', '--json']))
        verify_result(result | {'context': result['identity']['context']}, 'selected')
        if result['identity']['api_server'] != url or not result['identity']['ca_sha256']:
            raise Refused('plan did not retain the selected API identity')
        for name, extra in [('missing-context', ['--context', 'missing']), ('ambiguous-context', [])]:
            if name == 'ambiguous-context':
                answers.write_text('{"version":1,"config":{"InstallSecretStore":true}}')
            raw = probe.run(name, args + extra + ['--plan', '--json'], allowed_codes=(1, 2))
            if json.loads(raw).get('ok') is not False:
                raise Refused('invalid context was accepted')
        if any(method != 'GET' for method, _ in requests):
            raise Refused('plan attempted to mutate the API fixture')


def run_cell(probe):
    # Existing Go fixtures assert preservation of explicit 0640 settings. Their
    # normal umask is confined to that child; captures and HOME remain private.
    raw = probe.run('installer-contracts', ['go', 'test', '-json', '-count=1', '-timeout=10m',
                    './internal/installer', './internal/releaseverify', '-run', '^(' + '|'.join(TESTS) + ')$'], cwd=probe.root, umask=0o022)
    passed = {event.get('Test') for line in raw.splitlines()
              if (event := json.loads(line)).get('Action') == 'pass'}
    if not set(TESTS).issubset(passed):
        raise Refused('required bootstrap fixture did not run and pass')
    cli = probe.work / 'home/.local/bin/oberth'
    cli.parent.mkdir(parents=True)
    probe.run('build-cli', ['go', 'build', '-o', str(cli), './cmd/oberth'], cwd=probe.root)
    plan_fixture(probe, cli)
    probe.run('assertions', [sys.executable, '-I', '-B', '-m', 'unittest', 'discover',
                           '-s', str(probe.root / 'hack/bootstrap-acceptance'), '-p', 'test_*.py'])


def main():
    os.umask(0o077)
    root = Path(__file__).resolve().parents[2]
    with tempfile.TemporaryDirectory(prefix='oberth-bootstrap-contract-') as directory:
        probe = Probe(root, Path(directory), 'selected', Path(directory) / 'unused-kubeconfig')
        probe.secrets.write_bytes(b'FIXTURE_CANARY=' + secrets.token_hex(32).encode() + b'\n')
        try:
            run_cell(probe)
        finally:
            scan_logs(probe.logs, secret_values(probe.secrets) + private_values(probe.work))
    print('Bootstrap contract cell passed: verified fetch, selected plan, private answers, trust merge and output scan.')


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('Bootstrap contract cell failed; private artifacts withheld.', file=sys.stderr)
        sys.exit(1)
