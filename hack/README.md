# Development loop

Install the pinned linter once:

```sh
export GOTMPDIR="$HOME/.cache/go-tmp"
mkdir -p "$GOTMPDIR"
go install -p=2 -buildvcs=false github.com/golangci/golangci-lint/v2/cmd/golangci-lint@"$(cat hack/lint-version)"
```

`make test` runs race-enabled unit tests; `make lint` checks `.golangci.yml`;
`make guards` checks the declared package imports, retired words, spec references,
requirement coverage and unused code. `make check` first validates the portable
plans, then runs tests (including the Python tooling fixtures), lint and guards,
stopping at the first error and printing the failed command.
Lint defaults to the pinned `hack/lint-tool.sh`; `GOLANGCI_LINT` overrides it with an executable matching the pin. `make lint-selftest`
checks budget boundaries and refusals in disposable modules under `GOTMPDIR`.
Tests are exempt from function length only; standard error handling is allowed.
There is no lint baseline.

`make dev` builds incrementally in the development VM, pushes to its local
registry, upgrades next-dev and waits for `/healthz`, printing elapsed times.
To replace the placeholder, change `DEV_MAIN_PATH` from
`./hack/devloop/placeholder` and `DEV_CHART_PATH` from
`hack/devloop/placeholder/chart`; the chart accepts `image` and reports readiness.

Run the merge gate with the pinned linter and the external names policy:

```sh
GOLANGCI_LINT=/path/to/golangci-lint \
OBERTH_TEST_GIT=/path/to/packaged/git \
OBERTH_GUARDS_NAMES_FILE=/path/outside/the/repository/names.txt \
bash hack/gate.sh main
```

`hack/gate.sh <branch>` requires the branch to contain current main and verifies
commit identities/messages against repository Git configuration. It runs main's
complete plans bundle and
Makefile, shell/Python tooling, tool modules, lint configuration and version pin in a detached
VM worktree capped at six CPUs and 7681 MiB. It lists tooling changes on one
verdict line for review, and runs `make lint-selftest` only when tooling changed.
When tooling or plans change, a second detached worktree also runs the branch's
own check and parity. The external names policy is required, existing parity
cannot be removed, and the container has an 80-minute deadline plus kill grace.
Main must contain the tooling first; its initial installation needs review.
Base-image scripts, Python tests and Dockerfile logic come from main. Input locks,
patches and their pins stay from the branch and are checked by main's tests.
Every added path under `hack/`, including renamed files, is listed and removed
from the trusted checkout. The branch check receives those additions.
Added Python files in root packages and added `.pth` files are listed too.
Trusted Python entries and their child
commands use isolated mode, so branch-root modules and Python environment paths
cannot replace the interpreter's runner or imports. Plan helpers load by their
restored file paths; discovery adds the restored test directories.
The product keeps the branch's module files for tests, vet, lint and UI builds.
The gate copies main's module pair outside that checkout and passes it through
`OBERTH_TOOLS_MODFILE`. Guard, names, plan and self-test tool builds use that
explicit `-modfile`, with `GOWORK=off` and readonly dependencies. Standalone
tools use the current checkout's module pair. Lint and deadcode retain their
own tool modules. Module and workspace changes are listed, including
`go.work.sum`. The separate branch check uses its own module files and workspace.
The Go image pinned in `hack/devloop/Dockerfile` must already be present in the
development VM. Python, Git and SSH are included in that image. Missing tools
belong in the VM image bootstrap, rather than an interactive container change.
The names policy is checked on the branch snapshot before the VM starts and
stays outside both checkouts. A temporary container copy supplies both VM check
phases and is removed with the container.
The test Git must match main's architecture-specific qualification hash. Its
container copy supplies `OBERTH_TEST_GIT`; PATH keeps the image's Git and HTTPS
helper for tests that need them.
The root container drops `DAC_OVERRIDE` so filesystem permission tests observe
mode bits; its cache and scratch files remain owned by that container user.
Its private names-policy copy is owned by that user and retains its file mode.

The gate prints wall seconds for resolve, tooling, history, names, prepare,
check, parity and cleanup. Tooling changes also run the timed lint self-test.
`check-and-parity` includes the container phases; do not add both to the total.
Present parity runs its full corpus. Absent parity prints one pending line.
Install and walk each report that they were not run; they need a live environment
and are separate from this source check, whether or not `cmd/oberth` exists.
The branch stays untouched. The container, its private compiler scratch and
host bundles are removed after success or failure. The gate creates no cluster
namespace and builds or pulls no image.

