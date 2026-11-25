# Oberth headless acceptance walk

When launched by `.oberth/walk-vm.yaml`, the hosting installation needs the explicit
`--egress-allow` operator exception in
[walk setup](../../.oberth/resources/walk-vm.yaml): the host cluster Pod
CIDR, TCP 22. Its install/upgrade Review must name that destination. This opens
SSH to every Pod in the chosen range; include existing exceptions in the same
list. The VMI boots a run-verified pinned disk in `oberth-walk`. Installing
Oberth inside the guest cannot change the host's egress policy.

Run `./test/walk/walk` to verify a published signed release from an empty
local VM cluster through source-consuming CI, observation, promotion and a
signed, recorded release. It uses public CLI, Git, HTTP and MCP surfaces.
Exit 0 means every assertion and UX gate passed; 1 means failure; 2 means an
external prerequisite or unmeasured baseline makes completion
blocked or inconclusive. Failed assertions remain visible on a reference.
There is no runtime workaround or local packaging target.

The environment supplies a running capped VM, its explicit Docker context,
a local upstream and registry, trusted upstream host keys, and signing keys.
The walk manages only its chosen `walk-1` or `walk-2` cluster. It never starts,
stops or resizes a VM. Capacity checks and organizational scheduling belong to
the invoker. The runner has no train-workspace paths, coordination parser or
external bootstrap-script dependency.

Copy `env.example` to a private directory outside the checkout and fill in
its values. Paths resolve relative to that configuration file; absolute paths
are also accepted as operator input. Set restrictive permissions on the file
and its state/key directories. The example's placeholder upstream hostname
must be replaced with the endpoint reachable by cluster nodes. `upstream_host`
and `upstream_port` name the host's local SSH forward; `known_hosts` must pin
the advertised SSH host alias independently. `upstream_token` is an admin API
token in a private file; the walk reads it into an HTTP header. The HTTP API
and registry URLs must use loopback forwards. Registry manifest deletion must
be enabled so the walk can remove its named images, including signature tags.
Preflight probes deletion of a nonexistent digest in its own new repository
name, so a disabled deletion API blocks before the existing slot is touched.
The upstream account must permit private repository and SSH-key creation and
deletion. Supply an encrypted signing key, public key and password file.

Install these command-line prerequisites on the machine running the walk:
`curl`, `cosign`, `docker`, `k3d`, `kubectl`, `git`, `ssh`, `ssh-keygen`,
`openssl`, and Python 3.10 or newer. Their versions are environment choices;
the selected VM context must already exist in `docker_config`. No Python
packages need installing. Never select a real cluster. The environment
configuration, kubeconfig, state and private files stay outside the checkout.

```sh
./test/walk/walk --plan
./test/walk/walk --env /absolute/private/env.json --slot walk-1 \
  --mode fresh --version v0.23.4 --out /absolute/evidence/fresh
./test/walk/walk --env /absolute/private/env.json --slot walk-1 \
  --mode warm --version v0.23.4 --out /absolute/evidence/warm
```

Each evidence path must be new and have an existing parent. Slot 1 uses API
6551, SSH 34022 and HTTPS 34443; slot 2 uses 6552, 35022 and 35443. Every
cluster command has an explicit kubeconfig/context; every Docker command names
the configured VM context. Fresh mode recreates only that slot and requires
stock namespaces. Warm mode verifies the cluster UID and CLI hash, preserves
the platform/store and removes Oberth's namespace and old executions. An
exclusive per-slot lock protects both modes.

The CLI is downloaded into the invocation's private directory. The checked-in
`release.pub` verifies the release checksum manifest with its mandatory bundle;
the CLI checksum must match before the executable is used. The product's own
installer verifies and selects its chart and paired images. The only image
built by the walk is a source-free fixture tools image, using the VM builder.
It retains the base image's default root user and has no fixture security or
resource overrides. The pushed source reaches every consumer through Git and
the real source plugin; no pre-copied source tree or host path is supplied.

Certificate installation leaves connection-profile setup to the administrator.
Save the documented connection variables once for that identity and once for
the invited client after redemption; both manual profile actions are counted.
Later CLI calls use these saved profiles without repeated URL/certificate/key/CA
flags. The invited client has its own home and Git checkout. A normal read proves the redeemed profile
works without connection flags. Each checkout binds once; there are no shared-
checkout remote-replacement flags or release-time identity switches. Certificate and SSH key
creation are required user operations and are counted too. A pending upstream
check causes only its documented follow-up; an already-ready install does
not repeat registration. There is no bearer-token or client-identity fallback.

