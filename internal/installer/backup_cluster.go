package installer

// Cluster identity and step logs for `oberth backup` / `oberth restore`
// (#068): what a restore onto a reinstalled cluster must tell apart from a
// rollback of the same one, the service-account keys release Workload
// Identity Federation trusts, and the server's step logs.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/store"
)

// clusterIdentity is what tells one cluster from another and which
// service-account keys it signs and verifies with.
type clusterIdentity struct {
	UID          string
	KeyIDs       []string
	SigningKeyID string
}

// readClusterIdentity reads the kube-system namespace UID, the
// service-account JWKS key IDs and the key ID the API server signs new
// tokens with. Every part is best effort: an empty field means "unknown".
func readClusterIdentity(ctx context.Context, deps backupDeps, namespace string) clusterIdentity {
	var id clusterIdentity
	if ns, err := deps.KubeClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{}); err == nil {
		id.UID = string(ns.UID)
	}
	if out, err := deps.runStdout(ctx, nil, "kubectl", kubectlContextArgs(deps.ContextName, "get", "--raw", "/openid/v1/jwks")...); err == nil {
		var jwks struct {
			Keys []struct {
				KeyID string `json:"kid"`
			} `json:"keys"`
		}
		if json.Unmarshal(out, &jwks) == nil {
			for _, key := range jwks.Keys {
				if key.KeyID != "" {
					id.KeyIDs = append(id.KeyIDs, key.KeyID)
				}
			}
			slices.Sort(id.KeyIDs)
		}
	}
	id.SigningKeyID = signingKeyID(ctx, deps, namespace)
	if id.SigningKeyID == "" && len(id.KeyIDs) == 1 {
		id.SigningKeyID = id.KeyIDs[0]
	}
	return id
}

