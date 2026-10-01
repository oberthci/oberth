package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type ArtifactCollector interface {
	Collect(ctx context.Context, workflowName string) ([]byte, error)
}

type ArtifactStore interface {
	Extract(runID string, stream io.Reader, limit int64) error
	Evict(budget int64) ([]string, error)
}

func (jobs *ArgoJobs) SetArtifacts(collector ArtifactCollector, store ArtifactStore, limit, budget int64) {
	jobs.collector = collector
	jobs.artifacts = store
	jobs.artifactLimit = limit
	jobs.artifactBudget = budget
}

func (jobs *ArgoJobs) collectArtifacts(ctx context.Context, workflowName, runID string, trigger periapsis.Trigger) string {
	if jobs.collector == nil || jobs.artifacts == nil {
		return ""
	}
	// Tier gate (#208): only CI-trigger runs persist artifacts. Credentialed
	// tiers (release, plan, apply) hold secret material under the memory-only
	// contract; a release step that wrote a credential into OBERTH_ARTIFACTS
	// must never see it copied onto the server's /data PVC. The skip is
	// silent by design — it is static policy, not a per-run failure.
	if !artifacts.TierGated(string(trigger)) {
		return ""
	}
	archive, err := jobs.collector.Collect(ctx, workflowName)
	if err != nil {
		return fmt.Sprintf("collect: %v", err)
	}
	if len(archive) == 0 {
		return ""
	}
	if err := jobs.artifacts.Extract(runID, bytes.NewReader(archive), jobs.artifactLimit); err != nil {
		return fmt.Sprintf("store: %v", err)
	}
	details := map[string]any{"bytes": len(archive), "stored": true}
	if evicted, err := jobs.artifacts.Evict(jobs.artifactBudget); err == nil && len(evicted) != 0 {
		details["evicted"] = evicted
	}
	jobs.auditArtifacts(ctx, runID, details)
	return ""
}

func (jobs *ArgoJobs) auditArtifacts(ctx context.Context, runID string, details map[string]any) {
	if jobs.auditor == nil {
		return
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return
	}
	_, _ = jobs.auditor.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "oberth", Action: "ci.argo.artifacts",
		ResourceType: "run", ResourceID: runID, Details: string(encoded),
	})
}

func (jobs *ArgoJobs) reportArtifactFailure(runID, reason string) {
	jobs.mu.Lock()
	if jobs.artifactFailures == nil {
		jobs.artifactFailures = map[string]string{}
	}
	jobs.artifactFailures[runID] = reason
	jobs.mu.Unlock()
}

func (jobs *ArgoJobs) recordArtifactFailure(ctx context.Context, runID, reason string) {
	jobs.reportArtifactFailure(runID, reason)
	jobs.auditArtifacts(ctx, runID, map[string]any{"stored": false, "reason": reason})
}

func (jobs *ArgoJobs) ArtifactFailure(runID string) string {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	return jobs.artifactFailures[runID]
}
