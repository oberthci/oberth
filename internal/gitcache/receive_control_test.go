package gitcache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ReceiveTestDisconnect is the committed hook peer used by the native test.
func ReceiveTestDisconnect(ctx context.Context) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", os.Getenv("OBERTH_RECEIVE_GATE"))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err = conn.(*net.UnixConn).CloseRead(); err != nil {
		return err
	}
	return writeGateFrame(conn, gateRequest{Phase: "committed"})
}

func SetReceiveCleanupBudget(c *Cache, budget time.Duration) { c.budgets.cleanup = budget }

func HoldReceiveRepository(ctx context.Context, c *Cache, repo string) (func(), error) {
	store, err := c.path(repo)
	if err != nil {
		return nil, err
	}
	if err = c.lock(ctx, store); err != nil {
		return nil, err
	}
	return func() { c.unlock(store) }, nil
}

func HoldReceivePublication(c *Cache, repo, ref string) func() {
	store, _ := c.path(repo)
	key := store + "\x00" + ref
	c.setPublication(key, true)
	return func() { c.setPublication(key, false) }
}

type failedGateReply struct {
	net.Conn
	input  *bytes.Reader
	cancel context.CancelFunc
}

func (c failedGateReply) Read(p []byte) (int, error) { return c.input.Read(p) }
func (c failedGateReply) Close() error               { return nil }
func (c failedGateReply) Write([]byte) (int, error) {
	c.cancel()
	return 0, io.ErrClosedPipe
}

func TestCancelledGateReplyIsNotRefusal(t *testing.T) {
	for _, phase := range []string{"committed", "aborted", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			var frame bytes.Buffer
			if err := writeGateFrame(&frame, gateRequest{Phase: phase}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			refused := false
			session := gateSession{refused: func() { refused = true }}
			conn := failedGateReply{input: bytes.NewReader(frame.Bytes()), cancel: cancel}
			c := &Cache{}
			held := 0
			err := c.serveGateCall(ctx, conn, session, &held)
			if !errors.Is(err, io.ErrClosedPipe) || ctx.Err() == nil || refused != (phase == "unknown") {
				t.Fatal("reply loss confused with operation refusal", err, refused)
			}
		})
	}
}

func TestRollbackPersistsRejectionBeforeInspectionFailure(t *testing.T) {
	store := filepath.Join(t.TempDir(), "origin", "team", "widget.git")
	if err := os.MkdirAll(store, 0700); err != nil {
		t.Fatal(err)
	}
	record := Reservation{
		Before: map[string]string{}, State: "ready",
		Event: ReceiveEvent{ID: strings.Repeat("ab", 16), Actor: "agent", Fingerprint: "key",
			Repository: "origin/team/widget", Admission: ReleaseAdmission{DefaultBranch: "refs/heads/main"}},
	}
	if err := writeReservation(store, record); err != nil {
		t.Fatal(err)
	}
	c := &Cache{budgets: defaultGitBudgets(), config: Config{Binary: "/unavailable/git"}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.rollbackReservation(ctx, store, record); err == nil {
		t.Fatal("reference inspection unexpectedly succeeded")
	}
	got, err := readReservation(store)
	if err != nil || got.State != "rejected" {
		t.Fatal("failed inspection left a replayable record", got.State, err)
	}
}

func TestReceiveReplyFailureHasWholeFeedback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failure  error
		feedback string
	}{
		{"reply", errors.New(strings.Repeat("unavailable path ", 30)), reasonGateUnavailable + ". " + remedyGateRetry},
		{"phase", refuse("receive transaction"), reasonTransactionPhase + ". " + remedyTransactionPhase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diagnostics bytes.Buffer
			result := receiveCompletion{gate: &receiveGate{err: tc.failure}}
			_, err := (&Cache{}).receiveFeedback(result, ReceiveRequest{Diagnostics: &diagnostics})
			if !errors.Is(err, tc.failure) {
				t.Fatal("reply failure detail lost", err)
			}
			if diagnostics.String() != tc.feedback+"\n" {
				t.Fatal("gate feedback lost its whole guidance", diagnostics.String())
			}
		})
	}
}

func TestAbandonRemovesOwnedDebrisAndForgetsItsEntry(t *testing.T) {
	c, session, _ := gateFixture(t)
	foreign := filepath.Join(session.store, "refs", "heads", "foreign.lock")
	if err := os.WriteFile(foreign, []byte("another writer"), 0600); err != nil {
		t.Fatal(err)
	}
	files := c.trackReceiveFiles(session.store, session.owner)
	lock := filepath.Join(session.store, "refs", "heads", "abandoned.lock")
	quarantine := filepath.Join(session.store, "objects", "tmp_objdir-incoming-abandoned")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(quarantine, 0700); err != nil {
		t.Fatal(err)
	}
	files.capture([]string{lock, quarantine, foreign})
	c.setActive(session.owner, false)
	event := strings.TrimPrefix(session.owner, "receive-")
	if err := c.AbandonReceive(t.Context(), "origin/team/widget", event); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{lock, quarantine} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("abandon left its own debris", path, err)
		}
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "another writer" {
		t.Fatal("abandon removed foreign debris", err)
	}
	if c.receiveCleanupFor(session.store) != nil {
		t.Fatal("abandon retained its cleanup entry")
	}
}

func TestLostGateReplyDoesNotInventAcceptance(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		var feedback bytes.Buffer
		result := receiveCompletion{gate: &receiveGate{err: io.ErrUnexpectedEOF}, returned: true, accepted: accepted}
		_, err := (&Cache{}).receiveFeedback(result, ReceiveRequest{Diagnostics: &feedback})
		if (err == nil) != accepted || (feedback.String() == "push accepted.\n") != accepted {
			t.Fatal("lost reply overrode the completion decision", accepted, err, feedback.String())
		}
	}
}

func BeforeReceivePreparation(c *Cache, hook func()) { c.receivePrepared = hook }

func AfterReceiveTransport(c *Cache, hook func()) {
	var once sync.Once
	c.receiveReturned = func() { once.Do(hook) }
}
