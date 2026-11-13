package argoworkflow

import (
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// MaxTemplates bounds the existing nonroot annotation, not native templates.
const MaxTemplates = 128

// Policy is the administrator-owned admission boundary for repository YAML.
type Policy struct {
	Capability *Capability
	// RunnerImagePrefixes is the administrator's image allowlist.
	// Empty admits any image.
	RunnerImagePrefixes []string
}

// Admit refuses every construct through which a repository-authored document
// could obtain substrate Oberth did not grant it. It runs on the decoded
// object before ForceIdentity and before the object is submitted.
//
// The gate is a deny list of named fields rather than an allow list of shapes
// because the Argo schema is large and the interesting parts (dag, steps,
// container, script, synchronization, retryStrategy, volumeClaimTemplates)
// are exactly the parts a pipeline needs. Every rejection below names a
// concrete way to reach a credential, a host, or another tenant's identity.
func Admit(workflow *wfv1.Workflow, policy Policy) (resultErr error) {
	if err := pipeline.ValidateRunnerImagePrefixes(policy.RunnerImagePrefixes); err != nil {
		return fmt.Errorf("argoworkflow: %w", err)
	}
	if err := policy.Capability.Validate(); err != nil {
		return err
	}

	defer func() {
		if resultErr != nil {
			resultErr = &AdmissionError{Cause: resultErr}
		}
	}()
	if workflow == nil {
		return errors.New("argoworkflow: no Workflow to admit")
	}
	var problems []error
	problems = append(problems, admitMetadata(workflow)...)
	problems = append(problems, admitSpec(&workflow.Spec)...)
	problems = append(problems, admitTemplates(workflow, policy)...)
	if _, err := DeclaredNonrootTemplates(workflow); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

func admitMetadata(workflow *wfv1.Workflow) []error {
	var problems []error
	// The server owns the object name (it is the durable run's deterministic
	// Job name) and the namespace. A document that names itself would either
	// be overwritten silently or collide with another tenant's object.
	if strings.TrimSpace(workflow.Name) != "" {
		problems = append(problems, errors.New("argoworkflow: metadata.name is assigned by Oberth and must be omitted"))
	}
	if strings.TrimSpace(workflow.Namespace) != "" {
		problems = append(problems, errors.New("argoworkflow: metadata.namespace is assigned by Oberth and must be omitted"))
	}
	if len(workflow.OwnerReferences) != 0 {
		problems = append(problems, errors.New("argoworkflow: metadata.ownerReferences must be omitted"))
	}
	if len(workflow.Finalizers) != 0 {
		problems = append(problems, errors.New("argoworkflow: metadata.finalizers must be omitted"))
	}
	annotations := make(map[string]string, len(workflow.Annotations))
	for key, value := range workflow.Annotations {
		if !supportedWorkflowAnnotation(key) {
			annotations[key] = value
		}
	}
	problems = append(problems, admitWorkflowMetadata("metadata", &wfv1.WorkflowMetadata{Labels: workflow.Labels, Annotations: annotations})...)
	return problems
}

// Only annotations with a live authored declaration reader bypass the ownership
// namespace rule. Assist identity/hold markers are server-written, not opt-ins.
func supportedWorkflowAnnotation(key string) bool {
	switch key {
	case SecretPathsAnnotation, SourcesAnnotation, GoProxyAnnotation, NonrootTemplatesAnnotation:
		return true
	default:
		return false
	}
}

func admitSpec(spec *wfv1.WorkflowSpec) []error {
	var problems []error
	// A referenced template is a spec Oberth never read. Admission decisions
	// must be made against the exact bytes in the pushed revision.
	if spec.WorkflowTemplateRef != nil {
		problems = append(problems, errors.New("argoworkflow: spec.workflowTemplateRef submits a spec Oberth cannot review; inline the templates"))
	}
	// podSpecPatch is a strategic merge applied after Oberth's normalization.
	// It can set serviceAccountName, privileged, hostPID, or a projected token
	// volume, which would defeat the trigger-gated identity switch entirely.
	if strings.TrimSpace(spec.PodSpecPatch) != "" {
		problems = append(problems, errors.New("argoworkflow: spec.podSpecPatch is not admitted; it can override the server-assigned identity"))
	}
	problems = append(problems, admitArtifactGCPodMetadata(spec.ArtifactGC)...)
	if spec.ArtifactRepositoryRef != nil {
		problems = append(problems, errors.New("argoworkflow: spec.artifactRepositoryRef resolves credentials from a Kubernetes Secret; use volumeClaimTemplates for step hand-off"))
	}
	if spec.HostNetwork != nil && *spec.HostNetwork {
		problems = append(problems, errors.New("argoworkflow: spec.hostNetwork is not admitted"))
	}
	if len(spec.HostAliases) != 0 {
		problems = append(problems, errors.New("argoworkflow: spec.hostAliases is not admitted"))
	}
	if spec.DNSConfig != nil || spec.DNSPolicy != nil {
		problems = append(problems, errors.New("argoworkflow: spec.dnsConfig and spec.dnsPolicy would redirect name resolution inside the Pod and are not admitted"))
	}
	problems = append(problems, admitArguments("spec.arguments", spec.Arguments)...)
	problems = append(problems, admitHooks("spec.hooks", spec.Hooks)...)
	if spec.SchedulerName != "" {
		problems = append(problems, errors.New("argoworkflow: spec.schedulerName is not admitted"))
	}
	// Author Pod metadata is native except literal keys reserved for ownership
	// and the selectors installed by Oberth.
	problems = append(problems, admitPodMetadata("spec.podMetadata", spec.PodMetadata)...)
	problems = append(problems, admitWorkflowMetadata("spec.workflowMetadata", spec.WorkflowMetadata)...)
	problems = append(problems, admitSynchronization("spec.synchronization", spec.Synchronization)...)
	if spec.PodPriorityClassName != "" {
		problems = append(problems, errors.New("argoworkflow: spec.podPriorityClassName is not admitted"))
	}
	if spec.PodDisruptionBudget != nil {
		problems = append(problems, errors.New("argoworkflow: spec.podDisruptionBudget is not admitted"))
	}
	problems = append(problems, admitPodSecurityContext("spec.securityContext", spec.SecurityContext)...)
	problems = append(problems, admitVolumes("spec.volumes", spec.Volumes)...)
	for index, claim := range spec.VolumeClaimTemplates {
		if reservedVCTName(claim.Name) {
			problems = append(problems, fmt.Errorf(
				"argoworkflow: spec.volumeClaimTemplates[%d].metadata.name %q collides with a server-owned claim name",
				index, claim.Name))
		}
		if strings.TrimSpace(claim.Spec.VolumeName) != "" {
			problems = append(problems, fmt.Errorf("argoworkflow: spec.volumeClaimTemplates[%d].spec.volumeName binds an existing volume", index))
		}
		if claim.Spec.DataSource != nil {
			problems = append(problems, fmt.Errorf("argoworkflow: spec.volumeClaimTemplates[%d].spec.dataSource can clone another run's data", index))
		}
		if claim.Spec.DataSourceRef != nil {
			problems = append(problems, fmt.Errorf("argoworkflow: spec.volumeClaimTemplates[%d].spec.dataSourceRef can clone another run's data", index))
		}
		if claim.Spec.Selector != nil {
			problems = append(problems, fmt.Errorf("argoworkflow: spec.volumeClaimTemplates[%d].spec.selector can bind an existing volume", index))
		}
		if len(claim.Finalizers) != 0 {
			problems = append(problems, fmt.Errorf("argoworkflow: spec.volumeClaimTemplates[%d].metadata.finalizers must be omitted", index))
		}
		if len(claim.OwnerReferences) != 0 {
			problems = append(problems, fmt.Errorf("argoworkflow: spec.volumeClaimTemplates[%d].metadata.ownerReferences must be omitted", index))
		}
	}
	return problems
}

func admitTemplates(workflow *wfv1.Workflow, policy Policy) []error {
	var problems []error
	if workflow.Spec.TemplateDefaults != nil {
		problems = append(problems, admitTemplate("spec.templateDefaults", *workflow.Spec.TemplateDefaults, policy, 0)...)
	}
	for index, template := range workflow.Spec.Templates {
		location := fmt.Sprintf("spec.templates[%d]", index)
		if template.Name != "" {
			location = fmt.Sprintf("spec.templates[%q]", template.Name)
		}
		problems = append(problems, admitTemplate(location, template, policy, 0)...)
	}
	return problems
}

func admitTemplate(location string, template wfv1.Template, policy Policy, depth int) []error {
	if depth > MaxIdentityWalkDepth {
		return []error{fmt.Errorf("argoworkflow: %s nests inline templates deeper than %d levels", location, MaxIdentityWalkDepth)}
	}
	var problems []error
	// Argo can merge default tasks/steps into a graph after the server has
	// protected its concrete templates. Their inline leaves would bypass that
	// protection. DAG and Steps are the schema's two nested-template carriers.
	if location == "spec.templateDefaults" {
		if template.DAG != nil {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.dag is not admitted; declare tasks in concrete templates so their inline containers receive server protection", location))
		}
		if len(template.Steps) != 0 {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.steps is not admitted; declare steps in concrete templates so their inline containers receive server protection", location))
		}
		// Named helper defaults are also added after concrete-template
		// hardening, so they must be declared on the Pod leaf instead.
		if len(template.Sidecars) != 0 {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.sidecars is not admitted; declare helpers in concrete templates so they receive server protection", location))
		}
		if len(template.InitContainers) != 0 {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.initContainers is not admitted; declare helpers in concrete templates so they receive server protection", location))
		}
	}
	problems = append(problems, admitNestedTemplates(location, template, policy, depth)...)
	problems = append(problems, admitSynchronization(location+".synchronization", template.Synchronization)...)
	if strings.TrimSpace(template.PodSpecPatch) != "" {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.podSpecPatch is not admitted; it can override the server-assigned identity", location))
	}
	// A resource template creates arbitrary Kubernetes objects using the
	// workflow's own ServiceAccount. On the release tier that is a direct
	// privilege escalation out of the pipeline.
	if template.Resource != nil {
		if policy.Capability != nil {
			if len(template.InitContainers) != 0 || len(template.Sidecars) != 0 {
				problems = append(problems, fmt.Errorf("argoworkflow: %s: resource templates may not declare initContainers or sidecars", location))
			}
			if strings.HasPrefix(template.Name, "frag-") || strings.Contains(location, ".inline") || location == "spec.templateDefaults" {
				problems = append(problems, fmt.Errorf("argoworkflow: %s.resource must be a named template in the consuming repository", location))
			} else if err := admitCapabilityResource(location+".resource", template.Resource, policy.Capability); err != nil {
				problems = append(problems, err)
			}
		} else {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.resource: %w", location, ErrWorkflowCapabilityRequired))
		}
	}
	// Plugin and HTTP templates execute in the Argo agent pod, which runs
	// under an identity Oberth does not assign here.
	if template.Plugin != nil {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.plugin executes outside the server-assigned identity and is not admitted", location))
	}
	if template.HTTP != nil {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.http executes in the Argo agent Pod and is not admitted", location))
	}
	// Artifacts require an artifact repository, whose credentials live in a
	// Kubernetes Secret. Step hand-off uses volumeClaimTemplates instead.
	if len(template.Inputs.Artifacts) != 0 || len(template.Outputs.Artifacts) != 0 {
		problems = append(problems, fmt.Errorf("argoworkflow: %s declares artifacts, which resolve credentials from a Kubernetes Secret; hand off through volumeClaimTemplates", location))
	}
	// A data template's artifactPaths source embeds a full Artifact, with the
	// same accessKeySecret/secretKeySecret/sshPrivateKeySecret selectors the
	// artifact denial above exists to refuse. Same door, so the same lock.
	if template.Data != nil {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.data sources an artifact location and is not admitted", location))
	}
	if template.ArchiveLocation != nil {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.archiveLocation is not admitted", location))
	}
	if len(template.HostAliases) != 0 {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.hostAliases is not admitted", location))
	}
	if template.SchedulerName != "" || template.PriorityClassName != "" {
		problems = append(problems, fmt.Errorf("argoworkflow: %s scheduler and priority overrides are not admitted", location))
	}
	if template.Memoize != nil {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.memoize reads and writes a ConfigMap-backed cache and is not admitted", location))
	}
	problems = append(problems, admitTemplateMetadata(location+".metadata", template.Metadata)...)
	problems = append(problems, admitParameters(location+".inputs.parameters", template.Inputs.Parameters)...)
	problems = append(problems, admitParameters(location+".outputs.parameters", template.Outputs.Parameters)...)
	problems = append(problems, admitPodSecurityContext(location+".securityContext", template.SecurityContext)...)
	problems = append(problems, admitVolumes(location+".volumes", template.Volumes)...)
	problems = append(problems, admitContainers(location, template, policy)...)
	return problems
}

// admitNestedTemplates walks the two recursive template positions in the
// schema (WorkflowStep.Inline and DAGTask.Inline) so an inline template cannot
// carry a construct the gate rejects at the top level, and refuses the
// by-name references to objects outside this document.
func admitNestedTemplates(location string, template wfv1.Template, policy Policy, depth int) []error {
	var problems []error
	for group := range template.Steps {
		for index, step := range template.Steps[group].Steps {
			where := fmt.Sprintf("%s.steps[%d][%d]", location, group, index)
			if step.TemplateRef != nil {
				problems = append(problems, fmt.Errorf("argoworkflow: %s.templateRef points outside this document; inline the template", where))
			}
			problems = append(problems, admitArguments(where+".arguments", step.Arguments)...)
			problems = append(problems, admitHooks(where+".hooks", step.Hooks)...)
			if step.Inline != nil {
				problems = append(problems, admitTemplate(where+".inline", *step.Inline, policy, depth+1)...)
			}
		}
	}
	if template.DAG != nil {
		for index, task := range template.DAG.Tasks {
			where := fmt.Sprintf("%s.dag.tasks[%d]", location, index)
			if task.Name != "" {
				where = fmt.Sprintf("%s.dag.tasks[%q]", location, task.Name)
			}
			if task.TemplateRef != nil {
				problems = append(problems, fmt.Errorf("argoworkflow: %s.templateRef points outside this document; inline the template", where))
			}
			problems = append(problems, admitArguments(where+".arguments", task.Arguments)...)
			problems = append(problems, admitHooks(where+".hooks", task.Hooks)...)
			if task.Inline != nil {
				problems = append(problems, admitTemplate(where+".inline", *task.Inline, policy, depth+1)...)
			}
		}
	}
	return problems
}

// ReservedExecutorEnvPrefix names the environment the Argo emissary reads
// (and the hold pause the server injects); repositories may not set it.
const ReservedExecutorEnvPrefix = "ARGO_"

func admitContainers(location string, template wfv1.Template, policy Policy) []error {
	var problems []error
	if template.Container != nil {
		problems = append(problems, admitExecutorContainerName(location+".container", template.Container.Name, true)...)
		problems = append(problems, admitContainer(location+".container", template.Container, policy)...)
	}
	if template.Script != nil {
		problems = append(problems, admitExecutorContainerName(location+".script", template.Script.Name, true)...)
		problems = append(problems, admitContainer(location+".script", &template.Script.Container, policy)...)
	}
	if template.ContainerSet != nil {
		problems = append(problems, admitHelperMounts(location+".containerSet", template.ContainerSet.VolumeMounts, nil)...)
		for index := range template.ContainerSet.Containers {
			problems = append(problems, admitExecutorContainerName(fmt.Sprintf("%s.containerSet.containers[%d]", location, index), template.ContainerSet.Containers[index].Name, true)...)
			problems = append(problems, admitContainer(
				fmt.Sprintf("%s.containerSet.containers[%d]", location, index),
				&template.ContainerSet.Containers[index].Container, policy)...)
		}
	}
	for index := range template.InitContainers {
		problems = append(problems, admitExecutorContainerName(fmt.Sprintf("%s.initContainers[%d]", location, index), template.InitContainers[index].Name, false)...)
		problems = append(problems, admitContainer(
			fmt.Sprintf("%s.initContainers[%d]", location, index),
			&template.InitContainers[index].Container, policy)...)
	}
	for index := range template.Sidecars {
		problems = append(problems, admitExecutorContainerName(fmt.Sprintf("%s.sidecars[%d]", location, index), template.Sidecars[index].Name, false)...)
		problems = append(problems, admitContainer(
			fmt.Sprintf("%s.sidecars[%d]", location, index),
			&template.Sidecars[index].Container, policy)...)
	}
	return problems
}

func admitContainer(location string, container *corev1.Container, policy Policy) []error {
	if container == nil {
		return nil
	}
	var problems []error
	// Every template image must satisfy the administrator's allowlist.
	if !pipeline.RunnerImageAllowed(container.Image, policy.RunnerImagePrefixes) {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.image %q is outside the administrator prefix allowlist",
			location, container.Image))
	}
	// envFrom and valueFrom read Kubernetes Secrets and ConfigMaps directly
	// into the pipeline's environment, bypassing the store-sourced path.
	for index, source := range container.EnvFrom {
		if source.SecretRef != nil || source.ConfigMapRef != nil {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.envFrom[%d] reads a Kubernetes Secret or ConfigMap and is not admitted", location, index))
		}
	}
	for _, variable := range container.Env {
		if strings.Contains(variable.Name, "{{") {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.env[%q] has a templated name; environment names must be literal", location, variable.Name))
		}
		// ARGO_* is the executor's own control vocabulary (ARGO_DEBUG_PAUSE_*,
		// ARGO_CONTAINER_NAME, ARGO_DEADLINE, ...). A document that sets one
		// could pause or mislead the emissary; only the server may.
		if strings.HasPrefix(variable.Name, ReservedExecutorEnvPrefix) {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.env[%q] is reserved for the Argo executor and is not admitted", location, variable.Name))
		}
		if variable.ValueFrom == nil {
			continue
		}
		if variable.ValueFrom.SecretKeyRef != nil || variable.ValueFrom.ConfigMapKeyRef != nil {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.env[%q] reads a Kubernetes Secret or ConfigMap and is not admitted", location, variable.Name))
		}
		if variable.ValueFrom.FieldRef != nil && variable.ValueFrom.FieldRef.FieldPath != "spec.serviceAccountName" {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.env[%q] reads pod/downward state and is not admitted", location, variable.Name))
		}
	}
	problems = append(problems, admitHelperMounts(location, container.VolumeMounts, container.VolumeDevices)...)
	problems = append(problems, admitContainerPorts(location, container.Ports)...)
	problems = append(problems, admitContainerSecurityContext(location+".securityContext", container.SecurityContext)...)
	problems = append(problems, admitContainerResources(location, container.Resources)...)
	return problems
}

