package main

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"

	"fmt"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"slices"
)

type commonOptions struct {
	bearerFile         string
	transport          client.Options
	resultType         reflect.Type
	detail, json, help bool
}

type transportFlag struct {
	name, kind, description string
	value                   *string
}

func transportFlags(o *commonOptions) []transportFlag {
	return []transportFlag{
		{"url", "URL", "Installation's server URL.", &o.transport.URL},
		{"token-file", "path", "Private bearer file.", &o.transport.TokenFile},
		{"ca-file", "path", "Pinned CA file.", &o.transport.CAFile},
		{"client-cert", "path", "Client certificate file.", &o.transport.ClientCert},
		{"client-key", "path", "Private client key file.", &o.transport.ClientKey},
	}
}

func bearerFlags(o *commonOptions) []transportFlag {
	return []transportFlag{{"bearer-file", "path", "Save a new bearer privately before activating its digest.", &o.bearerFile}}
}

func parseCommon(args []string) (commonOptions, []string, []string) {
	var o commonOptions
	var rest, problems []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--detail":
			o.detail = true
			continue
		case "--help", "-h":
			o.help = true
			continue
		case "--json":
			o.json = true
			continue
		case "-o":
			if i+1 < len(args) && args[i+1] == "json" {
				o.json = true
				i++
			} else {
				problems = append(problems, "-o requires json")
			}
			continue
		}
		next, matched, problem := commonFlag(&o, args, i)
		i = next
		if problem != "" {
			problems = append(problems, problem)
		}
		if !matched {
			rest = append(rest, arg)
		}
	}
	if problem := bearerProblem(o, rest); problem != "" {
		problems = append(problems, problem)
	}
	return o, rest, problems
}

func bearerProblem(o commonOptions, rest []string) string {
	if o.bearerFile == "" {
		return ""
	}
	row, _, ok := findCommand(commands(), rest)
	if !ok || row.definition.Name != "uplink_add" {
		return "--bearer-file is only valid for uplink add"
	}
	return ""
}

func commonFlag(o *commonOptions, args []string, i int) (int, bool, string) {
	name, value, has := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
	for _, f := range append(transportFlags(o), bearerFlags(o)...) {
		if !strings.HasPrefix(args[i], "--") || name != f.name {
			continue
		}
		if !has && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			i++
			value = args[i]
			has = true
		}
		if !has {
			return i, true, "--" + name + " requires a path or URL"
		}
		*f.value = value
		return i, true, ""
	}
	return i, false, ""
}

type argumentField struct {
	name, flag                      string
	field                           reflect.StructField
	required, sensitive, positional bool
	position                        int
}

func argumentFields(t reflect.Type) []argumentField {
	var fields []argumentField
	for _, f := range apicontract.Arguments(t) {
		fields = append(fields, argumentField{f.Name, strings.ReplaceAll(f.Name, "_", "-"), f.Field, f.Required, f.Sensitive, f.Positional, f.Position})
	}
	slices.SortStableFunc(fields, func(a, b argumentField) int { return a.position - b.position })
	return fields
}

func toolArgs(row command, args []string, in io.Reader, problems []string) ([]byte, []string) {
	if row.raw || len(args) == 1 && args[0] == "-" {
		return rawArgs(row, args, in, problems)
	}
	fields := argumentFields(row.definition.InputType)
	values := map[string]json.RawMessage{}
	var positionals []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positionals = append(positionals, args[i])
			continue
		}
		var more []string
		i, more = argumentFlag(fields, values, args, i)
		problems = append(problems, more...)
	}

	problems = positionalArguments(fields, values, positionals, problems)
	if len(problems) == 0 {
		problems = toolGrantProblems(row, values)
	}
	data, _ := json.Marshal(values)
	return data, problems
}

func positionalArguments(fields []argumentField, values map[string]json.RawMessage, positionals, problems []string) []string {
	for _, field := range fields {
		if field.position > 0 && len(values[field.name]) == 0 && len(positionals) > 0 {
			problems = setArgument(values, field, positionals[0], problems)
			positionals = positionals[1:]
		}
		if field.required && len(values[field.name]) == 0 {
			problems = append(problems, "missing "+field.usage())
		}
	}
	if len(positionals) > 0 {
		problems = append(problems, "unexpected positional arguments: "+strings.Join(positionals, ", ")+"; use named flags")
	}
	return problems
}

func argumentFlag(fields []argumentField, values map[string]json.RawMessage, args []string, i int) (int, []string) {
	name, value, has := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
	field, ok := findField(fields, name)
	if !ok {
		end := i
		if !has && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			end++
		}
		return end, []string{"unknown flag " + args[i] + "; use the flags in command help"}
	}
	if !has && argumentType(field).Kind() == reflect.Bool {
		if next := explicitBoolean(args, i); next != "" {
			return i + 1, []string{"Use --" + name + "=" + next + " for a boolean value."}
		}
		value, has = "true", true
	}
	if !has && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
		i++
		value, has = args[i], true
	}
	if !has {
		return i, []string{"--" + name + " requires a value"}
	}
	return i, setArgument(values, field, value, nil)
}

func findField(fields []argumentField, flag string) (argumentField, bool) {
	for _, f := range fields {
		if f.flag == flag && !f.positional {
			return f, true
		}
	}
	return argumentField{}, false
}

// usage names the field as the command line takes it.
func (f argumentField) usage() string {
	if f.positional {
		return "<" + f.flag + ">"
	}
	return "--" + f.flag
}

func argumentType(f argumentField) reflect.Type {
	t := f.field.Type
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func privateField(f argumentField) bool {
	return f.sensitive
}

func setArgument(values map[string]json.RawMessage, f argumentField, text string, problems []string) []string {
	if privateField(f) {
		return append(problems, "private fields require tool input on stdin")
	}
	if len(values[f.name]) > 0 {
		return append(problems, "duplicate argument "+f.name)
	}
	var data []byte
	if argumentType(f).Kind() == reflect.String {
		data, _ = json.Marshal(text)
	} else {
		data = []byte(text)
	}
	value := reflect.New(f.field.Type)
	if json.Unmarshal(data, value.Interface()) != nil {
		return append(problems, fmt.Sprintf("invalid value for %s; expected %s", f.usage(), argumentKind(f)))
	}
	values[f.name] = data
	return problems
}

func rawArgs(row command, args []string, in io.Reader, problems []string) ([]byte, []string) {
	if len(args) == 0 {
		return []byte("{}"), problems
	}
	if len(args) != 1 {
		return nil, append(problems, "tool expects one JSON object or -")
	}
	data := []byte(args[0])
	if args[0] == "-" {
		var err error
		data, err = io.ReadAll(io.LimitReader(in, int64(row.definition.InputBytes)+1))
		if err != nil {
			return nil, append(problems, "cannot read tool input")
		}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, append(problems, "tool input must be a bounded JSON object")
	}
	for _, f := range argumentFields(row.definition.InputType) {
		if args[0] != "-" && privateField(f) && len(object[f.name]) > 0 {
			problems = append(problems, "private fields require tool input on stdin")
		}
	}
	return data, problems
}

func explicitBoolean(args []string, i int) string {
	if i+1 < len(args) && (args[i+1] == "true" || args[i+1] == "false") {
		return args[i+1]
	}
	return ""
}
