#!/usr/bin/env python3
"""Blocking bootstrap contract cell: local fixtures, no VM or Kubernetes cluster."""
import base64
import contextlib
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import time
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fixtures import private_values, tls_server
from probe import Probe, Refused, go_inventory, scan_go_events, scan_logs, secret_values, verify_result

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


def assert_no_mutations(requests):
    if any(method != 'GET' for method, _ in requests):
        raise Refused('plan attempted to mutate the API fixture')


def plan_fixture(probe, cli):
    requests = {name: [] for name in ('selected', 'other')}
    clusters, users, contexts, urls = [], [], [], {}
    with contextlib.ExitStack() as stack:
        for name in requests:
            token = secrets.token_hex(32)
            with probe.secrets.open('ab') as stream:
                stream.write(('FIXTURE_API_TOKEN_' + name + '=' + token + '\n').encode())

            def respond(method, path, headers, body, name=name, token=token):
                if method != 'GET' or headers.get('Authorization') != 'Bearer ' + token:
                    return 403, {}
                if path == '/api/v1/nodes':
                    return 200, {'apiVersion': 'v1', 'kind': 'NodeList', 'items': [{
                        'metadata': {'name': name + '-node', 'annotations': {'k3s.io/node-args': '["server"]'}},
                        'status': {'nodeInfo': {'architecture': 'amd64', 'kubeletVersion': 'v1.34.3+k3s1'},
                                   'conditions': [{'type': 'Ready', 'status': 'True'}]}}]}
                if path == '/apis/storage.k8s.io/v1/storageclasses':
                    return 200, {'apiVersion': 'storage.k8s.io/v1', 'kind': 'StorageClassList', 'items': [{
                        'metadata': {'name': 'fixture-storage', 'annotations': {
                            'storageclass.kubernetes.io/is-default-class': 'true'}}, 'provisioner': 'fixture'}]}
                return 404, {}

            url, cert, _ = stack.enter_context(tls_server(probe, name + '-api', respond, record=lambda method, path, name=name: requests[name].append((method, path)), allowed_methods={'GET'}))
            urls[name] = url
            clusters.append({'name': name, 'cluster': {'server': url,
                'certificate-authority-data': base64.b64encode(cert.read_bytes()).decode()}})
            users.append({'name': name, 'user': {'token': token}})
            contexts.append({'name': name, 'context': {'cluster': name, 'user': name}})
        config = probe.work / 'config'
        config.write_text(json.dumps({'apiVersion': 'v1', 'kind': 'Config', 'clusters': clusters,
                                     'users': users, 'contexts': contexts, 'current-context': 'other'}))
        original = config.read_bytes()
        tools = probe.work / 'tools'
        tools.mkdir()
        (tools / 'kubectl').write_text('''#!/bin/sh
set -eu
[ "$1" = --kubeconfig ] && shift 2
[ "$1" = --context ]
context=$2
case "$context" in selected|other) :;; *) exit 98;; esac
shift 2
[ "$*" = "get --raw /api/v1/nodes/$context-node/proxy/stats/summary" ]
echo '{"node":{"fs":{"availableBytes":4294967296}}}'
''')
        (tools / 'helm').write_text('#!/bin/sh\nexit 99\n')
        for tool in tools.iterdir():
            tool.chmod(0o700)
        probe.env['PATH'] = str(tools) + os.pathsep + probe.env['PATH']
        answers = probe.work / 'answers.json'
        answers.write_text(json.dumps({'version': 1, 'config': {
            'ContextName': 'other', 'InstallSecretStore': True, 'ClientAccess': 'none'}}))
        args = [str(cli), 'install', '--answers', str(answers), '--kubeconfig', str(config)]
        for context in ('selected', 'other'):
            counts = {name: len(items) for name, items in requests.items()}
            result = json.loads(probe.run('plan-' + context, args + ['--context', context, '--plan', '--json']))
            verify_result(result | {'context': result['identity']['context']}, context)
            if result['identity']['api_server'] != urls[context] or not result['identity']['ca_sha256']:
                raise Refused('plan did not retain the selected API identity')
            if len(requests[context]) <= counts[context] or any(len(items) != counts[name] for name, items in requests.items() if name != context):
                raise Refused('plan contacted the wrong context endpoint')
        for name, extra in [('missing-context', ['--context', 'missing']), ('ambiguous-context', [])]:
            if name == 'ambiguous-context':
                answers.write_text('{"version":1,"config":{"InstallSecretStore":true}}')
            counts = {name: len(items) for name, items in requests.items()}
            raw = probe.run(name, args + extra + ['--plan', '--json'], allowed_codes=(1, 2))
            if json.loads(raw).get('ok') is not False or counts != {name: len(items) for name, items in requests.items()}:
                raise Refused('invalid context was accepted or contacted an API')
        for items in requests.values():
            assert_no_mutations(items)
        if config.read_bytes() != original:
            raise Refused('plan rewrote fixture kubeconfig')


RACE_FLAGS = ['-race', '-gcflags=modernc.org/sqlite/...=-race=false',
              '-gcflags=modernc.org/libc/...=-race=false']
RACE_BUDGET = 3120
CELL_BUDGET = 120


def require_pass_events(raw):
    events = [json.loads(line) for line in raw.splitlines()]
    failed = [event['Test'].split('/')[0] for event in events
              if event.get('Action') == 'fail' and event.get('Test', '').startswith('Test')]
    if failed:
        # Top-level test identities come from Go's compiled test list, never Output.
        import re
        name = next((name for name in failed if re.fullmatch(r'Test[A-Za-z0-9_]+', name)), 'unknown')
        raise Refused('stage=rest-race test=' + name + ' failed; private artifacts withheld')
    passed = {event.get('Test') for event in events if event.get('Action') == 'pass'}
    for name in TESTS:
        if name not in passed:
            raise Refused('stage=fixture-events test=' + name + ' did not pass; private artifacts withheld')


