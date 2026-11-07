package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

// Batch branch deletion (branch_delete_many) and its receipts
// (branch_deletion_list).

const (
	branchPruneMaxEntries   = 1000
	branchPruneMaxExplicit  = 1000
	branchPrunePatchLimit   = 500
	branchPruneMaxPrefixes  = 32
	branchPruneMaxPrefixLen = 255
)

type BranchPruneGit interface {
	BranchInventory(context.Context, string, string) (gitcache.BranchInventory, error)
	BranchPatchEquivalence(context.Context, string, string, []gitcache.BranchHead, int) (map[string]bool, error)
	PruneBranches(context.Context, string, []gitcache.BranchHead, bool) ([]gitcache.PruneOutcome, error)
}

type BranchDeletionRecords interface {
	QueueBranchPrune(context.Context, string, int64, string, []model.BranchPruneEntry) error
	FinishBranchPrune(context.Context, string, string, int64, map[string]string, map[string]any) error
	BranchDeletions(context.Context, model.BranchDeletionFilter) ([]model.BranchDeletion, error)
	BranchDeletionBatch(context.Context, string) ([]model.BranchDeletion, error)
}

type branchPruneArguments struct {
	Repo     string `json:"repo"`
	Branches []struct {
		Branch      string `json:"branch"`
		ExpectedSHA string `json:"expected_sha"`
	} `json:"branches"`
	MergedInto       string   `json:"merged_into"`
	ExcludePrefixes  []string `json:"exclude_prefixes"`
	OlderThan        string   `json:"older_than"`
	AllowUnmerged    bool     `json:"allow_unmerged"`
	Confirm          bool     `json:"confirm"`
	PlanID           string   `json:"plan_id"`
	ConfirmProtected string   `json:"confirm_protected"`
}

type BranchPlanEntry struct {
	Branch          string `json:"branch"`
	HeadSHA         string `json:"head_sha"`
	Merged          bool   `json:"merged"`
	PatchEquivalent *bool  `json:"patch_equivalent"`
	Ahead           int    `json:"ahead"`
	Behind          int    `json:"behind"`
	LastCommit      string `json:"last_commit"`
	Protected       bool   `json:"protected,omitempty"`
}

type BranchPlanRefusal struct {
	Branch      string `json:"branch"`
	ExpectedSHA string `json:"expected_sha,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`
	Reason      string `json:"reason"`
}

type BranchPruneResponse struct {
	Repo            string              `json:"repo"`
	PlanID          string              `json:"plan_id"`
	DryRun          bool                `json:"dry_run"`
	DefaultBranch   string              `json:"default_branch"`
	MergedInto      string              `json:"merged_into"`
	TargetSHA       string              `json:"target_sha"`
	Branches        []BranchPlanEntry   `json:"branches"`
	Unmerged        []BranchPlanEntry   `json:"unmerged"`
	Refused         []BranchPlanRefusal `json:"refused"`
	Count           int                 `json:"count"`
	Truncated       bool                `json:"truncated,omitempty"`
	TotalCandidates int                 `json:"total_candidates"`
	BatchID         string              `json:"batch_id,omitempty"`
	Deleted         int                 `json:"deleted,omitempty"`
	Skipped         []BranchPlanRefusal `json:"skipped,omitempty"`
	Message         string              `json:"message"`
}

