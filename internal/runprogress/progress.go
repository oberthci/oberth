// Package runprogress defines in-process exit observations and declared step coordinates.
package runprogress

import (
	"errors"
	"fmt"
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

func (event Event) Terminal() bool { return event.Status.terminal() }

// PlannedStep describes a declared pipeline coordinate for validation and admission.
// It carries no observed status and cannot supply native history authority.
type PlannedStep struct {
	Stage        string `json:"burn"`
	Step         string `json:"step"`
	Ordinal      int    `json:"ordinal"`
	Secrets      bool   `json:"secrets,omitempty"`
	Credentialed bool   `json:"credentialed"`
}
