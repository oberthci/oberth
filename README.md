# Oberth

AI-native CI for Kubernetes — a self-hosted gate that runs every AI-agent push
as an Argo Workflow and lets only green reach your forge.

Agents `git push` to Oberth over SSH. Each push runs the repository's own
pipeline as an Argo Workflow — no forge queue, no API rate limits, feedback at
local speed. Green branches sync upstream automatically; red runs become
issues an agent can lock and fix. Every action is attributed to a durable
uplink identity and recorded in a tamper-evident audit chain — optionally
anchored outside the box.

Releases: [tags](https://github.com/oberthci/oberth/tags), installed from [oberth.ci](https://oberth.ci) · Helm chart at `https://charts.oberth.ci` ·
Website: [oberth.ci](https://oberth.ci)

## What it does

- **Git ingress** — authenticated Git-over-SSH (NodePort `30022`), smart
  protocol only. Oberth fetches and caches upstream repositories on demand;
  force-pushed feature branches are accepted, tags are creation-only.
- **CI** — each push submits the repository's Argo Workflow YAML
  (`.oberth/build.yaml` for branches, `.oberth/release.yaml` for tags); each
  template runs in its own container with its own exit code and named log
  slice. A priority-then-FIFO queue; how many Workflows run at once is the
  Argo controller's parallelism ([where limits live](docs/where-limits-live.md));
  a newer push to the same branch supersedes its own in-flight run.
- **Issues** — a red run files exactly one issue per repository and branch;
  repeated reds update it, green auto-closes it. Five-minute uplink-owned
  locks keep two agents off the same issue.
- **Promotion** — `promote` accepts a green branch SHA or an authorized
  [parked SHA](docs/park-promotion.md) and merges with the target locally.
  Parked sources always run merged-tree CI; green branch sources rerun CI when
  needed. The push never forces. A moved target fails the promotion; records
  are append-only.
- **Tags** — ordinary tags run `.oberth/build.yaml`. Version tags beginning
  `v<major>.<minor>.<patch>` and administrator release routes run release CI.
- **Release** — a release tag runs the release burn only if its commit is reachable
  from the default branch, with exactly the secrets the repository declares,
  intersected with the administrator allowlist. After every burn is green,
  Oberth publishes the exact admitted tag object upstream.
- **MCP** — tools over authenticated HTTPS for AI agents: SHA- and
  exact-run status/logs, long-poll wait, promotion, and the
  issue queue.
- **Audit** — every mutation joins a gap-free SHA-256 chain, always. Opt-in
  external anchoring adds RFC 3161 timestamps and public Rekor witnesses,
  reconciled against rollback-external immutable Kubernetes records.

### Release secrets from your secret store — never through etcd

A repository declares release credential paths in an annotation on its
`.oberth/release.yaml` Workflow:

```yaml
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/data/release/r2-upload-token,oberth/data/release/cosign-secret
```

Release steps use `oberth secretstore exec` to authenticate with their
ServiceAccount, fetch approved paths, and write private files to a verified
tmpfs. The wrapper strips store credentials from the child environment and
redacts fetched values from output. Branch builds receive no release credentials.

Setup runs in the operator's authenticated `bao`/`vault` CLI session.
The server daemon authenticates through its workload identity; the operator-only
`oberth secretstore sync` command reads `BAO_TOKEN` from its environment.
`oberth secretstore verify` checks the workload trust chain inside the pod.
The installer bootstraps verified OpenBao TLS directly on its retained PVC,
publishes only the public CA through a ConfigMap, and keeps its listener encrypted.

## Quick start

Install the CLI, then let Oberth set up its chart and a production OpenBao
with verified TLS on your existing Kubernetes cluster:

```bash
curl -fsSL https://oberth.ci/install.sh | sh
oberth install --install-secretstore
```

The script verifies the release checksums and signature and installs to
`/usr/local/bin`. Without root access, install into your home instead:

```bash
export OBERTH_INSTALL_DIR="$HOME/.local/bin"
curl -fsSL https://oberth.ci/install.sh | sh
```

Any cluster your kubeconfig reaches is a valid target, local or remote: the
install names the context and its API server before it changes anything.
When the kubeconfig (`--kubeconfig`, else `KUBECONFIG`, else `~/.kube/config`)
has more than one context, name the one to use with `--context`. `oberth
upgrade` follows the same rule.

### Without a terminal: scripts and AI agents

The install creates credentials that are shown once: the OpenBao root token
and unseal key, and the first uplink's bearer token. On a terminal they are
shown in a box. When the output is a log or an AI transcript, they go to a
secrets file instead, and the install refuses without one:

```bash
oberth install --context my-cluster --install-secretstore \
  --secrets-file ~/oberth-install-secrets.txt \
  --forge github --organization acme --client-host localhost
```

The secrets file must not exist yet and must lie outside any git worktree; it
is written with mode 0600, and the output names only its path and SHA-256. A
run that created nothing removes it again. Move it off the machine.

Everything the interactive install asks is a flag. `--upstream-url
ssh://git@forge.example.test:2222/acme` names any other forge; add
`--expected-host-fingerprint SHA256:...` (the forge's SSH host-key fingerprint
as its admin publishes it) so it is trusted without a prompt. The uplink
identity defaults to `user@host`, and its key to `~/.ssh/id_ed25519.pub`,
else to `~/.ssh/oberth_ed25519`, a pair the install generates; no other key
in `~/.ssh` is picked, and `--ssh-public-key` names one. This machine's
client files (`~/.config/oberth`) and, when Claude Code is installed, its CA
trust and the Oberth plugin are set up as on a terminal; `--client-access
none` skips them. Client files that already exist with other content, such as
another server's, are kept and nothing is written, unless
`--replace-client-files` is given. When clients reach the server on other
ports than the NodePorts 30022/30443 (a port forward, a container runtime
publishing them elsewhere), say so with `--client-ssh-port` and
`--client-https-port`: the client files, the push banner and every printed
command use them. `--assist` turns on
[assisted continuation](docs/assisted-continuation.md).

The Oberth plugin for Claude Code can come first: `/plugin install
oberth@oberthci` works before any server exists, and `/oberth:install` then
walks through these steps. Leave its URL/CA options empty until then. The
[plugin guide](docs/plugin-install.md) covers custom client ports and per-install
SSH bindings for repositories.

### First push

If setup has not already registered them, add your upstream and this machine's
uplink. Replace `acme`, `my-repo`, `operator@host`, and `oberth.example` with your
organization, repository, identity, and node address. Register the deploy key
printed by `upstream add` at your forge when prompted.

```bash
oberth upstream add github ssh://git@github.com/acme
kubectl exec -i -n oberth deploy/oberth -- \
  oberth uplink add - operator@host < ~/.ssh/id_ed25519.pub

# Run in your existing local repository, with the changes committed.
git remote add oberth ssh://git@oberth.example:30022/github/acme/my-repo.git
git push oberth HEAD:refs/heads/first-push
# Open https://oberth.example:30443/runs in your browser.
```

`uplink add` prints the uplink's bearer token once; store it. Clients trust
the server by its CA: `oberth client pin` prints the CA's SHA-256 pin from
`~/.config/oberth/ca.crt`. The push banner links to the queued run when a
dashboard URL is configured.

### First promotion

Green branch CI is the input to a promotion, which merges, tests and pushes a
target branch such as `main`. The first uplink is not an admin, and a
non-admin promotes a commit only with a single-use grant an admin issued for
that exact repository, SHA and target. Register an admin uplink for a person,
with a key no agent holds, and issue the grant with it:

```bash
kubectl exec -it -n oberth deploy/oberth -- \
  oberth uplink add --admin - admin@host < admin.pub
# Store the printed token in admin-token (mode 0600), then:
oberth mcp call --token-file admin-token promote_grant \
  '{"repo":"github/acme/my-repo","sha":"<green sha>","branch":"main"}'
```

The agent then promotes that SHA with the `promote` tool. A repository can
also allow promotion of parked commits without a grant; see
[park promotion](docs/park-promotion.md).

Both the server namespace (`oberth` by default) and pipeline namespace
(`oberth-argo`) require the Pod Security Admission `privileged` level, or no
PSA enforcement. The server's root cache initializer and the server/pipeline
`hostPath` caches are incompatible with `baseline` and `restricted`.
The installer checks both namespaces before writes and labels newly created
ones `pod-security.kubernetes.io/enforce=privileged`. It never weakens an
existing namespace policy. When using Helm directly, have the namespace
administrator create or label both namespaces with that value first; an
unlabelled namespace may still inherit a restrictive cluster default.

The Argo pod admission policy is enabled by default (`argo.podPolicy.enabled: true`)
and limits host access despite the privileged PSA label. It permits only
`NET_BIND_SERVICE` as an added capability, including nonroot virt-launcher pods.
An operator can set `argo.podPolicy.enabled: false` to remove both the policy and
its binding when replacing this boundary with equivalent admission controls.

`oberth setup` walks through the same install step by step. It lists the
SSH key pairs in `~/.ssh`, can connect further organizations with the same
upstream key (`--also-upstream codeberg.org/acme`), and asks which address
clients use: for a VM or remote host choose the node's address
(`--client-host 192.0.2.5`; NodePorts 30022/30443 listen on every node
address). It ends with the exact commands to clone from each organization,
connect another one, and give another machine an uplink.

The installer guides upstream and uplink registration. To manage the chart
separately after configuring your cluster and secret store, use the dedicated
Oberth repository:

```bash
helm repo add oberth https://charts.oberth.ci
helm repo update
helm show values oberth/oberth
```

The SSH host key is generated once into OpenBao and reused across upgrades.
With a production secret store the HTTPS certificate is a 30-day leaf the
server requests from a name-constrained CA in OpenBao's PKI engine and renews
in place; clients pin the CA. An init container
prepares the cache directories; the pod stays `0/1 Running` — live, not
ready — until the first upstream is registered. `upstream add` offers to mint
a deploy key and prints it once for registration at your forge. `uplink add`
prints a bearer token exactly once; wire it into your MCP client, which pins
the CA (`oberth client pin`; full walkthrough in
[docs/mcp-setup.md](docs/mcp-setup.md)):

```json
{
  "mcpServers": {
    "oberth": {
      "type": "url",
      "url": "https://<node>:30443/mcp",
      "headers": { "Authorization": "Bearer oberth_…" }
    }
  }
}
```

Claude Code connects through the Oberth plugin instead: it asks for the URL,
the token and the CA pin (`oberth client pin`), and `/oberth:connect` trusts
the CA only when it matches the pin.

```
/plugin install oberth --marketplace oberthci/oberth
```

## Upgrading

`oberth upgrade` keeps stored release configuration and merges new chart defaults
for keys that were absent, then applies `--values`/`--set` overrides. It uses
Helm's `--reset-then-reuse-values` mode. Native Git is the only source backend.

Upgrades require Helm 3.14 or newer. Before applying changes, the CLI reads
`helm get values --all` and pins the installed `compatibility.schemaIdentity`,
`compatibility.auditDomain` and `compatibility.witnessKeyInfo`, even when they
came only from the old chart defaults. Upgrade overrides cannot change those
identities. Stored user values and requested overrides are checked against the
target chart's schema before the Oberth or Argo controller upgrade begins;
unknown keys are reported and refused rather than silently removed.

Older Helm 4 installations whose Argo Role rules are owned by a legacy kubectl
manager can use the [explicit Role ownership migration](docs/argo-role-migration.md)
before an upgrade. It preserves permissions and unrelated ownership. PVC update
and list remain required for source-claim adoption and cleanup.

Chart values that no longer exist are refused, never ignored or stripped:
`gitBackend`, `gitcache`, `maxConcurrentJobs`, `argoSync`, `trustedTests`, `vm`,
`argo.vmLauncherServiceAccount`, `argo.containerDiskPrefixes`,
`argo.kubevirtCeiling` and `admission.` `exclusiveClasses`, `sizeWeights`,
`maxHeadWait`, `declaredResources`, `legacyResources` and `runtimeCapacity`.
When the stored release values, a `--values` file or a `--set` still sets one,
`oberth upgrade` names each key with its source and stops before any write.
Delete the keys from your values files and re-run the upgrade with
`--set <key>=null` for each named key, for example
`oberth upgrade --set trustedTests=null --set vm=null`. Helm's
`--reset-then-reuse-values` merge drops a stored key that an override sets to
null, so the key is gone from the stored values from then on.

When a target chart removes a configured key, export the installed values,
remove or migrate the unsupported keys in an explicit values file, and reconcile
with `helm upgrade ... --reset-values --values cleaned.yaml` before retrying.
Keep the deployment's existing compatibility identities in that file. A manual
downgrade also needs an explicit values file matching the older schema, without
the newer chart's keys; reusing newer values directly will be rejected. Database
migrations remain forward-only: an older binary may require restoring its
matching database backup before it can run.

Re-running `oberth install` and restoring a backup intentionally retain
`--reuse-values`: these paths preserve installed chart defaults, while restore
sets only the database identities it needs. Every newly introduced value must
therefore have a template fallback. The v0.17.53 raw-values render (without
the removed values) and actual Helm install/restore tests cover the
`utilityImage` fallback.

## How it works

1. **Push** — an agent pushes a branch over SSH; the push is attributed to its
   uplink identity and durably recorded before anything else happens.
2. **Run** — Oberth submits the repository's Argo Workflow YAML as an Argo
   Workflow. Each template runs in its own container with its own exit code,
   timeout, and named log slice. The server sets metadata, forces the
   ServiceAccount identity, and injects the source checkout.
3. **Record** — green publishes the exact branch SHA to the upstream forge;
   red on the default branch or a scheduled branch run creates or updates the
   branch's single CI issue, without locking it. Other branch runs and promotions
   file no CI issue; tag/release filing is not supported. Nothing lives only on
   the box.
4. **Promote** — `promote <sha> <branch>` verifies a green branch SHA or
   authorizes a parked SHA, merges locally, proves the merged tree for parked
   sources or a divergent target, and pushes without force.
5. **Release** — a tag reachable from the default branch runs the release
   Workflow. The presence of declared secret-store paths in the
   `oberth.ci/secret-paths` annotation selects the credentialed
   ServiceAccount and its projected token. Release steps wrap their command
   with `oberth secretstore exec`, which fetches the approved secrets into
   private files on verified tmpfs and redacts child output. Only
   after every burn is terminal green does Oberth sync the exact admitted
   tag object upstream.

## The pipeline is Argo Workflow YAML

Each repository carries its pipeline as standard Argo Workflow YAML in the
`.oberth/` directory: `.oberth/build.yaml` for branch-triggered CI,
`.oberth/release.yaml` for tag-triggered releases. Oberth submits the YAML as
an Argo Workflow, setting metadata, forcing the ServiceAccount identity by
trigger tier, and injecting the source checkout into every container template.
Tool installation is inline pipeline steps -- fetch, pin, unpack -- each with
its own named exit code. Version pins live in repository files next to the
code that uses them:

```yaml
# .oberth/build.yaml (branch-trigger, abbreviated)
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: ci
  activeDeadlineSeconds: 3600
  templates:
  - name: ci
    dag:
      tasks:
      - name: setup
        template: setup
      - name: lint
        template: lint
        depends: setup
      - name: test
        template: test
        depends: lint
      - name: build-amd64
        template: build-amd64
        depends: test
      - name: build-arm64
        template: build-arm64
        depends: test

  - name: lint
    steps:
    - - name: vet
        template: vet
    - - name: golangci-lint-run
        template: golangci-lint-run

  - name: vet
    container:
      image: "golang:1.26-trixie@sha256:..."
      workingDir: /work/src
      command: ["/usr/local/go/bin/go"]
      args: ["vet", "./..."]
      # ... env and volumeMounts
```

Each template is a container with its own exit code, timeout, and named log
slice. Oberth reads the YAML statically at submission -- it never executes
repository code in the server process.

Kubernetes places step Pods using their requests. Every limit on what runs at
once lives in Argo ([where limits live](docs/where-limits-live.md)): the
controller's `parallelism` bounds running Workflows, shared step limits are
native Argo `synchronization` against the ConfigMap `oberth-sync-limits` the
argo-workflows release renders, and `releases.serialize` runs releases one at
a time with a native mutex. Oberth submits every admitted run at once and keeps
only host safety that reads nothing from a workflow: the hard CPU/IO PSI valve
and the disk-space floor. The removed `oberth.ci/size`,
`oberth.ci/size-by-trigger`, `oberth.ci/concurrency-group`,
`oberth.ci/concurrency-limit` and `oberth.ci/exclusive` annotations are refused
at admission, naming their replacement. Missing requests receive the documented defaults;
explicit requests and limits remain owned by the workflow in git. See
[admission and native synchronization](docs/native-synchronization.md) and
[measured resource advice](docs/measured-requests.md).

A run that must not overlap another declares a native mutex, shared by name
across repositories and triggers:

```yaml
spec:
  synchronization:
    mutexes:
      - name: terraform-state
```

Size grants (`oberth size`, MCP `size_grant`, `size_revoke`, `size_grant_get`)
no longer change admission: Oberth reads no size from a workflow. Their one
remaining effect is automatic tag priority. A repository whose size grant was
revoked gets no automatic boost for its tags; `never_granted` and `granted`
keep it. Only the oldest waiting tag per repository gets the boost, so it caps
waiting tags, not a continuous stream. To check a repository:

```bash
oberth mcp call size_grant_get '{"repo":"example"}'
```

`revoked` disables the automatic boost; an admin re-enables it with
`size_grant` (any accepted maximum).

To skip heavy work on a branch run, guard it with the server-set
`oberth-trigger` workflow parameter (`branch`, `promotion`, `schedule` or
ordinary `tag` in build.yaml, and `tag` in release.yaml). The server replaces
the declared default with the durable event identity. That parameter never
grants release authority:

```yaml
spec:
  arguments:
    parameters:
    - name: oberth-trigger
      value: promotion   # replaced by the server with the run's trigger
  templates:
  - name: ci
    dag:
      tasks:
      - name: kernel-matrix
        template: kernel-matrix
        when: "'{{workflow.parameters.oberth-trigger}}' != 'branch'"
```

The `oberth.ci/size` and `oberth.ci/size-by-trigger` annotations are removed
and refused at admission.

Numeric resource ceilings use `oberth resource grant/get/revoke` or MCP
`resource_grant`, `resource_grant_get`, `resource_revoke`. Migration 40 copies
existing size grants; later size changes affect only automatic tag priority.
Declared workflows never fall back to sizes. See
[declared resources](docs/declared-resources.md).

## MCP tools

The authenticated `tools/list` response is the authoritative registry.
List it with `oberth mcp tools`. A non-admin token is not shown the admin-only
tools (a call to one is still refused by the server); available operations are:

| Tool | Description |
|------|-------------|
| `status` | Return concise CI status for a full SHA, short SHA, or branch, including the failed step. |
| `logs` | Return one named step log for a SHA, optionally filtered server-side. Prefer a pattern over retrieving a whole step: counts are lower bounds unless scan_complete is true; continue has_more pages with next_byte_offset as byte_offset and offset:0. |
| `run_get` | Return one run by full durable ID or unique hexadecimal prefix (at least 8 characters). Default (summary): id/repo/ref/sha, queue_position when available, superseded, status/phase/failed_burn/failed_step/error, display_status/display_reason and compact steps (burn, name, status, started_at, finished_at). display_status is one of queued, running, passed, failed, infra-retried, superseded or cancelled; display_reason names the cause. Set summary:false for the full record: step results with diagnostics, versioned retained timing (missing timing is unknown; unavailable=pruned once the run is older than the retained timing window), the repository record and Scheduling. |
| `run_resources` | Read captured per-template resource history and numeric declarations at the newest selected SHA. Git remains the source of truth. Each step reports sizing_acceptance: five comparable terminal runs must contain complete CPU/memory/storage/duration and known limits together. Suggestions use p95/max; incomplete, mixed-condition or censored history gets no suggestion. Suggestions omit an absent deadline in JSON and YAML; it is never zero. YAML includes paste-ready ordinary pod resources. Main-container duration does not measure Pod active lifetime through finalization, so no activeDeadlineSeconds is suggested. No logs, commands, environment or image details. |
| `run_retry` | Queue a fresh attempt for the newest interrupted or infrastructure-failed run, retaining repository, ref, SHA and release admission. Workflow-level failures without a failed step qualify. Tag/release retries require an admin, an unchanged tag object and complete credential-aware terminal observations for the entire tag history (or proof of no Workflow intent). Started secret-store/WIF work, old plans, missing evidence, a moved tag or an older source need run_rerun with force. Pending Workflow deletion is refused even with force. Test failures, older runs, promotion/plan/apply runs, published tags and refs with an active run are refused. The caller is recorded as actor. |
| `publication_abandon` | Abandon one pending upstream publication by its full publication ID. Requires an unscoped admin uplink and a reason. Atomically marks the publication abandoned and its run failed/abandoned (and its promotion failed), with the administrator in the audit chain. Never pushes; earlier pushes may already be visible upstream. Abandoned intents cannot be retried with publish_retry. An in-flight delivery settles before this operation; terminal publications are refused. |
| `run_cancel` | Cancel one queued or running run (a claimed run that has not started is queued). The run becomes interrupted with phase cancelled and a reason naming the caller; a claimed run's Workflow is deleted and its admission released, and it is never re-fired. Cancelling a terminal run is refused. Non-admin uplinks may cancel only branch-push runs; admins may cancel any run but must set force for tag/release and apply runs, whose publication may be left partial. Cancelling promotion CI fails the promotion without merging. Audited. |
| `run_rerun` | Rerun a failed or interrupted run at the same SHA and ref, linked to the original (rerun_of), for what run_retry refuses: a failed step such as a flaky test, a run that is no longer the newest of its ref, and tag/release test failures. SHA-keyed status, wait and promote then follow the newest run for that SHA. Refused while the ref has an active run, when a branch has moved past the run's SHA, and for promotion (use promote_retry), plan and apply runs. A run that passed every step and failed only in upstream publication is not re-executed: its publication is retried as publish_retry does (mode publish_retry, action publication_retry). Responses name the started action and include run_status/run_phase; no-work refusals say nothing started. Non-admin uplinks may rerun branch-push runs; tag/release runs need an admin; force-free replay requires the newest source, an unchanged tag object, current credential-aware plans and complete terminal observations for every tag attempt (or no Workflow intent). Secret-store/WIF starts, incomplete observation, legacy plans, older sources, moved tags and unknown evidence require force. Pending Workflow deletion is refused even with force. A tag that is already published or green is refused, so it is never published twice. Audited as run.rerun. |
| `run_hold_list` | List the parked retry attempts (holds) of an assisted branch CI run: step, pod, attempt, held_since, abandon_at (when the server abandons the hold unless someone acts: plan against this), kubelet deadline, whether run_hold_patch can apply files, and whether the hold was already continued or abandoned. Also returns the run's assist record and the installation policy (hold_ttl_s, exec_timeout_s in seconds). A run holds only when it asked for assistance (push option oberth.assist=1 or run_rerun assist), assist.enabled is on, and a credential-free step exited nonzero; the retry Pod parks before its command until run_continue, run_hold_abandon or assist.holdTTL (the server abandons an idle hold past the TTL; each exec, patch or continue restarts it, up to twice the TTL after held_since; the Pod deadline shown also covers the step's own budget after release). Read-only. |
| `run_hold_exec` | Run one bounded command (argv, no shell) inside the held step's main container, as an uplink whose scope covers the run's repository (who may push to it) or an admin: for diagnosing the failure in the live environment. Bounded by assist.execTimeout and assist.execOutputBytes (the deadline stops the stream; a process the image keeps running is not killed), serialized per run, capped installation-wide, and recorded in the run's assist record and audit (run.assist.exec) before the command runs. Any exec marks the run assisted: it is never evidence from this point, whatever the command did. |
| `run_hold_patch` | Apply a unified diff to the held step's writable source tree (/work/src) through the server's own Pod helper: exact-context hunks, relative cleaned paths only, no renames, binaries, mode changes, .git or symlink traversal, bounded by assist.patchMaxBytes/patchMaxFiles. The diff's sha256 is recorded in the run's assist record and audit (run.assist.patch) before it is applied, and the run is assisted from then on: never promotion, release or green-for-SHA evidence; push the change as a commit for a real green run. |
| `run_continue` | Release a held step: the parked executor runs the step's real command on the (possibly patched) tree and Argo continues the DAG natively on success. Recorded before the release (run.assist.continue); the run is assisted (as it already is after any exec or patch) and its status carries an 'assisted: ... not evidence' reason line. An assisted run is never published upstream, never a promotion source and never green for its SHA, even when no patch or exec happened (a plain release is still not evidence). A release that fails abandons the Pod. Any uplink whose scope covers the run's repository, or an admin. |
| `run_hold_abandon` | Delete a held step's parked Pod so the step fails normally and the run ends red as it would have without assistance. Recorded as run.assist.abandon. Any uplink whose scope covers the run's repository, or an admin. |
| `run_purge` | Purge retained logs, artifacts, or both for one terminal run. Requires an unscoped admin uplink and a reason. Active runs, release evidence, promotion evidence, open-issue evidence and active grants are protected. Run and step history stays visible; every purge is audited. |
| `artifacts` | List the files a run kept: test reports, coverage output, anything the pipeline copied to OBERTH_ARTIFACTS. Cheap; call this before artifact_get. |
| `artifact_get` | Return one artifact's contents by run ID and name, optionally filtered server-side. Prefer a pattern over retrieving a whole file: the response reports total and matched line counts so a narrowed read is never mistaken for a complete one. |
| `run_logs` | Return one exact burn/step log by full run ID or unique hexadecimal prefix (at least 8 characters), optionally filtered server-side. With no pattern, limit, tail, offset or byte_offset, returns the last 200 lines and a window explanation. Pass tail:true with limit:N for a larger tail, or limit:0 for the existing bounded full read. Explicit filters retain their behavior. A step without burn selects its unique matching burn; ambiguous names return the candidate burns. Active steps without retained output return an actionable pending error. Log discovery/filtering scan at most 4 MiB each per call and stop head pages early on limit. total_scanned_bytes includes both phases; scan_limit_bytes is the aggregate cap (4 MiB terminal, 8 MiB active). Pattern scans allow one concurrent call per bearer token and two globally; waiting shares the 10-second read budget. When has_more is true, continue with next_byte_offset as byte_offset, retaining tail/pattern/context and resetting offset to zero. Counts are lower bounds unless scan_complete is true; bytes is exact only when bytes_exact is true. withheld_bytes/withheld_lines report omitted data. Lines above 64 KiB are omitted and counted. |
| `wait` | Long-poll until a SHA or exact run reaches a terminal state; timeout returns still-running cleanly. Timeout defaults to 600 seconds and cannot exceed 600; larger values are rejected. The CLI allows at least 60 seconds, or the tool timeout plus 30 seconds, unless --timeout overrides its client deadline. Pass id alone to wait for one run by full ID or unique prefix of at least 8 hex characters. When trigger is set, waits for a run with that trigger (e.g. 'release' for a tag-push run). |
| `sync` | Park the exact SHA's WIP branch upstream without a green gate; this is not completion or promotion evidence. Rejects promotion, release, plan, and apply runs. When the SHA has branch-trigger runs on multiple distinct branches, the explicit branch argument is required. |
| `promote` | Green-gate and publish without force. Provide sha for one source, or 1..32 ordered sources for a merge train: sources must be exactly tested green branch commits or authorized park refs. Parked sources always run full merged-tree CI, with skipped branch CI recorded. Trains wait in the existing target queue, merge onto its current head and run CI once before publishing. Conflicts name source and paths; skip_conflicting drops them and records the reason. Example: {"repo":"codeberg/acme/widgets","branch":"main","sources":[{"sha":"1111111111111111111111111111111111111111"},{"sha":"2222222222222222222222222222222222222222"},{"sha":"3333333333333333333333333333333333333333"}]} |
| `promotion_list` | Recover durable promotion IDs by exact repository/source SHA/status. Read-only, newest admission first; pass next_before (or next_cursor for a scoped uplink) as before for older records. Status comes from the committed promotion row, as in promote_status; remote ref equality alone never marks a pending record passed. |
| `promote_status` | Wait for an append-only promotion record to become terminal. A failed or interrupted promotion that was retried names its successor in retried_as. Timeout defaults to 600 seconds and cannot exceed 600; larger values are rejected. The CLI allows at least 60 seconds, or the tool timeout plus 30 seconds, unless --timeout overrides its client deadline. |
| `promote_retry` | Retry a failed or interrupted promotion: a new promotion of the same repository, source SHA and target, merged onto the target's current head and re-tested like promote. After a transient push-only failure, an unchanged target reuses the exact tested SHA without repeating CI. Same authorization as promote; a grant is consumed only by a delivered promotion, so the original grant authorizes the retry. Refused while a promotion of the same tuple is pending or delivered, or when the source SHA's newest run is not green. Audited as promotion.retry. |
| `issue_create` | Create a manual issue, installation-wide or filed under one upstream. Uplinks scoped to that upstream see it; an uplink scoped to one upstream files there by default. |
| `issue_get` | Get an issue by ID. Full record by default for compatibility. Optional fields selects only named fields; body_limit caps retained body/occurrence-history bytes with an explicit truncated N bytes marker. The numeric occurrences count remains exact. Read-only; does not renew locks. |
| `issue_get_many` | Get 1..50 issues in request order, each with issue or error (not_found or response_limit). Full records by default. Optional fields and body_limit apply to each issue before the 256 KiB structured JSON budget; body_limit truncates body/occurrence history with a truncated N bytes marker, keeping numeric occurrences exact. Use compact options or issue_get for response_limit IDs. Read-only; does not renew locks. |
| `issue_update` | Update work_status (todo, active, waiting, review), title and body, or move a manual issue to an upstream (empty or global means installation-wide, admin only). A non-admin can move only a visible issue to an upstream in scope. expected_revision atomically checks the current metadata revision; a mismatch is a conflict. Revision advances only for real title, state, work_status or upstream changes, not body edits. When expected_body_sha256 is set, the server checks the existing body hash inside the write transaction and rejects a mismatch. Returns only id, state, updated_at, occurrences, appended_bytes (zero); use issue_get to read content. |
| `issue_append` | Append text to an issue body with a dated separator, without replacing the existing content. Use for CI investigation updates without re-sending the existing body. Returns only id, state, updated_at, occurrences, appended_bytes (UTF-8 bytes of text, excluding separator); use issue_get to read content. |
| `issue_close` | Close an issue without deleting its history. Returns only id, state, updated_at, occurrences, appended_bytes (zero); use issue_get to read content. |
| `issue_reopen` | Reopen a manual issue without changing its ID or history. Already-open manual issues are unchanged; CI issues cannot be reopened manually. |
| `issue_delete` | Delete an accidentally created manual issue. |
| `issue_list` | List issue metadata, newest creation ID first (not latest updated), with total and faceted state/kind counts. Filters apply before pagination; counts exclude the cursor. Returns work_status and computed stale for open issues past their window. Defaults to all issues, 50 per page; pass next_before as before with the same filters until absent for a complete queue scan. |
| `issue_unlock` | Break an issue lock, recording its previous owner. Admin only; visibility and audit gate enforced. |
| `issue_close_many` | Close 1..50 unique issues with a reason and per-ID results. Admin only; active locks remain enforced, one audit action per closure. |
| `issue_lock` | Acquire or renew the caller-owned five-minute issue lock. |
| `uplink_invite` | Create a single-use, expiring CSR invitation from a locally generated credential digest; unscoped admin only. See [uplink invitations](docs/uplink-invites.md). |
| `uplink_cert` | Issue a clientAuth certificate from a local CSR; unscoped admin only. |
| `client_renew` | Renew this uplink using its current client certificate and a new local CSR. |
| `uplink_list` | List public uplink identities and usage. Unscoped admin only. No tokens are returned. |
| `uplink_revoke` | Revoke an uplink SSH key and bearer immediately. Unscoped admin only; audited; the last active unscoped admin cannot be revoked. |
| `uplink_rotate` | Finish rotation to an already registered active replacement key with identical identity, admin status and scope, atomically revoking the selected old credential. Unscoped admin only. Issue the replacement through uplink add first; no key material crosses MCP. |
| `access_dismiss` | Dismiss one stale pending declaration. Unscoped admin only. Copy its exact repository key from access_list; legacy or orphaned keys are supported. Scoped uplinks are refused. Audited; later admission can observe it again. |
| `access_list` | List secret access grants for a repository. |
| `access_allow` | Grant a step access to a secret path. Requires admin uplink. |
| `access_revoke` | Revoke a step's access to a secret path. Requires admin uplink. |
| `workflow_capability_grant` | Grant operator-owned Kubernetes access and extended resources to one registered repository. The operator owns the ServiceAccount, RBAC and namespace quota. Administrator only; audited. Existing technology-specific grants confer no authority here. |
| `workflow_capability_revoke` | Revoke a repository's operator capability. New submissions and restart adoption refuse the revoked authority. Running Kubernetes workloads remain operator-owned. Administrator only; audited. |
| `workflow_capability_get` | Read the repository's current or revoked operator capability within the caller's upstream scope. |
| `resource_grant` | Set a repository's per-container CPU, memory and ephemeral-storage ceilings, expressed as requests and limits. Does not change workflow declarations or legacy size/priority policy. Admin only; audited. |
| `resource_revoke` | Revoke a numeric resource ceiling to the defaults (requests/limits: 4 CPU, 8Gi memory, 16Gi ephemeral-storage). Admin only; audited. |
| `resource_grant_get` | Read the repository's numeric resource ceiling and grant provenance. Applies to every fresh submission, including releases. |
| `size_grant` | Raise (or set) the largest admission size a repository's branch, promotion and schedule runs may declare. The default without a grant is L; XL reserves the whole host and makes every other repository wait for a full drain. Re-enables automatic tag priority: only the oldest waiting tag per repository qualifies, and only within this size maximum. Ungranted repositories default to enabled within L. Admin uplink required; audited. |
| `size_revoke` | Return a repository to maximum L and disable its automatic tag priority, even if never granted. Explicit admin overrides, promotions and 60-minute aging of standard runs into promotion still apply; running work continues. Admin uplink required; audited. |
| `size_grant_get` | Read legacy admission size and priority policy, plus the independent numeric resource ceiling. Use resource_grant to change request/limit ceilings. |
| `queue_cap_config` | Set the branch queue cap with max_not_started (0 disables, the default). At the cap, accepted branch pushes stay in Oberth's Git cache and durable receive journal until run creation resumes. Automatic upstream publication still requires successful CI. Queued and claimed waiting runs count; deferred receives survive restart and create runs in arrival order. Tags/releases, promotions and schedules bypass the cap; park refs retain their configured behavior. Unscoped admin only; audited. system_status reports queue_cap. |
| `release_wif_list` | Read the release WIF capability map: boot default from Helm/flags, runtime overrides, the effective map used by preflight and admission, configured role names and any stored grant left inactive. Public data only. Unscoped admin only. |
| `release_wif_grant` | Grant or replace one registered repository's release WIF entry at runtime (#1497): its release service_account_name and template-to-role map. Roles, namespace and dedicated step identities stay Helm configuration. Validated against the boot map; applies to the next preflight and tag admission without restart. Unscoped admin only; audited, reason required. |
| `release_wif_revoke` | Revoke a repository's release WIF entry at runtime, including a Helm-configured entry or one for a retired, unregistered upstream/org/repo. New release admissions refuse its WIF leaves. Unscoped admin only; audited, reason required. |
| `release_wif_reset` | Remove a repository's runtime release WIF override so the Helm boot default applies again. Unscoped admin only; audited, reason required. |
| `queue_priority_config` | Replace administrator RC branch globs (case-sensitive * and ? match slash); [] disables RC priority. Applies to queued runs immediately. A standard run waiting 60 minutes joins promotion (reason aged) at its own FIFO position; no automatic path grants high. Unscoped admin only; audited. |
| `queue_priority_get` | Read administrator RC globs and the aging threshold after which standard work joins promotion. Unscoped admin only. |
| `priority_grant` | Allow an identity to request submission priority up to max_class until expires (RFC3339). Unscoped admin only; audited. |
| `priority_grant_list` | List submission priority grants, including expiry. Unscoped admin only. |
| `priority_grant_revoke` | Revoke an identity's submission priority grant. Accepted runs keep their priority. Unscoped admin only; audited. |
| `run_priority` | Set and pin a waiting run to low, standard, promotion or high, including explicit lowering. Cannot preempt admitted work. Unscoped admin only; audited, reason required. |
| `admission_hold` | Hold new admission except allowed tag/release/promotion classes. Unscoped admin only; audited, reason required. Existing grants are untouched. Default allow is tag+release; [] holds all. Expires after duration (default 2h, maximum 12h), including across restart. Read current hold in system_status admission.hold. |
| `admission_release` | Release one admission reservation no owner will release: typically a granted reservation still held for a run that is already terminal because its owner hung or was lost (also an ungranted one of a terminal run, or a retained one while a stopping scheduler drains). A live run's reservation is refused (use run_cancel). The run's Workflow is deleted first as proof; a granted run with no recorded Workflow (a trusted preflight) needs force, as does a failed deletion. Admin uplink required, unscoped only; audited. system_status lists every reservation. |
| `repo_usage` | Report bounded outside-step kubelet/cAdvisor usage by repository and step: per-container app/init/helper/VM launcher p50/p95/max, pod storage peaks and per-run pod overlap; requests, paired used/request ratios and per-step peaks grouped by observed Workflow size class (S/M/L/XL or unknown). Peaks are sampled lower bounds; missing/killed samples remain unknown. Does not change admission. |
| `repo_list` | List registered repositories as id, name, upstream, default_branch, archived_at, non_default_branches (mirror branches besides the default branch; null until counted) and one latest_run (id, status, ref, finished_at). Archived repositories are hidden by default and counted in archived_hidden; pass archived=include for the whole catalog or archived=only. No run history or probe payload. |
| `repo_archive` | Archive a repository, retaining all history and refusing new work. Requires an unscoped admin; refuses in-flight runs and pending promotions. Drops the Git cache. |
| `repo_unarchive` | Unarchive a repository and allow new work again. Requires an unscoped admin. |
| `repo_remove` | Remove a repository mapping and its Git cache. Requires admin uplink. Refuses if runs are in-flight or promotions are pending. |
| `branch_delete_many` | Delete many mirror branches in one batch; upstream deletion follows asynchronously. Dry run by default: returns plan_id and per-branch head_sha, merged/patch_equivalent (git cherry), ahead/behind against the default branch head the forge last confirmed (not unpublished mirror commits) and last_commit; unmerged branches are listed separately. Select an explicit branches list of {branch, expected_sha}, or merged_into (the default branch) and/or older_than (30d, 72h) with optional exclude_prefixes. Executes only with confirm:true and the dry run's plan_id; any moved head makes the plan stale. Each ref is compared and deleted; unmerged branches need allow_unmerged and, to confirm, a reason that is recorded. The default branch is never deleted. Configured reserved branches are never selected; one may be deleted alone with confirm_reserved "<upstream>/<org>/<repo>:<branch>". Every plan entry has class merged, patch-equivalent, unique or unchecked (beyond the git cherry budget), and classes counts every candidate; report:true (dry run only) also lists kept candidates. Unscoped admin only; one audit entry per batch. Read per-branch upstream state with branch_deletion_list. |
| `branch_deletion_list` | List accepted branch deletions newest first with their upstream state: pending (with attempts and retry_after), propagated, failed (upstream unchanged, mirror branch restored) or skipped (branch pushed again, head moved before a prune, upstream repository archived with the mirror deletion kept, or a failed record resolved; error holds the reason line). Filter by repo and status, or read one prune batch or receive event with batch. counts holds every record per status for repo (or all repositories) or for the whole batch; page_counts only the returned page. Unscoped admin only. A full page returns next_before. |
| `branch_deletion_resolve` | Close failed branch deletion records as skipped with a reason line; no ref changes (a failed deletion already restored its mirror branch and left upstream unchanged). With ids, resolves those failed records of repo. Without ids, resolves only failed records whose branch no longer exists on the mirror and lists the rest as kept. Failed records also resolve on their own when a later deletion of the same branch propagates, and hourly when their branch is gone from the mirror. Unscoped admin only; one audit entry per call. |
| `ref_delete_many` | Delete stray mirror refs outside refs/heads/ and refs/tags/, such as refs/remotes/upstream/* left by an old mirror tool; they are never mirrored, so the deletion is local and nothing changes upstream. refs/heads/, refs/tags/, refs/oberth/ and refs/replace/ are never listed or deleted. Dry run by default: without a selection it lists every stray ref (at most 5000) under kept; returns plan_id and per-ref sha, object_type, symref, last_commit and class default-branch, tag (reachable from a tag only), unreachable or not-commit, with classes counting every stray ref. Select an explicit refs list of {ref, expected_sha}, or prefixes such as refs/remotes/. Unreachable and not-commit refs need allow_unreachable. Executes only with confirm:true and the dry run's plan_id; any moved ref makes the plan stale. At most 500 refs per call; each is compared and deleted on its exact object ID, and a symbolic ref is deleted itself, never its target. Unscoped admin only; audited with every ref and object ID. |
| `receive_outbox` | Repair durable receive records that block a repository. list shows every pending or quarantined record; show reads one (leniently when corrupt); retry delivers it now (an operator-quarantined record is restored first when otherwise valid); abandon moves it aside to abandoned/ so the repository accepts pushes again. retry and abandon are audited; abandon needs reason and the record id from show. Unscoped admin only. |
| `run_list` | List the queue first, in scheduler claim order (highest effective priority first, then FIFO), then running runs newest start first, then terminal runs newest finish first. Optional repo/ref and status filters apply before paging; status accepts one display status or a list. Queued and running runs are never dropped by the limit or pushed below finished runs. Every run that has not started has display_status queued with display_reason naming what it waits for. superseded:true finds branch runs with newer queued/running work in the same repository/ref/schedule-entry lane, excluding tags and promotion attempts. Priority-protected runs and runs with pending publications can still match; review before explicit admin run_cancel. A full terminal page returns next_before (or next_cursor for a scoped uplink); pass it as before with the same filters for older runs until it is absent. |
| `audit_list` | List verified audit-chain entries newest first. Admin uplink required; a scoped uplink gets no audit rows. Each entry carries its chain position (id, sha256, previous_sha256) and is recomputed against the chain's hash before it is returned. A full page returns next_before; pass it as before for older entries. |
| `schedule_list` | List the schedules declared in repositories' .oberth/schedule.yaml with their cron, next run, last outcome and any pause. A scoped uplink sees its own repositories. |
| `schedule_pause` | Pause one schedule entry: a due tick records outcome paused instead of firing. Requires admin uplink; audited as schedule.pause. |
| `schedule_resume` | Resume a paused schedule entry. It fires once at its next tick if it came due while paused. Requires admin uplink; audited as schedule.resume. |
| `schedule_trigger` | Run one schedule entry now: queues a trigger=schedule run at the entry's branch head through the normal CI admission path, as the caller. Refused while the repository has active runs, or the entry is paused, unless force is set (a forced standard-priority run supersedes only unprotected active runs of the same branch and entry; protected priority/RC runs are preserved, and a priority submission queues independently; other entries and ordinary pushes remain independent). Requires admin uplink; audited as schedule.trigger. |
| `system_status` | Return system health: database, upstreams, cluster, audit, and version. |
| `publish_retry` | Retry the upstream publication for a branch or tag run whose burns all passed but whose upstream push failed. Input is one full run ID or unique hexadecimal prefix of at least 8 characters. Refuses when the run is not in a publishing-failed or upstream publication infrastructure-failed state, or when the cached Git object no longer matches the record. Transient server/transport failures retry automatically up to five attempts over about thirty minutes. Responses name the actual action and include run_status/run_phase from the same committed snapshot as publication status. A pending response says publication is queued and names the publication and run IDs; poll run_get or wait for the run while finalization is recovered. A promotion (or its CI run) is retried with promote_retry instead, which reuses a tested push-only candidate when its target is unchanged, or merges onto the current head and re-tests. |
| `secretstore_plan` | Compute the policy/role diff that `oberth secretstore sync` would apply based on current approved grants. Admin uplink required. Read-only: the server never receives an admin Bao token. Returns per-repo policy names and paths; never secret values. |
| `secretstore_sync_receipt` | Record a secretstore.sync materialization receipt in the audit chain. Admin uplink required. The server recomputes the plan digest and records status current or stale. Posted by `oberth secretstore sync` after a successful sync. |
| `secretstore_verify` | Verify real secret-store login and KV reads using configured server or release-tier trust. Admin uplink required. Returns paths/key counts, optionally field names; never secret values. Does not change infrastructure or grants. |
| `promote_grant` | Create a time-bounded promotion grant authorizing any non-admin uplink to promote an exact (repository, SHA, target) tuple. Admin uplink required. The grant is single-use: consumed atomically when a matching promotion succeeds. |
| `promote_grant_revoke` | Revoke an active promotion grant. Admin uplink required. Already-consumed or already-revoked grants are refused. |
| `promote_grant_list` | List promotion grants with optional repository, SHA, and status filters. Read-only. |
| `run_wait` | Block server-side until one run finishes, holds a retry (assisted runs: a parked step waits for run_hold_*), or changes in any way, bounded by timeout_s (default and maximum 600). Returns state finished, held, changed or timeout with the run summary, its unreleased holds and retry_hint. Read-only; replaces client polling loops. |
| `events_subscribe` | Set which agent events this MCP session receives as server push: a notifications/claude/channel message on the session's GET event stream (Claude Code shows it only in an interactive session started with --dangerously-load-development-channels server:<name>; otherwise use run_wait and events_since), and notifications/resources/updated for run://<id> resources the session subscribed. Kinds: run.finished (status, failed step, reason, retry_hint), run.held (step, pod, held_since, abandon_at, holds_in_run), run.hold_abandoned (step, pod, reason ttl, cap or the agent, held_since, abandoned_at, failed attempt and exit code), run.lock_wait (a native Argo lock wait past agentEvents.lockWaitThreshold, with the lock and its holders; lock Parallelism/workflow-controller with postponed true is a Workflow the Argo controller postponed for its parallelism), promotion.finished (with the prune_source outcome), release.published and publication.failed (final, or an automatic retry attempt). Without a call a session receives every kind for every repository in its uplink scope. Events carry a monotonically increasing id; delivery is at-least-once; after a reconnect resume with events_since. Read-only. |
| `events_since` | Read agent events after a cursor in id order, scoped to the caller's uplink scope and the same optional filters as events_subscribe. Use after a reconnect or without a push stream: pass next_after_id back as after_id. gap is true when older events were pruned (agentEvents.retention). Read-only. |
| `events_wait` | Block server-side until at least one agent event after after_id passes every filter (the caller's uplink scope, repos, refs_glob, kinds, own, run_ids), or until timeout_s (default and maximum 600). Returns the events_since page of the first match; pass next_after_id back as after_id for the next wait, so one call covers every run and every hold of a worker. On timeout: no events, timed_out true and next_after_id moved past every event scanned. Without after_id it waits for the next event. run_ids match run events only. Read-only. |
| `sync_status` | Native Argo synchronization: the current holders and waiters of every semaphore and mutex observed on active runs (sampled every agentEvents.interval from Workflow status.synchronization), and of the Argo controller parallelism as Parallelism/workflow-controller (waiters are postponed Workflows, holders admitted ones), and the bounded stored history per lock: holder and waiter intervals, the maximum number of concurrent holders (proves two runs never overlapped) and wait durations. A scoped uplink sees only its own runs. Read-only. |

Authenticated `GET` endpoints (`/api/runs`, `/api/repos`, `/api/issues`,
`/api/status`) serve the same state; `/healthz`, `/livez`, and `/readyz` are
unauthenticated.

## Architecture

- **One pod, two ports** — SSH on NodePort `30022`, HTTPS/MCP on `30443`
  (TLS 1.3 or newer). Argo Workflows provides the execution engine.
- **One Workflow per run** — each push submits an Argo Workflow from the
  repository's `.oberth/build.yaml` or `.oberth/release.yaml`. Each DAG task
  runs in its own container. Repository setup steps install pinned,
  checksummed tools into a shared workspace volume.
- **PVC for state** — SQLite, bare Git caches, run workspaces, and retained
  logs on one volume, kept across uninstall. hostPath Go caches are split by
  repository *and* trust tier, so a poisoned branch build can never feed bytes
  into a signed release artifact.
- **Audit chain, external anchoring opt-in** — the gap-free SHA-256 action
  chain always runs and is verified at startup, on every cycle, and before
  every mutation. By default (`auditAnchor.tsaURL` and `auditAnchor.rekorURL`
  empty) no external service is contacted — Rekor and public TSAs are free
  services with no SLA, and a local kind install should never depend on
  them. Opting in layers on: RFC 3161 timestamps from an independent TSA →
  linked Rekor witnesses under a stable identity derived from the SSH host
  key → immutable Kubernetes continuity records the server can create but
  never modify. With both configured, a whole-database rollback fails closed
  instead of forgetting.
- **Degraded-mode startup** — with the Rekor witness enabled but unreachable
  at startup, the pod comes up running-but-not-ready with every mutation gate
  closed and retries recovery with backoff; it does not crash-loop.
  `auditAnchor.rekorCA` / `auditAnchor.tsaCA` pin private CAs for self-hosted
  witnesses, and `auditAnchor.acceptWitnessChainReset` is the one-shot,
  loudly-logged acknowledgment for reinstalls that abandon a published chain.
- **Rebuild and restore** — every identity Secret is generated on first
  install and kept across uninstall (`helm.sh/resource-policy: keep`). To
  adopt restored credentials after a cluster rebuild, set
  `ssh.hostKey.existingSecret`; Helm then neither adopts nor overwrites that
  Secret. A restored host key without restored local state fails closed
  against the published witness chain until the reset acknowledgment above.

## Security

- Workflow Pods use server-enforced identities, dropped capabilities,
  read-only root filesystems, seccomp `RuntimeDefault`, bounded resources and
  deadlines. Credentialed main containers and the executor receive separate,
  scoped projected tokens; ordinary build containers receive no store token.
  [Non-root test leaves](docs/nonroot-test-leaves.md) have a separately enforced
  UID and controller contract.
- **Declared paths and the trigger together are the security boundary.**
  Pipelines without declared secret-store paths bind to the pipeline
  ServiceAccount and receive no secrets on either trigger. Release pipelines
  with approved paths bind to the credentialed ServiceAccount — the only
  identity whose OpenBao role carries release-secret grants; branch (CI)
  pipelines with approved paths bind to the separate ci-secrets
  ServiceAccount, whose role reaches the upstream subtree only, so release
  credentials are unreachable from a branch push at the Vault layer. Branch
  pipelines may only declare upstream-scoped paths; system-namespace paths
  require a release trigger and an administrator allowlist entry. All fetched
  secret-store values are delivered to memory only.
- Secret-store values never appear in etcd, Kubernetes objects, node disk, or
  logs; per-run store tokens are read-only, short-lived, and revoked after
  every fetch. Every fetch is an actor-attributed entry in the audit chain.
- Bearer tokens map 1:1 to uplink SSH-key fingerprints and are stored only as
  SHA-256 digests, displayed exactly once. SSH accepts the two Git smart-
  protocol commands and nothing else — no shell, no PTY, no forwarding.
- RBAC is namespace-scoped with one documented exception: an optional
  `system:auth-delegator` binding so the store validates logins via
  TokenReview. Audit continuity records are create/get/list only.
- The audit chain is tamper-evident — including against the box it runs on —
  not tamper-proof. See [docs/security.md](docs/security.md) for the full
  invariant list.

## Tested on

**k3s, kind, and k0s** — a 187-scenario acceptance matrix covering Git
ingress, the CI lifecycle, promotion (including concurrent races), release
burns, the secret-store battery, MCP, persistence across restarts, and the
security posture, executed end-to-end on all three engines with **zero
product-blocking failures** and identical behavior across engines. The two
resilience findings it produced — degraded-mode startup during a public Rekor
outage and CA-trust knobs for self-hosted witnesses — shipped in v0.10.51.

## Development

For database changes, follow [Adding schema migrations](docs/schema-migrations.md).

```bash
go vet ./...
go test -race -count=1 ./...
golangci-lint run ./...
make build       # server and runner binaries
make local-flow  # executable FAB Example-flow gate, no cluster required
```

Watch a live run's current burn and step:

```bash
kubectl get pods -n oberth --watch \
  -L oberth.ci/repo,oberth.ci/trigger,oberth.ci/burn,oberth.ci/step
```

Oberth is its own CI authority: this repository carries
[`.oberth/build.yaml`](.oberth/build.yaml) and
[`.oberth/release.yaml`](.oberth/release.yaml), and an admitted release tag runs the
release workflow that builds, signs, publishes, and reads back every artifact.
Those workflows hold no upstream-forge mutation credential; Oberth publishes
the exact annotated tag only after the run is green.

## Web UI
<img width="1456" height="773" alt="image" src="https://github.com/user-attachments/assets/33684b0e-f2ef-4f77-8754-f17a593179e5" />


## Documentation

- [CLI command reference](docs/cli-reference.md) — rendered help for every command and subcommand

- [docs/mcp-setup.md](docs/mcp-setup.md) — connect Claude Code or any MCP client
- [docs/architecture.md](docs/architecture.md) — run lifecycle and recovery
- [docs/security.md](docs/security.md) — security invariants
- [docs/credential-lifecycle.md](docs/credential-lifecycle.md) — remote uplink and grant administration
- [docs/secretstore-verification.md](docs/secretstore-verification.md) — three-tier secret-store verification
- [docs/helm-service-migration.md](docs/helm-service-migration.md) — two-stage ownership-safe Service migration
- [docs/standalone-configuration.md](docs/standalone-configuration.md) — preserving deployment protocol identities and configuring an owned private module namespace
- [Historical release evidence](docs/archive/README.md) — superseded pins and qualification records
- [Deadcode gate](docs/deadcode.md) — pinned reachability checks and explicit exceptions
- [docs/backup-restore.md](docs/backup-restore.md) — `oberth backup` / `oberth restore`: one encrypted file with the history and the secrets pipelines use, restored onto a fresh install
- [AGENT-CONTRACT.md](AGENT-CONTRACT.md) — the cross-component compatibility contract

## License

Apache License 2.0 — see [LICENSE](LICENSE).

Source delivery uses the Git CLI exclusively. The runtime contains static Git,
static Oberth, its native SSH transport alias and a CA bundle, runs as UID/GID
65534, and includes neither a shell nor OpenSSH. See
[the source transport contract](docs/gitcache.md) for supported transports and
migration of retired configuration.

`hack/test-gitcache.sh` is the complete race-enabled CLI cache gate.
`hack/test-archive-history.sh` also checks every file at every commit in the
public three-pack Kubernetes gengo fixture through the CLI ArchiveTar path,
and verifies the incident-triggering checkout against Git archive.
