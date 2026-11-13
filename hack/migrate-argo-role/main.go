// migrate-argo-role transfers only the established Argo Role's rules ownership
// to Helm. Run explicitly before upgrading an older installation; never run as
// a chart hook or with the server's runtime ServiceAccount.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	rbacclient "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/tools/clientcmd"
)

const migrationManager = "oberth-argo-rules-migration"

type target struct {
	Namespace, Name, Release, ReleaseNamespace, CredentialedSA string
}

// Match the reviewed v0.17.9 rules and configured credentialed identity. Unknown custom permissions require
// separate review, not a forced ownership takeover.
func expectedRules(spec target) []rbacv1.PolicyRule {
	rules := []rbacv1.PolicyRule{}
	return append(rules,
		rbacv1.PolicyRule{APIGroups: []string{"argoproj.io"}, Resources: []string{"workflows"}, Verbs: []string{"create", "get", "list", "watch", "delete"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims"}, Verbs: []string{"create", "get", "list", "update", "delete"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create", "get", "list", "watch", "delete"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, ResourceNames: []string{spec.CredentialedSA}, Verbs: []string{"create"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"get"}},
	)
}

func validateTarget(spec target) error {
	if spec.Namespace == "" || spec.Name == "" || spec.Release == "" || spec.ReleaseNamespace == "" || spec.CredentialedSA == "" {
		return errors.New("namespace, role, release, release-namespace and credentialed-service-account must be nonempty")
	}
	return nil
}

type patchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func migrationPatch(role *rbacv1.Role, spec target) ([]byte, error) {
	if err := validateTarget(spec); err != nil {
		return nil, err
	}
	if role.APIVersion != rbacv1.SchemeGroupVersion.String() || role.Kind != "Role" || role.Namespace != spec.Namespace || role.Name != spec.Name || role.UID == "" || role.ResourceVersion == "" || role.DeletionTimestamp != nil {
		return nil, errors.New("role identity, version or lifecycle does not match the explicit target")
	}
	if role.Labels["app.kubernetes.io/managed-by"] != "Helm" || role.Annotations["meta.helm.sh/release-name"] != spec.Release || role.Annotations["meta.helm.sh/release-namespace"] != spec.ReleaseNamespace {
		return nil, errors.New("role does not belong to the specified Helm release")
	}
	if !reflect.DeepEqual(role.Rules, expectedRules(spec)) {
		return nil, errors.New("role rules differ from the reviewed live shape; review custom rules before migrating")
	}
	entries := role.DeepCopy().ManagedFields
	helmIndex, owners, changes := -1, 0, false
	kept := make([]metav1.ManagedFieldsEntry, 0, len(entries)+1)
	for _, entry := range entries {
		if entry.FieldsType != "FieldsV1" || entry.FieldsV1 == nil {
			return nil, errors.New("unrecognized managed fields encoding")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil || fields == nil {
			return nil, errors.New("invalid managed fields object")
		}
		helmApply := entry.Manager == "helm" && entry.Operation == metav1.ManagedFieldsOperationApply && entry.APIVersion == role.APIVersion && entry.Subresource == ""
		if rules, owns := fields["f:rules"]; owns {
			owners++
			var atomic map[string]json.RawMessage
			if err := json.Unmarshal(rules, &atomic); err != nil || atomic == nil || len(atomic) != 0 {
				return nil, errors.New("rules ownership is not the expected atomic field")
			}
			if entry.APIVersion != role.APIVersion || entry.Subresource != "" || (entry.Operation != metav1.ManagedFieldsOperationApply && entry.Operation != metav1.ManagedFieldsOperationUpdate) || (entry.Manager != "helm" && entry.Manager != "kubectl-patch" && entry.Manager != "kubectl-client-side-apply") {
				return nil, fmt.Errorf("unreviewed rules owner %q; no ownership changed", entry.Manager)
			}
			if !helmApply {
				delete(fields, "f:rules")
				changes = true
				if len(fields) == 0 {
					continue
				}
				encoded, err := json.Marshal(fields)
				if err != nil {
					return nil, err
				}
				entry.FieldsV1 = &metav1.FieldsV1{Raw: encoded}
			}
		}
		if helmApply {
			if helmIndex >= 0 {
				return nil, errors.New("duplicate Helm apply ownership entries")
			}
			helmIndex = len(kept)
		}
		kept = append(kept, entry)
	}
	if owners == 0 {
		return nil, errors.New("rules have no tracked owner; refusing to guess ownership")
	}
	if !changes {
		return nil, nil
	}
	if helmIndex < 0 {
		helmIndex = len(kept)
		kept = append(kept, metav1.ManagedFieldsEntry{Manager: "helm", Operation: metav1.ManagedFieldsOperationApply, APIVersion: role.APIVersion, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{}`)}})
	}
	var helmFields map[string]json.RawMessage
	if err := json.Unmarshal(kept[helmIndex].FieldsV1.Raw, &helmFields); err != nil {
		return nil, err
	}
	helmFields["f:rules"] = json.RawMessage(`{}`)
	encoded, err := json.Marshal(helmFields)
	if err != nil {
		return nil, err
	}
	kept[helmIndex].FieldsV1 = &metav1.FieldsV1{Raw: encoded}
	// Preconditions make this a single atomic, content-preserving transfer.
	// A stale read or recreated Role fails; there is no automatic force/retry.
	return json.Marshal([]patchOperation{
		{Op: "test", Path: "/metadata/uid", Value: role.UID},
		{Op: "test", Path: "/metadata/resourceVersion", Value: role.ResourceVersion},
		{Op: "test", Path: "/rules", Value: role.Rules},
		{Op: "test", Path: "/metadata/managedFields", Value: role.ManagedFields},
		{Op: "replace", Path: "/metadata/managedFields", Value: kept},
	})
}

func migrate(ctx context.Context, client rbacclient.RoleInterface, spec target, dryRun bool) (bool, error) {
	role, err := client.Get(ctx, spec.Name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("read Role: %w", err)
	}
	// Typed REST decoding clears TypeMeta; the typed Role endpoint fixes GVK.
	role.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("Role"))
	patch, err := migrationPatch(role, spec)
	if err != nil || len(patch) == 0 {
		return false, err
	}
	options := metav1.PatchOptions{FieldManager: migrationManager}
	if dryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}
	after, err := client.Patch(ctx, spec.Name, types.JSONPatchType, patch, options)
	if err != nil {
		return false, fmt.Errorf("atomic Role ownership transfer failed; inspect the current Role before retrying: %w", err)
	}
	after.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("Role"))
	beforeContent, afterContent := role.DeepCopy(), after.DeepCopy()
	beforeContent.ResourceVersion, afterContent.ResourceVersion = "", ""
	beforeContent.ManagedFields, afterContent.ManagedFields = nil, nil
	if !reflect.DeepEqual(beforeContent, afterContent) {
		return false, errors.New("server response changed Role identity or content; stop and inspect before upgrading")
	}
	var operations []struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(patch, &operations); err != nil {
		return false, err
	}
	var expected []metav1.ManagedFieldsEntry
	if err := json.Unmarshal(operations[len(operations)-1].Value, &expected); err != nil {
		return false, err
	}
	if !sameOwnership(expected, after.ManagedFields) {
		return false, errors.New("server response changed unrelated field ownership; stop and inspect before upgrading")
	}
	remaining, err := migrationPatch(after, spec)
	if err != nil {
		return false, fmt.Errorf("server response did not retain exclusive Helm rules ownership; stop and inspect before upgrading: %w", err)
	}
	if len(remaining) != 0 {
		return false, errors.New("server response still requires ownership migration; stop and inspect before upgrading")
	}
	return true, nil
}

// The API may reorder entries or update timestamps. Compare ownership itself,
// including all unrelated fields, rather than the serialized representation.
func sameOwnership(expected, actual []metav1.ManagedFieldsEntry) bool {
	if len(expected) != len(actual) {
		return false
	}
	used := make([]bool, len(actual))
	for _, want := range expected {
		found := false
		for i, got := range actual {
			if used[i] || want.Manager != got.Manager || want.Operation != got.Operation || want.APIVersion != got.APIVersion || want.FieldsType != got.FieldsType || want.Subresource != got.Subresource || want.FieldsV1 == nil || got.FieldsV1 == nil {
				continue
			}
			var wantFields, gotFields map[string]any
			if json.Unmarshal(want.FieldsV1.Raw, &wantFields) != nil || json.Unmarshal(got.FieldsV1.Raw, &gotFields) != nil || !reflect.DeepEqual(wantFields, gotFields) {
				continue
			}
			found, used[i] = true, true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("migrate-argo-role", flag.ContinueOnError)
	flags.SetOutput(output)
	var spec target
	flags.StringVar(&spec.Namespace, "namespace", "", "pipeline namespace containing the Role (required)")
	flags.StringVar(&spec.Name, "role", "", "exact Argo Role name (required)")
	flags.StringVar(&spec.Release, "release", "", "Helm release name (required)")
	flags.StringVar(&spec.ReleaseNamespace, "release-namespace", "", "Helm release namespace (required)")
	flags.StringVar(&spec.CredentialedSA, "credentialed-service-account", "oberth-argo-credentialed", "existing argo.credentialedServiceAccount value")
	kubeconfig := flags.String("kubeconfig", "", "kubeconfig path (default: normal kubeconfig loading rules)")
	kubeContext := flags.String("context", "", "explicit kubeconfig context (required)")
	dryRun := flags.Bool("dry-run", false, "validate the exact patch server-side without persisting it")
	timeout := flags.Duration("timeout", 30*time.Second, "total request deadline, at most 5m")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := validateTarget(spec); err != nil {
		return err
	}
	if *kubeContext == "" || *timeout <= 0 || *timeout > 5*time.Minute {
		return errors.New("explicit context and timeout in (0, 5m] required")
	}
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	loading.ExplicitPath = *kubeconfig
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &clientcmd.ConfigOverrides{CurrentContext: *kubeContext}).ClientConfig()
	if err != nil {
		return err
	}
	client, err := rbacclient.NewForConfig(config)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	changed, err := migrate(ctx, client.Roles(spec.Namespace), spec, *dryRun)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Role %s/%s: Helm rules ownership verified (transfer=%t, dry-run=%t); permissions unchanged\n", spec.Namespace, spec.Name, changed, *dryRun)
	return err
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
