package argoworkflow

import (
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const refusalImage = "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func refusalLeaf() wfv1.Template {
	return wfv1.Template{Container: &corev1.Container{Image: refusalImage, Command: []string{"/bin/true"}}}
}

// template carriers: returns workflow where *tmpl is placed at carrier position
func refusalCarry(carrier string, tmpl wfv1.Template) *wfv1.Workflow {
	w := &wfv1.Workflow{}
	w.Spec.Entrypoint = "main"
	switch carrier {
	case "top":
		tmpl.Name = "main"
		w.Spec.Templates = []wfv1.Template{tmpl}
	case "defaults":
		leaf := refusalLeaf()
		leaf.Name = "main"
		w.Spec.Templates = []wfv1.Template{leaf}
		w.Spec.TemplateDefaults = &tmpl
	case "inline-dag":
		w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Inline: &tmpl}}}}}
	case "inline-steps":
		w.Spec.Templates = []wfv1.Template{{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "a", Inline: &tmpl}}}}}}
	case "inline-deep":
		mid := wfv1.Template{Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "b", Inline: &tmpl}}}}}
		w.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Inline: &mid}}}}}
	case "second":
		leaf := refusalLeaf()
		leaf.Name = "main"
		tmpl.Name = "other"
		w.Spec.Templates = []wfv1.Template{leaf, tmpl}
	}
	return w
}

var refusalCarriers = []string{"top", "defaults", "inline-dag", "inline-steps", "inline-deep", "second"}

func TestRefusalPositionTemplateKeepRowsEveryCarrier(t *testing.T) {
	muts := map[string]func(*wfv1.Template){
		"18-podSpecPatch": func(x *wfv1.Template) { x.PodSpecPatch = `{"serviceAccountName":"x"}` },
		"24-hostAliases": func(x *wfv1.Template) {
			x.HostAliases = []corev1.HostAlias{{IP: "1.2.3.4", Hostnames: []string{"openbao"}}}
		},
		"26-scheduler":       func(x *wfv1.Template) { x.SchedulerName = "x" },
		"26-priority":        func(x *wfv1.Template) { x.PriorityClassName = "system-node-critical" },
		"27-securityContext": func(x *wfv1.Template) { x.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))} },
		"32-semaphore": func(x *wfv1.Template) {
			x.Synchronization = &wfv1.Synchronization{Semaphores: []*wfv1.SemaphoreRef{{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "other"}, Key: "k"}}}}
		},
		"32-semaphore-ns": func(x *wfv1.Template) {
			x.Synchronization = &wfv1.Synchronization{Semaphores: []*wfv1.SemaphoreRef{{Namespace: "kube-system", ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: SyncLimitsConfigMap}, Key: "k"}}}}
		},
		"39-in-cm": func(x *wfv1.Template) {
			x.Inputs.Parameters = []wfv1.Parameter{{Name: "p", ValueFrom: &wfv1.ValueFrom{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{Key: "k"}}}}
		},
		"39-out-cm": func(x *wfv1.Template) {
			x.Outputs.Parameters = []wfv1.Parameter{{Name: "p", ValueFrom: &wfv1.ValueFrom{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{Key: "k"}}}}
		},
		"45-pvc": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "oberth-data"}}}}
		},
		"46-secret": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}}}
		},
		"46-configmap": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}}}
		},
		"46-projected": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}}}
		},
		"46-csi": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "d"}}}}
		},
		"47-hostPath": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		},
		"48-nfs": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "s", Path: "/"}}}}
		},
		"48-ephemeral": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}}}
		},
		"48-image": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Image: &corev1.ImageVolumeSource{Reference: "x"}}}}
		},
		"48-downward": func(x *wfv1.Template) {
			x.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{}}}}
		},
		"48-empty": func(x *wfv1.Template) { x.Volumes = []corev1.Volume{{Name: "v"}} },
		"57-resource": func(x *wfv1.Template) {
			x.Container = nil
			x.Resource = &wfv1.ResourceTemplate{Action: "create", Manifest: "kind: Pod"}
		},
		"58-plugin":          func(x *wfv1.Template) { x.Container = nil; x.Plugin = &wfv1.Plugin{} },
		"58-http":            func(x *wfv1.Template) { x.Container = nil; x.HTTP = &wfv1.HTTP{URL: "http://x"} },
		"60-data":            func(x *wfv1.Template) { x.Container = nil; x.Data = &wfv1.Data{} },
		"59-in-artifact":     func(x *wfv1.Template) { x.Inputs.Artifacts = wfv1.Artifacts{{Name: "a", Path: "/a"}} },
		"59-out-artifact":    func(x *wfv1.Template) { x.Outputs.Artifacts = wfv1.Artifacts{{Name: "a", Path: "/a"}} },
		"61-archiveLocation": func(x *wfv1.Template) { x.ArchiveLocation = &wfv1.ArtifactLocation{} },
		"62-memoize": func(x *wfv1.Template) {
			x.Memoize = &wfv1.Memoize{Key: "k", Cache: &wfv1.Cache{ConfigMap: &corev1.LocalObjectReference{Name: "c"}}}
		},
		"63-annotation": func(x *wfv1.Template) {
			x.Metadata.Annotations = map[string]string{"container.apparmor.security.beta.kubernetes.io/main": "unconfined"}
		},
		"63-oberth-exclusive": func(x *wfv1.Template) { x.Metadata.Annotations = map[string]string{"oberth.ci/exclusive": "true"} },
		"31-label-oberth":     func(x *wfv1.Template) { x.Metadata.Labels = map[string]string{"oberth.ci/run": "x"} },
		"31-label-argo":       func(x *wfv1.Template) { x.Metadata.Labels = map[string]string{"workflows.argoproj.io/workflow": "x"} },
		"31-label-app":        func(x *wfv1.Template) { x.Metadata.Labels = map[string]string{"app.kubernetes.io/name": "oberth"} },
		"31-label-templated":  func(x *wfv1.Template) { x.Metadata.Labels = map[string]string{"{{workflow.parameters.k}}": "x"} },
		"31-label-openbao":    func(x *wfv1.Template) { x.Metadata.Labels = map[string]string{"openbao-active": "true"} },
	}
	for name, mut := range muts {
		for _, carrier := range refusalCarriers {
			t.Run(name+"/"+carrier, func(t *testing.T) {
				tmpl := refusalLeaf()
				mut(&tmpl)
				if err := Admit(refusalCarry(carrier, tmpl), Policy{}); err == nil {
					t.Errorf("ADMITTED")
				}
			})
		}
	}
}

