"""Declare product effort separately from fixture authoring and verification."""
from dataclasses import dataclass
import json
import shlex
from types import MappingProxyType
from walkassert import TEST_EMAIL, TEST_IDENTITY, TEST_NAME

SOURCE_LOADER = "init-artifact-oberth"


@dataclass(frozen=True)
class Command:
    role: str
    argv: tuple
    harness_flags: tuple = ()


COMMANDS = MappingProxyType({
    'key': Command('product', ('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', TEST_IDENTITY, '-f', '{private}/{key}')),
    'csr': Command('product', ('openssl', 'req', '-new', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1', '-nodes', '-keyout', '{private}/{client}.key', '-out', '{private}/{client}.csr', '-subj', "/CN=" + TEST_NAME)),
    'fingerprint': Command('product', ('ssh-keygen', '-lf', '{known_hosts}')),
    'git-init': Command('harness', ('git', 'init', '-b', 'main')),
    'git-name': Command('harness', ('git', 'config', 'user.name', TEST_NAME)),
    'git-email': Command('harness', ('git', 'config', 'user.email', TEST_EMAIL)),
    'git-origin': Command('harness', ('git', 'remote', 'add', 'origin', '{origin}')),
    'git-initialize': Command('harness', ('git', 'commit', '--allow-empty', '-m', 'Initialize disposable walk repository')),
    'git-push': Command('product', ('git', 'push', '{remote}', '{ref}')),
    'git-add': Command('harness', ('git', 'add', '{path}')),
    'git-commit': Command('harness', ('git', 'commit', '-m', '{message}')),
    'git-clone': Command('harness', ('git', 'clone', '--no-local', '{admin_repo}', '{agent_repo}')),
    'git-fetch': Command('verification', ('git', 'fetch', 'origin', 'main')),
    'git-tag': Command('product', ('git', 'tag', '-a', '{tag}', '{release_sha}', '-m', 'Disposable walk release')),
    'git-checkout': Command('harness', ('git', 'checkout', '--detach', '{sha}')),
    'profile': Command('product', ('tee', '{profile_file}')),
    'install': Command('product', ('oberth', 'install', '--kubeconfig', '{kube}', '--context', '{context}', '--yes', '--json', '--install-secretstore', '--secrets-file', '{private}/install.secrets', '--upstream-name', 'walk', '--upstream-url', '{upstream_url}', '--upstream-key-file', '{private}/upstream', '--expected-host-fingerprint', '{fingerprint}', '--uplink-identity', TEST_EMAIL, '--admin-csr', '{private}/admin.csr', '--admin-ssh-public-key', '{private}/admin-push.pub', '--ssh-public-key', '{private}/admin-push.pub', '--client-access', 'cli', '--client-host', 'localhost', '--client-ssh-port', '{ssh}', '--client-https-port', '{https}', '{resume}'), ('--json',)),
    'upstream': Command('product', ('oberth', 'upstream', 'add', '--yes', '--expected-host-fingerprint', '{fingerprint}', '--check-repo', '{name}', 'walk', '{upstream_url}', '--json'), ('--json',)),
    'repo': Command('product', ('oberth', 'repo', 'add', '{canonical}', 'walk')),
    'bind': Command('product', ('oberth', 'client', 'repository', '--path', '{repo}', '--installation', '{private}/install.json', '--registered-repo', '{canonical}', '--ssh-key', '{private}/{client}-push', '--client-cert', '{private}/{client}.csr.crt', '--client-key', '{private}/{client}.key', '--ca-file', '{ca}', '--consent')),
    'invite': Command('product', ('oberth', 'uplink', 'invite', TEST_EMAIL.replace("@", "+agent@"), '--scope', 'walk', '--expires', '1h', '--out', '{private}/invite.json')),
    'redeem': Command('product', ('oberth', 'client', 'redeem', '--invite-file', '{private}/invite.json', '--csr', '{private}/agent.csr', '--ssh-public-key', '{private}/agent-push.pub', '--certificate-out', '{private}/agent.csr.crt')),
    'log-command': Command('product', ('oberth', 'log', '{selector}', '{options}')),
    'log-follow': Command('product', ('oberth', 'log', '{id}')),
    'log-retained': Command('verification', ('oberth', 'log', '{id}', '--limit', '0')),
    'runs': Command('verification', ('oberth', 'runs', '--repo', '{canonical}', '--json')),
    'run-view': Command('verification', ('oberth', 'run', '{id}', '--json')),
    'wait': Command('product', ('oberth', 'run', 'wait', '{id}', '--until', 'finished', '--timeout', '{timeout}', '--json'), ('--json', '--until', '--timeout')),
    'promotion': Command('product', ('oberth', 'promotion', 'create', '--repo', '{canonical}', '--branch', 'main', '--sources-file', '{private}/promotion.json', '--json'), ('--json',)),
    'sync': Command('product', ('oberth', 'secretstore', 'sync', '--context', '{context}')),
    'upgrade': Command('product', ('oberth', 'upgrade', '--context', '{context}', '--yes', '--no-backup', '--backup-dir', '{private}/upgrade')),
    'store': Command('product', ('kubectl', '--kubeconfig', '{kube}', '--context', '{context}', '-n', 'openbao', 'exec', '-i', 'openbao-0', '--', 'sh', '-ec', '{store_script}')),
    'git-seed': Command('harness', ('git', 'push', '{remote}', '{ref}')),
    'wait-promotion': Command('verification', ('oberth', 'run', 'wait', '{id}', '--until', 'finished', '--timeout', '{timeout}', '--json')),
    'namespace-delete': Command('harness', ('kubectl', '--kubeconfig', '{kube}', '--context', '{context}', 'delete', 'namespace', 'oberth', '--wait=true', '--timeout=180s')),
    'workflows-delete': Command('harness', ('kubectl', '--kubeconfig', '{kube}', '--context', '{context}', '-n', 'oberth-argo', 'delete', 'workflows', '--all', '--wait=true')),
    'uplink_list': Command('verification', ('oberth', 'mcp', 'call', 'uplink_list', '{arguments}')),
    'run_logs': Command('verification', ('oberth', 'mcp', 'call', 'run_logs', '{arguments}')),
    'run_get': Command('verification', ('oberth', 'mcp', 'call', 'run_get', '{arguments}')),
    'events_wait': Command('verification', ('oberth', 'mcp', 'call', 'events_wait', '{arguments}')),
    'promote_status': Command('product', ('oberth', 'mcp', 'call', 'promote_status', '{arguments}')),
    'access_allow': Command('product', ('oberth', 'mcp', 'call', 'access_allow', '{arguments}')),
    'store-read': Command('product', ('read', '-r', 'BAO_TOKEN')),
    'store-export': Command('product', ('export', 'BAO_TOKEN')),
    'store-put': Command('product', ('bao', 'kv', 'put', '-mount=oberth', 'release/walk', '-')),
    'pod-watch': Command('verification', ('kubectl', '--kubeconfig', '{kube}', '--context', '{context}', 'get', 'pods', '--all-namespaces', '--watch', '--output-watch-events', '-o', 'json')),
    'server-scale': Command('harness', ('kubectl', '--kubeconfig', '{kube}', '--context', '{context}', '-n', 'oberth', 'scale', 'deployment', 'oberth', '--replicas={replicas}')),
    'workflow-resume': Command('harness', ('kubectl', '--kubeconfig', '{kube}', '--context', '{context}', '-n', 'oberth-argo', 'patch', 'workflow', '{workflow}', '--type=merge', '-p', '{{"spec":{{"suspend":false}}}}')),
    'restart-load-log': Command('verification', ('kubectl', '--kubeconfig', '{kube}', '--context', '{context}', '-n', 'oberth-argo', 'logs', '{pod}', '-c', SOURCE_LOADER, '--timestamps')),
    'restart-step-log': Command('verification', ('kubectl', '--kubeconfig', '{kube}', '--context', '{context}', '-n', 'oberth-argo', 'logs', '{pod}', '-c', 'main')),
    'restart-run-wait': Command('verification', ('oberth', 'run', 'wait', '{id}', '--until', 'finished', '--timeout', '{timeout}', '--json')),
    'workflow-watch': Command('verification', ('kubectl', '--kubeconfig', '{kube}', '--context', '{context}', '-n', 'oberth-argo', 'get', 'workflows', '--watch', '--output-watch-events', '-o', 'json')),
})
STORE_COMMANDS = ("store-read", "store-export", "store-put")


def render(s, name, values):
    common = {**s.config.values, **vars(s.data), "private": s.private, "kube": s.config.kube,
              "context": s.config.context, "canonical": s.canonical, **s.config.ports,
              "known_hosts": s.config.path("known_hosts"), "timeout": s.args.run_timeout,
              "admin_repo": s.admin_repo, "agent_repo": s.agent_repo,
              **values, "store_script": "; ".join(shlex.join(COMMANDS[c].argv) for c in STORE_COMMANDS)}
    argv = []
    for token in COMMANDS[name].argv:
        value = common.get(token[1:-1]) if token.startswith("{") and token.endswith("}") else None
        if isinstance(value, (tuple, list)):
            argv.extend(str(v) for v in value)
        else:
            argv.append(token.format_map(common))
    return argv


def flags(argv, operation=None):
    excluded = COMMANDS[operation].harness_flags if operation else ()
    result = [a.split("=", 1)[0] for a in argv if a.startswith("-") and a != "--"
              and a.split("=", 1)[0] not in excluded]
    if argv[:3] == ["oberth", "mcp", "call"]:
        result.extend(json.loads(argv[4]))
    return result
