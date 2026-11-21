# Operate Oberth

`oberth system status` shows installation readiness; `oberth commit status <ref>` shows CI. The
browser Status screen adds version, run counts and detail. See [installation](install.md) for
upgrades, backups and unseal.
Keep Git policy and rewrites compatible with the bound remote; `oberth repo add` rechecks.

## Read a failure

```sh
oberth run list
oberth run get <run>
oberth log <run> <step>
```
Copy the run ID from push output or a list. Hexadecimal IDs accept unambiguous prefixes of eight or
more characters; copy scheduled run IDs in full. A run shows its trigger, schedule entry, rerun
source, actor and assist request. Read the short reason, then expand Detail for the Pod name, full
message and exit code. Show text whole: status reasons have at most 40 characters; refusals have an
80-character reason and 120-character remedy. Steps appear in plan order
([pending steps](pipelines.md#plans-and-budgets)). Missing duration is not zero. Select a step,
attempt or container for logs; page through incomplete windows and earlier output. A `secret` tag
means declared credential intent, not that a value was read. Runs may be Argo-queued, waiting for a
node, running, passed, failed or cancelled. Holds are live observations of eligible containers.
Passing and publication are separate facts: check publication if a tag has not reached the forge.

## Wait, retry and assist

```sh
oberth run wait <run>
oberth run rerun <run>
```
A wait timeout leaves work running; waiting again is safe. Exit 0 includes failed runs: read the
printed state. For code failures, inspect step logs and fix before pushing again. A rerun creates a
new run, not another attempt. A moved branch may require pushing the intended commit again. Release
reruns require an administrator. Automated retry follows the [agent instructions](agents.md).

Held-step assist is disabled in this release. The following applies only after it is enabled through
review and real-cluster checks; it is not an available recovery path here.
`oberth run hold list <run>` shows held-since and approximate time left, or unknown availability.
Inspect with `run hold exec`, push an authored fix, then use `run hold release`. There is no
in-place retry or file-patch tool. Inspection uses the step's identity and mounts; it can change
files and outputs. Declared, resolved secret routes are excluded, but image contents, fetched values
and shared state are not certified secret-free. Asking for assist, listing or releasing preserves
evidence eligibility. An audited exec marks the run before dispatch: it then supplies no CI,
promotion or publication evidence, even if dispatch fails or the run passes. Never repeat an exec
after an uncertain reply. Read the original container's exit code separately from inspection output.
The helper's deadline bounds its own wait, not collection or finalization; the native run result
remains authoritative. Never print secrets, tokens, environment or credential files.
`oberth run cancel <run>` stops work. To purge retained logs:
`oberth run purge --id <run> --what logs --reason <reason>`. Read state after a lost mutation reply.

## Browser access

Open the push's watch link. Runs, Repositories, Issues and Status have separate views. Sign in with
a bearer for this installation or a certificate already configured in the browser. On its
administrator profile, create a short-lived credential in a new private directory outside Git.
Browser-only registration needs no SSH key. Add `--ssh-public-key` only when also allowing Git
pushes, and choose an unused identity name.
`<upstream>` is the first component of a full repository name; its scope includes every repository
there and is not limited to browser reads. An empty array removes scope restrictions.

```sh
mkdir -m 700 <private-directory>
oberth uplink add --identity browser-reader --scope '["<upstream>"]' \
  --bearer-file <private-directory>/browser.token --ttl 1h --reason 'Browser access'
```
The command writes a new private bearer file and refuses to overwrite one. Keep it and any private
key out of Git, reports and chat. Transfer the bearer privately to its intended user; paste it only
into this installation's Sign in form, never a URL or command argument. The browser retains it for
this origin and sends it in the authorization header. Sign out afterward; use a trusted browser
profile on shared machines. The public shell has no run data. Authenticated reads obey scope;
actions also require suitable state and server authority. The bearer expires in an hour. To end
access earlier, revoke the same identity:

```sh
oberth uplink revoke browser-reader --reason 'End browser access'
```
Revocation cannot retract copied data. Issue bodies are plain text.

## Grants and secrets

Use `oberth access list` before changing grants: it identifies repository, file, step and secret
path, not stored values or proof of a read. Follow
[secret writes](pipelines.md#secrets-and-identity). `oberth access revoke` revokes the grant and
deletes its value in both tiers. If it reports `Grant revoked; secret still stored.`, repair the
store and repeat. Revocation cannot retract read material. `oberth access dismiss` handles pending
requests; approved/revoked grants need revoke. Annotations, projections and public CAs grant
nothing. Audit records a request, not stored success. Inspect and approve resource grants with the
[resource recipe](pipelines.md#kubernetes-resources-and-virtual-machines).
Install configures the bundled store's static rules and installation audience; grants need no
synchronization or chart upgrade. Its contents are not in any backup; no store snapshot procedure
or operator token ships. Put secrets again from secure originals after loss. Keep approved paths,
workflows and steps beside backups, not values or proof of stored values:
`oberth access list --repo <repo> --kind store_path --json`.
The profile's `recovery` file unseals surviving data after restart; it cannot restore lost values.
See [backup and secret recovery](install.md#back-up).

For external stores, run these advanced checks in an operator Pod with store network access:

```sh
oberth secretstore check --config <server-config> \
  --ca-file <store-ca-file> --token-file <operator-token-file>
oberth secretstore setup --config <server-config> \
  --ca-file <store-ca-file> --token-file <operator-token-file>
```

Supply `--config` (reachable server store address/audience), `--ca-file` (CA), and `--token-file`
(private OpenBao operator token). The administrator profile lacks them; a projected Kubernetes login
token is not an operator token. Follow [operator inputs](install.md#unseal-after-restart).
`check` reads rules; `setup` writes static roles/policies, only for reviewed repair.

## Schedules and the one run cap

`.oberth/schedule.yaml` on the default branch contains a `schedules` list. Each entry has a unique
`name`, numeric five-field `cron`, optional `branch` and `timezone`, and Argo `concurrencyPolicy`.
UTC is default; named shortcuts and seconds refuse. Each entry selects its branch's build pipeline,
never a release, with a push's grants and its own source as last admitted. A branch on the forge
is found too; push others: `git push oberth <branch>`. Put `{{...}}` only in `spec.templates`;
keep other `spec` fields literal: Pod metadata, arguments, volumes and `templateDefaults`.
A branch cannot contain `{{`. Pushed runs have neither restriction. Preparation refuses before
publishing and names the field. Reason: `schedule field must be literal`:

> Move the expression into a template; schedules need literal spec-level fields.

For a dynamic branch, the reason is `schedule ref contains template syntax`:

> Rename the branch without '{{'.
```sh
oberth schedule list --repo <repo>
oberth schedule pause --repo <repo> --entry <entry>
oberth schedule resume --repo <repo> --entry <entry>
oberth schedule trigger --repo <repo> --entry <entry>
```

Pause stops future admission, not accepted work. `oberth schedule list` shows each entry's
repository, pause state and any refusal. The list does not predict the next occurrence.
Argo fires and handles overlap; Oberth adds no timer. `oberth lock status` shows lock holders.
Set `activeDeadlineSeconds` on scheduled pipelines: a run Oberth never records waits till then.

## Events and cursors

`oberth events wait --after-id <cursor>` observes authorized durable events; `oberth audit` gives
administrator action receipts. Progress observations are not audit records. `--repos`, `--run-ids`,
`--refs-glob` and `--kinds` take JSON arrays, even for one item. `--own` is a boolean. Start with
cursor 0; for example:

```sh
oberth events wait --after-id 0 --repos '["<repo>"]' --kinds '["run.updated"]' \
  --refs-glob '["refs/heads/*"]' --json
```

Scope applies before output; a glob's star never crosses a slash. Save the JSON `next_after_id` for
the next `--after-id`, even on an empty page or timeout. Do not derive it from visible events. Drain
more pages before waiting, skip handled IDs, and resume after the saved cursor on reconnect. After a
gap, reread current run/repository state and resume from the returned cursor. `run.updated` asks
observers to reread progress; it authorizes no action. Incomplete history is no permanent snapshot.

## Retention and publication

Ordinary events remain 30 days; progress remains 24 hours or the newest 10,000, whichever removes
first. This changes no terminal, hold or publication retention. Retention and timeouts cancel no
work. Logs default to 64 MiB per run. Purged, cut, running and empty output differ: read reason and
detail. A stream ending does not prove completion. `oberth log <run>` follows output and reports the
observed result; check the run record after reconnecting or removed output.
`oberth publication list --repo <repo>` shows deliveries. Pending or failed rows are the publication
outbox, separate from push recovery. `oberth publication retry` requests delivery;
`oberth publication abandon` abandons it. Their help names the required identifier and reason. A
release is published after its admitted tag is delivered with the tested commit and receipt.
