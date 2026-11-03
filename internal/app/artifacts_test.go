package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type fakeCollector struct {
	body  []byte
	err   error
	calls int
}

func (f *fakeCollector) Collect(context.Context, string) ([]byte, error) {
	f.calls++
	return f.body, f.err
}

type fakeArtifactStore struct {
	stored []byte
	err    error
	calls  int
}

func (f *fakeArtifactStore) Extract(_ string, stream io.Reader, _ int64) error {
	f.calls++
	body, readErr := io.ReadAll(stream)
	if readErr != nil {
		return readErr
	}
	f.stored = body
	return f.err
}

func jobsWithArtifacts(collector ArtifactCollector, store ArtifactStore) *ArgoJobs {
	return &ArgoJobs{collector: collector, artifacts: store, artifactLimit: 1 << 20}
}

func TestCollectArtifactsStoresWhatTheRunProduced(t *testing.T) {
	t.Parallel()
	collector := &fakeCollector{body: []byte("archive")}
	store := &fakeArtifactStore{}
	jobs := jobsWithArtifacts(collector, store)

	if reason := jobs.collectArtifacts(context.Background(), "wf", "run-1", pipeline.TriggerCI); reason != "" {
		t.Fatalf("unexpected failure reason %q", reason)
	}
	if string(store.stored) != "archive" {
		t.Fatalf("stored %q", store.stored)
	}
}

func TestCollectArtifactsReportsRatherThanFailingTheRun(t *testing.T) {
	t.Parallel()
	jobs := jobsWithArtifacts(&fakeCollector{err: errors.New("claim already gone")}, &fakeArtifactStore{})

	reason := jobs.collectArtifacts(context.Background(), "wf", "run-1", pipeline.TriggerCI)
	if reason == "" {
		t.Fatal("a failed collection reported nothing, so the loss would be silent")
	}
	var recorded bool
	jobs.auditor = &stubAuditor{onAppend: func(spec model.AuditActionSpec) {
		var details struct {
			Stored bool   `json:"stored"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(spec.Details), &details); err != nil {
			t.Fatal(err)
		}
		if spec.Action != "ci.argo.artifacts" || spec.ResourceID != "run-1" || details.Stored || details.Reason != reason {
			t.Fatalf("failure audit = %+v: %+v", spec, details)
		}
		recorded = true
	}}
	jobs.recordArtifactFailure(context.Background(), "run-1", reason)
	if !recorded {
		t.Fatal("artifact failure lost its durable audit evidence")
	}
}

func TestCollectArtifactsReportsAStoreRefusal(t *testing.T) {
	t.Parallel()
	jobs := jobsWithArtifacts(&fakeCollector{body: []byte("archive")},
		&fakeArtifactStore{err: errors.New("member escapes the collection")})

	if reason := jobs.collectArtifacts(context.Background(), "wf", "run-1", pipeline.TriggerCI); reason == "" {
		t.Fatal("a refused archive reported nothing")
	}
}

func TestCollectArtifactsSkipsAnEmptyCollection(t *testing.T) {
	t.Parallel()
	store := &fakeArtifactStore{}
	jobs := jobsWithArtifacts(&fakeCollector{body: nil}, store)

	if reason := jobs.collectArtifacts(context.Background(), "wf", "run-1", pipeline.TriggerCI); reason != "" {
		t.Fatalf("an empty collection reported %q", reason)
	}
	if store.calls != 0 {
		t.Fatal("an empty collection still reached the store")
	}
}

func TestCollectArtifactsIsANoOpWhenNotConfigured(t *testing.T) {
	t.Parallel()
	jobs := &ArgoJobs{}
	if reason := jobs.collectArtifacts(context.Background(), "wf", "run-1", pipeline.TriggerCI); reason != "" {
		t.Fatalf("an unconfigured deployment reported %q", reason)
	}
}

func TestCollectArtifactsSkipsCredentialedTiers(t *testing.T) {
	t.Parallel()
	for _, trigger := range []pipeline.Trigger{pipeline.TriggerRelease, pipeline.Trigger("plan"), pipeline.Trigger("apply")} {
		collector := &fakeCollector{body: []byte("archive")}
		store := &fakeArtifactStore{}
		jobs := jobsWithArtifacts(collector, store)

		if reason := jobs.collectArtifacts(context.Background(), "wf", "run-1", trigger); reason != "" {
			t.Fatalf("tier-gated skip for %q reported a failure: %s", trigger, reason)
		}
		if collector.calls != 0 {
			t.Fatalf("collection ran for credentialed trigger %q; the tier gate (#208) must stop it before the pod exec", trigger)
		}
		if store.calls != 0 {
			t.Fatalf("artifacts were persisted for credentialed trigger %q", trigger)
		}
	}
}

func TestCollectArtifactsRunsForTheCITrigger(t *testing.T) {
	t.Parallel()
	collector := &fakeCollector{body: []byte("archive")}
	store := &fakeArtifactStore{}
	jobs := jobsWithArtifacts(collector, store)

	if reason := jobs.collectArtifacts(context.Background(), "wf", "run-1", pipeline.TriggerCI); reason != "" {
		t.Fatalf("CI-tier collection failed: %s", reason)
	}
	if collector.calls != 1 || store.calls != 1 {
		t.Fatalf("CI-tier collection did not run (collector=%d store=%d)", collector.calls, store.calls)
	}
}
