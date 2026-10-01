package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// migrateV12SecretAccessQualifiedNames migrates the secret_access table's
// repo column from bare names to qualified "upstream/org/repo" names.
// This prevents same-name repos under different upstreams from aliasing
// each other's grants (#245 BLOCKER B).
//
// The migration is idempotent: already-qualified rows (containing "/")
// are left untouched.
func migrateV12SecretAccessQualifiedNames(ctx context.Context, db *sql.DB, _ func() time.Time) error {
	// Build the bare -> qualified name mapping from the current repo+upstream state.
	qualifiedNames := make(map[string]string)
	rows, err := db.QueryContext(ctx, `
		SELECT r.name, u.name, u.base_url
		FROM repositories r
		JOIN upstreams u ON u.id = r.upstream_id`)
	if err != nil {
		return fmt.Errorf("migration v12: read repo-upstream mapping: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var repoName, upstreamName, baseURL string
		if err := rows.Scan(&repoName, &upstreamName, &baseURL); err != nil {
			return fmt.Errorf("migration v12: scan repo-upstream row: %w", err)
		}
		org := v12OrgFromBaseURL(baseURL)
		if org == "" {
			org = upstreamName
		}
		qualified := upstreamName + "/" + org + "/" + repoName
		// If two repos have the same bare name, only the first wins for
		// bare-name migration. This is correct: the second repo will have
		// its grants created with qualified names going forward.
		if _, exists := qualifiedNames[repoName]; !exists {
			qualifiedNames[repoName] = qualified
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migration v12: read repo-upstream mapping: %w", err)
	}

	// Update secret_access rows that still use bare names.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration v12: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	saRows, err := tx.QueryContext(ctx, `SELECT id, repo FROM secret_access`)
	if err != nil {
		return fmt.Errorf("migration v12: read secret_access: %w", err)
	}
	defer func() { _ = saRows.Close() }()
	type saRow struct {
		id   int64
		repo string
	}
	var toUpdate []saRow
	for saRows.Next() {
		var r saRow
		if err := saRows.Scan(&r.id, &r.repo); err != nil {
			return fmt.Errorf("migration v12: scan secret_access: %w", err)
		}
		// Skip rows that are already qualified (contain "/").
		if strings.Contains(r.repo, "/") {
			continue
		}
		if _, ok := qualifiedNames[r.repo]; ok {
			toUpdate = append(toUpdate, r)
		}
	}
	if err := saRows.Err(); err != nil {
		return fmt.Errorf("migration v12: iterate secret_access: %w", err)
	}

	for _, r := range toUpdate {
		qualified := qualifiedNames[r.repo]
		if _, err := tx.ExecContext(ctx, `UPDATE secret_access SET repo = ? WHERE id = ?`, qualified, r.id); err != nil {
			return fmt.Errorf("migration v12: update secret_access row %d: %w", r.id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration v12: commit: %w", err)
	}
	return nil
}

func v12OrgFromBaseURL(baseURL string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	if filepath.IsAbs(base) {
		return filepath.Base(base)
	}
	parts := strings.Split(base, "/")
	return parts[len(parts)-1]
}
