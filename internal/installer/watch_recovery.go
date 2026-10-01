package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

const watchRecoverySchema = "oberth.watch-adoption/v2"
const watchLegacyManager = "kubectl-client-side-apply"
const watchArgsField = `.spec.template.spec.initContainers[name="fetch-token"].args`

var watchArgsTriePath = []string{"f:spec", "f:template", "f:spec", "f:initContainers", `k:{"name":"fetch-token"}`, "f:args"}

type watchPublicMetadata struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}
type watchRecoveryObject struct {
	Kind            string                      `json:"kind"`
	Name            string                      `json:"name"`
	UID             string                      `json:"uid"`
	ResourceVersion string                      `json:"resource_version"`
	Generation      int64                       `json:"generation"`
	Metadata        watchPublicMetadata         `json:"metadata"`
	ManagedFields   []metav1.ManagedFieldsEntry `json:"managed_fields"`
	Spec            json.RawMessage             `json:"spec"`
	Target          json.RawMessage             `json:"target"`
	DefaultedTarget json.RawMessage             `json:"defaulted_target"`
}
type watchPublicFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type watchReleaseGuard struct {
	Revision         int64  `json:"revision"`
	Status           string `json:"status"`
	Chart            string `json:"chart"`
	RecordUID        string `json:"record_uid"`
	RecordRV         string `json:"record_resource_version"`
	PreviousRevision int64  `json:"previous_revision"`
}
type watchRecoveryOrigin struct {
	Plan      watchPublicFile `json:"plan"`
	Receipt   watchPublicFile `json:"receipt"`
	Confirmed []watchAdopted  `json:"confirmed"`
}
type watchRecoveryArtifacts struct {
	Record          watchPublicFile `json:"record"`
	RecordBundle    watchPublicFile `json:"record_bundle"`
	Chart           watchPublicFile `json:"chart"`
	ChartBundle     watchPublicFile `json:"chart_bundle"`
	Checksums       watchPublicFile `json:"checksums"`
	ChecksumsBundle watchPublicFile `json:"checksums_bundle"`
	SourceSHA       string          `json:"source_sha"`
	InstallerSHA256 string          `json:"installer_sha256"`
	ServerImage     string          `json:"server_image"`
	HelmSHA256      string          `json:"helm_sha256"`
}
type watchRecoveryPlan struct {
	ready         bool
	ValuesSHA256  string                 `json:"values_sha256"`
	Schema        string                 `json:"schema"`
	Mode          string                 `json:"mode"`
	Namespace     string                 `json:"namespace"`
	Context       string                 `json:"context"`
	ClusterUID    string                 `json:"cluster_uid"`
	ServerVersion string                 `json:"server_version"`
	ChartVersion  string                 `json:"chart_version"`
	CreatedAt     time.Time              `json:"created_at"`
	ExpiresAt     time.Time              `json:"expires_at"`
	Release       watchReleaseGuard      `json:"release"`
	Origin        *watchRecoveryOrigin   `json:"origin"`
	Artifacts     watchRecoveryArtifacts `json:"artifacts"`
	Objects       []watchRecoveryObject  `json:"objects"`
}

func strictWatchDecode(raw []byte, target any) error {
	if !uniqueWatchJSON(raw) {
		return errors.New("invalid or duplicate public JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid public JSON shape")
	}
	return nil
}

