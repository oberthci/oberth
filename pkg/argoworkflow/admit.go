package argoworkflow

import (
	"errors"
	"fmt"
	"k8s.io/apimachinery/pkg/util/validation"
	"slices"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/pkg/argoworkflow/specsize"
)

func (g *graph) err() error {
	g.dedupeFindings()
	failures, needs := []admissioncontract.Finding{}, []admissioncontract.Finding{}
	for _, f := range g.findings {
		if g.offline() && f.Installation && (f.Code != "store_config" || g.offlineValidation) {
			needs = append(needs, f)
		} else {
			failures = append(failures, f)
		}
	}
	var err error
	if len(failures) > 0 {
		err = &AdmissionError{Findings: failures, Needs: needs, Detail: strings.Join(g.details, "\n")}
	}
	return errors.Join(append([]error{err}, g.reads...)...)
}
func (g *graph) dedupeFindings() {
	seen := map[admissioncontract.Finding]bool{}
	unique := g.findings[:0]
	for _, f := range g.findings {
		if !seen[f] {
			unique = append(unique, f)
			seen[f] = true
		}
	}
	g.findings = unique
	g.sortFindings(g.findings)
}

func (g *graph) admit() {
	if g.stopped {
		return
	}
	g.selectSecretGrants()
	g.capabilityPolicy()
	g.metadata()
	for _, c := range g.roots {
		g.arguments(c)
	}
	g.spec(&g.workflow.Spec, "spec", false)
	for _, name := range sortedKeys(g.objects) {
		g.spec(&g.objects[name].Spec, "templates["+name+"].spec", true)
	}
	for _, n := range g.nodes {
		g.admitNode(n)
	}
	inv := g.sources()
	g.inventory = inv
	g.parameterReferences()
	inv.validate()
	g.resultReferences(inv)
	if inv.usesSource {
		size, err := specsize.Submitted(g.workflow)
		if err != nil || size > admissioncontract.MaxSourceSpecBytes {
			g.refuse("spec", "source_size")
		}
	}
}
func (g *graph) metadata() {
	w := g.workflow
	for _, entry := range []struct {
		name    string
		present bool
	}{
		{"name", w.Name != ""}, {"namespace", w.Namespace != ""}, {"ownerReferences", len(w.OwnerReferences) > 0}, {"finalizers", len(w.Finalizers) > 0},
	} {
		if entry.present {
			g.refuse("metadata."+entry.name, "reserved")
		}
	}
	g.metadataKeys("metadata.labels", w.Labels, false)
	if _, ok := w.Annotations[SecretPathsAnnotation]; ok {
		g.refuse("metadata.annotations["+SecretPathsAnnotation+"]", "workflow_secret_paths")
	}
	g.metadataKeys("metadata.annotations", withoutSecretPaths(w.Annotations), false)
}

