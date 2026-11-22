package sshserver

// Delta check R4, real Git client through the receive path.

import (
	"context"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/publicerr"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Pushing a branch that is already up to date is an everyday command and
// must stay a success.
func TestFourthUpToDatePushSucceeds(t *testing.T) {
	e := newReviewEnv(t)
	e.f.Write(t, "a.txt", []byte("a"))
	e.f.CommitFile(t, "a")
	first := e.push(t, agent(e.f), remote, "HEAD:refs/heads/feature")
	if first.err != nil {
		t.Fatal("first push:", first.err, first.client, first.ssh)
	}
	second := e.push(t, agent(e.f), remote, "HEAD:refs/heads/feature")
	t.Logf("second push client output: %q", second.client)
	t.Logf("second push server text: %q", second.ssh)
	t.Logf("second push server result: %v", second.err)
	if second.err != nil {
		t.Fatal("an up-to-date push is answered with an error")
	}
}

// An ordinary refusal by the hook shows one reason, and not the text meant
// for a hook that did not run.
func TestFourthOrdinaryRefusalShowsOneReason(t *testing.T) {
	e := newReviewEnv(t)
	e.f.Write(t, "a.txt", []byte("a"))
	e.f.CommitFile(t, "a")
	res := e.push(t, agent(e.f), remote, "HEAD:refs/heads/main")
	combined := res.client + res.ssh
	t.Logf("client: %q", res.client)
	t.Logf("server text: %q", res.ssh)
	t.Logf("server result: %v", res.err)
	if res.err == nil || strings.Contains(combined, "receive gate did not run") ||
		strings.Contains(combined, "Restore the receive hook") {
		t.Fatal("a pusher refused for the default branch is told the receive hook did not run")
	}
}

func TestR5AdmissionRefusals(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "admission", true: "deadline"}[timeout], func(t *testing.T) {
			e := newReviewEnvConfigured(t, func(c *gitcache.Config) { c.AdmissionTimeout = 50 * time.Millisecond })
			reason := "pipeline is invalid"
			e.gate = func(ctx context.Context, _ gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
				if timeout {
					<-ctx.Done()
					return context.Cause(ctx)
				}
				return publicerr.New(publicerr.InvalidInput, reason, "Fix the pipeline and push again.", nil)
			}
			if timeout {
				reason = "push admission did not finish in time"
			}
			e.f.Write(t, "a.txt", []byte("a"))
			e.f.CommitFile(t, "a")
			res := e.push(t, agent(e.f), "-o", "oberth.run=1", remote, "HEAD:refs/heads/feature")
			combined := res.client + res.ssh
			if res.err == nil || strings.Count(combined, reason) != 1 || strings.Contains(combined, "Restore the receive hook") {
				t.Fatal("admission refusal lost or duplicated", res.err, combined)
			}
			if got := e.f.Git(t, e.f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/feature"); got != "" {
				t.Fatal("refused push moved its ref", got)
			}
		})
	}
}

func TestR5MissingHook(t *testing.T) {
	for _, change := range []string{"create", "update", "delete"} {
		t.Run(change, func(t *testing.T) { checkMissingHookPush(t, change) })
	}
}
func checkMissingHookPush(t *testing.T, change string) {
	t.Helper()
	e := newReviewEnvConfigured(t, func(c *gitcache.Config) {
		wrapper := filepath.Join(t.TempDir(), "git")
		script := "#!/bin/sh\ncase \" $* \" in\n*' receive-pack '*) /bin/rm -- " +
			strconv.Quote(c.HookBinary) + ";;\nesac\nexec /usr/bin/git \"$@\"\n"
		if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		c.Binary = wrapper
	})
	ref := "refs/heads/feature"
	if change != "create" {
		e.f.Git(t, e.f.Store(), "update-ref", ref, e.f.Commit)
	}
	e.f.Write(t, "a.txt", []byte("a"))
	e.f.CommitFile(t, "a")
	spec := "HEAD:" + ref
	want := e.f.Commit
	if change == "delete" {
		spec = ":" + ref
	}
	if change == "create" {
		want = ""
	}
	res := e.push(t, agent(e.f), remote, spec)
	if res.err == nil || !strings.Contains(res.ssh, "receive gate did not run") {
		t.Fatal("unchecked change lacked hook guidance", res.err, res.client, res.ssh)
	}
	if got := e.f.Git(t, e.f.Store(), "for-each-ref", "--format=%(objectname)", ref); got != want {
		t.Fatal("unchecked client push survived", got, want)
	}
}
