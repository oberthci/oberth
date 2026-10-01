package vmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

// PilotRestartHandler is an operator-owned finite lifecycle operation. It must
// claim the exact request in PilotJournal before side effects, and complete its
// durable response only after old cleanup and fresh actual endpoint binding.
// Repeated delivery can return that identical response but cannot repeat work.
type PilotRestartHandler func(context.Context, PilotRestartRequest) (PilotRestartResponse, error)

// ReadConductorStream demultiplexes the exact attached conductor's public
// result/control frames. It is not attach authentication: the runtime must bind
// the actual Pod/container/image, cancel lost streams and obtain the real exit.
func ReadConductorStream(ctx context.Context, reader io.ReadCloser, input io.WriteCloser, plan PilotPlan, attempt ConductorAttempt, restart PilotRestartHandler) (ConductorResults, error) {
	progress, err := newConductorProgress(plan, attempt)
	if err != nil {
		return ConductorResults{}, err
	}
	if reader == nil || input == nil || restart == nil {
		return ConductorResults{}, errors.New("vmrunner: conductor control channel is unavailable")
	}
	stop := context.AfterFunc(ctx, func() {
		_ = reader.Close()
		_ = input.Close()
	})
	defer stop()
	restarted := false
	err = readConductorFrames(reader, func(body []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(body, &header) != nil {
			return errors.New("vmrunner: malformed conductor frame")
		}
		if header.Type == "" {
			// The restart pass/final events cannot precede the explicit finite
			// operation. Merely starting the case does not call the handler.
			if progress.sequence >= 15 && !restarted {
				return errors.New("vmrunner: conductor omitted its restart operation")
			}
			return progress.accept(body)
		}
		if header.Type != "restart-beacon" || progress.sequence != 15 || restarted {
			return errors.New("vmrunner: unexpected or replayed conductor operation")
		}
		var request PilotRestartRequest
		if err := decodeConductorFrame(body, &request); err != nil {
			return err
		}
		if err := ValidatePilotRestartRequest(plan, attempt, request); err != nil {
			return err
		}
		response, err := restart(ctx, request)
		if err != nil {
			return err
		}
		if err := ValidatePilotRestartResponse(request, response); err != nil {
			return err
		}
		encoded, err := json.Marshal(response)
		if err != nil || len(encoded) >= MaxConductorEventBytes {
			return errors.New("vmrunner: invalid durable restart reply")
		}
		encoded = append(encoded, '\n')
		if n, err := input.Write(encoded); err != nil {
			return err
		} else if n != len(encoded) {
			return io.ErrShortWrite
		}
		if err := input.Close(); err != nil {
			return err
		}
		restarted = true
		return nil
	})
	if err != nil {
		return ConductorResults{}, err
	}
	if err := ctx.Err(); err != nil {
		return ConductorResults{}, err
	}
	if !restarted {
		return ConductorResults{}, errors.New("vmrunner: conductor did not complete its restart operation")
	}
	if err := verifyConductorResults(plan, attempt, progress.results); err != nil {
		return ConductorResults{}, err
	}
	return progress.results, nil
}
