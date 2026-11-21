package gitcache

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"github.com/oberthci/oberth/internal/publicerr"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGateHonorsEarlierCallerAdmissionDeadline(t *testing.T) {
	c, session, quarantine := gateFixture(t)
	c.config.AdmissionTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	expected, _ := ctx.Deadline()
	var calls atomic.Int32
	exited := make(chan struct{})
	c.config.PreReceive = func(admission context.Context, _ ReceiveEvent, _ *ReceiveSource) error {
		deadline, ok := admission.Deadline()
		if !ok || !deadline.Equal(expected) {
			t.Error("earlier caller deadline was extended")
		}
		calls.Add(1)
		defer close(exited)
		<-admission.Done()
		return context.Cause(admission)
	}
	gate, err := c.startGate(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.close()
	t.Setenv("OBERTH_RECEIVE_GATE", gate.address)
	t.Setenv("GIT_OBJECT_DIRECTORY", quarantine)
	err = callGate(ctx)
	var refusal *gateError
	if !errors.As(err, &refusal) || refusal.code != "unavailable" {
		t.Fatal("earlier deadline was accepted", err)
	}
	err = gate.close()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("cooperative admission did not exit")
	}
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatal("earlier deadline lost", err, calls.Load())
	}
}

func gateFixture(t *testing.T) (*Cache, gateSession, string) {
	t.Helper()
	c, err := New(t.Context(), Config{Root: t.TempDir(), Scratch: t.TempDir(), Binary: "/usr/bin/git"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	repo := "origin/team/widget"
	if err = c.Init(t.Context(), repo); err != nil {
		t.Fatal(err)
	}
	p, err := c.path(repo)
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(p, "objects", "tmp_objdir-incoming-test")
	if err = os.Mkdir(quarantine, 0700); err != nil {
		t.Fatal(err)
	}
	event := ReceiveEvent{
		ID:          strings.Repeat("ab", 16),
		Actor:       "agent",
		Fingerprint: "key",
		Repository:  repo,
		Admission:   ReleaseAdmission{DefaultBranch: "refs/heads/main"},
	}
	if err = writeReservation(p, Reservation{State: "reserved", Before: map[string]string{}, AlwaysRun: true, Event: event}); err != nil {
		t.Fatal(err)
	}
	c.setActive("receive-"+event.ID, true)
	return c, gateSession{store: p, owner: "receive-" + event.ID}, quarantine
}

func TestGateFrameAccepts8MiBRefusesOneMoreByte(t *testing.T) {
	valid := `{"code":"accepted"}`
	for _, size := range []int{8 << 20, (8 << 20) + 1} {
		var wire bytes.Buffer
		if err := binary.Write(&wire, binary.BigEndian, uint32(size)); err != nil {
			t.Fatal(err)
		}
		wire.WriteString(valid + strings.Repeat(" ", size-len(valid)))
		var reply gateReply
		err := readGateFrame(&wire, &reply)
		if (err == nil) != (size == 8<<20) {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}

func TestReceiveSourceRejectsForeignOrLinkedQuarantine(t *testing.T) {
	c, session, quarantine := gateFixture(t)
	r, err := readReservation(session.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{t.TempDir(), filepath.Join(session.store, "objects")} {
		if _, err = c.receiveSource(t.Context(), session.store, r, path); err == nil {
			t.Fatal("foreign quarantine accepted", path)
		}
	}
	if err = os.Remove(quarantine); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(t.TempDir(), quarantine); err != nil {
		t.Fatal(err)
	}
	if _, err = c.receiveSource(t.Context(), session.store, r, quarantine); err == nil {
		t.Fatal("linked quarantine accepted")
	}
}

func TestGateHasOneConfiguredAdmissionClock(t *testing.T) {
	clocks := 0
	for _, path := range []string{"gate.go", "gate_wire.go", "receive_source.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if configuredAdmissionClock(call, path) {
				clocks++
				return true
			}
			switch selector.Sel.Name {
			case "WithTimeout", "WithTimeoutCause", "WithDeadline", "WithDeadlineCause", "NewTimer", "NewTicker", "After":
				t.Error(path, "adds another admission clock", selector.Sel.Name)
			}
			return true
		})
	}
	if clocks != 1 {
		t.Fatal("configured admission clocks", clocks)
	}
}
func configuredAdmissionClock(call *ast.CallExpr, path string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "WithTimeout" || path != "gate.go" || len(call.Args) != 2 {
		return false
	}
	budget, ok := call.Args[1].(*ast.SelectorExpr)
	return ok && budget.Sel.Name == "AdmissionTimeout"
}

func TestGateWirePreservesRefusedVersusUnavailable(t *testing.T) {
	for _, code := range []publicerr.Code{publicerr.Forbidden, publicerr.Unavailable} {
		t.Run(string(code), func(t *testing.T) {
			c, session, quarantine := gateFixture(t)
			failure := publicerr.New(code, "admission did not finish", "Try this push again.", nil)
			c.config.PreReceive = func(context.Context, ReceiveEvent, *ReceiveSource) error { return failure }
			gate, err := c.startGate(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.close()
			t.Setenv("OBERTH_RECEIVE_GATE", gate.address)
			t.Setenv("GIT_OBJECT_DIRECTORY", quarantine)
			err = callGate(t.Context())
			var got *gateError
			if !errors.As(err, &got) || got.Error() != failure.Error() {
				t.Fatal(err)
			}
			want := "unavailable"
			if code == publicerr.Forbidden {
				want = "refused"
			}
			if got.code != want {
				t.Fatalf("wire code %s want %s", got.code, want)
			}
		})
	}
}

type frameHeaderOnly struct {
	header       *bytes.Reader
	payloadReads int
}

func (r *frameHeaderOnly) Read(p []byte) (int, error) {
	if r.header.Len() > 0 {
		return r.header.Read(p)
	}
	r.payloadReads++
	return 0, io.ErrUnexpectedEOF
}
func TestOversizedGateFrameRefusesBeforeReadingPayload(t *testing.T) {
	var prefix bytes.Buffer
	if err := binary.Write(&prefix, binary.BigEndian, uint32(maxGateFrame+1)); err != nil {
		t.Fatal(err)
	}
	reader := &frameHeaderOnly{header: bytes.NewReader(prefix.Bytes())}
	var reply gateReply
	err := readGateFrame(reader, &reply)
	var limit *LimitError
	if !errors.As(err, &limit) || reader.payloadReads != 0 {
		t.Fatal("oversized frame consumed payload", err, reader.payloadReads)
	}
}
