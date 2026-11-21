package main

import (
	"encoding/json"

	"github.com/oberthci/oberth/internal/apicontract"
)

// grantPlaceholders names each access selector's value in a usage line.
func grantPlaceholders() map[string]string {
	return map[string]string{"workflow": "<file>", "step": "<step>", "secret": "<path>", "audience": "<audience>", "claim": "<claim>", "tier": "<ci|release>"}
}

// grantArgumentProblems states, by flag, the first selector the grant kind
// needs or does not take, then the usage line for that kind, using the
// contract's one table. An unknown kind is left to the server's refusal.
func grantArgumentProblems(command, kind string, given func(string) bool) []string {
	name, missing := apicontract.GrantArgumentProblem(kind, given)
	if name == "" {
		return nil
	}
	if kind == "" {
		kind = "store_path"
	}
	takes, _ := apicontract.GrantArguments(kind)
	usage := "usage: oberth " + command
	if command != "access allow" {
		usage += " <repo>"
	}
	if kind != "store_path" {
		usage += " --kind " + kind
	}
	if command == "access allow" {
		// access allow takes no flag after a positional name, so its line gives every selector as a flag.
		usage += " --repo <repo>"
	}
	for _, selector := range takes[1:] {
		usage += " --" + selector + " " + grantPlaceholders()[selector]
	}
	problem := "--" + name + " does not apply to kind " + kind
	if missing {
		problem = "missing --" + name + " for kind " + kind
	}
	return []string{problem, usage}
}

// toolGrantProblems applies the table to access revoke's tool arguments.
func toolGrantProblems(row command, values map[string]json.RawMessage) []string {
	if row.definition.Name != "access_revoke" {
		return nil
	}
	text := func(name string) string {
		var value string
		_ = json.Unmarshal(values[name], &value)
		return value
	}
	return grantArgumentProblems(row.path, text("kind"), func(name string) bool { return text(name) != "" })
}
