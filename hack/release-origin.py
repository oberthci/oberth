"""Generate or check the standalone fetcher's snapshots of the public release address, its redirect rule and the version rule."""
# Spec: docs/plans/c13-own-pipelines.md#owned-data-and-public-contract
import argparse
from pathlib import Path
import re

root = Path(__file__).resolve().parent.parent
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--check", action="store_true")
args = parser.parse_args()
try:
    constant = (root / "internal/releaseverify/origin.go").read_text()
except OSError:
    raise SystemExit("Public release address is missing; check the canonical origin constant.") from None
match = re.search(r'^const Origin = "([^"\n]+)"$', constant, re.M)
hosts = re.search(r'^const AssetHosts = "([a-z0-9. -]+)"$', constant, re.M)
redirects = re.search(r'^const MaxRedirects = ([0-9]+)$', constant, re.M)
if match is None or hosts is None or redirects is None:
    raise SystemExit("Public release address is missing; check the canonical origin constant.")
script = root / "agent/skills/oberth/fetch-oberth"
text = script.read_text()
updated = text
for name, value in (("origin", match[1]), ("asset_hosts", "'" + " ".join("https://" + host + "/" for host in hosts[1].split()) + "'"),
                    ("max_redirects", redirects[1])):
    updated, count = re.subn(r"^[ \t]*" + name + r"=.*$", lambda _: name + "=" + value, updated, flags=re.M)
    if count != 1:
        raise SystemExit("Fetcher address is missing or duplicated; restore one " + name + " assignment.")
try:
    pattern = (root / "internal/workflowpolicy/release-version.pattern").read_text().strip()
except OSError:
    raise SystemExit("Release version rule is missing; use the complete admitted source.") from None
updated, count = re.subn(r"^version_pattern=.*$", lambda _: "version_pattern='" + pattern + "'", updated, flags=re.M)
if count != 1:
    raise SystemExit("Fetcher version rule is invalid; restore one version_pattern assignment.")
if args.check:
    if text != updated:
        raise SystemExit("Fetch origin or version rule differs; run python3 hack/release-origin.py.")
else:
    script.write_text(updated)
