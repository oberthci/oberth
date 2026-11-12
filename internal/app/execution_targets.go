package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// ExecutionTarget is an operator-owned authenticated native client bundle.
// It supplies no scheduler, grants, result importer or alternate Git authority.
type ExecutionTarget struct {
	ID             string
	Config         argojob.Config
	Transport      *rest.Config
	Kubernetes     kubernetes.Interface
	Workflows      func(string) argojob.WorkflowClient
	Exec           argojob.ExecStreamer
	ResourceSource argojob.ResourceUsageSource
}

type executionTargets struct {
	mu           sync.Mutex
	store        *store.Store
	owner, local *ArgoJobs
	definitions  map[string]ExecutionTarget
	children     map[string]*ArgoJobs
	cleanupAfter string
}

type targetSnapshot struct {
	Version   int
	Config    argojob.Config
	Source    []byte
	Fragments []retainedFragment
	Cleanup   map[string]string
}

// Fragment.Source is omitted from public locks, so custody explicitly retains it.
type retainedFragment struct {
	Fragment argoworkflow.Fragment
	Source   []byte
}

// WithExecutionTargets must also be installed with an empty definition set:
// after restart a missing operator bundle must retain a remote binding, never
// route an already-admitted run back to the local cluster.
func WithExecutionTargets(local *ArgoJobs, database *store.Store, definitions []ExecutionTarget) (*ArgoJobs, error) {
	if local == nil || database == nil {
		return nil, errors.New("app: target routing requires the canonical engine and store")
	}
	owner, err := NewArgoJobs(local.controller, local.config, local.auditor, local.secretAccess, local.fragments)
	if err != nil {
		return nil, err
	}
	owner.pinnedSources, owner.sourceRecorder = local.pinnedSources, local.sourceRecorder
	owner.resourceBlobs = local.resourceBlobs
	owner.capabilityGrants = local.capabilityGrants
	owner.identities = local.identities
	owner.collector = local.collector
	owner.artifacts = local.artifacts
	owner.artifactLimit = local.artifactLimit
	owner.reconcilerHealthy = local.reconcilerHealthy
	owner.declarations = local.declarations
	routes := &executionTargets{store: database, owner: owner, local: local, definitions: map[string]ExecutionTarget{}, children: map[string]*ArgoJobs{}}
	for _, definition := range definitions {
		if definition.ID == "" || definition.ID == "local" || definition.Transport == nil || definition.Kubernetes == nil || definition.Workflows == nil || definition.Exec == nil || !definition.Config.ExecutionPlacement.BranchOnly() {
			return nil, errors.New("app: complete branch-only execution target required")
		}
		if _, duplicate := routes.definitions[definition.ID]; duplicate {
			return nil, errors.New("app: duplicate execution target")
		}
		// Snapshot all public settings; caller-owned maps cannot mutate policy.
		runtime := definition.Config
		body, err := json.Marshal(definition.Config)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(body, &definition.Config); err != nil {
			return nil, err
		}
		definition.Config.CPUCapacity = runtime.CPUCapacity
		definition.Config.CPUAdmission = runtime.CPUAdmission
		definition.Config.CPUAdmissionDisabled = runtime.CPUAdmissionDisabled
		definition.Config.StepCPURequestMaxMilli = runtime.StepCPURequestMaxMilli
		definition.Config, err = argojob.DefaultedConfig(definition.Config)
		if err != nil {
			return nil, err
		}
		definition.Transport = rest.CopyConfig(definition.Transport)
		if err = definition.Config.ExecutionPlacement.VerifyTarget(definition.Transport); err != nil {
			return nil, err
		}
		for _, other := range routes.definitions {
			repo, _, ref, _, _, _, _ := other.Config.ExecutionPlacement.Identity()
			r, _, f, _, _, _, _ := definition.Config.ExecutionPlacement.Identity()
			if repo == r && ref == f {
				return nil, errors.New("app: ambiguous execution target scope")
			}
		}
		routes.definitions[definition.ID] = definition
	}
	owner.routes = routes
	return owner, nil
}

