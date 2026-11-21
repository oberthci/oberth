package argoworkflow

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"regexp"
	"slices"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (c *Capability) validate() bool {
	if c == nil {
		return true
	}
	if len(validation.IsDNS1123Label(c.Namespace)) > 0 || len(validation.IsDNS1123Subdomain(c.ServiceAccount)) > 0 || c.ServiceAccount == "default" {
		return false
	}
	seen := map[string]bool{}
	for _, kind := range c.ResourceKinds {
		key := kind.APIVersion + "/" + kind.Kind
		if !validResourceKind(kind) || seen[key] {
			return false
		}
		seen[key] = true
	}

	return true
}

// resourceStops refuses an oversized manifest or a release resource step,
// which end the inspection, and a step shape no capability can grant.
func (g *graph) resourceStops(n *node) bool {
	switch {
	case len(n.template.Resource.Manifest) > 128<<10:
		g.refuse(n.path+".resource.manifest", "manifest_size")
		return true
	case g.policy.Release:
		g.refuse(n.path+".resource", "release_resource")
		return true
	case !g.resourceShape(n):
		g.refuse(n.path+".resource", "capability")
	}
	return false
}
func (g *graph) resource(n *node) {
	r := n.template.Resource
	if g.resourceStops(n) {
		return
	}
	obj, ok := resourceEnvelope(r, g.request.IdentityRequest.Namespace)
	if !ok {
		g.refuse(n.path+".resource.manifest", "resource")
		return
	}
	metadata, _ := obj["metadata"].(map[string]any)
	namespace := stringField(metadata, "namespace")
	if namespace == "" {
		g.refuse(n.path+".resource.manifest.metadata.namespace", "resource_namespace")
		return
	}
	kind := ResourceKind{APIVersion: stringField(obj, "apiVersion"), Kind: stringField(obj, "kind"), Actions: []string{r.Action}}
	if !g.resourceKind(n.path+".resource", kind) {
		return
	}
	where := n.path + ".resource"
	if c := g.policy.Capability; c != nil && namespace != c.Namespace {
		where += ".manifest.metadata.namespace"
	}
	if len(validation.IsDNS1123Label(namespace)) > 0 {
		g.refuse(where, "capability")
	} else if c := g.policy.Capability; c == nil || namespace != c.Namespace || !g.resourceMatch(obj, n) {
		g.missingCapability(where, namespace, kind)
	}
}
func (g *graph) resourceShape(n *node) bool {
	return n.scope == "" && !projectsToken(n.template) && !g.policy.Release && !resourceInputs(n.template)
}
func (g *graph) resourceKind(path string, kind ResourceKind) bool {
	code := ""
	switch {
	case !validResourceKind(kind):
		code = "capability"
	case clusterScopedKind(kind):
		code = "cluster_kind"
	case argoWorkloadKind(kind):
		code = "workload_kind"
	}
	if code != "" {
		g.refuse(path, code)
	}
	return code == ""
}
func resourceEnvelope(r *wfv1.ResourceTemplate, namespace string) (map[string]any, bool) {
	if !inlineManifest(r) {
		return nil, false
	}
	// Unknown name and UID values cannot stand in for a known granted namespace.
	text := strings.NewReplacer("{{workflow.name}}", "WORKFLOW_NAME", "{{workflow.namespace}}", namespace, "{{workflow.uid}}", "WORKFLOW_UID").Replace(r.Manifest)
	// Runtime data must fill a double-quoted string value, preserving YAML type.
	var document yamlv3.Node
	if yamlv3.Unmarshal([]byte(text), &document) != nil || resourceDataExpressions(&document) != strings.Count(text, "{{") {
		return nil, false
	}
	raw, err := oneDocument([]byte(text))
	if err != nil {
		return nil, false
	}
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil || !exactKeys(object, []string{"apiVersion", "kind", "metadata"}) {
		return nil, false
	}
	meta, ok := object["metadata"].(map[string]any)
	if !ok || !exactKeys(meta, []string{"namespace", "name", "generateName"}) {
		return nil, false
	}
	if !stringFields(object, []string{"apiVersion", "kind"}) || !stringFields(meta, []string{"name", "generateName", "namespace"}) {
		return nil, false
	}
	return object, envelopeNames(object, meta)
}
func resourceDataExpressions(node *yamlv3.Node) int {
	count := 0
	if strings.Contains(node.Value, "{{") {
		encoded := regexp.MustCompile(`^{{=sprig\.b64enc\(inputs\.parameters\['[A-Za-z0-9_-]+'\]\)}}$`)
		if node.Kind != yamlv3.ScalarNode || node.Style != yamlv3.DoubleQuotedStyle || node.Tag != "!!str" || !encoded.MatchString(node.Value) {
			return -1
		}
		count++
	}
	for i, child := range node.Content {
		if node.Kind == yamlv3.MappingNode && i%2 == 0 && strings.Contains(child.Value, "{{") {
			return -1
		}
		n := resourceDataExpressions(child)
		if n < 0 {
			return -1
		}
		count += n
	}
	return count
}

