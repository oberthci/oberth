package main

import (
	"context"
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
	var repo string
	var transport []string
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		if !strings.HasPrefix(argument, "-") {
			if repo != "" {
				return fmt.Errorf("%w: %s", errUsage, usage)
			}
			repo = argument
			continue
		}
		name, value, found := strings.Cut(argument, "=")
		switch name {
		case "--url", "--token-file", "--ca-file":
			if !found {
				i++
				if i >= len(arguments) {
					return fmt.Errorf("%w: %s requires a value", errUsage, name)
				}
				value = arguments[i]
			}
			transport = append(transport, name+"="+value)
		default:
			return fmt.Errorf("%w: unknown flag %q", errUsage, argument)
		}
	}
	if repo == "" {
		return fmt.Errorf("%w: %s", errUsage, usage)
	}
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
