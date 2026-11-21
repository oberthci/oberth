"""Capture commands privately and use saved product connection settings."""
from dataclasses import dataclass, field
import json
import os
from pathlib import Path
import shlex
import signal
import ssl
import subprocess
import urllib.error
import urllib.parse
import urllib.request
from walkassert import Failure, HERE, require
from walkevidence import redact
from walkcommands import COMMANDS, render, STORE_COMMANDS
from walkstream import follow
from walkreference import kind_for


def kubectl_read(args):
    words = iter(str(a) for a in args)
    for word in words:
        if word in ("--kubeconfig", "--context", "-n", "--namespace"):
            next(words, None)
        elif word.startswith("-"):
            continue
        else:
            return word in ("get", "wait") or (word == "rollout" and next(words, None) == "status")
    return False


@dataclass
class Journey:
    name: str
    marker: str
    tag: str
    sha: str = ""
    run_id: str = ""
    release_sha: str = ""
    release_run: str = ""
    tag_object: str = ""
    cli_sha256: str = ""
    workflow: str = ""
    query_control: bool = False
    tools_image: str = ""
    tools_repository: str = ""
    image_repository: str = ""
    key_id: int | None = None
    repository_created: bool = False
    installed: bool = False
    recovery: Path | None = None
    documents: dict = field(default_factory=dict)


class Redirects(urllib.request.HTTPRedirectHandler):
    max_redirections = 3

    def __init__(self, origin):
        super().__init__()
        self.origin = urllib.parse.urlsplit(origin)

    def redirect_request(self, request, fp, code, message, headers, newurl):
        require(request.get_method() == "GET", "Protocol POST redirects are refused")
        target = urllib.parse.urlsplit(newurl)
        require((target.scheme, target.netloc) == (self.origin.scheme, self.origin.netloc),
                "Redirect leaves the configured origin")
        return super().redirect_request(request, fp, code, message, headers, newurl)


