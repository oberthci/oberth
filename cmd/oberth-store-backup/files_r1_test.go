package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/store"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func requireReason(t *testing.T, err error, want recoveryRefusal) {
	t.Helper()
	public := recoveryDiagnostic(err)
	var reason *publicerr.Error
	if err == nil || !errors.As(public, &reason) || reason.Reason() != recoveryError(want, nil).Reason() {
		t.Fatal("wrong refusal", public)
	}
}

func TestExistingBackupIsNeverOverwritten(t *testing.T) {
	c := cryptoFixture(t)
	sentinel := []byte("existing encrypted backup")
	must(t, os.WriteFile(c.output, sentinel, 0600))
	var out, diagnostic bytes.Buffer
	if runCommand(t.Context(), configArgs(c), &out, &diagnostic) != 1 || !strings.Contains(diagnostic.String(), recoveryError(fileExists, nil).Reason()) {
		t.Fatal("existing backup has no conflict", diagnostic.String())
	}
	body, err := os.ReadFile(c.output)
	must(t, err)
	if !bytes.Equal(body, sentinel) || out.Len() != 0 {
		t.Fatal("existing backup was changed")
	}
}

func TestRecoveryFileChecksPrecedeParsing(t *testing.T) {
	for _, mode := range []string{"identity-symlink", "input-symlink", "identity-empty", "input-empty", "relative-database", "positional"} {
		t.Run(mode, func(t *testing.T) {
			c := cryptoFixture(t)
			if mode == "relative-database" {
				cwd, err := os.Getwd()
				must(t, err)
				c.database, err = filepath.Rel(cwd, c.database)
				must(t, err)
				requireReason(t, encrypt(t.Context(), c, io.Discard), bindingInvalid)
				return
			}
			if mode == "positional" {
				_, err := parse(append(configArgs(c), "extra"))
				requireReason(t, err, bindingInvalid)
				return
			}
			must(t, encrypt(t.Context(), c, io.Discard))
			c = recoveryConfig(c)
			path := c.input
			if strings.HasPrefix(mode, "identity-") {
				path = c.identity
			}
			if strings.HasSuffix(mode, "symlink") {
				link := path + ".link"
				must(t, os.Symlink(path, link))
				if path == c.input {
					c.input = link
				} else {
					c.identity = link
				}
			} else {
				must(t, os.Truncate(path, 0))
			}
			requireReason(t, decrypt(t.Context(), c, io.Discard), bindingInvalid)
		})
	}
}

func TestRestoreExistingTargetPrecedesDecryption(t *testing.T) {
	c := cryptoFixture(t)
	c = recoveryConfig(c)
	must(t, os.WriteFile(c.output, []byte("existing database"), 0600))
	// Missing encrypted input and key prove that no decryption setup is reached.
	c.identity = filepath.Join(filepath.Dir(c.input), "missing.identity")
	requireReason(t, decrypt(t.Context(), c, io.Discard), fileExists)
	body, err := os.ReadFile(c.output)
	must(t, err)
	if string(body) != "existing database" {
		t.Fatal("existing database changed")
	}
	assertNoStage(t, filepath.Dir(c.input))
}

func TestOutputSyncFailureIsNotSuccess(t *testing.T) {
	reader, writer, err := os.Pipe()
	must(t, err)
	defer func() { must(t, reader.Close()) }()
	if err := syncOutput(writer); !errors.Is(err, syscall.EINVAL) {
		t.Fatal("sync failure lost", err)
	}
}

func TestUnsupportedHelperSchemaRefusesBeforeBackup(t *testing.T) {
	c := cryptoFixture(t)
	var out, diagnostic bytes.Buffer
	args := append(configArgs(c), "--expect-schema", fmt.Sprint(store.Schema+1))
	if runCommand(t.Context(), args, &out, &diagnostic) != 1 || !strings.Contains(diagnostic.String(), recoveryError(bindingInvalid, nil).Reason()) {
		t.Fatal("unsupported helper schema reached backup", diagnostic.String())
	}
	if _, err := os.Stat(c.output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported schema created an archive", err)
	}
}
