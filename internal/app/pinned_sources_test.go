package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type sourceRegistryFixture map[string]bool

func (f sourceRegistryFixture) RepositoryRegistered(_ context.Context, name string) (bool, error) {
	return f[name], nil
}

type sourceGitFixture struct {
	ensures, exports             int
	stale, unreachable, oversize bool
}

type sourceRecorderFixture struct {
	sources []pipeline.PinnedSource
	fail    bool
}

func (f *sourceRecorderFixture) RecordRunSources(_ context.Context, id, sha string, sources []pipeline.PinnedSource) error {
	if f.fail {
		return errors.New("record failed")
	}
	f.sources = append([]pipeline.PinnedSource(nil), sources...)
	return nil
}

func TestPinnedSourcesCreateRequiresReceiptAndWritesPublicRunLog(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded", true: "record-refused"}[fail], func(t *testing.T) {
			fixture := newRaceFixture(t)
			request := fixture.request("source-workflow", "source-run")
			request.UpstreamName = "forge"
			request.UpstreamOrg = "acme"
			file := filepath.Join(fixture.sourceDir, ".oberth", "build.yaml")
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("a", 40)
			extension := "sources:\n- repository: forge/acme/producer\n  commit: \"" + sha + "\"\n  path: /work/sources/producer\n"
			if err := os.WriteFile(file, append([]byte(extension), body...), 0644); err != nil {
				t.Fatal(err)
			}
			loader, err := NewPinnedSourceLoader(&sourceGitFixture{}, sourceRegistryFixture{"forge/acme/producer": true}, `{"forge/acme/test-repo":["forge/acme/producer"]}`)
			if err != nil {
				t.Fatal(err)
			}
			recorder := &sourceRecorderFixture{fail: fail}
			fixture.jobs.SetPinnedSources(loader, recorder)
			err = fixture.jobs.CreateWorkflow(t.Context(), request)
			if fail {
				if err == nil || len(fixture.controller.created) != 0 {
					t.Fatal("created without durable sources")
				}
				return
			}
			if err != nil || len(recorder.sources) != 1 {
				t.Fatalf("create: %v", err)
			}
			var log bytes.Buffer
			if _, err := fixture.jobs.Wait(t.Context(), request.WorkflowName, &log); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(log.String(), "source forge/acme/producer@"+sha+" mounted read-only at /work/sources/producer") {
				t.Fatalf("missing public source log: %s", &log)
			}
		})
	}
}

func (f *sourceGitFixture) Ensure(context.Context, string) (gitcache.Repository, error) {
	f.ensures++
	return gitcache.Repository{Stale: f.stale}, nil
}
func (f *sourceGitFixture) ReachableFromUpstreamDefault(context.Context, string, string) (bool, error) {
	return !f.unreachable, nil
}
func (f *sourceGitFixture) ExportPinnedTree(_ context.Context, repo, sha, dir string, limit int64) error {
	f.exports++
	if f.oversize {
		return errors.New("size bound")
	}
	if limit != 128<<20 {
		return errors.New("wrong bound")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "content"), []byte(repo+"@"+sha), 0644)
}

func TestPinnedSourcesAuthorizeBeforeFetchAndFailClosed(t *testing.T) {
	for _, mode := range []string{"allowed", "disabled", "unlisted", "unregistered", "wrong-upstream", "wrong-sha", "unreachable", "stale", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			git := &sourceGitFixture{stale: mode == "stale", unreachable: mode == "unreachable", oversize: mode == "oversize"}
			producer := "forge/acme/producer"
			consumer := "forge/acme/consumer"
			policy := `{"forge/acme/consumer":["forge/acme/producer"]}`
			if mode == "disabled" {
				policy = "{}"
			}
			if mode == "unlisted" {
				producer = "forge/acme/other"
			}
			if mode == "wrong-upstream" {
				consumer = "other/acme/consumer"
			}
			loader, err := NewPinnedSourceLoader(git, sourceRegistryFixture{producer: mode != "unregistered"}, policy)
			if err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("a", 40)
			if mode == "wrong-sha" {
				sha = "main"
			}
			parent := t.TempDir()
			sources := []pipeline.PinnedSource{{Repository: producer, Commit: sha, Path: "/work/sources/producer"}}
			trees, cleanup, err := loader.prepare(t.Context(), consumer, parent, sources)
			if mode == "allowed" {
				if err != nil || len(trees) != 1 {
					t.Fatalf("prepare: %v", err)
				}
				body, err := os.ReadFile(filepath.Join(trees[0].Directory, "content"))
				if err != nil || string(body) != producer+"@"+sha {
					t.Fatalf("wrong tree %q %v", body, err)
				}
			} else if err == nil {
				t.Fatal("unsafe source accepted")
			}
			cleanup()
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatal("source scratch leaked")
			}
			if mode == "disabled" || mode == "unlisted" || mode == "unregistered" || mode == "wrong-upstream" || mode == "wrong-sha" {
				if git.ensures != 0 || git.exports != 0 {
					t.Fatal("unauthorized source fetched")
				}
			}
		})
	}
}
