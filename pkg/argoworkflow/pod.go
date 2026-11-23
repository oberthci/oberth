package argoworkflow

import (
	"encoding/json"
	"fmt"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"reflect"
	"strings"
)

func (g *graph) podSecurity(s *corev1.PodSecurityContext, where string) {
	if s == nil {
		return
	}
	if w := s.WindowsOptions; w != nil && w.HostProcess != nil && *w.HostProcess {
		g.refuse(where+".windowsOptions.hostProcess", "host")
	}
}
func (g *graph) synchronization(s *wfv1.Synchronization, where string) {
	if s == nil {
		return
	}
	for i, mutex := range s.Mutexes {
		g.mutex(mutex, fmt.Sprintf("%s.mutexes[%d]", where, i))
	}
	for i, sem := range s.Semaphores {
		if sem == nil {
			continue
		}
		if sem.Database != nil || sem.Namespace != "" || sem.ConfigMapKeyRef == nil || sem.ConfigMapKeyRef.Name != "oberth-sync-limits" || !literal(sem.ConfigMapKeyRef.Key) {
			g.refuse(fmt.Sprintf("%s.semaphores[%d]", where, i), "read")
		}
	}
}
func (g *graph) mutex(mutex *wfv1.Mutex, where string) {
	if mutex == nil {
		g.refuse(where, "mutex_scope")
		return
	}
	// The slash terminates the exact catalogue repository, not a name prefix.
	name, scoped := strings.CutPrefix(mutex.Name, g.request.Repository+"/")
	if g.request.Repository == "" || !scoped || !literal(name) {
		g.refuse(where+".name", "mutex_scope")
	}
	if mutex.Namespace != "" && mutex.Namespace != g.request.IdentityRequest.Namespace {
		g.refuse(where+".namespace", "mutex_namespace")
	}
}
func (g *graph) claims(claims []corev1.PersistentVolumeClaim, where string) {
	for _, c := range claims {
		at := where + ".volumeClaimTemplates[" + c.Name + "]"
		if reservedName(c.Name) {
			g.refuse(at+".name", "reserved")
		}
		if c.Spec.VolumeName != "" || c.Spec.DataSource != nil || c.Spec.DataSourceRef != nil || c.Spec.Selector != nil {
			g.refuse(at+".spec", "read")
		}
		if len(c.OwnerReferences) > 0 {
			g.refuse(at+".metadata", "reserved")
		}
		if len(c.Finalizers) > 0 {
			g.refuse(at+".metadata.finalizers", "reserved")
		}
		g.metadataKeys(at+".metadata.labels", c.Labels, false)
		g.metadataKeys(at+".metadata.annotations", c.Annotations, false)
	}
}
func (g *graph) volumes(volumes []corev1.Volume, n *node, where string, shared bool) {
	for _, v := range volumes {
		at := where + "[" + v.Name + "]"
		if reservedName(v.Name) {
			g.refuse(at+".name", "reserved")
		}
		raw, _ := json.Marshal(v.VolumeSource)
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || len(fields) != 1 {
			g.refuse(at, "document")
			continue
		}
		if n == nil {
			n = &node{template: &wfv1.Template{Name: "*"}}
			if shared {
				n.scope = "shared"
			}
		}
		g.volumeSource(v, n, at, shared)

	}
}

func (g *graph) volumeSource(v corev1.Volume, n *node, at string, shared bool) {
	switch {
	case v.EmptyDir != nil, v.DownwardAPI != nil:
	case v.Secret != nil:
		g.clusterReads(v.Secret, n, at+".secret")
	case v.ConfigMap != nil:
		g.clusterReads(v.ConfigMap, n, at+".configMap")
	case v.Projected != nil:
		g.projected(v, n, at, shared)
	case v.PersistentVolumeClaim != nil:
		name := v.PersistentVolumeClaim.ClaimName
		if shared || !publicClaim(name) {
			g.refuse(at, "pvc")
			return
		}
		if !g.policy.PVCClaims[name] {
			g.missingGrant(n, "pvc", name, at)
		}
	default:
		raw, _ := json.Marshal(v.VolumeSource)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		for _, kind := range sortedKeys(fields) {
			g.refuse(at+"."+kind, "volume")
		}
	}
}
func (g *graph) projected(v corev1.Volume, n *node, at string, shared bool) {
	for i, s := range v.Projected.Sources {
		where := fmt.Sprintf("%s.projected.sources[%d]", at, i)
		g.projectedSource(s, n, where)
	}
	if tokenProjection(v) {
		g.token(v, n, at, shared)
	}
}

func (g *graph) projectedSource(s corev1.VolumeProjection, n *node, where string) {
	raw, _ := json.Marshal(s)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if len(fields) != 1 {
		g.refuse(where, "document")
		return
	}
	for kind := range fields {
		switch kind {
		case "downwardAPI", "serviceAccountToken":
		case "secret", "configMap":
			g.clusterReads(s, n, where)
		default:
			g.refuse(where+"."+kind, "projection")
		}
	}
}

func publicClaim(name string) bool {
	return name != "oberth" && !strings.HasPrefix(name, "oberth-") && len(validation.IsDNS1123Subdomain(name)) == 0
}

func (g *graph) clusterReads(value any, n *node, where string) {
	g.clusterValue(reflect.ValueOf(value), n, where)
}
func (g *graph) clusterValue(v reflect.Value, n *node, where string) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			g.clusterValue(v.Elem(), n, where)
		}
	case reflect.Struct:
		g.clusterStruct(v, n, where)
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			g.clusterValue(v.Index(i), n, fmt.Sprintf("%s[%d]", where, i))
		}
	}
}
func (g *graph) clusterStruct(v reflect.Value, n *node, where string) {
	if kind := clusterReferenceKind(v.Type()); kind != "" {
		name := v.FieldByName("Name")
		if !name.IsValid() {
			name = v.FieldByName("SecretName")
		}
		if name.IsValid() {
			g.clusterGrant(n, kind, name.String(), where)
		}
		return
	}
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		key := strings.Split(field.Tag.Get("json"), ",")[0]
		at := where
		if key != "" {
			at = fieldPath(where, key)
		}
		g.clusterValue(v.Field(i), n, at)
	}
}
func clusterReferenceKind(typ reflect.Type) string {
	switch typ {
	case reflect.TypeFor[corev1.SecretKeySelector](), reflect.TypeFor[corev1.SecretEnvSource](), reflect.TypeFor[corev1.SecretProjection](), reflect.TypeFor[corev1.SecretVolumeSource]():
		return "cluster-secret"
	case reflect.TypeFor[corev1.ConfigMapKeySelector](), reflect.TypeFor[corev1.ConfigMapEnvSource](), reflect.TypeFor[corev1.ConfigMapProjection](), reflect.TypeFor[corev1.ConfigMapVolumeSource]():
		return "configmap"
	}
	return ""
}

func (g *graph) clusterGrant(n *node, kind, name, where string) {
	if n.scope != "" {
		g.refuse(where, "shared_grant")
		return
	}
	if name == "oberth" || strings.HasPrefix(name, "oberth-") {
		g.refuse(where, "reserved")
		return
	}
	if !publicClaim(name) || !g.granted(n, kind, name, "") {
		g.missingGrant(n, kind, name, where)
	}
}
