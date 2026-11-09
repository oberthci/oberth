package gitcache

import (
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/boundedio"
)

// NonDefaultBranchCount returns how many refs/heads/* branches the cached
// mirror holds besides the branch its HEAD names (#1508). It is a state
// marker for branch cleanup, not a listing: one bounded for-each-ref, no
// upstream contact. Reserved refs (refs/oberth/*, refs/replace/*, tags) are
// outside refs/heads and never counted. An unborn or detached HEAD counts
// every branch.
func (c *Cache) NonDefaultBranchCount(ctx context.Context, input string) (int, error) {
	repo, path, err := c.path(input)
	if err != nil {
		return 0, err
	}
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return 0, err
	}
	defer lock.RUnlock()
	if !c.isBare(ctx, path) {
		return 0, fmt.Errorf("repository %s is not cached", repo)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// %(HEAD) is "*" for the branch HEAD points at, so one command answers
	// both "which is default" and "how many others".
	args := []string{"for-each-ref", fmt.Sprintf("--count=%d", maximumReceiveSnapshotRefs+1), "--format=%(HEAD) %(refname)", "refs/heads/"}
	buf := &boundedio.Buffer{Limit: maxRefsOutput, OnOverflow: cancel}
	err = c.run(ctx, commandSpec{dir: path, args: args, stdout: buf, stderr: buf})
	if buf.Err() != nil {
		return 0, fmt.Errorf("ref output exceeds %d bytes; too many refs", maxRefsOutput)
	}
	if err != nil {
		return 0, err
	}
	return countNonDefaultBranches(buf.String())
}

func countNonDefaultBranches(output string) (int, error) {
	count, total := 0, 0
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		// Each line is "<marker> <refname>" where marker is "*" or " ".
		if len(line) < 2 || (line[0] != '*' && line[0] != ' ') || line[1] != ' ' || !strings.HasPrefix(line[2:], "refs/heads/") {
			return 0, fmt.Errorf("unexpected ref output %q", line)
		}
		total++
		if total > maximumReceiveSnapshotRefs {
			return 0, fmt.Errorf("more than %d refs in a bounded namespace", maximumReceiveSnapshotRefs)
		}
		if line[0] != '*' {
			count++
		}
	}
	return count, nil
}
