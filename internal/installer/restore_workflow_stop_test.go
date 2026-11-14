package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRestoreOrphanedWorkflowsUseFencedStop(t *testing.T) {
	for _, mode := range []string{"running", "already-stopped", "terminal", "replacement", "changed-version", "missing-uid", "missing-version", "missing-run"} {
		t.Run(mode, func(t *testing.T) {
			observed := runWorkflow{Name: "orphan", RunID: "run", UID: "original", ResourceVersion: "7", Phase: "Running"}
			currentUID, currentVersion := observed.UID, observed.ResourceVersion
			wantError, wantPatches := false, 1
			switch mode {
			case "already-stopped":
				observed.Shutdown, wantPatches = "Stop", 0
			case "terminal":
				observed.Phase, wantPatches = "Failed", 0
			case "replacement":
				currentUID, wantError = "replacement", true
			case "changed-version":
				currentVersion, wantError = "8", true
			case "missing-uid":
				observed.UID, wantError, wantPatches = "", true, 0
			case "missing-version":
				observed.ResourceVersion, wantError, wantPatches = "", true, 0
			case "missing-run":
				observed.RunID, wantError, wantPatches = "", true, 0
			}
			patches, deletes := 0, 0
			shutdown := ""
			deps := backupDeps{Deps: Deps{ContextName: "restore-context", RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
				if name != "kubectl" || len(args) < 3 || args[0] != "--context" || args[1] != "restore-context" {
					t.Fatalf("wrong restore target: %s %v", name, args)
				}
				args = args[2:]
				if args[0] == "delete" {
					deletes++
					return nil, nil
				}
				if len(args) != 8 || args[0] != "patch" || args[1] != "workflows.argoproj.io" || args[2] != "orphan" || args[3] != "-n" || args[4] != "pipelines" || args[5] != "--type=merge" || args[6] != "-p" {
					t.Fatalf("unexpected restore mutation: %v", args)
				}
				patches++
				var body map[string]map[string]string
				if err := json.Unmarshal([]byte(args[7]), &body); err != nil {
					t.Fatal(err)
				}
				if len(body) != 2 || len(body["metadata"]) != 2 || len(body["spec"]) != 1 || body["spec"]["shutdown"] != "Stop" || body["metadata"]["uid"] != "original" || body["metadata"]["resourceVersion"] != "7" {
					t.Fatalf("Stop rewrote the graph or lost its object fence: %s", args[7])
				}
				if body["metadata"]["uid"] != currentUID || body["metadata"]["resourceVersion"] != currentVersion {
					return nil, errors.New("object precondition conflict")
				}
				shutdown = body["spec"]["shutdown"]
				return nil, nil
			}}}
			err := stopWorkflows(t.Context(), deps, "pipelines", []runWorkflow{observed})
			if (err != nil) != wantError || patches != wantPatches || deletes != 0 {
				t.Fatalf("patches=%d deletes=%d error=%v", patches, deletes, err)
			}
			if wantError && shutdown != "" {
				t.Fatal("conflict or missing custody changed the Workflow")
			}
			if !wantError && wantPatches == 1 {
				if shutdown != "Stop" {
					t.Fatal("native Stop was not applied")
				}
				observed.Shutdown, observed.ResourceVersion = shutdown, "8"
				if err := stopWorkflows(t.Context(), deps, "pipelines", []runWorkflow{observed}); err != nil || patches != 1 {
					t.Fatal("double Stop was not idempotent", err)
				}
			}
		})
	}
}

func TestRestoreListsWorkflowStopCustody(t *testing.T) {
	deps := backupDeps{runStdout: func(context.Context, []byte, string, ...string) ([]byte, error) {
		return []byte(`{"items":[{"metadata":{"name":"orphan","uid":"original","resourceVersion":"7","labels":{"oberth.ci/run":"run"}},"spec":{"shutdown":"Stop"},"status":{"phase":"Running"}}]}`), nil
	}}
	got, err := listRunWorkflows(t.Context(), deps, "pipelines")
	if err != nil || len(got) != 1 || got[0] != (runWorkflow{Name: "orphan", RunID: "run", UID: "original", ResourceVersion: "7", Shutdown: "Stop", Phase: "Running"}) {
		t.Fatalf("lost restore custody: %+v error=%v", got, err)
	}
}

func TestRestoreKeepsServerStoppedWhenWorkflowStopListingFails(t *testing.T) {
	archive, _, _, target := reinstallFixture(t)
	var output bytes.Buffer
	deps := target.deps(&output)
	stdout := deps.runStdout
	lists := 0
	deps.runStdout = func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		if name == "kubectl" && slices.Contains(args, "workflows.argoproj.io") {
			lists++
			if lists == 2 {
				return nil, errors.New("cluster API unreachable")
			}
		}
		return stdout(ctx, input, name, args...)
	}
	cfg := RestoreConfig{Input: archive, Yes: true, Timeout: 10 * time.Second}
	cfg.applyDefaults()
	err := runRestore(t.Context(), cfg, deps)
	if err == nil || !strings.Contains(err.Error(), "list Workflows for Stop") || !slices.Equal(target.scales, []int32{0}) || len(target.stopped) != 0 || len(target.deleted) != 0 {
		t.Fatalf("restore resumed without Stop custody: scales=%v stopped=%v deleted=%v error=%v", target.scales, target.stopped, target.deleted, err)
	}
}
