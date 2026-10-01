// Package runprogress defines the bounded, credential-free runner progress
// protocol used to expose a selected pipeline while its Job is still active.
package runprogress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	Version        = 1
	MarkerPrefix   = "@@oberth-step-progress@@ "
	MaxMarkerBytes = 16 << 10
)

type Status string

const (
	StepQueued   Status = "queued"
	StepRunning  Status = "running"
	StepPassed   Status = "passed"
	StepFailed   Status = "failed"
	StepSkipped  Status = "skipped"
	StepTimedOut Status = "timed_out"
)

func (status Status) terminal() bool {
	switch status {
	case StepPassed, StepFailed, StepSkipped, StepTimedOut:
		return true
	default:
		return false
	}
}

// Event is one state transition for a step in the selected, ordered pipeline.
// It deliberately excludes commands, arguments, environment, errors, and
// output so the progress channel can never carry credentials.
type Event struct {
	Version      int            `json:"version"`
	Burn         string         `json:"burn"`
	Step         string         `json:"step"`
	Ordinal      int            `json:"ordinal"`
	Status       Status         `json:"status"`
	DeclaredSize periapsis.Size `json:"declared_size,omitempty"`
	ExitCode     int            `json:"exit_code,omitempty"`
	StartedAt    *time.Time     `json:"started_at,omitempty"`
	FinishedAt   *time.Time     `json:"finished_at,omitempty"`
}

func (event Event) Validate() error {
	if event.Version != Version {
		return fmt.Errorf("unsupported progress version %d", event.Version)
	}
	if !validName(event.Burn) || !validName(event.Step) || event.Ordinal < 0 {
		return errors.New("progress burn, step, or ordinal is invalid")
	}
	if !event.DeclaredSize.Valid() {
		return fmt.Errorf("progress declared size %q is invalid", event.DeclaredSize)
	}
	switch event.Status {
	case StepQueued:
		if event.StartedAt != nil || event.FinishedAt != nil {
			return errors.New("queued progress cannot have timestamps")
		}
	case StepRunning:
		if event.StartedAt == nil || event.FinishedAt != nil {
			return errors.New("running progress requires only started_at")
		}
	case StepPassed, StepFailed, StepSkipped, StepTimedOut:
		if event.StartedAt == nil || event.FinishedAt == nil {
			return errors.New("terminal progress requires started_at and finished_at")
		}
		if event.FinishedAt.Before(*event.StartedAt) {
			return errors.New("progress finished_at precedes started_at")
		}
	default:
		return fmt.Errorf("unsupported progress status %q", event.Status)
	}
	return nil
}

func validName(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "\x00\r\n")
}

// MarshalMarker encodes one event as a single bounded runner-log record.
func MarshalMarker(event Event) ([]byte, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("encode step progress: %w", err)
	}
	if len(MarkerPrefix)+len(body)+1 > MaxMarkerBytes {
		return nil, errors.New("step progress marker exceeds its byte bound")
	}
	marker := make([]byte, 0, len(MarkerPrefix)+len(body)+1)
	marker = append(marker, MarkerPrefix...)
	marker = append(marker, body...)
	marker = append(marker, '\n')
	return marker, nil
}

// ParseMarker distinguishes ordinary runner output from a progress record and
// strictly decodes the latter. found remains true for malformed records so a
// protocol failure cannot silently become user log output.
func ParseMarker(line []byte) (event Event, found bool, err error) {
	if !bytes.HasPrefix(line, []byte(MarkerPrefix)) {
		return Event{}, false, nil
	}
	if len(line) > MaxMarkerBytes {
		return Event{}, true, errors.New("step progress marker exceeds its byte bound")
	}
	body := bytes.TrimSuffix(line[len(MarkerPrefix):], []byte("\n"))
	body = bytes.TrimSuffix(body, []byte("\r"))
	event, err = Decode(body)
	return event, true, err
}

// Decode strictly decodes one journal event.
func Decode(body []byte) (Event, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var event Event
	if err := decoder.Decode(&event); err != nil {
		return Event{}, fmt.Errorf("decode step progress: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Event{}, errors.New("step progress contains trailing JSON")
	}
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (event Event) Terminal() bool { return event.Status.terminal() }

// PlanVersion pins the durable plan record, and MaxPlanBytes bounds it. The
// plan is written once per run by the server, so the bound only has to admit a
// pipeline a human wrote; MaxPlannedSteps caps the entries long before this.
const (
	PlanVersion  = 1
	MaxPlanBytes = 256 << 10
)

// PlannedStep is one step the reviewed pipeline document declares.
//
// It deliberately carries no status. Every entry in a plan is by definition
// declared-and-not-reached, so a status field would be a place for a plan to
// claim an outcome. A planned step becomes an outcome only when a real
// observation or a durable result overwrites it.
type PlannedStep struct {
	Burn    string `json:"burn"`
	Step    string `json:"step"`
	Ordinal int    `json:"ordinal"`
}

// Plan is the durable record of what a run intended to do, written before its
// first Pod exists. It is the zeroth state of run progress: without it a run's
// step list can only ever describe what already happened.
type Plan struct {
	Version int           `json:"version"`
	Steps   []PlannedStep `json:"steps"`
}

func (plan Plan) Validate() error {
	if plan.Version != PlanVersion {
		return fmt.Errorf("unsupported plan version %d", plan.Version)
	}
	keys := make(map[string]struct{}, len(plan.Steps))
	ordinals := make(map[int]struct{}, len(plan.Steps))
	for index, step := range plan.Steps {
		if !validName(step.Burn) || !validName(step.Step) || step.Ordinal < 0 {
			return fmt.Errorf("plan entry %d has an invalid burn, step, or ordinal", index)
		}
		key := step.Burn + "\x00" + step.Step
		if _, duplicate := keys[key]; duplicate {
			return fmt.Errorf("plan declares %s/%s more than once", step.Burn, step.Step)
		}
		// A shared ordinal would make the plan's own ordering ambiguous, and
		// the run log's progress journal refuses the same thing for the same
		// reason.
		if _, duplicate := ordinals[step.Ordinal]; duplicate {
			return fmt.Errorf("plan reuses ordinal %d", step.Ordinal)
		}
		keys[key], ordinals[step.Ordinal] = struct{}{}, struct{}{}
	}
	return nil
}
