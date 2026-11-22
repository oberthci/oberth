package main

import "flag"

// flowFlags supplies the same native flag definitions to parsing and help.
func flowFlags(name string) *flag.FlagSet {
	if name == "access allow" {
		return grantFlags()
	}
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	switch name {
	case "issue import":
		f.Bool("dry-run", false, "Validate the entire file without writing.")
		f.String("upstream", "", "Map every source upstream to this registered local upstream.")
	case "invite":
		f.String("identity", "agent", "Invited identity.")
		f.String("out", "", "Private handoff file.")
		f.String("expires", "24h", "Invitation lifetime.")
	case "repo add":
		f.String("upstream", "", "Name of an existing or new upstream.")
		f.String("expected-host-fingerprint", "", "The forge's SSH host-key fingerprint, verified by the administrator.")
		f.Bool("repin", false, "Replace a changed forge host key after verification.")
		f.String("known-hosts", "", "Oberth server's SSH host keys, not the forge's; default: the selected profile's saved keys.")
	case "init":
		f.Bool("force", false, "Replace only the selected pipeline.")
		f.Bool("release", false, "Write the release pipeline.")
	case "check":
		f.String("repo", "", "Bound repository.")
		f.String("local", "", "Authored file to inspect.")
	case "promote":
		f.String("repo", "", "Bound repository.")
	case "log":
		f.String("pattern", "", "Line filter.")
		f.Bool("tail", false, "Read the end of the retained window.")
	}
	return f
}

func flagValue[T any](f *flag.FlagSet, name string) *T {
	value := f.Lookup(name).Value.(flag.Getter).Get().(T)
	return &value
}

func validFlowArgs(name string, args []string) bool {
	for _, row := range flowCommands() {
		if row.name == name {
			return len(args) >= row.minArgs && len(args) <= row.maxArgs
		}
	}
	return false
}
