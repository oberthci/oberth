package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/oberthci/oberth/internal/boundedio"
	"github.com/oberthci/oberth/internal/model"
)

// GreenIssueTrailers reads policy at the published commit, then asks Git to
// identify real trailer blocks. Prose matching is deliberately not supported.
// Both the policy and the entire history result have rejecting byte bounds.
func (c *Cache) GreenIssueTrailers(ctx context.Context, input, previous, head string) (bool, []model.CommitIssueTrailer, error) {
	if err := ValidateSHA(head); err != nil {
		return false, nil, err
	}
	if previous != "" {
		if err := ValidateSHA(previous); err != nil {
			return false, nil, err
		}
	}
	_, path, err := c.path(input)
	if err != nil {
		return false, nil, err
	}
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return false, nil, err
	}
	defer lock.RUnlock()
	if !c.isBare(ctx, path) {
		return false, nil, errors.New("repository is not cached")
	}
	entry, err := c.capture(ctx, path, "ls-tree", "--format=%(objectmode) %(objecttype) %(objectname)", head, "--", ".oberth/issues.yaml")
	if err != nil {
		return false, nil, err
	}
	if entry == "" {
		return false, nil, nil
	}
	parts := strings.Fields(entry)
	if len(parts) != 3 || (parts[0] != "100644" && parts[0] != "100755") || parts[1] != "blob" || ValidateSHA(parts[2]) != nil {
		return false, nil, fmt.Errorf("%w: policy must be a regular file", model.ErrIssueTrailerPermanent)
	}
	policyBytes, err := c.issueTrailerOutput(ctx, path, 16<<10, "cat-file", "blob", parts[2])
	if err != nil {
		return false, nil, err
	}
	var policy struct {
		Enabled bool `yaml:"close_from_trailers"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(policyBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&policy); err != nil {
		// Do not copy repository-controlled policy content into audit details.
		return false, nil, fmt.Errorf("%w: invalid issue trailer policy", model.ErrIssueTrailerPermanent)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return false, nil, fmt.Errorf("%w: policy must contain one document", model.ErrIssueTrailerPermanent)
	}
	if !policy.Enabled {
		return false, nil, nil
	}
	args := []string{"log", "--reverse", "--topo-order", "--format=%H%x00%(trailers:only,unfold)%x00", head}
	if previous != "" {
		args = append(args, "^"+previous)
	}
	args = append(args, "--")
	output, err := c.issueTrailerOutput(ctx, path, 4<<20, args...)
	if err != nil {
		return false, nil, err
	}
	trailers, err := parseIssueTrailers(output)
	return true, trailers, err
}

func (c *Cache) issueTrailerOutput(ctx context.Context, path string, limit int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sink := &boundedio.Buffer{
		Limit: limit, OnOverflow: cancel,
		LimitError: fmt.Errorf("%w: Git output exceeds %d byte limit", model.ErrIssueTrailerPermanent, limit),
	}
	err := c.run(ctx, commandSpec{dir: path, args: args, stdout: sink})
	if sink.Err() != nil {
		return nil, sink.Err()
	}
	if err != nil {
		return nil, err
	}
	return sink.Bytes(), nil
}

func parseIssueTrailers(output []byte) ([]model.CommitIssueTrailer, error) {
	if len(bytes.TrimSpace(output)) == 0 {
		return nil, nil
	}
	fields := strings.Split(string(output), "\x00")
	if len(fields)%2 != 1 || strings.TrimSpace(fields[len(fields)-1]) != "" {
		return nil, fmt.Errorf("%w: invalid Git trailer framing", model.ErrIssueTrailerPermanent)
	}
	var result []model.CommitIssueTrailer
	for i := 0; i < len(fields)-1; i += 2 {
		sha := strings.TrimSpace(fields[i])
		if err := ValidateSHA(sha); err != nil {
			return nil, fmt.Errorf("%w: invalid Git trailer commit SHA", model.ErrIssueTrailerPermanent)
		}
		seen := make(map[int64]int)
		for _, line := range strings.Split(fields[i+1], "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			key = strings.ToLower(key)
			if key != "fixes" && key != "closes" && key != "refs" {
				continue
			}
			tokens := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
			ids := make([]int64, 0, len(tokens))
			valid := len(tokens) > 0
			for _, token := range tokens {
				if !strings.HasPrefix(token, "#") {
					valid = false
					break
				}
				number := strings.TrimPrefix(token, "#")
				if number == "" || number[0] < '1' || number[0] > '9' {
					valid = false
					break
				}
				id, err := strconv.ParseInt(number, 10, 64)
				if err != nil || id <= 0 {
					valid = false
					break
				}
				ids = append(ids, id)
			}
			if !valid {
				continue
			}
			for _, id := range ids {
				closeIssue := key != "refs"
				if index, ok := seen[id]; ok {
					result[index].Close = result[index].Close || closeIssue
					continue
				}
				seen[id] = len(result)
				result = append(result, model.CommitIssueTrailer{IssueID: id, SHA: sha, Close: closeIssue})
			}
		}
	}
	return result, nil
}
