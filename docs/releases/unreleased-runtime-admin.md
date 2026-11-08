# Unreleased: runtime release WIF and capacity administration (#1497)

- New unscoped-admin MCP tools `release_wif_list`, `release_wif_grant`,
  `release_wif_revoke` and `release_wif_reset` (CLI `oberth release-wif`) change
  release WIF repository entries without a Helm upgrade or restart. Helm
  `argo.releaseWIF` remains the boot default and the only source of roles and
  dedicated step identities. A revoke also removes an entry for a retired,
  unregistered repository.
- New `admission_capacity_config` (CLI `oberth admission capacity`) sets or
  clears a server-owned runtime workflow ceiling that takes precedence over the
  mounted runtime-capacity file and the boot flag.
- Overrides live in the server database (migration
  `20261108090000-runtime-admin-overrides`), never in a Helm-owned object, so
  runtime changes and upgrades cannot conflict. Every change requires a reason
  and is audited with the previous value.
