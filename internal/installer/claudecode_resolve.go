package installer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/client"
)

// ClientSetupFlags are `oberth client setup`'s flags. Every empty field falls
// back to the CLI's existing configuration on this machine.
type ClientSetupFlags struct {
	ClientCert  string
	ClientKey   string
	URL         string
	CAFile      string
	TokenFile   string
	CATrust     string
	Marketplace string
	DryRun      bool
}

// ResolveClaudeCodeSetup turns the flags into a ClaudeCodeSetup. The server
// address, the CA and the token come from the one resolver every remote
// command uses (client.ResolveConnection): flags, then the environment, then
// the client files `oberth install` writes, whose token and CA are used only
// for the server those files name.
//
// The token is optional: without one the plugin asks for it in Claude Code.
// It is not read here; the returned setup reads it only when it is about to
// be saved. An explicit --token-file that cannot be used is an error.
func ResolveClaudeCodeSetup(flags ClientSetupFlags) (ClaudeCodeSetup, []string, error) {
	var notes []string
	setup := ClaudeCodeSetup{CATrust: flags.CATrust, Marketplace: strings.TrimSpace(flags.Marketplace), DryRun: flags.DryRun}
	if setup.CATrust == "" {
		setup.CATrust = CATrustSettings
	}
	if !validCATrust(setup.CATrust) {
		return setup, nil, fmt.Errorf("--ca-trust must be one of %s", strings.Join(CATrustChoices, ", "))
	}
	connection, err := client.ResolveConnection(client.ConnectionFlags{ClientCert: flags.ClientCert, ClientKey: flags.ClientKey, URL: flags.URL, TokenFile: flags.TokenFile, CAFile: flags.CAFile})
	if connection.MCPURL == "" {
		if err == nil {
			err = client.ErrNotConfigured
		}
		return setup, nil, err
	}
	if _, urlErr := client.ValidateServerURL(connection.MCPURL); urlErr != nil {
		return setup, nil, urlErr
	}
	setup.URL = connection.MCPURL
	setup.CA = connection.CACert
	setup.ClientCert, setup.ClientKey = connection.ClientCert, connection.ClientKey
	if err == nil && connection.ClientCert != "" {
		return setup, notes, nil
	}
	switch {
	case err != nil && strings.TrimSpace(flags.TokenFile) != "":
		return setup, nil, err
	case err != nil:
		// No usable credential on this machine: the plugin asks for it.
		notes = append(notes, "no token is saved for the plugin: "+err.Error())
	default:
		config := connection.Config()
		setup.Token = func(ctx context.Context) (string, error) {
			token, err := config.ResolveToken(ctx)
			if err != nil {
				return "", err
			}
			if strings.ContainsAny(token, "\r\n") {
				return "", errors.New("the token source printed more than one line")
			}
			return token, nil
		}
		setup.ReplaceToken = strings.TrimSpace(flags.TokenFile) != ""
	}
	return setup, notes, nil
}

func validCATrust(value string) bool {
	for _, choice := range CATrustChoices {
		if value == choice {
			return true
		}
	}
	return false
}
