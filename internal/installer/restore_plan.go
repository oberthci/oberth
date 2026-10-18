package installer

// What an archive holds and what a restore changes on the target, said
// before anything changes (#068): the parts restored, every count that goes
// from the target's value to the archive's, and the secrets by name.

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// The parts of an archive a restore can apply (oberth restore --only).
const (
	RestorePartHistory = "history"
	RestorePartSecrets = "secrets"
)

// restoreParts are the parts a restore applies. History is the database
// (runs, issues, promotions, uplinks, the audit chain) with the
// compatibility values it needs; secrets are the OpenBao KV secrets an
// operator put in, the policies and roles, and the grants that let pipeline
// steps read them.
type restoreParts struct {
	History, Secrets bool
}

// resolveRestoreParts applies --only to what the archive holds: everything
// in it by default.
func resolveRestoreParts(only []string, scope string) (restoreParts, error) {
	full := scope != BackupScopeHistory
	if len(only) == 0 {
		return restoreParts{History: true, Secrets: full}, nil
	}
	var parts restoreParts
	for _, name := range only {
		switch strings.TrimSpace(name) {
		case RestorePartHistory:
			parts.History = true
		case RestorePartSecrets:
			if !full {
				return parts, errors.New("--only secrets: this archive holds history only (it was written by oberth backup --history-only)")
			}
			parts.Secrets = true
		case "":
		default:
			return parts, fmt.Errorf("--only %q: name history or secrets (comma-separated)", name)
		}
	}
	if parts == (restoreParts{}) {
		return parts, errors.New("--only names no part to restore: use history or secrets")
	}
	return parts, nil
}

func (parts restoreParts) names() []string {
	var names []string
	if parts.History {
		names = append(names, RestorePartHistory)
	}
	if parts.Secrets {
		names = append(names, RestorePartSecrets)
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
	scope := "history and secrets"
	if m.Scope == BackupScopeHistory {
		scope = "history only (no secrets)"
	}
	_, _ = fmt.Fprintf(w, "\nBackup %s\n", displayLocalPath(input))
	_, _ = fmt.Fprintf(w, "  Taken      %s from context %s (%s); holds %s\n",
		m.CreatedAt, displayOr(m.Source.Context, "?"), displayOr(m.Source.ServerVersion, "unknown server version"), scope)
	_, _ = fmt.Fprintf(w, "  SHA-256    %s\n", archive.CipherSHA)
	if recipients, err := ArchiveRecipients(input); err == nil {
		names := make([]string, 0, len(recipients))
		for _, recipient := range recipients {
			names = append(names, recipient.String())
		}
		_, _ = fmt.Fprintf(w, "  Key        encrypted to %s\n", strings.Join(names, "; "))
	}
	c := archive.Contents
	_, _ = fmt.Fprintf(w, "  History    %s\n", describeContents(c))
	if len(c.Repositories) > 0 {
		_, _ = fmt.Fprintf(w, "             repositories:\n             %s\n", listSome(c.Repositories, limit))
	}
	if len(c.Uplinks) > 0 {
		_, _ = fmt.Fprintf(w, "             uplinks:\n             %s\n", listSome(c.Uplinks, limit))
	}
	compat := m.Compatibility
	_, _ = fmt.Fprintf(w, "             opened under %s / %s / %s\n", compat.SchemaIdentity, compat.AuditDomain, compat.WitnessKeyInfo)
	if m.Scope == BackupScopeHistory {
		return
	}
	paths := make([]string, 0, len(archive.KV.Secrets))
	for _, secret := range archive.KV.Secrets {
		paths = append(paths, secret.Path)
	}
	_, _ = fmt.Fprintf(w, "  Secrets    %d KV secrets, %d policies, %d roles, %d grants (names only, never values)\n",
		len(paths), len(archive.Policies), len(archive.Roles.Roles), countGrants(archive.Grants))
	if len(paths) > 0 {
		_, _ = fmt.Fprintf(w, "             %s\n", listSome(paths, limit))
	}
	if archive.SkippedIdentities > 0 {
		_, _ = fmt.Fprintf(w, "             (%d install identities in this older backup are left out: every install keeps its own)\n", archive.SkippedIdentities)
	}
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
	case m.Source.ClusterUID != "" && target.ClusterUID != "" && m.Source.ClusterUID == target.ClusterUID:
		_, _ = fmt.Fprintf(w, "  Cluster    SAME cluster the backup was taken from: this rolls Oberth back to %s; runs created since are dropped\n", m.CreatedAt)
	case m.Source.ClusterUID != "" && target.ClusterUID != "":
		_, _ = fmt.Fprintln(w, "  Cluster    a different cluster than the backup's (reinstalled or new)")
	case m.Source.APIServer != "" && m.Source.APIServer == deps.apiServer:
		_, _ = fmt.Fprintf(w, "  Cluster    same API server URL as the backup (%s), possibly the same cluster: if so this rolls Oberth back to %s and runs created since are dropped\n",
			deps.apiServer, m.CreatedAt)
	case m.Source.APIServer != "" && deps.apiServer != "":
		_, _ = fmt.Fprintf(w, "  Cluster    different cluster: backup from %s, restoring onto %s\n", m.Source.APIServer, deps.apiServer)
	}

	_, _ = fmt.Fprintln(w, "\nWhat changes on this install:")
	if parts.History {
		printHistoryChanges(w, archive, target)
	}
	if parts.Secrets {
		printSecretChanges(w, archive, target)
	}
	var untouched []string
	if !parts.History {
		untouched = append(untouched, "history (runs, issues, uplinks)")
	}
	if !parts.Secrets {
		untouched = append(untouched, "OpenBao secrets, policies, roles and grants")
	}
	untouched = append(untouched, "this install's identities, CA and Helm values")
	_, _ = fmt.Fprintf(w, "  Unchanged  %s\n", joinWords(untouched))

	_, _ = fmt.Fprintln(w, "\nHow:")
	_, _ = fmt.Fprintf(w, "  Server     deploy/oberth stopped (%d -> 0) while restoring, then started again\n", target.Server.Replicas)
	if parts.History {
		_, _ = fmt.Fprintf(w, "  Database   %s on PVC %s replaced; this install's files are kept in /data/pre-restore-%s/\n",
			serverDatabasePath, target.Server.ClaimName, target.Timestamp)
		if target.Pod == "" {
			_, _ = fmt.Fprintln(w, "             (no server pod is running: the target schema is checked in the restore pod before the swap)")
		}
		if len(target.Overlay) > 0 {
			have, want := target.Server.Compatibility, m.Compatibility
			_, _ = fmt.Fprintf(w, "  Helm       compatibility values the database needs: schemaIdentity %s -> %s, auditDomain %s -> %s, witnessKeyInfo %s -> %s\n",
				have.SchemaIdentity, want.SchemaIdentity, have.AuditDomain, want.AuditDomain, have.WitnessKeyInfo, want.WitnessKeyInfo)
		}
		if len(target.Orphans) > 0 {
			names := make([]string, 0, len(target.Orphans))
			for _, wf := range target.Orphans {
				names = append(names, wf.Name)
			}
			_, _ = fmt.Fprintf(w, "  Workflows  delete %d whose runs the restored database does not know: %s\n", len(names), strings.Join(names, ", "))
		}
	}
	if parts.Secrets {
		_, _ = fmt.Fprintln(w, "  OpenBao    each secret, policy and role verified by read-back")
	}
	for _, note := range target.Notes {
		_, _ = fmt.Fprintf(w, "  Note       %s\n", note)
	}
}

