package runner

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Store-sourced release secrets never exist as Kubernetes Secrets, etcd
// entries, or node-disk files. The Oberth server fetches them from OpenBao at
// admission, holds them only in process memory, and delivers them into the
// running release Job over an exec channel: a helper invocation of this same
// binary reads the payload from stdin and materializes it on the Job's
// memory-backed (tmpfs) volume, writing the manifest last. The main runner
// process refuses to interpret periapsis source until the manifest and every
// listed file verify, and fails closed on timeout.

// SecretStoreManifestFileName is written last by the delivery helper; its presence
// marks a complete, verifiable delivery.
const SecretStoreManifestFileName = ".oberth-secretstore-manifest.json" // #nosec G101 -- a manifest file name, not a credential.

// SecretStorePayloadVersion is the wire version both delivery sides must agree on.
const SecretStorePayloadVersion = 1

// DefaultSecretStoreAwaitTimeout bounds how long the runner waits for the server to
// deliver store-sourced secrets after the container starts.
const DefaultSecretStoreAwaitTimeout = 5 * time.Minute

const (
	maxSecretStorePayloadBytes = 16 << 20
	maxStoreValueBytes         = 1 << 20
	maxStoreTotalBytes         = 8 << 20
	maxStoreFiles              = 256
	maxStoreNameBytes          = 63
	storeAwaitPollDelay        = 100 * time.Millisecond
	storeSecretDirMode         = 0o700
	storeSecretFileMode        = 0o400
	storeManifestFileMode      = 0o400
)

// SecretStorePayload is the exec-delivered wire format for store-sourced release
// secrets. Values travel base64-encoded inside the JSON body and never appear
// in argv, environment, annotations, or logs.
type SecretStorePayload struct {
	Version int                        `json:"version"`
	Secrets []SecretStorePayloadSecret `json:"secrets"`
}

// SecretStorePayloadSecret delivers the keys of one declared secret store secret under its
// repository-chosen delivery name.
type SecretStorePayloadSecret struct {
	Name string            `json:"name"`
	Keys map[string][]byte `json:"keys"`
}

type storeManifest struct {
	Version int                 `json:"version"`
	Files   []storeManifestFile `json:"files"`
}

type storeManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// EncodeSecretStorePayload validates and serializes a delivery payload. The server
// calls it once per release Job with store-declared secrets.
func EncodeSecretStorePayload(payload SecretStorePayload) ([]byte, error) {
	if err := validateSecretStorePayload(payload); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode secret store payload: %w", err)
	}
	if len(encoded) > maxSecretStorePayloadBytes {
		return nil, fmt.Errorf("secret store payload exceeds %d bytes", maxSecretStorePayloadBytes)
	}
	return encoded, nil
}

