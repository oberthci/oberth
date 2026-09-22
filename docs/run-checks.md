# Checks a step publishes

A step's exit code says whether the run continues. It cannot say what the step
concluded, so anything a reader needs beyond red or green ends up in a log
nobody opens.

A step writes a check instead: one JSON file per conclusion under the run's own
artifact directory.

```sh
mkdir -p "$OBERTH_ARTIFACTS/checks"
cat > "$OBERTH_ARTIFACTS/checks/commit-judge.json" <<'JSON'
{"name":"commit-judge","verdict":"warn","summary":"the message does not describe the diff (0.38)"}
JSON
```

`oberth run <id>` lists them under the steps, and `-json` carries them as
`checks`:

```
  copy-source  copy-source          passed    8s
  install      install              passed    10s
  test         test                 passed    8s

  check commit-judge         warn      the message does not describe the diff (0.38)
  check affected-tests       pass      vitest 2 of 11
```

## The contract

| Field | Required | What it is |
|---|---|---|
| `name` | yes | what published this, shown as the check's name |
| `verdict` | yes | `pass`, `warn` or `fail` |
| `summary` | yes | one line a reader can act on |
| `details` | no | any JSON the step wants to keep with the run |

Written at `checks/<anything>.json`, directly under the prefix. A nested
directory or another extension is not a check and is kept as an ordinary
artifact.

## What it does not do

**A verdict does not decide the run.** A `fail` check on a step that exited 0
leaves the run green. The step's own exit code is the only thing that fails a
run, so a check can never turn an advisory judgment into a broken build by
accident. A step that wants to fail says so with its exit code, and publishes a
check to explain why.

## Bounds

At most 32 checks per run and 4 KiB per check. A file over the limit, with an
unknown verdict, or without a name or a summary is skipped: it stays an
artifact, the rest of the run's checks still show, and the run is unaffected.
Checks are read when the run is viewed, so nothing is stored twice and a run
that published none costs nothing.

Both engines deliver `$OBERTH_ARTIFACTS` the same way, so a step that publishes
a check behaves identically on Argo and on the Docker engine.
