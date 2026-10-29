package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
)

type ArtifactStore interface {
	List(runID string) ([]artifacts.Entry, error)
	ReadAll(runID, name string) ([]byte, error)
	OpenArtifact(runID, name string) (io.ReadCloser, error)
}

type ArtifactListResponse struct {
	RunID     string            `json:"run_id"`
	Artifacts []artifacts.Entry `json:"artifacts"`
}

type ArtifactResponse struct {
	RunID         string `json:"run_id"`
	Name          string `json:"name"`
	Body          string `json:"body"`
	TotalLines    int    `json:"total_lines"`
	MatchedLines  int    `json:"matched_lines"`
	ReturnedLines int    `json:"returned_lines"`
	Truncated     bool   `json:"truncated"`
	Bytes         int64  `json:"bytes"`
	// SHA256 is the digest of the whole stored artifact, not of Body, so a
	// filtered or truncated view still names the exact object it came from.
	SHA256 string `json:"sha256"`
}

func (service *API) RunArtifacts(ctx context.Context, actor api.Actor, id string) (any, error) {
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	run, err := service.artifactRun(ctx, id)
	if err != nil {
		return nil, err
	}
	entries, err := service.artifacts.List(run)
	if err != nil {
		return nil, fmt.Errorf("list run artifacts: %w", err)
	}
	if entries == nil {
		entries = []artifacts.Entry{}
	}
	return ArtifactListResponse{RunID: run, Artifacts: entries}, nil
}

func (service *API) RunArtifact(ctx context.Context, actor api.Actor, id, name string, filter runlog.Filter) (any, error) {
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	run, err := service.artifactRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: artifact name is required", ErrInvalidInput)
	}
	raw, err := service.artifacts.ReadAll(run, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: artifact %q", store.ErrNotFound, name)
		}
		return nil, fmt.Errorf("read run artifact: %w", err)
	}
	digest := sha256.Sum256(raw)
	sum := hex.EncodeToString(digest[:])
	if inlineBinary(raw) {
		// A JSON text channel cannot carry these bytes exactly, and a
		// line-filtered, size-bounded view of an executable is never what the
		// caller wants. Refuse loudly and point at the raw route (#1011).
		return nil, fmt.Errorf("%w: artifact %q is binary (%d bytes, sha256 %s); artifact_get returns text only. "+
			"Download the exact bytes with `oberth mcp call artifact_get --output FILE`, "+
			"`oberth artifacts --output FILE %s %s`, or GET /api/runs/%s/artifacts/%s",
			ErrInvalidInput, name, len(raw), sum, run, name, run, name)
	}
	body, meta, err := runlog.FilterBytes(raw, filter)
	if err != nil {
		return nil, err
	}
	return ArtifactResponse{
		RunID: run, Name: name, Body: string(body),
		TotalLines: meta.TotalLines, MatchedLines: meta.MatchedLines,
		ReturnedLines: meta.ReturnedLines, Truncated: meta.Truncated, Bytes: meta.Bytes,
		SHA256: sum,
	}, nil
}

// inlineBinary reports content the inline text view cannot represent byte for
// byte: a NUL byte or invalid UTF-8, which JSON encoding would replace.
func inlineBinary(raw []byte) bool {
	return bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw)
}

// OpenRunArtifact opens one retained artifact for a raw, unbounded download.
// It applies the same authorization and path checks as RunArtifact but no
// text-view filter or ceiling: the collection limit already bounded the
// object when it was stored. The digest is computed over the open descriptor
// and the descriptor is rewound, so the header and the body describe the same
// bytes even if the name is evicted mid-download.
func (service *API) OpenRunArtifact(ctx context.Context, actor api.Actor, id, name string) (api.ArtifactObject, error) {
	service, err := service.forActor(ctx, actor)
	if err != nil {
		return api.ArtifactObject{}, err
	}
	run, err := service.artifactRun(ctx, id)
	if err != nil {
		return api.ArtifactObject{}, err
	}
	if strings.TrimSpace(name) == "" {
		return api.ArtifactObject{}, fmt.Errorf("%w: artifact name is required", ErrInvalidInput)
	}
	content, err := service.artifacts.OpenArtifact(run, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return api.ArtifactObject{}, fmt.Errorf("%w: artifact %q", store.ErrNotFound, name)
		}
		return api.ArtifactObject{}, fmt.Errorf("open run artifact: %w", err)
	}
	object, err := describeArtifact(name, content)
	if err != nil {
		_ = content.Close()
		return api.ArtifactObject{}, err
	}
	return object, nil
}

func describeArtifact(name string, content io.ReadCloser) (api.ArtifactObject, error) {
	file, ok := content.(interface {
		io.ReadSeeker
		Stat() (os.FileInfo, error)
	})
	if !ok {
		return api.ArtifactObject{}, errors.New("open run artifact: store returned an unseekable stream")
	}
	info, err := file.Stat()
	if err != nil {
		return api.ArtifactObject{}, fmt.Errorf("stat run artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return api.ArtifactObject{}, fmt.Errorf("open run artifact: %q is not a regular file", name)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return api.ArtifactObject{}, fmt.Errorf("hash run artifact: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return api.ArtifactObject{}, fmt.Errorf("rewind run artifact: %w", err)
	}
	return api.ArtifactObject{Name: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), Content: content}, nil
}

func (service *API) artifactRun(ctx context.Context, id string) (string, error) {
	if service.artifacts == nil || service.history == nil {
		return "", refuse(ErrUnavailable, "artifacts")
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	run, err := service.resolveReadRunID(ctx, id)
	if err != nil {
		return "", err
	}
	if err := service.purgedEvidence(ctx, run, "artifacts"); err != nil {
		return "", err
	}
	return run.ID, nil
}
