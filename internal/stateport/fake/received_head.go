package fake

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

type receivedEvidence struct {
	ObjectSHA string
	Heads     []string
}

func (f *Runs) ReceivedHeads(ctx context.Context, q stateport.ReceivedRef) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "ReceivedHeads", false); err != nil {
		return nil, err
	}
	if q.RunID == "" {
		r, ok := f.data.receivedHeads[q.Repo+"\x00"+q.Ref]
		if ok && r.ObjectSHA == q.ObjectSHA {
			return slices.Clone(r.Heads), nil
		}
		return nil, nil
	}
	seen := map[string]bool{}
	for q.RunID != "" && !seen[q.RunID] {
		seen[q.RunID] = true
		head, previous, err := f.data.receivedRunHead(q)
		if err != nil {
			return nil, err
		}
		if head != nil {
			return head, nil
		}
		q.RunID = previous
	}
	return nil, nil
}

func (m *runMemory) receivedRunHead(q stateport.ReceivedRef) ([]string, string, error) {
	for _, receipt := range m.receipts {
		if !slices.Contains(receipt.IDs, q.RunID) {
			continue
		}
		var binding struct {
			Received []model.CIRequest
			Runs     []struct{ ID, RerunOf string }
		}
		if err := json.Unmarshal([]byte(receipt.Binding), &binding); err != nil {
			return nil, "", err
		}
		for _, r := range binding.Received {
			if r.Repo == q.Repo && r.Ref == q.Ref && r.ObjectSHA == q.ObjectSHA {
				return slices.Clone(receipt.Heads[r.Ref]), "", nil
			}
		}
		for _, r := range binding.Runs {
			if r.ID == q.RunID {
				return nil, r.RerunOf, nil
			}
		}
	}
	return nil, "", nil
}

func (m *runMemory) recordReceived(b stateport.BatchAcceptance) (map[string][]string, error) {
	if len(b.Received) > stateport.MaxReceivedRefs {
		return nil, runProblem(publicerr.InvalidInput)
	}
	refs := map[string]bool{}
	heads := map[string][]string{}
	for _, r := range b.Received {
		if refs[r.Ref] {
			return nil, runProblem(publicerr.InvalidInput)
		}
		refs[r.Ref] = true
		if err := fakeReceived(b, r); err != nil {
			return nil, err
		}
		key := r.Repo + "\x00" + r.Ref
		previous := m.receivedHeads[key]
		heads[r.Ref] = stateport.InheritReceivedHeads(r.PreviousSHA, previous.ObjectSHA, previous.Heads)
		m.receivedHeads[key] = receivedEvidence{ObjectSHA: r.ObjectSHA, Heads: heads[r.Ref]}
	}
	return heads, nil
}

func fakeReceived(b stateport.BatchAcceptance, r model.CIRequest) error {
	if r.ReceiveID != b.ReceiveID || r.Repo != b.Repository || jsonValue(r.Actor) != jsonValue(b.Actor) || !stateport.ValidReceivedRef(r) {
		return runProblem(publicerr.InvalidInput)
	}
	return nil
}
