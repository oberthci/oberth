package installer

import (
	"encoding/json"
	"fmt"
)

// namespaceValues fills derived defaults once, before native dependency rendering.
func namespaceValues(v map[string]any, namespace string, priorityDeclared bool) map[string]any {
	pipeline, _ := v["pipeline"].(map[string]any)
	server, _ := v["server"].(map[string]any)
	runtime, _ := server["runtime"].(map[string]any)
	source, _ := runtime["source"].(map[string]any)
	store, _ := v["secretStore"].(map[string]any)
	native, _ := v["argoNative"].(map[string]any)
	controller, _ := native["controller"].(map[string]any)
	if pipeline == nil || server == nil || runtime == nil || source == nil || store == nil || native == nil || controller == nil {
		return v
	}
	defaultText(pipeline, "namespace", pipelineNamespace(namespace))
	defaultText(runtime, "controller_namespace", namespace)
	defaultText(source, "endpoint", "https://oberth-source."+namespace+".svc:8445")
	defaultText(source, "server_name", "oberth."+namespace+".svc")
	if store["bundled"] != false {
		defaultText(store, "address", storeURL(namespace))
	}
	defaultText(native, "namespaceOverride", namespace)
	if priorityDeclared {
		setControlPriority(v, controller, namespace)
	}
	pipelineNamespace, ok := pipeline["namespace"].(string)
	if !ok {
		return v
	}
	controller["extraArgs"] = []any{"--namespaced", "--managed-namespace=" + pipelineNamespace}
	annotations, _ := controller["podAnnotations"].(map[string]any)
	controller["podAnnotations"] = merge(annotations, map[string]any{
		"workflows.argoproj.io/controller-instanceid": namespace,
		"checksum/start": startOnlyChecksum(v, controller),
	})
	controller["workflowNamespaces"] = []any{pipeline["namespace"]}
	controller["configMap"] = map[string]any{"create": false, "name": runtime["controller_configmap"]}
	return v
}

func setControlPriority(v, controller map[string]any, namespace string) {
	priority := "oberth-control-" + namespace
	controller["priorityClassName"] = priority
	if bundled, ok := v["storeNative"].(map[string]any); ok {
		if server, ok := bundled["server"].(map[string]any); ok {
			server["priorityClassName"] = priority
		}
	}
}

// startOnlyChecksum covers the inputs of the controller ConfigMap entries the
// pinned controller takes only at start: the images entry, from which it builds
// its image entrypoint index, and workflowDefaults, which its schedule
// controller is given once. It reloads the rest of that ConfigMap on change, so
// only a change of these rolls its Pods; an unchanged one leaves them alone.
func startOnlyChecksum(v, controller map[string]any) string {
	defaults, _ := json.Marshal(controller["workflowDefaults"])
	return publicDigest(fmt.Appendf(nil, "%v@%v\n%s", v["sourcePluginImageRepository"], v["sourcePluginImageDigest"], defaults))
}

func defaultText(values map[string]any, key, value string) {
	if values[key] == nil || values[key] == "" {
		values[key] = value
	}
}

func storeDNS(namespace string) string { return "oberth-store." + namespace + ".svc" }

func pipelineNamespace(namespace string) string { return namespace + "-runs" }

func storeURL(namespace string) string { return "https://" + storeDNS(namespace) + ":8200" }
