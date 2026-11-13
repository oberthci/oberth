// Command fixture creates synthetic upgrade data using the release's own store
// API. Seed with the published source; snapshot with each target source.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type run struct {
	ID        string
	Sequence  int64
	Admission int64
	Repo      int64
	Ref       string
	SHA       string
	Status    model.RunStatus
	Phase     string
	Job       string

	Started *time.Time
}

type migration struct {
	Version   int
	AppliedAt int64
}
type snapshotState struct {
	Ledger []migration
	Runs   []run
	Total  int
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "verify" {
		verifyCommand(os.Args[2:])
		return
	}
	if len(os.Args) < 3 || len(os.Args) > 4 {
		panic("usage: fixture seed|snapshot|refill DATABASE [RUNNER_IMAGE]")
	}
	ctx := context.Background()
	path := os.Args[2]
	if os.Args[1] == "diagnose" {
		diagnose(ctx, path)
		return
	}
	if os.Args[1] == "runs" {
		runs(ctx, path)
		return
	}
	if os.Args[1] == "refill" {
		refill(ctx, path)
		return
	}
	if os.Args[1] == "seed" {
		seed(ctx, path)
		return
	}
	if os.Args[1] != "snapshot" {
		panic("unknown command")
	}
	s, err := store.InspectCurrent(ctx, path, store.Options{})
	must(err)
	defer func() { must(s.Close()) }()
	runs, err := s.ListActiveRuns(ctx, model.RunListFilter{})
	must(err)
	values := make([]run, 0, len(runs))
	for _, r := range runs {
		values = append(values, run{r.ID, r.QueueSequence, r.AdmissionSequence, r.RepoID, r.Ref, r.SHA, r.Status, r.Phase, workflowName(r), r.StartedAt})
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	must(err)
	defer func() { must(db.Close()) }()
	rows, err := db.QueryContext(ctx, "SELECT version, applied_at FROM schema_migrations ORDER BY version")
	must(err)
	defer func() { must(rows.Close()) }()
	ledger := []migration{}
	for rows.Next() {
		var m migration
		must(rows.Scan(&m.Version, &m.AppliedAt))
		ledger = append(ledger, m)
	}
	must(rows.Err())
	if len(ledger) != store.LatestMigrationVersion() {
		panic("migration ledger is incomplete")
	}
	for i, m := range ledger {
		if m.Version != i+1 {
			panic("migration ledger has a gap or duplicate")
		}
	}
	var count int
	must(db.QueryRowContext(ctx, "SELECT count(*) FROM runs").Scan(&count))
	must(json.NewEncoder(os.Stdout).Encode(snapshotState{ledger, values, count}))
}
func seed(ctx context.Context, path string) {
	// Admin opens refuse a missing database; seed creates it the way a
	// server's first start does. CreateGenesis exists in every baseline.
	if created, err := store.CreateGenesis(ctx, path, store.Options{}); err == nil {
		must(created.Close())
	} else if !errors.Is(err, os.ErrExist) {
		must(err)
	}
	s, err := store.OpenAdminClient(ctx, path, store.Options{})
	must(err)
	defer func() { must(s.Close()) }()
	root := filepath.Join(filepath.Dir(path), "fixtures")
	repoPath := filepath.Join(root, "sample")
	must(os.MkdirAll(repoPath, 0750)) // #nosec G703 -- The CLI caller chooses the isolated fixture directory.
	git := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "/usr/bin/git", append([]string{"-C", repoPath}, args...)...) // #nosec G204,G702 -- Commands are fixed below; only the caller-selected fixture directory varies.
		out, err := cmd.CombinedOutput()
		if err != nil {
			panic(fmt.Sprintf("git fixture: %v: %s", err, out))
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	must(os.WriteFile(filepath.Join(repoPath, "README"), []byte("Oberth local upgrade fixture\n"), 0600)) // #nosec G703 -- The CLI caller chooses where its fixture repository is created.
	must(os.MkdirAll(filepath.Join(repoPath, ".oberth"), 0750))                                           // #nosec G703 -- Pipeline files belong beneath the caller-selected fixture directory.
	if len(os.Args) != 4 {
		panic("seed requires the signed release runner image")
	}
	pipeline := fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: trivial
  activeDeadlineSeconds: 600
  templates:
    - name: trivial
      container:
        image: %s
        command: [/usr/local/bin/oberth, version]
`, os.Args[3])
	for _, file := range []string{"build.yaml", "release.yaml"} {
		must(os.WriteFile(filepath.Join(repoPath, ".oberth", file), []byte(pipeline), 0600)) // #nosec G703 -- The pipeline is written beneath the caller-selected fixture directory.
	}
	git("add", "README", ".oberth")
	git("-c", "user.name=Oberth fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Create local fixture")
	sha := git("rev-parse", "HEAD")
	// Populate the native cache so exact-SHA checkouts never fetch an upstream.
	cache := filepath.Join(filepath.Dir(path), "git", "local", "fixtures", "sample.git")
	must(os.MkdirAll(filepath.Dir(cache), 0750)) // #nosec G703 -- The local cache belongs beside the caller-selected fixture database.
	upstreamRepo := filepath.Join(root, "sample.git")
	git("clone", "--bare", repoPath, upstreamRepo)
	git("clone", "--bare", upstreamRepo, cache)
	upstream, err := s.RegisterUpstream(ctx, "upgrade-fixture", model.UpstreamSpec{Name: "local", Kind: "local", BaseURL: root})
	must(err)
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "sample", UpstreamID: upstream.ID, DefaultBranch: "main"})
	must(err)
	enqueue := func(ref string, kind model.RefKind, trigger string) model.Run {
		r, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: kind, Ref: ref, SHA: sha, Release: kind == model.RefTag, Actor: "upgrade-fixture", Trigger: trigger})
		must(err)
		return r.Run
	}
	// This Argo suspension keeps a real in-flight Workflow across every restart
	// without running build containers or contacting an upstream.
	enqueue("feature/running", model.RefBranch, "push")
	running, err := s.ClaimNextRun(ctx)
	must(err)
	_, err = s.MarkRunSubmissionStarted(ctx, running.ID)
	must(err)
	_, err = setWorkflowName(ctx, s, running.ID, "oberth-e2e-running")
	must(err)
	_, err = s.MarkRunSubmitted(ctx, running.ID)
	must(err)
	enqueue("feature/normal", model.RefBranch, "push")
	enqueue("rc/upgrade", model.RefBranch, "push")
	enqueue("v0.0.1-fixture", model.RefTag, "tag")
	enqueue("feature/last", model.RefBranch, "push")
	identity := sha256.Sum256([]byte("Oberth upgrade fixture " + running.ID))
	workflow := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Workflow",
		"metadata": map[string]any{"name": "oberth-e2e-running", "namespace": "oberth-argo",
			"labels":      map[string]string{"oberth.ci/run": running.ID, "oberth.ci/sha": sha, "oberth.ci/trigger": "ci"},
			"annotations": map[string]string{"oberth.ci/run-id": running.ID, "oberth.ci/ref": running.Ref, "oberth.ci/spec-identity": hex.EncodeToString(identity[:]), "example.com/tier": "M"}},
		"spec": map[string]any{"entrypoint": "hold", "serviceAccountName": "oberth-argo-executor", "templates": []any{map[string]any{"name": "hold", "suspend": map[string]any{"duration": "3600s"}}}},
	}
	must(json.NewEncoder(os.Stdout).Encode(workflow))
}

// These API names changed between releases; their persisted JSON field did not.
func workflowName(r model.Run) string {
	data, err := json.Marshal(r)
	must(err)
	var value struct {
		Name string `json:"JobName"`
	}
	must(json.Unmarshal(data, &value))
	return value.Name
}
func setWorkflowName(ctx context.Context, s *store.Store, id, name string) (model.Run, error) {
	if v, ok := any(s).(interface {
		SetRunWorkflowName(context.Context, string, string) (model.Run, error)
	}); ok {
		return v.SetRunWorkflowName(ctx, id, name)
	}
	return any(s).(interface {
		SetRunJobName(context.Context, string, string) (model.Run, error)
	}).SetRunJobName(ctx, id, name)
}

func refill(ctx context.Context, path string) {
	s, err := store.OpenAdminClient(ctx, path, store.Options{})
	must(err)
	defer func() { must(s.Close()) }()
	active, err := s.ListActiveRuns(ctx, model.RunListFilter{})
	must(err)
	if len(active) != 3 {
		panic(fmt.Sprintf("expected holder and two normal runs, got %d", len(active)))
	}
	token := time.Now().UnixNano()
	for _, spec := range []struct {
		ref     string
		kind    model.RefKind
		trigger string
	}{
		{fmt.Sprintf("rc/probe-%d", token), model.RefBranch, "push"},
		{fmt.Sprintf("v0.0.1-probe-%d", token), model.RefTag, "tag"},
	} {
		_, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: active[0].RepoID, RefKind: spec.kind, Ref: spec.ref, SHA: active[0].SHA, Release: spec.kind == model.RefTag, Actor: "upgrade-fixture", Trigger: spec.trigger})
		must(err)
	}
}

func diagnose(ctx context.Context, path string) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	must(err)
	defer func() { must(db.Close()) }()
	rows, err := db.QueryContext(ctx, "SELECT id,status,phase,error FROM runs ORDER BY queue_sequence")
	must(err)
	defer func() { must(rows.Close()) }()
	for rows.Next() {
		var id, status, phase, message string
		must(rows.Scan(&id, &status, &phase, &message))
		must(json.NewEncoder(os.Stdout).Encode(map[string]string{"id": id, "status": status, "phase": phase, "error": message}))
	}
	must(rows.Err())
}

// runs prints every run (terminal ones too) with the reason line, so a
// rollback rehearsal can show that restored work was not dispatched again.
func runs(ctx context.Context, path string) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	must(err)
	defer func() { must(db.Close()) }()
	rows, err := db.QueryContext(ctx, "SELECT id,status,phase,job_name,reason,error FROM runs ORDER BY queue_sequence")
	must(err)
	defer func() { must(rows.Close()) }()
	for rows.Next() {
		var id, status, phase, job, reason, message string
		must(rows.Scan(&id, &status, &phase, &job, &reason, &message))
		must(json.NewEncoder(os.Stdout).Encode(map[string]string{"id": id, "status": status, "phase": phase, "job": job, "reason": reason, "error": message}))
	}
	must(rows.Err())
	var version int
	must(db.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version))
	must(json.NewEncoder(os.Stdout).Encode(map[string]int{"schema": version}))
}
