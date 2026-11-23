package gitcache

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// Check the sealed ref inventory in one Git process, preserving the first
// missing ref's classification and the distinction for a recorded tag.
func (c *Cache) checkArchiveObjects(ctx context.Context, p string, refs []sourcecontract.Ref) error {
	var input strings.Builder
	ordered := make([]sourcecontract.Ref, 0, len(refs))
	for _, ref := range refs {
		if ref.Name != "" {
			ordered = append(ordered, ref)
			input.WriteString(ref.ObjectSHA + "\n")
		}
	}
	if len(ordered) == 0 {
		return nil
	}
	data, err := c.capture(ctx, command{dir: p, args: []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"}, input: strings.NewReader(input.String())}, int64(len(ordered))*128)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) != len(ordered) {
		return refuse("ref object")
	}
	for i, ref := range ordered {
		fields := strings.Fields(lines[i])
		if len(fields) != 2 || fields[0] != ref.ObjectSHA {
			return refuse("ref object")
		}
		if fields[1] == "missing" {
			err := &gitport.AbsentError{Ref: ref.ObjectSHA}
			if strings.HasPrefix(ref.Name, "refs/tags/") {
				return &tagMovedError{err}
			}
			return err
		}
	}
	return nil
}
