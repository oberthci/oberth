package argoworkflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// SourcesAnnotation is the canonical representation after removing the
// Oberth-only top-level sources extension from the native Argo document.
// It is a declaration, never proof of authorization or delivery.
const SourcesAnnotation = "oberth.ci/sources"
const MaxPinnedSources = 4

type PinnedSource = pipeline.PinnedSource

var sourceSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func ValidSourceRepository(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 3 || strings.HasSuffix(value, ".git") {
		return false
	}
	for _, part := range parts {
		if !sourceSegment.MatchString(part) {
			return false
		}
	}
	return true
}

func ValidatePinnedSources(sources []PinnedSource) error {
	if len(sources) > MaxPinnedSources {
		return fmt.Errorf("argoworkflow: at most %d pinned sources are allowed", MaxPinnedSources)
	}
	seen := map[string]bool{}
	for _, source := range sources {
		name := strings.TrimPrefix(source.Path, "/work/sources/")
		if !ValidSourceRepository(source.Repository) || !IsFullFragmentSHA(source.Commit) || source.Commit != strings.ToLower(source.Commit) || name == source.Path || !sourceSegment.MatchString(name) || seen[source.Path] {
			return fmt.Errorf("argoworkflow: pinned source requires a canonical upstream/org/repository, full lowercase commit and unique /work/sources/name path")
		}
		seen[source.Path] = true
	}
	return nil
}

func decodePinnedSources(body []byte) ([]PinnedSource, error) {
	var sources []PinnedSource
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&sources); err != nil {
		return nil, fmt.Errorf("argoworkflow: decode sources: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("argoworkflow: trailing sources data")
	}
	if err := ValidatePinnedSources(sources); err != nil {
		return nil, err
	}
	return sources, nil
}

func PinnedSources(workflow *wfv1.Workflow) ([]PinnedSource, error) {
	body := workflow.Annotations[SourcesAnnotation]
	if body == "" {
		return nil, nil
	}
	return decodePinnedSources([]byte(body))
}
