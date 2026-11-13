package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	kt "k8s.io/client-go/testing"
)

func TestApplyJSONPreservesNetworkPolicyFailure(t *testing.T) {
	for _, mode := range []string{"absent enforcement", "cleanup failure"} {
		t.Run(mode, func(t *testing.T) {
			kube := fake.NewClientset(preflightNode(), preflightStorage())
			probeNamespaceUIDFixture(kube)
			kube.PrependReactor("create", "pods", readyPolicyModelPod)
			kube.PrependReactor("delete", "namespaces", func(kt.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "scratch", errors.New("fixture"))
			})
			model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "timeout"}
			ca := newTestServerCA(t, []string{"api.example.test"})
			var output bytes.Buffer
			calls, loads, helmReads := 0, 0, 0
			var measured NetworkPolicyProbeResult
			host := InstallDeps{Output: &output, CredentialSink: func(string, string) {},
				LookPath: func(name string) (string, error) { return name, nil },
				LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
					loads++
					return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "chosen", nil
				},
				RunCommand: preflightStats,
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if len(args) < 3 || args[0] != "--kube-context" || args[1] != "chosen" || args[2] != "list" {
						t.Fatalf("unexpected Helm command: %v", args)
					}
					helmReads++
					return []byte("[]"), nil
				},
				ProbeNetworkPolicy: func(ctx context.Context, cfg Config, deps Deps) NetworkPolicyProbeResult {
					calls++
					if mode == "cleanup failure" {
						deps.RunCommand = model.run
						measured = probeNetworkPolicy(ctx, cfg, deps, policyModelTiming())
					} else {
						measured = NetworkPolicyProbeResult{Status: "absent", Evidence: "Fixture observed allowed traffic with default deny.", Remedy: "Enable dataplane enforcement and retry install."}
					}
					return measured
				},
			}
			err := ExecuteJSON(t.Context(), Config{ContextName: "chosen", ClientAccess: "none"}, host)
			if err == nil {
				t.Fatal("JSON apply accepted failed measurement")
			}
			var result InstallResult
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.OK || calls != 1 || loads != 1 || helmReads != 1 {
				t.Fatalf("apply failure was re-planned: ok=%t calls=%d loads=%d reads=%d", result.OK, calls, loads, helmReads)
			}
			found := 0
			for _, check := range result.Checks {
				if check.Code != "network_policy" {
					continue
				}
				found++
				if check.OK || check.Warning || check.Reason != measured.Status || check.Remedy != measured.Remedy || check.Evidence != measured.Evidence {
					t.Fatalf("JSON lost apply observation: %+v; measured: %+v", check, measured)
				}
				if strings.Contains(check.Remedy, "first at install") {
					t.Fatal("apply failure became a deferred plan warning")
				}
			}
			if found != 1 {
				t.Fatalf("expected one policy check, got %d", found)
			}
			if result.Context != "chosen" || result.Review == nil || result.Review.Context != "chosen" || !slices.Contains(result.Review.DNSNames, "node.local") || result.Cluster == nil || result.Cluster.Context != "chosen" || result.Cluster.Server != "https://api.example.test" || result.Cluster.CAPin == "" || len(result.Cluster.Nodes) != 1 {
				t.Fatalf("apply preflight identity or review was lost: %+v", result)
			}
			remaining, listErr := kube.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
			if listErr != nil {
				t.Fatal(listErr)
			}
			if mode == "cleanup failure" {
				if measured.Status != "unverified" || len(remaining.Items) != 1 || !strings.Contains(measured.Remedy, "Cleanup is unverified") || !strings.Contains(measured.Remedy, remaining.Items[0].Name) {
					t.Fatalf("cleanup failure omitted owned leftover: %+v", measured)
				}
			} else if measured.Status != "absent" || len(remaining.Items) != 0 {
				t.Fatal("absent enforcement fixture changed", measured)
			}
		})
	}
}
