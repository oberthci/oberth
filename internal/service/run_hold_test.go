package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/holdfs"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type holdHistory struct {
	RunHistory
	runs map[string]model.Run
}

func (history holdHistory) ResolveRunID(_ context.Context, id string) (model.Run, error) {
	if run, ok := history.runs[id]; ok {
		return run, nil
	}
	return model.Run{}, store.ErrNotFound
}

// holdAssistStore mirrors store.AppendRunAssist: it authorizes, appends and
// marks a continue assisted.
type holdAssistStore struct {
	history holdHistory
	events  []model.RunAssistEvent
}

func (s *holdAssistStore) AppendRunAssist(_ context.Context, request model.RunAssistRequest) (model.Run, error) {
	run := s.history.runs[request.RunID]
	if request.Authorize != nil {
		if err := request.Authorize(run); err != nil {
			return model.Run{}, err
		}
	}
	event := request.Event
	record := model.RunAssistRecord{}
	if run.Assist != nil {
		record = *run.Assist
	}
	if event.Kind == model.RunAssistContinue {
		event.Assisted = record.Touched(event.Step)
	}
	if event.Kind != model.RunAssistAbandon {
		run.Assisted = true
	}
	record.Events = append(record.Events, event)
	run.Assist = &record
	s.events = append(s.events, event)
	s.history.runs[request.RunID] = run
	return run, nil
}

// holdOperator fakes the engine: a fixed hold list, and an exec that runs
// the real holdfs helper against a temporary tree when asked for it.
type holdOperator struct {
	enabled    bool
	holds      []model.AssistHold
	root       string
	execs      [][]string
	released   []string
	abandoned  []string
	releaseErr error
}

func (operator *holdOperator) AssistPolicy() model.AssistPolicy {
	return model.AssistPolicy{Enabled: operator.enabled, HoldTTL: 20 * time.Minute, MaxConcurrentHolds: 2, ExecTimeout: time.Minute, ExecOutputBytes: 4096, PatchMaxBytes: 1 << 16, PatchMaxFiles: 4}
}

func (operator *holdOperator) ListAssistHolds(context.Context, string) ([]model.AssistHold, error) {
	return operator.holds, nil
}

func (operator *holdOperator) ExecAssistHold(_ context.Context, request model.AssistExecRequest) (model.AssistExecResult, error) {
	operator.execs = append(operator.execs, request.Argv)
	if request.Argv[0] == assistHoldHelperPath && len(request.Argv) > 3 {
		args := append([]string{}, request.Argv[2:]...)
		for index := range args {
			if args[index] == assistHoldHelperRoot {
				args[index] = operator.root
			}
		}
		var stdout, stderr bytes.Buffer
		code := holdfs.Run(args, bytes.NewReader(request.Stdin), &stdout, &stderr)
		return model.AssistExecResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
	}
	if request.Argv[0] == "false" {
		return model.AssistExecResult{ExitCode: 1, Stderr: "nope"}, nil
	}
	return model.AssistExecResult{Stdout: "ok\n"}, nil
}

func (operator *holdOperator) ReleaseAssistHold(_ context.Context, pod string) error {
	if operator.releaseErr != nil {
		return operator.releaseErr
	}
	operator.released = append(operator.released, pod)
	return nil
}

func (operator *holdOperator) AbandonAssistHold(_ context.Context, pod string) error {
	operator.abandoned = append(operator.abandoned, pod)
	return nil
}

func holdFixture(t *testing.T) (*API, *holdOperator, *holdAssistStore, model.Run) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := model.Run{ID: strings.Repeat("a", 32), RepoID: 1, RefKind: model.RefBranch, Ref: "feature/x", Trigger: "branch", Status: model.RunRunning,
		Actor: "uplink-a", WorkflowName: "wf-a", AssistRequested: true, FailedStep: "test"}
	history := holdHistory{runs: map[string]model.Run{run.ID: run}}
	operator := &holdOperator{enabled: true, root: root, holds: []model.AssistHold{{Pod: "wf-a-test-1", Step: "test", Attempt: 1, PatchCapable: true, HeldSince: time.Now(), SourcePath: "/work/src"}}}
	assists := &holdAssistStore{history: history}
	service := &API{history: history, assistHolds: operator, runAssists: assists, assistGate: newAssistGate(2), signals: NewSignals()}
	return service, operator, assists, run
}

