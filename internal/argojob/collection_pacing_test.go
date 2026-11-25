package argojob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"golang.org/x/time/rate"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/clock"
)

type pacingLogs struct {
	LogStore
	entries map[string][]runlog.ContainerLog
}

func (s *pacingLogs) Containers(_ context.Context, id string) ([]runlog.ContainerLog, error) {
	return s.entries[id], nil
}
func (s *pacingLogs) RecordGap(_ context.Context, id string, entry runlog.ContainerLog, _ runlog.Gap) error {
	entry.State = runlog.Captured
	s.entries[id] = append(s.entries[id], entry)
	return nil
}
func (*pacingLogs) Track(context.Context, string, []runlog.ContainerLog) error { return nil }
func (*pacingLogs) KeepFollowing(string, []runlog.ContainerLog)                {}
func (*pacingLogs) Archived(context.Context, string, []runlog.ContainerLog) (bool, error) {
	return false, nil
}

type pacingPods struct {
	Pods
	reads int
}

func (p *pacingPods) Get(_ context.Context, name string, _ meta.GetOptions) (*core.Pod, error) {
	p.reads++
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
}

type pacingCache struct{ Cache }

func (pacingCache) OnChange(func(string))   {}
func (pacingCache) Pods(string) []*core.Pod { return nil }

type pacingPresentPods struct {
	Pods
	object *core.Pod
	reads  int
}

func (p *pacingPresentPods) Get(context.Context, string, meta.GetOptions) (*core.Pod, error) {
	p.reads++
	return p.object.DeepCopy(), nil
}

func TestCollectionReusesPodLookupWithinAttempt(t *testing.T) {
	e, track, native, _ := reviewR2Executor(t, &reviewR2Reporter{})
	track.archiveOnly, track.completed, track.historical = true, true, true
	e.collectionReads = rate.NewLimiter(rate.Inf, 1)
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Namespace: "ci", UID: "pod-uid", Labels: map[string]string{"workflows.argoproj.io/workflow": "run"}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "n", "workflows.argoproj.io/node-name": "run.build"}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "run", UID: "uid", Controller: new(true)}}}, Status: core.PodStatus{ContainerStatuses: []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{}}}}}}
	pods := &pacingPresentPods{object: pod}
	e.config.Pods, e.config.Logs = pods, &pacingLogs{entries: map[string][]runlog.ContainerLog{}}
	err := e.observeArchive(t.Context(), track)
	if !errors.Is(err, errArchivePending) || pods.reads != 1 || native.reads != 1 {
		t.Fatal("archive attempt repeats a live source lookup", pods.reads, native.reads, err)
	}
}

func TestCollectionFirstPassSpreadsLiveBacklog(t *testing.T) {
	now := time.Unix(1000, 0)
	logs := &pacingLogs{entries: map[string][]runlog.ContainerLog{}}
	pods := &pacingPods{}
	e := New(Config{Now: func() time.Time { return now }, Logs: logs, Pods: pods, Cache: pacingCache{}})
	defer e.queue.ShutDown()
	// Logical-clock measurement of the full observed shape; wall-clock QPS is
	// checked separately so this test does not take thirteen minutes.
	e.collectionReads = rate.NewLimiter(rate.Inf, 1)
	objects := make([]*wf.Workflow, 134)
	tracks := make([]*tracking, len(objects))
	for i := range objects {
		w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: fmt.Sprint("run-", i), Namespace: "ci"}, Status: wf.WorkflowStatus{Nodes: wf.Nodes{}}}
		for j := range 24 + btoi(i < 5) {
			id := fmt.Sprintf("node-%03d", j)
			w.Status.Nodes[id] = wf.NodeStatus{ID: id, Name: w.Name + "." + id, Type: wf.NodeTypePod, Phase: wf.NodeFailed, HostNodeName: "gone"}
		}
		objects[i], tracks[i] = w, &tracking{archiveOnly: true, binding: nativebinding.RunBinding{RunID: w.Name}}
	}
	remaining, passes, attempts := len(objects), 0, 0
	for remaining > 0 && passes < 100 {
		started := 0
		for i, w := range objects {
			if w == nil || e.collectionDelay(tracks[i]) > 0 {
				continue
			}
			started++
			_, _, err := e.collectionSources(t.Context(), tracks[i].binding, w)
			if err == nil {
				objects[i] = nil
				remaining--
			} else if !errors.Is(err, errCollectionPending) {
				t.Fatal(err)
			}
		}
		if started > 8 {
			t.Fatal("first pass exceeded Workflow budget", started)
		}
		attempts += started
		passes++
		now = now.Add(collectionInterval)
	}
	assertPacingResult(t, pacingResult{remaining, pods.reads, attempts, passes})
	t.Logf("134 Workflows / 3221 absent nodes: %d GETs, %d attempts, %d bounded windows, last window at %s", pods.reads, attempts, passes, time.Duration(passes-1)*collectionInterval)
}

