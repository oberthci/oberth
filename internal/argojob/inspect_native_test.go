package argojob_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	core "k8s.io/api/core/v1"
)

func inspectFixture(t *testing.T) (argojob.Config, *fake.Workflows, *fake.Pods, nativebinding.RunBinding) {
	return inspectFixtureMode(t, true)
}
func inspectFixtureMode(t *testing.T, assist bool) (argojob.Config, *fake.Workflows, *fake.Pods, nativebinding.RunBinding) {
	t.Helper()
	p, e, w := fixture(t, nativebinding.Preparation{Assist: assist, HoldTTL: 20 * time.Minute, HelperImage: "oberth.ci/helper:1"})
	b, err := e.Create(t.Context(), p)
	compositionOK(t, err)
	n := nativeNode("held", "", wf.NodeRunning, 1)
	n.Name = w.Object.Name
	n.TemplateName = "build"
	w.Object.Status.Nodes = wf.Nodes{n.ID: n}
	w.Object.Status.Phase = wf.WorkflowRunning
	pod := nativePod(w.Object, n, 0)
	pod.Status.Phase = core.PodRunning
	pod.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
	pod.Status.ContainerStatuses[0].Ready = true
	pods := &fake.Pods{Object: pod, ExecFunc: func(_ context.Context, r argojob.ExecRequest) error {
		if len(r.Command) < 4 || r.Command[0] != "/oberth-hold/bin/oberth-hold" || r.Command[2] != "--pod-uid" || r.Command[3] != string(pod.UID) {
			t.Fatal("unfenced helper request", r.Command)
		}
		if r.Command[1] == "status" {
			_, _ = fmt.Fprint(r.Err, "oberth-hold: ")
			return json.NewEncoder(r.Err).Encode(map[string]any{"t": "held", "heldSince": time.Unix(100, 0), "ttl": 1200, "remaining_ms": 60000})
		}
		kind := "exit"
		code := 7
		if r.Command[1] == "release" {
			kind = "closed"
			code = 0
		}
		_, err := fmt.Fprintf(r.Err, "oberth-hold: {\"t\":%q,\"code\":%d}\n", kind, code)
		return err
	}}
	owner := memoryAdmission(t, p)
	c := argojob.Config{Runs: owner, Progress: owner, Workflows: w, Pods: pods, Cache: &fake.Cache{WorkflowObject: w.Object, PodObjects: []*core.Pod{pod}}, Now: compositionClock, Reporter: &reportSpy{}}
	return c, w, pods, b
}
func TestBReadinessDiscovery(t *testing.T) {
	t.Parallel()
	c, w, p, b := inspectFixture(t)
	e := argojob.New(c)
	p.Object.Status.ContainerStatuses = append(p.Object.Status.ContainerStatuses, core.ContainerStatus{Name: "sidecar", Ready: false})
	// The copied informer object must see each native readiness change.
	cache := c.Cache.(*fake.Cache)
	cache.PodObjects[0] = p.Object
	holds, err := e.Holds(t.Context(), b)
	compositionOK(t, err)
	if len(holds) != 1 || holds[0].AvailabilityUnknown || holds[0].RemainingMS != 60000 {
		t.Fatal(holds)
	}
	for _, ready := range []bool{false, true, false} {
		p.Object.Status.ContainerStatuses[0].Ready = ready
		holds, err = e.Holds(t.Context(), b)
		compositionOK(t, err)
		if (len(holds) == 1) != ready {
			t.Fatal("aggregate Pod readiness used", holds, ready)
		}
	}
	p.Object.Status.ContainerStatuses[0].Ready = true
	p.ExecFunc = func(context.Context, argojob.ExecRequest) error { return errors.New("stream lost") }
	holds, err = e.Holds(t.Context(), b)
	compositionOK(t, err)
	if len(holds) != 1 || !holds[0].AvailabilityUnknown || !holds[0].HeldSince.IsZero() {
		t.Fatal("stale true guessed availability", holds)
	}
	_ = w
	unarmed, _, _, unarmedBinding := inspectFixtureMode(t, false)
	holds, err = argojob.New(unarmed).Holds(t.Context(), unarmedBinding)
	compositionOK(t, err)
	if len(holds) != 0 {
		t.Fatal("unarmed Pod discovered", holds)
	}
}
func TestBActionNativeBindingAndRelay(t *testing.T) {
	t.Parallel()
	c, _, p, b := inspectFixture(t)
	e := argojob.New(c)
	rows, err := e.Holds(t.Context(), b)
	compositionOK(t, err)
	h := rows[0]
	result, err := e.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: h, Command: []string{"sh", "-c", "echo output > file"}})
	compositionOK(t, err)
	if result.ExitCode != 7 || result.Stderr != "" {
		t.Fatal(result)
	}
	compositionOK(t, e.Abandon(t.Context(), b, h))
	calls := 0
	p.ExecFunc = func(context.Context, argojob.ExecRequest) error { calls++; return nil }
	for _, foreign := range []string{"", "replacement"} {
		changed := h
		changed.PodUID = foreign
		if _, err = e.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: changed, Command: []string{"touch", "effect"}}); err == nil {
			t.Fatal("foreign UID dispatched")
		}
	}
	checkForeignHoldFields(t, e, argojob.HoldExecRequest{Binding: b, Hold: h, Command: []string{"touch", "effect"}})
	if calls != 0 {
		t.Fatal("refused identity reached exec", calls)
	}
}
func TestBCanonicalArmingPreservesNativeInputs(t *testing.T) {
	t.Parallel()
	p, _, _ := fixture(t, nativebinding.Preparation{Assist: true, HoldTTL: 20 * time.Minute, HelperImage: "oberth.ci/helper:1"})
	v := p.View()
	main := v.Workflow.Spec.Templates[0].Container
	if !v.Plan.Entries[0].HoldArmed || !v.Plan.Entries[0].HoldEligible || !slices.Equal(main.Command, []string{"/oberth-hold/bin/oberth-hold", "run", "--", "true"}) {
		t.Fatal(main.Command, v.Plan)
	}
	if v.Workflow.Spec.ActiveDeadlineSeconds != nil || v.Workflow.Spec.Templates[0].ActiveDeadlineSeconds != nil || v.Workflow.Spec.Templates[0].RetryStrategy != nil {
		t.Fatal("native lifetime changed")
	}
	checkArmedEnvironment(t, main)
	if main.ReadinessProbe == nil || main.ReadinessProbe.TimeoutSeconds != 2 || main.ReadinessProbe.PeriodSeconds != 5 {
		t.Fatal(main.ReadinessProbe)
	}
	install := v.Workflow.Spec.Templates[0].InitContainers[0]
	if !slices.Equal(install.Command, []string{"oberth", "internal-hold", "install", "--to", "/oberth-hold/bin"}) {
		t.Fatal(install.Command)
	}
}

