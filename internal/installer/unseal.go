package installer

// `oberth unseal` -- the one short command a sealed store costs.
//
// A production OpenBao comes back sealed from every pod restart, which on a
// laptop means every time it sleeps long enough for the node to recycle the
// pod. Recovering meant remembering the exact kubectl exec incantation and
// finding the unseal key, which is why the key is now in the host secret
// store (see tokenstore.go): with it there, recovery is this command.

import (
	"context"
	"errors"
	"fmt"
)

// Unseal reads the unseal key from the host secret store and submits it to the
// OpenBao pod through the same in-pod bao path the installer uses, so no local
// bao CLI, port-forward, or exposed OpenBao API is needed.
func Unseal(ctx context.Context, cfg Config, deps InstallDeps) error {
	deps = deps.withDefaults()

	// The same context selection Execute makes: on macOS the install manages
	// the kind cluster, so unsealing anything else would be unsealing a store
	// this machine's install did not create.
	contextName := ""
	if deps.GOOS == "darwin" {
		contextName = KindContextName
	}
	kubeClient, restConfig, selectedContext, err := deps.LoadKubeConfig(contextName)
	if err != nil {
		if contextName != "" {
			return fmt.Errorf("load kubeconfig context %q: %w", contextName, err)
		}
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	return unsealStore(ctx, cfg, Deps{
		Output:      deps.Output,
		RunCommand:  deps.RunCommand,
		LookPath:    deps.LookPath,
		KubeClient:  kubeClient,
		RestConfig:  restConfig,
		ContextName: selectedContext,
	})
}

// unsealStore is the cluster-side half, separated so it can be tested against
// a fake Kubernetes client and a scripted command runner.
func unsealStore(ctx context.Context, cfg Config, deps Deps) error {
	namespace := cfg.OpenBaoNamespace
	if namespace == "" {
		namespace = DefaultOpenBaoNamespace
	}

	client, status, err := waitForOpenBaoStatus(ctx, cfg, deps, namespace)
	if err != nil {
		return fmt.Errorf("reach OpenBao in namespace %s: %w", namespace, err)
	}
	if !status.Initialized {
		return fmt.Errorf("OpenBao in namespace %s is not initialized yet; run oberth install --install-secretstore", namespace)
	}
	if !status.Sealed {
		_, _ = fmt.Fprintf(deps.Output, "OpenBao in namespace %s is already unsealed. Nothing to do.\n", namespace)
		return nil
	}

	key, err := readStoredSecret(ctx, deps, openBaoUnsealKeyLocation)
	switch {
	case errors.Is(err, errSecretNotStored):
		// Naming the store and the command is the whole point: an operator
		// who installed before the key was saved has it on paper somewhere,
		// and the kubectl path is what they can still use today.
		return fmt.Errorf("no unseal key in %s (read it with: %s). An install from before the key was saved never "+
			"put it there; unseal with the key you kept: kubectl exec -i -n %s %s -- bao operator unseal",
			secretStoreDisplayName(deps), retrievalCommand(deps, openBaoUnsealKeyLocation), namespace, client.pod)
	case err != nil:
		return fmt.Errorf("read the unseal key from %s: %w", secretStoreDisplayName(deps), err)
	}

	unsealed, err := client.unseal(ctx, key)
	if err != nil {
		return err
	}
	if unsealed.Sealed {
		return fmt.Errorf("OpenBao in namespace %s is still sealed after submitting the stored key; "+
			"the entry may belong to an earlier deployment of the store", namespace)
	}
	_, _ = fmt.Fprintf(deps.Output, "OpenBao in namespace %s is unsealed.\n", namespace)
	return nil
}
