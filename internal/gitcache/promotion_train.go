package gitcache

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

type PromotionTrainCandidate struct {
	MergeCandidate
	TreeSHA string
	Sources []model.PromotionSource
}

type PromotionTrainConflict struct {
	Index int
	SHA   string
	Paths []string
}

func (e *PromotionTrainConflict) Error() string {
	return fmt.Sprintf("promotion source %d (%s) conflicts at paths %q", e.Index, e.SHA, e.Paths)
}

// PreparePromotionTrain merges every member in order in one private checkout.
// Native Git must produce a clean three-way merge; the complete result always
// requires one CI run. Single-source PreparePromotion retains its own policy.
func (c *Cache) PreparePromotionTrain(ctx context.Context, input string, sources []model.PromotionSource, target, workspace string, skip bool) (plan PromotionTrainCandidate, resultErr error) {
	if len(sources) == 0 || len(sources) > model.MaxPromotionSources {
		return plan, fmt.Errorf("promotion train requires 1..%d sources", model.MaxPromotionSources)
	}
	if err := ValidateBranch(target); err != nil {
		return plan, err
	}
	seen := map[string]bool{}
	for _, source := range sources {
		if err := ValidateSHA(source.SHA); err != nil {
			return plan, err
		}
		if source.Ref != "" {
			if err := ValidateBranch(strings.TrimPrefix(source.Ref, "refs/heads/")); err != nil {
				return plan, err
			}
		}
		key := strings.ToLower(source.SHA)
		if seen[key] {
			return plan, fmt.Errorf("duplicate promotion source %s", source.SHA)
		}
		seen[key] = true
	}
	plan.Sources = append([]model.PromotionSource(nil), sources...)
	for i := range plan.Sources {
		plan.Sources[i].Skipped = false
		plan.Sources[i].ConflictPaths = nil
	}
	_, path, err := c.path(input)
	if err != nil {
		return plan, err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return plan, err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return plan, err
	}
	for _, source := range sources {
		if err := c.assertCommit(ctx, path, source.SHA); err != nil {
			return plan, err
		}
	}
	tracking := "refs/oberth/promotion-targets/" + target
	defer func() { _ = c.deleteRef(context.WithoutCancel(ctx), path, tracking, "") }()
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"fetch", "--no-tags", "upstream", "+refs/heads/" + target + ":" + tracking}}); err != nil {
		absent, lookupErr := c.remoteBranchAbsent(ctx, path, target)
		if lookupErr != nil || !absent {
			return plan, fmt.Errorf("fetch promotion target %s: %w", target, err)
		}
		plan.TargetUnborn = true
	} else {
		base, err := c.capture(ctx, path, "rev-parse", "--verify", tracking+"^{commit}")
		if err != nil {
			return plan, err
		}
		plan.BaseSHA = strings.TrimSpace(base)
		if err := ValidateSHA(plan.BaseSHA); err != nil {
			return plan, err
		}
	}
	initial := plan.BaseSHA
	if plan.TargetUnborn {
		initial = sources[0].SHA
	}
	if err := c.checkoutLocked(ctx, path, initial, workspace); err != nil {
		return plan, err
	}
	defer func() {
		if resultErr != nil {
			_ = os.RemoveAll(workspace)
		}
	}()
	accepted := 0
	for i, source := range sources {
		if err := c.run(ctx, commandSpec{dir: workspace, args: []string{"fetch", "--quiet", "--force", "origin", source.SHA}}); err != nil {
			return plan, fmt.Errorf("fetch promotion source %d (%s): %w", i+1, source.SHA, err)
		}
		if i == 0 && plan.TargetUnborn {
			accepted++
			continue
		}
		err := c.run(ctx, commandSpec{dir: workspace, args: []string{"-c", "user.name=Oberth", "-c", "user.email=oberth@localhost", "-c", "commit.gpgSign=false", "-c", "core.hooksPath=/dev/null", "merge", "--no-ff", "--no-edit", "--no-gpg-sign", "-m", fmt.Sprintf("Merge train source %d %s into %s", i+1, source.SHA, target), source.SHA}})
		if err == nil {
			accepted++
			continue
		}
		// Status prefixes protect leading filename whitespace from capture's
		// text trimming; NUL framing preserves tabs, newlines and spaces.
		names, pathsErr := c.capture(ctx, workspace, "diff", "--name-status", "--diff-filter=U", "-z")
		if pathsErr != nil || names == "" {
			return plan, fmt.Errorf("merge promotion source %d (%s): %w", i+1, source.SHA, err)
		}
		fields := strings.Split(strings.TrimSuffix(names, "\x00"), "\x00")
		if len(fields)%2 != 0 {
			return plan, fmt.Errorf("invalid conflict paths for source %d (%s)", i+1, source.SHA)
		}
		paths := make([]string, 0, len(fields)/2)
		for j := 0; j < len(fields); j += 2 {
			if fields[j] != "U" || fields[j+1] == "" {
				return plan, fmt.Errorf("invalid conflict path for source %d (%s)", i+1, source.SHA)
			}
			paths = append(paths, fields[j+1])
		}
		sort.Strings(paths)
		conflict := &PromotionTrainConflict{Index: i + 1, SHA: source.SHA, Paths: paths}
		if !skip {
			return plan, conflict
		}
		if err := c.run(ctx, commandSpec{dir: workspace, args: []string{"merge", "--abort"}}); err != nil {
			return plan, fmt.Errorf("restore train after source %d conflict: %w", i+1, err)
		}
		plan.Sources[i].Skipped = true
		plan.Sources[i].ConflictPaths = paths
	}
	if accepted == 0 {
		return plan, fmt.Errorf("all promotion sources conflict; no run started")
	}
	merged, err := c.capture(ctx, workspace, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return plan, err
	}
	tree, err := c.capture(ctx, workspace, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return plan, err
	}
	plan.MergedSHA, plan.TreeSHA = strings.TrimSpace(merged), strings.TrimSpace(tree)
	if err := ValidateSHA(plan.MergedSHA); err != nil {
		return plan, err
	}
	if err := ValidateSHA(plan.TreeSHA); err != nil {
		return plan, err
	}
	if err := c.pinPromotion(ctx, path, target, plan.MergedSHA, workspace); err != nil {
		return plan, err
	}
	return plan, nil
}