func (service *API) branchDeleteMany(ctx context.Context, actor api.Actor, raw json.RawMessage) (BranchPruneResponse, error) {
	if !actor.Admin || actor.Scoped() {
		return BranchPruneResponse{}, refuse(ErrForbidden, "branch_delete_many requires an unscoped admin uplink")
	}
	if service.branchPrune == nil || service.branchDeletions == nil {
		return BranchPruneResponse{}, refuse(ErrUnavailable, "branch deletion")
	}
	var args branchPruneArguments
	if err := decodeTool(raw, &args); err != nil {
		return BranchPruneResponse{}, err
	}
	repository, input, plan, selected, err := service.planBranchPrune(ctx, args)
	if err != nil {
		return BranchPruneResponse{}, err
	}
	if !args.Confirm {
		plan.DryRun = true
		plan.Message = fmt.Sprintf("Dry run: nothing deleted. Confirm with confirm:true and plan_id %q and the same selection.", plan.PlanID)
		return plan, nil
	}
	if strings.TrimSpace(args.PlanID) == "" {
		return plan, fmt.Errorf("%w: confirm requires the plan_id from a dry run", ErrInvalidInput)
	}
	if args.PlanID != plan.PlanID {
		return plan, refuse(ErrInvalidInput, "plan is stale: a branch head moved or the selection changed since the dry run; run the dry run again")
	}
	if len(selected) == 0 {
		plan.Message = "Nothing to delete."
		return plan, nil
	}
	batchID, err := newPruneBatchID()
	if err != nil {
		return plan, err
	}
	entries := make([]model.BranchPruneEntry, len(selected))
	heads := make([]gitcache.BranchHead, len(selected))
	protected := false
	for index, entry := range selected {
		entries[index] = model.BranchPruneEntry{Branch: entry.Branch, SHA: entry.HeadSHA}
		heads[index] = gitcache.BranchHead{Branch: entry.Branch, SHA: entry.HeadSHA}
		protected = protected || entry.Protected
	}
	if err := service.branchDeletions.QueueBranchPrune(ctx, batchID, repository.ID, actor.Identity, entries); err != nil {
		return plan, err
	}
	outcomes, pruneErr := service.branchPrune.PruneBranches(ctx, input, heads, protected)
	skipped := make(map[string]string)
	plan.Skipped = []BranchPlanRefusal{}
	if pruneErr != nil {
		for _, head := range heads {
			skipped[head.Branch] = "local deletion failed: " + pruneErr.Error()
		}
	}
	for _, outcome := range outcomes {
		if outcome.Result == gitcache.PruneDeleted {
			plan.Deleted++
			continue
		}
		reason := outcome.Result
		if outcome.Result == gitcache.PruneMoved {
			reason = "head moved to " + outcome.Current
		}
		skipped[outcome.Branch] = reason
		plan.Skipped = append(plan.Skipped, BranchPlanRefusal{Branch: outcome.Branch, ExpectedSHA: outcome.SHA, HeadSHA: outcome.Current, Reason: reason})
	}
	details := map[string]any{
		"plan_id": plan.PlanID, "batch_id": batchID, "repo": input, "repo_id": repository.ID,
		"deleted": plan.Deleted, "skipped": len(skipped), "failed": 0, "allow_unmerged": args.AllowUnmerged,
		"merged_into": plan.MergedInto, "older_than": args.OlderThan, "exclude_prefixes": args.ExcludePrefixes,
		"explicit": len(args.Branches) > 0, "protected_confirmed": protected,
	}
	if pruneErr != nil {
		details["failed"], details["deleted"], details["error"] = len(heads), 0, pruneErr.Error()
	}
	// The local refs already changed: record the outcome even if the
	// caller's request has been cancelled.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	finishErr := service.branchDeletions.FinishBranchPrune(finishCtx, batchID, actor.Identity, repository.ID, skipped, details)
	if service.deletionQueued != nil {
		service.deletionQueued()
	}
	if pruneErr != nil {
		if errors.Is(pruneErr, gitcache.ErrReceivePending) {
			return plan, errors.Join(refuse(ErrUnavailable, "%s", pruneErr.Error()), finishErr)
		}
		return plan, errors.Join(pruneErr, finishErr)
	}
	plan.BatchID = batchID
	plan.Message = fmt.Sprintf("Deleted %d branch(es) on the mirror; %d skipped. Upstream deletion is queued: read per-branch state with branch_deletion_list batch %q.", plan.Deleted, len(skipped), batchID)
	return plan, finishErr
}

