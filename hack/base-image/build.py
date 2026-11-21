#!/usr/bin/env python3
"""Fetch hash-pinned inputs, compile offline in Docker and assemble an OCI base."""
import argparse
import hashlib
import json
import shutil
import subprocess
import time
import urllib.request
from pathlib import Path


HERE = Path(__file__).resolve().parent
REPOSITORY = HERE.parents[1]


def build_inputs():
    return dict(line.split('=', 1) for line in
                (REPOSITORY / 'internal/staticgit/build.env').read_text().splitlines()
                if line and not line.startswith('#'))


def fetch(item, destination):
    destination.parent.mkdir(parents=True, exist_ok=True)
    download = not destination.exists()
    source = destination.with_name(destination.name + '.part') if download else destination
    if download:
        request = urllib.request.Request(item['url'])
        with urllib.request.urlopen(request, timeout=60) as response, source.open('wb') as output:
            shutil.copyfileobj(response, output)
    if hashlib.sha256(source.read_bytes()).hexdigest() != item['sha256']:
        raise ValueError('Pinned input differs: ' + str(source) + '; delete the file and rerun.')
    if download:
        source.replace(destination)


def context(workspace, architecture, lock):
    root = workspace / 'contexts' / architecture
    packages = root / 'packages'
    packages.mkdir(parents=True, exist_ok=True)
    names = {item['file'] for item in lock['platforms'][architecture]['packages']}
    unlisted = {entry.name for entry in packages.iterdir()} - names
    if unlisted:
        raise ValueError('Unlisted package input: ' + ', '.join(sorted(unlisted)) + '; move it aside and rerun.')
    for item in lock['platforms'][architecture]['packages']:
        fetch(item, packages / item['file'])
    pins = build_inputs()
    fetch(dict(lock['git'], sha256=pins['GIT_SOURCE_SHA256']), root / 'git.tar.xz')
    patch = HERE / 'git-no-shell.patch'
    if hashlib.sha256(patch.read_bytes()).hexdigest() != pins['GIT_PATCH_SHA256']:
        raise ValueError('Git patch differs from its reviewed source input.')
    for name in ['compile.sh', 'git-no-shell.patch']:
        shutil.copyfile(HERE / name, root / name)
    shutil.copyfile(REPOSITORY / 'internal/staticgit/build.env', root / 'build.env')
    shutil.copyfile(HERE / 'Dockerfile', root / 'Dockerfile')
    return root


def comparison(architecture, artifacts, expected):
    observed = {'architecture': architecture}
    for key, name in [('git_sha256', 'git'), ('musl_archive_sha256', 'musl.a'), ('zlib_archive_sha256', 'zlib.a')]:
        observed[key] = hashlib.sha256((artifacts / name).read_bytes()).hexdigest()
    return {'observed': observed, 'reviewed': expected, 'matches': observed == expected}


def assemble(workspace, artifacts, lock):
    source = workspace / 'assembly-context'
    source.mkdir(exist_ok=True)
    shutil.copyfile(HERE / 'layout.py', source / 'layout.py')
    shutil.copyfile(HERE / 'Dockerfile.layout', source / 'Dockerfile')
    for architecture in ['amd64', 'arm64']:
        target = source / 'artifacts' / architecture
        target.mkdir(parents=True, exist_ok=True)
        for name in ['git', 'ca-certificates.crt']:
            shutil.copyfile(artifacts / architecture / name, target / name)
    command = ['docker', 'buildx', 'build', '--platform=linux/amd64', '--network=none',
               '--build-arg=ASSEMBLER_IMAGE=' + lock['assembler_image'],
               '--progress=plain', '--output=type=local,dest=' + str(workspace / 'base-image'), str(source)]
    with (workspace / 'assembly.log').open('w') as log:
        subprocess.run(command, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=300)
    return 'sha256:' + hashlib.sha256((workspace / 'base-image/index.json').read_bytes()).hexdigest()


def run(workspace):
    lock = json.loads((HERE / 'inputs.lock.json').read_text())
    expected = json.loads((REPOSITORY / 'internal/staticgit/qualified-artifacts.json').read_text())
    workspace.mkdir(parents=True, exist_ok=True)
    artifacts = workspace / 'artifacts'
    receipts = {}
    for architecture in ['amd64', 'arm64']:
        source = context(workspace, architecture, lock)
        output = artifacts / architecture
        platform = lock['platforms'][architecture]
        packages = ' '.join('/inputs/packages/' + item['file'] for item in
                            sorted(platform['packages'], key=lambda item: item['file']))
        command = ['docker', 'buildx', 'build', '--platform=linux/' + architecture,
                   '--build-arg=BUILDER_IMAGE=' + platform['builder'],
                   '--build-arg=PACKAGE_FILES=' + packages,
                   '--network=none', '--progress=plain', '--output=type=local,dest=' + str(output), str(source)]
        start = time.monotonic()
        with (workspace / (architecture + '.log')).open('w') as log:
            subprocess.run(command, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=5400)
        receipts[architecture] = comparison(architecture, output, expected[architecture])
        receipts[architecture]['elapsed_s'] = round(time.monotonic() - start, 3)
        (workspace / 'build-report.json').write_text(json.dumps(receipts, indent=2) + '\n')
        print(architecture + ': ' + ('reviewed hashes match' if receipts[architecture]['matches'] else 'REQUALIFICATION REQUIRED'), flush=True)
    index = assemble(workspace, artifacts, lock)
    (workspace / 'candidate.digest').write_text(index + '\n')
    print('Candidate base index: ' + index, flush=True)
    print('Run the existing offline checker before writing the canonical pin.', flush=True)
    if not all(value['matches'] for value in receipts.values()):
        raise SystemExit('Build differs from reviewed artifacts; retain build-report.json for qualification review.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('workspace', type=Path)
    args = parser.parse_args()
    run(args.workspace.resolve())
