//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

// Verification is a private restore rehearsal, never a live restore operation.
func runVerify(args []string, public io.Writer) (resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	var input, key, inputSHA, toolSHA, identity, domain string
	var schema int
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input, "input", "", "encrypted archive")
	flags.StringVar(&key, "private-key", "", "dedicated private recipient")
	flags.StringVar(&inputSHA, "ciphertext-sha256", "", "bound encrypted archive")
	flags.StringVar(&toolSHA, "openssl-sha256", "", "pinned executable")
	flags.StringVar(&identity, "schema-identity", "", "original schema identity")
	flags.StringVar(&domain, "audit-domain", "", "original audit domain")
	flags.IntVar(&schema, "expect-schema", 0, "exact pre-upgrade schema")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || schema < 3 || !validHash(inputSHA) || !validHash(toolSHA) {
		return errors.New("verify arguments refused")
	}
	if err := requireSandbox(); err != nil {
		return err
	}
	for _, p := range []string{input, key} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return errors.New("verify path refused")
		}
	}
	tool, err := digest(opensslPath)
	if err != nil || tool != toolSHA {
		return errors.New("tool identity refused")
	}
	encrypted, err := os.Lstat(input)
	if err != nil || !encrypted.Mode().IsRegular() || encrypted.Size() > 256<<20 {
		return errors.New("ciphertext refused")
	}
	actual, err := digest(input)
	if err != nil || actual != inputSHA {
		return errors.New("ciphertext binding refused")
	}
	keyInfo, err := os.Lstat(key)
	if err != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm() != 0600 || keyInfo.Size() > 16384 {
		return errors.New("private recipient custody refused")
	}
	stat, ok := keyInfo.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) {
		return errors.New("private recipient owner refused")
	}
	opts := store.Options{Protocol: store.ProtocolConfig{SchemaIdentity: identity, AuditDomain: domain}}
	if err := opts.Protocol.Validate(); err != nil {
		return err
	}
	temp, err := os.MkdirTemp("/tmp", "oberth-backup-rehearsal-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temp)) }()
	plaintext, err := os.OpenFile(filepath.Join(temp, "authenticated.tar"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600) // #nosec G304 -- fixed filename within a fresh exclusive private tmpfs directory.
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, plaintext.Close()) }()
	if err := requireAuthenticatedCMS(input); err != nil {
		return err
	}
	if err := checkDeadline(ctx); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, opensslPath, "cms", "-decrypt", "-binary", "-inform", "DER", "-in", input, "-inkey", key) // #nosec G204 -- fixed operation; exact administrator-bound input and private recipient paths.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "OPENSSL_CONF=/dev/null"}
	cmd.Stdout = plaintext
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	// OpenSSL may emit provisional plaintext before authentication. Do not read or
	// parse any of it unless the positively joined child reports success.
	if err := cmd.Run(); err != nil {
		return err
	}
	if _, err := plaintext.Seek(0, io.SeekStart); err != nil {
		return err
	}
	report, err := store.VerifyBackupArchive(ctx, plaintext, schema, opts)
	if err != nil {
		return err
	}
	after, err := digest(input)
	if err != nil || after != inputSHA {
		return errors.New("ciphertext changed")
	}
	return emitReceipt(ctx, public, struct {
		Format string `json:"format"`
		store.BackupReport
		CipherSHA   string `json:"ciphertext_sha256"`
		LiveRestore bool   `json:"live_restore"`
	}{"oberth.private-backup-rehearsal/v1", report, inputSHA, false})
}