// admitContainerResources preserves native resource declarations without operator budgets.
func admitContainerResources(location string, resources corev1.ResourceRequirements) []error {
	var problems []error
	if len(resources.Claims) > 0 {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.resources.claims is not admitted; claim-backed device access is outside the reviewed Pod contract", location))
	}
	return problems
}

// admitContainerPorts refuses a port that reaches the node.
//
// The rest of the node-isolation fence -- hostNetwork, hostAliases, hostPath,
// nodeSelector, affinity -- was already denied, but a hostPort walked straight
// through it. A repository-authored pipeline could bind any port on the shared
// node: 30022 is Oberth's own Git ingress, and whichever of the two bound first
// would own it. That is denial of service against the CI server at best, and
// interception of Git traffic aimed at it at worst, from nothing more than a
// pushed branch.
//
// containerPort alone stays admitted. It is pod-local, it is what a daemon
// template legitimately declares for a sibling step to reach, and it grants
// nothing outside the Pod's own network namespace.
func admitContainerPorts(location string, ports []corev1.ContainerPort) []error {
	var problems []error
	for index, port := range ports {
		if port.HostPort != 0 {
			problems = append(problems, fmt.Errorf(
				"argoworkflow: %s.ports[%d].hostPort %d binds a port on the node and is not admitted; "+
					"containerPort is pod-local and needs no host binding",
				location, index, port.HostPort))
		}
		if strings.TrimSpace(port.HostIP) != "" {
			problems = append(problems, fmt.Errorf(
				"argoworkflow: %s.ports[%d].hostIP %q addresses the node and is not admitted",
				location, index, port.HostIP))
		}
	}
	return problems
}

