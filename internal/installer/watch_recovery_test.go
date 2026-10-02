package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func recoveryEntries() []metav1.ManagedFieldsEntry {
	return []metav1.ManagedFieldsEntry{
		{Manager: watchLegacyManager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:labels":{"f:app.kubernetes.io/name":{}}},"f:spec":{"f:template":{"f:spec":{"f:initContainers":{"k:{\"name\":\"fetch-token\"}":{".":{},"f:name":{},"f:image":{},"f:args":{}}}}}}}`)}},
		{Manager: "deployment-controller", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apps/v1", Subresource: "status", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:observedGeneration":{}}}`)}},
	}
}

func TestWatchHandoffSubtractsOnlyOneLegacyLeaf(t *testing.T) {
	entries := recoveryEntries()
	before, _ := json.Marshal(entries)
	index, next, err := watchArgsHandoff(entries)
	if err != nil {
		t.Fatal(err)
	}
	afterOriginal, _ := json.Marshal(entries)
	if index != 0 || !bytes.Equal(before, afterOriginal) || !reflect.DeepEqual(entries[1], next[1]) {
		t.Fatal("handoff modified original or unrelated controller ownership")
	}
	want := recoveryEntries()
	want[0].FieldsV1.Raw = []byte(`{"f:metadata":{"f:labels":{"f:app.kubernetes.io/name":{}}},"f:spec":{"f:template":{"f:spec":{"f:initContainers":{"k:{\"name\":\"fetch-token\"}":{".":{},"f:name":{},"f:image":{}}}}}}}`)
	if !sameWatchManaged(next, want) {
		t.Fatal("handoff widened beyond args leaf")
	}
}

func TestWatchHandoffRefusesUnprovedOwnership(t *testing.T) {
	cases := map[string]func([]metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry{
		"foreign manager": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry { e[0].Manager = "terraform"; return e },
		"apply operation": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
			e[0].Operation = metav1.ManagedFieldsOperationApply
			return e
		},
		"alternate version": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
			e[0].APIVersion = "apps/v1beta1"
			return e
		},
		"subresource":     func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry { e[0].Subresource = "status"; return e },
		"field type":      func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry { e[0].FieldsType = "unknown"; return e },
		"duplicate entry": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry { return append(e, *e[0].DeepCopy()) },
		"shared field": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
			x := *e[0].DeepCopy()
			x.Manager = "other"
			return append(e, x)
		},
		"atomic ancestor": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
			e[0].FieldsV1.Raw = []byte(`{"f:spec":{}}`)
			return e
		},
		"duplicate trie key": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
			e[0].FieldsV1.Raw = []byte(`{"f:spec":{},"f:spec":{}}`)
			return e
		},
		"empty field": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
			e[0].FieldsV1.Raw = []byte(`{"f:":{}}`)
			return e
		},
		"foreign path": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
			e[0].FieldsV1.Raw = bytes.ReplaceAll(e[0].FieldsV1.Raw, []byte("fetch-token"), []byte("other-init"))
			return e
		},
		"empty trie": func(e []metav1.ManagedFieldsEntry) []metav1.ManagedFieldsEntry {
			e[0].FieldsV1.Raw = []byte(`{}`)
			return e
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := watchArgsHandoff(change(recoveryEntries())); err == nil {
				t.Fatal("unproved field owner accepted")
			}
		})
	}
}

func TestWatchConflictRequiresExactStructuredCause(t *testing.T) {
	entries := recoveryEntries()
	stamp := metav1.NewTime(time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC))
	entries[0].Time = &stamp
	o := watchRecoveryObject{Kind: "Deployment", Name: "cloudflared-watch-oberth-v2", ManagedFields: entries}
	cause := metav1.StatusCause{Type: metav1.CauseTypeFieldManagerConflict, Field: watchArgsField, Message: `conflict with "kubectl-client-side-apply" using apps/v1`}
	if !plannedWatchConflict(apierrors.NewApplyConflict([]metav1.StatusCause{cause}, "ignored raw summary"), o) {
		t.Fatal("exact endpoint-bound cause refused")
	}
	for _, mutate := range []func(*metav1.StatusCause){func(c *metav1.StatusCause) { c.Field = ".metadata.labels.app.kubernetes.io/name" }, func(c *metav1.StatusCause) { c.Message += " extra" }, func(c *metav1.StatusCause) { c.Message += " at 2026-09-30T01:02:03Z" }, func(c *metav1.StatusCause) { c.Type = metav1.CauseTypeFieldValueInvalid }} {
		bad := cause
		mutate(&bad)
		if plannedWatchConflict(apierrors.NewApplyConflict([]metav1.StatusCause{bad}, ""), o) {
			t.Fatal("foreign cause accepted")
		}
	}
	changed := append([]metav1.ManagedFieldsEntry(nil), entries...)
	moved := metav1.NewTime(stamp.Add(time.Second))
	changed[0].Time = &moved
	if sameWatchManaged(entries, changed) || !entries[0].Time.Equal(&stamp) {
		t.Fatal("conflict message normalization changed complete timestamp binding")
	}
	if plannedWatchConflict(apierrors.NewApplyConflict([]metav1.StatusCause{cause, cause}, ""), o) || plannedWatchConflict(apierrors.NewApplyConflict(nil, ""), o) || plannedWatchConflict(errors.New(cause.Message), o) {
		t.Fatal("ambiguous or unstructured conflict accepted")
	}
}

