"""Write acceptance artifacts and scan their final published bytes."""
import json
import re
from walkassert import require


def write_json(path, value):
    temporary = path.with_suffix(path.suffix + ".new")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def redact(message, secrets, paths):
    if isinstance(message, dict):
        return {key: redact(value, secrets, paths) for key, value in message.items()}
    if isinstance(message, list):
        return [redact(value, secrets, paths) for value in message]
    if isinstance(message, str):
        return redact_text(message, secrets, paths)
    return message


def redact_text(message, secrets, paths):
    message = re.sub(r"-----BEGIN [^-]+-----.*?-----END [^-]+-----", "<private material>", message, flags=re.S)
    for value in sorted((str(v) for v in secrets if v), key=len, reverse=True):
        message = message.replace(value, "<private>")
    for path in sorted((str(p) for p in paths), key=len, reverse=True):
        message = message.replace(path, "<private path>")
    if re.search(r"(?i)(?:token|password|passphrase|private[_ -]?key)\s*[:=]", message):
        return "Credential-related diagnostic withheld; reproduce the recorded command with private output."
    return message[:4000]


def unsafe_evidence(out, secrets):
    patterns = (
        b"-----BEGIN PRIVATE KEY-----",
        b"-----BEGIN OPENSSH PRIVATE KEY-----",
        b"-----BEGIN ENCRYPTED COSIGN PRIVATE KEY-----",
    )
    unsafe = []
    variants = secret_variants(secrets)
    for path in out.rglob("*"):
        if path.is_file():
            body = path.read_bytes()
            if any(p in body for p in patterns) or any(v.encode() in body for v in variants if len(v) >= 8):
                unsafe.append(path)
    return unsafe


def secret_variants(secrets):
    return [encoded for value in secrets for part in (value, *(line for line in value.splitlines() if len(line) >= 16))
            for encoded in (part, json.dumps(part)[1:-1])]


def scan(out, secrets):
    unsafe = unsafe_evidence(out, secrets)
    require(not unsafe, "Public evidence scan failed; files withheld: " + ", ".join(p.name for p in unsafe))
