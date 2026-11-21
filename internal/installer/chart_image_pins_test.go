package installer_test

import (
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestRenderedImagesArePinned(t *testing.T) {
	t.Parallel()
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "development", true: "release"}[release], func(t *testing.T) {
			var values map[string]any
			products := []string{"europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:dev", "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-source-plugin:dev"}
			if release {
				values = map[string]any{"imageDigest": "sha256:" + strings.Repeat("b", 64), "sourcePluginImageDigest": "sha256:" + strings.Repeat("c", 64)}
				products = []string{"europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:" + strings.Repeat("b", 64), "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-source-plugin@sha256:" + strings.Repeat("c", 64)}
			}
			assertRenderedImagePins(t, rendered(t, values), products)
		})
	}
}

func assertRenderedImagePins(t *testing.T, objects []map[string]any, products []string) {
	t.Helper()
	images := renderedImageReferences(t, objects)
	executors := executorImageReferences(t, objects)
	if len(executors) != 2 || executors[0] == "" || executors[0] != executors[1] {
		t.Fatal("controller argument and configuration name different executors", executors)
	}
	images = append(images, executors...)
	pinned := regexp.MustCompile(`:[^/@]+@sha256:[a-f0-9]{64}$`)
	seen := map[string]bool{}
	for _, image := range images {
		seen[image] = true
		if image != products[0] && image != products[1] && !pinned.MatchString(image) {
			t.Errorf("unpinned third-party image: %s", image)
		}
	}
	if len(seen) < 5 || !seen[products[0]] || !seen[products[1]] {
		t.Fatal("rendered image walk omitted a product or native image", seen)
	}
}

func imageFields(value any) []string {
	var images []string
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			if image, ok := item.(string); ok && (key == "image" || strings.HasSuffix(key, "_image") || key == "image_digest") {
				images = append(images, image)
			}
			images = append(images, imageFields(item)...)
		}
	case []any:
		for _, item := range value {
			images = append(images, imageFields(item)...)
		}
	}
	return images
}

func renderedImageReferences(t *testing.T, objects []map[string]any) []string {
	t.Helper()
	var images []string
	for _, object := range objects {
		images = append(images, imageFields(object)...)
		if object["kind"] != "ConfigMap" {
			continue
		}
		for key, body := range object["data"].(map[string]any) {
			images = append(images, configurationImageReferences(t, key, body.(string))...)
		}
	}
	return images
}

func configurationImageReferences(t *testing.T, key, body string) []string {
	t.Helper()
	switch key {
	case "executor", "artifactDrivers", "images", "server.json", "source.json", "workflowDefaults":
		var value any
		if err := yaml.Unmarshal([]byte(body), &value); err != nil {
			t.Fatal("image-bearing configuration is invalid", key, err)
		}
		images := imageFields(value)
		if key == "images" {
			for image := range value.(map[string]any) {
				images = append(images, image)
			}
		}
		return images
	default:
		return nil
	}
}

func executorImageReferences(t *testing.T, objects []map[string]any) []string {
	t.Helper()
	config := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	var executor struct{ Image string }
	if err := yaml.Unmarshal([]byte(config["executor"].(string)), &executor); err != nil {
		t.Fatal(err)
	}
	images := []string{executor.Image}
	var controller appsv1.Deployment
	decodeObject(t, object(t, objects, "Deployment", "oberth-controller-workflow-controller"), &controller)
	args := controller.Spec.Template.Spec.Containers[0].Args
	for i, arg := range args {
		if arg == "--executor-image" && i+1 < len(args) {
			images = append(images, args[i+1])
		}
	}
	return images
}