func checkArmedEnvironment(t *testing.T, main *core.Container) {
	t.Helper()
	values := map[string]int{}
	for _, env := range main.Env {
		values[env.Name]++
	}
	for _, name := range []string{"ATTEMPT", "LIMIT", "TTL", "POD_UID", "PROTOCOL"} {
		if values["OBERTH_HOLD_"+name] != 1 {
			t.Fatal(values)
		}
	}
}

func TestBNoticeRefusalDoesNotBlockNativeResult(t *testing.T) {
	t.Parallel()
	c, w, p, b := inspectFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	saved, finished := 0, 0
	c.Reporter = &reportSpy{HeldFunc: func(_ context.Context, h model.HeldNotice) error {
		saved++
		if h.PodUID != string(p.Object.UID) || h.TTLSeconds != 1200 || !h.HeldSince.Equal(time.Unix(100, 0)) {
			t.Error(h)
		}
		w.Object.Status.Phase = wf.WorkflowFailed
		n := w.Object.Status.Nodes["held"]
		n.Phase = wf.NodeFailed
		w.Object.Status.Nodes[n.ID] = n
		p.Object.Status.ContainerStatuses[0].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 7}}
		c.Cache.(*fake.Cache).Changed(w.Object.Name)
		return errors.New("notice save refused")
	}, FinishedFunc: func(context.Context, string, argojob.Observation) error { finished++; cancel(); return nil }}
	if err := c.Runs.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, b); err != nil {
		t.Fatal(err)
	}
	_ = argojob.New(c).Run(ctx)
	if saved != 1 || finished != 1 {
		t.Fatal("notice failure blocked or retried terminal observation", saved, finished)
	}
}

func TestBRelayCapsBothStreamsAndKeepsStderr(t *testing.T) {
	t.Parallel()
	c, _, p, b := inspectFixture(t)
	e := argojob.New(c)
	rows, err := e.Holds(t.Context(), b)
	compositionOK(t, err)
	p.ExecFunc = func(_ context.Context, r argojob.ExecRequest) error {
		_, _ = io.WriteString(r.Out, "abcd")
		_, err := io.WriteString(r.Err, "ef\noberth-hold: {\"t\":\"exit\",\"code\":0}\n")
		return err
	}
	result, err := e.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: rows[0], Command: []string{"true"}, MaxOutput: 5})
	compositionOK(t, err)
	if result.Stdout != "abcd" || result.Stderr != "e" || !result.Truncated {
		t.Fatal(result)
	}
	result, err = e.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: rows[0], Command: []string{"true"}})
	compositionOK(t, err)
	if result.Stderr != "ef\n" || result.Truncated {
		t.Fatal("stderr or protocol trailer changed the payload", result)
	}
}

func checkForeignHoldFields(t *testing.T, e *argojob.Executor, request argojob.HoldExecRequest) {
	t.Helper()
	for _, field := range []string{"run", "node", "pod"} {
		changed := request.Hold
		switch field {
		case "run":
			changed.RunID = "foreign"
		case "node":
			changed.NodeID = "foreign"
		case "pod":
			changed.PodName = "foreign"
		}
		if _, err := e.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: request.Binding, Hold: changed, Command: request.Command}); err == nil {
			t.Fatal("foreign identity dispatched", field)
		}
	}
}

func TestArmingFactRequiresEffectiveAssist(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		p, _, _ := fixture(t, nativebinding.Preparation{Assist: enabled, HelperImage: "oberth.ci/helper:1"})
		entry := p.View().Plan.Entries[0]
		if !entry.HoldEligible || entry.HoldArmed != enabled {
			t.Fatal("request eligibility replaced actual arming", entry, enabled)
		}
	}
}