type pacingResult struct{ remaining, reads, attempts, passes int }

func assertPacingResult(t *testing.T, r pacingResult) {
	t.Helper()
	remaining, reads, attempts, passes := r.remaining, r.reads, r.attempts, r.passes
	if remaining != 0 || reads != 3221 || attempts != 407 || passes != 52 {
		t.Fatal("backfill repeated Pod reads or failed to finish its bounded passes", remaining, reads, attempts, passes)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestCollectionReadRateIsMeasured(t *testing.T) {
	wall := clock.RealClock{}
	e := New(Config{Now: wall.Now})
	defer e.queue.ShutDown()
	start := wall.Now()
	for range 8 {
		if err := e.collectionRead(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := wall.Since(start)
	if elapsed < 1300*time.Millisecond || e.collectionReads.Limit() != 5 || e.collectionReads.Burst() != 1 {
		t.Fatal("collection reads can saturate the shared API budget", elapsed, e.collectionReads.Limit(), e.collectionReads.Burst())
	}
	t.Log("8 collection reads at 5 QPS / burst 1:", elapsed)
}

func TestCollectionRetryBackoffSurvivesWatchEvents(t *testing.T) {
	now := time.Unix(1000, 0)
	var output bytes.Buffer
	e := New(Config{Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&output, nil))})
	defer e.queue.ShutDown()
	e = e.collector
	defer e.queue.ShutDown()
	track := &tracking{archiveOnly: true, binding: nativebinding.RunBinding{RunID: "run", Workflow: nativebinding.Object{Name: "workflow"}}}
	e.tracked["workflow"] = track
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour} {
		e.retryArchive(track, errors.New("step log source is not settled"))
		if got := track.archiveRetryAt.Sub(now); got != want {
			t.Fatal("archive retry did not back off", got, want)
		}
		for range 100 {
			e.process(t.Context(), "workflow")
		}
		if e.collectionCount != 0 {
			t.Fatal("watch events bypassed archive backoff")
		}
		now = now.Add(want)
	}
	if !bytes.Contains(output.Bytes(), []byte("run log archive deferred")) || !bytes.Contains(output.Bytes(), []byte("retry_after=1h0m0s")) {
		t.Fatal("archive delay has no visible reason", output.String())
	}
}

func TestCollectionCopyFailureDelaysPendingCapture(t *testing.T) {
	now := time.Unix(1000, 0)
	e := New(Config{Now: func() time.Time { return now }})
	defer e.queue.ShutDown()
	binding := nativebinding.RunBinding{RunID: "run", Workflow: nativebinding.Object{Name: "workflow"}}
	entry := runlog.ContainerLog{PodUID: "pod", Container: "main", Step: "build"}
	track := &tracking{archiveOnly: true, binding: binding, capturing: 1, captures: map[string]bool{}, pending: map[string]captureJob{"build": {binding: binding, entries: []runlog.ContainerLog{entry}}}}
	e.tracked["workflow"] = track
	e.captured(captureJob{binding: binding, origin: track}, []runlog.ContainerLog{entry})
	if _, ok := e.nextCapture(); ok || track.archiveRetryAt.Sub(now) != time.Minute {
		t.Fatal("failed archive copy immediately dispatched another capture")
	}
	now = now.Add(time.Minute)
	if _, ok := e.nextCapture(); !ok {
		t.Fatal("archive copy did not resume after its backoff")
	}
}