func TestRefusalPositionContainerKeepRowsEveryCarrier(t *testing.T) {
	muts := map[string]func(*corev1.Container){
		"72-envFrom-secret": func(c *corev1.Container) { c.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{}}} },
		"72-envFrom-cm": func(c *corev1.Container) {
			c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{}}}
		},
		"72-secretKeyRef": func(c *corev1.Container) {
			c.Env = []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}}}
		},
		"72-cmKeyRef": func(c *corev1.Container) {
			c.Env = []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{Key: "k"}}}}
		},
		"73-fieldRef-node": func(c *corev1.Container) {
			c.Env = []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}}}
		},
		"73-fieldRef-ann": func(c *corev1.Container) {
			c.Env = []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations['x']"}}}}
		},
		"73-fieldRef-space": func(c *corev1.Container) {
			c.Env = []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: " spec.serviceAccountName"}}}}
		},
		"73-fieldRef-tmpl": func(c *corev1.Container) {
			c.Env = []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "{{workflow.parameters.p}}"}}}}
		},
		"73-fieldRef-empty": func(c *corev1.Container) {
			c.Env = []corev1.EnvVar{{Name: "A", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{}}}}
		},
		"74-ARGO":     func(c *corev1.Container) { c.Env = []corev1.EnvVar{{Name: "ARGO_DEBUG_PAUSE_AFTER", Value: "true"}} },
		"75-hostPort": func(c *corev1.Container) { c.Ports = []corev1.ContainerPort{{ContainerPort: 80, HostPort: 80}} },
		"75-hostIP":   func(c *corev1.Container) { c.Ports = []corev1.ContainerPort{{ContainerPort: 80, HostIP: "0.0.0.0"}} },
		"76-secctx":   func(c *corev1.Container) { c.SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)} },
		"77-claims":   func(c *corev1.Container) { c.Resources.Claims = []corev1.ResourceClaim{{Name: "gpu"}} },
		"79-mount": func(c *corev1.Container) {
			c.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "/run/oberth/bin"}}
		},
		"79-mount-var": func(c *corev1.Container) {
			c.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "/var/run/oberth"}}
		},
		"79-mount-dotdot": func(c *corev1.Container) {
			c.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "/x/../run/oberth/bin"}}
		},
		"79-mount-tmpl": func(c *corev1.Container) {
			c.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "/{{inputs.parameters.p}}"}}
		},
		"79-device": func(c *corev1.Container) {
			c.VolumeDevices = []corev1.VolumeDevice{{Name: "v", DevicePath: "/run/oberth/x"}}
		},
		// kubelet: containerPath = filepath.Clean("/"+mountPath) for a relative path
		"79-mount-RELATIVE": func(c *corev1.Container) {
			c.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "run/oberth/bin"}}
		},
		"79-mount-RELATIVE-dot": func(c *corev1.Container) {
			c.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "./var/run/oberth"}}
		},
		"79-mount-RELATIVE-up": func(c *corev1.Container) {
			c.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "../run/oberth/vault-ca"}}
		},
	}
	forms := []string{"container", "script", "set", "init", "sidecar"}
	for name, mut := range muts {
		for _, form := range forms {
			for _, carrier := range refusalCarriers {
				if carrier == "defaults" && (form == "init" || form == "sidecar") {
					continue // refused wholesale; separately asserted below
				}
				t.Run(name+"/"+form+"/"+carrier, func(t *testing.T) {
					c := corev1.Container{Image: refusalImage, Command: []string{"/bin/true"}}
					mut(&c)
					tmpl := refusalLeaf()
					switch form {
					case "container":
						tmpl.Container = &c
					case "script":
						tmpl.Container = nil
						tmpl.Script = &wfv1.ScriptTemplate{Container: c, Source: "true"}
					case "set":
						c.Name = "main"
						tmpl.Container = nil
						tmpl.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: c}}}
					case "init":
						c.Name = "i"
						tmpl.InitContainers = []wfv1.UserContainer{{Container: c}}
					case "sidecar":
						c.Name = "s"
						tmpl.Sidecars = []wfv1.UserContainer{{Container: c}}
					}
					if err := Admit(refusalCarry(carrier, tmpl), Policy{}); err == nil {
						t.Errorf("ADMITTED")
					}
				})
			}
		}
	}
	// containerSet-common mounts
	for _, p := range []string{"/run/oberth", "run/oberth/bin"} {
		tmpl := wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{VolumeMounts: []corev1.VolumeMount{{Name: "v", MountPath: p}}, Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", Image: refusalImage}}}}}
		if err := Admit(refusalCarry("top", tmpl), Policy{}); err == nil {
			t.Errorf("containerSet common mount %q ADMITTED", p)
		}
	}
	for _, form := range []string{"init", "sidecar"} {
		tmpl := refusalLeaf()
		if form == "init" {
			tmpl.InitContainers = []wfv1.UserContainer{{Container: corev1.Container{Name: "i", Image: refusalImage}}}
		} else {
			tmpl.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Name: "i", Image: refusalImage}}}
		}
		if err := Admit(refusalCarry("defaults", tmpl), Policy{}); err == nil {
			t.Errorf("defaults %s ADMITTED", form)
		}
	}
}