// admitContainerSecurityContext refuses the whole container security context.
//
// The engine forces the entire container baseline server-side
// (internal/argojob.applyServerSecurity), so a value set here is either
// silently overwritten -- which makes it a lie in a reviewed file -- or, for
// any field the forcer does not name, a real weakening of that baseline.
//
// An earlier version checked only privileged, allowPrivilegeEscalation, added
// capabilities, and platform options. That left seccompProfile: Unconfined and
// runAsUser passing through untouched on both tiers, on the same node as the
// server. Refusing the struct removes the class of bug instead of chasing its
// instances, and it is the same answer Oberth already gives for
// serviceAccountName: the field is not repository-configurable.
func admitContainerSecurityContext(location string, security *corev1.SecurityContext) []error {
	if security == nil {
		return nil
	}
	return []error{fmt.Errorf(
		"argoworkflow: %s is assigned by Oberth and must be omitted; step containers run "+
			"with a forced baseline (no privilege escalation, read-only root filesystem)", location)}
}

// admitPodSecurityContext refuses the whole Pod security context, for the same
// reason: the workflow-level context is server-assigned in full, including the
// RuntimeDefault seccomp profile.
func admitPodSecurityContext(location string, security *corev1.PodSecurityContext) []error {
	if security == nil {
		return nil
	}
	return []error{fmt.Errorf(
		"argoworkflow: %s is assigned by Oberth and must be omitted; the Pod security "+
			"baseline (user, group, seccomp profile) is not repository-configurable", location)}
}

