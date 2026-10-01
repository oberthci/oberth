package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/cron"
)

const FileName = ".oberth/schedule.yaml"

const MaxSourceBytes = 1 << 20

const maxNameBytes = 64

type Limits struct {
	MinInterval time.Duration
	MaxEntries  int
}

type Entry struct {
	Name     string
	Cron     string
	Branch   string
	schedule cron.Schedule
}

type Refusal struct {
	Name   string
	Reason string
}

type File struct {
	Entries  []Entry
	Refused  []Refusal
	observed time.Time
}

type document struct {
	Schedules []documentEntry `json:"schedules"`
}

type documentEntry struct {
	Name   string `json:"name"`
	Cron   string `json:"cron"`
	Branch string `json:"branch,omitempty"`
}

func Parse(raw []byte, limits Limits, now time.Time) (File, error) {
	if len(raw) > MaxSourceBytes {
		return File{}, fmt.Errorf("schedule: document exceeds the %d byte limit", MaxSourceBytes)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return File{}, errors.New("schedule: document is empty")
	}
	var parsed document
	if err := yaml.UnmarshalStrict(raw, &parsed); err != nil {
		return File{}, fmt.Errorf("schedule: decode: %w", err)
	}
	if len(parsed.Schedules) == 0 {
		return File{}, errors.New("schedule: document declares no schedules")
	}
	if limits.MaxEntries > 0 && len(parsed.Schedules) > limits.MaxEntries {
		return File{}, fmt.Errorf("schedule: document declares %d entries, the limit is %d",
			len(parsed.Schedules), limits.MaxEntries)
	}

	file := File{observed: now.UTC()}
	seen := map[string]bool{}
	for index, entry := range parsed.Schedules {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			return File{}, fmt.Errorf("schedule: entry %d has no name", index)
		}
		if len(name) > maxNameBytes {
			return File{}, fmt.Errorf("schedule: entry %q has a name longer than %d bytes", name, maxNameBytes)
		}
		if seen[name] {
			return File{}, fmt.Errorf("schedule: entry %q is declared more than once", name)
		}
		seen[name] = true
		if err := validBranch(entry.Branch); err != nil {
			return File{}, fmt.Errorf("schedule: entry %q: %w", name, err)
		}
		if strings.TrimSpace(entry.Cron) == "" {
			return File{}, fmt.Errorf("schedule: entry %q has no cron expression", name)
		}

		compiled, err := cron.Parse(entry.Cron)
		if err != nil {
			file.Refused = append(file.Refused, Refusal{Name: name, Reason: err.Error()})
			continue
		}
		interval, err := compiled.Interval(now)
		if err != nil {
			file.Refused = append(file.Refused, Refusal{Name: name, Reason: err.Error()})
			continue
		}
		if limits.MinInterval > 0 && interval < limits.MinInterval {
			file.Refused = append(file.Refused, Refusal{
				Name: name,
				Reason: fmt.Sprintf("fires every %s, the minimum interval is %s",
					interval, limits.MinInterval),
			})
			continue
		}
		file.Entries = append(file.Entries, Entry{
			Name: name, Cron: entry.Cron, Branch: strings.TrimSpace(entry.Branch), schedule: compiled,
		})
	}
	return file, nil
}

func validBranch(branch string) error {
	trimmed := strings.TrimSpace(branch)
	if trimmed == "" {
		return nil
	}
	if strings.ContainsAny(trimmed, "\x00\r\n\\ ") {
		return errors.New("branch contains forbidden characters")
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("branch %q is not a branch name", trimmed)
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("branch %q must not contain %q", trimmed, segment)
		}
	}
	return nil
}

func (file File) Due(last map[string]time.Time, now time.Time) []Entry {
	var due []Entry
	for _, entry := range file.Entries {
		previous, seen := last[entry.Name]
		if !seen {
			previous = file.observed
		}
		next, err := entry.schedule.Next(previous)
		if err != nil {
			continue
		}
		if !next.After(now) {
			due = append(due, entry)
		}
	}
	return due
}

func (entry Entry) Next(after time.Time) (time.Time, error) {
	return entry.schedule.Next(after)
}
