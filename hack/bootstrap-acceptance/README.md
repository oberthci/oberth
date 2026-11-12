# Bootstrap acceptance cell (#1533)

The deterministic contract cell is blocking in branch and release CI. Full
fresh-machine acceptance remains unqualified until a disposable VM runner can
execute the separate machine cell. Passing the contract cell is not evidence
that installation, a held run or reboot recovery succeeded on a real machine.

## Ordinary CI Pod

From the source checkout, the CI entry point is:

```sh
sh hack/with-test-node.sh sh hack/bootstrap-acceptance/rest.sh \
  python3 -I -S -B .oberth/test-packages.py rest
```

`python3 -I -B hack/bootstrap-acceptance/ci.py` also runs the full rest command;
its PATH must already contain the verified test Node runtime.
It requires Go, Python 3.9+, OpenSSL, Git and the plugin's ordinary shell tools.
The existing rest-test Pod supplies them; no Helm, kubectl, VM, Docker socket,
cluster credentials, nested cluster, privileged Pod or new ServiceAccount is
required. The Helm and kubectl used by the plan fixture are deliberately narrow
local executables, and all Kubernetes HTTP requests go to a loopback TLS fixture.

The cell records the existing rest race run once and requires an explicit pass
event for every named fixture. It does not compile/run the 18 fixtures a second
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
that no API mutation occurs. Separate assertions cover mTLS acceptance, wrong
CA/client refusal, redirects, unsafe Helm archive members, exact pushed SHA,
assisted non-evidence classification, and secret scanning across chunk boundaries.

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
Failures identify the stage and available top-level Go or Python test identity, never
captured output, argv, credentials or exception text. There is no public upload
of private logs or inventories.

The existing race command has a 3120-second wall bound (including compilation),
followed by a 120-second bound for the added cell stages and final scan. This
leaves cleanup headroom inside the existing 3300-second rest Pod deadline.
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

## Fresh Linux VM (outside CI)

The entry point is `fresh_machine.py`; it is never called by either CI workflow.
It runs unchanged on a disposable amd64/arm64 systemd Linux VM with working
cgroups, container networking and persistent local disk. The runner supplies root,
Python 3.9+, CA roots, Git/git-shell, OpenSSL, OpenSSH client/server tools, useradd/passwd and ordinary
system utilities. Start without any `KUBECONFIG`, user kubeconfig, k3s installation
or prior cell state. Existing k3s configuration, drop-ins, data, service units,
environment files and enable links are refused before mutation, including the
resume and forge units. Every unit is exclusively created, refusing files and
symlinks. K3s gets explicit config/data/kubeconfig paths under the new private
state directory instead of the host's default configuration or data. See the
[K3s configuration file contract](https://docs.k3s.io/installation/configuration#configuration-file)
and [server options](https://docs.k3s.io/cli/server). Allocate at least 4 CPUs, 12 GiB memory and 40 GiB free disk;
these are provisional fixture prerequisites, not measured acceptance sizing.
Allow outbound HTTPS for the pinned tools, signed Oberth release, charts and
container images. The guest must permit k3s/sshd systemd services and a real reboot.

```sh
sudo python3 -I -B hack/bootstrap-acceptance/fresh_machine.py \
  --version v0.21.9 --confirm-disposable-host
```

`--state-dir /absolute/new/path` selects a new private state directory outside
the checkout; it must not exist; use only ASCII letters/digits, slash, dot, underscore and
hyphen in its path (no unit specifiers or whitespace).
`--answers /absolute/answers.json` optionally supplies nonsecret version-1 JSON
answers for resources and other installation settings. The cell fixes its own
context, kubeconfig, localhost DNS endpoint, production OpenBao, assist setting,
throwaway upstream and public host-key pin. Private material is generated locally;
never supply production credentials or an existing deployment's answers.

The cell installs k3s v1.34.3+k3s1 and Helm v3.19.0 from their official HTTPS
release origins, checking committed per-architecture SHA-256 pins in `pins.json`
before installation. It reads only the expected regular Helm archive member.
k3s supplies kubectl; Helm is installed in the private cell bin directory before
the plugin installer runs. Helm and kubectl remain prerequisites of Oberth itself;
this disposable-host harness owns their provisioning, not the installer.
Pins come from the versioned upstream checksum files, not a network checksum
accepted alongside the executable at runtime.

On that VM the script:

1. Installs a new single-node k3s with its kubeconfig written directly into private
   state and pins the `bootstrap-cell` context for every cluster command.
2. Starts a separate sshd with a generated host key and one restricted Git work
   key, serving a new bare fixture repository through a new fixture-only account
   (password login disabled, including on hosts whose root account is locked). It leaves the VM's normal SSH
   service and authorized_keys unchanged. No hosted forge account is needed.
3. Uses the verified plugin fetcher, plugin keygen, current installer answers,
   `--admin-csr` and public SSH key; plans, installs and requires every Verify
   check to pass. Connect pins/merges the CA and installs certificate settings;
   MCP uses that CA and client certificate, without a bearer fallback.
4. Registers the throwaway repository, configures its Oberth remote with the
   current add-repo/client binding, opens events_wait before the assisted push,
   binds the held run to the exact pushed SHA, patches the failed file, continues
   it and requires passed + assisted + the existing “not evidence” reason.
5. Captures private server/Pod/process logs and scans them. It records the boot
   ID, installs a one-shot resume unit and reboots the actual VM.
6. The resume unit requires a changed boot ID, independently checks initialized
   and sealed OpenBao, unseals through the current verified CLI, checks unsealed,
   repeats all-ok install Verify and mTLS status, rechecks the same assisted run
   and SHA in retained history, then captures/scans logs again.

The source/plugin are copied into root-only state so the resume command needs no
checkout mount after reboot. Command output, recovery material and private keys
remain under `/var/lib/oberth-bootstrap-cell` (mode 0700); files are owner-only.
A completed run writes a public-only `PASSED.json` there after scanning. Reboot
interrupts the initiating SSH session; the runner must reconnect and require the
resume unit's success and that receipt. On failure no receipt is written. Raw
state must never be uploaded: it contains recovery material. Destroy the VM and
its disks after either result, including failure before/after reboot. No automatic
wipe of another host or an existing installation is attempted.

## Runner work still required

CI cannot yet allocate this VM or receive a trustworthy post-reboot result. An
external runner must provision the fresh guest and host prerequisites, stage the
exact candidate checkout and a matching signed immutable release, run the command,
wait across the real reboot, retrieve only a successful receipt through an
authenticated channel and attest its candidate/VM identity, then destroy the guest
and all private storage. A release DAG dependency on that authenticated receipt
must be added only when the runner exists. A local JSON file alone is not an
authenticated release acceptance receipt. No live installation, push or reboot was
performed when adding these scripts, and no complete VM acceptance is claimed.
