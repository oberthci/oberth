# Write a pipeline

A pipeline is an Argo Workflow: `.oberth/build.yaml` runs CI; `.oberth/release.yaml` runs releases.
See the [admission boundary](security.md#the-six-things-oberth-protects) and
[source delivery](#source-is-explicit-and-private-to-each-step).

## Start with a complete file

Prepare a starter before connecting:

```sh
oberth init
oberth validate .oberth/build.yaml
```

The starter only prints a success message: it neither fetches source nor tests your project. Replace
it with your tests. `init` refuses overwrites without `--force`; `--release` changes only the
destination to `.oberth/release.yaml`, adding no signing or publication. `validate` connects to no
server. It runs push admission with placeholder server parameters and reports all refusals in
file/field order. “Needs before the first run:” lists installation requirements and exact commands;
needs alone exit 0, refusals exit 1. `-o json` returns `refusals` and `needs`. Never define reserved
parameters yourself. After [connecting](../README.md#first-green-build), check local build YAML:

```sh
oberth check --local .oberth/build.yaml
```

This uses server repository context and grants. Bare `oberth check` checks the server-resolved
revision, ignoring local edits. Neither starts a run or proves images/source are fetchable. See
[releasing](releasing.md#release-your-project) for release selection.

| File | Copy to | Shows |
| --- | --- | --- |
| [Build](examples/build.yaml) | `.oberth/build.yaml` | Source and a quoted stamped parameter |
| [Cache](examples/cache.yaml) | `.oberth/build.yaml` | An explicitly granted existing claim |
| [Pinned source](examples/pin.yaml) | `.oberth/build.yaml` | Another repository's exact commit |
| [Release](examples/release.yaml) | `.oberth/release.yaml` | Named secret delivery |

Replace fixture images and pinned repository/commit values with your own, and configure required
grants before pushing. The examples supply no images, repositories, claims or credentials. Build
images need `sh`, `git`, `tar` and standard file tools; the release image also needs `oberth`.
Replace the archive command with your tests or publisher. Scratch archives are not uploaded.

## Source is explicit and private to each step

A source-consuming step declares an input artifact, named `source` in the examples: plugin `oberth`,
key `{{workflow.parameters.oberth-source}}`, configuration `{"version":1,"kind":"main"}`, path
`/work/src`, mode `0755`. Oberth supplies the key at submission; never define reserved `oberth-*`
parameters yourself. `main` means the run's primary repository at its admitted commit, not the
default branch. A working directory under `/work/src` needs this input unless an authored volume
supplies it. Symbolic links and submodule entries are not delivered. Later branch changes do not
alter delivered source. Each step has a private copy, including pins. Committed files are read-only;
steps may create or replace files, but changes stay in that Pod. Outputs disappear with
it unless declared. The build example writes an archive on temporary `emptyDir` storage. For Git,
use `safe.directory=/work/src` as shown: this trusts the directory's owner without changing
permissions. Oberth adds that setting for main-source steps while retaining your settings; pins
receive no hook. Explicit per-command configuration is portable. A pin (kind `pin`, full repository
name and commit ID on the producer's default history) arrives under `/work/sources/<name>` without
`.git`. An administrator allows each pair, a repository's own older commits included, with
`oberth upgrade --source-pin <consumer>=<producer>`; a refused push prints it for its pair.

## Pass refs as data

Refs can contain shell syntax. Pass them and other parameters through environment variables or
separate arguments, then quote shell variables. Never interpolate parameters into shell program
text. The build example passes the stamped `oberth-trigger` through `env` as `TRIGGER`; it is not a
branch-name parameter and never becomes shell source.

## Ordering, retries and deadlines

Use Argo steps, DAG dependencies, retries, synchronization and `activeDeadlineSeconds`.
Semaphore keys belong in `oberth-sync-limits`. Mutex names must be literal `upstream/org/repo/lock`
for the exact repository, e.g. `forge/team/app/publish`. Omit the namespace or use the pipeline
namespace. Defaults and shared/inline templates follow this rule; lock names are never rewritten.
Oberth adds no queue or cap. Retries are attempts of one step; reruns are new runs. Memory limits
are bounds; inspect detail for OOM or eviction. `oberth lock status` shows holders, waiters
and `stale` ended runs. Argo `parallelism` is installation `runs.parallel` (default 4; 0 unlimited).

## Cache and outputs

A cache is an existing administrator-granted claim. The example's `build-cache` must match it;
admission refuses an unapproved claim even if present. Copy the refusal's exact command:

```sh
oberth access allow --kind pvc --tier ci 'forge/team/app' 'build-cache'
```

Mount it away from source and point build tools there. Branch code can poison shared caches: use a
separate release-tier claim for release bytes. For workflow-scoped storage, use
`volumeClaimTemplates`; pass outputs between steps on that volume, with Argo dependencies making the
writer finish first. This differs from the existing shared cache claim above.

## Secrets and identity

Declarations request authority; only an administrator grants it for an exact repo/file/step. Branch
builds cannot select release accounts through YAML, account names or release parameters. A step
lists the store paths it reads in its own template annotation `oberth.ci/secret-paths` (the release
example: `release/signing-key`) and calls `oberth secretstore exec -- <command>`. Oberth supplies
`/run/oberth/bin/oberth` (`command: [oberth, ...]` resolves there), the token, address and CA.
The program is pinned; no download or authored projection is needed. Secret files stay in memory.

A step that consumes a projected identity token itself declares that projection and needs its own
exact audience grant. Use one read-only main-container mount at a path of its choice and a lifetime
of 600 through 3,600 seconds. When this grant is missing, admission prints its exact command:

```sh
oberth access allow --kind identity --audience 'publisher' \
  'forge/team/app' '.oberth/release.yaml' 'publish' 'identity'
```

An administrator checks the refusal's repo, file, step, path and audience, then runs its command. A
step uses one store account and gets only paths it declares and is granted; a Workflow-wide list is
refused. If any secret grant names the step, each path needs a named grant; `*` fills no gap. A `*`
grant is one store account for the whole file: grant steps that must be kept apart by step name, as
the release guide does. A step with a secret grant cannot be held for inspection.

`oberth secret put` grants the named step access first, then stores the value, in one command. The
administrator supplies the value on standard input, never in command arguments:

```sh
oberth secret put --repo <repo> --file .oberth/release.yaml --step release-secret-reaches-step \
  release/signing-key < <value-file>
```

Use the administrator profile for that command. The command prints:

- Success: `Secret access granted.` then `Secret stored for the approved step.`
- Storage refused: `Secret access granted.` then `Secret not stored.`
  The error gives the reason and remedy. Storage failure does not revoke the grant.
- Grant refused: `Secret access not granted; secret not stored.`
  The error gives the reason and remedy; no storage request follows.

A lost reply leaves the outcome unknown; inspect the grant and stored state before retrying. Static
store rules let the server write values; each step reads only its own prefix. Inspect and revoke
through [grant operations](operations.md#grants-and-secrets).

## Kubernetes resources and virtual machines

Ordinary containers need no capability grant, but a resource beyond `cpu`, `memory`,
`ephemeral-storage` and `hugepages-*`, such as `devices.kubevirt.io/kvm`, is a host device: it needs
the exact `--kind device` grant that admission prints for its named step. A resource template needs
an administrator-approved capability: namespace, executor account, resource kind and allowed actions
for that workflow. Copy [resource.yaml](examples/resource.yaml) to `.oberth/build.yaml` and
[resource-capability.json](examples/resource-capability.json) to `resource-capability.json`. This
build creates a ConfigMap in `build-resources`; its capability permits only `create` on that
kind/namespace with account `resource-writer`. The cluster administrator first creates that
namespace and, in the pipeline namespace, that separate account with RBAC for ConfigMaps in
`build-resources`. Granting creates none of these. As Oberth administrator, approve and inspect:

```sh
oberth workflow capability grant --repo <repo> \
  --capability "$(cat resource-capability.json)" --reason 'Create build notes'
oberth workflow capability get <repo> --json
```

Admission selects the account; do not author `serviceAccountName`. After the example, the cluster
administrator deletes the generated `build-note-` ConfigMap. Owner references cannot clean up across
namespaces; use exit handlers there. A capability over a Pod-creating kind (Pod, Job, virtual
machine) runs any Pod in that namespace, bounded only by its Pod Security and the account's RBAC;
Oberth's own namespaces are never a target. Never bypass refusals with branch infrastructure keys.

## Build tags and release tags

Ordinary tags select `.oberth/build.yaml`; semantic versions such as `v0.1.0` select
`.oberth/release.yaml` from default history. Ambiguity refuses. Follow the
[project release steps](releasing.md#release-your-project) for eligibility and publication.

## Plans and budgets

The admitted plan is immutable, in run order. Pending entries exist before Pods start; retries stay
attempts of one logical step. Dynamic placeholders represent unresolved expansion, so `step_count`
is a lower bound while `steps_planned_exact` is false. Observations expand the display, not the
plan. Pipeline files have a 1 MiB limit; submitted source-consuming Workflow specs have a 512 KiB
limit. Display plan limits: 2,048 entries / 1 MiB; excess expansion collapses to dynamic
placeholders. These plan budgets do not limit native fan-out or refuse admission for expansion. The
server reserves at least 1,856 MiB of source-delivery scratch, separate from step storage. Pushes
allow 256 refs and a 1 GiB pack by default (`server.runtime.receive_max_input_bytes`).

## CI issues

Failing default-branch pushes and schedules open or update CI issues; a newer pass closes them.
Other branches and tags open none. A passing promotion run, or a delivered fast-forward's tested
run, counts as the default branch; a failed one changes nothing. Assisted, cancelled, restart-cut or
evicted runs are no evidence. Never change stored run state. Owners close manual issues.
