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

const resourceGrantUsage = "resource grant <repo> --requests-cpu 2 --requests-memory 4Gi --limits-cpu 4 --limits-memory 8Gi | resource revoke <repo> | resource get <repo>"

func runResourceGrant(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: %s", errUsage, resourceGrantUsage)
	}
	action := arguments[0]
	if action != "grant" && action != "get" && action != "revoke" {
		return fmt.Errorf("%w: %s", errUsage, resourceGrantUsage)
	}
	flags := flag.NewFlagSet("resource "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod)")
	requests, limits := map[string]string{}, map[string]string{}
	if action == "grant" {
		for _, tier := range []struct {
			name   string
			values map[string]string
		}{{"requests", requests}, {"limits", limits}} {
			for _, name := range []string{"cpu", "memory", "ephemeral-storage"} {
				value := ""
				if name == "ephemeral-storage" {
					value = "16Gi"
				}
				flags.String(tier.name+"-"+name, value, "administrator ceiling as a Kubernetes quantity")
			}
		}
	}
	rest := arguments[1:]
	repo := ""
	if len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
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
		return fmt.Errorf("%w: %s", errUsage, resourceGrantUsage)
	}
	resources := store.DefaultResourceCeiling()
	if action == "grant" {
		for _, tier := range []struct {
			name   string
			values map[string]string
		}{{"requests", requests}, {"limits", limits}} {
			for _, name := range []string{"cpu", "memory", "ephemeral-storage"} {
				tier.values[name] = flags.Lookup(tier.name + "-" + name).Value.String()
			}
		}
		var err error
		resources, err = store.ParseResourceCeiling(requests, limits)
		if err != nil {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
	}
	if action != "get" {
		if err := requestLiveAdminMutationGate(ctx, "resource."+action, *path); err != nil {
			return err
		}
	}
	database, err := store.OpenAdminClient(ctx, *path, configuredStoreOptions(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	repository, err := database.RepositoryByName(ctx, repo)
	if err != nil {
		return err
	}
	switch action {
	case "grant":
		_, err = database.SetResourceCeiling(ctx, repository.ID, administrativeActor, &resources)
	case "revoke":
		_, err = database.SetResourceCeiling(ctx, repository.ID, administrativeActor, nil)
	}
	if err != nil {
		return err
	}
	ceiling, err := database.ResourceCeiling(ctx, repository.ID)
	if err != nil {
		return err
	}
	grant, err := database.ResourceGrant(ctx, repository.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	value := map[string]any{"repo": repo, "resource_ceiling": ceiling}
	if err == nil {
		value["grant"] = grant
		value["state"] = "granted"
		if grant.RevokedAt != nil {
			value["state"] = "revoked"
		}
	} else {
		value["state"] = "never_granted"
	}
	return json.NewEncoder(output).Encode(value)
}
