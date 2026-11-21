package app

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func sourcePolicy(raw []byte) (map[string][]string, error) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 64<<10 {
		return nil, refusal("policy", nil)
	}
	var policy map[string][]string
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, refusal("policy", err)
	}
	if len(policy) > 256 {
		return nil, refusal("policy", nil)
	}
	for consumer, producers := range policy {
		if _, err := reponame.ParseRepository(consumer); err != nil || len(producers) > 64 {
			return nil, refusal("policy", err)
		}
		for _, producer := range producers {
			if _, err := reponame.ParseRepository(producer); err != nil {
				return nil, refusal("policy", err)
			}
		}
	}
	return policy, nil
}
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
		if !slices.Contains(s.policy[r.Repository], d.Repository) {
			return b, refusal("pin", nil)
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
