package main

import (
	"encoding/json"
	"flag"
	"slices"
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
	request, err := grantRequest(args)
	if err != nil {
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

// grantRequest reads the grant an access allow command names: selector flags
// first, then the names by position, each position skipped once its flag is set.
func grantRequest(args []string) (apicontract.AccessAllowArgs, error) {
	var request apicontract.AccessAllowArgs
	f := grantFlags()
	if err := selectorOrder(f, args, true, "reason"); err != nil {
		return request, err
	}
	rest, err := parseFlow(f, args)
	if err != nil {
		return request, err
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
		return request, usageProblems{"Too many grant arguments; run oberth help access allow."}
	}
	if problems := grantArgumentProblems("access allow", values["kind"], func(name string) bool { return values[name] != "" }); problems != nil {
		return request, usageProblems(problems)
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return request, err
	}
	err = json.Unmarshal(raw, &request)
	return request, err
}

// selectorOrder refuses a grant flag that is given twice or, when ordered,
// after a positional word. A pasted name that begins with "-" is read as a
// flag once the shell removes its quotes; in either place it would select
// another grant than the one printed. Flags named in free (such as a reason)
// may go anywhere.
func selectorOrder(f *flag.FlagSet, args []string, ordered bool, free ...string) error {
	seen := map[string]bool{}
	positional := false
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positional = true
			continue
		}
		name, _, attached := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		entry := f.Lookup(name)
		if entry == nil {
			continue
		}
		if !slices.Contains(free, name) && (ordered && positional || seen[name]) {
			return usageProblems{"Give each grant flag once, before the positional names; write a name that begins with - as its flag: --step='<name>'."}
		}
		seen[name] = true
		if boolean, ok := entry.Value.(interface{ IsBoolFlag() bool }); !attached && (!ok || !boolean.IsBoolFlag()) {
			i++
		}
	}
	return nil
}
