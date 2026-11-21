package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	"golang.org/x/crypto/ssh"
)

func TestCompositionR1ProjectedForgeRotation(t *testing.T) {
	dir := t.TempDir()
	first, key1 := composeSSHKey(t, dir, "first")
	second, key2 := composeSSHKey(t, dir, "second")
	for _, file := range []string{first, second} {
		if err := os.Chmod(file, 0440); err != nil {
			t.Fatal(err)
		}
	}
	projection := filepath.Join(dir, "key")
	if err := os.Symlink(first, projection); err != nil {
		t.Fatal(err)
	}
	owner := &serverOwners{c: &composition{}}
	owner.c.config.DatabaseFile = filepath.Join(t.TempDir(), "state.db")
	owner.c.config.Files.ForgeKey = projection
	for _, key := range []ssh.PublicKey{key1, key2} {
		identity, err := (serverAdministration{owner}).PublicIdentity(t.Context(), "shared")
		if err != nil || identity.PublicKey != string(ssh.MarshalAuthorizedKey(key)) {
			t.Fatal("projected forge identity refused or stale", err)
		}
		if err = os.Remove(projection); err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(second, projection); err != nil {
			t.Fatal(err)
		}
	}
}

func compositionRegressionFixture(t *testing.T) *serverFixture {
	t.Helper()
	f := fixtureServer(t)
	f.config.Files.SSHHostKey, _ = composeSSHKey(t, t.TempDir(), "host")
	f.config.Files.ForgeKey, _ = composeSSHKey(t, t.TempDir(), "forge")
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"git_binary": binary, "source_timeout_seconds": 60, "admission_timeout_seconds": 30, "source_client_burst": 96, "default_branch": "refs/heads/main", "issue_stale_after_seconds": 86400, "issue_active_stale_after_seconds": 7200})
	f.config.Runtime = raw
	return f
}

func TestCompositionR1RejectsBrokenStartupTargets(t *testing.T) {
	for _, field := range []string{"files.forge_key", "runtime.git_binary"} {
		for _, shape := range []string{"missing", "empty", "directory", "dangling", "invalid"} {
			t.Run(field+"/"+shape, func(t *testing.T) {
				f := compositionRegressionFixture(t)
				path := brokenStartupTarget(t, shape)
				if field == "files.forge_key" {
					f.config.Files.ForgeKey = path
				} else {
					var runtime map[string]any
					if err := json.Unmarshal(f.config.Runtime, &runtime); err != nil {
						t.Fatal(err)
					}
					runtime["git_binary"] = path
					f.config.Runtime, _ = json.Marshal(runtime)
				}
				if err := os.Remove(f.config.DatabaseFile); err != nil {
					t.Fatal(err)
				}
				assertStartupRefusalBeforeStorage(t, f, field)
			})
		}
	}
}
func brokenStartupTarget(t *testing.T, shape string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target")
	var err error
	switch shape {
	case "empty":
		err = os.WriteFile(path, nil, 0755)
	case "directory":
		err = os.Mkdir(path, 0755)
	case "dangling":
		err = os.Symlink(path+"-missing", path)
	case "invalid":
		err = os.WriteFile(path, []byte("not-a-key-or-executable"), 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func assertStartupRefusalBeforeStorage(t *testing.T, f *serverFixture, field string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, f.config, f.deps, f.trace) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error()+compositionRefusalDetail(err), field) {
			t.Error("startup refusal lost field", field, err)
		}
	case <-f.trace.ready:
		cancel()
		<-done
		t.Error("broken startup input announced ready", field)
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("startup did not finish")
	}
	if len(f.trace.snapshot()) != 0 {
		t.Error("startup bound listener before validating", field, f.trace.snapshot())
	}
	if _, err := os.Stat(f.config.DatabaseFile); !os.IsNotExist(err) {
		t.Error("startup opened storage before validating", field)
	}
}
func compositionRefusalDetail(err error) string {
	var detail *publicerr.Error
	if errors.As(err, &detail) {
		return detail.Detail
	}
	return ""
}

func TestCompositionR1ConstructorReceivesSignal(t *testing.T) {
	f := compositionRegressionFixture(t)
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	f.deps.constructors.CacheNew = func(ctx context.Context, _ *composition) (Part, error) {
		entered <- ctx
		select {
		case <-ctx.Done():
			return Part{}, ctx.Err()
		case <-release:
			return Part{}, errors.New("released constructor")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, f.config, f.deps, f.trace) }()
	active := <-entered
	cancel()
	select {
	case <-active.Done():
	case <-time.After(time.Second):
		t.Error("active constructor missed first signal")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("constructor did not join")
	}
}

func TestCompositionR1UnjoinedWorkerKeepsDependencies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, closed := make(chan struct{}), make(chan struct{})
		listenerClosed := make(chan struct{})
		life := newLifecycle(t.Context(), []Part{{Name: "store", Close: func(context.Context) error { close(closed); return nil }}, {Name: "listener", Stop: func() { close(listenerClosed) }}, {Name: "worker", Run: func(context.Context) error { <-release; return nil }}})
		if err := life.start(); err != nil {
			t.Fatal(err)
		}
		life.timeout = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
			expired, cancel := context.WithCancel(ctx)
			cancel()
			return expired, cancel
		}
		if err := life.stop(t.Context()); err == nil {
			t.Error("unjoined worker was not reported")
		}
		synctest.Wait()
		select {
		case <-closed:
			t.Error("dependency closed beneath live worker")
		default:
		}
		select {
		case <-listenerClosed:
		default:
			t.Error("independent listener left open")
		}
		close(release)
		synctest.Wait()
	})
}
func TestCompositionR1ListenPortsAreExplicit(t *testing.T) {
	for _, field := range []string{"https", "ssh", "source", "health"} {
		for _, address := range []string{"127.0.0.1:", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:service"} {
			raw := configCase(t, func(c map[string]any) { c["listen"].(map[string]any)[field] = address })
			if _, err := decodeConfig(raw); err == nil {
				t.Error("invalid listen port accepted", field, address)
			}
		}
	}
}
