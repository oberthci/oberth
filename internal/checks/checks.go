// Package checks reads the structured results a step publishes as files under
// $OBERTH_ARTIFACTS/checks. A step's exit code says whether the run continues;
// a check says what the step concluded, in one line a reader can act on.
package checks

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

const (
	// MaxBytes bounds one check. A check is a line in a run view, not a report.
	MaxBytes = 4 * 1024
	// MaxPerRun bounds how many a run may publish, so a loop in a step cannot
	// turn the view into a log.
	MaxPerRun = 32
	// Prefix is the directory under the run's artifact root that holds them.
	Prefix = "checks/"
)

// ErrRefused reports a file under Prefix that is not a readable check. The
// file is named so its author can fix it; a refusal never fails the run.
var ErrRefused = errors.New("checks: refused")

// Check is one step's conclusion: a verdict a reader can scan and a summary
// that says why. Details carry whatever the step wants to keep for later.
type Check struct {
	Name    string          `json:"name"`
	Verdict string          `json:"verdict"`
	Summary string          `json:"summary"`
	Details json.RawMessage `json:"details,omitempty"`
	Step    string          `json:"step,omitempty"`
}

// IsCheck reports whether an artifact name is a candidate check: a JSON file
// directly under Prefix, so a step's own directories cannot be mistaken for one.
func IsCheck(name string) bool {
	rest, found := strings.CutPrefix(name, Prefix)
	return found && rest != "" && !strings.Contains(rest, "/") && path.Ext(rest) == ".json"
}

// Parse reads one check, refusing anything a run view cannot show.
func Parse(name string, body []byte) (Check, error) {
	if len(body) > MaxBytes {
		return Check{}, fmt.Errorf("%w: %s is %d bytes, over %d", ErrRefused, name, len(body), MaxBytes)
	}
	var check Check
	if err := json.Unmarshal(body, &check); err != nil {
		return Check{}, fmt.Errorf("%w: %s: %v", ErrRefused, name, err)
	}
	switch check.Verdict {
	case "pass", "warn", "fail":
	default:
		return Check{}, fmt.Errorf("%w: %s: verdict %q is not pass, warn or fail", ErrRefused, name, check.Verdict)
	}
	check.Name = strings.TrimSpace(check.Name)
	check.Summary = strings.TrimSpace(check.Summary)
	if check.Name == "" || check.Summary == "" {
		return Check{}, fmt.Errorf("%w: %s: a check needs a name and a summary", ErrRefused, name)
	}
	return check, nil
}