// admitParameters reserves the oberth- prefix for server-owned run facts and
// refuses a parameter that sources its value from a Kubernetes
// ConfigMap. A parameter value is interpolated into step commands and can be
// emitted as an output, so a ConfigMap read here is an exfiltration path that
// the container-level envFrom denial alone does not close.
func admitParameters(location string, parameters []wfv1.Parameter) []error {
	var problems []error
	for index, parameter := range parameters {
		where := fmt.Sprintf("%s[%d]", location, index)
		if parameter.Name != "" {
			where = fmt.Sprintf("%s[%q]", location, parameter.Name)
		}
		if strings.HasPrefix(parameter.Name, "oberth-") {
			problems = append(problems, fmt.Errorf("argoworkflow: %s is server-owned; the oberth- parameter prefix is reserved; reference workflow parameters without declaring them", where))
		}
		if strings.HasPrefix(parameter.GlobalName, "oberth-") {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.globalName is server-owned; the oberth- parameter prefix is reserved", where))
		}
		if parameter.ValueFrom == nil || parameter.ValueFrom.ConfigMapKeyRef == nil {
			continue
		}
		problems = append(problems, fmt.Errorf("argoworkflow: %s.valueFrom.configMapKeyRef reads a Kubernetes ConfigMap and is not admitted", where))
	}
	return problems
}

