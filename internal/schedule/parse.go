// Package schedule reads bounded repository schedule declarations without firing work.
// Spec: docs/plans/c10-schedules.md#owned-data-and-public-contract
package schedule

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

const FileName = ".oberth/schedule.yaml"
const maxSourceBytes = 1 << 20

type Limits struct{ MaxEntries int }
type Entry struct {
	Name              string `yaml:"name"`
	Cron              string `yaml:"cron"`
	Branch            string `yaml:"branch,omitempty"`
	Timezone          string `yaml:"timezone,omitempty"`
	ConcurrencyPolicy string `yaml:"concurrencyPolicy,omitempty"`
	expression        string
}
type Refusal struct {
	Name, Branch string
	Error        error
}
type File struct {
	Entries []Entry
	Refused []Refusal
}
type declaration struct {
	Entry   `yaml:",inline"`
	Unknown map[string]yaml.Node `yaml:",inline"`
}
type document struct {
	Schedules []declaration        `yaml:"schedules"`
	Unknown   map[string]yaml.Node `yaml:",inline"`
}

func (l Limits) Validate() error {
	if l.MaxEntries < 0 || l.MaxEntries > 256 {
		return invalid(invalidLimits, fmt.Sprintf("setting schedules.maxEntries has value %d: use 1 through 256, or 0 for the default 8.", l.MaxEntries))
	}
	return nil
}

// Parse reports every decodable offender while retaining valid siblings of cron refusals.
func Parse(raw []byte, limits Limits, now time.Time) (File, error) {
	doc, err := decode(raw, limits)
	if err != nil {
		return File{}, err
	}
	fatal := unknownFields("file", doc.Unknown)
	cap := limits.MaxEntries
	if cap == 0 {
		cap = 8
	}
	if len(doc.Schedules) == 0 {
		fatal = append(fatal, invalid(emptyList, `file: field "schedules" has value []: add at least one entry.`))
	}
	if len(doc.Schedules) > cap {
		fatal = append(fatal, invalid(entryCount, fmt.Sprintf("file: field %q has value %d entries: the limit is %d.", "schedules", len(doc.Schedules), cap)))
	}
	all := slices.Clone(fatal)
	seen := map[string]bool{}
	result := File{}
	for index, item := range doc.Schedules {
		entry := item.Entry
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Branch = strings.TrimSpace(entry.Branch)
		if entry.Timezone == "" {
			entry.Timezone = "UTC"
		}
		label := fmt.Sprintf("entry #%d (%q)", index+1, entry.Name)
		zone, zoneError := entry.zone(label)
		issues := entry.validate(label, seen)
		if zoneError != nil {
			issues = append(issues, zoneError)
		}
		issues = append(issues, unknownFields(label, item.Unknown)...)
		fatal = append(fatal, issues...)
		all = append(all, issues...)
		parsed, issues := entry.compile(label, zone, now)
		if len(issues) > 0 {
			all = append(all, issues...)
			result.Refused = append(result.Refused, Refusal{entry.Name, entry.Branch, problems(issues)})
			continue
		}
		entry.expression = parsed.Expression()
		result.Entries = append(result.Entries, entry)
	}
	if len(fatal) > 0 {
		return File{}, problems(all)
	}
	return result, nil
}
func decode(raw []byte, limits Limits) (document, error) {
	if err := limits.Validate(); err != nil {
		return document{}, err
	}
	if len(raw) > maxSourceBytes {
		return document{}, invalid(documentSize, fmt.Sprintf("file: field %q has value %d bytes: the limit is %d.", "document", len(raw), maxSourceBytes))
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return document{}, invalid(documentSyntax, `file: field "document" has value "": write a schedules list.`)
	}
	var doc document
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return doc, invalid(documentSyntax, fmt.Sprintf("file: field %q: %v; fix YAML at the reported line.", "document", err))
	}
	return doc, nil
}
func (entry Entry) validate(label string, seen map[string]bool) []*publicerr.Error {
	var issues []*publicerr.Error
	if !validFact(entry.Name) || len(entry.Name) > 64 {
		issues = append(issues, entryName.fieldError(label, "name", entry.Name, "use 1 to 64 safe run-fact characters, starting with a letter or digit"))
	}
	if seen[entry.Name] {
		issues = append(issues, duplicateName.fieldError(label, "name", entry.Name, "choose a unique entry name"))
	}
	seen[entry.Name] = true
	if entry.Branch != "" {
		if err := sourcecontract.ValidatePublicRef("refs/heads/" + entry.Branch); err != nil {
			issues = append(issues, branchName.fieldError(label, "branch", entry.Branch, "use a public branch name such as main"))
		}
	}
	return issues
}
func (entry Entry) zone(label string) (*time.Location, *publicerr.Error) {
	if entry.Timezone == "Local" {
		return nil, unknownZone.fieldError(label, "timezone", entry.Timezone, "name a time zone or omit it for UTC")
	}
	zone, err := time.LoadLocation(entry.Timezone)
	if err != nil {
		return nil, unknownZone.fieldError(label, "timezone", entry.Timezone, "choose a time zone such as UTC or Europe/Amsterdam")
	}
	return zone, nil
}
func (entry Entry) compile(label string, zone *time.Location, now time.Time) (cronExpression, []*publicerr.Error) {
	var issues []*publicerr.Error
	if !validPolicy(entry.ConcurrencyPolicy) {
		issues = append(issues, concurrencyPolicy.fieldError(label, "concurrencyPolicy", entry.ConcurrencyPolicy, "use Allow, Forbid, Replace, or omit the field"))
	}
	parsed, err := parseCron(entry.Cron, zone)
	if err != nil {
		issues = append(issues, cronSyntax.fieldError(label, "cron", entry.Cron, err.Error()))
	} else if parsed.Next(now).IsZero() {
		issues = append(issues, calendarOccurrence.fieldError(label, "cron", entry.Cron, "choose a cron expression with a future calendar occurrence"))
	}
	return parsed, issues
}
func unknownFields(label string, fields map[string]yaml.Node) []*publicerr.Error {
	var issues []*publicerr.Error
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		node := fields[name]
		value := node.Value
		if node.Kind != yaml.ScalarNode {
			data, _ := yaml.Marshal(&node)
			value = strings.TrimSpace(string(data))
		}
		issues = append(issues, unknownField.fieldError(label, name, value, "remove this unknown field"))
	}
	return issues
}
func validPolicy(value string) bool {
	return value == "" || value == "Allow" || value == "Forbid" || value == "Replace"
}
func validFact(value string) bool {
	if value == "" {
		return false
	}
	for i, char := range value {
		alnum := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
		if !alnum && (i == 0 || !strings.ContainsRune("._/+@=,-", char)) {
			return false
		}
	}
	return true
}
func (e Entry) CronExpression() string { return e.expression }

// Next computes an advisory native time; it never arranges or fires work.
func (e Entry) Next(after time.Time) (time.Time, error) {
	zone, err := time.LoadLocation(e.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := parseCron(e.Cron, zone)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.Next(after), nil
}