// signingKeyID requests a short-lived token for the namespace's default
// service account and returns the key ID in its header. The token itself is
// discarded unread beyond that header; TokenRequest persists nothing.
func signingKeyID(ctx context.Context, deps backupDeps, namespace string) string {
	expiry := int64(600)
	request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry}}
	response, err := deps.KubeClient.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, "default", request, metav1.CreateOptions{})
	if err != nil || response == nil {
		return ""
	}
	header, _, ok := strings.Cut(response.Status.Token, ".")
	response.Status.Token = ""
	if !ok {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return ""
	}
	var fields struct {
		KeyID string `json:"kid"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	return fields.KeyID
}

// serviceAccountKeyNote explains a service-account signing key that differs
// from the source's, which breaks release Workload Identity Federation: the
// cloud provider trusts the source's JWKS. Empty when nothing is wrong or
// nothing can be told.
func serviceAccountKeyNote(source backupSource, target clusterIdentity, releaseWIF bool) string {
	if !releaseWIF || len(source.ServiceAccountKeyIDs) == 0 || len(target.KeyIDs) == 0 {
		return ""
	}
	want := source.SigningKeyID
	switch {
	case want != "" && target.SigningKeyID == want:
		return ""
	case want == "" && slices.ContainsFunc(source.ServiceAccountKeyIDs, func(kid string) bool { return slices.Contains(target.KeyIDs, kid) }):
		return ""
	}
	have := target.SigningKeyID
	if have == "" {
		have = strings.Join(target.KeyIDs, ", ")
	}
	return fmt.Sprintf("release Workload Identity Federation trusts the source cluster's service-account key %s, but this cluster signs with %s: "+
		"releases cannot authenticate until this cluster signs with the old key again (k3s: docs/backup-restore.md, \"Keep the service-account key\") "+
		"or the provider trusts the new JWKS (kubectl get --raw /openid/v1/jwks)",
		displayOr(want, strings.Join(source.ServiceAccountKeyIDs, ", ")), have)
}

// --- Step logs ---

// serverLogsScript streams the server's step logs directory as a tar on
// stdout, or nothing when there is none. It refuses, before streaming, a
// directory larger than $1 KiB, so an oversized one never reaches this
// machine's memory. tar is not the last command, for the same reason as
// stageUploadScript (#067).
const serverLogsScript = `set -eu
cd /data
[ -d logs ] || exit 0
size=$(du -sk logs | cut -f1)
if [ "$size" -gt "$1" ]; then echo "step logs are $size KiB, above the $1 KiB bound" >&2; exit 3; fi
tar -cf - logs
true`

// maxBackupLogsBytes bounds the logs member.
const maxBackupLogsBytes = maxBackupMemberBytes

// exportServerLogs reads the step logs through the running server pod and
// re-packs them, keeping only the files a restore admits. Logs are history:
// when they cannot be read (too large, or a file vanished mid-read as a run
// finished), the backup goes on without them and says why.
func exportServerLogs(ctx context.Context, deps backupDeps, namespace, pod string) ([]byte, *backupLogsSummary, string, error) {
	limitKiB := strconv.Itoa(maxBackupLogsBytes >> 10)
	out, err := deps.runStdout(ctx, nil, "kubectl", kubectlExecArgs(deps.ContextName, namespace, pod, serverContainerName, false, "sh", "-c", serverLogsScript, "sh", limitKiB)...)
	if err != nil {
		return nil, nil, "could not read them: " + oneLine(err.Error()), nil
	}
	if len(out) == 0 {
		return nil, nil, "the server has none yet", nil
	}
	body, summary, err := repackLogsTar(out)
	if err != nil {
		return nil, nil, "could not read them: " + oneLine(err.Error()), nil
	}
	if summary.Files == 0 {
		return nil, nil, "the server has none yet", nil
	}
	return body, &summary, "", nil
}

func oneLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

// admittedLogName reports whether a tar entry is a log file a restore may
// write: a regular file directly in logs/, not hidden (the server's
// .index-*.tmp files are in-flight renames).
func admittedLogName(name string) bool {
	return path.Clean(name) == name && path.Dir(name) == "logs" && !strings.HasPrefix(path.Base(name), ".")
}

// repackLogsTar rewrites the pod's tar with only the admitted log files, as
// plain ustar entries, so the restore pod extracts exactly what Go checked.
func repackLogsTar(body []byte) ([]byte, backupLogsSummary, error) {
	var summary backupLogsSummary
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	if err := writer.WriteHeader(&tar.Header{Name: "logs/", Typeflag: tar.TypeDir, Mode: 0o750, Format: tar.FormatUSTAR}); err != nil {
		return nil, summary, err
	}
	reader := tar.NewReader(bytes.NewReader(body))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, summary, fmt.Errorf("read logs tar: %w", err)
		}
		if header.Typeflag != tar.TypeReg || !admittedLogName(header.Name) {
			continue
		}
		if err := writer.WriteHeader(&tar.Header{Name: header.Name, Typeflag: tar.TypeReg, Mode: 0o640, Size: header.Size,
			ModTime: header.ModTime, Format: tar.FormatUSTAR}); err != nil {
			continue // a name ustar cannot hold is not one the server writes
		}
		if _, err := io.CopyN(writer, reader, header.Size); err != nil {
			return nil, summary, fmt.Errorf("read logs tar: %w", err)
		}
		summary.Files++
		summary.Bytes += header.Size
	}
	if err := writer.Close(); err != nil {
		return nil, summary, err
	}
	return out.Bytes(), summary, nil
}

// validateLogsTar admits only the logs directory and log files directly in
// it — exactly what repackLogsTar writes — so a restore never extracts
// anything outside /data/logs.
func validateLogsTar(body []byte) (backupLogsSummary, error) {
	var summary backupLogsSummary
	reader := tar.NewReader(bytes.NewReader(body))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return summary, nil
		}
		if err != nil {
			return summary, fmt.Errorf("read logs tar: %w", err)
		}
		name := strings.TrimSuffix(header.Name, "/")
		switch {
		case name == "logs" && header.Typeflag == tar.TypeDir:
		case header.Typeflag == tar.TypeReg && admittedLogName(name):
			summary.Files++
			summary.Bytes += header.Size
		default:
			return summary, fmt.Errorf("unexpected entry %q in the logs tar", header.Name)
		}
	}
}

// logsRestoreScript extracts the staged logs tar into /data after checking
// its hash on the volume, then removes the staging directory. $1 is the
// expected SHA-256. Existing logs of the same name are replaced.
const logsRestoreScript = `set -eu
cd /data
printf '%s  %s\n' "$1" .oberth-restore/logs.tar | sha256sum -c - >/dev/null
mkdir -p logs
tar -xf .oberth-restore/logs.tar
rm -rf .oberth-restore
find logs -type f | wc -l`

// --- Database contents ---

// summarizeDatabaseArchive counts what a verified store database archive
// holds, opening a private copy read-only.
func summarizeDatabaseArchive(ctx context.Context, archivePath, work string, protocol store.ProtocolConfig) (store.ContentSummary, error) {
	db, err := extractDatabaseFiles(archivePath)
	if err != nil {
		return store.ContentSummary{}, err
	}
	path, err := writeRestoredDatabase(work, db)
	if err != nil {
		return store.ContentSummary{}, err
	}
	return summarizeDatabase(ctx, path, protocol)
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
