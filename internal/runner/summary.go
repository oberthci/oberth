package runner

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
	SummaryVersion             = 1
	MaxTerminationSummaryBytes = periapsis.MaxTerminationSummaryBytes
)

type Status string

const (
	StatusPassed      Status = "passed"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
)

type StepStatus string

const (
	StepPassed   StepStatus = "passed"
	StepFailed   StepStatus = "failed"
	StepSkipped  StepStatus = "skipped"
	StepTimedOut StepStatus = "timed_out"
)

// StepResult is the durable, credential-free result of one subprocess.
type StepResult struct {
	Burn       string     `json:"burn"`
	Step       string     `json:"step"`
	Status     StepStatus `json:"status"`
	ExitCode   int        `json:"exit_code"`
	Error      string     `json:"-"`
	StartedAt  string     `json:"started_at"`
	FinishedAt string     `json:"finished_at"`
}

// Summary is the runner's in-process aggregate. Its Steps are written as the
// binding JSON array on the Kubernetes termination-message path.
type Summary struct {
	Version    int          `json:"version"`
	Trigger    string       `json:"trigger,omitempty"`
	Status     Status       `json:"status"`
	Error      string       `json:"error,omitempty"`
	Steps      []StepResult `json:"steps,omitempty"`
	StartedAt  string       `json:"started_at,omitempty"`
	FinishedAt string       `json:"finished_at,omitempty"`
}

type stepResultWire struct {
	Burn       *string     `json:"burn"`
	Step       *string     `json:"step"`
	Status     *StepStatus `json:"status"`
	ExitCode   *int        `json:"exit_code"`
	StartedAt  *string     `json:"started_at"`
	FinishedAt *string     `json:"finished_at"`
}

// DecodeStepResults strictly decodes the binding termination-message shape.
// It rejects non-arrays, unknown or missing fields, unsupported statuses, and
// malformed or reversed timestamps.
func DecodeStepResults(raw []byte) ([]StepResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire []stepResultWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("decode termination step results: %w", err)
	}
	if wire == nil {
		return nil, errors.New("termination step results must be a JSON array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("termination step results contain trailing JSON")
	}
	results := make([]StepResult, len(wire))
	for index, value := range wire {
		if value.Burn == nil || value.Step == nil || value.Status == nil || value.ExitCode == nil || value.StartedAt == nil || value.FinishedAt == nil {
			return nil, fmt.Errorf("termination step result %d is missing a required field", index)
		}
		result := StepResult{
			Burn: *value.Burn, Step: *value.Step, Status: *value.Status, ExitCode: *value.ExitCode,
			StartedAt: *value.StartedAt, FinishedAt: *value.FinishedAt,
		}
		if err := validateStepResult(result); err != nil {
			return nil, fmt.Errorf("termination step result %d: %w", index, err)
		}
		results[index] = result
	}
	return results, nil
}

func validateStepResult(result StepResult) error {
	if strings.TrimSpace(result.Burn) == "" || strings.TrimSpace(result.Step) == "" {
		return errors.New("burn and step are required")
	}
	switch result.Status {
	case StepPassed, StepFailed, StepSkipped, StepTimedOut:
	default:
		return fmt.Errorf("unsupported status %q", result.Status)
	}
	started, err := time.Parse(time.RFC3339Nano, result.StartedAt)
	if err != nil {
		return fmt.Errorf("started_at %q is invalid: %w", result.StartedAt, err)
	}
	finished, err := time.Parse(time.RFC3339Nano, result.FinishedAt)
	if err != nil {
		return fmt.Errorf("finished_at %q is invalid: %w", result.FinishedAt, err)
	}
	if finished.Before(started) {
		return errors.New("finished_at precedes started_at")
	}
	return nil
}

// MarshalSummary returns the complete binding []StepResult JSON value. Valid
// pipelines are admitted only when their complete result inventory fits the
// Kubernetes termination-message budget; callers outside that path fail
// closed instead of losing evidence.
func MarshalSummary(summary Summary) ([]byte, error) {
	steps := make([]StepResult, len(summary.Steps))
	copy(steps, summary.Steps)
	for index, result := range steps {
		if err := validateStepResult(result); err != nil {
			return nil, fmt.Errorf("termination step result %d: %w", index, err)
		}
	}
	raw, err := json.Marshal(steps)
	if err != nil {
		return nil, fmt.Errorf("marshal termination summary: %w", err)
	}
	if len(raw) > MaxTerminationSummaryBytes {
		return nil, fmt.Errorf("termination summary requires %d bytes, maximum is %d", len(raw), MaxTerminationSummaryBytes)
	}
	return raw, nil
}

// WriteSummary emits one bounded JSON value.
func WriteSummary(writer io.Writer, summary Summary) error {
	if writer == nil {
		return nil
	}
	raw, err := MarshalSummary(summary)
	if err != nil {
		return err
	}
	if _, err := writer.Write(raw); err != nil {
		return fmt.Errorf("write termination summary: %w", err)
	}
	return nil
}
