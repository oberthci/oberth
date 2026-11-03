// Command fixture creates synthetic upgrade data using the release's own store
// API. It is compiled against v0.17.81, never against the upgrade target.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	_ "modernc.org/sqlite"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	if len(os.Args) != 3 {
		panic("usage: fixture seed|snapshot DATABASE")
	}
	ctx := context.Background()
	path := os.Args[2]
	if os.Args[1] == "seed" {
		seed(ctx, path)
		return
	}
	if os.Args[1] != "snapshot" {
		panic("unknown command")
	}
	s, err := store.InspectCurrent(ctx, path, store.Options{})
	must(err)
	defer s.Close()
	runs, err := s.ListActiveRuns(ctx, model.RunListFilter{})
	must(err)
	type run struct {
		ID             string
		Sequence       int64
		Admission      int64
		Repo           int64
		Ref            string
		SHA            string
		Status         model.RunStatus
		Phase          string
		Job            string
		Priority       model.RunPriority
		PriorityReason string
	}
	values := make([]run, 0, len(runs))
	for _, r := range runs {
		values = append(values, run{r.ID, r.QueueSequence, r.AdmissionSequence, r.RepoID, r.Ref, r.SHA, r.Status, r.Phase, r.JobName, r.Priority, r.PriorityReason})
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	must(err)
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT version, applied_at FROM schema_migrations ORDER BY version")
	must(err)
	defer rows.Close()
	type migration struct {
		Version   int
		AppliedAt int64
	}
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
	must(json.NewEncoder(os.Stdout).Encode(struct {
		Ledger []migration
		Runs   []run
		Total  int
	}{ledger, values, count}))
}
func seed(ctx context.Context, path string) {
	s, err := store.Open(ctx, path, store.Options{})
	must(err)
	defer s.Close()
	root := filepath.Join(filepath.Dir(path), "fixtures")
	repoPath := filepath.Join(root, "sample")
	must(os.MkdirAll(repoPath, 0750))
	git := func(args ...string) string {
		cmd := exec.Command("/usr/bin/git", append([]string{"-C", repoPath}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			panic(fmt.Sprintf("git fixture: %v: %s", err, out))
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	must(os.WriteFile(filepath.Join(repoPath, "README"), []byte("Oberth local upgrade fixture\n"), 0640))
	git("add", "README")
	git("-c", "user.name=Oberth fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "Create local fixture")
	sha := git("rev-parse", "HEAD")
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "local", Kind: "local", BaseURL: root})
	must(err)
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "sample", UpstreamID: upstream.ID, DefaultBranch: "main"})
	must(err)
	enqueue := func(ref string, kind model.RefKind, trigger string) model.Run {
		r, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: kind, Ref: ref, SHA: sha, Actor: "upgrade-fixture", Trigger: trigger})
		must(err)
		return r.Run
	}
	// This Argo suspension keeps a real in-flight Workflow across every restart
	// without running build containers or contacting an upstream.
	enqueue("feature/running", model.RefBranch, "push")
	running, err := s.ClaimNextRun(ctx)
	must(err)
	_, err = s.MarkRunAdmitted(ctx, running.ID)
	must(err)
	_, err = s.SetRunJobName(ctx, running.ID, "oberth-e2e-running")
	must(err)
	_, err = s.MarkRunSubmitted(ctx, running.ID)
	must(err)
	_, err = s.SetQueuePriorityPatterns(ctx, "upgrade-fixture", []string{"rc/*"})
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
			"annotations": map[string]string{"oberth.ci/run-id": running.ID, "oberth.ci/ref": running.Ref, "oberth.ci/spec-identity": hex.EncodeToString(identity[:]), "oberth.ci/size": "M"}},
		"spec": map[string]any{"entrypoint": "hold", "serviceAccountName": "oberth-argo-executor", "templates": []any{map[string]any{"name": "hold", "suspend": map[string]any{"duration": "3600s"}}}},
	}
	must(json.NewEncoder(os.Stdout).Encode(workflow))
}