Caches are shared across worktrees. Host defaults remain
`GOCACHE=$HOME/.cache/go-build` and `GOMODCACHE=$HOME/go/pkg/mod`, respecting account
overrides. Scripts default `GOTMPDIR` to `$HOME/.cache/go-tmp` on disk; scratch files
and bundles use it through `TMPDIR`. Go builds use at most two package workers.
Image builds share VM cache IDs `oberth-go-build` and `oberth-go-mod` at
`/go/cache` and `/go/pkg/mod`. Gate containers use VM volumes
`oberth-dev-go-build`, `oberth-dev-go-mod` at the same paths. `/go/tmp` belongs to
the disposable container. `GATE_CACHE_PREFIX` changes the two gate volume names
to `<prefix>-go-build` and `<prefix>-go-mod`; use a fresh prefix for a cold run,
reuse it for a warm run, then remove only those measurement volumes. Build and
module caches persist intentionally; scratch does not. Each cache family
retains its own storage.

Measured product gate, 2026-11-23, on main `b56563d5` and branch snapshot
`d2d3707a`, in the development VM (6 CPUs, 10 GiB). The real gate used six CPUs,
`GOMAXPROCS=6`, Go 1.26.10, linter 2.12.2 and the qualified Git 2.56.0.
Both checks ran the complete race suite with `-count=1`; make was not stubbed.
Build and module volumes were reused after environment preflights. This is a
warm-cache measurement; tests themselves were rerun.

| Phase / observation | Measured |
| --- | ---: |
| Verdict | PASS |
| Resolve / tooling / history, each | 0 s |
| Names | 4 s |
| Prepare | 2 s |
| Trusted check, complete | 1576 s |
| Trusted Go race suite, within check | 1360 s |
| Lint self-test | 5 s |
| Parity, target absent | 0 s |
| Branch check, complete | 1478 s |
| Branch Go race suite, within check | 1265 s |
| Branch parity, target absent | 0 s |
| Container phases including startup | 3061 s |
| Cleanup | 1 s |
| Total | 3068 s |
| Peak container cgroup memory, including file cache | 7.501 GiB |
| Sampled OOM kills | 0 |
| Host one-minute load, range | 4.33–26.22 |
| Host five-minute load, range | 7.92–18.72 |

The chosen CPU limit is **six**, the measured passing configuration, with
matching `GOMAXPROCS`. The outer deadline is **4800 seconds (80 minutes)**:
3061 seconds plus 50% reserve, rounded up to five minutes. This supplies
56.8% headroom over the observed container phases. The passing measurement
used a 7200-second ceiling; its elapsed time is below the final deadline.
Each Go package retains its 20-minute limit. Two such limits total 40 minutes
inside the outer 80-minute budget. The longest measured package was the CLI
at 733 seconds; the installer took at most 678 seconds across the two checks.

The container retains its **7681 MiB** memory cap. Kernel `memory.peak`
includes charged file cache; samples were taken every ten seconds. The run
reclaimed cache at the cap without an OOM kill. Check and Go-suite rows are
nested timings and must not be added together. The container and host scratch
were removed; shared build and module caches remain.

This measures the source gate. The parity target was absent, the browser check
reported its missing-browser skip, and install and walk reported that they were
not run. The complete log is `trains/out/cx-1799-r2.gate.log.txt` beside the task
report outside this repository.

`make ui-check` runs the real embedded UI in an installed headless browser,
including burst bounds, action races, browser lifecycle, authentication and
all twelve visual captures. It uses the test-only example server on an
allocated loopback port; the product binary has no fixture mode or new
runtime dependency. The checker uses the installed script runtime's built-in
WebSocket support. Set `OBERTH_UI_ARTIFACTS` to choose the output directory.

`make check` includes this target's checker. On hosts or merge-gate VMs
without `chromium` on PATH it prints `ui-browser: SKIPPED` with the reason.
The complete browser gate, including compilation, has a hard 60-second limit.
Each page load has a five-second limit; failures identify the stalled step.
Artifacts default to the ignored `.cache/webui/` directory; set
`OBERTH_UI_ARTIFACTS` to choose another destination.
When the browser exists, missing checker tooling or any failed assertion
fails the gate; successful runs print `ui-browser: PASS`. The VM merge log
therefore records whether pixels and browser behavior were exercised. The
browser-check scripts are part of the trusted tooling bundle and must land
on main before that merge gate can use them. No tools are installed by the
check, and no native cluster walk is claimed by the fixture server.

Inventory regeneration also requires an external source-map JSON file, supplied
with `--source-map <file>` or `OBERTH_PARITY_SOURCE_MAP`. It contains the reference
`aliases`, `public_replacements`, `client_paths` and `source_folders`; keep their
source-specific values outside this repository. The portable `--check` needs no
source map. To run the pinned-source regression, supply the source map together
with `OBERTH_PARITY_REFERENCE` and `OBERTH_PARITY_REWRITE`.

Agent-host policy uses `host:fixture-host` in the external names file; unmarked entries
remain unconditional literals. `hack/guards/host_paths.json` scopes host naming to plugins,
the derived root marketplace file, and the `Agent plugins` install section in the named docs.
Contributor credit has no plugin exception; commit and tag metadata also has no host exception.
