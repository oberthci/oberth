#!/usr/bin/env python3
"""Verify contributor identity and attribution across reachable commit history."""
import re
import json
import subprocess
import sys

ATTRIBUTION = re.compile(r"^\s*(?:co[- ]?authored[- ]by\s*:|generated\s+(?:with|by)\b|"
                         r"ai[- ](?:assisted|generated|authored)\b)|\U0001f916", re.I)

CREDIT_TEXT = ("author", "generated", "written", "created", "contributor", "assisted", "owner", "maintainer", "built", "made", "powered", "\U0001f916")
CREDIT_KEYS = r"authors?|contributors?|owner|maintainers"

def source_attribution(text, hosts):
    line = re.sub(r"^\s*(?://|#|<!--)\s*", "", text)
    if ATTRIBUTION.search(line):
        return True
    label = re.match(r"^\s*(?:"+CREDIT_KEYS+r")\s*:\s*(.+)$",line,re.I)
    if label and hosts.search(label[1]): return True
    credit = re.search(r"(?:generated|written|created|authored|built|made|powered)\s+(?:with|by)\b", line, re.I)
    author = re.search(r"[\"'](?:"+CREDIT_KEYS+r")[\"']\s*:\s*(?:\{\s*[\"']name[\"']\s*:\s*)?[\"']([^\"']*)[\"']", line, re.I)
    return bool((credit and hosts.search(line)) or (author and hosts.search(author[1])))


def structured_credit(raw, hosts):
    try: data = json.loads(raw)
    except (ValueError, UnicodeError): return False
    def credited(value):
        if isinstance(value, dict):
            return any((re.fullmatch(CREDIT_KEYS,key,re.I) and hosts.search(json.dumps(item))) or credited(item) for key,item in value.items())
        return isinstance(value,list) and any(credited(item) for item in value)
    return bool(credited(data))

IDENTITY = re.compile(rb"(?m)^(author|committer|tagger) (.*) <([^>\n]*)> (-?[0-9]+ [+-][0-9]{4})$")


def objects(identifiers):
    data = subprocess.check_output(["git", "cat-file", "--batch"], input=b"\n".join(identifiers)+b"\n", stderr=subprocess.PIPE)
    offset = 0
    for expected in identifiers:
        end = data.index(b"\n", offset)
        sha, kind, size = data[offset:end].split()
        if sha != expected:
            raise ValueError("Unable to read repository metadata.")
        start = end+1
        offset = start+int(size)+1
        yield sha.decode(), kind, data[start:offset-1]


def metadata(head):
    commits = subprocess.check_output(["git", "rev-list", head, "--tags"], stderr=subprocess.PIPE).splitlines()
    tags = subprocess.check_output(["git", "for-each-ref", "--format=%(objectname)", "refs/tags"], stderr=subprocess.PIPE).splitlines()
    pending, seen = list(dict.fromkeys(commits+tags)), set()
    while pending:
        following = []
        for sha, kind, raw in objects(pending):
            seen.add(sha.encode())
            if kind in (b"commit", b"tag"):
                yield sha, kind, raw
            if kind == b"tag" and raw.splitlines()[1] == b"type tag":
                following.append(raw.splitlines()[0].split()[1])
        pending = list(dict.fromkeys(sha for sha in following if sha not in seen))


def public_metadata(raw, address):
    headers, separator, message = raw.partition(b"\n\n")
    def redact(match):
        return (match[1]+b" "+match[2]+b" <> "+match[4]) if match[3] == address else match[0]
    return IDENTITY.sub(redact, headers)+separator+message


def verify(head):
    name = subprocess.check_output(["git", "config", "user.name"]).strip()
    address = subprocess.check_output(["git", "config", "user.email"]).strip()
    count, errors = 0, []
    for sha, kind, raw in metadata(head):
        headers, _, body = raw.partition(b"\n\n")
        if kind == b"commit":
            identities = [(m[1], m[2], m[3]) for m in IDENTITY.finditer(headers) if m[1] != b"tagger"]
            if identities != [(b"author", name, address), (b"committer", name, address)]:
                errors.append(f"{sha}: author and committer must match the repository identity")
            if not body.strip():
                errors.append(f"{sha}: empty commit message")
            count += 1
        for line, text in enumerate(body.decode("utf-8", errors="replace").splitlines(), 1):
            if ATTRIBUTION.search(text):
                errors.append(f"{kind.decode()}/{sha}/message:{line}: [AUTHOR-ATTRIBUTION] "
                              "attribution line; remove the attribution before merging")
    if errors:
        raise ValueError("\n".join(errors))
    print(f"history: {count} commits verified")


if __name__ == "__main__":
    try:
        verify(sys.argv[1])
    except (ValueError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))
