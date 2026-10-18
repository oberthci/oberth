package installer

// Encrypted state archive for `oberth backup` / `oberth restore`.
//
// The plaintext is a ustar stream — manifest.json first, then a fixed set of
// members — enveloped by `openssl cms` AuthEnvelopedData (AES-256-GCM with an
// RSA-OAEP-SHA256 key transport) to an operator-held recipient certificate.
// This is the same construction cmd/oberth-store-backup uses for database
// recovery material (docs/database-backup.md), so one recipient key and one
// OpenSSL procedure cover both; no custom encryption format is introduced.
//
// Plaintext exists only inside a private 0700 working directory that the
// caller removes, and is parsed only after OpenSSL has authenticated the whole
// envelope and exited successfully.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// backupFormat is what this release writes: the database, the OpenBao
	// state a fresh install cannot recreate, and the grants. v2 (v0.17.32-33)
	// and v1 archives are still read; the Helm values and step logs they
	// carry are ignored.
	backupFormat       = "oberth.backup/v3"
	backupFormatV2     = "oberth.backup/v2"
	backupFormatV1     = "oberth.backup/v1"
	backupManifestName = "manifest.json"

	backupMemberKV           = "openbao/kv.json"
	backupMemberPolicies     = "openbao/policies.json"
	backupMemberRoles        = "openbao/roles.json"
	backupMemberReference    = "openbao/reference.json"
	backupMemberDatabase     = "database/oberth-database.tar"
	backupMemberHelmValues   = "kubernetes/helm-values.json"
	backupMemberSecretAccess = "kubernetes/secret-access.json" // #nosec G101 -- archive member NAME, not credential material.
	backupMemberLogs         = "server/logs.tar"

	// The database member is bounded by the store's own 128 MiB snapshot cap
	// plus tar framing; JSON members are far smaller in practice.
	maxBackupMemberBytes    = 160 << 20
	maxBackupPlaintextBytes = 256 << 20
	maxBackupManifestBytes  = 1 << 20
	maxBackupCipherBytes    = maxBackupPlaintextBytes + (16 << 20)

	// minBackupRecipientBits matches the recipient floor of
	// cmd/oberth-store-backup.
	minBackupRecipientBits = 3072
)

// backupMemberOrder is the exact, closed member inventory of a full v3
// archive in archive order. A restore refuses any archive whose inventory
// differs, so an unexpected file can never be smuggled in next to the
// verified ones.
var backupMemberOrder = []string{
	backupMemberKV,
	backupMemberPolicies,
	backupMemberRoles,
	backupMemberReference,
	backupMemberDatabase,
	backupMemberSecretAccess,
}

// legacyMemberOrder is the full inventory of v1 and v2 archives, which also
// carried the release's Helm values.
var legacyMemberOrder = []string{
	backupMemberKV,
	backupMemberPolicies,
	backupMemberRoles,
	backupMemberReference,
	backupMemberDatabase,
	backupMemberHelmValues,
	backupMemberSecretAccess,
}

// Archive scopes: a full archive holds the database and the secrets; a
// history archive only the database, needs no OpenBao root token to write or
// restore, and leaves the target's secrets alone.
const (
	BackupScopeFull    = "full"
	BackupScopeHistory = "history"
)

// requiredBackupMembers is the closed inventory of a format and scope, in
// archive order.
func requiredBackupMembers(format, scope string) []string {
	switch {
	case scope == BackupScopeHistory:
		return []string{backupMemberDatabase}
	case format == backupFormat:
		return backupMemberOrder
	}
	return legacyMemberOrder
}

// backupManifest is the archive's self-description. It carries no secret
// material: counts, public protocol identifiers, versions and member hashes.
type backupManifest struct {
	Format    string `json:"format"`
	CreatedAt string `json:"created_at"`
	// Scope is BackupScopeFull (also when empty: v1 archives) or
	// BackupScopeHistory.
	Scope         string                `json:"scope,omitempty"`
	ToolVersion   string                `json:"tool_version"`
	Source        backupSource          `json:"source"`
	Compatibility backupCompatibility   `json:"compatibility"`
	Database      backupDatabaseSummary `json:"database"`
	OpenBao       backupOpenBaoSummary  `json:"openbao"`
	// Logs summarised the step logs member of a v2 archive; read, not
	// written or restored.
	Logs *backupLogsSummary `json:"logs,omitempty"`
	// Contents counts what the database holds.
	Contents *backupContents      `json:"contents,omitempty"`
	Members  []backupMemberDigest `json:"members"`
	// NotRestored lists state that is recorded for reference only, with the
	// reason a restore cannot carry it over.
	NotRestored []string `json:"not_restored"`
}