func printHistoryChanges(w io.Writer, archive restoreArchive, target restoreTarget) {
	want := archive.Contents
	have := target.Contents
	if have == nil {
		_, _ = fmt.Fprintf(w, "  History    replaced with the backup's: %s\n", describeContents(want))
		_, _ = fmt.Fprintf(w, "             (this install's history could not be read: %s)\n", displayOr(target.ContentsNote, "the server is not running"))
		_, _ = fmt.Fprintln(w, "             uplinks and tokens become the backup's: a token this install printed stops working")
		return
	}
	_, _ = fmt.Fprintf(w, "  History    replaced, not merged: runs %d -> %d, issues %d -> %d (%d open), promotions %d -> %d, repositories %d -> %d\n",
		have.Runs, want.Runs, have.Issues, want.Issues, want.OpenIssues, have.Promotions, want.Promotions, len(have.Repositories), len(want.Repositories))
	gone, added := diffNames(have.Uplinks, want.Uplinks)
	if len(gone) == 0 && len(added) == 0 {
		_, _ = fmt.Fprintf(w, "             uplinks unchanged: %s\n", listSome(want.Uplinks, 8))
		return
	}
	if len(gone) > 0 {
		_, _ = fmt.Fprintf(w, "             uplinks removed: %s — their tokens, including the one this install printed, stop working\n", strings.Join(gone, ", "))
	}
	if len(added) > 0 {
		_, _ = fmt.Fprintf(w, "             uplinks restored: %s — their previous tokens work again\n", listSome(added, 8))
	}
}

