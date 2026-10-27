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

Oberth uses LargeObjectThreshold=1 to bound mirror memory, making the lazy
streaming decoder reachable even for ordinary source files. Switching this
threshold off would hide the bug by loading unbounded blobs into memory.

Regression coverage lives in the parent module's internal/gitcache tests:
TestStreamingDeltaBackwardCopies compares the streaming and eager decoders;
TestArchiveRealHistory uses Cache.ArchiveTar on every commit of the committed
real Kubernetes gengo multi-pack fixture. The diagnostic script's
--original-decoder option reverts the position correction in a build overlay
and disables Oberth's new decoded-object hash guard to reproduce the released
behavior without editing either the repository or input mirror.

Upstream license: LICENSE (Apache-2.0).
