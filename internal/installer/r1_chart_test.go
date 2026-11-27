package installer_test

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestExactPipelineEgress(t *testing.T) {
	t.Parallel()
	got := object(t, rendered(t, nil), "NetworkPolicy", "oberth-egress")["spec"]
	raw := `podSelector: {}
policyTypes: [Egress]
egress:
- to:
  - namespaceSelector:
      matchLabels: {kubernetes.io/metadata.name: kube-system}
    podSelector:
      matchLabels: {k8s-app: kube-dns}
  ports: [{protocol: UDP, port: 53}, {protocol: TCP, port: 53}]
- to:
  - podSelector: {}
- to:
  - namespaceSelector:
      matchLabels: {kubernetes.io/metadata.name: oberth}
    podSelector:
      matchLabels: {app: oberth}
  ports: [{protocol: TCP, port: https}]
- to:
  - namespaceSelector:
      matchLabels: {kubernetes.io/metadata.name: oberth}
    podSelector:
      matchLabels: {app: oberth}
  ports: [{protocol: TCP, port: ssh}]
- to:
  - namespaceSelector:
      matchLabels: {kubernetes.io/metadata.name: oberth}
    podSelector:
      matchLabels: {app: oberth}
  ports: [{protocol: TCP, port: source}]
- to:
  - namespaceSelector:
      matchLabels: {kubernetes.io/metadata.name: oberth}
    podSelector:
      matchLabels: {app.kubernetes.io/name: storeNative, app.kubernetes.io/instance: oberth, component: server}
  ports: [{protocol: TCP, port: 8200}]
- to:
  - ipBlock:
      cidr: 0.0.0.0/0
      except: [169.254.0.0/16, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10]
`
	var want map[string]any
	if err := yaml.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatal(err)
	}
	if jsonOf(t, got) != jsonOf(t, want) {
		t.Fatal("egress scope differs", got)
	}
}

// Pod selectors permit server Git without granting other namespace peers.
func TestStepPodsCanReachServerGitSSH(t *testing.T) {
	t.Parallel()
	for _, namespace := range []string{"oberth", "control"} {
		t.Run(namespace, func(t *testing.T) {
			objects := renderedIn(t, map[string]any{"pipeline": map[string]any{"namespace": "steps"}}, namespace)
			var policy networkingv1.NetworkPolicy
			decodeObject(t, object(t, objects, "NetworkPolicy", "oberth-egress"), &policy)
			if policy.Namespace != "steps" || len(policy.Spec.PodSelector.MatchLabels) != 0 || len(policy.Spec.PodSelector.MatchExpressions) != 0 {
				t.Fatal("policy must cover step Pods", policy)
			}
			server := serverDeployment(t, objects).Spec.Template
			if server.Labels["app"] != "oberth" {
				t.Fatal("SSH target selector must match the server Pod", server.Labels)
			}
			var want networkingv1.NetworkPolicyEgressRule
			raw := `to:
- namespaceSelector:
    matchLabels: {kubernetes.io/metadata.name: NAMESPACE}
  podSelector:
    matchLabels: {app: oberth}
ports: [{protocol: TCP, port: ssh}]`
			if err := yaml.Unmarshal([]byte(strings.ReplaceAll(raw, "NAMESPACE", namespace)), &want); err != nil {
				t.Fatal(err)
			}
			for _, rule := range policy.Spec.Egress {
				if reflect.DeepEqual(rule, want) {
					return
				}
			}
			t.Fatal("no explicit step-to-server TCP 2222 rule", policy.Spec.Egress)
		})
	}
}