// planBranchPrune computes the deterministic plan. The plan ID hashes the
// exact selection, so any head movement makes a confirmation stale.
func (service *API) planBranchPrune(ctx context.Context, args branchPruneArguments) (model.Repository, string, BranchPruneResponse, []BranchPlanEntry, error) {
	if strings.TrimSpace(args.Repo) == "" {
		return model.Repository{}, "", BranchPruneResponse{}, nil, fmt.Errorf("%w: repo is required", ErrInvalidInput)
	}
	explicit := len(args.Branches) > 0
	var olderThan time.Duration
	if args.OlderThan != "" {
		parsed, err := parseAge(args.OlderThan)
		if err != nil {
			return model.Repository{}, "", BranchPruneResponse{}, nil, err
		}
		olderThan = parsed
	}
	switch {
	case explicit && (args.MergedInto != "" || olderThan > 0 || len(args.ExcludePrefixes) > 0):
		return model.Repository{}, "", BranchPruneResponse{}, nil, fmt.Errorf("%w: branches is exclusive with merged_into, older_than and exclude_prefixes", ErrInvalidInput)
	case !explicit && args.MergedInto == "" && olderThan == 0:
		return model.Repository{}, "", BranchPruneResponse{}, nil, fmt.Errorf("%w: pass branches, or a selector: merged_into and/or older_than", ErrInvalidInput)
	case len(args.Branches) > branchPruneMaxExplicit:
		return model.Repository{}, "", BranchPruneResponse{}, nil, fmt.Errorf("%w: at most %d branches per call", ErrInvalidInput, branchPruneMaxExplicit)
	case len(args.ExcludePrefixes) > branchPruneMaxPrefixes:
		return model.Repository{}, "", BranchPruneResponse{}, nil, fmt.Errorf("%w: at most %d exclude_prefixes", ErrInvalidInput, branchPruneMaxPrefixes)
	case args.ConfirmProtected != "" && len(args.Branches) != 1:
		return model.Repository{}, "", BranchPruneResponse{}, nil, fmt.Errorf("%w: confirm_protected applies only to a single explicit branch", ErrInvalidInput)
	}
	for _, prefix := range args.ExcludePrefixes {
		if strings.TrimSpace(prefix) == "" || len(prefix) > branchPruneMaxPrefixLen {
			return model.Repository{}, "", BranchPruneResponse{}, nil, fmt.Errorf("%w: exclude_prefixes entries must be nonempty and at most %d bytes", ErrInvalidInput, branchPruneMaxPrefixLen)
		}
	}
	repository, err := service.runs.RepositoryByName(ctx, args.Repo)
	if err != nil {
		return model.Repository{}, "", BranchPruneResponse{}, nil, err
	}
	input := service.cacheInputFor(ctx, repository)
	inventory, err := service.branchPrune.BranchInventory(ctx, input, "")
	if err != nil {
		return model.Repository{}, "", BranchPruneResponse{}, nil, err
	}
	if args.MergedInto != "" && args.MergedInto != inventory.DefaultBranch {
		return model.Repository{}, "", BranchPruneResponse{}, nil, fmt.Errorf("%w: merged_into must be the default branch %q", ErrInvalidInput, inventory.DefaultBranch)
	}
	plan := BranchPruneResponse{Repo: input, DefaultBranch: inventory.DefaultBranch, MergedInto: inventory.Target, TargetSHA: inventory.TargetSHA,
		Branches: []BranchPlanEntry{}, Unmerged: []BranchPlanEntry{}, Refused: []BranchPlanRefusal{}}
	byName := make(map[string]gitcache.BranchInventoryEntry, len(inventory.Branches))
	for _, entry := range inventory.Branches {
		byName[entry.Branch] = entry
	}
	var candidates []gitcache.BranchInventoryEntry
	if explicit {
		seen := make(map[string]bool, len(args.Branches))
		for _, request := range args.Branches {
			if request.Branch == "" || seen[request.Branch] {
				return model.Repository{}, "", BranchPruneResponse{}, nil, fmt.Errorf("%w: branches must be nonempty and unique", ErrInvalidInput)
			}
			seen[request.Branch] = true
			entry, exists := byName[request.Branch]
			refusal := BranchPlanRefusal{Branch: request.Branch, ExpectedSHA: strings.ToLower(request.ExpectedSHA), HeadSHA: entry.SHA}
			switch {
			case !validOID(request.ExpectedSHA):
				refusal.Reason = "expected_sha must be a full object ID"
			case !exists:
				refusal.Reason = "branch does not exist on the mirror"
			case !sameOID(entry.SHA, request.ExpectedSHA):
				refusal.Reason = "head moved: expected " + strings.ToLower(request.ExpectedSHA) + ", found " + entry.SHA
			case entry.Protected && args.ConfirmProtected != input+":"+request.Branch:
				refusal.Reason = fmt.Sprintf("protected branch: delete it alone with confirm_protected %q", input+":"+request.Branch)
			default:
				candidates = append(candidates, entry)
				continue
			}
			plan.Refused = append(plan.Refused, refusal)
		}
	} else {
		cutoff := time.Now().Add(-olderThan)
		for _, entry := range inventory.Branches {
			if entry.Protected || excludedBranch(entry.Branch, args.ExcludePrefixes) {
				continue
			}
			if olderThan > 0 && !entry.CommittedAt.Before(cutoff) {
				continue
			}
			candidates = append(candidates, entry)
		}
	}
	var unmergedHeads []gitcache.BranchHead
	for _, entry := range candidates {
		if !entry.Merged {
			unmergedHeads = append(unmergedHeads, gitcache.BranchHead{Branch: entry.Branch, SHA: entry.SHA})
		}
	}
	equivalent := map[string]bool{}
	if len(unmergedHeads) > 0 {
		equivalent, err = service.branchPrune.BranchPatchEquivalence(ctx, input, inventory.Target, unmergedHeads, branchPrunePatchLimit)
		if err != nil {
			return model.Repository{}, "", BranchPruneResponse{}, nil, err
		}
	}
	var selected []BranchPlanEntry
	for _, entry := range candidates {
		planned := BranchPlanEntry{Branch: entry.Branch, HeadSHA: entry.SHA, Merged: entry.Merged, Ahead: entry.Ahead, Behind: entry.Behind,
			LastCommit: entry.CommittedAt.Format(time.RFC3339), Protected: entry.Protected}
		patch, checked := equivalent[entry.Branch]
		if entry.Merged {
			patch, checked = true, true
		}
		if checked {
			planned.PatchEquivalent = &patch
		}
		mergedLike := entry.Merged || (checked && patch)
		plan.TotalCandidates++
		switch {
		case mergedLike:
			if len(plan.Branches)+len(plan.Unmerged) >= branchPruneMaxEntries {
				plan.Truncated = true
				continue
			}
			plan.Branches = append(plan.Branches, planned)
			selected = append(selected, planned)
		case args.AllowUnmerged:
			if len(plan.Branches)+len(plan.Unmerged) >= branchPruneMaxEntries {
				plan.Truncated = true
				continue
			}
			plan.Unmerged = append(plan.Unmerged, planned)
			selected = append(selected, planned)
		case explicit:
			plan.Refused = append(plan.Refused, BranchPlanRefusal{Branch: entry.Branch, ExpectedSHA: entry.SHA, HeadSHA: entry.SHA,
				Reason: fmt.Sprintf("not merged into %s (%d commit(s) ahead); set allow_unmerged to delete it", inventory.Target, entry.Ahead)})
		}
	}
	plan.Count = len(selected)
	plan.PlanID = branchPlanID(input, args, selected)
	return repository, input, plan, selected, nil
}

