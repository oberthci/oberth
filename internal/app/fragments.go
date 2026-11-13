package app

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type FragmentLoader interface {
	Load(ctx context.Context, key argoworkflow.FragmentKey, trigger pipeline.Trigger, refKind model.RefKind, runTrigger string) (argoworkflow.Fragment, error)
}

func loadFragments(ctx context.Context, loader FragmentLoader, source []byte, trigger pipeline.Trigger, refKind model.RefKind, runTrigger string) (map[argoworkflow.FragmentKey]argoworkflow.Fragment, error) {
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return nil, err
	}
	keys, err := argoworkflow.FragmentRefs(workflow)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	if loader == nil {
		return nil, &argoworkflow.AdmissionError{Cause: fmt.Errorf("app: templateRef to %s points outside this document; inline the template", keys[0])}
	}
	fragments := make(map[argoworkflow.FragmentKey]argoworkflow.Fragment, len(keys))
	for _, key := range keys {
		fragment, err := loader.Load(ctx, key, trigger, refKind, runTrigger)
		if err != nil {
			return nil, err
		}
		fragments[key] = fragment
	}
	return fragments, nil
}
