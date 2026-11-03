package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
)

func runRepoArchive(ctx context.Context, action string, arguments []string, output io.Writer) error {
	usage := "Usage: oberth repo " + action + " <repository> [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]"
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, usage)
		return err
	}
	flags := flag.NewFlagSet("repo "+action, flag.ContinueOnError)
	transportFlags := mcpTransportFlags(flags)
	positional, _, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	if len(positional) != 1 || positional[0] == "" {
		return fmt.Errorf("%w: %s", errUsage, usage)
	}
	repo := positional[0]
	transport := transportFlags()
	config, err := parseMCPFlags("repo "+action, transport, 0)
	if err != nil {
		return err
	}
	raw, err := (remoteRunCanceller{config: config}).call(ctx, "repo_"+action, map[string]any{"repo": repo})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, strings.TrimSpace(string(raw)))
	return err
}