class Session:
    def __init__(self, config, args, ux, runtime):
        self.config, self.args, self.ux, self.runtime = config, args, ux, runtime
        self.private = config.path("private") / (config.slot + "-" + runtime.ids())
        self.private.mkdir(parents=True, mode=0o700)
        self.home = self.private / "home"
        self.admin_repo, self.agent_repo = self.private / "admin-repo", self.private / "agent-repo"
        self.home.mkdir(mode=0o700)
        self.binary = self.private / "oberth"
        self.secrets, self.commands, self.results = [], [], []
        self.sequence, self.current, self.lock = 0, "preflight", None
        self.pending_baseline = None
        self.expectations = json.loads((HERE / "expectations.json").read_text())
        self.data = Journey("walk-" + runtime.ids()[:12], runtime.ids(), "v0.0.0-walk-" + runtime.ids()[:12])
        self.env = {"PATH": os.environ.get("PATH", os.defpath), "LC_ALL": "C.UTF-8",
                    "HOME": str(self.home), "KUBECONFIG": str(config.kube),
                    "DOCKER_CONTEXT": config["docker_context"], "DOCKER_CONFIG": str(config.path("docker_config")),
                    "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1"}

    @property
    def kind(self):
        return kind_for(self.args.version, self.data.cli_sha256, self.ux.baseline)

    @property
    def canonical(self):
        return "walk/" + self.config["upstream_owner"] + "/" + self.data.name

    def clean(self, text):
        keys = (
            "state",
            "private",
            "upstream_token",
            "known_hosts",
            "signing_key",
            "signing_public",
            "signing_password",
        )
        paths = [self.private, self.config.root, HERE, *(self.config.path(key) for key in keys)]
        return redact(text, self.secrets, paths)

    def check_operation(self, argv, operation):
        if operation is not None:
            require(operation in COMMANDS, "Unknown declared operation")
            return
        program = Path(str(argv[0])).name
        product = program == self.binary.name and (len(argv) < 2 or argv[1] != "version")
        setup = program in ("ssh-keygen", "openssl", "tee")
        git_write = program == "git" and any(c.argv[1] in argv for name, c in COMMANDS.items()
                                             if name.startswith("git-"))
        if program == "kubectl":
            require(kubectl_read(argv[1:]), "Kubectl mutations must execute the declared command table")
        require(
            not (product or setup or git_write), "User commands must execute the declared command table"
        )

    def run(self, argv, *, timeout=120, input=None, env=None, cwd=None, accept=(0,), display=None, operation=None):
        self.check_operation(argv, operation)
        started = self.runtime.clock()
        self.sequence += 1
        stem = self.private / f"command-{self.sequence:04}"
        code, exception = 127, None
        with stem.with_suffix(".stdout").open("wb") as stdout, stem.with_suffix(".stderr").open("wb") as stderr:
            try:
                process = subprocess.Popen(
                    [str(a) for a in argv],
                    stdout=stdout,
                    stderr=stderr,
                    stdin=subprocess.PIPE,
                    env={**self.env, **(env or {})},
                    cwd=cwd or self.private,
                    start_new_session=True,
                )
                try:
                    process.communicate(input, timeout=timeout)
                except BaseException:
                    os.killpg(process.pid, signal.SIGTERM)
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        os.killpg(process.pid, signal.SIGKILL)
                        process.wait()
                    code = process.returncode
                    raise
                code = process.returncode
            except (OSError, subprocess.TimeoutExpired) as error:
                exception = error
                stderr.write((type(error).__name__ + ": " + str(error) + "\n").encode())
        command = {"step": self.current, "argv": [str(a) for a in (display or argv)], "exit_code": code,
                   "capture": stem.name, "operation": operation,
                   "duration_seconds": round(self.runtime.clock() - started, 3)}
        raw, errors = self.record(command)
        if exception is not None or code not in accept:
            reason = self.clean(shlex.join(command["argv"])) + f" (exit {code}): " + self.clean(command["message"])
            raise Failure(reason)
        return code, raw, errors

    def record(self, command):
        stem = self.private / command["capture"]
        raw, errors = stem.with_suffix(".stdout").read_bytes(), stem.with_suffix(".stderr").read_bytes()
        command["message"] = product_message(raw, errors)
        try:
            value = json.loads(raw)
        except ValueError:
            value = {}
        command["product_failure"] = failed_value(value)
        command["role"] = COMMANDS[command["operation"]].role if command["operation"] else "verification"
        self.commands.append(command)
        if command["operation"]:
            self.ux.command({k: command[k] for k in ("step", "argv", "exit_code", "operation", "duration_seconds")})
            if command["operation"] == "store":
                for inner in STORE_COMMANDS:
                    self.ux.command({"step": self.current, "argv": list(COMMANDS[inner].argv),
                                     "exit_code": command["exit_code"],
                                     "operation": inner, "duration_seconds": 0})
        return raw, errors

    def follow(self, identifier, running):
        require(self.ux.phase in ("first_green", "first_release", "checks"), "Infrastructure cannot run user commands")
        return follow(self, identifier, running)

    def profile(self, agent):
        home = self.private / "agent-home" if agent else self.home
        return {"HOME": str(home), "XDG_CONFIG_HOME": str(home / ".config"),
                "XDG_CACHE_HOME": str(home / ".cache"), "XDG_DATA_HOME": str(home / ".local/share")}

    def user(self, name, values=None, *, agent=False, upstream=False, **kwargs):
        require(COMMANDS[name].role != "product" or self.ux.phase,
                "Infrastructure cannot run user commands")
        argv = render(self, name, values or {})
        shown = argv[:]
        if argv[0] == "oberth":
            argv[0] = self.binary
        env = {**self.profile(agent), **kwargs.pop("env", {})}
        if upstream:
            env["GIT_SSH_COMMAND"] = "ssh -F " + shlex.quote(str(self.private / "upstream.conf"))
        cwd = self.agent_repo if agent else self.admin_repo
        if not name.startswith("git-") or name == "git-clone":
            cwd = self.private
        return self.run(argv, display=shown, operation=name, env=env, cwd=cwd, **kwargs)

    def uj(self, name, values=None, **kwargs):
        return json.loads(self.user(name, values, **kwargs)[1])

    def k(self, *args, **kwargs):
        require(kubectl_read(args), "Kubectl probes must be read-only")
        return self.run(
            ["kubectl", "--kubeconfig", self.config.kube, "--context", self.config.context, *args], **kwargs
        )

    def d(self, *args, **kwargs):
        require(not self.ux.phase, "Container infrastructure belongs outside the product journey")
        return self.run(["docker", "--context", self.config["docker_context"], *args], **kwargs)

    def g(self, *args, upstream=False, agent=False, **kwargs):
        env = self.profile(agent)
        if upstream:
            env["GIT_SSH_COMMAND"] = "ssh -F " + shlex.quote(str(self.private / "upstream.conf"))
        return self.run(["git", "-C", self.agent_repo if agent else self.admin_repo, *args],
                        env=env, display=["git", *args], **kwargs)

    def http(self, path, body=None):
        require(body is None, "HTTP probes must be read-only")
        context = ssl.create_default_context(cafile=str(self.home / ".config/oberth/installer-ca.crt"))
        context.load_cert_chain(str(self.private / "admin.csr.crt"), str(self.private / "admin.key"))
        headers = {"Content-Type": "application/json", "MCP-Protocol-Version": "2025-06-18"}
        try:
            return request(self.config.base + path, body, headers, context=context,
                           timeout=self.args.run_timeout + 30 if body is not None else 30)
        except Failure as error:
            self.sequence += 1
            capture = self.private / f"http-{self.sequence:04}.body"
            capture.write_text(str(error))
            self.commands.append({"step": self.current, "argv": ["POST" if body else "GET", self.config.base + path],
                                  "exit_code": 1, "message": str(error), "capture": capture.name})
            raise

    def mcp(self, name, arguments):
        raw = self.user(name, {"arguments": json.dumps(arguments)},
                        timeout=self.args.run_timeout + 30)[1]
        if name == "run_logs":
            return {"output": raw.decode(errors="replace")}
        try:
            value = json.loads(raw)
        except ValueError as error:
            self.commands[-1]["product_failure"] = True
            raise Failure("MCP invalid reply: " + self.clean(raw.decode(errors="replace"))) from error
        require(
            isinstance(value, dict) and not value.get("isError"),
            "MCP refused: " + self.clean(product_message(raw, b"")),
        )
        return value

    def upstream(self, path, body=None, method=None):
        token = self.config.path("upstream_token").read_text().strip()
        self.secrets.append(token)
        headers = {"Authorization": "token " + token, "Content-Type": "application/json"}
        raw = request(self.config["upstream_api"] + path, body, headers, method=method)
        return json.loads(raw) if raw else None

    def public_commands(self):
        return [{**{k: v for k, v in c.items() if k != "argv"}, "command": self.clean(shlex.join(c["argv"])),
                 "message": self.clean(c["message"])} for c in self.commands]


