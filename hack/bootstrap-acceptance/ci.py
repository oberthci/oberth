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
from probe import (Probe, Refused, diagnostic, go_inventory, safe_go_test, scan_go_events,
                   scan_logs, secret_values, stage_failure, verify_result)

# Exact test identities are checked in go test's event stream: an empty regex
# match or a skipped required fixture cannot manufacture a green cell.
INSTALLER_PACKAGE = 'github.com/oberthci/oberth/internal/installer'
PIN_PACKAGE = 'github.com/oberthci/oberth/internal/releaseverify'
CLI_PACKAGE = 'github.com/oberthci/oberth/cmd/oberth'
TESTS = (
    (INSTALLER_PACKAGE, 'TestPluginFetchVerifiedRelease'),
    (INSTALLER_PACKAGE, 'TestPluginFetchOverHTTPS'),
    (PIN_PACKAGE, 'TestPinnedKeyIsTheReleasePipelinePin'),
    (INSTALLER_PACKAGE, 'TestPluginInstallRequiresRecoveryFile'),
    (INSTALLER_PACKAGE, 'TestPluginInstallUsesVerifiedCLIAndSharedModes'),
    (INSTALLER_PACKAGE, 'TestAnswersStrictRoundTrip'),
    (INSTALLER_PACKAGE, 'TestSecretsFilePathRefusals'),
    (INSTALLER_PACKAGE, 'TestSecretsFileHoldsCredentialsAndOutputOnlyItsDigest'),
    (INSTALLER_PACKAGE, 'TestInstallReservesTheSecretsFileFirst'),
    (INSTALLER_PACKAGE, 'TestMultifileKubeconfigKeepsExplicitContext'),
    (INSTALLER_PACKAGE, 'TestPlanReportsSelectedRemoteClusterWithoutAnotherGate'),
    (INSTALLER_PACKAGE, 'TestPluginConnectRefusesWithoutAMatchingPin'),
    (INSTALLER_PACKAGE, 'TestPluginConnectStoresTheCAAndMergesSettings'),
    (INSTALLER_PACKAGE, 'TestPluginConnectRefusesSettingsItCannotMerge'),
    (INSTALLER_PACKAGE, 'TestPluginConnectIgnoresTheDefaultTrustOfOpenSSL'),
    (INSTALLER_PACKAGE, 'TestFirstAdminCSRDoesNotMintBearer'),
    (INSTALLER_PACKAGE, 'TestPluginInviteConfiguresMutualTLSAfterRedemption'),
    (INSTALLER_PACKAGE, 'TestAddRepoClusterActionsKeepTheChosenContext'),
)


# These complete flows must also pass in the recorded rest run. The existing
# credential inventory contract remains scoped to TESTS' installer fixtures.
FLOW_TESTS = (
    (CLI_PACKAGE, 'TestRunCreateFirstAdminLiveGate'),
    (INSTALLER_PACKAGE, 'TestInstallJSONOnboardingAndResume'),
)


def assert_no_mutations(requests):
    if any(method != 'GET' for method, _ in requests):
        raise Refused('plan attempted to mutate the API fixture', check='api-mutation')


def require_deferred_policy(result, policy):
    checks = [check for check in result.get('checks', [])
              if check.get('code') == 'network_policy']
    if (result.get('review', {}).get('network_policy') != policy or len(checks) != 1
            or checks[0].get('ok') is not False or checks[0].get('warning') is not True
            or checks[0].get('reason') != 'unverified'
            or 'first at install, after approval' not in checks[0].get('remedy', '')
            or checks[0].get('evidence')):
        raise Refused('plan did not defer enforcement honestly', check='deferred-policy')


def json_result(raw, check):
    try:
        return json.loads(raw)
    except ValueError:
        raise Refused('invalid JSON result', check=check) from None


def verify_plan(probe, name, args, context):
    # Failed JSON plans carry actionable checks despite their nonzero exit.
    raw = probe.run(name, args, allowed_codes=(0, 1, 2), package=CLI_PACKAGE)
    try:
        result = json_result(raw, 'plan-json')
        if not isinstance(result, dict) or not isinstance(result.get('identity'), dict):
            raise Refused('invalid plan identity', check='plan-schema')
        verify_result(result | {'context': result['identity'].get('context')}, context)
    except Refused as error:
        error.stage, error.package = name, CLI_PACKAGE
        raise
    if probe.last_returncode != 0:
        raise Refused('plan command failed despite a green result', stage=name,
                      check='plan-exit', package=CLI_PACKAGE)
    return result


