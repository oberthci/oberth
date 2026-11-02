# Production reachability gate

`make deadcode` runs `golang.org/x/tools/cmd/deadcode@v0.49.0` against every
main in `./...`, including the maintenance/build tools in `hack`. Install that
exact tool version into a local tool directory before running the target.
The required lint stages in both Oberth workflows run the same check.

Analysis uses CGO disabled, matching released Go binaries, on the Linux
platform running the gate. Cross-platform builds remain separate CI checks.
Each unreachable function must have an exact, reasoned entry in
`hack/deadcode-allowlist.json`. The checker rejects unlisted functions and
stale entries, so deleting a function also requires deleting its exception.

The current baseline retains 106 VM functions assigned to issues #1293 and
#1294. Remove those exact entries as those removals land. Other exact entries cover the shared test fixture packages (including
`internal/storetest`), MCP schema inventory used by cross-package tests,
and the instance recovery helper assigned to #1292. The gate does not permit
additional functions merely because they live in one of those packages.

Release analysis tooling is installed into the deadcode test-tools copy.
The release gate receives that copy read-only and cannot write or mount the
trusted publisher tools. The existing isolation tests verify this boundary.

The tool version is pinned in both workflows. Its module and transitive inputs
are verified through Go module sums and the Go checksum database, matching the
other `go install` tools in the build inventory.
