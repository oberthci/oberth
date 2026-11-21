package installer

import (
	"slices"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
)

// inputs resolves owned values before the SDK validates coalesced dependency defaults.
func inputs(ch *chart.Chart, overrides map[string]any, namespace string) (map[string]any, error) {
	if ch.Values["pipeline"] == nil || ch.Values["server"] == nil || ch.Values["argoNative"] == nil {
		return merge(ch.Values, overrides), nil
	}
	defaults := merge(ch.Values, map[string]any{
		"pipeline": map[string]any{"namespace": ""},
		"server": map[string]any{"runtime": map[string]any{
			"controller_namespace": "", "source": map[string]any{"endpoint": "", "server_name": ""},
		}},
		"argoNative": map[string]any{"namespaceOverride": ""},
	})
	store, _ := defaults["secretStore"].(map[string]any)
	if store != nil && store["bundled"] != false {
		store["address"] = ""
	}
	defaults = namespaceValues(defaults, namespace)
	return namespaceValues(merge(defaults, overrides), namespace), nil
}

func preparedChart(ch *chart.Chart, values map[string]any, namespace string) (*chart.Chart, map[string]any, error) {
	// Reload original files before dependency pruning. Historical charts are
	// freshly decoded from native release storage, which omits Raw.
	if len(ch.Raw) > 0 {
		files := make([]*loader.BufferedFile, 0, len(ch.Raw))
		for _, file := range ch.Raw {
			files = append(files, &loader.BufferedFile{Name: file.Name, Data: file.Data})
		}
		var err error
		ch, err = loader.LoadFiles(files)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := action.CheckDependencies(ch, ch.Metadata.Dependencies); err != nil {
		return nil, nil, problem("chart-files", err)
	}
	removeChartTests(ch)
	skipSharedDefinitions(ch, values)
	values, err := inputs(ch, values, namespace)
	if err == nil {
		err = chartutil.ProcessDependencies(ch, values)
	}
	return ch, values, err
}

func removeChartTests(ch *chart.Chart) {
	ch.Templates = slices.DeleteFunc(ch.Templates, func(file *chart.File) bool {
		return strings.HasPrefix(file.Name, "templates/tests/")
	})
	for _, dependency := range ch.Dependencies() {
		removeChartTests(dependency)
	}
}