func targetHash(value any) string { body, _ := json.Marshal(value); return targetBytesHash(body) }
func targetBytesHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (r *executionTargets) forRequest(ctx context.Context, request service.WorkflowRequest) (*ArgoJobs, error) {
	binding, found, err := r.store.ExecutionTarget(ctx, request.Run.ID, request.WorkflowName)
	if err != nil {
		return nil, err
	}
	if found {
		if binding.RepositoryID != request.Repository.ID || binding.Repository != model.CanonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repository.Name) {
			return nil, argojob.ErrTargetUncertain
		}
		if err := matchTargetRun(binding, request.Run, request.WorkflowName); err != nil {
			return nil, err
		}
		return r.restore(ctx, binding)
	}
	repo := model.CanonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repository.Name)
	var selected *ExecutionTarget
	for _, definition := range r.definitions {
		expected, _, ref, _, _, _, _ := definition.Config.ExecutionPlacement.Identity()
		if request.Run.RefKind == model.RefBranch && repo == expected && request.Run.Ref == ref {
			copy := definition
			selected = &copy
		}
	}
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	binding = model.ExecutionTargetBinding{RefKind: string(request.Run.RefKind), Trigger: request.Run.Trigger, ScheduleEntry: request.Run.ScheduleEntry, RunID: request.Run.ID, Workflow: request.WorkflowName, Target: "local", Repository: repo, RepositoryID: request.Repository.ID, Ref: request.Run.Ref, SHA: request.Run.SHA, TestedSHA: sha, Namespace: r.local.config.Namespace, Snapshot: json.RawMessage(`{}`)}
	if selected == nil {
		canonical, err := r.store.Run(ctx, request.Run.ID)
		if err != nil {
			return nil, err
		}
		// Trusted preflight has its own earlier Workflow name. Keep its local plan.
		if canonical.WorkflowName != request.WorkflowName {
			return r.local, nil
		}
		if err := r.store.BindExecutionTarget(ctx, binding); err != nil {
			return nil, err
		}
		return r.local, nil
	}
	if request.Run.RefKind != model.RefBranch || request.Run.Release || request.Run.Credentialed || request.Run.Trigger != "branch" {
		return nil, errors.New("app: remote execution requires an ordinary branch run")
	}
	authority, err := workflowpolicy.ResolveBranchAuthority(ctx, r.owner.resourceBlobs, repo, sha)
	if err != nil {
		return nil, err
	}
	source, err := r.local.readRunSource(request)
	if err != nil {
		return nil, err
	}
	fragments, err := loadFragments(ctx, r.owner.fragments, source, pipeline.TriggerCI, request.Run.RefKind, request.Run.Trigger)
	if err != nil {
		return nil, err
	}
	file, err := r.local.runSourceFile(request)
	if err != nil {
		return nil, err
	}
	resolved, err := argoworkflow.ResolveRunSource(source, fragments, argoworkflow.RunSourceBinding{RunID: request.Run.ID, RepositoryID: request.Repository.ID, SHA: sha, File: file, Trigger: argoworkflow.RunTriggerBranch})
	if err != nil {
		return nil, err
	}
	credentialed, err := workflowCredentialed(resolved.Workflow())
	if err != nil {
		return nil, err
	}
	if credentialed {
		return nil, errors.New("app: remote execution cannot acquire credentialed authority")
	}
	submission := argojob.Request{BranchAuthority: authority, RunID: request.Run.ID, RepositoryID: request.Repository.ID, Name: request.WorkflowName, Repo: request.Repository.Name, UpstreamName: request.UpstreamName, UpstreamOrg: request.UpstreamOrg, Ref: request.Run.Ref, SHA: sha, PipelineFile: file, Trigger: pipeline.TriggerCI, RunTrigger: argoworkflow.RunTriggerBranch, Source: source, Fragments: fragments}
	if err := selected.Config.ExecutionPlacement.ValidateRequest(selected.Config, submission); err != nil {
		return nil, err
	}
	snapshot := targetSnapshot{Version: 1, Config: selected.Config, Source: source, Cleanup: map[string]string{"namespace": selected.Config.Namespace, "workflow": binding.Workflow, "source-pvc": binding.Workflow + "-src", "seed-pod": binding.Workflow + "-src", "collector-pod": binding.Workflow + "-src-collect"}}
	for _, fragment := range fragments {
		snapshot.Fragments = append(snapshot.Fragments, retainedFragment{Fragment: fragment, Source: append([]byte(nil), fragment.Source...)})
	}
	sort.Slice(snapshot.Fragments, func(i, j int) bool {
		return snapshot.Fragments[i].Fragment.Key.String() < snapshot.Fragments[j].Fragment.Key.String()
	})
	body, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	binding.Target = selected.ID
	binding.Namespace = selected.Config.Namespace
	binding.PolicyBaseline, binding.PolicyDigest = authority.Identity()
	binding.SourceDigest = targetBytesHash(source)
	binding.GraphDigest = resolved.GraphIdentity()
	binding.FragmentLockDigest = targetHash(resolved.Lock())
	binding.Snapshot = body
	if err := r.store.BindExecutionTarget(ctx, binding); err != nil {
		return nil, err
	}
	return r.restore(ctx, binding)
}