// ReceiveSecretStoreSecrets is the exec-helper entrypoint: it reads one bounded JSON
// payload from input and materializes it under dir on the Job's memory-backed
// volume, atomically per file, manifest last. Reruns with the identical
// payload converge; the helper never prints secret values.
func ReceiveSecretStoreSecrets(input io.Reader, dir string) error {
	if input == nil {
		return errors.New("secret store payload input is required")
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return errors.New("secret store delivery directory must be a clean absolute path")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("inspect secret store delivery directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("secret store delivery directory is not a directory")
	}
	raw, err := io.ReadAll(io.LimitReader(input, maxSecretStorePayloadBytes+1))
	if err != nil {
		return fmt.Errorf("read secret store payload: %w", err)
	}
	if len(raw) > maxSecretStorePayloadBytes {
		return fmt.Errorf("secret store payload exceeds %d bytes", maxSecretStorePayloadBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var payload SecretStorePayload
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("decode secret store payload: %w", err)
	}
	if decoder.More() {
		return errors.New("secret store payload has trailing data")
	}
	if err := validateSecretStorePayload(payload); err != nil {
		return err
	}
	manifest := storeManifest{Version: SecretStorePayloadVersion}
	for _, secret := range payload.Secrets {
		secretDir := filepath.Join(dir, secret.Name)
		if err := os.MkdirAll(secretDir, storeSecretDirMode); err != nil {
			return fmt.Errorf("create secret store delivery directory %s: %w", secret.Name, err)
		}
		keys := make([]string, 0, len(secret.Keys))
		for key := range secret.Keys {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			value := secret.Keys[key]
			if err := writeStoreFile(secretDir, key, value); err != nil {
				return fmt.Errorf("write secret store secret %s/%s: %w", secret.Name, key, err)
			}
			digest := sha256.Sum256(value)
			manifest.Files = append(manifest.Files, storeManifestFile{
				Path:   secret.Name + "/" + key,
				SHA256: hex.EncodeToString(digest[:]),
			})
		}
	}
	slices.SortFunc(manifest.Files, func(left, right storeManifestFile) int {
		return strings.Compare(left.Path, right.Path)
	})
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode secret store delivery manifest: %w", err)
	}
	if err := writeStoreFile(dir, SecretStoreManifestFileName, encoded); err != nil {
		return fmt.Errorf("write secret store delivery manifest: %w", err)
	}
	return nil
}

// AwaitSecretStoreSecrets blocks until a complete, digest-verified delivery exists
// under dir, then returns every delivered value for masking. It fails closed
// after timeout: a release burn never starts on a partial delivery.
func AwaitSecretStoreSecrets(ctx context.Context, dir string, timeout time.Duration) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = DefaultSecretStoreAwaitTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(storeAwaitPollDelay)
	defer poll.Stop()
	for {
		values, err := loadStoreDelivery(dir)
		if err == nil {
			return values, nil
		}
		if !errors.Is(err, errStoreDeliveryPending) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("interrupted while waiting for secret store delivery: %w", ctx.Err())
		case <-deadline.C:
			return nil, fmt.Errorf(
				"timed out after %s waiting for secret store delivery under %s: the Oberth server injects declared SecretStoreSecrets after the runner starts; check the server log for delivery errors",
				timeout, dir,
			)
		case <-poll.C:
		}
	}
}

var errStoreDeliveryPending = errors.New("secret store delivery is not complete yet")

func loadStoreDelivery(dir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, SecretStoreManifestFileName)) // #nosec G304 -- dir is the runner-owned delivery mount; the name is a fixed constant.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errStoreDeliveryPending
	}
	if err != nil {
		return nil, fmt.Errorf("read secret store delivery manifest: %w", err)
	}
	if len(raw) > maxSecretStorePayloadBytes {
		return nil, errors.New("secret store delivery manifest exceeds the payload bound")
	}
	var manifest storeManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode secret store delivery manifest: %w", err)
	}
	if manifest.Version != SecretStorePayloadVersion {
		return nil, fmt.Errorf("secret store delivery manifest version %d is not supported", manifest.Version)
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > maxStoreFiles {
		return nil, fmt.Errorf("secret store delivery manifest lists %d files, want 1-%d", len(manifest.Files), maxStoreFiles)
	}
	expected := make(map[string]struct{}, len(manifest.Files))
	values := make([]string, 0, len(manifest.Files))
	total := 0
	for _, entry := range manifest.Files {
		name, key, ok := strings.Cut(entry.Path, "/")
		if !ok || !validStoreName(name) || !validStoreKey(key) {
			return nil, fmt.Errorf("secret store delivery manifest path %q is unsafe", entry.Path)
		}
		if _, duplicate := expected[entry.Path]; duplicate {
			return nil, fmt.Errorf("secret store delivery manifest path %q is duplicated", entry.Path)
		}
		expected[entry.Path] = struct{}{}
		wantDigest, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(wantDigest) != sha256.Size {
			return nil, fmt.Errorf("secret store delivery manifest digest for %q is invalid", entry.Path)
		}
		value, err := readBoundedStoreFile(filepath.Join(dir, name, key))
		if err != nil {
			return nil, fmt.Errorf("read delivered secret store entry %s: %w", entry.Path, err)
		}
		total += len(value)
		if total > maxStoreTotalBytes {
			return nil, fmt.Errorf("delivered secret store entries exceed %d bytes", maxStoreTotalBytes)
		}
		gotDigest := sha256.Sum256(value)
		if subtle.ConstantTimeCompare(gotDigest[:], wantDigest) != 1 {
			return nil, fmt.Errorf("delivered secret store entry %s does not match its manifest digest", entry.Path)
		}
		values = append(values, string(value))
	}
	if err := verifyNoUnexpectedStoreFiles(dir, expected); err != nil {
		return nil, err
	}
	return values, nil
}