func TestRunHoldToolsAuthorizeRepositoryPushersOrAdmin(t *testing.T) {
	service, operator, assists, run := holdFixture(t)
	ctx := context.Background()
	owner, other, admin := api.Actor{Identity: "uplink-a"}, api.Actor{Identity: "uplink-b"}, api.Actor{Identity: "root", Admin: true}

	if _, err := service.listRunHolds(ctx, other, run.ID); err != nil {
		t.Fatalf("listing is a read for any actor who sees the run: %v", err)
	}
	// An unscoped uplink may push to every repository, so it may service
	// the hold of a run another uplink pushed; the record names it.
	if _, err := service.execRunHold(ctx, other, run.ID, "", []string{"true"}, "", 0); err != nil {
		t.Fatalf("other unscoped uplink exec: %v", err)
	}
	if got := assists.events[len(assists.events)-1].Actor; got != "uplink-b" {
		t.Fatalf("assist record names %q, want the servicing uplink", got)
	}
	if _, err := service.execRunHold(ctx, owner, run.ID, "", []string{"true"}, "", 0); err != nil {
		t.Fatalf("owner exec: %v", err)
	}
	if _, err := service.execRunHold(ctx, admin, run.ID, "test", []string{"true"}, "", 0); err != nil {
		t.Fatalf("admin exec: %v", err)
	}
	if len(operator.execs) != 3 {
		t.Fatalf("execs = %v", operator.execs)
	}

	operator.enabled = false
	if _, err := service.listRunHolds(ctx, owner, run.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("disabled policy = %v, want unavailable", err)
	}
	operator.enabled = true

	plain := run
	plain.ID = strings.Repeat("b", 32)
	plain.AssistRequested = false
	service.history.(holdHistory).runs[plain.ID] = plain
	if _, err := service.execRunHold(ctx, owner, plain.ID, "", []string{"true"}, "", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("run that never asked = %v, want invalid input", err)
	}
	tag := run
	tag.ID = strings.Repeat("c", 32)
	tag.RefKind, tag.Release, tag.Trigger = model.RefTag, true, "release"
	service.history.(holdHistory).runs[tag.ID] = tag
	if _, err := service.execRunHold(ctx, admin, tag.ID, "", []string{"true"}, "", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("release run = %v, want forbidden", err)
	}
	finished := run
	finished.ID = strings.Repeat("d", 32)
	finished.Status = model.RunFailed
	service.history.(holdHistory).runs[finished.ID] = finished
	if _, err := service.continueRunHold(ctx, owner, finished.ID, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("finished run = %v, want invalid input", err)
	}
	if _, err := service.listRunHolds(ctx, owner, strings.Repeat("9", 32)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
}

func TestRunHoldExecBoundsArgv(t *testing.T) {
	service, _, assists, run := holdFixture(t)
	ctx := context.Background()
	owner := api.Actor{Identity: "uplink-a"}
	cases := map[string][]string{
		"empty":    {},
		"blank":    {" "},
		"too many": make([]string, maxAssistExecArgs+1),
		"too long": {strings.Repeat("x", maxAssistExecArgBytes+1)},
		"nul":      {"go", "test\x00"},
	}
	for name, argv := range cases {
		for index := range argv {
			if argv[index] == "" && name == "too many" {
				argv[index] = "x"
			}
		}
		if _, err := service.execRunHold(ctx, owner, run.ID, "", argv, "", 0); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want invalid input", name, err)
		}
	}
	if _, err := service.execRunHold(ctx, owner, run.ID, "", []string{"true"}, strings.Repeat("s", maxAssistStdinBytes+1), 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized stdin = %v", err)
	}
	if len(assists.events) != 0 {
		t.Fatal("refused execs were recorded")
	}
	response, err := service.execRunHold(ctx, owner, run.ID, "", []string{"false"}, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if response.Result.ExitCode != 1 || response.Step != "test" || len(assists.events) != 1 || assists.events[0].Kind != model.RunAssistExec || assists.events[0].Argv[0] != "false" {
		t.Fatalf("exec response %+v events %+v", response, assists.events)
	}
	if !service.history.(holdHistory).runs[run.ID].Assisted {
		t.Fatal("exec alone must mark the run assisted before the command runs")
	}
	// A refused exec (unknown step) records nothing; a recorded exec whose
	// command fails to start still leaves the run assisted.
	if _, err := service.execRunHold(ctx, owner, run.ID, "nope", []string{"true"}, "", 0); !errors.Is(err, ErrInvalidInput) || len(assists.events) != 1 {
		t.Fatalf("unknown step exec = %v, events %d", err, len(assists.events))
	}
}

func TestRunHoldPatchAppliesThroughHelperAndContinueMarksAssisted(t *testing.T) {
	service, operator, assists, run := holdFixture(t)
	ctx := context.Background()
	owner := api.Actor{Identity: "uplink-a"}
	diff := "--- a/go.mod\n+++ b/go.mod\n@@ -1,3 +1,3 @@\n module example.com/x\n \n-go 1.26\n+go 1.27\n--- /dev/null\n+++ b/NOTE\n@@ -0,0 +1 @@\n+patched\n"
	response, err := service.patchRunHold(ctx, owner, run.ID, "", diff)
	if err != nil {
		t.Fatal(err)
	}
	if response.PatchSHA256 != holdfs.Digest([]byte(diff)) || len(response.Files) != 2 || response.Step != "test" {
		t.Fatalf("patch response %+v", response)
	}
	if !service.history.(holdHistory).runs[run.ID].Assisted || assists.events[0].Kind != model.RunAssistPatch || assists.events[0].PatchSHA256 != response.PatchSHA256 {
		t.Fatalf("patch alone must mark the run assisted before it is applied: %+v", assists.events)
	}
	if got, _ := os.ReadFile(filepath.Join(operator.root, "go.mod")); string(got) != "module example.com/x\n\ngo 1.27\n" {
		t.Fatalf("go.mod = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(operator.root, "NOTE")); string(got) != "patched\n" {
		t.Fatalf("NOTE = %q", got)
	}
	if _, err := service.patchRunHold(ctx, owner, run.ID, "", diff); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("re-applying a consumed patch = %v, want context refusal", err)
	}
	if _, err := service.patchRunHold(ctx, owner, run.ID, "", "--- a/../x\n+++ b/../x\n@@ -1 +1 @@\n-a\n+b\n"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("traversal = %v", err)
	}
	operator.holds[0].PatchCapable = false
	if _, err := service.patchRunHold(ctx, owner, run.ID, "", diff); err == nil || !strings.Contains(err.Error(), "inspect-only") {
		t.Fatalf("inspect-only lane = %v", err)
	}
	operator.holds[0].PatchCapable = true

	operator.releaseErr = errors.New("exec failed")
	if _, err := service.continueRunHold(ctx, owner, run.ID, ""); err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("failed release = %v, want an abandoned hold", err)
	}
	if continued, abandonedEvent := assists.events[len(assists.events)-2], assists.events[len(assists.events)-1]; continued.Kind != model.RunAssistContinue || !continued.Assisted || abandonedEvent.Kind != model.RunAssistAbandon || operator.abandoned[0] != "wf-a-test-1" {
		t.Fatalf("failed release events: %+v", assists.events)
	}
	if !service.history.(holdHistory).runs[run.ID].Assisted {
		t.Fatal("run not assisted after an attempted continue")
	}
	operator.releaseErr = nil
	// A new attempt of the same step parks again only in a new run; emulate
	// a fresh hold Pod to exercise the successful path.
	operator.holds = []model.AssistHold{{Pod: "wf-a-test-2", Step: "test", Attempt: 2, PatchCapable: true, HeldSince: time.Now()}}
	continued, err := service.continueRunHold(ctx, owner, run.ID, "")
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !continued.Assisted || !strings.Contains(continued.Reason, "assisted: step test continued after patch sha256:"+response.PatchSHA256[:12]) || operator.released[0] != "wf-a-test-2" {
		t.Fatalf("continue response %+v released %v", continued, operator.released)
	}
	listed, err := service.listRunHolds(ctx, owner, run.ID)
	if err != nil || len(listed.Holds) != 1 || !listed.Holds[0].Released || !listed.Assisted {
		t.Fatalf("list after continue = %+v, %v", listed, err)
	}
}

