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

## Restore forge access

The database retains its upstream registrations, but deploy keys belong to the
installation. Restore prints the current public deploy key: add it with write
access in each restored repository's forge **Settings → Deploy keys**, then run
the printed context-specific `oberth upstream list --check` command.

If the key has not been created, run `oberth upstream add NAME BASE_URL` using the
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
