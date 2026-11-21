package argojob

import (
	"context"
	"slices"
	"strconv"
	"strings"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func heldPod(p *core.Pod) bool {
	return slices.ContainsFunc(p.Status.ContainerStatuses, func(c core.ContainerStatus) bool { return c.Name == "main" && c.State.Running != nil && c.Ready })
}
func holdArmed(w *wf.Workflow, n wf.NodeStatus) bool {
	t := nodeTemplate(w, n, admissioncontract.MaxNestingDepth)
	if t == nil {
		return false
	}
	main := holdMain(t)
	if main == nil || len(main.Command) < 4 || !slices.Equal(main.Command[:3], []string{holdBinary, "run", "--"}) {
		return false
	}
	if main.ReadinessProbe == nil || main.ReadinessProbe.Exec == nil || !slices.Equal(main.ReadinessProbe.Exec.Command, []string{holdBinary, "status", "--probe"}) {
		return false
	}
	values := armedEnvironment(main)
	if values == nil {
		return false
	}
	return armedAttempt(w, n, t, values)
}
func armedEnvironment(main *core.Container) map[string]core.EnvVar {
	values := map[string]core.EnvVar{}
	for _, v := range main.Env {
		if !strings.HasPrefix(v.Name, "OBERTH_HOLD_") {
			continue
		}
		if _, exists := values[v.Name]; exists {
			return nil
		}
		values[v.Name] = v
	}
	uid := values["OBERTH_HOLD_POD_UID"].ValueFrom
	if uid == nil || uid.FieldRef == nil || uid.FieldRef.FieldPath != "metadata.uid" || values["OBERTH_HOLD_PROTOCOL"].Value != "1" {
		return nil
	}
	return values
}
func armedAttempt(w *wf.Workflow, n wf.NodeStatus, t *wf.Template, values map[string]core.EnvVar) bool {
	limit, err := strconv.Atoi(values["OBERTH_HOLD_LIMIT"].Value)
	if err != nil || limit < 0 {
		return false
	}
	if values["OBERTH_HOLD_ATTEMPT"].Value == "0" {
		return limit == 0 && t.RetryStrategy == nil && w.Spec.RetryStrategy == nil
	}
	if values["OBERTH_HOLD_ATTEMPT"].Value != "{{retries}}" {
		return false
	}
	order := indexPaths(w)[n.ID].order
	return len(order) > 0 && order[len(order)-1] == limit
}
func holdCandidates(w *wf.Workflow, pods []*core.Pod, run string) []model.LiveHold {
	out := []model.LiveHold{}
	paths := indexPaths(w)
	for _, n := range w.Status.Nodes {
		p := boundPod(w, n, pods)
		if p == nil || !heldPod(p) || !holdArmed(w, n) {
			continue
		}
		out = append(out, model.LiveHold{HeldNotice: model.HeldNotice{RunID: run, Step: paths[n.ID].name, NodeID: n.ID, PodUID: string(p.UID)}, PodName: p.Name})
	}
	slices.SortFunc(out, func(a, b model.LiveHold) int {
		if a.Step < b.Step {
			return -1
		}
		if a.Step > b.Step {
			return 1
		}
		return 0
	})
	return out
}
func (e *Executor) Holds(ctx context.Context, b nativebinding.RunBinding) ([]model.LiveHold, error) {
	w, err := e.unpacked(ctx, b)
	if err != nil {
		return nil, err
	}
	out := holdCandidates(w, e.config.Cache.Pods(w.Name), b.RunID)
	for i := range out {
		e.queryHold(ctx, &out[i])
	}
	return out, nil
}
func (e *Executor) queryHold(ctx context.Context, h *model.LiveHold) {
	ctx, cancel := context.WithTimeout(ctx, e.config.ObservationTimeout)
	defer cancel()
	out := &boundedOutput{limit: 4096}
	err := e.config.Pods.Exec(ctx, ExecRequest{Pod: h.PodName, Container: "main", Command: []string{holdBinary, "status", "--pod-uid", h.PodUID}, Out: &boundedOutput{limit: 4096}, Err: out})
	status, _, ok := holdReply(out.body.Bytes())
	if err != nil || !ok || status.Type != "held" || status.HeldSince.IsZero() || status.TTL <= 0 || status.TTL > 14400 || status.RemainingMS < 0 {
		h.AvailabilityUnknown = true
		return
	}
	h.HeldSince, h.TTLSeconds, h.RemainingMS = status.HeldSince, status.TTL, status.RemainingMS
}
func (e *Executor) verifyHold(ctx context.Context, b nativebinding.RunBinding, h model.LiveHold) error {
	if h.RunID != b.RunID {
		return notHeld(nil)
	}
	w, err := e.unpacked(ctx, b)
	if err != nil {
		return err
	}
	p, err := e.config.Pods.Get(ctx, h.PodName, meta.GetOptions{})
	if err != nil || p == nil {
		return notHeld(err)
	}
	n, ok := w.Status.Nodes[h.NodeID]
	if !ok || h.PodUID == "" || !holdBindingMatches(w, n, p, h) {
		return notHeld(nil)
	}
	return nil
}
func holdBindingMatches(w *wf.Workflow, n wf.NodeStatus, p *core.Pod, h model.LiveHold) bool {
	return n.ID == h.NodeID && p.Name == h.PodName && string(p.UID) == h.PodUID && nodePath(w, n) == h.Step && podOwned(w, n, p) && holdArmed(w, n)
}

func (e *Executor) Abandon(ctx context.Context, b nativebinding.RunBinding, h model.LiveHold) error {
	if err := e.verifyHold(ctx, b, h); err != nil {
		return err
	}
	result, err := e.relayHold(ctx, h, []string{holdBinary, "release", "--pod-uid", h.PodUID}, DefaultExecOutput)
	if err != nil {
		return err
	}
	if result.ExitCode != 122 {
		return notHeld(nil)
	}
	return nil
}
func (e *Executor) reportHolds(ctx context.Context, t *tracking, w *wf.Workflow, pods []*core.Pod) {
	for _, h := range holdCandidates(w, pods, t.binding.RunID) {
		key := h.Step + "/" + h.NodeID + "/" + h.PodUID
		if t.holds[key] {
			continue
		}
		t.holds[key] = true
		e.queryHold(ctx, &h)
		if !h.AvailabilityUnknown {
			_ = e.config.Reporter.Held(ctx, h.HeldNotice)
		}
	}
}
