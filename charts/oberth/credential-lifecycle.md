
Remote uplink/grant lifecycle tools use existing database and audit authority
only (#1105); all five new tools require an unscoped administrator.
They require no new chart values or Kubernetes RBAC. Credential
issuance stays in-pod; MCP rotation hands off to an already-issued replacement.
