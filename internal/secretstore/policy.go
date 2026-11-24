package secretstore

func (c Config) resources(accessor string) []Change {
	return []Change{
		{Path: "/auth/" + c.AuthMount + "/config", Value: map[string]any{"kubernetes_host": "https://kubernetes.default.svc", "disable_local_ca_jwt": false}},
		{Path: "/sys/policies/acl/" + c.WriterRole, Value: map[string]any{"policy": "path \"" + c.Mount + "/steps/*\" { capabilities = [\"create\", \"update\", \"delete\"] }\npath \"auth/token/revoke-self\" { capabilities = [\"update\"] }"}},
		{Path: "/sys/policies/acl/" + c.StepRole, Value: map[string]any{"policy": "path \"" + c.Mount + "/steps/{{identity.entity.aliases." + accessor + ".metadata.service_account_name}}/*\" { capabilities = [\"read\"] }\npath \"auth/token/revoke-self\" { capabilities = [\"update\"] }"}},
		{Path: "/auth/" + c.AuthMount + "/role/" + c.WriterRole, Value: c.role(c.ServerAccount, c.ServerNamespace, c.WriterRole)},
		{Path: "/auth/" + c.AuthMount + "/role/" + c.StepRole, Value: c.role("*", c.PipelineNamespace, c.StepRole)},
		{Path: "/sys/policies/acl/" + BackupRole, Value: map[string]any{"policy": `path "sys/storage/raft/snapshot" { capabilities = ["read"] }
path "auth/token/revoke-self" { capabilities = ["update"] }`}},
		{Path: "/auth/" + c.AuthMount + "/role/" + BackupRole, Value: c.role(BackupRole, c.ServerNamespace, BackupRole)},
	}
}

func (c Config) role(account, namespace, policy string) map[string]any {
	return map[string]any{
		"bound_service_account_namespace_selector": "",
		"token_no_default_policy":                  true,
		"alias_name_source":                        "serviceaccount_uid",
		"token_ttl":                                600, "token_max_ttl": 600,
		"bound_service_account_names":      []string{account},
		"bound_service_account_namespaces": []string{namespace},
		"token_policies":                   []string{policy},
		"audience":                         c.Audience,
	}
}
