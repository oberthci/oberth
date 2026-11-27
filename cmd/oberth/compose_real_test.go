//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktest "k8s.io/client-go/testing"
	"os/exec"
)

func TestRealCompositionPushSourceAndNativeFinish(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.freshDataDirectory(t)
	commit := f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.checkRecovery(t)
	f.checkServerParts(t)
	f.checkWiredProbes(t)
	f.checkInspectionNoWrites(t)
	f.checkProtectedNamespaces(t)
	f.enrollJoined(t)
	f.checkReservationRetry(t)
	f.certificateAccess(t, f.joinedCertificate(t), true)
	f.certificateAccess(t, f.unknownCertificate(t), false)
	f.bindClient(t)
	f.git.Git(t, f.git.Work, "push", "-o", "oberth.run=1", "oberth", "HEAD:refs/heads/topic")
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if run.Origin.Actor != "joined-agent" {
		t.Fatal("push did not use the joined profile", run.Origin.Actor)
	}
	if run.CommitSHA == nil || *run.CommitSHA != commit || len(run.Admission.SourceBindings) != 1 {
		t.Fatal("accepted source differs from push", run.RunSummary)
	}
	f.checkRetention(t, run)
	f.checkExecutorAccount(t, run)
	f.deliverSource(t, run)
	f.finishNative(t, run)
	final := f.waitRun(t, func(r stateport.Run) bool { return r.Status == stepmodel.RunPassed })
	if final.ID != run.ID || final.Origin.WorkflowUID != run.Origin.WorkflowUID {
		t.Fatal("native finish changed run identity")
	}
	f.checkReleasePreflight(t, commit)
	f.checkOnce(t)
	f.checkAuthorityRefusals(t)
	f.checkPartFailure(t, done)
	cancel()
}
func (f *composedFixture) waitRun(t *testing.T, ready func(stateport.Run) bool) stateport.Run {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		changes := observeChanges(t, f.owners.c.store)
		rows, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && ready(rows[0]) {
			run, err := f.owners.c.store.Run(t.Context(), rows[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			return run
		}
		select {
		case <-changes:
		case <-deadline.C:
			t.Fatalf("run did not reach expected state: %+v; %s", rows, f.trace.String())
		}
	}
}
func (f *composedFixture) sourcePod(t *testing.T, r stateport.Run) *core.Pod {
	t.Helper()
	yes := true
	name := r.Origin.WorkflowName
	nodeName := name
	if path := r.Admission.Plan.Entries[0].Path; path != "" {
		nodeName += "." + path
	}
	account := r.Admission.Identity.Pipeline.Name
	for _, template := range r.Admission.Workflow.Spec.Templates {
		if template.Name == r.Admission.Plan.Entries[0].Name && template.ServiceAccountName != "" {
			account = template.ServiceAccountName
		}
	}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: f.config.PipelineNamespace, UID: "pod-uid", ResourceVersion: "1", Labels: map[string]string{"workflows.argoproj.io/workflow": name}, Annotations: map[string]string{"workflows.argoproj.io/node-id": name, "workflows.argoproj.io/node-name": nodeName}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: name, UID: types.UID(r.Origin.WorkflowUID), Controller: &yes}}}, Spec: core.PodSpec{ServiceAccountName: account, Containers: []core.Container{{Name: "main"}}}, Status: core.PodStatus{Phase: core.PodRunning, ContainerStatuses: []core.ContainerStatus{{Name: "main", State: core.ContainerState{Running: &core.ContainerStateRunning{StartedAt: meta.NewTime(f.clock.Now())}}}}}}
	pod, err := f.kube.CoreV1().Pods(pod.Namespace).Create(t.Context(), pod, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The controller creates the native node before its Pod loads artifacts.
	// Model both facts so successful delivery can supersede prior evidence.
	wc := f.argo.ArgoprojV1alpha1().Workflows(pod.Namespace)
	w, err := wc.Get(t.Context(), name, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if w.Status.Nodes == nil {
		w.Status.Nodes = wf.Nodes{}
	}
	if _, ok := w.Status.Nodes[name]; !ok {
		w.Status.Nodes[name] = wf.NodeStatus{ID: name, Name: nodeName, TemplateName: r.Admission.Plan.Entries[0].Name, Type: wf.NodeTypePod, Phase: wf.NodeRunning, StartedAt: meta.NewTime(f.clock.Now())}
		w.Status.Phase = wf.WorkflowRunning
		if _, err = wc.Update(t.Context(), w, meta.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return pod
}
func (f *composedFixture) deliverSource(t *testing.T, r stateport.Run) string {
	t.Helper()
	return f.deliverSourceThrough(t, r, nil)
}
func (f *composedFixture) deliverSourceThrough(t *testing.T, r stateport.Run, dial func(context.Context, string, string) (net.Conn, error)) string {
	t.Helper()
	return f.deliverLocated(t, r, r.ID, dial)
}

// deliverLocated loads r's source from its own Pod with the locator its
// Workflow spec stamps, and checks the unpacked tree is the admitted commit.
func (f *composedFixture) deliverLocated(t *testing.T, r stateport.Run, locator string, dial func(context.Context, string, string) (net.Conn, error)) string {
	t.Helper()
	pod := f.sourcePod(t, r)
	f.kube.PrependReactor("create", "tokenreviews", func(action ktest.Action) (bool, runtime.Object, error) {
		review := action.(ktest.CreateAction).GetObject().(*auth.TokenReview).DeepCopy()
		review.Status.Authenticated = review.Spec.Token == "bound-token"
		review.Status.User = auth.UserInfo{Username: "system:serviceaccount:" + pod.Namespace + ":" + pod.Spec.ServiceAccountName, Extra: map[string]auth.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}
		return true, review, nil
	})
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := client.WritePrivate(token, []byte("bound-token")); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "source")
	f.loadSourcePlugin(t, pluginRequest{Locator: locator, TokenFile: token, Destination: destination, Dial: dial})
	unpacked := t.TempDir()
	extract := exec.CommandContext(t.Context(), "tar", "-xzf", destination, "-C", unpacked)
	if output, err := extract.CombinedOutput(); err != nil {
		t.Fatalf("unpack native source artifact: %v: %s", err, output)
	}
	command := exec.CommandContext(t.Context(), f.git.Binary, "-C", unpacked, "rev-parse", "HEAD")
	output, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != *r.CommitSHA {
		t.Fatal("delivered source is not the admitted commit", string(output), err)
	}
	data, err := os.ReadFile(filepath.Join(unpacked, "file.txt"))
	if err != nil || string(data) != "exact source\n" {
		t.Fatal("real source transfer changed content", err)
	}
	return unpacked
}
func (f *composedFixture) finishNative(t *testing.T, r stateport.Run) { f.endNative(t, r, 0) }

// endNative ends the run's only step with the exit code; nonzero fails the run.
func (f *composedFixture) endNative(t *testing.T, r stateport.Run, exit int32) {
	t.Helper()
	phase, node, podPhase := wf.WorkflowSucceeded, wf.NodeSucceeded, core.PodSucceeded
	if exit != 0 {
		phase, node, podPhase = wf.WorkflowFailed, wf.NodeFailed, core.PodFailed
	}
	client := f.argo.ArgoprojV1alpha1().Workflows(f.config.PipelineNamespace)
	w, err := client.Get(t.Context(), r.Origin.WorkflowName, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nodeName := w.Name
	if path := r.Admission.Plan.Entries[0].Path; path != "" {
		nodeName += "." + path
	}
	w.Status = wf.WorkflowStatus{Phase: phase, StartedAt: meta.NewTime(f.clock.Now()), FinishedAt: meta.NewTime(f.clock.Now()), Nodes: wf.Nodes{w.Name: wf.NodeStatus{ID: w.Name, Name: nodeName, DisplayName: "build", TemplateName: "build", Type: wf.NodeTypePod, Phase: node, StartedAt: meta.NewTime(f.clock.Now()), FinishedAt: meta.NewTime(f.clock.Now())}}}
	if _, err = client.Update(t.Context(), w, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod, err := f.kube.CoreV1().Pods(f.config.PipelineNamespace).Get(t.Context(), w.Name, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = podPhase
	pod.Status.ContainerStatuses[0].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: exit, StartedAt: meta.NewTime(f.clock.Now()), FinishedAt: meta.NewTime(f.clock.Now())}}
	if _, err = f.kube.CoreV1().Pods(pod.Namespace).UpdateStatus(t.Context(), pod, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (f *composedFixture) checkStarting(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-f.listed:
	case err := <-done:
		t.Fatalf("server before watch: %v; %s", err, f.trace.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("native watch not reached: %s; %v", f.trace.String(), f.trace.snapshot())
	}
	response, err := f.client(f.config.Listen.HTTPS).Get("https://oberth.local/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("ready before native watches synchronized")
	}
	if !strings.Contains(f.owners.c.health.metrics(t.Context()), "oberth_watch_synced 0\n") {
		t.Fatal("unsynchronized native watch metric is not zero")
	}
	close(f.allowList)
}
func (f *composedFixture) checkOnce(t *testing.T) {
	t.Helper()
	creates := 0
	for _, action := range f.argo.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "workflows" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatal("push created native execution more than once", creates)
	}
	rows, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatal("one push did not record exactly one run", len(rows), err)
	}
}
func (f *composedFixture) checkPartFailure(t *testing.T, done <-chan error) {
	t.Helper()
	if err := f.ssh.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "ssh") {
			t.Fatal("ended SSH part did not take down server with its error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("failed SSH part left server running")
	}
}

func (f *composedFixture) checkRecovery(t *testing.T) {
	t.Helper()
	if !strings.Contains(f.owners.c.health.metrics(t.Context()), "oberth_watch_synced 1\n") {
		t.Fatal("synchronized native watch metric is not one")
	}
	events := f.trace.snapshot()
	wiring, replay := slices.Index(events, "recover wiring"), slices.Index(events, "replay pending")
	if wiring < 0 || replay < wiring {
		t.Fatal("real wiring did not recover before pending pushes", events)
	}
}
func (f *composedFixture) checkInspectionNoWrites(t *testing.T) {
	t.Helper()
	keys, err := f.owners.c.store.Keys(t.Context(), store.KeyFilter{Identity: "agent", Page: store.Page{Limit: 10}})
	if err != nil || len(keys) != 1 {
		t.Fatal("missing scoped push identity", err)
	}
	who, err := (currentAuthority{f.owners}).byFingerprint(t.Context(), keys[0].Fingerprint, "ssh")
	if err != nil {
		t.Fatal(err)
	}
	actions := len(f.kube.Actions())
	call, err := f.owners.service.PrepareRunCheck(t.Context(), apicontract.Caller{Actor: who}, apicontract.RunCheckArgs{Repo: "origin/team/widget", Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = call(t.Context())
	for _, action := range f.kube.Actions()[actions:] {
		if action.GetVerb() != "get" && action.GetVerb() != "list" && action.GetVerb() != "watch" {
			t.Fatalf("inspection wrote native state: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

func (f *composedFixture) checkReleasePreflight(t *testing.T, commit string) {
	t.Helper()
	keys, err := f.owners.c.store.Keys(t.Context(), store.KeyFilter{Identity: "agent", Page: store.Page{Limit: 10}})
	if err != nil || len(keys) != 1 {
		t.Fatal(err)
	}
	who, err := (currentAuthority{f.owners}).byFingerprint(t.Context(), keys[0].Fingerprint, "ssh")
	if err != nil {
		t.Fatal(err)
	}
	call, err := f.owners.service.PrepareRunCheck(t.Context(), apicontract.Caller{Actor: who}, apicontract.RunCheckArgs{Repo: "origin/team/widget", Ref: commit, ReleaseTag: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := call(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	assertReleasePreflight(t, result, commit)

	if _, err = f.owners.cache.ResolveRef(t.Context(), "origin/team/widget", "refs/tags/v1.0.0"); err == nil {
		t.Fatal("preflight created a tag")
	}
}

func assertReleasePreflight(t *testing.T, result apicontract.RunCheckResult, commit string) {
	t.Helper()
	if !result.OK || result.ReleaseTag != "v1.0.0" || result.CommitSHA == nil || *result.CommitSHA != commit || result.SelectedFile == nil || *result.SelectedFile != ".oberth/release.yaml" {
		t.Fatalf("release preflight lost exact commit/route: %+v", result)
	}
}
