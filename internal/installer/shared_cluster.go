package installer

import (
	"context"
	"reflect"
)

func (e *Engine) discoverSharedCluster(ctx context.Context, p *Plan) error {
	state, err := e.namespaceSnapshot(ctx, p.choices)
	if err != nil {
		return err
	}
	p.request.Values["pipeline"].(map[string]any)["createNamespace"] = pipelineNamespaceCreation(p, state.pipeline)
	p.request.Values["statusPodSecurity"] = namespaceSecurityLevel(state)
	p.portScope, err = e.checkNodePorts(ctx, p.choices)
	if err != nil {
		return err
	}
	p.definitions, err = e.workflowDefinitions(ctx, p.release.chart, p.choices.Namespace, p.history)
	if err != nil {
		return err
	}
	shared := make([]any, len(p.definitions.Shared))
	for i, name := range p.definitions.Shared {
		shared[i] = name
	}
	p.request.Values["sharedWorkflowDefinitions"] = shared
	return nil
}

func (e *Engine) recheckSharedCluster(ctx context.Context, p *Plan) (bool, error) {
	state, err := e.namespaceSnapshot(ctx, p.choices)
	if err != nil {
		return false, err
	}
	if p.request.Values["statusPodSecurity"] != namespaceSecurityLevel(state) {
		return false, described("target", nil, "Pipeline Pod Security changed after review.")
	}
	create := pipelineNamespaceCreation(p, state.pipeline)
	if p.request.Values["pipeline"].(map[string]any)["createNamespace"] != create {
		return false, described("target", nil, "Pipeline namespace ownership changed after review.")
	}
	scope, err := e.checkNodePorts(ctx, p.choices)
	if err != nil {
		return false, err
	}
	definitions, err := e.workflowDefinitions(ctx, p.release.chart, p.choices.Namespace, p.history)
	if err != nil {
		return false, err
	}
	if scope != p.portScope || !reflect.DeepEqual(definitions, p.definitions) {
		return false, described("target", nil, "Shared node ports or workflow definitions changed after review.")
	}
	return state.serverAbsent, nil
}

func namespaceSecurityLevel(state namespaceState) string {
	if state.pipeline == nil {
		return "baseline"
	}
	return state.pipeline.Labels[podSecurityEnforce]
}