func TestRefusalPositionArgumentsAndRefsEveryCarrier(t *testing.T) {
	cm := wfv1.Arguments{Parameters: []wfv1.Parameter{{Name: "p", ValueFrom: &wfv1.ValueFrom{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{Key: "k"}}}}}
	art := wfv1.Arguments{Artifacts: wfv1.Artifacts{{Name: "a"}}}
	ref := &wfv1.TemplateRef{Name: "o", Template: "t"}
	type c struct {
		name string
		w    func() *wfv1.Workflow
	}
	base := func(f func(w *wfv1.Workflow)) func() *wfv1.Workflow {
		return func() *wfv1.Workflow {
			leaf := refusalLeaf()
			leaf.Name = "leaf"
			w := &wfv1.Workflow{}
			w.Spec.Entrypoint = "main"
			w.Spec.Templates = []wfv1.Template{
				{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "s", Template: "leaf"}}}}},
				{Name: "d", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "t", Template: "leaf"}}}},
				leaf,
			}
			f(w)
			return w
		}
	}
	var cases []c
	for an, a := range map[string]wfv1.Arguments{"cm": cm, "artifact": art} {
		a := a
		cases = append(cases,
			c{an + "/spec.arguments", base(func(w *wfv1.Workflow) { w.Spec.Arguments = a })},
			c{an + "/step.arguments", base(func(w *wfv1.Workflow) { w.Spec.Templates[0].Steps[0].Steps[0].Arguments = a })},
			c{an + "/task.arguments", base(func(w *wfv1.Workflow) { w.Spec.Templates[1].DAG.Tasks[0].Arguments = a })},
			c{an + "/spec.hook", base(func(w *wfv1.Workflow) { w.Spec.Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf", Arguments: a}} })},
			c{an + "/step.hook", base(func(w *wfv1.Workflow) {
				w.Spec.Templates[0].Steps[0].Steps[0].Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf", Arguments: a}}
			})},
			c{an + "/task.hook", base(func(w *wfv1.Workflow) {
				w.Spec.Templates[1].DAG.Tasks[0].Hooks = wfv1.LifecycleHooks{"exit": {Template: "leaf", Arguments: a}}
			})},
		)
	}
	cases = append(cases,
		c{"ref/step", base(func(w *wfv1.Workflow) { w.Spec.Templates[0].Steps[0].Steps[0].TemplateRef = ref })},
		c{"ref/task", base(func(w *wfv1.Workflow) { w.Spec.Templates[1].DAG.Tasks[0].TemplateRef = ref })},
		c{"ref/spec.hook", base(func(w *wfv1.Workflow) { w.Spec.Hooks = wfv1.LifecycleHooks{"exit": {TemplateRef: ref}} })},
		c{"ref/step.hook", base(func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Steps[0].Steps[0].Hooks = wfv1.LifecycleHooks{"x": {TemplateRef: ref}}
		})},
		c{"ref/task.hook", base(func(w *wfv1.Workflow) {
			w.Spec.Templates[1].DAG.Tasks[0].Hooks = wfv1.LifecycleHooks{"x": {TemplateRef: ref}}
		})},
		c{"ref/clusterscope", base(func(w *wfv1.Workflow) {
			w.Spec.Templates[1].DAG.Tasks[0].TemplateRef = &wfv1.TemplateRef{Name: "o", Template: "t", ClusterScope: true}
		})},
		c{"ref/workflowTemplateRef", base(func(w *wfv1.Workflow) { w.Spec.WorkflowTemplateRef = &wfv1.WorkflowTemplateRef{Name: "o"} })},
		c{"spec/podSpecPatch", base(func(w *wfv1.Workflow) { w.Spec.PodSpecPatch = "{}" })},
		c{"spec/artifactGC.podSpecPatch", base(func(w *wfv1.Workflow) { w.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{PodSpecPatch: "{}"} })},
		c{"spec/dnsPolicy", base(func(w *wfv1.Workflow) { p := corev1.DNSDefault; w.Spec.DNSPolicy = &p })},
		c{"spec/podPriorityClassName", base(func(w *wfv1.Workflow) { w.Spec.PodPriorityClassName = "x" })},
		c{"spec/securityContext", base(func(w *wfv1.Workflow) { w.Spec.SecurityContext = &corev1.PodSecurityContext{} })},
		c{"spec/sem", base(func(w *wfv1.Workflow) {
			w.Spec.Synchronization = &wfv1.Synchronization{Semaphores: []*wfv1.SemaphoreRef{{Database: &wfv1.SyncDatabaseRef{Key: "k"}}}}
		})},
		c{"spec/podMetadata.ann", base(func(w *wfv1.Workflow) { w.Spec.PodMetadata = &wfv1.Metadata{Annotations: map[string]string{"a": "b"}} })},
		c{"vct/volumeName", base(func(w *wfv1.Workflow) {
			w.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "d"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv"}}}
		})},
		c{"vct/selector", base(func(w *wfv1.Workflow) {
			w.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "d"}, Spec: corev1.PersistentVolumeClaimSpec{Selector: &metav1.LabelSelector{}}}}
		})},
		c{"vct/dataSource", base(func(w *wfv1.Workflow) {
			w.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "d"}, Spec: corev1.PersistentVolumeClaimSpec{DataSource: &corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: "x"}}}}
		})},
		c{"vct/dataSourceRef", base(func(w *wfv1.Workflow) {
			w.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "d"}, Spec: corev1.PersistentVolumeClaimSpec{DataSourceRef: &corev1.TypedObjectReference{Kind: "PersistentVolumeClaim", Name: "x"}}}}
		})},
		c{"vct/src", base(func(w *wfv1.Workflow) {
			w.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "src"}}}
		})},
		c{"wfmeta/labelsFrom-oberth", base(func(w *wfv1.Workflow) {
			w.Spec.WorkflowMetadata = &wfv1.WorkflowMetadata{LabelsFrom: map[string]wfv1.LabelValueFrom{"oberth.ci/run": {Expression: "'x'"}}}
		})},
		c{"wfmeta/ann-argo", base(func(w *wfv1.Workflow) {
			w.Spec.WorkflowMetadata = &wfv1.WorkflowMetadata{Annotations: map[string]string{"workflows.argoproj.io/pod-name-format": "v1"}}
		})},
		c{"wfmeta/templated", base(func(w *wfv1.Workflow) {
			w.Spec.WorkflowMetadata = &wfv1.WorkflowMetadata{Labels: map[string]string{"{{workflow.parameters.k}}": "v"}}
		})},
		// supported author annotations must NOT be smuggled via workflowMetadata
		c{"wfmeta/supported-secret-paths", base(func(w *wfv1.Workflow) {
			w.Spec.WorkflowMetadata = &wfv1.WorkflowMetadata{Annotations: map[string]string{SecretPathsAnnotation: "a/b"}}
		})},
		// own metadata
		c{"meta/label-oberth", base(func(w *wfv1.Workflow) { w.Labels = map[string]string{"oberth.ci/tier": "release"} })},
		c{"meta/label-argo", base(func(w *wfv1.Workflow) { w.Labels = map[string]string{"workflows.argoproj.io/completed": "true"} })},
		c{"meta/ann-wif", base(func(w *wfv1.Workflow) { w.Annotations = map[string]string{"oberth.ci/release-wif-policy-sha256": "00"} })},
		c{"meta/ann-podname", base(func(w *wfv1.Workflow) {
			w.Annotations = map[string]string{"workflows.argoproj.io/pod-name-format": "v1"}
		})},
		c{"meta/ann-fragments", base(func(w *wfv1.Workflow) { w.Annotations = map[string]string{"oberth.ci/fragments": "{}"} })},
		c{"meta/ann-capability", base(func(w *wfv1.Workflow) { w.Annotations = map[string]string{"oberth.ci/workflow-capability": "x"} })},
		c{"meta/label-supported-key-as-label", base(func(w *wfv1.Workflow) { w.Labels = map[string]string{SecretPathsAnnotation: "x"} })},
		c{"meta/name", base(func(w *wfv1.Workflow) { w.Name = "x" })},
		c{"meta/ns", base(func(w *wfv1.Workflow) { w.Namespace = "x" })},
		c{"meta/owner", base(func(w *wfv1.Workflow) { w.OwnerReferences = []metav1.OwnerReference{{Name: "x"}} })},
		c{"meta/fin", base(func(w *wfv1.Workflow) { w.Finalizers = []string{"x/y"} })},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Admit(tc.w(), Policy{}); err == nil {
				t.Errorf("ADMITTED")
			}
		})
	}

}

