package gitcache

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
)

func TestAbsenceNeverConcealsMixedFailure(t *testing.T) {
	t.Parallel()
	absent := &gitport.AbsentError{Ref: "refs/heads/missing"}
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"absent", absent, true},
		{"wrapped absent", fmt.Errorf("lookup: %w", absent), true},
		{"joined absent", errors.Join(absent, absent), true},
		{"mixed failure", errors.Join(absent, io.ErrUnexpectedEOF), false},
		{"wrapped mixed failure", fmt.Errorf("lookup: %w", errors.Join(absent, io.ErrUnexpectedEOF)), false},
		{"unavailable with absent cause", &gitport.UnavailableError{Cause: absent}, false},
		{"no error", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isAbsent(test.err); got != test.want {
				t.Fatalf("absence=%v, want %v: %v", got, test.want, test.err)
			}
		})
	}
}
