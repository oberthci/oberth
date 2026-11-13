package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"regexp"
	"strings"
)

// SecretAccessGrantEntry is the database/API key for one secret approval.
type SecretAccessGrantEntry struct {
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Step     string `json:"step"`
	Secret   string `json:"secret"`
}

// grantEntryRepoPattern matches valid characters for the repo field of a grant
// entry. Repo names are qualified (upstream/org/repo) and are eventually
// interpolated into OpenBao policy HCL; quotes, backslashes, newlines, braces,
// and control characters would escape the HCL path string boundary and inject
// arbitrary policy rules (issue #416, same class as #411 and #414).
var grantEntryRepoPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// grantEntryStepPattern matches a DNS-1123 label: the grammar Argo Workflow
// template names must satisfy. The wildcard "*" is handled separately in
// ValidateGrantEntry before this pattern is tested.
var grantEntryStepPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidateGrantEntry checks an exact grant key before a database mutation.
func ValidateGrantEntry(index int, entry SecretAccessGrantEntry) error {
	if strings.TrimSpace(entry.Repo) == "" || strings.TrimSpace(entry.Step) == "" || strings.TrimSpace(entry.Secret) == "" {
		return fmt.Errorf("entry %d: repo, step, and secret are required", index)
	}
	if err := model.ValidateWorkflowFile(entry.Workflow); err != nil {
		return fmt.Errorf("entry %d: %w", index, err)
	}
	if !grantEntryRepoPattern.MatchString(entry.Repo) {
		return fmt.Errorf("entry %d: repo %q contains characters outside [A-Za-z0-9._/-]; refusing to persist (HCL injection risk)", index, entry.Repo)
	}
	if strings.ContainsAny(entry.Secret, "*?[]") {
		return fmt.Errorf("entry %d: wildcard and glob characters are not allowed in secret", index)
	}
	// Step must be either the wildcard "*" (grants access to all templates)
	// or a DNS-1123 label (grants access only to the named Argo template).
	// Per-step grants are enforced at admission: admitSecretstoreExecPaths
	// checks that each template's --path arguments have a wildcard grant or
	// a grant naming that exact template. Issue #623.
	if entry.Step != "*" {
		if len(entry.Step) > 63 || !grantEntryStepPattern.MatchString(entry.Step) {
			return fmt.Errorf("entry %d: step %q must be \"*\" (all templates) or a DNS-1123 label (lowercase alphanumeric and hyphens, 1-63 characters)", index, entry.Step)
		}
	}
	return nil
}

func canonicalGrantRepoKey(ctx context.Context, repo string, lookup func(context.Context, string) (model.Repository, error), qualify func(context.Context, int64) (string, error)) (string, error) {
	repository, err := lookup(ctx, repo)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) {
		return repo, nil
	}
	if err != nil {
		return "", err
	}
	return qualify(ctx, repository.ID)
}
