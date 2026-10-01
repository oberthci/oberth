package argoworkflow

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Identity is the server's decision about which Kubernetes identity one run's
// pods execute as. It is derived from the run's trigger, never from the
// repository-authored document.
//
// AI-INVARIANT: the repository's own serviceAccountName is never honored.
// Vault's Kubernetes auth method validates identity, not intent: a role bound
// to (namespace, release ServiceAccount) will happily issue a token to any pod
// running as that ServiceAccount, whatever caused it to exist. The only thing
// standing between a branch push and a release credential is that Oberth
// decides the ServiceAccount server-side. ForceIdentity is that decision.
type Identity struct {
	// Namespace is where the Workflow object and all its pods are created.
	Namespace string
	// ServiceAccountName is applied to every identity-bearing field in the
	// document. Required: an empty value would silently fall back to the
	// namespace "default" ServiceAccount.
	ServiceAccountName string
	// ExecutorServiceAccountName is the identity Argo's own init and wait
	// containers run their bookkeeping as. It is deliberately distinct from
	// ServiceAccountName: the executor needs Kubernetes API access to report
	// workflowtaskresults, and the pipeline's step containers must not
	// inherit that access just because the executor needs it.
	//
	// Argo mounts this identity's token into the executor container only
	// (workflow/controller/workflowpod.go: the exec-sa-token VolumeMount is
	// added to the executor container, never to main), which is what makes a
	// tokenless step container possible at all.
	ExecutorServiceAccountName string
	// AutomountServiceAccountToken controls whether the step container
	// receives a ServiceAccount token. False for branch-tier runs: with no
	// token file, an in-pod Vault login cannot even be attempted, so the tier
	// separation fails closed one layer below the Vault role binding.
	//
	// Argo refuses a Workflow with automount disabled and no executor
	// ServiceAccount ("executor.serviceAccountName must not be empty if
	// automountServiceAccountToken is false"), which is why the executor
	// identity above is not optional.
	AutomountServiceAccountToken bool
}

// Validate reports whether the identity is a usable Kubernetes identity.
func (identity Identity) Validate() error {
	var problems []error
	if messages := k8svalidation.IsDNS1123Label(identity.Namespace); len(messages) != 0 {
		problems = append(problems, fmt.Errorf("argoworkflow: namespace %q is not a DNS-1123 label: %s",
			identity.Namespace, strings.Join(messages, ", ")))
	}
	if messages := k8svalidation.IsDNS1123Subdomain(identity.ServiceAccountName); len(messages) != 0 {
		problems = append(problems, fmt.Errorf("argoworkflow: ServiceAccount %q is not a DNS-1123 subdomain: %s",
			identity.ServiceAccountName, strings.Join(messages, ", ")))
	}
	if messages := k8svalidation.IsDNS1123Subdomain(identity.ExecutorServiceAccountName); len(messages) != 0 {
		problems = append(problems, fmt.Errorf("argoworkflow: executor ServiceAccount %q is not a DNS-1123 subdomain: %s",
			identity.ExecutorServiceAccountName, strings.Join(messages, ", ")))
	}
	// A shared executor identity would hand every step container the
	// Kubernetes API access the executor needs for its own bookkeeping.
	if identity.ServiceAccountName != "" && identity.ServiceAccountName == identity.ExecutorServiceAccountName {
		problems = append(problems, errors.New("argoworkflow: the pipeline and executor ServiceAccounts must differ"))
	}
	return errors.Join(problems...)
}

// The two field names through which a Kubernetes identity reaches a Pod
// anywhere in the Argo schema.
const (
	serviceAccountField = "ServiceAccountName"
	automountField      = "AutomountServiceAccountToken"
)

// MaxIdentityWalkDepth bounds the normalization walk. The schema is
// self-referential (WorkflowStep.Inline and DAGTask.Inline are recursively
// defined *Template values), so a document could otherwise choose the server's
// recursion depth.
const MaxIdentityWalkDepth = 64

var errIdentityWalkTooDeep = fmt.Errorf("argoworkflow: document nests deeper than %d levels", MaxIdentityWalkDepth)

