package vmrunner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	MaxConductorEventBytes  = 4096
	MaxConductorStreamBytes = 128 << 10
)

// ConductorEvent is emitted by the pinned conductor on its reserved result
// channel. Candidate bytes never become events; they are assertion inputs only.
type ConductorEvent struct {
	Version   int    `json:"version"`
	Execution string `json:"execution"`
	Attempt   string `json:"attempt"`
	Inventory string `json:"inventory"`
	Sequence  int    `json:"sequence"`
	Case      string `json:"case"`
	Phase     string `json:"phase"`
}

type ConductorResults struct {
	Execution string
	Attempt   string
	Inventory string
	Cases     []string
	Complete  bool
}

type conductorProgress struct {
	results  ConductorResults
	cases    []string
	sequence int
}

func newConductorProgress(plan PilotPlan, attempt ConductorAttempt) (*conductorProgress, error) {
	if err := ValidateConductorAttempt(plan, attempt); err != nil {
		return nil, err
	}
	return &conductorProgress{results: ConductorResults{Execution: PilotIdentity(plan), Attempt: ConductorAttemptIdentity(attempt), Inventory: PilotInventoryIdentity(plan)}, cases: BeaconCases()}, nil
}

func decodeConductorFrame(body []byte, target any) error {
	if len(body) >= MaxConductorEventBytes || json.Unmarshal(body, target) != nil {
		return errors.New("vmrunner: malformed conductor frame")
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(body, canonical) {
		return errors.New("vmrunner: noncanonical conductor frame")
	}
	return nil
}

func (progress *conductorProgress) accept(body []byte) error {
	var event ConductorEvent
	if err := decodeConductorFrame(body, &event); err != nil {
		return err
	}
	sequence := progress.sequence + 1
	expected := len(progress.cases)*2 + 1
	if sequence > expected {
		return errors.New("vmrunner: conductor emitted excess or duplicate results")
	}
	if event.Version != 1 || event.Execution != progress.results.Execution || event.Attempt != progress.results.Attempt || event.Inventory != progress.results.Inventory || event.Sequence != sequence {
		return errors.New("vmrunner: conductor result binding or sequence differs")
	}
	if sequence == expected {
		if event.Phase != "complete" || event.Case != "" {
			return errors.New("vmrunner: conductor inventory has no final completion")
		}
		progress.results.Complete = true
	} else {
		index := (sequence - 1) / 2
		phase := "start"
		if sequence%2 == 0 {
			phase = "pass"
		}
		if event.Case != progress.cases[index] || event.Phase != phase {
			return fmt.Errorf("vmrunner: required conductor case %s did not complete in order", progress.cases[index])
		}
		if phase == "pass" {
			progress.results.Cases = append(progress.results.Cases, event.Case)
		}
	}
	progress.sequence = sequence
	return nil
}

// ReadConductorResults is only for a host-bound result stream after any control
// frames were separately validated. It does not authorize an arbitrary Reader,
// a process exit or a host restart. Live duplex sessions use ReadConductorStream.
func ReadConductorResults(reader io.Reader, plan PilotPlan, attempt ConductorAttempt) (ConductorResults, error) {
	progress, err := newConductorProgress(plan, attempt)
	if err != nil {
		return ConductorResults{}, err
	}
	if err := readConductorFrames(reader, progress.accept); err != nil {
		return ConductorResults{}, err
	}
	if err := verifyConductorResults(plan, attempt, progress.results); err != nil {
		return ConductorResults{}, err
	}
	return progress.results, nil
}

func readConductorFrames(reader io.Reader, accept func([]byte) error) error {
	if reader == nil {
		return errors.New("vmrunner: conductor result stream is unavailable")
	}
	bounded := &io.LimitedReader{R: reader, N: MaxConductorStreamBytes + 1}
	framed := bufio.NewReaderSize(bounded, MaxConductorEventBytes)
	frames := 0
	for {
		line, err := framed.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		if err != nil {
			return fmt.Errorf("vmrunner: truncated or oversized conductor frame: %w", err)
		}
		frames++
		if frames > len(BeaconCases())*2+2 {
			return errors.New("vmrunner: conductor frame count exceeded")
		}
		if err := accept(line[:len(line)-1]); err != nil {
			return err
		}
	}
	if bounded.N <= 0 {
		return errors.New("vmrunner: conductor stream exceeds its byte bound")
	}
	return nil
}

func verifyConductorResults(plan PilotPlan, attempt ConductorAttempt, results ConductorResults) error {
	if !results.Complete || results.Execution != PilotIdentity(plan) || results.Attempt != ConductorAttemptIdentity(attempt) || results.Inventory != PilotInventoryIdentity(plan) {
		return errors.New("vmrunner: incomplete or unbound conductor inventory")
	}
	cases := BeaconCases()
	if len(results.Cases) != len(cases) {
		return errors.New("vmrunner: conductor did not execute every required case")
	}
	for index := range cases {
		if results.Cases[index] != cases[index] {
			return errors.New("vmrunner: conductor inventory has missing, duplicate or unexpected cases")
		}
	}
	return nil
}