func exactKeys(object map[string]any, keys []string) bool {
	for k := range object {
		for _, exact := range keys {
			if k != exact && strings.EqualFold(k, exact) {
				return false
			}
		}
	}
	return true
}
func stringFields(m map[string]any, fields []string) bool {
	for _, f := range fields {
		if v, exists := m[f]; exists {
			if value, ok := v.(string); !ok || strings.Contains(value, "{{") {
				return false
			}
		}
	}
	return true
}
func stringField(m map[string]any, key string) string { value, _ := m[key].(string); return value }
func validResourceKind(k ResourceKind) bool {
	gv, err := schema.ParseGroupVersion(k.APIVersion)
	if err != nil || len(validation.IsDNS1123Label(gv.Version)) > 0 || gv.Group != "" && len(validation.IsDNS1123Subdomain(gv.Group)) > 0 {
		return false
	}
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}$`).MatchString(k.Kind) || strings.HasSuffix(k.Kind, "List") {
		return false
	}
	return validActions(k.Actions)
}
func validActions(actions []string) bool {
	if len(actions) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, a := range actions {
		if !slices.Contains([]string{"create", "get", "delete", "apply"}, a) || seen[a] {
			return false
		}
		seen[a] = true
	}
	return true
}

func resourceInputs(t *wfv1.Template) bool {
	return len(t.Inputs.Artifacts) > 0 || len(t.InitContainers) > 0 || len(t.Sidecars) > 0
}
func envelopeNames(object, meta map[string]any) bool {
	return stringField(object, "apiVersion") != "" && stringField(object, "kind") != "" && (stringField(meta, "name") != "" || stringField(meta, "generateName") != "")
}
func inlineManifest(r *wfv1.ResourceTemplate) bool {
	return r.ManifestFrom == nil && len(r.Flags) == 0 && r.MergeStrategy == "" && len(r.Manifest) > 0
}

func (g *graph) resourceMatch(obj map[string]any, n *node) bool {
	c := g.policy.Capability
	r := n.template.Resource
	version, kind := stringField(obj, "apiVersion"), stringField(obj, "kind")
	for _, k := range c.ResourceKinds {
		if k.APIVersion == version && k.Kind == kind && slices.Contains(k.Actions, r.Action) {
			return true
		}
	}
	return false
}

func (g *graph) protectedNamespaces() []string {
	return []string{g.policy.ServerNamespace, g.policy.ArgoNamespace, g.request.IdentityRequest.Namespace}
}

func (g *graph) capabilityPolicy() {
	c := g.policy.Capability
	if !c.validate() {
		g.refuse("policy.capability", "capability")
	}
	if c == nil {
		return
	}
	for _, namespace := range g.protectedNamespaces() {
		if namespace == "" {
			g.refuse("policy.capability.namespace", "namespace_policy")
		} else if c.Namespace == namespace {
			g.refuse("policy.capability.namespace", "protected_namespace")
		}
	}
	g.capabilityKinds(c)
}

func (g *graph) capabilityKinds(c *Capability) {
	if len(c.ResourceKinds) > admissioncontract.MaxCapabilityKinds {
		g.refuse("policy.capability.resourceKinds", "kind_count")
	}
	for _, k := range c.ResourceKinds {
		if clusterScopedKind(k) {
			g.refuse("policy.capability.resourceKinds["+k.Kind+"]", "cluster_kind")
			g.details = append(g.details, "cluster-scoped kind "+k.Kind+" cannot be changed by a pipeline")
		}
		if argoWorkloadKind(k) {
			g.refuse("policy.capability.resourceKinds["+k.Kind+"]", "workload_kind")
		}
	}
}
