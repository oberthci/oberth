"""Remove this invocation's exact upstream/registry names and private workspace."""
import json
import re
import urllib.request
import urllib.error
from walkassert import Blocked, Failure, require
from walkstate import preserve_warm
from walkio import Redirects, request


def registry_ready(s):
    url = s.config["registry_http"] + "/v2/walk-tools/" + s.data.name + "/manifests/sha256:" + "0" * 64
    try:
        request(url, method="DELETE")
    except Failure as error:
        if error.facts.get("http_status") != 404:
            raise Blocked(
                "Enable named manifest deletion in the provided registry before running: " + str(error)
            ) from error


def registry_remove(s, name):
    require(name in (s.data.tools_repository, s.data.image_repository) and name.endswith("/" + s.data.name),
            "Cleanup may remove only this invocation's image repository")
    base = s.config["registry_http"] + "/v2/" + name
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), Redirects(base))
    try:
        with opener.open(base + "/tags/list", timeout=30) as response:
            tags = json.load(response).get("tags") or []
    except urllib.error.HTTPError as error:
        error.close()
        if error.code == 404:
            return
        raise Failure(f"Cannot list owned registry tags: HTTP {error.code}") from error
    for tag in tags:
        registry_tag_remove(opener, base, tag)


def registry_tag_remove(opener, base, tag):
    headers = {"Accept": ", ".join(("application/vnd.oci.image.index.v1+json",
                "application/vnd.docker.distribution.manifest.list.v2+json",
                "application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json"))}
    try:
        query = urllib.request.Request(base + "/manifests/" + tag, headers=headers, method="HEAD")
        with opener.open(query, timeout=30) as response:
            identity = response.headers["Docker-Content-Digest"]
        require(re.fullmatch(r"sha256:[0-9a-f]{64}", identity), "Registry returned an invalid manifest identity")
        with opener.open(urllib.request.Request(base + "/manifests/" + identity, method="DELETE"), timeout=30):
            pass
        with opener.open(query, timeout=30):
            raise Failure("Owned registry tag remains readable after manifest deletion: " + tag)
    except urllib.error.HTTPError as error:
        error.close()
        if error.code != 404:
            raise Failure(f"Enable named manifest deletion in the environment registry: HTTP {error.code}") from error


def cleanup(s):
    require(re.fullmatch(r"walk-[0-9a-f]{12}", s.data.name), "Cleanup requires an invocation-owned name")
    failures = []
    actions = [("warm state", lambda: preserve_warm(s))]
    actions.append(("upstream keys", lambda: remove_upstream_keys(s)))
    if s.data.repository_created:
        actions.append(
            (
                "upstream repository",
                lambda: s.upstream("repos/" + s.config["upstream_owner"] + "/" + s.data.name, method="DELETE"),
            )
        )
    for name in (s.data.tools_repository, s.data.image_repository):
        if name:
            actions.append((name, lambda name=name: registry_remove(s, name)))
    if s.data.tools_repository:
        host = s.config["registry_http"].split("://", 1)[1]
        actions.append(
            ("local fixture image", lambda: s.d("image", "rm", host + "/" + s.data.tools_repository + ":fixture"))
        )
    for name, action in actions:
        try:
            action()
        except Exception as error:
            if not isinstance(error, Failure) or error.facts.get("http_status") != 404:
                failures.append(name + ": " + str(error))
    require(not failures, "Cleanup incomplete: " + "; ".join(failures))
    return {"owned_key_repository_images_removed": True, "minimal_warm_credentials_preserved": s.data.installed}


def remove_upstream_keys(s):
    ids = [s.data.key_id] if s.data.key_id is not None else []
    public_file = s.private / "upstream.pub"
    if not ids and public_file.exists():
        public = public_file.read_text().split()[:2]
        ids = [k["id"] for k in s.upstream("user/keys")
               if k["title"] == s.data.name and k["key"].split()[:2] == public]
    for identity in ids:
        try:
            s.upstream("user/keys/" + str(identity), method="DELETE")
        except Failure as error:
            if error.facts.get("http_status") != 404:
                raise


def remove_private(s):
    require(s.private.parent == s.config.path("private") and s.private.name.startswith(s.config.slot + "-"),
            "Private cleanup is outside this invocation")
    for path in sorted(s.private.rglob("*"), key=lambda p: len(p.parts), reverse=True):
        if path.is_symlink() or path.is_file():
            path.unlink()
        else:
            path.rmdir()
    s.private.rmdir()


def close_local(s):
    if s.lock:
        s.lock.close()
    if s.private.exists():
        remove_private(s)
