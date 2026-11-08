package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
)

// ref_delete_many (#1504): delete stray mirror refs outside refs/heads/ and
// refs/tags/, such as refs/remotes/upstream/* left by an old mirror tool.
// Same shape as branch_delete_many: a dry run returns a plan_id, and only a
// confirmation with that plan_id deletes, each ref compared and deleted on
// its exact object ID. Stray refs are never mirrored, so the deletion is
// local and final; the audit intent records every ref and object ID.

const (
	refPruneMaxEntries   = 500
	refPruneMaxPrefixes  = 32
	refPruneMaxPrefixLen = 255
)

// Stray ref classes: reachable from the default branch, reachable from a
// tag only, reachable from neither, or not peeling to a commit at all.
const (
	refClassDefaultBranch = "default-branch"
	refClassTag           = "tag"
	refClassUnreachable   = "unreachable"
	refClassNotCommit     = "not-commit"
)

// StrayRefGit is the cache side of ref_delete_many.
type StrayRefGit interface {
	StrayRefInventory(context.Context, string) (gitcache.StrayRefInventory, error)
	PruneStrayRefs(context.Context, string, []gitcache.StrayRefHead) ([]gitcache.StrayRefOutcome, error)
}

type refPruneArguments struct {
	Repo string `json:"repo"`
	Refs []struct {
		Ref         string `json:"ref"`
		ExpectedSHA string `json:"expected_sha"`
	} `json:"refs"`
	Prefixes         []string `json:"prefixes"`
	AllowUnreachable bool     `json:"allow_unreachable"`
	Confirm          bool     `json:"confirm"`
	PlanID           string   `json:"plan_id"`
}

type RefPlanEntry struct {
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
	ObjectType string `json:"object_type"`
	Commit     string `json:"commit,omitempty"`
	Symref     string `json:"symref,omitempty"`
	LastCommit string `json:"last_commit,omitempty"`
	Reachable  bool   `json:"reachable"`
	Class      string `json:"class"`
}

type RefPlanRefusal struct {
	Ref         string `json:"ref"`
	ExpectedSHA string `json:"expected_sha,omitempty"`
	SHA         string `json:"sha,omitempty"`
	Reason      string `json:"reason"`
}

type RefPruneResponse struct {
	Repo          string           `json:"repo"`
	PlanID        string           `json:"plan_id"`
	DryRun        bool             `json:"dry_run"`
	DefaultBranch string           `json:"default_branch"`
	DefaultSHA    string           `json:"default_sha"`
	Refs          []RefPlanEntry   `json:"refs"`
	Kept          []RefPlanEntry   `json:"kept"`
	KeptTruncated bool             `json:"kept_truncated,omitempty"`
	Refused       []RefPlanRefusal `json:"refused"`
	Count         int              `json:"count"`
	Truncated     bool             `json:"truncated,omitempty"`
	TotalRefs     int              `json:"total_refs"`
	Classes       map[string]int   `json:"classes"`
	Deleted       int              `json:"deleted,omitempty"`
	Skipped       []RefPlanRefusal `json:"skipped,omitempty"`
	Message       string           `json:"message"`
}

