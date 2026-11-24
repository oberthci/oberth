package app_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestFirstPublicationAndResetKeepSeparateAttemptEvidence(t *testing.T) {
	f := newMovingFixtureFrom(t, tracedPublicationFixture(t))
	g := f.git
	g.Write(t, "next", []byte("tested first publication"))
	next := g.CommitFile(t, "first publication")
	g.Git(t, g.Store(), "fetch", "--no-auto-maintenance", g.Work, next)
	g.Git(t, g.Store(), "update-ref", "refs/heads/main", next)
	p := model.Publication{ID: "first-then-reset", Repo: request().Repository, Ref: "refs/heads/main", Actor: movingCaller().Actor.Identity, ObjectSHA: next, CommitSHA: new(next)}
	landedOK(t, f.db.PreparePublication(t.Context(), stateport.Action{Actor: p.Actor}, stateport.PublicationIntent{Publication: p}))
	f.moving.MovingGit = &lostPublicationReply{MovingGit: f.moving.MovingGit}
	commands := filepath.Join(g.Root, "commands.jsonl")
	landedOK(t, os.WriteFile(commands, nil, 0600))
	landedOK(t, f.svc.Deliver(t.Context(), p.ID))
	// The first delivery binds its predecessor, pushes with a lease and reads
	// back. Its newly committed attempted flag must not add a recovery read.
	requirePublicationCommands(t, commands, []string{"read", "push", "read"})
	stored, err := f.db.Publication(t.Context(), p.ID)
	landedOK(t, err)
	requireEmptyHistoryPublication(t, stored, model.PublicationPending)
	if stored.RetryAt == nil {
		t.Fatal("lost reply did not defer the landed push", stored)
	}
	// A newer accepted local head makes the recovery preflight decisive: the
	// landed object must be confirmed before stale local evidence is checked.
	g.Write(t, "newer", []byte("newer local head"))
	newer := g.CommitFile(t, "local head after publication")
	g.Git(t, g.Store(), "fetch", "--no-auto-maintenance", g.Work, newer)
	g.Git(t, g.Store(), "update-ref", p.Ref, newer)
	call, err := f.svc.PreparePublishRetry(t.Context(), movingCaller(), apicontract.PublishRetryArgs{ID: p.ID, Reason: "confirm landed publication"})
	landedOK(t, err)
	_, err = call(t.Context())
	landedOK(t, err)
	landedOK(t, os.WriteFile(commands, nil, 0600))
	landedOK(t, f.svc.Deliver(t.Context(), p.ID))
	requirePublicationCommands(t, commands, []string{"read"})
	stored, err = f.db.Publication(t.Context(), p.ID)
	landedOK(t, err)
	requireEmptyHistoryPublication(t, stored, model.PublicationDelivered)
	if actual := g.Git(t, g.Bare, "rev-parse", p.Ref); actual != next {
		t.Fatal("retry changed the landed forge head", actual)
	}
	if actual := g.Git(t, g.Store(), "rev-parse", p.Ref); actual != newer {
		t.Fatal("retry changed the newer local head", actual)
	}
}

func tracedPublicationFixture(t *testing.T) *gittest.Fixture {
	t.Helper()
	g := gittest.New(t)
	body := `#!/usr/bin/python3
import json,os,sys
fd=os.open(os.path.dirname(__file__)+'/commands.jsonl',os.O_WRONLY|os.O_CREAT|os.O_APPEND,0o600)
os.write(fd,(json.dumps({'argv':sys.argv[1:]})+'\n').encode())
os.close(fd)
os.execv(` + strconv.Quote(g.Binary) + `,[` + strconv.Quote(g.Binary) + `]+sys.argv[1:])
`
	g.Binary = filepath.Join(g.Root, "traced-git")
	landedOK(t, os.WriteFile(g.Binary, []byte(body), 0700))
	return g
}

func requireEmptyHistoryPublication(t *testing.T, p model.Publication, status model.PublicationStatus) {
	t.Helper()
	if !p.Attempted || p.CIAfter != 0 || p.Attempts != 1 || p.Status != status {
		t.Fatal("publication lost prior attempt evidence or changed the CI bound", p)
	}
}

func requirePublicationCommands(t *testing.T, path string, want []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	landedOK(t, err)
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var command struct {
			Argv []string `json:"argv"`
		}
		landedOK(t, json.Unmarshal([]byte(line), &command))
		if slices.Contains(command.Argv, "ls-remote") {
			got = append(got, "read")
		}
		if slices.Contains(command.Argv, "push") {
			if !slices.ContainsFunc(command.Argv, func(arg string) bool { return strings.HasPrefix(arg, "--force-with-lease=") }) {
				t.Fatal("publication push lacked its lease", command.Argv)
			}
			got = append(got, "push")
		}
	}
	if !slices.Equal(got, want) {
		t.Fatal("unexpected publication reads or pushes", got, want, string(data))
	}
}
