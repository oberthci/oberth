package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const sizeGrantUsage = "size grant <repo> --max L|XL | size revoke <repo> | size get <repo>"

// runSizeGrant is the in-pod admin CLI for the audited repository size grant
// (#1021) exposed by the size_grant MCP tools.
func runSizeGrant(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: %s", errUsage, sizeGrantUsage)
	}
	action := arguments[0]
	if action != "grant" && action != "revoke" && action != "get" {
		return fmt.Errorf("%w: %s", errUsage, sizeGrantUsage)
	}
	flags := flag.NewFlagSet("size "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod)")
	maximum := ""
	if action == "grant" {
		flags.StringVar(&maximum, "max", "", "largest admission size the repository may declare: L or XL")
	}
	// Accept the repository before or after the flags.
	var repo string
	rest := arguments[1:]
	if len(rest) > 0 && len(rest[0]) > 0 && rest[0][0] != '-' {
		repo, rest = rest[0], rest[1:]
	}
	if err := flags.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if repo == "" && flags.NArg() == 1 {
		repo = flags.Arg(0)
	} else if repo == "" || flags.NArg() != 0 {
		return fmt.Errorf("%w: %s", errUsage, sizeGrantUsage)
	}
	if action == "grant" && !store.ValidSizeGrantMax(periapsis.Size(maximum)) {
		return fmt.Errorf("%w: size grant needs --max L or --max XL", errUsage)
	}
	if action != "get" {
		if err := requestLiveAdminMutationGate(ctx, "size."+action, *databasePath); err != nil {
			return err
		}
	}
	database, err := store.OpenAdminClient(ctx, *databasePath, configuredStoreOptions(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	repository, err := database.RepositoryByName(ctx, repo)
	if err != nil {
		return err
	}
	var value any
	switch action {
	case "grant":
		value, err = database.SetSizeGrant(ctx, repository.ID, administrativeActor, periapsis.Size(maximum))
	case "revoke":
		value, err = database.SetSizeGrant(ctx, repository.ID, administrativeActor, "")
	case "get":
		var size periapsis.Size
		size, err = database.MaxRunSize(ctx, repository.ID)
		if err == nil {
			grant, grantErr := database.SizeGrant(ctx, repository.ID)
			switch {
			case errors.Is(grantErr, store.ErrNotFound):
				value = map[string]any{"repo": repo, "max_size": size, "state": "never_granted"}
			case grantErr != nil:
				err = grantErr
			default:
				value = map[string]any{"repo": repo, "max_size": size, "grant": grant}
			}
		}
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(value)
}
