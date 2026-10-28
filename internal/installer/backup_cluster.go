package installer

// The source cluster's identity and the database contents for `oberth
// backup` / `oberth restore` (#068): what tells a restore onto a reinstalled
// cluster from a rollback of the same one, and what history an archive or a
// target holds.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/store"
)

// clusterUID is the kube-system namespace UID, "" when it cannot be read.
// Unlike the API server URL (https://127.0.0.1:6443 on every k3s) it differs
// between a cluster and its reinstall.
func clusterUID(ctx context.Context, deps backupDeps) string {
	ns, err := deps.KubeClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return string(ns.UID)
}

// --- Database contents ---

// summarizeDatabaseArchive counts what a verified store database archive
// holds, opening a private copy read-only.
func summarizeDatabaseArchive(ctx context.Context, archivePath, work string, protocol store.ProtocolConfig, maxBytes int64) (store.ContentSummary, error) {
	db, err := extractDatabaseFiles(ctx, archivePath, work, maxBytes)
	if err != nil {
		return store.ContentSummary{}, err
	}
	return summarizeDatabase(ctx, db.Main, protocol)
}

func summarizeDatabase(ctx context.Context, path string, protocol store.ProtocolConfig) (summary store.ContentSummary, resultErr error) {
	inspection, err := store.InspectCurrent(ctx, path, store.Options{Protocol: protocol})
	if err != nil {
		return summary, fmt.Errorf("open database for its summary: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, inspection.Close()) }()
	return inspection.Summary(ctx)
}

func backupContentsOf(summary store.ContentSummary) *backupContents {
	contents := &backupContents{
		Runs: summary.Runs, Issues: summary.Issues, OpenIssues: summary.OpenIssues,
		Promotions: summary.Promotions, Repositories: len(summary.Repositories), Uplinks: len(summary.Uplinks),
	}
	if !summary.LastRunAt.IsZero() {
		contents.LastRunAt = summary.LastRunAt.UTC().Format(time.RFC3339)
	}
	return contents
}

// describeContents is the one-line history summary.
func describeContents(summary store.ContentSummary) string {
	text := fmt.Sprintf("%s, %s (%d open), %s, %s, %s",
		plural(summary.Runs, "run"), plural(summary.Issues, "issue"), summary.OpenIssues, plural(summary.Promotions, "promotion"),
		plural(len(summary.Repositories), "repository"), plural(len(summary.Uplinks), "uplink"))
	if !summary.LastRunAt.IsZero() {
		text += ", last run " + summary.LastRunAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	return text
}

// plural is "1 run", "2 runs", "1 repository", "2 repositories".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if strings.HasSuffix(word, "y") {
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(word, "y"))
	}
	return fmt.Sprintf("%d %ss", n, word)
}
