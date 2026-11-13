package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// Groups without their own flag set still offer successful, offline help.
var commandGroups = map[string]string{
	"access":      "list|allow|revoke",
	"branch":      "prune|prune-refs|deletions",
	"preflight":   "drift|pki|upstreams",
	"release-wif": "list|grant|revoke|reset",
	"repo":        "add|remove|archive|unarchive|verify",
	"resource":    "grant|get|revoke",
	"secretstore": "setup|verify|sync|exec|bootstrap-tls",
	"size":        "grant|get|revoke",
	"uplink":      "add|list|remove|rotate|scope",
	"upstream":    "add|list|remove|provide-key",
}

func commandFlagHelp(flags *flag.FlagSet, usage string) error {
	var out strings.Builder
	fmt.Fprintln(&out, usage)
	flags.SetOutput(&out)
	flags.PrintDefaults()
	flags.SetOutput(io.Discard)
	return commandHelpError{usage: strings.TrimSpace(out.String())}
}

const gitSSHHelp = `Usage: oberth git-ssh [SSH transport arguments]
Git invokes this pinned-key SSH transport through GIT_SSH.
It uses OBERTH_GIT_SSH_CONFIG and accepts only the configured native Git service.
`

// printFlagUsage prints a command's flag help in the one format every command
// uses: "Usage: oberth <command> [flags]" and the flags. A flag set with its
// own Usage function calls that instead.
func printFlagUsage(flags *flag.FlagSet) {
	_, _ = fmt.Fprintf(flags.Output(), "Usage: oberth %s [flags]\n\nFlags:\n", flags.Name())
	flags.PrintDefaults()
}
