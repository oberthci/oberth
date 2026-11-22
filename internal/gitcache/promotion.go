package gitcache

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func (c *Cache) PreparePromotion(ctx context.Context, r gitport.PromotionRequest) (gitport.Promotion, error) {
	if err := r.Validate(); err != nil {
		return gitport.Promotion{}, err
	}
	p, err := c.path(r.Repository)
	if err != nil {
		return gitport.Promotion{}, err
	}
	if err = c.lock(ctx, p); err != nil {
		return gitport.Promotion{}, err
	}
	defer c.unlock(p)
	result := gitport.Promotion{Request: r}
	owner, err := c.preparePromotionOwner(ctx, p, r.ActionReceipt)
	if err != nil {
		return result, err
	}
	if err = c.keep(ctx, p, owner, promotionRoots(r)); err != nil {
		return result, err
	}
	base, conflicts, err := c.mergeSource(ctx, p, r)
	if err != nil {
		return result, err
	}
	if len(conflicts) > 0 {
		result.Conflicts = conflicts
		return result, nil
	}

	tree, err := c.capture(ctx, command{dir: p, args: []string{"rev-parse", "--verify", base + "^{tree}"}}, 128)
	if err != nil {
		return result, err
	}
	result.Tree = string(tree)
	result.Result = gitport.Ref{Name: r.Target.Name, ObjectSHA: base, CommitSHA: base}
	if err = c.keep(ctx, p, owner, []string{base}); err != nil {
		return gitport.Promotion{}, err
	}
	return result, nil
}

// merge writes the merge of base and source. Its identity and date come only
// from its parents, so a repeated preparation of the same roots writes the
// same commit.
func (c *Cache) merge(ctx context.Context, p, base, source string) (string, []string, error) {
	tree, conflicts, err := c.mergeTree(ctx, p, base, source)
	if err != nil || len(conflicts) > 0 {
		return "", conflicts, err
	}
	date, err := c.promotionDate(ctx, p, base, source)
	if err != nil {
		return "", nil, err
	}
	data, err := c.capture(
		ctx,
		command{
			dir: p,
			args: []string{
				"-c",
				"user.name=Oberth",
				"-c",
				"user.email=ci@oberth.ci",
				"commit-tree",
				tree,
				"-p",
				base,
				"-p",
				source,
			},
			input: strings.NewReader("Oberth promotion\n"),
			env:   []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date},
		},
		128,
	)
	return string(data), nil, err
}
func (c *Cache) mergeTree(ctx context.Context, p, base, source string) (string, []string, error) {
	data, err := c.capture(
		ctx,
		command{
			dir:  p,
			args: []string{"merge-tree", "--write-tree", "--name-only", "-z", base, source},
		},
		32<<20,
	)
	fields := strings.Split(string(data), "\x00")
	if err != nil {
		if len(fields) > 2 && len(fields[0]) >= 40 {
			var conflicts []string
			for _, s := range fields[1:] {
				if s == "" {
					break
				}
				conflicts = append(conflicts, s)
			}
			if len(conflicts) > 0 {
				return "", conflicts, nil
			}
		}
		return "", nil, err
	}
	return strings.TrimSpace(fields[0]), nil, nil
}

// promotionDate is the tested commit's committer date, in UTC, or one second
// after the base's when the base is not older. Tools that order commits by
// date, such as Go pseudo-versions, then rank the promotion above the head it
// replaces.
func (c *Cache) promotionDate(ctx context.Context, p, base, source string) (string, error) {
	based, err := c.committed(ctx, p, base)
	if err != nil {
		return "", err
	}
	tested, err := c.committed(ctx, p, source)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("@%d +0000", max(tested, based+1)), nil
}

// committed reads a commit's committer date in Unix seconds.
func (c *Cache) committed(ctx context.Context, p, oid string) (int64, error) {
	data, err := c.capture(ctx, command{dir: p, args: []string{"show", "--no-patch", "--format=%ct", oid}}, 32)
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return 0, unavailable("git", err)
	}
	return seconds, nil
}

func (c *Cache) mergeSource(ctx context.Context, p string, r gitport.PromotionRequest) (string, []string, error) {
	if err := c.commit(ctx, p, r.Source.Commit); err != nil {
		return "", nil, err
	}
	if r.Target.CommitSHA == "" {
		return r.Source.Commit, nil, nil
	}
	// A target in the tested commit's history fast-forwards: the tested commit
	// itself becomes the result, so no merge is written and nothing is retested.
	forward, err := c.ancestor(ctx, p, r.Target.CommitSHA, r.Source.Commit)
	if err != nil {
		return "", nil, err
	}
	if forward {
		return r.Source.Commit, nil, nil
	}
	return c.merge(ctx, p, r.Target.CommitSHA, r.Source.Commit)
}
func promotionRoots(r gitport.PromotionRequest) []string {
	roots := []string{r.Source.Commit}
	if r.Target.CommitSHA != "" {
		roots = append(roots, r.Target.CommitSHA)
	}
	return roots
}

func promotionOwner(receipt string) string {
	return "promotion-" + sourcecontract.Digest([]byte(receipt))
}

// AbandonPromotion releases only the preparation named by its action receipt.
// Prepared promotion owners survive GC until this call or successful publication.
func (c *Cache) AbandonPromotion(ctx context.Context, repo, receipt string) error {
	if receipt == "" {
		return refuse("promotion receipt")
	}
	p, err := c.path(repo)
	if err != nil {
		return err
	}
	if err = c.lock(ctx, p); err != nil {
		return err
	}
	defer c.unlock(p)
	owners, err := c.promotionOwners(ctx, p, receipt)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if err = c.release(ctx, p, owner); err != nil {
			return err
		}
	}
	return nil
}
