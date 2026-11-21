package main

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

type removalChartObject struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ObjectMeta `json:"metadata"`
	Spec            struct {
		Template             corev1.PodTemplateSpec         `json:"template"`
		VolumeClaimTemplates []corev1.PersistentVolumeClaim `json:"volumeClaimTemplates"`
		Replicas             *int32                         `json:"replicas"`
	} `json:"spec"`
}

func renderedRemovalNames(t *testing.T) map[string][]string {
	t.Helper()
	chart, err := loader.Load("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	files, err := installer.Render(chart, nil, "second")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]sets.Set[string]{"PVC": sets.New[string](), "Secret": sets.New[string]()}
	objects := map[string]json.RawMessage{}
	for name, text := range files {
		if strings.HasSuffix(name, ".yaml") {
			readContractObjects(t, text, objects)
		}
	}
	for name, raw := range objects {
		var object removalChartObject
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(name, err)
		}
		addRemovalChartNames(names, object)
	}
	result := make(map[string][]string)
	for kind, values := range names {
		if values.Len() == 0 {
			t.Fatal("chart produced no retained resources", kind)
		}
		result[kind] = sets.List(values)
	}
	return result
}

func addRemovalChartNames(names map[string]sets.Set[string], object removalChartObject) {
	if object.Kind == "PersistentVolumeClaim" && object.Metadata.Annotations["helm.sh/resource-policy"] == "keep" {
		names["PVC"].Insert(object.Metadata.Name)
	}
	if object.Kind == "StatefulSet" {
		replicas := int32(1)
		if object.Spec.Replicas != nil {
			replicas = *object.Spec.Replicas
		}
		for _, claim := range object.Spec.VolumeClaimTemplates {
			for ordinal := int32(0); ordinal < replicas; ordinal++ {
				names["PVC"].Insert(claim.Name + "-" + object.Metadata.Name + "-" + strconv.Itoa(int(ordinal)))
			}
		}
	}
	for _, volume := range object.Spec.Template.Spec.Volumes {
		if volume.Secret != nil {
			names["Secret"].Insert(volume.Secret.SecretName)
		}
	}
}

func TestDocumentedRemovalNamesComeFromPinnedChart(t *testing.T) {
	section := installDocSection(t, "## Remove an installation")
	documented := map[string][]string{}
	pattern := regexp.MustCompile("(?m)^\\| (PVC|Secret) `([^`]+)` \\|")
	for _, match := range pattern.FindAllStringSubmatch(section, -1) {
		documented[match[1]] = append(documented[match[1]], match[2])
	}
	for kind, names := range renderedRemovalNames(t) {
		slices.Sort(documented[kind])
		if !slices.Equal(names, documented[kind]) {
			t.Fatal("removal table differs from rendered chart", kind, names, documented[kind])
		}
	}
}
