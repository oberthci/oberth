package main

import (
	"strconv"
	"testing"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func statusControllerClient(namespace, cap, origin string) *fake.Clientset {
	return fake.NewClientset(&core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: "oberth-controller-config", Namespace: namespace, Annotations: map[string]string{parallelOriginAnnotation: origin, "private": "PRIVATE-CONTROLLER-ANNOTATION"}}, Data: map[string]string{"parallelism": cap, "executor": "PRIVATE-CONTROLLER-SETTINGS"}})
}

func TestEffectiveRunCapReadsControllerChanges(t *testing.T) {
	c := testComposition(t)
	kube := statusControllerClient(c.config.ArgoNamespace, "4", "default")
	o := &serverOwners{c: c, cluster: clusterClients{Kube: kube}}
	for _, cap := range []int{4, 0, 7, 4} {
		cm, err := kube.CoreV1().ConfigMaps(c.config.ArgoNamespace).Get(t.Context(), "oberth-controller-config", meta.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cm.Data["parallelism"] = strconv.Itoa(cap)
		if _, err = kube.CoreV1().ConfigMaps(c.config.ArgoNamespace).Update(t.Context(), cm, meta.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		rows := settingsRows{}
		rows.runCap(t.Context(), o)
		if len(rows.values) != 1 || rows.values[0].Value != strconv.Itoa(cap) {
			t.Fatal("live controller cap lost", rows.values)
		}
	}
}

func TestEffectiveRunCapRetainsExplicitDefaultOrigin(t *testing.T) {
	c := testComposition(t)
	rows := settingsRows{}
	rows.runCap(t.Context(), &serverOwners{c: c, cluster: clusterClients{Kube: statusControllerClient(c.config.ArgoNamespace, "4", "chosen")}})
	if len(rows.values) != 1 || rows.values[0].Value != "4" || rows.values[0].Source != "chosen" {
		t.Fatal("explicit controller default origin lost", rows.values)
	}
}

func TestEffectiveRunCapDoesNotInventAValue(t *testing.T) {
	c := testComposition(t)
	for _, cap := range []string{"", "invalid", "-1"} {
		rows := settingsRows{}
		rows.runCap(t.Context(), &serverOwners{c: c, cluster: clusterClients{Kube: statusControllerClient(c.config.ArgoNamespace, cap, "default")}})
		if len(rows.values) != 1 || rows.values[0].Value != "unavailable" {
			t.Fatal("invalid controller cap reported as effective", rows.values)
		}
	}
	rows := settingsRows{}
	rows.runCap(t.Context(), &serverOwners{c: c, cluster: clusterClients{Kube: fake.NewClientset()}})
	if len(rows.values) != 1 || rows.values[0].Value != "unavailable" {
		t.Fatal("missing controller map reported as effective", rows.values)
	}
}
