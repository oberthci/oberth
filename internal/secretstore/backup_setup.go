package secretstore

import "context"

// SetupBackup creates only the dedicated snapshot policy and login role.
// A delegated administrator needs read/create/update on these two paths, not root.
func (c *Client) SetupBackup(ctx context.Context, token []byte, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	changes := cfg.backupResources()
	plan, err := c.Plan(ctx, token, changes)
	if err != nil {
		return err
	}
	if err = c.apply(ctx, token, plan); err != nil {
		return err
	}
	plan, err = c.Plan(ctx, token, changes)
	if err == nil && len(plan) != 0 {
		return refuse("drift", nil)
	}
	return err
}

func (c Config) backupResources() []Change {
	return []Change{
		{Path: "/sys/policies/acl/" + BackupRole, Value: map[string]any{"policy": `path "sys/storage/raft/snapshot" { capabilities = ["read"] }
path "auth/token/revoke-self" { capabilities = ["update"] }`}},
		{Path: "/auth/" + c.AuthMount + "/role/" + BackupRole, Value: c.role(BackupRole, c.ServerNamespace, BackupRole)},
	}
}
