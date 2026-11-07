# Unreleased: v0.17.97

## Helper storage limits follow the Pod Argo runs (#1491 follow-ups)

New Workflows use helper-storage policy `argo-256Mi-512Mi-v4`. Like v3, a
helper keeps its 256Mi ephemeral-storage request and adds its 512Mi limit only
when the step itself declares an ephemeral-storage limit. v4 decides that from
the containers Argo v4.0.8 actually runs after merging `spec.templateDefaults`:

- A default `containerSet` limit no longer counts. Argo replaces container-set
  lists rather than merging them, so that limit never ran, and under v3 the
  step's Pod was capped at the helper limits alone.
- A default sidecar now counts even when the template does not list it, since
  Argo adds default sidecars by name.
- Native sidecars (init containers with `restartPolicy: Always`) now count;
  the kubelet adds their limits to the Pod total like regular containers.
- An explicit `0` limit on the template overrides a default limit.

Running and adopted Workflows keep the policy they were submitted with (v1, v2
or v3) and reconstruct with the same spec identity.

**Rollback:** a v0.17.96 server refuses to re-create Workflows with policy
`argo-256Mi-512Mi-v4`, and v0.17.95 also refuses v3, so drain v4 runs before
rolling back to v0.17.96, and v3 and v4 runs before rolling back to v0.17.95.

## Storage eviction classification

- "Container X exceeded its local ephemeral storage limit" is now a failure of
  the step, not infrastructure, when X is one of the step's own containers with
  a declared limit (the main container, a container-set member, a sidecar or a
  native sidecar). The run is not auto-retried, and the reason names the
  container and its limit, for example `Step build/test exceeded its declared
  ephemeral-storage budget: the kubelet evicted its Pod at container main's own
  limit 640Mi. Raise the step's ephemeral-storage limit in git.` A limit on an
  Argo or Oberth helper (`wait`, `init`) stays infrastructure, as do
  node-pressure evictions.
- The Pod-total message is now recognised with every quantity form the kubelet
  prints, including decimal exponents such as `1e9`.
- For a v3 Workflow whose only limit was a default `containerSet` member, a
  Pod-total eviction is infrastructure again: that total was helper limits
  alone. It gets the single auto-retry, which runs under v4.

## Release signing

The "already signed?" check in `.oberth/release.sh` retries once after 5
seconds before re-signing a registry object, so a transient TUF or Rekor
failure no longer adds a duplicate signature. The final verification is
unchanged.
