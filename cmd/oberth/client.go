package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/installer"
)

const clientHelp = `Usage: oberth client setup [flags]
Configure this machine's MCP client (Claude Code) for an Oberth server.
Use oberth client setup --help for its flags.`

const clientSetupUsage = `Usage: oberth client setup [flags]
Register Oberth as a user-scope Claude Code MCP server and trust its CA.

The server entry is an HTTP url, a headersHelper that reads the token when
Claude Code connects (no token is written to any file), and a 630000 ms
timeout so run_wait's 600 s server-side wait is not cut at 60 s. The CA is
trusted through env.NODE_EXTRA_CA_CERTS in ~/.claude/settings.json (other
keys are kept), or the operating system trust store with --ca-trust system.

Unset flags fall back to OBERTH_MCP_URL, OBERTH_BASE_URL, OBERTH_CA_CERT,
OBERTH_TOKEN_COMMAND, the files oberth install writes in ~/.config/oberth,
and the server Claude Code already has registered. A working headersHelper
already registered is kept unless --token-file or --token-command is given.
Running it again changes nothing. --dry-run prints what would change.`

// clientSetupDeps lets tests stub the claude command.
var clientSetupDeps = func(output io.Writer) installer.Deps { return installer.Deps{Output: output} }

func runClient(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: client setup", errUsage)
	}
	switch arguments[0] {
	case "--help", "-h":
		_, err := fmt.Fprintln(output, clientHelp)
		return err
	case "setup":
		return runClientSetup(ctx, arguments[1:], output)
	default:
		return fmt.Errorf("%w: unknown client command %q", errUsage, arguments[0])
	}
}

func runClientSetup(ctx context.Context, arguments []string, output io.Writer) error {
	var options installer.ClientSetupFlags
	flags := flag.NewFlagSet("client setup", flag.ContinueOnError)
	flags.StringVar(&options.URL, "url", "", "server MCP endpoint or base address, e.g. https://localhost:30443/mcp")
	flags.StringVar(&options.CAFile, "ca-file", "", "the deployment's CA certificate (PEM)")
	flags.StringVar(&options.TokenFile, "token-file", "", "owner-only (0600) file holding the bearer token; the headersHelper reads it")
	flags.StringVar(&options.TokenCommand, "token-command", "", "command that prints the bearer token; the headersHelper runs it")
	flags.StringVar(&options.CATrust, "ca-trust", installer.CATrustSettings, "how Claude Code trusts the CA: "+strings.Join(installer.CATrustChoices, ", "))
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
	deps := clientSetupDeps(output)
	if setup.DryRun {
		_, _ = fmt.Fprintln(output, "Dry run: nothing is changed.")
	}
	changes, setupErr := installer.SetupClaudeCode(ctx, deps, setup)
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
	if !changed {
		_, _ = fmt.Fprintln(output, "No change: Claude Code is already configured.")
	}
	if setup.DryRun {
		return nil
	}
	status, err := installer.VerifyClaudeCode(ctx, deps)
	if err != nil {
		return errors.Join(errors.New("the server is registered but Claude Code does not connect"), err)
	}
	_, err = fmt.Fprintf(output, "claude mcp get oberth: %s\n", status)
	return err
}
