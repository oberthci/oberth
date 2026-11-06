package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func genericProtectedRequest(t *testing.T) (Config, Request) {
	t.Helper()
	config, r := trustedKVMRequest(t)
	config.KVMEnabled = false
	r.Source = []byte(strings.Replace(string(r.Source), "        command:", "        resources:\n          requests: {cpu: 100m, memory: 64Mi}\n          limits: {cpu: 1, memory: 64Mi}\n        command:", 1))
	digest := sha256.Sum256(r.Source)
	r.TrustedKVM.WorkflowSpecDigest = hex.EncodeToString(digest[:])
	c := &argoworkflow.Capability{Namespace: config.Namespace, ServiceAccount: "operator-runner", ExtendedResources: map[string]int64{protectedtests.GenericDeviceResource: 1}}
	a, err := protectedtests.NewGenericAdmission(*r.TrustedKVM, c, config.TrustedKVMGroup)
	if err != nil {
		t.Fatal(err)
	}
	r.GenericProtected = &a
	r.Capability = c
	return config, prepareTrustedTestRequest(t, config, r)
}

func TestGenericProtectedBuildKeepsHostWireAndRequiresExplicitGrant(t *testing.T) {
	config, r := genericProtectedRequest(t)
	wf, err := Build(config, r)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Annotations[genericProtectedAnnotation] != r.GenericProtected.Digest() || wf.Annotations[capabilityAnnotation] == "" {
		t.Fatal("runtime authority lost")
	}
	var leaf *wfv1.Template
	for i := range wf.Spec.Templates {
		if wf.Spec.Templates[i].Name == trustedKVMTemplate {
			leaf = &wf.Spec.Templates[i]
		}
	}
	if leaf == nil {
		t.Fatal("closed host leaf missing")
	}
	host, err := base64.RawURLEncoding.DecodeString(leaf.Container.Args[1])
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(r.TrustedKVM)
	if string(host) != string(want) {
		t.Fatal("server runtime metadata entered pinned host wire")
	}
	quantity := leaf.Container.Resources.Requests[corev1.ResourceName(protectedtests.GenericDeviceResource)]
	if quantity.String() != "1" {
		t.Fatal("generic device request missing")
	}
	for name, mutate := range map[string]func(*Request){
		"missing capability":      func(r *Request) { r.Capability = nil },
		"missing host":            func(r *Request) { r.TrustedKVM = nil },
		"missing explicit opt-in": func(r *Request) { r.GenericProtected = nil },
		"other repository":        func(r *Request) { r.RepositoryID++ },
		"changed host": func(r *Request) {
			copy := *r.TrustedKVM
			copy.InventoryDigest = strings.Repeat("1", 64)
			r.TrustedKVM = &copy
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := r
			mutate(&copy)
			if _, err := Build(config, copy); err == nil {
				t.Fatal("unbound runtime accepted")
			}
		})
	}
	altered := wf.DeepCopy()
	altered.Annotations[genericProtectedAnnotation] = "changed"
	if err := sameSubmission(altered, wf); err == nil {
		t.Fatal("changed runtime annotation accepted")
	}
}

func TestGenericProtectedRecoveryReconstructsRevokedGroupWithoutLiveAuthority(t *testing.T) {
	for _, group := range []int64{0, 4321, 1234} {
		t.Run(fmt.Sprint(group), func(t *testing.T) {
			config, r := genericProtectedRequest(t)
			wf, err := Build(config, r)
			if err != nil {
				t.Fatal(err)
			}
			wf.UID = "workflow-uid"
			wf.Status.StartedAt = metav1.NewTime(r.TrustedKVM.AdmittedAt)
			var leaf *wfv1.Template
			for i := range wf.Spec.Templates {
				if wf.Spec.Templates[i].Name == trustedKVMTemplate {
					leaf = &wf.Spec.Templates[i]
				}
			}
			pod := trustedKVMMaterializedPod(t, wf, leaf)
			report := protectedtests.Report{Version: 1, IntentDigest: protectedtests.IntentDigest(*r.TrustedKVM), InventoryDigest: r.TrustedKVM.InventoryDigest, ArtifactSetDigest: strings.Repeat("a", 64), CaseCount: r.TrustedKVM.CaseCount}
			body, _ := json.Marshal(report)
			pod.Status.Phase = corev1.PodSucceeded
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://main", ImageID: "sha256:" + strings.Repeat("e", 64), State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://main", StartedAt: metav1.NewTime(r.TrustedKVM.AdmittedAt.Add(time.Second)), FinishedAt: metav1.NewTime(r.TrustedKVM.AdmittedAt.Add(2 * time.Second)), Message: string(body)}}}}
			config.TrustedKVMGroup = group
			controller, err := NewController(&staticWorkflowClient{workflow: wf}, trustedControllerClient(pod, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: r.Capability.ServiceAccount, Namespace: config.Namespace}}), config)
			if err != nil {
				t.Fatal(err)
			}
			controller.WithWorkflowCapabilityLoader(func(context.Context, int64) (*argoworkflow.Capability, error) { return r.Capability, nil })
			if group == 1234 {
				if _, err = controller.TrustedKVMReceipt(t.Context(), r, string(wf.UID)); err != nil {
					t.Fatalf("current generic authority lost valid full observer receipt: %v", err)
				}
			} else {
				if _, err = controller.Create(t.Context(), r); err == nil {
					t.Fatal("current grant overrode group mismatch")
				}
				if _, err = controller.TrustedKVMReceipt(t.Context(), r, string(wf.UID)); err == nil {
					t.Fatal("current grant overrode receipt group mismatch")
				}
			}
			controller.WithWorkflowCapabilityLoader(func(context.Context, int64) (*argoworkflow.Capability, error) { return nil, nil })
			// Actual reconstruction verifies the original full Workflow spec despite a
			// reset/changed current group and revoked grant. UID is cleanup evidence only.
			uid, err := controller.TrustedKVMWorkflowUID(t.Context(), r)
			if err != nil || uid != string(wf.UID) {
				t.Fatalf("cleanup reconstruction stranded: uid=%s err=%v", uid, err)
			}
			if _, err = controller.Create(t.Context(), r); err == nil {
				t.Fatal("reconstruction authorized a new create")
			}
			if _, err = controller.TrustedKVMReceipt(t.Context(), r, uid); err == nil {
				t.Fatal("revoked/reconfigured runtime authorized a receipt")
			}
		})
	}
}
