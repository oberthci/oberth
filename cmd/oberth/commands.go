package main

import (
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/setuptui"
)

type command struct {
	path       string
	definition apicontract.Definition
	raw        bool
}

// Every declaration retains a raw tool entry; flows own their human action names.
func commands() []command {
	rows := []command{}
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		path := d.Command
		if d.CommandFlow || path == "-" {
			path = ""
		}
		rows = append(rows, command{path, d, false})
	}
	slices.SortFunc(rows, func(a, b command) int { return strings.Compare(a.path, b.path) })
	return rows
}

func findCommand(rows []command, words []string) (command, []string, bool) {
	for _, row := range slices.Backward(rows) {
		parts := strings.Fields(row.path)
		if row.path != "" && len(words) >= len(parts) && slices.Equal(parts, words[:len(parts)]) {
			return row, words[len(parts):], true
		}
		if len(words) >= 2 && words[0] == "tool" && words[1] == row.definition.Name {
			row.raw = true
			return row, words[2:], true
		}
	}
	return command{}, nil, false
}

func (a application) help(rows []command, words, problems []string) int {
	if len(problems) > 0 {
		return a.usage(problems)
	}
	if len(words) > 0 && words[0] == "help" {
		words = words[1:]
	}
	if len(words) > 0 {
		return a.helpCommand(rows, words)
	}
	return a.catalogueHelp()
}

func (a application) commandHelp(row command) int {
	fields := helpFields(row)
	lines := []string{commandSynopsis(row, fields), row.definition.Summary}
	if row.definition.Name == "run_wait" {
		lines = append(lines, "Exit codes: 0 observed (including timeout); 1 failed/cancelled run or refusal; 2 usage; 130 interrupted.")
	}
	sensitive := false
	for _, f := range fields {
		lines = append(lines, argumentHelp(f))
		sensitive = sensitive || f.sensitive
	}
	if row.definition.Name == "uplink_add" {
		for _, flag := range bearerFlags(&commonOptions{}) {
			lines = append(lines, "--"+flag.name+" ("+flag.kind+"): "+flag.description)
		}
	}
	for _, flag := range transportFlags(&commonOptions{}) {
		lines = append(lines, "--"+flag.name+" ("+flag.kind+"): "+flag.description)
	}
	lines = append(lines, "--detail: show full detail", "--json, -o json: print the JSON result", "--help, -h: show this help")
	if sensitive {
		lines = append(lines, "Use oberth "+displayPath(row)+" - to read private fields from stdin JSON.")
	}
	for _, line := range lines {
		if err := writeWrapped(a.out, line); err != nil {
			return 1
		}
	}
	return 0
}
func helpFields(row command) []argumentField {
	return argumentFields(row.definition.InputType)
}

func commandSynopsis(row command, fields []argumentField) string {
	synopsis := "oberth " + displayPath(row)
	if row.raw {
		return synopsis + " [JSON|-] [flags]"
	}
	for _, f := range fields {
		if f.position > 0 {
			synopsis += " <" + f.flag + ">"
		}
	}
	if len(fields) > 0 {
		synopsis += " [flags]"
	}
	return synopsis
}
func argumentHelp(f argumentField) string {
	kind := argumentKind(f)
	if value := f.field.Tag.Get("default"); value != "" {
		kind += ", default " + value
	}
	if f.required {
		kind += ", required"
	}
	if f.sensitive {
		kind += ", stdin JSON only"
	}
	name := f.usage()
	if f.sensitive {
		name = f.flag
	}
	return name + " (" + kind + "): " + cliWords(f.field.Tag.Get("desc"))
}

func argumentKind(f argumentField) string {
	switch argumentType(f).Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int64:
		return "integer"
	case reflect.Slice, reflect.Array:
		return "JSON array"
	case reflect.Struct, reflect.Map:
		return "JSON object"
	default:
		return "string"
	}
}

func cliWords(text string) string {
	paths := map[string]string{}
	for _, row := range commands() {
		paths[row.definition.Name] = displayPath(row)
	}
	token := regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]*`)
	return token.ReplaceAllStringFunc(text, func(word string) string {
		if path, ok := paths[word]; ok && strings.Contains(word, "_") {
			return path
		}
		return word
	})
}

// writeWrapped wraps prose and preserves copyable values on their own lines.
func writeWrapped(out io.Writer, text string) error {
	for _, paragraph := range strings.Split(text, "\n") {
		for _, line := range wrapParagraph(paragraph) {
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// wrapParagraph wraps at 80 columns between words only. When a paragraph must
// wrap, a path or address gets a line of its own, so the sentence wraps before
// and after it and the value stays whole and copyable.
func wrapParagraph(text string) []string {
	if lines := setuptui.CopyableLines(text); lines != nil {
		return lines
	}
	if len([]rune(text)) <= 80 {
		return []string{text}
	}
	var lines []string
	line := ""
	flush := func() {
		if line != "" {
			lines = append(lines, line)
			line = ""
		}
	}
	for _, word := range strings.Fields(text) {
		if pathLike(word) {
			flush()
			lines = append(lines, word)
			continue
		}
		if line != "" && len([]rune(line))+1+len([]rune(word)) > 80-min(2, len(lines)*2) {
			flush()
		}
		line = strings.TrimPrefix(line+" "+word, " ")
	}
	flush()
	for i := 1; i < len(lines); i++ {
		lines[i] = "  " + lines[i]
	}
	return lines
}

func pathLike(word string) bool {
	return strings.HasPrefix(word, "/") || strings.HasPrefix(word, "~/") || strings.HasPrefix(word, "./") || strings.Contains(word, "://")
}

func unknownCommand(words []string) string {
	if len(words) == 0 {
		return "Missing command."
	}
	hint := "oberth help"
	text := strings.Join(words, " ")
	distance := 4
	for _, row := range commands() {
		if row.path == "" {
			continue
		}
		if delta := len(text) - len(row.path); delta > 3 || delta < -3 {
			continue
		}
		if d := commandDistance(text, row.path); d < distance {
			hint = "oberth help " + row.path
			distance = d
		}
	}
	return "Unknown command " + strings.Join(words, " ") + "." + commandHint(hint)
}

func commandDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for i := range previous {
		previous[i] = i
	}
	for i := range len(a) {
		next := make([]int, len(b)+1)
		next[0] = i + 1
		for j := range len(b) {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}
			next[j+1] = min(next[j]+1, previous[j+1]+1, previous[j]+cost)
		}
		previous = next
	}
	return previous[len(b)]
}

func (a application) helpCommand(rows []command, words []string) int {
	if len(words) == 1 && words[0] == "serve" {
		return a.serveHelp()
	}
	if code, ok := a.bespokeHelp(words); ok {
		return code
	}
	row, _, ok := findCommand(rows, words)
	if ok {
		return a.commandHelp(row)
	}
	found := false
	for _, child := range rows {
		if strings.HasPrefix(child.path, strings.Join(words, " ")+" ") {
			found = true
			if err := writeWrapped(a.out, "oberth "+child.path+": "+child.definition.Summary); err != nil {
				return 1
			}
		}
	}
	if found {
		return 0
	}
	return a.usage([]string{unknownCommand(words)})
}

func commandHint(hint string) string {
	if hint == "oberth help" {
		return ""
	}
	return " Did you mean oberth " + strings.TrimPrefix(hint, "oberth help ") + "?"
}
