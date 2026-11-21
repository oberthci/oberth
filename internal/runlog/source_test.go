package runlog

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type reservingSource struct {
	*Store
	calls int
}

func (s *reservingSource) Reserve(ctx context.Context, id string, entry ContainerLog) (*Reservation, error) {
	s.calls++
	if s.calls == 1 {
		return nil, errors.New("reservation temporarily unavailable")
	}
	return s.Store.Reserve(ctx, id, entry)
}
func TestCaptureRetriesAvailabilityAndConsumesOneSource(t *testing.T) {
	s := &reservingSource{Store: newStore(t, 1024)}
	source := &singleReadSource{Reader: strings.NewReader("body\n")}
	opens, retries := 0, 0
	request := SourceCapture{RunID: "run", Container: native("one"), Open: func(context.Context) (io.ReadCloser, error) {
		opens++
		if opens == 1 {
			return nil, errors.New("Pod not yet readable")
		}
		return source, nil
	}, Retry: func(context.Context) error { retries++; return nil }}
	if err := Capture(t.Context(), s, request); err != nil {
		t.Fatal(err)
	}
	if err := Capture(t.Context(), s, request); err != nil {
		t.Fatal(err)
	}
	body, _, err := s.Read(t.Context(), "run", ReadRequest{})
	if err != nil || opens != 2 || retries != 2 || source.closed != 1 || source.bytes != 5 || string(body) != "body\n" {
		t.Fatal("physical source reopened or bytes lost", opens, retries, source, string(body), err)
	}
}
