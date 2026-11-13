package argoworkflow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// Structural bounds. A repository-authored document is untrusted input to a
// server that will create Kubernetes objects from it, so every unbounded
// dimension gets a ceiling before the object exists.
const (
	MaxTemplates            = 128
	MaxVolumeClaimTemplates = 8
	MaxContainersPerPod     = 16

	// Resource amplification ceilings. Each bounds one dimension a
	// repository-authored document can amplify into Kubernetes objects.
	MaxDAGTasks          = 64
	MaxStepInvocations   = 64
	MaxWithItemsCount    = 100
	MaxWithSequenceCount = 100
	MaxEmptyDirSize      = "8Gi"
	MaxDiskEmptyDirSize  = "50Gi"
	MaxPVCCapacity       = "64Gi"
)

// Policy is the administrator-owned admission boundary for repository YAML.
type Policy struct {
	Capability *Capability
	// RunnerImagePrefixes is the administrator's image allowlist.
	// Empty selects the built-in
	// pipeline.DefaultRunnerImagePrefixes.
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
	if _, err := DeclaredGoProxyMirrorOnly(workflow); err != nil {
		problems = append(problems, err)
	}
	problems = append(problems, admitSpec(&workflow.Spec)...)
	problems = append(problems, admitTemplates(workflow, policy)...)
	if _, err := DeclaredNonrootTemplates(workflow); err != nil {
		problems = append(problems, err)
	}
	problems = append(problems, admitTriggerParameter(&workflow.Spec)...)
	problems = append(problems, admitScheduleParameter(&workflow.Spec)...)
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
	if strings.TrimSpace(workflow.GenerateName) != "" {
		problems = append(problems, errors.New("argoworkflow: metadata.generateName is assigned by Oberth and must be omitted"))
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
	return problems
}

func admitSpec(spec *wfv1.WorkflowSpec) []error {
	var problems []error
	if strings.TrimSpace(spec.Entrypoint) == "" {
		problems = append(problems, errors.New("argoworkflow: spec.entrypoint is required"))
	}
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
	if len(spec.ImagePullSecrets) != 0 {
		problems = append(problems, errors.New("argoworkflow: spec.imagePullSecrets reads Kubernetes Secrets; publish to an allowlisted public registry instead"))
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
	if spec.ActiveDeadlineSeconds != nil && *spec.ActiveDeadlineSeconds <= 0 {
		problems = append(problems, errors.New("argoworkflow: spec.activeDeadlineSeconds must be positive when supplied"))
	}
	problems = append(problems, admitPodSecurityContext("spec.securityContext", spec.SecurityContext)...)
	problems = append(problems, admitVolumes("spec.volumes", spec.Volumes)...)
	if len(spec.VolumeClaimTemplates) > MaxVolumeClaimTemplates {
		problems = append(problems, fmt.Errorf("argoworkflow: spec.volumeClaimTemplates declares %d entries, maximum is %d",
			len(spec.VolumeClaimTemplates), MaxVolumeClaimTemplates))
	}
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
		if storageReq, ok := claim.Spec.Resources.Requests[corev1.ResourceStorage]; ok && !storageReq.IsZero() {
			maxPVC := resource.MustParse(MaxPVCCapacity)
			if storageReq.Cmp(maxPVC) > 0 {
				problems = append(problems, fmt.Errorf(
					"argoworkflow: spec.volumeClaimTemplates[%d] requests %s storage, maximum is %s",
					index, storageReq.String(), MaxPVCCapacity))
			}
		}
	}
	return problems
}

func admitTemplates(workflow *wfv1.Workflow, policy Policy) []error {
	var problems []error
	if len(workflow.Spec.Templates) == 0 {
		problems = append(problems, errors.New("argoworkflow: spec.templates is required"))
	}
	if len(workflow.Spec.Templates) > MaxTemplates {
		problems = append(problems, fmt.Errorf("argoworkflow: spec.templates declares %d templates, maximum is %d",
			len(workflow.Spec.Templates), MaxTemplates))
	}
	if workflow.Spec.TemplateDefaults != nil {
		problems = append(problems, admitTemplate("spec.templateDefaults", *workflow.Spec.TemplateDefaults, policy, 0)...)
	}
	names := make(map[string]struct{}, len(workflow.Spec.Templates))
	entrypointFound := false
	for index, template := range workflow.Spec.Templates {
		location := fmt.Sprintf("spec.templates[%d]", index)
		if template.Name != "" {
			location = fmt.Sprintf("spec.templates[%q]", template.Name)
		}
		if messages := k8svalidation.IsDNS1123Label(template.Name); len(messages) != 0 {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.name %q is not a DNS-1123 label: %s",
				location, template.Name, strings.Join(messages, ", ")))
		}
		if _, duplicate := names[template.Name]; duplicate {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.name is duplicated", location))
		}
		names[template.Name] = struct{}{}
		if template.Name == workflow.Spec.Entrypoint {
			entrypointFound = true
		}
		problems = append(problems, admitTemplate(location, template, policy, 0)...)
	}
	if workflow.Spec.Entrypoint != "" && !entrypointFound {
		problems = append(problems, fmt.Errorf("argoworkflow: spec.entrypoint %q names no template", workflow.Spec.Entrypoint))
	}
	// Every by-name reference must resolve inside this document. Argo would
	// otherwise accept the Workflow and fail the step at run time, which turns
	// a typo into a red release rather than a refused push -- and, since
	// templateRef is already refused, an unresolvable name can only ever be a
	// mistake.
	for index, template := range workflow.Spec.Templates {
		location := fmt.Sprintf("spec.templates[%d]", index)
		if template.Name != "" {
			location = fmt.Sprintf("spec.templates[%q]", template.Name)
		}
		problems = append(problems, admitTemplateReferences(location, template, names)...)
	}
	check := func(where, referenced string) {
		if referenced == "" {
			return
		}
		if _, ok := names[referenced]; !ok {
			problems = append(problems, fmt.Errorf("argoworkflow: %s references template %q, which this document does not declare", where, referenced))
		}
	}
	if workflow.Spec.OnExit != "" {
		check("spec.onExit", workflow.Spec.OnExit)
	}
	for hookName, hook := range workflow.Spec.Hooks {
		check(fmt.Sprintf("spec.hooks[%q]", hookName), hook.Template)
	}
	return problems
}

