package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ssaObject identifies the Kubernetes object a server-side-apply conflict was
// reported for. Namespace is empty for cluster-scoped objects.
type ssaObject struct {
	Namespace  string // e.g. "oberth-argo"
	Name       string // e.g. "oberth-argo"
	APIVersion string // e.g. "rbac.authorization.k8s.io/v1" or "v1"
	Kind       string // e.g. "Role"
}

// ssaConflict describes one server-side-apply field-ownership conflict
// reported by Helm.
type ssaConflict struct {
	ssaObject
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
// preview uses whatever the current args produce (in Run, the identity
// preflight has already set cfg.watchTunnelOriginCACert for a pending or
// previously completed rotation, so the real field is exercised).
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
	conflicts := parseSSAConflicts(err.Error())
	if len(conflicts) == 0 {
		// Non-conflict Helm failure — propagate as-is so the caller can
		// diagnose. The preview is nonmutating, so this is safe.
		return fmt.Errorf("helm server-side preview failed (non-conflict): %w", err)
	}

	return fmt.Errorf("helm server-side preview detected SSA field-ownership conflicts; "+
		"refusing to proceed — no OpenBao writes, no Argo install, no cert rotation have been performed.\n\n%s",
		formatConflictRefusal(conflicts, ns))
}

// Helm 4 (v4.2.3) reports server-side-apply conflicts as one message per
// failed object, joined with " && ":
//
//	conflict occurred while applying object <ns>/<name> <group>/<version>, Kind=<Kind>:
//	Apply failed with 1 conflict: conflict with "<manager>" using <apiVersion>: <field>
//
// The core group renders as "/v1" (empty group). With several conflicting
// fields for one manager the API server lists them:
//
//	Apply failed with 2 conflicts: conflicts with "<manager>" using v1:
//	- .data.a
//	- .data.b
//
// Both shapes are parsed. The object header is optional so that a message
// without it still yields manager/field pairs (the refusal then cannot print
// a transfer command for that object and says so).
var (
	ssaObjectPattern  = regexp.MustCompile(`applying object (\S+) (\S*/\S+), Kind=([A-Za-z0-9]+):`)
	ssaManagerPattern = regexp.MustCompile(`conflicts? with "([^"]+)" using [^:\s]+:((?:\s*-\s+\S+)+|\s*\S+)`)
)

func parseSSAConflicts(helmError string) []ssaConflict {
	var conflicts []ssaConflict
	for _, segment := range strings.Split(helmError, " && ") {
		if !strings.Contains(segment, "conflict") {
			continue
		}
		var obj ssaObject
		if m := ssaObjectPattern.FindStringSubmatch(segment); m != nil {
			obj.Namespace, obj.Name = splitNamespacedName(m[1])
			obj.APIVersion = strings.TrimPrefix(m[2], "/")
			obj.Kind = m[3]
		}
		for _, m := range ssaManagerPattern.FindAllStringSubmatch(segment, -1) {
			for _, field := range splitConflictFields(m[2]) {
				conflicts = append(conflicts, ssaConflict{ssaObject: obj, Manager: m[1], Field: field})
			}
		}
	}
	return conflicts
}

// splitNamespacedName splits "<ns>/<name>" into its parts; a bare "<name>"
// (cluster-scoped object) yields an empty namespace.
func splitNamespacedName(ref string) (namespace, name string) {
	if idx := strings.Index(ref, "/"); idx >= 0 {
		return ref[:idx], ref[idx+1:]
	}
	return "", ref
}

// splitConflictFields turns the captured field text into individual field
// paths: either one field (" .data.ca.crt") or a dash list ("\n- .a\n- .b").
func splitConflictFields(captured string) []string {
	captured = strings.TrimSpace(captured)
	if captured == "" {
		return nil
	}
	if !strings.HasPrefix(captured, "-") {
		return []string{strings.Fields(captured)[0]}
	}
	var fields []string
	for _, item := range strings.Split(captured, "-") {
		if item = strings.TrimSpace(item); item != "" {
			fields = append(fields, strings.Fields(item)[0])
		}
	}
	return fields
}

// formatConflictRefusal renders the conflict list grouped by object and, for
// every object the message identified, the exact ownership-transfer command.
// The command re-applies the LIVE object (content unchanged) as field manager
// "helm" with --force-conflicts, in the object's OWN namespace — the oberth
// chart renders objects outside the release namespace (the oberth-argo Role),
// so the release namespace must not be assumed. defaultNamespace is used only
// when the message carried no namespace for a namespaced kind.
func formatConflictRefusal(conflicts []ssaConflict, defaultNamespace string) string {
	var b strings.Builder
	b.WriteString("SSA conflicts detected:\n")

	type objectConflicts struct {
		obj      ssaObject
		managers map[string]bool
		fields   []string
	}
	grouped := map[string]*objectConflicts{}
	var order []string

	for _, c := range conflicts {
		key := c.Namespace + "/" + c.Name + " " + c.Kind
		if c.Name == "" {
			key = "(unknown object)"
		}
		oc, ok := grouped[key]
		if !ok {
			oc = &objectConflicts{obj: c.ssaObject, managers: map[string]bool{}}
			grouped[key] = oc
			order = append(order, key)
		}
		oc.managers[c.Manager] = true
		oc.fields = append(oc.fields, c.Field)
	}

	for _, key := range order {
		oc := grouped[key]
		sort.Strings(oc.fields)
		managers := make([]string, 0, len(oc.managers))
		for m := range oc.managers {
			managers = append(managers, m)
		}
		sort.Strings(managers)
		fmt.Fprintf(&b, "  %s:\n", key)
		for _, f := range oc.fields {
			fmt.Fprintf(&b, "    field: %s\n", f)
		}
		fmt.Fprintf(&b, "    manager(s): %s\n", strings.Join(managers, ", "))
	}

	b.WriteString("\nTo transfer ownership to Helm (live content unchanged) and retry:\n")
	printed := 0
	for _, key := range order {
		oc := grouped[key]
		if oc.obj.Name == "" || oc.obj.Kind == "" {
			continue
		}
		ns := oc.obj.Namespace
		if ns == "" {
			ns = defaultNamespace
		}
		kind := strings.ToLower(oc.obj.Kind)
		fmt.Fprintf(&b, "  kubectl -n %s get %s %s -o yaml | kubectl -n %s apply --server-side --field-manager=helm --force-conflicts -f -\n",
			ns, kind, oc.obj.Name, ns)
		printed++
	}
	if printed == 0 {
		b.WriteString("  (the Helm message did not identify the object; inspect `kubectl get <kind> <name> --show-managed-fields -o yaml`\n" +
			"   and re-apply the live object with --server-side --field-manager=helm --force-conflicts)\n")
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
				"  2. Roll back to the last deployed revision: helm rollback oberth <revision> -n %s\n"+
				"  3. Re-run the installer. The identity preflight re-derives watchTunnel.originCACert\n"+
				"     from the OpenBao server leaf, so a pin that only lived in the failed attempt is\n"+
				"     restored without a values file.\n\n"+
				"the installer refuses to proceed on a failed latest revision to prevent "+
				"a half-applied upgrade from being silently compounded",
				namespace, namespace, namespace)
		}
	}

	return nil
}