func TestRunHoldStepSelectionAndAbandon(t *testing.T) {
	service, operator, assists, run := holdFixture(t)
	ctx := context.Background()
	owner := api.Actor{Identity: "uplink-a"}
	operator.holds = append(operator.holds, model.AssistHold{Pod: "wf-a-lint-1", Step: "lint", Attempt: 1, HeldSince: time.Now()})
	if _, err := service.continueRunHold(ctx, owner, run.ID, ""); err == nil || !strings.Contains(err.Error(), "several steps") {
		t.Fatalf("ambiguous step = %v", err)
	}
	if _, err := service.continueRunHold(ctx, owner, run.ID, "build"); err == nil || !strings.Contains(err.Error(), "does not hold step") {
		t.Fatalf("unknown step = %v", err)
	}
	abandoned, err := service.abandonRunHold(ctx, owner, run.ID, "lint")
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.Pod != "wf-a-lint-1" || abandoned.Assisted || operator.abandoned[0] != "wf-a-lint-1" || assists.events[0].Kind != model.RunAssistAbandon {
		t.Fatalf("abandon = %+v events %+v", abandoned, assists.events)
	}
	// The abandoned hold is no longer selectable, so the remaining one is unambiguous.
	if _, err := service.continueRunHold(ctx, owner, run.ID, ""); err != nil {
		t.Fatalf("continue after abandon: %v", err)
	}
	gate := newAssistGate(1)
	release, err := gate.acquire("r1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.acquire("r1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("same run twice = %v", err)
	}
	if _, err := gate.acquire("r2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("over the installation slot cap = %v", err)
	}
	release()
	if release2, err := gate.acquire("r2"); err != nil {
		t.Fatal(err)
	} else {
		release2()
	}
}

