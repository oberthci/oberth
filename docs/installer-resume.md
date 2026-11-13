# Resuming installation

## Resume a partial install

Keep the selected context, namespaces and other reviewed install answers unchanged.
If OpenBao was initialized before a failed install, the installer can read its root
token directly from the recovery file without a terminal or an environment variable:

```sh
oberth install --answers answers.yaml --context chosen-context --yes \
  --install-secretstore --resume-secrets-file /private/install-credentials \
  --secrets-file /private/new-install-credentials
```

The recovery input must be an owner-only regular file owned by the current user,
without symbolic or hard links. It is never modified. The output credentials file
must be a new path outside a Git worktree; it receives any newly created credentials.
For both resume and `install --upgrade`, every PKI and secret-store step reads
the root token privately from that file; `BAO_TOKEN` remains an operator option.
An existing `--secrets-file PATH` with `--install-secretstore` or `--upgrade` can supply the
recovery input. It is kept intact; any new credentials go to a separately reserved
`PATH.new-<random>` file, reported only by path and SHA-256. An empty new file is
removed when no credentials were created. Keep both files when a new one is saved.
A recovery file without `OPENBAO_ROOT_TOKEN` reports that missing key and the
accepted input flags; agents must never extract it themselves.

For an age-encrypted recovery file, also pass `--resume-age-identity /private/age-key`.
The installed `age` executable decrypts in memory using the identity file; neither
the token nor the identity is passed on argv or printed. A sealed OpenBao must be
unsealed before resuming. Install handles SIGHUP as cancellation, preserving already
saved credentials. Do not read the recovery file into an agent transcript.

Existing `Host oberth` SSH configuration is updated to the selected SSH port and
uplink key. An unconfigured Claude plugin has no MCP entry; configure it through
`/oberth:connect` after enrollment.

## Keep an already-issued admin certificate

Keep the original `--admin-csr`, `--admin-ssh-public-key` and `--uplink-identity`
answers when resuming after enrollment succeeded but upstream registration failed.
The installer reads `<CSR>.crt` itself. A valid client-auth certificate matching
that CSR's public key and the admin identity is kept unchanged; enrollment
and credential minting are skipped while the remaining install steps continue.
An omitted identity is recovered from the matching certificate. A malformed,
expired, wrong-purpose, mismatched or symlinked certificate stops recovery before
cluster changes. Keep that file and report the error; do not delete or replace it
to force another enrollment. Reuse does not establish successful authentication
against the selected deployment; finish the documented verification/connect checks.

When client routing is the only unfinished step, follow the printed port-forward
with **both** chosen local ports, mapped to the Service's `https` and `ssh` ports.
The names resolve custom Service port numbers too. Run the printed verification
command afterward; HTTPS alone cannot satisfy the SSH verification check.

## Restore forge access

The database retains its upstream registrations, but deploy keys belong to the
installation. Restore prints the current public deploy key: add it with write
access in each restored repository's forge **Settings → Deploy keys**, then run
the printed authenticated `oberth upstream list --check` command.

If the key has not been created, run `oberth upstream add --yes NAME BASE_URL` using the
restored registration's exact name and URL. It creates or recovers the installation's
key and keeps an identical database registration. Preserve `--dedicated-key` for
upstreams configured with dedicated keys. Re-enroll certificate clients against the
new installation's client CA.

## Agent review corrections

Stored priorities use only `low`, `standard`, `promotion` and `high`; migration
rewrites stored earlier class names. `tag_after` checks the current target again on
publication retries and accepts a descendant containing the promoted result. It
refuses a target that no longer contains that result, reports the reason, records
push intent first, and carries the tag request atomically through promotion retries.
Retrying a tagged promotion requires admin authority.

Run-list `ref_glob` uses the pattern's literal prefix to bound an indexed search;
patterns beginning with a wildcard have no literal range to narrow. `latest` keeps
the newest matching attempt in the caller's upstream scope. After adopting a
workflow, a new retry attempt replays its own output from the start when an earlier
attempt has a durable cursor, preserving the script's FAIL line for per-step logs.

## Unchanged releases

A resume with `--resume-secrets-file` compares Helm's rendered manifest and
merged effective values with the deployed Oberth release. When both match, it
continues onboarding without creating a Helm revision. A values change, local
chart change or chart version change still applies. A resume keeps this
no-op check even when `--upgrade` is present; a separate install with
`--upgrade` and no recovery input explicitly requests reconciliation. An existing authenticated admin is reused during
resume; later enrollment uses the admin API and never repeats first-admin exec.

The local `kubernetes.json` install record stores the context name, source
kubeconfig path when supplied, API server URL and SHA-256 of the CA bundle. Cluster
commands verify that identity before connecting, including explicit reuse of the
recorded context name. Changing `KUBECONFIG` cannot redirect that recorded name.
TLS server-name and verification settings are also checked. Equivalent embedded
CA bundles and CA files, including symlinks, identify the same cluster. A context using the
system trust store records the digest of an empty explicit CA bundle; TLS still
uses system trust. An explicit different context selects another target.
`scripts/setup-secretstore.sh --context NAME --kubeconfig PATH` uses the same
selection and checks; without those flags it uses the install record, never the
ambient current-context. Its external TokenReview setup requires an explicit CA
bundle in the selected kubeconfig.

Keep `installer-pending-*.json` in the owner-only Oberth configuration directory
for each Kubernetes target. Each contains the locally generated private installer
key and CSR saved before the first-admin exec. An interrupted installation reuses
that key to retrieve fresh credentials rather than creating another administrator.
No bootstrap token is stored in a Kubernetes Secret.

Losing a still-valid admin credential does not enable first-admin recovery with
a new key. Until that credential expires or is revoked it remains active; an
unexpiring bearer remains active indefinitely. Keep the original local installer
key so an interrupted response can be retried with the same enrollment.
