package gitcache

import (
	"context"
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
func (c *Cache) merge(ctx context.Context, p, base, source string) (string, []string, error) {
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
	tree := strings.TrimSpace(fields[0])
	// Fixed identity/date makes a repeated preparation of identical roots stable.
	data, err = c.capture(
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
			env:   []string{"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z"},
		},
		128,
	)
	return string(data), nil, err
}

func (c *Cache) mergeSource(ctx context.Context, p string, r gitport.PromotionRequest) (string, []string, error) {
	if err := c.commit(ctx, p, r.Source.Commit); err != nil {
		return "", nil, err
	}
	if r.Target.CommitSHA == "" {
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