func branchPlanID(input string, args branchPruneArguments, selected []BranchPlanEntry) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "oberth-branch-prune-v1\n%s\n%t\n%s\n", input, args.AllowUnmerged, args.ConfirmProtected)
	sorted := append([]BranchPlanEntry(nil), selected...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Branch < sorted[j].Branch })
	for _, entry := range sorted {
		_, _ = fmt.Fprintf(hash, "%s %s %t\n", entry.Branch, entry.HeadSHA, entry.Merged)
	}
	return hex.EncodeToString(hash.Sum(nil))[:32]
}

func excludedBranch(branch string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(branch, prefix) {
			return true
		}
	}
	return false
}

// parseAge accepts a Go duration or whole days such as 30d.
func parseAge(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if days, ok := strings.CutSuffix(value, "d"); ok {
		count, err := strconv.Atoi(days)
		if err == nil && count > 0 && count <= 36500 {
			return time.Duration(count) * 24 * time.Hour, nil
		}
	} else if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
		return parsed, nil
	}
	return 0, fmt.Errorf("%w: older_than must be a positive duration such as 30d or 72h", ErrInvalidInput)
}

func newPruneBatchID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate prune batch ID: %w", err)
	}
	return "prune-" + hex.EncodeToString(value[:]), nil
}

