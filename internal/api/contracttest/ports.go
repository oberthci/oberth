package contracttest

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

// AuthenticationCase supplies a real approved credential and its expected owner.
type AuthenticationCase struct {
	New    func(*testing.T) api.Authenticator
	Token  string
	Caller apicontract.Caller
}

func Authentication(t *testing.T, c AuthenticationCase) {
	t.Helper()
	a := c.New(t)
	got, err := a.Bearer(t.Context(), c.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.CredentialID != c.Caller.CredentialID || got.Actor.Identity != c.Caller.Actor.Identity {
		t.Fatal("authentication changed the credential owner")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.Bearer(ctx, c.Token); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
}

// StreamsCase supplies an authorized fixture for native bytes and an archive.
type StreamsCase struct {
	New    func(*testing.T) api.Streams
	Caller apicontract.Caller
	Input  apicontract.ContainerArgs
	Bytes  string
}

func Streams(t *testing.T, c StreamsCase) {
	t.Helper()
	s := c.New(t)
	reader, err := s.OpenLog(t.Context(), c.Caller, c.Input)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != c.Bytes {
		t.Fatal("native bytes changed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.OpenLog(ctx, c.Caller, c.Input); err == nil {
		t.Fatal("cancelled stream opened")
	}
	if err := s.CheckLog(ctx, c.Caller, c.Input.ID); err == nil {
		t.Fatal("cancelled authorization succeeded")
	}
}

// Diagnostics checks preservation of the fixed record through a sink fixture.
// The composition root supplies a capture of its real sink in the same suite.
func Diagnostics(t *testing.T, sink api.Diagnostics, snapshot func() []api.Diagnostic) {
	t.Helper()
	value := api.Diagnostic{Reference: "00000000000000000000000000000001", Tool: "run_wait", Transport: "sse", Outcome: api.Final, Frames: 1}
	sink.Record(value)
	got := snapshot()
	if len(got) != 1 || got[0] != value {
		t.Fatal("diagnostic sink changed the fixed transport record")
	}
}