`walkcommands.py` is the executable ledger of product and harness operations, including
key/request preparation, checkout setup, saved-profile writes, Git pushes,
Oberth commands and the commands inside the secret-store script. Step code
invokes its named rows. Each row declares `product`, `harness` or `verification`.
Only product rows contribute counts, retries and product-command time. Product
includes required keys, requests, saved profiles, binding, agent onboarding and
secret-store operations. Sample authoring, the second checkout and verification
reads are excluded and cannot be traded for extra product work. Infrastructure
cannot invoke product rows. MCP tools have their own declared rows and run through
the actual saved-profile CLI; each argument counts as a flag. Direct cluster
probes permit only read-only verbs, HTTP probes only GET, and container mutations
belong to infrastructure. Required cluster mutations execute named ledger rows.
The secret write counts its outer command and each inner `read`, `export` and
`bao` command, charging the duration once.

`live-logs` starts plain `oberth log <run>` as a bounded streaming process.
When the marker line arrives, its observation callback reads the agent output
and confirms the main container is Running once. There is no second Pod query
after the observation. A read that returns only after retention cannot satisfy
that shared observation. C8 has not defined a tool-follow primitive; the active
read uses the existing tool without inventing an argument. There is no state
polling loop or authored delay. `run wait` handles completion. Release publication
and `release-ui` are independent assertions.

| Step | Assertion and a broken product it catches |
| --- | --- |
| preflight | Tools/private prerequisites/slot lock work; catches an unavailable VM or competing invocation before reset. |
| target | Release signature and exact CLI hash verify; catches a modified downloaded binary. |
| cluster | Stock namespaces or matching warm UID/hash; catches residue and reuse of the wrong cluster. |
| fixture | Authored commit lives in the disposable upstream; catches a setup that never publishes actual source. |
| install | Default signed installation and endpoint trust pin agree; catches a wrong CA or failed endpoint check. |
| login | Saved administrator identity reads the authenticated catalog; catches missing saved client settings. |
| connect | Registered repository and pinned remote bind successfully; catches a nonexistent or misbound repository. |
| onboard | Invitation and distinct saved client profile work; catches shared-profile identity collisions. |
| push | Ordinary push returns one watch URL on the configured origin; catches silent admission or absent run links. |
| live-logs | CLI/MCP show the exact marker while the same run is running; catches retained-only or wrong-run logs. |
| build | Exact commit, clean tree and binary VCS stamp agree; catches missing/substituted/dirty source even with exit 0. |
| retained-logs | Both interfaces retain exact source and build markers; catches missing history after completion. |
| ui | Pages/assets load and data names the exact run, SHA and authored `build`/`publish` step; catches generic successful HTML or another run's data. |
| grant | Exact repository/file/step/path approval supplies signing material; catches an unmaterialized grant during the actual release. |
| release-ui | The release leaf uses its authored template as the row label; catches a generated Workflow name in the primary label. |
| failing-build | Each failed view names the authored step, a short reason and a working log command; catches a missing detail, generic next action or wrong selector. Printed commands are executed. |
| server-restart | The first server-dependent initializer in native Pod order starts inside a measured server outage; that same Pod later proves the exact tested commit. The run must finish green within its deadline, with no authored/native retries and one Ready replica restored in finally. No in-outage initializer start is BLOCKED. Evidence names the waiting container for each target. |
| admission-control | The same queries used for refusals find the accepted source-probe run and its Workflow; catches vacuous absence checks. |
| promote | Read main from the forge, fetch it, prove tested-source ancestry and CI at that exact main SHA; catches an unrelated green commit claimed by the product. |
| release | Exact tag object, source-stamped image, independent signature verification and durable `release.published` event agree; catches a passed run with no recorded publication. |
| invalid-yaml | Unknown fields in two otherwise ordinary templates cause immediate refusal naming both offenders/file and correction; catches a permissive decoder or execution after refusal. |
| ungranted-secret | `forbidden` code and complete repository/file/step/path in both reason and grant remedy; no execution; catches an unrelated YAML/volume refusal that merely names a step. |
| readonly-source | The ordinary image default root user attempts new files, Git writes, tracked writes and chmod; catches a writable root-owned tree hidden by non-root ownership checks. |
| cleanup | Exact owned key/repository/image names are deleted; catches accumulating upstream or registry resources. |
| ux-gate | Measured counts/time and explicit next-action expectations pass; catches extra user effort, 25% time growth or missing remedies. |

For an annotated release tag, the public run `sha` is the tag object and
`tested_sha` is its peeled source commit. The walk checks both identities, the
forge tag object and the binary VCS stamp independently.

Refusal code checks pin the public `code/reason/remedy` JSON object, allowing
only the Git transport’s `remote:` framing. Free-form terminal text cannot
establish a refusal code. If an old release emits no code or omits the
scope, it fails visibly. An accepted negative push remains a failure; a
bounded product wait attaches its later refusal and native-container count
as evidence. A later refusal never satisfies immediate admission refusal.