func admitArguments(location string, arguments wfv1.Arguments) []error {
	problems := admitParameters(location+".parameters", arguments.Parameters)
	if len(arguments.Artifacts) != 0 {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.artifacts resolve credentials from a Kubernetes Secret and are not admitted", location))
	}
	return problems
}

func admitHooks(location string, hooks wfv1.LifecycleHooks) []error {
	var problems []error
	for name, hook := range hooks {
		where := fmt.Sprintf("%s[%q]", location, name)
		if hook.TemplateRef != nil {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.templateRef points outside this document; inline the template", where))
		}
		problems = append(problems, admitArguments(where+".arguments", hook.Arguments)...)
	}
	return problems
}

// admitPodMetadata permits labels outside ownership/selector keys. Pod
// annotations can change runtime, CNI and webhook behavior, so are server-owned.
func admitPodMetadata(location string, metadata *wfv1.Metadata) []error {
	if metadata == nil {
		return nil
	}
	var problems []error
	if len(metadata.Annotations) != 0 {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.annotations is assigned by Oberth; Pod annotations select security profiles and injection webhooks", location))
	}
	return append(problems, admitPodMetadataKeys(location+".labels", metadata.Labels)...)
}

func admitTemplateMetadata(location string, metadata wfv1.Metadata) []error {
	var problems []error
	for name, value := range metadata.Annotations {
		switch name {
		case ReleaseWIFRoleAnnotation:
			if !ValidReleaseWIFRole(value) {
				problems = append(problems, fmt.Errorf("argoworkflow: %s.%s must name an exact release WIF role", location, name))
			}
		case WorkspaceMountsAnnotation:
			if !ValidWorkspaceMounts(value) {
				problems = append(problems, fmt.Errorf("argoworkflow: %s.%s must be none, cache or readonly", location, name))
			}
		case WorkspaceEnvAnnotation:
			if value != "none" {
				problems = append(problems, fmt.Errorf("argoworkflow: %s.%s must be none", location, name))
			}
		default:
			problems = append(problems, fmt.Errorf("argoworkflow: %s.annotations is assigned by Oberth; Pod annotations select security profiles and injection webhooks", location))
		}
	}
	problems = append(problems, admitPodMetadataKeys(location+".labels", metadata.Labels)...)
	return problems
}

