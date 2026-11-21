package argoworkflow

import (
	"fmt"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"path"
	"slices"
	"strings"
)

type containerAt struct {
	container *corev1.Container
	path      string
	main      bool
}

func containers(t *wfv1.Template) []containerAt {
	var result []containerAt
	if t.Container != nil {
		result = append(result, containerAt{t.Container, ".container", true})
	}
	if t.Script != nil {
		result = append(result, containerAt{&t.Script.Container, ".script", true})
	}
	if t.ContainerSet != nil {
		for i := range t.ContainerSet.Containers {
			result = append(result, containerAt{&t.ContainerSet.Containers[i].Container, fmt.Sprintf(".containerSet.containers[%d]", i), true})
		}
	}
	for i := range t.InitContainers {
		result = append(result, containerAt{&t.InitContainers[i].Container, fmt.Sprintf(".initContainers[%d]", i), false})
	}
	for i := range t.Sidecars {
		result = append(result, containerAt{&t.Sidecars[i].Container, fmt.Sprintf(".sidecars[%d]", i), false})
	}
	return result
}
func (g *graph) container(c containerAt, n *node, where string, allowed map[string]bool) {
	if reservedContainer(c.container.Name, c.main) {
		g.refuse(where+".name", "reserved")
	}
	if !imageAllowed(c.container.Image, g.policy.ImagePrefixes) {
		g.refuse(where+".image", "image")
	}
	g.containerSecurity(c.container.SecurityContext, where+".securityContext")
	for i, p := range c.container.Ports {
		if p.HostIP != "" || p.HostPort != 0 {
			g.refuse(fmt.Sprintf("%s.ports[%d]", where, i), "host")
		}
	}
	g.environment(c.container, n, where)
	g.mounts(c.container.VolumeMounts, c.container.VolumeDevices, where, allowed)
}
func reservedContainer(name string, main bool) bool {
	return reservedName(name) || name == "init" || name == "wait" || !main && name == "main" || strings.HasPrefix(name, "init-artifact-")
}
func reservedName(name string) bool {
	return strings.ContainsAny(name, "{}") || strings.HasPrefix(name, "oberth-") || strings.HasPrefix(name, "artifact-plugin-") || slices.Contains([]string{"exec-sa-token", "tmp-dir-argo", "var-run-argo", "argo-staging", "argo-env-config", "input-artifacts", "kubeconfig"}, name)
}
func imageAllowed(image string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	if strings.ContainsAny(image, "{}") {
		return false
	}
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(image, prefix) {
			return true
		}
	}
	return false
}
func cleanPath(p string) string {
	p = path.Clean("/" + p)
	if p == "/var/run" || strings.HasPrefix(p, "/var/run/") {
		p = strings.TrimPrefix(p, "/var")
	}
	return p
}
func overlaps(a, b string) bool {
	a = cleanPath(a)
	b = cleanPath(b)
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}

// ReservedMountRoots returns the delivery namespaces reserved by admission.
// Native execution keeps the mounts it adds inside these roots.
func ReservedMountRoots() []string {
	return []string{"/run/oberth", "/run/oberth-secrets", "/run/oberth-gar", "/run/secrets/kubernetes.io/serviceaccount", "/run/secrets/secretstore", "/run/argo", "/argo/staging"}
}

func parentPath(value string) bool { return slices.Contains(strings.Split(value, "/"), "..") }
func invalidMountPath(value string) bool {
	return !path.IsAbs(value) || parentPath(value)
}
func protectedPath(value string) bool { return invalidMountPath(value) || reservedPath(value) }
func reservedPath(value string) bool {
	if strings.ContainsAny(value, "{}") {
		return true
	}
	if cleanPath(value) == "/var" {
		return true
	}
	for _, root := range ReservedMountRoots() {
		if overlaps(value, root) {
			return true
		}
	}
	return false
}
func (g *graph) mounts(mounts []corev1.VolumeMount, devices []corev1.VolumeDevice, where string, allowed map[string]bool) {
	for _, m := range mounts {
		g.mount(m, where, allowed)
	}
	for _, d := range devices {
		if reservedName(d.Name) || protectedPath(d.DevicePath) {
			g.refuse(where+".volumeDevices["+d.Name+"]", "path")
		}
	}
}

func (g *graph) mount(m corev1.VolumeMount, where string, allowed map[string]bool) {
	at := where + ".volumeMounts[" + m.Name + "]"
	if reservedName(m.Name) {
		g.refuse(at+".name", "reserved")
	}
	if invalidMountPath(m.MountPath) || reservedPath(m.MountPath) && (!allowed[m.Name] || cleanPath(m.MountPath) != "/run/secrets/secretstore") {
		g.refuse(fmt.Sprintf("%s.mountPath[%q]", at, m.MountPath), "path")
	}
	if path.IsAbs(m.SubPath) || parentPath(m.SubPath) {
		g.refuse(fmt.Sprintf("%s.subPath[%q]", at, m.SubPath), "path")
	}
}

func tokenVolumeNames(t *wfv1.Template) map[string]bool {
	result := map[string]bool{}
	for _, v := range t.Volumes {
		if tokenProjection(v) {
			result[v.Name] = true
		}
	}
	return result
}

func (g *graph) environment(c *corev1.Container, n *node, where string) {
	for i, e := range c.EnvFrom {
		g.clusterReads(e, n, fmt.Sprintf("%s.envFrom[%d]", where, i))
	}
	for _, e := range c.Env {
		at := where + ".env[" + e.Name + "]"
		if !literal(e.Name) || strings.HasPrefix(e.Name, "ARGO_") {
			g.refuse(at, "reserved")
		}
		g.clusterReads(e.ValueFrom, n, at+".valueFrom")
	}
}

func (g *graph) containerSecurity(s *corev1.SecurityContext, where string) {
	if s == nil {
		return
	}
	if s.Privileged != nil && *s.Privileged {
		g.refuse(where+".privileged", "host")
	}
	if w := s.WindowsOptions; w != nil && w.HostProcess != nil && *w.HostProcess {
		g.refuse(where+".windowsOptions.hostProcess", "host")
	}
}
