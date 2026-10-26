//go:build linux && backup_synthetic

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func cmsRun(t *testing.T, args ...string) {
	t.Helper()
	c := exec.Command(opensslPath, args...)
	c.Env = []string{"PATH=/usr/bin:/bin", "OPENSSL_CONF=/dev/null"}
	c.Stdout = io.Discard
	c.Stderr = io.Discard
	if e := c.Run(); e != nil {
		t.Fatal("synthetic CMS setup failed")
	}
}
func cmsRecipient(t *testing.T, dir, prefix string) (string, string) {
	t.Helper()
	key := filepath.Join(dir, prefix+".key")
	cert := filepath.Join(dir, prefix+".pem")
	cmsRun(t, "genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:3072", "-out", key)
	if e := os.Chmod(key, 0600); e != nil {
		t.Fatal(e)
	}
	cmsRun(t, "req", "-new", "-x509", "-key", key, "-out", cert, "-days", "1", "-subj", "/CN=synthetic-backup-recipient")
	return key, cert
}
func TestEncryptedBackupActualCMS(t *testing.T) {
	if os.Getenv("OBERTH_BACKUP_SYNTHETIC") != "1" {
		t.Fatal("requires separately reviewed no-swap private tmpfs sandbox")
	}
	if err := requireSandbox(); err != nil {
		t.Fatal("synthetic sandbox refused before recipient generation")
	}
	ctx := context.Background()
	dir, e := os.MkdirTemp("/tmp", "backup-synthetic-")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	key, cert := cmsRecipient(t, dir, "recipient")
	wrongKey, _ := cmsRecipient(t, dir, "wrong")
	database := filepath.Join(dir, "source.sqlite")
	db, e := store.CreateGenesis(ctx, database, store.Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = db.Close() }()
	raw, e := sql.Open("sqlite", database)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = raw.Close() }()
	if _, e := raw.Exec("ALTER TABLE schedule_fires DROP COLUMN last_fired_at; DELETE FROM schema_migrations WHERE version=32; DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31; DROP TRIGGER run_timing_claim; DROP TRIGGER run_timing_grant; DROP TRIGGER run_timing_park; DROP TRIGGER run_timing_terminal; DROP TABLE step_timings; DROP TABLE run_admission_timings; DROP TABLE run_timings; DROP INDEX runs_admission_idx; ALTER TABLE runs DROP COLUMN admission_sequence; DROP TABLE kubevirt_grants; DROP TABLE uplink_scopes; ALTER TABLE issues DROP COLUMN scope_upstream_id; ALTER TABLE step_results DROP COLUMN termination_json; DELETE FROM schema_migrations WHERE version>=21; PRAGMA wal_autocheckpoint=0"); e != nil {
		t.Fatal(e)
	}
	if _, e := db.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "synthetic", Action: "backup", ResourceType: "fixture", ResourceID: "DATABASE_SECRET_CANARY_NEVER_PUBLIC"}); e != nil {
		t.Fatal(e)
	}
	toolHash, e := digest(opensslPath)
	if e != nil {
		t.Fatal(e)
	}
	certHash, e := digest(cert)
	if e != nil {
		t.Fatal(e)
	}
	c := config{database: database, recipient: cert, output: filepath.Join(dir, "backup.cms"), opensslSHA: toolHash, recipientSHA: certHash, schemaIdentity: "oberth-schema-v1", auditDomain: "oberth-audit-v1", schema: 20}
	sourceBefore := map[string]string{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		h, e := digest(database + suffix)
		if e != nil {
			t.Fatal(e)
		}
		sourceBefore[suffix] = h
	}
	var public bytes.Buffer
	if e := backup(ctx, c, &public); e != nil {
		t.Fatalf("synthetic production backup failed: %T", e)
	}
	if bytes.Contains(public.Bytes(), []byte("DATABASE_SECRET_CANARY")) {
		t.Fatal("public receipt leaked rows")
	}
	var receipt map[string]any
	if e := json.Unmarshal(public.Bytes(), &receipt); e != nil || receipt["schema"] != float64(20) {
		t.Fatal("public receipt refused")
	}
	cipherHash, err := digest(c.output)
	if err != nil {
		t.Fatal(err)
	}
	var verified bytes.Buffer
	if err := runVerify([]string{"--input", c.output, "--private-key", key, "--ciphertext-sha256", cipherHash, "--openssl-sha256", toolHash, "--schema-identity", c.schemaIdentity, "--audit-domain", c.auditDomain, "--expect-schema", "20"}, &verified); err != nil {
		t.Fatalf("private production rehearsal failed: %T", err)
	}
	if bytes.Contains(verified.Bytes(), []byte("DATABASE_SECRET_CANARY")) {
		t.Fatal("private rehearsal leaked rows")
	}
	encrypted, e := os.ReadFile(c.output)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(encrypted, []byte("DATABASE_SECRET_CANARY")) {
		t.Fatal("ciphertext plaintext canary")
	}
	if info, e := os.Stat(c.output); e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("ciphertext permissions")
	}
	// Reusing an occupied output must not overwrite or remove it.
	if e := backup(ctx, c, &bytes.Buffer{}); e == nil {
		t.Fatal("occupied output accepted")
	}
	again, e := os.ReadFile(c.output)
	if e != nil || !bytes.Equal(again, encrypted) {
		t.Fatal("occupied output changed")
	}
	t.Run("deadline-joined", func(t *testing.T) {
		deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		child := cmsEncryptCommand(deadline, cert)
		child.Stdout = io.Discard
		pipe, e := child.StdinPipe()
		if e != nil {
			t.Fatal(e)
		}
		if e := child.Start(); e != nil {
			t.Fatal(e)
		}
		pidfd, e := unix.PidfdOpen(child.Process.Pid, 0)
		if e != nil {
			_ = pipe.Close()
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatal(e)
		}
		defer unix.Close(pidfd)
		waitErr := child.Wait()
		_ = pipe.Close()
		if waitErr == nil || !errors.Is(deadline.Err(), context.DeadlineExceeded) {
			t.Fatal("real blocked CMS deadline not enforced")
		}
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		n, e := unix.Poll(fds, 0)
		if e != nil || n != 1 || fds[0].Revents&unix.POLLIN == 0 {
			t.Fatal("CMS process join not confirmed")
		}
	})
	t.Run("cipher-write-refused", func(t *testing.T) {
		full, e := os.OpenFile("/dev/full", os.O_WRONLY, 0)
		if e != nil {
			t.Fatal(e)
		}
		defer full.Close()
		// Retain the actual pinned tool characterization that exposed the gap:
		// direct stdout FD returned success despite /dev/full. This is never an
		// accepted production result; the shared production writer below must
		// propagate the real ENOSPC through the joined os/exec copy.
		direct := cmsEncryptCommand(ctx, cert)
		direct.Stdin = bytes.NewReader([]byte("synthetic-only"))
		direct.Stdout = full
		if e := direct.Run(); e != nil {
			t.Fatal("pinned direct-FD characterization changed")
		}
		child := cmsEncryptCommand(ctx, cert)
		child.Stdin = bytes.NewReader([]byte("synthetic-only"))
		child.Stdout = cmsCipherOutput(ctx, full)
		if e := child.Run(); !errors.Is(e, syscall.ENOSPC) {
			t.Fatal("production ciphertext writer did not report ENOSPC")
		}
	})
	for suffix, want := range sourceBefore {
		got, e := digest(database + suffix)
		if e != nil || got != want {
			t.Fatal("source DB/WAL/SHM changed")
		}
	}
	restored := filepath.Join(dir, "verified.tar")
	cmsRun(t, "cms", "-decrypt", "-binary", "-inform", "DER", "-in", c.output, "-inkey", key, "-out", restored)
	listing := exec.Command("/usr/bin/tar", "-tf", restored)
	names, e := listing.Output()
	if e != nil || string(names) != "oberth.sqlite\noberth.sqlite-wal\nmanifest.json\n" {
		t.Fatal("independent tar inventory refused")
	}
	archive, e := os.Open(restored)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = archive.Close() }()
	reader := tar.NewReader(archive)
	restore := filepath.Join(dir, "restore")
	if e := os.Mkdir(restore, 0700); e != nil {
		t.Fatal(e)
	}
	for {
		h, e := reader.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if h.Name == "manifest.json" {
			continue
		}
		if h.Name != "oberth.sqlite" && h.Name != "oberth.sqlite-wal" {
			t.Fatal("archive name")
		}
		f, e := os.OpenFile(filepath.Join(restore, h.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := io.Copy(f, reader); e != nil {
			t.Fatal(e)
		}
		if e := f.Close(); e != nil {
			t.Fatal(e)
		}
	}
	inspection, e := store.InspectCurrent(ctx, filepath.Join(restore, "oberth.sqlite"), store.Options{})
	if e != nil {
		t.Fatal(e)
	}
	head, e := inspection.VerifyAuditState(ctx)
	if e != nil || head.ID != 1 {
		t.Fatal("private restore audit refused")
	}
	if e := inspection.Close(); e != nil {
		t.Fatal(e)
	}

	t.Run("cbc-production-refused", func(t *testing.T) {
		// Generic CMS decryption accepts CBC; the production verifier must refuse it
		// even when it contains an otherwise valid backup and uses the correct key.
		cbc := filepath.Join(dir, "unauthenticated-cbc.cms")
		cmsRun(t, "cms", "-encrypt", "-binary", "-aes-256-cbc", "-outform", "DER", "-in", restored, "-out", cbc, "-recip", cert)
		cmsRun(t, "cms", "-decrypt", "-binary", "-inform", "DER", "-in", cbc, "-inkey", key)
		cbcHash, err := digest(cbc)
		if err != nil {
			t.Fatal(err)
		}
		var cbcPublic bytes.Buffer
		if err := runVerify([]string{"--input", cbc, "--private-key", key, "--ciphertext-sha256", cbcHash, "--openssl-sha256", toolHash, "--schema-identity", c.schemaIdentity, "--audit-domain", c.auditDomain, "--expect-schema", "20"}, &cbcPublic); err == nil || cbcPublic.Len() != 0 {
			t.Fatal("unauthenticated CBC accepted")
		}
	})
	for _, test := range []string{"corrupt", "truncated", "wrong-recipient"} {
		t.Run(test, func(t *testing.T) {
			payload := append([]byte(nil), encrypted...)
			useKey := key
			switch test {
			case "corrupt":
				payload[len(payload)-1] ^= 1
			case "truncated":
				payload = payload[:len(payload)-32]
			case "wrong-recipient":
				useKey = wrongKey
			}
			input := filepath.Join(dir, test+".cms")
			if e := os.WriteFile(input, payload, 0600); e != nil {
				t.Fatal(e)
			}
			inputHash, e := digest(input)
			if e != nil {
				t.Fatal(e)
			}
			var refused bytes.Buffer
			before, e := filepath.Glob("/tmp/oberth-backup-rehearsal-*")
			if e != nil {
				t.Fatal(e)
			}
			err := runVerify([]string{"--input", input, "--private-key", useKey, "--ciphertext-sha256", inputHash, "--openssl-sha256", toolHash, "--schema-identity", c.schemaIdentity, "--audit-domain", c.auditDomain, "--expect-schema", "20"}, &refused)
			if err == nil || refused.Len() != 0 {
				t.Fatal("invalid CMS accepted or emitted public result")
			}
			after, e := filepath.Glob("/tmp/oberth-backup-rehearsal-*")
			if e != nil || len(after) != len(before) {
				t.Fatal("unverified plaintext retained")
			}

		})
	}
}
