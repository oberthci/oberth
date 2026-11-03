package installer

import (
	"context"
	"encoding/json"
	"errors"
)

type upgradeServerPKI struct {
	config Config
	store  openBaoExec
	token  string
}

func upgradeHasTLSFlags(cfg UpgradeConfig) bool {
	return len(cfg.TLSExtraDNSNames)+len(cfg.TLSExtraIPs)+len(cfg.TLSRemoveDNSNames)+len(cfg.TLSRemoveIPs) > 0
}

// Plan and check everything on the read-only path, including --dry-run.
// The caller commits this plan only after chart validation and queue waiting.
func prepareUpgradeServerPKI(ctx context.Context, cfg *UpgradeConfig, deps Deps, values map[string]any) (*upgradeServerPKI, error) {
	// Reuse the selected deployed revision, including when the latest failed.
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	var live liveHelmValuesSubset
	if err := json.Unmarshal(raw, &live); err != nil {
		return nil, err
	}
	if !live.TLS.PKI.Enabled {
		if upgradeHasTLSFlags(*cfg) {
			return nil, errors.New("TLS name flags on upgrade require an existing server PKI; use oberth install --upgrade --install-secretstore to enable it")
		}
		return nil, nil
	}
	config := Config{Namespace: cfg.Namespace, OpenBaoNamespace: cfg.OpenBaoNamespace,
		TLSExtraDNSNames: cfg.TLSExtraDNSNames, TLSExtraIPs: cfg.TLSExtraIPs,
		TLSRemoveDNSNames: cfg.TLSRemoveDNSNames, TLSRemoveIPs: cfg.TLSRemoveIPs}
	mergeTLSNames(&config, live)
	store, token, err := serverPKIAccess(ctx, config, deps)
	if err != nil {
		return nil, err
	}
	if err := checkServerPKIRole(ctx, config, deps, store, token, upgradeHasTLSFlags(*cfg)); err != nil {
		return nil, err
	}
	plan := &upgradeServerPKI{config: config, store: store, token: token}
	plan.configure(cfg)
	return plan, nil
}

func (plan *upgradeServerPKI) configure(cfg *UpgradeConfig) {
	if plan != nil {
		cfg.TLSExtraDNSNames, cfg.TLSExtraIPs = plan.config.TLSExtraDNSNames, plan.config.TLSExtraIPs
		cfg.serverPKI = true
	}
}
