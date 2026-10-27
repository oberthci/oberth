`gengo-multipack.tar.gz` is the complete reachable public Git history of
https://github.com/kubernetes/gengo, mirrored on 2026-10-27. It contains 878
commits, all refs from that snapshot, their original object IDs and three
packs. HEAD is 25e2208e0dc371a827289e7faced19a2dbcd480b. No commit was
rewritten or synthesized; no shallow boundary, alternate or network access is
needed. GENGO-LICENSE is the upstream Apache-2.0 license.

The mirror was repacked with `git repack -a -d --max-pack-size=1m` using Git
2.54.0 (Apple Git-157), and reduced to HEAD, packed-refs, refs, config and pack/index files.
The deterministic tar sets mtimes/owners to zero, permissions to 0644/0755,
and gzip mtime to zero. Its SHA-256 is
`f6aa30b3c2a8b2b291773eba1bab53ae29ee3471bf4a5877256caecaf7c53fc7`.
The test checks this digest, three packs, the exact commit count and Git fsck
before comparing every commit. The archive holds source/object data, not hooks
or executables. Tests never execute code from the fixture.

With the v0.17.57 decoder, commit
483edb0165750b273a453b5d5de61183091fda14 first differs at byte 1120 of
v2/parser/parse_test.go. Blob e60cadd5eba759f211d8bdbaf3c2ffee6f32a6d3 is a
delta on 08357dca09a4d65ff57783d8bc108cc3ba83936a in
pack-08fe0e5bbc719aa34fdec11f0928cb2a1a225af7.pack. The same commit also
regresses worktree checkout, which is the input to production source seeding.

Run `hack/test-archive-history.sh` for the offline race-enabled CI gate.
For an external full mirror set OBERTH_ARCHIVE_PARITY_REPO to its absolute
path and run TestArchiveRealHistory; no commit sampling or limit is applied.
