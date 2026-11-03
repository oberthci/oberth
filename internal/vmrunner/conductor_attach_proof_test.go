package vmrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func proofFixture(t *testing.T) (PilotPlan, ConductorAttempt, ConductorAttachClaim, conductorAttachProof) {
	t.Helper()
	plan, attempt := testPilotPlan(), testConductorAttempt()
	attempt.PodName = "conductor-abc"
	attempt.NodeName = "worker-1"
	attempt.ContainerID = "containerd://" + strings.Repeat("a", 64)
	claim := ConductorAttachClaim{AttemptIdentity: ConductorAttemptIdentity(attempt), Challenge: strings.Repeat("b", 64), ClaimedAt: time.Now().UTC().Add(-time.Second)}
	if err := ValidateConductorAttachClaim(plan, attempt, claim); err != nil {
		t.Fatal(err)
	}
	proof := conductorAttachProof{Version: 1, Type: "attach-proof", RunID: plan.Spec.RunID, Execution: PilotIdentity(plan),
		Attempt: claim.AttemptIdentity, ContainerID: attempt.ContainerID, Nonce: claim.Challenge}
	return plan, attempt, claim, proof
}

func TestConductorAttachProofGatesResultStreamAndPreservesBufferedBytes(t *testing.T) {
	plan, attempt, claim, proof := proofFixture(t)
	var stream bytes.Buffer
	if err := json.NewEncoder(&stream).Encode(proof); err != nil {
		t.Fatal(err)
	}
	stream.Write(encodeConductorEvents(t, testConductorEvents(plan, attempt)))
	input := new(conductorInput)
	bound, err := ProveConductorStream(context.Background(), io.NopCloser(bytes.NewReader(stream.Bytes())), input, plan, attempt, claim)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bound.Close() }()
	var challenge conductorAttachChallenge
	if err := json.Unmarshal(bytes.TrimSpace(input.Bytes()), &challenge); err != nil || challenge.RunID != proof.RunID ||
		challenge.Execution != proof.Execution || challenge.Attempt != proof.Attempt || challenge.ContainerID != proof.ContainerID || challenge.Nonce != proof.Nonce {
		t.Fatalf("wrong outbound challenge: %#v %v", challenge, err)
	}
	results, err := ReadConductorResults(bound, plan, attempt)
	if err != nil || !results.Complete {
		t.Fatalf("post-proof results lost: %#v %v", results, err)
	}
}

func TestConductorAttachProofRejectsWrongOrMissingBinding(t *testing.T) {
	for _, mode := range []string{"missing", "wrong-run", "wrong-attempt", "wrong-container", "wrong-nonce", "duplicate-key", "oversized", "short-id"} {
		t.Run(mode, func(t *testing.T) {
			plan, attempt, claim, proof := proofFixture(t)
			var body []byte
			switch mode {
			case "missing":
				body = nil
			case "wrong-run":
				proof.RunID = "other-run"
			case "wrong-attempt":
				proof.Attempt = strings.Repeat("c", 64)
			case "wrong-container":
				proof.ContainerID = "containerd://" + strings.Repeat("c", 64)
			case "wrong-nonce":
				proof.Nonce = strings.Repeat("c", 64)
			case "short-id":
				attempt.ContainerID = "containerd://a"
			case "oversized":
				body = append(bytes.Repeat([]byte("x"), MaxConductorEventBytes), '\n')
			case "duplicate-key":
				body = []byte(`{"version":1,"version":1,"type":"attach-proof"}` + "\n")
			}
			if body == nil && mode != "missing" && mode != "short-id" {
				encoded, err := json.Marshal(proof)
				if err != nil {
					t.Fatal(err)
				}
				body = append(encoded, '\n')
			}
			if mode == "short-id" {
				encoded, err := json.Marshal(proof)
				if err != nil {
					t.Fatal(err)
				}
				body = append(encoded, '\n')
			}
			if bound, err := ProveConductorStream(context.Background(), io.NopCloser(bytes.NewReader(body)), new(conductorInput), plan, attempt, claim); err == nil {
				_ = bound.Close()
				t.Fatal("unbound conductor proof accepted")
			}
		})
	}
}
