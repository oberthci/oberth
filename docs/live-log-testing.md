# Live-log streaming checks

The existing branch `test-rest` and release `release-test` Go race suites include
`internal/api`. They run the live-log SSE server tests without Node or a browser:

```sh
go test -race -count=1 ./internal/api -run TestLiveLogStream
```

These checks cover initial authentication and cursor validation, immediate flush,
heartbeats, write deadlines, cancellation, terminal tails, idle stat hints and
bounded credential revalidation. `TestLiveLogStreamDroppedConnectionResumesExactCursor`
uses real authenticated HTTP connections: it disconnects after a UTF-8 chunk,
resumes by both polling and SSE from the exact delivered byte cursor, and checks
the final unterminated tail and terminal flag without duplicate bytes.

The Playwright suite in `hack/test-live-log-stream.mjs` remains an optional
browser check. The existing Go test images (`golang:1.26.10-trixie`) and setup
leaves do not provision Playwright or Chromium and its shared-library/font
runtime. The Go test leaves provision Node for the function-level checks below;
this does not provide a browser. The Playwright suite requires a separately
pinned browser runtime and dependencies.

On a developer machine with Playwright and its Chromium installed:

```sh
PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node hack/test-live-log-stream.mjs
```

The browser suite additionally checks DOM identity, animation-frame batching,
scroll preservation, row eviction, navigation and sign-out. The Go tests do not
establish those browser-layout behaviors.

`TestDashboardLiveLogBehavior` runs both `hack/test-live-logs.mjs` and
`hack/test-dashboard-runs.mjs` against the shipped dashboard functions. Missing
Node is a test failure, including on developer machines. These scripts use only
Node built-ins: no npm install, Playwright or browser is needed.

Branch `test-rest` and release `release-test` use `hack/with-test-node.sh`
(Linux/x86_64, matching the committed Node pin). It verifies both the
archive and extracted binary against `.oberth/pins/node*.sha256` before execution.
Branch CI caches only the archive under `GOCACHE/oberth-test-node`, so warm runs
need no Node download. A private copy is verified on every run; missing/corrupt
cache entries are downloaded again with bounded curl retries. With no persistent
Go cache, the archive cache lasts only as long as that leaf. Release tests reuse
the already fetched Node archive through a read-only mount, without another
download. Extraction and execution use Pod-private scratch, removed on exit.
Setup/publisher tools, other test leaves and the workflow DAG are unchanged.
Both Go suites keep `-race` and `-count=1`, so this gate runs on every CI attempt.
