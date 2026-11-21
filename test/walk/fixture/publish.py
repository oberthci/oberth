"""Publish the built fixture as an image through the local registry API."""
import gzip
import hashlib
import io
import json
import os
import tarfile
import urllib.parse
import urllib.request
from pathlib import Path


def digest(body):
    return "sha256:" + hashlib.sha256(body).hexdigest()


def image_content(binary):
    archive = io.BytesIO()
    with tarfile.open(fileobj=archive, mode="w") as tar:
        info = tarfile.TarInfo("walk-app")
        info.size = len(binary)
        info.mode = 0o755
        tar.addfile(info, io.BytesIO(binary))
    raw_layer = archive.getvalue()
    layer = gzip.compress(raw_layer, mtime=0)
    config = json.dumps(
        {
            "architecture": "amd64",
            "os": "linux",
            "config": {"Entrypoint": ["/walk-app"], "User": "65532:65532"},
            "rootfs": {"type": "layers", "diff_ids": [digest(raw_layer)]},
        },
        separators=(",", ":"),
    ).encode()
    return layer, config


def publish(registry, name, tag, binary):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    base = "http://" + registry

    def request(path, body=None, method=None, content_type=None):
        url = urllib.parse.urljoin(base, path)
        assert urllib.parse.urlsplit(url).netloc == registry
        headers = {"Accept": "application/vnd.oci.image.manifest.v1+json"}
        if content_type:
            headers["Content-Type"] = content_type
        with opener.open(
            urllib.request.Request(url, data=body, method=method, headers=headers), timeout=60
        ) as response:
            return response.headers, response.read()

    def upload(body):
        headers, _ = request(f"/v2/{name}/blobs/uploads/", b"", "POST")
        location = headers["Location"]
        separator = "&" if "?" in location else "?"
        request(
            location + separator + urllib.parse.urlencode({"digest": digest(body)}),
            body,
            "PUT",
            "application/octet-stream",
        )

    layer, config = image_content(binary)
    upload(layer)
    upload(config)
    manifest = json.dumps(
        {
            "schemaVersion": 2,
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "config": {
                "mediaType": "application/vnd.oci.image.config.v1+json",
                "digest": digest(config),
                "size": len(config),
            },
            "layers": [
                {
                    "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
                    "digest": digest(layer),
                    "size": len(layer),
                }
            ],
        },
        separators=(",", ":"),
    ).encode()
    request(f"/v2/{name}/manifests/{tag}", manifest, "PUT", "application/vnd.oci.image.manifest.v1+json")
    _, stored = request(f"/v2/{name}/manifests/{tag}", content_type=None)
    assert digest(stored) == digest(manifest)
    return f"{registry}/{name}@{digest(manifest)}"


if __name__ == "__main__":
    ref = publish(
        os.environ["WALK_REGISTRY"],
        os.environ["WALK_IMAGE"],
        os.environ["RELEASE_TAG"],
        Path("/tmp/walk-app").read_bytes(),
    )
    Path("/tmp/image-ref").write_text(ref)
    print("walk: image pushed " + ref)