func (service *API) refDeleteMany(ctx context.Context, actor api.Actor, raw json.RawMessage) (RefPruneResponse, error) {
	if !actor.Admin || actor.Scoped() {
		return RefPruneResponse{}, refuse(ErrForbidden, "ref_delete_many requires an unscoped admin uplink")
	}
	git, ok := service.branchPrune.(StrayRefGit)
	if !ok || service.auditor == nil {
		return RefPruneResponse{}, refuse(ErrUnavailable, "ref deletion")
	}
	var args refPruneArguments
	if err := decodeTool(raw, &args); err != nil {
		return RefPruneResponse{}, err
	}
	input, plan, selected, err := service.planRefPrune(ctx, git, args)
	if err != nil {
		return RefPruneResponse{}, err
	}
	if !args.Confirm {
		plan.DryRun = true
		if len(args.Refs) == 0 && len(args.Prefixes) == 0 {
			plan.Message = "Listing only: every stray ref is under kept. Select with prefixes or refs, then confirm with that dry run's plan_id."
		} else {
			plan.Message = fmt.Sprintf("Dry run: nothing deleted. Confirm with confirm:true and plan_id %q and the same selection.", plan.PlanID)
		}
		return plan, nil
	}
	switch {
	case len(args.Refs) == 0 && len(args.Prefixes) == 0:
		return plan, fmt.Errorf("%w: confirm requires a selection: refs or prefixes", ErrInvalidInput)
	case strings.TrimSpace(args.PlanID) == "":
		return plan, fmt.Errorf("%w: confirm requires the plan_id from a dry run", ErrInvalidInput)
	case args.PlanID != plan.PlanID:
		return plan, refuse(ErrInvalidInput, "plan is stale: a ref moved or the selection changed since the dry run; run the dry run again")
	case len(selected) == 0:
		plan.Message = "Nothing to delete."
		return plan, nil
	}
	heads := make([]gitcache.StrayRefHead, len(selected))
	listed := make([]string, len(selected))
	for index, entry := range selected {
		heads[index] = gitcache.StrayRefHead{Ref: entry.Ref, SHA: entry.SHA}
		listed[index] = entry.Ref + " " + entry.SHA + " " + entry.Class
	}
	// The intent lists every ref with its object ID before Git runs, so a
	// deletion can be traced and, while the objects last, restored.
	details := map[string]any{
		"plan_id": plan.PlanID, "repo": input, "count": len(selected), "refs": listed,
		"allow_unreachable": args.AllowUnreachable, "prefixes": args.Prefixes, "explicit": len(args.Refs) > 0,
	}
	var outcomes []gitcache.StrayRefOutcome
	mutationErr := auditedGitMutation(ctx, service.auditor, actor.Identity, "ref.prune", "repository", input, details, func() error {
		var pruneErr error
		outcomes, pruneErr = git.PruneStrayRefs(ctx, input, heads)
		// auditedGitMutation copies details into the outcome entry after
		// this returns, so the outcome carries the per-ref results.
		deleted, skipped := 0, []string{}
		for _, outcome := range outcomes {
			if outcome.Result == gitcache.PruneDeleted {
				deleted++
				continue
			}
			skipped = append(skipped, outcome.Ref+" "+outcome.Result)
		}
		details["deleted"], details["skipped"] = deleted, skipped
		return pruneErr
	})
	plan.Skipped = []RefPlanRefusal{}
	for _, outcome := range outcomes {
		if outcome.Result == gitcache.PruneDeleted {
			plan.Deleted++
			continue
		}
		reason := outcome.Result
		if outcome.Result == gitcache.PruneMoved {
			reason = "ref moved to " + outcome.Current
		}
		plan.Skipped = append(plan.Skipped, RefPlanRefusal{Ref: outcome.Ref, ExpectedSHA: outcome.SHA, SHA: outcome.Current, Reason: reason})
	}
	if mutationErr != nil {
		return plan, mutationErr
	}
	plan.Message = fmt.Sprintf("Deleted %d stray ref(s) on the mirror; %d skipped. Stray refs are not mirrored: nothing changes upstream.", plan.Deleted, len(plan.Skipped))
	return plan, nil
}

