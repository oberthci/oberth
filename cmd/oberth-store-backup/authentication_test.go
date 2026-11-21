package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestAgeRecoveryRefusals(t *testing.T) {
	damaged := recoveryError(damagedEncrypted, nil).Reason()
	for _, tc := range []struct {
		mode, reason string
		code         publicerr.Code
	}{
		{"truncated", damaged, publicerr.InvalidInput},
		{"altered-middle", damaged, publicerr.InvalidInput},
		{"wrong-identity", recoveryError(identityMismatch, nil).Reason(), publicerr.InvalidInput},
		{"foreign-installation", recoveryError(foreignInstallation, nil).Reason(), publicerr.SchemaRefused},
		{"plain-file", recoveryError(notBackup, nil).Reason(), publicerr.InvalidInput},
		{"trailing-bytes", damaged, publicerr.InvalidInput},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			c := cryptoFixture(t)
			must(t, encrypt(t.Context(), c, io.Discard))
			c = recoveryConfig(c)
			mutateArchive(t, &c, tc.mode)
			checkFailedRestore(t, c, tc.code, tc.reason)
		})
	}
}

func checkFailedRestore(t *testing.T, c config, code publicerr.Code, reason string) {
	t.Helper()
	var out bytes.Buffer
	err := decrypt(t.Context(), c, &out)
	var diagnostic *publicerr.Error
	if !errors.As(err, &diagnostic) {
		t.Fatal("unsafe or absent refusal", err)
	}
	if diagnostic.Code() != code || (reason != "" && diagnostic.Reason() != reason) {
		t.Fatal("restore cause lost", err)
	}
	if out.Len() != 0 {
		t.Fatal("failed restore emitted receipt")
	}
	if _, err := os.Stat(c.output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed restore published", err)
	}
	assertNoStage(t, filepath.Dir(c.input))
}

func mutateArchive(t *testing.T, c *config, mode string) {
	t.Helper()
	body, err := os.ReadFile(c.input)
	must(t, err)
	switch mode {
	case "truncated":
		body = body[:len(body)-1]
	case "altered-middle":
		body[len(body)/2] ^= 1
	case "trailing-bytes":
		body = append(body, 1)
	case "plain-file":
		body = []byte("plain file, not an encrypted archive\n")
	case "wrong-identity":
		key, err := age.GenerateX25519Identity()
		must(t, err)
		must(t, os.WriteFile(c.identity, []byte(key.String()+"\n"), 0600))
	case "foreign-installation":
		c.options.Protocol.AuditDomain += "-another"
	}
	must(t, os.WriteFile(c.input, body, 0600))
}

func assertNoStage(t *testing.T, parent string) {
	t.Helper()
	stages, err := filepath.Glob(filepath.Join(parent, ".oberth-state-*"))
	must(t, err)
	if len(stages) != 0 {
		t.Fatal("owned staging survived", stages)
	}
}

func TestAuthenticateEOFPrecedesAnyArchiveParsing(t *testing.T) {
	c := cryptoFixture(t)
	var encrypted bytes.Buffer
	recipient, err := age.ParseX25519Recipient(c.recipient)
	must(t, err)
	w, err := age.Encrypt(&encrypted, recipient)
	must(t, err)
	// An invalid first archive byte must not hide a bad final authentication tag.
	_, err = w.Write(bytes.Repeat([]byte("invalid archive"), 10000))
	must(t, err)
	must(t, w.Close())
	body := encrypted.Bytes()
	body[len(body)-1] ^= 1
	must(t, os.WriteFile(c.output, body, 0600))
	c = recoveryConfig(c)
	err = decrypt(t.Context(), c, io.Discard)
	var public *publicerr.Error
	if !errors.As(err, &public) || public.Reason() != recoveryError(damagedEncrypted, nil).Reason() {
		t.Fatal("archive parsed before final authentication", err)
	}
}

func TestTwoAgeRestoresNeverReplaceOneAnother(t *testing.T) {
	c := cryptoFixture(t)
	must(t, encrypt(t.Context(), c, io.Discard))
	c = recoveryConfig(c)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; results <- decrypt(t.Context(), c, io.Discard) }()
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if publicerr.Classify(err).Code() != publicerr.Conflict {
			t.Fatal("unexpected concurrent refusal", err)
		}
	}
	if successes != 1 {
		t.Fatal("restore publication winners", successes)
	}
	var out bytes.Buffer
	c.mode = "verify"
	must(t, decrypt(t.Context(), c, &out))
	var r receipt
	must(t, json.Unmarshal(out.Bytes(), &r))
	must(t, r.Validate())
	assertNoStage(t, filepath.Dir(c.input))
}
