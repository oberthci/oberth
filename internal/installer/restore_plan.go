package installer

// What an archive holds and what a restore changes on the target, said
// before anything changes (#068): the parts restored, every count that goes
// from the target's value to the archive's, the secrets by name, and each
// setting from its current value to the restored one.

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// The parts of an archive a restore can apply (oberth restore --only).
const (
	RestorePartHistory  = "history"
	RestorePartSecrets  = "secrets"
	RestorePartSettings = "settings"
)

// restoreParts are the parts a restore applies. History is the database
// (runs, issues, promotions, uplinks, the audit chain), the step logs and the
// compatibility values the database needs; secrets are OpenBao KV secrets,
// policies and roles; settings are the grants table, release WIF and the
// certificate names.
type restoreParts struct {
	History, Secrets, Settings bool
}

// resolveRestoreParts applies --only to what the archive holds: everything
// in it by default.
func resolveRestoreParts(only []string, scope string) (restoreParts, error) {
	full := scope != BackupScopeHistory
	if len(only) == 0 {
		return restoreParts{History: true, Secrets: full, Settings: full}, nil
	}
	var parts restoreParts
	for _, name := range only {
		switch strings.TrimSpace(name) {
		case RestorePartHistory:
			parts.History = true
		case RestorePartSecrets, RestorePartSettings:
			if !full {
				return parts, fmt.Errorf("--only %s: this archive holds history only (it was written by oberth backup --history-only)", name)
			}
			if name == RestorePartSecrets {
				parts.Secrets = true
			} else {
				parts.Settings = true
			}
		case "":
		default:
			return parts, fmt.Errorf("--only %q: name history, secrets or settings (comma-separated)", name)
		}
	}
	if parts == (restoreParts{}) {
		return parts, errors.New("--only names no part to restore: use history, secrets or settings")
	}
	return parts, nil
}

func (parts restoreParts) names() []string {
	var names []string
	for _, part := range []struct {
		on   bool
		name string
	}{{parts.History, RestorePartHistory}, {parts.Secrets, RestorePartSecrets}, {parts.Settings, RestorePartSettings}} {
		if part.on {
			names = append(names, part.name)
		}
	}
	return names
}

