# Bootstrap acceptance cell (#1533)

The deterministic contract cell is the automated part of bootstrap acceptance
and remains blocking in branch and release CI. The fresh-machine part is the
[manual release-candidate procedure](../../docs/fresh-machine-acceptance.md),
run on each release candidate before tagging. It uses a fresh macOS or Linux
machine with a working local `~/.kube/config` pointing at the operator's chosen
Kubernetes cluster. Passing the contract cell does not establish installation,
a held run or reboot recovery on that machine; record those results separately.

## Ordinary CI Pod

From the source checkout, the CI entry point is:

```sh
sh hack/with-test-node.sh sh hack/bootstrap-acceptance/rest.sh \
  python3 -I -S -B .oberth/test-packages.py rest
```

`python3 -I -B hack/bootstrap-acceptance/ci.py` also runs the full rest command;
its PATH must already contain the verified test Node runtime.
The full rest command requires Go, Python 3.9+, OpenSSL, Git/OpenSSH, Helm
(for ordinary chart tests) and the plugin's ordinary shell tools. The existing
rest-test Pod supplies them. The contract stages need no cluster credentials,
kubectl installation, VM, Docker socket, nested cluster, privileged Pod or new
ServiceAccount. The Helm and kubectl used by the plan fixture are deliberately
narrow local executables, and all Kubernetes HTTP requests go to a loopback TLS
fixture.

The cell records the existing rest race run once and requires an explicit pass
event for every required (Go package, top-level test) pair. A same-named test in
another package cannot satisfy that requirement, and a later skip cannot replace
the required terminal pass. It does not compile/run the 18 fixtures a second
time. The CLI build and one adverse fixture reuse the same race build flags/cache. Those tests cover:

- The fixed-origin fetcher, real OpenSSL checksum signature verification,
  pinned release key, all supported artifact names/architectures, refusal of
  tampering, and preservation of an earlier verified installation.
- Current answers schema and recovery-file path/mode/output validation;
  multi-file and explicit kubeconfig/context selection.
- Connect pin refusal, CA verification, CA bundle/settings merge and idempotence;
  client certificate enrollment, invite redemption and add-repo context binding.

It also builds the source CLI and runs real `install --plan --json --answers`
against a loopback TLS API with two context names. A CLI context overrides the
answers context; an unknown or ambiguous selection fails. The fixture asserts
that no API mutation occurs. Narrow Helm stubs model an existing release with
stored policy enabled, and then no release. The TLS fixture accepts reads only;
the live probe reaches a scratch namespace POST which receives 403 before its
body is read. Maintenance plans retain an honest unverified warning and stored
choice. The actual fresh plan must exit 1 with the hard `network_policy` check;
no unverified fresh result becomes green. Unexpected mutation attempts still
refuse. This restricted-cluster contract does not claim CNI enforcement: the
ordinary installer race tests cover synthetic enforced/denied/control/recovery
behavior and cleanup. Separate assertions cover mTLS acceptance, wrong
CA/client refusal, redirects, and secret scanning across chunk boundaries.
Plan verification honors the installer's advisory `warning: true` checks:
unknown remote host preparation may warn while aggregate `ok` remains true.
Every non-warning check and aggregate result must still pass; malformed warning
flags, empty checks and duplicate check identities refuse verification.
Failed JSON plans are checked before their nonzero exit is reported, so the
failure names the hard check. An adverse real CLI plan with zero available disk
must exit 1 and identify `disk`; a nonzero command can never become a pass merely
because its JSON result says `ok: true`.

## Captured output boundary

Captured subprocess stdout/stderr and MCP responses stay in an owner-only
scratch directory. The 17 installer contract fixtures export private inventories
through Go's standard test output directory before their keys/temp files vanish;
the remaining public-pin fixture generates no credential. Inventories include
private CA/leaf/signing keys recorded by their helpers, script-generated client
key files and the designated synthetic recovery credentials. The parent scans
both raw/base64 credential values and PEM body lines in captures and decoded
Go event Output strings, including split writes.
An adverse rerun of the CSR fixture deliberately prints its freshly generated
private CA key: its Go test must pass, while the parent scan must reject it.
That adverse capture is deleted and never published. The flag
`-bootstrap-credential-leak-control` belongs only to the installer test binary.

This boundary does not assert complete nondisclosure of every credential in
every rest-suite fixture, or contents retained only in nested Go buffers. It
covers the inventoried contract credentials in captured channels and Python-cell
credentials/keys; a nonempty canary alone is not evidence of that coverage.
Failures identify the originating stage, check name, and available Go package
and top-level test (or Python assertion identity), never captured output, argv,
credentials or exception text. Secret detections also identify the captured
channel. Final scanning cannot replace the originating failure with its own
stage; a secondary scan failure gets its own separate redacted line. There is no public upload
of private logs or inventories.

The existing race command has a 3120-second wall bound (including compilation),
followed by a 120-second bound for the added cell stages and final scan. This
leaves cleanup headroom inside the existing 3300-second rest Pod deadline.
After a failure, final scanning gets an independent 10-second alarm even when
the cell alarm has already expired. An interrupted scan always fails closed;
it never releases captured artifacts or converts the original failure to a pass.
Child environments retain GOMEMLIMIT, OBERTH_TEST_TIMEOUT_SCALE, GOPATH and
GIT_TERMINAL_PROMPT as well as the existing Go/cache controls. The cell prints
race_seconds and added_cell_seconds separately. A cold release still needs the
ordinary module-download network access; publisher isolation is unchanged.

Both `.oberth/build.yaml`'s `test-rest` and `.oberth/release.yaml`'s
`release-test-rest` run `rest.sh`, which must pass this cell using the ordinary
race-test rest group. They retain their existing CPU reservation (1000m), bounded
CPU/memory/storage limits and token/workspace isolation. Release build depends on
the successful release-test DAG; failure here prevents publication/finalization.
The cell does not fetch or execute unsigned candidate binaries: candidate CLI
planning uses the source build; fetch verification uses private signed fixtures.

## Fresh-machine acceptance (manual)

Follow [fresh-machine acceptance](../../docs/fresh-machine-acceptance.md) on
each release candidate before tagging, and retain its sanitized report with the
candidate's release evidence. A macOS VM with user-provisioned colima k3s is one
example; a Linux host with any reachable cluster is equally valid. The operator
supplies dependencies and the working kubeconfig. Oberth installs neither its
host dependencies nor a cluster, and must never write the user's kubeconfig.

The procedure covers the first-time plugin/install/connect path, a smoke run,
an exact-SHA held-run patch/continue round trip with the assisted “not evidence”
classification, cluster/host restart and OpenBao unseal, install re-run,
kubeconfig comparison, captured-output secret scanning, and cleanup. Run the
recovery portion with production-mode OpenBao and persistent cluster storage.
The report distinguishes environment blockers, product findings, operator
steps, and unperformed checks. A missing manual result leaves that candidate's
fresh-machine acceptance incomplete even if CI passes.

The manual procedure supersedes the unused `fresh_machine.py` root/systemd VM
harness and its private tool-provisioning/round-trip helpers and pins, which have
been removed. It needs no VM runner or additional CI wiring. The existing
contract-cell entry points and release dependencies remain in place.
