package gitcache_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func commitTree(t *testing.T, f *gittest.Fixture, records string) string {
	t.Helper()
	cmd := exec.Command(f.Binary, "mktree")
	cmd.Dir = f.Work
	cmd.Stdin = strings.NewReader(records)
	data, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return f.Git(t, f.Work, "commit-tree", strings.TrimSpace(string(data)), "-m", "bound")
}
func pinBinding(f *gittest.Fixture, commit string) sourcecontract.Binding {
	b := f.Binding()
	b.Descriptor = sourcecontract.Descriptor{
		Version:    1,
		Kind:       sourcecontract.Pinned,
		Repository: gittest.Repository,
		Commit:     commit,
	}
	b.Revision.Commit = commit
	b.Export = sourcecontract.Export{}
	b.Path = "/work/sources/pin"
	b.AuthorizationDigest = strings.Repeat("a", 64)
	return b
}
func TestPinFileCountAccepts4096Refuses4097(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	blob := f.Git(t, f.Work, "rev-parse", f.Commit+":file.txt")
	var tree strings.Builder
	for i := range gitcache.PinFiles {
		_, _ = fmt.Fprintf(&tree, "100644 blob %s\tf%04d\n", blob, i)
	}
	commit := commitTree(t, f, tree.String())
	f.Git(t, f.Store(), "fetch", f.Work, commit)
	stage, err := c.Stage(t.Context(), pinBinding(f, commit))
	if err != nil {
		t.Fatal(err)
	}
	files := readArchive(t, stage.Body)
	if len(files) != gitcache.PinFiles {
		t.Fatal(len(files))
	}
	_, _ = fmt.Fprintf(&tree, "100644 blob %s\tf4096\n", blob)
	commit = commitTree(t, f, tree.String())
	f.Git(t, f.Store(), "fetch", f.Work, commit)
	_, err = c.Stage(t.Context(), pinBinding(f, commit))
	var failure *sourcedelivery.Failure
	if !errors.As(err, &failure) || failure.Code != "too_large" ||
		failure.Limit != gitcache.PinFiles || failure.Unit != "files" {
		t.Fatal("4097 files did not name the file limit", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestPinPayloadAccepts128MiBRefusesOneMoreByte(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	for _, size := range []int64{gitcache.PinBytes, gitcache.PinBytes + 1} {
		cmd := exec.Command(f.Binary, "hash-object", "-w", "--stdin")
		cmd.Dir = f.Work
		cmd.Stdin = io.LimitReader(zeros{}, size)
		blob, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		commit := commitTree(t, f, "100644 blob "+strings.TrimSpace(string(blob))+"\tpayload\n")
		f.Git(t, f.Store(), "fetch", f.Work, commit)
		stage, err := c.Stage(t.Context(), pinBinding(f, commit))
		if size > gitcache.PinBytes {
			if err == nil {
				_ = stage.Body.Close()
				t.Fatal("N+1 accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if stage.Bytes != size {
			t.Fatalf("payload bytes %d", stage.Bytes)
		}
		if err = stage.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestReadPathAccepts4096BytesRefuses4097(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	blob := f.Git(t, f.Work, "rev-parse", f.Commit+":file.txt")
	path := strings.Repeat("a", 4096)
	commit := commitTree(t, f, "100644 blob "+blob+"\t"+path+"\n")
	f.Git(t, f.Store(), "fetch", f.Work, commit)
	rev := sourcecontract.Revision{Repository: gittest.Repository, Commit: commit}
	if _, err := c.ReadFile(t.Context(), rev, path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadFile(t.Context(), rev, path+"a"); err == nil {
		t.Fatal("4097-byte path accepted")
	}
}
func TestReceiveUpdateCountAccepts256Refuses257(t *testing.T) {
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	for _, n := range []int{gitcache.MaxUpdates, gitcache.MaxUpdates + 1} {
		var body bytes.Buffer
		for i := range n {
			line := strings.Repeat("0", 40) + " " + f.Commit + fmt.Sprintf(" refs/heads/set%d/f%d", n, i)
			if i == 0 {
				line += "\x00report-status"
			}
			line += "\n"
			_, _ = fmt.Fprintf(&body, "%04x%s", len(line)+4, line)
		}
		_, _ = body.WriteString("0000")
		cmd := exec.Command(f.Binary, "pack-objects", "--stdout", "--revs")
		cmd.Dir = f.Work
		cmd.Stdin = strings.NewReader(f.Commit + "\n")
		pack, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = body.Write(pack)
		delivered := 0
		err = c.Receive(t.Context(), gitcache.ReceiveRequest{
			AllowDefault: true,
			Repository:   gittest.Repository,
			Actor:        "agent",
			Fingerprint:  "key",
			Admission:    gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
			Input:        io.NopCloser(bytes.NewReader(body.Bytes())),
			Output:       io.Discard,
			Deliver: func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
				delivered = len(event.Updates)
				return nil, nil
			},
		})
		if n == gitcache.MaxUpdates {
			if err != nil || delivered != n {
				t.Fatalf("N: %d %v", delivered, err)
			}
		} else if err == nil || delivered != 0 {
			t.Fatalf("N+1: %d %v", delivered, err)
		}
		if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads/set257/"); n > gitcache.MaxUpdates &&
			got != "" {
			t.Fatal("refused receive changed refs", got)
		}

	}
}

func TestPublicRefCountAccepts4096Refuses4097(t *testing.T) {
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	var tx strings.Builder
	for i := range sourcecontract.MaxRefs - 2 {
		_, _ = fmt.Fprintf(&tx, "update refs/heads/b%04d %s\n", i, f.Commit)
	}
	cmd := exec.Command(f.Binary, "update-ref", "--stdin")
	cmd.Dir = f.Store()
	cmd.Stdin = strings.NewReader(tx.String())
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"last", "overflow"} {
		delivered := 0
		err := c.Receive(t.Context(), gitcache.ReceiveRequest{
			Repository: gittest.Repository, Actor: "agent", Fingerprint: "key",
			Admission: gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
			Input:     receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/" + name, NewSHA: f.Commit}),
			Output:    io.Discard,
			Deliver: func(_ context.Context, e gitcache.ReceiveEvent) ([]string, error) {
				delivered = len(e.Updates)
				return nil, nil
			}})
		if name == "last" {
			if err != nil || delivered != 1 {
				t.Fatalf("N: %d %v", delivered, err)
			}
		} else if err == nil || delivered != 0 {
			t.Fatalf("N+1: %d %v", delivered, err)
		}
	}
	refs, err := c.ListRefs(t.Context(), gittest.Repository)
	if err != nil || len(refs) != sourcecontract.MaxRefs {
		t.Fatalf("inventory %d: %v", len(refs), err)
	}
}

func TestReceiveRefSuffixAccepts255Refuses256(t *testing.T) {
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	for _, size := range []int{255, 256} {
		suffix := strings.Repeat("a/", 100) + strings.Repeat("b", size-200)
		err := c.Receive(
			t.Context(),
			gitcache.ReceiveRequest{
				Repository:  gittest.Repository,
				Actor:       "agent",
				Fingerprint: "key",
				Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
				Input:       receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/" + suffix, NewSHA: f.Commit}),
				Output:      io.Discard,
				Deliver: func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
					return nil, nil
				},
			},
		)
		if (err == nil) != (size == 255) {
			t.Fatalf("suffix %d: %v", size, err)
		}
	}
}
func TestReceiveMetadataAccepts8MiBRefusesOneMoreByte(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	record := gitcache.Reservation{
		State:  "reserved",
		Before: map[string]string{"refs/heads/main": f.Commit},
		Event: gitcache.ReceiveEvent{
			ID:          strings.Repeat("a", 32),
			Actor:       "a",
			Fingerprint: "key",
			Repository:  gittest.Repository,
			Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
		},
	}
	base, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{8 << 20, (8 << 20) + 1} {
		record.Event.Actor = strings.Repeat("a", size-len(base)+1)
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != size {
			t.Fatal("wrong fixture size")
		}
		if err = os.WriteFile(filepath.Join(f.Store(), "oberth-receive.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = c.PendingReceive(t.Context(), gittest.Repository)
		if (err == nil) != (size == 8<<20) {
			t.Fatalf("metadata %d: %v", size, err)
		}
	}
}
