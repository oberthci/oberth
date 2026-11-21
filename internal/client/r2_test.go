package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestLostReadExplainsRetriesAndExhaustion(t *testing.T) {
	clock := fake.NewClock()
	var diagnostic bytes.Buffer
	calls := 0
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, io.EOF }), func(context.Context, time.Duration) error { return nil })
	c.now = clock.Now
	c.diagnostic = &diagnostic
	started := clock.Now()
	c.wait = func(_ context.Context, d time.Duration) error {
		if clock.Now().Sub(started) < 5*time.Second && diagnostic.Len() != 0 {
			t.Fatal("retry announced too early")
		}
		clock.Advance(d)
		return nil
	}
	_, refused, err := c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
	var p *publicerr.Error
	if refused || !errors.As(err, &p) || p.Reason() != "Server is reachable but not answering." {
		t.Fatalf("lost reply misdiagnosed: %v", err)
	}
	if diagnostic.String() != "No reply yet, trying again.\n" || calls < 3 || clock.Now().Sub(started) > 2*time.Minute {
		t.Fatalf("wrong retry feedback/bound: %q %d", diagnostic.String(), calls)
	}
}

func TestUnusedCredentialReservationsAreRemoved(t *testing.T) {
	for _, result := range []string{"refused", "error", "empty", "secret", "invalid"} {
		t.Run(result, func(t *testing.T) { unusedCredentialCase(t, result) })
	}
}
func unusedCredentialCase(t *testing.T, result string) {
	t.Helper()
	base := t.TempDir()
	d := definition(t, "uplink_add")
	d.ResultType = reflect.TypeFor[struct {
		Token string `json:"token" sensitive:"true"`
	}]()
	paths, err := ReserveCredentials(base, d, Options{})
	if err != nil || len(paths) == 0 {
		t.Fatalf("no reservations: %v", err)
	}
	defer CleanupCredentials(paths)
	switch result {
	case "empty":
		_, err = SaveCredentials(json.RawMessage(`{}`), paths)
	case "secret":
		_, err = SaveCredentials(json.RawMessage(`{"token":"keep-this-secret"}`), paths)
	case "invalid":
		_, err = SaveCredentials(json.RawMessage(`broken`), paths)
	}
	CleanupCredentials(paths)
	want := 0
	if result == "secret" {
		want = 1
		data, e := ReadPrivate(paths["token"])
		if e != nil || string(data) != "keep-this-secret" {
			t.Fatal("saved secret was removed")
		}
	}
	assertCredentialFileCount(t, base, want)
	if result != "invalid" && err != nil {
		t.Fatal(err)
	}
}

func TestLostReadCancellationKeepsInterruptedReason(t *testing.T) {
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { return nil, io.EOF }), func(context.Context, time.Duration) error { return context.Canceled })
	_, _, err := c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "interrupted") {
		t.Fatal(err)
	}
}

func assertCredentialFileCount(t *testing.T, base string, want int) {
	t.Helper()
	files, err := os.ReadDir(filepath.Join(base, "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != want {
		t.Fatalf("unused reservations left: %d", len(files))
	}
}

func TestLostReadDeadlineAndProtocolReasons(t *testing.T) {
	f := readFeedback{lost: true}
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, failure("reply", nil)} {
		err := f.failure(definition(t, "repo_list"), cause)
		var p *publicerr.Error
		if !errors.As(err, &p) {
			t.Fatal(err)
		}
		if errors.Is(cause, context.DeadlineExceeded) {
			if p.Reason() != "Server is reachable but not answering." {
				t.Fatal(err)
			}
		} else if !errors.Is(err, cause) {
			t.Fatal("specific failure lost")
		}
	}
}
func TestConcurrentRetryDiagnostics(t *testing.T) {
	var out bytes.Buffer
	writer := &diagnosticWriter{out: &out}
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			if _, err := io.WriteString(writer, "No reply yet, trying again.\n"); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if strings.Count(out.String(), "No reply yet, trying again.\n") != 32 {
		t.Fatal("retry diagnostics interleaved or lost")
	}
}
