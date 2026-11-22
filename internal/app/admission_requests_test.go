package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/gitport"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktest "k8s.io/client-go/testing"
)

// Library default client rate: 5 requests a second after a burst of 10.
// Production's 30-second admission deadline was shared by six large pushes
// that arrived together, five seconds each: 10 + 5*5 requests per push.
const defaultRateShare = 10 + 5*5

// clusterBudget counts every request admission makes on the fake cluster and
// refuses, as the client's rate limiter does, those beyond its share.
type clusterBudget struct {
	mu    sync.Mutex
	left  int
	verbs map[string]int
}

func (b *clusterBudget) react(a ktest.Action) (bool, runtime.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verbs[a.GetVerb()]++
	if b.left == 0 {
		return true, nil, fmt.Errorf("client rate limiter Wait returned an error: %w", errors.New("rate: Wait(n=1) would exceed context deadline"))
	}
	b.left--
	return false, nil, nil
}
func (b *clusterBudget) take() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.verbs
	b.verbs = map[string]int{}
	return out
}
func total(verbs map[string]int) int {
	n := 0
	for _, count := range verbs {
		n += count
	}
	return n
}

type admissionCluster struct {
	kube      *kubefake.Clientset
	budget    *clusterBudget
	source    *fixture
	submitter *app.Submitter
	accounts  *slowAccounts
}

// slowAccounts holds reads outside the fake cluster's own lock, as a slow
// API server holds one caller without stopping the others.
type slowAccounts struct {
	argojob.Accounts
	hold func(name string)
}

func (s *slowAccounts) Get(ctx context.Context, name string, o meta.GetOptions) (*corev1.ServiceAccount, error) {
	if s.hold != nil {
		s.hold(name)
	}
	return s.Accounts.Get(ctx, name, o)
}

// newAdmissionCluster wires the real authority, account ensure and native
// preparation to one fake cluster, as the server composes them.
func newAdmissionCluster(t *testing.T, budget int) *admissionCluster {
	t.Helper()
	f := newFixture(t)
	c := &admissionCluster{kube: kubefake.NewClientset(), budget: &clusterBudget{left: budget, verbs: map[string]int{}}, source: f}
	c.kube.PrependReactor("create", "serviceaccounts", func(a ktest.Action) (bool, runtime.Object, error) {
		m := a.(ktest.CreateAction).GetObject().(meta.Object)
		m.SetUID(types.UID("uid-" + m.GetName()))
		return false, nil, nil
	})
	c.kube.PrependReactor("*", "*", c.budget.react)
	c.accounts = &slowAccounts{Accounts: c.kube.CoreV1().ServiceAccounts("runs")}
	executor := argojob.New(argojob.Config{Accounts: c.accounts, Runs: f.runs.Store, Progress: f.runs.Store, Workflows: &nativefake.Workflows{}, Now: landedClock})
	authority, err := app.NewAuthority(app.AuthorityConfig{Namespace: "runs", ServerNamespace: "oberth", ArgoNamespace: "argo", ImagePrefixes: []string{"oberth.ci/"}, Grants: f.runs.Grants, EnsureAccount: executor.EnsureAccount,
		Templates: func(context.Context, string) (*wf.WorkflowTemplate, error) { return nil, errors.New("unexpected template read") }})
	landedOK(t, err)
	f.authority.LoadFunc = authority.Load
	deps := f.d
	deps.Authority, deps.Native = f.authority, executor
	c.submitter, err = app.New(deps)
	landedOK(t, err)
	t.Cleanup(func() { landedOK(t, c.submitter.Close(context.Background())) })
	return c
}

// wideWorkflow runs n container steps one after another.
func wideWorkflow(n int) *wf.Workflow {
	w := workflow()
	w.Spec.Entrypoint = "main"
	main := wf.Template{Name: "main"}
	w.Spec.Templates = nil
	for i := range n {
		name := fmt.Sprintf("step-%d", i)
		main.Steps = append(main.Steps, wf.ParallelSteps{Steps: []wf.WorkflowStep{{Name: name, Template: name}}})
		w.Spec.Templates = append(w.Spec.Templates, wf.Template{Name: name, Container: &corev1.Container{Image: "oberth.ci/build:1", Command: []string{"true"}}})
	}
	w.Spec.Templates = append(w.Spec.Templates, main)
	return w
}

