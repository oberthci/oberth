package gitcache_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceiveDeletionRefusalsKeepRefsAndNameTheRemedy(t *testing.T) {
	for _, empty := range []bool{false, true} {
		for _, operator := range []bool{false, true} {
			for _, branch := range []string{"topic", "main", "release/stable"} {
				t.Run(strings.Join([]string{map[bool]string{true: "bootstrap", false: "established"}[empty], map[bool]string{true: "operator", false: "agent"}[operator], branch}, "/"), func(t *testing.T) {
					checkDeletionRefusal(t, empty, operator, branch)
				})
			}
		}
	}
}

func checkDeletionRefusal(t *testing.T, empty, operator bool, branch string) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.ReservedBranches = []string{"release/*"}
	admissions, deliveries := 0, 0
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { admissions++; return nil }
	if empty {
		f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	} else {
		f.Git(t, f.Bare, "update-ref", "refs/heads/topic", f.Commit)
	}
	forgeBefore := f.Git(t, f.Bare, "for-each-ref", "--format=%(refname) %(objectname)")
	c := thirdInit(t, cfg)
	f.Git(t, f.Store(), "fetch", f.Work, "refs/heads/main:refs/heads/main")
	ref := "refs/heads/" + branch
	f.Git(t, f.Store(), "update-ref", ref, f.Commit)
	before := publicReceiveRefs(t, f)
	var options []string
	if !operator {
		options = []string{"oberth.run=1"}
	}
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: ref, OldSHA: f.Commit}}, options)
	r.AlwaysRun = operator
	if empty {
		r.Admission.SHA = ""
	}
	r.AllowDefault = operator
	if operator {
		r.Actor = "operator"
	}
	var status, feedback bytes.Buffer
	r.Output, r.Diagnostics = &status, &feedback
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { deliveries++; return nil, nil }
	if err := c.Receive(t.Context(), r); err == nil {
		t.Fatal("deletion requiring admission returned success")
	}
	checkDeletionGuidance(t, branch, operator, [2]string{status.String(), feedback.String()})
	if admissions != 0 || deliveries != 0 || publicReceiveRefs(t, f) != before {
		t.Fatal("refused deletion reached admission, delivery or mutation", admissions, deliveries)
	}
	if got := f.Git(t, f.Bare, "for-each-ref", "--format=%(refname) %(objectname)"); got != forgeBefore {
		t.Fatal("refused deletion changed forge history", got)
	}
	if completionState(t, f) != "absent" {
		t.Fatal("refused deletion retained a receive record")
	}
	if _, err := c.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
		t.Fatal("refused deletion left pending work", err)
	}
}

func checkDeletionGuidance(t *testing.T, branch string, operator bool, output [2]string) {
	t.Helper()
	want := "deletion-only push cannot run CI. Delete topic branches directly on the forge; keep default and reserved branches."
	if !operator {
		want = "deletion-only push cannot run CI. Push the deletion again without -o oberth.run=1."
	}
	if branch != "topic" {
		want = "branch deletion is protected. Keep the default and reserved branches."
	}
	reason, remedy, _ := strings.Cut(want, ". ")
	if len(reason) > 40 || len(remedy) > 120 {
		t.Fatal("deletion message exceeds its bound", len(reason), len(remedy))
	}
	if !strings.Contains(output[1], want) || !strings.Contains(output[0], "ng refs/heads/"+branch) {
		t.Fatal("deletion refusal lost its specific whole guidance", output)
	}
}