type backupSource struct {
	Context          string `json:"context"`
	APIServer        string `json:"api_server"`
	Namespace        string `json:"namespace"`
	OpenBaoNamespace string `json:"openbao_namespace"`
	ArgoNamespace    string `json:"argo_namespace"`
	ServerImage      string `json:"server_image"`
	ServerVersion    string `json:"server_version"`
	ChartVersion     string `json:"chart_version"`
	// ClusterUID is the source's kube-system namespace UID: unlike the API
	// server URL (https://127.0.0.1:6443 on every k3s), it tells a rollback
	// of the same cluster from a restore onto a reinstalled one (#068).
	ClusterUID string `json:"cluster_uid,omitempty"`
	// ServiceAccountKeyIDs and SigningKeyID were recorded by v2 archives;
	// read, not written.
	ServiceAccountKeyIDs []string `json:"service_account_key_ids,omitempty"`
	SigningKeyID         string   `json:"service_account_signing_key_id,omitempty"`
}

// backupContents counts what the archived database holds.
type backupContents struct {
	Runs         int    `json:"runs"`
	LastRunAt    string `json:"last_run_at,omitempty"`
	Issues       int    `json:"issues"`
	OpenIssues   int    `json:"open_issues"`
	Promotions   int    `json:"promotions"`
	Repositories int    `json:"repositories"`
	Uplinks      int    `json:"uplinks"`
}

// backupLogsSummary describes the step logs member.
type backupLogsSummary struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// backupCompatibility holds the administrator-owned protocol identifiers a
// restored database can only be opened under (issue 048: the audit domain is
// otherwise not recorded anywhere readable).
type backupCompatibility struct {
	SchemaIdentity string `json:"schema_identity"`
	AuditDomain    string `json:"audit_domain"`
	WitnessKeyInfo string `json:"witness_key_info"`
}

type backupDatabaseSummary struct {
	Schema          int    `json:"schema"`
	AuditHeadID     int64  `json:"audit_head_id"`
	AuditHeadSHA256 string `json:"audit_head_sha256"`
	// Snapshot names how the consistent copy was taken: "in-pod" (the server
	// binary's own verified snapshot) or "stable-copy" (an older server
	// without it: both files streamed and hash-checked unchanged across the
	// copy, then snapshotted and verified locally).
	Snapshot string `json:"snapshot"`
}

type backupOpenBaoSummary struct {
	KVMount   string `json:"kv_mount"`
	Secrets   int    `json:"secrets"`
	Policies  int    `json:"policies"`
	AuthMount string `json:"auth_mount"`
	Roles     int    `json:"roles"`
}

type backupMemberDigest struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// backupNotRestored is the fixed reference-only statement every manifest
// carries, so the archive itself says what it cannot bring back.
var backupNotRestored = []string{
	"OpenBao Transit keys are non-exportable: only their metadata is recorded. The installer creates a new key on the target, so trusted-plan envelopes sealed under the old key cannot be decrypted after a restore.",
	"The server PKI CA key never leaves OpenBao: only the CA certificate is recorded. The installer issues a new CA on the target, so every client must re-pin the new server CA.",
	"The Kubernetes auth method configuration is cluster-specific (API server CA and issuer): it is recorded for reference and never restored.",
	"KV secrets are captured at their current version only; version history and per-secret metadata are not preserved.",
	"Files on the server volume (git caches, workspaces, artifacts, step logs) are not part of the backup: the server clones every repository afresh.",
	"The install's own identities in OpenBao (forge key, server keys, forge host keys under identities/) are left out: the new install keeps its own.",
	"Helm values are not part of the backup: re-apply your own values files after a restore. Only the compatibility values the database needs are set by a restore.",
}

// archiveCipher seals and opens the plaintext archive stream.
type archiveCipher interface {
	Encrypt(ctx context.Context, plaintext io.Reader, ciphertext io.Writer) error
	// Decrypt writes the authenticated plaintext to plaintextPath. Callers
	// must not read plaintextPath unless Decrypt returned nil.
	Decrypt(ctx context.Context, ciphertextPath, plaintextPath string) error
}

// opensslCMS is the production archiveCipher: the host's OpenSSL 3 `cms`
// command with the exact algorithm choices of cmd/oberth-store-backup.
type opensslCMS struct {
	binary     string
	recipient  string
	privateKey string
}

