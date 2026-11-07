package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func exclusiveRequest(runID string, repoID int64, classes ...string) admissionRequest {
	return admissionRequest{RunID: runID, RepoID: repoID, Repo: "repo", Ref: "refs/heads/" + runID, Exclusive: classes}
}

func exclusiveGate(jobs int, limits map[string]int) *weightedAdmission {
	gate := newWorkflowAdmissionForJobs(jobs)
	gate.exclusiveLimits = limits
	return gate
}

// #1499: one class is exclusive across repositories. The parked run holds no
// workflow slot and never blocks unrelated claims behind it.
func TestExclusiveClassSerializesAcrossRepositories(t *testing.T) {
	gate := exclusiveGate(6, map[string]int{"kvm": 1})
	releaseFirst := acquireRequestWithinBudget(t, gate.reserve(1), exclusiveRequest("first", 1, "kvm"))

	parked := gate.reserve(2)
	acquired := make(chan func(), 1)
	go acquireRequestForTest(parked, exclusiveRequest("other-repo", 2, "kvm"), acquired)
	awaitWaiting(t, parked)
	if granted(gate, parked) {
		t.Fatal("second repository ran beside the exclusive holder")
	}
	if _, runs := gateUsage(gate); runs != 1 {
		t.Fatalf("parked exclusive run holds a workflow slot: runs=%d", runs)
	}
	snapshot := snapshotFor(gate, 2)
	if snapshot == nil || !snapshot.ExclusiveBlocked || snapshot.ExclusiveClass != "kvm" || snapshot.ExclusiveLimit != 1 || snapshot.HeadOfQueue {
		t.Fatalf("snapshot does not name the exclusive class: %+v", snapshot)
	}
	if message := waitingAdmissionMessage(snapshot); !strings.Contains(message, `exclusive class "kvm"`) {
		t.Fatalf("waiting message %q", message)
	}
	if reason := waitingReason(snapshot); reason != "exclusive kvm busy" {
		t.Fatalf("queued reason %q", reason)
	}

	releaseUnrelated := acquireRequestWithinBudget(t, gate.reserve(3), exclusiveRequest("unrelated", 3))
	releaseOtherClass := acquireRequestWithinBudget(t, gate.reserve(4), exclusiveRequest("other-class", 3, "gpu"))
	releaseUnrelated()
	releaseOtherClass()

	releaseFirst()
	awaitAcquired(t, acquired)()
	gate.mu.Lock()
	classes := len(gate.exclusive)
	gate.mu.Unlock()
	if classes != 0 {
		t.Fatal("released gate retains exclusive holders")
	}
}

func TestExclusiveClassLimitAndFIFO(t *testing.T) {
	gate := exclusiveGate(6, map[string]int{"kvm": 2})
	releaseA := acquireRequestWithinBudget(t, gate.reserve(1), exclusiveRequest("a", 1, "kvm"))
	releaseB := acquireRequestWithinBudget(t, gate.reserve(2), exclusiveRequest("b", 2, "kvm"))

	older, newer := gate.reserve(3), gate.reserve(4)
	olderAcquired, newerAcquired := make(chan func(), 1), make(chan func(), 1)
	go acquireRequestForTest(newer, exclusiveRequest("newer", 3, "kvm"), newerAcquired)
	awaitWaiting(t, newer)
	go acquireRequestForTest(older, exclusiveRequest("older", 4, "kvm"), olderAcquired)
	awaitWaiting(t, older)

	releaseA()
	releaseOlder := awaitAcquired(t, olderAcquired)
	awaitWaiting(t, newer)
	releaseB()
	releaseNewer := awaitAcquired(t, newerAcquired)
	releaseOlder()
	releaseNewer()
}

// A restart adopts the live holder even when the class is no longer listed;
// new members wait instead of starting beside it.
func TestExclusiveClassAdoptedHolderBlocksNewMembers(t *testing.T) {
	gate := exclusiveGate(6, nil)
	run := model.Run{ID: "adopted", RepoID: 1, QueueSequence: 1, AdmissionSequence: 1}
	holder, err := gate.adopt(run, "")
	if err != nil {
		t.Fatal(err)
	}
	gate.adoptExclusive(holder, []string{"kvm"})
	waiter := gate.reserve(2)
	acquired := make(chan func(), 1)
	go acquireRequestForTest(waiter, exclusiveRequest("new", 2, "kvm"), acquired)
	awaitWaiting(t, waiter)
	holder.release()
	awaitAcquired(t, acquired)()
}

