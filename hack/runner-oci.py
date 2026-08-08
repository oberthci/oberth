#!/usr/bin/env python3
"""Safely extract and validate the single-image OCI layouts used by Oberth."""

import hashlib
import json
import os
import pathlib
import re
import shutil
import sys
import tarfile


ALLOWED_DIRECTORIES = {"blobs", "blobs/sha256"}
ALLOWED_FILES = {"oci-layout", "index.json"}
BLOB_PATTERN = re.compile(r"blobs/sha256/[0-9a-f]{64}\Z")
DIGEST_PATTERN = re.compile(r"sha256:([0-9a-f]{64})\Z")


class OCIError(ValueError):
    pass


def fail(label, message):
    raise OCIError(f"{label}: {message}")


def extract(archive, destination, label):
    if os.path.islink(destination):
        fail(label, "OCI layout destination is a symlink")
    os.makedirs(destination, mode=0o700, exist_ok=True)
    seen = set()
    try:
        with tarfile.open(archive, mode="r:*") as source:
            for member in source:
                raw_name = member.name
                name = raw_name
                while name.startswith("./"):
                    name = name[2:]
                path = pathlib.PurePosixPath(name)
                if not name or path.is_absolute() or ".." in path.parts or "." in path.parts:
                    fail(label, f"unsafe OCI archive member {raw_name!r}")
                if name in seen:
                    fail(label, f"duplicate OCI archive member {name!r}")
                seen.add(name)
                if member.isdir():
                    if name not in ALLOWED_DIRECTORIES:
                        fail(label, f"unsafe OCI archive member {raw_name!r}")
                    os.makedirs(os.path.join(destination, *path.parts), mode=0o700, exist_ok=True)
                    continue
                if not member.isfile():
                    fail(label, f"unsafe non-regular OCI archive member {raw_name!r}")
                if name not in ALLOWED_FILES and not BLOB_PATTERN.fullmatch(name):
                    fail(label, f"unsafe OCI archive member {raw_name!r}")
                target = os.path.join(destination, *path.parts)
                os.makedirs(os.path.dirname(target), mode=0o700, exist_ok=True)
                body = source.extractfile(member)
                if body is None:
                    fail(label, f"unreadable OCI archive member {raw_name!r}")
                with open(target, "xb") as output:
                    shutil.copyfileobj(body, output)
                os.chmod(target, 0o600)
    except (OSError, tarfile.TarError) as error:
        fail(label, f"cannot extract OCI archive: {error}")
    for required in ALLOWED_FILES:
        if required not in seen:
            fail(label, f"OCI archive is missing {required}")


def load_json(path, label, description):
    try:
        with open(path, "rb") as source:
            value = json.load(source)
    except (OSError, ValueError) as error:
        fail(label, f"invalid {description}: {error}")
    if not isinstance(value, dict):
        fail(label, f"invalid {description}: expected an object")
    return value


