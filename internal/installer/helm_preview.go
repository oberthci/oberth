package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ssaConflict describes one server-side-apply field-ownership conflict
// reported by Helm.
type ssaConflict struct {
	Object  string // e.g. "oberth/cloudflared-watch-oberth-origin-ca ConfigMap"
	Manager string // e.g. "kubectl-client-side-apply"
	Field   string // e.g. ".data.ca.crt"
}

// previewHelmUpgrade runs a nonmutating Helm server-side preview with the
// same arguments the real upgrade will use, WITHOUT --take-ownership. When
// the preview reports SSA field-ownership conflicts, the installer refuses
// with the object/field/manager list and the exact ownership-transfer command
// so the operator can resolve it before re-running. The preview catches
// conflicts before any OpenBao write, Argo install, or cert rotation.
//
// The sentinelOriginCACert parameter carries a placeholder origin CA cert
// that differs from the live value, so the preview exercises .data.ca.crt
// on the ConfigMap the same way the real upgrade would. When empty, the
// preview uses whatever the current args produce.
func previewHelmUpgrade(ctx context.Context, cfg Config, deps Deps, openbao OpenBaoResult, sentinelOriginCACert string) error {
	if deps.RunHelm == nil {
		return nil
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}

	// Only preview when upgrading an existing release.
	_, exists := findHelmRelease(ctx, deps, "oberth", ns)
	if !exists {
		return nil // fresh install — no existing objects to conflict with
	}

	// Build the same args the real upgrade will use, plus the preview flags.
	previewCfg := cfg
	if sentinelOriginCACert != "" {
		previewCfg.watchTunnelOriginCACert = sentinelOriginCACert
	}
	args := append(OberthHelmArgs(previewCfg, openbao, RekorResult{}),
		"--dry-run=server", "--no-hooks", "--output=json")

	raw, err := deps.RunHelm(ctx, args)
	if err == nil {
		// Preview succeeded — no conflicts.
		clear(raw)
		return nil
	}

	// Parse SSA conflicts from the error.
	errStr := err.Error()
	conflicts := parseSSAConflicts(errStr)
	if len(conflicts) == 0 {
		// Non-conflict Helm failure — propagate as-is so the caller can
		// diagnose. The preview is nonmutating, so this is safe.
		return fmt.Errorf("helm server-side preview failed (non-conflict): %w", err)
	}

	return fmt.Errorf("helm server-side preview detected SSA field-ownership conflicts; "+
		"refusing to proceed — no OpenBao writes, no Argo install, no cert rotation have been performed.\n\n%s",
		formatConflictRefusal(conflicts, ns))
}

// ssaConflictPattern matches Helm's SSA conflict messages. Helm 4 (v4.2.3)
// formats them as: conflict with "<manager>" using <apiVersion>: <field>
// preceded by the object reference.
var ssaConflictPattern = regexp.MustCompile(
	`(?:(\S+/\S+\s+\S+))?\s*:?\s*conflict with "([^"]+)" using [^:]+:\s*(\S+)`)

func parseSSAConflicts(helmError string) []ssaConflict {
	var conflicts []ssaConflict
	for _, line := range strings.Split(helmError, "\n") {
		// Also try a broader match on individual conflict lines.
		if !strings.Contains(line, "conflict") {
			continue
		}
		matches := ssaConflictPattern.FindAllStringSubmatch(line, -1)
		for _, m := range matches {
			c := ssaConflict{
				Object:  strings.TrimSpace(m[1]),
				Manager: m[2],
				Field:   m[3],
			}
			if c.Manager != "" && c.Field != "" {
				conflicts = append(conflicts, c)
			}
		}
	}
	return conflicts
}

func formatConflictRefusal(conflicts []ssaConflict, namespace string) string {
	var b strings.Builder
	b.WriteString("SSA conflicts detected:\n")

	// Group by object for readability.
	type objectConflicts struct {
		object   string
		managers map[string]bool
		fields   []string
	}
	grouped := map[string]*objectConflicts{}
	var order []string

	for _, c := range conflicts {
		key := c.Object
		if key == "" {
			key = "(unknown object)"
		}
		oc, ok := grouped[key]
		if !ok {
			oc = &objectConflicts{object: key, managers: map[string]bool{}}
			grouped[key] = oc
			order = append(order, key)
		}
		oc.managers[c.Manager] = true
		oc.fields = append(oc.fields, c.Field)
	}

	for _, key := range order {
		oc := grouped[key]
		sort.Strings(oc.fields)
		var managers []string
		for m := range oc.managers {
			managers = append(managers, m)
		}
		sort.Strings(managers)
		fmt.Fprintf(&b, "  %s:\n", oc.object)
		for _, f := range oc.fields {
			fmt.Fprintf(&b, "    field: %s\n", f)
		}
		fmt.Fprintf(&b, "    manager(s): %s\n", strings.Join(managers, ", "))
	}

	b.WriteString("\nTo transfer ownership to Helm and retry:\n")
	for _, key := range order {
		oc := grouped[key]
		// Extract kind and name from the object key.
		parts := strings.Fields(key)
		if len(parts) >= 2 {
			nsName := parts[0] // e.g. "oberth/cloudflared-watch-oberth-origin-ca"
			kind := strings.ToLower(parts[1])
			name := nsName
			if idx := strings.Index(nsName, "/"); idx >= 0 {
				name = nsName[idx+1:]
			}
			for m := range oc.managers {
				fmt.Fprintf(&b, "  kubectl -n %s get %s %s -o yaml | kubectl -n %s apply --server-side --field-manager=helm --force-conflicts -f -\n",
					namespace, kind, name, namespace)
				_ = m // manager info is for the diagnostic, not the command
				break // one command per object suffices
			}
		}
	}

	return b.String()
}

// detectFailedHelmRevision checks whether the latest Helm revision for the
// named release is in "failed" status. A failed latest revision means
// --reuse-values will pull values from the PREVIOUS deployed revision, not the
// failed one — the same values the failed upgrade already tried. Continuing
// without acknowledging this risks repeating the failure or silently dropping
// values that were present only in the failed attempt.
func detectFailedHelmRevision(ctx context.Context, deps Deps, namespace string) error {
	if deps.RunHelm == nil {
		return nil
	}

	// `helm list -a` includes all statuses (deployed, failed, etc).
	out, err := deps.RunHelm(ctx, []string{"list", "-n", namespace, "-a", "-o", "json"})
	if err != nil {
		return nil // cannot list — fresh install path handles this
	}

	var releases []helmRelease
	if json.Unmarshal(out, &releases) != nil {
		return nil
	}

	for _, r := range releases {
		if r.Name == "oberth" && r.Status == "failed" {
			return fmt.Errorf("the latest Helm revision for release \"oberth\" in namespace %q is "+
				"in \"failed\" status; --reuse-values will take values from the previous deployed "+
				"revision, not the failed one. This can silently drop values that were pinned only "+
				"in the failed attempt (e.g. a rotated originCACert).\n\n"+
				"To resolve:\n"+
				"  1. Inspect the failed revision: helm history oberth -n %s\n"+
				"  2. Roll back if needed: helm rollback oberth <revision> -n %s\n"+
				"  3. Or proceed with explicit values: oberth install --upgrade --values <values-file>\n\n"+
				"the installer refuses to proceed on a failed latest revision to prevent "+
				"a half-applied upgrade from being silently compounded",
				namespace, namespace, namespace)
		}
	}

	return nil
}
