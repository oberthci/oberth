package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// The real three-second head waits must overlap, including when an answered
// head refuses a commit that the recently refreshed cache still reaches.
func TestAdmissionWaitsOnceForDistinctSilentProducers(t *testing.T) {
	for _, answer := range []string{"silent", "reachable", "unreachable"} {
		t.Run(answer, func(t *testing.T) {
			f := newFixture(t)
			readers := map[string]func(context.Context, sourcecontract.Revision) (int64, bool, error){}
			var pins []sourcecontract.Descriptor
			var repositories []string
			for i := range 3 {
				repo := fmt.Sprintf("origin/team/producer%d", i)
				state := "silent"
				if i == 1 {
					state = answer
				}
				pin, read := pinProducer(t, repo, state)
				pins = append(pins, pin, pin)
				repositories = append(repositories, repo)
				readers[repo] = read
			}
			f.d.SourcePolicy, _ = json.Marshal(map[string][]string{request().Repository: repositories})
			f.setDocument(pinnedWorkflow(pins...))
			f.source.Reachable = func(ctx context.Context, at sourcecontract.Revision) (int64, bool, error) {
				return readers[at.Repository](ctx, at)
			}
			s := f.new(t)
			// Two serial waits already exceed this bound. It leaves two seconds
			// for local Git and preparation around the real three-second wait.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			prepared, err := s.Admit(ctx, request(), f.source)
			if ctx.Err() != nil {
				t.Fatal("producer head waits did not overlap", err)
			}
			if answer == "unreachable" {
				expectRefusal(t, err, "reachable")
				if prepared != nil {
					t.Fatal("answered head did not refuse the admission", err)
				}
			} else if err != nil || len(prepared.View().SourceBindings) != len(pins) {
				t.Fatal("silent and answered producers were not bound", err)
			}
		})
	}
}

func pinProducer(t *testing.T, repo, answer string) (sourcecontract.Descriptor, func(context.Context, sourcecontract.Revision) (int64, bool, error)) {
	t.Helper()
	g := gittest.New(t)
	// Cache both disconnected histories, with the requested commit on main.
	g.Git(t, g.Work, "checkout", "--orphan", "other")
	g.Write(t, "other", []byte("unrelated history"))
	other := g.CommitFile(t, "other history")
	g.Git(t, g.Work, "push", g.Bare, "other")
	arm := holdPinHead(t, g)
	c := g.Cache(t, nil)
	if err := c.Init(t.Context(), repo); err != nil {
		t.Fatal(err)
	}
	switch answer {
	case "silent":
		arm()
	case "unreachable":
		g.Git(t, g.Bare, "update-ref", "refs/heads/main", other)
	}
	return sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: repo, Commit: g.Commit}, c.Pin
}

// A FIFO holds only head reads after cloning. Cache.Close owns their lifetime
// and stops them before the FIFO is closed; no test sleeps or network is used.
func holdPinHead(t *testing.T, g *gittest.Fixture) func() {
	t.Helper()
	dir := t.TempDir()
	gate, armed := filepath.Join(dir, "gate"), filepath.Join(dir, "armed")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	fifo, err := os.OpenFile(gate, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fifo.Close() })
	script := fmt.Sprintf("#!/bin/sh\nfor a do\n if [ \"$a\" = ls-remote ] && [ -f '%s' ]; then read _ < '%s'; fi\ndone\nexec '%s' \"$@\"\n", armed, gate, g.Binary)
	g.Binary = filepath.Join(dir, "git")
	if err := os.WriteFile(g.Binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.WriteFile(armed, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