func (c *admissionCluster) push(ctx context.Context, repository, id string, steps int) error {
	source, raw := c.source.source, document(wideWorkflow(steps))
	source.Read = func(_ context.Context, _ gitport.Revision, path string) (gitport.File, error) {
		return gitport.File{Path: path, Blob: strings.Repeat("c", 40), Mode: 0100644, Bytes: raw}, nil
	}
	r := receive()
	r.ID, r.Source = id, source
	r.Updates[0].Request.Repository, r.Updates[0].Request.RunID = repository, "run-"+id
	_, err := c.submitter.Gate(ctx, r)
	return err
}

// The requests one admission makes do not grow with the number of steps.
func TestAdmissionRequestsDoNotGrowWithSteps(t *testing.T) {
	t.Parallel()
	counts := map[int][2]map[string]int{}
	for _, steps := range []int{1, 10, 40} {
		c := newAdmissionCluster(t, 1000)
		landedOK(t, c.push(t.Context(), request().Repository, "first", steps))
		first := c.budget.take()
		landedOK(t, c.push(t.Context(), request().Repository, "second", steps))
		counts[steps] = [2]map[string]int{first, c.budget.take()}
		t.Logf("steps=%d first push %v (%d requests), second push %v (%d requests)", steps, first, total(first), counts[steps][1], total(counts[steps][1]))
	}
	if total(counts[40][0]) != total(counts[1][0]) || total(counts[40][1]) != total(counts[1][1]) {
		t.Fatal("admission requests grow with steps", counts)
	}
}

// A 40-step pipeline admits within the share a library-default client serves.
func TestWidePipelineAdmitsAtDefaultClientRate(t *testing.T) {
	t.Parallel()
	c := newAdmissionCluster(t, defaultRateShare)
	if err := c.push(t.Context(), request().Repository, "first", 40); err != nil {
		t.Fatal("40-step push refused at the default client rate", total(c.budget.take()), err)
	}
}

// Accounts persist across pushes: a second push of the same pipeline creates none.
func TestSecondPushCreatesNoAccounts(t *testing.T) {
	t.Parallel()
	c := newAdmissionCluster(t, 1000)
	landedOK(t, c.push(t.Context(), request().Repository, "first", 40))
	if created := c.budget.take()["create"]; created != 1 {
		t.Fatal("first push should create the pipeline account once", created)
	}
	landedOK(t, c.push(t.Context(), request().Repository, "second", 40))
	if created := c.budget.take()["create"]; created != 0 {
		t.Fatal("second push re-created accounts", created)
	}
}

// A push refused for want of cluster time says to push again; only an account
// the cluster says is absent or may not be created keeps the install remedy.
func TestAccountRefusalNamesItsCause(t *testing.T) {
	t.Parallel()
	timeout, install := "Admission ran out of time", "Install the admitted step account"
	forbidden := func(ktest.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "x", errors.New("no create"))
	}
	for _, tc := range []struct {
		name, want, not string
		budget         int
		create         ktest.ReactionFunc
	}{
		{name: "ensure rate limited", budget: 1, want: timeout, not: install},
		{name: "verify rate limited", budget: 2, want: timeout, not: install},
		{name: "create forbidden", budget: 1000, create: forbidden, want: install, not: timeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newAdmissionCluster(t, tc.budget)
			if tc.create != nil {
				c.kube.PrependReactor("create", "serviceaccounts", tc.create)
			}
			err := c.push(t.Context(), request().Repository, "first", 40)
			t.Log(err)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.not) {
				t.Fatalf("refusal does not name its cause:\n%v", err)
			}
		})
	}
}

// One repository's slow admission never refuses another repository's push.
func TestSlowAdmissionDoesNotRefuseAnotherRepository(t *testing.T) {
	t.Parallel()
	c := newAdmissionCluster(t, defaultRateShare)
	slow := "origin/team/gadget"
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.accounts.hold = func(name string) {
		if strings.Contains(name, "gadget") {
			once.Do(func() { close(entered) })
			<-release
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.push(context.Background(), slow, "slow", 40) }()
	<-entered
	err := c.push(t.Context(), request().Repository, "other", 40)
	close(release)
	if err != nil {
		t.Fatal("slow admission refused another repository's push", err)
	}
	if err := <-done; err != nil {
		t.Fatal("slow admission failed after release", err)
	}
}
