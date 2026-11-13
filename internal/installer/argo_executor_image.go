package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// checkArgoExecutorImage preserves compatible stored values and refuses to
// replace an incompatible operator image. Resource main token delivery requires
// the exact digest-qualified image, independently of controller attestation.
func checkArgoExecutorImage(ctx context.Context, deps Deps, namespace string, exists bool) (bool, error) {
	if !exists {
		return true, nil
	}
	raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", namespace, "-o", "json"})
	if err != nil {
		return false, fmt.Errorf("read Argo executor image: %w", err)
	}
	var values map[string]any
	if err := json.Unmarshal(nonEmptyJSON(raw), &values); err != nil {
		return false, fmt.Errorf("decode Argo executor image: %w", err)
	}
	image := values
	for _, key := range []string{"executor", "image"} {
		value, present := image[key]
		if !present {
			image = nil
			break
		}
		object, ok := value.(map[string]any)
		if !ok {
			return false, fmt.Errorf("stored executor.image must be an object; left operator values unchanged; resource token delivery requires %s", argoworkflow.ExecutorImage)
		}
		image = object
	}
	repository, tag, _ := strings.Cut(argoworkflow.ExecutorImage, ":")
	registry, repository, _ := strings.Cut(repository, "/")
	parts := map[string]string{"registry": registry, "repository": repository, "tag": tag}
	_, tagSet := image["tag"]
	for key := range parts {
		if value, present := image[key]; present {
			text, ok := value.(string)
			if !ok {
				return false, fmt.Errorf("stored executor.image.%s must be a string; left operator values unchanged", key)
			}
			parts[key] = text
		}
	}
	actual := parts["repository"] + ":" + parts["tag"]
	if parts["registry"] != "" {
		actual = parts["registry"] + "/" + actual
	}
	if actual != argoworkflow.ExecutorImage {
		return false, fmt.Errorf("stored executor image %q cannot receive resource-template tokens; configure %s or use --skip-argo-workflows to keep the operator-managed release; left operator values unchanged", actual, argoworkflow.ExecutorImage)
	}
	return !tagSet, nil
}

// noticeArgoExecutorImage leaves nondefault-chart images with the operator and
// names a retained stored tag so a controller upgrade cannot hide its pin.
func noticeArgoExecutorImage(ctx context.Context, deps Deps, namespace, version string, exists bool) error {
	tag := ""
	if exists {
		raw, err := deps.RunHelm(ctx, []string{"get", "values", "argo-workflows", "-n", namespace, "-o", "json"})
		if err != nil {
			return fmt.Errorf("read stored Argo executor tag: %w", err)
		}
		var values map[string]any
		if err := json.Unmarshal(nonEmptyJSON(raw), &values); err != nil {
			return fmt.Errorf("decode stored Argo executor tag: %w", err)
		}
		executor, _ := values["executor"].(map[string]any)
		image, _ := executor["image"].(map[string]any)
		tag, _ = image["tag"].(string)
	}
	retained := ""
	if tag != "" {
		retained = fmt.Sprintf("; stored executor.image.tag=%q is present and left in place", tag)
	}
	_, _ = fmt.Fprintf(deps.Output, "Argo chart %s: executor image left unchanged; resource-template token delivery requires %s%s.\n", displayVersion(version), argoworkflow.ExecutorImage, retained)
	return nil
}