// admitVolumes allows exactly the two volume kinds a pipeline needs and no
// projection that could reach a credential or the host.
//
// emptyDir is scratch. persistentVolumeClaim is admitted only in its
// ephemeral form (spec.volumeClaimTemplates), which Argo creates and deletes
// per workflow; a claimName reference would let a push mount another tenant's
// data — including Oberth's own workspace PVC.
func admitVolumes(location string, volumes []corev1.Volume) []error {
	var problems []error
	for index, volume := range volumes {
		problems = append(problems, admitExecutorVolumeName(fmt.Sprintf("%s[%d]", location, index), volume.Name)...)
		where := fmt.Sprintf("%s[%d]", location, index)
		if volume.Name != "" {
			where = fmt.Sprintf("%s[%q]", location, volume.Name)
		}
		switch {
		case volume.EmptyDir != nil:
			continue
		case volume.PersistentVolumeClaim != nil:
			problems = append(problems, fmt.Errorf("argoworkflow: %s references an existing PersistentVolumeClaim; declare spec.volumeClaimTemplates instead", where))
		case volume.Secret != nil || volume.ConfigMap != nil || volume.Projected != nil || volume.CSI != nil:
			problems = append(problems, fmt.Errorf("argoworkflow: %s projects credential material into the Pod and is not admitted", where))
		case volume.HostPath != nil:
			problems = append(problems, fmt.Errorf("argoworkflow: %s mounts the node filesystem and is not admitted", where))
		default:
			problems = append(problems, fmt.Errorf("argoworkflow: %s declares an unsupported volume source; only emptyDir and spec.volumeClaimTemplates are admitted", where))
		}
	}
	return problems
}

// reservedVCTNames are volumeClaimTemplate names that collide with server-
// owned PVC names. Argo creates VCT PVCs as `<workflow>-<vct-name>`; the
// server creates source claims as `<workflow>-src`. A VCT named "src"
// therefore collides, and a collision lets the repository's writable volume
// replace the server's immutable source checkout.
var reservedVCTNames = map[string]bool{
	"src": true,
}

func reservedVCTName(name string) bool {
	return reservedVCTNames[name] || serverOwnedName(name) || reservedArgoVolumeName(name) || strings.Contains(name, "{{")
}

func admitArtifactGCPodMetadata(gc *wfv1.WorkflowLevelArtifactGC) []error {
	if gc == nil {
		return nil
	}
	problems := admitPodMetadata("spec.artifactGC.podMetadata", gc.PodMetadata)
	if strings.TrimSpace(gc.PodSpecPatch) != "" {
		problems = append(problems, errors.New("argoworkflow: spec.artifactGC.podSpecPatch is not admitted"))
	}
	return problems
}
