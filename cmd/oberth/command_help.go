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
	"branch":      "prune|deletions",
	"fragments":   "list|show",
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