// Exact spelling and required presence precede encoding/json's alias matching.
func exactRecoveryFields(raw []byte) bool {
	check := func(raw []byte, nullable string, names ...string) (map[string]json.RawMessage, bool) {
		var row map[string]json.RawMessage
		if json.Unmarshal(raw, &row) != nil || len(row) != len(names) {
			return nil, false
		}
		for _, name := range names {
			v, ok := row[name]
			if !ok || name != nullable && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return nil, false
			}
		}
		return row, true
	}
	p, ok := check(raw, "origin", "values_sha256", "schema", "mode", "namespace", "context", "cluster_uid", "server_version", "chart_version", "created_at", "expires_at", "release", "origin", "artifacts", "objects")
	if !ok {
		return false
	}
	if _, ok = check(p["release"], "", "revision", "status", "chart", "record_uid", "record_resource_version", "previous_revision"); !ok {
		return false
	}
	a, ok := check(p["artifacts"], "", "record", "record_bundle", "chart", "chart_bundle", "checksums", "checksums_bundle", "source_sha", "installer_sha256", "server_image", "helm_sha256")
	if !ok {
		return false
	}
	for _, key := range []string{"record", "record_bundle", "chart", "chart_bundle", "checksums", "checksums_bundle"} {
		if _, ok = check(a[key], "", "path", "sha256"); !ok {
			return false
		}
	}
	if !bytes.Equal(bytes.TrimSpace(p["origin"]), []byte("null")) {
		o, good := check(p["origin"], "", "plan", "receipt", "confirmed")
		if !good {
			return false
		}
		for _, key := range []string{"plan", "receipt"} {
			if _, ok = check(o[key], "", "path", "sha256"); !ok {
				return false
			}
		}
		var rows []json.RawMessage
		if json.Unmarshal(o["confirmed"], &rows) != nil {
			return false
		}
		for _, row := range rows {
			if _, ok = check(row, "", "kind", "name", "uid", "resource_version"); !ok {
				return false
			}
		}
	}
	var rows []json.RawMessage
	if json.Unmarshal(p["objects"], &rows) != nil {
		return false
	}
	for _, row := range rows {
		o, good := check(row, "", "kind", "name", "uid", "resource_version", "generation", "metadata", "managed_fields", "spec", "target", "defaulted_target")
		if !good {
			return false
		}
		if _, ok = check(o["metadata"], "", "labels", "annotations"); !ok {
			return false
		}
		var entries []map[string]json.RawMessage
		if json.Unmarshal(o["managed_fields"], &entries) != nil {
			return false
		}
		for _, entry := range entries {
			for _, key := range []string{"manager", "operation", "apiVersion", "fieldsType", "fieldsV1"} {
				v, exists := entry[key]
				if !exists || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
					return false
				}
			}
			for key, value := range entry {
				if key != "manager" && key != "operation" && key != "apiVersion" && key != "fieldsType" && key != "fieldsV1" && key != "time" && key != "subresource" || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return false
				}
			}
		}
	}
	return true
}

func readWatchRecoveryPlan(cfg Config) (*watchRecoveryPlan, error) {
	if cfg.WatchAdoptionPlan == "" {
		return nil, nil
	}
	name, err := filepath.Abs(cfg.WatchAdoptionPlan)
	if err != nil {
		return nil, errors.New("invalid public watch plan path")
	}
	raw, err := boundedWatchFile(name, 2<<20)
	if err != nil {
		return nil, err
	}
	var schema struct {
		Schema string `json:"schema"`
	}
	if !uniqueWatchJSON(raw) || json.Unmarshal(raw, &schema) != nil {
		return nil, errors.New("invalid public watch plan")
	}
	if schema.Schema != watchRecoverySchema {
		return nil, nil
	}
	var p watchRecoveryPlan
	if !exactRecoveryFields(raw) || strictWatchDecode(raw, &p) != nil {
		return nil, errors.New("invalid v2 public watch plan")
	}
	if p.Namespace != cfg.Namespace || p.ChartVersion != cfg.ChartVersion || p.Context == "" || p.ClusterUID == "" || !supportedWatchRecoveryAPI(p.ServerVersion) || len(p.Objects) != 4 || p.Release.Revision < 1 || p.Release.RecordUID == "" || p.Release.RecordRV == "" || !watchRecoveryDeadline(p) {
		return nil, errors.New("v2 watch scope, API version or deadline differs")
	}
	if !isSHA256Digest("sha256:" + p.ValuesSHA256) {
		return nil, errors.New("v2 watch public values digest is required")
	}
	if p.Mode == "failed-adoption-recovery" {
		if p.Origin == nil || p.Release.Revision != 72 || p.Release.Status != "failed" || p.Release.Chart != "oberth-0.16.25" || p.Release.PreviousRevision != 71 {
			return nil, errors.New("watch recovery is limited to the reviewed failed adoption revision")
		}
	} else {
		return nil, errors.New("unsupported watch recovery mode")
	}
	seen := map[string]bool{}
	rv := regexp.MustCompile(`^[1-9][0-9]*$`)
	for _, o := range p.Objects {
		key := o.Kind + "/" + o.Name
		if !watchObjectAllowed(o.Kind, o.Name) || seen[key] || o.UID == "" || !rv.MatchString(o.ResourceVersion) || o.Generation < 0 || o.Metadata.Labels == nil || o.Metadata.Annotations == nil || o.ManagedFields == nil || !uniqueWatchJSON(o.Spec) || !uniqueWatchJSON(o.Target) || !uniqueWatchJSON(o.DefaultedTarget) {
			return nil, errors.New("invalid v2 watch object")
		}
		seen[key] = true
		if err := validateRecoveryMetadata(o.Kind, o.Metadata); err != nil {
			return nil, err
		}
		if _, err := recoveryTarget(o, false, p.Namespace); err != nil {
			return nil, err
		}
		if _, err := recoveryTarget(o, true, p.Namespace); err != nil {
			return nil, err
		}
	}
	for _, o := range p.Objects {
		if o.Kind == "Deployment" {
			if _, _, err := watchArgsHandoff(o.ManagedFields); err != nil {
				return nil, err
			}
		}
	}
	return &p, nil
}

