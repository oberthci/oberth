package main

import (
	"context"
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
	var id, what, reason string
	var transport []string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--json" {
			continue
		}
		if !strings.HasPrefix(argument, "-") {
			if id != "" {
				return fmt.Errorf("%w: %s", errUsage, runPurgeUsage)
			}
			id = argument
			continue
		}
		name, value, hasValue := strings.Cut(argument, "=")
		switch name {
		case "--what", "--reason", "--url", "--token-file", "--ca-file":
			if !hasValue {
				index++
				if index >= len(arguments) {
					return fmt.Errorf("%w: %s requires a value", errUsage, name)
				}
				value = arguments[index]
			}
			switch name {
			case "--what":
				what = value
			case "--reason":
				reason = value
			default:
				transport = append(transport, name+"="+value)
			}
		default:
			return fmt.Errorf("%w: unknown flag %q", errUsage, argument)
		}
	}
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
