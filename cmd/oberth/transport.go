package main

import (
	"context"
	"flag"

	"github.com/oberthci/oberth/internal/client"
)

// Transport flag descriptions, shared by every remote command so --help reads
// the same whether the command talks REST or MCP.
const (
	transportURLUsage       = "server address or its /mcp endpoint (default: OBERTH_MCP_URL, OBERTH_BASE_URL, ~/.config/oberth)"
	transportTokenFileUsage = "owner-only (0600) file holding the bearer token (default: OBERTH_TOKEN, OBERTH_TOKEN_COMMAND, ~/.config/oberth)" //nolint:gosec // G101: flag help text naming where the token comes from, not a credential
	transportCAFileUsage    = "PEM CA certificate for TLS verification (default: OBERTH_CA_CERT, ~/.config/oberth/ca.crt)"
)

// registerTransportFlags adds --url, --token-file and --ca-file to a remote
// command's flag set. Every remote command, REST or MCP, resolves them with
// client.ResolveConnection: flag, then environment, then the client files.
func registerTransportFlags(flags *flag.FlagSet) *client.ConnectionFlags {
	var connection client.ConnectionFlags
	flags.StringVar(&connection.URL, "url", "", transportURLUsage)
	flags.StringVar(&connection.TokenFile, "token-file", "", transportTokenFileUsage)
	flags.StringVar(&connection.CAFile, "ca-file", "", transportCAFileUsage)
	return &connection
}

// remoteClient opens the REST client of a remote command.
func remoteClient(ctx context.Context, flags *client.ConnectionFlags) (*client.Client, error) {
	var given client.ConnectionFlags
	if flags != nil {
		given = *flags
	}
	connection, err := client.ResolveConnection(given)
	if err != nil {
		return nil, err
	}
	return client.New(ctx, connection.Config())
}