func watchRecoveryDeadline(p watchRecoveryPlan) bool {
	now := time.Now()
	return !p.CreatedAt.After(now) && now.Before(p.ExpiresAt) && p.ExpiresAt.After(p.CreatedAt) && p.ExpiresAt.Sub(p.CreatedAt) <= 30*time.Minute
}

func validateRecoveryMetadata(kind string, m watchPublicMetadata) error {
	for key := range m.Labels {
		if key != "app.kubernetes.io/name" && key != "app.kubernetes.io/instance" && key != "app.kubernetes.io/component" && key != "app.kubernetes.io/managed-by" && key != "helm.sh/chart" {
			return errors.New("unapproved watch metadata label")
		}
	}
	for key, value := range m.Annotations {
		if key == "meta.helm.sh/release-name" || key == "meta.helm.sh/release-namespace" {
			continue
		}
		if kind == "Deployment" && key == "deployment.kubernetes.io/revision" {
			revision, err := strconv.ParseInt(value, 10, 64)
			if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != value {
				return errors.New("unapproved watch annotation")
			}
			continue
		}
		if kind == "Deployment" && key == "oberth.ci/source" && value == "github.com/oberthci/terraform//k8s/cloudflared-watch" {
			continue
		}
		if key != "kubectl.kubernetes.io/last-applied-configuration" || len(value) > 262144 || !uniqueWatchJSON([]byte(value)) {
			return errors.New("unapproved watch annotation")
		}
		var id struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		if json.Unmarshal([]byte(value), &id) != nil || !watchObjectAllowed(id.Kind, id.Metadata.Name) {
			return errors.New("unapproved last-applied watch annotation")
		}
		var obj runtime.Object
		switch id.Kind {
		case "Deployment":
			obj = &appsv1.Deployment{}
		case "ServiceAccount":
			obj = &corev1.ServiceAccount{}
		case "ConfigMap":
			obj = &corev1.ConfigMap{}
		}
		if strictWatchDecode([]byte(value), obj) != nil {
			return errors.New("invalid public last-applied watch object")
		}
		if _, err := watchSpec(obj); err != nil {
			return errors.New("unapproved last-applied watch spec")
		}
	}
	return nil
}

