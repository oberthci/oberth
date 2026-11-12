#!/usr/bin/env python3
"""Out-of-CI cell for a disposable, root-owned systemd Linux VM. Reboots it."""
import argparse
import base64
import itertools
import json
import os
from pathlib import Path
import platform
import pwd
import shutil
import stat
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parent))
from fixtures import private_values
from machine_tools import install_tools
from probe import Probe, Refused, scan_logs, secret_values, verify_result
from roundtrip import WORKFLOW, held_roundtrip, verify_assisted_result


class Cell:
    def __init__(self, work, root):
        self.work, self.root = work, root
        self.probe = Probe(root, work, 'bootstrap-cell', work / 'kubeconfig')
        self.sequence = itertools.count(len(list(work.glob('*.stdout'))) + 1)
        self.probe.logs = sorted(work.glob('*.stdout')) + sorted(work.glob('*.stderr'))
        self.probe.env['PATH'] = str(work / 'bin') + os.pathsep + os.environ['PATH']
        self.cli = str(work / 'home/.local/bin/oberth')
        self.plugin = root / 'claude-plugin/scripts'
        self.request_ids = itertools.count(1 + max((int(p.stem.split('-')[1]) for p in work.glob('mcp-*.stdout')), default=0))

    def run(self, name, args, **options):
        return self.probe.run(str(next(self.sequence)) + '-' + name, args, **options)

    def k(self, *args, **options):
        return self.run('kubectl', ['kubectl', '--kubeconfig', str(self.work / 'kubeconfig'),
                        '--context', 'bootstrap-cell', *args], **options)

    def tool(self, name, arguments):
        settings = json.loads((self.work / 'claude/settings.json').read_text())['env']
        result = self.probe.mcp(self.url, settings['NODE_EXTRA_CA_CERTS'],
                               settings['CLAUDE_CODE_CLIENT_CERT'], settings['CLAUDE_CODE_CLIENT_KEY'],
                               'tools/call', {'name': name, 'arguments': arguments}, next(self.request_ids))
        if result.get('isError') is True or not isinstance(result.get('structuredContent'), dict):
            raise Refused('MCP tool did not return a successful typed record')
        return result['structuredContent']

    def scan(self):
        values = private_values(self.work)
        if self.probe.secrets.exists():
            values += secret_values(self.probe.secrets)
        kubeconfig = self.work / 'kubeconfig'
        if kubeconfig.exists():
            for line in kubeconfig.read_text().splitlines():
                if 'client-key-data:' in line:
                    encoded = line.split(':', 1)[1].strip().encode()
                    decoded = base64.b64decode(encoded)
                    values += [encoded, decoded]
                    values += [line for line in decoded.splitlines() if line and not line.startswith(b'-----')]
        if values:
            scan_logs(self.probe.logs, values)

    def capture(self):
        # Capture into private files before scanning; never emit raw cluster or
        # process diagnostics, even if the install failed part way through.
        for args in [['journalctl', '--no-pager', '-u', 'k3s', '-u', 'oberth-bootstrap-forge'],
                     ['ps', 'auxww']]:
            self.run('host-log', args, allowed_codes=(0, 1), timeout=60)
        pods = json.loads(self.k('get', 'pods', '--all-namespaces', '-o', 'json'))['items']
        for pod in pods:
            namespace, name = pod['metadata']['namespace'], pod['metadata']['name']
            for container in pod['spec'].get('containers', []) + pod['spec'].get('initContainers', []):
                self.k('logs', '-n', namespace, name, '-c', container['name'], '--timestamps=true',
                       '--tail=10000', allowed_codes=(0, 1))



