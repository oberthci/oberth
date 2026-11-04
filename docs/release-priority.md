# Execution priority and fairness

Waiting admission uses four classes, in this order:

| Class | Intended work | Authority |
| --- | --- | --- |
| `low` | Deliberately deferred or background builds | A submission may defer itself; changing an existing run requires an unscoped admin |
| `standard` | Ordinary builds, schedules and backlog work | Default |
| `promotion` | Ordinary promotion CI and eligible tag/release delivery | Server classification, or an unscoped admin / unexpired grant for an explicit request |
| `high` | Explicitly authorized urgent work | Unscoped admin, or an unexpired grant permitting `high` |

Both durable claims and in-memory admission order `high > promotion > standard
> low`, then the original admission sequence and queue sequence. Running or
already granted work is never preempted. Priority does not bypass admission
holds, PSI/pressure, resource ceilings, concurrency groups, cleanup or publication
guards. Bounded dispatch lookahead retains its existing claim ceiling; it exposes
higher-priority waiting work without creating additional execution capacity.

An ordinary promotion receives `promotion`, never automatic `high`. The oldest
waiting server-classified tag/release per repository also receives `promotion`
when its stored ref kind is `tag` and it has the release flag or tag/release
trigger. Claimed but unadmitted tags count as waiting. Later tags remain
`standard`; admitting or terminating the oldest releases its boost for the next.
Administrator RC globs classify matching branches as `promotion`. Defaults have
no RC globs. A branch/ref spelling, release flag, pipeline annotation, trigger
spelling or promotion operation never supplies authority for `high`.

After 60 minutes, ordinary `standard` work joins `promotion` at its original FIFO
position, with reason `aged: standard run waited at least 60 minutes`. It never
ages into `high`. Low work does not age. Every explicit admin override pins its
class, including a lowered `standard` or `promotion`; admission/end freezes the
aging clock. Aged work can therefore precede newer delivery work in the shared
promotion FIFO. Sustained explicitly urgent high work can still delay it.

## Requests and administration

`git push -o oberth.priority=high -o oberth.priority_reason='urgent repair'`
requests an execution class. MCP `promote`, `run_rerun`, `run_retry`,
`schedule_trigger` and `sync` accept the same optional `priority` and
`priority_reason` fields. All four canonical names are accepted. An unscoped
admin may request promotion/high directly; other identities require a matching,
unexpired `priority_grant {identity,max_class,expires}`. `max_class` is
`promotion` or `high`. A promotion-only grant cannot request high.

Authority is resolved in the insertion transaction from the authenticated actor
and exact SSH key fingerprint. An identity alias or scoped-admin key cannot
inherit unscoped admin authority. Low and standard submission requests need no
urgency grant. Low explicitly defers a new submission; a standard request leaves
any natural promotion/tag/RC classification intact. An explicit accepted high
request takes precedence over automatic delivery classification. Refused or
invalid requests leave the natural class intact and report `priority_refusal`.
A repository schedule may declare any class, but automatic execution requests as
`oberth:scheduler` and still needs its own grant for promotion/high.

`sync` publishes an existing branch result and can only raise its waiting build's
priority. It cannot lower an existing run or modify one already admitted. Use
unscoped-admin `run_priority` for explicit lowering:

```sh
oberth mcp call run_priority '{"run_id":"01234567","priority":"promotion","reason":"Ordinary delivery can wait behind urgent work"}'
```

That tool accepts a full ID or unique 8+ hex prefix, any of the four classes and
a printable 1–512 byte reason. It sets and pins the class, including lowering
high to promotion, standard or low. The service holds the admission boundary;
admitted or terminal runs refuse changes. The atomic `run.priority` audit records
actor, previous class, selected class and reason. Overrides survive restart and
automatic refire. Local updates refresh waiters immediately; external changes
and aging are polled within 15 seconds, with refresh deferred on DB contention.

`queue_priority_config` remains an unscoped-admin, audited replacement of at
most 16 case-sensitive RC globs. `*` and `?` match slash; `[]` disables RC
classification. `queue_priority_get` returns them and `starvation_minutes: 60`.
These globs confer only promotion class, regardless of the spelling they match.

## Tags, resources and supersession

Automatic tag priority remains provisional until exact checkout resolves size.
No size-grant record defaults to enabled within L. An oversized tag or an
unavailable size-limit lookup loses its automatic promotion class, retaining an
explicit admin override or accepted nonstandard request, otherwise using
standard order. The existing reason explains the fallback. This advisory lookup
never fails release execution; the tag retains its reviewed execution size.
`size_revoke` durably disables automatic tag priority even on a never-granted
repository. `size_grant` re-enables it within the selected maximum. Explicit
urgency, promotions and aging are independent of that opt-out.

The existing measured backfill and head-reservation rules remain. Same effective
and base classes may coexist only when resource/run-slot reservations stay safe;
otherwise a later candidate needs its measured p95 to finish before the head's
reservation. Head-wait drain, its bounded liveness escape, strict FIFO, group
parking and runtime capacity checks are unchanged. Priority grants no resource
or pressure exemption.

Accepted nonstandard requests, admin overrides and automatic RC/tag/promotion
work retain supersession protection. Historical automatic-protection rows do
not become urgency grants. Requests cancel nothing unless an authorized push
explicitly selects `oberth.supersede=true`; replacing protected work requires its
requester or an unscoped admin. Same-branch/schedule-entry, cancellation, pending
publication and promotion guards remain. A forced ordinary schedule only
supersedes unprotected work; force does not confer priority authority.

Restart requeue, trusted preflight recovery and eligible infrastructure retry
retain explicit priority, reasons, original wait and admission FIFO alongside
newer siblings. A manual retry earns its normal new queue place. Replay retains
its accepted authority/decision; revoking or expiring a grant affects future
requests, not an already accepted run.

## Compatibility and upgrade

| Legacy context | Current interpretation |
| --- | --- |
| `normal` input or persisted override | `standard` |
| Explicit authorized `high` input, override, accepted request or grant | `high` |
| Explicit authorized `highest` input, override, accepted request or grant | `high` |
| Automatic promotion/tag or RC classification formerly high/highest | `promotion`, subject to existing eligibility |
| Automatic historical request with `requested=''` | Protection evidence only; never explicit high authority |
| `low` | `low` |

Canonical responses, reason text and dashboard detail use only low, standard,
promotion and high. MCP schemas retain deprecated input aliases normal and
highest for existing clients, with the same authentication. Old high and highest
grants both authorize explicit high; an administrator can narrow a grant to
promotion when delivery-only authority is intended. The compatibility Go identifiers
`PriorityNormal` and `PriorityHighest` name the canonical standard/high values.

The timestamp migration `20261104190000-priority-four-classes` extends the three
priority tables' accepted vocabulary. It copies existing rows byte-for-byte:
requested/accepted names, overrides, grants, actors, reasons and timestamps stay
intact. Reads normalize legacy values; new writes use canonical accepted names
and retain the raw requested input. Historical audit records and shipped integer
migration 39 and the admin-lowering migration are unchanged. Take the normal
pre-upgrade backup; rollback restores that backup because older binaries cannot
interpret new classes. This is next-release work and does not alter the reviewed
v0.17.85 candidate or its CI.
