package main

import (
	"encoding/json"
	"flag"
	"github.com/oberthci/oberth/internal/apicontract"
	"reflect"
	"slices"
	"strings"
)

type commandSyntax struct {
	Summary string         `json:"summary"`
	Group   string         `json:"group"`
	Args    [2]int         `json:"args"`
	Flags   map[string]int `json:"flags"`
}

// commandCatalogue projects the parsers' declarations; there is no saved list.
func commandCatalogue() map[string]commandSyntax {
	rows := map[string]commandSyntax{}
	for _, row := range commands() {
		if row.path != "" {
			rows[row.path] = commandFlagSyntax(row)
		}
	}
	for _, flow := range flowCommands() {
		rows[flow.name] = commandSyntax{Args: [2]int{flow.minArgs, flow.maxArgs}, Flags: transportSyntax(flagSyntax(flowFlags(flow.name))), Summary: flow.summary}
	}
	for _, row := range installationCommands() {
		rows[row.name] = commandSyntax{Args: [2]int{row.minArgs, row.maxArgs}, Flags: helpSyntax(flagSyntax(installationFlags(row.name))), Summary: row.summary}
	}
	rows["version"] = commandSyntax{Summary: "Print the build identity or database schema.", Flags: helpSyntax(map[string]int{versionFlags()[0]: 0})}
	rows["mcp"] = commandSyntax{Summary: "Bridge agent tools over stdio with a pinned uplink profile.", Flags: helpSyntax(flagSyntax(mcpFlags()))}
	rows["serve"] = commandSyntax{Summary: "Start the configured server.", Flags: helpSyntax(map[string]int{"--config": 1})}
	rows["tool"] = commandSyntax{Summary: "Invoke a raw tool for agents or scripts.", Args: [2]int{1, 2}, Flags: transportSyntax(map[string]int{})}
	rows["tool list"] = commandSyntax{Summary: "List raw tool names.", Flags: helpSyntax(map[string]int{})}
	for name, row := range rows {
		row.Group = commandGroup(name)
		rows[name] = row
	}
	return rows
}
func commandFlagSyntax(row command) commandSyntax {
	syntax := commandSyntax{Summary: row.definition.Summary, Flags: transportSyntax(map[string]int{})}
	for _, field := range helpFields(row) {
		if field.sensitive {
			continue
		}
		arity := 1
		if argumentType(field).Kind() == reflect.Bool {
			arity = 0
		}
		if !field.positional {
			syntax.Flags["--"+field.flag] = arity
		}
		if field.position > 0 {
			syntax.Args[1]++
			if field.required {
				syntax.Args[0]++
			}
		}
	}
	if row.definition.Name == "uplink_add" {
		for _, flag := range bearerFlags(&commonOptions{}) {
			syntax.Flags["--"+flag.name] = 1
		}
	}
	return syntax
}

func flagSyntax(flags *flag.FlagSet) map[string]int {
	result := map[string]int{}
	flags.VisitAll(func(f *flag.Flag) {
		arity := 1
		if boolean, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			arity = 0
		}
		result["--"+f.Name] = arity
	})
	return result
}
func (a application) catalogue() int {
	if err := json.NewEncoder(a.out).Encode(map[string]any{"commands": commandCatalogue()}); err != nil {
		return a.failed(err)
	}
	return 0
}

func catalogueRequested(options commonOptions, rest, problems []string) bool {
	return options.json && len(rest) == 1 && rest[0] == "help" && len(problems) == 0
}

func helpRequested(options commonOptions, rest []string) bool {
	return len(rest) == 0 || rest[0] == "help" || options.help
}

func transportSyntax(flags map[string]int) map[string]int {
	for _, field := range transportFlags(&commonOptions{}) {
		flags["--"+field.name] = 1
	}
	flags["--detail"], flags["--json"], flags["--help"] = 0, 0, 0
	flags["-o"], flags["-h"] = 1, 0
	return flags
}

func helpSyntax(flags map[string]int) map[string]int {
	flags["--help"], flags["-h"] = 0, 0
	return flags
}

func commandGroup(name string) string {
	if slices.Contains(gettingStartedCommands(), name) {
		return "Getting started"
	}
	for _, row := range commands() {
		if row.definition.Command == name && row.definition.Access != apicontract.Read {
			return "Administration"
		}
	}
	switch strings.Fields(name)[0] {
	case "admin", "access", "backup", "secret", "secretstore", "serve", "unseal", "upgrade", "rollback", "invite", "uplink", "repo":
		return "Administration"
	}
	return "Daily use"
}

func gettingStartedCommands() []string {
	return []string{"init", "install", "repo add", "invite", "join", "validate", "version"}
}

func (a application) catalogueHelp() int {
	rows := commandCatalogue()
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, group := range []string{"Getting started", "Daily use", "Administration"} {
		if err := writeWrapped(a.out, group+":"); err != nil {
			return 1
		}
		groupNames := names
		if group == "Getting started" {
			groupNames = gettingStartedCommands()
		}
		for _, name := range groupNames {
			row := rows[name]
			if row.Group == group {
				if err := writeWrapped(a.out, name+": "+row.Summary); err != nil {
					return 1
				}
			}
		}
	}
	if err := writeWrapped(a.out, "Use help <command> for arguments and transport flags."); err != nil {
		return 1
	}
	return 0
}
