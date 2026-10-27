This is the production Go source, go.mod/go.sum and license from
`github.com/go-git/go-git/v5 v5.19.3`, upstream tag object
`bdae57cc427f94a6f7c627201bfe14508b5e19bd`. Upstream tests, examples' non-Go
assets and CI metadata are omitted. UPSTREAM.sha256 records the original
included bytes. The root module's existing go.sum retains its upstream module
checksums. This local replacement is temporary; replace it with a reviewed,
fixed upstream version once available.

The only production difference is in plumbing/format/packfile/patch_delta.go:

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

The vendored module has no tests. Decoder and provenance coverage lives in the
parent module's internal/gitcache tests; run it from the Oberth repository root
with `go test -race ./internal/gitcache` or `hack/test-archive-history.sh`:
TestStreamingDeltaBackwardCopies compares the streaming and eager decoders;
TestStreamingDeltaBackwardCopiesReopenFailure makes the second base Reader call
return an error and checks the error/reader lifecycle without a server panic.
TestGoGitPatchProvenance checks original bytes after reversing only the reviewed
patches and refuses every unlisted file, except this document and UPSTREAM.sha256;
the manifest's own SHA-256 is pinned in that test so its file set is reviewed too.
TestArchiveRealHistory uses Cache.ArchiveTar on every commit of the committed
real Kubernetes gengo multi-pack fixture. The diagnostic script's
--original-decoder option reverts the position correction in a build overlay
and disables Oberth's new decoded-object hash guard to reproduce the released
behavior without editing either the repository or input mirror.

Upstream license: LICENSE (Apache-2.0).
