package gitcache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

// A refusal the callback already gave when the admission context ends is the
// answer: either branch of the select returns it, and no stuck callback is
// observed.
func TestAdmissionKeepsARefusalReadyAtItsDeadline(t *testing.T) {
	observed := 0
	c := &Cache{config: Config{AdmissionMetric: func(string) { observed++ }}}
	refusal := publicerr.New(publicerr.InvalidInput, "pipeline admission failed", "Fix every reported finding, then push again.", nil)
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	for range 64 {
		done := make(chan error, 1)
		done <- refusal
		answered, err := c.admissionAnswer(expired, "receive", &ReceiveSource{}, done)
		if !answered || !errors.Is(err, refusal) {
			t.Fatal("a ready refusal was dropped at the deadline", answered, err)
		}
	}
	if observed != 0 {
		t.Fatal("a callback that had answered was observed as still running", observed)
	}
}

// A callback that has not answered when the admission context ends is never
// waited for: the check of its result at the deadline does not block, and it
// is observed as still running once.
func TestAdmissionNeverWaitsForACallbackThatHasNotAnswered(t *testing.T) {
	observed := 0
	c := &Cache{config: Config{AdmissionMetric: func(string) { observed++ }}}
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	returned := make(chan bool, 1)
	go func() {
		answered, _ := c.admissionAnswer(expired, "receive", &ReceiveSource{}, make(chan error))
		returned <- answered
	}()
	select {
	case answered := <-returned:
		if answered || observed != 1 {
			t.Fatal("a callback without an answer was taken as answered or not observed", answered, observed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the deadline waited for a callback that has not answered")
	}
}

// A callback whose release tag delivery has begun when the admission context
// ends is waited for, also when the delivery is recorded only after the
// deadline, and its approval is the answer.
func TestAdmissionWaitsForADeliveryUnderWay(t *testing.T) {
	c := &Cache{ctx: t.Context()}
	source := &ReceiveSource{}
	source.delivering.Store(true)
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	type answer struct {
		answered bool
		err      error
	}
	returned := make(chan answer, 1)
	go func() {
		answered, err := c.admissionAnswer(expired, "receive", source, done)
		returned <- answer{answered, err}
	}()
	select {
	case a := <-returned:
		t.Fatal("the deadline did not wait for a delivery under way", a)
	case <-time.After(200 * time.Millisecond):
	}
	source.delivered.Store(true)
	done <- nil
	select {
	case a := <-returned:
		if !a.answered || a.err != nil {
			t.Fatal("the delivery's approval was not the answer", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait for the delivery did not end with its answer")
	}
}

// The gate's reply: a refusal with a reason stands even after the deadline,
// a late approval or a bare late error never approves, and an admission that
// ran out of time says what timed out.
func TestAdmissionReplyAfterItsDeadline(t *testing.T) {
	live := t.Context()
	timedOut, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
	defer cancel()
	stopped, stop := context.WithCancel(t.Context())
	stop()
	refusal := publicerr.New(publicerr.InvalidInput, "pipeline admission failed", "Fix every reported finding, then push again.", nil)
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		answered bool
		err      error
		want     string
	}{
		{"approval in time", live, true, nil, ""},
		{"refusal in time", live, true, refusal, "pipeline admission failed"},
		{"refusal ready at the deadline", timedOut, true, refusal, "pipeline admission failed"},
		{"late approval", timedOut, true, nil, reasonGateTimeout},
		{"bare error at the deadline", timedOut, true, context.DeadlineExceeded, reasonGateTimeout},
		{"no answer at the deadline", timedOut, false, context.DeadlineExceeded, reasonGateTimeout},
		{"receive stopped", stopped, false, context.Canceled, reasonGateUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := admissionReply(tc.ctx, tc.answered, tc.err)
			if tc.want == "" {
				if err != nil {
					t.Fatal("an approval in time was refused", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reply %v, want %q", err, tc.want)
			}
		})
	}
	if publicerr.CheckMessage(reasonGateTimeout, 80) != nil || publicerr.CheckMessage(remedyGateTimeout, 120) != nil {
		t.Fatal("gate timeout line is not one sentence within bounds")
	}
}
