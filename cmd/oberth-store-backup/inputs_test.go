package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/store"
)

func configArgs(c config) []string {
	args := []string{"--database", c.database, "--output", c.output, "--recipient", c.recipient, "--input", c.input, "--identity", c.identity, "--schema-identity", c.options.Protocol.SchemaIdentity, "--audit-domain", c.options.Protocol.AuditDomain, "--expect-schema", fmt.Sprint(c.options.Schema), "--max-database-bytes", fmt.Sprint(c.options.MaxBytes)}
	if c.mode != "backup" {
		args = append([]string{c.mode}, args...)
	}
	return args
}

func digestFile(t *testing.T, path string) string {
	t.Helper()
	digest, err := fileDigest(t.Context(), path, nil)
	must(t, err)
	return digest
}

func TestBackupInputsAndDiagnosticPrivacy(t *testing.T) {
	c := cryptoFixture(t)
	for _, args := range [][]string{nil, {"extra"}, {"--expect-schema", fmt.Sprint(store.Schema + 1)}, {"--max-database-bytes", "-1"}, {"--unknown"}} {
		if _, err := parse(args); err == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
	if cleanPath("file:state?mode=rw") || cleanPath("relative") {
		t.Fatal("ambiguous path accepted")
	}
	c.recipient = "invalid"
	var out, diagnostic bytes.Buffer
	if runCommand(t.Context(), configArgs(c), &out, &diagnostic) != 1 || !strings.Contains(diagnostic.String(), recoveryError(recipientInvalid, nil).Reason()) {
		t.Fatal("recipient failure lost", diagnostic.String())
	}
	key, err := os.ReadFile(c.identity)
	must(t, err)
	c.mode = "restore"
	c.input = c.database
	must(t, os.Chmod(c.identity, 0644))
	diagnostic.Reset()
	if runCommand(t.Context(), configArgs(c), &out, &diagnostic) != 1 || !strings.Contains(diagnostic.String(), "permissions") {
		t.Fatal("identity mode failure lost", diagnostic.String())
	}
	if strings.Contains(diagnostic.String(), string(key)) || strings.Contains(diagnostic.String(), c.identity) {
		t.Fatal("identity exposed in a diagnostic")
	}
	must(t, os.Chmod(filepath.Dir(c.output), 0755))
	if file, err := exclusiveOutput(c.output); err == nil {
		must(t, file.Close())
		t.Fatal("public output directory accepted")
	}
}

func TestConfiguredDatabaseLimitKeepsActualCause(t *testing.T) {
	c := cryptoFixture(t)
	c.options.MaxBytes = 1
	var out, diagnostic bytes.Buffer
	if runCommand(t.Context(), configArgs(c), &out, &diagnostic) != 1 || (!strings.Contains(diagnostic.String(), "limit 1 bytes") || !strings.Contains(diagnostic.String(), recoveryError(configuredTooLarge, nil).Reason())) {
		t.Fatal("selected limit lost", diagnostic.String())
	}
	if strings.Contains(diagnostic.String(), "256 MiB") || !strings.Contains(diagnostic.String(), "4 GiB") {
		t.Fatal("obsolete helper ceiling", diagnostic.String())
	}
	if out.Len() != 0 {
		t.Fatal("failed backup emitted receipt")
	}
	if _, err := os.Stat(c.output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed backup kept output", err)
	}
	cause := &store.SizeLimitError{Size: 2, Limit: 1}
	err := backupError(publicerr.New(publicerr.InvalidInput, "A size refusal.", "Select a smaller database.", cause))
	if !errors.Is(err, cause) {
		t.Fatal("size cause lost", err)
	}
}

func TestStagingDiskFullKeepsItsCauseAndSafeDiagnostic(t *testing.T) {
	c := cryptoFixture(t)
	must(t, encrypt(t.Context(), c, io.Discard))
	c = recoveryConfig(c)
	key, err := readIdentity(c.identity)
	must(t, err)
	for _, cause := range []error{syscall.ENOSPC, syscall.EDQUOT} {
		failure := &os.PathError{Op: "write", Path: c.identity, Err: cause}
		err := authenticate(t.Context(), c, key, failedWriter{failure})
		if !errors.Is(err, cause) {
			t.Fatal("disk cause lost", err)
		}
		diagnostic := recoveryDiagnostic(err).Error()
		if !strings.Contains(diagnostic, recoveryError(volumeFull, nil).Reason()) || strings.Contains(diagnostic, c.identity) {
			t.Fatal("unsafe disk diagnostic", diagnostic)
		}
	}
}

func TestEveryRecoveryMessageIsCompleteAndBounded(t *testing.T) {
	for kind := bindingInvalid; kind <= recoveryIO; kind++ {
		reason, remedy := recoveryMessage(kind)
		must(t, publicerr.CheckMessage(reason, 80))
		must(t, publicerr.CheckMessage(remedy, 120))
	}
}