func (c opensslCMS) Encrypt(ctx context.Context, plaintext io.Reader, ciphertext io.Writer) error {
	// #nosec G204 -- fixed operation; the binary was resolved from PATH and the
	// recipient is a validated public certificate path.
	cmd := exec.CommandContext(ctx, c.binary, "cms", "-encrypt", "-binary", "-aes-256-gcm", "-outform", "DER",
		"-recip", c.recipient, "-keyopt", "rsa_padding_mode:oaep", "-keyopt", "rsa_oaep_md:sha256")
	cmd.Stdin = plaintext
	cmd.Stdout = ciphertext
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("openssl cms -encrypt: %w%s", err, opensslDiagnostic(stderr.Bytes()))
	}
	return nil
}

func (c opensslCMS) Decrypt(ctx context.Context, ciphertextPath, plaintextPath string) error {
	// #nosec G204 -- fixed operation; the input, key and output are explicit
	// operator paths and a path inside the private working directory.
	cmd := exec.CommandContext(ctx, c.binary, "cms", "-decrypt", "-binary", "-inform", "DER",
		"-in", ciphertextPath, "-inkey", c.privateKey, "-out", plaintextPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	// OpenSSL may emit provisional plaintext before the GCM tag is checked;
	// only a successful exit makes the output admissible.
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("openssl cms -decrypt (wrong private key, or a damaged or tampered archive): %w%s", err, opensslDiagnostic(stderr.Bytes()))
	}
	return nil
}

// opensslDiagnostic keeps the first stderr line of a failed OpenSSL call. It
// never carries plaintext: OpenSSL writes only status text to stderr.
func opensslDiagnostic(stderr []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(stderr)), "\n")
	if line == "" {
		return ""
	}
	return ": " + line
}

// validateBackupRecipient accepts exactly one PEM certificate carrying an RSA
// key of at least minBackupRecipientBits, which RSA-OAEP key transport needs.
func validateBackupRecipient(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("recipient certificate: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return fmt.Errorf("recipient certificate %s must be a regular PEM file of at most 64 KiB", path)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- explicit operator path to a public certificate.
	if err != nil {
		return fmt.Errorf("recipient certificate: %w", err)
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" || strings.TrimSpace(string(rest)) != "" {
		return fmt.Errorf("recipient %s must contain exactly one PEM CERTIFICATE block", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse recipient certificate: %w", err)
	}
	key, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || key.N.BitLen() < minBackupRecipientBits {
		return fmt.Errorf("recipient certificate must carry an RSA key of at least %d bits (RSA-OAEP key transport)", minBackupRecipientBits)
	}
	return nil
}

// validateBackupPrivateKey refuses a private key file that other local users
// could read; it never reads or prints the key bytes.
func validateBackupPrivateKey(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("private key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return fmt.Errorf("private key %s must be a regular PEM file of at most 64 KiB", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private key %s is readable by group or others (mode %04o); chmod 600 it first", path, info.Mode().Perm())
	}
	return nil
}

// authEnvelopedDataOID is id-ct-authEnvelopedData (RFC 5083).
var authEnvelopedDataOID = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 23}

// requireAuthEnvelopedData checks that the archive is a DER ContentInfo whose
// content type is AuthEnvelopedData. Unauthenticated CBC EnvelopedData — which
// anyone holding the public certificate could also produce, and which OpenSSL
// would happily decrypt — is refused before any decryption runs.
func requireAuthEnvelopedData(path string) error {
	file, err := os.Open(path) // #nosec G304 -- explicit operator archive path.
	if err != nil {
		return fmt.Errorf("open backup archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat backup archive: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxBackupCipherBytes {
		return fmt.Errorf("backup archive %s is not a regular file within the %d MiB bound", path, maxBackupCipherBytes>>20)
	}
	header := make([]byte, 32)
	n, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("read backup archive: %w", err)
	}
	header = header[:n]
	refused := errors.New("backup archive is not a CMS AuthEnvelopedData (AES-GCM) envelope; refusing to decrypt it")
	if len(header) < 2 || header[0] != 0x30 {
		return refused
	}
	offset := 2
	if header[1]&0x80 != 0 {
		offset += int(header[1] & 0x7f)
	}
	expected, err := asn1.Marshal(authEnvelopedDataOID)
	if err != nil {
		return err
	}
	if len(header) < offset+len(expected) || !bytes.Equal(header[offset:offset+len(expected)], expected) {
		return refused
	}
	return nil
}