func matchTargetRun(b model.ExecutionTargetBinding, run model.Run, name string) error {
	sha := run.TestedSHA
	if sha == "" {
		sha = run.SHA
	}
	if b.RunID != run.ID || b.Workflow != name || b.RepositoryID != run.RepoID || b.Ref != run.Ref || b.SHA != run.SHA || b.TestedSHA != sha || b.RefKind != string(run.RefKind) || b.Trigger != run.Trigger || b.ScheduleEntry != run.ScheduleEntry || (b.Target != "local" && (run.Trigger != "branch" || run.ScheduleEntry != "" || run.Release || run.Credentialed)) {
		return errors.New("app: canonical run differs from execution target binding")
	}
	return nil
}

func (r *executionTargets) forName(ctx context.Context, name, runID string) (*ArgoJobs, error) {
	b, found, err := r.store.ExecutionTarget(ctx, runID, name)
	if err != nil {
		return nil, err
	}
	if !found {
		for _, d := range r.definitions {
			_, _, _, _, id, wf, _ := d.Config.ExecutionPlacement.Identity()
			if name == wf || (runID != "" && runID == id) {
				return nil, argojob.ErrTargetUncertain
			}
		}
		return r.local, nil // Historical unbound work predates remote routing.
	}
	run, err := r.store.Run(ctx, b.RunID)
	if err != nil {
		return nil, err
	}
	if err := matchTargetRun(b, run, name); err != nil {
		return nil, err
	}
	return r.restore(ctx, b)
}