// ForceIdentity overwrites every field in the decoded document through which a
// Kubernetes identity can reach a Pod, and pins the target namespace.
//
// The walk is reflective rather than a hand-written field list, and that is a
// deliberate security decision. An earlier hand-written version normalized the
// twelve paths a careful reading of the schema produced; a type-graph audit
// then found twenty-eight, because Artifact (which carries its own
// artifactGC.serviceAccountName) appears under spec.arguments, under every
// template's inputs and outputs, under every DAG task's and step's arguments,
// under every lifecycle hook's arguments, under data.source.artifactPaths, and
// under resource.manifestFrom — at every level of inline template nesting. A
// list that must be re-derived by hand on every Argo upgrade is a list that
// will eventually be wrong, and being wrong here means a branch push silently
// running as the release ServiceAccount.
//
// Normalizing by field name is exhaustive by construction:
// TestForceIdentityLeavesNoUnnormalizedIdentity proves it against a document
// that sets an identity at every path the schema exposes, and
// TestIdentityFieldPathsAreKnown reports any new path an upgrade introduces.
//
// Prepare is the supported entrypoint: it admits the document and then binds
// the identity, in that order, so a caller cannot get the order wrong.
//
// The ordering is load-bearing. podSpecPatch is a raw strategic-merge string
// that Kubernetes applies to the finished Pod spec; no structural walk can
// neutralize it, so it must be refused rather than normalized. Refusing it
// before binding the identity means there is no window in which a bound
// identity can be patched back out.
func Prepare(workflow *wfv1.Workflow, policy Policy, identity Identity) error {
	if err := Admit(workflow, policy); err != nil {
		return err
	}
	return ForceIdentity(workflow, identity)
}