func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return "nothing"
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// listSome shows up to limit names, then how many more there are, wrapped
// to lines of about 100 columns under a 13-column indent.
func listSome(names []string, limit int) string {
	shown := names
	more := ""
	if len(names) > limit {
		shown, more = names[:limit], fmt.Sprintf(" and %d more", len(names)-limit)
	}
	var lines []string
	line := ""
	for i, name := range shown {
		item := name
		if i < len(shown)-1 {
			item += ","
		}
		if line != "" && len(line)+1+len(item) > 100 {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += item
	}
	lines = append(lines, line+more)
	return strings.Join(lines, "\n             ")
}

// --- Archive contents (also oberth restore --inspect) ---

func printArchiveContents(w io.Writer, archive restoreArchive, input string, all bool) {
	m := archive.Manifest
	limit := 6
	if all {
		limit = 1 << 20
	}
	scope := "history, secrets and settings"
	if m.Scope == BackupScopeHistory {
		scope = "history only (no secrets or settings)"
	}
	_, _ = fmt.Fprintf(w, "\nBackup %s\n", displayLocalPath(input))
	_, _ = fmt.Fprintf(w, "  Taken      %s from context %s (%s); holds %s\n",
		m.CreatedAt, displayOr(m.Source.Context, "?"), displayOr(m.Source.ServerVersion, "unknown server version"), scope)
	_, _ = fmt.Fprintf(w, "  SHA-256    %s\n", archive.CipherSHA)
	c := archive.Contents
	_, _ = fmt.Fprintf(w, "  History    %s\n", describeContents(c))
	if len(c.Repositories) > 0 {
		_, _ = fmt.Fprintf(w, "             repositories:\n             %s\n", listSome(c.Repositories, limit))
	}
	if len(c.Uplinks) > 0 {
		_, _ = fmt.Fprintf(w, "             uplinks:\n             %s\n", listSome(c.Uplinks, limit))
	}
	if m.Logs != nil {
		_, _ = fmt.Fprintf(w, "             step logs: %d files, %d KiB\n", m.Logs.Files, m.Logs.Bytes>>10)
	} else {
		_, _ = fmt.Fprintln(w, "             step logs: none")
	}
	if m.Scope == BackupScopeHistory {
		return
	}
	paths := make([]string, 0, len(archive.KV.Secrets))
	for _, secret := range archive.KV.Secrets {
		paths = append(paths, secret.Path)
	}
	_, _ = fmt.Fprintf(w, "  Secrets    %d KV secrets, %d policies, %d roles (names only, never values)\n", len(paths), len(archive.Policies), len(archive.Roles.Roles))
	if len(paths) > 0 {
		_, _ = fmt.Fprintf(w, "             %s\n", listSome(paths, limit))
	}
	for i, line := range settingsSummary(archive) {
		label := "             "
		if i == 0 {
			label = "  Settings   "
		}
		_, _ = fmt.Fprintf(w, "%s%s\n", label, line)
	}
}

// settingsSummary describes the settings an archive records.
func settingsSummary(archive restoreArchive) []string {
	compat := archive.Manifest.Compatibility
	lines := []string{fmt.Sprintf("compatibility %s / %s / %s", compat.SchemaIdentity, compat.AuditDomain, compat.WitnessKeyInfo)}
	if hasReleaseWIF(archive.Values) {
		lines = append(lines, "release WIF configured")
	}
	tls, _ := archive.Values["tls"].(map[string]any)
	if names := append(stringList(tls["extraDNSNames"]), stringList(tls["extraIPs"])...); len(names) > 0 {
		lines = append(lines, "certificate names "+strings.Join(names, ", "))
	}
	if archive.Grants.Present {
		lines = append(lines, fmt.Sprintf("%d grants", countGrants(archive.Grants)))
	}
	if own := operatorValues(archive.Values); len(own) > 0 {
		lines = append(lines, "your other Helm values (not restored; oberth backup export-values): "+listSome(own, 12))
	}
	return lines
}

// operatorValues lists the leaf value paths an archive records that are
// neither installer-managed nor applied by a restore: the operator's own
// values-file settings.
func operatorValues(values map[string]any) []string {
	return unappliedValues(values, nil)
}

// --- The plan: what changes on this install ---

func printRestorePlan(w io.Writer, cfg RestoreConfig, deps backupDeps, archive restoreArchive, target restoreTarget, parts restoreParts) {
	m := archive.Manifest
	_, _ = fmt.Fprintf(w, "\nRestore onto context %s (namespace %s, OpenBao %s, Argo %s): %s\n",
		deps.ContextName, cfg.Namespace, cfg.OpenBaoNamespace, cfg.ArgoNamespace, joinWords(parts.names()))
	// Same cluster means a rollback: every run created after the backup
	// disappears and its workflows are deleted below. The API server URL
	// alone cannot tell (every k3s serves https://127.0.0.1:6443); the
	// kube-system namespace UID can (#068).
	switch {
	case m.Source.ClusterUID != "" && target.Cluster.UID != "" && m.Source.ClusterUID == target.Cluster.UID:
		_, _ = fmt.Fprintf(w, "  Cluster    SAME cluster the backup was taken from: this rolls Oberth back to %s; runs created since are dropped\n", m.CreatedAt)
	case m.Source.ClusterUID != "" && target.Cluster.UID != "":
		_, _ = fmt.Fprintln(w, "  Cluster    a different cluster than the backup's (reinstalled or new)")
	case m.Source.APIServer != "" && m.Source.APIServer == deps.apiServer:
		_, _ = fmt.Fprintf(w, "  Cluster    same API server URL as the backup (%s), possibly the same cluster: if so this rolls Oberth back to %s and runs created since are dropped\n",
			deps.apiServer, m.CreatedAt)
	case m.Source.APIServer != "" && deps.apiServer != "":
		_, _ = fmt.Fprintf(w, "  Cluster    different cluster: backup from %s, restoring onto %s\n", m.Source.APIServer, deps.apiServer)
	}

	_, _ = fmt.Fprintln(w, "\nWhat changes on this install:")
	if parts.History {
		printHistoryChanges(w, cfg, archive, target)
	}
	if parts.Secrets {
		printSecretChanges(w, cfg, archive, target)
	}
	if parts.Settings || len(target.Overlay) > 0 {
		printSettingChanges(w, archive, target, parts)
	}
	var untouched []string
	if !parts.History {
		untouched = append(untouched, "history (runs, issues, uplinks, step logs)")
	}
	if !parts.Secrets {
		untouched = append(untouched, "OpenBao secrets, policies and roles")
	}
	if !parts.Settings {
		untouched = append(untouched, "settings (grants, release WIF, certificate names)")
	}
	if len(untouched) > 0 {
		_, _ = fmt.Fprintf(w, "  Unchanged  %s\n", joinWords(untouched))
	}

	_, _ = fmt.Fprintln(w, "\nHow:")
	_, _ = fmt.Fprintf(w, "  Server     deploy/oberth stopped (%d -> 0) while restoring, then started again\n", target.Server.Replicas)
	if parts.History {
		_, _ = fmt.Fprintf(w, "  Database   %s on PVC %s replaced; this install's files are kept in /data/pre-restore-%s/\n",
			serverDatabasePath, target.Server.ClaimName, target.Timestamp)
		if target.Pod == "" {
			_, _ = fmt.Fprintln(w, "             (no server pod is running: the target schema is checked in the restore pod before the swap)")
		}
		if len(target.Orphans) > 0 {
			names := make([]string, 0, len(target.Orphans))
			for _, wf := range target.Orphans {
				names = append(names, wf.Name)
			}
			_, _ = fmt.Fprintf(w, "  Workflows  delete %d whose runs the restored database does not know: %s\n", len(names), strings.Join(names, ", "))
		}
	}
	if len(target.Overlay) > 0 {
		_, _ = fmt.Fprintln(w, "  Helm       helm upgrade --reuse-values of release oberth with the settings above")
	}
	if parts.Secrets {
		_, _ = fmt.Fprintln(w, "  OpenBao    each secret, policy and role verified by read-back; Transit key, PKI CA and auth config are not restored")
	}
	for _, note := range target.Notes {
		_, _ = fmt.Fprintf(w, "  Note       %s\n", note)
	}
}

func printHistoryChanges(w io.Writer, cfg RestoreConfig, archive restoreArchive, target restoreTarget) {
	want := archive.Contents
	if have := target.Contents; have != nil {
		_, _ = fmt.Fprintf(w, "  History    replaced, not merged: runs %d -> %d, issues %d -> %d (%d open), promotions %d -> %d, repositories %d -> %d\n",
			have.Runs, want.Runs, have.Issues, want.Issues, want.OpenIssues, have.Promotions, want.Promotions, len(have.Repositories), len(want.Repositories))
		gone, added := diffNames(have.Uplinks, want.Uplinks)
		switch {
		case len(gone) == 0 && len(added) == 0:
			_, _ = fmt.Fprintf(w, "             uplinks unchanged: %s\n", listSome(want.Uplinks, 8))
		default:
			if len(gone) > 0 {
				_, _ = fmt.Fprintf(w, "             uplinks removed: %s — their tokens, including the one this install printed, stop working\n", strings.Join(gone, ", "))
			}
			if len(added) > 0 {
				_, _ = fmt.Fprintf(w, "             uplinks restored: %s — their previous tokens work again\n", listSome(added, 8))
			}
		}
	} else {
		_, _ = fmt.Fprintf(w, "  History    replaced with the archive's: %s\n", describeContents(want))
		_, _ = fmt.Fprintf(w, "             (this install's history could not be read: %s)\n", displayOr(target.ContentsNote, "the server is not running"))
		_, _ = fmt.Fprintln(w, "             uplinks and tokens become the archive's: a token this install printed stops working")
	}
	switch {
	case archive.Logs == nil:
		_, _ = fmt.Fprintln(w, "             step logs: none in the archive")
	case cfg.NoLogs:
		_, _ = fmt.Fprintln(w, "             step logs: not restored (--no-logs)")
	default:
		_, _ = fmt.Fprintf(w, "             step logs: %d files into /data/logs (same names replaced)\n", archive.Manifest.Logs.Files)
	}
}

func printSecretChanges(w io.Writer, cfg RestoreConfig, archive restoreArchive, target restoreTarget) {
	plan := target.OpenBao
	if !plan.Known {
		_, _ = fmt.Fprintf(w, "  Secrets    write %d secrets, %d policies, %d roles (no root token: this install was not compared)\n",
			len(target.KV.Secrets), len(archive.Policies), len(archive.Roles.Roles))
		return
	}
	_, _ = fmt.Fprintf(w, "  Secrets    %d new, %d replaced, %d unchanged\n", plan.SecretsNew, plan.SecretsReplaced, plan.SecretsUnchanged)
	if len(plan.ReplacedPaths) > 0 {
		_, _ = fmt.Fprintf(w, "             replaced: %s\n", strings.Join(plan.ReplacedPaths, ", "))
	}
	if len(plan.NewPaths) > 0 {
		_, _ = fmt.Fprintf(w, "             new: %s\n", listSome(plan.NewPaths, 8))
	}
	_, _ = fmt.Fprintf(w, "             policies %d new, %d replaced; roles %d new, %d replaced\n",
		len(plan.PoliciesNew), len(plan.PoliciesReplaced), len(plan.RolesNew), len(plan.RolesReplaced))
	if len(plan.PoliciesReplaced) > 0 {
		_, _ = fmt.Fprintf(w, "             replaced policies: %s\n", listSome(plan.PoliciesReplaced, 8))
	}
	if len(plan.RolesReplaced) > 0 {
		_, _ = fmt.Fprintf(w, "             replaced roles: %s\n", listSome(plan.RolesReplaced, 8))
	}
	for _, line := range identityPlanLines(target.Identity, cfg.Namespace) {
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
}

func printSettingChanges(w io.Writer, archive restoreArchive, target restoreTarget, parts restoreParts) {
	var lines []string
	if _, ok := target.Overlay["compatibility"]; ok {
		have, want := target.Server.Compatibility, archive.Manifest.Compatibility
		lines = append(lines, fmt.Sprintf("compatibility (the restored database needs it): schemaIdentity %s -> %s, auditDomain %s -> %s, witnessKeyInfo %s -> %s",
			have.SchemaIdentity, want.SchemaIdentity, have.AuditDomain, want.AuditDomain, have.WitnessKeyInfo, want.WitnessKeyInfo))
	}
	if argo, ok := target.Overlay["argo"].(map[string]any); ok && argo["releaseWIF"] != nil {
		if hasReleaseWIF(target.Values) {
			lines = append(lines, "argo.releaseWIF: replaced with the archive's")
		} else {
			lines = append(lines, "argo.releaseWIF: not set -> the archive's")
		}
	}
	if tls, ok := target.Overlay["tls"].(map[string]any); ok {
		targetTLS, _ := target.Values["tls"].(map[string]any)
		for _, key := range []string{"extraDNSNames", "extraIPs"} {
			if want, ok := tls[key]; ok {
				lines = append(lines, fmt.Sprintf("tls.%s: [%s] -> [%s]", key, strings.Join(stringList(targetTLS[key]), ", "), strings.Join(valueStrings(want), ", ")))
			}
		}
	}
	if parts.Settings && archive.Grants.Present {
		lines = append(lines, fmt.Sprintf("grants (ConfigMap %s): %d -> %d", secretAccessConfigMapName, target.Grants, countGrants(archive.Grants)))
	}
	if len(lines) == 0 {
		lines = []string{"already match this install's"}
	}
	for i, line := range lines {
		label := "             "
		if i == 0 {
			label = "  Settings   "
		}
		_, _ = fmt.Fprintf(w, "%s%s\n", label, line)
	}
}

// diffNames returns the names only in have and only in want.
func diffNames(have, want []string) (gone, added []string) {
	for _, name := range have {
		if !slices.Contains(want, name) {
			gone = append(gone, name)
		}
	}
	for _, name := range want {
		if !slices.Contains(have, name) {
			added = append(added, name)
		}
	}
	return gone, added
}