def verify_descriptor(layout, descriptor, label, description):
    if not isinstance(descriptor, dict):
        fail(label, f"invalid {description} descriptor")
    digest = descriptor.get("digest")
    size = descriptor.get("size")
    match = DIGEST_PATTERN.fullmatch(digest) if isinstance(digest, str) else None
    if match is None or isinstance(size, bool) or not isinstance(size, int) or size < 0:
        fail(label, f"invalid {description} digest or size")
    path = os.path.join(layout, "blobs", "sha256", match.group(1))
    if not os.path.isfile(path) or os.path.islink(path):
        fail(label, f"missing {description} blob {digest}")
    if os.path.getsize(path) != size:
        fail(label, f"{description} blob {digest} has an unexpected size")
    hasher = hashlib.sha256()
    with open(path, "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            hasher.update(chunk)
    if hasher.hexdigest() != match.group(1):
        fail(label, f"{description} blob {digest} failed SHA-256 verification")
    return digest, path


def validate(layout, expected_manifest, label):
    if not os.path.isdir(layout) or os.path.islink(layout):
        fail(label, "OCI layout is not a regular directory")
    for root, directories, files in os.walk(layout, followlinks=False):
        relative_root = os.path.relpath(root, layout)
        for name in directories:
            path = os.path.join(root, name)
            relative = os.path.normpath(os.path.join(relative_root, name)).replace(os.sep, "/")
            if relative.startswith("./"):
                relative = relative[2:]
            if os.path.islink(path) or relative not in ALLOWED_DIRECTORIES:
                fail(label, f"unexpected OCI layout entry {relative!r}")
        for name in files:
            path = os.path.join(root, name)
            relative = os.path.normpath(os.path.join(relative_root, name)).replace(os.sep, "/")
            if relative.startswith("./"):
                relative = relative[2:]
            if os.path.islink(path) or (relative not in ALLOWED_FILES and not BLOB_PATTERN.fullmatch(relative)):
                fail(label, f"unexpected OCI layout entry {relative!r}")

    oci_layout = load_json(os.path.join(layout, "oci-layout"), label, "oci-layout")
    if oci_layout.get("imageLayoutVersion") != "1.0.0":
        fail(label, "unsupported OCI layout version")
    index = load_json(os.path.join(layout, "index.json"), label, "index.json")
    if "subject" in index:
        fail(label, "subject descriptors are outside the single-image OCI contract")
    manifests = index.get("manifests")
    if index.get("schemaVersion") != 2 or not isinstance(manifests, list) or len(manifests) != 1:
        fail(label, "index must contain exactly one schema-2 manifest")
    if manifests[0].get("mediaType") not in {
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    }:
        fail(label, "unsupported manifest media type")
    manifest_digest, manifest_path = verify_descriptor(layout, manifests[0], label, "manifest")
    if expected_manifest != "-" and manifest_digest != expected_manifest:
        fail(label, f"manifest is {manifest_digest}, expected {expected_manifest}")
    manifest = load_json(manifest_path, label, "image manifest")
    if "subject" in manifest:
        fail(label, "subject descriptors are outside the single-image OCI contract")
    layers = manifest.get("layers")
    if manifest.get("schemaVersion") != 2 or not isinstance(layers, list) or not layers:
        fail(label, "image manifest must contain at least one layer")
    config_digest, _ = verify_descriptor(layout, manifest.get("config"), label, "config")
    reachable = {manifest_digest.removeprefix("sha256:"), config_digest.removeprefix("sha256:")}
    for position, layer in enumerate(layers):
        layer_digest, _ = verify_descriptor(layout, layer, label, f"layer {position}")
        reachable.add(layer_digest.removeprefix("sha256:"))
    blob_directory = pathlib.Path(layout, "blobs", "sha256")
    try:
        actual_blobs = {entry.name for entry in blob_directory.iterdir() if entry.is_file() and not entry.is_symlink()}
    except OSError as error:
        fail(label, f"cannot inspect OCI blobs: {error}")
    if actual_blobs != reachable:
        fail(label, "OCI layout contains missing or unreachable blobs")
    layout_hasher = hashlib.sha256()
    layout_hasher.update(b"oberth-single-image-oci-layout-v1\0")
    layout_files = []
    for root, _, files in os.walk(layout, followlinks=False):
        for name in files:
            path = os.path.join(root, name)
            relative = os.path.relpath(path, layout).replace(os.sep, "/")
            layout_files.append((relative, path))
    for relative, path in sorted(layout_files):
        encoded = relative.encode("utf-8")
        layout_hasher.update(len(encoded).to_bytes(8, "big"))
        layout_hasher.update(encoded)
        layout_hasher.update(os.path.getsize(path).to_bytes(8, "big"))
        with open(path, "rb") as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                layout_hasher.update(chunk)
    return {
        "manifest": manifest_digest,
        "configDigest": config_digest,
        "layerCount": len(layers),
        "layoutSHA256": layout_hasher.hexdigest(),
    }


def main(arguments):
    if len(arguments) < 1:
        raise OCIError("usage: runner-oci.py extract|validate ...")
    if arguments[0] == "extract" and len(arguments) == 4:
        extract(arguments[1], arguments[2], arguments[3])
        return
    if arguments[0] == "validate" and len(arguments) == 4:
        json.dump(validate(arguments[1], arguments[2], arguments[3]), sys.stdout, sort_keys=True)
        sys.stdout.write("\n")
        return
    raise OCIError("usage: runner-oci.py extract ARCHIVE DEST LABEL | validate LAYOUT EXPECTED_MANIFEST LABEL")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except OCIError as error:
        print(f"runner OCI: {error}", file=sys.stderr)
        sys.exit(1)
