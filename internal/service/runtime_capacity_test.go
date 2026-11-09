package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type capacityAudit struct {
	mu      sync.Mutex
	actions []model.AuditActionSpec
	fail    error
	entered chan struct{}
}

func (a *capacityAudit) AppendAuditAction(ctx context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	if a.entered != nil {
		close(a.entered)
		<-ctx.Done()
		return model.AuditAction{}, ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return model.AuditAction{}, a.fail
	}
	a.actions = append(a.actions, spec)
	return model.AuditAction{}, nil
}
func runtimeCapacityFixture(t *testing.T, jobs int) (*Scheduler, *capacityAudit) {
	t.Helper()
	gate := newAdmissionGate(jobs)
	path := filepath.Join(t.TempDir(), "max-concurrent-jobs")
	gate.capacityConfig = model.AdmissionCapacityConfig{MaxConcurrentJobs: jobs, BootDefault: jobs, Source: "boot", File: path}
	auditor := &capacityAudit{}
	return &Scheduler{admission: gate, maxConcurrent: jobs, capacityFile: path, auditor: auditor, mutationGate: func(context.Context) error { return nil }, wake: make(chan struct{}, 1)}, auditor
}
func writeCapacity(t *testing.T, s *Scheduler, text string) {
	t.Helper()
	if err := os.WriteFile(s.capacityFile, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func capacityWaiting(t *testing.T, g *admissionGate, seq int64, _ pipeline.Size) (*admissionReservation, <-chan func()) {
	t.Helper()
	r := g.reserve(seq)
	ch := make(chan func(), 1)
	go acquireRequestForTest(r, admissionRequest{RunID: "waiting"}, ch)
	t.Cleanup(r.cancel)
	awaitWaiting(t, r)
	return r, ch
}
func capacityGranted(t *testing.T, r *admissionReservation) bool {
	t.Helper()
	r.gate.mu.Lock()
	defer r.gate.mu.Unlock()
	return r.granted
}

func TestRuntimeCapacityRaiseAndLowerPreserveGrants(t *testing.T) {
	s, a := runtimeCapacityFixture(t, 2)
	g := s.admission
	var releases []func()
	for seq := int64(1); seq <= 2; seq++ {
		release, err := acquireWithinBudget(t, g.reserve(seq), pipeline.M)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
		defer release()
	}
	waiting, ready := capacityWaiting(t, g, 3, pipeline.M)
	writeCapacity(t, s, "3\n")
	s.refreshRuntimeCapacity(t.Context())
	var third func()
	select {
	case third = <-ready:
		defer third()
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("raise did not wake prepared waiter")
	}
	if !capacityGranted(t, waiting) || s.AdmissionState().Used != 3 || !s.dispatchSlotAvailable(2) {
		t.Fatal("raise left a stale gate or dispatch ceiling", s.AdmissionState())
	}
	writeCapacity(t, s, "1")
	s.refreshRuntimeCapacity(t.Context())
	state := s.AdmissionState()
	if state.Used != 3 || state.Capacity != 1 || state.Runs != 3 || state.Configuration.MaxConcurrentJobs != 1 || state.Configuration.Source != "file" || s.dispatchSlotAvailable(3) {
		t.Fatal("lower changed held grants", state)
	}
	next, nextReady := capacityWaiting(t, g, 4, pipeline.M)
	releases[0]()
	if capacityGranted(t, next) {
		t.Fatal("over-budget grant after lower")
	}
	releases[1]()
	if capacityGranted(t, next) {
		t.Fatal("still-full grant after lower")
	}
	third()
	select {
	case release := <-nextReady:
		release()
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("queue failed to resume after usage drained")
	}
	s.refreshRuntimeCapacity(t.Context())
	if len(a.actions) != 2 || a.actions[0].Action != "admission.capacity.change" || !strings.Contains(a.actions[1].Details, `"jobs":1`) {
		t.Fatal("changes not audited exactly once", a.actions)
	}
}

func TestRuntimeCapacityInvalidMissingAndAuditFailureKeepLastGood(t *testing.T) {
	s, a := runtimeCapacityFixture(t, 2)
	s.refreshRuntimeCapacity(t.Context())
	if state := s.AdmissionState(); state.Configuration.Source != "boot" || state.Configuration.Error == "" || state.Capacity != 2 {
		t.Fatal(state)
	}
	writeCapacity(t, s, "4")
	s.refreshRuntimeCapacity(t.Context())
	for _, text := range []string{"", "65", "-1", "+3", "2.5", "secret-content", strings.Repeat("2", 65)} {
		writeCapacity(t, s, text)
		s.refreshRuntimeCapacity(t.Context())
		state := s.AdmissionState()
		if state.Capacity != 4 || state.Configuration.MaxConcurrentJobs != 4 || state.Configuration.Error == "" || strings.Contains(state.Configuration.Error, "secret-content") {
			t.Fatal("invalid override changed or leaked", state)
		}
	}
	if err := os.Remove(s.capacityFile); err != nil {
		t.Fatal(err)
	}
	s.refreshRuntimeCapacity(t.Context())
	if s.AdmissionState().Capacity != 4 {
		t.Fatal("missing file restored boot instead of last good")
	}
	if err := os.Mkdir(s.capacityFile, 0700); err != nil {
		t.Fatal(err)
	}
	s.refreshRuntimeCapacity(t.Context())
	if s.AdmissionState().Capacity != 4 {
		t.Fatal("directory changed last good")
	}
	if err := os.Remove(s.capacityFile); err != nil {
		t.Fatal(err)
	}
	a.fail = errors.New("writer unavailable")
	writeCapacity(t, s, "3")
	s.refreshRuntimeCapacity(t.Context())
	if s.AdmissionState().Capacity != 4 || !strings.Contains(s.AdmissionState().Configuration.Error, "audited") {
		t.Fatal("unaudited change accepted")
	}
	a.fail = nil
	s.refreshRuntimeCapacity(t.Context())
	if s.AdmissionState().Capacity != 3 || s.AdmissionState().Configuration.Error != "" || len(a.actions) != 2 {
		t.Fatal("valid recovery did not apply", s.AdmissionState(), a.actions)
	}
}

func TestRuntimeCapacityAuditDoesNotBlockAdmissionOrStatus(t *testing.T) {
	s, a := runtimeCapacityFixture(t, 2)
	a.entered = make(chan struct{})
	writeCapacity(t, s, "3")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.refreshRuntimeCapacity(ctx) }()
	select {
	case <-a.entered:
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("audit did not start")
	}
	release, err := acquireWithinBudget(t, s.admission.reserve(1), pipeline.M)
	if err != nil {
		t.Fatal(err)
	}
	release()
	stateDone := make(chan *model.AdmissionState, 1)
	go func() { stateDone <- s.AdmissionState() }()
	select {
	case state := <-stateDone:
		if state.Capacity != 2 {
			t.Fatal(state)
		}
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("audit blocked status")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("reload ignored cancellation")
	}
	if s.AdmissionState().Capacity != 2 {
		t.Fatal("failed audit changed capacity")
	}
}

func TestRuntimeCapacityWatchReopensConfigMapSymlink(t *testing.T) {
	s, _ := runtimeCapacityFixture(t, 1)
	dir := filepath.Dir(s.capacityFile)
	for name, value := range map[string]string{"rev-a": "2", "rev-b": "4"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "max-concurrent-jobs"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("rev-a", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/max-concurrent-jobs", s.capacityFile); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); s.watchRuntimeCapacity(ctx, ticks) }()
	defer func() { cancel(); <-done }()
	tickAndAwait := func(want int) {
		t.Helper()
		select {
		case ticks <- time.Now():
		case <-time.After(testStartTimeout(time.Second)):
			t.Fatal("watch not polling")
		}
		deadline := time.Now().Add(testStartTimeout(time.Second))
		for s.AdmissionState().Configuration.MaxConcurrentJobs != want {
			if time.Now().After(deadline) {
				t.Fatal("key not reopened", s.AdmissionState())
			}
			time.Sleep(time.Millisecond)
		}
	}
	tickAndAwait(2)
	if err := os.Symlink("rev-b", filepath.Join(dir, "..data-new")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "..data-new"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	tickAndAwait(4)
}

func TestRuntimeCapacityRunUsesOverrideBeyondBootDispatchLimit(t *testing.T) {
	f := newConcurrencyFixture(t, pipeline.M, 1)
	s := f.scheduler
	s.capacityFile = filepath.Join(t.TempDir(), "capacity")
	s.admission.capacityConfig.File = s.capacityFile
	writeCapacity(t, s, "2")
	for i := range 3 {
		f.enqueue(t, i)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		close(f.jobs.release)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(testStartTimeout(15 * time.Second)):
			t.Error("scheduler did not stop")
		}
	}()
	for range 2 {
		select {
		case <-f.jobs.admitted:
		case <-time.After(testStartTimeout(10 * time.Second)):
			t.Fatal("boot dispatch ceiling ignored override")
		}
	}
	writeCapacity(t, s, "3")
	s.refreshRuntimeCapacity(t.Context())
	select {
	case <-f.jobs.admitted:
	case <-time.After(testStartTimeout(10 * time.Second)):
		t.Fatal("live raise did not wake scheduler")
	}
	if b := s.AdmissionState(); b.Runs != 3 {
		t.Fatalf("run ceiling changed numeric resources: %+v", b)
	}
}

func TestRuntimeCapacityDispatchUsesEffectiveLimitWhileClaimsPrepare(t *testing.T) {
	s, _ := runtimeCapacityFixture(t, 1)
	first, second := s.admission.reserve(1), s.admission.reserve(2)
	defer first.cancel()
	defer second.cancel()
	writeCapacity(t, s, "3")
	s.refreshRuntimeCapacity(t.Context())
	if !s.dispatchSlotAvailable(2) {
		t.Fatal("live raise retained boot dispatch ceiling while claims prepare")
	}
	writeCapacity(t, s, "1")
	s.refreshRuntimeCapacity(t.Context())
	if s.dispatchSlotAvailable(2) {
		t.Fatal("lower did not restrict future dispatch")
	}
}

// The server-owned override (#1497) wins over the mounted file, survives its
// reloads, and clearing it hands authority back to the file. A failed
// override read keeps the last good value instead of falling back.
func TestRuntimeCapacityOverrideTakesPrecedenceOverFile(t *testing.T) {
	s, auditor := runtimeCapacityFixture(t, 2)
	if err := os.WriteFile(s.capacityFile, []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var override *model.AdmissionCapacityOverride
	var readErr error
	s.SetCapacityOverrideReader(func(context.Context) (*model.AdmissionCapacityOverride, error) { return override, readErr })
	s.refreshRuntimeCapacity(ctx)
	if s.concurrentJobs() != 3 {
		t.Fatalf("file jobs = %d", s.concurrentJobs())
	}
	override = &model.AdmissionCapacityOverride{MaxConcurrentJobs: 1}
	if got := s.ApplyRuntimeCapacity(ctx); got.MaxConcurrentJobs != 1 || got.Source != "runtime" || s.concurrentJobs() != 1 {
		t.Fatalf("runtime override = %+v", got)
	}
	s.refreshRuntimeCapacity(ctx)
	if s.concurrentJobs() != 1 {
		t.Fatal("file reload replaced the runtime override")
	}
	readErr = errors.New("database is locked")
	if got := s.ApplyRuntimeCapacity(ctx); got.MaxConcurrentJobs != 1 || got.Error == "" || strings.Contains(got.Error, "locked") {
		t.Fatalf("override read failure must keep last good with a public error: %+v", got)
	}
	readErr, override = nil, nil
	if got := s.ApplyRuntimeCapacity(ctx); got.MaxConcurrentJobs != 3 || got.Source != "file" || got.Error != "" {
		t.Fatalf("cleared override = %+v", got)
	}
	if err := s.ValidateRuntimeCapacity(65); err == nil {
		t.Fatal("65 jobs validated")
	}
	sources := []string{}
	for _, action := range auditor.actions {
		sources = append(sources, action.Details)
	}
	if len(auditor.actions) != 3 || !strings.Contains(sources[1], `"source":"runtime"`) || !strings.Contains(sources[2], `"previous_source":"runtime"`) {
		t.Fatalf("capacity audit = %v", sources)
	}
}