// restore uses the durable configuration and sources, together with the one
// current authenticated transport for that ID. It never adopts mutable settings
// from a later target policy, nor probes any other target.
func (r *executionTargets) restore(ctx context.Context, b model.ExecutionTargetBinding) (result *ArgoJobs, resultErr error) {
	if b.Target == "local" {
		return r.local, nil
	}
	defer func() {
		if resultErr != nil {
			resultErr = fmt.Errorf("%w: %w", argojob.ErrTargetUncertain, resultErr)
		}
	}()
	definition, found := r.definitions[b.Target]
	if !found {
		return nil, fmt.Errorf("%w: original target bundle is unavailable", argojob.ErrTargetUncertain)
	}
	var snapshot targetSnapshot
	if json.Unmarshal(b.Snapshot, &snapshot) != nil || snapshot.Version != 1 || snapshot.Config.Namespace != b.Namespace || !snapshot.Config.ExecutionPlacement.BranchOnly() || targetBytesHash(snapshot.Source) != b.SourceDigest {
		return nil, errors.New("app: invalid retained execution target snapshot")
	}
	if err := snapshot.Config.ExecutionPlacement.VerifyTarget(definition.Transport); err != nil {
		return nil, err
	}
	// Never hold this lock through Git refreshes or canonical store I/O.
	// Unrelated local work and recovery must progress during a target outage.
	r.mu.Lock()
	defer r.mu.Unlock()
	if child, ok := r.children[b.RunID]; ok {
		return child, nil
	}
	snapshot.Config.CPUCapacity = definition.Config.CPUCapacity
	snapshot.Config.CPUAdmission = definition.Config.CPUAdmission
	snapshot.Config.CPUAdmissionDisabled = definition.Config.CPUAdmissionDisabled
	snapshot.Config.StepCPURequestMaxMilli = definition.Config.StepCPURequestMaxMilli
	journal := &targetJournal{store: r.store, binding: b}
	clients, err := argojob.NewTargetClients(definition.Workflows(b.Namespace), definition.Kubernetes, journal, snapshot.Config)
	if err != nil {
		return nil, err
	}
	snapshot.Config.Custody = journal
	controller, err := argojob.NewController(clients.WorkflowClient(), clients.Kubernetes(), snapshot.Config)
	if err != nil {
		return nil, err
	}
	if recorder, ok := r.owner.auditor.(argojob.TimingRecorder); ok {
		controller.WithTimingRecorder(recorder)
	}
	if recorder, ok := r.owner.auditor.(argojob.NodeBootRecorder); ok {
		controller.WithNodeBootRecorder(recorder)
	}
	if definition.ResourceSource != nil {
		recorder, ok := r.owner.auditor.(argojob.ResourceUsageRecorder)
		if !ok {
			return nil, errors.New("app: target resource sampler requires canonical storage")
		}
		controller.WithResourceSampler(definition.ResourceSource, recorder)
	}
	seeder := argojob.NewSourceSeeder(clients.Kubernetes(), definition.Exec, snapshot.Config)
	if seeder == nil {
		return nil, errors.New("app: target source delivery is unavailable")
	}
	child, err := NewArgoJobs(controller.WithSourceSeeder(seeder), snapshot.Config, r.owner.auditor, r.owner.secretAccess, retainedFragments(snapshot.Fragments))
	if err != nil {
		return nil, err
	}
	child.SetPinnedSources(r.owner.pinnedSources, r.owner.sourceRecorder)
	child.resourceBlobs = r.owner.resourceBlobs
	child.targetSource = snapshot.Source
	child.targetBinding = &b
	child.targetClients = clients
	child.SetWorkflowCapabilityLoader(r.owner.capabilityGrants)
	child.SetIdentityStore(r.owner.identities)
	child.SetDeclarationRecorder(r.owner.declarations)
	child.SetReconcilerHealth(r.owner.reconcilerHealthy)
	child.SetArtifacts(seeder, r.owner.artifacts, r.owner.artifactLimit)
	child.beforeCreate = func(ctx context.Context, request service.WorkflowRequest, submission argojob.Request) error {
		// Both audit and submitted Workflow use this exact capability/grant cut.
		workflow, err := argojob.Build(child.config, submission)
		if err != nil {
			return err
		}
		capsule := map[string]any{"workflow": workflow, "resource_maximum": submission.ResourceMaximum, "resource_ceiling": submission.ResourceCeiling, "pod_demand": submission.PodDemand, "resource_fallback": submission.ResourceFallback, "capability": submission.Capability, "identities": submission.Identities, "source_digest": b.SourceDigest, "graph_digest": b.GraphDigest, "fragment_lock_digest": b.FragmentLockDigest, "policy_digest": b.PolicyDigest}
		if submission.DemandReceipt != nil {
			capsule["demand_receipt"] = submission.DemandReceipt.Summary()
		}
		body, err := json.Marshal(capsule)
		if err != nil {
			return err
		}
		return r.store.BindExecutionTargetAdmission(ctx, b.RunID, body)
	}
	r.children[b.RunID] = child
	return child, nil
}

type retainedFragments []retainedFragment

func (f retainedFragments) Load(_ context.Context, key argoworkflow.FragmentKey, _ pipeline.Trigger, _ model.RefKind, _ string) (argoworkflow.Fragment, error) {
	for _, retained := range f {
		fragment := retained.Fragment
		if fragment.Key == key {
			fragment.Source = append([]byte(nil), retained.Source...)
			return fragment, nil
		}
	}
	return argoworkflow.Fragment{}, errors.New("app: fragment absent from retained target binding")
}

