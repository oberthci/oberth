package gitcache_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveAgentDeletionRetryWithoutRunOption(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	admissions, deliveries := 0, 0
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { admissions++; return nil }
	ref := "refs/heads/topic"
	f.Git(t, f.Bare, "update-ref", ref, f.Commit)
	c := thirdInit(t, cfg)
	for _, run := range []bool{true, false} {
		var options []string
		if run {
			options = []string{"oberth.run=1"}
		}
		r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: ref, OldSHA: f.Commit}}, options)
		r.AlwaysRun = false
		var status, feedback bytes.Buffer
		r.Output, r.Diagnostics = &status, &feedback
		r.Deliver = func(ctx context.Context, _ gitcache.ReceiveEvent) ([]string, error) {
			deliveries++
			_, err := c.Publish(ctx, publicationFor(f, ref, "", f.Commit))
			return nil, err
		}
		err := c.Receive(t.Context(), r)
		if run {
			if err == nil || deliveries != 0 {
				t.Fatal("deletion with run option was accepted", err, deliveries)
			}
			checkDeletionGuidance(t, "topic", false, [2]string{status.String(), feedback.String()})
		} else if err != nil || deliveries != 1 {
			t.Fatal("runless deletion retry failed", err, deliveries, feedback.String())
		}
	}
	if admissions != 0 || publicReceiveRefs(t, f) != "refs/heads/main "+f.Commit {
		t.Fatal("deletion ran CI or failed to preserve main", admissions, publicReceiveRefs(t, f))
	}
	if got := f.Git(t, f.Bare, "for-each-ref", "--format=%(refname) %(objectname)"); got != "refs/heads/main "+f.Commit {
		t.Fatal("runless deletion did not remove only the forge topic", got)
	}
}