func TestRefusalPositionDupRowsNoPanic(t *testing.T) {
	leaf := func(n string) wfv1.Template { x := refusalLeaf(); x.Name = n; return x }
	neg := resource.MustParse("-1")
	docs := map[string]*wfv1.Workflow{
		"zero":             {},
		"no-entrypoint":    {Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{leaf("main")}}},
		"no-templates":     {Spec: wfv1.WorkflowSpec{Entrypoint: "main"}},
		"dup-names":        {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{leaf("main"), leaf("main")}}},
		"empty-name":       {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{leaf(""), leaf("")}}},
		"undeclared-entry": {Spec: wfv1.WorkflowSpec{Entrypoint: "ghost", Templates: []wfv1.Template{leaf("main")}}},
		"undeclared-ref":   {Spec: wfv1.WorkflowSpec{Entrypoint: "main", OnExit: "ghost", Hooks: wfv1.LifecycleHooks{"x": {Template: "ghost"}}, Templates: []wfv1.Template{{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "s", Template: "ghost", Hooks: wfv1.LifecycleHooks{"x": {Template: "ghost2"}}}, {Name: "", Template: ""}}}}}, {Name: "d", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "t", Template: "ghost", Depends: "nope", Dependencies: []string{"nope2"}}}}}}}},
		"self-recursion":   {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "a", Template: "main"}, {Name: "b", Template: "main"}}}}}}}},
		"mutual-recursion": {Spec: wfv1.WorkflowSpec{Entrypoint: "a", Templates: []wfv1.Template{{Name: "a", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "x", Template: "b"}, {Name: "l", Template: "leaf"}}}}, {Name: "b", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "y", Template: "a"}, {Name: "l", Template: "leaf"}}}}}, leaf("leaf")}}},
		"negative-resources": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{func() wfv1.Template {
			x := leaf("main")
			x.Container.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{"cpu": neg, "memory": resource.MustParse("10Gi")}, Limits: corev1.ResourceList{"memory": resource.MustParse("1Mi")}}
			return x
		}()}}},
		"relative-mount": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}, Templates: []wfv1.Template{func() wfv1.Template {
			x := leaf("main")
			x.Container.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "rel/x"}, {Name: "v", MountPath: ""}}
			return x
		}()}}},
		"unknown-volume": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{func() wfv1.Template {
			x := leaf("main")
			x.Container.VolumeMounts = []corev1.VolumeMount{{Name: "ghost", MountPath: "/x"}}
			return x
		}()}}},
		"nil-pieces":         {Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{}}, {Name: "s", Steps: []wfv1.ParallelSteps{{}}}, {Name: "cs", ContainerSet: &wfv1.ContainerSetTemplate{}}, {Name: "nothing"}}}},
		"deadline-composite": {Spec: wfv1.WorkflowSpec{Entrypoint: "main", ActiveDeadlineSeconds: ptr.To(int64(-5)), Templates: []wfv1.Template{{Name: "main", ActiveDeadlineSeconds: &intstr.IntOrString{Type: intstr.Int, IntVal: 5}, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "t", Template: "leaf", Depends: "x", Dependencies: []string{"y"}}}}}, leaf("leaf")}}},
	}
	// exponential diamond without pods, depth 60
	var dia []wfv1.Template
	for i := 0; i < 60; i++ {
		next := fmt.Sprintf("t%d", i+1)
		dia = append(dia, wfv1.Template{Name: fmt.Sprintf("t%d", i), Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "a", Template: next}, {Name: "b", Template: next}}}}})
	}
	dia = append(dia, wfv1.Template{Name: "t60", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "back", Template: "t0"}}}}})
	docs["diamond-cycle"] = &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "t0", Templates: dia}}
	for name, w := range docs {
		t.Run(name, func(t *testing.T) {
			done := make(chan string, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						done <- fmt.Sprintf("PANIC %v", r)
					}
				}()
				aerr := Admit(w.DeepCopy(), Policy{})
				steps, perr := PlannedSteps(w.DeepCopy())
				_, nerr := DeclaredNonrootTemplates(w)
				done <- fmt.Sprintf("admit=%v | plan=%d,%v | nonroot=%v", aerr, len(steps), perr, nerr)
			}()
			select {
			case s := <-done:
				if strings.HasPrefix(s, "PANIC") {
					t.Errorf("%s", s)
				}
				t.Log(s)
			case <-time.After(20 * time.Second):
				t.Errorf("TIMEOUT (unbounded walk)")
			}
		})
	}
	for _, raw := range []string{"", "   \n", "---\n", "null", "{}", "[]", "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\n", "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec: {}\n", "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  templates: [null]\n"} {
		w, err := Decode([]byte(raw))
		if err != nil {
			t.Logf("decode %q -> err %v", raw, err)
			continue
		}
		aerr := Admit(w, Policy{})
		_, perr := PlannedSteps(w)
		t.Logf("decode %q -> ok; admit=%v plan=%v", raw, aerr, perr)
	}
}
