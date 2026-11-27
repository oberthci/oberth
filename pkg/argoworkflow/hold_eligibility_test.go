package argoworkflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func holdPlanFor(t *testing.T, w *wf.Workflow, r admissioncontract.PrepareRequest, p Policy) admissioncontract.Plan {
	t.Helper()
	g := newGraph(t.Context(), w, r, p)
	g.build()
	g.selectSecretGrants()
	g.inventory = g.sources()
	return g.eligibleHoldPlan(true)
}
func TestBAllResolvedSecretRoutes(t *testing.T) {
	t.Parallel()
	selector := &core.SecretKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "private"}, Key: "dummy"}
	for _, route := range []string{"env", "envFrom", "volume", "projection", "workflow volume", "csi", "input artifact", "output artifact", "archive", "inherited env", "init env", "sidecar env"} {
		t.Run(route, func(t *testing.T) {
			w := simpleWorkflow()
			kind := holdSecretRoute(w, selector, route)
			r := requestFor(t, w)
			r.Grants = []admissioncontract.GrantEvidence{clusterGrant("*", kind, "private")}
			policy := Policy{}
			plan := holdPlanFor(t, w, r, policy)
			if len(plan.Entries) != 1 || plan.Entries[0].HoldEligible || plan.Entries[0].HoldRefusal == "" {
				t.Fatal("declared secret remained eligible", plan)
			}
			admitted, err := Prepare(t.Context(), r, policy, identityHooks())
			if err != nil {
				if route == "csi" || route == "inherited env" {
					return
				}
				t.Fatal("granted ordinary execution refused", err)
			}
			if admitted.View().Plan.Entries[0].HoldEligible {
				t.Fatal("secret armed after sealing")
			}
		})
	}
}
func holdSecretRoute(w *wf.Workflow, s *core.SecretKeySelector, route string) string {
	t := &w.Spec.Templates[0]
	kind := "cluster-secret"
	switch route {
	case "env", "envFrom":
		clusterLocation(w, s, route)
	case "volume", "projection", "workflow volume":
		loc := route
		if route == "workflow volume" {
			loc = "volume"
		}
		clusterLocation(w, s, loc)
		t.Container.VolumeMounts = []core.VolumeMount{{Name: "v", MountPath: "/data"}}
		if route == "workflow volume" {
			w.Spec.Volumes = t.Volumes
			t.Volumes = nil
		}
	case "csi":
		t.Volumes = []core.Volume{{Name: "v", VolumeSource: core.VolumeSource{CSI: &core.CSIVolumeSource{Driver: "example.test", NodePublishSecretRef: &s.LocalObjectReference}}}}
		t.Container.VolumeMounts = []core.VolumeMount{{Name: "v", MountPath: "/data"}}
	case "input artifact":
		clusterLocation(w, s, "s3")
	case "output artifact", "archive":
		location := wf.ArtifactLocation{S3: &wf.S3Artifact{S3Bucket: wf.S3Bucket{Endpoint: "objects.example.test", Bucket: "data", AccessKeySecret: s}, Key: "output"}}
		if route == "archive" {
			t.ArchiveLocation = &location
		} else {
			t.Outputs.Artifacts = []wf.Artifact{{Name: "data", Path: "/output", ArtifactLocation: location}}
		}
	case "inherited env", "init env", "sidecar env":
		holdSecretContainer(w, s, route)
	}
	return kind
}
func TestBEligibilityBeforeTransformAndAfterSeal(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.RetryStrategy = &wf.RetryStrategy{Limit: new(intstr.FromInt(0))}
	w.Spec.Templates[0].Container.Command = nil
	w.Spec.TemplateDefaults = &wf.Template{Container: &core.Container{Command: []string{"true"}}}
	hook := identityHooks()
	calls := 0
	hook.TransformNative = func(_ context.Context, out *wf.Workflow, in admissioncontract.TransformInput) error {
		calls++
		if len(in.Plan.Entries) != 1 || !in.Plan.Entries[0].HoldEligible || len(out.Spec.Templates[0].Container.Command) != 0 {
			t.Fatal("authored effective decision absent", in.Plan)
		}
		out.Spec.Templates[0].Container.ReadinessProbe = &core.Probe{ProbeHandler: core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"true"}}}}
		out.Spec.Templates[0].Container.Env = []core.EnvVar{{Name: "OBERTH_HOLD_PROTOCOL", Value: "1"}}
		return nil
	}
	p, err := Prepare(t.Context(), requestFor(t, w), Policy{}, hook)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !p.View().Plan.Entries[0].HoldEligible {
		t.Fatal("decision recomputed over injections", calls, p.View().Plan)
	}
	raw, err := p.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var reopened admissioncontract.PreparedView
	if err = json.Unmarshal(raw, &reopened); err != nil {
		t.Fatal(err)
	}
	if !reopened.Plan.Entries[0].HoldEligible {
		t.Fatal("frozen decision lost on restore")
	}
}
func TestBEligibilityShapeAndRetryClauses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*wf.Workflow)
	}{
		{"no command", func(w *wf.Workflow) { w.Spec.Templates[0].Container.Command = nil }},
		{"empty command", func(w *wf.Workflow) { w.Spec.Templates[0].Container.Command = []string{""} }},
		{"unresolved command", func(w *wf.Workflow) {
			w.Spec.Templates[0].Container.Command = []string{"{{inputs.parameters.command}}"}
		}},
		{"daemon", func(w *wf.Workflow) { w.Spec.Templates[0].Daemon = new(true) }},
		{"readiness", func(w *wf.Workflow) { w.Spec.Templates[0].Container.ReadinessProbe = &core.Probe{} }},
		{"startup", func(w *wf.Workflow) { w.Spec.Templates[0].Container.StartupProbe = &core.Probe{} }},
		{"liveness", func(w *wf.Workflow) { w.Spec.Templates[0].Container.LivenessProbe = &core.Probe{} }},
		{"restart", func(w *wf.Workflow) {
			w.Spec.Templates[0].Container.RestartPolicy = new(core.ContainerRestartPolicyAlways)
		}},
		{"restart rules", func(w *wf.Workflow) {
			w.Spec.Templates[0].Container.RestartPolicyRules = []core.ContainerRestartRule{{}}
		}},
		{"unbounded", func(w *wf.Workflow) { w.Spec.Templates[0].RetryStrategy = &wf.RetryStrategy{} }},
		{"negative limit", func(w *wf.Workflow) {
			w.Spec.Templates[0].RetryStrategy = &wf.RetryStrategy{Limit: new(intstr.FromInt(-1))}
		}},
		{"unresolved limit", func(w *wf.Workflow) {
			w.Spec.Templates[0].RetryStrategy = &wf.RetryStrategy{Limit: new(intstr.FromString("{{inputs.parameters.limit}}"))}
		}},
		{"inherited unbounded", func(w *wf.Workflow) { w.Spec.RetryStrategy = &wf.RetryStrategy{} }},
		{"expression", func(w *wf.Workflow) {
			w.Spec.Templates[0].RetryStrategy = &wf.RetryStrategy{Limit: new(intstr.FromInt(1)), Expression: "true"}
		}},
		{"OnError", func(w *wf.Workflow) {
			w.Spec.Templates[0].RetryStrategy = &wf.RetryStrategy{Limit: new(intstr.FromInt(1)), RetryPolicy: wf.RetryPolicyOnError}
		}},
		{"container set", func(w *wf.Workflow) {
			w.Spec.Templates[0].Container = nil
			w.Spec.Templates[0].ContainerSet = &wf.ContainerSetTemplate{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := simpleWorkflow()
			tc.change(w)
			p := holdPlanFor(t, w, requestFor(t, w), Policy{})
			for _, e := range p.Entries {
				if e.HoldEligible || utf8.RuneCountInString(e.HoldRefusal) > 40 {
					t.Fatal(e)
				}
			}
		})
	}
	for _, policy := range []wf.RetryPolicy{"", wf.RetryPolicyAlways, wf.RetryPolicyOnFailure} {
		w := simpleWorkflow()
		w.Spec.Templates[0].RetryStrategy = &wf.RetryStrategy{Limit: new(intstr.FromInt(0)), RetryPolicy: policy}
		if !holdPlanFor(t, w, requestFor(t, w), Policy{}).Entries[0].HoldEligible {
			t.Fatal("supported retry refused", policy)
		}
	}
}
func TestBHoldNamesAreReserved(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"volume", "bin volume", "container"} {
		t.Run(route, func(t *testing.T) {
			w := simpleWorkflow()
			h := &w.Spec.Templates[0]
			switch route {
			case "bin volume":
				h.Volumes = []core.Volume{{Name: "oberth-hold-bin", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
			case "volume":
				h.Volumes = []core.Volume{{Name: "oberth-hold-state", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
			case "container":
				h.InitContainers = []wf.UserContainer{{Container: core.Container{Name: "oberth-hold-install", Image: "example.test/image:1"}}}
			}
			if err := (Policy{}).admit(t.Context(), w, requestFor(t, w), identityHooks()); err == nil {
				t.Fatal("reserved hold namespace admitted", route)
			}
		})
	}
}
func TestBMountedSecretDecisionAndResidualState(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	s := &core.SecretKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "private"}, Key: "dummy"}
	clusterLocation(w, s, "volume")
	if !holdPlanFor(t, w, requestFor(t, w), Policy{}).Entries[0].HoldEligible {
		t.Fatal("unmounted volume treated as step-readable")
	}
	// An ordinary workspace can contain previously written sensitive bytes. Only
	// declared inputs are classified; inspecting shared files is not secret-free.
	w.Spec.Templates[0].Volumes = []core.Volume{{Name: "work", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
	w.Spec.Templates[0].Container.VolumeMounts = []core.VolumeMount{{Name: "work", MountPath: "/data"}}
	if !holdPlanFor(t, w, requestFor(t, w), Policy{}).Entries[0].HoldEligible {
		t.Fatal("ordinary readable state refused")
	}
	p := holdPlanFor(t, w, requestFor(t, w), Policy{})
	if strings.Contains(p.Entries[0].HoldRefusal, "secret-free") {
		t.Fatal("invented data certification")
	}
}

func holdSecretContainer(w *wf.Workflow, s *core.SecretKeySelector, route string) {
	c := core.Container{Name: "observer", Image: "example.test/image:1", Command: []string{"true"}, Env: []core.EnvVar{{Name: "PRIVATE", ValueFrom: &core.EnvVarSource{SecretKeyRef: s}}}}
	switch route {
	case "inherited env":
		w.Spec.TemplateDefaults = &wf.Template{Container: &c}
	case "init env":
		w.Spec.Templates[0].InitContainers = []wf.UserContainer{{Container: c}}
	case "sidecar env":
		w.Spec.Templates[0].Sidecars = []wf.UserContainer{{Container: c}}
	}
}
func TestBEligibilityDynamicPlaceholders(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Entrypoint = "group"
	w.Spec.Arguments.Parameters = []wf.Parameter{{Name: "items", Value: wf.AnyStringPtr("[1,2]")}}
	w.Spec.Templates = append(w.Spec.Templates, wf.Template{Name: "group", DAG: &wf.DAGTemplate{Tasks: []wf.DAGTask{{Name: "loop", Template: "main", WithParam: "{{workflow.parameters.items}}"}}}})
	p, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	if err != nil {
		t.Fatal(err)
	}
	entries := p.View().Plan.Entries
	if len(entries) != 1 || !entries[0].Dynamic || entries[0].HoldEligible || entries[0].HoldRefusal != holdLoop {
		t.Fatal("dynamic leaf remained eligible", entries)
	}
	script := simpleWorkflow()
	script.Spec.Templates[0].Container = nil
	script.Spec.Templates[0].Script = &wf.ScriptTemplate{Container: core.Container{Image: "example.test/image:1", Command: []string{"sh"}}, Source: "exit 7"}
	p, err = Prepare(t.Context(), requestFor(t, script), Policy{}, identityHooks())
	if err != nil || !p.View().Plan.Entries[0].HoldEligible {
		t.Fatal("explicit script refused", err)
	}
}

func TestBProjectedTokenEligibility(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{false, true} {
		w, fixture := tokenWorkflow()
		w.Spec.Templates[0].Container.Command = []string{"true"}
		w.Spec.Templates[0].Inputs.Artifacts = nil
		w.Spec.Templates[0].Container.WorkingDir = ""
		if shared {
			w.Spec.Volumes = w.Spec.Templates[0].Volumes
			w.Spec.Templates[0].Volumes = nil
		}
		r := requestFor(t, w)
		r.Grants = fixture.Grants
		policy := fixture.Policy
		p := holdPlanFor(t, w, r, policy)
		if p.Entries[0].HoldEligible {
			t.Fatal("mounted token eligible", shared)
		}
		_, err := Prepare(t.Context(), r, policy, identityHooks())
		if shared && err == nil {
			t.Fatal("workflow token admission changed")
		}
		if !shared && err != nil {
			t.Fatal("granted ordinary token refused", err)
		}
	}
}

func TestBUnknownHoldEligibility(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	g := newGraph(t.Context(), w, requestFor(t, w), Policy{})
	g.build()
	g.inventory = g.sources()
	if g.holdRefusal(admissioncontract.PlanEntry{Template: "missing"}) == "" {
		t.Fatal("unknown template eligible")
	}
	authored := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "first", HoldEligible: true}, {Path: "second", HoldRefusal: "reads a secret"}}}
	final := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "second"}, {Path: "first"}, {Path: "unknown"}}}
	got := frozenHoldPlan(final, authored)
	if got.Entries[0].HoldEligible || got.Entries[0].HoldRefusal != "reads a secret" || !got.Entries[1].HoldEligible || got.Entries[2].HoldEligible {
		t.Fatal("decision lost plan identity", got)
	}
}

func TestBWorkflowSecretStillNeedsGrant(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Volumes = []core.Volume{{Name: "private", VolumeSource: core.VolumeSource{Secret: &core.SecretVolumeSource{SecretName: "private"}}}}
	for _, mounted := range []bool{false, true} {
		if mounted {
			w.Spec.Templates[0].Container.VolumeMounts = []core.VolumeMount{{Name: "private", MountPath: "/private"}}
		}
		if err := (Policy{}).admit(t.Context(), w, requestFor(t, w), identityHooks()); err == nil {
			t.Fatal("secret declaration bypassed grant", mounted)
		}
	}
}

func TestBMissingArtifactOrigin(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	g := newGraph(t.Context(), w, requestFor(t, w), Policy{})
	g.build()
	g.inventory = g.sources()
	n := g.byKey["main"]
	n.template.Inputs.Artifacts = []wf.Artifact{{Name: "unresolved"}}
	if !g.holdUsesArtifacts(n) {
		t.Fatal("artifact without resolved source eligible")
	}
}