def require_disk_refusal(probe, args):
    try:
        verify_plan(probe, 'plan-disk-refusal',
                    args + ['--context', 'selected', '--plan', '--json'], 'selected')
    except Refused as error:
        if error.check != 'disk':
            raise
        if (error.stage != 'plan-disk-refusal' or error.package != CLI_PACKAGE
                or probe.last_returncode != 1):
            raise Refused('failed plan check was not identified', check='hard-plan-diagnostic') from None
    else:
        raise Refused('hard disk failure was accepted', check='hard-plan-refusal')


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
                if path.split('?', 1)[0] == '/api/v1/namespaces':
                    return 200, {'apiVersion': 'v1', 'kind': 'NamespaceList', 'items': []}
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
cat "$(dirname "$0")/node-stats.json"
''')
        # Both fresh and maintenance plans must succeed using reads alone.
        # The loopback API refuses every attempted mutation before its body.
        (tools / 'release-state').write_text('existing')
        (tools / 'helm').write_text("#!/usr/bin/python3\nimport json\nfrom pathlib import Path\nimport sys\nargs = sys.argv[1:]\nflags = {}\nfor key in ('--kubeconfig', '--kube-context'):\n    if args.count(key) != 1:\n        sys.exit(98)\n    index = args.index(key)\n    if index + 1 == len(args):\n        sys.exit(98)\n    flags[key] = args[index + 1]\n    del args[index:index + 2]\nif flags['--kube-context'] not in ('selected', 'other') or not Path(flags['--kubeconfig']).is_file():\n    sys.exit(98)\nexisting = Path(__file__).with_name('release-state').read_text() == 'existing'\nif args == ['list', '-n', 'oberth', '-o', 'json', '--deployed', '--failed', '--pending', '--superseded', '--uninstalling', '--uninstalled']:\n    print(json.dumps([{'name': 'oberth', 'namespace': 'oberth', 'status': 'deployed', 'chart': 'oberth-0.21.10'}] if existing else []))\nelif args == ['list', '-n', 'oberth-argo', '-o', 'json', '--deployed', '--failed', '--pending', '--superseded', '--uninstalling', '--uninstalled']:\n    print(json.dumps([{'name': 'argo-workflows', 'namespace': 'oberth-argo', 'status': 'deployed', 'chart': 'argo-workflows-1.0.24'}] if existing else []))\nelif existing and args == ['get', 'values', 'argo-workflows', '-n', 'oberth-argo', '--all', '-o', 'json']:\n    print(json.dumps({'controller': {'parallelism': 17, 'namespaceParallelism': 5}}))\nelif existing and args == ['get', 'values', 'argo-workflows', '-n', 'oberth-argo', '-o', 'json']:\n    print(json.dumps({'executor': {'resources': {'requests': {'cpu': '75m'}}}}))\nelif existing and args == ['get', 'values', 'oberth', '-n', 'oberth', '--all', '-o', 'json']:\n    print(json.dumps({'networkPolicy': {'enabled': True}}))\nelse:\n    sys.exit(99)\n")
        for tool in tools.iterdir():
            tool.chmod(0o700)
        stats = tools / 'node-stats.json'
        healthy_stats = '{"node":{"fs":{"availableBytes":4294967296}}}'
        stats.write_text(healthy_stats)
        probe.env['PATH'] = str(tools) + os.pathsep + probe.env['PATH']
        answers = probe.work / 'answers.json'
        answers.write_text(json.dumps({'version': 1, 'config': {
            'ContextName': 'other', 'InstallSecretStore': True, 'ClientAccess': 'none',
            'ArgoParallelism': 7, 'ArgoNamespaceParallelism': 3}}))
        args = [str(cli), 'install', '--answers', str(answers), '--kubeconfig', str(config)]
        for context in ('selected', 'other'):
            counts = {name: len(items) for name, items in requests.items()}
            result = verify_plan(probe, 'plan-' + context,
                                 args + ['--context', context, '--plan', '--json'], context)
            if result['identity']['api_server'] != urls[context] or not result['identity']['ca_sha256']:
                raise Refused('plan did not retain the selected API identity', check='api-identity')
            if len(requests[context]) <= counts[context] or any(len(items) != counts[name] for name, items in requests.items() if name != context):
                raise Refused('plan contacted the wrong context endpoint', check='context-endpoint')
            if (result['review'].get('argo_parallelism') != '7 (requested)'
                    or result['review'].get('argo_namespace_parallelism') != '3 (requested)'):
                raise Refused('answers lost native Argo limits', check='argo-review')
            require_deferred_policy(result, 'true')
            if result.get('review', {}).get('argo_executor_resources') != {
                    'operator_set': True, 'resources': {'requests': {'cpu': '75m'}}}:
                raise Refused('operator executor resources were not preserved in review',
                              check='executor-resources-review')
            observed = requests[context][counts[context]:]
            assert_no_mutations(observed)
        # Exercise the production CLI's nonzero failed-plan path, not only a
        # synthetic result dictionary. Its hard disk check must survive capture.
        stats.write_text('{"node":{"fs":{"availableBytes":0}}}')
        try:
            require_disk_refusal(probe, args)
        finally:
            stats.write_text(healthy_stats)
        (tools / 'release-state').write_text('fresh')
        before = len(requests['selected'])
        try:
            result = verify_plan(probe, 'plan-fresh',
                                 args + ['--context', 'selected', '--plan', '--json'], 'selected')
            require_deferred_policy(result, 'true')
            if len(requests['selected']) <= before:
                raise Refused('fresh plan did not inspect the cluster', check='fresh-reads')
            assert_no_mutations(requests['selected'][before:])
        finally:
            (tools / 'release-state').write_text('existing')
        for name, extra in [('missing-context', ['--context', 'missing']), ('ambiguous-context', [])]:
            if name == 'ambiguous-context':
                answers.write_text('{"version":1,"config":{"InstallSecretStore":true}}')
            counts = {name: len(items) for name, items in requests.items()}
            raw = probe.run(name, args + extra + ['--plan', '--json'], allowed_codes=(1, 2))
            if json_result(raw, 'context-refusal-json').get('ok') is not False or counts != {name: len(items) for name, items in requests.items()}:
                raise Refused('invalid context was accepted or contacted an API', check='context-refusal')
        for items in requests.values():
            assert_no_mutations(items)
        if config.read_bytes() != original:
            raise Refused('plan rewrote fixture kubeconfig', check='kubeconfig-preserved')


RACE_FLAGS = ['-race', '-gcflags=modernc.org/sqlite/...=-race=false',
              '-gcflags=modernc.org/libc/...=-race=false']
RACE_BUDGET = 3120
CELL_BUDGET = 120
FAILURE_SCAN_BUDGET = 10


def require_pass_events(raw):
    try:
        events = [json.loads(line) for line in raw.splitlines()]
    except ValueError:
        raise Refused('invalid race event JSON', stage='fixture-events', check='event-json') from None
    terminal = {}
    for event in events:
        if not isinstance(event, dict):
            raise Refused('invalid race event', stage='fixture-events', check='event-schema')
        package, test, action = event.get('Package'), event.get('Test'), event.get('Action')
        if any(value is not None and not isinstance(value, str) for value in (package, test, action)):
            raise Refused('invalid race identity', stage='fixture-events', check='event-schema')
        if action == 'fail':
            raise Refused('race suite failed', stage='rest-race', check='go-test-pass' if test else 'go-package-pass',
                          package=package, test=safe_go_test(test))
        if action in ('pass', 'skip'):
            terminal[(package, test)] = action
    for package, test in TESTS + FLOW_TESTS:
        if terminal.get((package, test)) != 'pass':
            raise Refused('required fixture did not pass', stage='fixture-events', check='required-fixture-pass',
                          package=package, test=test)


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
            '-bootstrap-credential-leak-control'], cwd=probe.root, umask=0o022, go_events=True,
            package=INSTALLER_PACKAGE, test='TestFirstAdminCSRDoesNotMintBearer')
        values, tests = go_inventory(inventory)
        if tests != {'TestFirstAdminCSRDoesNotMintBearer'} or not values:
            raise Refused('fixture inventory missing', stage='leak-control', check='credential-inventory', package=INSTALLER_PACKAGE, test='TestFirstAdminCSRDoesNotMintBearer')
        try:
            scan_go_events(control.go_events[0], values)
        except Refused as error:
            if error.check == 'credential-exposure' and error.channel == 'go-output':
                return
            raise
        raise Refused('generated credential was not rejected', stage='leak-control', check='generated-key-rejected', package=INSTALLER_PACKAGE, test='TestFirstAdminCSRDoesNotMintBearer')
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
        raise Refused('race command failed', stage='rest-race', check='command-exit')
    values, inventoried = go_inventory(inventory)
    for package, name in TESTS:
        if package == INSTALLER_PACKAGE and name not in inventoried:
            raise Refused('fixture inventory missing', stage='fixture-inventory', check='credential-inventory', package=package, test=name)
    cli = probe.work / 'home/.local/bin/oberth'
    cli.parent.mkdir(parents=True)
    probe.run('build-cli', ['go', 'build', *RACE_FLAGS, '-o', str(cli), './cmd/oberth'], cwd=probe.root,
              package=CLI_PACKAGE)
    plan_fixture(probe, cli)
    raw = probe.run('assertions', [sys.executable, '-I', '-B',
                    str(probe.root / 'hack/bootstrap-acceptance/assertions.py')], allowed_codes=(0, 1))
    events = [json_result(line, 'assertion-json') for line in raw.splitlines()]
    for event in events:
        if not isinstance(event, dict):
            raise Refused('invalid assertion event', stage='assertions', check='assertion-schema')
        if event.get('action') in ('fail', 'skip'):
            import re
            name = event.get('test', '')
            if not isinstance(name, str) or not re.fullmatch(r'[A-Za-z0-9_.]{1,240}', name):
                name = 'unavailable'
            raise Refused('assertion did not pass', stage='assertions', check='python-test-pass', package=name.split('.')[0], test=name)
    if probe.last_returncode or not any(event.get('action') == 'pass' for event in events):
        raise Refused('assertions did not pass', stage='assertions', check='assertion-events')
    probe.stage = "leak-control"
    reject_generated_leak(probe)
    return race_seconds


def final_scan(probe):
    values, _ = go_inventory(probe.work / 'go-inventory')
    values = list(set(values + secret_values(probe.secrets) + private_values(probe.work)))
    for path in probe.go_events:
        scan_go_events(path, values)
    scan_logs(probe.logs, values)


def checked_cell(probe, race_command):
    failure = None
    scanning_failure = False
    def expired(signum, frame):
        raise Refused('time budget exceeded', stage=probe.stage,
                      check='failure-scan-timeout' if scanning_failure else 'cell-timeout')
    previous = signal.signal(signal.SIGALRM, expired)
    try:
        try:
            race_seconds = run_cell(probe, race_command)
        except Exception as error:
            # Freeze the originating stage before entering final scanning.
            failure = stage_failure(error, probe.stage)
        probe.stage = 'secret-scan'
        if failure is not None:
            scanning_failure = True
            # Independent one-shot alarm: the cell alarm may already have fired.
            signal.setitimer(signal.ITIMER_REAL, FAILURE_SCAN_BUDGET)
        elif probe.deadline is None:
            signal.setitimer(signal.ITIMER_REAL, CELL_BUDGET)
        try:
            final_scan(probe)
        except Exception as error:
            scan_failure = stage_failure(error, probe.stage)
            if failure is None:
                failure = scan_failure
            else:
                failure.cleanup_failure = scan_failure
        if failure is not None:
            raise failure from None
        return race_seconds, time.monotonic() - probe.race_finished
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
    print(f'Bootstrap contract cell passed: {len(TESTS) + len(FLOW_TESTS)} race fixture events; '
          f'race_seconds={race_seconds:.3f} added_cell_seconds={cell_seconds:.3f}.')


if __name__ == '__main__':
    try:
        main()
    except Refused as error:
        print('Bootstrap contract cell failed: ' + diagnostic(error), file=sys.stderr)
        if error.cleanup_failure is not None:
            print('Bootstrap contract cleanup failed: ' + diagnostic(error.cleanup_failure), file=sys.stderr)
        sys.exit(1)
    except Exception:
        print('Bootstrap contract cell failed: ' + diagnostic(Refused('internal error', stage='internal', check='internal-error')), file=sys.stderr)
        sys.exit(1)
