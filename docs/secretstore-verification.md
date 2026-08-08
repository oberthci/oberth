# Verifying the secret store integration

Three tiers, cheapest first. Tier 1 runs anywhere in seconds, Tier 2 proves a
live trust chain in one command, Tier 3 is the full release rehearsal.

## Tier 1 — script contract suite (no store, no cluster, <5 s)

```sh
bash hack/test-setup-secretstore.sh
```

Runs `scripts/setup-secretstore.sh` against mock `bao`/`kubectl` CLIs and
asserts its security contract:

- fresh setup performs exactly the five documented mutations and prints the
  helm handoff;
- an immediate re-run mutates **nothing** (idempotency is observed, not
  assumed);
- a drifted role or customized policy is refused without `--force`;
- an auth config pointing at a **different cluster** is refused without
  `--force-auth-config` (one auth mount serves exactly one cluster);
- a `jwt`-type mount at the target path and a KV **v1** mount fail closed;
- `--dry-run` writes nothing;
- the in-cluster branch works without kubectl;
- plain-HTTP warns, and a loopback store address is never emitted into the
  helm handoff.

`go test ./...` runs the same suite via `TestSetupSecretStoreScriptContract`,
so CI covers it on every push.

## Tier 2 — live trust chain, one command (~5 s)

After `setup-secretstore.sh` (store side) and `helm upgrade --install` with
`secretstore.enabled=true` (cluster side):

```sh
kubectl exec -n oberth deploy/oberth -- oberth secretstore verify
```

The verifier runs **inside the pod with only the pod's ServiceAccount
identity** — no vault token, no flags. It reads the live serve process's
`--secretstore-*` configuration from `/proc/1/cmdline`, then exercises the
production fetch path end to end: ServiceAccount JWT login at the Kubernetes
auth mount, TokenReview by the store, policy-gated KV reads of every
allowlisted path, TLS verification — and reports key counts only. Every
fetched value is zeroed before exit; nothing is printed or persisted.

```
secret store verify: address=https://openbao.example.eu:8200 mount=kubernetes role=oberth-ci paths=1
  ok oberth/data/r2-upload (2 keys)
secret store verify: OK — ServiceAccount login, TokenReview, read policy, and TLS all verified
```

To prove a candidate path **before** allowlisting it in the chart, pass it as
an argument:

```sh
kubectl exec -n oberth deploy/oberth -- \
  oberth secretstore verify oberth/data/new-secret
```

A failure names the failing stage (login vs read) and prints the common
causes: missing role binding, missing `system:auth-delegator` for an
out-of-cluster store, a `kubernetes_host` the store cannot reach, or a path
outside the read policy.

## Tier 3 — full release rehearsal (~15 min, real cluster)

The complete customer-shaped scenario on a disposable environment
(for example k3s on a VM):

1. **Store**: install OpenBao with TLS (never dev mode for this test), init,
   unseal, log in with the root token *on the store host only*.
2. **Trust**: `BAO_ADDR=https://<store>:8200 ./scripts/setup-secretstore.sh`
   — expect all five steps to report created; re-run once and expect all
   `unchanged` (Tier 1's idempotency claim, now against a real store).
3. **Secret**: `bao kv put oberth/r2-upload token=rehearsal-value`.
4. **Cluster**: `helm upgrade --install oberth oberth/oberth --namespace
   oberth --create-namespace --set secretstore.enabled=true --set
   secretstore.address=https://<store>:8200 --set
   'secretstore.allowedPaths={oberth/data/r2-upload}'` (plus
   `secretstore.caCert` for a private CA).
5. **Trust chain**: Tier 2 verify must print `OK`.
6. **Release**: in a test repository, declare
   `var SecretStoreSecrets = map[string]string{"r2-token": "oberth/data/r2-upload"}`,
   add a release step that asserts
   `test -s "$OBERTH_SECRETSTORE_DIR/r2-token/token"`, push a tag, and watch
   the Release burn go green.
7. **Fail-closed**: `bao kv metadata delete oberth/r2-upload`, push another
   tag, and confirm the run fails **before its Job exists** with the exact
   path in the error; the audit chain carries the
   `release.secretstore.fetch.*` intent and outcome entries.
8. **Negative boundary**: declare a path *not* in `secretstore.allowedPaths`
   and confirm admission refuses it; `kubectl get secrets -n oberth` and
   `kubectl get jobs -n oberth -o yaml | grep -c rehearsal-value` must show
   the value in no Kubernetes object at any point.

Steps 7 and 8 are the point of the design: a missing secret is a pre-flight
failure with a name, not a mid-build mystery, and the values never had a
Kubernetes-object or disk representation to leak.
