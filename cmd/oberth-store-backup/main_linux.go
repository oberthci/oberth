//go:build linux

// oberth-store-backup is a source-built, Linux-only maintenance utility. It is
// not a native MCP endpoint or part of the ordinary Oberth release inventory.
package main

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

const opensslPath = "/usr/bin/openssl"

type config struct {
	database, recipient, output, opensslSHA, recipientSHA, schemaIdentity, auditDomain string
	schema                                                                             int
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "database backup refused; no live restore or automatic retry")
		os.Exit(1)
	}
}
func run(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "verify" {
		return runVerify(args[1:], out)
	}
	c := config{}
	f := flag.NewFlagSet("oberth-store-backup", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&c.database, "database", "", "existing database")
	f.StringVar(&c.recipient, "recipient", "", "public CMS recipient certificate")
	f.StringVar(&c.output, "output", "", "new private encrypted output")
	f.StringVar(&c.opensslSHA, "openssl-sha256", "", "pinned OpenSSL executable")
	f.StringVar(&c.recipientSHA, "recipient-sha256", "", "pinned public certificate")
	f.StringVar(&c.schemaIdentity, "schema-identity", "", "original deployment identity")
	f.StringVar(&c.auditDomain, "audit-domain", "", "original audit domain")
	f.IntVar(&c.schema, "expect-schema", 0, "exact schema before and after read-only inspection")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("arguments refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	return backup(ctx, c, out)
}
func digest(p string) (string, error) {
	f, e := os.Open(p) // #nosec G304 -- source-owned maintenance input; paths are explicit operator bindings.
	if e != nil {
		return "", e
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func validHash(v string) bool {
	b, e := hex.DecodeString(v)
	return e == nil && len(b) == 32 && strings.ToLower(v) == v
}
func backup(ctx context.Context, c config, public io.Writer) (resultErr error) {
	if err := checkDeadline(ctx); err != nil {
		return err
	}
	for _, p := range []string{c.database, c.recipient, c.output} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "?\x00") {
			return errors.New("path refused")
		}
	}
	if c.schema < 3 || !validHash(c.opensslSHA) || !validHash(c.recipientSHA) {
		return errors.New("binding refused")
	}
	opts := store.Options{Protocol: store.ProtocolConfig{SchemaIdentity: c.schemaIdentity, AuditDomain: c.auditDomain}}
	if e := opts.Protocol.Validate(); e != nil {
		return e
	}
	if e := requireSandbox(); e != nil {
		return e
	}

	tool, e := digest(opensslPath)
	if e != nil || tool != c.opensslSHA {
		return errors.New("tool identity refused")
	}
	info, e := os.Lstat(c.recipient)
	if e != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return errors.New("recipient refused")
	}
	cert, e := os.ReadFile(c.recipient)
	if e != nil {
		return e
	}
	sum := sha256.Sum256(cert)
	if hex.EncodeToString(sum[:]) != c.recipientSHA {
		return errors.New("recipient changed")
	}
	block, rest := pem.Decode(cert)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("recipient format refused")
	}
	parsed, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		return e
	}
	key, ok := parsed.PublicKey.(*rsa.PublicKey)
	if !ok || key.N.BitLen() < 3072 {
		return errors.New("recipient key refused")
	}
	parent, e := os.Lstat(filepath.Dir(c.output))
	if e != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return errors.New("private output directory required")
	}
	if stat, ok := parent.Sys().(*syscall.Stat_t); !ok || int64(stat.Uid) != int64(os.Geteuid()) {
		return errors.New("output owner refused")
	}
	temp, e := os.MkdirTemp("/tmp", "oberth-backup-cms-")
	if e != nil {
		return e
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temp)) }()
	certCopy := filepath.Join(temp, "recipient.pem")
	if e := os.WriteFile(certCopy, cert, 0600); // #nosec G703 -- fixed filename within a newly exclusive private tmpfs directory.
	e != nil {
		return e
	}
	file, e := os.OpenFile(c.output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	succeeded := false
	defer func() {
		_ = file.Close()
		if !succeeded {
			_ = os.Remove(c.output)
		}
	}()
	cmd := cmsEncryptCommand(ctx, certCopy)
	cmd.Stdout = cmsCipherOutput(ctx, file)
	input, e := cmd.StdinPipe()
	if e != nil {
		return e
	}
	if e := cmd.Start(); e != nil {
		_ = input.Close()
		return e
	}
	report, exportErr := store.WriteStableVerifiedBackup(ctx, c.database, c.schema, opts, input)
	closeErr := input.Close()
	if exportErr != nil || closeErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if e := errors.Join(exportErr, closeErr, waitErr); e != nil {
		return e
	}
	if e := checkDeadline(ctx); e != nil {
		return e
	}
	if e := file.Sync(); e != nil {
		return e
	}
	if e := file.Close(); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(c.output))
	if e != nil {
		return e
	}
	syncErr := dir.Sync()
	closeErr = dir.Close()
	if e := errors.Join(syncErr, closeErr); e != nil {
		return e
	}
	cipherSHA, e := digest(c.output)
	if e != nil {
		return e
	}
	cipherInfo, e := os.Stat(c.output)
	if e != nil {
		return e
	}
	// A successful crypto process exit alone does not prove complete output.
	// This is a structural check only; private authenticated rehearsal is still
	// required before this archive can be accepted as usable recovery material.
	if e := requireAuthenticatedCMS(c.output); e != nil {
		return e
	}
	receipt := struct {
		Format string `json:"format"`
		store.BackupReport
		CipherSHA      string `json:"ciphertext_sha256"`
		CipherBytes    int64  `json:"ciphertext_bytes"`
		RecipientSHA   string `json:"recipient_certificate_sha256"`
		Encryption     string `json:"encryption"`
		SourceReadOnly bool   `json:"source_read_only"`
	}{"oberth.encrypted-backup-receipt/v1", report, cipherSHA, cipherInfo.Size(), c.recipientSHA, "CMS AuthEnvelopedData AES-256-GCM; RSA-OAEP-SHA256", true}
	if e := emitReceipt(ctx, public, receipt); e != nil {
		return e
	}
	succeeded = true
	return nil
}

