This is the production Go source, go.mod/go.sum and license from
`github.com/go-git/go-git/v5 v5.19.3`, upstream tag object
`bdae57cc427f94a6f7c627201bfe14508b5e19bd`. Upstream tests, examples' non-Go
assets and CI metadata are omitted. UPSTREAM.sha256 records the original
included bytes except go.mod/go.sum, which pin the reviewed dependency update
below. This local replacement is temporary; replace it with a reviewed, fixed
upstream version once available.

The first deviation is in plumbing/format/packfile/patch_delta.go:

- ReaderFromDelta must reset basePos to zero when reopening the base reader
  for a backwards copy. Otherwise a later forward copy discards the wrong
  number of bytes. Output can have the correct length and wrong content.
- Its deferred close must close the current reopened base reader, rather
  than capturing the initial reader and leaking the replacement on return.
  A failed reopen can leave that reader nil; the deferred close checks for nil
  so a read failure returns an error rather than panicking in the goroutine.

Oberth uses LargeObjectThreshold=1 to bound mirror memory, making the lazy
streaming decoder reachable even for ordinary source files. Switching this
threshold off would hide the bug by loading unbounded blobs into memory.

The second deviation is the dependency metadata in go.mod/go.sum:
golang.org/x/net is raised from v0.57.0 to v0.60.0 for CVE-2026-78669
(HIGH, HTTP/2 SETTINGS denial of service). Its minimum dependencies also raise
x/crypto v0.56.0 -> v0.57.0, x/sys v0.47.0 -> v0.48.0, and
x/text v0.41.0 -> v0.42.0. go.sum adds the selected modules' checksums,
including transitive x/term v0.46.0; existing upstream sums are retained.
The root module already selected these versions. Its go mod tidy removes unused
upstream test/module sums and classifies go-billy as a direct dependency.
No other vendored requirements or production source files are changed.

Root go mod tidy omits the replaced upstream module's checksums; retain them
here as provenance for the original v5.19.3 module:

    github.com/go-git/go-git/v5 v5.19.3 h1:qHttbZ+Am7wEjdTpR1XMQ4rl42FK9SIXzZ4o9x7s6M4=
    github.com/go-git/go-git/v5 v5.19.3/go.mod h1:Ye4C8dVigkqrv9UsB4NMdSLV4yAIkLiIhW61gcOyhl4=

The vendored module has no tests. Decoder and provenance coverage lives in the
parent module's internal/gitcache tests; run it from the Oberth repository root
with `go test -race ./internal/gitcache` or `hack/test-archive-history.sh`:
TestStreamingDeltaBackwardCopies compares the streaming and eager decoders;
TestStreamingDeltaBackwardCopiesReopenFailure makes the second base Reader call
return an error and checks the error/reader lifecycle without a server panic.
TestGoGitPatchProvenance checks original source bytes after reversing only the
reviewed decoder patches, and the reviewed go.mod/go.sum hashes directly. It
refuses every unlisted file, except this document and UPSTREAM.sha256;
the manifest's own SHA-256 is pinned in that test so its file set is reviewed too.
TestArchiveRealHistory uses Cache.ArchiveTar on every commit of the committed
real Kubernetes gengo multi-pack fixture. The diagnostic script's
--original-decoder option reverts the position correction in a build overlay
and disables Oberth's new decoded-object hash guard to reproduce the released
behavior without editing either the repository or input mirror.

Upstream license: LICENSE (Apache-2.0).
