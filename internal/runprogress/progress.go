// Package runprogress reads stored historical step journals and plans.
package runprogress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// LogDeliveryPrefix identifies retained delivery observations outside command output.
const LogDeliveryPrefix = "::oberth-log:: "

const Version = 1

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
	Version    int        `json:"version"`
	Stage      string     `json:"burn"`
	Step       string     `json:"step"`
	Ordinal    int        `json:"ordinal"`
	Status     Status     `json:"status"`
	ExitCode   int        `json:"exit_code,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func (event Event) Validate() error {
	if event.Version != Version {
		return fmt.Errorf("unsupported progress version %d", event.Version)
	}
	if !validName(event.Stage) || !validName(event.Step) || event.Ordinal < 0 {
		return errors.New("progress burn, step, or ordinal is invalid")
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

// Decode strictly decodes one journal event.
func Decode(body []byte) (Event, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	// Old retained journals can contain the former class field. Consume it only
	// while reading history; the event type and all new writers have no class.
	var record struct {
		Event
		HistoricalClass json.RawMessage `json:"declared_size"`
	}
	if err := decoder.Decode(&record); err != nil {
		return Event{}, fmt.Errorf("decode step progress: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Event{}, errors.New("step progress contains trailing JSON")
	}
	if err := record.Validate(); err != nil {
		return Event{}, err
	}
	return record.Event, nil
}

func (event Event) Terminal() bool { return event.Status.terminal() }

// PlanVersion pins the durable plan record, and MaxPlanBytes bounds it. The
// bound applies only to reading historical plan files.
const (
	PlanVersion  = 2
	MaxPlanBytes = 256 << 10
)

// PlannedStep is one step the reviewed pipeline document declares.
//
// It deliberately carries no status. Every entry in a plan is by definition
// declared-and-not-reached, so a status field would be a place for a plan to
// claim an outcome. A planned step becomes an outcome only when a real
// observation or a durable result overwrites it.
//
// Secrets records that the declared step runs a credential chain and so
// receives the secret store login token. It is a property of the reviewed
// document, not an outcome, so the plan may carry it; plans written before it
// existed decode with it false and cannot prove replay safety. Credentialed
// adds every admitted credential class, including WIF; version 2 records it
// explicitly even when false.
type PlannedStep struct {
	Stage        string `json:"burn"`
	Step         string `json:"step"`
	Ordinal      int    `json:"ordinal"`
	Secrets      bool   `json:"secrets,omitempty"`
	Credentialed bool   `json:"credentialed"`
}

// Plan is the durable record of what a run intended to do, written before its
// first Pod exists. It is the zeroth state of run progress: without it a run's
// step list can only ever describe what already happened.
type Plan struct {
	Version int           `json:"version"`
	Steps   []PlannedStep `json:"steps"`
}

func (plan Plan) Validate() error {
	if plan.Version != 1 && plan.Version != PlanVersion {
		return fmt.Errorf("unsupported plan version %d", plan.Version)
	}
	keys := make(map[string]struct{}, len(plan.Steps))
	ordinals := make(map[int]struct{}, len(plan.Steps))
	for index, step := range plan.Steps {
		if !validName(step.Stage) || !validName(step.Step) || step.Ordinal < 0 {
			return fmt.Errorf("plan entry %d has an invalid burn, step, or ordinal", index)
		}
		key := step.Stage + "\x00" + step.Step
		if _, duplicate := keys[key]; duplicate {
			return fmt.Errorf("plan declares %s/%s more than once", step.Stage, step.Step)
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
