package main

import (
	"flag"
	"slices"
	"strings"
)

func (a application) bespokeHelp(words []string) (int, bool) {
	if len(words) == 1 && words[0] == "tool" {
		return a.nativeHelp(nativeHelpRequest{"tool <name> [JSON|-]", commandCatalogue()["tool"].Summary, flag.NewFlagSet("tool", flag.ContinueOnError), true}), true
	}
	if slices.Equal(words, []string{"tool", "list"}) {
		return a.nativeHelp(nativeHelpRequest{"tool list", commandCatalogue()["tool list"].Summary, flag.NewFlagSet("tool list", flag.ContinueOnError), false}), true
	}

	for _, flow := range flowCommands() {
		if helpPrefix(words, flow.name) {
			if row, _, ok := findCommand(commands(), words); ok && len(strings.Fields(row.path)) > len(strings.Fields(flow.name)) {
				continue
			}
			return a.nativeHelp(nativeHelpRequest{flow.usage, flow.summary, flowFlags(flow.name), true}), true
		}
	}
	for _, row := range installationCommands() {
		if helpPrefix(words, row.name) {
			return a.nativeHelp(nativeHelpRequest{installationSynopsis(row.name), row.summary, installationFlags(row.name), false}), true
		}
	}
	if helpPrefix(words, "version") {
		name := "version"
		f := flag.NewFlagSet(name, flag.ContinueOnError)
		f.Bool(strings.TrimPrefix(versionFlags()[0], "--"), false, "Print the storage schema version.")
		return a.nativeHelp(nativeHelpRequest{name, commandCatalogue()["version"].Summary, f, false}), true
	}
	if helpPrefix(words, "mcp") {
		return a.nativeHelp(nativeHelpRequest{"mcp [flags]", commandCatalogue()["mcp"].Summary, mcpFlags(), false}), true
	}
	return 0, false
}

type nativeHelpRequest struct {
	synopsis, summary string
	flags             *flag.FlagSet
	transport         bool
}

func (a application) nativeHelp(h nativeHelpRequest) int {
	lines := []string{"oberth " + h.synopsis, h.summary}
	h.flags.VisitAll(func(f *flag.Flag) {
		kind, usage := flag.UnquoteUsage(f)
		if kind == "" {
			kind = "boolean"
		}
		lines = append(lines, "--"+f.Name+" ("+kind+"): "+usage)
	})
	if h.transport {
		for _, f := range transportFlags(&commonOptions{}) {
			lines = append(lines, "--"+f.name+" ("+f.kind+"): "+f.description)
		}
		lines = append(lines, "--detail: show full detail", "--json, -o json: print the JSON result")
	}
	lines = append(lines, "--help, -h: show this help")
	for _, line := range lines {
		if writeWrapped(a.out, line) != nil {
			return 1
		}
	}
	return 0
}
func helpPrefix(words []string, name string) bool {
	parts := strings.Fields(name)
	return len(words) >= len(parts) && slices.Equal(words[:len(parts)], parts)
}