func TestServerContainerSecurityAndMountIsolation(t *testing.T) {
	t.Parallel()
	pod := serverDeployment(t, rendered(t, nil)).Spec.Template.Spec
	c := pod.Containers[0]
	s := c.SecurityContext
	want := `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}`
	var expected corev1.SecurityContext
	if err := yaml.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, &expected) {
		t.Fatal(s)
	}
	for _, a := range c.VolumeMounts {
		for _, b := range c.VolumeMounts {
			if a.MountPath != b.MountPath && strings.HasPrefix(a.MountPath, b.MountPath+"/") {
				t.Fatal("nested mounts", a, b)
			}
		}
	}
	if c.LivenessProbe.InitialDelaySeconds != 0 || c.ReadinessProbe.InitialDelaySeconds != 0 || c.StartupProbe != nil {
		t.Fatal("liveness not immediate")
	}
}
func TestControllerScopeAndNoTestHooks(t *testing.T) {
	t.Parallel()
	objects := rendered(t, map[string]any{"pipeline": map[string]any{"namespace": "jobs"}})
	deployment := object(t, objects, "Deployment", "oberth-controller-workflow-controller")
	container := deployment["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	args := jsonOf(t, container["args"])
	if !strings.Contains(args, `"--namespaced"`) || !strings.Contains(args, `"--managed-namespace=jobs"`) {
		t.Fatal(args)
	}
	for _, o := range objects {
		name := o["metadata"].(map[string]any)["name"]
		if (o["kind"] == "ClusterRole" || o["kind"] == "ClusterRoleBinding") && !allowedClusterRoleName(name) {
			t.Fatal("controller has cluster authority", o)
		}
		if o["kind"] == "Pod" {
			t.Fatal("dependency test hook retained", o)
		}
	}
	assertNodeObserverScope(t, objects)
	binding := object(t, objects, "RoleBinding", "oberth-task-results")
	if jsonOf(t, binding["subjects"]) != `[{"apiGroup":"rbac.authorization.k8s.io","kind":"Group","name":"system:serviceaccounts:jobs"}]` {
		t.Fatal(binding)
	}
	role := object(t, objects, "Role", "oberth-task-results")
	if jsonOf(t, role["rules"]) != `[{"apiGroups":["argoproj.io"],"resources":["workflowtaskresults"],"verbs":["create","patch"]}]` {
		t.Fatal(role)
	}
}

func allowedClusterRoleName(name any) bool {
	switch name {
	case "oberth-source-review", "oberth-oberth-store-server-binding", "oberth-node-observer":
		return true
	}
	return false
}

func assertNodeObserverScope(t *testing.T, objects []map[string]any) {
	t.Helper()
	role := object(t, objects, "ClusterRole", "oberth-node-observer")
	if jsonOf(t, role["rules"]) != `[{"apiGroups":[""],"resources":["nodes"],"verbs":["get","list","watch"]}]` {
		t.Fatal("Node observer gained authority", role)
	}
	binding := object(t, objects, "ClusterRoleBinding", "oberth-node-observer")
	if jsonOf(t, binding["subjects"]) != `[{"kind":"ServiceAccount","name":"oberth","namespace":"oberth"}]` || jsonOf(t, binding["roleRef"]) != `{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"oberth-node-observer"}` {
		t.Fatal("Node observer escaped the server account", binding)
	}
}
func TestOwnedNativeInputsOnly(t *testing.T) {
	t.Parallel()
	for _, values := range []map[string]any{
		{"argoNative": map[string]any{"extraObjects": []any{}}},
		{"argoNative": map[string]any{"controller": map[string]any{"extraEnv": []any{}}}},
		{"storeNative": map[string]any{"server": map[string]any{"extraArgs": "unsafe"}}},
	} {
		f := setup(t)
		f.choices.Values = values
		if p, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
			p.Close()
			t.Fatal("unowned dependency input accepted", values)
		}
	}
	defaults := serverDeployment(t, rendered(t, nil)).Spec.Template.Spec
	if !reflect.DeepEqual(defaults.Containers[0].Args, []string{"serve", "--config", "/etc/oberth/config/server.json"}) {
		t.Fatal(defaults)
	}
}

func TestControllerExecutorFloor(t *testing.T) {
	t.Parallel()
	for _, floor := range []bool{true, false} {
		cm := object(t, rendered(t, map[string]any{"policies": map[string]any{"floor": floor}}), "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
		var executor map[string]any
		if err := yaml.Unmarshal([]byte(cm["executor"].(string)), &executor); err != nil {
			t.Fatal(err)
		}
		security := map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}
		if floor {
			if jsonOf(t, executor["securityContext"]) != jsonOf(t, security) {
				t.Fatal(executor)
			}
			var main map[string]any
			if err := yaml.Unmarshal([]byte(cm["mainContainer"].(string)), &main); err != nil {
				t.Fatal(err)
			}
			if jsonOf(t, main) != jsonOf(t, map[string]any{"securityContext": security}) {
				t.Fatal(main)
			}
		} else if executor["securityContext"] != nil || cm["mainContainer"] != nil {
			t.Fatal("explicit floor off ignored")
		}
		if executor["imagePullPolicy"] != "IfNotPresent" {
			t.Fatal(executor)
		}
	}
}
func TestPublicPortsAndRestartChecksums(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	pod := serverDeployment(t, objects).Spec.Template
	if *pod.Spec.SecurityContext.RunAsUser != 65534 || *pod.Spec.SecurityContext.RunAsGroup != 65534 {
		t.Fatal("server must not run as root")
	}
	if pod.Annotations["checksum/config"] == "" {
		t.Fatal("configuration changes cannot restart server")
	}
	changed := serverDeployment(t, rendered(t, map[string]any{"logs": map[string]any{"run_limit_bytes": 42}})).Spec.Template
	if pod.Annotations["checksum/config"] == changed.Annotations["checksum/config"] {
		t.Fatal("configuration checksum does not bind server JSON")
	}
	service := object(t, objects, "Service", "oberth")["spec"].(map[string]any)
	ports := service["ports"].([]any)
	if service["type"] != "NodePort" || ports[0].(map[string]any)["nodePort"] != float64(30443) || ports[1].(map[string]any)["nodePort"] != float64(30022) {
		t.Fatal(service)
	}
}
