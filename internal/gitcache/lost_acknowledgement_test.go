package gitcache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestPublishReadsBackSuccessAfterLostAcknowledgement(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(f.Root, "git-lost-ack")
	// A symlink to the immutable test binary cannot inherit a writable
	// executable descriptor from a parallel fork during fixture construction.
	if err = os.Symlink(binary, wrapper); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.Root, "git-path"), []byte(f.Binary), 0600); err != nil {
		t.Fatal(err)
	}
	c := reviewNew(t, gitcache.Config{Root: filepath.Join(f.Root, "cache"), Scratch: filepath.Join(f.Root, "scratch"), Binary: wrapper, HookBinary: binary,
		Upstream: func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{URL: f.Bare, DefaultBranch: "refs/heads/main", RepositoryID: 1}, nil
		}, LiveOwners: func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil },
		Log: func(reason, detail string) { t.Logf("%s: %s", reason, detail) }})
	if err = c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "update-ref", "refs/heads/topic", f.Commit)
	if _, err = c.Publish(t.Context(), publicationFor(f, "refs/heads/topic", f.Commit, "")); err != nil {
		t.Fatal("readback lost confirmed result", err)
	}
	proof, err := os.ReadFile(filepath.Join(f.Root, "lost-ack-proof"))
	if err != nil || string(proof) != "push succeeded; acknowledgement lost\nread back refs/heads/topic\n" {
		t.Fatal("lost acknowledgement and following read-back were not exercised", string(proof), err)
	}
	if got := f.Git(t, f.Bare, "rev-parse", "refs/heads/topic"); got != f.Commit {
		t.Fatal("forge did not retain the published object", got)
	}
}

// The native push exits before the helper deliberately loses its reply. Every
// other invocation replaces this process with the fixture's selected Git.
func lostAcknowledgementGit() int {
	root := filepath.Dir(os.Args[0])
	path, err := os.ReadFile(filepath.Join(root, "git-path"))
	if err != nil {
		return 2
	}
	proof := filepath.Join(root, "lost-ack-proof")
	if !slices.Contains(os.Args[1:], "push") {
		if slices.Contains(os.Args[1:], "ls-remote") && slices.Contains(os.Args[1:], "refs/heads/topic") {
			if _, err = os.Stat(proof); err == nil {
				if err = appendLostAckProof(proof, "read back refs/heads/topic\n"); err != nil {
					return 2
				}
			}
		}
		if err = syscall.Exec(string(path), append([]string{string(path)}, os.Args[1:]...), os.Environ()); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
		}
		return 2
	}
	cmd := exec.Command(string(path), os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err = cmd.Run(); err != nil {
		return 2
	}
	if err = appendLostAckProof(proof, "push succeeded; acknowledgement lost\n"); err != nil {
		return 2
	}
	return 1
}

func appendLostAckProof(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = strings.NewReader(line).WriteTo(f)
	return errors.Join(err, f.Close())
}
