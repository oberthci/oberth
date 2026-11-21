package app

import (
	"context"
	"encoding/json"

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
// repository pinning its own older commit is such a pair too.
func (s *Submitter) binder(r Request, source Source) func(context.Context, sourcecontract.Descriptor) (sourcecontract.Binding, error) {
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
			return b, refusal("pin", nil).withCommand(publicerr.SourcePinCommand(r.Repository, d.Repository))
		}
		b.Revision = sourcecontract.Revision{Repository: d.Repository, Commit: d.Commit}
		id, reachable, err := source.Pin(ctx, b.Revision)
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
