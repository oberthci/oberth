package gitcache_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestPublicationWithoutForceRefusesNonFastForward(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Write(t, "next", []byte("next"))
	next := f.CommitFile(t, "next")
	f.Git(t, f.Work, "push", f.Bare, "main")
	req := publicationFor(f, "refs/heads/main", f.Commit, next)
	if _, err := c.Publish(t.Context(), req); err == nil {
		t.Fatal("non-fast-forward accepted without force")
	}
	if got := f.Git(t, f.Bare, "rev-parse", "main"); got != next {
		t.Fatal("upstream rewound")
	}
}
func TestPendingReceiveFencesFetchRemoveAndArchive(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	r := gateReceive(f)
	r.Admission.SHA = ""
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/topic", OldSHA: f.Commit})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, io.ErrClosedPipe }
	if err := c.Receive(t.Context(), r); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	operations := map[string]func() error{
		"fetch":  func() error { return c.ForceFetch(t.Context(), gittest.Repository) },
		"remove": func() error { return c.Remove(t.Context(), gittest.Repository) },
		"archive": func() error {
			body, err := c.Archive(t.Context(), gittest.Repository)
			if body != nil {
				_ = body.Close()
			}
			return err
		},
	}
	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			if err := op(); err == nil {
				t.Fatal("pending receive was ignored")
			}
		})
	}
}
func TestRepeatedOwnerCursorRefusesWithoutDeleting(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	calls := 0
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) {
		calls++
		return gitcache.OwnerPage{Next: "same"}, nil
	})
	if err := c.Keep(t.Context(), "run", f.Bindings(f.Commit)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := c.GC(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cursor cycle was not refused", err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); got == "" {
		t.Fatal("incomplete owner read deleted refs")
	}
}
func TestReadFileRefusesSymlinkAndTreatsPathspecLiterally(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	if err := os.Symlink("file.txt", filepath.Join(f.Work, "link")); err != nil {
		t.Fatal(err)
	}
	f.Write(t, ":(glob)*", []byte("literal"))
	next := f.CommitFile(t, "names")
	f.Git(t, f.Work, "push", "--force", f.Bare, "main")
	c := f.Cache(t, nil)
	revision := gitport.Revision{Repository: gittest.Repository, Commit: next}
	if _, err := c.ReadFile(t.Context(), revision, "link"); err == nil {
		t.Fatal("symlink read accepted")
	}
	file, err := c.ReadFile(t.Context(), revision, ":(glob)*")
	if err != nil || string(file.Bytes) != "literal" {
		t.Fatal("pathspec was interpreted", err)
	}
}
func TestBatchExport3000FilesHasBoundedWorkIsDeterministicAndReadOnly(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	f := gittest.New(t)
	blob := f.Git(t, f.Work, "rev-parse", f.Commit+":file.txt")
	var tree strings.Builder
	for i := range 3000 {
		mode := "100644"
		if i == 0 {
			mode = "100755"
		}
		fmt.Fprintf(&tree, "%s blob %s\tf%04d\n", mode, blob, i)
	}
	commit := commitTree(t, f, tree.String())
	f.Git(t, f.Work, "update-ref", "refs/heads/main", commit)
	f.Git(t, f.Work, "push", "--force", f.Bare, "main")
	f.Binary = reviewWrapper(t, f)
	c := f.Cache(t, nil)
	b := f.Binding()
	b.Revision.Commit = commit
	b.Export = sourcecontract.Export{}
	var previous []byte
	for range 2 {
		commands := filepath.Join(f.Root, "commands.jsonl")
		if err := os.WriteFile(commands, nil, 0600); err != nil {
			t.Fatal(err)
		}
		stage, err := c.Stage(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		calls, err := os.ReadFile(commands)
		if err != nil {
			t.Fatal(err)
		}
		if batches, processes := strings.Count(string(calls), `"cat-file", "--batch"`), bytes.Count(calls, []byte{'\n'}); batches != 1 || processes > 16 {
			t.Fatalf("3000-file export started %d blob batches and %d processes", batches, processes)
		}
		data, err := io.ReadAll(stage.Body)
		_ = stage.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		checkBatchArchive(t, data)
		if previous != nil && !bytes.Equal(previous, data) {
			t.Fatal("archive changed between identical exports")
		}
		previous = data
	}
}

func checkBatchArchive(t *testing.T, data []byte) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	archive := tar.NewReader(gz)
	count := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(header.Name, "f") {
			continue
		}
		mode := int64(0444)
		if header.Name == "f0000" {
			mode = 0555
		}
		body, err := io.ReadAll(archive)
		if err != nil || string(body) != "exact source\n" || header.Mode != mode {
			t.Fatal("batch content or mode changed", header.Name, header.Mode, err)
		}
		count++
	}
	if count != 3000 {
		t.Fatal("batch dropped files", count)
	}
}
