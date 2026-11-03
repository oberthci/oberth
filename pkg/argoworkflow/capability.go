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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Capability is operator-owned, never decoded from repository YAML. The
// operator owns this account's RBAC and namespace ResourceQuota outside Oberth.
type Capability struct {
	Namespace         string                   `json:"namespace"`
	ServiceAccount    string                   `json:"service_account"`
	ResourceKinds     []ResourceKindCapability `json:"resource_kinds,omitempty"`
	ExtendedResources map[string]int64         `json:"extended_resources,omitempty"`
}

type ResourceKindCapability struct {
	APIVersion string   `json:"api_version"`
	Kind       string   `json:"kind"`
	Actions    []string `json:"actions"`
}

var capabilityKindPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}$`)

// CapabilityUsage includes defaults and inline templates, even unreachable
// ones. An unsupported nesting depth must never hide authority from admission.
func CapabilityUsage(wf *wfv1.Workflow) (resources, extended bool) {
	check := func(r corev1.ResourceRequirements) {
		for _, list := range []corev1.ResourceList{r.Requests, r.Limits} {
			for name := range list {
				if name != corev1.ResourceCPU && name != corev1.ResourceMemory && name != corev1.ResourceEphemeralStorage {
					extended = true
				}
			}
		}
	}
	var walk func(*wfv1.Template, int)
	walk = func(t *wfv1.Template, depth int) {
		if t == nil {
			return
		}
		if depth > MaxIdentityWalkDepth {
			resources, extended = true, true
			return
		}
		resources = resources || t.Resource != nil
		if t.Container != nil {
			check(t.Container.Resources)
		}
		if t.Script != nil {
			check(t.Script.Resources)
		}
		if t.ContainerSet != nil {
			for _, c := range t.ContainerSet.Containers {
				check(c.Resources)
			}
		}
		for _, group := range [][]wfv1.UserContainer{t.InitContainers, t.Sidecars} {
			for _, c := range group {
				check(c.Resources)
			}
		}
		for _, call := range invocations(t) {
			walk(call.inline, depth+1)
		}
	}
	if wf != nil {
		walk(wf.Spec.TemplateDefaults, 0)
		for i := range wf.Spec.Templates {
			walk(&wf.Spec.Templates[i], 0)
		}
	}
	return resources, extended
}

func validateExtendedPeak(wf *wfv1.Workflow, c *Capability) error {
	_, extended := CapabilityUsage(wf)
	if !extended {
		return nil
	}
	d, err := DeclaredCapabilityPeak(wf)
	if err != nil || d == nil || c == nil {
		return errors.New("argoworkflow: extended resources require an operator grant and complete bounded resource declarations")
	}
	for name, count := range d.ExtendedResources {
		if count > c.ExtendedResources[name] {
			return fmt.Errorf("argoworkflow: extended resource %s peak exceeds the operator grant", name)
		}
	}
	return nil
}

func (c *Capability) Validate() error {
	if c == nil {
		return nil
	}
	if c.Namespace == "" || len(k8svalidation.IsDNS1123Label(c.Namespace)) != 0 || c.ServiceAccount == "" || c.ServiceAccount == "default" || len(k8svalidation.IsDNS1123Subdomain(c.ServiceAccount)) != 0 {
		return errors.New("capability requires an explicit namespace and operator ServiceAccount")
	}
	if len(c.ResourceKinds) > 32 || len(c.ExtendedResources) > 32 {
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
	return ValidateExtendedResourceLimits(c.ExtendedResources)
}

// ValidateExtendedResourceLimits validates operator-owned integer capacity.
func ValidateExtendedResourceLimits(limits map[string]int64) error {
	if len(limits) > 32 {
		return errors.New("extended-resource capacity exceeds 32 entries")
	}
	for name, limit := range limits {
		parts := strings.Split(name, "/")
		if len(parts) != 2 || parts[0] == "" || len(k8svalidation.IsQualifiedName(name)) != 0 || parts[0] == "kubernetes.io" || strings.HasSuffix(parts[0], ".kubernetes.io") || limit < 1 || limit > 1_000_000 {
			return errors.New("capability requires qualified extended-resource names and integer ceilings in 1..1000000")
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
		return refuse("invalid or case-variant resource envelope keys")
	}
	var apiVersion, kind, namespace, name, generateName string
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

func admitExtendedResources(location string, resources corev1.ResourceRequirements, c *Capability) []error {
	var problems []error
	for _, list := range []corev1.ResourceList{resources.Requests, resources.Limits} {
		for name := range list {
			if name == corev1.ResourceCPU || name == corev1.ResourceMemory || name == corev1.ResourceEphemeralStorage {
				continue
			}
			request, requested := resources.Requests[name]
			limit, limited := resources.Limits[name]
			value, integer := request.AsInt64()
			bound := int64(0)
			if c != nil {
				bound = c.ExtendedResources[string(name)]
			}
			if !requested || !limited || !integer || value <= 0 || bound == 0 || value > bound || request.Cmp(limit) != 0 {
				problems = append(problems, fmt.Errorf("argoworkflow: %s extended resource %q requires equal positive integer requests/limits within its operator grant", location, name))
			}
		}
	}
	return problems
}
