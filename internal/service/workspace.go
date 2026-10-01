package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const workspaceCleanupPageSize = 100

const workspaceCleanupTimeout = 5 * time.Second

type workspaceLifecycle struct {
	root   string
	owners WorkspaceCleanupStore
	remove func(string) error
}

func boundedWorkspaceCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), workspaceCleanupTimeout)
}

func newWorkspaceLifecycle(root string, owners WorkspaceCleanupStore, remove func(string) error) (*workspaceLifecycle, error) {
	if owners == nil {
		return nil, errors.New("service: workspace cleanup store is required")
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || filepath.Dir(root) == root {
		return nil, errors.New("service: workspace root must be an absolute non-root directory")
	}
	if remove == nil {
		remove = os.RemoveAll
	}
	return &workspaceLifecycle{root: root, owners: owners, remove: remove}, nil
}

func (lifecycle *workspaceLifecycle) cleanupRun(ctx context.Context, runID string) error {
	eligible, err := lifecycle.owners.RunWorkspaceCleanupEligible(ctx, runID)
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	path, err := runWorkspacePath(lifecycle.root, runID)
	if err != nil {
		return err
	}
	if err := lifecycle.remove(path); err != nil {
		return fmt.Errorf("remove terminal run workspace %s: %w", runID, err)
	}
	return nil
}

func (lifecycle *workspaceLifecycle) cleanupPromotion(ctx context.Context, promotionID string) error {
	eligible, err := lifecycle.owners.PromotionWorkspaceCleanupEligible(ctx, promotionID)
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	path, err := promotionWorkspacePath(lifecycle.root, promotionID)
	if err != nil {
		return err
	}
	if err := lifecycle.remove(path); err != nil {
		return fmt.Errorf("remove terminal promotion workspace %s: %w", promotionID, err)
	}
	return nil
}

// cleanupTerminalPromotionWorkspace keeps the already committed promotion
// outcome authoritative. A removal failure is reported through the append-only
// audit log and the returned terminal record, while bounded startup recovery
// retains responsibility for retrying the exact owner path.
func (service *API) cleanupTerminalPromotionWorkspace(ctx context.Context, promotion model.Promotion) model.Promotion {
	if service.workspaces == nil || !promotion.Status.Terminal() {
		return promotion
	}
	cleanupErr := service.workspaces.cleanupPromotion(ctx, promotion.ID)
	if cleanupErr == nil {
		return promotion
	}
	details, _ := json.Marshal(map[string]string{
		"owner": promotion.ID,
		"error": cleanupErr.Error(),
	})
	var auditErr error
	if service.auditor != nil {
		_, auditErr = service.auditor.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: promotion.Actor, Action: "workspace.cleanup.failed", ResourceType: "promotion",
			ResourceID: promotion.ID, Details: string(details),
		})
	}
	note := fmt.Sprintf("workspace cleanup pending: %v", cleanupErr)
	if auditErr != nil {
		note += fmt.Sprintf("; record cleanup failure: %v", auditErr)
	}
	if promotion.Error == "" {
		promotion.Error = note
	} else {
		promotion.Error += "; " + note
	}
	return promotion
}

func (lifecycle *workspaceLifecycle) recover(ctx context.Context) error {
	if err := lifecycle.recoverRuns(ctx); err != nil {
		return err
	}
	return lifecycle.recoverPromotions(ctx)
}

func (lifecycle *workspaceLifecycle) recoverRuns(ctx context.Context) error {
	afterID := ""
	for {
		ids, err := lifecycle.owners.RunWorkspaceCleanupCandidates(ctx, afterID, workspaceCleanupPageSize)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := lifecycle.cleanupRun(ctx, id); err != nil {
				return err
			}
		}
		if len(ids) < workspaceCleanupPageSize {
			return nil
		}
		afterID = ids[len(ids)-1]
	}
}

func (lifecycle *workspaceLifecycle) recoverPromotions(ctx context.Context) error {
	afterID := ""
	for {
		ids, err := lifecycle.owners.PromotionWorkspaceCleanupCandidates(ctx, afterID, workspaceCleanupPageSize)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := lifecycle.cleanupPromotion(ctx, id); err != nil {
				return err
			}
		}
		if len(ids) < workspaceCleanupPageSize {
			return nil
		}
		afterID = ids[len(ids)-1]
	}
}

func runWorkspacePath(root, runID string) (string, error) {
	return ownedWorkspacePath(root, runID, runID)
}

func promotionWorkspacePath(root, promotionID string) (string, error) {
	return ownedWorkspacePath(root, promotionID, "promotion-"+promotionID)
}

func ownedWorkspacePath(root, ownerID, name string) (string, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || filepath.Dir(root) == root {
		return "", errors.New("service: workspace root must be an absolute non-root directory")
	}
	if !validWorkspaceOwnerID(ownerID) {
		return "", errors.New("service: workspace owner ID is invalid")
	}
	path := filepath.Join(root, name)
	if path == root || !pathWithin(root, path) {
		return "", errors.New("service: workspace path escaped root")
	}
	return path, nil
}

func validWorkspaceOwnerID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'f') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}
