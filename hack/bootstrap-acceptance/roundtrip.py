"""Exact-commit held-run exercise, used only on the disposable Linux host."""
import concurrent.futures
import json
import time

from probe import Refused

# An immutable multi-platform index supports both fresh-VM architectures.
IMAGE = 'golang:1.26.10-trixie@sha256:fd9a91f0ece5664556f4851d06dce5e5b505a71166dffd385a03ba9d200b6983'
WORKFLOW = '''apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: test
  activeDeadlineSeconds: 600
  templates:
  - name: test
    automountServiceAccountToken: false
    metadata:
      annotations:
        oberth.ci/workspace-mounts: none
        oberth.ci/workspace-env: none
    container:
      image: IMAGE
      command: [/bin/sh, -ec]
      args: ['test "$(cat /work/src/expect.txt)" = fixed']
      resources:
        requests: {cpu: 100m, memory: 128Mi, ephemeral-storage: 512Mi}
        limits: {cpu: '1', memory: 512Mi, ephemeral-storage: 512Mi}
'''.replace('IMAGE', IMAGE)


def held_roundtrip(cell, repo, canonical):
    branch = 'feature/bootstrap-cell'
    (repo / 'expect.txt').write_text('broken\n')
    cell.run('git-add', ['git', '-C', str(repo), 'add', 'expect.txt'])
    cell.run('git-commit', ['git', '-C', str(repo), '-c', 'user.name=fixture',
                          '-c', 'user.email=fixture@example.invalid', 'commit', '-m', 'Exercise held run'])
    sha = cell.run('git-sha', ['git', '-C', str(repo), 'rev-parse', 'HEAD']).decode().strip()
    page = cell.tool('events_since', {'after_id': 0, 'repos': [canonical], 'limit': 1000})
    cursor = page['latest_id']
    # Cursor is pinned before the push, so even an immediately returned event
    # cannot be missed when the background wait and SSH exchange overlap.
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
        waiting = pool.submit(cell.tool, 'events_wait', {'after_id': cursor, 'repos': [canonical],
                             'refs_glob': branch, 'kinds': ['run.held', 'run.finished'], 'timeout_s': 30})
        cell.run('push', ['git', '-C', str(repo), 'push', '-o', 'oberth.assist=1',
                         'oberth', 'HEAD:refs/heads/' + branch])
        deadline = time.monotonic() + 900
        while time.monotonic() < deadline:
            page = waiting.result(timeout=65)
            if page.get('gap'):
                raise Refused('held-run event cursor has a retention gap')
            for event in page['events']:
                if event.get('kind') == 'run.held':
                    run_id = event.get('run_id')
                    return finish_hold(cell, run_id, sha)
                if event.get('kind') == 'run.finished':
                    raise Refused('fixture finished without reaching a held step')
            waiting = pool.submit(cell.tool, 'events_wait', {'after_id': page['next_after_id'],
                                  'repos': [canonical], 'refs_glob': branch,
                                  'kinds': ['run.held', 'run.finished'], 'timeout_s': 30})
    raise Refused('held-run fixture exceeded its deadline')


def finish_hold(cell, run_id, sha):
    detail = cell.tool('run_get', {'id': run_id, 'summary': False})
    if detail['run']['sha'] != sha:
        raise Refused('hold does not belong to the exact pushed commit')
    holds = cell.tool('run_hold_list', {'id': run_id})['holds']
    if len(holds) != 1 or holds[0].get('patch_capable') is not True:
        raise Refused('fixture did not produce one patch-capable hold')
    step = holds[0]['step']
    cell.tool('run_hold_patch', {'id': run_id, 'step': step,
              'diff': '--- a/expect.txt\n+++ b/expect.txt\n@@ -1 +1 @@\n-broken\n+fixed\n'})
    cell.tool('run_continue', {'id': run_id, 'step': step})
    deadline = time.monotonic() + 900
    while time.monotonic() < deadline:
        detail = cell.tool('run_get', {'id': run_id, 'summary': False})
        run = detail['run']
        if run['status'] not in ('queued', 'running'):
            verify_assisted_result(run, sha)
            return {'sha': sha, 'run_id': run_id}
        time.sleep(2)
    raise Refused('continued run exceeded its deadline')


def verify_assisted_result(run, sha):
    if (run['sha'] != sha or run['status'] != 'passed' or run.get('assisted') is not True
            or 'not evidence' not in run.get('display_reason', '')):
        raise Refused('assisted result lost exact SHA, pass or non-evidence classification')
