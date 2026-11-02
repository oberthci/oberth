# Argo secret delivery

Credentialed steps use the server-injected `/run/oberth/bin/oberth`:

```yaml
command: ["/run/oberth/bin/oberth"]
args:
  - secretstore
  - exec
  - --dir=/run/oberth-secrets
  - --path=oberth/data/release/signing-key
  - --
  - ./.oberth/release.sh
```

Declare each path in `oberth.ci/secret-paths` and grant it through the
administrator-managed approval table and OpenBao role. Admission verifies
step paths against declarations and approvals. Branch credentials use a
separate role limited to the upstream subtree; release grants require a
release trigger.

The wrapper verifies tmpfs, effective-user ownership and directory permissions
before fetching. It logs in with the projected ServiceAccount token, fetches
KV values, revokes its short-lived token, and writes each value to
`$OBERTH_SECRETSTORE_DIR/<last-path-segment>/<key>` with mode 0400.
It removes `VAULT_*`, `BAO_*`, `CONSUL_*` and `OBERTH_VAULT_ROLE` from the child
environment, redacts fetched values from stdout and stderr, and preserves
the child's exit status. Disable disk-backed swap to keep tmpfs memory-only;
`OBERTH_REQUIRE_SWAPLESS` makes detected swap a hard failure.

The server injects a memory-backed emptyDir for the secret root. The admitted
Pod patch removes the Argo wait container's mirrors of that root and the main
release token. Validate the admitted Pod to verify that boundary; source YAML
alone cannot show the controller's mutation.

The envconsul and environment-variable materialize chain is retired.
`oberth secretstore materialize` is no longer accepted. Migrate callers to
`secretstore exec` and read credentials from files. See
[the example release workflow](../examples/go-project/.oberth/release.yaml).

OpenBao policy remains the enforcement boundary for reads. Join the Oberth
admission/fetch evidence with OpenBao audit records when attributing a read;
annotations record intent and do not replace store policy.

The `oberth-argo-` and per-step identity prefixes are reserved for policies and
roles managed by Oberth. Avoid manually provisioning identities under those
prefixes: authoritative sync removes managed names absent from the live grant
table. Sync refuses a ConfigMap fallback or a producer warning before making
changes, because a partial desired set cannot authorize orphan deletion.

Legacy workflows using envconsul remain recognized as secret-store consumers
and receive the token mount. The retired `oberth materialize` command is no
longer supported; new workflows use `oberth secretstore exec`.