// requireSandbox independently checks the essential no-swap/capability bounds.
// The retained launcher also proves private mount isolation and positive joins.
func requireSandbox() error {
	var fs syscall.Statfs_t
	if e := syscall.Statfs("/tmp", &fs); e != nil || fs.Type != 0x01021994 {
		return errors.New("private tmpfs required")
	}
	raw, e := os.ReadFile("/proc/self/cgroup")
	if e != nil {
		return e
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "0::/") || strings.Contains(line, "\n") {
		return errors.New("unified cgroup required")
	}
	group := strings.TrimPrefix(line, "0::")
	if filepath.Clean(group) != group || group == "/" {
		return errors.New("owned cgroup required")
	}
	swap, e := os.ReadFile("/sys/fs/cgroup" + group + "/memory.swap.max") // #nosec G304 G703 -- canonical unified path read from proc/self, fixed cgroup resource file.
	if e != nil || strings.TrimSpace(string(swap)) != "0" {
		return errors.New("no-swap boundary required")
	}
	status, e := os.ReadFile("/proc/self/status")
	if e != nil {
		return e
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(status), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			fields[k] = strings.TrimSpace(v)
		}
	}
	if fields["NoNewPrivs"] != "1" {
		return errors.New("no-new-privileges required")
	}
	for _, k := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		if fields[k] != "0000000000000000" {
			return errors.New("empty capabilities required")
		}
	}
	return nil
}

func cmsEncryptCommand(ctx context.Context, certCopy string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, opensslPath, "cms", "-encrypt", "-binary", "-aes-256-gcm", "-outform", "DER", "-recip", certCopy, "-keyopt", "rsa_padding_mode:oaep", "-keyopt", "rsa_oaep_md:sha256") // #nosec G204 -- fixed executable and operation; recipient is a private validated public-certificate copy.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "OPENSSL_CONF=/dev/null"}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

type cipherOutput struct {
	ctx  context.Context
	file *os.File
}

// Returning a wrapper rather than *os.File deliberately prevents os/exec's
// direct-FD optimization. Its joined copy goroutine must report ENOSPC and other
// output errors even if OpenSSL exits successfully after a failed stdout flush.
func cmsCipherOutput(ctx context.Context, file *os.File) io.Writer {
	return cipherOutput{ctx: ctx, file: file}
}

func (w cipherOutput) Write(p []byte) (int, error) {
	if err := checkDeadline(w.ctx); err != nil {
		return 0, err
	}
	n, err := w.file.Write(p)
	if err != nil {
		return n, err
	}
	return n, checkDeadline(w.ctx)
}
