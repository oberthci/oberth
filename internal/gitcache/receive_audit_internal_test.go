package gitcache

import (
	"context"
	"errors"
	"testing"
)

func TestAcceptedCleanupHasNoRefusalAudit(t *testing.T) {
	c := &Cache{config: Config{Refused: func(context.Context, ReceiveEvent, error) error {
		t.Error("accepted cleanup was audited as refusal")
		return nil
	}}}
	cause := &receiveCleanupPendingError{errors.New("fixture cleanup failure")}
	result := receiveCompletion{accepted: true, returned: true, err: cause}
	r := ReceiveRequest{Actor: "agent", Repository: "origin/team/widget"}
	if err := c.finishTransport(t.Context(), "", r, result); !errors.Is(err, cause) {
		t.Fatal("accepted cleanup lost its original result", err)
	}
}
