# Unreleased: v0.17.96

## Helper ephemeral storage no longer caps undeclared steps (#1491)

**Regression fix (v0.17.93 to v0.17.95).** Those releases gave Argo's `wait`
container and the other server helpers a 256Mi ephemeral-storage request and a
512Mi limit. The kubelet's Pod-level eviction adds up only the limits that are
declared. A step that declared no ephemeral-storage limit therefore ran in a Pod
capped at 512Mi in total. That total had to cover the step's writable layer,
its logs and its `/cache` and `/tmp` volumes, so cold Go module downloads were
evicted with "Pod ephemeral local storage usage exceeds the total limit of
containers 512Mi".

New Workflows use helper-storage policy `argo-256Mi-512Mi-v3`:

- Helpers keep their 256Mi ephemeral-storage request for scheduling.
- When the step's own containers (or `spec.templateDefaults`) declare an
  ephemeral-storage limit, each helper also keeps its 512Mi limit, so the Pod
  total is unchanged from v0.17.95.
- When the step declares no limit, helpers carry no limit, and the Pod has no
  ephemeral-storage total, as before v0.17.93.

Running and adopted Workflows keep the policy they were submitted with
(`argo-256Mi-512Mi-v1` or `-v2`) and reconstruct with the same spec identity.
Unknown policy values are still refused.

The kubelet message "Pod ephemeral local storage usage exceeds the total limit
of containers" is no longer treated as infrastructure when the step declared
its own limit. The run fails on that step and is not auto-retried. The reason
names the Pod total, for example `Step build/test exceeded its declared
ephemeral-storage budget: the kubelet evicted its Pod at the total container
limit 1152Mi (the step's limits plus Argo helper limits). Raise the step's
ephemeral-storage limit in git.` Node-pressure evictions ("The node was low on
resource") are still infrastructure. A Workflow submitted under v1/v2 whose
undeclared step hit the helper-only cap is still classified as infrastructure.
It gets the existing single auto-retry, which runs under v3.

## Release signatures in the public transparency log

Release signing no longer passes `--use-signing-config=false
--tlog-upload=false`. The pinned release key now signs through cosign's
TUF-provided Sigstore signing config, so the `SHA256SUMS`, `release.json` and
chart bundles and the GAR image and chart signatures carry a public Rekor
inclusion proof and an RFC 3161 timestamp. Every release verification,
including the read-back, public download and tokenless runtime checks, now
requires that log entry; `--insecure-ignore-tlog` is gone from
`.oberth/release.sh` and cosign no longer warns about skipped tlog
verification. The runtime check starts each verification from an empty TUF
cache, so the downloaded binary it executes cannot plant a trust root for the
second check. Key handling, read-back verification and digest pinning are
unchanged.

Release steps reach `rekor.sigstore.dev`, `timestamp.sigstore.dev` and
`tuf-repo-cdn.sigstore.dev` over HTTPS. The pipeline egress policy already
allows port 443 and has no host allowlist, so no policy changes.

Releases up to and including v0.17.95 have no log entry. They are still
re-verified by their own tagged `release.sh`; manual checks and
`hack/upgrade-e2e/run.sh --from` add `--insecure-ignore-tlog` only for those
versions. See [release signature verification](../security.md#release-signature-verification).
`oberth upgrade` does not run cosign, so upgrading from v0.17.95 is unaffected.
