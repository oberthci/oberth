package app

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type protectedControllerFixture struct {
	*blockingController
	preparer             *argojob.Controller
	request              argojob.Request
	createErr            error
	failed               bool
	waitErr              error
	infrastructureReason string
	waitRunID            string
	cleaned              bool
	absent               bool
	absenceErr           error
	// observeConfig makes TrustedKVMWorkflowUID rebuild the submission the way
	// the real observer does before comparing it with the live Workflow.
	observeConfig *argojob.Config
	observed      argojob.Request
}

func (c *protectedControllerFixture) PrepareNonroot(ctx context.Context, r argojob.Request) (argojob.Request, error) {
	return c.preparer.PrepareNonroot(ctx, r)
}

// Use the actual pinned upstream-rendered controller template, then model only
// Kubernetes metadata/status and its standard service-account admission volume.
func protectedProfilePreparer(t *testing.T, config argojob.Config) *argojob.Controller {
	t.Helper()
	raw, err := os.ReadFile("../argojob/testdata/nonroot-argo4.0.8/controller-profile.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
		ConfigMap   corev1.ConfigMap       `json:"configMap"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	created := metav1.NewTime(time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC))
	cm := fixture.ConfigMap.DeepCopy()
	cm.Namespace = config.Namespace
	cm.UID = "config-uid"
	cm.ResourceVersion = "1"
	cm.CreationTimestamp = created
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "controller-fixture", Namespace: config.Namespace, UID: "pod-uid", Labels: map[string]string{"app.kubernetes.io/name": "argo-workflows-workflow-controller"}, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "controller", UID: "replicaset-uid", Controller: ptr.To(true)}}}, Spec: fixture.PodTemplate.Spec}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "kube-api-access-fixture", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0644)), Sources: []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To(int64(3607))}},
		{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
		{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
	}}}})
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "kube-api-access-fixture", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", Ready: true, ImageID: argoworkflow.NonrootControllerAMD64, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: created}}}}}
	controller, err := argojob.NewController(wffake.NewSimpleClientset().ArgoprojV1alpha1().Workflows(config.Namespace), fake.NewClientset(pod, cm), config)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func (c *protectedControllerFixture) ConfirmTrustedKVMAbsent(context.Context, string) error {
	if c.absenceErr != nil {
		return c.absenceErr
	}
	if c.absent {
		return nil
	}
	return argojob.ErrTrustedKVMWorkflowPresent
}
func (c *protectedControllerFixture) Namespace() string { return "test-pipeline" }
func (c *protectedControllerFixture) Create(_ context.Context, r argojob.Request) (string, error) {
	c.request = r
	return r.Name, c.createErr
}
func (c *protectedControllerFixture) Wait(_ context.Context, _ string, runID string, _ io.Writer) (argojob.Completion, error) {
	c.waitRunID = runID
	completion := argojob.Completion{Succeeded: !c.failed, Infrastructure: c.infrastructureReason != "", Reason: c.infrastructureReason}
	if c.failed {
		completion.Name, completion.Phase = "trusted", "Failed"
		completion.FailedBurn, completion.FailedStep = "test", "unit"
		completion.Steps = []argojob.StepResult{{Burn: "test", Step: "unit", Status: runprogress.StepFailed, ExitCode: 1}}
	}
	return completion, c.waitErr
}
func (c *protectedControllerFixture) TrustedKVMWorkflowUID(ctx context.Context, r argojob.Request) (string, error) {
	if c.createErr != nil {
		return "", errors.New("not observed")
	}
	c.observed = r
	if c.observeConfig != nil {
		prepared, err := c.preparer.PrepareNonroot(ctx, r)
		if err != nil {
			return "", err
		}
		if _, err = argojob.Build(*c.observeConfig, prepared); err != nil {
			return "", err
		}
	}
	return "workflow-uid", nil
}
func (c *protectedControllerFixture) CleanupTrustedKVM(context.Context, string, string, string) error {
	c.cleaned = true
	return nil
}
func (c *protectedControllerFixture) TrustedKVMReceipt(_ context.Context, r argojob.Request, uid string) (vmrunner.KVMReceipt, error) {
	intent := *r.TrustedKVM
	d := vmrunner.KVMIntentDigest(intent)
	now := time.Now().UTC()
	return vmrunner.KVMReceipt{IntentDigest: d, WorkflowUID: uid, Binding: vmrunner.KVMTerminationBinding{Namespace: intent.Namespace, WorkflowName: intent.WorkflowName, WorkflowUID: uid, PodName: "pod", PodUID: "pod-uid", ContainerID: "containerd://id", HarnessImageRef: intent.Recipe.HarnessImage, AdmittedAt: intent.AdmittedAt, Deadline: intent.Deadline}, Report: vmrunner.KVMReport{Version: 1, IntentDigest: d, InventoryDigest: intent.InventoryDigest, CaseCount: intent.CaseCount, ArtifactSetDigest: strings.Repeat("f", 64)}, Termination: vmrunner.KVMTerminationRecord{ImageID: "sha256:" + strings.Repeat("c", 64), ContainerID: "containerd://id", PodUID: "pod-uid", WorkflowUID: uid, StartedAt: intent.AdmittedAt, FinishedAt: now}, FinishedAt: now}, nil
}
func TestTrustedPreflightAdapterLifecycle(t *testing.T) {
	for _, scenario := range []string{"pass", "workflow failed", "workflow observation failed", "infrastructure failed", "infrastructure observation failed", "definite create refusal", "uncertain create", "recover absent", "unapproved", "guest granted", "guest revoked", "guest recover"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			fixture := newRaceFixture(t)
			// The grant names more VMs than the default budget (two).
			guestBudget := store.KubeVirtBudget{MaxVMs: 3, MaxCores: 4, MaxMemoryBytes: 8 << 30}
			grants := &guestGrantLoader{allowed: scenario != "guest revoked", budget: guestBudget}
			if strings.HasPrefix(scenario, "guest ") {
				if err := os.WriteFile(filepath.Join(fixture.sourceDir, ".oberth", "build.yaml"), []byte(guestTestDocument), 0644); err != nil {
					t.Fatal(err)
				}
				fixture.jobs.SetKubeVirtGrantLoader(grants)
			}
			controller := &protectedControllerFixture{blockingController: newBlockingController(), failed: strings.HasPrefix(scenario, "workflow ") || strings.HasPrefix(scenario, "infrastructure ")}
			if strings.Contains(scenario, "observation failed") {
				controller.waitErr = errors.New("log read failed")
			}
			if strings.HasPrefix(scenario, "infrastructure ") {
				controller.infrastructureReason = `Evicted: Usage of EmptyDir volume "var-run-argo" exceeds the limit "1Gi".`
			}
			if scenario == "definite create refusal" {
				controller.createErr = argojob.ErrTrustedKVMCreateRefused
			}
			if scenario == "uncertain create" || scenario == "recover absent" || scenario == "guest recover" {
				controller.createErr = errors.New("transport uncertainty")
			}
			fixture.jobs.controller = controller
			fixture.jobs.config.KVMEnabled = true
			fixture.jobs.config.TrustedKVMGroup = 1234
			fixture.jobs.config.NonrootProfile = argoworkflow.NonrootStaticProfile
			controller.preparer = protectedProfilePreparer(t, fixture.jobs.config)
			database, err := store.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "upstream", Kind: "ssh", BaseURL: "ssh://example.org/org"})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("a", 40)
			if _, err = database.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature", SHA: sha, TestedSHA: sha, Actor: "agent", Trigger: "schedule", ScheduleEntry: "nightly"}); err != nil {
				t.Fatal(err)
			}
			run, err := database.ClaimNextRun(ctx)
			if err != nil {
				t.Fatal(err)
			}
			guest := strings.Repeat("b", 64)
			manifest := []byte(fmt.Sprintf("version: 1\nprofile: kvm-suite-amd64-v1\nguest_sha256: %s\nartifacts: [program]\ncases: [one]\n", guest))
			var bundle bytes.Buffer
			tw := tar.NewWriter(&bundle)
			if err = tw.WriteHeader(&tar.Header{Name: "manifest.yaml", Size: int64(len(manifest)), Mode: 0444, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
				t.Fatal(err)
			}
			if _, err = tw.Write(manifest); err != nil {
				t.Fatal(err)
			}
			if err = tw.Close(); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(bundle.Bytes())
			suite := hex.EncodeToString(sum[:])
			recipe := vmrunner.KVMRecipe{SuiteSHA256: suite, GuestSHA256: guest, HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("c", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"}
			workspace := t.TempDir()
			p, err := NewTrustedPreflights(fixture.jobs, database, []vmrunner.KVMRecipe{recipe}, workspace, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			fetches := 0
			p.fetch = func(context.Context, string, string, int64) ([]byte, error) { fetches++; return bundle.Bytes(), nil }
			contract := vmrunner.Contract{Version: 2, Profile: vmrunner.KVMSuiteAMD64Profile, SuiteSHA256: suite, GuestSHA256: guest, Artifacts: []string{"program"}}
			if scenario == "unapproved" {
				contract.SuiteSHA256 = strings.Repeat("e", 64)
			}
			err = p.Execute(ctx, service.JobRequest{JobName: "run-test", Run: run, Repository: repo, UpstreamName: "upstream", UpstreamOrg: "org", SourceDir: fixture.sourceDir}, vmrunner.Requirement{CandidateSHA: sha, BaselineSHA: strings.Repeat("d", 40), Contract: &contract}, io.Discard)
			if scenario == "unapproved" {
				if err == nil || fetches != 0 {
					t.Fatal("unapproved content fetched")
				}
				return
			}
			if scenario == "guest revoked" {
				if err == nil || !strings.Contains(err.Error(), "admin grant") {
					t.Fatalf("revoked preflight: %v", err)
				}
				if _, getErr := database.KVMPreflight(ctx, run.ID); !errors.Is(getErr, store.ErrNotFound) {
					t.Fatalf("revoked guest preflight reserved capacity: %v", getErr)
				}
				return
			}
			if controller.request.ScheduleEntry != "nightly" {
				t.Fatalf("preflight lost durable entry: %+v", controller.request)
			}
			if scenario == "guest granted" {
				budget := controller.request.KubeVirtBudget
				if !controller.request.KubeVirtGranted || budget.MaxVMs != guestBudget.MaxVMs || budget.MaxCores != guestBudget.MaxCores || budget.MaxMemory.Value() != guestBudget.MaxMemoryBytes {
					t.Fatal("preflight lost the live VM grant or budget")
				}
			}
			state, getErr := database.KVMPreflight(ctx, run.ID)
			if getErr != nil {
				t.Fatalf("preflight did not journal: execute=%v journal=%v", err, getErr)
			}
			if scenario == "pass" || scenario == "guest granted" {
				if err != nil || state.Receipt == nil || state.Refusal != nil || !state.Cleaned || controller.waitRunID != run.ID {
					t.Fatalf("success binding: %v %#v waitID=%q", err, state, controller.waitRunID)
				}
			} else {
				if err == nil || state.Refusal == nil || state.Receipt != nil {
					t.Fatalf("failure lost negative receipt: %v %#v", err, state)
				}
				if strings.HasPrefix(scenario, "workflow ") || strings.HasPrefix(scenario, "infrastructure ") {
					var failure *service.TrustedPreflightFailure
					if !errors.As(err, &failure) || len(failure.Result.Steps) != 1 || failure.Result.Steps[0].ExitCode != 1 {
						t.Fatalf("preflight discarded failed step evidence: %v", err)
					}
					if strings.HasPrefix(scenario, "infrastructure ") {
						if failure.Result.Phase != "infrastructure" || failure.Result.FailedStep != "" {
							t.Fatalf("infrastructure classified as command failure: %#v", failure.Result)
						}
					} else if failure.Result.Phase != "test" || failure.Result.FailedStep != "unit" {
						t.Fatalf("command failure classified as infrastructure: %#v", failure.Result)
					}
				}
				if strings.HasPrefix(scenario, "infrastructure ") && !strings.Contains(err.Error(), controller.infrastructureReason) {
					t.Fatalf("trusted preflight hid kubelet message: %v", err)
				}
				if scenario == "uncertain create" || scenario == "recover absent" || scenario == "guest recover" {
					if state.Cleaned {
						t.Fatal("uncertain create released capacity")
					}
					if scenario == "guest recover" {
						// The Workflow exists and the grant has since been revoked.
						// Recovery rebuilds the exact spec for observation only and
						// must admit it under any budget a grant could have named,
						// never the default, so cleanup is not stranded.
						grants.allowed = false
						if e := os.CopyFS(filepath.Join(workspace, run.ID, "src"), os.DirFS(fixture.sourceDir)); e != nil {
							t.Fatal(e)
						}
						controller.createErr = nil
						observe := fixture.jobs.config
						controller.observeConfig = &observe
						ids, recoverErr := p.Recover(ctx)
						if recoverErr != nil || len(ids) != 1 || ids[0] != run.ID {
							t.Fatalf("recover granted guest preflight: %v %v", ids, recoverErr)
						}
						observed := controller.observed
						if observed.ScheduleEntry != "nightly" || !observed.KubeVirtGranted || observed.KubeVirtBudget.String() != argoworkflow.VMIBudgetLimit().String() {
							t.Fatalf("recovery observed with budget %v granted=%v", observed.KubeVirtBudget, observed.KubeVirtGranted)
						}
						state, recoverErr = database.KVMPreflight(ctx, run.ID)
						if recoverErr != nil || !state.Cleaned || state.WorkflowUID != "workflow-uid" || !controller.cleaned {
							t.Fatalf("granted guest preflight not reconciled: %#v %v", state, recoverErr)
						}
					}
					if scenario == "recover absent" {
						controller.absent = true
						if e := os.RemoveAll(fixture.sourceDir); e != nil {
							t.Fatal(e)
						}
						ids, recoverErr := p.Recover(ctx)
						if recoverErr != nil || len(ids) != 1 || ids[0] != run.ID {
							t.Fatalf("recover absent: %v %v", ids, recoverErr)
						}
						state, recoverErr = database.KVMPreflight(ctx, run.ID)
						if recoverErr != nil || !state.Rejected || !state.Cleaned || state.WorkflowUID != "" {
							t.Fatalf("absent create not rejected/cleaned: %#v %v", state, recoverErr)
						}
						if _, recoverErr = database.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "next", SHA: sha, TestedSHA: sha, Actor: "agent", Trigger: "branch"}); recoverErr != nil {
							t.Fatal(recoverErr)
						}
						next, recoverErr := database.ClaimNextRun(ctx)
						if recoverErr != nil {
							t.Fatal(recoverErr)
						}
						nextIntent := state.Intent
						nextIntent.RunID = next.ID
						nextIntent.WorkflowName = "next-trusted"
						if _, recoverErr = database.ReserveKVMPreflight(ctx, nextIntent); recoverErr != nil {
							t.Fatalf("VM slot not released: %v", recoverErr)
						}
					}
				} else if !state.Cleaned {
					t.Fatal("definite failure not cleaned")
				}
			}
		})
	}
}

// One unavailable API observation must not hide successful rows behind it.
type recoveryRowsStore struct {
	trustedRunStore
	rows []vmrunner.KVMExecution
}

func (s *recoveryRowsStore) PendingKVMPreflights(context.Context) ([]vmrunner.KVMExecution, error) {
	return s.rows, nil
}
func TestTrustedPreflightRecoveryContinuesAfterRowError(t *testing.T) {
	failure := errors.New("API observation unavailable")
	p := &TrustedPreflights{store: &recoveryRowsStore{rows: []vmrunner.KVMExecution{
		{Intent: vmrunner.KVMIntent{RunID: "pending", WorkflowName: "unknown"}, Submitted: true},
		{Intent: vmrunner.KVMIntent{RunID: "cleaned"}, Cleaned: true},
	}}, controller: &protectedControllerFixture{absenceErr: failure}}
	ids, err := p.Recover(context.Background())
	if !errors.Is(err, failure) || len(ids) != 1 || ids[0] != "cleaned" {
		t.Fatalf("partial recovery lost successful rows: %v %v", ids, err)
	}
}

func TestTrustedPreflightRecipesRejectMixedPairs(t *testing.T) {
	recipes := []vmrunner.KVMRecipe{
		{SuiteSHA256: strings.Repeat("a", 64), GuestSHA256: strings.Repeat("b", 64), HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("e", 64), SuiteURL: "https://example.org/suite-a", GuestURL: "https://example.org/guest-a"},
		{SuiteSHA256: strings.Repeat("c", 64), GuestSHA256: strings.Repeat("d", 64), HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("f", 64), SuiteURL: "https://example.org/suite-b", GuestURL: "https://example.org/guest-b"},
	}
	for _, recipe := range recipes {
		if err := recipe.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	p := &TrustedPreflights{recipes: recipes}
	for _, tc := range []struct {
		name         string
		suite, guest int
		allowed      bool
	}{
		{"recipe A", 0, 0, true},
		{"recipe B", 1, 1, true},
		{"suite A guest B", 0, 1, false},
		{"suite B guest A", 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := vmrunner.Contract{Version: 2, Profile: vmrunner.KVMSuiteAMD64Profile, SuiteSHA256: recipes[tc.suite].SuiteSHA256, GuestSHA256: recipes[tc.guest].GuestSHA256, Artifacts: []string{"program"}}
			if err := vmrunner.ValidateContract(contract); err != nil {
				t.Fatal(err)
			}
			got, err := p.recipe(contract)
			if tc.allowed {
				if err != nil || got != recipes[tc.suite] {
					t.Fatalf("recipe = %+v, %v", got, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "not operator-approved") {
				t.Fatalf("mixed recipe accepted: %+v, %v", got, err)
			}
			if err := p.Supports(contract); (err == nil) != tc.allowed {
				t.Fatalf("Supports = %v, allowed=%v", err, tc.allowed)
			}
			// Exercise candidate reconciliation too: both pins change from a
			// third baseline, so a mixed A/B pair must reach and fail the live
			// recipe check rather than merely the half-rotation guard.
			baseline := contract
			baseline.SuiteSHA256, baseline.GuestSHA256 = strings.Repeat("1", 64), strings.Repeat("2", 64)
			if _, err := vmrunner.RequiredContract(&contract, &baseline, p.Supports); (err == nil) != tc.allowed {
				t.Fatalf("candidate rotation = %v, allowed=%v", err, tc.allowed)
			}
		})
	}
}