// admitTemplateReferences checks the by-name template references a DAG task,
// a step, or a lifecycle hook makes.
func admitTemplateReferences(location string, template wfv1.Template, names map[string]struct{}) []error {
	var problems []error
	check := func(where, referenced string) {
		if referenced == "" {
			return
		}
		if _, ok := names[referenced]; !ok {
			problems = append(problems, fmt.Errorf("argoworkflow: %s references template %q, which this document does not declare", where, referenced))
		}
	}
	if template.DAG != nil {
		for index, task := range template.DAG.Tasks {
			where := fmt.Sprintf("%s.dag.tasks[%d]", location, index)
			if task.Name != "" {
				where = fmt.Sprintf("%s.dag.tasks[%q]", location, task.Name)
			}
			check(where, task.Template)
			for name, hook := range task.Hooks {
				check(fmt.Sprintf("%s.hooks[%q]", where, name), hook.Template)
			}
			if task.Inline != nil {
				problems = append(problems, admitTemplateReferences(where+".inline", *task.Inline, names)...)
			}
		}
	}
	for group := range template.Steps {
		for index, step := range template.Steps[group].Steps {
			where := fmt.Sprintf("%s.steps[%d][%d]", location, group, index)
			check(where, step.Template)
			for name, hook := range step.Hooks {
				check(fmt.Sprintf("%s.hooks[%q]", where, name), hook.Template)
			}
			if step.Inline != nil {
				problems = append(problems, admitTemplateReferences(where+".inline", *step.Inline, names)...)
			}
		}
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
	// Argo applies this deadline to a leaf Pod, never a DAG or steps gate.
	// Keep the check in the recursive path so inline composites also refuse.
	if template.ActiveDeadlineSeconds != nil && (template.DAG != nil || len(template.Steps) != 0) {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.activeDeadlineSeconds is only valid for leaf templates", location))
	}
	if strings.TrimSpace(template.PodSpecPatch) != "" {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.podSpecPatch is not admitted; it can override the server-assigned identity", location))
	}
	// A resource template creates arbitrary Kubernetes objects using the
	// workflow's own ServiceAccount. On the release tier that is a direct
	// privilege escalation out of the pipeline.
	if template.Resource != nil {
		if policy.Capability != nil {
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
	totalStepInvocations := 0
	for group := range template.Steps {
		for index, step := range template.Steps[group].Steps {
			totalStepInvocations++
			where := fmt.Sprintf("%s.steps[%d][%d]", location, group, index)
			if step.TemplateRef != nil {
				problems = append(problems, fmt.Errorf("argoworkflow: %s.templateRef points outside this document; inline the template", where))
			}
			problems = append(problems, admitFanOut(where, len(step.WithItems), step.WithSequence)...)
			problems = append(problems, admitArguments(where+".arguments", step.Arguments)...)
			problems = append(problems, admitHooks(where+".hooks", step.Hooks)...)
			if step.Inline != nil {
				problems = append(problems, admitTemplate(where+".inline", *step.Inline, policy, depth+1)...)
			}
		}
	}
	if totalStepInvocations > MaxStepInvocations {
		problems = append(problems, fmt.Errorf("argoworkflow: %s declares %d step invocations, maximum is %d",
			location, totalStepInvocations, MaxStepInvocations))
	}
	if template.DAG != nil {
		if len(template.DAG.Tasks) > MaxDAGTasks {
			problems = append(problems, fmt.Errorf("argoworkflow: %s.dag declares %d tasks, maximum is %d",
				location, len(template.DAG.Tasks), MaxDAGTasks))
		}
		// Argo rejects a DAG template that mixes enhanced depends (string
		// expressions) with legacy dependencies (string lists). Catch the
		// same combination at admission so `oberth validate` reports it
		// before a push creates a red run.
		hasDepends, hasDependencies := false, false
		for _, task := range template.DAG.Tasks {
			if strings.TrimSpace(task.Depends) != "" {
				hasDepends = true
			}
			if len(task.Dependencies) != 0 {
				hasDependencies = true
			}
		}
		if hasDepends && hasDependencies {
			problems = append(problems, fmt.Errorf(
				"argoworkflow: %s.dag cannot use both 'depends' and 'dependencies' in the same DAG template",
				location))
		}
		for index, task := range template.DAG.Tasks {
			where := fmt.Sprintf("%s.dag.tasks[%d]", location, index)
			if task.Name != "" {
				where = fmt.Sprintf("%s.dag.tasks[%q]", location, task.Name)
			}
			if task.TemplateRef != nil {
				problems = append(problems, fmt.Errorf("argoworkflow: %s.templateRef points outside this document; inline the template", where))
			}
			problems = append(problems, admitFanOut(where, len(task.WithItems), task.WithSequence)...)
			problems = append(problems, admitArguments(where+".arguments", task.Arguments)...)
			problems = append(problems, admitHooks(where+".hooks", task.Hooks)...)
			if task.Inline != nil {
				problems = append(problems, admitTemplate(where+".inline", *task.Inline, policy, depth+1)...)
			}
		}
	}
	return problems
}

// admitFanOut caps literal withItems and withSequence cardinality.
func admitFanOut(location string, withItemsCount int, withSequence *wfv1.Sequence) []error {
	var problems []error
	if withItemsCount > MaxWithItemsCount {
		problems = append(problems, fmt.Errorf(
			"argoworkflow: %s.withItems declares %d entries, maximum is %d",
			location, withItemsCount, MaxWithItemsCount))
	}
	if withSequence != nil && withSequence.Count != nil {
		if count, err := strconv.Atoi(withSequence.Count.String()); err == nil && count > MaxWithSequenceCount {
			problems = append(problems, fmt.Errorf(
				"argoworkflow: %s.withSequence.count %d exceeds the %d ceiling",
				location, count, MaxWithSequenceCount))
		}
	}
	return problems
}

// ReservedExecutorEnvPrefix names the environment the Argo emissary reads
// (and the hold pause the server injects); repositories may not set it.
const ReservedExecutorEnvPrefix = "ARGO_"

func admitContainers(location string, template wfv1.Template, policy Policy) []error {
	var problems []error
	containers := 0
	if template.Container != nil {
		containers++
		problems = append(problems, admitExecutorContainerName(location+".container", template.Container.Name, true)...)
		problems = append(problems, admitContainer(location+".container", template.Container, policy)...)
	}
	if template.Script != nil {
		containers++
		problems = append(problems, admitExecutorContainerName(location+".script", template.Script.Name, true)...)
		problems = append(problems, admitContainer(location+".script", &template.Script.Container, policy)...)
	}
	if template.ContainerSet != nil {
		problems = append(problems, admitHelperMounts(location+".containerSet", template.ContainerSet.VolumeMounts, nil)...)
		for index := range template.ContainerSet.Containers {
			containers++
			problems = append(problems, admitExecutorContainerName(fmt.Sprintf("%s.containerSet.containers[%d]", location, index), template.ContainerSet.Containers[index].Name, true)...)
			problems = append(problems, admitContainer(
				fmt.Sprintf("%s.containerSet.containers[%d]", location, index),
				&template.ContainerSet.Containers[index].Container, policy)...)
		}
	}
	for index := range template.InitContainers {
		containers++
		problems = append(problems, admitExecutorContainerName(fmt.Sprintf("%s.initContainers[%d]", location, index), template.InitContainers[index].Name, false)...)
		problems = append(problems, admitContainer(
			fmt.Sprintf("%s.initContainers[%d]", location, index),
			&template.InitContainers[index].Container, policy)...)
	}
	for index := range template.Sidecars {
		containers++
		problems = append(problems, admitExecutorContainerName(fmt.Sprintf("%s.sidecars[%d]", location, index), template.Sidecars[index].Name, false)...)
		problems = append(problems, admitContainer(
			fmt.Sprintf("%s.sidecars[%d]", location, index),
			&template.Sidecars[index].Container, policy)...)
	}
	if containers > MaxContainersPerPod {
		problems = append(problems, fmt.Errorf("argoworkflow: %s declares %d containers, maximum is %d",
			location, containers, MaxContainersPerPod))
	}
	return problems
}

func admitContainer(location string, container *corev1.Container, policy Policy) []error {
	if container == nil {
		return nil
	}
	var problems []error
	// Every template image must satisfy the administrator's allowlist.
	if err := pipeline.ValidateRunnerImage(container.Image); err != nil {
		problems = append(problems, fmt.Errorf("argoworkflow: %s.image: %w", location, err))
	} else if !pipeline.RunnerImageAllowed(container.Image, policy.RunnerImagePrefixes) {
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
		if variable.ValueFrom.FieldRef != nil || variable.ValueFrom.ResourceFieldRef != nil {
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
	for _, list := range []corev1.ResourceList{resources.Requests, resources.Limits} {
		for name, q := range list {
			if q.Sign() < 0 {
				problems = append(problems, fmt.Errorf("argoworkflow: %s resource %s must not be negative", location, name))
			}

		}
	}
	for name, q := range resources.Requests {
		if limit, ok := resources.Limits[name]; ok && q.Cmp(limit) > 0 {
			problems = append(problems, fmt.Errorf("argoworkflow: %s request %s exceeds its limit", location, name))
		}
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

// admitParameters refuses a parameter that sources its value from a Kubernetes
// ConfigMap. A parameter value is interpolated into step commands and can be
// emitted as an output, so a ConfigMap read here is an exfiltration path that
// the container-level envFrom denial alone does not close.
func admitParameters(location string, parameters []wfv1.Parameter) []error {
	var problems []error
	for index, parameter := range parameters {
		if parameter.ValueFrom == nil || parameter.ValueFrom.ConfigMapKeyRef == nil {
			continue
		}
		where := fmt.Sprintf("%s[%d]", location, index)
		if parameter.Name != "" {
			where = fmt.Sprintf("%s[%q]", location, parameter.Name)
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
			// Require a bounded sizeLimit on every emptyDir, not just
			// memory-backed ones. An uncapped disk-backed emptyDir can
			// exhaust the node's ephemeral storage just as a memory one
			// exhausts RAM. (Issue #440)
			if volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.IsZero() {
				medium := "disk-backed"
				if volume.EmptyDir.Medium == corev1.StorageMediumMemory {
					medium = "memory-backed"
				}
				problems = append(problems, fmt.Errorf(
					"argoworkflow: %s is a %s emptyDir without a sizeLimit; a bounded sizeLimit is required",
					where, medium))
			} else {
				var ceiling string
				if volume.EmptyDir.Medium == corev1.StorageMediumMemory {
					ceiling = MaxEmptyDirSize
				} else {
					ceiling = MaxDiskEmptyDirSize
				}
				maxEmpty := resource.MustParse(ceiling)
				if volume.EmptyDir.SizeLimit.Cmp(maxEmpty) > 0 {
					problems = append(problems, fmt.Errorf(
						"argoworkflow: %s emptyDir sizeLimit %s exceeds the %s ceiling",
						where, volume.EmptyDir.SizeLimit.String(), ceiling))
				}
			}
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
