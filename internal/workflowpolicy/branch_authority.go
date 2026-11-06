package workflowpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// BranchAuthority is produced only by native candidate/current-default policy
// resolution. A request boolean or an unreadable policy cannot construct it.
type BranchAuthority struct {
	repository, candidate, baseline, digest string
}

func ResolveBranchAuthority(ctx context.Context, git PolicyGit, repository, candidate string) (*BranchAuthority, error) {
	baseline, err := Check(ctx, git, repository, candidate)
	if err != nil {
		return nil, err
	}
	if baseline == "" {
		return nil, errors.New("trusted policy does not authorize an ordinary branch-only execution")
	}
	body, err := json.Marshal([]string{repository, candidate, baseline, "native-absent-candidate-and-default/v1"})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	return &BranchAuthority{repository: repository, candidate: candidate, baseline: baseline, digest: hex.EncodeToString(sum[:])}, nil
}

func (a *BranchAuthority) Matches(repository, candidate, baseline, digest string) bool {
	return a != nil && a.repository == repository && a.candidate == candidate && a.baseline == baseline && a.digest == digest && a.digest != ""
}

func (a *BranchAuthority) Identity() (baseline, digest string) {
	if a == nil {
		return "", ""
	}
	return a.baseline, a.digest
}