type targetJournal struct {
	store   *store.Store
	binding model.ExecutionTargetBinding
}

func (j *targetJournal) Binding() model.ExecutionTargetBinding { return j.binding }
func (j *targetJournal) Prepare(ctx context.Context, r model.ExecutionTargetResource) (bool, error) {
	return j.store.PrepareExecutionTargetResource(ctx, r)
}
func (j *targetJournal) Observe(ctx context.Context, kind, namespace, name, uid string) error {
	return j.store.ObserveExecutionTargetUID(ctx, j.binding.RunID, kind, namespace, name, uid)
}
func (j *targetJournal) Resources(ctx context.Context) ([]model.ExecutionTargetResource, error) {
	return j.store.ExecutionTargetResources(ctx, j.binding.RunID)
}
func (j *targetJournal) RetainOwned(ctx context.Context, r model.ExecutionTargetResource, uid string) error {
	return j.store.RetainExecutionTargetOwnedResource(ctx, r, uid)
}
func (j *targetJournal) Cancelled(ctx context.Context) (bool, error) {
	return j.store.ExecutionTargetCancelled(ctx, j.binding.RunID)
}

func (jobs *ArgoJobs) AwaitRunAdmission(ctx context.Context, request service.WorkflowRequest) error {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return err
		}
		return child.AwaitAdmission(ctx)
	}
	return jobs.AwaitAdmission(ctx)
}
func (jobs *ArgoJobs) WorkflowNamespaceForRun(ctx context.Context, run model.Run) (string, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, run.WorkflowName, run.ID)
		if err != nil {
			return "", err
		}
		return child.WorkflowNamespace(), nil
	}
	return jobs.WorkflowNamespace(), nil
}

// ExecutionTargetProfiles keeps each selected controller's own health visible.
// It performs no discovery, allocation or cluster reads on a health request.
func (jobs *ArgoJobs) ExecutionTargetProfiles() map[string]*model.ControllerProfileStatus {
	if jobs.routes == nil {
		return nil
	}
	r := jobs.routes
	r.mu.Lock()
	defer r.mu.Unlock()
	profiles := map[string]*model.ControllerProfileStatus{}
	for id := range r.definitions {
		profiles[id] = &model.ControllerProfileStatus{State: "unverified", AdmissionPaused: true}
	}
	keys := make([]string, 0, len(r.children))
	for id := range r.children {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	observed := map[string]bool{}
	for _, key := range keys {
		child := r.children[key]
		status := child.ControllerProfileStatus()
		if status == nil {
			continue
		}
		id := child.targetBinding.Target
		previous := profiles[id]
		if !observed[id] || (status.AdmissionPaused && !previous.AdmissionPaused) {
			profiles[id] = status
		}
		observed[id] = true
	}
	return profiles
}

// ReconcileTargetCleanup shares the canonical cleanup owner. One terminal run
// per bounded attempt; failed reads keep durable custody and rotate fairly.
func (jobs *ArgoJobs) ReconcileTargetCleanup(ctx context.Context) error {
	if jobs.routes == nil {
		return nil
	}
	r := jobs.routes
	r.mu.Lock()
	after := r.cleanupAfter
	r.mu.Unlock()
	b, found, err := r.store.NextExecutionTargetCleanup(ctx, after)
	if err != nil || !found {
		return err
	}
	r.mu.Lock()
	r.cleanupAfter = b.RunID
	r.mu.Unlock()
	child, err := r.forName(ctx, b.Workflow, b.RunID)
	if err != nil {
		return err
	}
	if err := r.store.CancelExecutionTarget(ctx, b.RunID); err != nil {
		return err
	}
	done, err := child.targetClients.Settle(ctx, time.Time{})
	if err != nil || !done {
		return err
	}
	return r.completeCleanup(ctx, b.RunID)
}

func (r *executionTargets) completeCleanup(ctx context.Context, runID string) error {
	if err := r.store.CompleteExecutionTargetCleanup(ctx, runID); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.children, runID)
	r.mu.Unlock()
	return nil
}