def require_fresh_host(work):
    if sys.version_info < (3, 9):
        raise Refused('fresh-machine cell requires Python 3.9 or newer')
    if platform.system() != 'Linux' or os.geteuid() != 0:
        raise Refused('fresh-machine cell requires root on a disposable Linux VM')
    for name in ('systemctl', 'journalctl', 'git', 'openssl', 'ssh', 'ssh-keygen', 'sshd', 'useradd', 'passwd'):
        if not shutil.which(name):
            raise Refused('fresh-machine host prerequisite is missing')
    if (os.environ.get('KUBECONFIG') or (Path.home() / '.kube/config').exists()
            or Path('/etc/rancher/k3s/k3s.yaml').exists() or Path('/usr/local/bin/k3s').exists()
            or Path('/etc/systemd/system/k3s.service').exists() or work.exists()):
        raise Refused('cell requires a kubeconfig-less fresh host and a new state directory')
    if not work.is_absolute() or any(c.isspace() for c in str(work)):
        raise Refused('state directory must be an absolute path without whitespace')
    if work.resolve().is_relative_to(Path(__file__).resolve().parents[2]):
        raise Refused('private machine state must be outside the checkout')


def start_cluster(cell):
    arch = {'x86_64': 'amd64', 'amd64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}.get(platform.machine(), '')
    install_tools(cell.work, arch)
    unit = Path('/etc/systemd/system/k3s.service')
    if unit.exists():
        raise Refused('fresh host already has a k3s service')
    unit.write_text('[Unit]\nAfter=network-online.target\nWants=network-online.target\n'
                    '[Service]\nType=notify\nExecStart=/usr/local/bin/k3s server --disable traefik '
                    '--write-kubeconfig-mode 600\nKillMode=process\nDelegate=yes\n'
                    'Restart=on-failure\nLimitNOFILE=1048576\nLimitNPROC=infinity\n'
                    '[Install]\nWantedBy=multi-user.target\n')
    cell.run('daemon-reload', ['systemctl', 'daemon-reload'])
    cell.run('start-k3s', ['systemctl', 'enable', '--now', 'k3s'], timeout=600)
    source = Path('/etc/rancher/k3s/k3s.yaml')
    deadline = time.monotonic() + 600
    while not source.exists() and time.monotonic() < deadline:
        time.sleep(2)
    if not source.exists():
        raise Refused('k3s did not create its private kubeconfig')
    shutil.copyfile(source, cell.work / 'kubeconfig')
    (cell.work / 'kubeconfig').chmod(0o600)
    cell.run('context', ['kubectl', '--kubeconfig', str(cell.work / 'kubeconfig'),
                        'config', 'rename-context', 'default', 'bootstrap-cell'])
    cell.k('wait', '--for=condition=Ready', 'nodes', '--all', '--timeout=600s')


def start_forge(cell):
    # A new fixture account and isolated sshd, never the host's SSH service or
    # root authorized_keys. Locked root accounts work too; only the fixture
    # account has an empty password, and password authentication is disabled.
    for name in ('forge-host', 'upstream', 'push'):
        cell.run('ssh-key', ['ssh-keygen', '-q', '-t', 'ed25519', '-N', '',
                            '-f', str(cell.work / (name + '.key'))])
    user = 'oberth-bootstrap'
    home = Path('/var/lib/oberth-bootstrap-forge')
    try:
        pwd.getpwnam(user)
    except KeyError:
        pass
    else:
        raise Refused('fixture SSH account already exists')
    if home.exists():
        raise Refused('fixture forge storage already exists')
    cell.run('forge-user', ['useradd', '--system', '--create-home', '--home-dir', str(home), '--shell', '/bin/sh', user])
    cell.run('forge-account', ['passwd', '-d', user])
    identity = pwd.getpwnam(user)
    home.chmod(0o700)
    key = (cell.work / 'upstream.key.pub').read_text().strip()
    git_shell = shutil.which('git-shell')
    if not git_shell:
        raise Refused('git-shell host prerequisite is missing')
    forced = 'restrict,command="' + git_shell + r' -c \"$SSH_ORIGINAL_COMMAND\"" '
    authorized = home / 'authorized_keys'
    authorized.write_text(forced + key + '\n')
    os.chown(authorized, identity.pw_uid, identity.pw_gid)
    authorized.chmod(0o600)
    node = json.loads(cell.k('get', 'nodes', '-o', 'json'))['items'][0]
    address = next(a['address'] for a in node['status']['addresses'] if a['type'] == 'InternalIP')
    config = cell.work / 'sshd_config'
    config.write_text('Port 32223\nListenAddress ' + address + '\nHostKey ' + str(cell.work / 'forge-host.key') +
                      '\nAuthorizedKeysFile ' + str(authorized) + '\nPermitRootLogin no\n'
                      'PasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\n'
                      'StrictModes yes\nAllowUsers ' + user + '\nPidFile ' + str(cell.work / 'sshd.pid') + '\n')
    Path('/run/sshd').mkdir(exist_ok=True)
    unit = Path('/etc/systemd/system/oberth-bootstrap-forge.service')
    if unit.exists():
        raise Refused('fresh host already has a fixture forge service')
    unit.write_text('[Unit]\nAfter=k3s.service\n[Service]\nExecStart=' + shutil.which('sshd') +
                    ' -D -e -f ' + str(config) + '\nRestart=on-failure\n[Install]\nWantedBy=multi-user.target\n')
    cell.run('forge-config', [shutil.which('sshd'), '-t', '-f', str(config)])
    cell.run('daemon-reload', ['systemctl', 'daemon-reload'])
    cell.run('start-forge', ['systemctl', 'enable', '--now', 'oberth-bootstrap-forge'])
    bare = home / 'workspace/fixture.git'
    bare.parent.mkdir(parents=True)
    cell.run('bare', ['git', 'init', '--bare', '--initial-branch=main', str(bare)])
    repo = cell.work / 'client'
    cell.run('clone', ['git', 'clone', str(bare), str(repo)])
    (repo / '.oberth').mkdir()
    (repo / '.oberth/build.yaml').write_text(WORKFLOW)
    (repo / 'expect.txt').write_text('fixed\n')
    cell.run('seed-add', ['git', '-C', str(repo), 'add', '.'])
    cell.run('seed-commit', ['git', '-C', str(repo), '-c', 'user.name=fixture',
                           '-c', 'user.email=fixture@example.invalid', 'commit', '-m', 'Seed fixture'])
    cell.run('seed', ['git', '-C', str(repo), 'push', 'origin', 'main'])
    for path in [home, *home.rglob('*')]:
        os.chown(path, identity.pw_uid, identity.pw_gid)
    fingerprint = cell.run('fingerprint', ['ssh-keygen', '-lf', str(cell.work / 'forge-host.key.pub')]).decode().split()[1]
    host = '[' + address + ']' if ':' in address else address
    return repo, 'ssh://' + user + '@' + host + ':32223' + str(bare.parent), fingerprint


def install(cell, version, answers):
    cell.run('fetch', ['sh', str(cell.plugin / 'fetch-oberth'), version])
    cell.run('client-key', ['sh', str(cell.plugin / 'trust'), 'keygen'])
    repo, forge, fingerprint = start_forge(cell)
    cfg = json.loads(answers.read_text()) if answers else {'version': 1, 'config': {}}
    if cfg.get('version') != 1 or not isinstance(cfg.get('config'), dict):
        raise Refused('answers require the current version/config schema')
    cfg['config'].update(AdminBearer=False, SecretsAgeRecipient='', InstallSecretStoreDev=False, ContextName='bootstrap-cell', InstallSecretStore=True, ClientHost='localhost',
                         ClientAccess='none', ForgeURL=forge, AssistEnabled=True,
                         ExpectedHostFingerprint=fingerprint, UpstreamPrivateKeyPath=str(cell.work / 'upstream.key'))
    config = cell.work / 'answers.json'
    config.write_text(json.dumps(cfg))
    csr = cell.work / 'claude/oberth/current/client.csr'
    common = ['--context', 'bootstrap-cell', '--kubeconfig', str(cell.work / 'kubeconfig'), '--answers', str(config)]
    plan = json.loads(cell.run('plan', ['sh', str(cell.plugin / 'install'), 'plan', *common]))
    verify_result(plan | {'context': plan['identity']['context']}, 'bootstrap-cell')
    result = json.loads(cell.run('install', ['sh', str(cell.plugin / 'install'), 'run', *common, '--yes', '--json',
                       '--secrets-file', str(cell.probe.secrets), '--admin-csr', str(csr),
                       '--admin-ssh-public-key', str(cell.work / 'push.key.pub')]))
    verify_result(result, 'bootstrap-cell')
    checked = json.loads(cell.run('verify', ['sh', str(cell.plugin / 'install'), 'verify', *common]))
    verify_result(checked, 'bootstrap-cell')
    cell.url = checked['mcp_url']
    # The endpoint is always DNS; CA constraints must never be bypassed for IPs.
    if not cell.url.startswith('https://localhost:'):
        raise Refused('fresh-machine result did not retain the local DNS endpoint')
    cell.run('trust', ['sh', str(cell.plugin / 'trust'), 'connect', '--url', cell.url, '--pin', checked['ca_sha256']])
    cell.run('client-cert', ['sh', str(cell.plugin / 'trust'), 'credentials'])
    cell.tool('system_status', {})
    namespace = cfg['config'].get('Namespace') or 'oberth'
    upstream = result['upstream']['name']
    cell.k('exec', '-n', namespace, 'deployment/oberth', '-c', 'oberth', '--', '/usr/local/bin/oberth',
           'repo', 'add', '--default-branch', 'main', 'fixture', upstream)
    canonical = upstream + '/workspace/fixture'
    installation = cell.work / 'installation.json'
    installation.write_text(json.dumps(checked))
    env = json.loads((cell.work / 'claude/settings.json').read_text())['env']
    cell.run('configure', ['sh', str(cell.plugin / 'add-repo'), 'configure', str(repo), 'remote',
                          '--installation', str(installation), '--registered-repo', canonical,
                          '--ssh-key', str(cell.work / 'push.key'), '--ca-file', env['NODE_EXTRA_CA_CERTS'],
                          '--client-cert', env['CLAUDE_CODE_CLIENT_CERT'], '--client-key', env['CLAUDE_CODE_CLIENT_KEY'],
                          '--assist', '--consent'])
    return held_roundtrip(cell, repo, canonical)


def arrange_reboot(cell, receipt):
    state = {'version': 1, 'boot_id': Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
             'mcp_url': cell.url, 'roundtrip': receipt, 'release_version': cell.version, 'source_sha': cell.source_sha}
    (cell.work / 'state.json').write_text(json.dumps(state))
    unit = Path('/etc/systemd/system/oberth-bootstrap-resume.service')
    unit.write_text('[Unit]\nAfter=k3s.service network-online.target\nWants=network-online.target\n'
                    '[Service]\nType=oneshot\nExecStart=/usr/bin/python3 -I -B ' +
                    str(cell.root / 'hack/bootstrap-acceptance/fresh_machine.py') + ' --resume ' + str(cell.work) +
                    '\nStandardOutput=null\nStandardError=null\n[Install]\nWantedBy=multi-user.target\n')
    cell.run('daemon-reload', ['systemctl', 'daemon-reload'])
    cell.run('resume-enable', ['systemctl', 'enable', 'oberth-bootstrap-resume'])
    cell.capture()
    cell.scan()
    cell.run('reboot', ['systemctl', 'reboot'])


def resume(cell):
    state = json.loads((cell.work / 'state.json').read_text())
    if state.get('version') != 1 or state['boot_id'] == Path('/proc/sys/kernel/random/boot_id').read_text().strip():
        raise Refused('recovery requires an actual new boot of the same disposable VM')
    cell.url = state['mcp_url']
    # Wait for verified pod selection and TLS access; a sealed initialized Bao
    # is required before unseal, not a server readiness shortcut.
    config = json.loads((cell.work / 'answers.json').read_text())['config']
    common = [cell.cli, 'unseal', '--context', 'bootstrap-cell', '--kubeconfig', str(cell.work / 'kubeconfig'),
              '--openbao-namespace', config.get('OpenBaoNamespace') or 'openbao']
    deadline = time.monotonic() + 600
    status = None
    while time.monotonic() < deadline:
        try:
            status = json.loads(cell.run('sealed', common + ['--status', '--json'], timeout=60))
            break
        except Refused:
            time.sleep(2)
    if not status or status.get('initialized') is not True or status.get('sealed') is not True:
        raise Refused('reboot did not leave initialized OpenBao sealed')
    cell.run('unseal', common + ['--secrets-file', str(cell.probe.secrets)])
    after = json.loads(cell.run('unsealed', common + ['--status', '--json']))
    if after.get('sealed') is not False or after.get('initialized') is not True:
        raise Refused('unseal did not recover initialized OpenBao')
    checked = json.loads(cell.run('verify-recovered', [cell.cli, 'install', '--verify', '--json',
                        '--context', 'bootstrap-cell', '--kubeconfig', str(cell.work / 'kubeconfig'),
                        '--answers', str(cell.work / 'answers.json')]))
    verify_result(checked, 'bootstrap-cell')
    cell.tool('system_status', {})
    retained = cell.tool('run_get', {'id': state['roundtrip']['run_id'], 'summary': False})
    verify_assisted_result(retained['run'], state['roundtrip']['sha'])
    cell.capture()
    cell.scan()
    (cell.work / 'PASSED.json').write_text(json.dumps({'version': 1, 'ok': True,
                                                    'roundtrip': state['roundtrip'], 'reboot_verified': True, 'release_version': state['release_version'], 'source_sha': state['source_sha']}))
    cell.run('resume-disable', ['systemctl', 'disable', 'oberth-bootstrap-resume'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', help='immutable signed Oberth release, vMAJOR.MINOR.PATCH')
    parser.add_argument('--answers', type=Path, help='optional nonsecret version-1 JSON installer answers')
    parser.add_argument('--state-dir', type=Path, default=Path('/var/lib/oberth-bootstrap-cell'))
    parser.add_argument('--confirm-disposable-host', action='store_true')
    parser.add_argument('--resume', type=Path, help=argparse.SUPPRESS)
    args = parser.parse_args()
    os.umask(0o077)
    if args.resume:
        work = args.resume
        info = work.lstat()
        if os.geteuid() != 0 or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o700 or not stat.S_ISDIR(info.st_mode):
            raise Refused('unsafe recovery state directory')
        cell = Cell(work, work / 'source')
    else:
        if not args.confirm_disposable_host or not args.version:
            raise Refused('explicit disposable-host confirmation and release version are required')
        work = args.state_dir
        require_fresh_host(work)
        work.mkdir(mode=0o700, parents=True)
        root = Path(__file__).resolve().parents[2]
        shutil.copytree(root / 'claude-plugin', work / 'source/claude-plugin')
        shutil.copytree(Path(__file__).resolve().parent, work / 'source/hack/bootstrap-acceptance',
                        ignore=shutil.ignore_patterns('__pycache__'))
        cell = Cell(work, work / 'source')
        cell.version = args.version
        cell.source_sha = cell.run('source-sha', ['git', '-C', str(root), 'rev-parse', 'HEAD']).decode().strip()
        if cell.run('source-clean', ['git', '-C', str(root), 'status', '--porcelain']):
            raise Refused('fresh-machine cell requires a clean exact source commit')
    try:
        if args.resume:
            resume(cell)
        else:
            start_cluster(cell)
            arrange_reboot(cell, install(cell, args.version, args.answers))
    finally:
        cell.scan()


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('Fresh-machine bootstrap cell failed; private state withheld. Destroy the disposable VM.', file=sys.stderr)
        sys.exit(1)
