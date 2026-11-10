# Oberth local upgrade rehearsal

Run from this checkout, for example:

```bash
bash hack/upgrade-e2e/run.sh --from 0.17.81 \
  --candidate fix/ob-82-rc --candidate fix/ob-83-rc
```

`--from` and the ordered `--candidate` list are required. They can also be set
with `FROM_VERSION` and whitespace-separated `CANDIDATES`. Requires Docker, k3d,
Go, kubectl, Helm 3, cosign, jq, curl and ripgrep. Set `E2E_HELM` if the Helm 3
executable is not named `helm`.
An optional `E2E_WRAPPER` executable receives the rehearsal command and its
arguments; unset it to run directly.
The script creates its own Docker cluster and private kubeconfig, then deletes
both on success or failure. It never selects an existing cluster.

The script downloads the requested published chart and signed release record,
checks the public key against its pinned SHA-256, verifies the record's signature,
and verifies the chart digest and server image reference against that record.
For `--from` releases after v0.17.95 the signature check also requires the
record's public Rekor inclusion proof. Releases up to and including v0.17.95
were signed without a transparency-log entry, so for those versions only the
pinned-key signature is checked (cosign then prints its tlog warning), and the
rehearsal does not establish publication time or transparency inclusion.

It builds each candidate in order. Use exact commit arguments to repeat a
trial, or `E2E_REPO` to select the source repository. Application versions come
from each candidate chart. Each ref is resolved once and recorded in `sources.txt`.
Candidate images replace only the server executable in the published runtime.
The script refuses that shortcut if the candidate changes any Dockerfile or
static Git substrate input. The imported containerd manifest digest becomes
the chart image reference; no registry upload is required. These images are for local testing only.

The included backfill regression assertion must fail on the published source
and pass unchanged on each candidate. Select a release path spanning that fix;
this assertion is specific to that behavior, even though versions are arguments. Focused store tests also
exercise migration replay protection and queue classification. All builds and checks inherit the optional wrapper.

The published store API seeds a local Git repository, four queued runs spanning
normal, high and highest priority, and one running branch run bound to a real
Argo Workflow. Its suspension occupies the sole execution slot during each
upgrade and restart comparison. A prepopulated native Git cache supplies exact
commit checkouts. A disposable local bare upstream supports trusted-policy
refreshes without network access. The source includes trivial build
and release workflows. After every checkpoint, the script deletes the holder,
observes the daemon create the highest-priority queued Workflow, deletes that
Workflow, and observes the high-priority run next. The controller suspends these
generated Workflows before containers run. The high-priority Workflow becomes
the next holder, and two new waiters refill the queue. Every next upgrade or
restart compares against the state captured after that deliberate refill.
Original ledger rows remain protected throughout; the script never restores a
database snapshot to undo scheduler activity.

This exercises real daemon claiming, admission, exact-SHA checkout and Workflow
submission. Fixture creation bypasses push authentication, and the controller’s
suspension excludes container execution, normal completion and publication.

OpenBao is required by the published chart. An isolated in-memory development
store supplies newly generated server identities through Kubernetes auth. No
existing tokens, cloud credentials, real upstreams or repository secrets are
used. The CLI uses its token helper entirely inside that pod; the script never
exports its login, reads private identities, or collects identity-store logs.
The chart's development HTTP mode and disabled NetworkPolicy are local test
values. The test does not cover production TLS, networking or OpenBao recovery.
In the Oberth namespace, Helm release metadata is the only Kubernetes Secret
type permitted.

After each Helm upgrade, the script checks that every existing migration row is
unchanged and that each new version appears exactly once with an application
time at or after upgrade start. The ledger must reach the target binary’s
reported schema version. A repeat restart must leave every row unchanged.
Snapshots use the target source’s store reader. Comparisons retain integer
nanosecond precision. The script also compares every run ID, commit, sequence,
status, phase and priority across each upgrade or restart, the holder Workflow
UID and running state, all release PriorityClass UIDs and scheduling settings, and the server Pod's class. Every
observed server Pod must be ready with zero container restarts. The fixture
snapshot uses read-only access to the database. Evidence goes under a new
temporary directory by default; set `E2E_EVIDENCE` to choose another directory.

An additional isolated source mutation appends one real migration after the
last candidate’s schema version. The ordinary migration engine applies it to a
published-source fixture database, then reopens that database. The probe checks
that the new row appears once, existing rows retain exact timestamps, the target
reader accepts it, and the published reader refuses the newer schema. Evidence
is named `migration-probe-*`. This source mutation is never used by the live
candidate images and does not qualify a release containing that migration.

The observation window is bounded by the rehearsal and does not prove long-term
crash freedom. Full suites remain pending CI.

If a host wrapper suspends an in-flight fixture wait past its watch deadline,
the runner performs a fresh status read and accepts only `Succeeded`. Failed,
pending or unreadable fixture Pods still fail; no completion assertion is
waived and no host guard is disabled.

## Rollback rehearsal (`--rehearse-rollback`)

```bash
bash hack/upgrade-e2e/run.sh --from 0.20.1 --candidate HEAD --rehearse-rollback
```

Opt-in; without the flag nothing above changes. It takes exactly one
candidate and also needs `openssl`. On the same throwaway k3d cluster it:

1. deploys the published release, seeds the five runs and the suspended
   holder Workflow as above, and upgrades with Helm to the candidate as
   committed (`candidate-1`);
2. builds a second image of the same commit, `rehearsal`, whose archived source
   tree alone carries one throwaway migration
   (`internal/store/migrations/29991231235959-upgrade-e2e-rehearsal.sql`), so
   the next step crosses a real schema migration; nothing is committed to the
   product;
3. runs the product command `oberth upgrade --chart <rehearsal chart> --yes
   --backup-dir <evidence>/rehearsal` with the rehearsal CLI (the local chart is
   unsigned, hence `--chart`; signature verification is covered by the unit
   tests on the published record), under a private `HOME`, so the operator's
   backup key and client configuration are never used;
4. deletes the holder so the upgraded server admits a queued run (work done on
   the new version);
5. runs `oberth upgrade --rollback <record> --yes` and checks that the server
   reports the candidate's version and schema again, the run set is exactly
   the backup's, every restored run is interrupted (the queued ones with the
   reason "restored from a backup; not dispatched again", the holder as
   restored without re-executing it), no Workflow was created for them after
   the rollback began, and the Workflow the upgraded server admitted is gone.

The two chart versions are stamped `v<major>.<minor+1>.0-e2e.a.<sha>` and
`...-e2e.b.<sha>`. The behavioral backfill check and the migration probe of
the default path are skipped. Evidence is under `<evidence>/rehearsal/`.

The rollback target here is a build of the candidate itself: the restored-runs
marker is read by the server the rollback returns to, so rolling back to a
release older than v0.21.0 restores the history and the version but cannot
stop that older server from dispatching the restored queued runs again.
