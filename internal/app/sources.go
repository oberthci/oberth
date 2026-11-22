package app

import (
	"context"
	"encoding/json"
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

// binder binds main to the admitted commit and a pin to its exact producer
// commit, only for a pair the administrator's source policy lists. A
// repository pinning its own older commit is such a pair too. One admission
// decides each pinned revision once and reuses the decision for every
// declaration of it: the same question gets the same answer, at the cost of
// one forge read per producer commit.
func (s *Submitter) binder(r Request, source Source) func(context.Context, sourcecontract.Descriptor) (sourcecontract.Binding, error) {
	pins := pinDecisions{decided: map[sourcecontract.Revision]pinDecision{}}
	return func(ctx context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
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
		id, reachable, err := pins.decide(ctx, source, b.Revision)
		if err != nil {
			return b, refusal("unavailable", err)
		}
		if !reachable || id < 1 {
			return b, refusal("reachable", err)
		}
		proof, _ := json.Marshal(struct {
			Consumer   string
			Descriptor sourcecontract.Descriptor
			Allowed    []string
		}{r.Repository, d, s.policy[r.Repository]})
		b.RepositoryID, b.AuthorizationDigest = id, sourcecontract.Digest(proof)
		return b, nil
	}
}

// pinDecisions keeps one admission's decision for each pinned revision.
type pinDecisions struct {
	mu      sync.Mutex
	decided map[sourcecontract.Revision]pinDecision
}

type pinDecision struct {
	id        int64
	reachable bool
	err       error
}

func (p *pinDecisions) decide(ctx context.Context, source Source, at sourcecontract.Revision) (int64, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.decided[at]; ok {
		return d.id, d.reachable, d.err
	}
	id, reachable, err := source.Pin(ctx, at)
	p.decided[at] = pinDecision{id: id, reachable: reachable, err: err}
	return id, reachable, err
}
