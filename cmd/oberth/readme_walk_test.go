//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/restartretry"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"golang.org/x/crypto/ssh"
)

type readmeWalk struct {
	f                                *composedFixture
	app                              application
	out, diagnostic                  bytes.Buffer
	commit, invitation, runID, known string
}

func TestReadmeFirstDaySequenceRegistersVerifiesJoinsAndPushes(t *testing.T) {
	t.Parallel()
	newReadmeWalk(t).run(t, readmeFirstDayCommands(t))
}

// newReadmeWalk prepares an installation that README commands run against.
func newReadmeWalk(t *testing.T) *readmeWalk {
	t.Helper()
	f := composeFixture(t, composeBinary(t))
	freshRepoAddCache(f)
	w := &readmeWalk{f: f, commit: f.prepareCommit(t)}
	f.config.Public.URL = "https://127.0.0.1"
	port := reserveInstalledSSH(t, f)
	f.config.Public.SSHPort = port
	w.app = application{base: t.TempDir(), directory: f.git.Work, in: strings.NewReader(""), out: &w.out, diagnostic: &w.diagnostic, env: func(string) string { return "" }, executable: func() (string, error) { return f.binary, nil }}
	w.app.install = func(ctx context.Context, request installationRequest, _ io.Reader, _ io.Writer) error {
		if request.name != "install" || request.text("context") != "installed" || request.enabled("dry-run") {
			t.Fatal("README install did not apply the selected fixture")
		}
		w.known = w.installFixture(ctx, t, port)
		return nil
	}
	return w
}

func (w *readmeWalk) run(t *testing.T, commands []string) {
	t.Helper()
	if len(commands) != 8 {
		t.Fatal("README first-day count changed", commands)
	}
	w.runCommands(t, commands)
}

// runCommands runs README commands in order, from the first one.
func (w *readmeWalk) runCommands(t *testing.T, commands []string) {
	t.Helper()
	for index, command := range commands {
		replace := strings.NewReplacer("<context>", "installed", "<known-hosts-file>", w.known, "<forge-url>", "ssh://"+w.f.forge.address+"/team/widget.git", "<fingerprint>", ssh.FingerprintSHA256(w.f.forge.key), "<upstream>", "127-0-0-1", "<invitation-file>", w.invitation, "<run>", w.runID)
		args := strings.Fields(replace.Replace(command))
		if args[0] == "git" {
			w.push(t, args[1:])
			continue
		}
		w.out.Reset()
		w.diagnostic.Reset()
		code := w.app.run(t.Context(), args[1:])
		if index == 1 {
			if code != 1 || !strings.Contains(w.diagnostic.String(), ssh.FingerprintSHA256(w.f.forge.key)) {
				t.Fatal("README initial registration did not show offered fingerprint", code, w.diagnostic.String())
			}
		} else if code != 0 {
			t.Fatal("README command failed", command, code, w.diagnostic.String())
		}
		if index == 0 {
			w.start(t)
		}
		if args[1] == "invite" {
			w.readInvitation(t)
		}
		if args[1] == "log" {
			w.assertLogOutput(t)
		}
	}
}

func (w *readmeWalk) assertLogOutput(t *testing.T) {
	t.Helper()
	if !strings.Contains(w.out.String(), "fake logs\n") || strings.Contains(w.out.String(), "view output") {
		t.Fatal("README log did not select native authored output", w.out.String())
	}
}

func (w *readmeWalk) readInvitation(t *testing.T) {
	t.Helper()
	lines := strings.Split(w.out.String(), "\n")
	for i, line := range lines[:len(lines)-1] {
		if line == "invitation:" {
			w.invitation = lines[i+1]
		}
	}
	if w.invitation == "" {
		t.Fatal("README invitation did not name its file")
	}
}

func (w *readmeWalk) push(t *testing.T, args []string) {
	t.Helper()
	f := w.f
	f.git.Git(t, f.git.Work, args...)
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if run.CommitSHA == nil || *run.CommitSHA != w.commit || run.Origin.Actor == "install-admin" {
		t.Fatal("README push did not use the joined identity and exact commit")
	}
	w.runID = run.ID
	f.deliverSource(t, run)
	f.captureViewLog(t, run.ID)
	f.finishNative(t, run)
	f.waitRun(t, func(r stateport.Run) bool { return r.ID == run.ID && r.Status == stepmodel.RunPassed })
	w.waitLogCapture(t)
}

func (w *readmeWalk) waitLogCapture(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		entries, err := w.f.owners.c.logs.Containers(ctx, w.runID)
		if err != nil {
			t.Fatal("README native log inventory was unavailable", err)
		}
		for _, entry := range entries {
			if entry.Main && entry.State == runlog.Captured {
				return
			}
		}
		if err := restartretry.Wait(ctx, 10*time.Millisecond); err != nil {
			t.Fatal("README native log capture did not complete", entries, err)
		}
	}
}

func (w *readmeWalk) installFixture(ctx context.Context, t *testing.T, port int) string {
	f, app := w.f, &w.app
	t.Helper()
	profile := installComposedProfile(ctx, t, f, port)
	var err error
	f.adminCertificate, err = tls.LoadX509KeyPair(profile.AdminCert, profile.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	app.base = filepath.Dir(filepath.Dir(profile.AdminKey))
	if err = os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	selection, err := client.PrepareProfileSelection(app.base, filepath.Base(filepath.Dir(profile.AdminKey)))
	if err != nil {
		t.Fatal(err)
	}
	defer selection.Close()
	if err = selection.Commit(); err != nil {
		t.Fatal(err)
	}
	return profile.KnownHosts
}

func (w *readmeWalk) start(t *testing.T) {
	t.Helper()
	f := w.f
	close(f.allowList)
	_, done := f.run(t)
	f.waitStarted(t, done)
	w.app.config = client.Config{Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport}
}
