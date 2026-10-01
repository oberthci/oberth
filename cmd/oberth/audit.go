package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/store"
)

func runAudit(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: audit requires a subcommand: head", errUsage)
	}
	switch arguments[0] {
	case "head":
		return runAuditHead(ctx, arguments[1:], output)
	default:
		return fmt.Errorf("%w: unknown audit subcommand %q", errUsage, arguments[0])
	}
}

func runAuditHead(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("audit head", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	database := flags.String("database", "/data/oberth.sqlite", "SQLite database path")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: audit head accepts flags only", errUsage)
	}
	if strings.TrimSpace(*database) == "" {
		return fmt.Errorf("%w: database path is required", errUsage)
	}
	inspection, err := store.InspectCurrent(ctx, *database, configuredStoreOptions(ctx))
	if err != nil {
		return fmt.Errorf("read audit head: %w", err)
	}
	head, headErr := inspection.AuditHeadHint(ctx)
	closeErr := inspection.Close()
	if err := errors.Join(headErr, closeErr); err != nil {
		return fmt.Errorf("read audit head: %w", err)
	}
	_, err = fmt.Fprintf(output, "%d:%s\n", head.ID, hex.EncodeToString(head.SHA256))
	return err
}
