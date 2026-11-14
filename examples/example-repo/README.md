# A repository for fresh-machine acceptance

Copy the **contents** of one variant into the root of a throwaway repository,
including its hidden `.oberth` directory. Do not copy both variants into one
repository. Follow the [fresh-machine walk](../../docs/fresh-machine-acceptance.md#example-repository-walk)
after installing and connecting to the exact candidate.

| Directory | Target | Validation |
| --- | --- | --- |
| [`v0.22.7/`](v0.22.7/.oberth/build.yaml) | `land/v0.22.7` | Real Decode, Admit and Build for branch, promotion and release tag; three refusal cases |
| [`next/`](next/.oberth/build.yaml) | **DRAFT v0.23.0** | Decodes at the integration snapshot below; full admission and execution are blocked |

The shell project has no downloaded tools or dependencies. `app.sh` prints
`hello from example-repo`. `test.sh` checks it, requires `expect.txt` to contain
`fixed`, and polls a local HTTP sidecar. The sidecar serves `ready` on port 8080.
The optional first argument to `test.sh` selects a loopback fixture URL for the
repository test. Every authored container declares its own resources. v0.22.7 supplies the
security context; the next variant authors the security floor. All images use public digest pins already present in Oberth.

`build.yaml` tests the project, hashes `app.sh` into an Argo output parameter,
passes that parameter to `report`, and finishes with an exit handler. Changing
`cancel.txt` from `no` to `yes` holds the last task and then the exit handler for
600 seconds each, making Stop and Terminate observable. Restore `no` before
promotion. The credential-free build also supports the manual assist check by
changing `expect.txt` to `broken`.

`release.yaml` repeats the tests and parameter handoff on a version tag, then
reads **one** KV v2 secret, `oberth/data/release/example-repo`, granted only to
`read-secret` in `.oberth/release.yaml`. The secret must have a nonempty `marker`
field. The only public result is `secret: marker present`, followed by the tag.
No secret is passed through an output parameter. The example release records
a successful tag run; it does not publish an application to an external service.

In v0.22.7, the granted main invokes the injected client with a static path;
Oberth supplies its login token, client, CA and private scratch. Only that main
gets secret-store settings. Branch and promotion builds need no secret grants.
Run facts are already available as native workflow parameters on this branch.

## DRAFT v0.23.0

The next variant shows the intended cutover:

- Each source-reading leaf declares the exact `oberth` main input artifact at
  `/work/src`, with the server's public locator parameter. It receives its own
  Pod-local source copy. No workflow-level artifact argument is authored.
- Run facts use workflow parameters. No `OBERTH_*` environment is assumed.
- `read-secret` authors its own OpenBao client image, exact-audience projected
  token at `/var/run/secrets/secretstore`, public CA map, and memory scratch at
  `/private`. Its role is its assigned ServiceAccount name, obtained through
  the permitted `spec.serviceAccountName` field reference. The address is the
  `oberth-secretstore-address` parameter; no role parameter is assumed.
- The client script reads the marker into memory scratch, prints only its
  presence, and attempts to revoke its session and remove the private files
  on exit. Its session token is passed to the client process in `BAO_TOKEN`;
  do not capture process environments. It explicitly runs as UID 0 to write the
  authored scratch volume; the no-escalation, drop-ALL and seccomp floor stays.
- A separate, ungranted CI step mounts `example-repo-cache-ci` at `/cache`.
  An administrator must provision it in the pipeline namespace and grant it
  with `oberth access allow --kind pvc --tier ci <repo-key> example-repo-cache-ci`.
  The claim belongs only to this repository and tier. No granted secret step
  mounts it. Release has no cache claim.

The native client uses the documented [OpenBao CLI environment](https://openbao.org/docs/commands/#environment-variables)
and KV v2 command syntax. The token audience `oberth-secretstore` is the managed-store default. For an
external store, use the installation's exact configured audience. The
`oberth-secretstore-ca` ConfigMap must be published in the pipeline namespace.
Scratch and credential mounts stay beside `/work/src`; output parameter files
stay outside authored mounts and credential paths. Trusted Argo helpers can
see native mirrored mounts. Every container in a granted Pod shares its trust.

Validation snapshot: `int/v0.23.0` at
`93b28e0bc9ddc16ea2c48d17d0c4b7f472add0c2`. Both next workflows decode there.
With the matching token audience and public CA admission policy, that code still
refuses source inputs with **declares artifacts** and refuses the authored
**securityContext**; build also refuses the **existing PersistentVolumeClaim**. This stops before Build or submission. The snapshot
contains authored secret-client admission, but not the assembled source and
cache admission cutover. No successful next Build, source delivery, cache mount,
secret read or live run is claimed. Revalidate against the assembled candidate
before using this draft for acceptance; never weaken admission to run it.

The reproducible boundary probe is
[`hack/testdata/example-repo-next/admission_test.go.txt`](../../hack/testdata/example-repo-next/admission_test.go.txt).
Copy it as `example_repo_next_probe_test.go` into `pkg/argoworkflow` in a disposable checkout of that exact integration
commit, copy `next/` to the same example path, then run the queued fast gate
with `TestExampleRepoNextIntegrationBoundary` as the focus. It requires both
recorded refusals; a future change forces the boundary to be updated.

The v0.22.7 proof lives in
[`example_repo_test.go`](../../internal/argojob/example_repo_test.go).
It loads the actual files, admits them and calls the real Build with exact-file,
exact-step grants and a materialized release identity. It checks parameter and
sidecar preservation, authored resources, token defaults, assist eligibility,
and secret-store isolation. Shell checks use a loopback fixture and deliberately
break `expect.txt`; they are not live Kubernetes evidence. The bootstrap cell's
private recorder and final credential scan remain separate acceptance checks.
