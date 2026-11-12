"""Pinned host tools for the disposable-machine cell, never the Oberth CLI."""
import hashlib
import json
from pathlib import Path
import tarfile
import urllib.parse
import urllib.request

from probe import Refused


def verified_download(url, destination, digest, bound):
    # Fixed caller-owned HTTPS coordinates and committed hashes, not a mutable
    # checksum fetched beside the executable. Refuse redirects to plain HTTP.
    with urllib.request.urlopen(url, timeout=300) as response:
        if urllib.parse.urlsplit(response.url).scheme != 'https':
            raise Refused('tool transport is not HTTPS')
        with destination.open('xb') as stream:
            size = 0
            check = hashlib.sha256()
            while chunk := response.read(65536):
                size += len(chunk)
                if size > bound:
                    raise Refused('tool exceeds download bound')
                stream.write(chunk)
                check.update(chunk)
    if check.hexdigest() != digest:
        raise Refused('pinned tool digest mismatch')


def helm_binary(archive, arch):
    # Read only the one expected regular file. Never extract archive paths.
    with tarfile.open(archive, 'r:gz') as tar:
        entries = [entry for entry in tar if entry.name == 'linux-' + arch + '/helm']
        if len(entries) != 1 or not entries[0].isfile() or entries[0].size > 128 << 20:
            raise Refused('pinned Helm archive has no unique regular executable')
        stream = tar.extractfile(entries[0])
        return stream.read(128 << 20)


def install_tools(work, arch):
    pins = json.loads(Path(__file__).with_name('pins.json').read_text())
    if arch not in ('amd64', 'arm64'):
        raise Refused('fresh machine requires amd64 or arm64')
    binary = work / 'k3s'
    name = 'k3s' if arch == 'amd64' else 'k3s-arm64'
    version = urllib.parse.quote(pins['k3s']['version'], safe='')
    verified_download('https://github.com/k3s-io/k3s/releases/download/' + version + '/' + name,
                      binary, pins['k3s'][arch], 256 << 20)
    archive = work / 'helm.tar.gz'
    verified_download('https://get.helm.sh/helm-' + pins['helm']['version'] + '-linux-' + arch + '.tar.gz',
                      archive, pins['helm'][arch], 64 << 20)
    for source, target in [(binary, Path('/usr/local/bin/k3s'))]:
        if target.exists() or target.is_symlink():
            raise Refused('fresh host already has a k3s binary')
        target.write_bytes(source.read_bytes())
        target.chmod(0o755)
    helm = work / 'bin/helm'
    helm.parent.mkdir()
    helm.write_bytes(helm_binary(archive, arch))
    helm.chmod(0o755)
    (work / 'bin/kubectl').symlink_to('/usr/local/bin/k3s')
