package argojob_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"strings"
	"testing"
)

func TestReviewDefaultsOnlyForArmedTemplate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		kind          string
		assist, armed bool
	}{{"review-defaults", false, false}, {"review-defaults-ineligible", true, false}, {"review-defaults", true, true}} {
		p, _, _ := fixtureKind(t, test.kind, nativebinding.Preparation{Assist: test.assist, HelperImage: "oberth.ci/helper:1"})
		main := p.View().Workflow.Spec.Templates[0].Container
		if test.armed {
			if len(main.Command) < 4 || main.Command[3] != "sh" {
				t.Fatal("effective command not armed", main)
			}
		} else if len(main.Command) != 0 || len(main.Env) != 0 {
			t.Fatal("unarmed template defaults rewritten", main)
		}
	}
}
func TestReviewInheritedRetryArming(t *testing.T) {
	t.Parallel()
	p, _, _ := fixtureKind(t, "review-inherited-retry", nativebinding.Preparation{Assist: true, HelperImage: "oberth.ci/helper:1"})
	values := map[string]string{}
	for _, v := range p.View().Workflow.Spec.Templates[0].Container.Env {
		values[v.Name] = v.Value
	}
	if values["OBERTH_HOLD_ATTEMPT"] != "{{retries}}" || values["OBERTH_HOLD_LIMIT"] != "2" {
		t.Fatal(values)
	}
}
func TestReviewExecEndingsPreserveOutput(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"timeout", "start_failed"} {
		c, _, pods, b := inspectFixture(t)
		e := argojob.New(c)
		rows, err := e.Holds(t.Context(), b)
		compositionOK(t, err)
		pods.ExecFunc = func(_ context.Context, q argojob.ExecRequest) error {
			_, _ = fmt.Fprint(q.Out, "partial output")
			_, _ = fmt.Fprintf(q.Err, "partial error\noberth-hold: {\"t\":%q}\n", kind)
			return nil
		}
		out, err := e.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: rows[0], Command: []string{"command"}})
		want := "Command timed out"
		if kind == "start_failed" {
			want = "Command could not start in the held step"
		}
		problem := publicerr.Classify(err)
		if err == nil || problem.Reason() != want || out.Stdout != "partial output" || out.Stderr != "partial error\n" || !strings.Contains(problem.Detail, "partial output") {
			t.Fatal(out, err)
		}
	}
}
func TestReviewSameUIDForeignPodRefused(t *testing.T) {
	t.Parallel()
	c, _, pods, b := inspectFixture(t)
	e := argojob.New(c)
	rows, err := e.Holds(t.Context(), b)
	compositionOK(t, err)
	pods.Object.OwnerReferences[0].UID = "foreign"
	calls := 0
	pods.ExecFunc = func(context.Context, argojob.ExecRequest) error { calls++; return nil }
	if _, err = e.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: rows[0], Command: []string{"touch", "effect"}}); err == nil || calls != 0 {
		t.Fatal(err, calls)
	}
}

func TestReviewDormantWorkflowBytes(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"review-defaults", "review-defaults-ineligible"} {
		p, _, _ := fixtureKind(t, kind, nativebinding.Preparation{Assist: kind == "review-defaults-ineligible", HelperImage: "oberth.ci/helper:1"})
		raw, err := json.Marshal(p.View().Workflow)
		compositionOK(t, err)
		got := fmt.Sprintf("%x", sha256.Sum256(raw))
		want := map[string]string{"review-defaults": "a19e3ea421b0eff7ca539d206534c3957c2d8849e5590a05de54f50696c0dfd9", "review-defaults-ineligible": "563a36e7a85334a76ac47669cc4ce1417f230852a504cbd9c62deb4a66dc1471"}[kind]
		t.Log(kind, got)
		if got != want {
			t.Fatal("unarmed workflow differs from main sealing", kind, got, want)
		}
	}
}
