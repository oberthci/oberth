package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func sourcePolicy(raw []byte) (workflowpolicy.SourcePolicy, error) {
	policy, err := workflowpolicy.ParseSourcePolicy(raw)
	if err != nil {
		return nil, refusal("policy", err)
	}
	return policy, nil
}

// sourceBinder binds main to the admitted commit and a pin to its exact producer
// commit, only for a pair the administrator's source policy lists. A
// repository pinning its own older commit is such a pair too. One admission
// decides each pinned revision once and reuses the decision for every
// declaration of it: the same question gets the same answer, at the cost of
// one forge read per producer commit.
type sourceBinder struct {
	s      *Submitter
	r      Request
	source Source
	pins   map[sourcecontract.Revision]*pinDecision
}

func (s *Submitter) binder(r Request, source Source) *sourceBinder {
	return &sourceBinder{s: s, r: r, source: source, pins: map[sourcecontract.Revision]*pinDecision{}}
}

func (bdr *sourceBinder) bind(ctx context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
	s, r, source := bdr.s, bdr.r, bdr.source
	b := sourcecontract.Binding{Descriptor: d, RepositoryID: r.RepositoryID, Revision: sourcecontract.Revision{Repository: r.Repository, Commit: r.Ref.CommitSHA}}
	if d.Kind == sourcecontract.Main {
		snapshot, err := source.Checkout(ctx, r.Repository, r.Ref)
		if r.Release && r.ReleaseHead.CommitSHA != "" {
			snapshot.DefaultBranch = r.ReleaseHead
		}
		b.Export = snapshot
		return b, err
	}
	if !s.policy.Allows(r.Repository, d.Repository) {
		return b, refusal("pin", nil).withCommand(publicerr.SourcePinCommand(r.Repository, d.Repository, s.d.Version))
	}
	b.Revision = sourcecontract.Revision{Repository: d.Repository, Commit: d.Commit}
	decision := bdr.pins[b.Revision]
	if decision == nil {
		if err := ctx.Err(); err != nil {
			return b, err
		}
		return b, refusal("unavailable", errors.New("source pin was not prepared"))
	}
	if decision.err != nil {
		return b, refusal("unavailable", decision.err)
	}
	if !decision.reachable || decision.id < 1 {
		return b, refusal("reachable", nil)
	}
	proof, _ := json.Marshal(struct {
		Consumer   string
		Descriptor sourcecontract.Descriptor
		Allowed    []string
	}{r.Repository, d, s.policy[r.Repository]})
	b.RepositoryID, b.AuthorizationDigest = decision.id, sourcecontract.Digest(proof)
	return b, nil
}

// prepare starts only allowed, distinct pins, then joins every decision before
// binding declarations. A refusal for one producer does not cancel the others.
// C4's pinned-destination limit bounds this fan-out to at most four decisions.
func (bdr *sourceBinder) prepare(ctx context.Context, descriptors []sourcecontract.Descriptor) {
	if ctx.Err() != nil {
		return
	}
	var wg sync.WaitGroup
	for _, d := range descriptors {
		if d.Kind != sourcecontract.Pinned || !bdr.s.policy.Allows(bdr.r.Repository, d.Repository) {
			continue
		}
		at := sourcecontract.Revision{Repository: d.Repository, Commit: d.Commit}
		if _, ok := bdr.pins[at]; ok {
			continue
		}
		decision := new(pinDecision)
		bdr.pins[at] = decision
		wg.Go(func() {
			decision.id, decision.reachable, decision.err = bdr.source.Pin(ctx, at)
		})
	}
	wg.Wait()
}

type pinDecision struct {
	id        int64
	reachable bool
	err       error
}
