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
runtime. The branch setup also does not provision Node. Release website tooling
provisions pinned Node in a separate leaf, which does not provide a browser to
the Go test leaf. The suite cannot run headless with the current provisioned
tools; enabling it requires a separately pinned browser runtime and dependencies.
No CI image or network installer is added for this small hardening batch.

On a developer machine with Playwright and its Chromium installed:

```sh
PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node hack/test-live-log-stream.mjs
```

The browser suite additionally checks DOM identity, animation-frame batching,
scroll preservation, row eviction, navigation and sign-out. The Go tests do not
establish those browser-layout behaviors. `hack/test-live-logs.mjs` checks the
shipped dashboard functions when Node is available; its Go wrapper skips when
Node is absent. The transport tests above are Go-only and do not skip in CI.
