package vmrunner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// These field orders match the independently versioned conductor producer.
// The proof is stream continuity under a separately authenticated exact CRI
// selector, never an identity claim from a Pod credential.
type conductorAttachChallenge struct {
	Version     int    `json:"version"`
	Type        string `json:"type"`
	RunID       string `json:"run_id"`
	Execution   string `json:"execution"`
	Attempt     string `json:"attempt"`
	ContainerID string `json:"container_id"`
	Nonce       string `json:"nonce"`
}

type conductorAttachProof struct {
	Version     int    `json:"version"`
	Type        string `json:"type"`
	RunID       string `json:"run_id"`
	Execution   string `json:"execution"`
	Attempt     string `json:"attempt"`
	ContainerID string `json:"container_id"`
	Nonce       string `json:"nonce"`
}

type bufferedConductorStream struct {
	*bufio.Reader
	closer io.Closer
}

func (stream *bufferedConductorStream) Close() error { return stream.closer.Close() }

// ProveConductorStream consumes only the first, nonsecret proof frame and
// returns a reader retaining any result bytes already buffered. The caller
// must bind the runtime's precise start and challenge in PilotJournal before
// invoking a fixture capability callback.
func ProveConductorStream(ctx context.Context, reader io.ReadCloser, input io.WriteCloser, plan PilotPlan, attempt ConductorAttempt, claim ConductorAttachClaim) (io.ReadCloser, error) {
	if reader == nil || input == nil || !claim.BoundAt.IsZero() || ValidateConductorAttachClaim(plan, attempt, claim) != nil {
		return nil, errors.New("vmrunner: unclaimed conductor attach stream")
	}
	stop := context.AfterFunc(ctx, func() { _ = reader.Close(); _ = input.Close() })
	defer stop()
	challenge := conductorAttachChallenge{Version: 1, Type: "attach-challenge", RunID: plan.Spec.RunID,
		Execution: PilotIdentity(plan), Attempt: ConductorAttemptIdentity(attempt), ContainerID: attempt.ContainerID, Nonce: claim.Challenge}
	body, err := json.Marshal(challenge)
	if err != nil || len(body) >= MaxConductorEventBytes-1 {
		return nil, errors.New("vmrunner: invalid conductor attach challenge")
	}
	body = append(body, '\n')
	if n, err := input.Write(body); err != nil || n != len(body) {
		return nil, errors.New("vmrunner: conductor attach challenge delivery failed")
	}
	framed := bufio.NewReaderSize(reader, MaxConductorEventBytes)
	line, err := framed.ReadSlice('\n')
	if err != nil || len(line) < 2 || len(line) >= MaxConductorEventBytes || ctx.Err() != nil {
		return nil, errors.New("vmrunner: missing or oversized conductor attach proof")
	}
	var proof conductorAttachProof
	if err := json.Unmarshal(line[:len(line)-1], &proof); err != nil {
		return nil, errors.New("vmrunner: malformed conductor attach proof")
	}
	canonical, err := json.Marshal(proof)
	if err != nil || !bytes.Equal(canonical, line[:len(line)-1]) || proof != (conductorAttachProof{
		Version: 1, Type: "attach-proof", RunID: challenge.RunID, Execution: challenge.Execution,
		Attempt: challenge.Attempt, ContainerID: challenge.ContainerID, Nonce: challenge.Nonce,
	}) {
		return nil, errors.New("vmrunner: conductor attach proof differs from claimed stream")
	}
	return &bufferedConductorStream{Reader: framed, closer: reader}, nil
}
