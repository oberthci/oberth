package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/service"
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

func runAuditHead(ctx context.Context, args []string, output io.Writer) error {
	flags, connection, parse := adminCLIFlags("audit head", args)
	positional, help, err := parse()
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, "Usage: oberth audit head [flags]")
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: audit head takes no arguments", errUsage)
	}
	raw, err := existingAdminTool(ctx, connection, "audit_list", map[string]any{"limit": 1})
	if err != nil {
		return err
	}
	var result service.AuditListResponse
	if json.Unmarshal(raw, &result) == nil && result.HeadID == 0 && len(result.Entries) == 0 {
		_, err = fmt.Fprintln(output, "0:"+strings.Repeat("0", 64))
		return err
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Entries) != 1 || result.Entries[0].ID != result.HeadID || !result.Entries[0].Verified {
		return errors.New("invalid audit head response")
	}
	digest, err := hex.DecodeString(result.Entries[0].SHA256)
	if err != nil || len(digest) != 32 {
		return errors.New("invalid audit head digest")
	}
	_, err = fmt.Fprintf(output, "%d:%s\n", result.HeadID, result.Entries[0].SHA256)
	return err
}
func runAuditRecord(ctx context.Context, args []string, output io.Writer) error {
	flags, connection, parse := adminCLIFlags("audit record", args)
	details := flags.String("details", "{}", "public lifecycle audit details as a JSON object")
	positional, help, err := parse()
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, "Usage: oberth audit record <action> [--details JSON]")
	}
	if len(positional) != 1 || !json.Valid([]byte(*details)) {
		return fmt.Errorf("%w: audit record <action> [--details JSON]", errUsage)
	}
	return callAdminCLI(ctx, connection, "audit-record", map[string]any{"action": positional[0], "details": json.RawMessage(*details)}, output)
}
