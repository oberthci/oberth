"""Check source execution, user-visible observations and independent publication."""
import json
import re
import shlex
import urllib.parse
import walkrestart
from walkassert import workflow_sha
from walkevidence import write_json
from walkassert import (
    Assets,
    Failure,
    SECRET,
    build_log,
    checked_run,
    pushed_run,
    release_record,
    require,
    refusal,
    ui_data,
)


def detail(s, identifier):
    return json.loads(s.http("/api/runs/" + identifier))


def wait(s, identifier, operation="wait"):
    if operation == "wait":
        s.ux.event(s.current, "wait", "Use the product's blocking run wait")
    result = s.uj(operation, {"id": identifier}, timeout=s.args.run_timeout + 30)
    require(result["state"] != "timeout", "Product wait timed out; inspect the run with oberth log " + identifier)
    return detail(s, identifier)


def retained(s, identifier):
    return json.loads(s.http("/api/runs/" + identifier + "/logs?limit=0"))["output"]


def push(s):
    _, stdout, stderr = s.user(
        "git-push", {"remote": "oberth", "ref": "HEAD:refs/heads/walk/change"}, timeout=180, agent=True
    )
    message = (stdout + stderr).decode(errors="replace")
    link_id = pushed_run(message, s.config.base)
    observed = detail(s, link_id)
    run = checked_run(observed, {"sha": s.data.sha, "ref": "walk/change", "release": False})
    require(run["id"].startswith(link_id), "Push watch URL resolves a different run")
    s.data.run_id = run["id"]
    s.data.workflow = run["job_name"]
    return {"run_id": s.data.run_id, "commit_sha": s.data.sha}


def running_pods(s):
    pods = json.loads(s.k("-n", "oberth-argo", "get", "pods", "-l",
                         "workflows.argoproj.io/workflow=" + s.data.workflow, "-o", "json")[1])["items"]
    return [p["metadata"]["name"] for p in pods if p.get("status", {}).get("phase") == "Running"
            and any(c["name"] == "main" and "running" in c.get("state", {})
                    for c in p.get("status", {}).get("containerStatuses", []))]


def live_logs(s):
    observation = {}
    def running():
        observation["cli_marker"] = True
        observation["agent"] = agent_logs(s)
        observation["pods"] = running_pods(s)
        return bool(observation["pods"]) and "walk: live " + s.data.sha in observation["agent"]
    observed, command = s.follow(s.data.run_id, running)
    agent = observation.get("agent")
    if agent is None:
        agent = agent_logs(s)
    if not observed:
        if observation.get("cli_marker"):
            raise Failure("Agent output was not observed with a running main container", observation)
        raise Failure("CLI and agent tools do not stream live output; only the dashboard does",
                      {"cli_message": command["message"], "agent_output": agent})
    return {"both_transports_active": True, "running_pods": observation["pods"],
            "agent_follow": "Active tool output and the CLI marker share one Running observation"}


def agent_logs(s):
    try:
        return s.mcp("run_logs", {"id": s.data.run_id})["output"]
    except Failure as error:
        return str(error)


def build(s):
    observed = wait(s, s.data.run_id)
    checked_run(
        observed, {"id": s.data.run_id, "status": "passed", "release": False, "ref": "walk/change", "sha": s.data.sha}
    )
    logs = retained(s, s.data.run_id)
    build_log(logs, s.data.sha)
    return {"exact_commit": s.data.sha, "clean_source_and_vcs_stamp": True}


def retained_logs(s):
    cli = s.user("log-retained", {"id": s.data.run_id})[1].decode()
    mcp = s.mcp("run_logs", {"id": s.data.run_id, "raw": True, "limit": 0})["output"]
    build_log(cli, s.data.sha)
    build_log(mcp, s.data.sha)
    return {"run_id": s.data.run_id, "both_transports_retained": True}