func TestWatchRecoveryPublicDeploymentAnnotations(t *testing.T) {
	const source = "github.com/oberthci/terraform//k8s/cloudflared-watch"
	metadata := func(revision, origin string) watchPublicMetadata {
		return watchPublicMetadata{Annotations: map[string]string{
			"deployment.kubernetes.io/revision": revision,
			"oberth.ci/source":                  origin,
		}}
	}
	const deployment = "cloudflared-watch-oberth-v2"
	for _, revision := range []string{"1", "2", "9223372036854775807"} {
		if err := validateRecoveryMetadata("Deployment", deployment, metadata(revision, source)); err != nil {
			t.Fatalf("canonical public deployment metadata refused: %v", err)
		}
	}
	for _, revision := range []string{"", "0", "-1", "+2", "02", "2 ", " 2", "2.0", "2e0", "9223372036854775808", strings.Repeat("1", 100)} {
		if validateRecoveryMetadata("Deployment", deployment, metadata(revision, source)) == nil {
			t.Fatalf("noncanonical or unbounded revision %q accepted", revision)
		}
	}

	allowed := []struct{ kind, name string }{
		{"ServiceAccount", "cloudflared-watch"},
		{"ConfigMap", "cloudflared-watch-openbao-ca"},
		{"ConfigMap", "cloudflared-watch-oberth-origin-ca"},
		{"Deployment", deployment},
	}
	for _, object := range allowed {
		if err := validateRecoveryMetadata(object.kind, object.name, watchPublicMetadata{Annotations: map[string]string{"oberth.ci/source": source}}); err != nil {
			t.Errorf("reviewed Terraform source marker refused for %s/%s: %v", object.kind, object.name, err)
		}
		if err := validateRecoveryMetadata(object.kind, object.name, watchPublicMetadata{}); err != nil {
			t.Errorf("missing optional source marker refused for %s/%s: %v", object.kind, object.name, err)
		}
	}

	for _, object := range []struct{ kind, name string }{
		{"ServiceAccount", "other"},
		{"ConfigMap", "cloudflared-watch"},
		{"Deployment", "other"},
		{"Secret", "cloudflared-watch"},
	} {
		if validateRecoveryMetadata(object.kind, object.name, watchPublicMetadata{Annotations: map[string]string{"oberth.ci/source": source}}) == nil {
			t.Errorf("Terraform source marker accepted for unreviewed %s/%s", object.kind, object.name)
		}
	}
	for _, origin := range []string{"", source + "/", "https://" + source, strings.Replace(source, "oberthci", "other", 1), strings.ToUpper(source)} {
		if validateRecoveryMetadata("Deployment", deployment, metadata("2", origin)) == nil {
			t.Fatal("unapproved source accepted")
		}
	}
	unknown := metadata("2", source)
	unknown.Annotations["oberth.ci/other"] = "public"
	if validateRecoveryMetadata("Deployment", deployment, unknown) == nil {
		t.Fatal("new annotation allowance admitted an unrelated key")
	}
	wrongKey := watchPublicMetadata{Annotations: map[string]string{"oberth.ci/source-path": source}}
	if validateRecoveryMetadata("Deployment", deployment, wrongKey) == nil {
		t.Fatal("Terraform source marker accepted under an unapproved key")
	}
}

type recoveryUnitState struct {
	commits     int
	dryCAS      int
	applies     int
	postFailure bool
	uncertain   bool
}