type BranchDeletionView struct {
	ID         string `json:"id"`
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	OldSHA     string `json:"old_sha"`
	Actor      string `json:"actor"`
	Source     string `json:"source"`
	BatchID    string `json:"batch_id,omitempty"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	RetryAfter string `json:"retry_after,omitempty"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	Sequence   int64  `json:"sequence"`
}

func (service *API) branchDeletionList(ctx context.Context, actor api.Actor, raw json.RawMessage) (map[string]any, error) {
	if !actor.Admin || actor.Scoped() {
		return nil, refuse(ErrForbidden, "branch_deletion_list requires an unscoped admin uplink")
	}
	if service.branchDeletions == nil {
		return nil, refuse(ErrUnavailable, "branch deletion")
	}
	var args struct {
		Repo   string `json:"repo"`
		Status string `json:"status"`
		Batch  string `json:"batch"`
		Before int64  `json:"before"`
		Limit  int    `json:"limit"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	var records []model.BranchDeletion
	var err error
	if args.Limit <= 0 || args.Limit > 200 {
		args.Limit = 50
	}
	if args.Batch != "" {
		records, err = service.branchDeletions.BranchDeletionBatch(ctx, args.Batch)
	} else {
		filter := model.BranchDeletionFilter{Status: model.BranchDeletionStatus(args.Status), Before: args.Before, Limit: args.Limit}
		if args.Status != "" && !filter.Status.Valid() {
			return nil, fmt.Errorf("%w: status must be pending, propagated, failed or skipped", ErrInvalidInput)
		}
		if args.Repo != "" {
			repository, lookupErr := service.runs.RepositoryByName(ctx, args.Repo)
			if lookupErr != nil {
				return nil, lookupErr
			}
			filter.RepoID = repository.ID
		}
		records, err = service.branchDeletions.BranchDeletions(ctx, filter)
	}
	if err != nil {
		return nil, err
	}
	views := make([]BranchDeletionView, 0, len(records))
	counts := map[string]int{}
	for _, record := range records {
		view := BranchDeletionView{ID: record.ID, Repo: record.Repository, Branch: record.Branch, OldSHA: record.OldSHA, Actor: record.Actor,
			Source: record.Source, BatchID: record.BatchID, Status: string(record.Status), Attempts: record.Attempts, Error: record.Error,
			CreatedAt: record.CreatedAt.Format(time.RFC3339), UpdatedAt: record.UpdatedAt.Format(time.RFC3339), Sequence: record.Sequence}
		if record.Status == model.BranchDeletionPending && !record.RetryAfter.IsZero() {
			view.RetryAfter = record.RetryAfter.Format(time.RFC3339)
		}
		counts[view.Status]++
		views = append(views, view)
	}
	result := map[string]any{"deletions": views, "counts": counts}
	if args.Batch == "" && len(views) == args.Limit {
		result["next_before"] = views[len(views)-1].Sequence
	}
	return result, nil
}