// withoutSecretPaths leaves the declaration to its own rule instead of the reserved-key refusal.
func withoutSecretPaths(annotations map[string]string) map[string]string {
	rest := map[string]string{}
	for k, v := range annotations {
		if k != SecretPathsAnnotation {
			rest[k] = v
		}
	}
	return rest
}
func (g *graph) spec(s *wfv1.WorkflowSpec, where string, shared bool) {
	for _, v := range []struct {
		field, code string
		present     bool
	}{
		{"workflowTemplateRef", "reference", s.WorkflowTemplateRef != nil}, {"podSpecPatch", "patch", s.PodSpecPatch != ""},
		{"hostNetwork", "host", s.HostNetwork != nil && *s.HostNetwork},
		{"artifactRepositoryRef", "read", s.ArtifactRepositoryRef != nil}, {"arguments.artifacts", "artifact", len(s.Arguments.Artifacts) > 0},
	} {
		if v.present {
			g.refuse(where+"."+v.field, v.code)
		}
	}
	g.parameters(s.Arguments.Parameters, where+".arguments.parameters")
	g.releaseArguments(s.Arguments.Parameters, where+".arguments.parameters")
	g.synchronization(s.Synchronization, where+".synchronization")
	g.podSecurity(s.SecurityContext, where+".securityContext")
	g.volumes(s.Volumes, nil, where+".volumes", shared)
	if s.PodMetadata != nil {
		g.podMetadata(s.PodMetadata, where+".podMetadata")
	}
	if m := s.WorkflowMetadata; m != nil {
		g.metadataKeys(where+".workflowMetadata.labels", m.Labels, false)
		g.metadataKeys(where+".workflowMetadata.annotations", m.Annotations, false)
		for _, k := range sortedKeys(m.LabelsFrom) {
			if reservedKey(k) {
				g.refuse(where+".workflowMetadata.labelsFrom["+k+"]", "reserved")
			}
		}
	}
	g.artifactGC(s, where)

	g.claims(s.VolumeClaimTemplates, where)
	if s.TemplateDefaults != nil {
		n := &node{path: where + ".templateDefaults", template: s.TemplateDefaults, isDefault: true}
		if shared {
			n.scope = "shared"
		}
		g.admitNode(n)
	}
}
func (g *graph) admitNode(n *node) {
	t := n.template
	p := n.path
	g.templateForm(n)
	for _, entry := range []struct {
		field, code string
		present     bool
	}{
		{"podSpecPatch", "patch", t.PodSpecPatch != ""},
		{"memoize", "read", t.Memoize != nil},
	} {
		if entry.present {
			g.refuse(p+"."+entry.field, entry.code)
		}
	}
	if n.scope != "" && (t.ServiceAccountName != "" || t.AutomountServiceAccountToken != nil || t.Executor != nil) {
		g.refuse(p+".identity", "identity")
	}
	metadata := t.Metadata
	if !n.isDefault {
		metadata.Annotations = withoutSecretPaths(metadata.Annotations)
	}
	g.podMetadata(&metadata, p+".metadata")
	g.metadataKeys(p+".annotations", t.Annotations, false)
	g.podSecurity(t.SecurityContext, p+".securityContext")
	g.synchronization(t.Synchronization, p+".synchronization")
	g.parameters(t.Inputs.Parameters, p+".inputs.parameters")
	g.parameters(t.Outputs.Parameters, p+".outputs.parameters")
	g.volumes(t.Volumes, n, p+".volumes", n.scope != "")
	g.templateContainers(n)
	g.imagePulls(n)

	g.clusterReads(t.ArchiveLocation, n, p+".archiveLocation")
	g.artifacts(n)
	g.credentials(n)
	if t.Resource != nil {
		g.resource(n)
	}
	for _, c := range n.calls {
		g.arguments(c)
		for _, h := range c.hooks {
			g.arguments(h)
		}
	}
}
func (g *graph) arguments(c *call) {
	g.parameters(c.arguments.Parameters, c.path+".arguments.parameters")
	for _, a := range c.arguments.Artifacts {
		if !literal(a.Name) || a.From == "" || !artifactArgument(a) {
			g.refuse(c.path+".arguments.artifacts["+a.Name+"]", "artifact")
		}
	}
}
func (g *graph) parameters(params []wfv1.Parameter, where string) {
	for _, p := range params {
		loc := where + "[" + p.Name + "]"
		g.releaseOutput(p.GlobalName, loc)
		if !literal(p.Name) || strings.HasPrefix(p.Name, "oberth-") || strings.HasPrefix(p.GlobalName, "oberth-") {
			g.refuse(loc, "reserved")
		}
		// Only literal '..' is refused for tidiness; a substituted '..' or
		// symlink can still expose the executor token.
		if p.ValueFrom != nil && parentPath(p.ValueFrom.Path) {
			g.refuse(loc+".valueFrom.path", "path")
		}
		if p.ValueFrom != nil && p.ValueFrom.ConfigMapKeyRef != nil {
			g.refuse(loc+".valueFrom.configMapKeyRef", "read")
		}
	}
}
func sortedKeys[K ~string, T any](m map[K]T) []K {
	result := make([]K, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	slices.Sort(result)
	return result
}
func (g *graph) metadataKeys(where string, entries map[string]string, pod bool) {
	for _, key := range sortedKeys(entries) {
		if key == "workflows.argoproj.io/pod-name-format" && strings.HasSuffix(where, "annotations") {
			continue
		}
		if reservedKey(key) || pod && len(validation.IsQualifiedName(key)) != 0 {
			g.refuse(fmt.Sprintf("%s[%q]", where, key), "reserved")
		}
	}
}
func reservedKey(key string) bool {
	return strings.ContainsAny(key, "{}") || strings.HasPrefix(key, "oberth.ci/") || strings.HasPrefix(key, "workflows.argoproj.io/")
}
func (g *graph) podMetadata(m *wfv1.Metadata, where string) {
	g.metadataKeys(where+".labels", m.Labels, true)
	for _, key := range sortedKeys(m.Annotations) {
		at := fmt.Sprintf("%s.annotations[%q]", where, key)
		switch {
		case reservedKey(key):
			g.refuse(at, "reserved")
		case strings.HasPrefix(key, cdiAnnotationPrefix):
			g.refuse(at, "device_annotation")
		}
	}
}

// cdiAnnotationPrefix names Container Device Interface requests, which a
// runtime may turn into host devices without the device plugin's allocation.
const cdiAnnotationPrefix = "cdi.k8s.io/"

func (g *graph) templateContainers(n *node) {
	t := n.template
	p := n.path
	for _, c := range containers(t) {
		if c.container.Image == "" && !n.isDefault {
			g.refuse(p+c.path+".image", "native")
		}
		g.container(c, n, p+c.path, tokenVolumeNames(t))
	}
	if t.ContainerSet != nil {
		g.mounts(t.ContainerSet.VolumeMounts, nil, p+".containerSet", nil)
	}
}

func (g *graph) artifactGC(s *wfv1.WorkflowSpec, where string) {
	if s.ArtifactGC != nil {
		if s.ArtifactGC.PodSpecPatch != "" {
			g.refuse(where+".artifactGC.podSpecPatch", "patch")
		}
		if s.ArtifactGC.PodMetadata != nil {
			g.podMetadata(s.ArtifactGC.PodMetadata, where+".artifactGC.podMetadata")
		}
	}
}

func (g *graph) imagePulls(n *node) {
	if !leaf(n.template) || n.isDefault {
		return
	}
	spec, where := &g.workflow.Spec, "spec"
	// Pull credentials belong to the consumer even when its Pod template is shared.
	consumer := *n
	consumer.scope = ""
	for i, s := range spec.ImagePullSecrets {
		g.clusterGrant(&consumer, "image-pull-secret", s.Name, fmt.Sprintf("%s.imagePullSecrets[%d]", where, i))
	}
}

func (g *graph) releaseArguments(params []wfv1.Parameter, where string) {
	if !g.policy.Release {
		return
	}
	for _, p := range params {
		if admissioncontract.IsReleaseParameter(p.Name) {
			g.refuse(where+"["+p.Name+"]", "release_parameter")
		}
	}
}
func (g *graph) releaseOutput(name, where string) {
	if !g.policy.Release {
		return
	}
	if admissioncontract.IsReleaseParameter(name) {
		g.refuse(where, "release_parameter")
	}
	if name != "" && !literal(name) {
		g.refuse(where, "reserved")
	}
}