// writeBackupArchive streams manifest + members (read from dir, by member
// name) through the cipher into a new 0600 output file. The output path must
// not exist; a failed write removes the partial file.
func writeBackupArchive(ctx context.Context, dir string, manifest *backupManifest, cipher archiveCipher, output string) (resultErr error) {
	manifest.Members = manifest.Members[:0]
	var total int64
	for _, name := range requiredBackupMembers(manifest.Format, manifest.Scope) {
		digest, err := digestBackupMember(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("backup member %s: %w", name, err)
		}
		digest.Name = name
		total += digest.Size
		manifest.Members = append(manifest.Members, digest)
	}
	if total > maxBackupPlaintextBytes {
		return fmt.Errorf("backup plaintext is %d MiB, above the %d MiB bound", total>>20, maxBackupPlaintextBytes>>20)
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode backup manifest: %w", err)
	}

	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- explicit operator output path, created exclusively.
	if err != nil {
		return fmt.Errorf("create backup archive: %w", err)
	}
	succeeded := false
	defer func() {
		_ = file.Close()
		if !succeeded {
			_ = os.Remove(output)
		}
	}()

	reader, writer := io.Pipe()
	produced := make(chan error, 1)
	go func() {
		err := writeBackupTar(writer, dir, manifestBytes, manifest.Members)
		_ = writer.CloseWithError(err)
		produced <- err
	}()
	encryptErr := cipher.Encrypt(ctx, reader, file)
	// Unblock the producer if the cipher stopped reading early.
	_ = reader.CloseWithError(errors.New("archive encryption stopped"))
	produceErr := <-produced
	if encryptErr != nil {
		return encryptErr
	}
	if produceErr != nil {
		return fmt.Errorf("write backup archive stream: %w", produceErr)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync backup archive: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close backup archive: %w", err)
	}
	succeeded = true
	return nil
}

