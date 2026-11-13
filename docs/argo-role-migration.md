# Argo Role ownership

v0.22.4 folds the four remaining additive pipeline Roles into the server's base `-argo` Role. Effective permissions remain the same; the executor's own task-result Role remains separate. Helm removes the former split Roles and bindings on upgrade.

The base Role rules are no longer frozen at the v0.17.9 shape. If another field manager owns that rules list, Helm server-side apply can refuse the update. Resolve the conflicting ownership before upgrading. Oberth does not transfer managed fields automatically, and the old frozen-rule migration helper is removed.