func recoveryTarget(o watchRecoveryObject, defaulted bool, ns string) (runtime.Object, error) {
	raw := o.Target
	if defaulted {
		raw = o.DefaultedTarget
	}
	var obj runtime.Object
	expectedVersion := "v1"
	switch o.Kind {
	case "Deployment":
		obj = &appsv1.Deployment{}
		expectedVersion = "apps/v1"
	case "ServiceAccount":
		obj = &corev1.ServiceAccount{}
	case "ConfigMap":
		obj = &corev1.ConfigMap{}
	default:
		return nil, errors.New("unsupported watch target")
	}
	if strictWatchDecode(raw, obj) != nil {
		return nil, errors.New("invalid public watch target")
	}
	v, err := watchSpec(obj)
	if err != nil {
		return nil, err
	}
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind != o.Kind || gvk.GroupVersion().String() != expectedVersion || v.meta.GetName() != o.Name || v.meta.GetNamespace() != ns || len(v.meta.GetManagedFields()) != 0 || v.meta.GetDeletionTimestamp() != nil || watchOwner(v.meta) != (watchOwnership{"Helm", "oberth", ns}) {
		return nil, errors.New("watch target identity or ownership differs")
	}
	full, err := watchCompleteMetadata(obj)
	if err != nil || full.GenerateName != "" || full.GetSelfLink() != "" || len(full.Finalizers) != 0 || len(full.OwnerReferences) != 0 || full.DeletionGracePeriodSeconds != nil {
		return nil, errors.New("watch target contains unsupported non-label metadata")
	}
	if !defaulted && (!full.CreationTimestamp.IsZero() || full.Generation != 0) {
		return nil, errors.New("rendered target contains unrelated API metadata")
	}
	if !defaulted && (v.meta.GetUID() != "" || v.meta.GetResourceVersion() != "") {
		return nil, errors.New("rendered target contains API identity")
	}
	if defaulted && (string(v.meta.GetUID()) != o.UID || v.meta.GetResourceVersion() != "") {
		return nil, errors.New("defaulted target must bind UID and omit variable RV")
	}
	if validateRecoveryMetadata(o.Kind, watchPublicMetadata{v.meta.GetLabels(), v.meta.GetAnnotations()}) != nil {
		return nil, errors.New("unapproved target metadata")
	}
	return obj, nil
}

func emptyRecoveryObject(kind string) runtime.Object {
	switch kind {
	case "Deployment":
		return &appsv1.Deployment{}
	case "ServiceAccount":
		return &corev1.ServiceAccount{}
	case "ConfigMap":
		return &corev1.ConfigMap{}
	}
	return nil
}

// These are direct API apply requests, never Helm's render-only dry-run.
func patchRecoveryObject(ctx context.Context, deps Deps, ns, kind, name string, patchType types.PatchType, body []byte, opts metav1.PatchOptions) (runtime.Object, error) {
	switch kind {
	case "Deployment":
		return deps.KubeClient.AppsV1().Deployments(ns).Patch(ctx, name, patchType, body, opts)
	case "ServiceAccount":
		return deps.KubeClient.CoreV1().ServiceAccounts(ns).Patch(ctx, name, patchType, body, opts)
	case "ConfigMap":
		return deps.KubeClient.CoreV1().ConfigMaps(ns).Patch(ctx, name, patchType, body, opts)
	}
	return nil, errors.New("unsupported watch patch")
}

func recoverySSAWithGuard(ctx context.Context, deps Deps, p watchRecoveryPlan, expectedConflict bool, guard func() error) error {
	for _, o := range p.Objects {
		if err := guard(); err != nil {
			return err
		}
		obj, err := recoveryTarget(o, false, p.Namespace)
		if err != nil {
			return err
		}
		v, _ := watchSpec(obj)
		v.meta.SetUID(types.UID(o.UID))
		v.meta.SetResourceVersion(o.ResourceVersion)
		body, err := json.Marshal(obj)
		if err != nil {
			return errors.New("cannot encode watch apply")
		}
		force := false
		x, e := patchRecoveryObject(ctx, deps, p.Namespace, o.Kind, o.Name, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: "helm", Force: &force, DryRun: []string{metav1.DryRunAll}, FieldValidation: "Strict"})
		if expectedConflict && o.Kind == "Deployment" {
			if !plannedWatchConflict(e, o) {
				return errors.New("watch apply did not report only the reviewed legacy args conflict")
			}
			continue
		}
		if e != nil {
			return errors.New("watch force=false apply preflight failed")
		}
		got, e := watchSpec(x)
		if e != nil {
			return errors.New("invalid watch SSA response")
		}
		wantObj, _ := recoveryTarget(o, true, p.Namespace)
		want, _ := watchSpec(wantObj)
		gotMetadata, e := watchCompleteMetadata(x)
		wantMetadata, f := watchCompleteMetadata(wantObj)
		wantMetadata.ResourceVersion = o.ResourceVersion
		// Dry-run apply computes a prospective fieldset; it never persists it.
		// Complete current fieldsets are separately reobserved after all four.
		wantMetadata.ManagedFields = gotMetadata.ManagedFields
		if e != nil || f != nil || !sameWatchCompleteMetadata(gotMetadata, wantMetadata) || !sameWatchJSON(got.spec, want.spec) {
			return errors.New("watch SSA defaulted public target differs")
		}
	}
	return nil
}

