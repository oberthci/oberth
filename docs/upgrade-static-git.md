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
v0.17.59 guide) no longer apply. `oberth upgrade` strips either boolean value
of an already stored `gitcache.shadow`, reports the migration, and applies the
same cleaned snapshot it schema-validates; the saved Helm revision is unchanged
for rollback. An explicit new `shadow: true` override, malformed value,
unsupported backend or unrelated unknown key still fails schema validation.
The v0.17.63 CLI predates this migration: with that CLI, use an explicitly
cleaned values file and `helm upgrade --reset-values --values CLEANED_FILE`,
preserving compatibility identities and the original values for rollback.
Do not use the cleaned snapshot as rollback configuration.

Standalone serve/admin-gc installations require the executable alias
`oberth-git-ssh -> oberth` adjacent to the installed binary. The container ships
it. Existing hooks are replaced atomically on cache preparation. For rollback,
keep the old published CLI/chart/image pins and verify cache preparation occurs
before accepting pushes, so the old release rewrites native hook aliases.

See [Git runtime and required qualification](gitcache.md) for build inputs,
component security scans, transport policy and CI coverage.
