These generic JSON fixtures preserve the KubeVirt API shape of defaulted VMIs:
current cores/sockets/threads, guest memory, optional resource requests/limits,
and separate maxSockets/maxGuest hotplug ceilings. They are constructed fixtures,
not dumps from a private cluster. The 6Gi guest / 24Gi maximum case pins the
reported production failure; the variants pin guest/request accounting and request-only
legacy objects.

KubeVirt references:
- https://kubevirt.io/api-reference/main/definitions.html (Memory, CPU,
  LiveUpdateConfiguration; maxHotplugRatio defaults to four).
- https://kubevirt.io/user-guide/compute/cpu_hotplug/ (maxSockets vs sockets).
- https://kubevirt.io/user-guide/compute/memory_hotplug/ (maxGuest vs guest).

Oberth charges max(current guest memory, requests.memory) and
cores*sockets*threads, never hotplug ceilings or launcher resource limits.
An exact, active server-owned claim is always a componentwise floor, even
when live fields decode cleanly. A malformed field does not erase readable
siblings; missing/nonpositive CPU factors contribute one while preserving the
remaining sockets/threads product. Ambiguous fields still require a matching
claim or the existing conservative unclaimed fallback. Numeric overflow
saturates and refuses capacity. A changed current topology or memory value
still consumes real capacity. No hotplug maximum or launcher limit is charged.
