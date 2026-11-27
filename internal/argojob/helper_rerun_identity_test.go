package argojob

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRerunCapturesRootBeforeAuthoredCommand(t *testing.T) {
	for _, swapped := range []bool{false, true} {
		t.Run(strconv.FormatBool(swapped), func(t *testing.T) { checkRootCapture(t, swapped) })
	}
}

func checkRootCapture(t *testing.T, swapped bool) {
	t.Helper()
	r := rerunRuntime(t)
	// The executable starts with no injected identity, as in the Pod.
	r.sourceInfo = nil
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	diagnostic := &holdReadyWriter{ready: make(chan struct{})}
	done := make(chan int, 1)
	command := `if test -f "$1/FIXED"; then exit 0; fi; if test "$2" = true; then mv "$1" "$1-original"; mkdir "$1"; fi; exit 7`
	go func() {
		done <- r.dispatch(ctx, []string{"run", "--", "sh", "-c", command, "sh", r.sourceRoot, strconv.FormatBool(swapped)}, HoldIO{Out: io.Discard, Err: diagnostic})
	}()
	select {
	case <-diagnostic.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("identity capture hold did not start")
	}
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	var response bytes.Buffer
	code := r.client(ctx, []string{"rerun", "--pod-uid", "uid", "--sha256", digest}, HoldIO{In: bytes.NewReader(bundle), Err: &response, Out: io.Discard})
	want := 0
	if swapped {
		if code != 120 {
			t.Fatal("source root replaced by authored command was admitted", code, response.String())
		}
		if code = r.client(ctx, []string{"release", "--pod-uid", "uid"}, HoldIO{Err: io.Discard, Out: io.Discard}); code != 122 {
			// A release reports closed (122); its successful receipt still ends the hold.
			t.Fatal("root refusal lost the held owner", code)
		}
		want = 7
	} else if code != 0 {
		t.Fatal("source identity not captured at command start", code, response.String())
	}
	select {
	case got := <-done:
		if got != want {
			t.Fatal("identity capture changed authored exit", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("identity capture owner did not finish")
	}
}