`expectations.json` is the single declaration of required next actions. Only
product-command failures and the declared push/refusal outputs are checked;
harness assertions and setup-tool captures cannot manufacture a missing remedy
or satisfy one. Failed structured run results count as product failures even
when the CLI exits zero. The push always
needs its actual watch URL. Generic phrases such as "run failed", "see above"
or "use of closed network connection" cannot satisfy these expectations.
The requirement is explicit and reviewable rather than a natural-language
classifier. `UX-FINDINGS.md` contains product requirements, owners and the walk
assertion that will close each; proposed UX work remains for the owner to decide.

`result.json`, per-step JSON and `summary.txt` contain command text, exit code
and the product's own message, with private values/paths redacted. A useful
remedy does not suppress the underlying error. Private capture IDs are
relative identifiers, never home-directory paths. Known credentials and key
headers are scanned after the final result and summary are written. A scan
failure withholds unsafe files and emits a failing manifest. Only the minimal
per-slot recovery credentials and upstream key needed for warm mode survive
under configured private state; the invocation workspace is removed after
final evidence publication. The next successful fresh install replaces that
slot's saved warm credentials. Remote cleanup failures remain explicit failures
with owned resource names in the result; they do not become passing warnings.

`golden-path.json` pins the reference release and verified CLI SHA-256. Matching
bytes identify a reference automatically; other signed binaries are candidates.
A candidate never reads `reference-gaps.json` and must pass every functional
assertion and message expectation. Reference exceptions use exact step names
and normalized observed signatures, existing requirement IDs and the exact
binary identity. IDs, hashes and the two owned slots' endpoint ports are normalized; no wildcard
signature matching is used. Requirement IDs and binary identity stay pinned.

The owner-authorized `server-restart` reference gap is bound to the measured
record in `reference-restart.json`. Its signature includes the exact failed
container/error and a recomputed first-dependent-container start inside the
outage. The first committed restart record anchors the eight-gap policy when
main has no copy; subsequent gap/signature growth is refused. Candidates must
pass the restart assertion and never consult reference exceptions.

Each `Step` declares its phase in the same table as its action and dependencies.
Milestones freeze command/flag/manual/retry/wait counts and the sum of declared
command durations. They exclude harness check time and each operation’s table-declared
`harness_flags`: parsing `--json`, default `--until finished` and bounding
`--timeout`. Other flags on the same command still count. The first-release prefix
includes first-green work. Headless prompt counting is absent. A missing seconds
value produces a named regression, and the final secret scan still runs.

The reference profile records the faster of two complete signed samples per
milestone and mode, retaining the winning sample's counts. Existing samples use
the same product-only classification; their original evidence is unchanged.
Fresh first green needs 18 user commands / 73 flags; warm needs 17 / 69. First
release needs 30 / 97 and 29 / 93 respectively.

`candidate_target` beside the reference owns the candidate command/flag limits
and their one-line owner reason. Those declared limits govern candidates, while
the remaining count fields and product seconds use the measured reference
profile. Seconds allow 25%. An owner target relaxation needs a new reason.

The start load1 guard blocks at 16. Independent load5 samples above 12 make only
a time overrun, or an otherwise passing run, inconclusive (exit 2) and prevent
calibration. Functional failures, count regressions, missing measurements and
failed secret scans remain failures regardless of load.

Run each mode twice with `--record-baseline` to collect reference samples.
No profile is usable until two accepted samples exist. Recording occurs only
after final scanning and private cleanup. Increases need `--baseline-reason`.
Policy tests compare with main, then its merge base if main has no copy. When
both predate reference calibration, the first branch commit introducing the gap
policy anchors both files; the tests print that selection and fail if no anchor
exists. Added gaps and unexplained growth are tested before main integration.

To add a step, add its action, dependencies, milestone and explicit phase to
`STEPS`, the operations and their roles to `COMMANDS`, a broken-product row here, and its
product-message expectation. Tests enforce the table, counting rules and README
coverage.

Focused offline checks:

```sh
python3 -m unittest discover -s test/walk -p 'test_*.py' -v
shellcheck test/walk/fixture/*.sh
```

The restart inspection starts with a suspended native fixture and a scoped Pod
watch before scaling the server Deployment to zero. After the old container's
observed exit, the fixture resumes sequential source steps. The first native
server-dependent initializer is selected in Pod spec order: the executor-token
waiter when present before the source-plugin initializer, otherwise the source
initializer. Its observed start triggers a 15-second outage hold through the
bounded event stream. Evidence records the selected container and its native
start time per Pod and target; archive-load log timestamps remain separate facts.
A finally path restores one replica, waits for Ready and verifies both requested
and Ready counts, including on errors. At least one initializer must start inside
the exit-to-Ready interval, and that same Pod must later prove the tested commit.
The full run must be green without retries and inside its deadline. No qualifying
start is BLOCKED; a failed run, wrong source, short hold or failed restoration is
a failure. All outage operations remain outside golden-path prefixes.

Walk-step requirements in the step table are **defined**, not candidate proof.
A requirement is proven only by a checked-in candidate result bound to its
result hash and verified CLI digest, with that particular step passing.
Reference runs and defined table entries do not establish candidate acceptance.
