package argojob

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestTrustedOnlyRequestRequiresControllerProfileBeforePreparation(t *testing.T) {
	config, request := trustedKVMRequest(t)
	config.NonrootProfile = ""
	request.nonrootProof = nil
	controller, err := NewController(&staticWorkflowClient{}, fake.NewClientset(), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.PrepareNonroot(t.Context(), request); err == nil {
		t.Fatal("protected-only request accepted without qualified controller profile")
	}
	if _, err := Build(config, request); err == nil {
		t.Fatal("Build accepted missing private proof")
	}
}

func TestTrustedOnlyProofBindsIntentAndPreservesProducer(t *testing.T) {
	config, request, wf, _ := trustedKVMBuilt(t)
	if wf.Annotations[nonrootProfileAnnotation] == "" {
		t.Fatal("controller binding absent")
	}
	for _, tmpl := range wf.Spec.Templates {
		if tmpl.Name == "main" && tmpl.SecurityContext != nil {
			t.Fatal("ordinary producer identity altered")
		}
	}
	for _, mode := range []string{"missing", "expired", "different-intent"} {
		t.Run(mode, func(t *testing.T) {
			changed := request
			switch mode {
			case "missing":
				changed.nonrootProof = nil
			case "expired":
				proof := *request.nonrootProof
				proof.verifiedAt = time.Now().Add(-time.Minute)
				changed.nonrootProof = &proof
			case "different-intent":
				intent := *request.TrustedKVM
				intent.InventoryDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				changed.TrustedKVM = &intent
			}
			if _, err := Build(config, changed); err == nil {
				t.Fatal("unbound controller proof accepted")
			}
		})
	}
}

func TestTrustedOnlyRefreshRejectsControllerChangeBeforeSeedCreateOrAdoption(t *testing.T) {
	for _, mode := range []string{"before-seed", "after-seed", "adoption"} {
		t.Run(mode, func(t *testing.T) {
			config, request, existing, _ := trustedKVMBuilt(t)
			request.SourceDir = writeCheckout(t)
			request = prepareTrustedTestRequest(t, config, request)
			pod, cm := nonrootVerifierObjects()
			client := fake.NewClientset(pod, cm, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: existing.Spec.ServiceAccountName, Namespace: config.Namespace}})
			runningSeedPods(client)
			reads := 0
			failAt := 1
			if mode == "after-seed" {
				failAt = 3
			}
			if mode == "adoption" {
				failAt = 2
			}
			client.PrependReactor("get", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
				reads++
				if reads == failAt {
					changed := cm.DeepCopy()
					changed.ResourceVersion = "2"
					return true, changed, nil
				}
				return false, nil, nil
			})
			workflows := &nonrootWorkflowClient{}
			if mode == "adoption" {
				existing.Annotations[nonrootProfileAnnotation] = request.nonrootProof.binding()
				workflows.existing = existing
			}
			controller, err := NewController(workflows, client, config)
			if err != nil {
				t.Fatal(err)
			}
			seeds := 0
			var seedRecords []seedExecCall
			seedExec := recordingExec(&seedRecords)
			controller.WithSourceSeeder(NewSourceSeeder(client, func(ctx context.Context, ns, pod, container string, command []string, input io.Reader, stdout, stderr io.Writer) error {
				seeds++
				return seedExec(ctx, ns, pod, container, command, input, stdout, stderr)
			}, config))
			_, createErr := controller.Create(t.Context(), request)
			if reads != failAt || createErr == nil || !strings.Contains(createErr.Error(), "nonroot controller identity changed after the submission binding was prepared") {
				t.Fatalf("profile change boundary not reached: reads=%d want=%d err=%v", reads, failAt, createErr)
			}
			if workflows.creates != 0 {
				t.Fatal("producer workflow created after proof change")
			}
			if mode == "before-seed" && seeds != 0 {
				t.Fatal("source seeded before controller qualification")
			}
			if mode == "after-seed" && seeds == 0 {
				t.Fatal("post-seed boundary not exercised")
			}
		})
	}
}