// A scoped uplink services holds only on runs of repositories its scope
// covers, the same rule that decides where it may push; a scope that does
// not cover the run is refused even if the run were visible.
func TestRunHoldScopedServicerNeedsTheRunRepository(t *testing.T) {
	service, operator, assists, run := holdFixture(t)
	ctx := context.Background()
	scopedActor := api.Actor{Identity: "uplink-c", Scope: []string{"codeberg"}}
	covering := *service
	covering.scope = &repoScope{actor: scopedActor, repos: map[int64]model.Repository{run.RepoID: {ID: run.RepoID}}}
	if _, err := covering.execRunHold(ctx, scopedActor, run.ID, "", []string{"true"}, "", 0); err != nil {
		t.Fatalf("scoped uplink covering the repository: %v", err)
	}
	if _, err := covering.patchRunHold(ctx, scopedActor, run.ID, "", "--- a/go.mod\n+++ b/go.mod\n@@ -1 +1 @@\n-module example.com/x\n+module example.com/y\n"); err != nil {
		t.Fatalf("scoped uplink patch: %v", err)
	}
	if got := assists.events[len(assists.events)-1]; got.Actor != "uplink-c" || got.Kind != model.RunAssistPatch {
		t.Fatalf("last assist event = %+v, want a patch by uplink-c", got)
	}
	elsewhere := *service
	elsewhere.scope = &repoScope{actor: scopedActor, repos: map[int64]model.Repository{run.RepoID + 1: {ID: run.RepoID + 1}}}
	before := len(operator.execs)
	for name, call := range map[string]func() error{
		"exec": func() error {
			_, err := elsewhere.execRunHold(ctx, scopedActor, run.ID, "", []string{"true"}, "", 0)
			return err
		},
		"continue": func() error { _, err := elsewhere.continueRunHold(ctx, scopedActor, run.ID, ""); return err },
		"abandon":  func() error { _, err := elsewhere.abandonRunHold(ctx, scopedActor, run.ID, ""); return err },
	} {
		if err := call(); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s outside the scope = %v, want forbidden", name, err)
		}
	}
	// A scoped actor without its scoped view fails closed.
	if _, err := service.execRunHold(ctx, scopedActor, run.ID, "", []string{"true"}, "", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("scoped actor on an unscoped view = %v, want forbidden", err)
	}
	if len(operator.execs) != before || len(operator.released) != 0 || len(operator.abandoned) != 0 {
		t.Fatalf("a refused call reached the engine: execs %v released %v abandoned %v", operator.execs[before:], operator.released, operator.abandoned)
	}
	// The run's own uplink and an admin keep working.
	if _, err := elsewhere.execRunHold(ctx, api.Actor{Identity: "root", Admin: true}, run.ID, "", []string{"true"}, "", 0); err != nil {
		t.Fatalf("admin exec: %v", err)
	}
}

