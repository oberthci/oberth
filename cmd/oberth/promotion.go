package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const promotionRetryUsage = "Usage: oberth promotion retry <promotion-id> [--json] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]"

// runPromotion serves `oberth promotion retry`, the CLI form of MCP
// promote_retry (#1097): a failed or interrupted promotion is re-run as a new
// promotion of the same tuple, merged onto the target's current head and
// re-tested.
func runPromotion(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 || arguments[0] == "--help" || arguments[0] == "-h" {
		_, err := fmt.Fprintln(output, promotionRetryUsage)
		if len(arguments) == 0 && err == nil {
			return fmt.Errorf("%w: promotion retry <promotion-id>", errUsage)
		}
		return err
	}
	if arguments[0] != "retry" {
		return fmt.Errorf("%w: unknown promotion command %q; expected retry", errUsage, arguments[0])
	}
	arguments = arguments[1:]
	if len(arguments) == 1 && (arguments[0] == "--help" || arguments[0] == "-h") {
		_, err := fmt.Fprintln(output, promotionRetryUsage)
		return err
	}
	forwarded := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		// The MCP tool already answers in JSON; consume the shared flag.
		if argument != "--json" && argument != "-json" {
			forwarded = append(forwarded, argument)
		}
	}
	if len(forwarded) == 0 || strings.HasPrefix(forwarded[0], "-") {
		return fmt.Errorf("%w: promotion retry <promotion-id>", errUsage)
	}
	encoded, err := json.Marshal(map[string]string{"id": forwarded[0]})
	if err != nil {
		return err
	}
	return runMCPCall(ctx, append([]string{"promote_retry", string(encoded)}, forwarded[1:]...), output)
}
