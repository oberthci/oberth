package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestUnpreparedPinFailsClosedWithItsCause(t *testing.T) {
	t.Parallel()
	s := &Submitter{policy: workflowpolicy.SourcePolicy{"origin/team/widget": {"origin/team/source"}}}
	binder := s.binder(Request{Repository: "origin/team/widget"}, nil)
	pin := sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: "origin/team/source", Commit: strings.Repeat("e", 40)}
	bound, err := binder.bind(t.Context(), pin)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Code != "unavailable" || refusal.Detail != "source pin was not prepared" || bound.AuthorizationDigest != "" {
		t.Fatal("unprepared pin did not fail closed with its cause", bound, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	binder.prepare(ctx, []sourcecontract.Descriptor{pin})
	bound, err = binder.bind(ctx, pin)
	if !errors.Is(err, context.Canceled) || bound.AuthorizationDigest != "" {
		t.Fatal("cancelled preparation was mistaken for a missing hook", bound, err)
	}
}
