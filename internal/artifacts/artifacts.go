package artifacts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	ErrTooLarge = errors.New("artifacts: collection exceeds the configured limit")
	ErrRefused  = errors.New("artifacts: member refused")
)

type Entry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type Manifest struct {
	Entries []Entry `json:"entries"`
	Bytes   int64   `json:"bytes"`
}

type Store struct {
	directory string
}

func Open(directory string) (*Store, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("artifacts: directory is required")
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("artifacts: create directory: %w", err)
	}
	return &Store{directory: directory}, nil
}

type stagedMember struct {
	name string
	body []byte
	mode os.FileMode
}

// Extract judges the whole archive, then writes it under the run's directory.
// scanPatterns is the redaction gate (#208): every staged member is scanned
// with ScanReaderForSecrets before anything is written, and one match refuses
// the entire collection — fail-closed, never redact-in-place. Persisting call
// sites pass DefaultScanPatterns (or a superset); the parameter is part of the
// signature so a new call site cannot silently skip the gate.
func (store *Store) Extract(runID string, stream io.Reader, limit int64, scanPatterns []string) (Manifest, error) {
	runDirectory, err := store.runPath(runID)
	if err != nil {
		return Manifest{}, err
	}
	if limit <= 0 {
		return Manifest{}, errors.New("artifacts: limit must be positive")
	}

	staged, total, err := judge(stream, limit, scanPatterns)
	if err != nil {
		return Manifest{}, err
	}

	if err := os.RemoveAll(runDirectory); err != nil {
		return Manifest{}, fmt.Errorf("artifacts: replace previous collection: %w", err)
	}
	if len(staged) == 0 {
		return Manifest{}, nil
	}
	if err := os.MkdirAll(runDirectory, 0o750); err != nil {
		return Manifest{}, fmt.Errorf("artifacts: create run directory: %w", err)
	}

	manifest := Manifest{Bytes: total}
	for _, item := range staged {
		destination := filepath.Join(runDirectory, filepath.FromSlash(item.name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
			_ = os.RemoveAll(runDirectory)
			return Manifest{}, fmt.Errorf("artifacts: create %s: %w", item.name, err)
		}
		if err := os.WriteFile(destination, item.body, item.mode); err != nil {
			_ = os.RemoveAll(runDirectory)
			return Manifest{}, fmt.Errorf("artifacts: write %s: %w", item.name, err)
		}
		info, statErr := os.Stat(destination)
		modified := time.Time{}
		if statErr == nil {
			modified = info.ModTime()
		}
		manifest.Entries = append(manifest.Entries, Entry{
			Name: item.name, Size: int64(len(item.body)), Modified: modified,
		})
	}
	sort.Slice(manifest.Entries, func(i, j int) bool {
		return manifest.Entries[i].Name < manifest.Entries[j].Name
	})
	return manifest, nil
}

func judge(stream io.Reader, limit int64, scanPatterns []string) ([]stagedMember, int64, error) {
	decompressor, err := gzip.NewReader(stream)
	if err != nil {
		return nil, 0, fmt.Errorf("artifacts: read collection: %w", err)
	}
	defer func() { _ = decompressor.Close() }()

	reader := tar.NewReader(decompressor)
	var staged []stagedMember
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("artifacts: read collection: %w", err)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		name, err := safeMemberName(header.Name)
		if err != nil {
			return nil, 0, err
		}
		if header.Typeflag != tar.TypeReg {
			return nil, 0, fmt.Errorf("%w: %q is not a regular file", ErrRefused, header.Name)
		}
		if header.Linkname != "" {
			return nil, 0, fmt.Errorf("%w: %q carries a link target", ErrRefused, header.Name)
		}
		remaining := limit - total
		if remaining <= 0 {
			return nil, 0, fmt.Errorf("%w: over %d bytes", ErrTooLarge, limit)
		}
		body, err := io.ReadAll(io.LimitReader(reader, remaining+1))
		if err != nil {
			return nil, 0, fmt.Errorf("artifacts: read member %q: %w", header.Name, err)
		}
		total += int64(len(body))
		if total > limit {
			return nil, 0, fmt.Errorf("%w: over %d bytes", ErrTooLarge, limit)
		}
		// Redaction gate (#208): refuse the whole collection when any member
		// contains secret material. Runs before any write, and the error names
		// the member — never the matched content.
		if err := ScanReaderForSecrets(bytes.NewReader(body), scanPatterns); err != nil {
			return nil, 0, fmt.Errorf("%w in member %q", err, header.Name)
		}
		staged = append(staged, stagedMember{name: name, body: body, mode: 0o640})
	}
	return staged, total, nil
}

func safeMemberName(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: member has no name", ErrRefused)
	}
	if strings.ContainsAny(raw, "\x00\n\r") {
		return "", fmt.Errorf("%w: %q contains control characters", ErrRefused, raw)
	}
	if strings.Contains(raw, `\`) {
		return "", fmt.Errorf("%w: %q contains a backslash", ErrRefused, raw)
	}
	if strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("%w: %q is an absolute path", ErrRefused, raw)
	}
	cleaned := path.Clean(raw)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("%w: %q escapes the collection", ErrRefused, raw)
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", fmt.Errorf("%w: %q escapes the collection", ErrRefused, raw)
		}
	}
	return cleaned, nil
}

func (store *Store) List(runID string) ([]Entry, error) {
	runDirectory, err := store.runPath(runID)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	walkErr := filepath.Walk(runDirectory, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		relative, relErr := filepath.Rel(runDirectory, current)
		if relErr != nil {
			return relErr
		}
		entries = append(entries, Entry{
			Name: filepath.ToSlash(relative), Size: info.Size(), Modified: info.ModTime(),
		})
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return nil, fmt.Errorf("artifacts: list %s: %w", runID, walkErr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (store *Store) OpenArtifact(runID, name string) (io.ReadCloser, error) {
	resolved, err := store.artifactPath(runID, name)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(resolved) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("artifacts: open %s: %w", name, err)
	}
	return file, nil
}

func (store *Store) ReadAll(runID, name string) ([]byte, error) {
	file, err := store.OpenArtifact(runID, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(file)
}

func (store *Store) artifactPath(runID, name string) (string, error) {
	runDirectory, err := store.runPath(runID)
	if err != nil {
		return "", err
	}
	cleaned, err := safeMemberName(name)
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(runDirectory, filepath.FromSlash(cleaned))
	relative, err := filepath.Rel(runDirectory, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the collection", ErrRefused, name)
	}
	return resolved, nil
}

func (store *Store) runPath(runID string) (string, error) {
	if !safeID(runID) {
		return "", errors.New("artifacts: invalid run ID")
	}
	return filepath.Join(store.directory, runID), nil
}

func safeID(value string) bool {
	if value == "" || len(value) > 80 || strings.HasPrefix(value, ".") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}
