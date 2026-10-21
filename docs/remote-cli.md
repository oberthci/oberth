# Reading Oberth from a terminal

Every other `oberth` command opens the server's database and git cache directly,
so it only works inside the pod. These read the HTTP API, so they work from the
machine you pushed from.

## Configuration

`oberth install` offers to write this on an interactive install, right after it
registers the uplink, into `~/.config/oberth/env`. Source it, or copy the three
variables into your shell profile:

```
. ~/.config/oberth/env
```

The rest of this section is what that file contains and why.

| Variable | Meaning |
|---|---|
| `OBERTH_BASE_URL` | The server's address. Setting it is what makes a command read the server |
| `OBERTH_TOKEN` | An uplink bearer token |
| `OBERTH_TOKEN_COMMAND` | A command whose standard output is the token, used when `OBERTH_TOKEN` is unset |
| `OBERTH_CA_CERT` | PEM of the authority that signed the server's certificate |

A token of an upstream-scoped uplink reads only its upstreams' runs, issues
and repositories; see [Limit an uplink to some upstreams](mcp-setup.md#limit-an-uplink-to-some-upstreams).

Oberth never writes a token to disk. There is no config file and no
`oberth login`. `OBERTH_TOKEN_COMMAND` exists so the token can come from a
password manager or the system keychain without ever landing in a file:

```
export OBERTH_TOKEN_COMMAND='op read "op://vault/oberth/credential"'
```

The one exception is an explicit choice. On a host with no keychain,
`secret-tool` or `pass`, the client files `oberth install` writes read the
token from `~/.config/oberth/token`, and the credentials screen of
`oberth setup` offers **Save to file**: pressing it writes the bearer token
to exactly that file (mode 0600; a different token already there is kept as
`token.bak-<time>`). Nothing writes it unless you press it.

## Commands

```
oberth runs [--repo NAME] [--ref REF] [--limit N] [--before CURSOR] [--all]
oberth run <run-id>
oberth run retry <run-id> [--json]
oberth run rerun <run-id> [--reason TEXT] [--force] [--json]
oberth run cancel <run-id> [--reason TEXT] [--force] [--json]
oberth promotion retry <promotion-id> [--json]
oberth runs cancel --repo NAME --ref BRANCH [--reason TEXT] [--force] [--json]
oberth admission [--json]
oberth admission release <run-id> [--force] [--json]
oberth log <run-id> --burn B --step S [--pattern P] [--context N] [--offset N] [--limit N] [--tail]
oberth artifacts <run-id> [name]
oberth repos
oberth issues
oberth status
```

The list and detail commands are read-only. `oberth run retry` calls the
`run_retry` MCP tool using the same uplink token; it queues a fresh attempt
for an interrupted or infrastructure-failed branch run and records the caller
as actor. Workflow-level failures without a failed step are eligible. The
source must still be the newest run for its branch; test failures, older runs,
and release runs cannot be retried this way.

`oberth issues` prints the first page and its full matching count, such as
`1–50 of 93`. The dashboard shows the same page range and state/kind counts.

`oberth run rerun` calls the `run_rerun` MCP tool with the same uplink
token. It reruns a failed or interrupted run at the same SHA and ref without
a push, for the cases `run retry` refuses: a failed test step, a run that is
no longer the newest of its branch (as long as the branch head is still that
SHA), and tag/release runs. The new run is linked to the original (`oberth
run` prints `rerun of`), and `status`, `wait` and `promote` on the SHA follow
it. A non-admin uplink may rerun branch-push runs; a tag/release rerun needs
an admin and `--force`, and a tag that is already published or green is
refused. When every step passed and only the upstream push failed, the push
is retried instead of re-running the pipeline.

`oberth promotion retry` calls the `promote_retry` MCP tool with the same
uplink token. A failed or interrupted promotion (the push failed, the target
moved, its CI hit an infrastructure failure, or a restart interrupted it) is
re-run as a new promotion of the same repository, source SHA and target:
merged onto the target's current head and re-tested, so nothing is ever
pushed that was not tested against the head it lands on. It needs the same
authority as `promote`; for a non-admin uplink the original promotion grant
still counts, because a grant is consumed only when its promotion is
delivered. Follow the new ID with `promote_status`. When delivery finds the
target moved, the server already retries on its own (at most twice in a
row), and `promote_status` on the failed promotion names the retry in
`retried_as`.
`oberth runs` and `oberth run` use one pre-admission label, `queued`, with
the scheduling reason. The default list puts the whole queue first, then
admitted and terminal runs by admission time, newest first. A trusted
preflight is `running`. Supersession, explicit cancellation and restart all
display as `cancelled`, with a reason naming the newer SHA, actor or retry
action. `--json` keeps the underlying `Status` field and adds
`display_status` and `display_reason`. Successful releases display `passed`;
`release` is a separate kind. The `status`/`wait` MCP tool's legacy
`status: "delivered"` remains an alias of `passed`, alongside
`display_status: "passed"`.

The limit counts admitted/terminal rows; queued rows are additional on page 1.
`--all` follows the server cursor through dated pages. If a cursor run was
pruned, restart from the first page. Queue positions are global even when a
repository or ref filter is used, and withheld for scoped uplinks. The deprecated
`oberth runs --active` compatibility view stays unpaged and uses the same server
queue positions. Elapsed time begins at the weighted grant, includes preflight,
and resets at re-admission after a VM-capacity park. Interrupted results neither
open nor resolve CI-red issues.

`oberth run cancel` calls the `run_cancel` MCP tool with the same uplink
token. It cancels one queued, waiting or running run: the run becomes
`interrupted` with phase `cancelled` (shown as `cancelled` by `oberth runs`
and `oberth run`) and a reason naming the caller; a claimed run's Workflow is
deleted and its admission capacity released immediately; it is never re-fired
and opens no CI issue. A non-admin uplink may cancel only branch-push runs
outside a concurrency group in repositories it can see. An admin may cancel
any run, but a tag/release or apply run needs `--force`, because anything its
Workflow already published stays published. Cancelling promotion CI fails the
promotion without merging. A terminal run is refused. `oberth runs cancel`
cancels every active run of one ref the same way, reporting each refusal.

`oberth admission` prints the server's admission gate from `system_status`:
the weight budget and its use, the queue-head run every later run waits
behind, and every reservation with its state (`preparing`, `waiting`,
`group`, `vm`, `granted`, `retained`), size, weight, repository/ref, group and
age. `oberth admission release <run-id>` calls the admin-only
`admission_release` MCP tool to free a reservation no owner will release,
typically a granted one still held for a run that is already terminal. It
deletes the run's Workflow first as proof; `--force` releases without that
proof (a failed deletion, or a run in a trusted preflight that has no recorded
Workflow), and the audit records it unverified. A live run's reservation is
refused: cancel the run instead. The gate lives in the server process, so both
are remote-only.

Inside the server pod, pass `--database /data/oberth.sqlite` instead of the
MCP flags: the command then passes the live daemon's audit mutation gate,
records the same audited cancel as the administrative actor, and wakes the
daemon's cancellation pass over its private admin socket.
Publishing stays in the dashboard and MCP.

`--json` on any of them emits the server's own payload unchanged, so a script
never parses a rendered table.

`oberth log` accepts the same five filters as the `logs` MCP tool and prints the
same counts, on stderr. A step log can exceed a context window, so prefer a
pattern over retrieving the whole thing:

```
oberth log run-abc --burn ci --step test --pattern 'FAIL|Error' --context 3
```

The `[burn/step] ` prefix is stripped from each line, since the step is already
named in the command. Argo executor lifecycle lines (`time=… level=INFO …
argo=true`, such as "waiting for signals" or "sub-process exited"
error=<nil>) are hidden and counted on stderr; a warning, an error or a
non-nil exit status is always shown. `--raw` keeps the prefix and every
executor line, and `--json` returns the server's payload unchanged. The
dashboard folds the same lines behind a "▸ N Argo executor lines" toggle.

## Local or remote

`artifacts` reads either. **If `OBERTH_BASE_URL` is set it reads the server;
otherwise it reads the local store**, which is the only thing that works inside
the pod. Every command that can go either way prints which it used:

```
reading: server
reading: local store
```

That line goes to stderr, so it never contaminates output you pipe or redirect.

## Certificates

TLS verification is always on and there is no flag to disable it. A CI client
that can be told to trust anything is a CI client that will be.

Two failures look similar and need different fixes, so the client names which
one happened.

**Unknown authority** means the signer is not in this machine's trust store.
Point `OBERTH_CA_CERT` at the PEM that signed the server's certificate.

**A hostname mismatch** means the certificate does not cover the address you
used, and no trust anchor can fix that. The client lists the names the
certificate does carry:

```
client: the server's certificate does not cover "localhost"; it is issued for
oberth, oberth.oberth, oberth.oberth.svc, oberth.oberth.svc.cluster.local
```

The chart issues a certificate for the four in-cluster names, plus whatever the
deployment adds with `tls.extraDNSNames` and `tls.extraIPs`
(`oberth install --tls-extra-dns-name`, `--tls-extra-ip`). A macOS kind install
names `localhost` and `127.0.0.1` by itself, since its NodePorts are bound to
the loopback interface and both are therefore true of it.

So a developer who hits this is looking at a deployment that has not named the
address they are using. Either reach it by a name the certificate carries, or
have whoever runs the server add the one they expect people to use. This is a
deployment decision, not something the client should route around, and it is
why the names are a chart value rather than a client flag.

Changing those values on an existing release has no effect on its own: the TLS
Secret is generated once and kept, so the certificate is re-issued only after
that Secret is deleted. Re-issuing does not invalidate an uplink, which is keyed
by SSH public-key fingerprint, but it does change the TLS fingerprint people
compared out of band.