type exclusiveEngine struct {
	*groupEngine
	classes []string
	err     error
}

func (engine exclusiveEngine) PipelineExclusiveClasses(context.Context, WorkflowRequest) ([]string, error) {
	return engine.classes, engine.err
}

// A class outside the administrator allowlist is an admission refusal, never
// an auto-retried infrastructure failure; an empty allowlist refuses all.
func TestExclusiveClassAllowlistRefusesUnknownClasses(t *testing.T) {
	f := newGroupFixture(t, 4)
	request := WorkflowRequest{Run: model.Run{ID: "run"}}
	f.scheduler.jobs = exclusiveEngine{groupEngine: f.engine, classes: []string{"kvm"}}
	_, err := f.scheduler.pipelineExclusiveClasses(t.Context(), request, nil)
	var admission *argoworkflow.AdmissionError
	if !errors.As(err, &admission) || !strings.Contains(err.Error(), `exclusive class "kvm" is not on this server's allowlist`) {
		t.Fatalf("feature-off declaration not refused at admission: %v", err)
	}
	f.scheduler.admission.exclusiveLimits = map[string]int{"kvm": 1}
	classes, err := f.scheduler.pipelineExclusiveClasses(t.Context(), request, nil)
	if err != nil || len(classes) != 1 || classes[0] != "kvm" {
		t.Fatalf("allowlisted class refused: %v %v", classes, err)
	}
	f.scheduler.jobs = exclusiveEngine{groupEngine: f.engine, classes: []string{"gpu"}}
	if _, err := f.scheduler.pipelineExclusiveClasses(t.Context(), request, nil); !errors.As(err, &admission) {
		t.Fatalf("unlisted class admitted: %v", err)
	}
}

func snapshotFor(gate *weightedAdmission, sequence int64) *model.AdmissionObservation {
	return (&Scheduler{admission: gate}).AdmissionSnapshot(sequence)
}

// #1499: admission reads classes from the fragment-resolved receipt, so a
// class declared only inside a fragment meets the allowlist and the limit even
// when the repository's own document declares none.
func TestExclusiveClassFromFragmentIsAdmittedFromTheReceipt(t *testing.T) {
	const image = "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	key := argoworkflow.FragmentKey{Repo: "acme/vm-test", Version: "v1"}
	fragment := argoworkflow.Fragment{Key: key, SHA: strings.Repeat("1", 40), Source: []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: run
  templates:
    - name: run
      metadata:
        annotations:
          oberth.ci/exclusive: kvm
      container:
        image: ` + image + `
        command: [/bin/true]
`)}
	source := []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      steps:
        - - name: verify
            templateRef:
              name: ` + key.String() + `
              template: run
`)
	resolved, err := argoworkflow.ResolveRunSource(source, map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment}, argoworkflow.RunSourceBinding{})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := resolved.Receipt(argoworkflow.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	f := newGroupFixture(t, 4)
	request := WorkflowRequest{Run: model.Run{ID: "run"}}
	// The raw document declares no class; only the receipt sees the fragment's.
	f.scheduler.jobs = exclusiveEngine{groupEngine: f.engine}
	var admission *argoworkflow.AdmissionError
	if _, err := f.scheduler.pipelineExclusiveClasses(t.Context(), request, receipt); !errors.As(err, &admission) || !strings.Contains(err.Error(), `exclusive class "kvm" is not on this server's allowlist`) {
		t.Fatalf("fragment class bypassed the allowlist: %v", err)
	}
	f.scheduler.admission.exclusiveLimits = map[string]int{"kvm": 1}
	classes, err := f.scheduler.pipelineExclusiveClasses(t.Context(), request, receipt)
	if err != nil || len(classes) != 1 || classes[0] != "kvm" {
		t.Fatalf("fragment class not admitted as a member: %v %v", classes, err)
	}
}
