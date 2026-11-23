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
OBERTH_GUARDS_NAMES_FILE=/path/outside/the/repository/names.txt \
bash hack/gate.sh main
```

`hack/gate.sh <branch>` requires the branch to contain current main and verifies
commit identities/messages against repository Git configuration. It runs main's
complete plans bundle and
Makefile, shell/Python tooling, tool modules, lint configuration and version pin in a detached
VM worktree capped at two CPUs and 7681 MiB. It lists tooling changes on one
verdict line for review, and runs `make lint-selftest` only when tooling changed.
When tooling or plans change, a second detached worktree also runs the branch's
own check and parity. The external names policy is required, existing parity
cannot be removed, and the container has a 25-minute deadline plus kill grace.
Main must contain the tooling first; its initial installation needs review.
Base-image scripts, Python tests and Dockerfile logic come from main. Input locks,
patches and their pins stay from the branch and are checked by main's tests.
Every added path under `hack/`, including renamed files, is listed and removed
from the trusted checkout. The branch check receives those additions.
The Go image pinned in `hack/devloop/Dockerfile` must already be present in the
development VM. Python, Git and SSH are included in that image. Missing tools
belong in the VM image bootstrap, rather than an interactive container change.
The names policy is checked on the branch snapshot before the VM starts and
stays outside both checkouts. A temporary container copy supplies both VM check
phases and is removed with the container.

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

Measured baseline, 2026-11-16, main `828deeb` product and plans in the
development VM (6 CPUs, 10 GiB), with Go 1.26.10 and linter 2.12.2. The gate
used two CPUs and a temporary 6 GiB measurement cap. Both runs used the
corrected guards from `c0191ea`; the remaining check tooling came from main.
The guard changes distinguish walk definitions from proof and keep successful
module-download diagnostics separate from JSON output.

| Phase / observation | Cold | Warm |
| --- | ---: | ---: |
| Verdict | PASS | PASS |
| Resolve / tooling / history, each | <1 s | <1 s |
| Names | <1 s | 1 s |
| Prepare | 2 s | 1 s |
| Check (plans, race tests, tooling, lint, guards) | 927 s | 199 s |
| Parity (target pending) | <1 s | <1 s |
| Container phases including startup | 928 s | 199 s |
| Cleanup | 0 s | 0 s |
| Total | 930 s | 201 s |
| Peak container cgroup memory | 5.925 GiB | 6.000 GiB |
| Peak sampled reachability process RSS | 4.738 GiB | 4.538 GiB |
| Peak sampled linter process RSS | 2.597 GiB | 2.418 GiB |
| Host one-minute load, range | 6.62–16.82 | 6.25–8.81 |
| Host five-minute load, range | 6.24–12.40 | 8.57–10.36 |

The gate cap is **7681 MiB**: the larger kernel memory peak,
6,442,455,040 bytes, plus 25%, rounded up to a whole MiB. Reachability analysis
(`deadcode -test=true ./...`) dominates process memory; lint is the next
largest process. A separate trial kept the same whole-module analysis but
used one package worker and `GOMAXPROCS=1`: the 3 GiB container was OOM-killed
in 22 seconds. Lower parallelism alone does not make that cap sufficient;
the analyzer retains the module's type and SSA graphs. The gate keeps two
package workers and the measured headroom.

Times are gate wall seconds. The kernel cgroup `memory.peak` includes charged
file cache; process RSS and host load were sampled about once a second.
Each run started below one-minute load 16 and five-minute load 12, with more
than 6 GiB host memory available. Load can rise during a run; the table shows
the observed ranges. Runs were serial. Cold used fresh private build/module
volumes; warm reused those volumes, including valid Go test results. Image
layers and operating-system page caches were warm. Neither successful run
had an OOM event. The temporary cap can reclaim file cache; the final cap
adds headroom above the recorded charge.

Parity's target, install and walk remain pending; these figures measure the
check gate, not live product execution. Main did not require tooling self-tests
for these measurements. Earlier cold attempts retained a store deadline
assertion failure, a successful tool's diagnostics mixed into JSON, and a
requirements-function complexity refusal. The guard fixes have focused
regression coverage; the successful pair includes them. All attempt containers
and gate scratch were removed. The two private measurement volumes are removed
after branch verification. Logs, samples, kernel counters, source identities,
listings and scripts accompany the report at `trains/out/w0-guard-proven/`
outside this repository.

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