// ForceIdentity is normally reached through Prepare. Called directly it still
// fails closed: it re-refuses the one construct that could undo its own work,
// so an out-of-order caller cannot produce a bound-then-patched object.
func ForceIdentity(workflow *wfv1.Workflow, identity Identity) error {
	if workflow == nil {
		return errors.New("argoworkflow: no Workflow to bind")
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	if err := rejectPodSpecPatch(workflow); err != nil {
		return err
	}
	// Status is never submitted, but a repository can write one into its YAML.
	// StoredWorkflowSpec is a complete second WorkflowSpec the controller can
	// execute from; clearing status removes it along with any stored templates.
	workflow.Status = wfv1.WorkflowStatus{}
	workflow.Namespace = identity.Namespace
	// Allocate the executor config so the identity has somewhere to land even
	// when the document declared none. Argo requires it whenever the step
	// container's token is withheld.
	if workflow.Spec.Executor == nil {
		workflow.Spec.Executor = &wfv1.ExecutorConfig{}
	}
	// Resolve inherited opt-outs before normalizing nil pointers. A less
	// privileged parent/default can never be widened by a nested template.
	disabled := workflow.Spec.AutomountServiceAccountToken != nil && !*workflow.Spec.AutomountServiceAccountToken
	if defaults := workflow.Spec.TemplateDefaults; defaults != nil && defaults.AutomountServiceAccountToken != nil && !*defaults.AutomountServiceAccountToken {
		disabled = true
	}
	for i := range workflow.Spec.Templates {
		inheritTokenOptOut(&workflow.Spec.Templates[i], disabled, 0)
	}
	return normalizeIdentity(reflect.ValueOf(workflow), identity, 0)
}

func inheritTokenOptOut(template *wfv1.Template, disabled bool, depth int) {
	if template == nil || depth > MaxIdentityWalkDepth {
		return
	}
	disabled = disabled || (template.AutomountServiceAccountToken != nil && !*template.AutomountServiceAccountToken)
	if disabled {
		value := false
		template.AutomountServiceAccountToken = &value
	}
	for _, group := range template.Steps {
		for _, step := range group.Steps {
			inheritTokenOptOut(step.Inline, disabled, depth+1)
		}
	}
	if template.DAG != nil {
		for _, task := range template.DAG.Tasks {
			inheritTokenOptOut(task.Inline, disabled, depth+1)
		}
	}
}

// rejectPodSpecPatch is the backstop for the one field a structural walk
// cannot make safe. It duplicates a check in Admit on purpose: this is the
// invariant that must hold even if the gate is bypassed.
func rejectPodSpecPatch(workflow *wfv1.Workflow) error {
	found := false
	walkPodSpecPatches(workflow, func(patch string) {
		if strings.TrimSpace(patch) != "" {
			found = true
		}
	})
	if found {
		return errors.New("argoworkflow: podSpecPatch cannot be bound to a server-assigned identity; it must be refused")
	}
	return nil
}

func walkPodSpecPatches(workflow *wfv1.Workflow, visit func(string)) {
	visit(workflow.Spec.PodSpecPatch)
	if workflow.Spec.ArtifactGC != nil {
		visit(workflow.Spec.ArtifactGC.PodSpecPatch)
	}
	visitTemplatePodSpecPatches(workflow.Spec.TemplateDefaults, visit, 0)
	for index := range workflow.Spec.Templates {
		visitTemplatePodSpecPatches(&workflow.Spec.Templates[index], visit, 0)
	}
}

func visitTemplatePodSpecPatches(template *wfv1.Template, visit func(string), depth int) {
	if template == nil || depth > MaxIdentityWalkDepth {
		return
	}
	visit(template.PodSpecPatch)
	for group := range template.Steps {
		for step := range template.Steps[group].Steps {
			visitTemplatePodSpecPatches(template.Steps[group].Steps[step].Inline, visit, depth+1)
		}
	}
	if template.DAG != nil {
		for task := range template.DAG.Tasks {
			visitTemplatePodSpecPatches(template.DAG.Tasks[task].Inline, visit, depth+1)
		}
	}
}

func normalizeIdentity(value reflect.Value, identity Identity, depth int) error {
	if depth > MaxIdentityWalkDepth {
		return errIdentityWalkTooDeep
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return normalizeIdentity(value.Elem(), identity, depth+1)
	case reflect.Slice, reflect.Array:
		for index := range value.Len() {
			if err := normalizeIdentity(value.Index(index), identity, depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		return normalizeIdentityMap(value, identity, depth)
	case reflect.Struct:
		return normalizeIdentityStruct(value, identity, depth)
	default:
		return nil
	}
}

// normalizeIdentityMap rebuilds map entries: a map value is not addressable, so
// it cannot be mutated in place. Rebuilding keeps the walk exhaustive even
// though no map in today's schema reaches an identity field.
func normalizeIdentityMap(value reflect.Value, identity Identity, depth int) error {
	if value.IsNil() || !carriesIdentity(value.Type().Elem(), map[reflect.Type]bool{}, 0) {
		return nil
	}
	for _, key := range value.MapKeys() {
		entry := reflect.New(value.Type().Elem()).Elem()
		entry.Set(value.MapIndex(key))
		if err := normalizeIdentity(entry, identity, depth+1); err != nil {
			return err
		}
		value.SetMapIndex(key, entry)
	}
	return nil
}

// executorConfigType is the one struct whose ServiceAccountName names Argo's
// own executor rather than the pipeline's step containers.
var executorConfigType = reflect.TypeOf(wfv1.ExecutorConfig{})

func normalizeIdentityStruct(value reflect.Value, identity Identity, depth int) error {
	account := identity.ServiceAccountName
	if value.Type() == executorConfigType {
		account = identity.ExecutorServiceAccountName
	}
	for index := range value.NumField() {
		definition := value.Type().Field(index)
		if !definition.IsExported() {
			continue
		}
		field := value.Field(index)
		switch definition.Name {
		case serviceAccountField:
			if field.Kind() == reflect.String && field.CanSet() {
				field.SetString(account)
				continue
			}
		case automountField:
			if setAutomount(field, identity.AutomountServiceAccountToken) {
				continue
			}
		}
		if err := normalizeIdentity(field, identity, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func setAutomount(field reflect.Value, automount bool) bool {
	if !field.CanSet() {
		return false
	}
	switch field.Kind() {
	case reflect.Bool:
		field.SetBool(automount)
		return true
	case reflect.Pointer:
		if field.Type().Elem().Kind() != reflect.Bool {
			return false
		}
		if !field.IsNil() && !field.Elem().Bool() {
			return true
		}
		value := automount
		field.Set(reflect.ValueOf(&value))
		return true
	default:
		return false
	}
}

// carriesIdentity reports whether a type can reach an identity field at all,
// so the map rebuild only runs where it could matter.
func carriesIdentity(walked reflect.Type, visiting map[reflect.Type]bool, depth int) bool {
	if depth > 12 {
		return false
	}
	for walked.Kind() == reflect.Pointer || walked.Kind() == reflect.Slice ||
		walked.Kind() == reflect.Array || walked.Kind() == reflect.Map {
		walked = walked.Elem()
	}
	if walked.Kind() != reflect.Struct || visiting[walked] {
		return false
	}
	visiting[walked] = true
	defer delete(visiting, walked)
	for index := range walked.NumField() {
		field := walked.Field(index)
		if !field.IsExported() {
			continue
		}
		if field.Name == serviceAccountField || field.Name == automountField {
			return true
		}
		if carriesIdentity(field.Type, visiting, depth+1) {
			return true
		}
	}
	return false
}

// IdentityFieldPaths enumerates every distinct type-graph path at which the
// upstream schema exposes an identity field. It exists so an Argo upgrade that
// widens the attack surface is visible in a test diff rather than only in the
// normalizer's behavior.
func IdentityFieldPaths() []string {
	return identityFieldPaths(reflect.TypeOf(wfv1.WorkflowSpec{}), "WorkflowSpec", map[reflect.Type]bool{}, 0)
}

func identityFieldPaths(walked reflect.Type, path string, visiting map[reflect.Type]bool, depth int) []string {
	if depth > 12 {
		return nil
	}
	for walked.Kind() == reflect.Pointer || walked.Kind() == reflect.Slice ||
		walked.Kind() == reflect.Array || walked.Kind() == reflect.Map {
		walked = walked.Elem()
	}
	if walked.Kind() != reflect.Struct || visiting[walked] {
		return nil
	}
	visiting[walked] = true
	defer delete(visiting, walked)
	var paths []string
	for index := range walked.NumField() {
		field := walked.Field(index)
		if !field.IsExported() {
			continue
		}
		next := path + "." + field.Name
		if field.Name == serviceAccountField || field.Name == automountField {
			paths = append(paths, next)
			continue
		}
		paths = append(paths, identityFieldPaths(field.Type, next, visiting, depth+1)...)
	}
	return paths
}
