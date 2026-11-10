package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/recordjson"
)

const publicationUsage = "Usage: oberth publication abandon <publication-id> --reason <text> [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]"

// Abandon must go through the live scheduler, which serializes it with delivery.
func runPublication(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, publicationUsage)
		return err
	}
	options, err := parseCancelOptions(arguments, false)
	if err != nil {
		return err
	}
	if len(options.positional) != 2 || options.positional[0] != "abandon" || strings.TrimSpace(options.positional[1]) == "" || strings.TrimSpace(options.reason) == "" || options.force || options.database != "" {
		return fmt.Errorf("%w: %s", errUsage, publicationUsage)
	}
	config, err := parseMCPFlags("publication abandon", options.transport, 0)
	if err != nil {
		return err
	}
	raw, err := (remoteRunCanceller{config: config}).call(ctx, "publication_abandon", map[string]any{"id": options.positional[1], "reason": strings.TrimSpace(options.reason)})
	if err != nil {
		return err
	}
	var result api.PublicationAbandonResponse
	if options.asJSON || recordjson.Unmarshal(raw, &result) != nil || result.PublicationID == "" {
		_, err = fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
		return err
	}
	_, err = fmt.Fprintf(output, "%s  %s  %s\n%s\n", result.PublicationID, result.Status, result.Reason, result.Message)
	return err
}
