package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/store"
)

// runKubeVirtGrant is an in-pod admin CLI for the same audited repo grant
// exposed by the MCP tools. Budget flags may follow the repository:
// `oberth kubevirt grant <repo> --max-vms 3 --max-cores 12 --max-memory 24Gi`.
func runKubeVirtGrant(ctx context.Context, arguments []string, output io.Writer) error {
	const usage = "kubevirt grant <repo> [--max-vms N --max-cores C --max-memory M] | revoke <repo> | get <repo>"
	if len(arguments) == 0 {
		return fmt.Errorf("%w: %s", errUsage, usage)
	}
	action := arguments[0]
	if action != "grant" && action != "revoke" && action != "get" {
		return fmt.Errorf("%w: %s", errUsage, usage)
	}
	flags := flag.NewFlagSet("kubevirt "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod)")
	var maxVMs, maxCores *int
	var maxMemory *string
	if action == "grant" {
		maxVMs = flags.Int("max-vms", 0, "maximum VMI create templates per run, 1..16 (default 2)")
		maxCores = flags.Int("max-cores", 0, "maximum summed VMI cores per run, 1..64 (default 6)")
		maxMemory = flags.String("max-memory", "", "maximum summed VMI memory per run, a Kubernetes quantity 1Gi..256Gi (default 12Gi)")
	}
	parse := func(values []string) error {
		if err := flags.Parse(values); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				flags.SetOutput(output)
				flags.Usage()
				return flag.ErrHelp
			}
			return fmt.Errorf("%w: %w", errUsage, err)
		}
		return nil
	}
	if err := parse(arguments[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() == 0 {
		return fmt.Errorf("%w: kubevirt %s <repo>", errUsage, action)
	}
	repoName := flags.Arg(0)
	if err := parse(flags.Args()[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: kubevirt %s <repo>", errUsage, action)
	}
	var budget store.KubeVirtBudget
	if action == "grant" {
		budget.MaxVMs, budget.MaxCores = *maxVMs, *maxCores
		if *maxMemory != "" {
			bytes, err := store.ParseKubeVirtMemory(*maxMemory)
			if err != nil {
				return fmt.Errorf("%w: %w", errUsage, err)
			}
			budget.MaxMemoryBytes = bytes
		}
		if err := budget.WithDefaults().Validate(); err != nil {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
	}
	if action != "get" {
		if err := requestLiveAdminMutationGate(ctx, "kubevirt."+action, *databasePath); err != nil {
			return err
		}
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, configuredStoreOptions(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	repository, err := database.RepositoryByName(ctx, repoName)
	if err != nil {
		return err
	}
	var value any
	switch action {
	case "grant", "revoke":
		value, err = database.SetKubeVirtGrant(ctx, repository.ID, administrativeActor, action == "grant", budget)
	case "get":
		value, err = database.KubeVirtGrant(ctx, repository.ID)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(value)
}
