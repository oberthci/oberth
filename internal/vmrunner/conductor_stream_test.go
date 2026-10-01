package vmrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type conductorInput struct {
	bytes.Buffer
	closed   int
	writeErr error
}

func (input *conductorInput) Write(body []byte) (int, error) {
	if input.writeErr != nil {
		return 0, input.writeErr
	}
	return input.Buffer.Write(body)
}
func (input *conductorInput) Close() error { input.closed++; return nil }

func testRestartRequest(plan PilotPlan, attempt ConductorAttempt) PilotRestartRequest {
	return PilotRestartRequest{Version: 1, Type: "restart-beacon", Execution: PilotIdentity(plan), Attempt: ConductorAttemptIdentity(attempt), Inventory: PilotInventoryIdentity(plan), Sequence: 1, Case: "restart-fresh-relay", Old: PilotEndpoint{Attempt: InitialVMResource, VMIUID: "old-vmi", LauncherUID: "old-launcher", Address: "10.42.1.10:443", ServerName: "beacon.fixture", CertificateSHA256: strings.Repeat("1", 64), FixtureGeneration: strings.Repeat("2", 64)}}
}

func testRestartResponse(request PilotRestartRequest) PilotRestartResponse {
	fresh := request.Old
	fresh.Attempt = RestartVMResource
	fresh.VMIUID = "new-vmi"
	fresh.LauncherUID = "new-launcher"
	fresh.CertificateSHA256 = strings.Repeat("3", 64)
	return PilotRestartResponse{Version: 1, Type: "restart-complete", Execution: request.Execution, Attempt: request.Attempt, Operation: PilotRestartIdentity(request), OldVMIUID: request.Old.VMIUID, Endpoint: fresh}
}

func conductorControlStream(t *testing.T, plan PilotPlan, attempt ConductorAttempt, request PilotRestartRequest, position int, duplicate bool) []byte {
	t.Helper()
	events := testConductorEvents(plan, attempt)
	var stream bytes.Buffer
	for index, event := range events {
		if index == position {
			if err := json.NewEncoder(&stream).Encode(request); err != nil {
				t.Fatal(err)
			}
			if duplicate {
				if err := json.NewEncoder(&stream).Encode(request); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := json.NewEncoder(&stream).Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	return stream.Bytes()
}

func TestConductorStreamUsesOnlyExplicitBoundRestartOperation(t *testing.T) {
	plan, attempt := testPilotPlan(), testConductorAttempt()
	request := testRestartRequest(plan, attempt)
	input := new(conductorInput)
	calls := 0
	restart := func(_ context.Context, actual PilotRestartRequest) (PilotRestartResponse, error) {
		calls++
		if actual != request {
			t.Fatal("handler received different operation")
		}
		return testRestartResponse(request), nil
	}
	results, err := ReadConductorStream(context.Background(), io.NopCloser(bytes.NewReader(conductorControlStream(t, plan, attempt, request, 15, false))), input, plan, attempt, restart)
	if err != nil || !results.Complete || calls != 1 || input.closed != 1 {
		t.Fatalf("exact stream: %#v, calls=%d, closed=%d, err=%v", results, calls, input.closed, err)
	}
	var response PilotRestartResponse
	if err := json.Unmarshal(bytes.TrimSpace(input.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response != testRestartResponse(request) {
		t.Fatal("host reply changed")
	}
	// Generic case-start must never call lifecycle work without the typed frame.
	calls = 0
	if _, err := ReadConductorStream(context.Background(), io.NopCloser(bytes.NewReader(encodeConductorEvents(t, testConductorEvents(plan, attempt)))), new(conductorInput), plan, attempt, restart); err == nil || calls != 0 {
		t.Fatalf("case progress caused lifecycle operation: calls=%d, err=%v", calls, err)
	}
}

func TestConductorStreamRejectsReplayWrongPhaseAndUnboundReplies(t *testing.T) {
	for _, kind := range []string{"duplicate", "early", "late", "attempt", "inventory", "old-uid", "handler-error", "old-reply", "write-error", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			plan, attempt := testPilotPlan(), testConductorAttempt()
			request := testRestartRequest(plan, attempt)
			position := 15
			duplicate := false
			switch kind {
			case "duplicate":
				duplicate = true
			case "early":
				position = 14
			case "late":
				position = 16
			case "attempt":
				request.Attempt = strings.Repeat("0", 64)
			case "inventory":
				request.Inventory = strings.Repeat("0", 64)
			case "old-uid":
				request.Old.VMIUID = ""
			}
			stream := conductorControlStream(t, plan, attempt, request, position, duplicate)
			if kind == "truncated" {
				stream = stream[:len(stream)-1]
			}
			input := new(conductorInput)
			if kind == "write-error" {
				input.writeErr = errors.New("closed input")
			}
			calls := 0
			handler := func(_ context.Context, request PilotRestartRequest) (PilotRestartResponse, error) {
				calls++
				if kind == "handler-error" {
					return PilotRestartResponse{}, errors.New("old launcher remains")
				}
				response := testRestartResponse(request)
				if kind == "old-reply" {
					response.Endpoint.VMIUID = request.Old.VMIUID
				}
				return response, nil
			}
			if _, err := ReadConductorStream(context.Background(), io.NopCloser(bytes.NewReader(stream)), input, plan, attempt, handler); err == nil {
				t.Fatalf("accepted %s stream", kind)
			}
			if calls > 1 {
				t.Fatalf("replayed operation dispatched %d times", calls)
			}
			if (kind == "early" || kind == "late" || kind == "attempt" || kind == "inventory" || kind == "old-uid") && calls != 0 {
				t.Fatalf("invalid operation dispatched %d times", calls)
			}
		})
	}
}

type blockedConductorStream struct {
	done chan struct{}
	once sync.Once
}

func (reader *blockedConductorStream) Read([]byte) (int, error) { <-reader.done; return 0, io.EOF }
func (reader *blockedConductorStream) Close() error {
	reader.once.Do(func() { close(reader.done) })
	return nil
}

func TestConductorStreamCancellationClosesBlockedReader(t *testing.T) {
	plan, attempt := testPilotPlan(), testConductorAttempt()
	reader := &blockedConductorStream{done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := ReadConductorStream(ctx, reader, new(conductorInput), plan, attempt, func(context.Context, PilotRestartRequest) (PilotRestartResponse, error) {
		t.Fatal("canceled stream dispatched restart")
		return PilotRestartResponse{}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked reader ignored cancellation: %v", err)
	}
}