def ui(s, identifier=None, sha=None, release=False):
    identifier, sha = identifier or s.data.run_id, sha or s.data.sha
    for path in ("/", "/runs", "/runs/" + identifier):
        html = s.http(path)
        require(b"<html" in html.lower(), "UI route did not return HTML: " + path)
        assets = Assets()
        assets.feed(html.decode())
        require(assets.urls, "UI route omits its script: " + path)
        for asset in set(assets.urls):
            require(asset.startswith("/") and not asset.startswith("//"), "UI script must stay on its verified origin")
            require(s.http(asset), "UI script is empty: " + asset)
    query = urllib.parse.urlencode({"repo": s.canonical, "limit": 100})
    rows = json.loads(s.http("/api/runs?" + query))["runs"]
    require(any(r["id"] == identifier and r["sha"] == sha for r in rows), "UI list omits the exact pushed run/commit")
    observed = json.loads(s.http("/api/runs/" + identifier + "?view=dashboard"))
    status = detail(s, identifier)["run"]["status"]
    facts = {"run_id": identifier, "dashboard": observed,
             "label_rule": "display_name || step || node_id; template is a subtitle"}
    try:
        ui_data(observed, {"id": identifier, "sha": sha, "release": release, "status": status,
                           "tested_sha": s.data.release_sha if release else s.data.sha},
                step="publish" if release else "build")
    except Failure as error:
        raise Failure(str(error), facts) from error
    return facts


def forge_ref(s, ref):
    output = s.g("ls-remote", "origin", ref, upstream=True)[1].decode().split()
    require(len(output) == 2 and output[1] == ref, "Forge does not retain the exact requested ref: " + ref)
    return output[0]


def promote(s):
    sources = s.private / "promotion.json"
    write_json(sources, [{"sha": s.data.sha, "ref": "walk/change"}])
    created = s.uj("promotion")
    s.ux.event(s.current, "wait", "Wait for promotion through promote_status")
    response = s.mcp("promote_status", {"id": created["id"], "timeout_s": s.args.run_timeout})
    promotion = response["promotion"]
    require(
        not response["still_running"] and promotion["status"] == "passed",
        "Promotion not passed: " + json.dumps(promotion),
    )
    main = forge_ref(s, "refs/heads/main")
    require(main == promotion["result_sha"], "Forge main differs from the product's claimed publication")
    s.user("git-fetch", upstream=True)
    ancestor = s.g("merge-base", "--is-ancestor", s.data.sha, main, accept=(0, 1, 128))[0]
    require(ancestor == 0, "Forge main does not contain the exact tested source commit: " + s.data.sha)
    checked_run(wait(s, promotion["run_id"], "wait-promotion"),
                {"id": promotion["run_id"], "sha": main, "status": "passed"})
    build_log(retained(s, promotion["run_id"]), main)
    s.data.release_sha = main
    return {"forge_main": main, "tested_source_is_ancestor": True, "promotion_id": created["id"]}


def recovery_values(s):
    path = s.private / "install.secrets"
    if not path.exists() or "OPENBAO_ROOT_TOKEN=" not in path.read_text():
        path = s.data.recovery
    values = {}
    for line in path.read_text().splitlines():
        if "=" in line:
            key, value = line.split("=", 1)
            parsed = shlex.split(value)
            if parsed:
                values[key] = parsed[0]
                s.secrets.append(parsed[0])
    return values


def grant(s):
    root = recovery_values(s)["OPENBAO_ROOT_TOKEN"]
    key, password = s.config.path("signing_key").read_text(), s.config.path("signing_password").read_text().strip()
    s.secrets.extend([key, password])
    s.user("store", input=(root + "\n" + json.dumps({"signing_key": key, "signing_password": password})).encode())
    s.mcp(
        "access_allow", {"repo": s.canonical, "workflow": ".oberth/release.yaml", "step": "publish", "secret": SECRET}
    )
    s.user("sync", env={"BAO_TOKEN": root}, timeout=300)
    (s.private / "upgrade").mkdir()
    s.user("upgrade", env={"BAO_TOKEN": root}, timeout=600)
    return {"grant": {"repository": s.canonical, "file": ".oberth/release.yaml", "step": "publish", "secret": SECRET}}


