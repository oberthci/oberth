"""Assemble deterministic OCI bytes from the two compiled Git artifacts."""
import hashlib
import gzip
import io
import json
import tarfile
from pathlib import Path

MEDIA = 'application/vnd.oci.'


def digest(body):
    return 'sha256:' + hashlib.sha256(body).hexdigest()


def blob(root, body, media):
    value = digest(body)
    (root / 'blobs' / 'sha256' / value[7:]).write_bytes(body)
    return {'mediaType': MEDIA + media, 'digest': value, 'size': len(body)}


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':')).encode()


def layer(artifacts):
    output = io.BytesIO()
    directories = ['data', 'etc', 'etc/ssl', 'etc/ssl/certs', 'tmp', 'usr', 'usr/bin', 'usr/local', 'usr/local/bin']
    files = {'usr/bin/git': (artifacts / 'git').read_bytes(),
             'etc/ssl/certs/ca-certificates.crt': (artifacts / 'ca-certificates.crt').read_bytes()}
    with tarfile.open(fileobj=output, mode='w', format=tarfile.USTAR_FORMAT) as archive:
        for name in sorted(directories + list(files)):
            entry = tarfile.TarInfo(name)
            entry.mode = 0o755
            if name in directories:
                entry.type = tarfile.DIRTYPE
                if name == 'data':
                    entry.uid = entry.gid = 65534
                if name == 'tmp':
                    entry.mode = 0o1777
                archive.addfile(entry)
            else:
                body = files[name]
                entry.size = len(body)
                entry.mode = 0o755 if name == 'usr/bin/git' else 0o644
                archive.addfile(entry, io.BytesIO(body))
    return output.getvalue()


def assemble(artifacts, destination):
    destination = Path(destination)
    (destination / 'blobs' / 'sha256').mkdir(parents=True, exist_ok=True)
    manifests = []
    for architecture in ['amd64', 'arm64']:
        body = layer(Path(artifacts) / architecture)
        compressed = io.BytesIO()
        with gzip.GzipFile(filename='', mode='wb', fileobj=compressed, mtime=0) as writer:
            writer.write(body)
        content = blob(destination, compressed.getvalue(), 'image.layer.v1.tar+gzip')
        config = {'architecture': architecture, 'os': 'linux',
                  'rootfs': {'type': 'layers', 'diff_ids': [digest(body)]}}
        configuration = blob(destination, encoded(config), 'image.config.v1+json')
        manifest = {'schemaVersion': 2, 'mediaType': MEDIA + 'image.manifest.v1+json',
                    'config': configuration, 'layers': [content]}
        descriptor = blob(destination, encoded(manifest), 'image.manifest.v1+json')
        descriptor['platform'] = {'architecture': architecture, 'os': 'linux'}
        manifests.append(descriptor)
    index = encoded({'schemaVersion': 2, 'mediaType': MEDIA + 'image.index.v1+json', 'manifests': manifests})
    (destination / 'index.json').write_bytes(index)
    (destination / 'oci-layout').write_bytes(encoded({'imageLayoutVersion': '1.0.0'}))
    return digest(index)