// verifyNoUnexpectedStoreFiles rejects any non-hidden regular file the
// manifest does not list, so a partial or foreign write can never feed an
// unverified value to a release step.
func verifyNoUnexpectedStoreFiles(dir string, expected map[string]struct{}) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inspect secret store delivery directory: %w", walkErr)
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("inspect secret store delivery directory: %w", err)
		}
		if relative == "." {
			return nil
		}
		base := filepath.Base(relative)
		if strings.HasPrefix(base, ".") {
			// The manifest itself and in-flight temporary files are dot-prefixed.
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if _, ok := expected[filepath.ToSlash(relative)]; !ok {
			return fmt.Errorf("secret store delivery directory contains unexpected file %q", relative)
		}
		return nil
	})
}

func readBoundedStoreFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0) // #nosec G304 -- path is assembled from validated manifest segments under the trusted delivery root.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > maxStoreValueBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxStoreValueBytes)
	}
	value, err := io.ReadAll(io.LimitReader(file, maxStoreValueBytes+1))
	if err != nil {
		return nil, err
	}
	if len(value) > maxStoreValueBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxStoreValueBytes)
	}
	return value, nil
}

func writeStoreFile(dir, name string, value []byte) error {
	temporary, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		if temporaryName != "" {
			_ = os.Remove(temporaryName)
		}
	}()
	if _, err := temporary.Write(value); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(storeSecretFileMode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filepath.Join(dir, name)); err != nil {
		return err
	}
	temporaryName = ""
	return nil
}

func validateSecretStorePayload(payload SecretStorePayload) error {
	if payload.Version != SecretStorePayloadVersion {
		return fmt.Errorf("secret store payload version %d is not supported", payload.Version)
	}
	if len(payload.Secrets) == 0 {
		return errors.New("secret store payload has no secrets")
	}
	seenNames := make(map[string]struct{}, len(payload.Secrets))
	files := 0
	total := 0
	for _, secret := range payload.Secrets {
		if !validStoreName(secret.Name) {
			return fmt.Errorf("secret store name %q is unsafe", secret.Name)
		}
		if _, duplicate := seenNames[secret.Name]; duplicate {
			return fmt.Errorf("secret store name %q is duplicated", secret.Name)
		}
		seenNames[secret.Name] = struct{}{}
		if len(secret.Keys) == 0 {
			return fmt.Errorf("secret store entry %q has no keys", secret.Name)
		}
		for key, value := range secret.Keys {
			if !validStoreKey(key) {
				return fmt.Errorf("secret store entry %q key %q is unsafe", secret.Name, key)
			}
			if len(value) == 0 {
				return fmt.Errorf("secret store entry %q key %q has an empty value", secret.Name, key)
			}
			if len(value) > maxStoreValueBytes {
				return fmt.Errorf("secret store entry %q key %q exceeds %d bytes", secret.Name, key, maxStoreValueBytes)
			}
			files++
			total += len(value)
		}
	}
	if files > maxStoreFiles {
		return fmt.Errorf("secret store payload has %d files, maximum is %d", files, maxStoreFiles)
	}
	if total > maxStoreTotalBytes {
		return fmt.Errorf("secret store payload exceeds %d bytes", maxStoreTotalBytes)
	}
	return nil
}

// validStoreName accepts the delivery directory grammar: a DNS-1123-style
// label extended with dots and underscores, never dot-led, and never a path
// escape.
func validStoreName(value string) bool {
	if value == "" || len(value) > maxStoreNameBytes {
		return false
	}
	for index, character := range []byte(value) {
		alphanumeric := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
		if index == 0 || index == len(value)-1 {
			if !alphanumeric {
				return false
			}
			continue
		}
		if !alphanumeric && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

// validStoreKey accepts Kubernetes configuration-key grammar without hidden
// files: dot-led names would collide with the manifest and temporary files.
func validStoreKey(value string) bool {
	if value == "" || len(value) > 253 || value[0] == '.' {
		return false
	}
	for _, character := range []byte(value) {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.'
		if !valid {
			return false
		}
	}
	return true
}