def release(s):
    s.user("git-tag")
    tag_object = s.g("rev-parse", s.data.tag)[1].decode().strip()
    _, stdout, stderr = s.user("git-push", {"remote": "oberth", "ref": "refs/tags/" + s.data.tag}, timeout=180)
    identifier = pushed_run((stdout + stderr).decode(), s.config.base)
    checked_run(wait(s, identifier), {"id": identifier, "release": True, "ref": s.data.tag,
                                    "sha": tag_object, "tested_sha": s.data.release_sha, "status": "passed"})
    logs = retained(s, identifier)
    for marker in (
        "walk: release " + s.data.tag,
        "walk: signed and verified ",
        "walk: stamped " + s.data.release_sha + " clean",
    ):
        require(marker in logs, "Release proof is missing: " + marker)
    require(
        forge_ref(s, "refs/tags/" + s.data.tag) == tag_object, "Forge tag object differs from the pushed annotated tag"
    )
    events = s.mcp("events_wait", {"after_id": 0, "repos": [s.canonical], "run_ids": [identifier],
                                   "kinds": ["release.published"], "timeout_s": s.args.run_timeout})["events"]
    record = release_record(events, {"repo": s.canonical, "run_id": identifier, "ref": s.data.tag, "sha": tag_object})
    host = urllib.parse.urlsplit(s.config["registry_http"]).netloc
    image = host + "/" + s.data.image_repository + ":" + s.data.tag
    s.run(
        [
            "cosign",
            "verify",
            "--allow-http-registry",
            "--insecure-ignore-tlog",
            "--key",
            s.config.path("signing_public"),
            image,
        ]
    )
    s.data.release_run, s.data.tag_object = identifier, tag_object
    facts = {"run_id": identifier, "tag_object": tag_object, "tested_commit": s.data.release_sha,
             "release_event_id": record["id"], "signed_image": image}
    return facts


def negative(s, kind):
    s.user("git-checkout", agent=True)
    path = s.agent_repo / ".oberth/build.yaml"
    write_json(path, s.data.documents[kind])
    s.user("git-add", {"path": ".oberth/build.yaml"}, agent=True)
    s.user("git-commit", {"message": "Exercise " + kind + " refusal"}, agent=True)
    sha = s.g("rev-parse", "HEAD", agent=True)[1].decode().strip()
    ref = "walk/" + kind
    try:
        code, stdout, stderr = s.user(
            "git-push",
            {"remote": "oberth", "ref": "HEAD:refs/heads/" + ref},
            accept=(0, 1, 128),
            timeout=180,
            agent=True,
        )
        message = (stdout + stderr).decode(errors="replace")
        if kind == "failing":
            require(code == 0, "Deliberate failure push was refused: " + message)
            return failing_build(s, pushed_run(message, s.config.base), sha)
        if kind == "restart":
            require(code == 0, "Restart probe push was refused: " + message)
            return walkrestart.run(s, pushed_run(message, s.config.base), sha)
        if kind == "readonly":
            require(code == 0, "Read-only probe push failed: " + message)
            identifier = pushed_run(message, s.config.base)
            observed = wait(s, identifier)
            output = retained(s, identifier)
            found_run, found_workflow = source_queries(s, ref, sha)
            s.data.query_control = found_run and found_workflow
            require(s.data.query_control, "Accepted source run was absent from the refusal queries")
            if observed["run"]["status"] != "passed":
                raise Failure("Default-user source probe failed: " + observed["run"].get("error", ""),
                              {"run": observed["run"], "probe_log": output})
            checked_run(observed, {"id": identifier, "status": "passed", "sha": sha})
            require("walk: source read-only" in output, "Default-user source probe did not prove denied writes/chmod")
            return {"run_id": identifier, "default_root_user": True, "writes_and_chmod": "denied"}
        return refused_push(s, kind, ref, sha, code, message)
    finally:
        s.user("git-checkout", agent=True)


