# Ephemeral Beacon fixture capabilities

This package implements one inactive host-side unit of the Beacon transport
pilot. It fixes synthetic capabilities to an admitted `PilotPlan`: a per-run
CA, two independent 32-byte tenant keys, two random tenant IDs and one fixture
generation. It imports no filesystem, Kubernetes, subprocess or network client.
It does not enable a scheduler capability or satisfy the full E2E suite.

`BindEndpoint` accepts the host's observed VMI/launcher identities and literal
port443 endpoint. It binds the initial slot once, then allows one replacement
with distinct VMI and launcher identities. Each binding gets a fresh Ed25519
server-only leaf for `beacon.fixture`, signed by this run's CA. The generation
and tenant capabilities persist across that one restart. The CA signing key
is erased from owned memory after the second leaf; it is never encoded or
exported. Certificates expire within the admitted deadline plus one minute.

`WithGuestMaterial` lends only the matching leaf certificate, private key and
two synthetic tenant capabilities to a delivery callback. `WithBootstrap`
lends exactly one newline-terminated canonical JSON frame, bounded to32KiB,
with the public CA, synthetic tenant keys, exact plan/inventory/suite/process
identities and initial endpoint. Its schema matches the independently owned
E2E conductor. When a host timestamp has zero nanoseconds, matching Kubernetes
serialization, the stale process check allows its fixture creation second.
Precise timestamps must be at or after exact fixture creation. The observed
timestamp is preserved; exact process authority still comes from the host's
durable attempt binding.

Each delivery is consumed before invoking its callback, including failures;
an ambiguous failure cannot trigger another delivery. Callbacks must not
reenter Fixture methods, log capabilities or retain/copy borrowed bytes. They
must return after their own bounded transport operation. Owned callback
buffers are cleared before returning, and adapter errors become static
errors. General JSON serialization of capability handles is refused; their
usual string/debug formatting is redacted. `Close` waits for active callbacks,
clears owned keys and refuses further use. These measures do not promise
erasure of every Go/runtime copy or prevent a trusted caller from copying
borrowed material.

The caller must independently verify producer artifacts, actual admitted
guest/conductor specs, same-node isolation, exact process/endpoint identities,
durable restart authorization, old-resource absence and all cleanup. This
package assumes those host observations; it neither observes nor authorizes
Kubernetes operations. No delivery adapter exists here. Keys must eventually
travel only through reviewed memory-only guest and authenticated conductor
transports, never argv, env, logs, files, Secret/ConfigMap or other API objects.
A host crash discards the fixture and requires cleanup, never reconstructed
passing evidence. Required-suite dispatch remains unavailable.