// This fake explicitly models protocol/receipts, not real SSA ownership.
// The separate real-API test qualifies Kubernetes managedFields semantics.
func recoveryUnitFixture(t *testing.T) (Config, Deps, *fake.Clientset, *recoveryUnitState, *bytes.Buffer) {
	t.Helper()
	cfg, deps, c, old, out := watchFixture(t)
	p := watchRecoveryPlan{Schema: watchRecoverySchema, Mode: "failed-adoption-recovery", Namespace: "oberth", ChartVersion: "v0.16.26", CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Minute), Release: watchReleaseGuard{Revision: 72, Status: "failed", Chart: "oberth-0.16.25", PreviousRevision: 71}}
	for _, o := range old.Objects {
		v, err := getWatchObject(context.Background(), deps, p.Namespace, o)
		if err != nil {
			t.Fatal(err)
		}
		v.meta.SetLabels(map[string]string{"app.kubernetes.io/name": "cloudflared-watch", "app.kubernetes.io/instance": "oberth", "app.kubernetes.io/managed-by": "Helm"})
		v.meta.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"})
		if o.Kind == "Deployment" {
			annotations := v.meta.GetAnnotations()
			annotations["deployment.kubernetes.io/revision"] = "2"
			annotations["oberth.ci/source"] = "github.com/oberthci/terraform//k8s/cloudflared-watch"
			v.meta.SetManagedFields(recoveryEntries())
		}
		var resource schema.GroupVersionResource
		switch o.Kind {
		case "Deployment":
			resource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		case "ServiceAccount":
			resource = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
		case "ConfigMap":
			resource = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
		}
		if err := c.Tracker().Update(resource, v.meta.(runtime.Object), p.Namespace); err != nil {
			t.Fatal(err)
		}
		current := v.meta.(runtime.Object)
		target := current.DeepCopyObject()
		tv, _ := watchSpec(target)
		tv.meta.SetUID("")
		tv.meta.SetResourceVersion("")
		tv.meta.SetManagedFields(nil)
		defaulted := target.DeepCopyObject()
		dv, _ := watchSpec(defaulted)
		dv.meta.SetUID(types.UID(o.UID))
		render, _ := json.Marshal(target)
		def, _ := json.Marshal(defaulted)
		entries := v.meta.GetManagedFields()
		if entries == nil {
			entries = []metav1.ManagedFieldsEntry{}
		}
		p.Objects = append(p.Objects, watchRecoveryObject{Kind: o.Kind, Name: o.Name, UID: o.UID, ResourceVersion: o.ResourceVersion, Generation: v.meta.GetGeneration(), Metadata: watchPublicMetadata{v.meta.GetLabels(), v.meta.GetAnnotations()}, ManagedFields: entries, Spec: v.spec, Target: render, DefaultedTarget: def})
	}
	cfg.watchRecovery = &p
	cfg.watchChart = "reviewed-synthetic-chart"
	cfg.ChartVersion = p.ChartVersion
	deps.RunHelm = func(_ context.Context, args []string) ([]byte, error) {
		if args[0] != "upgrade" || !isWatchPreview(args) {
			t.Fatalf("unexpected Helm effect: %v", args)
		}
		var manifest strings.Builder
		for _, o := range p.Objects {
			manifest.Write(o.Target)
			manifest.WriteString("\n---\n")
		}
		return json.Marshal(map[string]any{"name": "oberth", "namespace": "oberth", "version": 73, "manifest": manifest.String(), "chart": map[string]any{"metadata": map[string]any{"name": "oberth", "version": "0.16.26"}}})
	}
	state := &recoveryUnitState{}
	c.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		x := a.(ktesting.PatchAction)
		opts := a.(interface{ GetPatchOptions() metav1.PatchOptions }).GetPatchOptions()
		current, err := c.Tracker().Get(a.GetResource(), a.GetNamespace(), x.GetName())
		if err != nil {
			return true, nil, err
		}
		v, _ := watchSpec(current)
		if x.GetPatchType() == types.ApplyPatchType {
			state.applies++
			if opts.Force == nil || *opts.Force || opts.FieldManager != "helm" || !reflect.DeepEqual(opts.DryRun, []string{metav1.DryRunAll}) || opts.FieldValidation != "Strict" {
				t.Fatal("SSA options widened")
			}
			if state.commits > 0 && state.postFailure {
				return true, nil, errors.New("post-handoff preflight failure")
			}
			if a.GetResource().Resource == "deployments" {
				for _, entry := range v.meta.GetManagedFields() {
					tree, _ := decodeWatchTrie(entry.FieldsV1.Raw)
					owned, _ := ownsWatchArgs(tree)
					if owned {
						return true, nil, apierrors.NewApplyConflict([]metav1.StatusCause{{Type: metav1.CauseTypeFieldManagerConflict, Field: watchArgsField, Message: watchLegacyConflictMessage(v.meta.GetManagedFields())}}, "")
					}
				}
			}
			result := current.DeepCopyObject()
			if json.Unmarshal(x.GetPatch(), result) != nil {
				t.Fatal("bad apply")
			}
			return true, result, nil
		}
		if x.GetPatchType() != types.JSONPatchType || opts.Force != nil || opts.FieldManager != "oberth-watch-adoption" {
			t.Fatal("unexpected metadata effect")
		}
		patch, err := jsonpatch.DecodePatch(x.GetPatch())
		if err != nil {
			return true, nil, err
		}
		raw, _ := json.Marshal(current)
		next, err := patch.Apply(raw)
		if err != nil {
			return true, nil, err
		}
		result := current.DeepCopyObject()
		if err = json.Unmarshal(next, result); err != nil {
			return true, nil, err
		}
		if len(opts.DryRun) > 0 {
			state.dryCAS++
			return true, result, nil
		}
		state.commits++
		rv, _ := watchSpec(result)
		rv.meta.SetResourceVersion("1" + rv.meta.GetResourceVersion())
		if err = c.Tracker().Update(a.GetResource(), result, a.GetNamespace()); err != nil {
			return true, nil, err
		}
		if state.uncertain {
			return true, nil, errors.New("lost committed reply")
		}
		return true, result, nil
	})
	return cfg, deps, c, state, out
}

func TestWatchRecoveryRetainsHandoffReceiptOnPostFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "post SSA failure"}[fail], func(t *testing.T) {
			cfg, deps, c, state, out := recoveryUnitFixture(t)
			state.postFailure = fail
			receipt, err := applyWatchRecovery(context.Background(), cfg, deps, false, func(p watchRecoveryPlan) error {
				if !watchRecoveryDeadline(p) {
					return errors.New("expired")
				}
				return nil
			})
			if (err != nil) != fail || len(receipt) != 1 || state.commits != 1 || state.dryCAS != 1 || !strings.Contains(out.String(), `"uid":"deploy-uid"`) {
				t.Fatalf("wrong handoff result err=%v receipt=%v commits=%d output=%q", err, receipt, state.commits, out.String())
			}
			v, err := c.AppsV1().Deployments("oberth").Get(context.Background(), "cloudflared-watch-oberth-v2", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !sameWatchJSON(mustWatchSpec(t, v), cfg.watchRecovery.Objects[3].Spec) {
				t.Fatal("handoff changed spec")
			}
			if cfg.watchRecovery.ready == fail {
				t.Fatal("qualification readiness does not match outcome")
			}
		})
	}
}

func writeResumeEvidence(t *testing.T, value any) watchPublicFile {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return writeResumeRawEvidence(t, raw)
}

func writeResumeRawEvidence(t *testing.T, raw []byte) watchPublicFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	return watchPublicFile{Path: path, SHA256: hex.EncodeToString(hash[:])}
}