def refused_push(s, kind, ref, sha, code, message):
    if code == 0:
        identifier = pushed_run(message, s.config.base)
        observed = wait(s, identifier)
        containers = json.loads(s.http("/api/runs/" + identifier + "/containers"))["containers"]
        raise Failure("Negative push was accepted: " + message,
                      {"accepted_run": observed, "native_container_count": len(containers)})
    name = "ungranted-secret" if kind == "ungranted" else "invalid-yaml"
    expected = dict(s.expectations[name]["refusal"])
    expected["contains"] = [token.replace("{repo}", s.canonical) for token in expected["contains"]]
    evidence = refusal(message, expected)
    found_run, found_workflow = source_queries(s, ref, sha)
    require(not found_run, "Refused push still created a durable run")
    require(not found_workflow, "Refused push still created a native Workflow")
    return {"refused": True, "no_run_or_workflow": True, "refusal": evidence}


def source_queries(s, ref, sha):
    query = urllib.parse.urlencode({"repo": s.canonical, "ref": ref, "limit": 100})
    rows = json.loads(s.http("/api/runs?" + query))["runs"]
    workflows = json.loads(s.k("-n", "oberth-argo", "get", "workflows", "-o", "json")[1])["items"]
    found_run = any(r["sha"] == sha and r["ref"] == ref for r in rows)
    found_workflow = any(workflow_sha(wf) == sha for wf in workflows)
    return found_run, found_workflow


def admission_control(s):
    require(s.data.query_control, "Refusal queries have not found their accepted positive control")
    return {"accepted_run_and_workflow_found": True}


def failing_build(s, identifier, sha):
    waited = s.uj("wait", {"id": identifier}, timeout=s.args.run_timeout + 30)
    viewed = s.uj("run-view", {"id": identifier})
    agent = s.mcp("run_get", {"id": identifier})
    checked_run(viewed, {"id": identifier, "sha": sha, "status": "failed"})
    require("walk: deliberate failure " + sha in retained(s, identifier), "Expected failure marker is missing")
    expected = "oberth log " + identifier
    views = {"run wait": waited, "run view": viewed, "agent view": agent}
    commands = {name: failure_log_command(value, identifier) for name, value in views.items()}
    missing = [name for name, command in commands.items() if command is None]
    if missing:
        raise Failure("Failed build views omit the exact log command: " + ", ".join(missing),
                      {"run_id": identifier, "expected_log_command": expected, "views": views})
    for name, view in views.items():
        texts = list(view_text(view))
        require(any(re.search(r"(?<![\w/-])failing(?![\w/-])", text) for text in texts),
                name + " omits the authored failed step: failing")
        require(any(short_failure_reason(text) for text in failure_reasons(view)),
                name + " omits a short failure reason")
    for command in {tuple(argv) for argv in commands.values()}:
        output = s.user("log-command", {"selector": command[2], "options": list(command[3:])})[1].decode()
        require("walk: deliberate failure " + sha in output, "Printed log command does not select the failed step")
    return {"run_id": identifier, "exact_log_command": expected, "views": views}


def view_text(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for child in value.values():
            yield from view_text(child)
    elif isinstance(value, list):
        for child in value:
            yield from view_text(child)


def failure_log_command(view, identifier):
    for text in view_text(view):
        for line in text.splitlines():
            match = re.search(r"\boberth\s+log\s+", line)
            if match is None:
                continue
            try:
                argv = shlex.split(line[match.start():].split(";", 1)[0])
            except ValueError:
                continue
            if len(argv) >= 3 and re.fullmatch(r"[0-9a-f]{8,32}", argv[2]) and identifier.startswith(argv[2]):
                return argv
    return None


def short_failure_reason(text):
    return bool(text.strip()) and len(text.strip()) <= 40


def failure_reasons(value):
    if isinstance(value, dict):
        for key, child in value.items():
            if isinstance(child, str) and (key.lower().endswith("reason") or key == "error"):
                yield child
            yield from failure_reasons(child)
    elif isinstance(value, list):
        for child in value:
            yield from failure_reasons(child)
    elif isinstance(value, str):
        yield from display_reasons(value)


def display_reasons(text):
    lines = text.splitlines()
    for index, line in enumerate(lines):
        if line.startswith("failing:"):
            yield line.split(":", 1)[1].strip()
        elif line.strip() == "failing" and index + 1 < len(lines):
            yield lines[index + 1].strip()
