package argojob

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// This is the test that turns "admission-proven" into "actually works": it
// submits the repository's OWN .oberth/build.yaml against a real Argo
// controller and watches the real steps run.
//
// The source comes from the same SourceSeeder the server uses, pointed at this
// checkout: the earlier version of this test took a claim someone had filled by
// hand, which is exactly why it never noticed that a pipeline cannot mount the
// server's own claim across namespaces.
//
// It is slow by nature -- the setup burn compiles golangci-lint and helm from
// source, trivy uses the official image directly, and the test burn runs the
// race suite -- so it also takes its own deadline from
// OBERTH_ARGO_LIVE_PIPELINE_TIMEOUT.
const (
	liveSourceRunEnvironment     = "OBERTH_ARGO_LIVE_SOURCE_RUN"
	livePipelineTimeoutEnvVar    = "OBERTH_ARGO_LIVE_PIPELINE_TIMEOUT"
	livePipelineDefaultDeadline  = 90 * time.Minute
	livePipelineProgressInterval = 30 * time.Second
)

func TestLiveRepositoryBuildPipelineExecutes(t *testing.T) {
	cluster := requireLiveCluster(t)
	runID := strings.TrimSpace(os.Getenv(liveSourceRunEnvironment))
	if runID == "" {
		runID = "oberth-live-ci"
	}
	deadline := livePipelineDefaultDeadline
	if raw := strings.TrimSpace(os.Getenv(livePipelineTimeoutEnvVar)); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("%s: %v", livePipelineTimeoutEnvVar, err)
		}
		deadline = parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	config := cluster.config()
	config.WorkflowTimeout = deadline
	// The repository's own images, exactly as .oberth/build.yaml declares them.
	config.RunnerImagePrefixes = []string{"golang:", "aquasec/trivy:"}

	controller, err := NewController(cluster.workflows, cluster.kube, config)
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	// The production seeding path, not a hand-filled claim: Create copies this
	// checkout into a claim it makes in the pipeline namespace.
	seeder := NewSourceSeeder(cluster.kube, cluster.execStreamer(), config)
	if seeder == nil {
		t.Fatal("source seeder was not constructed")
	}
	controller = controller.WithSourceSeeder(seeder)
	// The committed pipeline by default. OBERTH_ARGO_LIVE_PIPELINE_FILE exists
	// for lab networks that need a variant (this host reaches proxy.golang.org
	// only over IPv6, while kind's pod network is IPv4-only), so the file under
	// test can be a GOPROXY-adjusted copy without editing the real one.
	pipelineFile := strings.TrimSpace(os.Getenv("OBERTH_ARGO_LIVE_PIPELINE_FILE"))
	if pipelineFile == "" {
		pipelineFile = "../../.oberth/build.yaml"
	}
	source, err := os.ReadFile(pipelineFile)
	if err != nil {
		t.Fatalf("read %s: %v", pipelineFile, err)
	}
	t.Logf("pipeline document: %s", pipelineFile)
	request := Request{
		RunID: runID, Name: runID, Repo: "oberth", UpstreamOrg: "skipops",
		Ref: "refs/heads/argo-workflow-execution-engine", SHA: testSHA,
		Trigger: periapsis.TriggerCI, Source: source,
		// This repository's own working tree is the checkout under test.
		SourceDir: "../..",
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
		seeder.DeleteClaim(cleanupCtx, sourceClaimName(request.Name))
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("submit build.yaml: %v", err)
	}
	t.Logf("submitted %s; watching real steps (deadline %s)", request.Name, deadline)

	stop := make(chan struct{})
	defer close(stop)
	go reportProgress(t, ctx, controller, request.Name, stop)

	log := &recordingWriter{}
	completion, err := controller.Wait(ctx, request.Name, request.RunID, log)
	if err != nil {
		t.Fatalf("wait: %v\n%s", err, truncate(log.String(), 20000))
	}

	// Report every step before asserting, so a failure is diagnosable from the
	// test output alone rather than needing the cluster.
	for _, step := range completion.Steps {
		t.Logf("  %-14s %-24s %-8s exit=%d", step.Burn, step.Step, step.Status, step.ExitCode)
	}
	if !completion.Succeeded {
		t.Fatalf("pipeline failed at %s/%s: phase=%s reason=%s\n%s",
			completion.FailedBurn, completion.FailedStep, completion.Phase, completion.Reason,
			truncate(log.String(), 40000))
	}

	// Every burn periapsis.go declares for the CI trigger must have run, with
	// its steps attributed to it.
	burns := map[string][]string{}
	for _, step := range completion.Steps {
		burns[step.Burn] = append(burns[step.Burn], step.Step)
		if step.Status != runprogress.StepPassed {
			t.Errorf("step %s/%s = %s (exit %d)", step.Burn, step.Step, step.Status, step.ExitCode)
		}
	}
	for _, burn := range []string{"setup", "lint", "test", "security", "chart", "build-amd64", "build-arm64"} {
		if len(burns[burn]) == 0 {
			t.Errorf("burn %q ran no steps; observed %v", burn, burns)
		}
	}
	// The setup burn is where per-step granularity actually pays: eight
	// sequential commands, each with its own exit code and log slice.
	if len(burns["setup"]) < 8 {
		t.Errorf("setup ran %d steps, want the 8 periapsis.go declares: %v", len(burns["setup"]), burns["setup"])
	}

	body := log.String()
	if !strings.Contains(body, runprogress.MarkerPrefix) {
		t.Error("no step progress markers reached the run log")
	}
	// Real per-step log slices, not one undifferentiated stream.
	for _, marker := range []string{"[setup/", "[lint/", "[test/"} {
		if !strings.Contains(body, marker) {
			t.Errorf("no log lines attributed to %s", marker)
		}
	}
	t.Logf("pipeline completed: %d steps across %d burns", len(completion.Steps), len(burns))
}

// reportProgress logs which steps are running while the pipeline executes, so
// a slow or stuck run is diagnosable without attaching to the cluster.
func reportProgress(t *testing.T, ctx context.Context, controller *Controller, name string, stop <-chan struct{}) {
	ticker := time.NewTicker(livePipelineProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		workflow, err := controller.workflows.Get(ctx, name, metaGetOptions())
		if err != nil {
			continue
		}
		running, done := []string{}, 0
		for _, step := range StepResults(workflow) {
			switch step.Status {
			case runprogress.StepRunning:
				running = append(running, step.Burn+"/"+step.Step)
			case runprogress.StepPassed, runprogress.StepFailed, runprogress.StepSkipped:
				done++
			}
		}
		t.Logf("  ... phase=%s done=%d running=%v", workflow.Status.Phase, done, running)
	}
}

func metaGetOptions() metav1.GetOptions { return metav1.GetOptions{} }
