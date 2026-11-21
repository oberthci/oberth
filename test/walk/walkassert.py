"""Assertions over public run, refusal, publication and UI evidence."""
import hashlib
from html.parser import HTMLParser
import json
import re
from pathlib import Path

HERE = Path(__file__).resolve().parent
TEST_NAME = "Test User"
TEST_EMAIL = "test@example.invalid"
TEST_IDENTITY = f"{TEST_NAME} <{TEST_EMAIL}>"
SECRET = "oberth/data/release/walk"


class Failure(Exception):
    def __init__(self, message, facts=None):
        super().__init__(message)
        self.facts = facts or {}


class Blocked(Failure):
    pass


def require(condition, message):
    if not condition:
        raise Failure(message)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def workflow_sha(workflow):
    parameters = workflow.get("spec", {}).get("arguments", {}).get("parameters", [])
    return next((p.get("value") for p in parameters if p.get("name") == "oberth-sha"), None)


def outside_git(path):
    return not any((parent / ".git").exists() for parent in (path, *path.parents))


def checked_run(detail, expected):
    run = detail["run"]
    for field, value in expected.items():
        message = f"Run {field} differs: expected {value}, got {run.get(field)}"
        if field == "status" and run.get("error"):
            message += "; " + run["error"]
        require(run.get(field) == value, message)
    return run


def build_log(output, sha):
    for marker in (f"walk: source {sha} clean", f"walk: stamped {sha} clean", "walk: build passed"):
        require(marker in output, "Build missing proof: " + marker)


def pushed_run(text, base):
    links = re.findall(re.escape(base) + r"/runs/([0-9a-f]{8,32})\b", text)
    require(len(set(links)) == 1, "Push must print one run URL to watch; no history polling substitutes for it")
    return links[0]


def refusal(text, expected):
    """Require one canonical public error object, allowing only SSH line framing."""
    remote = [line.removeprefix("remote: ").strip() for line in text.splitlines() if line.startswith("remote: ")]
    body = "\n".join(remote) if remote else text
    try:
        error, _ = json.JSONDecoder().raw_decode(body[body.index("{"):])
    except (ValueError, TypeError) as cause:
        raise Failure("Push refusal must expose a canonical JSON error") from cause
    require(isinstance(error, dict) and error.get("code") == expected["code"], "Push refusal has the wrong error code")
    scope = error.get("reason", "") if expected["code"] == "forbidden" else "\n".join(str(v) for v in error.values())
    require(all(re.search(r"(?<![\w./-])" + re.escape(t) + r"(?![\w./-])", scope) for t in expected["contains"]),
            "Refusal omits its exact scope")
    remedy = error.get("remedy", "")
    require(expected["remedy"] in remedy, "Refusal omits the required correction command")
    if expected["code"] == "forbidden":
        require(
            all(t in remedy for t in expected["contains"]),
            "Grant request must include the same exact repository/file/step/path",
        )
    return error


def release_record(events, expected):
    for event in events:
        if event.get("kind") == "release.published" and all(event.get(k) == v for k, v in expected.items()):
            require(event.get("data", {}).get("tag") == expected["ref"], "Release event omits its exact tag")
            return event
    raise Failure("Durable release.published event is missing for the exact run/tag/commit")


class Assets(HTMLParser):
    def __init__(self):
        super().__init__()
        self.urls = []

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "script" and attrs.get("src"):
            self.urls.append(attrs["src"])


def ui_data(detail, expected, step="build"):
    checked_run(detail, expected)
    labels = [row.get("display_name") or row.get("step") or row.get("node_id") for row in detail.get("steps", [])]
    require(step in labels, "Leaf entrypoint label is not authored: " + step + "; labels: " + ", ".join(labels))
