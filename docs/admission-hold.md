# Hold ordinary admission for release qualification

An unscoped administrator can pause new ordinary admission while allowing
selected classes through. Existing running work and granted reservations keep
their resources. Wait for those runs to finish before starting measurements
that need the host to themselves.

```sh
oberth admission hold on --reason 'release qualification'
oberth admission hold on --reason 'release qualification' --allow tag,release,promotion --duration 4h
oberth admission hold on --reason 'maintenance drain' --allow '' --duration 30m
oberth admission hold off --reason 'qualification complete'
oberth mcp call system_status '{}'
```

The first command defaults to allowing `tag` and `release`, for two hours. A
run passes if **any** allowed class matches: `tag` means a tag ref, `release`
means release intent, and `promotion` means a promotion-triggered run. These
classes are independent of priority: even an administrator's highest-priority
ordinary branch run waits during a hold. An empty allow list holds all new
admission. The workflow count ceiling, hard pressure valve, disk floor, groups and VM
limits still apply to allowed runs. Kubernetes decides placement from pod
requests; the hold does not restore application-side weight or CPU/memory
budgets.

All three interfaces use the same authorization, audit and validation path:

- MCP: `admission_hold({"on":true,"reason":"release qualification","duration":"2h","allow":["tag","release"]})`.
- REST: authenticated `POST /api/admission/hold` with that JSON body.
- CLI: `oberth admission hold on|off --reason <reason>` with the usual MCP
  connection flags. `--json` returns the resulting hold object.

`on` and a nonempty reason are required for both enabling and disabling. Reasons
are limited to 512 bytes and cannot contain control characters. `duration` is a
positive Go duration with a hard cap of 12 hours; omitting it uses two hours.
`allow` and `duration` apply only to enabling. Omitting `allow` uses the default;
`[]` allows no class. Unknown or duplicate class names are rejected. Calling
`on` again replaces the hold and starts a new bounded duration, with an audit
record of both settings.

The hold and its absolute UTC expiry are persisted together with an
`admission.hold` audit entry. A server restart reloads the same deadline; it does
not extend it. The deadline itself releases the hold without another admin
command. Eligibility and status evaluate the deadline on every observation;
an idle scheduler resumes on its normal dispatch wake, at most 15 seconds
later under normal operation. No background database write is needed to expire
it. The audit entry includes the previous and new settings, actor and deadline;
there is no separate expiry-write dependency.

`system_status.admission.hold` (also `GET /api/status`) reports effective `on`,
reason, allow list, actor, absolute `expires_at` and `changed_at`. Expired holds
report `on:false` while retaining their last settings for diagnosis. Scoped
status omits installation-wide hold configuration. A held run uses its existing
queued status and reason line:

```text
admission held by admin: release qualification, until <UTC time>
```

Held work is neither cancelled nor re-enqueued. Its original queue sequence and
priority/FIFO order are preserved; allowed work can pass it. Claimed work still
preparing or waiting for resources observes the same hold at the resource gate.
A grant already owned before the hold is untouched, including across restart
adoption. Clearing or expiring the hold makes waiting work eligible again under
normal priority, FIFO and resource rules.

## Workflow count and release isolation

The runtime ceiling counts granted runs, independent of the historical
S/M/L/XL label. Setting it to one is valid; zero is no ceiling. Kubernetes
places each pod using its requests. The hold selects which classes may obtain
new workflow slots; it does not change the ceiling or preempt existing grants.
Use the hold to keep ordinary work queued while releases proceed. See
[runtime admission capacity](runtime-admission-capacity.md).
