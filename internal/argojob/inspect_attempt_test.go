package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"slices"
	"testing"
	"time"
)

func TestBResolvedAttemptEligibility(t *testing.T) {
	t.Parallel()
	leaf := wf.Template{Name: "build", Container: &core.Container{Command: []string{"sh"}}, RetryStrategy: &wf.RetryStrategy{Limit: new(intstr.FromInt(2)), Backoff: &wf.Backoff{MaxDuration: "1s"}}}
	p := nativebinding.Preparation{Assist: true, HoldTTL: 20 * time.Minute, HelperImage: "oberth.ci/helper:1"}
	if err := armHold(&leaf, &wf.Workflow{}, p, admissioncontract.PlanEntry{HoldEligible: true}); err != nil {
		t.Fatal(err)
	}
	w := &wf.Workflow{Spec: wf.WorkflowSpec{Templates: []wf.Template{leaf}}, Status: wf.WorkflowStatus{Nodes: wf.Nodes{}}}
	w.Name = "run"
	retry := wf.NodeStatus{ID: "retry", Name: "run.build", Type: wf.NodeTypeRetry, Children: []string{"zero", "one", "two"}}
	w.Status.Nodes[retry.ID] = retry
	for i, id := range retry.Children {
		n := wf.NodeStatus{ID: id, Name: retry.Name + "(" + string(rune('0'+i)) + ")", Type: wf.NodeTypePod, TemplateName: "build"}
		w.Status.Nodes[id] = n
	}
	if holdArmed(w, w.Status.Nodes["zero"]) || holdArmed(w, w.Status.Nodes["one"]) || !holdArmed(w, w.Status.Nodes["two"]) {
		t.Fatal("attempt order not from native parent")
	}
	delete(w.Status.Nodes, "two")
	if holdArmed(w, w.Status.Nodes["one"]) {
		t.Fatal("early cutoff or maxDuration borrowed missing last child")
	}
	retry.Children = nil
	w.Status.Nodes[retry.ID] = retry
	if holdArmed(w, w.Status.Nodes["one"]) {
		t.Fatal("missing native attempt index accepted")
	}
	for _, r := range []*wf.RetryStrategy{{}, {Limit: new(intstr.FromInt(2)), Expression: "true"}} {
		t := wf.Template{Name: "build", Container: &core.Container{Command: []string{"true"}}, RetryStrategy: r}
		if err := armHold(&t, &wf.Workflow{}, p, admissioncontract.PlanEntry{HoldEligible: false}); err != nil {
			panic(err)
		}
		if !slices.Equal(t.Container.Command, []string{"true"}) {
			panic("ineligible retry armed")
		}
	}
}
func TestBAllTemplateUsesMustBeEligible(t *testing.T) {
	t.Parallel()
	for _, entries := range [][]admissioncontract.PlanEntry{
		{{Template: "build", HoldEligible: true}, {Template: "build", HoldEligible: false}},
		{{Template: "build", HoldEligible: false}, {Template: "build", HoldEligible: true}},
	} {
		folded, known := planTemplate(admissioncontract.Plan{Entries: entries}, "build")
		if !known || folded.HoldEligible {
			t.Fatal("one use bypassed another", folded)
		}
	}
	if unknown, known := planTemplate(admissioncontract.Plan{}, "unknown"); known || unknown.HoldEligible {
		t.Fatal("unknown template eligible")
	}
}
func TestBCanonicalUIDAndScriptArguments(t *testing.T) {
	t.Parallel()
	main := core.Container{Command: []string{"sh", "-e"}, Args: []string{"authored"}, Env: []core.EnvVar{{Name: "OBERTH_HOLD_POD_UID", Value: "foreign"}, {Name: "OBERTH_HOLD_POD_UID", Value: "foreign2"}}}
	leaf := wf.Template{Script: &wf.ScriptTemplate{Container: main, Source: "exit 7"}}
	p := nativebinding.Preparation{Assist: true, HelperImage: "oberth.ci/helper:1"}
	if err := armHold(&leaf, &wf.Workflow{}, p, admissioncontract.PlanEntry{HoldEligible: true}); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range leaf.Script.Env {
		if e.Name == "OBERTH_HOLD_POD_UID" {
			count++
			if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.FieldRef.FieldPath != "metadata.uid" {
				t.Fatal(e)
			}
		}
	}
	if count != 1 || !slices.Equal(leaf.Script.Args, main.Args) || !slices.Equal(leaf.Script.Command, []string{holdBinary, "run", "--", "sh", "-e"}) {
		t.Fatal(leaf.Script)
	}
}

func TestBArmingConfigurationBounds(t *testing.T) {
	t.Parallel()
	for _, ttl := range []time.Duration{-time.Second, time.Nanosecond, 4*time.Hour + time.Second} {
		leaf := wf.Template{Container: &core.Container{Command: []string{"true"}}}
		if armHold(&leaf, &wf.Workflow{}, nativebinding.Preparation{Assist: true, HoldTTL: ttl, HelperImage: "oberth.ci/helper:1"}, admissioncontract.PlanEntry{HoldEligible: true}) == nil {
			t.Fatal("invalid helper TTL accepted", ttl)
		}
	}
}
