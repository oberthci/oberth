package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/store"
)

func runAudit(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		return runAuditList(ctx, arguments, output)
	}
	switch arguments[0] {
	case "head":
		return runAuditHead(ctx, arguments[1:], output)
	case "record":
		return runAuditRecord(ctx, arguments[1:], output)
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

// runAuditRecord appends a lifecycle action (backup.completed,
// restore.completed, upgrade.completed) reported by the workstation CLI after
// an operation the server cannot observe itself (#1104). It runs in-pod on the
// live daemon's database, after the daemon's audit mutation gate, and only the
// store's fixed action list is accepted.
func runAuditRecord(ctx context.Context, arguments []string, output io.Writer) error {
	return runAuditRecordWithGate(ctx, arguments, output, requestLiveAdminMutationGate)
}

func runAuditRecordWithGate(ctx context.Context, arguments []string, output io.Writer, mutationGate func(context.Context, string, string) error) error {
	flags := flag.NewFlagSet("audit record", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	database := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod; requires the live admin daemon)")
	details := flags.String("details", "{}", "JSON details to record")
	if err := flags.Parse(permuteFlagsFirst(arguments)); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: audit record <action> [--details JSON]", errUsage)
	}
	if !json.Valid([]byte(*details)) {
		return fmt.Errorf("%w: --details must be JSON", errUsage)
	}
	if mutationGate == nil {
		return errors.New("admin audit mutation gate is unavailable")
	}
	if err := mutationGate(ctx, "audit.database.open", *database); err != nil {
		return err
	}
	handle, err := store.OpenAdminClient(ctx, *database, configuredStoreOptions(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	if err := mutationGate(ctx, "audit.record", *database); err != nil {
		return err
	}
	action, err := handle.RecordOperationAudit(ctx, administrativeActor, flags.Arg(0), *details)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "recorded %s as audit entry %d\n", action.Action, action.ID)
	return err
}
