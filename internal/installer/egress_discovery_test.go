package installer_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
)

func TestEgressUnreadableInventoryRefusesBeforeWrites(t *testing.T) {
	f := setup(t)
	f.client.PrependReactor("list", "services", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", errors.New("list denied"))
	})
	if _, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
		t.Fatal("unread network inventory accepted")
	}
	assertNoInstallWrites(t, f)
}

func TestEgressDiscoversClusterRangesAndBindsReview(t *testing.T) {
	f := setup(t)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"198.18.0.0/16", "2001:db8:100::/64"}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "203.0.113.22"}, {Type: corev1.NodeExternalIP, Address: "2001:db8:ffff::22"}}}}
	node, err := f.client.CoreV1().Nodes().Create(t.Context(), node, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.client.NetworkingV1().ServiceCIDRs().Create(t.Context(), &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"198.19.0.0/16", "2001:db8:200::/108"}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan := prepare(t, f)
	request := f.charts.Previews[len(f.charts.Previews)-1]
	var policy networkingv1.NetworkPolicy
	decodeObject(t, object(t, rendered(t, request.Values), "NetworkPolicy", "oberth-egress"), &policy)
	checkNetworkPackets(t, policy, true)
	node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "203.0.113.23"})
	if _, err = f.client.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	if _, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil); err == nil {
		t.Fatal("changed network inventory passed stale review")
	}
	assertNoInstallWrites(t, f)
}

func TestUpgradeReviewsChangedDefaultEgressAndOptOut(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		f, maintenance := maintenanceFixture(t)
		delete(f.charts.History.Deployed.Values, "egressNetwork")
		f.charts.History.Deployed.Values["policies"] = map[string]any{"floor": true, "egress": true}
		f.charts.History.Latest = f.charts.History.Deployed
		if disabled {
			on := false
			f.choices.Egress = &on
		}
		plan, err := maintenance.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Plan.Close()
		want := "link-local filter -> internet open, internal closed"
		if disabled {
			want = "link-local filter -> off"
		}
		if !strings.Contains(strings.Join(plan.Plan.Review(), "\n"), want+" (--no-egress)") {
			t.Fatal("upgrade hid changed egress rules", plan.Plan.Review())
		}
		if !disabled {
			request := f.charts.Previews[len(f.charts.Previews)-1]
			var network networkingv1.NetworkPolicy
			decodeObject(t, object(t, rendered(t, request.Values), "NetworkPolicy", "oberth-egress"), &network)
			if !policyAllows(network, networkTarget{ip: "93.184.216.34"}, corev1.ProtocolTCP, 443) {
				t.Fatal("saved egress true cut live internet traffic")
			}
		}
	}
}
