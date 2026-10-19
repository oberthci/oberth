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
// exposed by the MCP tools.
func runKubeVirtGrant(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: kubevirt grant|revoke|get <repo>", errUsage)
	}
	action := arguments[0]
	if action != "grant" && action != "revoke" && action != "get" {
		return fmt.Errorf("%w: kubevirt grant|revoke|get <repo>", errUsage)
	}
	flags := flag.NewFlagSet("kubevirt "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod)")
	if err := flags.Parse(arguments[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: kubevirt %s <repo>", errUsage, action)
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
	repository, err := database.RepositoryByName(ctx, flags.Arg(0))
	if err != nil {
		return err
	}
	var value any
	switch action {
	case "grant", "revoke":
		value, err = database.SetKubeVirtGrant(ctx, repository.ID, administrativeActor, action == "grant")
	case "get":
		value, err = database.KubeVirtGrant(ctx, repository.ID)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(value)
}
