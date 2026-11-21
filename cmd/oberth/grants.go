package main

import (
	"encoding/json"
	"flag"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
)

// Admission emits either repository/file/step/path or repository/claim recipes.
// Flags come from the same binding as raw tools; only this user flow maps recipes.
func grantPositions(kind string) []string {
	if kind == "pvc" {
		return []string{"repo", "claim"}
	}
	return []string{"repo", "workflow", "step", "secret"}
}
func grantFlags() *flag.FlagSet {
	f := flag.NewFlagSet("access allow", flag.ContinueOnError)
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		if d.Name != "access_allow" {
			continue
		}
		for _, a := range argumentFields(d.InputType) {
			f.String(a.flag, "", a.field.Tag.Get("desc"))
		}
	}
	return f
}
func allowAccess(s *flowSession, args []string) error {
	f := grantFlags()
	rest, err := parseFlow(f, args)
	if err != nil {
		return err
	}
	values := map[string]string{}
	f.Visit(func(entry *flag.Flag) { values[strings.ReplaceAll(entry.Name, "-", "_")] = entry.Value.String() })
	for _, name := range grantPositions(values["kind"]) {
		if _, set := values[name]; set {
			continue
		}
		if len(rest) == 0 {
			break
		}
		values[name], rest = rest[0], rest[1:]
	}
	if len(rest) != 0 {
		return usageProblems{"Too many grant arguments; run oberth help access allow."}
	}
	if problems := grantArgumentProblems("access allow", values["kind"], func(name string) bool { return values[name] != "" }); problems != nil {
		return usageProblems(problems)
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	var request apicontract.AccessAllowArgs
	if err = json.Unmarshal(raw, &request); err != nil {
		return err
	}
	if err = s.invoke("access_allow", request, nil); err != nil {
		return err
	}
	if s.options.json {
		return nil
	}
	return s.app.render(s.last, false, s.options)
}
