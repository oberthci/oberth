package installer

import (
	"fmt"
	"strings"
)

// ClientAccessChoices is the vocabulary of Config.ClientAccess and of
// `oberth install --client-access`, in offer order.
var ClientAccessChoices = []string{"both", "cli", "mcp", "none"}

// ValidClientAccess reports whether value is empty (ask) or one of
// ClientAccessChoices.
func ValidClientAccess(value string) error {
	if value == "" {
		return nil
	}
	for _, choice := range ClientAccessChoices {
		if value == choice {
			return nil
		}
	}
	return fmt.Errorf("client access must be one of %s", strings.Join(ClientAccessChoices, ", "))
}

// clientAccessDecided maps a choice made in advance onto the offer's options.
func clientAccessDecided(cfg Config) (int, bool) {
	switch cfg.ClientAccess {
	case "both":
		return clientAccessBoth, true
	case "cli":
		return clientAccessCLI, true
	case "mcp":
		return clientAccessMCP, true
	case "none":
		return clientAccessNeither, true
	}
	return 0, false
}

// ClientConfigDir is where the CLI and MCP client files live
// ($XDG_CONFIG_HOME/oberth, else ~/.config/oberth).
func ClientConfigDir() (string, error) { return clientConfigRoot() }

// TokenCommands returns the command the client files use to read the bearer
// token on this host, and the command that stores it there.
func TokenCommands() (read, store string) { return tokenCommandForHost() }