def failed_value(value):
    if not isinstance(value, dict):
        return False
    return bool(value.get("error") or value.get("run", {}).get("error") or value.get("isError"))


def product_message(stdout, stderr):
    if stderr.strip():
        return stderr.decode(errors="replace").strip().splitlines()[0]
    try:
        value = json.loads(stdout)
    except ValueError:
        return stdout.decode(errors="replace").strip()[:4000]
    value = structured_output(value)
    if isinstance(value, dict) and isinstance(value.get("run"), dict) and value["run"].get("error"):
        return message_value(value["run"]) + ("; " + value["retry_hint"]["action"] if value.get("retry_hint") else "")
    return message_value(value)


def structured_output(value):
    if isinstance(value, dict) and isinstance(value.get("result"), dict):
        value = value["result"]
    if isinstance(value, dict) and value.get("isError") and value.get("structuredContent"):
        value = value["structuredContent"]
    return value


def message_value(value):
    if not isinstance(value, dict):
        return "Product returned a structured response"
    error = value.get("error") or value.get("message") or value.get("reason")
    if error:
        return error_message(value, error)
    if value.get("content"):
        return "\n".join(c.get("text", "") for c in value["content"])
    failures = [c for c in value.get("checks", []) if not c.get("ok")]
    return json.dumps(failures) if failures else "Product returned a structured response"


def error_message(value, error):
    if isinstance(error, dict):
        return json.dumps(error)
    code = str(value["code"]) + ": " if "code" in value else ""
    remedy = " - " + value["remedy"] if value.get("remedy") else ""
    return code + str(error) + remedy


def request(url, body=None, headers=None, *, context=None, method=None, timeout=60):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), Redirects(url),
                                        urllib.request.HTTPSHandler(context=context))
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, headers=headers or {}, method=method)
    try:
        with opener.open(req, timeout=timeout) as response:
            raw = response.read(4 * 1024 * 1024 + 1)
            require(len(raw) <= 4 * 1024 * 1024, "Response exceeds 4 MiB")
            return raw
    except urllib.error.HTTPError as error:
        with error:
            raw = error.read(4 * 1024 * 1024)
        raise Failure(f"HTTP {error.code}: " + raw.decode(errors="replace"), {"http_status": error.code}) from error
    except (urllib.error.URLError, OSError) as error:
        raise Failure("HTTP transport failed: " + str(error)) from error
