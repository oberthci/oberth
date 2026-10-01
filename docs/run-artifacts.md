# Run artifacts

A run can keep files that outlive it: test reports, coverage output, generated
diffs. Before this, nothing survived a run except its log.

## Producing one

Every step gets `OBERTH_ARTIFACTS`, a writable directory on the run's own
volume. Copy anything worth keeping into it. There is no new pipeline syntax and
nothing for the admission gate to review.

```yaml
- name: test
  container:
    command: [/bin/sh, -c]
    args:
      - ./mvnw test; cp -r target/surefire-reports "$OBERTH_ARTIFACTS/"
```

Argo's own `outputs.artifacts` stays refused. It resolves credentials from a
Kubernetes Secret, and a repository-authored document must never reach one.

## Reading them

```
oberth artifacts <run-id>            # what a run kept, with sizes
oberth artifacts <run-id> <name>     # one artifact to stdout
```

Over HTTP, `GET /api/runs/{id}/artifacts` lists and
`GET /api/runs/{id}/artifacts/{name}` downloads. Over MCP, `artifacts` lists and
`artifact_get` returns contents, taking the same `pattern`, `context`, `offset`,
`limit` and `tail` as `logs` and reporting the same counts. A 3000 line report
with one failure in it is one filtered call, not 24,000 tokens.

The dashboard shows what a run kept beneath its log.

## How collection works

The server mounts a second directory from the run's existing claim, writable,
confined by subPath so a step can write artifacts but cannot reach the checkout,
the trust anchor, or the server binary through it.

When the run reaches a terminal phase the server collects that directory the
same way it seeded the checkout: a short-lived Pod that mounts the claim, holds
no ServiceAccount token, and exists only to be a filesystem the exec stream can
reach. Seeding streams a tar in; collection streams one out.

Collection happens for a failed run as well as a green one. The report that
explains a failure is the main thing worth keeping.

### The deadline

The claim is owned by the Workflow, so Kubernetes deletes it when the Workflow's
TTL expires. Collection runs immediately on completion and finishes in seconds,
but `--argo-workflow-ttl` is a real bound on this feature. A collection that
misses its window is recorded in the audit chain as
`ci.argo.artifacts` with `stored: false` and a reason, never dropped silently.

One gap is known and not yet closed: a run whose in-process state did not
survive a server restart finishes through a different path that has no claim
left to collect from. It is recorded, not lost.

## Limits

| Setting | Default | Meaning |
|---|---|---|
| `--artifacts-limit-bytes` | 256 MiB | Per run. A collection over this is refused whole, never half-stored |
| `--artifacts-budget-bytes` | 4 GiB | Total. Passing it evicts the oldest runs' artifacts first |

Eviction removes artifact bytes only. The run and its log survive.

The per-run limit applies to decompressed bytes, so a small archive that expands
enormously is refused on the way in rather than after it fills the volume.

## What the server refuses

Artifacts arrive as a tar stream produced by the repository's own files, so
extraction treats every member as hostile. The whole archive is judged before
any member is written: absolute paths, `..` traversal, symlinks, hardlinks,
device nodes and anything that is not a regular file are refused by name, and a
refused archive leaves nothing behind.

The same judgment pass scans every member for structural secret material —
PEM private-key headers, which appear verbatim both in raw key files and
embedded inside JSON service-account keys. One match refuses the entire
collection, fail-closed, with the member named and the content never echoed.
The refusal is recorded as the run's artifact failure. A test-fixture key will
trip this gate; that is a bounded, honest false positive, preferred over
persisting real key material to the server's disk.

## Collection is CI-tier only

Only CI-trigger (branch) runs collect artifacts. Credentialed tiers — release,
plan, apply — are excluded: their steps hold secret material under the
memory-only contract, and a credential accidentally written into
`$OBERTH_ARTIFACTS` must never be copied onto the server's `/data` volume.
For those runs the directory still exists and is writable, but its contents
are reclaimed with the run's claim and never collected.

## Redaction does not cover artifacts

Oberth redacts secrets write-side, by wrapping a step's stdout and stderr. A
file written by the step never passes through that writer, so **a secret copied
into an artifact is stored unredacted.**

This is a real limitation, not an oversight. It is why artifacts require the
same authorisation as the run's logs and are never a more public surface than
the run itself, why collection is CI-tier only, and why the extraction pass
refuses structural key material outright. Treat an artifact as you would treat
the working tree of the build that produced it.

## Serving is deliberately inert

A coverage report is HTML the repository wrote. Served inline on the dashboard's
origin it would be stored cross-site scripting against an authenticated session.
Downloads therefore go out as `application/octet-stream` with `nosniff`,
`Content-Disposition: attachment`, and `Content-Security-Policy: default-src
'none'; sandbox`. An artifact is a file you download, never a page the dashboard
renders.
