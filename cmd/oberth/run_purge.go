package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
)

const runPurgeUsage = "Usage: oberth run purge <run-id> --what logs|artifacts|both --reason <text> [--json] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]"

func runRunPurge(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, runPurgeUsage)
		return err
	}
	flags := flag.NewFlagSet("run purge", flag.ContinueOnError)
	whatFlag := flags.String("what", "", "logs, artifacts or both")
	reasonFlag := flags.String("reason", "", "purge reason")
	flags.Bool("json", false, "emit JSON")
	transportFlags := mcpTransportFlags(flags)
	positional, _, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	id := ""
	if len(positional) == 1 {
		id = positional[0]
	}
	what, reason := *whatFlag, *reasonFlag
	transport := transportFlags()
	if id == "" || (what != "logs" && what != "artifacts" && what != "both") || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: %s", errUsage, runPurgeUsage)
	}
	config, err := parseMCPFlags("run purge", transport, 0)
	if err != nil {
		return err
	}
	raw, err := (remoteRunCanceller{config: config}).call(ctx, "run_purge", map[string]any{"id": id, "what": what, "reason": reason})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, strings.TrimSpace(string(raw)))
	return err
}
