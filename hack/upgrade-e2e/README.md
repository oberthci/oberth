# Oberth local upgrade rehearsal

Run `bash hack/upgrade-e2e/run.sh` from this checkout. Requires Docker, k3d,
Go, kubectl, Helm 3, cosign, jq, curl and ripgrep. The default host check runner
is `$HOME/work/codex-lab/local-check.py`; override its directory with `CODEX_LAB`.
The script creates its own Docker cluster and private kubeconfig, then deletes
both on success or failure. It never selects an existing cluster.

The script downloads the published v0.17.81 chart and signed release record,
checks the public key against its pinned SHA-256, verifies the record's signature,
and verifies the chart digest and server image reference against that record.
Transparency-log verification is disabled for this pinned-key blob signature;
this rehearsal does not establish publication time or transparency inclusion.

It builds candidate cba75ca7 and the `fix/ob-83-rc` head, if present. Override
`CANDIDATE_83` with a commit to repeat an exact trial, or `E2E_REPO` to select the
source repository. Each ref is resolved once and recorded in `sources.txt`.
Candidate images replace only the server executable in the published runtime.
The script refuses that shortcut if the candidate changes any Dockerfile or
static Git substrate input. The imported containerd manifest digest becomes the chart image reference; no
registry upload is required. These images are for local testing only.

A regression assertion must fail on the published source and pass unchanged on
each candidate. Focused store tests also exercise migration replay protection and
queue classification. All builds and checks execute inside `local-check.py`.

The v0.17.81 store API seeds a local Git repository, four queued runs spanning
normal, high and highest priority, and one running branch run bound to a real
Argo Workflow. Its suspension occupies the sole execution slot without running
build containers. The fixture bypasses push authentication and normal Workflow
submission, so it does not test Git push, execution or workflow completion.

OpenBao is required by the published chart. An isolated in-memory development
store supplies newly generated server identities through Kubernetes auth. No
existing tokens, cloud credentials, real upstreams or repository secrets are
used. The CLI uses its token helper entirely inside that pod; the script never
exports its login, reads private identities, or collects identity-store logs.
The chart's development HTTP mode and disabled NetworkPolicy are local test
values. The test does not cover production TLS, networking or OpenBao recovery.
In the Oberth namespace, Helm release metadata is the only Kubernetes Secret
type permitted.

After each Helm upgrade and another server restart, the script compares every
migration version and original application timestamp, run ID, commit, sequence,
status, phase and priority, the Workflow UID and running state, all release
PriorityClass UIDs and scheduling settings, and the server Pod's class. Every
observed server Pod must be ready with zero container restarts. The fixture
snapshot uses read-only access to the database. Evidence goes under
`$HOME/work/codex-lab/handoffs/CX-OB-UPGRADE-E2E/run-<UTC timestamp>` by default;
set `E2E_EVIDENCE` to choose another directory.

This release path currently has schema version 39 on all three sources. It
checks that migrations already applied once retain their original ledger rows;
it does not exercise a newly introduced schema migration. The observation window
is bounded by the rehearsal and does not prove long-term crash freedom. Full
suites remain pending CI.
