package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/api"
)

const runRerunUsage = "Usage: oberth run rerun <run-id> [--reason <text>] [--force] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]"

// runRunRerun is `oberth run rerun`, the CLI form of MCP run_rerun (#1098):
// a failed or interrupted run is rerun at the same SHA and ref, linked to the
// original, without a push.
func runRunRerun(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, runRerunUsage)
		return err
	}
	options, err := parseCancelOptions(arguments, false)
	if err != nil {
		return err
	}
	if options.database != "" {
		return fmt.Errorf("%w: run rerun is a remote MCP command; --database is not supported", errUsage)
	}
	if len(options.positional) != 1 {
		return fmt.Errorf("%w: run rerun <run-id>", errUsage)
	}
	config, err := parseMCPFlags("run rerun", options.transport, 0)
	if err != nil {
		return err
	}
	toolArguments := map[string]any{"id": options.positional[0]}
	if reason := strings.TrimSpace(options.reason); reason != "" {
		toolArguments["reason"] = reason
	}
	if options.force {
		toolArguments["force"] = true
	}
	raw, err := remoteRunCanceller{config: config}.call(ctx, "run_rerun", toolArguments)
	if err != nil {
		return err
	}
	return writeRunRerun(output, raw, options.asJSON)
}

func writeRunRerun(output io.Writer, raw json.RawMessage, asJSON bool) error {
	var response api.RunRerunResponse
	if asJSON || json.Unmarshal(raw, &response) != nil || response.Mode == "" {
		_, err := fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
		return err
	}
	var line string
	if response.Mode == "publish_retry" {
		line = fmt.Sprintf("%s  publication retried (%s): %s", response.RerunOf, response.PublicationID, response.Status)
		if response.Error != "" {
			line += "\n  error: " + response.Error
		}
	} else {
		line = fmt.Sprintf("%s  %s  %s  %s  (rerun of %s)", response.RunID, response.Status, shortSHA(response.SHA), response.Ref, response.RerunOf)
	}
	if response.Warning != "" {
		line += "\n  warning: " + response.Warning
	}
	_, err := fmt.Fprintln(output, line)
	return err
}