def reject_generated_leak(probe):
    directory = probe.work / 'adverse'
    directory.mkdir(mode=0o700)
    control = Probe(probe.root, directory, 'selected', directory / 'unused')
    control.deadline = probe.deadline
    inventory = directory / 'inventory'
    inventory.mkdir(mode=0o700)
    try:
        control.run('generated-key-leak', ['go', 'test', *RACE_FLAGS, '-json', '-count=1',
            '-timeout=30s', '-outputdir', str(inventory), './internal/installer',
            '-run', '^TestFirstAdminCSRDoesNotMintBearer$', '-args',
            '-bootstrap-credential-leak-control'], cwd=probe.root, umask=0o022, go_events=True)
        values, tests = go_inventory(inventory)
        if tests != {'TestFirstAdminCSRDoesNotMintBearer'} or not values:
            raise Refused('stage=leak-control fixture inventory missing')
        try:
            scan_go_events(control.go_events[0], values)
        except Refused as error:
            if 'secret detected in decoded Go output; artifacts withheld' in str(error):
                return
            raise
        raise Refused('stage=leak-control generated credential was not rejected')
    finally:
        # Deliberate adverse output is never part of normal scan/publication.
        shutil.rmtree(directory)


def run_cell(probe, race_command):
    inventory = probe.work / 'go-inventory'
    inventory.mkdir(mode=0o700)
    started = time.monotonic()
    raw = probe.run('rest-race', [*race_command, '--json', '--outputdir', str(inventory)],
                    timeout=RACE_BUDGET, allowed_codes=(0, 1), cwd=probe.root,
                    umask=0o022, go_events=True)
    probe.race_finished = time.monotonic()
    race_seconds = probe.race_finished - started
    probe.budget(CELL_BUDGET)
    signal.setitimer(signal.ITIMER_REAL, CELL_BUDGET)
    require_pass_events(raw)
    if probe.last_returncode:
        raise Refused('stage=rest-race package failed; private artifacts withheld')
    values, inventoried = go_inventory(inventory)
    for name in TESTS:
        if name != 'TestPinnedKeyIsTheReleasePipelinePin' and name not in inventoried:
            raise Refused('stage=fixture-inventory test=' + name + ' missing; private artifacts withheld')
    cli = probe.work / 'home/.local/bin/oberth'
    cli.parent.mkdir(parents=True)
    probe.run('build-cli', ['go', 'build', *RACE_FLAGS, '-o', str(cli), './cmd/oberth'], cwd=probe.root)
    plan_fixture(probe, cli)
    raw = probe.run('assertions', [sys.executable, '-I', '-B',
                    str(probe.root / 'hack/bootstrap-acceptance/assertions.py')], allowed_codes=(0, 1))
    events = [json.loads(line) for line in raw.splitlines()]
    for event in events:
        if event.get('action') in ('fail', 'skip'):
            import re
            name = event.get('test', '')
            if not isinstance(name, str) or not re.fullmatch(r'[A-Za-z0-9_.]{1,240}', name):
                name = 'unavailable'
            raise Refused('stage=assertions test=' + name + ' did not pass; private artifacts withheld')
    if probe.last_returncode or not any(event.get('action') == 'pass' for event in events):
        raise Refused('stage=assertions test=unavailable assertions did not pass; private artifacts withheld')
    probe.stage = "leak-control"
    reject_generated_leak(probe)
    return race_seconds


def checked_cell(probe, race_command):
    def expired(signum, frame):
        raise Refused('stage=' + probe.stage + ' cell time budget exceeded; private artifacts withheld')
    previous = signal.signal(signal.SIGALRM, expired)
    try:
        try:
            race_seconds = run_cell(probe, race_command)
        finally:
            if probe.deadline is None:
                signal.setitimer(signal.ITIMER_REAL, CELL_BUDGET)
            probe.stage = 'secret-scan'
            values, _ = go_inventory(probe.work / 'go-inventory')
            values = list(set(values + secret_values(probe.secrets) + private_values(probe.work)))
            for path in probe.go_events:
                scan_go_events(path, values)
            scan_logs(probe.logs, values)
        return race_seconds, time.monotonic() - probe.race_finished
    except Refused as error:
        if str(error).startswith('stage='):
            raise
        raise Refused('stage=' + probe.stage + ' ' + str(error)) from None
    except Exception:
        raise Refused('stage=' + probe.stage + ' internal assertion failed; private artifacts withheld') from None
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def main():
    os.umask(0o077)
    root = Path(__file__).resolve().parents[2]
    race_command = sys.argv[1:] or [sys.executable, '-I', '-S', '-B', '.oberth/test-packages.py', 'rest']
    with tempfile.TemporaryDirectory(prefix='oberth-bootstrap-contract-') as directory:
        probe = Probe(root, Path(directory), 'selected', Path(directory) / 'unused-kubeconfig')
        probe.secrets.write_bytes(b'FIXTURE_CANARY=' + secrets.token_hex(32).encode() + b'\n')
        race_seconds, cell_seconds = checked_cell(probe, race_command)
    print('Bootstrap contract cell passed: 18 race fixture events; '
          f'race_seconds={race_seconds:.3f} added_cell_seconds={cell_seconds:.3f}.')


if __name__ == '__main__':
    try:
        main()
    except Refused as error:
        print('Bootstrap contract cell failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
    except Exception:
        print('Bootstrap contract cell failed: stage=internal; private artifacts withheld.', file=sys.stderr)
        sys.exit(1)
