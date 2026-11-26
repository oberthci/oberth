"""Stage the current run's unsigned bytes through the local-chart install path."""
import json
import shutil
import tarfile
import urllib.parse
from walkassert import require, digest


def target(s, *, fixture=True, publish=True):
    directory = s.args.candidate.resolve()
    listed = dict(line.split("  ", 1)[::-1] for line in (directory / "SHA256SUMS").read_text().splitlines())
    require(set(p.name for p in directory.iterdir()) == set(listed) | {"SHA256SUMS", "oberth.oci", "oberth-source-plugin.oci"},
            "Candidate inventory differs from its checksums")
    for name, expected in listed.items():
        require(digest(directory / name) == expected, "Candidate file differs from its checksum")
    record = json.loads((directory / "release.json").read_text())
    s.args.version = record["version"]
    host = urllib.parse.urlsplit(s.config["registry_http"]).netloc
    for name in ("oberth", "oberth-source-plugin") if publish else ():
        s.run(["skopeo", "copy", "--all", "--preserve-digests", "--dest-tls-verify=false",
               "oci:" + str(directory / (name + ".oci")) + ":" + record["version"],
               "docker://" + host + "/" + name + ":" + record["commit"]])
    shutil.copyfile(directory / "oberth-linux-amd64", s.binary)
    s.binary.chmod(0o700)
    s.data.cli_sha256 = digest(s.binary)
    require(s.args.version in s.run([s.binary, "version"])[1].decode(), "Candidate CLI version differs")
    chart = s.private / "candidate-chart"
    chart.mkdir()
    with tarfile.open(directory / "oberth.tgz") as archive:
        archive.extractall(chart, filter="data")
    require((chart / "oberth/Chart.yaml").is_file(), "Candidate chart is missing")
    from walksetup import tools_image
    if fixture:
        tools_image(s)
    return {"candidate_commit": record["commit"], "cli_sha256": s.data.cli_sha256,
            "manifest_sha256": digest(directory / "SHA256SUMS"),
            "unsigned_local_chart": True, "images": record["images"]}