func makeRecoveryResume(t *testing.T, prior watchRecoveryPlan, committed watchRecoveryPlan, receipt []watchAdopted) watchRecoveryPlan {
	t.Helper()
	priorBlob := writeResumeEvidence(t, prior)
	receiptBlob := writeResumeEvidence(t, receipt)
	committed.Mode = "post-handoff-resume"
	committed.Resume = &watchRecoveryResume{PriorPlan: priorBlob, HandoffReceipt: receiptBlob}
	return committed
}

func TestWatchRecoveryBoundedResumeValidationAndNoWriteExecution(t *testing.T) {
	cfg, deps, client, state, out := recoveryUnitFixture(t)
	prior := *cfg.watchRecovery
	prior.Context, prior.ClusterUID, prior.ServerVersion = "ctx", "cluster", "v1.36.2+k3s1"
	prior.ValuesSHA256 = watchValuesDigest([]byte(`{}`))
	prior.Release.RecordUID, prior.Release.RecordRV = "record-uid", "72"
	prior.Origin = &watchRecoveryOrigin{Plan: watchPublicFile{Path: "/original/plan", SHA256: originalWatchPlanSHA}, Receipt: watchPublicFile{Path: "/original/receipt", SHA256: originalWatchReceiptSHA}, Confirmed: []watchAdopted{}}
	*cfg.watchRecovery = prior
	state.postFailure = true
	oldReceipt, err := applyWatchRecovery(context.Background(), cfg, deps, false, func(watchRecoveryPlan) error { return nil })
	if err == nil || len(oldReceipt) != 1 || state.commits != 1 {
		t.Fatalf("expected confirmed metadata handoff followed by failure, receipt=%v commits=%d err=%v", oldReceipt, state.commits, err)
	}
	committed := *cfg.watchRecovery
	state.postFailure = false
	resume := makeRecoveryResume(t, prior, committed, oldReceipt)
	if err = validateRecoveryResume(resume); err != nil {
		t.Fatalf("valid receipt-bound resume refused: %v", err)
	}
	for name, mutate := range map[string]func(*watchRecoveryPlan, []watchAdopted){
		"wrong_prior_mode": func(p *watchRecoveryPlan, _ []watchAdopted) {
			wrong := prior
			wrong.Mode = "post-handoff-resume"
			p.Resume.PriorPlan = writeResumeEvidence(t, wrong)
		},
		"receipt_rv_mismatch": func(p *watchRecoveryPlan, rows []watchAdopted) {
			rows[0].ResourceVersion = "999"
			p.Resume = &watchRecoveryResume{PriorPlan: p.Resume.PriorPlan, HandoffReceipt: writeResumeEvidence(t, rows)}
		},
		"receipt_uid_mismatch": func(p *watchRecoveryPlan, rows []watchAdopted) {
			rows[0].UID = "foreign"
			p.Resume = &watchRecoveryResume{PriorPlan: p.Resume.PriorPlan, HandoffReceipt: writeResumeEvidence(t, rows)}
		},
		"duplicate_receipt_key": func(p *watchRecoveryPlan, rows []watchAdopted) {
			row, _ := json.Marshal(rows[0])
			duplicate := strings.TrimSuffix(string(row), "}") + `,"kind":"Deployment"}`
			p.Resume.HandoffReceipt = writeResumeRawEvidence(t, []byte("["+duplicate+"]"))
		},
		"prior_hash_mismatch": func(p *watchRecoveryPlan, _ []watchAdopted) { p.Resume.PriorPlan.SHA256 = strings.Repeat("0", 64) },
		"remaining_args_owner": func(p *watchRecoveryPlan, _ []watchAdopted) {
			for i := range p.Objects {
				if p.Objects[i].Kind == "Deployment" {
					p.Objects[i].ManagedFields = prior.Objects[i].ManagedFields
				}
			}
		},
		"spec_drift": func(p *watchRecoveryPlan, _ []watchAdopted) {
			for i := range p.Objects {
				if p.Objects[i].Kind == "Deployment" {
					p.Objects[i].Spec = json.RawMessage(`{"drift":true}`)
				}
			}
		},
		"other_object_rv_drift": func(p *watchRecoveryPlan, _ []watchAdopted) {
			for i := range p.Objects {
				if p.Objects[i].Kind == "ConfigMap" {
					p.Objects[i].ResourceVersion = "999"
					return
				}
			}
		},
		"deployment_uid_drift": func(p *watchRecoveryPlan, _ []watchAdopted) {
			for i := range p.Objects {
				if p.Objects[i].Kind == "Deployment" {
					p.Objects[i].UID = "foreign"
					return
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := resume
			resumeBinding := *resume.Resume
			candidate.Resume = &resumeBinding
			candidate.Objects = append([]watchRecoveryObject(nil), resume.Objects...)
			rows := append([]watchAdopted(nil), oldReceipt...)
			mutate(&candidate, rows)
			if validateRecoveryResume(candidate) == nil {
				t.Fatal("invalid resume evidence accepted")
			}
		})
	}

	beforeResumeApplies := state.applies
	expired := resume
	expired.CreatedAt, expired.ExpiresAt = time.Now().Add(-2*time.Minute), time.Now().Add(-time.Minute)
	*cfg.watchRecovery = expired
	if _, err = applyWatchRecovery(context.Background(), cfg, deps, false, func(p watchRecoveryPlan) error {
		if !watchRecoveryDeadline(p) {
			return errors.New("expired")
		}
		return nil
	}); err == nil || state.applies != beforeResumeApplies || state.commits != 1 || state.dryCAS != 1 {
		t.Fatal("expired resume reached effects")
	}
	*cfg.watchRecovery = resume
	state.postFailure = true
	if _, err = applyWatchRecovery(context.Background(), cfg, deps, false, func(watchRecoveryPlan) error { return nil }); err == nil || cfg.watchRecovery.ready || state.commits != 1 || state.dryCAS != 1 {
		t.Fatal("failed resumed SSA wrote metadata or became ready")
	}
	state.postFailure = false
	client.ClearActions()
	*cfg.watchRecovery = resume
	beforeResumeApplies = state.applies
	returned, err := applyWatchRecovery(context.Background(), cfg, deps, false, func(watchRecoveryPlan) error { return nil })
	if err != nil || len(returned) != 1 || !cfg.watchRecovery.ready || state.commits != 1 || state.dryCAS != 1 || state.applies-beforeResumeApplies != 4 {
		t.Fatalf("resume did not qualify without another write: receipt=%v commits=%d dryCAS=%d applies=%d err=%v", returned, state.commits, state.dryCAS, state.applies, err)
	}
	for _, action := range client.Actions() {
		if patched, ok := action.(ktesting.PatchAction); ok && patched.GetPatchType() == types.JSONPatchType {
			t.Fatal("resume issued another metadata JSONPatch")
		}
	}
	if !strings.Contains(out.String(), "confirmed handoff resumed") {
		t.Fatal("resume receipt was not emitted")
	}
}
func mustWatchSpec(t *testing.T, obj any) []byte {
	t.Helper()
	v, err := watchSpec(obj)
	if err != nil {
		t.Fatal(err)
	}
	return v.spec
}

func TestWatchRecoveryAllFourPreflightAndUnknownReplyStop(t *testing.T) {
	for index := 0; index < 4; index++ {
		cfg, deps, c, state, _ := recoveryUnitFixture(t)
		o := cfg.watchRecovery.Objects[index]
		v, err := getWatchObject(context.Background(), deps, "oberth", watchAdoptionObject{Kind: o.Kind, Name: o.Name})
		if err != nil {
			t.Fatal(err)
		}
		v.meta.SetUID("replacement")
		var resource schema.GroupVersionResource
		switch o.Kind {
		case "Deployment":
			resource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
		case "ServiceAccount":
			resource = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
		case "ConfigMap":
			resource = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
		}
		if err = c.Tracker().Update(resource, v.meta.(runtime.Object), "oberth"); err != nil {
			t.Fatal(err)
		}
		if _, err = applyWatchRecovery(context.Background(), cfg, deps, false, func(watchRecoveryPlan) error { return nil }); err == nil || state.commits != 0 || state.applies != 0 {
			t.Fatal("a preflight drift reached effects")
		}
	}
	cfg, deps, _, state, _ := recoveryUnitFixture(t)
	state.uncertain = true
	receipt, err := applyWatchRecovery(context.Background(), cfg, deps, false, func(watchRecoveryPlan) error { return nil })
	if err == nil || len(receipt) != 0 || state.commits != 1 || state.applies != 4 {
		t.Fatal("uncertain CAS reply retried or reached post apply")
	}
}

func TestWatchRecoveryRevisionAndExpiryGuardsStopBeforeCommit(t *testing.T) {
	for _, boundary := range []string{"initial", "after CAS dryrun", "after commit"} {
		t.Run(boundary, func(t *testing.T) {
			cfg, deps, _, state, _ := recoveryUnitFixture(t)
			receipt, err := applyWatchRecovery(context.Background(), cfg, deps, false, func(p watchRecoveryPlan) error {
				if boundary == "initial" || boundary == "after CAS dryrun" && state.dryCAS > 0 || boundary == "after commit" && state.commits > 0 {
					return errors.New("revision/deadline guard moved")
				}
				return nil
			})
			want := 0
			if boundary == "after commit" {
				want = 1
			}
			if err == nil || state.commits != want || len(receipt) != want {
				t.Fatalf("continued at %s receipt=%v commits=%d err=%v", boundary, receipt, state.commits, err)
			}
		})
	}
}

func TestWatchReleaseSignatureAndSourcePin(t *testing.T) {
	committed, err := os.ReadFile("../../.oberth/pins/release-cosign.pub")
	if err != nil || !bytes.Equal(committed, []byte(watchReleasePublicKey)) {
		t.Fatal("watch trust key diverges from release authority")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("synthetic public release")
	hash := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, key, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := json.Marshal(map[string]any{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json", "verificationMaterial": map[string]any{"publicKey": map[string]any{"hint": "synthetic"}}, "messageSignature": map[string]any{"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": base64.StdEncoding.EncodeToString(hash[:])}, "signature": base64.StdEncoding.EncodeToString(sig)}})
	if verifyWatchBundleKey(payload, bundle, &key.PublicKey) != nil {
		t.Fatal("valid detached signature refused")
	}
	if verifyWatchBundleKey([]byte("altered"), bundle, &key.PublicKey) == nil || verifyWatchBundle(payload, bundle) == nil {
		t.Fatal("changed payload or caller signing key trusted")
	}
}

func TestWatchRecoveryStrictPublicPlan(t *testing.T) {
	cfg, _, _, _, _ := recoveryUnitFixture(t)
	p := *cfg.watchRecovery
	p.Context = "reviewed-context"
	p.ClusterUID = "reviewed-cluster"
	p.ServerVersion = "v1.36.2+k3s1"
	p.ValuesSHA256 = watchValuesDigest([]byte(`{}`))
	p.Release.RecordUID = "failed-record-uid"
	p.Release.RecordRV = "99"
	p.Origin = &watchRecoveryOrigin{Plan: watchPublicFile{"/public/original-plan", originalWatchPlanSHA}, Receipt: watchPublicFile{"/public/original-receipt", originalWatchReceiptSHA}, Confirmed: []watchAdopted{}}
	cfg.WatchAdoptionPlan = filepath.Join(t.TempDir(), "plan.json")
	base, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(cfg.WatchAdoptionPlan, base, 0600) != nil {
		t.Fatal("plan fixture write failed")
	}
	if _, err = readWatchRecoveryPlan(cfg); err != nil {
		t.Fatal("valid public plan shape refused", err)
	}
	var historical map[string]any
	if json.Unmarshal(base, &historical) != nil {
		t.Fatal("historical plan fixture decode failed")
	}
	delete(historical, "resume")
	historicalRaw, _ := json.Marshal(historical)
	if os.WriteFile(cfg.WatchAdoptionPlan, historicalRaw, 0600) != nil {
		t.Fatal("historical plan fixture write failed")
	}
	if _, err = readWatchRecoveryPlan(cfg); err != nil {
		t.Fatalf("historical plan without resume key refused: %v", err)
	}
	if os.WriteFile(cfg.WatchAdoptionPlan, base, 0600) != nil {
		t.Fatal("current plan fixture restore failed")
	}
	wrongMode := p
	wrongMode.Resume = &watchRecoveryResume{PriorPlan: watchPublicFile{Path: "/prior", SHA256: strings.Repeat("a", 64)}, HandoffReceipt: watchPublicFile{Path: "/receipt", SHA256: strings.Repeat("b", 64)}}
	wrongModeRaw, _ := json.Marshal(wrongMode)
	if os.WriteFile(cfg.WatchAdoptionPlan, wrongModeRaw, 0600) != nil {
		t.Fatal("wrong-mode plan fixture write failed")
	}
	if _, err = readWatchRecoveryPlan(cfg); err == nil {
		t.Fatal("normal recovery mode accepted resume evidence")
	}
	if os.WriteFile(cfg.WatchAdoptionPlan, base, 0600) != nil {
		t.Fatal("current plan fixture restore failed")
	}
	unprepared := cfg
	unprepared.watchRecovery = nil
	_, deps, client, state, _ := recoveryUnitFixture(t)
	client.ClearActions()
	if _, err = adoptWatchTunnelReceipt(context.Background(), unprepared, deps, false); err == nil || state.commits != 0 || len(client.Actions()) != 0 {
		t.Fatal("unprepared public v2 plan reached API effects")
	}
	for _, change := range []func(map[string]any){
		func(m map[string]any) { delete(m, "values_sha256") },
		func(m map[string]any) { m["values_sha256"] = nil },
		func(m map[string]any) { m["values_sha256"] = 17 },
		func(m map[string]any) { m["values_sha256"] = strings.Repeat("A", 64) },
		func(m map[string]any) { m["Namespace"] = m["namespace"]; delete(m, "namespace") },
		func(m map[string]any) { m["origin"] = nil },
		func(m map[string]any) { m["mode"] = "initial" },
		func(m map[string]any) { m["release"].(map[string]any)["revision"] = 73 },
		func(m map[string]any) { m["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano) },
		func(m map[string]any) { m["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano) },
		func(m map[string]any) {
			m["objects"].([]any)[3].(map[string]any)["managed_fields"].([]any)[0].(map[string]any)["Manager"] = watchLegacyManager
		},
		func(m map[string]any) {
			m["objects"].([]any)[0].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)["foreign"] = "unreviewed"
		},
	} {
		var fields map[string]any
		if json.Unmarshal(base, &fields) != nil {
			t.Fatal("fixture decode failed")
		}
		change(fields)
		raw, _ := json.Marshal(fields)
		if os.WriteFile(cfg.WatchAdoptionPlan, raw, 0600) != nil {
			t.Fatal("fixture write failed")
		}
		if _, err = readWatchRecoveryPlan(cfg); err == nil {
			t.Fatal("unreviewed public plan accepted")
		}
	}
	if recoveryOrigin(p) == nil {
		t.Fatal("synthetic original receipt authorized production origin")
	}
}

func TestWatchPublicFileBoundAndCASFullMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public.json")
	if os.WriteFile(path, []byte(`{"public":true}`), 0600) != nil {
		t.Fatal("fixture write failed")
	}
	if _, err := boundedWatchFile(path, 4); err == nil {
		t.Fatal("oversize read accepted")
	}
	link := path + "-link"
	if os.Symlink(path, link) != nil {
		t.Fatal("fixture symlink failed")
	}
	if _, err := boundedWatchFile(link, 1024); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := watchFile(watchPublicFile{Path: path, SHA256: strings.Repeat("0", 64)}, 1024); err == nil {
		t.Fatal("unbound public bytes accepted")
	}
	cfg, deps, _, _, _ := recoveryUnitFixture(t)
	o := cfg.watchRecovery.Objects[3]
	v, err := getWatchObject(context.Background(), deps, "oberth", watchAdoptionObject{Kind: o.Kind, Name: o.Name})
	if err != nil {
		t.Fatal(err)
	}
	body, err := watchMetadataCAS(v, 0)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := jsonpatch.DecodePatch(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(metav1.Object){func(m metav1.Object) { m.SetUID("replacement") }, func(m metav1.Object) { m.SetResourceVersion("foreign-rv") }, func(m metav1.Object) {
		entries := m.GetManagedFields()
		entries[0].Manager = "foreign"
		m.SetManagedFields(entries)
	}, func(m metav1.Object) {
		labels := m.GetLabels()
		labels["app.kubernetes.io/name"] = "foreign"
		m.SetLabels(labels)
	}} {
		obj := v.meta.(runtime.Object).DeepCopyObject()
		mv, _ := watchSpec(obj)
		change(mv.meta)
		raw, _ := json.Marshal(obj)
		if _, err = patch.Apply(raw); err == nil {
			t.Fatal("CAS did not bind UID/RV/full metadata entry")
		}
	}
}

func TestWatchRecoveryRunnerExcludesAmbientAuthority(t *testing.T) {
	for _, name := range []string{"HELM_KUBETOKEN", "HELM_KUBEASUSER", "HELM_KUBEINSECURE_SKIP_TLS_VERIFY", "HELM_DRIVER", "KUBECONFIG", "HTTPS_PROXY", "HELM_PLUGINS"} {
		t.Setenv(name, "synthetic-forbidden-authority")
	}
	raw, err := runBoundedRecoveryHelm(context.Background(), "/usr/bin/env", nil)
	if err != nil || bytes.Contains(raw, []byte("synthetic-forbidden-authority")) || !bytes.Contains(raw, []byte("HELM_DRIVER=secret\n")) || !bytes.Contains(raw, []byte("HELM_NO_PLUGINS=1\n")) {
		t.Fatal("ambient authority reached the pinned recovery runner")
	}
}

func TestWatchRecoveryFreezesOneIdentityAndRejectsAuthorityHooks(t *testing.T) {
	// Predictable public Go test data; no key generation or real credential.
	cert, err := os.ReadFile("testdata/watch-public-identity/cert.pem")
	if err != nil {
		t.Fatal("public certificate fixture unavailable")
	}
	key, err := os.ReadFile("testdata/watch-public-identity/key.pem")
	if err != nil {
		t.Fatal("public key fixture unavailable")
	}
	base := &rest.Config{Host: "https://synthetic-watch.invalid:6443", TLSClientConfig: rest.TLSClientConfig{CAData: cert, CertData: cert, KeyData: key, ServerName: "synthetic-watch.invalid"}}
	for name, change := range map[string]func(*rest.Config){
		"impersonate username": func(c *rest.Config) { c.Impersonate.UserName = "synthetic-other" },
		"impersonate UID":      func(c *rest.Config) { c.Impersonate.UID = "synthetic-uid" },
		"impersonate groups":   func(c *rest.Config) { c.Impersonate.Groups = []string{"synthetic-group"} },
		"impersonate extra":    func(c *rest.Config) { c.Impersonate.Extra = map[string][]string{"synthetic": {"other"}} },
		"transport":            func(c *rest.Config) { c.Transport = &http.Transport{} },
		"wrapper":              func(c *rest.Config) { c.WrapTransport = func(t http.RoundTripper) http.RoundTripper { return t } },
		"proxy":                func(c *rest.Config) { c.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil } },
		"dial": func(c *rest.Config) {
			c.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("never called") }
		},
		"exec":          func(c *rest.Config) { c.ExecProvider = &clientcmdapi.ExecConfig{Command: "never-executed"} },
		"auth provider": func(c *rest.Config) { c.AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "never-executed"} },
		"token file":    func(c *rest.Config) { c.BearerTokenFile = "/unread-dynamic-token" },
		"basic":         func(c *rest.Config) { c.Username = "synthetic" },
		"insecure":      func(c *rest.Config) { c.Insecure = true },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := rest.CopyConfig(base)
			change(candidate)
			if _, err := freezeWatchRecoveryConfig(candidate); err == nil {
				t.Fatal("unsupported selected authority accepted")
			}
		})
	}
	dir := t.TempDir()
	fileConfig := rest.CopyConfig(base)
	for name, raw := range map[string][]byte{"ca": base.CAData, "cert": base.CertData, "key": base.KeyData} {
		if os.WriteFile(filepath.Join(dir, name), raw, 0600) != nil {
			t.Fatal("synthetic credential fixture failed")
		}
	}
	fileConfig.CAData, fileConfig.CertData, fileConfig.KeyData = nil, nil, nil
	fileConfig.CAFile, fileConfig.CertFile, fileConfig.KeyFile = filepath.Join(dir, "ca"), filepath.Join(dir, "cert"), filepath.Join(dir, "key")
	frozen, err := freezeWatchRecoveryConfig(fileConfig)
	if err != nil {
		t.Fatal("synthetic frozen identity refused")
	}
	defer clear(frozen.KeyData)
	for _, name := range []string{"ca", "cert", "key"} {
		replacement := filepath.Join(dir, name+"-replacement")
		if os.WriteFile(replacement, []byte("synthetic replacement must never be read"), 0600) != nil || os.Rename(replacement, filepath.Join(dir, name)) != nil {
			t.Fatal("synthetic replacement fixture failed")
		}
	}
	if frozen.CAFile != "" || frozen.CertFile != "" || frozen.KeyFile != "" || !bytes.Equal(frozen.CAData, base.CAData) || !bytes.Equal(frozen.CertData, base.CertData) || !bytes.Equal(frozen.KeyData, base.KeyData) {
		t.Fatal("API credential retained a live file or changed literal identity")
	}
	if _, err = kubernetes.NewForConfig(frozen); err != nil {
		t.Fatal("API client reloaded replaced credential paths")
	}
	raw, err := watchRecoveryKubeconfig(frozen)
	if err != nil {
		t.Fatal("same-identity Helm config refused")
	}
	defer clear(raw)
	k, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatal("frozen synthetic config invalid")
	}
	a, c := k.AuthInfos["watch-recovery"], k.Clusters["watch-recovery"]
	if a == nil || c == nil || a.Impersonate != "" || a.ImpersonateUID != "" || len(a.ImpersonateGroups) != 0 || len(a.ImpersonateUserExtra) != 0 || a.ClientKey != "" || a.ClientCertificate != "" || c.CertificateAuthority != "" || !bytes.Equal(a.ClientKeyData, frozen.KeyData) || !bytes.Equal(a.ClientCertificateData, frozen.CertData) || !bytes.Equal(c.CertificateAuthorityData, frozen.CAData) || c.Server != frozen.Host || c.TLSServerName != frozen.ServerName {
		t.Fatal("API and Helm did not retain the same frozen principal and trust")
	}
	t.Setenv("HTTPS_PROXY", "http://synthetic-untrusted.invalid")
	if selected, err := frozen.Proxy(&http.Request{}); err != nil || selected != nil {
		t.Fatal("frozen API retained ambient proxy input")
	}
}

func TestWatchRecoveryObservedOrderAndCompleteMetadata(t *testing.T) {
	cfg, deps, _, state, _ := recoveryUnitFixture(t)
	for i := range cfg.watchRecovery.Objects {
		if cfg.watchRecovery.Objects[i].Kind == "Deployment" {
			entries := cfg.watchRecovery.Objects[i].ManagedFields
			entries[0], entries[1] = entries[1], entries[0]
		}
	}
	if rows, err := applyWatchRecovery(context.Background(), cfg, deps, false, func(watchRecoveryPlan) error { return nil }); err != nil || len(rows) != 1 || state.commits != 1 {
		t.Fatal("validated semantically identical plan order selected the wrong observed owner")
	}
	cfg, deps, _, _, _ = recoveryUnitFixture(t)
	o := cfg.watchRecovery.Objects[3]
	before, err := getWatchObject(context.Background(), deps, "oberth", watchAdoptionObject{Kind: o.Kind, Name: o.Name})
	if err != nil {
		t.Fatal(err)
	}
	_, next, err := watchArgsHandoff(before.meta.GetManagedFields())
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(metav1.Object){func(m metav1.Object) { m.SetFinalizers([]string{"foreign-added"}) }, func(m metav1.Object) {
		m.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign-owner"}})
	}, func(m metav1.Object) { m.SetGenerateName("foreign-") }, func(m metav1.Object) { m.SetCreationTimestamp(metav1.Now()) }, func(m metav1.Object) {
		m.GetAnnotations()["deployment.kubernetes.io/revision"] = "3"
	}, func(m metav1.Object) {
		delete(m.GetAnnotations(), "oberth.ci/source")
	}} {
		obj := before.meta.(runtime.Object).DeepCopyObject()
		v, _ := watchSpec(obj)
		v.meta.SetResourceVersion("new-rv")
		v.meta.SetManagedFields(next)
		change(v.meta)
		if validHandoffReply(obj, before, o, "oberth", next, true) {
			t.Fatal("handoff reply accepted unrelated complete metadata change")
		}
	}
	bad := cfg.watchRecovery.Objects[3]
	obj, err := recoveryTarget(bad, true, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := watchSpec(obj)
	v.meta.SetFinalizers([]string{"foreign"})
	bad.DefaultedTarget, _ = json.Marshal(obj)
	if _, err = recoveryTarget(bad, true, "oberth"); err == nil {
		t.Fatal("approved defaulted target admitted unrelated finalizer authority")
	}
}

func TestWatchRecoveryPublicAnnotationDriftRefusesBeforeEffects(t *testing.T) {
	for _, key := range []string{"deployment.kubernetes.io/revision", "oberth.ci/source"} {
		t.Run(key, func(t *testing.T) {
			cfg, deps, _, state, _ := recoveryUnitFixture(t)
			for i := range cfg.watchRecovery.Objects {
				o := &cfg.watchRecovery.Objects[i]
				if o.Kind != "Deployment" {
					continue
				}
				// The fixture API keeps its original annotations. Change only the
				// approved plan, using another valid revision or an omitted key.
				annotations := make(map[string]string, len(o.Metadata.Annotations))
				for k, v := range o.Metadata.Annotations {
					annotations[k] = v
				}
				if key == "deployment.kubernetes.io/revision" {
					annotations[key] = "3"
				} else {
					delete(annotations, key)
				}
				o.Metadata.Annotations = annotations
			}
			rows, err := applyWatchRecovery(context.Background(), cfg, deps, false, func(watchRecoveryPlan) error { return nil })
			if err == nil || err.Error() != "watch recovery state differs for Deployment/cloudflared-watch-oberth-v2" || len(rows) != 0 || state.commits != 0 || state.dryCAS != 0 || state.applies != 0 {
				t.Fatal("changed approved public metadata reached an effect boundary")
			}
		})
	}
}

func TestWatchExecutableDescriptorRemainsBoundAcrossPathReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public-executable")
	original, replacement := []byte("original running inode"), []byte("replacement installation inode")
	if os.WriteFile(path, original, 0600) != nil {
		t.Fatal("public descriptor fixture failed")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	other := path + "-replacement"
	if os.WriteFile(other, replacement, 0600) != nil || os.Rename(other, path) != nil {
		t.Fatal("public replacement fixture failed")
	}
	digest, err := watchDescriptorDigest(f)
	hash := sha256.Sum256(original)
	if err != nil || digest != hex.EncodeToString(hash[:]) {
		t.Fatal("descriptor followed replacement installation path")
	}
	named, err := watchBinaryDigest(path)
	if err != nil || named == digest {
		t.Fatal("replacement control did not select different bytes")
	}
}
