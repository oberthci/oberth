package installer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestBackupClosesFileBeforeLink(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"success", "source failure", "close failure"} {
		t.Run(kind, func(t *testing.T) { checkClosedBeforeBackupLink(t, kind) })
	}
}

func checkClosedBeforeBackupLink(t *testing.T, kind string) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stage-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	final := filepath.Join(filepath.Dir(file.Name()), "published.age")
	var prior error
	if kind == "source failure" {
		prior = errors.New("snapshot refused")
	}
	if kind == "close failure" {
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	called := false
	err = closeBackupBeforePublish(file, prior, final, func() error {
		called = true
		if _, statErr := file.Stat(); !errors.Is(statErr, os.ErrClosed) {
			t.Error("publication ran with an open file", statErr)
		}
		return os.Link(file.Name(), final)
	})
	if kind == "success" {
		if err != nil || !called {
			t.Fatal("closed file was not published", err)
		}
		if _, err = os.Stat(final); err != nil {
			t.Fatal(err)
		}
		return
	}
	assertBackupPublicationStopped(t, err, prior, called)
	if _, err = os.Stat(final); !os.IsNotExist(err) {
		t.Fatal("failed close published a backup", err)
	}
}

func assertBackupPublicationStopped(t *testing.T, err, prior error, called bool) {
	t.Helper()
	if err == nil || called {
		t.Fatal("failed source/close reached publication", err, called)
	}
	if prior != nil && !errors.Is(err, prior) {
		t.Fatal("source cause was lost", err)
	}
	if prior == nil && !errors.Is(err, os.ErrClosed) {
		t.Fatal("close cause was lost", err)
	}
}

func TestBackupPublishedFailureClassification(t *testing.T) {
	t.Parallel()
	cause := &os.PathError{Op: "sync", Path: ".oberth-backup-incomplete-gone", Err: syscall.EIO}
	for _, err := range []error{backupDirectorySyncFailure(cause, "published.age"), backupFailure("backup-cleanup", cause, "published.age")} {
		var problem *Problem
		if !errors.As(err, &problem) || !errors.Is(err, cause) {
			t.Fatal("publication warning lost its cause", err)
		}
		if !strings.HasPrefix(problem.Reason, "Backup is published") || !strings.Contains(problem.Remedy, "Keep the backup") || strings.Contains(problem.Remedy, "new backup path") {
			t.Fatal("published backup was presented as a failed backup", err)
		}
		if !strings.Contains(problem.Detail, "published.age") || strings.Contains(problem.Detail, ".oberth-backup-incomplete-") {
			t.Fatal("publication warning lost final path or exposed staging name", err)
		}
	}
	if backupDirectorySyncFailure(nil, "published.age") != nil {
		t.Fatal("successful directory sync produced a warning")
	}
}
