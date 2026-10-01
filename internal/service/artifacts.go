package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
)

type ArtifactStore interface {
	List(runID string) ([]artifacts.Entry, error)
	ReadAll(runID, name string) ([]byte, error)
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
}

func (response ArtifactResponse) ArtifactBytes() []byte { return []byte(response.Body) }

func (response ArtifactResponse) ArtifactName() string { return response.Name }

func (service *API) RunArtifacts(ctx context.Context, _ api.Actor, id string) (any, error) {
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

func (service *API) RunArtifact(ctx context.Context, _ api.Actor, id, name string, filter runlog.Filter) (any, error) {
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
	body, meta, err := runlog.FilterBytes(raw, filter)
	if err != nil {
		return nil, err
	}
	return ArtifactResponse{
		RunID: run, Name: name, Body: string(body),
		TotalLines: meta.TotalLines, MatchedLines: meta.MatchedLines,
		ReturnedLines: meta.ReturnedLines, Truncated: meta.Truncated, Bytes: meta.Bytes,
	}, nil
}

func (service *API) artifactRun(ctx context.Context, id string) (string, error) {
	if service.artifacts == nil || service.history == nil {
		return "", fmt.Errorf("%w: artifacts", ErrUnavailable)
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	run, err := service.history.Run(ctx, id)
	if err != nil {
		return "", err
	}
	return run.ID, nil
}
