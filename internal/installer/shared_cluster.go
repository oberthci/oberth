package installer

import (
	"context"
	"reflect"
	"strings"
)

func (e *Engine) discoverSharedCluster(ctx context.Context, p *Plan) error {
	if _, present := p.release.chart.Values["egressNetwork"]; present {
		network, err := e.networkSnapshot(ctx, p)
		if err != nil {
			return err
		}
		p.request.Values["egressNetwork"] = network
	}
	state, err := e.namespaceSnapshot(ctx, p.choices)
	if err != nil {
		return err
	}
	p.request.Values["pipeline"].(map[string]any)["createNamespace"] = pipelineNamespaceCreation(p, state.pipeline)
	p.request.Values["statusPodSecurity"] = namespaceSecurityLevel(state)
	p.serverPodSecurity = "restricted (new namespace)"
	if state.server != nil {
		p.serverPodSecurity = state.server.Labels[podSecurityEnforce]
		if p.serverPodSecurity == "" {
			p.serverPodSecurity = "unlabelled (existing namespace)"
		}
	}
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
	if err := e.recheckNetwork(ctx, p); err != nil {
		return false, err
	}
	state, err := e.namespaceSnapshot(ctx, p.choices)
	if err != nil {
		return false, err
	}
	level, _, _ := strings.Cut(p.podSecurity, " ")
	if level != namespaceSecurityLevel(state) {
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
