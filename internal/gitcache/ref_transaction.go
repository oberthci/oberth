package gitcache

import (
	"context"
	"fmt"
	"strings"
)

// refTransaction keeps native commands and their owned lock paths together.
// The caller holds the repository gate or owns a private staging store.
func (c *Cache) refTransaction(ctx context.Context, store string, updates []RefUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	var tx strings.Builder
	tx.WriteString("start\n")
	var names []string
	deletes := false
	for _, u := range updates {
		names = append(names, u.Ref)
		operation := "update"
		if u.NewSHA == "" {
			operation = "delete"
			deletes = true
		}
		fmt.Fprintf(&tx, "%s %s", operation, u.Ref)
		if u.NewSHA != "" {
			fmt.Fprintf(&tx, " %s", u.NewSHA)
		}
		if u.OldSHA != "" {
			fmt.Fprintf(&tx, " %s", u.OldSHA)
		}
		tx.WriteByte('\n')
	}
	tx.WriteString("prepare\ncommit\n")
	return c.run(ctx, command{dir: store, args: []string{"update-ref", "--stdin", "--no-deref"},
		input: strings.NewReader(tx.String()), refWrites: names, refDeletes: deletes})
}
