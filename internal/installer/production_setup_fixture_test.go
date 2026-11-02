package installer

import (
	"context"
)

func setupProductionSecretStoreFixture(ctx context.Context, cfg Config, deps Deps, openbao OpenBaoResult) (SecretStoreResult, error) {
	var credentials heldCredentials
	defer credentials.flush(deps.Output, isColor(deps))
	return setupProductionSecretStoreCollect(ctx, cfg, deps, deps, openbao, &credentials)
}
