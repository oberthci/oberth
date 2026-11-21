package argoworkflow

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestResourceSubmissionRequiresAccountUID(t *testing.T) {
	t.Parallel()
	for _, uid := range []string{"resource-uid", ""} {
		t.Run("uid="+uid, func(t *testing.T) {
			wf, fixture := resourceWorkflow(`apiVersion: example.test/v1
kind: Widget
metadata: {name: test, namespace: resources}`)
			r := requestFor(t, wf)
			r.IdentityRequest.ResourceAccount = "resources"
			hooks := identityHooks()
			verify := hooks.VerifyIdentities
			hooks.VerifyIdentities = func(ctx context.Context, request admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
				evidence, err := verify(ctx, request)
				evidence.Resource.UID = uid
				return evidence, err
			}
			transforms := 0
			hooks.TransformNative = func(context.Context, *wfv1.Workflow, admissioncontract.TransformInput) error {
				transforms++
				return nil
			}
			prepared, err := Prepare(t.Context(), r, fixture.Policy, hooks)
			if uid != "" {
				if err != nil || prepared == nil || transforms != 1 {
					t.Fatalf("verified resource identity refused: %v; transforms=%d", err, transforms)
				}
				if prepared.View().Identity.Resource.UID != uid {
					t.Fatal("resource UID missing from sealed evidence")
				}
				return
			}
			const want = ".oberth/build.yaml: identity: verified execution accounts are required; Ask the operator to configure and verify the pipeline and resource accounts."
			if err == nil || err.Error() != want || prepared != nil || transforms != 0 {
				t.Fatalf("missing resource UID: prepared=%v, err=%v, transforms=%d; want %s", prepared != nil, err, transforms, want)
			}
		})
	}
}

func TestCapabilityPolicyRefusalPreservesApprovedAuthority(t *testing.T) {
	t.Parallel()
	const proposal = `{"namespace":"runs","service_account":"resource","resource_kinds":[{"api_version":"v1","kind":"ConfigMap","actions":["create"]}]}`
	for _, configured := range []bool{false, true} {
		for _, tc := range []struct{ name, raw string }{
			{"protected namespace", strings.Replace(proposal, `"runs"`, `"oberth"`, 1)},
			{"pipeline namespace", strings.Replace(proposal, `"runs"`, `"ci"`, 1)},
			{"default account", strings.Replace(proposal, `"resource"`, `"default"`, 1)},
			{"ungranted action", strings.Replace(proposal, `"create"`, `"patch"`, 1)},
		} {
			t.Run(tc.name+"/configured="+strconv.FormatBool(configured), func(t *testing.T) {
				checkCapabilityReplacement(t, configured, proposal, tc.raw)
			})
		}
	}
}

func checkCapabilityReplacement(t *testing.T, configured bool, proposal, invalid string) {
	t.Helper()
	p := Policy{ServerNamespace: "oberth", ArgoNamespace: "controller"}
	var want *Capability
	if configured {
		prior := strings.Replace(proposal, `"runs"`, `"existing"`, 1)
		if err := p.LoadCapability(t.Context(), "ci", []byte(prior)); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(prior), &want); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.LoadCapability(t.Context(), "ci", []byte(invalid)); err == nil {
		t.Fatal("invalid authority accepted")
	}
	if !reflect.DeepEqual(p.Capability, want) {
		t.Fatalf("refused authority replaced policy: got %+v, want %+v", p.Capability, want)
	}
	if err := p.LoadCapability(t.Context(), "ci", []byte(proposal)); err != nil {
		t.Fatal("valid replacement refused", err)
	}
	if p.Capability.Namespace != "runs" || p.Capability.ServiceAccount != "resource" {
		t.Fatal("valid replacement not applied")
	}
}
