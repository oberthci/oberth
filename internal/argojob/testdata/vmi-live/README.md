These generic JSON fixtures preserve the KubeVirt API shape of defaulted VMIs:
current cores/sockets/threads, guest memory, optional resource requests/limits,
and separate maxSockets/maxGuest hotplug ceilings. They are constructed fixtures,
not dumps from a private cluster. The 6Gi guest / 24Gi maximum case pins the
reported production failure; the variants pin guest precedence and request-only
legacy objects.

KubeVirt references:
- https://kubevirt.io/api-reference/main/definitions.html (Memory, CPU,
  LiveUpdateConfiguration; maxHotplugRatio defaults to four).
- https://kubevirt.io/user-guide/compute/cpu_hotplug/ (maxSockets vs sockets).
- https://kubevirt.io/user-guide/compute/memory_hotplug/ (maxGuest vs guest).

Oberth charges current guest memory (falling back to requests.memory) and
cores*sockets*threads, never hotplug ceilings or launcher resource limits.
Missing/invalid current fields require an identity-matching, server-owned claim
for template-declared accounting; otherwise the existing conservative fallback
applies. A changed current topology or guest value still consumes real capacity.