func plannedWatchConflict(err error, o watchRecoveryObject) bool {
	if !apierrors.IsConflict(err) {
		return false
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	s := status.Status()
	if s.Code != 409 || s.Details == nil || s.Details.Name != "" && s.Details.Name != o.Name || s.Details.Kind != "" && s.Details.Kind != "deployments" || s.Details.Group != "" && s.Details.Group != "apps" || len(s.Details.Causes) != 1 {
		return false
	}
	c := s.Details.Causes[0]
	return o.Kind == "Deployment" && o.Name == "cloudflared-watch-oberth-v2" && c.Type == metav1.CauseTypeFieldManagerConflict && c.Field == watchArgsField && c.Message == watchLegacyConflictMessage(o.ManagedFields)
}

func sameRecoveryMetadata(m metav1.Object, want watchPublicMetadata) bool {
	b, _ := json.Marshal(watchPublicMetadata{m.GetLabels(), m.GetAnnotations()})
	w, _ := json.Marshal(want)
	return sameWatchJSON(b, w)
}

func recoveryObserved(ctx context.Context, deps Deps, p watchRecoveryPlan) ([]watchObserved, error) {
	out := make([]watchObserved, 0, 4)
	for _, o := range p.Objects {
		v, e := getWatchObject(ctx, deps, p.Namespace, watchAdoptionObject{Kind: o.Kind, Name: o.Name})
		if e != nil {
			return nil, errors.New("cannot observe watch object")
		}
		if v.meta.GetDeletionTimestamp() != nil || v.meta.GetNamespace() != p.Namespace || string(v.meta.GetUID()) != o.UID || v.meta.GetResourceVersion() != o.ResourceVersion || v.meta.GetGeneration() != o.Generation || !sameRecoveryMetadata(v.meta, o.Metadata) || !sameWatchManaged(v.meta.GetManagedFields(), o.ManagedFields) || !sameWatchJSON(v.spec, o.Spec) {
			return nil, fmt.Errorf("watch recovery state differs for %s/%s", o.Kind, o.Name)
		}
		defaulted, e := recoveryTarget(o, true, p.Namespace)
		if e != nil {
			return nil, e
		}
		approved, e := watchCompleteMetadata(defaulted)
		current, f := watchCompleteMetadata(v.meta)
		approved.Labels, approved.Annotations = o.Metadata.Labels, o.Metadata.Annotations
		approved.UID, approved.ResourceVersion, approved.Generation = types.UID(o.UID), o.ResourceVersion, o.Generation
		approved.ManagedFields = o.ManagedFields
		if e != nil || f != nil || !sameWatchCompleteMetadata(current, approved) {
			return nil, errors.New("watch current complete metadata differs from approved public target identity")
		}
		owner := watchOwner(v.meta)
		if owner != (watchOwnership{}) && owner != (watchOwnership{"Helm", "oberth", p.Namespace}) {
			return nil, errors.New("foreign watch ownership")
		}
		if p.Mode == "failed-adoption-recovery" && owner != (watchOwnership{"Helm", "oberth", p.Namespace}) {
			return nil, errors.New("failed recovery requires confirmed Helm ownership")
		}
		out = append(out, v)
	}
	return out, nil
}

func supportedWatchRecoveryAPI(version string) bool {
	version = strings.TrimPrefix(strings.Split(version, "+")[0], "v")
	return version == "1.36.2" || version == "1.36.3"
}
