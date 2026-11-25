package main

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const walkRoleLabel = "oberth.walk/role"

type walkSetup struct {
	roles    map[string]rbacv1.Role
	bindings map[string]rbacv1.RoleBinding
	policies map[string]networkingv1.NetworkPolicy
}

func readWalkSetup(t *testing.T) walkSetup {
	t.Helper()
	body, err := os.ReadFile(walkResourcesFile)
	if err != nil {
		t.Fatal(err)
	}
	s := walkSetup{map[string]rbacv1.Role{}, map[string]rbacv1.RoleBinding{}, map[string]networkingv1.NetworkPolicy{}}
	for _, document := range strings.Split(string(body), "\n---\n") {
		var head struct{ Kind string }
		if err = yaml.Unmarshal([]byte(document), &head); err != nil {
			t.Fatal(err)
		}
		switch head.Kind {
		case "Role":
			var o rbacv1.Role
			err = yaml.UnmarshalStrict([]byte(document), &o)
			s.roles[o.Name] = o
		case "RoleBinding":
			var o rbacv1.RoleBinding
			err = yaml.UnmarshalStrict([]byte(document), &o)
			s.bindings[o.Name] = o
		case "NetworkPolicy":
			var o networkingv1.NetworkPolicy
			err = yaml.UnmarshalStrict([]byte(document), &o)
			s.policies[o.Name] = o
		}
		if err != nil {
			t.Fatal(head.Kind, err)
		}
	}
	return s
}

// The executor may only create, poll and delete the walk's three kinds; delete
// needs get, list and watch because kubectl waits until the object is gone.
func TestWalkSetupGrantsExactExecutorVerbs(t *testing.T) {
	s := readWalkSetup(t)
	verbs := []string{"create", "get", "list", "watch", "delete"}
	want := map[string]string{"kubevirt.io": "virtualmachineinstances", "": "persistentvolumeclaims", "batch": "jobs"}
	role := s.roles["walk-executor"]
	if role.Namespace != "oberth-walk" || len(role.Rules) != len(want) {
		t.Fatal("walk executor Role is missing or broader", role.Rules)
	}
	for _, rule := range role.Rules {
		if len(rule.APIGroups) != 1 || !slices.Equal(rule.Resources, []string{want[rule.APIGroups[0]]}) || !slices.Equal(rule.Verbs, verbs) || len(rule.ResourceNames) != 0 {
			t.Fatal("walk executor rule is not exact", rule)
		}
		delete(want, rule.APIGroups[0])
	}
	if len(want) != 0 {
		t.Fatal("walk executor Role misses a kind", want)
	}
	assertWalkBinding(t, s.bindings["walk-executor"])
}

func assertWalkBinding(t *testing.T, binding rbacv1.RoleBinding) {
	t.Helper()
	subject := rbacv1.Subject{Kind: "ServiceAccount", Name: "REPLACE_WITH_VM_EXECUTOR_ACCOUNT", Namespace: "REPLACE_WITH_PIPELINE_NAMESPACE"}
	if binding.Namespace != "oberth-walk" || binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != "walk-executor" || !slices.Equal(binding.Subjects, []rbacv1.Subject{subject}) {
		t.Fatal("walk executor Role is not bound to the executor account only", binding)
	}
}

// The disk Job reaches only DNS and HTTPS; the VMI accepts only SSH from the
// pipeline namespace and sends nothing. The labels match the walk manifests.
func TestWalkSetupLimitsJobAndVMNetwork(t *testing.T) {
	s := readWalkSetup(t)
	disk, vm := s.policies["walk-guest-disk"], s.policies["walk-vm"]
	assertWalkPolicySelects(t, disk, "guest-disk")
	assertWalkPolicySelects(t, vm, "walk-vm")
	if len(disk.Spec.Ingress) != 0 || !slices.Equal(policyPorts(t, disk.Spec.Egress), []string{"UDP/53", "TCP/53", "TCP/443"}) {
		t.Fatal("disk Job network is wider than DNS and HTTPS", disk.Spec)
	}
	ssh := vm.Spec.Ingress
	if len(vm.Spec.Egress) != 0 || len(ssh) != 1 || len(ssh[0].From) != 1 || !slices.Equal(policyPorts(t, nil, ssh[0].Ports...), []string{"TCP/22"}) {
		t.Fatal("walk VM network is wider than SSH", vm.Spec)
	}
	from := ssh[0].From[0]
	if from.IPBlock != nil || from.PodSelector != nil || from.NamespaceSelector == nil || !maps.Equal(from.NamespaceSelector.MatchLabels, map[string]string{"kubernetes.io/metadata.name": "REPLACE_WITH_PIPELINE_NAMESPACE"}) {
		t.Fatal("walk VM accepts SSH from outside the pipeline namespace", from)
	}
}

func assertWalkPolicySelects(t *testing.T, policy networkingv1.NetworkPolicy, role string) {
	t.Helper()
	both := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
	selector := policy.Spec.PodSelector
	if policy.Namespace != "oberth-walk" || !maps.Equal(selector.MatchLabels, map[string]string{walkRoleLabel: role}) || len(selector.MatchExpressions) != 0 || !slices.Equal(policy.Spec.PolicyTypes, both) {
		t.Fatal("walk Pod is not isolated by its NetworkPolicy", role)
	}
}

// policyPorts lists protocol/port of peer-free egress rules and the extra ports.
func policyPorts(t *testing.T, rules []networkingv1.NetworkPolicyEgressRule, extra ...networkingv1.NetworkPolicyPort) []string {
	t.Helper()
	ports := extra
	for _, rule := range rules {
		if len(rule.To) != 0 {
			t.Fatal("walk egress names peers it does not need", rule.To)
		}
		ports = append(ports, rule.Ports...)
	}
	out := []string{}
	for _, port := range ports {
		if port.Protocol == nil || port.Port == nil || port.EndPort != nil {
			t.Fatal("walk policy port is open-ended", port)
		}
		out = append(out, string(*port.Protocol)+"/"+port.Port.String())
	}
	return out
}
