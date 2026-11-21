package gitcache_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/gitcache"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestMissingReceiveBeforeRefusesWithoutChangingRefs(t *testing.T) {
	for _, state := range []string{"reserved", "ready", "rejected"} {
		t.Run(state, func(t *testing.T) {
			f, c, _ := completionFixture(t)
			record, next := pendingAbandonReceive(t, c, f)
			record.State, record.Before, record.Prepared = state, nil, nil
			writeReceiveEvidence(t, f, record)
			before := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/")
			err := c.RetryReceive(t.Context(), gittest.Repository, func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
				t.Error("receive without predecessors was delivered")
				return nil, nil
			})
			var corrupt *gitcache.CorruptReceiveError
			if !errors.As(err, &corrupt) {
				t.Fatal("retry accepted missing predecessors", err)
			}
			if after := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/"); after != before {
				t.Fatal("retry changed refs without predecessor evidence", before, after)
			}
			pending := inspectCorruptReceive(t, c, record.Event.ID)
			if err := c.AbandonReceive(t.Context(), gittest.Repository, pending.Event.ID); err != nil {
				t.Fatal(err)
			}
			if completionState(t, f) != "absent" {
				t.Fatal("abandon kept corrupt record")
			}
			if after := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/"); after != before {
				t.Fatal("abandon changed refs without predecessor evidence", before, after)
			}
			pushAfterAbandon(t, c, f, next)
		})
	}
}
