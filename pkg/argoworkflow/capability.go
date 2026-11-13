package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Capability is operator-owned, never decoded from repository YAML. The
// operator owns this account's RBAC and namespace ResourceQuota outside Oberth.
type Capability struct {
	Namespace      string                   `json:"namespace"`
	ServiceAccount string                   `json:"service_account"`
	ResourceKinds  []ResourceKindCapability `json:"resource_kinds,omitempty"`
}

type ResourceKindCapability struct {
	APIVersion string   `json:"api_version"`
	Kind       string   `json:"kind"`
	Actions    []string `json:"actions"`
}

var (
	capabilityKindPattern        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}$`)
	resourceMetadataFieldPattern = regexp.MustCompile(`(?i)(annotations|labels)$`)
)

// CapabilityUsage includes defaults and inline templates, even unreachable
// ones. An unsupported nesting depth must never hide authority from admission.
func CapabilityUsage(wf *wfv1.Workflow) bool {
	var walk func(*wfv1.Template, int) bool
	walk = func(t *wfv1.Template, depth int) bool {
		if t == nil {
			return false
		}
		if depth > MaxIdentityWalkDepth {
			return true
		}
		if t.Resource != nil {
			return true
		}
		for _, call := range invocations(t) {
			if walk(call.inline, depth+1) {
				return true
			}
		}
		return false
	}
	if wf != nil {
		if walk(wf.Spec.TemplateDefaults, 0) {
			return true
		}
		for i := range wf.Spec.Templates {
			if walk(&wf.Spec.Templates[i], 0) {
				return true
			}
		}
	}
	return false
}

func (c *Capability) Validate() error {
	if c == nil {
		return nil
	}
	if c.Namespace == "" || len(k8svalidation.IsDNS1123Label(c.Namespace)) != 0 || c.ServiceAccount == "" || c.ServiceAccount == "default" || len(k8svalidation.IsDNS1123Subdomain(c.ServiceAccount)) != 0 {
		return errors.New("capability requires an explicit namespace and operator ServiceAccount")
	}
	if len(c.ResourceKinds) > 32 {
		return errors.New("capability allowlist exceeds 32 entries")
	}
	seen := map[string]bool{}
	for _, entry := range c.ResourceKinds {
		gv, err := schema.ParseGroupVersion(entry.APIVersion)
		key := entry.APIVersion + "/" + entry.Kind
		if err != nil || gv.Version == "" || len(k8svalidation.IsDNS1123Label(gv.Version)) != 0 || gv.Group != "" && len(k8svalidation.IsDNS1123Subdomain(gv.Group)) != 0 || !capabilityKindPattern.MatchString(entry.Kind) || strings.HasSuffix(entry.Kind, "List") || seen[key] {
			return errors.New("capability requires unique, static API version/kind pairs")
		}
		seen[key] = true
		if len(entry.Actions) == 0 || len(entry.Actions) > 4 {
			return errors.New("capability requires explicit resource actions")
		}
		actions := map[string]bool{}
		for _, action := range entry.Actions {
			if !slices.Contains([]string{"create", "get", "delete", "apply"}, action) || actions[action] {
				return errors.New("capability resource action is unsupported or repeated")
			}
			actions[action] = true
		}
	}
	return nil
}

func admitCapabilityResource(location string, r *wfv1.ResourceTemplate, capability *Capability) error {
	refuse := func(reason string) error { return fmt.Errorf("argoworkflow: %s: %s", location, reason) }
	if capability == nil {
		return refuse("resource access requires an operator capability grant")
	}
	if r.ManifestFrom != nil || len(r.Flags) != 0 || r.MergeStrategy != "" || len(r.Manifest) == 0 || len(r.Manifest) > 128<<10 {
		return refuse("resource requires one bounded inline manifest without flags or merge overrides")
	}
	// Only server-generated DNS/UID values can be substituted into an opaque
	// manifest. Arbitrary parameters could inject YAML and replace its envelope.
	static := strings.NewReplacer("{{workflow.name}}", "name", "{{workflow.namespace}}", "namespace", "{{workflow.uid}}", "uid").Replace(r.Manifest)
	// Closing JSON objects can contain adjacent braces; only an opening
	// template delimiter can begin an unapproved Argo substitution.
	if strings.Contains(static, "{{") {
		return refuse("resource manifest contains a dynamic expression outside the server-owned workflow identity")
	}

	decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(r.Manifest), 4096)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return refuse("invalid resource manifest")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return refuse("resource manifest must contain exactly one object")
	}
	// Strict conversion rejects duplicate keys. Decode only the public envelope;
	// the object's spec and status belong to its operator-provided API.
	encoded, err := yaml.YAMLToJSONStrict([]byte(r.Manifest))
	if err != nil {
		return refuse("resource manifest contains invalid or duplicate keys")
	}
	if !bytes.HasPrefix(bytes.TrimSpace(encoded), []byte("{")) {
		return refuse("resource manifest must be an object")
	}
	// Decode by exact keys. encoding/json's struct matching folds both ASCII
	// and Unicode (for example Kind and nameſpace), unlike Kubernetes.
	var object, metadata map[string]json.RawMessage
	if json.Unmarshal(encoded, &object) != nil || rejectEnvelopeCaseVariants(object, "apiVersion", "kind", "metadata") != nil || json.Unmarshal(object["metadata"], &metadata) != nil || rejectEnvelopeCaseVariants(metadata, "namespace", "name", "generateName") != nil {
		return refuse("invalid or case-variant resource envelope keys, Pod annotations or reserved labels")
	}
	var apiVersion, kind, namespace, name, generateName string
	var resourceObject any
	if json.Unmarshal(encoded, &resourceObject) != nil || resourcePriorityOverride(resourceObject) {
		return refuse("resource Pod metadata, patches and priority overrides are not admitted")
	}
	for _, field := range []struct {
		raw         json.RawMessage
		destination *string
	}{
		{object["apiVersion"], &apiVersion}, {object["kind"], &kind},
		{metadata["namespace"], &namespace}, {metadata["name"], &name}, {metadata["generateName"], &generateName},
	} {
		if len(field.raw) != 0 && json.Unmarshal(field.raw, field.destination) != nil {
			return refuse("invalid resource envelope")
		}
	}
	if namespace != "" && namespace != capability.Namespace {
		return refuse("resource namespace differs from the operator capability")
	}
	if name == "" && generateName == "" {
		return refuse("resource requires a name or generateName")
	}
	for _, entry := range capability.ResourceKinds {
		if entry.APIVersion == apiVersion && entry.Kind == kind && slices.Contains(entry.Actions, r.Action) {
			return nil
		}
	}
	return refuse("resource API version, kind or action is not granted")
}

// Operator resource grants do not delegate the control-plane scheduling tier.
// Check embedded Pod templates too, without interpreting the operator's API.
func resourcePriorityOverride(value any) bool {
	pending := []any{value}
	for len(pending) > 0 {
		value, pending = pending[len(pending)-1], pending[:len(pending)-1]
		switch node := value.(type) {
		case map[string]any:
			// A controller can copy metadata from any parent field, including
			// list entries. Inspect its shape at every depth, not its parent's
			// spelling or the granted resource kind.
			if resourceMetadataOverride(node, false) {
				return true
			}
			for key, child := range node {
				if strings.EqualFold(key, "metadata") || strings.EqualFold(key, "podMetadata") {
					metadata, object := child.(map[string]any)
					if child != nil && (!object || resourceMetadataOverride(metadata, true)) {
						return true
					}
				}
				if (strings.EqualFold(key, "priorityClassName") || strings.EqualFold(key, "podPriorityClassName") || strings.EqualFold(key, "podSpecPatch")) && child != nil && child != "" {
					return true
				}
				pending = append(pending, child)
			}
		case []any:
			pending = append(pending, node...)
		}
	}
	return false
}

// An opaque operator API may propagate its metadata onto a Pod. Apply the
// same boundary to metadata envelopes rather than guessing each granted kind.
func resourceMetadataOverride(metadata map[string]any, envelope bool) bool {
	for name, value := range metadata {
		// APIs also expose direct podAnnotations/podLabels-style maps.
		// This is independent of the resource kind and parent field name.
		// Kubernetes label selectors read Pod labels; they do not write
		// them. Native affinity may select even server-owned label keys.
		if strings.EqualFold(name, "matchLabels") {
			continue
		}
		field := resourceMetadataFieldPattern.FindString(name)
		if field == "" {
			continue
		}
		if value == nil {
			continue
		}
		entries, ok := value.(map[string]any)
		if !ok {
			// Opaque APIs also use arrays/scalars called labels, for example
			// metric labels. Only actual metadata envelopes require maps.
			if envelope {
				return true
			}
			continue
		}
		if strings.EqualFold(field, "annotations") && len(entries) != 0 {
			return true
		}
		labels := make(map[string]string, len(entries))
		for key, value := range entries {
			text, ok := value.(string)
			if !ok {
				return true
			}
			labels[key] = text
		}
		if len(admitPodMetadataKeys("resource metadata labels", labels)) != 0 {
			return true
		}
	}
	return false
}

// Unknown spec/status fields remain operator-defined. Only the envelope keys
// used for authorization have reserved spellings, including optional fields.
func rejectEnvelopeCaseVariants(object map[string]json.RawMessage, keys ...string) error {
	for key := range object {
		for _, exact := range keys {
			if key != exact && strings.EqualFold(key, exact) {
				return errors.New("case-variant resource envelope key")
			}
		}
	}
	return nil
}
