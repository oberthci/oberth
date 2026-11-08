package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// branch_deletion_resolve (#1500): close failed branch deletion records. A
// failed record already restored its mirror branch and left upstream
// unchanged, so resolving it changes no ref; it only ends the record as
// skipped with a reason line and one audit entry.

const (
	branchResolveMaxIDs    = 1000
	branchResolveMaxReason = 512
	branchResolvePageLimit = 500
)

type BranchDeletionResolveKept struct {
	ID     string `json:"id"`
	Branch string `json:"branch"`
	Reason string `json:"reason"`
}

type BranchDeletionResolveResponse struct {
	Repo     string                      `json:"repo"`
	Resolved []string                    `json:"resolved"`
	Kept     []BranchDeletionResolveKept `json:"kept"`
	Message  string                      `json:"message"`
}

func (service *API) branchDeletionResolve(ctx context.Context, actor api.Actor, raw json.RawMessage) (BranchDeletionResolveResponse, error) {
	if !actor.Admin || actor.Scoped() {
		return BranchDeletionResolveResponse{}, refuse(ErrForbidden, "branch_deletion_resolve requires an unscoped admin uplink")
	}
	if service.branchDeletions == nil || service.branchPrune == nil {
		return BranchDeletionResolveResponse{}, refuse(ErrUnavailable, "branch deletion")
	}
	var args struct {
		Repo   string   `json:"repo"`
		IDs    []string `json:"ids"`
		Reason string   `json:"reason"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return BranchDeletionResolveResponse{}, err
	}
	reason := strings.TrimSpace(args.Reason)
	switch {
	case strings.TrimSpace(args.Repo) == "":
		return BranchDeletionResolveResponse{}, fmt.Errorf("%w: repo is required", ErrInvalidInput)
	case reason == "" || len(reason) > branchResolveMaxReason:
		return BranchDeletionResolveResponse{}, fmt.Errorf("%w: reason of 1..%d bytes is required", ErrInvalidInput, branchResolveMaxReason)
	case len(args.IDs) > branchResolveMaxIDs:
		return BranchDeletionResolveResponse{}, fmt.Errorf("%w: at most %d ids per call", ErrInvalidInput, branchResolveMaxIDs)
	}
	for _, id := range args.IDs {
		if strings.TrimSpace(id) == "" {
			return BranchDeletionResolveResponse{}, fmt.Errorf("%w: ids must be nonempty", ErrInvalidInput)
		}
	}
	repository, err := service.runs.RepositoryByName(ctx, args.Repo)
	if err != nil {
		return BranchDeletionResolveResponse{}, err
	}
	input := service.cacheInputFor(ctx, repository)
	response := BranchDeletionResolveResponse{Repo: input, Resolved: []string{}, Kept: []BranchDeletionResolveKept{}}
	ids := args.IDs
	if len(ids) == 0 {
		// Without ids, resolve only what is provably moot: failed records
		// whose branch no longer exists on the mirror.
		reader, ok := service.branchPrune.(mirrorBranchReader)
		if !ok {
			return BranchDeletionResolveResponse{}, refuse(ErrUnavailable, "mirror branch lookup")
		}
		failed, err := service.branchDeletions.BranchDeletions(ctx, model.BranchDeletionFilter{RepoID: repository.ID, Status: model.BranchDeletionFailed, Limit: branchResolvePageLimit})
		if err != nil {
			return BranchDeletionResolveResponse{}, err
		}
		absent, present, err := absentFailedDeletions(ctx, reader, input, failed)
		if err != nil {
			return BranchDeletionResolveResponse{}, err
		}
		for _, record := range present {
			response.Kept = append(response.Kept, BranchDeletionResolveKept{ID: record.ID, Branch: record.Branch,
				Reason: "branch exists on the mirror; pass its id to resolve it"})
		}
		ids = absent
		reason = absentBranchReason + ": " + reason
	}
	if len(ids) > 0 {
		resolved, err := service.branchDeletions.ResolveBranchDeletions(ctx, repository.ID, ids, actor.Identity, reason)
		if err != nil {
			return BranchDeletionResolveResponse{}, err
		}
		response.Resolved = resolved
	}
	response.Message = fmt.Sprintf("Resolved %d failed record(s) as skipped; no ref changed.", len(response.Resolved))
	if len(response.Resolved) < len(ids) {
		response.Message += fmt.Sprintf(" %d id(s) were not failed records of %s and were left unchanged.", len(ids)-len(response.Resolved), input)
	}
	return response, nil
}
