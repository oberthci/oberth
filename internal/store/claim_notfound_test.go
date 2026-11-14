package store

import (
	"errors"
	"testing"
	"time"
)

func TestStartRunRequiresAnAcceptedID(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	if _, err := s.StartRun(t.Context(), ""); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.StartRun(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
