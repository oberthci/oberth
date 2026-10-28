# Upgrade to the static Git runtime

Git CLI is the only backend. The minimal runtime has no shell, OpenSSH or curl.
Upstream HTTPS registrations are incompatible and now refused explicitly,
including when a cache already exists. Before upgrading, use the old server's
`oberth upstream list` to capture its complete metadata table, then run the new
CLI offline:

```
oberth preflight upstreams --inventory upstreams.txt
```

A refusal names registrations to migrate to `ssh://` with independently verified
host keys. Do not upgrade until the complete inventory passes. Protect the
inventory as operator metadata; never paste upstream URLs or credentials into
issues. This preflight does not access the server or modify registrations.

The chart keeps `gitBackend: cli` and `gitcache.shadow: false` as accepted,
ignored legacy defaults so install reruns, restore and Helm value reuse work.
Old instructions that enable shadow comparison (including the historical
v0.17.59 guide) no longer apply: `go-git` and `shadow: true` are refused. Remove
such settings explicitly before upgrade; preserve other compatibility values.

Standalone serve/admin-gc installations require the executable alias
`oberth-git-ssh -> oberth` adjacent to the installed binary. The container ships
it. Existing hooks are replaced atomically on cache preparation. For rollback,
keep the old published CLI/chart/image pins and verify cache preparation occurs
before accepting pushes, so the old release rewrites native hook aliases.

See [Git runtime and required qualification](gitcache.md) for build inputs,
component security scans, transport policy and CI coverage.