// run_hold_list reports the server's own abandon deadline and spells the
// policy durations in seconds and text, never nanoseconds.
func TestRunHoldListReportsAbandonAtAndReadableDurations(t *testing.T) {
	service, operator, assists, run := holdFixture(t)
	ctx := context.Background()
	held := operator.holds[0].HeldSince
	listed, err := service.listRunHolds(ctx, api.Actor{Identity: "uplink-a"}, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if at := listed.Holds[0].AbandonAt; at == nil || !at.Equal(held.Add(20*time.Minute)) {
		t.Fatalf("idle abandon_at = %v, want held_since + hold TTL", at)
	}
	// An action moves the deadline to last action + TTL, capped at 2 TTLs.
	worked := run
	worked.Assist = &model.RunAssistRecord{Events: []model.RunAssistEvent{{Pod: operator.holds[0].Pod, Kind: model.RunAssistExec, At: held.Add(30 * time.Minute)}}}
	assists.history.runs[run.ID] = worked
	listed, err = service.listRunHolds(ctx, api.Actor{Identity: "uplink-a"}, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if at := listed.Holds[0].AbandonAt; at == nil || !at.Equal(held.Add(40*time.Minute)) {
		t.Fatalf("worked abandon_at = %v, want the 2-TTL cap", at)
	}
	encoded, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Policy map[string]any `json:"policy"`
		Holds  []map[string]any
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Policy["hold_ttl"] != "20m0s" || wire.Policy["hold_ttl_s"] != float64(1200) || wire.Policy["exec_timeout"] != "1m0s" || wire.Policy["exec_timeout_s"] != float64(60) || wire.Policy["enabled"] != true {
		t.Fatalf("policy = %v", wire.Policy)
	}
	released := worked
	released.Assist.Events = append(released.Assist.Events, model.RunAssistEvent{Pod: operator.holds[0].Pod, Kind: model.RunAssistContinue, At: held.Add(31 * time.Minute)})
	assists.history.runs[run.ID] = released
	if listed, err = service.listRunHolds(ctx, api.Actor{Identity: "uplink-a"}, run.ID); err != nil || listed.Holds[0].AbandonAt != nil {
		t.Fatalf("released hold abandon_at = %v, %v", listed.Holds[0].AbandonAt, err)
	}
}