func printSecretChanges(w io.Writer, archive restoreArchive, target restoreTarget) {
	plan := target.OpenBao
	grants := fmt.Sprintf("grants (ConfigMap %s) %d -> %d", secretAccessConfigMapName, target.Grants, countGrants(archive.Grants))
	if !archive.Grants.Present {
		grants = fmt.Sprintf("grants kept (%d; the backup has none)", target.Grants)
	}
	if !plan.Known {
		_, _ = fmt.Fprintf(w, "  Secrets    write %d secrets, %d policies, %d roles (no root token: this install was not compared); %s\n",
			len(archive.KV.Secrets), len(archive.Policies), len(archive.Roles.Roles), grants)
		return
	}
	_, _ = fmt.Fprintf(w, "  Secrets    %d new, %d replaced, %d unchanged\n", plan.SecretsNew, plan.SecretsReplaced, plan.SecretsUnchanged)
	if len(plan.ReplacedPaths) > 0 {
		_, _ = fmt.Fprintf(w, "             replaced: %s\n", strings.Join(plan.ReplacedPaths, ", "))
	}
	if len(plan.NewPaths) > 0 {
		_, _ = fmt.Fprintf(w, "             new: %s\n", listSome(plan.NewPaths, 8))
	}
	_, _ = fmt.Fprintf(w, "             policies %d new, %d replaced; roles %d new, %d replaced; %s\n",
		len(plan.PoliciesNew), len(plan.PoliciesReplaced), len(plan.RolesNew), len(plan.RolesReplaced), grants)
	if len(plan.PoliciesReplaced) > 0 {
		_, _ = fmt.Fprintf(w, "             replaced policies: %s\n", listSome(plan.PoliciesReplaced, 8))
	}
	if len(plan.RolesReplaced) > 0 {
		_, _ = fmt.Fprintf(w, "             replaced roles: %s\n", listSome(plan.RolesReplaced, 8))
	}
	printPinChanges(w, target.Pins, plan)
}

// printPinChanges says which restored secrets pin the backup's server
// (#074): paths, line counts and host names only, never values.
func printPinChanges(w io.Writer, pins serverPins, plan openBaoRestorePlan) {
	updated, current := splitPinRewrites(pins.Rewritten, plan)
	if len(updated) > 0 {
		_, _ = fmt.Fprintf(w, "             known_hosts pins of this server updated to its SSH host key: %s\n", describePinRewrites(updated))
	}
	if len(current) > 0 {
		_, _ = fmt.Fprintf(w, "             known_hosts pins of this server already current here: %s\n", describePinRewrites(current))
	}
	for _, left := range []struct {
		findings []pinFinding
		what     string
	}{
		{pins.Mixed, "known_hosts lines naming this server among other hosts, left as they are"},
		{pins.Loopback, "known_hosts lines for loopback names of this server, left as they are"},
		{pins.OldKey, "known_hosts lines pinning the backup's SSH host key under other names, left as they are"},
	} {
		if len(left.findings) > 0 {
			_, _ = fmt.Fprintf(w, "             %s: %s\n", left.what, describePinFindings(left.findings))
		}
	}
	if len(pins.OldCA) > 0 {
		_, _ = fmt.Fprintf(w, "             still hold the backup's server CA (update them by hand): %s\n", listSome(pins.OldCA, 8))
	}
	if pins.Note != "" && len(pins.Rewritten) == 0 {
		_, _ = fmt.Fprintf(w, "             known_hosts pins of this server: %s\n", pins.Note)
	}
}

func describePinRewrites(rewrites []pinRewrite) string {
	parts := make([]string, 0, len(rewrites))
	for _, rewrite := range rewrites {
		part := fmt.Sprintf("%s (%s", rewrite.Path, plural(rewrite.Lines, "line"))
		if len(rewrite.Hosts) > 0 {
			part += ": " + strings.Join(rewrite.Hosts, ", ")
		}
		if rewrite.Dropped > 0 {
			part += fmt.Sprintf("; %d duplicate dropped", rewrite.Dropped)
		}
		parts = append(parts, part+")")
	}
	return listSome(parts, 8)
}

func describePinFindings(findings []pinFinding) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		parts = append(parts, finding.String())
	}
	return listSome(parts, 8)
}

// splitPinRewrites separates the rewritten secrets this restore writes from
// those this install already holds as rewritten.
func splitPinRewrites(rewrites []pinRewrite, plan openBaoRestorePlan) (updated, current []pinRewrite) {
	for _, rewrite := range rewrites {
		if plan.Known && !slices.Contains(plan.NewPaths, rewrite.Path) && !slices.Contains(plan.ReplacedPaths, rewrite.Path) {
			current = append(current, rewrite)
		} else {
			updated = append(updated, rewrite)
		}
	}
	return updated, current
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
