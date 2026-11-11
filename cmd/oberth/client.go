package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
)

const clientHelp = `Usage: oberth client <command> [flags]
Configure this machine's MCP client (Claude Code) for an Oberth server.

Commands:
  setup   trust the server's CA in Claude Code and install the Oberth plugin
  renew   renew with the current certificate and rotate the local key
  pin     print the SHA-256 pin of the server's CA, for /oberth:connect

Use oberth client <command> --help for its flags.`

const clientSetupUsage = `Usage: oberth client setup [flags]
Make Claude Code trust the Oberth server's CA and install the Oberth plugin.

The CA is stored as oberth/ca.crt in Claude Code's directory (~/.claude) and
env.NODE_EXTRA_CA_CERTS in its settings.json is made to hold it; other keys
and an existing bundle are kept, and the operating system's trust store is
never touched. The plugin (` + installer.ClaudePluginID + `) carries the
agent-loop skill and /oberth:connect. The configured user MCP entry uses the
client certificate and key, or an explicit bearer credential in a private
file read by a header helper. Unconfigured plugins create no MCP entry.

Unset flags fall back to OBERTH_MCP_URL, OBERTH_BASE_URL, OBERTH_CA_CERT,
OBERTH_TOKEN, OBERTH_TOKEN_COMMAND and the files oberth install writes in
~/.config/oberth. Running it again changes nothing. --dry-run prints what
would change. Restart Claude Code once afterwards.`

const clientPinUsage = `Usage: oberth client pin [--ca-file <path>]
Print the SHA-256 pin of the Oberth server's CA certificate: 64 hex digits.

Give it to the people who connect Claude Code with the Oberth plugin. Its
/oberth:connect trusts the CA the server presents only when the fingerprint
equals this pin. The CA is --ca-file, else OBERTH_CA_CERT, else the ca.crt
oberth install writes in ~/.config/oberth; the server is not contacted.`

// clientSetupDeps lets tests stub the claude command.
var clientSetupDeps = func(output io.Writer) installer.Deps { return installer.Deps{Output: output} }

func runClient(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: client setup|pin", errUsage)
	}
	switch arguments[0] {
	case "--help", "-h":
		_, err := fmt.Fprintln(output, clientHelp)
		return err
	case "setup":
		return runClientSetup(ctx, arguments[1:], output)
	case "renew":
		return runClientRenew(ctx, arguments[1:], output)
	case "pin":
		return runClientPin(arguments[1:], output)
	default:
		return fmt.Errorf("%w: unknown client command %q", errUsage, arguments[0])
	}
}

func runClientSetup(ctx context.Context, arguments []string, output io.Writer) error {
	var options installer.ClientSetupFlags
	flags := flag.NewFlagSet("client setup", flag.ContinueOnError)
	flags.StringVar(&options.ClientCert, "client-cert", "", "PEM client certificate")
	flags.StringVar(&options.ClientKey, "client-key", "", "owner-only PEM client private key")
	flags.StringVar(&options.URL, "url", "", "server MCP endpoint or base address, e.g. https://localhost:30443/mcp")
	flags.StringVar(&options.CAFile, "ca-file", "", "the deployment's CA certificate (PEM)")
	flags.StringVar(&options.TokenFile, "token-file", "", "owner-only (0600) file holding the bearer token; it replaces the private bearer file used by Claude Code")
	flags.StringVar(&options.CATrust, "ca-trust", installer.CATrustSettings, "how Claude Code trusts the CA: "+strings.Join(installer.CATrustChoices, ", "))
	flags.StringVar(&options.Marketplace, "marketplace", "", "plugin marketplace source: owner/repo, a git URL or a path (default "+installer.DefaultClaudePluginMarketplace+")")
	flags.BoolVar(&options.DryRun, "dry-run", false, "print what would change and change nothing")
	positional, help, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, clientSetupUsage)
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: client setup takes no arguments", errUsage)
	}

	setup, notes, err := installer.ResolveClaudeCodeSetup(options)
	if err != nil {
		return err
	}
	if setup.DryRun {
		_, _ = fmt.Fprintln(output, "Dry run: nothing is changed.")
	}
	changes, setupErr := installer.SetupClaudeCode(ctx, clientSetupDeps(output), setup)
	changed := false
	for _, change := range changes {
		_, _ = fmt.Fprint(output, change.String())
		changed = changed || change.Changed()
	}
	for _, note := range notes {
		_, _ = fmt.Fprintf(output, "Note: %s\n", note)
	}
	if setupErr != nil {
		return setupErr
	}
	switch {
	case !changed:
		_, err = fmt.Fprintln(output, "No change: Claude Code is already configured.")
	case !setup.DryRun:
		_, err = fmt.Fprintln(output, "Restart Claude Code to apply this.")
	}
	return err
}

func runClientPin(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("client pin", flag.ContinueOnError)
	caFile := flags.String("ca-file", "", "the deployment's CA certificate (PEM)")
	positional, help, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	if help {
		return commandFlagHelp(flags, clientPinUsage)
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: client pin takes no arguments", errUsage)
	}
	authority := strings.TrimSpace(*caFile)
	if authority == "" {
		authority = client.FromEnv().CACert
	}
	if authority == "" {
		root, err := client.ConfigDir()
		if err != nil {
			return err
		}
		if err := client.TrustClientFiles(root); err != nil {
			return fmt.Errorf("pass --ca-file: the client files were not read: %w", err)
		}
		authority = filepath.Join(root, "ca.crt")
		if _, err := os.Stat(authority); err != nil {
			return errors.New("no CA certificate: pass --ca-file, set OBERTH_CA_CERT, or keep the ca.crt oberth install writes in ~/.config/oberth")
		}
	}
	pin, err := installer.CAPin(authority)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, pin)
	return err
}
