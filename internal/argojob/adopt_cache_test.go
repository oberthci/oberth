package argojob_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestAdoptConfirmsCachedMismatchOnce(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"matching", "lagging", "foreign", "unavailable", "absent"} {
		t.Run(state, func(t *testing.T) {
			_, native, b := submitted(t)
			cache := &fake.Cache{WorkflowObject: native.Object.DeepCopy()}
			if state != "matching" {
				cache.WorkflowObject.Labels[nativebinding.RunLabel] = "predecessor"
			}
			switch state {
			case "foreign":
				native.Object.Labels[nativebinding.RunLabel] = "foreign"
			case "unavailable":
				native.GetError = context.DeadlineExceeded
			case "absent":
				native.Object = nil
			}
			before := native.Gets
			_, err := argojob.New(argojob.Config{Workflows: native, Cache: cache}).Adopt(t.Context(), b)
			wantReads := 1
			if state == "matching" {
				wantReads = 0
			}
			if native.Gets-before != wantReads {
				t.Fatal("unexpected live reads", native.Gets-before, wantReads)
			}
			switch state {
			case "matching", "lagging":
				compositionOK(t, err)
			case "foreign":
				if !errors.As(err, new(*nativebinding.Refusal)) {
					t.Fatal("foreign live owner accepted", err)
				}
			case "unavailable":
				if err == nil || errors.As(err, new(*nativebinding.Refusal)) || errors.As(err, new(argojob.ErrAbsent)) {
					t.Fatal("unavailable read established ownership or absence", err)
				}
			case "absent":
				if !errors.As(err, new(argojob.ErrAbsent)) {
					t.Fatal("live absence lost", err)
				}
			}
		})
	}
}