// planRefPrune computes the deterministic plan. The plan ID hashes the exact
// selection with each object ID and class, so any movement makes a
// confirmation stale.
func (service *API) planRefPrune(ctx context.Context, git StrayRefGit, args refPruneArguments) (string, RefPruneResponse, []RefPlanEntry, error) {
	explicit := len(args.Refs) > 0
	switch {
	case strings.TrimSpace(args.Repo) == "":
		return "", RefPruneResponse{}, nil, fmt.Errorf("%w: repo is required", ErrInvalidInput)
	case explicit && len(args.Prefixes) > 0:
		return "", RefPruneResponse{}, nil, fmt.Errorf("%w: refs is exclusive with prefixes", ErrInvalidInput)
	case len(args.Refs) > refPruneMaxEntries:
		return "", RefPruneResponse{}, nil, fmt.Errorf("%w: at most %d refs per call", ErrInvalidInput, refPruneMaxEntries)
	case len(args.Prefixes) > refPruneMaxPrefixes:
		return "", RefPruneResponse{}, nil, fmt.Errorf("%w: at most %d prefixes", ErrInvalidInput, refPruneMaxPrefixes)
	}
	for _, prefix := range args.Prefixes {
		if len(prefix) > refPruneMaxPrefixLen || !strings.HasPrefix(prefix, "refs/") || gitcache.StrayRefProtected(prefix) {
			return "", RefPruneResponse{}, nil, fmt.Errorf("%w: prefixes must start with refs/, be at most %d bytes and stay outside refs/heads/, refs/tags/, refs/oberth/ and refs/replace/", ErrInvalidInput, refPruneMaxPrefixLen)
		}
	}
	repository, err := service.runs.RepositoryByName(ctx, args.Repo)
	if err != nil {
		return "", RefPruneResponse{}, nil, err
	}
	input := service.cacheInputFor(ctx, repository)
	inventory, err := git.StrayRefInventory(ctx, input)
	if err != nil {
		return "", RefPruneResponse{}, nil, err
	}
	plan := RefPruneResponse{Repo: input, DefaultBranch: inventory.DefaultBranch, DefaultSHA: inventory.DefaultSHA,
		Refs: []RefPlanEntry{}, Kept: []RefPlanEntry{}, Refused: []RefPlanRefusal{}, TotalRefs: len(inventory.Refs), Truncated: inventory.Truncated,
		Classes: map[string]int{refClassDefaultBranch: 0, refClassTag: 0, refClassUnreachable: 0, refClassNotCommit: 0}}
	entries := make(map[string]RefPlanEntry, len(inventory.Refs))
	ordered := make([]RefPlanEntry, 0, len(inventory.Refs))
	for _, ref := range inventory.Refs {
		entry := refPlanEntry(ref)
		plan.Classes[entry.Class]++
		entries[entry.Ref] = entry
		ordered = append(ordered, entry)
	}
	var selected []RefPlanEntry
	keep := func(entry RefPlanEntry) {
		if len(plan.Kept) >= refPruneMaxEntries {
			plan.KeptTruncated = true
			return
		}
		plan.Kept = append(plan.Kept, entry)
	}
	choose := func(entry RefPlanEntry) {
		if len(selected) >= refPruneMaxEntries {
			plan.Truncated = true
			return
		}
		selected = append(selected, entry)
	}
	if explicit {
		seen := make(map[string]bool, len(args.Refs))
		for _, request := range args.Refs {
			if request.Ref == "" || seen[request.Ref] {
				return "", RefPruneResponse{}, nil, fmt.Errorf("%w: refs must be nonempty and unique", ErrInvalidInput)
			}
			seen[request.Ref] = true
			entry, exists := entries[request.Ref]
			refusal := RefPlanRefusal{Ref: request.Ref, ExpectedSHA: strings.ToLower(request.ExpectedSHA), SHA: entry.SHA}
			switch {
			case gitcache.ValidateStrayRef(request.Ref) != nil:
				refusal.Reason = "not a stray ref: refs/heads/, refs/tags/, refs/oberth/ and refs/replace/ are never deleted here"
			case !validOID(request.ExpectedSHA):
				refusal.Reason = "expected_sha must be a full object ID"
			case !exists:
				refusal.Reason = "ref does not exist on the mirror"
			case !sameOID(entry.SHA, request.ExpectedSHA):
				refusal.Reason = "ref moved: expected " + strings.ToLower(request.ExpectedSHA) + ", found " + entry.SHA
			case !entry.Reachable && !args.AllowUnreachable:
				refusal.Reason = "not reachable from " + inventory.DefaultBranch + " or a tag (class " + entry.Class + "); set allow_unreachable to delete it"
			default:
				choose(entry)
				continue
			}
			plan.Refused = append(plan.Refused, refusal)
		}
	} else {
		for _, entry := range ordered {
			switch {
			case !hasAnyPrefix(entry.Ref, args.Prefixes):
				keep(entry)
			case !entry.Reachable && !args.AllowUnreachable:
				keep(entry)
			default:
				choose(entry)
			}
		}
	}
	plan.Refs = append(plan.Refs, selected...)
	plan.Count = len(selected)
	plan.PlanID = refPlanID(input, args.AllowUnreachable, selected)
	return input, plan, selected, nil
}

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func refPlanEntry(ref gitcache.StrayRef) RefPlanEntry {
	entry := RefPlanEntry{Ref: ref.Ref, SHA: ref.SHA, ObjectType: ref.ObjectType, Commit: ref.Commit, Symref: ref.Symref,
		Reachable: ref.ReachableFromDefault || ref.ReachableFromTag}
	if !ref.CommittedAt.IsZero() {
		entry.LastCommit = ref.CommittedAt.Format(time.RFC3339)
	}
	switch {
	case ref.Commit == "":
		entry.Class = refClassNotCommit
	case ref.ReachableFromDefault:
		entry.Class = refClassDefaultBranch
	case ref.ReachableFromTag:
		entry.Class = refClassTag
	default:
		entry.Class = refClassUnreachable
	}
	return entry
}

func refPlanID(input string, allowUnreachable bool, selected []RefPlanEntry) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "oberth-ref-prune-v1\n%s\n%t\n", input, allowUnreachable)
	sorted := append([]RefPlanEntry(nil), selected...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Ref < sorted[j].Ref })
	for _, entry := range sorted {
		_, _ = fmt.Fprintf(hash, "%s %s %s\n", entry.Ref, entry.SHA, entry.Class)
	}
	return hex.EncodeToString(hash.Sum(nil))[:32]
}