// writeBackupTar writes the gzip-compressed plaintext tar (v2; step logs
// compress well). v1 archives were uncompressed; openBackupArchive reads both.
func writeBackupTar(w io.Writer, dir string, manifest []byte, members []backupMemberDigest) (resultErr error) {
	compressed := gzip.NewWriter(w)
	defer func() { resultErr = errors.Join(resultErr, compressed.Close()) }()
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: backupManifestName, Mode: 0o600, Size: int64(len(manifest)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	if _, err := archive.Write(manifest); err != nil {
		return err
	}
	for _, member := range members {
		if err := archive.WriteHeader(&tar.Header{Name: member.Name, Mode: 0o600, Size: member.Size, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(member.Name))) // #nosec G304 -- fixed member name inside the private working directory.
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(archive, f, member.Size)
		closeErr := f.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
	}
	return archive.Close()
}

func digestBackupMember(path string) (backupMemberDigest, error) {
	f, err := os.Open(path) // #nosec G304 -- fixed member path inside the private working directory.
	if err != nil {
		return backupMemberDigest{}, err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, maxBackupMemberBytes+1))
	if err != nil {
		return backupMemberDigest{}, err
	}
	if n > maxBackupMemberBytes {
		return backupMemberDigest{}, fmt.Errorf("member exceeds the %d MiB bound", maxBackupMemberBytes>>20)
	}
	return backupMemberDigest{Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// openBackupArchive authenticates and decrypts input into dir, extracts the
// closed member inventory, and verifies every member against the manifest's
// size and SHA-256. The decrypted tar stream is removed once extracted.
func openBackupArchive(ctx context.Context, input string, cipher archiveCipher, dir string) (backupManifest, error) {
	if err := requireAuthEnvelopedData(input); err != nil {
		return backupManifest{}, err
	}
	plaintext := filepath.Join(dir, "archive.tar")
	if err := cipher.Decrypt(ctx, input, plaintext); err != nil {
		_ = os.Remove(plaintext)
		return backupManifest{}, err
	}
	defer func() { _ = os.Remove(plaintext) }()

	f, err := os.Open(plaintext) // #nosec G304 -- fixed name inside the private working directory.
	if err != nil {
		return backupManifest{}, fmt.Errorf("open decrypted archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	// The bound applies to the decompressed stream, so a small compressed
	// archive cannot expand without limit.
	buffered := bufio.NewReader(f)
	var plain io.Reader = buffered
	if magic, err := buffered.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		unzipped, err := gzip.NewReader(buffered)
		if err != nil {
			return backupManifest{}, fmt.Errorf("read decrypted archive: %w", err)
		}
		defer func() { _ = unzipped.Close() }()
		plain = unzipped
	}
	reader := tar.NewReader(io.LimitReader(plain, maxBackupPlaintextBytes+(1<<20)))

	allowed := make(map[string]bool, len(legacyMemberOrder)+1)
	for _, name := range append(slices.Clone(legacyMemberOrder), backupMemberLogs) {
		allowed[name] = true
	}
	var manifestBytes []byte
	var extracted []backupMemberDigest
	for {
		if err := ctx.Err(); err != nil {
			return backupManifest{}, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return backupManifest{}, fmt.Errorf("read decrypted archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 {
			return backupManifest{}, fmt.Errorf("archive entry %q is not a regular file", header.Name)
		}
		if header.Name == backupManifestName {
			if manifestBytes != nil || len(extracted) != 0 || header.Size > maxBackupManifestBytes {
				return backupManifest{}, errors.New("archive manifest is misplaced, duplicated or oversized")
			}
			if manifestBytes, err = io.ReadAll(io.LimitReader(reader, maxBackupManifestBytes)); err != nil {
				return backupManifest{}, fmt.Errorf("read archive manifest: %w", err)
			}
			continue
		}
		if manifestBytes == nil || !allowed[header.Name] || header.Size > maxBackupMemberBytes {
			return backupManifest{}, fmt.Errorf("archive entry %q is not part of the %s inventory", header.Name, backupFormat)
		}
		delete(allowed, header.Name)
		digest, err := extractBackupMember(reader, filepath.Join(dir, filepath.FromSlash(header.Name)), header.Size)
		if err != nil {
			return backupManifest{}, fmt.Errorf("extract %s: %w", header.Name, err)
		}
		digest.Name = header.Name
		extracted = append(extracted, digest)
	}
	if manifestBytes == nil {
		return backupManifest{}, errors.New("archive has no manifest")
	}
	// Read the format leniently first: a newer archive must say which
	// release wrote it, not fail on the first field this one does not know.
	var header struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(manifestBytes, &header); err != nil {
		return backupManifest{}, fmt.Errorf("parse archive manifest: %w", err)
	}
	if !slices.Contains([]string{backupFormat, backupFormatV2, backupFormatV1}, header.Format) {
		return backupManifest{}, fmt.Errorf("archive format %q is not %q; use the oberth release that wrote it", header.Format, backupFormat)
	}
	var manifest backupManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return backupManifest{}, fmt.Errorf("parse archive manifest: %w", err)
	}
	if manifest.Format == backupFormatV1 && manifest.Scope != "" || manifest.Scope != "" && manifest.Scope != BackupScopeFull && manifest.Scope != BackupScopeHistory {
		return backupManifest{}, fmt.Errorf("archive scope %q is not one this release reads", manifest.Scope)
	}
	// The closed inventory in order; a v2 archive may end with step logs,
	// which this release no longer restores.
	want := slices.Clone(requiredBackupMembers(manifest.Format, manifest.Scope))
	if manifest.Format == backupFormatV2 && len(extracted) == len(want)+1 && extracted[len(want)].Name == backupMemberLogs {
		want = append(want, backupMemberLogs)
	}
	if len(manifest.Members) != len(want) || len(extracted) != len(want) {
		return backupManifest{}, errors.New("archive member inventory is incomplete")
	}
	for i, name := range want {
		if manifest.Members[i].Name != name || extracted[i] != manifest.Members[i] {
			return backupManifest{}, fmt.Errorf("archive member %s does not match its manifest size/SHA-256", name)
		}
	}
	return manifest, nil
}

func extractBackupMember(r io.Reader, destination string, size int64) (backupMemberDigest, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return backupMemberDigest{}, err
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- inventory-checked member name inside the private working directory.
	if err != nil {
		return backupMemberDigest{}, err
	}
	hash := sha256.New()
	n, copyErr := io.CopyN(io.MultiWriter(out, hash), r, size)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return backupMemberDigest{}, err
	}
	return backupMemberDigest{Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// writeBackupJSON writes one JSON member into the private working directory.
func writeBackupJSON(dir, name string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	return writeBackupFile(dir, name, body)
}

func writeBackupFile(dir, name string, body []byte) error {
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600) // #nosec G306 G703 -- fixed member name inside the private working directory.
}

// readBackupJSON decodes one extracted member strictly.
func readBackupJSON(dir, name string, value any) error {
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))) // #nosec G304 -- fixed member name inside the private working directory.
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	return nil
}

// fileSHA256 returns the hex SHA-256 of a file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- explicit operator or private working path.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
